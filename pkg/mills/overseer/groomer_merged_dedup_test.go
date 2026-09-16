package overseer

import (
	"context"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

func (e *groomerEnv) seedInState(t *testing.T, id, title string, state store.BacklogState, createdAgo time.Duration) *store.BacklogItem {
	t.Helper()
	item := &store.BacklogItem{
		ID: id, Title: title, State: state,
		Priority: store.P2, CreatedBy: "test",
		CreatedAt: e.now.Add(-createdAgo),
	}
	if err := e.store.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	return item
}

// The kill test. An escalated item whose duplicate already MERGED is provably
// dead work — the change is on main under the canonical item — so it retires.
// Before this pass the groomer only listed the queued lane, so escalated
// duplicates accumulated forever and kept re-seeding the council.
func TestGroomerRetiresEscalatedDuplicateOfMerged(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}, nil)
	env.seedInState(t, "CANON", "Add size aware spawn state pruning with HUD pressure",
		store.BacklogMerged, 48*time.Hour)
	env.seedInState(t, "DUP", "Add the size aware spawn state pruning with HUD pressure",
		store.BacklogEscalated, 24*time.Hour)

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 1 {
		t.Fatalf("acted = %d, want 1 (res=%+v)", res.Acted, res)
	}
	if got := env.itemState(t, "DUP"); got != store.BacklogRetired {
		t.Fatalf("escalated duplicate state = %s, want retired", got)
	}
	if got := env.itemState(t, "CANON"); got != store.BacklogMerged {
		t.Fatalf("canonical state = %s, want merged (must never be touched)", got)
	}
}

// An empty queued lane must not short-circuit the tick: queue_depth 0 is the
// steady state between council rounds, which is exactly when the escalated pile
// most needs draining. This is the regression the pass would silently have had.
func TestGroomerGroomsEscalatedWhenQueueEmpty(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}, nil)
	// No queued items at all.
	env.seedInState(t, "CANON", "Wire gate outcome telemetry into the HUD",
		store.BacklogMerged, 48*time.Hour)
	env.seedInState(t, "DUP", "Wire the gate outcome telemetry into the HUD",
		store.BacklogEscalated, 24*time.Hour)

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 1 {
		t.Fatalf("acted = %d, want 1 with an empty queued lane (res=%+v)", res.Acted, res)
	}
	if got := env.itemState(t, "DUP"); got != store.BacklogRetired {
		t.Fatalf("state = %s, want retired", got)
	}
}

// An escalated item whose twin is merely QUEUED or ESCALATED must survive:
// nobody has done that work yet, so retiring it silently loses it. Only a
// merged canonical proves the work exists.
func TestGroomerKeepsEscalatedWhenTwinNotMerged(t *testing.T) {
	for _, twinState := range []store.BacklogState{store.BacklogQueued, store.BacklogEscalated} {
		t.Run(string(twinState), func(t *testing.T) {
			env := newGroomerEnv(t, mills.GroomerPolicy{
				Enabled: true, DryRun: boolPtr(false),
				Allow: mills.GroomerAllowPolicy{DedupClose: true},
			}, nil)
			env.seedInState(t, "TWIN", "Add dependency health policy evaluator for Mills",
				twinState, 48*time.Hour)
			env.seedInState(t, "ESC", "Add the dependency health policy evaluator for Mills",
				store.BacklogEscalated, 24*time.Hour)

			if _, err := env.groomer.Tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if got := env.itemState(t, "ESC"); got != store.BacklogEscalated {
				t.Fatalf("escalated item = %s, want escalated — no merged canonical exists", got)
			}
		})
	}
}

