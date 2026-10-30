package looker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
)

// defaultFixture: two users, three questions, five AI queries in History of
// which three follow a question.
func defaultFixture(t *testing.T) *fakeLooker {
	f := newFake(t)
	f.caRows = []map[string]any{
		caRow_("1", "c1", "Orders last week?", 0, "Thumbs up", "Success", 12),
		caRow_("2", "c2", "Revenue?", time.Minute, "Unrated", "Timeout", 0),
		caRow_("1", "c1", "And by region?", 3*time.Minute, "Thumbs down", "Error", 0),
	}
	f.histRows = []map[string]any{
		histRow("101", "1", 20*time.Second),
		histRow("103", "2", 90*time.Second),
		histRow("102", "1", 3*time.Minute+10*time.Second),
		histRow("104", "1", 30*time.Minute), // long after any question
		histRow("105", "3", 5*time.Second),  // a user with no question
	}
	for _, id := range []string{"101", "102", "103", "104", "105"} {
		f.sql[id] = "SELECT " + id + " FROM orders"
	}
	return f
}

type row struct {
	TurnID     string           `json:"turn_id"`
	Question   string           `json:"question_text"`
	Answer     *string          `json:"answer_text"`
	Outcome    *string          `json:"outcome"`
	ErrorType  *string          `json:"error_type"`
	Conv       *string          `json:"conversation_id"`
	User       *string          `json:"user_id"`
	UserName   *string          `json:"user_name"`
	Completed  *string          `json:"completed_at"`
	Statements []map[string]any `json:"statements"`
	ToolCalls  []map[string]any `json:"tool_calls"`
	Reasoning  []map[string]any `json:"reasoning"`
	Extra      map[string]any   `json:"extra"`
}

func decodeRows(t *testing.T, recs []bundle.Record) ([]row, []string) {
	t.Helper()
	var out []row
	var cols []string
	for _, r := range recs {
		var p struct {
			Columns []string        `json:"columns"`
			Row     json.RawMessage `json:"row"`
		}
		b, _ := json.Marshal(r.Payload)
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		cols = p.Columns
		var x row
		if err := json.Unmarshal(p.Row, &x); err != nil {
			t.Fatal(err)
		}
		out = append(out, x)
	}
	return out, cols
}

func byQuestion(rs []row) map[string]row {
	m := map[string]row{}
	for _, r := range rs {
		m[r.Question] = r
	}
	return m
}

func runLooker(t *testing.T, f *fakeLooker, perUser string, opts agent.Options) (agent.Collected, error) {
	t.Helper()
	srv := f.server()
	return agent.CollectAll(context.Background(), collect, agent.Inputs(f.inputs(srv, perUser)), opts)
}

func hasNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func TestCollectSystemActivity(t *testing.T) {
	f := defaultFixture(t)
	c, err := runLooker(t, f, "no", agent.Options{MaxConversations: 2000})
	if err != nil {
		t.Fatal(err)
	}
	rs, cols := decodeRows(t, c.Records)
	if len(rs) != 3 {
		t.Fatalf("got %d turns, want 3", len(rs))
	}
	for _, col := range []string{"conversation_id", "statements", "tool_calls", "extra", "outcome"} {
		if !strings.Contains(strings.Join(cols, ","), col) {
			t.Errorf("columns %v lack %s", cols, col)
		}
	}
	if strings.Contains(strings.Join(cols, ","), "user_name") {
		t.Errorf("user_name declared without --include-users: %v", cols)
	}
	m := byQuestion(rs)
	first := m["Orders last week?"]
	if first.Answer != nil {
		t.Errorf("answer from System Activity: %v", *first.Answer)
	}
	if len(first.Statements) != 1 || first.Statements[0]["sql"] != "SELECT 101 FROM orders" || first.Statements[0]["status"] != "succeeded" {
		t.Errorf("first turn statements = %v", first.Statements)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0]["name"] != "looker_query" {
		t.Errorf("first turn tool calls = %v", first.ToolCalls)
	} else if in := first.ToolCalls[0]["input"].(map[string]any); in["can"] != nil || in["view"] != "orders" {
		t.Errorf("looker_query input = %v", in)
	}
	if fb, _ := first.Extra["feedback"].(map[string]any); fb["score"] != float64(1) || fb["rating"] != "Thumbs up" {
		t.Errorf("feedback = %v", first.Extra["feedback"])
	}
	if *first.Outcome != "completed" || first.Completed == nil || *first.Completed != "2026-10-01T12:00:12Z" {
		t.Errorf("outcome %v completed %v", *first.Outcome, first.Completed)
	}
	if first.Conv == nil || *first.Conv != "c1" || *first.User != "1" {
		t.Errorf("conv/user = %v %v", first.Conv, first.User)
	}
	second := m["And by region?"]
	if len(second.Statements) != 1 || second.Statements[0]["sql"] != "SELECT 102 FROM orders" {
		t.Errorf("second turn statements = %v", second.Statements)
	}
	if *second.Outcome != "failed" || *second.ErrorType != "error" {
		t.Errorf("second outcome = %v %v", *second.Outcome, *second.ErrorType)
	}
	rev := m["Revenue?"]
	if *rev.Outcome != "failed" || *rev.ErrorType != "timeout" || rev.Extra["feedback"] != nil {
		t.Errorf("timeout turn = %+v", rev)
	}
	if len(rev.Statements) != 1 || rev.Statements[0]["sql"] != "SELECT 103 FROM orders" {
		t.Errorf("third turn statements = %v", rev.Statements)
	}
	if !hasNote(c.Notes, "answer_text is null") || !hasNote(c.Notes, "3 of 5 queries matched, 2 matched no question") {
		t.Errorf("notes = %v", c.Notes)
	}
	if len(c.Redactions) != 2 {
		t.Errorf("redactions = %v", c.Redactions)
	}
	if f.hit("GET /api/4.0/conversations") > 0 || f.hit("POST /api/4.0/login/") > 0 {
		t.Error("default mode impersonated a user or read conversations")
	}
	if f.hit("GET /api/4.0/queries/104") > 0 {
		t.Error("fetched the SQL of an unmatched query")
	}
	if open := f.tokensOut(); len(open) > 0 {
		t.Errorf("sessions left open: %v", open)
	}
	if c.Conversations != 2 || !c.Sampled {
		t.Errorf("conversations %d sampled %v", c.Conversations, c.Sampled)
	}
	checkPlan(t, f, c, "no", agent.Options{MaxConversations: 2000})
}

func TestIncludeUsers(t *testing.T) {
	f := defaultFixture(t)
	c, err := runLooker(t, f, "no", agent.Options{IncludeUsers: true})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := decodeRows(t, c.Records)
	if rs[0].UserName == nil || !strings.HasPrefix(*rs[0].UserName, "Name ") {
		t.Errorf("user name not kept with --include-users: %+v", rs[0])
	}
	for _, q := range f.bodies {
		if q.View == "user" {
			t.Error("read the user directory")
		}
	}
}

func TestSince(t *testing.T) {
	f := defaultFixture(t)
	since := base.Add(2 * time.Minute)
	c, err := runLooker(t, f, "no", agent.Options{Since: since})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := decodeRows(t, c.Records)
	if len(rs) != 1 || rs[0].Question != "And by region?" {
		t.Fatalf("turns since %s = %+v", since, rs)
	}
	for _, q := range f.bodies {
		var tf string
		for k, v := range q.Filters {
			if strings.HasSuffix(k, "created_time") {
				tf = v
			}
		}
		if tf != "after 2026/10/01 12:02:00" {
			t.Errorf("%s filter = %q", q.View, tf)
		}
	}
}

func TestMaxConversationsAndPaging(t *testing.T) {
	old := caPageSize
	caPageSize = 3
	defer func() { caPageSize = old }()
	f := newFake(t)
	// 6 conversations of 2 questions, a minute apart; conversation i's
	// questions at minutes i and i+10, so the newest four rows hold the
	// second questions of conversations 5,4,3,2.
	for i := 0; i < 6; i++ {
		conv := fmt.Sprintf("c%d", i)
		f.caRows = append(f.caRows,
			caRow_("1", conv, fmt.Sprintf("q%d-a", i), time.Duration(i)*time.Minute, "", "Success", 1),
			caRow_("1", conv, fmt.Sprintf("q%d-b", i), time.Duration(i+10)*time.Minute, "", "Success", 1))
	}
	c, err := runLooker(t, f, "no", agent.Options{MaxConversations: 2})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := decodeRows(t, c.Records)
	got := map[string]bool{}
	for _, r := range rs {
		got[r.Question] = true
	}
	for _, q := range []string{"q5-a", "q5-b", "q4-a", "q4-b"} {
		if !got[q] {
			t.Errorf("missing %s; got %v", q, got)
		}
	}
	if len(rs) != 4 || c.Conversations != 2 || !hasNote(c.Notes, "--max-conversations 2") {
		t.Errorf("turns %d conversations %d notes %v", len(rs), c.Conversations, c.Notes)
	}
	pages := 0
	for _, q := range f.bodies {
		if q.View == caView {
			pages++
			if q.Limit != "3" {
				t.Errorf("page limit %s", q.Limit)
			}
		}
	}
	if pages < 2 {
		t.Errorf("read %d pages, want keyset paging", pages)
	}
	checkPlan(t, f, c, "no", agent.Options{MaxConversations: 2})
}

