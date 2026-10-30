package hex

import (
	"context"
	"fmt"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// Check requests: the token's user (GET /v1/users/me) and one page of 100
// threads with Collect's filters. ListThreads reports no total, so the
// window is counted exactly only when it fits that page.

type me struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
	Org   struct {
		ID string `json:"id"`
	} `json:"org"`
}

// whoami is the cheap authenticated call shared by Check and Verify.
func whoami(ctx context.Context, cl *client) (fact, warning string, err error) {
	var u me
	if err := cl.get(ctx, mePath, &u); err != nil {
		return "", "", err
	}
	fact = "Connected to Hex at " + cl.base
	switch {
	case u.Email != "":
		fact += " as " + u.Email
		if u.Role != "" {
			fact += " (" + u.Role + ")"
		}
	case u.ID == "":
		fact += " with a workspace token"
	}
	if u.Role != "" && u.Role != "ADMIN" && u.Role != "MANAGER" {
		warning = fmt.Sprintf("this token's user has the %s org role: listing threads needs Manager or higher (Admin to see every non-private thread)", u.Role)
	} else if u.Role == "MANAGER" {
		warning = "a Manager's token sees only the threads they created plus Threads-type threads on projects they can access; an Admin's token sees every non-private thread"
	}
	return fact, warning, nil
}

// verifyToken checks the token with one authenticated call, for the token
// field.
func verifyToken(ctx context.Context, in agent.Inputs) (string, error) {
	cl, err := newClient(in)
	if err != nil {
		return "", err
	}
	fact, warning, err := whoami(ctx, cl)
	if warning != "" {
		fact += " (" + warning + ")"
	}
	return fact, err
}

func check(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	var res agent.CheckResult
	cl, err := newClient(in)
	if err != nil {
		return res, err
	}
	fact, warning, err := whoami(ctx, cl)
	if err != nil {
		return res, err
	}
	res.Facts = append(res.Facts, fact)
	if warning != "" {
		res.Warnings = append(res.Warnings, warning)
	}
	var p threadPage
	if err := cl.get(ctx, listQuery(in, opts, threadPageSize), &p); err != nil {
		if statusOf(err) == 403 {
			return res, fmt.Errorf("%s\n\n%s", err, "Ask a Hex Admin for a token with the Manager org role or higher.")
		}
		return res, err
	}
	var inWindow, private int64
	for _, t := range p.Threads {
		created, _ := parseTime(t.CreatedAt)
		if !opts.Since.IsZero() && !created.IsZero() && created.Before(opts.Since) {
			break
		}
		if t.Sensitive || t.Private {
			private++
			continue
		}
		inWindow++
	}
	more := p.Pagination.After != nil && *p.Pagination.After != "" && int(inWindow+private) == len(p.Threads)
	if more {
		res.Facts = append(res.Facts, fmt.Sprintf("more than %s threads %s: Hex reports no total, so they are counted while collecting",
			agent.Thousands(inWindow), agent.Window(opts)))
	} else {
		res.Facts = append(res.Facts, agent.Conversations(inWindow, "thread", "threads", opts))
		res.Conversations = &inWindow
	}
	if private > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d private threads in the first page will be skipped: Hex hides them from everyone but their author", private))
	}
	if inWindow == 0 && !more {
		w := "no threads " + agent.Window(opts)
		if in.Get("source") != "" || in.Get("thread-type") != "" {
			w += " with the --source / --thread-type filters"
		}
		res.Warnings = append(res.Warnings, w+": widen --since, or check the token's user can see the agent's threads")
		return res, nil
	}
	n := inWindow
	if opts.MaxConversations > 0 && (more || n > int64(opts.MaxConversations)) {
		n = int64(opts.MaxConversations)
	}
	if n > 30 {
		// At least one messages request a thread, two seconds apart.
		res.Facts = append(res.Facts, fmt.Sprintf("Hex allows 30 API requests a minute: reading %s threads takes %s minutes or more",
			agent.Thousands(n), agent.Thousands(n/30)))
	}
	return res, nil
}
