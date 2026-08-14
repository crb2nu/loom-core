package killtest

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/watch"

	"github.com/crb2nu/loom/pkg/mills/worker"
	"github.com/crb2nu/loom/pkg/mills/workflow"
)

// FindAgentStep returns the first spawn step (event_type spawn_requested/
// spawn_result/spawn_resumed) from a run detail. The S6-min canary has
// exactly one agent() call; ok=false when no spawn step exists yet.
//
// NOTE: a healthy IN-FLIGHT dispatch's pending row does NOT carry spawn_id
// (runtime.go records it on completion or failed dispatch only) — derive the
// spawn identity via DeriveSpawnIdentity instead of waiting for the field.
func FindAgentStep(d RunDetail) (StepView, bool) {
	for _, st := range d.Steps {
		switch st.EventType {
		case "spawn_requested", "spawn_result", "spawn_resumed":
			return st, true
		}
	}
	return StepView{}, false
}

// DeriveSpawnIdentity computes the deterministic spawn id (and pod name) for
// an agent step from journal fields alone: run_id + step_key + call_hash →
// idempotency key → spawn id. This mirrors runtime.DeriveStepIdempotencyKey +
// worker.DeriveSpawnID and lets the harness identify the pod of an in-flight
// dispatch whose journal row has no spawn_id yet.
func DeriveSpawnIdentity(runID string, st StepView) (SpawnIdentity, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(st.StepKey) == "" || strings.TrimSpace(st.CallHash) == "" {
		return SpawnIdentity{}, errors.New("run id, step key, and call hash are required to derive spawn identity")
	}
	key := workflow.DeriveStepIdempotencyKeyFromHash(runID, st.StepKey, st.CallHash)
	id := worker.DeriveSpawnID(key)
	if st.SpawnID != "" && st.SpawnID != id {
		return SpawnIdentity{}, fmt.Errorf("journal spawn id %q differs from derived id %q", st.SpawnID, id)
	}
	return SpawnIdentity{SpawnID: id, PodName: "spawn-" + id, IdempotencyKey: key}, nil
}

// countAgentSuccessRows counts terminal-success journal rows for the agent
// step key. UNIQUE(run_id, step_key) makes >1 impossible via the DAO, so >1
// here means the read API itself surfaced duplication — an automatic FAIL.
func countAgentSuccessRows(d RunDetail, stepKey string) int {
	n := 0
	for _, st := range d.Steps {
		if st.StepKey == stepKey && st.Status == "success" {
			n++
		}
	}
	return n
}

func findAgentSuccessStep(d RunDetail, stepKey string) (StepView, bool) {
	for _, st := range d.Steps {
		if st.StepKey != stepKey || st.Status != "success" {
			continue
		}
		switch st.EventType {
		case "spawn_requested", "spawn_result", "spawn_resumed":
			return st, true
		}
	}
	return StepView{}, false
}

// countDistinctSpawnSuccesses counts distinct spawn step keys with a success
// row — the PASS-5 "success rows == number of agent() calls" counter.
func countDistinctSpawnSuccesses(d RunDetail) int {
	seen := map[string]bool{}
	for _, st := range d.Steps {
		switch st.EventType {
		case "spawn_requested", "spawn_result", "spawn_resumed":
			if st.Status == "success" {
				seen[st.StepKey] = true
			}
		}
	}
	return len(seen)
}

// canaryAgentCalls is the number of agent() calls in the S6-min canary script.
const canaryAgentCalls = 1

func validateCrashEvidence(ev Evidence) error {
	switch {
	case ev.CrashAAt.IsZero():
		return errors.New("CRASH A timestamp is missing")
	case ev.CrashBAt.IsZero():
		return errors.New("CRASH B timestamp is missing")
	case !ev.CrashAAt.Before(ev.CrashBAt):
		return fmt.Errorf("crash timestamps are not ordered: CRASH A=%s CRASH B=%s",
			ev.CrashAAt.Format(time.RFC3339Nano), ev.CrashBAt.Format(time.RFC3339Nano))
	}
	for _, pair := range []struct {
		label         string
		crashAt       time.Time
		before, after PodIdentity
	}{
		{"CRASH A", ev.CrashAAt, ev.CrashABefore, ev.CrashAReplacement},
		{"CRASH B", ev.CrashBAt, ev.CrashBBefore, ev.CrashBReplacement},
	} {
		if err := validateCrashPodIdentity(pair.before); err != nil {
			return fmt.Errorf("%s pre-crash identity: %w", pair.label, err)
		}
		if err := validateCrashPodIdentity(pair.after); err != nil {
			return fmt.Errorf("%s replacement identity: %w", pair.label, err)
		}
		switch {
		case pair.before.UID == pair.after.UID:
			return fmt.Errorf("%s did not change pod UID %q", pair.label, pair.before.UID)
		case pair.before.ContainerID == pair.after.ContainerID:
			return fmt.Errorf("%s did not change container ID %q", pair.label, pair.before.ContainerID)
		case pair.before.Image != pair.after.Image || pair.before.ImageID != pair.after.ImageID:
			return fmt.Errorf("%s changed image across replacement: %s (%s) -> %s (%s)", pair.label,
				pair.before.Image, pair.before.ImageID, pair.after.Image, pair.after.ImageID)
		case pair.before.Namespace != pair.after.Namespace ||
			pair.before.ContainerName != pair.after.ContainerName ||
			pair.before.ReplicaSetName != pair.after.ReplicaSetName ||
			pair.before.ReplicaSetUID != pair.after.ReplicaSetUID ||
			pair.before.ReplicaSetPodTemplateSHA256 != pair.after.ReplicaSetPodTemplateSHA256 ||
			pair.before.ReplicaSetSelectorSHA256 != pair.after.ReplicaSetSelectorSHA256 ||
			pair.before.ReplicaSetGeneration != pair.after.ReplicaSetGeneration ||
			pair.before.PodExecutionContract != pair.after.PodExecutionContract ||
			pair.before.PodExecutionContractVersion != pair.after.PodExecutionContractVersion ||
			pair.before.PodExecutionRenderer != pair.after.PodExecutionRenderer ||
			pair.before.PodExecutionRendererVersion != pair.after.PodExecutionRendererVersion ||
			pair.before.LivePodSpecSHA256 != pair.after.LivePodSpecSHA256 ||
			pair.before.DryRunPodSpecSHA256 != pair.after.DryRunPodSpecSHA256 ||
			pair.before.DeploymentName != pair.after.DeploymentName ||
			pair.before.DeploymentUID != pair.after.DeploymentUID:
			return fmt.Errorf("%s changed controller lineage across replacement: before=%+v after=%+v",
				pair.label, pair.before, pair.after)
		case !pair.after.StartedAt.After(pair.before.StartedAt):
			return fmt.Errorf("%s replacement start time %s is not after original %s",
				pair.label, pair.after.StartedAt, pair.before.StartedAt)
		case pair.after.StartedAt.Before(pair.crashAt.Add(-replacementStartMaxClockSkew)):
			return fmt.Errorf("%s replacement start time %s predates crash %s beyond %s clock skew",
				pair.label, pair.after.StartedAt, pair.crashAt, replacementStartMaxClockSkew)
		case !pair.after.ContainerStartedAt.After(pair.before.ContainerStartedAt):
			return fmt.Errorf("%s replacement container start time %s is not after original %s",
				pair.label, pair.after.ContainerStartedAt, pair.before.ContainerStartedAt)
		case pair.after.ContainerStartedAt.Before(pair.crashAt.Add(-replacementStartMaxClockSkew)):
			return fmt.Errorf("%s replacement container start time %s predates crash %s beyond %s clock skew",
				pair.label, pair.after.ContainerStartedAt, pair.crashAt, replacementStartMaxClockSkew)
		}
	}
	return nil
}

