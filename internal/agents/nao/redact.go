package nao

import (
	"encoding/json"
	"errors"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/files"
)

const resultsRedaction = "nao message_part.tool_output and mcp_call_log.tool_output: rows returned by the agent's queries removed " +
	"(columns, row count and errors kept); context-file reads, clarifications and nao's MCP answers kept, except reads of " +
	"sample-data files. nao context folder: databases/**/preview.md and profiling.md (sample rows and " +
	"top values) left out. Use --include-results to keep them."

// keptTools have outputs that are not business data, so they are kept: the
// context-file tools return the semantic layer (which is what shows a missing
// definition), and clarification and suggest_follow_ups return the agent's
// own words. Their outputs still go through the secret scan, and a read of a
// sample-data file is withheld like a query result.
var keptTools = map[string]bool{
	"read": true, "search": true, "list": true, "grep": true, "glob": true,
	"clarification": true, "suggest_follow_ups": true,
}

// keptOutputKeys survive redaction of a tool's output: shape and outcome, no
// values. Each keeps only a scalar (see keptValue).
var keptOutputKeys = map[string]bool{
	"row_count": true, "rowCount": true, "columns": true, "column_names": true,
	"error": true, "status": true, "message": true, "truncated": true, "is_truncated": true,
	"id": true, "query_id": true,
}

const (
	maxKeptString  = 500
	maxColumnName  = 200
	sampleRedacted = "sample rows from a nao preview/profiling file removed by benchouse-cli"
)

// dataSample reports whether p is one of the files `nao sync` writes with
// the warehouse's own values in it: preview.md holds sample rows and
// profiling.md each column's top values. columns.md, annotations.md and the
// rest describe the schema and are kept.
func dataSample(p string) bool {
	p = strings.ReplaceAll(p, "\\", "/")
	b := strings.ToLower(path.Base(p))
	return (b == "preview.md" || b == "profiling.md") && strings.Contains("/"+p, "/databases/")
}

// redactPart strips query results from a message_part row and masks secrets
// in the outputs it keeps. It returns the new row and true when it changed
// something.
//
// tool_output is jsonb on Postgres (an object in the row) and text on sqlite
// (a string holding JSON); the redacted value keeps that shape.
func redactPart(row json.RawMessage, m map[string]any, opts agent.Options) (json.RawMessage, bool) {
	return redactOutput(row, m, opts, keptTools)
}

// keptMCPTools are the MCP tools whose logged outputs are kept (secret
// scanned): ask_nao and get_nao_answer return nao's own answer, grep and ls
// the context layer. Every other tool's output (execute_sql,
// execute_semantic_query, display_chart in chart-data mode, stories) can hold
// the warehouse's rows, so it is reduced to its shape like a query result.
var keptMCPTools = map[string]bool{
	"ask_nao": true, "get_nao_answer": true, "grep": true, "ls": true,
}

// redactMCPCall strips query results from an mcp_call_log row. nao logs an
// MCP tool's output verbatim (mcp/logging.ts: the parsed JSON when it fits
// in 10,000 characters), so an execute_sql call's row holds its data.
func redactMCPCall(row json.RawMessage, m map[string]any, opts agent.Options) (json.RawMessage, bool) {
	return redactOutput(row, m, opts, keptMCPTools)
}

func redactOutput(row json.RawMessage, m map[string]any, opts agent.Options, kept map[string]bool) (json.RawMessage, bool) {
	raw, present := m["tool_output"]
	if !present || raw == nil {
		return row, false
	}
	tool := strings.ToLower(str(m["tool_name"]))
	if kept[tool] {
		return redactKept(row, m, raw, tool, opts)
	}
	if opts.IncludeResults {
		return row, false
	}

	var out map[string]any
	asString := false
	switch v := raw.(type) {
	case map[string]any:
		out = v
	case string:
		if strings.TrimSpace(v) == "" {
			return row, false
		}
		asString = true
		if err := decodeInto(v, &out); err != nil {
			// Not JSON: plain text output, which may itself be a result.
			m["tool_output"] = `{"redacted":"tool output removed"}`
			b, _ := json.Marshal(m)
			return b, true
		}
	default:
		m["tool_output"] = map[string]any{"redacted": "tool output removed"}
		b, _ := json.Marshal(m)
		return b, true
	}

	shape := map[string]any{"redacted": "query results removed by benchouse-cli"}
	for k, v := range out {
		if !keptOutputKeys[k] {
			continue
		}
		if v, ok := keptValue(k, v); ok {
			shape[k] = v
		}
	}
	// Keep the column names when the output only carries them as row keys.
	if _, ok := shape["columns"]; !ok {
		if cols := columnsOf(out); len(cols) > 0 {
			shape["columns"] = cols
		}
	}
	return setOutput(row, m, shape, asString)
}

