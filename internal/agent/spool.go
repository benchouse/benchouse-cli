package agent

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
)

// spoolInMemory is how many ids an IDSpool holds before it moves them to a
// temp file. Under the default cap (2,000 conversations) nothing spills.
const spoolInMemory = 10_000

// IDSpool keeps the ids of the conversations a reader chose, to fetch their
// children in batches afterwards. Up to spoolInMemory ids stay in memory;
// beyond that (a run with no --max-conversations cap) they spill to a temp
// file (0600, removed by Close), so memory stays bounded however many
// conversations the window holds.
type IDSpool struct {
	mem []string
	f   *os.File
	w   *bufio.Writer
	n   int
}

// Add appends an id.
func (s *IDSpool) Add(id string) error {
	s.n++
	if s.f == nil && len(s.mem) < spoolInMemory {
		s.mem = append(s.mem, id)
		return nil
	}
	if s.f == nil {
		f, err := os.CreateTemp("", "benchouse-ids-*")
		if err != nil {
			return fmt.Errorf("spill conversation ids: %w", err)
		}
		s.f, s.w = f, bufio.NewWriter(f)
		for _, m := range s.mem {
			if err := s.write(m); err != nil {
				return err
			}
		}
		s.mem = nil
	}
	return s.write(id)
}

func (s *IDSpool) write(id string) error {
	if _, err := s.w.WriteString(strconv.Quote(id) + "\n"); err != nil {
		return fmt.Errorf("spill conversation ids: %w", err)
	}
	return nil
}

// Len is how many ids were added.
func (s *IDSpool) Len() int { return s.n }

// Spilled reports whether the ids moved to a temp file.
func (s *IDSpool) Spilled() bool { return s.f != nil }

// Batches calls fn with the ids in the order they were added, size at a
// time. It may be called more than once. fn must not keep the slice. An
// error from fn stops it.
func (s *IDSpool) Batches(size int, fn func([]string) error) error {
	if size < 1 {
		size = BatchSize
	}
	if s.f == nil {
		for start := 0; start < len(s.mem); start += size {
			if err := fn(s.mem[start:min(start+size, len(s.mem))]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := s.w.Flush(); err != nil {
		return err
	}
	if _, err := s.f.Seek(0, 0); err != nil {
		return err
	}
	defer s.f.Seek(0, 2) //nolint:errcheck // back to the end for further Adds
	sc := bufio.NewScanner(s.f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	batch := make([]string, 0, size)
	for sc.Scan() {
		id, err := strconv.Unquote(sc.Text())
		if err != nil {
			return fmt.Errorf("read spilled conversation ids: %w", err)
		}
		batch = append(batch, id)
		if len(batch) == size {
			if err := fn(batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(batch) > 0 {
		return fn(batch)
	}
	return nil
}

// Close removes the temp file, if any.
func (s *IDSpool) Close() error {
	if s.f == nil {
		return nil
	}
	name := s.f.Name()
	err := s.f.Close()
	if rerr := os.Remove(name); err == nil {
		err = rerr
	}
	s.f, s.w = nil, nil
	return err
}
