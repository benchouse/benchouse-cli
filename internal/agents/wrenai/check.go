package wrenai

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/files"
	"github.com/benchouse/benchouse-cli/internal/pgread"
)

// Check statements: the version, each Collect statement under
// pgread.LimitZero, and this count of responses in the window.
const (
	sqliteVersionStmt = `SELECT sqlite_version() AS version`
	countStmt         = `SELECT count(*) AS n, count(DISTINCT thread_id) AS threads FROM thread_response WHERE created_at >= ?`
)

// Rows lets the Postgres check helpers run statements on d.
func (d *db) Rows(ctx context.Context, stmt string, args ...any) ([]json.RawMessage, error) {
	return d.rows(ctx, stmt, args...)
}

// Each lets the Postgres helpers stream statements on d.
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
	return fmt.Sprintf("Opened wren-ui's sqlite database %s (SQLite %s)", filepath.Base(in.Get("sqlite-path")), v.Version), nil
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
	grant := ""
	if d.postgres {
		grant = fmt.Sprintf("; GRANT SELECT ON thread, thread_response, asking_task TO %s", in.Get("user"))
	}

	since := d.bound(sinceOf(opts))
	if _, err := d.rows(ctx, pgread.LimitZero(threadsStmt), since, agent.BatchSize); err != nil {
		return res, fmt.Errorf("thread_response is not readable (is this wren-ui's database, from Dec 2024 or later?%s): %s", grant, agent.Clip(err))
	}
	if _, err := d.rows(ctx, pgread.LimitZero(batched(turnsStmt, 1)), 0, since); err != nil {
		return res, fmt.Errorf("thread_response is not readable (is this wren-ui's database, from Dec 2024 or later?%s): %s", grant, agent.Clip(err))
	}
	if _, err := d.rows(ctx, pgread.LimitZero(batched(askStmt, 1)), 0, since); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("asking_task not readable, so turns will have no reasoning or ask status "+
			"(Wren before May 2025 lacks it%s): %s", grant, agent.Clip(err)))
	}
	rows, err := d.rows(ctx, countStmt, since)
	if err == nil && len(rows) == 1 {
		var v struct {
			N       json.Number `json:"n"`
			Threads json.Number `json:"threads"`
		}
		if pgread.Decode(rows[0], &v) == nil {
			n, _ := v.N.Int64()
			threads, _ := v.Threads.Int64()
			res.Facts = append(res.Facts, fmt.Sprintf("%s questions in %s", agent.Thousands(n), agent.Conversations(threads, "thread", "threads", opts)))
			res.Conversations = &threads
			if n == 0 {
				res.Warnings = append(res.Warnings, "no questions in the window: widen --since, or check this is the live database (a copy may be old)")
			}
		}
	}
	return res, nil
}

// maxMDLCheck bounds a JSON file read for the context check.
const maxMDLCheck = 32 << 20

// checkContext verifies dir holds a Wren MDL: JSON with models (and usually
// relationships and views), as wren-ui deploys it, or a list of models.
func checkContext(dir string) (agent.CheckResult, error) {
	var res agent.CheckResult
	sum, err := files.Scan(dir, keepContext)
	if err != nil {
		return res, err
	}
	if err := sum.Limits(dir); err != nil {
		return res, err
	}
	var mdlFiles, models, relationships, views, otherJSON int
	for _, rel := range sum.Kept {
		if strings.ToLower(path.Ext(rel)) != ".json" {
			continue
		}
		b, err := files.ReadBounded(filepath.Join(dir, filepath.FromSlash(rel)), maxMDLCheck)
		if err != nil {
			otherJSON++
			continue
		}
		m, r, v, ok := mdlCounts(b)
		if !ok {
			otherJSON++
			continue
		}
		mdlFiles++
		models, relationships, views = models+m, relationships+r, views+v
	}
	if mdlFiles == 0 {
		msg := "no Wren MDL found in " + dir + ": expected a JSON file with \"models\" (and \"relationships\"), " +
			"e.g. saved with curl http://localhost:3000/api/v1/models > mdl/models.json"
		if otherJSON > 0 {
			msg += fmt.Sprintf(" (%d JSON files there are not MDL)", otherJSON)
		}
		return res, fmt.Errorf("%s", msg)
	}
	res.Facts = append(res.Facts, fmt.Sprintf("%d MDL files: %s models, %s relationships, %s views", mdlFiles,
		agent.Thousands(int64(models)), agent.Thousands(int64(relationships)), agent.Thousands(int64(views))))
	if models > 0 && relationships == 0 {
		res.Warnings = append(res.Warnings, "the MDL has no relationships: joins the agent may need are undefined")
	}
	if otherJSON > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d JSON files are not MDL; they are still included", otherJSON))
	}
	res.Facts = append(res.Facts, fmt.Sprintf("%s files (%s) will be included", agent.Thousands(int64(len(sum.Kept))), agent.Bytes(sum.KeptBytes)))
	return res, nil
}

// mdlCounts reads an MDL document: an object with a models array, or a bare
// array of models (objects with a name and columns).
func mdlCounts(b []byte) (models, relationships, views int, ok bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) == nil {
		var ms, rs, vs []json.RawMessage
		if json.Unmarshal(obj["models"], &ms) != nil || obj["models"] == nil {
			return 0, 0, 0, false
		}
		_ = json.Unmarshal(obj["relationships"], &rs)
		_ = json.Unmarshal(obj["views"], &vs)
		return len(ms), len(rs), len(vs), true
	}
	var arr []struct {
		Name    string            `json:"name"`
		Columns []json.RawMessage `json:"columns"`
		Fields  []json.RawMessage `json:"fields"`
	}
	if json.Unmarshal(b, &arr) != nil || len(arr) == 0 {
		return 0, 0, 0, false
	}
	for _, m := range arr {
		if m.Name == "" || (m.Columns == nil && m.Fields == nil) {
			return 0, 0, 0, false
		}
	}
	return len(arr), 0, 0, true
}
