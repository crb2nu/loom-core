package main

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

type safetyQuiescenceResponse struct {
	ObservedAt time.Time              `json:"observed_at"`
	Quiescent  bool                   `json:"quiescent"`
	Counts     store.QuiescenceCounts `json:"counts"`
	InMemory   safetyInMemoryActivity `json:"in_memory"`
}

type safetyInMemoryActivity struct {
	AdmissionClosed      bool                `json:"admission_closed"`
	CrashLeaseActive     bool                `json:"crash_lease_active"`
	PolicyGeneration     uint64              `json:"policy_generation"`
	SourcesReady         bool                `json:"sources_ready"`
	SampleStable         bool                `json:"sample_stable"`
	WiringRequired       bool                `json:"wiring_required"`
	ActivitySources      int                 `json:"activity_sources"`
	SourceGeneration     uint64              `json:"source_generation"`
	SourceOperations     map[string]int64    `json:"source_operations"`
	SourceRunIDs         map[string][]string `json:"source_run_ids"`
	MissingSources       []string            `json:"missing_sources"`
	WiringError          string              `json:"wiring_error,omitempty"`
	ActiveAdmissions     int64               `json:"active_admissions"`
	CanaryAdmissions     int64               `json:"canary_admissions"`
	BackgroundOperations int64               `json:"background_operations"`
	SpinWorkers          int64               `json:"spin_workers"`
	AuditOutstanding     int64               `json:"audit_outstanding"`

	policyIdentity *mills.Policy
}

func durableCountsIdleForWorkflow(counts store.QuiescenceCounts, expected int) bool {
	return counts.QueuedBacklog == 0 && counts.ActivePipelineRuns == 0 &&
		counts.ActiveWorkflowRuns == expected && counts.ActiveSpinningRoomRuns == 0 &&
		counts.ActiveCouncilRuns == 0 && counts.ActiveCrossRepoRuns == 0 && counts.PendingDispatches == 0
}

func (a safetyInMemoryActivity) Quiescent() bool {
	return a.SampleStable && a.valuesIdle()
}

func (a safetyInMemoryActivity) valuesIdle() bool {
	return a.valuesIdleWithAllowances(0, "", false)
}

func (a safetyInMemoryActivity) valuesIdleWithCanaryAllowance(allowed int64) bool {
	return a.valuesIdleWithAllowances(allowed, "", false)
}

func (a safetyInMemoryActivity) valuesIdleForWorkflowCrashLease(runID string) bool {
	return a.valuesIdleWithAllowances(0, runID, true)
}

func (a safetyInMemoryActivity) valuesIdleWithAllowances(allowedCanary int64, workflowRunID string, allowCrashLease bool) bool {
	wiringComplete := !a.WiringRequired || (a.SourcesReady && a.ActivitySources >= len(requiredActivitySourceNames) &&
		a.SourceGeneration > 0 && len(a.MissingSources) == 0 && a.WiringError == "")
	workflowActive := a.SourceOperations[activitySourceWorkflow]
	workflowAllowed := workflowRunID != "" && ((workflowActive == 0 && len(a.SourceRunIDs[activitySourceWorkflow]) == 0) ||
		(workflowActive == 1 && len(a.SourceRunIDs[activitySourceWorkflow]) == 1 &&
			a.SourceRunIDs[activitySourceWorkflow][0] == workflowRunID))
	if workflowRunID == "" {
		workflowAllowed = workflowActive == 0 && len(a.SourceRunIDs[activitySourceWorkflow]) == 0
	}
	return a.AdmissionClosed && (allowCrashLease || !a.CrashLeaseActive) && wiringComplete &&
		sourceOperationsIdleExceptWorkflow(a.SourceOperations) &&
		workflowAllowed && a.ActiveAdmissions == 0 && a.BackgroundOperations == workflowActive &&
		a.CanaryAdmissions == allowedCanary && a.SpinWorkers == 0 && a.AuditOutstanding == 0
}

func sourceOperationsIdleExceptWorkflow(operations map[string]int64) bool {
	for name, active := range operations {
		if name == activitySourceWorkflow {
			continue
		}
		if active != 0 {
			return false
		}
	}
	return true
}

