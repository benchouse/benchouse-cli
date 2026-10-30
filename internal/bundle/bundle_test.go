package bundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWriteThenInspect(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "b.tar.gz")
	recs := []Record{
		{SourceKind: "nao", RecordKind: "nao_chat", NativeIDs: map[string]string{"chat_id": "c1"}, FetchMethod: "sql:chat", Payload: json.RawMessage(`{"id":"c1","n":12345678901234567890}`)},
		{SourceKind: "nao", RecordKind: "nao_chat_message", FetchMethod: "sql:chat_message", Payload: json.RawMessage(`{"id":"m1"}`)},
		{SourceKind: "nao", RecordKind: "nao_chat_message", FetchMethod: "sql:chat_message", Payload: json.RawMessage(`{"id":"m2"}`)},
	}
	m := Manifest{CLIVersion: "test", CreatedAt: time.Unix(0, 0).UTC(), Agent: "nao", Email: "a@b.co",
		Files: []File{{Path: "dbt/models/x.sql", Bytes: 8, SHA256: sha("select 1")}}}
	if err := Write(context.Background(), dst, false, m, recordsOf(t, filepath.Dir(dst), recs), stagingOf(t, filepath.Dir(dst), "dbt/models/x.sql", "select 1")); err != nil {
		t.Fatal(err)
	}
	s, err := Inspect(dst)
	if err != nil {
		t.Fatal(err)
	}
	if s.RecordLines != 3 || s.Manifest.Records["nao_chat_message"] != 2 || s.Manifest.Format != Format {
		t.Errorf("summary = %+v", s)
	}
	if strings.Join(s.Entries, ",") != "manifest.json,records.jsonl,dbt/models/x.sql" {
		t.Errorf("entries = %v", s.Entries)
	}
	// Payloads are stored verbatim: a large integer survives exactly.
	b := readEntry(t, dst, "records.jsonl")
	if !strings.Contains(b, "12345678901234567890") {
		t.Errorf("payload number altered:\n%s", b)
	}
	if st, _ := os.Stat(dst); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("bundle mode %v, want 0600", st.Mode().Perm())
	}
}

func readEntry(t *testing.T, src, name string) string {
	t.Helper()
	f, _ := os.Open(src)
	defer f.Close()
	gz, _ := gzip.NewReader(f)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("%s not found", name)
		}
		if h.Name == name {
			var sb strings.Builder
			buf := make([]byte, 4096)
			for {
				n, err := tr.Read(buf)
				sb.Write(buf[:n])
				if err != nil {
					return sb.String()
				}
			}
		}
	}
}

// raw writes a tar.gz of the given entries in order, bypassing Write.
func raw(t *testing.T, entries ...[2]string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "raw.tar.gz")
	f, _ := os.Create(dst)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0o644, Size: int64(len(e[1])), Typeflag: tar.TypeReg})
		tw.Write([]byte(e[1]))
	}
	tw.Close()
	gz.Close()
	f.Close()
	return dst
}

func TestInspectRejectsMalformedBundles(t *testing.T) {
	good := `{"format":"benchouse-bundle/1","records":{"nao_chat":1}}`
	rec := `{"source_kind":"nao","record_kind":"nao_chat","payload":{}}` + "\n"
	cases := map[string][][2]string{
		"manifest not first": {{"records.jsonl", rec}, {"manifest.json", good}},
		"wrong format":       {{"manifest.json", `{"format":"other/9"}`}},
		"path traversal":     {{"manifest.json", good}, {"records.jsonl", rec}, {"dbt/../../etc/passwd", "x"}},
		"absolute path":      {{"manifest.json", good}, {"/etc/passwd", "x"}},
		"unknown top level":  {{"manifest.json", good}, {"records.jsonl", rec}, {"secrets/x", "x"}},
		"count mismatch":     {{"manifest.json", good}, {"records.jsonl", rec + rec}},
		"uncounted kind":     {{"manifest.json", good}, {"records.jsonl", rec + `{"record_kind":"span","payload":{}}` + "\n"}},
	}
	for name, entries := range cases {
		if _, err := Inspect(raw(t, entries...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Inspect(raw(t, [2]string{"manifest.json", good}, [2]string{"records.jsonl", rec})); err != nil {
		t.Errorf("a valid bundle was rejected: %v", err)
	}
}

func TestWriteRefusesToClobberOrFollowLinks(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{CLIVersion: "test", Agent: "nao"}
	ctx := context.Background()
	dst := filepath.Join(dir, "b.tar.gz")
	os.WriteFile(dst, []byte("keep me"), 0o644)
	if err := Write(ctx, dst, false, m, nil, nil); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("existing file overwritten without --force: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "keep me" {
		t.Errorf("existing file changed: %q", b)
	}
	if err := Write(ctx, dst, true, m, nil, nil); err != nil {
		t.Fatalf("--force: %v", err)
	}
	if st, _ := os.Stat(dst); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("overwritten bundle mode %v, want 0600", st.Mode().Perm())
	}
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("victim"), 0o644)
	link := filepath.Join(dir, "link.tar.gz")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	for _, force := range []bool{false, true} {
		if err := Write(ctx, link, force, m, nil, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("force=%v: wrote through a symlink: %v", force, err)
		}
	}
	if b, _ := os.ReadFile(target); string(b) != "victim" {
		t.Errorf("symlink target changed: %q", b)
	}
	assertNoTemp(t, dir)
}

func TestWriteCleansUpWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := filepath.Join(dir, "b.tar.gz")
	recs := recordsOf(t, dir, []Record{{RecordKind: "x", Payload: json.RawMessage(`{}`)}})
	st := stagingOf(t, dir, "dbt/a.sql", "x")
	if err := Write(ctx, dst, false, Manifest{}, recs, st); err == nil {
		t.Fatal("a cancelled write succeeded")
	}
	recs.Close() // the collector's deferred Closes
	st.Close()
	if _, err := os.Lstat(dst); err == nil {
		t.Error("a cancelled write left the bundle")
	}
	assertNoTemp(t, dir)
}

