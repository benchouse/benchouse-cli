package sigma

import (
	"context"
	"fmt"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// Check: sign in (Snowflake: snowflake.SessionStmt; Databricks: SCIM Me and
// the warehouse; BigQuery: the token exchange), the two information-schema
// statements Collect runs, and Collect's count of the conversations in the
// window with the latest activity.

func check(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	var res agent.CheckResult
	b, err := open(in)
	if err != nil {
		return res, err
	}
	fact, err := b.signIn(ctx)
	if err != nil {
		return res, err
	}
	res.Facts = append(res.Facts, fact)
	l, candidates, err := discover(ctx, b, in, func(string, string, int) {})
	if err != nil {
		return res, err
	}
	res.Facts = append(res.Facts, tableFact(l))
	if len(candidates) > 1 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("several AI usage tables (%s): %s will be read; give --table to choose",
			strings.Join(candidates, ", "), l.name))
	}
	if a := l.absent(); len(a) > 0 {
		res.Warnings = append(res.Warnings, "the AI usage table lacks documented columns, which will be empty: "+strings.Join(a, ", "))
	}
	if u := l.unmapped(); len(u) > 0 {
		res.Facts = append(res.Facts, "Columns not mapped (not collected): "+strings.Join(u, ", "))
	}
	var n int64
	var last string
	if _, err := b.each(ctx, l.countStmt(opts), nil, func(m map[string]any) error {
		if f, ok := number(m["n"]); ok {
			n = int64(f)
		}
		if t, ok := parseTime(m["last_at"]); ok {
			last = t.Format("2006-01-02 15:04 UTC")
		}
		return nil
	}); err != nil {
		return res, fmt.Errorf("%s is not readable (%s): grant the connection SELECT on it", l.ref(), agent.Clip(err))
	}
	res.Conversations = &n
	f := agent.Conversations(n, "Sigma AI conversation", "Sigma AI conversations", opts)
	if last != "" {
		f += "; the latest at " + last
	}
	res.Facts = append(res.Facts, f)
	if n == 0 {
		res.Warnings = append(res.Warnings, "no AI usage logged "+agent.Window(opts)+
			": widen --since; only questions asked after the AI usage table was set up are logged")
	}
	return res, nil
}

func tableFact(l layout) string {
	return fmt.Sprintf("Sigma AI usage table %s: %d columns, %d of %d documented", l.ref(), len(l.cols),
		len(documented)-len(l.absent()), len(documented))
}

// tableLookup lists the AI usage tables in the location, for the prompt.
func tableLookup(ctx context.Context, in agent.Inputs) ([][2]string, error) {
	b, err := open(in)
	if err != nil {
		return nil, err
	}
	d := dialectFor(in.Get("platform"))
	loc, err := locationOf(in)
	if err != nil {
		return nil, err
	}
	stmt, args := d.tables(loc, "")
	out := [][2]string{{"", "find it (the first AI_USAGE_ table)"}}
	_, err = b.each(ctx, stmt, args, func(m map[string]any) error {
		if n := str(m["name"]); n != "" && validName(n) == nil {
			out = append(out, [2]string{n, n})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 1 {
		return nil, errNotConfigured(loc, "")
	}
	return out, nil
}

// verifyTable finds the table and checks its columns, as Check does.
func verifyTable(ctx context.Context, in agent.Inputs) (string, error) {
	b, err := open(in)
	if err != nil {
		return "", err
	}
	l, _, err := discover(ctx, b, in, func(string, string, int) {})
	if err != nil {
		return "", err
	}
	return tableFact(l), nil
}
