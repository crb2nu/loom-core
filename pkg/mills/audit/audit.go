package audit

import "context"

// Stable audit-advisory digest identifiers. The one-time cleanup script reads
// these constants directly so its narrow selector is reviewable in the audit
// domain instead of being duplicated in shell.
const (
	AuditAdvisoryDigestLabel        = "audit-digest"
	AuditAdvisoryDigestTitlePrefix  = "Audit advisory digest — "
	AuditAdvisoryDigestTitleSuffix  = " (UTC)"
	AuditAdvisoryDigestMarkerPrefix = "<!-- mills-audit-digest:period="
	AuditAdvisoryDigestMarkerSuffix = " -->"
	// DefaultAuditAdvisoryStalenessDays is the conservative operator default;
	// callers may override it with an explicit positive window.
	DefaultAuditAdvisoryStalenessDays = 30
)

// IntakeDecision is the structured result of an intake admission decision.
type IntakeDecision string

// IntakeRejectionReason is a stable machine-readable rejection category.
type IntakeRejectionReason string

const (
	IntakeDecisionRejected IntakeDecision = "rejected"

	IntakeRejectionUnknownRepository IntakeRejectionReason = "unknown_repository"
	IntakeRejectionClassifierError   IntakeRejectionReason = "classifier_error"
)

// IntakeEvent records a fail-closed admission rejection.
type IntakeEvent struct {
	Decision IntakeDecision        `json:"decision"`
	Project  string                `json:"project,omitempty"`
	Reason   IntakeRejectionReason `json:"reason"`
}

// IntakeEmitter receives structured intake audit events.
type IntakeEmitter interface {
	EmitIntake(context.Context, IntakeEvent)
}

// IntakeEmitterFunc adapts a function to IntakeEmitter.
type IntakeEmitterFunc func(context.Context, IntakeEvent)

func (f IntakeEmitterFunc) EmitIntake(ctx context.Context, event IntakeEvent) {
	f(ctx, event)
}
