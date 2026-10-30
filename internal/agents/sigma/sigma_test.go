package sigma

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
)

type row struct {
	TurnID         string           `json:"turn_id"`
	Question       string           `json:"question_text"`
	Answer         *string          `json:"answer_text"`
	ConversationID string           `json:"conversation_id"`
	Position       int              `json:"position"`
	SessionID      *string          `json:"session_id"`
	UserID         string           `json:"user_id"`
	UserName       *string          `json:"user_name"`
	Model          *string          `json:"model_name"`
	InputTokens    *int             `json:"input_tokens"`
	OutputTokens   *int             `json:"output_tokens"`
	Outcome        *string          `json:"outcome"`
	ErrorMessage   *string          `json:"error_message"`
	Statements     []map[string]any `json:"statements"`
	ToolCalls      []map[string]any `json:"tool_calls"`
	Extra          map[string]any   `json:"extra"`
}

func decode(t *testing.T, recs []bundle.Record) (map[string]row, []string) {
	t.Helper()
	out := map[string]row{}
	var cols []string
	for _, r := range recs {
		var p struct {
			Contract string
			Columns  []string
			Row      row
		}
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Contract != "turns/v1" || r.RecordKind != "sql_turn" {
			t.Fatalf("record = %+v", r)
		}
		cols = p.Columns
		out[p.Row.TurnID] = p.Row
	}
	return out, cols
}

