package telemetry

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
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/upload"
	"github.com/jackc/pgx/v5/pgconn"
)

// Sink is a fake PostHog batch endpoint that records every event.
type Sink struct {
	*httptest.Server
	mu       sync.Mutex
	Events   []map[string]any
	Requests int
}

// NewSink starts a Sink; delay holds every answer back.
func NewSink(t *testing.T, delay time.Duration) *Sink {
	s := &Sink{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			APIKey string           `json:"api_key"`
			Batch  []map[string]any `json:"batch"`
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.Requests++
		if body.APIKey == APIKey {
			s.Events = append(s.Events, body.Batch...)
		}
		s.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
		}
		w.Write([]byte(`{"status":1}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *Sink) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.Events {
		out = append(out, e["event"].(string))
	}
	return out
}

func TestEventsAreBatchedWithBaseProperties(t *testing.T) {
	sink := NewSink(t, 0)
	dir := t.TempDir()
	StartWith(Options{Endpoint: sink.URL, Dir: dir, Version: "1.2.3"})
	SetMode("plain")
	Started("upload")
	AgentSelected("nao")
	FieldFailed("nao", "host", "verify", errors.New("dial tcp 10.0.0.1:5432: connect: connection refused"))
	Quit("done")
	Quit("error") // only the first counts
	Close(time.Second)

	if got := fmt.Sprint(sink.names()); got != "[cli_started cli_agent_selected cli_field_failed cli_quit]" {
		t.Fatalf("events = %s", got)
	}
	id, _ := os.ReadFile(filepath.Join(dir, "install-id"))
	st, _ := os.Stat(filepath.Join(dir, "install-id"))
	if st == nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Errorf("install-id mode = %v", st)
	}
	for _, e := range sink.Events {
		p := e["properties"].(map[string]any)
		if p["distinct_id"] != string(id[:36]) || p["cli_version"] != "1.2.3" || p["mode"] != "plain" ||
			p["$process_person_profile"] != false || p["os"] == "" || p["arch"] == "" {
			t.Errorf("properties = %v", p)
		}
		if v, ok := p["$ip"]; !ok || v != nil {
			t.Errorf("$ip = %v, %v", v, ok)
		}
	}
	ff := sink.Events[2]["properties"].(map[string]any)
	if ff["category"] != Network || ff["message"] != "dial tcp <host>: connect: connection refused" || ff["field"] != "host" {
		t.Errorf("field failed = %v", ff)
	}
	q := sink.Events[3]["properties"].(map[string]any)
	if q["how"] != "done" || q["agent"] != "nao" || q["last_step"] != "start" {
		t.Errorf("quit = %v", q)
	}

	// The install id is kept across runs.
	StartWith(Options{Endpoint: sink.URL, Dir: dir, Version: "1.2.3"})
	Started("agents")
	Close(time.Second)
	if p := sink.Events[4]["properties"].(map[string]any); p["distinct_id"] != string(id[:36]) {
		t.Errorf("a new install id: %v", p["distinct_id"])
	}
}

func TestDisabledInTestsSendsNothingAndWritesNoID(t *testing.T) {
	sink := NewSink(t, 0)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("APPDATA", home) // os.UserConfigDir on Windows
	t.Setenv("AppData", home)
	os.Unsetenv(EnvEndpoint)
	Start("dev")
	if Enabled() {
		t.Fatal("telemetry is on inside a test binary")
	}
	Started("guided")
	Quit("done")
	Close(time.Second)
	t.Setenv(EnvEndpoint, "")
	Start("dev")
	if Enabled() {
		t.Fatal("telemetry is on with an empty endpoint")
	}
	Started("guided")
	Close(time.Second)
	if sink.Requests != 0 {
		t.Errorf("%d requests", sink.Requests)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Errorf("files written: %v", entries)
	}
}

func TestEndpointOverrideTurnsItOnInTests(t *testing.T) {
	sink := NewSink(t, 0)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", os.Getenv("HOME"))
	t.Setenv("APPDATA", os.Getenv("HOME")) // os.UserConfigDir on Windows
	t.Setenv(EnvEndpoint, sink.URL)
	Start("dev")
	Started("plan")
	Close(time.Second)
	if fmt.Sprint(sink.names()) != "[cli_started]" {
		t.Errorf("events = %v", sink.names())
	}
}

func TestSlowEndpointDoesNotDelayExit(t *testing.T) {
	sink := NewSink(t, 10*time.Second)
	StartWith(Options{Endpoint: sink.URL, Dir: t.TempDir(), Version: "dev"})
	for range 120 { // more than one batch
		Started("guided")
	}
	start := time.Now()
	Close(time.Second)
	if d := time.Since(start); d > 1200*time.Millisecond {
		t.Errorf("Close took %v", d)
	}
}

func TestUnreachableEndpointIsSilent(t *testing.T) {
	StartWith(Options{Endpoint: "http://127.0.0.1:1/batch/", Dir: t.TempDir(), Version: "dev"})
	Started("guided")
	start := time.Now()
	Close(time.Second)
	if d := time.Since(start); d > 1200*time.Millisecond {
		t.Errorf("Close took %v", d)
	}
}

func TestCategoryTypedErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{context.Canceled, Canceled},
		{fmt.Errorf("check: %w", context.DeadlineExceeded), Timeout},
		{&pgconn.PgError{Code: "28P01", Message: "password authentication failed"}, AuthFailed},
		{fmt.Errorf("x: %w", &pgconn.PgError{Code: "42501"}), PermissionDenied},
		{&pgconn.PgError{Code: "42P01"}, NotFound},
		{&upload.StatusError{Code: 429, Err: errors.New("Benchouse is not accepting uploads right now")}, RateLimited},
		{&upload.StatusError{Code: 502, Err: errors.New("upload refused")}, ServerError},
		{errors.New("snowflake http 403: forbidden"), PermissionDenied},
		{errors.New("Nao needs --host, --password"), NotConfigured},
		{errors.New("--email \"x\" is not an email address"), InvalidInput},
		{errors.New("something odd"), Unknown},
	} {
		if got := Category(c.err); got != c.want {
			t.Errorf("Category(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestHTTPStatus(t *testing.T) {
	for in, want := range map[string]int{
		"databricks http 403: no":                   403,
		"upload refused (HTTP 500): x":              500,
		"langfuse rejected the API keys (401): fix": 401,
		"no status here (2024)":                     0,
		"connection refused":                        0,
	} {
		if got := HTTPStatus(errors.New(in)); got != want {
			t.Errorf("HTTPStatus(%q) = %d, want %d", in, got, want)
		}
	}
	if got := HTTPStatus(&upload.StatusError{Code: 503, Err: errors.New("busy")}); got != 503 {
		t.Errorf("StatusError: %d", got)
	}
}

func TestSizeBucket(t *testing.T) {
	for n, want := range map[int64]string{0: "<1MB", 1 << 20: "1-5MB", 6 << 20: "5-32MB", 32 << 20: "5-32MB", 33 << 20: ">32MB"} {
		if got := SizeBucket(n); got != want {
			t.Errorf("SizeBucket(%d) = %s, want %s", n, got, want)
		}
	}
}
