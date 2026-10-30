// Package looker reads Looker's AI assistant: Conversational Analytics
// (Gemini in Looker), over the Looker API 4.0.
//
// Which history a Looker instance exposes depends on its version, its
// preview features and who may impersonate whom, so the reader is layered
// and finds out at run time:
//
//  1. Default: one login as the API client, no impersonation.
//     (a) The System Activity Conversational Analytics explore, through
//     run_inline_query: one turn per question in the --since window, with
//     the rating (extra.feedback), answer success (outcome), latency
//     (completed_at), user id, time and conversation id when the explore has
//     them. Looker does not document this explore, so its name and fields
//     are discovered from the system__activity model's metadata by name
//     pattern (see explore.go). Question text is recorded only for users
//     who consented to sharing their queries while the End User
//     Conversational Analytics Query Review preview is on.
//     (b) Best effort: the System Activity History explore, filtered to
//     queries whose API Client Name is Conversational Analytics. Each such
//     query's SQL (GET /queries/{id}/run/sql, which renders the SQL without
//     running it) and definition (GET /queries/{id}) become a statement and
//     a looker_query tool call on the question of the same user it follows:
//     logged from 5s before the question to 2 minutes after it (or its
//     latency + 30s, up to 10 minutes), and before that user's next
//     question. Unmatched queries are counted in a note. The answer text is
//     not in System Activity: answer_text is null, and a note says so.
//  2. --per-user yes (opt-in): search_conversations returns only the
//     caller's own conversations, so the API client logs in as each user
//     with conversations in the window (POST /login/{user_id}, admin only),
//     lists their conversations, reads the most recent across users
//     (all_conversation_messages) and logs out every such session. These
//     turns carry the full answer, generatedSql and generatedLookerQuery,
//     and are enriched with the System Activity rating, success and latency
//     of the same question. Users Looker will not log in as keep their
//     System Activity turns; a client that may not sudo at all falls back
//     to the default.
//
// No customer data is read: the generated queries are never run, and with
// --include-results off the result rows a conversation message carries are
// dropped. The user directory is not read; user ids are kept (and
// pseudonymised by --anonymize-users).
//
// Written from the Looker API 4.0 OpenAPI spec, version 4.0.26.22 (Looker
// 26.22), spec/Looker.4.0.oas.json at looker-open-source/sdk-codegen commit
// ad0a156 (2026-10-26), and the Looker docs on System Activity
// (usage-reports-with-system-activity-explores, system-activity-dashboards)
// and API authorization, read Oct 2026. The conversation endpoints are beta
// in that spec, and ConversationMessage.message is untyped there: it is read
// as the spec's ChatMessage. Tested against fixtures only.
package looker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/files"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

