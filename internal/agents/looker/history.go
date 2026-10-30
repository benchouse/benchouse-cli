package looker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/turns"
)

// The History explore logs every query Looker ran in the last 90 days, with
// the API client that issued it. A query Conversational Analytics ran is
// tied to no question there, so it is attached to a question by user and
// time (see matcher).

const (
	// matchWindow is how long after a question a query of the same user is
	// read as the one answering it, unless the next question of that user
	// comes sooner. A turn whose latency is known gets latency + matchSlack
	// instead, up to maxMatchWindow.
	matchWindow    = 2 * time.Minute
	matchSlack     = 30 * time.Second
	maxMatchWindow = 10 * time.Minute
	// matchSkew accepts a query logged a little before the question, for
	// clocks and second-rounded timestamps.
	matchSkew = 5 * time.Second
	// maxHistory bounds the History rows read in one run.
	maxHistory = 50000
	// maxStatementsPerTurn bounds the SQL fetched for one question.
	maxStatementsPerTurn = 20
	// queryDefFields are the parts of a query definition kept as the
	// generated Looker query: what was asked of the model, no results.
	queryDefFields = "model,view,fields,pivots,filters,filter_expression,sorts,limit,dynamic_fields"
)

const (
	sqlPath = "/queries/%s/run/sql"
	defPath = "/queries/%s?fields=" + queryDefFields
)

// aiQuery is one History row of a query Conversational Analytics issued.
type aiQuery struct {
	QueryID, User, Client, Source, Status, Model, View string
	At                                                 time.Time
	used                                               bool
}

// matcher holds the AI-issued queries of the window by user, oldest first.
type matcher struct {
	byUser  map[string][]*aiQuery
	total   int
	matched int
}

func newMatcher() *matcher { return &matcher{byUser: map[string][]*aiQuery{}} }

func (m *matcher) add(q *aiQuery) {
	m.byUser[q.User] = append(m.byUser[q.User], q)
	m.total++
}

func (m *matcher) sort() {
	for _, qs := range m.byUser {
		sort.SliceStable(qs, func(i, j int) bool { return qs[i].At.Before(qs[j].At) })
	}
}

// users returns the users with AI queries.
func (m *matcher) users() []string {
	var out []string
	for u := range m.byUser {
		if u != "" {
			out = append(out, u)
		}
	}
	sort.Strings(out)
	return out
}

// take claims the unclaimed queries of user logged in
// [asked-matchSkew, asked+window), and before until when until is set: the
// next question of the same user starts its own window.
func (m *matcher) take(user string, asked, until time.Time, latency time.Duration) []*aiQuery {
	if m == nil || user == "" || asked.IsZero() {
		return nil
	}
	w := matchWindow
	if latency > 0 {
		w = max(w, min(latency+matchSlack, maxMatchWindow))
	}
	end := asked.Add(w)
	if !until.IsZero() && until.Before(end) {
		end = until
	}
	qs := m.byUser[user]
	i := sort.Search(len(qs), func(i int) bool { return !qs[i].At.Before(asked.Add(-matchSkew)) })
	var out []*aiQuery
	for ; i < len(qs) && qs[i].At.Before(end) && len(out) < maxStatementsPerTurn; i++ {
		if !qs[i].used {
			qs[i].used = true
			out = append(out, qs[i])
		}
	}
	m.matched += len(out)
	return out
}

func (m *matcher) unmatched() int {
	if m == nil {
		return 0
	}
	return m.total - m.matched
}

// historyQuery is the History read: AI-issued queries in the window,
// newest first.
func historyQuery(h histFields, since time.Time, limit int) inlineQuery {
	f := map[string]string{h.APIClient: caClientNames}
	if w := window(since, time.Time{}); w != "" {
		f[h.Time] = w
	}
	return inlineQuery{Model: saModel, View: historyExplore, Fields: h.fields(), Filters: f,
		Sorts: []string{h.Time + " desc"}, Limit: fmt.Sprint(limit), QueryTimezone: "UTC"}
}

