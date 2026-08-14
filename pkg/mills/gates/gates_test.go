package gates

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

// fixturePolicy yields the validated default policy with a few overrides
// the gate tests exercise (protected paths in particular).
func fixturePolicy(t *testing.T) *mills.Policy {
	t.Helper()
	p := mills.Default()
	// Default returns enabled=true (slice 6.6, 2026-05-02). Re-affirm
	// explicitly so the gates tests don't depend on the default flipping.
	on := true
	p.Enabled = &on
	if err := p.Validate(); err != nil {
		t.Fatalf("fixture policy: %v", err)
	}
	return p
}

func fixtureItem(slices ...store.Slice) *store.BacklogItem {
	return &store.BacklogItem{
		ID:        "MILLS-T",
		Title:     "test item",
		State:     store.BacklogQueued,
		Priority:  store.P2,
		Slices:    slices,
		CreatedBy: "test",
	}
}

// ---------- DiffSize ----------

func TestDiffSize_PassUnderCap(t *testing.T) {
	g := &DiffSize{}
	out, err := g.Evaluate(context.Background(), StageInput{LinesAdded: 100, LinesRemoved: 50})
	if err != nil || !out.Pass {
		t.Errorf("expected pass, got %+v err=%v", out, err)
	}
}

func TestDiffSize_FailOverCap(t *testing.T) {
	g := &DiffSize{MaxLines: 100}
	out, _ := g.Evaluate(context.Background(), StageInput{LinesAdded: 80, LinesRemoved: 50})
	if out.Pass {
		t.Errorf("expected fail, got pass: %+v", out)
	}
	if len(out.Reasons) == 0 || !strings.Contains(out.Reasons[0], "130") {
		t.Errorf("reason should report total lines: %v", out.Reasons)
	}
}

func TestDiffSize_RecoversCountFromPatchWhenTelemetryZero(t *testing.T) {
	g := &DiffSize{MaxLines: 5}
	// The Codex parser reports 0/0 line counts, but the raw patch carries a
	// 7-line change that exceeds the cap. The gate must recover the real size
	// from DiffPatch instead of failing open.
	patch := []byte("--- a/x.go\n+++ b/x.go\n@@ -1,2 +1,7 @@\n+a\n+b\n+c\n+d\n+e\n+f\n-old\n")
	out, _ := g.Evaluate(context.Background(), StageInput{LinesAdded: 0, LinesRemoved: 0, DiffPatch: patch})
	if out.Pass {
		t.Errorf("oversized patch with zero telemetry should fail, got pass: %+v", out)
	}
	// A small patch under the cap still passes.
	small := []byte("--- a/x.go\n+++ b/x.go\n@@ -1 +1,2 @@\n+a\n")
	out, _ = g.Evaluate(context.Background(), StageInput{LinesAdded: 0, LinesRemoved: 0, DiffPatch: small})
	if !out.Pass {
		t.Errorf("small patch should pass: %+v", out)
	}
	// A genuinely empty diff (no patch) still reads as zero and passes — the
	// fallback only fires when a patch is present.
	out, _ = g.Evaluate(context.Background(), StageInput{LinesAdded: 0, LinesRemoved: 0})
	if !out.Pass {
		t.Errorf("empty diff should pass: %+v", out)
	}
}

// ---------- Scope ----------

func TestScope_NoFilesChangedPasses(t *testing.T) {
	g := &Scope{}
	out, _ := g.Evaluate(context.Background(), StageInput{Item: fixtureItem()})
	if !out.Pass {
		t.Errorf("expected pass with no diff, got %+v", out)
	}
}

func TestScope_AllInScopePasses(t *testing.T) {
	g := &Scope{}
	in := StageInput{
		Item: fixtureItem(store.Slice{
			Name:  "core",
			Files: []string{"pkg/auth/login.go"},
			Tests: []string{"pkg/auth/login_test.go"},
		}),
		FilesChanged: []string{"pkg/auth/login.go", "pkg/auth/login_test.go"},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("expected pass, got %+v", out)
	}
}

