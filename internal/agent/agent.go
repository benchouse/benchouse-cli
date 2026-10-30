// Package agent describes the analytics agents the CLI can read.
//
// An agent is data plus two functions: the inputs it needs (which drive both
// the command-line flags and the interactive prompts), the statements it will
// run, and how to run them. Adding an agent means adding one package that
// registers one Agent; nothing else in the CLI changes.
package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/bundle"
)

// Status says how much an agent's reader has been exercised.
type Status string

const (
	// Supported: run against a real deployment of the agent.
	Supported Status = "supported"
	// Experimental: written from the agent's published schema or API
	// documentation, tested against fixtures only.
	Experimental Status = "experimental"
)

// Field is one input an agent needs.
type Field struct {
	// Key names the input. It becomes the flag --<key> and the environment
	// variable BENCHOUSE_<KEY> (uppercased, dashes as underscores).
	Key string
	// Label is the prompt text.
	Label string
	// Help is one line under the prompt and in --help.
	Help string
	// Secret inputs are read without echo and never written anywhere.
	Secret bool
	// Optional inputs may be left empty.
	Optional bool
	// Default is used when the input is left empty.
	Default string
	// Choices restricts the value to one of these.
	Choices []string
	// When, if set, says whether the field applies given the inputs so far.
	When func(Inputs) bool
	// Lookup, if set, lists the values the source offers (value, label), so
	// the interactive prompt can show a pick list instead of a text box.
	Lookup func(ctx context.Context, in Inputs) ([][2]string, error)
	// Validate, if set, checks a non-empty value's format, offline and
	// instantly: as the user types or leaves the field, after a line-based
	// answer, and in Agent.Validate for flags and environment variables. The
	// error says what is expected; it must not echo a secret.
	Validate func(v string) error
	// Verify, if set, is a live check run when the user leaves this field
	// in a guided run, with the inputs answered so far: the token works, the
	// warehouse exists. fact is one short line to show ("Token works:
	// signed in as ana@example.com"), or "" for nothing to show. An error
	// keeps the user on the field. It must never contain a secret. Verify
	// is not run without prompts: Agent.Check covers the same ground, and
	// should share the code so the two cannot disagree.
	Verify func(ctx context.Context, in Inputs) (fact string, err error)
}

// EnvName is the environment variable a field can be supplied through.
func (f Field) EnvName() string {
	return "BENCHOUSE_" + strings.ToUpper(strings.ReplaceAll(f.Key, "-", "_"))
}

// Check runs the field's Validate on a non-empty value; nil without one.
func (f Field) Check(v string) error {
	if f.Validate == nil || strings.TrimSpace(v) == "" {
		return nil
	}
	return f.Validate(strings.TrimSpace(v))
}

// Applies reports whether the field is asked for given in.
func (f Field) Applies(in Inputs) bool { return f.When == nil || f.When(in) }

// Inputs are the values the user supplied, by field key.
type Inputs map[string]string

// Get returns the value for key, or "".
func (in Inputs) Get(key string) string { return strings.TrimSpace(in[key]) }

// Options shape what is collected, whatever the agent.
type Options struct {
	// Since bounds the capture window. Zero means everything the source holds.
	Since time.Time
	// IncludeResults keeps the rows the agent's queries returned. Off by
	// default: the findings need the question, the SQL and the answer, not
	// the customer's data.
	IncludeResults bool
	// IncludeUsers keeps the agent's user directory (names and emails).
	// Off by default.
	IncludeUsers bool
	// AnonymizeUsers replaces user ids, names and emails with pseudonyms
	// as records are emitted (see package anon). Readers need not handle it.
	AnonymizeUsers bool
	// MaxConversations caps how many conversations are collected: the most
	// recent ones in the window, whichever limit is hit first. Zero means no
	// cap. Readers choose the conversations first and fetch only their
	// children (messages, parts, spans), in batches of BatchSize ids.
	MaxConversations int
}

// DefaultMaxConversations is the --max-conversations default.
const DefaultMaxConversations = 2000

// BatchSize is how many conversation ids one child fetch names, so every
// statement against the customer's database is bounded.
const BatchSize = 200

// Emit receives one collected record. Readers call it as rows arrive and
// keep nothing: the CLI writes each record to disk before the next is read.
// An error from Emit stops the collection and is returned by Collect.
type Emit func(bundle.Record) error

// CollectFunc reads the conversations, emitting each record.
type CollectFunc func(ctx context.Context, in Inputs, opts Options, emit Emit) (Result, error)

