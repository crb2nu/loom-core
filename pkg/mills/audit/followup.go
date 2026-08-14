package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// DefaultFollowupThreshold is the survival score below which the
// Followup writer creates an advisory issue. Mirrors spec §"Audit swarm
// flow" #5: "open MR (council artifact) / open issue (pipeline merge)
// when survival_score < 0.6". v2.0 uses GitLab issues for both subjects;
// promoting council follow-ups to MRs lands in v2.1 once the operator
// plumbs the council branch ref onto the audit row.
const DefaultFollowupThreshold = 0.6

// Issuer is the surface Followup depends on. Production satisfies it
// with *clients.GitLabClient (its existing CreateIssue method).
type Issuer interface {
	CreateIssue(ctx context.Context, req pipeline.IssueRequest) (pipeline.IssueResponse, error)
}

// DigestIssuer is an OPTIONAL capability an Issuer may also implement so the
// Followup writer folds advisory findings into a single rolling per-day digest
// issue instead of opening one issue per finding. When the wired Issuer
// satisfies it, OnRecorded finds (or opens) the current UTC day's digest and
// appends each subsequent finding as a comment. An Issuer that implements only
// CreateIssue keeps the legacy one-issue-per-finding behaviour unchanged.
//
// Production satisfies it with *clients.GitLabClient (ListIssues-backed
// FindOpenAuditDigest + note-backed CommentIssue). This mirrors the escalation
// DedupIssueClient idiom (pkg/mills/pipeline/escalate.go) so the two
// issue-dedup paths stay structurally identical, and it is fail-open: any
// GitLab error degrades to opening a fresh issue rather than dropping the
// finding.
type DigestIssuer interface {
	Issuer
	// FindOpenAuditDigest returns the open digest issue for the given period
	// ("YYYY-MM-DD", UTC), matched via pipeline.AuditDigestMarker. found=false
	// with a nil error means no digest exists for that day yet.
	FindOpenAuditDigest(ctx context.Context, period string) (pipeline.IssueRef, bool, error)
	// CommentIssue appends a finding entry to an existing digest issue.
	CommentIssue(ctx context.Context, iid int64, body string) error
}

// Followup writes advisory GitLab issues when an audit row's survival
// score crosses below Threshold. It is wired to QueueWorker.OnRecorded
// in the operator so every persisted finding gets considered without
// the dispatcher needing to know about issue creation.
//
// v2.0 is intentionally non-blocking: a low score yields an issue, not
// a merge revert / artifact rewrite. Operators triage the issue and
// decide what to do. The HUD audit panel surfaces the issue link
// inline once slice 3.5 ships.
type Followup struct {
	// Issuer creates the advisory issue. nil disables the writer (the
	// operator boots without GitLab; OnRecorded becomes a no-op).
	Issuer Issuer

	// Threshold is the survival_score boundary. Findings with
	// SurvivalScore < Threshold trigger an issue; anything ≥ Threshold
	// is silently dropped. Zero falls back to DefaultFollowupThreshold.
	Threshold float64

	// Logger surfaces issue URLs + per-finding skip reasons. nil
	// discards.
	Logger *slog.Logger

	// Clock is injected for deterministic timestamps in the issue body.
	// Defaults to time.Now.
	Clock func() time.Time
}

// NewFollowup constructs a Followup writer with sensible defaults. nil
// Issuer is permitted; the resulting writer's OnRecorded is a no-op
// (logs once at boot via the operator's wiring path).
func NewFollowup(issuer Issuer) *Followup {
	return &Followup{
		Issuer:    issuer,
		Threshold: DefaultFollowupThreshold,
	}
}

