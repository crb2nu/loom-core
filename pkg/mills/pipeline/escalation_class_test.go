package pipeline

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

// TestEscalationClassLabel guards the metric label mapping. The key
// regression it locks in: the "[class=…]" marker carries ErrorClass values
// ("infra", "config"), NOT FailureClass spellings ("infrastructure",
// "configuration"), so the FailureClass spellings must fall through to
// "unclassified" rather than minting a phantom series.
// TestEscalationClassLabel pins the metric label under the explicit-class
// contract: the class the escalating call site DECLARED wins, and the needle
// fallback runs only when it declared none. The old table asserted the opposite
// (parse "[class=…]" out of the reason prose); that transport is gone, and the
// "[class=…]" text that remains in a reason is operator-facing prose only.
func TestEscalationClassLabel(t *testing.T) {
	cases := []struct {
		name   string
		cls    ErrorClass
		reason string
		want   string
	}{
		{"declared wins", ClassConfig, "stage merge terminal config error: status 405", "config"},
		{"declared wins over contradicting prose", ClassCode,
			"gate tests failed (connection reset by peer); implement exceeded 3 attempts", "code"},
		{"undeclared falls back to needles", "", "flexinfer chat: status 429: too many requests", "transient_quota"},
		{"undeclared with no evidence", "", "", "unclassified"},
		{"invalid declared class falls back", ErrorClass("wat"),
			"flexinfer chat: status 429: too many requests", "transient_quota"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escalationClassLabel(tc.cls, tc.reason, ""); got != tc.want {
				t.Fatalf("escalationClassLabel(%q, %q) = %q, want %q", tc.cls, tc.reason, got, tc.want)
			}
		})
	}
}
func TestRunner_EscalateIncrementsClassMetric(t *testing.T) {
	ctx := context.Background()
	st, run, item := newRunnerEnv(t)
	// nil PolicyManager → EscalationAutoRetryCap defaults to 0, and a
	// [class=code] reason is non-transient, so escalateWithItem takes the
	// deterministic real-escalation path (not maybeAutoRetry).
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)

	before := testutil.ToFloat64(mills.EscalationClassTotal.WithLabelValues("code"))
	reason := "stage tests errored after 3 attempts [class=code]: devbox quality gate failed"
	if err := r.escalateWithItem(ctx, run, item, ClassCode, reason); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if after := testutil.ToFloat64(mills.EscalationClassTotal.WithLabelValues("code")); after != before+1 {
		t.Fatalf("EscalationClassTotal{class=code} = %v, want %v", after, before+1)
	}
}

func TestRunner_TaggedExternalDependencyEscalationUsesDedicatedMetricClass(t *testing.T) {
	ctx := context.Background()
	st, run, item := newRunnerEnv(t)
	run.ExternalDependencyID = "external_dependency.gitlab.auth_failure"
	run.ExternalDependency = "gitlab"
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)

	externalBefore := testutil.ToFloat64(mills.EscalationClassTotal.WithLabelValues(
		telemetry.EscalationClassExternalDependency,
	))
	codeBefore := testutil.ToFloat64(mills.EscalationClassTotal.WithLabelValues(string(ClassCode)))
	if err := r.escalateWithItem(ctx, run, item, ClassCode, "compile failed"); err != nil {
		t.Fatalf("escalate: %v", err)
	}

	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.EscalationClass != telemetry.EscalationClassExternalDependency ||
		got.FailureClass != string(FailureCode) ||
		got.ExternalDependencyID != run.ExternalDependencyID ||
		got.ExternalDependency != run.ExternalDependency {
		t.Fatalf("persisted classification = class %q failure %q dependency %q/%q",
			got.EscalationClass, got.FailureClass, got.ExternalDependencyID, got.ExternalDependency)
	}
	if after := testutil.ToFloat64(mills.EscalationClassTotal.WithLabelValues(
		telemetry.EscalationClassExternalDependency,
	)); after != externalBefore+1 {
		t.Fatalf("external_dependency metric = %v, want %v", after, externalBefore+1)
	}
	if after := testutil.ToFloat64(mills.EscalationClassTotal.WithLabelValues(string(ClassCode))); after != codeBefore {
		t.Fatalf("code metric = %v, want unchanged %v", after, codeBefore)
	}
}

