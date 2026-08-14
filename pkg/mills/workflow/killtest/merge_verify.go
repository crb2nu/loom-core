package killtest

// merge_verify.go collects the S6-full merging canary's PASS-3 evidence
// after the run reaches done: the GitLab MR population for the run's
// deterministic merge branch (any state — a duplicate open MR is as fatal as
// a duplicate merged one) and the journal's merge success rows from the same
// operator run detail every other verdict reads.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CanaryMergeBranchForRun mirrors the operator's deterministic merge branch
// derivation (cmd/loom-mills-operator newCanaryMerger). Independent
// re-derivation is deliberate: the evidence must not trust an operator field
// for the identity it is auditing.
func CanaryMergeBranchForRun(runID string) string { return "mills-wf-merge/" + runID }

type gitlabMergeRequestRow struct {
	IID            int64  `json:"iid"`
	State          string `json:"state"`
	SourceBranch   string `json:"source_branch"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	SquashCommit   string `json:"squash_commit_sha"`
}

// CollectCanaryMergeEvidence fills ev.CanaryMerge for a merging canary run.
// Fail-closed: any read error leaves the evidence absent, which PASS-3
// treats as FAIL.
func (h *Harness) CollectCanaryMergeEvidence(ctx context.Context, runID string, ev *Evidence) error {
	if !h.cfg.Merging {
		return fmt.Errorf("collect canary merge evidence: harness is not in merging mode")
	}
	if strings.TrimSpace(h.cfg.GitLabAPIURL) == "" || strings.TrimSpace(h.cfg.GitLabToken) == "" {
		return fmt.Errorf("collect canary merge evidence: GitLab API URL and token are required")
	}
	branch := CanaryMergeBranchForRun(runID)

	reqURL := fmt.Sprintf("%s/projects/%s/merge_requests?source_branch=%s&state=all&per_page=100",
		strings.TrimRight(h.cfg.GitLabAPIURL, "/"),
		url.PathEscape(h.cfg.GitLabProject),
		url.QueryEscape(branch))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("collect canary merge evidence: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", h.cfg.GitLabToken)
	resp, err := h.http.Do(req)
	if err != nil {
		return fmt.Errorf("collect canary merge evidence: list MRs for %q: %w", branch, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("collect canary merge evidence: read MR list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("collect canary merge evidence: list MRs for %q: %d: %s",
			branch, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rows []gitlabMergeRequestRow
	if err := json.Unmarshal(body, &rows); err != nil {
		return fmt.Errorf("collect canary merge evidence: decode MR list: %w", err)
	}

	detail, err := h.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("collect canary merge evidence: run detail: %w", err)
	}
	journalRows := 0
	for _, step := range detail.Steps {
		if step.Status == "success" && strings.Contains(step.StepKey, "merge~") {
			journalRows++
		}
	}

	evidence := &CanaryMergeEvidence{
		SourceBranch:            branch,
		MRCount:                 len(rows),
		JournalMergeSuccessRows: journalRows,
		CollectedAt:             time.Now().UTC(),
	}
	if len(rows) > 0 {
		evidence.MRIID = rows[0].IID
		evidence.MRState = rows[0].State
		evidence.MergeCommitSHA = rows[0].MergeCommitSHA
		if evidence.MergeCommitSHA == "" {
			evidence.MergeCommitSHA = rows[0].SquashCommit
		}
	}
	ev.CanaryMerge = evidence
	return nil
}
