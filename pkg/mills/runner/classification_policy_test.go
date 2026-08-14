package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// stubHealthGates is the runner-side HealthGatePreflight double.
// pipeline.FailClosedPreflight satisfies the same shape in production.
type stubHealthGates struct {
	decision gates.HealthDecision
	err      error
	calls    int
}

func (s *stubHealthGates) DecideHealthGates(context.Context) (gates.HealthDecision, error) {
	s.calls++
	return s.decision, s.err
}

func passingHealthGates() *stubHealthGates {
	return &stubHealthGates{decision: gates.HealthDecision{Allowed: true, Status: "pass"}}
}

func blockingHealthGates() *stubHealthGates {
	return &stubHealthGates{decision: gates.HealthDecision{
		Allowed: false, FailClosed: true, Status: "block",
		Reasons: []string{"longhorn volume degraded"},
	}}
}

// externalIncidentEditorOutput pairs an outside-system remediation proposal
// (which the policy must drop for an external incident) with a repo-owned
// documentation follow-up (which it must keep and label).
func externalIncidentEditorOutput() *council.EditorOutput {
	return &council.EditorOutput{
		Backend: "claude-code",
		Model:   "claude-opus",
		BacklogProposals: []council.BacklogProposal{
			{
				Title:  "Ask GitLab to restart its runners",
				Slices: []store.Slice{{Name: "infra", Files: []string{"k8s/base/servers/gateway/deployment.yaml"}}},
			},
			{
				Title:  "Document GitLab outage triage",
				Slices: []store.Slice{{Name: "docs", Files: []string{"docs/council-external-dependency-incidents.md"}}},
			},
		},
		Sidecar: council.Sidecar{BacklogDeltas: council.SidecarBacklog{Created: 2}},
	}
}

func externalIncidentBrief() *council.Brief {
	return &council.Brief{ClassifiedCIFailures: []*store.ClassifiedCIFailureSummary{
		{RunID: "run-1", BacklogID: "b-1", ExternalDependency: "gitlab", FailureClass: "infrastructure"},
		{RunID: "run-2", BacklogID: "b-2", ExternalDependencyID: "dep-openai", FailureClass: "infrastructure"},
	}}
}

