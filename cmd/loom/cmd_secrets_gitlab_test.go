package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGitLabRotationClientOverlapLifecycle(t *testing.T) {
	const oldSecret = "old-secret"
	const newSecret = "new-secret"
	old := gitLabRotationToken{
		ID: 2, Name: "k3s-automation", UserID: 1, Scopes: []string{"api", "read_user"},
		Active: true, ExpiresAt: "2027-01-02",
	}
	created := gitLabRotationToken{
		ID: 23, Name: "k3s-automation-rtest", UserID: 1, Scopes: []string{"read_user", "api"},
		Active: true, ExpiresAt: "2027-01-02", Token: newSecret,
	}
	var mu sync.Mutex
	revoked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth := r.Header.Get("PRIVATE-TOKEN")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/personal_access_tokens/self" && auth == oldSecret:
			writeRotationJSON(t, w, http.StatusOK, old)
		case r.Method == http.MethodPost && r.URL.Path == "/users/1/personal_access_tokens" && auth == oldSecret:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read create body: %v", err)
			}
			if strings.Contains(string(body), oldSecret) || strings.Contains(string(body), newSecret) {
				t.Error("create payload must not contain either credential")
			}
			writeRotationJSON(t, w, http.StatusCreated, created)
		case r.Method == http.MethodGet && r.URL.Path == "/personal_access_tokens/self" && auth == newSecret:
			info := created
			info.Token = ""
			writeRotationJSON(t, w, http.StatusOK, info)
		case r.Method == http.MethodGet && r.URL.Path == "/personal_access_tokens/2" && auth == newSecret:
			info := old
			info.Revoked = revoked
			info.Active = !revoked
			writeRotationJSON(t, w, http.StatusOK, info)
		case r.Method == http.MethodDelete && r.URL.Path == "/personal_access_tokens/2" && auth == newSecret:
			revoked = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s auth=%q", r.Method, r.URL.Path, auth)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()

	client, err := newGitLabRotationClient(server.URL, "")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	current, err := client.self(t.Context(), oldSecret)
	if err != nil {
		t.Fatalf("self: %v", err)
	}
	if current.ID != old.ID {
		t.Fatalf("old id = %d, want %d", current.ID, old.ID)
	}
	replacement, err := client.createReplacement(t.Context(), oldSecret, current, created.Name)
	if err != nil {
		t.Fatalf("create replacement: %v", err)
	}
	if replacement.Token != newSecret || replacement.ID != created.ID {
		t.Fatalf("unexpected replacement metadata: %#v", replacement)
	}
	if err := client.revoke(t.Context(), newSecret, old.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	oldAfter, err := client.get(t.Context(), newSecret, old.ID)
	if err != nil {
		t.Fatalf("get old after revoke: %v", err)
	}
	if oldAfter.Active || !oldAfter.Revoked {
		t.Fatalf("old token was not revoked: %#v", oldAfter)
	}
}

func TestUpdateGitLabEnvFileAtomicAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ai.env")
	original := "export KEEP='value'\nGITLAB_TOKEN='stale-one'\nexport GITLAB_PERSONAL_ACCESS_TOKEN=stale-two\nGITLAB_TOKEN=duplicate\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	const replacement = "replacement-safe-value"
	if err := updateGitLabEnvFile(path, replacement); err != nil {
		t.Fatalf("update env: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "export KEEP='value'") {
		t.Fatalf("unrelated env entry changed: %s", text)
	}
	for _, key := range []string{"GITLAB_PERSONAL_ACCESS_TOKEN", "GITLAB_TOKEN", "GITLAB_PAT"} {
		line := "export " + key + "='" + replacement + "'"
		if strings.Count(text, line) != 1 {
			t.Fatalf("%s count = %d, want 1: %s", key, strings.Count(text, line), text)
		}
	}
	if strings.Contains(text, "stale-one") || strings.Contains(text, "stale-two") || strings.Contains(text, "duplicate") {
		t.Fatalf("stale values remain: %s", text)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestGitLabRotationStateRequiresPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.json")
	state := gitLabRotationState{
		Version: gitLabRotationStateVersion,
		APIURL:  "https://gitlab.example/api/v4",
		Old:     gitLabRotationToken{ID: 2},
		New:     gitLabRotationToken{ID: 23, Token: "new-secret"},
	}
	if err := writeGitLabRotationState(path, state); err != nil {
		t.Fatalf("write state: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readGitLabRotationState(path); err == nil || !strings.Contains(err.Error(), "must not be accessible") {
		t.Fatalf("read state error = %v, want private-mode failure", err)
	}
}

func TestGitLabRotationClientRejectsNonTLSRemote(t *testing.T) {
	if _, err := newGitLabRotationClient("http://gitlab.example/api/v4", ""); err == nil {
		t.Fatal("expected non-TLS remote URL to be rejected")
	}
}

func TestSanitizeRotationText(t *testing.T) {
	const secret = "glpat-super-secret"
	got := sanitizeRotationText("request failed with "+secret, secret)
	if strings.Contains(got, secret) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("sanitizeRotationText = %q", got)
	}
}

func writeRotationJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}
}

func TestGitLabRotationStateStatusNeverPrintsToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.json")
	state := gitLabRotationState{
		Version:   gitLabRotationStateVersion,
		APIURL:    "https://gitlab.example/api/v4",
		CreatedAt: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
		Old:       gitLabRotationToken{ID: 2, Name: "old", Active: true},
		New:       gitLabRotationToken{ID: 23, Name: "new", Active: true, Token: "must-not-print"},
	}
	if err := writeGitLabRotationState(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := readGitLabRotationState(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(struct {
		OldID int `json:"old_id"`
		NewID int `json:"new_id"`
	}{OldID: loaded.Old.ID, NewID: loaded.New.ID})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), loaded.New.Token) {
		t.Fatalf("status projection contains token: %s", data)
	}
}
