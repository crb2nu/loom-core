package clients

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
)

// This file owns the READ side of #374: observing and classifying a merge
// request's source-head movements. It issues no mutations. GitLab's rebase
// endpoint accepts no source-SHA precondition, so a movement can never be
// proved to be solely the replay Mills asked for; the classifier's only job is
// to separate "one clean movement off the reviewed SHA" (re-gate and continue)
// from "something else moved the branch" (stop and name both SHAs), and to let
// the ledger bound rebase↔push ping-pong.
//
// Nothing here reuses a CI verdict across a movement. A rebase changes the
// base, and identical patches on a different base do not imply identical CI
// results — the merge stays bound to a SHA that was actually tested.

const (
	// headSettleDefaultSeconds bounds the wait for GitLab's async rebase to
	// report rebase_in_progress=false. Cap exhausted settles 'ambiguous':
	// absence of evidence is never read as a clean movement.
	headSettleDefaultSeconds = 120
	// headObserveDefaultInterval is the poll cadence while a rebase is in
	// progress.
	headObserveDefaultInterval = 2 * time.Second
	// headEventsPageSize is how deep into the (project-wide, desc-ordered)
	// activity feed one observation reads. Movements land at the head of the
	// feed within seconds, so one page is ample; the cursor filter discards
	// everything older regardless.
	headEventsPageSize = 100
)

// ----- wire shapes (verified live against GitLab 18.4.3 CE, 2026-07-25) -----

type mrVersionResponse struct {
	ID             int64  `json:"id"`
	HeadCommitSHA  string `json:"head_commit_sha"`
	BaseCommitSHA  string `json:"base_commit_sha"`
	StartCommitSHA string `json:"start_commit_sha"`
	CreatedAt      string `json:"created_at"`
	State          string `json:"state"`
}

type projectEventResponse struct {
	ID             int64  `json:"id"`
	ActionName     string `json:"action_name"`
	CreatedAt      string `json:"created_at"`
	AuthorUsername string `json:"author_username"`
	PushData       struct {
		Action     string `json:"action"`
		RefType    string `json:"ref_type"`
		Ref        string `json:"ref"`
		CommitFrom string `json:"commit_from"`
		CommitTo   string `json:"commit_to"`
	} `json:"push_data"`
}

// ----- reads -----

// ReadHeadCursors snapshots the ledger positions a later observation measures
// against: the live MR head, the newest MR version id, the newest push event
// on the source branch, and the target branch tip. Read-only.
func (c *GitLabClient) ReadHeadCursors(ctx context.Context, req pipeline.HeadCursorRequest) (pipeline.HeadCursors, error) {
	if req.MRIID == 0 {
		return pipeline.HeadCursors{}, fmt.Errorf("gitlab: MRIID required")
	}
	cli, err := c.forObservedProject(req.Project)
	if err != nil {
		return pipeline.HeadCursors{}, err
	}
	mr, err := cli.getMRWithRebaseState(ctx, req.MRIID)
	if err != nil {
		return pipeline.HeadCursors{}, err
	}
	versions, err := cli.mrVersions(ctx, req.MRIID)
	if err != nil {
		return pipeline.HeadCursors{}, err
	}
	pushes, err := cli.pushEvents(ctx, req.SourceBranch)
	if err != nil {
		return pipeline.HeadCursors{}, err
	}
	cursors := pipeline.HeadCursors{
		SHA:              mr.SHA,
		RebaseInProgress: mr.RebaseInProgress,
	}
	for _, v := range versions {
		if v.ID > cursors.VersionsCursor {
			cursors.VersionsCursor = v.ID
		}
	}
	for _, p := range pushes {
		if p.ID > cursors.EventsCursor {
			cursors.EventsCursor = p.ID
		}
	}
	if branch := strings.TrimSpace(req.TargetBranch); branch != "" {
		tip, err := cli.branchTip(ctx, branch)
		if err != nil {
			return pipeline.HeadCursors{}, err
		}
		cursors.TargetHeadSHA = tip
	}
	return cursors, nil
}

