package telemetry

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/benchouse/benchouse-cli/internal/upload"
	"github.com/jackc/pgx/v5/pgconn"
)

// The error categories Category returns.
const (
	AuthFailed       = "auth_failed"
	PermissionDenied = "permission_denied"
	NotFound         = "not_found"
	NotConfigured    = "not_configured"
	Network          = "network"
	TLS              = "tls"
	Timeout          = "timeout"
	RateLimited      = "rate_limited"
	InvalidInput     = "invalid_input"
	ServerError      = "server_error"
	Canceled         = "canceled"
	Unknown          = "unknown"
)

// Category sorts err into one of the categories above, from typed errors
// first, then HTTP statuses and database codes, then the wording agents
// use. It is "" for nil.
func Category(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Timeout
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		if c := pgCategory(pg.Code); c != "" {
			return c
		}
	}
	var ue x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	if errors.As(err, &ue) || errors.As(err, &he) || errors.As(err, &ci) {
		return TLS
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Timeout
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return Network
	}
	if c := statusCategory(HTTPStatus(err)); c != "" {
		return c
	}
	msg := strings.ToLower(err.Error())
	if m := sqlstateRe.FindStringSubmatch(msg); m != nil {
		if c := pgCategory(strings.ToUpper(m[1])); c != "" {
			return c
		}
	}
	for _, r := range rules {
		for _, k := range r.keys {
			if strings.Contains(msg, k) {
				return r.cat
			}
		}
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return Network
	}
	return Unknown
}

func pgCategory(code string) string {
	switch {
	case code == "28P01" || code == "28000":
		return AuthFailed
	case code == "42501":
		return PermissionDenied
	case code == "42P01" || code == "3D000" || code == "42703" || code == "3F000" || code == "42883":
		return NotFound
	case code == "53300" || code == "53400":
		return RateLimited
	case code == "57014":
		return Timeout
	case strings.HasPrefix(code, "08"):
		return Network
	case strings.HasPrefix(code, "22") || strings.HasPrefix(code, "42"):
		return InvalidInput
	}
	return ""
}

func statusCategory(code int) string {
	switch {
	case code == 401:
		return AuthFailed
	case code == 403:
		return PermissionDenied
	case code == 404 || code == 410:
		return NotFound
	case code == 408 || code == 504:
		return Timeout
	case code == 429:
		return RateLimited
	case code >= 500:
		return ServerError
	case code >= 400:
		return InvalidInput
	}
	return ""
}

var sqlstateRe = regexp.MustCompile(`sqlstate ([0-9a-z]{5})`)

// rules map wording found across the agents to a category, first match
// wins: specific before general.
var rules = []struct {
	cat  string
	keys []string
}{
	{Canceled, []string{"context canceled", "operation was canceled"}},
	{Timeout, []string{"deadline exceeded", "i/o timeout", "timed out", "timeout", "no answer within"}},
	{TLS, []string{"x509", "tls:", "certificate", "handshake failure"}},
	{Network, []string{"no such host", "connection refused", "connection reset", "network is unreachable",
		"no route to host", "broken pipe", ": eof", "dial tcp", "server misbehaving", "unexpected eof"}},
	{AuthFailed, []string{"390100", "390144", "390318", "390303", "incorrect username or password",
		"password authentication failed", "rejected the", "refused the programmatic access token",
		"invalid token", "invalid api key", "unauthorized", "unauthenticated", "authentication failed",
		"bad credentials", "bad token", "invalid credentials", "expired token", "token has expired", "jwt"}},
	{PermissionDenied, []string{"permission", "insufficient privilege", "not authorized", "access denied",
		"forbidden", "is not readable", "does not have privilege"}},
	{RateLimited, []string{"too many requests", "rate limit", "not accepting uploads", "too many connections", "quota"}},
	{NotFound, []string{"does not exist", "not found", "no such file", "is not a folder", "nothing to include",
		"unknown agent", "no rows", "returned no row", "not reachable"}},
	{NotConfigured, []string{" needs --", "is required", "not available", "not configured", "not set up", "not enabled"}},
	{InvalidInput, []string{"is needed", "must be", "not valid", "yaml:", "not an email", "is not a valid", "invalid", "not in pem form", "not supported",
		"only https", "give the", "use 30d", "over the", "too large", "is the public key", "not a private key",
		"bits;", "unexpected argument", "unknown command", "flag provided but not defined"}},
	{ServerError, []string{"internal server error", "bad gateway", "service unavailable", "unexpected answer",
		"unexpected response", "internal error"}},
}

var statusRe = regexp.MustCompile(`(?i)\bhttp[ /]?(?:status )?(\d{3})\b|\((\d{3})\)`)

// HTTPStatus is the HTTP status an error reports: from upload.StatusError,
// or written into its text ("http 401", "(HTTP 403)", "rejected ... (401)").
// It is 0 when there is none.
func HTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	var se *upload.StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	m := statusRe.FindStringSubmatch(err.Error())
	if m == nil {
		return 0
	}
	s := m[1]
	if s == "" {
		s = m[2]
	}
	n, _ := strconv.Atoi(s)
	if n < 100 || n > 599 {
		return 0
	}
	return n
}

// Message is err's text passed through Scrub; "" for nil.
func Message(err error) string {
	if err == nil {
		return ""
	}
	return Scrub(err.Error())
}
