package looker

import (
	"container/heap"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/turns"
)

// --per-user reads each user's own Conversational Analytics conversations:
// search_conversations only returns the conversations of the user the token
// belongs to, so the API client logs in as each user (login_user, admin
// only) and logs out again. Two passes: the first lists every user's
// conversations (ids and times only) to choose the most recent across
// users; the second reads the chosen ones' messages.

const (
	convPageSize = 100
	// maxConvPages bounds one user's conversation listing: 5,000.
	maxConvPages = 50
	// maxUsers bounds the users read.
	maxUsers = 5000
	// maxListed bounds the conversations held when there is no cap.
	maxListed  = 100000
	convFields = "id,user_id,created_at,updated_at"
	// enrichWindow is how far apart a System Activity row and a message of
	// the same question may be to be read as the same turn.
	enrichWindow = 10 * time.Minute
)

func searchPath(offset int) string {
	v := url.Values{"fields": {convFields}, "sorts": {"updated_at desc"}, "limit": {strconv.Itoa(convPageSize)},
		"offset": {strconv.Itoa(offset)}, "deleted": {"false"}}
	return "/conversations/search?" + v.Encode()
}

const messagesPath = "/conversations/%s/messages"

type convMeta struct {
	ID, User         string
	Created, Updated time.Time
}

// convHeap keeps the most recent conversations: a min-heap on Updated.
type convHeap []convMeta

func (h convHeap) Len() int           { return len(h) }
func (h convHeap) Less(i, j int) bool { return h[i].Updated.Before(h[j].Updated) }
func (h convHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *convHeap) Push(x any)        { *h = append(*h, x.(convMeta)) }
func (h *convHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// asUser runs fn with a sudo session for user, always logging it out.
func (r *run) asUser(ctx context.Context, user string, fn func(token string) error) error {
	tok, err := r.c.sudo(ctx, user)
	if err != nil {
		return err
	}
	r.res.Tally(purposeSudo, sudoStatement, 1)
	defer func() {
		if err := r.c.logout(ctx, tok); err != nil {
			r.logoutFailed++
		} else {
			r.res.Tally(purposeSudoLogout, sudoLogoutStatement, 1)
		}
	}()
	return fn(tok)
}

// errSudoRefused says the API client may not log in as other users.
type errSudoRefused struct{ status int }

func (e errSudoRefused) Error() string {
	return fmt.Sprintf("Looker refused to log in as a user (HTTP %d): --per-user needs an API client of an admin (the sudo permission)", e.status)
}

// listConversations is the first pass. It returns the chosen conversations,
// the users whose conversations were read, and errSudoRefused when the very
// first login as a user is refused (the client cannot sudo at all).
func (r *run) listConversations(ctx context.Context, users []string) ([]convMeta, map[string]bool, error) {
	cap := r.opts.MaxConversations
	h := &convHeap{}
	readable := map[string]bool{}
	skipped, listedCapped := 0, false
	for _, u := range users {
		err := r.asUser(ctx, u, func(tok string) error {
			n := 0
			for page := 0; page < maxConvPages; page++ {
				var cs []struct {
					ID        any    `json:"id"`
					UserID    any    `json:"user_id"`
					CreatedAt string `json:"created_at"`
					UpdatedAt string `json:"updated_at"`
				}
				if err := r.c.getJSON(ctx, tok, searchPath(page*convPageSize), &cs); err != nil {
					return err
				}
				r.res.Tally(purposeConvs, convsStatement, len(cs))
				older := false
				for _, c := range cs {
					m := convMeta{ID: str(c.ID), User: u}
					m.Created, _ = parseTime(c.CreatedAt)
					m.Updated, _ = parseTime(c.UpdatedAt)
					if m.Updated.IsZero() {
						m.Updated = m.Created
					}
					if m.ID == "" {
						continue
					}
					if !r.opts.Since.IsZero() && m.Updated.Before(r.opts.Since) {
						older = true
						continue
					}
					n++
					switch {
					case cap > 0:
						heap.Push(h, m)
						if h.Len() > cap {
							heap.Pop(h)
						}
					case h.Len() < maxListed:
						heap.Push(h, m)
					default:
						listedCapped = true
					}
				}
				if len(cs) < convPageSize || older || (cap > 0 && n >= cap) {
					break
				}
			}
			return nil
		})
		if err != nil {
			if s := statusOf(err); (s == http.StatusForbidden || s == http.StatusUnauthorized) && len(readable) == 0 && skipped == 0 {
				return nil, nil, errSudoRefused{status: s}
			}
			skipped++
			if r.sudoErr == "" {
				r.sudoErr = clip(err.Error(), 200)
			}
			continue
		}
		readable[u] = true
	}
	if skipped > 0 {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("--per-user: could not read the conversations of %d users (%s); their System Activity questions are kept without answers",
			skipped, r.sudoErr))
	}
	if listedCapped {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("--per-user: held the %d most recent conversations listed; set --max-conversations or narrow with --since", maxListed))
	}
	out := make([]convMeta, h.Len())
	copy(out, *h)
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, readable, nil
}

