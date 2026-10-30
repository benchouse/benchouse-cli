package sigma

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkSF(t *testing.T, w *warehouse, in map[string]string, opts agent.Options) (agent.CheckResult, error) {
	t.Helper()
	srv := w.snowflakeServer()
	t.Cleanup(srv.Close)
	useSnowflake(t, srv)
	inputs := agent.Inputs(sfInputs())
	for k, v := range in {
		inputs[k] = v
	}
	return check(context.Background(), inputs, opts)
}

func TestCheckFacts(t *testing.T) {
	res, err := checkSF(t, newWarehouse(t), map[string]string{"table": ""}, agent.Options{MaxConversations: 2})
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Join(res.Facts, "\n")
	for _, want := range []string{
		"Connected to Snowflake 9.30.1",
		`Sigma AI usage table ANALYTICS.SIGMA."AI_USAGE_ACME": 22 columns, 19 of 19 documented`,
		"Columns not mapped (not collected): custom_col",
		"3 Sigma AI conversations in total: the 2 most recent will be sent; the latest at 2026-10-02 12:05 UTC",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if res.Conversations == nil || *res.Conversations != 3 || len(res.Warnings) != 0 {
		t.Errorf("conversations = %v warnings %v", res.Conversations, res.Warnings)
	}
}

func TestCheckNamesTheSetupStep(t *testing.T) {
	w := newWarehouse(t)
	w.tables = []string{}
	_, err := checkSF(t, w, map[string]string{"table": ""}, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "AI usage logging is not configured: a Sigma admin turns it on under Administration → AI settings") ||
		!strings.Contains(err.Error(), docsURL) || !strings.Contains(err.Error(), "only conversations after that are logged") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckRefusesATableWithoutTheColumns(t *testing.T) {
	w := newWarehouse(t)
	w.cols = []column{{Name: "trace_id", Type: "TEXT"}, {Name: "QUESTION", Type: "TEXT"}}
	_, err := checkSF(t, w, nil, agent.Options{})
	if err == nil || !strings.Contains(err.Error(), "lacks input, trace_created_at_utc") {
		t.Errorf("err = %v", err)
	}
}

func TestCheckWarnsOnSeveralTablesAndAnEmptyWindow(t *testing.T) {
	w := newWarehouse(t)
	w.tables = []string{"AI_USAGE_ACME", "AI_USAGE_OLD"}
	w.rows = nil
	res, err := checkSF(t, w, map[string]string{"table": ""}, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	warn := strings.Join(res.Warnings, "\n")
	if !strings.Contains(warn, "several AI usage tables (AI_USAGE_ACME, AI_USAGE_OLD)") || !strings.Contains(warn, "no AI usage logged") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestVerifyFields(t *testing.T) {
	w := newWarehouse(t)
	srv := w.databricksServer()
	defer srv.Close()
	in := agent.Inputs{"platform": "databricks", "host": srv.URL, "token": "dapi-tok", "warehouse-id": "wh1", "catalog": "main", "schema": "sigma"}
	ctx := context.Background()
	if f, err := verifyToken(ctx, in); err != nil || !strings.Contains(f, "signed in to "+srv.URL+" as ana@example.com") {
		t.Errorf("token: %q %v", f, err)
	}
	if f, err := verifyDBXWarehouse(ctx, in); err != nil || f != "SQL warehouse Serverless (RUNNING)" {
		t.Errorf("warehouse: %q %v", f, err)
	}
	if f, err := verifyTable(ctx, in); err != nil || !strings.Contains(f, "`main`.`sigma`.`AI_USAGE_ACME`: 22 columns") {
		t.Errorf("table: %q %v", f, err)
	}
	if l, err := tableLookup(ctx, in); err != nil || len(l) != 2 || l[1][0] != "AI_USAGE_ACME" {
		t.Errorf("lookup: %v %v", l, err)
	}
	bad := agent.Inputs{"platform": "databricks", "host": srv.URL, "token": "wrong"}
	if _, err := verifyToken(ctx, bad); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Errorf("bad token: %v", err)
	}
	res, err := check(ctx, in, agent.Options{})
	if err != nil || !strings.Contains(res.Facts[0], "Connected to Databricks at "+srv.URL+" as ana@example.com, SQL warehouse Serverless") {
		t.Errorf("check: %v %v", res.Facts, err)
	}
}

func TestValidate(t *testing.T) {
	a, err := agent.Get("sigma")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Validate(agent.Inputs{"platform": "databricks", "host": "https://dbc-1.cloud.databricks.com", "token": "t",
		"warehouse-id": "wh1", "catalog": "main", "schema": "sigma"}); err != nil {
		t.Errorf("databricks: %v", err)
	}
	if err := a.Validate(agent.Inputs{"platform": "snowflake", "account": "acme-x", "user": "u", "token": "t",
		"database": "DB", "schema": "S"}); err != nil {
		t.Errorf("snowflake: %v", err)
	}
	if err := a.Validate(agent.Inputs{"platform": "bigquery", "dataset": "d"}); err == nil || !strings.Contains(err.Error(), "--key-file") {
		t.Errorf("bigquery without a key: %v", err)
	}
	if err := a.Validate(agent.Inputs{"platform": "snowflake", "account": "acme-x", "user": "u", "token": "t",
		"database": "DB", "schema": "S", "table": "x`; drop"}); err == nil {
		t.Error("a table name with SQL in it passed")
	}
	if _, err := locationOf(agent.Inputs{"platform": "snowflake", "database": "my-db", "schema": "S"}); err == nil {
		t.Error("a Snowflake database that needs quoting passed")
	}
}
