package vendorsessions

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestReader(f *os.File) *bufio.Reader {
	return bufio.NewReaderSize(f, 64*1024)
}

// writeFixture writes a transcript file and stamps its mtime.
func writeFixture(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func fixtureStore(t *testing.T) Store {
	t.Helper()
	root := t.TempDir()
	claudeRoot := filepath.Join(root, "claude-projects")
	codexRoot := filepath.Join(root, "codex-sessions")

	now := time.Now()

	// Claude session in services/loom-core.
	writeFixture(t,
		filepath.Join(claudeRoot, "-Users-u-workspace-services-loom-core", "11111111-aaaa-bbbb-cccc-000000000001.jsonl"),
		strings.Join([]string{
			`{"type":"summary","summary":"HUD degraded mode work"}`,
			`{"type":"user","cwd":"/Users/u/workspace/services/loom-core","sessionId":"11111111-aaaa-bbbb-cccc-000000000001","timestamp":"2026-07-14T10:00:00.000Z","message":{"role":"user","content":"fix the spawn recovery marmalade bug"}}`,
			`{"type":"assistant","cwd":"/Users/u/workspace/services/loom-core","sessionId":"11111111-aaaa-bbbb-cccc-000000000001","timestamp":"2026-07-14T10:00:05.000Z","message":{"role":"assistant","content":[{"type":"text","text":"On it — the marmalade bug lives in embed.go"}]}}`,
		}, "\n")+"\n",
		now.Add(-1*time.Hour))

	// Claude session in another repo.
	writeFixture(t,
		filepath.Join(claudeRoot, "-Users-u-workspace-labs-game", "22222222-aaaa-bbbb-cccc-000000000002.jsonl"),
		`{"type":"user","cwd":"/Users/u/workspace/labs/game","sessionId":"22222222-aaaa-bbbb-cccc-000000000002","timestamp":"2026-07-13T09:00:00.000Z","message":{"role":"user","content":"tune the jump physics"}}`+"\n",
		now.Add(-30*time.Hour))

	// Codex session in services/loom-core.
	writeFixture(t,
		filepath.Join(codexRoot, "2026", "07", "14", "rollout-2026-07-14T08-00-00-abc123.jsonl"),
		strings.Join([]string{
			`{"timestamp":"2026-07-14T08:00:00.000Z","type":"session_meta","payload":{"id":"abc123","cwd":"/Users/u/workspace/services/loom-core","originator":"Codex Desktop","cli_version":"0.144.0","source":"vscode"}}`,
			`{"timestamp":"2026-07-14T08:01:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"investigate the marmalade flake in CI"}]}}`,
		}, "\n")+"\n",
		now.Add(-2*time.Hour))

	return Store{ClaudeRoot: claudeRoot, CodexRoot: codexRoot}
}

func TestListBothVendorsNewestFirst(t *testing.T) {
	store := fixtureStore(t)
	sessions, err := store.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 {
		t.Fatalf("got %d sessions, want 3", len(sessions))
	}
	if sessions[0].Vendor != VendorClaude || sessions[1].Vendor != VendorCodex {
		t.Fatalf("order wrong: %s then %s (want claude then codex)", sessions[0].Vendor, sessions[1].Vendor)
	}
	if sessions[0].CWD != "/Users/u/workspace/services/loom-core" {
		t.Fatalf("claude cwd = %q", sessions[0].CWD)
	}
	if sessions[1].ID != "abc123" || sessions[1].Source != "Codex Desktop" {
		t.Fatalf("codex meta = %+v", sessions[1])
	}
	if sessions[0].StartedAt.IsZero() || sessions[1].StartedAt.IsZero() {
		t.Fatal("started_at not extracted from transcript heads")
	}
}

func TestListVendorAndCwdFilters(t *testing.T) {
	store := fixtureStore(t)

	claudeOnly, err := store.List(ListOptions{Vendor: VendorClaude})
	if err != nil {
		t.Fatal(err)
	}
	if len(claudeOnly) != 2 {
		t.Fatalf("claude-only = %d sessions, want 2", len(claudeOnly))
	}

	loomCore, err := store.List(ListOptions{CwdContains: "services/loom-core"})
	if err != nil {
		t.Fatal(err)
	}
	if len(loomCore) != 2 {
		t.Fatalf("cwd-filtered = %d sessions, want 2 (one per vendor)", len(loomCore))
	}
	for _, s := range loomCore {
		if !strings.Contains(s.CWD, "services/loom-core") {
			t.Fatalf("unexpected cwd %q", s.CWD)
		}
	}
}

func TestListSinceFilter(t *testing.T) {
	store := fixtureStore(t)
	recent, err := store.List(ListOptions{Since: time.Now().Add(-3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Fatalf("since-filtered = %d sessions, want 2", len(recent))
	}
}

func TestSearchAcrossVendors(t *testing.T) {
	store := fixtureStore(t)
	matches, err := store.Search(SearchOptions{Query: "MARMALADE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 3 {
		t.Fatalf("got %d matches, want 3 (2 claude lines + 1 codex line)", len(matches))
	}
	vendors := map[string]int{}
	for _, m := range matches {
		vendors[m.Vendor]++
		if !strings.Contains(strings.ToLower(m.Snippet), "marmalade") {
			t.Fatalf("snippet missing needle: %q", m.Snippet)
		}
		if m.SessionID == "" || m.Line == 0 {
			t.Fatalf("match missing identity: %+v", m)
		}
	}
	if vendors[VendorClaude] != 2 || vendors[VendorCodex] != 1 {
		t.Fatalf("vendor split = %v", vendors)
	}
}

func TestSearchRoleAndTimestampExtraction(t *testing.T) {
	store := fixtureStore(t)
	matches, err := store.Search(SearchOptions{Query: "jump physics"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if matches[0].Role != "user" {
		t.Fatalf("role = %q, want user", matches[0].Role)
	}
	if matches[0].Timestamp == "" {
		t.Fatal("timestamp not extracted")
	}
}

func TestSearchMaxPerSession(t *testing.T) {
	store := fixtureStore(t)
	matches, err := store.Search(SearchOptions{Query: "marmalade", MaxPerSession: 1})
	if err != nil {
		t.Fatal(err)
	}
	perSession := map[string]int{}
	for _, m := range matches {
		perSession[m.SessionID]++
	}
	for id, n := range perSession {
		if n > 1 {
			t.Fatalf("session %s has %d matches, want <=1", id, n)
		}
	}
}

func TestMissingRootsAreNotErrors(t *testing.T) {
	store := Store{ClaudeRoot: "/nonexistent/claude", CodexRoot: "/nonexistent/codex"}
	sessions, err := store.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("got %d sessions from missing roots", len(sessions))
	}
	matches, err := store.Search(SearchOptions{Query: "anything"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("got %d matches from missing roots", len(matches))
	}
}

func TestReadLimitedLineOversized(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 3<<20)
	path := filepath.Join(dir, "big.jsonl")
	writeFixture(t, path,
		`{"type":"user","cwd":"/tmp/big","sessionId":"s","timestamp":"2026-07-14T10:00:00.000Z","message":{"role":"user","content":"`+big+` needle-here"}}`+"\n"+
			`{"type":"user","cwd":"/tmp/big","message":{"role":"user","content":"short needle-here"}}`+"\n",
		time.Now())

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// A 1MB cap truncates the first line but must still land at the start
	// of the second line.
	r := newTestReader(f)
	line1, err := readLimitedLine(r, 1<<20)
	if err != nil {
		t.Fatalf("line1 err: %v", err)
	}
	if len(line1) != 1<<20 {
		t.Fatalf("line1 len = %d, want %d", len(line1), 1<<20)
	}
	line2, _ := readLimitedLine(r, 1<<20)
	if !strings.Contains(string(line2), "short needle-here") {
		t.Fatalf("line2 misaligned: %.80s", line2)
	}
}
