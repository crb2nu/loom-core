package gates

import (
	"context"
	"strings"
)

const (
	BranchPushedReasonPresent    = "branch_pushed.present"
	BranchPushedReasonMissingRef = "branch_pushed.missing_ref"
	missingRemoteRefSignature    = "couldn't find remote ref refs/heads/"
)

// BranchPushed blocks only when cumulative git capture proves that the
// implement branch does not exist on the remote. Other capture failures are
// inconclusive infrastructure signals and remain fail-open.
type BranchPushed struct{}

func (g *BranchPushed) Name() string { return "branch_pushed" }

func (g *BranchPushed) Evaluate(_ context.Context, in StageInput) (Outcome, error) {
	if in.GitCaptureStatus == "fetch_failed" && strings.Contains(in.GitCaptureReason, missingRemoteRefSignature) {
		return fail("[" + BranchPushedReasonMissingRef + "] implement branch was never pushed to origin; " +
			"commit the changes with a configured git identity, then push with git push -u origin HEAD"), nil
	}
	return codedPass(BranchPushedReasonPresent), nil
}
