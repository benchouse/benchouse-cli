// Package tui is the full-screen guided run: pick the agent, connect to its
// conversations, point at the dbt project and the agent's context, review,
// collect and upload. Each step is verified as soon as it is finished.
//
// The package owns no collection or upload logic: the command passes those
// in as functions (Config), so the full-screen run and the line-based one
// share them. Every string that comes from the user's systems or from a
// server (check findings, errors, lookup labels, collection notes, the
// bundle summary, the upload reference) is passed through prompt.Clean
// before it is drawn.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/telemetry"
	"github.com/benchouse/benchouse-cli/internal/upload"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

// Config is what the guided run starts from and the work it may do.
type Config struct {
	// Upload is true for "benchouse upload" (the review ends in Upload or
	// Save only), false for "benchouse collect" (it ends in Save).
	Upload bool
	// Agents to choose from, supported first.
	Agents []agent.Agent
	// Agent was given with --agent; the agent step is skipped.
	Agent *agent.Agent
	// Inputs were given as flags or environment variables; they are not
	// asked for again.
	Inputs agent.Inputs
	Opts   agent.Options
	// DBTDir, CtxDir and Email were given as flags.
	DBTDir, CtxDir, Email string
	// Since is the --since text, shown on the review step.
	Since string
	// CapSet: --max-conversations was given; an agent's own default cap
	// (agent.CapFor) does not replace it.
	CapSet bool

	// CheckDBT verifies a dbt project folder; nil means not checked.
	CheckDBT func(dir string) (agent.CheckResult, error)
	// Collect writes the bundle for s, passing progress lines to progress.
	Collect func(ctx context.Context, s Selection, progress func(string)) (Bundle, error)
	// Send uploads the bundle at path.
	Send func(ctx context.Context, path, email string) (upload.Receipt, error)
	// RequestAgentAnonymous records a request for an agent the CLI does
	// not support, before any email is asked for; it returns the
	// request's id, or "" with an error (not fatal: the flow goes on).
	RequestAgentAnonymous func(ctx context.Context, name string) (string, error)
	// RequestAgentEmail asks to be told when the agent is supported,
	// linked to the anonymous request's id when there is one.
	RequestAgentEmail func(ctx context.Context, name, email, id string) error
	// CheckTimeout bounds each check and lookup (default 60 s).
	CheckTimeout time.Duration
}

// Selection is what the user chose, for Collect.
type Selection struct {
	Agent                 agent.Agent
	Inputs                agent.Inputs
	Opts                  agent.Options
	DBTDir, CtxDir, Email string
}

// Bundle is a written bundle.
type Bundle struct {
	// Path is absolute; Rel is the path as given (for commands to retry with).
	Path, Rel string
	Size      int64
	// Summary is what's in it, uncleaned (see summaryBody in the command).
	Summary string
	// Stats are its counts, for the usage stats (Duration is set here).
	Stats telemetry.Collection
}

// Result says how the run ended.
type Result int

const (
	// Quit: the user left before saving or uploading anything.
	Quit Result = iota
	// Requested: the user asked for an agent the CLI does not support.
	Requested
	// Saved: a bundle was written and not uploaded.
	Saved
	// Uploaded: a bundle was written and uploaded.
	Uploaded
)

// Outcome is what happened, for the command to report after the screen
// is restored.
type Outcome struct {
	Result Result
	// Interrupted: Ctrl-C, or the process was asked to stop.
	Interrupted bool
	// RequestedAgent and Email for Requested; Email for Uploaded.
	RequestedAgent, Email string
	// Bundle is set once one was written (it stays on disk).
	Bundle *Bundle
	// Receipt for Uploaded.
	Receipt upload.Receipt
	// UploadErr is the last upload failure, when the user saved instead.
	UploadErr error
}