// Unrelated titles must not be dragged together just because one merged.
func TestGroomerIgnoresDistinctEscalatedItem(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}, nil)
	env.seedInState(t, "CANON", "Add size aware spawn state pruning",
		store.BacklogMerged, 48*time.Hour)
	env.seedInState(t, "OTHER", "Document operator runbook for storage saturation",
		store.BacklogEscalated, 24*time.Hour)

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 0 {
		t.Fatalf("acted = %d, want 0 for a distinct item", res.Acted)
	}
	if got := env.itemState(t, "OTHER"); got != store.BacklogEscalated {
		t.Fatalf("distinct item = %s, want escalated", got)
	}
}

// The dedup_close allow flag governs this pass too — it reuses the action
// rather than adding parallel policy surface, so an operator who has not opted
// in gets a planned/skipped decision, never a transition.
func TestGroomerEscalatedDedupRespectsAllowFlag(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{DedupClose: false},
	}, nil)
	env.seedInState(t, "CANON", "Add size aware spawn state pruning with HUD pressure",
		store.BacklogMerged, 48*time.Hour)
	env.seedInState(t, "DUP", "Add the size aware spawn state pruning with HUD pressure",
		store.BacklogEscalated, 24*time.Hour)

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 0 {
		t.Fatalf("acted = %d, want 0 when dedup_close is not allowed", res.Acted)
	}
	if got := env.itemState(t, "DUP"); got != store.BacklogEscalated {
		t.Fatalf("state = %s, want escalated", got)
	}
}

// Dry run records an auditable decision without transitioning, so the pass can
// soak before an operator flips the allow flag.
func TestGroomerEscalatedDedupDryRunPlansOnly(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(true),
		Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}, nil)
	env.seedInState(t, "CANON", "Add size aware spawn state pruning with HUD pressure",
		store.BacklogMerged, 48*time.Hour)
	env.seedInState(t, "DUP", "Add the size aware spawn state pruning with HUD pressure",
		store.BacklogEscalated, 24*time.Hour)

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Planned != 1 {
		t.Fatalf("planned = %d, want 1 (res=%+v)", res.Planned, res)
	}
	if res.Acted != 0 {
		t.Fatalf("acted = %d, want 0 in dry run", res.Acted)
	}
	if got := env.itemState(t, "DUP"); got != store.BacklogEscalated {
		t.Fatalf("state = %s, want escalated in dry run", got)
	}
}

func (e *groomerEnv) seedInStateWithSlices(
	t *testing.T, id, title string, state store.BacklogState,
	createdAgo time.Duration, files ...string,
) *store.BacklogItem {
	t.Helper()
	item := &store.BacklogItem{
		ID: id, Title: title, State: state,
		Priority: store.P2, CreatedBy: "test",
		CreatedAt: e.now.Add(-createdAgo),
		Slices:    []store.Slice{{Name: "impl", Files: files}},
	}
	if err := e.store.Backlog.Put(context.Background(), item); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	return item
}

// seedDoneRunWithFiles gives a backlog item one merged (done) pipeline run
// whose implement stage captured the given files_changed — the authoritative
// record of what the item's branch actually delivered.
func (e *groomerEnv) seedDoneRunWithFiles(t *testing.T, backlogID string, files ...string) {
	t.Helper()
	ctx := context.Background()
	started := e.now.Add(-36 * time.Hour)
	ended := e.now.Add(-35 * time.Hour)
	run := &store.PipelineRun{
		ID: "PIPE-" + backlogID, BacklogID: backlogID,
		Template: "mills-default-pipeline", State: store.PipelineDone,
		Attempts: 1, StartedAt: started,
	}
	run.EndedAt = &ended
	if err := e.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run for %s: %v", backlogID, err)
	}
	outcome := store.StageOutcomeSuccess
	if err := e.store.Pipeline.PutStage(ctx, &store.StageResult{
		PipelineRunID: run.ID, Stage: "implement", Attempt: 1,
		StartedAt: started, EndedAt: &ended, Outcome: &outcome,
		Artifacts: map[string]any{"files_changed": files},
	}); err != nil {
		t.Fatalf("seed stage for %s: %v", backlogID, err)
	}
}

