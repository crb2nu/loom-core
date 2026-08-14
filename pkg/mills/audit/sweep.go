package audit

import (
	"context"
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
	// Execute is a compatibility alias for Apply.
	Execute bool
}

// AdvisorySweepResult records the stable selection and any successfully
// closed issue IIDs. Selected is populated in both dry-run and execute modes.
type AdvisorySweepResult struct {
	Cutoff   time.Time
	Selected []DigestIssue
	Closed   []int64
}

// SweepAuditAdvisories selects stale bot-authored audit digest issues and,
// only when Apply or Execute is true, closes them. Selection completes before
// mutation.
// A close failure stops the sweep and reports the successful prefix, allowing
// an operator to safely rerun the idempotent operation.
func SweepAuditAdvisories(ctx context.Context, client DigestIssueClient, opts AdvisorySweepOptions) (AdvisorySweepResult, error) {
	var result AdvisorySweepResult
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
		if opts.StaleAfter < 0 {
			return result, errors.New("audit advisory sweep: stale-after must be positive")
		}
		result.Cutoff = opts.Now.UTC().Add(-opts.StaleAfter)
	} else {
		result.Cutoff = opts.Cutoff.UTC()
	}

	issues, err := client.ListOpenIssues(ctx)
	if err != nil {
		return result, fmt.Errorf("audit advisory sweep: list issues: %w", err)
	}
	for _, issue := range issues {
		if isStaleDigest(issue, opts.Author, result.Cutoff) {
			result.Selected = append(result.Selected, issue)
		}
	}
	if !opts.Apply && !opts.Execute {
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

func isStaleDigest(issue DigestIssue, author string, cutoff time.Time) bool {
	return !issue.CreatedAt.IsZero() && issue.CreatedAt.Before(cutoff) && IsAuditAdvisoryDigest(issue, author)
}

// ParseAdvisorySweepFlags resolves the operator-facing flags. Mutations remain
// disabled unless -apply is present. An omitted cutoff uses the package age
// default relative to now.
func ParseAdvisorySweepFlags(args []string, now time.Time) (AdvisorySweepOptions, error) {
	fs := flag.NewFlagSet("audit-advisory-sweep", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	apply := fs.Bool("apply", false, "close the selected issues (default: dry-run)")
	cutoffText := fs.String("cutoff", "", "select issues created strictly before RFC3339 time")
	author := fs.String("author", AuditAdvisoryDigestAuthor, "exact digest bot username")
	if err := fs.Parse(args); err != nil {
		return AdvisorySweepOptions{}, err
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
