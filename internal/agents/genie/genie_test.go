package genie

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
)

const sp = "01f19596d9c91b95b1efe87e93ae213a"

// fakeWorkspace answers the Databricks APIs the reader uses. A table mapped
// to nil answers 403, as a missing system-table grant does; a message absent
// from messages answers 403, as Genie does for a conversation the token may
// not read; a message in limited answers 429 once.
type fakeWorkspace struct {
	t        *testing.T
	tables   map[string][]string
	messages map[string]string
	limited  map[string]bool
	users    int
	// pendingAudit makes the audit statement outlive its wait and arrive in
	// two chunks, to exercise polling and chunk links.
	pendingAudit bool
	seen         []string
	// answer, if set, is asked first: rows for a statement and its bound
	// parameters, or false to fall through to tables.
	answer func(stmt string, params map[string]string) ([]string, bool)
}

func (f *fakeWorkspace) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			f.t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPost && p == "/api/2.0/sql/statements":
			var req struct {
				Statement   string `json:"statement"`
				WarehouseID string `json:"warehouse_id"`
				Parameters  []param
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.WarehouseID != "wh1" {
				f.t.Errorf("warehouse_id = %q", req.WarehouseID)
			}
			f.seen = append(f.seen, req.Statement)
			params := map[string]string{}
			for _, p := range req.Parameters {
				params[p.Name] = p.Value
			}
			if f.answer != nil {
				if rows, ok := f.answer(req.Statement, params); ok {
					writeRows(w, "st", "SUCCEEDED", rows, "")
					return
				}
			}
			if f.chosen(w, req.Statement, params) {
				return
			}
			f.statement(w, req.Statement)
		case r.Method != http.MethodGet:
			f.t.Errorf("not a read: %s %s", r.Method, p)
			http.Error(w, "{}", http.StatusMethodNotAllowed)
		case p == "/api/2.0/sql/statements/st-audit":
			f.seen = append(f.seen, "poll")
			rows := f.tables["system.access.audit"]
			writeRows(w, "st-audit", "SUCCEEDED", rows[:1], "/api/2.0/sql/statements/st-audit/result/chunks/1")
		case p == "/api/2.0/sql/statements/st-audit/result/chunks/1":
			f.seen = append(f.seen, "chunk")
			writeRows(w, "", "", f.tables["system.access.audit"][1:], "")
		case strings.HasPrefix(p, "/api/2.0/genie/spaces/"+sp+"/conversations/"):
			f.seen = append(f.seen, "GET "+p)
			msg := p[strings.LastIndex(p, "/")+1:]
			if f.limited[msg] {
				delete(f.limited, msg)
				http.Error(w, `{"error_code":"RESOURCE_EXHAUSTED"}`, http.StatusTooManyRequests)
				return
			}
			body, ok := f.messages[msg]
			if !ok {
				http.Error(w, `{"error_code":"PERMISSION_DENIED"}`, http.StatusForbidden)
				return
			}
			w.Write([]byte(body))
		case p == "/api/2.0/preview/scim/v2/Users":
			f.seen = append(f.seen, "GET "+r.URL.String())
			var start int
			fmt.Sscanf(r.URL.Query().Get("startIndex"), "%d", &start)
			if start > f.users {
				w.Write([]byte(`{"totalResults":2,"Resources":[]}`))
				return
			}
			fmt.Fprintf(w, `{"totalResults":2,"Resources":[{"id":"u%d","userName":"ann%d@shop.test","displayName":"Ann %d"}]}`, start, start, start)
		case p == "/api/2.0/genie/spaces":
			f.seen = append(f.seen, "GET "+r.URL.String())
			if r.URL.Query().Get("page_token") == "" {
				w.Write([]byte(`{"spaces":[{"space_id":"s1","title":"Orders"}],"next_page_token":"p2"}`))
				return
			}
			w.Write([]byte(`{"spaces":[{"space_id":"s2","title":""}]}`))
		default:
			f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, "{}", http.StatusNotFound)
		}
	}))
}

func (f *fakeWorkspace) statement(w http.ResponseWriter, stmt string) {
	for table, rows := range f.tables {
		if !strings.Contains(stmt, "FROM "+table+"\n") {
			continue
		}
		if rows == nil {
			http.Error(w, `{"error_code":"PERMISSION_DENIED","message":"no SELECT on `+table+`"}`, http.StatusForbidden)
			return
		}
		if table == "system.access.audit" && f.pendingAudit {
			writeRows(w, "st-audit", "PENDING", nil, "")
			return
		}
		writeRows(w, "st", "SUCCEEDED", rows, "")
		return
	}
	f.t.Errorf("unexpected statement %s", stmt)
	http.Error(w, "{}", http.StatusBadRequest)
}

