package files

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Fake credentials, assembled so no scanner flags this source file.
var (
	awsKey    = "AKIA" + "IOSFODNN7EXAMPLE"
	ghToken   = "ghp_" + strings.Repeat("a1B2", 9)
	ghPAT     = "github_pat_" + strings.Repeat("11AB", 6)
	slackTok  = "xoxb-" + "123456789012-abcdefABCDEF"
	slackHook = "https://hooks.slack.com/services/" + "T000/B000/XXXXyyyyZZZZ"
	openaiKey = "sk-" + "proj-" + strings.Repeat("Ab3", 10)
	antKey    = "sk-ant-" + "api03-" + strings.Repeat("xY9", 10)
	googleKey = "AIza" + strings.Repeat("Sy1", 11) + "xx"
	stripeKey = "sk_live_" + strings.Repeat("4eC3", 6)
	jwt       = "eyJ" + "hbGciOiJIUzI1NiJ9" + ".eyJ" + "zdWIiOiIxMjM0In0" + ".dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
)

func maskString(t *testing.T, rel, in string) (string, int, string) {
	t.Helper()
	out, n, reason := mask(rel, []byte(in))
	return string(out), n, reason
}

func TestTokensAreMaskedInAnyFile(t *testing.T) {
	cases := []struct{ name, in, secret, keep string }{
		{"aws", "aws s3 ls # key " + awsKey + " ok", awsKey, "aws s3 ls"},
		{"github", "token " + ghToken, ghToken, "token"},
		{"github pat", "x=" + ghPAT, ghPAT, "x="},
		{"slack token", "slack: " + slackTok, slackTok, "slack:"},
		{"slack webhook", "post to " + slackHook + " daily", "XXXXyyyyZZZZ", "hooks.slack.com/services/"},
		{"openai", "OPENAI " + openaiKey, openaiKey, "OPENAI"},
		{"anthropic", "key " + antKey, antKey, "key"},
		{"google", "maps " + googleKey, googleKey, "maps"},
		{"stripe", "charge " + stripeKey, stripeKey, "charge"},
		{"jwt", "auth header " + jwt, jwt, "auth header"},
		{"bearer", "curl -H 'Authorization: Bearer abc123DEF456ghi789'", "abc123DEF456ghi789", "Authorization: Bearer"},
		{"snowflake dsn", "dsn = analyst:Hunter2pw@xy12345.eu-west-1/ANALYTICS", "Hunter2pw", "analyst:"},
		{"query param", "jdbc:postgresql://db/x?user=a&password=Hunter2pw&ssl=true", "Hunter2pw", "&ssl=true"},
		{"libpq", "host=db user=a password=Hunter2pw dbname=x", "Hunter2pw", "dbname=x"},
		{"odbc", "Server=db;Uid=a;Pwd=Hunter2pw;Database=x", "Hunter2pw", "Database=x"},
		{"sql comment", "select 1 -- password = Hunter2pw\n", "Hunter2pw", "select 1"},
		{"hash comment", "# password: Hunter2pw\nname: x\n", "Hunter2pw", "name: x"},
		{"trailing comment", "x: y # old password: Hunter2pw\n", "Hunter2pw", "x: y"},
		{"python quoted", "conn = connect(password=\"Hunter2pw\")\n", "Hunter2pw", "connect("},
		{"env default", "{{ env_var('DBT_PASSWORD', 'Hunter2pw') }}", "Hunter2pw", "{{ env_var('DBT_PASSWORD', '[redacted]') }}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, n, reason := maskString(t, "notes.sql", c.in)
			if reason != "" {
				t.Fatalf("skipped: %s", reason)
			}
			if strings.Contains(out, c.secret) || n == 0 {
				t.Errorf("not masked (%d): %q", n, out)
			}
			if !strings.Contains(out, c.keep) {
				t.Errorf("lost %q: %q", c.keep, out)
			}
		})
	}
}

