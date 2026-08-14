package gates

import (
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// Production fixtures. Both are REAL scope escalations from 2026-07-26, the
// afternoon whose 24h KPI read 83% escalation / 17% auto-merge. They are the
// admissibility rule's acceptance criteria: if either stops being admitted, the
// amendment has stopped covering the case it was written for.
var (
	// tokenSweepSlices: a design-token sweep over the Mills HUD components. The
	// author declared only components/mills; the sweep necessarily restyles the
	// shared PanelShell/EmptyState it renders through. Human resolution was
	// "widen scope, requeue" → merged !1249.
	tokenSweepSlices = []store.Slice{{
		Name: "token-sweep",
		Files: []string{
			"internal/hud/frontend/src/lib/components/mills/MillsPipelineDrawer.svelte",
			"internal/hud/frontend/src/lib/components/mills/MillsBacklogPanel.svelte",
		},
	}}
	tokenSweepViolations = []string{
		"internal/hud/frontend/src/lib/components/shared/EmptyState.svelte",
		"internal/hud/frontend/src/lib/components/shared/PanelShell.svelte",
	}

	// stopLeverSlices: an operator stop-lever feature. Declared the operator
	// cmd, the pipeline package, and the Mills HUD components; the
	// implementation necessarily reached the sibling store DAO, the sibling
	// spawn client, and the sibling HUD store. Human resolution: widen, requeue.
	stopLeverSlices = []store.Slice{{
		Name: "stop-lever",
		Files: []string{
			"cmd/loom-mills-operator/main.go",
			"pkg/mills/pipeline/runner.go",
			"internal/hud/frontend/src/lib/components/mills/MillsStopLever.svelte",
		},
	}}
	stopLeverViolations = []string{
		"pkg/mills/store/dao_pipeline.go",
		"pkg/mills/clients/spawn.go",
		"internal/hud/frontend/src/lib/stores/mills.svelte.ts",
	}
)

func itemWith(slices []store.Slice) *store.BacklogItem {
	return &store.BacklogItem{ID: "BL-SCOPE-1", Title: "scope fixture", Slices: slices}
}

func boolPtr(b bool) *bool { return &b }

func TestEvaluateScopeAmendment(t *testing.T) {
	defaultProtected := mills.Default().Pipeline.ProtectedPaths

	cases := []struct {
		name         string
		slices       []store.Slice
		violations   []string
		pol          mills.ScopeAmendmentPolicy
		protected    []string
		wantAdmitted bool
		// wantRules is the expected per-file rule, in violation order. Empty
		// skips the per-file assertion.
		wantRules []string
		// wantAncestors, when non-empty, pins the shared ancestor that admitted
		// each file — the value the operator sees in the amended gate reason.
		wantAncestors []string
	}{
		{
			// PRODUCTION FIXTURE 1 (token-sweep). Both violations sit in a
			// sibling directory under …/lib/components, six segments deep —
			// far past the depth-2 floor.
			name:          "token-sweep sibling components admitted",
			slices:        tokenSweepSlices,
			violations:    tokenSweepViolations,
			wantAdmitted:  true,
			wantRules:     []string{AmendRuleSharedAncestor, AmendRuleSharedAncestor},
			wantAncestors: []string{"internal/hud/frontend/src/lib/components", "internal/hud/frontend/src/lib/components"},
		},
		{
			// PRODUCTION FIXTURE 2 (stop-lever). Two violations share
			// "pkg/mills" (depth 2, the exact floor), the third shares
			// "internal/hud/frontend/src/lib" with the declared component.
			name:         "stop-lever sibling packages admitted",
			slices:       stopLeverSlices,
			violations:   stopLeverViolations,
			wantAdmitted: true,
			wantRules:    []string{AmendRuleSharedAncestor, AmendRuleSharedAncestor, AmendRuleSharedAncestor},
			wantAncestors: []string{
				"pkg/mills", "pkg/mills", "internal/hud/frontend/src/lib",
			},
		},
		{
			// Absolute in-pod spawn paths must read exactly like their
			// repo-relative form; the k8s substrate reports these on every run.
			name:   "absolute spawn paths admitted like their relative form",
			slices: stopLeverSlices,
			violations: []string{
				"/workspace/services/loom-core/pkg/mills/store/dao_pipeline.go",
				"/workspace/services/loom-core/pkg/mills/clients/spawn.go",
			},
			wantAdmitted:  true,
			wantRules:     []string{AmendRuleSharedAncestor, AmendRuleSharedAncestor},
			wantAncestors: []string{"pkg/mills", "pkg/mills"},
		},
		{
			// A repo-root file has no directory ancestor to share. The gate's
			// own docs/dep-manifest carve-outs cover the legitimate root files;
			// the amendment must never widen an envelope to the root.
			name:         "repo-root file refused",
			slices:       stopLeverSlices,
			violations:   []string{"Makefile"},
			wantAdmitted: false,
			wantRules:    []string{AmendRuleNoSharedAncestor},
		},
		{
			// The gate's reason for existing: an unrelated-tree detour. Tested
			// with NO protected paths configured so the refusal is provably the
			// ancestor rule, not the sensitive-path shortcut.
			name:         "platform/gitops reach refused when nothing declared there",
			slices:       stopLeverSlices,
			violations:   []string{"platform/gitops/k3s/mills/configmap-policy.yaml"},
			protected:    nil,
			wantAdmitted: false,
			wantRules:    []string{AmendRuleNoSharedAncestor},
		},
		{
			// Sharing only the top segment ("pkg") is not a sibling reach — it
			// is a different module. Depth 1 < the depth-2 floor.
			name:         "depth-1 share refused",
			slices:       []store.Slice{{Name: "s", Files: []string{"pkg/mills/pipeline/runner.go"}}},
			violations:   []string{"pkg/journalengine/engine.go"},
			wantAdmitted: false,
			wantRules:    []string{AmendRuleNoSharedAncestor},
		},
		{
			// Defense in depth: path_policy would fail this anyway, but scope
			// must never be the mechanism that widens an item onto a protected
			// path. Note the FIRST declared file is inside platform/gitops, so
			// the ancestor rule alone WOULD have admitted it.
			name: "protected path refused even with a shared ancestor",
			slices: []store.Slice{{Name: "s", Files: []string{
				"platform/gitops/k3s/mills/deployment.yaml",
			}}},
			violations:   []string{"platform/gitops/k3s/mills/configmap-policy.yaml"},
			protected:    defaultProtected,
			wantAdmitted: false,
			wantRules:    []string{AmendRuleSensitivePath},
		},
		{
			// The absolute spawn form of a protected path must refuse too: the
			// repo-relative glob "platform/gitops/**" cannot match
			// /workspace/services/loom-core/platform/gitops/... on its own.
			name: "absolute protected path refused",
			slices: []store.Slice{{Name: "s", Files: []string{
				"platform/gitops/k3s/mills/deployment.yaml",
			}}},
			violations:   []string{"/workspace/services/loom-core/platform/gitops/k3s/mills/configmap-policy.yaml"},
			protected:    defaultProtected,
			wantAdmitted: false,
			wantRules:    []string{AmendRuleSensitivePath},
		},
		{
			// Past the cap this is a decomposition failure, not a forgotten
			// sibling: refuse as a whole even though every file is individually
			// admissible.
			name:   "max_files exceeded refuses the whole decision",
			slices: []store.Slice{{Name: "s", Files: []string{"pkg/mills/pipeline/runner.go"}}},
			violations: []string{
				"pkg/mills/store/a.go", "pkg/mills/store/b.go", "pkg/mills/store/c.go",
				"pkg/mills/store/d.go", "pkg/mills/store/e.go", "pkg/mills/store/f.go",
				"pkg/mills/store/g.go",
			},
			wantAdmitted: false,
		},
		{
			// Exactly at the cap still amends — the boundary is inclusive.
			name:   "max_files boundary admits",
			slices: []store.Slice{{Name: "s", Files: []string{"pkg/mills/pipeline/runner.go"}}},
			violations: []string{
				"pkg/mills/store/a.go", "pkg/mills/store/b.go", "pkg/mills/store/c.go",
				"pkg/mills/store/d.go", "pkg/mills/store/e.go", "pkg/mills/store/f.go",
			},
			wantAdmitted: true,
		},
		{
			// A slice-less item has no anchor. Scope.Evaluate resolves these to
			// an advisory skip long before the runner reaches the amendment;
			// this is the belt to that suspenders.
			name:         "empty declared scope refused",
			slices:       nil,
			violations:   []string{"pkg/mills/store/dao_pipeline.go"},
			wantAdmitted: false,
			wantRules:    []string{AmendRuleNoDeclaredScope},
		},
		{
			// A slice declaring only globs pins no directory to anchor on.
			name:         "glob-only declared scope refused",
			slices:       []store.Slice{{Name: "s", Files: []string{"pkg/**/*.go"}}},
			violations:   []string{"internal/hud/api.go"},
			wantAdmitted: false,
			wantRules:    []string{AmendRuleNoDeclaredScope},
		},
		{
			name:         "policy disabled refuses everything",
			slices:       tokenSweepSlices,
			violations:   tokenSweepViolations,
			pol:          mills.ScopeAmendmentPolicy{Enabled: boolPtr(false)},
			wantAdmitted: false,
			wantRules:    []string{AmendRulePolicyDisabled, AmendRulePolicyDisabled},
		},
		{
			// A raised floor must actually bite: at depth 7 the token-sweep
			// pair (which shares 6 segments) no longer clears.
			name:         "raised ancestor_depth refuses a previously admitted reach",
			slices:       tokenSweepSlices,
			violations:   tokenSweepViolations,
			pol:          mills.ScopeAmendmentPolicy{AncestorDepth: 7},
			wantAdmitted: false,
			wantRules:    []string{AmendRuleNoSharedAncestor, AmendRuleNoSharedAncestor},
		},
		{
			// One inadmissible file poisons the whole decision: a diff that is
			// half detour is a detour.
			name:         "mixed admissibility refuses as a whole",
			slices:       stopLeverSlices,
			violations:   []string{"pkg/mills/store/dao_pipeline.go", "libs/game/scripts/main.gd"},
			wantAdmitted: false,
			wantRules:    []string{AmendRuleSharedAncestor, AmendRuleNoSharedAncestor},
		},
		{
			// Declared TESTS anchor too: buildAllowedSet already treats them as
			// part of the envelope, so the amendment must measure against the
			// same envelope the gate enforced.
			name:         "declared tests anchor the ancestor",
			slices:       []store.Slice{{Name: "s", Tests: []string{"pkg/mills/pipeline/runner_test.go"}}},
			violations:   []string{"pkg/mills/store/dao_pipeline.go"},
			wantAdmitted: true,
			wantRules:    []string{AmendRuleSharedAncestor},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateScopeAmendment(itemWith(tc.slices), tc.violations, tc.pol, tc.protected)
			if got.Admitted != tc.wantAdmitted {
				t.Fatalf("Admitted = %v, want %v (verdicts %+v, refusal %q)",
					got.Admitted, tc.wantAdmitted, got.Verdicts, got.Refusal)
			}
			if len(tc.wantRules) > 0 {
				if len(got.Verdicts) != len(tc.wantRules) {
					t.Fatalf("verdict count = %d, want %d: %+v", len(got.Verdicts), len(tc.wantRules), got.Verdicts)
				}
				for i, want := range tc.wantRules {
					if got.Verdicts[i].Rule != want {
						t.Errorf("verdict[%d] (%s) rule = %q, want %q",
							i, got.Verdicts[i].File, got.Verdicts[i].Rule, want)
					}
				}
			}
			for i, want := range tc.wantAncestors {
				if got.Verdicts[i].Ancestor != want {
					t.Errorf("verdict[%d] (%s) ancestor = %q, want %q",
						i, got.Verdicts[i].File, got.Verdicts[i].Ancestor, want)
				}
			}
		})
	}
}

