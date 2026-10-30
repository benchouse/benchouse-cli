package telemetry

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// Realistic error strings from every agent, with the fragments that must
// not survive Scrub and the ones that should (so the message stays useful).
var scrubCases = []struct {
	name     string
	in       string
	banned   []string
	kept     []string
	category string
}{
	{
		name: "snowflake 401 with account and user",
		in: `snowflake http 401: {"data":null,"code":"390100","message":"Incorrect username or password was specified for user ANALYTICS_SVC on account xy12345.eu-west-1.","success":false}. ` +
			`Snowflake refused the programmatic access token: check it has not expired or been revoked, and that the account's network policy admits this machine`,
		banned:   []string{"ANALYTICS_SVC", "xy12345", "eu-west-1"},
		kept:     []string{"snowflake http 401", "Incorrect username or password"},
		category: AuthFailed,
	},
	{
		name:     "snowflake jwt rejected via account URL",
		in:       `Post "https://acme-prod.snowflakecomputing.com/api/v2/statements": snowflake http 401: {"code":"390144","message":"JWT token is invalid. [a1b2c3d4-0000-4000-8000-123456789abc]"}`,
		banned:   []string{"acme-prod", "snowflakecomputing", "a1b2c3d4", "123456789abc"},
		kept:     []string{"401", "JWT token is invalid"},
		category: AuthFailed,
	},
	{
		name:     "pgx auth failure with host user db",
		in:       "failed to connect to `user=nao_reader database=nao_prod`: 10.20.30.40:5432 (db.internal.acme.io): failed SASL auth: FATAL: password authentication failed for user \"nao_reader\" (SQLSTATE 28P01)",
		banned:   []string{"nao_reader", "nao_prod", "10.20.30.40", "acme", "db.internal"},
		kept:     []string{"password authentication failed for user <name>", "28P01"},
		category: AuthFailed,
	},
	{
		name:     "pgx dial error with bare host",
		in:       "failed to connect to `host=warehouse-01 user=bench database=wren`: hostname resolving error (lookup warehouse-01: no such host)",
		banned:   []string{"warehouse-01", "bench", "wren"},
		kept:     []string{"no such host"},
		category: Network,
	},
	{
		name:     "postgres permission denied",
		in:       `ERROR: permission denied for table thread_response (SQLSTATE 42501) as role analyst_ro`,
		banned:   []string{"analyst_ro"},
		kept:     []string{"permission denied", "42501"},
		category: PermissionDenied,
	},
	{
		name:     "postgres undefined table",
		in:       `thread_response is not readable (is this wren-ui's database, from Dec 2024 or later?): ERROR: relation "public.thread_response" does not exist (SQLSTATE 42P01)`,
		banned:   []string{"public.thread_response"},
		kept:     []string{"does not exist"},
		category: NotFound,
	},
	{
		name:     "databricks 403 with workspace and principal",
		in:       `could not connect to https://adb-1234567890123456.7.azuredatabricks.net: databricks http 403: {"error_code":"PERMISSION_DENIED","message":"User jane.doe@acme.com does not have permission to use SQL warehouse 0a1b2c3d4e5f6789 in workspace 1234567890123456."}. Check the workspace URL and that the token is valid`,
		banned:   []string{"adb-1234567890123456", "azuredatabricks", "jane.doe", "acme.com", "0a1b2c3d4e5f6789", "1234567890123456"},
		kept:     []string{"databricks http 403", "does not have permission"},
		category: PermissionDenied,
	},
	{
		name:     "databricks token in message",
		in:       "bad token dapi0123456789abcdef0123456789abcdef-3 for principal svc-genie-bot",
		banned:   []string{"dapi0123456789abcdef", "svc-genie-bot"},
		category: AuthFailed,
	},
	{
		name:     "looker json body",
		in:       `Looker at https://acme.cloud.looker.com:19999 rejected the client id and secret (HTTP 401): {"message":"Not found","documentation_url":"https://cloud.google.com/looker/docs/r/err/4.0/401"}`,
		banned:   []string{"acme", "looker.com", "19999", "cloud.google.com"},
		kept:     []string{"HTTP 401", "Not found"},
		category: AuthFailed,
	},
	{
		name:     "looker system activity",
		in:       `Looker refused System Activity (HTTP 403): the API key's user needs the see_system_activity permission (looker at acme.eu.looker.com, user id 42, client_id Kx7pQ2mZ9vB4nW8rT1yL)`,
		banned:   []string{"acme", "Kx7pQ2mZ9vB4nW8rT1yL"},
		kept:     []string{"HTTP 403"},
		category: PermissionDenied,
	},
	{
		name:     "langfuse json error",
		in:       `langfuse https://langfuse.internal.acme.org/api/public/traces: HTTP 500: {"error":"Internal Server Error","message":"prisma: connection to db-langfuse.acme.org:5432 failed for tenant acme-analytics"}`,
		banned:   []string{"langfuse.internal", "acme"},
		kept:     []string{"HTTP 500"},
		category: ServerError,
	},
	{
		name:     "langfuse rejected keys",
		in:       `langfuse rejected the API keys (401): check the public and secret key (pk-lf-1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d) and the host's region`,
		banned:   []string{"1a2b3c4d", "pk-lf"},
		kept:     []string{"rejected the API keys"},
		category: AuthFailed,
	},
	{
		name:     "metabase session token",
		in:       "metabase: X-Metabase-Session 3f2a9c1e-77b0-4c1d-9e8f-0123456789ab is not valid for user maria@corp.example",
		banned:   []string{"3f2a9c1e", "maria", "corp.example"},
		category: InvalidInput,
	},
	{
		name:     "nao sqlite path not found",
		in:       "nao open /Users/jordan.smith/Library/Application Support/nao/nao.db: no such file or directory",
		banned:   []string{"jordan.smith", "Library", "nao.db"},
		kept:     []string{"no such file or directory"},
		category: NotFound,
	},
	{
		name:     "home path with tilde and windows path",
		in:       `~/work/acme-dbt is not a folder; tried C:\Users\jdoe\dbt\acme_project and \\fileserver\share\models`,
		banned:   []string{"acme-dbt", "jdoe", "acme_project", "fileserver"},
		kept:     []string{"is not a folder"},
		category: NotFound,
	},
	{
		name:     "dbt file names",
		in:       "/home/ci/acme/analytics: models/marts/revenue_by_customer.sql: yaml: line 3: mapping values are not allowed",
		banned:   []string{"acme", "revenue_by_customer"},
		category: InvalidInput,
	},
	{
		name:     "x509",
		in:       `Post "https://wren.acme.local/api": tls: failed to verify certificate: x509: certificate is valid for ingress.local, not wren.acme.local`,
		banned:   []string{"wren", "acme", "ingress.local"},
		kept:     []string{"x509"},
		category: TLS,
	},
	{
		name:     "timeout with IPv6",
		in:       "dial tcp [2001:db8:85a3::8a2e:370:7334]:443: i/o timeout (no answer within 1m0s)",
		banned:   []string{"2001:db8", "7334"},
		kept:     []string{"i/o timeout"},
		category: Timeout,
	},
	{
		name:     "jwt in text",
		in:       "invalid bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyQGFjbWUuY29tIn0.c2lnbmF0dXJlLWJ5dGVz",
		banned:   []string{"eyJhbGci", "eyJzdWIi", "c2lnbmF0"},
		category: InvalidInput,
	},
	{
		name:     "cortex role and warehouse",
		in:       `SQL access control error: Insufficient privileges to operate on warehouse COMPUTE_WH as role BENCHOUSE_READER. Grant usage to role "BENCHOUSE_READER"`,
		banned:   []string{"COMPUTE_WH", "BENCHOUSE_READER"},
		kept:     []string{"Insufficient privileges"},
		category: PermissionDenied,
	},
	{
		name:     "lightdash project uuid and phone-like id",
		in:       "lightdash http 404: project 8d1b6a2e-3c4f-4e5a-9b0c-1d2e3f4a5b6c not found (org 5551234567)",
		banned:   []string{"8d1b6a2e", "5551234567"},
		kept:     []string{"404"},
		category: NotFound,
	},
	{
		name:     "terminal escapes",
		in:       "server said \x1b]52;c;ZXZpbA==\x07 hello\x1b[2J",
		banned:   []string{"\x1b", "\x07"},
		category: Unknown,
	},
}