// historyLimit bounds the History read in proportion to the cap.
func historyLimit(maxConv int) int {
	if maxConv <= 0 {
		return maxHistory
	}
	return min(maxHistory, max(1000, maxConv*25))
}

// readHistory runs the History query and files its rows in a matcher.
func readHistory(ctx context.Context, c *client, h histFields, since time.Time, limit int) (*matcher, inlineQuery, int, error) {
	q := historyQuery(h, since, limit)
	rows, err := c.run(ctx, "", q)
	if err != nil {
		return nil, q, 0, err
	}
	m := newMatcher()
	for _, r := range rows {
		at, ok := parseTime(r[h.Time])
		id := str(r[h.QueryID])
		if !ok || id == "" {
			continue
		}
		m.add(&aiQuery{QueryID: id, User: str(r[h.User]), Client: str(r[h.APIClient]), Source: str(r[h.Source]),
			Status: str(r[h.Status]), Model: str(r[h.Model]), View: str(r[h.View]), At: at})
	}
	m.sort()
	return m, q, len(rows), nil
}

// fetched is what was read for one AI query: its SQL and definition.
type fetched struct {
	stmt turns.Statement
	call *turns.ToolCall
}

// fetchQuery reads one query's SQL (result_format=sql renders the SQL
// without running it) and its definition. Neither returns result rows.
func fetchQuery(ctx context.Context, c *client, token string, q *aiQuery, tally func(purpose, stmt string, rows int)) (fetched, error) {
	id, err := pathID("query", q.QueryID)
	if err != nil {
		return fetched{}, err
	}
	var f fetched
	sql, err := c.getText(ctx, token, fmt.Sprintf(sqlPath, id))
	if err != nil {
		return f, err
	}
	tally(purposeSQL, "GET "+apiPrefix+fmt.Sprintf(sqlPath, "<query id>"), 1)
	at := q.At
	f.stmt = turns.Statement{SQL: strings.TrimSpace(sql), StartedAt: &at, Status: statementStatus(q.Status)}
	var def map[string]any
	if err := c.getJSON(ctx, token, fmt.Sprintf(defPath, id), &def); err == nil {
		tally(purposeDef, "GET "+apiPrefix+fmt.Sprintf(defPath, "<query id>"), 1)
		delete(def, "can")
		f.call = &turns.ToolCall{Name: "looker_query", Input: def, Status: f.stmt.Status, StartedAt: &at}
	}
	return f, nil
}

// statementStatus maps History's status (complete, error, killed, ...).
func statementStatus(s string) string {
	switch s = strings.ToLower(s); s {
	case "complete", "cache_only_miss":
		return "succeeded"
	case "error", "killed":
		return "failed"
	}
	return s
}

// attach adds the fetched queries to t: SQL as statements, the definition
// as a looker_query tool call, the History row as extra.history.
func attach(t *turns.Turn, fs []fetched, qs []*aiQuery) {
	var hist []map[string]any
	for i, f := range fs {
		if f.stmt.SQL != "" {
			t.Statements = append(t.Statements, f.stmt)
		}
		if f.call != nil {
			t.ToolCalls = append(t.ToolCalls, *f.call)
		}
		q := qs[i]
		h := map[string]any{"query_id": q.QueryID, "at": q.At.Format(time.RFC3339), "api_client": q.Client}
		if q.Source != "" {
			h["source"] = q.Source
		}
		if q.Model != "" {
			h["explore"] = q.Model + "/" + q.View
		}
		hist = append(hist, h)
	}
	if len(hist) > 0 {
		if t.Extra == nil {
			t.Extra = map[string]any{}
		}
		t.Extra["history"] = hist
		t.Extra["statements_matched_by"] = "user and time (System Activity History)"
	}
}
