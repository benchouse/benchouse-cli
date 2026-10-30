package lightdash

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/pgread"
)

// checkDB answers the check's statements: version, LIMIT 0 probes (which
// fail for tables in denied), the count and the agent names.
type checkDB struct {
	denied map[string]bool
	ran    []string
}

func (c *checkDB) Rows(_ context.Context, stmt string, args ...any) ([]json.RawMessage, error) {
	c.ran = append(c.ran, stmt)
	switch {
	case stmt == pgread.ServerStmt:
		return []json.RawMessage{json.RawMessage(`{"version":"PostgreSQL 16.4 on x86_64-pc-linux-gnu","usr":"benchouse_reader"}`)}, nil
	case strings.HasSuffix(stmt, "LIMIT 0"):
		for table := range c.denied {
			if strings.Contains(stmt, table+" ") {
				return nil, errors.New(`ERROR: permission denied for table ` + table + ` (SQLSTATE 42501)`)
			}
		}
		return nil, nil
	case stmt == countStmt:
		if len(args) != 1 {
			return nil, errors.New("count without its bound")
		}
		return []json.RawMessage{json.RawMessage(`{"n":1204}`)}, nil
	case stmt == threadCountStmt:
		return []json.RawMessage{json.RawMessage(`{"n":5000}`)}, nil
	case stmt == agentsStmt:
		return []json.RawMessage{json.RawMessage(`{"ai_agent_uuid":"a1","name":"Sales analyst"}`)}, nil
	}
	return nil, errors.New("unexpected statement " + stmt)
}

func (c *checkDB) Each(ctx context.Context, stmt string, args []any, fn func(json.RawMessage) error) error {
	rows, err := c.Rows(ctx, stmt, args...)
	for _, r := range rows {
		if err := fn(r); err != nil {
			return err
		}
	}
	return err
}

func (c *checkDB) Close() error { return nil }

func runCheck(t *testing.T, db *checkDB, opts agent.Options) (agent.CheckResult, error) {
	t.Helper()
	old := openDB
	openDB = func(agent.Inputs) (pgread.Querier, error) { return db, nil }
	t.Cleanup(func() { openDB = old })
	a, _ := agent.Get("lightdash")
	in := agent.Inputs{"host": "db.internal", "database": "lightdash", "user": "benchouse_reader", "password": "s3cret-pw"}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	return a.Check(context.Background(), in, opts)
}

func TestCheckReportsServerCountsAndAgents(t *testing.T) {
	db := &checkDB{}
	res, err := runCheck(t, db, agent.Options{Since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), MaxConversations: 2000})
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Join(res.Facts, "\n")
	for _, want := range []string{
		"Connected to PostgreSQL 16.4 at db.internal:5432/lightdash as benchouse_reader",
		"1,204 AI agent questions since 2026-08-01",
		"1 AI agents: Sales analyst",
		"5,000 threads since 2026-08-01: the 2,000 most recent will be sent",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v", res.Warnings)
	}
	probes := 0
	for _, s := range db.ran {
		if strings.HasSuffix(s, "LIMIT 0") {
			probes++
		}
	}
	if want := len(checkTables(agent.Options{})); probes != want {
		t.Errorf("%d probes for %d statements", probes, want)
	}
	if res.Conversations == nil || *res.Conversations != 5000 {
		t.Errorf("conversations = %v", res.Conversations)
	}
}

func TestCheckFailsOnPromptsAndWarnsOnOptional(t *testing.T) {
	res, err := runCheck(t, &checkDB{denied: map[string]bool{"ai_agent_reasoning": true}}, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "GRANT SELECT ON ai_agent_reasoning TO benchouse_reader") {
		t.Errorf("warnings = %v", res.Warnings)
	}
	_, err = runCheck(t, &checkDB{denied: map[string]bool{"ai_prompt": true}}, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "GRANT SELECT ON ai_prompt TO benchouse_reader") ||
		!strings.Contains(err.Error(), "Lightdash's database") {
		t.Errorf("err = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "s3cret-pw") {
		t.Error("the password is in the error")
	}
}
