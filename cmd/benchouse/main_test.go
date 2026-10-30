package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/bundle"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"all":        {},
		"":           {},
		"30d":        now.AddDate(0, 0, -30),
		"2w":         now.AddDate(0, 0, -14),
		"6m":         now.AddDate(0, -6, 0),
		"1y":         now.AddDate(-1, 0, 0),
		"2026-01-15": time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"0d", "yesterday", "5x"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("parseSince(%q) accepted", bad)
		}
	}
}

func TestPeekAgent(t *testing.T) {
	for want, args := range map[string][]string{
		"nao":              {"--agent", "nao", "--yes"},
		"snowflake-cortex": {"--yes", "--agent=snowflake-cortex"},
		"":                 {"--yes"},
	} {
		if got := peekAgent(args); got != want {
			t.Errorf("peekAgent(%v) = %q", args, got)
		}
	}
}

func TestSendNeedsYesWithoutATerminal(t *testing.T) {
	m := bundle.Manifest{Email: "a@b.co"}
	if err := confirmSend(false, true, m, 10, "b.tar.gz", "a@b.co"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("--no-input without --yes: err = %v", err)
	}
	if err := confirmSend(true, true, m, 10, "b.tar.gz", "c@d.co"); err != nil {
		t.Errorf("--yes: %v", err)
	}
}

func TestSummaryAndPlanStripTerminalEscapes(t *testing.T) {
	var b bytes.Buffer
	m := bundle.Manifest{Agent: "nao\x1b[2J",
		Files:      []bundle.File{{Path: "dbt/a.sql"}},
		Skipped:    []bundle.Skip{{Path: "dbt/\x1b]52;c;cm0gLXJmIH4=\x07x.sql", Reason: "too big"}},
		Redactions: []string{"note \x1b[2J"}}
	printSummary(&b, m, 1)
	printPlan(&b, []bundle.Query{{Purpose: "p\x1b]0;title\x07", Statement: "select 1"}})
	if s := b.String(); strings.Contains(s, "\x1b]") || strings.Contains(s, "\x1b[2J") || !strings.Contains(s, "dbt/x.sql") {
		t.Errorf("escape reached the terminal: %q", s)
	}
}
