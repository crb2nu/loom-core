package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

const safetyCrashLeaseTTL = 90 * time.Second

type safetyCrashLease struct {
	Token     string
	RequestID string
	RunID     string
	SpawnID   string
	ExpiresAt time.Time
	Validated bool
}

type safetyCrashLeaseRequest struct {
	RequestID string `json:"request_id"`
	RunID     string `json:"run_id"`
	SpawnID   string `json:"spawn_id"`
}

type safetyCrashLeaseResponse struct {
	Token     string    `json:"token"`
	RequestID string    `json:"request_id"`
	RunID     string    `json:"run_id"`
	SpawnID   string    `json:"spawn_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (o *operator) crashLeaseActiveLocked(now time.Time) bool {
	if o.crashLease == nil {
		return false
	}
	if !now.Before(o.crashLease.ExpiresAt) {
		o.crashLease = nil
		return false
	}
	return true
}

func randomCrashLeaseToken() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func crashLeaseResponse(lease *safetyCrashLease) safetyCrashLeaseResponse {
	return safetyCrashLeaseResponse{
		Token: lease.Token, RequestID: lease.RequestID, RunID: lease.RunID,
		SpawnID: lease.SpawnID, ExpiresAt: lease.ExpiresAt,
	}
}

func (o *operator) validateCrashLeaseTarget(
	ctx context.Context,
	lease *safetyCrashLease,
	policyGeneration uint64,
) (int, string) {
	snapshot, err := o.readSafetyQuiescence(ctx)
	if err != nil {
		return http.StatusServiceUnavailable, "crash lease quiescence unavailable"
	}
	if !durableCountsIdleForWorkflow(snapshot.Counts, 1) || !snapshot.InMemory.SampleStable ||
		!snapshot.InMemory.valuesIdleForWorkflowCrashLease(lease.RunID) ||
		snapshot.InMemory.PolicyGeneration != policyGeneration {
		return http.StatusConflict, "target workflow is not the sole stable safe activity"
	}
	run, err := o.store.Workflow.GetWorkflowRun(ctx, lease.RunID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return http.StatusNotFound, "workflow run not found"
		}
		return http.StatusServiceUnavailable, "workflow run unavailable"
	}
	if run.State != store.WorkflowRunRunning {
		return http.StatusConflict, "workflow run is not running"
	}
	return 0, ""
}

// handleSafetyCrashLease atomically closes every non-target admission before
// proving the target workflow is the sole durable/in-process activity. The
// fence remains active across the harness's Kubernetes identity reads and
// UID-preconditioned delete, closing the final preflight→mutation TOCTOU.
func (o *operator) handleSafetyCrashLease(w http.ResponseWriter, r *http.Request) {
	var req safetyCrashLeaseRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid crash lease request: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.RequestID = strings.TrimSpace(req.RequestID)
	req.RunID = strings.TrimSpace(req.RunID)
	req.SpawnID = strings.TrimSpace(req.SpawnID)
	if req.RequestID == "" || req.RunID == "" || req.SpawnID == "" {
		http.Error(w, "request_id, run_id, and spawn_id are required", http.StatusBadRequest)
		return
	}

	o.workflowCanaryMu.Lock()
	defer o.workflowCanaryMu.Unlock()
	o.admissionMu.Lock()
	now := time.Now().UTC()
	if o.crashLeaseActiveLocked(now) {
		existing := o.crashLease
		if existing.Validated && existing.RequestID == req.RequestID &&
			existing.RunID == req.RunID && existing.SpawnID == req.SpawnID {
			o.admissionMu.Unlock()
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, crashLeaseResponse(existing))
			return
		}
		o.admissionMu.Unlock()
		http.Error(w, "another crash lease is active", http.StatusConflict)
		return
	}
	policy := o.policy.Current()
	if policy == nil || policy.IsEnabled() || !policy.WorkflowsEnabled() {
		o.admissionMu.Unlock()
		http.Error(w, "crash lease requires policy.enabled=false and workflows.enabled=true", http.StatusConflict)
		return
	}
	policyGeneration := o.policyGeneration.Load()
	token, err := randomCrashLeaseToken()
	if err != nil {
		o.admissionMu.Unlock()
		http.Error(w, "create crash lease token", http.StatusInternalServerError)
		return
	}
	lease := &safetyCrashLease{
		Token: token, RequestID: req.RequestID, RunID: req.RunID, SpawnID: req.SpawnID,
		ExpiresAt: now.Add(safetyCrashLeaseTTL),
	}
	o.crashLease = lease
	o.admissionMu.Unlock()

	clearLease := func() {
		o.admissionMu.Lock()
		if o.crashLease != nil && o.crashLease.Token == token {
			o.crashLease = nil
		}
		o.admissionMu.Unlock()
	}
	status, message := o.validateCrashLeaseTarget(r.Context(), lease, policyGeneration)
	if status != 0 {
		clearLease()
		http.Error(w, message, status)
		return
	}
	o.admissionMu.Lock()
	currentPolicy := o.policy.Current()
	if !o.crashLeaseActiveLocked(time.Now().UTC()) || o.crashLease.Token != token ||
		currentPolicy != policy || o.policyGeneration.Load() != policyGeneration ||
		currentPolicy == nil || currentPolicy.IsEnabled() || !currentPolicy.WorkflowsEnabled() {
		o.admissionMu.Unlock()
		clearLease()
		http.Error(w, "policy or lease changed during crash proof", http.StatusConflict)
		return
	}
	o.crashLease.Validated = true
	lease = o.crashLease
	o.admissionMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, crashLeaseResponse(lease))
}

// handleSafetyCrashLeaseRenew repeats the load-bearing proof and extends the
// same token immediately before the harness's UID-preconditioned delete. A
// slow identity/readiness check can therefore never outlive its fence.
func (o *operator) handleSafetyCrashLeaseRenew(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.PathValue("token"))
	o.workflowCanaryMu.Lock()
	defer o.workflowCanaryMu.Unlock()

	o.admissionMu.Lock()
	now := time.Now().UTC()
	if !o.crashLeaseActiveLocked(now) || o.crashLease.Token != token || !o.crashLease.Validated {
		o.admissionMu.Unlock()
		http.Error(w, "crash lease not found", http.StatusNotFound)
		return
	}
	policy := o.policy.Current()
	policyGeneration := o.policyGeneration.Load()
	if policy == nil || policy.IsEnabled() || !policy.WorkflowsEnabled() {
		o.crashLease = nil
		o.admissionMu.Unlock()
		http.Error(w, "crash lease policy is no longer valid", http.StatusConflict)
		return
	}
	lease := o.crashLease
	// Keep admissions fenced while the renewed proof reads durable state. A
	// failed proof clears the lease rather than preserving an unproved window.
	lease.ExpiresAt = now.Add(safetyCrashLeaseTTL)
	o.admissionMu.Unlock()

	status, message := o.validateCrashLeaseTarget(r.Context(), lease, policyGeneration)
	if status != 0 {
		o.admissionMu.Lock()
		if o.crashLease != nil && o.crashLease.Token == token {
			o.crashLease = nil
		}
		o.admissionMu.Unlock()
		http.Error(w, message, status)
		return
	}

	o.admissionMu.Lock()
	currentPolicy := o.policy.Current()
	if !o.crashLeaseActiveLocked(time.Now().UTC()) || o.crashLease.Token != token ||
		currentPolicy != policy || o.policyGeneration.Load() != policyGeneration ||
		currentPolicy == nil || currentPolicy.IsEnabled() || !currentPolicy.WorkflowsEnabled() {
		if o.crashLease != nil && o.crashLease.Token == token {
			o.crashLease = nil
		}
		o.admissionMu.Unlock()
		http.Error(w, "policy or lease changed during renewed crash proof", http.StatusConflict)
		return
	}
	o.crashLease.ExpiresAt = time.Now().UTC().Add(safetyCrashLeaseTTL)
	lease = o.crashLease
	o.admissionMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, crashLeaseResponse(lease))
}

func (o *operator) handleSafetyCrashLeaseRelease(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.PathValue("token"))
	o.admissionMu.Lock()
	defer o.admissionMu.Unlock()
	if !o.crashLeaseActiveLocked(time.Now().UTC()) {
		http.Error(w, "crash lease not found", http.StatusNotFound)
		return
	}
	if o.crashLease.Token != token {
		http.Error(w, "crash lease token does not match the active lease", http.StatusConflict)
		return
	}
	o.crashLease = nil
	w.WriteHeader(http.StatusNoContent)
}