func TestEscalationMetadataFromEvidence(t *testing.T) {
	retryableFalse := false
	retryableTrue := true
	cases := []struct {
		name string
		// cls is what the escalating call site DECLARED. Empty means the site
		// had no verdict, which is the only case the needle fallback covers.
		cls    ErrorClass
		reason string
		tail   string
		want   store.EscalationMetadata
	}{
		{
			name:   "declared class wins over the evidence needles",
			cls:    ClassConfig,
			reason: "stage ci_watch terminal config error (not retried) [class=config]: gitlab: status 401: unauthorized",
			want: store.EscalationMetadata{
				EscalationClass:      "config",
				FailureClass:         "configuration",
				ExternalDependencyID: "external_dependency.gitlab.auth_failure",
				ExternalDependency:   "gitlab",
				Retryable:            &retryableFalse,
			},
		},
		{
			// EXPECTATION CHANGED (unclassified-escalation fix): a missing
			// marker used to persist NO class at all, which is the bug — the
			// run bucketed as "unclassified" and auto-requeue failed it closed
			// to a human. The evidence now falls through to Classify; this
			// prose matches no infra/transient needle, so it lands on the
			// conservative ClassCode default — same never-auto-requeued
			// outcome as before, but now attributable.
			name:   "external dependency from log tail without class marker",
			reason: "stage build errored after retries",
			tail:   "registry cache export failed: error writing manifest blob",
			want: store.EscalationMetadata{
				EscalationClass:      "code",
				FailureClass:         "code",
				ExternalDependencyID: "external_dependency.blob_storage.manifest_write",
				ExternalDependency:   "container_registry_blob_storage",
				Retryable:            &retryableTrue,
			},
		},
		{
			// An unparseable marker is still ignored AS A MARKER; the evidence
			// fallback then classifies the text (no infra needle here → code).
			name:   "unknown class marker falls back to evidence classification",
			reason: "stage x errored [class=infrastructure]: boom",
			want: store.EscalationMetadata{
				EscalationClass: "code",
				FailureClass:    "code",
				Retryable:       &retryableTrue,
			},
		},
		{
			// THE WIN: a real no-marker path (Runner.Start's drive-failure
			// escalation) carrying genuine spawn-infra evidence now classifies
			// infra instead of NULL, so the auto-requeue sweep can see it.
			name:   "unmarked drive failure with spawn-infra evidence classifies infra",
			reason: "pipeline drive failed: hud spawn spawn-c725e916ef11 status=failed: agent turn driver lost across mobile-hud restart; unkeyed spawn cannot be re-driven",
			want: store.EscalationMetadata{
				EscalationClass: "infra",
				FailureClass:    "infrastructure",
				Retryable:       &retryableTrue,
			},
		},
		{
			// The live 2026-07-27 shape: gate-retry-cap exhaustion escalates
			// with no marker and RetryFrom = implement/pr_self_review, which is
			// exactly where the 5 unclassified production runs sat.
			name:   "unmarked gate-retry-cap exhaustion is attributable, not unclassified",
			reason: "gate post_implement_gate failed (nonempty_diff); implement exceeded 3 attempts",
			want: store.EscalationMetadata{
				EscalationClass: "code",
				FailureClass:    "code",
				Retryable:       &retryableTrue,
			},
		},
		{
			// SAFETY PIN: with no evidence at all there is nothing to classify,
			// so we must NOT invent a class. Guessing here would put a run into
			// a retry policy its evidence cannot support.
			name:   "empty evidence stays unclassified rather than guessed",
			reason: "",
			tail:   "",
			want:   store.EscalationMetadata{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := escalationMetadataFromEvidence(tc.cls, tc.reason, tc.tail)
			if got.EscalationClass != tc.want.EscalationClass ||
				got.FailureClass != tc.want.FailureClass ||
				got.ExternalDependencyID != tc.want.ExternalDependencyID ||
				got.ExternalDependency != tc.want.ExternalDependency {
				t.Fatalf("metadata = %+v, want %+v", got, tc.want)
			}
			switch {
			case tc.want.Retryable == nil && got.Retryable != nil:
				t.Fatalf("Retryable = %v, want nil", *got.Retryable)
			case tc.want.Retryable != nil && got.Retryable == nil:
				t.Fatalf("Retryable = nil, want %v", *tc.want.Retryable)
			case tc.want.Retryable != nil && *got.Retryable != *tc.want.Retryable:
				t.Fatalf("Retryable = %v, want %v", *got.Retryable, *tc.want.Retryable)
			}
		})
	}
}

