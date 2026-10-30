package hex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

func init() {
	minGap = 0
	sleep = func(context.Context, time.Duration) error { return nil }
}

func rec(v map[string]any) map[string]any {
	b, _ := json.Marshal(v)
	return map[string]any{"type": "text", "text": string(b), "createdAt": v["createdAt"]}
}

func txt(s, at string) map[string]any {
	return map[string]any{"type": "text", "text": s, "createdAt": at}
}

// t1 has full records over two message pages (newest first), plus the
// rendered text blocks the records repeat.
var t1Newest = []map[string]any{
	txt("user: And by region?", "2026-10-02T10:01:00Z"),
	rec(map[string]any{"role": "user", "createdAt": "2026-10-02T10:01:00Z", "content": []any{map[string]any{"type": "text", "text": "And by region?"}}}),
	rec(map[string]any{"role": "assistant", "createdAt": "2026-10-02T10:01:05Z", "content": "EMEA leads."}),
}

var t1Older = []map[string]any{
	rec(map[string]any{"role": "user", "createdAt": "2026-10-02T10:00:00Z", "content": []any{map[string]any{"type": "text", "text": "Orders last week?"}}}),
	rec(map[string]any{"role": "assistant", "createdAt": "2026-10-02T10:00:02Z", "content": []any{
		map[string]any{"type": "thinking", "thinking": "Count orders in week 39."},
		map[string]any{"type": "tool_use", "id": "c1", "name": "run_sql", "input": map[string]any{"sql": "select count(*) from orders where week = 39"}},
		map[string]any{"type": "tool_use", "id": "c2", "name": "search_docs", "input": map[string]any{"q": "orders"}},
	}}),
	rec(map[string]any{"role": "tool", "createdAt": "2026-10-02T10:00:03Z", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": `[{"n":1204}]`},
		map[string]any{"type": "tool_result", "tool_use_id": "c2", "content": "orders: one row per order"},
	}}),
	rec(map[string]any{"role": "assistant", "createdAt": "2026-10-02T10:00:04Z", "content": []any{map[string]any{"type": "text", "text": "1,204 orders."}}}),
}

// t3 has rendered text only.
var t3Blocks = []map[string]any{
	txt("user: Revenue by region?", "2026-09-30T09:00:00Z"),
	txt("assistant: [thinking] Use the regions table.", "2026-09-30T09:00:01Z"),
	txt("assistant: EMEA leads with 40%.", "2026-09-30T09:00:02Z"),
}

func threadJSON(id, created string, extra map[string]any) map[string]any {
	t := map[string]any{"id": id, "title": "t " + id, "source": "SLACK", "type": "Threads", "classification": "Analysis",
		"userId": "u-" + id, "createdAt": created, "updatedAt": created, "sensitive": false, "orgId": "o1",
		"url": "https://app.hex.tech/t/" + id, "effectiveModel": "claude-x", "model": "Auto", "auto": true}
	for k, v := range extra {
		t[k] = v
	}
	return t
}

type fakeHex struct {
	t        *testing.T
	limited  bool
	requests []string
	pages    map[string]map[string]any // thread list pages by after cursor
}

func newFake(t *testing.T) *fakeHex {
	f := &fakeHex{t: t, pages: map[string]map[string]any{
		"": {"threads": []any{
			threadJSON("t1", "2026-10-02T10:00:00Z", map[string]any{"feedback": []any{map[string]any{"id": "f1", "type": "THUMBS", "userLiked": false, "userFeedback": "wrong week"}},
				"warnings": []any{map[string]any{"id": "w1", "type": "DATA_LIMITATION", "summary": "partial"}}}),
			threadJSON("t2", "2026-10-01T10:00:00Z", map[string]any{"sensitive": true, "title": nil}),
		}, "pagination": map[string]any{"after": "c2", "before": nil}},
		"c2": {"threads": []any{
			threadJSON("t3", "2026-09-30T09:00:00Z", map[string]any{"source": "HEX"}),
			threadJSON("t4", "2026-08-01T09:00:00Z", nil),
		}, "pagination": map[string]any{"after": nil, "before": "c1"}},
	}}
	return f
}

