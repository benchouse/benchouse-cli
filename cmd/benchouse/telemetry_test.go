package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/prompt"
	"github.com/benchouse/benchouse-cli/internal/telemetry"
)

// Credentials the scripted runs below are given; none may reach the stats.
const (
	tHost     = "db.secret-corp.io"
	tPassword = "hunter2-Sup3rS3cret"
	tEmail    = "jane@secret-corp.io"
)

var registerOnce sync.Once

func registerTelemetryAgent() {
	registerOnce.Do(func() {
		agent.Register(agent.Agent{ID: "telemetry-test", Name: "Telemetry test", Summary: "test",
			Fields: []agent.Field{{Key: "host", Label: "Host"}, {Key: "password", Label: "Password", Secret: true}},
			Plan:   func(agent.Inputs, agent.Options) []bundle.Query { return nil },
			Collect: func(_ context.Context, in agent.Inputs, _ agent.Options, emit agent.Emit) (agent.Result, error) {
				if in.Get("host") == "down."+tHost {
					return agent.Result{}, fmt.Errorf("failed to connect to `host=%s user=alice database=prod`: password %s rejected: FATAL: password authentication failed for user \"alice\" (SQLSTATE 28P01)",
						in.Get("host"), in.Get("password"))
				}
				for range 3 {
					if err := emit(bundle.Record{SourceKind: "x", RecordKind: "chat", Payload: json.RawMessage(`{"q":"` + in.Get("host") + `"}`)}); err != nil {
						return agent.Result{}, err
					}
				}
				return agent.Result{Conversations: 3}, nil
			}})
	})
}

type statsSink struct {
	*httptest.Server
	mu     sync.Mutex
	events []map[string]any
	raw    []byte
}

func newSink(t *testing.T) *statsSink {
	s := &statsSink{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Batch []map[string]any `json:"batch"`
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.events = append(s.events, body.Batch...)
		s.raw = append(s.raw, raw...)
		s.mu.Unlock()
	}))
	t.Cleanup(s.Close)
	t.Setenv(telemetry.EnvEndpoint, s.URL)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("APPDATA", home) // os.UserConfigDir on Windows
	t.Cleanup(func() { telemetry.StartWith(telemetry.Options{}) })
	return s
}

func (s *statsSink) names() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		n := e["event"].(string)
		if st, ok := e["properties"].(map[string]any)["step"].(string); ok {
			n += ":" + st
		}
		out = append(out, n)
	}
	return strings.Join(out, " ")
}

// runScripted runs the command as main does, without exiting.
func runScripted(args ...string) error {
	telemetry.Start("test")
	ctx := context.Background()
	err := run(ctx, args)
	telemetry.Quit(quitHow(ctx, err))
	telemetry.Close(statsBudget)
	return err
}

func (s *statsSink) noLeaks(t *testing.T, extra ...string) {
	t.Helper()
	for _, bad := range append([]string{tHost, "secret-corp", tPassword, "hunter2", tEmail, "alice"}, extra...) {
		if strings.Contains(string(s.raw), bad) {
			t.Errorf("%q reached the usage stats:\n%s", bad, s.raw)
		}
	}
	for _, e := range s.events {
		p := e["properties"].(map[string]any)
		if p["mode"] != "scripted" || p["cli_version"] != "test" {
			t.Errorf("properties = %v", p)
		}
	}
}

func TestUsageStatsForScriptedCollect(t *testing.T) {
	registerTelemetryAgent()
	s := newSink(t)
	t.Setenv("BENCHOUSE_PASSWORD", tPassword)
	out := filepath.Join(t.TempDir(), "secret-corp-bundle.tar.gz")
	if err := runScripted("collect", "--agent", "telemetry-test", "--host", tHost, "--out", out, "--no-input", "--email", tEmail); err != nil {
		t.Fatal(err)
	}
	want := "cli_started cli_agent_selected cli_collected cli_step_viewed:summary cli_step_viewed:done cli_saved cli_quit"
	if got := s.names(); got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	c := s.events[2]["properties"].(map[string]any)
	if c["conversations"] != float64(3) || c["records"] != float64(3) || c["size_bucket"] != "<1MB" || c["anonymize"] != true {
		t.Errorf("collected = %v", c)
	}
	if q := s.events[6]["properties"].(map[string]any); q["how"] != "done" || q["last_step"] != "done" || q["agent"] != "telemetry-test" {
		t.Errorf("quit = %v", q)
	}
	s.noLeaks(t, out, filepath.Dir(out))
}

func TestUsageStatsForScriptedFailures(t *testing.T) {
	registerTelemetryAgent()
	s := newSink(t)
	t.Setenv("BENCHOUSE_PASSWORD", tPassword)
	dir := t.TempDir()
	// A required field missing, then a collection that fails.
	if err := runScripted("collect", "--agent", "telemetry-test", "--no-input"); err == nil {
		t.Fatal("ran without --host")
	}
	if err := runScripted("collect", "--agent", "telemetry-test", "--host", "down."+tHost, "--out", filepath.Join(dir, "b.tar.gz"), "--no-input"); err == nil {
		t.Fatal("the collection did not fail")
	}
	want := "cli_started cli_agent_selected cli_field_failed cli_quit cli_started cli_agent_selected cli_collect_failed cli_quit"
	if got := s.names(); got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	if p := s.events[2]["properties"].(map[string]any); p["field"] != "host" || p["kind"] != "validate" || p["category"] != telemetry.NotConfigured {
		t.Errorf("field failed = %v", p)
	}
	p := s.events[6]["properties"].(map[string]any)
	if p["category"] != telemetry.AuthFailed || !strings.Contains(p["message"].(string), "password authentication failed for user <name>") {
		t.Errorf("collect failed = %v", p)
	}
	if q := s.events[7]["properties"].(map[string]any); q["how"] != "error" {
		t.Errorf("quit = %v", q)
	}
	s.noLeaks(t, dir)
}

func TestUsageStatsOffInTestsByDefault(t *testing.T) {
	registerTelemetryAgent()
	s := newSink(t)
	os.Unsetenv(telemetry.EnvEndpoint)
	if err := runScripted("agents"); err != nil {
		t.Fatal(err)
	}
	if s.names() != "" {
		t.Errorf("events sent: %s", s.names())
	}
	entries, _ := os.ReadDir(os.Getenv("HOME"))
	if len(entries) != 0 {
		t.Errorf("files written: %v", entries)
	}
}

func TestQuitHow(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		ctx  context.Context
		err  error
		want string
	}{
		{context.Background(), nil, "done"},
		{canceled, errors.New("x"), "signal"},
		{context.Background(), fmt.Errorf("prompt: %w", prompt.ErrInterrupted), "ctrl_c"},
		{context.Background(), errors.New("boom"), "error"},
	} {
		if got := quitHow(c.ctx, c.err); got != c.want {
			t.Errorf("quitHow(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}