// Result is what a collection produced, besides the records it emitted.
type Result struct {
	Queries    []bundle.Query
	Redactions []string
	// Sources identify the agents read, in the vendor's own terms.
	Sources []bundle.Source
	// Notes are things the user should know, e.g. an optional source that
	// could not be read.
	Notes []string
	// Sampled says the reader applied Options.MaxConversations; only then
	// does the manifest carry a sample object.
	Sampled bool
	// Conversations is how many conversations were collected.
	Conversations int
	// InWindow is how many conversations the window holds, when the reader
	// counted them; nil when unknown.
	InWindow *int64
}

// Tally records one run of a batched statement: the first run adds the
// query, later runs of the same purpose and statement add a batch and rows.
func (r *Result) Tally(purpose, stmt string, rows int) {
	for i := range r.Queries {
		q := &r.Queries[i]
		if q.Purpose == purpose && q.Statement == stmt && q.Batches > 0 {
			q.Batches++
			q.Rows += rows
			return
		}
	}
	r.Queries = append(r.Queries, bundle.Query{Purpose: purpose, Statement: stmt, Rows: rows, Batches: 1})
}

// Collected is a Result with its records held in memory: what CollectAll
// returns, for tests.
type Collected struct {
	Result
	Records []bundle.Record
}

// CollectAll runs collect and keeps every record it emits. For tests and
// small sources only: the CLI never holds a collection in memory.
func CollectAll(ctx context.Context, collect CollectFunc, in Inputs, opts Options) (Collected, error) {
	var c Collected
	res, err := collect(ctx, in, opts, func(r bundle.Record) error {
		c.Records = append(c.Records, r)
		return nil
	})
	c.Result = res
	return c, err
}

// CheckResult is what a check found, for the user to read before moving on.
type CheckResult struct {
	// Facts are short findings, e.g. "Connected to Postgres 16 at db:5432 as
	// benchouse_reader" or "1,204 chats in the last 90 days".
	Facts []string
	// Warnings are problems that do not stop the run, e.g. "no compiled
	// manifest: run dbt compile for richer findings".
	Warnings []string
	// Conversations is how many conversations the window holds, when the
	// check counted them; the guided run shows it beside the
	// --max-conversations choice.
	Conversations *int64
}

// Context describes the folder an agent's context or semantic layer lives in.
type Context struct {
	// Label names it in prompts, e.g. "nao context folder".
	Label string
	// Help says what to point at.
	Help string
	// Check, if set, verifies dir looks like this agent's context (for nao:
	// RULES.md, databases/) and says what it holds. An error means it does
	// not, with a message saying what was expected.
	Check func(dir string) (CheckResult, error)
	// Keep reports whether a path relative to the folder belongs in the
	// bundle. Credentials files must return false.
	Keep func(rel string) bool
	// KeepWith, if set, is used instead of Keep when the answer depends on
	// the collection options, e.g. a file of sample rows that only belongs
	// with IncludeResults.
	KeepWith func(rel string, opts Options) bool
}

// Keeps returns the keep rule to apply for opts.
func (c *Context) Keeps(opts Options) func(rel string) bool {
	if c.KeepWith != nil {
		return func(rel string) bool { return c.KeepWith(rel, opts) }
	}
	return c.Keep
}

// Agent is one supported analytics agent.
type Agent struct {
	ID      string
	Name    string
	Summary string
	Status  Status
	// DefaultMaxConversations replaces the global default cap for this
	// agent when the user did not choose one: a reader limited by a slow API
	// sets it so a default run still takes minutes, not hours. 0 keeps the
	// global default.
	DefaultMaxConversations int
	// Fields are the connection inputs for reading conversations.
	Fields []Field
	// Context is the agent's context folder, or nil if it has none.
	Context *Context
	// Setup is shown before the connection prompts: what access is needed
	// and the least-privilege way to grant it.
	Setup string
	// Plan returns the statements Collect would run, for review before
	// anything connects.
	Plan func(in Inputs, opts Options) []bundle.Query
	// Check, if set, connects with in and verifies everything Collect needs
	// is reachable and readable, cheaply (LIMIT 0 / counts / a single API
	// call), without collecting. It runs right after the user finishes the
	// connection step. An error is shown with the inputs so they can be
	// corrected; it must never contain a secret.
	Check func(ctx context.Context, in Inputs, opts Options) (CheckResult, error)
	// Collect reads the conversations, emitting each record as it is read.
	Collect CollectFunc
}

var registry = map[string]Agent{}

// Register adds an agent. Agent packages call it from init.
//
// Every Secret field gets a sibling --<key>-file (see SecretFileSuffix), so
// a token or password can be read from a file instead of appearing in the
// process list.
func Register(a Agent) {
	if _, dup := registry[a.ID]; dup {
		panic("agent registered twice: " + a.ID)
	}
	a.Fields = withSecretFiles(a.Fields)
	registry[a.ID] = a
}

