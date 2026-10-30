package thoughtspot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

// The primary route: the Spotter analytics system model ("TS: Spotter
// Analytics Model"), read with searchdata. ThoughtSpot documents the model
// but not its full column list, so the columns are discovered from the
// model's metadata by name (roles below) and the query is built from what
// is there.

const (
	defaultModel = "TS: Spotter Analytics Model"
	metadataPath = "/metadata/search"
	searchPath   = "/searchdata"
	pageRows     = 1000
	// maxRows bounds the walk whatever the caps: 200 pages.
	maxRows = 200 * pageRows
	// drainPages bounds the pages read after --max-conversations turned a
	// conversation away, to complete the chosen ones' older turns.
	drainPages = 3
)

// role is what a column of the model holds.
type role string

const (
	roleQuestion     role = "question"
	roleAnswer       role = "answer"
	roleRating       role = "rating"
	roleTime         role = "time"
	roleUserID       role = "user_id"
	roleUserName     role = "user_name"
	roleConversation role = "conversation"
	roleMessage      role = "message"
	roleSource       role = "source"
	roleDataModel    role = "data_model"
	roleComment      role = "comment"
)

// roleOrder is the order roles are queried in and the order of preference
// when two columns could hold one role.
var roleOrder = []role{roleQuestion, roleAnswer, roleRating, roleComment, roleTime, roleConversation, roleMessage,
	roleUserID, roleUserName, roleSource, roleDataModel}

// roleOf classifies a column name. The documented columns are "User Query",
// "Response" and "User Feedback Rating"; the rest are matched by pattern.
func roleOf(name string) role {
	n := strings.ToLower(strings.Join(strings.Fields(name), " "))
	has := func(s ...string) bool {
		for _, x := range s {
			if strings.Contains(n, x) {
				return true
			}
		}
		return false
	}
	switch {
	case n == "user query" || n == "query text" || n == "question" || n == "user question" || n == "prompt":
		return roleQuestion
	case n == "response" || n == "spotter response" || n == "response text" || n == "answer":
		return roleAnswer
	case has("feedback rating") || n == "rating" || n == "feedback":
		return roleRating
	case has("feedback comment", "feedback text"):
		return roleComment
	case has("timestamp") || n == "time" || n == "date" || n == "event time" || n == "created at" || n == "query time":
		return roleTime
	case has("conversation id", "conversation guid", "session id", "chat id"):
		return roleConversation
	case has("message id", "response id", "query id", "event id"):
		return roleMessage
	case has("user id", "user guid"):
		return roleUserID
	case n == "user" || n == "user name" || n == "username" || n == "user display name" || n == "author":
		return roleUserName
	case has("origin", "client", "entry point") || n == "source":
		return roleSource
	case has("data source", "model name", "worksheet", "data model") || n == "model":
		return roleDataModel
	}
	return ""
}

// model is the analytics model found, with the column chosen for each role.
type model struct {
	ID, Name string
	Cols     map[role]string
	// Guessed is set when the model's columns could not be listed and the
	// documented names are assumed.
	Guessed bool
}

func (m model) has(r role) bool { return m.Cols[r] != "" }

// queryString is the searchdata query: every chosen column, the --since
// filter, newest first.
func (m model) queryString(opts agent.Options) string {
	var parts []string
	for _, r := range roleOrder {
		if c := m.Cols[r]; c != "" {
			parts = append(parts, "["+c+"]")
		}
	}
	return strings.Join(parts, " ") + m.tail(opts)
}

func (m model) tail(opts agent.Options) string {
	t := m.Cols[roleTime]
	if t == "" {
		return ""
	}
	s := ""
	if !opts.Since.IsZero() {
		s += fmt.Sprintf(" [%s] >= '%s'", t, opts.Since.UTC().Format("01/02/2006"))
	}
	return s + fmt.Sprintf(" sort by [%s] descending", t)
}

func metadataBody(name string) map[string]any {
	return map[string]any{"metadata": []any{map[string]any{"type": "LOGICAL_TABLE", "identifier": name}},
		"include_details": true, "record_size": 10}
}

func metadataStatement(name string) string {
	b, _ := json.Marshal(metadataBody(name))
	return "POST " + apiPrefix + metadataPath + " " + string(b)
}

func searchBody(guid, query string, offset, size int) map[string]any {
	return map[string]any{"logical_table_identifier": guid, "query_string": query, "data_format": "COMPACT",
		"record_offset": offset, "record_size": size}
}