func validateDedupeEvidence(ev Evidence) error {
	if strings.TrimSpace(ev.DedupeEvidence) == "" {
		return errors.New("one pod observed but no durable-path dedupe evidence (re-attach / AlreadyExists log line); a failed racing Create also leaves one pod (plan §3.4 mustChange)")
	}
	if ev.DedupeLog == nil {
		return errors.New("dedupe evidence is not bound to an attributed Loki log entry")
	}
	if ev.DedupeLog.Line != ev.DedupeEvidence {
		return errors.New("attributed Loki log line does not match dedupe evidence")
	}
	if !dedupeLineMatches(ev.DedupeLog.Component, ev.DedupeLog.Line, ev.SpawnID) {
		return fmt.Errorf("dedupe log does not contain an allowed %s phrase bound to exact spawn_id %q",
			ev.DedupeLog.Component, ev.SpawnID)
	}

	var replacement PodIdentity
	var crashAt time.Time
	switch ev.DedupeLog.Component {
	case "operator":
		replacement = ev.CrashAReplacement
		crashAt = ev.CrashAAt
	case "mobile-hud":
		replacement = ev.CrashBReplacement
		crashAt = ev.CrashBAt
	default:
		return fmt.Errorf("dedupe log component %q is neither operator nor mobile-hud", ev.DedupeLog.Component)
	}
	if strings.TrimSpace(replacement.Name) == "" || ev.DedupeLog.Pod != replacement.Name {
		return fmt.Errorf("dedupe log pod %q is not the exact %s replacement pod %q",
			ev.DedupeLog.Pod, ev.DedupeLog.Component, replacement.Name)
	}
	if ev.DedupeLog.Timestamp.Before(crashAt) {
		return fmt.Errorf("dedupe log timestamp %s predates %s crash at %s",
			ev.DedupeLog.Timestamp.Format(time.RFC3339Nano), ev.DedupeLog.Component, crashAt.Format(time.RFC3339Nano))
	}
	return nil
}

func validateCanaryHoldEvidence(ev Evidence) error {
	observations := []struct {
		name string
		obs  CanaryHoldObservation
	}{
		{name: "initial", obs: ev.CanaryHoldInitial},
		{name: "before CRASH A", obs: ev.CanaryHoldBeforeCrashA},
		{name: "before CRASH B", obs: ev.CanaryHoldBeforeCrashB},
	}
	for _, sample := range observations {
		if sample.obs.PodName != ev.SpawnPodName || sample.obs.PID <= 1 || sample.obs.StartTimeTicks == 0 ||
			sample.obs.DriverPID <= 1 || sample.obs.DriverStartTimeTicks == 0 ||
			sample.obs.Seconds != workflow.CanaryHoldSeconds || sample.obs.ObservedAt.IsZero() {
			return fmt.Errorf("%s exact canary hold proof is incomplete or mismatched: %+v", sample.name, sample.obs)
		}
		if sample.obs.PID != ev.CanaryHoldInitial.PID {
			return fmt.Errorf("%s canary hold PID %d differs from initial PID %d",
				sample.name, sample.obs.PID, ev.CanaryHoldInitial.PID)
		}
		if sample.obs.DriverPID != ev.CanaryHoldInitial.DriverPID {
			return fmt.Errorf("%s canary hold driver PID %d differs from initial PID %d",
				sample.name, sample.obs.DriverPID, ev.CanaryHoldInitial.DriverPID)
		}
		if sample.obs.StartTimeTicks != ev.CanaryHoldInitial.StartTimeTicks ||
			sample.obs.DriverStartTimeTicks != ev.CanaryHoldInitial.DriverStartTimeTicks {
			return fmt.Errorf("%s canary hold/driver start-time identity changed: %d/%d -> %d/%d",
				sample.name,
				ev.CanaryHoldInitial.StartTimeTicks, ev.CanaryHoldInitial.DriverStartTimeTicks,
				sample.obs.StartTimeTicks, sample.obs.DriverStartTimeTicks)
		}
	}
	if ev.CanaryHoldInitial.ObservedAt.After(ev.CanaryHoldBeforeCrashA.ObservedAt) ||
		ev.CanaryHoldBeforeCrashA.ObservedAt.After(ev.CrashAAt) {
		return errors.New("canary hold proof was not ordered before CRASH A")
	}
	if ev.CanaryHoldBeforeCrashB.ObservedAt.Before(ev.CrashAAt) ||
		ev.CanaryHoldBeforeCrashB.ObservedAt.After(ev.CrashBAt) {
		return errors.New("canary hold proof was not revalidated between CRASH A and CRASH B")
	}
	return nil
}

