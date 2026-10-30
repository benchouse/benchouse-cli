package bundle

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// Summary is what Inspect reports about a bundle.
type Summary struct {
	Manifest Manifest
	// RecordLines is how many lines records.jsonl holds.
	RecordLines int
	// Entries lists every path in the archive, in order.
	Entries []string
	// Bytes is the uncompressed size of all entries.
	Bytes int64
}

// Limits on what Inspect reads, so a hostile or corrupt bundle cannot
// exhaust memory or disk time: a small file can decompress (or, through a
// sparse tar header, synthesize) gigabytes.
const (
	MaxManifestBytes = 4 << 20
	MaxEntries       = 100_000
	MaxTotalBytes    = 1 << 30
	MaxLineBytes     = 64 << 20
)

// Inspect reads a bundle and checks it is well formed: the manifest comes
// first and has this format, every path is a relative regular file under an
// allowed top-level name and appears once, records.jsonl agrees with the
// manifest's counts, and the project files are exactly the manifest's files,
// with the sizes and SHA-256 sums it lists. The backend enforces the same.
func Inspect(src string) (Summary, error) {
	f, err := os.Open(src)
	if err != nil {
		return Summary{}, err
	}
	defer f.Close()
	return inspect(f)
}

func inspect(f io.Reader) (Summary, error) {
	gz, err := gzip.NewReader(f)
	if err != nil {
		return Summary{}, fmt.Errorf("not a gzip file: %w", err)
	}
	tr := tar.NewReader(gz)

	var s Summary
	counts := map[string]int{}
	seen := map[string]bool{}   // exact names
	folded := map[string]bool{} // case-insensitive: one file on macOS and Windows
	files := map[string]File{}  // project files found, by path
	for i := 0; ; i++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return s, err
		}
		if i >= MaxEntries {
			return s, fmt.Errorf("more than %d entries", MaxEntries)
		}
		if err := checkName(h); err != nil {
			return s, err
		}
		if seen[h.Name] || folded[strings.ToLower(h.Name)] {
			return s, fmt.Errorf("%s: appears more than once", h.Name)
		}
		seen[h.Name], folded[strings.ToLower(h.Name)] = true, true
		if h.Size < 0 || h.Size > MaxTotalBytes-s.Bytes {
			return s, fmt.Errorf("%s: entries total more than %d MiB", h.Name, MaxTotalBytes>>20)
		}
		s.Entries = append(s.Entries, h.Name)
		s.Bytes += h.Size
		switch {
		case i == 0:
			if h.Name != "manifest.json" {
				return s, fmt.Errorf("first entry is %q, want manifest.json", h.Name)
			}
			if h.Size > MaxManifestBytes {
				return s, fmt.Errorf("manifest.json is larger than %d MiB", MaxManifestBytes>>20)
			}
			if err := json.NewDecoder(io.LimitReader(tr, MaxManifestBytes)).Decode(&s.Manifest); err != nil {
				return s, fmt.Errorf("manifest.json: %w", err)
			}
			if s.Manifest.Format != Format {
				return s, fmt.Errorf("format %q, want %q", s.Manifest.Format, Format)
			}
			if err := checkSample(s.Manifest.Sample); err != nil {
				return s, fmt.Errorf("manifest.json sample: %w", err)
			}
		case h.Name == "records.jsonl":
			sc := bufio.NewScanner(tr)
			sc.Buffer(make([]byte, 1<<20), MaxLineBytes)
			for sc.Scan() {
				var r Record
				if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
					return s, fmt.Errorf("records.jsonl line %d: %w", s.RecordLines+1, err)
				}
				s.RecordLines++
				counts[r.RecordKind]++
			}
			if err := sc.Err(); err != nil {
				return s, fmt.Errorf("records.jsonl: %w", err)
			}
		default: // dbt/ or context/
			sum := sha256.New()
			n, err := io.Copy(sum, tr)
			if err != nil {
				return s, fmt.Errorf("%s: %w", h.Name, err)
			}
			files[h.Name] = File{Path: h.Name, Bytes: n, SHA256: hex.EncodeToString(sum.Sum(nil))}
		}
	}
	if len(s.Entries) == 0 {
		return s, errors.New("empty archive")
	}
	for k, n := range s.Manifest.Records {
		if counts[k] != n {
			return s, fmt.Errorf("manifest says %d %s records, records.jsonl holds %d", n, k, counts[k])
		}
	}
	for k, n := range counts {
		if s.Manifest.Records[k] != n {
			return s, fmt.Errorf("records.jsonl holds %d %s records the manifest does not count", n, k)
		}
	}
	listed := map[string]bool{}
	for _, mf := range s.Manifest.Files {
		if listed[mf.Path] {
			return s, fmt.Errorf("manifest lists %s more than once", mf.Path)
		}
		listed[mf.Path] = true
		got, ok := files[mf.Path]
		switch {
		case !ok:
			return s, fmt.Errorf("manifest lists %s, which the bundle does not hold", mf.Path)
		case got.Bytes != mf.Bytes:
			return s, fmt.Errorf("%s: %d bytes, manifest says %d", mf.Path, got.Bytes, mf.Bytes)
		case !strings.EqualFold(got.SHA256, mf.SHA256):
			return s, fmt.Errorf("%s: sha256 differs from the manifest", mf.Path)
		}
	}
	for p := range files {
		if !listed[p] {
			return s, fmt.Errorf("%s is in the bundle but not in the manifest", p)
		}
	}
	return s, nil
}

// checkSample checks the optional sample object loosely: counts are not
// negative, a cap is respected, and the order is one this format knows. The
// in-window count may be below the collected count (rows can arrive between
// the count and the read), so it is not compared.
func checkSample(sm *Sample) error {
	if sm == nil {
		return nil
	}
	switch {
	case sm.MaxConversations < 0, sm.Conversations < 0, sm.InWindow != nil && *sm.InWindow < 0:
		return errors.New("negative count")
	case sm.MaxConversations > 0 && sm.Conversations > sm.MaxConversations:
		return fmt.Errorf("%d conversations, over the cap of %d", sm.Conversations, sm.MaxConversations)
	case sm.Order != "" && sm.Order != SampleMostRecent:
		return fmt.Errorf("order %q, want %q", sm.Order, SampleMostRecent)
	}
	return nil
}

func checkName(h *tar.Header) error {
	if h.Typeflag != tar.TypeReg {
		return fmt.Errorf("%s: only regular files are allowed", h.Name)
	}
	// archive/tar expands PAX (and GNU) sparse files into their logical
	// size, synthesizing zeros: a few bytes on disk become gigabytes read.
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return fmt.Errorf("%s: sparse files are not allowed", h.Name)
		}
	}
	name := h.Name
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00") || path.Clean(name) != name {
		return fmt.Errorf("%s: path must be relative and clean", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("%s: path must be relative and clean", name)
		}
	}
	switch top, rest, _ := strings.Cut(name, "/"); top {
	case "manifest.json", "records.jsonl":
		if strings.Contains(name, "/") {
			return fmt.Errorf("%s: unexpected path", name)
		}
	case "dbt", "context":
		if rest == "" {
			return fmt.Errorf("%s: unexpected path", name)
		}
	default:
		return fmt.Errorf("%s: unexpected top-level entry %q", name, top)
	}
	return nil
}
