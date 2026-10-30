// Package sigma reads Sigma's AI usage log (Sigma Assistant and Sigma
// agents), which Sigma writes into the customer's own warehouse.
//
// Once a Sigma admin sets it up (Administration → AI settings → Chat
// history and usage data → Track AI usage → Set up AI usage table), Sigma
// writes one row per span to a table named AI_USAGE_<ORGANIZATION_NAME> on a
// write-enabled connection: Snowflake, Databricks or BigQuery. Nothing is
// logged retroactively. The legacy Assistant usage table ASK_<ORGANIZATION>
// (and its dashboard, removed 2026-09-15) is not read.
//
// Written from https://help.sigmacomputing.com/docs/configure-ai-usage-dashboard
// (read 2026-10-30), whose migration INSERT names the columns: trace_id,
// span_id, parent_id, span_type, trace_name, agent_name, agent_id,
// user_name, session_id, ai_provider, model, tokens_input, tokens_output,
// tags, tool_calls, input, output, metadata, trace_created_at_utc (quoted
// lowercase identifiers), beside Sigma's input-table columns ID, SEQ_NUM,
// ROW_VERSION, UPDATED_BY, UPDATED_AT. The page says "one question is
// treated as one span" for migrated rows, and maps the old question to
// input {question: ...} and the result to output {result: ...}; it lists
// only "generation" as a span_type. The changelog
// https://help.sigmacomputing.com/changelog/2026-09-18 (read 2026-10-30)
// does not describe the table. Column types, the shape of tool_calls and
// metadata, and how spans nest in a trace are not documented: the reader
// discovers the columns from the information schema at run time, maps them
// by name, notes any it does not use, and reads JSON leniently.
//
// Mapping: a conversation is a session_id (the trace_id when the session is
// empty); a turn is a trace: question = the input of its root span (no
// parent_id, else the earliest), answer = the root's output (else the last
// non-tool span's), statements = SQL found in tool calls (a sql/query
// argument that reads as a SELECT or WITH), tool_calls = every other tool
// call and tool span (outputs only with --include-results), model and
// summed tokens, user_name as the user id. Tested against fixtures only.
package sigma

import (
	"context"
	"fmt"
	"regexp"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/agents/genie"
	"github.com/benchouse/benchouse-cli/internal/bundle"
	"github.com/benchouse/benchouse-cli/internal/snowflake"
)

const docsURL = "https://help.sigmacomputing.com/docs/configure-ai-usage-dashboard"

func init() {
	agent.Register(agent.Agent{
		ID:      "sigma",
		Name:    "Sigma",
		Summary: "Sigma Assistant and agents; reads the AI usage table Sigma writes to your warehouse",
		Status:  agent.Experimental,
		Setup:   setup,
		Fields:  fields(),
		Context: nil,
		Plan:    plan,
		Check:   check,
		Collect: collect,
	})
}

const setup = `Sigma logs AI usage to a table in your own warehouse (AI_USAGE_<ORG>) once
a Sigma admin sets it up: Administration → AI settings → Chat history and
usage data → Track AI usage → Set up AI usage table. Only conversations
after that are logged. The CLI only runs SELECTs on that table and the
information schema.

The connection needs:
  - Snowflake:  a role with USAGE on the database, schema and a warehouse,
                and SELECT on the table (PAT or key pair)
  - Databricks: CAN USE on a SQL warehouse, USE CATALOG / USE SCHEMA and
                SELECT on the table (personal access token)
  - BigQuery:   a service account with BigQuery Job User on the project and
                BigQuery Data Viewer on the dataset (a JSON key file; the
                token is requested with the bigquery.readonly scope)`

func isSF(in agent.Inputs) bool  { p := in.Get("platform"); return p == "snowflake" || p == "" }
func isDBX(in agent.Inputs) bool { return in.Get("platform") == "databricks" }
func isBQ(in agent.Inputs) bool  { return in.Get("platform") == "bigquery" }
func isPAT(in agent.Inputs) bool { return isSF(in) && in.Get("auth") != "keypair" }
func isKeyPair(in agent.Inputs) bool {
	return isSF(in) && in.Get("auth") == "keypair"
}