// chosen answers the statements that choose conversations from the audit
// table and read their audit events in batches, the way Databricks would.
func (f *fakeWorkspace) chosen(w http.ResponseWriter, stmt string, params map[string]string) bool {
	rows, ok := f.tables["system.access.audit"]
	if !ok || rows == nil || !strings.Contains(stmt, "FROM system.access.audit") {
		return false
	}
	switch {
	case strings.Contains(stmt, "GROUP BY conversation_id"):
		last := map[string]string{}
		for _, r := range rows {
			conv, action, at := auditFields(r)
			if conv != "" && (action == "genieStartConversationMessage" || action == "genieCreateConversationMessage") && at > last[conv] {
				last[conv] = at
			}
		}
		convs := make([]string, 0, len(last))
		for c := range last {
			convs = append(convs, c)
		}
		sort.Slice(convs, func(i, j int) bool {
			if last[convs[i]] != last[convs[j]] {
				return last[convs[i]] > last[convs[j]]
			}
			return convs[i] < convs[j]
		})
		var n int
		if i := strings.LastIndex(stmt, " LIMIT "); i >= 0 {
			fmt.Sscanf(stmt[i+len(" LIMIT "):], "%d", &n)
			convs = convs[:min(n, len(convs))]
		}
		var out []string
		for _, c := range convs {
			out = append(out, fmt.Sprintf(`{"conversation_id":%q,"last_event_time":%q}`, c, last[c]))
		}
		writeRows(w, "st", "SUCCEEDED", out, "")
		return true
	case len(params) > 0:
		in := map[string]bool{}
		for _, v := range params {
			in[v] = true
		}
		var out []string
		for _, r := range rows {
			if conv, _, _ := auditFields(r); in[conv] {
				out = append(out, r)
			}
		}
		if f.pendingAudit {
			writeRows(w, "st-audit", "PENDING", nil, "")
			return true
		}
		writeRows(w, "st", "SUCCEEDED", out, "")
		return true
	}
	return false
}

// auditFields reads an audit row's conversation id (request or response),
// action and event time.
func auditFields(row string) (conv, action, at string) {
	var a struct {
		Action string `json:"action_name"`
		At     string `json:"event_time"`
		Params struct {
			Conv string `json:"conversation_id"`
		} `json:"request_params"`
		Response struct {
			Result string `json:"result"`
		} `json:"response"`
	}
	json.Unmarshal([]byte(row), &a)
	conv = a.Params.Conv
	if conv == "" {
		var r struct {
			Conv string `json:"conversation_id"`
		}
		json.Unmarshal([]byte(a.Response.Result), &r)
		conv = r.Conv
	}
	return conv, a.Action, a.At
}

func writeRows(w http.ResponseWriter, id, state string, rows []string, next string) {
	data := make([][]*string, len(rows))
	for i := range rows {
		data[i] = []*string{&rows[i]}
	}
	resp := map[string]any{"result": map[string]any{"data_array": data, "next_chunk_internal_link": next}}
	if id != "" {
		resp["statement_id"] = id
		resp["status"] = map[string]any{"state": state}
	}
	json.NewEncoder(w).Encode(resp)
}

func audit(id, action, params, result string) string {
	return fmt.Sprintf(`{"event_id":%q,"event_time":"2026-10-01T10:00:00.000Z","event_date":"2026-10-01",
"service_name":"aibiGenie","action_name":%q,"user_identity":{"email":"ann@shop.test"},
"request_params":%s,"response":{"status_code":200,"result":%q}}`, id, action, params, result)
}

