package pgread

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

type fakeQ struct{ err error }

func (f fakeQ) Rows(context.Context, string, ...any) ([]json.RawMessage, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []json.RawMessage{json.RawMessage(`{"version":"PostgreSQL 16.2 on aarch64","usr":"reader"}`)}, nil
}
func (fakeQ) Each(context.Context, string, []any, func(json.RawMessage) error) error { return nil }
func (fakeQ) Close() error                                                           { return nil }

func TestVerifyPassword(t *testing.T) {
	in := agent.Inputs{"host": "db", "port": "5432", "database": "app", "user": "reader", "password": "s3cret"}
	var verify func(context.Context, agent.Inputs) (string, error)
	for _, f := range Fields("X", func(agent.Inputs) (Querier, error) { return fakeQ{}, nil }) {
		if f.Key == "password" {
			verify = f.Verify
		}
	}
	if fact, err := verify(context.Background(), in); err != nil || fact != "Connected to PostgreSQL 16.2 at db:5432/app as reader" {
		t.Errorf("fact %q, err %v", fact, err)
	}
	bad := Verify(func(agent.Inputs) (Querier, error) {
		return fakeQ{err: errors.New("password authentication failed for user reader")}, nil
	})
	if _, err := bad(context.Background(), in); err == nil || !strings.Contains(err.Error(), "could not connect: password authentication failed") ||
		strings.Contains(err.Error(), "s3cret") {
		t.Errorf("err %v", err)
	}
}

// The TLS choice comes before the password, so its live check connects as
// collecting will.
func TestPasswordComesAfterTLS(t *testing.T) {
	var keys []string
	for _, f := range Fields("X", nil) {
		keys = append(keys, f.Key)
	}
	if got := strings.Join(keys, " "); got != "host port database user sslmode sslrootcert password" {
		t.Errorf("fields = %s", got)
	}
}
