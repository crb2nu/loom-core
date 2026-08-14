package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/journalengine"
	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

func newPromptStore(t *testing.T) (*store.Store, *store.BacklogItem) {
	t.Helper()
	st, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(t.TempDir(), "prompt.db"),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	item := &store.BacklogItem{
		ID:       "MILLS-PROMPT-JOURNAL-001",
		Title:    "wire the item memory journal",
		State:    store.BacklogRunning,
		Priority: store.P2,
	}
	if err := st.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	return st, item
}

func seedItemJournal(t *testing.T, st *store.Store, item *store.BacklogItem) {
	t.Helper()
	j, err := st.ItemMemory.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get journal: %v", err)
	}
	j.RecordTurn(0, `Pipeline stage "research" ran (attempt 1).`, nil,
		"Outcome: succeeded.\nLog tail:\nresearch found pkg/mills/pipeline/runner.go")
	if err := st.ItemMemory.Put(context.Background(), item.ID, j); err != nil {
		t.Fatalf("put journal: %v", err)
	}
}

// The journal must LEAD the prompt (after any invariant preamble) — everything
// below it is volatile per-item text, and a stable block placed behind volatile
// bytes buys nothing from the prefix cache.
func TestItemJournalLeadsStagePrompt(t *testing.T) {
	t.Setenv(pipeline.ItemJournalEnv, "1")
	st, item := newPromptStore(t)
	seedItemJournal(t, st, item)

	prompt := implementPromptFor(st.ItemMemory)(pipeline.JobContext{
		Stage: pipeline.Stage{ID: "implement"},
		Item:  item,
	})
	if !strings.HasPrefix(prompt, itemJournalPreface) {
		t.Fatalf("journal block does not lead the implement prompt:\n%s", prompt)
	}
	journalEnd := strings.Index(prompt, "Implement backlog item")
	if journalEnd < 0 {
		t.Fatalf("stage template missing:\n%s", prompt)
	}
	if !strings.Contains(prompt[:journalEnd], "research found pkg/mills/pipeline/runner.go") {
		t.Errorf("journal content is not above the stage template:\n%s", prompt)
	}
	if strings.Index(prompt, "Backlog context:") < journalEnd {
		t.Error("volatile backlog context must sit below the journal")
	}
}

// The research stage's repo digest is read once at construction and identical
// for every item, so it belongs above the journal, not trailing the volatile
// per-item text (where it used to sit).
func TestResearchPromptDigestLeadsJournal(t *testing.T) {
	t.Setenv(pipeline.ItemJournalEnv, "1")
	st, item := newPromptStore(t)
	seedItemJournal(t, st, item)

	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, "pkg", "mills"), 0o755); err != nil {
		t.Fatalf("seed repo root: %v", err)
	}
	prompt := researchPromptFor(repoRoot, st.ItemMemory)(pipeline.JobContext{
		Stage: pipeline.Stage{ID: "research"},
		Item:  item,
	})
	digestAt := strings.Index(prompt, "Repository directory layout")
	journalAt := strings.Index(prompt, itemJournalPreface)
	templateAt := strings.Index(prompt, "Research backlog item")
	if digestAt < 0 || journalAt < 0 || templateAt < 0 {
		t.Fatalf("prompt missing a required block (digest=%d journal=%d template=%d):\n%s",
			digestAt, journalAt, templateAt, prompt)
	}
	if digestAt >= journalAt || journalAt >= templateAt {
		t.Errorf("assembly order = digest@%d journal@%d template@%d, want digest < journal < template",
			digestAt, journalAt, templateAt)
	}
	if !strings.HasSuffix(strings.TrimSpace(prompt), researchPathDiscipline) {
		t.Error("path discipline must remain the last block so 'the structure above' still reads true")
	}
}

