package audit

import (
	"context"
	"fmt"
)

// DigestSupersessionIssuer is the OPTIONAL capability the Followup writer uses
// right after it opens a new daily digest: it retires the newest still-open
// prior digest so the open `audit-digest` pile stops growing by one issue per
// day. It is kept separate from DigestIssuer so issuers that only support the
// append-only filing path keep working unchanged; capability detection is a
// type assertion in Followup.recordToDigest, fail-open like the rest of the
// digest path.
//
// Production satisfies it with *clients.GitLabClient (ListIssues-backed
// FindPreviousOpenAuditDigest, notes-backed IssueHasComment/CommentIssue, and
// CloseIssue); the operator build carries the compile-time guard.
type DigestSupersessionIssuer interface {
	// FindPreviousOpenAuditDigest returns the newest open digest whose period
	// is strictly before currentPeriod ("YYYY-MM-DD", UTC). found=false with a
	// nil error means nothing is left to supersede.
	FindPreviousOpenAuditDigest(ctx context.Context, currentPeriod string) (refIID int64, found bool, err error)
	// IssueHasComment reports whether the issue already carries a note whose
	// body exactly equals body — the idempotency probe for a retried close.
	IssueHasComment(ctx context.Context, iid int64, body string) (bool, error)
	CommentIssue(ctx context.Context, iid int64, body string) error
	CloseIssue(ctx context.Context, iid int64) error
}

// SupersedePreviousDigest comments "superseded by #<newIID>" on the newest open
// digest older than currentPeriod and closes it. Only that one digest is
// touched: older strays (a failed close, or the pile from before supersession
// shipped) are left for the operator's confirm-gated `loom audit-advisory-sweep`
// (pkg/mills/audit/sweep.go), so an unattended filing can never mass-close
// issues.
//
// Every step is idempotent: a closed digest disappears from the lookup, and a
// retry after a failed close finds the exact note before posting it again.
func SupersedePreviousDigest(ctx context.Context, issuer DigestSupersessionIssuer, currentPeriod string, newIID int64) error {
	if issuer == nil || newIID == 0 {
		return nil
	}
	priorIID, found, err := issuer.FindPreviousOpenAuditDigest(ctx, currentPeriod)
	if err != nil || !found || priorIID == 0 || priorIID == newIID {
		return err
	}
	comment := supersessionComment(newIID)
	hasComment, err := issuer.IssueHasComment(ctx, priorIID, comment)
	if err != nil {
		return err
	}
	if !hasComment {
		if err := issuer.CommentIssue(ctx, priorIID, comment); err != nil {
			return err
		}
	}
	return issuer.CloseIssue(ctx, priorIID)
}

// supersessionComment is the stable note body posted on a superseded digest.
// It doubles as the retry marker IssueHasComment matches exactly, so its text
// must not vary between runs.
func supersessionComment(newIID int64) string {
	return fmt.Sprintf("superseded by #%d", newIID)
}
