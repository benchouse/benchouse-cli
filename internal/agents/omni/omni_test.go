package omni

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

func init() {
	minGap = 0
	sleep = func(context.Context, time.Duration) error { return nil }
	now = func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) }
}

type fakeOmni struct {
	t        *testing.T
	personal bool
	limited  bool
	key      string // the API key accepted; "" means om-good
	requests []string
}

func conv(id, user string, msgs ...map[string]any) map[string]any {
	return map[string]any{"id": id, "userId": user, "organizationId": "org1", "createdAt": msgs[0]["timestamp"],
		"updatedAt": msgs[len(msgs)-1]["timestamp"], "messages": msgs}
}

func user(text, at string) map[string]any {
	return map[string]any{"role": "user", "content": text, "timestamp": at}
}

func reply(text, at, job string) map[string]any {
	return map[string]any{"role": "assistant", "content": text, "timestamp": at, "jobId": job, "omniChatUrl": "https://acme.omniapp.co/chat/" + job}
}

var convs = map[string]map[string]any{
	"c1": conv("c1", "u1",
		user("Orders last week?", "2026-10-29T10:00:00Z"), reply("**1,204** orders.", "2026-10-29T10:00:09Z", "j1"),
		user("By region?", "2026-10-29T10:01:00Z"), reply("", "2026-10-29T10:01:30Z", "j2")),
	"c2": conv("c2", "u2", user("Churn in Q3?", "2026-10-20T08:00:00Z"), reply("4.1%.", "2026-10-20T08:00:05Z", "j3")),
	"c3": conv("c3", "u1", user("Old question?", "2026-08-01T08:00:00Z"), reply("Old answer.", "2026-08-01T08:00:05Z", "j4")),
}

func jobJSON(id, conv, state, completed string, extra map[string]any) map[string]any {
	j := map[string]any{"id": id, "state": state, "prompt": "p", "conversationId": conv, "modelId": "m-1", "branchId": nil,
		"organizationId": "org1", "userId": "u1", "topicName": "orders", "requestedLlm": map[string]any{"model": "claude-sonnet-5", "provider": "anthropic"},
		"createdAt": completed, "updatedAt": completed}
	if completed != "" {
		j["completedAt"] = completed
	}
	for k, v := range extra {
		j[k] = v
	}
	return j
}

var jobs = map[string]map[string]any{
	"j1": jobJSON("j1", "c1", "COMPLETE", "2026-10-29T10:00:09Z", nil),
	"j2": jobJSON("j2", "c1", "FAILED", "2026-10-29T10:01:30Z", map[string]any{"error": map[string]any{"code": "NO_TOPIC", "message": "no topic fits"}, "requestedLlm": nil}),
	"j3": jobJSON("j3", "c2", "COMPLETE", "2026-10-20T08:00:05Z", nil),
	"j4": jobJSON("j4", "c3", "COMPLETE", "2026-08-01T08:00:05Z", nil),
}

var j1Result = map[string]any{
	"actions": []any{
		map[string]any{"type": "search_model", "status": "complete", "message": "Looking for an orders topic.", "timestamp": "2026-10-29T10:00:01Z"},
		map[string]any{"type": "generate_query", "status": "complete", "message": "Counting orders in week 43.", "timestamp": "2026-10-29T10:00:03Z",
			"result": map[string]any{"queryName": "Orders last week", "status": "success", "query": map[string]any{"table": "orders", "fields": []any{"orders.count"}},
				"csvResult": "orders.count\n1204", "csvResultWasTruncated": false, "hasResults": true, "totalRowCount": 1}},
		map[string]any{"type": "generate_query", "status": "complete", "message": "Checking with SQL.", "timestamp": "2026-10-29T10:00:05Z",
			"result": map[string]any{"queryName": "Check", "status": "success", "query": map[string]any{"userEditedSQL": "SELECT count(*) FROM orders"},
				"csvResult": "count\n1204", "csvResultWasTruncated": false, "hasResults": true, "totalRowCount": 1}},
		map[string]any{"type": "summarize", "status": "complete", "message": "**1,204** orders.", "timestamp": "2026-10-29T10:00:08Z"},
	},
	"message": "**1,204** orders.", "resultSummary": "**1,204** orders.", "topic": "orders",
}

