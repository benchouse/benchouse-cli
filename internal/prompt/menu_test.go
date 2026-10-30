package prompt

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadKeyDecodesArrowsAndShortcuts(t *testing.T) {
	in := "\x1b[A\x1b[B\x1bOA\x1bOB\x1b[H\x1b[F\x1b[1~\x1b[4~kj\r\n\x03" + "7" + "x" + "\x1b[C"
	p := &Prompter{in: bufio.NewReader(strings.NewReader(in))}
	want := []key{keyUp, keyDown, keyUp, keyDown, keyHome, keyEnd, keyHome, keyEnd, keyUp, keyDown,
		keyEnter, keyEnter, keyInterrupt, keyNone, keyNone, keyNone}
	for i, w := range want {
		k, digit, err := p.readKey()
		if err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
		if k != w {
			t.Errorf("key %d = %v, want %v", i, k, w)
		}
		if i == 13 && digit != 7 {
			t.Errorf("digit = %d, want 7", digit)
		}
	}
}

func TestChooseWithoutATerminalIsNumbered(t *testing.T) {
	var out strings.Builder
	p := &Prompter{in: bufio.NewReader(strings.NewReader("2\n")), out: &out, fd: -1}
	got, err := p.Choose("Pick", [][2]string{{"a", "A"}, {"b", "B"}}, "")
	if err != nil || got != "b" {
		t.Fatalf("got %q, %v", got, err)
	}
	if !strings.Contains(out.String(), "1) A") {
		t.Errorf("numbered list not shown:\n%s", out.String())
	}
}