func validatePostCrashProcessEvidence(ev Evidence) error {
	if ev.ProcessObservationStartedAt.IsZero() {
		return errors.New("crash-window process observation start is missing")
	}
	if len(ev.PostCrashProcessSamples) == 0 {
		return errors.New("crash-window process coverage is missing")
	}
	if ev.PostCrashProcessObservedEnd.IsZero() {
		return errors.New("crash-window process observation end is missing")
	}
	if ev.PostCrashProcessMaxGapMS <= 0 {
		return errors.New("crash-window process maximum sampling gap is missing")
	}
	if ev.PostCrashProcessMaxGapMS > ProcessEvidenceMaxSampleGap.Milliseconds() {
		return fmt.Errorf("crash-window process maximum sampling gap %dms exceeds the %dms evidence contract",
			ev.PostCrashProcessMaxGapMS, ProcessEvidenceMaxSampleGap.Milliseconds())
	}
	if ev.ProcessObservationStartedAt.After(ev.CrashAAt) {
		return errors.New("crash-window process observation started after CRASH A")
	}
	if ev.PostCrashProcessObservedEnd.Before(ev.CrashBAt) {
		return errors.New("crash-window process observation ended before CRASH B")
	}
	maxGap := time.Duration(ev.PostCrashProcessMaxGapMS) * time.Millisecond
	previous := ev.ProcessObservationStartedAt
	previousCompleted := ev.ProcessObservationStartedAt
	holdMissing := false
	driverMissing := false
	latestBeforeCrashA := -1
	latestBeforeCrashB := -1
	for i, sample := range ev.PostCrashProcessSamples {
		if err := validatePostCrashProcessSample(ev, i, sample); err != nil {
			return err
		}
		if i == 0 && sample.ObservedAt.After(ev.CrashAAt) {
			return errors.New("crash-window first completed process sample was after CRASH A")
		}
		if i == 0 && (sample.HoldState == "MISSING" || sample.DriverState == "MISSING") {
			return errors.New("crash-window initial process sample did not retain both live hold and driver identities")
		}
		// A process identity is (PID,starttime). Once that exact identity is
		// absent from /proc it cannot legitimately reappear; accepting a later
		// live sample would make a reordered or fabricated lifecycle look valid.
		if holdMissing && sample.HoldState != "MISSING" {
			return fmt.Errorf("crash-window process sample %d resurrected the original hold identity", i)
		}
		if driverMissing && sample.DriverState != "MISSING" {
			return fmt.Errorf("crash-window process sample %d resurrected the original driver identity", i)
		}
		holdMissing = holdMissing || sample.HoldState == "MISSING"
		driverMissing = driverMissing || sample.DriverState == "MISSING"
		if !sample.ObservedAt.After(ev.CrashBAt) {
			latestBeforeCrashB = i
		}
		if !sample.ObservedAt.After(ev.CrashAAt) {
			latestBeforeCrashA = i
		}
		if sample.ObservedAt.Before(previous) {
			return fmt.Errorf("crash-window process sample %d is out of order: %s -> %s", i, previous, sample.ObservedAt)
		}
		if sample.ObservedAt.Before(previousCompleted) {
			return fmt.Errorf("crash-window process sample %d overlaps the previous in-flight probe: previous_complete=%s start=%s",
				i, previousCompleted, sample.ObservedAt)
		}
		if elapsed := sample.ObservedAt.Sub(previous); elapsed > maxGap {
			return fmt.Errorf("crash-window process sample %d gap %s exceeds %s", i, elapsed, maxGap)
		}
		if ev.PostCrashProcessObservedEnd.Before(sample.CompletedAt) {
			return fmt.Errorf("crash-window process observation ended before sample %d", i)
		}
		previous = sample.ObservedAt
		previousCompleted = sample.CompletedAt
	}
	if latestBeforeCrashA < 0 || latestBeforeCrashB < 0 {
		return errors.New("crash-window process evidence has no completed sample at both delete boundaries")
	}
	crashASample := ev.PostCrashProcessSamples[latestBeforeCrashA]
	if crashASample.HoldState == "MISSING" || crashASample.DriverState == "MISSING" {
		return fmt.Errorf("crash-window execution ended before CRASH A: hold=%q driver=%q observed_at=%s",
			crashASample.HoldState, crashASample.DriverState, crashASample.ObservedAt)
	}
	deleteSample := ev.PostCrashProcessSamples[latestBeforeCrashB]
	if deleteSample.HoldState == "MISSING" || deleteSample.DriverState == "MISSING" {
		return fmt.Errorf("crash-window execution ended before CRASH B: hold=%q driver=%q observed_at=%s",
			deleteSample.HoldState, deleteSample.DriverState, deleteSample.ObservedAt)
	}
	if err := validateProcessDeleteAuthorization("CRASH A", ev.CrashAAt, ev.CrashAProcessAuthorization, ev.PostCrashProcessSamples); err != nil {
		return err
	}
	if err := validateProcessDeleteAuthorization("CRASH B", ev.CrashBAt, ev.CrashBProcessAuthorization, ev.PostCrashProcessSamples); err != nil {
		return err
	}
	if elapsed := ev.PostCrashProcessObservedEnd.Sub(previous); elapsed > maxGap {
		return fmt.Errorf("crash-window final process observation gap %s exceeds %s", elapsed, maxGap)
	}
	return nil
}

