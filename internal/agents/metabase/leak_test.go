package metabase

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
	"github.com/benchouse/benchouse-cli/internal/pgread"
)

// leakFixture is the default fixture with every canary Metabase can carry
// planted: the person in the user directory (names people only there;
// messages carry numeric user ids, so People stays false), the row value
// in run_query's output table, and the credential in a tool's input.
func leakFixture() map[string][]string {
	data := fixture()
	for i, r := range data[messagesStmt] {
		r = strings.ReplaceAll(r, `| count |\\n| 4211 |`, `| code |\\n| `+leaktest.Row+` |`)
		r = strings.ReplaceAll(r, `\"input\":{\"query\":\"orders\"}`, `\"input\":{\"query\":\"orders\",\"api_key\":\"`+leaktest.Secret+`\"}`)
		data[messagesStmt][i] = r
	}
	first, last, _ := strings.Cut(leaktest.Name, " ")
	data[usersStmt] = []string{fmt.Sprintf(`{"id":7,"first_name":%q,"last_name":%q,"email":%q}`, first, last, leaktest.Email)}
	return data
}

func leakInputs(t *testing.T, open func(agent.Inputs) (pgread.Querier, error)) agent.Inputs {
	openDB = open
	t.Cleanup(func() { openDB = func(in agent.Inputs) (pgread.Querier, error) { return pgread.Open(in) } })
	return agent.Inputs{"host": leaktest.Host, "database": "metabase", "user": "reader", "password": leaktest.Secret}
}

func TestNoCanaryLeaks(t *testing.T) {
	if all := strings.Join(leakFixture()[messagesStmt], ""); !strings.Contains(all, leaktest.Row) || !strings.Contains(all, leaktest.Secret) {
		t.Fatal("the row and secret canaries were not planted")
	}
	leaktest.Run(t, leaktest.Case{
		Agent: "metabase",
		Inputs: func(t *testing.T) agent.Inputs {
			f := &fakeDB{t: t, data: leakFixture(), missing: map[string]bool{}}
			return leakInputs(t, func(agent.Inputs) (pgread.Querier, error) { return f, nil })
		},
		Rows:      true,
		Directory: true, // the user directory names people
		Fail: func(t *testing.T) agent.Inputs {
			return leakInputs(t, func(agent.Inputs) (pgread.Querier, error) {
				return nil, errors.New("failed to connect to `host=" + leaktest.Host + " user=" + leaktest.Email +
					" database=metabase`: password " + leaktest.Secret + " rejected: FATAL: password authentication failed for user \"" +
					leaktest.Name + "\" (SQLSTATE 28P01)")
			})
		},
	})
}

// Regression: tool inputs (and kept outputs) were copied into the bundle
// unscanned, so a token the model passed to a tool survived.
func TestToolPayloadSecretsAreMasked(t *testing.T) {
	for _, raw := range []string{
		`{"query":"orders","api_key":"` + leaktest.Secret + `"}`,
		`{"note":"use ` + leaktest.Secret + ` to log in"}`,
		`{"password":"hunter2-Sup3r"}`,
	} {
		got := string(maskSecrets([]byte(raw)))
		if strings.Contains(got, leaktest.Secret) || strings.Contains(got, "hunter2") || !strings.Contains(got, "[redacted]") {
			t.Errorf("maskSecrets(%s) = %s", raw, got)
		}
	}
	if got := string(maskSecrets([]byte(`{"query":"orders"}`))); got != `{"query":"orders"}` {
		t.Errorf("a payload without secrets changed: %s", got)
	}
}
