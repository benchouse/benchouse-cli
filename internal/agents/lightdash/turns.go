package lightdash

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/pgread"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

// promptRow is an ai_prompt row with its thread (to_jsonb(t)) and position.
type promptRow struct {
	UUID           string          `json:"ai_prompt_uuid"`
	ThreadUUID     string          `json:"ai_thread_uuid"`
	CreatedAt      string          `json:"created_at"`
	UserUUID       *string         `json:"created_by_user_uuid"`
	Prompt         string          `json:"prompt"`
	Response       *string         `json:"response"`
	ErrorMessage   *string         `json:"error_message"`
	RespondedAt    *string         `json:"responded_at"`
	RetriedAt      *string         `json:"retried_at"`
	HumanScore     *json.Number    `json:"human_score"`
	HumanFeedback  *string         `json:"human_feedback"`
	MetricQuery    json.RawMessage `json:"metric_query"` // the pre-agent copilot's generated query
	ExecutionMode  *string         `json:"execution_mode"`
	NeedsUserInput *bool           `json:"needs_user_input"`
	Hidden         *bool           `json:"hidden"`
	Position       *json.Number    `json:"turn_position"`
	ModelConfig    *modelConfig    `json:"model_config"`
	TokenUsage     json.RawMessage `json:"token_usage"`
	Thread         struct {
		AgentUUID   *string `json:"agent_uuid"`
		CreatedFrom string  `json:"created_from"`
		Title       *string `json:"title"`
	} `json:"thread"`
}

type modelConfig struct {
	ModelName     string `json:"modelName"`
	ModelProvider string `json:"modelProvider"`
}

type toolCallRow struct {
	PromptUUID string          `json:"ai_prompt_uuid"`
	CallID     string          `json:"tool_call_id"`
	Name       string          `json:"tool_name"`
	Args       json.RawMessage `json:"tool_args"`
	CreatedAt  string          `json:"created_at"`
}

type toolResultRow struct {
	PromptUUID string          `json:"ai_prompt_uuid"`
	CallID     string          `json:"tool_call_id"`
	Name       string          `json:"tool_name"`
	Result     string          `json:"result"`
	Metadata   json.RawMessage `json:"metadata"`
	CreatedAt  string          `json:"created_at"`
}

// resultMeta is the part of ai_agent_tool_result.metadata read here: every
// tool sets status; runSql adds rowCount, the runQuery family queryUuid.
type resultMeta struct {
	Status    string       `json:"status"`
	RowCount  *json.Number `json:"rowCount"`
	QueryUUID string       `json:"queryUuid"`
}

type compiledRow struct {
	PromptUUID  string       `json:"ai_prompt_uuid"`
	CallID      string       `json:"tool_call_id"`
	QueryUUID   string       `json:"query_uuid"`
	CreatedAt   string       `json:"created_at"`
	CompiledSQL string       `json:"compiled_sql"`
	Status      string       `json:"status"`
	Error       *string      `json:"error"`
	TotalRows   *json.Number `json:"total_row_count"`
}

type reasoningRow struct {
	PromptUUID string `json:"ai_prompt_uuid"`
	Text       string `json:"text"`
}

type agentRow struct {
	UUID string `json:"ai_agent_uuid"`
	Name string `json:"name"`
}

type userRow struct {
	UUID      string  `json:"user_uuid"`
	FirstName string  `json:"first_name"`
	LastName  string  `json:"last_name"`
	Email     *string `json:"email"`
}

// keptTools have results that describe the semantic layer, not the data, so
// they are kept: they are what shows a missing or ambiguous definition.
// searchFieldValues is not here: field values are data.
var keptTools = map[string]bool{
	"findExplores": true, "findFields": true, "searchSemanticLayer": true, "grepFields": true,
	"discoverFields": true, "getMetadata": true, "findContent": true, "findCharts": true,
	"findDashboards": true, "listContent": true,
}

func isRunSQL(name string) bool { return name == "runSql" || name == "run_sql" }

