// Package genie reads Databricks AI/BI Genie space history.
//
// Databricks does not store the conversation where the other telemetry is.
// The system tables hold identifiers, timings and executed SQL: Genie's audit
// events in system.access.audit, its statements in system.query.history and
// its own SKU in system.billing.usage. The question and answer text live only
// behind the Genie Conversation API, reachable because the audit log keeps
// the conversation and message ids. So a collection is a reconstruction:
// scan the audit log, then fetch each message it names.
//
// The record kinds, native ids, fetch methods and the query-history and
// billing statements are those of Benchouse's live Genie connector
// (genie/0.1.0) when no --max-conversations cap is set. That connector reads
// system.query.history without a time bound, and so does this reader then;
// the --since window is applied to those rows after they arrive.
//
// Audit events are read per conversation, cap or not: the reader first
// chooses the conversations from the audit log (the most recent ones under
// a cap, the default), then, agent.BatchSize at a time, reads their audit
// events, fetches the messages those name and, under a cap, the
// query-history rows of the statements those messages ran, each id list
// bound as statement parameters. Only one batch's message refs are held, and
// the chosen conversation ids spill to a temp file when there are many, so
// a run without a cap is bounded too. Under a cap the billing rows (Genie's
// SKU is not attributed to a conversation) are the most recent in the
// window.
package genie

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
)

const sourceKind = "databricks_genie"

