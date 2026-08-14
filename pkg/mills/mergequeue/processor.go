package mergequeue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/mills/pipeline"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// Drive-state keys persisted in a queue entry's detail_json so a restarted
// operator resumes the head candidate instead of re-mutating.
const (
	detailLedgerSeq         = "ledger_seq"
	detailVersionsCursor    = "versions_cursor"
	detailEventsCursor      = "events_cursor"
	detailPipelineWaitSince = "pipeline_wait_since"
	detailPipelineCreated   = "pipeline_create_attempted"
	detailPipelineURL       = "pipeline_url"
)

const (
	// defaultInterval is the processor's tick cadence. Each tick drives every
	// lane head at most one step, so the cadence bounds queue reactivity, not
	// correctness.
	defaultInterval = 15 * time.Second
	// defaultAwaitPipeline bounds how long a rebased head may wait for a
	// terminal branch pipeline before the candidate is evicted ci_timeout.
	// Sized above the repo's observed 17–28 minute pipelines.
	defaultAwaitPipeline = 45 * time.Minute
	// pipelineAppearGrace is how long the queue waits for the rebase push to
	// mint a branch pipeline before creating one via the API (once).
	pipelineAppearGrace = 5 * time.Minute
	// maxHeadDriveAttempts bounds queued→rebasing loops for one head. A rebase
	// that keeps settling noop while the MR stays behind is a wedge, not
	// progress.
	maxHeadDriveAttempts = 3
	// eventActor stamps the queue's audit events.
	eventActor = "mergequeue"
)

// Processor drives the serial merge queue. Construct with fields, then Run in
// the operator errgroup. All fields are read-only after Run starts.
type Processor struct {
	// Store is the canonical mills store (queue DAO + ledger + events).
	Store *store.Store
	// ForProject resolves the Forge for an entry's project. Nil disables the
	// processor (Run blocks until ctx cancel).
	ForProject func(project string) Forge
	// Enabled is the hot-reloaded policy fence, consulted every tick:
	// mills kill switch AND merge_queue.enabled. When it reports false the
	// processor halts WITHOUT losing queue state — entries stay put and the
	// merge stage's waiters observe the disable and fall back.
	Enabled func() bool
	Logger  *slog.Logger
	// Interval overrides the tick cadence (tests). Zero → defaultInterval.
	Interval time.Duration
	// AwaitPipeline overrides the pipeline wait bound. Zero → default.
	AwaitPipeline time.Duration
	// Now is injectable for tests. Nil → time.Now.
	Now func() time.Time

	// busy tracks lanes with an in-flight drive goroutine so a slow step
	// (ObserveHead settle, the merge PUT's bounded retries) never stacks a
	// second driver on the same lane. Lanes drive independently.
	busy sync.Map
}

// Run ticks until ctx is cancelled. Returns nil on clean shutdown so it
// composes with errgroup.WithContext alongside the other mills schedulers.
func (p *Processor) Run(ctx context.Context) error {
	if p == nil || p.Store == nil || p.Store.MergeQueue == nil || p.ForProject == nil {
		<-ctx.Done()
		return nil
	}
	interval := p.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	if p.Logger != nil {
		enabled := p.Enabled != nil && p.Enabled()
		p.Logger.Info("merge queue processor armed", "enabled", enabled, "interval", interval)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.tick(ctx)
		}
	}
}

// tick drives every lane head one step. The policy fence halts processing
// without losing state.
func (p *Processor) tick(ctx context.Context) {
	if p.Enabled == nil || !p.Enabled() {
		return
	}
	heads, err := p.Store.MergeQueue.Heads(ctx)
	if err != nil {
		p.logger().Warn("merge queue: heads read failed", "error", err)
		return
	}
	p.observeDepth(ctx)
	for _, head := range heads {
		lane := head.Project + "→" + head.TargetBranch
		if _, inFlight := p.busy.LoadOrStore(lane, struct{}{}); inFlight {
			continue
		}
		go func(e *store.MergeQueueEntry, lane string) {
			defer p.busy.Delete(lane)
			if err := p.driveHead(ctx, e); err != nil && ctx.Err() == nil {
				p.logger().Warn("merge queue: drive failed; will retry next tick",
					"run", e.PipelineRunID, "mr", e.MRIID, "state", string(e.State), "error", err)
			}
		}(head, lane)
	}
}