// workspace is a space with three questions: m1 answered (rate-limited once),
// m2 a follow-up whose payload embeds result rows, m3 unreadable to the token.
func workspace(t *testing.T) *fakeWorkspace {
	return &fakeWorkspace{t: t,
		tables: map[string][]string{
			"system.access.audit": {
				audit("e1", "genieStartConversationMessage", `{"space_id":"`+sp+`"}`, `{"message_id":"m1","conversation_id":"c1"}`),
				audit("e2", "genieGetConversationMessage", `{"space_id":"`+sp+`","conversation_id":"c1","message_id":"m1"}`, ""),
				audit("e3", "genieCreateConversationMessage", `{"space_id":"`+sp+`","conversation_id":"c1"}`, `{"message_id":"m2","conversation_id":"c1"}`),
				audit("e4", "genieStartConversationMessage", `{"space_id":"`+sp+`"}`, `{"message_id":"m3","conversation_id":"c3"}`),
			},
			"system.query.history": {
				`{"statement_id":"st-old","start_time":"2026-08-01T09:00:00.000Z","query_source":{"genie_space_id":"` + sp + `"},"statement_text":"DESCRIBE orders"}`,
				`{"statement_id":"st-1","start_time":"2026-10-01T10:00:05.000Z","query_source":{"genie_space_id":"` + sp + `"},"statement_text":"SELECT count(*) FROM orders","produced_rows":1}`,
			},
			"system.billing.usage": nil,
		},
		messages: map[string]string{
			"m1": `{"id":"m1","space_id":"` + sp + `","conversation_id":"c1","user_id":72231541836345,"status":"COMPLETED",
"content":"How many orders?","attachments":[{"query":{"query":"SELECT count(*) FROM orders","statement_id":"st-1",
"query_result_metadata":{"row_count":1}},"attachment_id":"a1"},{"text":{"content":"There are 42 orders.","purpose":"TEXT_ATTACHMENT_PURPOSE_ANSWER"}}],
"query_result":{"statement_id":"st-1","row_count":1},"message_id":"m1"}`,
			"m2": `{"id":"m2","conversation_id":"c1","user_id":9007199254740993,"status":"COMPLETED","content":"By month?",
"attachments":[{"query":{"query":"SELECT month, count(*) FROM orders GROUP BY 1","statement_id":"st-2",
"query_result_metadata":{"row_count":2}},"attachment_id":"a2"}],
"query_result":{"statement_id":"st-2","row_count":2,"statement_response":{"result":{"data_array":[["2026-09","SECRET_ROW_VALUE"]]}}},"message_id":"m2"}`,
		},
		limited: map[string]bool{"m1": true},
		users:   2,
	}
}

func collectFrom(t *testing.T, f *fakeWorkspace, opts agent.Options, edit ...func(agent.Inputs)) (agent.Collected, error) {
	t.Helper()
	defer func(p, r time.Duration, n int) { pollEvery, retryAfter, scimPageSize = p, r, n }(pollEvery, retryAfter, scimPageSize)
	pollEvery, retryAfter, scimPageSize = time.Millisecond, time.Millisecond, 1
	srv := f.server()
	defer srv.Close()
	a, err := agent.Get("databricks-genie")
	if err != nil {
		t.Fatal(err)
	}
	in := agent.Inputs{"host": srv.URL, "token": "tok", "warehouse-id": "wh1", "space-id": sp}
	for _, e := range edit {
		e(in)
	}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	return agent.CollectAll(context.Background(), a.Collect, in, opts)
}

func counts(res agent.Collected) map[string]int {
	c := map[string]int{}
	for _, r := range res.Records {
		c[r.RecordKind]++
	}
	return c
}

func payloadOf(res agent.Collected, kind, key, id string) string {
	for _, r := range res.Records {
		if r.RecordKind == kind && r.NativeIDs[key] == id {
			return string(r.Payload)
		}
	}
	return ""
}

