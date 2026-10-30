package snowflake

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Helpers for an agent's Check.

// SessionStmt names the session a statement runs in. The CURRENT_*
// functions need no warehouse.
const SessionStmt = `SELECT CURRENT_ACCOUNT() AS ACCOUNT, CURRENT_USER() AS USR, CURRENT_ROLE() AS ROLE, ` +
	`CURRENT_WAREHOUSE() AS WAREHOUSE, CURRENT_VERSION() AS VERSION`

// Session is who and where statements run as.
type Session struct {
	Account, User, Role, Warehouse, Version string
}

// ReadSession runs SessionStmt.
func (c *Client) ReadSession(ctx context.Context) (Session, error) {
	rows, err := c.Query(ctx, SessionStmt)
	if err != nil {
		return Session{}, err
	}
	if len(rows) == 0 {
		return Session{}, fmt.Errorf("snowflake returned no session row")
	}
	get := func(k string) string {
		if v := rows[0][k]; v != nil {
			return *v
		}
		return ""
	}
	return Session{Account: get("account"), User: get("usr"), Role: get("role"), Warehouse: get("warehouse"), Version: get("version")}, nil
}

// Fact renders the session for a CheckResult, e.g. "Connected to Snowflake
// 9.30.1, account ACME, as READER with role ANALYST on warehouse WH".
func (s Session) Fact() string {
	out := "Connected to Snowflake"
	if s.Version != "" {
		out += " " + s.Version
	}
	if s.Account != "" {
		out += ", account " + s.Account
	}
	if s.User != "" {
		out += ", as " + s.User
	}
	if s.Role != "" {
		out += " with role " + s.Role
	}
	if s.Warehouse != "" {
		out += " on warehouse " + s.Warehouse
	}
	return out
}

// NoWarehouse is the error for a session without a warehouse to run on.
func (s Session) NoWarehouse() error {
	user := s.User
	if user == "" {
		user = "<user>"
	}
	return fmt.Errorf("no warehouse to run on: give --warehouse, or set one for the user "+
		"(ALTER USER %s SET DEFAULT_WAREHOUSE = <warehouse>) and grant its role USAGE on it", user)
}

// Columns runs stmt (typically wrapped with LIMIT 0) and returns its column
// names, lowercased: the SQL API reports them even for an empty result.
func (c *Client) Columns(ctx context.Context, stmt string) ([]string, error) {
	cols, _, err := c.query(ctx, stmt)
	return cols, err
}

// Count runs a statement returning one row with a column N.
func (c *Client) Count(ctx context.Context, stmt string) (int64, error) {
	rows, err := c.Query(ctx, stmt)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 || rows[0]["n"] == nil {
		return 0, fmt.Errorf("the count returned no row")
	}
	return strconv.ParseInt(strings.TrimSpace(*rows[0]["n"]), 10, 64)
}

// SignedIn renders the session for a field check: "Signed in to Snowflake
// account ACME as READER (role ANALYST, warehouse WH)".
func (s Session) SignedIn() string {
	out := "Signed in to Snowflake"
	if s.Account != "" {
		out += " account " + s.Account
	}
	if s.User != "" {
		out += " as " + s.User
	}
	var ctx []string
	if s.Role != "" {
		ctx = append(ctx, "role "+s.Role)
	}
	if s.Warehouse != "" {
		ctx = append(ctx, "warehouse "+s.Warehouse)
	} else {
		ctx = append(ctx, "no default warehouse")
	}
	return out + " (" + strings.Join(ctx, ", ") + ")"
}

// RolesStmt lists the account roles the user can use: those granted to it
// and every role below them. A context function: no warehouse needed.
const RolesStmt = `SELECT CURRENT_AVAILABLE_ROLES() AS ROLES`

// Roles runs RolesStmt and returns the role names, sorted as returned.
func (c *Client) Roles(ctx context.Context) ([]string, error) {
	rows, err := c.Query(ctx, RolesStmt)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || rows[0]["roles"] == nil {
		return nil, fmt.Errorf("snowflake returned no roles")
	}
	var roles []string
	if err := json.Unmarshal([]byte(*rows[0]["roles"]), &roles); err != nil {
		return nil, fmt.Errorf("read the role list: %w", err)
	}
	return roles, nil
}

// WarehousesStmt lists the warehouses the session's role has a privilege
// on. SHOW needs no running warehouse.
const WarehousesStmt = `SHOW WAREHOUSES`

// Warehouse is one row of SHOW WAREHOUSES.
type Warehouse struct{ Name, State, Size string }

// Warehouses runs WarehousesStmt.
func (c *Client) Warehouses(ctx context.Context) ([]Warehouse, error) {
	rows, err := c.Query(ctx, WarehousesStmt)
	if err != nil {
		return nil, err
	}
	get := func(r map[string]*string, k string) string {
		if v := r[k]; v != nil {
			return *v
		}
		return ""
	}
	out := make([]Warehouse, 0, len(rows))
	for _, r := range rows {
		if n := get(r, "name"); n != "" {
			out = append(out, Warehouse{Name: n, State: get(r, "state"), Size: get(r, "size")})
		}
	}
	return out, nil
}

// Label is how a warehouse is shown in a pick list: "WH (STARTED, X-Small)".
func (w Warehouse) Label() string {
	var d []string
	for _, x := range []string{w.State, w.Size} {
		if x != "" {
			d = append(d, x)
		}
	}
	if len(d) == 0 {
		return w.Name
	}
	return w.Name + " (" + strings.Join(d, ", ") + ")"
}
