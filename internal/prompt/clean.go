package prompt

import (
	"io"
	"strings"
	"unicode/utf8"
)

// Clean makes text from outside the CLI (file names, server error bodies,
// project names, notes) safe to print on a terminal: it removes escape
// sequences (CSI, OSC, DCS and the like, in 7- and 8-bit form), C0 controls
// other than newline and tab, DEL and C1 controls, and replaces invalid UTF-8.
// Without it a file named "\x1b]52;c;...\a" could write the clipboard, and
// "\x1b[2J" could wipe what the user was about to approve.
func Clean(s string) string { return clean(s, false) }

// styles are the only sequences the CLI emits itself (see bold, dim). Output
// cleaned at the Prompter boundary keeps exactly these; they can only change
// weight, never move the cursor or talk to the terminal.
var styles = []string{"\x1b[0m", "\x1b[1m", "\x1b[2m"}

func clean(s string, keepStyles bool) string {
	if !needsClean(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if keepStyles && s[i] == 0x1b {
			kept := false
			for _, st := range styles {
				if strings.HasPrefix(s[i:], st) {
					b.WriteString(st)
					i += len(st)
					kept = true
					break
				}
			}
			if kept {
				continue
			}
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteRune(utf8.RuneError)
			i++
		case r == 0x1b:
			i = skipEscape(s, i+1)
		case r == 0x9b: // 8-bit CSI
			i = skipCSI(s, i+n)
		case r == 0x90 || r == 0x9d || r == 0x98 || r == 0x9e || r == 0x9f: // 8-bit DCS, OSC, SOS, PM, APC
			i = skipString(s, i+n)
		case r == '\n' || r == '\t':
			b.WriteRune(r)
			i += n
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			i += n
		default:
			b.WriteString(s[i : i+n])
			i += n
		}
	}
	return b.String()
}

func needsClean(s string) bool {
	for _, r := range s {
		if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) || r == utf8.RuneError {
			return true
		}
	}
	return false
}

// skipEscape returns the index after the escape sequence whose ESC was just
// before i.
func skipEscape(s string, i int) int {
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[':
		return skipCSI(s, i+1)
	case ']', 'P', 'X', '^', '_':
		return skipString(s, i+1)
	}
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f { // intermediates
		i++
	}
	if i < len(s) {
		i++ // the final byte
	}
	return i
}

// skipCSI skips parameter and intermediate bytes up to the final byte.
func skipCSI(s string, i int) int {
	for i < len(s) {
		switch c := s[i]; {
		case c >= 0x40 && c <= 0x7e:
			return i + 1
		case c < 0x20 || c > 0x7e: // malformed: end here, the byte is judged on its own
			return i
		}
		i++
	}
	return i
}

// skipString skips an OSC/DCS-style string up to BEL or ST (ESC \ or 0x9c).
func skipString(s string, i int) int {
	for i < len(s) {
		if s[i] == 0x07 {
			return i + 1
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
			return i + 2
		}
		if strings.HasPrefix(s[i:], "\u009c") {
			return i + len("\u009c")
		}
		i++
	}
	return i
}

// cleanWriter cleans every write, keeping the CLI's own styles.
type cleanWriter struct{ w io.Writer }

// NewWriter returns a writer that cleans what is written to w like Clean,
// except for the CLI's own bold and dim styling. Use it for any output that
// includes text from outside the CLI. Each Write is cleaned on its own, so a
// sequence split across writes is dropped, never completed.
func NewWriter(w io.Writer) io.Writer {
	if _, ok := w.(cleanWriter); ok {
		return w
	}
	return cleanWriter{w}
}

func (c cleanWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(c.w, clean(string(p), true)); err != nil {
		return 0, err
	}
	return len(p), nil
}
