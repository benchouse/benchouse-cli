package langfuse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/turns"
)

const resultsRedaction = "Langfuse observation outputs of SQL and other tool calls removed (row counts and status kept); " +
	"generation prompts and completions not read, only reasoning and usage. Use --include-results to keep tool outputs."

const usersRedaction = "Langfuse user ids that are email addresses left out. Use --include-users, or --anonymize-users for pseudonyms, to keep them."

var columns = turns.NewColumns(
	turns.ColCompletedAt, turns.ColOutcome, turns.ColErrorMessage, turns.ColConversationID, turns.ColUserID,
	turns.ColModelName, turns.ColInputTokens, turns.ColOutputTokens, turns.ColStatements, turns.ColReasoning,
	turns.ColToolCalls, turns.ColExtra,
)

// looksSQL is the conservative heuristic for an observation that is not the
// named SQL tool: its SQL must start with SELECT or WITH.
var looksSQL = regexp.MustCompile(`(?is)^\s*(with|select)\b`)

// sqlKeys are where a tool's input usually carries its SQL.
var sqlKeys = []string{"sql", "query", "statement", "sql_query", "sqlQuery", "code"}

// questionKeys and answerKeys are tried, in order, on an object input or
// output that is not a chat message list.
var (
	questionKeys = []string{"question", "query", "input", "prompt", "user_input", "message", "text", "content"}
	answerKeys   = []string{"answer", "output", "response", "result", "completion", "message", "text", "content"}
)

type mapper struct {
	sqlTool        string
	includeResults bool
	includeUsers   bool
	// anonymize: user ids are kept as they are and pseudonymised after
	// collection (package anon), under a key that stays on this machine.
	anonymize    bool
	droppedUsers bool
}

