package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/files"
	"github.com/benchouse/benchouse-cli/internal/prompt"
	"github.com/benchouse/benchouse-cli/internal/tui"
	"github.com/benchouse/benchouse-cli/internal/upload"
	"golang.org/x/term"
)

// checkDBT verifies a dbt project folder in the guided run.
var checkDBT = files.CheckDBT

// useTUI reports whether the guided run is the full-screen one. It falls
// back to line-based prompts with --plain, BENCHOUSE_PLAIN=1, TERM=dumb,
// --yes (a scripted run), or when stdout or stderr is not a terminal.
func useTUI(c *common) bool {
	if c.plain || c.yes || os.Getenv("BENCHOUSE_PLAIN") == "1" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// runTUI runs the full-screen guided run for j, then prints what happened
// to stderr, so it stays in the scrollback, and the upload reference (or,
// for collect, the bundle path) to stdout as the line-based run does.
func runTUI(ctx context.Context, j *job, uploading bool) error {
	cfg := tui.Config{
		Upload: uploading, Agents: agent.All(), Agent: j.preset, Inputs: j.inputs, Opts: j.opts,
		DBTDir: j.dbtDir, CtxDir: j.ctxDir, Email: j.email, Since: j.since, CapSet: j.capSet,
		CheckDBT: checkDBT,
		Collect: func(ctx context.Context, s tui.Selection, progress func(string)) (tui.Bundle, error) {
			b := *j
			b.agent, b.inputs, b.opts = s.Agent, s.Inputs, s.Opts
			b.dbtDir, b.ctxDir, b.email = s.DBTDir, s.CtxDir, s.Email
			if b.out == "" {
				b.out = fmt.Sprintf("benchouse-%s-%s.tar.gz", s.Agent.ID, time.Now().Format("20060102-150405"))
			}
			m, size, err := b.buildWith(ctx, progress)
			if err != nil {
				return tui.Bundle{}, err
			}
			abs, _ := filepath.Abs(b.out)
			var sum bytes.Buffer
			summaryBody(&sum, m, size)
			return tui.Bundle{Path: abs, Rel: b.out, Size: size, Summary: sum.String(), Stats: b.stats}, nil
		},
		Send: func(ctx context.Context, path, email string) (upload.Receipt, error) {
			return upload.Send(ctx, j.endpoint, path, email, version)
		},
		RequestAgentAnonymous: func(ctx context.Context, name string) (string, error) {
			return upload.RequestAgentAnonymous(ctx, upload.Site(), name)
		},
		RequestAgentEmail: func(ctx context.Context, name, email, id string) error {
			return upload.RequestAgentEmail(ctx, upload.Site(), name, email, id)
		},
	}
	tuiActive.Store(true)
	o, err := tui.Run(ctx, cfg)
	tuiActive.Store(false)
	if err != nil {
		return err
	}

	w := prompt.NewWriter(os.Stderr)
	switch o.Result {
	case tui.Requested:
		fmt.Fprintf(w, "Thanks. We've noted %s.\n", o.RequestedAgent)
		if o.Email != "" {
			fmt.Fprintf(w, "We'll email %s when it's supported, typically within 72 hours.\n", o.Email)
		}
		return nil
	case tui.Uploaded:
		id := prompt.Clean(o.Receipt.ID) // from the server
		fmt.Fprintf(w, "Uploaded (%s, reference %s).\nAndré will email %s the top issues within a few days,\nand can walk you through the full findings in 30 minutes if you'd like.\nThe bundle stays at %s\n",
			human(o.Receipt.Bytes), id, o.Email, o.Bundle.Path)
		fmt.Println(id)
		return nil
	case tui.Saved:
		email := o.Email
		if email == "" {
			email = "you@company.com"
		}
		fmt.Fprintf(w, "Bundle written to %s\nInspect it with: benchouse inspect %s\nSend it with: benchouse send %s --email %s\n",
			o.Bundle.Path, o.Bundle.Rel, o.Bundle.Rel, email)
		if o.UploadErr != nil {
			return o.UploadErr
		}
		if !uploading {
			fmt.Println(o.Bundle.Path)
		}
		return nil
	}
	if o.Bundle != nil {
		fmt.Fprintf(w, "The bundle stays at %s (not uploaded)\n", o.Bundle.Path)
	}
	return prompt.ErrInterrupted
}