func init() {
	agent.Register(agent.Agent{
		ID:      "databricks-genie",
		Name:    "Databricks Genie",
		Summary: "AI/BI Genie spaces; reads the audit log and query history, and each message from the Genie API",
		Status:  agent.Supported,
		Setup: `Genie's history is split: the system tables say what happened and which SQL
ran, the Genie Conversation API holds the questions and answers. The CLI
uses a personal access token over HTTPS, runs only SELECTs through the SQL
Statement Execution API (POST /api/2.0/sql/statements, polled with GET until
done) and only GETs elsewhere.

The token's identity needs:
  - a SQL warehouse:   CAN USE, to run the system-table SELECTs
  - audit events:      USE CATALOG on system, USE SCHEMA on system.access,
                       SELECT on system.access.audit
  - statements, cost (optional): USE SCHEMA + SELECT on system.query.history
                       and system.billing.usage
  - the Genie space:   CAN VIEW or CAN RUN to read its messages. Messages from
                       other people's conversations may need CAN MANAGE; any
                       the API refuses are listed as notes, not failures
  - user directory (--include-users only): workspace admin, for SCIM
System tables need Unity Catalog; an account and metastore admin has these
grants by default.`,
		Fields: []agent.Field{
			{Key: "host", Label: "Databricks workspace URL", Validate: agent.URL(false),
				Help: "e.g. https://adb-1234567890123456.7.azuredatabricks.net or https://dbc-a1b2c3d4-e5f6.cloud.databricks.com"},
			{Key: "token", Label: "Personal access token", Secret: true, Validate: agent.Token, Verify: verifyToken},
			{Key: "warehouse-id", Label: "SQL warehouse", Lookup: Warehouses, Validate: validWarehouse, Verify: verifyWarehouse,
				Help: "the id in the warehouse's connection details, or its HTTP path (/sql/1.0/warehouses/<id>)"},
			{Key: "space-id", Label: "Genie space", Optional: true, Lookup: Spaces, Validate: validSpace, Verify: verifySpace,
				Help: "the space id from its URL (/genie/rooms/<id>); leave empty to read every space"},
		},
		Context: nil, // a space's instructions are not files; a dbt folder still works
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
}

// retryAfter is the wait before the one retry of a rate-limited message
// fetch; tests shorten it.
var retryAfter = 3 * time.Second

// arm is one system-table read.
type arm struct {
	purpose, kind, table, cursor string
	where                        func(space, sinceDate string) string
	order                        string
	keys                         func(map[string]any) map[string]string
	required                     bool
}

var (
	auditArm = arm{purpose: "Genie audit events (which questions were asked, and their ids)", kind: "audit_event",
		table: "system.access.audit", cursor: "event_time", order: "event_time", required: true,
		where: func(space, sinceDate string) string {
			w := "service_name = 'aibiGenie'"
			// request_params.space_id is null on genieCreateSpace and
			// genieListSpaces, so those lifecycle rows drop out: they carry no
			// conversation.
			if space != "" {
				w += fmt.Sprintf(" AND request_params.space_id = '%s'", space)
			}
			// event_date is the partition column; filtering on event_time
			// scans the whole table.
			if sinceDate != "" {
				w += fmt.Sprintf(" AND event_date >= DATE '%s'", sinceDate)
			}
			return w
		},
		keys: auditKeys}
	historyArm = arm{purpose: "statements Genie executed", kind: "statement_history",
		table: "system.query.history", cursor: "start_time", order: "start_time",
		where: func(space, _ string) string {
			if space != "" {
				return fmt.Sprintf("query_source.genie_space_id = '%s'", space)
			}
			return "query_source.genie_space_id IS NOT NULL"
		},
		keys: func(m map[string]any) map[string]string {
			ids := map[string]string{"statement_id": str(m["statement_id"])}
			if qs, ok := m["query_source"].(map[string]any); ok {
				ids["genie_space_id"] = str(qs["genie_space_id"])
			}
			return ids
		}}
	// Genie's SKU has no space attribution: usage_metadata has no
	// genie_space_id, so this is account-wide.
	billingArm = arm{purpose: "Genie usage (cost, account-wide)", kind: "billing_usage",
		table: "system.billing.usage", cursor: "usage_start_time", order: "usage_start_time",
		where: func(_, sinceDate string) string {
			w := "billing_origin_product = 'GENIE'"
			if sinceDate != "" {
				w += fmt.Sprintf(" AND usage_date >= DATE '%s'", sinceDate)
			}
			return w
		},
		keys: func(m map[string]any) map[string]string { return map[string]string{"record_id": str(m["record_id"])} }}
)

// statement is the arm's SQL, laid out exactly as the live connector sends it.
func (a arm) statement(space, sinceDate string) string {
	return `SELECT to_json(struct(*)) AS row_json FROM ` + a.table + `
             WHERE ` + a.where(space, sinceDate) + ` ORDER BY ` + a.order
}

func auditKeys(m map[string]any) map[string]string {
	ids := map[string]string{"event_id": str(m["event_id"])}
	if rp, ok := m["request_params"].(map[string]any); ok {
		ids["space_id"] = str(rp["space_id"])
		ids["conversation_id"] = str(rp["conversation_id"])
		ids["message_id"] = str(rp["message_id"])
	}
	// response.result is a JSON string inside the struct; the ids of a newly
	// started conversation live there.
	if rs, ok := m["response"].(map[string]any); ok {
		if res := str(rs["result"]); res != "" {
			var inner map[string]any
			if json.Unmarshal([]byte(res), &inner) == nil {
				if v := str(inner["message_id"]); v != "" {
					ids["message_id"] = v
				}
				if v := str(inner["conversation_id"]); v != "" {
					ids["conversation_id"] = v
				}
			}
		}
	}
	ids["action_name"] = str(m["action_name"])
	return ids
}

const (
	messageMethod = "rest:GET /api/2.0/genie/spaces/{space}/conversations/{conv}/messages/{msg}"
	scimMethod    = "rest:/api/2.0/preview/scim/v2/Users"
	messagesPurp  = "conversation content (question, SQL, answer): one call per message the audit events name"
	usersPurp     = "user directory (names, emails)"
)

func sinceDate(opts agent.Options) string {
	if opts.Since.IsZero() {
		return ""
	}
	return opts.Since.UTC().Format("2006-01-02")
}

func messageStatement(space string) string {
	if space == "" {
		space = "{space_id}"
	}
	return "GET " + fmt.Sprintf(messagePath, space, "{conversation_id}", "{message_id}")
}

func scimStatement() string {
	return "GET " + fmt.Sprintf(scimPath, 1, scimPageSize) +
		fmt.Sprintf(", then startIndex=%d, %d, ... until a short page", 1+scimPageSize, 1+2*scimPageSize)
}

func plan(in agent.Inputs, opts agent.Options) []bundle.Query {
	space, since := in.Get("space-id"), sinceDate(opts)
	var out []bundle.Query
	if opts.MaxConversations > 0 {
		out = append(out,
			bundle.Query{Purpose: countPurpose, Statement: countStatement(space, since)},
			bundle.Query{Purpose: convPurpose, Statement: conversationsStatement(space, since, opts.MaxConversations)},
			bundle.Query{Purpose: auditBatchPurpose, Statement: auditBatchPlanned(space, since)},
			bundle.Query{Purpose: messagesPurp, Statement: messageStatement(space)},
			bundle.Query{Purpose: historyBatchPurpose, Statement: historyBatchPlanned(space)},
			bundle.Query{Purpose: billingArm.purpose, Statement: recentStatement(billingArm, space, since, perConversation*opts.MaxConversations)},
		)
	} else {
		out = append(out,
			bundle.Query{Purpose: convPurpose, Statement: conversationsStatement(space, since, 0)},
			bundle.Query{Purpose: auditBatchPurpose, Statement: auditBatchPlanned(space, since)},
			bundle.Query{Purpose: messagesPurp, Statement: messageStatement(space)},
			bundle.Query{Purpose: historyArm.purpose, Statement: historyArm.statement(space, since)},
			bundle.Query{Purpose: billingArm.purpose, Statement: billingArm.statement(space, since)},
		)
	}
	if opts.IncludeUsers {
		out = append(out, bundle.Query{Purpose: usersPurp, Statement: scimStatement()})
	}
	return out
}

// Under a cap: the conversations are chosen first, then read in batches.

// convExpr is an audit event's conversation id: in request_params for a
// follow-up, in the response for a conversation's first message.
const convExpr = `coalesce(request_params.conversation_id, get_json_object(response.result, '$.conversation_id'))`

// questionActions are the audit actions that ask a question.
const questionActions = `action_name IN ('genieStartConversationMessage', 'genieCreateConversationMessage')`

// perConversation bounds the billing rows, which no conversation owns: the
// most recent perConversation × MaxConversations in the window.
const perConversation = 5

const (
	countPurpose        = "Genie conversations and questions in the window (count)"
	convPurpose         = "Genie conversations (the most recent in the window)"
	auditBatchPurpose   = "Genie audit events of the chosen conversations"
	historyBatchPurpose = "statements Genie executed for the chosen messages"
)

// conversationsStatement chooses the n conversations most recently asked
// in, in the window; every one when n is 0.
func conversationsStatement(space, since string, n int) string {
	limit := ""
	if n > 0 {
		limit = ` LIMIT ` + fmt.Sprint(n)
	}
	return `SELECT to_json(struct(conversation_id, max(event_time) AS last_event_time)) AS row_json FROM (
  SELECT ` + convExpr + ` AS conversation_id, event_time FROM ` + auditArm.table + `
             WHERE ` + auditArm.where(space, since) + ` AND ` + questionActions + `
) WHERE conversation_id IS NOT NULL GROUP BY conversation_id ORDER BY last_event_time DESC` + limit
}

// auditBatchStatement reads every audit event of the conversations bound
// as in.
func auditBatchStatement(space, since, in string) string {
	return `SELECT to_json(struct(*)) AS row_json FROM ` + auditArm.table + `
             WHERE ` + auditArm.where(space, since) + ` AND ` + convExpr + ` IN (` + in + `) ORDER BY event_time`
}

func auditBatchPlanned(space, since string) string {
	return auditBatchStatement(space, since, fmt.Sprintf("<up to %d conversation ids>", agent.BatchSize))
}

// historyBatchStatement reads the query-history rows of the statements
// bound as in. Bounded by the ids, not the window: a statement id names one
// row.
func historyBatchStatement(space, in string) string {
	return `SELECT to_json(struct(*)) AS row_json FROM ` + historyArm.table + `
             WHERE ` + historyArm.where(space, "") + ` AND statement_id IN (` + in + `) ORDER BY start_time`
}

func historyBatchPlanned(space string) string {
	return historyBatchStatement(space, fmt.Sprintf("<up to %d statement ids>", agent.BatchSize))
}

// recentStatement is the arm's statement for its most recent n rows.
func recentStatement(a arm, space, since string, n int) string {
	return `SELECT to_json(struct(*)) AS row_json FROM ` + a.table + `
             WHERE ` + a.where(space, since) + ` ORDER BY ` + a.order + ` DESC LIMIT ` + fmt.Sprint(n)
}

// connect builds the workspace client and checks the space id, which is
// written into system-table SQL and request paths. Collect and Check share
// it.
func connect(in agent.Inputs) (*client, string, error) {
	cl, err := newClient(in.Get("host"), in.Get("token"), in.Get("warehouse-id"))
	if err != nil {
		return nil, "", err
	}
	if cl.host == "" || cl.token == "" || cl.warehouseID == "" {
		return nil, "", errors.New("Databricks Genie needs --host, --token and --warehouse-id")
	}
	space := in.Get("space-id")
	if space != "" && !validID(space) {
		return nil, "", fmt.Errorf("--space-id %q is not a Genie space id (the id from the space's URL, /genie/rooms/<id>)", space)
	}
	return cl, space, nil
}

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	cl, space, err := connect(in)
	if err != nil {
		return agent.Result{}, err
	}
	since := sinceDate(opts)
	r := &reader{cl: cl, space: space, since: since, opts: opts, emit: emit,
		sources: map[string]bool{}, res: agent.Result{Sampled: true}}
	res := &r.res

	capped := opts.MaxConversations > 0
	if capped {
		if n, err := countConversations(ctx, cl, space, since); err == nil {
			res.InWindow = &n
			res.Queries = append(res.Queries, bundle.Query{Purpose: countPurpose, Statement: countStatement(space, since), Rows: 1})
		}
	}
	if err := r.readChosen(ctx); err != nil {
		return *res, err
	}
	if res.InWindow != nil && *res.InWindow > int64(res.Conversations) {
		res.Notes = append(res.Notes, fmt.Sprintf("%s Genie conversations in the window: the %s most recent were read",
			agent.Thousands(*res.InWindow), agent.Thousands(int64(res.Conversations))))
	}
	if r.invalid > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("skipped %d Genie messages whose audit event names an id that is not one "+
			"(letters, digits, - and _); their audit events are kept", r.invalid))
	}
	if r.failed > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("could not fetch %d of %d Genie messages (their audit events are kept); first: %s",
			r.failed, r.named, r.firstFailure))
	}
	if r.historyErr != nil {
		res.Notes = append(res.Notes, fmt.Sprintf("skipped %s: %v", historyArm.purpose, short(r.historyErr)))
	}

	if capped {
		lim := perConversation * opts.MaxConversations
		n, err := r.readArm(ctx, billingArm, recentStatement(billingArm, space, since, lim), nil)
		switch {
		case errors.Is(err, errEmit) || ctx.Err() != nil:
			return *res, err
		case err != nil:
			res.Notes = append(res.Notes, fmt.Sprintf("skipped %s: %v", billingArm.purpose, short(err)))
		case n == lim:
			res.Notes = append(res.Notes, fmt.Sprintf("Genie usage is account-wide, not tied to a conversation: the %s most recent rows in the window were read",
				agent.Thousands(int64(lim))))
		}
	} else {
		for _, a := range []arm{historyArm, billingArm} {
			var keep func(map[string]any) bool
			if a.table == historyArm.table {
				keep = sinceFilter(opts.Since)
			}
			if _, err := r.readArm(ctx, a, a.statement(space, since), keep); err != nil {
				if errors.Is(err, errEmit) || ctx.Err() != nil {
					return *res, err
				}
				res.Notes = append(res.Notes, fmt.Sprintf("skipped %s: %v", a.purpose, short(err)))
			}
		}
	}
	if opts.IncludeUsers {
		if err := r.readUsers(ctx); err != nil {
			if errors.Is(err, errEmit) || ctx.Err() != nil {
				return *res, err
			}
			res.Notes = append(res.Notes, fmt.Sprintf("skipped %s: %v", usersPurp, short(err)))
		}
	}
	if !opts.IncludeResults {
		res.Redactions = append(res.Redactions, resultsRedaction)
	}
	// One source per space: cost and capture are matched to agents by space.
	for _, sp := range r.sourceOrder {
		res.Sources = append(res.Sources, bundle.Source{SourceKind: sourceKind, NativeID: sp, Account: cl.host})
	}
	return *res, nil
}