func TestLookalikesAreNotMasked(t *testing.T) {
	for _, in := range []string{
		"select * from task-scheduler-for-the-warehouse-jobs",
		"risk-adjusted-revenue-by-customer-segment",
		"Bearer authentication is described in RULES.md",
		"-- the password column is hashed\nselect password_hash from users",
		"{{ env_var('DBT_SCHEMA', 'analytics') }}",
		"select * from orders where status = 'open'",
		"see https://github.com/getnao/nao and git@github.com:getnao/nao.git",
		"max_tokens = 4096\n",
		"{{ config(unique_key='order_id') }}\n",
		"password = os.environ[\"PW\"]\n",
		"conn = connect(password=\"{{ env_var('PW') }}\")\n",
		"AKIAshort and sk-short and ghp_short",
		"## Password: rules\n",
	} {
		out, n, reason := maskString(t, "x.sql", in)
		if reason != "" || n != 0 || out != in {
			t.Errorf("%q changed (%d, %q): %q", in, n, reason, out)
		}
	}
}

func TestPrivateKeysAreLeftOut(t *testing.T) {
	for _, in := range []string{
		"-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n",
		"-----BEGIN PGP PRIVATE KEY BLOCK-----\nabc\n",
		"PuTTY-User-Key-File-3: ssh-ed25519\n",
	} {
		if _, _, reason := maskString(t, "notes.md", in); reason != "contains what looks like a secret: private key" {
			t.Errorf("%q: reason %q", in, reason)
		}
	}
}

func TestJSONMaskedByKeyPreservingLayout(t *testing.T) {
	in := `{
  "name": "shop",
  "database": {"user": "a", "password": "LEAK-1", "port": 5432},
  "credentials": {"user": "LEAK-2", "nested": ["LEAK-3"]},
  "pin_secret": 123456,
  "api_key": "{{ env('API_KEY') }}",
  "client_secret": "{{ env_var('CS', 'LEAK-4') }}",
  "url": "postgres://u:LEAK-5@db/x",
  "columns": {"customer_key": {"description": "surrogate key"}},
  "unique_key": "order_id",
  "signing_key": "LEAK-6",
  "enabled_token_auth": true
}`
	out, n, reason := maskString(t, "wren/mdl.json", in)
	if reason != "" {
		t.Fatal(reason)
	}
	for _, leak := range []string{"LEAK-1", "LEAK-2", "LEAK-3", "LEAK-4", "LEAK-5", "LEAK-6", "123456"} {
		if strings.Contains(out, leak) {
			t.Errorf("%s survived:\n%s", leak, out)
		}
	}
	for _, keep := range []string{`"user": "a"`, `"port": 5432`, `{{ env('API_KEY') }}`, `"description": "surrogate key"`,
		`"unique_key": "order_id"`, `{{ env_var('CS', '[redacted]') }}`, `"enabled_token_auth": true`, `"name": "shop",` + "\n"} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %q:\n%s", keep, out)
		}
	}
	if n != 7 {
		t.Errorf("masked %d, want 7", n)
	}
	clean := `{"b":1,  "a":[true,null,"x"]}`
	if out, n, _ := maskString(t, "x.json", clean); out != clean || n != 0 {
		t.Errorf("clean JSON changed: %q", out)
	}
	// Not JSON (comments): the line scan still finds quoted secrets.
	out, _, reason = maskString(t, "x.json", "{\n  // c\n  \"password\": \"LEAK-7\",\n}\n")
	if reason != "" || strings.Contains(out, "LEAK-7") || !strings.Contains(out, `"password": "[redacted]",`) {
		t.Errorf("unparsable JSON: %q %q", out, reason)
	}
}

func TestManifestCompiledCodeIsScanned(t *testing.T) {
	in := `{"nodes":{"model.shop.orders":{"raw_code":"select * from {{ source('s','o') }}",` +
		`"compiled_code":"select * from read_s3('s3://b/o', '` + awsKey + `', 'x?token=Hunter2pw')",` +
		`"config":{"unique_key":"order_id"},"columns":{"customer_key":{"name":"customer_key"},` +
		`"access_token":{"name":"access_token","description":"hashed"}}}}}`
	out, n, reason := maskString(t, "target/manifest.json", in)
	if reason != "" || strings.Contains(out, awsKey) || strings.Contains(out, "Hunter2pw") || n != 2 {
		t.Errorf("(%d, %q) %s", n, reason, out)
	}
	if !strings.Contains(out, `"unique_key":"order_id"`) || !strings.Contains(out, `"customer_key":{"name":"customer_key"}`) ||
		!strings.Contains(out, `"access_token":{"name":"access_token","description":"hashed"}`) ||
		!strings.Contains(out, "select * from read_s3(") {
		t.Errorf("lost structure: %s", out)
	}
}