func collectSF(t *testing.T, w *warehouse, in map[string]string, opts agent.Options) agent.Collected {
	t.Helper()
	srv := w.snowflakeServer()
	t.Cleanup(srv.Close)
	useSnowflake(t, srv)
	inputs := agent.Inputs(sfInputs())
	for k, v := range in {
		inputs[k] = v
	}
	c, err := agent.CollectAll(context.Background(), collect, inputs, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ran(c agent.Collected) []string {
	var out []string
	for _, q := range c.Queries {
		out = append(out, q.Statement)
	}
	return out
}

func planStatements(in agent.Inputs, opts agent.Options) []string {
	var out []string
	for _, q := range plan(in, opts) {
		out = append(out, q.Statement)
	}
	return out
}

func TestSnowflakeTurnsFromSpans(t *testing.T) {
	w := newWarehouse(t)
	opts := agent.Options{MaxConversations: agent.DefaultMaxConversations}
	c := collectSF(t, w, nil, opts)
	turns, cols := decode(t, c.Records)
	if len(turns) != 4 || c.Conversations != 3 || !c.Sampled || c.InWindow == nil || *c.InWindow != 3 {
		t.Fatalf("turns=%d conversations=%d inWindow=%v", len(turns), c.Conversations, c.InWindow)
	}
	t1 := turns["t1"]
	if t1.Question != "Orders last week?" || t1.Answer == nil || *t1.Answer != "1,204 orders." {
		t.Errorf("t1 = %q / %v (the latest ROW_VERSION must win)", t1.Question, t1.Answer)
	}
	if t1.ConversationID != "s1" || t1.Position != 1 || turns["t2"].Position != 2 || turns["t2"].ConversationID != "s1" {
		t.Errorf("grouping: t1 %s/%d t2 %s/%d", t1.ConversationID, t1.Position, turns["t2"].ConversationID, turns["t2"].Position)
	}
	if t1.Model == nil || *t1.Model != "gpt-5" || *t1.InputTokens != 107 || *t1.OutputTokens != 20 {
		t.Errorf("model/tokens = %v %v %v", t1.Model, *t1.InputTokens, *t1.OutputTokens)
	}
	if len(t1.Statements) != 2 || t1.Statements[0]["sql"] != "SELECT count(*) FROM orders WHERE week = 39" ||
		t1.Statements[0]["status"] != "succeeded" || t1.Statements[1]["sql"] != "select id from orders limit 5" {
		t.Errorf("statements = %v", t1.Statements)
	}
	for _, s := range t1.Statements {
		if _, ok := s["result"]; ok {
			t.Errorf("result kept without --include-results: %v", s)
		}
	}
	if len(t1.ToolCalls) != 1 || t1.ToolCalls[0]["name"] != "search_docs" || t1.ToolCalls[0]["output"] != nil {
		t.Errorf("tool calls = %v (a natural-language query is not SQL; outputs dropped)", t1.ToolCalls)
	}
	if t1.UserID != "ana@example.com" || t1.UserName != nil || contains(cols, "user_name") {
		t.Errorf("user = %q %v cols %v", t1.UserID, t1.UserName, cols)
	}
	if t1.Extra["agent_name"] != "Sales agent" || t1.Extra["ai_provider"] != "openai" {
		t.Errorf("extra = %v", t1.Extra)
	}
	t3 := turns["t3"]
	if t3.ConversationID != "t3" || t3.SessionID != nil || t3.Outcome == nil || *t3.Outcome != "failed" || *t3.ErrorMessage != "timeout" {
		t.Errorf("legacy row without session: %+v", t3)
	}
	if len(c.Redactions) != 2 {
		t.Errorf("redactions = %v", c.Redactions)
	}
	if !strings.Contains(strings.Join(c.Notes, "\n"), "custom_col") {
		t.Errorf("unmapped column not noted: %v", c.Notes)
	}
	if got, want := ran(c), planStatements(agent.Inputs(sfInputs()), opts); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("plan != ran:\nran  %q\nplan %q", got, want)
	}
	if len(c.Sources) != 1 || c.Sources[0].Account != "ACME-X" || c.Sources[0].NativeID != "AI_USAGE_ACME" {
		t.Errorf("sources = %+v", c.Sources)
	}
	// The batch binds the chosen conversation ids; no id is spliced in.
	last := w.args[len(w.args)-1]
	if strings.Join(last, ",") != "s1,t3,s2" || strings.Contains(w.ran[len(w.ran)-1], "s1") {
		t.Errorf("batch args = %v, stmt %s", last, w.ran[len(w.ran)-1])
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func TestIncludeResultsAndUsers(t *testing.T) {
	c := collectSF(t, newWarehouse(t), nil, agent.Options{MaxConversations: 10, IncludeResults: true, IncludeUsers: true})
	turns, cols := decode(t, c.Records)
	t1 := turns["t1"]
	if t1.Statements[0]["result"] == nil || t1.Statements[1]["result"] != "5 rows" || t1.ToolCalls[0]["output"] != "docs" {
		t.Errorf("results dropped with --include-results: %v %v", t1.Statements, t1.ToolCalls)
	}
	if t1.UserName == nil || *t1.UserName != "ana@example.com" || !contains(cols, "user_name") || len(c.Redactions) != 0 {
		t.Errorf("user name = %v cols %v redactions %v", t1.UserName, cols, c.Redactions)
	}
}

func TestCapChoosesTheMostRecentConversations(t *testing.T) {
	w := newWarehouse(t)
	c := collectSF(t, w, nil, agent.Options{MaxConversations: 2})
	turns, _ := decode(t, c.Records)
	if len(turns) != 3 || c.Conversations != 2 || *c.InWindow != 3 {
		t.Fatalf("turns=%v conversations=%d", len(turns), c.Conversations)
	}
	if _, ok := turns["t4"]; ok {
		t.Error("the oldest conversation was read past the cap")
	}
	if !strings.Contains(strings.Join(c.Notes, "\n"), "3 Sigma AI conversations in the window: the 2 most recent were read") {
		t.Errorf("notes = %v", c.Notes)
	}
	if !strings.Contains(c.Queries[3].Statement, "ORDER BY 2 DESC LIMIT 2") {
		t.Errorf("candidates = %s", c.Queries[3].Statement)
	}
}

func TestSinceWindow(t *testing.T) {
	w := newWarehouse(t)
	opts := agent.Options{Since: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), MaxConversations: 100}
	c := collectSF(t, w, nil, opts)
	turns, _ := decode(t, c.Records)
	if len(turns) != 3 || *c.InWindow != 2 {
		t.Fatalf("turns = %d in window %d", len(turns), *c.InWindow)
	}
	for _, q := range c.Queries[2:] {
		if !strings.Contains(q.Statement, `"trace_created_at_utc" >= '2026-09-15 00:00:00'::TIMESTAMP_NTZ`) {
			t.Errorf("no window in %s", q.Statement)
		}
	}
}

func TestUncappedRunStillBatches(t *testing.T) {
	w := newWarehouse(t)
	w.rows = nil
	for i := range 450 {
		w.rows = append(w.rows, map[string]any{"trace_id": fmt.Sprintf("t%d", i), "session_id": fmt.Sprintf("s%d", i),
			"input": "q", "trace_created_at_utc": time.Date(2026, 10, 1, 0, 0, i, 0, time.UTC).Format("2006-01-02 15:04:05")})
	}
	c := collectSF(t, w, nil, agent.Options{})
	if len(c.Records) != 450 || c.Sampled || c.InWindow != nil {
		t.Fatalf("records = %d sampled %v", len(c.Records), c.Sampled)
	}
	q := c.Queries[len(c.Queries)-1]
	if q.Batches != 3 || q.Rows != 450 || !strings.Contains(q.Statement, "<up to 200 conversation ids>") {
		t.Errorf("batched query = %+v", q)
	}
	if strings.Contains(c.Queries[2].Statement, "LIMIT") {
		t.Errorf("uncapped candidates have a LIMIT: %s", c.Queries[2].Statement)
	}
}

func TestDatabricksDiscoversTheTable(t *testing.T) {
	w := newWarehouse(t)
	w.cols = append(allColumns()[:0:0], allColumns()...)
	srv := w.databricksServer()
	defer srv.Close()
	dbxPollEvery = time.Millisecond
	in := agent.Inputs{"platform": "databricks", "host": srv.URL, "token": "dapi-tok", "warehouse-id": "/sql/1.0/warehouses/wh1",
		"catalog": "main", "schema": "sigma"}
	opts := agent.Options{MaxConversations: 50}
	c, err := agent.CollectAll(context.Background(), collect, in, opts)
	if err != nil {
		t.Fatal(err)
	}
	turns, _ := decode(t, c.Records)
	if len(turns) != 4 || turns["t1"].Answer == nil || *turns["t1"].Answer != "1,204 orders." {
		t.Fatalf("turns = %v", turns)
	}
	// The plan names the table it will find; the run names it.
	want := planStatements(in, opts)
	for i := range want {
		want[i] = strings.ReplaceAll(want[i], tableUnknown, w.table)
	}
	if got := ran(c); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("plan != ran:\nran  %q\nplan %q", got, want)
	}
	if !strings.Contains(c.Queries[4].Statement, "COALESCE(NULLIF(`session_id`, ''), `trace_id`) IN (<up to 200") ||
		!strings.Contains(c.Queries[4].Statement, "FROM `main`.`sigma`.`AI_USAGE_ACME`") {
		t.Errorf("batch = %s", c.Queries[4].Statement)
	}
	if w.args[0][0] != "sigma" || len(w.args[len(w.args)-1]) != 3 {
		t.Errorf("params = %v", w.args)
	}
}

func TestBigQueryPagesThroughJobs(t *testing.T) {
	w := newWarehouse(t)
	key, pub := keyFile(t)
	bq := w.bigqueryServer(t, pub)
	in := agent.Inputs{"platform": "bigquery", "key-file": key, "dataset": "sigma_logs", "table": "AI_USAGE_ACME"}
	opts := agent.Options{Since: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), MaxConversations: 10}
	c, err := agent.CollectAll(context.Background(), collect, in, opts)
	if err != nil {
		t.Fatal(err)
	}
	turns, _ := decode(t, c.Records)
	if len(turns) != 3 || turns["t2"].Question != "And the week before?" {
		t.Fatalf("turns = %v", turns)
	}
	if bq.tokens != 1 || bq.paged == 0 {
		t.Errorf("tokens = %d, pages followed = %d", bq.tokens, bq.paged)
	}
	if bq.claims["scope"] != bqScope || bq.claims["aud"] != bqTokenURL || bq.claims["iss"] != "benchouse@acme-proj.iam.gserviceaccount.com" {
		t.Errorf("claims = %v (the key file's token_uri must not be used)", bq.claims)
	}
	if got, want := ran(c), planStatements(in, opts); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("plan != ran:\nran  %q\nplan %q", got, want)
	}
	if !strings.Contains(c.Queries[4].Statement, "FROM `acme-proj.sigma_logs.AI_USAGE_ACME` AS t WHERE") ||
		!strings.Contains(c.Queries[4].Statement, ">= TIMESTAMP '2026-09-15 00:00:00+00'") {
		t.Errorf("batch = %s", c.Queries[4].Statement)
	}
	if c.Sources[0].Account != "acme-proj" {
		t.Errorf("sources = %+v", c.Sources)
	}
}

