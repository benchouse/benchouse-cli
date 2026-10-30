package tui

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/prompt"
	"github.com/benchouse/benchouse-cli/internal/telemetry"
	"github.com/benchouse/benchouse-cli/internal/upload"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

// key handles a keypress (Ctrl-C is handled before).
func (m *Model) key(k tea.KeyMsg) tea.Cmd {
	s := k.String()
	switch m.phase {
	case phList:
		n := len(m.cfg.Agents) + 1 // plus "Other"
		switch s {
		case "up", "k", "ctrl+p":
			m.cursor = (m.cursor - 1 + n) % n
		case "down", "j", "ctrl+n":
			m.cursor = (m.cursor + 1) % n
		case "home", "g":
			m.cursor = 0
		case "end", "G":
			m.cursor = n - 1
		case "enter":
			if m.cursor == len(m.cfg.Agents) {
				telemetry.AgentSelected("other")
				return m.showOtherForm()
			}
			telemetry.AgentSelected(m.cfg.Agents[m.cursor].ID)
			m.setAgent(m.cfg.Agents[m.cursor])
			return tea.Batch(tea.ClearScreen, m.enterConv())
		}
		return nil

	case phSetup:
		switch s {
		case "enter":
			return m.editConv()
		case "v":
			return m.showQueries()
		case "esc":
			if m.cfg.Agent == nil {
				m.step, m.phase = stepAgent, phList
				return tea.ClearScreen
			}
			return nil
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(k)
		return cmd

	case phForm:
		if s == "esc" {
			m.form = nil
			if m.fk == fkConv && len(m.segStarts) > 1 {
				return m.prevSegment()
			}
			return m.formBack()
		}
		return m.updateForm(k)

	case phBusy:
		if s == "esc" {
			return m.cancelBusy()
		}
		return nil

	case phChecked:
		st := m.state(m.step)
		switch {
		case s == "esc" || s == "e" || (s == "enter" && st.v == vFailed):
			return m.editStep()
		case s == "r":
			return m.retryStep()
		case s == "s" && (m.step == stepDBT || m.step == stepCtx):
			m.setState(m.step, checkState{v: vSkipped})
			return m.nextStep()
		case s == "v" && m.step == stepConv && st.v != vFailed:
			return m.showQueries()
		case s == "enter":
			return m.nextStep()
		}
		return nil

	case phQueries:
		switch s {
		case "esc", "q", "enter":
			m.phase = m.backTo
			if m.phase == phSetup {
				m.setSetupContent()
			}
			return nil
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(k)
		return cmd

	case phSent:
		if s == "enter" || s == "q" || s == "esc" {
			m.outcome.Result = Requested
			return m.quit(false)
		}
		return nil

	case phRequestErr:
		switch s {
		case "r":
			return m.startRequest()
		case "enter", "e", "esc":
			return m.showOtherEmail()
		}
		return nil

	case phSummary:
		acts := m.actions()
		switch s {
		case "up", "k", "left", "h":
			m.menu = (m.menu - 1 + len(acts)) % len(acts)
		case "down", "j", "right", "l", "tab":
			m.menu = (m.menu + 1) % len(acts)
		case "esc":
			return m.discardBundle()
		case "enter":
			switch acts[m.menu] {
			case actUpload:
				return m.startUpload()
			case actSave:
				telemetry.Saved(m.agent.ID)
				m.outcome.Result = Saved
				return m.quit(false)
			case actBack:
				return m.discardBundle()
			}
		}
		return nil

	case phReceipt:
		if s == "enter" || s == "q" || s == "esc" {
			m.outcome.Result, m.outcome.Receipt, m.outcome.Email = Uploaded, m.receipt, m.email
			return m.quit(false)
		}
		return nil

	case phCollectErr:
		switch s {
		case "r":
			return m.startCollect()
		case "enter", "esc", "e":
			return m.enterReview()
		}
		return nil

	case phUploadErr:
		switch s {
		case "r":
			return m.startUpload()
		case "esc":
			m.phase = phSummary
		case "enter", "q":
			telemetry.Saved(m.agent.ID)
			m.outcome.Result, m.outcome.UploadErr = Saved, m.uploadErr
			return m.quit(false)
		}
		return nil
	}
	return nil
}

const (
	actUpload = "Upload"
	actSave   = "Save only"
	actBack   = "Back"
)

func (m *Model) actions() []string {
	if m.cfg.Upload {
		return []string{actUpload, actSave, actBack}
	}
	return []string{actSave, actBack}
}

// --- Agent step ---------------------------------------------------------

func (m *Model) setAgent(a agent.Agent) {
	m.agent = &a
	m.maxConv = agent.CapFor(a, m.cfg.Opts.MaxConversations, m.cfg.CapSet)
	m.given, m.vals, m.asked = map[string]bool{}, map[string]*string{}, map[string]bool{}
	for _, f := range a.Fields {
		v := m.cfg.Inputs[f.Key]
		m.given[f.Key] = strings.TrimSpace(v) != ""
		if v == "" {
			v = f.Default
		}
		m.vals[f.Key] = &v
	}
	m.conn, m.dbt, m.ctxc = checkState{}, checkState{}, checkState{}
	m.dbtSeen, m.ctxSeen, m.picked = false, false, nil
	m.verified, m.verifyErr, m.segStarts = map[string]string{}, nil, nil
}

// The request-an-agent flow has two steps of its own: the agent's name,
// sent as soon as it is entered (so it is noted even if the user leaves),
// then an optional email, linked to that request.

func (m *Model) showOtherForm() tea.Cmd {
	m.step, m.other = stepAgent, 1
	f := m.newForm(huh.NewGroup(
		huh.NewInput().Title("Which agent do you use?").CharLimit(upload.MaxAgentName).Value(&m.otherName).Validate(validAgentName),
	))
	return m.setForm(f, fkOther)
}

func validAgentName(s string) error {
	if err := required(s); err != nil {
		return err
	}
	if len([]rune(strings.TrimSpace(s))) > upload.MaxAgentName {
		return fmt.Errorf("at most %d characters", upload.MaxAgentName)
	}
	return nil
}

// otherNamed sends the anonymous request, unless it was sent for this
// name already (esc from the email step and back).
func (m *Model) otherNamed() tea.Cmd {
	name := strings.TrimSpace(m.otherName)
	if name == m.otherSent {
		return m.showOtherEmail()
	}
	telemetry.OtherRequested(len([]rune(name)))
	ctx, id := m.startOp(opRequest, "Noting "+prompt.Clean(name)+"…", m.cfg.CheckTimeout)
	anon := m.cfg.RequestAgentAnonymous
	return m.background(func() tea.Msg {
		arID := ""
		if anon != nil {
			arID, _ = anon(ctx, name) // not fatal: the email request still names the agent
		}
		return anonDoneMsg{id, name, arID}
	}, func(error) tea.Msg { return anonDoneMsg{id: id, name: name} })
}

func (m *Model) showOtherEmail() tea.Cmd {
	m.step, m.other = stepAgent, 2
	f := m.newForm(huh.NewGroup(
		huh.NewInput().Title("Your email").
			Description("we'll tell you when it's supported, typically within 72 hours\nEnter to skip: no email is fine, we've noted the agent").
			Value(&m.otherEmail).Validate(validEmail(false)),
	))
	return m.setForm(f, fkOtherEmail)
}

func (m *Model) startRequest() tea.Cmd {
	email, name := m.normEmail(m.otherEmail), strings.TrimSpace(m.otherName)
	m.outcome.RequestedAgent, m.outcome.Email = name, email
	telemetry.OtherEmailGiven(email != "")
	if email == "" {
		m.phase = phSent
		return nil
	}
	ctx, id := m.startOp(opRequest, "Sending your request…", m.cfg.CheckTimeout)
	req, arID := m.cfg.RequestAgentEmail, m.otherID
	return m.background(func() tea.Msg {
		var err error
		if req == nil {
			err = errors.New("not available")
		} else {
			err = req(ctx, name, email, arID)
		}
		if err != nil {
			err = fmt.Errorf("could not send the request (%w); email andre@benchouse.ai instead", err)
		}
		return requestDoneMsg{id, err}
	}, func(err error) tea.Msg { return requestDoneMsg{id, err} })
}

// --- Conversations step -------------------------------------------------

func (m *Model) enterConv() tea.Cmd {
	m.step, m.phase = stepConv, phSetup
	m.setSetupContent()
	return nil
}

func (m *Model) setSetupContent() {
	m.viewport.Width, m.viewport.Height = m.bodyWidth(), m.viewportHeight()
	m.viewport.SetContent(prompt.Clean(m.agent.Setup))
	m.viewport.GotoTop()
}

// editConv starts the connection form from its first field, keeping the
// values entered so far.
func (m *Model) editConv() tea.Cmd {
	m.step = stepConv
	m.asked, m.picked, m.lookupMsg = map[string]bool{}, nil, ""
	m.verified, m.verifyErr, m.segStarts = map[string]string{}, nil, nil
	return m.nextSegment()
}

// currentInputs is the given inputs plus every answer so far.
func (m *Model) currentInputs() agent.Inputs {
	in := agent.Inputs{}
	for k, v := range m.cfg.Inputs {
		in[k] = v
	}
	for k := range m.asked {
		in[k] = *m.vals[k]
	}
	return in
}

// wanted reports whether f is still to be asked given in.
func (m *Model) wanted(f agent.Field, in agent.Inputs) bool {
	return !m.given[f.Key] && !m.asked[f.Key] && f.Applies(in)
}

// nextSegment shows the next form: the first field still to be asked and
// the ones after it that do not depend on anything on the same form, so a
// field shown or hidden by an answer (Postgres host after "storage") is
// decided once that answer is in. A field with a Lookup gets a form of its
// own, after its list has been fetched. A field with a Verify ends its
// form: it is checked live before the next form is built, so a later
// field (a list of warehouses) can rely on it (the token). With nothing
// left to ask, the inputs are validated and the connection checked.
func (m *Model) nextSegment() tea.Cmd {
	in := m.currentInputs()
	fields := m.agent.Fields
	start := -1
	for i, f := range fields {
		if m.wanted(f, in) {
			start = i
			break
		}
	}
	if start < 0 {
		return m.validateAndCheck()
	}
	if fields[start].Lookup != nil {
		return m.startLookup(fields[start])
	}
	seg := []agent.Field{fields[start]}
	for _, g := range fields[start+1:] {
		if seg[len(seg)-1].Verify != nil || g.Lookup != nil || dependsOn(g, seg, in) {
			break
		}
		if m.wanted(g, in) {
			seg = append(seg, g)
		}
	}
	m.segment, m.lookupOps, m.lookupMsg = seg, nil, ""
	return m.showSegment("")
}

// showSegment builds the form for m.segment (a list when it is a lookup
// with options), with the focus on the field focus names, or the first.
func (m *Model) showSegment(focus string) tea.Cmd {
	var hf []huh.Field
	at := 0
	for i, f := range m.segment {
		var opts [][2]string
		if f.Lookup != nil {
			opts = m.lookupOps
		}
		hf = append(hf, m.huhField(f, opts))
		if f.Key == focus {
			at = i
		}
	}
	msg := m.lookupMsg
	cmds := []tea.Cmd{m.setForm(m.newForm(huh.NewGroup(hf...)), fkConv)}
	m.lookupMsg = msg
	for range at {
		cmds = append(cmds, m.form.NextField())
	}
	if i := m.fieldIndex(m.segment[0].Key); len(m.segStarts) == 0 || m.segStarts[len(m.segStarts)-1] != i {
		m.segStarts = append(m.segStarts, i)
	}
	return tea.Batch(cmds...)
}

func (m *Model) fieldIndex(key string) int {
	for i, f := range m.agent.Fields {
		if f.Key == key {
			return i
		}
	}
	return -1
}

// prevSegment goes back from a connection form to the one before (esc):
// its answers and everything after them are asked again, values kept.
func (m *Model) prevSegment() tea.Cmd {
	m.segStarts = m.segStarts[:len(m.segStarts)-1]
	from := m.segStarts[len(m.segStarts)-1]
	m.segStarts = m.segStarts[:len(m.segStarts)-1]
	m.forget(from)
	return tea.Batch(tea.ClearScreen, m.nextSegment())
}

// forget unmarks every field from index from on, and their findings.
func (m *Model) forget(from int) {
	for _, f := range m.agent.Fields[from:] {
		delete(m.asked, f.Key)
		delete(m.verified, f.Key)
	}
	m.verifyErr = nil
	var kept []string
	for _, p := range m.picked {
		keep := true
		for _, f := range m.agent.Fields[from:] {
			keep = keep && !strings.HasPrefix(p, prompt.Clean(f.Label)+": ")
		}
		if keep {
			kept = append(kept, p)
		}
	}
	m.picked = kept
}

// segmentDone moves on from an answered connection form: its last field's
// Verify first, when it has one and applies.
func (m *Model) segmentDone() tea.Cmd {
	for _, f := range m.segment {
		m.asked[f.Key] = true
	}
	last := m.segment[len(m.segment)-1]
	if last.Verify != nil && last.Applies(m.currentInputs()) {
		return m.startVerify(last)
	}
	m.verifyErr = nil
	return m.nextSegment()
}

func (m *Model) startVerify(f agent.Field) tea.Cmd {
	ctx, id := m.startOp(opVerify, "Checking "+lowerFirst(prompt.Clean(f.Label))+"…", m.cfg.CheckTimeout)
	in, timeout := cloneInputs(m.currentInputs()), m.cfg.CheckTimeout
	return m.background(func() tea.Msg {
		fact, err := f.Verify(ctx, in)
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%w (no answer within %s)", err, timeout)
		}
		return verifyDoneMsg{id, f.Key, fact, err}
	}, func(err error) tea.Msg { return verifyDoneMsg{id: id, key: f.Key, err: err} })
}

// verifyDone moves on with the fact, or shows the form again with the
// error, the focus on the field that failed.
func (m *Model) verifyDone(key, fact string, err error) tea.Cmd {
	if err != nil {
		m.fieldFailed(key, "verify", err)
		for _, f := range m.segment {
			delete(m.asked, f.Key)
		}
		delete(m.verified, key)
		m.verifyErr = err
		return m.showSegment(key)
	}
	telemetry.FieldVerified(m.agent.ID, key)
	m.verified[key], m.verifyErr = fact, nil
	return m.nextSegment()
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if len(r) > 1 && r[1] >= 'a' && r[1] <= 'z' {
		r[0] = []rune(strings.ToLower(string(r[0])))[0]
	}
	return string(r)
}

// dependsOn reports whether g's When changes with any value the fields in
// seg could take (their choices, default, empty or something typed).
func dependsOn(g agent.Field, seg []agent.Field, in agent.Inputs) bool {
	if g.When == nil {
		return false
	}
	base := g.Applies(in)
	for _, s := range seg {
		try := append([]string{"", "x", s.Default}, s.Choices...)
		for _, v := range try {
			alt := agent.Inputs{}
			for k, x := range in {
				alt[k] = x
			}
			alt[s.Key] = v
			if g.Applies(alt) != base {
				return true
			}
		}
	}
	return false
}

// huhField is the form field for f; options, when set, make it a list.
func (m *Model) huhField(f agent.Field, options [][2]string) huh.Field {
	val := m.vals[f.Key]
	title, help := prompt.Clean(f.Label), prompt.Clean(f.Help)
	pick := func(opts []huh.Option[string]) huh.Field {
		found := false
		for _, o := range opts {
			found = found || o.Value == *val
		}
		if !found && len(opts) > 0 {
			*val = opts[0].Value
		}
		return huh.NewSelect[string]().Key(f.Key).Title(title).Description(help).Options(opts...).Value(val)
	}
	switch {
	case options != nil:
		opts := make([]huh.Option[string], len(options))
		for i, o := range options {
			opts[i] = huh.NewOption(prompt.Clean(o[1]), o[0])
		}
		return pick(opts)
	case len(f.Choices) > 0:
		opts := make([]huh.Option[string], len(f.Choices))
		for i, c := range f.Choices {
			opts[i] = huh.NewOption(prompt.Clean(c), c)
		}
		return pick(opts)
	}
	in := huh.NewInput().Key(f.Key).Title(title).Description(help).Value(val)
	if f.Secret {
		in = in.EchoMode(huh.EchoModePassword)
	}
	return in.Validate(func(s string) error {
		err := f.Check(s)
		if !f.Optional {
			if rerr := required(s); rerr != nil {
				err = rerr
			}
		}
		if err != nil {
			m.fieldFailed(f.Key, "validate", err, s)
		}
		return err
	})
}

func (m *Model) startLookup(f agent.Field) tea.Cmd {
	m.lookup = &f
	ctx, id := m.startOp(opLookup, "Listing "+strings.ToLower(prompt.Clean(f.Label))+"…", m.cfg.CheckTimeout)
	in := m.currentInputs()
	return m.background(func() tea.Msg {
		opts, err := f.Lookup(ctx, in)
		return lookupDoneMsg{id, opts, err}
	}, func(err error) tea.Msg { return lookupDoneMsg{id: id, err: err} })
}

// lookupDone shows the list, takes the only entry, or falls back to typing.
func (m *Model) lookupDone(opts [][2]string, err error) tea.Cmd {
	f := *m.lookup
	switch {
	case err != nil:
		m.lookupMsg = "could not list them (" + short(err) + "); type it instead"
		opts = nil
	case len(opts) == 1:
		*m.vals[f.Key] = opts[0][0]
		m.segment, m.lookupOps = []agent.Field{f}, opts
		m.picked = append(m.picked, prompt.Clean(f.Label)+": "+prompt.Clean(opts[0][1]))
		return m.segmentDone()
	case len(opts) == 0:
		opts = nil
	}
	m.segment, m.lookupOps = []agent.Field{f}, opts
	return m.showSegment("")
}

// validateAndCheck validates the inputs, then runs the agent's check.
func (m *Model) validateAndCheck() tea.Cmd {
	in := m.currentInputs()
	if err := m.agent.Validate(in); err != nil {
		m.fieldFailed(telemetry.FieldOf(err), "validate", err)
		m.conn = checkState{v: vFailed, err: err}
		m.phase = phChecked
		return nil
	}
	m.inputs = in
	if m.agent.Check == nil {
		m.conn = checkState{v: vUnverified}
		m.phase = phChecked
		return nil
	}
	m.conn = checkState{}
	ctx, id := m.startOp(opCheck, "Checking connection…", m.cfg.CheckTimeout)
	a, opts, timeout := *m.agent, m.opts(), m.cfg.CheckTimeout
	in = cloneInputs(in)
	return m.background(func() tea.Msg {
		res, err := a.Check(ctx, in, opts)
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%w (no answer within %s)", err, timeout)
		}
		return checkDoneMsg{id, res, err}
	}, func(err error) tea.Msg { return checkDoneMsg{id: id, err: err} })
}

