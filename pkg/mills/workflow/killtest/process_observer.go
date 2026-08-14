package killtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// CanaryProcessObserver begins synchronously before CRASH A and samples on an
// independent cadence through both singleton replacements and until the exact
// spawn pod disappears. Samples remain private until Record is called, avoiding
// races with evidence checkpoint serialization.
type CanaryProcessObserver struct {
	h         *Harness
	spawnID   string
	initial   CanaryHoldObservation
	startedAt time.Time
	gap       time.Duration
	poll      time.Duration

	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	activateCh chan struct{}

	mu               sync.Mutex
	samples          []CanaryProcessSample
	endedAt          time.Time
	errors           []string
	transient        []string
	transientCount   int
	manualStop       bool
	activated        bool
	sampleInFlight   bool
	sampleStartedAt  time.Time
	sampleDeadlineAt time.Time
}

// transientObservationError marks a sample attempt that failed on the
// observation TRANSPORT — a kubectl call killed or timed out while the cluster
// absorbed crash churn — rather than on evidence integrity. The run loop
// retries these on the next poll tick; beginSample's gap check remains the
// fail-closed arbiter, so if no attempt completes within ProcessMaxSampleGap
// of the last completed sample the window still fails exactly as before.
// Both v5 gate attempts died 2/3 on precisely this: one kubectl call
// exceeding its deadline during the third consecutive dual-crash cycle
// aborted an otherwise contract-clean window. Retryable attempts never
// append evidence — a pass still requires fully-validated samples.
type transientObservationError struct{ err error }

func (e transientObservationError) Error() string { return e.err.Error() }
func (e transientObservationError) Unwrap() error { return e.err }

// transientObservationFailureLimit bounds the stored transient-failure
// messages in evidence; the total count is always recorded.
const transientObservationFailureLimit = 32

// StartCanaryProcessObservation takes the first sample before returning. The
// caller can therefore make HUD deletion conditional on an active observer.
func (h *Harness) StartCanaryProcessObservation(
	ctx context.Context,
	spawnID string,
	initial CanaryHoldObservation,
	startedAt time.Time,
) (*CanaryProcessObserver, error) {
	observer, err := h.StartPausedCanaryProcessObservation(ctx, spawnID, initial, startedAt)
	if err != nil {
		return observer, err
	}
	if err := observer.Activate(); err != nil {
		return observer, fmt.Errorf("start crash-window process observation: activate observer: %w", err)
	}
	return observer, nil
}

// StartPausedCanaryProcessObservation takes and validates the synchronous first
// sample, then waits for Activate before issuing any periodic Kubernetes reads.
// This lets a caller establish the initial process proof, take a final coherent
// source snapshot, then begin observation at the bounded DELETE request boundary.
func (h *Harness) StartPausedCanaryProcessObservation(
	ctx context.Context,
	spawnID string,
	initial CanaryHoldObservation,
	startedAt time.Time,
) (*CanaryProcessObserver, error) {
	if startedAt.IsZero() {
		return nil, errors.New("start crash-window process observation: start timestamp is required")
	}
	if h.cfg.ProcessPollInterval <= 0 || h.cfg.ProcessMaxSampleGap <= 0 ||
		h.cfg.ProcessPollInterval >= h.cfg.ProcessMaxSampleGap ||
		h.cfg.ProcessMaxSampleGap > ProcessEvidenceMaxSampleGap {
		return nil, fmt.Errorf("start crash-window process observation: invalid cadence poll=%s max_gap=%s",
			h.cfg.ProcessPollInterval, h.cfg.ProcessMaxSampleGap)
	}

	observerCtx, cancel := context.WithCancel(ctx)
	observer := &CanaryProcessObserver{
		h: h, spawnID: spawnID, initial: initial, startedAt: startedAt,
		gap: h.cfg.ProcessMaxSampleGap, poll: h.cfg.ProcessPollInterval,
		ctx: observerCtx, cancel: cancel, done: make(chan struct{}), activateCh: make(chan struct{}),
	}
	sampleCtx, sampleStartedAt, finishSample, err := observer.beginSample()
	if err != nil {
		message := "prepare initial crash-window process probe: " + err.Error()
		observer.addError(message)
		cancel()
		close(observer.done)
		return observer, fmt.Errorf("start crash-window process observation: %s", message)
	}
	sample, err := observer.probe(sampleCtx)
	var deadlineErr error
	if err == nil {
		deadlineErr = completedProbeDeadlineError(sampleCtx)
	}
	finishSample()
	if err != nil {
		message := "probe crash-window processes: " + err.Error()
		observer.addError(message)
		cancel()
		close(observer.done)
		return observer, fmt.Errorf("start crash-window process observation: %s", message)
	}
	sample.ObservedAt = sampleStartedAt
	sample.CompletedAt = time.Now().UTC()
	appendErr := observer.appendSample(sample)
	if deadlineErr != nil {
		appendErr = errors.Join(appendErr, fmt.Errorf("crash-window process sampling gap: probe completed after deadline: %w", deadlineErr))
	}
	if appendErr != nil {
		message := "crash-window process invariant: " + appendErr.Error()
		observer.addError(message)
		cancel()
		close(observer.done)
		return observer, fmt.Errorf("start crash-window process observation: %s", message)
	}
	go observer.run()
	return observer, nil
}

