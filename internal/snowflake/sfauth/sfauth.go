// Package sfauth authenticates requests to the Snowflake SQL API with either a
// programmatic access token (PAT) or a JWT signed by the user's RSA key.
//
// The JWT is signed here rather than through a library because only signing is
// needed: RS256 is a SHA-256 and an rsa.SignPKCS1v15, and it keeps the CLI free
// of a Snowflake driver.
//
// Passphrase-protected keys are refused with the openssl command that removes
// the passphrase; Go's standard library cannot open an encrypted PKCS#8 key.
package sfauth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The values of a Snowflake connection's `auth` config key. Absent means PAT,
// so every connection made before key pairs existed reads as what it is.
const (
	AuthPAT     = "pat"
	AuthKeyPair = "keypair"
)

// TokenTypeHeader decides whether Snowflake reads the bearer at all: without
// it a PAT is taken for a session token and refused with a bare 401, with no
// hint that the token itself was fine.
const TokenTypeHeader = "X-Snowflake-Authorization-Token-Type"

// Credential authenticates one request.
type Credential interface {
	// Authorize sets the Authorization and token-type headers.
	Authorize(req *http.Request) error
	// Rejected is appended to a 401, so the person fixing it is told what we
	// sent rather than only that it was refused.
	Rejected() string
}

// New returns the credential a connection's auth mode and secret describe. A
// key that does not parse yields a credential whose every use fails with the
// reason: the constructors that call this have no error to return, and a
// connection that cannot authenticate should say why on first use rather
// than send a request that 401s.
func New(auth, account, user, secret string) Credential {
	switch auth {
	case "", AuthPAT:
		return PAT(strings.TrimSpace(secret))
	case AuthKeyPair:
		kp, err := NewKeyPair(account, user, secret)
		if err != nil {
			return broken{err}
		}
		return kp
	}
	return broken{fmt.Errorf("unknown snowflake auth %q", auth)}
}

// Validate is New's error, for checking a connection before it is stored.
func Validate(auth, account, user, secret string) error {
	if b, ok := New(auth, account, user, secret).(broken); ok {
		return b.err
	}
	return nil
}

// Hint is how a stored private key is recognised without revealing it: its
// public key's fingerprint, the same string Snowflake shows in DESC USER as
// RSA_PUBLIC_KEY_FP. Empty when the secret is not a usable key.
func Hint(secret string) string {
	k, err := parseKey(secret)
	if err != nil {
		return ""
	}
	fp, err := fingerprint(k)
	if err != nil {
		return ""
	}
	return fp
}

// PAT is a programmatic access token.
type PAT string

func (p PAT) Authorize(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+string(p))
	req.Header.Set(TokenTypeHeader, "PROGRAMMATIC_ACCESS_TOKEN")
	return nil
}

func (PAT) Rejected() string {
	return "Snowflake refused the programmatic access token: check it has not expired or been revoked, " +
		"and that the account's network policy admits this machine"
}

type broken struct{ err error }

func (b broken) Authorize(*http.Request) error { return b.err }
func (b broken) Rejected() string              { return b.err.Error() }

// KeyPair signs a short-lived JWT for every request.
//
// Minted per request rather than cached: signing costs about a millisecond
// against a round trip of hundreds, and a token that cannot be stale removes
// the expiry edge from eval runs that share one client for hours.
type KeyPair struct {
	Account     string // the account as it appears in the claims
	User        string
	Fingerprint string // SHA256:<base64>
	key         *rsa.PrivateKey
}

// NewKeyPair parses a PEM private key for account and user.
func NewKeyPair(account, user, secret string) (*KeyPair, error) {
	if strings.TrimSpace(user) == "" {
		return nil, errors.New("key-pair authentication needs the Snowflake user the public key is set on")
	}
	k, err := parseKey(secret)
	if err != nil {
		return nil, err
	}
	fp, err := fingerprint(k)
	if err != nil {
		return nil, err
	}
	return &KeyPair{
		Account:     AccountClaim(account),
		User:        strings.ToUpper(strings.TrimSpace(user)),
		Fingerprint: fp,
		key:         k,
	}, nil
}

