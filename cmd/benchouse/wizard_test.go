package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/prompt"
)

func TestWizardReasksAfterAFailedVerify(t *testing.T) {
	var hosts []string
	a := agent.Agent{ID: "t", Name: "T", Fields: []agent.Field{
		{Key: "port", Label: "Port", Default: "5432", Validate: agent.Port},
		{Key: "host", Label: "Host"},
		{Key: "token", Label: "Token", Secret: true, Verify: func(_ context.Context, in agent.Inputs) (string, error) {
			hosts = append(hosts, in.Get("host"))
			if in.Get("token") != "good" {
				return "", errors.New("token refused")
			}
			return "signed in as ana", nil
		}},
	}}
	var out bytes.Buffer
	// port 99999 is refused offline, Enter takes the default; host h1, a
	// bad token; asked again from the port: Enter, Enter keeps h1, the
	// good token.
	p := prompt.NewFrom(strings.NewReader("99999\n\nh1\nbad\n\n\ngood\n"), &out)
	in := agent.Inputs{}
	if err := askConnection(context.Background(), p, a, in); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"a port is a number from 1 to 65535", "✗ token refused", "✓ signed in as ana", "Host [h1]"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if in.Get("port") != "5432" || in.Get("host") != "h1" || in.Get("token") != "good" {
		t.Errorf("inputs = %v", in)
	}
	if len(hosts) != 2 {
		t.Errorf("verified %d times, want 2", len(hosts))
	}
}

// Without prompts, Validate still applies to flag values and Verify does
// not run: Check covers it.
func TestFlagsAreValidatedNotVerified(t *testing.T) {
	verified := false
	a := agent.Agent{ID: "t", Name: "T", Fields: []agent.Field{
		{Key: "port", Label: "Port", Validate: agent.Port},
		{Key: "token", Label: "Token", Verify: func(context.Context, agent.Inputs) (string, error) {
			verified = true
			return "", nil
		}},
	}}
	if err := a.Validate(agent.Inputs{"port": "0", "token": "x"}); err == nil || !strings.Contains(err.Error(), "--port: a port is") {
		t.Errorf("err = %v", err)
	}
	if err := a.Validate(agent.Inputs{"port": "5432", "token": "x"}); err != nil || verified {
		t.Errorf("err = %v, verified %v", err, verified)
	}
}

// The name is sent before the email is asked for; the email request links
// to its id. A failed first call is not fatal, and the email can be skipped.
func TestPlainAgentRequest(t *testing.T) {
	type call struct {
		path string
		body map[string]any
	}
	calls := make(chan call, 4)
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		calls <- call{r.URL.Path, b}
		if r.URL.Path == "/api/v1/agent-request" {
			if fail {
				http.Error(w, "down", http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"ar_9"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pr, pw := io.Pipe()
	var out bytes.Buffer
	done := make(chan error)
	go func() { done <- requestAgent(context.Background(), prompt.NewFrom(pr, &out), srv.URL) }()
	pw.Write([]byte("Hex Magic\n"))
	first := <-calls // arrives before the email is typed
	if first.path != "/api/v1/agent-request" || first.body["agent"] != "Hex Magic" || first.body["source"] != "cli" {
		t.Fatalf("first call %+v", first)
	}
	pw.Write([]byte("me@example.com\n"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	second := <-calls
	ref, _ := second.body["ref"].(map[string]any)
	if second.path != "/api/v1/waitlist" || second.body["email"] != "me@example.com" || ref["content"] != "ar_9" {
		t.Errorf("second call %+v", second)
	}
	for _, want := range []string{"Step 1/2", "Step 2/2", "Thanks. We've noted Hex Magic.", "We'll email me@example.com"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// Network failure on the first call, then no email.
	fail = true
	out.Reset()
	if err := requestAgent(context.Background(), prompt.NewFrom(strings.NewReader("Hex\n\n"), &out), srv.URL); err != nil {
		t.Fatal(err)
	}
	<-calls
	select {
	case c := <-calls:
		t.Errorf("an email request without an email: %+v", c)
	default:
	}
	if !strings.Contains(out.String(), "No email — that's fine") || strings.Contains(out.String(), "We'll email") {
		t.Errorf("skipped email:\n%s", out.String())
	}
}
