package langfuse

import (
	"context"
	"fmt"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// Check requests: the project the key pair belongs to, and one observation
// of the first page Collect would read. The v2 observations endpoint pages
// by cursor and reports no total, so the window's size is not counted.

const projectsPath = "/api/public/projects"

func checkPage(in agent.Inputs, opts agent.Options) string {
	v := firstQuery(in, opts)
	v.Set("limit", "1")
	v.Set("fields", "core,basic")
	return endpoint + "?" + v.Encode()
}

// project is the check's first request: the project the key pair belongs
// to. Only refused keys are an error; a project that cannot be named is a
// warning.
func project(ctx context.Context, cl client) (fact, warning string, err error) {
	var projects struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	fact = "Connected to Langfuse at " + cl.base
	if err := cl.get(ctx, projectsPath, &projects); err != nil {
		if strings.Contains(err.Error(), "rejected the API keys") {
			return "", "", err
		}
		return fact, "could not name the project: " + agent.Clip(err), nil
	}
	if len(projects.Data) > 0 && projects.Data[0].Name != "" {
		fact += ", project " + projects.Data[0].Name
	}
	return fact, "", nil
}

// verifyKeys checks the key pair with one authenticated call, for the
// secret key's field.
func verifyKeys(ctx context.Context, in agent.Inputs) (string, error) {
	cl, err := newClient(in)
	if err != nil {
		return "", err
	}
	fact, warning, err := project(ctx, cl)
	if warning != "" {
		fact += " (" + warning + ")"
	}
	return fact, err
}

// keyValidator refuses a key that looks like its pair's other half (a
// pasted secret key in the public key field), without echoing it.
func keyValidator(want, other, otherName string) func(string) error {
	return func(v string) error {
		if err := agent.Token(v); err != nil {
			return err
		}
		if strings.HasPrefix(v, other) {
			return fmt.Errorf("that looks like the %s key (%s...): this one starts with %s", otherName, other, want)
		}
		return nil
	}
}

func check(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	var res agent.CheckResult
	cl, err := newClient(in)
	if err != nil {
		return res, err
	}
	fact, warning, err := project(ctx, cl)
	if err != nil {
		return res, err
	}
	res.Facts = append(res.Facts, fact)
	if warning != "" {
		res.Warnings = append(res.Warnings, warning)
	}

	var p struct {
		Data []struct {
			StartTime string `json:"startTime"`
			TraceName string `json:"traceName"`
			Name      string `json:"name"`
		} `json:"data"`
	}
	if err := cl.get(ctx, checkPage(in, opts), &p); err != nil {
		if !isNotFound(err) {
			return res, err
		}
		return checkLegacy(ctx, cl, in, opts, res)
	}
	if len(p.Data) == 0 {
		w := "no observations " + agent.Window(opts)
		if name := in.Get("trace-name"); name != "" {
			w += fmt.Sprintf(" for trace name %q: check --trace-name (it must match exactly)", name)
		} else {
			w += ": widen --since, or check the keys belong to the project your agent traces to"
		}
		res.Warnings = append(res.Warnings, w)
		return res, nil
	}
	o := p.Data[0]
	fact = "newest observation " + agent.Window(opts) + ": " + o.StartTime
	if o.TraceName != "" {
		fact += " (trace " + o.TraceName + ")"
	}
	res.Facts = append(res.Facts, fact, "the number of traces is counted while collecting: Langfuse's API reports no total")
	return res, nil
}

// checkLegacy checks a Langfuse without the v2 observations API (v3 not in
// v4 write mode) with the first trace page Collect would read, limit 1;
// that endpoint counts the window's traces.
func checkLegacy(ctx context.Context, cl client, in agent.Inputs, opts agent.Options, res agent.CheckResult) (agent.CheckResult, error) {
	v := legacyQuery(in, opts)
	v.Set("limit", "1")
	v.Set("page", "1")
	v.Set("fields", "core")
	var p legacyTracePage
	if err := cl.get(ctx, legacyTraces+"?"+v.Encode(), &p); err != nil {
		return res, neither(cl, err)
	}
	res.Facts = append(res.Facts, "no v2 observations API (Langfuse v3 not in v4 write mode): traces are read with the v1 endpoints")
	if len(p.Data) == 0 {
		w := "no traces " + agent.Window(opts)
		if name := in.Get("trace-name"); name != "" {
			w += fmt.Sprintf(" for trace name %q: check --trace-name (it must match exactly)", name)
		} else {
			w += ": widen --since, or check the keys belong to the project your agent traces to"
		}
		res.Warnings = append(res.Warnings, w)
		return res, nil
	}
	fact := fmt.Sprintf("%s traces %s, newest %s", agent.Thousands(int64(p.Meta.TotalItems)), agent.Window(opts), str(p.Data[0]["timestamp"]))
	if name := str(p.Data[0]["name"]); name != "" {
		fact += " (trace " + name + ")"
	}
	res.Facts = append(res.Facts, fact)
	return res, nil
}
