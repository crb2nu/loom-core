package pipeline

import (
	"errors"
	"strings"

	"github.com/crb2nu/loom/pkg/mills/guard"
)

// KnownFailureSignature reports whether any live classifier already explains a
// piece of failure evidence: the structured CI failure_reason matcher, the
// hand-mined external-incident signatures, the external-dependency taxonomy in
// pkg/mills/guard, or the incident-code table in pkg/mcperror.
//
// It exists so the reconciler's signature-candidate miner can skip the failures
// the factory already understands without re-implementing (and drifting from)
// the classifier corpus. Every arm delegates to the real classifier; adding a
// signature anywhere in that corpus narrows this predicate automatically.
//
// Note what it does NOT report: ClassifyFailure fails closed to FailureCode for
// unrecognized text, so "has a class" is true of everything and would be a
// useless predicate. Only a positive MATCH — a structured reason, or a named
// external dependency — counts as explained.
func KnownFailureSignature(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	if _, ok := ClassifyCIFailureSignature(text); ok {
		return true
	}
	if _, _, ok := classifyObservedExternalIncident(text); ok {
		return true
	}
	if _, ok := guard.ClassifyFailure(text); ok {
		return true
	}
	// ClassifyFailureRecord runs the mcperror incident-code table as its last
	// arm and reports the match through ExternalDependencyID; going through the
	// record keeps this predicate in step with any future arm added there.
	return ClassifyFailureRecord(errors.New(text)).ExternalDependencyID != ""
}
