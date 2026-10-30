package nao

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/pgread"
)

// Check statements: the version, the project list, each Collect statement
// under pgread.LimitZero, and this count of chats in the window.
const (
	sqliteVersionStmt = `SELECT sqlite_version() AS version`
)

// Rows lets the Postgres check helpers run statements on d.
func (d *db) Rows(ctx context.Context, stmt string, args ...any) ([]json.RawMessage, error) {
	return d.rows(ctx, stmt, args...)
}

// Each streams stmt's rows, for the same helpers.
func (d *db) Each(ctx context.Context, stmt string, args []any, fn func(json.RawMessage) error) error {
	return d.each(ctx, stmt, args, fn)
}

// connFact connects and names the database: the check's first finding,
// and the live check of the password or sqlite path field.
func connFact(ctx context.Context, d *db, in agent.Inputs) (string, error) {
	if d.postgres {
		return pgread.Connect(ctx, d, in)
	}
	rows, err := d.rows(ctx, sqliteVersionStmt)
	if err != nil {
		return "", fmt.Errorf("could not open %s: %s", in.Get("sqlite-path"), agent.Clip(err))
	}
	var v struct{ Version string }
	if len(rows) > 0 {
		_ = json.Unmarshal(rows[0], &v)
	}
	return fmt.Sprintf("Opened nao's sqlite database %s (SQLite %s)", filepath.Base(in.Get("sqlite-path")), v.Version), nil
}

func verifyConn(ctx context.Context, in agent.Inputs) (string, error) {
	d, err := open(in)
	if err != nil {
		return "", err
	}
	defer d.Close()
	fact, err := connFact(ctx, d, in)
	if err != nil {
		return "", err
	}
	if d.note != "" {
		fact += " (" + d.note + ")"
	}
	return fact, nil
}

func check(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	var res agent.CheckResult
	d, err := open(in)
	if err != nil {
		return res, err
	}
	defer d.Close()
	if d.note != "" {
		res.Warnings = append(res.Warnings, d.note)
	}
	fact, err := connFact(ctx, d, in)
	if err != nil {
		return res, err
	}
	res.Facts = append(res.Facts, fact)

	projects, err := d.rows(ctx, projectsStmt)
	if err != nil {
		return res, fmt.Errorf("cannot list nao projects (is this nao's database?%s): %s", grantHint(d, in, "project"), agent.Clip(err))
	}
	projectID, err := pickProject(projects, in.Get("project"))
	if err != nil {
		return res, err
	}
	res.Facts = append(res.Facts, projectFact(projects, projectID))

	var sinceMS int64
	if !opts.Since.IsZero() {
		sinceMS = opts.Since.UnixMilli()
	}
	since := d.bound(sinceMS)
	chatsQ, countQ := chatStatements(ctx, d)
	if _, err := d.rows(ctx, pgread.LimitZero(chatsQ), projectID, since, d.limit(opts.MaxConversations)); err != nil {
		return res, fmt.Errorf("nao chat is not readable (needed for %s%s): %s", chatsPurpose, grantHint(d, in, "chat"), agent.Clip(err))
	}
	for _, a := range arms {
		var err error
		switch a.scope {
		case users:
			if !opts.IncludeUsers {
				continue
			}
			_, err = d.rows(ctx, pgread.LimitZero(a.stmt))
		case window:
			_, err = d.rows(ctx, pgread.LimitZero(a.stmt), projectID, since, d.limit(mcpPerChat*opts.MaxConversations))
		case children:
			_, err = d.rows(ctx, pgread.LimitZero(a.sql(1)), "", since)
		}
		if err == nil {
			continue
		}
		if a.optional {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s not readable, so %s will be missing (older nao versions lack it%s): %s",
				a.table, a.purpose, grantHint(d, in, a.table), agent.Clip(err)))
			continue
		}
		return res, fmt.Errorf("nao %s is not readable (needed for %s%s): %s", a.table, a.purpose, grantHint(d, in, a.table), agent.Clip(err))
	}

	if n, err := countChats(ctx, d, countQ, projectID, since); err == nil {
		res.Facts = append(res.Facts, agent.Conversations(n, "chat", "chats", opts))
		res.Conversations = &n
		if n == 0 {
			res.Warnings = append(res.Warnings, "no chats in the window: widen --since, or check this is the project people use")
		}
	}
	return res, nil
}

// grantHint is the Postgres grant that would make table readable, or nothing
// for a sqlite file, which has no grants.
func grantHint(d *db, in agent.Inputs, table string) string {
	if !d.postgres {
		return ""
	}
	if table == "user" {
		return fmt.Sprintf(`; on Postgres: GRANT SELECT (id, name, email, email_verified) ON "user" TO %s`, in.Get("user"))
	}
	return fmt.Sprintf("; on Postgres: GRANT SELECT ON %s TO %s", table, in.Get("user"))
}

// projectFact names the project that will be read, and the others.
func projectFact(rows []json.RawMessage, id string) string {
	var chosen string
	var names []string
	for _, r := range rows {
		var p struct {
			ID   any    `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(r, &p) != nil {
			continue
		}
		label := p.Name
		if label == "" {
			label = str(p.ID)
		}
		names = append(names, label)
		if str(p.ID) == id {
			chosen = label
		}
	}
	if len(names) == 1 {
		return "nao project " + chosen
	}
	return fmt.Sprintf("%d nao projects (%s); reading %s", len(names), strings.Join(names, ", "), chosen)
}
