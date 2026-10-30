package lightdash

import (
	"fmt"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/leaktest"
	"github.com/benchouse/benchouse-cli/internal/pgread"
)

// leakFixture is one Lightdash thread with every canary planted: the
// person as the prompt's author and in the user directory, the row value in
// a runSql result.
func leakFixture() map[string][]string {
	first, last, _ := strings.Cut(leaktest.Name, " ")
	return map[string][]string{
		promptsStmt: {`{"ai_prompt_uuid":"p1","ai_thread_uuid":"t1","created_at":"2026-10-02T12:00:00+00:00",
		  "created_by_user_uuid":"` + leaktest.Email + `","prompt":"Order codes?","response":"One order.",
		  "responded_at":"2026-10-02T12:00:09+00:00","turn_position":1,
		  "thread":{"ai_thread_uuid":"t1","agent_uuid":"a1","created_from":"web_app","title":"Orders"}}`},
		toolCallsStmt: {`{"ai_prompt_uuid":"p1","tool_call_id":"c3","tool_name":"runSql","tool_args":{"sql":"SELECT code FROM orders"},"created_at":"2026-10-02T12:00:05+00:00"}`},
		toolResultsStmt: {`{"ai_prompt_uuid":"p1","tool_call_id":"c3","tool_name":"runSql","result":"code\n` + leaktest.Row +
			`","metadata":{"status":"success","rowCount":1},"created_at":"2026-10-02T12:00:06+00:00"}`},
		compiledStmt:  {},
		reasoningStmt: {},
		agentsStmt:    {`{"ai_agent_uuid":"a1","name":"Sales analyst"}`},
		usersStmt: {`{"user_uuid":"` + leaktest.Email + `","first_name":"` + first + `","last_name":"` + last +
			`","email":"` + leaktest.Email + `"}`},
	}
}

// fakeOpen serves leakFixture; a host starting with "down." fails the way
// a refused Postgres login does, naming the host, user and password.
func fakeOpen(t *testing.T) {
	openDB = func(in agent.Inputs) (pgread.Querier, error) {
		if strings.HasPrefix(in.Get("host"), "down.") {
			return nil, fmt.Errorf("failed to connect to `host=%s user=%s database=lightdash`: password %s rejected: "+
				"FATAL: password authentication failed for user %q (SQLSTATE 28P01)", in.Get("host"), in.Get("user"), in.Get("password"), in.Get("user"))
		}
		return &fakeDB{t: t, data: leakFixture(), missing: map[string]bool{}}, nil
	}
	t.Cleanup(func() { openDB = func(in agent.Inputs) (pgread.Querier, error) { return pgread.Open(in) } })
}

func TestNoCanaryLeaks(t *testing.T) {
	fakeOpen(t)
	in := func(host string) agent.Inputs {
		return agent.Inputs{"host": host, "database": "lightdash", "user": leaktest.Email, "password": leaktest.Secret}
	}
	leaktest.Run(t, leaktest.Case{
		Agent:  "lightdash",
		Inputs: func(*testing.T) agent.Inputs { return in(leaktest.Host) },
		Rows:   true,
		People: true,
		Fail:   func(*testing.T) agent.Inputs { return in("down." + leaktest.Host) },
	})
}
