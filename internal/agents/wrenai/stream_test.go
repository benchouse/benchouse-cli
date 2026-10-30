package wrenai

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// bigFixture is a wren-ui database with threads threads; thread i has three
// responses, the newest at 2026-01-01 00:00:00 + i minutes, so higher ids
// are more recent. Thread 0 and 1 share a newest timestamp, to exercise the
// keyset tie-break.
func bigFixture(t *testing.T, threads int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "db.sqlite3")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE thread (id integer PRIMARY KEY, project_id integer, created_at datetime)`,
		`CREATE TABLE thread_response (id integer PRIMARY KEY AUTOINCREMENT, thread_id integer, question text, sql text,
		  answer_detail text, asking_task_id integer, adjustment text, created_at datetime)`,
		`CREATE TABLE asking_task (id integer PRIMARY KEY, detail text)`,
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i < %d)
		  INSERT INTO thread SELECT i, 1, '2026-01-01 00:00:00' FROM n`, threads-1),
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i < %d)
		  INSERT INTO thread_response (thread_id, question, answer_detail, created_at)
		  SELECT i / 3, 'q'||i, '{}', datetime('2026-01-01 00:00:00', '+'||(max(i/3, 1))||' minutes', '-'||(2 - i %% 3)||' seconds') FROM n`, 3*threads-1),
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return p
}

func threadsOf(t *testing.T, res agent.Collected) map[int]int {
	t.Helper()
	out := map[int]int{}
	for _, p := range rows(t, res) {
		id, _ := strconv.Atoi(p.Row["conversation_id"].(string))
		out[id]++
	}
	return out
}

func TestMaxConversationsKeepsTheMostRecentThreadsAndOnlyTheirResponses(t *testing.T) {
	p := bigFixture(t, 30)
	res := run(t, p, agent.Options{MaxConversations: 4})
	got := threadsOf(t, res)
	if len(got) != 4 {
		t.Fatalf("threads = %v", got)
	}
	for id := 26; id < 30; id++ {
		if got[id] != 3 {
			t.Errorf("thread %d has %d responses, want 3 (threads %v)", id, got[id], got)
		}
	}
	if !res.Sampled || res.Conversations != 4 || res.InWindow == nil || *res.InWindow != 30 {
		t.Errorf("sample: sampled %v, %d of %v", res.Sampled, res.Conversations, res.InWindow)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "30 threads in the window: the 4 most recent were read") {
		t.Errorf("notes = %v", res.Notes)
	}
}

// More threads than a page: the keyset pages them without loss or overlap,
// with and without a cap, and each batch statement names at most 200 ids.
func TestThreadsArePagedInBatches(t *testing.T) {
	p := bigFixture(t, 450)
	for _, tc := range []struct{ max, want, pages int }{{0, 450, 3}, {250, 250, 2}} {
		res := run(t, p, agent.Options{MaxConversations: tc.max})
		got := threadsOf(t, res)
		if len(got) != tc.want || res.Conversations != tc.want {
			t.Fatalf("max %d: threads = %d, conversations = %d", tc.max, len(got), res.Conversations)
		}
		for id, n := range got {
			if n != 3 {
				t.Errorf("max %d: thread %d has %d responses", tc.max, id, n)
			}
			if id < 450-tc.want && !(tc.max == 0) {
				t.Errorf("max %d: older thread %d read", tc.max, id)
			}
		}
		for _, q := range res.Queries {
			if q.Purpose == turnsPurpose && q.Batches != tc.pages {
				t.Errorf("max %d: response batches = %d, want %d", tc.max, q.Batches, tc.pages)
			}
		}
	}
}