func TestCompiledCodeFromASecretEnvVarIsReplaced(t *testing.T) {
	in := `{"nodes":{` +
		`"a":{"raw_code":"select '{{ env_var(\"DBT_API_TOKEN\") }}' as t","compiled_code":"select 'plainLEAK' as t"},` +
		`"b":{"compiled_code":"select 'x' as s, 'postgres://u:LEAK@h/d'","raw_code":"select '{{ env_var(\"DBT_SCHEMA\") }}' as s"}}}`
	out, _, reason := maskString(t, "target/manifest.json", in)
	if reason != "" || strings.Contains(out, "LEAK") {
		t.Fatalf("(%q) %s", reason, out)
	}
	if !strings.Contains(out, `"compiled_code":`+compiledFromSecret) || !strings.Contains(out, `select 'x' as s`) {
		t.Errorf("%s", out)
	}
}

func TestSecretKeys(t *testing.T) {
	yes := []string{"password", "db_passwd", "passphrase", "Authorization", "auth", "basic_auth", "bearer_token",
		"dsn", "DATABASE_DSN", "connection_string", "connstr", "webhook", "webhook_url", "client_secret",
		"encryption_key", "license_key", "signing_key", "api_key", "apiKey", "private-key", "secretKey"}
	no := []string{"author", "authors", "auth_method", "primary_key", "foreign_key", "unique_key", "sort_key",
		"dist_key", "partition_key", "cluster_key", "surrogate_key", "natural_key", "key", "keys", "monkey",
		"description", "name", "hockey_score", "keyboard"}
	for _, k := range yes {
		if m, _ := isSecretKey(k); !m {
			t.Errorf("%s: not a secret key", k)
		}
	}
	for _, k := range no {
		if m, _ := isSecretKey(k); m {
			t.Errorf("%s: flagged as a secret key", k)
		}
	}
}

func TestDBTConfigsWithColumnKeysAreUntouched(t *testing.T) {
	in := `version: 2
models:
  - name: orders
    config:
      materialized: incremental
      unique_key: order_id
      sort_key: created_at
      dist_key: customer_id
      partition_key: day
      cluster_key: [region]
    columns:
      - name: surrogate_key
        description: "{{ dbt_utils.generate_surrogate_key(['order_id']) }}"
      - name: key
        tests: [unique]
    meta:
      surrogate_key: order_sk
      natural_key: order_number
      customer_key:
        description: links to customers
      key: id
`
	out, n, reason := maskString(t, "models/schema.yml", in)
	if reason != "" || n != 0 || out != in {
		t.Errorf("dbt config changed (%d, %q):\n%s", n, reason, out)
	}
}

func TestYAMLNewSecretKeysAndEnvRefs(t *testing.T) {
	in := `outputs:
  dsn: postgres://LEAK-1
  passphrase: LEAK-2
  authorization: Basic LEAK-3
  webhook_url: https://example.com/hook/LEAK-4
  encryption_key: LEAK-5
  password: $uper1LEAK
  pw_ref: ${DB_PASSWORD}
  token: $DBT_TOKEN
  api_key: "{{ env_var('KEY', 'LEAK-6') }}"
  author: Ann
`
	out, _, reason := maskString(t, "nao_config.yaml", in)
	if reason != "" {
		t.Fatal(reason)
	}
	for i := 1; i <= 6; i++ {
		if leak := "LEAK-" + string(rune('0'+i)); strings.Contains(out, leak) {
			t.Errorf("%s survived:\n%s", leak, out)
		}
	}
	for _, keep := range []string{"${DB_PASSWORD}", "$DBT_TOKEN", "{{ env_var('KEY', '[redacted]') }}", "author: Ann"} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %q:\n%s", keep, out)
		}
	}
	if strings.Contains(out, "uper1") {
		t.Errorf("$uper1 is a password, not an env ref:\n%s", out)
	}
}

