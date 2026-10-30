package nao

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/files"
)

// checkContext verifies dir is a nao context folder (the folder `nao init`
// ran in) and says what it holds.
func checkContext(dir string) (agent.CheckResult, error) {
	var res agent.CheckResult
	st, err := os.Stat(dir)
	if err != nil {
		return res, err
	}
	if !st.IsDir() {
		return res, fmt.Errorf("%s is not a folder", dir)
	}
	isFile := func(rel string) bool {
		fi, err := os.Lstat(filepath.Join(dir, rel))
		return err == nil && fi.Mode().IsRegular()
	}
	isDir := func(rel string) bool {
		fi, err := os.Lstat(filepath.Join(dir, rel))
		return err == nil && fi.IsDir()
	}
	rules, config, databases := isFile("RULES.md"), isFile("nao_config.yaml"), isDir("databases")
	if !rules && !config && !databases {
		msg := "not a nao context folder: expected RULES.md, nao_config.yaml or databases/ in " + dir +
			" (point at the folder you ran `nao init` in)"
		if found := files.FindBelow(dir, "nao_config.yaml", 2); len(found) == 1 {
			msg += "; did you mean " + filepath.Join(dir, filepath.FromSlash(found[0])) + "?"
		}
		return res, fmt.Errorf("%s", msg)
	}

	sum, err := files.Scan(dir, keepContext)
	if err != nil {
		return res, err
	}
	if err := sum.Limits(dir); err != nil {
		return res, err
	}
	samples, err := files.Scan(dir, dataSample)
	if err != nil {
		return res, err
	}
	tables := map[string]bool{}
	var semantic, agentDocs int
	for _, rel := range sum.Kept {
		switch {
		case strings.HasPrefix(rel, "databases/"):
			tables[path.Dir(rel)] = true
		case strings.HasPrefix(rel, "semantics/"):
			semantic++
		case strings.HasPrefix(rel, "agent/"):
			agentDocs++
		}
	}

	var present []string
	if rules {
		present = append(present, "RULES.md")
	} else {
		res.Warnings = append(res.Warnings, "no RULES.md: the agent's rules will be missing from the findings")
	}
	if config {
		present = append(present, "nao_config.yaml (secrets masked)")
	}
	if len(present) > 0 {
		res.Facts = append(res.Facts, strings.Join(present, " and ")+" present")
	}
	switch {
	case !databases:
		res.Warnings = append(res.Warnings, "no databases/ folder: run `nao sync` so the table docs are included")
	case len(tables) == 0 && len(samples.Kept) > 0:
		res.Warnings = append(res.Warnings, fmt.Sprintf("databases/ holds only sample-data files (%d preview.md/profiling.md), "+
			"which are left out without --include-results: run `nao sync` for the column docs", len(samples.Kept)))
	case len(tables) == 0:
		res.Warnings = append(res.Warnings, "databases/ has no table docs: run `nao sync`")
	default:
		res.Facts = append(res.Facts, fmt.Sprintf("%s table docs under databases/", agent.Thousands(int64(len(tables)))))
	}
	if semantic > 0 {
		res.Facts = append(res.Facts, fmt.Sprintf("%s semantic files under semantics/", agent.Thousands(int64(semantic))))
	}
	if agentDocs > 0 {
		res.Facts = append(res.Facts, fmt.Sprintf("%s agent docs under agent/", agent.Thousands(int64(agentDocs))))
	}
	if len(samples.Kept) > 0 && len(tables) > 0 {
		res.Facts = append(res.Facts, fmt.Sprintf("%s sample-data files left out (kept only with --include-results)",
			agent.Thousands(int64(len(samples.Kept)))))
	}
	res.Facts = append(res.Facts, fmt.Sprintf("%s files (%s) will be included", agent.Thousands(int64(len(sum.Kept))), agent.Bytes(sum.KeptBytes)))
	return res, nil
}
