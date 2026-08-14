package mirror

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/vendorsessions"
)

type vendorPush struct {
	Host     string `json:"host"`
	Sessions []struct {
		Vendor     string                 `json:"vendor"`
		ID         string                 `json:"id"`
		ModifiedAt string                 `json:"modified_at"`
		SizeBytes  int64                  `json:"size_bytes"`
		Entries    []vendorsessions.Entry `json:"entries"`
	} `json:"sessions"`
}

// vendorFixture writes one claude transcript and returns a store rooted at it.
func vendorFixture(t *testing.T, mtime time.Time) (vendorsessions.Store, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "-Users-u-ws-repo", "aaaa1111-0000-0000-0000-000000000001.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"user","cwd":"/Users/u/ws/repo","sessionId":"aaaa1111-0000-0000-0000-000000000001","timestamp":"2026-07-26T10:00:00.000Z","message":{"role":"user","content":"chase the marmalade wedge"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return vendorsessions.Store{ClaudeRoot: root, CodexRoot: filepath.Join(root, "no-codex")}, path
}

func TestVendorSyncPushesEntriesOnceThenMetadataOnly(t *testing.T) {
	var pushes []vendorPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/vendor-sessions/mirror" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var p vendorPush
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("bad payload: %v", err)
		}
		pushes = append(pushes, p)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, _ := vendorFixture(t, time.Now().Add(-time.Hour))
	s := New(Config{URL: srv.URL}, &fakeReader{}, srv.Client(), nil)
	s.SetVendorStore(&store, "test-mac")

	s.VendorSyncOnce(context.Background())
	s.VendorSyncOnce(context.Background())

	if len(pushes) != 2 {
		t.Fatalf("pushes = %d, want 2", len(pushes))
	}
	if pushes[0].Host != "test-mac" {
		t.Fatalf("host = %q", pushes[0].Host)
	}
	if len(pushes[0].Sessions) != 1 || len(pushes[0].Sessions[0].Entries) == 0 {
		t.Fatalf("first push should carry entries: %+v", pushes[0].Sessions)
	}
	// Unchanged transcript: second push is metadata-only (entries omitted).
	if len(pushes[1].Sessions) != 1 || pushes[1].Sessions[0].Entries != nil {
		t.Fatalf("second push should omit entries: %+v", pushes[1].Sessions)
	}
}

func TestVendorSyncResendsEntriesAfterFailedPush(t *testing.T) {
	fail := true
	var entryPushes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p vendorPush
		_ = json.Unmarshal(body, &p)
		if len(p.Sessions) == 1 && len(p.Sessions[0].Entries) > 0 {
			entryPushes++
		}
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, _ := vendorFixture(t, time.Now().Add(-time.Hour))
	s := New(Config{URL: srv.URL}, &fakeReader{}, srv.Client(), nil)
	s.SetVendorStore(&store, "test-mac")

	// Failed push must not commit cursors — the tail ships again next cycle.
	s.VendorSyncOnce(context.Background())
	fail = false
	s.VendorSyncOnce(context.Background())

	if entryPushes != 2 {
		t.Fatalf("entry-bearing pushes = %d, want 2 (resend after failure)", entryPushes)
	}
}

func TestVendorSyncChangedFileReshipsEntries(t *testing.T) {
	var pushes []vendorPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p vendorPush
		_ = json.Unmarshal(body, &p)
		pushes = append(pushes, p)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, path := vendorFixture(t, time.Now().Add(-time.Hour))
	s := New(Config{URL: srv.URL}, &fakeReader{}, srv.Client(), nil)
	s.SetVendorStore(&store, "test-mac")

	s.VendorSyncOnce(context.Background())

	// Append a line and bump mtime — the next sync must re-extract.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"wedge found"}]}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	s.VendorSyncOnce(context.Background())

	if len(pushes) != 2 {
		t.Fatalf("pushes = %d, want 2", len(pushes))
	}
	if len(pushes[1].Sessions) != 1 || len(pushes[1].Sessions[0].Entries) != 2 {
		t.Fatalf("changed transcript should reship entries: %+v", pushes[1].Sessions)
	}
}

