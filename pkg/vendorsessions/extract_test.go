package vendorsessions

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestListExtractsTitles: the session list carries a human title — Claude's
// summary record or first user prompt, Codex's first user_message.
func TestListExtractsTitles(t *testing.T) {
	store := fixtureStore(t)
	sessions, err := store.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Session{}
	for _, s := range sessions {
		byID[s.ID] = s
	}

	// Claude session whose head starts with a summary record: the summary is
	// the conversation's own title and wins over the user prompt.
	if got := byID["11111111-aaaa-bbbb-cccc-000000000001"].Title; got != "HUD degraded mode work" {
		t.Fatalf("claude summary title = %q", got)
	}
	// Claude session with no summary: first user prompt titles it.
	if got := byID["22222222-aaaa-bbbb-cccc-000000000002"].Title; got != "tune the jump physics" {
		t.Fatalf("claude prompt title = %q", got)
	}
	// Codex session: first user message (response_item input_text form).
	if got := byID["abc123"].Title; got != "investigate the marmalade flake in CI" {
		t.Fatalf("codex title = %q", got)
	}
}

// TestClaudeTitleSkipsCommandWrappers: injected command/reminder records
// (text starting "<") never title a session; multi-line prompts title by
// their first line, capped with an ellipsis.
func TestClaudeTitleSkipsCommandWrappers(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("y", 200)
	writeFixture(t,
		filepath.Join(root, "-Users-u-repo", "33333333-aaaa-bbbb-cccc-000000000003.jsonl"),
		strings.Join([]string{
			`{"type":"user","cwd":"/Users/u/repo","sessionId":"s3","timestamp":"2026-07-14T10:00:00.000Z","message":{"role":"user","content":"<command-name>/clear</command-name>"}}`,
			`{"type":"user","cwd":"/Users/u/repo","sessionId":"s3","timestamp":"2026-07-14T10:00:01.000Z","message":{"role":"user","content":"first real line ` + long + `\nsecond line"}}`,
		}, "\n")+"\n",
		time.Now())
	sessions, err := Store{ClaudeRoot: root}.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions", len(sessions))
	}
	title := sessions[0].Title
	if strings.HasPrefix(title, "<") {
		t.Fatalf("command wrapper leaked into title: %q", title)
	}
	if !strings.HasPrefix(title, "first real line") {
		t.Fatalf("title = %q, want the first real prompt line", title)
	}
	if strings.Contains(title, "second line") {
		t.Fatalf("title crossed the first newline: %q", title)
	}
	if !strings.HasSuffix(title, "…") || len(title) > maxTitleLen+len("…") {
		t.Fatalf("long title not capped: %d chars %q", len(title), title)
	}
}

// TestKindDetection: Codex automation runs and Claude sidechain (subagent)
// transcripts are tagged so UIs can de-noise them; interactive chats stay
// kindless.
func TestKindDetection(t *testing.T) {
	root := t.TempDir()
	claudeRoot := filepath.Join(root, "claude")
	codexRoot := filepath.Join(root, "codex")

	writeFixture(t,
		filepath.Join(codexRoot, "2026", "07", "14", "rollout-2026-07-14T09-00-00-auto1.jsonl"),
		strings.Join([]string{
			`{"timestamp":"2026-07-14T09:00:00.000Z","type":"session_meta","payload":{"id":"auto1","cwd":"/Users/u/repo","originator":"Codex Desktop","source":"vscode","thread_source":"automation"}}`,
			`{"timestamp":"2026-07-14T09:00:01.000Z","type":"event_msg","payload":{"type":"user_message","message":"Automation: Roadmap issue sync\nrun the reconciliation"}}`,
		}, "\n")+"\n",
		time.Now())

	writeFixture(t,
		filepath.Join(claudeRoot, "-Users-u-repo", "44444444-aaaa-bbbb-cccc-000000000004.jsonl"),
		`{"type":"user","isSidechain":true,"cwd":"/Users/u/repo","sessionId":"s4","timestamp":"2026-07-14T10:00:00.000Z","message":{"role":"user","content":"Search the codebase for callers of Foo"}}`+"\n",
		time.Now())

	sessions, err := Store{ClaudeRoot: claudeRoot, CodexRoot: codexRoot}.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	titles := map[string]string{}
	for _, s := range sessions {
		kinds[s.ID] = s.Kind
		titles[s.ID] = s.Title
	}
	if kinds["auto1"] != KindAutomation {
		t.Fatalf("codex automation kind = %q", kinds["auto1"])
	}
	if titles["auto1"] != "Automation: Roadmap issue sync" {
		t.Fatalf("automation title = %q", titles["auto1"])
	}
	if kinds["44444444-aaaa-bbbb-cccc-000000000004"] != KindSidechain {
		t.Fatalf("claude sidechain kind = %q", kinds["44444444-aaaa-bbbb-cccc-000000000004"])
	}

	// The interactive fixtures carry no kind.
	interactive := fixtureStore(t)
	all, err := interactive.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		if s.Kind != "" {
			t.Fatalf("interactive session %s got kind %q", s.ID, s.Kind)
		}
	}
}

