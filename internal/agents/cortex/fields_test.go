package cortex

import (
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// Both registrations check the secret and the warehouse live and list the
// roles and warehouses.
func TestFieldsCheckLive(t *testing.T) {
	for _, id := range []string{"snowflake-cortex", "snowflake-intelligence"} {
		a, _ := agent.Get(id)
		got := map[string]string{}
		for _, f := range a.Fields {
			if f.Verify != nil {
				got[f.Key] += "verify "
			}
			if f.Lookup != nil {
				got[f.Key] += "lookup "
			}
			if f.Validate != nil {
				got[f.Key] += "validate "
			}
		}
		want := map[string]string{"account": "validate ", "token": "verify validate ", "private-key": "verify validate ",
			"role": "lookup ", "warehouse": "verify lookup ", "token-file": "validate "}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s %s: %q, want %q", id, k, got[k], v)
			}
		}
	}
}
