package hex

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
	defaultHost = "https://app.hex.tech"
	// apiRoot is the OpenAPI server path under the host.
	apiRoot = "/api"
	// maxBody bounds one response: a page of 100 threads or 50 full
	// message records.
	maxBody = 64 << 20
	// requestTimeout bounds one request, retries aside.
	requestTimeout = 60 * time.Second
	maxRetries     = 6
)

// minGap spaces requests: the hex-api group allows 30 requests a minute and
// 1,800 an hour, so one request every two seconds stays under both. Tests
// set it to zero.
var minGap = 2 * time.Second

// sleep waits between requests and retries; tests replace it.
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

var now = time.Now

type client struct {
	base, token string
	http        *http.Client
	last        time.Time
}

// apiError is a non-2xx answer, with its status so callers can tell a
// missing role (403) from a missing thread (404).
type apiError struct {
	Status int
	Path   string
	Msg    string
}

func (e *apiError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "Hex rejected the token (401): check it is a current API token (Settings > API keys) and that --host is your workspace's Hex address"
	case http.StatusForbidden:
		return fmt.Sprintf("Hex refused %s (403): listing threads needs a token of a user with the Manager org role or higher (Admins see every non-private thread)", e.Path)
	}
	return fmt.Sprintf("hex %s: HTTP %d: %s", e.Path, e.Status, e.Msg)
}

func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// newClient checks the host and builds the API client; Collect, Check and
// Verify share it. A host given with the /api suffix is accepted.
func newClient(in agent.Inputs) (*client, error) {
	host := in.Get("host")
	if host == "" {
		host = defaultHost
	}
	base, err := netsafe.BaseURL(host, true) // single-tenant hosts may sit under a path
	if err != nil {
		return nil, fmt.Errorf("--host: %w", err)
	}
	base = strings.TrimSuffix(base, apiRoot)
	if in.Get("token") == "" {
		return nil, errors.New("--token: give a Hex API token")
	}
	return &client{base: base, token: in.Get("token"), http: netsafe.Client(requestTimeout)}, nil
}

// throttle waits until minGap has passed since the previous request.
func (c *client) throttle(ctx context.Context) error {
	if c.last.IsZero() || minGap <= 0 {
		return nil
	}
	if wait := minGap - now().Sub(c.last); wait > 0 {
		return sleep(ctx, wait)
	}
	return nil
}

// get fetches apiRoot+path into out, spaced by minGap and retrying with
// backoff on 429 and gateway errors.
func (c *client) get(ctx context.Context, path string, out any) error {
	for attempt := 0; ; attempt++ {
		if err := c.throttle(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+apiRoot+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		c.last = now()
		if err != nil {
			return fmt.Errorf("hex %s: %w", c.base, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if len(body) > maxBody {
			return fmt.Errorf("hex answered %s with more than %d bytes", endpointOf(path), maxBody)
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("hex %s: unexpected response: %w", endpointOf(path), err)
			}
			return nil
		case retryable(resp.StatusCode) && attempt < maxRetries:
			if err := sleep(ctx, backoff(resp.Header.Get("Retry-After"), attempt)); err != nil {
				return err
			}
			continue
		}
		return &apiError{Status: resp.StatusCode, Path: endpointOf(path), Msg: short(string(body))}
	}
}

func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// backoff honours Retry-After (seconds, at most five minutes: the hourly
// limit can ask for more than a minute), else doubles from two seconds up to
// a minute.
func backoff(retryAfter string, attempt int) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, 5*time.Minute)
	}
	return min(2*time.Second<<attempt, time.Minute)
}

// endpointOf is path without its query, for messages.
func endpointOf(path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	return apiRoot + path
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// pathID escapes an id for a request path, refusing one that is not an id.
func pathID(id string) (string, error) {
	if !idPattern.MatchString(id) || strings.Contains(id, "..") {
		return "", fmt.Errorf("hex thread id %q is not an id", short(id))
	}
	return url.PathEscape(id), nil
}

func short(s string) string {
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
