package killtest

import (
	"strings"
	"testing"
	"time"
)

func TestEvaluateRejectsAdversarialProcessDeleteEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
	}{
		{
			name: "sample completion is missing",
			mutate: func(ev *Evidence) {
				ev.PostCrashProcessSamples[0].CompletedAt = time.Time{}
			},
		},
		{
			name: "sample completes before it starts",
			mutate: func(ev *Evidence) {
				completedAt := ev.PostCrashProcessSamples[0].ObservedAt.Add(-time.Nanosecond)
				ev.PostCrashProcessSamples[0].CompletedAt = completedAt
				ev.CrashAProcessAuthorization.SampleCompletedAt = completedAt
				ev.CrashBProcessAuthorization.SampleCompletedAt = completedAt
			},
		},
		{
			name: "CRASH A sample completes after delete",
			mutate: func(ev *Evidence) {
				completedAt := ev.CrashAAt.Add(time.Nanosecond)
				ev.PostCrashProcessSamples[0].CompletedAt = completedAt
				ev.CrashAProcessAuthorization.SampleCompletedAt = completedAt
				ev.CrashBProcessAuthorization.SampleCompletedAt = completedAt
			},
		},
		{
			name: "CRASH A authorization completion does not bind sample",
			mutate: func(ev *Evidence) {
				ev.CrashAProcessAuthorization.SampleCompletedAt =
					ev.PostCrashProcessSamples[0].CompletedAt.Add(time.Nanosecond)
			},
		},
		{
			name: "CRASH B authorization has wrong sample index",
			mutate: func(ev *Evidence) {
				ev.CrashBProcessAuthorization.SampleIndex = 1
			},
		},
		{
			name: "CRASH B authorization is missing",
			mutate: func(ev *Evidence) {
				ev.CrashBProcessAuthorization = ProcessDeleteAuthorization{}
			},
		},
		{
			name: "process probe is in flight at CRASH B",
			mutate: func(ev *Evidence) {
				inFlight := ev.PostCrashProcessSamples[0]
				inFlight.ObservedAt = ev.CrashBAt.Add(-10 * time.Millisecond)
				inFlight.CompletedAt = ev.CrashBAt.Add(10 * time.Millisecond)
				ev.PostCrashProcessSamples = append(ev.PostCrashProcessSamples, inFlight)
			},
		},
		{
			name: "hold is dead uppercase",
			mutate: func(ev *Evidence) {
				ev.PostCrashProcessSamples[0].HoldState = "X"
			},
		},
		{
			name: "driver is dead uppercase",
			mutate: func(ev *Evidence) {
				ev.PostCrashProcessSamples[0].DriverState = "X"
			},
		},
		{
			name: "hold is dead lowercase",
			mutate: func(ev *Evidence) {
				ev.PostCrashProcessSamples[0].HoldState = "x"
			},
		},
		{
			name: "driver is dead lowercase",
			mutate: func(ev *Evidence) {
				ev.PostCrashProcessSamples[0].DriverState = "x"
			},
		},
		{
			name: "hold is stopped",
			mutate: func(ev *Evidence) {
				ev.PostCrashProcessSamples[0].HoldState = "T"
			},
		},
		{
			name: "driver is stopped",
			mutate: func(ev *Evidence) {
				ev.PostCrashProcessSamples[0].DriverState = "T"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := passingEvidence()
			test.mutate(&evidence)

			verdict := Evaluate(evidence)
			if verdict.Pass1NoDoubleSpawn || verdict.Overall {
				t.Fatalf("adversarial process evidence passed: %+v", verdict)
			}
			if !strings.Contains(verdict.Pass1Reason, "crash-window") {
				t.Fatalf("PASS-1 reason %q does not identify crash-window evidence", verdict.Pass1Reason)
			}
		})
	}
}

