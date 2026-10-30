// Package snowflake is a minimal client for the Snowflake SQL API v2.
//
// It speaks HTTPS with a programmatic access token or a key-pair JWT (see
// sfauth) instead of linking a Snowflake driver, which keeps the CLI a small
// static binary. Every statement it runs is a read.
package snowflake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/netsafe"
	"github.com/benchouse/benchouse-cli/internal/snowflake/sfauth"
)

// Config is the non-secret half of a Snowflake connection.
type Config struct {
	Account   string
	User      string
	Role      string
	Warehouse string
	// Auth is sfauth.AuthPAT (also when empty) or sfauth.AuthKeyPair.
	Auth string
}

type Client struct {
	Cfg  Config
	Auth sfauth.Credential
	HTTP *http.Client
	// BaseURL overrides the account-derived host. Tests set it.
	BaseURL string
	// PollEvery is the wait between polls of a running statement.
	PollEvery time.Duration
	// TimeZone, if set, is the session time zone the statements run in. It
	// decides how a zoneless timestamp is compared and rendered.
	TimeZone string
}

// maxResponse bounds one SQL API response.
const maxResponse = 128 << 20

// New builds a client from a connection and its secret: a PAT, or a PEM
// private key when cfg.Auth is sfauth.AuthKeyPair.
func New(cfg Config, secret string) *Client {
	return &Client{
		Cfg:       cfg,
		Auth:      sfauth.New(cfg.Auth, cfg.Account, cfg.User, secret),
		HTTP:      netsafe.Client(180 * time.Second),
		PollEvery: 3 * time.Second,
	}
}

