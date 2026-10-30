// Package sqliteuri opens sqlite files read-only by URI.
//
// A file name is put into the URI's path, percent-encoded, so a name holding
// ?, #, % or spaces cannot add or change URI parameters (mode=ro among
// them).
package sqliteuri

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DSN is the modernc.org/sqlite data source name that opens the file at
// path read-only (mode=ro). immutable also tells sqlite the file cannot
// change, so no lock is taken and no -shm file is written: safe to read
// while the application holds the database open, at the cost of ignoring
// any write-ahead log (see WALNote).
func DSN(path string, immutable bool) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("no sqlite file given")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") { // a Windows drive: file:///C:/...
		p = "/" + p
	}
	q := "mode=ro"
	if immutable {
		q += "&immutable=1"
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: q}).String(), nil
}

// WALNote returns a note for the user when the database at path has a
// non-empty write-ahead log, which an immutable open does not read: the most
// recent writes (the newest chats) may then be missing. app names the
// application that writes the file. It returns "" otherwise.
func WALNote(path, app string) string {
	st, err := os.Stat(strings.TrimSpace(path) + "-wal")
	if err != nil || st.Size() == 0 {
		return ""
	}
	return fmt.Sprintf("%s-wal exists: the newest %s chats may still be in it and were not read. "+
		"To include them, stop %s (it folds the log into the database when it closes) or run "+
		"`sqlite3 %s 'PRAGMA wal_checkpoint;'`, then collect again", path, app, app, path)
}
