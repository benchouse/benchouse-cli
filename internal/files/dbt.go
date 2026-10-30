package files

import (
	"path"
	"strings"
)

// DBTKeep is the dbt project files worth reading: models, sources, macros,
// semantic models, metrics, docs and the compiled manifest and catalog.
// profiles.yml (connection credentials), packages and logs are never kept.
func DBTKeep(rel string) bool {
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	switch {
	case strings.HasPrefix(rel, "dbt_packages/"), strings.HasPrefix(rel, "dbt_modules/"),
		strings.HasPrefix(rel, "logs/"), strings.HasPrefix(rel, ".git/"):
		return false
	case strings.HasPrefix(rel, "target/"):
		return rel == "target/manifest.json" || rel == "target/catalog.json" || rel == "target/semantic_manifest.json"
	}
	if credentialsFile(rel) {
		return false
	}
	switch b := strings.ToLower(path.Base(rel)); {
	case b == "readme.md", b == "readme", b == "readme.txt":
		return true
	}
	switch strings.ToLower(path.Ext(rel)) {
	case ".yml", ".yaml", ".sql", ".md":
		return true
	}
	return false
}
