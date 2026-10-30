package omni

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/turns"
)

var columns = turns.NewColumns(
	turns.ColCompletedAt, turns.ColOutcome, turns.ColErrorType, turns.ColErrorMessage, turns.ColConversationID,
	turns.ColPosition, turns.ColUserID, turns.ColModelName, turns.ColSemanticModelRef, turns.ColStatements,
	turns.ColReasoning, turns.ColToolCalls, turns.ColExtra,
)

// draft is one user message and the assistant messages after it.
type draft struct {
	position int
	question message
	replies  []message
	jobs     []jobRead
}

type jobRead struct {
	msg    message
	job    *job
	result *jobResult
}

type mapper struct {
	includeResults bool
	since          time.Time
	now            time.Time
	// counts for notes
	tooOld, expired, jobErrors, resultErrors, beforeSince, orphans int
}

func (m *mapper) notes() []string {
	var out []string
	if m.tooOld > 0 {
		out = append(out, fmt.Sprintf("%d answers are older than 14 days: Omni no longer keeps their job results, so they have no generated queries", m.tooOld))
	}
	if m.expired > 0 {
		out = append(out, fmt.Sprintf("%d job results were already gone (404), so those answers have no generated queries", m.expired))
	}
	if m.jobErrors > 0 {
		out = append(out, fmt.Sprintf("%d AI jobs could not be read: those answers have no model, outcome or queries", m.jobErrors))
	}
	if m.resultErrors > 0 {
		out = append(out, fmt.Sprintf("%d job results could not be read: those answers have no generated queries", m.resultErrors))
	}
	if m.beforeSince > 0 {
		out = append(out, fmt.Sprintf("left out %d earlier turns of conversations active in the window, asked before --since", m.beforeSince))
	}
	if m.orphans > 0 {
		out = append(out, fmt.Sprintf("left out %d assistant messages with no question before them", m.orphans))
	}
	return out
}

// drafts pairs a conversation's messages into turns, keeping those asked in
// the window. Positions count every turn of the conversation.
func (m *mapper) drafts(c conversation) []*draft {
	var out []*draft
	var cur *draft
	pos := 0
	for _, msg := range c.Messages {
		switch strings.ToLower(msg.Role) {
		case "user":
			pos++
			cur = &draft{position: pos, question: msg}
			at, ok := parseTime(msg.Timestamp)
			if ok && !m.since.IsZero() && at.Before(m.since) {
				m.beforeSince++
				cur.position = -1
				continue
			}
			out = append(out, cur)
		case "assistant":
			if cur == nil {
				m.orphans++
				continue
			}
			cur.replies = append(cur.replies, msg)
		}
	}
	keep := out[:0]
	for _, d := range out {
		if d.position > 0 {
			keep = append(keep, d)
		}
	}
	return keep
}

