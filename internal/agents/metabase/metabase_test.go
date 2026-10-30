package metabase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/pgread"
)

// jsonText renders v as Metabase stores data and usage on Postgres: a text
// column holding JSON, which row_to_json renders as a JSON string.
func jsonText(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The rows are shaped like row_to_json output of Metabase's schema: integer
// ids, timestamptz with an offset, data and usage as text.
func fixture() map[string][]string {
	v2Answer := `[
	  {"type":"text","text":"Let me check."},
	  {"type":"tool-search","toolCallId":"s1","state":"output-available","input":{"query":"orders"},
	   "output":{"output":"orders: one row per order"}},
	  {"type":"tool-create_sql_query","toolCallId":"q1","state":"output-available",
	   "input":{"database_id":1,"sql_query":"SELECT 1","title":"Orders"},
	   "output":{"output":"Query created","structured_output":{"query-id":"abc","query-content":"SELECT count(*) FROM orders WHERE created_at >= date '2026-09-01'","database":1}}},
	  {"type":"tool-run_query","toolCallId":"r1","state":"output-available","input":{"query_id":"abc"},
	   "output":{"output":"| count |\n| 4211 |"}},
	  {"type":"tool-construct_notebook_query","toolCallId":"n1","state":"output-available","input":{"source":"orders"},
	   "output":{"output":"done","structured_output":{"query-id":"def","query":{"source-table":5,"aggregation":[["count"]]},"chart-type":"bar"}}},
	  {"type":"text","text":"There were 4,211 orders."}]`
	v1Answer := `[
	  {"type":"text","text":"Revenue in 2025 was 9.1M."},
	  {"type":"tool-input","id":"t1","function":"create_sql_query","arguments":{"sql_query":"SELECT sum(total) FROM orders WHERE year = 2025"}},
	  {"type":"tool-output","id":"t1","result":{"structured-output":{"query-id":"x"}}}]`
	return map[string][]string{
		messagesStmt: {
			`{"id":10,"conversation_id":"c0","created_at":"2026-08-01T09:00:00+00:00","role":"user","profile_id":null,
			  "data":` + jsonText(`[{"role":"user","content":"Revenue in 2025"}]`) + `,"usage":null,"conversation_user_id":8,"role_position":1}`,
			`{"id":11,"conversation_id":"c0","created_at":"2026-08-01T09:00:01+00:00","role":"assistant","profile_id":"internal",
			  "data":` + jsonText(v1Answer) + `,"usage":` + jsonText(`{"gpt-5":{"prompt":300,"completion":40}}`) + `,"conversation_user_id":8,"role_position":1}`,
			`{"id":1,"conversation_id":"c1","created_at":"2026-10-02T12:00:00+00:00","role":"user","profile_id":null,"data_version":2,
			  "data":` + jsonText(`[{"type":"text","text":"How many orders last month?"}]`) + `,"usage":null,"user_id":7,
			  "conversation_user_id":7,"finished":null,"error":null,"forked_from_message_id":null,"role_position":1}`,
			`{"id":2,"conversation_id":"c1","created_at":"2026-10-02T12:00:01+00:00","role":"assistant","profile_id":"internal","data_version":2,
			  "data":` + jsonText(v2Answer) + `,"usage":` + jsonText(`{"claude-sonnet-5":{"prompt":1000,"completion":200},"claude-haiku-5":{"prompt":50,"completion":5}}`) + `,
			  "user_id":7,"conversation_user_id":7,"finished":true,"error":null,"role_position":1}`,
			`{"id":3,"conversation_id":"c1","created_at":"2026-10-02T12:01:00+00:00","role":"user","data_version":2,
			  "data":` + jsonText(`[{"type":"text","text":"And by region?"}]`) + `,"user_id":7,"conversation_user_id":7,"finished":null,"role_position":2}`,
			`{"id":4,"conversation_id":"c1","created_at":"2026-10-02T12:01:01+00:00","role":"assistant","profile_id":"internal","data_version":2,
			  "data":"[]","usage":null,"user_id":7,"conversation_user_id":7,"finished":false,
			  "error":` + jsonText(`{"message":"Rate limited"}`) + `,"role_position":2}`,
			`{"id":20,"conversation_id":"c2","created_at":"2026-10-03T08:00:00+00:00","role":"user","data_version":2,
			  "data":` + jsonText(`[{"type":"text","text":"How many orders last month?"}]`) + `,"user_id":9,"conversation_user_id":9,"forked_from_message_id":1,"role_position":1}`,
			`{"id":21,"conversation_id":"c2","created_at":"2026-10-03T08:00:00+00:00","role":"assistant","data_version":2,
			  "data":` + jsonText(`[{"type":"text","text":"There were 4,211 orders."}]`) + `,"user_id":9,"conversation_user_id":9,"forked_from_message_id":2,"role_position":1}`,
		},
		feedbackStmt: {
			`{"id":1,"message_id":2,"user_id":7,"positive":false,"issue_type":"not-factual","freeform_feedback":"wrong month",
			  "created_at":"2026-10-02T12:02:00+00:00","updated_at":"2026-10-02T12:02:00+00:00"}`,
		},
		usersStmt: {`{"id":7,"first_name":"Ann","last_name":"Lee","email":"ann@shop.test"}`},
	}
}

// fakeDB serves a fixture (statement → rows) the way Postgres would answer
// Collect's statements: conversations chosen by latest message in the
// window, newest first, with keyset paging and a LIMIT; batched statements
// filtered to the page's conversation ids and the window. It records what
// ran.
type fakeDB struct {
	t       *testing.T
	data    map[string][]string
	missing map[string]bool
	ran     []string
}

type fakeMessage struct {
	Conversation string
	At           time.Time
}

// messages are the fixture's messages by id.
func (f *fakeDB) messages() map[string]fakeMessage {
	out := map[string]fakeMessage{}
	for _, r := range f.data[messagesStmt] {
		var m struct {
			ID           json.Number `json:"id"`
			Conversation string      `json:"conversation_id"`
			CreatedAt    string      `json:"created_at"`
		}
		if err := json.Unmarshal([]byte(r), &m); err != nil {
			f.t.Fatalf("fixture row: %v\n%s", err, r)
		}
		at, _ := pgread.Time(m.CreatedAt)
		out[m.ID.String()] = fakeMessage{m.Conversation, at}
	}
	return out
}

// base is the fixture statement a batched statement was made from.
func base(stmt string, args []any) string {
	for _, s := range []string{messagesStmt, feedbackStmt} {
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
		return errors.New("relation does not exist")
	}
	if strings.Contains(stmt, "$1") && len(args) < 1 {
		f.t.Errorf("windowed statement without its bound: %s", stmt)
	}
	msgs := f.messages()
	switch stmt {
	case conversationsStmt, conversationCountStmt:
		since := args[0].(time.Time)
		last := map[string]time.Time{}
		for _, m := range msgs {
			if m.At.After(since) && m.At.After(last[m.Conversation]) {
				last[m.Conversation] = m.At
			}
		}
		if stmt == conversationCountStmt {
			return fn(json.RawMessage(fmt.Sprintf(`{"n":%d}`, len(last))))
		}
		type conv struct {
			id string
			at time.Time
		}
		var cs []conv
		curAt, _ := pgread.Time(fmt.Sprint(args[1]))
		inf := args[1] == "infinity"
		for id, at := range last {
			if inf || at.Before(curAt) || (at.Equal(curAt) && id < args[2].(string)) {
				cs = append(cs, conv{id, at})
			}
		}
		sort.Slice(cs, func(i, j int) bool {
			if !cs[i].at.Equal(cs[j].at) {
				return cs[i].at.After(cs[j].at)
			}
			return cs[i].id > cs[j].id
		})
		if n := args[3].(int); len(cs) > n {
			cs = cs[:n]
		}
		for _, c := range cs {
			if err := fn(json.RawMessage(fmt.Sprintf(`{"id":%q,"last_at":%q}`, c.id, c.at.Format(time.RFC3339Nano)))); err != nil {
				return err
			}
		}
		return nil
	}
	rows, ok := f.data[stmt]
	if !ok {
		f.t.Errorf("unexpected statement %s", stmt)
	}
	batched := stmt != usersStmt
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
				ID        json.Number `json:"id"`
				MessageID json.Number `json:"message_id"`
			}
			json.Unmarshal([]byte(r), &k)
			id := k.ID
			if stmt == feedbackStmt {
				id = k.MessageID
			}
			m := msgs[id.String()]
			if !ids[m.Conversation] || !m.At.After(args[0].(time.Time)) {
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
	return runOn(t, fixture(), opts, missing...)
}

func runOn(t *testing.T, data map[string][]string, opts agent.Options, missing ...string) (agent.Collected, *fakeDB) {
	t.Helper()
	f := &fakeDB{t: t, data: data, missing: map[string]bool{}}
	for _, m := range missing {
		f.missing[m] = true
	}
	openDB = func(agent.Inputs) (pgread.Querier, error) { return f, nil }
	t.Cleanup(func() { openDB = func(in agent.Inputs) (pgread.Querier, error) { return pgread.Open(in) } })
	a, err := agent.Get("metabase")
	if err != nil {
		t.Fatal(err)
	}
	in := agent.Inputs{"database": "metabase", "user": "reader"}
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
	Columns []string       `json:"columns"`
	Row     map[string]any `json:"row"`
}

func rows(t *testing.T, res agent.Collected) map[string]payload {
	t.Helper()
	out := map[string]payload{}
	for _, r := range res.Records {
		if r.SourceKind != "custom_sql" || r.RecordKind != "sql_turn" || r.FetchMethod != "sql:metabot_message" {
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

func TestTurnsMapV2Messages(t *testing.T) {
	res, _ := run(t, agent.Options{})
	got := rows(t, res)
	if len(got) != 3 {
		t.Fatalf("turns = %d, want 3 (fork copies left out): %v", len(got), got)
	}
	r := got["1"].Row
	if r["question_text"] != "How many orders last month?" || r["answer_text"] != "Let me check.\n\nThere were 4,211 orders." {
		t.Errorf("question/answer = %q / %q", r["question_text"], r["answer_text"])
	}
	if r["outcome"] != "completed" || r["conversation_id"] != "c1" || r["position"] != float64(1) ||
		r["user_id"] != "7" || r["client_interface"] != "web" || r["asked_at"] != "2026-10-02T12:00:00Z" {
		t.Errorf("turn 1 = %v", r)
	}
	if r["model_name"] != "claude-sonnet-5" || r["input_tokens"] != float64(1050) || r["output_tokens"] != float64(205) {
		t.Errorf("model/tokens = %v %v %v", r["model_name"], r["input_tokens"], r["output_tokens"])
	}
	sts := r["statements"].([]any)
	if len(sts) != 1 || sts[0].(map[string]any)["sql"] != "SELECT count(*) FROM orders WHERE created_at >= date '2026-09-01'" {
		t.Errorf("statements = %v (query-content must win over the input)", sts)
	}
	if _, ok := sts[0].(map[string]any)["status"]; ok {
		t.Error("a generated query was given an execution status the data does not record")
	}
	tcs := r["tool_calls"].([]any)
	if len(tcs) != 3 {
		t.Fatalf("tool_calls = %v", tcs)
	}
	all, _ := json.Marshal(tcs)
	if strings.Contains(string(all), "4211") {
		t.Errorf("run_query result survived: %s", all)
	}
	if !strings.Contains(string(all), "one row per order") {
		t.Errorf("search output was redacted: %s", all)
	}
	if !strings.Contains(string(all), `"source-table":5`) {
		t.Errorf("notebook MBQL was lost: %s", all)
	}
	extra := r["extra"].(map[string]any)
	fb := extra["feedback"].([]any)[0].(map[string]any)
	if fb["score"] != float64(-1) || fb["issue_type"] != "not-factual" || fb["comment"] != "wrong month" {
		t.Errorf("feedback = %v", fb)
	}
	if _, ok := extra["declined"]; ok {
		t.Error("declined is not derivable from Metabot and must not be set")
	}

	r3 := got["3"].Row
	if r3["outcome"] != "failed" || r3["error_message"] != "Rate limited" || r3["answer_text"] != nil || r3["position"] != float64(2) {
		t.Errorf("turn 3 = %v", r3)
	}
	for _, c := range got["1"].Columns {
		if c == "user_email" || c == "user_name" {
			t.Error("user columns declared without --include-users")
		}
	}
	if len(res.Redactions) != 1 || len(res.Notes) != 1 { // the fork copy
		t.Errorf("redactions = %v, notes = %v", res.Redactions, res.Notes)
	}
}

func TestV1MessagesAreConverted(t *testing.T) {
	res, _ := run(t, agent.Options{})
	r := rows(t, res)["10"].Row
	if r["question_text"] != "Revenue in 2025" || r["answer_text"] != "Revenue in 2025 was 9.1M." || r["user_id"] != "8" {
		t.Errorf("v1 turn = %v", r)
	}
	if r["outcome"] != "completed" || r["model_name"] != "gpt-5" {
		t.Errorf("v1 outcome/model = %v / %v", r["outcome"], r["model_name"])
	}
	sts := r["statements"].([]any)
	if len(sts) != 1 || sts[0].(map[string]any)["sql"] != "SELECT sum(total) FROM orders WHERE year = 2025" {
		t.Errorf("v1 statements = %v (falls back to sql_query input)", sts)
	}
}

func TestIncludeResultsAndUsers(t *testing.T) {
	res, _ := run(t, agent.Options{IncludeResults: true, IncludeUsers: true})
	r := rows(t, res)["1"].Row
	if r["user_email"] != "ann@shop.test" || r["user_name"] != "Ann Lee" {
		t.Errorf("users = %v / %v", r["user_email"], r["user_name"])
	}
	all, _ := json.Marshal(r["tool_calls"])
	if !strings.Contains(string(all), "4211") {
		t.Errorf("--include-results did not keep the run_query output: %s", all)
	}
	if len(res.Redactions) != 0 {
		t.Errorf("redactions = %v", res.Redactions)
	}
}

func TestSinceBoundsTheWindow(t *testing.T) {
	res, _ := run(t, agent.Options{Since: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)})
	got := rows(t, res)
	if len(got) != 2 || got["10"].Row != nil {
		t.Errorf("turns = %v", got)
	}
}

func TestOlderMetabaseDegrades(t *testing.T) {
	res, _ := run(t, agent.Options{}, feedbackStmt)
	if len(rows(t, res)) != 3 || len(res.Notes) != 2 {
		t.Errorf("turns = %d, notes = %v", len(rows(t, res)), res.Notes)
	}
}

func TestMessagesAreRequired(t *testing.T) {
	f := &fakeDB{t: t, data: fixture(), missing: map[string]bool{messagesStmt: true}}
	openDB = func(agent.Inputs) (pgread.Querier, error) { return f, nil }
	defer func() { openDB = func(in agent.Inputs) (pgread.Querier, error) { return pgread.Open(in) } }()
	if _, err := agent.CollectAll(context.Background(), collect, agent.Inputs{}, agent.Options{}); err == nil {
		t.Error("a database without metabot_message should fail")
	}
}

// distinct is ran in first-run order, each batched statement once, in its
// planned form.
func distinct(ran []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range ran {
		s = pgread.Planned(s, "conversation")
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
		a, _ := agent.Get("metabase")
		plan := a.Plan(agent.Inputs{}, opts)
		ran := distinct(f.ran)
		if len(plan) != len(ran) || len(plan) != len(res.Queries) {
			t.Fatalf("plan %d, ran %d, queries %d", len(plan), len(ran), len(res.Queries))
		}
		directory := false
		for i := range plan {
			if plan[i].Statement != ran[i] || res.Queries[i].Statement != ran[i] {
				t.Errorf("statement %d differs from the plan:\n%s\n%s", i, plan[i].Statement, ran[i])
			}
			directory = directory || strings.Contains(plan[i].Statement, "core_user")
		}
		if directory != opts.IncludeUsers {
			t.Errorf("directory planned = %v with IncludeUsers = %v", directory, opts.IncludeUsers)
		}
	}
}

// bigFixture has convs conversations, conversation i with one question and
// one answer at minute i (higher i is more recent), and feedback on every
// answer.
func bigFixture(convs int) map[string][]string {
	data := map[string][]string{}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < convs; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		q, a := 2*i+1, 2*i+2
		data[messagesStmt] = append(data[messagesStmt],
			fmt.Sprintf(`{"id":%d,"conversation_id":"c%04d","created_at":%q,"role":"user","data_version":2,"data":%s,"role_position":1}`,
				q, i, at.Format(time.RFC3339), jsonText(`[{"type":"text","text":"q"}]`)),
			fmt.Sprintf(`{"id":%d,"conversation_id":"c%04d","created_at":%q,"role":"assistant","data_version":2,"data":%s,"role_position":1}`,
				a, i, at.Add(time.Second).Format(time.RFC3339), jsonText(`[{"type":"text","text":"a"}]`)))
		data[feedbackStmt] = append(data[feedbackStmt],
			fmt.Sprintf(`{"id":%d,"message_id":%d,"positive":true,"created_at":%q}`, i, a, at.Format(time.RFC3339)))
	}
	return data
}

func TestMaxConversationsKeepsTheMostRecentAndOnlyTheirMessages(t *testing.T) {
	res, _ := runOn(t, bigFixture(450), agent.Options{MaxConversations: 250})
	got := rows(t, res)
	convs := map[string]bool{}
	for _, p := range got {
		convs[p.Row["conversation_id"].(string)] = true
		if p.Row["answer_text"] != "a" {
			t.Errorf("answer = %v", p.Row["answer_text"])
		}
		if _, ok := p.Row["extra"].(map[string]any)["feedback"]; !ok {
			t.Errorf("feedback missing: %v", p.Row["extra"])
		}
	}
	if len(got) != 250 || len(convs) != 250 {
		t.Fatalf("turns %d in %d conversations", len(got), len(convs))
	}
	for i := 200; i < 450; i++ {
		if !convs[fmt.Sprintf("c%04d", i)] {
			t.Fatalf("conversation c%04d missing", i)
		}
	}
	if !res.Sampled || res.Conversations != 250 || res.InWindow == nil || *res.InWindow != 450 {
		t.Errorf("sample: sampled %v, %d of %v", res.Sampled, res.Conversations, res.InWindow)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "450 conversations in the window: the 250 most recent were read") {
		t.Errorf("notes = %v", res.Notes)
	}
	for _, q := range res.Queries {
		switch q.Statement {
		case pgread.Planned(messagesStmt, "conversation"):
			if q.Batches != 2 || q.Rows != 500 {
				t.Errorf("messages query = %+v", q)
			}
		case pgread.Planned(feedbackStmt, "conversation"):
			if q.Batches != 2 || q.Rows != 250 {
				t.Errorf("feedback query = %+v", q)
			}
		}
	}
}

func TestNoCapReadsEveryConversationPageByPage(t *testing.T) {
	res, _ := runOn(t, bigFixture(450), agent.Options{})
	if got := rows(t, res); len(got) != 450 || res.Conversations != 450 || res.InWindow != nil {
		t.Errorf("turns %d, conversations %d, in window %v", len(got), res.Conversations, res.InWindow)
	}
	for _, q := range res.Queries {
		if q.Statement == conversationsStmt && q.Batches != 3 {
			t.Errorf("conversation pages = %d, want 3", q.Batches)
		}
	}
}

// TestRunQueryMarksItsStatementExecuted uses one turn as Metabase v0.65.2.3
// wrote it (OSS, Postgres app DB, Metabot on an OpenAI-compatible model):
// create_sql_query builds the query, run_query runs it by id and reports
// returned="3" before the rows. The statement is marked succeeded with 3
// rows, and the rows themselves stay out.
func TestRunQueryMarksItsStatementExecuted(t *testing.T) {
	b, err := os.ReadFile("testdata/v0.65-run-query.json")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(b, &msgs); err != nil {
		t.Fatal(err)
	}
	data := fixture()
	data[messagesStmt] = nil
	for _, m := range msgs {
		data[messagesStmt] = append(data[messagesStmt], string(m))
	}
	res, _ := runOn(t, data, agent.Options{})
	got := rows(t, res)
	if len(got) != 1 {
		t.Fatalf("%d turns", len(got))
	}
	for _, p := range got {
		st, _ := p.Row["statements"].([]any)
		if len(st) != 1 {
			t.Fatalf("statements = %v", p.Row["statements"])
		}
		s := st[0].(map[string]any)
		if s["status"] != "succeeded" || s["rows"] != float64(3) || !strings.Contains(s["sql"].(string), "FROM PEOPLE") {
			t.Errorf("statement = %v", s)
		}
	}
	for _, r := range res.Records {
		if strings.Contains(string(r.Payload), "ann@shop.test") {
			t.Errorf("run_query rows kept: %s", r.Payload)
		}
	}
}
