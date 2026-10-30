// Package thoughtspot reads ThoughtSpot Spotter, ThoughtSpot's AI analyst,
// over the REST API v2.0.
//
// Two routes, chosen with --route (auto by default):
//
//  1. analytics-model (primary): the "TS: Spotter Analytics Model" system
//     model behind the Spotter Conversations Liveboard, which records every
//     user's questions (User Query), Spotter's replies (Response) and the
//     upvote/downvote (User Feedback Rating) for 180 days, visible to
//     administrators by default. The model is found by name with POST
//     /metadata/search (include_details, for its columns); ThoughtSpot does
//     not publish the full column list, so the columns are matched by name
//     (analytics.go) and the query is built from what is there. It is read
//     with POST /searchdata, newest first, 1,000 rows a page; each row is
//     one turn, the rating goes to extra.feedback.
//  2. saved-conversations (secondary, best effort): GET
//     /ai/agent/conversations lists the signed-in user's OWN saved
//     conversations (26.7.0.cl or later, CAN_USE_SPOTTER; unsaved
//     conversations are not persisted); GET .../{id}/messages gives each
//     prompt and its response items, and GET .../answers/{id}/details each
//     answer's search tokens and formulas, kept as a thoughtspot_answer
//     tool call. ThoughtSpot generates search tokens, not SQL, so no
//     statements are recorded.
//
// auto reads the analytics model and falls back to saved conversations when
// the model cannot be read (missing, not shared, 403). The routes are never
// both read, so no question is sent twice.
//
// Authentication is a bearer token: given with --token, or obtained with
// POST /auth/token/full from --username and a trusted-auth secret key or a
// password.
//
// Written from ThoughtSpot's docs, read 2026-10-30:
// https://docs.thoughtspot.com/cloud/latest/spotter-conversations-liveboard,
// https://developers.thoughtspot.com/docs/spotter-agent-conversation-mgmt-apis,
// https://developers.thoughtspot.com/docs/api-authv2,
// https://developers.thoughtspot.com/docs/fetch-data-and-report-apis and
// https://developers.thoughtspot.com/docs/rest-apiv2-metadata-search. Not
// pinned by the docs and read defensively: the analytics model's column
// names besides the three documented ones, the searchdata response
// (contents[0].column_names/data_rows), the search syntax of the --since
// filter ([Timestamp] >= 'MM/DD/YYYY'), whether the timestamp comes back
// bucketed by day (a note says so), and the timestamp formats of the
// conversation APIs. Tested against fixtures only.
package thoughtspot

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/files"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

