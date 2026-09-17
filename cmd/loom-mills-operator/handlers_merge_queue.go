package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/crb2nu/loom/pkg/mills/mergequeue"
	"github.com/crb2nu/loom/pkg/mills/store"
)

type externalEnqueueRequest struct {
	Producer       string `json:"producer"`
	IdempotencyKey string `json:"idempotency_key"`
	Project        string `json:"project"`
	MRIID          int64  `json:"mr_iid"`
	SourceBranch   string `json:"source_branch"`
	TargetBranch   string `json:"target_branch"`
	ObservedSHA    string `json:"observed_sha"`
}

// handleMergeQueueList is the open read behind GET /api/mills/merge-queue:
// every active entry in FIFO order plus a per-lane depth summary. Serves the
// HUD merge-queue panel and fleet producers checking lane pressure before an
// enqueue; same open-read posture as /backlog and /pipeline/runs.
func (o *operator) handleMergeQueueList(w http.ResponseWriter, r *http.Request) {
	if o.store == nil || o.store.MergeQueue == nil {
		http.Error(w, "merge queue store unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()
	active, err := o.store.MergeQueue.ListActive(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if active == nil {
		active = []*store.MergeQueueEntry{}
	}
	recentSettled, err := o.store.MergeQueue.ListSettled(ctx, time.Now().UTC().Add(-24*time.Hour), 20)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if recentSettled == nil {
		recentSettled = []*store.MergeQueueEntry{}
	}
	lanes := map[string]int{}
	for _, e := range active {
		lanes[e.Project+"→"+e.TargetBranch]++
	}
	enabled := o.policy != nil && o.policy.Current().MergeQueueEnabled()
	writeJSON(w, http.StatusOK, map[string]any{
		"active":         active,
		"recent_settled": recentSettled,
		"summary":        map[string]any{"depth": len(active), "lanes": lanes, "enabled": enabled},
	})
}

func (o *operator) handleMergeQueueEnqueue(w http.ResponseWriter, r *http.Request) {
	var req externalEnqueueRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	enq := &mergequeue.ExternalEnqueuer{Store: o.store, Enabled: func() bool { return o.policy != nil && o.policy.Current().MergeQueueEnabled() }, MaxDepth: func() int { return o.policy.Current().MergeQueueMaxDepth() }, CheckPermission: o.mergeQueuePermission}
	result, err := enq.Enqueue(r.Context(), mergequeue.ExternalCandidate{Producer: req.Producer, IdempotencyKey: req.IdempotencyKey, Project: req.Project, MRIID: req.MRIID, SourceBranch: req.SourceBranch, TargetBranch: req.TargetBranch, ObservedSHA: req.ObservedSHA})
	if err != nil {
		// JSON outcomes let fleet producers surface denial/unavailability without
		// treating either as the policy-disabled direct-merge fallback.
		if errors.Is(err, mergequeue.ErrMergePermissionDenied) {
			writeJSON(w, http.StatusForbidden, map[string]string{"outcome": "forbidden", "reason": mergequeue.ErrMergePermissionDenied.Error()})
			return
		}
		if errors.Is(err, mergequeue.ErrMergePermissionUnavailable) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"outcome": "unavailable", "reason": mergequeue.ErrMergePermissionUnavailable.Error() + "; retry later"})
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	status := http.StatusAccepted
	if result.Outcome == "duplicate" {
		status = http.StatusOK
	}
	if result.Outcome == "disabled" {
		status = http.StatusConflict
	}
	// conflicted: the queue already evicted this exact head for a rebase
	// conflict, so nothing was enqueued. 409 tells the producer the arm did
	// not happen (mrwatch records the action as an error and its per-MR daily
	// budget bounds retries); the body names the prior verdict.
	if result.Outcome == "conflicted" {
		status = http.StatusConflict
	}
	if result.Outcome == "full" {
		status = http.StatusTooManyRequests
	}
	writeJSON(w, status, result)
}
