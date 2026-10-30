// Package metabase reads Metabase Metabot conversations
// (github.com/metabase/metabase).
//
// Metabase persists Metabot conversations in its application database:
// metabot_conversation (one row per conversation) and metabot_message (one
// row per message, role user or assistant). A message's content is the JSON
// text column `data`; its token usage per model is `usage`. Feedback is
// metabot_feedback, one row per submitter per message.
//
// `data` has two at-rest formats, told apart by data_version (absent before
// Metabase added it, which means 1):
//   - 2: a list of Vercel AI SDK UIMessage parts — text, reasoning, and
//     "tool-<name>" parts carrying input, state and a trimmed output whose
//     structured_output keeps query-id, query-content (the SQL), query (MBQL),
//     database and chart-type.
//   - 1: the user's message ([{role, content}]), or the native agent's
//     entries (text / tool-input / tool-output / data / error), or the retired
//     external ai-service's (TEXT / TOOL_CALL / TOOL_RESULT / ...). They are
//     converted to v2 parts here the way Metabase itself converts them on read
//     (metabase.metabot.schema.migrate-v1-to-v2).
//
// A turn is a user message plus the assistant messages that follow it in the
// conversation. Its statements are the SQL of the query-generation tools
// Metabase's own admin analytics read (create_sql_query, edit_sql_query,
// replace_sql_query): structured_output.query-content, falling back to the
// tool's sql_query / new_query input. construct_notebook_query builds MBQL,
// not SQL, so it is a tool call with the MBQL as its output. Metabot creates
// queries without running them; when it runs one with run_query (by query
// id) in the same turn, the statement is marked succeeded with the row count
// run_query reported, else it carries no status.
// Reasoning is not persisted by Metabase.
//
// Written from the source at commit 037141bb2a02e63e75a7abed6b3416518276e9e1
// (2026-10-29): resources/migrations/056_update_migrations.yaml (the tables),
// 060/, 061/, 064/ and 065/ (later columns and metabot_feedback),
// src/metabase/metabot/schema/{v1,v2,migrate_v1_to_v2}.clj,
// src/metabase/metabot/persistence.clj and
// enterprise/backend/src/metabase_enterprise/metabot_analytics/queries.clj.
// Tested against fixtures only. Messages are read whole (m.*), so columns
// added in later versions are used when present and not required.
//
// Postgres only. Metabase's application database can also be MySQL/MariaDB
// or H2; those are not supported.
package metabase

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/pgread"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

