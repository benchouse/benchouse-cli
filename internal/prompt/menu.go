package prompt

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/term"
)

// ErrInterrupted is returned when the user presses Ctrl-C in a menu.
var ErrInterrupted = errors.New("interrupted")

// key is one decoded keypress.
type key int

const (
	keyNone key = iota
	keyUp
	keyDown
	keyHome
	keyEnd
	keyEnter
	keyInterrupt
)

// menu shows options as a list the user moves through with the arrow keys
// (or j/k, or a digit) and picks with Enter. When it is done the list is
// replaced by one line, the question and the answer, so the transcript reads
// like the numbered prompt would.
func (p *Prompter) menu(label string, options [][2]string, def string) (string, error) {
	sel := 0
	for i, o := range options {
		if o[0] == def {
			sel = i
		}
	}
	state, err := term.MakeRaw(p.fd)
	if err != nil { // not a real terminal after all: fall back to numbers
		p.tty = nil
		return p.Choose(label, options, def)
	}
	defer term.Restore(p.fd, state)

	w := p.tty
	fmt.Fprint(w, "\x1b[?25l") // hide the cursor while the list is drawn
	defer fmt.Fprint(w, "\x1b[?25h")

	draw := func(first bool) {
		if !first {
			fmt.Fprintf(w, "\x1b[%dA", len(options)+1) // back to the question line
		}
		fmt.Fprintf(w, "\r\x1b[2K%s %s\r\n", bold(label), dim("↑/↓ to move, Enter to choose"))
		for i, o := range options {
			fmt.Fprint(w, "\r\x1b[2K")
			if i == sel {
				fmt.Fprintf(w, "%s %s\r\n", accent("❯"), accent(Clean(o[1])))
			} else {
				fmt.Fprintf(w, "  %s\r\n", Clean(o[1]))
			}
		}
	}
	draw(true)
	for {
		k, digit, err := p.readKey()
		if err != nil {
			return "", err
		}
		switch k {
		case keyUp:
			sel = (sel - 1 + len(options)) % len(options)
		case keyDown:
			sel = (sel + 1) % len(options)
		case keyHome:
			sel = 0
		case keyEnd:
			sel = len(options) - 1
		case keyInterrupt:
			p.collapse(label, "", len(options))
			return "", ErrInterrupted
		case keyEnter:
			p.collapse(label, Clean(options[sel][1]), len(options))
			return options[sel][0], nil
		case keyNone:
			if digit >= 1 && digit <= len(options) {
				sel = digit - 1
			}
		}
		draw(false)
	}
}

// collapse replaces the drawn list with "question answer".
func (p *Prompter) collapse(label, answer string, n int) {
	w := p.tty
	fmt.Fprintf(w, "\x1b[%dA", n+1)
	fmt.Fprintf(w, "\r\x1b[2K%s %s\r\n", bold(label), answer)
	for i := 0; i < n; i++ {
		fmt.Fprint(w, "\r\x1b[2K\r\n")
	}
	fmt.Fprintf(w, "\x1b[%dA", n)
}

// readKey reads one keypress from the raw terminal. digit is set for 1-9.
func (p *Prompter) readKey() (key, int, error) {
	b, err := p.in.ReadByte()
	if err != nil {
		return keyNone, 0, err
	}
	switch b {
	case '\r', '\n':
		return keyEnter, 0, nil
	case 3, 4: // Ctrl-C, Ctrl-D
		return keyInterrupt, 0, nil
	case 'k', 16: // k, Ctrl-P
		return keyUp, 0, nil
	case 'j', 14: // j, Ctrl-N
		return keyDown, 0, nil
	case 'g':
		return keyHome, 0, nil
	case 'G':
		return keyEnd, 0, nil
	case 0x1b:
		return p.readEscape()
	}
	if b >= '1' && b <= '9' {
		return keyNone, int(b - '0'), nil
	}
	return keyNone, 0, nil
}

// readEscape decodes the rest of an escape sequence: ESC [ A / ESC O A for
// the arrows, and the Home/End variants. Anything else is ignored.
func (p *Prompter) readEscape() (key, int, error) {
	if p.in.Buffered() == 0 {
		return keyNone, 0, nil // a lone Esc press
	}
	b, err := p.in.ReadByte()
	if err != nil || (b != '[' && b != 'O') {
		return keyNone, 0, err
	}
	var seq strings.Builder
	for i := 0; i < 8; i++ { // CSI parameters end with a byte in @..~
		c, err := p.in.ReadByte()
		if err != nil {
			return keyNone, 0, err
		}
		seq.WriteByte(c)
		if c >= '@' && c <= '~' {
			break
		}
	}
	switch seq.String() {
	case "A":
		return keyUp, 0, nil
	case "B":
		return keyDown, 0, nil
	case "H", "1~", "7~":
		return keyHome, 0, nil
	case "F", "4~", "8~":
		return keyEnd, 0, nil
	}
	return keyNone, 0, nil
}

func accent(s string) string {
	if !color {
		return s
	}
	return "\x1b[36m" + s + "\x1b[0m" // cyan
}