var (
	plainIdent  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,254}$`)
	nameIdent   = regexp.MustCompile(`^[A-Za-z0-9_$-]{1,255}$`)
	projectID   = regexp.MustCompile(`^([a-z0-9][a-z0-9.-]*[a-z0-9]:)?[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	datasetName = regexp.MustCompile(`^[A-Za-z0-9_]{1,1000}$`)
)

func validName(v string) error {
	if !nameIdent.MatchString(v) {
		return fmt.Errorf("give the name only: letters, digits, _, $ or -")
	}
	return nil
}

func validProject(v string) error {
	if !projectID.MatchString(v) {
		return fmt.Errorf("give the project id (e.g. my-project-123), not its name or number")
	}
	return nil
}

func validDataset(v string) error {
	if !datasetName.MatchString(v) {
		return fmt.Errorf("a dataset name holds letters, digits and _ only")
	}
	return nil
}

func fields() []agent.Field {
	return []agent.Field{
		{Key: "platform", Label: "Where does Sigma write its AI usage table?", Choices: []string{"snowflake", "databricks", "bigquery"},
			Help: "the data platform of the write-enabled connection chosen when the AI usage table was set up"},
		// Snowflake, as the Cortex reader asks for it.
		{Key: "account", Label: "Snowflake account identifier", When: isSF, Validate: snowflake.ValidAccount,
			Help: "e.g. myorg-myaccount (the part before .snowflakecomputing.com)"},
		{Key: "user", Label: "Snowflake user", When: isSF},
		{Key: "auth", Label: "Authenticate with", When: isSF, Choices: []string{"pat", "keypair"}, Default: "pat",
			Help: "pat: a programmatic access token; keypair: an unencrypted PEM private key file"},
		// Databricks, as the Genie reader asks for it.
		{Key: "host", Label: "Databricks workspace URL", When: isDBX, Validate: agent.URL(false),
			Help: "e.g. https://adb-1234567890123456.7.azuredatabricks.net or https://dbc-a1b2c3d4-e5f6.cloud.databricks.com"},
		{Key: "token", Label: "Access token", Secret: true, When: func(in agent.Inputs) bool { return isPAT(in) || isDBX(in) },
			Validate: agent.Token, Verify: verifyToken,
			Help: "Snowflake: a programmatic access token; Databricks: a personal access token"},
		{Key: "private-key", Label: "Path to the private key (.p8)", When: isKeyPair,
			Validate: agent.ExistingFile, Verify: snowflake.VerifySignIn(connectSF)},
		{Key: "role", Label: "Role", When: isSF, Optional: true, Lookup: snowflake.RoleLookup(connectSF),
			Help: "leave empty for the user's default role"},
		{Key: "warehouse", Label: "Warehouse", When: isSF, Optional: true, Lookup: snowflake.WarehouseLookup(connectSF),
			Verify: snowflake.VerifyWarehouse(connectSF), Help: "leave empty for the user's default warehouse"},
		{Key: "warehouse-id", Label: "SQL warehouse", When: isDBX, Lookup: genie.Warehouses, Validate: validWarehouse,
			Verify: verifyDBXWarehouse, Help: "the id in the warehouse's connection details, or its HTTP path (/sql/1.0/warehouses/<id>)"},
		// BigQuery.
		{Key: "key-file", Label: "Service account key file (JSON)", When: isBQ, Validate: agent.Chain(agent.ExistingFile, validKeyFile),
			Verify: verifyKeyFile, Help: "a JSON key of a service account with BigQuery Job User and Data Viewer"},
		{Key: "project", Label: "BigQuery project id", When: isBQ, Optional: true, Validate: validProject,
			Help: "the project the dataset is in and jobs run in; leave empty for the key file's project"},
		// Where the table is.
		{Key: "database", Label: "Snowflake database", When: isSF, Validate: validName,
			Help: "the database of the connection's write-back location"},
		{Key: "catalog", Label: "Unity Catalog catalog", When: isDBX, Validate: validName,
			Help: "the catalog of the connection's write-back location"},
		{Key: "schema", Label: "Schema", When: func(in agent.Inputs) bool { return !isBQ(in) }, Validate: validName,
			Help: "the schema the AI usage table was created in"},
		{Key: "dataset", Label: "BigQuery dataset", When: isBQ, Lookup: bqDatasets, Validate: validDataset,
			Help: "the dataset the AI usage table was created in"},
		{Key: "table", Label: "AI usage table", Optional: true, Lookup: tableLookup, Validate: validName, Verify: verifyTable,
			Help: "AI_USAGE_<ORGANIZATION>; leave empty to find it"},
	}
}

