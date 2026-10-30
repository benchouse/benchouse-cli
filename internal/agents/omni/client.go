package omni

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
	// apiRoot is the OpenAPI server path under the host.
	apiRoot = "/api"
	// maxBody bounds one response: a page of conversations, one
	// conversation, or one job result (which can carry CSV rows).
	maxBody = 64 << 20
	// requestTimeout bounds one request, retries aside.
	requestTimeout = 60 * time.Second
	maxRetries     = 6
)

// minGap spaces requests: Omni's API allows 60 requests a minute. Tests set
// it to zero.
var minGap = time.Second

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
// refused token (403) from an expired job result (404).
type apiError struct {
	Status int
	Path   string
	Msg    string
}

func (e *apiError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "Omni rejected the API key (401): check it is a current Organization API key (Settings > API keys) and that --host is your Omni address"
	case http.StatusForbidden:
		return fmt.Sprintf("Omni refused %s (403): reading every user's AI conversations needs an Organization API key; a personal access token reads only its own", e.Path)
	}
	return fmt.Sprintf("omni %s: HTTP %d: %s", e.Path, e.Status, e.Msg)
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
		return nil, errors.New("--host: give your Omni address, e.g. https://acme.omniapp.co")
	}
	base, err := baseURL(host)
	if err != nil {
		return nil, fmt.Errorf("--host: %w", err)
	}
	if in.Get("api-key") == "" {
		return nil, errors.New("--api-key: give an Omni Organization API key")
	}
	return &client{base: base, token: in.Get("api-key"), http: netsafe.Client(requestTimeout)}, nil
}

// baseURL checks an Omni address by netsafe's rules (https only) and
// returns scheme://host: a trailing /api, as the API reference writes the
// server, is accepted; any other path is not.
func baseURL(host string) (string, error) {
	base, err := netsafe.BaseURL(host, true)
	if err != nil {
		return "", err
	}
	base = strings.TrimSuffix(base, apiRoot)
	if u, _ := url.Parse(base); u != nil && u.Path != "" {
		return "", fmt.Errorf("give the Omni address only, without a path (https://%s)", u.Host)
	}
	return base, nil
}

// validHost is the host field's Validate.
func validHost(v string) error {
	_, err := baseURL(v)
	return err
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
			return fmt.Errorf("omni %s: %w", c.base, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if len(body) > maxBody {
			return fmt.Errorf("omni answered %s with more than %d bytes", endpointOf(path), maxBody)
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("omni %s: unexpected response: %w", endpointOf(path), err)
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

// backoff honours Retry-After (seconds, at most five minutes), else doubles
// from two seconds up to a minute.
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
		return "", fmt.Errorf("omni id %q is not an id", short(id))
	}
	return url.PathEscape(id), nil
}

func short(s string) string {
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