// turn maps one trace's observations onto a turn. It reports false when the
// trace's root observation is not among them.
//
//   - question: the root input as text: a plain string; the last user message
//     of a message list ({messages: [...]} or a bare list, content as a string
//     or text parts); else the first of question, query, input, prompt, ...;
//     else the input as compact JSON.
//   - answer: the root output the same way (last assistant message, choices,
//     or answer, output, response, ... keys); null when it is not text.
//   - asked_at / completed_at: root start and end; outcome failed when the root
//     is at level ERROR, completed when it ended, else running.
//   - conversation_id: sessionId; user_id: userId.
//   - model, tokens and reasoning: from GENERATION observations.
//   - statements: SQL in the input of the --sql-tool observations, or of any
//     other non-generation observation when it starts with SELECT or WITH.
//   - tool_calls: every other non-generation observation.
func (m *mapper) turn(traceID string, obs []map[string]any) (turns.Turn, bool) {
	sort.SliceStable(obs, func(i, j int) bool { return str(obs[i]["startTime"]) < str(obs[j]["startTime"]) })
	var root map[string]any
	for _, o := range obs {
		if o["isRootObservation"] == true {
			root = o
			break
		}
	}
	for _, o := range obs {
		if root != nil {
			break
		}
		if str(o["parentObservationId"]) == "" {
			root = o
		}
	}
	if root == nil {
		return turns.Turn{}, false
	}
	asked, ok := parseTime(root["startTime"])
	if !ok {
		return turns.Turn{}, false
	}
	t := turns.Turn{
		TurnID:         traceID,
		AskedAt:        asked,
		ConversationID: str(root["sessionId"]),
		UserID:         m.user(str(root["userId"])),
		Statements:     []turns.Statement{},
		Reasoning:      []turns.Reasoning{},
		ToolCalls:      []turns.ToolCall{},
		Extra:          map[string]any{},
	}
	in := parseIO(root["input"])
	t.Question = text(in, "user", 0)
	if strings.TrimSpace(t.Question) == "" && in != nil {
		b, _ := json.Marshal(in)
		t.Question = string(b)
	}
	if s := text(parseIO(root["output"]), "assistant", 0); strings.TrimSpace(s) != "" {
		t.Answer = &s
	}
	if end, ok := parseTime(root["endTime"]); ok {
		t.CompletedAt = &end
	}
	switch {
	case str(root["level"]) == "ERROR":
		t.Outcome, t.ErrorMessage = "failed", str(root["statusMessage"])
		t.Answer = nil
	case t.CompletedAt != nil:
		t.Outcome = "completed"
	default:
		t.Outcome = "running"
	}
	for k, key := range map[string]string{"trace_name": "traceName", "environment": "environment", "release": "release"} {
		if v := str(root[key]); v != "" {
			t.Extra[k] = v
		}
	}
	if tags, ok := root["tags"].([]any); ok && len(tags) > 0 {
		t.Extra["tags"] = tags
	}

	var in64, out64 int64
	var haveUsage bool
	var cost float64
	for _, o := range obs {
		if c, ok := float(o["totalCost"]); ok {
			cost += c
		}
		typ := str(o["type"])
		if typ == "GENERATION" {
			if model := str(o["model"]); model != "" {
				t.ModelName = model // the last generation's model wins
			}
			if u, ok := o["usageDetails"].(map[string]any); ok {
				if n, ok := integer(u["input"]); ok {
					in64, haveUsage = in64+n, true
				}
				if n, ok := integer(u["output"]); ok {
					out64, haveUsage = out64+n, true
				}
			}
			for _, r := range reasoningOf(parseIO(o["output"]), 0) {
				t.Reasoning = append(t.Reasoning, turns.Reasoning{Text: r, Label: "deliberation"})
			}
			continue
		}
		if typ == "EMBEDDING" || sameObs(o, root) {
			continue
		}
		start, _ := parseTime(o["startTime"])
		end, ended := parseTime(o["endTime"])
		status, errMsg := "", ""
		switch {
		case str(o["level"]) == "ERROR":
			status, errMsg = "failed", str(o["statusMessage"])
		case ended:
			status = "succeeded"
		}
		input := parseIO(o["input"])
		hinted := m.sqlTool != "" && strings.EqualFold(str(o["name"]), m.sqlTool)
		if sql := sqlOf(input, hinted, 0); sql != "" {
			st := turns.Statement{SQL: sql, Status: status, Error: errMsg, StartedAt: tp(start, true)}
			if ended {
				st.EndedAt = &end
			}
			output := parseIO(o["output"])
			if n, ok := rowCount(output); ok {
				st.Rows = &n
			}
			if m.includeResults {
				st.Result = output
			}
			t.Statements = append(t.Statements, st)
			continue
		}
		tc := turns.ToolCall{Name: str(o["name"]), Input: input, Status: status, StartedAt: tp(start, true)}
		if tc.Name == "" {
			tc.Name = strings.ToLower(typ)
		}
		if ended {
			tc.EndedAt = &end
		}
		if m.includeResults {
			tc.Output = parseIO(o["output"])
		}
		t.ToolCalls = append(t.ToolCalls, tc)
	}
	if haveUsage {
		t.InputTokens, t.OutputTokens = &in64, &out64
	}
	if cost > 0 {
		t.Extra["cost_usd"] = cost
	}
	t.Extra["observations"] = len(obs)
	return t, true
}

// user keeps a user id, unless it is an email address, users were not asked
// for and nothing will pseudonymise it: then it is left out. (An unkeyed
// hash would not do: a list of likely emails reverses it.)
func (m *mapper) user(id string) string {
	if id == "" || m.includeUsers || m.anonymize || !strings.Contains(id, "@") {
		return id
	}
	m.droppedUsers = true
	return ""
}

func sameObs(a, b map[string]any) bool { return str(a["id"]) == str(b["id"]) }

// parseIO reads an observation input or output. The v2 API returns them as
// raw strings, JSON-encoded when the SDK sent structured data.
func parseIO(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, `"`) {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		var out any
		if dec.Decode(&out) == nil {
			return out
		}
	}
	return s
}