func (m *Model) checkDone(res agent.CheckResult, err error) tea.Cmd {
	st := m.state(m.step)
	st.res, st.err, st.v = res, err, vOK
	if err != nil {
		st.v = vFailed
	}
	telemetry.CheckFinished(m.agent.ID, m.viewName(), err, len(res.Warnings), time.Since(m.opStart))
	m.setState(m.step, st)
	m.phase = phChecked
	return nil
}

func (m *Model) showQueries() tea.Cmd {
	in := m.inputs
	if in == nil {
		in = m.currentInputs()
	}
	qs := m.agent.Plan(in, m.opts())
	var b strings.Builder
	for i, q := range qs {
		fmt.Fprintf(&b, "-- %d. %s\n%s;\n\n", i+1, prompt.Clean(q.Purpose), prompt.Clean(q.Statement))
	}
	m.backTo, m.phase = m.phase, phQueries
	m.viewport.Width, m.viewport.Height = m.bodyWidth(), m.viewportHeight()
	m.viewport.SetContent(strings.TrimRight(b.String(), "\n"))
	m.viewport.GotoTop()
	return nil
}

func (m *Model) queryCount() int {
	in := m.inputs
	if in == nil {
		in = m.currentInputs()
	}
	return len(m.agent.Plan(in, m.opts()))
}

