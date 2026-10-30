package files

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// RedactConfig blanks the secrets in a YAML file such as nao_config.yaml or a
// dbt schema file: the values under credential-like keys go, the keys stay
// ("there is a password" is context, the password is not).
//
// Line-based first, so a file without secrets comes out byte-for-byte
// unchanged. A value that is an environment reference ({{ env('X') }}, ${X})
// is kept: it is a name, not a secret. The result is then parsed and checked
// by key, which catches a value on the next line, an inline {password: x}, a
// secret under a credentials: block, and a URL carrying a password; if that
// pass finds anything the file is re-emitted. A file that is not valid YAML
// cannot be checked by key, so none of it is kept.
func RedactConfig(b []byte) []byte {
	lined := redactLines(b)
	docs, err := parseAll(lined)
	if err != nil {
		return []byte(unreadable)
	}
	changed := false
	for _, d := range docs {
		changed = scrub(d, false) || changed
	}
	if !changed {
		return lined
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			return []byte(unreadable)
		}
	}
	_ = enc.Close()
	return out.Bytes()
}

const unreadable = "# [redacted: this file is not valid YAML, so its secrets cannot be found and none of it is kept]\n"

func parseAll(b []byte) ([]*yaml.Node, error) {
	var docs []*yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, &n)
	}
}

const redacted = "[redacted]"

// scrub withholds every scalar under a secret key, and the userinfo of any
// URL anywhere. It reports whether it changed anything.
func scrub(n *yaml.Node, secret bool) bool {
	changed := false
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			changed = scrub(c, secret) || changed
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			v := n.Content[i+1]
			match, strong := isSecretKey(n.Content[i].Value)
			// A weak key (customer_key, auth) withholds a scalar, not a block:
			// in a data model it names a column with its documentation below.
			if match && !strong && v.Kind != yaml.ScalarNode && v.Kind != yaml.AliasNode {
				match = false
			}
			changed = scrub(n.Content[i], false) || changed // a key can hold a token too, even a complex key
			changed = scrub(v, secret || match) || changed
		}
	case yaml.AliasNode:
		// An alias under a secret key points at a value set elsewhere, under
		// a name that may say nothing.
		if secret {
			*n = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.DoubleQuotedStyle, Value: redacted}
			changed = true
		}
	case yaml.ScalarNode:
		switch v := n.Value; {
		case secret && v != "" && v != redacted && maskEnvDefaults(v, true) != v:
			n.Value = maskEnvDefaults(v, true)
			changed = true
		case secret && v != "" && v != redacted && !isEnvRef(v) && n.Tag != "!!null" && n.Tag != "!!bool":
			n.Value, n.Tag, n.Style = redacted, "!!str", yaml.DoubleQuotedStyle
			changed = true
		case urlUserinfo.MatchString(v):
			n.Value = urlUserinfo.ReplaceAllString(v, "${1}"+redacted+"@")
			changed = true
		}
		changed = maskScalar(n) || changed
	}
	return changed
}

// escapable reports whether the encoder would write v with a letter escape
// (\t, \n, \x85, \u2028...): a control or unprintable character.
func escapable(v string) bool {
	return strings.IndexFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f || !unicode.IsPrint(r) }) >= 0
}

// maskScalar masks form-recognisable secrets in a scalar's decoded value.
// The text scan after re-emitting cannot be relied on for them: the encoder
// escapes a tab or newline before a token as \t or \n, gluing it to a
// letter, and a token rule's word boundary no longer matches.
func maskScalar(n *yaml.Node) bool {
	if n.Kind != yaml.ScalarNode || !escapable(n.Value) {
		return false // re-emitted as written: the text scan sees it
	}
	m := maskTokens(n.Value)
	if m == n.Value {
		return false
	}
	n.Value = m
	return true
}

// urlUserinfo is a URL's user:password@ (or token@), which a DSN carries.
var urlUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s@'"]+@`)

func redactLines(b []byte) []byte {
	lines := strings.Split(string(b), "\n")
	out := make([]string, 0, len(lines))
	blockIndent := -1 // >= 0 while skipping a redacted block scalar's body
	for _, line := range lines {
		if blockIndent >= 0 {
			if strings.TrimSpace(line) == "" || indentOf(line) > blockIndent {
				continue
			}
			blockIndent = -1
		}
		line = urlUserinfo.ReplaceAllString(line, "${1}"+redacted+"@")
		m := matchSecretLine(line)
		if m == nil {
			out = append(out, line)
			continue
		}
		prefix, value := m[1], strings.TrimSpace(m[3])
		switch {
		case value == "" || isEnvRef(value):
			out = append(out, line)
		case maskEnvDefaults(value, true) != value:
			out = append(out, prefix+maskEnvDefaults(m[3], true))
		case value == "|" || value == ">" || strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">"):
			out = append(out, prefix+`"[redacted]"`)
			blockIndent = indentOf(line)
		default:
			out = append(out, prefix+`"[redacted]"`)
		}
	}
	return []byte(strings.Join(out, "\n"))
}

// secretLine matches `key: value`. Group 1 is everything up to and including
// the colon and a space, so the replacement keeps indentation and list
// markers; group 2 is the key, group 3 the value.
var secretLine = regexp.MustCompile(`(?i)^(\s*(?:-\s+)?["']?([a-z0-9_.-]+)["']?\s*:\s?)(.*)$`)

// matchSecretLine returns secretLine's groups when the key names a
// credential. A weak key (license_key) counts only with a value on the line:
// with none, a block of column documentation follows.
func matchSecretLine(line string) []string {
	m := secretLine.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	match, strong := isSecretKey(m[2])
	if !match || (!strong && strings.TrimSpace(m[3]) == "") {
		return nil
	}
	return m
}

func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t-"))
}
