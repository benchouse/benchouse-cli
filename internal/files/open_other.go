//go:build !unix

package files

import "os"

// openNoFollow opens p read-only. Without O_NOFOLLOW (Windows), readRegular's
// os.SameFile check against the walk's Lstat is what refuses a swapped-in
// symlink: the open follows it, and the target is a different file.
func openNoFollow(p string) (*os.File, error) { return os.Open(p) }

func isSymlinkErr(error) bool { return false }
