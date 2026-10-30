// Command benchouse reads an analytics agent's conversation history on your
// machine, packages it with your dbt project and the agent's context, and
// sends it to Benchouse for an accuracy review. Credentials never leave the
// machine; you can inspect everything before it is sent.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	_ "github.com/benchouse/benchouse-cli/internal/agents" // registers every agent
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/prompt"
	"github.com/benchouse/benchouse-cli/internal/telemetry"
	"golang.org/x/term"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `benchouse — send your analytics agent's conversations to Benchouse for review

Usage:
  benchouse                      guided upload (asks for everything; --plain for line-by-line)
  benchouse upload  [flags]      collect, review and upload
  benchouse collect [flags]      collect into a bundle file, upload nothing
  benchouse send <bundle>        show a bundle written by collect, confirm, upload
  benchouse inspect <bundle>     show what a bundle contains
  benchouse plan --agent <id>    print the queries that would run, connect to nothing
  benchouse agents [--json]      list supported agents
  benchouse version

Run "benchouse upload --agent <id> --help" for an agent's flags.

Credentials are read from flags, from BENCHOUSE_<FLAG> environment variables
(e.g. BENCHOUSE_PASSWORD, BENCHOUSE_TOKEN) or from a prompt. They are used to
read locally and are never written to the bundle or sent anywhere.

Docs: https://github.com/benchouse/benchouse-cli
`

// statsBudget bounds how long the exit waits for usage stats to be sent.
const statsBudget = time.Second

func main() {
	telemetry.Start(version)
	ctx, cancel := context.WithCancel(context.Background())
	restore := saveTerminal()
	go onInterrupt(cancel, restore)
	err := run(ctx, os.Args[1:])
	telemetry.Quit(quitHow(ctx, err))
	telemetry.Close(statsBudget)
	switch {
	case err == nil:
		return
	case errors.Is(err, errUsage):
		os.Exit(2)
	case ctx.Err() != nil || errors.Is(err, prompt.ErrInterrupted):
		restore()
		fmt.Fprintln(os.Stderr, "\ninterrupted: nothing was uploaded")
		os.Exit(130)
	}
	// Errors carry server bodies and database messages: clean them.
	fmt.Fprintln(os.Stderr, "error:", prompt.Clean(err.Error()))
	os.Exit(1)
}

// quitHow is how a run ended, for the usage stats: done, signal (ctx was
// cancelled by Ctrl-C or SIGTERM), ctrl_c (a prompt or the full-screen run
// was left with Ctrl-C) or error.
func quitHow(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return "done"
	case ctx.Err() != nil:
		return "signal"
	case errors.Is(err, prompt.ErrInterrupted):
		return "ctrl_c"
	}
	return "error"
}

// onInterrupt handles Ctrl-C and SIGTERM. The first signal cancels ctx, so
// a bundle being written stops and removes its temp files; if the command
// has not returned within two seconds (it may be blocked reading a prompt,
// which no context can interrupt), or on a second signal, the process exits.
// Either way the terminal is put back as it was: a signal during a hidden
// password prompt would otherwise leave echo switched off.
//
// While the full-screen guided run owns the terminal, Ctrl-C is a key, not a
// signal; a SIGTERM cancels ctx, which stops the run, and the run restores
// the screen itself and lets a bundle being written clean up. So the timer
// is longer, and a forced exit first leaves the alternate screen.
func onInterrupt(cancel context.CancelFunc, restore func()) {
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	s := <-sig
	if s == os.Interrupt {
		telemetry.Quit("ctrl_c")
	} else {
		telemetry.Quit("signal")
	}
	cancel()
	wait := 2 * time.Second
	if tuiActive.Load() {
		wait = 15 * time.Second
	}
	select {
	case <-sig:
	case <-time.After(wait):
	}
	if tuiActive.Load() {
		fmt.Fprint(os.Stderr, "\x1b[?1049l\x1b[?25h") // leave the alternate screen, show the cursor
	}
	restore()
	fmt.Fprintln(os.Stderr, "\ninterrupted: nothing was uploaded")
	telemetry.Close(statsBudget)
	os.Exit(130)
}

// tuiActive is set while the full-screen guided run owns the terminal.
var tuiActive atomic.Bool

// saveTerminal records the terminal's state and returns a function that
// puts it back. It does nothing when stdin is not a terminal.
func saveTerminal() func() {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return func() {}
	}
	state, err := term.GetState(fd)
	if err != nil {
		return func() {}
	}
	return func() { term.Restore(fd, state) }
}

var errUsage = errors.New("usage")

// command names the command for the usage stats; upload and collect
// report cli_started once their mode is known (see newJob).
var command string

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		command = "guided"
		return uploadCmd(ctx, nil)
	}
	switch args[0] {
	case "upload":
		command = "upload"
		return uploadCmd(ctx, args[1:])
	case "collect":
		command = "collect"
		return collect(ctx, args[1:])
	case "send":
		started("send", prompt.Interactive() && !hasFlag(args[1:], "yes", "no-input"))
		return send(ctx, args[1:])
	case "inspect":
		started("inspect", false)
		return inspect(args[1:])
	case "plan":
		started("plan", false)
		return planCmd(args[1:])
	case "agents":
		started("agents", false)
		return agents(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	}
	if strings.HasPrefix(args[0], "-") { // flags without a command mean upload
		command = "upload"
		return uploadCmd(ctx, args)
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q: %w", args[0], errUsage)
}

// started reports cli_started for a command in plain (prompting) or
// scripted mode.
func started(cmd string, prompting bool) {
	command = cmd
	if prompting {
		telemetry.SetMode("plain")
	} else {
		telemetry.SetMode("scripted")
	}
	telemetry.Started(cmd)
}

// hasFlag reports whether args hold any of the boolean flags names.
func hasFlag(args []string, names ...string) bool {
	for _, a := range args {
		for _, n := range names {
			if a == "-"+n || a == "--"+n || strings.HasPrefix(a, "--"+n+"=") || strings.HasPrefix(a, "-"+n+"=") {
				return true
			}
		}
	}
	return false
}

func agents(args []string) error {
	all := agent.All()
	if len(args) > 0 && args[0] == "--json" {
		type field struct {
			Flag     string   `json:"flag"`
			Env      string   `json:"env"`
			Label    string   `json:"label"`
			Secret   bool     `json:"secret,omitempty"`
			Optional bool     `json:"optional,omitempty"`
			Default  string   `json:"default,omitempty"`
			Choices  []string `json:"choices,omitempty"`
		}
		type entry struct {
			ID      string  `json:"id"`
			Name    string  `json:"name"`
			Status  string  `json:"status"`
			Summary string  `json:"summary"`
			Context string  `json:"context,omitempty"`
			Fields  []field `json:"fields"`
		}
		var out []entry
		for _, a := range all {
			e := entry{ID: a.ID, Name: a.Name, Status: string(a.Status), Summary: a.Summary}
			if a.Context != nil {
				e.Context = a.Context.Label
			}
			for _, f := range a.Fields {
				e.Fields = append(e.Fields, field{"--" + f.Key, f.EnvName(), f.Label, f.Secret, f.Optional, f.Default, f.Choices})
			}
			out = append(out, e)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	for _, a := range all {
		status := ""
		fmt.Printf("  %-22s %s%s\n  %-22s %s\n", a.ID, a.Name, status, "", a.Summary)
	}
	return nil
}

func inspect(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: benchouse inspect <bundle.tar.gz>")
	}
	s, err := bundle.Inspect(args[0])
	if err != nil {
		return err
	}
	printSummary(os.Stdout, s.Manifest, s.Bytes)
	return nil
}
