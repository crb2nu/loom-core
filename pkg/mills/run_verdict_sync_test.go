package mills_test

import (
	"testing"

	mills "github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/mergequeue"
)

// The merge-queue processor duplicates the settle-correction kind as a literal
// (import direction: pkg/mills imports mergequeue, so mergequeue cannot import
// the canonical constant). A drift between the two silently drops queue-settle
// corrections from bulk discounting — this external-package test pins the pair.
func TestMergeQueueSettledVerdictKindInSync(t *testing.T) {
	if mills.RunVerdictKindMergeQueueSettled != mergequeue.RunVerdictKindMergeQueueSettled {
		t.Fatalf("verdict kind drift: mills=%q mergequeue=%q",
			mills.RunVerdictKindMergeQueueSettled, mergequeue.RunVerdictKindMergeQueueSettled)
	}
	for _, kind := range mills.RunVerdictCorrectionKinds() {
		if kind == mills.RunVerdictKindMergeQueueSettled {
			return
		}
	}
	t.Fatalf("RunVerdictKindMergeQueueSettled missing from RunVerdictCorrectionKinds")
}
