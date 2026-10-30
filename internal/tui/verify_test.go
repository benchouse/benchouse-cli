package tui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/teatest"
)

func lipglossWidth(s string) int { return lipgloss.Width(s) }

// gamma is a Genie-shaped agent: host and token on one form, the token
// verified live, then a warehouse list that needs the verified token.
func gamma(listed *atomic.Int32) agent.Agent {
	plan := func(agent.Inputs, agent.Options) []bundle.Query {
		return []bundle.Query{{Purpose: "x", Statement: "SELECT 1"}}
	}
	return agent.Agent{ID: "gamma", Name: "Gamma", Status: agent.Supported, Setup: "Make a token.", Plan: plan,
		Check: func(context.Context, agent.Inputs, agent.Options) (agent.CheckResult, error) {
			return agent.CheckResult{Facts: []string{"all good"}}, nil
		},
		Fields: []agent.Field{
			{Key: "host", Label: "Host", Validate: agent.URL(false)},
			{Key: "token", Label: "Token", Secret: true,
				Verify: func(_ context.Context, in agent.Inputs) (string, error) {
					if in.Get("token") != "good" {
						return "", errors.New("the token was refused")
					}
					return "Token works: signed in as ana@" + in.Get("host"), nil
				}},
			{Key: "warehouse", Label: "Warehouse",
				Lookup: func(_ context.Context, in agent.Inputs) ([][2]string, error) {
					listed.Add(1)
					if in.Get("token") != "good" {
						return nil, errors.New("listed before the token was verified")
					}
					return [][2]string{{"w1", "first (RUNNING)"}, {"w2", "second (STOPPED)"}}, nil
				},
				Verify: func(_ context.Context, in agent.Inputs) (string, error) {
					return "SQL warehouse " + in.Get("warehouse"), nil
				}},
		}}
}

func TestFailingVerifyKeepsFocusAndShowsTheError(t *testing.T) {
	var listed atomic.Int32
	a := gamma(&listed)
	m := newModel(t, Config{Agents: []agent.Agent{a}, Agent: &a})
	m.Update(key("enter")) // setup → host + token
	if m.phase != phForm || len(m.segment) != 2 || m.segment[1].Key != "token" {
		t.Fatalf("first form should be host + token (the verified field ends it): %v", m.segment)
	}
	*m.vals["host"], *m.vals["token"] = "x.example.com", "bad"
	m.segmentDone()
	if m.phase != phBusy || m.op != opVerify || !strings.Contains(m.View(), "Checking token…") {
		t.Fatalf("no live check: phase %v op %v\n%s", m.phase, m.op, m.View())
	}
	if listed.Load() != 0 {
		t.Fatal("the warehouses were listed before the token was checked")
	}
	m.Update(verifyDoneMsg{id: m.opID, key: "token", err: errors.New("the token was refused")})
	v := m.View()
	if m.phase != phForm || !strings.Contains(v, "✗ the token was refused") {
		t.Fatalf("error not shown on the form:\n%s", v)
	}
	if got := m.form.GetFocusedField().GetKey(); got != "token" {
		t.Errorf("focus on %q, want token", got)
	}
	if m.asked["host"] || m.asked["token"] {
		t.Error("a failed form stays unanswered")
	}
	if !strings.Contains(v, "x.example.com") {
		t.Errorf("host value lost:\n%s", v)
	}
	// Fixed: the fact is shown and the next form (the list) is built.
	*m.vals["token"] = "good"
	m.segmentDone()
	m.Update(verifyDoneMsg{id: m.opID, key: "token", fact: "Token works: signed in as ana"})
	if m.phase != phBusy || m.op != opLookup {
		t.Fatalf("verified token did not lead to the warehouse list: phase %v op %v", m.phase, m.op)
	}
	m.Update(lookupDoneMsg{id: m.opID, opts: [][2]string{{"w1", "first"}, {"w2", "second"}}})
	if v := m.View(); m.phase != phForm || !strings.Contains(v, "✓ Token works: signed in as ana") || !strings.Contains(v, "second") {
		t.Errorf("fact or list missing:\n%s", v)
	}
	// esc on the list goes back to host + token, not to the setup text.
	m.Update(key("esc"))
	if m.phase != phForm || len(m.segment) != 2 || m.segment[0].Key != "host" {
		t.Errorf("esc did not go to the previous form: phase %v segment %v", m.phase, m.segment)
	}
	if strings.Contains(m.View(), "Token works") {
		t.Error("the fact of a form asked again should go")
	}
	m.Update(key("esc"))
	if m.phase != phSetup {
		t.Errorf("esc on the first form: phase %v, want the setup text", m.phase)
	}
}