func TestCollectReconstructsConversationsFromAuditAndAPI(t *testing.T) {
	f := workspace(t)
	f.pendingAudit = true
	res, err := collectFrom(t, f, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := counts(res)
	if c["audit_event"] != 4 || c["conversation_message"] != 2 || c["statement_history"] != 2 || c["billing_usage"] != 0 {
		t.Errorf("counts = %v", c)
	}
	if c["directory_user"] != 0 {
		t.Error("the user directory was read without --include-users")
	}
	for _, s := range f.seen {
		if strings.Contains(s, "scim") {
			t.Error("SCIM was called without --include-users")
		}
	}
	// billing without the grant, and m3 refused by the API: both notes.
	if len(res.Notes) != 2 || !strings.Contains(res.Notes[0], "1 of 3 Genie messages") {
		t.Errorf("notes = %v", res.Notes)
	}
	for _, r := range res.Records {
		if r.SourceKind != "databricks_genie" || r.FetchMethod == "" {
			t.Errorf("record = %+v", r)
		}
	}

	// The ids of a started conversation come from response.result.
	for _, r := range res.Records {
		if r.RecordKind == "audit_event" && r.NativeIDs["event_id"] == "e1" {
			want := map[string]string{"event_id": "e1", "space_id": sp, "conversation_id": "c1", "message_id": "m1",
				"action_name": "genieStartConversationMessage"}
			if fmt.Sprint(r.NativeIDs) != fmt.Sprint(want) || r.FetchMethod != "sql:system.access.audit" {
				t.Errorf("audit record = %+v", r)
			}
		}
		if r.RecordKind == "conversation_message" && r.NativeIDs["message_id"] == "m1" {
			if r.NativeIDs["space_id"] != sp || r.NativeIDs["conversation_id"] != "c1" ||
				r.FetchMethod != "rest:GET /api/2.0/genie/spaces/{space}/conversations/{conv}/messages/{msg}" ||
				r.CursorValue != "2026-10-01T10:00:00.000Z" {
				t.Errorf("message record = %+v", r)
			}
		}
		if r.RecordKind == "statement_history" && r.NativeIDs["statement_id"] == "st-1" && r.NativeIDs["genie_space_id"] != sp {
			t.Errorf("history ids = %v", r.NativeIDs)
		}
	}

	// m1 is verbatim; m2 loses its embedded rows and keeps everything else.
	if p := payloadOf(res, "conversation_message", "message_id", "m1"); p != f.messages["m1"] {
		t.Errorf("m1 not verbatim: %s", p)
	}
	m2 := payloadOf(res, "conversation_message", "message_id", "m2")
	if strings.Contains(m2, "SECRET_ROW_VALUE") {
		t.Errorf("result rows survived: %s", m2)
	}
	for _, keep := range []string{"SELECT month, count(*) FROM orders GROUP BY 1", `"statement_id":"st-2"`, `"row_count":2`, "By month?", "9007199254740993"} {
		if !strings.Contains(m2, keep) {
			t.Errorf("lost %q: %s", keep, m2)
		}
	}
	if len(res.Redactions) != 1 {
		t.Errorf("redactions = %v", res.Redactions)
	}

	// m1 was rate limited once and retried; the poll and chunk were followed.
	var m1Calls, polls, chunks int
	for _, s := range f.seen {
		switch {
		case strings.HasSuffix(s, "/messages/m1"):
			m1Calls++
		case s == "poll":
			polls++
		case s == "chunk":
			chunks++
		}
	}
	if m1Calls != 2 || polls != 1 || chunks != 1 {
		t.Errorf("m1 calls %d, polls %d, chunks %d; seen %v", m1Calls, polls, chunks, f.seen)
	}
}

// boundIDs is a batch's id list, :id1, :id2, ...
var boundIDs = regexp.MustCompile(`IN \(:id\d+(, :id\d+)*\)`)

func TestPlanListsWhatCollectRuns(t *testing.T) {
	f := workspace(t)
	f.tables["system.billing.usage"] = []string{`{"record_id":"b1","usage_start_time":"2026-10-01T00:00:00.000Z","sku_name":"GENIE_FREE_USAGE"}`}
	opts := agent.Options{IncludeUsers: true, Since: time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)}
	res, err := collectFrom(t, f, opts)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := agent.Get("databricks-genie")
	defer func(n int) { scimPageSize = n }(scimPageSize)
	scimPageSize = 1 // the page size collectFrom ran with
	plan := a.Plan(agent.Inputs{"space-id": sp}, opts)
	if len(plan) != 6 || len(res.Queries) != 6 {
		t.Fatalf("plan %d, ran %d", len(plan), len(res.Queries))
	}
	for i, q := range res.Queries {
		if q.Statement != plan[i].Statement || q.Purpose != plan[i].Purpose {
			t.Errorf("ran %q, planned %q", q.Statement, plan[i].Statement)
		}
	}
	// Every request that reached the workspace is a planned statement or an
	// instance of a planned GET.
	for _, s := range f.seen {
		ok := false
		for _, q := range plan {
			switch {
			case s == q.Statement:
				ok = true
			case boundIDs.MatchString(s):
				ok = boundIDs.ReplaceAllString(s, "IN (<up to 200 conversation ids>)") == q.Statement
			case strings.HasPrefix(s, "GET /api/2.0/genie/spaces/"+sp+"/conversations/"):
				ok = strings.HasPrefix(q.Statement, "GET /api/2.0/genie/spaces/"+sp+"/conversations/{conversation_id}/messages/{message_id}")
			case strings.HasPrefix(s, "GET /api/2.0/preview/scim/v2/Users?"):
				ok = strings.HasPrefix(q.Statement, "GET /api/2.0/preview/scim/v2/Users?startIndex=1&count=")
			}
			if ok {
				break
			}
		}
		if !ok {
			t.Errorf("unplanned request %q", s)
		}
	}
	for _, q := range plan[:5] {
		if strings.Contains(q.Statement, "scim") {
			t.Errorf("SCIM planned out of place: %s", q.Statement)
		}
	}
	if p := a.Plan(agent.Inputs{}, agent.Options{}); len(p) != 5 ||
		!strings.HasSuffix(p[0].Statement, "ORDER BY last_event_time DESC") ||
		!strings.Contains(p[1].Statement, "IN (<up to 200 conversation ids>)") ||
		!strings.Contains(p[3].Statement, "genie_space_id IS NOT NULL") {
		t.Errorf("plan without space or users = %v", p)
	}
}

