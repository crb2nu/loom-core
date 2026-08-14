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
