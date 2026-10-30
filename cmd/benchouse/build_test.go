package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/bundle"
)

// streamAgent emits n records, one conversation each, then fails if fail.
func streamAgent(n int, fail bool) agent.Agent {
	return agent.Agent{ID: "stream-test", Name: "stream test",
		Collect: func(ctx context.Context, in agent.Inputs, opts agent.Options, emit agent.Emit) (agent.Result, error) {
			res := agent.Result{Sampled: true}
			for i := 0; i < n; i++ {
				if opts.MaxConversations > 0 && i == opts.MaxConversations {
					break
				}
				if err := emit(bundle.Record{SourceKind: "x", RecordKind: "chat", Payload: json.RawMessage(`{"i":1}`)}); err != nil {
					return res, err
				}
				res.Conversations++
			}
			total := int64(n)
			res.InWindow = &total
			if fail {
				return res, errors.New("source went away")
			}
			return res, nil
		}}
}

func TestBuildStreamsRecordsAndWritesTheSample(t *testing.T) {
	dir := t.TempDir()
	since := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	j := &job{agent: streamAgent(25, false), out: filepath.Join(dir, "b.tar.gz"),
		opts: agent.Options{Since: since, MaxConversations: 10}}
	m, _, err := j.buildWith(context.Background(), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if m.Records["chat"] != 10 {
		t.Errorf("records = %v", m.Records)
	}
	s := m.Sample
	if s == nil || s.MaxConversations != 10 || s.Conversations != 10 || s.InWindow == nil || *s.InWindow != 25 ||
		s.Order != bundle.SampleMostRecent || s.Since == nil || !s.Since.Equal(since) {
		t.Fatalf("sample = %+v", s)
	}
	if got := sampleText(*s); got != "10 most recent conversations of 25 since 2026-08-01 (cap 10)" {
		t.Errorf("sample text = %q", got)
	}
	assertOnly(t, dir, "b.tar.gz")
}

func TestBuildRemovesTheRecordsFileWhenCollectionFails(t *testing.T) {
	dir := t.TempDir()
	j := &job{agent: streamAgent(5, true), out: filepath.Join(dir, "b.tar.gz")}
	if _, _, err := j.buildWith(context.Background(), func(string) {}); err == nil {
		t.Fatal("no error")
	}
	assertOnly(t, dir)
}

func TestBuildRefusesAnExistingOutBeforeReading(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "b.tar.gz")
	os.WriteFile(out, []byte("x"), 0o600)
	read := false
	a := agent.Agent{ID: "x", Name: "x", Collect: func(context.Context, agent.Inputs, agent.Options, agent.Emit) (agent.Result, error) {
		read = true
		return agent.Result{}, nil
	}}
	j := &job{agent: a, out: out}
	if _, _, err := j.buildWith(context.Background(), func(string) {}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	if read {
		t.Error("the source was read before the destination was checked")
	}
}

func TestNoCapIsWarned(t *testing.T) {
	var lines []string
	j := &job{agent: streamAgent(1, false), out: filepath.Join(t.TempDir(), "b.tar.gz")}
	if _, _, err := j.buildWith(context.Background(), func(s string) { lines = append(lines, s) }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "no --max-conversations cap") {
		t.Errorf("progress = %v", lines)
	}
}

// assertOnly fails unless dir holds exactly names.
func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Errorf("%s holds %v, want %v", dir, got, names)
	}
}
