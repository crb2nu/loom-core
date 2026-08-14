package cfaccess

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	testKID   = "test-key-1"
	testAUD   = "aud-123"
	testEmail = "cody.r.blevins@gmail.com"
)

// jwksServer serves the RSA public key at the Cloudflare Access certs path and
// returns a signer for minting tokens that verify against it.
func jwksServer(t *testing.T) (issuer string, signer jose.Signer, priv *rsa.PrivateKey) {
	t.Helper()
	var err error
	priv, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: priv.Public(), KeyID: testKID, Algorithm: "RS256", Use: "sig",
	}}}
	body, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cdn-cgi/access/certs" {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	signer, err = jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", testKID),
	)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return srv.URL, signer, priv
}

func mint(t *testing.T, signer jose.Signer, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	tok, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return tok
}

func claims(issuer, aud, email string, exp time.Time) map[string]any {
	return map[string]any{
		"iss":   issuer,
		"aud":   []string{aud},
		"email": email,
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"exp":   exp.Unix(),
		"sub":   "user-1",
	}
}

func reqWithToken(tok string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/labs/auth-check", nil)
	if tok != "" {
		r.Header.Set(HeaderJWT, tok)
	}
	return r
}

func TestVerifier_AcceptsAllowlistedAdmin(t *testing.T) {
	issuer, signer, _ := jwksServer(t)
	v := New(context.Background(), Config{TeamDomain: issuer, AUD: testAUD, AdminEmails: []string{testEmail}})
	if !v.Enabled() || !v.AUDChecked() {
		t.Fatalf("expected enabled + aud-checked")
	}
	tok := mint(t, signer, claims(issuer, testAUD, "Cody.R.Blevins@Gmail.com", time.Now().Add(time.Hour)))
	email, ok := v.AdminEmail(reqWithToken(tok))
	if !ok {
		t.Fatal("expected admin ok")
	}
	if email != testEmail {
		t.Fatalf("email = %q, want lowercased %q", email, testEmail)
	}
}

func TestVerifier_RejectsNonAdminEmail(t *testing.T) {
	issuer, signer, _ := jwksServer(t)
	v := New(context.Background(), Config{TeamDomain: issuer, AUD: testAUD, AdminEmails: []string{testEmail}})
	tok := mint(t, signer, claims(issuer, testAUD, "intruder@example.com", time.Now().Add(time.Hour)))
	if _, ok := v.AdminEmail(reqWithToken(tok)); ok {
		t.Fatal("non-admin email must not be authorized")
	}
}

func TestVerifier_RejectsWrongAUD(t *testing.T) {
	issuer, signer, _ := jwksServer(t)
	v := New(context.Background(), Config{TeamDomain: issuer, AUD: testAUD, AdminEmails: []string{testEmail}})
	tok := mint(t, signer, claims(issuer, "some-other-app", testEmail, time.Now().Add(time.Hour)))
	if _, ok := v.AdminEmail(reqWithToken(tok)); ok {
		t.Fatal("token for a different app AUD must be rejected")
	}
}

func TestVerifier_RejectsExpired(t *testing.T) {
	issuer, signer, _ := jwksServer(t)
	v := New(context.Background(), Config{TeamDomain: issuer, AUD: testAUD, AdminEmails: []string{testEmail}})
	tok := mint(t, signer, claims(issuer, testAUD, testEmail, time.Now().Add(-time.Hour)))
	if _, ok := v.AdminEmail(reqWithToken(tok)); ok {
		t.Fatal("expired token must be rejected")
	}
}

func TestVerifier_RejectsBadSignature(t *testing.T) {
	issuer, _, _ := jwksServer(t)
	v := New(context.Background(), Config{TeamDomain: issuer, AUD: testAUD, AdminEmails: []string{testEmail}})
	// Sign with a DIFFERENT key under the same kid — the forged token cannot
	// verify against the server's published key (the in-cluster spoof case).
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	forger, _ := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: otherKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", testKID),
	)
	tok := mint(t, forger, claims(issuer, testAUD, testEmail, time.Now().Add(time.Hour)))
	if _, ok := v.AdminEmail(reqWithToken(tok)); ok {
		t.Fatal("a token not signed by Cloudflare must be rejected")
	}
}

func TestVerifier_NoHeaderIsNotAdmin(t *testing.T) {
	issuer, _, _ := jwksServer(t)
	v := New(context.Background(), Config{TeamDomain: issuer, AUD: testAUD, AdminEmails: []string{testEmail}})
	if _, ok := v.AdminEmail(reqWithToken("")); ok {
		t.Fatal("a request without the Access header is not admin")
	}
}

func TestVerifier_AUDOptional(t *testing.T) {
	issuer, signer, _ := jwksServer(t)
	// No AUD configured → aud check skipped, but signature+issuer+email still gate.
	v := New(context.Background(), Config{TeamDomain: issuer, AdminEmails: []string{testEmail}})
	if v.AUDChecked() {
		t.Fatal("aud should not be checked when unset")
	}
	tok := mint(t, signer, claims(issuer, "whatever-app", testEmail, time.Now().Add(time.Hour)))
	if _, ok := v.AdminEmail(reqWithToken(tok)); !ok {
		t.Fatal("with aud unset, a valid signed token for an admin email should pass")
	}
}

func TestVerifier_DisabledWhenUnconfigured(t *testing.T) {
	if v := New(context.Background(), Config{AdminEmails: []string{testEmail}}); v.Enabled() {
		t.Fatal("no team domain → disabled")
	}
	if v := New(context.Background(), Config{TeamDomain: "team.cloudflareaccess.com"}); v.Enabled() {
		t.Fatal("no admin emails → disabled")
	}
	// A nil verifier is safe to call.
	var nilV *Verifier
	if _, ok := nilV.AdminEmail(reqWithToken("x")); ok {
		t.Fatal("nil verifier must report not-admin")
	}
}

func TestNormalizeIssuer(t *testing.T) {
	cases := map[string]string{
		"team.cloudflareaccess.com":          "https://team.cloudflareaccess.com",
		"https://team.cloudflareaccess.com/": "https://team.cloudflareaccess.com",
		"http://127.0.0.1:8080":              "http://127.0.0.1:8080",
		"":                                   "",
		"  team.cloudflareaccess.com  ":      "https://team.cloudflareaccess.com",
	}
	for in, want := range cases {
		if got := normalizeIssuer(in); got != want {
			t.Errorf("normalizeIssuer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseEmails(t *testing.T) {
	got := ParseEmails("a@x.com, b@y.com;c@z.com  d@w.com")
	want := []string{"a@x.com", "b@y.com", "c@z.com", "d@w.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if len(ParseEmails("   ")) != 0 {
		t.Fatal("blank → empty")
	}
}
