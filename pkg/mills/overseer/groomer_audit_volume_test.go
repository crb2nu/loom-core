package overseer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestGroomerSiblingAuditBoundedAcrossTicks(t *testing.T) {
	for _, merged := range []bool{false, true} {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("merged=%t/dry=%t", merged, dryRun), func(t *testing.T) {
				env := newGroomerEnv(t, mills.GroomerPolicy{
					Enabled: true, DryRun: boolPtr(dryRun),
					Allow: mills.GroomerAllowPolicy{DedupClose: true},
				}, nil)
				const count = 40
				for i := 0; i < count; i++ {
					state := store.BacklogQueued
					if merged {
						state = store.BacklogMerged
						if i == count-1 {
							state = store.BacklogEscalated
						}
					}
					item := env.seedInState(t, fmt.Sprintf("slice-%02d", i), "Wire Mills delivery health telemetry", state, time.Duration(count-i)*time.Minute)
					item.PlanID = "shared-plan"
					if err := env.store.Backlog.Put(context.Background(), item); err != nil {
						t.Fatal(err)
					}
				}
				wantFlags, wantSkipped := count-1, count*(count-1)/2
				if merged {
					wantFlags, wantSkipped = 1, count-1
				}
				for tick := 0; tick < 3; tick++ {
					// A new Groomer must honor the persisted witness too. Toggle
					// dry-run after the first observation without minting new soak
					// evidence for unchanged pairs.
					if tick == 2 {
						env.groomer = &Groomer{Store: env.store, Policy: env.groomer.Policy, Recorder: env.groomer.Recorder, Now: env.groomer.Now}
						env.policy.Overseers.Groomer.DryRun = boolPtr(!dryRun)
					}
					res, err := env.groomer.Tick(context.Background())
					if err != nil || res.Acted != 0 || res.Planned != 0 || res.Errored != 0 || res.Skipped != wantSkipped {
						t.Fatalf("tick %d: result=%+v err=%v", tick, res, err)
					}
					if got := env.eventCount(t, "overseer.groomer.dedup_skipped.plan_sibling"); got != wantFlags {
						t.Fatalf("tick %d: audit records=%d, want %d first witnesses", tick, got, wantFlags)
					}
					wantSoak := 0
					if dryRun {
						wantSoak = wantFlags
					}
					assertGroomerSoakDecisions(t, env, wantSoak)
				}
			})
		}
	}
}

func TestGroomerUnrelatedSlicesDoNotCreateEvidence(t *testing.T) {
	for _, merged := range []bool{false, true} {
		for _, samePlan := range []bool{false, true} {
			t.Run(fmt.Sprintf("merged=%t/samePlan=%t", merged, samePlan), func(t *testing.T) {
				env := newGroomerEnv(t, mills.GroomerPolicy{Enabled: true, DryRun: boolPtr(true)}, nil)
				aState, bState := store.BacklogQueued, store.BacklogQueued
				if merged {
					aState, bState = store.BacklogMerged, store.BacklogEscalated
				}
				for _, item := range []*store.BacklogItem{
					env.seedInState(t, "A", "Document database restore procedures — runbook", aState, 2*time.Hour),
					env.seedInState(t, "B", "Render mobile navigation tabs — frontend", bState, time.Hour),
				} {
					if samePlan {
						item.PlanID = "shared-plan"
						if err := env.store.Backlog.Put(context.Background(), item); err != nil {
							t.Fatal(err)
						}
					}
				}
				res, err := env.groomer.Tick(context.Background())
				if err != nil || res.Acted != 0 || res.Planned != 0 || res.Errored != 0 || res.Skipped != 1 {
					t.Fatalf("result=%+v err=%v", res, err)
				}
				if got := env.eventCount(t, "overseer.groomer.dedup_skipped.plan_sibling"); got != 0 {
					t.Fatalf("unrelated pairs generated %d audit records", got)
				}
				assertGroomerSoakDecisions(t, env, 0)
				if env.itemState(t, "A") != aState || env.itemState(t, "B") != bState {
					t.Fatal("protected item state changed")
				}
			})
		}
	}
}