func TestVendorSyncBudgetDefersAndCatchesUp(t *testing.T) {
	// Two transcripts in the same root; a budget that fits only one tail
	// forces the second to defer to a follow-up cycle. Regression for the
	// first-rollout 413: a full 16-session ship exceeded the ingress's 1m
	// default body cap.
	// Each fixture tail costs ~90-100 approximated bytes (extracted text +
	// role + timestamp + overhead); 100 fits exactly one.
	oldBudget := vendorPushBudgetBytes
	vendorPushBudgetBytes = 100
	defer func() { vendorPushBudgetBytes = oldBudget }()

	var pushes []vendorPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p vendorPush
		_ = json.Unmarshal(body, &p)
		pushes = append(pushes, p)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, path := vendorFixture(t, time.Now().Add(-time.Hour))
	second := filepath.Join(filepath.Dir(path), "bbbb2222-0000-0000-0000-000000000002.jsonl")
	if err := os.WriteFile(second, []byte(`{"type":"user","cwd":"/Users/u/ws/repo","sessionId":"bbbb2222-0000-0000-0000-000000000002","timestamp":"2026-07-26T11:00:00.000Z","message":{"role":"user","content":"second transcript"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(second, time.Now().Add(-30*time.Minute), time.Now().Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}

	s := New(Config{URL: srv.URL}, &fakeReader{}, srv.Client(), nil)
	s.SetVendorStore(&store, "test-mac")

	s.VendorSyncOnce(context.Background())
	if !s.lastVendorSync.IsZero() {
		t.Fatal("a deferred tail should schedule an immediate follow-up")
	}
	s.VendorSyncOnce(context.Background())
	// Nothing left to defer: the follow-up must not loop hot.
	if s.lastVendorSync.IsZero() {
		t.Fatal("catch-up cycle should settle back to the normal interval")
	}

	if len(pushes) != 2 {
		t.Fatalf("pushes = %d, want 2", len(pushes))
	}
	withEntries := func(p vendorPush) int {
		n := 0
		for _, sess := range p.Sessions {
			if len(sess.Entries) > 0 {
				n++
			}
		}
		return n
	}
	if got := withEntries(pushes[0]); got != 1 {
		t.Fatalf("push 1 entry-bearing sessions = %d, want 1 (budget)", got)
	}
	if got := withEntries(pushes[1]); got != 1 {
		t.Fatalf("push 2 entry-bearing sessions = %d, want 1 (deferred catch-up)", got)
	}
	if len(pushes[0].Sessions) != 2 || len(pushes[1].Sessions) != 2 {
		t.Fatalf("metadata must still cover both sessions in both pushes: %d/%d",
			len(pushes[0].Sessions), len(pushes[1].Sessions))
	}
}

func TestVendorSyncEpochChangeResetsCursors(t *testing.T) {
	epoch := "epoch-a"
	var pushes []vendorPush
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p vendorPush
		_ = json.Unmarshal(body, &p)
		pushes = append(pushes, p)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"epoch":"` + epoch + `"}`))
	}))
	defer srv.Close()

	store, _ := vendorFixture(t, time.Now().Add(-time.Hour))
	s := New(Config{URL: srv.URL}, &fakeReader{}, srv.Client(), nil)
	s.SetVendorStore(&store, "test-mac")

	// Cycle 1: full ship, adopts epoch-a.
	s.VendorSyncOnce(context.Background())
	// Receiver "restarts" (fresh empty store, new epoch): the delta push
	// must reset cursors and schedule an immediate re-sync...
	epoch = "epoch-b"
	s.VendorSyncOnce(context.Background())
	if !s.lastVendorSync.IsZero() {
		t.Fatal("epoch change on a delta push should force an immediate re-sync")
	}
	// ...so the next cycle re-ships full entries.
	s.VendorSyncOnce(context.Background())

	if len(pushes) != 3 {
		t.Fatalf("pushes = %d, want 3", len(pushes))
	}
	if len(pushes[1].Sessions[0].Entries) != 0 {
		t.Fatalf("push 2 should have been metadata-only: %+v", pushes[1].Sessions)
	}
	if len(pushes[2].Sessions[0].Entries) == 0 {
		t.Fatal("push 3 should re-ship entries after the receiver restart")
	}
}

func TestMaybeVendorSyncHonorsIntervalAndDisable(t *testing.T) {
	var pushes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/vendor-sessions/mirror" {
			pushes++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store, _ := vendorFixture(t, time.Now().Add(-time.Hour))
	s := New(Config{URL: srv.URL}, &fakeReader{}, srv.Client(), nil)
	s.SetVendorStore(&store, "test-mac")

	s.maybeVendorSync(context.Background())
	// Immediately due again? No — the interval floor (15s) gates it.
	s.maybeVendorSync(context.Background())
	if pushes != 1 {
		t.Fatalf("pushes = %d, want 1 (interval gate)", pushes)
	}

	// Nil store (env-disabled path) never syncs.
	s.SetVendorStore(nil, "")
	s.lastVendorSync = time.Time{}
	s.maybeVendorSync(context.Background())
	if pushes != 1 {
		t.Fatalf("pushes = %d, want 1 (disabled)", pushes)
	}
}

func TestConfigFromEnv_VendorSettings(t *testing.T) {
	t.Setenv("LOOM_HUD_MIRROR_URL", "https://hud.example")
	t.Setenv("LOOM_HUD_MIRROR_VENDOR_SESSIONS", "0")
	t.Setenv("LOOM_HUD_MIRROR_VENDOR_INTERVAL", "2m")

	cfg := NewConfigFromEnv()
	if !cfg.VendorSessionsDisabled {
		t.Fatal("VendorSessionsDisabled = false, want true")
	}
	if cfg.VendorInterval != 2*time.Minute {
		t.Fatalf("VendorInterval = %v, want 2m", cfg.VendorInterval)
	}

	// Disabled config must produce a service with no vendor store.
	s := New(cfg, &fakeReader{}, nil, nil)
	if s.vendorStore != nil {
		t.Fatal("vendor store should be nil when disabled")
	}
}