func TestEnvRef(t *testing.T) {
	for _, v := range []string{"${X}", "${db_pass}", "$DB_PASS", `"{{ env('X') }}"`, "{{ env_var('X') | as_number }}", "{{ env_var('X', '[redacted]') }}"} {
		if !isEnvRef(v) {
			t.Errorf("%s: not an env ref", v)
		}
	}
	for _, v := range []string{"$uper1", "$a", "$X", "{{ env_var('X', 'pw') }}", "x${X}", "{{ env('X') }}tail"} {
		if isEnvRef(v) {
			t.Errorf("%s: taken for an env ref", v)
		}
	}
}

func TestNewCredentialsFiles(t *testing.T) {
	for _, rel := range []string{"id_rsa", ".ssh/id_ed25519", "id_ecdsa", "cert.pfx", "c.p12", "k.jks", "a.keystore",
		"prod.tfvars", "terraform.tfstate", "terraform.tfstate.backup", "secrets.yml", "config/secrets.yaml",
		"client_secret.json", "service-account-prod.json", "vault.kdbx", ".pgpass", ".my.cnf"} {
		if !credentialsFile(rel) {
			t.Errorf("%s: not a credentials file", rel)
		}
	}
	for _, rel := range []string{"id_rsa.pub", "models/secrets_report.sql", "service.json", "manifest.json", "keys.md"} {
		if credentialsFile(rel) {
			t.Errorf("%s: taken for a credentials file", rel)
		}
	}
}

func TestReadRefusesSwappedFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "hello\n")
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("LEAK"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen, err := os.Lstat(filepath.Join(root, "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Swap a.md for a symlink after the walk saw it.
	if err := os.Remove(filepath.Join(root, "a.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a.md")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	raw, reason, err := readRegular(filepath.Join(root, "a.md"), seen)
	if err != nil || reason == "" || raw != nil {
		t.Errorf("read through a swapped symlink: %q %q %v", raw, reason, err)
	}
	// A different regular file in its place is refused too.
	if err := os.Remove(filepath.Join(root, "a.md")); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.md", "other\n")
	if _, reason, err := readRegular(filepath.Join(root, "a.md"), seen); err != nil || reason != "changed while being read" {
		t.Errorf("replaced file: %q %v", reason, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Remove(filepath.Join(root, "a.md")); err != nil {
			t.Fatal(err)
		}
		if err := mkfifo(filepath.Join(root, "a.md")); err == nil {
			if _, reason, err := readRegular(filepath.Join(root, "a.md"), seen); err != nil || reason != "not a regular file" {
				t.Errorf("fifo: %q %v", reason, err)
			}
		}
	}
	// The unswapped case reads.
	write(t, root, "b.md", "fine\n")
	fi, _ := os.Lstat(filepath.Join(root, "b.md"))
	if raw, reason, err := readRegular(filepath.Join(root, "b.md"), fi); err != nil || reason != "" || string(raw) != "fine\n" {
		t.Errorf("plain read: %q %q %v", raw, reason, err)
	}
}

func TestReadSkipsFilesWithUnmaskableSecretsAndCountsMasks(t *testing.T) {
	root := t.TempDir()
	write(t, root, "models/orders.sql", "select 1 -- key "+awsKey+"\n")
	write(t, root, "notes.md", "-----BEGIN PGP PRIVATE KEY BLOCK-----\n")
	write(t, root, "id_rsa", "x")
	got, skipped, err := Read(root, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Masked != 1 || strings.Contains(string(got[0].Data), awsKey) {
		t.Errorf("got %+v", got)
	}
	reasons := map[string]string{}
	for _, s := range skipped {
		reasons[s.Rel] = s.Reason
	}
	if reasons["notes.md"] != "contains what looks like a secret: private key" || reasons["id_rsa"] != "credentials file" {
		t.Errorf("skipped %v", reasons)
	}
}

func TestMaskTokensExported(t *testing.T) {
	out, n := MaskTokens(`{"content":"use ` + ghToken + `"}`)
	if n != 1 || strings.Contains(out, ghToken) {
		t.Errorf("%d %s", n, out)
	}
}