func TestSinceBoundsTheWindow(t *testing.T) {
	f := workspace(t)
	res, err := collectFrom(t, f, agent.Options{Since: time.Date(2026, 9, 15, 18, 30, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.seen[0], "AND event_date >= DATE '2026-09-15'") {
		t.Errorf("audit statement = %s", f.seen[0])
	}
	// query history is read unbounded, as in production, and filtered here.
	if c := counts(res); c["statement_history"] != 1 || payloadOf(res, "statement_history", "statement_id", "st-1") == "" {
		t.Errorf("counts = %v", c)
	}
	for _, q := range res.Queries {
		if strings.Contains(q.Statement, "system.query.history") && q.Rows != 2 {
			t.Errorf("history rows read = %d, want 2", q.Rows)
		}
	}
}

func TestIncludeResultsAndUsers(t *testing.T) {
	f := workspace(t)
	res, err := collectFrom(t, f, agent.Options{IncludeResults: true, IncludeUsers: true})
	if err != nil {
		t.Fatal(err)
	}
	if p := payloadOf(res, "conversation_message", "message_id", "m2"); p != f.messages["m2"] {
		t.Errorf("--include-results altered m2: %s", p)
	}
	if len(res.Redactions) != 0 {
		t.Errorf("redactions = %v", res.Redactions)
	}
	// Two users over pages of one: three requests, the last one empty.
	if c := counts(res); c["directory_user"] != 2 {
		t.Errorf("counts = %v", c)
	}
	if payloadOf(res, "directory_user", "id", "u1") == "" || payloadOf(res, "directory_user", "id", "u2") == "" {
		t.Error("a SCIM page was lost")
	}
	scim := 0
	for _, s := range f.seen {
		if strings.Contains(s, "scim") {
			scim++
		}
	}
	if scim != 3 {
		t.Errorf("scim requests = %d", scim)
	}
}

func TestAuditFailingIsAnError(t *testing.T) {
	f := workspace(t)
	f.tables["system.access.audit"] = nil
	if _, err := collectFrom(t, f, agent.Options{}); err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("err = %v", err)
	}
}

// The statements are the live connector's, byte for byte, layout included.
func TestStatementsMatchTheLiveConnector(t *testing.T) {
	want := "SELECT to_json(struct(*)) AS row_json FROM system.access.audit\n" +
		"             WHERE service_name = 'aibiGenie' AND request_params.space_id = '" + sp + "'" +
		" AND event_date >= DATE '2026-09-15' ORDER BY event_time"
	if got := auditArm.statement(sp, "2026-09-15"); got != want {
		t.Errorf("audit:\n%q\nwant\n%q", got, want)
	}
	want = "SELECT to_json(struct(*)) AS row_json FROM system.query.history\n" +
		"             WHERE query_source.genie_space_id = '" + sp + "' ORDER BY start_time"
	if got := historyArm.statement(sp, "2026-09-15"); got != want {
		t.Errorf("history:\n%q\nwant\n%q", got, want)
	}
	want = "SELECT to_json(struct(*)) AS row_json FROM system.billing.usage\n" +
		"             WHERE billing_origin_product = 'GENIE' AND usage_date >= DATE '2026-09-15' ORDER BY usage_start_time"
	if got := billingArm.statement(sp, "2026-09-15"); got != want {
		t.Errorf("billing:\n%q\nwant\n%q", got, want)
	}
}

func TestSpacesFollowsPages(t *testing.T) {
	f := workspace(t)
	srv := f.server()
	defer srv.Close()
	got, err := Spaces(context.Background(), agent.Inputs{"host": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[[s1 Orders] [s2 s2]]" {
		t.Errorf("spaces = %v", got)
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"dbc-1.cloud.databricks.com":          "https://dbc-1.cloud.databricks.com",
		" https://adb-2.azuredatabricks.net/": "https://adb-2.azuredatabricks.net",
		"http://127.0.0.1:8080":               "http://127.0.0.1:8080",
		"":                                    "",
	} {
		if got, err := normalizeHost(in); err != nil || got != want {
			t.Errorf("normalizeHost(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{
		"http://dbc-1.cloud.databricks.com",      // a token over plain http
		"https://dbc-1.cloud.databricks.com/x",   // a path
		"https://me@dbc-1.cloud.databricks.com",  // user info
		"https://dbc-1.cloud.databricks.com?a=b", // a query
		"ftp://dbc-1.cloud.databricks.com",
		"https://dbc_1 .cloud.databricks.com",
	} {
		if got, err := normalizeHost(in); err == nil {
			t.Errorf("normalizeHost(%q) = %q, want an error", in, got)
		}
	}
}

// A chunk link from the server is followed only under /api/ on the
// configured host: "@evil.tld/x" appended to the host would send the token
// to evil.tld.
func TestChunkLinkCannotLeaveTheHost(t *testing.T) {
	for _, next := range []string{"@evil.tld/x", "//evil.tld/api/x", "https://evil.tld/api/x",
		"/api/2.0/x@evil.tld", "/api//evil.tld", "/other/x", `/api/\evil.tld`} {
		evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("token sent to another host for %q", next)
		}))
		var calls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			writeRows(w, "st", "SUCCEEDED", []string{`{"a":1}`}, strings.ReplaceAll(next, "evil.tld", strings.TrimPrefix(evil.URL, "http://")))
		}))
		cl, err := newClient(srv.URL, "tok", "wh1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cl.queryJSONRows(context.Background(), "SELECT 1"); err == nil {
			t.Errorf("next link %q followed", next)
		}
		if calls != 1 {
			t.Errorf("next link %q: %d requests", next, calls)
		}
		srv.Close()
		evil.Close()
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("followed a redirect; Authorization = %q", r.Header.Get("Authorization"))
	}))
	defer evil.Close()
	srv := httptest.NewServer(http.RedirectHandler(evil.URL+"/api/2.0/sql/statements", http.StatusTemporaryRedirect))
	defer srv.Close()
	cl, err := newClient(srv.URL, "tok", "wh1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = cl.queryJSONRows(context.Background(), "SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("err = %v, want a refused redirect", err)
	}
}

