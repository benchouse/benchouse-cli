package sigma

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/files"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

// span is one row of the AI usage table.
type span struct {
	conv, trace, id, parent, typ string
	at                           time.Time
	row                          map[string]any
	// version orders edits of the same row (Sigma input-table bookkeeping).
	version []float64
}

func newSpan(m map[string]any) span {
	s := span{row: m, trace: str(m[cTrace]), id: str(m[cSpan]), parent: str(m[cParent]), typ: str(m[cSpanType])}
	s.conv = str(m[cSession])
	if s.conv == "" {
		s.conv = s.trace
	}
	s.at, _ = parseTime(m[cAt])
	for _, k := range []string{sRowVersion, sSeq} {
		f, _ := number(m[k])
		s.version = append(s.version, f)
	}
	return s
}

// key identifies a row across edits: Sigma's row ID, else trace and span.
func (s span) key() string {
	if id := str(s.row[sID]); id != "" {
		return "id:" + id
	}
	return "span:" + s.trace + "\x00" + s.id + "\x00" + s.at.String()
}

func newer(a, b span) bool {
	for i := range a.version {
		if i < len(b.version) && a.version[i] != b.version[i] {
			return a.version[i] > b.version[i]
		}
	}
	return false
}

func (s span) isTool() bool {
	t := strings.ToLower(s.typ)
	return strings.Contains(t, "tool") || strings.Contains(t, "function")
}

// mapper turns one conversation's spans into turns.
type mapper struct {
	l              layout
	includeResults bool
	includeUsers   bool
	// stripped counts tool outputs dropped without --include-results.
	stripped int
}

// columns are the turns/v1 columns the table lets the reader fill.
func (mp *mapper) columns() turns.Columns {
	l := mp.l
	cols := []string{turns.ColConversationID, turns.ColPosition, turns.ColStatements, turns.ColToolCalls, turns.ColExtra}
	add := func(c string, tc ...string) {
		if l.has(c) {
			cols = append(cols, tc...)
		}
	}
	add(cSession, turns.ColSessionID)
	add(cModel, turns.ColModelName)
	add(cTokIn, turns.ColInputTokens)
	add(cTokOut, turns.ColOutputTokens)
	add(cUserName, turns.ColUserID)
	if mp.includeUsers {
		add(cUserName, turns.ColUserName)
	}
	add(cMeta, turns.ColOutcome, turns.ColErrorMessage)
	return turns.NewColumns(cols...)
}

// conversation builds the turns of one conversation's spans: one per
// trace, in order of the trace's first span.
func (mp *mapper) conversation(conv string, spans []span) []turns.Turn {
	byTrace := map[string][]span{}
	var order []string
	for _, s := range spans {
		k := s.trace
		if k == "" {
			k = s.id
		}
		if _, ok := byTrace[k]; !ok {
			order = append(order, k)
		}
		byTrace[k] = append(byTrace[k], s)
	}
	for _, k := range order {
		ss := byTrace[k]
		sort.SliceStable(ss, func(i, j int) bool { return ss[i].at.Before(ss[j].at) })
	}
	sort.SliceStable(order, func(i, j int) bool { return byTrace[order[i]][0].at.Before(byTrace[order[j]][0].at) })
	var out []turns.Turn
	for _, k := range order {
		t, ok := mp.turn(conv, k, byTrace[k])
		if !ok {
			continue
		}
		t.Position = turns.Ptr(int64(len(out) + 1))
		out = append(out, t)
	}
	return out
}

var questionKeys = []string{"question", "input", "query", "prompt", "text", "content", "message"}
var answerKeys = []string{"result", "output", "answer", "response", "text", "content", "message"}

