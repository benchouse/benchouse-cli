package cortex

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// isResultKey is the rule redactSpan must enforce, written independently:
// a tool attribute naming a result, output, data or content segment, in
// any case, except its row counts.
func isResultKey(k string) bool {
	l := strings.ToLower(k)
	if !strings.Contains(l, ".tool.") || strings.HasSuffix(l, ".num_rows") || strings.HasSuffix(l, ".row_count") {
		return false
	}
	for _, seg := range strings.Split(l, ".") {
		switch seg {
		case "result", "results", "output", "outputs", "data", "content":
			return true
		}
	}
	return false
}

// A span row is whatever the event table holds: redactSpan never panics on
// it, and without --include-results no tool result attribute keeps its
// value.
func FuzzRedactSpan(f *testing.F) {
	for _, s := range []string{
		`{"RECORD_ATTRIBUTES":{"snow.ai.observability.agent.tool.sql_execution.result":"[[\"CANARY-ROW-4242\"]]","snow.ai.observability.agent.tool.sql_execution.num_rows":1}}`,
		`{"RECORD_ATTRIBUTES":{"ai.observability.tool.CORTEX_SEARCH.Results":["CANARY-ROW-4242"],"agent.response":"ok"}}`,
		`{"RECORD_ATTRIBUTES":{"x.tool.y.data.z":{"rows":[1]}},"RECORD":{"name":"span"}}`,
		`{"RECORD_ATTRIBUTES":"not an object"}`,
		`{"RECORD_ATTRIBUTES":null}`,
		`{}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var m map[string]any
		if dec.Decode(&m) != nil || m == nil {
			return
		}
		out := json.RawMessage(data)
		if r, ok := redactSpan(m); ok {
			out = r
		}
		dec = json.NewDecoder(bytes.NewReader(out))
		dec.UseNumber()
		var got map[string]any
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("redacted span is not JSON: %v", err)
		}
		ra, _ := got["RECORD_ATTRIBUTES"].(map[string]any)
		for k, v := range ra {
			if isResultKey(k) && v != "[removed by benchouse-cli]" {
				t.Fatalf("result attribute %q kept: %v", k, v)
			}
		}
	})
}
