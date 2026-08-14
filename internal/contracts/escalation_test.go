package contracts

import (
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestMillsEscalationFailureRecordContract(t *testing.T) {
	retryable := false
	freeRetry := false
	terminal := true
	record := pipeline.FailureRecord{
		BacklogID:     "BL-ESC-1",
		PipelineRunID: "PIPE-ESC-1",
		Reason:        "stage ci_watch terminal config error (not retried) [class=config]: gitlab: status 401: unauthorized",
		State:         store.PipelineEscalated,
		CostUSD:       0.42,
		Attempts:      1,
		Classification: &pipeline.EscalationClassification{
			Classifier:           EscalationFailureClassifierMills,
			EscalationClass:      "config",
			FailureClass:         "configuration",
			Retryable:            &retryable,
			FreeRetry:            &freeRetry,
			Terminal:             &terminal,
			ExternalDependencyID: "external_dependency.gitlab.auth_failure",
			ExternalDependency:   "gitlab",
		},
		EscalationClass:      "config",
		FailureClass:         "configuration",
		Retryable:            &retryable,
		ExternalDependencyID: "external_dependency.gitlab.auth_failure",
		ExternalDependency:   "gitlab",
		StageStack: []pipeline.FailureStage{
			{Stage: "ci_watch", Attempt: 1, Outcome: "error", CostUSD: 0.42, Duration: "1s"},
		},
		GateVerdicts: []pipeline.FailureGate{
			{Gate: "scope", AfterStage: "implement", Outcome: "pass"},
		},
		LastLogTail: "gitlab: status 401: unauthorized",
		GeneratedAt: time.Date(2026, 7, 8, 18, 30, 0, 0, time.UTC),
	}

	assertGolden(t, "escalation_failure_record", marshalIndent(t, record))
}

func TestEscalationFailureClassificationContract(t *testing.T) {
	got := EscalationFailureClassification{
		Classifier: EscalationFailureClassifierMills,
		Class:      EscalationFailureTransientQuota,
		Retryable:  true,
		FreeRetry:  true,
		Terminal:   false,
	}

	assertGolden(t, "escalation_failure_classification", marshalIndent(t, got))
}