// buildTurns assembles one Turn per prompt from the rows of every arm. It
// returns the turns and how many hidden prompts it left out.
func buildTurns(got map[string][]json.RawMessage, opts agent.Options) ([]turns.Turn, int, error) {
	calls := map[string][]toolCallRow{}
	for _, r := range got["ai_agent_tool_call"] {
		var c toolCallRow
		if pgread.Decode(r, &c) == nil {
			calls[c.PromptUUID] = append(calls[c.PromptUUID], c)
		}
	}
	results := map[string]toolResultRow{} // by tool_call_id
	for _, r := range got["ai_agent_tool_result"] {
		var x toolResultRow
		if pgread.Decode(r, &x) == nil {
			results[x.CallID] = x
		}
	}
	compiled := map[string][]compiledRow{} // by tool_call_id
	for _, r := range got["query_history"] {
		var q compiledRow
		if pgread.Decode(r, &q) == nil {
			compiled[q.CallID] = append(compiled[q.CallID], q)
		}
	}
	reasoning := map[string][]turns.Reasoning{}
	for _, r := range got["ai_agent_reasoning"] {
		var x reasoningRow
		if pgread.Decode(r, &x) == nil {
			reasoning[x.PromptUUID] = append(reasoning[x.PromptUUID], turns.Reasoning{Text: x.Text})
		}
	}
	agents := map[string]string{}
	for _, r := range got["ai_agent"] {
		var a agentRow
		if pgread.Decode(r, &a) == nil {
			agents[a.UUID] = a.Name
		}
	}
	users := map[string]userRow{}
	for _, r := range got["users"] {
		var u userRow
		if pgread.Decode(r, &u) == nil {
			users[u.UUID] = u
		}
	}

	var out []turns.Turn
	hidden := 0
	for _, r := range got["ai_prompt"] {
		var p promptRow
		if err := pgread.Decode(r, &p); err != nil {
			return nil, 0, fmt.Errorf("lightdash ai_prompt row: %w", err)
		}
		if p.Hidden != nil && *p.Hidden {
			hidden++
			continue
		}
		t, err := promptTurn(p, calls[p.UUID], results, compiled, reasoning[p.UUID], opts)
		if err != nil {
			return nil, 0, err
		}
		if p.Thread.AgentUUID != nil {
			if name := agents[*p.Thread.AgentUUID]; name != "" {
				t.Extra["agent_name"] = name
			}
		}
		if opts.IncludeUsers && p.UserUUID != nil {
			if u, ok := users[*p.UserUUID]; ok {
				t.UserName = strings.TrimSpace(u.FirstName + " " + u.LastName)
				t.UserEmail = trim(u.Email)
			}
		}
		if len(t.Extra) == 0 {
			t.Extra = nil
		}
		out = append(out, t)
	}
	return out, hidden, nil
}