// TestSearchSkipsStructuralJSON: search matches conversational text only —
// JSON keys, infra records, and tool traffic neither match nor produce
// raw-JSONL snippets. Every snippet reads as words, not braces.
func TestSearchSkipsStructuralJSON(t *testing.T) {
	store := fixtureStore(t)

	// "sessionId" appears as a KEY on every claude line but never as spoken
	// text; the old raw-line grep matched everything, the extractor nothing.
	matches, err := store.Search(SearchOptions{Query: "sessionId"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("structural key matched %d times: %+v", len(matches), matches[0])
	}

	// Real hits carry clean snippets — no JSON syntax from the record shell.
	matches, err = store.Search(SearchOptions{Query: "marmalade"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("expected marmalade matches")
	}
	for _, m := range matches {
		if strings.ContainsAny(m.Snippet, "{}") || strings.Contains(m.Snippet, `"type"`) {
			t.Fatalf("snippet leaked raw JSON: %q", m.Snippet)
		}
	}
}

// TestTailExtractsCleanText: federated tail entries ship extracted speech,
// not raw JSONL, and skip infra records entirely.
func TestTailExtractsCleanText(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "-Users-u-repo", "55555555-aaaa-bbbb-cccc-000000000005.jsonl")
	writeFixture(t, path,
		strings.Join([]string{
			`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-07-14T10:00:00.000Z","sessionId":"s5","content":"please fix the flaky test"}`,
			`{"type":"queue-operation","operation":"dequeue","timestamp":"2026-07-14T10:00:01.000Z","sessionId":"s5"}`,
			`{"type":"file-history-snapshot","messageId":"m1","snapshot":{"trackedFileBackups":{}}}`,
			`{"type":"user","cwd":"/Users/u/repo","sessionId":"s5","timestamp":"2026-07-14T10:00:02.000Z","message":{"role":"user","content":"please fix the flaky test"}}`,
			`{"type":"assistant","cwd":"/Users/u/repo","sessionId":"s5","timestamp":"2026-07-14T10:00:03.000Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`,
			`{"type":"assistant","cwd":"/Users/u/repo","sessionId":"s5","timestamp":"2026-07-14T10:00:04.000Z","message":{"role":"assistant","content":[{"type":"text","text":"the fix is in"}]}}`,
		}, "\n")+"\n",
		time.Now())

	entries := Tail(Session{Vendor: VendorClaude, ID: "s5", Path: path}, TailOptions{})
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (enqueue text + user turn + assistant text): %+v", len(entries), entries)
	}
	for _, e := range entries {
		if strings.ContainsAny(e.Text, "{}") {
			t.Fatalf("tail entry leaked raw JSON: %q", e.Text)
		}
	}
	if entries[1].Role != "user" || entries[1].Text != "please fix the flaky test" {
		t.Fatalf("user entry = %+v", entries[1])
	}
	if entries[2].Role != "assistant" || entries[2].Text != "the fix is in" {
		t.Fatalf("assistant entry = %+v", entries[2])
	}
}