// ObserveHead settles and classifies one head movement per §5.3 of the #374
// design. It never issues a mutation: it waits for any in-flight rebase to
// report done, then reads the MR, its versions, and the push activity feed and
// returns a verdict plus the verbatim evidence.
//
// Classification order is deliberate:
//  1. noop is decided ONLY by successor == reviewed on the MR itself, so a
//     lagging or dropped activity feed can never be mistaken for "nothing
//     happened".
//  2. versions is the primary witness (written on the MR's own diff-refresh
//     path); push events corroborate but their absence never contradicts.
//  3. anything else is ambiguous, with the evidence recorded verbatim.
func (c *GitLabClient) ObserveHead(ctx context.Context, req pipeline.HeadObservationRequest) (pipeline.HeadObservation, error) {
	if req.MRIID == 0 {
		return pipeline.HeadObservation{}, fmt.Errorf("gitlab: MRIID required")
	}
	if strings.TrimSpace(req.ReviewedSHA) == "" {
		return pipeline.HeadObservation{}, fmt.Errorf("gitlab: ReviewedSHA required to observe a head movement: %w", pipeline.ErrMergeAuthorizationStale)
	}
	cli, err := c.forObservedProject(req.Project)
	if err != nil {
		return pipeline.HeadObservation{}, err
	}

	obs := pipeline.HeadObservation{
		VersionsCursor: req.VersionsCursor,
		EventsCursor:   req.EventsCursor,
	}
	deadline := headSettleDeadline(req)
	interval := cli.headObserveInterval
	if interval <= 0 {
		interval = headObserveDefaultInterval
	}
	started := time.Now()
	settleCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	for {
		obs.Attempts++
		mr, err := cli.getMRWithRebaseState(settleCtx, req.MRIID)
		if err != nil {
			if ctx.Err() != nil {
				return obs, ctx.Err()
			}
			if settleCtx.Err() != nil {
				break
			}
			return obs, err
		}
		obs.RebaseInProgress = mr.RebaseInProgress
		obs.MergeError = strings.TrimSpace(mr.MergeError)
		obs.SuccessorSHA = mr.SHA

		if !mr.RebaseInProgress {
			obs.SettledAfterMS = time.Since(started).Milliseconds()
			if obs.MergeError != "" {
				obs.Verdict = pipeline.HeadVerdictFailed
				obs.Reason = "gitlab reported merge_error after the rebase settled: " + obs.MergeError
				return obs, nil
			}
			return cli.classifyHead(settleCtx, req, obs)
		}

		select {
		case <-settleCtx.Done():
			// fallthrough to the deadline verdict below
		case <-time.After(interval):
			continue
		}
		break
	}

	if ctx.Err() != nil {
		return obs, ctx.Err()
	}
	obs.SettledAfterMS = time.Since(started).Milliseconds()
	obs.Verdict = pipeline.HeadVerdictAmbiguous
	obs.Reason = fmt.Sprintf("rebase still in progress after %s settle deadline; absence of a verdict is never read as a clean movement", deadline)
	return obs, nil
}

// classifyHead runs steps 1–5 of §5.3 once the MR reports no rebase in flight.
func (c *GitLabClient) classifyHead(ctx context.Context, req pipeline.HeadObservationRequest, obs pipeline.HeadObservation) (pipeline.HeadObservation, error) {
	// Step 1 — noop by SHA equality on the MR itself, never by absence of
	// ledger rows.
	if obs.SuccessorSHA == req.ReviewedSHA {
		obs.Verdict = pipeline.HeadVerdictNoop
		obs.Reason = "mr head is unchanged (successor == reviewed sha)"
		return obs, nil
	}

	// Step 2 — versions after the cursor: the primary witness.
	versions, err := c.mrVersions(ctx, req.MRIID)
	if err != nil {
		return obs, err
	}
	for _, v := range versions {
		if v.ID > req.VersionsCursor {
			obs.Versions = append(obs.Versions, v)
		}
	}

	// Step 3 — push events after the cursor on this ref: corroboration.
	pushes, err := c.pushEvents(ctx, req.SourceBranch)
	if err != nil {
		return obs, err
	}
	for _, p := range pushes {
		if p.ID > req.EventsCursor {
			obs.Pushes = append(obs.Pushes, p)
		}
	}

	// Step 4 — exactly one diff refresh landing on the successor, with push
	// evidence either absent (the feed is async) or matching the exact
	// reviewed→successor edge.
	if len(obs.Versions) == 1 && obs.Versions[0].HeadCommitSHA == obs.SuccessorSHA {
		switch len(obs.Pushes) {
		case 0:
			obs.Verdict = pipeline.HeadVerdictAttributed
			obs.Reason = "exactly one movement; version head == successor; activity feed had not caught up"
			return obs, nil
		case 1:
			p := obs.Pushes[0]
			if p.CommitFrom == req.ReviewedSHA && p.CommitTo == obs.SuccessorSHA {
				obs.Verdict = pipeline.HeadVerdictAttributed
				obs.Reason = "exactly one movement; commit_from == reviewed_sha"
				return obs, nil
			}
			obs.Verdict = pipeline.HeadVerdictAmbiguous
			obs.Reason = fmt.Sprintf("single push does not describe the reviewed->successor edge (from %s to %s)", shortSHA(p.CommitFrom), shortSHA(p.CommitTo))
			return obs, nil
		}
	}

	// Step 5 — everything else.
	obs.Verdict = pipeline.HeadVerdictAmbiguous
	switch {
	case len(obs.Versions) == 0:
		obs.Reason = fmt.Sprintf("mr head moved %s -> %s with no version row to witness it", shortSHA(req.ReviewedSHA), shortSHA(obs.SuccessorSHA))
	case len(obs.Versions) > 1:
		obs.Reason = fmt.Sprintf("%d movements observed; a rebase Mills requested produces exactly one", len(obs.Versions))
	default:
		obs.Reason = fmt.Sprintf("version head %s does not match mr head %s", shortSHA(obs.Versions[0].HeadCommitSHA), shortSHA(obs.SuccessorSHA))
	}
	if len(obs.Pushes) > 1 {
		obs.Reason += fmt.Sprintf("; %d pushes on the source branch since the cursor", len(obs.Pushes))
	}
	return obs, nil
}