// enrichIndex holds the System Activity facts of the per-user users'
// questions, keyed by user and question, so a message read as that user gets
// its rating, success and latency.
type enrichIndex map[string][]caRow

func enrichKey(user, q string) string {
	return user + "\x00" + strings.Join(strings.Fields(strings.ToLower(q)), " ")
}

func (e enrichIndex) add(c caRow) {
	k := enrichKey(c.User, c.Question)
	c.Question = "" // the message holds it
	e[k] = append(e[k], c)
}

// take returns the row of user's question q nearest asked (same
// conversation first), removing it.
func (e enrichIndex) take(user, conv, q string, asked time.Time) (caRow, bool) {
	k := enrichKey(user, q)
	rows := e[k]
	best := -1
	for i, c := range rows {
		if conv != "" && c.Conv == conv {
			best = i
			break
		}
		if d := absDur(c.At.Sub(asked)); d <= enrichWindow && (best < 0 || d < absDur(rows[best].At.Sub(asked))) {
			best = i
		}
	}
	if best < 0 {
		return caRow{}, false
	}
	c := rows[best]
	e[k] = append(rows[:best:best], rows[best+1:]...)
	return c, true
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// readConversations is the second pass: the chosen conversations' messages,
// one user at a time, newest user first.
func (r *run) readConversations(ctx context.Context, chosen []convMeta, idx enrichIndex) error {
	byUser := map[string][]convMeta{}
	var order []string
	for _, c := range chosen {
		if _, ok := byUser[c.User]; !ok {
			order = append(order, c.User)
		}
		byUser[c.User] = append(byUser[c.User], c)
	}
	failed := 0
	for _, u := range order {
		err := r.asUser(ctx, u, func(tok string) error {
			for _, conv := range byUser[u] {
				id, err := pathID("conversation", conv.ID)
				if err != nil {
					failed++
					continue
				}
				var msgs []map[string]any
				if err := r.c.getJSON(ctx, tok, fmt.Sprintf(messagesPath, id), &msgs); err != nil {
					failed++
					if r.convErr == "" {
						r.convErr = clip(err.Error(), 200)
					}
					continue
				}
				r.res.Tally(purposeMessages, messagesStatement, len(msgs))
				ts := messageTurns(conv, msgs, r.opts.IncludeResults)
				for i := range ts {
					t := &ts[i]
					if c, ok := idx.take(u, conv.ID, t.Question, t.AskedAt); ok {
						c.enrich(t)
						if r.opts.IncludeUsers {
							t.UserName, t.UserEmail = c.UserName, c.UserEmail
						}
					}
					if len(t.Statements) == 0 && t.Extra["asked_at_estimated"] == nil {
						var until time.Time
						if i+1 < len(ts) {
							until = ts[i+1].AskedAt
						}
						r.attachHistory(ctx, t, tok, u, t.AskedAt, until, 0)
					}
					if err := r.emit(*t); err != nil {
						return err
					}
				}
				r.convs++
			}
			return nil
		})
		if err != nil {
			if r.stopErr(err) {
				return err
			}
			failed += len(byUser[u])
			if r.convErr == "" {
				r.convErr = clip(err.Error(), 200)
			}
		}
	}
	if failed > 0 {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("--per-user: could not read %d conversations (%s)", failed, r.convErr))
	}
	return nil
}

