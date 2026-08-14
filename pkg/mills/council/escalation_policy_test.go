package council

import (
	"testing"
)

func TestDecideEscalation_RetriesWithinBudgets(t *testing.T) {
	policy := EscalationPolicy{MaxAttempts: 3, TransientRetryCap: 2}
	cases := []struct {
		name string
		ctx  EscalationContext
	}{
		{
			name: "transient uses total attempt budget",
			ctx: EscalationContext{
				FailureClass: EscalationFailureTransient,
				Attempts:     4,
			},
		},
		{
			name: "code uses effective attempt budget",
			ctx: EscalationContext{
				FailureClass:      EscalationFailureCode,
				Attempts:          99,
				EffectiveAttempts: 2,
			},
		},
		{
			name: "infra uses effective attempt budget",
			ctx: EscalationContext{
				FailureClass:      EscalationFailureInfrastructure,
				EffectiveAttempts: 2,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideEscalation(policy, tc.ctx)
			if got.Action != EscalationActionRetry {
				t.Fatalf("Action = %q, want retry: %+v", got.Action, got)
			}
		})
	}
}

func TestDecideEscalation_TerminalConfigEscalatesImmediately(t *testing.T) {
	got := DecideEscalation(EscalationPolicy{MaxAttempts: 3}, EscalationContext{
		FailureClass: EscalationFailureConfiguration,
		Attempts:     1,
	})
	if got.Action != EscalationActionHumanEscalate {
		t.Fatalf("Action = %q, want human_escalate", got.Action)
	}
	if got.Class != EscalationFailureConfiguration {
		t.Fatalf("Class = %q, want configuration", got.Class)
	}
}

func TestDecideEscalation_TransientCapCanAutoRetryRun(t *testing.T) {
	policy := EscalationPolicy{MaxAttempts: 3, TransientRetryCap: 2, EscalationAutoRetryCap: 2}
	got := DecideEscalation(policy, EscalationContext{
		FailureClass:       EscalationFailureTransientQuota,
		Attempts:           5,
		PriorAutoRetryRuns: 1,
	})
	if got.Action != EscalationActionAutoRetryRun {
		t.Fatalf("Action = %q, want auto_retry_run: %+v", got.Action, got)
	}
}

func TestDecideEscalation_TransientCapEscalatesWhenAutoRetryCapExhausted(t *testing.T) {
	policy := EscalationPolicy{MaxAttempts: 3, TransientRetryCap: 2, EscalationAutoRetryCap: 2}
	got := DecideEscalation(policy, EscalationContext{
		FailureClass:       EscalationFailureTransient,
		Attempts:           5,
		PriorAutoRetryRuns: 2,
	})
	if got.Action != EscalationActionHumanEscalate {
		t.Fatalf("Action = %q, want human_escalate: %+v", got.Action, got)
	}
}

func TestDecideEscalation_CodeAndInfraEscalateAtBudget(t *testing.T) {
	policy := EscalationPolicy{MaxAttempts: 3, TransientRetryCap: 5, EscalationAutoRetryCap: 10}
	for _, class := range []EscalationFailureClass{EscalationFailureCode, EscalationFailureInfrastructure} {
		t.Run(string(class), func(t *testing.T) {
			got := DecideEscalation(policy, EscalationContext{
				FailureClass:      class,
				EffectiveAttempts: 3,
			})
			if got.Action != EscalationActionHumanEscalate {
				t.Fatalf("Action = %q, want human_escalate: %+v", got.Action, got)
			}
		})
	}
}

func TestDecideEscalation_UnknownClassFailsClosedToCode(t *testing.T) {
	got := DecideEscalation(EscalationPolicy{MaxAttempts: 3}, EscalationContext{
		FailureClass:      EscalationFailureClass("surprise"),
		EffectiveAttempts: 3,
	})
	if got.Class != EscalationFailureCode {
		t.Fatalf("Class = %q, want code", got.Class)
	}
	if got.Action != EscalationActionHumanEscalate {
		t.Fatalf("Action = %q, want human_escalate", got.Action)
	}
}

func TestDecideEscalation_DefaultPolicyNormalizesZeroValues(t *testing.T) {
	got := DecideEscalation(EscalationPolicy{}, EscalationContext{
		FailureClass: EscalationFailureTransient,
		Attempts:     7,
	})
	if got.Action != EscalationActionRetry {
		t.Fatalf("Action = %q, want retry under default total cap 8", got.Action)
	}

	got = DecideEscalation(EscalationPolicy{}, EscalationContext{
		FailureClass: EscalationFailureTransient,
		Attempts:     8,
	})
	if got.Action != EscalationActionHumanEscalate {
		t.Fatalf("Action = %q, want human_escalate once default cap is reached", got.Action)
	}
}
