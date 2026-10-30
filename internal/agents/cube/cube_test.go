package cube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

func init() {
	sleep = func(context.Context, time.Duration) error { return nil }
}

const token = "cube-key-1"

// fakeCube serves the deployments and chat history endpoints over threads,
// newest updated first, paged by an integer cursor.
type fakeCube struct {
	t        *testing.T
	threads  []map[string]any
	status   int // non-zero: chat history answers with it
	noTurns  map[string]bool
	throttle int // 429s to send before the first threads page

	mu       sync.Mutex
	requests []string // path?query
}

func (f *fakeCube) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.RequestURI())
		f.mu.Unlock()
		if r.Method != http.MethodGet {
			f.t.Errorf("not a GET: %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/api/v1/deployments":
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": 42, "name": "prod"}, map[string]any{"id": 7, "name": "dev"}},
				"pageInfo": map[string]any{"hasNextPage": false}, "pagination": map[string]any{"total": 2, "offset": 0, "limit": 100}})
		case r.URL.Path == "/api/v1/deployments/42":
			w.Write([]byte(`{"id":42,"name":"prod","deploymentUrl":"https://prod.cubecloud.dev","creationStep":"ready"}`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/deployments/42/chat-history/threads/"):
			if f.status != 0 {
				http.Error(w, `{"message":"nope"}`, f.status)
				return
			}
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/deployments/42/chat-history/threads/")
			for _, th := range f.threads {
				if th["chatId"] == id {
					json.NewEncoder(w).Encode(th)
					return
				}
			}
			http.NotFound(w, r)
		case r.URL.Path == "/api/v1/deployments/42/chat-history/threads":
			if f.status != 0 {
				http.Error(w, `{"message":"nope"}`, f.status)
				return
			}
			f.mu.Lock()
			if f.throttle > 0 {
				f.throttle--
				f.mu.Unlock()
				w.Header().Set("Retry-After", "1")
				http.Error(w, "slow down", http.StatusTooManyRequests)
				return
			}
			f.mu.Unlock()
			if q.Get("sortBy") != "updatedAt" || q.Get("sortOrder") != "desc" {
				f.t.Errorf("sort = %s %s", q.Get("sortBy"), q.Get("sortOrder"))
			}
			first, _ := strconv.Atoi(q.Get("first"))
			withTurns := q.Get("includeTurns") == "true"
			if withTurns && first > 50 || first > 200 || first < 1 {
				f.t.Errorf("first = %d with turns %v", first, withTurns)
			}
			var in []map[string]any
			since := q.Get("updatedAfter")
			for _, th := range f.threads {
				if since == "" || th["updatedAt"].(string) >= since {
					in = append(in, th)
				}
			}
			start, _ := strconv.Atoi(q.Get("after"))
			end := min(start+first, len(in))
			var items []map[string]any
			for _, th := range in[start:end] {
				c := map[string]any{}
				for k, v := range th {
					c[k] = v
				}
				if !withTurns || f.noTurns[th["chatId"].(string)] {
					c["turns"] = nil
				}
				items = append(items, c)
			}
			pi := map[string]any{"hasNextPage": end < len(in), "hasPreviousPage": start > 0, "endCursor": strconv.Itoa(end)}
			json.NewEncoder(w).Encode(map[string]any{"items": items, "pageInfo": pi})
		default:
			http.NotFound(w, r)
		}
	}))
}

func ptr(s string) *string { return &s }

func cubeThread(id, updated string, turnsJSON string) map[string]any {
	var ts []any
	if err := json.Unmarshal([]byte(turnsJSON), &ts); err != nil {
		panic(err)
	}
	return map[string]any{"chatId": id, "createdAt": updated, "updatedAt": updated, "agentId": 3, "title": "Orders",
		"source": "web", "initialContext": nil,
		"user":  map[string]any{"id": 11, "type": "internal", "email": "ann@shop.test", "name": "Ann", "externalId": nil},
		"turns": ts}
}

