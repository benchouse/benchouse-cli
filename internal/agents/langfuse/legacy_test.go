package langfuse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// v3Fixture is distilled from a real self-hosted Langfuse v3.225.15
// (langfuse/langfuse:3): its 404 for the v2 observations API, four traces
// from GET /api/public/traces/{id} and their observations from
// GET /api/public/observations?traceId=. trace-000 and trace-010 share a
// session; trace-000 and trace-004 have an SDK-style root span (trace-004's
// at level ERROR); trace-001 has only top-level observations, its question
// and answer on the trace.
type v3Fixture struct {
	V2404        map[string]any              `json:"v2_404"`
	Traces       []map[string]any            `json:"traces"`
	Observations map[string][]map[string]any `json:"observations"`
}

func serveV3(t *testing.T, pageSize int) (*httptest.Server, *[]string) {
	t.Helper()
	b, err := os.ReadFile("testdata/v3-legacy.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx v3Fixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)
		q := r.URL.Query()
		switch r.URL.Path {
		case endpoint:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(fx.V2404)
		case projectsPath:
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "p", "name": "realruns"}}})
		case legacyTraces:
			page, _ := strconv.Atoi(q.Get("page"))
			limit := pageSize
			if l, _ := strconv.Atoi(q.Get("limit")); l < limit {
				limit = l
			}
			from, to := (page-1)*limit, page*limit
			data := []map[string]any{}
			for i, tr := range fx.Traces {
				if i >= from && i < to {
					data = append(data, tr)
				}
			}
			pages := (len(fx.Traces) + limit - 1) / limit
			json.NewEncoder(w).Encode(map[string]any{"data": data,
				"meta": map[string]any{"page": page, "limit": limit, "totalItems": len(fx.Traces), "totalPages": pages}})
		case legacyObs:
			obs := fx.Observations[q.Get("traceId")]
			json.NewEncoder(w).Encode(map[string]any{"data": obs,
				"meta": map[string]any{"page": 1, "limit": 100, "totalItems": len(obs), "totalPages": 1}})
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestLangfuseV3WithoutV2ObservationsReadsV1(t *testing.T) {
	srv, seen := serveV3(t, 2)
	a, _ := agent.Get("langfuse")
	in := agent.Inputs{"host": srv.URL, "public-key": "pk-lf-1", "secret-key": "sk-lf-1"}
	res, err := agent.CollectAll(context.Background(), a.Collect, in, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := rows(t, res)
	if len(got) != 4 || res.Result.Conversations != 3 {
		t.Fatalf("%d turns, %d conversations: %v", len(got), res.Result.Conversations, res.Result.Notes)
	}
	r0 := got["trace-000"]
	if r0["question_text"] != "What was revenue in week 0?" || r0["answer_text"] != "Revenue in week 0 was 1,000 EUR." ||
		r0["conversation_id"] != "sess-0" || r0["user_id"] != nil || r0["model_name"] != "claude-haiku-4-5" ||
		r0["outcome"] != "completed" {
		t.Errorf("trace-000 = %v", r0)
	}
	st := r0["statements"].([]any)
	if len(st) != 1 || !strings.HasPrefix(st[0].(map[string]any)["sql"].(string), "SELECT sum(amount) FROM orders") {
		t.Errorf("statements = %v", st)
	}
	if tc := r0["tool_calls"].([]any); len(tc) != 1 || tc[0].(map[string]any)["name"] != "lookup_docs" {
		t.Errorf("tool calls = %v (the root span is not a tool call)", tc)
	}
	r1 := got["trace-001"]
	if r1["question_text"] != "What was revenue in week 1?" || r1["answer_text"] != "Revenue in week 1 was 1,001 EUR." ||
		r1["user_id"] != "user-1" || r1["input_tokens"] != float64(101) || len(r1["statements"].([]any)) != 1 {
		t.Errorf("trace-001 (no root span) = %v", r1)
	}
	if r4 := got["trace-004"]; r4["outcome"] != "failed" || r4["error_message"] != "warehouse timeout" {
		t.Errorf("trace-004 = %v", r4)
	}
	for _, r := range res.Records {
		if s := string(r.Payload); strings.Contains(s, "canary-row") || strings.Contains(s, "canary-result-value") {
			t.Errorf("result rows kept: %s", s)
		}
	}
	// The plan lists every request that ran.
	planned := map[string]bool{}
	for _, q := range a.Plan(in, agent.Options{}) {
		planned[q.Purpose] = true
	}
	for _, q := range res.Result.Queries {
		if !planned[q.Purpose] {
			t.Errorf("ran %q, not in the plan", q.Purpose)
		}
	}
	if len(*seen) < 1+2+4 {
		t.Errorf("requests: %v", *seen)
	}

	// The conversation cap counts sessions: two keep sess-0's two traces and sess-1's.
	res, err = agent.CollectAll(context.Background(), a.Collect, in, agent.Options{MaxConversations: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := rows(t, res); len(got) != 3 || got["trace-004"] != nil {
		t.Errorf("cap 2: %d turns", len(got))
	}
}

func TestLangfuseV3Check(t *testing.T) {
	srv, _ := serveV3(t, 50)
	a, _ := agent.Get("langfuse")
	res, err := a.Check(context.Background(), agent.Inputs{"host": srv.URL, "public-key": "pk-lf-1", "secret-key": "sk-lf-1"}, agent.Options{})
	if err != nil || len(res.Facts) != 3 || !strings.Contains(res.Facts[2], "4 traces") {
		t.Errorf("facts %q, err %v", res.Facts, err)
	}
}
