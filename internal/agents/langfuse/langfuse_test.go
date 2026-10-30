package langfuse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// obs builds a v2 observation; input and output are raw strings, as the API
// returns them.
func obs(id, trace, parent, typ, name, start, end, input, output string, extra map[string]any) map[string]any {
	o := map[string]any{"id": id, "traceId": trace, "parentObservationId": nil, "type": typ, "name": name,
		"startTime": start, "endTime": nil, "projectId": "p1", "level": "DEFAULT",
		"modelId": nil, "inputPrice": nil, "outputPrice": nil, "totalPrice": nil}
	if parent != "" {
		o["parentObservationId"] = parent
	} else {
		o["isRootObservation"] = true
	}
	if end != "" {
		o["endTime"] = end
	}
	if input != "" {
		o["input"] = input
	}
	if output != "" {
		o["output"] = output
	}
	for k, v := range extra {
		o[k] = v
	}
	return o
}

var pages = map[string][]map[string]any{
	"": {
		obs("o1", "t1", "", "AGENT", "analytics-agent", "2026-10-01T10:00:00Z", "2026-10-01T10:00:09Z",
			`{"messages":[{"role":"system","content":"You are an analyst."},{"role":"user","content":"Orders last week?"}]}`,
			`{"role":"assistant","content":"1,204 orders."}`,
			map[string]any{"sessionId": "s1", "userId": "ann@shop.test", "traceName": "analytics-agent", "tags": []any{"prod"}}),
		obs("o2", "t1", "o1", "GENERATION", "plan", "2026-10-01T10:00:01Z", "2026-10-01T10:00:02Z", `[{"role":"user","content":"secret prompt"}]`,
			`{"role":"assistant","content":[{"type":"thinking","thinking":"Count orders in week 39."},{"type":"text","text":"calling run_query"}]}`,
			map[string]any{"model": "gpt-x", "usageDetails": map[string]any{"input": 900, "output": 40, "total": 940}, "totalCost": 0.01}),
		obs("o3", "t1", "o1", "TOOL", "run_query", "2026-10-01T10:00:03Z", "2026-10-01T10:00:04Z",
			`{"query":"select count(*) from orders where week = 39"}`, `{"rows":[{"n":1204}]}`, nil),
		obs("o4", "t1", "o1", "TOOL", "search_docs", "2026-10-01T10:00:02Z", "2026-10-01T10:00:03Z", `{"q":"orders"}`, `"orders: one row per order"`, nil),
	},
	"c2": {
		obs("o5", "t1", "o1", "GENERATION", "answer", "2026-10-01T10:00:05Z", "2026-10-01T10:00:08Z", "", `"1,204 orders."`,
			map[string]any{"model": "gpt-y", "usageDetails": map[string]any{"input": 100, "output": 10}}),
		obs("o6", "t2", "", "AGENT", "analytics-agent", "2026-09-30T10:00:00Z", "2026-09-30T10:00:30Z", "Revenue by region?", "",
			map[string]any{"level": "ERROR", "statusMessage": "timeout", "userId": "u-7"}),
		obs("o7", "t2", "o6", "TOOL", "warehouse", "2026-09-30T10:00:01Z", "", `{"sql":"SHOW TABLES"}`, "", nil),
		obs("o8", "t3", "o9", "TOOL", "orphan", "2026-09-29T10:00:00Z", "2026-09-29T10:00:01Z", "", "", nil),
	},
}