func (o *operator) inMemoryActivity() safetyInMemoryActivity {
	// Serialize with requireWorkAdmission's check+increment boundary. Once the
	// effective policy is false, a zero here means no pre-barrier request can
	// still appear after this snapshot.
	o.admissionMu.Lock()
	activeAdmissions := o.activeAdmissions.Load()
	crashLeaseActive := o.crashLeaseActiveLocked(time.Now().UTC())
	sources := make(map[string]activeOperationSource, len(o.activitySources))
	for name, source := range o.activitySources {
		sources[name] = source
	}
	sourcesReady := o.activityReady
	wiringRequired := o.activityWiringRequired
	wiringError := o.activityWiringError
	sourceGeneration := o.activityGeneration
	policy := o.policy.Current()
	policyGeneration := o.policyGeneration.Load()
	o.admissionMu.Unlock()
	sourceOperations := make(map[string]int64, len(sources))
	sourceRunIDs := make(map[string][]string)
	backgroundOperations := int64(0)
	for name, source := range sources {
		if source == nil {
			continue
		}
		active := int64(0)
		if snapshotSource, ok := source.(interface{ ActiveOperationSnapshot() (int64, []string) }); ok {
			var ids []string
			active, ids = snapshotSource.ActiveOperationSnapshot()
			sourceRunIDs[name] = ids
		} else {
			active = source.ActiveOperations()
			if runSource, ok := source.(interface{ ActiveRunIDs() []string }); ok {
				sourceRunIDs[name] = runSource.ActiveRunIDs()
			}
		}
		sourceOperations[name] = active
		backgroundOperations += active
	}
	missingSources := missingRequiredActivitySources(sources)
	sort.Strings(missingSources)
	auditOutstanding := int64(0)
	if o.auditWorker != nil {
		auditOutstanding = o.auditWorker.Activity()
	}
	return safetyInMemoryActivity{
		AdmissionClosed:      policy != nil && !policy.IsEnabled(),
		CrashLeaseActive:     crashLeaseActive,
		PolicyGeneration:     policyGeneration,
		SourcesReady:         sourcesReady,
		WiringRequired:       wiringRequired,
		ActivitySources:      len(sourceOperations),
		SourceGeneration:     sourceGeneration,
		SourceOperations:     sourceOperations,
		SourceRunIDs:         sourceRunIDs,
		MissingSources:       missingSources,
		WiringError:          wiringError,
		ActiveAdmissions:     activeAdmissions,
		CanaryAdmissions:     o.canaryAdmissions.Load(),
		BackgroundOperations: backgroundOperations,
		SpinWorkers:          o.spinWorkers.Load(),
		AuditOutstanding:     auditOutstanding,
		policyIdentity:       policy,
	}
}

func sameSafetyActivity(a, b safetyInMemoryActivity) bool {
	if a.AdmissionClosed != b.AdmissionClosed || a.CrashLeaseActive != b.CrashLeaseActive ||
		a.PolicyGeneration != b.PolicyGeneration ||
		a.SourcesReady != b.SourcesReady || a.WiringRequired != b.WiringRequired ||
		a.ActivitySources != b.ActivitySources || a.SourceGeneration != b.SourceGeneration ||
		a.WiringError != b.WiringError || a.ActiveAdmissions != b.ActiveAdmissions ||
		a.CanaryAdmissions != b.CanaryAdmissions ||
		a.BackgroundOperations != b.BackgroundOperations || a.SpinWorkers != b.SpinWorkers ||
		a.AuditOutstanding != b.AuditOutstanding || a.policyIdentity != b.policyIdentity ||
		len(a.MissingSources) != len(b.MissingSources) || len(a.SourceOperations) != len(b.SourceOperations) ||
		len(a.SourceRunIDs) != len(b.SourceRunIDs) {
		return false
	}
	for i := range a.MissingSources {
		if a.MissingSources[i] != b.MissingSources[i] {
			return false
		}
	}
	for name, active := range a.SourceOperations {
		other, ok := b.SourceOperations[name]
		if !ok || other != active {
			return false
		}
	}
	for name, ids := range a.SourceRunIDs {
		other, ok := b.SourceRunIDs[name]
		if !ok || len(ids) != len(other) {
			return false
		}
		for i := range ids {
			if ids[i] != other[i] {
				return false
			}
		}
	}
	return true
}

func (o *operator) readSafetyQuiescence(ctx context.Context) (safetyQuiescenceResponse, error) {
	return o.readSafetyQuiescenceWithCanaryAllowance(ctx, 0)
}

func (o *operator) readSafetyQuiescenceWithCanaryAllowance(ctx context.Context, allowedCanaryAdmissions int64) (safetyQuiescenceResponse, error) {
	// Bracket the durable read with two activity samples. This prevents a
	// pre-barrier operation from writing after the DB snapshot and decrementing
	// before the response, which would otherwise synthesize a false all-zero.
	before := o.inMemoryActivity()
	counts, err := o.store.ReadQuiescence(ctx)
	if err != nil {
		return safetyQuiescenceResponse{}, err
	}
	after := o.inMemoryActivity()
	after.SampleStable = sameSafetyActivity(before, after)
	return safetyQuiescenceResponse{
		ObservedAt: time.Now().UTC(),
		Quiescent: counts.Quiescent() && before.valuesIdleWithCanaryAllowance(allowedCanaryAdmissions) &&
			after.SampleStable && after.valuesIdleWithCanaryAllowance(allowedCanaryAdmissions),
		Counts:   counts,
		InMemory: after,
	}, nil
}

// handleSafetyQuiescence returns the exact durable-work snapshot used before
// deliberate operator fault injection. A failed database read is 503 rather
// than a partial response so automation cannot interpret missing counts as
// idle work.
func (o *operator) handleSafetyQuiescence(w http.ResponseWriter, r *http.Request) {
	// A cached zero-count snapshot could authorize a destructive action after
	// new work starts. Require every caller and intermediary to revalidate.
	w.Header().Set("Cache-Control", "no-store")
	snapshot, err := o.readSafetyQuiescence(r.Context())
	if err != nil {
		if o.logger != nil {
			o.logger.Error("quiescence read failed", "error", err)
		}
		http.Error(w, "quiescence unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
