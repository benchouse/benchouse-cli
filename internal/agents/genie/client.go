package genie

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/netsafe"
)

// client speaks to one Databricks workspace over REST with a bearer token:
// the SQL Statement Execution API for the system tables, the Genie API for
// conversation content and spaces, and SCIM for the user directory. It is a
// port of Benchouse's production Genie connector client.
type client struct {
	host, token, warehouseID string
	http                     *http.Client
}

// maxResponse bounds one API response. An inline result chunk is at most
// 25 MiB.
const maxResponse = 64 << 20

// maxChunks bounds the result chunks followed for one statement: a server
// repeating a link would otherwise be followed forever.
const maxChunks = 10000

// pollEvery is the wait between polls of a running statement; tests shorten
// it.
var pollEvery = 5 * time.Second

func newClient(host, token, warehouse string) (*client, error) {
	h, err := normalizeHost(host)
	if err != nil {
		return nil, fmt.Errorf("--host: %w", err)
	}
	return &client{host: h, token: token, warehouseID: warehouseID(warehouse), http: netsafe.Client(180 * time.Second)}, nil
}

// normalizeHost accepts a workspace host with or without its scheme, and
// only a host: https (http only for this machine), no path, no user info.
func normalizeHost(h string) (string, error) {
	if strings.TrimSpace(h) == "" {
		return "", nil
	}
	return netsafe.BaseURL(h, false)
}

// idPattern is what a Databricks id interpolated into a request path may
// hold: space, conversation and message ids are hex, statement ids UUIDs.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// validID reports whether id is safe to put into a request path.
func validID(id string) bool { return idPattern.MatchString(id) }

// pathID escapes an id for a request path, refusing one that is not an id.
func pathID(kind, id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("databricks %s id %q is not an id", kind, clip(id))
	}
	return url.PathEscape(id), nil
}