// locationOf is where the table lives, from the inputs.
func locationOf(in agent.Inputs) (location, error) {
	switch {
	case isBQ(in):
		p := in.Get("project")
		if p == "" {
			if k, err := readKeyFile(in.Get("key-file")); err == nil {
				p = k.ProjectID
			}
		}
		if err := validProject(p); err != nil {
			return location{db: p, schema: in.Get("dataset")}, fmt.Errorf("--project: %w", err)
		}
		if err := validDataset(in.Get("dataset")); err != nil {
			return location{db: p, schema: in.Get("dataset")}, fmt.Errorf("--dataset: %w", err)
		}
		return location{db: p, schema: in.Get("dataset")}, nil
	case isDBX(in):
		loc := location{db: in.Get("catalog"), schema: in.Get("schema")}
		for k, v := range map[string]string{"catalog": loc.db, "schema": loc.schema} {
			if err := validName(v); err != nil {
				return loc, fmt.Errorf("--%s: %w", k, err)
			}
		}
		return loc, nil
	}
	loc := location{db: in.Get("database"), schema: in.Get("schema")}
	for k, v := range map[string]string{"database": loc.db, "schema": loc.schema} {
		if !plainIdent.MatchString(v) {
			return loc, fmt.Errorf("--%s: give an unquoted Snowflake identifier (letters, digits, _ and $, not starting with a digit)", k)
		}
	}
	return loc, nil
}

func plan(in agent.Inputs, opts agent.Options) []bundle.Query {
	var out []bundle.Query
	for _, q := range planned(in, opts) {
		out = append(out, bundle.Query{Purpose: q.purpose, Statement: q.stmt})
	}
	return out
}

// backend runs a statement whose single column is a JSON object per row,
// streaming the rows. args bind the dialect's placeholders in order.
type backend interface {
	each(ctx context.Context, stmt string, args []string, fn func(map[string]any) error) (int, error)
	// signIn proves the credentials, returning a fact.
	signIn(ctx context.Context) (string, error)
	// account names where the table is, for the bundle's source.
	account() string
}

// open connects to the chosen platform. Nothing is run yet.
func open(in agent.Inputs) (backend, error) {
	switch {
	case isDBX(in):
		cl, err := newDBX(in.Get("host"), in.Get("token"), in.Get("warehouse-id"))
		if err != nil {
			return nil, err
		}
		return cl, nil
	case isBQ(in):
		return newBQ(in)
	}
	cl, err := connectSF(in)
	if err != nil {
		return nil, err
	}
	return sfBackend{cl: cl, acct: in.Get("account")}, nil
}

// verifyToken signs in with the token on the chosen platform.
func verifyToken(ctx context.Context, in agent.Inputs) (string, error) {
	if isDBX(in) {
		cl, err := newDBX(in.Get("host"), in.Get("token"), "")
		if err != nil {
			return "", err
		}
		return cl.tokenWorks(ctx)
	}
	return snowflake.VerifySignIn(connectSF)(ctx, in)
}
