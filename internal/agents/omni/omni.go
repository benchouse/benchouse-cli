// Package omni reads Omni's AI assistant conversations (omni.co) over
// Omni's public API.
//
// Written from Omni's OpenAPI document (https://docs.omni.co/api/openapi.yaml,
// read 2026-10-30) and the pages it backs:
//
//   - GET /api/v1/ai/conversations (https://docs.omni.co/api/ai/list-ai-conversations):
//     "ordered by most-recent activity", pageSize 1..100, cursor paging
//     (pageInfo.nextCursor, hasNextPage, totalRecords). There is no date
//     filter: paging stops at the first conversation last active before
//     --since. userId filters to one user and "Requires an Organization API
//     key"; "User-scoped tokens automatically see only their own
//     conversations and cannot use this parameter".
//   - GET /api/v1/ai/conversations/{conversationId} (get-conversation): the
//     messages, "alternating user and assistant turns", each {role, content,
//     timestamp}; assistant messages carry jobId and omniChatUrl.
//     "Organization API keys can access any conversation in the
//     organization; Personal Access Tokens can only access the
//     authenticating user's conversations."
//   - GET /api/v1/ai/jobs/{jobId}: state (COMPLETE, FAILED, CANCELLED, ...),
//     requestedLlm, modelId, branchId, topicName, error, completedAt.
//   - GET /api/v1/ai/jobs/{jobId}/result: "all actions taken by the AI
//     (queries generated, data retrieved) and the final summarized answer".
//     "Results are only available for jobs in COMPLETE state and are
//     retained for 14 days after completion." A generate_query action's
//     result holds query (the semantic query JSON), status, totalRowCount
//     and csvResult (the rows).
//   - Rate limit: 60 requests a minute (429); requests are spaced a second
//     apart.
//
// A turn is a user message and the assistant message(s) after it. For each
// assistant jobId the job is read, and its result when the job completed
// within the last 14 days (older results are gone; a note counts them). A
// generated query becomes a statement when its query JSON carries SQL (a
// sql / userEditedSQL key: the documents do not say whether it ever does),
// otherwise a generate_query tool_call with the query JSON as its input.
// csvResult is kept only with --include-results; row counts always. The
// other actions' explanations are reasoning, labelled with the action type.
//
// Feedback is not readable: the API only writes it (POST
// /v1/ai/jobs/{jobId}/feedback), so turns carry none. Whether the key is an
// Organization API key is probed with the userId filter (a personal token
// cannot use it): the documents give the rule, not the refusal's status, so
// 400 and 403 both read as a personal token.
//
// The context folder is the Omni model as Omni writes it to git: model and
// relationships files, <view>.view and <topic>.topic files (with or without
// a .yaml suffix; the docs show both "views/orders.yaml" from the model YAML
// API and "<view_name>.view" names from the bulk-update guide), so any
// *.yaml / *.yml and *.view / *.topic / *.composite_topic is kept.
package omni

import (
	"context"
	"errors"
	"fmt"
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
		ID:      "omni",
		Name:    "Omni",
		Summary: "Omni AI assistant conversations, with the queries it generated, via the Omni API",
		Status:  agent.Experimental,
		Setup: `The CLI reads Omni's AI conversations over the API with an Organization API
key (Settings > API keys), which can access any conversation in the
organization, and only makes GET requests. A personal access token works too
but reads only its own user's conversations.

The queries the AI generated come from each answer's job result, which Omni
keeps for 14 days: older answers are read without their queries. Omni allows
60 API requests a minute.`,
		Fields: []agent.Field{
			{Key: "host", Label: "Omni address", Validate: validHost,
				Help: "your organization's Omni address, e.g. https://acme.omniapp.co"},
			{Key: "api-key", Label: "Omni API key", Secret: true, Validate: agent.Token, Verify: verifyKey,
				Help: "an Organization API key (a personal access token reads only its own conversations)"},
		},
		Context: &agent.Context{
			Label: "Omni model folder",
			Help:  "the git repository Omni syncs your shared model to (model, relationships, *.view and *.topic files)",
			Check: checkContext,
			Keep:  keepContext,
		},
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
}

const (
	fetchMethod      = "api:omni"
	conversationsAPI = "/v1/ai/conversations"
	pageSize         = 100
	// noUser is a well-formed user id no one has: the probe that tells an
	// Organization API key from a personal token.
	noUser = "00000000-0000-0000-0000-000000000000"
	// resultRetention is how long Omni keeps a job's result.
	resultRetention = 14 * 24 * time.Hour
	// maxPages bounds the conversation walk (100,000 conversations).
	maxPages = 1000
)

