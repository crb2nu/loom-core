package audit

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// DigestIssue is the narrow issue projection used by the one-shot advisory
// sweep. The selector deliberately requires every stable digest identifier;
// age and a label alone are not enough to authorize a close.
type DigestIssue struct {
	IID         int64
	Title       string
	Description string
	Author      string
	State       string
	Labels      []string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// SweepReport is the reviewed output that authorizes the second, mutating
// phase of an advisory sweep. Issues is the complete allowlist for execution.
type SweepReport struct {
	ID     string
	Author string
	Issues []DigestIssue
}

// SweepApproval records the explicit human decision for one report. Both the
// report identifier and approver are required so an approval cannot be reused
// for another sweep or produced anonymously.
type SweepApproval struct {
	SweepID    string
	ApprovedBy string
	Approved   bool
}

// SweepExecutionClient is the mutation surface for an approved report.
// GetIssue must fetch current tracker state; execution calls it immediately
// before commenting on each issue.
type SweepExecutionClient interface {
	GetIssue(context.Context, int64) (DigestIssue, error)
	CommentIssue(context.Context, int64, string) error
	CloseIssue(context.Context, int64) error
}

// SweepExecutionResult makes partial progress explicit when execution stops.
type SweepExecutionResult struct {
	Closed []int64
}

// DigestIssueClient is the GitLab surface required by SweepAuditAdvisories.
// Implementations must return the complete open issue set (including all
// pages) so selection finishes before the first mutation is attempted.
type DigestIssueClient interface {
	ListOpenIssues(context.Context) ([]DigestIssue, error)
	CloseIssue(context.Context, int64) error
}

// AdvisorySweepOptions configures a single stale digest sweep. Apply and its
// compatibility alias Execute are false by default, so zero-value mode is a
// dry run.
type AdvisorySweepOptions struct {
	Now        time.Time
	StaleAfter time.Duration
	Cutoff     time.Time
	Author     string
	Apply      bool
	// Confirm is the operator-facing spelling for authorizing mutations.
	Confirm bool
	// Emitter receives exactly one aggregate record for a confirmed sweep.
	Emitter SweepEmitter
	// Execute is a compatibility alias for Apply.
	Execute bool
}

// DigestStatus reports age and the conservative recent-modification signal.
type DigestStatus struct {
	DigestIssue
	Age              time.Duration
	RecentlyModified bool
}

// AdvisorySweepResult includes the complete digest census and the narrower
// stale selection used by the existing execution path.
type AdvisorySweepResult struct {
	Digests  []DigestStatus
	Cutoff   time.Time
	Report   SweepReport
	Selected []DigestIssue
	Closed   []int64
}

// SweepAuditAdvisories selects stale bot-authored audit digest issues and,
// only when Apply or Execute is true, closes them. Selection completes before
// mutation.
// A close failure stops the sweep and reports the successful prefix, allowing
// an operator to safely rerun the idempotent operation.
func SweepAuditAdvisories(ctx context.Context, client DigestIssueClient, opts AdvisorySweepOptions) (result AdvisorySweepResult, retErr error) {
	if client == nil {
		return result, errors.New("audit advisory sweep: issue client is required")
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.Author == "" {
		opts.Author = AuditAdvisoryDigestAuthor
	}
	if strings.TrimSpace(opts.Author) == "" {
		return result, errors.New("audit advisory sweep: digest author is required")
	}
	if opts.Cutoff.IsZero() {
		if opts.StaleAfter == 0 {
			opts.StaleAfter = AuditAdvisoryDefaultStaleAfter
		}
		if opts.StaleAfter < 7*24*time.Hour {
			return result, errors.New("audit advisory sweep: older-than must be at least 7d")
		}
		result.Cutoff = opts.Now.UTC().Add(-opts.StaleAfter)
	} else {
		result.Cutoff = opts.Cutoff.UTC()
		if opts.Now.UTC().Sub(result.Cutoff) < 7*24*time.Hour {
			return result, errors.New("audit advisory sweep: cutoff must be at least 7d old")
		}
	}
	confirmed := opts.Apply || opts.Execute || opts.Confirm
	if opts.Confirm && opts.Emitter == nil {
		return result, errors.New("audit advisory sweep: audit emitter is required for confirmed sweeps")
	}
	if confirmed && opts.Emitter != nil {
		defer func() {
			event := SweepEvent{Cutoff: result.Cutoff, Confirmed: true, Closed: append([]int64(nil), result.Closed...)}
			for _, issue := range result.Selected {
				event.Selected = append(event.Selected, issue.IID)
			}
			if retErr != nil {
				event.Error = retErr.Error()
			}
			opts.Emitter.EmitSweep(ctx, event)
		}()
	}

	issues, err := client.ListOpenIssues(ctx)
	if err != nil {
		return result, fmt.Errorf("audit advisory sweep: list issues: %w", err)
	}
	for _, issue := range issues {
		if issue.State == "opened" {
			for _, label := range issue.Labels {
				if label == AuditAdvisoryDigestLabel {
					result.Digests = append(result.Digests, DigestStatus{DigestIssue: issue, Age: opts.Now.Sub(issue.CreatedAt), RecentlyModified: issue.UpdatedAt.IsZero() || !issue.UpdatedAt.Before(opts.Now.Add(-24*time.Hour))})
					break
				}
			}
		}
		if isStaleDigest(issue, opts.Author, result.Cutoff) {
			result.Selected = append(result.Selected, issue)
		}
	}
	result.Report = buildSweepReport(opts.Author, result.Cutoff, result.Selected)
	if !confirmed {
		return result, nil
	}
	for _, issue := range result.Selected {
		if err := client.CloseIssue(ctx, issue.IID); err != nil {
			return result, fmt.Errorf("audit advisory sweep: close issue %d: %w", issue.IID, err)
		}
		result.Closed = append(result.Closed, issue.IID)
	}
	return result, nil
}

func buildSweepReport(author string, cutoff time.Time, issues []DigestIssue) SweepReport {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s", author, cutoff.Format(time.RFC3339Nano))
	for _, issue := range issues {
		_, _ = fmt.Fprintf(h, "\x00%d", issue.IID)
	}
	return SweepReport{
		ID:     fmt.Sprintf("audit-advisory-%x", h.Sum(nil)[:8]),
		Author: author,
		Issues: append([]DigestIssue(nil), issues...),
	}
}

func isStaleDigest(issue DigestIssue, author string, cutoff time.Time) bool {
	return !issue.CreatedAt.IsZero() && issue.CreatedAt.Before(cutoff) && IsAuditAdvisoryDigest(issue, author)
}

// ExecuteApprovedSweep closes the issues allowlisted by an approved report.
// It stops at the first refusal or tracker error and returns the successfully
// closed prefix. A comment naming the sweep is always posted before closure.
func ExecuteApprovedSweep(ctx context.Context, client SweepExecutionClient, report SweepReport, approval SweepApproval, now time.Time) (SweepExecutionResult, error) {
	var result SweepExecutionResult
	if client == nil {
		return result, errors.New("audit advisory sweep execute: issue client is required")
	}
	report.ID = strings.TrimSpace(report.ID)
	report.Author = strings.TrimSpace(report.Author)
	if report.ID == "" || report.Author == "" {
		return result, errors.New("audit advisory sweep execute: report id and author are required")
	}
	if !approval.Approved || strings.TrimSpace(approval.ApprovedBy) == "" || strings.TrimSpace(approval.SweepID) != report.ID {
		return result, errors.New("audit advisory sweep execute: valid human approval for this report is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	seen := make(map[int64]struct{}, len(report.Issues))
	for _, member := range report.Issues {
		if member.IID <= 0 {
			return result, errors.New("audit advisory sweep execute: report contains an invalid issue")
		}
		if _, duplicate := seen[member.IID]; duplicate {
			return result, fmt.Errorf("audit advisory sweep execute: report contains duplicate issue %d", member.IID)
		}
		seen[member.IID] = struct{}{}
	}

	for _, member := range report.Issues {
		issue, err := client.GetIssue(ctx, member.IID)
		if err != nil {
			return result, fmt.Errorf("audit advisory sweep execute: revalidate issue %d: %w", member.IID, err)
		}
		if issue.IID != member.IID || !IsAuditAdvisoryDigest(issue, report.Author) {
			return result, fmt.Errorf("audit advisory sweep execute: issue %d no longer belongs to sweep %s", member.IID, report.ID)
		}
		if issue.State != "opened" {
			continue // A retry after a partial batch is safe.
		}
		if issue.UpdatedAt.IsZero() || !issue.UpdatedAt.Before(now.Add(-24*time.Hour)) {
			return result, fmt.Errorf("audit advisory sweep execute: issue %d was modified within the preceding 24 hours", member.IID)
		}
		comment := fmt.Sprintf("Closing stale audit-advisory digest approved by %s (sweep %s).", strings.TrimSpace(approval.ApprovedBy), report.ID)
		if err := client.CommentIssue(ctx, member.IID, comment); err != nil {
			return result, fmt.Errorf("audit advisory sweep execute: comment on issue %d: %w", member.IID, err)
		}
		if err := client.CloseIssue(ctx, member.IID); err != nil {
			return result, fmt.Errorf("audit advisory sweep execute: close issue %d: %w", member.IID, err)
		}
		result.Closed = append(result.Closed, member.IID)
	}
	return result, nil
}

// ParseAdvisorySweepFlags resolves the operator-facing flags. Mutations remain
// disabled unless -apply is present. An omitted cutoff uses the package age
// default relative to now.
func ParseAdvisorySweepFlags(args []string, now time.Time) (AdvisorySweepOptions, error) {
	fs := flag.NewFlagSet("audit-advisory-sweep", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "report open digests without mutations")
	apply := fs.Bool("apply", false, "close the selected issues (default: dry-run)")
	cutoffText := fs.String("cutoff", "", "select issues created strictly before RFC3339 time")
	author := fs.String("author", AuditAdvisoryDigestAuthor, "exact digest bot username")
	if err := fs.Parse(args); err != nil {
		return AdvisorySweepOptions{}, err
	}
	if *dryRun && *apply {
		return AdvisorySweepOptions{}, errors.New("audit advisory sweep: --dry-run and --apply are mutually exclusive")
	}
	if fs.NArg() != 0 {
		return AdvisorySweepOptions{}, fmt.Errorf("audit advisory sweep: unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	opts := AdvisorySweepOptions{Now: now, StaleAfter: AuditAdvisoryDefaultStaleAfter, Author: *author, Apply: *apply}
	if *cutoffText != "" {
		cutoff, err := time.Parse(time.RFC3339, *cutoffText)
		if err != nil {
			return AdvisorySweepOptions{}, fmt.Errorf("audit advisory sweep: invalid -cutoff: %w", err)
		}
		opts.Cutoff = cutoff
	}
	return opts, nil
}