// An empty violation list must never read as an admission — the caller uses
// Admitted as permission to continue past a FAILING gate.
func TestEvaluateScopeAmendment_NoViolationsIsNotAnAdmission(t *testing.T) {
	got := EvaluateScopeAmendment(itemWith(tokenSweepSlices), nil, mills.ScopeAmendmentPolicy{}, nil)
	if got.Admitted {
		t.Fatalf("empty violation list must not be Admitted: %+v", got)
	}
	if got.Refusal == "" {
		t.Errorf("expected a refusal explanation, got %+v", got)
	}
}

// Duplicate violations (spawn telemetry reports a created-then-modified file
// twice) must produce ONE verdict, or the max_files cap would count phantoms.
func TestEvaluateScopeAmendment_DeduplicatesViolations(t *testing.T) {
	v := []string{"pkg/mills/store/a.go", "pkg/mills/store/a.go"}
	got := EvaluateScopeAmendment(itemWith(stopLeverSlices), v, mills.ScopeAmendmentPolicy{}, nil)
	if len(got.Verdicts) != 1 {
		t.Fatalf("verdicts = %d, want 1: %+v", len(got.Verdicts), got.Verdicts)
	}
	if !got.Admitted {
		t.Errorf("want admitted, got %+v", got)
	}
}

