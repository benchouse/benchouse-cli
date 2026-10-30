package genie

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// checkWorkspace answers the check's requests. Tables in denied answer 403;
// noSpace makes the space GET answer 403.
type checkWorkspace struct {
	t        *testing.T
	denied   map[string]bool
	noSpace  bool
	stopped  bool
	badToken bool
	stmts    []string
}

func (c *checkWorkspace) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.badToken {
			http.Error(w, `{"error_code":"401","message":"Credential was not sent or was of an unsupported type"}`, http.StatusUnauthorized)
			return
		}
		switch p := r.URL.Path; {
		case p == mePath:
			w.Write([]byte(`{"userName":"ann@shop.test"}`))
		case p == warehousePath+"wh1":
			state := "RUNNING"
			if c.stopped {
				state = "STOPPED"
			}
			w.Write([]byte(`{"id":"wh1","name":"Starter Warehouse","state":"` + state + `"}`))
		case p == spacePath+sp:
			if c.noSpace {
				http.Error(w, `{"error_code":"PERMISSION_DENIED"}`, http.StatusForbidden)
				return
			}
			w.Write([]byte(`{"space_id":"` + sp + `","title":"Orders"}`))
		case p == "/api/2.0/genie/spaces":
			w.Write([]byte(`{"spaces":[{"space_id":"a"},{"space_id":"b"}],"next_page_token":"x"}`))
		case p == "/api/2.0/sql/statements" && r.Method == http.MethodPost:
			var req struct{ Statement string }
			json.NewDecoder(r.Body).Decode(&req)
			c.stmts = append(c.stmts, req.Statement)
			for table := range c.denied {
				if strings.Contains(req.Statement, "FROM "+table+"\n") {
					http.Error(w, `{"error_code":"PERMISSION_DENIED","message":"no SELECT on `+table+`"}`, http.StatusForbidden)
					return
				}
			}
			var rows []string
			if strings.Contains(req.Statement, "count(*)") {
				rows = []string{`{"n":57}`}
			} else if !strings.HasSuffix(req.Statement, "LIMIT 0") {
				c.t.Errorf("a system-table read without LIMIT 0: %s", req.Statement)
			}
			writeRows(w, "st", "SUCCEEDED", rows, "")
		default:
			c.t.Errorf("unexpected %s %s", r.Method, r.URL)
			http.Error(w, "{}", http.StatusNotFound)
		}
	}))
}

func checkGenie(t *testing.T, c *checkWorkspace, space string, opts agent.Options) (agent.CheckResult, error) {
	t.Helper()
	srv := c.server()
	t.Cleanup(srv.Close)
	a, _ := agent.Get("databricks-genie")
	in := agent.Inputs{"host": srv.URL, "token": "dapi-SECRET", "warehouse-id": "wh1", "space-id": space}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	return a.Check(context.Background(), in, opts)
}

func TestCheckGenie(t *testing.T) {
	c := &checkWorkspace{t: t, stopped: true, denied: map[string]bool{"system.billing.usage": true}}
	res, err := checkGenie(t, c, sp, agent.Options{Since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	facts, warns := strings.Join(res.Facts, "\n"), strings.Join(res.Warnings, "\n")
	for _, want := range []string{"as ann@shop.test", "SQL warehouse Starter Warehouse (STOPPED)", `Genie space "Orders" (` + sp + ")",
		"57 Genie questions since 2026-08-01"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if len(res.Warnings) != 2 || !strings.Contains(warns, "warehouse is stopped") ||
		!strings.Contains(warns, "grant USE SCHEMA on system.billing and SELECT on system.billing.usage") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	if !strings.Contains(c.stmts[0], "event_date >= DATE '2026-08-01'") {
		t.Errorf("the count is not bounded by the window: %s", c.stmts[0])
	}
}

func TestCheckGenieAllSpacesUnbounded(t *testing.T) {
	c := &checkWorkspace{t: t}
	res, err := checkGenie(t, c, "", agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Facts, "\n"), "2+ Genie spaces visible") {
		t.Errorf("facts = %v", res.Facts)
	}
	for _, s := range c.stmts {
		if strings.Contains(s, "count(*)") {
			t.Errorf("counted the whole audit log: %s", s)
		}
	}
}

func TestCheckGenieFailures(t *testing.T) {
	_, err := checkGenie(t, &checkWorkspace{t: t, badToken: true}, sp, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "could not connect") || strings.Contains(err.Error(), "dapi-SECRET") {
		t.Errorf("bad token: %v", err)
	}
	_, err = checkGenie(t, &checkWorkspace{t: t, noSpace: true}, sp, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "CAN VIEW") {
		t.Errorf("no space: %v", err)
	}
	_, err = checkGenie(t, &checkWorkspace{t: t, denied: map[string]bool{"system.access.audit": true}}, sp, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "SELECT on system.access.audit") {
		t.Errorf("no audit: %v", err)
	}
}
