package main

import (
	"strings"
	"testing"
)

// onboardPolicyFixture mirrors the shape of the live gitops ConfigMap: nested
// under `data.policy.yaml`, comment-heavy, with a nested `projects:` under
// intake.gitlab that must be distinguished from other `projects` keys.
const onboardPolicyFixture = `apiVersion: v1
kind: ConfigMap
data:
  policy.yaml: |
    version: 2
    enabled: true    # kill switch

    pipeline:
      protected_paths:
        - "**/*auth*.go"
      protected_paths_per_repo:
        # existing entry
        "services/flexdeck":
          - "internal/auth/**"
        "libs/edilint":
          - ".gitlab-ci.yml"
      # budget overlay
      per_repo_overrides:
        "services/flexdeck":
          max_usd_per_run: 3
          max_runs_per_day: 3

    cross_repo:
      enabled: true
      # demand list
      demand_projects:
        - "services/flexdeck"
        # Added 2026-09-01: libs/edilint
        - "libs/edilint"
      # allow_bootstrapped: runtime counterpart
      allow_bootstrapped: true

    intake:
      gitlab:
        enabled: true
        eligible_label: "mills-eligible"
        projects:
          - "services/flexdeck"
          - "libs/edilint"
      canary_gc:
        enabled: true
`

func TestApplyOnboardPolicyEdit_InsertsEverywhereAndKeepsComments(t *testing.T) {
	res, err := applyOnboardPolicyEdit(onboardPolicyFixture, onboardPolicyEdit{
		Project:        "labs/newthing",
		IntakeIssues:   true,
		ProtectedPaths: []string{".gitlab-ci.yml", "**/secret*.yaml"},
		MaxUSDPerRun:   2.5,
		MaxRunsPerDay:  4,
		Note:           "2026-09-04: onboarded from the HUD",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := []string{
		"cross_repo.demand_projects", "intake.gitlab.projects",
		"pipeline.protected_paths_per_repo", "pipeline.per_repo_overrides",
	}
	if strings.Join(res.Changed, ",") != strings.Join(want, ",") {
		t.Fatalf("changed = %v, want %v", res.Changed, want)
	}
	c := res.Content
	// Sequence items land after the last item, at the item indent, with the note.
	if !strings.Contains(c, "        - \"libs/edilint\"\n        # 2026-09-04: onboarded from the HUD\n        - \"labs/newthing\"\n      # allow_bootstrapped") {
		t.Fatalf("demand_projects insertion misplaced:\n%s", c)
	}
	if !strings.Contains(c, "          - \"libs/edilint\"\n          # 2026-09-04: onboarded from the HUD\n          - \"labs/newthing\"\n      canary_gc:") {
		t.Fatalf("intake projects insertion misplaced:\n%s", c)
	}
	// Map entries land after the last entry's body, one level in.
	if !strings.Contains(c, "        \"libs/edilint\":\n          - \".gitlab-ci.yml\"\n        # 2026-09-04: onboarded from the HUD\n        \"labs/newthing\":\n          - \".gitlab-ci.yml\"\n          - \"**/secret*.yaml\"\n      # budget overlay") {
		t.Fatalf("protected_paths_per_repo insertion misplaced:\n%s", c)
	}
	if !strings.Contains(c, "          max_runs_per_day: 3\n        # 2026-09-04: onboarded from the HUD\n        \"labs/newthing\":\n          max_usd_per_run: 2.5\n          max_runs_per_day: 4\n\n    cross_repo:") {
		t.Fatalf("per_repo_overrides insertion misplaced:\n%s", c)
	}
	// Every original comment survives byte-for-byte.
	for _, keep := range []string{"# kill switch", "# existing entry", "# demand list", "# Added 2026-09-01: libs/edilint", "# allow_bootstrapped: runtime counterpart", "# budget overlay"} {
		if !strings.Contains(c, keep) {
			t.Fatalf("comment %q lost", keep)
		}
	}
}

func TestApplyOnboardPolicyEdit_IsIdempotent(t *testing.T) {
	first, err := applyOnboardPolicyEdit(onboardPolicyFixture, onboardPolicyEdit{
		Project: "labs/newthing", IntakeIssues: true, ProtectedPaths: []string{"x"}, MaxRunsPerDay: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := applyOnboardPolicyEdit(first.Content, onboardPolicyEdit{
		Project: "labs/newthing", IntakeIssues: true, ProtectedPaths: []string{"x"}, MaxRunsPerDay: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Changed) != 0 || len(second.Skipped) != 4 {
		t.Fatalf("second apply changed=%v skipped=%v", second.Changed, second.Skipped)
	}
	if second.Content != first.Content {
		t.Fatal("second apply altered content")
	}
	// An already-listed repo is skipped even when unquoted in the file.
	res, err := applyOnboardPolicyEdit(strings.Replace(onboardPolicyFixture, `- "libs/edilint"`, `- libs/edilint`, 1),
		onboardPolicyEdit{Project: "libs/edilint"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 0 {
		t.Fatalf("unquoted existing item not detected: %v", res.Changed)
	}
}

func TestApplyOnboardPolicyEdit_FailsClosedOnMissingAnchor(t *testing.T) {
	if _, err := applyOnboardPolicyEdit("data:\n  policy.yaml: |\n    version: 2\n", onboardPolicyEdit{Project: "a/b"}); err == nil {
		t.Fatal("expected anchor error")
	}
	if _, err := applyOnboardPolicyEdit(onboardPolicyFixture, onboardPolicyEdit{}); err == nil {
		t.Fatal("expected project-required error")
	}
	// Inline empty map cannot be appended to.
	fx := strings.Replace(onboardPolicyFixture,
		"      protected_paths_per_repo:\n        # existing entry\n        \"services/flexdeck\":\n          - \"internal/auth/**\"\n        \"libs/edilint\":\n          - \".gitlab-ci.yml\"\n",
		"      protected_paths_per_repo: {}\n", 1)
	if _, err := applyOnboardPolicyEdit(fx, onboardPolicyEdit{Project: "a/b", ProtectedPaths: []string{"x"}}); err == nil || !strings.Contains(err.Error(), "inline empty map") {
		t.Fatalf("expected inline-map error, got %v", err)
	}
}

func TestBumpPolicyChecksum(t *testing.T) {
	dep := "metadata:\n  annotations:\n    loom.flexinfer.ai/policy-checksum: " + strings.Repeat("0", 64) + "\nspec: {}\n"
	out, sum, err := bumpPolicyChecksum(dep, "policy text")
	if err != nil {
		t.Fatal(err)
	}
	if len(sum) != 64 || !strings.Contains(out, "policy-checksum: "+sum) || strings.Contains(out, strings.Repeat("0", 64)) {
		t.Fatalf("checksum not rewritten: %s", out)
	}
	if _, _, err := bumpPolicyChecksum("spec: {}\n", "x"); err == nil {
		t.Fatal("expected missing-annotation error")
	}
	again, _, _ := bumpPolicyChecksum(out, "policy text")
	if again != out {
		t.Fatal("unchanged checksum must be a no-op")
	}
}
