package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ErrExists is returned by Write when dst exists and overwriting was not asked for.
var ErrExists = errors.New("already exists")

// Write creates the bundle at dst: the manifest first, then records.jsonl,
// then the project files. records.jsonl is copied from recs, the temp file
// the collection streamed into (nil for none); the manifest's Records counts
// are filled in here from recs, so they always agree with what was written.
// The project files are copied from files, the temp file they were staged
// into (nil for none). Write closes neither: their creators do.
//
// The bundle is written to a new owner-only temp file next to dst, synced,
// and only then moved into place, so dst is either absent or complete. An
// existing dst is refused unless force is set, and a symlink at dst always
// is: the CLI never writes through a link someone else may have planted.
// A cancelled ctx stops the write and removes every temp file.
func Write(ctx context.Context, dst string, force bool, m Manifest, recs *Records, files *Staging) (err error) {
	m.Format = Format
	m.Records = map[string]int{}
	if recs != nil {
		m.Records = recs.Counts()
	}
	if m.Queries == nil {
		m.Queries = []Query{}
	}
	if m.Files == nil {
		m.Files = []File{}
	}
	if m.Redactions == nil {
		m.Redactions = []string{}
	}
	if err := checkDest(dst, force); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	dir := filepath.Dir(dst)
	// CreateTemp opens with O_EXCL and mode 0600.
	f, err := os.CreateTemp(dir, ".benchouse-*.tar.gz.tmp")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := put(tw, "manifest.json", manifest, m.CreatedAt); err != nil {
		return err
	}

	var rd io.Reader = bytes.NewReader(nil)
	var size int64
	if recs != nil {
		if rd, size, err = recs.reader(); err != nil {
			return err
		}
	}
	if err := tw.WriteHeader(header("records.jsonl", size, m.CreatedAt)); err != nil {
		return err
	}
	if _, err := io.Copy(tw, ctxReader{ctx, rd}); err != nil {
		return err
	}

	err = files.each(func(e File, r io.Reader) error {
		if err := tw.WriteHeader(header(e.Path, e.Bytes, m.CreatedAt)); err != nil {
			return err
		}
		_, err := io.Copy(tw, ctxReader{ctx, r})
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := place(f.Name(), dst, force); err != nil {
		return err
	}
	committed = true
	syncDir(dir)
	return nil
}

// checkDest refuses a symlink at dst, anything that is not a regular file,
// and an existing file unless force is set.
func checkDest(dst string, force bool) error {
	st, err := os.Lstat(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case st.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink; refusing to write through it", dst)
	case !st.Mode().IsRegular():
		return fmt.Errorf("%s exists and is not a regular file", dst)
	case !force:
		return fmt.Errorf("%s %w; pass --force to overwrite it, or choose another --out", dst, ErrExists)
	}
	return nil
}

// place moves the finished temp file to dst. Without force it links, which
// fails if dst appeared meanwhile, so nothing is ever clobbered; filesystems
// without hard links fall back to a re-check and a rename.
func place(tmp, dst string, force bool) error {
	if err := checkDest(dst, force); err != nil {
		return err
	}
	if !force {
		err := os.Link(tmp, dst)
		if err == nil {
			os.Remove(tmp)
			return nil
		}
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s %w; pass --force to overwrite it, or choose another --out", dst, ErrExists)
		}
	}
	// rename replaces a file or a link itself, never a link's target.
	return os.Rename(tmp, dst)
}

// syncDir makes the rename durable where the platform allows it.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

func put(tw *tar.Writer, name string, data []byte, at time.Time) error {
	if err := tw.WriteHeader(header(name, int64(len(data)), at)); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func header(name string, size int64, at time.Time) *tar.Header {
	return &tar.Header{Name: name, Mode: 0o644, Size: size, ModTime: at, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
}

// ctxReader stops a long copy when ctx is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