func TestEvaluateRejectsAdversarialPass8CrossBindings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
		want   string
	}{
		{
			name: "target step key drifts",
			mutate: func(ev *Evidence) {
				ev.CrashASafety.Target.AgentStep.StepKey = "root/other#0"
			},
			want: "crash target step key drifted",
		},
		{
			name: "target idempotency key drifts",
			mutate: func(ev *Evidence) {
				ev.CrashBSafety.Target.DerivedSpawn.IdempotencyKey = "forged"
			},
			want: "crash target idempotency key drifted",
		},
		{
			name: "CRASH A preflight overlaps initial gate",
			mutate: func(ev *Evidence) {
				start := ev.InitialPreflight.FluxSourcesEnd.GitRepositories.ObservedAt
				ev.CrashASafety.ImmediatePreflight.FluxSourcesStart.GitRepositoriesOpenedAt = start
				ev.CrashASafety.ImmediatePreflight.FluxSourcesStart.ObservedAt = start
			},
			want: "CRASH A preflight did not begin after initial gate",
		},
		{
			name: "final preflight overlaps CRASH B",
			mutate: func(ev *Evidence) {
				ev.FinalPreflight.FluxSourcesStart.GitRepositoriesOpenedAt = ev.CrashBAt
				ev.FinalPreflight.FluxSourcesStart.ObservedAt = ev.CrashBAt
			},
			want: "final preflight did not begin after CRASH B",
		},
		{
			name: "crashes reuse lease request",
			mutate: func(ev *Evidence) {
				requestID := ev.CrashASafety.LeaseAcquired.RequestID
				ev.CrashBSafety.LeaseAcquired.RequestID = requestID
				ev.CrashBSafety.LeaseRenewed.RequestID = requestID
			},
			want: "both crashes reused lease request_id",
		},
		{
			name: "operator changes before CRASH A",
			mutate: func(ev *Evidence) {
				ev.CrashASafety.ImmediatePreflight.Operator.Name = "operator-unplanned"
				ev.CrashASafety.ImmediatePreflight.Operator.UID = "operator-unplanned-uid"
				ev.CrashASafety.ImmediatePreflight.AuthorityPlane.Operator.PodName = "operator-unplanned"
				ev.CrashASafety.ImmediatePreflight.AuthorityPlane.Operator.PodUID = "operator-unplanned-uid"
				ev.CrashASafety.ImmediatePreflight.EffectivePolicyAuthority = ev.CrashASafety.ImmediatePreflight.AuthorityPlane.Operator
				ev.CrashASafety.ImmediatePreflight.Quiescence.OperatorAuthority = ev.CrashASafety.ImmediatePreflight.AuthorityPlane.Operator
			},
			want: "workload pod identity changed before CRASH A",
		},
		{
			name: "mobile-hud changes before CRASH B",
			mutate: func(ev *Evidence) {
				ev.CrashBSafety.ImmediatePreflight.Hud.Name = "hud-unplanned"
				ev.CrashBSafety.ImmediatePreflight.Hud.UID = "hud-unplanned-uid"
			},
			want: "mobile-hud changed unexpectedly before CRASH B",
		},
		{
			name: "operator replacement drifts before final gate",
			mutate: func(ev *Evidence) {
				ev.FinalPreflight.Operator.Name = "operator-third-incarnation"
				ev.FinalPreflight.Operator.UID = "operator-third-uid"
				ev.FinalPreflight.AuthorityPlane.Operator.PodName = "operator-third-incarnation"
				ev.FinalPreflight.AuthorityPlane.Operator.PodUID = "operator-third-uid"
				ev.FinalPreflight.EffectivePolicyAuthority = ev.FinalPreflight.AuthorityPlane.Operator
				ev.FinalPreflight.Quiescence.OperatorAuthority = ev.FinalPreflight.AuthorityPlane.Operator
			},
			want: "planned replacements did not persist through final gate",
		},
		{
			name: "replacement start time is missing",
			mutate: func(ev *Evidence) {
				ev.CrashAReplacement.StartedAt = time.Time{}
				ev.CrashBSafety.ImmediatePreflight.Operator.StartedAt = time.Time{}
				ev.FinalPreflight.Operator.StartedAt = time.Time{}
			},
			want: "replacement identity: incomplete identity",
		},
		{
			name: "replacement coherently predates CRASH A",
			mutate: func(ev *Evidence) {
				startedAt := ev.CrashAAt.Add(-replacementStartMaxClockSkew - time.Nanosecond)
				ev.CrashAReplacement.StartedAt = startedAt
				ev.CrashBSafety.ImmediatePreflight.Operator.StartedAt = startedAt
				ev.FinalPreflight.Operator.StartedAt = startedAt
			},
			want: "replacement start time",
		},
		{
			name: "serialized crash preflight uses RollingUpdate",
			mutate: func(ev *Evidence) {
				ev.CrashASafety.ImmediatePreflight.OperatorDeployment.Strategy = "RollingUpdate"
			},
			want: "stable singleton",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := passingEvidence()
			if baseline := Evaluate(evidence); !baseline.Pass8CrashSafety {
				t.Fatalf("passing fixture failed PASS-8 before mutation: %+v", baseline)
			}
			test.mutate(&evidence)

			verdict := Evaluate(evidence)
			if verdict.Pass8CrashSafety || verdict.Overall {
				t.Fatalf("adversarial PASS-8 evidence passed: %+v", verdict)
			}
			if !strings.Contains(verdict.Pass8Reason, test.want) {
				t.Fatalf("PASS-8 reason = %q, want substring %q", verdict.Pass8Reason, test.want)
			}
		})
	}
}
