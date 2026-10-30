package telemetry

import (
	"regexp"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// The events, one function each, so every property name and value is
// decided here. docs/usage.md ("Usage stats") lists them; keep it in step.

// Started: a command began. command is guided, upload, collect, send,
// inspect, plan or agents.
func Started(command string) {
	if c := current.Load(); c != nil {
		c.mu.Lock()
		c.started = true
		c.mu.Unlock()
	}
	track("cli_started", map[string]any{"command": command})
}

// AgentSelected: the user picked an agent (its id), or "other".
func AgentSelected(agentID string) {
	if c := current.Load(); c != nil {
		c.mu.Lock()
		c.agent = agentID
		c.mu.Unlock()
	}
	track("cli_agent_selected", map[string]any{"agent": agentID})
}

// OtherRequested: an unsupported agent was named; only the name's length is
// sent (the name itself goes to the agent-request endpoint).
func OtherRequested(nameLen int) {
	track("cli_other_requested", map[string]any{"agent_text_length": nameLen})
}

// OtherEmailGiven: the request-an-agent flow ended with or without an email.
func OtherEmailGiven(given bool) {
	track("cli_other_email_given", map[string]any{"email_given": given})
}

// Reached records where the run is, for cli_quit, without an event.
func Reached(step string) {
	if c := current.Load(); c != nil {
		c.mu.Lock()
		c.step = step
		c.mu.Unlock()
	}
}

// StepViewed: a step was shown. index is 1-based, of count.
func StepViewed(agentID, step string, index, count int) {
	Reached(step)
	track("cli_step_viewed", map[string]any{"agent": agentID, "step": step, "step_index": index, "step_count": count})
}

// KnownInputs registers a run's inputs with Known under group: the
// values of fields without fixed choices (secret ones as <secret>), plus
// extra values such as the email and folder paths.
func KnownInputs(group string, fields []agent.Field, in agent.Inputs, extra ...string) {
	vals := map[string]bool{}
	for _, f := range fields {
		if len(f.Choices) == 0 {
			if v := in[f.Key]; v != "" {
				vals[v] = vals[v] || f.Secret
			}
		}
	}
	for _, v := range extra {
		if _, ok := vals[v]; !ok && v != "" {
			vals[v] = false
		}
	}
	Known(group, vals)
}

// Steps is the order of the steps a run can view: step_index is a
// position in it. context is there when the agent has a context folder;
// uploading only for an upload.
func Steps(hasContext, uploading bool) []string {
	s := []string{"conversations", "dbt"}
	if hasContext {
		s = append(s, "context")
	}
	s = append(s, "review", "summary")
	if uploading {
		s = append(s, "uploading")
	}
	return append(s, "done")
}

// View is StepViewed with the index and count taken from Steps.
func View(agentID, step string, hasContext, uploading bool) {
	steps := Steps(hasContext, uploading)
	for i, x := range steps {
		if x == step {
			StepViewed(agentID, step, i+1, len(steps))
			return
		}
	}
}

var fieldRe = regexp.MustCompile(`--([a-z][a-z0-9-]*)`)

// FieldOf is the field (flag) an agent's Validate error names first:
// "--host: ..." or "Nao needs --host, --password" give "host".
func FieldOf(err error) string {
	if err == nil {
		return ""
	}
	if m := fieldRe.FindStringSubmatch(err.Error()); m != nil {
		return m[1]
	}
	return ""
}

// FieldFailed: a connection field was refused, by its own validation
// (kind "validate") or by a live check (kind "verify").
func FieldFailed(agentID, field, kind string, err error) {
	track("cli_field_failed", map[string]any{"agent": agentID, "field": field, "kind": kind,
		"category": Category(err), "message": Message(err)})
}

// FieldVerified: a field's live check passed.
func FieldVerified(agentID, field string) {
	track("cli_field_verified", map[string]any{"agent": agentID, "field": field})
}

// CheckFinished: a step's check (conversations, dbt or context) ran.
func CheckFinished(agentID, step string, err error, warnings int, d time.Duration) {
	p := map[string]any{"agent": agentID, "step": step, "ok": err == nil, "category": Category(err),
		"warnings": warnings, "duration_ms": d.Milliseconds()}
	if err != nil {
		p["message"] = Message(err)
	}
	track("cli_check_finished", p)
}

// Collection is what a bundle holds, in counts.
type Collection struct {
	Conversations, Records int
	Sampled                bool
	FilesDBT, FilesContext int
	Size                   int64
	Duration               time.Duration
	IncludeResults         bool
	IncludeUsers           bool
	Anonymize              bool
}

// Collected: a bundle was written.
func Collected(agentID string, c Collection) {
	track("cli_collected", map[string]any{"agent": agentID, "conversations": c.Conversations, "records": c.Records,
		"sampled": c.Sampled, "files_dbt": c.FilesDBT, "files_context": c.FilesContext, "size_bucket": SizeBucket(c.Size),
		"duration_ms": c.Duration.Milliseconds(), "include_results": c.IncludeResults, "include_users": c.IncludeUsers,
		"anonymize": c.Anonymize})
}

// CollectFailed: collecting stopped with err.
func CollectFailed(agentID string, err error) {
	track("cli_collect_failed", map[string]any{"agent": agentID, "category": Category(err), "message": Message(err)})
}

// Uploaded: a bundle of size bytes was uploaded.
func Uploaded(agentID string, size int64) {
	track("cli_uploaded", map[string]any{"agent": agentID, "size_bucket": SizeBucket(size)})
}

// UploadFailed: an upload failed; http_status is 0 when no answer came.
func UploadFailed(agentID string, err error) {
	track("cli_upload_failed", map[string]any{"agent": agentID, "http_status": HTTPStatus(err), "category": Category(err)})
}

// Saved: a bundle was written and kept, not uploaded.
func Saved(agentID string) { track("cli_saved", map[string]any{"agent": agentID}) }

// Quit: the run ended. how is done, ctrl_c, error or signal. Only the
// first call sends anything, and only after Started.
func Quit(how string) {
	c := current.Load()
	if c == nil {
		return
	}
	c.quitOnce.Do(func() {
		c.mu.Lock()
		step, agent, started := c.step, c.agent, c.started
		c.mu.Unlock()
		if !started {
			return
		}
		track("cli_quit", map[string]any{"last_step": step, "how": how, "agent": agent})
	})
}

// SizeBucket is a bundle size as <1MB, 1-5MB, 5-32MB or >32MB.
func SizeBucket(n int64) string {
	const mb = 1 << 20
	switch {
	case n < mb:
		return "<1MB"
	case n < 5*mb:
		return "1-5MB"
	case n <= 32*mb:
		return "5-32MB"
	}
	return ">32MB"
}
