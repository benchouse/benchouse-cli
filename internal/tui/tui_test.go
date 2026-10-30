package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
)

func isPG(in agent.Inputs) bool { return in.Get("storage") != "sqlite" }

// fakeAgents: alpha has a context folder and a field that depends on
// another, beta is experimental with one secret field.
func fakeAgents(check func(context.Context, agent.Inputs, agent.Options) (agent.CheckResult, error)) []agent.Agent {
	if check == nil {
		check = func(context.Context, agent.Inputs, agent.Options) (agent.CheckResult, error) {
			return agent.CheckResult{}, nil
		}
	}
	plan := func(agent.Inputs, agent.Options) []bundle.Query {
		return []bundle.Query{{Purpose: "chats", Statement: "SELECT 1"}, {Purpose: "parts", Statement: "SELECT 2"}}
	}
	return []agent.Agent{
		{ID: "alpha", Name: "Alpha", Summary: "the first", Status: agent.Supported, Setup: "Grant SELECT.",
			Fields: []agent.Field{
				{Key: "storage", Label: "Storage", Choices: []string{"postgres", "sqlite"}, Default: "postgres"},
				{Key: "host", Label: "Host", When: isPG},
				{Key: "path", Label: "Path", When: func(in agent.Inputs) bool { return !isPG(in) }},
			},
			Context: &agent.Context{Label: "alpha context folder", Help: "where alpha lives", Keep: func(string) bool { return true }},
			Plan:    plan, Check: check},
		{ID: "beta", Name: "Beta", Summary: "the second", Status: agent.Experimental, Setup: "Make a token.",
			Fields: []agent.Field{{Key: "token", Label: "Token", Secret: true}},
			Plan:   plan, Check: check},
	}
}

