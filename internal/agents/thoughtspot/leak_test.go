package thoughtspot

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
)

// leakServer is the fake ThoughtSpot behind a proxy that accepts the
// canary credential as the bearer token. The analytics model's rows name
// the canary people in the User Name column, which is only read with
// --include-users (and pseudonymised then). ThoughtSpot's answers are text:
// no result rows reach the reader.
func leakServer(t *testing.T) string {
	t.Helper()
	rows := analyticsRows(2, 2)
	rows[0][5], rows[1][5] = leaktest.Name, leaktest.Email
	f := &fakeTS{t: t, rows: rows}
	inner := f.server()
	t.Cleanup(inner.Close)
	u, _ := url.Parse(inner.URL)
	proxy := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+leaktest.Secret {
			r.Header.Set("Authorization", "Bearer "+bearer)
		} else {
			r.Header.Set("Authorization", "Bearer wrong")
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestNoCanaryLeaks(t *testing.T) {
	leaktest.Run(t, leaktest.Case{
		Agent: "thoughtspot",
		Inputs: func(t *testing.T) agent.Inputs {
			return agent.Inputs{"host": leakServer(t), "auth": "token", "token": leaktest.Secret}
		},
		// The user name is read only with --include-users.
		Directory: true,
		Fail: func(t *testing.T) agent.Inputs {
			return agent.Inputs{"host": leaktest.EchoServer(t, http.StatusUnauthorized).URL, "auth": "token", "token": leaktest.Secret}
		},
	})
}
