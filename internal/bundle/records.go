package bundle

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Records is records.jsonl while it is being collected: an owner-only temp
// file beside the bundle that each record is appended to as it arrives, so
// a collection is never held in memory. Write copies it into the bundle;
// Close removes it, and every caller defers Close.
type Records struct {
	f      *os.File
	bw     *bufio.Writer
	cw     *countingWriter
	enc    *json.Encoder
	counts map[string]int
	n      int
	closed bool
}

// MaxRecordsBytes bounds records.jsonl, so a collection too large for a
// bundle stops as soon as it is, not after reading everything: Inspect (and
// the backend) refuse a bundle whose entries total more than MaxTotalBytes,
// and the rest is left for the manifest and up to 256 MiB of project files.
const MaxRecordsBytes = MaxTotalBytes - 256<<20 - MaxManifestBytes

// ErrTooManyRecords is Add's error once records.jsonl would pass
// MaxRecordsBytes.
var ErrTooManyRecords = fmt.Errorf("the conversations read pass %d MiB, more than a bundle can hold", MaxRecordsBytes>>20)

// recordsLimit is MaxRecordsBytes; tests lower it.
var recordsLimit int64 = MaxRecordsBytes

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// NewRecords creates the temp file in dir (the bundle's directory, so it
// never lands somewhere the user did not choose).
func NewRecords(dir string) (*Records, error) {
	// CreateTemp opens with O_EXCL and mode 0600.
	f, err := os.CreateTemp(dir, ".benchouse-records-*.jsonl.tmp")
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(f, 256<<10)
	cw := &countingWriter{w: bw}
	enc := json.NewEncoder(cw)
	enc.SetEscapeHTML(false)
	return &Records{f: f, bw: bw, cw: cw, enc: enc, counts: map[string]int{}}, nil
}

// NewRecordsFor creates the temp file beside dst.
func NewRecordsFor(dst string) (*Records, error) { return NewRecords(filepath.Dir(dst)) }

// Add appends one record.
func (r *Records) Add(rec Record) error {
	if r.closed {
		return errors.New("records file is closed")
	}
	if err := r.enc.Encode(rec); err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	if r.cw.n > recordsLimit {
		return ErrTooManyRecords
	}
	r.counts[rec.RecordKind]++
	r.n++
	return nil
}

// Len is how many records were added.
func (r *Records) Len() int { return r.n }

// Counts returns a copy of the records added, per record kind.
func (r *Records) Counts() map[string]int {
	out := make(map[string]int, len(r.counts))
	for k, n := range r.counts {
		out[k] = n
	}
	return out
}

// reader flushes what was added and returns the file from its start, with
// its size.
func (r *Records) reader() (io.Reader, int64, error) {
	if r.closed {
		return nil, 0, errors.New("records file is closed")
	}
	if err := r.bw.Flush(); err != nil {
		return nil, 0, err
	}
	size, err := r.f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, 0, err
	}
	if _, err := r.f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	return io.LimitReader(r.f, size), size, nil
}

// Close closes and removes the temp file. It is safe to call more than once.
func (r *Records) Close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	err := r.f.Close()
	if rmErr := os.Remove(r.f.Name()); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
		err = rmErr
	}
	return err
}