func key(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func newModel(t *testing.T, cfg Config) *Model {
	t.Helper()
	m := New(context.Background(), cfg)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return m
}

func TestAgentSelectionMovesWithArrows(t *testing.T) {
	m := newModel(t, Config{Agents: fakeAgents(nil)})
	if !strings.Contains(m.View(), "❯ Alpha") {
		t.Fatalf("first agent not selected:\n%s", m.View())
	}
	m.Update(key("down"))
	if m.cursor != 1 || !strings.Contains(m.View(), "❯ Beta") || strings.Contains(m.View(), "experimental") {
		t.Errorf("down: cursor %d\n%s", m.cursor, m.View())
	}
	m.Update(key("down"))
	if !strings.Contains(m.View(), "❯ Other") {
		t.Errorf("Other not reachable:\n%s", m.View())
	}
	m.Update(key("down")) // wraps
	m.Update(key("up"))
	m.Update(key("up"))
	if m.cursor != 1 {
		t.Errorf("cursor = %d, want 1", m.cursor)
	}
	m.Update(key("enter"))
	if m.agent == nil || m.agent.ID != "beta" || m.step != stepConv || m.phase != phSetup {
		t.Errorf("enter did not choose beta: step %v phase %v", m.step, m.phase)
	}
}

func TestHeaderStepCountWithAndWithoutContext(t *testing.T) {
	agents := fakeAgents(nil)
	for _, c := range []struct {
		a            agent.Agent
		conv, review string
	}{
		{agents[0], "Step 2/5  ·  Alpha conversations", "Step 5/5  ·  Review & upload"},
		{agents[1], "Step 2/4  ·  Beta conversations", "Step 4/4  ·  Review & upload"},
	} {
		a := c.a
		m := newModel(t, Config{Agents: agents, Agent: &a, Upload: true})
		if v := m.View(); !strings.Contains(v, c.conv) {
			t.Errorf("%s: header missing %q:\n%s", a.ID, c.conv, v)
		}
		m.enterReview()
		if v := m.View(); !strings.Contains(v, c.review) {
			t.Errorf("%s: header missing %q:\n%s", a.ID, c.review, v)
		}
	}
	m := newModel(t, Config{Agents: agents})
	if !strings.Contains(m.View(), "Step 1/5  ·  Agent") {
		t.Errorf("agent step header:\n%s", m.View())
	}
	m.Update(key("down"))
	if !strings.Contains(m.View(), "Step 1/4") {
		t.Errorf("step count does not follow the highlighted agent:\n%s", m.View())
	}
}

func TestCtrlCQuitsAndCancelsTheRunningCheck(t *testing.T) {
	agents := fakeAgents(nil)
	a := agents[1]
	m := newModel(t, Config{Agents: agents, Agent: &a, Inputs: agent.Inputs{"token": "t"}})
	ctx, _ := m.startOp(opCheck, "Checking connection…", time.Minute)
	_, cmd := m.Update(key("ctrl+c"))
	if cmd == nil {
		t.Fatal("no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("ctrl+c did not quit")
	}
	if ctx.Err() == nil {
		t.Errorf("the running check was not cancelled")
	}
	if !m.outcome.Interrupted || m.View() != "" {
		t.Errorf("outcome %+v, view %q", m.outcome, m.View())
	}
}

func TestStatusBarFollowsTheSteps(t *testing.T) {
	agents := fakeAgents(nil)
	a := agents[0]
	m := newModel(t, Config{Agents: agents, Agent: &a, Opts: agent.Options{AnonymizeUsers: true}})
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if v := m.View(); !strings.Contains(v, "connection … · dbt … · context … · people pseudonymised ") || strings.Contains(v, "Alpha · connection") {
		t.Errorf("initial status:\n%s", v)
	}
	m.asked = map[string]bool{"storage": true, "host": true}
	*m.vals["host"] = "db"
	m.validateAndCheck()
	m.Update(checkDoneMsg{id: m.opID, res: agent.CheckResult{Facts: []string{"ok"}}})
	m.step = stepDBT
	m.dirSubmitted(stepDBT) // empty: skipped
	m.anonymize = false
	if v := m.View(); !strings.Contains(v, "connection ✓ · dbt skipped · context … · people real") {
		t.Errorf("status after checks:\n%s", v)
	}
}

func TestCheckFindingsAreCleaned(t *testing.T) {
	agents := fakeAgents(nil)
	a := agents[1]
	m := newModel(t, Config{Agents: agents, Agent: &a, Inputs: agent.Inputs{"token": "t"}})
	a.Check = func(context.Context, agent.Inputs, agent.Options) (agent.CheckResult, error) {
		return agent.CheckResult{}, nil
	}
	m.agent = &a
	m.validateAndCheck()
	m.Update(checkDoneMsg{id: m.opID, res: agent.CheckResult{
		Facts:    []string{"Connected to evil\x1b]52;c;ZWNobyBwd25lZA==\x07 host"},
		Warnings: []string{"warn\x1b[2J ing"},
	}})
	v := m.View()
	if strings.Contains(v, "\x1b]52") || strings.Contains(v, "ZWNob") || strings.Contains(v, "\x1b[2J") {
		t.Errorf("escape reached the screen: %q", v)
	}
	if !strings.Contains(v, "✓ Connected to evil host") || !strings.Contains(v, "! warn ing") {
		t.Errorf("findings missing:\n%s", v)
	}
	// Errors are cleaned too.
	m.validateAndCheck()
	m.Update(checkDoneMsg{id: m.opID, err: errors.New("refused\x1b]0;pwned\x07 by server")})
	if v := m.View(); strings.Contains(v, "\x1b]0") || !strings.Contains(v, "refused by server") {
		t.Errorf("error not cleaned: %q", v)
	}
}

func TestCheckErrorGoesBackToTheFormKeepingValues(t *testing.T) {
	agents := fakeAgents(nil)
	a := agents[0]
	m := newModel(t, Config{Agents: agents, Agent: &a})
	m.Update(key("enter")) // setup → form: storage
	if m.phase != phForm || len(m.segment) != 1 || m.segment[0].Key != "storage" {
		t.Fatalf("first form: phase %v segment %v", m.phase, m.segment)
	}
	m.asked["storage"] = true
	m.nextSegment()
	if len(m.segment) != 1 || m.segment[0].Key != "host" {
		t.Fatalf("second form should hold host only, got %v", m.segment)
	}
	*m.vals["host"] = "db.example.com"
	m.asked["host"] = true
	m.validateAndCheck()
	m.Update(checkDoneMsg{id: m.opID, err: errors.New("connection refused")})
	if m.phase != phChecked || !strings.Contains(m.View(), "connection refused") || !strings.Contains(m.View(), "connection ✗") {
		t.Fatalf("error not shown:\n%s", m.View())
	}
	m.Update(key("enter")) // edit
	if m.phase != phForm || m.segment[0].Key != "storage" {
		t.Fatalf("enter did not go back to the form: phase %v", m.phase)
	}
	m.asked["storage"] = true
	m.nextSegment()
	if !strings.Contains(m.View(), "db.example.com") {
		t.Errorf("value lost:\n%s", m.View())
	}
	// r retries without the form.
	m.asked["host"] = true
	m.validateAndCheck()
	m.Update(checkDoneMsg{id: m.opID, err: errors.New("still refused")})
	m.Update(key("r"))
	if m.phase != phBusy || m.op != opCheck {
		t.Errorf("r did not retry: phase %v", m.phase)
	}
}

func TestNoCheckIsVerifiedWhileCollecting(t *testing.T) {
	agents := fakeAgents(nil)
	a := agents[1]
	a.Check = nil
	m := newModel(t, Config{Agents: agents, Agent: &a, Inputs: agent.Inputs{"token": "t"}})
	m.Update(key("enter")) // nothing left to ask
	v := m.View()
	if m.phase != phChecked || !strings.Contains(v, "will be verified while collecting") || !strings.Contains(v, "connection –") {
		t.Errorf("nil check:\n%s", v)
	}
	if !strings.Contains(v, "2 read-only queries") {
		t.Errorf("query count missing:\n%s", v)
	}
	m.Update(key("v"))
	if m.phase != phQueries || !strings.Contains(m.View(), "-- 1. chats") {
		t.Errorf("queries view:\n%s", m.View())
	}
	m.Update(key("esc"))
	if m.phase != phChecked {
		t.Errorf("esc from queries: %v", m.phase)
	}
}

func TestValidation(t *testing.T) {
	if required("  ") == nil || required("x") != nil {
		t.Error("required")
	}
	if validEmail(true)("") == nil || validEmail(true)("nope") == nil || validEmail(true)("a@b.co") != nil || validEmail(false)("") != nil {
		t.Error("validEmail")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	os.WriteFile(file, nil, 0o600)
	if validDir("") != nil || validDir(dir) != nil || validDir(file) == nil || validDir(filepath.Join(dir, "missing")) == nil {
		t.Error("validDir")
	}
	agents := fakeAgents(nil)
	host, storage := agents[0].Fields[1], agents[0].Fields[0]
	if !dependsOn(host, []agent.Field{storage}, agent.Inputs{"storage": "postgres"}) {
		t.Error("host should depend on storage")
	}
	if dependsOn(storage, []agent.Field{host}, agent.Inputs{}) {
		t.Error("storage has no condition")
	}
}

// TestGuidedRunEndToEnd drives the real program: Beta, a required secret
// left empty then filled, the check, dbt skipped, the review, a fake
// collection, Save only.
func TestGuidedRunEndToEnd(t *testing.T) {
	var gotIn agent.Inputs
	check := func(_ context.Context, in agent.Inputs, _ agent.Options) (agent.CheckResult, error) {
		gotIn = in
		return agent.CheckResult{Facts: []string{"Connected as reader"}}, nil
	}
	var sel Selection
	cfg := Config{Upload: true, Agents: fakeAgents(check), Opts: agent.Options{AnonymizeUsers: true, MaxConversations: 2000}, Since: "90d",
		Collect: func(_ context.Context, s Selection, progress func(string)) (Bundle, error) {
			sel = s
			progress("Reading Beta conversations...")
			return Bundle{Path: "/tmp/b.tar.gz", Rel: "b.tar.gz", Size: 2048, Summary: "  agent       beta\n  records     3\n"}, nil
		}}
	tm := teatest.NewTestModel(t, New(context.Background(), cfg), teatest.WithInitialTermSize(100, 40))
	wait := func(s string) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte(s)) },
			teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
	}
	wait("Which agent do you use?")
	tm.Send(key("down"))
	tm.Send(key("enter"))
	wait("Make a token.")
	tm.Send(key("enter"))
	wait("Token")
	tm.Send(key("enter")) // empty: refused
	wait("this one is needed")
	tm.Type("s3cret")
	tm.Send(key("enter"))
	wait("Connected as reader")
	tm.Send(key("enter")) // → dbt
	wait("Path to your dbt project")
	tm.Send(key("enter")) // skip
	wait("At most how many conversations?")
	tm.Send(key("enter")) // window: 90 days, the --since default
	time.Sleep(100 * time.Millisecond)
	tm.Send(key("down")) // cap: 2,000 → 10,000
	tm.Send(key("enter"))
	wait("Pseudonymise people?")
	time.Sleep(100 * time.Millisecond)
	tm.Send(key("enter"))              // yes
	tm.Send(key("enter"))              // no results
	tm.Send(key("enter"))              // no user directory
	time.Sleep(100 * time.Millisecond) // let the email field take focus
	tm.Type("me@example.com")
	tm.Send(key("enter"))
	wait("What's in the bundle")
	tm.Send(key("down")) // Save only
	tm.Send(key("enter"))
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(*Model)
	if fm.users {
		t.Error("the user directory was included without being asked for")
	}
	if fm.outcome.Result != Saved || fm.outcome.Bundle == nil || fm.outcome.Email != "me@example.com" {
		t.Errorf("outcome = %+v", fm.outcome)
	}
	if gotIn.Get("token") != "s3cret" || sel.Inputs.Get("token") != "s3cret" || !sel.Opts.AnonymizeUsers || sel.Opts.IncludeResults || sel.DBTDir != "" {
		t.Errorf("check saw %v; collect got %+v", gotIn, sel)
	}
	if sel.Opts.MaxConversations != 10000 {
		t.Errorf("cap = %d, want the 10,000 chosen", sel.Opts.MaxConversations)
	}
	if d := time.Since(sel.Opts.Since); d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Errorf("window starts %v ago, want 90 days", d)
	}
}