// --- dbt and context steps ----------------------------------------------

func (m *Model) enterDBT() tea.Cmd {
	m.step = stepDBT
	if !m.dbtSeen && strings.TrimSpace(m.dbtVal) != "" {
		m.dbtSeen = true
		return m.runDirCheck(stepDBT, expandHome(strings.TrimSpace(m.dbtVal)))
	}
	m.dbtSeen = true
	return m.showDirForm(stepDBT)
}

func (m *Model) enterCtx() tea.Cmd {
	m.step = stepCtx
	if !m.ctxSeen && strings.TrimSpace(m.ctxVal) != "" {
		m.ctxSeen = true
		return m.runDirCheck(stepCtx, expandHome(strings.TrimSpace(m.ctxVal)))
	}
	m.ctxSeen = true
	return m.showDirForm(stepCtx)
}

func (m *Model) showDirForm(s step) tea.Cmd {
	m.step = s
	var in *huh.Input
	if s == stepDBT {
		in = huh.NewInput().Title("Path to your dbt project").
			Description("Models, sources, semantic models and docs show what your agent could have known.\nprofiles.yml, .env files and keys are never read; secrets in YAML are masked.").
			Value(&m.dbtVal)
	} else {
		in = huh.NewInput().Title("Path to the " + prompt.Clean(m.agent.Context.Label)).
			Description(prompt.Clean(m.agent.Context.Help)).Value(&m.ctxVal)
	}
	in = in.Placeholder("Enter to skip").Validate(validDir)
	fk := fkDBT
	if s == stepCtx {
		fk = fkCtx
	}
	return m.setForm(m.newForm(huh.NewGroup(in)), fk)
}