// The chosen slice must be the one whose declared files share the DEEPEST
// ancestor, with ties going to the first — otherwise a replay after a CAS
// re-read could append the same file to a different slice.
func TestEvaluateScopeAmendment_PicksDeepestAncestorSlice(t *testing.T) {
	item := itemWith([]store.Slice{
		{Name: "hud", Files: []string{"internal/hud/api.go"}},
		{Name: "pipeline", Files: []string{"pkg/mills/pipeline/runner.go"}},
	})
	got := EvaluateScopeAmendment(item, []string{"pkg/mills/store/dao_pipeline.go"}, mills.ScopeAmendmentPolicy{}, nil)
	if !got.Admitted {
		t.Fatalf("want admitted: %+v", got)
	}
	if got.Verdicts[0].SliceIndex != 1 {
		t.Errorf("slice index = %d, want 1 (the pkg/mills anchor)", got.Verdicts[0].SliceIndex)
	}
}

func TestAmendmentDecision_Summary(t *testing.T) {
	got := EvaluateScopeAmendment(itemWith(tokenSweepSlices), tokenSweepViolations, mills.ScopeAmendmentPolicy{}, nil)
	summary := got.Summary()
	if !strings.HasPrefix(summary, "scope amended: ") {
		t.Fatalf("summary = %q, want the 'scope amended: ' prefix the HUD renders", summary)
	}
	for _, f := range tokenSweepViolations {
		if !strings.Contains(summary, f) {
			t.Errorf("summary %q missing %q", summary, f)
		}
	}
	if !strings.Contains(summary, "internal/hud/frontend/src/lib/components") {
		t.Errorf("summary %q missing the shared ancestor", summary)
	}
}

