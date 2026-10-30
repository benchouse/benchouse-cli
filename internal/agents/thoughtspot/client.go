package thoughtspot

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

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/netsafe"
)

const (
	apiPrefix = "/api/rest/2.0"
	// maxBody bounds one response: a searchdata page of pageRows rows, each
	// holding a question and Spotter's response.
	maxBody        = 128 << 20
	requestTimeout = 180 * time.Second
	maxRetries     = 6
	// tokenValidity is how long a token from auth/token/full lasts, in
	// seconds; the client signs in again once on a 401.
	tokenValidity = 3600
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

// client speaks to one ThoughtSpot instance's REST API v2.0 with a bearer
// token: given directly, or obtained with a username and a secret key or
// password (POST /auth/token/full).
type client struct {
	base               string
	mode               string // token, secret-key, password
	username, secret   string
	orgID              string
	token              string
	http               *http.Client
	relogged, loggedIn bool
}

type apiError struct {
	Status int
	Path   string
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("thoughtspot %s: HTTP %d: %s", e.Path, e.Status, e.Msg)
}

func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

func authMode(in agent.Inputs) string {
	m := in.Get("auth")
	if m == "" {
		return authToken
	}
	return m
}

const (
	authToken    = "token"
	authSecret   = "secret-key"
	authPassword = "password"
)

func newClient(in agent.Inputs) (*client, error) {
	host := in.Get("host")
	if host == "" {
		return nil, errors.New("--host: give the ThoughtSpot URL, e.g. https://acme.thoughtspot.cloud")
	}
	base, err := netsafe.BaseURL(host, false)
	if err != nil {
		return nil, fmt.Errorf("--host: %w", err)
	}
	c := &client{base: base, mode: authMode(in), orgID: in.Get("org-id"), http: netsafe.Client(requestTimeout)}
	switch c.mode {
	case authToken:
		c.token = in.Get("token")
		if c.token == "" {
			return nil, errors.New("--token: give a ThoughtSpot REST API v2 bearer token")
		}
		c.loggedIn = true
	case authSecret, authPassword:
		c.username, c.secret = in.Get("username"), in.Get("secret")
		if c.username == "" || c.secret == "" {
			return nil, fmt.Errorf("--auth %s needs --username and --secret", c.mode)
		}
	default:
		return nil, fmt.Errorf("--auth must be token, secret-key or password, not %q", c.mode)
	}
	return c, nil
}

// loginStatement is the sign-in request as Plan shows it, secret masked.
func loginStatement(mode string) string {
	field := "secret_key"
	if mode == authPassword {
		field = "password"
	}
	return fmt.Sprintf(`POST %s/auth/token/full {"username":"<username>","%s":"<secret>","validity_time_in_sec":%d}`, apiPrefix, field, tokenValidity)
}

// login exchanges the username and secret for a bearer token. The secret
// travels in the JSON body, never the URL.
func (c *client) login(ctx context.Context) error {
	body := map[string]any{"username": c.username, "validity_time_in_sec": tokenValidity}
	if c.mode == authPassword {
		body["password"] = c.secret
	} else {
		body["secret_key"] = c.secret
	}
	if c.orgID != "" {
		if n, err := strconv.Atoi(c.orgID); err == nil {
			body["org_id"] = n
		}
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, "/auth/token/full", body, &out, false); err != nil {
		switch statusOf(err) {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
			hint := "check --username and --secret"
			if c.mode == authSecret {
				hint += " (the secret key comes from Develop > Security settings, with trusted authentication on)"
			}
			return fmt.Errorf("ThoughtSpot at %s refused the sign-in (HTTP %d): %s", c.base, statusOf(err), hint)
		}
		return err
	}
	if out.Token == "" {
		return errors.New("thoughtspot auth/token/full: the answer holds no token")
	}
	c.token, c.loggedIn = out.Token, true
	return nil
}

func (c *client) ensure(ctx context.Context) error {
	if c.loggedIn {
		return nil
	}
	return c.login(ctx)
}

func (c *client) get(ctx context.Context, path string, out any) error {
	if err := c.ensure(ctx); err != nil {
		return err
	}
	return c.do(ctx, http.MethodGet, path, nil, out, true)
}

func (c *client) post(ctx context.Context, path string, body, out any) error {
	if err := c.ensure(ctx); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, body, out, true)
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,200}$`)

func pathID(kind, id string) (string, error) {
	if !idPattern.MatchString(id) || strings.Contains(id, "..") {
		return "", fmt.Errorf("thoughtspot %s id %q is not an id", kind, clip(id, 80))
	}
	return url.PathEscape(id), nil
}

// do sends one request under apiPrefix, retrying 429 and gateway errors
// with backoff, and signing in again once when a token it obtained expires.
// Redirects are refused by netsafe.Client.
func (c *client) do(ctx context.Context, method, path string, body, out any, auth bool) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return fmt.Errorf("refusing request path %q", path)
	}
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = b
	}
	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+apiPrefix+path, rdr)
		if err != nil {
			return err
		}
		if b, _ := url.Parse(c.base); b == nil || req.URL.Host != b.Host {
			return fmt.Errorf("refusing request path %q: it leaves %s", path, c.base)
		}
		if auth {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "benchouse-cli")
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("thoughtspot %s: %w", c.base, err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if len(raw) > maxBody {
			return fmt.Errorf("thoughtspot %s answered with more than %d bytes", pathOnly(path), maxBody)
		}
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if auth {
				c.relogged = false
			}
			if out == nil || len(bytes.TrimSpace(raw)) == 0 {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("thoughtspot %s: unexpected response: %w", pathOnly(path), err)
			}
			return nil
		case retryable(resp.StatusCode) && attempt < maxRetries:
			if err := sleep(ctx, backoff(resp.Header.Get("Retry-After"), attempt)); err != nil {
				return err
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized && auth && c.mode != authToken && !c.relogged:
			c.relogged = true
			if err := c.login(ctx); err != nil {
				return err
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized && auth:
			return fmt.Errorf("ThoughtSpot at %s rejected the token (401): check --token, or sign in with --auth secret-key or password", c.base)
		}
		return &apiError{Status: resp.StatusCode, Path: apiPrefix + pathOnly(path), Msg: errMessage(raw)}
	}
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// errMessage digs a message out of ThoughtSpot's error body
// ({"error":{"message":...}} or {"message":...}), else clips the body.
func errMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if e.Message != "" {
			return clip(e.Message, 200)
		}
		switch v := e.Error.(type) {
		case string:
			return clip(v, 200)
		case map[string]any:
			if m, ok := v["message"].(string); ok {
				return clip(m, 200)
			}
			if m, ok := v["message"].(map[string]any); ok {
				b, _ := json.Marshal(m)
				return clip(string(b), 200)
			}
		}
	}
	return clip(strings.TrimSpace(string(raw)), 200)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

func backoff(retryAfter string, attempt int) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, time.Minute)
	}
	return min(time.Second<<attempt, 30*time.Second)
}

// sessionUser is GET /auth/session/user, read loosely.
type sessionUser struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Privileges  []string `json:"privileges"`
}

const sessionPath = "/auth/session/user"

func (c *client) session(ctx context.Context) (sessionUser, error) {
	var u sessionUser
	err := c.get(ctx, sessionPath, &u)
	return u, err
}

func (u sessionUser) has(p string) bool {
	for _, x := range u.Privileges {
		if strings.EqualFold(x, p) {
			return true
		}
	}
	return false
}
