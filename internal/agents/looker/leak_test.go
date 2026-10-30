package looker

import (
	"net/http"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
)

// leakFake is the per-user fixture with every canary Looker can carry
// planted: the API client secret, the person as System Activity's
// user.name and user.email (never collected without --include-users), and the row value in a conversation's result.
func leakFake(t *testing.T) agent.Inputs {
	f := perUserFixture(t)
	f.clientSecret = leaktest.Secret
	for _, r := range f.caRows {
		r["user.name"], r["user.email"] = leaktest.Name, leaktest.Email
	}
	data := f.msgs["c1"][2]["message"].(map[string]any)["systemMessage"].(map[string]any)["data"].(map[string]any)
	data["result"] = map[string]any{"data": []any{map[string]any{"orders.code": leaktest.Row}}}
	in := f.inputs(f.server(), "yes")
	in["client-secret"] = leaktest.Secret
	return in
}

func TestNoCanaryLeaks(t *testing.T) {
	leaktest.Run(t, leaktest.Case{
		Agent:     "looker",
		Inputs:    leakFake,
		Rows:      true,
		Directory: true, // names and emails come only with --include-users
		// People stays false: Looker records name people by numeric user id
		// only; user.name and user.email come only with --include-users.
		Fail: func(t *testing.T) agent.Inputs {
			srv := leaktest.EchoServer(t, http.StatusUnauthorized)
			return agent.Inputs{"host": srv.URL, "client-id": "cid", "client-secret": leaktest.Secret, "per-user": "yes"}
		},
	})
}
