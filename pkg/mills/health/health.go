package health

import "github.com/crb2nu/loom/pkg/mills/sigfp"

// ClassifyEvidence exposes promoted signature classifications to the health
// path while keeping signature recognition centralized in sigfp.
func ClassifyEvidence(evidence string) (sigfp.Classification, bool) {
	return sigfp.ClassifyCurated(evidence)
}
