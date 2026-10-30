package looker

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// System Activity is the LookML model Looker ships for instance telemetry.
// Its explore names and the History fields used below are documented
// (docs.cloud.google.com/looker/docs/usage-reports-with-system-activity-explores);
// the Conversational Analytics explore is not, so it and its fields are
// discovered from the model's metadata at run time.
const (
	saModel        = "system__activity"
	historyExplore = "history"
	// caClientNames filters History to queries Conversational Analytics
	// issued. "Conversational Analytics" is the documented value of API
	// Client Name; "%Gemini%" (a Looker contains-match) catches a Gemini
	// client name a later release may use. A comma is OR in Looker filters.
	caClientNames = "Conversational Analytics,%Gemini%"
)

// Paths of the metadata requests.
const (
	modelPath   = "/lookml_models/" + saModel + "?fields=explores"
	explorePath = "/lookml_models/" + saModel + "/explores/%s?fields=fields"
	runPath     = "/queries/run/json?apply_formatting=false&apply_vis=false"
)

type field struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Label   string `json:"label"`
	Measure bool   `json:"measure"`
}

// explores lists the System Activity explores.
func (c *client) explores(ctx context.Context) ([]string, error) {
	var m struct {
		Explores []struct {
			Name string `json:"name"`
		} `json:"explores"`
	}
	if err := c.getJSON(ctx, "", modelPath, &m); err != nil {
		return nil, err
	}
	var out []string
	for _, e := range m.Explores {
		out = append(out, e.Name)
	}
	return out, nil
}

var exploreName = regexp.MustCompile(`^[a-z0-9_]{1,128}$`)

// exploreFields lists an explore's dimensions and measures.
func (c *client) exploreFields(ctx context.Context, explore string) ([]field, error) {
	if !exploreName.MatchString(explore) {
		return nil, fmt.Errorf("looker explore name %q is not a name", clip(explore, 80))
	}
	var e struct {
		Fields struct {
			Dimensions []field `json:"dimensions"`
			Measures   []field `json:"measures"`
		} `json:"fields"`
	}
	if err := c.getJSON(ctx, "", fmt.Sprintf(explorePath, explore), &e); err != nil {
		return nil, err
	}
	out := e.Fields.Dimensions
	for _, m := range e.Fields.Measures {
		m.Measure = true
		out = append(out, m)
	}
	return out, nil
}

// caExplorePattern ranks System Activity explore names that may hold
// Conversational Analytics telemetry: the best match first.
var caExplorePattern = []*regexp.Regexp{
	regexp.MustCompile(`conversational_analytics`),
	regexp.MustCompile(`conversation`),
	regexp.MustCompile(`gemini`),
	regexp.MustCompile(`(^|_)ca(_|$)`),
	regexp.MustCompile(`assistant|agent`),
}

// caCandidates returns the explores that may be the Conversational
// Analytics one, best first.
func caCandidates(names []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, re := range caExplorePattern {
		var tier []string
		for _, n := range names {
			if !seen[n] && re.MatchString(strings.ToLower(n)) {
				tier = append(tier, n)
				seen[n] = true
			}
		}
		sort.Strings(tier)
		out = append(out, tier...)
	}
	return out
}

// caFields are the fields chosen in the Conversational Analytics explore.
// Only Question and Time are required; each other is used when found.
type caFields struct {
	Explore                           string
	Question, Time                    string
	User, Conversation, Message       string
	Rating, Comment, Success, Latency string
	Agent, Model, ExploreF, Surface   string
	UserName, UserEmail               string
	Count                             string
}

// Field-name patterns, tried in order on the part after the view ("view.field").
var (
	pQuestion     = []string{`^question$`, `^(user_)?(question|prompt)(_text)?$`, `^(user_)?query_text$`, `question|prompt`}
	pTime         = []string{`^(created|asked|submitted|question)_time$`, `^timestamp_time$`, `^[a-z_]*created[a-z_]*_time$`, `_time$`}
	pConversation = []string{`^conversation_id$`, `conversation[a-z_]*_id$`}
	pMessage      = []string{`^(user_)?message_id$`, `^turn_id$`, `^query_id$`}
	pRating       = []string{`^rating$`, `^user_rating$`, `rating`}
	pComment      = []string{`^(feedback_)?comment$`, `comment|feedback_text`}
	pSuccess      = []string{`^answer_success$`, `success`, `^(answer_)?(status|result|outcome)$`}
	pLatency      = []string{`^latency(_seconds|_s)?$`, `latency`, `duration`}
	pAgent        = []string{`^agent_id$`}
	pModel        = []string{`^(lookml_)?model(_name)?$`}
	pExplore      = []string{`^explore(_name)?$`}
	pSurface      = []string{`surface`}
)