const (
	purposeScope    = "whether the key reads every user's conversations (the userId filter needs an Organization API key; no user has this id)"
	purposeList     = "AI conversations, most recent activity first (first page)"
	purposeListNext = "following pages, until there are none, --since is passed or --max-conversations is reached"
	purposeConv     = "a chosen conversation's messages"
	purposeJob      = "the AI job behind an answer (state, model, topic, error)"
	purposeResult   = "the job's result (actions and generated queries), only for jobs completed in the last 14 days"

	scopePath    = conversationsAPI + "?pageSize=1&userId=" + noUser
	listPath     = conversationsAPI + "?pageSize=100"
	nextCursor   = "&cursor=<pageInfo.nextCursor of the previous page>"
	convStmt     = "GET " + apiRoot + conversationsAPI + "/<conversation id>"
	jobStmt      = "GET " + apiRoot + "/v1/ai/jobs/<job id>"
	resultStmt   = "GET " + apiRoot + "/v1/ai/jobs/<job id>/result"
	feedbackNote = "Omni's API does not expose answer feedback (it can only be written), so turns carry none"
)

func plan(agent.Inputs, agent.Options) []bundle.Query {
	return []bundle.Query{
		{Purpose: purposeScope, Statement: "GET " + apiRoot + scopePath},
		{Purpose: purposeList, Statement: "GET " + apiRoot + listPath},
		{Purpose: purposeListNext, Statement: "GET " + apiRoot + listPath + nextCursor},
		{Purpose: purposeConv, Statement: convStmt},
		{Purpose: purposeJob, Statement: jobStmt},
		{Purpose: purposeResult, Statement: resultStmt},
	}
}

type conversationItem struct {
	ID        string `json:"id"`
	UpdatedAt string `json:"updatedAt"`
	CreatedAt string `json:"createdAt"`
}

type listPage struct {
	Data          []conversationItem `json:"data"`
	Conversations []conversationItem `json:"conversations"`
	PageInfo      struct {
		HasNextPage  bool    `json:"hasNextPage"`
		NextCursor   *string `json:"nextCursor"`
		TotalRecords *int64  `json:"totalRecords"`
	} `json:"pageInfo"`
}

func (p listPage) items() []conversationItem {
	if len(p.Data) > 0 {
		return p.Data
	}
	return p.Conversations
}

type conversation struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	CreatedAt string    `json:"createdAt"`
	UpdatedAt string    `json:"updatedAt"`
	Messages  []message `json:"messages"`
}

type message struct {
	Role        string `json:"role"`
	Content     string `json:"content"`
	Timestamp   string `json:"timestamp"`
	JobID       string `json:"jobId"`
	OmniChatURL string `json:"omniChatUrl"`
}

