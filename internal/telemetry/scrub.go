package telemetry

import (
	"encoding/json"
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/benchouse/benchouse-cli/internal/prompt"
)

// MaxMessage bounds a scrubbed message, in characters.
const MaxMessage = 200

// Scrub removes anything identifying from an error message before it is
// sent, keeping its shape ("password authentication failed for user
// <name>"): emails become <email>; URLs, hostnames and IP addresses <host>;
// file paths <path>; quoted strings and identifiers named after "user",
// "role", "as", "database" and the like <name>; UUIDs, hex ids and long
// digit runs <id>; anything token-like <secret>. The result is cleaned of
// terminal escapes, on one line, and at most MaxMessage characters.
func Scrub(msg string) string {
	s := scrub(replaceKnown(msg), 0)
	if r := []rune(s); len(r) > MaxMessage {
		s = string(r[:MaxMessage-1]) + "…"
	}
	return s
}

var (
	knownMu sync.Mutex
	known   = map[string]map[string]string{} // group → value → placeholder
)

// Known registers the values a run was given (connection fields, the
// email, folder paths), so Scrub replaces them wherever they appear, even
// when no pattern would recognise them: secret values become <secret>,
// others <name>. Each call replaces the group's earlier values. Values
// shorter than 4 characters are ignored.
func Known(group string, values map[string]bool) {
	m := map[string]string{}
	for v, secret := range values {
		v = strings.TrimSpace(v)
		if len(v) < 4 {
			continue
		}
		m[v] = "<name>"
		if secret {
			m[v] = "<secret>"
		}
	}
	knownMu.Lock()
	known[group] = m
	knownMu.Unlock()
}

// replaceKnown replaces the Known values in s, longest first.
func replaceKnown(s string) string {
	knownMu.Lock()
	var vals []string
	repl := map[string]string{}
	for _, g := range known {
		for v, p := range g {
			vals = append(vals, v)
			repl[v] = p
		}
	}
	knownMu.Unlock()
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, repl[v])
	}
	return s
}

var (
	// A JSON error body's message is kept, scrubbed: it says what went
	// wrong. Every other quoted string goes.
	jsonMsgRe = regexp.MustCompile(`"(message|error|error_description|detail|msg|title)"\s*:\s*("(?:[^"\\]|\\.)*")`)

	emailRe   = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	urlRe     = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://[^\s"'<>()\[\]{}]+`)
	quotedRe  = regexp.MustCompile("\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`|'[^'\\s][^']*'|‘[^’]*’|“[^”]*”")
	uuidRe    = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	winPathRe = regexp.MustCompile(`(?i)(?:\b[a-z]:[\\/]|\\\\)[^\s"'<>|:;,()]+`)
	unixPath  = regexp.MustCompile(`(^|[\s(\[=,:])(~?/[^\s"'<>;,()\[\]]*|~|\.\.?/[^\s"'<>;,()\[\]]+)`)
	ipv4Re    = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b`)
	ipv6Re    = regexp.MustCompile(`\[?(?:[0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}(?:%\w+)?\]?(?::\d+)?`)
	relPath   = regexp.MustCompile(`(?:^|\s)[^\s/<>()"']+(?:/[^\s/()"']*)+`)
	glued     = regexp.MustCompile(`[A-Za-z0-9_\-.]*<(id|secret)>[A-Za-z0-9_\-.]*`)
	hostRe    = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9_\-]*[a-z0-9])?\.)+[a-z][a-z0-9\-]*[a-z0-9](?::\d+)?\b`)
	hostPort  = regexp.MustCompile(`(?i)\b[a-z][a-z0-9_\-]*:\d{2,5}\b`)
	hexRe     = regexp.MustCompile(`(?i)\b(?:0x)?[0-9a-f]*[0-9][0-9a-f]*[a-f][0-9a-f]*\b|\b(?:0x)?[0-9a-f]*[a-f][0-9a-f]*[0-9][0-9a-f]*\b`)
	digitsRe  = regexp.MustCompile(`\b\d{7,}\b`)
	tokenRe   = regexp.MustCompile(`[A-Za-z0-9_\-+/=.~]{20,}`)
	// keyword names the identifier after it: "user bob", "role=ANALYST",
	// "as svc_reader", "database analytics", "account xy12345".
	keywordRe = regexp.MustCompile(`(?i)\b(` + keywords + `)(\s*[=:]\s*|\s+)(?:(` + keywords + `)(\s*[=:]\s*|\s+))?([^\s,;:()\[\]{}<>]+)`)
)

const keywords = `user(?:name)?|role|as|principal|login|account|database|db|dbname|schema|catalog|warehouse|workspace|org(?:anization)?|tenant|host(?:name)?|owner|lookup|tcp`

// keywordKeep are words after a keyword that are not identifiers.
var keywordKeep = map[string]bool{
	"a": true, "an": true, "the": true, "this": true, "that": true, "it": true, "is": true, "are": true, "was": true,
	"not": true, "no": true, "to": true, "of": true, "in": true, "on": true, "with": true, "and": true, "or": true,
	"does": true, "do": true, "has": true, "have": true, "must": true, "needs": true, "cannot": true, "could": true,
	"name": true, "id": true, "and/or": true, "which": true, "you": true, "your": true, "its": true, "any": true,
	"we": true, "be": true, "been": true, "from": true, "at": true, "user": true, "role": true, "access": true,
	"more": true, "less": true, "than": true, "now": true, "yet": true, "same": true, "well": true, "possible": true,
	"expected": true, "following": true, "default": true, "session": true, "unknown": true, "invalid": true,
	"select": true, "usage": true, "read": true, "write": true, "query": true, "queries": true,
	"resolving": true, "url": true, "id:": true, "system": true, "audit": true, "activity": true, "permission": true, "privilege": true, "privileges": true,
}

// pathWords look like relative paths but are words.
var pathWords = map[string]bool{"and/or": true, "i/o": true, "read/write": true, "on/off": true, "yes/no": true, "true/false": true}

// fileExt are extensions that make a dotted word a file name, not a host.
var fileExt = map[string]bool{"yml": true, "yaml": true, "json": true, "sql": true, "md": true, "py": true, "go": true,
	"txt": true, "csv": true, "toml": true, "lock": true, "pem": true, "p8": true, "key": true, "db": true, "sqlite": true,
	"gz": true, "tar": true, "lkml": true, "env": true, "log": true, "html": true, "js": true, "ts": true}

func scrub(msg string, depth int) string {
	s := prompt.Clean(msg)
	s = strings.Join(strings.Fields(s), " ")
	if depth == 0 {
		s = jsonMsgRe.ReplaceAllStringFunc(s, func(m string) string {
			sub := jsonMsgRe.FindStringSubmatch(m)
			var inner string
			if json.Unmarshal([]byte(sub[2]), &inner) != nil {
				return sub[1] + ": <name>"
			}
			return sub[1] + ": " + strings.ReplaceAll(strings.ReplaceAll(scrub(inner, depth+1), `"`, ""), "'", "")
		})
	}
	s = emailRe.ReplaceAllString(s, "<email>")
	s = urlRe.ReplaceAllString(s, "<host>")
	s = quotedRe.ReplaceAllString(s, "<name>")
	s = uuidRe.ReplaceAllString(s, "<id>")
	s = winPathRe.ReplaceAllString(s, "<path>")
	s = unixPath.ReplaceAllString(s, "$1<path>")
	s = relPath.ReplaceAllStringFunc(s, func(m string) string {
		t := strings.TrimSpace(m)
		if pathWords[strings.ToLower(strings.TrimRight(t, ".,:;"))] || len(t) < 6 {
			return m
		}
		return m[:len(m)-len(t)] + "<path>"
	})
	s = ipv4Re.ReplaceAllString(s, "<host>")
	s = ipv6Re.ReplaceAllStringFunc(s, func(m string) string {
		if looksIPv6(m) {
			return "<host>"
		}
		return m
	})
	s = hostRe.ReplaceAllStringFunc(s, func(m string) string {
		if isPlaceholderAdjacent(m) {
			return m
		}
		if i := strings.LastIndex(m, "."); i >= 0 && fileExt[strings.ToLower(m[i+1:])] {
			return "<path>" // a file name, which can be as telling as a path
		}
		return "<host>"
	})
	s = hostPort.ReplaceAllString(s, "<host>")
	s = tokenRe.ReplaceAllStringFunc(s, func(m string) string {
		if tokenLike(m) {
			return "<secret>"
		}
		return m
	})
	s = hexRe.ReplaceAllStringFunc(s, func(m string) string {
		h := strings.TrimPrefix(strings.ToLower(m), "0x")
		if len(h) >= 8 || strings.HasPrefix(strings.ToLower(m), "0x") {
			return "<id>"
		}
		return m
	})
	s = digitsRe.ReplaceAllString(s, "<id>")
	s = glued.ReplaceAllString(s, "<$1>") // pk-lf-<id>: the prefix goes too
	for range 3 {                         // "as role X": the second pass reaches X
		s = keywordRe.ReplaceAllStringFunc(s, keyword)
	}
	return s
}