func TestOtherAgentIsRequested(t *testing.T) {
	var anon []string
	var got [3]string
	cfg := Config{Agents: fakeAgents(nil),
		RequestAgentAnonymous: func(_ context.Context, name string) (string, error) {
			anon = append(anon, name)
			return "ar_1", nil
		},
		RequestAgentEmail: func(_ context.Context, name, email, id string) error {
			got = [3]string{name, email, id}
			return nil
		}}
	tm := teatest.NewTestModel(t, New(context.Background(), cfg), teatest.WithInitialTermSize(100, 40))
	wait := func(s string) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte(s)) },
			teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
	}
	wait("Which agent do you use?")
	tm.Send(key("up")) // wraps to Other
	tm.Send(key("enter"))
	wait("Step 1/2")
	tm.Type("Hex Magic")
	tm.Send(key("enter"))
	wait("Step 2/2")
	tm.Type("me@example.com")
	tm.Send(key("enter"))
	wait("We'll email me@example.com when it's supported")
	tm.Send(key("enter"))
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(*Model)
	if fm.outcome.Result != Requested || got != [3]string{"Hex Magic", "me@example.com", "ar_1"} {
		t.Errorf("outcome %+v, request %v", fm.outcome, got)
	}
	if len(anon) != 1 || anon[0] != "Hex Magic" {
		t.Errorf("anonymous requests %v", anon)
	}
}