func (m *Model) dirSubmitted(s step) tea.Cmd {
	v := m.dbtVal
	if s == stepCtx {
		v = m.ctxVal
	}
	v = strings.TrimSpace(v)
	if v == "" {
		m.setState(s, checkState{v: vSkipped})
		return m.nextStep()
	}
	return m.runDirCheck(s, expandHome(v))
}

func (m *Model) runDirCheck(s step, dir string) tea.Cmd {
	m.step = s
	var check func(string) (agent.CheckResult, error)
	label := "Checking the dbt project…"
	if s == stepDBT {
		check = m.cfg.CheckDBT
	} else {
		check = m.agent.Context.Check
		label = "Checking the " + prompt.Clean(m.agent.Context.Label) + "…"
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		err := fmt.Errorf("%s is not a folder", dir)
		telemetry.CheckFinished(m.agent.ID, m.viewName(), err, 0, 0)
		m.setState(s, checkState{v: vFailed, dir: dir, err: err})
		m.phase = phChecked
		return nil
	}
	if check == nil {
		m.setState(s, checkState{v: vUnverified, dir: dir})
		m.phase = phChecked
		return nil
	}
	m.setState(s, checkState{dir: dir})
	_, id := m.startOp(opCheck, label, m.cfg.CheckTimeout)
	return m.background(func() tea.Msg {
		res, err := check(dir)
		return checkDoneMsg{id, res, err}
	}, func(err error) tea.Msg { return checkDoneMsg{id: id, err: err} })
}