func init() {
	agent.Register(agent.Agent{
		ID:      "looker",
		Name:    "Looker Conversational Analytics",
		Summary: "Gemini in Looker (Conversational Analytics); reads System Activity and, opt-in, each user's conversations",
		Status:  agent.Experimental,
		Setup:   setup,
		Fields: []agent.Field{
			{Key: "host", Label: "Looker instance URL", Help: "e.g. https://acme.cloud.looker.com (https only)"},
			{Key: "client-id", Label: "API client ID", Help: "an API key of a Looker user (Admin > Users > Edit > API keys)"},
			{Key: "client-secret", Label: "API client secret", Secret: true, Help: "the secret of that API key"},
			{Key: "per-user", Label: "Read full answers per user", Default: "no", Choices: []string{"no", "yes"},
				Help: "Logs in as each user (admin impersonation) to read full answers and generated SQL. " +
					"Off by default — your security team may want to approve this"},
		},
		Context: &agent.Context{
			Label: "LookML project folder",
			Help:  "the LookML project your Explores come from (its *.model.lkml and *.view.lkml files)",
			Check: checkContext,
			Keep:  keepContext,
		},
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
}

const setup = `The CLI reads Looker's API 4.0 with an API key (client ID and secret) and
only reads: System Activity queries, query definitions and SQL (never run),
and, with --per-user, conversations.

The API key's user needs:
  - see_system_activity (or the Admin role): the System Activity
    Conversational Analytics and History explores. Question text, ratings
    and answer success appear there only while the End User Conversational
    Analytics Query Review preview is on, for users who consented.
  - only with --per-user yes: the Admin role (sudo), to log in as each user
    and read their own conversations with full answers. Each session is
    logged out right after.`

const fetchMethod = "api:looker"

const sourceKind = turns.SourceKind

// Request purposes, shared by Plan and Result.Queries.
const (
	purposeLogin      = "log in as the API client (client id and secret in the form body)"
	purposeModel      = "list the System Activity explores, to find the Conversational Analytics one"
	purposeCAFields   = "fields of a candidate Conversational Analytics explore (names are discovered, not assumed)"
	purposeHistFields = "fields of the System Activity History explore"
	purposeHistory    = "queries Conversational Analytics issued (History, by API Client Name), newest first"
	purposeCA         = "Conversational Analytics questions, newest first (first page)"
	purposeCANext     = "following pages, before the oldest question read, until --max-conversations or the window ends"
	purposeSQL        = "SQL of a matched query (result_format=sql renders it without running it)"
	purposeDef        = "definition of a matched query (model, explore, fields, filters)"
	purposeUsers      = "--per-user: users with Conversational Analytics questions in the window"
	purposeSudo       = "--per-user: log in as a user (login_user, admin only)"
	purposeConvs      = "--per-user: that user's conversations, most recently updated first (ids and times)"
	purposeMessages   = "--per-user: the messages of a chosen conversation"
	purposeSudoLogout = "--per-user: log out that user's session"
	purposeLogout     = "log out the API client's session"
)

var (
	loginStatement      = "POST " + apiPrefix + "/login client_id=<client id>&client_secret=<client secret>"
	modelStatement      = "GET " + apiPrefix + modelPath
	caFieldsStatement   = "GET " + apiPrefix + fmt.Sprintf(explorePath, "<conversational analytics explore>")
	histFieldsStatement = "GET " + apiPrefix + fmt.Sprintf(explorePath, historyExplore)
	caNextStatement     = "POST " + apiPrefix + runPath + " <the first page's query, with the time filter \"<since> to <oldest time read + 1s>\">"
	sudoStatement       = "POST " + apiPrefix + "/login/<user id>"
	convsStatement      = "GET " + apiPrefix + strings.Replace(searchPath(0), "offset=0", "offset=<0, 100, ...>", 1)
	messagesStatement   = "GET " + apiPrefix + fmt.Sprintf(messagesPath, "<conversation id>")
	sudoLogoutStatement = "DELETE " + apiPrefix + "/logout (with the user's token)"
	logoutStatement     = "DELETE " + apiPrefix + "/logout"
)

// placeholderCA stands for the discovered Conversational Analytics fields
// in Plan.
var placeholderCA = caFields{Explore: "<conversational analytics explore>", Question: "<question>", Time: "<created time>",
	User: "<user id>", Conversation: "<conversation id>", Rating: "<rating>", Success: "<answer success>", Latency: "<latency>"}

// documentedHistory are the History fields Plan shows; Collect uses those
// the explore actually has.
var documentedHistory = histFields{QueryID: "query.id", User: "user.id", Time: "history.created_time",
	APIClient: "query_api_client_context.name", Source: "history.source", Status: "history.status",
	Model: "query.model", View: "query.view"}

func plannedQuery(purpose, stmt string, rows int) bundle.Query {
	return bundle.Query{Purpose: purpose, Statement: stmt, Rows: rows}
}

func plan(in agent.Inputs, opts agent.Options) []bundle.Query {
	q := []bundle.Query{
		{Purpose: purposeLogin, Statement: loginStatement},
		{Purpose: purposeModel, Statement: modelStatement},
		{Purpose: purposeHistFields, Statement: histFieldsStatement},
		{Purpose: purposeHistory, Statement: historyQuery(documentedHistory, opts.Since, historyLimit(opts.MaxConversations)).statement()},
		{Purpose: purposeCAFields, Statement: caFieldsStatement},
		{Purpose: purposeCA, Statement: caQuery(placeholderCA, opts.Since, time.Time{}, false, caPageSize).statement()},
		{Purpose: purposeCANext, Statement: caNextStatement},
		{Purpose: purposeSQL, Statement: "GET " + apiPrefix + fmt.Sprintf(sqlPath, "<query id>")},
		{Purpose: purposeDef, Statement: "GET " + apiPrefix + fmt.Sprintf(defPath, "<query id>")},
	}
	if perUser(in) {
		q = append(q,
			bundle.Query{Purpose: purposeUsers, Statement: usersQuery(placeholderCA, opts.Since).statement()},
			bundle.Query{Purpose: purposeSudo, Statement: sudoStatement},
			bundle.Query{Purpose: purposeConvs, Statement: convsStatement},
			bundle.Query{Purpose: purposeMessages, Statement: messagesStatement},
			bundle.Query{Purpose: purposeSudoLogout, Statement: sudoLogoutStatement},
		)
	}
	return append(q, bundle.Query{Purpose: purposeLogout, Statement: logoutStatement})
}

func perUser(in agent.Inputs) bool { return strings.EqualFold(in.Get("per-user"), "yes") }

// usersQuery lists the distinct users with questions in the window.
func usersQuery(f caFields, since time.Time) inlineQuery {
	q := inlineQuery{Model: saModel, View: f.Explore, Fields: []string{f.User}, Sorts: []string{f.User},
		Limit: fmt.Sprint(maxUsers), QueryTimezone: "UTC"}
	if w := window(since, time.Time{}); w != "" {
		q.Filters = map[string]string{f.Time: w}
	}
	return q
}

// run is one collection's state.
type run struct {
	c       *client
	opts    agent.Options
	res     agent.Result
	m       *matcher
	cols    turns.Columns
	emitFn  agent.Emit
	emitErr error
	emitted int
	// emittedSA counts the turns read from System Activity alone.
	emittedSA int
	convs     int
	capped    bool
	fetchErr  string
	sudoErr   string
	convErr   string

	fetchFailed, logoutFailed int
}

func (r *run) emit(t turns.Turn) error {
	rec, err := turns.Record(t, r.cols, fetchMethod)
	if err != nil {
		r.res.Notes = append(r.res.Notes, "skipped a turn: "+err.Error())
		return nil
	}
	r.emitted++
	if err := r.emitFn(rec); err != nil {
		r.emitErr = err
		return err
	}
	return nil
}

// stopErr reports whether err must end the collection: the CLI refused a
// record, or the run was cancelled.
func (r *run) stopErr(err error) bool {
	return err != nil && (errors.Is(err, r.emitErr) && r.emitErr != nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded))
}

// connect logs in and reads what System Activity offers. Check and Collect
// share it.
type found struct {
	explores []string
	ca       caFields
	caOK     bool
	caTried  []string
	hist     histFields
	histOK   bool
	histErr  error
}

func needSystemActivity(err error) error {
	switch statusOf(err) {
	case http.StatusForbidden, http.StatusUnauthorized, http.StatusNotFound:
		return fmt.Errorf("Looker refused System Activity (HTTP %d): the API key's user needs the see_system_activity permission "+
			"(or the Admin role) to read the system__activity model", statusOf(err))
	}
	return fmt.Errorf("reading System Activity: %w", err)
}

// discover finds the System Activity explores and fields to read, tallying
// the metadata requests on res.
func discover(ctx context.Context, c *client, res *agent.Result) (found, error) {
	var f found
	ex, err := c.explores(ctx)
	if err != nil {
		return f, needSystemActivity(err)
	}
	res.Queries = append(res.Queries, plannedQuery(purposeModel, modelStatement, len(ex)))
	f.explores = ex
	for _, name := range ex {
		if name != historyExplore {
			continue
		}
		fs, err := c.exploreFields(ctx, historyExplore)
		if err != nil {
			f.histErr = err
			break
		}
		res.Tally(purposeHistFields, histFieldsStatement, len(fs))
		f.hist, f.histOK = pickHistory(fs)
	}
	for _, name := range caCandidates(ex) {
		fs, err := c.exploreFields(ctx, name)
		if err != nil {
			continue
		}
		res.Tally(purposeCAFields, caFieldsStatement, len(fs))
		f.caTried = append(f.caTried, name)
		if ca, ok := pickCA(name, fs); ok {
			f.ca, f.caOK = ca, true
			break
		}
	}
	return f, nil
}

func (f found) caMissing() string {
	if len(f.caTried) == 0 {
		return "System Activity has no Conversational Analytics explore (none of its explores is named like one)"
	}
	return "System Activity explores " + strings.Join(f.caTried, ", ") + " hold no question text and time field"
}

func (f found) histMissing() string {
	switch {
	case f.histErr != nil:
		return "System Activity History could not be read: " + clip(f.histErr.Error(), 200)
	case !contains(f.explores, historyExplore):
		return "System Activity has no History explore"
	}
	return "System Activity History has no recognisable AI source (no query_api_client_context.name field: " +
		"API Client Name needs a recent Looker); statements will be missing"
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func columns(f found, perUser, includeUsers bool) turns.Columns {
	cols := []string{turns.ColCompletedAt, turns.ColOutcome, turns.ColErrorType, turns.ColErrorMessage, turns.ColUserID,
		turns.ColSemanticModelRef, turns.ColStatements, turns.ColToolCalls, turns.ColExtra}
	if perUser || f.ca.Conversation != "" {
		cols = append(cols, turns.ColConversationID)
	}
	if perUser {
		cols = append(cols, turns.ColPosition, turns.ColReasoning, turns.ColMessages)
	}
	if includeUsers && (perUser || f.ca.UserName != "" || f.ca.UserEmail != "") {
		cols = append(cols, turns.ColUserName, turns.ColUserEmail)
	}
	return turns.NewColumns(cols...)
}

const (
	answerNote = "answer_text is null on System Activity turns: System Activity records the question, rating, answer success " +
		"and latency, not the answer. --per-user reads full answers."
	resultsRedaction = "no query result rows read: the generated queries are not run (their SQL is rendered with result_format=sql); " +
		"result rows, code output and chart specs in conversation messages removed. Use --include-results to keep them."
	usersRedaction = "Looker user directory not read: user ids kept, names and emails left out. Use --include-users to keep the names System Activity shows."
)

func collect(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
	c, err := newClient(in.Get("host"), in.Get("client-id"), in.Get("client-secret"))
	if err != nil {
		return agent.Result{}, err
	}
	r := &run{c: c, opts: opts, emitFn: emit, res: agent.Result{Sampled: opts.MaxConversations > 0}}
	if err := c.login(ctx); err != nil {
		return r.res, err
	}
	r.res.Queries = append(r.res.Queries, plannedQuery(purposeLogin, loginStatement, 1))
	err = r.collect(ctx, in)
	if lerr := c.logoutAdmin(ctx); lerr == nil {
		r.res.Tally(purposeLogout, logoutStatement, 1)
	}
	return r.res, err
}

func (r *run) collect(ctx context.Context, in agent.Inputs) error {
	c, opts := r.c, r.opts
	f, err := discover(ctx, c, &r.res)
	if err != nil {
		return err
	}
	if f.histOK {
		m, q, n, err := readHistory(ctx, c, f.hist, opts.Since, historyLimit(opts.MaxConversations))
		if err != nil {
			f.histOK, f.histErr = false, err
		} else {
			r.m = m
			r.res.Queries = append(r.res.Queries, plannedQuery(purposeHistory, q.statement(), n))
			if n >= historyLimit(opts.MaxConversations) {
				r.res.Notes = append(r.res.Notes, fmt.Sprintf("read the %s most recent Conversational Analytics queries in History; older ones are not matched (narrow with --since)",
					agent.Thousands(int64(n))))
			}
		}
	}
	if !f.histOK {
		r.res.Notes = append(r.res.Notes, f.histMissing())
	}
	if !f.caOK {
		r.res.Notes = append(r.res.Notes, f.caMissing())
	}
	if !f.caOK && !f.histOK {
		return errors.New(f.caMissing() + "; " + f.histMissing())
	}
	pu := perUser(in)
	r.cols = columns(f, pu, opts.IncludeUsers)

	handled := false
	if pu {
		if handled, err = r.perUser(ctx, f); err != nil {
			return err
		}
	}
	if !handled && f.caOK {
		ch := newChooser(opts.MaxConversations)
		if err := r.systemActivityTurns(ctx, f.ca, ch, nil, nil); err != nil {
			return err
		}
		r.convs += len(ch.chosen)
		r.capped = r.capped || ch.capped
	}
	if !handled || r.emittedSA > 0 {
		r.res.Notes = append(r.res.Notes, answerNote)
	}
	r.finish(f)
	if r.emitted == 0 && !handled && r.m != nil && r.m.total > 0 {
		return fmt.Errorf("System Activity History holds %s Conversational Analytics queries %s but no question text was readable "+
			"(%s); the End User Conversational Analytics Query Review preview may be off, or run with --per-user yes to read conversations",
			agent.Thousands(int64(r.m.total)), agent.Window(opts), strings.TrimSuffix(f.caMissing(), "."))
	}
	return nil
}

// perUser runs the --per-user layer. handled is false when the client may
// not log in as users, so the default layer runs instead.
func (r *run) perUser(ctx context.Context, f found) (bool, error) {
	users := map[string]bool{}
	if f.caOK && f.ca.User != "" {
		q := usersQuery(f.ca, r.opts.Since)
		rows, err := r.c.run(ctx, "", q)
		if err != nil {
			r.res.Notes = append(r.res.Notes, "--per-user: could not list the users with questions: "+clip(err.Error(), 200))
		} else {
			r.res.Queries = append(r.res.Queries, plannedQuery(purposeUsers, q.statement(), len(rows)))
			for _, row := range rows {
				if u := str(row[f.ca.User]); u != "" {
					users[u] = true
				}
			}
		}
	}
	if r.m != nil {
		for _, u := range r.m.users() {
			users[u] = true
		}
	}
	list := make([]string, 0, len(users))
	for u := range users {
		list = append(list, u)
	}
	sort.Strings(list)
	if len(list) > maxUsers {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("--per-user: read the first %d users only", maxUsers))
		list = list[:maxUsers]
	}
	if len(list) == 0 {
		r.res.Notes = append(r.res.Notes, "--per-user: no user has Conversational Analytics activity in System Activity "+agent.Window(r.opts))
		return true, nil
	}
	chosen, readable, err := r.listConversations(ctx, list)
	var refused errSudoRefused
	if errors.As(err, &refused) {
		r.res.Notes = append(r.res.Notes, refused.Error()+"; read System Activity only")
		return false, nil
	}
	if err != nil {
		return false, err
	}
	cap := r.opts.MaxConversations
	if cap > 0 && len(chosen) >= cap {
		r.capped = true
	}
	idx := enrichIndex{}
	if f.caOK {
		budget := 0 // no cap
		if cap > 0 {
			budget = max(cap-len(chosen), -1)
			if budget == 0 {
				budget = -1
			}
		}
		ch := newChooser(budget)
		held, dropped := 0, 0
		err := r.systemActivityTurns(ctx, f.ca, ch, func(c caRow) bool { return !readable[c.User] }, func(c caRow) {
			if held >= maxEnrich {
				dropped++
				return
			}
			held++
			idx.add(c)
		})
		if err != nil {
			return true, err
		}
		if dropped > 0 {
			r.res.Notes = append(r.res.Notes, fmt.Sprintf("--per-user: held System Activity facts of %d questions; %d older ones were not matched to messages", maxEnrich, dropped))
		}
		r.convs += len(ch.chosen)
	}
	return true, r.readConversations(ctx, chosen, idx)
}

// maxEnrich bounds the System Activity rows held to enrich --per-user turns.
const maxEnrich = 200000

func (r *run) finish(f found) {
	r.res.Conversations = r.convs
	r.res.Sources = append(r.res.Sources, bundle.Source{SourceKind: sourceKind, NativeID: "conversational-analytics",
		Account: r.c.base, Label: "Looker Conversational Analytics"})
	if r.capped {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("stopped at --max-conversations %s: the most recent conversations were read (narrow with --since)",
			agent.Thousands(int64(r.opts.MaxConversations))))
	}
	if f.caOK && f.ca.Conversation == "" {
		r.res.Notes = append(r.res.Notes, "the System Activity Conversational Analytics explore has no conversation id: each question counts as a conversation")
	}
	if r.m != nil {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("SQL attached from System Activity History by user and time (a Conversational Analytics query "+
			"logged within %s after a question, or its latency + %s up to %s, before the user's next question): %s of %s queries matched, %s matched no question",
			matchWindow, matchSlack, maxMatchWindow, agent.Thousands(int64(r.m.matched)), agent.Thousands(int64(r.m.total)),
			agent.Thousands(int64(r.m.unmatched()))))
	}
	if r.fetchFailed > 0 {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("could not read the SQL of %d matched queries (%s)", r.fetchFailed, r.fetchErr))
	}
	if r.logoutFailed > 0 {
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("--per-user: %d user sessions could not be logged out; they expire on their own within the hour", r.logoutFailed))
	}
	if !r.opts.IncludeResults {
		r.res.Redactions = append(r.res.Redactions, resultsRedaction)
	}
	if !r.opts.IncludeUsers {
		r.res.Redactions = append(r.res.Redactions, usersRedaction)
	}
}

