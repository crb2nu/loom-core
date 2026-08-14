package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/mills/workflow"
)

type sequenceActivitySource struct {
	values []int64
	calls  int
}

type activitySourceFunc func() int64

func (f activitySourceFunc) ActiveOperations() int64 { return f() }

func seedCrashLeaseWorkflow(t *testing.T, op *operator, runID string) {
	t.Helper()
	off := false
	op.policy.Current().Enabled = &off
	op.policy.Current().Workflows.Enabled = true
	op.policy.Current().Workflows.SubstrateK8sOnly = true
	now := time.Now().UTC()
	if err := op.store.Workflow.PutWorkflowRun(context.Background(), &store.WorkflowRun{
		ID: runID, Engine: store.WorkflowEngineImperative, Template: "workflow-canary",
		TemplateVersion: workflow.CanaryTemplateVersion, State: store.WorkflowRunRunning, StartedAt: &now,
	}); err != nil {
		t.Fatalf("seed crash-lease workflow: %v", err)
	}
}

func requestCrashLease(t *testing.T, op *operator, requestID, runID, spawnID string) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(safetyCrashLeaseRequest{RequestID: requestID, RunID: runID, SpawnID: spawnID})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	op.handleSafetyCrashLease(recorder, httptest.NewRequest(http.MethodPost,
		"/api/mills/safety/crash-lease", bytes.NewReader(payload)))
	return recorder
}

func decodeCrashLease(t *testing.T, recorder *httptest.ResponseRecorder) safetyCrashLeaseResponse {
	t.Helper()
	var lease safetyCrashLeaseResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &lease); err != nil {
		t.Fatalf("decode crash lease: %v body=%s", err, recorder.Body.String())
	}
	return lease
}

func (s *sequenceActivitySource) ActiveOperations() int64 {
	idx := s.calls
	s.calls++
	if idx >= len(s.values) {
		return s.values[len(s.values)-1]
	}
	return s.values[idx]
}

func TestHandleSafetyQuiescence_ReportsExactSnapshot(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	if err := op.store.Backlog.Put(context.Background(), &store.BacklogItem{
		ID: "MILLS-SAFETY-QUEUED", Title: "queued", State: store.BacklogQueued,
		Priority: store.P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("seed queued backlog: %v", err)
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/mills/safety/quiescence", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", got)
	}
	var got safetyQuiescenceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ObservedAt.IsZero() || time.Since(got.ObservedAt) > time.Minute {
		t.Fatalf("observed_at not current: %v", got.ObservedAt)
	}
	if got.Quiescent {
		t.Fatalf("queued work reported quiescent: %+v", got)
	}
	if got.Counts.QueuedBacklog != 1 {
		t.Fatalf("queued_backlog=%d, want 1", got.Counts.QueuedBacklog)
	}
}

func TestHandleSafetyQuiescence_QueryFailureIsServiceUnavailable(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	if err := op.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/mills/safety/quiescence", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s, want 503", rec.Code, rec.Body.String())
	}
}

func TestHandleSafetyQuiescence_InMemoryWorkBlocks(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.activeAdmissions.Store(1)
	rec := httptest.NewRecorder()
	op.handleSafetyQuiescence(rec, httptest.NewRequest(http.MethodGet, "/api/mills/safety/quiescence", nil))
	var got safetyQuiescenceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Quiescent || got.InMemory.ActiveAdmissions != 1 {
		t.Fatalf("in-memory work reported quiescent: %+v", got)
	}
}

func TestSafetyQuiescenceRejectsActivityTransitionAcrossDBSnapshot(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	source := &sequenceActivitySource{values: []int64{1, 0}}
	op.withActivitySources(namedActivitySource{name: "transition_test", source: source})
	snapshot, err := op.readSafetyQuiescence(context.Background())
	if err != nil {
		t.Fatalf("readSafetyQuiescence: %v", err)
	}
	if snapshot.Quiescent || snapshot.InMemory.SampleStable {
		t.Fatalf("activity transition synthesized safe zero: %+v", snapshot)
	}
}

