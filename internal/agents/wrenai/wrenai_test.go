package wrenai

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/files"
)

// fixture builds a sqlite database shaped like wren-ui's after its knex
// migrations at v1-final: datetime text timestamps, jsonb columns as text.
// withAsk=false drops asking_task and adjustment, as before May 2025.
func fixture(t *testing.T, withAsk bool) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "db.sqlite3")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE project (id integer PRIMARY KEY AUTOINCREMENT, type varchar(255), display_name varchar(255))`,
		`CREATE TABLE thread (id integer PRIMARY KEY AUTOINCREMENT, project_id integer, summary text,
		  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
		  questions text, query_id varchar(255), questions_status varchar(255), questions_error text)`,
		`CREATE TABLE thread_response (id integer PRIMARY KEY AUTOINCREMENT, thread_id integer REFERENCES thread(id),
		  question text, breakdown_detail text, sql text, answer_detail text DEFAULT '{}', view_id integer,
		  chart_detail text, asking_task_id integer, adjustment text,
		  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE asking_task (id integer PRIMARY KEY AUTOINCREMENT, query_id varchar(255) NOT NULL UNIQUE,
		  question text, detail text DEFAULT '{}', thread_id integer, thread_response_id integer,
		  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`INSERT INTO project (id, type, display_name) VALUES (1, 'POSTGRES', 'shop')`,
		`INSERT INTO thread (id, project_id, summary, created_at) VALUES (1, 1, 'Orders last week?', '2026-10-01 09:00:00'), (2, 1, 'Weather?', '2026-10-02 09:00:00')`,
		`INSERT INTO asking_task (id, query_id, question, detail) VALUES
		  (1, 'q1', 'Orders last week?', '{"status":"FINISHED","type":"TEXT_TO_SQL","rephrasedQuestion":"How many orders were placed last week?","sqlGenerationReasoning":"Count orders in week 39.","retrievedTables":["orders"],"response":[{"type":"LLM","sql":"SELECT count(*) FROM orders"}],"error":null}'),
		  (2, 'q2', 'By region?', '{"status":"FINISHED","type":"TEXT_TO_SQL","response":[{"type":"VIEW","sql":"SELECT 1"}]}'),
		  (3, 'q3', 'Weather?', '{"status":"FAILED","type":"TEXT_TO_SQL","invalidSql":"SELECT temp FROM weather","error":{"code":"NO_RELEVANT_DATA","message":"I cannot find the exact data you are looking for"}}')`,
		`INSERT INTO thread_response (id, thread_id, question, sql, answer_detail, chart_detail, asking_task_id, created_at) VALUES
		  (1, 1, 'Orders last week?', 'SELECT count(*) FROM orders', '{"status":"FINISHED","content":"1,204 orders.","numRowsUsedInLLM":1}', '{"chartSchema":{"data":{"values":[{"n":1204}]}}}', 1, '2026-10-01 09:00:05'),
		  (2, 1, 'By region?', 'SELECT region, count(*) FROM orders GROUP BY 1', '{"status":"FAILED","error":{"code":"IBIS_SERVER_ERROR","message":"column region does not exist"}}', NULL, 2, '2026-10-01 09:01:00'),
		  (3, 2, 'Weather?', NULL, '{}', NULL, 3, '2026-10-02 09:00:05')`,
	}
	if !withAsk {
		stmts = append(stmts, `DROP TABLE asking_task`, `ALTER TABLE thread_response DROP COLUMN asking_task_id`,
			`ALTER TABLE thread_response DROP COLUMN adjustment`)
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return p
}

func run(t *testing.T, path string, opts agent.Options) agent.Collected {
	t.Helper()
	a, err := agent.Get("wren-ai")
	if err != nil {
		t.Fatal(err)
	}
	in := agent.Inputs{"sqlite-path": path}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	res, err := agent.CollectAll(context.Background(), a.Collect, in, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
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
		if r.SourceKind != "custom_sql" || r.RecordKind != "sql_turn" || r.FetchMethod != fetchMethod {
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

func TestTurnsMapTheResponseAndItsAskingTask(t *testing.T) {
	res := run(t, fixture(t, true), agent.Options{})
	got := rows(t, res)
	if len(got) != 3 || len(res.Notes) != 0 {
		t.Fatalf("turns = %d, notes = %v", len(got), res.Notes)
	}

	ok := got["1"].Row
	if ok["question_text"] != "Orders last week?" || ok["answer_text"] != "1,204 orders." || ok["outcome"] != "completed" {
		t.Errorf("turn 1 = %v", ok)
	}
	if ok["asked_at"] != "2026-10-01T09:00:05Z" || ok["conversation_id"] != "1" || ok["position"] != float64(1) {
		t.Errorf("turn 1 time/thread = %v %v %v", ok["asked_at"], ok["conversation_id"], ok["position"])
	}
	st := ok["statements"].([]any)[0].(map[string]any)
	if st["sql"] != "SELECT count(*) FROM orders" || st["status"] != "succeeded" || st["is_answer"] != true {
		t.Errorf("turn 1 statement = %v", st)
	}
	if r := ok["reasoning"].([]any); len(r) != 3 {
		t.Errorf("turn 1 reasoning = %v", r)
	}
	if ex := ok["extra"].(map[string]any); ex["candidate_type"] != "LLM" || ex["declined"] != nil {
		t.Errorf("turn 1 extra = %v", ex)
	}

	failed := got["2"].Row
	st = failed["statements"].([]any)[0].(map[string]any)
	if failed["outcome"] != "failed" || failed["error_type"] != "IBIS_SERVER_ERROR" || st["status"] != "failed" ||
		st["error"] != "column region does not exist" || failed["answer_text"] != nil || failed["position"] != float64(2) {
		t.Errorf("turn 2 = %v", failed)
	}

	declined := got["3"].Row
	if declined["extra"].(map[string]any)["declined"] != true || declined["outcome"] != "failed" ||
		declined["error_type"] != "NO_RELEVANT_DATA" {
		t.Errorf("turn 3 = %v", declined)
	}
	if sts := declined["statements"].([]any); len(sts) != 1 || sts[0].(map[string]any)["is_answer"] != false {
		t.Errorf("turn 3 statements = %v", sts)
	}

	for _, r := range res.Records {
		if strings.Contains(string(r.Payload), "1204}") {
			t.Error("chart data reached the bundle")
		}
	}
}

func TestOlderWrenWithoutAskingTaskDegrades(t *testing.T) {
	res := run(t, fixture(t, false), agent.Options{})
	if len(rows(t, res)) != 3 || len(res.Notes) != 1 || len(res.Queries) != 2 {
		t.Errorf("records = %d, notes = %v, queries = %d", len(res.Records), res.Notes, len(res.Queries))
	}
}

func TestSinceBoundsTheWindowAndKeepsPosition(t *testing.T) {
	res := run(t, fixture(t, true), agent.Options{Since: time.Date(2026, 10, 1, 9, 0, 30, 0, time.UTC)})
	got := rows(t, res)
	if len(got) != 2 || got["1"].Row != nil {
		t.Fatalf("turns = %v", got)
	}
	if got["2"].Row["position"] != float64(2) {
		t.Errorf("position after the window cut = %v", got["2"].Row["position"])
	}
}

func TestPlanListsWhatCollectRuns(t *testing.T) {
	for _, opts := range []agent.Options{{}, {MaxConversations: 1}} {
		a, _ := agent.Get("wren-ai")
		planned := map[string]bool{}
		for _, q := range a.Plan(agent.Inputs{}, opts) {
			planned[q.Statement] = true
		}
		res := run(t, fixture(t, true), opts)
		if len(res.Queries) < 3 {
			t.Fatalf("queries = %+v", res.Queries)
		}
		for _, q := range res.Queries {
			if !planned[q.Statement] {
				t.Errorf("ran an unplanned statement: %q", q.Statement)
			}
		}
	}
}

func TestKeepContext(t *testing.T) {
	for rel, want := range map[string]bool{"models.json": true, "mdl/notes.md": true, "x.yml": true, "data.csv": false} {
		if keepContext(rel) != want {
			t.Errorf("keep %s = %v", rel, !want)
		}
	}
}

// Any .json is kept, so an MDL export that carries connection settings must
// come out masked, and a credentials file beside it left out.
func TestContextJSONIsMasked(t *testing.T) {
	root := t.TempDir()
	mdl := `{"catalog":"wren","models":[{"name":"orders","columns":[{"name":"password_hash","type":"varchar"}]}],` +
		`"dataSource":{"type":"POSTGRES","properties":{"host":"db","user":"wren","password":"LEAK-PW"}}}`
	for rel, body := range map[string]string{"mdl.json": mdl, "service-account.json": `{"private_key_id":"LEAK-SA"}`} {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, skipped, err := files.Read(root, keepContext)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(skipped) != 1 || skipped[0].Reason != "credentials file" {
		t.Fatalf("got %d files, skipped %+v", len(got), skipped)
	}
	out := string(got[0].Data)
	if strings.Contains(out, "LEAK") || got[0].Masked != 1 || !strings.Contains(out, `"name":"password_hash"`) || !strings.Contains(out, `"user":"wren"`) {
		t.Errorf("masked %d: %s", got[0].Masked, out)
	}
}
