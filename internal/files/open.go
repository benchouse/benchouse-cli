package files

import (
	"fmt"
	"io"
	"io/fs"
	"os"
)

var tooLarge = fmt.Sprintf("larger than %d MiB", MaxFileBytes>>20)

// readRegular reads the file the walk saw at p, and only that file. Between
// the walk listing p and the read, p could be swapped for a symlink (to
// ~/.ssh, say) or a FIFO: openNoFollow refuses a symlink where the platform
// allows it, and the open file must be a regular file and the same file the
// walk saw (os.SameFile on the open descriptor's stat), else it is skipped.
// The read is capped, so a file that grew after the size check cannot exceed
// MaxFileBytes.
func readRegular(p string, seen fs.FileInfo) ([]byte, string, error) {
	f, err := openNoFollow(p)
	if err != nil {
		if isSymlinkErr(err) {
			return nil, "changed while being read (now a symlink)", nil
		}
		return nil, "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	if !fi.Mode().IsRegular() {
		return nil, "not a regular file", nil
	}
	if !os.SameFile(fi, seen) {
		return nil, "changed while being read", nil
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > MaxFileBytes {
		return nil, tooLarge, nil
	}
	return raw, "", nil
}
