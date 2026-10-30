package metabase

import (
	"encoding/json"
	"strings"
)

// part is one v2 at-rest message part, the subset of fields read here.
type part struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"` // dynamic-tool parts only
	State      string          `json:"state"`
	Input      json.RawMessage `json:"input"`
	Output     json.RawMessage `json:"output"`
	ErrorText  string          `json:"errorText"`
}

// tool returns the tool name of a tool part, or "".
func (p part) tool() string {
	if p.Type == "dynamic-tool" {
		return p.ToolName
	}
	if strings.HasPrefix(p.Type, "tool-") {
		return strings.TrimPrefix(p.Type, "tool-")
	}
	return ""
}

// parseParts reads metabot_message.data in either at-rest format and returns
// v2 parts. version is data_version: 2 is the v2 format, anything else
// (including a database from before the column existed) is v1.
func parseParts(data json.RawMessage, version int64) []part {
	if len(data) == 0 {
		return nil
	}
	if version == 2 {
		var ps []part
		if json.Unmarshal(data, &ps) != nil {
			return nil
		}
		return ps
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(data, &entries) != nil || len(entries) == 0 {
		return nil
	}
	switch first := entries[0]; {
	case first["_type"] != nil:
		return fromAIService(entries)
	case first["type"] != nil:
		return fromNative(entries)
	case str(first["role"]) == "user":
		var out []part
		for _, e := range entries {
			out = append(out, part{Type: "text", Text: str(e["content"])})
		}
		return out
	}
	return nil
}

// fromNative converts the v1 native agent's entries, as
// migrate-v1-native->v2 does: each tool-input merges with its tool-output.
func fromNative(entries []map[string]json.RawMessage) []part {
	outputs := map[string]map[string]json.RawMessage{}
	for _, e := range entries {
		if str(e["type"]) == "tool-output" {
			outputs[str(e["id"])] = e
		}
	}
	var out []part
	for _, e := range entries {
		switch str(e["type"]) {
		case "text":
			out = append(out, part{Type: "text", Text: str(e["text"])})
		case "tool-input":
			p := part{Type: "tool-" + str(e["function"]), ToolCallID: str(e["id"]), State: "input-available", Input: e["arguments"]}
			if o, ok := outputs[p.ToolCallID]; ok {
				if present(o["error"]) {
					p.State, p.ErrorText = "output-error", errorText(o["error"])
				} else {
					p.State, p.Output = "output-available", o["result"]
				}
			}
			out = append(out, p)
		}
	}
	return out
}

// fromAIService converts the retired external ai-service's entries, as
// migrate-v1-external-ai-service->v2 does: each call in a TOOL_CALL merges
// with its TOOL_RESULT, whose content becomes the output.
func fromAIService(entries []map[string]json.RawMessage) []part {
	results := map[string]string{}
	for _, e := range entries {
		if str(e["_type"]) == "TOOL_RESULT" {
			results[str(e["tool_call_id"])] = str(e["content"])
		}
	}
	var out []part
	for _, e := range entries {
		switch str(e["_type"]) {
		case "TEXT":
			out = append(out, part{Type: "text", Text: str(e["content"])})
		case "TOOL_CALL":
			var calls []struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"` // JSON as a string
			}
			_ = json.Unmarshal(e["tool_calls"], &calls)
			for _, c := range calls {
				p := part{Type: "tool-" + c.Name, ToolCallID: c.ID, State: "input-available"}
				if json.Valid([]byte(c.Arguments)) {
					p.Input = json.RawMessage(c.Arguments)
				} else if b, err := json.Marshal(c.Arguments); err == nil {
					p.Input = b
				}
				if r, ok := results[c.ID]; ok {
					p.State = "output-available"
					p.Output, _ = json.Marshal(map[string]string{"output": r})
				}
				out = append(out, p)
			}
		}
	}
	return out
}

func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// str reads a JSON string, or "" for anything else.
func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// errorText reads an error that is a string or a map with a message.
func errorText(raw json.RawMessage) string {
	if s := str(raw); s != "" {
		return s
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"message", "errorText", "error"} {
			if s, ok := m[k].(string); ok && s != "" {
				return s
			}
		}
	}
	return strings.TrimSpace(string(raw))
}

// structured returns a tool output's structured output: new writes use
// structured_output, rows migrated from v1 may keep structured-output.
func structured(output json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(output, &m) != nil {
		return nil
	}
	for _, k := range []string{"structured_output", "structured-output"} {
		var s map[string]json.RawMessage
		if present(m[k]) && json.Unmarshal(m[k], &s) == nil {
			return s
		}
	}
	return nil
}