func fixtures() []map[string]any {
	t1 := `[{"index":0,"messageId":"m1","prompt":"Orders last week?","files":[],"tools":["cubeSqlApi"],
	  "queries":[{"messageId":"q1","tool":"cubeSqlApi","status":"success","sql":"SELECT count(*) FROM orders","dataSource":"default",
	    "rowCount":1,"rows":[[1204]],"columns":["count"],"error":null,"title":"Orders",
	    "feedback":{"isPositive":false,"createdAt":"2026-10-02T12:05:00Z","userId":11}}],
	  "steps":[{"type":"model","messageId":"s1","text":"Let me query.","reasoning":"Count orders in week 39.","tool":null,"input":null,"status":"success","startedAt":"2026-10-02T12:00:01Z"},
	    {"type":"tool","messageId":"s2","text":null,"reasoning":null,"tool":"cubeSqlApi","input":"{\"query\":\"orders\"}","status":"success","startedAt":"2026-10-02T12:00:02Z"},
	    {"type":"model","messageId":"s3","text":"1,204 orders.","reasoning":null,"tool":null,"input":null,"status":"success","startedAt":"2026-10-02T12:00:05Z"}],
	  "finalAnswer":"1,204 orders.","startedAt":"2026-10-02T12:00:00Z","endedAt":"2026-10-02T12:00:06Z"},
	 {"index":1,"messageId":"m2","prompt":"By region?","files":[],"tools":["runSql"],
	  "queries":[{"messageId":"q2","tool":"runSql","status":"error","sql":"SELECT region, count(*) FROM orderz","dataSource":null,
	    "rowCount":null,"rows":null,"columns":null,"error":"relation orderz does not exist","title":null,"feedback":null}],
	  "steps":null,"finalAnswer":null,"startedAt":"2026-10-02T12:01:00Z","endedAt":"2026-10-02T12:01:04Z"}]`
	t2 := `[{"index":0,"messageId":"m3","prompt":"Revenue?","files":[],"tools":[],"queries":[],"steps":[],
	  "finalAnswer":"$10k.","startedAt":"2026-09-20T09:00:00Z","endedAt":"2026-09-20T09:00:03Z"},
	 {"index":1,"messageId":"m4","prompt":"And today?","files":[],"tools":[],"queries":[],"steps":[],
	  "finalAnswer":"$1k.","startedAt":"2026-10-01T09:00:00Z","endedAt":"2026-10-01T09:00:03Z"}]`
	t3 := `[{"index":0,"messageId":"m5","prompt":"Churn?","files":[],"tools":[],"queries":[],"steps":[],
	  "finalAnswer":"2%.","startedAt":"2026-08-01T09:00:00Z","endedAt":"2026-08-01T09:00:03Z"}]`
	return []map[string]any{
		cubeThread("c1", "2026-10-02T12:01:04Z", t1),
		cubeThread("c2", "2026-10-01T09:00:03Z", t2),
		cubeThread("c3", "2026-08-01T09:00:03Z", t3),
	}
}

func inputs(url string) agent.Inputs {
	return agent.Inputs{"host": url, "api-token": token, "deployment-id": "42"}
}

type row struct {
	Columns []string       `json:"columns"`
	Row     map[string]any `json:"row"`
}

func rows(t *testing.T, c agent.Collected) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, r := range c.Records {
		var p row
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out[p.Row["turn_id"].(string)] = p.Row
	}
	return out
}

func TestCollectMapsTurns(t *testing.T) {
	f := &fakeCube{t: t, threads: fixtures(), throttle: 1}
	srv := f.server()
	defer srv.Close()
	opts := agent.Options{MaxConversations: 2000}
	c, err := agent.CollectAll(context.Background(), collect, inputs(srv.URL), opts)
	if err != nil {
		t.Fatal(err)
	}
	got := rows(t, c)
	if len(got) != 5 || c.Conversations != 3 {
		t.Fatalf("%d turns, %d conversations", len(got), c.Conversations)
	}
	r := got["c1:m1"]
	if r["question_text"] != "Orders last week?" || r["answer_text"] != "1,204 orders." || r["outcome"] != "completed" ||
		r["position"] != 1.0 || r["user_id"] != "11" || r["client_interface"] != "web" {
		t.Errorf("turn = %v", r)
	}
	if _, ok := r["user_email"]; ok {
		t.Error("user_email declared without --include-users")
	}
	st := r["statements"].([]any)[0].(map[string]any)
	if st["sql"] != "SELECT count(*) FROM orders" || st["status"] != "succeeded" || st["rows"] != 1.0 || st["result"] != nil {
		t.Errorf("statement = %v", st)
	}
	ex := r["extra"].(map[string]any)
	fb := ex["feedback"].([]any)[0].(map[string]any)
	if fb["is_positive"] != false || fb["score"] != -1.0 || ex["agent_id"] != "3" {
		t.Errorf("extra = %v", ex)
	}
	if strings.Contains(string(c.Records[0].Payload), "1204]") {
		t.Error("result rows leaked")
	}
	if len(r["reasoning"].([]any)) != 1 || len(r["messages"].([]any)) != 1 || len(r["tool_calls"].([]any)) != 1 {
		t.Errorf("steps = %v / %v / %v", r["reasoning"], r["messages"], r["tool_calls"])
	}
	r2 := got["c1:m2"]
	if r2["outcome"] != "failed" || r2["answer_text"] != nil || r2["error_message"] != "relation orderz does not exist" {
		t.Errorf("failed turn = %v", r2)
	}
	if tc := r2["tool_calls"].([]any); len(tc) != 1 || tc[0].(map[string]any)["name"] != "runSql" {
		t.Errorf("tools fallback = %v", tc)
	}
	// Plan is what ran.
	planned := map[string]bool{}
	for _, q := range plan(inputs(srv.URL), opts) {
		planned[q.Statement] = true
	}
	for _, q := range c.Queries {
		if !planned[q.Statement] {
			t.Errorf("ran %q, not in Plan", q.Statement)
		}
	}
	if len(c.Redactions) != 2 {
		t.Errorf("redactions = %v", c.Redactions)
	}
}