func (mp *mapper) turn(conv, id string, ss []span) (turns.Turn, bool) {
	root := ss[0]
	for _, s := range ss {
		if strings.TrimSpace(s.parent) == "" {
			root = s
			break
		}
	}
	q := textOf(root.row[cInput], questionKeys, "user")
	if q == "" {
		for _, s := range ss {
			if q = textOf(s.row[cInput], questionKeys, "user"); q != "" {
				break
			}
		}
	}
	if q == "" {
		return turns.Turn{}, false
	}
	t := turns.Turn{TurnID: id, AskedAt: ss[0].at, Question: q, ConversationID: conv}
	if a := textOf(root.row[cOutput], answerKeys, "assistant"); a != "" {
		t.Answer = &a
	} else {
		for i := len(ss) - 1; i >= 0; i-- {
			if ss[i].isTool() {
				continue
			}
			if a := textOf(ss[i].row[cOutput], answerKeys, "assistant"); a != "" {
				t.Answer = &a
				break
			}
		}
	}
	first := func(col string) string {
		if v := str(root.row[col]); v != "" {
			return v
		}
		for _, s := range ss {
			if v := str(s.row[col]); v != "" {
				return v
			}
		}
		return ""
	}
	if s := str(root.row[cSession]); s != "" {
		t.SessionID = s
	}
	t.ModelName = first(cModel)
	t.InputTokens = sum(ss, cTokIn)
	t.OutputTokens = sum(ss, cTokOut)
	user := first(cUserName)
	meta := decodeJSON(root.row[cMeta])
	if uid := lookup(meta, "user_id", "userId", "User Id"); uid != "" {
		t.UserID = uid
	} else {
		t.UserID = user
	}
	if mp.includeUsers {
		t.UserName = user
	}
	if e := lookup(meta, "error", "Error"); e != "" && e != "false" && e != "null" {
		t.Outcome, t.ErrorMessage = "failed", e
	} else if t.Answer != nil {
		t.Outcome = "completed"
	}
	for _, s := range ss {
		for _, call := range toolCalls(s.row[cTools]) {
			mp.add(&t, call)
		}
		if s.isTool() && s.id != root.id {
			name := lookup(decodeJSON(s.row[cMeta]), "name", "tool_name", "tool")
			if name == "" {
				name = s.typ
			}
			mp.add(&t, call{name: name, input: decodeJSON(s.row[cInput]), output: decodeJSON(s.row[cOutput])})
		}
	}
	extra := map[string]any{"spans": len(ss)}
	for _, c := range []string{cAgentName, cAgentID, cTraceName, cProvider} {
		if v := first(c); v != "" {
			extra[c] = v
		}
	}
	if v := decodeJSON(root.row[cTags]); v != nil && str(v) != "" {
		extra[cTags] = v
	}
	if meta != nil {
		extra[cMeta] = meta
	}
	t.Extra = extra
	return t, true
}

type call struct {
	name, status, err string
	input, output     any
}

// add files a tool call as a statement when it carries SQL, else as a tool
// call. Outputs are dropped unless --include-results.
func (mp *mapper) add(t *turns.Turn, c call) {
	if c.output != nil && !mp.includeResults {
		mp.stripped++
		c.output = nil
	}
	c.output = maskSecrets(c.output) // kept results are data, never credentials
	status := strings.ToLower(c.status)
	if sql := sqlOf(c.name, c.input); sql != "" {
		st := turns.Statement{SQL: sql, Error: c.err, Result: c.output}
		switch {
		case c.err != "" || status == "failed" || status == "error":
			st.Status = "failed"
		case status == "success" || status == "succeeded" || status == "ok" || status == "completed":
			st.Status = "succeeded"
		}
		t.Statements = append(t.Statements, st)
		return
	}
	if c.name == "" {
		c.name = "tool"
	}
	tc := turns.ToolCall{Name: c.name, Input: c.input, Output: c.output, Status: c.status}
	if c.err != "" && tc.Status == "" {
		tc.Status = "failed"
	}
	t.ToolCalls = append(t.ToolCalls, tc)
}

// toolCalls reads a tool_calls value: a JSON array (or one object) of calls,
// possibly as a string.
func toolCalls(v any) []call {
	v = decodeJSON(v)
	var items []any
	switch t := v.(type) {
	case []any:
		items = t
	case map[string]any:
		items = []any{t}
	default:
		return nil
	}
	var out []call
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		c := call{
			name:   firstStr(m, "name", "tool", "tool_name", "toolName"),
			input:  decodeJSON(firstVal(m, "input", "arguments", "args", "parameters")),
			output: decodeJSON(firstVal(m, "output", "result", "response")),
			status: firstStr(m, "status"),
			err:    firstStr(m, "error"),
		}
		if fn != nil {
			if c.name == "" {
				c.name = firstStr(fn, "name")
			}
			if c.input == nil {
				c.input = decodeJSON(fn["arguments"])
			}
		}
		out = append(out, c)
	}
	return out
}

// sqlLoose reads as SQL under an argument named sql; sqlStrict, under a
// vaguer name (query, statement, code), also needs a FROM.
var (
	sqlPrefix = `(?is)^\s*(?:(?:--[^\n]*\n|/\*.*?\*/)\s*)*\(?\s*`
	sqlLoose  = regexp.MustCompile(sqlPrefix + `(?:select\s+\S|with\s+\S.*\bselect\s)`)
	sqlStrict = regexp.MustCompile(sqlPrefix + `(?:with\s+\S.*)?select\s.+\sfrom\s+[\w."\x60\[\]]+(?:\s|;|\)|$)`)
)