// Run shows the guided run until the user finishes or quits. Cancelling ctx
// stops it as Ctrl-C does.
func Run(ctx context.Context, cfg Config, opts ...tea.ProgramOption) (Outcome, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := New(ctx, cfg)
	// The command's own signal handling cancels ctx; Bubble Tea's would
	// race it, so it is off.
	// It draws on stderr, like the line-based prompts, so stdout stays for
	// the upload reference.
	p := tea.NewProgram(m, append([]tea.ProgramOption{tea.WithAltScreen(), tea.WithoutSignalHandler(), tea.WithOutput(os.Stderr)}, opts...)...)
	go func() {
		<-ctx.Done()
		p.Send(interruptMsg{}) // a no-op once the program has finished
	}()
	final, err := p.Run()
	cancel()
	// Let a collection that was stopped remove its temp files before the
	// process exits.
	waitTimeout(m.wg, 10*time.Second)
	fm, ok := final.(*Model)
	if !ok {
		fm = m
	}
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, tea.ErrInterrupted) {
		fm.outcome.Interrupted, err = true, nil
	}
	return fm.outcome, err
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

type step int

const (
	stepAgent step = iota
	stepConv
	stepDBT
	stepCtx
	stepReview
)

type phase int

const (
	phList       phase = iota // agent list
	phSetup                   // the agent's Setup text
	phForm                    // a huh form (see formKind)
	phBusy                    // a spinner: lookup, check, request, collect, upload
	phChecked                 // check results
	phQueries                 // the read-only queries
	phSent                    // agent request sent
	phSummary                 // bundle written: Upload / Save only / Back
	phReceipt                 // uploaded
	phUploadErr               // upload failed
	phRequestErr              // agent request failed
	phCollectErr              // collection failed
)

type formKind int

const (
	fkOther formKind = iota
	fkOtherEmail
	fkConv
	fkDBT
	fkCtx
	fkReview
)

// verdict is what the status bar shows for a step.
type verdict int

const (
	vPending    verdict = iota // not reached yet
	vOK                        // checked and fine
	vFailed                    // check failed
	vSkipped                   // left empty
	vUnverified                // no check for it: verified while collecting
)

// checkState is one step's check.
type checkState struct {
	v   verdict
	res agent.CheckResult
	err error
	dir string // dbt and context
}

type opKind int

const (
	opNone opKind = iota
	opLookup
	opVerify
	opCheck
	opRequest
	opCollect
	opUpload
)

// Model is the guided run. Use it through Run; it is exported for tests.
type Model struct {
	cfg    Config
	ctx    context.Context
	wg     *sync.WaitGroup
	width  int
	height int

	step  step
	phase phase

	// Agent step.
	cursor     int
	agent      *agent.Agent
	other      int // 1 or 2 while in the request-an-agent flow, else 0
	otherName  string
	otherEmail string
	otherSent  string // the name the anonymous request was sent for
	otherID    string // its id, "" when it failed
	requestErr error

	// Conversations step.
	given     map[string]bool    // supplied by flags or environment
	vals      map[string]*string // form values, by field key
	asked     map[string]bool    // answered in this pass through the form
	inputs    agent.Inputs       // validated inputs, after the form
	segment   []agent.Field      // fields on the current form
	lookup    *agent.Field       // the field being listed
	lookupMsg string             // why the lookup fell back to typing
	lookupOps [][2]string        // the list on the current lookup form
	picked    []string           // lookups answered without asking
	verified  map[string]string  // facts from fields' Verify, by field key
	verifyErr error              // why the current form's Verify failed
	segStarts []int              // index of each connection form's first field, for esc
	conn      checkState
	viewport  viewport.Model
	backTo    phase // where esc leaves the queries view

	// dbt and context steps.
	dbtVal, ctxVal string
	dbt, ctxc      checkState
	dbtSeen        bool // the dbt flag value was checked once
	ctxSeen        bool

	// Review step.
	window                    string // a windowChoices value
	maxConv                   int
	anonymize, results, users bool
	email                     string
	notes                     []string
	bundle                    *Bundle
	menu                      int // summary action cursor
	receipt                   upload.Receipt
	uploadErr                 error
	collectErr                error

	form *huh.Form
	fk   formKind

	spin      spinner.Model
	op        opKind
	opID      int
	busyLabel string
	opStart   time.Time // when the running operation started
	cancel    context.CancelFunc
	ch        chan tea.Msg

	quitting bool
	outcome  Outcome

	// Usage stats: the step last reported, and the field failures
	// reported (by field and message), so a form validating on every key
	// reports each once.
	viewed   string
	reported map[string]bool
}

