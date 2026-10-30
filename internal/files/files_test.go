package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDBTFolderKeepsSourceAndLeavesOutCredentials(t *testing.T) {
	root := t.TempDir()
	write(t, root, "dbt_project.yml", "name: shop\nprofile: shop\n")
	write(t, root, "models/orders.sql", "select * from {{ ref('stg_orders') }}\n")
	write(t, root, "models/schema.yml", "version: 2\nmodels:\n  - name: orders\n")
	write(t, root, "profiles.yml", "shop:\n  outputs:\n    dev:\n      password: LEAK-PROFILE\n")
	write(t, root, ".env", "SNOWFLAKE_PASSWORD=LEAK-ENV\n")
	write(t, root, "keys/rsa_key.p8", "-----BEGIN PRIVATE KEY-----\nLEAK\n-----END PRIVATE KEY-----\n")
	write(t, root, "target/manifest.json", `{"nodes":{}}`)
	write(t, root, "target/run_results.json", `{}`)
	write(t, root, "dbt_packages/dbt_utils/macros/x.sql", "select 1")
	write(t, root, "logs/dbt.log", "noise")
	write(t, root, "seeds/customers.csv", "id,email\n1,a@b.c\n")

	got, skipped, err := Read(root, DBTKeep)
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, f := range got {
		rels = append(rels, f.Rel)
		if strings.Contains(string(f.Data), "LEAK") {
			t.Errorf("%s leaked a secret", f.Rel)
		}
	}
	want := "dbt_project.yml,models/orders.sql,models/schema.yml,target/manifest.json"
	if strings.Join(rels, ",") != want {
		t.Errorf("kept %v, want %s", rels, want)
	}
	reasons := map[string]string{}
	for _, s := range skipped {
		reasons[s.Rel] = s.Reason
	}
	for _, rel := range []string{"profiles.yml", ".env", "keys/rsa_key.p8"} {
		if reasons[rel] != "credentials file" {
			t.Errorf("%s: skipped reason %q, want credentials file", rel, reasons[rel])
		}
	}
}

func TestSecretsInsideKeptFilesAreMasked(t *testing.T) {
	root := t.TempDir()
	write(t, root, "nao_config.yaml", `databases:
  - name: main
    url: postgresql://nao:LEAK-DSN@db.example.com:5432/world
    password: LEAK-PWD
    user: nao_c2
llm:
  provider: anthropic
  api_key: "{{ env('NAO_TOKEN') }}"
`)
	write(t, root, "RULES.md", "Connect with https://admin:LEAK-URL@wiki.example.com\n")
	write(t, root, "databases/orders.md", "-----BEGIN RSA PRIVATE KEY-----\nLEAK-KEY\n")

	got, skipped, err := Read(root, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	masked := 0
	for _, f := range got {
		all += string(f.Data)
		masked += f.Masked
	}
	for _, leak := range []string{"LEAK-DSN", "LEAK-PWD", "LEAK-URL", "LEAK-KEY"} {
		if strings.Contains(all, leak) {
			t.Errorf("%s survived:\n%s", leak, all)
		}
	}
	for _, keep := range []string{"user: nao_c2", "{{ env('NAO_TOKEN') }}", "db.example.com"} {
		if !strings.Contains(all, keep) {
			t.Errorf("lost %q", keep)
		}
	}
	if masked != 3 {
		t.Errorf("masked %d values, want 3", masked)
	}
	if len(skipped) != 1 || skipped[0].Reason != "contains what looks like a secret: private key" {
		t.Errorf("skipped = %+v", skipped)
	}
}

func TestJinjaYAMLWithoutSecretsIsKept(t *testing.T) {
	in := "models:\n  - name: orders\n    config:\n      materialized: {{ var('mat') }}\n"
	out, err := maskYAML([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Errorf("changed:\n%s", out)
	}
	// The line rule masks a same-line secret even in a file YAML cannot parse.
	out, err = maskYAML([]byte("x: {% if a %}\npassword: [LEAK\n"))
	if err != nil || strings.Contains(string(out), "LEAK") {
		t.Errorf("same-line secret: %q, %v", out, err)
	}
	// A secret on the next line of an unparsable file cannot be found: the
	// file is left out.
	if _, err := maskYAML([]byte("x: {% if a %}\npassword:\n  LEAK-NEXT\n")); err == nil {
		t.Error("an unparsable file with a next-line secret was kept")
	}
}

func TestCleanFileIsUnchanged(t *testing.T) {
	clean := "# models\nllm:\n  provider: anthropic   # the default\n  api_key: ${NAO_KEY}\n"
	if got := string(RedactConfig([]byte(clean))); got != clean {
		t.Errorf("a clean file changed:\n%q", got)
	}
}