func TestCollectPagesAndCaps(t *testing.T) {
	var many []map[string]any
	for i := 0; i < 120; i++ {
		at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Hour).Format(time.RFC3339)
		many = append(many, cubeThread(fmt.Sprintf("t%03d", i), at,
			fmt.Sprintf(`[{"index":0,"messageId":"m","prompt":"q%d","finalAnswer":"a","startedAt":%q}]`, i, at)))
	}
	f := &fakeCube{t: t, threads: many}
	srv := f.server()
	defer srv.Close()
	c, err := agent.CollectAll(context.Background(), collect, inputs(srv.URL), agent.Options{MaxConversations: 70})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Records) != 70 || c.Conversations != 70 || !c.Sampled {
		t.Fatalf("%d records, %d conversations", len(c.Records), c.Conversations)
	}
	if !strings.Contains(strings.Join(c.Notes, "\n"), "--max-conversations 70") {
		t.Errorf("notes = %v", c.Notes)
	}
	// Two pages: 50, then the remaining 20.
	var pages []string
	for _, r := range f.requests {
		if strings.HasPrefix(r, "/api/v1/deployments/42/chat-history/threads?") {
			pages = append(pages, r)
		}
	}
	if len(pages) != 2 || !strings.Contains(pages[1], "first=20") {
		t.Errorf("pages = %v", pages)
	}
	if q := c.Queries[len(c.Queries)-1]; q.Batches != 1 || q.Rows != 20 {
		t.Errorf("tally = %+v", q)
	}

	// No cap: every page.
	c, err = agent.CollectAll(context.Background(), collect, inputs(srv.URL), agent.Options{})
	if err != nil || len(c.Records) != 120 || c.Sampled {
		t.Fatalf("uncapped: %d records, %v", len(c.Records), err)
	}
}

