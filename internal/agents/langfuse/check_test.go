package langfuse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

func checkServer(t *testing.T, observations string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "pk-lf-1" || p != "sk-lf-1" {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("not a GET: %s", r.Method)
		}
		switch r.URL.Path {
		case projectsPath:
			w.Write([]byte(`{"data":[{"id":"p1","name":"analytics-prod"}]}`))
		case legacyTraces:
			// A Langfuse old enough to lack v2 observations but newer than
			// the v1 traces API does not exist: 404 here too.
			http.Error(w, "{}", http.StatusNotFound)
		case endpoint:
			if r.URL.Query().Get("limit") != "1" {
				t.Errorf("limit = %q", r.URL.Query().Get("limit"))
			}
			if status != 0 {
				http.Error(w, "{}", status)
				return
			}
			w.Write([]byte(observations))
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}))
}

func runCheck(t *testing.T, srv *httptest.Server, secret string, opts agent.Options) (agent.CheckResult, error) {
	t.Helper()
	a, _ := agent.Get("langfuse")
	in := agent.Inputs{"host": srv.URL, "public-key": "pk-lf-1", "secret-key": secret, "trace-name": "analytics-agent"}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	return a.Check(context.Background(), in, opts)
}

func TestCheck(t *testing.T) {
	srv := checkServer(t, `{"data":[{"id":"o1","traceId":"t1","startTime":"2026-10-01T10:00:00Z","traceName":"analytics-agent"}],"meta":{"cursor":"c2"}}`, 0)
	defer srv.Close()
	res, err := runCheck(t, srv, "sk-lf-1", agent.Options{Since: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Join(res.Facts, "\n")
	for _, want := range []string{"Connected to Langfuse at " + srv.URL + ", project analytics-prod",
		"newest observation since 2026-09-01: 2026-10-01T10:00:00Z (trace analytics-agent)"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
}

func TestCheckEmptyAndRefused(t *testing.T) {
	srv := checkServer(t, `{"data":[],"meta":{}}`, 0)
	defer srv.Close()
	res, err := runCheck(t, srv, "sk-lf-1", agent.Options{})
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], `for trace name "analytics-agent"`) {
		t.Errorf("empty: %v %v", res.Warnings, err)
	}

	_, err = runCheck(t, srv, "sk-wrong", agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "rejected the API keys") || strings.Contains(err.Error(), "sk-wrong") {
		t.Errorf("bad key: %v", err)
	}

	srv = checkServer(t, "", http.StatusNotFound)
	defer srv.Close()
	_, err = runCheck(t, srv, "sk-lf-1", agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "serves neither") {
		t.Errorf("old langfuse: %v", err)
	}
}

func TestCheckRefusesPlainHTTPToAnotherHost(t *testing.T) {
	a, _ := agent.Get("langfuse")
	in := agent.Inputs{"host": "http://langfuse.example.com", "public-key": "pk", "secret-key": "sk"}
	// Refused by the field before anything connects, and by the check.
	if err := a.Validate(in); err == nil || !strings.Contains(err.Error(), "--host:") || !strings.Contains(err.Error(), "https") {
		t.Errorf("Validate: %v", err)
	}
	if _, err := a.Check(context.Background(), in, agent.Options{}); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("err = %v", err)
	}
}
