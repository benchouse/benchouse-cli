package main

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/prompt"
	"github.com/benchouse/benchouse-cli/internal/telemetry"
	"github.com/benchouse/benchouse-cli/internal/upload"
)

const privacyURL = "https://benchouse.ai/privacy"

const other = "other"

// chooseAgent asks which agent to read. Picking "Other" records a request for
// that agent instead, and done is true.
func chooseAgent(ctx context.Context, p *prompt.Prompter) (*agent.Agent, bool, error) {
	p.Say("\n%s", prompt.Bold("Benchouse: find out where your analytics agent gets answers wrong"))
	p.Say("This reads your agent's conversations on this machine, shows you exactly what it")
	p.Say("collected, and uploads it only when you say so. Credentials never leave this machine.\n")

	var opts [][2]string
	for _, a := range agent.All() {
		label := a.Name
		opts = append(opts, [2]string{a.ID, label})
	}
	opts = append(opts, [2]string{other, "Other"})
	id, err := p.Choose("Which agent do you use?", opts, "")
	if err != nil {
		return nil, false, err
	}
	telemetry.AgentSelected(id)
	if id != other {
		a, err := agent.Get(id)
		return &a, false, err
	}
	telemetry.Reached("other")

	return nil, true, requestAgent(ctx, p, upload.Site())
}

// requestAgent is the line-based request-an-agent flow: the name, sent at
// once (a failure is not fatal), then an optional email linked to it.
func requestAgent(ctx context.Context, p *prompt.Prompter, site string) error {
	p.Say("\n%s", prompt.Bold("Step 1/2: request an agent"))
	var name string
	for {
		var err error
		if name, err = p.Ask("Which agent do you use?", "", ""); err != nil {
			return err
		}
		if n := len([]rune(name)); n > 0 && n <= upload.MaxAgentName {
			break
		}
		p.Say("  give its name, at most %d characters", upload.MaxAgentName)
	}
	telemetry.OtherRequested(len([]rune(name)))
	id, _ := upload.RequestAgentAnonymous(ctx, site, name) // not fatal: the email request names the agent too

	p.Say("\n%s", prompt.Bold("Step 2/2: your email (optional)"))
	var email string
	for {
		v, err := p.Ask("Your email", "we'll tell you when it's supported, typically within 72 hours; Enter to skip", "")
		if err != nil {
			return err
		}
		if v == "" {
			telemetry.OtherEmailGiven(false)
			p.Say("\nNo email — that's fine, we've noted the agent.")
			p.Say("Thanks. We've noted %s.", name)
			return nil
		}
		if addr, err := mail.ParseAddress(v); err == nil {
			email = addr.Address
			break
		}
		p.Say("  that doesn't look like an email address")
	}
	telemetry.OtherEmailGiven(true)
	if err := upload.RequestAgentEmail(ctx, site, name, email, id); err != nil {
		return fmt.Errorf("could not send the request (%w); email andre@benchouse.ai instead", err)
	}
	p.Say("\nThanks. We've noted %s.", name)
	p.Say("We'll email %s when it's supported, typically within 72 hours.", email)
	return nil
}

// wizard asks for whatever the flags did not supply, in three steps:
// conversations, dbt project, agent context.
func wizard(ctx context.Context, p *prompt.Prompter, j *job, wantEmail bool) error {
	a := j.agent
	steps := 2
	if a.Context != nil {
		steps = 3
	}

	j.view("conversations", wantEmail)
	p.Say("\n%s", prompt.Bold(fmt.Sprintf("Step 1/%d: %s conversations", steps, a.Name)))
	if a.Status != agent.Supported {
		p.Say(prompt.Dim(fmt.Sprintf("(%s: written from %s's published schema and not yet run against a live deployment; tell us if anything looks off)", a.Status, a.Name)))
	}
	p.Say("%s\n", a.Setup)
	if err := askConnection(ctx, p, a, j.inputs); err != nil {
		return err
	}
	if err := a.Validate(j.inputs); err != nil {
		telemetry.KnownInputs("cmd", a.Fields, j.inputs)
		telemetry.FieldFailed(a.ID, telemetry.FieldOf(err), "validate", err)
		return err
	}

	qs := a.Plan(j.inputs, j.opts)
	p.Say("\nThe CLI will run %d read-only queries. Results stay on this machine until you approve the upload.", len(qs))
	p.Say("It collects %s %s.", limitsText(j.opts), prompt.Dim("(change with --max-conversations and --since)"))
	p.Say(prompt.Dim("What we keep and why: " + privacyURL))
	if !j.opts.IncludeResults {
		p.Say(prompt.Dim("Rows your agent's queries returned are left out by default; only columns and row counts are kept."))
	}
	view, err := p.Confirm("View the queries?", false)
	if err != nil {
		return err
	}
	if view {
		printPlan(os.Stderr, qs)
	}

	if j.askAnonymize {
		keep, err := p.Confirm("Keep people's real names, emails and user ids? (No replaces them with pseudonyms; questions and answers stay as they are)", false)
		if err != nil {
			return err
		}
		j.opts.AnonymizeUsers = !keep
	}

	if j.dbtDir == "" {
		j.view("dbt", wantEmail)
		p.Say("\n%s", prompt.Bold(fmt.Sprintf("Step 2/%d: dbt project (optional)", steps)))
		p.Say("Models, sources, semantic models and docs show what your agent could have known.")
		p.Say(prompt.Dim("profiles.yml, .env files and keys are never read; secrets in YAML are masked."))
		if j.dbtDir, err = askDir(p, "Path to your dbt project (Enter to skip)"); err != nil {
			return err
		}
	}
	if a.Context != nil && j.ctxDir == "" {
		j.view("context", wantEmail)
		p.Say("\n%s", prompt.Bold(fmt.Sprintf("Step 3/%d: %s (optional)", steps, a.Context.Label)))
		p.Say(a.Context.Help)
		if j.ctxDir, err = askDir(p, "Path (Enter to skip)"); err != nil {
			return err
		}
	}

	if wantEmail && j.email == "" {
		p.Say("")
		if j.email, err = askEmail(p, "Your email", "André emails you the top issues within a few days"); err != nil {
			return err
		}
	}
	p.Say("")
	return nil
}