// accountPattern is what an account identifier may hold (myorg-myaccount,
// or a locator with its region: xy12345.eu-west-1.aws). It becomes the host
// name the token is sent to, so nothing else is accepted.
var accountPattern = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*$`)

// handlePattern is what a statement handle the server returns may hold
// before it goes into a request path: a UUID.
var handlePattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)

// checkHandle refuses a statement handle that could change the request path
// it is put into.
func checkHandle(resp *sqlResponse) error {
	if resp.StatementHandle != "" && !handlePattern.MatchString(resp.StatementHandle) {
		return fmt.Errorf("snowflake returned a statement handle that is not one: %.80q", resp.StatementHandle)
	}
	return nil
}

func (c *Client) base() (string, error) {
	if c.BaseURL != "" {
		return c.BaseURL, nil
	}
	account, err := Account(c.Cfg.Account)
	if err != nil {
		return "", err
	}
	return "https://" + account + ".snowflakecomputing.com", nil
}

// Account is the account identifier in what the user gave: the identifier
// itself (myorg-myaccount, or a locator with its region), or the account's
// URL. It becomes the host the token is sent to, so nothing else passes.
func Account(raw string) (string, error) {
	a := strings.ToLower(strings.TrimSpace(raw))
	if u, ok := strings.CutPrefix(a, "https://"); ok { // the account URL, pasted
		a = strings.TrimSuffix(u, "/")
	}
	a = strings.TrimSuffix(a, ".snowflakecomputing.com")
	if !accountPattern.MatchString(a) {
		return "", fmt.Errorf("snowflake account %q is not an account identifier (e.g. myorg-myaccount)", clipValue(raw))
	}
	return a, nil
}

// ValidAccount is Account's check, for an account field.
func ValidAccount(v string) error {
	_, err := Account(v)
	return err
}

func clipValue(s string) string {
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "..."
	}
	return s
}

type sqlResponse struct {
	ResultSetMetaData struct {
		PartitionInfo []struct {
			RowCount int `json:"rowCount"`
		} `json:"partitionInfo"`
		RowType []struct {
			Name string `json:"name"`
		} `json:"rowType"`
	} `json:"resultSetMetaData"`
	Data            [][]*string `json:"data"`
	StatementHandle string      `json:"statementHandle"`
	Message         string      `json:"message"`
	Code            string      `json:"code"`
}

// Query runs stmt and returns its rows as maps keyed by lowercased column
// name. NULLs are absent from the map.
func (c *Client) Query(ctx context.Context, stmt string) ([]map[string]*string, error) {
	cols, rows, err := c.query(ctx, stmt)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]*string, 0, len(rows))
	for _, row := range rows {
		m := make(map[string]*string, len(cols))
		for i, name := range cols {
			if i < len(row) {
				m[name] = row[i]
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// QueryJSON runs a statement whose single column is a JSON object per row
// (typically OBJECT_CONSTRUCT(*)), and returns those objects verbatim.
func (c *Client) QueryJSON(ctx context.Context, stmt string) ([]json.RawMessage, error) {
	_, rows, err := c.query(ctx, stmt)
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if len(row) > 0 && row[0] != nil {
			out = append(out, json.RawMessage(*row[0]))
		}
	}
	return out, nil
}

// EachJSON runs a statement whose single column is a JSON object per row
// and calls fn with each object, fetching result partitions one at a time:
// at most one partition is held in memory, however large the result. It
// returns how many rows fn was given. An error from fn stops the read.
func (c *Client) EachJSON(ctx context.Context, stmt string, fn func(json.RawMessage) error) (int, error) {
	return c.EachJSONArgs(ctx, stmt, nil, fn)
}

// EachJSONArgs is EachJSON with text values bound to the statement's ?
// placeholders, in order, by the server: ids are never spliced into SQL.
func (c *Client) EachJSONArgs(ctx context.Context, stmt string, args []string, fn func(json.RawMessage) error) (int, error) {
	n := 0
	_, err := c.each(ctx, stmt, args, func(_ []string, rows [][]*string) error {
		for _, row := range rows {
			if len(row) == 0 || row[0] == nil {
				continue
			}
			if err := fn(json.RawMessage(*row[0])); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

func (c *Client) query(ctx context.Context, stmt string) ([]string, [][]*string, error) {
	var all [][]*string
	cols, err := c.each(ctx, stmt, nil, func(_ []string, rows [][]*string) error {
		all = append(all, rows...)
		return nil
	})
	return cols, all, err
}

// each runs stmt and hands fn each result partition in turn, fetching the
// next only when fn has returned.
func (c *Client) each(ctx context.Context, stmt string, args []string, fn func(cols []string, rows [][]*string) error) ([]string, error) {
	req := map[string]any{"statement": stmt, "timeout": 170}
	if len(args) > 0 {
		b := make(map[string]any, len(args))
		for i, a := range args {
			b[strconv.Itoa(i+1)] = map[string]string{"type": "TEXT", "value": a}
		}
		req["bindings"] = b
	}
	if c.Cfg.Warehouse != "" {
		req["warehouse"] = c.Cfg.Warehouse
	}
	if c.Cfg.Role != "" {
		req["role"] = c.Cfg.Role
	}
	if c.TimeZone != "" {
		req["parameters"] = map[string]string{"timezone": c.TimeZone}
	}
	base, err := c.base()
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, base+"/api/v2/statements", req)
	if err != nil {
		return nil, err
	}
	if err := checkHandle(resp); err != nil {
		return nil, err
	}
	started := time.Now()
	for resp.Code == "async" && resp.StatementHandle != "" {
		if time.Since(started) > 5*time.Minute {
			return nil, fmt.Errorf("statement %s still running after 5 minutes", resp.StatementHandle)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.PollEvery):
		}
		if resp, err = c.do(ctx, http.MethodGet, base+"/api/v2/statements/"+resp.StatementHandle, nil); err != nil {
			return nil, err
		}
		if err := checkHandle(resp); err != nil {
			return nil, err
		}
	}
	cols := make([]string, len(resp.ResultSetMetaData.RowType))
	for i, ct := range resp.ResultSetMetaData.RowType {
		cols[i] = strings.ToLower(ct.Name)
	}
	partitions := len(resp.ResultSetMetaData.PartitionInfo)
	handle := resp.StatementHandle
	first := resp.Data
	resp = nil // let partition 0 go once fn is done with it
	if err := fn(cols, first); err != nil {
		return cols, err
	}
	first = nil
	for p := 1; p < partitions; p++ {
		part, err := c.do(ctx, http.MethodGet,
			fmt.Sprintf("%s/api/v2/statements/%s?partition=%d", base, handle, p), nil)
		if err != nil {
			return cols, fmt.Errorf("partition %d: %w", p, err)
		}
		if err := fn(cols, part.Data); err != nil {
			return cols, err
		}
	}
	return cols, nil
}

func (c *Client) do(ctx context.Context, method, url string, body any) (*sqlResponse, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	if err := c.Auth.Authorize(req); err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "benchouse-cli")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxResponse {
		return nil, fmt.Errorf("snowflake answered with more than %d bytes", maxResponse)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		snippet := string(raw)
		if len(snippet) > 400 {
			snippet = snippet[:400]
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("snowflake http 401: %s. %s", snippet, c.Auth.Rejected())
		}
		return nil, fmt.Errorf("snowflake http %d: %s", resp.StatusCode, snippet)
	}
	var out sqlResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode snowflake response: %w", err)
	}
	if resp.StatusCode == http.StatusAccepted {
		out.Code = "async"
	}
	return &out, nil
}
