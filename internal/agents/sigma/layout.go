package sigma

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// The AI usage table's documented columns (configure-ai-usage-dashboard,
// the INSERT that migrates Assistant usage): Sigma creates them as quoted
// lowercase identifiers.
const (
	cTrace     = "trace_id"
	cSpan      = "span_id"
	cParent    = "parent_id"
	cSpanType  = "span_type"
	cTraceName = "trace_name"
	cAgentName = "agent_name"
	cAgentID   = "agent_id"
	cUserName  = "user_name"
	cSession   = "session_id"
	cProvider  = "ai_provider"
	cModel     = "model"
	cTokIn     = "tokens_input"
	cTokOut    = "tokens_output"
	cTags      = "tags"
	cTools     = "tool_calls"
	cInput     = "input"
	cOutput    = "output"
	cMeta      = "metadata"
	cAt        = "trace_created_at_utc"
)

var documented = []string{cTrace, cSpan, cParent, cSpanType, cTraceName, cAgentName, cAgentID, cUserName,
	cSession, cProvider, cModel, cTokIn, cTokOut, cTags, cTools, cInput, cOutput, cMeta, cAt}

// Sigma input-table bookkeeping columns, in the same INSERT. Read only to
// keep the latest version of an edited row.
const (
	sID         = "id"
	sSeq        = "seq_num"
	sRowVersion = "row_version"
	sUpdatedBy  = "updated_by"
	sUpdatedAt  = "updated_at"
)

var bookkeeping = []string{sID, sSeq, sRowVersion, sUpdatedBy, sUpdatedAt}

// required are the columns a turn cannot be built without.
var required = []string{cTrace, cInput, cAt}

type column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// layout is the table the statements read: as planned (documented column
// names, the table as given) or as discovered at run time.
type layout struct {
	d    dialect
	loc  location
	name string
	// cols are the discovered columns by lowercased name; nil when planned.
	cols map[string]column
	// order is the discovered column names, as the table lists them.
	order []string
}

// tableUnknown stands for the table in a plan made before discovery.
const tableUnknown = "<the AI_USAGE_ table found>"

func (l layout) ref() string { return l.d.table(l.loc, l.name) }

func (l layout) has(c string) bool {
	if l.cols == nil {
		return true
	}
	_, ok := l.cols[c]
	return ok
}

func (l layout) col(c string) string {
	if k, ok := l.cols[c]; ok {
		return l.d.quote(k.Name)
	}
	return l.d.quote(c)
}

func (l layout) typ(c string) string { return l.cols[c].Type }

// conv is a row's conversation: its session, or its trace when Sigma left
// the session empty (rows migrated from Assistant usage have none).
func (l layout) conv() string {
	if l.has(cSession) {
		return "COALESCE(NULLIF(" + l.col(cSession) + ", ''), " + l.col(cTrace) + ")"
	}
	return l.col(cTrace)
}

