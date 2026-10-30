package genie

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/agent"
)

// Live checks of single fields, for the guided run, and their offline
// format checks. Each is a piece of check, which calls the same functions,
// so a field verified while typing cannot fail the full check for another
// reason than the ones it did not cover.

// warehouseInPath finds a warehouse id inside a pasted HTTP path or JDBC
// URL: /sql/1.0/warehouses/<id>, or /sql/1.0/endpoints/<id> on older
// workspaces.
var warehouseInPath = regexp.MustCompile(`/sql/[0-9.]+/(?:warehouses|endpoints)/([^/?#;\s]+)`)

// warehouseID is the id in what the user gave: the id itself, or the
// warehouse's HTTP path (or a JDBC URL holding it), as the connection
// details page shows them. Databricks documents the id only as a string
// (16 hex digits in practice), so nothing more is assumed about it.
func warehouseID(raw string) string {
	raw = strings.TrimSpace(raw)
	if m := warehouseInPath.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	return strings.Trim(raw, "/")
}

func validWarehouse(v string) error {
	if !validID(warehouseID(v)) {
		return errors.New("give the warehouse id (the end of its HTTP path, /sql/1.0/warehouses/<id>) or the HTTP path itself")
	}
	return nil
}

func validSpace(v string) error {
	if !validID(v) {
		return errors.New("give the space id from its URL (/genie/rooms/<id>)")
	}
	return nil
}

// whoami is the check's first request: who the token signs in as.
func whoami(ctx context.Context, cl *client) (string, error) {
	var me struct {
		UserName string `json:"userName"`
	}
	if err := cl.getJSON(ctx, http.MethodGet, mePath, nil, &me); err != nil {
		return "", fmt.Errorf("could not connect to %s: %s. Check the workspace URL and that the token is valid", cl.host, agent.Clip(err))
	}
	return me.UserName, nil
}

func verifyToken(ctx context.Context, in agent.Inputs) (string, error) {
	cl, err := newClient(in.Get("host"), in.Get("token"), "")
	if err != nil {
		return "", err
	}
	user, err := whoami(ctx, cl)
	if err != nil {
		return "", err
	}
	if user == "" {
		return "Token works on " + cl.host, nil
	}
	return "Token works: signed in to " + cl.host + " as " + user, nil
}

type warehouse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	State       string `json:"state"`
	ClusterSize string `json:"cluster_size"`
}

// stopped is the warning for a warehouse that is not running.
func (w warehouse) stopped() string {
	if w.State == "STOPPED" || w.State == "STOPPING" {
		return "the SQL warehouse is stopped: the first statement starts it, which can take a few minutes"
	}
	return ""
}

// getWarehouse reads the configured warehouse.
func getWarehouse(ctx context.Context, cl *client) (warehouse, error) {
	var wh warehouse
	id, err := pathID("warehouse", cl.warehouseID)
	if err != nil {
		return wh, fmt.Errorf("--warehouse-id: %w", err)
	}
	if err := cl.getJSON(ctx, http.MethodGet, warehousePath+id, nil, &wh); err != nil {
		return wh, fmt.Errorf("SQL warehouse %s is not reachable (%s): check the id (the end of its HTTP path, "+
			"/sql/1.0/warehouses/<id>) and that the token's identity has CAN USE on it", cl.warehouseID, agent.Clip(err))
	}
	return wh, nil
}

func verifyWarehouse(ctx context.Context, in agent.Inputs) (string, error) {
	cl, err := newClient(in.Get("host"), in.Get("token"), in.Get("warehouse-id"))
	if err != nil {
		return "", err
	}
	wh, err := getWarehouse(ctx, cl)
	if err != nil {
		return "", err
	}
	fact := fmt.Sprintf("SQL warehouse %s (%s)", wh.Name, wh.State)
	if w := wh.stopped(); w != "" {
		fact += ": " + w
	}
	return fact, nil
}

// maxWarehousePages bounds the list walk, against a server repeating a
// page token.
const maxWarehousePages = 20

// Warehouses lists the SQL warehouses the token can see, for the
// warehouse-id prompt: GET /api/2.0/sql/warehouses.
func Warehouses(ctx context.Context, in agent.Inputs) ([][2]string, error) {
	cl, err := newClient(in.Get("host"), in.Get("token"), "")
	if err != nil {
		return nil, err
	}
	var out [][2]string
	token := ""
	for range maxWarehousePages {
		path := warehousePath[:len(warehousePath)-1]
		if token != "" {
			path += "?page_token=" + url.QueryEscape(token)
		}
		var resp struct {
			Warehouses []warehouse `json:"warehouses"`
			Next       string      `json:"next_page_token"`
		}
		if err := cl.getJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, fmt.Errorf("list SQL warehouses: %s", agent.Clip(err))
		}
		for _, w := range resp.Warehouses {
			if !validID(w.ID) {
				continue
			}
			label := w.Name
			if label == "" {
				label = w.ID
			}
			var detail []string
			for _, d := range []string{w.State, w.ClusterSize} {
				if d != "" {
					detail = append(detail, d)
				}
			}
			if len(detail) > 0 {
				label += " (" + strings.Join(detail, ", ") + ")"
			}
			out = append(out, [2]string{w.ID, label})
		}
		if resp.Next == "" || resp.Next == token {
			break
		}
		token = resp.Next
	}
	return out, nil
}

// getSpace reads one Genie space.
func getSpace(ctx context.Context, cl *client, spaceID string) (string, error) {
	var sp struct {
		Title string `json:"title"`
	}
	if err := cl.getJSON(ctx, http.MethodGet, spacePath+url.PathEscape(spaceID), nil, &sp); err != nil {
		return "", fmt.Errorf("Genie space %s is not readable (%s): check the id (from the space's URL, /genie/rooms/<id>) "+
			"and that the token's identity has CAN VIEW on the space", spaceID, agent.Clip(err))
	}
	return fmt.Sprintf("Genie space %q (%s)", sp.Title, spaceID), nil
}

func verifySpace(ctx context.Context, in agent.Inputs) (string, error) {
	id := in.Get("space-id")
	if id == "" {
		return "", nil // every space: the check counts them
	}
	if err := validSpace(id); err != nil {
		return "", err
	}
	cl, err := newClient(in.Get("host"), in.Get("token"), "")
	if err != nil {
		return "", err
	}
	return getSpace(ctx, cl, id)
}