// fake serves pages by cursor and answers the first request with 429.
func fake(t *testing.T, seen *[]*http.Request) *httptest.Server {
	limited := false
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "pk-lf-1" || p != "sk-lf-1" {
			t.Errorf("auth = %q %q", u, p)
		}
		if r.Method != http.MethodGet || r.URL.Path != endpoint {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if !limited {
			limited = true
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"message":"rate limited"}`, http.StatusTooManyRequests)
			return
		}
		*seen = append(*seen, r)
		cursor := r.URL.Query().Get("cursor")
		meta := map[string]any{}
		if cursor == "" {
			meta["cursor"] = "c2"
		}
		json.NewEncoder(w).Encode(map[string]any{"data": pages[cursor], "meta": meta})
	}))
}

func run(t *testing.T, in agent.Inputs, opts agent.Options) (agent.Collected, []*http.Request, []time.Duration) {
	t.Helper()
	var seen []*http.Request
	srv := fake(t, &seen)
	defer srv.Close()
	var waits []time.Duration
	orig := sleep
	sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	t.Cleanup(func() { sleep = orig })
	full := agent.Inputs{"host": srv.URL + "/", "public-key": "pk-lf-1", "secret-key": "sk-lf-1"}
	for k, v := range in {
		full[k] = v
	}
	a, err := agent.Get("langfuse")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(full); err != nil {
		t.Fatal(err)
	}
	res, err := agent.CollectAll(context.Background(), a.Collect, full, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res, seen, waits
}

func rows(t *testing.T, res agent.Collected) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, r := range res.Records {
		if r.SourceKind != "custom_sql" || r.RecordKind != "sql_turn" || r.FetchMethod != fetchMethod {
			t.Errorf("record = %+v", r)
		}
		var p struct{ Row map[string]any }
		json.Unmarshal(r.Payload, &p)
		out[r.NativeIDs["turn_id"]] = p.Row
	}
	return out
}

func TestTracesBecomeTurnsAcrossPagesAfterARetry(t *testing.T) {
	res, seen, waits := run(t, nil, agent.Options{})
	if len(seen) != 2 || len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("pages = %d, waits = %v", len(seen), waits)
	}
	got := rows(t, res)
	if len(got) != 2 {
		t.Fatalf("turns = %v", got)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "1 traces") {
		t.Errorf("notes = %v", res.Notes)
	}

	t1 := got["t1"]
	if t1["question_text"] != "Orders last week?" || t1["answer_text"] != "1,204 orders." || t1["outcome"] != "completed" {
		t.Errorf("t1 = %v", t1)
	}
	if t1["asked_at"] != "2026-10-01T10:00:00Z" || t1["completed_at"] != "2026-10-01T10:00:09Z" || t1["conversation_id"] != "s1" {
		t.Errorf("t1 times/session = %v", t1)
	}
	if t1["model_name"] != "gpt-y" || t1["input_tokens"] != float64(1000) || t1["output_tokens"] != float64(50) {
		t.Errorf("t1 model/tokens = %v %v %v", t1["model_name"], t1["input_tokens"], t1["output_tokens"])
	}
	if u, ok := t1["user_id"]; ok && u != nil {
		t.Errorf("an email user id survived: %v", u)
	}
	sts := t1["statements"].([]any)
	st := sts[0].(map[string]any)
	if len(sts) != 1 || st["sql"] != "select count(*) from orders where week = 39" || st["status"] != "succeeded" ||
		st["rows"] != float64(1) || st["result"] != nil {
		t.Errorf("t1 statements = %v", sts)
	}
	tools := t1["tool_calls"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "search_docs" || tools[0].(map[string]any)["output"] != nil {
		t.Errorf("t1 tool calls = %v", tools)
	}
	if r := t1["reasoning"].([]any); len(r) != 1 || r[0].(map[string]any)["text"] != "Count orders in week 39." {
		t.Errorf("t1 reasoning = %v", r)
	}
	if ex := t1["extra"].(map[string]any); ex["trace_name"] != "analytics-agent" || ex["observations"] != float64(5) {
		t.Errorf("t1 extra = %v", ex)
	}

	t2 := got["t2"]
	if t2["question_text"] != "Revenue by region?" || t2["outcome"] != "failed" || t2["error_message"] != "timeout" ||
		t2["answer_text"] != nil || t2["user_id"] != "u-7" {
		t.Errorf("t2 = %v", t2)
	}
	if len(t2["statements"].([]any)) != 0 || len(t2["tool_calls"].([]any)) != 1 {
		t.Errorf("SHOW TABLES without --sql-tool should be a tool call: %v", t2)
	}

	all := ""
	for _, r := range res.Records {
		all += string(r.Payload)
	}
	for _, leak := range []string{"1204", "secret prompt", "ann@shop.test", "one row per order"} {
		if strings.Contains(all, leak) {
			t.Errorf("%q reached the bundle", leak)
		}
	}
	if len(res.Redactions) != 2 {
		t.Errorf("redactions = %v", res.Redactions)
	}
}

func TestIncludeOptionsAndSQLToolHint(t *testing.T) {
	res, _, _ := run(t, agent.Inputs{"sql-tool": "Warehouse"}, agent.Options{IncludeResults: true, IncludeUsers: true})
	got := rows(t, res)
	if got["t1"]["user_id"] != "ann@shop.test" {
		t.Errorf("--include-users did not keep the user id: %v", got["t1"]["user_id"])
	}
	st := got["t1"]["statements"].([]any)[0].(map[string]any)
	if st["result"] == nil || got["t1"]["tool_calls"].([]any)[0].(map[string]any)["output"] == nil {
		t.Error("--include-results did not keep outputs")
	}
	sts := got["t2"]["statements"].([]any)
	if len(sts) != 1 || sts[0].(map[string]any)["sql"] != "SHOW TABLES" {
		t.Errorf("--sql-tool statement = %v", sts)
	}
	if len(res.Redactions) != 0 {
		t.Errorf("redactions = %v", res.Redactions)
	}
}

func TestSinceTraceNameAndPlanMatchWhatRan(t *testing.T) {
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	res, seen, _ := run(t, nil, agent.Options{Since: since})
	if got := seen[0].URL.Query().Get("fromStartTime"); got != "2026-09-01T00:00:00Z" {
		t.Errorf("fromStartTime = %q", got)
	}
	if l := seen[0].URL.Query().Get("limit"); l != "100" {
		t.Errorf("limit = %q", l)
	}
	a, _ := agent.Get("langfuse")
	plan := a.Plan(agent.Inputs{}, agent.Options{Since: since})
	if len(res.Queries) != 2 || res.Queries[0].Statement != plan[0].Statement ||
		res.Queries[1].Statement != plan[1].Statement || res.Queries[1].Batches != 1 ||
		!strings.HasPrefix(plan[1].Statement, plan[0].Statement+"&cursor=") {
		t.Errorf("plan %v, ran %v", plan, res.Queries)
	}
	if res.Queries[0].Rows != 4 || res.Queries[1].Rows != 4 {
		t.Errorf("rows = %v", res.Queries)
	}

	_, seen, _ = run(t, agent.Inputs{"trace-name": "analytics-agent"}, agent.Options{Since: since})
	q := seen[0].URL.Query()
	if q.Get("fromStartTime") != "" || !strings.Contains(q.Get("filter"), `"column":"traceName"`) ||
		!strings.Contains(q.Get("filter"), `"column":"startTime"`) {
		t.Errorf("filter query = %v", q)
	}
}

func TestMaxTracesStopsPaging(t *testing.T) {
	res, seen, _ := run(t, agent.Inputs{"max-traces": "1"}, agent.Options{})
	got := rows(t, res)
	if len(got) != 1 || got["t1"] == nil || len(seen) != 2 {
		t.Fatalf("turns = %d, pages = %d", len(got), len(seen))
	}
	if ex := got["t1"]["extra"].(map[string]any); ex["observations"] != float64(5) {
		t.Errorf("t1 lost its page-2 observation: %v", ex)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "max-traces") {
		t.Errorf("notes = %v", res.Notes)
	}
}

func TestTextExtraction(t *testing.T) {
	for _, c := range []struct {
		in   string
		role string
		want string
	}{
		{`plain words`, "user", "plain words"},
		{`{"question":"q1","history":[]}`, "user", "q1"},
		{`{"input":{"query":"q2"}}`, "user", "q2"},
		{`[{"role":"user","content":[{"type":"text","text":"q3"}]}]`, "user", "q3"},
		{`{"choices":[{"message":{"role":"assistant","content":"a1"}}]}`, "assistant", "a1"},
		{`{"answer":"a2","sql":"select 1"}`, "assistant", "a2"},
	} {
		if got := text(parseIO(c.in), c.role, 0); got != c.want {
			t.Errorf("text(%s) = %q, want %q", c.in, got, c.want)
		}
	}
	if sqlOf(parseIO(`{"arguments":{"sql":"WITH x AS (select 1) select * from x"}}`), false, 0) == "" {
		t.Error("nested SQL not found")
	}
	if sqlOf(parseIO(`{"query":"orders last week"}`), false, 0) != "" {
		t.Error("a search query was taken for SQL")
	}
}

func TestBackoff(t *testing.T) {
	if backoff("3", 0) != 3*time.Second || backoff("", 2) != 4*time.Second || backoff("", 10) != 30*time.Second {
		t.Error("backoff")
	}
}

// With --anonymize-users the reader keeps real user ids: package anon
// pseudonymises them after collection under a machine-local key, which an
// unkeyed hash here would undercut.
func TestAnonymizeKeepsUserIDsForAnon(t *testing.T) {
	res, _, _ := run(t, nil, agent.Options{AnonymizeUsers: true})
	got := rows(t, res)
	if got["t1"]["user_id"] != "ann@shop.test" || got["t2"]["user_id"] != "u-7" {
		t.Errorf("user ids = %v, %v", got["t1"]["user_id"], got["t2"]["user_id"])
	}
	for _, r := range res.Redactions {
		if r == usersRedaction {
			t.Error("users redaction claimed while ids were kept for anon")
		}
	}
	res, _, _ = run(t, nil, agent.Options{})
	got = rows(t, res)
	if got["t2"]["user_id"] != "u-7" {
		t.Errorf("a non-email user id was dropped: %v", got["t2"]["user_id"])
	}
}

func TestHostMustBeHTTPS(t *testing.T) {
	a, _ := agent.Get("langfuse")
	for _, host := range []string{"http://langfuse.example.com", "https://me:pw@langfuse.example.com", "https://langfuse.example.com?x=1"} {
		in := agent.Inputs{"host": host, "public-key": "pk", "secret-key": "sk"}
		if _, err := agent.CollectAll(context.Background(), a.Collect, in, agent.Options{}); err == nil || !strings.Contains(err.Error(), "--host") {
			t.Errorf("host %q: err = %v", host, err)
		}
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, ok := r.BasicAuth()
		t.Errorf("followed a redirect (basic auth sent: %v)", ok)
	}))
	defer evil.Close()
	srv := httptest.NewServer(http.RedirectHandler(evil.URL+endpoint, http.StatusFound))
	defer srv.Close()
	a, _ := agent.Get("langfuse")
	in := agent.Inputs{"host": srv.URL, "public-key": "pk", "secret-key": "sk"}
	if _, err := agent.CollectAll(context.Background(), a.Collect, in, agent.Options{}); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("err = %v, want a refused redirect", err)
	}
}

func TestRepeatedCursorStops(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		o := obs(fmt.Sprintf("o%d", n), fmt.Sprintf("t%d", n), "", "AGENT", "a", "2026-10-01T10:00:00Z", "", "q?", "a.", nil)
		json.NewEncoder(w).Encode(map[string]any{"data": []any{o}, "meta": map[string]any{"cursor": "same"}})
	}))
	defer srv.Close()
	a, _ := agent.Get("langfuse")
	in := agent.Inputs{"host": srv.URL, "public-key": "pk", "secret-key": "sk", "max-traces": "100"}
	res, err := agent.CollectAll(context.Background(), a.Collect, in, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "repeated") {
		t.Errorf("requests = %d, notes = %v", n, res.Notes)
	}
}
