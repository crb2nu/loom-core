package clients

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func newProjectsTestClient(t *testing.T, rt *recordingTransport) *GitLabClient {
	t.Helper()
	c, err := NewGitLabClient(GitLabConfig{
		APIURL:  "https://gitlab.example/api/v4",
		Token:   "tok-group",
		Project: "services/loom-core",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	c.SetTransport(rt)
	return c
}

func TestLookupNamespaceID_ExactFullPathMatch(t *testing.T) {
	rt := &recordingTransport{routes: map[string]func(*http.Request) (int, any){
		"GET /api/v4/namespaces": func(*http.Request) (int, any) {
			// GitLab search is substring-based: "services" also matches
			// "archived-services". Only the exact full_path may win.
			return 200, []map[string]any{
				{"id": 7, "full_path": "archived-services"},
				{"id": 3, "full_path": "services"},
			}
		},
	}}
	c := newProjectsTestClient(t, rt)

	id, err := c.LookupNamespaceID(context.Background(), "services")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if id != 3 {
		t.Errorf("namespace id = %d, want 3", id)
	}
}

func TestLookupNamespaceID_NotFound(t *testing.T) {
	rt := &recordingTransport{routes: map[string]func(*http.Request) (int, any){
		"GET /api/v4/namespaces": func(*http.Request) (int, any) { return 200, []map[string]any{} },
	}}
	c := newProjectsTestClient(t, rt)

	_, err := c.LookupNamespaceID(context.Background(), "nope")
	if !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("err = %v, want ErrNamespaceNotFound", err)
	}
}

func TestCreateProject_PostsExpectedPayload(t *testing.T) {
	rt := &recordingTransport{routes: map[string]func(*http.Request) (int, any){
		"POST /api/v4/projects": func(*http.Request) (int, any) {
			return 201, map[string]any{
				"id":                  99,
				"path_with_namespace": "services/procmodel",
				"web_url":             "https://gitlab.example/services/procmodel",
				"default_branch":      "main",
			}
		},
	}}
	c := newProjectsTestClient(t, rt)

	got, err := c.CreateProject(context.Background(), CreateProjectRequest{
		Path:        "procmodel",
		NamespaceID: 3,
		Description: "Business process analyzer",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got.PathWithNamespace != "services/procmodel" || got.ID != 99 {
		t.Errorf("resp = %+v", got)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(rt.requests[0].Body), &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	// Defaults: name falls back to path, visibility private, branch main,
	// and the repo is created EMPTY (the seed commit is the root commit).
	for k, want := range map[string]any{
		"name":                   "procmodel",
		"path":                   "procmodel",
		"namespace_id":           float64(3),
		"visibility":             "private",
		"default_branch":         "main",
		"initialize_with_readme": false,
		"description":            "Business process analyzer",
	} {
		if body[k] != want {
			t.Errorf("payload[%s] = %v, want %v", k, body[k], want)
		}
	}
}

func TestProjectExists_TrueWithWebURL(t *testing.T) {
	rt := &recordingTransport{routes: map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects": func(*http.Request) (int, any) {
			return 200, map[string]any{
				"id":                  99,
				"path_with_namespace": "services/familyforge",
				"web_url":             "https://gitlab.example/services/familyforge",
			}
		},
	}}
	c := newProjectsTestClient(t, rt)

	exists, url, err := c.ProjectExists(context.Background(), "services/familyforge")
	if err != nil {
		t.Fatalf("ProjectExists: %v", err)
	}
	if !exists || url != "https://gitlab.example/services/familyforge" {
		t.Errorf("exists=%v url=%q, want true + web url", exists, url)
	}
	// The project path must be percent-encoded in the request.
	if !strings.Contains(rt.requests[0].Path, "services%2Ffamilyforge") {
		t.Errorf("request path = %q, want encoded project segment", rt.requests[0].Path)
	}
}

func TestProjectExists_FalseOn404(t *testing.T) {
	// No routes → the transport's default 404. A missing repo is a clean
	// negative, not an error (the bootstrap pre-flight trigger).
	c := newProjectsTestClient(t, &recordingTransport{})

	exists, url, err := c.ProjectExists(context.Background(), "services/nope")
	if err != nil {
		t.Fatalf("404 must be a nil error, got %v", err)
	}
	if exists || url != "" {
		t.Errorf("exists=%v url=%q, want false + empty", exists, url)
	}
}

func TestProjectExists_ErrorOnNon404(t *testing.T) {
	rt := &recordingTransport{routes: map[string]func(*http.Request) (int, any){
		"GET /api/v4/projects": func(*http.Request) (int, any) {
			return 500, map[string]any{"message": "boom"}
		},
	}}
	c := newProjectsTestClient(t, rt)

	// A 5xx (or 403) must surface as an error so the caller defers instead of
	// mistaking a transient/permissions problem for a missing repo.
	if _, _, err := c.ProjectExists(context.Background(), "services/x"); err == nil {
		t.Fatal("expected an error on a 500 response")
	} else if status, ok := GitLabHTTPStatus(err); !ok || status != http.StatusInternalServerError {
		t.Fatalf("GitLabHTTPStatus=(%d,%v), want (500,true): %v", status, ok, err)
	}
}

func TestProjectExists_RequiresProject(t *testing.T) {
	c := newProjectsTestClient(t, &recordingTransport{})
	if _, _, err := c.ProjectExists(context.Background(), "  "); err == nil {
		t.Error("empty project must error")
	}
}

func TestCreateProject_RequiresPathAndNamespace(t *testing.T) {
	c := newProjectsTestClient(t, &recordingTransport{})
	if _, err := c.CreateProject(context.Background(), CreateProjectRequest{NamespaceID: 3}); err == nil ||
		!strings.Contains(err.Error(), "Path required") {
		t.Errorf("missing path err = %v", err)
	}
	if _, err := c.CreateProject(context.Background(), CreateProjectRequest{Path: "x"}); err == nil ||
		!strings.Contains(err.Error(), "NamespaceID required") {
		t.Errorf("missing namespace err = %v", err)
	}
}
