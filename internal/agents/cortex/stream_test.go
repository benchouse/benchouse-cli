package cortex

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// sampleSnowflake serves a capped collection: 3 most recent traces, their
// spans by bound trace id, usage by bound request id, and counts.
func sampleSnowflake(t *testing.T, seen *[]string, bindings *[]map[string]map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Statement string
			Bindings  map[string]map[string]string
		}
		json.NewDecoder(r.Body).Decode(&req)
		st := req.Statement
		*seen = append(*seen, st)
		if req.Bindings != nil {
			*bindings = append(*bindings, req.Bindings)
		}
		rows := []string{}
		col := "ROW_JSON"
		switch {
		case strings.HasPrefix(st, "SELECT COUNT(DISTINCT"):
			col, rows = "N", []string{"500"}
		case strings.HasPrefix(st, "SELECT COUNT(*)"):
			col, rows = "N", []string{"40"}
		case strings.Contains(st, "GROUP BY TRACE"):
			rows = []string{`{"TRACE_ID":"t3"}`, `{"TRACE_ID":"t2"}`, `{"TRACE_ID":"t1"}`}
		case strings.Contains(st, "FROM SNOWFLAKE.LOCAL.AI_OBSERVABILITY_EVENTS"):
			// Each trace id is bound twice (trace match + record-id match);
			// emit each trace's spans once.
			done := map[string]bool{}
			for i := 1; i <= len(req.Bindings); i++ {
				id := req.Bindings[fmt.Sprint(i)]["value"]
				if done[id] {
					continue
				}
				done[id] = true
				rows = append(rows, fmt.Sprintf(`{"TIMESTAMP":"2026-10-01","TRACE":{"trace_id":"%s","span_id":"s"},"RECORD_ATTRIBUTES":{"ai.observability.record_id":"rec-%s","snow.ai.observability.agent.request_id":"r-%s"}}`, id, id, id))
				// Snowflake writes the request span without a trace id; only the
				// record-id match brings it, with the asker on it.
				if strings.Contains(st, `"ai.observability.record_id"`) {
					rows = append(rows, fmt.Sprintf(`{"TIMESTAMP":"2026-10-01","TRACE":{},"RECORD_ATTRIBUTES":{"ai.observability.record_id":"rec-%s","snow.ai.observability.user.name":"ASKER-%s"}}`, id, id))
				}
			}
		case strings.Contains(st, "FROM SNOWFLAKE.ACCOUNT_USAGE.CORTEX_AGENT_USAGE_HISTORY"):
			for _, b := range req.Bindings {
				rows = append(rows, fmt.Sprintf(`{"REQUEST_ID":"%s","START_TIME":"2026-10-01"}`, b["value"]))
			}
		case strings.Contains(st, "FROM SNOWFLAKE.LOCAL.CORTEX_ANALYST_REQUESTS_V"):
			rows = []string{`{"REQUEST_ID":"a2"}`, `{"REQUEST_ID":"a1"}`}
		case strings.Contains(st, "FROM SNOWFLAKE.ACCOUNT_USAGE"):
		default:
			t.Errorf("unexpected statement %s", st)
		}
		data := make([][]*string, len(rows))
		for i := range rows {
			data[i] = []*string{&rows[i]}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"resultSetMetaData": map[string]any{"rowType": []map[string]string{{"name": col}}},
			"data":              data,
		})
	}))
}

