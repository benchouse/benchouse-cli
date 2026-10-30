package omni

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/files"
)

// Check requests: one conversation (pageSize=1), whose pageInfo.totalRecords
// counts every conversation the key sees, and the userId probe that tells an
// Organization API key from a personal token.

const checkPath = conversationsAPI + "?pageSize=1"

// count is the cheap authenticated call shared by Check and Verify.
func count(ctx context.Context, cl *client) (total *int64, newest string, err error) {
	var p listPage
	if err := cl.get(ctx, checkPath, &p); err != nil {
		return nil, "", err
	}
	if it := p.items(); len(it) > 0 {
		newest = it[0].UpdatedAt
	}
	if p.PageInfo.TotalRecords == nil && len(p.items()) == 0 {
		var zero int64
		return &zero, "", nil
	}
	return p.PageInfo.TotalRecords, newest, nil
}

// verifyKey checks the API key with one authenticated call, for its field.
func verifyKey(ctx context.Context, in agent.Inputs) (string, error) {
	cl, err := newClient(in)
	if err != nil {
		return "", err
	}
	total, _, err := count(ctx, cl)
	if err != nil {
		return "", err
	}
	fact := "Key works: connected to Omni at " + cl.base
	if total != nil {
		fact += fmt.Sprintf(", %s AI conversations visible", agent.Thousands(*total))
	}
	return fact, nil
}

func check(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	var res agent.CheckResult
	cl, err := newClient(in)
	if err != nil {
		return res, err
	}
	total, newest, err := count(ctx, cl)
	if err != nil {
		return res, err
	}
	res.Facts = append(res.Facts, "Connected to Omni at "+cl.base)
	org, known, err := scope(ctx, cl)
	switch {
	case err != nil && !known:
		res.Warnings = append(res.Warnings, "could not tell whether the key is an Organization API key: "+agent.Clip(err))
	case org:
		res.Facts = append(res.Facts, "Organization API key: every user's AI conversations are readable")
	default:
		res.Warnings = append(res.Warnings, "this is a personal access token: only its own user's conversations will be read. "+
			"Use an Organization API key (Settings > API keys) to read everyone's")
	}
	switch {
	case total == nil:
		res.Facts = append(res.Facts, "Omni reported no conversation count: conversations are counted while collecting")
	case *total == 0:
		res.Warnings = append(res.Warnings, "no AI conversations are visible to this key")
	case opts.Since.IsZero():
		res.Facts = append(res.Facts, agent.Conversations(*total, "AI conversation", "AI conversations", opts))
		res.Conversations = total
	default:
		f := agent.Counted(*total, "AI conversation", "AI conversations", agent.Options{}) +
			": Omni has no date filter, so those active " + agent.Window(opts) + " are counted while collecting"
		if t, ok := parseTime(newest); ok && t.Before(opts.Since) {
			res.Warnings = append(res.Warnings, "no conversation was active "+agent.Window(opts)+" (the most recent was "+t.Format("2006-01-02")+"): widen --since")
		}
		res.Facts = append(res.Facts, f)
	}
	if total != nil && *total > 0 {
		res.Facts = append(res.Facts, "generated queries are read for answers from the last 14 days only: Omni keeps job results that long")
	}
	return res, nil
}

// keepContext keeps the Omni model files: YAML, and the extensionless
// .view / .topic / .composite_topic names Omni uses.
func keepContext(rel string) bool {
	l := strings.ToLower(rel)
	for _, ext := range []string{".yaml", ".yml", ".view", ".topic", ".composite_topic"} {
		if strings.HasSuffix(l, ext) {
			return true
		}
	}
	b := path.Base(l)
	return b == "model" || b == "relationships"
}

// checkContext confirms dir holds an Omni model: a model or relationships
// file, or view or topic files.
func checkContext(dir string) (agent.CheckResult, error) {
	var res agent.CheckResult
	sum, err := files.Scan(dir, keepContext)
	if err != nil {
		return res, err
	}
	if err := sum.Limits(dir); err != nil {
		return res, err
	}
	var views, topics int
	model, rels := false, false
	for _, rel := range sum.Kept {
		b := strings.ToLower(path.Base(rel))
		stem := strings.TrimSuffix(strings.TrimSuffix(b, ".yaml"), ".yml")
		switch {
		case stem == "model":
			model = true
		case stem == "relationships":
			rels = true
		case strings.HasSuffix(stem, ".view"):
			views++
		case strings.HasSuffix(stem, ".topic"), strings.HasSuffix(stem, ".composite_topic"):
			topics++
		}
	}
	if !model && !rels && views == 0 && topics == 0 {
		return res, fmt.Errorf("no Omni model in %s: expected model.yaml, relationships.yaml, *.view.yaml or *.topic.yaml files", dir)
	}
	fact := fmt.Sprintf("Omni model: %d views, %d topics", views, topics)
	if model {
		fact += ", model file"
	}
	if rels {
		fact += ", relationships file"
	}
	res.Facts = append(res.Facts, fact, fmt.Sprintf("%d files (%s) will be included", len(sum.Kept), agent.Bytes(sum.KeptBytes)))
	if !model {
		res.Warnings = append(res.Warnings, "no model file: the model's defaults and access rules will be missing")
	}
	return res, nil
}
