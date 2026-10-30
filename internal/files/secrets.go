package files

import (
	"regexp"
	"strings"
)

// isSecretKey reports whether a key (a YAML or JSON key, an assignment's
// left-hand side) names a credential. Strong keys withhold everything under
// them, a nested block included; weak keys (the *_key family, auth) only
// withhold a scalar value, because data models use them for columns:
// customer_key: {description: ...} is documentation, not a secret.
func isSecretKey(k string) (match, strong bool) {
	k = strings.ToLower(strings.Trim(k, `"' `))
	n := strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(k)
	if n == "" {
		return false, false
	}
	for _, s := range strongSubstrings {
		if strings.Contains(n, s) {
			return true, true
		}
	}
	parts := strings.Split(n, "_")
	last := parts[len(parts)-1]
	for _, p := range parts {
		if p == "dsn" || p == "connstr" {
			return true, true
		}
	}
	if last == "auth" || last == "authorization" {
		return true, false
	}
	if len(parts) >= 2 && last == "key" && parts[len(parts)-2] != "" && !columnKeys[strings.Join(parts[:len(parts)-1], "_")] {
		return true, false
	}
	if compoundKeys[n] {
		return true, false
	}
	return false, false
}

var strongSubstrings = []string{
	"password", "passwd", "pwd", "passphrase", "secret", "token", "credential",
	"private_key", "privatekey", "access_key", "accesskey", "api_key", "apikey",
	"bearer", "connection_string", "connectionstring", "conn_str", "webhook",
}

// columnKeys are *_key names that mean a column or a table design choice in
// dbt, warehouse DDL and data models, never a secret.
var columnKeys = map[string]bool{
	"primary": true, "foreign": true, "unique": true, "sort": true, "dist": true,
	"partition": true, "cluster": true, "clustering": true, "surrogate": true, "natural": true,
	"business": true, "composite": true, "candidate": true, "join": true, "merge": true,
	"lookup": true, "group": true, "hash": true, "range": true, "row": true, "dedup": true,
	"dedupe": true, "idempotency": true, "cache": true, "sorting": true, "distribution": true,
	"entity": true, "foreign_table": true, "parent": true, "child": true, "unique_id": true,
}

// compoundKeys are credential names written without a separator.
var compoundKeys = map[string]bool{
	"secretkey": true, "signingkey": true, "licensekey": true, "encryptionkey": true,
	"masterkey": true, "clientkey": true, "authkey": true,
}

// mentionsSecretKey reports whether any word in b is a credential-like key.
func mentionsSecretKey(b []byte) bool {
	for _, w := range keyWord.FindAllString(string(b), -1) {
		if m, _ := isSecretKey(w); m {
			return true
		}
	}
	return false
}

var keyWord = regexp.MustCompile(`[A-Za-z0-9_.-]+`)

// Environment references. A value that is only a reference is a name, not a
// secret, and is kept; a reference with a default ({{ env_var('X', 'pw') }})
// carries a literal, which is masked inside the reference's shape.
var (
	envRef = regexp.MustCompile(`^["']?(?:` +
		`\{\{\s*env(?:_var)?\(\s*(?:'[A-Za-z_][A-Za-z0-9_]*'|"[A-Za-z_][A-Za-z0-9_]*")\s*(?:,\s*(?:'\[redacted\]'|"\[redacted\]"|''|""))?\s*\)\s*(?:\|[^}]*)?\}\}` +
		`|\$\{[A-Za-z_][A-Za-z0-9_]*\}` +
		`|\$[A-Z_][A-Z0-9_]+` +
		`)["']?$`)
	envDefault = regexp.MustCompile(`(env(?:_var)?\(\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]\s*,\s*)('[^'\n]*'|"[^"\n]*")`)
)

func isEnvRef(v string) bool { return envRef.MatchString(strings.TrimSpace(v)) }

// maskEnvDefaults replaces the default literal of env()/env_var() calls in s:
// every one when always is set (the value sits under a secret key), otherwise
// only where the variable's own name says it holds a credential.
func maskEnvDefaults(s string, always bool) string {
	return envDefault.ReplaceAllStringFunc(s, func(m string) string {
		g := envDefault.FindStringSubmatch(m)
		lit := g[3]
		if len(lit) <= 2 || lit[1:len(lit)-1] == redacted {
			return m
		}
		if secret, _ := isSecretKey(g[2]); !always && !secret {
			return m
		}
		q := lit[:1]
		return g[1] + q + redacted + q
	})
}

// tokenRule is one shape of secret found by its form rather than its key.
type tokenRule struct {
	kind string
	re   *regexp.Regexp
	// group is the submatch to replace; 0 replaces the whole match.
	group int
	// valid, if set, must accept the replaced text, which keeps lookalike
	// words out.
	valid func(string) bool
}