func TestOtherFlowStepsAndSkippedEmail(t *testing.T) {
	var anon []string
	emailed := false
	cfg := Config{Agents: fakeAgents(nil),
		RequestAgentAnonymous: func(_ context.Context, name string) (string, error) {
			anon = append(anon, name)
			return "", errors.New("network down") // not fatal
		},
		RequestAgentEmail: func(context.Context, string, string, string) error { emailed = true; return nil }}
	m := newModel(t, cfg)
	m.cursor = len(cfg.Agents)
	m.Update(key("enter"))
	v := m.View()
	if !strings.Contains(v, "Step 1/2  ·  Request an agent") || strings.Contains(v, "connection") {
		t.Errorf("step 1 header or status:\n%s", v)
	}
	m.otherName = "Hex"
	m.formDone()
	if m.phase != phBusy || m.op != opRequest {
		t.Fatalf("the name was not sent at once: phase %v", m.phase)
	}
	m.Update(anonDoneMsg{id: m.opID, name: "Hex"}) // what a failed call returns
	if m.phase != phForm || m.fk != fkOtherEmail || !strings.Contains(m.View(), "Step 2/2") {
		t.Fatalf("a failed anonymous request should still reach the email step:\n%s", m.View())
	}
	// esc goes back to step 1 with the name kept; unchanged, it is not sent again.
	m.Update(key("esc"))
	if m.fk != fkOther || !strings.Contains(m.View(), "Hex") || !strings.Contains(m.View(), "Step 1/2") {
		t.Fatalf("esc on step 2:\n%s", m.View())
	}
	m.formDone()
	if m.fk != fkOtherEmail || m.op != opNone {
		t.Errorf("an unchanged name was sent again: op %v", m.op)
	}
	m.otherEmail = ""
	m.formDone()
	v = m.View()
	if m.phase != phSent || !strings.Contains(v, "Thanks. We've noted Hex.") || strings.Contains(v, "We'll email") || !strings.Contains(v, "No email") {
		t.Errorf("skipped email:\n%s", v)
	}
	if emailed {
		t.Error("an email request without an email")
	}
	if !strings.Contains(v, "Step 2/2") {
		t.Errorf("done screen header:\n%s", v)
	}
}

func TestReviewChoicesDefaultToFlags(t *testing.T) {
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n := int64(1_000_000)
	m := New(context.Background(), Config{Since: "2026-01-01", Opts: agent.Options{Since: since, MaxConversations: 750}})
	m.conn.res.Conversations = &n
	if m.window != "flag" || !m.since(time.Now()).Equal(since) || m.maxConv != 750 {
		t.Errorf("window %q since %v cap %d", m.window, m.since(time.Now()), m.maxConv)
	}
	if got := len(m.windowOptions()); got != 5 {
		t.Errorf("window options = %d, want the four and the --since date", got)
	}
	if got := len(m.capOptions()); got != 5 {
		t.Errorf("cap options = %d, want the four and the flag's", got)
	}
	if !strings.Contains(m.windowText(), "counted 1,000,000 since 2026-01-01") {
		t.Errorf("window text = %q", m.windowText())
	}
	m.window, m.maxConv = "all", 0
	if o := m.opts(); !o.Since.IsZero() || o.MaxConversations != 0 {
		t.Errorf("opts = %+v", o)
	}
	if defaultWindow("") != "90d" || defaultWindow("6M") != "6m" {
		t.Error("defaultWindow")
	}
}