// keepContext is the LookML project's files: LookML (manifest.lkml
// included), LookML dashboards and docs.
func keepContext(rel string) bool {
	l := strings.ToLower(rel)
	return strings.HasSuffix(l, ".lkml") || strings.HasSuffix(l, ".lookml") || strings.HasSuffix(l, ".md")
}

// checkContext confirms dir is a LookML project: it finds model or view
// files, and counts them.
func checkContext(dir string) (agent.CheckResult, error) {
	var res agent.CheckResult
	sum, err := files.Scan(dir, keepContext)
	if err != nil {
		return res, err
	}
	if err := sum.Limits(dir); err != nil {
		return res, err
	}
	var models, views, explores, dashboards int
	manifest := false
	for _, rel := range sum.Kept {
		b := strings.ToLower(path.Base(rel))
		switch {
		case strings.HasSuffix(b, ".model.lkml"):
			models++
		case strings.HasSuffix(b, ".view.lkml"):
			views++
		case strings.HasSuffix(b, ".explore.lkml"):
			explores++
		case strings.HasSuffix(b, ".dashboard.lookml"), strings.HasSuffix(b, ".dashboard.lkml"):
			dashboards++
		case b == "manifest.lkml":
			manifest = true
		}
	}
	if models == 0 && views == 0 {
		return res, fmt.Errorf("no LookML project in %s: expected *.model.lkml or *.view.lkml files", dir)
	}
	fact := fmt.Sprintf("LookML project: %d models, %d views", models, views)
	if explores > 0 {
		fact += fmt.Sprintf(", %d explore files", explores)
	}
	if dashboards > 0 {
		fact += fmt.Sprintf(", %d LookML dashboards", dashboards)
	}
	if manifest {
		fact += ", manifest.lkml"
	}
	res.Facts = append(res.Facts, fact, fmt.Sprintf("%d files (%s) will be included", len(sum.Kept), agent.Bytes(sum.KeptBytes)))
	if models == 0 {
		res.Warnings = append(res.Warnings, "no *.model.lkml file: the Explores' joins and access rules will be missing")
	}
	return res, nil
}
