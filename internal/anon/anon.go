// Package anon replaces people with pseudonyms in captured records: user
// ids, names and emails become person_<hash>. What people asked and what the
// agent answered is left alone; only the fields that say who someone is
// change.
//
// The hash is an HMAC under a key generated on this machine and never
// written to a bundle, so a pseudonym cannot be reversed by hashing a list of
// likely emails. The same person gets the same pseudonym in every field and
// in every bundle made on this machine, which keeps "how many people asked"
// and "who keeps hitting the same gap" answerable.
package anon

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/bundle"
)

// Redaction is the manifest line for an anonymised bundle.
const Redaction = "people: user ids, names, emails and IP addresses replaced with pseudonyms (person_<hash>) " +
	"under a key that stays on this machine; questions and answers unchanged"

// Hasher pseudonymises values under a secret key.
type Hasher struct{ key []byte }

// New returns a Hasher with the given key. Tests use it; the CLI uses Load.
func New(key []byte) *Hasher { return &Hasher{key: key} }

// Load returns the Hasher for this machine, creating its key on first use in
// the user's config directory with owner-only permissions.
func Load() (*Hasher, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return loadFrom(filepath.Join(dir, "benchouse", "anonymize.key"))
}

func loadFrom(p string) (*Hasher, error) {
	if h, err := readKey(p); h != nil || err != nil {
		return h, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	// The key is written whole to a temp file and linked into place: a
	// reader never sees half a key, and two runs racing to create it end up
	// with one key (the link fails for the loser, which reads the winner's).
	tmp, err := os.CreateTemp(filepath.Dir(p), ".anonymize-*.key")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(key); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tmp.Name(), p); err != nil {
		if errors.Is(err, fs.ErrExist) {
			h, err := readKey(p)
			if h == nil && err == nil {
				err = fmt.Errorf("%s vanished while it was being created; run again", p)
			}
			return h, err
		}
		// No hard links here (FAT, some network drives): write the key
		// straight to p instead. O_EXCL still lets only one run create it.
		f, cerr := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(cerr, fs.ErrExist) {
			return readKey(p)
		}
		if cerr != nil {
			return nil, fmt.Errorf("create %s: %w (hard link: %v)", p, cerr, err)
		}
		if _, werr := f.Write(key); werr != nil {
			f.Close()
			os.Remove(p)
			return nil, fmt.Errorf("write %s: %w", p, werr)
		}
		if cerr := f.Close(); cerr != nil {
			os.Remove(p)
			return nil, fmt.Errorf("write %s: %w", p, cerr)
		}
	}
	return New(key), nil
}

// readKey returns the key at p, nil and no error when there is none yet, or
// an error naming the file when it is not a key this CLI wrote.
func readKey(p string) (*Hasher, error) {
	key, err := os.ReadFile(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", p, err)
	case len(key) != 32:
		return nil, fmt.Errorf("%s holds %d bytes, not a 32-byte key; delete it to make a new one "+
			"(pseudonyms in new bundles will then differ from earlier ones)", p, len(key))
	}
	return New(key), nil
}

// Person returns the pseudonym for v. Case and surrounding space are
// ignored, so Ann@Shop.com and ann@shop.com are one person.
func (h *Hasher) Person(v string) string {
	m := hmac.New(sha256.New, h.key)
	m.Write([]byte(strings.ToLower(strings.TrimSpace(v))))
	return "person_" + hex.EncodeToString(m.Sum(nil))[:16]
}

// personKeys name fields that identify a person, compared lowercased with
// separators removed. "name" alone is not here: it names tables, tools and
// projects far more often than people (directory records are handled whole).
var personKeys = map[string]bool{
	"userid": true, "username": true, "useremail": true, "email": true, "emailaddress": true,
	"user": true, "useruuid": true, "userlogin": true, "loginname": true, "login": true,
	"firstname": true, "lastname": true, "fullname": true, "displayname": true,
	"owner": true, "ownerid": true, "owneremail": true, "createdby": true, "createdbyuseruuid": true,
	"updatedby": true, "requester": true, "requestedby": true, "author": true, "authoremail": true,
	"useridentity": true, "externaluserid": true, "principal": true, "executedby": true, "executedas": true, "runas": true, "runasuser": true,
	// Addresses identify people as well as names do.
	"sourceipaddress": true, "clientip": true, "clientipaddress": true, "ipaddress": true, "remoteaddr": true, "remoteaddress": true,
}

func norm(k string) string {
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(k))
}

// isPerson reports whether key names a person. Dotted attribute keys
// (snow.ai.observability.user.name) count when their last two segments are
// user + name/id/email.
func isPerson(key string) bool {
	k := norm(key)
	if personKeys[k] {
		return true
	}
	// Any key that ends naming a user or an email: executed_as_user_id,
	// owner_email, created_by_user_name, requester_email...
	for _, suffix := range []string{"email", "userid", "username", "useruuid", "userlogin"} {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	parts := strings.Split(strings.ToLower(key), ".")
	if n := len(parts); n >= 2 && parts[n-2] == "user" {
		switch parts[n-1] {
		case "name", "id", "email", "login":
			return true
		}
	}
	return false
}

// Record returns r with people pseudonymised in its payload and native ids.
func (h *Hasher) Record(r bundle.Record) (bundle.Record, error) {
	whole := r.RecordKind == "directory_user" // every string in a directory row is about the person
	dec := json.NewDecoder(bytes.NewReader(r.Payload))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return r, fmt.Errorf("%s record: %w", r.RecordKind, err)
	}
	v = h.walk(v, whole)
	b, err := json.Marshal(v)
	if err != nil {
		return r, err
	}
	out := r
	out.Payload = b
	if len(r.NativeIDs) > 0 {
		out.NativeIDs = make(map[string]string, len(r.NativeIDs))
		for k, id := range r.NativeIDs {
			if whole || isPerson(k) {
				id = h.Person(id)
			}
			out.NativeIDs[k] = id
		}
	}
	return out, nil
}

func (h *Hasher) walk(v any, person bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			t[k] = h.walk(c, person || isPerson(k))
		}
		return t
	case []any:
		for i, c := range t {
			t[i] = h.walk(c, person)
		}
		return t
	case string:
		if person && t != "" {
			return h.Person(t)
		}
		return t
	case json.Number:
		if person {
			return h.Person(t.String())
		}
		return t
	}
	return v
}
