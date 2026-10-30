package genie

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// Under a cap the reader chooses the most recent conversations, reads only
// their audit events and messages, the history rows of the statements those
// messages ran, and the most recent billing rows.
func TestCapChoosesTheMostRecentConversations(t *testing.T) {
	f := workspace(t)
	f.tables["system.billing.usage"] = []string{`{"record_id":"b1","usage_start_time":"2026-10-01"}`}
	var auditParams, historyParams map[string]string
	f.answer = func(stmt string, params map[string]string) ([]string, bool) {
		switch {
		case strings.Contains(stmt, "count(DISTINCT"):
			return []string{`{"n":3000,"conversations":1200}`}, true
		case strings.Contains(stmt, "GROUP BY conversation_id"):
			return []string{`{"conversation_id":"c1","last_event_time":"2026-10-01T10:00:00.000Z"}`}, true
		case strings.Contains(stmt, "FROM system.access.audit") && len(params) > 0:
			auditParams = params
			var out []string
			for _, row := range f.tables["system.access.audit"] {
				if strings.Contains(row, `"c1"`) || strings.Contains(row, `\"c1\"`) {
					out = append(out, row)
				}
			}
			return out, true
		case strings.Contains(stmt, "FROM system.query.history"):
			historyParams = params
			return f.tables["system.query.history"][1:], true
		}
		return nil, false
	}
	res, err := collectFrom(t, f, agent.Options{Since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), MaxConversations: 1})
	if err != nil {
		t.Fatal(err)
	}
	c := counts(res)
	// c1's three audit events and two messages; c3 (m3) not read.
	if c["audit_event"] != 3 || c["conversation_message"] != 2 || c["statement_history"] != 1 || c["billing_usage"] != 1 {
		t.Errorf("counts = %v", c)
	}
	for _, s := range f.seen {
		if strings.Contains(s, "/m3") {
			t.Error("a message of an unchosen conversation was fetched")
		}
	}
	if auditParams["id1"] != "c1" || len(auditParams) != 1 {
		t.Errorf("audit batch params = %v", auditParams)
	}
	if historyParams["id1"] != "st-1" || historyParams["id2"] != "st-2" {
		t.Errorf("history batch params = %v", historyParams)
	}
	if !res.Sampled || res.Conversations != 1 || res.InWindow == nil || *res.InWindow != 1200 {
		t.Errorf("sample: %d of %v", res.Conversations, res.InWindow)
	}
	all := strings.Join(f.seen, "\n")
	for _, want := range []string{"ORDER BY last_event_time DESC LIMIT 1", "IN (:id1) ORDER BY event_time",
		"statement_id IN (:id1, :id2)", "ORDER BY usage_start_time DESC LIMIT 5"} {
		if !strings.Contains(all, want) {
			t.Errorf("no statement holds %q:\n%s", want, all)
		}
	}
	notes := strings.Join(res.Notes, "\n")
	if !strings.Contains(notes, "1,200 Genie conversations in the window: the 1 most recent were read") {
		t.Errorf("notes = %v", res.Notes)
	}
	var batched bool
	for _, q := range res.Queries {
		if strings.Contains(q.Statement, "<up to 200 conversation ids>") && q.Batches == 1 && q.Rows == 3 {
			batched = true
		}
	}
	if !batched {
		t.Errorf("queries = %+v", res.Queries)
	}
}

func TestCheckCountsConversationsAgainstTheCap(t *testing.T) {
	// countStatement asks for both counts in one statement.
	s := countStatement(sp, "2026-08-01")
	if !strings.Contains(s, "count(*) AS n") || !strings.Contains(s, "count(DISTINCT") {
		t.Errorf("count statement = %s", s)
	}
	if got := agent.Conversations(1200, "Genie conversation", "Genie conversations", agent.Options{MaxConversations: 1000}); got !=
		"1,200 Genie conversations in total: the 1,000 most recent will be sent" {
		t.Errorf("fact = %q", got)
	}
}

// Without a cap every conversation is read, but still per batch of
// conversation ids: each batch's audit events, then its messages, so only
// one batch's message refs are held.
func TestNoCapReadsConversationsInBatches(t *testing.T) {
	const convs = 450
	f := workspace(t)
	f.limited = nil
	f.tables["system.billing.usage"] = []string{}
	f.tables["system.access.audit"] = nil
	f.messages = map[string]string{}
	var audit []string
	for i := range convs {
		c, m := fmt.Sprintf("c%03d", i), fmt.Sprintf("m%03d", i)
		at := fmt.Sprintf("2026-10-01T10:%02d:%02d.000Z", i/60, i%60)
		audit = append(audit, fmt.Sprintf(`{"event_id":"e%d","event_time":%q,"service_name":"aibiGenie",
"action_name":"genieStartConversationMessage","request_params":{"space_id":"`+sp+`"},
"response":{"status_code":200,"result":%q}}`, i, at, fmt.Sprintf(`{"message_id":%q,"conversation_id":%q}`, m, c)))
		f.messages[m] = fmt.Sprintf(`{"id":%q,"conversation_id":%q,"content":"q"}`, m, c)
	}
	f.tables["system.access.audit"] = audit
	var order []string // audit batches and message fetches, in the order they ran
	f.answer = func(stmt string, params map[string]string) ([]string, bool) {
		if len(params) > 0 && strings.Contains(stmt, "FROM system.access.audit") {
			order = append(order, fmt.Sprintf("audit:%d", len(params)))
		}
		return nil, false
	}
	res, err := collectFrom(t, f, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := counts(res)
	if c["audit_event"] != convs || c["conversation_message"] != convs || res.Conversations != convs {
		t.Fatalf("counts = %v, conversations %d", c, res.Conversations)
	}
	if strings.Join(order, ",") != "audit:200,audit:200,audit:50" {
		t.Errorf("audit batches = %v", order)
	}
	// The second batch's audit statement ran after the first batch's
	// messages were fetched.
	firstMsg, secondAudit := -1, -1
	n := 0
	for i, s := range f.seen {
		if strings.HasPrefix(s, "GET ") && firstMsg < 0 {
			firstMsg = i
		}
		if strings.Contains(s, "IN (:id1") {
			n++
			if n == 2 {
				secondAudit = i
			}
		}
	}
	if firstMsg < 0 || secondAudit < firstMsg {
		t.Errorf("messages were not fetched per batch: first message at %d, second audit batch at %d", firstMsg, secondAudit)
	}
	for _, q := range res.Queries {
		if q.Purpose == auditBatchPurpose && (q.Batches != 3 || q.Rows != convs) {
			t.Errorf("audit query = %+v", q)
		}
		if q.Purpose == messagesPurp && q.Rows != convs {
			t.Errorf("messages query = %+v", q)
		}
	}
}
