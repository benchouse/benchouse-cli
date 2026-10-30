package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// tarOf writes a tar of the given entries in order, bypassing Write.
func tarOf(entries ...[2]string) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0o644, Size: int64(len(e[1])), Typeflag: tar.TypeReg})
		tw.Write([]byte(e[1]))
	}
	tw.Close()
	return b.Bytes()
}

func gz(b []byte) []byte {
	var out bytes.Buffer
	w, _ := gzip.NewWriterLevel(&out, gzip.NoCompression) // the reader sees the same archive, the fuzzer runs faster
	w.Write(b)
	w.Close()
	return out.Bytes()
}

// hugeHeader is a tar entry claiming far more bytes than follow.
func hugeHeader() []byte {
	b := make([]byte, 512)
	copy(b, "dbt/huge.sql")
	copy(b[100:], "0000600\x00")
	copy(b[108:], "0000000\x00")
	copy(b[116:], "0000000\x00")
	copy(b[124:], "77777777777\x00")
	copy(b[136:], "00000000000\x00")
	b[156] = '0'
	copy(b[257:], "ustar\x0000")
	copy(b[148:], "        ")
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:], fmt.Sprintf("%06o\x00 ", sum))
	return b
}

var (
	fuzzManifest = `{"format":"benchouse-bundle/1","agent":"nao","records":{"chat":1},"files":[{"path":"dbt/a.sql","bytes":1,"sha256":"ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"}]}`
	fuzzRecords  = `{"source_kind":"nao","record_kind":"chat","payload":{}}` + "\n"
)

func fuzzSeeds() [][]byte {
	good := tarOf([2]string{"manifest.json", fuzzManifest}, [2]string{"records.jsonl", fuzzRecords}, [2]string{"dbt/a.sql", "a"})
	return [][]byte{
		good,
		tarOf([2]string{"records.jsonl", fuzzRecords}),
		tarOf([2]string{"manifest.json", fuzzManifest}, [2]string{"../etc/passwd", "x"}),
		tarOf([2]string{"manifest.json", `{"format":"benchouse-bundle/1","sample":{"max_conversations":1,"conversations":5}}`}),
		append(tarOf([2]string{"manifest.json", fuzzManifest})[:512*2], hugeHeader()...),
		good[:700],
		{},
	}
}

// Inspect reads files a user was handed: whatever the bytes, it returns an
// error or a summary, never panics, and stays inside its limits.
func FuzzInspect(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(gz(s))
	}
	f.Add([]byte("not gzip"))
	f.Add([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		inspectChecked(t, data)
	})
}

// FuzzInspectTar fuzzes the archive inside the gzip layer, where the
// interesting structure is.
func FuzzInspectTar(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		inspectChecked(t, gz(data))
	})
}

// inspectChecked runs the reader Inspect uses on the bytes in memory, so
// the fuzzer (and its minimizer) is not slowed by a file per input.
func inspectChecked(t *testing.T, data []byte) {
	type result struct {
		s   Summary
		err error
	}
	done := make(chan result, 1)
	go func() { s, err := inspect(bytes.NewReader(data)); done <- result{s, err} }()
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		buf := make([]byte, 1<<16)
		t.Fatalf("Inspect hung on this input:\n%s", buf[:runtime.Stack(buf, true)])
	}
	s, err := r.s, r.err
	if err != nil {
		return
	}
	if s.Bytes > MaxTotalBytes || len(s.Entries) > MaxEntries {
		t.Fatalf("accepted a bundle over the limits: %d bytes, %d entries", s.Bytes, len(s.Entries))
	}
	if len(s.Entries) == 0 || s.Entries[0] != "manifest.json" || s.Manifest.Format != Format {
		t.Fatalf("accepted a bundle without a manifest first: %v", s.Entries)
	}
	for _, e := range s.Entries {
		if e == "" || e[0] == '/' || bytes.Contains([]byte(e), []byte("..")) && filepath.Clean(e) != e {
			t.Fatalf("accepted entry %q", e)
		}
	}
}

// The first seed is a valid bundle, so the fuzzer starts from one.
func TestFuzzSeedIsValid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "b.tar.gz")
	os.WriteFile(p, gz(fuzzSeeds()[0]), 0o600)
	if _, err := Inspect(p); err != nil {
		t.Fatal(err)
	}
}