func TestScope_OutOfScopeFails(t *testing.T) {
	g := &Scope{}
	in := StageInput{
		Item: fixtureItem(store.Slice{
			Name:  "core",
			Files: []string{"pkg/auth/login.go"},
		}),
		FilesChanged: []string{"pkg/auth/login.go", "internal/billing/charge.go"},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if out.Pass {
		t.Errorf("expected fail, got pass: %+v", out)
	}
	if !strings.Contains(out.Reasons[0], "internal/billing/charge.go") {
		t.Errorf("violation should be named: %v", out.Reasons)
	}
}

func TestScope_DeterministicReplay(t *testing.T) {
	sliceA := store.Slice{
		Name:  "a",
		Files: []string{"pkg/auth/*.go", "pkg/auth/login.go", "./pkg/auth/login.go"},
		Tests: []string{"pkg/auth/login_test.go"},
	}
	sliceB := store.Slice{
		Name:  "b",
		Files: []string{"internal/allowed/file.go", "pkg/auth/*.go"},
		Tests: []string{"internal/allowed/file_test.go"},
	}
	wantViolations := []string{
		"internal/outside/a.go", "internal/outside/b.go", "other/a.go",
		"other/b.go", "other/c.go", "other/d.go", "other/e.go", "other/f.go", "other/g.go",
	}
	wantOutcome := Outcome{
		Reasons: []string{
			"[scope.outside_declared_scope] file outside slice scope: internal/outside/a.go",
			"[scope.outside_declared_scope] file outside slice scope: internal/outside/b.go",
			"[scope.outside_declared_scope] file outside slice scope: other/a.go",
			"[scope.outside_declared_scope] file outside slice scope: other/b.go",
			"[scope.outside_declared_scope] file outside slice scope: other/c.go",
			"[scope.outside_declared_scope] file outside slice scope: other/d.go",
			"[scope.outside_declared_scope] file outside slice scope: other/e.go",
			"[scope.outside_declared_scope] file outside slice scope: other/f.go",
			"[scope.outside_declared_scope] file outside slice scope: other/g.go",
		},
		JudgedBy: "go",
	}
	fixtures := []struct {
		name string
		in   StageInput
	}{
		{
			name: "original ordering",
			in: StageInput{
				Item: fixtureItem(sliceA, sliceB),
				FilesChanged: []string{
					"other/g.go", "./other/a.go", "other/f.go", "other/e.go",
					"/workspace/services/loom-core/internal/outside/b.go",
					"other/d.go", "other/c.go", "other/b.go",
					"internal/outside/a.go", "internal/outside/b.go",
				},
			},
		},
		{
			name: "reordered slices declarations and paths",
			in: StageInput{
				Item: fixtureItem(
					store.Slice{Name: "b", Files: []string{"pkg/auth/*.go", "internal/allowed/file.go"}, Tests: []string{"internal/allowed/file_test.go"}},
					store.Slice{Name: "a", Files: []string{"./pkg/auth/login.go", "pkg/auth/*.go"}, Tests: []string{"pkg/auth/login_test.go"}},
				),
				FilesChanged: []string{
					"other/c.go", "internal/outside/b.go", "other/g.go", "other/a.go",
					"./other/f.go", "/workspace/services/loom-core/internal/outside/a.go",
					"other/e.go", "other/d.go", "other/b.go",
				},
			},
		},
	}

	g := &Scope{}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			wantBytes, err := json.Marshal(struct {
				Outcome    Outcome
				Violations []string
			}{wantOutcome, wantViolations})
			if err != nil {
				t.Fatal(err)
			}
			for evaluation := 1; evaluation <= 3; evaluation++ {
				gotOutcome, err := g.Evaluate(context.Background(), fixture.in)
				if err != nil {
					t.Fatalf("evaluation %d: %v", evaluation, err)
				}
				gotBytes, err := json.Marshal(struct {
					Outcome    Outcome
					Violations []string
				}{gotOutcome, ScopeViolations(fixture.in)})
				if err != nil {
					t.Fatalf("evaluation %d marshal: %v", evaluation, err)
				}
				if string(gotBytes) != string(wantBytes) {
					t.Fatalf("evaluation %d differs:\n got: %s\nwant: %s", evaluation, gotBytes, wantBytes)
				}
			}
		})
	}
}

func TestScope_TestFilesAlwaysAllowed(t *testing.T) {
	g := &Scope{}
	in := StageInput{
		Item: fixtureItem(store.Slice{
			Name:  "core",
			Files: []string{"pkg/auth/login.go"},
		}),
		// Test file not in the slice's tests[] list — should still be
		// allowed because looksLikeTestFile catches it.
		FilesChanged: []string{"pkg/auth/login.go", "pkg/auth/edge_test.go"},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("test file should be allowed: %+v", out)
	}
}

func TestScope_DocsGuardrailFilesAlwaysAllowed(t *testing.T) {
	g := &Scope{}
	in := StageInput{
		Item: fixtureItem(store.Slice{
			Name:  "core",
			Files: []string{"pkg/mills/gates/health.go"},
		}),
		// CHANGELOG.md + a docs/ file are NOT in the slice's files, but the CI
		// docs guardrail mandates them for a code-facing MR (the implement
		// spawn adds a CHANGELOG entry), so the scope gate must allow them.
		// Includes the absolute spawn-style path the gate actually sees in prod.
		FilesChanged: []string{
			"pkg/mills/gates/health.go",
			"CHANGELOG.md",
			"docs/mills-infrastructure-health-gates.md",
			"/workspace/services/loom-core/CHANGELOG.md",
		},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("docs-guardrail files should be in scope: %+v", out)
	}
}

func TestScope_ChangelogFragmentAlwaysAllowed(t *testing.T) {
	// The implement spawn satisfies the docs guardrail by writing a
	// changelog.d/<slug>.<category>.md fragment, which is never in the slice's
	// files list — so the scope gate must allow it (repo-relative and the
	// absolute spawn-style path), exactly as it allows CHANGELOG.md.
	g := &Scope{}
	in := StageInput{
		Item: fixtureItem(store.Slice{
			Name:  "core",
			Files: []string{"pkg/mills/gates/health.go"},
		}),
		FilesChanged: []string{
			"pkg/mills/gates/health.go",
			"changelog.d/feat-thing.added.md",
			"/workspace/services/loom-core/changelog.d/feat-thing.added.md",
		},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("changelog.d fragment should be in scope: %+v", out)
	}
}

