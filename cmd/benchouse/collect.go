package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/pipeline"
	"github.com/benchouse/benchouse-cli/internal/prompt"
	"github.com/benchouse/benchouse-cli/internal/telemetry"
	"github.com/benchouse/benchouse-cli/internal/upload"
)

// job is everything one collect or upload run needs.
type job struct {
	agent    agent.Agent
	inputs   agent.Inputs
	opts     agent.Options
	dbtDir   string
	ctxDir   string
	email    string
	out      string
	endpoint string
	yes      bool
	force    bool
	showPlan bool
	// askAnonymize: the guided run asks, because the flag was not given.
	askAnonymize bool
	// tui: the guided run is the full-screen one (see tui.go); preset is
	// the agent given with --agent, if any, and since the --since text.
	tui    bool
	preset *agent.Agent
	since  string
	// stats are the last bundle's counts, for the usage stats.
	stats telemetry.Collection
}

// common holds the flags every agent shares.
type common struct {
	agent, since, dbt, context, email, out, endpoint            string
	maxConversations                                            int
	results, users, anonymize, yes, force, showQueries, noInput bool
	plain                                                       bool // line-based prompts instead of the full-screen guided run
	anonymizeSet                                                bool // --anonymize-users given explicitly
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.agent, "agent", "", "agent to read (see: benchouse agents)")
	fs.StringVar(&c.since, "since", "90d", "capture window: 30d, 6m, 2026-01-01, or all")
	fs.IntVar(&c.maxConversations, "max-conversations", agent.DefaultMaxConversations, "collect at most this many conversations, the most recent in the window; 0 = no cap")
	fs.StringVar(&c.dbt, "dbt", "", "path to the dbt project folder (optional)")
	fs.StringVar(&c.context, "context", "", "path to the agent's context or semantic layer folder (optional)")
	fs.StringVar(&c.email, "email", "", "where to send the findings")
	fs.StringVar(&c.out, "out", "", "bundle file to write (default benchouse-<agent>-<date>.tar.gz)")
	fs.StringVar(&c.endpoint, "endpoint", upload.DefaultEndpoint, "upload endpoint")
	fs.BoolVar(&c.results, "include-results", false, "keep the rows the agent's queries returned (off: only columns and row counts)")
	fs.BoolVar(&c.users, "include-users", false, "include the agent's user directory (names, emails)")
	fs.BoolVar(&c.anonymize, "anonymize-users", true, "replace user ids, names, emails and IP addresses with pseudonyms; --anonymize-users=false keeps them (questions and answers are never changed)")
	fs.BoolVar(&c.yes, "yes", false, "upload without asking for confirmation")
	fs.BoolVar(&c.force, "force", false, "overwrite the --out file if it already exists")
	fs.BoolVar(&c.showQueries, "show-queries", false, "print every query before running it")
	fs.BoolVar(&c.noInput, "no-input", false, "never prompt; fail if something is missing (default when stdin is not a terminal)")
	fs.BoolVar(&c.plain, "plain", false, "ask line by line instead of the full-screen guided run (also BENCHOUSE_PLAIN=1)")
}