// tokenRules mask self-delimiting secrets wherever they appear: the match can
// be replaced without guessing where the secret ends.
var tokenRules = []tokenRule{
	{kind: "AWS access key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{kind: "GitHub token", re: regexp.MustCompile(`\b(?:ghp|gho|ghs|ghu|ghr)_[A-Za-z0-9]{36,}\b`)},
	{kind: "GitHub token", re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}`)},
	{kind: "Slack webhook", re: regexp.MustCompile(`(https://hooks\.slack\.com/services/)([A-Za-z0-9/_-]+)`), group: 2},
	{kind: "Slack token", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{kind: "Anthropic key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{kind: "OpenAI key", re: regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`), valid: randomish},
	{kind: "Stripe key", re: regexp.MustCompile(`\b(?:sk|rk)_live_[0-9A-Za-z]{16,}`)},
	{kind: "Google API key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`)},
	{kind: "JWT", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{kind: "bearer token", re: regexp.MustCompile(`(?i)(\bbearer\s+)([A-Za-z0-9._~+/=-]{16,})`), group: 2, valid: randomish},
	// user:password@host/ without a scheme: a Snowflake or MySQL DSN.
	{kind: "DSN password", re: regexp.MustCompile(`(?i)(\b[a-z0-9_.-]+:)([^@\s/:'"<>{}\[\]]{3,})(@[a-z0-9-]+(?:\.[a-z0-9-]+)*(?::\d+)?/)`), group: 2, valid: literal},
	// ?password=... in a URL or connection string.
	{kind: "credential parameter", re: regexp.MustCompile(`(?i)([?&;](?:password|passwd|pwd|token|access_token|auth_token|api_key|apikey|secret|client_secret)=)([^&\s'"#<>;]+)`), group: 2, valid: literal},
	// password=... / Pwd=...; in a libpq or ODBC connection string.
	{kind: "DSN password", re: regexp.MustCompile(`(?i)((?:^|[\s;'"(,])(?:password|pwd)\s*=\s*)([^\s;'"&,()<>\[\]{}]+)(?:[\s;'"&,)]|$)`), group: 2, valid: literal},
	// A credential named in a comment: # password: x, -- pwd = x, // token: x.
	// ## starts a Markdown heading, not a comment.
	{kind: "secret in a comment", re: regexp.MustCompile(`(?im)((?:^|\s)(?:#[^#\n]|--|//)[^\n]*?\b(?:password|passwd|pwd|passphrase|secret|token|api[_ -]?key|access[_ ]key|private[_ ]key)\b\s*[:=]\s*)([^\s,;]{4,})`), group: 2, valid: literal},
}

// quotedAssign masks `password = "x"` / "api_key": "x" in any file: a quoted
// literal under a credential-like name.
var quotedAssign = regexp.MustCompile(`(?i)(["']?([a-z0-9_.-]+)["']?\s*(?::|=|:=)\s*)("[^"\n]*"|'[^'\n]*')`)

// failRules find secrets that cannot be masked in place: the whole file is
// left out. They are also checked last, so a masking bug fails closed.
var failRules = []tokenRule{
	{kind: "private key", re: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)},
	{kind: "private key", re: regexp.MustCompile(`PuTTY-User-Key-File-\d`)},
}

// MaskTokens masks secrets recognisable by their form (cloud and API keys,
// JWTs, webhook URLs, DSN passwords, credentials in comments and quoted
// assignments) and returns the result with the number of values masked.
func MaskTokens(s string) (string, int) {
	before := strings.Count(s, redacted)
	s = maskTokens(s)
	return s, strings.Count(s, redacted) - before
}

func maskTokens(s string) string {
	s = urlUserinfo.ReplaceAllString(s, "${1}"+redacted+"@")
	s = maskEnvDefaults(s, false)
	for _, r := range tokenRules {
		s = replaceRule(r, s)
	}
	s = quotedAssign.ReplaceAllStringFunc(s, func(m string) string {
		g := quotedAssign.FindStringSubmatch(m)
		secret, strong := isSecretKey(g[2])
		inner := g[3][1 : len(g[3])-1]
		if !secret || !literal(inner) || (!strong && !keyLike(inner)) {
			return m
		}
		q := g[3][:1]
		return g[1] + q + redacted + q
	})
	return s
}

func replaceRule(r tokenRule, s string) string {
	return r.re.ReplaceAllStringFunc(s, func(m string) string {
		if r.group == 0 {
			if r.valid != nil && !r.valid(m) {
				return m
			}
			return redacted
		}
		loc := r.re.FindStringSubmatchIndex(m)
		a, b := loc[2*r.group], loc[2*r.group+1]
		if a < 0 || (r.valid != nil && !r.valid(m[a:b])) {
			return m
		}
		return m[:a] + redacted + m[b:]
	})
}

// detectSecret names the first secret in s that masking did not remove, or "".
func detectSecret(s string) string {
	for _, r := range failRules {
		if r.re.MatchString(s) {
			return r.kind
		}
	}
	for _, r := range tokenRules {
		if replaceRule(r, s) != s {
			return r.kind
		}
	}
	return ""
}

// literal reports whether v is a literal value rather than a reference, a
// placeholder or something already masked.
func literal(v string) bool {
	v = strings.Trim(strings.TrimSpace(v), `"'`)
	if v == "" || strings.Contains(v, redacted) || isEnvRef(v) {
		return false
	}
	switch {
	case strings.HasPrefix(v, "{{"), strings.HasPrefix(v, "${"), strings.HasPrefix(v, "{%"),
		strings.HasPrefix(v, "<"), strings.HasPrefix(v, "%("), strings.HasPrefix(v, "%s"),
		strings.Trim(v, "*x.•X") == "":
		return false
	}
	switch strings.ToLower(v) {
	case "none", "null", "nil", "true", "false", "empty", "unset", "required", "optional", "string":
		return false
	}
	return true
}

// keyLike reports whether a value under a weak key (license_key,
// signing_key) looks like key material rather than a column name.
func keyLike(v string) bool {
	return len(v) >= 8 && !identifier.MatchString(v)
}

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// randomish keeps words out of the key and bearer rules: a real key mixes
// letters and digits.
func randomish(v string) bool {
	var letter, digit bool
	for _, c := range v {
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			letter = true
		}
	}
	return letter && digit
}
