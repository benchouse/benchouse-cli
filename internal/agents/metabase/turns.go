package metabase

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/files"
	"github.com/benchouse/benchouse-cli/internal/pgread"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

// messageRow is a metabot_message row with conversation_user_id and
// role_position. Columns from later Metabase versions are pointers or raw
// JSON, absent on older ones.
type messageRow struct {
	ID                  json.Number     `json:"id"`
	ConversationID      string          `json:"conversation_id"`
	CreatedAt           string          `json:"created_at"`
	Role                string          `json:"role"`
	ProfileID           *string         `json:"profile_id"`
	Data                json.RawMessage `json:"data"`  // text holding JSON
	Usage               json.RawMessage `json:"usage"` // text: {"<model>": {"prompt": n, "completion": m}}
	DataVersion         *json.Number    `json:"data_version"`
	UserID              *json.Number    `json:"user_id"`
	ConversationUserID  *json.Number    `json:"conversation_user_id"`
	SlackMsgID          *string         `json:"slack_msg_id"`
	ChannelID           *string         `json:"channel_id"`
	DeletedAt           *string         `json:"deleted_at"`
	Finished            json.RawMessage `json:"finished"` // absent before v61; null while in flight
	Error               *string         `json:"error"`
	FinishReason        *string         `json:"finish_reason"`
	ContextWindowFull   *bool           `json:"context_window_full"`
	ForkedFromMessageID *json.Number    `json:"forked_from_message_id"`
	RolePosition        *json.Number    `json:"role_position"`
}

type feedbackRow struct {
	MessageID json.Number `json:"message_id"`
	Positive  *bool       `json:"positive"`
	IssueType *string     `json:"issue_type"`
	Freeform  *string     `json:"freeform_feedback"`
}

type userRow struct {
	ID        json.Number `json:"id"`
	FirstName *string     `json:"first_name"`
	LastName  *string     `json:"last_name"`
	Email     string      `json:"email"`
}

// sqlTools generate SQL (Metabase's metabot.tools/query-generation-tool-names
// minus construct_notebook_query, which builds MBQL).
var sqlTools = map[string]bool{"create_sql_query": true, "edit_sql_query": true, "replace_sql_query": true}

// keptTools return metadata or the agent's own words, not data, so their
// outputs are kept: they show which definitions the agent found or lacked.
var keptTools = map[string]bool{
	"search": true, "list_available_data_sources": true, "list_available_fields": true,
	"read_resource": true, "retrieve_library_entities": true, "list_snippets": true,
	"get_snippet_details": true, "load_skill": true, "ask_for_sql_clarification": true,
}

// building is a turn being assembled from its messages.
type building struct {
	turn       turns.Turn
	answer     []string
	messageIDs []string
	assistants int
	finished   json.RawMessage
	sawError   bool
	models     map[string]int64
	// queries maps a generated query's id to its statement, so a later
	// run_query of that id can say it ran and how many rows it returned.
	queries map[string]int
}

// buildTurns groups messages into turns: a user message opens one, the
// assistant messages after it in the conversation answer it. It returns the
// turns and what was left out: questions copied into forked conversations,
// and messages without a question in the window.
func buildTurns(got map[string][]json.RawMessage, opts agent.Options) (out []turns.Turn, copies, orphans int) {
	feedback := map[string][]any{}
	for _, r := range got["metabot_feedback"] {
		var f feedbackRow
		if pgread.Decode(r, &f) != nil {
			continue
		}
		fb := map[string]any{}
		if f.Positive != nil {
			fb["score"] = map[bool]int{true: 1, false: -1}[*f.Positive]
		}
		if f.IssueType != nil && *f.IssueType != "" {
			fb["issue_type"] = *f.IssueType
		}
		if f.Freeform != nil && strings.TrimSpace(*f.Freeform) != "" {
			fb["comment"] = strings.TrimSpace(*f.Freeform)
		}
		feedback[f.MessageID.String()] = append(feedback[f.MessageID.String()], fb)
	}
	users := map[string]userRow{}
	for _, r := range got["core_user"] {
		var u userRow
		if pgread.Decode(r, &u) == nil {
			users[u.ID.String()] = u
		}
	}

	var cur *building
	flush := func() {
		if cur != nil {
			out = append(out, finish(cur, feedback))
			cur = nil
		}
	}
	for _, r := range got["metabot_message"] {
		var m messageRow
		if pgread.Decode(r, &m) != nil {
			continue
		}
		if cur != nil && cur.turn.ConversationID != m.ConversationID {
			flush()
		}
		version := int64(1)
		if m.DataVersion != nil {
			version, _ = m.DataVersion.Int64()
		}
		parts := parseParts(pgread.JSONText(m.Data), version)

		if m.Role == "user" {
			flush()
			// A forked conversation copies its source's messages; the turn is
			// already counted in the source conversation.
			if m.ForkedFromMessageID != nil {
				copies++
				continue
			}
			b, err := openTurn(m, parts, users, opts)
			if err != nil {
				orphans++
				continue
			}
			cur = b
			continue
		}
		if m.Role != "assistant" {
			continue
		}
		if cur == nil {
			if m.ForkedFromMessageID == nil {
				orphans++ // its question is before the window, or was a fork copy
			}
			continue
		}
		addAssistant(cur, m, parts, opts)
	}
	flush()
	return out, copies, orphans
}

