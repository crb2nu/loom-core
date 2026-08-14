package vendorsessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

func entriesPtr(e ...MirroredEntry) *[]MirroredEntry { return &e }

func ingestSession(id, modifiedAt string, entries *[]MirroredEntry) IngestSession {
	return IngestSession{
		Vendor:     "claude",
		ID:         id,
		Path:       "/remote/" + id + ".jsonl",
		CWD:        "/Users/u/ws/services/loom-core",
		Title:      "fix the marmalade flake",
		Kind:       "sidechain",
		ModifiedAt: modifiedAt,
		SizeBytes:  1024,
		Entries:    entries,
	}
}

func doPost(t *testing.T, d *Domain, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	d.RegisterRoutes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestMirrorIngestThenListMergesWithHostTag(t *testing.T) {
	t.Parallel()
	ops := &fakeOps{listResult: []bridge.VendorSessionInfo{
		{Vendor: "claude", ID: "local-1", Path: "/local/1.jsonl", ModifiedAt: "2026-07-26T12:00:00Z"},
	}}
	d := New(&fakeDeps{ops: ops})

	rec := doPost(t, d, "/api/vendor-sessions/mirror", map[string]any{
		"host": "codys-mac",
		"sessions": []IngestSession{
			ingestSession("remote-1", "2026-07-26T13:00:00Z",
				entriesPtr(MirroredEntry{Line: 4, Role: "user", Text: "remote marmalade line"})),
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest status = %d: %s", rec.Code, rec.Body.String())
	}
	var ack struct {
		Epoch string `json:"epoch"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil || ack.Epoch == "" {
		t.Fatalf("ingest ack must carry a non-empty epoch: %s (err=%v)", rec.Body.String(), err)
	}

	list := doGet(t, d, "/api/vendor-sessions")
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d", list.Code)
	}
	var resp struct {
		Sessions []SessionOut `json:"sessions"`
		Count    int          `json:"count"`
		Degraded bool         `json:"degraded"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 2 || resp.Degraded {
		t.Fatalf("count=%d degraded=%v, want 2/false", resp.Count, resp.Degraded)
	}
	// Newest-modified first: the federated row (13:00) precedes local (12:00).
	if resp.Sessions[0].ID != "remote-1" || resp.Sessions[0].Host != "codys-mac" {
		t.Fatalf("first row = %+v, want federated remote-1 tagged codys-mac", resp.Sessions[0])
	}
	// Title/Kind must survive the ingest → store → list round trip: the UI's
	// title-first rows and background-run collapse depend on these fields
	// flowing from federated senders, not just the local bridge.
	if resp.Sessions[0].Title != "fix the marmalade flake" || resp.Sessions[0].Kind != "sidechain" {
		t.Fatalf("federated row dropped title/kind: %+v", resp.Sessions[0])
	}
	if resp.Sessions[1].ID != "local-1" || resp.Sessions[1].Host != "" {
		t.Fatalf("second row = %+v, want untagged local-1", resp.Sessions[1])
	}
}

func TestMirrorLocalRowWinsOnCollision(t *testing.T) {
	t.Parallel()
	ops := &fakeOps{listResult: []bridge.VendorSessionInfo{
		{Vendor: "claude", ID: "dup", Path: "/local/dup.jsonl", ModifiedAt: "2026-07-26T12:00:00Z"},
	}}
	d := New(&fakeDeps{ops: ops})
	d.mirror.Ingest("codys-mac", []IngestSession{ingestSession("dup", "2026-07-26T13:00:00Z", nil)})

	list := doGet(t, d, "/api/vendor-sessions")
	var resp struct {
		Sessions []SessionOut `json:"sessions"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Sessions) != 1 || resp.Sessions[0].Host != "" || resp.Sessions[0].Path != "/local/dup.jsonl" {
		t.Fatalf("collision should keep the local row: %+v", resp.Sessions)
	}
}

func TestMirrorSearchScansEntriesWithCaps(t *testing.T) {
	t.Parallel()
	d := New(&fakeDeps{ops: nil})
	d.mirror.Ingest("codys-mac", []IngestSession{
		ingestSession("s1", "2026-07-26T13:00:00Z", entriesPtr(
			MirroredEntry{Line: 1, Text: "marmalade one"},
			MirroredEntry{Line: 2, Text: "marmalade two"},
			MirroredEntry{Line: 3, Text: "marmalade three"},
			MirroredEntry{Line: 4, Text: "unrelated"},
		)),
	})

	rec := doGet(t, d, "/api/vendor-sessions/search?query=MARMALADE&max_per_session=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Matches  []MatchOut `json:"matches"`
		Degraded bool       `json:"degraded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// Case-insensitive, capped at 2 per session, host-tagged, and NOT
	// degraded even with no local bridge — the mirror is a live source.
	if len(resp.Matches) != 2 || resp.Degraded {
		t.Fatalf("matches=%d degraded=%v, want 2/false", len(resp.Matches), resp.Degraded)
	}
	if resp.Matches[0].Host != "codys-mac" || resp.Matches[0].SessionID != "s1" || resp.Matches[0].Line != 1 {
		t.Fatalf("match = %+v", resp.Matches[0])
	}
}

func TestMirrorEntriesCarryForwardWhenOmitted(t *testing.T) {
	t.Parallel()
	d := New(&fakeDeps{ops: nil})
	d.mirror.Ingest("codys-mac", []IngestSession{
		ingestSession("s1", "2026-07-26T13:00:00Z", entriesPtr(MirroredEntry{Line: 1, Text: "marmalade kept"})),
	})
	// Metadata-only re-push (entries nil) must keep the searchable tail.
	d.mirror.Ingest("codys-mac", []IngestSession{ingestSession("s1", "2026-07-26T13:00:00Z", nil)})

	if got := d.mirror.Search(bridge.VendorSessionSearchParams{Query: "marmalade"}); len(got) != 1 {
		t.Fatalf("matches = %d, want 1 (entries carried forward)", len(got))
	}

	// A push WITHOUT the session drops it entirely (full-snapshot contract).
	d.mirror.Ingest("codys-mac", []IngestSession{ingestSession("s2", "2026-07-26T14:00:00Z", nil)})
	if got := d.mirror.Search(bridge.VendorSessionSearchParams{Query: "marmalade"}); len(got) != 0 {
		t.Fatalf("matches = %d, want 0 (s1 dropped from snapshot)", len(got))
	}
}

func TestMirrorHostTTLExpires(t *testing.T) {
	t.Parallel()
	d := New(&fakeDeps{ops: nil})
	now := time.Now()
	d.mirror.now = func() time.Time { return now }
	d.mirror.Ingest("codys-mac", []IngestSession{ingestSession("s1", "2026-07-26T13:00:00Z", nil)})

	if !d.mirror.HasLiveHosts() {
		t.Fatal("host should be live right after ingest")
	}
	now = now.Add(mirrorHostTTL + time.Second)
	if d.mirror.HasLiveHosts() {
		t.Fatal("host should expire after the TTL")
	}
	if got := d.mirror.Sessions(bridge.VendorSessionListParams{}); len(got) != 0 {
		t.Fatalf("sessions = %d, want 0 after TTL", len(got))
	}

	// And the list handler reports degraded again once nothing can answer.
	rec := doGet(t, d, "/api/vendor-sessions")
	var resp struct {
		Degraded bool `json:"degraded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Degraded {
		t.Fatal("degraded = false, want true (no bridge, host expired)")
	}
}

func TestMirrorCoversForBrokenBridge(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("bridge down")
	ops := &fakeOps{listErr: errBoom, searchErr: errBoom}
	d := New(&fakeDeps{ops: ops})
	d.mirror.Ingest("codys-mac", []IngestSession{
		ingestSession("s1", "2026-07-26T13:00:00Z", entriesPtr(MirroredEntry{Line: 1, Text: "marmalade"})),
	})

	list := doGet(t, d, "/api/vendor-sessions")
	if list.Code != http.StatusOK {
		t.Fatalf("list with live mirror should not 502, got %d", list.Code)
	}
	search := doGet(t, d, "/api/vendor-sessions/search?query=marmalade")
	if search.Code != http.StatusOK {
		t.Fatalf("search with live mirror should not 502, got %d", search.Code)
	}
}

func TestMirrorIngestValidation(t *testing.T) {
	t.Parallel()
	d := New(&fakeDeps{ops: nil})

	if rec := doPost(t, d, "/api/vendor-sessions/mirror", map[string]any{"sessions": []IngestSession{}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing host: status = %d, want 400", rec.Code)
	}
	bad := ingestSession("s1", "2026-07-26T13:00:00Z", nil)
	bad.Vendor = "gemini"
	if rec := doPost(t, d, "/api/vendor-sessions/mirror", map[string]any{"host": "h", "sessions": []IngestSession{bad}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad vendor: status = %d, want 400", rec.Code)
	}
}

func TestMirrorListFiltersApplyToFederatedRows(t *testing.T) {
	t.Parallel()
	d := New(&fakeDeps{ops: nil})
	other := ingestSession("codex-1", "2026-07-26T13:00:00Z", nil)
	other.Vendor = "codex"
	other.CWD = "/Users/u/ws/labs/game"
	d.mirror.Ingest("codys-mac", []IngestSession{
		ingestSession("claude-1", "2026-07-26T13:00:00Z", nil),
		other,
	})

	var resp struct {
		Sessions []SessionOut `json:"sessions"`
	}
	rec := doGet(t, d, "/api/vendor-sessions?vendor=codex")
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Sessions) != 1 || resp.Sessions[0].Vendor != "codex" {
		t.Fatalf("vendor filter: %+v", resp.Sessions)
	}

	rec = doGet(t, d, "/api/vendor-sessions?cwd_contains=loom-core")
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Sessions) != 1 || resp.Sessions[0].ID != "claude-1" {
		t.Fatalf("cwd filter: %+v", resp.Sessions)
	}
}
