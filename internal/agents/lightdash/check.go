package lightdash

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/pgread"
)

// Check statements: each Collect statement under pgread.LimitZero, plus
// these.
const (
	countStmt = `SELECT count(*) AS n FROM ai_prompt WHERE created_at > $1`
)

// noThread is a thread id no row has, to probe a batched statement.
const noThread = "00000000-0000-0000-0000-000000000000"

func checkTables(opts agent.Options) []pgread.Table {
	since := pgread.Since(opts)
	out := []pgread.Table{{Name: "ai_prompt", Purpose: threadsPurpose, Stmt: threadsStmt,
		Args: append(append([]any{since}, pgread.FirstCursor...), 1)}}
	for _, a := range batch {
		out = append(out, pgread.Table{Name: a.table, Purpose: a.purpose, Stmt: pgread.Batched(a.stmt, 2, 1),
			Args: []any{since, noThread}, Optional: a.optional})
	}
	for _, a := range directories(opts) {
		out = append(out, pgread.Table{Name: a.table, Purpose: a.purpose, Stmt: a.stmt, Optional: a.optional})
	}
	return out
}

func check(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	d, err := openDB(in)
	if err != nil {
		return agent.CheckResult{}, err
	}
	defer d.Close()
	res, err := pgread.CheckApp(ctx, d, in, opts, "Lightdash's database, with AI agents enabled", checkTables(opts))
	if err != nil {
		return res, err
	}
	if n, err := pgread.Count(ctx, d, countStmt, pgread.Since(opts)); err == nil {
		res.Facts = append(res.Facts, agent.Counted(n, "AI agent question", "AI agent questions", opts))
		if n == 0 {
			res.Warnings = append(res.Warnings, "no AI agent questions in the window: widen --since, or check AI agents are in use on this instance")
		}
	}
	if n, err := pgread.Count(ctx, d, threadCountStmt, pgread.Since(opts)); err == nil {
		res.Facts = append(res.Facts, agent.Conversations(n, "thread", "threads", opts))
		res.Conversations = &n
	}
	if rows, err := d.Rows(ctx, agentsStmt); err == nil && len(rows) > 0 {
		var names []string
		for _, r := range rows {
			var a struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(r, &a) == nil && a.Name != "" {
				names = append(names, a.Name)
			}
		}
		if len(names) > 5 {
			names = append(names[:5], "...")
		}
		res.Facts = append(res.Facts, fmt.Sprintf("%d AI agents: %s", len(rows), strings.Join(names, ", ")))
	}
	return res, nil
}