// Activate begins periodic sampling. It is safe to call more than once; a
// repeated activation does not start another observer loop.
func (o *CanaryProcessObserver) Activate() error {
	if o == nil {
		return errors.New("crash-window process observer is nil")
	}
	now := time.Now().UTC()
	o.mu.Lock()
	if o.activated {
		o.mu.Unlock()
		return nil
	}
	if o.manualStop {
		o.mu.Unlock()
		return errors.New("crash-window process observer was stopped before activation")
	}
	if len(o.errors) > 0 {
		err := errors.New(strings.Join(o.errors, "; "))
		o.mu.Unlock()
		return err
	}
	if message := o.overdueMessageLocked(now); message != "" {
		o.errors = append(o.errors, message)
		o.mu.Unlock()
		o.cancel()
		return errors.New(message)
	}
	o.activated = true
	close(o.activateCh)
	o.mu.Unlock()
	return nil
}

// AssertFreshForDelete is the no-I/O half of the paused-observer handoff. Call
// it after the final external source read and immediately before returning to
// the UID-preconditioned delete. A slow final fence then fails before mutation
// instead of being discovered only by post-delete activation.
func (o *CanaryProcessObserver) AssertFreshForDelete() error {
	_, err := o.AuthorizePausedDelete(time.Now().UTC())
	return err
}

// AuthorizePausedDelete returns the exact completed live sample used at the
// CRASH A boundary.
func (o *CanaryProcessObserver) AuthorizePausedDelete(at time.Time) (ProcessDeleteAuthorization, error) {
	if o == nil {
		return ProcessDeleteAuthorization{}, errors.New("crash-window process observer is nil")
	}
	o.mu.Lock()
	if o.activated {
		o.mu.Unlock()
		return ProcessDeleteAuthorization{}, errors.New("crash-window process observer activated before the CRASH A source fence")
	}
	if o.manualStop {
		o.mu.Unlock()
		return ProcessDeleteAuthorization{}, errors.New("crash-window process observer was stopped before delete")
	}
	if len(o.errors) > 0 {
		err := errors.New(strings.Join(o.errors, "; "))
		o.mu.Unlock()
		return ProcessDeleteAuthorization{}, err
	}
	if len(o.samples) == 0 || o.sampleInFlight || !o.endedAt.IsZero() {
		o.mu.Unlock()
		return ProcessDeleteAuthorization{}, errors.New("crash-window process observer is not paused on one completed sample")
	}
	authorization, err := o.deleteAuthorizationLocked(at)
	if err != nil {
		o.mu.Unlock()
		return ProcessDeleteAuthorization{}, err
	}
	message := o.overdueMessageLocked(at)
	if message != "" {
		o.errors = append(o.errors, message)
	}
	o.mu.Unlock()
	if message != "" {
		o.cancel()
		return ProcessDeleteAuthorization{}, errors.New(message)
	}
	return authorization, nil
}

