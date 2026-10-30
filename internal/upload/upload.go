// Package upload sends a bundle to Benchouse, and asks for an agent the CLI
// does not support yet.
package upload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/netsafe"
)

// DefaultEndpoint receives bundles.
const DefaultEndpoint = "https://api.benchouse.ai"

// DefaultSite receives agent requests.
const DefaultSite = "https://benchouse.ai"

// Site is where agent requests go: DefaultSite, or $BENCHOUSE_SITE for
// testing against a local benchouse.ai (the https-only rule still applies
// to anything but a loopback host when the request is sent).
func Site() string {
	if s := strings.TrimSpace(os.Getenv("BENCHOUSE_SITE")); s != "" {
		return s
	}
	return DefaultSite
}

// MaxBytes is the largest bundle the endpoint accepts.
const MaxBytes = 32 << 20

// Receipt is the endpoint's answer to an upload.
type Receipt struct {
	ID    string `json:"id"`
	Bytes int64  `json:"bytes"`
}

// Send uploads the bundle at path.
func Send(ctx context.Context, endpoint, path, email, cliVersion string) (Receipt, error) {
	f, err := os.Open(path)
	if err != nil {
		return Receipt{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Receipt{}, err
	}
	if st.Size() > MaxBytes {
		return Receipt{}, fmt.Errorf("the bundle is %d MiB, over the %d MiB limit: use --since to capture a shorter window",
			st.Size()>>20, MaxBytes>>20)
	}
	base, err := netsafe.BaseURL(endpoint, true)
	if err != nil {
		return Receipt{}, fmt.Errorf("--endpoint: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/uploads", f)
	if err != nil {
		return Receipt{}, err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("X-Benchouse-Email", email)
	req.Header.Set("X-Benchouse-CLI", cliVersion)
	req.Header.Set("User-Agent", "benchouse-cli/"+cliVersion)

	resp, err := netsafe.Client(10 * time.Minute).Do(req)
	if err != nil {
		return Receipt{}, fmt.Errorf("upload failed: %w (the bundle is still at %s)", err, path)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(body))
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return Receipt{}, &StatusError{Code: resp.StatusCode, Err: refused(resp, msg, path, email)}
	}
	var r Receipt
	if err := json.Unmarshal(body, &r); err != nil {
		return Receipt{}, fmt.Errorf("unexpected answer from %s: %s", endpoint, body)
	}
	return r, nil
}

// StatusError is an upload the server refused, with its HTTP status. Its
// text is Err's.
type StatusError struct {
	Code int
	Err  error
}

func (e *StatusError) Error() string { return e.Err.Error() }
func (e *StatusError) Unwrap() error { return e.Err }

// refused says why the server refused an upload, and what to do.
func refused(resp *http.Response, msg, path, email string) error {
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		wait := resp.Header.Get("Retry-After")
		if wait == "" {
			wait = "a while"
		} else {
			wait += " seconds"
		}
		return fmt.Errorf("Benchouse is not accepting uploads right now (%s); try again in %s with: benchouse send %s --email %s",
			msg, wait, path, email)
	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("the bundle is too large (%s): collect a shorter window with --since", msg)
	}
	return fmt.Errorf("upload refused (HTTP %d): %s (the bundle is still at %s)", resp.StatusCode, msg, path)
}

// MaxAgentName bounds the agent name a request carries.
const MaxAgentName = 200

// RequestAgentAnonymous records that someone asked for an agent, before
// (and whether or not) they leave an email: POST {site}/api/v1/agent-request
// with {"agent", "source": "cli"}. It returns the request's id (ar_...) for
// RequestAgentEmail to link to.
func RequestAgentAnonymous(ctx context.Context, site, agentName string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := postJSON(ctx, site, "/api/v1/agent-request", map[string]any{"agent": agentName, "source": "cli"}, &out)
	if err != nil {
		return "", err
	}
	if !agentRequestID.MatchString(out.ID) {
		return "", nil // recorded, but no id worth linking to
	}
	return out.ID, nil
}

// agentRequestID is what an id from agent-request may look like before it
// is sent back.
var agentRequestID = regexp.MustCompile(`^ar_[A-Za-z0-9_-]{1,64}$`)

// RequestAgentEmail asks Benchouse to tell email when the agent is
// supported, through the waitlist form the website uses, linked to the
// anonymous request id when there is one. Benchouse replies by email.
func RequestAgentEmail(ctx context.Context, site, agentName, email, id string) error {
	return postJSON(ctx, site, "/api/v1/waitlist", map[string]any{
		"email":   email,
		"agent":   agentName,
		"urgency": "cli: requested agent support",
		"ref":     map[string]string{"source": "benchouse-cli", "content": id},
	}, nil)
}

// postJSON posts body to site+path (https, or http to this machine; no
// redirects) and decodes a JSON answer into out when out is set.
func postJSON(ctx context.Context, site, path string, body, out any) error {
	b, _ := json.Marshal(body)
	base, err := netsafe.BaseURL(site, true)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "benchouse-cli")
	resp, err := netsafe.Client(30 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("request refused (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		_ = json.Unmarshal(raw, out) // an unreadable answer only loses the id
	}
	return nil
}
