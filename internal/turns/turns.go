// Package turns builds records in Benchouse's turns/v1 contract: one row per
// question and answer, with the SQL the agent ran, its reasoning and its tool
// calls as arrays on the same row.
//
// It is the common form for agents without a dedicated Benchouse connector.
// An agent reader maps its own storage onto Turn; Record produces the same
// payload a live custom-SQL connection stores, so the backend analyses it
// with the same derivation. Contract reference:
// https://github.com/benchouse/benchouse-cli/blob/main/docs/turns-v1.md
package turns

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/bundle"
)

// Contract is the version stamped on every row.
const Contract = "turns/v1"

// SourceKind is the backend source kind for turns/v1 rows.
const SourceKind = "custom_sql"

// RecordKind is the backend record kind for turns/v1 rows.
const RecordKind = "sql_turn"

// Turn is one question and the agent's answer.
//
// Pointers and nil slices mean "not recorded for this turn"; which columns an
// agent records at all is declared separately (see Columns), because "the
// agent never records this" and "this turn has none" read differently.
type Turn struct {
	TurnID       string
	AskedAt      time.Time
	Question     string
	Answer       *string
	UpdatedAt    *time.Time
	CompletedAt  *time.Time
	Outcome      string // completed, failed, running, or the agent's own word
	ErrorType    string
	ErrorMessage string

	ConversationID string
	Position       *int64

	UserID, UserName, UserEmail, UserRole string
	ClientInterface                       string
	SessionID                             string
	ModelName                             string
	InputTokens, OutputTokens             *int64
	SemanticModelRef                      string

	Statements []Statement
	Reasoning  []Reasoning
	Messages   []Message
	ToolCalls  []ToolCall
	// Extra holds anything else. Two keys are read by Benchouse: "declined"
	// (bool, the agent says it declined to answer) and "missing_concept"
	// (text, what it said it was missing).
	Extra map[string]any
}

