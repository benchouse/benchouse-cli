package genie

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// Check requests, all reads: who the token is (SCIM Me), the SQL warehouse,
// the Genie space (or one page of the space list), a count of the Genie
// questions in the audit log over the window (a LIMIT 0 probe when the
// window is unbounded, since the audit log is large), LIMIT 0 probes of the
// optional system tables, and with --include-users one SCIM user.

const (
	mePath        = "/api/2.0/preview/scim/v2/Me"
	warehousePath = "/api/2.0/sql/warehouses/"
	spacePath     = "/api/2.0/genie/spaces/"
)

// countStatement counts the questions (n) and the conversations they were
// asked in over the window.
func countStatement(space, since string) string {
	return `SELECT to_json(struct(count(*) AS n, count(DISTINCT ` + convExpr + `) AS conversations)) AS row_json FROM ` + auditArm.table + `
             WHERE ` + auditArm.where(space, since) + ` AND ` + questionActions
}

func probeStatement(a arm, space, since string) string { return a.statement(space, since) + " LIMIT 0" }

func check(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	var res agent.CheckResult
	cl, spaceID, err := connect(in)
	if err != nil {
		return res, err
	}
	since := sinceDate(opts)

	user, err := whoami(ctx, cl)
	if err != nil {
		return res, err
	}
	fact := "Connected to " + cl.host
	if user != "" {
		fact += " as " + user
	}
	res.Facts = append(res.Facts, fact)

	wh, err := getWarehouse(ctx, cl)
	if err != nil {
		return res, err
	}
	res.Facts = append(res.Facts, fmt.Sprintf("SQL warehouse %s (%s)", wh.Name, wh.State))
	if w := wh.stopped(); w != "" {
		res.Warnings = append(res.Warnings, w)
	}

	if spaceID != "" {
		fact, err := getSpace(ctx, cl, spaceID)
		if err != nil {
			return res, err
		}
		res.Facts = append(res.Facts, fact)
	} else {
		var list struct {
			Spaces []space `json:"spaces"`
			Next   string  `json:"next_page_token"`
		}
		if err := cl.getJSON(ctx, http.MethodGet, spacePath[:len(spacePath)-1]+"?page_size=100", nil, &list); err != nil {
			res.Warnings = append(res.Warnings, "could not list Genie spaces: "+agent.Clip(err))
		} else {
			n := agent.Thousands(int64(len(list.Spaces)))
			if list.Next != "" {
				n += "+"
			}
			res.Facts = append(res.Facts, fmt.Sprintf("%s Genie spaces visible to the token; every space will be read", n))
		}
	}

	auditErr := func(err error) error {
		return fmt.Errorf("system.access.audit is not readable (%s): the token's identity needs USE CATALOG on system, "+
			"USE SCHEMA on system.access and SELECT on system.access.audit (system tables need Unity Catalog)", agent.Clip(err))
	}
	if since != "" {
		rows, err := cl.queryJSONRows(ctx, countStatement(spaceID, since))
		if err != nil {
			return res, auditErr(err)
		}
		var v struct {
			N             int64  `json:"n"`
			Conversations *int64 `json:"conversations"`
		}
		if len(rows) > 0 && json.Unmarshal([]byte(rows[0]), &v) == nil {
			if v.Conversations != nil {
				res.Conversations = v.Conversations
				q := "questions"
				if v.N == 1 {
					q = "question"
				}
				res.Facts = append(res.Facts, fmt.Sprintf("%s (%s %s)",
					agent.Conversations(*v.Conversations, "Genie conversation", "Genie conversations", opts), agent.Thousands(v.N), q))
			} else {
				res.Facts = append(res.Facts, agent.Counted(v.N, "Genie question", "Genie questions", opts))
			}
			if v.N == 0 {
				res.Warnings = append(res.Warnings, "no Genie questions in the audit log in the window: widen --since, or check the space id")
			}
		}
	} else if _, err := cl.queryJSONRows(ctx, probeStatement(auditArm, spaceID, since)); err != nil {
		return res, auditErr(err)
	}

	for _, a := range []arm{historyArm, billingArm} {
		if _, err := cl.queryJSONRows(ctx, probeStatement(a, spaceID, since)); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s not readable (%s), so %s will be missing: grant USE SCHEMA on %s and SELECT on %s",
				a.table, agent.Clip(err), a.purpose, strings.TrimSuffix(a.table, "."+lastPart(a.table)), a.table))
		}
	}
	if opts.IncludeUsers {
		var body struct{}
		if err := cl.getJSON(ctx, http.MethodGet, fmt.Sprintf(scimPath, 1, 1), nil, &body); err != nil {
			res.Warnings = append(res.Warnings, "the user directory is not readable, so --include-users will add no names: "+
				"it needs a workspace admin token ("+agent.Clip(err)+")")
		}
	}
	return res, nil
}

// lastPart is the last dotted component of a table name.
func lastPart(table string) string { return table[strings.LastIndex(table, ".")+1:] }