func searchStatement(guid, query string, offset string) string {
	return fmt.Sprintf(`POST %s%s {"logical_table_identifier":%q,"query_string":%q,"data_format":"COMPACT","record_offset":%s,"record_size":%d}`,
		apiPrefix, searchPath, guid, query, offset, pageRows)
}

// errNoModel says the analytics model is not there for this user.
var errNoModel = errors.New("no analytics model")

// findModel looks the model up by name and lists its columns.
func findModel(ctx context.Context, cl *client, name string) (model, error) {
	var found []map[string]any
	if err := cl.post(ctx, metadataPath, metadataBody(name), &found); err != nil {
		return model{}, err
	}
	var pick map[string]any
	for _, f := range found {
		if strings.EqualFold(str(f["metadata_name"]), name) {
			pick = f
			break
		}
	}
	if pick == nil && len(found) == 1 {
		pick = found[0]
	}
	if pick == nil || str(pick["metadata_id"]) == "" {
		return model{}, fmt.Errorf("%w: ThoughtSpot has no model named %q visible to this user", errNoModel, name)
	}
	m := model{ID: str(pick["metadata_id"]), Name: str(pick["metadata_name"]), Cols: map[role]string{}}
	names := columnNames(pick)
	if len(names) == 0 {
		// The details did not list columns: assume the documented ones.
		m.Guessed = true
		names = []string{"User Query", "Response", "User Feedback Rating", "Timestamp"}
	}
	for _, n := range names {
		if r := roleOf(n); r != "" && m.Cols[r] == "" {
			m.Cols[r] = n
		}
	}
	if !m.has(roleQuestion) {
		return m, fmt.Errorf("%w: %q has no User Query column (columns: %s)", errNoModel, name, clip(strings.Join(names, ", "), 300))
	}
	if !m.has(roleTime) {
		return m, fmt.Errorf("%w: %q has no timestamp column (columns: %s)", errNoModel, name, clip(strings.Join(names, ", "), 300))
	}
	return m, nil
}

// columnNames finds the model's column names in a metadata/search result:
// a "columns" array under metadata_detail (or anywhere below it) whose
// elements carry header.name or name.
func columnNames(v any) []string {
	var out []string
	var walk func(any, int)
	walk = func(v any, depth int) {
		if depth > 6 || len(out) > 0 {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			if cols, ok := x["columns"].([]any); ok {
				for _, c := range cols {
					cm, _ := c.(map[string]any)
					if cm == nil {
						continue
					}
					n := str(cm["name"])
					if h, ok := cm["header"].(map[string]any); ok && str(h["name"]) != "" {
						n = str(h["name"])
					}
					if n != "" {
						out = append(out, n)
					}
				}
				if len(out) > 0 {
					return
				}
			}
			for k, c := range x {
				if k == "metadata_detail" || depth > 0 {
					walk(c, depth+1)
				}
			}
		case []any:
			for _, c := range x {
				walk(c, depth+1)
			}
		}
	}
	walk(v, 0)
	return out
}

// searchResult is a searchdata answer: contents[0], read loosely.
type searchResult struct {
	Contents []struct {
		ColumnNames []string          `json:"column_names"`
		DataRows    []json.RawMessage `json:"data_rows"`
		Available   *int64            `json:"available_data_row_count"`
		Returned    *int64            `json:"returned_data_row_count"`
	} `json:"contents"`
}

// rows decodes the rows as maps by column name: COMPACT gives arrays, FULL
// gives objects.
func (s searchResult) rows() (cols []string, out []map[string]any, err error) {
	if len(s.Contents) == 0 {
		return nil, nil, nil
	}
	c := s.Contents[0]
	for _, raw := range c.DataRows {
		var arr []any
		if json.Unmarshal(raw, &arr) == nil {
			m := map[string]any{}
			for i, v := range arr {
				if i < len(c.ColumnNames) {
					m[c.ColumnNames[i]] = v
				}
			}
			out = append(out, m)
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, nil, fmt.Errorf("thoughtspot searchdata: a row is neither an array nor an object")
		}
		out = append(out, obj)
	}
	return c.ColumnNames, out, nil
}

// analyticsColumns are what the model route records.
func analyticsColumns(m model, opts agent.Options) turns.Columns {
	cols := []string{turns.ColExtra}
	if m.has(roleConversation) {
		cols = append(cols, turns.ColConversationID)
	}
	if m.has(roleUserID) {
		cols = append(cols, turns.ColUserID)
	}
	if m.has(roleUserName) && opts.IncludeUsers {
		cols = append(cols, turns.ColUserName)
	}
	if m.has(roleSource) {
		cols = append(cols, turns.ColClientInterface)
	}
	if m.has(roleDataModel) {
		cols = append(cols, turns.ColSemanticModelRef)
	}
	return turns.NewColumns(cols...)
}

