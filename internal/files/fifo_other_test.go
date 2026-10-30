//go:build !unix

package files

import "errors"

func mkfifo(string) error { return errors.New("no fifos") }
