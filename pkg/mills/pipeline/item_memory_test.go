package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/journalengine"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// TestItemMemory_RendersAsStrictPrefixExtension is the consumer-side assertion
// pkg/journalengine/doc.go demands: the package can prove Render is append-only
// in isolation, but only this test proves the Mills record hook does not smuggle
// a volatile byte (a clock reading, an attempt counter in the wrong place, a
// map rendered unsorted) into the stable prefix.
func TestItemMemory_RendersAsStrictPrefixExtension(t *testing.T) {
	t.Setenv(ItemJournalEnv, "1")
	st, _, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	ctx := context.Background()

	stages := []struct {
		id      string
		attempt int
		out     StageOutput
		logTail string
		err     error
	}{
		{id: "research", attempt: 1, out: StageOutput{LogTail: "prior art: pkg/mills/pipeline/runner.go"}, logTail: "prior art: pkg/mills/pipeline/runner.go"},
		{id: "plan_slice", attempt: 1, out: StageOutput{LogTail: "3 slices persisted"}, logTail: "3 slices persisted"},
		{
			id:      "implement",
			attempt: 1,
			out: StageOutput{
				FilesChanged:   []string{"pkg/mills/pipeline/runner.go", "pkg/mills/store/dao_item_memory.go"},
				LinesAdded:     120,
				LinesRemoved:   4,
				DiffPatch:      []byte("diff --git a/x b/x\n+y\n"),
				CommitMessages: []string{"feat(mills): item memory\n\nlong body that must collapse to one line"},
			},
			logTail: "pushed feat/mills-item-memory",
			err:     errors.New("scope gate failed: out-of-envelope path"),
		},
		{id: "implement", attempt: 2, out: StageOutput{FilesChanged: []string{"pkg/mills/pipeline/runner.go"}, LinesAdded: 90}, logTail: "pushed again"},
	}

	var renders []string
	for _, s := range stages {
		r.recordItemMemory(ctx, item, Stage{ID: s.id}, s.attempt, s.out, s.logTail, s.err)
		j, err := st.ItemMemory.Get(ctx, item.ID)
		if err != nil {
			t.Fatalf("reload journal after %s: %v", s.id, err)
		}
		renders = append(renders, j.Render())
	}

	if len(renders) < 3 {
		t.Fatalf("need at least 3 renders to assert the contract, got %d", len(renders))
	}
	for i := 1; i < len(renders); i++ {
		if err := journalengine.CheckPrefixExtension(renders[i-1], renders[i]); err != nil {
			t.Fatalf("prefix cache contract broken between stage %d and %d: %v", i-1, i, err)
		}
	}

	final := renders[len(renders)-1]
	for _, want := range []string{
		`Pipeline stage "research" ran (attempt 1).`,
		`Pipeline stage "implement" ran (attempt 2).`,
		"Outcome: FAILED — scope gate failed: out-of-envelope path",
		"Diff: 2 file(s), +120/-4",
		"feat(mills): item memory long body that must collapse to one line",
	} {
		if !strings.Contains(final, want) {
			t.Errorf("journal render missing %q:\n%s", want, final)
		}
	}
	// The patch bytes themselves must never enter the journal — it is re-sent
	// in every later prompt, and the stat carries the same signal for a
	// fraction of the tokens.
	if strings.Contains(final, "diff --git") {
		t.Errorf("journal render carries raw patch bytes:\n%s", final)
	}
	// Epoch must be derived from the journal, never the clock.
	if strings.Contains(final, time.Now().UTC().Format("2006-01-02")) {
		t.Errorf("journal render carries a wall-clock date; the prefix must be time-free:\n%s", final)
	}
}

func TestItemMemory_DisabledByDefault(t *testing.T) {
	st, _, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	ctx := context.Background()

	r.recordItemMemory(ctx, item, Stage{ID: "implement"}, 1, StageOutput{LogTail: "did work"}, "did work", nil)

	j, err := st.ItemMemory.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := j.Render(); got != journalengine.EmptyJournal {
		t.Errorf("journal recorded with the knob off: %q", got)
	}
}

// A DAO failure must be logged and swallowed: the stage result is already
// durable at the record point, so failing here would erase real work.
func TestItemMemory_RecordSurvivesStoreFailure(t *testing.T) {
	t.Setenv(ItemJournalEnv, "1")
	st, _, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)

	if _, err := st.DB().ExecContext(context.Background(), `DROP TABLE backlog_item_memory`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	// No panic, no propagation — recordItemMemory returns void by design.
	r.recordItemMemory(context.Background(), item, Stage{ID: "implement"}, 1,
		StageOutput{LogTail: "did work"}, "did work", nil)
}

