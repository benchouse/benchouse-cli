package thoughtspot

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

const (
	bearer   = "tok-1"
	secret   = "s3cr3t-key"
	modelID  = "guid-model-1"
	tsLayout = "2006-01-02 15:04:05"
)

var base = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

var modelCols = []string{"User Query", "Response", "User Feedback Rating", "Timestamp", "Conversation Id", "User Name", "Data Source"}

// fakeTS is an httptest ThoughtSpot: sign-in, session, metadata search,
// searchdata over in-memory rows (newest first), and saved conversations.
type fakeTS struct {
	t           *testing.T
	rows        [][]any // in modelCols order, newest first
	noModel     bool
	modelStatus int
	savedStatus int
	convs       []map[string]any
	msgs        map[string]any
	throttle    int

	mu       sync.Mutex
	searches []map[string]any
	requests []string
}

func (f *fakeTS) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, apiPrefix)
		var body map[string]any
		if r.Method == http.MethodPost {
			json.NewDecoder(r.Body).Decode(&body)
		}
		if p == "/auth/token/full" {
			if body["username"] == "admin" && (body["secret_key"] == secret || body["password"] == secret) {
				w.Write([]byte(`{"token":"` + bearer + `","expiration_time_in_millis":1}`))
				return
			}
			http.Error(w, `{"error":{"message":"bad credentials"}}`, http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			http.Error(w, `{"error":{"message":"unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		switch {
		case p == sessionPath:
			w.Write([]byte(`{"id":"u-1","name":"admin","display_name":"Admin","privileges":["ADMINISTRATION","CAN_USE_SPOTTER"]}`))
		case p == metadataPath:
			if f.modelStatus != 0 {
				http.Error(w, `{"error":{"message":"forbidden"}}`, f.modelStatus)
				return
			}
			if f.noModel {
				w.Write([]byte(`[]`))
				return
			}
			var cols []any
			for _, c := range modelCols {
				cols = append(cols, map[string]any{"header": map[string]any{"name": c, "id": "c-" + c}})
			}
			json.NewEncoder(w).Encode([]any{map[string]any{"metadata_id": modelID, "metadata_name": defaultModel,
				"metadata_type": "LOGICAL_TABLE", "metadata_detail": map[string]any{"columns": cols}}})
		case p == searchPath:
			f.mu.Lock()
			f.searches = append(f.searches, body)
			if f.throttle > 0 {
				f.throttle--
				f.mu.Unlock()
				http.Error(w, "busy", http.StatusTooManyRequests)
				return
			}
			f.mu.Unlock()
			if body["logical_table_identifier"] != modelID {
				f.t.Errorf("searchdata on %v", body["logical_table_identifier"])
			}
			q := body["query_string"].(string)
			if strings.HasPrefix(q, "unique count") {
				convs := map[any]bool{}
				for _, r := range f.rows {
					convs[r[4]] = true
				}
				json.NewEncoder(w).Encode(map[string]any{"contents": []any{map[string]any{"column_names": []string{"Unique Number of Conversation Id"},
					"data_rows": [][]any{{len(convs)}}, "available_data_row_count": 1}}})
				return
			}
			off, size := int(body["record_offset"].(float64)), int(body["record_size"].(float64))
			end := min(off+size, len(f.rows))
			page := [][]any{}
			if off < len(f.rows) {
				page = f.rows[off:end]
			}
			json.NewEncoder(w).Encode(map[string]any{"contents": []any{map[string]any{"column_names": modelCols, "data_rows": page,
				"record_offset": off, "record_size": size, "returned_data_row_count": len(page), "available_data_row_count": len(f.rows)}}})
		case p == convsPath:
			if f.savedStatus != 0 {
				http.Error(w, `{"error":{"message":"nope"}}`, f.savedStatus)
				return
			}
			off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			lim, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			end := min(off+lim, len(f.convs))
			page := []map[string]any{}
			if off < len(f.convs) {
				page = f.convs[off:end]
			}
			json.NewEncoder(w).Encode(map[string]any{"conversations": page, "has_more": end < len(f.convs)})
		case strings.HasSuffix(p, "/messages"):
			id := strings.TrimSuffix(strings.TrimPrefix(p, convsPath+"/"), "/messages")
			if m, ok := f.msgs[id]; ok {
				json.NewEncoder(w).Encode(m)
				return
			}
			http.NotFound(w, r)
		case strings.HasSuffix(p, "/details"):
			w.Write([]byte(`{"title":"Sales by state","tokens":["[sales] [state]"],"formulas":[],"parameters":[],"visualization_type":"COLUMN"}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// analyticsRows builds n conversations of k turns, a minute apart, newest
// first.
func analyticsRows(n, k int) [][]any {
	var out [][]any
	for i := 0; i < n*k; i++ {
		at := base.Add(-time.Duration(i) * time.Minute)
		conv := fmt.Sprintf("conv-%04d", i/k)
		rating := "{Null}"
		if i%7 == 0 {
			rating = "Downvote"
		}
		out = append(out, []any{fmt.Sprintf("question %d", i), fmt.Sprintf("answer %d", i), rating, at.Format(tsLayout),
			conv, "Ann Analyst", "Retail Sales"})
	}
	return out
}

func tokenInputs(url string) agent.Inputs {
	return agent.Inputs{"host": url, "auth": "token", "token": bearer}
}

func payloads(t *testing.T, c agent.Collected) []map[string]any {
	var out []map[string]any
	for _, r := range c.Records {
		var p struct {
			Row map[string]any `json:"row"`
		}
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Row)
	}
	return out
}

func checkPlan(t *testing.T, in agent.Inputs, opts agent.Options, c agent.Collected) {
	t.Helper()
	planned := map[string]bool{}
	for _, q := range plan(in, opts) {
		planned[strings.TrimSuffix(q.Purpose, " (only when the analytics model cannot be read)")] = true
	}
	for _, q := range c.Queries {
		if !planned[q.Purpose] {
			t.Errorf("ran %q (%s), not in Plan", q.Purpose, q.Statement)
		}
	}
}

func TestAnalyticsModelPages(t *testing.T) {
	f := &fakeTS{t: t, rows: analyticsRows(500, 5), throttle: 1}
	srv := f.server()
	defer srv.Close()
	in := tokenInputs(srv.URL)
	opts := agent.Options{}
	c, err := agent.CollectAll(context.Background(), collect, in, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Records) != 2500 || c.Conversations != 500 || c.Sampled {
		t.Fatalf("%d records, %d conversations", len(c.Records), c.Conversations)
	}
	rows := payloads(t, c)
	r := rows[0]
	if r["question_text"] != "question 0" || r["answer_text"] != "answer 0" || r["conversation_id"] != "conv-0000" ||
		r["asked_at"] != "2026-10-01T12:00:00Z" || r["semantic_model_ref"] != "Retail Sales" {
		t.Errorf("row = %v", r)
	}
	if fb := r["extra"].(map[string]any)["feedback"].(map[string]any); fb["rating"] != "Downvote" || fb["score"] != -1.0 {
		t.Errorf("feedback = %v", fb)
	}
	if _, ok := rows[1]["extra"].(map[string]any)["feedback"]; ok {
		t.Error("{Null} rating became feedback")
	}
	if _, ok := r["user_name"]; ok || strings.Contains(string(c.Records[0].Payload), "Ann Analyst") {
		t.Error("user name kept without --include-users")
	}
	if rows[0]["turn_id"] == rows[1]["turn_id"] {
		t.Error("turn ids collide")
	}
	// 1 throttled + 3 pages.
	if len(f.searches) != 4 {
		t.Errorf("%d searchdata calls", len(f.searches))
	}
	q := f.searches[1]["query_string"].(string)
	if !strings.HasPrefix(q, "[User Query] [Response] [User Feedback Rating] [Timestamp] [Conversation Id] [User Name] [Data Source]") ||
		!strings.HasSuffix(q, "sort by [Timestamp] descending") {
		t.Errorf("query = %q", q)
	}
	checkPlan(t, in, opts, c)
	if len(c.Redactions) != 1 {
		t.Errorf("redactions = %v", c.Redactions)
	}
}

func TestAnalyticsCapAndSince(t *testing.T) {
	f := &fakeTS{t: t, rows: analyticsRows(500, 5)}
	srv := f.server()
	defer srv.Close()
	c, err := agent.CollectAll(context.Background(), collect, tokenInputs(srv.URL), agent.Options{MaxConversations: 10, IncludeUsers: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Records) != 50 || c.Conversations != 10 || !c.Sampled {
		t.Fatalf("%d records, %d conversations", len(c.Records), c.Conversations)
	}
	if len(f.searches) > 1+drainPages {
		t.Errorf("%d pages read under the cap", len(f.searches))
	}
	if !strings.Contains(strings.Join(c.Notes, "\n"), "--max-conversations 10") {
		t.Errorf("notes = %v", c.Notes)
	}
	if payloads(t, c)[0]["user_name"] != "Ann Analyst" {
		t.Error("user name dropped with --include-users")
	}

	f.searches = nil
	since := base.Add(-30 * time.Minute)
	c, err = agent.CollectAll(context.Background(), collect, tokenInputs(srv.URL), agent.Options{Since: since})
	if err != nil {
		t.Fatal(err)
	}
	// The fake ignores the filter: the reader drops older rows itself.
	if len(c.Records) != 31 {
		t.Errorf("%d records since %s", len(c.Records), since)
	}
	if q := f.searches[0]["query_string"].(string); !strings.Contains(q, "[Timestamp] >= '10/01/2026'") {
		t.Errorf("query = %q", q)
	}
}

func savedFixture(f *fakeTS) {
	f.noModel = true
	f.convs = []map[string]any{
		{"conversation_identifier": "c-old", "conversation_title": "Old", "created_at": "2026-08-01T10:00:00Z", "updated_at": "2026-08-01T10:00:00Z"},
		{"conversation_identifier": "c-new", "conversation_title": "Sales", "created_at": "2026-10-01T10:00:00Z", "updated_at": "2026-10-01T10:05:00Z",
			"data_source_names": []any{map[string]any{"id": "ds1", "name": "Retail Sales"}}, "analyst_id": nil},
	}
	f.msgs = map[string]any{
		"c-new": map[string]any{"messages": []any{
			map[string]any{"message_id": "m1", "timestamp_in_millis": base.UnixMilli(),
				"user_prompt": map[string]any{"message": "Sales by state?", "attachments": []any{}},
				"response_items": []any{
					map[string]any{"type": "text", "content": "Here are sales by state."},
					map[string]any{"type": "answer", "answer_id": "a1", "content": "Sales by state"},
					map[string]any{"type": "notification", "content": "Using Retail Sales"},
				}},
		}, "code_execution_files": []any{}},
		"c-old": map[string]any{"messages": []any{
			map[string]any{"message_id": "m0", "timestamp_in_millis": base.Add(-60 * 24 * time.Hour).UnixMilli(),
				"user_prompt": map[string]any{"message": "Old?"}, "response_items": []any{}},
		}},
	}
}

func TestAutoFallsBackToSavedConversations(t *testing.T) {
	f := &fakeTS{t: t}
	savedFixture(f)
	srv := f.server()
	defer srv.Close()
	in := agent.Inputs{"host": srv.URL, "auth": "secret-key", "username": "admin", "secret": secret}
	opts := agent.Options{Since: base.Add(-7 * 24 * time.Hour), MaxConversations: 5}
	c, err := agent.CollectAll(context.Background(), collect, in, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Records) != 1 || c.Conversations != 1 {
		t.Fatalf("%d records, %d conversations", len(c.Records), c.Conversations)
	}
	r := payloads(t, c)[0]
	if r["question_text"] != "Sales by state?" || r["answer_text"] != "Here are sales by state." || r["user_id"] != "u-1" ||
		r["semantic_model_ref"] != "Retail Sales" {
		t.Errorf("row = %v", r)
	}
	tc := r["tool_calls"].([]any)[0].(map[string]any)
	if tc["name"] != "thoughtspot_answer" || !strings.Contains(fmt.Sprint(tc["input"]), "[sales] [state]") {
		t.Errorf("tool call = %v", tc)
	}
	if len(r["messages"].([]any)) != 1 {
		t.Errorf("messages = %v", r["messages"])
	}
	if !strings.Contains(strings.Join(c.Notes, "\n"), "could not be read, so only your own saved conversations") {
		t.Errorf("notes = %v", c.Notes)
	}
	checkPlan(t, in, opts, c)
	for _, q := range c.Queries {
		if strings.Contains(q.Statement, secret) {
			t.Error("secret in a query statement")
		}
	}
}

func TestRouteErrors(t *testing.T) {
	f := &fakeTS{t: t, rows: analyticsRows(2, 2), modelStatus: http.StatusForbidden}
	srv := f.server()
	defer srv.Close()
	ctx := context.Background()
	in := tokenInputs(srv.URL)
	in["route"] = routeModel
	if _, err := agent.CollectAll(ctx, collect, in, agent.Options{}); err == nil || !strings.Contains(err.Error(), "ADMINISTRATION") {
		t.Errorf("403 model: %v", err)
	}
	if _, err := check(ctx, in, agent.Options{}); err == nil || !strings.Contains(err.Error(), "ADMINISTRATION") {
		t.Errorf("check 403 model: %v", err)
	}
	in["route"] = routeSaved
	f.savedStatus = http.StatusForbidden
	if _, err := agent.CollectAll(ctx, collect, in, agent.Options{}); err == nil || !strings.Contains(err.Error(), "CAN_USE_SPOTTER") {
		t.Errorf("403 saved: %v", err)
	}
	f.savedStatus = http.StatusNotFound
	if _, err := check(ctx, in, agent.Options{}); err == nil || !strings.Contains(err.Error(), "26.7.0.cl") {
		t.Errorf("404 saved: %v", err)
	}
	in["route"] = routeAuto
	if _, err := check(ctx, in, agent.Options{}); err == nil || !strings.Contains(err.Error(), "neither route") {
		t.Errorf("auto, both down: %v", err)
	}
}

func TestCheckCounts(t *testing.T) {
	f := &fakeTS{t: t, rows: analyticsRows(30, 2)}
	savedFixture(f)
	f.noModel = false
	srv := f.server()
	defer srv.Close()
	res, err := check(context.Background(), tokenInputs(srv.URL), agent.Options{MaxConversations: 20})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(res.Facts, "\n")
	for _, want := range []string{"as admin", "60 Spotter questions in total", "30 conversations in total: the 20 most recent will be sent",
		"saved conversations readable"} {
		if !strings.Contains(all, want) {
			t.Errorf("facts lack %q: %v", want, res.Facts)
		}
	}
	if res.Conversations == nil || *res.Conversations != 30 || len(res.Warnings) != 0 {
		t.Errorf("conversations = %v, warnings = %v", res.Conversations, res.Warnings)
	}
}

func TestVerify(t *testing.T) {
	f := &fakeTS{t: t}
	srv := f.server()
	defer srv.Close()
	ctx := context.Background()
	for _, mode := range []string{"secret-key", "password"} {
		fact, err := verify(ctx, agent.Inputs{"host": srv.URL, "auth": mode, "username": "admin", "secret": secret})
		if err != nil || fact != "Signed in to ThoughtSpot as admin" {
			t.Errorf("%s: %q, %v", mode, fact, err)
		}
	}
	_, err := verify(ctx, agent.Inputs{"host": srv.URL, "auth": "secret-key", "username": "admin", "secret": "wrong-one"})
	if err == nil || strings.Contains(err.Error(), "wrong-one") || !strings.Contains(err.Error(), "refused the sign-in") {
		t.Errorf("bad secret: %v", err)
	}
	if _, err := verify(ctx, agent.Inputs{"host": srv.URL, "token": "nope"}); err == nil || !strings.Contains(err.Error(), "rejected the token") {
		t.Errorf("bad token: %v", err)
	}
}

func TestValidate(t *testing.T) {
	a, err := agent.Get("thoughtspot")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(agent.Inputs{"host": "http://acme.thoughtspot.cloud", "token": "t"}); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http host: %v", err)
	}
	if err := a.Validate(agent.Inputs{"host": "acme.thoughtspot.cloud", "auth": "secret-key", "secret": "x"}); err == nil ||
		!strings.Contains(err.Error(), "--username") {
		t.Errorf("missing username: %v", err)
	}
	if err := a.Validate(agent.Inputs{"host": "acme.thoughtspot.cloud", "token": "t"}); err != nil {
		t.Errorf("token: %v", err)
	}
}

func TestParsing(t *testing.T) {
	for _, v := range []any{1759320000.0, 1759320000000.0, "2025-10-01T12:00:00Z", "2025-10-01 12:00:00", "10/01/2025 12:00:00"} {
		if got, ok := parseTime(v); !ok || !got.Equal(time.Date(2025, 10, 1, 12, 0, 0, 0, time.UTC)) {
			t.Errorf("parseTime(%v) = %v, %v", v, got, ok)
		}
	}
	for name, want := range map[string]role{"User Query": roleQuestion, "Response": roleAnswer, "User Feedback Rating": roleRating,
		"Timestamp": roleTime, "Conversation ID": roleConversation, "User": roleUserName, "Spotter Response Time": ""} {
		if got := roleOf(name); got != want {
			t.Errorf("roleOf(%q) = %q, want %q", name, got, want)
		}
	}
	if fb := feedback("Upvote", ""); fb["score"] != 1 {
		t.Errorf("upvote = %v", fb)
	}
}

func TestContext(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"Retail.model.tml": "model:\n  name: Retail\n", "orders.table.tml": "table:\n  name: orders\n",
		"wh.connection.tml": "connection:\n  password: x\n", "Sales.liveboard.tml": "liveboard:\n  name: Sales\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := checkContext(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Facts[0], "1 model, 1 table, 1 liveboard") || !strings.Contains(res.Facts[1], "3 files") {
		t.Errorf("facts = %v", res.Facts)
	}
	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, "x.liveboard.tml"), []byte("liveboard: {}\n"), 0o644)
	if _, err := checkContext(empty); err == nil {
		t.Error("liveboards only passed")
	}
}
