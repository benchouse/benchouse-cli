package langfuse

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

// The v1 read, for a Langfuse that has no v2 observations endpoint. A
// self-hosted Langfuse v3 (to v3.225, Oct 2026) answers it with 404 "only
// available in a Langfuse v4 write mode" unless the operator opted into
// LANGFUSE_MIGRATION_V4_WRITE_MODE, so this is the path for most v3
// installs. Verified against langfuse/langfuse:3 (v3.225.15).
//
// Traces are paged newest first (GET /api/public/traces, page-numbered),
// with their input, output, session and user; each chosen trace's
// observations come from GET /api/public/observations?traceId=. On v3 the
// question and answer live on the trace (trace-create, or the SDK copying
// the root span's input and output there); observations carry no session
// or user, so those are copied from the trace.

const (
	legacyTraces    = "/api/public/traces"
	legacyObs       = "/api/public/observations"
	legacyPageSize  = 50
	legacyObsPage   = 100
	legacyTracesFor = "traces, newest first (only when the host has no v2 observations API: Langfuse v3 not in v4 write mode)"
	legacyNextFor   = "following trace pages, until there are none or --max-conversations (or --max-traces) is reached"
	legacyObsFor    = "observations of each trace read (v1)"
	legacyObsStmt   = "GET " + legacyObs + "?traceId=<trace id>&limit=100&page=<n>"
	legacyNote      = "this Langfuse has no v2 observations API (v3 not in v4 write mode): read with the v1 traces and observations endpoints"
)

func legacyQuery(in agent.Inputs, opts agent.Options) url.Values {
	v := url.Values{"limit": {strconv.Itoa(legacyPageSize)}, "orderBy": {"timestamp.desc"}, "fields": {"core,io"}}
	if name := in.Get("trace-name"); name != "" {
		v.Set("name", name)
	}
	if !opts.Since.IsZero() {
		v.Set("fromTimestamp", opts.Since.UTC().Format(time.RFC3339))
	}
	return v
}

func legacyPage(in agent.Inputs, opts agent.Options, n int) string {
	v := legacyQuery(in, opts)
	v.Set("page", strconv.Itoa(n))
	return legacyTraces + "?" + v.Encode()
}

func legacyPlan(in agent.Inputs, opts agent.Options) []bundle.Query {
	return []bundle.Query{
		{Purpose: legacyTracesFor, Statement: "GET " + legacyPage(in, opts, 1)},
		{Purpose: legacyNextFor, Statement: "GET " + legacyNextStmt(in, opts)},
		{Purpose: legacyObsFor, Statement: legacyObsStmt},
	}
}

func legacyNextStmt(in agent.Inputs, opts agent.Options) string {
	v := legacyQuery(in, opts)
	return legacyTraces + "?" + v.Encode() + "&page=<n>"
}

type legacyTracePage struct {
	Data []map[string]any `json:"data"`
	Meta struct {
		Page       int `json:"page"`
		TotalPages int `json:"totalPages"`
		TotalItems int `json:"totalItems"`
	} `json:"meta"`
}

func collectLegacy(ctx context.Context, cl client, in agent.Inputs, opts agent.Options, m mapper, maxTraces int, emit agent.Emit) (agent.Result, error) {
	res := agent.Result{Sampled: true, Notes: []string{legacyNote}}
	chosen := map[string]bool{}
	traces, drained := 0, 0
	convCapped, traceCapped := false, false
	var inWindow int64 = -1
pages:
	for n := 1; n <= maxPages; n++ {
		var p legacyTracePage
		if err := cl.get(ctx, legacyPage(in, opts, n), &p); err != nil {
			if n == 1 {
				return res, neither(cl, err)
			}
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after page %d: %v", n-1, err))
			break
		}
		if n == 1 {
			res.Queries = append(res.Queries, bundle.Query{Purpose: legacyTracesFor, Statement: "GET " + legacyPage(in, opts, 1), Rows: len(p.Data)})
			inWindow = int64(p.Meta.TotalItems)
		} else {
			res.Tally(legacyNextFor, "GET "+legacyNextStmt(in, opts), len(p.Data))
		}
		for _, tr := range p.Data {
			id := str(tr["id"])
			if id == "" {
				continue
			}
			key := "trace:" + id
			if s := str(tr["sessionId"]); s != "" {
				key = "session:" + s
			}
			switch {
			case traces >= maxTraces:
				traceCapped = true
				break pages
			case !chosen[key] && opts.MaxConversations > 0 && len(chosen) >= opts.MaxConversations:
				convCapped = true
				continue
			}
			chosen[key] = true
			traces++
			obs, err := legacyObservations(ctx, cl, id, &res)
			if err != nil {
				return res, err
			}
			t, ok := m.turn(id, legacyRooted(tr, obs))
			if !ok {
				continue
			}
			rec, err := turns.Record(t, columns, fetchMethod)
			if err != nil {
				res.Notes = append(res.Notes, "skipped a trace: "+err.Error())
				continue
			}
			if err := emit(rec); err != nil {
				return res, err
			}
		}
		if len(p.Data) == 0 || p.Meta.TotalPages <= n {
			break
		}
		// A chosen session's older traces may sit on later pages: read a
		// few more once the cap is full, as the v2 read does.
		if convCapped {
			if drained++; drained > drainPages {
				break
			}
		}
	}
	res.Conversations = len(chosen)
	if inWindow >= 0 {
		// totalItems counts traces, not conversations: a note, not InWindow.
		res.Notes = append(res.Notes, fmt.Sprintf("%s traces in the window, %s read", agent.Thousands(inWindow), agent.Thousands(int64(traces))))
	}
	if convCapped {
		res.Notes = append(res.Notes, fmt.Sprintf("stopped at --max-conversations %s: the most recent sessions (or traces without one) were read, older traces were not (narrow with --since)",
			agent.Thousands(int64(opts.MaxConversations))))
	}
	if traceCapped {
		res.Notes = append(res.Notes, fmt.Sprintf("stopped at --max-traces %d; older traces were not read (narrow with --since)", maxTraces))
	}
	if !opts.IncludeResults {
		res.Redactions = append(res.Redactions, resultsRedaction)
	}
	if m.droppedUsers {
		res.Redactions = append(res.Redactions, usersRedaction)
	}
	return res, nil
}