func init() {
	notToken := func(in agent.Inputs) bool { return authMode(in) != authToken }
	agent.Register(agent.Agent{
		ID:      "thoughtspot",
		Name:    "ThoughtSpot Spotter",
		Summary: "ThoughtSpot's AI analyst; reads the Spotter analytics model, or your saved Spotter conversations",
		Status:  agent.Experimental,
		Setup:   setup,
		Fields: []agent.Field{
			{Key: "host", Label: "ThoughtSpot URL", Validate: agent.URL(false),
				Help: "your instance, e.g. https://acme.thoughtspot.cloud (https only)"},
			{Key: "auth", Label: "Sign in with", Default: authToken, Choices: []string{authToken, authSecret, authPassword},
				Help: "token = a REST API v2 bearer token; secret-key = username + trusted-auth secret key; password = username + password"},
			{Key: "username", Label: "Username", When: notToken, Validate: agent.Token,
				Help: "the ThoughtSpot user to sign in as (an administrator sees every user's Spotter questions)"},
			{Key: "token", Label: "Bearer token", Secret: true, Validate: agent.Token, Verify: verify,
				When: func(in agent.Inputs) bool { return authMode(in) == authToken },
				Help: "from POST /api/rest/2.0/auth/token/full, or the REST Playground"},
			{Key: "secret", Label: "Secret key or password", Secret: true, When: notToken, Verify: verify,
				Help: "the trusted-auth secret key (Develop > Security settings) or the user's password, per --auth"},
			{Key: "org-id", Label: "Org ID", Optional: true, When: notToken, Validate: agent.PositiveInt,
				Help: "the Org to sign in to; leave empty for the primary Org"},
			{Key: "route", Label: "What to read", Default: routeAuto, Choices: []string{routeAuto, routeModel, routeSaved},
				Help: "auto = the Spotter analytics model, else your own saved conversations"},
			{Key: "model", Label: "Spotter analytics model name", Default: defaultModel,
				When: func(in agent.Inputs) bool { return in.Get("route") != routeSaved },
				Help: "the system model behind the Spotter Conversations Liveboard"},
		},
		Context: &agent.Context{
			Label: "ThoughtSpot TML folder",
			Help:  "an export of your models as TML (*.model.tml, *.worksheet.tml, *.table.tml), if you have one",
			Check: checkContext,
			Keep:  keepContext,
		},
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
}

const setup = `The CLI reads ThoughtSpot's REST API v2.0 and changes nothing (its POSTs are
the sign-in, a metadata search and searchdata queries).

  - Everyone's Spotter questions, replies and ratings: the
    "TS: Spotter Analytics Model" system model (180 days). Administrators can
    read it by default; otherwise share the model with the user.
  - Fallback, your own saved conversations only: ThoughtSpot 26.7.0.cl or
    later and the CAN_USE_SPOTTER privilege. Unsaved conversations are not
    kept by ThoughtSpot.

Sign in with a bearer token, or a username with a trusted-auth secret key or
password.`

const (
	routeAuto  = "auto"
	routeModel = "analytics-model"
	routeSaved = "saved-conversations"
)

// Request purposes, shared by Plan and Result.Queries.
const (
	purposeLogin      = "sign in: exchange the username and secret for a bearer token"
	purposeSession    = "the signed-in user and their privileges"
	purposeMetadata   = "find the Spotter analytics model and list its columns"
	purposeSearch     = "Spotter questions, responses and ratings from the analytics model, newest first (first page)"
	purposeSearchNext = "following pages, until there are none or --max-conversations is reached"
	purposeConvs      = "the user's saved Spotter conversations (headers)"
	purposeMessages   = "the messages of a chosen saved conversation"
	purposeAnswer     = "an answer's search tokens and formulas"
)

func route(in agent.Inputs) string {
	if r := in.Get("route"); r != "" {
		return r
	}
	return routeAuto
}

func modelName(in agent.Inputs) string {
	if m := in.Get("model"); m != "" {
		return m
	}
	return defaultModel
}

func plan(in agent.Inputs, opts agent.Options) []bundle.Query {
	var out []bundle.Query
	if authMode(in) != authToken {
		out = append(out, bundle.Query{Purpose: purposeLogin, Statement: loginStatement(authMode(in))})
	}
	out = append(out, bundle.Query{Purpose: purposeSession, Statement: "GET " + apiPrefix + sessionPath})
	r := route(in)
	if r != routeSaved {
		q := "<the model's question, response, rating, time, conversation and user columns> " +
			"[<time>] >= '<since, MM/DD/YYYY>' sort by [<time>] descending"
		out = append(out,
			bundle.Query{Purpose: purposeMetadata, Statement: metadataStatement(modelName(in))},
			bundle.Query{Purpose: purposeSearch, Statement: searchStatement("<model id>", q, "0")},
			bundle.Query{Purpose: purposeSearchNext, Statement: searchStatement("<model id>", q, "<1000, 2000, ...>")},
		)
	}
	if r != routeModel {
		suffix := ""
		if r == routeAuto {
			suffix = " (only when the analytics model cannot be read)"
		}
		out = append(out,
			bundle.Query{Purpose: purposeConvs + suffix, Statement: convsListStatement},
			bundle.Query{Purpose: purposeMessages + suffix, Statement: messagesStatement},
			bundle.Query{Purpose: purposeAnswer + suffix, Statement: answerStatement},
		)
	}
	return out
}

// signIn builds the client, signs in if needed and reads the session user.
func signIn(ctx context.Context, in agent.Inputs, res *agent.Result) (*client, sessionUser, error) {
	cl, err := newClient(in)
	if err != nil {
		return nil, sessionUser{}, err
	}
	if cl.mode != authToken {
		if err := cl.login(ctx); err != nil {
			return nil, sessionUser{}, err
		}
		if res != nil {
			res.Queries = append(res.Queries, bundle.Query{Purpose: purposeLogin, Statement: loginStatement(cl.mode), Rows: 1})
		}
	}
	u, err := cl.session(ctx)
	if err != nil {
		return nil, u, err
	}
	if res != nil {
		res.Queries = append(res.Queries, bundle.Query{Purpose: purposeSession, Statement: "GET " + apiPrefix + sessionPath, Rows: 1})
	}
	return cl, u, nil
}

func (u sessionUser) label() string {
	if u.Name != "" {
		return u.Name
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.ID
}

// verify signs in, for the token and secret fields.
func verify(ctx context.Context, in agent.Inputs) (string, error) {
	_, u, err := signIn(ctx, in, nil)
	if err != nil {
		return "", err
	}
	return "Signed in to ThoughtSpot as " + u.label(), nil
}

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	res := agent.Result{Sampled: opts.MaxConversations > 0}
	cl, u, err := signIn(ctx, in, &res)
	if err != nil {
		return res, err
	}
	r := route(in)
	name := modelName(in)
	if r != routeSaved {
		err := func() error {
			m, err := findModel(ctx, cl, name)
			if err != nil {
				res.Queries = append(res.Queries, bundle.Query{Purpose: purposeMetadata, Statement: metadataStatement(name)})
				return err
			}
			res.Queries = append(res.Queries, bundle.Query{Purpose: purposeMetadata, Statement: metadataStatement(name), Rows: 1})
			res.Sources = append(res.Sources, bundle.Source{SourceKind: turns.SourceKind, NativeID: m.ID, Account: cl.base, Label: m.Name})
			return readAnalytics(ctx, cl, m, opts, &res, emit)
		}()
		switch {
		case err == nil:
			return res, nil
		case r == routeModel || !analyticsUnreadable(err) || ctx.Err() != nil:
			return res, explainAnalytics(err, name)
		}
		res.Notes = append(res.Notes, "the Spotter analytics model could not be read, so only your own saved conversations were: "+
			agent.Clip(explainAnalytics(err, name)))
	}
	if err := readSaved(ctx, cl, u.ID, opts, &res, emit); err != nil {
		if isEmitErr(err) {
			return res, err.(emitErr).error
		}
		return res, explainSaved(err)
	}
	res.Sources = append(res.Sources, bundle.Source{SourceKind: turns.SourceKind, NativeID: "saved-conversations", Account: cl.base,
		Label: "Spotter saved conversations of " + u.label()})
	return res, nil
}

func check(ctx context.Context, in agent.Inputs, opts agent.Options) (agent.CheckResult, error) {
	var res agent.CheckResult
	cl, u, err := signIn(ctx, in, nil)
	if err != nil {
		return res, err
	}
	res.Facts = append(res.Facts, "Signed in to ThoughtSpot at "+cl.base+" as "+u.label())
	r := route(in)
	name := modelName(in)
	var modelErr, savedErr error
	if r != routeSaved {
		modelErr = checkModel(ctx, cl, name, opts, &res)
		if modelErr != nil {
			modelErr = explainAnalytics(modelErr, name)
		}
	}
	if r != routeModel {
		savedErr = checkSaved(ctx, cl, &res)
		if savedErr != nil {
			savedErr = explainSaved(savedErr)
		}
	}
	var missing []string
	if len(u.Privileges) > 0 {
		if r != routeSaved && !u.has("ADMINISTRATION") && modelErr != nil {
			missing = append(missing, "ADMINISTRATION (or the analytics model shared with the user)")
		}
		if r != routeModel && !u.has("CAN_USE_SPOTTER") {
			missing = append(missing, "CAN_USE_SPOTTER")
		}
	}
	if len(missing) > 0 {
		res.Warnings = append(res.Warnings, "the user lacks "+strings.Join(missing, " and "))
	}
	switch r {
	case routeModel:
		return res, modelErr
	case routeSaved:
		return res, savedErr
	}
	switch {
	case modelErr != nil && savedErr != nil:
		return res, fmt.Errorf("neither route can be read: %v; and %v", modelErr, savedErr)
	case modelErr != nil:
		res.Warnings = append(res.Warnings, agent.Clip(modelErr)+": only your own saved conversations will be read")
	case savedErr != nil:
		res.Warnings = append(res.Warnings, "saved conversations (the fallback) cannot be read: "+agent.Clip(savedErr))
	}
	return res, nil
}

// checkModel finds the model and counts its rows and conversations in the
// window with one-row searchdata queries.
func checkModel(ctx context.Context, cl *client, name string, opts agent.Options, res *agent.CheckResult) error {
	m, err := findModel(ctx, cl, name)
	if err != nil {
		return err
	}
	var found []string
	for _, r := range roleOrder {
		if m.has(r) {
			found = append(found, string(r))
		}
	}
	res.Facts = append(res.Facts, fmt.Sprintf("analytics model %q readable (columns: %s)", m.Name, strings.Join(found, ", ")))
	if m.Guessed {
		res.Warnings = append(res.Warnings, "the model's columns could not be listed; the documented column names are assumed")
	}
	var sr searchResult
	if err := cl.post(ctx, searchPath, searchBody(m.ID, m.queryString(opts), 0, 1), &sr); err != nil {
		return err
	}
	if len(sr.Contents) > 0 && sr.Contents[0].Available != nil {
		n := *sr.Contents[0].Available
		if n == 0 {
			res.Warnings = append(res.Warnings, "no Spotter questions "+agent.Window(opts)+" in the analytics model (it keeps 180 days)")
		} else {
			res.Facts = append(res.Facts, agent.Counted(n, "Spotter question", "Spotter questions", opts))
		}
	}
	if !m.has(roleConversation) {
		return nil
	}
	var cr searchResult
	q := "unique count [" + m.Cols[roleConversation] + "]" + strings.Replace(m.tail(opts), " sort by ["+m.Cols[roleTime]+"] descending", "", 1)
	if err := cl.post(ctx, searchPath, searchBody(m.ID, q, 0, 1), &cr); err != nil {
		return nil // the count is a nicety
	}
	if _, rows, err := cr.rows(); err == nil && len(rows) == 1 {
		for _, v := range rows[0] {
			if n, ok := asInt(v); ok {
				res.Facts = append(res.Facts, agent.Conversations(n, "conversation", "conversations", opts))
				res.Conversations = &n
			}
		}
	}
	return nil
}

func asInt(v any) (int64, bool) {
	if f, ok := v.(float64); ok && f >= 0 {
		return int64(f), true
	}
	return 0, false
}

func checkSaved(ctx context.Context, cl *client, res *agent.CheckResult) error {
	var l convList
	p := strings.Replace(convsList(0), "limit=50", "limit=1", 1)
	if err := cl.get(ctx, p, &l); err != nil {
		return err
	}
	if len(l.Conversations) == 0 {
		res.Facts = append(res.Facts, "saved conversations readable: none saved by this user")
	} else {
		res.Facts = append(res.Facts, "saved conversations readable (this user's own)")
	}
	return nil
}

var errNoTML = errors.New("no TML")

// keepContext keeps TML files and docs; connection TML is left out (it
// names the warehouse connection and may hold credentials).
func keepContext(rel string) bool {
	b := strings.ToLower(path.Base(rel))
	if strings.HasSuffix(b, ".connection.tml") || strings.HasPrefix(b, ".") {
		return false
	}
	return strings.HasSuffix(b, ".tml") || strings.HasSuffix(b, ".md")
}

// checkContext confirms dir holds exported TML models, worksheets, views or
// tables.
func checkContext(dir string) (agent.CheckResult, error) {
	var res agent.CheckResult
	sum, err := files.Scan(dir, keepContext)
	if err != nil {
		return res, err
	}
	if err := sum.Limits(dir); err != nil {
		return res, err
	}
	kinds := map[string]int{}
	for _, rel := range sum.Kept {
		b := strings.ToLower(path.Base(rel))
		if !strings.HasSuffix(b, ".tml") {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(b, ".tml"), ".")
		kind := "other"
		if len(parts) > 1 {
			kind = parts[len(parts)-1]
		}
		kinds[kind]++
	}
	data := kinds["model"] + kinds["worksheet"] + kinds["table"] + kinds["view"] + kinds["sql_view"]
	if data == 0 {
		return res, fmt.Errorf("%w in %s: expected exported *.model.tml, *.worksheet.tml, *.table.tml or *.view.tml files", errNoTML, dir)
	}
	var parts []string
	for _, k := range []string{"model", "worksheet", "view", "sql_view", "table", "answer", "liveboard"} {
		if n := kinds[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	res.Facts = append(res.Facts, "ThoughtSpot TML: "+strings.Join(parts, ", "),
		fmt.Sprintf("%d files (%s) will be included", len(sum.Kept), agent.Bytes(sum.KeptBytes)))
	if len(sum.TooLarge) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d files are too large and will be left out", len(sum.TooLarge)))
	}
	return res, nil
}
