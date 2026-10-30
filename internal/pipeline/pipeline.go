// Package pipeline turns one agent's collection into a bundle on disk: the
// reader streams records through the pseudonymiser into a temp file, the
// project folders are masked and staged, and the bundle is written and read
// back. The CLI's collect, upload and guided runs all build through it, and
// so do the cross-agent leak tests (package leaktest), so what they check
// is what users get.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/anon"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/files"
	"github.com/benchouse/benchouse-cli/internal/telemetry"
)

// Job is one bundle to build.
type Job struct {
	Agent  agent.Agent
	Inputs agent.Inputs
	Opts   agent.Options
	// DBTDir and CtxDir are the project folders to include, or "".
	DBTDir, CtxDir string
	Email          string
	// Out is the bundle file; Force overwrites an existing one.
	Out   string
	Force bool
	// Version is the CLI version written to the manifest.
	Version string
}

// Built is what Build wrote.
type Built struct {
	// Manifest is the manifest read back from the written bundle.
	Manifest bundle.Manifest
	Size     int64
	// Stats are the bundle's counts, for the usage stats.
	Stats telemetry.Collection
}

// Build collects and writes the bundle, passing each progress line to
// progress (uncleaned: notes come from the source). It prints nothing.
func Build(ctx context.Context, j Job, progress func(string)) (Built, error) {
	start := time.Now()
	progress(fmt.Sprintf("Reading %s conversations (%s)...", j.Agent.Name, LimitsText(j.Opts)))
	if j.Opts.MaxConversations == 0 {
		progress("  warning: no --max-conversations cap: every conversation in the window is read, which can be slow and large")
	}
	if err := CheckDest(j.Out, j.Force); err != nil { // fail before reading anything
		return Built{}, err
	}
	recs, err := bundle.NewRecordsFor(j.Out)
	if err != nil {
		return Built{}, err
	}
	defer recs.Close()
	emit, err := Sink(ctx, recs, j.Opts, progress)
	if err != nil {
		return Built{}, err
	}
	res, err := j.Agent.Collect(ctx, j.Inputs, j.Opts, emit)
	if err != nil {
		return Built{}, err
	}
	for _, n := range res.Notes {
		progress("  note: " + n)
	}
	if j.Opts.AnonymizeUsers {
		res.Redactions = append(res.Redactions, anon.Redaction)
	}

	m := bundle.Manifest{CLIVersion: j.Version, CreatedAt: time.Now().UTC().Truncate(time.Second),
		Agent: j.Agent.ID, Email: j.Email, Sources: res.Sources, Queries: res.Queries, Redactions: res.Redactions}
	if !j.Opts.Since.IsZero() {
		s := j.Opts.Since.UTC()
		m.Since = &s
	}
	if res.Sampled {
		m.Sample = &bundle.Sample{MaxConversations: j.Opts.MaxConversations, Conversations: res.Conversations,
			InWindow: res.InWindow, Since: m.Since, Order: bundle.SampleMostRecent}
	}
	// Project files are masked one at a time and staged in a temp file
	// beside the bundle; only their paths, sizes and digests stay in memory.
	staged, err := bundle.NewStagingFor(j.Out)
	if err != nil {
		return Built{Manifest: m}, err
	}
	defer staged.Close()
	addFolder := func(dir, prefix string, keep func(string) bool) error {
		if dir == "" {
			return nil
		}
		progress(fmt.Sprintf("Reading %s...", dir))
		start := staged.Len()
		masked := 0
		skipped, err := files.Walk(dir, keep, func(f files.File) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := staged.Add(prefix+"/"+f.Rel, f.Data, f.Masked); err != nil {
				return err
			}
			masked += f.Masked
			return nil
		})
		if err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		if staged.Len() == start {
			return fmt.Errorf("%s: nothing to include (no matching files)", dir)
		}
		staged.SortFrom(start)
		for _, s := range skipped {
			m.Skipped = append(m.Skipped, bundle.Skip{Path: prefix + "/" + s.Rel, Reason: s.Reason})
		}
		if masked > 0 {
			m.Redactions = append(m.Redactions, fmt.Sprintf("%s/: %d secret values (passwords, tokens, keys, URL credentials) replaced with [redacted]", prefix, masked))
		}
		return nil
	}
	if err := addFolder(j.DBTDir, "dbt", files.DBTKeep); err != nil {
		return Built{Manifest: m}, err
	}
	if j.Agent.Context != nil {
		if err := addFolder(j.CtxDir, "context", j.Agent.Context.Keeps(j.Opts)); err != nil {
			return Built{Manifest: m}, err
		}
	}
	m.Files = staged.Files()
	if err := bundle.Write(ctx, j.Out, j.Force, m, recs, staged); err != nil {
		return Built{Manifest: m}, err
	}
	s, err := bundle.Inspect(j.Out) // re-read what was written: the summary shows the file, not intentions
	if err != nil {
		return Built{Manifest: m}, fmt.Errorf("the bundle failed its own check: %w", err)
	}
	st, err := os.Stat(j.Out)
	if err != nil {
		return Built{Manifest: m}, err
	}
	return Built{Manifest: s.Manifest, Size: st.Size(),
		Stats: CollectionOf(s.Manifest, res.Conversations, st.Size(), j.Opts, time.Since(start))}, nil
}