// messageTurns maps one conversation's messages onto turns: a message with
// a userMessage opens a turn, the systemMessages after it are its answer.
//
// ConversationMessage.message is untyped in the API spec; it is read as the
// spec's ChatMessage (timestamp, messageId, userMessage.text, systemMessage
// {text, data, analysis, chart, error, schema}), the shape
// conversational_analytics_chat returns. snake_case spellings are accepted
// too.
func messageTurns(conv convMeta, msgs []map[string]any, includeResults bool) []turns.Turn {
	sort.SliceStable(msgs, func(i, j int) bool {
		a, _ := number(msgs[i]["order"])
		b, _ := number(msgs[j]["order"])
		return a < b
	})
	var out []turns.Turn
	var cur *turns.Turn
	var answer []string
	var errs []string
	flush := func() {
		if cur == nil {
			return
		}
		if len(answer) > 0 {
			a := strings.Join(answer, "\n\n")
			cur.Answer = &a
			cur.Outcome = "completed"
		}
		if len(errs) > 0 {
			cur.ErrorMessage = strings.Join(errs, "; ")
			if cur.Answer == nil {
				cur.Outcome = "failed"
			}
		}
		out = append(out, *cur)
		cur, answer, errs = nil, nil, nil
	}
	pos := int64(0)
	for _, msg := range msgs {
		cm := obj(msg["message"])
		if cm == nil {
			continue
		}
		ts, hasTS := parseTime(cm["timestamp"])
		if um := obj(get(cm, "userMessage", "user_message")); um != nil || strings.EqualFold(str(msg["type"]), "user") {
			q := str(get(um, "text"))
			if q == "" {
				continue
			}
			flush()
			pos++
			p := pos
			id := str(msg["id"])
			if id == "" {
				id = str(get(cm, "messageId", "message_id"))
			}
			t := turns.Turn{TurnID: id, Question: q, ConversationID: conv.ID, Position: &p, UserID: conv.User,
				AskedAt: ts, Extra: map[string]any{"source": "conversations_api"}}
			if !hasTS {
				t.AskedAt = conv.Created
				t.Extra["asked_at_estimated"] = "conversation created_at"
			}
			cur = &t
			continue
		}
		sm := obj(get(cm, "systemMessage", "system_message"))
		if cur == nil || sm == nil {
			continue
		}
		if hasTS {
			done := ts
			cur.CompletedAt = &done
		}
		readSystem(cur, sm, &answer, &errs, includeResults)
	}
	flush()
	return out
}

// readSystem files one system message's parts on t.
func readSystem(t *turns.Turn, sm map[string]any, answer, errs *[]string, includeResults bool) {
	if tx := obj(get(sm, "text")); tx != nil {
		parts := texts(get(tx, "parts"))
		switch kind := strings.ToUpper(str(get(tx, "textType", "text_type"))); {
		case strings.Contains(kind, "THOUGHT"), strings.Contains(kind, "PROGRESS"):
			for _, p := range parts {
				t.Reasoning = append(t.Reasoning, turns.Reasoning{Text: p, Label: "deliberation"})
			}
		default:
			*answer = append(*answer, parts...)
		}
	}
	if d := obj(get(sm, "data")); d != nil {
		sql := str(get(d, "generatedSql", "generated_sql"))
		lq := obj(get(d, "generatedLookerQuery", "generated_looker_query"))
		if lq == nil {
			lq = obj(get(obj(get(d, "query")), "looker"))
		}
		result := obj(get(d, "result"))
		rows, hasRows := get(result, "data").([]any)
		if sql != "" {
			s := turns.Statement{SQL: sql, Status: "succeeded"}
			if hasRows {
				n := int64(len(rows))
				s.Rows = &n
				if includeResults {
					s.Result = rows
				}
			}
			t.Statements = append(t.Statements, s)
		}
		if lq != nil {
			delete(lq, "can")
			call := turns.ToolCall{Name: "looker_query", Input: lq}
			if includeResults && hasRows && sql == "" {
				call.Output = rows
			}
			t.ToolCalls = append(t.ToolCalls, call)
		}
		if q := str(get(obj(get(d, "query")), "question")); q != "" {
			t.Reasoning = append(t.Reasoning, turns.Reasoning{Text: q, Label: "restatement"})
		}
	}
	if a := obj(get(sm, "analysis")); a != nil {
		if ev := obj(get(a, "progressEvent", "progress_event")); ev != nil {
			if p := str(get(ev, "plannerReasoning", "planner_reasoning")); p != "" {
				t.Reasoning = append(t.Reasoning, turns.Reasoning{Text: p, Label: "plan"})
			}
			if code := str(get(ev, "code")); code != "" {
				call := turns.ToolCall{Name: "python", Input: code, Status: "succeeded"}
				if e := str(get(ev, "executionError", "execution_error")); e != "" {
					call.Status = "failed"
				}
				if includeResults {
					call.Output = get(ev, "executionOutput", "execution_output")
				}
				t.ToolCalls = append(t.ToolCalls, call)
			}
			if nl := str(get(ev, "resultNaturalLanguage", "result_natural_language")); nl != "" {
				*answer = append(*answer, nl)
			}
		}
	}
	if c := obj(get(sm, "chart")); c != nil {
		text := str(get(obj(get(c, "query")), "instructions"))
		if text == "" {
			text = "chart"
		}
		t.Messages = append(t.Messages, turns.Message{Text: text, Kind: "visualization"})
	}
	if e := obj(get(sm, "error")); e != nil {
		if s := str(get(e, "text")); s != "" {
			*errs = append(*errs, s)
		}
	}
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// get returns the first of keys present in m.
func get(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func texts(v any) []string {
	var out []string
	switch x := v.(type) {
	case []any:
		for _, p := range x {
			if s := str(p); s != "" {
				if m, ok := p.(map[string]any); ok {
					s = str(get(m, "text"))
				}
				if s != "" {
					out = append(out, s)
				}
			}
		}
	case string:
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}