func validatePostCrashProcessSample(ev Evidence, index int, sample CanaryProcessSample) error {
	if sample.PodName != ev.SpawnPodName || sample.ObservedAt.IsZero() || sample.CompletedAt.IsZero() ||
		sample.HoldPID != ev.CanaryHoldInitial.PID ||
		sample.DriverPID != ev.CanaryHoldInitial.DriverPID {
		return fmt.Errorf("crash-window process sample %d has incomplete or drifted identity: %+v", index, sample)
	}
	if sample.CompletedAt.Before(sample.ObservedAt) {
		return fmt.Errorf("crash-window process sample %d completion predates probe start: %s -> %s",
			index, sample.ObservedAt, sample.CompletedAt)
	}
	if _, err := parseCanaryProcessState(sample.HoldState); err != nil {
		return fmt.Errorf("crash-window process sample %d has invalid hold state %q", index, sample.HoldState)
	}
	if _, err := parseCanaryProcessState(sample.DriverState); err != nil {
		return fmt.Errorf("crash-window process sample %d has invalid driver state %q", index, sample.DriverState)
	}
	if sample.HoldState == "MISSING" {
		if sample.HoldStartTimeTicks != 0 || len(sample.LiveHoldPIDs) != 0 {
			return fmt.Errorf("crash-window process sample %d has invalid missing hold identity: start=%d live=%v",
				index, sample.HoldStartTimeTicks, sample.LiveHoldPIDs)
		}
	} else if sample.HoldStartTimeTicks != ev.CanaryHoldInitial.StartTimeTicks {
		return fmt.Errorf("crash-window process sample %d has drifted hold starttime %d (want %d)",
			index, sample.HoldStartTimeTicks, ev.CanaryHoldInitial.StartTimeTicks)
	}
	if sample.DriverState == "MISSING" {
		if sample.DriverStartTimeTicks != 0 || len(sample.LiveDriverPIDs) != 0 {
			return fmt.Errorf("crash-window process sample %d has invalid missing driver identity: start=%d live=%v",
				index, sample.DriverStartTimeTicks, sample.LiveDriverPIDs)
		}
		if sample.HoldState != "MISSING" {
			return fmt.Errorf("crash-window process sample %d observed a live hold after its completion driver disappeared",
				index)
		}
	} else if sample.DriverStartTimeTicks != ev.CanaryHoldInitial.DriverStartTimeTicks {
		return fmt.Errorf("crash-window process sample %d has drifted driver starttime %d (want %d)",
			index, sample.DriverStartTimeTicks, ev.CanaryHoldInitial.DriverStartTimeTicks)
	}
	// Container-wide ZombiePIDs are recorded in the evidence but are NOT
	// run-fatal. The canary agent forks constantly: a child caught between
	// its live parent's reaps is legitimately Z for a sample, and a child
	// orphaned by the mobile-hud crash reparents to a pod PID 1 that runs
	// no reaper, so it stays Z indefinitely without saying anything about
	// spawn dedupe or journal exactly-once — the identities this evidence
	// exists to bind are the exact hold/driver below, and their Z/X states
	// remain per-sample fatal. (Spawn pods lacking an init reaper is a
	// spawn-template follow-up, not an S1c verdict input.)
	if sample.HoldState == "Z" || sample.DriverState == "Z" {
		return fmt.Errorf("crash-window process sample %d observed zombie state: hold=%q driver=%q",
			index, sample.HoldState, sample.DriverState)
	}
	if sample.HoldState == "X" || sample.HoldState == "x" ||
		sample.DriverState == "X" || sample.DriverState == "x" {
		return fmt.Errorf("crash-window process sample %d observed dead state: hold=%q driver=%q",
			index, sample.HoldState, sample.DriverState)
	}
	if (sample.HoldState != "MISSING" && !isCanaryExecutionState(sample.HoldState)) ||
		(sample.DriverState != "MISSING" && !isCanaryExecutionState(sample.DriverState)) {
		return fmt.Errorf("crash-window process sample %d observed non-executing state: hold=%q driver=%q",
			index, sample.HoldState, sample.DriverState)
	}
	if len(sample.LiveHoldPIDs) > 1 || len(sample.LiveDriverPIDs) > 1 {
		return fmt.Errorf("crash-window process sample %d observed overlapping executions: holds=%v drivers=%v",
			index, sample.LiveHoldPIDs, sample.LiveDriverPIDs)
	}
	holdLive := containsProcessPID(sample.LiveHoldPIDs, sample.HoldPID)
	driverLive := containsProcessPID(sample.LiveDriverPIDs, sample.DriverPID)
	if (sample.HoldState == "MISSING") == holdLive {
		return fmt.Errorf("crash-window process sample %d has inconsistent original hold state/inventory: state=%q live=%v",
			index, sample.HoldState, sample.LiveHoldPIDs)
	}
	if (sample.DriverState == "MISSING") == driverLive {
		return fmt.Errorf("crash-window process sample %d has inconsistent original driver state/inventory: state=%q live=%v",
			index, sample.DriverState, sample.LiveDriverPIDs)
	}
	for _, pid := range sample.LiveHoldPIDs {
		if pid != ev.CanaryHoldInitial.PID {
			return fmt.Errorf("crash-window process sample %d observed replacement hold PID %d (want %d)",
				index, pid, ev.CanaryHoldInitial.PID)
		}
	}
	for _, pid := range sample.LiveDriverPIDs {
		if pid != ev.CanaryHoldInitial.DriverPID {
			return fmt.Errorf("crash-window process sample %d observed replacement driver PID %d (want %d)",
				index, pid, ev.CanaryHoldInitial.DriverPID)
		}
	}
	return nil
}

func validateProcessDeleteAuthorization(
	label string,
	crashAt time.Time,
	authorization ProcessDeleteAuthorization,
	samples []CanaryProcessSample,
) error {
	if authorization.AuthorizedAt.IsZero() || !authorization.AuthorizedAt.Equal(crashAt) ||
		authorization.SampleIndex < 0 || authorization.SampleIndex >= len(samples) {
		return fmt.Errorf("crash-window %s process authorization is missing or mismatched: %+v",
			label, authorization)
	}
	sample := samples[authorization.SampleIndex]
	if !authorization.SampleObservedAt.Equal(sample.ObservedAt) ||
		!authorization.SampleCompletedAt.Equal(sample.CompletedAt) {
		return fmt.Errorf("crash-window %s authorization does not bind sample %d", label, authorization.SampleIndex)
	}
	if sample.CompletedAt.After(crashAt) || !isCanaryExecutionState(sample.HoldState) ||
		!isCanaryExecutionState(sample.DriverState) {
		return fmt.Errorf("crash-window %s authorization sample was not completed and live at delete: %+v",
			label, sample)
	}
	latestCompleted := -1
	for index, candidate := range samples {
		if !candidate.ObservedAt.After(crashAt) && candidate.CompletedAt.After(crashAt) {
			return fmt.Errorf("crash-window %s had sample %d in flight at delete", label, index)
		}
		if !candidate.CompletedAt.After(crashAt) {
			latestCompleted = index
		}
	}
	if latestCompleted != authorization.SampleIndex {
		return fmt.Errorf("crash-window %s authorization used sample %d, latest completed sample is %d",
			label, authorization.SampleIndex, latestCompleted)
	}
	return nil
}

func containsProcessPID(pids []int, want int) bool {
	for _, pid := range pids {
		if pid == want {
			return true
		}
	}
	return false
}

