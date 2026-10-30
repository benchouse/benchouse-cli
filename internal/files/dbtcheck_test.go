package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func joined(xs []string) string { return strings.Join(xs, "\n") }

func TestCheckDBTReportsTheProject(t *testing.T) {
	root := tree(t, map[string]string{
		"dbt_project.yml":               "name: shop\nversion: '1.2.0'\nmodel-paths: [\"transform\"]\n",
		"profiles.yml":                  "shop:\n  outputs:\n    prod:\n      password: hunter2\n",
		"transform/orders.sql":          "select 1",
		"transform/staging/stg.sql":     "select 2",
		"transform/py_model.py":         "def model(dbt, s): pass",
		"transform/schema.yml":          "version: 2\nmodels:\n  - name: orders\nsources:\n  - name: raw\n    tables: [{name: a}, {name: b}]\n",
		"transform/semantic.yml":        "semantic_models:\n  - name: orders\nmetrics:\n  - name: revenue\n  - name: aov\n",
		"transform/jinja.yml":           "models:\n  - name: {{ var('x') }}\n    : :\n",
		"macros/m.sql":                  "{% macro m() %}{% endmacro %}",
		"notes.txt":                     "left out",
		"target/manifest.json":          "{}",
		"target/semantic_manifest.json": "{}",
		"target/run_results.json":       "{}",
	})
	old := time.Now().Add(-45 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "target", "manifest.json"), old, old); err != nil {
		t.Fatal(err)
	}
	res, err := CheckDBT(root)
	if err != nil {
		t.Fatal(err)
	}
	facts, warns := joined(res.Facts), joined(res.Warnings)
	for _, want := range []string{
		"dbt project shop (version 1.2.0)",
		"3 models in transform, 1 macro files",
		"1 sources (2 tables), 1 documented models, 1 semantic models, 2 metrics",
		"compiled manifest target/manifest.json from",
		"semantic manifest target/semantic_manifest.json present",
		"9 files (314 B) will be included; 4 left out",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	for _, want := range []string{"45 days old: run `dbt compile`", "profiles.yml is present", "never read", "1 YAML files could not be parsed"} {
		if !strings.Contains(warns, want) {
			t.Errorf("warnings lack %q:\n%s", want, warns)
		}
	}
	if strings.Contains(facts+warns, "hunter2") {
		t.Error("profiles.yml content leaked")
	}
}

func TestCheckDBTWithoutManifest(t *testing.T) {
	res, err := CheckDBT(tree(t, map[string]string{"dbt_project.yml": "name: shop\n", "models/a.sql": "select 1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joined(res.Warnings), "no compiled manifest: run `dbt compile` for richer findings") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	if !strings.Contains(joined(res.Facts), "1 models in models") {
		t.Errorf("facts = %v", res.Facts)
	}
}

func TestCheckDBTFindsTheProjectBelow(t *testing.T) {
	root := tree(t, map[string]string{"analytics/dbt/dbt_project.yml": "name: shop\n"})
	_, err := CheckDBT(root)
	if err == nil || !strings.Contains(err.Error(), "did you mean "+filepath.Join(root, "analytics", "dbt")) {
		t.Errorf("err = %v", err)
	}

	root = tree(t, map[string]string{"a/dbt_project.yml": "name: a\n", "b/dbt_project.yml": "name: b\n"})
	_, err = CheckDBT(root)
	if err == nil || !strings.Contains(err.Error(), "not a dbt project: no dbt_project.yml") || !strings.Contains(err.Error(), "a, b") {
		t.Errorf("err = %v", err)
	}

	root = tree(t, map[string]string{"x/y/z/dbt_project.yml": "name: deep\n", "readme.md": "hi"})
	_, err = CheckDBT(root)
	if err == nil || !strings.Contains(err.Error(), "not a dbt project: no dbt_project.yml") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckDBTRefusesABadProjectFile(t *testing.T) {
	if _, err := CheckDBT(tree(t, map[string]string{"dbt_project.yml": "version: 2\n"})); err == nil || !strings.Contains(err.Error(), "no name") {
		t.Errorf("err = %v", err)
	}
	if _, err := CheckDBT(tree(t, map[string]string{"dbt_project.yml": "name: [\n"})); err == nil || !strings.Contains(err.Error(), "not valid YAML") {
		t.Errorf("err = %v", err)
	}
	if _, err := CheckDBT(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing folder passed")
	}
}

func TestScanCountsWithoutReading(t *testing.T) {
	root := tree(t, map[string]string{"a.sql": "12345", "b.txt": "x", ".env": "SECRET=1", "node_modules/x.sql": "no"})
	s, err := Scan(root, DBTKeep)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Kept) != 1 || s.KeptBytes != 5 || s.Left != 2 || len(s.Credentials) != 1 {
		t.Errorf("summary = %+v", s)
	}
}
