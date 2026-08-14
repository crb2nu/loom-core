package vendorsessions

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

type fakeOps struct {
	listParams   *bridge.VendorSessionListParams
	listResult   []bridge.VendorSessionInfo
	listErr      error
	searchParams *bridge.VendorSessionSearchParams
	searchResult []bridge.VendorSessionMatch
	searchErr    error
	tailVendor   string
	tailID       string
	tailLines    int
	tailResult   *bridge.VendorSessionTailResult
	tailErr      error
}

func (f *fakeOps) VendorSessionTail(vendor, id string, lines int) (*bridge.VendorSessionTailResult, error) {
	f.tailVendor, f.tailID, f.tailLines = vendor, id, lines
	return f.tailResult, f.tailErr
}

func (f *fakeOps) VendorSessionList(p bridge.VendorSessionListParams) ([]bridge.VendorSessionInfo, error) {
	f.listParams = &p
	return f.listResult, f.listErr
}

func (f *fakeOps) VendorSessionSearch(p bridge.VendorSessionSearchParams) ([]bridge.VendorSessionMatch, error) {
	f.searchParams = &p
	return f.searchResult, f.searchErr
}

type fakeDeps struct {
	ops Ops
}

func (f *fakeDeps) WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeDeps) WriteError(w http.ResponseWriter, status int, msg string, _ error) {
	http.Error(w, msg, status)
}

func (f *fakeDeps) VendorSessions() Ops { return f.ops }

func doGet(t *testing.T, d *Domain, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHandleList_PassesFiltersAndReturnsSessions(t *testing.T) {
	t.Parallel()
	ops := &fakeOps{listResult: []bridge.VendorSessionInfo{
		{Vendor: "claude", ID: "abc", Path: "/p/abc.jsonl", CWD: "/w/services/loom-core", ModifiedAt: "2026-07-26T10:00:00Z", SizeBytes: 42},
		{Vendor: "codex", ID: "def", Path: "/p/def.jsonl", ModifiedAt: "2026-07-26T09:00:00Z"},
	}}
	d := New(&fakeDeps{ops: ops})

	rec := doGet(t, d, "/api/vendor-sessions?vendor=claude&cwd_contains=loom-core&since_hours=24&limit=10")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	want := bridge.VendorSessionListParams{Vendor: "claude", CwdContains: "loom-core", SinceHours: 24, Limit: 10}
	if ops.listParams == nil || *ops.listParams != want {
		t.Errorf("list params = %+v, want %+v", ops.listParams, want)
	}
	var body struct {
		Sessions []bridge.VendorSessionInfo `json:"sessions"`
		Count    int                        `json:"count"`
		Degraded bool                       `json:"degraded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if body.Count != 2 || len(body.Sessions) != 2 {
		t.Errorf("count = %d, sessions = %d, want 2/2", body.Count, len(body.Sessions))
	}
	if body.Degraded {
		t.Error("degraded should be false when the bridge answered")
	}
	if body.Sessions[0].Vendor != "claude" || body.Sessions[0].ID != "abc" {
		t.Errorf("first session = %+v, want claude/abc", body.Sessions[0])
	}
}

func TestHandleList_NilSessionsEncodeAsEmptyArray(t *testing.T) {
	t.Parallel()
	d := New(&fakeDeps{ops: &fakeOps{listResult: nil}})

	rec := doGet(t, d, "/api/vendor-sessions")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(body["sessions"]) != "[]" {
		t.Errorf("sessions = %s, want []", body["sessions"])
	}
}

func TestHandleList_MalformedIntsDegradeToDefaults(t *testing.T) {
	t.Parallel()
	ops := &fakeOps{}
	d := New(&fakeDeps{ops: ops})

	rec := doGet(t, d, "/api/vendor-sessions?since_hours=bogus&limit=-5")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ops.listParams.SinceHours != 0 || ops.listParams.Limit != 0 {
		t.Errorf("params = %+v, want zero SinceHours/Limit", ops.listParams)
	}
}

func TestHandleList_InvalidVendorIs400(t *testing.T) {
	t.Parallel()
	ops := &fakeOps{}
	d := New(&fakeDeps{ops: ops})

	rec := doGet(t, d, "/api/vendor-sessions?vendor=gemini")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ops.listParams != nil {
		t.Error("bridge must not be called on an invalid vendor")
	}
}

func TestHandleList_NilOpsIsDegraded200(t *testing.T) {
	t.Parallel()
	d := New(&fakeDeps{ops: nil})

	rec := doGet(t, d, "/api/vendor-sessions")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on nil ops; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Count    int  `json:"count"`
		Degraded bool `json:"degraded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Degraded || body.Count != 0 {
		t.Errorf("body = %+v, want degraded placeholder", body)
	}
}

func TestHandleList_BridgeErrorIs502(t *testing.T) {
	t.Parallel()
	d := New(&fakeDeps{ops: &fakeOps{listErr: errors.New("socket closed")}})

	rec := doGet(t, d, "/api/vendor-sessions")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestHandleSearch_RequiresQuery(t *testing.T) {
	t.Parallel()
	ops := &fakeOps{}
	d := New(&fakeDeps{ops: ops})

	rec := doGet(t, d, "/api/vendor-sessions/search")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ops.searchParams != nil {
		t.Error("bridge must not be called without a query")
	}
}

func TestHandleSearch_PassesParamsAndReturnsMatches(t *testing.T) {
	t.Parallel()
	ops := &fakeOps{searchResult: []bridge.VendorSessionMatch{
		{Vendor: "codex", SessionID: "def", Line: 12, Snippet: "…decided to use pipeline()…"},
	}}
	d := New(&fakeDeps{ops: ops})

	rec := doGet(t, d, "/api/vendor-sessions/search?query=pipeline&vendor=codex&max_results=5&max_per_session=2")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ops.searchParams == nil {
		t.Fatal("search params not captured")
	}
	if ops.searchParams.Query != "pipeline" || ops.searchParams.Vendor != "codex" ||
		ops.searchParams.MaxResults != 5 || ops.searchParams.MaxPerSession != 2 {
		t.Errorf("search params = %+v", ops.searchParams)
	}
	var body struct {
		Query    string                      `json:"query"`
		Matches  []bridge.VendorSessionMatch `json:"matches"`
		Count    int                         `json:"count"`
		Degraded bool                        `json:"degraded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Query != "pipeline" || body.Count != 1 || len(body.Matches) != 1 {
		t.Errorf("body = %+v, want 1 match for %q", body, "pipeline")
	}
	if body.Matches[0].SessionID != "def" || body.Matches[0].Line != 12 {
		t.Errorf("match = %+v", body.Matches[0])
	}
}

func TestHandleSearch_NilOpsIsDegraded200(t *testing.T) {
	t.Parallel()
	d := New(&fakeDeps{ops: nil})

	rec := doGet(t, d, "/api/vendor-sessions/search?query=x")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on nil ops", rec.Code)
	}
	var body struct {
		Degraded bool `json:"degraded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Degraded {
		t.Error("want degraded placeholder on nil ops")
	}
}

func TestHandleSearch_BridgeErrorIs502(t *testing.T) {
	t.Parallel()
	d := New(&fakeDeps{ops: &fakeOps{searchErr: errors.New("timeout")}})

	rec := doGet(t, d, "/api/vendor-sessions/search?query=x")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

func TestHandleTailValidationAndResponse(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/api/vendor-sessions/tail", "/api/vendor-sessions/tail?vendor=claude"} {
		if rec := doGet(t, New(&fakeDeps{}), path); rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", path, rec.Code)
		}
	}
	ops := &fakeOps{tailResult: &bridge.VendorSessionTailResult{
		Lines:      []bridge.VendorSessionTailLine{{Role: "user", Timestamp: "2026-08-08T00:00:00Z", Text: "hello"}},
		TotalLines: 8, Truncated: true,
	}}
	rec := doGet(t, New(&fakeDeps{ops: ops}), "/api/vendor-sessions/tail?vendor=claude&id=abc&lines=50")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	if ops.tailVendor != "claude" || ops.tailID != "abc" || ops.tailLines != 50 {
		t.Fatalf("tail args = %q %q %d", ops.tailVendor, ops.tailID, ops.tailLines)
	}
	var body bridge.VendorSessionTailResult
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Lines) != 1 || body.TotalLines != 8 || !body.Truncated || body.Degraded {
		t.Fatalf("body = %+v", body)
	}
}

