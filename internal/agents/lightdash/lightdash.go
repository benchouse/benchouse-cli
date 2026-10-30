// Package lightdash reads Lightdash AI agent conversations
// (github.com/lightdash/lightdash).
//
// Lightdash keeps its AI agent threads in its own Postgres database, next to
// everything else the app stores. A thread (ai_thread) holds prompts
// (ai_prompt): one row per question, with the answer, error, model, token
// usage and the user's thumbs up or down on the same row. The agent's steps
// are rows keyed by the prompt: tool calls (ai_agent_tool_call), their
// results (ai_agent_tool_result) and reasoning (ai_agent_reasoning).
//
// Lightdash's agent mostly queries through the semantic layer: its runQuery
// family of tools takes a metric query (explore, dimensions, metrics,
// filters), not SQL. The SQL those produce is not on the tool call; it is in
// query_history.compiled_sql, linked through the tool result's
// metadata.queryUuid. runSql carries its SQL in tool_args.sql. So each turn's
// statements are the runSql SQL plus the compiled SQL of every metric query
// that reached query_history; the metric queries themselves are the turn's
// tool_calls.
//
// Written from the source at commit 9c572b8f111cbf6a1d7332318c675c002ee06f8d
// (Lightdash 3.106.0, 2026-10-29): packages/backend/src/ee/database/entities/ai.ts,
// aiAgent.ts, the ee migrations creating ai_agent_tool_call (2025-06-24),
// ai_agent_tool_result (2025-06-25, metadata added 2025-10-02) and
// ai_agent_reasoning (2025-10-23), and
// packages/backend/src/database/entities/queryHistory.ts. Tested against
// fixtures only. Every statement selects whole rows (p.*, tc.*) where it can,
// so a column added or missing in another version does not break the read.
package lightdash

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/pgread"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