// Ids from the audit log go into request paths; one that is not an id is
// skipped with a note, and the server never sees it.
func TestInvalidIDsAreNotFetched(t *testing.T) {
	audits := []bundle.Record{
		{NativeIDs: map[string]string{"action_name": "genieStartConversationMessage", "space_id": sp, "conversation_id": "c1", "message_id": "m1"}},
		{NativeIDs: map[string]string{"action_name": "genieStartConversationMessage", "space_id": sp, "conversation_id": "../../../2.0/preview/scim/v2/Users?", "message_id": "m2"}},
		{NativeIDs: map[string]string{"action_name": "genieStartConversationMessage", "space_id": "@evil.tld", "conversation_id": "c3", "message_id": "m3"}},
	}
	refs, invalid := pendingMessages(audits, sp)
	if len(refs) != 1 || refs[0].msg != "m1" || invalid != 2 {
		t.Errorf("refs = %v, invalid = %d", refs, invalid)
	}
	cl := &client{host: "http://127.0.0.1:1", token: "tok"}
	if _, _, err := cl.message(context.Background(), sp, "c1/../../x", "m1"); err == nil {
		t.Error("fetched a message with a path in its id")
	}
	// Refused by the field's format check, and by Collect when called
	// without it.
	a, _ := agent.Get("databricks-genie")
	bad := agent.Inputs{"host": "https://x.cloud.databricks.com", "token": "tok", "warehouse-id": "wh1", "space-id": "x' OR 1=1 --"}
	if err := a.Validate(bad); err == nil || !strings.Contains(err.Error(), "space id") {
		t.Errorf("Validate: err = %v, want a refused space id", err)
	}
	if _, err := agent.CollectAll(context.Background(), a.Collect, bad, agent.Options{}); err == nil || !strings.Contains(err.Error(), "space id") {
		t.Errorf("Collect: err = %v, want a refused space id", err)
	}
}
