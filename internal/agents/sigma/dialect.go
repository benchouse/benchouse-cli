package sigma

import (
	"fmt"
	"strings"
	"time"
)

// dialect is the SQL one warehouse speaks: quoting, placeholders, how a row
// becomes one JSON column, and where its information schema lives. Every
// statement the reader runs is built here, so Plan and Collect cannot
// disagree.
type dialect interface {
	// quote quotes an identifier exactly as spelled.
	quote(id string) string
	// ph is the i-th (from 1) bound parameter.
	ph(i int) string
	// table is the AI usage table's full reference.
	table(loc location, name string) string
	// tables lists AI_USAGE_ tables in the location (named: one table,
	// case-insensitively), as rows {schema, name}; args are its bindings.
	tables(loc location, named string) (stmt string, args []string)
	// columns lists a table's columns as rows {name, type}.
	columns(loc location, schema, table string) (stmt string, args []string)
	// jsonOf wraps a SELECT so each row comes back as one JSON object.
	jsonOf(inner string) string
	// jsonFrom selects every column of from, as one JSON object per row.
	jsonFrom(from, where string) string
	// since is the window condition on the timestamp column col of type typ
	// ("" when not discovered: a timestamp is assumed).
	since(col, typ string, t time.Time) string
	// text reports whether a column type is a string type.
	text(typ string) bool
}

// location is where the table lives: Snowflake database.schema, Databricks
// catalog.schema, BigQuery project.dataset.
type location struct{ db, schema string }

func (l location) String() string { return l.db + "." + l.schema }

const tsLayout = "2006-01-02 15:04:05"

// usagePrefix is the documented table name prefix: AI_USAGE_<ORGANIZATION_NAME>.
const usagePrefix = "AI_USAGE_"

func dialectFor(platform string) dialect {
	switch platform {
	case "databricks":
		return databricksSQL{}
	case "bigquery":
		return bigquerySQL{}
	}
	return snowflakeSQL{}
}

// snowflakeSQL: database and schema are unquoted identifiers (case-folded
// by Snowflake), the table and its columns quoted exactly as the
// information schema spells them.
type snowflakeSQL struct{}

func (snowflakeSQL) quote(id string) string { return `"` + strings.ReplaceAll(id, `"`, `""`) + `"` }
func (snowflakeSQL) ph(int) string          { return "?" }
func (d snowflakeSQL) table(loc location, name string) string {
	return loc.db + "." + loc.schema + "." + d.quote(name)
}
func (snowflakeSQL) tables(loc location, named string) (string, []string) {
	stmt := "SELECT TO_JSON(OBJECT_CONSTRUCT('schema', TABLE_SCHEMA, 'name', TABLE_NAME)) AS ROW_JSON FROM " +
		loc.db + ".INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = UPPER(?) AND "
	if named != "" {
		return stmt + "UPPER(TABLE_NAME) = UPPER(?) ORDER BY TABLE_NAME", []string{loc.schema, named}
	}
	return stmt + "STARTSWITH(UPPER(TABLE_NAME), '" + usagePrefix + "') ORDER BY TABLE_NAME", []string{loc.schema}
}
func (snowflakeSQL) columns(loc location, schema, table string) (string, []string) {
	return "SELECT TO_JSON(OBJECT_CONSTRUCT('name', COLUMN_NAME, 'type', DATA_TYPE)) AS ROW_JSON FROM " +
		loc.db + ".INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? ORDER BY ORDINAL_POSITION", []string{schema, table}
}
func (snowflakeSQL) jsonOf(inner string) string {
	return "SELECT TO_JSON(OBJECT_CONSTRUCT_KEEP_NULL(*)) AS ROW_JSON FROM (" + inner + ")"
}
func (snowflakeSQL) jsonFrom(from, where string) string {
	return "SELECT TO_JSON(OBJECT_CONSTRUCT_KEEP_NULL(*)) AS ROW_JSON FROM " + from + where
}
func (d snowflakeSQL) since(col, typ string, t time.Time) string {
	lit := fmt.Sprintf("'%s'::TIMESTAMP_NTZ", t.UTC().Format(tsLayout))
	if d.text(typ) {
		return "TRY_TO_TIMESTAMP(" + col + ") >= " + lit
	}
	return col + " >= " + lit
}
func (snowflakeSQL) text(typ string) bool { return isText(typ) }

// databricksSQL: Unity Catalog names, backquoted; named parameters.
type databricksSQL struct{}