// askConnection asks for each connection field the flags did not supply.
// A field with a Verify is checked live once answered; when the check
// fails, the fields asked since the last check are asked again, each
// offering its answer as the default, so a wrong host can be corrected as
// well as a wrong token.
func askConnection(ctx context.Context, p *prompt.Prompter, a agent.Agent, in agent.Inputs) error {
	asked := map[string]bool{}
	groupStart := 0
	for i := 0; i < len(a.Fields); i++ {
		f := a.Fields[i]
		if !f.Applies(in) || (in.Get(f.Key) != "" && !asked[f.Key]) {
			continue
		}
		if err := askField(ctx, p, a, f, in); err != nil {
			return err
		}
		asked[f.Key] = true
		if f.Verify == nil {
			continue
		}
		p.Say(prompt.Dim("  checking…"))
		fact, err := f.Verify(ctx, in)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			telemetry.FieldVerified(a.ID, f.Key)
			if fact != "" {
				p.Say("  ✓ %s", prompt.Clean(fact))
			}
			groupStart = i + 1
			continue
		}
		telemetry.KnownInputs("cmd", a.Fields, in)
		telemetry.FieldFailed(a.ID, f.Key, "verify", err)
		p.Say("  ✗ %s", prompt.Clean(short(err)))
		p.Say(prompt.Dim("  correct it and try again (Enter keeps an answer)"))
		i = groupStart - 1
	}
	return nil
}

func askField(ctx context.Context, p *prompt.Prompter, a agent.Agent, f agent.Field, in agent.Inputs) error {
	def, prior := f.Default, in.Get(f.Key)
	if prior != "" && !f.Secret {
		def = prior // asked again after a failed check: keep the answer on Enter
	}
	for {
		var v string
		var err error
		switch {
		case f.Lookup != nil:
			opts, lerr := f.Lookup(ctx, in)
			switch {
			case lerr != nil:
				p.Say(prompt.Dim(fmt.Sprintf("could not list them (%v)", short(lerr))))
				v, err = p.Ask(f.Label, f.Help, def)
			case len(opts) == 1:
				p.Say("%s: %s", f.Label, opts[0][1])
				v = opts[0][0]
			case len(opts) > 1:
				v, err = p.Choose(f.Label, opts, def)
			default:
				v, err = p.Ask(f.Label, f.Help, def)
			}
		case len(f.Choices) > 0:
			opts := make([][2]string, len(f.Choices))
			for i, c := range f.Choices {
				opts[i] = [2]string{c, c}
			}
			v, err = p.Choose(f.Label, opts, def)
		case f.Secret:
			v, err = p.Secret(f.Label, f.Help)
		default:
			v, err = p.Ask(f.Label, f.Help, def)
		}
		if err != nil {
			return err
		}
		if f.Secret && strings.TrimSpace(v) == "" && prior != "" {
			v = prior // a secret is not echoed as a default, but Enter keeps it too
		}
		if strings.TrimSpace(v) == "" && !f.Optional {
			telemetry.FieldFailed(a.ID, f.Key, "validate", errors.New("this one is needed"))
			p.Say("  this one is needed")
			continue
		}
		if err := f.Check(v); err != nil {
			telemetry.KnownInputs("cmd", a.Fields, in, v)
			telemetry.FieldFailed(a.ID, f.Key, "validate", err)
			p.Say("  %s", err)
			continue
		}
		in[f.Key] = v
		return nil
	}
}

func askDir(p *prompt.Prompter, label string) (string, error) {
	for {
		v, err := p.Ask(label, "", "")
		if err != nil || v == "" {
			return "", err
		}
		v = expandHome(v)
		if st, err := os.Stat(v); err != nil || !st.IsDir() {
			p.Say("  %s is not a folder", v)
			continue
		}
		return v, nil
	}
}

func askEmail(p *prompt.Prompter, label, help string) (string, error) {
	for {
		v, err := p.Ask(label, help, "")
		if err != nil {
			return "", err
		}
		if addr, err := mail.ParseAddress(v); err == nil {
			return addr.Address, nil
		}
		p.Say("  that doesn't look like an email address")
		help = ""
	}
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}

func short(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}