// leftOut are the notes on what buildTurns left out.
func leftOut(copies, orphans int) []string {
	var notes []string
	if copies > 0 {
		notes = append(notes, fmt.Sprintf("left out %d questions copied into forked conversations (each is read once, from the original)", copies))
	}
	if orphans > 0 {
		notes = append(notes, fmt.Sprintf("left out %d messages without a question in the window", orphans))
	}
	return notes
}

func openTurn(m messageRow, parts []part, users map[string]userRow, opts agent.Options) (*building, error) {
	asked, ok := pgread.Time(m.CreatedAt)
	if !ok {
		return nil, fmt.Errorf("message %s: created_at %q", m.ID, m.CreatedAt)
	}
	var q []string
	for _, p := range parts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			q = append(q, p.Text)
		}
	}
	t := turns.Turn{
		TurnID:          m.ID.String(),
		AskedAt:         asked,
		Question:        strings.Join(q, "\n\n"),
		ConversationID:  m.ConversationID,
		ClientInterface: "web",
		Statements:      []turns.Statement{},
		ToolCalls:       []turns.ToolCall{},
		Extra:           map[string]any{},
	}
	if m.RolePosition != nil {
		if n, err := m.RolePosition.Int64(); err == nil {
			t.Position = &n
		}
	}
	switch {
	case m.UserID != nil:
		t.UserID = m.UserID.String()
	case m.ConversationUserID != nil:
		t.UserID = m.ConversationUserID.String()
	}
	if m.SlackMsgID != nil || m.ChannelID != nil {
		t.ClientInterface = "slack"
	}
	if opts.IncludeUsers {
		if u, ok := users[t.UserID]; ok {
			t.UserName = strings.TrimSpace(deref(u.FirstName) + " " + deref(u.LastName))
			t.UserEmail = u.Email
		}
	}
	return &building{turn: t, messageIDs: []string{m.ID.String()}, models: map[string]int64{}}, nil
}

func addAssistant(b *building, m messageRow, parts []part, opts agent.Options) {
	t := &b.turn
	b.assistants++
	b.messageIDs = append(b.messageIDs, m.ID.String())
	b.finished = m.Finished
	if m.ProfileID != nil && *m.ProfileID != "" {
		t.Extra["profile"] = *m.ProfileID // internal, slackbot, embedding_next, nlq, sql, ...
		if *m.ProfileID == "slackbot" {
			t.ClientInterface = "slack"
		}
	}
	if m.SlackMsgID != nil {
		t.ClientInterface = "slack"
	}
	if m.Error != nil && strings.TrimSpace(*m.Error) != "" {
		b.sawError = true
		t.ErrorMessage = errorText(json.RawMessage(*m.Error)) // text, sometimes a JSON payload
	}
	if m.FinishReason != nil && *m.FinishReason != "" {
		t.Extra["finish_reason"] = *m.FinishReason
	}
	if m.ContextWindowFull != nil && *m.ContextWindowFull {
		t.Extra["context_window_full"] = true
	}
	if m.DeletedAt != nil {
		t.Extra["answer_deleted_at"] = *m.DeletedAt // a Slack response removed by a user
	}

	if u := pgread.JSONText(m.Usage); u != nil {
		var usage map[string]struct {
			Prompt     int64 `json:"prompt"`
			Completion int64 `json:"completion"`
		}
		if json.Unmarshal(u, &usage) == nil {
			for model, n := range usage {
				t.InputTokens = add(t.InputTokens, n.Prompt)
				t.OutputTokens = add(t.OutputTokens, n.Completion)
				b.models[model] += n.Prompt + n.Completion
			}
		}
	}

	for _, p := range parts {
		switch name := p.tool(); {
		case p.Type == "text":
			if strings.TrimSpace(p.Text) != "" {
				b.answer = append(b.answer, p.Text)
			}
		case name != "":
			addTool(b, name, p, opts)
		}
	}
}

