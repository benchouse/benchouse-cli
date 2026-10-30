package nao

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

func runCheck(t *testing.T, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	t.Helper()
	a, _ := agent.Get("nao")
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	return a.Check(context.Background(), in, opts)
}

func TestCheckSQLite(t *testing.T) {
	res, err := runCheck(t, agent.Inputs{"storage": "sqlite", "sqlite-path": fixture(t, 1)},
		agent.Options{Since: time.UnixMilli(1000).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Join(res.Facts, "\n")
	for _, want := range []string{"Opened nao's sqlite database db.sqlite (SQLite 3.", "nao project Shop", "1 chat since 1970-01-01"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	warns := strings.Join(res.Warnings, "\n")
	if len(res.Warnings) != 2 || !strings.Contains(warns, "message_feedback not readable") || !strings.Contains(warns, "mcp_call_log") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	if strings.Contains(warns, "GRANT") {
		t.Errorf("a grant suggested for a sqlite file: %s", warns)
	}
}

func TestCheckNamesTheProjectToChoose(t *testing.T) {
	p := fixture(t, 2)
	_, err := runCheck(t, agent.Inputs{"storage": "sqlite", "sqlite-path": p}, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "choose one with --project") {
		t.Errorf("err = %v", err)
	}
	res, err := runCheck(t, agent.Inputs{"storage": "sqlite", "sqlite-path": p, "project": "finance"}, agent.Options{})
	if err != nil || !strings.Contains(strings.Join(res.Facts, "\n"), "2 nao projects (Finance, Shop); reading Finance") {
		t.Errorf("facts = %v, err = %v", res.Facts, err)
	}
}

func TestCheckFailures(t *testing.T) {
	p := fixture(t, 1)
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE message_part`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err = runCheck(t, agent.Inputs{"storage": "sqlite", "sqlite-path": p}, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "nao message_part is not readable") {
		t.Errorf("missing message_part: %v", err)
	}

	other := filepath.Join(t.TempDir(), "other.sqlite")
	db, _ = sql.Open("sqlite", "file:"+other)
	db.Exec(`CREATE TABLE t (x int)`)
	db.Close()
	_, err = runCheck(t, agent.Inputs{"storage": "sqlite", "sqlite-path": other}, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "is this nao's database?") {
		t.Errorf("not nao: %v", err)
	}

	// A missing file is refused by the field, before anything opens it.
	a, _ := agent.Get("nao")
	missing := agent.Inputs{"storage": "sqlite", "sqlite-path": filepath.Join(t.TempDir(), "missing.sqlite")}
	if err := a.Validate(missing); err == nil || !strings.Contains(err.Error(), "--sqlite-path: no such file") {
		t.Errorf("missing file: %v", err)
	}
}

func mkTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestContextCheck(t *testing.T) {
	root := mkTree(t, map[string]string{
		"RULES.md":        "# rules",
		"nao_config.yaml": "project_name: shop\n",
		"databases/type=postgres/database=shop/schema=public/table=orders/columns.md": "id",
		"databases/type=postgres/database=shop/schema=public/table=orders/preview.md": "rows",
		"databases/type=postgres/database=shop/schema=public/table=users/columns.md":  "id",
		"semantics/metrics.yaml": "metrics: []",
		"agent/tools.md":         "tools",
	})
	res, err := checkContext(root)
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Join(res.Facts, "\n")
	for _, want := range []string{"RULES.md and nao_config.yaml (secrets masked) present", "2 table docs under databases/",
		"1 semantic files", "1 agent docs", "1 sample-data files left out", "6 files ("} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v", res.Warnings)
	}

	res, err = checkContext(mkTree(t, map[string]string{"databases/x/preview.md": "rows", "databases/x/profiling.md": "top"}))
	if err != nil || len(res.Warnings) != 2 || !strings.Contains(strings.Join(res.Warnings, "\n"), "only sample-data files") {
		t.Errorf("samples only: %v %v", res.Warnings, err)
	}

	_, err = checkContext(mkTree(t, map[string]string{"shop/nao_config.yaml": "x: 1", "README.md": "hi"}))
	if err == nil || !strings.Contains(err.Error(), "not a nao context folder") || !strings.Contains(err.Error(), "did you mean") {
		t.Errorf("one level too high: %v", err)
	}
}