// text extracts the human-readable text from an input or output, preferring
// the last message with role.
func text(v any, role string, depth int) string {
	if depth > 4 {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case []any:
		if s := parts(t); s != "" {
			return s
		}
		for i := len(t) - 1; i >= 0; i-- {
			if msg, ok := t[i].(map[string]any); ok && strings.EqualFold(str(msg["role"]), role) {
				return text(msg["content"], role, depth+1)
			}
		}
		if len(t) > 0 {
			return text(t[len(t)-1], role, depth+1)
		}
	case map[string]any:
		if msgs, ok := t["messages"].([]any); ok {
			return text(msgs, role, depth+1)
		}
		if _, ok := t["role"]; ok {
			return text(t["content"], role, depth+1)
		}
		if ch, ok := t["choices"].([]any); ok && len(ch) > 0 {
			if c, ok := ch[0].(map[string]any); ok {
				return text(c["message"], role, depth+1)
			}
		}
		keys := answerKeys
		if role == "user" {
			keys = questionKeys
		}
		for _, k := range keys {
			if s := text(t[k], role, depth+1); strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// parts joins content parts [{type: "text", text: ...}], or "" when xs is not
// a list of text parts.
func parts(xs []any) string {
	var out []string
	for _, x := range xs {
		p, ok := x.(map[string]any)
		if !ok || str(p["type"]) != "text" {
			return ""
		}
		out = append(out, str(p["text"]))
	}
	return strings.Join(out, "\n")
}

// reasoningOf finds reasoning a generation exposes: reasoning_content,
// reasoning or thinking text, or thinking / reasoning content parts.
func reasoningOf(v any, depth int) []string {
	if depth > 4 {
		return nil
	}
	var out []string
	switch t := v.(type) {
	case []any:
		for _, x := range t {
			out = append(out, reasoningOf(x, depth+1)...)
		}
	case map[string]any:
		switch str(t["type"]) {
		case "thinking":
			if s := str(t["thinking"]); s != "" {
				return []string{s}
			}
		case "reasoning":
			if s := str(t["text"]); s != "" {
				return []string{s}
			}
		}
		for _, k := range []string{"reasoning_content", "reasoning", "thinking"} {
			if s, ok := t[k].(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		for _, k := range []string{"content", "message", "choices"} {
			if _, isString := t[k].(string); !isString {
				out = append(out, reasoningOf(t[k], depth+1)...)
			}
		}
	}
	return out
}

// sqlOf returns the SQL in a tool input: the input itself when it is a
// string, or the first string under sqlKeys (also one level down, under
// arguments, args, input or params). Unless hinted, it must look like SQL.
func sqlOf(v any, hinted bool, depth int) string {
	ok := func(s string) bool { return strings.TrimSpace(s) != "" && (hinted || looksSQL.MatchString(s)) }
	switch t := v.(type) {
	case string:
		if ok(t) {
			return t
		}
	case map[string]any:
		for _, k := range sqlKeys {
			if s, isString := t[k].(string); isString && ok(s) {
				return s
			}
		}
		if depth == 0 {
			for _, k := range []string{"arguments", "args", "input", "params"} {
				if s := sqlOf(t[k], hinted, depth+1); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// rowCount reads the number of rows a SQL tool returned, from an explicit
// count or the length of its row list.
func rowCount(v any) (int64, bool) {
	switch t := v.(type) {
	case []any:
		return int64(len(t)), true
	case map[string]any:
		for _, k := range []string{"row_count", "rowCount", "num_rows", "rows_count"} {
			if n, ok := integer(t[k]); ok {
				return n, true
			}
		}
		for _, k := range []string{"rows", "data", "results", "records"} {
			if xs, ok := t[k].([]any); ok {
				return int64(len(xs)), true
			}
		}
	}
	return 0, false
}

func parseTime(v any) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, str(v))
	return t.UTC(), err == nil
}

func tp(t time.Time, ok bool) *time.Time {
	if !ok || t.IsZero() {
		return nil
	}
	return &t
}

func integer(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, true
		}
		f, err := t.Float64()
		return int64(f), err == nil
	case float64:
		return int64(t), true
	}
	return 0, false
}

func float(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	}
	return 0, false
}

func decode(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	err := dec.Decode(&m)
	return m, err
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	default:
		return fmt.Sprintf("%v", t)
	}
}