func TestItemJournalOmittedWhenDisabledOrEmpty(t *testing.T) {
	st, item := newPromptStore(t)
	seedItemJournal(t, st, item)

	// Knob off (default).
	if got := itemJournalBlock(st.ItemMemory, item); got != "" {
		t.Errorf("journal block rendered with the knob off:\n%s", got)
	}

	t.Setenv(pipeline.ItemJournalEnv, "1")
	// Nil DAO (store-less wiring).
	if got := itemJournalBlock(nil, item); got != "" {
		t.Errorf("journal block rendered without a DAO:\n%s", got)
	}
	// An item with no memory yet renders nothing rather than the
	// "You are at the beginning" marker, which would only waste tokens.
	empty := &store.BacklogItem{ID: "MILLS-PROMPT-JOURNAL-EMPTY", Title: "fresh"}
	if err := st.Backlog.Put(context.Background(), empty); err != nil {
		t.Fatalf("seed empty item: %v", err)
	}
	if got := itemJournalBlock(st.ItemMemory, empty); got != "" {
		t.Errorf("empty journal rendered a block:\n%s", got)
	}
	if journalengine.EmptyJournal == "" {
		t.Fatal("journalengine.EmptyJournal unexpectedly blank")
	}
}

func TestResearchNotesReachTheImplementPrompt(t *testing.T) {
	jc := pipeline.JobContext{
		Stage: pipeline.Stage{ID: "implement"},
		Item:  &store.BacklogItem{ID: "MILLS-RESEARCH-NOTES-001", Title: "pipe research into implement"},
		Prior: map[string]pipeline.StageOutput{
			"research": {Artifacts: map[string]any{
				"research_notes": "runner.go:1425 is the durable stage write; gates read stage_results.",
			}},
		},
	}
	prompt := implementPromptFor(nil)(jc)
	if !strings.Contains(prompt, researchNotesHeader) {
		t.Fatalf("implement prompt missing the research findings header:\n%s", prompt)
	}
	if !strings.Contains(prompt, "runner.go:1425 is the durable stage write") {
		t.Errorf("implement prompt missing the research notes body:\n%s", prompt)
	}
	// The retry block stays last.
	jc.RetryContext = &pipeline.StageRetryContext{Attempt: 2, GateStage: "post_implement_gate"}
	retry := implementPromptFor(nil)(jc)
	if strings.Index(retry, "RETRY CONTEXT") < strings.Index(retry, researchNotesHeader) {
		t.Error("retry context must remain the last block of the implement prompt")
	}
}

func TestResearchNotesKillSwitchAndGuards(t *testing.T) {
	notes := map[string]any{"research_notes": "grounded findings"}
	base := pipeline.JobContext{
		Stage: pipeline.Stage{ID: "implement"},
		Item:  &store.BacklogItem{ID: "MILLS-RESEARCH-NOTES-002"},
		Prior: map[string]pipeline.StageOutput{"research": {Artifacts: notes}},
	}

	t.Setenv(researchNotesEnv, "0")
	if got := researchNotesBlock(base); got != "" {
		t.Errorf("kill switch did not suppress the block:\n%s", got)
	}
	t.Setenv(researchNotesEnv, "1")
	if got := researchNotesBlock(base); got == "" {
		t.Error("block suppressed with the kill switch off")
	}

	// No research stage in Prior, an empty note, and a non-string artifact all
	// degrade to "" rather than panicking or emitting an empty section.
	for name, jc := range map[string]pipeline.JobContext{
		"no prior":      {Item: base.Item},
		"nil artifacts": {Item: base.Item, Prior: map[string]pipeline.StageOutput{"research": {}}},
		"blank note":    {Item: base.Item, Prior: map[string]pipeline.StageOutput{"research": {Artifacts: map[string]any{"research_notes": "   "}}}},
		"non-string":    {Item: base.Item, Prior: map[string]pipeline.StageOutput{"research": {Artifacts: map[string]any{"research_notes": 42}}}},
	} {
		if got := researchNotesBlock(jc); got != "" {
			t.Errorf("%s: expected no block, got:\n%s", name, got)
		}
	}
}

func TestResearchNotesAreCapped(t *testing.T) {
	jc := pipeline.JobContext{
		Item: &store.BacklogItem{ID: "MILLS-RESEARCH-NOTES-003"},
		Prior: map[string]pipeline.StageOutput{
			"research": {Artifacts: map[string]any{
				"research_notes": strings.Repeat("n", maxResearchNotesBytes*3),
			}},
		},
	}
	got := researchNotesBlock(jc)
	body := strings.TrimPrefix(got, researchNotesHeader+"\n\n")
	if len(body) > maxResearchNotesBytes {
		t.Fatalf("notes body = %d bytes, want <= %d", len(body), maxResearchNotesBytes)
	}
	if !strings.HasSuffix(body, "[... research notes truncated]") {
		t.Error("truncated notes must carry the elision marker")
	}
}
