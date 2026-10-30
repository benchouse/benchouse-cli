package sfauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testKey is generated per run: a private key in testdata would fail CI's
// secret scan, on this branch and on any branch that merged it.
func testKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pkcs8PEM(t *testing.T, k *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestAccountClaim(t *testing.T) {
	for in, want := range map[string]string{
		"ylifatf-rv42873":                        "YLIFATF-RV42873",
		"xy12345.eu-west-1":                      "XY12345",
		"xy12345.us-east-2.aws":                  "XY12345",
		"xy12345.east-us-2.azure.privatelink":    "XY12345",
		"ylifatf-rv42873.snowflakecomputing.com": "YLIFATF-RV42873",
		" Ylifatf-Rv42873 ":                      "YLIFATF-RV42873",
	} {
		if got := AccountClaim(in); got != want {
			t.Errorf("AccountClaim(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeyPairJWT(t *testing.T) {
	k := testKey(t, 2048)
	cred := New(AuthKeyPair, "xy12345.eu-west-1", "benchouse_read", pkcs8PEM(t, k))
	kp, ok := cred.(*KeyPair)
	if !ok {
		t.Fatalf("got %T, want *KeyPair", cred)
	}

	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	sum := sha256.Sum256(der)
	fp := "SHA256:" + base64.StdEncoding.EncodeToString(sum[:])
	if kp.Fingerprint != fp {
		t.Fatalf("fingerprint %q, want %q", kp.Fingerprint, fp)
	}

	now := time.Unix(1_800_000_000, 0)
	req, _ := http.NewRequest(http.MethodPost, "https://x", nil)
	if err := kp.authorize(req, now); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get(TokenTypeHeader); got != "KEYPAIR_JWT" {
		t.Errorf("token type %q", got)
	}
	tok := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	var hdr, claims map[string]any
	decode := func(s string, v any) {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	decode(parts[0], &hdr)
	decode(parts[1], &claims)
	if hdr["alg"] != "RS256" || hdr["typ"] != "JWT" {
		t.Errorf("header %v", hdr)
	}
	if claims["iss"] != "XY12345.BENCHOUSE_READ."+fp || claims["sub"] != "XY12345.BENCHOUSE_READ" {
		t.Errorf("claims %v", claims)
	}
	iat, exp := int64(claims["iat"].(float64)), int64(claims["exp"].(float64))
	if iat >= now.Unix() || exp-now.Unix() > 3600 || exp <= now.Unix() {
		t.Errorf("iat %d exp %d around now %d", iat, exp, now.Unix())
	}

	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&k.PublicKey, 0x5 /* crypto.SHA256 */, digest[:], sig); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}
}

func TestPKCS1AndCRLF(t *testing.T) {
	k := testKey(t, 2048)
	p := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	p = strings.ReplaceAll(p, "\n", "\r\n")
	if err := Validate(AuthKeyPair, "acct", "u", p); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedKeys(t *testing.T) {
	k := testKey(t, 2048)
	pub, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	small := testKey(t, 1024)
	for name, tc := range map[string]struct{ secret, user, want string }{
		"encrypted":  {"-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----\n", "u", "-nocrypt"},
		"public key": {string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})), "u", "public key"},
		"not pem":    {"ver:1-hint:abc-secret", "u", "PEM"},
		"short key":  {pkcs8PEM(t, small), "u", "2048"},
		"no user":    {pkcs8PEM(t, k), "", "user"},
	} {
		err := Validate(AuthKeyPair, "acct", tc.user, tc.secret)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want it to mention %q", name, err, tc.want)
		}
		// A bad key must still produce a credential whose use fails legibly.
		req, _ := http.NewRequest(http.MethodGet, "https://x", nil)
		if err := New(AuthKeyPair, "acct", tc.user, tc.secret).Authorize(req); err == nil {
			t.Errorf("%s: Authorize succeeded with a bad key", name)
		}
	}
}

func TestPATIsDefault(t *testing.T) {
	for _, auth := range []string{"", AuthPAT} {
		req, _ := http.NewRequest(http.MethodGet, "https://x", nil)
		if err := New(auth, "acct", "", " tok\n").Authorize(req); err != nil {
			t.Fatal(err)
		}
		if req.Header.Get("Authorization") != "Bearer tok" ||
			req.Header.Get(TokenTypeHeader) != "PROGRAMMATIC_ACCESS_TOKEN" {
			t.Errorf("auth %q: headers %v", auth, req.Header)
		}
	}
	if err := Validate("password", "acct", "u", "x"); err == nil {
		t.Error("unknown auth accepted")
	}
}

func TestRejectionNamesEveryPart(t *testing.T) {
	cred := New(AuthKeyPair, "ylifatf-rv42873", "benchouse_read", pkcs8PEM(t, testKey(t, 2048)))
	msg := cred.Rejected()
	for _, want := range []string{"YLIFATF-RV42873", "BENCHOUSE_READ", "SHA256:", "RSA_PUBLIC_KEY_FP", "DESC USER"} {
		if !strings.Contains(msg, want) {
			t.Errorf("rejection %q does not mention %q", msg, want)
		}
	}
}

func TestHint(t *testing.T) {
	k := testKey(t, 2048)
	if h := Hint(pkcs8PEM(t, k)); !strings.HasPrefix(h, "SHA256:") {
		t.Errorf("hint %q", h)
	}
	if h := Hint("garbage"); h != "" {
		t.Errorf("hint of garbage %q", h)
	}
}
