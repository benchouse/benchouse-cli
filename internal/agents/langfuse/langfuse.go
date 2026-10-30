// Package langfuse reads custom analytics agents traced with Langfuse
// (langfuse.com, cloud or self-hosted).
//
// A team that built its own agent and traces it with Langfuse has one trace
// per question: the root observation holds the question (input) and the
// answer (output), generations hold the model and token usage, and tool or
// span observations hold the SQL the agent ran. This reader pages through
// GET /api/public/v2/observations and groups observations by trace; each
// trace becomes one turns/v1 turn (see map.go for the field mapping).
//
// The pages arrive newest first (by startTime), so a trace is complete once
// a page reaches past its root observation's start: it is emitted then and
// forgotten, and only the traces still open are held. The conversation is
// the session (sessionId, carried on every observation by the trace_context
// fields) or, for a trace without one, the trace. --max-conversations keeps
// the most recent conversations: the first N distinct ones met while paging,
// i.e. the N whose newest observation is newest. A trace of a chosen
// conversation is read wherever it appears; a trace of any other is skipped.
// Once the cap is full, paging continues only until the open traces are
// complete (at most drainPages more pages), so older traces of a chosen
// session beyond that are not read. --max-traces is a hard safety cap on
// traces, whatever the conversation cap.
//
// Written from Langfuse's OpenAPI spec (web/public/generated/api/openapi.yml
// at langfuse/langfuse main, commit 5cb2047, Oct 2026). The older
// GET /api/public/traces and /api/public/observations endpoints are not used:
// they are deprecated, rejected on Langfuse v4 events-only deployments, and
// removed from Langfuse Cloud on 2026-11-16. v2 observations exists since
// Langfuse v3 of Nov 2025, but a v3 deployment serves it only in v4 write
// mode (LANGFUSE_MIGRATION_V4_WRITE_MODE, off by default): on a 404 the
// reader falls back to the v1 traces and observations endpoints, which is
// what a default self-hosted v3 needs (legacy.go).
package langfuse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/netsafe"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

