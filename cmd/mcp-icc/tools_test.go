package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crb2nu/loom/internal/iccclient"
)

// --- shared helpers ------------------------------------------------------

// newTestICCServer spins an httptest server that the given handler
// drives and returns an iccclient pointed at it. Mirrors the helper
// in cmd/mcp-icc-capture/tools_http_test.go.
func newTestICCServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *iccclient.Client) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	icc := iccclient.NewForTest(srv.URL, srv.Client())
	return srv, icc
}

func readBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(raw) == 0 {
		return map[string]any{}
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode body: %v\n%s", err, string(raw))
	}
	return out
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func decodeResult(t *testing.T, text string, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(text), dst); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text)
	}
}

// --- write gate ---------------------------------------------------------

// All write tools share the same withWriteGate wrapper. Exercising
// one happy path (create_project) plus one URL-templated path
// (artifact_demote) gives high confidence the gate fires everywhere
// without having to write 16+ near-duplicate tests.

func TestWriteGate_DisabledRefusesBeforeNetwork(t *testing.T) {
	called := false
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		writeJSON(t, w, http.StatusOK, map[string]any{})
	})

	// writesEnabled=false → handler should return writes_disabled
	// without ever hitting the network.
	handler := withWriteGate(false, makeCreateHandler(icc, "project_create", "/api/projects"))
	result, err := handler(context.Background(), map[string]any{
		"payload": map[string]any{"name": "X"},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error result, got success: %s", result.Content[0].Text)
	}
	if !strings.Contains(result.Content[0].Text, "writes_disabled") {
		t.Fatalf("expected writes_disabled, got: %s", result.Content[0].Text)
	}
	if called {
		t.Fatalf("expected NO HTTP call when writes disabled")
	}
}

func TestWriteGate_EnabledForwardsPayload(t *testing.T) {
	var seen map[string]any
	var seenPath string

	_, icc := newTestICCServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seen = readBody(t, r)
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"ok":     true,
			"result": map[string]any{"id": "prj_1", "name": "X"},
		})
	})

	handler := withWriteGate(true, makeCreateHandler(icc, "project_create", "/api/projects"))
	result, err := handler(context.Background(), map[string]any{
		"payload": map[string]any{"name": "X", "slug": "x"},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got: %s", result.Content[0].Text)
	}
	if seenPath != "/api/projects" {
		t.Fatalf("expected /api/projects, got %s", seenPath)
	}
	if seen["name"] != "X" || seen["slug"] != "x" {
		t.Fatalf("expected payload forwarded, got %+v", seen)
	}
}

// --- URL-templated id path (artifact demote) ----------------------------

func TestArtifactDemote_TemplatesIDIntoURL(t *testing.T) {
	var seenPath string
	var seenBody map[string]any

	_, icc := newTestICCServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenBody = readBody(t, r)
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"ok":     true,
			"result": map[string]any{"artifact": map[string]any{"id": "art_1"}},
		})
	})

	handler := withWriteGate(true, makeIDInURLHandler(icc, "artifact_demote", "artifact_id",
		func(id string) string { return "/api/artifacts/" + id + "/demote" }))
	result, err := handler(context.Background(), map[string]any{
		"artifact_id": "art_42",
		"payload":     map[string]any{"reason": "test", "keep_code_ref": true},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got: %s", result.Content[0].Text)
	}
	if seenPath != "/api/artifacts/art_42/demote" {
		t.Fatalf("expected url-templated path, got %s", seenPath)
	}
	if seenBody["reason"] != "test" || seenBody["keep_code_ref"] != true {
		t.Fatalf("expected payload forwarded verbatim, got %+v", seenBody)
	}
}

func TestArtifactDemote_EmptyIDRefusesBeforeNetwork(t *testing.T) {
	called := false
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		writeJSON(t, w, http.StatusOK, map[string]any{})
	})

	handler := withWriteGate(true, makeIDInURLHandler(icc, "artifact_demote", "artifact_id",
		func(id string) string { return "/api/artifacts/" + id + "/demote" }))
	result, _ := handler(context.Background(), map[string]any{
		"artifact_id": "",
		"payload":     map[string]any{"reason": "x"},
	})
	if !result.IsError {
		t.Fatalf("expected client-side error for empty artifact_id")
	}
	if called {
		t.Fatalf("expected NO HTTP call for empty artifact_id")
	}
}

// --- update tools fold id into payload ---------------------------------

func TestUpdateHandler_FoldsIDIntoPayload(t *testing.T) {
	var seen map[string]any
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = readBody(t, r)
		writeJSON(t, w, http.StatusOK, map[string]any{
			"ok":     true,
			"result": map[string]any{"id": "art_1"},
		})
	})

	handler := withWriteGate(true, makeIDPayloadHandler(icc, "artifact_update",
		"/api/artifacts/update", "id"))
	// Caller provides id at top level only; handler should fold it
	// into the payload under "id" so the backend's pop("id") works.
	result, err := handler(context.Background(), map[string]any{
		"id":      "art_1",
		"payload": map[string]any{"title": "new title"},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got: %s", result.Content[0].Text)
	}
	if seen["id"] != "art_1" {
		t.Fatalf("expected id folded into payload, got %+v", seen)
	}
	if seen["title"] != "new title" {
		t.Fatalf("expected title forwarded, got %+v", seen)
	}
}