// readAnalytics pages through the model newest first, emitting a turn per
// row, until --max-conversations or the window ends.
func readAnalytics(ctx context.Context, cl *client, m model, opts agent.Options, res *agent.Result, emit agent.Emit) error {
	q := m.queryString(opts)
	cols := analyticsColumns(m, opts)
	nextStmt := searchStatement(m.ID, q, "<1000, 2000, ...>")
	chosen := map[string]bool{}
	capped, drained := false, 0
	old, skipped, midnight, timed := 0, 0, 0, 0
	for offset := 0; offset < maxRows; offset += pageRows {
		var sr searchResult
		if err := cl.post(ctx, searchPath, searchBody(m.ID, q, offset, pageRows), &sr); err != nil {
			if offset == 0 {
				return err
			}
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after %s rows of the analytics model: %s", agent.Thousands(int64(offset)), agent.Clip(err)))
			break
		}
		_, rows, err := sr.rows()
		if err != nil {
			return err
		}
		if offset == 0 {
			res.Queries = append(res.Queries, bundle.Query{Purpose: purposeSearch, Statement: searchStatement(m.ID, q, "0"), Rows: len(rows)})
		} else {
			res.Tally(purposeSearchNext, nextStmt, len(rows))
		}
		turnedAway := false
		for _, r := range rows {
			t, ok := analyticsTurn(m, r, opts)
			if !ok {
				skipped++
				continue
			}
			timed++
			if t.AskedAt.Equal(t.AskedAt.Truncate(24 * time.Hour)) {
				midnight++
			}
			if !opts.Since.IsZero() && t.AskedAt.Before(opts.Since) {
				old++
				continue
			}
			key := t.ConversationID
			if key == "" {
				key = "turn:" + t.TurnID
			}
			if !chosen[key] {
				if opts.MaxConversations > 0 && len(chosen) >= opts.MaxConversations {
					capped, turnedAway = true, true
					continue
				}
				chosen[key] = true
			}
			rec, err := turns.Record(t, cols, fetchAnalytics)
			if err != nil {
				skipped++
				continue
			}
			if err := emit(rec); err != nil {
				return err
			}
		}
		if len(rows) < pageRows {
			break
		}
		if capped {
			// Without a conversation column every row is its own
			// conversation: nothing older belongs to a chosen one.
			if !m.has(roleConversation) {
				break
			}
			if turnedAway {
				if drained++; drained >= drainPages {
					break
				}
			}
		}
		if offset+pageRows >= maxRows {
			res.Notes = append(res.Notes, fmt.Sprintf("stopped after %s rows of the analytics model (narrow with --since)", agent.Thousands(maxRows)))
		}
	}
	res.Conversations += len(chosen)
	if capped {
		res.Notes = append(res.Notes, fmt.Sprintf("stopped at --max-conversations %s: the most recent Spotter conversations were read, older ones were not (narrow with --since)",
			agent.Thousands(int64(opts.MaxConversations))))
	}
	if !m.has(roleConversation) {
		res.Notes = append(res.Notes, "the analytics model has no conversation id column: each question counts as its own conversation")
	}
	if old > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("left out %d rows before --since", old))
	}
	if skipped > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("skipped %d rows of the analytics model without a question or a readable time", skipped))
	}
	if timed > 0 && midnight == timed {
		res.Notes = append(res.Notes, "every analytics model time is at midnight: ThoughtSpot returned the timestamp bucketed by day, so asked_at is the day only")
	}
	if m.Guessed {
		res.Notes = append(res.Notes, "the analytics model's columns could not be listed; the documented names (User Query, Response, User Feedback Rating) and Timestamp were assumed")
	}
	res.Notes = append(res.Notes, "the Spotter analytics model keeps 180 days of history")
	if m.has(roleUserName) && !opts.IncludeUsers {
		res.Redactions = append(res.Redactions, "ThoughtSpot user names from the analytics model were dropped (--include-users keeps them)")
	}
	return nil
}

// fetchAnalytics and fetchSaved are the records' fetch methods.
const (
	fetchAnalytics = "api:thoughtspot/searchdata"
	fetchSaved     = "api:thoughtspot/ai/agent/conversations"
)

