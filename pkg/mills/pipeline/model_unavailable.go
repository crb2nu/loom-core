package pipeline

import (
	"errors"
	"strings"
)

// ErrModelUnavailable marks a FlexInfer chat failure whose ONLY cause is that
// every candidate model was temporarily unservable — a 503 "service_unavailable"
// or the shared-GPU proxy's "model '…' is parked behind a higher-priority
// primary" rejection, exhausted across the whole fallback chain — as opposed to
// a model-not-found drift (code class) or a real transport outage. The
// FlexInfer client wraps it (fmt.Errorf("…: %w", ErrModelUnavailable)); the
// research stage soft-skips on it (advisory context, not load-bearing) and
// Classify maps it to ClassTransient so a still-parked model retries free
// instead of escalating as a code defect.
//
// Live evidence (2026-07-16 telemetry sweep): research had 24 stage errors in
// 7d, all `status 503 … model parked behind higher-priority primary`; one run
// retried a parked model 8×, and 3 runs escalated at the research stage — none
// of which are code defects.
var ErrModelUnavailable = errors.New("flexinfer: model unavailable")

// IsModelUnavailable reports whether err is (or wraps) the model-unavailable
// class. Used by the research stage to decide the advisory soft-skip.
func IsModelUnavailable(err error) bool {
	return err != nil && errors.Is(err, ErrModelUnavailable)
}

// researchSkipNote renders the artifact/log note recorded when the research
// stage soft-skips because every candidate model was unavailable. Single source
// of truth so the artifact, the log_tail, and tests agree on the exact text.
func researchSkipNote(err error) string {
	msg := "unknown error"
	if err != nil {
		if trimmed := strings.TrimSpace(err.Error()); trimmed != "" {
			msg = trimmed
		}
	}
	return "research skipped: model unavailable (" + msg + ")"
}

const (
	// researchSkippedArtifactKey flags a soft-skipped research stage in
	// stage_results.artifacts_json so downstream readers can tell an advisory
	// model-unavailable skip apart from a real (possibly empty) research result.
	researchSkippedArtifactKey = "research_skipped"
	// researchSkipReasonArtifactKey carries the human-readable skip note.
	researchSkipReasonArtifactKey = "research_skip_reason"
)
