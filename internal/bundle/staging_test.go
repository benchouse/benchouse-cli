package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStagingIsOwnerOnlyOrderedAndRemoved(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "b.tar.gz")
	st, err := NewStagingFor(dst)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(st.f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(st.f.Name()) != dir || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Errorf("staging file %s mode %v, want 0600 beside the bundle", st.f.Name(), fi.Mode().Perm())
	}
	add := func(pairs ...[2]string) {
		for _, p := range pairs {
			if _, err := st.Add(p[0], []byte(p[1]), 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	add([2]string{"dbt/b.sql", "select 2"}, [2]string{"dbt/a/x.sql", "select 1"}, [2]string{"dbt/a.sql", "select 3"})
	st.SortFrom(0)
	add([2]string{"context/z.md", "zz"}, [2]string{"context/a.md", "a"})
	st.SortFrom(3) // a second folder sorts on its own, after the first
	var got []string
	for _, f := range st.Files() {
		got = append(got, f.Path)
	}
	if strings.Join(got, " ") != "dbt/a.sql dbt/a/x.sql dbt/b.sql context/a.md context/z.md" {
		t.Errorf("order = %v", got)
	}
	m := Manifest{CreatedAt: time.Unix(0, 0).UTC(), Files: st.Files()}
	if err := Write(context.Background(), dst, false, m, nil, st); err != nil {
		t.Fatal(err)
	}
	if got := readEntry(t, dst, "dbt/a/x.sql"); got != "select 1" {
		t.Errorf("dbt/a/x.sql = %q", got)
	}
	if _, err := Inspect(dst); err != nil {
		t.Fatal(err)
	}
	// A failed write (dst exists) leaves the staging file to its owner's
	// Close, which removes it.
	if err := Write(context.Background(), dst, false, m, nil, st); err == nil {
		t.Fatal("overwrote without force")
	}
	st.Close()
	st.Close()
	assertNoTemp(t, dir)
	if _, err := st.Add("dbt/c.sql", []byte("x"), 0); err == nil {
		t.Error("Add after Close succeeded")
	}
}

// A collection too large for a bundle stops at the record that passes the
// limit, instead of after reading everything and failing Inspect (seen on a
// nao Postgres with 1,000,000 messages and no --max-conversations).
func TestRecordsStopAtTheBundleLimit(t *testing.T) {
	orig := recordsLimit
	recordsLimit = 4 << 10
	t.Cleanup(func() { recordsLimit = orig })
	r, err := NewRecords(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	payload := json.RawMessage(`{"text":"` + strings.Repeat("x", 900) + `"}`)
	var n int
	for n = 0; n < 100; n++ {
		if err := r.Add(Record{SourceKind: "nao", RecordKind: "nao_message_part", Payload: payload}); err != nil {
			if !errors.Is(err, ErrTooManyRecords) {
				t.Fatal(err)
			}
			break
		}
	}
	if n < 3 || n > 5 {
		t.Errorf("stopped after %d records of ~1 KB under a 4 KB limit", n)
	}
}