func policyTestRunner(gatesImpl HealthGatePreflight) *Runner {
	return &Runner{
		HealthGates: gatesImpl,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestApplyClassificationPolicy_UnwiredGatesLeaveEditorOutputByteIdentical pins
// the hard requirement: until an operator configures HealthGates the seam is a
// no-op, even when the run carries evidence the policy would otherwise act on.
func TestApplyClassificationPolicy_UnwiredGatesLeaveEditorOutputByteIdentical(t *testing.T) {
	t.Parallel()
	out := externalIncidentEditorOutput()
	want := externalIncidentEditorOutput()

	policyTestRunner(nil).applyClassificationPolicy(
		context.Background(), "run-x", externalIncidentBrief(), out)

	if !reflect.DeepEqual(out, want) {
		t.Fatalf("editor output mutated with no health gates configured:\n got %#v\nwant %#v", out, want)
	}
}

// TestApplyClassificationPolicy_ExternalIncidentSuppressesOutsideSystemWork
// proves the wired policy reclassifies a matching run: outside-system
// remediation is dropped and the surviving repo follow-up is labeled.
func TestApplyClassificationPolicy_ExternalIncidentSuppressesOutsideSystemWork(t *testing.T) {
	t.Parallel()
	out := externalIncidentEditorOutput()
	health := passingHealthGates()

	policyTestRunner(health).applyClassificationPolicy(
		context.Background(), "run-x", externalIncidentBrief(), out)

	if health.calls != 1 {
		t.Fatalf("health gate calls = %d, want 1", health.calls)
	}
	if len(out.BacklogProposals) != 1 || out.BacklogProposals[0].Title != "Document GitLab outage triage" {
		t.Fatalf("proposals = %#v, want only the in-repo documentation follow-up", out.BacklogProposals)
	}
	if out.Sidecar.BacklogDeltas.Created != 1 {
		t.Fatalf("sidecar created = %d, want 1", out.Sidecar.BacklogDeltas.Created)
	}
	var labeled bool
	for _, label := range out.BacklogProposals[0].Labels {
		if label == council.ExternalDependencyIncidentLabel {
			labeled = true
		}
	}
	if !labeled {
		t.Fatalf("labels = %v, want %q", out.BacklogProposals[0].Labels, council.ExternalDependencyIncidentLabel)
	}
}

// TestApplyClassificationPolicy_NonExternalEvidenceLeavesProposalsUnchanged is
// the empty-policy default: healthy storage plus evidence that is not an
// external incident must pass the editor's proposals straight through.
func TestApplyClassificationPolicy_NonExternalEvidenceLeavesProposalsUnchanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		brief *council.Brief
	}{
		{name: "nil brief", brief: nil},
		{name: "no classified failures", brief: &council.Brief{}},
		{
			name: "repository regression only",
			brief: &council.Brief{ClassifiedCIFailures: []*store.ClassifiedCIFailureSummary{
				{RunID: "run-1", FailureClass: "code"},
			}},
		},
		{
			// One external failure in the window must not declare the whole
			// run external — that would suppress legitimate repository work.
			name: "mixed evidence",
			brief: &council.Brief{ClassifiedCIFailures: []*store.ClassifiedCIFailureSummary{
				{RunID: "run-1", ExternalDependency: "gitlab"},
				{RunID: "run-2", FailureClass: "code"},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := externalIncidentEditorOutput()
			want := externalIncidentEditorOutput()

			policyTestRunner(passingHealthGates()).applyClassificationPolicy(
				context.Background(), "run-x", tc.brief, out)

			if !reflect.DeepEqual(out, want) {
				t.Fatalf("editor output mutated for non-external evidence:\n got %#v\nwant %#v", out, want)
			}
		})
	}
}

// TestApplyClassificationPolicy_BlockedGatesFailClosed pins the fail-closed
// arm: unhealthy storage clears every proposal and stamps the block reason.
func TestApplyClassificationPolicy_BlockedGatesFailClosed(t *testing.T) {
	t.Parallel()
	out := externalIncidentEditorOutput()

	policyTestRunner(blockingHealthGates()).applyClassificationPolicy(
		context.Background(), "run-x", nil, out)

	if len(out.BacklogProposals) != 0 || out.Sidecar.BacklogDeltas.Created != 0 {
		t.Fatalf("output = %+v, want every proposal cleared", out)
	}
	if !strings.Contains(out.Sidecar.OmitReason, "storage health") {
		t.Fatalf("omit reason = %q, want the storage-health planning block", out.Sidecar.OmitReason)
	}
}

// TestApplyClassificationPolicy_MalformedGateEvidenceBlocksAndNeverPanics
// covers the degenerate inputs: an unavailable gate is unknown health (block,
// logged, not a run failure) and a nil editor output is a safe no-op.
func TestApplyClassificationPolicy_MalformedGateEvidenceBlocksAndNeverPanics(t *testing.T) {
	t.Parallel()

	out := externalIncidentEditorOutput()
	r := policyTestRunner(&stubHealthGates{err: errors.New("storage evaluator unreachable")})
	r.applyClassificationPolicy(context.Background(), "run-x", externalIncidentBrief(), out)
	if len(out.BacklogProposals) != 0 {
		t.Fatalf("proposals = %#v, want an unavailable gate treated as blocked", out.BacklogProposals)
	}
	if !strings.Contains(out.Sidecar.OmitReason, "storage health") {
		t.Fatalf("omit reason = %q, want the storage-health planning block", out.Sidecar.OmitReason)
	}

	// A nil output must not panic on either gate verdict.
	policyTestRunner(passingHealthGates()).applyClassificationPolicy(context.Background(), "run-x", nil, nil)
	policyTestRunner(blockingHealthGates()).applyClassificationPolicy(context.Background(), "run-x", nil, nil)
}

// TestCouncilIncidentClass_RequiresUnanimousExternalEvidence documents the
// reducer's contract directly, independent of the policy it feeds.
func TestCouncilIncidentClass_RequiresUnanimousExternalEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		brief *council.Brief
		want  string
	}{
		{name: "nil brief", brief: nil, want: ""},
		{name: "no evidence", brief: &council.Brief{}, want: ""},
		{
			name:  "unanimous external",
			brief: externalIncidentBrief(),
			want:  string(council.CIIncidentExternalDependency),
		},
		{
			name: "mixed",
			brief: &council.Brief{ClassifiedCIFailures: []*store.ClassifiedCIFailureSummary{
				{RunID: "run-1", ExternalDependency: "gitlab"},
				{RunID: "run-2", FailureClass: "code"},
			}},
			want: "",
		},
		{
			name: "nil entries only",
			brief: &council.Brief{ClassifiedCIFailures: []*store.ClassifiedCIFailureSummary{
				nil, nil,
			}},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := councilIncidentClass(tc.brief); got != tc.want {
				t.Fatalf("councilIncidentClass = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRun_HealthyGatesPreserveTheHappyPath proves wiring the gate does not
// regress an ordinary run: healthy storage with no external evidence still
// mints every proposed backlog item.
func TestRun_HealthyGatesPreserveTheHappyPath(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(2))
	env.runner.HealthGates = passingHealthGates()

	res, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerManual})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Mutation.CreatedItems) != 2 {
		t.Fatalf("created items = %d, want 2", len(res.Mutation.CreatedItems))
	}
	if res.Editor.Sidecar.OmitReason != "" {
		t.Fatalf("omit reason = %q, want empty", res.Editor.Sidecar.OmitReason)
	}
}