func TestItemMemory_OversizedRecordIsSkipped(t *testing.T) {
	t.Setenv(ItemJournalEnv, "1")
	st, _, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)
	ctx := context.Background()

	// Each recorded outcome is capped at 8 KiB, so drive the snapshot over the
	// 256 KiB row cap by recording many of them.
	huge := strings.Repeat("x", itemMemoryMaxOwnBytes)
	for i := 0; i < 64; i++ {
		r.recordItemMemory(ctx, item, Stage{ID: "implement"}, i+1, StageOutput{}, huge, nil)
	}
	j, err := st.ItemMemory.Get(ctx, item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	snap, err := j.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(snap) > store.ItemMemoryMaxSnapshotBytes {
		t.Errorf("persisted snapshot = %d bytes, over the %d cap", len(snap), store.ItemMemoryMaxSnapshotBytes)
	}
}

func TestItemMemoryOutcome_TruncatesFromTheTail(t *testing.T) {
	out := itemMemoryOutcome(
		StageOutput{FilesChanged: []string{"a.go"}, LinesAdded: 1},
		strings.Repeat("L", itemMemoryMaxOwnBytes*2),
		nil,
	)
	if len(out) > itemMemoryMaxOwnBytes {
		t.Fatalf("composed outcome = %d bytes, want <= %d", len(out), itemMemoryMaxOwnBytes)
	}
	if !strings.HasSuffix(out, itemMemoryTruncationMarker) {
		t.Errorf("truncated outcome does not carry the marker:\n%s", out[len(out)-80:])
	}
	// The structured summary sits above the log tail, so truncation keeps it.
	if !strings.Contains(out, "Diff: 1 file(s), +1/-0 — a.go") {
		t.Error("truncation dropped the diff stat; it must outrank the log tail")
	}
}

func TestItemJournalEnabled_ParsesTruthyForms(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv(ItemJournalEnv, v)
		if !ItemJournalEnabled() {
			t.Errorf("%q should enable the item journal", v)
		}
	}
	for _, v := range []string{"", "0", "false", "off", "no", "maybe"} {
		t.Setenv(ItemJournalEnv, v)
		if ItemJournalEnabled() {
			t.Errorf("%q should not enable the item journal", v)
		}
	}
}

// The record hook fires from runStage, so a full drive must leave one journal
// turn per non-gate stage.
func TestItemMemory_DriveRecordsEveryStage(t *testing.T) {
	t.Setenv(ItemJournalEnv, "1")
	st, run, item := newRunnerEnv(t)
	disp := &fakeDispatcher{canned: map[string]StageOutput{
		"implement": {
			FilesChanged:   []string{"foo.go"},
			LinesAdded:     5,
			DiffPatch:      []byte("diff --git a/foo.go b/foo.go\n+x\n"),
			CommitMessages: []string{"feat: x"},
		},
		"mr":    {MRIID: 42},
		"merge": {MergedSHA: "abcdef"},
	}}
	r := New(st, newPassingGates(t), disp, nil)
	r.Clock = func() time.Time { return time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC) }
	if err := r.Drive(context.Background(), run, item); err != nil {
		t.Fatalf("drive: %v", err)
	}
	j, err := st.ItemMemory.Get(context.Background(), item.ID)
	if err != nil {
		t.Fatalf("get journal: %v", err)
	}
	rendered := j.Render()
	for _, stage := range []string{"plan_slice", "research", "implement", "tests", "pr_self_review", "mr", "ci_watch", "merge", "cleanup"} {
		if !strings.Contains(rendered, `Pipeline stage "`+stage+`" ran`) {
			t.Errorf("journal missing stage %q:\n%s", stage, rendered)
		}
	}
	// Epochs are derived from the journal's own entry count, so they are
	// strictly increasing per turn and identical within one. A clock-derived
	// epoch would be neither reproducible nor replay-stable.
	prevEpoch := -1
	for i, e := range j.Entries() {
		switch {
		case e.Kind == journalengine.KindSituation:
			if e.Epoch <= prevEpoch {
				t.Fatalf("entry %d epoch = %d, not greater than the previous turn's %d", i, e.Epoch, prevEpoch)
			}
			prevEpoch = e.Epoch
		case e.Epoch != prevEpoch:
			t.Fatalf("entry %d epoch = %d, want %d (same turn)", i, e.Epoch, prevEpoch)
		}
	}
}