func init() {
	agent.Register(agent.Agent{
		ID:      "langfuse",
		Name:    "Langfuse (custom agents)",
		Summary: "your own analytics agent traced with Langfuse; reads traces over the public API",
		Status:  agent.Experimental,
		Setup: `For an analytics agent you built yourself and trace with Langfuse (cloud or
self-hosted, a version from Nov 2025 or later). The CLI reads the project's
observations over Langfuse's public API with a project API key pair
(Project settings > API keys) and only makes GET requests.

Each trace should be one question: the root observation's input is the
question and its output the answer. Give the trace name your agent uses if
the project also holds other traces, and the name of the tool that runs SQL
if it is not detectable from its input.`,
		Fields: []agent.Field{
			{Key: "host", Label: "Langfuse host", Default: "https://cloud.langfuse.com", Validate: agent.URL(true),
				Help: "https://cloud.langfuse.com (EU), https://us.cloud.langfuse.com (US), or your self-hosted URL"},
			{Key: "public-key", Label: "Public key", Help: "pk-lf-...", Validate: keyValidator("pk-", "sk-", "secret")},
			{Key: "secret-key", Label: "Secret key", Secret: true, Help: "sk-lf-...", Validate: keyValidator("sk-", "pk-", "public"),
				Verify: verifyKeys},
			{Key: "trace-name", Label: "Trace name of the analytics agent", Optional: true,
				Help: "only read traces with this name; leave empty to read every trace in the project"},
			{Key: "sql-tool", Label: "Name of the tool that runs SQL", Optional: true,
				Help: "observations with this name are read as SQL statements; others only when their input starts with SELECT or WITH"},
			{Key: "max-traces", Label: "Maximum traces to read", Default: "5000", Validate: agent.PositiveInt,
				Help: "a hard safety cap on traces, newest first; --max-conversations (sessions, or traces without one) is the usual limit"},
		},
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
}

const (
	endpoint    = "/api/public/v2/observations"
	fetchMethod = "api:" + endpoint
	pageSize    = 100
	fields      = "core,basic,time,io,model,usage,trace_context"
	maxRetries  = 6
	// maxPages bounds the walk: 100,000 observations is past what
	// --max-traces lets through, so reaching it means a server that does
	// not stop paging.
	maxPages = 1000
	// requestTimeout bounds one page, retries aside.
	requestTimeout = 60 * time.Second
	// maxBody bounds one page's response.
	maxBody = 256 << 20
	// drainPages bounds the pages read after a cap is reached, to complete
	// the traces still open.
	drainPages = 10
	// defaultMaxTraces is --max-traces when it is not a positive number.
	defaultMaxTraces = 5000
)

// sleep waits between retries; tests replace it.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// firstPage is the query of the first request. A trace-name filter needs the
// structured filter parameter, which overrides the query-parameter filters,
// so the time bound moves into it too.
func firstPage(in agent.Inputs, opts agent.Options) string {
	return endpoint + "?" + firstQuery(in, opts).Encode()
}

// firstQuery is firstPage's query parameters.
func firstQuery(in agent.Inputs, opts agent.Options) url.Values {
	v := url.Values{"fields": {fields}, "limit": {strconv.Itoa(pageSize)}}
	name := in.Get("trace-name")
	switch {
	case name != "":
		f := []map[string]any{{"type": "string", "column": "traceName", "operator": "=", "value": name}}
		if !opts.Since.IsZero() {
			f = append(f, map[string]any{"type": "datetime", "column": "startTime", "operator": ">=",
				"value": opts.Since.UTC().Format(time.RFC3339)})
		}
		b, _ := json.Marshal(f)
		v.Set("filter", string(b))
	case !opts.Since.IsZero():
		v.Set("fromStartTime", opts.Since.UTC().Format(time.RFC3339))
	}
	return v
}

const (
	firstPurpose = "observations of every trace, newest first (first page)"
	nextPurpose  = "following pages, until there are none or --max-conversations (or --max-traces) is reached"
	nextCursor   = "&cursor=<meta.cursor of the previous page>"
)

func plan(in agent.Inputs, opts agent.Options) []bundle.Query {
	first := firstPage(in, opts)
	return append([]bundle.Query{
		{Purpose: firstPurpose, Statement: "GET " + first},
		{Purpose: nextPurpose, Statement: "GET " + first + nextCursor},
	}, legacyPlan(in, opts)...)
}

type page struct {
	Data []json.RawMessage `json:"data"`
	Meta struct {
		Cursor *string `json:"cursor"`
	} `json:"meta"`
}

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	maxTraces, err := strconv.Atoi(in.Get("max-traces"))
	if err != nil || maxTraces <= 0 {
		maxTraces = defaultMaxTraces
	}
	cl, err := newClient(in)
	if err != nil {
		return agent.Result{}, err
	}

	res := agent.Result{Sampled: true}
	m := mapper{sqlTool: in.Get("sql-tool"), includeResults: opts.IncludeResults, includeUsers: opts.IncludeUsers,
		anonymize: opts.AnonymizeUsers}
	o := newOpen(opts.MaxConversations, maxTraces)
	emitted, noRoot := 0, 0
	send := func(id string, tr *trace) error {
		t, ok := m.turn(id, tr.obs)
		if !ok {
			noRoot++
			return nil
		}
		rec, err := turns.Record(t, columns, fetchMethod)
		if err != nil {
			res.Notes = append(res.Notes, "skipped a trace: "+err.Error())
			return nil
		}
		emitted++
		return emit(rec)
	}

	first := firstPage(in, opts)
	cursor := ""
	seen := map[string]bool{}
	drained := 0
	unfinished := 0
	for n := 1; ; n++ {
		if n > maxPages {
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after %d pages; older traces were not read (narrow with --since)", maxPages))
			break
		}
		path := first
		if n > 1 {
			path = first + "&cursor=" + url.QueryEscape(cursor)
		}
		var p page
		if err := cl.get(ctx, path, &p); err != nil {
			if n == 1 && isNotFound(err) {
				return collectLegacy(ctx, cl, in, opts, m, maxTraces, emit)
			}
			if n == 1 {
				return res, err
			}
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after page %d: %v", n-1, err))
			break
		}
		if n == 1 {
			res.Queries = append(res.Queries, bundle.Query{Purpose: firstPurpose, Statement: "GET " + path, Rows: len(p.Data)})
		} else {
			res.Tally(nextPurpose, "GET "+first+nextCursor, len(p.Data))
		}
		oldest, hasOldest := time.Time{}, false
		for _, raw := range p.Data {
			if at, ok := o.add(raw); ok && (!hasOldest || at.Before(oldest)) {
				oldest, hasOldest = at, true
			}
		}
		// Every trace whose root started after this page's oldest
		// observation has all its observations: send it.
		if hasOldest {
			if err := o.complete(func(tr *trace) bool { return tr.hasRoot && tr.rootStart.After(oldest) }, send); err != nil {
				return res, err
			}
		}
		if p.Meta.Cursor == nil || *p.Meta.Cursor == "" || len(p.Data) == 0 {
			break
		}
		if o.full() {
			if len(o.pending) == 0 {
				break
			}
			if drained++; drained > drainPages {
				unfinished = len(o.pending)
				break
			}
		}
		if seen[*p.Meta.Cursor] {
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after page %d: Langfuse repeated a page cursor", n))
			break
		}
		cursor = *p.Meta.Cursor
		seen[cursor] = true
	}
	// What is still open is as complete as it will get.
	if err := o.complete(func(*trace) bool { return true }, send); err != nil {
		return res, err
	}
	res.Conversations = len(o.chosen)

	if o.convCapped {
		res.Notes = append(res.Notes, fmt.Sprintf("stopped at --max-conversations %s: the most recent sessions (or traces without one) were read, older traces were not (narrow with --since)",
			agent.Thousands(int64(opts.MaxConversations))))
	}
	if o.traceCapped {
		res.Notes = append(res.Notes, fmt.Sprintf("stopped at --max-traces %d; older traces were not read (narrow with --since)", maxTraces))
	}
	if unfinished > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("%d traces were still open when reading stopped and may be missing steps", unfinished))
	}
	if o.late > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("left out %d observations that arrived after their trace was complete (Langfuse did not page newest first)", o.late))
	}
	if noRoot > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("skipped %d traces whose root observation is outside the window", noRoot))
	}
	if emitted == 0 && noRoot > 0 {
		return res, errors.New("no Langfuse trace in the window has a root observation")
	}
	if !opts.IncludeResults {
		res.Redactions = append(res.Redactions, resultsRedaction)
	}
	if m.droppedUsers {
		res.Redactions = append(res.Redactions, usersRedaction)
	}
	return res, nil
}