// parse reads common flags plus the chosen agent's fields. The agent is found
// first so its fields can be declared as flags.
func parse(name string, args []string) (*common, agent.Inputs, *agent.Agent, error) {
	agentID := peekAgent(args)
	var a *agent.Agent
	if agentID != "" {
		got, err := agent.Get(agentID)
		if err != nil {
			return nil, nil, nil, err
		}
		a = &got
	}

	fs := flag.NewFlagSet("benchouse "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := &common{}
	c.register(fs)
	in := agent.Inputs{}
	if a != nil {
		for _, f := range a.Fields {
			f := f
			help := f.Label
			if f.Help != "" {
				help += " — " + f.Help
			}
			if len(f.Choices) > 0 {
				help += " (" + strings.Join(f.Choices, "|") + ")"
			}
			if f.Secret {
				help += " [or $" + f.EnvName() + "]"
			}
			fs.Func(f.Key, help, func(v string) error { in[f.Key] = v; return nil })
		}
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printFlags(name, fs, a)
			return nil, nil, nil, errHelp
		}
		return nil, nil, nil, fmt.Errorf("%w (see: benchouse %s --help)", err, name)
	}
	if fs.NArg() > 0 {
		return nil, nil, nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	fs.Visit(func(f *flag.Flag) { c.anonymizeSet = c.anonymizeSet || f.Name == "anonymize-users" })
	if a != nil {
		for _, f := range a.Fields { // environment fills what flags did not
			if _, set := in[f.Key]; !set {
				if v, ok := os.LookupEnv(f.EnvName()); ok {
					in[f.Key] = v
				}
			}
		}
	}
	if c.email == "" {
		c.email = os.Getenv("BENCHOUSE_EMAIL")
	}
	return c, in, a, nil
}

var errHelp = errors.New("help")

func printFlags(name string, fs *flag.FlagSet, a *agent.Agent) {
	w := os.Stdout
	if a == nil {
		fmt.Fprintf(w, "Usage: benchouse %s --agent <id> [flags]\n\nAdd --agent <id> to see that agent's flags. Agents:\n", name)
		for _, x := range agent.All() {
			fmt.Fprintf(w, "  %s\n", x.ID)
		}
		fmt.Fprintln(w, "\nFlags:")
	} else {
		fmt.Fprintf(w, "Usage: benchouse %s --agent %s [flags]\n\n%s\n\nFlags:\n", name, a.ID, a.Setup)
	}
	fs.VisitAll(func(f *flag.Flag) {
		def := ""
		if f.DefValue != "" && f.DefValue != "false" {
			def = " (default " + f.DefValue + ")"
		}
		fmt.Fprintf(w, "  --%-18s %s%s\n", f.Name, f.Usage, def)
	})
}

func peekAgent(args []string) string {
	for i, a := range args {
		switch {
		case a == "--agent" || a == "-agent":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "--agent="):
			return strings.TrimPrefix(a, "--agent=")
		case strings.HasPrefix(a, "-agent="):
			return strings.TrimPrefix(a, "-agent=")
		}
	}
	return ""
}

