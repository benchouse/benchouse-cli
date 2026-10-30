// Package wrenai reads conversations from Wren AI's GenBI app
// (github.com/Canner/WrenAI, the wren-ui service).
//
// wren-ui keeps every question in its own application database: a sqlite
// file in the wren-ui data volume by default, or Postgres. One
// thread_response row is one turn: the question, the SQL Wren generated, and
// in answer_detail (JSON) the text answer it streamed and its status. Since
// May 2025 the asking_task row the response came from holds the AI service's
// result (status, type, error, rephrased question and SQL-generation
// reasoning).
//
// Written from the knex migrations in wren-ui/migrations at tag v1-final
// (commit e42b8d0, May 2026): the GenBI app was archived to the legacy/v1
// branch when WrenAI's main branch became the Wren CLI, so that schema is
// final. thread_response needs the answer_detail column (migration
// 20241207000000); asking_task and adjustment (20250509-20250510) are read
// when present. Turns are emitted in the turns/v1 form.
//
// wren-ui does not store query result rows with a response (the answer is
// re-fetched on demand), so nothing here carries business data; chart_detail
// is not read because a chart spec can embed the rows it plots.
package wrenai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/netsafe"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

func init() {
	agent.Register(agent.Agent{
		ID:      "wren-ai",
		Name:    "Wren AI",
		Summary: "open-source text-to-SQL (GenBI app); reads wren-ui's sqlite or Postgres database",
		Status:  agent.Experimental,
		Setup: `Wren AI's UI service (wren-ui) stores threads in its own database: by
default the sqlite file /app/data/db.sqlite3 in the wren-ui "data" volume,
or Postgres when wren-ui runs with DB_TYPE=pg and PG_URL.

For sqlite, copy the file out of the container and point at the copy:

  docker cp $(docker ps -qf name=wren-ui):/app/data/db.sqlite3 ./wren-db.sqlite3

On Postgres, a read-only user is enough:

  CREATE USER benchouse_reader PASSWORD '...';
  GRANT SELECT ON thread, thread_response, asking_task TO benchouse_reader;`,
		Fields: []agent.Field{
			{Key: "storage", Label: "Where does wren-ui store its data?", Choices: []string{"sqlite", "postgres"}, Default: "sqlite",
				Help: "sqlite for the default docker-compose install; postgres if wren-ui runs with DB_TYPE=pg"},
			{Key: "host", Label: "Postgres host", When: isPG, Default: "localhost", Validate: agent.Host},
			{Key: "port", Label: "Postgres port", When: isPG, Default: "5432", Validate: agent.Port},
			{Key: "database", Label: "Database name", When: isPG},
			{Key: "user", Label: "Username", When: isPG},
			// TLS before the password, so the password's check connects as
			// collecting will.
			{Key: "sslmode", Label: "SSL mode", When: isPG, Default: netsafe.Auto, Choices: netsafe.PGSSLModes, Help: netsafe.PGSSLHelp},
			{Key: "password", Label: "Password", When: isPG, Secret: true, Optional: true, Verify: verifyConn},
			{Key: "sqlite-path", Label: "Path to wren-ui's db.sqlite3", When: isSQLite, Validate: agent.ExistingFile, Verify: verifyConn,
				Help: "a copy of /app/data/db.sqlite3 from the wren-ui container"},
		},
		Context: &agent.Context{
			Label: "Wren MDL folder",
			Help: "a folder with Wren's deployed MDL (models, relationships, views), e.g. saved with\n" +
				"curl http://localhost:3000/api/v1/models > mdl/models.json",
			Check: checkContext,
			Keep:  keepContext,
		},
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
}

func isPG(in agent.Inputs) bool     { return in.Get("storage") == "postgres" }
func isSQLite(in agent.Inputs) bool { return in.Get("storage") != "postgres" }

// keepContext keeps an exported MDL (wren-ui's GET /api/v1/models answers
// JSON) and any YAML or Markdown notes beside it.
func keepContext(rel string) bool {
	switch strings.ToLower(path.Ext(rel)) {
	case ".json", ".yaml", ".yml", ".md":
		return true
	}
	return false
}

const fetchMethod = "sql:thread_response"

// The conversation is the thread. Threads are chosen first, the most
// recently active in the window (by their newest response), a page of
// agent.BatchSize at a time with keyset pagination on (last_at, thread_id),
// until --max-conversations threads are read; each page's responses are
// fetched right away with the page's thread ids as bound parameters. So
// nothing but one page is ever held, with or without a cap.

// threadsStmt is the first page of threads; threadsNextStmt every later one,
// after the last (last_at, thread_id) of the previous page.
const (
	threadsStmt = `SELECT thread_id, last_at FROM (
  SELECT thread_id, max(created_at) AS last_at FROM thread_response WHERE created_at >= ? GROUP BY thread_id
) t ORDER BY last_at DESC, thread_id DESC LIMIT ?`
	threadsNextStmt = `SELECT thread_id, last_at FROM (
  SELECT thread_id, max(created_at) AS last_at FROM thread_response WHERE created_at >= ? GROUP BY thread_id
) t WHERE last_at < ? OR (last_at = ? AND thread_id < ?) ORDER BY last_at DESC, thread_id DESC LIMIT ?`
	// threadCountStmt counts the threads the window holds, for the sample.
	threadCountStmt = `SELECT count(DISTINCT thread_id) AS n FROM thread_response WHERE created_at >= ?`
)

// idsToken marks where a batched statement's thread id list goes.
const idsToken = "{ids}"

// turnsStmt reads one row per thread_response of a batch of threads.
// position counts the responses before it in the thread, so it stays right
// when Since cuts the thread's start off.
const turnsStmt = `SELECT tr.id, tr.thread_id, tr.question, tr.sql, tr.answer_detail, tr.created_at, t.project_id,
  (SELECT count(*) FROM thread_response p WHERE p.thread_id = tr.thread_id AND p.id <= tr.id) AS position
  FROM thread_response tr JOIN thread t ON t.id = tr.thread_id
  WHERE tr.thread_id IN ({ids}) AND tr.created_at >= ? ORDER BY tr.created_at, tr.id`

// askStmt reads the AI service's result for each response of a batch of
// threads. Optional: the asking_task table and the adjustment column arrived
// in May 2025.
const askStmt = `SELECT tr.id AS thread_response_id, tr.adjustment, atk.detail AS ask_detail
  FROM thread_response tr LEFT JOIN asking_task atk ON atk.id = tr.asking_task_id
  WHERE tr.thread_id IN ({ids}) AND tr.created_at >= ?`

const (
	threadsPurpose     = "conversations (the most recently active threads in the window)"
	threadsNextPurpose = "conversations, next page (keyset on last activity, thread id)"
	countPurpose       = "conversations in the window (count)"
	turnsPurpose       = "questions, generated SQL and answers (thread_response)"
	askPurpose         = "AI service results: status, errors, reasoning (asking_task)"
)

// planned is a batched statement as the plan and the manifest show it.
func planned(stmt string) string {
	return strings.Replace(stmt, idsToken, fmt.Sprintf("<up to %d thread ids>", agent.BatchSize), 1)
}

// batched is a batched statement for n ids.
func batched(stmt string, n int) string {
	if n < 1 {
		n = 1
	}
	return strings.Replace(stmt, idsToken, strings.TrimSuffix(strings.Repeat("?, ", n), ", "), 1)
}

var columns = turns.NewColumns(
	turns.ColOutcome, turns.ColErrorType, turns.ColErrorMessage, turns.ColConversationID,
	turns.ColPosition, turns.ColClientInterface, turns.ColStatements, turns.ColReasoning, turns.ColExtra,
)

func plan(in agent.Inputs, opts agent.Options) []bundle.Query {
	var out []bundle.Query
	if opts.MaxConversations > 0 {
		out = append(out, bundle.Query{Purpose: countPurpose, Statement: threadCountStmt})
	}
	return append(out,
		bundle.Query{Purpose: threadsPurpose, Statement: threadsStmt},
		bundle.Query{Purpose: askPurpose, Statement: planned(askStmt)},
		bundle.Query{Purpose: turnsPurpose, Statement: planned(turnsStmt)},
		bundle.Query{Purpose: threadsNextPurpose, Statement: threadsNextStmt},
	)
}

// sinceOf is the window's lower bound: Since, or the epoch.
func sinceOf(opts agent.Options) time.Time {
	if opts.Since.IsZero() {
		return time.Unix(0, 0)
	}
	return opts.Since
}

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	d, err := open(in)
	if err != nil {
		return agent.Result{}, err
	}
	defer d.Close()

	since := d.bound(sinceOf(opts))
	res := agent.Result{Sampled: true}
	if d.note != "" {
		res.Notes = append(res.Notes, d.note)
	}
	if opts.MaxConversations > 0 {
		if rows, err := d.rows(ctx, threadCountStmt, since); err == nil && len(rows) == 1 {
			var v struct {
				N json.Number `json:"n"`
			}
			if decodeInto(rows[0], &v) == nil {
				if n, err := v.N.Int64(); err == nil {
					res.InWindow = &n
					res.Queries = append(res.Queries, bundle.Query{Purpose: countPurpose, Statement: threadCountStmt, Rows: 1})
				}
			}
		}
	}

	askOK := true
	var lastAt, lastID any
	for first := true; ; first = false {
		size := agent.BatchSize
		if opts.MaxConversations > 0 {
			size = min(size, opts.MaxConversations-res.Conversations)
		}
		if size <= 0 {
			break
		}
		stmt, purpose, args := threadsStmt, threadsPurpose, []any{since, size}
		if !first {
			stmt, purpose, args = threadsNextStmt, threadsNextPurpose, []any{since, lastAt, lastAt, lastID, size}
		}
		var ids []any
		err := d.each(ctx, stmt, args, func(row json.RawMessage) error {
			m, err := decode(row)
			if err != nil {
				return err
			}
			lastAt, lastID = arg(m["last_at"]), arg(m["thread_id"])
			ids = append(ids, lastID)
			return nil
		})
		if err != nil {
			return res, fmt.Errorf("read wren-ui threads (is this wren-ui's database, from Dec 2024 or later?): %w", err)
		}
		res.Tally(purpose, stmt, len(ids))
		if len(ids) == 0 {
			break
		}
		res.Conversations += len(ids)
		args = append(append(make([]any, 0, len(ids)+1), ids...), since)

		// The batch's asking_task results, then its responses, joined.
		asks := map[string]map[string]any{}
		if askOK {
			n := 0
			err := d.each(ctx, batched(askStmt, len(ids)), args, func(row json.RawMessage) error {
				n++
				if m, err := decode(row); err == nil {
					asks[str(m["thread_response_id"])] = m
				}
				return nil
			})
			if err != nil {
				if ctx.Err() != nil {
					return res, err
				}
				askOK = false
				res.Notes = append(res.Notes, fmt.Sprintf("skipped asking_task (Wren before May 2025 lacks it; turns have no reasoning or ask status): %v", err))
			} else {
				res.Tally(askPurpose, planned(askStmt), n)
			}
		}
		n := 0
		err = d.each(ctx, batched(turnsStmt, len(ids)), args, func(row json.RawMessage) error {
			n++
			m, err := decode(row)
			if err != nil {
				return nil
			}
			t, err := toTurn(m, asks[str(m["id"])])
			if err != nil {
				res.Notes = append(res.Notes, "skipped a thread_response: "+err.Error())
				return nil
			}
			rec, err := turns.Record(t, columns, fetchMethod)
			if err != nil {
				res.Notes = append(res.Notes, "skipped a thread_response: "+err.Error())
				return nil
			}
			return emit(rec)
		})
		if err != nil {
			return res, fmt.Errorf("read wren-ui thread responses: %w", err)
		}
		res.Tally(turnsPurpose, planned(turnsStmt), n)
		if len(ids) < size {
			break
		}
	}
	if res.InWindow != nil && *res.InWindow > int64(res.Conversations) {
		res.Notes = append(res.Notes, fmt.Sprintf("%s threads in the window: the %s most recent were read",
			agent.Thousands(*res.InWindow), agent.Thousands(int64(res.Conversations))))
	}
	return res, nil
}

// arg turns a decoded column back into a bound parameter of the same type:
// an integer stays an integer, so sqlite compares it as one.
func arg(v any) any {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return f
		}
	}
	return v
}

