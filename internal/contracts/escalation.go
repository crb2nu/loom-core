package contracts

import "github.com/crb2nu/loom/pkg/mills/pipeline"

// EscalationFailureClassifier names the deterministic classifier that produced
// an escalation failure-class record.
type EscalationFailureClassifier = string

const (
	EscalationFailureClassifierMills EscalationFailureClassifier = pipeline.FailureClassifierName
)

// EscalationFailureClass is the wire-safe closed taxonomy attached to
// escalated pipeline records and handoff metadata.
type EscalationFailureClass = pipeline.FailureClass

const (
	EscalationFailureTransient      EscalationFailureClass = pipeline.FailureTransient
	EscalationFailureTransientQuota EscalationFailureClass = pipeline.FailureTransientQuota
	EscalationFailureInfrastructure EscalationFailureClass = pipeline.FailureInfrastructure
	EscalationFailureCode           EscalationFailureClass = pipeline.FailureCode
	EscalationFailureConfiguration  EscalationFailureClass = pipeline.FailureConfiguration
)

// EscalationFailureClassification is the JSON contract downstream planners and
// operators consume from `loom ci classify` output and policy-facing failure
// records: classifier provenance plus the full retry semantics of the class.
type EscalationFailureClassification = pipeline.FailureClassification

// EscalationRecordClassification is the grouped classification block emitted on
// escalated FailureRecord payloads, escalation events
// (pipeline.escalation.published), and handoff context under the key
// "classification".
type EscalationRecordClassification = pipeline.EscalationClassification