// promptTurn maps one prompt and its steps.
func promptTurn(p promptRow, calls []toolCallRow, results map[string]toolResultRow,
	compiled map[string][]compiledRow, reasoning []turns.Reasoning, opts agent.Options) (turns.Turn, error) {
	asked, ok := pgread.Time(p.CreatedAt)
	if !ok {
		return turns.Turn{}, fmt.Errorf("lightdash prompt %s: created_at %q", p.UUID, p.CreatedAt)
	}
	t := turns.Turn{
		TurnID:          p.UUID,
		AskedAt:         asked,
		Question:        p.Prompt,
		Answer:          p.Response,
		CompletedAt:     pgread.TimePtr(p.RespondedAt),
		ConversationID:  p.ThreadUUID,
		ClientInterface: clientInterface(p.Thread.CreatedFrom),
		Reasoning:       reasoning,
		Statements:      []turns.Statement{},
		ToolCalls:       []turns.ToolCall{},
		Extra:           map[string]any{},
	}
	if p.UserUUID != nil {
		t.UserID = *p.UserUUID
	}
	if p.Position != nil {
		if n, err := p.Position.Int64(); err == nil {
			t.Position = &n
		}
	}
	if p.ModelConfig != nil {
		t.ModelName = p.ModelConfig.ModelName
		if p.ModelConfig.ModelProvider != "" {
			t.Extra["model_provider"] = p.ModelConfig.ModelProvider
		}
	}
	switch {
	case trim(p.ErrorMessage) != "":
		t.Outcome, t.ErrorMessage, t.Answer = "failed", trim(p.ErrorMessage), nil
	case p.Response != nil:
		t.Outcome = "completed"
	default:
		t.Outcome = "running"
	}

	// Feedback: human_score is -1, 0 or 1; human_feedback the comment.
	if p.HumanScore != nil || trim(p.HumanFeedback) != "" {
		fb := map[string]any{}
		if p.HumanScore != nil {
			if n, err := p.HumanScore.Int64(); err == nil {
				fb["score"] = n
			}
		}
		if c := trim(p.HumanFeedback); c != "" {
			fb["comment"] = c
		}
		t.Extra["feedback"] = fb
	}
	if p.NeedsUserInput != nil && *p.NeedsUserInput {
		t.Extra["needs_user_input"] = true // the answer is a question back to the user
	}
	if m := trim(p.ExecutionMode); m != "" && m != "standard" {
		t.Extra["execution_mode"] = m
	}
	if p.RetriedAt != nil {
		t.Extra["retried_at"] = *p.RetriedAt
	}
	if pgread.Present(p.TokenUsage) {
		var u struct {
			TotalTokens *json.Number `json:"totalTokens"`
		}
		if json.Unmarshal(p.TokenUsage, &u) == nil && u.TotalTokens != nil {
			if n, err := u.TotalTokens.Int64(); err == nil {
				t.Extra["total_tokens"] = n // Lightdash records only the total, not input/output
			}
		}
	}
	if p.Thread.Title != nil && *p.Thread.Title != "" {
		t.Extra["thread_title"] = *p.Thread.Title
	}

	for _, c := range calls {
		res, hasResult := results[c.CallID]
		var meta resultMeta
		if hasResult && pgread.Present(res.Metadata) {
			_ = json.Unmarshal(res.Metadata, &meta)
		}
		status := toolStatus(meta.Status)
		started := pgread.TimePtr(&c.CreatedAt)

		if isRunSQL(c.Name) {
			var args struct {
				SQL string `json:"sql"`
			}
			_ = json.Unmarshal(c.Args, &args)
			st := turns.Statement{SQL: args.SQL, Status: status, StartedAt: started}
			if meta.RowCount != nil {
				if n, err := meta.RowCount.Int64(); err == nil {
					st.Rows = &n
				}
			}
			if status == "failed" && hasResult {
				st.Error = res.Result // the error text runSql returned
			}
			if opts.IncludeResults && hasResult && status == "succeeded" {
				st.Result = res.Result
			}
			t.Statements = append(t.Statements, st)
			continue
		}

		tc := turns.ToolCall{Name: c.Name, Status: status, StartedAt: started}
		if status == "" {
			tc.Status = meta.Status
		}
		if pgread.Present(c.Args) {
			tc.Input = c.Args
		}
		if hasResult {
			if opts.IncludeResults || keptTools[c.Name] || status == "failed" {
				tc.Output = res.Result
			} else {
				tc.Output = map[string]any{"redacted": "tool result removed by benchouse-cli"}
			}
		}
		t.ToolCalls = append(t.ToolCalls, tc)

		// The SQL Lightdash compiled from this metric query, if it ran.
		for _, q := range compiled[c.CallID] {
			if strings.TrimSpace(q.CompiledSQL) == "" {
				continue
			}
			st := turns.Statement{SQL: q.CompiledSQL, Status: queryStatus(q.Status), StartedAt: pgread.TimePtr(&q.CreatedAt)}
			if q.TotalRows != nil {
				if n, err := q.TotalRows.Int64(); err == nil {
					st.Rows = &n
				}
			}
			if q.Error != nil {
				st.Error = *q.Error
			}
			t.Statements = append(t.Statements, st)
		}
	}

	// The pre-agent copilot stored its generated metric query on the prompt.
	if pgread.Present(p.MetricQuery) {
		t.ToolCalls = append(t.ToolCalls, turns.ToolCall{Name: "ai_prompt.metric_query", Input: p.MetricQuery})
	}
	return t, nil
}

// toolStatus maps ai_agent_tool_result.metadata.status to the contract.
func toolStatus(s string) string {
	switch s {
	case "success":
		return "succeeded"
	case "error", "timeout":
		return "failed"
	case "rejected": // the user refused the SQL approval
		return "not_executed"
	}
	return ""
}

// queryStatus maps query_history.status to the contract. expired means the
// query ran and its cached results have since expired.
func queryStatus(s string) string {
	switch s {
	case "ready", "expired":
		return "succeeded"
	case "error", "cancelled":
		return "failed"
	}
	return ""
}
