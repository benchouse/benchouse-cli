package prompt

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestCleanStripsTerminalEscapes(t *testing.T) {
	for in, want := range map[string]string{
		"models/\x1b]52;c;ZWNobyBwd25lZA==\x07x.sql": "models/x.sql",
		"a\x1b]52;c;ZWNobw==\x1b\\b":                 "ab",
		"\x1b[2Jcleared":                             "cleared",
		"red \x1b[31mtext\x1b[0m":                    "red text",
		"bell\x07 cr\r del\x7f nul\x00":              "bell cr del nul",
		"c1 \u009b2J csi \u009d0;t\u009c osc":        "c1  csi  osc",
		"line\nnext\ttab ünïcode 日本":                 "line\nnext\ttab ünïcode 日本",
		"bad \xff byte":                              "bad � byte",
		"trailing esc\x1b":                           "trailing esc",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrompterOutputIsCleanedButKeepsStyles(t *testing.T) {
	var out bytes.Buffer
	p := &Prompter{in: bufio.NewReader(strings.NewReader("1\n")), out: NewWriter(&out), fd: -1}
	p.Say("\x1b[1mtitle\x1b[0m")
	if _, err := p.Choose("Project", [][2]string{{"p1", "evil\x1b]52;c;aGk=\x07\x1b[2J name"}}, ""); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Contains(s, "\x1b]") || strings.Contains(s, "\x1b[2J") || !strings.Contains(s, "evil name") {
		t.Errorf("escape reached the terminal: %q", s)
	}
	if !strings.Contains(s, "\x1b[1mtitle\x1b[0m") {
		t.Errorf("own styling lost: %q", s)
	}
}
