// Package cube reads Cube's AI agents (Cube Cloud's chat, "D3" analytics
// agents) through the chat history export of the Cube Cloud control plane
// API.
//
// One request shape does the work: GET
// /api/v1/deployments/{deploymentId}/chat-history/threads with
// includeTurns=true, sorted by updatedAt descending and paged by cursor
// (first/after, at most 50 threads per page with turns). A thread is a
// conversation; each of its turns (the user's prompt, the final answer, the
// steps and tools the agent used, and each query it ran with its SQL,
// status, row count and thumbs-up/down feedback) becomes one turns/v1 turn
// (see map.go). --since becomes updatedAfter, Cube's incremental-export
// filter; turns asked before it are left out. --max-conversations keeps
// the most recently updated threads. A listed thread that comes without its
// turns is read again with GET .../chat-history/threads/{chatId}.
//
// Written from Cube's API reference, read 2026-10-30:
// https://docs.cube.dev/api-reference/chat-history/list-chat-threads.md,
// https://docs.cube.dev/api-reference/chat-history/get-a-chat-thread.md,
// https://docs.cube.dev/api-reference/deployments/get-deployments.md and
// https://docs.cube.dev/api-reference/authentication.md (bearer API key on
// https://<tenant>.cubecloud.dev). The export is admin only and in preview:
// Cube answers 404 when it is not enabled for the account. Cube cuts texts
// over 20,000 characters and errors over 2,000 with "…". Tested against
// fixtures only.
package cube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

func init() {
	agent.Register(agent.Agent{
		ID:      "cube",
		Name:    "Cube",
		Summary: "Cube Cloud's AI agents; reads the chat history export of the Cube Cloud API",
		Status:  agent.Experimental,
		Setup:   setup,
		Fields: []agent.Field{
			{Key: "host", Label: "Cube Cloud URL", Validate: agent.URL(false),
				Help: "your tenant's Cube Cloud address, e.g. https://acme.cubecloud.dev (https only)"},
			{Key: "api-token", Label: "Cube API key", Secret: true, Validate: agent.Token, Verify: verifyToken,
				Help: "an API key of an admin user (Settings > API keys); sent as a bearer token"},
			{Key: "deployment-id", Label: "Deployment ID", Validate: deploymentID, Lookup: lookupDeployments,
				Help: "the numeric id of the deployment whose agents to read (in its URL in Cube Cloud)"},
		},
		Context: &agent.Context{
			Label: "Cube data model folder",
			Help:  "the deployment's data model: the model/ folder with your cubes and views (*.yml, *.yaml, *.js)",
			Check: checkContext,
			Keep:  keepContext,
		},
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
}

const setup = `The CLI reads the chat history export of the Cube Cloud API with an API
key and only makes GET requests: the deployment, and its chat threads with
their turns (prompts, answers, steps, queries with SQL and feedback).

  - The key must belong to a user with the admin role: the export is admin
    only. A key scoped to the deployment is enough.
  - Chat history export is a Cube preview feature: if it is not enabled for
    your account Cube answers 404; ask Cube support to enable it.

Query result rows (up to 100 per query) are in the export; they are
dropped unless --include-results.`

const (
	fetchMethod  = "api:cube/chat-history"
	pageSize     = 50 // Cube's maximum with includeTurns=true
	maxPages     = 2000
	drainNoteCap = 10
)

// Request purposes, shared by Plan and Result.Queries.
const (
	purposeDeployment = "the deployment, to confirm the id"
	purposeFirst      = "chat threads with their turns, most recently updated first (first page)"
	purposeNext       = "following pages, until there are none or --max-conversations is reached"
	purposeThread     = "a listed thread that came without its turns (only then)"
)

func deploymentPath(id string) string { return "/api/v1/deployments/" + url.PathEscape(id) }

func threadsPath(dep string) string { return deploymentPath(dep) + "/chat-history/threads" }

// listQuery is the first page's query; first is the page size.
func listQuery(opts agent.Options, first int, withTurns bool) string {
	v := url.Values{"sortBy": {"updatedAt"}, "sortOrder": {"desc"}, "first": {strconv.Itoa(first)}}
	if withTurns {
		v.Set("includeTurns", "true")
	}
	if !opts.Since.IsZero() {
		v.Set("updatedAfter", opts.Since.UTC().Format(time.RFC3339))
	}
	return v.Encode()
}

const afterParam = "&after=<pageInfo.endCursor of the previous page>"

func firstSize(opts agent.Options) int {
	if opts.MaxConversations > 0 && opts.MaxConversations < pageSize {
		return opts.MaxConversations
	}
	return pageSize
}

func plan(in agent.Inputs, opts agent.Options) []bundle.Query {
	dep := in.Get("deployment-id")
	if dep == "" {
		dep = "<deployment id>"
	}
	first := threadsPath(dep) + "?" + listQuery(opts, firstSize(opts), true)
	return []bundle.Query{
		{Purpose: purposeDeployment, Statement: "GET " + deploymentPath(dep)},
		{Purpose: purposeFirst, Statement: "GET " + first},
		{Purpose: purposeNext, Statement: "GET " + first + afterParam},
		{Purpose: purposeThread, Statement: "GET " + threadsPath(dep) + "/<chatId>"},
	}
}

// listPage is one page of threads.
type listPage struct {
	Items    []thread `json:"items"`
	PageInfo struct {
		EndCursor   *string `json:"endCursor"`
		HasNextPage bool    `json:"hasNextPage"`
	} `json:"pageInfo"`
}

type deployment struct {
	ID   jsonID `json:"id"`
	Name string `json:"name"`
}

// jsonID reads an id given as a number or a string.
type jsonID string

func (j *jsonID) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" {
		s = ""
	}
	*j = jsonID(s)
	return nil
}