func TestKeysetBoundaryDedup(t *testing.T) {
	old := caPageSize
	caPageSize = 2
	defer func() { caPageSize = old }()
	f := newFake(t)
	// Three questions in the same second straddle a page boundary.
	for i, q := range []string{"x", "y", "z"} {
		r := caRow_("1", fmt.Sprintf("c%d", i), q, time.Minute, "", "", 0)
		f.caRows = append(f.caRows, r)
	}
	f.caRows = append(f.caRows, caRow_("1", "c9", "older", 0, "", "", 0))
	c, err := runLooker(t, f, "no", agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := decodeRows(t, c.Records)
	seen := map[string]int{}
	for _, r := range rs {
		seen[r.Question]++
	}
	// The fake cannot return the third same-second row (the page is full
	// of that second and Looker has no offset), so the walk stops with a
	// note rather than looping; what it read is read once.
	for q, n := range seen {
		if n != 1 {
			t.Errorf("%s emitted %d times", q, n)
		}
	}
	if len(rs) < 2 {
		t.Errorf("turns = %v", seen)
	}
}

func TestMatcher(t *testing.T) {
	m := newMatcher()
	q := func(id string, d time.Duration) *aiQuery { return &aiQuery{QueryID: id, User: "u", At: base.Add(d)} }
	for _, x := range []*aiQuery{q("early", -3*time.Second), q("a", 30*time.Second), q("b", 100*time.Second),
		q("late", 4*time.Minute), q("next", 5*time.Minute+10*time.Second), q("tooEarly", -time.Minute)} {
		m.add(x)
	}
	m.sort()
	ids := func(qs []*aiQuery) string {
		var s []string
		for _, q := range qs {
			s = append(s, q.QueryID)
		}
		return strings.Join(s, ",")
	}
	// The newer question at +5m bounds nothing here; the 2-minute window does.
	if got := ids(m.take("u", base, base.Add(5*time.Minute), 0)); got != "early,a,b" {
		t.Errorf("window: %s", got)
	}
	// Claimed queries are not handed out again; latency widens the window.
	if got := ids(m.take("u", base, base.Add(5*time.Minute), 5*time.Minute)); got != "late" {
		t.Errorf("latency window: %s", got)
	}
	// The next question of the same user cuts the window.
	if got := ids(m.take("u", base.Add(5*time.Minute), base.Add(5*time.Minute+5*time.Second), 0)); got != "" {
		t.Errorf("until: %s", got)
	}
	if got := ids(m.take("other", base, time.Time{}, 0)); got != "" {
		t.Errorf("other user: %s", got)
	}
	if m.unmatched() != 2 {
		t.Errorf("unmatched = %d", m.unmatched())
	}
}

func TestSystemActivityForbidden(t *testing.T) {
	f := defaultFixture(t)
	f.saStatus = 403
	_, err := runLooker(t, f, "no", agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "see_system_activity") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "csecret") {
		t.Error("secret in error")
	}
	if open := f.tokensOut(); len(open) > 0 {
		t.Errorf("sessions left open: %v", open)
	}
}

func TestBadCredentials(t *testing.T) {
	f := defaultFixture(t)
	srv := f.server()
	in := agent.Inputs(f.inputs(srv, "no"))
	in["client-secret"] = "wrong"
	_, err := agent.CollectAll(context.Background(), collect, in, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "rejected the client id and secret") || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("err = %v", err)
	}
}

func TestHistoryOnlyIsAnError(t *testing.T) {
	f := defaultFixture(t)
	f.noCA = true
	_, err := runLooker(t, f, "no", agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "--per-user") {
		t.Fatalf("err = %v", err)
	}
}

