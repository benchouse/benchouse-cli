package cortex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/snowflake"
)

// fakeSnowflake answers SQL API statements by the view they read. A view
// mapped to nil answers 403, as Snowflake does without the grant.
func fakeSnowflake(t *testing.T, views map[string][]string, seen *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		var req struct{ Statement string }
		json.NewDecoder(r.Body).Decode(&req)
		*seen = append(*seen, req.Statement)
		for view, rows := range views {
			if !strings.Contains(req.Statement, "FROM "+view) {
				continue
			}
			if rows == nil {
				http.Error(w, `{"message":"Insufficient privileges"}`, http.StatusForbidden)
				return
			}
			data := make([][]*string, len(rows))
			for i := range rows {
				data[i] = []*string{&rows[i]}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"resultSetMetaData": map[string]any{"rowType": []map[string]string{{"name": "ROW_JSON"}}},
				"data":              data,
			})
			return
		}
		t.Errorf("unexpected statement %s", req.Statement)
		http.Error(w, "{}", http.StatusBadRequest)
	}))
}

func collectWith(t *testing.T, srv *httptest.Server, in agent.Inputs, opts agent.Options) (agent.Collected, error) {
	t.Helper()
	newClient = func(cfg snowflake.Config, secret string) *snowflake.Client {
		c := snowflake.New(cfg, secret)
		c.BaseURL = srv.URL
		return c
	}
	t.Cleanup(func() { newClient = snowflake.New })
	base := agent.Inputs{"account": "acme-x", "user": "reader", "auth": "pat", "token": "tok", "product": "both"}
	for k, v := range in {
		base[k] = v
	}
	return agent.CollectAll(context.Background(), collect, base, opts)
}

const span = `{"TIMESTAMP":"2026-10-01 10:00:00","TRACE":{"trace_id":"t1","span_id":"s1"},
"RECORD_ATTRIBUTES":{"snow.ai.observability.agent.request_id":"r1",
"snow.ai.observability.agent.tool.sql_execution.query":"select count(*) from orders",
"snow.ai.observability.agent.tool.sql_execution.result":"[{\"COUNT\":42}]",
"snow.ai.observability.agent.tool.sql_execution.result.num_rows":1,
"snow.ai.observability.agent.response":"There are 42 orders."}}`

func TestBothProductsAreReadAndOptionalViewsMayFail(t *testing.T) {
	var seen []string
	srv := fakeSnowflake(t, map[string][]string{
		"SNOWFLAKE.LOCAL.CORTEX_ANALYST_REQUESTS_V":            {`{"REQUEST_ID":"a1","TIMESTAMP":"2026-10-01 09:00:00"}`},
		"SNOWFLAKE.ACCOUNT_USAGE.CORTEX_ANALYST_USAGE_HISTORY": nil,
		"SNOWFLAKE.LOCAL.AI_OBSERVABILITY_EVENTS":              {span},
		"SNOWFLAKE.ACCOUNT_USAGE.CORTEX_AGENT_USAGE_HISTORY":   nil,
		"SNOWFLAKE.ACCOUNT_USAGE.QUERY_HISTORY":                {`{"QUERY_ID":"q1","START_TIME":"2026-10-01"}`},
	}, &seen)
	defer srv.Close()

	res, err := collectWith(t, srv, nil, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, r := range res.Records {
		kinds[r.RecordKind] = r.SourceKind
	}
	if kinds["analyst_request"] != analystKind || kinds["span"] != agentsKind || kinds["statement_history"] != agentsKind {
		t.Errorf("kinds = %v", kinds)
	}
	if len(res.Notes) != 2 {
		t.Errorf("notes = %v", res.Notes)
	}
	for _, s := range seen {
		if strings.Contains(s, "USERS") {
			t.Error("the user directory was read without --include-users")
		}
		if !strings.HasPrefix(s, "SELECT ") {
			t.Errorf("not a read: %s", s)
		}
	}
	for _, r := range res.Records {
		if r.RecordKind != "span" {
			continue
		}
		p := string(r.Payload)
		if strings.Contains(p, `COUNT\":42`) {
			t.Errorf("tool result survived: %s", p)
		}
		for _, keep := range []string{"select count(*) from orders", "There are 42 orders.", "num_rows"} {
			if !strings.Contains(p, keep) {
				t.Errorf("lost %q: %s", keep, p)
			}
		}
		if r.NativeIDs["request_id"] != "r1" || r.NativeIDs["trace_id"] != "t1" {
			t.Errorf("native ids = %v", r.NativeIDs)
		}
	}
}

func TestARequiredViewFailingForEveryProductIsAnError(t *testing.T) {
	var seen []string
	srv := fakeSnowflake(t, map[string][]string{
		"SNOWFLAKE.LOCAL.CORTEX_ANALYST_REQUESTS_V":            nil,
		"SNOWFLAKE.ACCOUNT_USAGE.CORTEX_ANALYST_USAGE_HISTORY": nil,
	}, &seen)
	defer srv.Close()
	if _, err := collectWith(t, srv, agent.Inputs{"product": "analyst"}, agent.Options{}); err == nil {
		t.Fatal("no error when nothing could be read")
	}
}

func TestPlanHonoursProductAndSince(t *testing.T) {
	a, _ := agent.Get("snowflake-cortex")
	qs := a.Plan(agent.Inputs{"product": "analyst"}, agent.Options{})
	if len(qs) != 2 {
		t.Fatalf("analyst plan = %d statements", len(qs))
	}
	for _, q := range qs {
		if strings.Contains(q.Statement, "AI_OBSERVABILITY") {
			t.Error("agents view planned for analyst only")
		}
	}
}

func TestIntelligenceReadsOnlyAgentsTraces(t *testing.T) {
	a, err := agent.Get("snowflake-intelligence")
	if err != nil {
		t.Fatal(err)
	}
	qs := a.Plan(agent.Inputs{"product": "analyst"}, agent.Options{})
	if len(qs) != 3 {
		t.Fatalf("plan = %d statements, want the 3 agents views", len(qs))
	}
	for _, q := range qs {
		if strings.Contains(q.Statement, "ANALYST") {
			t.Errorf("analyst view planned: %s", q.Statement)
		}
	}
}
