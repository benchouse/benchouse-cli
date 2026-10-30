package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/telemetry"
	"github.com/benchouse/benchouse-cli/internal/upload"
	"github.com/charmbracelet/x/exp/teatest"
)

// TestUsageStatsForAFullRun drives a whole guided run (a failed check, a
// retry, a dbt folder, collect, upload) against a fake PostHog endpoint and
// checks the events, in order, and that no credential, host, path or
// email from the run reaches any of them.
func TestUsageStatsForAFullRun(t *testing.T) {
	var mu sync.Mutex
	var events []map[string]any
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Batch []map[string]any `json:"batch"`
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		mu.Lock()
		events = append(events, body.Batch...)
		mu.Unlock()
	}))
	defer sink.Close()
	telemetry.StartWith(telemetry.Options{Endpoint: sink.URL, Dir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { telemetry.StartWith(telemetry.Options{}) })
	telemetry.SetMode("tui")
	telemetry.Started("guided")

	const token = "s3cretTOKENabc123xyz"
	dbtDir := filepath.Join(t.TempDir(), "acme-analytics")
	if err := os.MkdirAll(dbtDir, 0o700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	check := func(_ context.Context, in agent.Inputs, _ agent.Options) (agent.CheckResult, error) {
		calls++
		if calls == 1 {
			return agent.CheckResult{}, fmt.Errorf(`snowflake http 401: {"code":"390100","message":"Incorrect username or password was specified for user ANALYTICS_SVC"} at https://acme-prod.snowflakecomputing.com with token %s`, in.Get("token"))
		}
		return agent.CheckResult{Facts: []string{"Connected as reader"}}, nil
	}
	cfg := Config{Upload: true, Agents: fakeAgents(check), Opts: agent.Options{AnonymizeUsers: true, MaxConversations: 2000}, Since: "90d",
		CheckDBT: func(dir string) (agent.CheckResult, error) {
			return agent.CheckResult{Facts: []string{"3 models in " + dir}, Warnings: []string{"no manifest in " + dir}}, nil
		},
		Collect: func(_ context.Context, s Selection, progress func(string)) (Bundle, error) {
			return Bundle{Path: filepath.Join(dbtDir, "b.tar.gz"), Rel: "b.tar.gz", Size: 2 << 20, Summary: "  agent       beta\n",
				Stats: telemetry.Collection{Conversations: 12, Records: 40, Sampled: true, FilesDBT: 3, Size: 2 << 20,
					IncludeResults: s.Opts.IncludeResults, IncludeUsers: s.Opts.IncludeUsers, Anonymize: s.Opts.AnonymizeUsers}}, nil
		},
		Send: func(context.Context, string, string) (upload.Receipt, error) {
			return upload.Receipt{ID: "up_1", Bytes: 2 << 20}, nil
		},
	}
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
	tm.Type(token)
	tm.Send(key("enter"))
	wait("Incorrect username")
	tm.Send(key("r"))
	wait("Connected as reader")
	tm.Send(key("enter")) // → dbt
	wait("Path to your dbt project")
	tm.Type(dbtDir)
	tm.Send(key("enter"))
	wait("3 models")
	tm.Send(key("enter")) // → review
	wait("At most how many conversations?")
	tm.Send(key("enter"))
	time.Sleep(100 * time.Millisecond)
	tm.Send(key("enter"))
	wait("Pseudonymise people?")
	time.Sleep(100 * time.Millisecond)
	tm.Send(key("enter"))
	tm.Send(key("enter"))
	tm.Send(key("enter"))
	time.Sleep(100 * time.Millisecond)
	tm.Type("me@example.com")
	tm.Send(key("enter"))
	wait("What's in the bundle")
	tm.Send(key("enter")) // Upload
	wait("up_1")
	tm.Send(key("enter"))
	tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second))
	telemetry.Quit("done")
	telemetry.Close(time.Second)

	mu.Lock()
	defer mu.Unlock()
	var got []string
	for _, e := range events {
		name := e["event"].(string)
		p := e["properties"].(map[string]any)
		if s, ok := p["step"].(string); ok {
			name += ":" + s
		}
		if k, ok := p["kind"].(string); ok {
			name += ":" + k
		}
		if ok, isBool := p["ok"].(bool); isBool {
			name += fmt.Sprintf(":%v", ok)
		}
		got = append(got, name)
	}
	want := []string{
		"cli_started",
		"cli_agent_selected",
		"cli_step_viewed:conversations",
		"cli_field_failed:validate",
		"cli_check_finished:conversations:false",
		"cli_check_finished:conversations:true",
		"cli_step_viewed:dbt",
		"cli_check_finished:dbt:true",
		"cli_step_viewed:review",
		"cli_collected",
		"cli_step_viewed:summary",
		"cli_step_viewed:uploading",
		"cli_uploaded",
		"cli_step_viewed:done",
		"cli_quit",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	byName := map[string]map[string]any{}
	for i, e := range events {
		byName[got[i]] = e["properties"].(map[string]any)
	}
	if p := byName["cli_check_finished:conversations:false"]; p["category"] != telemetry.AuthFailed || p["agent"] != "beta" {
		t.Errorf("failed check = %v", p)
	}
	if p := byName["cli_check_finished:dbt:true"]; p["warnings"] != float64(1) {
		t.Errorf("dbt check = %v", p)
	}
	if p := byName["cli_step_viewed:dbt"]; p["step_index"] != float64(2) || p["step_count"] != float64(6) {
		t.Errorf("dbt view = %v", p)
	}
	if p := byName["cli_collected"]; p["conversations"] != float64(12) || p["size_bucket"] != "1-5MB" || p["anonymize"] != true || p["files_dbt"] != float64(3) {
		t.Errorf("collected = %v", p)
	}
	if p := byName["cli_quit"]; p["last_step"] != "done" || p["agent"] != "beta" {
		t.Errorf("quit = %v", p)
	}

	raw, _ := json.Marshal(events)
	for _, bad := range []string{token, "ANALYTICS_SVC", "acme", "snowflakecomputing", "example.com", "me@", dbtDir, filepath.Dir(dbtDir), "up_1"} {
		if bytes.Contains(raw, []byte(bad)) {
			t.Errorf("%q reached the usage stats:\n%s", bad, raw)
		}
	}
}
