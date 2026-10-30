package looker

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
	"strconv"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/netsafe"
)

// apiPrefix is the Looker API 4.0 root; API 3.x was removed in Looker 23.18.
const apiPrefix = "/api/4.0"

const (
	// maxBody bounds one response: a page of System Activity rows or one
	// conversation's messages.
	maxBody = 64 << 20
	// requestTimeout bounds one request, retries aside. System Activity
	// queries can be slow.
	requestTimeout = 180 * time.Second
	maxRetries     = 5
)

// sleep waits between retries; tests replace it.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// client speaks to one Looker instance's API 4.0. token is the API client's
// own session; sudo sessions (login_user) are separate tokens passed per
// call and always logged out.
type client struct {
	base           string // scheme://host[:port]
	id, secret     string
	token          string
	http           *http.Client
	relogged       bool
	loggedOutAdmin bool
}

// apiError is a non-2xx answer, with its status so callers can tell a
// missing permission (403) from a missing endpoint (404).
type apiError struct {
	Status int
	Path   string
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("looker %s: HTTP %d: %s", e.Path, e.Status, e.Msg)
}

// statusOf returns the HTTP status of an apiError, or 0.
func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

func newClient(host, id, secret string) (*client, error) {
	if strings.TrimSpace(host) == "" {
		return nil, errors.New("--host: give the Looker instance URL, e.g. https://acme.cloud.looker.com")
	}
	base, err := netsafe.BaseURL(host, false)
	if err != nil {
		return nil, fmt.Errorf("--host: %w", err)
	}
	return &client{base: base, id: id, secret: secret, http: netsafe.Client(requestTimeout)}, nil
}

// idPattern is what an id interpolated into a request path may hold: Looker
// user and query ids are numeric strings, conversation ids may be UUIDs.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// pathID escapes an id for a request path, refusing one that is not an id.
func pathID(kind, id string) (string, error) {
	if !idPattern.MatchString(id) || strings.Contains(id, "..") {
		return "", fmt.Errorf("looker %s id %q is not an id", kind, clip(id, 80))
	}
	return url.PathEscape(id), nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// login presents the client id and secret in the form body (never in the
// URL, where they would reach access logs) and keeps the access token.
func (c *client) login(ctx context.Context) error {
	form := url.Values{"client_id": {c.id}, "client_secret": {c.secret}}.Encode()
	raw, err := c.send(ctx, http.MethodPost, "/login", "", strings.NewReader(form), "application/x-www-form-urlencoded")
	if err != nil {
		switch statusOf(err) {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return fmt.Errorf("Looker at %s rejected the client id and secret (HTTP %d): check them (Admin > Users > Edit > API keys) "+
				"and that --host is the instance URL", c.base, statusOf(err))
		}
		return err
	}
	tok, err := accessToken(raw)
	if err != nil {
		return err
	}
	c.token = tok
	return nil
}

func accessToken(raw []byte) (string, error) {
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &t); err != nil || t.AccessToken == "" {
		return "", errors.New("looker login: the answer holds no access_token")
	}
	return t.AccessToken, nil
}

// sudo logs in as userID (login_user): admin only. The token must be logged
// out with logout.
func (c *client) sudo(ctx context.Context, userID string) (string, error) {
	id, err := pathID("user", userID)
	if err != nil {
		return "", err
	}
	raw, err := c.send(ctx, http.MethodPost, "/login/"+id, c.token, nil, "")
	if err != nil {
		return "", err
	}
	return accessToken(raw)
}

// logout invalidates token. It runs even when ctx is cancelled, so a sudo
// session is never left behind by an interrupted run.
func (c *client) logout(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, err := c.send(ctx, http.MethodDelete, "/logout", token, nil, "")
	return err
}

// logoutAdmin ends the API client's own session, once.
func (c *client) logoutAdmin(ctx context.Context) error {
	if c.token == "" || c.loggedOutAdmin {
		return nil
	}
	c.loggedOutAdmin = true
	return c.logout(ctx, c.token)
}

// getJSON GETs path as token ("" = the API client) into out.
func (c *client) getJSON(ctx context.Context, token, path string, out any) error {
	raw, err := c.send(ctx, http.MethodGet, path, c.tok(token), nil, "")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("looker %s: unexpected response: %w", path, err)
	}
	return nil
}

// getText GETs path as token and returns the body as text (result_format=sql).
func (c *client) getText(ctx context.Context, token, path string) (string, error) {
	raw, err := c.send(ctx, http.MethodGet, path, c.tok(token), nil, "")
	return string(raw), err
}

// postJSON POSTs body as JSON and decodes the answer into out.
func (c *client) postJSON(ctx context.Context, token, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	raw, err := c.send(ctx, http.MethodPost, path, c.tok(token), bytes.NewReader(b), "application/json")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("looker %s: unexpected response: %w", path, err)
	}
	return nil
}

func (c *client) tok(t string) string {
	if t == "" {
		return c.token
	}
	return t
}

// send makes one request under apiPrefix, retrying 429 and gateway errors
// with backoff. When the API client's own token has expired (401) it logs in
// again once. Redirects are refused by netsafe.Client.
func (c *client) send(ctx context.Context, method, path, token string, body io.Reader, ctype string) ([]byte, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, fmt.Errorf("refusing request path %q", path)
	}
	var payload []byte
	if body != nil {
		b, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		payload = b
	}
	u, err := url.Parse(c.base + apiPrefix + path)
	if err != nil {
		return nil, err
	}
	if b, _ := url.Parse(c.base); b == nil || u.Host != b.Host || u.Scheme != b.Scheme || u.User != nil {
		return nil, fmt.Errorf("refusing request path %q: it leaves %s", path, c.base)
	}
	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "token "+token)
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("User-Agent", "benchouse-cli")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("looker %s: %w", c.base, err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(raw) > maxBody {
			return nil, fmt.Errorf("looker %s answered with more than %d bytes", path, maxBody)
		}
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if token == c.token {
				c.relogged = false // a later expiry may log in again
			}
			return raw, nil
		case retryable(resp.StatusCode) && attempt < maxRetries:
			if err := sleep(ctx, backoff(resp.Header.Get("Retry-After"), attempt)); err != nil {
				return nil, err
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized && token != "" && token == c.token && !c.relogged && path != "/logout":
			// The API client's session expired (tokens last an hour).
			c.relogged = true
			if err := c.login(ctx); err != nil {
				return nil, err
			}
			token = c.token
			continue
		}
		return nil, &apiError{Status: resp.StatusCode, Path: apiPrefix + path, Msg: errMessage(raw)}
	}
}

// errMessage is the message of a Looker Error body, or the body clipped.
func errMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return clip(e.Message, 200)
	}
	return clip(strings.TrimSpace(string(raw)), 200)
}

func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// backoff honours Retry-After (seconds), else doubles from one second up to
// thirty.
func backoff(retryAfter string, attempt int) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, time.Minute)
	}
	return min(time.Second<<attempt, 30*time.Second)
}