// keptValue returns what survives of a kept key: a column list as short
// strings, anything else only as a scalar (a short string, a number, a
// bool). An error or message is a string the agent's database wrote and may
// quote a value, so it is length-capped and secret-scanned too.
func keptValue(k string, v any) (any, bool) {
	if k == "columns" || k == "column_names" {
		arr, ok := v.([]any)
		if !ok {
			return nil, false
		}
		cols := make([]any, 0, len(arr))
		for _, c := range arr {
			if s, ok := c.(string); ok && len(s) <= maxColumnName {
				s, _ = files.MaskTokens(s)
				cols = append(cols, s)
			}
		}
		return cols, true
	}
	switch t := v.(type) {
	case string:
		if len(t) > maxKeptString {
			return nil, false
		}
		s, _ := files.MaskTokens(t)
		return s, true
	case json.Number, bool, float64:
		return t, true
	}
	return nil, false
}

// redactKept handles a context-file or conversational tool's output: kept,
// with secrets masked, unless it shows a sample-data file and results are off.
func redactKept(row json.RawMessage, m map[string]any, raw any, tool string, opts agent.Options) (json.RawMessage, bool) {
	var (
		out      any
		asString bool
	)
	if s, ok := raw.(string); ok {
		asString = true
		if err := decodeInto(s, &out); err != nil {
			out = s // plain text: scanned as is
			asString = false
		}
	} else {
		out = raw
	}
	changed := false
	if !opts.IncludeResults {
		switch tool {
		case "read":
			if dataSample(inputPath(m, "file_path")) {
				out, changed = map[string]any{"redacted": sampleRedacted}, true
			}
		case "grep":
			changed = dropSampleMatches(out)
		case "ask_nao", "get_nao_answer":
			if t, ok := out.(string); ok {
				out, changed = dropAnswerPreviews(t)
			}
		}
	}
	out, n := maskStrings(out)
	if !changed && n == 0 {
		return row, false
	}
	return setOutput(row, m, out, asString)
}

// dropAnswerPreviews removes the rows from an ask_nao / get_nao_answer
// output. nao's sub-agent answer (mcp/tools/sub-agent.ts) is the answer text,
// then a line of JSON {"queries":[{id, columns, row_count, preview}], ...}
// where preview is the first rows of each query: the preview goes, the rest
// of the line stays.
func dropAnswerPreviews(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	changed := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "{") || !strings.Contains(t, `"queries"`) {
			continue
		}
		var v map[string]any
		if decodeInto(t, &v) != nil {
			continue
		}
		qs, ok := v["queries"].([]any)
		if !ok {
			continue
		}
		for _, q := range qs {
			if qm, ok := q.(map[string]any); ok {
				if _, has := qm["preview"]; has {
					delete(qm, "preview")
					changed = true
				}
			}
		}
		if b, err := json.Marshal(v); err == nil {
			lines[i] = string(b)
		}
	}
	if !changed {
		return s, false
	}
	return strings.Join(lines, "\n"), true
}

// dropSampleMatches blanks grep matches found in sample-data files, which
// quote their rows. It reports whether it changed anything.
func dropSampleMatches(out any) bool {
	o, ok := out.(map[string]any)
	if !ok {
		return false
	}
	matches, ok := o["matches"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, x := range matches {
		mm, ok := x.(map[string]any)
		if !ok || !dataSample(str(mm["path"])) {
			continue
		}
		mm["line_content"] = sampleRedacted
		delete(mm, "context_before")
		delete(mm, "context_after")
		changed = true
	}
	return changed
}

// inputPath reads a string field of the part's tool_input, which, like
// tool_output, is an object on Postgres and a JSON string on sqlite.
func inputPath(m map[string]any, key string) string {
	var in map[string]any
	switch v := m["tool_input"].(type) {
	case map[string]any:
		in = v
	case string:
		if decodeInto(v, &in) != nil {
			return ""
		}
	}
	return str(in[key])
}

// maskStrings masks secrets in every string inside v and counts them.
func maskStrings(v any) (any, int) {
	switch t := v.(type) {
	case string:
		return files.MaskTokens(t)
	case map[string]any:
		total := 0
		for k, c := range t {
			var n int
			t[k], n = maskStrings(c)
			total += n
		}
		return t, total
	case []any:
		total := 0
		for i, c := range t {
			var n int
			t[i], n = maskStrings(c)
			total += n
		}
		return t, total
	}
	return v, 0
}

func setOutput(row json.RawMessage, m map[string]any, out any, asString bool) (json.RawMessage, bool) {
	if asString {
		b, err := json.Marshal(out)
		if err != nil {
			return row, false
		}
		m["tool_output"] = string(b)
	} else {
		m["tool_output"] = out
	}
	b, err := json.Marshal(m)
	if err != nil {
		return row, false
	}
	return b, true
}

// decodeInto decodes s, which must be one JSON value and nothing else: a
// Decoder alone accepts a prefix, so "17 of 103 products ..." would read as
// the number 17.
func decodeInto(s string, v any) error {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

// columnsOf finds column names in an output shaped {"data": [{col: v}, ...]}
// or {"rows": [...]}.
func columnsOf(out map[string]any) []string {
	for _, key := range []string{"data", "rows", "results", "records"} {
		arr, ok := out[key].([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		first, ok := arr[0].(map[string]any)
		if !ok {
			continue
		}
		cols := make([]string, 0, len(first))
		for c := range first {
			if len(c) <= maxColumnName {
				cols = append(cols, c)
			}
		}
		sort.Strings(cols)
		return cols
	}
	return nil
}