func decodeInto(row json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(row))
	dec.UseNumber()
	return dec.Decode(v)
}

// dataErrors are error codes raised while running the generated SQL, as
// opposed to while generating the text answer from its rows
// (wren-ui/src/apollo/server/utils/error.ts).
var dataErrors = map[string]bool{
	"IBIS_SERVER_ERROR": true, "WREN_ENGINE_ERROR": true, "SQL_EXECUTION_ERROR": true,
	"CONNECTION_ERROR": true, "CONNECTION_REFUSED": true, "DRY_RUN_ERROR": true,
	"DRY_PLAN_ERROR": true, "INVALID_SQL_ERROR": true,
}

// declinedErrors are the AI service's own "I cannot answer this from the
// data" results; MISLEADING_QUERY as the ask type means the same.
var declinedErrors = map[string]bool{"NO_RELEVANT_DATA": true, "NO_RELEVANT_SQL": true}

// toTurn maps a thread_response row, and its asking_task result if any, onto
// a turn.
func toTurn(m, ask map[string]any) (turns.Turn, error) {
	asked, err := parseTime(m["created_at"])
	if err != nil {
		return turns.Turn{}, fmt.Errorf("thread_response %s: created_at: %w", str(m["id"]), err)
	}
	t := turns.Turn{
		TurnID:          str(m["id"]),
		AskedAt:         asked,
		Question:        str(m["question"]),
		ConversationID:  str(m["thread_id"]),
		ClientInterface: "web",
		Statements:      []turns.Statement{},
		Reasoning:       []turns.Reasoning{},
		Extra:           map[string]any{},
	}
	if p, ok := num(m["position"]); ok {
		t.Position = &p
	}
	if p := str(m["project_id"]); p != "" {
		t.Extra["project_id"] = p
	}

	answer := object(m["answer_detail"])
	answerStatus := str(answer["status"])
	if c := str(answer["content"]); strings.TrimSpace(c) != "" {
		t.Answer = &c
	}
	if n, ok := num(answer["numRowsUsedInLLM"]); ok {
		t.Extra["num_rows_used_in_llm"] = n
	}
	answerErrCode, answerErrMsg := errorOf(answer["error"])

	var detail map[string]any
	if ask != nil {
		detail = object(ask["ask_detail"])
		if adj := object(ask["adjustment"]); adj != nil {
			t.Extra["adjustment_type"] = str(adj["type"])
		}
	}
	askStatus := str(detail["status"])
	askType := str(detail["type"])
	askErrCode, askErrMsg := errorOf(detail["error"])

	// The SQL Wren answered with. It runs when the text answer is generated.
	if sql := str(m["sql"]); strings.TrimSpace(sql) != "" {
		st := turns.Statement{SQL: sql, IsAnswer: turns.Ptr(true)}
		switch answerStatus {
		case "", "NOT_STARTED":
			st.Status = "not_executed"
		case "FAILED":
			if dataErrors[answerErrCode] {
				st.Status, st.Error = "failed", answerErrMsg
			}
		case "FETCHING_DATA":
			// still running
		default: // PREPROCESSING, STREAMING, FINISHED, INTERRUPTED: rows were fetched
			st.Status = "succeeded"
		}
		t.Statements = append(t.Statements, st)
	}
	// SQL the AI service generated and then rejected as invalid.
	if bad := str(detail["invalidSql"]); strings.TrimSpace(bad) != "" {
		t.Statements = append(t.Statements, turns.Statement{SQL: bad, Status: "failed",
			Error: askErrMsg, IsAnswer: turns.Ptr(false)})
	}

	if s := str(detail["rephrasedQuestion"]); s != "" {
		t.Reasoning = append(t.Reasoning, turns.Reasoning{Text: s, Label: "restatement"})
	}
	if s := str(detail["intentReasoning"]); s != "" {
		t.Reasoning = append(t.Reasoning, turns.Reasoning{Text: s, Label: "intent"})
	}
	if tables, ok := detail["retrievedTables"].([]any); ok && len(tables) > 0 {
		names := make([]string, 0, len(tables))
		for _, x := range tables {
			names = append(names, str(x))
		}
		t.Reasoning = append(t.Reasoning, turns.Reasoning{Text: "Retrieved tables: " + strings.Join(names, ", "), Label: "data_sourcing"})
	}
	if s := str(detail["sqlGenerationReasoning"]); s != "" {
		t.Reasoning = append(t.Reasoning, turns.Reasoning{Text: s, Label: "plan"})
	}
	if askType != "" {
		t.Extra["ask_type"] = askType
	}
	if cands, ok := detail["response"].([]any); ok && len(cands) > 0 {
		if c := object(cands[0]); c != nil && str(c["type"]) != "" {
			t.Extra["candidate_type"] = str(c["type"]) // LLM, VIEW or SQL_PAIR
		}
	}
	if askType == "MISLEADING_QUERY" || declinedErrors[askErrCode] {
		t.Extra["declined"] = true
	}

	switch {
	case answerStatus == "FINISHED":
		t.Outcome = "completed"
	case answerStatus == "FAILED":
		t.Outcome, t.ErrorType, t.ErrorMessage = "failed", answerErrCode, answerErrMsg
	case answerStatus == "INTERRUPTED":
		t.Outcome = "interrupted"
	case answerStatus != "" && answerStatus != "NOT_STARTED":
		t.Outcome = "running"
	case askStatus == "FAILED":
		t.Outcome, t.ErrorType, t.ErrorMessage = "failed", askErrCode, askErrMsg
	case askStatus == "STOPPED":
		t.Outcome = "stopped"
	}
	if t.ErrorType == "" && askErrCode != "" {
		t.ErrorType, t.ErrorMessage = askErrCode, askErrMsg
	}
	return t, nil
}

// errorOf reads a Wren error object, {code, message} from the AI service or a
// GraphQL error's extensions.
func errorOf(v any) (code, message string) {
	e := object(v)
	if e == nil {
		if s := str(v); s != "" && s != "null" {
			return "", s
		}
		return "", ""
	}
	return str(e["code"]), str(e["message"])
}

// object reads a JSON column: an object on Postgres, a string holding JSON
// on sqlite. Anything else is nil.
func object(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(t))
		dec.UseNumber()
		if dec.Decode(&m) == nil {
			return m
		}
	}
	return nil
}

// parseTime reads created_at as either dialect renders it.
func parseTime(v any) (time.Time, error) {
	if n, ok := v.(json.Number); ok {
		ms, err := n.Int64()
		if err != nil {
			return time.Time{}, err
		}
		return time.UnixMilli(ms).UTC(), nil
	}
	s := str(v)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
}

func num(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case float64:
		return int64(t), true
	}
	return 0, false
}

// decode reads a row keeping numbers exact.
func decode(row json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(row))
	dec.UseNumber()
	var m map[string]any
	err := dec.Decode(&m)
	return m, err
}

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
