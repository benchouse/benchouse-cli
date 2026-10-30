package hex

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
)

// leakHex serves one thread with every canary Hex can hold planted: the
// person as the thread's user and in its feedback, the row value in a SQL
// tool result, and the credential as the only token accepted.
func leakHex(t *testing.T) *httptest.Server {
	t.Helper()
	thread := threadJSON("t1", "2026-10-02T10:00:00Z", map[string]any{"userId": leaktest.Email,
		"feedback": []any{map[string]any{"id": "f1", "type": "THUMBS", "userLiked": false, "userFeedback": "wrong week",
			"user": map[string]any{"name": leaktest.Name, "email": leaktest.Email}}}})
	msgs := []map[string]any{
		rec(map[string]any{"role": "user", "createdAt": "2026-10-02T10:00:00Z", "content": []any{map[string]any{"type": "text", "text": "Order codes?"}}}),
		rec(map[string]any{"role": "assistant", "createdAt": "2026-10-02T10:00:02Z", "content": []any{
			map[string]any{"type": "tool_use", "id": "c1", "name": "run_sql", "input": map[string]any{"sql": "select code from orders"}},
		}}),
		rec(map[string]any{"role": "tool", "createdAt": "2026-10-02T10:00:03Z", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": `[{"code":"` + leaktest.Row + `"}]`},
		}}),
		rec(map[string]any{"role": "assistant", "createdAt": "2026-10-02T10:00:04Z", "content": []any{map[string]any{"type": "text", "text": "One order."}}}),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+leaktest.Secret {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		enc := json.NewEncoder(w)
		switch {
		case r.URL.Path == "/api/v1/threads":
			enc.Encode(map[string]any{"threads": []any{thread}, "pagination": map[string]any{"after": nil}})
		case strings.HasSuffix(r.URL.Path, "/messages"):
			enc.Encode(map[string]any{"content": msgs, "pagination": map[string]any{"before": nil, "after": nil}})
		case r.URL.Path == "/api/v1/users/me":
			enc.Encode(map[string]any{"id": "u1", "name": leaktest.Name, "email": leaktest.Email, "role": "ADMIN"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNoCanaryLeaks(t *testing.T) {
	leaktest.Run(t, leaktest.Case{
		Agent:  "hex",
		Inputs: func(t *testing.T) agent.Inputs { return agent.Inputs{"host": leakHex(t).URL, "token": leaktest.Secret} },
		Rows:   true,
		People: true,
		Fail: func(t *testing.T) agent.Inputs {
			return agent.Inputs{"host": leaktest.EchoServer(t, http.StatusUnauthorized).URL, "token": leaktest.Secret}
		},
	})
}