// TestRunner_EscalateStampsClassAndDiscountsNoOp drives a real escalation
// through the store and proves the two ends meet: a spawn-saturation
// escalation (class=transient_quota, cost $0) is stamped on the run and then
// discounted by CountBudgetedSince, while a real code-class escalation is
// stamped and still counts. Without the fix, a burst of the former exhausts
// MaxRunsPerDay on no-op runs (the 2026-07-02 wedge).
func TestRunner_EscalateStampsClassAndDiscountsNoOp(t *testing.T) {
	ctx := context.Background()
	st, run, item := newRunnerEnv(t)
	// nil PolicyManager → EscalationAutoRetryCap defaults to 0, so
	// escalateWithItem takes the deterministic real-escalation path.
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)

	// A no-op capacity escalation: cost stays $0, reason carries the
	// transient_quota class marker the runner emits at the transient hard cap.
	reason := "stage plan_slice errored after 8 total attempts (cap 8) [class=transient_quota]: 400 max concurrent spawns reached (6)"
	if err := r.escalateWithItem(ctx, run, item, ClassTransientQuota, reason); err != nil {
		t.Fatalf("escalate: %v", err)
	}

	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("run state = %s, want escalated", got.State)
	}
	if got.EscalationClass != "transient_quota" {
		t.Fatalf("EscalationClass = %q, want transient_quota", got.EscalationClass)
	}
	if got.FailureClass != "transient_quota" {
		t.Fatalf("FailureClass = %q, want transient_quota", got.FailureClass)
	}
	if got.EscalationRetryable == nil || !*got.EscalationRetryable {
		t.Fatalf("EscalationRetryable = %v, want true", got.EscalationRetryable)
	}

	// Bound the window just before the run's persisted StartedAt so the seeded
	// row is inside it regardless of the helper's fixed seed date.
	since := got.StartedAt.Add(-time.Hour)
	total, err := st.Pipeline.CountSince(ctx, since)
	if err != nil {
		t.Fatalf("count-since: %v", err)
	}
	budgeted, err := st.Pipeline.CountBudgetedSince(ctx, since)
	if err != nil {
		t.Fatalf("count-budgeted-since: %v", err)
	}
	if total != 1 {
		t.Fatalf("CountSince = %d, want 1 (the no-op run exists)", total)
	}
	if budgeted != 0 {
		t.Fatalf("CountBudgetedSince = %d, want 0 (no-op capacity escalation discounted)", budgeted)
	}
}

// TestRunner_EscalateFiresOnEscalatedHook proves the real-escalation path
// invokes the OnEscalated hook (squads failed-outcome attribution) and
// that hook errors never undo the state transition.
func TestRunner_EscalateFiresOnEscalatedHook(t *testing.T) {
	ctx := context.Background()
	st, run, item := newRunnerEnv(t)
	r := New(st, newPassingGates(t), &fakeDispatcher{}, nil)

	var hookRun, hookItem string
	r.OnEscalated = func(_ context.Context, hr *store.PipelineRun, hi *store.BacklogItem) error {
		hookRun, hookItem = hr.ID, hi.ID
		return fmt.Errorf("hook failure must not undo escalation")
	}

	reason := "stage tests errored after 3 attempts [class=code]: devbox quality gate failed"
	if err := r.escalateWithItem(ctx, run, item, ClassCode, reason); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if hookRun != run.ID || hookItem != item.ID {
		t.Fatalf("hook args: got (%q,%q) want (%q,%q)", hookRun, hookItem, run.ID, item.ID)
	}
	got, err := st.Pipeline.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != store.PipelineEscalated {
		t.Fatalf("run state: got %v want escalated (hook error must not undo)", got.State)
	}
}
