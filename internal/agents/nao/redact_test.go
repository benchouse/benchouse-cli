package nao

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// fakeToken is assembled so no scanner flags this source file.
var fakeToken = "ghp_" + strings.Repeat("a1B2", 9)

func part(t *testing.T, tool string, input, output any) (json.RawMessage, map[string]any) {
	t.Helper()
	enc := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	row, _ := json.Marshal(map[string]any{"id": "p", "tool_name": tool, "tool_input": enc(input), "tool_output": enc(output)})
	m, err := decode(row)
	if err != nil {
		t.Fatal(err)
	}
	return row, m
}

func outputOf(t *testing.T, row json.RawMessage) string {
	t.Helper()
	m, _ := decode(row)
	s, _ := m["tool_output"].(string)
	return s
}

func TestKeepContextLeavesOutSampleData(t *testing.T) {
	table := "databases/type=clickhouse/database=world/schema=s/table=orders/"
	for rel, want := range map[string]bool{
		table + "columns.md": true, table + "annotations.md": true, table + "ai_summary.md": true,
		table + "preview.md": false, table + "profiling.md": false,
		"RULES.md": true, "nao_config.yaml": true, "semantics/preview.md": true,
	} {
		if got := keepContext(rel); got != want {
			t.Errorf("keepContext(%s) = %v, want %v", rel, got, want)
		}
	}
	if !keepContextWith(table+"preview.md", agent.Options{IncludeResults: true}) {
		t.Error("--include-results did not keep preview.md")
	}
	a, _ := agent.Get("nao")
	if a.Context.Keeps(agent.Options{})(table+"profiling.md") || !a.Context.Keeps(agent.Options{IncludeResults: true})(table+"profiling.md") {
		t.Error("the registered context does not apply the options")
	}
}

func TestKeptToolOutputsAreSecretScanned(t *testing.T) {
	for _, opts := range []agent.Options{{}, {IncludeResults: true}} {
		row, m := part(t, "read", map[string]any{"file_path": "RULES.md"},
			map[string]any{"content": "# rules\nuse token " + fakeToken + " for the API", "numberOfTotalLines": 2})
		got, ok := redactPart(row, m, opts)
		out := outputOf(t, got)
		if !ok || strings.Contains(out, fakeToken) || !strings.Contains(out, "# rules") || !strings.Contains(out, "[redacted]") {
			t.Errorf("results=%v: %s", opts.IncludeResults, out)
		}
	}
	// A clean read is left byte-for-byte.
	row, m := part(t, "read", map[string]any{"file_path": "RULES.md"}, map[string]any{"content": "One row per order."})
	if got, ok := redactPart(row, m, agent.Options{}); ok || string(got) != string(row) {
		t.Errorf("clean read changed: %s", got)
	}
}

func TestReadsAndGrepsOfSampleDataAreWithheld(t *testing.T) {
	preview := "/work/databases/type=clickhouse/database=w/schema=s/table=customers/preview.md"
	row, m := part(t, "read", map[string]any{"file_path": preview},
		map[string]any{"content": `- {"email": "ann@shop.test"}`})
	got, _ := redactPart(row, m, agent.Options{})
	if out := outputOf(t, got); strings.Contains(out, "ann@shop.test") || !strings.Contains(out, "sample rows") {
		t.Errorf("preview read kept: %s", out)
	}
	row, m = part(t, "read", map[string]any{"file_path": preview}, map[string]any{"content": `- {"email": "ann@shop.test"}`})
	if got, _ := redactPart(row, m, agent.Options{IncludeResults: true}); !strings.Contains(outputOf(t, got), "ann@shop.test") {
		t.Error("--include-results withheld the preview read")
	}

	row, m = part(t, "grep", map[string]any{"pattern": "ann"}, map[string]any{"matches": []any{
		map[string]any{"path": preview, "line_number": 3, "line_content": `{"email": "ann@shop.test"}`, "context_after": []any{"bob@shop.test"}},
		map[string]any{"path": "databases/x/table=customers/columns.md", "line_number": 9, "line_content": "- email (String)"},
	}, "total_matches": 2, "truncated": false})
	got, _ = redactPart(row, m, agent.Options{})
	out := outputOf(t, got)
	if strings.Contains(out, "shop.test") || !strings.Contains(out, "- email (String)") || !strings.Contains(out, `"total_matches":2`) {
		t.Errorf("grep: %s", out)
	}
}