func TestEscWhileVerifyingReturnsToTheField(t *testing.T) {
	var listed atomic.Int32
	a := gamma(&listed)
	m := newModel(t, Config{Agents: []agent.Agent{a}, Agent: &a})
	m.Update(key("enter"))
	*m.vals["host"], *m.vals["token"] = "x.example.com", "good"
	m.segmentDone()
	m.Update(key("esc"))
	if m.phase != phForm || m.op != opNone || m.form.GetFocusedField().GetKey() != "token" {
		t.Errorf("esc while checking: phase %v op %v", m.phase, m.op)
	}
	// A late answer of the cancelled check is dropped.
	m.Update(verifyDoneMsg{id: m.opID, key: "token", fact: "late"})
	if strings.Contains(m.View(), "late") {
		t.Error("the cancelled check's answer was shown")
	}
}

func TestValidateRunsOnTheField(t *testing.T) {
	var listed atomic.Int32
	a := gamma(&listed)
	cfg := Config{Agents: []agent.Agent{a}, Agent: &a}
	tm := teatest.NewTestModel(t, New(context.Background(), cfg), teatest.WithInitialTermSize(100, 40))
	wait := func(s string) {
		t.Helper()
		teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte(s)) },
			teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
	}
	wait("Make a token.")
	tm.Send(key("enter"))
	wait("Host")
	tm.Type("http://db.example.com")
	tm.Send(key("enter"))
	wait("credentials are only sent over https")
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlU}) // clear the line
	tm.Type("db.example.com")
	tm.Send(key("enter"))
	time.Sleep(100 * time.Millisecond)
	tm.Type("bad")
	tm.Send(key("enter"))
	wait("the token was refused")
	for range 3 {
		tm.Send(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	tm.Type("good")
	tm.Send(key("enter"))
	wait("second (STOPPED)")
	tm.Send(key("down"))
	tm.Send(key("enter"))
	wait("all good") // the full check still runs at the end
	tm.Send(key("ctrl+c"))
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(*Model)
	if fm.verified["token"] != "Token works: signed in as ana@db.example.com" {
		t.Errorf("token fact = %q", fm.verified["token"])
	}
	if fm.verified["warehouse"] != "SQL warehouse w2" {
		t.Errorf("warehouse fact = %q", fm.verified["warehouse"])
	}
	if listed.Load() != 1 {
		t.Errorf("listed %d times, want once (after the token was verified)", listed.Load())
	}
}

func TestBackHintsOnEveryStep(t *testing.T) {
	var listed atomic.Int32
	a := gamma(&listed)
	m := newModel(t, Config{Agents: []agent.Agent{a}})
	if strings.Contains(m.View(), "← esc") {
		t.Error("step 1 has nothing to go back to")
	}
	m.Update(key("enter")) // Gamma
	for _, enter := range []func(){
		func() {},
		func() { m.Update(key("enter")) },
		func() { m.enterDBT() },
		func() { m.enterReview() },
	} {
		enter()
		v := m.View()
		if !strings.Contains(v, "← esc") || !strings.Contains(v, "esc back") {
			t.Errorf("step %d, phase %v: no back hint:\n%s", m.stepNumber(), m.phase, v)
		}
	}
	if !strings.Contains(m.View(), "shift+tab previous field") {
		t.Error("previous field hint missing on a form")
	}
}

// Keys on the left, the run's state on the right; on a narrow screen the
// keys win and the state is cut from its left end.
func TestStatusBarKeysLeftStateRight(t *testing.T) {
	agents := fakeAgents(nil)
	m := newModel(t, Config{Agents: agents})
	last := func() string {
		l := m.statusLines()
		return l[len(l)-1]
	}
	if s := last(); !strings.HasPrefix(s, " ") || !strings.Contains(s, "↑/↓ choose") || strings.Contains(s, "connection") || strings.Contains(s, "no agent") {
		t.Errorf("step 1 status: %q", s)
	}
	a := agents[0]
	m = newModel(t, Config{Agents: agents, Agent: &a})
	s := last()
	if k, st := strings.Index(s, "enter next"), strings.Index(s, "connection"); k < 0 || st < k {
		t.Errorf("keys should come first: %q", s)
	}
	if strings.Contains(s, "Alpha") {
		t.Errorf("the status bar repeats the agent: %q", s)
	}
	m.Update(tea.WindowSizeMsg{Width: 70, Height: 30})
	s = last()
	if !strings.Contains(s, "ctrl+c quit") || !strings.HasSuffix(strings.TrimRight(s, " "), "people real") || !strings.Contains(s, "…") {
		t.Errorf("narrow status: %q", s)
	}
	if w := lipglossWidth(s); w > 70 {
		t.Errorf("status is %d wide on a 70-column screen", w)
	}
}
