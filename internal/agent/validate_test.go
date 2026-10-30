package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidators(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "db.sqlite")
	os.WriteFile(file, []byte("x"), 0o600)
	cases := []struct {
		name string
		f    func(string) error
		good []string
		bad  []string
	}{
		{"URL", URL(false),
			[]string{"https://x.cloud.databricks.com", "x.azuredatabricks.net", "http://localhost:8080", "https://analytics.example.com"},
			[]string{"http://x.example.com", "https://u:p@x.example.com", "https://x.example.com/path", "ftp://x", "https://x.example.com/?o=1"}},
		{"URL with path", URL(true), []string{"https://x.example.com/langfuse"}, []string{"https://x.example.com/?q=1"}},
		{"Port", Port, []string{"1", "5432", "65535"}, []string{"0", "65536", "-1", "54x", "port"}},
		{"PositiveInt", PositiveInt, []string{"1", "5000"}, []string{"0", "-3", "many"}},
		{"Host", Host, []string{"localhost", "db.example.com", "10.0.0.5", "::1", "/var/run/postgresql"},
			[]string{"postgres://db", "user@db", "db.example.com/app", "db example"}},
		{"ExistingFile", ExistingFile, []string{file}, []string{filepath.Join(dir, "missing"), dir, "~/db.sqlite"}},
		{"Email", Email, []string{"a@b.co", "Ana <ana@example.com>"}, []string{"ana", "a@"}},
		{"Token", Token, []string{"dapi123abc", "pk-lf-1"}, []string{"two words", `"quoted"`}},
	}
	for _, c := range cases {
		for _, v := range c.good {
			if err := c.f(v); err != nil {
				t.Errorf("%s(%q) = %v, want ok", c.name, v, err)
			}
		}
		for _, v := range c.bad {
			if err := c.f(v); err == nil {
				t.Errorf("%s(%q) passed, want an error", c.name, v)
			}
		}
	}
	if err := Token("sk-lf secret"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("Token must not echo the value: %v", err)
	}
}

func TestSecretFileFieldsAreValidated(t *testing.T) {
	a := Agent{ID: "x", Name: "X", Fields: withSecretFiles([]Field{{Key: "token", Label: "Token", Secret: true}})}
	err := a.Validate(Inputs{"token-file": filepath.Join(t.TempDir(), "missing")})
	if err == nil {
		t.Fatal("a missing secret file passed")
	}
}
