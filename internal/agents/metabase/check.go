package metabase

import (
	"context"
	"fmt"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/pgread"
)

// Check statements: each Collect statement under pgread.LimitZero, plus
// this count of questions (user messages) and conversations in the window.
const countStmt = `SELECT count(*) AS n, count(DISTINCT conversation_id) AS conversations
  FROM metabot_message WHERE role = 'user' AND created_at > $1`

func check(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	d, err := openDB(in)
	if err != nil {
		return agent.CheckResult{}, err
	}
	defer d.Close()
	since := pgread.Since(opts)
	tables := []pgread.Table{{Name: "metabot_message", Purpose: conversationsPurpose, Stmt: conversationsStmt,
		Args: append(append([]any{since}, pgread.FirstCursor...), 1)}}
	for _, a := range batch {
		// No conversation has an empty id: the probe reads nothing.
		tables = append(tables, pgread.Table{Name: a.table, Purpose: a.purpose, Stmt: pgread.Batched(a.stmt, 2, 1),
			Args: []any{since, noConversation}, Optional: a.optional})
	}
	if opts.IncludeUsers {
		tables = append(tables, pgread.Table{Name: usersArm.table, Purpose: usersArm.purpose, Stmt: usersStmt, Optional: true})
	}
	res, err := pgread.CheckApp(ctx, d, in, opts, "Metabase's Postgres application database, version 56 or later", tables)
	if err != nil {
		return res, err
	}
	rows, err := d.Rows(ctx, countStmt, pgread.Since(opts))
	if err == nil && len(rows) == 1 {
		var v struct {
			N             int64 `json:"n"`
			Conversations int64 `json:"conversations"`
		}
		if pgread.Decode(rows[0], &v) == nil {
			res.Facts = append(res.Facts, fmt.Sprintf("%s Metabot questions in %s",
				agent.Thousands(v.N), agent.Conversations(v.Conversations, "conversation", "conversations", opts)))
			res.Conversations = &v.Conversations
			if v.N == 0 {
				res.Warnings = append(res.Warnings, "no Metabot questions in the window: widen --since, or check Metabot is in use")
			}
		}
	}
	return res, nil
}

// noConversation is the id the batched probes bind: it casts to text and to
// uuid, whichever metabot_conversation.id is in this Metabase version.
const noConversation = "00000000-0000-0000-0000-000000000000"
