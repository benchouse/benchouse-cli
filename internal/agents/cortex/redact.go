package cortex

import (
	"encoding/json"
	"strings"
)

const spanRedaction = "Cortex Agents spans: tool result attributes (rows returned by executed SQL and other tool outputs) " +
	"removed from RECORD_ATTRIBUTES; row counts, SQL, status and the agent's answer kept. Use --include-results to keep them."

// resultAttr reports whether a span attribute holds a tool's returned data.
// The agent's own answer (agent.response) and planning are kept; a tool's
// result payload and output are not, except their row counts. Keys are
// compared lowercased: an upper-case RESULT holds rows all the same.
func resultAttr(key string) bool {
	key = strings.ToLower(key)
	if !strings.Contains(key, ".tool.") {
		return false
	}
	if strings.HasSuffix(key, ".num_rows") || strings.HasSuffix(key, ".row_count") {
		return false
	}
	for _, seg := range strings.Split(key, ".") {
		switch seg {
		case "result", "results", "output", "outputs", "data", "content":
			return true
		}
	}
	return false
}

// redactSpan removes result attributes from a span row. It returns the new
// row and true when it changed something.
func redactSpan(m map[string]any) (json.RawMessage, bool) {
	ra, ok := m["RECORD_ATTRIBUTES"].(map[string]any)
	if !ok {
		return nil, false
	}
	changed := false
	for k := range ra {
		if resultAttr(k) {
			ra[k] = "[removed by benchouse-cli]"
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return b, true
}