func TestScope_DocsCarveOutDoesNotMaskCodeViolation(t *testing.T) {
	// The carve-out is docs-only: a genuinely out-of-scope code file must
	// still fail even when an allowed CHANGELOG.md is also in the diff.
	g := &Scope{}
	in := StageInput{
		Item: fixtureItem(store.Slice{
			Name:  "core",
			Files: []string{"pkg/mills/gates/health.go"},
		}),
		FilesChanged: []string{"pkg/mills/gates/health.go", "CHANGELOG.md", "internal/billing/charge.go"},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if out.Pass {
		t.Errorf("expected fail for the out-of-scope code file: %+v", out)
	}
	if !strings.Contains(out.Reasons[0], "internal/billing/charge.go") {
		t.Errorf("the code file should be the violation, not CHANGELOG: %v", out.Reasons)
	}
}

func TestScope_AutoManagedDepFilesAlwaysAllowed(t *testing.T) {
	g := &Scope{}
	in := StageInput{
		Item: fixtureItem(store.Slice{
			Name:  "core",
			Files: []string{"pkg/mills/policy.go"},
		}),
		// go.sum/go.mod are toolchain side-effects of adding an import; the
		// slice never lists them, but they must not escalate. Includes the
		// absolute spawn-style path the gate sees in prod.
		FilesChanged: []string{
			"pkg/mills/policy.go",
			"go.sum",
			"go.mod",
			"/workspace/services/loom-core/go.sum",
		},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("auto-managed dependency files should be in scope: %+v", out)
	}
}

func TestScope_DepCarveOutDoesNotMaskCodeViolation(t *testing.T) {
	g := &Scope{}
	in := StageInput{
		Item: fixtureItem(store.Slice{
			Name:  "core",
			Files: []string{"pkg/mills/policy.go"},
		}),
		FilesChanged: []string{"pkg/mills/policy.go", "go.sum", "internal/other/thing.go"},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if out.Pass {
		t.Errorf("an out-of-scope code file must still fail alongside an allowed go.sum: %+v", out)
	}
	if !strings.Contains(out.Reasons[0], "internal/other/thing.go") {
		t.Errorf("the code file should be the violation, not go.sum: %v", out.Reasons)
	}
}

// TestScope_NoSlicesSkips: a slice-less, non-canary, non-bootstrapped item has
// no scope to enforce. That is not a defect in the diff, so the gate returns an
// advisory SKIP (Pass=true, Skip=true) carrying the drift reason — not a fail.
// The run proceeds instead of false-failing (live 2026-07-16); the skip is
// persisted as gate_outcomes.outcome='skip' and excluded from gate_pass_rate.
func TestScope_NoSlicesSkips(t *testing.T) {
	g := &Scope{}
	in := StageInput{
		Item:         fixtureItem(),
		FilesChanged: []string{"pkg/auth/login.go"},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("no-slices scope should skip (Pass=true), got fail: %+v", out)
	}
	if !out.Skip {
		t.Errorf("no-slices scope must set Skip, got %+v", out)
	}
	if out.Terminal {
		t.Errorf("a skip is not a terminal fail, got %+v", out)
	}
	if len(out.Reasons) == 0 || out.Reasons[0] != "backlog item has no slices; no scope to enforce" {
		t.Errorf("skip must carry the drift reason, got %+v", out.Reasons)
	}
}

// TestScope_EscalationShapedSlicelessItemsSkip pins the skip contract against
// the two real intake shapes that terminally dead-ended at post_implement_gate
// on 2026-07-16, hours before the fail→skip conversion merged:
//
//   - issue #338 (run PIPE-gl-47-334-…): a GitLab-issue import — the importer
//     has no file knowledge, so issueToBacklog never sets Slices and the
//     authored plan is slice-less too;
//   - issue #332 (run PIPE-psl-plan-council-document-degraded-mode-…): a
//     plan-slice-emitter item whose council plan slice declared no files, so
//     the emitted single-slice scope collapsed to nothing.
//
// Both must skip (advisory, non-terminal), never fail: the diff is not the
// defect, the item's missing decomposition is.
func TestScope_EscalationShapedSlicelessItemsSkip(t *testing.T) {
	iid := int64(334)
	cases := []struct {
		name string
		item *store.BacklogItem
	}{
		{
			name: "gitlab-importer item (issue #338 shape)",
			item: &store.BacklogItem{
				ID:             "gl-47-334",
				GitLabIssueIID: &iid,
				Title:          "Make operator-triggered Council runs durable",
				Labels:         []string{"bug", "mills-eligible"},
				State:          store.BacklogQueued,
				PlanID:         "plan-mills-gl-47-334",
				CreatedBy:      "mills:gitlab-importer",
			},
		},
		{
			name: "plan-slice-emitter item with file-less slice (issue #332 shape)",
			item: &store.BacklogItem{
				ID:        "psl-plan-council-document-degraded-mode-1",
				Title:     "Document degraded-mode expectations — incident-runbook",
				State:     store.BacklogQueued,
				PlanID:    "plan-council-document-degraded-mode",
				Slices:    []store.Slice{},
				CreatedBy: "mills:plan-slice-emitter",
			},
		},
	}
	g := &Scope{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := g.Evaluate(context.Background(), StageInput{
				Item:         tc.item,
				FilesChanged: []string{"pkg/mills/runner/council.go", "docs/mills-incident-runbook.md"},
			})
			if err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if !out.Pass || !out.Skip {
				t.Errorf("want advisory skip (Pass=true, Skip=true), got %+v", out)
			}
			if out.Terminal {
				t.Errorf("a slice-less item must never terminal-fail, got %+v", out)
			}
		})
	}
}

// TestScope_NoSlicesBootstrappedProjectPasses pins the bootstrapped-repo
// carve-out: a plan authored before its repo existed cannot declare file
// paths, so its items land slice-less by construction. Escalations #272–#278
// wedged the entire first bootstrapped project on this.
func TestScope_NoSlicesBootstrappedProjectPasses(t *testing.T) {
	g := &Scope{}
	in := StageInput{
		Item:                fixtureItem(),
		FilesChanged:        []string{"procmodel/model.py"},
		ProjectBootstrapped: true,
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("expected pass for slice-less item on bootstrapped project, got %+v", out)
	}
	if len(out.Reasons) == 0 {
		t.Errorf("bootstrapped pass should record a warning reason for audit, got %+v", out)
	}
}

// TestScope_SlicedItemStillEnforcedOnBootstrappedProject: the carve-out only
// relaxes the empty-allowlist case. Once slices with files exist, scope is
// enforced on a bootstrapped repo like anywhere else.
func TestScope_SlicedItemStillEnforcedOnBootstrappedProject(t *testing.T) {
	g := &Scope{}
	in := StageInput{
		Item: fixtureItem(store.Slice{
			Name:  "core",
			Files: []string{"procmodel/model.py"},
		}),
		FilesChanged:        []string{"procmodel/model.py", "unrelated/detour.py"},
		ProjectBootstrapped: true,
	}
	out, _ := g.Evaluate(context.Background(), in)
	if out.Pass {
		t.Errorf("expected fail for out-of-scope file on bootstrapped project with slices, got pass: %+v", out)
	}
	if out.Terminal {
		t.Errorf("an ordinary scope violation is retryable, not terminal: %+v", out)
	}
}

