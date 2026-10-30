// Package netsafe holds the rules every connection the CLI makes follows, so
// credentials only ever travel encrypted and only to the host the user gave:
//
//   - HTTP: credentials go over https; plain http only to this machine
//     (localhost, 127.0.0.0/8, ::1). Redirects are never followed, so a
//     token or an email header cannot be forwarded to another host.
//   - Postgres and MySQL: TLS is required by default for any host but this
//     machine. Weaker modes stay available when asked for explicitly, with a
//     warning.
package netsafe

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Warnings receives the warnings for weak connection settings the user
// chose explicitly. Tests replace it.
var Warnings io.Writer = os.Stderr

func warn(format string, a ...any) { fmt.Fprintf(Warnings, "warning: "+format+"\n", a...) }

// IsLoopback reports whether host (without a port) is this machine:
// localhost, an address in 127.0.0.0/8, ::1, or a unix socket directory. It
// never resolves names, so localhost.example.com is not loopback.
func IsLoopback(host string) bool {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "/") {
		return true // a unix socket directory
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Auto is the default TLS mode: computed from the host when connecting.
const Auto = "auto"

// PGSSLModes are the choices of a Postgres sslmode field.
var PGSSLModes = []string{Auto, "disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// PGSSLHelp is the help line of a Postgres sslmode field.
const PGSSLHelp = "auto = require (prefer for localhost), or verify-full when a CA certificate is given; " +
	"disable only for a server without TLS"

// PGSSLMode returns the sslmode to connect to host with. mode is what the
// user chose; empty or auto becomes require, prefer on this machine, and
// verify-full when rootCert (a CA file) is given. A mode that can fall back
// to plain text for a remote host is kept, with a warning.
func PGSSLMode(host, mode, rootCert string) string {
	mode = strings.TrimSpace(mode)
	switch mode {
	case "", Auto:
		switch {
		case rootCert != "":
			return "verify-full"
		case IsLoopback(host):
			return "prefer"
		}
		return "require"
	case "disable", "allow", "prefer":
		if !IsLoopback(host) {
			warn("--sslmode %s lets the connection to %s, password included, travel unencrypted; "+
				"leave --sslmode unset to require TLS", mode, host)
		}
	}
	return mode
}

// MySQLTLSModes are the choices of a MySQL tls field.
var MySQLTLSModes = []string{Auto, "true", "false", "skip-verify", "preferred"}

// MySQLTLSHelp is the help line of a MySQL tls field.
const MySQLTLSHelp = "auto = true, a verified certificate (preferred for localhost); skip-verify and false only when asked for"

// MySQLTLS returns the go-sql-driver tls setting to connect to host with.
// Empty or auto becomes true (TLS with a verified certificate), preferred on
// this machine. false, skip-verify and preferred for a remote host are kept,
// with a warning.
func MySQLTLS(host, mode string) string {
	mode = strings.TrimSpace(mode)
	switch mode {
	case "", Auto:
		if IsLoopback(host) {
			return "preferred"
		}
		return "true"
	case "false", "preferred":
		if !IsLoopback(host) {
			warn("--tls %s lets the connection to %s, password included, travel unencrypted; "+
				"leave --tls unset to require a verified TLS connection", mode, host)
		}
	case "skip-verify":
		if !IsLoopback(host) {
			warn("--tls skip-verify encrypts the connection to %s but does not check whose certificate it is; "+
				"leave --tls unset to verify it", host)
		}
	}
	return mode
}

var hostname = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// BaseURL checks a service address the CLI sends credentials or an email to,
// and returns it as scheme://host[:port][/path] with no trailing slash. A
// missing scheme means https. http is accepted only for this machine. A
// path is accepted only when allowPath (a self-hosted service under a
// prefix); user info, a query or a fragment never.
func BaseURL(raw string, allowPath bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("no address given")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	host := u.Hostname()
	switch {
	case u.Scheme != "https" && u.Scheme != "http":
		return "", fmt.Errorf("%q: only https addresses are accepted", raw)
	case u.User != nil:
		return "", fmt.Errorf("%q: give the host without a user name or password in it", u.Redacted())
	case host == "":
		return "", fmt.Errorf("%q has no host", raw)
	case net.ParseIP(host) == nil && !hostname.MatchString(host):
		return "", fmt.Errorf("%q: %q is not a host name", raw, host)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", fmt.Errorf("%q: give the address without a query or fragment", raw)
	case u.Scheme == "http" && !IsLoopback(host):
		return "", fmt.Errorf("%q: credentials are only sent over https; use https://%s", raw, u.Host)
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	if path != "" && !allowPath {
		return "", fmt.Errorf("%q: give the host only, without a path", raw)
	}
	return u.Scheme + "://" + u.Host + path, nil
}

// Client is an http.Client that never follows a redirect: a redirect to
// another host would carry the request's credentials there, and the hosts
// the CLI talks to answer API calls directly.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: refuseRedirect}
}

func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("the server redirected to %s://%s%s, which the CLI does not follow "+
		"so credentials never leave the host you gave; check the address", req.URL.Scheme, req.URL.Host, req.URL.EscapedPath())
}
