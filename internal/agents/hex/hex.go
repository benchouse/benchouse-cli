// Package hex reads the Hex agent's threads (hex.tech: Threads, Notebook
// and Modeling agents) over Hex's public API.
//
// Written from Hex's OpenAPI document (https://static.hex.site/openapi.json,
// API 1.0.0, read 2026-10-30), linked from the reference at
// https://learn.hex.tech/docs/api-integrations/api/reference:
//
//   - GET /api/v1/threads (ListThreads): newest created first, cursor paging
//     (pagination.after / before), limit 1..100. "Requires Manager org role
//     or higher. Workspace admins receive every non-private thread; managers
//     receive threads they created plus Threads-type threads on projects
//     they have access to. Threads marked private are returned with their
//     title redacted." Filters used: numDays (from --since), source, type.
//   - GET /api/v1/threads/{threadId}/messages?full=true (GetThreadMessages):
//     a flat list of content blocks {type: "text", text, createdAt}, at most
//     50 a page; the first answer is the most recent page and the before
//     cursor reads older ones. Within a page blocks are oldest first, each
//     text block "prefixed with the authoring role, and thinking and error
//     blocks are tagged". full=true adds "JSON-encoded message records with
//     thinking, tool calls, results, and mentions".
//   - GET /api/v1/users/me: the token's user and org role (Check and Verify
//     only).
//   - Rate limits: the hex-api group, 30 requests a minute and 1,800 an
//     hour by default; requests are spaced two seconds apart.
//
// What the documents leave open, handled both ways (see map.go):
//
//   - the shape of a full=true message record is not specified. A block
//     whose text (or record/message/json field) decodes to a JSON object
//     with a role or content is read as a record: role, content as a string
//     or parts (text, thinking/reasoning, tool_use/tool_call, tool_result,
//     error, AI SDK tool-<name> parts), top-level toolCalls and thinking. When
//     a thread has records, its rendered text blocks are ignored (they
//     repeat them); otherwise the rendered text is read by its role prefix
//     ("user:", "assistant:") and its [thinking] / [error] tag.
//   - which thread flag means private: sensitive (documented) or private.
//     Either skips the thread, counted in a note.
//   - which list cursor reaches older threads: pagination.after (the next
//     page of a newest-first list), as GetThreadMessages names before as
//     the way to older messages.
//
// A turn is a user message and the assistant blocks that follow it, until
// the next user message. Statements are SQL found in tool-call inputs (a
// sql/query/statement/code key holding SQL starting SELECT or WITH, or any
// non-empty one of those keys when the tool's name mentions sql); other tool
// calls are tool_calls, with outputs only under --include-results.
// Thinking blocks are reasoning. The thread's feedback and warnings, which
// Hex records per thread, go to extra on its last turn; source becomes
// client_interface; effectiveModel the model.
package hex

import (
	"context"
	"errors"
	"fmt"
	"math"
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
		ID:   "hex",
		Name: "Hex",
		// Hex allows 30 requests a minute and each thread costs at least
		// one: 150 threads is about five minutes. The user can raise it.
		DefaultMaxConversations: 150,
		Summary:                 "Hex agent threads (Threads, Notebook and Modeling agents) via the Hex public API",
		Status:                  agent.Experimental,
		Setup: `The CLI reads Hex's agent threads over the public API with an API token
(Settings > API keys) and only makes GET requests.

Listing threads requires the Manager org role or higher: an Admin's token sees
every non-private thread in the workspace; a Manager's sees the threads they
created plus Threads-type threads on projects they can access. Private
threads are skipped. Hex allows 30 API requests a minute, so the CLI reads
about one thread every two to four seconds.`,
		Fields: []agent.Field{
			{Key: "host", Label: "Hex address", Default: defaultHost, Validate: agent.URL(true),
				Help: "https://app.hex.tech, or your EU or single-tenant Hex address"},
			{Key: "token", Label: "Hex API token", Secret: true, Validate: agent.Token, Verify: verifyToken,
				Help: "a token of a user with the Manager org role or higher (Admin to see every thread)"},
			{Key: "source", Label: "Only threads from", Optional: true, Choices: []string{"HEX", "SLACK", "MCP", "PUBLIC_API"},
				Help: "leave empty for every source (HEX, SLACK, MCP, PUBLIC_API)"},
			{Key: "thread-type", Label: "Only threads of type", Optional: true, Choices: []string{"THREADS", "NOTEBOOK", "MODELING"},
				Help: "leave empty for every agent (THREADS, NOTEBOOK, MODELING)"},
		},
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
}