// addTool maps one tool part: SQL tools become statements, every other tool
// a tool call.
func addTool(b *building, name string, p part, opts agent.Options) {
	t := &b.turn
	so := structured(p.Output)
	if name == "run_query" {
		markRun(b, p)
	}
	if sqlTools[name] {
		sql := ""
		if so != nil {
			sql = str(so["query-content"])
		}
		if sql == "" {
			var in map[string]json.RawMessage
			_ = json.Unmarshal(p.Input, &in)
			switch name {
			case "create_sql_query":
				sql = str(in["sql_query"])
			case "replace_sql_query":
				sql = str(in["new_query"])
			}
		}
		if sql != "" {
			st := turns.Statement{SQL: sql}
			switch p.State {
			case "output-error":
				st.Status, st.Error = "failed", p.ErrorText
			case "output-denied":
				st.Status = "not_executed"
			}
			t.Statements = append(t.Statements, st)
			if id := str(so["query-id"]); id != "" {
				if b.queries == nil {
					b.queries = map[string]int{}
				}
				b.queries[id] = len(t.Statements) - 1
			}
			return
		}
		// edit_sql_query stores only a diff as input: keep it as a tool call.
	}

	tc := turns.ToolCall{Name: name, Status: toolStatus(p.State)}
	if present(p.Input) {
		tc.Input = maskSecrets(p.Input)
	}
	switch {
	case p.State == "output-error":
		tc.Output = map[string]string{"error": p.ErrorText}
	case !present(p.Output):
	case opts.IncludeResults || keptTools[name]:
		tc.Output = maskSecrets(p.Output)
	case so != nil:
		// The generated query (MBQL, SQL, query id, chart type) is not data.
		tc.Output = map[string]any{"structured_output": so, "redacted": "tool output text removed by benchouse-cli"}
	default:
		tc.Output = map[string]string{"redacted": "tool output removed by benchouse-cli"}
	}
	t.ToolCalls = append(t.ToolCalls, tc)
}

// runResult reads the head of run_query's output text,
// <query_results query_id="..." returned="3" truncated="false">.
var runResult = regexp.MustCompile(`<query_results query_id="([^"]+)" returned="(\d+)"`)

// markRun records on the statement a run_query ran that it was executed:
// Metabot creates a query without running it, and runs it with run_query
// (by query id) when the answer needs its values. Seen in Metabase
// v0.65.2.3's own rows. Only the row count is read from the output.
func markRun(b *building, p part) {
	var in struct {
		QueryID string `json:"query_id"`
	}
	_ = json.Unmarshal(p.Input, &in)
	i, ok := b.queries[in.QueryID]
	if !ok {
		return
	}
	st := &b.turn.Statements[i]
	switch p.State {
	case "output-error":
		st.Status, st.Error = "failed", p.ErrorText
		return
	case "output-available":
	default:
		return
	}
	var out struct {
		Output string `json:"output"`
	}
	_ = json.Unmarshal(p.Output, &out)
	m := runResult.FindStringSubmatch(out.Output)
	if m == nil || m[1] != in.QueryID {
		return // e.g. "No query with id ..."
	}
	st.Status = "succeeded"
	if n, err := strconv.ParseInt(m[2], 10, 64); err == nil {
		st.Rows = &n
	}
}

// finish fills what depends on all of a turn's messages.
func finish(b *building, feedback map[string][]any) turns.Turn {
	t := b.turn
	if len(b.answer) > 0 && !b.sawError {
		t.Answer = turns.Ptr(strings.Join(b.answer, "\n\n"))
	}
	switch {
	case b.sawError:
		t.Outcome = "failed"
	case b.assistants == 0:
		// no answer recorded: leave the outcome unknown
	case b.finished == nil: // a Metabase from before the finished column
		if t.Answer != nil {
			t.Outcome = "completed"
		}
	case string(b.finished) == "true":
		t.Outcome = "completed"
	case string(b.finished) == "false":
		t.Outcome = "aborted" // the client stopped the stream
	default:
		t.Outcome = "running"
	}
	// The model that used the most tokens answered; others are listed.
	var models []string
	for m := range b.models {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool {
		if b.models[models[i]] != b.models[models[j]] {
			return b.models[models[i]] > b.models[models[j]]
		}
		return models[i] < models[j]
	})
	if len(models) > 0 {
		t.ModelName = models[0]
	}
	if len(models) > 1 {
		t.Extra["models"] = models
	}
	var fb []any
	for _, id := range b.messageIDs {
		fb = append(fb, feedback[id]...)
	}
	if len(fb) > 0 {
		t.Extra["feedback"] = fb
	}
	if len(t.Extra) == 0 {
		t.Extra = nil
	}
	return t
}

func toolStatus(state string) string {
	switch state {
	case "output-available":
		return "succeeded"
	case "output-error":
		return "failed"
	case "output-denied":
		return "denied"
	}
	return state
}

func add(p *int64, n int64) *int64 {
	if p == nil {
		return &n
	}
	v := *p + n
	return &v
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// maskSecrets masks credentials recognisable by their form or their key
// (tokens, keys, "password": "...") in a kept tool input or output: what
// the model passed to a tool is collected, a credential in it is not. When
// masking the JSON text whole would break it, each string is masked alone.
func maskSecrets(raw json.RawMessage) json.RawMessage {
	if s, n := files.MaskTokens(string(raw)); n == 0 {
		return raw
	} else if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return json.RawMessage(`{"redacted":"unreadable tool payload removed by benchouse-cli"}`)
	}
	b, _ := json.Marshal(maskStrings(v))
	return b
}

func maskStrings(v any) any {
	switch t := v.(type) {
	case string:
		s, _ := files.MaskTokens(t)
		return s
	case map[string]any:
		for k, c := range t {
			t[k] = maskStrings(c)
		}
	case []any:
		for i, c := range t {
			t[i] = maskStrings(c)
		}
	}
	return v
}