func TestSafetyQuiescenceRejectsCancellingSourceCounters(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	off := false
	op.policy.Current().Enabled = &off
	op.withActivitySources(
		namedActivitySource{name: "positive", source: &sequenceActivitySource{values: []int64{1}}},
		namedActivitySource{name: "negative", source: &sequenceActivitySource{values: []int64{-1}}},
	)
	snapshot, err := op.readSafetyQuiescence(context.Background())
	if err != nil {
		t.Fatalf("readSafetyQuiescence: %v", err)
	}
	if snapshot.Quiescent || snapshot.InMemory.BackgroundOperations != 0 {
		t.Fatalf("cancelling source counters reported safe: %+v", snapshot)
	}
}

func TestSafetyActivitySourceWiringIsOneAtomicSnapshot(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	op.beginActivitySourceWiring()
	before := op.inMemoryActivity()
	if before.SourcesReady {
		t.Fatalf("source set reported ready while wiring: %+v", before)
	}
	for _, name := range requiredActivitySourceNames {
		op.addActivitySource(name, &sequenceActivitySource{values: []int64{0}})
	}
	op.markActivitySourcesReady()
	after := op.inMemoryActivity()
	if !after.SourcesReady || after.ActivitySources != len(requiredActivitySourceNames) ||
		len(after.MissingSources) != 0 || after.SourceGeneration <= before.SourceGeneration {
		t.Fatalf("source snapshot was not published atomically: before=%+v after=%+v", before, after)
	}
}

func TestSafetyActivitySourceWiringRejectsDuplicateInstance(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	op.beginActivitySourceWiring()
	source := &sequenceActivitySource{values: []int64{0}}
	op.addActivitySource(activitySourceReconciler, source)
	op.addActivitySource(activitySourcePipeline, source)
	op.markActivitySourcesReady()
	got := op.inMemoryActivity()
	if got.SourcesReady || got.WiringError == "" {
		t.Fatalf("duplicate source instance reported ready: %+v", got)
	}
}

func TestSafetyQuiescenceRequiresClosedAdmission(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	open, err := op.readSafetyQuiescence(context.Background())
	if err != nil {
		t.Fatalf("read open quiescence: %v", err)
	}
	if open.Quiescent || open.InMemory.AdmissionClosed {
		t.Fatalf("open admission reported safe: %+v", open)
	}
	off := false
	op.policy.Current().Enabled = &off
	closed, err := op.readSafetyQuiescence(context.Background())
	if err != nil {
		t.Fatalf("read closed quiescence: %v", err)
	}
	if !closed.Quiescent || !closed.InMemory.AdmissionClosed {
		t.Fatalf("closed idle admission not quiescent: %+v", closed)
	}
}

