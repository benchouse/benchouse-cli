package hex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/turns"
)

var columns = turns.NewColumns(
	turns.ColCompletedAt, turns.ColOutcome, turns.ColErrorMessage, turns.ColConversationID, turns.ColPosition,
	turns.ColUserID, turns.ColClientInterface, turns.ColModelName, turns.ColStatements, turns.ColReasoning,
	turns.ColToolCalls, turns.ColExtra,
)

// event is one step of a thread, from a full record or a rendered block.
type event struct {
	kind    string // user, answer, thinking, error, tool_use, tool_result
	at      time.Time
	text    string
	name    string
	id      string
	input   any
	output  any
	isError bool
	hasOut  bool
}

type call struct {
	name, id      string
	input, output any
	hasOut, isErr bool
	at            time.Time
}

type draft struct {
	asked     time.Time
	question  []string
	answer    []string
	reasoning []turns.Reasoning
	calls     []*call
	errors    []string
	last      time.Time
	assistant bool
}

type mapper struct {
	includeResults bool
	// counts for notes
	fromRecords, fromText, unread, orphans int
}

func (m *mapper) notes() []string {
	var out []string
	if m.fromText > 0 {
		out = append(out, fmt.Sprintf("%d threads had no full message records (full=true): read from their rendered text, without tool calls or SQL", m.fromText))
	}
	if m.unread > 0 {
		out = append(out, fmt.Sprintf("%d threads held no message the CLI could read as a question", m.unread))
	}
	if m.orphans > 0 {
		out = append(out, fmt.Sprintf("left out %d assistant blocks that came before any question (a thread's oldest messages beyond the page limit)", m.orphans))
	}
	return out
}

// turns maps one thread's blocks, oldest first, onto its turns.
func (m *mapper) turns(t thread, blocks []map[string]any) []turns.Turn {
	events, records := parseBlocks(blocks, t.created)
	if records {
		m.fromRecords++
	} else if len(blocks) > 0 {
		m.fromText++
	}
	var drafts []*draft
	var cur *draft
	for _, e := range events {
		if e.kind == "user" {
			if cur == nil || cur.assistant {
				cur = &draft{asked: e.at}
				drafts = append(drafts, cur)
			}
			cur.question = append(cur.question, e.text)
			continue
		}
		if cur == nil {
			m.orphans++
			continue
		}
		cur.assistant = true
		if !e.at.IsZero() {
			cur.last = e.at
		}
		switch e.kind {
		case "answer":
			cur.answer = append(cur.answer, e.text)
		case "thinking":
			cur.reasoning = append(cur.reasoning, turns.Reasoning{Text: e.text, Label: "deliberation"})
		case "error":
			cur.errors = append(cur.errors, e.text)
		case "tool_use":
			cur.calls = append(cur.calls, &call{name: e.name, id: e.id, input: e.input, output: e.output, hasOut: e.hasOut, isErr: e.isError, at: e.at})
		case "tool_result":
			if c := cur.match(e.id); c != nil {
				c.output, c.hasOut, c.isErr = e.output, true, e.isError
			}
		}
	}
	if len(drafts) == 0 {
		m.unread++
		return nil
	}
	tid := t.id()
	out := make([]turns.Turn, 0, len(drafts))
	for i, d := range drafts {
		tr := turns.Turn{
			TurnID:          tid + ":" + strconv.Itoa(i+1),
			AskedAt:         d.asked,
			Question:        strings.TrimSpace(strings.Join(d.question, "\n\n")),
			ConversationID:  tid,
			Position:        turns.Ptr(int64(i + 1)),
			ClientInterface: clientInterface(t.Source),
			Reasoning:       d.reasoning,
			Statements:      []turns.Statement{},
			ToolCalls:       []turns.ToolCall{},
		}
		if tr.AskedAt.IsZero() {
			tr.AskedAt = t.created
		}
		if t.UserID != nil {
			tr.UserID = *t.UserID
		}
		switch {
		case t.EffectiveModel != nil && *t.EffectiveModel != "":
			tr.ModelName = *t.EffectiveModel
		case t.Model != "" && !strings.EqualFold(t.Model, "auto"):
			tr.ModelName = t.Model
		}
		if a := strings.TrimSpace(strings.Join(d.answer, "\n\n")); a != "" {
			tr.Answer = &a
		}
		if !d.last.IsZero() {
			tr.CompletedAt = turns.Ptr(d.last)
		}
		switch {
		case len(d.errors) > 0:
			tr.Outcome, tr.ErrorMessage = "failed", strings.Join(d.errors, "; ")
		case tr.Answer != nil:
			tr.Outcome = "completed"
		}
		for _, c := range d.calls {
			m.addCall(&tr, c)
		}
		extra := map[string]any{"thread_type": t.Type}
		if t.Classification != nil {
			extra["classification"] = *t.Classification
		}
		if t.Title != nil && *t.Title != "" {
			extra["thread_title"] = *t.Title
		}
		if len(t.Topics) > 0 {
			var names []string
			for _, tp := range t.Topics {
				names = append(names, tp.Name)
			}
			extra["topics"] = names
		}
		if i == len(drafts)-1 {
			if len(t.Feedback) > 0 {
				extra["feedback"] = t.Feedback
			}
			if len(t.Warnings) > 0 {
				extra["warnings"] = t.Warnings
			}
		}
		tr.Extra = extra
		out = append(out, tr)
	}
	return out
}

