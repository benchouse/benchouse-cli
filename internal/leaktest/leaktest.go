// Package leaktest is the cross-agent canary suite: every agent's tests
// plant the same canaries (a credential, a person, a business result value,
// an internal host name) in that agent's fake source, then Run builds real
// bundles from it through package pipeline, with usage stats captured, and
// fails naming the agent, the canary and where it surfaced.
//
// With the default options (no --include-results, no --include-users,
// people pseudonymised) no canary may appear in the records, the manifest
// (queries, redactions, sources), the progress lines (the reader's notes),
// any bundle entry, or any usage-stats event. Run also checks the switches
// work: --include-results keeps the row value, --anonymize-users=false
// keeps the person (with --include-users, the directory's), and none of
// them ever lets the credential through.
//
// Every registered agent must have a leak_test.go calling Run; the agents
// package test fails for one that does not.
package leaktest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/pipeline"
	"github.com/benchouse/benchouse-cli/internal/telemetry"
)

// The canaries. Each is distinctive enough that finding it anywhere is a
// leak, and shaped like the real thing so the masks that would catch the
// real thing get their chance.
const (
	// Secret is the credential: pass it as the agent's password or token,
	// and plant it where the source keeps credentials (a token column, a
	// connection's password). It is GitHub-token shaped.
	Secret = "ghp_CanaryS3cret9b7e1fQz4YtR2mWx8LpAvN0K"
	// Email and Name are a person, planted in the source's identity
	// fields (author, owner, user directory) — not in question text, which
	// is collected unchanged by design.
	Email = "canary.person@canary-corp.test"
	Name  = "Canaryfirst Canarylast"
	// Row is a value in a business result row (what a query returned).
	Row = "CANARY-ROW-4242"
	// Host is an internal host name: plant it where the source names
	// infrastructure (a warehouse connection, an error), and in inputs
	// where the agent takes a host that the fake does not need to resolve.
	Host = "canary-host-7731.corp.internal"
)

// Canaries lists every canary by name.
var Canaries = map[string]string{"Secret": Secret, "Email": Email, "Name": Name, "Row": Row, "Host": Host}

// Case is one agent's leak test.
type Case struct {
	// Agent is the registered agent id.
	Agent string
	// Inputs returns the inputs for a collection from a fake with every
	// canary the source can hold planted. Called once per run; t.Cleanup
	// can stop servers.
	Inputs func(t *testing.T) agent.Inputs
	// Rows: the fake returns Row in a query result, so --include-results
	// must keep it.
	Rows bool
	// People: the fake names Email or Name in identity fields, so
	// --anonymize-users=false must keep one of them.
	People bool
	// Directory: with --include-users the fake's user directory (or
	// per-turn user columns) names Email or Name, so --include-users with
	// --anonymize-users=false must keep one of them.
	Directory bool
	// Fail, if set, returns inputs whose collection fails with an error
	// that mentions canaries (EchoServer helps): the usage stats must not
	// carry them.
	Fail func(t *testing.T) agent.Inputs
	// Context, if set, returns a context folder to include, with canaries
	// planted in files the agent's keep rule should leave out or mask.
	Context func(t *testing.T) string
	// Allow lists canaries (by name, see Canaries) the source cannot avoid
	// under default options, with the reason; keep it empty.
	Allow map[string]string
}

// run is one option set and what must and must not survive it.
type run struct {
	name     string
	opts     agent.Options
	mustNot  []string // canary names
	mustSome []string // at least one of these must appear
}

// Run builds bundles for c under each option set and fails loudly for
// every canary found where it must not be, or missing where a switch must
// keep it.
func Run(t *testing.T, c Case) {
	t.Helper()
	isolate(t)
	stats := captureStats(t)

	all := []string{"Secret", "Email", "Name", "Row", "Host"}
	runs := []run{
		{name: "default options", opts: agent.Options{AnonymizeUsers: true, MaxConversations: agent.DefaultMaxConversations}, mustNot: all},
		{name: "--include-users", opts: agent.Options{AnonymizeUsers: true, IncludeUsers: true, MaxConversations: agent.DefaultMaxConversations},
			mustNot: all},
		{name: "--include-results", opts: agent.Options{AnonymizeUsers: true, IncludeResults: true, MaxConversations: agent.DefaultMaxConversations},
			mustNot: []string{"Secret", "Email", "Name", "Host"}},
		{name: "--anonymize-users=false", opts: agent.Options{MaxConversations: agent.DefaultMaxConversations},
			mustNot: []string{"Secret", "Row", "Host"}},
		{name: "--include-users --anonymize-users=false", opts: agent.Options{IncludeUsers: true, MaxConversations: agent.DefaultMaxConversations},
			mustNot: []string{"Secret", "Row", "Host"}},
	}
	if c.Rows {
		runs[2].mustSome = []string{"Row"}
	}
	if c.People {
		runs[3].mustSome = []string{"Email", "Name"}
	}
	if c.People || c.Directory {
		runs[4].mustSome = []string{"Email", "Name"}
	}

	a, err := agent.Get(c.Agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		in := c.Inputs(t)
		ctxDir := ""
		if c.Context != nil {
			ctxDir = c.Context(t)
		}
		found := build(t, a, in, ctxDir, r.opts)
		for _, name := range r.mustNot {
			if why, ok := c.Allow[name]; ok {
				t.Logf("LEAK ALLOWED: agent %s: canary %s under %s: %s", c.Agent, name, r.name, why)
				continue
			}
			for _, where := range found[name] {
				t.Errorf("LEAK: agent %s: canary %s (%q) found in %s [%s]", c.Agent, name, Canaries[name], where, r.name)
			}
		}
		if len(r.mustSome) > 0 {
			kept := false
			for _, name := range r.mustSome {
				kept = kept || len(found[name]) > 0
			}
			if !kept {
				t.Errorf("SWITCH IGNORED: agent %s: %s should keep %s, but the bundle holds none of them",
					c.Agent, r.name, strings.Join(r.mustSome, " or "))
			}
		}
	}
	if c.Fail != nil {
		in := c.Fail(t)
		telemetry.KnownInputs("cmd", a.Fields, in)
		_, err := pipeline.Build(context.Background(), pipeline.Job{Agent: a, Inputs: in,
			Opts: agent.Options{AnonymizeUsers: true, MaxConversations: agent.DefaultMaxConversations},
			Out:  filepath.Join(t.TempDir(), "fail.tar.gz"), Version: "leaktest"}, func(string) {})
		if err == nil {
			t.Errorf("agent %s: the Fail inputs collected without an error", c.Agent)
		} else {
			telemetry.CollectFailed(a.ID, err)
		}
	}
	telemetry.Close(5 * time.Second)
	raw := stats.raw()
	if len(raw) == 0 {
		t.Errorf("agent %s: no usage stats were captured: the stats check saw nothing", c.Agent)
	}
	for _, name := range all {
		if i := bytes.Index(raw, []byte(Canaries[name])); i >= 0 {
			t.Errorf("LEAK: agent %s: canary %s (%q) found in the usage stats: …%s…", c.Agent, name, Canaries[name], around(raw, i, len(Canaries[name])))
		}
	}
}