// SecretFileSuffix names the input that reads a Secret field from a file:
// --token-file for --token, BENCHOUSE_TOKEN_FILE for BENCHOUSE_TOKEN.
const SecretFileSuffix = "-file"

// maxSecretFile bounds a secret file: a PEM key is a few KiB.
const maxSecretFile = 64 << 10

// withSecretFiles adds a <key>-file field after each Secret field. The file
// field is never prompted for (the prompt asks for the secret itself); it is
// a flag and an environment variable only. The secret is not asked for when
// its file was given, and Lookups see the secret read from its file.
func withSecretFiles(fields []Field) []Field {
	keys := map[string]bool{}
	for _, f := range fields {
		keys[f.Key] = true
	}
	var out []Field
	for _, f := range fields {
		fileKey := f.Key + SecretFileSuffix
		if !f.Secret || keys[fileKey] {
			out = append(out, f)
			continue
		}
		when := f.When
		f.When = func(in Inputs) bool { return (when == nil || when(in)) && in.Get(fileKey) == "" }
		out = append(out, f, Field{
			Key:      fileKey,
			Label:    f.Label + " file",
			Help:     "read " + strings.ToLower(f.Label) + " from this file instead, so it stays out of the process list",
			Optional: true,
			When:     func(Inputs) bool { return false },
			Validate: ExistingFile,
		})
	}
	for i, f := range out {
		if lookup := f.Lookup; lookup != nil {
			out[i].Lookup = func(ctx context.Context, in Inputs) ([][2]string, error) {
				if err := ResolveSecretFiles(in, out); err != nil {
					return nil, err
				}
				return lookup(ctx, in)
			}
		}
		if verify := f.Verify; verify != nil {
			out[i].Verify = func(ctx context.Context, in Inputs) (string, error) {
				if err := ResolveSecretFiles(in, out); err != nil {
					return "", err
				}
				return verify(ctx, in)
			}
		}
	}
	return out
}

// ResolveSecretFiles fills each Secret field left empty from the file its
// <key>-file input names: the file's contents up to 64 KiB, trailing newline
// removed. A secret given directly wins over its file.
func ResolveSecretFiles(in Inputs, fields []Field) error {
	for _, f := range fields {
		if !f.Secret || in.Get(f.Key) != "" {
			continue
		}
		p := in.Get(f.Key + SecretFileSuffix)
		if p == "" {
			continue
		}
		v, err := readSecretFile(p)
		if err != nil {
			return fmt.Errorf("--%s%s: %w", f.Key, SecretFileSuffix, err)
		}
		in[f.Key] = v
	}
	return nil
}

func readSecretFile(p string) (string, error) {
	fh, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, maxSecretFile+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxSecretFile {
		return "", fmt.Errorf("%s is over %d KiB: not a secret", p, maxSecretFile>>10)
	}
	v := strings.TrimRight(string(b), "\r\n")
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("%s is empty", p)
	}
	return v, nil
}

// CapFor is the conversation cap to use for a: the user's choice when set,
// else the agent's own default, else the global one.
func CapFor(a Agent, chosen int, set bool) int {
	if set || a.DefaultMaxConversations <= 0 {
		return chosen
	}
	return a.DefaultMaxConversations
}

// All returns every agent alphabetically by name.
func All() []Agent {
	out := make([]Agent, 0, len(registry))
	for _, a := range registry {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// Get returns the agent with this id.
func Get(id string) (Agent, error) {
	a, ok := registry[strings.ToLower(strings.TrimSpace(id))]
	if !ok {
		ids := make([]string, 0, len(registry))
		for k := range registry {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		return Agent{}, fmt.Errorf("unknown agent %q (supported: %s)", id, strings.Join(ids, ", "))
	}
	return a, nil
}

// Validate reads secrets given as files, then checks that every applicable
// required field has a value, that choices are respected and that each
// value passes its field's Validate.
func (a Agent) Validate(in Inputs) error {
	if err := ResolveSecretFiles(in, a.Fields); err != nil {
		return err
	}
	var missing []string
	for _, f := range a.Fields {
		if !f.Applies(in) {
			continue
		}
		v := in.Get(f.Key)
		if v == "" && f.Default != "" {
			in[f.Key] = f.Default
			v = f.Default
		}
		if v == "" {
			if !f.Optional {
				missing = append(missing, "--"+f.Key)
			}
			continue
		}
		if len(f.Choices) > 0 && !contains(f.Choices, v) {
			return fmt.Errorf("--%s must be one of %s, not %q", f.Key, strings.Join(f.Choices, ", "), v)
		}
		if err := f.Check(v); err != nil {
			return fmt.Errorf("--%s: %w", f.Key, err)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s needs %s", a.Name, strings.Join(missing, ", "))
	}
	return nil
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
