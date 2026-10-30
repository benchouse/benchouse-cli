// Package cortex reads Snowflake Cortex Analyst and Cortex Agents history.
//
// Snowflake records both inside the account: Analyst requests in
// SNOWFLAKE.LOCAL.CORTEX_ANALYST_REQUESTS_V, Agents runs as trace spans in
// SNOWFLAKE.LOCAL.AI_OBSERVABILITY_EVENTS, with usage and the statements the
// agents executed in SNOWFLAKE.ACCOUNT_USAGE. The statements here are the
// ones Benchouse's live Snowflake connector runs. The SNOWFLAKE.LOCAL tables
// have no documented retention, which is one more reason to extract them.
package cortex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/snowflake"
	"github.com/benchouse/benchouse-cli/internal/snowflake/sfauth"
)

const (
	analystKind = "snowflake_cortex_analyst"
	agentsKind  = "snowflake_cortex_agents"
)

func init() {
	agent.Register(agent.Agent{
		ID:      "snowflake-cortex",
		Name:    "Snowflake Cortex",
		Summary: "Cortex Analyst and Cortex Agents; reads the account's request history and traces",
		Status:  agent.Supported,
		Setup:   setup,
		Fields:  append(fields(), agent.Field{Key: "product", Label: "Which Cortex product?", Choices: []string{"both", "analyst", "agents"}, Default: "both"}),
		Context: &agent.Context{
			Label: "semantic models folder",
			Help:  "where your Cortex Analyst semantic model YAML or semantic view definitions live (a repo folder or a download from the stage)",
			Check: checkContext,
			Keep:  keepContext,
		},
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
	// Snowflake Intelligence answers through Cortex Agents and records its
	// runs in the same trace table, so it is the agents reader under the
	// name people know it by.
	agent.Register(agent.Agent{
		ID:      "snowflake-intelligence",
		Name:    "Snowflake Intelligence",
		Summary: "Snowflake Intelligence agents; reads their Cortex Agents traces",
		Status:  agent.Supported,
		Setup:   setup,
		Fields:  fields(),
		Context: &agent.Context{
			Label: "semantic views folder",
			Help:  "where the semantic views or semantic model YAML your agents use are defined",
			Check: checkContext,
			Keep:  keepContext,
		},
		Plan: func(in agent.Inputs, opts agent.Options) []bundle.Query { return plan(agentsOnly(in), opts) },
		Check: func(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
			return check(ctx, agentsOnly(in), opts)
		},
		Collect: func(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
			return collect(ctx, agentsOnly(in), opts, emit)
		},
	})
}

func agentsOnly(in agent.Inputs) agent.Inputs {
	out := agent.Inputs{}
	for k, v := range in {
		out[k] = v
	}
	out["product"] = "agents"
	return out
}

const setup = `Cortex history lives in your Snowflake account. The CLI talks to the
Snowflake SQL API over HTTPS with a programmatic access token (PAT) or a
key pair, and only runs SELECTs.

The role needs:
  - Analyst requests:  the SNOWFLAKE.CORTEX_ANALYST_REQUESTS_ADMIN application role
  - Agents traces:     the SNOWFLAKE.AI_OBSERVABILITY_EVENTS_LOOKUP application role
  - usage + statements (optional): IMPORTED PRIVILEGES on the SNOWFLAKE database
and a warehouse to run on.`

func fields() []agent.Field {
	return []agent.Field{
		{Key: "account", Label: "Snowflake account identifier", Validate: snowflake.ValidAccount,
			Help: "e.g. myorg-myaccount (the part before .snowflakecomputing.com)"},
		{Key: "user", Label: "Snowflake user"},
		{Key: "auth", Label: "Authenticate with", Choices: []string{"pat", "keypair"}, Default: "pat",
			Help: "pat: a programmatic access token; keypair: an unencrypted PEM private key file"},
		{Key: "token", Label: "Programmatic access token", Secret: true, When: isPAT,
			Validate: agent.Token, Verify: snowflake.VerifySignIn(connect)},
		{Key: "private-key", Label: "Path to the private key (.p8)", When: isKeyPair,
			Validate: agent.ExistingFile, Verify: snowflake.VerifySignIn(connect)},
		{Key: "role", Label: "Role", Optional: true, Lookup: snowflake.RoleLookup(connect),
			Help: "leave empty for the user's default role"},
		{Key: "warehouse", Label: "Warehouse", Optional: true, Lookup: snowflake.WarehouseLookup(connect),
			Verify: snowflake.VerifyWarehouse(connect), Help: "leave empty for the user's default warehouse"},
	}
}

// newClient is replaced in tests to point at a fake SQL API.
var newClient = snowflake.New

func isPAT(in agent.Inputs) bool     { return in.Get("auth") != "keypair" }
func isKeyPair(in agent.Inputs) bool { return in.Get("auth") == "keypair" }

func keepContext(rel string) bool {
	l := strings.ToLower(rel)
	return strings.HasSuffix(l, ".yaml") || strings.HasSuffix(l, ".yml") ||
		strings.HasSuffix(l, ".sql") || strings.HasSuffix(l, ".md")
}

// arm is one view read.
type arm struct {
	source, purpose, kind, from, timeCol, cast, extra string
	keys                                              func(map[string]any) map[string]string
	required                                          bool
	users                                             bool
	scope                                             scope
	// in is the column a byIDs arm matches against the batch of ids.
	in string
}

// scope says how an arm's rows are chosen under Options.MaxConversations.
type scope int

const (
	// recent: the most recent rows in the window, LIMIT the cap (Analyst
	// requests, each one a conversation), or perConversation × the cap for
	// rows not tied to a conversation.
	recent scope = iota
	// traces: spans of the most recent traces, chosen first by traceIDsStatement,
	// fetched agent.BatchSize trace ids at a time.
	traces
	// byIDs: rows whose `in` column names a request the chosen spans carry,
	// fetched agent.BatchSize ids at a time.
	byIDs
	// all: the whole view (the user directory).
	all
)

// perConversation bounds rows that cannot be tied to a chosen conversation
// (usage, statements): the most recent perConversation × MaxConversations.
const perConversation = 5

var (
	analystRequests = arm{source: analystKind, purpose: "Cortex Analyst requests (the most recent in the window)", kind: "analyst_request",
		from: "SNOWFLAKE.LOCAL.CORTEX_ANALYST_REQUESTS_V", timeCol: "TIMESTAMP", cast: "timestamp_ntz", required: true,
		keys: func(m map[string]any) map[string]string { return map[string]string{"request_id": str(m["REQUEST_ID"])} }}
	agentSpans = arm{source: agentsKind, purpose: "Cortex Agents traces (every span of the chosen traces)", kind: "span",
		from: "SNOWFLAKE.LOCAL.AI_OBSERVABILITY_EVENTS", timeCol: "TIMESTAMP", cast: "timestamp_ntz", required: true,
		keys: spanKeys, scope: traces, in: `TRACE:"trace_id"::string`}
)

var arms = []arm{
	analystRequests,
	{source: analystKind, purpose: "Cortex Analyst usage (cost, the most recent in the window)", kind: "analyst_billing",
		from: "SNOWFLAKE.ACCOUNT_USAGE.CORTEX_ANALYST_USAGE_HISTORY", timeCol: "START_TIME", cast: "timestamp_ltz",
		keys: func(m map[string]any) map[string]string {
			return map[string]string{"username": str(m["USERNAME"]), "start_time": str(m["START_TIME"])}
		}},
	agentSpans,
	{source: agentsKind, purpose: "Cortex Agents usage (cost) of the chosen requests", kind: "billing_usage",
		from: "SNOWFLAKE.ACCOUNT_USAGE.CORTEX_AGENT_USAGE_HISTORY", timeCol: "START_TIME", cast: "timestamp_ltz",
		keys:  func(m map[string]any) map[string]string { return map[string]string{"request_id": str(m["REQUEST_ID"])} },
		scope: byIDs, in: "REQUEST_ID"},
	{source: agentsKind, purpose: "statements the agents executed (the most recent in the window)", kind: "statement_history",
		from: "SNOWFLAKE.ACCOUNT_USAGE.QUERY_HISTORY", timeCol: "START_TIME", cast: "timestamp_ltz", extra: "AGENT_TYPE IS NOT NULL",
		keys: func(m map[string]any) map[string]string { return map[string]string{"query_id": str(m["QUERY_ID"])} }},
	{purpose: "user directory (names, emails)", kind: "directory_user",
		from: "SNOWFLAKE.ACCOUNT_USAGE.USERS", users: true, scope: all,
		keys: func(m map[string]any) map[string]string {
			return map[string]string{"user_id": str(m["USER_ID"]), "name": str(m["NAME"])}
		}},
}

func spanKeys(m map[string]any) map[string]string {
	ids := map[string]string{}
	if tr, ok := m["TRACE"].(map[string]any); ok {
		ids["trace_id"] = str(tr["trace_id"])
		ids["span_id"] = str(tr["span_id"])
	}
	if ra, ok := m["RECORD_ATTRIBUTES"].(map[string]any); ok {
		ids["input_id"] = str(ra["ai.observability.input_id"])
		ids["record_id"] = str(ra["ai.observability.record_id"])
		if rq := str(ra["snow.ai.observability.agent.planning.request_id"]); rq != "" {
			ids["request_id"] = rq
		}
		if rq := str(ra["snow.ai.observability.agent.request_id"]); rq != "" {
			ids["request_id"] = rq
		}
	}
	return ids
}

// where is the arm's filter, with its leading " WHERE ", or "".
func (a arm) where(opts agent.Options) string {
	var where []string
	if a.extra != "" {
		where = append(where, a.extra)
	}
	if a.timeCol != "" && !opts.Since.IsZero() {
		where = append(where, fmt.Sprintf("%s >= '%s'::%s", a.timeCol, opts.Since.UTC().Format("2006-01-02 15:04:05"), a.cast))
	}
	if len(where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(where, " AND ")
}

// and is where with one more condition.
func (a arm) and(opts agent.Options, cond string) string {
	if w := a.where(opts); w != "" {
		return w + " AND " + cond
	}
	return " WHERE " + cond
}

const selectJSON = "SELECT TO_JSON(OBJECT_CONSTRUCT_KEEP_NULL(*)) AS ROW_JSON FROM "

// limit is the arm's LIMIT under opts: the cap for Analyst requests,
// perConversation × the cap for rows not tied to one, 0 for none.
func (a arm) limit(opts agent.Options) int {
	if a.kind == analystRequests.kind {
		return opts.MaxConversations
	}
	return perConversation * opts.MaxConversations
}

// statement is the arm's SQL for one run. Without a cap every arm reads its
// whole window, oldest first, as before; with one, recent arms read the most
// recent rows and traces/byIDs arms read batches (see batchStatement).
func (a arm) statement(opts agent.Options) string {
	if a.scope == all {
		return selectJSON + a.from + a.where(opts) + " ORDER BY NAME"
	}
	if opts.MaxConversations == 0 || a.scope != recent {
		return selectJSON + a.from + a.where(opts) + " ORDER BY " + a.timeCol
	}
	return selectJSON + a.from + a.where(opts) + fmt.Sprintf(" ORDER BY %s DESC LIMIT %d", a.timeCol, a.limit(opts))
}

// batchStatement is a traces or byIDs arm's SQL for n ids, bound as ?.
// For Agents spans the ids are bound twice (see batchArgs).
func (a arm) batchStatement(opts agent.Options, n int) string {
	return selectJSON + a.from + a.and(opts, a.match(placeholders(n))) + " ORDER BY " + a.timeCol
}

// match is the arm's id predicate over list (placeholders or a planned note).
//
// Agents spans are matched by trace id OR by the record id of a span in
// those traces: Snowflake writes the CORTEX_AGENT_REQUEST span without a
// trace id, and it is the only span carrying the asker (user, role,
// session). Matching on trace id alone dropped every asker.
func (a arm) match(list string) string {
	if a.scope != traces {
		return a.in + " IN (" + list + ")"
	}
	return "(" + a.in + " IN (" + list + ") OR " + recordIDExpr + " IN (SELECT " + recordIDExpr + " FROM " + a.from +
		" WHERE " + a.in + " IN (" + list + ") AND " + recordIDExpr + " IS NOT NULL))"
}

const recordIDExpr = `RECORD_ATTRIBUTES:"ai.observability.record_id"::string`

// batchArgs are the bind values for batchStatement.
func (a arm) batchArgs(batch []string) []string {
	if a.scope != traces {
		return batch
	}
	return append(append([]string{}, batch...), batch...)
}

// plannedBatch is batchStatement as the plan and the manifest show it.
func (a arm) plannedBatch(opts agent.Options) string {
	noun := "trace ids"
	if a.scope == byIDs {
		noun = "request ids"
	}
	return selectJSON + a.from + a.and(opts, a.match(fmt.Sprintf("<up to %d %s>", agent.BatchSize, noun))) + " ORDER BY " + a.timeCol
}

// batched reports whether the arm is read in batches of ids under opts.
func (a arm) batched(opts agent.Options) bool {
	return opts.MaxConversations > 0 && (a.scope == traces || a.scope == byIDs)
}

// planned is the arm's statement as the plan shows it.
func (a arm) planned(opts agent.Options) string {
	if a.batched(opts) {
		return a.plannedBatch(opts)
	}
	return a.statement(opts)
}

// traceIDsStatement chooses the Cortex Agents conversations: the most
// recent traces (one per agent request) in the window. Every span carries
// its trace id, so a trace's spans are fetched whole.
func traceIDsStatement(opts agent.Options) string {
	return "SELECT TO_JSON(OBJECT_CONSTRUCT('TRACE_ID', TRACE:\"trace_id\"::string)) AS ROW_JSON FROM " + agentSpans.from +
		agentSpans.and(opts, `TRACE:"trace_id" IS NOT NULL`) +
		fmt.Sprintf(` GROUP BY TRACE:"trace_id"::string ORDER BY MAX(TIMESTAMP) DESC LIMIT %d`, opts.MaxConversations)
}

const traceIDsPurpose = "Cortex Agents conversations (the most recent traces in the window)"

// countStatement counts the conversations a required arm's window holds:
// Analyst requests, or distinct Agents traces.
func (a arm) countStatement(opts agent.Options) string {
	if a.scope == traces {
		return `SELECT COUNT(DISTINCT TRACE:"trace_id"::string) AS N FROM ` + a.from + a.where(opts)
	}
	return "SELECT COUNT(*) AS N FROM " + a.from + a.where(opts)
}

func placeholders(n int) string {
	if n < 1 {
		n = 1
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func selected(in agent.Inputs, opts agent.Options) []arm {
	p := in.Get("product")
	var out []arm
	for _, a := range arms {
		switch {
		case a.users:
			if opts.IncludeUsers {
				out = append(out, a)
			}
		case p == "analyst" && a.source != analystKind, p == "agents" && a.source != agentsKind:
		default:
			out = append(out, a)
		}
	}
	return out
}

func plan(in agent.Inputs, opts agent.Options) []bundle.Query {
	var out []bundle.Query
	for _, a := range selected(in, opts) {
		if opts.MaxConversations > 0 && a.required {
			out = append(out, bundle.Query{Purpose: "conversations in the window (count)", Statement: a.countStatement(opts)})
		}
		if a.batched(opts) && a.scope == traces {
			out = append(out, bundle.Query{Purpose: traceIDsPurpose, Statement: traceIDsStatement(opts)})
		}
		out = append(out, bundle.Query{Purpose: a.purpose, Statement: a.planned(opts)})
	}
	return out
}

// connect builds the SQL API client from the inputs, reading the private
// key file for key-pair auth. Collect and Check share it.
func connect(in agent.Inputs) (*snowflake.Client, error) {
	secret := in.Get("token")
	if isKeyPair(in) {
		b, err := os.ReadFile(in.Get("private-key"))
		if err != nil {
			return nil, fmt.Errorf("read private key: %w", err)
		}
		secret = string(b)
	}
	if err := sfauth.Validate(in.Get("auth"), in.Get("account"), in.Get("user"), secret); err != nil {
		return nil, err
	}
	return newClient(snowflake.Config{
		Account: in.Get("account"), User: in.Get("user"), Role: in.Get("role"),
		Warehouse: in.Get("warehouse"), Auth: in.Get("auth"),
	}, secret), nil
}

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	cl, err := connect(in)
	if err != nil {
		return agent.Result{}, err
	}

	res := agent.Result{Sampled: true}
	var failed []error
	products := map[string]bool{}
	var known, inWindow int64 = 0, 0
	counted := true
	// requestIDs are the agent requests the chosen spans belong to, for the
	// byIDs arms; bounded by the chosen traces.
	requestIDs := map[string]bool{}
	var traceSeen map[string]bool // without a cap: distinct traces read
	if opts.MaxConversations == 0 {
		traceSeen = map[string]bool{}
	}

	row := func(a arm, raw json.RawMessage) error {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber() // re-encoding a redacted span must not alter numbers
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return nil
		}
		if a.kind == "span" && !opts.IncludeResults {
			if r, ok := redactSpan(m); ok {
				raw = r
			}
		}
		source := a.source
		if source == "" { // the directory serves whichever product was read
			source = analystKind
			if in.Get("product") == "agents" {
				source = agentsKind
			}
		}
		rec := bundle.Record{SourceKind: source, RecordKind: a.kind, NativeIDs: a.keys(m),
			FetchMethod: "sql:" + a.from, Payload: raw}
		if a.timeCol != "" {
			rec.CursorValue = str(m[a.timeCol])
		}
		if a.kind == "span" {
			if rq := rec.NativeIDs["request_id"]; rq != "" {
				requestIDs[rq] = true
			}
			if traceSeen != nil && rec.NativeIDs["trace_id"] != "" {
				traceSeen[rec.NativeIDs["trace_id"]] = true
			}
		}
		if a.kind == analystRequests.kind {
			res.Conversations++
		}
		if err := emit(rec); err != nil {
			return fmt.Errorf("%w: %w", errEmit, err)
		}
		return nil
	}

	for _, a := range selected(in, opts) {
		if opts.MaxConversations > 0 && a.required {
			if n, err := cl.Count(ctx, a.countStatement(opts)); err == nil {
				inWindow += n
				known++
				res.Queries = append(res.Queries, bundle.Query{Purpose: "conversations in the window (count)", Statement: a.countStatement(opts), Rows: 1})
				if n > int64(opts.MaxConversations) {
					_, many := a.countNoun()
					res.Notes = append(res.Notes, fmt.Sprintf("%s %s in the window: the %s most recent were read",
						agent.Thousands(n), many, agent.Thousands(int64(opts.MaxConversations))))
				}
			} else {
				counted = false
			}
		}
		var err error
		switch {
		case a.batched(opts) && a.scope == traces:
			err = readTraces(ctx, cl, a, opts, &res, row)
		case a.batched(opts) && a.scope == byIDs:
			ids := make([]string, 0, len(requestIDs))
			for id := range requestIDs {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			err = readBatches(ctx, cl, a, opts, ids, &res, row)
		default:
			stmt := a.statement(opts)
			var n int
			n, err = cl.EachJSON(ctx, stmt, func(raw json.RawMessage) error { return row(a, raw) })
			if err == nil {
				res.Queries = append(res.Queries, bundle.Query{Purpose: a.purpose, Statement: stmt, Rows: n})
				if lim := a.limit(opts); a.scope == recent && lim > 0 && n == lim && a.kind != analystRequests.kind {
					res.Notes = append(res.Notes, fmt.Sprintf("%s: stopped at %s rows (%d per conversation in the cap), as these rows are not tied to a conversation",
						a.purpose, agent.Thousands(int64(lim)), perConversation))
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, errEmit) {
				return res, err
			}
			if a.required {
				failed = append(failed, fmt.Errorf("%s: %w", a.purpose, err))
			}
			res.Notes = append(res.Notes, fmt.Sprintf("skipped %s: %v", a.purpose, short(err)))
			continue
		}
		if a.required {
			products[a.source] = true
		}
	}
	if len(products) == 0 && len(failed) > 0 {
		return res, errors.Join(failed...)
	}
	if traceSeen != nil {
		res.Conversations += len(traceSeen)
	}
	if opts.MaxConversations > 0 && counted && known > 0 {
		res.InWindow = &inWindow
	}
	if !opts.IncludeResults {
		res.Redactions = append(res.Redactions, spanRedaction)
	}
	account := strings.ToUpper(strings.TrimSuffix(strings.ToLower(in.Get("account")), ".snowflakecomputing.com"))
	if products[analystKind] {
		// Analyst capture is account-wide: Snowflake publishes no id for a
		// semantic model, so the account is the agent.
		res.Sources = append(res.Sources, bundle.Source{SourceKind: analystKind, NativeID: "*", Account: account})
	}
	if products[agentsKind] {
		res.Sources = append(res.Sources, bundle.Source{SourceKind: agentsKind, NativeID: "*", Account: account})
	}
	return res, nil
}

// errEmit wraps an error from emit, which stops the collection rather than
// skipping a view.
var errEmit = errors.New("writing records")

// readTraces chooses the most recent traces, then reads their spans.
func readTraces(ctx context.Context, cl *snowflake.Client, a arm, opts agent.Options, res *agent.Result,
	row func(arm, json.RawMessage) error) error {
	var ids []string
	stmt := traceIDsStatement(opts)
	n, err := cl.EachJSON(ctx, stmt, func(raw json.RawMessage) error {
		var v struct {
			TraceID string `json:"TRACE_ID"`
		}
		if json.Unmarshal(raw, &v) == nil && v.TraceID != "" {
			ids = append(ids, v.TraceID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	res.Queries = append(res.Queries, bundle.Query{Purpose: traceIDsPurpose, Statement: stmt, Rows: n})
	res.Conversations += len(ids)
	return readBatches(ctx, cl, a, opts, ids, res, row)
}

// readBatches reads a traces or byIDs arm for ids, agent.BatchSize at a
// time, each id bound by the server.
func readBatches(ctx context.Context, cl *snowflake.Client, a arm, opts agent.Options, ids []string, res *agent.Result,
	row func(arm, json.RawMessage) error) error {
	if len(ids) == 0 {
		res.Queries = append(res.Queries, bundle.Query{Purpose: a.purpose, Statement: a.plannedBatch(opts)})
		return nil
	}
	for start := 0; start < len(ids); start += agent.BatchSize {
		batch := ids[start:min(start+agent.BatchSize, len(ids))]
		n, err := cl.EachJSONArgs(ctx, a.batchStatement(opts, len(batch)), a.batchArgs(batch), func(raw json.RawMessage) error { return row(a, raw) })
		if err != nil {
			return err
		}
		res.Tally(a.purpose, a.plannedBatch(opts), n)
	}
	return nil
}

func short(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}
