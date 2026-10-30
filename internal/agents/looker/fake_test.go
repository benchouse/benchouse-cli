package looker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() {
	sleep = func(context.Context, time.Duration) error { return nil }
}

const (
	caView   = "conversational_analytics_query"
	tsLayout = "2006-01-02 15:04:05"
)

// base is the fixtures' clock.
var base = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) string { return base.Add(d).Format(tsLayout) }

// fakeLooker is an httptest Looker: login, sudo, logout, System Activity
// metadata and run_inline_query over in-memory rows, query SQL and
// definitions, and per-user conversations.
type fakeLooker struct {
	t *testing.T

	clientSecret string // what login accepts as client_secret

	saStatus   int // non-zero: the System Activity model answers with it
	sudoStatus int // non-zero: login_user answers with it
	noCA       bool

	caRows   []map[string]any
	histRows []map[string]any
	sql      map[string]string
	convs    map[string][]map[string]any // user id → conversations
	msgs     map[string][]map[string]any // conversation id → messages

	mu        sync.Mutex
	tokens    map[string]string // token → user ("" = the API client)
	loggedOut map[string]bool
	requests  []string // "METHOD /path" without the query
	bodies    []inlineQuery
	sudos     int
	logins    int
}

func newFake(t *testing.T) *fakeLooker {
	return &fakeLooker{t: t, clientSecret: "csecret", sql: map[string]string{}, convs: map[string][]map[string]any{},
		msgs: map[string][]map[string]any{}, tokens: map[string]string{}, loggedOut: map[string]bool{}}
}

func caFieldList() []map[string]any {
	d := func(n, typ string) map[string]any { return map[string]any{"name": n, "type": typ} }
	return []map[string]any{
		d(caView+".created_date", "date_date"), d(caView+".created_time", "date_time"),
		d(caView+".created_time_of_day", "date_time_of_day"), d(caView+".question", "string"),
		d(caView+".conversation_id", "string"), d(caView+".rating", "string"), d(caView+".answer_success", "string"),
		d(caView+".latency", "number"), d(caView+".agent_id", "string"), d(caView+".model", "string"),
		d(caView+".explore", "string"), d("user.id", "number"), d("user.name", "string"), d("user.email", "string"),
	}
}

func histFieldList() []map[string]any {
	var out []map[string]any
	for _, n := range []string{"history.created_time", "history.source", "history.status", "query.id", "query.model",
		"query.view", "user.id", "query_api_client_context.name"} {
		out = append(out, map[string]any{"name": n, "type": "string"})
	}
	return out
}

func (f *fakeLooker) server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	return srv
}

func (f *fakeLooker) inputs(srv *httptest.Server, perUser string) map[string]string {
	return map[string]string{"host": srv.URL, "client-id": "cid", "client-secret": "csecret", "per-user": perUser}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	writeJSON(w, map[string]any{"message": msg})
}

var (
	reSudo     = regexp.MustCompile(`^/api/4.0/login/([^/]+)$`)
	reExplore  = regexp.MustCompile(`^/api/4.0/lookml_models/system__activity/explores/([^/]+)$`)
	reSQL      = regexp.MustCompile(`^/api/4.0/queries/([^/]+)/run/sql$`)
	reQuery    = regexp.MustCompile(`^/api/4.0/queries/([0-9]+)$`)
	reMessages = regexp.MustCompile(`^/api/4.0/conversations/([^/]+)/messages$`)
)