func (f *fakeHex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if r.Method != http.MethodGet {
		f.t.Errorf("not a GET: %s", r.Method)
	}
	if r.Header.Get("Authorization") != "Bearer hx-good" {
		http.Error(w, `{"message":"unauthorized","code":"UNAUTHORIZED"}`, http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	write := func(v any) { json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/api/v1/users/me":
		write(map[string]any{"id": "u1", "name": "Ana", "email": "ana@shop.test", "role": "ADMIN", "lastLoginDate": nil, "org": map[string]any{"id": "o1"}})
	case r.URL.Path == "/api/v1/threads":
		if !f.limited {
			f.limited = true
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"message":"slow down"}`, http.StatusTooManyRequests)
			return
		}
		p, ok := f.pages[q.Get("after")]
		if !ok {
			f.t.Errorf("unknown cursor %q", q.Get("after"))
		}
		write(p)
	case strings.HasPrefix(r.URL.Path, "/api/v1/threads/") && strings.HasSuffix(r.URL.Path, "/messages"):
		if q.Get("full") != "true" || q.Get("limit") != "50" {
			f.t.Errorf("messages query %q", r.URL.RawQuery)
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/threads/"), "/messages")
		switch {
		case id == "t1" && q.Get("before") == "":
			write(map[string]any{"content": t1Newest, "pagination": map[string]any{"before": "m1", "after": nil}})
		case id == "t1" && q.Get("before") == "m1":
			write(map[string]any{"content": t1Older, "pagination": map[string]any{"before": nil, "after": "m2"}})
		case id == "t3":
			write(map[string]any{"content": t3Blocks, "pagination": map[string]any{"before": nil, "after": nil}})
		case id == "t4":
			write(map[string]any{"content": []any{txt("user: Old one?", "2026-08-01T09:00:00Z"), txt("assistant: Yes.", "2026-08-01T09:00:01Z")},
				"pagination": map[string]any{"before": nil, "after": nil}})
		default:
			f.t.Errorf("messages of %s (before %q)", id, q.Get("before"))
			http.Error(w, "{}", http.StatusNotFound)
		}
	default:
		f.t.Errorf("unexpected %s", r.URL)
		http.NotFound(w, r)
	}
}

func run(t *testing.T, opts agent.Options) (agent.Collected, *fakeHex, agent.Inputs) {
	t.Helper()
	f := newFake(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	in := agent.Inputs{"host": srv.URL, "token": "hx-good"}
	res, err := agent.CollectAll(context.Background(), collect, in, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res, f, in
}

func rows(t *testing.T, c agent.Collected) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, r := range c.Records {
		var p struct {
			Row map[string]any `json:"row"`
		}
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out[p.Row["turn_id"].(string)] = p.Row
	}
	return out
}

func TestCollectMapsThreads(t *testing.T) {
	res, f, in := run(t, agent.Options{})
	got := rows(t, res)
	if len(got) != 4 || res.Conversations != 3 {
		t.Fatalf("turns %d, conversations %d: %v", len(got), res.Conversations, keysOf(got))
	}
	first := got["t1:1"]
	if first["question_text"] != "Orders last week?" || first["answer_text"] != "1,204 orders." || first["position"] != float64(1) ||
		first["conversation_id"] != "t1" || first["client_interface"] != "slack" || first["model_name"] != "claude-x" ||
		first["user_id"] != "u-t1" || first["outcome"] != "completed" || first["asked_at"] != "2026-10-02T10:00:00Z" {
		t.Errorf("t1:1 = %v", first)
	}
	sts := first["statements"].([]any)
	if len(sts) != 1 || sts[0].(map[string]any)["sql"] != "select count(*) from orders where week = 39" ||
		sts[0].(map[string]any)["status"] != "succeeded" || sts[0].(map[string]any)["result"] != nil {
		t.Errorf("statements = %v", sts)
	}
	tcs := first["tool_calls"].([]any)
	if len(tcs) != 1 || tcs[0].(map[string]any)["name"] != "search_docs" || tcs[0].(map[string]any)["output"] != nil {
		t.Errorf("tool calls = %v", tcs)
	}
	if r := first["reasoning"].([]any); len(r) != 1 || r[0].(map[string]any)["text"] != "Count orders in week 39." {
		t.Errorf("reasoning = %v", r)
	}
	if ex := first["extra"].(map[string]any); ex["feedback"] != nil {
		t.Errorf("feedback on the first turn: %v", ex)
	}
	second := got["t1:2"]
	ex := second["extra"].(map[string]any)
	if second["question_text"] != "And by region?" || second["answer_text"] != "EMEA leads." || ex["feedback"] == nil || ex["warnings"] == nil {
		t.Errorf("t1:2 = %v", second)
	}
	plain := got["t3:1"]
	if plain["question_text"] != "Revenue by region?" || plain["answer_text"] != "EMEA leads with 40%." || plain["client_interface"] != "web" ||
		len(plain["reasoning"].([]any)) != 1 {
		t.Errorf("t3:1 = %v", plain)
	}
	notes := strings.Join(res.Notes, "\n")
	if !strings.Contains(notes, "skipped 1 private threads") || !strings.Contains(notes, "2 threads had no full message records") {
		t.Errorf("notes = %v", res.Notes)
	}
	if len(res.Redactions) != 1 {
		t.Errorf("redactions = %v", res.Redactions)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "/t2/") {
			t.Errorf("read the private thread: %s", r)
		}
	}
	// Plan is the truth: every request ran appears in it, and the 429 was retried.
	planned := map[string]bool{}
	for _, q := range plan(in, agent.Options{}) {
		planned[q.Statement] = true
	}
	if len(res.Queries) != 4 {
		t.Errorf("queries = %+v", res.Queries)
	}
	for _, q := range res.Queries {
		if !planned[q.Statement] {
			t.Errorf("ran but not planned: %s", q.Statement)
		}
	}
	if f.requests[0] != f.requests[1] {
		t.Errorf("429 not retried: %v", f.requests[:2])
	}
}

func TestIncludeResultsKeepsOutputs(t *testing.T) {
	res, _, _ := run(t, agent.Options{IncludeResults: true})
	first := rows(t, res)["t1:1"]
	if first["statements"].([]any)[0].(map[string]any)["result"] != `[{"n":1204}]` ||
		first["tool_calls"].([]any)[0].(map[string]any)["output"] != "orders: one row per order" {
		t.Errorf("outputs dropped: %v", first)
	}
	if len(res.Redactions) != 0 {
		t.Errorf("redactions = %v", res.Redactions)
	}
}

func TestSinceStopsPaging(t *testing.T) {
	now = func() time.Time { return time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC) }
	defer func() { now = time.Now }()
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	res, f, _ := run(t, agent.Options{Since: since})
	got := rows(t, res)
	if len(got) != 3 || got["t4:1"] != nil {
		t.Errorf("turns = %v", keysOf(got))
	}
	if !strings.Contains(f.requests[0], "numDays=32") {
		t.Errorf("first request %s", f.requests[0])
	}
	for _, r := range f.requests {
		if strings.Contains(r, "/t4/") {
			t.Errorf("read a thread older than --since: %s", r)
		}
	}
}

func TestMaxConversationsCaps(t *testing.T) {
	res, f, _ := run(t, agent.Options{MaxConversations: 1})
	got := rows(t, res)
	if len(got) != 2 || res.Conversations != 1 || !res.Sampled {
		t.Errorf("turns = %v, conversations %d", keysOf(got), res.Conversations)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "--max-conversations 1") {
		t.Errorf("notes = %v", res.Notes)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "after=c2") {
			t.Errorf("paged past the cap: %s", r)
		}
	}
}

func TestCheckAndVerify(t *testing.T) {
	srv := httptest.NewServer(newFake(t))
	defer srv.Close()
	in := agent.Inputs{"host": srv.URL, "token": "hx-good"}
	fact, err := verifyToken(context.Background(), in)
	if err != nil || fact != "Connected to Hex at "+srv.URL+" as ana@shop.test (ADMIN)" {
		t.Errorf("verify: %q %v", fact, err)
	}
	res, err := check(context.Background(), in, agent.Options{MaxConversations: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if facts := strings.Join(res.Facts, "\n"); !strings.Contains(facts, "more than 1 threads in total") {
		t.Errorf("facts = %v", res.Facts)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "1 private threads") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	bad := agent.Inputs{"host": srv.URL, "token": "hx-SECRETWRONG"}
	if _, err := verifyToken(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "rejected the token") || strings.Contains(err.Error(), "SECRETWRONG") {
		t.Errorf("bad token: %v", err)
	}
	if _, err := check(context.Background(), bad, agent.Options{}); err == nil {
		t.Error("check passed with a bad token")
	}
}

func TestFieldsValidate(t *testing.T) {
	a, err := agent.Get("hex")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(agent.Inputs{"host": "http://app.hex.tech", "token": "x"}); err == nil || !strings.Contains(err.Error(), "--host") {
		t.Errorf("http host: %v", err)
	}
	in := agent.Inputs{"token": "x"}
	if err := a.Validate(in); err != nil || in["host"] != defaultHost {
		t.Errorf("default host: %v %v", err, in)
	}
	if err := a.Validate(agent.Inputs{"token": "x", "source": "EMAIL"}); err == nil {
		t.Error("bad source accepted")
	}
}

func TestRenderedPrefixes(t *testing.T) {
	cases := []struct{ in, kind, text string }{
		{"user: hi", "user", "hi"},
		{"**Assistant**: done", "answer", "done"},
		{"assistant: [thinking] hmm", "thinking", "hmm"},
		{"[error] assistant: boom", "error", "boom"},
		{"<thinking> plan", "thinking", "plan"},
	}
	for _, c := range cases {
		e, _ := renderedEvent(c.in, "")
		if e.kind != c.kind || e.text != c.text {
			t.Errorf("%q: %s %q", c.in, e.kind, e.text)
		}
	}
	if e, _ := renderedEvent("more text", "user"); e.kind != "user" {
		t.Errorf("continuation: %v", e)
	}
}

func TestSQLDetectionIsConservative(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  string
	}{
		{"run_sql", map[string]any{"sql": "SELECT 1"}, "SELECT 1"},
		{"create_sql_cell", map[string]any{"code": "from x select y"}, "from x select y"},
		{"python", map[string]any{"code": "print(1)"}, ""},
		{"search", map[string]any{"query": "orders by week"}, ""},
		{"explore", map[string]any{"query": "with a as (select 1) select * from a"}, "with a as (select 1) select * from a"},
		{"run_sql", `{"sql":"select 2"}`, "select 2"},
	}
	for _, c := range cases {
		if got := sqlOf(c.name, c.input); got != c.want {
			t.Errorf("%s %v: %q", c.name, c.input, got)
		}
	}
}

func keysOf(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