func TestApplyAmendment(t *testing.T) {
	item := itemWith(stopLeverSlices)
	d := EvaluateScopeAmendment(item, stopLeverViolations, mills.ScopeAmendmentPolicy{}, nil)
	if !d.Admitted {
		t.Fatalf("fixture must be admitted: %+v", d)
	}
	out := ApplyAmendment(item.Slices, d)
	if len(out) != 1 {
		t.Fatalf("slice count = %d, want 1", len(out))
	}
	for _, want := range stopLeverViolations {
		found := false
		for _, f := range out[0].Files {
			if f == want {
				found = true
			}
		}
		if !found {
			t.Errorf("amended files %v missing %q", out[0].Files, want)
		}
	}
	// The input must not be mutated: the runner keeps the pre-amendment slices
	// so it can restore them when the CAS write fails.
	if len(item.Slices[0].Files) != 3 {
		t.Errorf("input slices mutated: %v", item.Slices[0].Files)
	}
	// Idempotent under the CAS retry: re-applying appends nothing.
	again := ApplyAmendment(out, d)
	if len(again[0].Files) != len(out[0].Files) {
		t.Errorf("re-apply appended duplicates: %v", again[0].Files)
	}
}

func TestApplyAmendment_RefusedDecisionIsANoOp(t *testing.T) {
	item := itemWith(stopLeverSlices)
	d := EvaluateScopeAmendment(item, []string{"libs/game/scripts/main.gd"}, mills.ScopeAmendmentPolicy{}, nil)
	out := ApplyAmendment(item.Slices, d)
	if len(out[0].Files) != 3 {
		t.Fatalf("refused decision must not widen scope: %v", out[0].Files)
	}
}

