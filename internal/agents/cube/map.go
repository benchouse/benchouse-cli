package cube

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

// The export's shapes, as documented (ChatHistoryThread, ChatHistoryTurn,
// ChatHistoryQuery, ChatHistoryStep). Every field is optional here: an
// unexpected null or a missing key degrades a turn, never the run.

type thread struct {
	ChatID         string  `json:"chatId"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
	AgentID        *jsonID `json:"agentId"`
	Title          *string `json:"title"`
	InitialContext *string `json:"initialContext"`
	Source         *string `json:"source"`
	User           *user   `json:"user"`
	Turns          []turn  `json:"turns"`
}

type user struct {
	ID         jsonID  `json:"id"`
	Type       string  `json:"type"`
	Email      *string `json:"email"`
	Name       *string `json:"name"`
	ExternalID *string `json:"externalId"`
}

type turn struct {
	Index       *int64   `json:"index"`
	MessageID   string   `json:"messageId"`
	Prompt      string   `json:"prompt"`
	Files       []string `json:"files"`
	Tools       []string `json:"tools"`
	Queries     []query  `json:"queries"`
	Steps       []step   `json:"steps"`
	FinalAnswer *string  `json:"finalAnswer"`
	StartedAt   *string  `json:"startedAt"`
	EndedAt     *string  `json:"endedAt"`
}

type query struct {
	MessageID  string            `json:"messageId"`
	Tool       string            `json:"tool"`
	Status     string            `json:"status"`
	SQL        *string           `json:"sql"`
	DataSource *string           `json:"dataSource"`
	RowCount   *int64            `json:"rowCount"`
	Rows       []json.RawMessage `json:"rows"`
	Columns    []string          `json:"columns"`
	Error      *string           `json:"error"`
	Title      *string           `json:"title"`
	Feedback   *struct {
		IsPositive *bool   `json:"isPositive"`
		CreatedAt  string  `json:"createdAt"`
		UserID     *jsonID `json:"userId"`
	} `json:"feedback"`
}

type step struct {
	Type      string  `json:"type"`
	MessageID string  `json:"messageId"`
	Text      *string `json:"text"`
	Reasoning *string `json:"reasoning"`
	Tool      *string `json:"tool"`
	Input     *string `json:"input"`
	Status    *string `json:"status"`
	StartedAt *string `json:"startedAt"`
}

// columns are what Cube records. user_name and user_email are added with
// --include-users.
func columnsFor(opts agent.Options) turns.Columns {
	cols := []string{turns.ColCompletedAt, turns.ColOutcome, turns.ColErrorMessage, turns.ColConversationID,
		turns.ColPosition, turns.ColUserID, turns.ColUserRole, turns.ColClientInterface, turns.ColStatements,
		turns.ColReasoning, turns.ColMessages, turns.ColToolCalls, turns.ColExtra}
	if opts.IncludeUsers {
		cols = append(cols, turns.ColUserName, turns.ColUserEmail)
	}
	return turns.NewColumns(cols...)
}

// truncLimit is where Cube cuts a text, marking it with "…".
const truncLimit = 20000

type mapper struct {
	opts         agent.Options
	cols         turns.Columns
	emitted      int
	old          int // turns asked before --since
	noTime       int // turns timed by their thread
	skipped      []string
	truncated    int
	unreadable   int
	unreadableEx []string
}

func newMapper(opts agent.Options) *mapper { return &mapper{opts: opts, cols: columnsFor(opts)} }

func (m *mapper) notes() []string {
	var out []string
	if m.old > 0 {
		out = append(out, fmt.Sprintf("left out %d turns asked before --since in threads updated after it", m.old))
	}
	if m.noTime > 0 {
		out = append(out, fmt.Sprintf("%d turns had no startedAt and carry their thread's creation time", m.noTime))
	}
	if m.truncated > 0 {
		out = append(out, fmt.Sprintf("%d texts were cut by Cube's export at %s characters (marked …)", m.truncated, agent.Thousands(truncLimit)))
	}
	if len(m.skipped) > 0 {
		out = append(out, fmt.Sprintf("skipped %d turns: %s", len(m.skipped), strings.Join(m.skipped[:min(len(m.skipped), 5)], "; ")))
	}
	if m.unreadable > 0 {
		out = append(out, fmt.Sprintf("%d threads came without turns and could not be read: %s", m.unreadable, strings.Join(m.unreadableEx, "; ")))
	}
	return out
}

func (m *mapper) thread(th thread, emit agent.Emit) error {
	// Cube's turn index may count from 0 or 1; position counts from 1.
	offset := int64(0)
	for _, t := range th.Turns {
		if t.Index != nil && *t.Index == 0 {
			offset = 1
		}
	}
	for i, t := range th.Turns {
		tt, ok := m.turn(th, t, i, offset)
		if !ok {
			continue
		}
		rec, err := turns.Record(tt, m.cols, fetchMethod)
		if err != nil {
			m.skipped = append(m.skipped, err.Error())
			continue
		}
		m.emitted++
		if err := emit(rec); err != nil {
			return err
		}
	}
	return nil
}

func (m *mapper) turn(th thread, t turn, i int, offset int64) (turns.Turn, bool) {
	asked, ok := parseTime(t.StartedAt)
	if !ok {
		if asked, ok = parseTime(&th.CreatedAt); !ok {
			m.skipped = append(m.skipped, fmt.Sprintf("thread %s turn %d has no time", clip(th.ChatID, 40), i))
			return turns.Turn{}, false
		}
		m.noTime++
	}
	if !m.opts.Since.IsZero() && asked.Before(m.opts.Since) {
		m.old++
		return turns.Turn{}, false
	}
	id := t.MessageID
	if id == "" {
		id = strconv.Itoa(i)
	}
	pos := int64(i + 1)
	if t.Index != nil {
		pos = *t.Index + offset
	}
	out := turns.Turn{
		TurnID:         th.ChatID + ":" + id,
		AskedAt:        asked,
		Question:       m.text(t.Prompt),
		ConversationID: th.ChatID,
		Position:       &pos,
		Extra:          map[string]any{},
	}
	if t.FinalAnswer != nil {
		a := m.text(*t.FinalAnswer)
		out.Answer = &a
	}
	if end, ok := parseTime(t.EndedAt); ok {
		out.CompletedAt = &end
	}
	if th.Source != nil {
		out.ClientInterface = *th.Source
	}
	if u := th.User; u != nil {
		out.UserID = string(u.ID)
		out.UserRole = u.Type
		if m.opts.IncludeUsers {
			out.UserName = deref(u.Name)
			out.UserEmail = deref(u.Email)
			if x := deref(u.ExternalID); x != "" {
				out.Extra["external_user_id"] = x
			}
		}
	}

	var failed string
	var feedback, queries []map[string]any
	for _, q := range t.Queries {
		info := map[string]any{"tool": q.Tool, "status": q.Status}
		if q.MessageID != "" {
			info["message_id"] = q.MessageID
		}
		if s := deref(q.Title); s != "" {
			info["title"] = s
		}
		if s := deref(q.DataSource); s != "" {
			info["data_source"] = s
		}
		if len(q.Columns) > 0 {
			info["columns"] = q.Columns
		}
		queries = append(queries, info)
		if q.Status == "error" && failed == "" {
			failed = deref(q.Error)
			if failed == "" {
				failed = "a query failed"
			}
		}
		if fb := q.Feedback; fb != nil && fb.IsPositive != nil {
			f := map[string]any{"is_positive": *fb.IsPositive, "score": map[bool]int{true: 1, false: -1}[*fb.IsPositive]}
			if fb.CreatedAt != "" {
				f["created_at"] = fb.CreatedAt
			}
			if q.MessageID != "" {
				f["query_message_id"] = q.MessageID
			}
			if fb.UserID != nil && *fb.UserID != "" {
				f["user_id"] = string(*fb.UserID)
			}
			feedback = append(feedback, f)
		}
		sql := deref(q.SQL)
		if strings.TrimSpace(sql) == "" {
			continue
		}
		st := turns.Statement{SQL: m.text(sql), Status: statementStatus(q.Status), Error: deref(q.Error), Rows: q.RowCount}
		if m.opts.IncludeResults && q.Rows != nil {
			st.Result = map[string]any{"columns": q.Columns, "rows": q.Rows}
		}
		out.Statements = append(out.Statements, st)
	}
	if len(feedback) > 0 {
		out.Extra["feedback"] = feedback
	}
	if len(queries) > 0 {
		out.Extra["queries"] = queries
	}

	answer := ""
	if out.Answer != nil {
		answer = strings.TrimSpace(*out.Answer)
	}
	if t.Steps != nil {
		for _, s := range t.Steps {
			switch s.Type {
			case "model":
				if r := strings.TrimSpace(deref(s.Reasoning)); r != "" {
					out.Reasoning = append(out.Reasoning, turns.Reasoning{Text: m.text(r)})
				}
				if x := strings.TrimSpace(deref(s.Text)); x != "" && x != answer {
					out.Messages = append(out.Messages, turns.Message{Text: m.text(x), Kind: "model_text"})
				}
			case "tool":
				name := deref(s.Tool)
				if name == "" {
					name = "tool"
				}
				tc := turns.ToolCall{Name: name, Status: deref(s.Status)}
				if in := deref(s.Input); in != "" {
					var v any
					if json.Unmarshal([]byte(in), &v) == nil {
						tc.Input = v
					} else {
						tc.Input = m.text(in)
					}
				}
				if at, ok := parseTime(s.StartedAt); ok {
					tc.StartedAt = &at
				}
				if tc.Status == "error" && failed == "" {
					failed = "a tool call failed"
				}
				out.ToolCalls = append(out.ToolCalls, tc)
			}
		}
	} else {
		for _, name := range t.Tools {
			out.ToolCalls = append(out.ToolCalls, turns.ToolCall{Name: name})
		}
	}
	if len(t.Files) > 0 {
		out.Extra["files"] = t.Files
	}

	switch {
	case out.Answer != nil && answer != "":
		out.Outcome = "completed"
	case failed != "":
		out.Outcome = "failed"
		out.ErrorMessage = failed
	case out.CompletedAt == nil:
		out.Outcome = "running"
	}
	if th.AgentID != nil && *th.AgentID != "" {
		out.Extra["agent_id"] = string(*th.AgentID)
	}
	if s := deref(th.Title); s != "" {
		out.Extra["thread_title"] = s
	}
	if s := deref(th.InitialContext); s != "" {
		out.Extra["initial_context"] = s
	}
	return out, true
}

// text passes a text through, counting the ones Cube cut.
func (m *mapper) text(s string) string {
	if strings.HasSuffix(s, "…") && utf8.RuneCountInString(s) >= truncLimit {
		m.truncated++
	}
	return s
}

func statementStatus(s string) string {
	switch s {
	case "success":
		return "succeeded"
	case "error":
		return "failed"
	case "incomplete":
		return "not_executed"
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func parseTime(s *string) (time.Time, bool) {
	if s == nil || strings.TrimSpace(*s) == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999"} {
		if t, err := time.Parse(layout, strings.TrimSpace(*s)); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
