package nao

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestWriteSmokeFixture writes a small synthetic nao sqlite database to
// $BENCHOUSE_SMOKE_FIXTURE, for CI's end-to-end smoke run of the built
// binary (benchouse collect --agent nao --storage sqlite ...). Without the
// variable it does nothing.
func TestWriteSmokeFixture(t *testing.T) {
	p := os.Getenv("BENCHOUSE_SMOKE_FIXTURE")
	if p == "" {
		t.Skip("BENCHOUSE_SMOKE_FIXTURE not set")
	}
	os.Remove(p)
	db, err := sql.Open("sqlite", p) // a plain name: a Windows path is not a URI
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Add(-time.Hour).UnixMilli()
	for _, s := range []string{
		`CREATE TABLE project (id TEXT, name TEXT, type TEXT, path TEXT)`,
		`CREATE TABLE chat (id TEXT, project_id TEXT, title TEXT, created_at INTEGER, updated_at INTEGER)`,
		`CREATE TABLE chat_message (id TEXT, chat_id TEXT, role TEXT, created_at INTEGER)`,
		`CREATE TABLE message_part (id TEXT, message_id TEXT, type TEXT, text TEXT, tool_name TEXT, tool_output TEXT, created_at INTEGER)`,
		`INSERT INTO project VALUES ('p1','Demo shop','local','/demo')`,
		fmt.Sprintf(`INSERT INTO chat VALUES ('c1','p1','orders',%d,%d)`, now, now+5000),
		fmt.Sprintf(`INSERT INTO chat_message VALUES ('m1','c1','user',%d), ('m2','c1','assistant',%d)`, now, now+100),
		fmt.Sprintf(`INSERT INTO message_part VALUES
		  ('t0','m1','text','How many orders last week?',NULL,NULL,%[1]d),
		  ('s1','m2','tool','','execute_sql','{"id":"q1","columns":["n"],"row_count":1,"data":[{"n":42}]}',%[2]d),
		  ('t1','m2','text','There were 42 orders.',NULL,NULL,%[3]d)`, now+1, now+150, now+200),
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}