// ----- helpers -----

// forObservedProject enforces the same project binding every other
// authorization-sensitive call uses: an observation must never silently follow
// a rerouted item into a different project that happens to hold the same IID.
func (c *GitLabClient) forObservedProject(project string) (*GitLabClient, error) {
	project = strings.TrimSpace(project)
	if project == "" || project == c.cfg.Project {
		return c, nil
	}
	return nil, fmt.Errorf("gitlab: head observation project %q does not match client project %q: %w", project, c.cfg.Project, pipeline.ErrMergeAuthorizationStale)
}

// mrRebaseStateResponse extends mrResponse with the rebase fields GitLab only
// returns when include_rebase_in_progress=true is requested.
type mrRebaseStateResponse struct {
	mrResponse
	RebaseInProgress bool `json:"rebase_in_progress"`
	DiffRefs         struct {
		BaseSHA  string `json:"base_sha"`
		HeadSHA  string `json:"head_sha"`
		StartSHA string `json:"start_sha"`
	} `json:"diff_refs"`
}

func (c *GitLabClient) getMRWithRebaseState(ctx context.Context, mrIID int64) (mrRebaseStateResponse, error) {
	// rebase_in_progress is ABSENT from the body unless this parameter is set
	// (verified live 2026-07-25) — without it the zero value would read as a
	// settled rebase on every poll.
	path := fmt.Sprintf("/projects/%s/merge_requests/%d?include_rebase_in_progress=true", c.projectPath(), mrIID)
	var mr mrRebaseStateResponse
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &mr); err != nil {
		return mrRebaseStateResponse{}, err
	}
	return mr, nil
}

func (c *GitLabClient) mrVersions(ctx context.Context, mrIID int64) ([]pipeline.MRVersionRecord, error) {
	path := fmt.Sprintf("/projects/%s/merge_requests/%d/versions?per_page=%d", c.projectPath(), mrIID, headEventsPageSize)
	var rows []mrVersionResponse
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]pipeline.MRVersionRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, pipeline.MRVersionRecord{
			ID:             r.ID,
			HeadCommitSHA:  r.HeadCommitSHA,
			BaseCommitSHA:  r.BaseCommitSHA,
			StartCommitSHA: r.StartCommitSHA,
			CreatedAt:      r.CreatedAt,
		})
	}
	return out, nil
}

// pushEvents returns the project's recent pushed events filtered to one ref.
// The feed is project-wide and newest-first; filtering by ref here keeps an
// unrelated branch's activity out of the evidence bundle.
func (c *GitLabClient) pushEvents(ctx context.Context, ref string) ([]pipeline.PushEventRecord, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil
	}
	path := fmt.Sprintf("/projects/%s/events?action=pushed&per_page=%d", c.projectPath(), headEventsPageSize)
	var rows []projectEventResponse
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &rows); err != nil {
		return nil, err
	}
	var out []pipeline.PushEventRecord
	for _, r := range rows {
		if r.PushData.Ref != ref {
			continue
		}
		out = append(out, pipeline.PushEventRecord{
			ID:         r.ID,
			CommitFrom: r.PushData.CommitFrom,
			CommitTo:   r.PushData.CommitTo,
			Ref:        r.PushData.Ref,
			Author:     r.AuthorUsername,
			Action:     r.PushData.Action,
			CreatedAt:  r.CreatedAt,
		})
	}
	return out, nil
}

type branchTipResponse struct {
	Name   string `json:"name"`
	Commit struct {
		ID string `json:"id"`
	} `json:"commit"`
}

func (c *GitLabClient) branchTip(ctx context.Context, branch string) (string, error) {
	path := fmt.Sprintf("/projects/%s/repository/branches/%s", c.projectPath(), url.PathEscape(branch))
	var got branchTipResponse
	if err := c.requestJSON(ctx, http.MethodGet, path, nil, &got); err != nil {
		return "", err
	}
	return got.Commit.ID, nil
}

// headSettleDeadline resolves the settle cap: explicit request value, then
// LOOM_MILLS_MERGE_REBASE_SETTLE_SECONDS (stage env first, then process env,
// mirroring ciWatchMaxMinutes), then the 120s default.
func headSettleDeadline(req pipeline.HeadObservationRequest) time.Duration {
	if req.SettleDeadline > 0 {
		return req.SettleDeadline
	}
	raw := ""
	if req.Env != nil {
		raw = strings.TrimSpace(req.Env["LOOM_MILLS_MERGE_REBASE_SETTLE_SECONDS"])
	}
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("LOOM_MILLS_MERGE_REBASE_SETTLE_SECONDS"))
	}
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return headSettleDefaultSeconds * time.Second
}
