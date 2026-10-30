package langfuse

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
)

// leakLangfuse serves one trace with every canary Langfuse can hold
// planted: the person as the trace's user, the row value in a query tool's
// output, and the credential as the only secret key accepted.
func leakLangfuse(t *testing.T) *httptest.Server {
	t.Helper()
	data := []map[string]any{
		obs("o1", "t1", "", "AGENT", "analytics-agent", "2026-10-01T10:00:00Z", "2026-10-01T10:00:09Z",
			`{"messages":[{"role":"user","content":"Order codes?"}]}`, `{"role":"assistant","content":"One order."}`,
			map[string]any{"sessionId": "s1", "userId": leaktest.Email, "traceName": "analytics-agent"}),
		obs("o3", "t1", "o1", "TOOL", "run_query", "2026-10-01T10:00:03Z", "2026-10-01T10:00:04Z",
			`{"query":"select code from orders"}`, `{"rows":[{"code":"`+leaktest.Row+`"}]}`, nil),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "pk-lf-1" || p != leaktest.Secret {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": map[string]any{}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNoCanaryLeaks(t *testing.T) {
	leaktest.Run(t, leaktest.Case{
		Agent: "langfuse",
		Inputs: func(t *testing.T) agent.Inputs {
			return agent.Inputs{"host": leakLangfuse(t).URL, "public-key": "pk-lf-1", "secret-key": leaktest.Secret}
		},
		Rows: true,
		// People stays false: an email user id is dropped unless pseudonymised
		// or --include-users is given, so --anonymize-users=false alone keeps
		// no person (see mapper.user); with --include-users it is kept.
		Directory: true,
		Fail: func(t *testing.T) agent.Inputs {
			return agent.Inputs{"host": leaktest.EchoServer(t, http.StatusUnauthorized).URL, "public-key": "pk-lf-1", "secret-key": leaktest.Secret}
		},
	})
}
