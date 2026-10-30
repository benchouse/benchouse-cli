package agents

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// Every agent verifies its inputs right after the wizard step that asks for
// them: the connection, and the context folder when it has one.
func TestEveryAgentHasChecks(t *testing.T) {
	all := agent.All()
	if len(all) < 9 {
		t.Fatalf("%d agents registered", len(all))
	}
	for _, a := range all {
		if a.Check == nil {
			t.Errorf("%s has no Check", a.ID)
		}
		if a.Context != nil && a.Context.Check == nil {
			t.Errorf("%s has a context folder but no Context.Check", a.ID)
		}
	}
}

// Every agent runs the canary leak suite (package leaktest) against its
// fake: a leak_test.go in its package that calls leaktest.Run with its id.
func TestEveryAgentRunsTheLeakSuite(t *testing.T) {
	covered := map[string]string{}
	paths, err := filepath.Glob(filepath.Join("*", "leak_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	agentRe := regexp.MustCompile(`Agent:\s*"([^"]+)"`)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "leaktest.Run(") {
			continue
		}
		for _, m := range agentRe.FindAllStringSubmatch(string(b), -1) {
			covered[m[1]] = p
		}
	}
	for _, a := range agent.All() {
		if covered[a.ID] == "" {
			t.Errorf("agent %s has no canary leak test: add a leak_test.go to its package calling leaktest.Run(t, leaktest.Case{Agent: %q, ...})", a.ID, a.ID)
		}
	}
}