// New returns the model for cfg.
func New(ctx context.Context, cfg Config) *Model {
	if cfg.CheckTimeout == 0 {
		cfg.CheckTimeout = 60 * time.Second
	}
	if cfg.Inputs == nil {
		cfg.Inputs = agent.Inputs{}
	}
	m := &Model{cfg: cfg, ctx: ctx, wg: &sync.WaitGroup{}, width: 80, height: 24,
		dbtVal: cfg.DBTDir, ctxVal: cfg.CtxDir, email: cfg.Email,
		anonymize: cfg.Opts.AnonymizeUsers, results: cfg.Opts.IncludeResults, users: cfg.Opts.IncludeUsers,
		window: defaultWindow(cfg.Since), maxConv: cfg.Opts.MaxConversations}
	m.spin = spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(accentStyle))
	m.viewport = viewport.New(m.width, 10)
	if cfg.Agent != nil {
		m.setAgent(*cfg.Agent)
		m.enterConv()
	}
	return m
}

type (
	interruptMsg  struct{}
	lookupDoneMsg struct {
		id   int
		opts [][2]string
		err  error
	}
	verifyDoneMsg struct {
		id   int
		key  string
		fact string
		err  error
	}
	checkDoneMsg struct {
		id  int
		res agent.CheckResult
		err error
	}
	requestDoneMsg struct {
		id  int
		err error
	}
	anonDoneMsg struct {
		id   int
		name string
		arID string
	}
	progressMsg struct {
		id int
		s  string
	}
	collectDoneMsg struct {
		id  int
		b   Bundle
		err error
	}
	uploadDoneMsg struct {
		id  int
		r   upload.Receipt
		err error
	}
)

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	if m.phase == phForm && m.form != nil {
		return m.form.Init()
	}
	return nil
}

// startOp cancels any running operation and starts a new one. Results of
// earlier operations are recognised by their id and dropped.
func (m *Model) startOp(kind opKind, label string, timeout time.Duration) (context.Context, int) {
	m.cancelOp()
	m.opID++
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(m.ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(m.ctx)
	}
	m.op, m.cancel, m.busyLabel, m.phase, m.opStart = kind, cancel, label, phBusy, time.Now()
	return ctx, m.opID
}

func (m *Model) cancelOp() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.op = opNone
}

// endOp marks the operation finished; it reports whether msg id is current.
func (m *Model) endOp(id int) bool {
	if id != m.opID || m.op == opNone {
		return false
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.op = opNone
	return true
}

// background runs f off the UI goroutine, counted so Run can wait for it.
// A panic in f (a check in an agent package, say) becomes fail's message
// instead of taking the terminal down with it.
func (m *Model) background(f func() tea.Msg, fail func(error) tea.Msg) tea.Cmd {
	m.wg.Add(1)
	return tea.Batch(m.spin.Tick, func() (msg tea.Msg) {
		defer m.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				msg = fail(fmt.Errorf("internal error: %v", r))
			}
		}()
		return f()
	})
}

// quit leaves the program, stopping whatever runs.
func (m *Model) quit(interrupted bool) tea.Cmd {
	m.cancelOp()
	m.quitting = true
	m.outcome.Interrupted = m.outcome.Interrupted || interrupted
	if m.outcome.Email == "" {
		m.outcome.Email = m.normEmail(m.email)
	}
	if m.bundle != nil && m.outcome.Bundle == nil {
		m.outcome.Bundle = m.bundle
	}
	return tea.Quit
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.known()
	cmd := m.update(msg)
	m.trackView()
	return m, cmd
}