func (l layout) where(opts agent.Options, conds ...string) string {
	if !opts.Since.IsZero() {
		conds = append(conds, l.d.since(l.col(cAt), l.typ(cAt), opts.Since))
	}
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// countStmt counts the conversations in the window, with the latest activity.
func (l layout) countStmt(opts agent.Options) string {
	return l.d.jsonOf("SELECT COUNT(DISTINCT " + l.conv() + ") AS n, MAX(" + l.col(cAt) + ") AS last_at FROM " + l.ref() + l.where(opts))
}

// candidatesStmt chooses the conversations: the most recent in the window
// by last activity, at most the cap.
func (l layout) candidatesStmt(opts agent.Options) string {
	inner := "SELECT " + l.conv() + " AS conversation_id, MAX(" + l.col(cAt) + ") AS last_at FROM " + l.ref() +
		l.where(opts, l.conv()+" IS NOT NULL") + " GROUP BY 1 ORDER BY 2 DESC"
	if opts.MaxConversations > 0 {
		inner += fmt.Sprintf(" LIMIT %d", opts.MaxConversations)
	}
	return l.d.jsonOf(inner)
}

// batchStmt reads every row of n chosen conversations, bound as parameters.
func (l layout) batchStmt(opts agent.Options, n int) string {
	return l.d.jsonFrom(l.ref(), l.where(opts, l.conv()+" IN ("+placeholders(l.d, n)+")"))
}

// plannedBatch is batchStmt as the plan and the manifest show it.
func (l layout) plannedBatch(opts agent.Options) string {
	return l.d.jsonFrom(l.ref(), l.where(opts, fmt.Sprintf("%s IN (<up to %d conversation ids>)", l.conv(), agent.BatchSize)))
}

const (
	purposeTables     = "find the Sigma AI usage table (information schema)"
	purposeColumns    = "the AI usage table's columns (information schema)"
	purposeCount      = "conversations in the window (count)"
	purposeCandidates = "Sigma AI conversations (the most recent sessions in the window)"
	purposeRows       = "AI usage spans of the chosen conversations"
)

func planned(in agent.Inputs, opts agent.Options) []queryPlan {
	d := dialectFor(in.Get("platform"))
	loc, _ := locationOf(in)
	name := in.Get("table")
	if name == "" {
		name = tableUnknown
	}
	l := layout{d: d, loc: loc, name: name}
	ts, _ := d.tables(loc, in.Get("table"))
	cs, _ := d.columns(loc, "", name)
	out := []queryPlan{{purposeTables, ts}, {purposeColumns, cs}}
	if opts.MaxConversations > 0 {
		out = append(out, queryPlan{purposeCount, l.countStmt(opts)})
	}
	return append(out, queryPlan{purposeCandidates, l.candidatesStmt(opts)}, queryPlan{purposeRows, l.plannedBatch(opts)})
}

type queryPlan struct{ purpose, stmt string }

// errNotConfigured is the setup step, for a location without the table.
func errNotConfigured(loc location, named string) error {
	what := "no " + usagePrefix + "<ORGANIZATION> table in " + loc.String()
	if named != "" {
		what = "no table " + named + " in " + loc.String()
	}
	return fmt.Errorf("%s. AI usage logging is not configured: a Sigma admin turns it on under Administration → AI settings "+
		"→ Chat history and usage data → Track AI usage → Set up AI usage table (see %s); only conversations after that are logged. "+
		"If it was set up, give the database/schema (or dataset) and --table it writes to, and check the role can see it", what, docsURL)
}

// discover finds the table and its columns. record is told each statement run.
func discover(ctx context.Context, b backend, in agent.Inputs, record func(purpose, stmt string, rows int)) (layout, []string, error) {
	d := dialectFor(in.Get("platform"))
	loc, err := locationOf(in)
	if err != nil {
		return layout{}, nil, err
	}
	named := in.Get("table")
	type found struct{ Schema, Name string }
	var tables []found
	stmt, args := d.tables(loc, named)
	n, err := b.each(ctx, stmt, args, func(m map[string]any) error {
		f := found{Schema: str(m["schema"]), Name: str(m["name"])}
		if f.Name != "" {
			tables = append(tables, f)
		}
		return nil
	})
	if err != nil {
		return layout{}, nil, fmt.Errorf("could not read the information schema of %s: %s", loc, agent.Clip(err))
	}
	record(purposeTables, stmt, n)
	if len(tables) == 0 {
		return layout{}, nil, errNotConfigured(loc, named)
	}
	var names []string
	for _, t := range tables {
		names = append(names, t.Name)
	}
	pick := tables[0]
	l := layout{d: d, loc: loc, name: pick.Name, cols: map[string]column{}}
	stmt, args = d.columns(loc, pick.Schema, pick.Name)
	n, err = b.each(ctx, stmt, args, func(m map[string]any) error {
		c := column{Name: str(m["name"]), Type: str(m["type"])}
		if c.Name != "" {
			l.cols[strings.ToLower(c.Name)] = c
			l.order = append(l.order, c.Name)
		}
		return nil
	})
	if err != nil {
		return layout{}, nil, fmt.Errorf("could not read the columns of %s: %s", l.ref(), agent.Clip(err))
	}
	record(purposeColumns, stmt, n)
	if len(l.cols) == 0 {
		return layout{}, nil, fmt.Errorf("%s has no columns the connection can see: grant it SELECT on the table", l.ref())
	}
	var missing []string
	for _, c := range required {
		if !l.has(c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return layout{}, nil, fmt.Errorf("%s lacks %s: it is not a Sigma AI usage table (the retired ASK_ Assistant usage table is not read)",
			l.ref(), strings.Join(missing, ", "))
	}
	return l, names, nil
}

// absent are the documented columns the table lacks.
func (l layout) absent() []string {
	var out []string
	for _, c := range documented {
		if !l.has(c) {
			out = append(out, c)
		}
	}
	return out
}

// unmapped are the table's columns the reader does not use.
func (l layout) unmapped() []string {
	known := map[string]bool{}
	for _, c := range append(append([]string{}, documented...), bookkeeping...) {
		known[c] = true
	}
	var out []string
	for _, c := range l.order {
		if !known[strings.ToLower(c)] {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// decodeRow reads one JSON row, keys lowercased, numbers kept exact.
func decodeRow(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode row: %w", err)
	}
	if m == nil {
		return nil, errors.New("decode row: not an object")
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out, nil
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