// match finds the call a tool result answers: by id, else the last call
// still without an output.
func (d *draft) match(id string) *call {
	for i := len(d.calls) - 1; i >= 0; i-- {
		if id != "" && d.calls[i].id == id {
			return d.calls[i]
		}
	}
	for i := len(d.calls) - 1; i >= 0; i-- {
		if !d.calls[i].hasOut {
			return d.calls[i]
		}
	}
	return nil
}

func (m *mapper) addCall(tr *turns.Turn, c *call) {
	status := ""
	if c.hasOut {
		status = "succeeded"
		if c.isErr {
			status = "failed"
		}
	}
	var at *time.Time
	if !c.at.IsZero() {
		at = turns.Ptr(c.at)
	}
	if sql := sqlOf(c.name, c.input); sql != "" {
		st := turns.Statement{SQL: sql, Status: status, StartedAt: at}
		if status == "" {
			st.Status = "not_executed"
		}
		if c.isErr {
			st.Error = short(text(c.output))
		}
		if m.includeResults && c.hasOut {
			st.Result = c.output
		}
		tr.Statements = append(tr.Statements, st)
		return
	}
	tc := turns.ToolCall{Name: c.name, Input: c.input, Status: status, StartedAt: at}
	if tc.Name == "" {
		tc.Name = "tool"
	}
	if m.includeResults && c.hasOut {
		tc.Output = c.output
	}
	tr.ToolCalls = append(tr.ToolCalls, tc)
}

var (
	looksSQL = regexp.MustCompile(`(?is)^\s*(with|select)\b`)
	sqlKeys  = []string{"sql", "query", "statement", "sql_query", "sqlQuery", "code", "source"}
)

// sqlOf returns the SQL a tool call's input carries, conservatively: a SQL
// key whose value starts with SELECT or WITH, or any non-empty one when the
// tool's name mentions sql. A string input counts the same way.
func sqlOf(name string, input any) string {
	named := strings.Contains(strings.ToLower(name), "sql")
	check := func(s string) string {
		s = strings.TrimSpace(s)
		if s != "" && (looksSQL.MatchString(s) || named) {
			return s
		}
		return ""
	}
	switch v := input.(type) {
	case string:
		if named || looksSQL.MatchString(v) {
			if obj, ok := decodeObject(v); ok {
				return sqlOf(name, obj)
			}
			return check(v)
		}
	case map[string]any:
		for _, k := range sqlKeys {
			if s, ok := v[k].(string); ok {
				if sql := check(s); sql != "" {
					return sql
				}
			}
		}
		for _, k := range []string{"input", "args", "arguments", "parameters", "cell"} {
			if sql := sqlOf(name, v[k]); sql != "" {
				return sql
			}
		}
	}
	return ""
}

// parseBlocks reads a thread's blocks into events. When any block is a full
// message record, only records are read: the rendered text repeats them.
func parseBlocks(blocks []map[string]any, threadAt time.Time) ([]event, bool) {
	var recs []event
	found := false
	for _, b := range blocks {
		at, ok := parseTime(stringOf(b["createdAt"]))
		if !ok {
			at = threadAt
		}
		if rec := recordOf(b); rec != nil {
			found = true
			recs = append(recs, recordEvents(rec, at)...)
		}
	}
	if found {
		return recs, true
	}
	var out []event
	role := ""
	for _, b := range blocks {
		at, ok := parseTime(stringOf(b["createdAt"]))
		if !ok {
			at = threadAt
		}
		e, r := renderedEvent(stringOf(b["text"]), role)
		role = r
		if strings.TrimSpace(e.text) == "" {
			continue
		}
		e.at = at
		out = append(out, e)
	}
	return out, false
}