// --- Review step --------------------------------------------------------

func (m *Model) enterReview() tea.Cmd {
	m.step = stepReview
	emailHelp := "André emails you the top issues within a few days"
	if !m.cfg.Upload {
		emailHelp = "optional: recorded in the bundle, for when you send it"
	}
	f := m.newForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Capture window").
			Description("Conversations older than this are not read.").
			Options(m.windowOptions()...).Value(&m.window),
		huh.NewSelect[int]().Title("At most how many conversations?").
			Description(m.capHelp()).
			Options(m.capOptions()...).Value(&m.maxConv),
	), huh.NewGroup(
		huh.NewConfirm().Title("Pseudonymise people?").
			Description("Replaces user ids, names, emails and IP addresses with stable pseudonyms.\nQuestions and answers stay as they are.").
			Affirmative("Yes").Negative("No, keep real identities").Value(&m.anonymize),
		huh.NewConfirm().Title("Include query results?").
			Description("No: the rows your agent's queries returned are left out; only columns and row counts are kept.").
			Affirmative("Yes").Negative("No").Value(&m.results),
		huh.NewConfirm().Title("Include the user directory?").
			Description("No: the agent's list of users (names, emails) is left out. Turns keep a user id either way.").
			Affirmative("Yes").Negative("No").Value(&m.users),
		huh.NewInput().Title("Your email").Description(emailHelp).Value(&m.email).Validate(validEmail(m.cfg.Upload)),
	))
	return m.setForm(f, fkReview)
}