const (
	fetchMethod     = "api:hex"
	threadsPath     = "/v1/threads"
	mePath          = "/v1/users/me"
	threadPageSize  = 100
	messagePageSize = 50
	// maxThreadPages bounds the thread walk (100,000 threads).
	maxThreadPages = 1000
	// maxMessagePages bounds one thread (2,500 blocks); past it the
	// oldest messages are not read.
	maxMessagePages = 50
)

const (
	purposeList      = "agent threads, most recently created first (first page)"
	purposeListNext  = "following pages, until there are none, --since is passed or --max-conversations is reached"
	purposeMessages  = "the newest page of a chosen thread's messages, with full message records"
	purposeOlder     = "older pages of that thread's messages, until there are none"
	nextThreadCursor = "&after=<pagination.after of the previous page>"
	olderCursor      = "&before=<pagination.before of the previous page>"
	messagesStmt     = "GET " + apiRoot + "/v1/threads/<thread id>/messages?" + messagesQuery
	messagesQuery    = "full=true&limit=50"
)

// listQuery is the thread list's query: the page size, the --since window
// as whole days back from now (Hex's numDays), and the filters.
func listQuery(in agent.Inputs, opts agent.Options, limit int) string {
	v := url.Values{"limit": {strconv.Itoa(limit)}}
	if !opts.Since.IsZero() {
		v.Set("numDays", strconv.Itoa(numDays(opts.Since)))
	}
	if s := in.Get("source"); s != "" {
		v.Set("source", s)
	}
	if t := in.Get("thread-type"); t != "" {
		v.Set("type", t)
	}
	return threadsPath + "?" + v.Encode()
}

func numDays(since time.Time) int {
	return max(int(math.Ceil(now().Sub(since).Hours()/24)), 1)
}

func plan(in agent.Inputs, opts agent.Options) []bundle.Query {
	first := "GET " + apiRoot + listQuery(in, opts, threadPageSize)
	return []bundle.Query{
		{Purpose: purposeList, Statement: first},
		{Purpose: purposeListNext, Statement: first + nextThreadCursor},
		{Purpose: purposeMessages, Statement: messagesStmt},
		{Purpose: purposeOlder, Statement: messagesStmt + olderCursor},
	}
}