// pickCA chooses the fields of explore by name; ok is false when it has no
// question text or no time to window by.
func pickCA(explore string, fs []field) (caFields, bool) {
	f := caFields{Explore: explore}
	dims := only(fs, false)
	f.Question = pick(dims, pQuestion, notCountOrID)
	f.Time = pick(dims, pTime, notTimeOfDay)
	if f.Question == "" || f.Time == "" {
		return f, false
	}
	f.User = exact(dims, "user.id")
	if f.User == "" {
		f.User = pick(dims, []string{`^user_id$`}, nil)
	}
	f.Conversation = pick(dims, pConversation, nil)
	if f.Conversation == "" {
		f.Conversation = exact(dims, "conversation.id")
	}
	f.Message = pick(dims, pMessage, nil)
	f.Rating = pick(dims, pRating, notCountOrID)
	f.Comment = pick(dims, pComment, notCountOrID)
	f.Success = pick(dims, pSuccess, notCountOrID)
	f.Latency = pick(dims, pLatency, notCountOrID)
	f.Agent = pick(dims, pAgent, nil)
	f.Model = pick(dims, pModel, nil)
	f.ExploreF = pick(dims, pExplore, nil)
	f.Surface = pick(dims, pSurface, nil)
	f.UserName = exact(dims, "user.name")
	f.UserEmail = exact(dims, "user.email")
	view := viewOf(f.Question)
	for _, m := range only(fs, true) {
		if m.Name == view+".count" {
			f.Count = m.Name
		}
	}
	return f, true
}

// fields lists the fields a turns query selects, in a fixed order.
func (f caFields) fields(includeUsers bool) []string {
	var out []string
	for _, n := range []string{f.Time, f.Question, f.User, f.Conversation, f.Message, f.Rating, f.Comment,
		f.Success, f.Latency, f.Agent, f.Model, f.ExploreF, f.Surface} {
		if n != "" {
			out = append(out, n)
		}
	}
	if includeUsers {
		for _, n := range []string{f.UserName, f.UserEmail} {
			if n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

// histFields are the History explore fields used. APIClient, QueryID and
// Time are required.
type histFields struct {
	QueryID, User, Time, APIClient, Source, Status, Model, View string
}

// pickHistory checks History has the documented fields. query.id and
// user.id are Looker's standard id dimensions of the joined Query and User
// views; they are looked up rather than assumed.
func pickHistory(fs []field) (histFields, bool) {
	dims := only(fs, false)
	h := histFields{
		QueryID:   exact(dims, "query.id"),
		User:      exact(dims, "user.id"),
		Time:      exact(dims, "history.created_time"),
		APIClient: exact(dims, "query_api_client_context.name"),
		Source:    exact(dims, "history.source"),
		Status:    exact(dims, "history.status"),
		Model:     exact(dims, "query.model"),
		View:      exact(dims, "query.view"),
	}
	return h, h.QueryID != "" && h.Time != "" && h.APIClient != ""
}

func (h histFields) fields() []string {
	var out []string
	for _, n := range []string{h.Time, h.QueryID, h.User, h.APIClient, h.Source, h.Status, h.Model, h.View} {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

func only(fs []field, measure bool) []field {
	var out []field
	for _, f := range fs {
		if f.Measure == measure {
			out = append(out, f)
		}
	}
	return out
}

func viewOf(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return ""
}

func short(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}

func exact(fs []field, name string) string {
	for _, f := range fs {
		if f.Name == name {
			return f.Name
		}
	}
	return ""
}

func notCountOrID(n string) bool {
	s := short(n)
	return !strings.Contains(s, "count") && !strings.HasSuffix(s, "_id") && s != "id"
}

func notTimeOfDay(n string) bool { return !strings.HasSuffix(n, "_time_of_day") }

// pick returns the first field whose short name matches the earliest
// pattern, among those ok accepts.
func pick(fs []field, patterns []string, ok func(string) bool) string {
	for _, p := range patterns {
		re := regexp.MustCompile(p)
		for _, f := range fs {
			if re.MatchString(strings.ToLower(short(f.Name))) && (ok == nil || ok(f.Name)) {
				return f.Name
			}
		}
	}
	return ""
}

// inlineQuery is the run_inline_query body (the Query schema's writable
// fields used here). limit is a string in the schema.
type inlineQuery struct {
	Model         string            `json:"model"`
	View          string            `json:"view"`
	Fields        []string          `json:"fields"`
	Filters       map[string]string `json:"filters,omitempty"`
	Sorts         []string          `json:"sorts,omitempty"`
	Limit         string            `json:"limit"`
	QueryTimezone string            `json:"query_timezone"`
}

func (q inlineQuery) statement() string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(q)
	return "POST " + apiPrefix + runPath + " " + strings.TrimSpace(b.String())
}

// run posts q to run_inline_query (result_format json): one object per
// row, keyed by field name. It runs a System Activity query, never one of
// the customer's.
func (c *client) run(ctx context.Context, token string, q inlineQuery) ([]map[string]any, error) {
	var rows []map[string]any
	if err := c.postJSON(ctx, token, runPath, q, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// lookerTime renders t for a Looker date filter.
func lookerTime(t time.Time) string { return t.UTC().Format("2006/01/02 15:04:05") }

// window is the Looker filter expression for [since, before): "after X",
// "X to Y" or "before Y"; "" for no bound.
func window(since, before time.Time) string {
	switch {
	case since.IsZero() && before.IsZero():
		return ""
	case before.IsZero():
		return "after " + lookerTime(since)
	case since.IsZero():
		return "before " + lookerTime(before)
	}
	return lookerTime(since) + " to " + lookerTime(before)
}

var timeLayouts = []string{"2006-01-02 15:04:05", time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05.999999", "2006-01-02"}

// parseTime reads a Looker time value. System Activity answers with
// "2006-01-02 15:04:05" in the query time zone, which every query here sets
// to UTC.
func parseTime(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return time.Time{}, false
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, strings.TrimSpace(s)); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// str renders a row value as text.
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return x.String()
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}
