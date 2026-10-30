package snowflake

import (
	"context"
	"fmt"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// Field checks for an agent's Snowflake connection fields, built on the
// agent's own connect so they sign in exactly as its Check does.

// Connect builds a client from an agent's inputs.
type Connect func(agent.Inputs) (*Client, error)

// Connected reads the session, with the error every Snowflake check gives
// when it cannot sign in.
func (c *Client) Connected(ctx context.Context) (Session, error) {
	s, err := c.ReadSession(ctx)
	if err != nil {
		return s, fmt.Errorf("could not connect to Snowflake: %s", agent.Clip(err))
	}
	return s, nil
}

// VerifySignIn checks the token or key signs in, for the secret's field.
func VerifySignIn(connect Connect) func(context.Context, agent.Inputs) (string, error) {
	return func(ctx context.Context, in agent.Inputs) (string, error) {
		cl, err := connect(in)
		if err != nil {
			return "", err
		}
		s, err := cl.Connected(ctx)
		if err != nil {
			return "", err
		}
		return s.SignedIn(), nil
	}
}

// RoleLookup lists the roles the user can use, after the user's default.
func RoleLookup(connect Connect) func(context.Context, agent.Inputs) ([][2]string, error) {
	return func(ctx context.Context, in agent.Inputs) ([][2]string, error) {
		cl, err := connect(in)
		if err != nil {
			return nil, err
		}
		roles, err := cl.Roles(ctx)
		if err != nil {
			return nil, err
		}
		out := [][2]string{{"", "the user's default role"}}
		for _, r := range roles {
			out = append(out, [2]string{r, r})
		}
		return out, nil
	}
}

// WarehouseLookup lists the warehouses the chosen role can use, after the
// user's default.
func WarehouseLookup(connect Connect) func(context.Context, agent.Inputs) ([][2]string, error) {
	return func(ctx context.Context, in agent.Inputs) ([][2]string, error) {
		cl, err := connect(in)
		if err != nil {
			return nil, err
		}
		whs, err := cl.Warehouses(ctx)
		if err != nil {
			return nil, err
		}
		out := [][2]string{{"", "the user's default warehouse"}}
		for _, w := range whs {
			out = append(out, [2]string{w.Name, w.Label()})
		}
		return out, nil
	}
}

// VerifyWarehouse signs in with the chosen role and warehouse and checks
// there is a warehouse to run on, as the agent's Check requires.
func VerifyWarehouse(connect Connect) func(context.Context, agent.Inputs) (string, error) {
	return func(ctx context.Context, in agent.Inputs) (string, error) {
		cl, err := connect(in)
		if err != nil {
			return "", err
		}
		s, err := cl.Connected(ctx)
		if err != nil {
			return "", err
		}
		if s.Warehouse == "" {
			return "", s.NoWarehouse()
		}
		return fmt.Sprintf("Statements will run as role %s on warehouse %s", s.Role, s.Warehouse), nil
	}
}
