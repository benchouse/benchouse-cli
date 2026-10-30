package cube

import (
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
	// maxBody bounds one response: a page of up to 50 threads with their
	// turns, each text cut by Cube at 20,000 characters, and up to 100
	// result rows per query.
	maxBody = 128 << 20
	// requestTimeout bounds one request, retries aside.
	requestTimeout = 120 * time.Second
	maxRetries     = 6
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

// client speaks to one Cube Cloud tenant's control plane API with a bearer
// API key.
type client struct {
	base, token string
	http        *http.Client
}

// apiError is a non-2xx answer, with its status so callers can tell a
// missing role (403) from a feature that is off (404).
type apiError struct {
	Status int
	Path   string
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("cube %s: HTTP %d: %s", e.Path, e.Status, e.Msg)
}

func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// newClient checks the host and builds the client. Collect, Check, Verify
// and Lookup share it.
func newClient(in agent.Inputs) (*client, error) {
	host := in.Get("host")
	if host == "" {
		return nil, errors.New("--host: give the Cube Cloud URL, e.g. https://acme.cubecloud.dev")
	}
	base, err := netsafe.BaseURL(host, false)
	if err != nil {
		return nil, fmt.Errorf("--host: %w", err)
	}
	if in.Get("api-token") == "" {
		return nil, errors.New("--api-token: give a Cube API key (Settings > API keys) of an admin user")
	}
	return &client{base: base, token: in.Get("api-token"), http: netsafe.Client(requestTimeout)}, nil
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,200}$`)

// pathID escapes an id for a request path, refusing one that is not an id.
func pathID(kind, id string) (string, error) {
	if !idPattern.MatchString(id) || strings.Contains(id, "..") {
		return "", fmt.Errorf("cube %s id %q is not an id", kind, clip(id, 80))
	}
	return url.PathEscape(id), nil
}

// get fetches path into out, retrying 429 and gateway errors with backoff.
// Redirects are refused by netsafe.Client.
func (c *client) get(ctx context.Context, path string, out any) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return fmt.Errorf("refusing request path %q", path)
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			return err
		}
		if req.URL.Host != hostOf(c.base) {
			return fmt.Errorf("refusing request path %q: it leaves %s", path, c.base)
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "benchouse-cli")
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("cube %s: %w", c.base, err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if len(raw) > maxBody {
			return fmt.Errorf("cube %s answered with more than %d bytes", pathOnly(path), maxBody)
		}
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("cube %s: unexpected response: %w", pathOnly(path), err)
			}
			return nil
		case retryable(resp.StatusCode) && attempt < maxRetries:
			if err := sleep(ctx, backoff(resp.Header.Get("Retry-After"), attempt)); err != nil {
				return err
			}
			continue
		}
		return &apiError{Status: resp.StatusCode, Path: pathOnly(path), Msg: errMessage(raw)}
	}
}

func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Host
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// errMessage is the message of an error body, or the body clipped.
func errMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if e.Message != "" {
			return clip(e.Message, 200)
		}
		if e.Error != "" {
			return clip(e.Error, 200)
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

// backoff honours Retry-After (seconds), else doubles from one second up to
// thirty.
func backoff(retryAfter string, attempt int) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, time.Minute)
	}
	return min(time.Second<<attempt, 30*time.Second)
}

// explain turns an API error into what to fix. what names the request.
func explain(err error, what string) error {
	switch statusOf(err) {
	case http.StatusUnauthorized:
		return errors.New("Cube rejected the API key (401): check --api-token and that --host is your tenant's URL")
	case http.StatusForbidden:
		return fmt.Errorf("Cube refused %s (403): chat history export needs an API key of a user with the admin role", what)
	}
	return err
}