func deploymentID(v string) error {
	if _, err := strconv.ParseUint(v, 10, 63); err != nil {
		return errors.New("a deployment id is a number (it is in the deployment's URL in Cube Cloud)")
	}
	return nil
}

// readDeployment confirms the deployment exists for this key.
func readDeployment(ctx context.Context, cl *client, dep string) (deployment, error) {
	var d deployment
	if err := deploymentID(dep); err != nil {
		return d, fmt.Errorf("--deployment-id: %w", err)
	}
	if err := cl.get(ctx, deploymentPath(dep), &d); err != nil {
		if statusOf(err) == http.StatusNotFound {
			return d, fmt.Errorf("Cube at %s has no deployment %s visible to this API key: check --deployment-id", cl.base, dep)
		}
		return d, explain(err, "the deployment")
	}
	return d, nil
}

// previewError is the error for a 404 on the chat history endpoints once
// the deployment is known to exist.
var previewError = errors.New("chat history export is a Cube preview feature: ask Cube support to enable it for your account " +
	"(Cube answered 404 on /chat-history/threads)")

func listErr(err error) error {
	if statusOf(err) == http.StatusNotFound {
		return previewError
	}
	return explain(err, "the chat history export")
}

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	res := agent.Result{Sampled: opts.MaxConversations > 0}
	cl, err := newClient(in)
	if err != nil {
		return res, err
	}
	dep := in.Get("deployment-id")
	d, err := readDeployment(ctx, cl, dep)
	if err != nil {
		return res, err
	}
	res.Queries = append(res.Queries, bundle.Query{Purpose: purposeDeployment, Statement: "GET " + deploymentPath(dep), Rows: 1})
	name := d.Name
	if name == "" {
		name = dep
	}
	res.Sources = append(res.Sources, bundle.Source{SourceKind: turns.SourceKind, NativeID: dep, Account: cl.base, Label: name})

	m := newMapper(opts)
	first := threadsPath(dep) + "?" + listQuery(opts, firstSize(opts), true)
	cursor := ""
	seen := map[string]bool{}
	chats := map[string]bool{}
	capped := false
	for n := 1; ; n++ {
		if n > maxPages {
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after %d pages; older threads were not read (narrow with --since)", maxPages))
			break
		}
		path := first
		if n > 1 {
			size := pageSize
			if opts.MaxConversations > 0 {
				size = min(pageSize, opts.MaxConversations-len(chats))
			}
			path = threadsPath(dep) + "?" + listQuery(opts, size, true) + "&after=" + url.QueryEscape(cursor)
		}
		var p listPage
		if err := cl.get(ctx, path, &p); err != nil {
			if n == 1 {
				return res, listErr(err)
			}
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after page %d: %s", n-1, agent.Clip(listErr(err))))
			break
		}
		if n == 1 {
			res.Queries = append(res.Queries, bundle.Query{Purpose: purposeFirst, Statement: "GET " + path, Rows: len(p.Items)})
		} else {
			res.Tally(purposeNext, "GET "+first+afterParam, len(p.Items))
		}
		for _, th := range p.Items {
			if th.ChatID == "" || chats[th.ChatID] {
				continue
			}
			if opts.MaxConversations > 0 && len(chats) >= opts.MaxConversations {
				capped = true
				break
			}
			chats[th.ChatID] = true
			if th.Turns == nil {
				full, err := readThread(ctx, cl, dep, th.ChatID)
				res.Tally(purposeThread, "GET "+threadsPath(dep)+"/<chatId>", boolInt(err == nil))
				if err != nil {
					m.unreadable++
					if len(m.unreadableEx) < drainNoteCap {
						m.unreadableEx = append(m.unreadableEx, agent.Clip(err))
					}
					continue
				}
				th = full
			}
			if err := m.thread(th, emit); err != nil {
				return res, err
			}
		}
		if capped || (opts.MaxConversations > 0 && len(chats) >= opts.MaxConversations) {
			capped = capped || p.PageInfo.HasNextPage
			break
		}
		if !p.PageInfo.HasNextPage || p.PageInfo.EndCursor == nil || *p.PageInfo.EndCursor == "" || len(p.Items) == 0 {
			break
		}
		if seen[*p.PageInfo.EndCursor] {
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after page %d: Cube repeated a page cursor", n))
			break
		}
		cursor = *p.PageInfo.EndCursor
		seen[cursor] = true
	}
	res.Conversations = len(chats)
	if capped {
		res.Notes = append(res.Notes, fmt.Sprintf("stopped at --max-conversations %s: the most recently updated threads were read, older ones were not (narrow with --since)",
			agent.Thousands(int64(opts.MaxConversations))))
	}
	res.Notes = append(res.Notes, m.notes()...)
	if !opts.IncludeResults {
		res.Redactions = append(res.Redactions, "Cube query result rows (up to 100 per query) were dropped; SQL, columns and row counts kept (--include-results keeps them)")
	}
	if !opts.IncludeUsers {
		res.Redactions = append(res.Redactions, "Cube user names, emails and external user ids were dropped; the numeric user id is kept (--include-users keeps them)")
	}
	return res, nil
}

func readThread(ctx context.Context, cl *client, dep, chat string) (thread, error) {
	var th thread
	id, err := pathID("chat", chat)
	if err != nil {
		return th, err
	}
	err = cl.get(ctx, threadsPath(dep)+"/"+id, &th)
	if err != nil {
		return th, listErr(err)
	}
	if th.Turns == nil {
		th.Turns = []turn{}
	}
	return th, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
