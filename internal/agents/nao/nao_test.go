package nao

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// fixture builds a sqlite database shaped like nao's: millisecond integer
// timestamps, tool_output as text. projects is how many projects to create.
func fixture(t *testing.T, projects int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE project (id TEXT, name TEXT, type TEXT, path TEXT)`,
		`CREATE TABLE chat (id TEXT, project_id TEXT, title TEXT, created_at INTEGER, updated_at INTEGER)`,
		`CREATE TABLE chat_message (id TEXT, chat_id TEXT, role TEXT, created_at INTEGER)`,
		`CREATE TABLE message_part (id TEXT, message_id TEXT, type TEXT, text TEXT, tool_name TEXT, tool_output TEXT, created_at INTEGER)`,
		`CREATE TABLE llm_inference (id TEXT, project_id TEXT, chat_id TEXT, input_tokens INTEGER, created_at INTEGER)`,
		`CREATE TABLE "user" (id TEXT, name TEXT, email TEXT, email_verified INTEGER, github_token TEXT)`,
		// no message_feedback and no mcp_call_log: an older nao
		`INSERT INTO project VALUES ('p1','Shop','local','/work')`,
		`INSERT INTO chat VALUES ('c1','p1','orders',1000,5000), ('c0','p1','old',100,200)`,
		`INSERT INTO chat_message VALUES ('m1','c1','user',1000), ('m2','c1','assistant',1100), ('m0','c0','user',100)`,
		`INSERT INTO message_part VALUES
		  ('t1','m2','text','There are 42 orders.',NULL,NULL,1200),
		  ('s1','m2','tool','', 'execute_sql', '{"id":"q1","columns":["n"],"row_count":1,"data":[{"n":42}]}', 1150),
		  ('r1','m2','tool','', 'read', '{"content":"# orders\nOne row per order."}', 1140),
		  ('k1','m2','tool','', 'clarification', '{"question":"Which year?"}', 1145)`,
		`INSERT INTO llm_inference VALUES ('i1','p1','c1',900,1200)`,
		`INSERT INTO "user" VALUES ('u1','Ann','ann@shop.test',1,'ghp_LEAK')`,
	}
	if projects > 1 {
		stmts = append(stmts, `INSERT INTO project VALUES ('p2','Finance','local','/fin')`)
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return p
}

func run(t *testing.T, in agent.Inputs, opts agent.Options) agent.Collected {
	t.Helper()
	a, err := agent.Get("nao")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	res, err := agent.CollectAll(context.Background(), a.Collect, in, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func counts(res agent.Collected) map[string]int {
	c := map[string]int{}
	for _, r := range res.Records {
		c[r.RecordKind]++
	}
	return c
}

func TestCollectReadsEveryArmAndRedactsResults(t *testing.T) {
	res := run(t, agent.Inputs{"storage": "sqlite", "sqlite-path": fixture(t, 1)}, agent.Options{})
	c := counts(res)
	if c["nao_chat"] != 2 || c["nao_chat_message"] != 3 || c["nao_message_part"] != 4 || c["nao_llm_inference"] != 1 {
		t.Errorf("counts = %v", c)
	}
	if c["directory_user"] != 0 {
		t.Error("the user directory was read without --include-users")
	}
	if len(res.Notes) != 2 { // message_feedback and mcp_call_log are missing
		t.Errorf("notes = %v", res.Notes)
	}
	for _, r := range res.Records {
		if r.SourceKind != "nao" || r.FetchMethod == "" {
			t.Errorf("record = %+v", r)
		}
		if r.RecordKind != "nao_message_part" {
			continue
		}
		var m map[string]any
		json.Unmarshal(r.Payload, &m)
		out, _ := m["tool_output"].(string)
		switch m["tool_name"] {
		case "execute_sql":
			if strings.Contains(out, "42") && strings.Contains(out, "data") {
				t.Errorf("query result survived: %s", out)
			}
			if !strings.Contains(out, `"row_count":1`) || !strings.Contains(out, `"columns":["n"]`) {
				t.Errorf("shape lost: %s", out)
			}
		case "read":
			if !strings.Contains(out, "One row per order") {
				t.Errorf("context read was redacted: %s", out)
			}
		case "clarification":
			if !strings.Contains(out, "Which year?") {
				t.Errorf("clarification was redacted: %s", out)
			}
		}
	}
	if len(res.Redactions) != 1 {
		t.Errorf("redactions = %v", res.Redactions)
	}
}

func TestIncludeResultsKeepsRowsAndIncludeUsersReadsOnlySafeColumns(t *testing.T) {
	res := run(t, agent.Inputs{"storage": "sqlite", "sqlite-path": fixture(t, 1)},
		agent.Options{IncludeResults: true, IncludeUsers: true})
	all := ""
	for _, r := range res.Records {
		all += string(r.Payload)
	}
	if !strings.Contains(all, `\"data\":[{\"n\":42}]`) {
		t.Error("--include-results did not keep the rows")
	}
	if counts(res)["directory_user"] != 1 {
		t.Error("--include-users did not read the directory")
	}
	if strings.Contains(all, "ghp_LEAK") {
		t.Error("a user's git token was read")
	}
}