// keyword replaces the identifier after a keyword ("as role X" names X).
func keyword(m string) string {
	sub := keywordRe.FindStringSubmatch(m)
	word := sub[5]
	lw := strings.ToLower(strings.TrimRight(word, ".!?"))
	if strings.HasPrefix(word, "<") || keywordKeep[lw] || isCode(word) {
		return m
	}
	return sub[1] + sub[2] + sub[3] + sub[4] + "<name>"
}

// isCode reports whether w is an error code or a number, which is kept:
// 28P01, 403, 390100.
func isCode(w string) bool {
	w = strings.TrimRight(w, ".!?")
	if w == "" || len(w) > 6 {
		return false
	}
	digits := 0
	for _, r := range w {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return digits > 0
}

func isPlaceholderAdjacent(m string) bool {
	// Version numbers like "v1.2" or "16.2" never reach here (they need a
	// letter label at the end); short words like "e.g" do: keep them.
	return len(m) <= 4 || strings.EqualFold(m, "e.g") || strings.EqualFold(m, "i.e")
}

// looksIPv6 reports whether m is an IPv6 address, maybe bracketed, with
// a zone or a port.
func looksIPv6(m string) bool {
	t := m
	if i := strings.LastIndex(t, "]:"); i >= 0 {
		t = t[:i+1]
	}
	t = strings.Trim(t, "[]")
	if i := strings.Index(t, "%"); i >= 0 {
		t = t[:i]
	}
	ip := net.ParseIP(t)
	return ip != nil && strings.Contains(t, ":")
}

// tokenLike reports whether a 20+ character run looks like a secret or an
// opaque id rather than a word: it holds a digit, mixes cases inside, or is
// long and without separators.
func tokenLike(m string) bool {
	if !utf8.ValidString(m) {
		return true
	}
	hasDigit, hasUpper, hasLower := false, false, false
	for _, r := range m {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		}
	}
	if strings.Contains(m, "<") {
		return false
	}
	if hasDigit {
		return true
	}
	if hasUpper && hasLower && !strings.ContainsAny(m, "_-./") {
		return true
	}
	return len(m) >= 40
}