var (
	rolePrefix = regexp.MustCompile(`(?is)^\s*\**\s*\[?\s*(user|human|assistant|agent|hex|ai|magic|system|tool)\s*\]?\s*\**\s*:\s*`)
	tagPrefix  = regexp.MustCompile(`(?is)^\s*(?:[\[<(]\s*(thinking|reasoning|error)\s*[\]>)]\s*:?|(thinking|reasoning|error)\s*:)\s*`)
)

// renderedEvent reads one rendered text block: an optional role prefix and
// an optional thinking/error tag, in either order. A block without a role
// continues the previous block's role.
func renderedEvent(s, prevRole string) (event, string) {
	role, tag := "", ""
	for range 2 {
		if m := rolePrefix.FindStringSubmatch(s); m != nil && role == "" {
			role = normRole(m[1])
			s = s[len(m[0]):]
			continue
		}
		if m := tagPrefix.FindStringSubmatch(s); m != nil && tag == "" {
			tag = strings.ToLower(m[1] + m[2])
			s = s[len(m[0]):]
		}
	}
	if role == "" {
		role = prevRole
	}
	if role == "" {
		role = "assistant"
	}
	text := strings.TrimSpace(s)
	switch {
	case tag == "thinking" || tag == "reasoning":
		return event{kind: "thinking", text: text}, role
	case tag == "error":
		return event{kind: "error", text: text}, role
	case role == "user":
		return event{kind: "user", text: text}, role
	case role == "system" || role == "tool":
		return event{}, role
	}
	return event{kind: "answer", text: text}, role
}

func normRole(r string) string {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "user", "human":
		return "user"
	case "assistant", "agent", "hex", "ai", "magic", "model", "bot":
		return "assistant"
	case "tool", "function":
		return "tool"
	case "system", "developer":
		return "system"
	}
	return ""
}

// recordOf returns the full message record a block carries, or nil: a
// record/message/json field, the block itself when it has a role, or text
// holding a JSON object with a role or content.
func recordOf(b map[string]any) map[string]any {
	for _, k := range []string{"record", "message", "json", "data"} {
		switch v := b[k].(type) {
		case map[string]any:
			return v
		case string:
			if obj, ok := decodeObject(v); ok && isRecord(obj) {
				return obj
			}
		}
	}
	if _, ok := b["role"]; ok {
		return b
	}
	if obj, ok := decodeObject(stringOf(b["text"])); ok && isRecord(obj) {
		return obj
	}
	return nil
}

func isRecord(obj map[string]any) bool {
	for _, k := range []string{"role", "content", "parts", "author"} {
		if _, ok := obj[k]; ok {
			return true
		}
	}
	return false
}

// recordEvents reads one full message record.
func recordEvents(rec map[string]any, at time.Time) []event {
	if t, ok := parseTime(firstString(rec, "createdAt", "created_at", "timestamp")); ok {
		at = t
	}
	if inner, ok := rec["message"].(map[string]any); ok && rec["content"] == nil && rec["parts"] == nil {
		if rec["role"] == nil {
			return recordEvents(inner, at)
		}
		rec = mergeRole(inner, rec["role"])
	}
	role := normRole(firstString(rec, "role", "author", "sender"))
	if role == "" {
		role = normRole(stringOf(rec["type"]))
	}
	var out []event
	add := func(e event) {
		if e.at.IsZero() {
			e.at = at
		}
		out = append(out, e)
	}
	textKind := "answer"
	switch role {
	case "user":
		textKind = "user"
	case "system":
		return nil
	}
	content := rec["content"]
	if content == nil {
		content = rec["parts"]
	}
	if content == nil {
		content = rec["blocks"]
	}
	switch c := content.(type) {
	case string:
		if role == "tool" {
			add(event{kind: "tool_result", id: firstString(rec, "tool_call_id", "toolCallId", "tool_use_id"), output: c, hasOut: true})
		} else if strings.TrimSpace(c) != "" {
			add(event{kind: textKind, text: c})
		}
	case []any:
		for _, p := range c {
			switch part := p.(type) {
			case string:
				if strings.TrimSpace(part) != "" {
					add(event{kind: textKind, text: part})
				}
			case map[string]any:
				for _, e := range partEvents(part, textKind) {
					add(e)
				}
			}
		}
	}
	if s := firstString(rec, "thinking", "reasoning"); strings.TrimSpace(s) != "" {
		add(event{kind: "thinking", text: s})
	}
	for _, k := range []string{"toolCalls", "tool_calls"} {
		if calls, ok := rec[k].([]any); ok {
			for _, c := range calls {
				if cm, ok := c.(map[string]any); ok {
					add(toolUse(cm))
				}
			}
		}
	}
	switch e := rec["error"].(type) {
	case string:
		if e != "" {
			add(event{kind: "error", text: e})
		}
	case map[string]any:
		add(event{kind: "error", text: firstString(e, "message", "type")})
	}
	return out
}