func TestCollectSinceAndOptIns(t *testing.T) {
	f := &fakeCube{t: t, threads: fixtures(), noTurns: map[string]bool{"c2": true}}
	srv := f.server()
	defer srv.Close()
	since := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	c, err := agent.CollectAll(context.Background(), collect, inputs(srv.URL),
		agent.Options{Since: since, IncludeResults: true, IncludeUsers: true})
	if err != nil {
		t.Fatal(err)
	}
	got := rows(t, c)
	// c3 is outside the window; c2's turn of 09-20 is too.
	if len(got) != 3 || got["c2:m3"] != nil || got["c2:m4"] == nil {
		t.Fatalf("turns = %v", keys(got))
	}
	if !strings.Contains(f.requests[1], "updatedAfter=2026-09-25T00%3A00%3A00Z") {
		t.Errorf("request = %s", f.requests[1])
	}
	r := got["c1:m1"]
	if r["user_email"] != "ann@shop.test" || r["user_name"] != "Ann" {
		t.Errorf("users = %v", r)
	}
	if res := r["statements"].([]any)[0].(map[string]any)["result"]; res == nil {
		t.Error("result dropped with --include-results")
	}
	// c2 came without turns: read on its own.
	found := false
	for _, q := range c.Queries {
		found = found || (q.Purpose == purposeThread && q.Rows == 1)
	}
	if !found || len(c.Redactions) != 0 {
		t.Errorf("queries = %+v, redactions = %v", c.Queries, c.Redactions)
	}
	if !strings.Contains(strings.Join(c.Notes, "\n"), "before --since") {
		t.Errorf("notes = %v", c.Notes)
	}
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestCheckAndErrors(t *testing.T) {
	f := &fakeCube{t: t, threads: fixtures()}
	srv := f.server()
	defer srv.Close()
	ctx := context.Background()
	res, err := check(ctx, inputs(srv.URL), agent.Options{MaxConversations: 2})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(res.Facts, "\n")
	if res.Conversations == nil || *res.Conversations != 3 || !strings.Contains(all, "prod (id 42)") ||
		!strings.Contains(all, "the 2 most recent will be sent") {
		t.Errorf("facts = %v", res.Facts)
	}

	f.status = http.StatusNotFound
	if _, err := check(ctx, inputs(srv.URL), agent.Options{}); err == nil || !strings.Contains(err.Error(), "ask Cube support to enable it") {
		t.Errorf("404: %v", err)
	}
	if _, err := agent.CollectAll(ctx, collect, inputs(srv.URL), agent.Options{}); err == nil || !strings.Contains(err.Error(), "preview feature") {
		t.Errorf("collect 404: %v", err)
	}
	f.status = http.StatusForbidden
	if _, err := check(ctx, inputs(srv.URL), agent.Options{}); err == nil || !strings.Contains(err.Error(), "admin role") {
		t.Errorf("403: %v", err)
	}
	in := inputs(srv.URL)
	in["deployment-id"] = "9"
	if _, err := check(ctx, in, agent.Options{}); err == nil || !strings.Contains(err.Error(), "no deployment 9") {
		t.Errorf("missing deployment: %v", err)
	}
}

func TestVerifyAndLookup(t *testing.T) {
	f := &fakeCube{t: t}
	srv := f.server()
	defer srv.Close()
	ctx := context.Background()
	fact, err := verifyToken(ctx, inputs(srv.URL))
	if err != nil || !strings.Contains(fact, "2 deployments") {
		t.Errorf("verify = %q, %v", fact, err)
	}
	in := inputs(srv.URL)
	in["api-token"] = "wrong-secret"
	if _, err := verifyToken(ctx, in); err == nil || strings.Contains(err.Error(), "wrong-secret") || !strings.Contains(err.Error(), "401") {
		t.Errorf("bad key: %v", err)
	}
	opts, err := lookupDeployments(ctx, inputs(srv.URL))
	if err != nil || len(opts) != 2 || opts[0] != [2]string{"42", "prod"} {
		t.Errorf("lookup = %v, %v", opts, err)
	}
}

func TestValidate(t *testing.T) {
	a, err := agent.Get("cube")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		in   agent.Inputs
		want string
	}{
		{agent.Inputs{"host": "http://acme.cubecloud.dev", "api-token": "k", "deployment-id": "1"}, "https"},
		{agent.Inputs{"host": "https://acme.cubecloud.dev", "api-token": "k", "deployment-id": "prod"}, "number"},
		{agent.Inputs{"host": "acme.cubecloud.dev", "api-token": "k", "deployment-id": "1"}, ""},
	} {
		err := a.Validate(c.in)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%v: %v", c.in, err)
		}
	}
}

func TestContext(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cube.js", "module.exports = { apiSecret: 'x' }")
	write("model/cubes/orders.yml", "cubes:\n  - name: orders\n    sql_table: orders\n    joins:\n      - name: users\n  - name: users\n    sql_table: users\n")
	write("model/views/sales.yml", "views:\n  - name: sales\n    cubes:\n      - join_path: orders\n")
	write("model/cubes/legacy.js", "cube(`events`, {\n  sql: `SELECT 1`,\n});\n")
	res, err := checkContext(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Facts[0], "3 cubes and 1 views") {
		t.Errorf("facts = %v", res.Facts)
	}
	if keepContext("cube.js") || !keepContext("model/cubes/orders.yml") || keepContext("model/.env") {
		t.Error("keep rule")
	}
	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, "notes.yml"), []byte("a: 1\n"), 0o644)
	if _, err := checkContext(empty); err == nil {
		t.Error("a folder without cubes passed")
	}
}
