package looker

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/turns"
)

// caPageSize is how many Conversational Analytics rows one query reads; a
// var so a test can force page boundaries.
var caPageSize = 1000

const (
	// maxCAPages bounds the keyset walk: a million questions.
	maxCAPages = 1000
	// drainPages is how many pages are read after --max-conversations is
	// full, for older questions of the conversations already chosen.
	drainPages = 2
)

// caRow is one Conversational Analytics row: one question.
type caRow struct {
	TurnID, Conv, User, UserName, UserEmail, Question string
	At                                                time.Time
	Rating, Comment, Success                          string
	Latency                                           float64
	HasLatency                                        bool
	Agent, Model, Explore, Surface                    string
}

func (f caFields) row(r map[string]any) (caRow, bool) {
	at, ok := parseTime(r[f.Time])
	q := str(r[f.Question])
	if !ok || q == "" {
		return caRow{}, false
	}
	c := caRow{Question: q, At: at, User: str(r[f.User]), Conv: str(r[f.Conversation]), UserName: str(r[f.UserName]),
		UserEmail: str(r[f.UserEmail]), Rating: str(r[f.Rating]), Comment: str(r[f.Comment]), Success: str(r[f.Success]),
		Agent: str(r[f.Agent]), Model: str(r[f.Model]), Explore: str(r[f.ExploreF]), Surface: str(r[f.Surface])}
	if f.Latency != "" {
		c.Latency, c.HasLatency = number(r[f.Latency])
	}
	c.TurnID = str(r[f.Message])
	if c.TurnID == "" {
		h := fnv.New64a()
		fmt.Fprintf(h, "%s\x00%s\x00%s", c.User, at.Format(time.RFC3339), q)
		c.TurnID = fmt.Sprintf("sa-%016x", h.Sum64())
	}
	return c, true
}

// convKey is the conversation a row counts toward for --max-conversations:
// its conversation id, or the question itself when the explore has none.
func (c caRow) convKey() string {
	if c.Conv != "" {
		return "c:" + c.Conv
	}
	return "t:" + c.TurnID
}

func (c caRow) latency() time.Duration {
	if !c.HasLatency || c.Latency <= 0 {
		return 0
	}
	return time.Duration(c.Latency * float64(time.Second))
}

// turn maps a row onto a turn. The answer is not in System Activity.
func (c caRow) turn(includeUsers bool) turns.Turn {
	t := turns.Turn{TurnID: c.TurnID, AskedAt: c.At, Question: c.Question, ConversationID: c.Conv, UserID: c.User,
		Extra: map[string]any{"source": "system_activity"}}
	if includeUsers {
		t.UserName, t.UserEmail = c.UserName, c.UserEmail
	}
	c.enrich(&t)
	return t
}

// enrich sets what System Activity knows about a turn: outcome, latency,
// rating, agent and explore.
func (c caRow) enrich(t *turns.Turn) {
	if t.Extra == nil {
		t.Extra = map[string]any{}
	}
	switch s := strings.ToLower(c.Success); {
	case s == "":
	case strings.Contains(s, "success"):
		if t.Outcome == "" {
			t.Outcome = "completed"
		}
	case strings.Contains(s, "timeout"):
		t.Outcome, t.ErrorType = "failed", "timeout"
	case strings.Contains(s, "error") || strings.Contains(s, "fail"):
		t.Outcome, t.ErrorType = "failed", "error"
	default:
		if t.Outcome == "" {
			t.Outcome = s
		}
	}
	if c.Success != "" {
		t.Extra["answer_success"] = c.Success
	}
	if d := c.latency(); d > 0 {
		t.Extra["latency_seconds"] = c.Latency
		if t.CompletedAt == nil {
			done := c.At.Add(d)
			t.CompletedAt = &done
		}
	}
	if fb := feedback(c.Rating, c.Comment); fb != nil {
		t.Extra["feedback"] = fb
	}
	if c.Agent != "" {
		t.Extra["agent_id"] = c.Agent
	}
	if c.Surface != "" {
		t.Extra["surface"] = c.Surface
	}
	if t.SemanticModelRef == "" {
		switch {
		case c.Model != "" && c.Explore != "":
			t.SemanticModelRef = c.Model + "/" + c.Explore
		case c.Explore != "":
			t.SemanticModelRef = c.Explore
		case c.Agent != "":
			t.SemanticModelRef = "agent:" + c.Agent
		}
	}
}

// feedback maps the documented ratings (Thumbs up, Thumbs down, Unrated,
// Removed rating) to a score, keeping the rating as given.
func feedback(rating, comment string) map[string]any {
	r := strings.ToLower(rating)
	if (r == "" || r == "unrated") && comment == "" {
		return nil
	}
	fb := map[string]any{}
	if rating != "" {
		fb["rating"] = rating
	}
	switch {
	case strings.Contains(r, "up") || r == "positive" || r == "1":
		fb["score"] = 1
	case strings.Contains(r, "down") || r == "negative" || r == "-1":
		fb["score"] = -1
	}
	if comment != "" {
		fb["comment"] = comment
	}
	return fb
}