// CollectionOf is a bundle's counts, for the usage stats.
func CollectionOf(m bundle.Manifest, conversations int, size int64, o agent.Options, d time.Duration) telemetry.Collection {
	c := telemetry.Collection{Conversations: conversations, Sampled: m.Sample != nil, Size: size, Duration: d,
		IncludeResults: o.IncludeResults, IncludeUsers: o.IncludeUsers, Anonymize: o.AnonymizeUsers}
	for _, n := range m.Records {
		c.Records += n
	}
	for _, f := range m.Files {
		switch top, _, _ := strings.Cut(f.Path, "/"); top {
		case "dbt":
			c.FilesDBT++
		case "context":
			c.FilesContext++
		}
	}
	return c
}

// Sink returns the Emit a reader streams into: each record is pseudonymised
// when asked, then appended to the records temp file. Nothing is kept in
// memory. Progress is reported every 10,000 records.
func Sink(ctx context.Context, recs *bundle.Records, opts agent.Options, progress func(string)) (agent.Emit, error) {
	var h *anon.Hasher
	if opts.AnonymizeUsers {
		var err error
		if h, err = anon.Load(); err != nil {
			return nil, fmt.Errorf("anonymize: %w", err)
		}
	}
	return func(r bundle.Record) error {
		if recs.Len()%1024 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		if h != nil {
			var err error
			if r, err = h.Record(r); err != nil {
				return fmt.Errorf("anonymize: %w", err)
			}
		}
		if err := recs.Add(r); err != nil {
			if errors.Is(err, bundle.ErrTooManyRecords) {
				return fmt.Errorf("%w (after %s records): lower --max-conversations or narrow --since", err, agent.Thousands(int64(recs.Len())))
			}
			return err
		}
		if n := recs.Len(); n%10_000 == 0 {
			progress(fmt.Sprintf("  %s records so far", agent.Thousands(int64(n))))
		}
		return nil
	}, nil
}

// LimitsText says what will be collected: "the 2,000 most recent
// conversations since 2026-08-01".
func LimitsText(o agent.Options) string {
	s := "every conversation"
	if o.MaxConversations > 0 {
		s = "the " + agent.Thousands(int64(o.MaxConversations)) + " most recent conversations"
	}
	if o.Since.IsZero() {
		return s + ", all history"
	}
	return s + " since " + o.Since.Format("2006-01-02")
}

// CheckDest refuses an existing --out before anything is read, so a long
// collection does not end in "already exists".
func CheckDest(dst string, force bool) error {
	if force {
		return nil
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s %w; pass --force to overwrite it, or choose another --out", dst, bundle.ErrExists)
	}
	return nil
}
