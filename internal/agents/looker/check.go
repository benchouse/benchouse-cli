package looker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// Check requests: login, the System Activity metadata Collect reads, one
// count of the questions and users in the window, one count of the AI
// queries in History and the SQL of up to sqlSample of them, and, only with
// --per-user yes, one login as a user (logged out at once).

const (
	// sqlSample is how many recent AI queries Check fetches the SQL of.
	sqlSample = 20
	// countLimit bounds a row count when the explore has no count measure.
	countLimit = 10000
	// historyCount is History's documented run-count measure.
	historyCount = "history.query_run_count"
)

func check(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	var res agent.CheckResult
	c, err := newClient(in.Get("host"), in.Get("client-id"), in.Get("client-secret"))
	if err != nil {
		return res, err
	}
	if err := c.login(ctx); err != nil {
		return res, err
	}
	defer c.logoutAdmin(ctx)
	res.Facts = append(res.Facts, "Connected to Looker at "+c.base)

	var scratch agent.Result
	f, err := discover(ctx, c, &scratch)
	if err != nil {
		return res, err
	}
	caReadable, histReadable := false, false
	var users []string

	if f.caOK {
		n, us, capped, err := countCA(ctx, c, f.ca, opts.Since)
		if err != nil {
			res.Warnings = append(res.Warnings, "System Activity Conversational Analytics could not be read: "+clip(err.Error(), 200))
		} else {
			caReadable = true
			users = us
			count := agent.Thousands(n)
			if capped {
				count = "over " + count
			}
			fact := fmt.Sprintf("System Activity Conversational Analytics (%s): %s questions", f.ca.Explore, count)
			if f.ca.User != "" {
				fact += fmt.Sprintf(" from %s users", agent.Thousands(int64(len(us))))
			}
			res.Facts = append(res.Facts, fact+" "+agent.Window(opts))
			if n == 0 {
				res.Warnings = append(res.Warnings, "no Conversational Analytics questions in System Activity "+agent.Window(opts)+
					": question text is recorded only for users who consented while the End User Conversational Analytics Query Review preview is on")
			}
		}
	} else {
		res.Warnings = append(res.Warnings, f.caMissing())
	}

	if f.histOK {
		n, sampled, withSQL, hu, err := checkHistory(ctx, c, f.hist, opts.Since)
		if err != nil {
			res.Warnings = append(res.Warnings, "System Activity History could not be read: "+clip(err.Error(), 200))
		} else {
			histReadable = true
			if len(users) == 0 {
				users = hu
			}
			if n == 0 {
				res.Warnings = append(res.Warnings, "query history: no queries from Conversational Analytics (API Client Name) "+
					agent.Window(opts)+"; statements will be missing")
			} else {
				res.Facts = append(res.Facts, fmt.Sprintf("query history: %s AI-generated queries %s (SQL found for %d%% of the %d most recent)",
					agent.Thousands(n), agent.Window(opts), withSQL*100/max(sampled, 1), sampled))
			}
		}
	} else {
		res.Warnings = append(res.Warnings, f.histMissing())
	}

	if !caReadable && !histReadable {
		return res, errors.New("neither System Activity source is readable: " + f.caMissing() + "; " + f.histMissing() +
			". The API key's user needs see_system_activity (or the Admin role)")
	}

	if !perUser(in) {
		res.Facts = append(res.Facts, "full answers: available with --per-user yes (logs in as each user; off)")
		return res, nil
	}
	if len(users) == 0 {
		res.Warnings = append(res.Warnings, "--per-user: no user with Conversational Analytics activity "+agent.Window(opts)+" to log in as")
		return res, nil
	}
	tok, err := c.sudo(ctx, users[0])
	switch s := statusOf(err); {
	case err == nil:
		c.logout(ctx, tok)
		res.Facts = append(res.Facts, fmt.Sprintf("full answers: readable with --per-user (logged in and out as user %s)", users[0]))
	case s == http.StatusForbidden || s == http.StatusUnauthorized:
		res.Warnings = append(res.Warnings, fmt.Sprintf("--per-user: sudo is not permitted for this client (HTTP %d): it needs the Admin role; "+
			"the run will read System Activity only", s))
	default:
		res.Warnings = append(res.Warnings, "--per-user: logging in as a user failed: "+clip(err.Error(), 200))
	}
	return res, nil
}

// countCA counts the questions and users in the window: with the
// explore's count measure when it has one, else by reading up to
// countLimit rows.
func countCA(ctx context.Context, c *client, f caFields, since time.Time) (int64, []string, bool, error) {
	q := inlineQuery{Model: saModel, View: f.Explore, QueryTimezone: "UTC"}
	if w := window(since, time.Time{}); w != "" {
		q.Filters = map[string]string{f.Time: w}
	}
	switch {
	case f.Count != "" && f.User != "":
		q.Fields, q.Limit = []string{f.User, f.Count}, fmt.Sprint(maxUsers)
	case f.Count != "":
		q.Fields, q.Limit = []string{f.Count}, "1"
	default:
		q.Fields, q.Limit = f.fields(false)[:2], fmt.Sprint(countLimit+1)
		if f.User != "" {
			q.Fields = append(q.Fields, f.User)
		}
	}
	rows, err := c.run(ctx, "", q)
	if err != nil {
		return 0, nil, false, err
	}
	var n int64
	seen := map[string]bool{}
	var users []string
	for _, r := range rows {
		if f.Count != "" {
			v, _ := number(r[f.Count])
			n += int64(v)
		} else {
			n++
		}
		if u := str(r[f.User]); f.User != "" && u != "" && !seen[u] {
			seen[u] = true
			users = append(users, u)
		}
	}
	capped := f.Count == "" && n > countLimit
	if capped {
		n = countLimit
	}
	return n, users, capped, nil
}

// checkHistory counts the AI-issued queries in the window and fetches the
// SQL of the most recent ones.
func checkHistory(ctx context.Context, c *client, h histFields, since time.Time) (n int64, sampled, withSQL int, users []string, err error) {
	q := historyQuery(h, since, sqlSample)
	rows, err := c.run(ctx, "", q)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if u := str(r[h.User]); u != "" && !seen[u] {
			seen[u] = true
			users = append(users, u)
		}
		id := str(r[h.QueryID])
		if id == "" {
			continue
		}
		sampled++
		if pid, err := pathID("query", id); err == nil {
			if sql, err := c.getText(ctx, "", fmt.Sprintf(sqlPath, pid)); err == nil && sql != "" {
				withSQL++
			}
		}
	}
	n = int64(len(rows))
	if len(rows) == sqlSample {
		cq := historyQuery(h, since, 1)
		cq.Fields, cq.Sorts = []string{historyCount}, nil
		if cr, err := c.run(ctx, "", cq); err == nil && len(cr) == 1 {
			if v, ok := number(cr[0][historyCount]); ok {
				n = int64(v)
			}
		}
	}
	return n, sampled, withSQL, users, nil
}