// legacyObservations reads every observation of one trace.
func legacyObservations(ctx context.Context, cl client, traceID string, res *agent.Result) ([]map[string]any, error) {
	var out []map[string]any
	for n := 1; n <= maxPages; n++ {
		v := url.Values{"traceId": {traceID}, "limit": {strconv.Itoa(legacyObsPage)}, "page": {strconv.Itoa(n)}}
		var p legacyTracePage
		if err := cl.get(ctx, legacyObs+"?"+v.Encode(), &p); err != nil {
			return nil, err
		}
		res.Tally(legacyObsFor, legacyObsStmt, len(p.Data))
		out = append(out, p.Data...)
		if len(p.Data) == 0 || p.Meta.TotalPages <= n {
			break
		}
	}
	return out, nil
}

// legacyRooted gives a v1 trace's observations the shape the mapper reads
// from v2: a root observation holding the trace's question and answer,
// and the trace's session, user, name and tags on it. The root is the
// trace's only parentless non-generation observation (an SDK's root span)
// when there is one; otherwise it is made from the trace itself.
func legacyRooted(tr map[string]any, obs []map[string]any) []map[string]any {
	var parentless []map[string]any
	for _, o := range obs {
		if str(o["parentObservationId"]) == "" {
			parentless = append(parentless, o)
		}
		if _, ok := o["totalCost"]; !ok {
			if c, ok := float(o["calculatedTotalCost"]); ok && c > 0 {
				o["totalCost"] = c
			}
		}
	}
	var root map[string]any
	if len(parentless) == 1 && str(parentless[0]["type"]) != "GENERATION" {
		root = parentless[0]
	} else {
		root = map[string]any{"id": "trace:" + str(tr["id"]), "type": "SPAN", "startTime": tr["timestamp"]}
		var end time.Time
		for _, o := range obs {
			if t, ok := parseTime(o["endTime"]); ok && t.After(end) {
				end = t
			}
			if str(o["parentObservationId"]) == "" && str(o["level"]) == "ERROR" && root["level"] == nil {
				root["level"], root["statusMessage"] = "ERROR", o["statusMessage"]
			}
		}
		if !end.IsZero() {
			root["endTime"] = end.UTC().Format(time.RFC3339Nano)
		} else if tr["output"] != nil {
			root["endTime"] = tr["timestamp"]
		}
		obs = append(obs, root)
	}
	root["isRootObservation"] = true
	if tr["input"] != nil {
		root["input"] = tr["input"]
	}
	if tr["output"] != nil {
		root["output"] = tr["output"]
	}
	for k, from := range map[string]string{"sessionId": "sessionId", "userId": "userId", "traceName": "name",
		"tags": "tags", "environment": "environment", "release": "release"} {
		if v, ok := tr[from]; ok && v != nil {
			root[k] = v
		}
	}
	return obs
}

// neither is the error for a host that serves no traces API at all.
func neither(cl client, err error) error {
	if isNotFound(err) {
		return fmt.Errorf("langfuse %s serves neither %s nor %s (404): is this a Langfuse host? (a version from 2024 or later is needed)", cl.base, endpoint, legacyTraces)
	}
	return err
}
