// Package nao reads conversations from nao (github.com/getnao/nao).
//
// nao keeps its history in its own application database, Postgres for a
// team deployment or a sqlite file for a default local install, and that
// database is the only complete record: the SQL nao ran is a row in
// message_part next to the question, the answer and the reasoning. The
// statements here are the ones Benchouse's live nao connector runs.
package nao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/netsafe"
	"github.com/benchouse/benchouse-cli/internal/pgread"
)

const sourceKind = "nao"

func init() {
	agent.Register(agent.Agent{
		ID:      "nao",
		Name:    "nao",
		Summary: "open-source analytics agent; reads its Postgres or sqlite database",
		Status:  agent.Supported,
		Setup: `nao stores its chats in its own database: Postgres when DB_URI is set,
otherwise a sqlite file (db.sqlite next to where nao runs).

Read-only access is enough. On Postgres, a dedicated user is best:

  CREATE USER benchouse_reader PASSWORD '...';
  GRANT SELECT ON chat, chat_message, message_part, llm_inference,
                  message_feedback, mcp_call_log, project TO benchouse_reader;`,
		Fields: []agent.Field{
			{Key: "storage", Label: "Where does nao store its data?", Choices: []string{"postgres", "sqlite"}, Default: "postgres",
				Help: "postgres if nao's DB_URI points at Postgres; sqlite for a default local install"},
			{Key: "host", Label: "Postgres host", When: isPG, Default: "localhost", Validate: agent.Host},
			{Key: "port", Label: "Postgres port", When: isPG, Default: "5432", Validate: agent.Port},
			{Key: "database", Label: "Database name", When: isPG},
			{Key: "user", Label: "Username", When: isPG},
			// TLS before the password, so the password's check connects as
			// collecting will.
			{Key: "sslmode", Label: "SSL mode", When: isPG, Default: netsafe.Auto, Choices: netsafe.PGSSLModes, Help: netsafe.PGSSLHelp},
			{Key: "password", Label: "Password", When: isPG, Secret: true, Optional: true, Verify: verifyConn},
			{Key: "sqlite-path", Label: "Path to nao's db.sqlite", When: isSQLite, Validate: agent.ExistingFile, Verify: verifyConn},
			{Key: "project", Label: "nao project (id or name)", Optional: true, Lookup: Projects,
				Help: "only needed when the database holds more than one project"},
		},
		Context: &agent.Context{
			Label:    "nao context folder",
			Help:     "the folder nao was initialised in: it holds RULES.md, nao_config.yaml, databases/ and semantics/",
			Check:    checkContext,
			Keep:     keepContext,
			KeepWith: keepContextWith,
		},
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
}

func isPG(in agent.Inputs) bool     { return in.Get("storage") != "sqlite" }
func isSQLite(in agent.Inputs) bool { return in.Get("storage") == "sqlite" }

// keepContext is the files nao reads as context, under the default options.
func keepContext(rel string) bool { return keepContextWith(rel, agent.Options{}) }

// keepContextWith is the files nao reads as context. nao_config.yaml is kept
// with its secrets masked; it says which databases and models the agent uses.
// The sample-data files under databases/ (preview.md, profiling.md) hold the
// warehouse's values, so they come only with IncludeResults.
func keepContextWith(rel string, opts agent.Options) bool {
	if dataSample(rel) && !opts.IncludeResults {
		return false
	}
	switch {
	case rel == "RULES.md", rel == "nao_config.yaml":
		return true
	case strings.HasPrefix(rel, "databases/"), strings.HasPrefix(rel, "semantics/"):
		return true
	case strings.HasPrefix(rel, "agent/") && path.Ext(rel) == ".md":
		return true
	}
	return false
}

// arm is one table read.
type arm struct {
	purpose, kind, table, cursor, stmt string
	keys                               func(map[string]any) map[string]string
	scope                              scope
	optional                           bool // older nao versions lack the table
}

// scope says how an arm's rows are chosen.
type scope int

const (
	// children: rows of the chosen chats, fetched agent.BatchSize chat ids
	// at a time. The statement's {ids} becomes the batch's placeholders.
	children scope = iota
	// window: the most recent rows of the project in the window, LIMIT
	// mcpPerChat × the conversation cap.
	window
	// users: the directory, only with --include-users.
	users
)

// idsToken marks where a children statement's id list goes.
const idsToken = "{ids}"

// chatsStmt chooses the conversations: the most recent chats of the
// project updated in the window, at most the cap (LIMIT -1 or NULL is none).
const chatsStmt = `SELECT * FROM chat WHERE project_id = ? AND updated_at > ? ORDER BY updated_at DESC LIMIT ?`

// chatCountStmt counts the chats the window holds, for the sample.
const chatCountStmt = `SELECT count(*) AS n FROM chat WHERE project_id = ? AND updated_at > ?`

const (
	chatsPurpose = "conversations (the most recent chats in the window)"
	countPurpose = "conversations in the window (count)"
	chatKind     = "nao_chat"
)

// mcpPerChat bounds the MCP call log, which is not tied to chats: the most
// recent mcpPerChat × MaxConversations calls in the window.
const mcpPerChat = 5

func chatKeys(m map[string]any) map[string]string { return map[string]string{"chat_id": str(m["id"])} }

var arms = []arm{
	{purpose: "messages", kind: "nao_chat_message", table: "chat_message", cursor: "created_at",
		stmt: `SELECT * FROM chat_message WHERE chat_id IN ({ids}) AND created_at > ? ORDER BY created_at`,
		keys: func(m map[string]any) map[string]string {
			return map[string]string{"message_id": str(m["id"]), "chat_id": str(m["chat_id"])}
		}},
	{purpose: "answers, reasoning and the SQL nao ran", kind: "nao_message_part", table: "message_part", cursor: "created_at",
		stmt: `SELECT mp.* FROM message_part mp JOIN chat_message cm ON cm.id = mp.message_id
  WHERE cm.chat_id IN ({ids}) AND mp.created_at > ? ORDER BY mp.created_at`,
		keys: func(m map[string]any) map[string]string {
			return map[string]string{"part_id": str(m["id"]), "message_id": str(m["message_id"])}
		}},
	{purpose: "token usage (cost)", kind: "nao_llm_inference", table: "llm_inference", cursor: "created_at",
		stmt: `SELECT * FROM llm_inference WHERE chat_id IN ({ids}) AND created_at > ? ORDER BY created_at`,
		keys: func(m map[string]any) map[string]string {
			return map[string]string{"inference_id": str(m["id"]), "chat_id": str(m["chat_id"])}
		}, optional: true},
	{purpose: "thumbs up and down", kind: "nao_feedback", table: "message_feedback", cursor: "updated_at",
		stmt: `SELECT mf.* FROM message_feedback mf JOIN chat_message cm ON cm.id = mf.message_id
  WHERE cm.chat_id IN ({ids}) AND mf.updated_at > ? ORDER BY mf.updated_at`,
		keys:     func(m map[string]any) map[string]string { return map[string]string{"message_id": str(m["message_id"])} },
		optional: true},
	{purpose: "questions asked over MCP (the most recent in the window)", kind: "nao_mcp_call", table: "mcp_call_log", cursor: "called_at",
		stmt:     `SELECT * FROM mcp_call_log WHERE project_id = ? AND called_at > ? ORDER BY called_at DESC LIMIT ?`,
		keys:     func(m map[string]any) map[string]string { return map[string]string{"mcp_call_id": str(m["id"])} },
		scope:    window,
		optional: true},
	// Only id, name and email: nao's user table also holds users' Git tokens.
	{purpose: "user directory (names, emails)", kind: "directory_user", table: "user",
		stmt:     `SELECT id, name, email, email_verified FROM "user" ORDER BY id`,
		keys:     func(m map[string]any) map[string]string { return map[string]string{"user_id": str(m["id"])} },
		scope:    users,
		optional: true},
}

// planned is the statement as the plan and the manifest show it: a
// children statement's id list is "<up to 200 chat ids>".
func (a arm) planned() string {
	return strings.Replace(a.stmt, idsToken, fmt.Sprintf("<up to %d chat ids>", agent.BatchSize), 1)
}

// sql is the statement for a batch of n ids.
func (a arm) sql(n int) string {
	return strings.Replace(a.stmt, idsToken, placeholders(n), 1)
}

func placeholders(n int) string {
	if n < 1 {
		n = 1
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

const projectsStmt = `SELECT id, name, type, path FROM project ORDER BY name`

func plan(in agent.Inputs, opts agent.Options) []bundle.Query {
	out := []bundle.Query{{Purpose: "list projects", Statement: projectsStmt}}
	if opts.MaxConversations > 0 {
		out = append(out, bundle.Query{Purpose: countPurpose, Statement: chatCountStmt})
	}
	out = append(out, bundle.Query{Purpose: chatsPurpose, Statement: chatsStmt})
	for _, a := range arms {
		if a.scope == users && !opts.IncludeUsers {
			continue
		}
		out = append(out, bundle.Query{Purpose: a.purpose, Statement: a.planned()})
	}
	return out
}

// limit renders a LIMIT argument: n, or no limit when n is 0 (Postgres
// reads LIMIT NULL as none, sqlite a negative limit).
func (d *db) limit(n int) any {
	switch {
	case n > 0:
		return n
	case d.postgres:
		return nil
	}
	return -1
}

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	d, err := open(in)
	if err != nil {
		return agent.Result{}, err
	}
	defer d.Close()

	res := agent.Result{Sampled: true}
	if d.note != "" {
		res.Notes = append(res.Notes, d.note)
	}
	projects, err := d.rows(ctx, projectsStmt)
	if err != nil {
		return res, fmt.Errorf("list nao projects (is this nao's database?): %w", err)
	}
	res.Queries = append(res.Queries, bundle.Query{Purpose: "list projects", Statement: projectsStmt, Rows: len(projects)})
	projectID, err := pickProject(projects, in.Get("project"))
	if err != nil {
		return res, err
	}

	var sinceMS int64
	if !opts.Since.IsZero() {
		sinceMS = opts.Since.UnixMilli()
	}
	since := d.bound(sinceMS)
	record := func(a arm, row json.RawMessage) error {
		m, err := decode(row)
		if err != nil {
			return nil
		}
		switch a.kind {
		case "nao_message_part":
			if r, ok := redactPart(row, m, opts); ok {
				row = r
			}
		case "nao_mcp_call":
			if r, ok := redactMCPCall(row, m, opts); ok {
				row = r
			}
		}
		rec := bundle.Record{SourceKind: sourceKind, RecordKind: a.kind, NativeIDs: a.keys(m),
			FetchMethod: "sql:" + a.table, Payload: row}
		if a.cursor != "" {
			rec.CursorValue = str(m[a.cursor])
		}
		return emit(rec)
	}

	if opts.MaxConversations > 0 {
		if n, err := countChats(ctx, d, projectID, since); err == nil {
			res.InWindow = &n
			res.Queries = append(res.Queries, bundle.Query{Purpose: countPurpose, Statement: chatCountStmt, Rows: 1})
		}
	}

	// The conversations first: only their ids are kept, to fetch children,
	// spilling to a temp file when there are many (no cap).
	var ids agent.IDSpool
	defer ids.Close()
	chatArm := arm{kind: chatKind, table: "chat", cursor: "created_at", keys: chatKeys}
	err = d.each(ctx, chatsStmt, []any{projectID, since, d.limit(opts.MaxConversations)}, func(row json.RawMessage) error {
		var c struct {
			ID any `json:"id"`
		}
		if pgread.Decode(row, &c) == nil && str(c.ID) != "" {
			if err := ids.Add(str(c.ID)); err != nil {
				return err
			}
		}
		return record(chatArm, row)
	})
	if err != nil {
		return res, fmt.Errorf("nao chat: %w", err)
	}
	total := ids.Len()
	res.Conversations = total
	res.Queries = append(res.Queries, bundle.Query{Purpose: chatsPurpose, Statement: chatsStmt, Rows: total})
	if res.InWindow != nil && *res.InWindow > int64(total) {
		res.Notes = append(res.Notes, fmt.Sprintf("%s chats in the window: the %s most recent were read",
			agent.Thousands(*res.InWindow), agent.Thousands(int64(total))))
	}

arms:
	for _, a := range arms {
		switch a.scope {
		case users:
			if !opts.IncludeUsers {
				continue
			}
			n := 0
			err := d.each(ctx, a.stmt, nil, func(row json.RawMessage) error { n++; return record(a, row) })
			if err != nil {
				if ctx.Err() != nil || !a.optional {
					return res, fmt.Errorf("nao %s: %w", a.table, err)
				}
				res.Notes = append(res.Notes, fmt.Sprintf("skipped %s: %v", a.table, err))
				continue
			}
			res.Queries = append(res.Queries, bundle.Query{Purpose: a.purpose, Statement: a.stmt, Rows: n})
		case window:
			limit := mcpPerChat * opts.MaxConversations
			n := 0
			err := d.each(ctx, a.stmt, []any{projectID, since, d.limit(limit)}, func(row json.RawMessage) error { n++; return record(a, row) })
			if err != nil {
				if ctx.Err() != nil || !a.optional {
					return res, fmt.Errorf("nao %s: %w", a.table, err)
				}
				res.Notes = append(res.Notes, fmt.Sprintf("skipped %s: %v", a.table, err))
				continue
			}
			res.Queries = append(res.Queries, bundle.Query{Purpose: a.purpose, Statement: a.stmt, Rows: n})
			if limit > 0 && n == limit {
				res.Notes = append(res.Notes, fmt.Sprintf("MCP calls are not tied to chats: the %s most recent in the window were read (%d per conversation in the cap)",
					agent.Thousands(int64(limit)), mcpPerChat))
			}
		case children:
			if total == 0 {
				res.Queries = append(res.Queries, bundle.Query{Purpose: a.purpose, Statement: a.planned()})
				continue
			}
			done := 0
			err := ids.Batches(agent.BatchSize, func(batch []string) error {
				args := make([]any, 0, len(batch)+1)
				for _, id := range batch {
					args = append(args, id)
				}
				args = append(args, since)
				n := 0
				if err := d.each(ctx, a.sql(len(batch)), args, func(row json.RawMessage) error { n++; return record(a, row) }); err != nil {
					return &armError{err}
				}
				res.Tally(a.purpose, a.planned(), n)
				done += len(batch)
				return nil
			})
			if err != nil {
				var ae *armError
				if !errors.As(err, &ae) || ctx.Err() != nil || !a.optional {
					return res, fmt.Errorf("nao %s: %w", a.table, err)
				}
				if done == 0 {
					res.Notes = append(res.Notes, fmt.Sprintf("skipped %s: %v", a.table, ae.err))
				} else {
					res.Notes = append(res.Notes, fmt.Sprintf("%s read for %d of %d chats only: %v", a.table, done, total, ae.err))
				}
				continue arms
			}
		}
	}
	if !opts.IncludeResults {
		res.Redactions = append(res.Redactions, resultsRedaction)
	}
	account := "sqlite" // a local file path says nothing about the agent and may name the user
	if isPG(in) {
		account = in.Get("host") + "/" + in.Get("database")
	}
	res.Sources = []bundle.Source{{SourceKind: sourceKind, NativeID: projectID, Account: account}}
	return res, nil
}

// armError is a failed statement of an arm, as opposed to a failure to
// write a record or to read the spooled ids.
type armError struct{ err error }

func (e *armError) Error() string { return e.err.Error() }
func (e *armError) Unwrap() error { return e.err }

// countChats counts the chats in the window.
func countChats(ctx context.Context, d *db, projectID string, since any) (int64, error) {
	rows, err := d.rows(ctx, chatCountStmt, projectID, since)
	if err != nil || len(rows) != 1 {
		return 0, fmt.Errorf("count chats: %v", err)
	}
	var v struct {
		N json.Number `json:"n"`
	}
	if err := pgread.Decode(rows[0], &v); err != nil {
		return 0, err
	}
	return v.N.Int64()
}

// pickProject chooses the project to read: the only one, or the one named.
func pickProject(rows []json.RawMessage, want string) (string, error) {
	type project struct{ ID, Name, Type, Path string }
	var ps []project
	for _, r := range rows {
		var p struct {
			ID   any    `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
			Path string `json:"path"`
		}
		if json.Unmarshal(r, &p) == nil && str(p.ID) != "" {
			ps = append(ps, project{str(p.ID), p.Name, p.Type, p.Path})
		}
	}
	if want != "" {
		for _, p := range ps {
			if p.ID == want || strings.EqualFold(p.Name, want) {
				return p.ID, nil
			}
		}
		return "", fmt.Errorf("no nao project %q in this database", want)
	}
	switch len(ps) {
	case 0:
		return "", fmt.Errorf("this nao database has no projects")
	case 1:
		return ps[0].ID, nil
	}
	var names []string
	for _, p := range ps {
		names = append(names, fmt.Sprintf("%s (%s)", p.Name, p.ID))
	}
	return "", fmt.Errorf("this nao database has %d projects; choose one with --project: %s",
		len(ps), strings.Join(names, ", "))
}

// Projects lists the projects in a nao database, for the interactive picker.
func Projects(ctx context.Context, in agent.Inputs) ([][2]string, error) {
	d, err := open(in)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	rows, err := d.rows(ctx, projectsStmt)
	if err != nil {
		return nil, err
	}
	var out [][2]string
	for _, r := range rows {
		var p struct {
			ID   any    `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(r, &p) == nil {
			out = append(out, [2]string{str(p.ID), p.Name})
		}
	}
	return out, nil
}

// decode reads a row keeping numbers exact, so re-encoding a redacted row
// cannot alter an id above 2^53.
func decode(row json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(row))
	dec.UseNumber()
	var m map[string]any
	err := dec.Decode(&m)
	return m, err
}

// str renders a JSON scalar as a string. %.0f keeps a millisecond epoch
// from turning into 1.757e+12.
func str(v any) string {
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