// turn maps one draft, with its jobs read, onto a turn.
func (m *mapper) turn(c conversation, d *draft) (turns.Turn, bool) {
	asked, ok := parseTime(d.question.Timestamp)
	if !ok {
		if asked, ok = parseTime(c.CreatedAt); !ok {
			return turns.Turn{}, false
		}
	}
	tr := turns.Turn{
		TurnID:         c.ID + ":" + itoa(d.position),
		AskedAt:        asked,
		Question:       d.question.Content,
		ConversationID: c.ID,
		Position:       turns.Ptr(int64(d.position)),
		UserID:         c.UserID,
		Statements:     []turns.Statement{},
		ToolCalls:      []turns.ToolCall{},
		Reasoning:      []turns.Reasoning{},
	}
	var answers, jobIDs, urls []string
	extra := map[string]any{}
	for _, r := range d.replies {
		if s := strings.TrimSpace(r.Content); s != "" {
			answers = append(answers, s)
		}
		if t, ok := parseTime(r.Timestamp); ok {
			tr.CompletedAt = turns.Ptr(t)
		}
		if r.JobID != "" {
			jobIDs = append(jobIDs, r.JobID)
		}
		if r.OmniChatURL != "" {
			urls = append(urls, r.OmniChatURL)
		}
	}
	unavailable := 0
	for _, jr := range d.jobs {
		j := jr.job
		if j == nil {
			continue
		}
		switch strings.ToUpper(j.State) {
		case "COMPLETE":
			tr.Outcome = "completed"
		case "FAILED":
			tr.Outcome = "failed"
		case "CANCELLED":
			tr.Outcome = "cancelled"
		default:
			if tr.Outcome == "" {
				tr.Outcome = "running"
			}
		}
		if j.Error != nil {
			tr.ErrorType = j.Error.Code
			tr.ErrorMessage = strings.TrimSpace(j.Error.Message + " " + j.Error.Detail)
		}
		if j.RequestedLlm != nil && j.RequestedLlm.Model != "" {
			tr.ModelName = j.RequestedLlm.Model
		}
		if j.ModelID != nil {
			tr.SemanticModelRef = *j.ModelID
		}
		if j.TopicName != nil && *j.TopicName != "" {
			extra["topic"] = *j.TopicName
		}
		if j.BranchID != nil && *j.BranchID != "" {
			extra["branch_id"] = *j.BranchID
		}
		if t, ok := parseTime(j.CompletedAt); ok {
			tr.CompletedAt = turns.Ptr(t)
		}
		if jr.result == nil {
			if strings.EqualFold(j.State, "COMPLETE") {
				unavailable++
			}
			continue
		}
		if jr.result.Topic != "" {
			extra["topic"] = jr.result.Topic
		}
		if len(answers) == 0 {
			if s := strings.TrimSpace(jr.result.Message); s != "" {
				answers = append(answers, s)
			}
		}
		m.actions(&tr, jr.result.Actions)
	}
	if len(answers) > 0 {
		a := strings.Join(answers, "\n\n")
		tr.Answer = &a
		if tr.Outcome == "" {
			tr.Outcome = "completed"
		}
	}
	if len(jobIDs) > 0 {
		extra["job_ids"] = jobIDs
	}
	if len(urls) > 0 {
		extra["omni_chat_url"] = urls[len(urls)-1]
	}
	if unavailable > 0 {
		extra["queries_unavailable"] = "job result no longer kept by Omni (14-day retention)"
	}
	if len(extra) > 0 {
		tr.Extra = extra
	}
	return tr, true
}

var looksSQL = regexp.MustCompile(`(?is)^\s*(with|select)\b`)

var sqlKeys = []string{"sql", "userEditedSQL", "user_edited_sql", "rawSql", "raw_sql", "generatedSql"}

// actions maps a job's actions: generated queries to statements (SQL) or
// tool calls (query JSON), every explanation to reasoning, other actions to
// tool calls by type.
func (m *mapper) actions(tr *turns.Turn, acts []action) {
	for _, a := range acts {
		var at *time.Time
		if t, ok := parseTime(a.Timestamp); ok {
			at = turns.Ptr(t)
		}
		typ := a.Type
		if typ == "" {
			typ = "action"
		}
		if s := strings.TrimSpace(a.Message); s != "" && typ != "summarize" && typ != "validate_analysis" {
			tr.Reasoning = append(tr.Reasoning, turns.Reasoning{Text: s, Label: typ})
		}
		if a.Result == nil {
			if typ != "summarize" {
				tr.ToolCalls = append(tr.ToolCalls, turns.ToolCall{Name: typ, Status: a.Status, StartedAt: at})
			}
			continue
		}
		r := a.Result
		status := ""
		switch str(r["status"]) {
		case "success":
			status = "succeeded"
		case "error":
			status = "failed"
		}
		rows, hasRows := integer(r["totalRowCount"])
		if sql := sqlOf(r["query"]); sql != "" {
			st := turns.Statement{SQL: sql, Status: status, StartedAt: at}
			if hasRows {
				st.Rows = turns.Ptr(rows)
			}
			if m.includeResults {
				st.Result = r["csvResult"]
			}
			tr.Statements = append(tr.Statements, st)
			continue
		}
		out := map[string]any{}
		for _, k := range []string{"queryName", "status", "totalRowCount", "hasResults", "csvResultWasTruncated", "resultId"} {
			if v, ok := r[k]; ok {
				out[k] = v
			}
		}
		if m.includeResults {
			out["csvResult"] = r["csvResult"]
		}
		tc := turns.ToolCall{Name: typ, Input: r["query"], Output: out, Status: status, StartedAt: at}
		if tc.Status == "" {
			tc.Status = a.Status
		}
		tr.ToolCalls = append(tr.ToolCalls, tc)
	}
}

// sqlOf returns SQL an Omni query JSON carries, if any.
func sqlOf(q any) string {
	switch v := q.(type) {
	case string:
		if looksSQL.MatchString(v) {
			return strings.TrimSpace(v)
		}
	case map[string]any:
		for _, k := range sqlKeys {
			if s, ok := v[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func integer(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}
