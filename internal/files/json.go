package files

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
)

// maskJSON masks a JSON file by key, with the same rules as YAML: values
// under a credential-like key are replaced, URL credentials anywhere are
// masked. The file is edited in place, byte ranges only, so key order and
// formatting survive and a file without secrets comes out unchanged. A file
// that does not parse (JSON with comments, a template) gets the line-based
// scan instead.
//
// dbt's target/manifest.json is kept: compiled_code is the SQL the agent's
// warehouse actually ran, worth having. It is rendered, though, so an
// env_var() in raw_code appears there as its value. Where raw_code reads a
// credential-named variable (env_var('DBT_PASSWORD')) the node's compiled
// code is replaced whole; everywhere else it goes through the token scan
// like any other text.
func maskJSON(raw []byte) []byte {
	edits, err := jsonEdits(raw)
	if err != nil {
		return raw // the token scan that follows handles quoted assignments
	}
	if len(edits) == 0 {
		return raw
	}
	var out bytes.Buffer
	last := 0
	for _, e := range edits {
		out.Write(raw[last:e.from])
		out.WriteString(e.with)
		last = e.to
	}
	out.Write(raw[last:])
	return out.Bytes()
}

type edit struct {
	from, to int
	with     string
}

type jsonFrame struct {
	obj     bool
	secret  bool // everything below is withheld
	wantKey bool
	key     string
	// names: the object's keys are names (of columns, tables, dbt nodes),
	// not settings, so a column called access_token is not a credential.
	names bool
	// A dbt node: raw_code that reads a credential-named env_var, and where
	// its compiled_code sits. The compiled code holds the rendered value.
	rawSecretEnv bool
	compiled     *edit
}

// compiledFromSecret stands in for compiled code rendered from a secret.
const compiledFromSecret = `"[redacted: compiled with the value of a credential env_var; raw_code shows the source]"`

// envVarName finds env_var('NAME') where NAME names a credential.
var envVarName = regexp.MustCompile(`env(?:_var)?\(\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]`)

func readsSecretEnv(code string) bool {
	for _, m := range envVarName.FindAllStringSubmatch(code, -1) {
		if secret, _ := isSecretKey(m[1]); secret {
			return true
		}
	}
	return false
}

var nameMaps = map[string]bool{
	"columns": true, "fields": true, "tables": true, "nodes": true, "sources": true, "macros": true,
	"metrics": true, "semantic_models": true, "exposures": true, "indexes": true, "child_map": true, "parent_map": true,
	"uniqueconstraints": true, "foreignkeys": true, "compositeprimarykeys": true, "checkconstraints": true,
}

// jsonEdits walks the token stream and returns, in order, the byte ranges of
// the scalar values to replace.
func jsonEdits(raw []byte) ([]edit, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var (
		stack []*jsonFrame
		edits []edit
	)
	// valueCtx says whether the value about to be read sits under a secret
	// key, and whether that key is a strong one.
	valueCtx := func() (secret, strong bool) {
		if len(stack) == 0 {
			return false, false
		}
		top := stack[len(stack)-1]
		if top.secret {
			return true, true
		}
		if top.obj && !top.names {
			return isSecretKey(top.key)
		}
		return false, false
	}
	afterValue := func() {
		if len(stack) > 0 && stack[len(stack)-1].obj {
			stack[len(stack)-1].wantKey = true
		}
	}
	for {
		start := int(dec.InputOffset())
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			if len(stack) != 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return tidy(edits), nil
		}
		if err != nil {
			return nil, err
		}
		end := int(dec.InputOffset())
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				secret, strong := valueCtx()
				f := &jsonFrame{obj: d == '{', secret: secret && strong, wantKey: d == '{'}
				if n := len(stack); n > 0 && stack[n-1].obj && d == '{' {
					f.names = nameMaps[strings.ToLower(stack[n-1].key)]
				}
				stack = append(stack, f)
			case '}', ']':
				if len(stack) == 0 {
					return nil, errors.New("unbalanced JSON")
				}
				if f := stack[len(stack)-1]; f.rawSecretEnv && f.compiled != nil {
					edits = append(edits, *f.compiled)
				}
				stack = stack[:len(stack)-1]
				afterValue()
			}
			continue
		}
		if len(stack) > 0 {
			if top := stack[len(stack)-1]; top.obj && top.wantKey {
				s, _ := tok.(string)
				top.key, top.wantKey = s, false
				continue
			}
		}
		secret, _ := valueCtx()
		from := start + len(raw[start:end]) - len(bytes.TrimLeft(raw[start:end], " \t\r\n:,"))
		switch v := tok.(type) {
		case string:
			if top := topObj(stack); top != nil {
				switch top.key {
				case "raw_code", "raw_sql":
					top.rawSecretEnv = top.rawSecretEnv || readsSecretEnv(v)
				case "compiled_code", "compiled_sql":
					top.compiled = &edit{from, end, compiledFromSecret}
				}
			}
			switch {
			case secret && v != "" && literal(v):
				edits = append(edits, edit{from, end, `"` + redacted + `"`})
			case secret && v != "" && maskEnvDefaults(v, true) != v:
				edits = append(edits, edit{from, end, quoteJSON(maskEnvDefaults(v, true))})
			case urlUserinfo.MatchString(v):
				edits = append(edits, edit{from, end, quoteJSON(urlUserinfo.ReplaceAllString(v, "${1}"+redacted+"@"))})
			}
		case json.Number:
			if secret {
				edits = append(edits, edit{from, end, `"` + redacted + `"`})
			}
		}
		afterValue()
	}
}

func topObj(stack []*jsonFrame) *jsonFrame {
	if n := len(stack); n > 0 && stack[n-1].obj {
		return stack[n-1]
	}
	return nil
}

// tidy orders edits and drops any inside a wider one (a URL edit within
// compiled code that is replaced whole).
func tidy(edits []edit) []edit {
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].from != edits[j].from {
			return edits[i].from < edits[j].from
		}
		return edits[i].to > edits[j].to
	})
	out := edits[:0]
	end := -1
	for _, e := range edits {
		if e.from < end {
			continue
		}
		out = append(out, e)
		end = e.to
	}
	return out
}

func quoteJSON(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}