// recordsOf streams recs into a records file in d, closed (and removed) at
// the end of the test.
func recordsOf(t *testing.T, d string, recs []Record) *Records {
	t.Helper()
	r, err := NewRecords(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	for _, rec := range recs {
		if err := r.Add(rec); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// stagingOf stages files (path, content pairs) in d, closed (and removed)
// at the end of the test.
func stagingOf(t *testing.T, d string, pairs ...string) *Staging {
	t.Helper()
	s, err := NewStaging(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for i := 0; i+1 < len(pairs); i += 2 {
		if _, err := s.Add(pairs[i], []byte(pairs[i+1]), 0); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestInspectChecksFilesAgainstTheManifest(t *testing.T) {
	rec := `{"source_kind":"nao","record_kind":"nao_chat","payload":{}}` + "\n"
	man := func(files string) string {
		return `{"format":"benchouse-bundle/1","records":{"nao_chat":1},"files":[` + files + `]}`
	}
	ok := `{"path":"dbt/a..b.sql","bytes":3,"sha256":"` + sha("abc") + `"}`
	if _, err := Inspect(raw(t, [2]string{"manifest.json", man(ok)}, [2]string{"records.jsonl", rec}, [2]string{"dbt/a..b.sql", "abc"})); err != nil {
		t.Errorf("a file named a..b.sql was rejected: %v", err)
	}
	cases := map[string][][2]string{
		"unlisted file":         {{"manifest.json", man("")}, {"records.jsonl", rec}, {"dbt/x.sql", "abc"}},
		"missing file":          {{"manifest.json", man(ok)}, {"records.jsonl", rec}},
		"wrong content":         {{"manifest.json", man(ok)}, {"records.jsonl", rec}, {"dbt/a..b.sql", "abd"}},
		"wrong size":            {{"manifest.json", man(ok)}, {"records.jsonl", rec}, {"dbt/a..b.sql", "abcd"}},
		"duplicate":             {{"manifest.json", man(ok)}, {"records.jsonl", rec}, {"dbt/a..b.sql", "abc"}, {"dbt/a..b.sql", "abc"}},
		"case duplicate":        {{"manifest.json", man(ok)}, {"records.jsonl", rec}, {"dbt/a..b.sql", "abc"}, {"dbt/A..B.sql", "abc"}},
		"dot segment":           {{"manifest.json", man("")}, {"records.jsonl", rec}, {"dbt/./x", "x"}},
		"dotdot":                {{"manifest.json", man("")}, {"records.jsonl", rec}, {"..", "x"}},
		"backslash":             {{"manifest.json", man("")}, {"records.jsonl", rec}, {"dbt/..\\..\\x", "x"}},
		"bare top level":        {{"manifest.json", man("")}, {"records.jsonl", rec}, {"dbt", "x"}},
		"manifest listed twice": {{"manifest.json", man(ok + "," + ok)}, {"records.jsonl", rec}, {"dbt/a..b.sql", "abc"}},
	}
	for name, entries := range cases {
		if _, err := Inspect(raw(t, entries...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestInspectRefusesBombs(t *testing.T) {
	good := `{"format":"benchouse-bundle/1","records":{}}`
	write := func(hdrs ...*tar.Header) string {
		dst := filepath.Join(t.TempDir(), "bomb.tar.gz")
		f, _ := os.Create(dst)
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(good)), Typeflag: tar.TypeReg})
		tw.Write([]byte(good))
		for _, h := range hdrs {
			tw.WriteHeader(h) // no data: the check must come before any read
			tw.Flush()
		}
		gz.Close()
		f.Close()
		return dst
	}
	huge := &tar.Header{Name: "dbt/big.sql", Mode: 0o600, Size: MaxTotalBytes, Typeflag: tar.TypeReg}
	if _, err := Inspect(write(huge)); err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Errorf("oversized entry: %v", err)
	}
	if _, err := Inspect(sparseBomb(t, good)); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Errorf("a PAX sparse header: %v", err)
	}
	link := &tar.Header{Name: "dbt/l", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}
	if _, err := Inspect(write(link)); err == nil {
		t.Error("a symlink entry was accepted")
	}
	bigManifest := `{"format":"benchouse-bundle/1","records":{},"redactions":["` + strings.Repeat("x", MaxManifestBytes) + `"]}`
	if _, err := Inspect(raw(t, [2]string{"manifest.json", bigManifest})); err == nil {
		t.Error("an oversized manifest was accepted")
	}
}

// sparseBomb hand-writes a PAX 1.0 sparse entry (archive/tar's writer will
// not): a few bytes on disk that the reader expands to a terabyte of zeros.
func sparseBomb(t *testing.T, manifest string) string {
	t.Helper()
	block := func(name string, size int, flag byte) []byte {
		b := make([]byte, 512)
		copy(b, name)
		copy(b[100:], "0000600\x00")
		copy(b[108:], "0000000\x00")
		copy(b[116:], "0000000\x00")
		copy(b[124:], fmt.Sprintf("%011o\x00", size))
		copy(b[136:], "00000000000\x00")
		b[156] = flag
		copy(b[257:], "ustar\x0000")
		copy(b[148:], "        ")
		sum := 0
		for _, c := range b {
			sum += int(c)
		}
		copy(b[148:], fmt.Sprintf("%06o\x00 ", sum))
		return b
	}
	pad := func(d []byte) []byte { return append(d, make([]byte, (512-len(d)%512)%512)...) }
	rec := func(k, v string) string {
		l := len(k) + len(v) + 3
		for len(fmt.Sprintf("%d %s=%s\n", l, k, v)) != l {
			l++
		}
		return fmt.Sprintf("%d %s=%s\n", l, k, v)
	}
	pax := rec("GNU.sparse.major", "1") + rec("GNU.sparse.minor", "0") + rec("GNU.sparse.name", "dbt/s.sql") + rec("GNU.sparse.realsize", "1099511627776")
	// The 1.0 map lives in the data: one region of 0 bytes at the end.
	data := "1\n1099511627776\n0\n"
	var out []byte
	out = append(out, block("manifest.json", len(manifest), '0')...)
	out = append(out, pad([]byte(manifest))...)
	out = append(out, block("PaxHeaders/s", len(pax), 'x')...)
	out = append(out, pad([]byte(pax))...)
	out = append(out, block("GNUSparseFile.0/s", 512, '0')...)
	out = append(out, pad([]byte(data))...)
	out = append(out, make([]byte, 1024)...)
	dst := filepath.Join(t.TempDir(), "sparse.tar.gz")
	f, _ := os.Create(dst)
	gz := gzip.NewWriter(f)
	gz.Write(out)
	gz.Close()
	f.Close()
	return dst
}

func TestRecordsFileIsOwnerOnlyAndRemovedOnClose(t *testing.T) {
	d := t.TempDir()
	r, err := NewRecords(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add(Record{RecordKind: "k", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(r.f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	if r.Len() != 1 || r.Counts()["k"] != 1 {
		t.Errorf("len %d counts %v", r.Len(), r.Counts())
	}
	r.Close()
	r.Close()
	assertNoTemp(t, d)
}

func TestInspectChecksTheSample(t *testing.T) {
	neg := int64(-1)
	for _, tc := range []struct {
		s  *Sample
		ok bool
	}{
		{nil, true},
		{&Sample{MaxConversations: 2000, Conversations: 10, Order: SampleMostRecent}, true},
		{&Sample{MaxConversations: 0, Conversations: 5000, Order: SampleMostRecent}, true},
		{&Sample{MaxConversations: 2, Conversations: 3, Order: SampleMostRecent}, false},
		{&Sample{Conversations: 1, InWindow: &neg}, false},
		{&Sample{Conversations: 1, Order: "random"}, false},
	} {
		dst := filepath.Join(t.TempDir(), "b.tar.gz")
		m := Manifest{CreatedAt: time.Unix(0, 0).UTC(), Sample: tc.s}
		if err := Write(context.Background(), dst, false, m, nil, nil); err != nil {
			t.Fatal(err)
		}
		_, err := Inspect(dst)
		if (err == nil) != tc.ok {
			t.Errorf("sample %+v: err = %v", tc.s, err)
		}
	}
}
