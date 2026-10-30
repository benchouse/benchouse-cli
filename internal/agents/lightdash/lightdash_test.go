package lightdash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/pgread"
)

// The rows below are shaped like row_to_json output of Lightdash's schema:
// jsonb columns as nested JSON, timestamptz with an offset, the thread as
// to_jsonb(t).
var fixture = map[string][]string{
	promptsStmt: {
		`{"ai_prompt_uuid":"p1","ai_thread_uuid":"t1","created_at":"2026-10-02T12:00:00.123+00:00",
		  "created_by_user_uuid":"u1","prompt":"Revenue last week?","response":"Revenue was 1.2M.",
		  "error_message":null,"responded_at":"2026-10-02T12:00:09+00:00","retried_at":null,
		  "human_score":-1,"human_feedback":"wrong week","metric_query":null,"hidden":false,
		  "needs_user_input":false,"execution_mode":"standard",
		  "model_config":{"modelName":"claude-sonnet-5","modelProvider":"anthropic"},
		  "token_usage":{"totalTokens":5120},"turn_position":1,
		  "thread":{"ai_thread_uuid":"t1","agent_uuid":"a1","created_from":"web_app","title":"Revenue"}}`,
		`{"ai_prompt_uuid":"p2","ai_thread_uuid":"t1","created_at":"2026-10-02T12:05:00+00:00",
		  "created_by_user_uuid":"u1","prompt":"Top customers by orders?","response":null,
		  "error_message":"warehouse timeout","responded_at":null,"turn_position":2,
		  "thread":{"ai_thread_uuid":"t1","agent_uuid":"a1","created_from":"slack"}}`,
		`{"ai_prompt_uuid":"p0","ai_thread_uuid":"t0","created_at":"2026-09-01T08:00:00+00:00",
		  "created_by_user_uuid":"u2","prompt":"Orders by region","response":"Here is the chart.",
		  "metric_query":{"exploreName":"orders","dimensions":["orders_region"],"metrics":["orders_count"]},
		  "turn_position":1,"thread":{"ai_thread_uuid":"t0","agent_uuid":null,"created_from":"slack"}}`,
		`{"ai_prompt_uuid":"ph","ai_thread_uuid":"t1","created_at":"2026-10-02T12:06:00+00:00",
		  "created_by_user_uuid":"u1","prompt":"(migration prompt)","response":"ok","hidden":true,"turn_position":3,
		  "thread":{"ai_thread_uuid":"t1","agent_uuid":"a1","created_from":"web_app"}}`,
	},
	toolCallsStmt: {
		`{"ai_prompt_uuid":"p1","tool_call_id":"c1","tool_name":"findFields","tool_args":{"table":"orders"},"created_at":"2026-10-02T12:00:01+00:00"}`,
		`{"ai_prompt_uuid":"p1","tool_call_id":"c2","tool_name":"runQuery","tool_args":{"queryConfig":{"exploreName":"orders","metrics":["orders_revenue"]}},"created_at":"2026-10-02T12:00:03+00:00"}`,
		`{"ai_prompt_uuid":"p1","tool_call_id":"c3","tool_name":"runSql","tool_args":{"sql":"SELECT sum(amount) FROM orders","limit":500},"created_at":"2026-10-02T12:00:05+00:00"}`,
	},
	toolResultsStmt: {
		`{"ai_prompt_uuid":"p1","tool_call_id":"c1","tool_name":"findFields","result":"orders_revenue: sum of amount","metadata":{"status":"success"},"created_at":"2026-10-02T12:00:02+00:00"}`,
		`{"ai_prompt_uuid":"p1","tool_call_id":"c2","tool_name":"runQuery","result":"week,revenue\n39,1200000","metadata":{"status":"success","queryUuid":"q1"},"created_at":"2026-10-02T12:00:04+00:00"}`,
		`{"ai_prompt_uuid":"p1","tool_call_id":"c3","tool_name":"runSql","result":"sum\n1200000","metadata":{"status":"success","rowCount":1},"created_at":"2026-10-02T12:00:06+00:00"}`,
	},
	compiledStmt: {
		`{"ai_prompt_uuid":"p1","tool_call_id":"c2","query_uuid":"q1","created_at":"2026-10-02T12:00:03.5+00:00",
		  "compiled_sql":"SELECT DATE_TRUNC('week', o.created_at) AS week, SUM(o.amount) AS revenue FROM orders o GROUP BY 1",
		  "status":"ready","error":null,"total_row_count":1,"warehouse_execution_time_ms":120}`,
	},
	reasoningStmt: {
		`{"ai_prompt_uuid":"p1","reasoning_id":"r1","text":"Revenue is a metric on orders.","created_at":"2026-10-02T12:00:00.5+00:00"}`,
	},
	agentsStmt: {`{"ai_agent_uuid":"a1","name":"Sales analyst"}`},
	usersStmt:  {`{"user_uuid":"u1","first_name":"Ann","last_name":"Lee","email":"ann@shop.test"}`},
}

