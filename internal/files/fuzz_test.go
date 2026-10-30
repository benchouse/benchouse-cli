package files

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// fuzzTokens are secrets recognisable by their form, one per rule family.
var fuzzTokens = []string{
	"ghp_CanaryS3cret9b7e1fQz4YtR2mWx8LpAvN0K",
	"AKIAQ7CANARY4242ZZZZ",
	"sk-ant-api03-Canary9b7e1fQz4YtR2mWx8LpAvN0K",
	"xoxb-4242424242-Canary9b7e1fQz4Y",
	"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJjYW5hcnkifQ.Q2FuYXJ5U2lnbmF0dXJl",
	"AIzaSyCanary9b7e1fQz4YtR2mWx8LpAvN0K12345",
}

var fuzzExts = []string{"models/x.yml", "x.json", "models/x.sql", "README.md", "x.py", "profiles.toml"}

// seps surround a planted token: a token glued to a word is a different
// word, which no form-based rule can know is a secret.
var seps = []string{" ", "\n", "\"", "'", ": ", "=", "\t", "(", ","}

// mask never panics, and a token planted anywhere in a file never comes
// out: it is masked, or the file is left out.
func FuzzMask(f *testing.F) {
	for _, s := range []string{
		"",
		"version: 2\nmodels:\n  - name: orders\n    description: one row per order\n",
		"outputs:\n  dev:\n    type: postgres\n    host: db\n    password: \"{{ env_var('PW') }}\"\n",
		`{"name": "x", "config": {"token": "abc", "items": [1, 2, {"k": "v"}]}}`,
		"select * from orders -- password: hunter2\nwhere id = 1\n",
		"# Title\n\nSome `code` and a [link](https://user:pw@example.com/x).\n",
		"key: |\n  multi\n  line\n",
		"a: &x {b: c}\nd: *x\n",
		"{{ config(materialized='table') }}\nselect 1\n",
		`{"nodes": {"model.x": {"raw_code": "{{ env_var('DBT_PASSWORD') }}", "compiled_code": "x"}}}`,
	} {
		for i := range fuzzExts {
			f.Add(s, uint16(len(s)/2), uint8(i), uint8(i), uint8(i))
		}
	}
	f.Fuzz(func(t *testing.T, content string, at uint16, ext, tok, sep uint8) {
		rel := fuzzExts[int(ext)%len(fuzzExts)]
		token := fuzzTokens[int(tok)%len(fuzzTokens)]
		s := seps[int(sep)%len(seps)]
		i := int(at) % (len(content) + 1)
		for i > 0 && i < len(content) && !utf8.RuneStart(content[i]) {
			i--
		}
		planted := content[:i] + s + token + s + content[i:]

		mask(rel, []byte(content)) // never panics on the file as given
		out, _, reason := mask(rel, []byte(planted))
		if reason != "" {
			if out != nil {
				t.Fatalf("%s left out (%s) but bytes returned", rel, reason)
			}
			return
		}
		if strings.Contains(string(out), token) {
			t.Fatalf("%s: planted token survived masking:\n--- in\n%s\n--- out\n%s", rel, planted, out)
		}
	})
}

// MaskTokens on its own never panics and never keeps a planted token.
func FuzzMaskTokens(f *testing.F) {
	f.Add("password=x; user=y", uint16(3), uint8(0), uint8(0))
	f.Add("https://a:b@host/x?token=y", uint16(10), uint8(1), uint8(1))
	f.Fuzz(func(t *testing.T, content string, at uint16, tok, sep uint8) {
		token := fuzzTokens[int(tok)%len(fuzzTokens)]
		s := seps[int(sep)%len(seps)]
		i := int(at) % (len(content) + 1)
		for i > 0 && i < len(content) && !utf8.RuneStart(content[i]) {
			i--
		}
		out, _ := MaskTokens(content[:i] + s + token + s + content[i:])
		if strings.Contains(out, token) {
			t.Fatalf("planted token survived: %q", out)
		}
	})
}
