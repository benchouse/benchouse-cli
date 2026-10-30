package cortex

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/files"
)

// Check statements: snowflake.SessionStmt; for each required view a count
// of the conversations in the window (Analyst requests, distinct Agents
// traces); for each optional view `SELECT * FROM <view> LIMIT 0`.
// ACCOUNT_USAGE views are probed, not counted: QUERY_HISTORY is large.

func (a arm) probeStatement() string { return "SELECT * FROM " + a.from + " LIMIT 0" }

// grant is the statement that makes the arm's view readable to role.
func (a arm) grant(role string) string {
	if role == "" {
		role = "<role>"
	}
	switch {
	case strings.Contains(a.from, "CORTEX_ANALYST_REQUESTS"):
		return "GRANT APPLICATION ROLE SNOWFLAKE.CORTEX_ANALYST_REQUESTS_ADMIN TO ROLE " + role
	case strings.Contains(a.from, "AI_OBSERVABILITY_EVENTS"):
		return "GRANT APPLICATION ROLE SNOWFLAKE.AI_OBSERVABILITY_EVENTS_LOOKUP TO ROLE " + role
	}
	return "GRANT IMPORTED PRIVILEGES ON DATABASE SNOWFLAKE TO ROLE " + role
}

// countNoun names what a required arm's count counts.
func (a arm) countNoun() (string, string) {
	if a.source == analystKind {
		return "Cortex Analyst request", "Cortex Analyst requests"
	}
	return "Cortex Agents request", "Cortex Agents requests"
}

func check(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	var res agent.CheckResult
	cl, err := connect(in)
	if err != nil {
		return res, err
	}
	sess, err := cl.Connected(ctx)
	if err != nil {
		return res, err
	}
	res.Facts = append(res.Facts, sess.Fact())
	if sess.Warehouse == "" {
		return res, sess.NoWarehouse()
	}

	var failed []error
	readable := map[string]bool{}
	tried := map[string]bool{}
	for _, a := range selected(in, opts) {
		if !a.required {
			if _, err := cl.Query(ctx, a.probeStatement()); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s not readable (%s), so %s will be missing: %s to include it",
					a.from, agent.Clip(err), a.purpose, a.grant(sess.Role)))
			}
			continue
		}
		tried[a.source] = true
		n, err := cl.Count(ctx, a.countStatement(opts))
		if err != nil {
			failed = append(failed, fmt.Errorf("%s not readable (%s): %s", a.from, agent.Clip(err), a.grant(sess.Role)))
			continue
		}
		readable[a.source] = true
		total := n
		if res.Conversations != nil {
			total += *res.Conversations
		}
		res.Conversations = &total
		one, many := a.countNoun()
		res.Facts = append(res.Facts, agent.Conversations(n, one, many, opts))
		if n == 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("no %s in the window: widen --since, or check the role sees the account's history", many))
		}
	}
	if len(readable) == 0 && len(failed) > 0 {
		return res, errors.Join(failed...)
	}
	for _, err := range failed {
		res.Warnings = append(res.Warnings, err.Error()+"; only the readable product will be collected")
	}
	return res, nil
}

// maxModelCheck bounds a semantic model file read for the context check.
const maxModelCheck = 4 << 20

var semanticViewDDL = regexp.MustCompile(`(?is)\bCREATE\s+(?:OR\s+REPLACE\s+)?(?:SECURE\s+)?SEMANTIC\s+VIEW\b|SYSTEM\$CREATE_SEMANTIC_VIEW_FROM_YAML`)

// checkContext verifies dir holds Cortex semantic models: YAML whose
// top-level tables: list has entries with a base_table, or semantic view
// DDL.
func checkContext(dir string) (agent.CheckResult, error) {
	var res agent.CheckResult
	sum, err := files.Scan(dir, keepContext)
	if err != nil {
		return res, err
	}
	if err := sum.Limits(dir); err != nil {
		return res, err
	}
	var models, tables, verified, views, otherYAML int
	var names []string
	for _, rel := range sum.Kept {
		ext := strings.ToLower(path.Ext(rel))
		if ext != ".yaml" && ext != ".yml" && ext != ".sql" {
			continue
		}
		b, err := files.ReadBounded(filepath.Join(dir, filepath.FromSlash(rel)), maxModelCheck)
		if err != nil {
			continue
		}
		if ext == ".sql" {
			views += len(semanticViewDDL.FindAllIndex(b, -1))
			continue
		}
		t, v, ok := semanticModel(b)
		if !ok {
			otherYAML++
			continue
		}
		models++
		tables += t
		verified += v
		names = append(names, rel)
	}
	if models == 0 && views == 0 {
		msg := "no Cortex semantic model found in " + dir + ": expected semantic model YAML (a top-level tables: list " +
			"whose entries have a base_table) or semantic view DDL (CREATE SEMANTIC VIEW ...)"
		if otherYAML > 0 {
			msg += fmt.Sprintf("; %d YAML files there are not semantic models", otherYAML)
		}
		return res, errors.New(msg)
	}
	if models > 0 {
		if len(names) > 5 {
			names = append(names[:5], "...")
		}
		res.Facts = append(res.Facts, fmt.Sprintf("%d semantic model YAML files (%s tables, %s verified queries): %s",
			models, agent.Thousands(int64(tables)), agent.Thousands(int64(verified)), strings.Join(names, ", ")))
	}
	if views > 0 {
		res.Facts = append(res.Facts, fmt.Sprintf("%d semantic view definitions in .sql files", views))
	}
	if otherYAML > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d YAML files are not semantic models; they are still included", otherYAML))
	}
	res.Facts = append(res.Facts, fmt.Sprintf("%s files (%s) will be included", agent.Thousands(int64(len(sum.Kept))), agent.Bytes(sum.KeptBytes)))
	return res, nil
}

// semanticModel reports whether b is a Cortex Analyst semantic model, with
// its table and verified-query counts.
func semanticModel(b []byte) (tables, verified int, ok bool) {
	var doc struct {
		Tables []struct {
			BaseTable any `yaml:"base_table"`
		} `yaml:"tables"`
		VerifiedQueries []any `yaml:"verified_queries"`
	}
	if yaml.Unmarshal(b, &doc) != nil {
		return 0, 0, false
	}
	for _, t := range doc.Tables {
		if t.BaseTable != nil {
			ok = true
		}
	}
	return len(doc.Tables), len(doc.VerifiedQueries), ok
}
