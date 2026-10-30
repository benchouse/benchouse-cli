package files

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Summary is what a folder would contribute to a bundle, from a walk that
// only stats files: what Read would consider, before secret masking (which
// can still leave a file out).
type Summary struct {
	// Kept are the paths the keep rule accepts, relative and slash-separated,
	// in path order.
	Kept []string
	// KeptBytes is their total size.
	KeptBytes int64
	// Left counts the regular files the keep rule refuses, credentials files
	// included.
	Left int
	// Credentials are the credentials files noticed (never kept).
	Credentials []string
	// TooLarge are kept paths over MaxFileBytes, which Read leaves out.
	TooLarge []string
}

// Scan walks root the way Read does (same skipped folders, no symlinks) and
// reports what keep would include, without reading any file.
func Scan(root string, keep func(rel string) bool) (Summary, error) {
	var s Summary
	info, err := os.Stat(root)
	if err != nil {
		return s, err
	}
	if !info.IsDir() {
		return s, fmt.Errorf("%s is not a folder", root)
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if credentialsFile(rel) {
			s.Credentials = append(s.Credentials, rel)
		}
		if credentialsFile(rel) || !keep(rel) {
			s.Left++
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Size() > MaxFileBytes {
			s.TooLarge = append(s.TooLarge, rel)
			s.Left++
			return nil
		}
		s.Kept = append(s.Kept, rel)
		s.KeptBytes += fi.Size()
		return nil
	})
	sort.Strings(s.Kept)
	return s, err
}

// Limits returns an error when the summary is past what Read accepts, with
// the message Read would give.
func (s Summary) Limits(root string) error {
	switch {
	case len(s.Kept) > MaxFiles:
		return fmt.Errorf("%s has more than %d files to include; point at the project folder itself", root, MaxFiles)
	case s.KeptBytes > MaxTotalBytes:
		return fmt.Errorf("%s holds more than %d MiB of files to include", root, MaxTotalBytes>>20)
	}
	return nil
}
