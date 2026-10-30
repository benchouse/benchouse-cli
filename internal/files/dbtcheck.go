package files

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// now is replaced in tests.
var now = time.Now

// staleManifest is the age past which a compiled manifest is called old.
const staleManifest = 30 * 24 * time.Hour

// maxYAMLCheck bounds a YAML file read for counting; larger ones are left
// uncounted (they are still included).
const maxYAMLCheck = 4 << 20

type dbtProject struct {
	Name        string   `yaml:"name"`
	Version     any      `yaml:"version"`
	ModelPaths  []string `yaml:"model-paths"`
	SourcePaths []string `yaml:"source-paths"` // before dbt 1.0
	MacroPaths  []string `yaml:"macro-paths"`
}

// CheckDBT verifies dir is a dbt project and says what it holds: the
// project's name and version, its models, sources, semantic models and
// metrics, whether a compiled manifest is there and how old it is, and what
// the bundle would include. It reads only dbt_project.yml and the project's
// YAML files; profiles.yml is never opened.
func CheckDBT(dir string) (agent.CheckResult, error) {
	var res agent.CheckResult
	info, err := os.Stat(dir)
	if err != nil {
		return res, err
	}
	if !info.IsDir() {
		return res, fmt.Errorf("%s is not a folder", dir)
	}
	projectFile := filepath.Join(dir, "dbt_project.yml")
	if _, err := os.Stat(projectFile); err != nil {
		found := findBelow(dir, "dbt_project.yml", 2)
		if len(found) == 1 {
			return res, fmt.Errorf("no dbt_project.yml in %s; did you mean %s?", dir, filepath.Join(dir, filepath.FromSlash(found[0])))
		}
		msg := "not a dbt project: no dbt_project.yml in " + dir
		if len(found) > 1 {
			msg += fmt.Sprintf(" (found %d projects below it: %s; point at one)", len(found), strings.Join(found, ", "))
		}
		return res, errors.New(msg)
	}
	raw, err := readBounded(projectFile, maxYAMLCheck)
	if err != nil {
		return res, fmt.Errorf("read dbt_project.yml: %w", err)
	}
	var p dbtProject
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return res, fmt.Errorf("dbt_project.yml is not valid YAML: %v", err)
	}
	if strings.TrimSpace(p.Name) == "" {
		return res, errors.New("dbt_project.yml has no name: is this a dbt project?")
	}
	head := "dbt project " + p.Name
	if v := strings.TrimSpace(fmt.Sprint(p.Version)); p.Version != nil && v != "" {
		head += " (version " + v + ")"
	}
	res.Facts = append(res.Facts, head)

	modelPaths := cleanPaths(p.ModelPaths)
	if len(modelPaths) == 0 {
		modelPaths = cleanPaths(p.SourcePaths)
	}
	if len(modelPaths) == 0 {
		modelPaths = []string{"models"}
	}
	macroPaths := cleanPaths(p.MacroPaths)
	if len(macroPaths) == 0 {
		macroPaths = []string{"macros"}
	}

	sum, err := Scan(dir, DBTKeep)
	if err != nil {
		return res, err
	}
	if err := sum.Limits(dir); err != nil {
		return res, err
	}

	// Models are .sql and .py files under the model paths; .py is not kept
	// in the bundle but still counts as a model.
	var models, macros int
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(path.Ext(rel))
		switch {
		case under(rel, modelPaths) && (ext == ".sql" || ext == ".py"):
			models++
		case under(rel, macroPaths) && ext == ".sql":
			macros++
		}
		return nil
	})
	res.Facts = append(res.Facts, fmt.Sprintf("%s models in %s, %s macro files",
		agent.Thousands(int64(models)), strings.Join(modelPaths, ", "), agent.Thousands(int64(macros))))

	var sources, sourceTables, semantic, metrics, documented, unparsed int
	for _, rel := range sum.Kept {
		ext := strings.ToLower(path.Ext(rel))
		if (ext != ".yml" && ext != ".yaml") || strings.HasPrefix(rel, "target/") || notSchemaYAML(rel) {
			continue
		}
		b, err := readBounded(filepath.Join(dir, filepath.FromSlash(rel)), maxYAMLCheck)
		if err != nil {
			continue
		}
		var doc map[string]any
		if err := yaml.Unmarshal(b, &doc); err != nil {
			unparsed++
			continue
		}
		for _, s := range list(doc["sources"]) {
			sources++
			if m, ok := s.(map[string]any); ok {
				sourceTables += len(list(m["tables"]))
			}
		}
		semantic += len(list(doc["semantic_models"]))
		metrics += len(list(doc["metrics"]))
		documented += len(list(doc["models"]))
	}
	res.Facts = append(res.Facts, fmt.Sprintf("%s sources (%s tables), %s documented models, %s semantic models, %s metrics",
		agent.Thousands(int64(sources)), agent.Thousands(int64(sourceTables)), agent.Thousands(int64(documented)),
		agent.Thousands(int64(semantic)), agent.Thousands(int64(metrics))))
	if unparsed > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d YAML files could not be parsed (Jinja in YAML?) and were not counted; they are still included", unparsed))
	}

	if st, err := os.Stat(filepath.Join(dir, "target", "manifest.json")); err == nil {
		age := now().Sub(st.ModTime())
		res.Facts = append(res.Facts, fmt.Sprintf("compiled manifest target/manifest.json from %s (%s)",
			st.ModTime().UTC().Format("2006-01-02"), agoText(age)))
		if age > staleManifest {
			res.Warnings = append(res.Warnings, fmt.Sprintf("the compiled manifest is %d days old: run `dbt compile` to refresh it", int(age.Hours()/24)))
		}
	} else {
		res.Warnings = append(res.Warnings, "no compiled manifest: run `dbt compile` for richer findings")
	}
	if _, err := os.Stat(filepath.Join(dir, "target", "semantic_manifest.json")); err == nil {
		res.Facts = append(res.Facts, "semantic manifest target/semantic_manifest.json present")
	}
	for _, c := range sum.Credentials {
		if b := strings.ToLower(path.Base(c)); b == "profiles.yml" || b == "profiles.yaml" {
			res.Warnings = append(res.Warnings, c+" is present: it holds connection credentials and is never read or included")
		}
	}
	res.Facts = append(res.Facts, fmt.Sprintf("%s files (%s) will be included; %s left out",
		agent.Thousands(int64(len(sum.Kept))), agent.Bytes(sum.KeptBytes), agent.Thousands(int64(sum.Left))))
	if len(sum.TooLarge) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d files are larger than %d MiB and will be left out, e.g. %s",
			len(sum.TooLarge), MaxFileBytes>>20, sum.TooLarge[0]))
	}
	return res, nil
}