func analyticsTurn(m model, r map[string]any, opts agent.Options) (turns.Turn, bool) {
	get := func(x role) any {
		if c := m.Cols[x]; c != "" {
			return r[c]
		}
		return nil
	}
	question := strings.TrimSpace(str(get(roleQuestion)))
	at, ok := parseTime(get(roleTime))
	if question == "" || !ok {
		return turns.Turn{}, false
	}
	t := turns.Turn{AskedAt: at, Question: question, Extra: map[string]any{}}
	if a := str(get(roleAnswer)); m.has(roleAnswer) && a != "" {
		t.Answer = &a
	}
	t.ConversationID = str(get(roleConversation))
	t.UserID = str(get(roleUserID))
	userName := str(get(roleUserName))
	if opts.IncludeUsers {
		t.UserName = userName
	}
	t.ClientInterface = str(get(roleSource))
	t.SemanticModelRef = str(get(roleDataModel))
	if fb := feedback(get(roleRating), str(get(roleComment))); fb != nil {
		t.Extra["feedback"] = fb
	}
	t.Extra["source"] = "spotter_analytics_model"
	if id := str(get(roleMessage)); id != "" {
		t.TurnID = id
	} else {
		// No message id: a stable hash of what identifies the row. The
		// user name feeds the hash only, never the record.
		h := sha256.Sum256([]byte(strings.Join([]string{t.ConversationID, at.Format(time.RFC3339Nano), t.UserID, userName, question}, "\x1f")))
		t.TurnID = "ts-" + hex.EncodeToString(h[:8])
	}
	return t, true
}

// feedback reads the rating ("Upvote"/"Downvote", a number, thumbs) into
// extra.feedback: the raw rating, and score +1/-1 when it is clear.
func feedback(v any, comment string) map[string]any {
	raw := strings.TrimSpace(str(v))
	if raw == "" && comment == "" {
		return nil
	}
	fb := map[string]any{}
	if raw != "" {
		fb["rating"] = raw
		l := strings.ToLower(raw)
		switch {
		case strings.Contains(l, "down") || strings.Contains(l, "negative") || strings.Contains(l, "dislike") || l == "-1" || l == "thumbs down":
			fb["score"] = -1
		case strings.Contains(l, "up") || strings.Contains(l, "positive") || strings.Contains(l, "like") || l == "1":
			fb["score"] = 1
		case l == "0" || l == "none" || l == "{null}" || l == "no feedback":
			return nilIfEmpty(comment)
		}
	}
	if comment != "" {
		fb["comment"] = comment
	}
	return fb
}

func nilIfEmpty(comment string) map[string]any {
	if comment == "" {
		return nil
	}
	return map[string]any{"comment": comment}
}

// str renders a JSON scalar as text.
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		if x == "{Null}" || x == "{null}" {
			return ""
		}
		return x
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// parseTime reads a time the way ThoughtSpot may return it: epoch seconds
// or milliseconds (a number or numeric string), ISO 8601, or a formatted
// date and time.
func parseTime(v any) (time.Time, bool) {
	switch x := v.(type) {
	case float64:
		return epoch(x)
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return time.Time{}, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return epoch(f)
		}
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999",
			"2006-01-02 15:04:05", "01/02/2006 15:04:05", "01/02/2006 03:04:05 PM", "2006-01-02", "01/02/2006", "Jan 2, 2006 3:04 PM"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

func epoch(f float64) (time.Time, bool) {
	switch {
	case f <= 0:
		return time.Time{}, false
	case f > 1e14: // microseconds
		return time.UnixMicro(int64(f)).UTC(), true
	case f > 1e11: // milliseconds
		return time.UnixMilli(int64(f)).UTC(), true
	}
	return time.Unix(int64(f), 0).UTC(), true
}

// analyticsUnreadable says whether an error means the model route cannot
// be used at all (missing, forbidden, not supported), so auto falls back.
func analyticsUnreadable(err error) bool {
	if errors.Is(err, errNoModel) {
		return true
	}
	switch statusOf(err) {
	case http.StatusForbidden, http.StatusNotFound, http.StatusBadRequest:
		return true
	}
	return false
}

// explainAnalytics names what to fix for the model route.
func explainAnalytics(err error, name string) error {
	switch {
	case errors.Is(err, errNoModel):
		return fmt.Errorf("%v: the Spotter analytics model is visible to administrators by default (in the primary Org); "+
			"sign in as an admin or share %q with this user", err, name)
	case statusOf(err) == http.StatusForbidden:
		return fmt.Errorf("ThoughtSpot refused to read %q (403): it needs the ADMINISTRATION privilege, or the model shared with this user", name)
	}
	return err
}