// The kill test for the sibling-slice false retire. The live incident: the
// …spawn-state-pruning-with-hud-pressure-s-2 slice (HUD pressure metrics,
// internal/hud/monitor + fleetview) reads near-identical to
// bl-hud-spawn-state-pressure-prune-20260726, whose merge (!1241) delivered
// only the prune mechanics under internal/spawn. Title similarity clears the
// deterministic bar, but the merged run's captured files never touch the
// candidate's declared slice files — so the retire must be vetoed: the
// candidate's work is demonstrably NOT on main.
func TestGroomerVetoesMergedDedupOnDisjointDeliveredFiles(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}, nil)
	env.seedInState(t, "CANON", "Add size aware spawn state pruning with HUD pressure",
		store.BacklogMerged, 48*time.Hour)
	env.seedDoneRunWithFiles(t, "CANON",
		"internal/spawn/controller.go", "internal/spawn/store.go",
		"pkg/mills/pipeline/spawn_class.go", "changelog.d/prune.fixed.md")
	env.seedInStateWithSlices(t, "DUP", "Add the size aware spawn state pruning with HUD pressure",
		store.BacklogEscalated, 24*time.Hour,
		"internal/hud/monitor/spawn_pressure.go", "internal/hud/fleetview/spawn_pressure.go")

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 0 {
		t.Fatalf("acted = %d, want 0 — merged run never delivered the candidate's files (res=%+v)", res.Acted, res)
	}
	if got := env.itemState(t, "DUP"); got != store.BacklogEscalated {
		t.Fatalf("state = %s, want escalated (the -2 work is not on main)", got)
	}
	events, err := env.store.Events.ListBySubject(context.Background(), groomerSubjectKind, "DUP", 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Kind == "overseer.groomer.dedup_scope_veto" {
			found = true
			if ev.Payload["reason"] != "no_scope_intersection" {
				t.Fatalf("veto reason = %v, want no_scope_intersection", ev.Payload["reason"])
			}
			if ev.Payload["canonical_id"] != "CANON" {
				t.Fatalf("veto canonical_id = %v, want CANON", ev.Payload["canonical_id"])
			}
		}
	}
	if !found {
		t.Fatalf("expected a dedup_scope_veto audit event, got %v", events)
	}
}

// When the merged run's captured files DO cover the candidate's declared
// slice files, the retire proceeds exactly as before — with the witness in
// the audit payload.
func TestGroomerMergedDedupRetiresWhenDeliveredFilesCover(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false),
		Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}, nil)
	env.seedInState(t, "CANON", "Add size aware spawn state pruning with HUD pressure",
		store.BacklogMerged, 48*time.Hour)
	env.seedDoneRunWithFiles(t, "CANON",
		"internal/spawn/controller.go", "internal/hud/monitor/spawn_pressure.go")
	env.seedInStateWithSlices(t, "DUP", "Add the size aware spawn state pruning with HUD pressure",
		store.BacklogEscalated, 24*time.Hour,
		"internal/hud/monitor/spawn_pressure.go", "internal/hud/fleetview/spawn_pressure.go")

	res, err := env.groomer.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if res.Acted != 1 {
		t.Fatalf("acted = %d, want 1 when delivered files cover the declared slice (res=%+v)", res.Acted, res)
	}
	if got := env.itemState(t, "DUP"); got != store.BacklogRetired {
		t.Fatalf("state = %s, want retired", got)
	}
	events, err := env.store.Events.ListBySubject(context.Background(), groomerSubjectKind, "DUP", 10)
	if err != nil || len(events) == 0 {
		t.Fatalf("expected retire event, got %v err=%v", events, err)
	}
	if events[0].Payload["scope_coverage"] != string(store.ScopeCoverageOverlap) {
		t.Fatalf("scope_coverage = %v, want overlap", events[0].Payload["scope_coverage"])
	}
	if events[0].Payload["scope_witness"] != "internal/hud/monitor/spawn_pressure.go" {
		t.Fatalf("scope_witness = %v", events[0].Payload["scope_witness"])
	}
}