func (f *fakeOmni) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if r.Method != http.MethodGet {
		f.t.Errorf("not a GET: %s", r.Method)
	}
	key := f.key
	if key == "" {
		key = "om-good"
	}
	if r.Header.Get("Authorization") != "Bearer "+key {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	write := func(v any) { json.NewEncoder(w).Encode(v) }
	item := func(id string) map[string]any {
		c := convs[id]
		return map[string]any{"id": id, "name": nil, "lastUserPrompt": "x", "updatedAt": c["updatedAt"], "createdAt": c["createdAt"]}
	}
	switch p := r.URL.Path; {
	case p == "/api/v1/ai/conversations":
		switch {
		case q.Get("userId") != "":
			if f.personal {
				http.Error(w, `{"message":"userId requires an organization API key"}`, http.StatusBadRequest)
				return
			}
			write(map[string]any{"data": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "nextCursor": nil, "pageSize": 1, "totalRecords": 0}})
		case q.Get("pageSize") == "1":
			write(map[string]any{"data": []any{item("c1")}, "pageInfo": map[string]any{"hasNextPage": true, "nextCursor": "x", "pageSize": 1, "totalRecords": 3}})
		case !f.limited:
			f.limited = true
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"message":"too many"}`, http.StatusTooManyRequests)
		case q.Get("cursor") == "":
			write(map[string]any{"data": []any{item("c1"), item("c2")}, "pageInfo": map[string]any{"hasNextPage": true, "nextCursor": "p2", "pageSize": 100, "totalRecords": 3}})
		case q.Get("cursor") == "p2":
			write(map[string]any{"data": []any{item("c3")}, "pageInfo": map[string]any{"hasNextPage": false, "nextCursor": nil, "pageSize": 100, "totalRecords": 3}})
		default:
			f.t.Errorf("cursor %q", q.Get("cursor"))
		}
	case strings.HasPrefix(p, "/api/v1/ai/conversations/"):
		c, ok := convs[strings.TrimPrefix(p, "/api/v1/ai/conversations/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		write(c)
	case strings.HasPrefix(p, "/api/v1/ai/jobs/") && strings.HasSuffix(p, "/result"):
		switch id := strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/ai/jobs/"), "/result"); id {
		case "j1":
			write(j1Result)
		case "j3":
			http.Error(w, `{"message":"result expired"}`, http.StatusNotFound)
		default:
			f.t.Errorf("result of %s requested", id)
			http.NotFound(w, r)
		}
	case strings.HasPrefix(p, "/api/v1/ai/jobs/"):
		j, ok := jobs[strings.TrimPrefix(p, "/api/v1/ai/jobs/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		write(j)
	default:
		f.t.Errorf("unexpected %s", r.URL)
		http.NotFound(w, r)
	}
}

func start(t *testing.T, personal bool) (*fakeOmni, agent.Inputs) {
	f := &fakeOmni{t: t, personal: personal}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, agent.Inputs{"host": srv.URL, "api-key": "om-good"}
}

func run(t *testing.T, opts agent.Options) (agent.Collected, *fakeOmni, agent.Inputs) {
	t.Helper()
	f, in := start(t, false)
	res, err := agent.CollectAll(context.Background(), collect, in, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res, f, in
}

func rows(t *testing.T, c agent.Collected) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, r := range c.Records {
		var p struct {
			Row map[string]any `json:"row"`
		}
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out[p.Row["turn_id"].(string)] = p.Row
	}
	return out
}

func TestCollectMapsConversations(t *testing.T) {
	res, f, in := run(t, agent.Options{})
	got := rows(t, res)
	if len(got) != 4 || res.Conversations != 3 || res.InWindow == nil || *res.InWindow != 3 {
		t.Fatalf("turns %d, conversations %d, in window %v", len(got), res.Conversations, res.InWindow)
	}
	a := got["c1:1"]
	if a["question_text"] != "Orders last week?" || a["answer_text"] != "**1,204** orders." || a["outcome"] != "completed" ||
		a["model_name"] != "claude-sonnet-5" || a["semantic_model_ref"] != "m-1" || a["user_id"] != "u1" || a["position"] != float64(1) ||
		a["completed_at"] != "2026-10-29T10:00:09Z" {
		t.Errorf("c1:1 = %v", a)
	}
	sts := a["statements"].([]any)
	if len(sts) != 1 || sts[0].(map[string]any)["sql"] != "SELECT count(*) FROM orders" || sts[0].(map[string]any)["rows"] != float64(1) ||
		sts[0].(map[string]any)["status"] != "succeeded" || sts[0].(map[string]any)["result"] != nil {
		t.Errorf("statements = %v", sts)
	}
	var gq map[string]any
	for _, tc := range a["tool_calls"].([]any) {
		if tc.(map[string]any)["name"] == "generate_query" {
			gq = tc.(map[string]any)
		}
	}
	if gq == nil || gq["input"].(map[string]any)["table"] != "orders" || gq["output"].(map[string]any)["csvResult"] != nil ||
		gq["output"].(map[string]any)["totalRowCount"] != float64(1) {
		t.Errorf("generate_query = %v", gq)
	}
	if r := a["reasoning"].([]any); len(r) != 3 || r[1].(map[string]any)["label"] != "generate_query" {
		t.Errorf("reasoning = %v", r)
	}
	if ex := a["extra"].(map[string]any); ex["topic"] != "orders" || ex["omni_chat_url"] == nil {
		t.Errorf("extra = %v", ex)
	}
	b := got["c1:2"]
	if b["outcome"] != "failed" || b["error_type"] != "NO_TOPIC" || b["error_message"] != "no topic fits" || b["answer_text"] != nil || b["model_name"] != nil {
		t.Errorf("c1:2 = %v", b)
	}
	if ex := got["c3:1"]["extra"].(map[string]any); ex["queries_unavailable"] == nil {
		t.Errorf("c3:1 extra = %v", ex)
	}
	notes := strings.Join(res.Notes, "\n")
	for _, want := range []string{"1 answers are older than 14 days", "1 job results were already gone", "feedback"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q: %v", want, res.Notes)
		}
	}
	if strings.Contains(notes, "personal access token") || len(res.Redactions) != 1 {
		t.Errorf("notes %v, redactions %v", res.Notes, res.Redactions)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "/j4/result") || strings.Contains(r, "/j2/result") {
			t.Errorf("requested an unavailable result: %s", r)
		}
	}
	planned := map[string]bool{}
	for _, q := range plan(in, agent.Options{}) {
		planned[q.Statement] = true
	}
	if len(res.Queries) != 6 {
		t.Errorf("queries = %+v", res.Queries)
	}
	for _, q := range res.Queries {
		if !planned[q.Statement] {
			t.Errorf("ran but not planned: %s", q.Statement)
		}
	}
	if f.requests[1] != f.requests[2] || !strings.Contains(f.requests[1], "pageSize=100") {
		t.Errorf("429 not retried: %v", f.requests[:3])
	}
}

func TestIncludeResultsKeepsRows(t *testing.T) {
	res, _, _ := run(t, agent.Options{IncludeResults: true})
	a := rows(t, res)["c1:1"]
	if a["statements"].([]any)[0].(map[string]any)["result"] != "count\n1204" {
		t.Errorf("statement result dropped: %v", a["statements"])
	}
	for _, tc := range a["tool_calls"].([]any) {
		if m := tc.(map[string]any); m["name"] == "generate_query" && m["output"].(map[string]any)["csvResult"] != "orders.count\n1204" {
			t.Errorf("tool output dropped: %v", m)
		}
	}
	if len(res.Redactions) != 0 {
		t.Errorf("redactions = %v", res.Redactions)
	}
}

func TestSinceStopsPaging(t *testing.T) {
	res, f, _ := run(t, agent.Options{Since: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)})
	got := rows(t, res)
	if len(got) != 2 || got["c2:1"] != nil || res.InWindow != nil {
		t.Errorf("turns = %d (%v), in window %v", len(got), got, res.InWindow)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "cursor=p2") || strings.Contains(r, "/c2") {
			t.Errorf("read past --since: %s", r)
		}
	}
}

func TestMaxConversationsCaps(t *testing.T) {
	res, f, _ := run(t, agent.Options{MaxConversations: 1})
	if got := rows(t, res); len(got) != 2 || res.Conversations != 1 || !res.Sampled {
		t.Errorf("turns %d, conversations %d", len(got), res.Conversations)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "--max-conversations 1") {
		t.Errorf("notes = %v", res.Notes)
	}
	for _, r := range f.requests {
		if strings.Contains(r, "/c2") || strings.Contains(r, "cursor=p2") {
			t.Errorf("read past the cap: %s", r)
		}
	}
}

func TestPersonalTokenIsNoted(t *testing.T) {
	_, in := start(t, true)
	res, err := agent.CollectAll(context.Background(), collect, in, agent.Options{MaxConversations: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "personal access token") {
		t.Errorf("notes = %v", res.Notes)
	}
	cr, err := check(context.Background(), in, agent.Options{})
	if err != nil || len(cr.Warnings) != 1 || !strings.Contains(cr.Warnings[0], "personal access token") {
		t.Errorf("check: %v %v", cr, err)
	}
}

func TestCheckAndVerify(t *testing.T) {
	_, in := start(t, false)
	fact, err := verifyKey(context.Background(), in)
	if err != nil || !strings.HasSuffix(fact, ", 3 AI conversations visible") {
		t.Errorf("verify: %q %v", fact, err)
	}
	res, err := check(context.Background(), in, agent.Options{MaxConversations: 2})
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Join(res.Facts, "\n")
	if !strings.Contains(facts, "Organization API key") || !strings.Contains(facts, "3 AI conversations in total: the 2 most recent will be sent") ||
		res.Conversations == nil || *res.Conversations != 3 || len(res.Warnings) != 0 {
		t.Errorf("check = %+v", res)
	}
	res, _ = check(context.Background(), in, agent.Options{Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)})
	if !strings.Contains(strings.Join(res.Facts, "\n"), "counted while collecting") || res.Conversations != nil {
		t.Errorf("check with --since = %+v", res)
	}
	bad := agent.Inputs{"host": in["host"], "api-key": "om-SECRETWRONG"}
	if _, err := verifyKey(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "rejected the API key") || strings.Contains(err.Error(), "SECRETWRONG") {
		t.Errorf("bad key: %v", err)
	}
	if _, err := agent.CollectAll(context.Background(), collect, bad, agent.Options{}); err == nil {
		t.Error("collect passed with a bad key")
	}
}

func TestFieldsValidate(t *testing.T) {
	a, err := agent.Get("omni")
	if err != nil {
		t.Fatal(err)
	}
	for host, ok := range map[string]bool{
		"https://acme.omniapp.co": true, "acme.omniapp.co": true, "https://acme.omniapp.co/api": true,
		"http://acme.omniapp.co": false, "https://acme.omniapp.co/models": false,
	} {
		err := a.Validate(agent.Inputs{"host": host, "api-key": "k"})
		if (err == nil) != ok {
			t.Errorf("%s: %v", host, err)
		}
	}
	if err := a.Validate(agent.Inputs{"api-key": "k"}); err == nil || !strings.Contains(err.Error(), "--host") {
		t.Errorf("no host: %v", err)
	}
}

func TestContextCheck(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"model.yaml":                 "default_topic_access_filters: []\n",
		"relationships.yaml":         "- join_from_view: orders\n",
		"ecom/orders.view.yaml":      "dimensions:\n  id: {}\n",
		"ecom/customers.view":        "dimensions:\n  id: {}\n",
		"orders.topic.yaml":          "base_view: ecom__orders\n",
		"README.txt":                 "not kept",
		"ecom/notes.composite_topic": "x: 1\n",
	} {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	res, err := checkContext(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Facts[0], "2 views, 2 topics, model file, relationships file") || !strings.Contains(res.Facts[1], "6 files") {
		t.Errorf("facts = %v", res.Facts)
	}
	dbt := t.TempDir()
	os.WriteFile(filepath.Join(dbt, "dbt_project.yml"), []byte("name: x\n"), 0o644)
	if _, err := checkContext(dbt); err == nil || !strings.Contains(err.Error(), "no Omni model") {
		t.Errorf("dbt folder: %v", err)
	}
}
