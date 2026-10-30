// Package pgread reads a self-hosted application's own Postgres database, for
// agents whose conversations live in the app's tables (Lightdash, Metabase).
//
// It is the Postgres half of the nao reader, factored out because more than
// one agent needs exactly it: a read-only session
// (default_transaction_read_only=on) and server-rendered rows (row_to_json),
// so jsonb and large integers arrive exactly as stored.
package pgread

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/netsafe"
)

// Querier runs a SELECT and returns one JSON object per row: Rows for small
// results, Each to stream. Agents take it as an interface so tests can serve
// rows shaped like row_to_json output without a Postgres server.
type Querier interface {
	Rows(ctx context.Context, stmt string, args ...any) ([]json.RawMessage, error)
	Each(ctx context.Context, stmt string, args []any, fn func(json.RawMessage) error) error
	Close() error
}

// Fields are the connection inputs for an application database. app names
// it in the prompts, e.g. "Lightdash"; open is how the agent connects, so
// the password's live check connects as its Check does. TLS comes before
// the password, so that check connects as collecting will.
func Fields(app string, open func(agent.Inputs) (Querier, error)) []agent.Field {
	return []agent.Field{
		{Key: "host", Label: app + " Postgres host", Default: "localhost", Validate: agent.Host},
		{Key: "port", Label: "Postgres port", Default: "5432", Validate: agent.Port},
		{Key: "database", Label: "Database name", Help: "the database " + app + " stores its own data in"},
		{Key: "user", Label: "Username"},
		{Key: "sslmode", Label: "SSL mode", Default: netsafe.Auto, Choices: netsafe.PGSSLModes, Help: netsafe.PGSSLHelp},
		{Key: "sslrootcert", Label: "CA certificate file", Optional: true, When: verifying, Validate: agent.ExistingFile,
			Help: "PEM file of the CA that signed the server's certificate; leave empty for the system's CAs"},
		{Key: "password", Label: "Password", Secret: true, Optional: true, Verify: Verify(open)},
	}
}

// Verify connects with open and names the server, for a password field.
func Verify(open func(agent.Inputs) (Querier, error)) func(context.Context, agent.Inputs) (string, error) {
	return func(ctx context.Context, in agent.Inputs) (string, error) {
		q, err := open(in)
		if err != nil {
			return "", err
		}
		defer q.Close()
		return Connect(ctx, q, in)
	}
}

// verifying reports whether the server certificate is checked, which is when
// a CA file means something.
func verifying(in agent.Inputs) bool {
	m := in.Get("sslmode")
	return m == "verify-ca" || m == "verify-full"
}

// DB is a read-only connection to one application database.
type DB struct {
	conn     *sql.DB
	describe string // for errors, never includes the password
}

// Open connects with the inputs from Fields. Nothing is sent until the first
// query.
func Open(in agent.Inputs) (*DB, error) {
	conn, err := sql.Open("pgx", ConnURL(in, nil))
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	return &DB{conn: conn, describe: Describe(in)}, nil
}

// ConnURL is the connection URL for the inputs from Fields (host, port,
// database, user, password, sslmode, sslrootcert), for a read-only session,
// with extra parameters added. The sslmode is resolved by
// netsafe.PGSSLMode: TLS is required unless the host is this machine or the
// user chose otherwise.
func ConnURL(in agent.Inputs, extra url.Values) string {
	q := url.Values{"sslmode": {netsafe.PGSSLMode(in.Get("host"), in.Get("sslmode"), in.Get("sslrootcert"))},
		"default_transaction_read_only": {"on"}}
	if ca := in.Get("sslrootcert"); ca != "" {
		q.Set("sslrootcert", ca)
	}
	for k, v := range extra {
		q[k] = v
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(in.Get("user"), in["password"]),
		Host:     net.JoinHostPort(in.Get("host"), port(in)),
		Path:     "/" + in.Get("database"),
		RawQuery: q.Encode(),
	}
	return u.String()
}

// Describe names the connection for errors, without the password.
func Describe(in agent.Inputs) string {
	return in.Get("user") + "@" + net.JoinHostPort(in.Get("host"), port(in)) + "/" + in.Get("database")
}

func port(in agent.Inputs) string {
	if p := in.Get("port"); p != "" {
		return p
	}
	return "5432"
}

// Close closes the connection.
func (d *DB) Close() error { return d.conn.Close() }

// Rows runs stmt and returns one JSON object per row with every column,
// rendered by the server (row_to_json) so jsonb and large integers arrive
// exactly as stored instead of passing through float64. For small results;
// Each streams.
func (d *DB) Rows(ctx context.Context, stmt string, args ...any) ([]json.RawMessage, error) {
	var out []json.RawMessage
	err := d.Each(ctx, stmt, args, func(r json.RawMessage) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

// Each runs stmt and calls fn with each row as Rows renders it, as the
// rows arrive: nothing is accumulated. An error from fn stops the read.
func (d *DB) Each(ctx context.Context, stmt string, args []any, fn func(json.RawMessage) error) error {
	return Each(ctx, d.conn, d.describe, stmt, args, fn)
}

// Each runs stmt on conn wrapped in row_to_json and calls fn per row. The
// row's bytes are copied, so fn may keep them.
func Each(ctx context.Context, conn *sql.DB, describe, stmt string, args []any, fn func(json.RawMessage) error) error {
	wrapped := `SELECT row_to_json(r)::text FROM (` + stmt + `) AS r`
	rs, err := conn.QueryContext(ctx, wrapped, args...)
	if err != nil {
		return fmt.Errorf("query %s: %w", describe, err)
	}
	defer rs.Close()
	for rs.Next() {
		var s sql.NullString
		if err := rs.Scan(&s); err != nil {
			return err
		}
		if s.Valid {
			if err := fn(json.RawMessage(s.String)); err != nil {
				return err
			}
		}
	}
	return rs.Err()
}

// Since is the lower bound for a `created_at > $1` window: opts.Since, or the
// epoch when the whole history is wanted.
func Since(opts agent.Options) time.Time {
	if opts.Since.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return opts.Since.UTC()
}

// Decode reads a row keeping numbers exact, so an id above 2^53 survives.
func Decode(row json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(row))
	dec.UseNumber()
	return dec.Decode(v)
}

// Time parses a timestamp as row_to_json renders it: ISO 8601 with an offset
// for timestamptz ("2026-10-02T12:00:00.123+00:00", or "+00" with hours
// only), without one for timestamp, which is then read as UTC.
func Time(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999999-07",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02 15:04:05.999999999",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// TimePtr is Time for an optional column: nil when absent or unparseable.
func TimePtr(s *string) *time.Time {
	if s == nil {
		return nil
	}
	if t, ok := Time(*s); ok {
		return &t
	}
	return nil
}

// Str renders a JSON scalar as a string.
func Str(v any) string {
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
		return fmt.Sprintf("%v", t)
	}
}

// Present reports whether a raw JSON column holds a value (not absent, not
// null).
func Present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// JSONText reads a column that may hold JSON either as jsonb (an object or
// array in the row) or as text holding JSON (a string in the row), as
// Metabase's text columns do on Postgres. It returns the inner JSON.
func JSONText(raw json.RawMessage) json.RawMessage {
	if !Present(raw) {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return json.RawMessage(s)
	}
	return raw
}
