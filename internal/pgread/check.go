package pgread

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// Helpers for an agent's Check: what the server is, whether a statement can
// run, and how many rows a window holds, each as cheap as Postgres allows.

// LimitZero wraps a SELECT so the server parses and plans it, checking every
// table, column and privilege it names, but reads no rows. It works the same
// on sqlite.
func LimitZero(stmt string) string { return "SELECT * FROM (\n" + stmt + "\n) AS probe LIMIT 0" }

// ServerStmt names the server and the session user.
const ServerStmt = `SELECT version() AS version, current_user AS usr`

// Server runs ServerStmt and returns a fact like "Connected to PostgreSQL
// 16.2 at db:5432/lightdash as benchouse_reader". No password is in it.
func Server(ctx context.Context, q Querier, in agent.Inputs) (string, error) {
	rows, err := q.Rows(ctx, ServerStmt)
	if err != nil {
		return "", err
	}
	var v struct {
		Version string `json:"version"`
		User    string `json:"usr"`
	}
	if len(rows) > 0 {
		_ = json.Unmarshal(rows[0], &v)
	}
	return ServerFact(v.Version, in.Get("host")+":"+port(in)+"/"+in.Get("database"), v.User), nil
}

// Connect is Server with the error every application check gives when it
// cannot connect.
func Connect(ctx context.Context, q Querier, in agent.Inputs) (string, error) {
	fact, err := Server(ctx, q, in)
	if err != nil {
		return "", fmt.Errorf("could not connect: %s", agent.Clip(err))
	}
	return fact, nil
}

// ServerFact renders a connection fact from version() output.
func ServerFact(version, where, user string) string {
	s := "Connected to " + ShortVersion(version)
	if where != "" {
		s += " at " + where
	}
	if user != "" {
		s += " as " + user
	}
	return s
}

// ShortVersion keeps the product and number of a version() string:
// "PostgreSQL 16.2 on aarch64-apple-darwin..." becomes "PostgreSQL 16.2".
func ShortVersion(v string) string {
	f := strings.Fields(v)
	switch {
	case len(f) >= 2:
		return strings.TrimSuffix(f[0]+" "+f[1], ",")
	case len(f) == 1:
		return f[0]
	}
	return "Postgres"
}

// Count runs a statement returning one row with a column n and returns n.
func Count(ctx context.Context, q Querier, stmt string, args ...any) (int64, error) {
	rows, err := q.Rows(ctx, stmt, args...)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, fmt.Errorf("the count returned no row")
	}
	var v struct {
		N json.Number `json:"n"`
	}
	if err := Decode(rows[0], &v); err != nil {
		return 0, err
	}
	return v.N.Int64()
}

// Table is one statement an agent's Collect runs, as its Check sees it.
type Table struct {
	// Name is the table the statement depends on, for messages.
	Name string
	// Purpose says what is lost when an optional table cannot be read.
	Purpose string
	// Stmt is the Collect statement, probed with LimitZero.
	Stmt string
	// Windowed statements take the Since bound as $1.
	Windowed bool
	// Args, if set, are the probe's arguments instead (a batched statement
	// probed with one id, a keyset statement with its first-page cursor).
	Args []any
	// Optional tables become a warning when they cannot be read.
	Optional bool
}

// Probe runs each table's statement with LimitZero. A required table that
// fails is an error naming the grant to give; an optional one a warning.
func Probe(ctx context.Context, q Querier, in agent.Inputs, opts agent.Options, tables []Table, res *agent.CheckResult) error {
	user := in.Get("user")
	if user == "" {
		user = "<user>"
	}
	for _, t := range tables {
		var err error
		switch {
		case t.Args != nil:
			_, err = q.Rows(ctx, LimitZero(t.Stmt), t.Args...)
		case t.Windowed:
			_, err = q.Rows(ctx, LimitZero(t.Stmt), Since(opts))
		default:
			_, err = q.Rows(ctx, LimitZero(t.Stmt))
		}
		if err == nil {
			continue
		}
		if t.Optional {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s not readable, so %s will be missing (an older version may lack it; "+
				"otherwise GRANT SELECT ON %s TO %s): %s", t.Name, t.Purpose, t.Name, user, agent.Clip(err)))
			continue
		}
		return fmt.Errorf("%s is not readable: %s. Check this is the right database and run GRANT SELECT ON %s TO %s",
			t.Name, agent.Clip(err), t.Name, user)
	}
	return nil
}

// CheckApp is the start of an application database's check: connect, name
// the server, and probe every table. app names the application in errors,
// hint says what the database should be, e.g. "Lightdash's database, with
// AI agents enabled".
func CheckApp(ctx context.Context, q Querier, in agent.Inputs, opts agent.Options, hint string, tables []Table) (agent.CheckResult, error) {
	var res agent.CheckResult
	fact, err := Connect(ctx, q, in)
	if err != nil {
		return res, err
	}
	res.Facts = append(res.Facts, fact)
	if err := Probe(ctx, q, in, opts, tables, &res); err != nil {
		return res, fmt.Errorf("%w (is this %s?)", err, hint)
	}
	return res, nil
}