// TestScope_CanaryHeartbeatAllowedForCanaryItems pins the DEBT-073(c)
// carve-out: a CanaryLabel item editing the heartbeat fixture passes
// even though council-emitted slice lists omit the file (escalations
// #151/#163 fired on every canary run before this).
func TestScope_CanaryHeartbeatAllowedForCanaryItems(t *testing.T) {
	g := &Scope{}
	item := fixtureItem(store.Slice{
		Name:  "canary",
		Files: []string{"docs/changelog.md"},
	})
	item.Labels = []string{CanaryLabel}
	in := StageInput{
		Item:         item,
		FilesChanged: []string{"testdata/mills-canary/heartbeat.md"},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("canary heartbeat edit should be in scope for canary items: %+v", out)
	}
}

// TestScope_CanaryNoSlicesHeartbeatAllowed pins the 2026-06-23 fix: a
// CanaryLabel item carrying NO slices (plan_slice failed to persist one,
// PIPE-MILLS-CANARY-20260623-235142) that edits only the heartbeat fixture
// must PASS via the canary carve-out instead of failing the empty-slices
// check and escalating. Before the fix the empty-slices guard fired before
// the carve-out seeded canaryAllowedPaths.
func TestScope_CanaryNoSlicesHeartbeatAllowed(t *testing.T) {
	g := &Scope{}
	item := fixtureItem() // no slices, as a slice-less canary
	item.Labels = []string{CanaryLabel}
	in := StageInput{
		Item:         item,
		FilesChanged: []string{"testdata/mills-canary/heartbeat.md"},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("slice-less canary heartbeat edit should pass scope, got: %+v", out)
	}
}

// TestScope_CanaryHeartbeatStillFailsForNonCanaryItems guards the
// carve-out's blast radius: a real backlog item touching the canary
// fixture is still a scope violation.
func TestScope_TestdataFixturesAllowed(t *testing.T) {
	g := &Scope{}
	// Golden files and other Go test fixtures live under testdata/, are
	// never listed in slice files, and are ignored by the toolchain — the
	// gate must treat them as test collateral (2026-07-08 regression:
	// escalation_failure_classification.golden escalated a real run).
	// This deliberately supersedes the earlier expectation that a
	// non-canary item touching testdata/mills-canary/heartbeat.md fails:
	// testdata edits are inert, so blocking them costs more (a false
	// escalation) than it protects.
	in := StageInput{
		Item: fixtureItem(store.Slice{
			Name:  "core",
			Files: []string{"internal/contracts/escalation.go"},
		}),
		FilesChanged: []string{
			"internal/contracts/testdata/escalation_failure_classification.golden",
			"/workspace/services/loom-core/testdata/mills-canary/heartbeat.md",
		},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("testdata fixtures should be allowed as test collateral: %+v", out)
	}
}

// TestScope_AbsoluteSpawnPathsMatchRepoRelativeAllow guards the Mills A2
// canary 2026-06-19 regression: spawn-driven stages (k8s pod / harvester-vm)
// report ABSOLUTE changed paths under the in-pod/VM workdir, but slice.files +
// canaryAllowedPaths are repo-relative and run.WorktreePath is empty for
// spawns. The gate must match an absolute changed path against a repo-relative
// allowed path on a segment boundary, or every spawn implement false-fails.
func TestScope_AbsoluteSpawnPathsMatchRepoRelativeAllow(t *testing.T) {
	g := &Scope{}
	// (a) canary fixture, absolute path, canary item → allowed via allowlist.
	canary := fixtureItem(store.Slice{Name: "canary", Files: []string{"docs/changelog.md"}})
	canary.Labels = []string{CanaryLabel}
	out, _ := g.Evaluate(context.Background(), StageInput{
		Item:         canary,
		FilesChanged: []string{"/workspace/services/loom-core/testdata/mills-canary/heartbeat.md"},
	})
	if !out.Pass {
		t.Errorf("absolute canary heartbeat path should pass for canary item: %+v", out)
	}
	// (b) slice file, absolute path → allowed via slice.files suffix match.
	out, _ = g.Evaluate(context.Background(), StageInput{
		Item:         fixtureItem(store.Slice{Name: "core", Files: []string{"pkg/auth/login.go"}}),
		FilesChanged: []string{"/workspace/services/loom-core/pkg/auth/login.go"},
	})
	if !out.Pass {
		t.Errorf("absolute in-slice path should pass: %+v", out)
	}
	// (c) absolute path NOT under any allowed repo-relative path → still fails.
	out, _ = g.Evaluate(context.Background(), StageInput{
		Item:         fixtureItem(store.Slice{Name: "core", Files: []string{"pkg/auth/login.go"}}),
		FilesChanged: []string{"/workspace/services/loom-core/internal/secret/keys.go"},
	})
	if out.Pass {
		t.Errorf("absolute out-of-scope path should still fail: %+v", out)
	}
}

func TestScope_GlobSlicePatternsMatchAbsoluteSpawnPaths(t *testing.T) {
	g := &Scope{}
	// A slice declaring a glob pattern + a spawn-reported ABSOLUTE path: the
	// repo-relative glob (pkg/auth/*.go) can't match the absolute path via
	// filepath.Match directly (it never crosses "/"), so without the suffix
	// fallback this legitimate change false-fails the scope gate.
	item := fixtureItem(store.Slice{Name: "core", Files: []string{"pkg/auth/*.go"}})
	out, _ := g.Evaluate(context.Background(), StageInput{
		Item:         item,
		FilesChanged: []string{"/workspace/services/loom-core/pkg/auth/login.go"},
	})
	if !out.Pass {
		t.Errorf("absolute path matching a repo-relative glob should pass: %+v", out)
	}
	// A glob still must not wave through an out-of-scope path.
	out, _ = g.Evaluate(context.Background(), StageInput{
		Item:         item,
		FilesChanged: []string{"/workspace/services/loom-core/internal/secret/keys.go"},
	})
	if out.Pass {
		t.Errorf("absolute path outside the glob scope should still fail: %+v", out)
	}
}