// AssertActiveFreshForDelete is the no-I/O freshness gate used before CRASH B.
// The observer must already be active from CRASH A and paused between completed
// samples with both original identities still live.
func (o *CanaryProcessObserver) AssertActiveFreshForDelete() error {
	_, err := o.AuthorizeActiveDelete(time.Now().UTC())
	return err
}

// AuthorizeActiveDelete returns the exact completed live sample used at the
// CRASH B boundary.
func (o *CanaryProcessObserver) AuthorizeActiveDelete(at time.Time) (ProcessDeleteAuthorization, error) {
	if o == nil {
		return ProcessDeleteAuthorization{}, errors.New("crash-window process observer is nil")
	}
	o.mu.Lock()
	if !o.activated {
		o.mu.Unlock()
		return ProcessDeleteAuthorization{}, errors.New("crash-window process observer is not active before delete")
	}
	if o.manualStop {
		o.mu.Unlock()
		return ProcessDeleteAuthorization{}, errors.New("crash-window process observer was stopped before delete")
	}
	if !o.endedAt.IsZero() {
		o.mu.Unlock()
		return ProcessDeleteAuthorization{}, errors.New("crash-window process observer ended before delete")
	}
	if len(o.errors) > 0 {
		err := errors.New(strings.Join(o.errors, "; "))
		o.mu.Unlock()
		return ProcessDeleteAuthorization{}, err
	}
	if len(o.samples) == 0 {
		o.mu.Unlock()
		return ProcessDeleteAuthorization{}, errors.New("crash-window process observer has no completed sample")
	}
	message := o.overdueMessageLocked(at)
	if message != "" {
		o.errors = append(o.errors, message)
		o.mu.Unlock()
		o.cancel()
		return ProcessDeleteAuthorization{}, errors.New(message)
	}
	if o.sampleInFlight {
		o.mu.Unlock()
		return ProcessDeleteAuthorization{}, errors.New("crash-window process observer has an in-flight sample at the delete boundary")
	}
	authorization, err := o.deleteAuthorizationLocked(at)
	if err != nil {
		o.mu.Unlock()
		return ProcessDeleteAuthorization{}, err
	}
	o.mu.Unlock()
	return authorization, nil
}

func (o *CanaryProcessObserver) deleteAuthorizationLocked(at time.Time) (ProcessDeleteAuthorization, error) {
	if len(o.samples) == 0 {
		return ProcessDeleteAuthorization{}, errors.New("crash-window process observer has no completed sample")
	}
	latest := o.samples[len(o.samples)-1]
	if !isCanaryExecutionState(latest.HoldState) || !isCanaryExecutionState(latest.DriverState) {
		return ProcessDeleteAuthorization{}, fmt.Errorf("crash-window execution ended or is not live before delete: hold=%q driver=%q observed_at=%s",
			latest.HoldState, latest.DriverState, latest.ObservedAt)
	}
	if latest.CompletedAt.IsZero() || latest.CompletedAt.Before(latest.ObservedAt) || latest.CompletedAt.After(at) {
		return ProcessDeleteAuthorization{}, fmt.Errorf("crash-window latest sample did not complete before delete: start=%s complete=%s delete=%s",
			latest.ObservedAt, latest.CompletedAt, at)
	}
	return ProcessDeleteAuthorization{
		SampleIndex: len(o.samples) - 1, SampleObservedAt: latest.ObservedAt,
		SampleCompletedAt: latest.CompletedAt, AuthorizedAt: at,
	}, nil
}

func (o *CanaryProcessObserver) run() {
	defer close(o.done)
	select {
	case <-o.ctx.Done():
		o.recordContextEnd()
		return
	case <-o.activateCh:
	}
	timer := time.NewTimer(o.poll)
	defer timer.Stop()
	for {
		select {
		case <-o.ctx.Done():
			o.recordContextEnd()
			return
		case <-timer.C:
			ended, err := o.observeOnce()
			if err != nil {
				var transient transientObservationError
				if errors.As(err, &transient) {
					// Transport flake: retry at the next tick. The
					// next beginSample fails closed on the gap
					// contract if coverage is genuinely broken.
					o.addTransient(err.Error())
					timer.Reset(o.poll)
					continue
				}
				o.addError(err.Error())
				return
			}
			if ended {
				return
			}
			timer.Reset(o.poll)
		}
	}
}

