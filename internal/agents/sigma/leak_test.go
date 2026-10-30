package sigma

import (
	"net/http"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
)

// leakWarehouse is the fake AI usage table with the canaries planted: the
// person as every row's user_name and in a display-name column, the row
// value in a run_sql tool call's output, and the credential and host in a
// span's tool output error. The credential is also the Snowflake token.
func leakWarehouse(t *testing.T) *warehouse {
	w := newWarehouse(t)
	for _, r := range w.rows {
		r["user_name"] = leaktest.Email
	}
	w.rows[1]["tool_calls"] = `[{"name":"run_sql","input":{"sql":"SELECT code FROM orders"},"output":[{"code":"` + leaktest.Row + `"}],"status":"success"},` +
		`{"name":"connect","input":{},"output":"failed: password=` + leaktest.Secret + ` rejected","status":"error"}]`
	return w
}

func TestNoCanaryLeaks(t *testing.T) {
	leaktest.Run(t, leaktest.Case{
		Agent: "sigma",
		Inputs: func(t *testing.T) agent.Inputs {
			srv := leakWarehouse(t).snowflakeServer()
			t.Cleanup(srv.Close)
			useSnowflake(t, srv)
			in := agent.Inputs(sfInputs())
			in["token"] = leaktest.Secret
			return in
		},
		Rows:   true,
		People: true,
		Fail: func(t *testing.T) agent.Inputs {
			return agent.Inputs{"platform": "databricks", "host": leaktest.EchoServer(t, http.StatusForbidden).URL, "token": leaktest.Secret,
				"warehouse-id": "wh1", "catalog": "main", "schema": "sigma", "table": "AI_USAGE_ACME"}
		},
	})
}