// caQuery is one page of questions, newest first, before before (zero: the
// first page).
func caQuery(f caFields, since, before time.Time, includeUsers bool, limit int) inlineQuery {
	q := inlineQuery{Model: saModel, View: f.Explore, Fields: f.fields(includeUsers),
		Sorts: []string{f.Time + " desc"}, Limit: fmt.Sprint(limit), QueryTimezone: "UTC"}
	if w := window(since, before); w != "" {
		q.Filters = map[string]string{f.Time: w}
	}
	return q
}

// eachCA pages through the questions newest first by keyset on the time
// field: each page asks for rows before the oldest second of the previous
// one (inclusive, deduplicated), so no page is ever unbounded. fn returns
// false to stop.
func (r *run) eachCA(ctx context.Context, f caFields, fn func(caRow) (bool, error)) error {
	var before time.Time
	boundary := map[string]bool{} // turn ids at the previous page's oldest second
	for page := 1; ; page++ {
		if page > maxCAPages {
			r.res.Notes = append(r.res.Notes, fmt.Sprintf("stopped after %d pages of Conversational Analytics questions (narrow with --since)", maxCAPages))
			return nil
		}
		q := caQuery(f, r.opts.Since, before, r.opts.IncludeUsers, caPageSize)
		rows, err := r.c.run(ctx, "", q)
		if err != nil {
			return err
		}
		if page == 1 {
			r.res.Queries = append(r.res.Queries, plannedQuery(purposeCA, q.statement(), len(rows)))
		} else {
			r.res.Tally(purposeCANext, caNextStatement, len(rows))
		}
		var oldest time.Time
		fresh := 0
		next := map[string]bool{}
		for _, raw := range rows {
			c, ok := f.row(raw)
			if !ok {
				continue
			}
			if !r.opts.Since.IsZero() && c.At.Before(r.opts.Since) {
				continue
			}
			sec := c.At.Truncate(time.Second)
			if oldest.IsZero() || sec.Before(oldest) {
				oldest, next = sec, map[string]bool{}
			}
			if sec.Equal(oldest) {
				next[c.TurnID] = true
			}
			if boundary[c.TurnID] {
				continue
			}
			fresh++
			more, err := fn(c)
			if err != nil || !more {
				return err
			}
		}
		if len(rows) < caPageSize || oldest.IsZero() {
			return nil
		}
		if fresh == 0 {
			r.res.Notes = append(r.res.Notes, "stopped paging Conversational Analytics questions: a page held only questions of one second already read")
			return nil
		}
		before, boundary = oldest.Add(time.Second), next
	}
}

// chooser picks the conversations to collect: the first cap distinct ones
// met newest first. A cap of 0 takes every one, a negative cap none.
type chooser struct {
	cap    int
	chosen map[string]bool
	capped bool
}

func newChooser(cap int) *chooser { return &chooser{cap: cap, chosen: map[string]bool{}} }

func (c *chooser) admit(key string) bool {
	if c.chosen[key] {
		return true
	}
	if c.cap < 0 { // no room left
		c.capped = true
		return false
	}
	if c.cap > 0 && len(c.chosen) >= c.cap {
		c.capped = true
		return false
	}
	c.chosen[key] = true
	return true
}

// systemActivityTurns emits a turn for each question in the window, the
// most recent conversations first, with the History queries matched to it.
// keep, if set, says which rows are emitted (the others go to aside).
func (r *run) systemActivityTurns(ctx context.Context, f caFields, ch *chooser, keep func(caRow) bool, aside func(caRow)) error {
	lastAsk := map[string]time.Time{} // per user: the newer question just handled
	idle := 0                         // rows turned away by the cap since the last one taken
	return r.eachCA(ctx, f, func(c caRow) (bool, error) {
		if keep != nil && !keep(c) {
			aside(c)
			idle = 0
			return true, nil
		}
		if !ch.admit(c.convKey()) {
			// Past the cap: read on a little for older questions of the
			// chosen conversations, then stop.
			if idle++; idle > drainPages*caPageSize {
				return false, nil
			}
			return true, nil
		}
		idle = 0
		t := c.turn(r.opts.IncludeUsers)
		until := lastAsk[c.User]
		lastAsk[c.User] = c.At
		r.attachHistory(ctx, &t, "", c.User, c.At, until, c.latency())
		r.emittedSA++
		return true, r.emit(t)
	})
}

// attachHistory claims the History queries of user after asked and fetches
// their SQL and definitions as token.
func (r *run) attachHistory(ctx context.Context, t *turns.Turn, token, user string, asked, until time.Time, latency time.Duration) {
	qs := r.m.take(user, asked, until, latency)
	if len(qs) == 0 {
		return
	}
	var got []fetched
	var kept []*aiQuery
	for _, q := range qs {
		f, err := fetchQuery(ctx, r.c, token, q, r.res.Tally)
		if err != nil {
			r.fetchFailed++
			if r.fetchErr == "" {
				r.fetchErr = clip(err.Error(), 200)
			}
			continue
		}
		got, kept = append(got, f), append(kept, q)
	}
	attach(t, got, kept)
}
