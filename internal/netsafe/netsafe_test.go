package netsafe

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	orig := Warnings
	Warnings = &b
	t.Cleanup(func() { Warnings = orig })
	return &b
}

func TestIsLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "LOCALHOST": true, "127.0.0.1": true, "127.8.9.10": true, "::1": true, "[::1]": true,
		"/var/run/postgresql": true,
		"db.example.com":      false, "localhost.example.com": false, "10.0.0.5": false, "0.0.0.0": false, "": false,
	} {
		if got := IsLoopback(host); got != want {
			t.Errorf("IsLoopback(%q) = %v", host, got)
		}
	}
}

func TestPGSSLModeDefaultsToRequireForRemoteHosts(t *testing.T) {
	w := capture(t)
	for _, c := range []struct{ host, mode, ca, want string }{
		{"db.example.com", "", "", "require"},
		{"db.example.com", Auto, "", "require"},
		{"localhost", Auto, "", "prefer"},
		{"127.0.0.1", "", "", "prefer"},
		{"db.example.com", Auto, "/ca.pem", "verify-full"},
		{"db.example.com", "verify-ca", "/ca.pem", "verify-ca"},
		{"localhost", "disable", "", "disable"},
	} {
		if got := PGSSLMode(c.host, c.mode, c.ca); got != c.want {
			t.Errorf("PGSSLMode(%q, %q, %q) = %q, want %q", c.host, c.mode, c.ca, got, c.want)
		}
	}
	if w.Len() != 0 {
		t.Errorf("warned for a safe choice: %s", w)
	}
	for _, mode := range []string{"disable", "allow", "prefer"} {
		w.Reset()
		if got := PGSSLMode("db.example.com", mode, ""); got != mode {
			t.Errorf("explicit %s became %s", mode, got)
		}
		if !strings.Contains(w.String(), "warning: --sslmode "+mode) {
			t.Errorf("no warning for explicit %s: %q", mode, w)
		}
	}
}

func TestMySQLTLSDefaultsToVerified(t *testing.T) {
	w := capture(t)
	if got := MySQLTLS("db.example.com", ""); got != "true" {
		t.Errorf("remote default = %q", got)
	}
	if got := MySQLTLS("localhost", Auto); got != "preferred" {
		t.Errorf("localhost default = %q", got)
	}
	if w.Len() != 0 {
		t.Errorf("warned for a default: %s", w)
	}
	for _, mode := range []string{"false", "preferred", "skip-verify"} {
		w.Reset()
		if got := MySQLTLS("db.example.com", mode); got != mode || !strings.Contains(w.String(), "warning: --tls "+mode) {
			t.Errorf("explicit %s = %q, warning %q", mode, got, w)
		}
	}
}

func TestBaseURL(t *testing.T) {
	for _, c := range []struct {
		in        string
		allowPath bool
		want      string
	}{
		{"api.benchouse.ai", false, "https://api.benchouse.ai"},
		{"https://api.benchouse.ai/", false, "https://api.benchouse.ai"},
		{"http://localhost:8080", false, "http://localhost:8080"},
		{"http://127.0.0.1:1234/", false, "http://127.0.0.1:1234"},
		{"http://[::1]:9", false, "http://[::1]:9"},
		{"https://example.com/langfuse/", true, "https://example.com/langfuse"},
	} {
		if got, err := BaseURL(c.in, c.allowPath); err != nil || got != c.want {
			t.Errorf("BaseURL(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{
		"", "http://api.benchouse.ai", "http://localhost.evil.tld", "ftp://example.com", "https://u:p@example.com",
		"https://example.com?x=1", "https://example.com#f", "https://example.com/x", "https://exa mple.com",
		"https://-bad.example.com",
	} {
		if got, err := BaseURL(in, false); err == nil {
			t.Errorf("BaseURL(%q) = %q, want an error", in, got)
		}
	}
}

// A redirect is refused, so a header such as Authorization or
// X-Benchouse-Email never reaches the host it points at.
func TestClientRefusesRedirects(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("followed a redirect with Authorization %q", r.Header.Get("Authorization"))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.RedirectHandler(other.URL+"/x", http.StatusMovedPermanently))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer secret")
	_, err := Client(time.Second).Do(req)
	if err == nil || !strings.Contains(err.Error(), "redirected") {
		t.Errorf("err = %v", err)
	}
}
