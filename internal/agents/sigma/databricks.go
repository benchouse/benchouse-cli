package sigma

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/netsafe"
)

// dbxClient runs statements on a Databricks SQL warehouse through the SQL
// Statement Execution API (POST /api/2.0/sql/statements), the way the Genie
// reader's client does: a bearer token, inline JSON_ARRAY results, chunk
// links followed one at a time, ids bound as named parameters.
type dbxClient struct {
	host, token, warehouseID string
	http                     *http.Client
}

const (
	dbxMePath        = "/api/2.0/preview/scim/v2/Me"
	dbxWarehousePath = "/api/2.0/sql/warehouses/"
	dbxStatements    = "/api/2.0/sql/statements"
	dbxMaxResponse   = 64 << 20
	dbxMaxChunks     = 10000
)

// dbxPollEvery is the wait between polls of a running statement; tests
// shorten it.
var dbxPollEvery = 5 * time.Second

func newDBX(host, token, warehouse string) (*dbxClient, error) {
	h, err := netsafe.BaseURL(host, false)
	if err != nil {
		return nil, fmt.Errorf("--host: %w", err)
	}
	return &dbxClient{host: h, token: token, warehouseID: dbxWarehouseID(warehouse), http: netsafe.Client(180 * time.Second)}, nil
}

var (
	dbxID          = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	dbxWarehouseIn = regexp.MustCompile(`/sql/[0-9.]+/(?:warehouses|endpoints)/([^/?#;\s]+)`)
)

// dbxWarehouseID is the id in the id itself or a pasted HTTP path.
func dbxWarehouseID(raw string) string {
	raw = strings.TrimSpace(raw)
	if m := dbxWarehouseIn.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	return strings.Trim(raw, "/")
}

func validWarehouse(v string) error {
	if !dbxID.MatchString(dbxWarehouseID(v)) {
		return errors.New("give the warehouse id (the end of its HTTP path, /sql/1.0/warehouses/<id>) or the HTTP path itself")
	}
	return nil
}

func (c *dbxClient) account() string { return c.host }