// TestScope_DirectoryEnvelopeAllowsSiblingBasenames guards the 2026-07-08
// regression that escalated 8 of the day's 19 runs: the council decomposes
// work into slice `files` BEFORE the files exist, and its guessed basenames
// are near-misses of what implement actually writes (slice said
// pkg/mills/pipeline/escalation.go; the agent — correctly — edited
// escalate.go). The enforceable envelope is the parent directory, so a
// changed file in the same directory as a slice-declared file is in scope.
func TestScope_DirectoryEnvelopeAllowsSiblingBasenames(t *testing.T) {
	g := &Scope{}
	// Real slice + changed paths from PIPE-psl-plan-council-thread-external-
	// dependency-incident-metadata-into-mills-esca-1-1783512456.
	item := fixtureItem(store.Slice{
		Name: "emit-classified-escalation-fields",
		Files: []string{
			"pkg/mills/pipeline/escalation.go",
			"pkg/mills/pipeline/escalation_test.go",
			"pkg/mills/audit/audit.go",
		},
	})
	out, _ := g.Evaluate(context.Background(), StageInput{
		Item: item,
		FilesChanged: []string{
			"/workspace/services/loom-core/pkg/mills/audit/types.go",
			"/workspace/services/loom-core/pkg/mills/pipeline/escalate.go",
		},
	})
	if !out.Pass {
		t.Errorf("sibling files in slice-declared directories should pass: %+v", out)
	}
	// Repo-relative form matches too.
	out, _ = g.Evaluate(context.Background(), StageInput{
		Item:         item,
		FilesChanged: []string{"pkg/mills/pipeline/escalate.go"},
	})
	if !out.Pass {
		t.Errorf("repo-relative sibling should pass: %+v", out)
	}
}

// TestScope_DirectoryEnvelopeDoesNotCrossDirectories pins the envelope's
// bounds: declared directories and their descendants open up — parents and
// unrelated packages still violate.
func TestScope_DirectoryEnvelopeDoesNotCrossDirectories(t *testing.T) {
	g := &Scope{}
	item := fixtureItem(store.Slice{
		Name:  "core",
		Files: []string{"pkg/auth/login.go"},
	})
	for _, changed := range []string{
		"pkg/x.go",                    // parent directory
		"pkg/authentication/login.go", // shared string prefix, not a descendant
		"internal/billing/pay.go",     // unrelated package
		"/workspace/services/loom-core/internal/billing/pay.go", // absolute form
	} {
		out, _ := g.Evaluate(context.Background(), StageInput{
			Item:         item,
			FilesChanged: []string{changed},
		})
		if out.Pass {
			t.Errorf("%s should stay out of scope: %+v", changed, out)
		}
	}
}

func TestScope_WindowsPathsUseCanonicalDirectoryEnvelope(t *testing.T) {
	g := &Scope{}
	item := fixtureItem(store.Slice{
		Name:  "core",
		Files: []string{`pkg\auth\login.go`},
	})

	out, _ := g.Evaluate(context.Background(), StageInput{
		Item:         item,
		FilesChanged: []string{`C:\workspace\services\loom-core\pkg\auth\handlers\session.go`},
	})
	if !out.Pass {
		t.Errorf("Windows descendant path should pass the canonical directory envelope: %+v", out)
	}

	out, _ = g.Evaluate(context.Background(), StageInput{
		Item:         item,
		FilesChanged: []string{`C:\workspace\services\loom-core\pkg\authentication\session.go`},
	})
	if out.Pass {
		t.Errorf("Windows path sharing only a string prefix must remain out of scope: %+v", out)
	}
}

// TestScope_DirectoryEnvelopeIncludesDescendants pins the 2026-07-08
// follow-up regression (PIPE-…emit-classification…-1783538288): the slice
// declared pkg/mills/store/store.go and the agent added the REQUIRED
// numbered migration pkg/mills/store/migrations/010_….sql — a file it
// could neither omit nor relocate. Descendants of a declared directory
// are in scope, in relative and absolute form.
func TestScope_DirectoryEnvelopeIncludesDescendants(t *testing.T) {
	g := &Scope{}
	item := fixtureItem(store.Slice{
		Name:  "escalation-metadata",
		Files: []string{"pkg/mills/store/store.go"},
	})
	for _, changed := range []string{
		"pkg/mills/store/migrations/010_pipeline_escalation_metadata.sql",
		"/workspace/services/loom-core/pkg/mills/store/migrations/010_pipeline_escalation_metadata.sql",
	} {
		out, _ := g.Evaluate(context.Background(), StageInput{
			Item:         item,
			FilesChanged: []string{changed},
		})
		if !out.Pass {
			t.Errorf("%s should be in scope as a descendant: %+v", changed, out)
		}
	}
}

// TestScope_TopLevelSliceFileDoesNotOpenRepoRoot: a slice naming a repo-root
// file must not turn the directory envelope into an allow-anything-at-root
// grant.
func TestScope_TopLevelSliceFileDoesNotOpenRepoRoot(t *testing.T) {
	g := &Scope{}
	item := fixtureItem(store.Slice{Name: "core", Files: []string{"Makefile"}})
	out, _ := g.Evaluate(context.Background(), StageInput{
		Item:         item,
		FilesChanged: []string{"main.go"},
	})
	if out.Pass {
		t.Errorf("root sibling of a root slice file should stay out of scope: %+v", out)
	}
}