func (o *CanaryProcessObserver) observeOnce() (bool, error) {
	sampleCtx, sampleStartedAt, finishSample, err := o.beginSample()
	if err != nil {
		return false, err
	}
	defer finishSample()

	// Each observation call gets its own attempt bound under the sample
	// deadline: a hung transport call dies early instead of consuming the
	// whole sampling-gap budget, leaving room for the next tick's retry
	// (v6d run 1: one hung probe exec ate the full 3s gap on its own).
	attempt := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(sampleCtx, o.h.cfg.ProcessProbeAttemptTimeout)
	}

	statusCtx, cancelStatus := attempt()
	_, _, names, err := o.h.SpawnPodStatus(statusCtx, o.spawnID)
	cancelStatus()
	if err != nil {
		return false, transientObservationError{fmt.Errorf("observe exact crash-window pod: %w", err)}
	}
	if len(names) == 0 {
		return true, o.finish(time.Now().UTC())
	}

	probeCtx, cancelProbe := attempt()
	sample, err := o.probe(probeCtx)
	cancelProbe()
	var deadlineErr error
	if err == nil {
		deadlineErr = completedProbeDeadlineError(sampleCtx)
	}
	if err != nil {
		// The canary pod terminates naturally at its gate during the
		// observation window, and kubelet surfaces that teardown to an
		// in-flight exec probe under many signatures: "pod not found",
		// "container not found", or SIGKILL of the exec'd process (exit
		// 137). Rather than enumerate signatures — each new one has failed a
		// run — treat EVERY probe failure as a teardown candidate and let a
		// single authoritative pod re-read decide. A pod that is gone or is
		// terminal/terminating/all-containers-terminated ends the evidence
		// window exactly like clean disappearance; a pod still genuinely
		// alive (exec transport broke, RBAC, a real fault) fails closed. No
		// error signature can pass a live pod, so this cannot mask a
		// second live spawn.
		recheckCtx, cancelRecheck := attempt()
		_, _, namesAfter, recheckErr := o.h.SpawnPodStatus(recheckCtx, o.spawnID)
		cancelRecheck()
		if recheckErr != nil {
			return false, transientObservationError{fmt.Errorf("probe crash-window processes: %v; confirm exact pod after probe failure: %w", err, recheckErr)}
		}
		if len(namesAfter) == 0 {
			return true, o.finish(time.Now().UTC())
		}
		downCtx, cancelDown := attempt()
		tornDown, downErr := o.h.SpawnPodTornDown(downCtx, o.spawnID)
		cancelDown()
		if downErr != nil {
			return false, transientObservationError{fmt.Errorf("probe crash-window processes: %v; confirm teardown after probe failure: %w", err, downErr)}
		}
		if tornDown {
			return true, o.finish(time.Now().UTC())
		}
		return false, transientObservationError{fmt.Errorf("probe crash-window processes while exact pod still exists: %w", err)}
	}
	sample.ObservedAt = sampleStartedAt
	sample.CompletedAt = time.Now().UTC()
	appendErr := o.appendSample(sample)
	if deadlineErr != nil {
		appendErr = errors.Join(appendErr, fmt.Errorf("crash-window process sampling gap: probe completed after deadline: %w", deadlineErr))
	}
	if appendErr != nil {
		return false, appendErr
	}
	return false, nil
}

// completedProbeDeadlineError fails closed even when a transport or test double
// returns a response after ignoring context cancellation. A snapshot received
// at or after its deadline cannot establish bounded process coverage.
func completedProbeDeadlineError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if ok && !time.Now().UTC().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func (o *CanaryProcessObserver) probe(ctx context.Context) (CanaryProcessSample, error) {
	if o.h.processProbeFn != nil {
		return o.h.processProbeFn(
			ctx,
			o.spawnID,
			o.initial.PID,
			o.initial.StartTimeTicks,
			o.initial.DriverPID,
			o.initial.DriverStartTimeTicks,
		)
	}
	return o.h.ProbeCanaryProcesses(
		ctx,
		o.spawnID,
		o.initial.PID,
		o.initial.StartTimeTicks,
		o.initial.DriverPID,
		o.initial.DriverStartTimeTicks,
	)
}

