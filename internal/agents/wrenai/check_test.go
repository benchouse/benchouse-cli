package wrenai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

func runCheck(t *testing.T, path string, opts agent.Options) (agent.CheckResult, error) {
	t.Helper()
	a, _ := agent.Get("wren-ai")
	in := agent.Inputs{"sqlite-path": path}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	return a.Check(context.Background(), in, opts)
}

func TestCheck(t *testing.T) {
	res, err := runCheck(t, fixture(t, true), agent.Options{Since: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Join(res.Facts, "\n")
	for _, want := range []string{"Opened wren-ui's sqlite database db.sqlite3 (SQLite 3.", "1 questions in 1 thread since 2026-10-02"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v", res.Warnings)
	}

	res, err = runCheck(t, fixture(t, false), agent.Options{})
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "asking_task not readable") {
		t.Errorf("old wren: %v %v", res.Warnings, err)
	}
	if !strings.Contains(strings.Join(res.Facts, "\n"), "3 questions in 2 threads in total") {
		t.Errorf("facts = %v", res.Facts)
	}

	a, _ := agent.Get("wren-ai")
	if err := a.Validate(agent.Inputs{"sqlite-path": filepath.Join(t.TempDir(), "nope.sqlite3")}); err == nil ||
		!strings.Contains(err.Error(), "--sqlite-path: no such file") {
		t.Errorf("missing file: %v", err)
	}
}

func writeTree(t *testing.T, files map[string]string) string {
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
	res, err := checkContext(writeTree(t, map[string]string{
		"mdl/models.json": `{"catalog":"wren","models":[{"name":"orders","columns":[]},{"name":"users","columns":[]}],
			"relationships":[{"name":"orders_users"}],"views":[]}`,
		"mdl/list.json":    `[{"name":"items","columns":[{"name":"id"}]}]`,
		"notes/other.json": `{"hello":"world"}`,
		"README.md":        "notes",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(res.Facts, "\n"), "2 MDL files: 3 models, 1 relationships, 0 views") {
		t.Errorf("facts = %v", res.Facts)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "1 JSON files are not MDL") {
		t.Errorf("warnings = %v", res.Warnings)
	}

	_, err = checkContext(writeTree(t, map[string]string{"x.json": `{"a":1}`, "y.yaml": "a: 1"}))
	if err == nil || !strings.Contains(err.Error(), "no Wren MDL found") || !strings.Contains(err.Error(), "1 JSON files there are not MDL") {
		t.Errorf("err = %v", err)
	}
}