// OnRecorded inspects a freshly-recorded finding. When SurvivalScore is
// strictly below Threshold (and the Issuer is wired), the audit context
// is surfaced on GitLab. Returns nil even on Issuer error so the
// QueueWorker keeps draining; errors are logged for triage.
//
// Surfacing form depends on the Issuer's capabilities:
//   - When the Issuer implements DigestIssuer (production's GitLab client),
//     the finding is folded into the current UTC day's rolling digest issue —
//     one issue per day plus a comment per finding. This is the noise-reducing
//     default and dedups every finding recorded on the same day.
//   - Otherwise the legacy path opens one advisory issue per finding. That path
//     enforces no idempotency: the same finding fired twice creates two issues
//     (v2.1 would dedup by subject_kind/subject_id/finding_id). The worker fires
//     OnRecorded once per finding under normal operation, so a duplicate is a
//     reset-after-restart scenario in practice.
func (f *Followup) OnRecorded(ctx context.Context, finding *store.AuditFinding) error {
	if f == nil || finding == nil {
		return nil
	}
	threshold := f.Threshold
	if threshold <= 0 {
		threshold = DefaultFollowupThreshold
	}
	if finding.SurvivalScore >= threshold {
		return nil // above threshold; nothing to do
	}
	if f.Issuer == nil {
		f.warn("audit/followup: skipping issue (no Issuer wired)",
			"subject_kind", string(finding.SubjectKind),
			"subject_id", finding.SubjectID,
			"survival", finding.SurvivalScore,
		)
		return nil
	}

	// Preferred path: fold the finding into the day's rolling digest when the
	// Issuer supports it. Capability detection mirrors the escalator's
	// DedupIssueClient branch so both dedup paths look the same.
	if dg, ok := f.Issuer.(DigestIssuer); ok {
		return f.recordToDigest(ctx, dg, finding)
	}

	// Legacy path: one advisory issue per finding.
	req := pipeline.IssueRequest{
		Title:       f.title(finding),
		Description: f.body(finding),
		Labels:      f.labels(finding),
	}
	resp, err := f.Issuer.CreateIssue(ctx, req)
	if err != nil {
		f.warn("audit/followup: create issue failed",
			"subject_kind", string(finding.SubjectKind),
			"subject_id", finding.SubjectID,
			"error", err,
		)
		// Best-effort: never surface to the QueueWorker.
		return nil
	}
	f.info("audit/followup: issue opened",
		"subject_kind", string(finding.SubjectKind),
		"subject_id", finding.SubjectID,
		"survival", finding.SurvivalScore,
		"iid", resp.IID,
		"url", resp.URL,
	)
	return nil
}

// digestPeriod is the UTC calendar day ("YYYY-MM-DD") used to bucket advisory
// findings into one rolling digest issue.
func (f *Followup) digestPeriod() string {
	return f.now().UTC().Format("2006-01-02")
}

// recordToDigest folds a sub-threshold finding into the current UTC day's
// rolling digest issue: it appends a comment when the digest already exists,
// otherwise it opens the digest seeded with this finding. Fail-open throughout
// — any GitLab error is logged and swallowed so the QueueWorker keeps draining,
// matching the legacy path's contract.
func (f *Followup) recordToDigest(ctx context.Context, dg DigestIssuer, finding *store.AuditFinding) error {
	period := f.digestPeriod()
	ref, found, err := dg.FindOpenAuditDigest(ctx, period)
	if err != nil {
		// Lookup failed: fall through to opening a fresh digest rather than
		// dropping the finding. A duplicate digest is cheap noise; a lost
		// advisory finding is not. Mirrors the escalator's dedup-lookup
		// fail-open.
		f.warn("audit/followup: digest lookup failed; opening a new digest",
			"period", period,
			"subject_id", finding.SubjectID,
			"error", err,
		)
		found = false
	}

	if found {
		if cerr := dg.CommentIssue(ctx, ref.IID, f.entry(finding)); cerr != nil {
			f.warn("audit/followup: append to digest failed",
				"period", period,
				"iid", ref.IID,
				"subject_id", finding.SubjectID,
				"error", cerr,
			)
			return nil
		}
		f.info("audit/followup: finding appended to digest",
			"period", period,
			"iid", ref.IID,
			"subject_kind", string(finding.SubjectKind),
			"subject_id", finding.SubjectID,
			"survival", finding.SurvivalScore,
		)
		return nil
	}

	req := pipeline.IssueRequest{
		Title:       f.digestTitle(period),
		Description: f.digestBody(period, finding),
		Labels:      f.digestLabels(),
	}
	resp, cerr := dg.CreateIssue(ctx, req)
	if cerr != nil {
		f.warn("audit/followup: open digest failed",
			"period", period,
			"subject_id", finding.SubjectID,
			"error", cerr,
		)
		return nil
	}
	f.info("audit/followup: digest opened",
		"period", period,
		"iid", resp.IID,
		"url", resp.URL,
		"subject_kind", string(finding.SubjectKind),
		"subject_id", finding.SubjectID,
		"survival", finding.SurvivalScore,
	)
	return nil
}