type job struct {
	ID           string  `json:"id"`
	State        string  `json:"state"`
	ModelID      *string `json:"modelId"`
	BranchID     *string `json:"branchId"`
	TopicName    *string `json:"topicName"`
	RequestedLlm *struct {
		Model    string `json:"model"`
		Provider string `json:"provider"`
	} `json:"requestedLlm"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	} `json:"error"`
	CreatedAt   string `json:"createdAt"`
	CompletedAt string `json:"completedAt"`
}

type jobResult struct {
	Actions       []action `json:"actions"`
	Message       string   `json:"message"`
	ResultSummary string   `json:"resultSummary"`
	Topic         string   `json:"topic"`
}

type action struct {
	Type      string         `json:"type"`
	Status    string         `json:"status"`
	Message   string         `json:"message"`
	Timestamp string         `json:"timestamp"`
	Result    map[string]any `json:"result"`
}

// scope probes whether the key is an Organization API key: true when it can
// use the userId filter, false when it is refused as a personal token.
func scope(ctx context.Context, cl *client) (org bool, known bool, err error) {
	var p listPage
	err = cl.get(ctx, scopePath, &p)
	switch s := statusOf(err); {
	case err == nil, s == 404:
		return true, true, nil
	case s == 400, s == 403:
		return false, true, nil
	case s == 401:
		return false, false, err
	}
	return false, false, err
}

const personalNote = "this is a personal access token: only its own user's conversations were read. " +
	"Use an Organization API key (Settings > API keys) to read everyone's"

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	cl, err := newClient(in)
	if err != nil {
		return agent.Result{}, err
	}
	res := agent.Result{Sampled: opts.MaxConversations > 0}
	org, known, err := scope(ctx, cl)
	if statusOf(err) == 401 {
		return res, err
	}
	if errors.Is(err, context.Canceled) {
		return res, err
	}
	res.Queries = append(res.Queries, bundle.Query{Purpose: purposeScope, Statement: "GET " + apiRoot + scopePath})
	switch {
	case !known:
		res.Notes = append(res.Notes, "could not tell whether the key is an Organization API key: "+agent.Clip(err))
	case !org:
		res.Notes = append(res.Notes, personalNote)
	}

	m := &mapper{includeResults: opts.IncludeResults, since: opts.Since, now: now().UTC()}
	var (
		cursor        string
		seen          = map[string]bool{}
		failed        int
		capped, ended bool
	)
	for n := 1; !capped && !ended; n++ {
		if n > maxPages {
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after %d pages of conversations; older ones were not read (narrow with --since)", maxPages))
			break
		}
		path := listPath
		if n > 1 {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var p listPage
		if err := cl.get(ctx, path, &p); err != nil {
			if n == 1 {
				return res, err
			}
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after page %d of conversations: %s", n-1, agent.Clip(err)))
			break
		}
		items := p.items()
		if n == 1 {
			res.Queries = append(res.Queries, bundle.Query{Purpose: purposeList, Statement: "GET " + apiRoot + path, Rows: len(items)})
			if opts.Since.IsZero() && p.PageInfo.TotalRecords != nil {
				res.InWindow = p.PageInfo.TotalRecords
			}
		} else {
			res.Tally(purposeListNext, "GET "+apiRoot+listPath+nextCursor, len(items))
		}
		for _, it := range items {
			if at, ok := parseTime(it.UpdatedAt); ok && !opts.Since.IsZero() && at.Before(opts.Since) {
				ended = true // most recent activity first: the rest is older
				break
			}
			if it.ID == "" {
				continue
			}
			if opts.MaxConversations > 0 && res.Conversations >= opts.MaxConversations {
				capped = true
				break
			}
			conv, err := readConversation(ctx, cl, it.ID, &res)
			if err != nil {
				switch {
				case statusOf(err) == 401, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
					return res, err
				case statusOf(err) != 403 && statusOf(err) != 404:
					res.Notes = append(res.Notes, fmt.Sprintf("could not read conversation %s: %s", it.ID, agent.Clip(err)))
				}
				failed++
				continue
			}
			res.Conversations++
			for _, d := range m.drafts(conv) {
				for _, msg := range d.replies {
					if msg.JobID == "" {
						continue
					}
					j, r := readJob(ctx, cl, m, msg, &res)
					d.jobs = append(d.jobs, jobRead{msg: msg, job: j, result: r})
				}
				tr, ok := m.turn(conv, d)
				if !ok {
					continue
				}
				rec, err := turns.Record(tr, columns, fetchMethod)
				if err != nil {
					res.Notes = append(res.Notes, "skipped a turn: "+err.Error())
					continue
				}
				if err := emit(rec); err != nil {
					return res, err
				}
			}
		}
		if !p.PageInfo.HasNextPage || p.PageInfo.NextCursor == nil || *p.PageInfo.NextCursor == "" || len(items) == 0 {
			break
		}
		if opts.MaxConversations > 0 && res.Conversations >= opts.MaxConversations {
			capped = true
			break
		}
		next := *p.PageInfo.NextCursor
		if seen[next] {
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after page %d of conversations: Omni repeated a page cursor", n))
			break
		}
		cursor = next
		seen[cursor] = true
	}
	if capped {
		res.Notes = append(res.Notes, fmt.Sprintf("stopped at --max-conversations %s: the most recently active conversations were read, older ones were not (narrow with --since)",
			agent.Thousands(int64(opts.MaxConversations))))
	}
	if failed > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("%d conversations could not be read (deleted, or not visible to this key)", failed))
	}
	res.Notes = append(res.Notes, m.notes()...)
	res.Notes = append(res.Notes, feedbackNote)
	if !opts.IncludeResults {
		res.Redactions = append(res.Redactions, "Omni generated-query results (csvResult) removed (query, status and row counts kept). Use --include-results to keep them.")
	}
	return res, nil
}

func readConversation(ctx context.Context, cl *client, id string, res *agent.Result) (conversation, error) {
	var c conversation
	pid, err := pathID(id)
	if err != nil {
		return c, err
	}
	if err := cl.get(ctx, conversationsAPI+"/"+pid, &c); err != nil {
		return c, err
	}
	res.Tally(purposeConv, convStmt, len(c.Messages))
	if c.ID == "" {
		c.ID = id
	}
	return c, nil
}

// readJob reads an answer's job, and its result when the job completed
// within resultRetention. Failures degrade to a turn without them.
func readJob(ctx context.Context, cl *client, m *mapper, msg message, res *agent.Result) (*job, *jobResult) {
	pid, err := pathID(msg.JobID)
	if err != nil {
		m.jobErrors++
		return nil, nil
	}
	var j job
	if err := cl.get(ctx, "/v1/ai/jobs/"+pid, &j); err != nil {
		m.jobErrors++
		res.Tally(purposeJob, jobStmt, 0)
		return nil, nil
	}
	res.Tally(purposeJob, jobStmt, 1)
	if !strings.EqualFold(j.State, "COMPLETE") {
		return &j, nil
	}
	done, ok := parseTime(j.CompletedAt)
	if !ok {
		done, ok = parseTime(msg.Timestamp)
	}
	if ok && m.now.Sub(done) > resultRetention {
		m.tooOld++
		return &j, nil
	}
	var r jobResult
	if err := cl.get(ctx, "/v1/ai/jobs/"+pid+"/result", &r); err != nil {
		res.Tally(purposeResult, resultStmt, 0)
		if statusOf(err) == 404 {
			m.expired++
		} else {
			m.resultErrors++
		}
		return &j, nil
	}
	res.Tally(purposeResult, resultStmt, len(r.Actions))
	return &j, &r
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	return t.UTC(), err == nil
}

func itoa(n int) string { return strconv.Itoa(n) }