// notSchemaYAML names project YAML that never holds sources, models or
// metrics.
func notSchemaYAML(rel string) bool {
	switch strings.ToLower(rel) {
	case "dbt_project.yml", "packages.yml", "dependencies.yml", "selectors.yml", "package-lock.yml":
		return true
	}
	return false
}

func cleanPaths(ps []string) []string {
	var out []string
	for _, p := range ps {
		p = strings.Trim(path.Clean("/"+filepath.ToSlash(strings.TrimSpace(p))), "/")
		if p != "" && p != "." {
			out = append(out, p)
		}
	}
	return out
}

func under(rel string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func agoText(d time.Duration) string {
	switch days := int(d.Hours() / 24); {
	case d < time.Hour:
		return "less than an hour old"
	case days < 1:
		return fmt.Sprintf("%d hours old", int(d.Hours()))
	case days == 1:
		return "1 day old"
	default:
		return fmt.Sprintf("%d days old", days)
	}
}

// readBounded reads at most max bytes of a regular file, refusing a larger
// one.
func readBounded(p string, max int64) ([]byte, error) {
	f, err := openNoFollow(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d MiB", p, max>>20)
	}
	return b, nil
}

// ReadBounded is readBounded for the agents' context checks: at most max
// bytes of a regular file, never through a symlink where the platform can
// refuse one.
func ReadBounded(p string, max int64) ([]byte, error) { return readBounded(p, max) }

// findBelow lists the paths (relative, slash-separated) of files called name
// one to depth folders below dir, skipping folders that never hold project
// source.
func findBelow(dir, name string, depth int) []string {
	var out []string
	var walk func(d, rel string, level int)
	walk = func(d, rel string, level int) {
		if level > depth {
			return
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || skipDir(e.Name()) || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			sub := path.Join(rel, e.Name())
			if st, err := os.Lstat(filepath.Join(d, e.Name(), name)); err == nil && st.Mode().IsRegular() {
				out = append(out, sub)
			}
			walk(filepath.Join(d, e.Name()), sub, level+1)
		}
	}
	walk(dir, "", 1)
	sort.Strings(out)
	return out
}

// FindBelow lists the folders one to depth levels below dir that hold a
// file called name, as slash-separated paths relative to dir.
func FindBelow(dir, name string, depth int) []string { return findBelow(dir, name, depth) }