func TestGroomerSiblingProtectionReevaluatedAfterAudit(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{
		Enabled: true, DryRun: boolPtr(false), Allow: mills.GroomerAllowPolicy{DedupClose: true},
	}, nil)
	a := env.seedInState(t, "A", "Wire Mills delivery health telemetry", store.BacklogQueued, 2*time.Hour)
	b := env.seedInState(t, "B", a.Title, store.BacklogQueued, time.Hour)
	for _, item := range []*store.BacklogItem{a, b} {
		item.PlanID = "shared-plan"
		if err := env.store.Backlog.Put(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := env.groomer.Tick(context.Background()); err != nil || res.Acted != 0 || res.Skipped != 1 {
		t.Fatalf("initial result=%+v err=%v", res, err)
	}
	b.PlanID = "different-plan"
	if err := env.store.Backlog.Put(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if res, err := env.groomer.Tick(context.Background()); err != nil || res.Acted != 1 {
		t.Fatalf("changed identity result=%+v err=%v", res, err)
	}
	if env.itemState(t, "B") != store.BacklogRetired {
		t.Fatal("a historical skip observation blocked a genuine duplicate")
	}
}

func TestGroomerSiblingAuditUnrelatedPairDoesNotHideWitness(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{Enabled: true, DryRun: boolPtr(true)}, nil)
	candidate := &store.BacklogItem{ID: "candidate", Title: "Wire Mills delivery health telemetry", PlanID: "shared-plan"}
	unrelated := &store.BacklogItem{ID: "unrelated", Title: "Document database restore procedures", PlanID: candidate.PlanID}
	related := &store.BacklogItem{ID: "related", Title: candidate.Title, PlanID: candidate.PlanID}
	res := TickResult{}
	audited := map[string]bool{}
	for _, canonical := range []*store.BacklogItem{unrelated, related} {
		if !env.groomer.skipPlanSibling(context.Background(), &res, canonical, candidate, true, audited) {
			t.Fatal("sibling protection was bypassed")
		}
	}
	events, err := env.store.Events.ListBySubject(context.Background(), groomerSubjectKind, candidate.ID, 10)
	if err != nil || len(events) != 1 || events[0].Payload["canonical_id"] != related.ID {
		t.Fatalf("relevant witness missing: events=%+v err=%v", events, err)
	}
	assertGroomerSoakDecisions(t, env, 1)
}

func TestGroomerSiblingAuditFailureBoundedAndRetried(t *testing.T) {
	env := newGroomerEnv(t, mills.GroomerPolicy{Enabled: true, DryRun: boolPtr(true)}, nil)
	candidate := &store.BacklogItem{ID: "candidate", Title: "Wire Mills delivery health telemetry", PlanID: "shared-plan"}
	canonical := &store.BacklogItem{ID: "canonical", Title: candidate.Title, PlanID: candidate.PlanID}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := TickResult{}
	audited := map[string]bool{}
	for i := 0; i < 10; i++ {
		if !env.groomer.skipPlanSibling(ctx, &res, canonical, candidate, true, audited) {
			t.Fatal("audit failure bypassed sibling protection")
		}
	}
	if res.Skipped != 10 || res.Errored != 1 {
		t.Fatalf("failed recording retried for every pair: %+v", res)
	}
	// A fresh tick retries the failed witness; a failed write must not record
	// a soak decision or permanently suppress evidence.
	if !env.groomer.skipPlanSibling(context.Background(), &res, canonical, candidate, true, map[string]bool{}) {
		t.Fatal("retry bypassed sibling protection")
	}
	if got := env.eventCount(t, "overseer.groomer.dedup_skipped.plan_sibling"); got != 1 {
		t.Fatalf("retry recorded %d witnesses, want 1", got)
	}
	assertGroomerSoakDecisions(t, env, 1)
}

func assertGroomerSoakDecisions(t *testing.T, env *groomerEnv, want int) {
	t.Helper()
	days, err := env.store.OverseerSoakTelemetry(context.Background(), env.now.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, day := range days {
		got += day.Decisions
		if day.WouldHaveActed != 0 || day.Disagreements != 0 {
			t.Fatalf("protected pair counted as an intervention: %+v", day)
		}
	}
	if got != want {
		t.Fatalf("soak decisions=%d, want %d", got, want)
	}
}