func (c *dbxClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	if !strings.HasPrefix(path, "/api/") || strings.Contains(path, "//") || strings.Contains(path, "@") ||
		strings.Contains(path, `\`) {
		return nil, fmt.Errorf("refusing request path %.80q", path)
	}
	u, err := url.Parse(c.host + path)
	if err != nil {
		return nil, err
	}
	if base, _ := url.Parse(c.host); base == nil || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil {
		return nil, fmt.Errorf("refusing request path %.80q: it leaves %s", path, c.host)
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "benchouse-cli")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, dbxMaxResponse+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > dbxMaxResponse {
		return nil, fmt.Errorf("databricks answered with more than %d bytes", dbxMaxResponse)
	}
	if resp.StatusCode != http.StatusOK {
		s := string(raw)
		if len(s) > 300 {
			s = s[:300]
		}
		return nil, fmt.Errorf("databricks http %d: %s", resp.StatusCode, s)
	}
	return raw, nil
}

func (c *dbxClient) getJSON(ctx context.Context, method, path string, body, out any) error {
	raw, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

type dbxStatement struct {
	StatementID string `json:"statement_id"`
	Status      struct {
		State string `json:"state"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"status"`
	Result struct {
		DataArray [][]*string `json:"data_array"`
		NextChunk string      `json:"next_chunk_internal_link"`
	} `json:"result"`
}

type dbxParam struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type"`
}

// each runs stmt with args bound as :p1, :p2, ... and calls fn per row,
// one result chunk at a time.
func (c *dbxClient) each(ctx context.Context, stmt string, args []string, fn func(map[string]any) error) (int, error) {
	req := map[string]any{"warehouse_id": c.warehouseID, "statement": stmt, "wait_timeout": "50s",
		"format": "JSON_ARRAY", "disposition": "INLINE"}
	if len(args) > 0 {
		ps := make([]dbxParam, len(args))
		for i, a := range args {
			ps[i] = dbxParam{Name: fmt.Sprintf("p%d", i+1), Value: a, Type: "STRING"}
		}
		req["parameters"] = ps
	}
	var resp dbxStatement
	if err := c.getJSON(ctx, http.MethodPost, dbxStatements, req, &resp); err != nil {
		return 0, err
	}
	id := resp.StatementID
	for i := 0; resp.Status.State == "PENDING" || resp.Status.State == "RUNNING"; i++ {
		if i > 60 {
			return 0, fmt.Errorf("statement %s still running after 5m", id)
		}
		if !dbxID.MatchString(id) {
			return 0, fmt.Errorf("databricks returned a statement id that is not one: %.80q", id)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(dbxPollEvery):
		}
		resp = dbxStatement{}
		if err := c.getJSON(ctx, http.MethodGet, dbxStatements+"/"+url.PathEscape(id), nil, &resp); err != nil {
			return 0, err
		}
		if resp.StatementID != "" {
			id = resp.StatementID
		}
	}
	if resp.Status.State != "SUCCEEDED" {
		return 0, fmt.Errorf("statement %s: %s %s", id, resp.Status.State, resp.Status.Error.Message)
	}
	n := 0
	rows := func(data [][]*string) error {
		for _, r := range data {
			if len(r) == 0 || r[0] == nil {
				continue
			}
			m, err := decodeRow([]byte(*r[0]))
			if err != nil {
				return err
			}
			if err := fn(m); err != nil {
				return err
			}
			n++
		}
		return nil
	}
	if err := rows(resp.Result.DataArray); err != nil {
		return n, err
	}
	for next, i := resp.Result.NextChunk, 0; next != ""; i++ {
		if i >= dbxMaxChunks {
			return n, fmt.Errorf("statement %s: more than %d result chunks", id, dbxMaxChunks)
		}
		var chunk dbxStatement
		if err := c.getJSON(ctx, http.MethodGet, next, nil, &chunk); err != nil {
			return n, fmt.Errorf("chunk: %w", err)
		}
		if err := rows(chunk.Result.DataArray); err != nil {
			return n, err
		}
		next = chunk.Result.NextChunk
	}
	return n, nil
}

func (c *dbxClient) whoami(ctx context.Context) (string, error) {
	var me struct {
		UserName string `json:"userName"`
	}
	if err := c.getJSON(ctx, http.MethodGet, dbxMePath, nil, &me); err != nil {
		return "", fmt.Errorf("could not connect to %s: %s. Check the workspace URL and that the token is valid", c.host, agent.Clip(err))
	}
	return me.UserName, nil
}

func (c *dbxClient) tokenWorks(ctx context.Context) (string, error) {
	user, err := c.whoami(ctx)
	if err != nil {
		return "", err
	}
	if user == "" {
		return "Token works on " + c.host, nil
	}
	return "Token works: signed in to " + c.host + " as " + user, nil
}

type dbxWarehouse struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

func (c *dbxClient) warehouse(ctx context.Context) (dbxWarehouse, error) {
	var wh dbxWarehouse
	if !dbxID.MatchString(c.warehouseID) {
		return wh, errors.New("--warehouse-id: " + "give the warehouse id (the end of its HTTP path, /sql/1.0/warehouses/<id>)")
	}
	if err := c.getJSON(ctx, http.MethodGet, dbxWarehousePath+url.PathEscape(c.warehouseID), nil, &wh); err != nil {
		return wh, fmt.Errorf("SQL warehouse %s is not reachable (%s): check the id and that the token's identity has CAN USE on it",
			c.warehouseID, agent.Clip(err))
	}
	return wh, nil
}

func (w dbxWarehouse) fact() string {
	f := fmt.Sprintf("SQL warehouse %s (%s)", w.Name, w.State)
	if w.State == "STOPPED" || w.State == "STOPPING" {
		f += ": stopped, the first statement starts it, which can take a few minutes"
	}
	return f
}

// signIn checks the token and the warehouse.
func (c *dbxClient) signIn(ctx context.Context) (string, error) {
	user, err := c.whoami(ctx)
	if err != nil {
		return "", err
	}
	wh, err := c.warehouse(ctx)
	if err != nil {
		return "", err
	}
	f := "Connected to Databricks at " + c.host
	if user != "" {
		f += " as " + user
	}
	return f + ", " + wh.fact(), nil
}

func verifyDBXWarehouse(ctx context.Context, in agent.Inputs) (string, error) {
	c, err := newDBX(in.Get("host"), in.Get("token"), in.Get("warehouse-id"))
	if err != nil {
		return "", err
	}
	wh, err := c.warehouse(ctx)
	if err != nil {
		return "", err
	}
	return wh.fact(), nil
}
