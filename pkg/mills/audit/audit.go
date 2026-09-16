package audit

import (
	"context"
	"regexp"
	"time"
)

// StampWriteDecision is the stable outcome recorded for every attempted stamp
// write.
type StampWriteDecision string

const (
	StampWriteAllowed  StampWriteDecision = "allowed"
	StampWriteRejected StampWriteDecision = "rejected"
)

// StampWriteEvent is the structured audit record emitted once at the stamp
// write boundary, whether the attempt succeeds or is rejected.
type StampWriteEvent struct {
	SourceProject string             `json:"source_project"`
	TargetProject string             `json:"target_project"`
	StampID       string             `json:"stamp_id"`
	Decision      StampWriteDecision `json:"decision"`
	Reason        string             `json:"reason"`
}

// StampWriteEmitter receives stamp-write audit records.
type StampWriteEmitter interface {
	EmitStampWrite(context.Context, StampWriteEvent)
}

// StampWriteEmitterFunc adapts a function to StampWriteEmitter.
type StampWriteEmitterFunc func(context.Context, StampWriteEvent)

func (f StampWriteEmitterFunc) EmitStampWrite(ctx context.Context, event StampWriteEvent) {
	f(ctx, event)
}

// SweepEvent is the single aggregate audit record emitted for a confirmed
// stale-advisory sweep. It deliberately records the successful prefix and the
// terminal error rather than emitting one record per issue.
type SweepEvent struct {
	Cutoff    time.Time `json:"cutoff"`
	Selected  []int64   `json:"selected"`
	Closed    []int64   `json:"closed"`
	Error     string    `json:"error,omitempty"`
	Confirmed bool      `json:"confirmed"`
}

// SweepEmitter receives aggregate stale-advisory sweep audit records.
type SweepEmitter interface {
	EmitSweep(context.Context, SweepEvent)
}

// SweepEmitterFunc adapts a function to SweepEmitter.
type SweepEmitterFunc func(context.Context, SweepEvent)

func (f SweepEmitterFunc) EmitSweep(ctx context.Context, event SweepEvent) {
	f(ctx, event)
}

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

// NormalizeDigestBody ignores only the generated date boilerplate and marker.
func NormalizeDigestBody(body string) string {
	for _, pattern := range digestDatePatterns {
		body = pattern.ReplaceAllString(body, "${1}<date>${2}")
	}
	return body
}

var digestDatePatterns = []*regexp.Regexp{
	regexp.MustCompile("(?m)^(\\*\\*Rubric:\\*\\* .* · \\*\\*Cost:\\*\\* .* · \\*\\*Recorded:\\*\\* `)[^`]+(`)$"),
	regexp.MustCompile(`(?m)^(<!-- mills-audit-digest:period=)[0-9]{4}-[0-9]{2}-[0-9]{2}( -->)$`),
	regexp.MustCompile(`(?m)^(Rolling digest of advisory audit findings with survival score below .*?, recorded on )[0-9]{4}-[0-9]{2}-[0-9]{2}( \(UTC\).)$`),
}