// trace is one open trace: its observations so far.
type trace struct {
	obs       []map[string]any
	hasRoot   bool
	rootStart time.Time
	key       string // the conversation: session:<id>, or trace:<id> without one
	admitted  bool   // the caps let it in (decided at its root)
}

// openTraces holds the traces read but not yet complete, and what the caps
// have chosen. Everything in it is bounded by the caps: at most maxTraces
// trace ids and convCap conversation keys.
type openTraces struct {
	convCap, maxTraces int
	pending            map[string]*trace
	order              []string        // pending trace ids, in the order they were met
	done               map[string]bool // traces sent
	dropped            map[string]bool // traces the caps turned away
	chosen             map[string]bool // conversation keys
	traces             int
	late               int
	convCapped         bool
	traceCapped        bool
}

func newOpen(convCap, maxTraces int) *openTraces {
	return &openTraces{convCap: convCap, maxTraces: maxTraces, pending: map[string]*trace{},
		done: map[string]bool{}, dropped: map[string]bool{}, chosen: map[string]bool{}}
}

// full reports whether a cap has turned a trace away: no new conversation
// will be read.
func (o *openTraces) full() bool { return o.convCapped || o.traceCapped }

// add files one observation, returning its start time. An observation of a
// trace the caps turned away, or of one already sent, is dropped.
func (o *openTraces) add(raw json.RawMessage) (time.Time, bool) {
	obs, err := decode(raw)
	if err != nil {
		return time.Time{}, false
	}
	at, hasAt := parseTime(obs["startTime"])
	id := str(obs["traceId"])
	if id == "" {
		return at, hasAt
	}
	if o.dropped[id] {
		return at, hasAt
	}
	if o.done[id] {
		o.late++
		return at, hasAt
	}
	tr := o.pending[id]
	if tr == nil {
		if o.traces >= o.maxTraces {
			o.traceCapped = true
			return at, hasAt
		}
		tr = &trace{key: "trace:" + id}
		o.pending[id] = tr
		o.order = append(o.order, id)
	}
	// The session may be only on the root: Langfuse v4 fills a child's
	// sessionId only when the SDK propagates it (an OTel child span
	// otherwise reads ""), and children arrive first, newest first.
	if s := str(obs["sessionId"]); s != "" && !tr.admitted {
		tr.key = "session:" + s
	}
	tr.obs = append(tr.obs, obs)
	if obs["isRootObservation"] == true || str(obs["parentObservationId"]) == "" {
		if t, ok := parseTime(obs["startTime"]); ok {
			tr.hasRoot, tr.rootStart = true, t
		}
		if !tr.admitted && !o.admit(id, tr) {
			return at, hasAt
		}
	}
	return at, hasAt
}

