package genie

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
)

// leakWorkspace is the workspace fixture with every canary planted: the
// person as every audit event's user and in the SCIM directory, and the
// row value in a message's embedded result rows.
func leakWorkspace(t *testing.T) *fakeWorkspace {
	f := workspace(t)
	f.limited = nil
	for i, r := range f.tables["system.access.audit"] {
		r = strings.Replace(r, `"user_identity":{"email":"ann@shop.test"}`,
			fmt.Sprintf(`"user_identity":{"email":%q,"subject_name":%q}`, leaktest.Email, leaktest.Name), 1)
		f.tables["system.access.audit"][i] = r
	}
	f.tables["system.query.history"] = append(f.tables["system.query.history"],
		fmt.Sprintf(`{"statement_id":"st-2","start_time":"2026-10-01T10:00:06.000Z","query_source":{"genie_space_id":%q},"statement_text":"SELECT month, count(*) FROM orders GROUP BY 1","executed_by":%q}`, sp, leaktest.Email))
	f.messages["m2"] = strings.Replace(f.messages["m2"], "SECRET_ROW_VALUE", leaktest.Row, 1)
	return f
}

// behindSecret serves f behind the canary credential: requests bearing it
// reach the fake as if they carried its own token, and the directory
// names the canary person.
func behindSecret(t *testing.T, f *fakeWorkspace) *httptest.Server {
	fake := f.server()
	t.Cleanup(fake.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+leaktest.Secret {
			http.Error(w, `{"error_code":"UNAUTHENTICATED"}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/api/2.0/preview/scim/v2/Users" {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("startIndex") > "1" {
				w.Write([]byte(`{"totalResults":1,"Resources":[]}`))
				return
			}
			fmt.Fprintf(w, `{"totalResults":1,"Resources":[{"id":"u1","userName":%q,"displayName":%q,"emails":[{"value":%q}]}]}`,
				leaktest.Email, leaktest.Name, leaktest.Email)
			return
		}
		r.Header.Set("Authorization", "Bearer tok")
		fake.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fastPolls(t *testing.T) {
	p, r, n := pollEvery, retryAfter, scimPageSize
	pollEvery, retryAfter = time.Millisecond, time.Millisecond
	t.Cleanup(func() { pollEvery, retryAfter, scimPageSize = p, r, n })
}

func TestNoCanaryLeaks(t *testing.T) {
	leaktest.Run(t, leaktest.Case{
		Agent: "databricks-genie",
		Inputs: func(t *testing.T) agent.Inputs {
			fastPolls(t)
			srv := behindSecret(t, leakWorkspace(t))
			return agent.Inputs{"host": srv.URL, "token": leaktest.Secret, "warehouse-id": "wh1", "space-id": sp}
		},
		Rows:   true,
		People: true,
		Fail: func(t *testing.T) agent.Inputs {
			fastPolls(t)
			return agent.Inputs{"host": leaktest.EchoServer(t, http.StatusForbidden).URL, "token": leaktest.Secret,
				"warehouse-id": "wh1", "space-id": sp}
		},
	})
}
