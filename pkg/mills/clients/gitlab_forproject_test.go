package clients

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGitLabClient_ForProject verifies per-item cross-repo scoping: a different
// project yields a new client bound to it (sharing token/http), while an empty
// project or the same project returns the receiver unchanged.
func TestGitLabClient_ForProject(t *testing.T) {
	var requestedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"iid":17,"state":"opened"}`)
	}))
	defer server.Close()

	base, err := NewGitLabClient(GitLabConfig{
		APIURL:  server.URL + "/api/v4",
		Token:   "tok-123",
		Project: "services/loom-core",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	// Empty project → same receiver.
	if got := base.ForProject(""); got != base {
		t.Errorf("ForProject(\"\") should return the receiver")
	}
	if got := base.ForProject("  "); got != base {
		t.Errorf("ForProject(whitespace) should return the receiver")
	}
	// Same project → same receiver.
	if got := base.ForProject("services/loom-core"); got != base {
		t.Errorf("ForProject(home) should return the receiver")
	}

	// Different project → new client scoped to it, base unchanged.
	scoped := base.ForProject("services/loom-flightdeck")
	if scoped == base {
		t.Fatalf("ForProject(other) should return a distinct client")
	}
	if scoped.cfg.Project != "services/loom-flightdeck" {
		t.Errorf("scoped project = %q, want services/loom-flightdeck", scoped.cfg.Project)
	}
	if base.cfg.Project != "services/loom-core" {
		t.Errorf("base project mutated to %q; ForProject must not alter the receiver", base.cfg.Project)
	}
	// Shared token + http transport (copy is shallow by design).
	if scoped.cfg.Token != base.cfg.Token {
		t.Errorf("scoped token = %q, want shared %q", scoped.cfg.Token, base.cfg.Token)
	}
	if scoped.http != base.http {
		t.Errorf("scoped client should share the base http client")
	}
	// projectPath reflects the override.
	if got := scoped.projectPath(); got != "services%2Floom-flightdeck" {
		t.Errorf("scoped projectPath = %q, want services%%2Floom-flightdeck", got)
	}
	if state, err := scoped.MRState(context.Background(), 17); err != nil {
		t.Fatalf("scoped MRState: %v", err)
	} else if state != "opened" {
		t.Fatalf("scoped MRState = %q, want opened", state)
	}
	if want := "/api/v4/projects/services%2Floom-flightdeck/merge_requests/17"; requestedPath != want {
		t.Fatalf("scoped request path = %q, want %q", requestedPath, want)
	}
	if base.cfg.Project != "services/loom-core" {
		t.Errorf("base project mutated after scoped request to %q", base.cfg.Project)
	}
}
