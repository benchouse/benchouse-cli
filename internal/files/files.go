// Package files copies a project folder (a dbt project, an agent's context
// folder) into a bundle: only the files an agent's Keep rule names, with
// credentials files left out and secrets inside kept files masked.
package files

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Limits on what one folder may contribute.
const (
	MaxFileBytes  = 64 << 20
	MaxTotalBytes = 256 << 20
	MaxFiles      = 50000
)

// File is one kept file, ready for the bundle.
type File struct {
	// Rel is the path relative to the folder, with forward slashes.
	Rel string
	// Data is the masked content. From Walk it is only valid during the
	// callback.
	Data   []byte
	SHA256 string
	// Masked counts values replaced by [redacted].
	Masked int
}

// Skipped is one file left out, and why.
type Skipped struct {
	Rel    string
	Reason string
}

// Read walks root and returns the files keep accepts, masked, in path order.
// It holds every file in memory: for tests and small folders. The CLI uses
// Walk, which holds one file at a time.
func Read(root string, keep func(rel string) bool) ([]File, []Skipped, error) {
	var out []File
	skipped, err := Walk(root, keep, func(f File) error {
		f.Data = append([]byte(nil), f.Data...)
		out = append(out, f)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, skipped, nil
}

// Walk walks root and calls fn with each file keep accepts, masked, one at
// a time and in walk order (not path order: sort what fn kept). f.Data is
// only valid during the call; fn copies or writes it out. The per-file,
// total and file-count caps apply as in Read. An error from fn stops the
// walk and is returned.
func Walk(root string, keep func(rel string) bool, fn func(f File) error) ([]Skipped, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", root)
	}
	var (
		n       int
		skipped []Skipped
		total   int64
	)
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks and devices are never followed
		}
		if !keep(rel) {
			if credentialsFile(rel) {
				skipped = append(skipped, Skipped{rel, "credentials file"})
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Size() > MaxFileBytes {
			skipped = append(skipped, Skipped{rel, tooLarge})
			return nil
		}
		if n >= MaxFiles {
			return fmt.Errorf("%s has more than %d files to include; point at the project folder itself", root, MaxFiles)
		}
		raw, reason, err := readRegular(p, fi)
		if err != nil {
			return err
		}
		if reason != "" {
			skipped = append(skipped, Skipped{rel, reason})
			return nil
		}
		data, masked, reason := mask(rel, raw)
		if reason != "" {
			skipped = append(skipped, Skipped{rel, reason})
			return nil
		}
		total += int64(len(data))
		if total > MaxTotalBytes {
			return fmt.Errorf("%s holds more than %d MiB of files to include", root, MaxTotalBytes>>20)
		}
		sum := sha256.Sum256(data)
		n++
		return fn(File{Rel: rel, Data: data, SHA256: hex.EncodeToString(sum[:]), Masked: masked})
	})
	if err != nil {
		return nil, err
	}
	return skipped, nil
}

// skipDir names folders that never hold project source.
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", ".venv", "venv", "__pycache__", "dbt_packages", "dbt_modules", "logs", ".idea", ".vscode":
		return true
	}
	return false
}

// credentialsFile names files that hold secrets by design. They are never
// kept, whatever an agent's Keep rule says, and are listed as skipped so the
// user sees they were noticed.
func credentialsFile(rel string) bool {
	b := strings.ToLower(path.Base(rel))
	ext := path.Ext(b)
	switch {
	case b == "profiles.yml", b == "profiles.yaml", strings.HasPrefix(b, ".env"),
		b == "credentials.json", strings.HasSuffix(b, "-credentials.json"), b == ".netrc",
		b == ".pgpass", b == ".my.cnf", b == "secrets.yml", b == "secrets.yaml",
		b == "id_rsa", b == "id_dsa", b == "id_ed25519", b == "id_ecdsa",
		strings.HasPrefix(b, "service-account") && ext == ".json",
		strings.Contains(b, "secret") && ext == ".json":
		return true
	}
	switch ext {
	case ".pem", ".p8", ".key", ".pfx", ".p12", ".jks", ".keystore", ".tfvars", ".tfstate", ".kdbx":
		return true
	}
	// terraform.tfstate.backup, prod.tfvars.json
	return strings.Contains(b, ".tfstate") || strings.Contains(b, ".tfvars")
}

// mask returns the bytes to store, how many values were masked, and a reason
// when the file must be left out entirely.
func mask(rel string, raw []byte) ([]byte, int, string) {
	if credentialsFile(rel) {
		return nil, 0, "credentials file"
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return nil, 0, "binary file"
	}
	for _, r := range failRules {
		if r.re.Match(raw) {
			return nil, 0, secretReason + r.kind
		}
	}
	before := bytes.Count(raw, []byte(redacted))
	out := raw
	switch strings.ToLower(path.Ext(rel)) {
	case ".yml", ".yaml":
		var err error
		if out, err = maskYAML(raw); err != nil {
			return nil, 0, err.Error()
		}
	case ".json":
		out = maskJSON(raw)
	}
	// Every file, whatever its type: secrets recognisable by their form.
	masked := maskTokens(string(out))
	if kind := detectSecret(masked); kind != "" {
		return nil, 0, secretReason + kind // masking missed it: fail closed
	}
	// A re-emitted YAML or JSON file writes a tab or newline before a token
	// as \t or \n, gluing it to a letter; look again with the escapes
	// set apart, so such a token fails closed too.
	if !strings.Contains(masked, `\`) {
		return []byte(masked), strings.Count(masked, redacted) - before, ""
	}
	if kind := detectSecret(escapeSeq.ReplaceAllString(masked, "$0 ")); kind != "" {
		return nil, 0, secretReason + kind
	}
	return []byte(masked), strings.Count(masked, redacted) - before, ""
}

// escapeSeq is an escape sequence YAML and JSON encoders write.
var escapeSeq = regexp.MustCompile(`\\(?:[0abtnvfreN_LP"/\\ ]|x[0-9A-Fa-f]{2}|u[0-9A-Fa-f]{4}|U[0-9A-Fa-f]{8})`)

const secretReason = "contains what looks like a secret: "

// maskYAML masks a YAML file. dbt YAML often carries Jinja that is not valid
// YAML; such a file is kept after the line rule unless it also mentions a
// credential-like key, in which case it cannot be checked and is left out.
func maskYAML(raw []byte) ([]byte, error) {
	lined := redactLines(raw)
	if _, err := parseAll(lined); err != nil {
		if mentionsSecretKey(lined) && bytes.Contains(lined, []byte(":")) && !onlyRedacted(lined) {
			return nil, errors.New("not valid YAML and mentions credentials, so it cannot be checked")
		}
		return lined, nil
	}
	return RedactConfig(raw), nil
}

// onlyRedacted reports whether every credential-like key in b already has a
// masked or environment-reference value, i.e. the line rule handled them all.
// A key with an empty value followed by more-indented lines holds its value
// on the next line, which the line rule cannot see.
func onlyRedacted(b []byte) bool {
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		m := matchSecretLine(line)
		if m == nil {
			continue
		}
		v := strings.TrimSpace(m[3])
		switch {
		case v == `"`+redacted+`"` || isEnvRef(v):
		case v == "":
			for _, next := range lines[i+1:] {
				if strings.TrimSpace(next) == "" {
					continue
				}
				if indentOf(next) > indentOf(line) {
					return false
				}
				break
			}
		default:
			return false
		}
	}
	return true
}