// driveHead advances one lane's head candidate a single step. Transient errors
// return non-nil and are retried on a later tick; deterministic dead-ends
// evict with a distinct reason.
func (p *Processor) driveHead(ctx context.Context, e *store.MergeQueueEntry) error {
	forge := p.ForProject(e.Project)
	if forge == nil {
		return fmt.Errorf("no forge for project %q", e.Project)
	}
	switch e.State {
	case store.MergeQueueQueued:
		return p.driveQueued(ctx, forge, e)
	case store.MergeQueueRebasing:
		return p.driveRebasing(ctx, forge, e)
	case store.MergeQueueAwaitingPipeline:
		return p.driveAwaitingPipeline(ctx, forge, e)
	case store.MergeQueueMerging:
		return p.driveMerging(ctx, forge, e)
	default:
		return nil
	}
}

// driveQueued decides the head's path: already merged/closed externally,
// up to date (→ merging), or behind (→ rebase).
func (p *Processor) driveQueued(ctx context.Context, forge Forge, e *store.MergeQueueEntry) error {
	snap, err := forge.MRSnapshot(ctx, e.MRIID)
	if err != nil {
		return fmt.Errorf("mr snapshot: %w", err)
	}
	switch snap.State {
	case "merged":
		// Someone else landed it; the waiting run reports success.
		return p.settleMerged(ctx, e, store.MergeQueueQueued, snap.MergedSHA)
	case "closed":
		return p.evict(ctx, e, store.MergeQueueEvictMRClosed, "merge request was closed while queued", nil)
	}
	if snap.SHA != e.CurrentSHA {
		// The head moved underneath the queue (external push). Fail closed —
		// the authorization chain is broken and the run must re-gate.
		return p.evict(ctx, e, store.MergeQueueEvictHeadMoved,
			fmt.Sprintf("head moved externally while queued: authorized %s, observed %s", shortSHA(e.CurrentSHA), shortSHA(snap.SHA)), nil)
	}

	tip, err := forge.BranchTip(ctx, e.TargetBranch)
	if err != nil {
		return fmt.Errorf("target tip: %w", err)
	}
	if snap.BaseSHA == tip {
		// Based on the exact target tip. For PIPELINE candidates the run's
		// ci_watch verdict for this head IS a verdict on the tree that will
		// land — merge directly. EXTERNAL candidates (shepherd, mcp-gitlab)
		// carry no such proof: they are routinely enqueued while their head
		// pipeline is still running, and merging now would burn the merge
		// call's short not-ready settle window against a full CI run and
		// evict a healthy MR. Route them through awaiting_pipeline, which
		// merges the moment a terminal successful pipeline exists for the
		// head and evicts ci_red/ci_timeout otherwise.
		to := store.MergeQueueMerging
		var detail map[string]any
		if isExternalCandidate(e) {
			to = store.MergeQueueAwaitingPipeline
			detail = cloneDetail(e.Detail)
			detail[detailPipelineWaitSince] = p.now().UTC().Format(time.RFC3339)
		}
		_, err := p.Store.MergeQueue.Transition(ctx, store.MergeQueueTransition{
			ID: e.ID, From: store.MergeQueueQueued, To: to, Detail: detail,
		})
		return ignoreConflict(err)
	}

	if e.Attempts >= maxHeadDriveAttempts {
		return p.evict(ctx, e, store.MergeQueueEvictRebaseAmbiguous,
			fmt.Sprintf("head still behind %s after %d rebase attempts", e.TargetBranch, e.Attempts), nil)
	}

	// Behind the tip: request a rebase. Cursors are snapshotted BEFORE the PUT
	// and the movement is a durable #374 ledger row, so a process death
	// between the PUT and the observation re-observes instead of re-mutating.
	cursors, err := forge.ReadHeadCursors(ctx, pipeline.HeadCursorRequest{
		Project: e.Project, MRIID: e.MRIID,
		SourceBranch: e.SourceBranch, TargetBranch: e.TargetBranch,
	})
	if err != nil {
		return fmt.Errorf("read cursors: %w", err)
	}

	ledgerSeq, err := p.openLedgerRow(ctx, e, tip)
	if err != nil {
		return fmt.Errorf("open ledger row: %w", err)
	}

	if err := forge.RequestRebase(ctx, e.MRIID); err != nil {
		// A rebase already in flight still needs observing; other errors
		// settle the ledger row failed and evict.
		if !errors.Is(err, ErrRebaseInProgress) {
			p.settleLedgerRow(ctx, e, ledgerSeq, store.MRHeadTransitionFailed, "", map[string]any{"error": err.Error()})
			return p.evict(ctx, e, store.MergeQueueEvictRebaseConflict,
				"rebase request refused: "+err.Error(), nil)
		}
	}

	detail := cloneDetail(e.Detail)
	detail[detailLedgerSeq] = ledgerSeq
	detail[detailVersionsCursor] = cursors.VersionsCursor
	detail[detailEventsCursor] = cursors.EventsCursor
	_, err = p.Store.MergeQueue.Transition(ctx, store.MergeQueueTransition{
		ID: e.ID, From: store.MergeQueueQueued, To: store.MergeQueueRebasing,
		Detail: detail, BumpAttempts: true,
	})
	return ignoreConflict(err)
}