// admit applies the caps to a trace once its root, which carries the
// session, has been read. A trace turned away is dropped.
func (o *openTraces) admit(id string, tr *trace) bool {
	switch {
	case o.traces >= o.maxTraces:
		o.traceCapped = true
	case !o.chosen[tr.key] && o.convCap > 0 && len(o.chosen) >= o.convCap:
		o.convCapped = true
	default:
		o.chosen[tr.key] = true
		o.traces++
		tr.admitted = true
		return true
	}
	delete(o.pending, id)
	o.dropped[id] = true
	for i, p := range o.order {
		if p == id {
			o.order = append(o.order[:i], o.order[i+1:]...)
			break
		}
	}
	return false
}

// complete sends, in the order they were met, the open traces ready says are
// complete, and forgets them.
func (o *openTraces) complete(ready func(*trace) bool, send func(string, *trace) error) error {
	keep := o.order[:0]
	for i, id := range o.order {
		tr := o.pending[id]
		if !ready(tr) {
			keep = append(keep, id)
			continue
		}
		delete(o.pending, id)
		o.done[id] = true
		if err := send(id, tr); err != nil {
			o.order = append(keep, o.order[i+1:]...)
			return err
		}
	}
	o.order = keep
	return nil
}

type client struct {
	base, public, secret string
	http                 *http.Client
}

// newClient checks the host and builds the API client. Collect and Check
// share it.
func newClient(in agent.Inputs) (client, error) {
	host := in.Get("host")
	if host == "" {
		host = "https://cloud.langfuse.com"
	}
	base, err := netsafe.BaseURL(host, true) // a self-hosted Langfuse may sit under a path
	if err != nil {
		return client{}, fmt.Errorf("--host: %w", err)
	}
	return client{base: base, public: in.Get("public-key"), secret: in.Get("secret-key"), http: netsafe.Client(requestTimeout)}, nil
}

// get fetches path into out, retrying with backoff on 429 and gateway errors.
func (c client) get(ctx context.Context, path string, out any) error {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth(c.public, c.secret)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("langfuse %s: %w", c.base, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if len(body) > maxBody {
			return fmt.Errorf("langfuse answered with more than %d bytes", maxBody)
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("langfuse: unexpected response: %w", err)
			}
			return nil
		case retryable(resp.StatusCode) && attempt < maxRetries:
			if err := sleep(ctx, backoff(resp.Header.Get("Retry-After"), attempt)); err != nil {
				return err
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return fmt.Errorf("langfuse rejected the API keys (%d): check the public and secret key and the host's region", resp.StatusCode)
		case resp.StatusCode == http.StatusNotFound:
			return &notFoundError{base: c.base, path: pathOnly(path), body: short(string(body))}
		}
		return fmt.Errorf("langfuse %s: HTTP %d: %s", pathOnly(path), resp.StatusCode, short(string(body)))
	}
}

// notFoundError is a 404. On the v2 observations endpoint it means a
// Langfuse that does not serve it: older than v3 of Nov 2025, or a v3
// deployment not in v4 write mode ("The observations v2 API is only
// available in a Langfuse v4 write mode"), which is every v3 self-hosted
// install by default. Collect then reads the v1 endpoints (legacy.go).
type notFoundError struct{ base, path, body string }

func (e *notFoundError) Error() string {
	return fmt.Sprintf("langfuse %s has no %s (404): %s", e.base, e.path, e.body)
}

func isNotFound(err error) bool {
	var nf *notFoundError
	return errors.As(err, &nf)
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// backoff honours Retry-After (seconds), else doubles from one second up to
// thirty.
func backoff(retryAfter string, attempt int) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, time.Minute)
	}
	return min(time.Second<<attempt, 30*time.Second)
}

func short(s string) string {
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