func mergeRole(m map[string]any, role any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	if out["role"] == nil {
		out["role"] = role
	}
	return out
}

// partEvents reads one content part of a record.
func partEvents(p map[string]any, textKind string) []event {
	at, _ := parseTime(firstString(p, "createdAt", "timestamp"))
	typ := strings.ToLower(stringOf(p["type"]))
	switch typ {
	case "text", "markdown", "output_text", "input_text", "":
		if s := firstString(p, "text", "content", "markdown"); strings.TrimSpace(s) != "" {
			return []event{{kind: textKind, text: s, at: at}}
		}
	case "thinking", "reasoning", "redacted_thinking":
		if s := firstString(p, "thinking", "text", "reasoning", "content"); strings.TrimSpace(s) != "" {
			return []event{{kind: "thinking", text: s, at: at}}
		}
	case "tool_use", "tool_call", "tool-call", "toolcall", "function_call", "server_tool_use":
		e := toolUse(p)
		e.at = at
		return []event{e}
	case "tool_result", "tool-result", "toolresult", "function_result", "function_call_output":
		return []event{{kind: "tool_result", at: at, id: firstString(p, "tool_use_id", "toolUseId", "toolCallId", "tool_call_id", "call_id", "id"),
			output: firstValue(p, "content", "output", "result"), hasOut: true, isError: truthy(p["is_error"]) || truthy(p["isError"])}}
	case "tool-invocation":
		if inv, ok := p["toolInvocation"].(map[string]any); ok {
			e := toolUse(inv)
			e.at = at
			return []event{e}
		}
	case "error":
		return []event{{kind: "error", text: firstString(p, "message", "text", "error"), at: at}}
	case "mention":
		return nil
	default:
		if strings.HasPrefix(typ, "tool-") { // AI SDK typed tool part
			e := toolUse(p)
			if e.name == "" {
				e.name = stringOf(p["type"])[len("tool-"):]
			}
			e.at = at
			return []event{e}
		}
	}
	return nil
}

// toolUse reads a tool call in any of the usual shapes; an output on the
// call itself (AI SDK parts) completes it.
func toolUse(p map[string]any) event {
	e := event{kind: "tool_use", name: firstString(p, "name", "toolName", "tool", "tool_name"),
		id:    firstString(p, "id", "toolCallId", "tool_call_id", "toolUseId", "call_id"),
		input: firstValue(p, "input", "args", "arguments", "parameters", "params")}
	if fn, ok := p["function"].(map[string]any); ok { // OpenAI shape
		if e.name == "" {
			e.name = stringOf(fn["name"])
		}
		if e.input == nil {
			e.input = fn["arguments"]
		}
	}
	if s, ok := e.input.(string); ok {
		if obj, ok := decodeObject(s); ok {
			e.input = obj
		}
	}
	if out := firstValue(p, "output", "result"); out != nil {
		e.output, e.hasOut = out, true
		e.isError = truthy(p["is_error"]) || truthy(p["isError"]) || stringOf(p["state"]) == "output-error"
	}
	if errText := stringOf(p["errorText"]); errText != "" {
		e.output, e.hasOut, e.isError = errText, true, true
	}
	return e
}

func decodeObject(s string) (map[string]any, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, false
	}
	return m, true
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := stringOf(m[k]); s != "" {
			return s
		}
	}
	return ""
}

func firstValue(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func stringOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	}
	return ""
}

// text renders a tool output for an error message.
func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func truthy(v any) bool { b, _ := v.(bool); return b }