func (m *Model) opts() agent.Options {
	o := m.cfg.Opts
	o.AnonymizeUsers, o.IncludeResults, o.IncludeUsers = m.anonymize, m.results, m.users
	o.Since, o.MaxConversations = m.since(time.Now()), m.maxConv
	return o
}

// windowChoices are the capture windows offered on the review step, by
// their --since spelling. "flag" is a --since value that is none of them.
var windowChoices = [][2]string{{"30d", "30 days"}, {"90d", "90 days"}, {"6m", "6 months"}, {"all", "all history"}}

// defaultWindow is the choice matching the --since text, or "flag".
func defaultWindow(since string) string {
	s := strings.ToLower(strings.TrimSpace(since))
	if s == "" {
		s = "90d"
	}
	for _, c := range windowChoices {
		if c[0] == s {
			return s
		}
	}
	return "flag"
}

func (m *Model) windowOptions() []huh.Option[string] {
	var out []huh.Option[string]
	for _, c := range windowChoices {
		out = append(out, huh.NewOption(c[1], c[0]))
	}
	if m.window == "flag" || defaultWindow(m.cfg.Since) == "flag" {
		label := "since " + m.cfg.Opts.Since.Format("2006-01-02")
		if m.cfg.Opts.Since.IsZero() {
			label = "all history"
		}
		out = append(out, huh.NewOption(label+" (--since "+prompt.Clean(m.cfg.Since)+")", "flag"))
	}
	return out
}

// since is the start of the chosen window; zero for all history.
func (m *Model) since(now time.Time) time.Time {
	switch m.window {
	case "30d":
		return now.AddDate(0, 0, -30)
	case "90d":
		return now.AddDate(0, 0, -90)
	case "6m":
		return now.AddDate(0, -6, 0)
	case "all":
		return time.Time{}
	}
	return m.cfg.Opts.Since
}

// capChoices are the --max-conversations values offered; 0 is no cap.
var capChoices = []int{500, 2000, 10000, 0}