// TestRun_BlockedGatesFailClosedBeforeBacklogMutation proves the policy reaches
// the mutator: a blocked storage gate mints no backlog work at all.
func TestRun_BlockedGatesFailClosedBeforeBacklogMutation(t *testing.T) {
	env := newRunnerEnv(t, sampleProposals(2))
	env.runner.HealthGates = blockingHealthGates()

	res, err := env.runner.Run(context.Background(), RunInput{Trigger: store.CouncilTriggerManual})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Mutation.CreatedItems) != 0 {
		t.Fatalf("created items = %#v, want none under a blocked storage gate", res.Mutation.CreatedItems)
	}
	if !strings.Contains(res.Editor.Sidecar.OmitReason, "storage health") {
		t.Fatalf("omit reason = %q, want the storage-health planning block", res.Editor.Sidecar.OmitReason)
	}
}

// TestApplyClassificationPolicy_ObserveModeVerdictDoesNotBlockPlanning pins the
// vocabulary seam between the gate and the planning policy. The operator's
// observe mode returns an *allowing* verdict whose status word is "observe";
// council.planningStorageHealthy recognizes only "" and "pass", so forwarding
// that word verbatim would silently drop every proposal — turning a
// deliberately non-blocking mode into a total planning outage.
func TestApplyClassificationPolicy_ObserveModeVerdictDoesNotBlockPlanning(t *testing.T) {
	t.Parallel()
	out := externalIncidentEditorOutput()
	observing := &stubHealthGates{decision: gates.HealthDecision{
		Allowed: true,
		Status:  "observe",
		Reasons: []string{"storage degraded; not enforcing"},
	}}

	policyTestRunner(observing).applyClassificationPolicy(
		context.Background(), "run-x", nil, out)

	if len(out.BacklogProposals) != 2 {
		t.Fatalf("proposals = %d, want 2; an allowing observe verdict must not block planning", len(out.BacklogProposals))
	}
	if out.Sidecar.OmitReason != "" {
		t.Fatalf("omit reason = %q, want empty under an allowing verdict", out.Sidecar.OmitReason)
	}
}

// A non-allowing verdict must still block regardless of its status word.
func TestStorageVerdictForPlanning_NormalizesBothArms(t *testing.T) {
	t.Parallel()
	allowed := storageVerdictForPlanning(gates.HealthDecision{Allowed: true, Status: "observe"})
	if !allowed.Allowed || allowed.Status != "pass" {
		t.Fatalf("allowing verdict = %+v, want {true pass}", *allowed)
	}
	blocked := storageVerdictForPlanning(gates.HealthDecision{Allowed: false, Status: "observe"})
	if blocked.Allowed || blocked.Status != "block" {
		t.Fatalf("blocking verdict = %+v, want {false block}", *blocked)
	}
}