// driveRebasing settles the requested rebase via the #374 observation
// machinery and routes on the verdict.
func (p *Processor) driveRebasing(ctx context.Context, forge Forge, e *store.MergeQueueEntry) error {
	obs, err := forge.ObserveHead(ctx, pipeline.HeadObservationRequest{
		Project: e.Project, MRIID: e.MRIID,
		SourceBranch: e.SourceBranch, TargetBranch: e.TargetBranch,
		ReviewedSHA:    e.CurrentSHA,
		VersionsCursor: detailInt64(e.Detail, detailVersionsCursor),
		EventsCursor:   detailInt64(e.Detail, detailEventsCursor),
	})
	if err != nil {
		return fmt.Errorf("observe head: %w", err)
	}
	p.settleLedgerRow(ctx, e, detailInt64(e.Detail, detailLedgerSeq), obs.Verdict.State(), obs.SuccessorSHA, map[string]any{
		"verdict": string(obs.Verdict), "reason": obs.Reason, "attempts": obs.Attempts,
	})

	switch obs.Verdict {
	case pipeline.HeadVerdictAttributed:
		detail := cloneDetail(e.Detail)
		detail[detailPipelineWaitSince] = p.now().UTC().Format(time.RFC3339)
		delete(detail, detailPipelineCreated)
		_, err := p.Store.MergeQueue.Transition(ctx, store.MergeQueueTransition{
			ID: e.ID, From: store.MergeQueueRebasing, To: store.MergeQueueAwaitingPipeline,
			CurrentSHA: obs.SuccessorSHA, Detail: detail,
		})
		return ignoreConflict(err)
	case pipeline.HeadVerdictNoop:
		// The head did not move. Re-evaluate from queued; the attempt bound
		// converts a persistent noop-while-behind wedge into an eviction.
		_, err := p.Store.MergeQueue.Transition(ctx, store.MergeQueueTransition{
			ID: e.ID, From: store.MergeQueueRebasing, To: store.MergeQueueQueued,
		})
		return ignoreConflict(err)
	case pipeline.HeadVerdictFailed:
		return p.evict(ctx, e, store.MergeQueueEvictRebaseConflict,
			"rebase failed: "+obs.Reason, map[string]any{"merge_error": obs.MergeError})
	default: // ambiguous
		return p.evict(ctx, e, store.MergeQueueEvictRebaseAmbiguous,
			"head movement ambiguous: "+obs.Reason, nil)
	}
}