func TestSinceBoundsTheWindow(t *testing.T) {
	res := run(t, agent.Inputs{"storage": "sqlite", "sqlite-path": fixture(t, 1)},
		agent.Options{Since: time.UnixMilli(500)})
	if c := counts(res); c["nao_chat"] != 1 || c["nao_chat_message"] != 2 {
		t.Errorf("counts = %v", c)
	}
}

func TestSeveralProjectsNeedAChoice(t *testing.T) {
	p := fixture(t, 2)
	a, _ := agent.Get("nao")
	_, err := agent.CollectAll(context.Background(), a.Collect, agent.Inputs{"storage": "sqlite", "sqlite-path": p}, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("err = %v", err)
	}
	res := run(t, agent.Inputs{"storage": "sqlite", "sqlite-path": p, "project": "shop"}, agent.Options{})
	if counts(res)["nao_chat"] != 2 {
		t.Errorf("choosing by name failed: %v", counts(res))
	}
	opts, err := Projects(context.Background(), agent.Inputs{"storage": "sqlite", "sqlite-path": p})
	if err != nil || len(opts) != 2 {
		t.Errorf("Projects = %v, %v", opts, err)
	}
}

func TestPlanListsWhatCollectRuns(t *testing.T) {
	a, _ := agent.Get("nao")
	in := agent.Inputs{"storage": "sqlite", "sqlite-path": fixture(t, 1)}
	plan := a.Plan(in, agent.Options{})
	if len(plan) != 7 {
		t.Errorf("plan has %d statements, want 7 (projects + six arms)", len(plan))
	}
	for _, q := range plan {
		if strings.Contains(q.Statement, `"user"`) {
			t.Error("the directory query is planned without --include-users")
		}
	}
}

func TestRebind(t *testing.T) {
	if got := rebind("a = ? AND b > ?"); got != "a = $1 AND b > $2" {
		t.Errorf("rebind = %q", got)
	}
}

func TestChatsDeletedInNaoAreLeftOut(t *testing.T) {
	p := fixture(t, 1)
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`ALTER TABLE chat ADD COLUMN deleted_at INTEGER`,
		`UPDATE chat SET deleted_at = 300 WHERE id = 'c0'`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	res := run(t, agent.Inputs{"storage": "sqlite", "sqlite-path": p}, agent.Options{})
	if c := counts(res); c["nao_chat"] != 1 {
		t.Errorf("deleted chat collected: counts = %v", c)
	}
	for _, r := range res.Records {
		if r.NativeIDs["chat_id"] == "c0" {
			t.Errorf("a record of the deleted chat was collected: %+v", r.NativeIDs)
		}
	}
}