// perUserFixture adds conversations to the default fixture: user 1 has c1
// (two questions) and an old c0; user 2 has c2.
func perUserFixture(t *testing.T) *fakeLooker {
	f := defaultFixture(t)
	conv := func(id string, created, updated time.Duration) map[string]any {
		return map[string]any{"id": id, "user_id": "1", "created_at": base.Add(created).Format(time.RFC3339),
			"updated_at": base.Add(updated).Format(time.RFC3339)}
	}
	f.convs["1"] = []map[string]any{conv("c1", 0, 4*time.Minute), conv("c0", -48*time.Hour, -47*time.Hour)}
	f.convs["2"] = []map[string]any{conv("c2", time.Minute, 2*time.Minute)}
	ts := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339) }
	f.msgs["c1"] = []map[string]any{
		{"id": "m1", "type": "user", "order": 1, "message": map[string]any{"timestamp": ts(0), "userMessage": map[string]any{"text": "Orders last week?"}}},
		{"id": "m2", "type": "system", "order": 2, "message": map[string]any{"timestamp": ts(5 * time.Second), "systemMessage": map[string]any{
			"text": map[string]any{"parts": []any{"Looking at orders"}, "textType": "THOUGHT"}}}},
		{"id": "m3", "type": "system", "order": 3, "message": map[string]any{"timestamp": ts(10 * time.Second), "systemMessage": map[string]any{
			"data": map[string]any{"generatedSql": "SELECT count(*) FROM orders", "generatedLookerQuery": map[string]any{"model": "ecomm", "view": "orders", "can": map[string]any{}},
				"result": map[string]any{"data": []any{map[string]any{"orders.count": 1204}}}}}}},
		{"id": "m4", "type": "system", "order": 4, "message": map[string]any{"timestamp": ts(12 * time.Second), "systemMessage": map[string]any{
			"text": map[string]any{"parts": []any{"1,204 orders."}, "textType": "FINAL_RESPONSE"}}}},
		{"id": "m5", "type": "user", "order": 5, "message": map[string]any{"timestamp": ts(3 * time.Minute), "userMessage": map[string]any{"text": "And by region?"}}},
		{"id": "m6", "type": "system", "order": 6, "message": map[string]any{"timestamp": ts(3*time.Minute + 4*time.Second), "systemMessage": map[string]any{
			"error": map[string]any{"text": "the explore has no region"}}}},
	}
	f.msgs["c2"] = []map[string]any{
		{"id": "n1", "order": 1, "message": map[string]any{"timestamp": ts(time.Minute), "userMessage": map[string]any{"text": "Revenue?"}}},
	}
	f.msgs["c0"] = []map[string]any{
		{"id": "o1", "order": 1, "message": map[string]any{"timestamp": ts(-48 * time.Hour), "userMessage": map[string]any{"text": "old"}}},
	}
	return f
}

func TestPerUser(t *testing.T) {
	f := perUserFixture(t)
	opts := agent.Options{Since: base.Add(-time.Hour), MaxConversations: 2000}
	c, err := runLooker(t, f, "yes", opts)
	if err != nil {
		t.Fatal(err)
	}
	rs, cols := decodeRows(t, c.Records)
	if len(rs) != 3 {
		t.Fatalf("got %d turns: %+v", len(rs), rs)
	}
	if !strings.Contains(strings.Join(cols, ","), "reasoning") {
		t.Errorf("columns = %v", cols)
	}
	m := byQuestion(rs)
	first := m["Orders last week?"]
	if first.TurnID != "m1" || first.Answer == nil || *first.Answer != "1,204 orders." || *first.Outcome != "completed" {
		t.Errorf("first = %+v", first)
	}
	if len(first.Statements) != 1 || first.Statements[0]["sql"] != "SELECT count(*) FROM orders" || first.Statements[0]["rows"] != float64(1) {
		t.Errorf("statements = %v", first.Statements)
	}
	if first.Statements[0]["result"] != nil {
		t.Error("result rows kept without --include-results")
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0]["name"] != "looker_query" {
		t.Errorf("tool calls = %v", first.ToolCalls)
	}
	if len(first.Reasoning) != 1 || first.Reasoning[0]["text"] != "Looking at orders" {
		t.Errorf("reasoning = %v", first.Reasoning)
	}
	if fb, _ := first.Extra["feedback"].(map[string]any); fb["score"] != float64(1) {
		t.Errorf("not enriched from System Activity: %v", first.Extra)
	}
	second := m["And by region?"]
	if *second.Outcome != "failed" || second.Answer != nil {
		t.Errorf("second = %+v", second)
	}
	// No generatedSql in the messages: History's query is attached.
	if len(second.Statements) != 1 || second.Statements[0]["sql"] != "SELECT 102 FROM orders" {
		t.Errorf("second statements = %v", second.Statements)
	}
	if m["old"].TurnID != "" {
		t.Error("read a conversation outside the window")
	}
	if open := f.tokensOut(); len(open) > 0 {
		t.Errorf("sessions left open: %v", open)
	}
	if hasNote(c.Notes, "answer_text is null") {
		t.Errorf("answer note on a per-user run: %v", c.Notes)
	}
	if c.Conversations != 2 {
		t.Errorf("conversations = %d", c.Conversations)
	}
	checkPlan(t, f, c, "yes", opts)
}