func TestUpdateHandler_PayloadIDWinsOverTopLevel(t *testing.T) {
	var seen map[string]any
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = readBody(t, r)
		writeJSON(t, w, http.StatusOK, map[string]any{
			"ok":     true,
			"result": map[string]any{"id": "art_real"},
		})
	})

	handler := withWriteGate(true, makeIDPayloadHandler(icc, "artifact_update",
		"/api/artifacts/update", "id"))
	// Top-level id is required by the schema; payload.id wins for
	// the actual server call (you-asked-for-it-you-get-it).
	result, err := handler(context.Background(), map[string]any{
		"id":      "art_top",
		"payload": map[string]any{"id": "art_payload", "title": "x"},
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got: %s", result.Content[0].Text)
	}
	if seen["id"] != "art_payload" {
		t.Fatalf("expected payload.id to win, got %+v", seen)
	}
}

// --- reads --------------------------------------------------------------

func TestProjectList_PassThroughOverview(t *testing.T) {
	var seenPath string
	overview := map[string]any{
		"projects": []map[string]any{
			{"id": "prj_1", "name": "P1", "open_action_items": 3},
		},
	}
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		writeJSON(t, w, http.StatusOK, overview)
	})

	handler := makeProjectListHandler(icc)
	result, err := handler(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got: %s", result.Content[0].Text)
	}
	if seenPath != "/api/projects/overview" {
		t.Fatalf("expected /api/projects/overview, got %s", seenPath)
	}
	// Result is the raw payload (bare {projects: [...]}).
	var got map[string]any
	decodeResult(t, result.Content[0].Text, &got)
	projects, ok := got["projects"].([]any)
	if !ok || len(projects) != 1 {
		t.Fatalf("expected projects list pass-through, got %+v", got)
	}
}

func TestListReader_ForwardsQueryFilters(t *testing.T) {
	var seenQuery string
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		writeJSON(t, w, http.StatusOK, map[string]any{"action_items": []any{}})
	})

	handler := makeListReader(icc, "action_item_list", "/api/action-items",
		"project_id", "vendor_id", "status", "owner", "include_done")
	_, err := handler(context.Background(), map[string]any{
		"project_id":   "prj_1",
		"status":       "open",
		"include_done": false,
		"vendor_id":    "", // empty → should be skipped
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !strings.Contains(seenQuery, "project_id=prj_1") {
		t.Fatalf("missing project_id, got %q", seenQuery)
	}
	if !strings.Contains(seenQuery, "status=open") {
		t.Fatalf("missing status, got %q", seenQuery)
	}
	if !strings.Contains(seenQuery, "include_done=false") {
		t.Fatalf("missing include_done=false, got %q", seenQuery)
	}
	if strings.Contains(seenQuery, "vendor_id=") {
		t.Fatalf("expected empty vendor_id to be skipped, got %q", seenQuery)
	}
}

func TestProjectKanban_TemplatesIDIntoURL(t *testing.T) {
	var seenPath string
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		writeJSON(t, w, http.StatusOK, map[string]any{"columns": []any{}})
	})

	handler := makeProjectScopedReader(icc, "project_kanban", "kanban", "include_done")
	result, err := handler(context.Background(), map[string]any{
		"project_id": "prj_99",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got: %s", result.Content[0].Text)
	}
	if seenPath != "/api/projects/prj_99/kanban" {
		t.Fatalf("expected /api/projects/prj_99/kanban, got %s", seenPath)
	}
}

func TestProjectKanban_MissingIDRefuses(t *testing.T) {
	called := false
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		writeJSON(t, w, http.StatusOK, map[string]any{})
	})

	handler := makeProjectScopedReader(icc, "project_kanban", "kanban")
	result, _ := handler(context.Background(), map[string]any{})
	if !result.IsError {
		t.Fatalf("expected client-side error for missing project_id")
	}
	if called {
		t.Fatalf("expected NO HTTP call for missing project_id")
	}
}

func TestSearch_RequiresQ(t *testing.T) {
	called := false
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		writeJSON(t, w, http.StatusOK, map[string]any{})
	})

	handler := makeSearchHandler(icc)
	result, _ := handler(context.Background(), map[string]any{})
	if !result.IsError {
		t.Fatalf("expected client-side error for missing q")
	}
	if called {
		t.Fatalf("expected NO HTTP call for missing q")
	}
}

