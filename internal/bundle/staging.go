package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Staging holds the project files (dbt, context) while a bundle is being
// built: an owner-only temp file beside the bundle that each masked file is
// appended to as it is read, so a folder is never held in memory. Only each
// file's path, offset, size and digest are kept. Write copies the files
// into the bundle in Staging's order; Close removes the temp file, and
// every caller defers Close.
type Staging struct {
	f       *os.File
	entries []staged
	size    int64
	closed  bool
}

type staged struct {
	File
	off int64
}

// NewStaging creates the temp file in dir (the bundle's directory, so it
// never lands somewhere the user did not choose).
func NewStaging(dir string) (*Staging, error) {
	// CreateTemp opens with O_EXCL and mode 0600.
	f, err := os.CreateTemp(dir, ".benchouse-files-*.tmp")
	if err != nil {
		return nil, err
	}
	return &Staging{f: f}, nil
}

// NewStagingFor creates the temp file beside dst.
func NewStagingFor(dst string) (*Staging, error) { return NewStaging(filepath.Dir(dst)) }

// Add appends one file's bytes under path (inside the bundle, e.g.
// dbt/models/orders.sql), with how many values were masked in it, and
// returns its manifest entry. data is not kept.
func (s *Staging) Add(path string, data []byte, masked int) (File, error) {
	if s.closed {
		return File{}, errors.New("staging file is closed")
	}
	if _, err := s.f.WriteAt(data, s.size); err != nil {
		return File{}, fmt.Errorf("stage %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	e := staged{File: File{Path: path, Bytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Masked: masked}, off: s.size}
	s.size += int64(len(data))
	s.entries = append(s.entries, e)
	return e.File, nil
}

// Len is how many files were added.
func (s *Staging) Len() int {
	if s == nil {
		return 0
	}
	return len(s.entries)
}

// SortFrom orders the files added from index i on by path, so each folder
// lands in the bundle in path order whatever order it was walked in.
func (s *Staging) SortFrom(i int) {
	tail := s.entries[i:]
	sort.SliceStable(tail, func(a, b int) bool { return tail[a].Path < tail[b].Path })
}

// Files returns the manifest entries of the files added, in order.
func (s *Staging) Files() []File {
	if s == nil {
		return nil
	}
	out := make([]File, len(s.entries))
	for i, e := range s.entries {
		out[i] = e.File
	}
	return out
}

// each calls fn with each file's entry and a reader of its bytes, in order.
func (s *Staging) each(fn func(File, io.Reader) error) error {
	if s == nil {
		return nil
	}
	if s.closed {
		return errors.New("staging file is closed")
	}
	for _, e := range s.entries {
		if err := fn(e.File, io.NewSectionReader(s.f, e.off, e.Bytes)); err != nil {
			return err
		}
	}
	return nil
}

// Close closes and removes the temp file. It is safe to call more than once.
func (s *Staging) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	err := s.f.Close()
	if rmErr := os.Remove(s.f.Name()); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
		err = rmErr
	}
	return err
}