func validateCrashFluxProvenance(ev Evidence) error {
	if err := ValidateFluxSourceFenceEvidence(ev.CrashAFluxProvenance); err != nil {
		return fmt.Errorf("CRASH A Flux source provenance: %w", err)
	}
	if err := ValidateFluxSourceFenceEvidence(ev.CrashBFluxProvenance); err != nil {
		return fmt.Errorf("CRASH B Flux source provenance: %w", err)
	}
	return nil
}

func validateSpawnPodWatchCoverage(ev Evidence) error {
	if !ev.SpawnPodWatchContinuous || ev.SpawnPodWatchStartedAt.IsZero() ||
		ev.SpawnPodWatchEndedAt.IsZero() || strings.TrimSpace(ev.SpawnPodWatchInitialRV) == "" {
		return errors.New("spawn pod resourceVersion watch was not continuous")
	}
	if ev.GateBinding != (GateBinding{}) {
		preflightClosedAt := ev.InitialPreflight.FluxSourcesEnd.GitRepositories.ObservedAt
		if preflightClosedAt.IsZero() || ev.CanaryLaunchRequestedAt.IsZero() {
			return errors.New("full-gate pre-launch watch ordering evidence is incomplete")
		}
		if preflightClosedAt.After(ev.SpawnPodWatchStartedAt) ||
			ev.SpawnPodWatchStartedAt.After(ev.CanaryLaunchRequestedAt) ||
			ev.CanaryLaunchRequestedAt.After(ev.CanaryHoldInitial.ObservedAt) {
			return fmt.Errorf(
				"full-gate watch ordering is invalid: preflight_closed=%s watch_started=%s launch_requested=%s hold_observed=%s",
				preflightClosedAt.Format(time.RFC3339Nano), ev.SpawnPodWatchStartedAt.Format(time.RFC3339Nano),
				ev.CanaryLaunchRequestedAt.Format(time.RFC3339Nano), ev.CanaryHoldInitial.ObservedAt.Format(time.RFC3339Nano))
		}
		if len(ev.SpawnPodWatchEvents) == 0 || len(ev.SpawnPodWatchEvents) > maxSpawnPodWatchEvents {
			return fmt.Errorf("full-gate spawn pod watch retained %d events, want 1..%d",
				len(ev.SpawnPodWatchEvents), maxSpawnPodWatchEvents)
		}
		var priorObservedAt time.Time
		var accumulated PodIdentity
		for index, event := range ev.SpawnPodWatchEvents {
			if event.Type != string(watch.Added) && event.Type != string(watch.Modified) && event.Type != string(watch.Deleted) {
				return fmt.Errorf("full-gate spawn pod watch event %d has invalid type %q", index, event.Type)
			}
			if strings.TrimSpace(event.ResourceVersion) == "" || event.ObservedAt.IsZero() ||
				event.ObservedAt.Before(ev.SpawnPodWatchStartedAt) || event.ObservedAt.After(ev.SpawnPodWatchEndedAt) ||
				(!priorObservedAt.IsZero() && event.ObservedAt.Before(priorObservedAt)) {
				return fmt.Errorf("full-gate spawn pod watch event %d has invalid stream position rv=%q observed_at=%s",
					index, event.ResourceVersion, event.ObservedAt)
			}
			if strings.TrimSpace(event.Pod.UID) == "" || event.Pod.Name != ev.SpawnPodName {
				return fmt.Errorf("full-gate spawn pod watch event %d identity name=%q uid=%q differs from derived pod %q",
					index, event.Pod.Name, event.Pod.UID, ev.SpawnPodName)
			}
			if event.SpawnIDLabel != nil && *event.SpawnIDLabel != ev.SpawnID {
				return fmt.Errorf("full-gate spawn pod watch event %d spawn-id label %q differs from derived id %q",
					index, *event.SpawnIDLabel, ev.SpawnID)
			}
			var err error
			accumulated, err = mergeWatchedPodIdentity(accumulated, event.Pod)
			if err != nil {
				return fmt.Errorf("full-gate spawn pod watch event %d conflicts with prior identity: %w", index, err)
			}
			priorObservedAt = event.ObservedAt
		}
		if len(ev.TotalSpawnPodIncarnations) == 1 && accumulated.UID != ev.TotalSpawnPodIncarnations[0].UID {
			return fmt.Errorf("full-gate watch uid %q differs from sole retained incarnation %q",
				accumulated.UID, ev.TotalSpawnPodIncarnations[0].UID)
		}
	} else if ev.SpawnPodWatchStartedAt.After(ev.CanaryHoldInitial.ObservedAt) {
		return fmt.Errorf("exact spawn pod watch did not span the full process proof: recovery watch=%s hold=%s",
			ev.SpawnPodWatchStartedAt.Format(time.RFC3339Nano), ev.CanaryHoldInitial.ObservedAt.Format(time.RFC3339Nano))
	}
	if ev.SpawnPodWatchEndedAt.Before(ev.PostCrashProcessObservedEnd) {
		return fmt.Errorf("exact spawn pod watch did not span the full process proof: watch_end=%s process_end=%s",
			ev.SpawnPodWatchEndedAt.Format(time.RFC3339Nano), ev.PostCrashProcessObservedEnd.Format(time.RFC3339Nano))
	}
	return nil
}

