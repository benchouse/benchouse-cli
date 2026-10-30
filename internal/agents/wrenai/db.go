package wrenai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
	_ "modernc.org/sqlite"             // pure-Go sqlite, registers "sqlite"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/pgread"
	"github.com/benchouse/benchouse-cli/internal/sqliteuri"
)

// db reads wren-ui's application database: a sqlite file by default
// (DB_TYPE=sqlite, SQLITE_FILE=/app/data/db.sqlite3 in Wren's
// docker-compose), or Postgres when DB_TYPE=pg and PG_URL are set. Both are
// created by the same knex migrations; they differ in two ways handled here:
// knex timestamps are `timestamptz` on Postgres and `datetime` text on sqlite,
// and jsonb columns are text on sqlite.
//
// The helpers follow internal/agents/nao/db.go.
type db struct {
	conn     *sql.DB
	postgres bool
	describe string // for errors, never includes the password
	note     string // for the user, e.g. an unread write-ahead log
}

func open(in map[string]string) (*db, error) {
	switch in["storage"] {
	case "sqlite":
		p := strings.TrimSpace(in["sqlite-path"])
		// Read-only and immutable: safe while wren-ui holds the file open.
		dsn, err := sqliteuri.DSN(p, true)
		if err != nil {
			return nil, err
		}
		conn, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		conn.SetMaxOpenConns(1)
		return &db{conn: conn, describe: p, note: sqliteuri.WALNote(p, "wren-ui")}, nil
	case "postgres":
		conn, err := sql.Open("pgx", pgread.ConnURL(agent.Inputs(in), nil))
		if err != nil {
			return nil, err
		}
		conn.SetMaxOpenConns(1)
		return &db{conn: conn, postgres: true, describe: pgread.Describe(agent.Inputs(in))}, nil
	}
	return nil, fmt.Errorf("storage must be postgres or sqlite, not %q", in["storage"])
}

func (d *db) Close() error { return d.conn.Close() }

// bound renders a lower bound on created_at in the type the dialect compares
// against: a timestamp on Postgres, and on sqlite the text form SQLite's
// CURRENT_TIMESTAMP default writes (UTC, "YYYY-MM-DD HH:MM:SS").
func (d *db) bound(t time.Time) any {
	if d.postgres {
		return t.UTC()
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

// rows runs stmt and returns one JSON object per row with every column.
// For small results (versions, counts); each streams.
func (d *db) rows(ctx context.Context, stmt string, args ...any) ([]json.RawMessage, error) {
	var out []json.RawMessage
	err := d.each(ctx, stmt, args, func(r json.RawMessage) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

// each runs stmt and calls fn with one JSON object per row, as rows arrive,
// holding none of them. An error from fn stops the read.
//
// On Postgres the server renders the JSON (row_to_json), so jsonb arrives as
// objects. On sqlite, []byte values are rendered as strings so text never
// becomes base64; jsonb columns are then strings holding JSON.
func (d *db) each(ctx context.Context, stmt string, args []any, fn func(json.RawMessage) error) error {
	if d.postgres {
		return pgread.Each(ctx, d.conn, "wren-ui "+d.describe, rebind(stmt), args, fn)
	}
	rs, err := d.conn.QueryContext(ctx, stmt, args...)
	if err != nil {
		return fmt.Errorf("query wren-ui %s: %w", d.describe, err)
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	m := make(map[string]any, len(cols))
	for rs.Next() {
		for i := range vals {
			vals[i] = nil
		}
		if err := rs.Scan(ptrs...); err != nil {
			return err
		}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				m[c] = string(b)
				continue
			}
			m[c] = vals[i]
		}
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return err
		}
	}
	return rs.Err()
}

// rebind rewrites `?` placeholders to `$n` for Postgres. Safe because the
// statements are constants in this package with no `?` inside literals.
func rebind(stmt string) string {
	var b strings.Builder
	n := 0
	for _, r := range stmt {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
