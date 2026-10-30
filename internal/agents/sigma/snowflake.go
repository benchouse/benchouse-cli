package sigma

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/snowflake"
	"github.com/benchouse/benchouse-cli/internal/snowflake/sfauth"
)

// newSnowflake is replaced in tests to point at a fake SQL API.
var newSnowflake = snowflake.New

// connectSF builds the SQL API client the way the Cortex reader does: a PAT,
// or the private key file read for key-pair auth. Sessions run in UTC, so a
// zoneless trace_created_at_utc compares as UTC.
func connectSF(in agent.Inputs) (*snowflake.Client, error) {
	secret := in.Get("token")
	if isKeyPair(in) {
		b, err := os.ReadFile(in.Get("private-key"))
		if err != nil {
			return nil, fmt.Errorf("read private key: %w", err)
		}
		secret = string(b)
	}
	if err := sfauth.Validate(in.Get("auth"), in.Get("account"), in.Get("user"), secret); err != nil {
		return nil, err
	}
	cl := newSnowflake(snowflake.Config{
		Account: in.Get("account"), User: in.Get("user"), Role: in.Get("role"),
		Warehouse: in.Get("warehouse"), Auth: in.Get("auth"),
	}, secret)
	cl.TimeZone = "UTC"
	return cl, nil
}

type sfBackend struct {
	cl   *snowflake.Client
	acct string
}

func (b sfBackend) each(ctx context.Context, stmt string, args []string, fn func(map[string]any) error) (int, error) {
	return b.cl.EachJSONArgs(ctx, stmt, args, func(raw json.RawMessage) error {
		m, err := decodeRow(raw)
		if err != nil {
			return err
		}
		return fn(m)
	})
}

func (b sfBackend) signIn(ctx context.Context) (string, error) {
	s, err := b.cl.Connected(ctx)
	if err != nil {
		return "", err
	}
	if s.Warehouse == "" {
		return "", s.NoWarehouse()
	}
	return s.Fact(), nil
}

func (b sfBackend) account() string {
	return strings.ToUpper(strings.TrimSuffix(strings.ToLower(b.acct), ".snowflakecomputing.com"))
}