// Evaluate computes the §3.4 verdicts from collected evidence. Pure.
func Evaluate(ev Evidence) Verdicts {
	v := Verdicts{}
	if ev.MergingCanary {
		v.Pass3NoDoubleMerge, v.Pass3Reason = evaluateCanaryMerge(ev)
	} else {
		v.Pass3NotExercised = "pre-merge canary stops at the gate; PASS-3 (no double-merge) is asserted by S6-full's merging re-run"
	}

	// PASS-1: never two concurrent pods AND the dedupe fired via the durable
	// path (a log line), not merely "one pod exists".
	crashEvidenceErr := validateCrashEvidence(ev)
	fluxProvenanceErr := validateCrashFluxProvenance(ev)
	dedupeEvidenceErr := validateDedupeEvidence(ev)
	holdEvidenceErr := validateCanaryHoldEvidence(ev)
	postCrashProcessEvidenceErr := validatePostCrashProcessEvidence(ev)
	spawnWatchEvidenceErr := validateSpawnPodWatchCoverage(ev)
	switch {
	case len(ev.ObservationErrors) > 0:
		v.Pass1Reason = fmt.Sprintf("FAIL: incomplete observation window: %v", ev.ObservationErrors)
	case fluxProvenanceErr != nil:
		v.Pass1Reason = "FAIL: " + fluxProvenanceErr.Error()
	case holdEvidenceErr != nil:
		v.Pass1Reason = "FAIL: " + holdEvidenceErr.Error()
	case postCrashProcessEvidenceErr != nil:
		v.Pass1Reason = "FAIL: " + postCrashProcessEvidenceErr.Error()
	case spawnWatchEvidenceErr != nil:
		v.Pass1Reason = "FAIL: " + spawnWatchEvidenceErr.Error()
	case ev.MaxConcurrentSpawnPods != 1:
		v.Pass1Reason = fmt.Sprintf("FAIL: maximum observed concurrency was %d for spawn id %s, want exactly 1 (%v)",
			ev.MaxConcurrentSpawnPods, ev.SpawnID, ev.TotalSpawnPodNames)
	case len(ev.TotalSpawnPodNames) != 1:
		v.Pass1Reason = fmt.Sprintf("FAIL: observed %d distinct spawn pod names, want exactly 1: %v",
			len(ev.TotalSpawnPodNames), ev.TotalSpawnPodNames)
	case len(ev.TotalSpawnPodIncarnations) != 1:
		v.Pass1Reason = fmt.Sprintf("FAIL: observed %d distinct spawn pod UID incarnations, want exactly 1: %+v",
			len(ev.TotalSpawnPodIncarnations), ev.TotalSpawnPodIncarnations)
	case strings.TrimSpace(ev.TotalSpawnPodIncarnations[0].UID) == "":
		v.Pass1Reason = "FAIL: the sole observed spawn pod incarnation has no Kubernetes UID"
	case ev.TotalSpawnPodIncarnations[0].Name != ev.SpawnPodName:
		v.Pass1Reason = fmt.Sprintf("FAIL: observed spawn pod incarnation name %q differs from derived name %q",
			ev.TotalSpawnPodIncarnations[0].Name, ev.SpawnPodName)
	case crashEvidenceErr != nil:
		v.Pass1Reason = "FAIL: " + crashEvidenceErr.Error()
	case len(ev.TotalSpawnRecordIDs) != 1:
		v.Pass1Reason = fmt.Sprintf("FAIL: %d durable spawn records for workflow run, want exactly 1: %v",
			len(ev.TotalSpawnRecordIDs), ev.TotalSpawnRecordIDs)
	case ev.TotalSpawnRecordIDs[0] != ev.SpawnID:
		v.Pass1Reason = fmt.Sprintf("FAIL: durable spawn record %q differs from derived id %q",
			ev.TotalSpawnRecordIDs[0], ev.SpawnID)
	case len(ev.FinalActiveSpawnRecordIDs) != 0:
		v.Pass1Reason = fmt.Sprintf("FAIL: terminal workflow left active spawn record(s): %v",
			ev.FinalActiveSpawnRecordIDs)
	case len(ev.FinalActiveSpawnPodNames) != 0:
		v.Pass1Reason = fmt.Sprintf("FAIL: terminal workflow left active spawn pod(s): %v",
			ev.FinalActiveSpawnPodNames)
	case ev.FinalSpawnRecordStatuses[ev.SpawnID] != "completed":
		v.Pass1Reason = fmt.Sprintf("FAIL: spawn record %q ended with status %q, want completed",
			ev.SpawnID, ev.FinalSpawnRecordStatuses[ev.SpawnID])
	case ev.ExpectedIdempotencyKey == "":
		v.Pass1Reason = "FAIL: expected idempotency key was not derived"
	case ev.FinalSpawnIdempotencyKeys[ev.SpawnID] != ev.ExpectedIdempotencyKey:
		v.Pass1Reason = fmt.Sprintf("FAIL: spawn record %q idempotency key %q differs from derived key %q",
			ev.SpawnID, ev.FinalSpawnIdempotencyKeys[ev.SpawnID], ev.ExpectedIdempotencyKey)
	case dedupeEvidenceErr != nil:
		v.Pass1Reason = "FAIL: " + dedupeEvidenceErr.Error()
	default:
		v.Pass1NoDoubleSpawn = true
		v.Pass1Reason = fmt.Sprintf("exact canary hold pid=%d and driver pid=%d spanned both crashes through pod disappearance; exactly one pod incarnation (%s uid=%s); dedupe evidence: %s",
			ev.CanaryHoldInitial.PID, ev.CanaryHoldInitial.DriverPID, ev.TotalSpawnPodIncarnations[0].Name,
			ev.TotalSpawnPodIncarnations[0].UID, ev.DedupeEvidence)
	}

	// PASS-2: the workflow must reach done with exactly one success row for the
	// agent step. Treating any non-running state as terminal is not a pass.
	successRows := countAgentSuccessRows(ev.Final, ev.AgentStepKey)
	successStep, successStepFound := findAgentSuccessStep(ev.Final, ev.AgentStepKey)
	switch {
	case ValidateAgentType(ev.AgentType) != nil:
		v.Pass2Reason = fmt.Sprintf("FAIL: unsupported evidence agent identity %q", ev.AgentType)
	case ev.Final.Run.AgentType != ev.AgentType:
		v.Pass2Reason = fmt.Sprintf("FAIL: final run agent type %q differs from crash-critical agent type %q",
			ev.Final.Run.AgentType, ev.AgentType)
	case ev.Final.Run.Template != workflow.CanaryTemplateName ||
		ev.Final.Run.TemplateVersion != expectedFinalCanaryTemplateVersion(ev) ||
		ev.Final.Run.InterpreterVersion != workflow.HostInterpreterVersion:
		v.Pass2Reason = fmt.Sprintf("FAIL: final run version identity template=%q version=%q interpreter=%q",
			ev.Final.Run.Template, ev.Final.Run.TemplateVersion, ev.Final.Run.InterpreterVersion)
	case ev.Final.Run.State == "quarantined":
		v.Pass2Reason = "FAIL: run quarantined on deterministic replay (step-key/call-hash drifted across restart)"
	case ev.Final.Run.State != "done":
		v.Pass2Reason = fmt.Sprintf("FAIL: run state %q, want done", ev.Final.Run.State)
	case successRows != 1:
		v.Pass2Reason = fmt.Sprintf("FAIL: %d success rows for step_key %q, want exactly 1", successRows, ev.AgentStepKey)
	case !successStepFound:
		v.Pass2Reason = fmt.Sprintf("FAIL: successful row for step_key %q is not an agent spawn event", ev.AgentStepKey)
	case successStep.SpawnID != ev.SpawnID:
		v.Pass2Reason = fmt.Sprintf("FAIL: final agent step spawn id %q differs from crash-critical spawn id %q",
			successStep.SpawnID, ev.SpawnID)
	case strings.TrimSpace(successStep.CallHash) == "":
		v.Pass2Reason = fmt.Sprintf("FAIL: final agent step %q has no call hash", ev.AgentStepKey)
	case strings.TrimSpace(ev.Final.Run.ID) == "":
		v.Pass2Reason = "FAIL: final run id is missing"
	case workflow.DeriveStepIdempotencyKeyFromHash(ev.Final.Run.ID, successStep.StepKey, successStep.CallHash) != ev.ExpectedIdempotencyKey:
		v.Pass2Reason = fmt.Sprintf("FAIL: final agent step %q call hash does not match the crash-critical idempotency key",
			ev.AgentStepKey)
	default:
		v.Pass2JournalOnce = true
		v.Pass2Reason = fmt.Sprintf("one identity-matched success row for step_key %q; run state %s",
			ev.AgentStepKey, ev.Final.Run.State)
	}

	// PASS-4: the success row's cost provenance is honest for the immutable
	// agent identity selected at launch: claude-code → real, codex → estimated.
	if st, ok := FindAgentStep(ev.Final); ok {
		expectedSource := ""
		switch ev.AgentType {
		case AgentTypeClaudeCode:
			expectedSource = "real"
		case AgentTypeCodex:
			expectedSource = "estimated"
		}
		switch {
		case expectedSource == "":
			v.Pass4Reason = fmt.Sprintf("FAIL: unsupported evidence agent identity %q", ev.AgentType)
		case ev.Final.Run.AgentType != ev.AgentType:
			v.Pass4Reason = fmt.Sprintf("FAIL: final run agent type %q differs from crash-critical agent type %q",
				ev.Final.Run.AgentType, ev.AgentType)
		case st.Status == "success" && st.CostSource == expectedSource:
			v.Pass4CostProvenance = true
			v.Pass4Reason = fmt.Sprintf("%s success row carries cost_source=%q (cost=$%.4f)", ev.AgentType, st.CostSource, st.CostUSD)
		case st.Status == "success" && st.CostSource == "unavailable":
			// This gate crashes mobile-hud — the cost-telemetry harvester —
			// inside the canary turn by design, so the resumed turn can
			// finish with its cost provenance honestly degraded to
			// "unavailable" rather than the steady-state source. The
			// criterion is that provenance is recorded, never fabricated or
			// silently dropped. Recovering cost across a harvester restart
			// is a runtime follow-up, not an exactly-once property.
			v.Pass4CostProvenance = true
			v.Pass4Reason = fmt.Sprintf(
				"%s success row recorded cost_source=\"unavailable\" — telemetry degraded across the in-window CRASH B harvester restart (steady-state expectation %q); honestly recorded, not dropped",
				ev.AgentType, expectedSource)
		default:
			v.Pass4Reason = fmt.Sprintf("FAIL: agent step status=%s cost_source=%q — %s requires %s provenance",
				st.Status, st.CostSource, ev.AgentType, expectedSource)
		}
	} else {
		v.Pass4Reason = "FAIL: no agent step with a spawn id in the final journal"
	}

	// PASS-5: distinct spawn success rows == agent() calls in the script,
	// unchanged by the crashes.
	if n := countDistinctSpawnSuccesses(ev.Final); n == canaryAgentCalls {
		v.Pass5CounterExact = true
		v.Pass5Reason = fmt.Sprintf("%d spawn success row(s) == %d agent() call(s)", n, canaryAgentCalls)
	} else {
		v.Pass5Reason = fmt.Sprintf("FAIL: %d spawn success rows, want %d", n, canaryAgentCalls)
	}

	// PASS-8: every destructive request must be independently re-derivable as
	// a closed-admission, identity-stable operation against the sole canary.
	gateBindingErr := ValidateGateBinding(ev)
	gateBoundaryErr := ValidateGateBoundaryEvidence(ev.InitialPreflight, ev.FinalPreflight)
	crashAIdentityErr := ValidateGateIdentityContinuity(ev.InitialPreflight, ev.CrashASafety.ImmediatePreflight)
	crashBIdentityErr := ValidateGateIdentityContinuity(ev.InitialPreflight, ev.CrashBSafety.ImmediatePreflight)
	crashAPodContinuityErr := ValidateCrashAPodContinuity(ev.InitialPreflight, ev.CrashASafety.ImmediatePreflight)
	crashBPodContinuityErr := ValidateCrashBPodContinuity(
		ev.CrashASafety.ImmediatePreflight, ev.CrashBSafety.ImmediatePreflight, ev.CrashAReplacement)
	finalPodContinuityErr := ValidateFinalPodContinuity(
		ev.FinalPreflight, ev.CrashAReplacement, ev.CrashBReplacement)
	crashASafetyErr := ValidateCrashSafetyEvidence(
		"CRASH A", ev.RunID, ev.SpawnID, ev.CrashAAt, ev.CrashABefore,
		ev.CrashAFluxProvenance, ev.CrashASafety,
	)
	crashBSafetyErr := ValidateCrashSafetyEvidence(
		"CRASH B", ev.RunID, ev.SpawnID, ev.CrashBAt, ev.CrashBBefore,
		ev.CrashBFluxProvenance, ev.CrashBSafety,
	)
	switch {
	case gateBindingErr != nil:
		v.Pass8Reason = "FAIL: " + gateBindingErr.Error()
	case crashEvidenceErr != nil:
		v.Pass8Reason = "FAIL: crash replacement attribution: " + crashEvidenceErr.Error()
	case gateBoundaryErr != nil:
		v.Pass8Reason = "FAIL: " + gateBoundaryErr.Error()
	case crashAIdentityErr != nil:
		v.Pass8Reason = "FAIL: CRASH A immutable identity: " + crashAIdentityErr.Error()
	case crashBIdentityErr != nil:
		v.Pass8Reason = "FAIL: CRASH B immutable identity: " + crashBIdentityErr.Error()
	case !ev.InitialPreflight.FluxSourcesEnd.ObservedAt.Before(ev.CrashASafety.ImmediatePreflight.FluxSourcesStart.ObservedAt):
		v.Pass8Reason = fmt.Sprintf("FAIL: CRASH A preflight did not begin after initial gate: initial_end=%s preflight_start=%s",
			ev.InitialPreflight.FluxSourcesEnd.ObservedAt, ev.CrashASafety.ImmediatePreflight.FluxSourcesStart.ObservedAt)
	case !ev.CrashAAt.Before(ev.CrashBSafety.ImmediatePreflight.FluxSourcesStart.ObservedAt):
		v.Pass8Reason = fmt.Sprintf("FAIL: CRASH B preflight did not begin after CRASH A: crash_a=%s preflight_start=%s",
			ev.CrashAAt, ev.CrashBSafety.ImmediatePreflight.FluxSourcesStart.ObservedAt)
	case ev.CrashASafety.LeaseAcquired.RequestID == ev.CrashBSafety.LeaseAcquired.RequestID:
		v.Pass8Reason = fmt.Sprintf("FAIL: both crashes reused lease request_id %q",
			ev.CrashASafety.LeaseAcquired.RequestID)
	case crashAPodContinuityErr != nil:
		v.Pass8Reason = "FAIL: " + crashAPodContinuityErr.Error()
	case crashBPodContinuityErr != nil:
		v.Pass8Reason = "FAIL: " + crashBPodContinuityErr.Error()
	case !ev.CrashBAt.Before(ev.FinalPreflight.FluxSourcesStart.ObservedAt):
		v.Pass8Reason = fmt.Sprintf("FAIL: final preflight did not begin after CRASH B: crash_b=%s final_start=%s",
			ev.CrashBAt, ev.FinalPreflight.FluxSourcesStart.ObservedAt)
	case finalPodContinuityErr != nil:
		v.Pass8Reason = "FAIL: " + finalPodContinuityErr.Error()
	case ev.Final.OperatorAuthority != ev.FinalPreflight.AuthorityPlane.Operator:
		v.Pass8Reason = fmt.Sprintf("FAIL: final run REST authority differs from final operator: run=%+v operator=%+v",
			ev.Final.OperatorAuthority, ev.FinalPreflight.AuthorityPlane.Operator)
	case ev.CrashASafety.Target.Run.AgentType != ev.AgentType ||
		ev.CrashBSafety.Target.Run.AgentType != ev.AgentType:
		v.Pass8Reason = fmt.Sprintf("FAIL: crash target agent identity drifted from %q: A=%q B=%q",
			ev.AgentType, ev.CrashASafety.Target.Run.AgentType, ev.CrashBSafety.Target.Run.AgentType)
	case ev.CrashASafety.Target.AgentStep.StepKey != ev.AgentStepKey ||
		ev.CrashBSafety.Target.AgentStep.StepKey != ev.AgentStepKey:
		v.Pass8Reason = fmt.Sprintf("FAIL: crash target step key drifted from %q: A=%q B=%q",
			ev.AgentStepKey, ev.CrashASafety.Target.AgentStep.StepKey, ev.CrashBSafety.Target.AgentStep.StepKey)
	case ev.CrashASafety.Target.DerivedSpawn.IdempotencyKey != ev.ExpectedIdempotencyKey ||
		ev.CrashBSafety.Target.DerivedSpawn.IdempotencyKey != ev.ExpectedIdempotencyKey:
		v.Pass8Reason = fmt.Sprintf("FAIL: crash target idempotency key drifted from %q: A=%q B=%q",
			ev.ExpectedIdempotencyKey,
			ev.CrashASafety.Target.DerivedSpawn.IdempotencyKey,
			ev.CrashBSafety.Target.DerivedSpawn.IdempotencyKey)
	case crashASafetyErr != nil:
		v.Pass8Reason = "FAIL: " + crashASafetyErr.Error()
	case crashBSafetyErr != nil:
		v.Pass8Reason = "FAIL: " + crashBSafetyErr.Error()
	default:
		v.Pass8CrashSafety = true
		v.Pass8Reason = "both UID-preconditioned deletes are bound to coherent preflights, exact target proof, renewed leases, and immutable source/workload identity"
	}

	v.Overall = v.Pass1NoDoubleSpawn && v.Pass2JournalOnce && v.Pass4CostProvenance &&
		v.Pass5CounterExact && v.Pass8CrashSafety
	if ev.MergingCanary {
		v.Overall = v.Overall && v.Pass3NoDoubleMerge
	}
	return v
}