func TestCapChoosesTheMostRecentTracesAndRequests(t *testing.T) {
	var seen []string
	var binds []map[string]map[string]string
	srv := sampleSnowflake(t, &seen, &binds)
	defer srv.Close()
	opts := agent.Options{Since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), MaxConversations: 3}
	res, err := collectWith(t, srv, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	c := map[string]int{}
	for _, r := range res.Records {
		c[r.RecordKind]++
	}
	// 3 traces' spans plus their 3 request spans, which carry no trace id.
	if c["span"] != 6 || c["billing_usage"] != 3 || c["analyst_request"] != 2 {
		t.Errorf("counts = %v", c)
	}
	askers := 0
	for _, r := range res.Records {
		if strings.Contains(string(r.Payload), "ASKER-") {
			askers++
		}
	}
	if askers != 3 {
		t.Errorf("request spans with the asker = %d, want 3 (one per chosen trace)", askers)
	}
	// 2 Analyst requests + 3 traces; 40 + 500 in the window.
	if !res.Sampled || res.Conversations != 5 || res.InWindow == nil || *res.InWindow != 540 {
		t.Errorf("sample: %d of %v", res.Conversations, res.InWindow)
	}
	all := strings.Join(seen, "\n")
	for _, want := range []string{
		"FROM SNOWFLAKE.LOCAL.CORTEX_ANALYST_REQUESTS_V WHERE TIMESTAMP >= '2026-08-01 00:00:00'::timestamp_ntz ORDER BY TIMESTAMP DESC LIMIT 3",
		"FROM SNOWFLAKE.ACCOUNT_USAGE.CORTEX_ANALYST_USAGE_HISTORY WHERE START_TIME >= '2026-08-01 00:00:00'::timestamp_ltz ORDER BY START_TIME DESC LIMIT 15",
		`ORDER BY MAX(TIMESTAMP) DESC LIMIT 3`,
		`AND (TRACE:"trace_id"::string IN (?, ?, ?) OR RECORD_ATTRIBUTES:"ai.observability.record_id"::string IN (SELECT`,
		"AND REQUEST_ID IN (?, ?, ?) ORDER BY START_TIME",
		"AGENT_TYPE IS NOT NULL AND START_TIME >= '2026-08-01 00:00:00'::timestamp_ltz ORDER BY START_TIME DESC LIMIT 15",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no statement holds %q:\n%s", want, all)
		}
	}
	// Ids are bound by the server, never spliced into the SQL.
	if strings.Contains(all, "'t1'") || len(binds) != 2 || binds[0]["1"]["value"] != "t3" || binds[0]["4"]["value"] != "t3" || binds[0]["1"]["type"] != "TEXT" {
		t.Errorf("bindings = %v", binds)
	}
	var spans, usage bool
	for _, q := range res.Queries {
		if strings.Contains(q.Statement, "<up to 200 trace ids>") && q.Batches == 1 && q.Rows == 6 {
			spans = true
		}
		if strings.Contains(q.Statement, "<up to 200 request ids>") && q.Batches == 1 && q.Rows == 3 {
			usage = true
		}
	}
	if !spans || !usage {
		t.Errorf("batched queries not recorded: %+v", res.Queries)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "500 Cortex Agents requests in the window: the 3 most recent were read") {
		t.Errorf("notes = %v", res.Notes)
	}

	// The plan names every statement, batches by their placeholder.
	a, _ := agent.Get("snowflake-cortex")
	plan := a.Plan(agent.Inputs{"product": "both"}, opts)
	var planned []string
	for _, q := range plan {
		planned = append(planned, q.Statement)
	}
	p := strings.Join(planned, "\n")
	for _, want := range []string{"<up to 200 trace ids>", "<up to 200 request ids>", "GROUP BY TRACE", "COUNT(DISTINCT"} {
		if !strings.Contains(p, want) {
			t.Errorf("plan lacks %q:\n%s", want, p)
		}
	}
}

func TestCheckSaysWhenTheCapApplies(t *testing.T) {
	f := &fakeCheck{warehouse: "WH", counts: map[string]string{analystV: "312", eventsV: "1204"}}
	res, err := checkWith(t, f, "snowflake-cortex", nil, agent.Options{MaxConversations: 1000})
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Join(res.Facts, "\n")
	if !strings.Contains(facts, "1,204 Cortex Agents requests in total: the 1,000 most recent will be sent") ||
		strings.Contains(facts, "312 Cortex Analyst requests in total:") {
		t.Errorf("facts = %s", facts)
	}
}
