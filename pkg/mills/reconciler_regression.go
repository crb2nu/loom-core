package mills

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// Durable identity of a post-merge regression attribution. Exported so the
// operator's read endpoint selects the same actor + kind the sweep writes —
// the two surfaces must never drift on a string literal.
const (
	// RegressionAttributedEventKind marks a merged MR whose work was later
	// reverted on the default branch.
	RegressionAttributedEventKind = "regression.attributed"
	// RegressionAttributionActor is the sole writer of that kind, so the read
	// path can select on actor (an indexed window scan) and filter on kind.
	RegressionAttributionActor = "reconciler.regression"
	// regressionSubjectKind keys the first-writer dedup on the REGRESSED merge
	// request: one attribution per MR, no matter how many sweeps re-observe the
	// same revert commit.
	regressionSubjectKind = "merge_request"
)

const (
	// DefaultRegressionSweepInterval is how often the sweep runs when the
	// operator sets no interval. Hourly: a revert is a rare, human-paced event
	// and each pass costs two read-only GitLab list calls.
	DefaultRegressionSweepInterval = time.Hour
	// defaultRegressionLookback bounds BOTH list calls. A revert landing more
	// than a week after the merge is outside the factory's feedback loop; the
	// attribution would arrive too late to steer anything.
	defaultRegressionLookback = 168 * time.Hour
	// defaultRegressionBranch is the branch reverts are read from when the
	// operator names none.
	defaultRegressionBranch = "main"
	// regressionMergedMRPageSize / regressionCommitPageSize bound the two list
	// calls to one page each, matching the clients' single-page contract.
	regressionMergedMRPageSize = 100
	regressionCommitPageSize   = 100
	// regressionMinSHAPrefix is the shortest revert-trailer commit id the
	// matcher will resolve. Git's own trailer carries the full 40 characters;
	// a hand-written shorter prefix is accepted only down to this length,
	// below which a collision across a repo's history stops being negligible
	// and the attribution would be a guess.
	regressionMinSHAPrefix = 12
	// regressionSweepTimeout reserves part of the tick budget for the sweep's
	// two sequential GitLab calls.
	regressionSweepTimeout = 15 * time.Second
)

// revertTrailerPattern matches Git's canonical revert trailer, which `git
// revert` writes on its own line in the commit body. Anchoring to the start of
// a line is what separates a real revert from prose that merely mentions a
// commit ("this partially reverts commit …", a quoted trailer inside a
// changelog): a fuzzy mention must never produce an attribution.
var revertTrailerPattern = regexp.MustCompile(`(?im)^[ \t]*This reverts commit ([0-9a-f]{7,40})\b`)

// MergedMRRecord is the bounded view of one merged merge request the regression
// sweep needs. LandedSHA is the commit the MR's work became on the target
// branch — the only identity a revert commit can name.
type MergedMRRecord struct {
	IID       int64
	Title     string
	LandedSHA string
	MergedAt  time.Time
}

// MergedMRLister lists the merge requests merged since a cutoff, newest first,
// bounded to one page of limit items. Satisfied by an adapter over
// clients.GitLabClient.ListMergedMergeRequests — pkg/mills cannot import
// pkg/mills/clients (the dependency runs the other way), so the projection into
// MergedMRRecord happens at the operator wiring seam.
type MergedMRLister interface {
	ListMergedMRs(ctx context.Context, since time.Time, limit int) ([]MergedMRRecord, error)
}

// BranchCommitRecord is the bounded view of one branch commit. Message is the
// FULL message: the revert trailer lives in the body, so a title-only record
// could not see a revert at all.
type BranchCommitRecord struct {
	SHA       string
	Title     string
	Message   string
	CreatedAt time.Time
}

// BranchCommitLister lists a branch's recent commits, newest first, bounded to
// one page of limit items. Satisfied by an adapter over
// clients.GitLabClient.ListBranchCommits.
type BranchCommitLister interface {
	ListBranchCommits(ctx context.Context, ref string, since time.Time, limit int) ([]BranchCommitRecord, error)
}

