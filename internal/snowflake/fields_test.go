package snowflake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// fakeAccount answers the field checks' statements; the PAT "bad" is
// refused, and the session has a warehouse only when one is asked for.
func fakeAccount(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "bad") {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"code":"390303","message":"Invalid OAuth access token."}`))
			return
		}
		var req struct{ Statement, Role, Warehouse string }
		json.NewDecoder(r.Body).Decode(&req)
		s := func(v string) *string { return &v }
		var cols []string
		var data [][]*string
		switch req.Statement {
		case SessionStmt:
			role, wh := "PUBLIC", (*string)(nil)
			if req.Role != "" {
				role = req.Role
			}
			if req.Warehouse != "" {
				wh = s(req.Warehouse)
			}
			cols, data = []string{"ACCOUNT", "USR", "ROLE", "WAREHOUSE", "VERSION"}, [][]*string{{s("ACME"), s("READER"), s(role), wh, s("9.30.1")}}
		case RolesStmt:
			cols, data = []string{"ROLES"}, [][]*string{{s(`["PUBLIC","ANALYST"]`)}}
		case WarehousesStmt:
			cols = []string{"name", "state", "type", "size"}
			data = [][]*string{{s("WH_XS"), s("SUSPENDED"), s("STANDARD"), s("X-Small")}, {s("WH_L"), s("STARTED"), s("STANDARD"), s("Large")}}
		default:
			t.Errorf("unexpected statement %q", req.Statement)
		}
		rt := []map[string]string{}
		for _, c := range cols {
			rt = append(rt, map[string]string{"name": c})
		}
		json.NewEncoder(w).Encode(map[string]any{"resultSetMetaData": map[string]any{"rowType": rt}, "data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFieldChecks(t *testing.T) {
	srv := fakeAccount(t)
	connect := func(in agent.Inputs) (*Client, error) {
		c := New(Config{Account: "acme", User: "reader", Role: in.Get("role"), Warehouse: in.Get("warehouse")}, in.Get("token"))
		c.BaseURL = srv.URL
		return c, nil
	}
	ctx := context.Background()
	in := agent.Inputs{"token": "good"}
	fact, err := VerifySignIn(connect)(ctx, in)
	if err != nil || fact != "Signed in to Snowflake account ACME as READER (role PUBLIC, no default warehouse)" {
		t.Errorf("sign in: %q, %v", fact, err)
	}
	if _, err := VerifySignIn(connect)(ctx, agent.Inputs{"token": "bad"}); err == nil ||
		!strings.Contains(err.Error(), "could not connect to Snowflake") || strings.Contains(err.Error(), "bad") {
		t.Errorf("refused token: %v", err)
	}
	roles, err := RoleLookup(connect)(ctx, in)
	if err != nil || len(roles) != 3 || roles[0][0] != "" || roles[2] != [2]string{"ANALYST", "ANALYST"} {
		t.Errorf("roles = %v, %v", roles, err)
	}
	whs, err := WarehouseLookup(connect)(ctx, in)
	if err != nil || len(whs) != 3 || whs[1] != [2]string{"WH_XS", "WH_XS (SUSPENDED, X-Small)"} {
		t.Errorf("warehouses = %v, %v", whs, err)
	}
	// The user's default warehouse, when there is none, is refused with
	// the grant to give.
	if _, err := VerifyWarehouse(connect)(ctx, in); err == nil || !strings.Contains(err.Error(), "DEFAULT_WAREHOUSE") {
		t.Errorf("no warehouse: %v", err)
	}
	in["role"], in["warehouse"] = "ANALYST", "WH_L"
	if fact, err := VerifyWarehouse(connect)(ctx, in); err != nil || fact != "Statements will run as role ANALYST on warehouse WH_L" {
		t.Errorf("warehouse: %q, %v", fact, err)
	}
}

func TestValidAccount(t *testing.T) {
	for _, ok := range []string{"myorg-myaccount", "https://myorg-myaccount.snowflakecomputing.com/", "xy12345.eu-west-1.aws"} {
		if err := ValidAccount(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"my org", "evil.tld/x", "a@b"} {
		if ValidAccount(bad) == nil {
			t.Errorf("%q passed", bad)
		}
	}
}