// Statement is one SQL statement the agent ran or proposed.
type Statement struct {
	SQL       string     `json:"sql"`
	Status    string     `json:"status,omitempty"` // succeeded, failed, not_executed
	Error     string     `json:"error,omitempty"`
	Rows      *int64     `json:"rows,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	IsAnswer  *bool      `json:"is_answer,omitempty"`
	// Result is a sample of what the statement returned. Readers leave it
	// empty unless the user passed --include-results.
	Result any `json:"result,omitempty"`
}

// Reasoning is one reasoning step. Label is restatement, assumption,
// data_sourcing, plan, deliberation, or the agent's own word.
type Reasoning struct {
	Text  string `json:"text"`
	Label string `json:"label,omitempty"`
}

// Message is anything else the agent said. Kind is clarifying_question,
// suggested_questions, visualization, restatement, or the agent's own word.
type Message struct {
	Text string `json:"text"`
	Kind string `json:"kind,omitempty"`
}

// ToolCall is one non-SQL tool the agent called.
type ToolCall struct {
	Name      string     `json:"name"`
	Input     any        `json:"input,omitempty"`
	Output    any        `json:"output,omitempty"`
	Status    string     `json:"status,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Column names, in contract order.
const (
	ColTurnID           = "turn_id"
	ColAskedAt          = "asked_at"
	ColQuestion         = "question_text"
	ColAnswer           = "answer_text"
	ColUpdatedAt        = "updated_at"
	ColCompletedAt      = "completed_at"
	ColOutcome          = "outcome"
	ColErrorType        = "error_type"
	ColErrorMessage     = "error_message"
	ColConversationID   = "conversation_id"
	ColPosition         = "position"
	ColUserID           = "user_id"
	ColUserName         = "user_name"
	ColUserEmail        = "user_email"
	ColUserRole         = "user_role"
	ColClientInterface  = "client_interface"
	ColSessionID        = "session_id"
	ColModelName        = "model_name"
	ColInputTokens      = "input_tokens"
	ColOutputTokens     = "output_tokens"
	ColSemanticModelRef = "semantic_model_ref"
	ColStatements       = "statements"
	ColReasoning        = "reasoning"
	ColMessages         = "messages"
	ColToolCalls        = "tool_calls"
	ColExtra            = "extra"
)

// Required are the columns every turn has.
var Required = []string{ColTurnID, ColAskedAt, ColQuestion, ColAnswer}

var order = []string{
	ColTurnID, ColAskedAt, ColQuestion, ColAnswer, ColUpdatedAt, ColCompletedAt, ColOutcome,
	ColErrorType, ColErrorMessage, ColConversationID, ColPosition, ColUserID, ColUserName,
	ColUserEmail, ColUserRole, ColClientInterface, ColSessionID, ColModelName, ColInputTokens,
	ColOutputTokens, ColSemanticModelRef, ColStatements, ColReasoning, ColMessages, ColToolCalls, ColExtra,
}

// Columns is the set of columns an agent records. It always includes Required.
type Columns []string

// NewColumns returns Required plus the named optional columns.
func NewColumns(optional ...string) Columns {
	seen := map[string]bool{}
	var out Columns
	for _, c := range append(append([]string{}, Required...), optional...) {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	rank := map[string]int{}
	for i, c := range order {
		rank[c] = i
	}
	sort.Slice(out, func(i, j int) bool { return rank[out[i]] < rank[out[j]] })
	return out
}

// Record renders t as a turns/v1 bundle record. cols says which columns the
// agent records; a value for a column not in cols is dropped, and a column in
// cols with no value is written as null.
func Record(t Turn, cols Columns, fetchMethod string) (bundle.Record, error) {
	if strings.TrimSpace(t.TurnID) == "" {
		return bundle.Record{}, fmt.Errorf("turn without an id")
	}
	if t.AskedAt.IsZero() {
		return bundle.Record{}, fmt.Errorf("turn %s: no asked_at", t.TurnID)
	}
	if strings.TrimSpace(t.Question) == "" {
		return bundle.Record{}, fmt.Errorf("turn %s: no question", t.TurnID)
	}
	all := map[string]any{
		ColTurnID:           t.TurnID,
		ColAskedAt:          ts(&t.AskedAt),
		ColQuestion:         t.Question,
		ColAnswer:           strp(t.Answer),
		ColUpdatedAt:        ts(t.UpdatedAt),
		ColCompletedAt:      ts(t.CompletedAt),
		ColOutcome:          str(t.Outcome),
		ColErrorType:        str(t.ErrorType),
		ColErrorMessage:     str(t.ErrorMessage),
		ColConversationID:   str(t.ConversationID),
		ColPosition:         intp(t.Position),
		ColUserID:           str(t.UserID),
		ColUserName:         str(t.UserName),
		ColUserEmail:        str(t.UserEmail),
		ColUserRole:         str(t.UserRole),
		ColClientInterface:  str(t.ClientInterface),
		ColSessionID:        str(t.SessionID),
		ColModelName:        str(t.ModelName),
		ColInputTokens:      intp(t.InputTokens),
		ColOutputTokens:     intp(t.OutputTokens),
		ColSemanticModelRef: str(t.SemanticModelRef),
		ColStatements:       nonEmpty(t.Statements, func(s Statement) string { return s.SQL }),
		ColReasoning:        nonEmpty(t.Reasoning, func(x Reasoning) string { return x.Text }),
		ColMessages:         nonEmpty(t.Messages, func(x Message) string { return x.Text }),
		ColToolCalls:        nonEmpty(t.ToolCalls, func(x ToolCall) string { return x.Name }),
		ColExtra:            extra(t.Extra),
	}
	row := make(map[string]any, len(cols))
	for _, c := range cols {
		row[c] = all[c]
	}
	rowJSON, err := json.Marshal(row) // encoding/json sorts keys: canonical
	if err != nil {
		return bundle.Record{}, err
	}
	payload, err := json.Marshal(struct {
		Contract string          `json:"contract"`
		Columns  []string        `json:"columns"`
		Row      json.RawMessage `json:"row"`
	}{Contract, cols, rowJSON})
	if err != nil {
		return bundle.Record{}, err
	}
	ids := map[string]string{"turn_id": t.TurnID}
	if t.ConversationID != "" {
		ids["conversation_id"] = t.ConversationID
	}
	cursor := t.AskedAt
	if t.UpdatedAt != nil && contains(cols, ColUpdatedAt) {
		cursor = *t.UpdatedAt
	}
	return bundle.Record{SourceKind: SourceKind, RecordKind: RecordKind, NativeIDs: ids,
		FetchMethod: fetchMethod, CursorValue: cursor.UTC().Format(time.RFC3339Nano), Payload: payload}, nil
}

func ts(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func str(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strp(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func intp(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

func extra(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

// nonEmpty keeps elements whose key text is non-blank and always returns a
// non-nil slice: [] means "recorded, and there were none".
func nonEmpty[T any](xs []T, key func(T) string) []T {
	out := make([]T, 0, len(xs))
	for _, x := range xs {
		if strings.TrimSpace(key(x)) != "" {
			out = append(out, x)
		}
	}
	return out
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// Ptr returns a pointer to v, for optional Turn fields.
func Ptr[T any](v T) *T { return &v }