func (m *Model) capOptions() []huh.Option[int] {
	label := func(n int) string {
		if n == 0 {
			return "no cap (can be slow and large)"
		}
		return agent.Thousands(int64(n)) + " most recent"
	}
	var out []huh.Option[int]
	listed := false
	for _, n := range capChoices {
		out = append(out, huh.NewOption(label(n), n))
		listed = listed || n == m.maxConv
	}
	if !listed {
		note := " (--max-conversations)"
		if !m.cfg.CapSet && m.agent != nil {
			note = " (recommended for " + prompt.Clean(m.agent.Name) + ")"
		}
		out = append([]huh.Option[int]{huh.NewOption(label(m.maxConv)+note, m.maxConv)}, out...)
	}
	return out
}

// capHelp shows the connection check's count, so the cap can be chosen
// against it.
func (m *Model) capHelp() string {
	if n := m.conn.res.Conversations; n != nil {
		return fmt.Sprintf("%s conversations %s at the connection check.", agent.Thousands(*n), agent.Window(m.cfg.Opts))
	}
	return "The most recent conversations in the window are read."
}

func (m *Model) selection() Selection {
	s := Selection{Agent: *m.agent, Inputs: cloneInputs(m.inputs), Opts: m.opts(), Email: m.normEmail(m.email)}
	if m.dbt.v == vOK || m.dbt.v == vUnverified {
		s.DBTDir = m.dbt.dir
	}
	if m.agent.Context != nil && (m.ctxc.v == vOK || m.ctxc.v == vUnverified) {
		s.CtxDir = m.ctxc.dir
	}
	return s
}

func (m *Model) startCollect() tea.Cmd {
	m.notes, m.bundle = nil, nil
	ctx, id := m.startOp(opCollect, "Collecting…", 0)
	ch := make(chan tea.Msg, 16)
	m.ch = ch
	sel, collect := m.selection(), m.cfg.Collect
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(ch)
		send := func(msg tea.Msg) {
			select {
			case ch <- msg:
			case <-ctx.Done():
			}
		}
		var b Bundle
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("internal error: %v", r)
				}
			}()
			b, err = collect(ctx, sel, func(s string) { send(progressMsg{id, s}) })
		}()
		if ctx.Err() != nil && err == nil && b.Path != "" {
			os.Remove(b.Path) // stopped after the bundle was written: nobody saw it
		}
		send(collectDoneMsg{id, b, err})
	}()
	return tea.Batch(m.spin.Tick, m.waitCh())
}

func (m *Model) collectDone(b Bundle, err error) tea.Cmd {
	if err != nil {
		telemetry.CollectFailed(m.agent.ID, err)
		m.collectErr = err
		m.phase = phCollectErr
		return nil
	}
	b.Stats.Duration = time.Since(m.opStart)
	telemetry.Collected(m.agent.ID, b.Stats)
	m.bundle, m.menu, m.phase = &b, 0, phSummary
	m.outcome.Bundle = m.bundle
	return tea.ClearScreen
}

// discardBundle removes the bundle just written and returns to the review,
// so changed choices are collected afresh.
func (m *Model) discardBundle() tea.Cmd {
	if m.bundle != nil {
		os.Remove(m.bundle.Path)
	}
	m.bundle, m.outcome.Bundle = nil, nil
	return tea.Batch(tea.ClearScreen, m.enterReview())
}

func (m *Model) startUpload() tea.Cmd {
	ctx, id := m.startOp(opUpload, "Uploading…", 0)
	path, email, send := m.bundle.Path, m.normEmail(m.email), m.cfg.Send
	return m.background(func() tea.Msg {
		if send == nil {
			return uploadDoneMsg{id: id, err: errors.New("uploading is not available")}
		}
		r, err := send(ctx, path, email)
		return uploadDoneMsg{id, r, err}
	}, func(err error) tea.Msg { return uploadDoneMsg{id: id, err: err} })
}

// --- navigation ---------------------------------------------------------

// formDone acts on a submitted form.
func (m *Model) formDone() tea.Cmd {
	switch m.fk {
	case fkOther:
		return m.otherNamed()
	case fkOtherEmail:
		return m.startRequest()
	case fkConv:
		return m.segmentDone()
	case fkDBT:
		return m.dirSubmitted(stepDBT)
	case fkCtx:
		return m.dirSubmitted(stepCtx)
	case fkReview:
		return m.startCollect()
	}
	return nil
}

