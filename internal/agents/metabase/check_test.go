package metabase

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/pgread"
)

type checkDB struct{ denied string }

func (c *checkDB) Rows(_ context.Context, stmt string, args ...any) ([]json.RawMessage, error) {
	switch {
	case stmt == pgread.ServerStmt:
		return []json.RawMessage{json.RawMessage(`{"version":"PostgreSQL 15.8, compiled by gcc","usr":"reader"}`)}, nil
	case strings.HasSuffix(stmt, "LIMIT 0"):
		if c.denied != "" && strings.Contains(stmt, c.denied+" ") {
			return nil, errors.New(`ERROR: relation "` + c.denied + `" does not exist (SQLSTATE 42P01)`)
		}
		return nil, nil
	case stmt == countStmt:
		return []json.RawMessage{json.RawMessage(`{"n":312,"conversations":40}`)}, nil
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

func checkWith(t *testing.T, db *checkDB) (agent.CheckResult, error) {
	t.Helper()
	old := openDB
	openDB = func(agent.Inputs) (pgread.Querier, error) { return db, nil }
	t.Cleanup(func() { openDB = old })
	a, _ := agent.Get("metabase")
	in := agent.Inputs{"database": "metabase", "user": "reader"}
	if err := a.Validate(in); err != nil {
		t.Fatal(err)
	}
	return a.Check(context.Background(), in, agent.Options{MaxConversations: 25})
}

func TestCheck(t *testing.T) {
	res, err := checkWith(t, &checkDB{})
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Join(res.Facts, "\n")
	for _, want := range []string{"Connected to PostgreSQL 15.8 at localhost:5432/metabase as reader", "312 Metabot questions in 40 conversations in total: the 25 most recent will be sent"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	res, err = checkWith(t, &checkDB{denied: "metabot_feedback"})
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "thumbs up and down will be missing") {
		t.Errorf("optional: %v %v", res.Warnings, err)
	}
	_, err = checkWith(t, &checkDB{denied: "metabot_message"})
	if err == nil || !strings.Contains(err.Error(), "metabot_message is not readable") || !strings.Contains(err.Error(), "version 56 or later") {
		t.Errorf("required: %v", err)
	}
}
