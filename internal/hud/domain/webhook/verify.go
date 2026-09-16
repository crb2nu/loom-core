package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// verifyGitLabToken checks the X-Gitlab-Token header against the configured secret
// using constant-time comparison. An empty secret fails closed — there is
// nothing to verify against, so every request is rejected. Anything else
// would leave the endpoint unauthenticated when the secret is unset
// (docs/harvest-gitlab-hookify-notify-contract.md §4.1).
func verifyGitLabToken(headerToken, secret string) bool {
	if secret == "" {
		return false // fail closed: no secret configured, reject all requests
	}
	return subtle.ConstantTimeCompare([]byte(headerToken), []byte(secret)) == 1
}

// verifyGitHubSignature checks the X-Hub-Signature-256 header against an HMAC-SHA256
// of the request body using the configured secret. An empty secret fails
// closed, same as verifyGitLabToken.
func verifyGitHubSignature(signature, secret string, body []byte) bool {
	if secret == "" {
		return false // fail closed: no secret configured, reject all requests
	}
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sigHex := strings.TrimPrefix(signature, "sha256=")
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)

	return hmac.Equal(sigBytes, expected)
}

// computeGitHubSignature returns a "sha256=<hex>" signature for testing.
func computeGitHubSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return fmt.Sprintf("sha256=%s", hex.EncodeToString(mac.Sum(nil)))
}
