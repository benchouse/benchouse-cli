// Package telemetry sends anonymous usage stats: which steps a run reaches,
// which agent was picked and which kinds of errors occur. It never sends
// field values, credentials, names, emails, paths, queries, questions or
// data: events carry agent ids, step names, counts, durations, error
// categories and error messages passed through Scrub.
//
// Sending never blocks or fails a run. Events are queued in memory and
// posted in the background, in batches, with a short timeout per request;
// Close gives what is left a hard time budget and then drops it. Errors are
// never shown to the user.
//
// The stats go to PostHog (project 502049), anonymously: the distinct id is
// a random install id kept in the user's config directory, and events ask
// PostHog not to create person profiles.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// DefaultEndpoint is PostHog's batch capture API (US cloud).
const DefaultEndpoint = "https://us.i.posthog.com/batch/"

// APIKey is the PostHog project key. It is public by design, like a
// browser analytics key: it can only send events to the project, never read
// them.
const APIKey = "phc_opogYivcrqaXKgVstvjNFXUKGWcnKEp5sxwLymhtkSVZ"

// EnvEndpoint overrides the endpoint, for development and tests.
const EnvEndpoint = "BENCHOUSE_TELEMETRY_ENDPOINT"

const (
	requestTimeout = 3 * time.Second
	batchDelay     = 200 * time.Millisecond // gather events this long before posting
	maxBatch       = 50
	maxQueue       = 1000 // beyond this, events are dropped
)

// Options configure a client; Start fills them from the environment.
type Options struct {
	// Endpoint to post batches to; "" disables the client.
	Endpoint string
	// Dir holds the install-id file.
	Dir string
	// Version is the CLI version, sent with every event.
	Version string
	// HTTP is the client to post with (default: one with requestTimeout).
	HTTP *http.Client
}

type event struct {
	Event      string         `json:"event"`
	Properties map[string]any `json:"properties"`
	Timestamp  string         `json:"timestamp"`
}

type client struct {
	endpoint, id, version string
	http                  *http.Client
	ctx                   context.Context
	cancel                context.CancelFunc

	mu      sync.Mutex
	queue   []event
	mode    string
	agent   string
	step    string
	started bool

	wake      chan struct{}
	posting   sync.WaitGroup
	closeOnce sync.Once
	closed    chan struct{}
	quitOnce  sync.Once
}

var current atomic.Pointer[client]

// Start sets up the process-wide client from the environment: the default
// endpoint, or $BENCHOUSE_TELEMETRY_ENDPOINT when set. Inside a test binary
// nothing is sent unless that variable names an endpoint.
func Start(version string) {
	endpoint := DefaultEndpoint
	if v, ok := os.LookupEnv(EnvEndpoint); ok {
		endpoint = strings.TrimSpace(v)
	} else if testing.Testing() {
		endpoint = ""
	}
	dir := ""
	if d, err := os.UserConfigDir(); err == nil {
		dir = filepath.Join(d, "benchouse")
	}
	StartWith(Options{Endpoint: endpoint, Dir: dir, Version: version})
}

// StartWith sets up the process-wide client with o, replacing any earlier
// one (which is dropped, not flushed). With no endpoint it does nothing and
// writes no install id.
func StartWith(o Options) {
	if old := current.Swap(nil); old != nil {
		old.cancel()
	}
	if o.Endpoint == "" {
		return
	}
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{endpoint: o.Endpoint, id: installID(o.Dir), version: o.Version, http: hc,
		ctx: ctx, cancel: cancel, mode: "scripted", step: "start",
		wake: make(chan struct{}, 1), closed: make(chan struct{})}
	current.Store(c)
	go c.loop()
}

// Enabled reports whether events are being sent.
func Enabled() bool { return current.Load() != nil }

// SetMode sets the mode sent with every later event: tui, plain or scripted.
func SetMode(mode string) {
	if c := current.Load(); c != nil {
		c.mu.Lock()
		c.mode = mode
		c.mu.Unlock()
	}
}

// track queues an event. Property values must already be safe to send.
func track(name string, props map[string]any) {
	c := current.Load()
	if c == nil {
		return
	}
	p := map[string]any{}
	for k, v := range props {
		p[k] = v
	}
	c.mu.Lock()
	p["distinct_id"] = c.id
	p["cli_version"] = c.version
	p["os"] = runtime.GOOS
	p["arch"] = runtime.GOARCH
	p["mode"] = c.mode
	p["$process_person_profile"] = false // anonymous: no person profile
	// PostHog records the request's IP as $ip unless the event sets it;
	// null keeps it out of the event, and no GeoIP lookup is made. The
	// address still reaches PostHog's servers as part of the connection.
	p["$ip"] = nil
	p["$geoip_disable"] = true
	if len(c.queue) < maxQueue {
		c.queue = append(c.queue, event{Event: name, Properties: p, Timestamp: time.Now().UTC().Format(time.RFC3339Nano)})
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *client) take() []event {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := min(len(c.queue), maxBatch)
	b := c.queue[:n:n]
	c.queue = c.queue[n:]
	return b
}

func (c *client) loop() {
	for {
		select {
		case <-c.wake:
		case <-c.ctx.Done():
			return
		}
		select {
		case <-time.After(batchDelay):
		case <-c.ctx.Done():
			return
		}
		for b := c.take(); len(b) > 0; b = c.take() {
			c.post(b)
		}
	}
}

func (c *client) post(b []event) {
	c.posting.Add(1)
	defer c.posting.Done()
	body, err := json.Marshal(map[string]any{"api_key": APIKey, "batch": b})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
}

// Close sends what is queued, waiting at most budget, then stops the
// client: anything unsent is dropped. Safe to call more than once and from
// several goroutines; each call waits at most budget.
func Close(budget time.Duration) {
	c := current.Load()
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		go func() {
			for b := c.take(); len(b) > 0; b = c.take() {
				c.post(b)
			}
			c.posting.Wait()
			close(c.closed)
		}()
	})
	select {
	case <-c.closed:
	case <-time.After(budget):
	}
	c.cancel()
}

var idRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// installID reads the install id in dir, creating it (0600) on first run.
// When it cannot be kept, a fresh id is used for this run only.
func installID(dir string) string {
	if dir == "" {
		return newUUID()
	}
	p := filepath.Join(dir, "install-id")
	if b, err := os.ReadFile(p); err == nil {
		if id := strings.TrimSpace(string(b)); idRe.MatchString(id) {
			return id
		}
	}
	id := newUUID()
	if os.MkdirAll(dir, 0o700) == nil {
		os.WriteFile(p, []byte(id+"\n"), 0o600)
	}
	return id
}

// newUUID is a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