func (m *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.Width, m.viewport.Height = m.bodyWidth(), m.viewportHeight()
		if m.form != nil {
			f, cmd := m.form.Update(tea.WindowSizeMsg{Width: m.bodyWidth(), Height: m.bodyHeight() - 4})
			m.form = f.(*huh.Form)
			return cmd
		}
		return nil
	case interruptMsg:
		return m.quit(true)
	case spinner.TickMsg:
		if m.phase != phBusy {
			return nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return cmd
	case lookupDoneMsg:
		if !m.endOp(msg.id) {
			return nil
		}
		return m.lookupDone(msg.opts, msg.err)
	case verifyDoneMsg:
		if !m.endOp(msg.id) {
			return nil
		}
		return m.verifyDone(msg.key, msg.fact, msg.err)
	case checkDoneMsg:
		if !m.endOp(msg.id) {
			return nil
		}
		return m.checkDone(msg.res, msg.err)
	case anonDoneMsg:
		if !m.endOp(msg.id) {
			return nil
		}
		m.otherSent, m.otherID = msg.name, msg.arID
		return m.showOtherEmail()
	case requestDoneMsg:
		if !m.endOp(msg.id) {
			return nil
		}
		if msg.err != nil {
			m.requestErr, m.phase = msg.err, phRequestErr
			return nil
		}
		m.phase = phSent
		return nil
	case progressMsg:
		if msg.id != m.opID || m.op != opCollect {
			return nil
		}
		m.notes = append(m.notes, msg.s)
		return m.waitCh()
	case collectDoneMsg:
		if !m.endOp(msg.id) {
			return nil
		}
		return m.collectDone(msg.b, msg.err)
	case uploadDoneMsg:
		if !m.endOp(msg.id) {
			return nil
		}
		if msg.err != nil {
			telemetry.UploadFailed(m.agent.ID, msg.err)
			m.uploadErr, m.phase = msg.err, phUploadErr
			return nil
		}
		telemetry.Uploaded(m.agent.ID, m.bundle.Size)
		m.receipt, m.phase = msg.r, phReceipt
		return nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m.quit(true)
		}
		return m.key(msg)
	}
	if m.phase == phForm && m.form != nil {
		return m.updateForm(msg)
	}
	return nil
}

// trackView reports the step on screen when it changes.
func (m *Model) trackView() {
	v := m.viewName()
	if v == m.viewed {
		return
	}
	m.viewed = v
	if m.agent == nil || v == "agent" || v == "other" {
		telemetry.Reached(v)
		return
	}
	telemetry.View(m.agent.ID, v, m.agent.Context != nil, m.cfg.Upload)
}

// viewName names the step on screen, as telemetry.Steps does, or "agent"
// and "other" for the agent list and the request-an-agent flow.
func (m *Model) viewName() string {
	switch m.step {
	case stepAgent:
		if m.other > 0 {
			return "other"
		}
		return "agent"
	case stepConv:
		return "conversations"
	case stepDBT:
		return "dbt"
	case stepCtx:
		return "context"
	}
	switch {
	case m.phase == phSummary:
		return "summary"
	case m.phase == phBusy && m.op == opUpload, m.phase == phUploadErr:
		return "uploading"
	case m.phase == phReceipt:
		return "done"
	}
	return "review"
}

// known registers the values typed so far, so the usage stats scrub them
// from error messages; extra are values not stored yet.
func (m *Model) known(extra ...string) {
	if m.agent == nil {
		return
	}
	in := cloneInputs(m.cfg.Inputs)
	for k, v := range m.vals {
		if *v != "" {
			in[k] = *v
		}
	}
	extra = append(extra, m.email, m.dbtVal, m.ctxVal, expandHome(strings.TrimSpace(m.dbtVal)), expandHome(strings.TrimSpace(m.ctxVal)))
	if m.bundle != nil {
		extra = append(extra, m.bundle.Path, m.bundle.Rel)
	}
	telemetry.KnownInputs("tui", m.agent.Fields, in, extra...)
}

// fieldFailed reports a refused field once per field and message; value
// is what was typed, scrubbed from the message.
func (m *Model) fieldFailed(key, kind string, err error, value ...string) {
	m.known(value...)
	k := key + "\x00" + kind + "\x00" + err.Error()
	if m.reported == nil {
		m.reported = map[string]bool{}
	}
	if m.reported[k] {
		return
	}
	m.reported[k] = true
	telemetry.FieldFailed(m.agent.ID, key, kind, err)
}

// updateForm passes msg to the form and acts when it is submitted.
func (m *Model) updateForm(msg tea.Msg) tea.Cmd {
	f, cmd := m.form.Update(msg)
	m.form = f.(*huh.Form)
	if m.form.State == huh.StateCompleted {
		m.form = nil
		return tea.Batch(cmd, m.formDone())
	}
	return cmd
}

// waitCh delivers the next progress line or the end of a collection.
func (m *Model) waitCh() tea.Cmd {
	ch := m.ch
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}
