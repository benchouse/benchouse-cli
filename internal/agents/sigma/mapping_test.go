package sigma

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/turns"
)

func TestSQLDetectionIsConservative(t *testing.T) {
	for _, c := range []struct {
		name  string
		input any
		want  string
	}{
		{"run_sql", map[string]any{"sql": "SELECT 1"}, "SELECT 1"},
		{"x", map[string]any{"query": "-- top\nWITH a AS (select 1) SELECT * FROM a"}, "-- top\nWITH a AS (select 1) SELECT * FROM a"},
		{"search", map[string]any{"query": "select the best customers"}, ""},
		{"search", map[string]any{"query": "orders last week"}, ""},
		{"search", map[string]any{"query": "select the customers from Ohio who churned?"}, ""},
		{"execute_sql", "select * from t", "select * from t"},
		{"chart", "select * from t", ""},
		{"x", map[string]any{"sql": "DELETE FROM t"}, ""},
	} {
		if got := sqlOf(c.name, c.input); got != c.want {
			t.Errorf("sqlOf(%s, %v) = %q, want %q", c.name, c.input, got, c.want)
		}
	}
}

func TestTextOf(t *testing.T) {
	msgs := `[{"role":"user","content":"first"},{"role":"assistant","content":"a"},{"role":"user","content":[{"type":"text","text":"second"}]}]`
	for _, c := range []struct {
		v    any
		want string
	}{
		{`{"question":"Orders?"}`, "Orders?"},
		{map[string]any{"prompt": map[string]any{"text": "42"}}, "42"},
		{"plain", "plain"},
		{msgs, "second"},
		{nil, ""},
		{`{"other":1}`, `{"other":1}`},
	} {
		if got := textOf(c.v, questionKeys, "user"); got != c.want {
			t.Errorf("textOf(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestParseTime(t *testing.T) {
	want := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for _, v := range []any{"2026-10-02 12:00:00.000", "2026-10-02T12:00:00Z", "2026-10-02T12:00:00.000+00:00",
		"2026-10-02 05:00:00.000 -0700", "2026-10-02T12:00:00", json.Number("1790942400")} {
		if got, ok := parseTime(v); !ok || !got.Equal(want) {
			t.Errorf("parseTime(%v) = %v %v", v, got, ok)
		}
	}
}

func TestToolCallShapes(t *testing.T) {
	calls := toolCalls(`[{"function":{"name":"run_sql","arguments":"{\"sql\":\"select 1\"}"}},"junk"]`)
	if len(calls) != 1 || calls[0].name != "run_sql" || sqlOf(calls[0].name, calls[0].input) != "select 1" {
		t.Errorf("calls = %+v", calls)
	}
	if toolCalls(nil) != nil || toolCalls("not json") != nil {
		t.Error("non-JSON tool_calls produced calls")
	}
}

// With --include-results a tool's output is kept, but a credential in it
// is still masked (found by the canary leak suite).
func TestKeptToolOutputsAreSecretScanned(t *testing.T) {
	const tok = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	mp := &mapper{includeResults: true}
	var tr turns.Turn
	mp.add(&tr, call{name: "connect", output: map[string]any{"log": []any{"password=" + tok + " rejected"}}})
	mp.add(&tr, call{name: "run_sql", input: map[string]any{"sql": "SELECT 1"}, output: "token " + tok})
	b, _ := json.Marshal(tr)
	if strings.Contains(string(b), tok) {
		t.Errorf("token kept: %s", b)
	}
	if !strings.Contains(string(b), "rejected") {
		t.Errorf("output dropped: %s", b)
	}
}