func (o *CanaryProcessObserver) beginSample() (context.Context, time.Time, context.CancelFunc, error) {
	now := time.Now().UTC()
	o.mu.Lock()
	if !o.endedAt.IsZero() {
		o.mu.Unlock()
		return nil, time.Time{}, nil, errors.New("crash-window process observation already ended")
	}
	if len(o.errors) > 0 {
		err := errors.New(strings.Join(o.errors, "; "))
		o.mu.Unlock()
		return nil, time.Time{}, nil, err
	}
	if o.sampleInFlight {
		o.mu.Unlock()
		return nil, time.Time{}, nil, errors.New("crash-window process observation already has an in-flight sample")
	}
	previous := o.startedAt
	if n := len(o.samples); n > 0 {
		previous = o.samples[n-1].ObservedAt
	}
	deadline := previous.Add(o.gap)
	if !now.Before(deadline) {
		suffix := o.lastTransientSuffixLocked()
		o.mu.Unlock()
		return nil, time.Time{}, nil, fmt.Errorf("crash-window process sampling gap %s exceeds %s before next observation%s",
			now.Sub(previous), o.gap, suffix)
	}
	o.sampleInFlight = true
	o.sampleStartedAt = now
	o.sampleDeadlineAt = deadline
	o.mu.Unlock()

	sampleCtx, cancel := context.WithDeadline(o.ctx, deadline)
	finish := func() {
		cancel()
		o.mu.Lock()
		o.sampleInFlight = false
		o.sampleStartedAt = time.Time{}
		o.sampleDeadlineAt = time.Time{}
		o.mu.Unlock()
	}
	return sampleCtx, now, finish, nil
}

func (o *CanaryProcessObserver) recordContextEnd() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.manualStop && o.endedAt.IsZero() && len(o.errors) == 0 {
		o.errors = append(o.errors, "crash-window process observer context ended before exact pod deletion")
	}
}

func (o *CanaryProcessObserver) overdueMessageLocked(now time.Time) string {
	if !o.endedAt.IsZero() || len(o.samples) == 0 {
		return ""
	}
	previous := o.samples[len(o.samples)-1].ObservedAt
	deadline := previous.Add(o.gap)
	if !now.After(deadline) {
		return ""
	}
	detail := "before the next observation"
	if o.sampleInFlight {
		detail = fmt.Sprintf("while a sample was in-flight since %s (deadline %s)",
			o.sampleStartedAt, o.sampleDeadlineAt)
	} else if !o.activated {
		detail = "while the observer was paused"
	}
	return fmt.Sprintf("crash-window process sampling gap %s exceeds %s %s%s",
		now.Sub(previous), o.gap, detail, o.lastTransientSuffixLocked())
}

// lastTransientSuffixLocked surfaces the most recent retried transport
// failure in a fatal gap-breach message, so a window that fails closed after
// retries still names its underlying cause. Callers must hold o.mu.
func (o *CanaryProcessObserver) lastTransientSuffixLocked() string {
	if o.transientCount == 0 || len(o.transient) == 0 {
		return ""
	}
	return fmt.Sprintf("; after %d transient observation failures, last: %s",
		o.transientCount, o.transient[len(o.transient)-1])
}

func (o *CanaryProcessObserver) appendSample(sample CanaryProcessSample) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	previous := o.startedAt
	if n := len(o.samples); n > 0 {
		previous = o.samples[n-1].ObservedAt
	}
	if sample.ObservedAt.Before(previous) {
		o.samples = append(o.samples, cloneCanaryProcessSample(sample))
		return fmt.Errorf("crash-window process sample time moved backwards: %s -> %s", previous, sample.ObservedAt)
	}
	if elapsed := sample.ObservedAt.Sub(previous); elapsed > o.gap {
		o.samples = append(o.samples, cloneCanaryProcessSample(sample))
		return fmt.Errorf("crash-window process sampling gap %s exceeds %s", elapsed, o.gap)
	}
	evidence := Evidence{SpawnPodName: o.initial.PodName, CanaryHoldInitial: o.initial}
	index := len(o.samples)
	o.samples = append(o.samples, cloneCanaryProcessSample(sample))
	if err := validatePostCrashProcessSample(evidence, index, sample); err != nil {
		return err
	}
	if index == 0 && (sample.HoldState == "MISSING" || sample.DriverState == "MISSING") {
		return errors.New("crash-window initial process sample did not retain both live hold and driver identities")
	}
	return nil
}

