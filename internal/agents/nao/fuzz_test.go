package nao

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// A message_part row is whatever nao's database holds: redactPart never
// panics on it, and without --include-results the output of a tool that
// is not a context or conversational tool keeps only its shape (row count,
// columns, status, error) — never its data.
func FuzzRedactPart(f *testing.F) {
	for _, s := range []string{
		`{"id":"s1","tool_name":"execute_sql","tool_output":"{\"id\":\"q1\",\"columns\":[\"n\"],\"row_count\":1,\"data\":[{\"n\":\"CANARY-ROW-4242\"}]}"}`,
		`{"id":"s2","tool_name":"execute_sql","tool_output":{"columns":["n"],"rows":[["CANARY-ROW-4242"]],"error":null}}`,
		`{"id":"s3","tool_name":"EXECUTE_SQL","tool_output":"plain text CANARY-ROW-4242"}`,
		`{"id":"s4","tool_name":"read","tool_input":"{\"file_path\":\"databases/x/preview.md\"}","tool_output":"| CANARY-ROW-4242 |"}`,
		`{"id":"s5","tool_name":"grep","tool_output":{"matches":[{"path":"databases/x/profiling.md","line_content":"CANARY-ROW-4242"}]}}`,
		`{"id":"s6","tool_name":"chart","tool_output":[1,2,3]}`,
		`{"id":"s7","tool_name":7,"tool_output":"{\"message\":{\"nested\":\"CANARY-ROW-4242\"},\"status\":\"ok\"}"}`,
		`{"tool_output":null}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := decode(data)
		if err != nil || m == nil {
			return
		}
		tool := strings.ToLower(str(m["tool_name"]))
		out := json.RawMessage(data)
		if r, ok := redactPart(out, m, agent.Options{}); ok {
			out = r
		}
		if keptTools[tool] {
			return // context and conversational tools: kept, secret-scanned
		}
		got, err := decode(out)
		if err != nil {
			t.Fatalf("redacted row is not JSON: %v", err)
		}
		var o any = got["tool_output"]
		if s, ok := o.(string); ok {
			var v any
			if decodeInto(s, &v) != nil {
				if strings.TrimSpace(s) != "" {
					t.Fatalf("plain-text output kept: %q", s)
				}
				return
			}
			o = v
		}
		switch v := o.(type) {
		case nil:
		case map[string]any:
			for k, x := range v {
				if k == "redacted" {
					continue
				}
				if !keptOutputKeys[k] {
					t.Fatalf("output key %q kept without --include-results: %s", k, out)
				}
				switch x := x.(type) {
				case []any:
					for _, c := range x {
						if _, ok := c.(string); !ok {
							t.Fatalf("output %q holds a non-name %v: %s", k, c, out)
						}
					}
				case map[string]any:
					t.Fatalf("output %q holds an object: %s", k, out)
				}
			}
		default:
			t.Fatalf("output of %T kept without --include-results: %s", o, out)
		}
	})
}
