package cortex

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
	"github.com/benchouse/benchouse-cli/internal/snowflake"
)

// leakSnowflake serves a capped collection with every canary planted: the
// person as the Analyst requester, the billed user and the span's
// user attributes, the row value in a SQL tool result, and the person in
// the user directory.
func leakSnowflake(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+leaktest.Secret {
			http.Error(w, `{"message":"bad token"}`, http.StatusUnauthorized)
			return
		}
		var req struct {
			Statement string
			Bindings  map[string]map[string]string
		}
		json.NewDecoder(r.Body).Decode(&req)
		st := req.Statement
		rows := []string{}
		col := "ROW_JSON"
		result, _ := json.Marshal(fmt.Sprintf(`[{"CODE":"%s"}]`, leaktest.Row))
		switch {
		case strings.HasPrefix(st, "SELECT COUNT("):
			col, rows = "N", []string{"1"}
		case strings.Contains(st, "GROUP BY TRACE"):
			rows = []string{`{"TRACE_ID":"t1"}`}
		case strings.Contains(st, "FROM SNOWFLAKE.LOCAL.AI_OBSERVABILITY_EVENTS"):
			rows = []string{fmt.Sprintf(`{"TIMESTAMP":"2026-10-01","TRACE":{"trace_id":"t1","span_id":"s1"},"RECORD_ATTRIBUTES":{
"snow.ai.observability.agent.request_id":"r1","snow.ai.observability.user.name":"%s","snow.ai.observability.user.email":"%s",
"snow.ai.observability.agent.tool.sql_execution.query":"select code from orders",
"snow.ai.observability.agent.tool.sql_execution.result":%s,
"snow.ai.observability.agent.tool.sql_execution.result.num_rows":1,
"snow.ai.observability.agent.response":"One order."}}`, leaktest.Name, leaktest.Email, result)}
		case strings.Contains(st, "FROM SNOWFLAKE.ACCOUNT_USAGE.CORTEX_AGENT_USAGE_HISTORY"):
			rows = []string{fmt.Sprintf(`{"REQUEST_ID":"r1","START_TIME":"2026-10-01","USER_NAME":"%s"}`, leaktest.Email)}
		case strings.Contains(st, "FROM SNOWFLAKE.LOCAL.CORTEX_ANALYST_REQUESTS_V"):
			rows = []string{fmt.Sprintf(`{"REQUEST_ID":"a1","TIMESTAMP":"2026-10-01","USER_NAME":"%s","REQUEST_BODY":{"messages":[{"content":"orders?"}]}}`, leaktest.Email)}
		case strings.Contains(st, "FROM SNOWFLAKE.ACCOUNT_USAGE.CORTEX_ANALYST_USAGE_HISTORY"):
			rows = []string{fmt.Sprintf(`{"START_TIME":"2026-10-01","USERNAME":"%s","CREDITS":0.1}`, leaktest.Email)}
		case strings.Contains(st, "FROM SNOWFLAKE.ACCOUNT_USAGE.QUERY_HISTORY"):
			rows = []string{fmt.Sprintf(`{"QUERY_ID":"q1","START_TIME":"2026-10-01","USER_NAME":"%s","QUERY_TEXT":"select code from orders"}`, leaktest.Email)}
		case strings.Contains(st, "FROM SNOWFLAKE.ACCOUNT_USAGE.USERS"):
			rows = []string{fmt.Sprintf(`{"USER_ID":1,"NAME":"%s","LOGIN_NAME":"%s","DISPLAY_NAME":"%s","EMAIL":"%s"}`,
				leaktest.Email, leaktest.Email, leaktest.Name, leaktest.Email)}
		default:
			t.Errorf("unexpected statement %s", st)
		}
		data := make([][]*string, len(rows))
		for i := range rows {
			data[i] = []*string{&rows[i]}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"resultSetMetaData": map[string]any{"rowType": []map[string]string{{"name": col}}},
			"data":              data,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// pointAt sends the reader's SQL API calls to url for the rest of t.
func pointAt(t *testing.T, url string) {
	newClient = func(cfg snowflake.Config, secret string) *snowflake.Client {
		c := snowflake.New(cfg, secret)
		c.BaseURL = url
		return c
	}
	t.Cleanup(func() { newClient = snowflake.New })
}

func leakInputs(t *testing.T) agent.Inputs {
	pointAt(t, leakSnowflake(t).URL)
	return agent.Inputs{"account": "acme-x", "user": "reader", "auth": "pat", "token": leaktest.Secret}
}

// leakContext is a semantic model folder with the credential in a
// connection file the masks must catch.
func leakContext(t *testing.T) string {
	dir := t.TempDir()
	files := map[string]string{
		"orders.yaml":      "name: orders\ntables:\n  - name: orders\n    base_table: {database: SHOP, schema: PUBLIC, table: ORDERS}\n",
		"connection.yml":   "snowflake:\n  account: acme-x\n  password: " + leaktest.Secret + "\n",
		"README.md":        "Token for the service user: " + leaktest.Secret + "\n",
		"setup/grants.sql": "-- password: " + leaktest.Secret + "\nGRANT USAGE ON DATABASE SHOP TO ROLE ANALYST;\n",
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func leakFail(t *testing.T) agent.Inputs {
	pointAt(t, leaktest.EchoServer(t, http.StatusForbidden).URL)
	return agent.Inputs{"account": "acme-x", "user": "reader", "auth": "pat", "token": leaktest.Secret}
}

func TestNoCanaryLeaksCortex(t *testing.T) {
	leaktest.Run(t, leaktest.Case{
		Agent: "snowflake-cortex",
		Inputs: func(t *testing.T) agent.Inputs {
			in := leakInputs(t)
			in["product"] = "both"
			return in
		},
		Context: leakContext,
		Rows:    true,
		People:  true,
		Fail:    leakFail,
	})
}

func TestNoCanaryLeaksIntelligence(t *testing.T) {
	leaktest.Run(t, leaktest.Case{
		Agent:   "snowflake-intelligence",
		Inputs:  leakInputs,
		Context: leakContext,
		Rows:    true,
		People:  true,
		Fail:    leakFail,
	})
}