// lifetime stays under Snowflake's one-hour maximum, and iat is backdated so
// a clock a little ahead of Snowflake's does not mint a token from the future.
const (
	lifetime = 59 * time.Minute
	backdate = time.Minute
)

func (k *KeyPair) Authorize(req *http.Request) error { return k.authorize(req, time.Now()) }

func (k *KeyPair) authorize(req *http.Request, now time.Time) error {
	tok, err := k.jwt(now)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(TokenTypeHeader, "KEYPAIR_JWT")
	return nil
}

func (k *KeyPair) subject() string { return k.Account + "." + k.User }

func (k *KeyPair) jwt(now time.Time) (string, error) {
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"iss": k.subject() + "." + k.Fingerprint,
		"sub": k.subject(),
		"iat": now.Add(-backdate).Unix(),
		"exp": now.Add(lifetime).Unix(),
	})
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(hdr) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("signing the snowflake jwt: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// Rejected names every part Snowflake compares, in the form it compares them.
// From outside, a wrong account, a wrong user and an unregistered key are the
// same 401.
func (k *KeyPair) Rejected() string {
	return fmt.Sprintf("Snowflake rejected the key-pair JWT (iss %s.%s). One of these does not match: "+
		"account %s (the identifier up to its first dot, uppercased); user %s; "+
		"key fingerprint %s, which must equal RSA_PUBLIC_KEY_FP or RSA_PUBLIC_KEY_2_FP in DESC USER %s",
		k.subject(), k.Fingerprint, k.Account, k.User, k.Fingerprint, k.User)
}

// AccountClaim is the account as a JWT names it: no host suffix, no region or
// cloud (a locator's xy12345.eu-west-1 is XY12345), uppercased. The org-account
// form (ylifatf-rv42873) has no dot and is kept whole.
func AccountClaim(account string) string {
	a := strings.TrimSpace(account)
	a = strings.TrimSuffix(strings.ToLower(a), ".snowflakecomputing.com")
	if i := strings.IndexByte(a, '.'); i >= 0 {
		a = a[:i]
	}
	return strings.ToUpper(a)
}

// fingerprint is what Snowflake computed when the public key was registered:
// SHA-256 over the DER SubjectPublicKeyInfo, standard base64.
func fingerprint(k *rsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return "SHA256:" + base64.StdEncoding.EncodeToString(sum[:]), nil
}

var errPassphrase = errors.New("this private key is protected by a passphrase, which is not supported " +
	"Write an unencrypted copy with: " +
	"openssl pkcs8 -in rsa_key.p8 -out rsa_key_plain.p8 -nocrypt")

func parseKey(secret string) (*rsa.PrivateKey, error) {
	// Before decoding: an encrypted key mangled in the paste should still be
	// told about the passphrase, which is the thing to fix.
	if strings.Contains(secret, "ENCRYPTED") {
		return nil, errPassphrase
	}
	block, _ := pem.Decode([]byte(strings.TrimSpace(secret)))
	if block == nil {
		return nil, errors.New("the private key is not in PEM form: paste the whole file, " +
			"from -----BEGIN PRIVATE KEY----- to -----END PRIVATE KEY-----")
	}
	if strings.Contains(block.Type, "PUBLIC KEY") {
		return nil, errors.New("this is the public key: it goes on the Snowflake user. Paste the private key here")
	}
	var key any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("a PEM %q block is not a private key", block.Type)
	}
	if err != nil {
		// Not wrapped: a parse error can quote the bytes it choked on.
		return nil, errors.New("the private key could not be read; generate it with openssl as the setup guide shows")
	}
	rk, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("snowflake key-pair authentication needs an RSA key")
	}
	if rk.N.BitLen() < 2048 {
		return nil, fmt.Errorf("the key is %d bits; Snowflake requires at least 2048", rk.N.BitLen())
	}
	return rk, nil
}