// TestScope_DuplicateChangedPathsRenderedOnce: spawn telemetry can report the
// same path twice (created + modified); the persisted reason must count and
// list it once (every 2026-07-08 scope escalation doubled each violation).
func TestScope_DuplicateChangedPathsRenderedOnce(t *testing.T) {
	g := &Scope{}
	item := fixtureItem(store.Slice{Name: "core", Files: []string{"pkg/auth/login.go"}})
	out, _ := g.Evaluate(context.Background(), StageInput{
		Item: item,
		FilesChanged: []string{
			"internal/billing/pay.go",
			"internal/billing/pay.go",
		},
	})
	if out.Pass {
		t.Fatalf("expected fail, got pass: %+v", out)
	}
	if len(out.Reasons) != 1 {
		t.Fatalf("duplicate changed path should be counted once: %v", out.Reasons)
	}
	if out.Reasons[0] != "[scope.outside_declared_scope] file outside slice scope: internal/billing/pay.go" {
		t.Errorf("duplicate changed path should be rendered once: %v", out.Reasons)
	}
}

// ---------- PathPolicy ----------

func TestPathPolicy_NoTouchPasses(t *testing.T) {
	g := &PathPolicy{}
	in := StageInput{
		Policy:       fixturePolicy(t),
		FilesChanged: []string{"internal/hud/spawn.go", "pkg/skills/fileops.go"},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("expected pass, got %+v", out)
	}
}

func TestPathPolicy_UndeclaredTouchFails(t *testing.T) {
	g := &PathPolicy{}
	in := StageInput{
		Policy: fixturePolicy(t),
		Item:   fixtureItem(),
		FilesChanged: []string{
			"platform/gitops/k3s/devbox/code-server.yaml",
		},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if out.Pass {
		t.Errorf("expected fail for undeclared protected touch, got %+v", out)
	}
	if !strings.Contains(out.Reasons[0], "platform/gitops") {
		t.Errorf("reason should name the path: %v", out.Reasons)
	}
}

func TestPathPolicy_DeclaredTouchPasses(t *testing.T) {
	g := &PathPolicy{}
	item := fixtureItem()
	item.Policy.ProtectedPathsTouched = []string{"platform/gitops/k3s/devbox/code-server.yaml"}
	in := StageInput{
		Policy:       fixturePolicy(t),
		Item:         item,
		FilesChanged: []string{"platform/gitops/k3s/devbox/code-server.yaml"},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("declared touch should pass, got %+v", out)
	}
}

func TestPathPolicy_DeclaredTouchPassesForAbsoluteSpawnPaths(t *testing.T) {
	g := &PathPolicy{}
	// The plan-slice emitter pre-declares REPO-RELATIVE protected paths, but a
	// spawn stage reports ABSOLUTE in-pod paths. The `**/*auth*.go` protected
	// glob matches both, so without absolute-suffix subtraction the gate would
	// re-fire on a plan-declared touch. (`auth` is used deliberately: anchored
	// patterns like platform/gitops/** never match an absolute path at all.)
	item := fixtureItem()
	item.Policy.ProtectedPathsTouched = []string{"pkg/x/auth.go"}
	in := StageInput{
		Policy:       fixturePolicy(t),
		Item:         item,
		FilesChanged: []string{"/workspace/services/loom-core/pkg/x/auth.go"},
	}
	out, _ := g.Evaluate(context.Background(), in)
	if !out.Pass {
		t.Errorf("absolute spawn path matching a repo-relative pre-declaration should pass, got %+v", out)
	}
	// A DIFFERENT protected absolute path the slice never declared still fails.
	in.FilesChanged = []string{"/workspace/services/loom-core/pkg/y/auth.go"}
	out, _ = g.Evaluate(context.Background(), in)
	if out.Pass {
		t.Errorf("undeclared protected absolute touch should still fail, got %+v", out)
	}
}

func TestPathPolicy_MissingPolicyFails(t *testing.T) {
	g := &PathPolicy{}
	out, _ := g.Evaluate(context.Background(), StageInput{FilesChanged: []string{"x"}})
	if out.Pass {
		t.Errorf("expected fail with no policy, got pass")
	}
}

// ---------- SecretScan ----------

func TestSecretScan_CleanDiffPasses(t *testing.T) {
	g := &SecretScan{}
	out, _ := g.Evaluate(context.Background(), StageInput{
		DiffPatch: []byte("+func login() {}\n-func old() {}\n"),
	})
	if !out.Pass {
		t.Errorf("expected pass, got %+v", out)
	}
}

func TestSecretScan_CatchesAWSKey(t *testing.T) {
	g := &SecretScan{}
	out, _ := g.Evaluate(context.Background(), StageInput{
		DiffPatch: []byte("+const key = \"AKIAIOSFODNN7EXAMPLE\"\n"),
	})
	if out.Pass {
		t.Errorf("expected fail, got %+v", out)
	}
	if !strings.Contains(out.Reasons[0], "AWS access key") {
		t.Errorf("reason should name the matched pattern: %v", out.Reasons)
	}
}

func TestSecretScan_CatchesAnthropicKey(t *testing.T) {
	g := &SecretScan{}
	out, _ := g.Evaluate(context.Background(), StageInput{
		DiffPatch: []byte("+ANTHROPIC_API_KEY=sk-ant-abcdefghijklmnopqrstuvwxyz123\n"),
	})
	if out.Pass {
		t.Errorf("expected fail, got %+v", out)
	}
}

func TestSecretScan_DeletionsIgnored(t *testing.T) {
	g := &SecretScan{}
	out, _ := g.Evaluate(context.Background(), StageInput{
		// `-` line: deletion of an existing key. Already a fix; don't
		// fail the diff that's removing it.
		DiffPatch: []byte("-const key = \"AKIAIOSFODNN7EXAMPLE\"\n"),
	})
	if !out.Pass {
		t.Errorf("deletion should pass, got %+v", out)
	}
}

func TestSecretScan_FilenameNotMatched(t *testing.T) {
	g := &SecretScan{}
	// `+++` header lines should be skipped so a filename that happens to
	// contain a JWT-shape doesn't trip the gate.
	out, _ := g.Evaluate(context.Background(), StageInput{
		DiffPatch: []byte("+++ b/eyJlbmNvZGVkIjoidGVzdCJ9.eyJjbGFpbSI6InZhbHVlIn0.signature/foo.go\n+func ok() {}\n"),
	})
	if !out.Pass {
		t.Errorf("filename should not match: %+v", out)
	}
}

// ---------- CommitFormat ----------

func TestCommitFormat_NoCommitsPasses(t *testing.T) {
	g := &CommitFormat{}
	out, _ := g.Evaluate(context.Background(), StageInput{})
	if !out.Pass {
		t.Errorf("expected pass with no commits, got %+v", out)
	}
}

func TestCommitFormat_ConventionalPasses(t *testing.T) {
	g := &CommitFormat{}
	out, _ := g.Evaluate(context.Background(), StageInput{
		CommitMessages: []string{
			"feat(mills): add diff_size gate",
			"fix: handle nil item",
			"refactor!: drop legacy path",
		},
	})
	if !out.Pass {
		t.Errorf("expected pass for conventional commits, got %+v", out)
	}
}

func TestCommitFormat_BadShapeFails(t *testing.T) {
	g := &CommitFormat{}
	out, _ := g.Evaluate(context.Background(), StageInput{
		CommitMessages: []string{
			"updated some stuff",
		},
	})
	if out.Pass {
		t.Errorf("expected fail, got %+v", out)
	}
}

func TestCommitFormat_UnknownTypeFails(t *testing.T) {
	g := &CommitFormat{}
	out, _ := g.Evaluate(context.Background(), StageInput{
		CommitMessages: []string{"wip: half-done"},
	})
	if out.Pass {
		t.Errorf("expected fail for unknown type, got %+v", out)
	}
	if !strings.Contains(out.Reasons[0], "wip") {
		t.Errorf("reason should name unknown type: %v", out.Reasons)
	}
}

func TestCommitFormat_LongSubjectFails(t *testing.T) {
	g := &CommitFormat{MaxSubjectLen: 30}
	out, _ := g.Evaluate(context.Background(), StageInput{
		CommitMessages: []string{
			"feat(mills): a really long subject that exceeds the cap",
		},
	})
	if out.Pass {
		t.Errorf("expected fail for long subject, got %+v", out)
	}
}

// ---------- Registry ----------

func TestDefault_HasAllCoreGates(t *testing.T) {
	r := Default()
	got := r.Names()
	want := []string{"branch_pushed", "commit_format", "diff_size", "docs_guardrail", "fabricated_slice", "nonempty_diff", "path_policy", "scope", "secret_scan"}
	if len(got) != len(want) {
		t.Fatalf("default registry: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("registry[%d] got %q want %q", i, got[i], want[i])
		}
	}
}

func TestRegistry_DuplicatePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	r := NewRegistry()
	r.Register(&DiffSize{})
	r.Register(&DiffSize{})
}