// errEmit marks an error from emit: it stops the collection.
var errEmit = errors.New("writing records")

// reader is one collection in progress. It keeps ids, never records: the
// chosen conversation ids (spilling to a temp file past agent.IDSpool's
// limit), and, for one batch of conversations at a time, the message refs
// their audit events name and the statement ids those messages ran.
type reader struct {
	cl          *client
	space       string
	since       string
	opts        agent.Options
	emit        agent.Emit
	res         agent.Result
	refs        *refs    // the current batch's messages
	statements  []string // the current batch's statement ids (under a cap)
	stmtSeen    map[string]bool
	sources     map[string]bool
	sourceOrder []string

	invalid, named, failed int
	firstFailure           string
	historyErr             error
}

func (r *reader) put(rec bundle.Record) error {
	if sp := rec.NativeIDs["space_id"]; sp != "" && !r.sources[sp] && (r.space == "" || sp == r.space) {
		r.sources[sp] = true
		r.sourceOrder = append(r.sourceOrder, sp)
	}
	if err := r.emit(rec); err != nil {
		return fmt.Errorf("%w: %w", errEmit, err)
	}
	return nil
}

// readArm runs one unbatched system-table statement, emits its rows and
// records the query. keep, if given, drops rows after they arrive.
func (r *reader) readArm(ctx context.Context, a arm, stmt string, keep func(map[string]any) bool) (int, error) {
	n, err := r.readRows(ctx, a, stmt, nil, keep)
	if err != nil {
		return n, err
	}
	r.res.Queries = append(r.res.Queries, bundle.Query{Purpose: a.purpose, Statement: stmt, Rows: n})
	return n, nil
}