func (o *CanaryProcessObserver) finish(observedAt time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.samples) == 0 {
		return errors.New("crash-window process observer reached pod deletion without a sample")
	}
	last := o.samples[len(o.samples)-1].ObservedAt
	if observedAt.Before(last) {
		return fmt.Errorf("crash-window process observation end predates last sample: %s -> %s", last, observedAt)
	}
	if elapsed := observedAt.Sub(last); elapsed > o.gap {
		return fmt.Errorf("crash-window final process sampling gap %s exceeds %s", elapsed, o.gap)
	}
	o.endedAt = observedAt
	return nil
}

func (o *CanaryProcessObserver) addError(message string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if message != "" {
		o.errors = append(o.errors, message)
	}
}

func (o *CanaryProcessObserver) addTransient(message string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if message == "" {
		return
	}
	o.transientCount++
	if len(o.transient) < transientObservationFailureLimit {
		o.transient = append(o.transient, message)
	}
}

// Record folds an atomic observer snapshot into evidence. It may be called
// during a replacement rollout without stopping the observer.
func (o *CanaryProcessObserver) Record(ev *Evidence) error {
	if o == nil {
		return errors.New("crash-window process observer is nil")
	}
	o.mu.Lock()
	overdue := false
	if len(o.errors) == 0 {
		if message := o.overdueMessageLocked(time.Now().UTC()); message != "" {
			o.errors = append(o.errors, message)
			overdue = true
		}
	}
	samples := make([]CanaryProcessSample, len(o.samples))
	for i := range o.samples {
		samples[i] = cloneCanaryProcessSample(o.samples[i])
	}
	endedAt := o.endedAt
	errorsCopy := append([]string(nil), o.errors...)
	transientCopy := append([]string(nil), o.transient...)
	transientCount := o.transientCount
	maxGapMS := (o.gap + time.Millisecond - 1).Milliseconds()
	o.mu.Unlock()
	if overdue {
		o.cancel()
	}

	ev.PostCrashProcessSamples = samples
	ev.ProcessObservationStartedAt = o.startedAt
	ev.PostCrashProcessObservedEnd = endedAt
	ev.PostCrashProcessMaxGapMS = maxGapMS
	ev.PostCrashProcessTransientFailures = transientCopy
	ev.PostCrashProcessTransientFailureCount = transientCount
	for _, message := range errorsCopy {
		appendObservationError(ev, message)
	}
	if len(errorsCopy) > 0 {
		return errors.New(strings.Join(errorsCopy, "; "))
	}
	return nil
}

// StopAndRecord is idempotent and preserves a fail-closed error when work ends
// before the exact pod disappearance was observed.
func (o *CanaryProcessObserver) StopAndRecord(ev *Evidence) error {
	if o == nil {
		return errors.New("crash-window process observer is nil")
	}
	o.mu.Lock()
	if o.endedAt.IsZero() && len(o.errors) == 0 {
		o.errors = append(o.errors, "crash-window process observer stopped before exact pod deletion")
	}
	o.manualStop = true
	o.mu.Unlock()
	o.cancel()
	<-o.done
	return o.Record(ev)
}

func cloneCanaryProcessSample(sample CanaryProcessSample) CanaryProcessSample {
	sample.LiveHoldPIDs = append([]int(nil), sample.LiveHoldPIDs...)
	sample.LiveDriverPIDs = append([]int(nil), sample.LiveDriverPIDs...)
	sample.ZombiePIDs = append([]int(nil), sample.ZombiePIDs...)
	return sample
}
