package main

import (
	"encoding/json"
	"net/http"

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
	active, err := o.store.MergeQueue.ListActive(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if active == nil {
		active = []*store.MergeQueueEntry{}
	}
	lanes := map[string]int{}
	for _, e := range active {
		lanes[e.Project+"→"+e.TargetBranch]++
	}
	enabled := o.policy != nil && o.policy.Current().MergeQueueEnabled()
	writeJSON(w, http.StatusOK, map[string]any{
		"active":  active,
		"summary": map[string]any{"depth": len(active), "lanes": lanes, "enabled": enabled},
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
	enq := &mergequeue.ExternalEnqueuer{Store: o.store, Enabled: func() bool { return o.policy != nil && o.policy.Current().MergeQueueEnabled() }, MaxDepth: func() int { return o.policy.Current().MergeQueueMaxDepth() }}
	result, err := enq.Enqueue(r.Context(), mergequeue.ExternalCandidate{Producer: req.Producer, IdempotencyKey: req.IdempotencyKey, Project: req.Project, MRIID: req.MRIID, SourceBranch: req.SourceBranch, TargetBranch: req.TargetBranch, ObservedSHA: req.ObservedSHA})
	if err != nil {
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
	if result.Outcome == "full" {
		status = http.StatusTooManyRequests
	}
	writeJSON(w, status, result)
}