func TestScrub(t *testing.T) {
	for _, c := range scrubCases {
		t.Run(c.name, func(t *testing.T) {
			got := Scrub(c.in)
			for _, b := range c.banned {
				if strings.Contains(strings.ToLower(got), strings.ToLower(b)) {
					t.Errorf("%q survived:\n%s", b, got)
				}
			}
			for _, k := range c.kept {
				if !strings.Contains(got, k) {
					t.Errorf("lost %q:\n%s", k, got)
				}
			}
			if cat := Category(errors.New(c.in)); cat != c.category {
				t.Errorf("Category = %s, want %s", cat, c.category)
			}
			if strings.Contains(got, "@") {
				t.Errorf("an @ survived: %s", got)
			}
			if n := utf8.RuneCountInString(got); n > MaxMessage {
				t.Errorf("%d characters", n)
			}
		})
	}
}

func TestScrubKeepsPlainMessages(t *testing.T) {
	for _, s := range []string{
		"context deadline exceeded",
		"the bundle is too large: collect a shorter window with --since",
		"this one is needed",
		"permission denied for schema",
	} {
		if got := Scrub(s); got != s {
			t.Errorf("Scrub(%q) = %q", s, got)
		}
	}
}

func TestScrubTruncates(t *testing.T) {
	got := Scrub(strings.Repeat("word ", 100))
	if n := utf8.RuneCountInString(got); n != MaxMessage || !strings.HasSuffix(got, "…") {
		t.Errorf("%d characters: %q", n, got)
	}
}

func TestScrubReplacesKnownValues(t *testing.T) {
	Known("test", map[string]bool{"hunter2!": true, "warehouse7": false, "ab": true})
	t.Cleanup(func() { Known("test", nil) })
	got := Scrub("password hunter2! rejected on warehouse7 (ab)")
	if got != "password <secret> rejected on <name> (ab)" {
		t.Errorf("got %q", got)
	}
}