// readRows runs one system-table statement and emits its rows verbatim,
// noting the messages audit events name. params bind the statement's :ids.
func (r *reader) readRows(ctx context.Context, a arm, stmt string, params []param, keep func(map[string]any) bool) (int, error) {
	return r.cl.queryJSONEach(ctx, stmt, params, func(row string) error {
		m, err := decode([]byte(row))
		if err != nil || (keep != nil && !keep(m)) {
			return nil
		}
		rec := bundle.Record{SourceKind: sourceKind, RecordKind: a.kind, NativeIDs: a.keys(m),
			FetchMethod: "sql:" + a.table, CursorValue: str(m[a.cursor]), Payload: json.RawMessage(row)}
		if a.kind == auditArm.kind && r.refs != nil {
			r.refs.add(rec)
		}
		return r.put(rec)
	})
}

// batchRun adds one run of the batched query at r.res.Queries[i].
func (r *reader) batchRun(i, rows int) {
	r.res.Queries[i].Batches++
	r.res.Queries[i].Rows += rows
}

// readChosen chooses the conversations (the most recent, under a cap),
// then, agent.BatchSize at a time, reads their audit events, fetches the
// messages those name and, under a cap, the query-history rows of the
// statements those messages ran. Only one batch's refs are held.
func (r *reader) readChosen(ctx context.Context) error {
	capped := r.opts.MaxConversations > 0
	stmt := conversationsStatement(r.space, r.since, r.opts.MaxConversations)
	var ids agent.IDSpool
	defer ids.Close()
	n, err := r.cl.queryJSONEach(ctx, stmt, nil, func(row string) error {
		var v struct {
			ConversationID string `json:"conversation_id"`
		}
		if json.Unmarshal([]byte(row), &v) == nil && v.ConversationID != "" {
			return ids.Add(v.ConversationID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%s: %w", convPurpose, err)
	}
	r.res.Queries = append(r.res.Queries, bundle.Query{Purpose: convPurpose, Statement: stmt, Rows: n})
	r.res.Conversations = ids.Len()

	// The batched queries, in plan order, filled in as batches run.
	auditAt := len(r.res.Queries)
	r.res.Queries = append(r.res.Queries, bundle.Query{Purpose: auditBatchPurpose, Statement: auditBatchPlanned(r.space, r.since)})
	messagesAt := len(r.res.Queries)
	r.res.Queries = append(r.res.Queries, bundle.Query{Purpose: messagesPurp, Statement: messageStatement(r.space)})
	historyAt := -1
	if capped {
		historyAt = len(r.res.Queries)
		r.res.Queries = append(r.res.Queries, bundle.Query{Purpose: historyBatchPurpose, Statement: historyBatchPlanned(r.space)})
	}

	return ids.Batches(agent.BatchSize, func(batch []string) error {
		r.refs = newRefs(r.space)
		ps, list := idParams(batch)
		n, err := r.readRows(ctx, auditArm, auditBatchStatement(r.space, r.since, list), ps, nil)
		if err != nil {
			if errors.Is(err, errEmit) || ctx.Err() != nil {
				return err
			}
			return fmt.Errorf("%s: %w", auditBatchPurpose, err)
		}
		r.batchRun(auditAt, n)
		r.invalid += r.refs.invalid
		fetched, err := r.fetchMessages(ctx)
		r.res.Queries[messagesAt].Rows += fetched
		if err != nil {
			return err
		}
		if capped && r.historyErr == nil {
			if err := r.readHistoryOfMessages(ctx, historyAt); err != nil {
				if errors.Is(err, errEmit) || ctx.Err() != nil {
					return err
				}
				r.historyErr = err
			}
		}
		r.refs, r.statements, r.stmtSeen = nil, nil, nil
		return nil
	})
}

// readHistoryOfMessages reads the query-history rows of the statements the
// batch's messages ran, agent.BatchSize ids at a time.
func (r *reader) readHistoryOfMessages(ctx context.Context, at int) error {
	for start := 0; start < len(r.statements); start += agent.BatchSize {
		ps, list := idParams(r.statements[start:min(start+agent.BatchSize, len(r.statements))])
		n, err := r.readRows(ctx, historyArm, historyBatchStatement(r.space, list), ps, nil)
		if err != nil {
			return err
		}
		r.batchRun(at, n)
	}
	return nil
}

// countConversations counts the conversations asked in, in the window.
func countConversations(ctx context.Context, cl *client, space, since string) (int64, error) {
	rows, err := cl.queryJSONRows(ctx, countStatement(space, since))
	if err != nil {
		return 0, err
	}
	var v struct {
		Conversations *int64 `json:"conversations"`
	}
	if len(rows) == 0 || json.Unmarshal([]byte(rows[0]), &v) != nil || v.Conversations == nil {
		return 0, errors.New("the count returned no conversations")
	}
	return *v.Conversations, nil
}

// sinceFilter keeps query-history rows that started on or after the day the
// window opens, the same day granularity the audit and billing arms use. A
// row whose start_time cannot be read is kept.
func sinceFilter(since time.Time) func(map[string]any) bool {
	return func(m map[string]any) bool {
		if since.IsZero() {
			return true
		}
		t, ok := parseTime(str(m["start_time"]))
		if !ok {
			return true
		}
		day := since.UTC().Truncate(24 * time.Hour)
		return !t.UTC().Before(day)
	}
}

func parseTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z0700", "2006-01-02 15:04:05.999999999", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// messageRef addresses one Genie message for the Conversation API.
type messageRef struct{ space, conv, msg, eventTime string }

// refs lists the messages the audit events started or posted, each once.
// Poll calls (genieGetConversationMessage) name the same messages and are
// not followed.
type refs struct {
	defaultSpace string
	seen         map[string]bool
	out          []messageRef
	invalid      int
}

func newRefs(space string) *refs {
	return &refs{defaultSpace: space, seen: map[string]bool{}}
}

func (p *refs) add(r bundle.Record) {
	action := r.NativeIDs["action_name"]
	if action != "genieStartConversationMessage" && action != "genieCreateConversationMessage" {
		return
	}
	sp := r.NativeIDs["space_id"]
	if sp == "" {
		sp = p.defaultSpace
	}
	conv, msg := r.NativeIDs["conversation_id"], r.NativeIDs["message_id"]
	if conv == "" || msg == "" || p.seen[msg] {
		return
	}
	p.seen[msg] = true
	if !validID(sp) || !validID(conv) || !validID(msg) {
		p.invalid++
		return
	}
	p.out = append(p.out, messageRef{space: sp, conv: conv, msg: msg, eventTime: r.CursorValue})
}

// pendingMessages lists the messages the audit events started or posted.
func pendingMessages(audits []bundle.Record, defaultSpace string) ([]messageRef, int) {
	p := newRefs(defaultSpace)
	for _, r := range audits {
		p.add(r)
	}
	return p.out, p.invalid
}

// fetchMessages pulls the content of the current batch's messages,
// sequentially: the API rate limits, so a 429 gets one retry. A message that
// cannot be fetched is counted for a note; the audit event that names it is
// still in the bundle. It returns how many it fetched.
func (r *reader) fetchMessages(ctx context.Context) (int, error) {
	var fetched int
	for _, ref := range r.refs.out {
		r.named++
		if ctx.Err() != nil {
			r.fail(ref.msg, ctx.Err())
			continue
		}
		body, status, err := r.cl.message(ctx, ref.space, ref.conv, ref.msg)
		if err != nil && status == 429 {
			select {
			case <-ctx.Done():
			case <-time.After(retryAfter):
			}
			body, _, err = r.cl.message(ctx, ref.space, ref.conv, ref.msg)
		}
		if err != nil {
			r.fail(ref.msg, errors.New(short(err)))
			continue
		}
		fetched++
		if r.opts.MaxConversations > 0 {
			r.noteStatements(body)
		}
		if !r.opts.IncludeResults {
			if red, ok := redactMessage(body); ok {
				body = red
			}
		}
		if err := r.put(bundle.Record{SourceKind: sourceKind, RecordKind: "conversation_message",
			NativeIDs:   map[string]string{"space_id": ref.space, "conversation_id": ref.conv, "message_id": ref.msg},
			FetchMethod: messageMethod, CursorValue: ref.eventTime, Payload: body}); err != nil {
			return fetched, err
		}
	}
	return fetched, nil
}

func (r *reader) fail(msg string, err error) {
	if r.failed == 0 {
		r.firstFailure = fmt.Sprintf("%s: %v", msg, err)
	}
	r.failed++
}

// noteStatements keeps the statement ids a message's queries ran, for the
// query-history read.
func (r *reader) noteStatements(body []byte) {
	var m struct {
		Attachments []struct {
			Query struct {
				StatementID string `json:"statement_id"`
			} `json:"query"`
		} `json:"attachments"`
		QueryResult struct {
			StatementID string `json:"statement_id"`
		} `json:"query_result"`
	}
	if json.Unmarshal(body, &m) != nil {
		return
	}
	ids := []string{m.QueryResult.StatementID}
	for _, a := range m.Attachments {
		ids = append(ids, a.Query.StatementID)
	}
	if r.stmtSeen == nil {
		r.stmtSeen = map[string]bool{}
	}
	for _, id := range ids {
		if id != "" && !r.stmtSeen[id] {
			r.stmtSeen[id] = true
			r.statements = append(r.statements, id)
		}
	}
}

func (r *reader) readUsers(ctx context.Context) error {
	users, err := r.cl.users(ctx)
	if err != nil {
		return err
	}
	r.res.Queries = append(r.res.Queries, bundle.Query{Purpose: usersPurp, Statement: scimStatement(), Rows: len(users)})
	for _, u := range users {
		m, err := decode(u)
		if err != nil {
			continue
		}
		if err := r.put(bundle.Record{SourceKind: sourceKind, RecordKind: "directory_user",
			NativeIDs: map[string]string{"id": str(m["id"])}, FetchMethod: scimMethod, Payload: u}); err != nil {
			return err
		}
	}
	return nil
}

// Spaces lists the Genie spaces the token can see, for the interactive
// picker.
func Spaces(ctx context.Context, in agent.Inputs) ([][2]string, error) {
	cl, err := newClient(in.Get("host"), in.Get("token"), in.Get("warehouse-id"))
	if err != nil {
		return nil, err
	}
	ss, err := cl.spaces(ctx)
	if err != nil {
		return nil, err
	}
	out := make([][2]string, 0, len(ss))
	for _, s := range ss {
		label := s.Title
		if label == "" {
			label = s.SpaceID
		}
		out = append(out, [2]string{s.SpaceID, label})
	}
	return out, nil
}

// decode reads a row keeping numbers exact, so re-encoding a redacted
// message cannot alter an id above 2^53.
func decode(row []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(row))
	dec.UseNumber()
	var m map[string]any
	err := dec.Decode(&m)
	return m, err
}

func short(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// str renders a JSON value as a string; objects and arrays as JSON, as the
// live connector does.
func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return fmt.Sprintf("%.0f", t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
