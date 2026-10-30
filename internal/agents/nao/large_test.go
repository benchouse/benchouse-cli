package nao

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/pipeline"
)

// largeFixture is a nao database of parts message parts in 2,000 chats
// (four parts per message), every fourth part a SQL tool output with rows,
// built in sqlite itself.
func largeFixture(t *testing.T, parts int) string {
	t.Helper()
	const chats = 2000
	messages := parts / 4
	p := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Add(-24 * time.Hour).UnixMilli()
	for _, s := range []string{
		`PRAGMA journal_mode = OFF`, `PRAGMA synchronous = OFF`,
		`CREATE TABLE project (id TEXT, name TEXT, type TEXT, path TEXT)`,
		`CREATE TABLE chat (id TEXT, project_id TEXT, title TEXT, created_at INTEGER, updated_at INTEGER)`,
		`CREATE TABLE chat_message (id TEXT, chat_id TEXT, role TEXT, created_at INTEGER)`,
		`CREATE TABLE message_part (id TEXT, message_id TEXT, type TEXT, text TEXT, tool_name TEXT, tool_output TEXT, created_at INTEGER)`,
		`INSERT INTO project VALUES ('p1','Shop','local','/work')`,
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i < %d)
		  INSERT INTO chat SELECT 'c'||i, 'p1', 'chat '||i, %d+i, %d+i FROM n`, chats-1, now, now+1000),
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i < %d)
		  INSERT INTO chat_message SELECT 'm'||i, 'c'||(i %% %d), CASE i %% 2 WHEN 0 THEN 'user' ELSE 'assistant' END, %d+i FROM n`,
			messages-1, chats, now+2000),
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i < %d)
		  INSERT INTO message_part SELECT 'p'||i, 'm'||(i / 4),
		    CASE i %% 4 WHEN 3 THEN 'tool' ELSE 'text' END,
		    CASE i %% 4 WHEN 3 THEN '' ELSE 'How many orders shipped late last week, by region? answer part '||i END,
		    CASE i %% 4 WHEN 3 THEN 'execute_sql' END,
		    CASE i %% 4 WHEN 3 THEN '{"id":"q'||i||'","columns":["region","n"],"row_count":2,"data":[{"region":"EU","n":'||i||'},{"region":"US","n":7}]}' END,
		    %d+i FROM n`, parts-1, now+3000),
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%.80s: %v", s, err)
		}
	}
	return p
}

// A million message parts go through the whole pipeline (reader,
// pseudonymiser, records spool, bundle write and read-back) with the default
// options and cap: the heap, sampled throughout, stays bounded, and the run
// finishes in reasonable time.
func TestMillionPartsThroughThePipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 1,000,000-part database")
	}
	const parts = 1_000_000
	start := time.Now()
	p := largeFixture(t, parts)
	built := time.Since(start)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())

	a, _ := agent.Get("nao")
	in := agent.Inputs{"storage": "sqlite", "sqlite-path": p}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var peak, live uint64
	var mu sync.Mutex
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // sample the heap in use, garbage included, every 20ms
		defer wg.Done()
		var ms runtime.MemStats
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				runtime.ReadMemStats(&ms)
				mu.Lock()
				peak = max(peak, ms.HeapInuse)
				mu.Unlock()
			}
		}
	}()
	n := 0
	progress := func(s string) {
		n++
		if n%10 == 0 { // every 100,000 records: what is actually held
			var ms runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&ms)
			mu.Lock()
			live = max(live, ms.HeapAlloc)
			mu.Unlock()
		}
	}
	collectStart := time.Now()
	b, err := pipeline.Build(context.Background(), pipeline.Job{Agent: a, Inputs: in, Out: filepath.Join(t.TempDir(), "b.tar.gz"),
		Opts: agent.Options{AnonymizeUsers: true, MaxConversations: agent.DefaultMaxConversations}, Version: "test"}, progress)
	took := time.Since(collectStart)
	close(done)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Manifest.Records["nao_message_part"]; got != parts {
		t.Fatalf("collected %d parts, want %d", got, parts)
	}
	t.Logf("fixture %v; collect+write %v for %d records (%d MiB bundle); heap in use peak %d MiB, live peak %d MiB",
		built.Round(time.Millisecond), took.Round(time.Millisecond), b.Stats.Records, b.Size>>20, peak>>20, live>>20)
	if peak > 64<<20 || live > 32<<20 {
		t.Errorf("heap peaked at %d MiB in use, %d MiB live: the collection is being held in memory", peak>>20, live>>20)
	}
	if took > 5*time.Minute {
		t.Errorf("collecting a million parts took %v", took)
	}
}