// digestTitle names the rolling digest issue for a UTC day.
func (f *Followup) digestTitle(period string) string {
	return fmt.Sprintf("Audit advisory digest — %s (UTC)", period)
}

// digestBody seeds a new digest issue: a short header, the dedup marker, and
// the first finding rendered as an entry. Subsequent findings are appended as
// comments carrying only the entry (see entry).
func (f *Followup) digestBody(period string, finding *store.AuditFinding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Rolling digest of advisory audit findings with survival score below `%.2f`, recorded on %s (UTC).\n",
		f.threshold(), period)
	b.WriteString("Each finding is appended below as a comment. Advisory only — v2.0 does not block on these.\n\n")
	fmt.Fprintf(&b, "%s\n\n", pipeline.AuditDigestMarker(period))
	b.WriteString("---\n\n")
	b.WriteString(f.entry(finding))
	return b.String()
}

// digestLabels tag the digest so existing `audit-followup` triage filters still
// see it while `audit-digest` lets the matcher list only digests.
func (f *Followup) digestLabels() []string {
	return []string{"audit-followup", pipeline.AuditDigestLabel}
}

// entry renders one advisory finding as a self-contained markdown block shared
// by the digest seed body and every appended comment. It carries the same
// context the legacy per-issue body did (subject, score, severity, rubric,
// auditor pool, findings) so nothing is lost by folding many issues into one.
func (f *Followup) entry(finding *store.AuditFinding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### [%s] `%s` — survival %.2f (`%s`)\n",
		strings.ToLower(string(finding.SubjectKind)),
		finding.SubjectID,
		finding.SurvivalScore,
		finding.Severity,
	)
	fmt.Fprintf(&b, "**Rubric:** `%s` · **Cost:** `$%.4f` · **Recorded:** `%s`\n\n",
		finding.RubricID, finding.CostUSD, f.now().UTC().Format(time.RFC3339))

	if len(finding.AuditorPool) > 0 {
		parts := make([]string, 0, len(finding.AuditorPool))
		for _, m := range finding.AuditorPool {
			role, _ := m["role"].(string)
			backend, _ := m["backend"].(string)
			model, _ := m["model"].(string)
			parts = append(parts, fmt.Sprintf("`%s/%s` (%s)", backend, model, role))
		}
		fmt.Fprintf(&b, "**Auditor pool:** %s\n\n", strings.Join(parts, ", "))
	}

	if len(finding.Findings) > 0 {
		for _, item := range finding.Findings {
			id, _ := item["id"].(string)
			title, _ := item["title"].(string)
			severity, _ := item["severity"].(string)
			detail, _ := item["detail"].(string)
			fmt.Fprintf(&b, "- **`%s` — %s** (`%s`)", id, title, severity)
			if detail != "" {
				fmt.Fprintf(&b, "\n  %s", detail)
			}
			b.WriteString("\n")
		}
	} else {
		b.WriteString("_No structured findings — see the survival score._\n")
	}
	return b.String()
}

// title formats the issue title. Stable + scannable in a triage list:
// the subject id is the longest variable element so we keep it last
// for prefix-matching searches.
func (f *Followup) title(finding *store.AuditFinding) string {
	return fmt.Sprintf("Audit follow-up [%s] %s — survival %.2f",
		strings.ToLower(string(finding.SubjectKind)),
		finding.SubjectID,
		finding.SurvivalScore,
	)
}