func TestRegistry_GetUnknown(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Get("nope"); err == nil {
		t.Fatal("expected ErrUnknownGate")
	}
}

func TestEvaluateAll_AggregatesPass(t *testing.T) {
	r := Default()
	in := StageInput{
		Policy:       fixturePolicy(t),
		Item:         fixtureItem(store.Slice{Name: "x", Files: []string{"a.go"}}),
		FilesChanged: []string{"a.go"},
		LinesAdded:   10,
	}
	outcomes, allPass, err := r.EvaluateAll(context.Background(),
		[]string{"diff_size", "scope", "path_policy", "secret_scan", "commit_format"}, in)
	if err != nil {
		t.Fatalf("evaluate-all: %v", err)
	}
	if !allPass {
		t.Errorf("expected all pass; outcomes=%+v", outcomes)
	}
	if len(outcomes) != 5 {
		t.Errorf("expected 5 outcomes, got %d", len(outcomes))
	}
}

func TestEvaluateAll_UnknownGateFails(t *testing.T) {
	r := Default()
	outcomes, allPass, err := r.EvaluateAll(context.Background(),
		[]string{"diff_size", "nope"}, StageInput{LinesAdded: 1})
	if err != nil {
		t.Fatalf("evaluate-all should not error on unknown gate; got %v", err)
	}
	if allPass {
		t.Errorf("unknown gate should cause aggregate fail")
	}
	if len(outcomes) != 2 || outcomes[1].Outcome.Pass {
		t.Errorf("expected per-gate fail outcome for unknown gate; got %+v", outcomes)
	}
}

