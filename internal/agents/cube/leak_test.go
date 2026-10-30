package cube

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
)

// leakThreads are the fixtures with every canary planted: the person as
// the thread's user (email, name and external id) and the row value in a
// query's rows. The host is the deployment's URL (see withSecret).
func leakThreads() []map[string]any {
	ths := fixtures()
	for _, th := range ths {
		th["user"] = map[string]any{"id": 11, "type": "internal", "email": leaktest.Email, "name": leaktest.Name, "externalId": leaktest.Email}
	}
	b, _ := json.Marshal(ths[0]["turns"])
	s := strings.Replace(string(b), `[[1204]]`, `[["`+leaktest.Row+`"]]`, 1)
	var ts []any
	if err := json.Unmarshal([]byte(s), &ts); err != nil {
		panic(err)
	}
	ths[0]["turns"] = ts
	return ths
}

// withSecret serves h behind the canary credential: requests bearing it
// reach the fake as if they carried its own token.
func withSecret(t *testing.T, h http.Handler) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+leaktest.Secret {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		r.Header.Set("Authorization", "Bearer "+token)
		if r.URL.Path == "/api/v1/deployments/42" {
			w.Write([]byte(`{"id":42,"name":"prod","deploymentUrl":"https://` + leaktest.Host + `","creationStep":"ready"}`))
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func leakContext(t *testing.T) string {
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"model/cubes/orders.yml": "cubes:\n  - name: orders\n    sql_table: public.orders\n",
		"cube.js":                "module.exports = { apiSecret: '" + leaktest.Secret + "' };\n",
		".env":                   "CUBEJS_DB_PASS=" + leaktest.Secret + "\nCUBEJS_DB_HOST=" + leaktest.Host + "\n",
		"model/README.md":        "Service token: " + leaktest.Secret + "\n",
	} {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestNoCanaryLeaks(t *testing.T) {
	leaktest.Run(t, leaktest.Case{
		Agent: "cube",
		Inputs: func(t *testing.T) agent.Inputs {
			f := &fakeCube{t: t, threads: leakThreads()}
			fake := f.server()
			t.Cleanup(fake.Close)
			return agent.Inputs{"host": withSecret(t, fake.Config.Handler).URL, "api-token": leaktest.Secret, "deployment-id": "42"}
		},
		Context: leakContext,
		Rows:    true,
		// People stay false: Cube's names and emails come only with
		// --include-users (Directory).
		Directory: true,
		Fail: func(t *testing.T) agent.Inputs {
			return agent.Inputs{"host": leaktest.EchoServer(t, http.StatusForbidden).URL, "api-token": leaktest.Secret, "deployment-id": "42"}
		},
	})
}