func TestSearch_ForwardsAllFilters(t *testing.T) {
	var seenQuery string
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		writeJSON(t, w, http.StatusOK, map[string]any{"results": []any{}})
	})

	handler := makeSearchHandler(icc)
	_, err := handler(context.Background(), map[string]any{
		"q":           "cohere volume",
		"mode":        "hybrid",
		"project_id":  "prj_1",
		"include_phi": "1",
		"reason":      "weekly audit",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	for _, want := range []string{"q=cohere", "mode=hybrid", "project_id=prj_1",
		"include_phi=1", "reason=weekly"} {
		if !strings.Contains(seenQuery, want) {
			t.Fatalf("missing %q in query: %q", want, seenQuery)
		}
	}
}

// --- icc_quick_capture --------------------------------------------------

func TestQuickCapture_BuildsArtifactPayload(t *testing.T) {
	var seen map[string]any
	var seenPath string

	_, icc := newTestICCServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seen = readBody(t, r)
		writeJSON(t, w, http.StatusCreated, map[string]any{
			"ok":     true,
			"result": map[string]any{"id": "art_1", "title": "X"},
		})
	})

	handler := withWriteGate(true, makeQuickCaptureHandler(icc))
	result, err := handler(context.Background(), map[string]any{
		"project_id": "prj_abc",
		"title":      "Cohere volume snapshot",
		"summary":    "weekly audit",
		"code_path":  "services/cohere-volume/audit.py",
		"session_id": "sess_42",
	})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got: %s", result.Content[0].Text)
	}
	if seenPath != "/api/artifacts" {
		t.Fatalf("expected /api/artifacts, got %s", seenPath)
	}
	for k, want := range map[string]string{
		"project_id": "prj_abc",
		"title":      "Cohere volume snapshot",
		"summary":    "weekly audit",
		"code_path":  "services/cohere-volume/audit.py",
		"session_id": "sess_42",
	} {
		if seen[k] != want {
			t.Fatalf("expected %s=%q, got %+v", k, want, seen[k])
		}
	}
	// Defaults filled in.
	if seen["classification"] != "possible_phi" {
		t.Fatalf("expected default classification=possible_phi, got %+v", seen["classification"])
	}
	if seen["kind"] != "note" {
		t.Fatalf("expected default kind=note, got %+v", seen["kind"])
	}
}

func TestQuickCapture_MissingRequiredFields(t *testing.T) {
	called := false
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		writeJSON(t, w, http.StatusOK, map[string]any{})
	})

	handler := withWriteGate(true, makeQuickCaptureHandler(icc))
	result, _ := handler(context.Background(), map[string]any{
		"project_id": "prj_abc",
		// title missing
	})
	if !result.IsError {
		t.Fatalf("expected client-side error for missing title")
	}
	if called {
		t.Fatalf("expected NO HTTP call for missing title")
	}
}

func TestQuickCapture_DisabledRespectsGate(t *testing.T) {
	called := false
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		writeJSON(t, w, http.StatusOK, map[string]any{})
	})

	handler := withWriteGate(false, makeQuickCaptureHandler(icc))
	result, _ := handler(context.Background(), map[string]any{
		"project_id": "prj_abc",
		"title":      "X",
	})
	if !result.IsError {
		t.Fatalf("expected writes_disabled error")
	}
	if !strings.Contains(result.Content[0].Text, "writes_disabled") {
		t.Fatalf("expected writes_disabled in error, got: %s", result.Content[0].Text)
	}
	if called {
		t.Fatalf("expected NO HTTP call when writes disabled")
	}
}

// --- missing baseURL fails loud at call time ----------------------------

func TestRead_MissingBaseURLFailsLoud(t *testing.T) {
	icc := iccclient.NewForTest("", &http.Client{})
	handler := makeProjectListHandler(icc)
	result, _ := handler(context.Background(), map[string]any{})
	if !result.IsError {
		t.Fatalf("expected ICC_BASE_URL error")
	}
	if !strings.Contains(result.Content[0].Text, "ICC_BASE_URL") {
		t.Fatalf("expected ICC_BASE_URL in error, got: %s", result.Content[0].Text)
	}
}

func TestWrite_MissingBaseURLFailsLoud(t *testing.T) {
	icc := iccclient.NewForTest("", &http.Client{})
	handler := withWriteGate(true, makeCreateHandler(icc, "project_create", "/api/projects"))
	result, _ := handler(context.Background(), map[string]any{
		"payload": map[string]any{"name": "X"},
	})
	if !result.IsError {
		t.Fatalf("expected ICC_BASE_URL error")
	}
	if !strings.Contains(result.Content[0].Text, "ICC_BASE_URL") {
		t.Fatalf("expected ICC_BASE_URL in error, got: %s", result.Content[0].Text)
	}
}

// --- trusted-context headers ------------------------------------------

func TestTrustedContextHeadersAreSent(t *testing.T) {
	var seenHeaders http.Header
	_, icc := newTestICCServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		writeJSON(t, w, http.StatusOK, map[string]any{"projects": []any{}})
	})

	handler := makeProjectListHandler(icc)
	_, err := handler(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if got := seenHeaders.Get("X-Requested-With"); got != "integration-command-center" {
		t.Fatalf("expected XRW header, got %q", got)
	}
	if got := seenHeaders.Get("Origin"); got == "" {
		t.Fatalf("expected non-empty Origin header")
	}
}
