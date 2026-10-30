package nao

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
)

// bigFixture is a nao database with chats chats (chat i updated at 10000+i)
// and messages messages spread over them, built in sqlite itself.
func bigFixture(t *testing.T, chats, messages int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE project (id TEXT, name TEXT, type TEXT, path TEXT)`,
		`CREATE TABLE chat (id TEXT, project_id TEXT, title TEXT, created_at INTEGER, updated_at INTEGER)`,
		`CREATE TABLE chat_message (id TEXT, chat_id TEXT, role TEXT, created_at INTEGER)`,
		`CREATE TABLE message_part (id TEXT, message_id TEXT, type TEXT, text TEXT, tool_name TEXT, tool_output TEXT, created_at INTEGER)`,
		`CREATE TABLE mcp_call_log (id TEXT, project_id TEXT, called_at INTEGER)`,
		`INSERT INTO project VALUES ('p1','Shop','local','/work')`,
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i < %d)
		  INSERT INTO chat SELECT 'c'||i, 'p1', 'chat '||i, 1000+i, 10000+i FROM n`, chats-1),
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i < %d)
		  INSERT INTO chat_message SELECT 'm'||i, 'c'||(i %% %d), 'user', 20000+i FROM n`, messages-1, chats),
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i < %d)
		  INSERT INTO message_part SELECT 'p'||i, 'm'||i, 'text', 'answer '||i, NULL, NULL, 20000+i FROM n`, messages-1),
		`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i < 99)
		  INSERT INTO mcp_call_log SELECT 'x'||i, 'p1', 30000+i FROM n`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return p
}

func TestMaxConversationsKeepsTheMostRecentChatsAndOnlyTheirChildren(t *testing.T) {
	p := bigFixture(t, 30, 90)
	res := run(t, agent.Inputs{"storage": "sqlite", "sqlite-path": p}, agent.Options{MaxConversations: 4})
	chats := map[string]bool{}
	for _, r := range res.Records {
		if r.RecordKind == chatKind {
			chats[r.NativeIDs["chat_id"]] = true
		}
	}
	// c26..c29 are the most recently updated.
	if len(chats) != 4 || !chats["c29"] || !chats["c26"] {
		t.Fatalf("chats = %v", chats)
	}
	c := counts(res)
	if c["nao_chat_message"] != 12 || c["nao_message_part"] != 12 { // 3 messages per chat, one part each
		t.Errorf("counts = %v", c)
	}
	for _, r := range res.Records {
		if r.RecordKind == "nao_chat_message" && !chats[r.NativeIDs["chat_id"]] {
			t.Errorf("message of an unchosen chat: %v", r.NativeIDs)
		}
	}
	if c["nao_mcp_call"] != 4*mcpPerChat {
		t.Errorf("mcp calls = %d, want %d", c["nao_mcp_call"], 4*mcpPerChat)
	}
	if !res.Sampled || res.Conversations != 4 || res.InWindow == nil || *res.InWindow != 30 {
		t.Errorf("sample: sampled %v, %d of %v", res.Sampled, res.Conversations, res.InWindow)
	}
	notes := strings.Join(res.Notes, "\n")
	if !strings.Contains(notes, "30 chats in the window: the 4 most recent were read") || !strings.Contains(notes, "MCP calls") {
		t.Errorf("notes = %v", res.Notes)
	}
}

func TestChildrenAreFetchedInBatches(t *testing.T) {
	p := bigFixture(t, 450, 900)
	res := run(t, agent.Inputs{"storage": "sqlite", "sqlite-path": p}, agent.Options{})
	if res.Conversations != 450 || res.InWindow != nil {
		t.Errorf("conversations %d, in window %v (no cap: not counted)", res.Conversations, res.InWindow)
	}
	var msgs *bundle.Query
	for i, q := range res.Queries {
		if q.Purpose == "messages" {
			msgs = &res.Queries[i]
		}
	}
	if msgs == nil || msgs.Batches != 3 || msgs.Rows != 900 || !strings.Contains(msgs.Statement, "IN (<up to 200 chat ids>)") {
		t.Fatalf("messages query = %+v", msgs)
	}
	if c := counts(res); c["nao_mcp_call"] != 100 {
		t.Errorf("no cap must read every MCP call: %v", c)
	}
}

// 200,000 messages and parts stream through emit without the reader
// holding them: the heap, sampled as rows arrive, stays far below the
// size of the collection.
func TestCollectionMemoryIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 200k-row database")
	}
	const messages = 200_000
	p := bigFixture(t, 1000, messages)
	a, _ := agent.Get("nao")
	in := agent.Inputs{"storage": "sqlite", "sqlite-path": p}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	var n int
	var peak uint64
	var ms runtime.MemStats
	var bytes int
	_, err := a.Collect(context.Background(), in, agent.Options{IncludeResults: true}, func(r bundle.Record) error {
		n++
		bytes += len(r.Payload)
		if n%50_000 == 0 {
			runtime.GC()
			runtime.ReadMemStats(&ms)
			peak = max(peak, ms.HeapAlloc)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := 1000 + 2*messages + 100; n != want {
		t.Fatalf("emitted %d records, want %d", n, want)
	}
	// The payloads alone are ~25 MB, records as Go values several times
	// that; a streaming reader holds a batch.
	t.Logf("heap peak %d MiB, payloads %d MiB", peak>>20, bytes>>20)
	if peak > 24<<20 {
		t.Errorf("heap peaked at %d MiB for %d MiB of payloads: rows are being accumulated", peak>>20, bytes>>20)
	}
}

// Without a cap, more chats than agent.IDSpool keeps in memory: their ids
// spill to a temp file and every chat's children are still read, in
// batches.
func TestNoCapSpillsChatIDs(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 10k-chat database")
	}
	const chats = 10_250
	p := bigFixture(t, chats, chats)
	res := run(t, agent.Inputs{"storage": "sqlite", "sqlite-path": p}, agent.Options{})
	if res.Conversations != chats {
		t.Fatalf("conversations = %d", res.Conversations)
	}
	c := counts(res)
	if c[chatKind] != chats || c["nao_chat_message"] != chats || c["nao_message_part"] != chats {
		t.Errorf("counts = %v", c)
	}
	for _, q := range res.Queries {
		if q.Purpose == "messages" && (q.Batches != (chats+agent.BatchSize-1)/agent.BatchSize || q.Rows != chats) {
			t.Errorf("messages query = %+v", q)
		}
	}
}