func TestWorkAdmissionHonorsGlobalBarrierAndTracksInFlight(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	called := false
	handler := op.requireWorkAdmission(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		if got := op.activeAdmissions.Load(); got != 1 {
			t.Fatalf("active admissions inside handler = %d, want 1", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/work", nil))
	if rec.Code != http.StatusNoContent || !called || op.activeAdmissions.Load() != 0 {
		t.Fatalf("admitted response=%d called=%t active=%d", rec.Code, called, op.activeAdmissions.Load())
	}

	off := false
	op.policy.Current().Enabled = &off
	called = false
	rec = httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/work", nil))
	if rec.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("closed admission response=%d called=%t", rec.Code, called)
	}
}

func TestOperationalActivityIsTrackedWithoutAdmissionRejection(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	off := false
	op.policy.Current().Enabled = &off

	called := false
	handler := op.trackSafetyActivity(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		if got := op.activeAdmissions.Load(); got != 1 {
			t.Fatalf("tracked operations inside handler = %d, want 1", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/operate", nil))
	if rec.Code != http.StatusNoContent || !called || op.activeAdmissions.Load() != 0 {
		t.Fatalf("operation response=%d called=%t active=%d", rec.Code, called, op.activeAdmissions.Load())
	}
}

func TestSafetyCrashLeaseAcceptsOnlySoleTargetAndIsIdempotent(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedCrashLeaseWorkflow(t, op, "wf-canary-lease")

	first := requestCrashLease(t, op, "request-1", "wf-canary-lease", "spawn-1")
	if first.Code != http.StatusOK {
		t.Fatalf("first lease status=%d body=%s", first.Code, first.Body.String())
	}
	lease := decodeCrashLease(t, first)
	if lease.Token == "" || lease.RunID != "wf-canary-lease" || lease.SpawnID != "spawn-1" {
		t.Fatalf("incomplete lease: %+v", lease)
	}

	retry := requestCrashLease(t, op, "request-1", "wf-canary-lease", "spawn-1")
	if retry.Code != http.StatusOK || decodeCrashLease(t, retry).Token != lease.Token {
		t.Fatalf("idempotent retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	conflict := requestCrashLease(t, op, "request-2", "wf-canary-lease", "spawn-1")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("competing lease status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	renew := httptest.NewRecorder()
	renewRequest := httptest.NewRequest(http.MethodPost, "/api/mills/safety/crash-lease/"+lease.Token+"/renew", nil)
	renewRequest.SetPathValue("token", lease.Token)
	op.handleSafetyCrashLeaseRenew(renew, renewRequest)
	if renew.Code != http.StatusOK || decodeCrashLease(t, renew).Token != lease.Token {
		t.Fatalf("renew status=%d body=%s", renew.Code, renew.Body.String())
	}

	operateCalled := false
	operate := op.trackSafetyActivity(func(w http.ResponseWriter, _ *http.Request) {
		operateCalled = true
		w.WriteHeader(http.StatusNoContent)
	})
	blocked := httptest.NewRecorder()
	operate(blocked, httptest.NewRequest(http.MethodPost, "/operate", nil))
	if blocked.Code != http.StatusLocked || operateCalled {
		t.Fatalf("operational mutation crossed lease: status=%d called=%t", blocked.Code, operateCalled)
	}
	// Even if the in-memory policy object is unexpectedly reopened, the lease
	// remains the independent admission fence.
	on := true
	op.policy.Current().Enabled = &on
	admitCalled := false
	admit := op.requireWorkAdmission(func(w http.ResponseWriter, _ *http.Request) {
		admitCalled = true
		w.WriteHeader(http.StatusNoContent)
	})
	admission := httptest.NewRecorder()
	admit(admission, httptest.NewRequest(http.MethodPost, "/work", nil))
	if admission.Code != http.StatusServiceUnavailable || admitCalled {
		t.Fatalf("work admission crossed lease: status=%d called=%t", admission.Code, admitCalled)
	}
	canary := httptest.NewRecorder()
	op.handleWorkflowCanaryStart(canary, httptest.NewRequest(http.MethodPost, "/api/mills/workflow/canary",
		bytes.NewBufferString(`{"run_id":"wf-canary-other"}`)))
	if canary.Code != http.StatusLocked {
		t.Fatalf("canary crossed lease: status=%d body=%s", canary.Code, canary.Body.String())
	}

	mismatch := httptest.NewRecorder()
	mismatchRequest := httptest.NewRequest(http.MethodDelete, "/api/mills/safety/crash-lease/wrong-token", nil)
	mismatchRequest.SetPathValue("token", "wrong-token")
	op.handleSafetyCrashLeaseRelease(mismatch, mismatchRequest)
	if mismatch.Code != http.StatusConflict {
		t.Fatalf("mismatched release status=%d body=%s", mismatch.Code, mismatch.Body.String())
	}
	op.admissionMu.Lock()
	leaseRemains := op.crashLease != nil && op.crashLease.Token == lease.Token
	op.admissionMu.Unlock()
	if !leaseRemains {
		t.Fatal("mismatched release cleared the active crash lease")
	}

	release := httptest.NewRecorder()
	releaseRequest := httptest.NewRequest(http.MethodDelete, "/api/mills/safety/crash-lease/"+lease.Token, nil)
	releaseRequest.SetPathValue("token", lease.Token)
	op.handleSafetyCrashLeaseRelease(release, releaseRequest)
	if release.Code != http.StatusNoContent {
		t.Fatalf("release status=%d body=%s", release.Code, release.Body.String())
	}
	missing := httptest.NewRecorder()
	op.handleSafetyCrashLeaseRelease(missing, releaseRequest)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("second release status=%d, want 404", missing.Code)
	}
}

func TestSafetyCrashLeaseRejectsAdmissionAndUnstableSnapshot(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*operator)
	}{
		{"active admission", func(op *operator) { op.activeAdmissions.Store(1) }},
		{"active source", func(op *operator) {
			op.withActivitySources(namedActivitySource{name: "busy", source: &sequenceActivitySource{values: []int64{1}}})
		}},
		{"source drains across snapshot", func(op *operator) {
			op.withActivitySources(namedActivitySource{name: "transition", source: &sequenceActivitySource{values: []int64{1, 0}}})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			op, cleanup := newTestOperator(t)
			defer cleanup()
			seedCrashLeaseWorkflow(t, op, "wf-canary-unsafe")
			test.setup(op)
			recorder := requestCrashLease(t, op, "request-unsafe", "wf-canary-unsafe", "spawn-unsafe")
			if recorder.Code != http.StatusConflict {
				t.Fatalf("unsafe lease status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			op.admissionMu.Lock()
			leaseRemains := op.crashLease != nil
			op.admissionMu.Unlock()
			if leaseRemains {
				t.Fatal("rejected proof left its provisional lease active")
			}
		})
	}
}

func TestSafetyCrashLeaseRejectsPolicyGenerationDrift(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedCrashLeaseWorkflow(t, op, "wf-canary-policy-drift")
	var calls int
	op.withActivitySources(namedActivitySource{name: "policy-drift", source: activitySourceFunc(func() int64 {
		calls++
		if calls == 1 {
			op.policyGeneration.Add(1)
		}
		return 0
	})})
	recorder := requestCrashLease(t, op, "request-drift", "wf-canary-policy-drift", "spawn-drift")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("policy-drift lease status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestSafetyCrashLeaseExpiresFailClosedThenAllowsFreshProof(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedCrashLeaseWorkflow(t, op, "wf-canary-expiry")
	first := requestCrashLease(t, op, "request-expired", "wf-canary-expiry", "spawn-expired")
	if first.Code != http.StatusOK {
		t.Fatalf("first lease status=%d body=%s", first.Code, first.Body.String())
	}
	oldToken := decodeCrashLease(t, first).Token
	op.admissionMu.Lock()
	op.crashLease.ExpiresAt = time.Now().Add(-time.Second)
	op.admissionMu.Unlock()
	expiredRenew := httptest.NewRecorder()
	expiredRenewRequest := httptest.NewRequest(http.MethodPost,
		"/api/mills/safety/crash-lease/"+oldToken+"/renew", nil)
	expiredRenewRequest.SetPathValue("token", oldToken)
	op.handleSafetyCrashLeaseRenew(expiredRenew, expiredRenewRequest)
	if expiredRenew.Code != http.StatusNotFound {
		t.Fatalf("expired renewal status=%d, want 404", expiredRenew.Code)
	}
	fresh := requestCrashLease(t, op, "request-fresh", "wf-canary-expiry", "spawn-expired")
	if fresh.Code != http.StatusOK {
		t.Fatalf("fresh lease status=%d body=%s", fresh.Code, fresh.Body.String())
	}
	if newToken := decodeCrashLease(t, fresh).Token; newToken == oldToken || newToken == "" {
		t.Fatalf("expired lease token was reused: old=%q new=%q", oldToken, newToken)
	}
}
