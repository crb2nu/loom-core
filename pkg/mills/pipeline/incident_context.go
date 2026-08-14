package pipeline

import "github.com/crb2nu/loom/pkg/mills/council"

// IncidentContextFromFailureClassification projects a live pipeline failure
// classification into the council planning context shape. This is the input
// plumbing counterpart to persisted ci_watch summaries.
func IncidentContextFromFailureClassification(source string, classification FailureClassification) council.IncidentContext {
	ctx := council.IncidentContext{
		Source:               source,
		Classifier:           classification.Classifier,
		FailureClass:         string(classification.Class),
		ExternalDependencyID: classification.ExternalDependencyID,
		ExternalDependency:   classification.ExternalDependency,
		Retryable:            boolPtr(classification.Retryable),
		FreeRetry:            boolPtr(classification.FreeRetry),
		Terminal:             boolPtr(classification.Terminal),
	}
	return council.NormalizeIncidentContext(ctx)
}

// IncidentContextsFromFailureClassifications converts multiple pipeline
// classifications while preserving their order.
func IncidentContextsFromFailureClassifications(source string, classifications []FailureClassification) []council.IncidentContext {
	if len(classifications) == 0 {
		return nil
	}
	out := make([]council.IncidentContext, 0, len(classifications))
	for _, classification := range classifications {
		out = append(out, IncidentContextFromFailureClassification(source, classification))
	}
	return out
}

func boolPtr(v bool) *bool {
	return &v
}