// formBack leaves a form with esc: to the step before.
func (m *Model) formBack() tea.Cmd {
	switch m.fk {
	case fkOther:
		m.phase, m.other = phList, 0
	case fkOtherEmail:
		return m.showOtherForm()
	case fkConv:
		m.phase = phSetup
		m.setSetupContent()
	case fkDBT:
		return m.reenter(stepConv)
	case fkCtx:
		return m.reenter(stepDBT)
	case fkReview:
		if m.agent.Context != nil {
			return m.reenter(stepCtx)
		}
		return m.reenter(stepDBT)
	}
	return tea.ClearScreen
}

// reenter shows step s as it was left: its results, or its form.
func (m *Model) reenter(s step) tea.Cmd {
	m.step = s
	switch s {
	case stepConv:
		m.phase = phChecked
	case stepDBT, stepCtx:
		if v := m.state(s).v; v == vSkipped || v == vPending {
			return tea.Batch(tea.ClearScreen, m.showDirForm(s))
		}
		m.phase = phChecked
	}
	return tea.ClearScreen
}

// nextStep moves on from a finished step.
func (m *Model) nextStep() tea.Cmd {
	var cmd tea.Cmd
	switch m.step {
	case stepConv:
		cmd = m.enterDBT()
	case stepDBT:
		if m.agent.Context != nil {
			cmd = m.enterCtx()
		} else {
			cmd = m.enterReview()
		}
	case stepCtx:
		cmd = m.enterReview()
	}
	return tea.Batch(tea.ClearScreen, cmd)
}

func (m *Model) editStep() tea.Cmd {
	switch m.step {
	case stepConv:
		return m.editConv()
	case stepDBT, stepCtx:
		return m.showDirForm(m.step)
	}
	return nil
}

func (m *Model) retryStep() tea.Cmd {
	switch m.step {
	case stepConv:
		return m.validateAndCheck()
	case stepDBT, stepCtx:
		if d := m.state(m.step).dir; d != "" {
			return m.runDirCheck(m.step, d)
		}
		return m.showDirForm(m.step)
	}
	return nil
}

// cancelBusy stops the running operation (esc) and goes back.
func (m *Model) cancelBusy() tea.Cmd {
	op := m.op
	m.cancelOp()
	switch op {
	case opRequest:
		if m.other == 2 {
			return m.showOtherEmail()
		}
		return m.showOtherForm()
	case opVerify:
		// Back to the form, on the field that was being checked.
		for _, f := range m.segment {
			delete(m.asked, f.Key)
		}
		m.verifyErr = nil
		return m.showSegment(m.segment[len(m.segment)-1].Key)
	case opCollect:
		return m.enterReview()
	case opUpload:
		m.phase = phSummary
		return nil
	}
	switch m.step {
	case stepConv:
		m.conn = checkState{}
		return m.editConv()
	case stepDBT, stepCtx:
		m.setState(m.step, checkState{})
		return m.showDirForm(m.step)
	}
	return nil
}

func (m *Model) state(s step) checkState {
	switch s {
	case stepConv:
		return m.conn
	case stepDBT:
		return m.dbt
	case stepCtx:
		return m.ctxc
	}
	return checkState{}
}

func (m *Model) setState(s step, st checkState) {
	switch s {
	case stepConv:
		m.conn = st
	case stepDBT:
		m.dbt = st
	case stepCtx:
		m.ctxc = st
	}
}

// --- forms --------------------------------------------------------------

func (m *Model) newForm(groups ...*huh.Group) *huh.Form {
	return huh.NewForm(groups...).WithTheme(theme()).WithShowHelp(false).WithWidth(min(m.bodyWidth(), 88))
}

func (m *Model) setForm(f *huh.Form, fk formKind) tea.Cmd {
	m.form, m.fk, m.phase, m.lookupMsg = f, fk, phForm, ""
	return f.Init()
}

func required(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("this one is needed")
	}
	return nil
}

func validEmail(needed bool) func(string) error {
	return func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" && !needed {
			return nil
		}
		if _, err := mail.ParseAddress(s); err != nil {
			return errors.New("that doesn't look like an email address")
		}
		return nil
	}
}

func validDir(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	p := expandHome(s)
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a folder", prompt.Clean(p))
	}
	return nil
}

func (m *Model) normEmail(s string) string {
	if a, err := mail.ParseAddress(strings.TrimSpace(s)); err == nil {
		return a.Address
	}
	return strings.TrimSpace(s)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}

func cloneInputs(in agent.Inputs) agent.Inputs {
	out := agent.Inputs{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// short is an error, cleaned, cut to one readable line.
func short(err error) string {
	s := strings.Join(strings.Fields(prompt.Clean(err.Error())), " ")
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "…"
	}
	return s
}