// clip shortens a server-supplied value for an error message.
func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// apiPath checks a path the server handed back (a result chunk link) before
// the token is sent to it: an absolute path under /api/ on the configured
// host, nothing that could name another host.
func apiPath(p string) error {
	if !strings.HasPrefix(p, "/api/") || strings.Contains(p, "//") || strings.Contains(p, "@") ||
		strings.Contains(p, `\`) || strings.Contains(p, "://") {
		return fmt.Errorf("databricks returned a link outside its API (%q); not following it", clip(p))
	}
	return nil
}

// do sends one request and returns the raw body, with the HTTP status so a
// caller can tell a rate limit from a refusal.
func (c *client) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	// path is appended to the configured host, so it must not be able to
	// change it: an absolute path, and the result must still be that host.
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, 0, fmt.Errorf("refusing request path %q", path)
	}
	u, err := url.Parse(c.host + path)
	if err != nil {
		return nil, 0, err
	}
	if base, _ := url.Parse(c.host); base == nil || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil {
		return nil, 0, fmt.Errorf("refusing request path %q: it leaves %s", path, c.host)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "benchouse-cli")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(raw) > maxResponse {
		return nil, resp.StatusCode, fmt.Errorf("databricks answered with more than %d bytes", maxResponse)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(raw)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, resp.StatusCode, fmt.Errorf("databricks http %d: %s", resp.StatusCode, snippet)
	}
	return raw, resp.StatusCode, nil
}

// getJSON sends one request and decodes the answer into out.
func (c *client) getJSON(ctx context.Context, method, path string, body, out any) error {
	raw, _, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

type stmtResponse struct {
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

// queryJSONRows runs SQL whose single column is a JSON object per row and
// returns those objects verbatim. For small results; queryJSONEach streams.
func (c *client) queryJSONRows(ctx context.Context, stmt string) ([]string, error) {
	var out []string
	_, err := c.queryJSONEach(ctx, stmt, nil, func(row string) error {
		out = append(out, row)
		return nil
	})
	return out, err
}

// param is a named parameter of a statement (:name), bound by the server.
type param struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type"`
}

// idParams binds ids as :id1, :id2, ... and returns the list for an IN.
func idParams(ids []string) ([]param, string) {
	ps := make([]param, len(ids))
	names := make([]string, len(ids))
	for i, id := range ids {
		ps[i] = param{Name: fmt.Sprintf("id%d", i+1), Value: id, Type: "STRING"}
		names[i] = ":" + ps[i].Name
	}
	return ps, strings.Join(names, ", ")
}

// queryJSONEach runs SQL whose single column is a JSON object per row and
// calls fn with each object, one result chunk at a time: the next chunk is
// fetched only when fn has seen every row of this one. It polls a statement
// that outlives the 50s wait and follows chunk links, so a large result is
// never silently cut. It returns how many rows fn was given.
func (c *client) queryJSONEach(ctx context.Context, stmt string, params []param, fn func(string) error) (int, error) {
	req := map[string]any{
		"warehouse_id": c.warehouseID,
		"statement":    stmt,
		"wait_timeout": "50s",
		"format":       "JSON_ARRAY",
		"disposition":  "INLINE",
	}
	if len(params) > 0 {
		req["parameters"] = params
	}
	var resp stmtResponse
	if err := c.getJSON(ctx, http.MethodPost, "/api/2.0/sql/statements", req, &resp); err != nil {
		return 0, err
	}
	// Read the id before resetting resp: a poll URL with an empty id is
	// answered with ENDPOINT_NOT_FOUND.
	stmtID := resp.StatementID
	for i := 0; resp.Status.State == "PENDING" || resp.Status.State == "RUNNING"; i++ {
		if i > 60 {
			return 0, fmt.Errorf("statement %s still running after 5m", stmtID)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(pollEvery):
		}
		id, err := pathID("statement", stmtID)
		if err != nil {
			return 0, err
		}
		resp = stmtResponse{}
		if err := c.getJSON(ctx, http.MethodGet, "/api/2.0/sql/statements/"+id, nil, &resp); err != nil {
			return 0, err
		}
		if resp.StatementID != "" {
			stmtID = resp.StatementID
		}
	}
	if resp.Status.State != "SUCCEEDED" {
		return 0, fmt.Errorf("statement %s: %s %s", stmtID, resp.Status.State, resp.Status.Error.Message)
	}
	n := 0
	each := func(rows [][]*string) error {
		for _, r := range rows {
			if len(r) > 0 && r[0] != nil {
				if err := fn(*r[0]); err != nil {
					return err
				}
				n++
			}
		}
		return nil
	}
	if err := each(resp.Result.DataArray); err != nil {
		return n, err
	}
	for next, i := resp.Result.NextChunk, 0; next != ""; i++ {
		if err := apiPath(next); err != nil {
			return n, err
		}
		if i >= maxChunks {
			return n, fmt.Errorf("statement %s: more than %d result chunks", stmtID, maxChunks)
		}
		var chunk stmtResponse
		if err := c.getJSON(ctx, http.MethodGet, next, nil, &chunk); err != nil {
			return n, fmt.Errorf("chunk: %w", err)
		}
		if err := each(chunk.Result.DataArray); err != nil {
			return n, err
		}
		next = chunk.Result.NextChunk
	}
	return n, nil
}

const messagePath = "/api/2.0/genie/spaces/%s/conversations/%s/messages/%s"

// message fetches one Genie message verbatim. This is the only source of
// conversation content; the audit log carries none.
func (c *client) message(ctx context.Context, space, conv, msg string) ([]byte, int, error) {
	for _, id := range []struct{ kind, v string }{{"space", space}, {"conversation", conv}, {"message", msg}} {
		if _, err := pathID(id.kind, id.v); err != nil {
			return nil, 0, err
		}
	}
	return c.do(ctx, http.MethodGet, fmt.Sprintf(messagePath, url.PathEscape(space), url.PathEscape(conv), url.PathEscape(msg)), nil)
}

// scimPageSize is a var so a test can force a page boundary.
var scimPageSize = 100

const scimPath = "/api/2.0/preview/scim/v2/Users?startIndex=%d&count=%d"

// users reads the workspace's user directory, one page at a time, each
// Resource verbatim. It stops on a short page rather than on totalResults:
// the count a server reports and the rows it returns can disagree.
func (c *client) users(ctx context.Context) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for start := 1; ; start += scimPageSize {
		var body struct {
			Resources []json.RawMessage `json:"Resources"`
		}
		if err := c.getJSON(ctx, http.MethodGet, fmt.Sprintf(scimPath, start, scimPageSize), nil, &body); err != nil {
			return nil, err
		}
		out = append(out, body.Resources...)
		if len(body.Resources) < scimPageSize {
			return out, nil
		}
	}
}

// space is one Genie space as the list endpoint reports it.
type space struct {
	SpaceID     string `json:"space_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// maxSpacePages bounds the walk: 4,000 spaces is past any real workspace, so
// hitting it means a server repeating a page token.
const maxSpacePages = 40

// spaces lists every Genie space the token can see. Trashed spaces are not
// returned.
func (c *client) spaces(ctx context.Context) ([]space, error) {
	var out []space
	token := ""
	for page := 0; page < maxSpacePages; page++ {
		path := "/api/2.0/genie/spaces?page_size=100"
		if token != "" {
			path += "&page_token=" + url.QueryEscape(token)
		}
		var resp struct {
			Spaces        []space `json:"spaces"`
			NextPageToken string  `json:"next_page_token"`
		}
		if err := c.getJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, fmt.Errorf("list genie spaces: %w", err)
		}
		out = append(out, resp.Spaces...)
		if resp.NextPageToken == "" || resp.NextPageToken == token {
			return out, nil
		}
		token = resp.NextPageToken
	}
	return out, nil
}