// driveAwaitingPipeline waits for a terminal branch pipeline on the rebased
// head, creating one bounded recovery pipeline if none appears.
func (p *Processor) driveAwaitingPipeline(ctx context.Context, forge Forge, e *store.MergeQueueEntry) error {
	waitSince := detailTime(e.Detail, detailPipelineWaitSince, p.now())
	awaitMax := p.AwaitPipeline
	if awaitMax <= 0 {
		awaitMax = defaultAwaitPipeline
	}

	ps, err := forge.BranchPipelineStatus(ctx, e.CurrentSHA, e.SourceBranch)
	if err != nil {
		return fmt.Errorf("pipeline status: %w", err)
	}
	if ps.Found {
		switch ps.Status {
		case "success":
			detail := cloneDetail(e.Detail)
			detail[detailPipelineURL] = ps.WebURL
			_, err := p.Store.MergeQueue.Transition(ctx, store.MergeQueueTransition{
				ID: e.ID, From: store.MergeQueueAwaitingPipeline, To: store.MergeQueueMerging,
				Detail: detail,
			})
			return ignoreConflict(err)
		case "failed", "canceled":
			return p.evict(ctx, e, store.MergeQueueEvictCIRed,
				fmt.Sprintf("pipeline %d on rebased head %s: %s", ps.ID, shortSHA(e.CurrentSHA), ps.Status),
				map[string]any{"pipeline_url": ps.WebURL, "pipeline_status": ps.Status})
		case "skipped":
			// A skipped pipeline never turns terminal-green; fall through to
			// the create/timeout path below as if none existed.
		default:
			// running / pending / created — keep waiting inside the bound.
			if p.now().Sub(waitSince) > awaitMax {
				return p.evict(ctx, e, store.MergeQueueEvictCITimeout,
					fmt.Sprintf("pipeline %d still %s after %s", ps.ID, ps.Status, awaitMax),
					map[string]any{"pipeline_url": ps.WebURL})
			}
			return nil
		}
	}

	if p.now().Sub(waitSince) > awaitMax {
		return p.evict(ctx, e, store.MergeQueueEvictCITimeout,
			fmt.Sprintf("no terminal pipeline for rebased head %s within %s", shortSHA(e.CurrentSHA), awaitMax), nil)
	}
	if !detailBool(e.Detail, detailPipelineCreated) && p.now().Sub(waitSince) > pipelineAppearGrace {
		created, err := forge.CreateQueuePipeline(ctx, e.SourceBranch)
		if err != nil {
			return fmt.Errorf("create recovery pipeline: %w", err)
		}
		if created.SHA != "" && created.SHA != e.CurrentSHA {
			return p.evict(ctx, e, store.MergeQueueEvictHeadMoved,
				fmt.Sprintf("recovery pipeline built %s, queue head is %s", shortSHA(created.SHA), shortSHA(e.CurrentSHA)), nil)
		}
		detail := cloneDetail(e.Detail)
		detail[detailPipelineCreated] = true
		_, terr := p.Store.MergeQueue.Transition(ctx, store.MergeQueueTransition{
			ID: e.ID, From: store.MergeQueueAwaitingPipeline, To: store.MergeQueueAwaitingPipeline,
			Detail: detail,
		})
		return ignoreConflict(terr)
	}
	return nil
}

// driveMerging performs the SHA-preconditioned merge through the client's
// bounded recovery machinery and settles the entry.
func (p *Processor) driveMerging(ctx context.Context, forge Forge, e *store.MergeQueueEntry) error {
	resp, err := forge.Merge(ctx, pipeline.MergeRequestArgs{
		MRIID: e.MRIID, Project: e.Project,
		SourceBranch: e.SourceBranch, TargetBranch: e.TargetBranch,
		ExpectedSHA: e.CurrentSHA,
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err() // shutdown mid-merge: resume next process
		}
		var headMoved *pipeline.MergeSourceSHAMismatchError
		if errors.As(err, &headMoved) {
			return p.evict(ctx, e, store.MergeQueueEvictHeadMoved,
				fmt.Sprintf("head moved before merge: authorized %s, observed %s", shortSHA(headMoved.ReviewedSHA), shortSHA(headMoved.ObservedSHA)), nil)
		}
		return p.evict(ctx, e, store.MergeQueueEvictMergeFailed, err.Error(), nil)
	}
	return p.settleMerged(ctx, e, store.MergeQueueMerging, resp.MergedSHA)
}

// ----- settle helpers -----

func (p *Processor) settleMerged(ctx context.Context, e *store.MergeQueueEntry, from store.MergeQueueState, mergedSHA string) error {
	got, err := p.Store.MergeQueue.MarkMerged(ctx, e.ID, from, mergedSHA)
	if err != nil && !errors.Is(err, store.ErrMergeQueueConflict) {
		return err
	}
	MergedTotal.Inc()
	if got != nil {
		QueueWaitSeconds.Observe(p.now().Sub(got.EnqueuedAt).Seconds())
	}
	p.appendEvent(ctx, e, "mergequeue.merged", map[string]any{"merged_sha": mergedSHA})
	p.logger().Info("merge queue: merged", "run", e.PipelineRunID, "mr", e.MRIID, "project", e.Project, "sha", shortSHA(mergedSHA))
	return nil
}

// evict terminalizes the head with a distinct reason. The waiting merge stage
// surfaces the reason as a stage error, which routes the run into the existing
// escalation path — the queue itself never retries.
func (p *Processor) evict(ctx context.Context, e *store.MergeQueueEntry, reason, detail string, extra map[string]any) error {
	d := map[string]any{"detail": detail}
	for k, v := range extra {
		d[k] = v
	}
	if _, err := p.Store.MergeQueue.MarkEvicted(ctx, e.ID, reason, d); err != nil && !errors.Is(err, store.ErrMergeQueueConflict) {
		return err
	}
	EvictionsTotal.WithLabelValues(reason).Inc()
	payload := map[string]any{"reason": reason, "detail": detail}
	p.appendEvent(ctx, e, "mergequeue.evicted", payload)
	p.logger().Warn("merge queue: evicted", "run", e.PipelineRunID, "mr", e.MRIID, "reason", reason, "detail", detail)
	return nil
}

