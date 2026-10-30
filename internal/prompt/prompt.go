// Package prompt asks questions on a terminal.
package prompt

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// Prompter reads answers from in and writes questions to out.
type Prompter struct {
	in  *bufio.Reader
	out io.Writer
	fd  int // terminal fd for secret input, or -1
	// tty receives the menu's own cursor-control sequences unfiltered; it is
	// nil unless stdin and stderr are both terminals. Text written to it is
	// always passed through Clean first.
	tty io.Writer
}

// New returns a Prompter on stdin/stderr. Questions go to stderr so stdout
// stays clean for piping.
func New() *Prompter {
	fd := -1
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fd = int(os.Stdin.Fd())
	}
	// Everything shown goes through a cleaning writer: labels, option texts
	// and messages carry names from the user's systems (project names,
	// errors from servers) that must not reach the terminal as escapes.
	p := &Prompter{in: bufio.NewReader(os.Stdin), out: NewWriter(os.Stderr), fd: fd}
	if fd >= 0 && term.IsTerminal(int(os.Stderr.Fd())) && os.Getenv("TERM") != "dumb" {
		p.tty = os.Stderr
	}
	return p
}

// NewFrom returns a Prompter reading answers from in and writing to out,
// with no terminal: for tests and scripted input.
func NewFrom(in io.Reader, out io.Writer) *Prompter {
	return &Prompter{in: bufio.NewReader(in), out: NewWriter(out), fd: -1}
}

// Interactive reports whether stdin is a terminal.
func Interactive() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// Say prints a line.
func (p *Prompter) Say(format string, a ...any) { fmt.Fprintf(p.out, format+"\n", a...) }

// Ask asks for a line of text. An empty answer returns def.
func (p *Prompter) Ask(label, help, def string) (string, error) {
	if help != "" {
		fmt.Fprintf(p.out, "  %s\n", dim(help))
	}
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", bold(label), def)
	} else {
		fmt.Fprintf(p.out, "%s: ", bold(label))
	}
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// Secret asks for a value without echoing it.
func (p *Prompter) Secret(label, help string) (string, error) {
	if help != "" {
		fmt.Fprintf(p.out, "  %s\n", dim(help))
	}
	fmt.Fprintf(p.out, "%s: ", bold(label))
	if p.fd < 0 {
		line, err := p.in.ReadString('\n')
		return strings.TrimSpace(line), err
	}
	b, err := term.ReadPassword(p.fd)
	fmt.Fprintln(p.out)
	return strings.TrimSpace(string(b)), err
}

// Choose asks the user to pick one of options (value, label): with the
// arrow keys on a terminal, by number otherwise. def is preselected.
func (p *Prompter) Choose(label string, options [][2]string, def string) (string, error) {
	if p.tty != nil && len(options) > 0 {
		return p.menu(label, options, def)
	}
	fmt.Fprintf(p.out, "%s\n", bold(label))
	defN := 0
	for i, o := range options {
		marker := " "
		if o[0] == def {
			marker, defN = "*", i+1
		}
		fmt.Fprintf(p.out, "  %s %d) %s\n", marker, i+1, o[1])
	}
	for {
		prompt := "Choose a number"
		if defN > 0 {
			prompt += fmt.Sprintf(" [%d]", defN)
		}
		fmt.Fprintf(p.out, "%s: ", prompt)
		line, err := p.in.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		line = strings.TrimSpace(line)
		if line == "" && defN > 0 {
			return options[defN-1][0], nil
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
			return options[n-1][0], nil
		}
		for _, o := range options { // accept the value typed out, too
			if strings.EqualFold(line, o[0]) {
				return o[0], nil
			}
		}
		fmt.Fprintf(p.out, "  enter a number from 1 to %d\n", len(options))
	}
}

// Confirm asks a yes/no question.
func (p *Prompter) Confirm(label string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Fprintf(p.out, "%s [%s]: ", bold(label), hint)
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

var color = os.Getenv("NO_COLOR") == "" && term.IsTerminal(int(os.Stderr.Fd()))

func bold(s string) string {
	if !color {
		return Clean(s)
	}
	return "\x1b[1m" + Clean(s) + "\x1b[0m"
}

func dim(s string) string {
	if !color {
		return Clean(s)
	}
	return "\x1b[2m" + Clean(s) + "\x1b[0m"
}

// Bold and Dim style text for callers.
func Bold(s string) string { return bold(s) }
func Dim(s string) string  { return dim(s) }