func TestBigQueryRefusedKey(t *testing.T) {
	w := newWarehouse(t)
	key, pub := keyFile(t)
	bq := w.bigqueryServer(t, pub)
	bq.refuse = true
	_, err := verifyKeyFile(context.Background(), agent.Inputs{"platform": "bigquery", "key-file": key})
	if err == nil || !strings.Contains(err.Error(), "Invalid JWT Signature") {
		t.Errorf("err = %v", err)
	}
	bq.refuse = false
	fact, err := verifyKeyFile(context.Background(), agent.Inputs{"platform": "bigquery", "key-file": key})
	if err != nil || !strings.Contains(fact, "benchouse@acme-proj") {
		t.Errorf("fact = %q, %v", fact, err)
	}
	ds, err := bqDatasets(context.Background(), agent.Inputs{"platform": "bigquery", "key-file": key})
	if err != nil || len(ds) != 1 || ds[0][0] != "sigma_logs" {
		t.Errorf("datasets = %v, %v", ds, err)
	}
}

func TestKeyFileValidation(t *testing.T) {
	key, _ := keyFile(t)
	if err := validKeyFile(key); err != nil {
		t.Errorf("valid key refused: %v", err)
	}
	p := t.TempDir() + "/x.json"
	writeFile(t, p, `{"type":"authorized_user","client_email":"a","private_key":"b"}`)
	if err := validKeyFile(p); err == nil || !strings.Contains(err.Error(), "service account") {
		t.Errorf("err = %v", err)
	}
}
