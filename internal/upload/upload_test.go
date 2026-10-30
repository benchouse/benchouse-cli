package upload

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bundleFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := os.WriteFile(p, []byte("gz"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The bundle and the email go over https, except to this machine.
func TestPlainHTTPOnlyToThisMachine(t *testing.T) {
	ctx := context.Background()
	if _, err := Send(ctx, "http://api.example.com", bundleFile(t), "a@b.test", "v"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("Send over http: err = %v", err)
	}
	if err := RequestAgentEmail(ctx, "http://example.com", "x", "a@b.test", ""); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("RequestAgent over http: err = %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/uploads" {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"u1","bytes":2}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if r, err := Send(ctx, srv.URL+"/", bundleFile(t), "a@b.test", "v"); err != nil || r.ID != "u1" {
		t.Errorf("Send to loopback = %v, %v", r, err)
	}
	if err := RequestAgentEmail(ctx, srv.URL, "x", "a@b.test", ""); err != nil {
		t.Errorf("RequestAgent to loopback: %v", err)
	}
}

// A redirect would carry X-Benchouse-Email (and the bundle) to another host.
func TestRedirectsAreRefused(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("followed a redirect; email header %q", r.Header.Get("X-Benchouse-Email"))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.RedirectHandler(other.URL+"/v1/uploads", http.StatusTemporaryRedirect))
	defer srv.Close()
	// net/http cannot replay a file body, so Send sees the 307 itself.
	if _, err := Send(context.Background(), srv.URL, bundleFile(t), "a@b.test", "v"); err == nil {
		t.Errorf("Send: err = %v", err)
	}
	if err := RequestAgentEmail(context.Background(), srv.URL, "x", "a@b.test", ""); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("RequestAgent: err = %v", err)
	}
	// A 302 turns the POST into a GET that net/http would follow with the
	// email header.
	found := httptest.NewServer(http.RedirectHandler(other.URL+"/v1/uploads", http.StatusFound))
	defer found.Close()
	if _, err := Send(context.Background(), found.URL, bundleFile(t), "a@b.test", "v"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("Send after 302: err = %v", err)
	}
}

func TestAgentRequestPayloads(t *testing.T) {
	var got []map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		got, paths = append(got, body), append(paths, r.URL.Path)
		if r.URL.Path == "/api/v1/agent-request" {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"ar_123"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ctx := context.Background()
	id, err := RequestAgentAnonymous(ctx, srv.URL, "Hex Magic")
	if err != nil || id != "ar_123" {
		t.Fatalf("anonymous: %q, %v", id, err)
	}
	if err := RequestAgentEmail(ctx, srv.URL, "Hex Magic", "a@b.test", id); err != nil {
		t.Fatal(err)
	}
	if paths[0] != "/api/v1/agent-request" || got[0]["agent"] != "Hex Magic" || got[0]["source"] != "cli" || len(got[0]) != 2 {
		t.Errorf("anonymous request: %s %v", paths[0], got[0])
	}
	ref, _ := got[1]["ref"].(map[string]any)
	if paths[1] != "/api/v1/waitlist" || got[1]["email"] != "a@b.test" || got[1]["agent"] != "Hex Magic" ||
		got[1]["urgency"] != "cli: requested agent support" || ref["source"] != "benchouse-cli" || ref["content"] != "ar_123" {
		t.Errorf("email request: %s %v", paths[1], got[1])
	}
	if _, err := RequestAgentAnonymous(ctx, "http://example.com", "x"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("anonymous over http: %v", err)
	}
}

func TestAgentRequestFailuresAndOddIDs(t *testing.T) {
	status, answer := http.StatusNotFound, `{}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(answer))
	}))
	defer srv.Close()
	if _, err := RequestAgentAnonymous(context.Background(), srv.URL, "x"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}
	status, answer = http.StatusCreated, `{"id":"../../etc"}`
	if id, err := RequestAgentAnonymous(context.Background(), srv.URL, "x"); err != nil || id != "" {
		t.Errorf("odd id kept: %q, %v", id, err)
	}
}