func (f *fakeLooker) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	f.requests = append(f.requests, r.Method+" "+p)
	if r.Method == http.MethodPost && p == "/api/4.0/login" {
		if r.URL.RawQuery != "" {
			f.t.Errorf("login put credentials in the URL: %s", r.URL.RawQuery)
		}
		r.ParseForm()
		if r.PostForm.Get("client_id") != "cid" || r.PostForm.Get("client_secret") != f.clientSecret {
			fail(w, http.StatusNotFound, "Not found")
			return
		}
		f.logins++
		t := fmt.Sprintf("admin-token-%d", f.logins)
		f.tokens[t] = ""
		writeJSON(w, map[string]any{"access_token": t, "token_type": "Bearer", "expires_in": 3600})
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
	user, ok := f.tokens[tok]
	if !ok || f.loggedOut[tok] {
		fail(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	switch m := r.Method; {
	case m == http.MethodDelete && p == "/api/4.0/logout":
		f.loggedOut[tok] = true
		w.WriteHeader(http.StatusNoContent)
	case m == http.MethodPost && reSudo.MatchString(p):
		if user != "" {
			fail(w, http.StatusForbidden, "only admins")
			return
		}
		if f.sudoStatus != 0 {
			fail(w, f.sudoStatus, "Not allowed")
			return
		}
		f.sudos++
		uid := reSudo.FindStringSubmatch(p)[1]
		t := fmt.Sprintf("sudo-%s-%d", uid, f.sudos)
		f.tokens[t] = uid
		writeJSON(w, map[string]any{"access_token": t})
	case m == http.MethodGet && p == "/api/4.0/lookml_models/system__activity":
		if f.saStatus != 0 {
			fail(w, f.saStatus, "You are not allowed")
			return
		}
		ex := []map[string]any{{"name": "history"}, {"name": "user"}, {"name": "event"}}
		if !f.noCA {
			ex = append(ex, map[string]any{"name": caView})
		}
		writeJSON(w, map[string]any{"explores": ex})
	case m == http.MethodGet && reExplore.MatchString(p):
		switch reExplore.FindStringSubmatch(p)[1] {
		case "history":
			writeJSON(w, map[string]any{"fields": map[string]any{"dimensions": histFieldList(),
				"measures": []map[string]any{{"name": "history.query_run_count"}}}})
		case caView:
			writeJSON(w, map[string]any{"fields": map[string]any{"dimensions": caFieldList(),
				"measures": []map[string]any{{"name": caView + ".count"}}}})
		default:
			fail(w, http.StatusNotFound, "no explore")
		}
	case m == http.MethodPost && p == "/api/4.0/queries/run/json":
		if user != "" {
			fail(w, http.StatusForbidden, "System Activity is admin only in this fake")
			return
		}
		var q inlineQuery
		json.NewDecoder(r.Body).Decode(&q)
		f.bodies = append(f.bodies, q)
		rows := f.histRows
		if q.View == caView {
			rows = f.caRows
		}
		writeJSON(w, runFake(f.t, q, rows))
	case m == http.MethodGet && reSQL.MatchString(p):
		sql, ok := f.sql[reSQL.FindStringSubmatch(p)[1]]
		if !ok {
			fail(w, http.StatusNotFound, "no query")
			return
		}
		w.Header().Set("Content-Type", "text")
		w.Write([]byte(sql))
	case m == http.MethodGet && reQuery.MatchString(p):
		id := reQuery.FindStringSubmatch(p)[1]
		if _, ok := f.sql[id]; !ok {
			fail(w, http.StatusNotFound, "no query")
			return
		}
		writeJSON(w, map[string]any{"id": id, "model": "ecomm", "view": "orders", "fields": []string{"orders.count"}, "can": map[string]any{"run": true}})
	case m == http.MethodGet && p == "/api/4.0/conversations/search":
		cs := append([]map[string]any(nil), f.convs[user]...)
		sort.Slice(cs, func(i, j int) bool { return str(cs[i]["updated_at"]) > str(cs[j]["updated_at"]) })
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		lim, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		end := min(off+lim, len(cs))
		if off > len(cs) {
			off = len(cs)
		}
		writeJSON(w, cs[off:end])
	case m == http.MethodGet && reMessages.MatchString(p):
		id := reMessages.FindStringSubmatch(p)[1]
		owned := false
		for _, c := range f.convs[user] {
			owned = owned || str(c["id"]) == id
		}
		if !owned {
			fail(w, http.StatusNotFound, "not your conversation")
			return
		}
		writeJSON(w, f.msgs[id])
	default:
		fail(w, http.StatusNotFound, "unknown "+m+" "+p)
	}
}

// runFake evaluates an inline query the way Looker would for these
// fixtures: time and equality filters, grouping by the dimensions selected
// (a measure counts), sort and limit.
func runFake(t *testing.T, q inlineQuery, rows []map[string]any) []map[string]any {
	var kept []map[string]any
	for _, r := range rows {
		ok := true
		for k, v := range q.Filters {
			if !filterOK(t, v, str(r[k])) {
				ok = false
			}
		}
		if ok {
			kept = append(kept, r)
		}
	}
	var measure string
	var dims []string
	for _, f := range q.Fields {
		if strings.HasSuffix(f, ".count") || strings.HasSuffix(f, "_count") {
			measure = f
		} else {
			dims = append(dims, f)
		}
	}
	groups := map[string]map[string]any{}
	var order []string
	for _, r := range kept {
		var key []string
		out := map[string]any{}
		for _, d := range dims {
			key = append(key, str(r[d]))
			out[d] = r[d]
		}
		k := strings.Join(key, "\x00")
		if g, ok := groups[k]; ok {
			if measure != "" {
				g[measure] = g[measure].(float64) + 1
			}
			continue
		}
		if measure != "" {
			out[measure] = float64(1)
		}
		groups[k] = out
		order = append(order, k)
	}
	var out []map[string]any
	for _, k := range order {
		out = append(out, groups[k])
	}
	if len(q.Sorts) > 0 {
		f := strings.Fields(q.Sorts[0])
		desc := len(f) > 1 && f[1] == "desc"
		sort.SliceStable(out, func(i, j int) bool {
			a, b := str(out[i][f[0]]), str(out[j][f[0]])
			if desc {
				return a > b
			}
			return a < b
		})
	}
	if n, _ := strconv.Atoi(q.Limit); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

func filterOK(t *testing.T, expr, v string) bool {
	parse := func(s string) time.Time {
		tm, err := time.Parse("2006/01/02 15:04:05", s)
		if err != nil {
			t.Fatalf("bad filter time %q", s)
		}
		return tm
	}
	val, err := time.Parse(tsLayout, v)
	switch {
	case strings.HasPrefix(expr, "after "):
		return err == nil && !val.Before(parse(strings.TrimPrefix(expr, "after ")))
	case strings.HasPrefix(expr, "before "):
		return err == nil && val.Before(parse(strings.TrimPrefix(expr, "before ")))
	case strings.Contains(expr, " to "):
		p := strings.SplitN(expr, " to ", 2)
		return err == nil && !val.Before(parse(p[0])) && val.Before(parse(p[1]))
	}
	for _, alt := range strings.Split(expr, ",") {
		if strings.HasPrefix(alt, "%") && strings.HasSuffix(alt, "%") && strings.Contains(v, strings.Trim(alt, "%")) || alt == v {
			return true
		}
	}
	return false
}

func caRow_(user, conv, q string, d time.Duration, rating, success string, latency float64) map[string]any {
	return map[string]any{caView + ".created_time": at(d), caView + ".question": q, "user.id": user,
		caView + ".conversation_id": conv, caView + ".rating": rating, caView + ".answer_success": success,
		caView + ".latency": latency, caView + ".model": "ecomm", caView + ".explore": "orders",
		"user.name": "Name " + user, "user.email": user + "@acme.test"}
}

func histRow(id, user string, d time.Duration) map[string]any {
	return map[string]any{"history.created_time": at(d), "query.id": id, "user.id": user,
		"query_api_client_context.name": "Conversational Analytics", "history.source": "api4", "history.status": "complete",
		"query.model": "ecomm", "query.view": "orders"}
}

// tokensOut reports whether every token handed out was logged out.
func (f *fakeLooker) tokensOut() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var open []string
	for t := range f.tokens {
		if !f.loggedOut[t] {
			open = append(open, t)
		}
	}
	sort.Strings(open)
	return open
}

func (f *fakeLooker) hit(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}
