package sqliteuri

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// A file name holding ?, #, % or spaces stays a file name: it cannot add a
// URI parameter, and the file opens read-only.
func TestDSNKeepsOddFileNamesAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"plain.db", "a b.db", "q?mode=rw.db", "hash#x.db", "pct%41.db", "amp&immutable=0.db"} {
		// Build the fixture under a plain name, then rename it: the writer is
		// the test, not the code under test.
		tmp := filepath.Join(dir, "fixture.db")
		w, err := sql.Open("sqlite", tmp)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Exec(`CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('` + name + `')`); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		w.Close()
		p := filepath.Join(dir, name)
		if err := os.Rename(tmp, p); err != nil {
			t.Fatal(err)
		}

		for _, immutable := range []bool{false, true} {
			dsn, err := DSN(p, immutable)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(dsn, "?") != 1 || !strings.HasSuffix(dsn, "?mode=ro") && !strings.HasSuffix(dsn, "?mode=ro&immutable=1") {
				t.Errorf("%s: dsn = %s", name, dsn)
			}
			db, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatal(err)
			}
			var v string
			if err := db.QueryRow(`SELECT v FROM t`).Scan(&v); err != nil || v != name {
				t.Errorf("%s (immutable=%v): read %q, %v", name, immutable, v, err)
			}
			if _, err := db.Exec(`INSERT INTO t VALUES ('x')`); err == nil {
				t.Errorf("%s (immutable=%v): wrote through a read-only DSN", name, immutable)
			}
			db.Close()
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "q")); err == nil {
		t.Error("a file named after the part before ? was created")
	}
}

func TestDSNMissingFileIsNotCreated(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.db")
	dsn, _ := DSN(p, true)
	db, _ := sql.Open("sqlite", dsn)
	defer db.Close()
	if err := db.Ping(); err == nil {
		t.Error("opened a file that does not exist")
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("opening created the file")
	}
}

func TestWALNote(t *testing.T) {
	p := filepath.Join(t.TempDir(), "db.sqlite")
	if WALNote(p, "nao") != "" {
		t.Error("note without a WAL")
	}
	os.WriteFile(p+"-wal", nil, 0o600)
	if WALNote(p, "nao") != "" {
		t.Error("note for an empty WAL")
	}
	os.WriteFile(p+"-wal", []byte("x"), 0o600)
	if n := WALNote(p, "nao"); !strings.Contains(n, "wal_checkpoint") || !strings.Contains(n, "stop nao") {
		t.Errorf("note = %q", n)
	}
}
