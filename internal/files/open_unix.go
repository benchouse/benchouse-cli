//go:build unix

package files

import (
	"errors"
	"os"
	"syscall"
)

// openNoFollow opens p read-only, refusing a symlink in the last component.
// O_NONBLOCK keeps a FIFO swapped in for the file from blocking the open; it
// has no effect on a regular file.
func openNoFollow(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// isSymlinkErr reports the error O_NOFOLLOW gives for a symlink: ELOOP on
// Linux and macOS, EMLINK on FreeBSD, EFTYPE on NetBSD.
func isSymlinkErr(err error) bool {
	return errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK)
}
