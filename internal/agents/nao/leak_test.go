package nao

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
)

// leakFixture is a nao sqlite database with every canary planted: the
// person in the user directory and as a chat's user_id, the credential in
// the user table's Git token column, the row value in a query result, and
// the host in the database's own path.
func leakFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), leaktest.Host)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "db.sqlite")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE project (id TEXT, name TEXT, type TEXT, path TEXT)`,
		`CREATE TABLE chat (id TEXT, project_id TEXT, user_id TEXT, title TEXT, created_at INTEGER, updated_at INTEGER)`,
		`CREATE TABLE chat_message (id TEXT, chat_id TEXT, role TEXT, created_at INTEGER)`,
		`CREATE TABLE message_part (id TEXT, message_id TEXT, type TEXT, text TEXT, tool_name TEXT, tool_input TEXT, tool_output TEXT, created_at INTEGER)`,
		`CREATE TABLE "user" (id TEXT, name TEXT, email TEXT, email_verified INTEGER, github_token TEXT)`,
		`INSERT INTO project VALUES ('p1','Shop','local','/work')`,
		`INSERT INTO chat VALUES ('c1','p1','` + leaktest.Email + `','orders',1000,5000)`,
		`INSERT INTO chat_message VALUES ('m1','c1','user',1000), ('m2','c1','assistant',1100)`,
		`INSERT INTO message_part VALUES
		  ('t1','m2','text','There are 42 orders.',NULL,NULL,NULL,1200),
		  ('s1','m2','tool','','execute_sql','{"sql":"SELECT code FROM orders"}',
		   '{"id":"q1","columns":["code"],"row_count":1,"data":[{"code":"` + leaktest.Row + `"}]}', 1150)`,
		`INSERT INTO "user" VALUES ('` + leaktest.Email + `','` + leaktest.Name + `','` + leaktest.Email + `',1,'` + leaktest.Secret + `')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return p
}

// leakContext is a nao context folder with the credential in its config
// and the row value in a sample-data file.
func leakContext(t *testing.T) string {
	return mkTree(t, map[string]string{
		"RULES.md":                         "# Rules\nAlways answer in EUR.\n",
		"nao_config.yaml":                  "databases:\n  - name: shop\n    password: " + leaktest.Secret + "\n",
		"databases/shop/orders/preview.md": "| code |\n|---|\n| " + leaktest.Row + " |\n",
		"databases/shop/orders/columns.md": "# orders\n- code\n",
	})
}

func TestNoCanaryLeaks(t *testing.T) {
	leaktest.Run(t, leaktest.Case{
		Agent: "nao",
		Inputs: func(t *testing.T) agent.Inputs {
			return agent.Inputs{"storage": "sqlite", "sqlite-path": leakFixture(t)}
		},
		Context: leakContext,
		Rows:    true,
		People:  true,
		Fail: func(t *testing.T) agent.Inputs {
			p := filepath.Join(t.TempDir(), leaktest.Host, leaktest.Email, "db.sqlite")
			return agent.Inputs{"storage": "sqlite", "sqlite-path": p}
		},
	})
}