// RegressionSweepResult summarises one post-merge regression attribution sweep.
type RegressionSweepResult struct {
	// MergedInspected is the number of merged MRs in the window that carried a
	// usable landed SHA.
	MergedInspected int
	// CommitsScanned is the number of default-branch commits read.
	CommitsScanned int
	// Reverts is the number of revert trailers found across those commits,
	// whether or not they resolved to a merged MR.
	Reverts int
	// Attributed is the number of NEW attributions written this sweep;
	// re-observing an already-attributed revert does not count.
	Attributed int
	// Ambiguous is the number of revert trailers whose commit id was too short
	// or prefixed more than one merged MR, and so were deliberately dropped.
	Ambiguous int
	// Errored is the number of attribution writes that failed; the revert is
	// re-observed on a later sweep.
	Errored int
}

// SweepRegressionAttribution attributes post-merge regressions from the one
// piece of ground truth a repository actually records: a revert.
//
// Each pass lists the MRs merged in the lookback window and the default
// branch's commits over the same window, then attributes a regression only when
// a commit carries Git's canonical revert trailer naming a merged MR's landed
// commit. There is deliberately NO file-overlap, timing, or similarity
// heuristic: a wrong attribution teaches the factory a false lesson, and every
// downstream consumer (judge calibration, promotion evidence) reads these
// events as fact.
//
// Exactly-once per regressed MR: the attribution is a first-writer event keyed
// on (kind, subject_kind=merge_request, subject_id=<iid>), so repeated sweeps
// over the same window converge instead of accumulating duplicates.
//
// Best-effort: a nil client pair disables the sweep, list failures return an
// error the caller logs (never wedging the tick), and a failed individual write
// is counted and retried on a later pass.
func (r *Reconciler) SweepRegressionAttribution(ctx context.Context) (RegressionSweepResult, error) {
	res := RegressionSweepResult{}
	if r == nil || r.Store == nil || r.Store.Events == nil {
		return res, errors.New("reconciler: not configured")
	}
	if r.RegressionMergedMRs == nil || r.RegressionCommits == nil {
		return res, nil // sweep disabled (no GitLab client wired)
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	since := r.now().Add(-r.regressionLookback())
	merged, err := r.RegressionMergedMRs.ListMergedMRs(ctx, since, regressionMergedMRPageSize)
	if err != nil {
		if cancelErr := contextCancellationError(ctx, err); cancelErr != nil {
			return res, cancelErr
		}
		RegressionSweepErrorsTotal.WithLabelValues("list_merged").Inc()
		return res, fmt.Errorf("list merged mrs: %w", err)
	}
	landed := make(map[string]MergedMRRecord, len(merged))
	for _, mr := range merged {
		sha := strings.ToLower(strings.TrimSpace(mr.LandedSHA))
		if len(sha) < regressionMinSHAPrefix {
			continue // no provable landed identity — never guess one
		}
		mr.LandedSHA = sha
		landed[sha] = mr
	}
	res.MergedInspected = len(landed)
	if len(landed) == 0 {
		return res, nil
	}

	commits, err := r.RegressionCommits.ListBranchCommits(ctx, r.regressionBranch(), since, regressionCommitPageSize)
	if err != nil {
		if cancelErr := contextCancellationError(ctx, err); cancelErr != nil {
			return res, cancelErr
		}
		RegressionSweepErrorsTotal.WithLabelValues("list_commits").Inc()
		return res, fmt.Errorf("list branch commits: %w", err)
	}
	res.CommitsScanned = len(commits)

	for _, commit := range commits {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		for _, reverted := range revertedCommitSHAs(commit.Message) {
			res.Reverts++
			mr, ok := matchRevertedMR(landed, reverted)
			if !ok {
				// Either the reverted commit is not a mills merge in this
				// window, or the trailer's prefix is too short / matches more
				// than one merged MR. Both are non-events by design.
				if isAmbiguousRevertID(landed, reverted) {
					res.Ambiguous++
				}
				continue
			}
			appended, err := r.appendRegressionAttribution(ctx, mr, commit)
			if err != nil {
				if cancelErr := contextCancellationError(ctx, err); cancelErr != nil {
					return res, cancelErr
				}
				res.Errored++
				continue
			}
			if appended {
				res.Attributed++
				RegressionAttributionsTotal.Inc()
			}
		}
	}
	return res, nil
}

// appendRegressionAttribution writes the first-writer attribution event for one
// regressed MR. It returns (appended, err) mirroring AppendOnceBySubjectKind:
// appended is false when this MR was already attributed, which is what keeps
// the counter and any downstream consumer exactly-once across sweeps.
func (r *Reconciler) appendRegressionAttribution(ctx context.Context, mr MergedMRRecord, commit BranchCommitRecord) (bool, error) {
	appended, err := r.Store.Events.AppendOnceBySubjectKind(ctx, &store.Event{
		Actor:       RegressionAttributionActor,
		Kind:        RegressionAttributedEventKind,
		SubjectKind: regressionSubjectKind,
		SubjectID:   strconv.FormatInt(mr.IID, 10),
		Payload: map[string]any{
			"regressed_mr_iid": mr.IID,
			"merged_sha":       mr.LandedSHA,
			"revert_sha":       commit.SHA,
			"revert_title":     commit.Title,
		},
	})
	// A cancelled sweep is not an attribution failure — the caller unwinds and
	// the revert is re-observed next pass — so it neither counts nor logs.
	if err != nil && contextCancellationError(ctx, err) == nil {
		RegressionSweepErrorsTotal.WithLabelValues("append_event").Inc()
		if r.Logger != nil {
			r.Logger.Warn("reconciler: append regression attribution failed",
				"mr_iid", mr.IID, "revert_sha", commit.SHA, "error", err)
		}
	}
	return appended, err
}

// revertedCommitSHAs returns the commit ids named by Git revert trailers in a
// commit message, lowercased and de-duplicated in order. A message with no
// trailer yields nothing — a prose mention of a SHA is not a revert.
func revertedCommitSHAs(message string) []string {
	if message == "" {
		return nil
	}
	matches := revertTrailerPattern.FindAllStringSubmatch(message, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		sha := strings.ToLower(m[1])
		if seen[sha] {
			continue
		}
		seen[sha] = true
		out = append(out, sha)
	}
	return out
}

// matchRevertedMR resolves a revert trailer's commit id to the merged MR whose
// work landed as that commit. An exact hit wins outright; otherwise the id must
// be a prefix of at least regressionMinSHAPrefix characters matching exactly
// ONE landed SHA. Anything shorter or ambiguous resolves to nothing: a wrong
// attribution is worse than a missing one.
func matchRevertedMR(landed map[string]MergedMRRecord, revertedSHA string) (MergedMRRecord, bool) {
	if mr, ok := landed[revertedSHA]; ok {
		return mr, true
	}
	if len(revertedSHA) < regressionMinSHAPrefix {
		return MergedMRRecord{}, false
	}
	var (
		match MergedMRRecord
		hits  int
	)
	for sha, mr := range landed {
		if strings.HasPrefix(sha, revertedSHA) {
			match = mr
			hits++
			if hits > 1 {
				return MergedMRRecord{}, false
			}
		}
	}
	if hits != 1 {
		return MergedMRRecord{}, false
	}
	return match, true
}

// isAmbiguousRevertID reports whether a trailer failed to resolve because it was
// under-specified — too short, or a prefix of more than one merged MR — rather
// than because it simply names a commit outside this window. Only the former is
// worth counting: it is the signal that the matcher is being asked to guess.
func isAmbiguousRevertID(landed map[string]MergedMRRecord, revertedSHA string) bool {
	if len(revertedSHA) < regressionMinSHAPrefix {
		return true
	}
	hits := 0
	for sha := range landed {
		if strings.HasPrefix(sha, revertedSHA) {
			hits++
		}
	}
	return hits > 1
}

func (r *Reconciler) regressionLookback() time.Duration {
	if r != nil && r.RegressionLookback > 0 {
		return r.RegressionLookback
	}
	return defaultRegressionLookback
}

func (r *Reconciler) regressionBranch() string {
	if r != nil {
		if branch := strings.TrimSpace(r.RegressionBranch); branch != "" {
			return branch
		}
	}
	return defaultRegressionBranch
}

func (r *Reconciler) regressionSweepInterval() time.Duration {
	if r != nil && r.RegressionSweepInterval > 0 {
		return r.RegressionSweepInterval
	}
	return DefaultRegressionSweepInterval
}

// regressionSweepDue reports whether the interval has elapsed since the last
// attempt. The schedule is process-local (ticks are serial, so no locking): a
// restart merely runs one extra pass, which is idempotent.
func (r *Reconciler) regressionSweepDue(now time.Time) bool {
	if r == nil || r.RegressionMergedMRs == nil || r.RegressionCommits == nil {
		return false
	}
	return !now.Before(r.nextRegressionSweep)
}