func TestHandleTailInvalidLinesIs400(t *testing.T) {
	t.Parallel()
	ops := &fakeOps{}
	rec := doGet(t, New(&fakeDeps{ops: ops}), "/api/vendor-sessions/tail?vendor=claude&id=abc&lines=nope")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if ops.tailVendor != "" {
		t.Fatal("bridge must not be called for invalid lines")
	}
}

func TestHandleTailNilOpsCompleteDegradedPlaceholder(t *testing.T) {
	t.Parallel()
	rec := doGet(t, New(&fakeDeps{}), "/api/vendor-sessions/tail?vendor=codex&id=abc")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"lines", "total_lines", "truncated", "degraded"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing key %q: %s", key, rec.Body.String())
		}
	}
	if string(raw["lines"]) != "[]" || string(raw["degraded"]) != "true" {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestHandleTailFallsBackToMirror(t *testing.T) {
	t.Parallel()
	d := New(&fakeDeps{ops: &fakeOps{tailErr: errors.New("not found")}})
	d.mirror.Ingest("remote-host", []IngestSession{{
		Vendor: "claude", ID: "abc", ModifiedAt: "2026-08-08T00:00:00Z",
		Entries: entriesPtr(
			MirroredEntry{Line: 7, Role: "user", Timestamp: "t1", Text: "older"},
			MirroredEntry{Line: 8, Role: "assistant", Timestamp: "t2", Text: "newer"},
		),
	}})

	rec := doGet(t, d, "/api/vendor-sessions/tail?vendor=claude&id=abc&lines=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var body bridge.VendorSessionTailResult
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Lines) != 1 || body.Lines[0].Text != "newer" || body.TotalLines != 8 || !body.Truncated || body.Degraded {
		t.Fatalf("body = %+v", body)
	}
}

func TestHandleTailNilOpsUsesMirrorWithoutDegrading(t *testing.T) {
	t.Parallel()
	d := New(&fakeDeps{})
	d.mirror.Ingest("remote-host", []IngestSession{{
		Vendor: "codex", ID: "xyz", ModifiedAt: "2026-08-08T00:00:00Z",
		Entries: entriesPtr(MirroredEntry{Line: 1, Role: "user", Text: "hello"}),
	}})
	rec := doGet(t, d, "/api/vendor-sessions/tail?vendor=codex&id=xyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var body bridge.VendorSessionTailResult
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Lines) != 1 || body.Degraded {
		t.Fatalf("body = %+v", body)
	}
}
