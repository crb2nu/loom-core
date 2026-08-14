package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeGitLabResponseRecursive(t *testing.T) {
	input := map[string]any{
		"id":            float64(47),
		"runners_token": "runner-secret",
		"nested": map[string]any{
			"access_token": "access-secret",
			"password":     "password-secret",
			"secret":       "generic-secret",
			"safe":         "visible",
		},
		"items": []any{map[string]any{
			"refresh_token": "refresh-secret",
			"import_url":    "https://oauth2:secret@gitlab.example/project.git",
		}},
	}

	got := sanitizeGitLabResponse(input).(map[string]any)
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal sanitized response: %v", err)
	}
	text := string(encoded)
	for _, secret := range []string{"runner-secret", "access-secret", "password-secret", "generic-secret", "refresh-secret", "oauth2:secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("sanitized response still contains %q: %s", secret, text)
		}
	}
	if got["id"] != float64(47) || got["nested"].(map[string]any)["safe"] != "visible" {
		t.Fatalf("safe fields changed: %#v", got)
	}
}

func TestSanitizeGitLabErrorText(t *testing.T) {
	configured := "configured-secret"
	input := `failed PRIVATE-TOKEN: glpat-leaked Authorization: Bearer bearer-leaked configured-secret`
	got := sanitizeGitLabErrorText(input, configured)
	for _, secret := range []string{"glpat-leaked", "bearer-leaked", configured} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitized error still contains %q: %s", secret, got)
		}
	}
}

func TestHandleGetProjectRedactsRunnerToken(t *testing.T) {
	result, err := gitlabJSONResult(map[string]any{
		"id":            47,
		"runners_token": "runner-secret",
	})
	if err != nil {
		t.Fatalf("gitlabJSONResult: %v", err)
	}
	parsed := mustParseJSON(t, result)
	if parsed["runners_token"] != redactedValue {
		t.Fatalf("runners_token = %v, want %q", parsed["runners_token"], redactedValue)
	}
}
