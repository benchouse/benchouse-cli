package nao

import (
	"context"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

func TestVerifyConnSQLite(t *testing.T) {
	fact, err := verifyConn(context.Background(), agent.Inputs{"storage": "sqlite", "sqlite-path": fixture(t, 1)})
	if err != nil || !strings.HasPrefix(fact, "Opened nao's sqlite database db.sqlite (SQLite 3.") {
		t.Errorf("fact %q, err %v", fact, err)
	}
}
