// Package bundle defines the upload format: one gzipped tar holding a
// manifest, the captured conversation records, and copies of the project
// folders the user chose to include.
//
//	manifest.json      what is inside, how it was made, every query that ran
//	records.jsonl      one captured row per line, verbatim except for redactions
//	dbt/...            the dbt project, filtered (see package files)
//	context/...        the agent's context or semantic layer, filtered
//
// The manifest is the first entry so a reader can decide what to do with a
// bundle without unpacking it.
package bundle

import (
	"encoding/json"
	"time"
)

// Format names this layout. Bump it on any change a reader must know about.
const Format = "benchouse-bundle/1"

// Record is one captured row.
//
// The fields mirror the Benchouse backend's raw record, so an upload feeds
// the same derivation as a live connection: RecordKind and SourceKind use the
// backend's identifiers, Payload is the row exactly as the source returned
// it, and NativeIDs carry the source's own keys.
type Record struct {
	SourceKind  string            `json:"source_kind"`
	RecordKind  string            `json:"record_kind"`
	NativeIDs   map[string]string `json:"native_ids,omitempty"`
	FetchMethod string            `json:"fetch_method"`
	CursorValue string            `json:"cursor_value,omitempty"`
	Payload     json.RawMessage   `json:"payload"`
}

// Manifest describes a bundle.
type Manifest struct {
	Format     string    `json:"format"`
	CLIVersion string    `json:"cli_version"`
	CreatedAt  time.Time `json:"created_at"`
	// Agent is the agent id the user picked, e.g. "nao" or "snowflake-cortex".
	Agent string `json:"agent"`
	// Email is who to send the findings to. Optional in a bundle written to
	// disk; required for an upload.
	Email string `json:"email,omitempty"`
	// Since is the start of the capture window, when one was set.
	Since *time.Time `json:"since,omitempty"`
	// Sources identify the agents the records came from, so an upload can be
	// matched to a later live connection for the same agent.
	Sources []Source `json:"sources,omitempty"`
	// Sample, when present, says which conversations were collected.
	Sample *Sample `json:"sample,omitempty"`
	// Records counts captured rows per record kind.
	Records map[string]int `json:"records"`
	// Queries is every statement the CLI ran against the source, in order.
	Queries []Query `json:"queries"`
	// Files lists every project file in the bundle.
	Files []File `json:"files"`
	// Redactions says what was removed or masked, and why.
	Redactions []string `json:"redactions"`
	// Skipped lists project files left out, with the reason.
	Skipped []Skip `json:"skipped,omitempty"`
}

// Source identifies one agent in the vendor's own terms. Nothing secret:
// an account locator, a project or space id.
type Source struct {
	SourceKind string `json:"source_kind"`
	// NativeID is the vendor's id for the agent: a nao project id, a Genie
	// space id, "*" for account-wide Cortex Analyst capture.
	NativeID string `json:"native_id,omitempty"`
	// Account is the vendor account or host the agent runs in.
	Account string `json:"account,omitempty"`
	Label   string `json:"label,omitempty"`
}

// Query is one statement run against the source.
type Query struct {
	Purpose   string `json:"purpose"`
	Statement string `json:"statement"`
	Rows      int    `json:"rows"`
	// Batches is how many times a batched statement ran (one batch per up
	// to 200 conversation ids); absent for a statement run once. Rows is
	// then the total over every batch.
	Batches int `json:"batches,omitempty"`
}

// Sample says how the conversations in a bundle were chosen: the most
// recent MaxConversations in the window (whichever limit is hit first).
type Sample struct {
	// MaxConversations is the cap; 0 means none was applied.
	MaxConversations int `json:"max_conversations"`
	// Conversations is how many were collected.
	Conversations int `json:"conversations"`
	// InWindow is how many the window held, when they were counted.
	InWindow *int64 `json:"in_window,omitempty"`
	// Since is the start of the window, when one was set.
	Since *time.Time `json:"since,omitempty"`
	// Order is how they were ranked: "most_recent".
	Order string `json:"order"`
}

// SampleMostRecent is the only Sample.Order this CLI writes.
const SampleMostRecent = "most_recent"

// File is one project file copied into the bundle.
type File struct {
	Path   string `json:"path"` // inside the bundle, e.g. dbt/models/orders.sql
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	// Masked counts values replaced by "<redacted>" in this file.
	Masked int `json:"masked,omitempty"`
}

// Skip is one project file left out.
type Skip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
