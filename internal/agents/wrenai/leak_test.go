package wrenai

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
)

// leakFixture is a wren-ui sqlite database with the canaries it can hold
// planted: the credential and host in the project's connection info (never
// read), the row value in a chart spec (never read) and in the answer's
// numbers, and the host in the database's own path. Wren keeps no people
// on threads.
func leakFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), leaktest.Host)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "db.sqlite3")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE project (id integer PRIMARY KEY, type varchar(255), display_name varchar(255), connection_info text)`,
		`CREATE TABLE thread (id integer PRIMARY KEY, project_id integer, summary text,
		  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
		  questions text, query_id varchar(255), questions_status varchar(255), questions_error text)`,
		`CREATE TABLE thread_response (id integer PRIMARY KEY, thread_id integer, question text, breakdown_detail text, sql text,
		  answer_detail text DEFAULT '{}', view_id integer, chart_detail text, asking_task_id integer, adjustment text,
		  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE asking_task (id integer PRIMARY KEY, query_id varchar(255) NOT NULL UNIQUE, question text, detail text DEFAULT '{}',
		  thread_id integer, thread_response_id integer,
		  created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`INSERT INTO project VALUES (1, 'POSTGRES', 'shop', '{"host":"` + leaktest.Host + `","password":"` + leaktest.Secret + `"}')`,
		`INSERT INTO thread (id, project_id, summary, created_at) VALUES (1, 1, 'Top order code?', '2026-10-01 09:00:00')`,
		`INSERT INTO asking_task (id, query_id, question, detail) VALUES
		  (1, 'q1', 'Top order code?', '{"status":"FINISHED","type":"TEXT_TO_SQL","response":[{"type":"LLM","sql":"SELECT code FROM orders LIMIT 1"}]}')`,
		`INSERT INTO thread_response (id, thread_id, question, sql, answer_detail, chart_detail, asking_task_id, created_at) VALUES
		  (1, 1, 'Top order code?', 'SELECT code FROM orders LIMIT 1', '{"status":"FINISHED","numRowsUsedInLLM":1}',
		   '{"chartSchema":{"data":{"values":[{"code":"` + leaktest.Row + `"}]}}}', 1, '2026-10-01 09:00:05')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return p
}

// leakContext is an exported MDL with the credential in its data source.
func leakContext(t *testing.T) string {
	dir := t.TempDir()
	mdl := `{"dataSource":{"type":"POSTGRES","properties":{"password":"` + leaktest.Secret + `"}},"models":[{"name":"orders"}]}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(mdl), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNoCanaryLeaks(t *testing.T) {
	leaktest.Run(t, leaktest.Case{
		Agent: "wren-ai",
		Inputs: func(t *testing.T) agent.Inputs {
			return agent.Inputs{"storage": "sqlite", "sqlite-path": leakFixture(t)}
		},
		Context: leakContext,
		Fail: func(t *testing.T) agent.Inputs {
			return agent.Inputs{"storage": "sqlite", "sqlite-path": filepath.Join(t.TempDir(), leaktest.Host, leaktest.Email, "db.sqlite3")}
		},
	})
}