func init() {
	agent.Register(agent.Agent{
		ID:      "metabase",
		Name:    "Metabase Metabot",
		Summary: "Metabot conversations; reads Metabase's own Postgres application database",
		Status:  agent.Experimental,
		Setup: `Metabase stores Metabot conversations in its application database (the one
MB_DB_* points Metabase at). Only a Postgres application database is
supported; MySQL/MariaDB and the default H2 file are not.

Read-only access is enough; a dedicated user is best:

  CREATE USER benchouse_reader PASSWORD '...';
  GRANT SELECT ON metabot_conversation, metabot_message, metabot_feedback
                  TO benchouse_reader;
  -- only with --include-users:
  GRANT SELECT ON core_user TO benchouse_reader;

Metabot's semantic layer (models, metrics, segments) lives inside Metabase,
not in files, so there is no context folder to collect.`,
		Fields:  pgread.Fields("Metabase", func(in agent.Inputs) (pgread.Querier, error) { return openDB(in) }),
		Context: nil,
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
}

// openDB is replaced in tests with a fixture that serves rows.
var openDB = func(in agent.Inputs) (pgread.Querier, error) { return pgread.Open(in) }

// Conversations are chosen first: the most recently active in the window
// (by their latest message), a page of agent.BatchSize at a time, newest
// first, up to --max-conversations. Each page's messages and feedback are
// then read with the page's conversation ids as bound parameters ({ids},
// after the Since bound $1), and that page's turns are built and emitted
// before the next page is read: one page of rows is held at a time.
//
// role_position numbers each role's messages within the whole conversation
// before the window applies, so a user message's role_position is the turn's
// position.
const (
	conversationsStmt = `SELECT c.conversation_id AS id, c.last_at FROM (
  SELECT conversation_id, max(created_at) AS last_at FROM metabot_message
  WHERE created_at > $1 GROUP BY conversation_id
) c WHERE c.last_at < $2 OR (c.last_at = $2 AND c.conversation_id::text < $3)
ORDER BY c.last_at DESC, c.conversation_id::text DESC LIMIT $4`
	conversationCountStmt = `SELECT count(DISTINCT conversation_id) AS n FROM metabot_message WHERE created_at > $1`
	messagesStmt          = `SELECT * FROM (
  SELECT m.*, c.user_id AS conversation_user_id,
         row_number() OVER (PARTITION BY m.conversation_id, m.role ORDER BY m.created_at, m.id) AS role_position
  FROM metabot_message m JOIN metabot_conversation c ON c.id = m.conversation_id
  WHERE m.conversation_id IN ({ids})
) m WHERE m.created_at > $1 ORDER BY m.conversation_id, m.created_at, m.id`
	feedbackStmt = `SELECT f.* FROM metabot_feedback f
  JOIN metabot_message m ON m.id = f.message_id
  WHERE m.conversation_id IN ({ids}) AND m.created_at > $1 ORDER BY f.created_at`
	// Only names and email.
	usersStmt = `SELECT id, first_name, last_name, email FROM core_user ORDER BY id`
)

const (
	conversationsPurpose = "conversations (the most recently active in the window)"
	countPurpose         = "conversations in the window (count)"
)

// arm is one table read.
type arm struct {
	purpose, table, stmt string
	optional             bool // older Metabase versions lack it
}

// planned is a batched statement as the plan and the manifest show it.
func (a arm) planned() string { return pgread.Planned(a.stmt, "conversation") }

// batch arms are read per page of conversation ids.
var batch = []arm{
	{purpose: "Metabot messages (questions, answers, generated queries)", table: "metabot_message", stmt: messagesStmt},
	{purpose: "thumbs up and down", table: "metabot_feedback", stmt: feedbackStmt, optional: true},
}

// usersArm is the directory, read once, only with --include-users.
var usersArm = arm{purpose: "user directory (names, emails)", table: "core_user", stmt: usersStmt, optional: true}

func plan(in agent.Inputs, opts agent.Options) []bundle.Query {
	var out []bundle.Query
	if opts.IncludeUsers {
		out = append(out, bundle.Query{Purpose: usersArm.purpose, Statement: usersArm.stmt})
	}
	if opts.MaxConversations > 0 {
		out = append(out, bundle.Query{Purpose: countPurpose, Statement: conversationCountStmt})
	}
	out = append(out, bundle.Query{Purpose: conversationsPurpose, Statement: conversationsStmt})
	for _, a := range batch {
		out = append(out, bundle.Query{Purpose: a.purpose, Statement: a.planned()})
	}
	return out
}

const hint = "is this Metabase's Postgres application database, version 56 or later?"

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	d, err := openDB(in)
	if err != nil {
		return agent.Result{}, err
	}
	defer d.Close()

	res := agent.Result{Sampled: true}
	var users []json.RawMessage
	if opts.IncludeUsers {
		users, err = d.Rows(ctx, usersStmt)
		if err != nil {
			if ctx.Err() != nil {
				return res, err
			}
			res.Notes = append(res.Notes, fmt.Sprintf("skipped %s: %v", usersArm.table, err))
		} else {
			res.Queries = append(res.Queries, bundle.Query{Purpose: usersArm.purpose, Statement: usersStmt, Rows: len(users)})
		}
	}

	since := pgread.Since(opts)
	if opts.MaxConversations > 0 {
		if n, err := pgread.Count(ctx, d, conversationCountStmt, since); err == nil {
			res.InWindow = &n
			res.Queries = append(res.Queries, bundle.Query{Purpose: countPurpose, Statement: conversationCountStmt, Rows: 1})
		}
	}

	cols := columns(opts)
	skipped := map[string]bool{} // optional tables that failed
	copies, orphans, unmapped := 0, 0, 0
	// readPage reads one page of conversations' rows, builds their turns and
	// emits them; nothing of the page is kept.
	readPage := func(ids []any) error {
		got := map[string][]json.RawMessage{"core_user": users}
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
					return fmt.Errorf("metabase %s (%s): %w", a.table, hint, err)
				}
				skipped[a.table] = true
				res.Notes = append(res.Notes, fmt.Sprintf("skipped %s: %v", a.table, err))
				continue
			}
			res.Tally(a.purpose, a.planned(), len(rows))
			got[a.table] = rows
		}
		ts, c, o := buildTurns(got, opts)
		copies, orphans = copies+c, orphans+o
		for _, t := range ts {
			rec, err := turns.Record(t, cols, "sql:metabot_message")
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
	n, err := pgread.EachPage(ctx, d, conversationsStmt, since, opts.MaxConversations,
		func(rows int) { res.Tally(conversationsPurpose, conversationsStmt, rows) },
		func(ids []any) error { batchErr = readPage(ids); return batchErr })
	res.Conversations = n
	if err != nil {
		if err == batchErr || ctx.Err() != nil {
			return res, err
		}
		return res, fmt.Errorf("metabase metabot_message conversations (%s): %w", hint, err)
	}
	if res.InWindow != nil && *res.InWindow > int64(n) {
		res.Notes = append(res.Notes, fmt.Sprintf("%s conversations in the window: the %s most recent were read",
			agent.Thousands(*res.InWindow), agent.Thousands(int64(n))))
	}
	res.Notes = append(res.Notes, leftOut(copies, orphans)...)
	if unmapped > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("skipped %d messages that could not be mapped to a turn", unmapped))
	}
	if !opts.IncludeResults {
		res.Redactions = append(res.Redactions, resultsRedaction)
	}
	return res, nil
}

const resultsRedaction = "Metabase metabot_message.data tool outputs: the text returned to the model (which holds query results for " +
	"run_query, field values and similar tools) removed; generated SQL/MBQL, query ids, errors and metadata lookups kept. " +
	"Use --include-results to keep them."

// columns are what Metabot records. The user's name and email are only read
// with --include-users.
func columns(opts agent.Options) turns.Columns {
	optional := []string{
		turns.ColOutcome, turns.ColErrorMessage, turns.ColConversationID, turns.ColPosition,
		turns.ColUserID, turns.ColClientInterface, turns.ColModelName, turns.ColInputTokens,
		turns.ColOutputTokens, turns.ColStatements, turns.ColToolCalls, turns.ColExtra,
	}
	if opts.IncludeUsers {
		optional = append(optional, turns.ColUserName, turns.ColUserEmail)
	}
	return turns.NewColumns(optional...)
}
