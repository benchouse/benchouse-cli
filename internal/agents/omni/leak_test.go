package omni

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
)

// leakServer is the fake Omni with every canary it can carry planted, by
// rewriting its answers: the API key is the credential, user u1 is the
// person (by email) and the result CSV holds the row value. Host is not
// planted in the data: the only host Omni names is its own (the chat links),
// which the bundle carries by design, as the manifest's source account
// does. The Fail run covers Host in the usage stats.
func leakServer(t *testing.T) agent.Inputs {
	f := &fakeOmni{t: t, key: leaktest.Secret}
	repl := strings.NewReplacer(
		`"userId":"u1"`, `"userId":"`+leaktest.Email+`"`,
		`orders.count\n1204`, `orders.code\n`+leaktest.Row,
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		f.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		w.Write([]byte(repl.Replace(rec.Body.String())))
	}))
	t.Cleanup(srv.Close)
	return agent.Inputs{"host": srv.URL, "api-key": leaktest.Secret}
}

func TestNoCanaryLeaks(t *testing.T) {
	leaktest.Run(t, leaktest.Case{
		Agent:  "omni",
		Inputs: leakServer,
		Rows:   true,
		People: true,
		Fail: func(t *testing.T) agent.Inputs {
			return agent.Inputs{"host": leaktest.EchoServer(t, http.StatusUnauthorized).URL, "api-key": leaktest.Secret}
		},
	})
}