func TestPerUserMaxConversations(t *testing.T) {
	f := perUserFixture(t)
	c, err := runLooker(t, f, "yes", agent.Options{MaxConversations: 1})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := decodeRows(t, c.Records)
	for _, r := range rs {
		if *r.Conv != "c1" {
			t.Errorf("read %s past the cap of the most recent conversation", *r.Conv)
		}
	}
	if len(rs) != 2 || c.Conversations != 1 || !hasNote(c.Notes, "--max-conversations 1") {
		t.Errorf("turns %d conversations %d notes %v", len(rs), c.Conversations, c.Notes)
	}
	if f.hit("GET /api/4.0/conversations/c2/messages") > 0 {
		t.Error("read messages of a conversation not chosen")
	}
}

func TestPerUserSudoForbidden(t *testing.T) {
	f := perUserFixture(t)
	f.sudoStatus = 403
	c, err := runLooker(t, f, "yes", agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := decodeRows(t, c.Records)
	if len(rs) != 3 || rs[0].Answer != nil {
		t.Errorf("fallback turns = %+v", rs)
	}
	if !hasNote(c.Notes, "refused to log in as a user (HTTP 403)") || !hasNote(c.Notes, "answer_text is null") {
		t.Errorf("notes = %v", c.Notes)
	}
	if f.hit("GET /api/4.0/conversations") > 0 {
		t.Error("read conversations without sudo")
	}
}

// checkPlan: every request the fake saw has a statement in Plan, and every
// Result query's purpose is a Plan purpose.
func checkPlan(t *testing.T, f *fakeLooker, c agent.Collected, perUser string, opts agent.Options) {
	t.Helper()
	p := plan(agent.Inputs{"per-user": perUser}, opts)
	purposes := map[string]bool{}
	var pats []*regexp.Regexp
	ph := regexp.MustCompile(`<[^>]*>`)
	for _, q := range p {
		purposes[q.Purpose] = true
		s := ph.ReplaceAllString(q.Statement, "PLACEHOLDER")
		parts := strings.Fields(s)
		u := strings.SplitN(parts[1], "?", 2)[0]
		re := "^" + parts[0] + " " + strings.ReplaceAll(regexp.QuoteMeta(u), "PLACEHOLDER", "[^/]+") + "$"
		pats = append(pats, regexp.MustCompile(re))
	}
	for _, q := range c.Queries {
		if !purposes[q.Purpose] {
			t.Errorf("Result query %q is not in Plan", q.Purpose)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		ok := false
		for _, re := range pats {
			ok = ok || re.MatchString(r)
		}
		if !ok {
			t.Errorf("request %s is not in Plan", r)
		}
	}
}

func TestCheck(t *testing.T) {
	f := defaultFixture(t)
	srv := f.server()
	res, err := check(context.Background(), agent.Inputs(f.inputs(srv, "no")), agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Join(res.Facts, "\n")
	for _, want := range []string{"Connected to Looker", "Conversational Analytics (" + caView + "): 3 questions from 2 users in total",
		"query history: 5 AI-generated queries in total (SQL found for 100% of the 5 most recent)", "available with --per-user yes"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if f.hit("POST /api/4.0/login/") > 0 {
		t.Error("Check impersonated a user without --per-user")
	}
	if open := f.tokensOut(); len(open) > 0 {
		t.Errorf("sessions left open: %v", open)
	}
}

func TestCheckPerUser(t *testing.T) {
	f := perUserFixture(t)
	srv := f.server()
	res, err := check(context.Background(), agent.Inputs(f.inputs(srv, "yes")), agent.Options{})
	if err != nil || !strings.Contains(strings.Join(res.Facts, "\n"), "readable with --per-user") {
		t.Fatalf("err %v facts %v warnings %v", err, res.Facts, res.Warnings)
	}
	f.sudoStatus = 403
	res, err = check(context.Background(), agent.Inputs(f.inputs(srv, "yes")), agent.Options{})
	if err != nil || !strings.Contains(strings.Join(res.Warnings, "\n"), "sudo is not permitted") {
		t.Fatalf("err %v warnings %v", err, res.Warnings)
	}
	if open := f.tokensOut(); len(open) > 0 {
		t.Errorf("sessions left open: %v", open)
	}
}

func TestCheckSystemActivityForbidden(t *testing.T) {
	f := defaultFixture(t)
	f.saStatus = 403
	srv := f.server()
	_, err := check(context.Background(), agent.Inputs(f.inputs(srv, "no")), agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "see_system_activity") {
		t.Fatalf("err = %v", err)
	}
}

func TestHostMustBeHTTPS(t *testing.T) {
	if _, err := newClient("http://acme.cloud.looker.com", "a", "b"); err == nil {
		t.Error("accepted http for a remote host")
	}
	if c, err := newClient("acme.cloud.looker.com", "a", "b"); err != nil || c.base != "https://acme.cloud.looker.com" {
		t.Errorf("base %v err %v", c, err)
	}
}

func TestPickCA(t *testing.T) {
	var fs []field
	b, _ := json.Marshal(caFieldList())
	json.Unmarshal(b, &fs)
	got, ok := pickCA(caView, fs)
	if !ok || got.Question != caView+".question" || got.Time != caView+".created_time" || got.User != "user.id" ||
		got.Conversation != caView+".conversation_id" || got.Rating != caView+".rating" ||
		got.Success != caView+".answer_success" || got.Latency != caView+".latency" {
		t.Errorf("picked %+v", got)
	}
	if _, ok := pickCA("x", []field{{Name: "x.count", Measure: true}}); ok {
		t.Error("picked an explore without a question")
	}
	if got := caCandidates([]string{"history", "gemini_usage", "conversational_analytics_query", "user"}); strings.Join(got, ",") != "conversational_analytics_query,gemini_usage" {
		t.Errorf("candidates = %v", got)
	}
}

func TestContextCheck(t *testing.T) {
	dir := t.TempDir()
	if _, err := checkContext(dir); err == nil {
		t.Error("accepted an empty folder")
	}
	for name, body := range map[string]string{"ecomm.model.lkml": "explore: orders {}", "views/orders.view.lkml": "view: orders {}",
		"manifest.lkml": "project_name: \"x\"", "README.md": "#", "secrets.json": "{}"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	res, err := checkContext(dir)
	if err != nil || !strings.Contains(res.Facts[0], "1 models, 1 views") || !strings.Contains(res.Facts[1], "4 files") {
		t.Errorf("facts %v err %v", res.Facts, err)
	}
	if keepContext("secrets.json") {
		t.Error("kept a json file")
	}
}

func TestRegistered(t *testing.T) {
	a, err := agent.Get("looker")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != agent.Experimental || a.Context == nil {
		t.Errorf("agent = %+v", a)
	}
	for _, f := range a.Fields {
		if f.Key == "client-secret" && !f.Secret {
			t.Error("client-secret not secret")
		}
		if f.Key == "per-user" && (f.Default != "no" || len(f.Choices) != 2) {
			t.Errorf("per-user field = %+v", f)
		}
	}
}

func TestEmitErrorStopsAndLogsOut(t *testing.T) {
	f := perUserFixture(t)
	srv := f.server()
	stop := fmt.Errorf("disk full")
	n := 0
	_, err := collect(context.Background(), agent.Inputs(f.inputs(srv, "yes")), agent.Options{}, func(bundle.Record) error {
		n++
		return stop
	})
	if err != stop || n != 1 {
		t.Fatalf("err %v after %d records", err, n)
	}
	if open := f.tokensOut(); len(open) > 0 {
		t.Errorf("sessions left open: %v", open)
	}
}

func TestPerUserIncludeResults(t *testing.T) {
	f := perUserFixture(t)
	c, err := runLooker(t, f, "yes", agent.Options{IncludeResults: true, Since: base.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	rs, _ := decodeRows(t, c.Records)
	first := byQuestion(rs)["Orders last week?"]
	if first.Statements[0]["result"] == nil {
		t.Errorf("result rows dropped with --include-results: %v", first.Statements)
	}
	if len(c.Redactions) != 1 {
		t.Errorf("redactions = %v", c.Redactions)
	}
}