var sqlArgs = []string{"sql", "query", "statement", "sql_query", "sqlQuery", "code"}

// sqlOf finds SQL in a tool call's input, conservatively: a sql-ish argument
// whose value reads as a SELECT or WITH, or the whole input when the tool's
// name says SQL.
func sqlOf(name string, input any) string {
	switch t := input.(type) {
	case map[string]any:
		for _, k := range sqlArgs {
			re := sqlStrict
			if k == "sql" || k == "sql_query" || k == "sqlQuery" {
				re = sqlLoose
			}
			if s, ok := t[k].(string); ok && re.MatchString(s) && !(re == sqlStrict && strings.HasSuffix(strings.TrimSpace(s), "?")) {
				return strings.TrimSpace(s)
			}
		}
	case string:
		if strings.Contains(strings.ToLower(name), "sql") && sqlLoose.MatchString(t) {
			return strings.TrimSpace(t)
		}
	}
	return ""
}

// textOf reads text out of an input or output value: a string, JSON in a
// string, an object holding it under one of keys, or a chat message list
// (the last message of role).
func textOf(v any, keys []string, role string) string {
	v = decodeJSON(v)
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case map[string]any:
		for _, k := range keys {
			if x, ok := t[k]; ok {
				if s := textOf(x, keys, role); s != "" {
					return s
				}
			}
		}
		if parts, ok := t["parts"]; ok {
			return textOf(parts, keys, role)
		}
	case []any:
		var msgs, texts []string
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				if r := str(m["role"]); r != "" {
					if strings.EqualFold(r, role) || (role == "user" && strings.EqualFold(r, "human")) {
						msgs = append(msgs, textOf(m, keys, role))
					}
					continue
				}
			}
			if s := textOf(e, keys, role); s != "" {
				texts = append(texts, s)
			}
		}
		if len(msgs) > 0 {
			return msgs[len(msgs)-1]
		}
		return strings.Join(texts, "\n")
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// decodeJSON returns v, or the JSON a string holds when it is an object or
// an array.
func decodeJSON(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	if ts := strings.TrimSpace(s); strings.HasPrefix(ts, "{") || strings.HasPrefix(ts, "[") {
		var out any
		dec := json.NewDecoder(strings.NewReader(ts))
		dec.UseNumber()
		if dec.Decode(&out) == nil {
			return out
		}
	}
	return v
}

// lookup finds a text value under one of keys (case-insensitively) in an
// object, or in a [{key, value}] list.
func lookup(v any, keys ...string) string {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range keys {
			for mk, mv := range t {
				if strings.EqualFold(mk, k) && mv != nil {
					return str(mv)
				}
			}
		}
	case []any:
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				name := firstStr(m, "key", "name")
				for _, k := range keys {
					if name != "" && strings.EqualFold(name, k) {
						return str(firstVal(m, "value"))
					}
				}
				if s := lookup(m, keys...); s != "" && name == "" {
					return s
				}
			}
		}
	}
	return ""
}

func firstVal(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := str(m[k]); s != "" {
			return s
		}
	}
	return ""
}

func number(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

func sum(ss []span, col string) *int64 {
	var total int64
	seen := false
	for _, s := range ss {
		if f, ok := number(s.row[col]); ok {
			total += int64(f)
			seen = true
		}
	}
	if !seen {
		return nil
	}
	return &total
}

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999 -0700",
	"2006-01-02 15:04:05.999999999 Z07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999 MST",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
}

// parseTime reads a timestamp as the warehouses render it in JSON; one
// without a zone is UTC (the column is trace_created_at_utc).
func parseTime(v any) (time.Time, bool) {
	if f, ok := v.(json.Number); ok {
		if x, err := f.Float64(); err == nil {
			return time.Unix(0, int64(x*1e9)).UTC(), true
		}
	}
	s := strings.TrimSpace(str(v))
	if s == "" {
		return time.Time{}, false
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true
		}
	}
	if x, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Unix(0, int64(x*1e9)).UTC(), true
	}
	return time.Time{}, false
}

// maskSecrets masks recognisable secrets (tokens, keys, DSN passwords) in
// every string of v, in place.
func maskSecrets(v any) any {
	switch t := v.(type) {
	case string:
		s, _ := files.MaskTokens(t)
		return s
	case map[string]any:
		for k, c := range t {
			t[k] = maskSecrets(c)
		}
	case []any:
		for i, c := range t {
			t[i] = maskSecrets(c)
		}
	}
	return v
}
