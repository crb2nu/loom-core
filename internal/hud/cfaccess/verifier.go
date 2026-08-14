// Package cfaccess verifies Cloudflare Access application JWTs so the HUD can
// grant admin to an SSO-authenticated user (e.g. a Google/Gmail login) without
// a manually-entered admin token.
//
// Security model: Cloudflare Access validates the user's identity at the edge
// and injects a signed `Cf-Access-Jwt-Assertion` header into every request it
// forwards to the origin. We MUST verify that JWT's signature against the
// team's JWKS (plus issuer / aud / exp) — NOT trust the bare
// `Cf-Access-Authenticated-User-Email` header — because an in-cluster caller
// can reach the HUD's ClusterIP directly (bypassing Cloudflare) and set any
// header it likes. A forged header carries no valid Cloudflare signature, so
// signature verification is what actually closes the spoofing hole; the manual
// admin token stays as the LAN / CLI / non-browser fallback.
package cfaccess

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// HeaderJWT is the header Cloudflare Access injects at the edge.
const HeaderJWT = "Cf-Access-Jwt-Assertion"

// Config configures the verifier. The feature is OFF (New returns nil) unless
// TeamDomain and at least one admin email are set.
type Config struct {
	// TeamDomain is the Cloudflare Access team domain, e.g.
	// "flexinfer.cloudflareaccess.com" (a bare host or a full https URL). It is
	// both the token issuer and the JWKS host.
	TeamDomain string
	// AUD is the Access application's Audience (AUD) tag. When set, the token's
	// `aud` must contain it (scopes the token to THIS app). When empty, the aud
	// check is skipped — signature+issuer+expiry+email still gate, which already
	// blocks header spoofing; setting AUD additionally prevents replay of a
	// token minted for a different app in the same Access team.
	AUD string
	// AdminEmails is the allowlist of emails granted HUD admin (case-insensitive).
	AdminEmails []string
	// VerifyTimeout bounds a single token verification (incl. a lazy JWKS fetch)
	// so a slow certs endpoint can't hang a request. Default 5s.
	VerifyTimeout time.Duration
}

// Verifier verifies Access JWTs and checks the email against the admin allowlist.
// A nil *Verifier is valid and always reports "not an admin" (feature disabled).
type Verifier struct {
	verifier    *oidc.IDTokenVerifier
	adminEmails map[string]struct{}
	timeout     time.Duration
	audChecked  bool
}

// New builds a Verifier from cfg. It returns (nil, nil) when the feature is
// disabled (no team domain, or no admin emails) so callers can treat a nil
// verifier as "Access admin off" and fall back to the token gate.
//
// ctx governs the background JWKS key-set refresh for the life of the process;
// pass a long-lived context (not a per-request one).
func New(ctx context.Context, cfg Config) *Verifier {
	issuer := normalizeIssuer(cfg.TeamDomain)
	emails := normalizeEmails(cfg.AdminEmails)
	if issuer == "" || len(emails) == 0 {
		return nil
	}
	keySet := oidc.NewRemoteKeySet(ctx, issuer+"/cdn-cgi/access/certs")
	oidcCfg := &oidc.Config{
		// Cloudflare Access signs with RS256; pin it rather than accept the
		// library default set.
		SupportedSigningAlgs: []string{oidc.RS256},
	}
	if cfg.AUD != "" {
		oidcCfg.ClientID = cfg.AUD
	} else {
		oidcCfg.SkipClientIDCheck = true
	}
	timeout := cfg.VerifyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Verifier{
		verifier:    oidc.NewVerifier(issuer, keySet, oidcCfg),
		adminEmails: emails,
		timeout:     timeout,
		audChecked:  cfg.AUD != "",
	}
}

// Enabled reports whether Access-admin verification is active.
func (v *Verifier) Enabled() bool { return v != nil }

// AUDChecked reports whether the aud claim is being verified (an AUD was set).
func (v *Verifier) AUDChecked() bool { return v != nil && v.audChecked }

// AdminEmail verifies the request's Access JWT and returns the authenticated
// email IFF it is a valid Cloudflare Access token for an allowlisted admin.
// ok=false when the feature is off, there's no token, the token is invalid, or
// the email isn't an admin. It never writes to the response and never errors —
// a non-admin simply gets ok=false so the caller falls through to the token gate.
func (v *Verifier) AdminEmail(r *http.Request) (email string, ok bool) {
	if v == nil || r == nil {
		return "", false
	}
	raw := strings.TrimSpace(r.Header.Get(HeaderJWT))
	if raw == "" {
		return "", false
	}
	ctx := r.Context()
	if v.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, v.timeout)
		defer cancel()
	}
	tok, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return "", false
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := tok.Claims(&claims); err != nil {
		return "", false
	}
	got := strings.ToLower(strings.TrimSpace(claims.Email))
	if got == "" {
		return "", false
	}
	if _, isAdmin := v.adminEmails[got]; !isAdmin {
		return "", false
	}
	return got, true
}

// normalizeIssuer returns the Cloudflare Access issuer URL for a team domain:
// a full "https://<team>.cloudflareaccess.com" with no trailing slash. Accepts
// a bare host or an http(s) URL; forces https (Access is always https).
func normalizeIssuer(teamDomain string) string {
	d := strings.TrimSpace(teamDomain)
	if d == "" {
		return ""
	}
	d = strings.TrimSuffix(d, "/")
	switch {
	// Respect an explicit scheme (an http:// URL is only ever used by tests
	// pointing at a local JWKS server; production passes a bare domain).
	case strings.HasPrefix(d, "https://"), strings.HasPrefix(d, "http://"):
		return d
	default:
		return "https://" + d
	}
}

// normalizeEmails lowercases + trims the allowlist and drops blanks.
func normalizeEmails(emails []string) map[string]struct{} {
	out := make(map[string]struct{}, len(emails))
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			out[e] = struct{}{}
		}
	}
	return out
}

// ParseEmails splits a comma/space/semicolon-separated allowlist string (e.g.
// from an env var) into a slice for Config.AdminEmails.
func ParseEmails(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