func TestRequestedGateVerdictsAreDeterministicOnReplay(t *testing.T) {
	item := fixtureItem(store.Slice{Name: "gate", Files: []string{"pkg/a.go"}})
	item.SpecDoc = "docs/spec.md"
	item.SpecAnchor = "acceptance"
	input := StageInput{
		RunID: "run-1", Item: item, FilesChanged: []string{"pkg/a.go"},
		LinesAdded: 1, DiffPatch: []byte("diff --git a/pkg/a.go b/pkg/a.go\n+x\n"),
		CommitMessages: []string{"feat: add a"}, TestsPassed: true,
	}

	registry := NewRegistry()
	registry.Register(NewSpecConformanceGate(&FakeRubricJudge{Default: RubricVerdict{Score: 0.9, Model: "fake"}}))
	registry.Register(NewPRSelfReviewGate(&FakeRubricJudge{Default: RubricVerdict{Score: 0.9, Model: "fake"}}))
	registry.Register(&Scope{})
	registry.Register(&NonEmptyDiff{})
	harness := &telemetry.GateDeterminismHarness{}
	registry.SetTelemetrySink(harness)

	names := []string{"spec_conformance", "scope", "pr_self_review", "nonempty_diff"}
	for replay := 0; replay < 4; replay++ {
		input.RunID = "run-" + string(rune('1'+replay))
		outcomes, passed, err := registry.EvaluateAll(context.Background(), names, input)
		if err != nil || !passed || len(outcomes) != len(names) {
			t.Fatalf("replay %d: outcomes=%+v passed=%v err=%v", replay, outcomes, passed, err)
		}
	}

	records := harness.Records()
	if len(records) != 4*len(names) {
		t.Fatalf("records=%d, want %d", len(records), 4*len(names))
	}
	first := make(map[string]telemetry.GateEvaluation)
	wantReason := map[string]string{
		"spec_conformance": "[" + SpecConformanceReasonPassed + "]",
		"scope":            "[" + ScopeReasonInScope + "]",
		"pr_self_review":   "[" + PRSelfReviewReasonPassed + "]",
		"nonempty_diff":    "[" + NonEmptyDiffReasonPresent + "]",
	}
	for _, record := range records {
		if record.InputDigest == "" {
			t.Fatalf("empty input digest: %+v", record)
		}
		if record.Reason != wantReason[record.GateID] {
			t.Errorf("%s reason = %q, want exact code %q", record.GateID, record.Reason, wantReason[record.GateID])
		}
		if prior, ok := first[record.GateID]; ok {
			if record.InputDigest != prior.InputDigest || record.Verdict != prior.Verdict {
				t.Errorf("non-deterministic replay for %s: first=%+v later=%+v", record.GateID, prior, record)
			}
		} else {
			first[record.GateID] = record
		}
	}
	if flakes := harness.Flakes(); len(flakes) != 0 {
		t.Fatalf("determinism harness reported flakes: %+v", flakes)
	}
}

func TestRequestedGateFailureReasonCodesAreStableOnReplay(t *testing.T) {
	item := fixtureItem(store.Slice{Name: "gate", Files: []string{"pkg/allowed.go"}})
	fixtures := []struct {
		name string
		gate Gate
		in   StageInput
		code string
	}{
		{"spec_conformance", NewSpecConformanceGate(&FakeRubricJudge{Default: RubricVerdict{Score: 0.2}}), StageInput{}, SpecConformanceReasonBelowScore},
		{"scope", &Scope{}, StageInput{Item: item, FilesChanged: []string{"internal/outside.go"}}, ScopeReasonOutside},
		{"pr_self_review", NewPRSelfReviewGate(&FakeRubricJudge{Default: RubricVerdict{Score: 0.2}}), StageInput{}, PRSelfReviewReasonBelowScore},
		{"nonempty_diff", &NonEmptyDiff{}, StageInput{}, NonEmptyDiffReasonEmpty},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			want := "[" + fixture.code + "]"
			for replay := 0; replay < 8; replay++ {
				out, err := fixture.gate.Evaluate(context.Background(), fixture.in)
				if err != nil || out.Pass {
					t.Fatalf("replay %d: outcome=%+v err=%v", replay, out, err)
				}
				if got := strings.Join(out.Reasons, " "); !strings.Contains(got, want) {
					t.Fatalf("replay %d: reasons=%q, want exact code %q", replay, got, want)
				}
			}
		})
	}
}

func TestGateVerdictTelemetryCoversFailSkipAndEvaluationError(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&NonEmptyDiff{})
	registry.Register(fixedOutcomeGate{name: "not_applicable", outcome: skip("no basis")})
	registry.Register(errorTelemetryGate{name: "evaluation_error"})
	harness := &telemetry.GateDeterminismHarness{}
	registry.SetTelemetrySink(harness)

	_, _, err := registry.EvaluateAll(context.Background(),
		[]string{"nonempty_diff", "not_applicable", "evaluation_error"}, StageInput{})
	if err == nil {
		t.Fatal("expected evaluation error")
	}
	records := harness.Records()
	if len(records) != 3 {
		t.Fatalf("records=%+v, want three", records)
	}
	want := []telemetry.GateVerdict{
		telemetry.GateVerdictFail, telemetry.GateVerdictSkip, telemetry.GateVerdictError,
	}
	for i := range want {
		if records[i].Verdict != want[i] || records[i].InputDigest == "" {
			t.Errorf("record[%d]=%+v, want verdict %q and input hash", i, records[i], want[i])
		}
	}
}

func TestRequestedGateDigestsExcludeIrrelevantInputs(t *testing.T) {
	base := StageInput{Item: fixtureItem(), FilesChanged: []string{"a.go"}, DiffPatch: []byte("+x\n")}
	changed := base
	changed.Policy = fixturePolicy(t)
	changed.GitCaptureReason = "diagnostic changed"
	changed.ProjectBootstrapped = true
	for _, gateID := range []string{"spec_conformance", "pr_self_review", "nonempty_diff"} {
		if got, want := inputDigestForGate(gateID, changed), inputDigestForGate(gateID, base); got != want {
			t.Errorf("%s digest included irrelevant input: got %q want %q", gateID, got, want)
		}
	}
	changed.DiffPatch = []byte("+different\n")
	if inputDigestForGate("pr_self_review", changed) == inputDigestForGate("pr_self_review", base) {
		t.Fatal("pr_self_review digest ignored verdict-relevant diff")
	}
}

type fixedOutcomeGate struct {
	name    string
	outcome Outcome
}

func (g fixedOutcomeGate) Name() string { return g.name }
func (g fixedOutcomeGate) Evaluate(context.Context, StageInput) (Outcome, error) {
	return g.outcome, nil
}