func (databricksSQL) quote(id string) string { return "`" + strings.ReplaceAll(id, "`", "``") + "`" }
func (databricksSQL) ph(i int) string        { return fmt.Sprintf(":p%d", i) }
func (d databricksSQL) table(loc location, name string) string {
	return d.quote(loc.db) + "." + d.quote(loc.schema) + "." + d.quote(name)
}
func (d databricksSQL) tables(loc location, named string) (string, []string) {
	stmt := "SELECT to_json(named_struct('schema', table_schema, 'name', table_name)) AS row_json FROM " +
		d.quote(loc.db) + ".information_schema.tables WHERE lower(table_schema) = lower(:p1) AND "
	if named != "" {
		return stmt + "upper(table_name) = upper(:p2) ORDER BY table_name", []string{loc.schema, named}
	}
	return stmt + "startswith(upper(table_name), '" + usagePrefix + "') ORDER BY table_name", []string{loc.schema}
}
func (d databricksSQL) columns(loc location, schema, table string) (string, []string) {
	return "SELECT to_json(named_struct('name', column_name, 'type', data_type)) AS row_json FROM " +
		d.quote(loc.db) + ".information_schema.columns WHERE table_schema = :p1 AND table_name = :p2 ORDER BY ordinal_position", []string{schema, table}
}
func (databricksSQL) jsonOf(inner string) string {
	return "SELECT to_json(struct(*)) AS row_json FROM (" + inner + ") AS q"
}
func (databricksSQL) jsonFrom(from, where string) string {
	return "SELECT to_json(struct(*)) AS row_json FROM " + from + where
}
func (d databricksSQL) since(col, typ string, t time.Time) string {
	lit := fmt.Sprintf("TIMESTAMP '%s'", t.UTC().Format(tsLayout))
	if d.text(typ) {
		return "try_cast(" + col + " AS TIMESTAMP) >= " + lit
	}
	return col + " >= " + lit
}
func (databricksSQL) text(typ string) bool { return isText(typ) }

// bigquerySQL: GoogleSQL, project.dataset.table in one backquoted path;
// positional parameters. loc.db is the project, loc.schema the dataset.
type bigquerySQL struct{}

func (bigquerySQL) quote(id string) string { return "`" + id + "`" } // names are validated: no backquotes
func (bigquerySQL) ph(int) string          { return "?" }
func (bigquerySQL) table(loc location, name string) string {
	return "`" + loc.db + "." + loc.schema + "." + name + "`"
}
func (bigquerySQL) tables(loc location, named string) (string, []string) {
	stmt := "SELECT TO_JSON_STRING(STRUCT(table_schema AS schema, table_name AS name)) AS row_json FROM `" +
		loc.db + "." + loc.schema + "`.INFORMATION_SCHEMA.TABLES WHERE "
	if named != "" {
		return stmt + "UPPER(table_name) = UPPER(?) ORDER BY table_name", []string{named}
	}
	return stmt + "STARTS_WITH(UPPER(table_name), '" + usagePrefix + "') ORDER BY table_name", nil
}
func (bigquerySQL) columns(loc location, _, table string) (string, []string) {
	return "SELECT TO_JSON_STRING(STRUCT(column_name AS name, data_type AS type)) AS row_json FROM `" +
		loc.db + "." + loc.schema + "`.INFORMATION_SCHEMA.COLUMNS WHERE table_name = ? ORDER BY ordinal_position", []string{table}
}
func (bigquerySQL) jsonOf(inner string) string {
	return "SELECT TO_JSON_STRING(q) AS row_json FROM (" + inner + ") AS q"
}
func (bigquerySQL) jsonFrom(from, where string) string {
	return "SELECT TO_JSON_STRING(t) AS row_json FROM " + from + " AS t" + where
}
func (d bigquerySQL) since(col, typ string, t time.Time) string {
	u := t.UTC().Format(tsLayout)
	switch {
	case d.text(typ):
		return "SAFE_CAST(" + col + " AS TIMESTAMP) >= TIMESTAMP '" + u + "+00'"
	case strings.EqualFold(typ, "DATETIME"):
		return col + " >= DATETIME '" + u + "'"
	}
	return col + " >= TIMESTAMP '" + u + "+00'"
}
func (bigquerySQL) text(typ string) bool { return isText(typ) }

func isText(typ string) bool {
	t := strings.ToUpper(strings.TrimSpace(typ))
	return t == "TEXT" || t == "STRING" || strings.HasPrefix(t, "VARCHAR") || strings.HasPrefix(t, "STRING(")
}

func placeholders(d dialect, n int) string {
	if n < 1 {
		n = 1
	}
	ps := make([]string, n)
	for i := range ps {
		ps[i] = d.ph(i + 1)
	}
	return strings.Join(ps, ", ")
}