// ----- ledger helpers (#374) -----

// openLedgerRow mints (or adopts) the run's open mr_head_transitions row for
// the queue's rebase request. ErrHeadTransitionOpen means a previous process
// died between the PUT and the observation — adopt that row's seq and
// re-observe rather than re-mutating.
func (p *Processor) openLedgerRow(ctx context.Context, e *store.MergeQueueEntry, targetTip string) (int64, error) {
	if p.Store.MRHeadTransitions == nil {
		return 0, nil
	}
	row, err := p.Store.MRHeadTransitions.Open(ctx, &store.MRHeadTransition{
		PipelineRunID: e.PipelineRunID,
		Project:       e.Project,
		MRIID:         e.MRIID,
		SourceBranch:  e.SourceBranch,
		TargetBranch:  e.TargetBranch,
		ReviewedSHA:   e.CurrentSHA,
		TargetHeadSHA: targetTip,
		Trigger:       store.MRHeadTriggerRebaseRequest,
		State:         store.MRHeadTransitionRequested,
		Provenance:    map[string]any{"actor": eventActor},
	})
	if errors.Is(err, store.ErrHeadTransitionOpen) {
		open, oerr := p.Store.MRHeadTransitions.OpenTransition(ctx, e.PipelineRunID)
		if oerr != nil || open == nil {
			return 0, fmt.Errorf("adopt open transition: %v", oerr)
		}
		return open.Seq, nil
	}
	if err != nil {
		return 0, err
	}
	return row.Seq, nil
}

func (p *Processor) settleLedgerRow(ctx context.Context, e *store.MergeQueueEntry, seq int64, state store.MRHeadTransitionState, successor string, provenance map[string]any) {
	if p.Store.MRHeadTransitions == nil || seq <= 0 {
		return
	}
	_, err := p.Store.MRHeadTransitions.Settle(ctx, store.SettleRequest{
		PipelineRunID: e.PipelineRunID,
		Seq:           seq,
		State:         state,
		SuccessorSHA:  successor,
		Provenance:    provenance,
	})
	if err != nil && !errors.Is(err, store.ErrHeadTransitionSettled) {
		p.logger().Warn("merge queue: ledger settle failed", "run", e.PipelineRunID, "seq", seq, "error", err)
	}
}

// ----- misc helpers -----

func (p *Processor) appendEvent(ctx context.Context, e *store.MergeQueueEntry, kind string, payload map[string]any) {
	if p.Store.Events == nil {
		return
	}
	payload["project"] = e.Project
	payload["mr_iid"] = e.MRIID
	payload["backlog_id"] = e.BacklogID
	err := p.Store.Events.Append(ctx, &store.Event{
		Actor: eventActor, Kind: kind,
		SubjectKind: "pipeline_run", SubjectID: e.PipelineRunID,
		Payload: payload,
	})
	if err != nil {
		p.logger().Warn("merge queue: event append failed", "kind", kind, "error", err)
	}
}

func (p *Processor) observeDepth(ctx context.Context) {
	active, err := p.Store.MergeQueue.ListActive(ctx)
	if err != nil {
		return
	}
	DepthGauge.Set(float64(len(active)))
}

func (p *Processor) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

func (p *Processor) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// isExternalCandidate reports whether the entry was submitted by a fleet
// producer (shepherd, mcp-gitlab) rather than a Mills pipeline run. External
// enqueues stamp their producer into the durable detail bundle
// (mergequeue.ExternalEnqueuer); pipeline candidates never do.
func isExternalCandidate(e *store.MergeQueueEntry) bool {
	producer, _ := e.Detail["producer"].(string)
	return producer != ""
}

// ignoreConflict swallows CAS conflicts: a racing tick already advanced the
// entry, which is progress, not failure.
func ignoreConflict(err error) error {
	if errors.Is(err, store.ErrMergeQueueConflict) {
		return nil
	}
	return err
}

func cloneDetail(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+3)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func detailInt64(d map[string]any, key string) int64 {
	switch v := d[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func detailBool(d map[string]any, key string) bool {
	b, _ := d[key].(bool)
	return b
}

func detailTime(d map[string]any, key string, fallback time.Time) time.Time {
	s, _ := d[key].(string)
	if s == "" {
		return fallback
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fallback
	}
	return t
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