// build runs one collection as the CLI does and returns, by canary name,
// every place it surfaced.
func build(t *testing.T, a agent.Agent, in agent.Inputs, ctxDir string, opts agent.Options) map[string][]string {
	t.Helper()
	if err := a.Validate(in); err != nil {
		t.Fatalf("agent %s: inputs: %v", a.ID, err)
	}
	telemetry.KnownInputs("cmd", a.Fields, in)
	var progress []string
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	b, err := pipeline.Build(context.Background(), pipeline.Job{Agent: a, Inputs: in, Opts: opts, CtxDir: ctxDir,
		Out: out, Version: "leaktest"}, func(s string) { progress = append(progress, s) })
	if err != nil {
		telemetry.CollectFailed(a.ID, err)
		t.Fatalf("agent %s: collect: %v", a.ID, err)
	}
	telemetry.Collected(a.ID, b.Stats)

	found := map[string][]string{}
	look := func(where string, data []byte) {
		for name, v := range Canaries {
			if i := bytes.Index(data, []byte(v)); i >= 0 {
				found[name] = append(found[name], fmt.Sprintf("%s: …%s…", where, around(data, i, len(v))))
			}
		}
	}
	// The progress lines (the reader's notes) stay on this machine, so
	// they may name the user's own host, path or account; but they reach
	// terminals and CI logs, so never a credential or a result value.
	lines := []byte(strings.Join(progress, "\n"))
	for _, name := range []string{"Secret", "Row"} {
		if i := bytes.Index(lines, []byte(Canaries[name])); i >= 0 && !(name == "Row" && opts.IncludeResults) {
			found[name] = append(found[name], "progress lines (reader notes): …"+around(lines, i, len(Canaries[name]))+"…")
		}
	}
	m, _ := json.Marshal(b.Manifest)
	look("manifest (queries, redactions, sources)", m)
	entries, err := untar(out)
	if err != nil {
		t.Fatalf("agent %s: read the bundle: %v", a.ID, err)
	}
	for _, e := range entries {
		look("bundle entry "+e.name, e.data)
	}
	return found
}

type entry struct {
	name string
	data []byte
}

func untar(p string) ([]entry, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var out []entry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out = append(out, entry{h.Name, b})
	}
}

func around(b []byte, i, n int) string {
	lo, hi := max(0, i-60), min(len(b), i+n+60)
	return strings.ReplaceAll(string(b[lo:hi]), "\n", "\\n")
}

// isolate points the config directory (the anonymize key, the install id)
// at a temp dir on every OS.
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA", "USERPROFILE"} {
		t.Setenv(k, home)
	}
}

type statsCapture struct {
	mu  sync.Mutex
	buf []byte
}

func (s *statsCapture) raw() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf...)
}

// captureStats starts the usage-stats client against a local capture
// server, the way the CLI would with BENCHOUSE_TELEMETRY_ENDPOINT set.
func captureStats(t *testing.T) *statsCapture {
	t.Helper()
	s := &statsCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.buf = append(s.buf, b...)
		s.buf = append(s.buf, '\n')
		s.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	t.Setenv(telemetry.EnvEndpoint, srv.URL)
	telemetry.Start("leaktest")
	telemetry.SetMode("scripted")
	telemetry.Started("collect")
	t.Cleanup(func() {
		telemetry.Known("cmd", nil)
		telemetry.StartWith(telemetry.Options{})
	})
	return s
}

// EchoBody is what EchoServer answers with: an error naming every canary
// in the shapes real upstream errors use (a quoted user name, an email, a
// host and port, a bearer token, a quoted bad value).
var EchoBody = fmt.Sprintf(`{"error":"access denied for user \"%s\" <%s> connecting to %s:443 with token %s; `+
	`invalid input syntax for type integer: \"%s\""}`, Name, Email, Host, Secret, Row)

// EchoServer starts a server that fails every request with status and
// EchoBody, for a Case's Fail inputs.
func EchoServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, EchoBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}