// fakeDB serves a fixture (statement → rows) the way Postgres would answer
// Collect's statements: the threads chosen by latest prompt in the window,
// newest first, with keyset paging and a LIMIT; batched statements filtered
// to the page's thread ids and the window. It records what ran.
type fakeDB struct {
	t       *testing.T
	data    map[string][]string
	missing map[string]bool // statements that fail, as on an older Lightdash
	ran     []string
}

type fakePrompt struct {
	UUID, Thread string
	At           time.Time
}

func (f *fakeDB) prompts() map[string]fakePrompt {
	out := map[string]fakePrompt{}
	for _, r := range f.data[promptsStmt] {
		var p struct {
			UUID      string `json:"ai_prompt_uuid"`
			Thread    string `json:"ai_thread_uuid"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal([]byte(r), &p); err != nil {
			f.t.Fatal(err)
		}
		at, _ := pgread.Time(p.CreatedAt)
		out[p.UUID] = fakePrompt{p.UUID, p.Thread, at}
	}
	return out
}

// base is the fixture statement a batched statement was made from.
func base(stmt string, args []any) string {
	for _, s := range []string{promptsStmt, toolCallsStmt, toolResultsStmt, compiledStmt, reasoningStmt} {
		if len(args) > 1 && stmt == pgread.Batched(s, 2, len(args)-1) {
			return s
		}
	}
	return stmt
}

func (f *fakeDB) Rows(ctx context.Context, stmt string, args ...any) ([]json.RawMessage, error) {
	var out []json.RawMessage
	err := f.Each(ctx, stmt, args, func(r json.RawMessage) error { out = append(out, r); return nil })
	return out, err
}

func (f *fakeDB) Each(_ context.Context, stmt string, args []any, fn func(json.RawMessage) error) error {
	stmt = base(stmt, args)
	f.ran = append(f.ran, stmt)
	if f.missing[stmt] {
		return errors.New(`relation does not exist`)
	}
	if strings.Contains(stmt, "$1") && len(args) < 1 {
		f.t.Errorf("windowed statement without its bound: %s", stmt)
	}
	prompts := f.prompts()
	switch stmt {
	case threadsStmt, threadCountStmt:
		since := args[0].(time.Time)
		last := map[string]time.Time{}
		for _, p := range prompts {
			if p.At.After(since) && p.At.After(last[p.Thread]) {
				last[p.Thread] = p.At
			}
		}
		if stmt == threadCountStmt {
			return fn(json.RawMessage(fmt.Sprintf(`{"n":%d}`, len(last))))
		}
		type th struct {
			id string
			at time.Time
		}
		var ths []th
		curAt, _ := pgread.Time(fmt.Sprint(args[1]))
		inf := args[1] == "infinity"
		for id, at := range last {
			if inf || at.Before(curAt) || (at.Equal(curAt) && id < args[2].(string)) {
				ths = append(ths, th{id, at})
			}
		}
		sort.Slice(ths, func(i, j int) bool {
			if !ths[i].at.Equal(ths[j].at) {
				return ths[i].at.After(ths[j].at)
			}
			return ths[i].id > ths[j].id
		})
		if n := args[3].(int); len(ths) > n {
			ths = ths[:n]
		}
		for _, x := range ths {
			if err := fn(json.RawMessage(fmt.Sprintf(`{"id":%q,"last_at":%q}`, x.id, x.at.Format(time.RFC3339Nano)))); err != nil {
				return err
			}
		}
		return nil
	}
	rows, ok := f.data[stmt]
	if !ok {
		f.t.Errorf("unexpected statement %s", stmt)
	}
	batched := stmt != agentsStmt && stmt != usersStmt
	ids := map[string]bool{}
	if batched {
		if len(args) < 2 || len(args) > agent.BatchSize+1 {
			f.t.Errorf("batch of %d ids", len(args)-1)
		}
		for _, id := range args[1:] {
			ids[id.(string)] = true
		}
	}
	for _, r := range rows {
		if batched {
			var k struct {
				Prompt string `json:"ai_prompt_uuid"`
			}
			json.Unmarshal([]byte(r), &k)
			p := prompts[k.Prompt]
			if !ids[p.Thread] || !p.At.After(args[0].(time.Time)) {
				continue
			}
		}
		if err := fn(json.RawMessage(r)); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeDB) Close() error { return nil }

func run(t *testing.T, opts agent.Options, missing ...string) (agent.Collected, *fakeDB) {
	t.Helper()
	return runOn(t, fixture, opts, missing...)
}

func runOn(t *testing.T, data map[string][]string, opts agent.Options, missing ...string) (agent.Collected, *fakeDB) {
	t.Helper()
	f := &fakeDB{t: t, data: data, missing: map[string]bool{}}
	for _, m := range missing {
		f.missing[m] = true
	}
	openDB = func(agent.Inputs) (pgread.Querier, error) { return f, nil }
	t.Cleanup(func() { openDB = func(in agent.Inputs) (pgread.Querier, error) { return pgread.Open(in) } })
	a, err := agent.Get("lightdash")
	if err != nil {
		t.Fatal(err)
	}
	in := agent.Inputs{"database": "lightdash", "user": "reader"}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	res, err := agent.CollectAll(context.Background(), a.Collect, in, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res, f
}

type payload struct {
	Contract string         `json:"contract"`
	Columns  []string       `json:"columns"`
	Row      map[string]any `json:"row"`
}

func rows(t *testing.T, res agent.Collected) map[string]payload {
	t.Helper()
	out := map[string]payload{}
	for _, r := range res.Records {
		if r.SourceKind != "custom_sql" || r.RecordKind != "sql_turn" || r.FetchMethod != "sql:ai_prompt" {
			t.Errorf("record = %+v", r)
		}
		var p payload
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out[r.NativeIDs["turn_id"]] = p
	}
	return out
}

func TestTurnsMapQuestionAnswerSQLAndFeedback(t *testing.T) {
	res, _ := run(t, agent.Options{})
	got := rows(t, res)
	if len(got) != 3 {
		t.Fatalf("turns = %d, want 3 (the hidden prompt left out)", len(got))
	}
	p1 := got["p1"].Row
	if p1["question_text"] != "Revenue last week?" || p1["answer_text"] != "Revenue was 1.2M." {
		t.Errorf("p1 = %v", p1)
	}
	if p1["asked_at"] != "2026-10-02T12:00:00.123Z" || p1["completed_at"] != "2026-10-02T12:00:09Z" {
		t.Errorf("timestamps = %v / %v", p1["asked_at"], p1["completed_at"])
	}
	if p1["model_name"] != "claude-sonnet-5" || p1["client_interface"] != "web" || p1["user_id"] != "u1" ||
		p1["conversation_id"] != "t1" || p1["position"] != float64(1) || p1["outcome"] != "completed" {
		t.Errorf("p1 = %v", p1)
	}
	sts := p1["statements"].([]any)
	if len(sts) != 2 {
		t.Fatalf("statements = %v", sts)
	}
	compiled := sts[0].(map[string]any)
	if !strings.Contains(compiled["sql"].(string), "SUM(o.amount)") || compiled["status"] != "succeeded" || compiled["rows"] != float64(1) {
		t.Errorf("compiled statement = %v", compiled)
	}
	runSQL := sts[1].(map[string]any)
	if runSQL["sql"] != "SELECT sum(amount) FROM orders" || runSQL["rows"] != float64(1) || runSQL["result"] != nil {
		t.Errorf("runSql statement = %v", runSQL)
	}
	tcs := p1["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Fatalf("tool_calls = %v", tcs)
	}
	if out := tcs[0].(map[string]any)["output"]; out != "orders_revenue: sum of amount" {
		t.Errorf("semantic-layer lookup was redacted: %v", out)
	}
	rq := tcs[1].(map[string]any)
	if s, _ := json.Marshal(rq); strings.Contains(string(s), "1200000") || !strings.Contains(string(s), "orders_revenue") {
		t.Errorf("runQuery tool call = %s", s)
	}
	if r := p1["reasoning"].([]any); len(r) != 1 {
		t.Errorf("reasoning = %v", r)
	}
	extra := p1["extra"].(map[string]any)
	fb := extra["feedback"].(map[string]any)
	if fb["score"] != float64(-1) || fb["comment"] != "wrong week" || extra["agent_name"] != "Sales analyst" || extra["total_tokens"] != float64(5120) {
		t.Errorf("extra = %v", extra)
	}
	if _, ok := extra["declined"]; ok {
		t.Error("declined is not derivable from Lightdash and must not be set")
	}

	p2 := got["p2"].Row
	if p2["outcome"] != "failed" || p2["error_message"] != "warehouse timeout" || p2["answer_text"] != nil || p2["client_interface"] != "slack" {
		t.Errorf("p2 = %v", p2)
	}
	p0 := got["p0"].Row
	if tc := p0["tool_calls"].([]any); len(tc) != 1 || tc[0].(map[string]any)["name"] != "ai_prompt.metric_query" {
		t.Errorf("legacy copilot metric query = %v", p0["tool_calls"])
	}
	for _, c := range got["p1"].Columns {
		if c == "user_email" || c == "user_name" {
			t.Error("user columns declared without --include-users")
		}
	}
	if len(res.Redactions) != 1 || len(res.Notes) != 1 {
		t.Errorf("redactions = %v, notes = %v", res.Redactions, res.Notes)
	}
}

func TestIncludeResultsAndUsers(t *testing.T) {
	res, _ := run(t, agent.Options{IncludeResults: true, IncludeUsers: true})
	p1 := rows(t, res)["p1"].Row
	if p1["user_email"] != "ann@shop.test" || p1["user_name"] != "Ann Lee" {
		t.Errorf("users = %v / %v", p1["user_email"], p1["user_name"])
	}
	sts := p1["statements"].([]any)
	if sts[1].(map[string]any)["result"] != "sum\n1200000" {
		t.Errorf("--include-results did not keep the runSql result: %v", sts[1])
	}
	if out := p1["tool_calls"].([]any)[1].(map[string]any)["output"]; out != "week,revenue\n39,1200000" {
		t.Errorf("--include-results did not keep the runQuery result: %v", out)
	}
	if len(res.Redactions) != 0 {
		t.Errorf("redactions = %v", res.Redactions)
	}
}

func TestSinceBoundsTheWindow(t *testing.T) {
	res, _ := run(t, agent.Options{Since: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)})
	got := rows(t, res)
	if len(got) != 2 || got["p0"].Row != nil {
		t.Errorf("turns = %v", got)
	}
}

func TestOlderLightdashDegrades(t *testing.T) {
	res, _ := run(t, agent.Options{}, reasoningStmt, compiledStmt)
	if len(rows(t, res)) != 3 {
		t.Error("turns lost when optional tables are missing")
	}
	if len(res.Notes) != 3 { // two skipped tables + the hidden prompt
		t.Errorf("notes = %v", res.Notes)
	}
}

func TestPromptsAreRequired(t *testing.T) {
	f := &fakeDB{t: t, data: fixture, missing: map[string]bool{promptsStmt: true}}
	openDB = func(agent.Inputs) (pgread.Querier, error) { return f, nil }
	defer func() { openDB = func(in agent.Inputs) (pgread.Querier, error) { return pgread.Open(in) } }()
	if _, err := agent.CollectAll(context.Background(), collect, agent.Inputs{}, agent.Options{}); err == nil {
		t.Error("a database without ai_prompt should fail")
	}
}

// distinct is ran in first-run order, each batched statement once, in its
// planned form.
func distinct(ran []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range ran {
		s = pgread.Planned(s, "thread")
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func TestPlanListsWhatCollectRuns(t *testing.T) {
	for _, opts := range []agent.Options{{}, {IncludeUsers: true}, {MaxConversations: 2000}} {
		res, f := run(t, opts)
		a, _ := agent.Get("lightdash")
		plan := a.Plan(agent.Inputs{}, opts)
		ran := distinct(f.ran)
		if len(plan) != len(ran) || len(plan) != len(res.Queries) {
			t.Fatalf("plan %d, ran %d, queries %d", len(plan), len(ran), len(res.Queries))
		}
		for i := range plan {
			if plan[i].Statement != ran[i] || res.Queries[i].Statement != ran[i] {
				t.Errorf("statement %d differs from the plan:\n%s\n%s", i, plan[i].Statement, ran[i])
			}
		}
		directory := false
		for _, q := range plan {
			directory = directory || strings.Contains(q.Statement, "emails")
		}
		if directory != opts.IncludeUsers {
			t.Errorf("directory planned = %v with IncludeUsers = %v", directory, opts.IncludeUsers)
		}
	}
}

// bigFixture has threads threads, thread i with two prompts at minute i
// (so higher i is more recent), each prompt with one tool call.
func bigFixture(threads int) map[string][]string {
	data := map[string][]string{toolResultsStmt: nil, compiledStmt: nil, reasoningStmt: nil, agentsStmt: nil}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < threads; i++ {
		for j := 0; j < 2; j++ {
			at := start.Add(time.Duration(i)*time.Minute + time.Duration(j)*time.Second).Format(time.RFC3339)
			data[promptsStmt] = append(data[promptsStmt], fmt.Sprintf(
				`{"ai_prompt_uuid":"p%d-%d","ai_thread_uuid":"t%04d","created_at":%q,"prompt":"q","response":"a","turn_position":%d,"thread":{"created_from":"web_app"}}`,
				i, j, i, at, j+1))
			data[toolCallsStmt] = append(data[toolCallsStmt], fmt.Sprintf(
				`{"ai_prompt_uuid":"p%d-%d","tool_call_id":"c%d-%d","tool_name":"findFields","tool_args":{},"created_at":%q}`, i, j, i, j, at))
		}
	}
	return data
}

func TestMaxConversationsKeepsTheMostRecentThreadsAndOnlyTheirSteps(t *testing.T) {
	res, f := runOn(t, bigFixture(450), agent.Options{MaxConversations: 250})
	got := rows(t, res)
	threads := map[string]bool{}
	for _, p := range got {
		threads[p.Row["conversation_id"].(string)] = true
		if tc := p.Row["tool_calls"].([]any); len(tc) != 1 {
			t.Errorf("tool calls = %v", tc)
		}
	}
	if len(got) != 500 || len(threads) != 250 {
		t.Fatalf("turns %d in %d threads", len(got), len(threads))
	}
	for i := 200; i < 450; i++ { // t0200..t0449 are the most recent
		if !threads[fmt.Sprintf("t%04d", i)] {
			t.Fatalf("thread t%04d missing", i)
		}
	}
	if !res.Sampled || res.Conversations != 250 || res.InWindow == nil || *res.InWindow != 450 {
		t.Errorf("sample: sampled %v, %d of %v", res.Sampled, res.Conversations, res.InWindow)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "450 threads in the window: the 250 most recent were read") {
		t.Errorf("notes = %v", res.Notes)
	}
	q := map[string]bundle.Query{}
	for _, x := range res.Queries {
		q[x.Statement] = x
	}
	if p := q[pgread.Planned(promptsStmt, "thread")]; p.Batches != 2 || p.Rows != 500 {
		t.Errorf("prompts query = %+v", p)
	}
	if p := q[threadsStmt]; p.Batches != 2 || p.Rows != 250 {
		t.Errorf("threads query = %+v", p)
	}
	for i, s := range f.ran {
		if strings.Contains(s, "IN ($2") {
			t.Errorf("statement %d was not a batch of a known statement", i)
		}
	}
}

func TestNoCapReadsEveryThreadPageByPage(t *testing.T) {
	res, _ := runOn(t, bigFixture(450), agent.Options{})
	if got := rows(t, res); len(got) != 900 || res.Conversations != 450 || res.InWindow != nil {
		t.Errorf("turns %d, conversations %d, in window %v", len(got), res.Conversations, res.InWindow)
	}
	for _, x := range res.Queries {
		if x.Statement == threadsStmt && x.Batches != 3 {
			t.Errorf("thread pages = %d, want 3", x.Batches)
		}
	}
}