func init() {
	agent.Register(agent.Agent{
		ID:      "lightdash",
		Name:    "Lightdash AI",
		Summary: "Lightdash AI agents; reads the threads in Lightdash's own Postgres database",
		Status:  agent.Experimental,
		Setup: `Lightdash stores AI agent threads in its own Postgres database (the one
PGHOST/PGDATABASE point Lightdash at). Read-only access is enough; a
dedicated user is best:

  CREATE USER benchouse_reader PASSWORD '...';
  GRANT SELECT ON ai_thread, ai_prompt, ai_agent, ai_agent_tool_call,
                  ai_agent_tool_result, ai_agent_reasoning, query_history
                  TO benchouse_reader;
  -- only with --include-users:
  GRANT SELECT ON users, emails TO benchouse_reader;

query_history is read only for the rows the agent's tool results point at,
for the SQL Lightdash compiled from the agent's metric queries. The semantic
layer is your dbt project (with its Lightdash meta and lightdash.config.yml):
pass it with --dbt.`,
		Fields:  pgread.Fields("Lightdash", func(in agent.Inputs) (pgread.Querier, error) { return openDB(in) }),
		Context: nil, // the dbt project is the context; the global --dbt covers it
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
}

// openDB is replaced in tests with a fixture that serves rows.
var openDB = func(in agent.Inputs) (pgread.Querier, error) { return pgread.Open(in) }

// Threads are chosen first: the most recently active in the window (by
// their latest prompt), a page of agent.BatchSize at a time, newest first,
// up to --max-conversations. Each page's prompts and steps are then read with
// the page's thread ids as bound parameters ({ids}, after the Since bound
// $1), and that page's turns are built and emitted before the next page is
// read: one page of rows is held at a time.
//
// The window is on the prompt's created_at everywhere, so a turn arrives
// with all its steps or not at all. turn_position counts every prompt in the
// thread, before the window is applied.
const (
	threadsStmt = `SELECT t.ai_thread_uuid AS id, t.last_at FROM (
  SELECT ai_thread_uuid, max(created_at) AS last_at FROM ai_prompt
  WHERE created_at > $1 GROUP BY ai_thread_uuid
) t WHERE t.last_at < $2 OR (t.last_at = $2 AND t.ai_thread_uuid::text < $3)
ORDER BY t.last_at DESC, t.ai_thread_uuid::text DESC LIMIT $4`
	threadCountStmt = `SELECT count(DISTINCT ai_thread_uuid) AS n FROM ai_prompt WHERE created_at > $1`
	promptsStmt     = `SELECT * FROM (
  SELECT p.*, to_jsonb(t) AS thread,
         row_number() OVER (PARTITION BY p.ai_thread_uuid ORDER BY p.created_at, p.ai_prompt_uuid) AS turn_position
  FROM ai_prompt p JOIN ai_thread t ON t.ai_thread_uuid = p.ai_thread_uuid
  WHERE p.ai_thread_uuid IN ({ids})
) p WHERE p.created_at > $1 ORDER BY p.created_at`
	toolCallsStmt = `SELECT tc.* FROM ai_agent_tool_call tc
  JOIN ai_prompt p ON p.ai_prompt_uuid = tc.ai_prompt_uuid
  WHERE p.ai_thread_uuid IN ({ids}) AND p.created_at > $1 ORDER BY tc.created_at`
	toolResultsStmt = `SELECT tr.* FROM ai_agent_tool_result tr
  JOIN ai_prompt p ON p.ai_prompt_uuid = tr.ai_prompt_uuid
  WHERE p.ai_thread_uuid IN ({ids}) AND p.created_at > $1 ORDER BY tr.created_at`
	compiledStmt = `SELECT tr.ai_prompt_uuid, tr.tool_call_id, qh.query_uuid, qh.created_at, qh.compiled_sql,
       qh.status, qh.error, qh.total_row_count, qh.warehouse_execution_time_ms
  FROM ai_agent_tool_result tr
  JOIN ai_prompt p ON p.ai_prompt_uuid = tr.ai_prompt_uuid
  JOIN query_history qh ON qh.query_uuid::text = tr.metadata->>'queryUuid'
  WHERE p.ai_thread_uuid IN ({ids}) AND p.created_at > $1 ORDER BY qh.created_at`
	reasoningStmt = `SELECT r.* FROM ai_agent_reasoning r
  JOIN ai_prompt p ON p.ai_prompt_uuid = r.ai_prompt_uuid
  WHERE p.ai_thread_uuid IN ({ids}) AND p.created_at > $1 ORDER BY r.created_at`
	agentsStmt = `SELECT ai_agent_uuid, name FROM ai_agent ORDER BY name`
	// Only names and the primary email.
	usersStmt = `SELECT u.user_uuid, u.first_name, u.last_name, e.email
  FROM users u LEFT JOIN emails e ON e.user_id = u.user_id AND e.is_primary
  ORDER BY u.user_uuid`
)

const (
	threadsPurpose = "conversations (the most recently active threads in the window)"
	countPurpose   = "conversations in the window (count)"
)

// arm is one table read.
type arm struct {
	purpose, table, stmt string
	batched              bool // per page of thread ids, after the Since bound $1
	optional             bool // older Lightdash versions lack it
	users                bool // only with --include-users
}

// planned is the statement as the plan and the manifest show it.
func (a arm) planned() string {
	if !a.batched {
		return a.stmt
	}
	return pgread.Planned(a.stmt, "thread")
}

// directory arms are read once, before the threads; batch arms per page.
var directory = []arm{
	{purpose: "agent names", table: "ai_agent", stmt: agentsStmt, optional: true},
	{purpose: "user directory (names, emails)", table: "users", stmt: usersStmt, optional: true, users: true},
}

var batch = []arm{
	{purpose: "questions and answers", table: "ai_prompt", stmt: promptsStmt, batched: true},
	{purpose: "tool calls (metric queries, SQL)", table: "ai_agent_tool_call", stmt: toolCallsStmt, batched: true, optional: true},
	{purpose: "tool results (status, row counts)", table: "ai_agent_tool_result", stmt: toolResultsStmt, batched: true, optional: true},
	{purpose: "SQL compiled from the agent's metric queries", table: "query_history", stmt: compiledStmt, batched: true, optional: true},
	{purpose: "reasoning", table: "ai_agent_reasoning", stmt: reasoningStmt, batched: true, optional: true},
}

func directories(opts agent.Options) []arm {
	var out []arm
	for _, a := range directory {
		if a.users && !opts.IncludeUsers {
			continue
		}
		out = append(out, a)
	}
	return out
}

func plan(in agent.Inputs, opts agent.Options) []bundle.Query {
	var out []bundle.Query
	for _, a := range directories(opts) {
		out = append(out, bundle.Query{Purpose: a.purpose, Statement: a.stmt})
	}
	if opts.MaxConversations > 0 {
		out = append(out, bundle.Query{Purpose: countPurpose, Statement: threadCountStmt})
	}
	out = append(out, bundle.Query{Purpose: threadsPurpose, Statement: threadsStmt})
	for _, a := range batch {
		out = append(out, bundle.Query{Purpose: a.purpose, Statement: a.planned()})
	}
	return out
}

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	d, err := openDB(in)
	if err != nil {
		return agent.Result{}, err
	}
	defer d.Close()

	res := agent.Result{Sampled: true}
	dirs := map[string][]json.RawMessage{}
	for _, a := range directories(opts) {
		rows, err := d.Rows(ctx, a.stmt)
		if err != nil {
			if ctx.Err() != nil {
				return res, err
			}
			res.Notes = append(res.Notes, fmt.Sprintf("skipped %s: %v", a.table, err))
			continue
		}
		res.Queries = append(res.Queries, bundle.Query{Purpose: a.purpose, Statement: a.stmt, Rows: len(rows)})
		dirs[a.table] = rows
	}

	since := pgread.Since(opts)
	if opts.MaxConversations > 0 {
		if n, err := pgread.Count(ctx, d, threadCountStmt, since); err == nil {
			res.InWindow = &n
			res.Queries = append(res.Queries, bundle.Query{Purpose: countPurpose, Statement: threadCountStmt, Rows: 1})
		}
	}

	cols := columns(opts)
	skipped := map[string]bool{} // optional tables that failed
	hidden, unmapped := 0, 0
	// readPage reads one page of threads' rows, builds their turns and emits
	// them; nothing of the page is kept.
	readPage := func(ids []any) error {
		got := map[string][]json.RawMessage{"ai_agent": dirs["ai_agent"], "users": dirs["users"]}
		args := append([]any{since}, ids...)
		for _, a := range batch {
			if skipped[a.table] {
				continue
			}
			var rows []json.RawMessage
			err := d.Each(ctx, pgread.Batched(a.stmt, 2, len(ids)), args, func(r json.RawMessage) error {
				rows = append(rows, r)
				return nil
			})
			if err != nil {
				if ctx.Err() != nil || !a.optional {
					return fmt.Errorf("lightdash %s (is this Lightdash's database, with AI agents enabled?): %w", a.table, err)
				}
				skipped[a.table] = true
				res.Notes = append(res.Notes, fmt.Sprintf("skipped %s: %v", a.table, err))
				continue
			}
			res.Tally(a.purpose, a.planned(), len(rows))
			got[a.table] = rows
		}
		ts, h, err := buildTurns(got, opts)
		if err != nil {
			return err
		}
		hidden += h
		for _, t := range ts {
			rec, err := turns.Record(t, cols, "sql:ai_prompt")
			if err != nil {
				unmapped++
				continue
			}
			if err := emit(rec); err != nil {
				return err
			}
		}
		return nil
	}
	var batchErr error // an error from a page's reads or emit, returned as is
	n, err := pgread.EachPage(ctx, d, threadsStmt, since, opts.MaxConversations,
		func(rows int) { res.Tally(threadsPurpose, threadsStmt, rows) },
		func(ids []any) error { batchErr = readPage(ids); return batchErr })
	res.Conversations = n
	if err != nil {
		if err == batchErr || ctx.Err() != nil {
			return res, err
		}
		return res, fmt.Errorf("lightdash ai_prompt threads (is this Lightdash's database, with AI agents enabled?): %w", err)
	}
	if res.InWindow != nil && *res.InWindow > int64(n) {
		res.Notes = append(res.Notes, fmt.Sprintf("%s threads in the window: the %s most recent were read",
			agent.Thousands(*res.InWindow), agent.Thousands(int64(n))))
	}
	if hidden > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("left out %d hidden prompts (sent by Lightdash itself, not shown to the user)", hidden))
	}
	if unmapped > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("skipped %d prompts that could not be mapped to a turn", unmapped))
	}
	if !opts.IncludeResults {
		res.Redactions = append(res.Redactions, resultsRedaction)
	}
	return res, nil
}

const resultsRedaction = "Lightdash ai_agent_tool_result.result: rows returned by runSql, runQuery and the other data tools removed " +
	"(status, row count, compiled SQL and errors kept); semantic-layer lookups kept. Use --include-results to keep them."

// columns are what Lightdash records. The user's name and email are only
// read with --include-users.
func columns(opts agent.Options) turns.Columns {
	optional := []string{
		turns.ColCompletedAt, turns.ColOutcome, turns.ColErrorMessage, turns.ColConversationID,
		turns.ColPosition, turns.ColUserID, turns.ColClientInterface, turns.ColModelName,
		turns.ColStatements, turns.ColReasoning, turns.ColToolCalls, turns.ColExtra,
	}
	if opts.IncludeUsers {
		optional = append(optional, turns.ColUserName, turns.ColUserEmail)
	}
	return turns.NewColumns(optional...)
}

// clientInterface maps ai_thread.created_from to the contract's words.
func clientInterface(createdFrom string) string {
	switch createdFrom {
	case "web_app":
		return "web"
	case "":
		return ""
	}
	return createdFrom // slack, api, evals, scheduler, data_app, dashboard_analysis
}

func trim(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}
