package agent

import (
	"errors"
	"fmt"
	"net/mail"
	"os"
	"strconv"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/netsafe"
)

// Validators for Field.Validate: offline format checks shared by the
// agents. Each takes a trimmed, non-empty value (Field.Check skips empty
// ones) and says what is expected. None of them echoes the value, so they
// are safe on secret fields too.

// URL accepts the address of a service the CLI sends credentials to, by
// netsafe's rules: https, or http to this machine only; a path only when
// allowPath (a self-hosted service under a prefix).
func URL(allowPath bool) func(string) error {
	return func(v string) error {
		_, err := netsafe.BaseURL(v, allowPath)
		return err
	}
}

// Port accepts a TCP port, 1 to 65535.
func Port(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("a port is a number from 1 to 65535")
	}
	return nil
}

// PositiveInt accepts a whole number above zero.
func PositiveInt(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return errors.New("give a whole number above zero")
	}
	return nil
}

// Host accepts a database server's host name or address (or, as Postgres
// does, a unix socket directory), not a URL.
func Host(v string) error {
	switch {
	case strings.Contains(v, "://"):
		return errors.New("give the host name only, without a scheme (db.example.com, not postgres://db.example.com)")
	case strings.ContainsAny(v, " \t@"):
		return errors.New("a host name has no spaces or @: give the user name in its own field")
	case strings.HasPrefix(v, "/"):
		return nil // a unix socket directory
	case strings.ContainsAny(v, "/?#"):
		return errors.New("give the host name only, without a path")
	}
	return nil
}

// ExistingFile accepts the path of a readable regular file.
func ExistingFile(v string) error {
	if err := tilde(v); err != nil {
		return err
	}
	st, err := os.Stat(v)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return errors.New("no such file")
	case err != nil:
		return fmt.Errorf("cannot read it: %s", Clip(err))
	case st.IsDir():
		return errors.New("that is a folder: give the file inside it")
	case !st.Mode().IsRegular():
		return errors.New("not a regular file")
	}
	f, err := os.Open(v)
	if err != nil {
		return fmt.Errorf("cannot read it: %s", Clip(err))
	}
	return f.Close()
}

// tilde refuses a path starting with ~: the shell expands it, a flag value
// and a prompt answer are taken as typed.
func tilde(v string) error {
	if v != "~" && !strings.HasPrefix(v, "~/") {
		return nil
	}
	if h, err := os.UserHomeDir(); err == nil {
		return fmt.Errorf("~ is not expanded here: write it out (%s%s)", h, strings.TrimPrefix(v, "~"))
	}
	return errors.New("~ is not expanded here: give the full path")
}

// Email accepts one email address.
func Email(v string) error {
	if _, err := mail.ParseAddress(v); err != nil {
		return errors.New("that doesn't look like an email address")
	}
	return nil
}

// Token accepts an id or key typed or pasted on one line: no spaces, no
// quotes, nothing that could not be part of one.
func Token(v string) error {
	if strings.ContainsAny(v, " \t\r\n\"'") {
		return errors.New("it has a space or a quote in it: paste the value only")
	}
	return nil
}

// Chain runs validators in order and returns the first error.
func Chain(vs ...func(string) error) func(string) error {
	return func(v string) error {
		for _, f := range vs {
			if err := f(v); err != nil {
				return err
			}
		}
		return nil
	}
}