// thread is a ListThreads item; only what the reader uses.
type thread struct {
	ID             string  `json:"id"`
	ThreadID       string  `json:"threadId"`
	Title          *string `json:"title"`
	Source         string  `json:"source"`
	Type           string  `json:"type"`
	Classification *string `json:"classification"`
	UserID         *string `json:"userId"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
	Sensitive      bool    `json:"sensitive"`
	Private        bool    `json:"private"`
	URL            string  `json:"url"`
	EffectiveModel *string `json:"effectiveModel"`
	Model          string  `json:"model"`
	Warnings       []any   `json:"warnings"`
	Feedback       []any   `json:"feedback"`
	Topics         []topic `json:"topics"`
	created        time.Time
}

type topic struct {
	Name string `json:"name"`
}

func (t thread) id() string {
	if t.ID != "" {
		return t.ID
	}
	return t.ThreadID
}

type pagination struct {
	After  *string `json:"after"`
	Before *string `json:"before"`
}

type threadPage struct {
	Threads    []thread   `json:"threads"`
	Pagination pagination `json:"pagination"`
}

type messagePage struct {
	Content    []map[string]any `json:"content"`
	Pagination pagination       `json:"pagination"`
}

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	cl, err := newClient(in)
	if err != nil {
		return agent.Result{}, err
	}
	res := agent.Result{Sampled: opts.MaxConversations > 0}
	m := mapper{includeResults: opts.IncludeResults}
	first := listQuery(in, opts, threadPageSize)
	var (
		cursor          string
		seen            = map[string]bool{}
		private, failed int
		emitted         int
		capped, ended   bool
	)
	for n := 1; !capped && !ended; n++ {
		if n > maxThreadPages {
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after %d pages of threads; older threads were not read (narrow with --since)", maxThreadPages))
			break
		}
		path := first
		if n > 1 {
			path += "&after=" + url.QueryEscape(cursor)
		}
		var p threadPage
		if err := cl.get(ctx, path, &p); err != nil {
			if n == 1 {
				return res, err
			}
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after page %d of threads: %s", n-1, agent.Clip(err)))
			break
		}
		if n == 1 {
			res.Queries = append(res.Queries, bundle.Query{Purpose: purposeList, Statement: "GET " + apiRoot + path, Rows: len(p.Threads)})
		} else {
			res.Tally(purposeListNext, "GET "+apiRoot+first+nextThreadCursor, len(p.Threads))
		}
		for _, t := range p.Threads {
			t.created, _ = parseTime(t.CreatedAt)
			if !opts.Since.IsZero() && !t.created.IsZero() && t.created.Before(opts.Since) {
				ended = true // newest created first: the rest is older
				break
			}
			if t.id() == "" {
				continue
			}
			if t.Sensitive || t.Private {
				private++
				continue
			}
			if opts.MaxConversations > 0 && res.Conversations >= opts.MaxConversations {
				capped = true
				break
			}
			blocks, truncated, err := readMessages(ctx, cl, t.id(), &res)
			if err != nil {
				if statusOf(err) == 401 {
					return res, err
				}
				if s := statusOf(err); s == 403 || s == 404 {
					if s == 403 {
						private++
					} else {
						failed++
					}
					continue
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return res, err
				}
				failed++
				res.Notes = append(res.Notes, fmt.Sprintf("could not read thread %s: %s", t.id(), agent.Clip(err)))
				continue
			}
			res.Conversations++
			if truncated {
				res.Notes = append(res.Notes, fmt.Sprintf("thread %s has more than %d pages of messages: its oldest messages were not read", t.id(), maxMessagePages))
			}
			for _, tr := range m.turns(t, blocks) {
				rec, err := turns.Record(tr, columns, fetchMethod)
				if err != nil {
					res.Notes = append(res.Notes, "skipped a turn: "+err.Error())
					continue
				}
				emitted++
				if err := emit(rec); err != nil {
					return res, err
				}
			}
		}
		next := p.Pagination.After
		if next == nil || *next == "" || len(p.Threads) == 0 {
			break
		}
		if opts.MaxConversations > 0 && res.Conversations >= opts.MaxConversations {
			capped = true // more pages exist, and the cap is full
			break
		}
		if seen[*next] {
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after page %d of threads: Hex repeated a page cursor", n))
			break
		}
		cursor = *next
		seen[cursor] = true
	}
	if capped {
		res.Notes = append(res.Notes, fmt.Sprintf("stopped at --max-conversations %s: the most recently created threads were read, older ones were not (narrow with --since)",
			agent.Thousands(int64(opts.MaxConversations))))
	}
	if private > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("skipped %d private threads: Hex hides their contents from everyone but their author", private))
	}
	if failed > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("%d threads could not be read (deleted, or not visible to this token)", failed))
	}
	res.Notes = append(res.Notes, m.notes()...)
	res.Notes = append(res.Notes, "feedback and warnings are recorded by Hex per thread: they are on each thread's last turn (extra.feedback, extra.warnings)")
	if !opts.IncludeResults {
		res.Redactions = append(res.Redactions, "Hex tool-call outputs and query results removed (tool names, inputs, SQL and status kept). Use --include-results to keep them.")
	}
	if emitted == 0 && res.Conversations > 0 && m.unread > 0 {
		return res, errors.New("hex: no thread's messages could be read as questions and answers; the message format may have changed (please open an issue)")
	}
	return res, nil
}

// readMessages reads every page of one thread's messages, newest page first,
// and returns its blocks oldest first. One thread is held at a time, at most
// maxMessagePages pages.
func readMessages(ctx context.Context, cl *client, id string, res *agent.Result) ([]map[string]any, bool, error) {
	pid, err := pathID(id)
	if err != nil {
		return nil, false, err
	}
	base := "/v1/threads/" + pid + "/messages?" + messagesQuery
	var pages [][]map[string]any
	seen := map[string]bool{}
	cursor := ""
	truncated := false
	for n := 1; ; n++ {
		if n > maxMessagePages {
			truncated = true
			break
		}
		path := base
		if n > 1 {
			path += "&before=" + url.QueryEscape(cursor)
		}
		var p messagePage
		if err := cl.get(ctx, path, &p); err != nil {
			if n == 1 {
				return nil, false, err
			}
			res.Notes = append(res.Notes, fmt.Sprintf("thread %s: older messages not read: %s", id, agent.Clip(err)))
			break
		}
		if n == 1 {
			res.Tally(purposeMessages, messagesStmt, len(p.Content))
		} else {
			res.Tally(purposeOlder, messagesStmt+olderCursor, len(p.Content))
		}
		pages = append(pages, p.Content)
		b := p.Pagination.Before
		if b == nil || *b == "" || len(p.Content) == 0 || seen[*b] {
			break
		}
		cursor = *b
		seen[cursor] = true
	}
	var out []map[string]any
	for i := len(pages) - 1; i >= 0; i-- {
		out = append(out, pages[i]...)
	}
	return out, truncated, nil
}

// clientInterface maps Hex's thread source onto turns/v1's words.
func clientInterface(source string) string {
	switch strings.ToUpper(source) {
	case "HEX":
		return "web"
	case "SLACK":
		return "slack"
	case "MCP":
		return "mcp"
	case "PUBLIC_API":
		return "api"
	}
	return strings.ToLower(source)
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	return t.UTC(), err == nil
}
