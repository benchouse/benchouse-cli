package cortex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/snowflake"
)

// fakeCheck answers the check's statements. Views in denied answer 403;
// warehouse "" makes CURRENT_WAREHOUSE() null.
type fakeCheck struct {
	denied    map[string]bool
	warehouse string
	counts    map[string]string
	seen      []string
}

func (f *fakeCheck) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Statement string }
		json.NewDecoder(r.Body).Decode(&req)
		f.seen = append(f.seen, req.Statement)
		write := func(cols []string, row []*string) {
			rt := make([]map[string]string, len(cols))
			for i, c := range cols {
				rt[i] = map[string]string{"name": c}
			}
			data := [][]*string{}
			if row != nil {
				data = append(data, row)
			}
			json.NewEncoder(w).Encode(map[string]any{"resultSetMetaData": map[string]any{"rowType": rt}, "data": data})
		}
		s := func(v string) *string { return &v }
		if req.Statement == snowflake.SessionStmt {
			var wh *string
			if f.warehouse != "" {
				wh = s(f.warehouse)
			}
			write([]string{"ACCOUNT", "USR", "ROLE", "WAREHOUSE", "VERSION"}, []*string{s("ACME"), s("READER"), s("ANALYST"), wh, s("9.30.1")})
			return
		}
		for view := range f.denied {
			if strings.Contains(req.Statement, "FROM "+view) {
				http.Error(w, `{"message":"SQL compilation error: Object '`+view+`' does not exist or not authorized."}`, http.StatusForbidden)
				return
			}
		}
		if strings.HasSuffix(req.Statement, "LIMIT 0") {
			write([]string{"X"}, nil)
			return
		}
		for view, n := range f.counts {
			if strings.HasPrefix(req.Statement, "SELECT COUNT(") && strings.Contains(req.Statement, " AS N FROM "+view) {
				write([]string{"N"}, []*string{s(n)})
				return
			}
		}
		t.Errorf("unexpected statement %s", req.Statement)
		http.Error(w, "{}", http.StatusBadRequest)
	}))
}

func checkWith(t *testing.T, f *fakeCheck, id string, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	t.Helper()
	srv := f.server(t)
	t.Cleanup(srv.Close)
	newClient = func(cfg snowflake.Config, secret string) *snowflake.Client {
		c := snowflake.New(cfg, secret)
		c.BaseURL = srv.URL
		return c
	}
	t.Cleanup(func() { newClient = snowflake.New })
	base := agent.Inputs{"account": "acme-x", "user": "reader", "auth": "pat", "token": "tok-SECRET"}
	for k, v := range in {
		base[k] = v
	}
	a, _ := agent.Get(id)
	if err := a.Validate(base); err != nil {
		t.Fatal(err)
	}
	return a.Check(context.Background(), base, opts)
}

const (
	analystV = "SNOWFLAKE.LOCAL.CORTEX_ANALYST_REQUESTS_V"
	eventsV  = "SNOWFLAKE.LOCAL.AI_OBSERVABILITY_EVENTS"
)

func TestCheckCountsAndWarnsOnUsage(t *testing.T) {
	f := &fakeCheck{warehouse: "WH", counts: map[string]string{analystV: "312", eventsV: "1204"},
		denied: map[string]bool{"SNOWFLAKE.ACCOUNT_USAGE.CORTEX_ANALYST_USAGE_HISTORY": true}}
	res, err := checkWith(t, f, "snowflake-cortex", nil, agent.Options{Since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Join(res.Facts, "\n")
	for _, want := range []string{
		"Connected to Snowflake 9.30.1, account ACME, as READER with role ANALYST on warehouse WH",
		"312 Cortex Analyst requests since 2026-08-01",
		"1,204 Cortex Agents requests since 2026-08-01",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "GRANT IMPORTED PRIVILEGES ON DATABASE SNOWFLAKE TO ROLE ANALYST") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	for _, s := range f.seen {
		if strings.Contains(s, "QUERY_HISTORY") && !strings.HasSuffix(s, "LIMIT 0") {
			t.Errorf("query history was not probed with LIMIT 0: %s", s)
		}
	}
}

func TestCheckFailures(t *testing.T) {
	_, err := checkWith(t, &fakeCheck{}, "snowflake-cortex", nil, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "no warehouse") || !strings.Contains(err.Error(), "DEFAULT_WAREHOUSE") {
		t.Errorf("no warehouse: %v", err)
	}

	f := &fakeCheck{warehouse: "WH", denied: map[string]bool{analystV: true}}
	_, err = checkWith(t, f, "snowflake-cortex", agent.Inputs{"product": "analyst"}, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "GRANT APPLICATION ROLE SNOWFLAKE.CORTEX_ANALYST_REQUESTS_ADMIN TO ROLE ANALYST") {
		t.Errorf("analyst denied: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "tok-SECRET") {
		t.Error("the token is in the error")
	}

	// both products: one readable is enough, the other a warning.
	f = &fakeCheck{warehouse: "WH", counts: map[string]string{eventsV: "5"}, denied: map[string]bool{analystV: true}}
	res, err := checkWith(t, f, "snowflake-cortex", nil, agent.Options{})
	if err != nil || !strings.Contains(strings.Join(res.Warnings, "\n"), "only the readable product") {
		t.Errorf("half readable: %v %v", res.Warnings, err)
	}
}

func TestIntelligenceChecksAgentsOnly(t *testing.T) {
	f := &fakeCheck{warehouse: "WH", counts: map[string]string{eventsV: "0"}}
	res, err := checkWith(t, f, "snowflake-intelligence", nil, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range f.seen {
		if strings.Contains(s, "ANALYST") {
			t.Errorf("analyst view read: %s", s)
		}
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "no Cortex Agents requests in the window") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestContextCheck(t *testing.T) {
	root := t.TempDir()
	for rel, body := range map[string]string{
		"models/orders.yaml":  "name: orders\ntables:\n  - name: orders\n    base_table: {database: DB, schema: S, table: ORDERS}\n  - name: users\n    base_table: {database: DB, schema: S, table: USERS}\nverified_queries:\n  - name: q1\n",
		"views/sales.sql":     "CREATE OR REPLACE SEMANTIC VIEW sales TABLES (o AS orders);\ncreate semantic view x tables (a);",
		"other/config.yml":    "foo: bar\n",
		"README.md":           "notes",
		"models/ignored.json": "{}",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o600)
	}
	res, err := checkContext(root)
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Join(res.Facts, "\n")
	for _, want := range []string{"1 semantic model YAML files (2 tables, 1 verified queries): models/orders.yaml",
		"2 semantic view definitions", "4 files ("} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if len(res.Warnings) != 1 {
		t.Errorf("warnings = %v", res.Warnings)
	}

	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, "x.yaml"), []byte("a: 1\n"), 0o600)
	if _, err := checkContext(empty); err == nil || !strings.Contains(err.Error(), "no Cortex semantic model found") ||
		!strings.Contains(err.Error(), "1 YAML files there are not semantic models") {
		t.Errorf("err = %v", err)
	}
}