// body composes the markdown description. Links the auditor pool +
// every Finding so the triage queue sees the full picture without
// needing the audit drawer.
func (f *Followup) body(finding *store.AuditFinding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Audit rubric:** `%s`  \n", finding.RubricID)
	fmt.Fprintf(&b, "**Survival score:** `%.3f` (threshold `%.2f`)  \n", finding.SurvivalScore, f.threshold())
	fmt.Fprintf(&b, "**Severity:** `%s`  \n", finding.Severity)
	fmt.Fprintf(&b, "**Subject:** `%s` `%s`  \n", finding.SubjectKind, finding.SubjectID)
	fmt.Fprintf(&b, "**Cost:** `$%.4f`  \n", finding.CostUSD)
	fmt.Fprintf(&b, "**Recorded at:** `%s`\n\n", f.now().UTC().Format(time.RFC3339))

	if len(finding.AuditorPool) > 0 {
		b.WriteString("## Auditor pool\n\n")
		for _, m := range finding.AuditorPool {
			role, _ := m["role"].(string)
			backend, _ := m["backend"].(string)
			model, _ := m["model"].(string)
			fmt.Fprintf(&b, "- `%s` / `%s` (role: %s)\n", backend, model, role)
		}
		b.WriteString("\n")
	}

	if len(finding.Findings) > 0 {
		b.WriteString("## Findings\n\n")
		for _, item := range finding.Findings {
			id, _ := item["id"].(string)
			title, _ := item["title"].(string)
			severity, _ := item["severity"].(string)
			detail, _ := item["detail"].(string)
			fmt.Fprintf(&b, "### `%s` — %s (`%s`)\n", id, title, severity)
			if detail != "" {
				fmt.Fprintf(&b, "%s\n\n", detail)
			} else {
				b.WriteString("\n")
			}
		}
	} else {
		b.WriteString("## Findings\n\n_No structured findings — see the survival score._\n\n")
	}

	b.WriteString("---\n")
	b.WriteString("_Opened automatically by the Loom Mills audit subsystem (advisory; v2.0 does not block on this)._\n")
	return b.String()
}

// labels assemble the triage label set. We always emit `audit-followup`
// + a severity-prefixed label so triage queues can filter by severity
// without parsing the title; subject-kind variants help split the
// queue between council artifacts and pipeline merges.
func (f *Followup) labels(finding *store.AuditFinding) []string {
	out := []string{
		"audit-followup",
		"severity-" + strings.ToLower(string(finding.Severity)),
	}
	switch finding.SubjectKind {
	case store.AuditSubjectCouncilArtifact:
		out = append(out, "council-artifact")
	case store.AuditSubjectPipelineMerge:
		out = append(out, "pipeline-merge")
	}
	return out
}

func (f *Followup) threshold() float64 {
	if f == nil || f.Threshold <= 0 {
		return DefaultFollowupThreshold
	}
	return f.Threshold
}

func (f *Followup) now() time.Time {
	if f != nil && f.Clock != nil {
		return f.Clock()
	}
	return time.Now()
}

func (f *Followup) warn(msg string, kv ...any) {
	if f == nil || f.Logger == nil {
		return
	}
	f.Logger.Warn(msg, kv...)
}

func (f *Followup) info(msg string, kv ...any) {
	if f == nil || f.Logger == nil {
		return
	}
	f.Logger.Info(msg, kv...)
}

// Compile-time guard that *Followup satisfies the QueueWorker.OnRecorded
// callback shape. Catches signature drift before runtime.
var _ func(context.Context, *store.AuditFinding) error = (&Followup{}).OnRecorded

// errInvalidIssuer is reserved for a future builder that surfaces
// configuration errors at construction time. Not used today; here as
// a placeholder so test files can reference it without a follow-up
// import churn when Followup gets a stricter constructor.
var errInvalidIssuer = errors.New("audit/followup: invalid Issuer configuration")

// _ keeps errInvalidIssuer referenced so `go vet` doesn't flag it as
// unused while the constructor stays simple.
var _ = errInvalidIssuer