// parseSince reads 30d, 6m, 1y, a date, or "all".
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "all" {
		return time.Time{}, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if len(s) >= 2 {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err == nil && n > 0 {
			switch s[len(s)-1] {
			case 'd':
				return now.AddDate(0, 0, -n), nil
			case 'w':
				return now.AddDate(0, 0, -7*n), nil
			case 'm':
				return now.AddDate(0, -n, 0), nil
			case 'y':
				return now.AddDate(-n, 0, 0), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("--since %q: use 30d, 6m, 1y, a date like 2026-01-01, or all", s)
}

// newJob turns parsed flags into a job, prompting for anything missing when
// prompting is allowed.
func newJob(ctx context.Context, name string, args []string, wantEmail bool) (*job, *prompt.Prompter, error) {
	c, in, a, err := parse(name, args)
	if err != nil {
		if !errors.Is(err, errHelp) {
			started(command, false)
		}
		return nil, nil, err
	}
	interactive := !c.noInput && prompt.Interactive()
	switch {
	case interactive && useTUI(c):
		telemetry.SetMode("tui")
	case interactive && !c.yes:
		telemetry.SetMode("plain")
	default:
		telemetry.SetMode("scripted")
	}
	telemetry.Started(command)
	if a != nil {
		telemetry.KnownInputs("cmd", a.Fields, in, c.email, c.dbt, c.context, c.out)
		telemetry.AgentSelected(a.ID)
	}
	var p *prompt.Prompter
	if interactive {
		p = prompt.New()
	}
	since, err := parseSince(c.since, time.Now())
	if err != nil {
		return nil, nil, err
	}
	if c.maxConversations < 0 {
		return nil, nil, fmt.Errorf("--max-conversations must be 0 (no cap) or more, not %d", c.maxConversations)
	}
	j := &job{askAnonymize: !c.anonymizeSet, inputs: in, dbtDir: c.dbt, ctxDir: c.context, email: strings.TrimSpace(c.email), out: c.out,
		endpoint: c.endpoint, yes: c.yes, force: c.force, showPlan: c.showQueries,
		opts: agent.Options{Since: since, IncludeResults: c.results, IncludeUsers: c.users, AnonymizeUsers: c.anonymize,
			MaxConversations: c.maxConversations}}

	if interactive && useTUI(c) {
		j.tui, j.preset, j.since = true, a, c.since
		return j, nil, nil
	}
	if a == nil {
		if !interactive {
			return nil, nil, fmt.Errorf("--agent is required (one of: %s)", agentIDs())
		}
		got, done, err := chooseAgent(ctx, p)
		if err != nil || done {
			return nil, nil, err
		}
		a = got
	}
	j.agent = *a

	if interactive {
		if err := wizard(ctx, p, j, wantEmail); err != nil {
			return nil, nil, err
		}
	} else {
		if err := j.agent.Validate(j.inputs); err != nil {
			telemetry.FieldFailed(j.agent.ID, telemetry.FieldOf(err), "validate", err)
			return nil, nil, err
		}
		if wantEmail && j.email == "" {
			return nil, nil, errors.New("--email is required to upload: it is where the findings are sent")
		}
	}
	if j.email != "" {
		addr, err := mail.ParseAddress(j.email)
		if err != nil {
			return nil, nil, fmt.Errorf("--email %q is not an email address", j.email)
		}
		j.email = addr.Address
	}
	if j.out == "" {
		j.out = fmt.Sprintf("benchouse-%s-%s.tar.gz", j.agent.ID, time.Now().Format("20060102-150405"))
	}
	return j, p, nil
}

func agentIDs() string {
	var ids []string
	for _, a := range agent.All() {
		ids = append(ids, a.ID)
	}
	return strings.Join(ids, ", ")
}

// build collects and writes the bundle, reporting progress on stderr.
func (j *job) build(ctx context.Context) (bundle.Manifest, int64, error) {
	if j.showPlan {
		printPlan(os.Stderr, j.agent.Plan(j.inputs, j.opts))
	}
	telemetry.KnownInputs("cmd", j.agent.Fields, j.inputs, j.email, j.dbtDir, j.ctxDir, j.out)
	m, size, err := j.buildWith(ctx, status)
	if err != nil {
		telemetry.CollectFailed(j.agent.ID, err)
	} else {
		telemetry.Collected(j.agent.ID, j.stats)
	}
	return m, size, err
}

// buildWith collects and writes the bundle, passing each progress line to
// progress (uncleaned: notes come from the source). It prints nothing, so
// the full-screen guided run can show the lines itself.
func (j *job) buildWith(ctx context.Context, progress func(string)) (bundle.Manifest, int64, error) {
	b, err := pipeline.Build(ctx, pipeline.Job{Agent: j.agent, Inputs: j.inputs, Opts: j.opts, DBTDir: j.dbtDir,
		CtxDir: j.ctxDir, Email: j.email, Out: j.out, Force: j.force, Version: version}, progress)
	j.stats = b.Stats
	return b.Manifest, b.Size, err
}

// view reports a step of the line-based run.
func (j *job) view(step string, uploading bool) {
	telemetry.View(j.agent.ID, step, j.agent.Context != nil, uploading)
}

func limitsText(o agent.Options) string { return pipeline.LimitsText(o) }

func collect(ctx context.Context, args []string) error {
	j, _, err := newJob(ctx, "collect", args, false)
	if err != nil {
		return quiet(err)
	}
	if j.tui {
		return runTUI(ctx, j, false)
	}
	m, size, err := j.build(ctx)
	if err != nil {
		return err
	}
	j.view("summary", false)
	printSummary(os.Stderr, m, size)
	j.view("done", false)
	telemetry.Saved(j.agent.ID)
	abs, _ := filepath.Abs(j.out)
	fmt.Fprintf(os.Stderr, "\nBundle written to %s\nInspect it, then send it with: benchouse send %s --email you@company.com\n", abs, j.out)
	fmt.Println(abs)
	return nil
}

func uploadCmd(ctx context.Context, args []string) error {
	j, p, err := newJob(ctx, "upload", args, true)
	if err != nil {
		return quiet(err)
	}
	if j.tui {
		return runTUI(ctx, j, true)
	}
	m, size, err := j.build(ctx)
	if err != nil {
		return err
	}
	j.view("summary", true)
	printSummary(os.Stderr, m, size)
	abs, _ := filepath.Abs(j.out)
	fmt.Fprintf(os.Stderr, "\nBundle: %s (inspect it with: tar -tzf %s)\n", abs, j.out)
	if !j.yes {
		if p == nil {
			return fmt.Errorf("not uploading without confirmation: pass --yes, or send it later with: benchouse send %s --email %s", j.out, j.email)
		}
		ok, err := p.Confirm(fmt.Sprintf("Upload %s to Benchouse? Findings go to %s", human(size), j.email), false)
		if err != nil {
			return err
		}
		if !ok {
			telemetry.Saved(j.agent.ID)
			fmt.Fprintf(os.Stderr, "Not uploaded. The bundle stays at %s; send it later with: benchouse send %s --email %s\n", abs, j.out, j.email)
			return nil
		}
	}
	return sendFile(ctx, j.endpoint, j.out, j.email, &j.agent)
}

func send(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("benchouse send", flag.ContinueOnError)
	email := fs.String("email", os.Getenv("BENCHOUSE_EMAIL"), "where to send the findings")
	endpoint := fs.String("endpoint", upload.DefaultEndpoint, "upload endpoint")
	yes := fs.Bool("yes", false, "upload without asking for confirmation")
	noInput := fs.Bool("no-input", false, "never prompt; requires --yes (default when stdin is not a terminal)")
	// Flags may come before or after the file.
	var file string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		rest = fs.Args()
		if len(rest) > 0 {
			if file != "" {
				return fmt.Errorf("usage: benchouse send <bundle> --email you@company.com [--yes]")
			}
			file, rest = rest[0], rest[1:]
		}
	}
	if file == "" || *email == "" {
		return fmt.Errorf("usage: benchouse send <bundle> --email you@company.com [--yes]")
	}
	addr, err := mail.ParseAddress(strings.TrimSpace(*email))
	if err != nil {
		return fmt.Errorf("--email %q is not an email address", *email)
	}
	s, err := bundle.Inspect(file)
	if err != nil {
		return fmt.Errorf("%s is not a valid bundle: %w", file, err)
	}
	st, err := os.Stat(file)
	if err != nil {
		return err
	}
	printSummary(os.Stderr, s.Manifest, st.Size())
	if err := confirmSend(*yes, *noInput, s.Manifest, st.Size(), file, addr.Address); err != nil {
		if errors.Is(err, errNotSent) {
			fmt.Fprintln(os.Stderr, "Not uploaded.")
			return nil
		}
		return err
	}
	var a *agent.Agent
	if got, err := agent.Get(s.Manifest.Agent); err == nil { // the bundle may come from anywhere: only known ids
		a = &got
	}
	return sendFile(ctx, *endpoint, file, addr.Address, a)
}

// errNotSent: the user declined; nothing failed.
var errNotSent = errors.New("not uploaded")

// confirmSend shows where the findings go and asks before uploading, unless
// --yes. Without a terminal (or with --no-input) --yes is required.
func confirmSend(yes, noInput bool, m bundle.Manifest, size int64, file, email string) error {
	if m.Email != "" && !strings.EqualFold(m.Email, email) {
		fmt.Fprintf(os.Stderr, "  %s\n", prompt.Clean("note: the bundle was collected for "+m.Email+"; findings will go to "+email))
	}
	if yes {
		return nil
	}
	if noInput || !prompt.Interactive() {
		return fmt.Errorf("not uploading without confirmation: review the summary above, then pass --yes")
	}
	ok, err := prompt.New().Confirm(fmt.Sprintf("Upload %s (%s) to Benchouse? Findings go to %s", file, human(size), email), false)
	if err != nil {
		return err
	}
	if !ok {
		return errNotSent
	}
	return nil
}

// sendFile uploads file; a is the bundle's agent, nil when unknown.
func sendFile(ctx context.Context, endpoint, file, email string, a *agent.Agent) error {
	id := "unknown"
	if a != nil {
		id = a.ID
		telemetry.View(id, "uploading", a.Context != nil, true)
	}
	status("Uploading...")
	r, err := upload.Send(ctx, endpoint, file, email, version)
	if err != nil {
		telemetry.UploadFailed(id, err)
		return err
	}
	telemetry.Uploaded(id, r.Bytes)
	if a != nil {
		telemetry.View(id, "done", a.Context != nil, true)
	}
	ref := prompt.Clean(r.ID) // from the server
	fmt.Fprintf(os.Stderr, "\nUploaded (%s, reference %s).\nAndré will email %s the top issues within a few days,\nand can walk you through the full findings in 30 minutes if you'd like.\n", human(r.Bytes), ref, email)
	fmt.Println(ref)
	return nil
}

func planCmd(args []string) error {
	c, in, a, err := parse("plan", args)
	if err != nil {
		return quiet(err)
	}
	if a == nil {
		return fmt.Errorf("--agent is required (one of: %s)", agentIDs())
	}
	since, err := parseSince(c.since, time.Now())
	if err != nil {
		return err
	}
	printPlan(os.Stdout, a.Plan(in, agent.Options{Since: since, IncludeResults: c.results, IncludeUsers: c.users,
		MaxConversations: c.maxConversations}))
	return nil
}

func quiet(err error) error {
	if errors.Is(err, errHelp) {
		return nil
	}
	return err
}

func status(s string) { fmt.Fprintln(os.Stderr, prompt.Dim(s)) } // Dim cleans s

func printPlan(w io.Writer, qs []bundle.Query) {
	w = prompt.NewWriter(w)
	fmt.Fprintf(w, "%d read-only queries:\n", len(qs))
	for i, q := range qs {
		fmt.Fprintf(w, "\n-- %d. %s\n%s;\n", i+1, q.Purpose, q.Statement)
	}
	fmt.Fprintln(w)
}

// printSummary shows what a bundle holds. Everything in it (file names,
// notes, redaction lines) may come from the user's systems or from a bundle
// someone else made, so it is cleaned of terminal escapes.
func printSummary(w io.Writer, m bundle.Manifest, size int64) {
	w = prompt.NewWriter(w)
	fmt.Fprintf(w, "\n%s\n", prompt.Bold("What's in the bundle"))
	summaryBody(w, m, size)
}

// summaryBody is printSummary without its heading. It writes the text as
// is: callers clean it (printSummary through prompt.NewWriter, the
// full-screen run with prompt.Clean).
func summaryBody(w io.Writer, m bundle.Manifest, size int64) {
	fmt.Fprintf(w, "  agent       %s\n", m.Agent)
	if m.Since != nil {
		fmt.Fprintf(w, "  since       %s\n", m.Since.Format("2006-01-02"))
	}
	if m.Sample != nil {
		fmt.Fprintf(w, "  sample      %s\n", sampleText(*m.Sample))
	}
	kinds := make([]string, 0, len(m.Records))
	total := 0
	for k, n := range m.Records {
		kinds = append(kinds, k)
		total += n
	}
	sort.Strings(kinds)
	fmt.Fprintf(w, "  records     %d\n", total)
	for _, k := range kinds {
		fmt.Fprintf(w, "    %-24s %d\n", k, m.Records[k])
	}
	byTop := map[string]int{}
	for _, f := range m.Files {
		top, _, _ := strings.Cut(f.Path, "/")
		byTop[top]++
	}
	for _, top := range []string{"dbt", "context"} {
		if byTop[top] > 0 {
			fmt.Fprintf(w, "  %-11s %s\n", top, plural(byTop[top], "file"))
		}
	}
	if len(m.Skipped) > 0 {
		fmt.Fprintf(w, "  left out    %s\n", plural(len(m.Skipped), "file"))
		for i, s := range m.Skipped {
			if i == 8 {
				fmt.Fprintf(w, "    ... and %d more\n", len(m.Skipped)-8)
				break
			}
			fmt.Fprintf(w, "    %s (%s)\n", s.Path, s.Reason)
		}
	}
	if len(m.Redactions) > 0 {
		fmt.Fprintf(w, "  removed\n")
		for _, r := range m.Redactions {
			fmt.Fprintf(w, "    - %s\n", r)
		}
	}
	fmt.Fprintf(w, "  queries run %d (listed in manifest.json)\n", len(m.Queries))
	if size > 0 {
		fmt.Fprintf(w, "  size        %s\n", human(size))
	}
}

// sampleText is "2,000 most recent conversations of 1,000,000 since
// 2026-08-01".
func sampleText(s bundle.Sample) string {
	noun := "conversations"
	if s.Conversations == 1 {
		noun = "conversation"
	}
	out := agent.Thousands(int64(s.Conversations)) + " "
	if s.MaxConversations > 0 {
		out += "most recent "
	}
	out += noun
	if s.InWindow != nil {
		out += " of " + agent.Thousands(*s.InWindow)
	}
	if s.Since != nil {
		out += " since " + s.Since.Format("2006-01-02")
	} else {
		out += ", all history"
	}
	if s.MaxConversations > 0 {
		out += " (cap " + agent.Thousands(int64(s.MaxConversations)) + ")"
	} else {
		out += " (no cap)"
	}
	return out
}

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