// The amended paths must actually satisfy the gate on the next evaluation —
// otherwise amend-and-proceed would just move the failure downstream. Covers
// the absolute-spawn-path form too, since that is what production writes.
func TestApplyAmendment_AmendedScopePassesTheGate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		violations []string
	}{
		{"relative", tokenSweepViolations},
		{"absolute spawn paths", []string{
			"/workspace/services/loom-core/pkg/mills/store/dao_pipeline.go",
			"/workspace/services/loom-core/pkg/mills/clients/spawn.go",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slices := tokenSweepSlices
			if tc.name != "relative" {
				slices = stopLeverSlices
			}
			item := itemWith(slices)
			d := EvaluateScopeAmendment(item, tc.violations, mills.ScopeAmendmentPolicy{}, nil)
			if !d.Admitted {
				t.Fatalf("fixture must be admitted: %+v", d)
			}
			item.Slices = ApplyAmendment(item.Slices, d)
			in := StageInput{Item: item, FilesChanged: tc.violations}
			if got := ScopeViolations(in); len(got) != 0 {
				t.Fatalf("amended scope still violates: %v", got)
			}
		})
	}
}

// ScopeViolations must agree with Scope.Evaluate — it is the runner's only
// source for the FULL (uncapped, untruncated) list.
func TestScopeViolations_MatchesGateVerdict(t *testing.T) {
	item := itemWith(tokenSweepSlices)
	in := StageInput{
		Item: item,
		FilesChanged: append([]string{
			"internal/hud/frontend/src/lib/components/mills/MillsPipelineDrawer.svelte",
		}, tokenSweepViolations...),
	}
	got := ScopeViolations(in)
	if len(got) != 2 {
		t.Fatalf("violations = %v, want the 2 shared-component files", got)
	}
	out, err := (&Scope{}).Evaluate(t.Context(), in)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if out.Pass {
		t.Fatalf("gate must fail on the same input: %+v", out)
	}
	for _, f := range got {
		if !strings.Contains(strings.Join(out.Reasons, " "), f) {
			t.Errorf("gate reason %v does not name %q", out.Reasons, f)
		}
	}
}

// A slice-less item yields no enforceable envelope, so there is nothing to
// amend — the gate already resolves it to an advisory skip.
func TestScopeViolations_SlicelessItemHasNone(t *testing.T) {
	in := StageInput{Item: itemWith(nil), FilesChanged: []string{"pkg/x/y.go"}}
	if got := ScopeViolations(in); got != nil {
		t.Fatalf("violations = %v, want nil for a slice-less item", got)
	}
}