// A canonical with no captured runs falls back to its declared slices; a
// canonical with neither capture nor slices cannot prove it delivered the
// candidate's declared files, so the retire is vetoed as unverifiable.
func TestGroomerMergedDedupDeclaredFallbackAndUnverifiable(t *testing.T) {
	t.Run("declared slices cover", func(t *testing.T) {
		env := newGroomerEnv(t, mills.GroomerPolicy{
			Enabled: true, DryRun: boolPtr(false),
			Allow: mills.GroomerAllowPolicy{DedupClose: true},
		}, nil)
		env.seedInStateWithSlices(t, "CANON", "Add size aware spawn state pruning with HUD pressure",
			store.BacklogMerged, 48*time.Hour, "internal/hud/monitor/fleet.go")
		env.seedInStateWithSlices(t, "DUP", "Add the size aware spawn state pruning with HUD pressure",
			store.BacklogEscalated, 24*time.Hour, "internal/hud/monitor/spawn_pressure.go")

		res, err := env.groomer.Tick(context.Background())
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
		if res.Acted != 1 {
			t.Fatalf("acted = %d, want 1 via declared-slice fallback (res=%+v)", res.Acted, res)
		}
		if got := env.itemState(t, "DUP"); got != store.BacklogRetired {
			t.Fatalf("state = %s, want retired", got)
		}
	})
	t.Run("unverifiable canonical is vetoed", func(t *testing.T) {
		env := newGroomerEnv(t, mills.GroomerPolicy{
			Enabled: true, DryRun: boolPtr(false),
			Allow: mills.GroomerAllowPolicy{DedupClose: true},
		}, nil)
		env.seedInState(t, "CANON", "Add size aware spawn state pruning with HUD pressure",
			store.BacklogMerged, 48*time.Hour)
		env.seedInStateWithSlices(t, "DUP", "Add the size aware spawn state pruning with HUD pressure",
			store.BacklogEscalated, 24*time.Hour, "internal/hud/monitor/spawn_pressure.go")

		res, err := env.groomer.Tick(context.Background())
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
		if res.Acted != 0 {
			t.Fatalf("acted = %d, want 0 for an unverifiable canonical (res=%+v)", res.Acted, res)
		}
		if got := env.itemState(t, "DUP"); got != store.BacklogEscalated {
			t.Fatalf("state = %s, want escalated", got)
		}
	})
}

// An excluded perfect match must not mask a genuine gray-band canonical.
func TestGroomerMergedSiblingDoesNotMaskDuplicate(t *testing.T) {
	chat := &fakeChat{replies: []string{verdictJSON(t, "duplicate", 0.99)}}
	env := newGroomerEnv(t, mills.GroomerPolicy{Enabled: true}, chat)
	for _, item := range []*store.BacklogItem{
		env.seedInState(t, "SIBLING", "Mills telemetry stage rollup endpoint", store.BacklogMerged, 3*time.Hour),
		env.seedInState(t, "CANDIDATE", "Mills telemetry stage rollup endpoint", store.BacklogEscalated, time.Hour),
	} {
		item.PlanID = "shared-plan"
		if err := env.store.Backlog.Put(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	env.seedInState(t, "GENUINE", "Mills telemetry stage rollup panel for HUD", store.BacklogMerged, 2*time.Hour)
	res, err := env.groomer.Tick(context.Background())
	if err != nil || res.Planned != 1 || res.Skipped != 1 || chat.calls != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", res, chat.calls, err)
	}
	events, err := env.store.Events.ListBySubject(context.Background(), groomerSubjectKind, "CANDIDATE", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Kind == "overseer.groomer.dedup_close.dryrun" && ev.Payload["canonical_id"] == "GENUINE" {
			return
		}
	}
	t.Fatalf("missing genuine duplicate proposal: %+v", events)
}