func TestRedactedOutputKeepsOnlyScalars(t *testing.T) {
	long := strings.Repeat("x", 501)
	row, m := part(t, "execute_sql", map[string]any{"sql_query": "select 1"}, map[string]any{
		"data":      []any{map[string]any{"email": "ann@shop.test"}},
		"row_count": 1,
		"columns":   []any{"email", map[string]any{"name": "nested"}, strings.Repeat("c", 201)},
		"message":   map[string]any{"rows": []any{"ann@shop.test"}},
		"error":     long,
		"status":    "ok",
		"truncated": false,
		"id":        "query_ab12",
	})
	got, _ := redactPart(row, m, agent.Options{})
	var out map[string]any
	if err := json.Unmarshal([]byte(outputOf(t, got)), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["message"]; ok {
		t.Error("an object under message survived")
	}
	if _, ok := out["error"]; ok {
		t.Error("an over-long error survived")
	}
	if out["status"] != "ok" || out["truncated"] != false || out["row_count"] != float64(1) || out["id"] != "query_ab12" {
		t.Errorf("scalars lost: %v", out)
	}
	if cols, _ := out["columns"].([]any); len(cols) != 1 || cols[0] != "email" {
		t.Errorf("columns = %v", out["columns"])
	}
	if strings.Contains(outputOf(t, got), "ann@shop.test") {
		t.Error("a value survived")
	}
	// A secret quoted in an error message is masked.
	row, m = part(t, "execute_sql", nil, map[string]any{"error": "auth failed for " + fakeToken})
	got, _ = redactPart(row, m, agent.Options{})
	if o := outputOf(t, got); strings.Contains(o, fakeToken) || !strings.Contains(o, "auth failed") {
		t.Errorf("error: %s", o)
	}
}

// TestMCPExecuteSQLOutputIsRedacted: nao's mcp/logging.ts stores an MCP
// tool's output in mcp_call_log.tool_output, the parsed JSON when it fits.
// For execute_sql that is executeSql.Output plus query_id, rows included:
// jsonb on Postgres (an object in the row), text on sqlite.
func TestMCPExecuteSQLOutputIsRedacted(t *testing.T) {
	output := map[string]any{"_version": "1", "id": "query_8f2c", "query_id": "query_8f2c", "dialect": "postgres",
		"columns": []any{"email", "revenue"}, "row_count": 1,
		"data": []any{map[string]any{"email": "canary-row@shop.test", "revenue": 4242.42}}}
	input := map[string]any{"sql_query": "SELECT email, revenue FROM customers LIMIT 1", "chat_id": "c1"}
	pg, _ := json.Marshal(map[string]any{"id": "m1", "tool_name": "execute_sql", "tool_input": input, "tool_output": output})
	sq, _ := json.Marshal(map[string]any{"id": "m1", "tool_name": "execute_sql", "tool_input": mustJSON(input), "tool_output": mustJSON(output)})
	for name, row := range map[string]json.RawMessage{"postgres": pg, "sqlite": sq} {
		m, _ := decode(row)
		got, ok := redactMCPCall(row, m, agent.Options{})
		s := string(got)
		if !ok || strings.Contains(s, "canary-row") || strings.Contains(s, "4242") ||
			!strings.Contains(s, "SELECT email, revenue") || !strings.Contains(s, "row_count") || !strings.Contains(s, "revenue") {
			t.Errorf("%s: %s", name, s)
		}
		m, _ = decode(row)
		if got, _ := redactMCPCall(row, m, agent.Options{IncludeResults: true}); !strings.Contains(string(got), "canary-row") {
			t.Errorf("%s: --include-results dropped the rows", name)
		}
	}
	// nao's own answer is kept, secret scanned.
	ans, _ := json.Marshal(map[string]any{"id": "m2", "tool_name": "get_nao_answer", "tool_input": map[string]any{"chatId": "c1"},
		"tool_output": "Refunds are flat at 6%. key " + fakeToken})
	m, _ := decode(ans)
	got, _ := redactMCPCall(ans, m, agent.Options{})
	if s := string(got); !strings.Contains(s, "Refunds are flat") || strings.Contains(s, fakeToken) {
		t.Errorf("answer: %s", s)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// TestAskNaoPreviewRowsAreRedacted: an MCP ask_nao answer, as nao logged it
// (mcp/tools/sub-agent.ts answerCompletePayload; captured from nao at commit
// dc02889 on Postgres): the answer, then a JSON line whose queries carry
// preview rows. The answer stays, the preview goes.
func TestAskNaoPreviewRowsAreRedacted(t *testing.T) {
	out := "There are 3 customers.\n\n[chatId: c1]\n[chatUrl: http://localhost:5005/c1]\n[model: openaiCompatible/fake-model]\n" +
		`{"queries":[{"id":"query_7b517889","columns":["customer_id","first_name"],"row_count":3,"preview":[{"customer_id":1,"first_name":"Kathleen"}]}],"stories":[],"story_ids":[]}`
	row, _ := json.Marshal(map[string]any{"id": "m3", "tool_name": "ask_nao", "tool_input": map[string]any{"question": "How many customers?"}, "tool_output": out})
	m, _ := decode(row)
	got, ok := redactMCPCall(row, m, agent.Options{})
	if s := string(got); !ok || strings.Contains(s, "Kathleen") || !strings.Contains(s, "There are 3 customers.") ||
		!strings.Contains(s, "query_7b517889") || !strings.Contains(s, "row_count") {
		t.Errorf("ask_nao: %s", s)
	}
	m, _ = decode(row)
	if got, _ := redactMCPCall(row, m, agent.Options{IncludeResults: true}); !strings.Contains(string(got), "Kathleen") {
		t.Error("--include-results dropped the preview")
	}
}

// An answer that starts with a number is text, not the number: on Postgres
// mcp_call_log.tool_output is a jsonb string, and "17 of 103 ..." once read
// as 17, so its preview rows were kept (seen on nao's real rows copied to
// Postgres; sqlite, where the column is text holding JSON, was right).
func TestAnswerStartingWithANumberIsText(t *testing.T) {
	out := "17 of 103 products changed.\n\n[chatId: c1]\n" +
		`{"queries":[{"id":"query_3cb1cb4b","columns":["total_products"],"row_count":1,"preview":[{"total_products":103}]}],"stories":[],"story_ids":[]}`
	row, _ := json.Marshal(map[string]any{"id": "m4", "tool_name": "get_nao_answer", "tool_output": out})
	m, _ := decode(row)
	got, _ := redactMCPCall(row, m, agent.Options{})
	if s := string(got); strings.Contains(s, "preview") || !strings.Contains(s, "17 of 103 products changed.") {
		t.Errorf("get_nao_answer: %s", s)
	}
}
