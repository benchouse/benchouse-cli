package genie

import "encoding/json"

const resultsRedaction = "Genie conversation messages: query result rows (statement_response, data_array, " +
	"data_typed_array) removed wherever they appear; SQL, statement ids, row counts, thoughts and the answer text kept. " +
	"The message API returns row counts but not rows, so normally nothing is removed. Use --include-results to keep them."

// resultKeys hold a statement's returned data. The message endpoint answers
// with query_result {statement_id, row_count} and per-attachment
// query_result_metadata.row_count only; the rows themselves sit behind the
// separate query-result endpoint, which the CLI never calls. These keys are
// what that endpoint's payload is made of, so a message that ever embeds one
// still leaves its rows behind.
var resultKeys = map[string]bool{"statement_response": true, "data_array": true, "data_typed_array": true}

// redactMessage removes result rows from a message body. It returns the new
// body and true when it changed something.
func redactMessage(body []byte) (json.RawMessage, bool) {
	m, err := decode(body)
	if err != nil {
		return nil, false
	}
	if !strip(m) {
		return nil, false
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return b, true
}

func strip(v any) bool {
	changed := false
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if resultKeys[k] {
				t[k] = "[removed by benchouse-cli]"
				changed = true
				continue
			}
			if strip(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range t {
			if strip(child) {
				changed = true
			}
		}
	}
	return changed
}