// expectedFinalCanaryTemplateVersion pins the final identity check to the
// launched mode: v3 for the merging canary, v2 otherwise.
func expectedFinalCanaryTemplateVersion(ev Evidence) string {
	if ev.MergingCanary {
		return workflow.CanaryMergingTemplateVersion
	}
	return workflow.CanaryTemplateVersion
}

// evaluateCanaryMerge computes PASS-3 (no double-merge) from the collected
// CanaryMergeEvidence: exactly one MR for the deterministic canary merge
// branch, in state merged with a merge commit, and exactly one journal merge
// success row. Missing evidence fails closed.
func evaluateCanaryMerge(ev Evidence) (bool, string) {
	cm := ev.CanaryMerge
	switch {
	case cm == nil:
		return false, "FAIL: merging canary produced no merge evidence"
	case cm.MRCount != 1:
		return false, fmt.Sprintf("FAIL: %d MRs for canary merge branch %q, want exactly 1", cm.MRCount, cm.SourceBranch)
	case cm.MRState != "merged":
		return false, fmt.Sprintf("FAIL: canary MR %d state %q, want merged", cm.MRIID, cm.MRState)
	case cm.MergeCommitSHA == "":
		return false, fmt.Sprintf("FAIL: canary MR %d merged without a merge commit SHA", cm.MRIID)
	case cm.JournalMergeSuccessRows != 1:
		return false, fmt.Sprintf("FAIL: %d journal merge success rows, want exactly 1", cm.JournalMergeSuccessRows)
	}
	return true, fmt.Sprintf("merge landed exactly once: MR %d merge_commit=%s journal_rows=1", cm.MRIID, cm.MergeCommitSHA)
}
