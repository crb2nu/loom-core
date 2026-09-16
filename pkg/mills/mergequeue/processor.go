package mergequeue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
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
	detailPipelineID        = "pipeline_id"
	detailPipelineLane      = "pipeline_lane"
	detailProof             = "proof"
	detailProofSource       = "proof_source"
	detailSpecRef           = "speculative_ref"
	detailSpecSHA           = "speculative_sha"
	detailSpecPipelineID    = "speculative_pipeline_id"
	detailSpecPredecessorID = "speculative_predecessor_id"
	// detailSpecFailedOnto records the base a speculative head could not be
	// built on (cherry-pick conflict, API refusal) so the tick loop does not
	// retry the identical attempt every 15s; a new base clears it.
	detailSpecFailedOnto = "speculative_failed_onto"
	// detailObservedSHA carries the full successor head on head_moved
	// evictions so the stage can reconstruct a typed head-movement error
	// (A1) instead of scraping short SHAs out of the detail text.
	detailObservedSHA = "observed_sha"
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
	// AwaitPipelineFn, when set, is consulted on every await so a
	// hot-reloaded policy (merge_queue.await_pipeline_minutes) takes effect
	// without a restart. A zero result falls back to AwaitPipeline, then to
	// the default.
	AwaitPipelineFn func() time.Duration
	// SpeculationDepth is hot-reloaded; zero disables successor preparation.
	SpeculationDepth func() int
	// Now is injectable for tests. Nil → time.Now.
	Now func() time.Time

	// busy tracks lanes with an in-flight drive goroutine so a slow step
	// (ObserveHead settle, the merge PUT's bounded retries) never stacks a
	// second driver on the same lane. Lanes drive independently.
	busy              sync.Map
	recoveryMu        sync.Mutex
	recoveryPipelines map[string]recoveryPipeline
	// External re-admits green evictions (shepherd A2): head_moved and
	// ci_timeout evictions re-enqueue ONCE as external candidates under the
	// observed head when RequeueEvictions reports the policy flag on. nil or
	// a false flag disables the hop; a candidate the evictor itself produced
	// (producer == evictionRequeueProducer) is never re-requeued.
	External         *ExternalEnqueuer
	RequeueEvictions func() bool
	// MainRedExternal gates each lane before its head is advanced.
	MainRedExternal *MainRedExternalController
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
	p.prepareSpeculation(ctx)
	// Revisit recently-settled external candidates before admitting more work.
	// A process may die after MarkMerged commits but before the owning backlog
	// transition does; replaying terminal rows makes that gap self-healing.
	p.settleRecentExternalAdoptions(ctx)
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

func (p *Processor) prepareSpeculation(ctx context.Context) {
	if p.SpeculationDepth == nil || p.SpeculationDepth() <= 0 {
		return
	}
	active, err := p.Store.MergeQueue.ListActive(ctx)
	if err != nil {
		return
	}
	depth := p.SpeculationDepth()
	for i, head := range active {
		if head.State != store.MergeQueueAwaitingPipeline || head.CurrentSHA == "" {
			continue
		}
		if held, err := p.deferMainRedExternal(ctx, head); err != nil || held {
			continue
		}
		onto := head.CurrentSHA
		prepared := 0
		// One MR may hold several active entries (its own run plus an external
		// candidate a producer such as the mrwatch shepherd submitted for the
		// same head). Stacking an MR on top of itself cherry-picks its own
		// commits onto its own head and fails every tick (2026-09-12: 720
		// failed attempts in 3h for !1918), so each MR appears once per chain.
		chain := map[int64]bool{head.MRIID: true}
		for j := i + 1; j < len(active) && prepared < depth; j++ {
			next := active[j]
			if next.Project != head.Project || next.TargetBranch != head.TargetBranch || chain[next.MRIID] {
				continue
			}
			chain[next.MRIID] = true
			prepared++
			if sha, _ := next.Detail[detailSpecSHA].(string); sha != "" {
				onto = sha
				continue
			}
			if failed, _ := next.Detail[detailSpecFailedOnto].(string); failed == onto {
				break
			}
			forge, ok := p.ForProject(next.Project).(SpeculativeForge)
			if !ok {
				break
			}
			ref := fmt.Sprintf("mills-mq/spec-%d-%s", next.MRIID, shortSHA(onto))
			spec, err := forge.PrepareSpeculativeHead(ctx, next.MRIID, onto, ref)
			if err != nil {
				p.logger().Warn("merge queue: speculative head failed", "mr", next.MRIID, "onto", shortSHA(onto), "error", err)
				SpeculationTotal.WithLabelValues("miss").Inc()
				failedDetail := cloneDetail(next.Detail)
				failedDetail[detailSpecFailedOnto] = onto
				_, _ = p.Store.MergeQueue.Transition(ctx, store.MergeQueueTransition{ID: next.ID, From: next.State, To: next.State, Detail: failedDetail})
				break
			}
			detail := cloneDetail(next.Detail)
			detail[detailSpecRef], detail[detailSpecSHA] = spec.Ref, spec.SHA
			detail[detailSpecPipelineID], detail[detailSpecPredecessorID] = spec.Pipeline.ID, head.ID
			if spec.Adopted {
				detail[detailPipelineLane] = "adopted"
			} else {
				detail[detailPipelineLane] = "queue"
			}
			if _, err := p.Store.MergeQueue.Transition(ctx, store.MergeQueueTransition{ID: next.ID, From: next.State, To: next.State, Detail: detail}); err != nil {
				_ = forge.CancelQueuePipeline(ctx, spec.Pipeline.ID)
				_ = forge.DeleteQueueRef(ctx, spec.Ref)
				break
			}
			onto = spec.SHA
		}
	}
}

func (p *Processor) cleanupSpeculation(ctx context.Context, predecessor *store.MergeQueueEntry, cancel bool) {
	active, err := p.Store.MergeQueue.ListActive(ctx)
	if err != nil {
		return
	}
	for _, e := range active {
		if detailInt64(e.Detail, detailSpecPredecessorID) != predecessor.ID {
			continue
		}
		forge, ok := p.ForProject(e.Project).(SpeculativeForge)
		if !ok {
			continue
		}
		if cancel {
			if err := forge.CancelQueuePipeline(ctx, detailInt64(e.Detail, detailSpecPipelineID)); err != nil {
				continue
			}
		}
		if ref, _ := e.Detail[detailSpecRef].(string); ref != "" {
			if err := forge.DeleteQueueRef(ctx, ref); err != nil {
				continue
			}
		}
		detail := cloneDetail(e.Detail)
		delete(detail, detailSpecRef)
		delete(detail, detailSpecPredecessorID)
		if cancel {
			delete(detail, detailSpecSHA)
			delete(detail, detailSpecPipelineID)
			SpeculationTotal.WithLabelValues("cancelled").Inc()
		}
		_, _ = p.Store.MergeQueue.Transition(ctx, store.MergeQueueTransition{ID: e.ID, From: e.State, To: e.State, Detail: detail})
	}
}

// driveHead advances one lane's head candidate a single step. Transient errors
// return non-nil and are retried on a later tick; deterministic dead-ends
// evict with a distinct reason.
func (p *Processor) driveHead(ctx context.Context, e *store.MergeQueueEntry) error {
	if held, err := p.deferMainRedExternal(ctx, e); err != nil || held {
		return err
	}
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
		// the authorization chain is broken and the run must re-gate. The
		// observed successor rides structurally so the stage can hand the
		// runner a typed head-movement it can rewind on (A1).
		return p.evict(ctx, e, store.MergeQueueEvictHeadMoved,
			fmt.Sprintf("head moved externally while queued: authorized %s, observed %s", shortSHA(e.CurrentSHA), shortSHA(snap.SHA)),
			map[string]any{detailObservedSHA: snap.SHA})
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

// awaitPipelineMax resolves the pipeline wait bound: the hot-reloaded policy
// value first, then the static override, then the compiled default.
func (p *Processor) awaitPipelineMax() time.Duration {
	if p.AwaitPipelineFn != nil {
		if d := p.AwaitPipelineFn(); d > 0 {
			return d
		}
	}
	if p.AwaitPipeline > 0 {
		return p.AwaitPipeline
	}
	return defaultAwaitPipeline
}

// driveAwaitingPipeline waits for a terminal branch pipeline on the rebased
// head, creating one bounded recovery pipeline if none appears. External
// candidates are additionally proven by a successful merge-request pipeline
// for the head: repos like flexinfer gate MRs on detached MR pipelines, and
// their branch pipelines either never exist or park on blocking manual jobs.
func (p *Processor) driveAwaitingPipeline(ctx context.Context, forge Forge, e *store.MergeQueueEntry) error {
	waitSince := detailTime(e.Detail, detailPipelineWaitSince, p.now())
	awaitMax := p.awaitPipelineMax()

	ps, err := forge.BranchPipelineStatus(ctx, e.CurrentSHA, e.SourceBranch)
	if err != nil {
		return fmt.Errorf("pipeline status: %w", err)
	}
	// The MR pipeline is POSITIVE proof only: success merges, and a live one
	// holds the wait open, but red never evicts — GitLab pins a spurious 0-job
	// FAILED merge_request_event placeholder on MR heads even in repos whose
	// workflow rules suppress MR pipelines (see GitLabClient.Merge), so a red
	// MR pipeline cannot be distinguished from that phantom. A genuinely red
	// external MR falls through to the ci_timeout bound instead.
	var mrPS PipelineStatus
	if isExternalCandidate(e) {
		mrPS, err = forge.MRPipelineStatus(ctx, e.MRIID, e.CurrentSHA)
		if err != nil {
			return fmt.Errorf("mr pipeline status: %w", err)
		}
	}
	if ps.Found && ps.Status == "success" {
		proof, err := forge.PipelineProof(ctx, e.CurrentSHA)
		if err != nil || !proof.Found || proof.SHA != e.CurrentSHA {
			if err != nil {
				return fmt.Errorf("exact pipeline proof: %w", err)
			}
			return fmt.Errorf("exact pipeline %d has no commit-tree proof", ps.ID)
		}
		p.observePipelineTiming(ctx, forge, e, proof.ID, pipelineLane(e, proof))
		return p.promoteToMergingWithProof(ctx, e, proof)
	}
	if mrPS.Found && mrPS.Status == "success" {
		proof, err := forge.PipelineProof(ctx, e.CurrentSHA)
		if err != nil || !proof.Found || proof.SHA != e.CurrentSHA || proof.Status != "success" {
			if err != nil {
				return fmt.Errorf("exact mr pipeline proof: %w", err)
			}
			return fmt.Errorf("mr pipeline %d has no successful commit-tree proof", mrPS.ID)
		}
		p.observePipelineTiming(ctx, forge, e, proof.ID, "adopted")
		return p.promoteToMergingWithProof(ctx, e, proof)
	}
	proof, err := forge.PipelineProof(ctx, e.CurrentSHA)
	if err != nil {
		return fmt.Errorf("pipeline proof: %w", err)
	}
	if proof.Found {
		switch proof.Status {
		case "success":
			p.observePipelineTiming(ctx, forge, e, proof.ID, pipelineLane(e, proof))
			return p.promoteToMergingWithProof(ctx, e, proof)
		case "failed", "canceled":
			p.observePipelineTiming(ctx, forge, e, proof.ID, pipelineLane(e, proof))
			return p.evict(ctx, e, store.MergeQueueEvictCIRed,
				fmt.Sprintf("pipeline %d proving tree %s: %s", proof.ID, shortSHA(proof.Tree), proof.Status),
				map[string]any{"pipeline_url": proof.WebURL, "pipeline_status": proof.Status, detailProofSource: proof.Source})
		default:
			if pipelineProgressing(proof.Status) {
				return nil
			}
		}
	}
	if ps.Found {
		switch ps.Status {
		case "failed", "canceled":
			p.observePipelineTiming(ctx, forge, e, ps.ID, pipelineLane(e, PipelineProof{PipelineStatus: ps}))
			return p.evict(ctx, e, store.MergeQueueEvictCIRed,
				fmt.Sprintf("pipeline %d on rebased head %s: %s", ps.ID, shortSHA(e.CurrentSHA), ps.Status),
				map[string]any{"pipeline_url": ps.WebURL, "pipeline_status": ps.Status})
		case "skipped", "manual":
			// Neither ever turns terminal-green on its own (manual blocks on a
			// human playing a job); fall through to the create/timeout path
			// below as if none existed.
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
	if mrPS.Found && pipelineProgressing(mrPS.Status) {
		// A live MR pipeline is the candidate's proof in flight: hold the wait
		// open and DON'T mint a recovery branch pipeline underneath it (in
		// MR-pipeline repos that recovery would just park manual or skipped).
		if p.now().Sub(waitSince) > awaitMax {
			return p.evict(ctx, e, store.MergeQueueEvictCITimeout,
				fmt.Sprintf("merge request pipeline %d still %s after %s", mrPS.ID, mrPS.Status, awaitMax),
				map[string]any{"pipeline_url": mrPS.WebURL})
		}
		return nil
	}

	if p.now().Sub(waitSince) > awaitMax {
		return p.evict(ctx, e, store.MergeQueueEvictCITimeout,
			fmt.Sprintf("no terminal pipeline for rebased head %s within %s", shortSHA(e.CurrentSHA), awaitMax), nil)
	}
	if !detailBool(e.Detail, detailPipelineCreated) && p.now().Sub(waitSince) > pipelineAppearGrace {
		created, adopted, err := p.adoptOrCreateRecoveryPipeline(ctx, forge, e)
		if err != nil {
			return err
		}
		if created.SHA != "" && created.SHA != e.CurrentSHA {
			return p.evict(ctx, e, store.MergeQueueEvictHeadMoved,
				fmt.Sprintf("recovery pipeline built %s, queue head is %s", shortSHA(created.SHA), shortSHA(e.CurrentSHA)),
				map[string]any{detailObservedSHA: created.SHA})
		}
		detail := cloneDetail(e.Detail)
		detail[detailPipelineCreated] = true
		detail[detailPipelineID] = created.ID
		detail[detailPipelineURL] = created.WebURL
		if adopted {
			detail[detailPipelineLane] = "adopted"
			p.logger().Info("merge queue: adopted recovery pipeline", "adopted_pipeline_id", created.ID, "ref", e.SourceBranch, "sha", e.CurrentSHA, "minted_by", "merge_queue.recovery")
			mills.PipelineAdoptionsTotal.WithLabelValues("merge_queue.recovery").Inc()
		} else {
			detail[detailPipelineLane] = "queue"
			p.logger().Info("merge queue: minted recovery pipeline", "pipeline_id", created.ID, "ref", e.SourceBranch, "sha", e.CurrentSHA, "minted_by", "merge_queue.recovery")
			mills.PipelineMintsTotal.WithLabelValues("merge_queue.recovery").Inc()
		}
		_, terr := p.Store.MergeQueue.Transition(ctx, store.MergeQueueTransition{
			ID: e.ID, From: store.MergeQueueAwaitingPipeline, To: store.MergeQueueAwaitingPipeline,
			Detail: detail,
		})
		return ignoreConflict(terr)
	}
	return nil
}

func (p *Processor) adoptOrCreateRecoveryPipeline(ctx context.Context, forge Forge, e *store.MergeQueueEntry) (PipelineStatus, bool, error) {
	key := e.Project + "\x00" + e.SourceBranch + "\x00" + e.CurrentSHA
	p.recoveryMu.Lock()
	defer p.recoveryMu.Unlock()
	if p.recoveryPipelines == nil {
		p.recoveryPipelines = make(map[string]recoveryPipeline)
	}
	if cached, ok := p.recoveryPipelines[key]; ok {
		// The entry whose recovery minted the pipeline keeps its "queue"
		// lane on every later tick (the cache outlives the entry pointer, so
		// identity is the persisted entry id + MR, never the pointer); any
		// other entry sharing the exact head adopts the same pipeline.
		adopted := !cached.minted || cached.minter != recoveryMinterOf(e)
		return cached.status, adopted, nil
	}
	ps, err := forge.FindActivePipeline(ctx, e.SourceBranch, e.CurrentSHA)
	if err != nil {
		return PipelineStatus{}, false, fmt.Errorf("find active recovery pipeline: %w", err)
	}
	if ps.Found {
		p.recoveryPipelines[key] = recoveryPipeline{status: ps}
		return ps, true, nil
	}
	ps, err = forge.CreateQueuePipeline(ctx, e.SourceBranch, e.MRIID)
	if err != nil {
		return PipelineStatus{}, false, fmt.Errorf("create recovery pipeline: %w", err)
	}
	p.recoveryPipelines[key] = recoveryPipeline{status: ps, minted: true, minter: recoveryMinterOf(e)}
	return ps, false, nil
}

// recoveryPipeline is one exact-head recovery pipeline shared by every queue
// entry on that head. minted records that the merge queue created it (as
// opposed to adopting an active pipeline the forge already had) and minter
// which entry did so, so that entry alone reports the "queue" lane.
type recoveryPipeline struct {
	status PipelineStatus
	minted bool
	minter recoveryMinter
}

// recoveryMinter identifies a queue entry by persisted values, so the same
// entry re-read from the store on a later tick still matches.
type recoveryMinter struct {
	entryID int64
	mrIID   int64
}

func recoveryMinterOf(e *store.MergeQueueEntry) recoveryMinter {
	return recoveryMinter{entryID: e.ID, mrIID: e.MRIID}
}

func pipelineLane(e *store.MergeQueueEntry, proof PipelineProof) string {
	if lane, _ := e.Detail[detailPipelineLane].(string); lane == "queue" {
		return lane
	}
	return "adopted"
}

func (p *Processor) observePipelineTiming(ctx context.Context, forge Forge, e *store.MergeQueueEntry, pipelineID int64, lane string) {
	if pipelineID <= 0 {
		return
	}
	timing, err := forge.PipelineTiming(ctx, pipelineID)
	if err != nil {
		p.logger().Warn("merge queue: pipeline timing read failed", "pipeline_id", pipelineID, "mr", e.MRIID, "error", err)
		return
	}
	if timing.Duration != nil {
		mills.MergeQueuePipelineWallSeconds.WithLabelValues(lane).Observe(*timing.Duration)
	}
	if timing.QueuedDuration != nil {
		mills.MergeQueuePipelineQueuedSeconds.WithLabelValues(lane).Observe(*timing.QueuedDuration)
	}
}

// promoteToMergingWithProof advances an awaiting_pipeline head whose proof
// turned terminal-green, recording which pipeline (and tree) proved it.
func (p *Processor) promoteToMergingWithProof(ctx context.Context, e *store.MergeQueueEntry, proof PipelineProof) error {
	detail := cloneDetail(e.Detail)
	detail[detailPipelineURL] = proof.WebURL
	detail[detailProofSource] = proof.Source
	detail[detailProof] = map[string]any{"pipeline_id": proof.ID, "sha": proof.SHA, "tree": proof.Tree}
	_, err := p.Store.MergeQueue.Transition(ctx, store.MergeQueueTransition{
		ID: e.ID, From: store.MergeQueueAwaitingPipeline, To: store.MergeQueueMerging,
		Detail: detail,
	})
	if err == nil {
		ProofSourceTotal.WithLabelValues(proof.Source).Inc()
		if proof.Source == "speculative" {
			SpeculationTotal.WithLabelValues("hit").Inc()
		}
	}
	return ignoreConflict(err)
}

// pipelineProgressing reports whether a pipeline status can still reach a
// terminal verdict on its own (created / waiting_for_resource / preparing /
// pending / running / scheduled). Terminal statuses and the dead-ends —
// skipped, and manual (blocked on a human playing a job) — are not.
func pipelineProgressing(status string) bool {
	switch status {
	case "success", "failed", "canceled", "skipped", "manual":
		return false
	default:
		return true
	}
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
				fmt.Sprintf("head moved before merge: authorized %s, observed %s", shortSHA(headMoved.ReviewedSHA), shortSHA(headMoved.ObservedSHA)),
				map[string]any{detailObservedSHA: headMoved.ObservedSHA})
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
		e.SettledAt = got.SettledAt
	}
	p.cleanupSpeculation(ctx, e, false)
	p.appendEvent(ctx, e, "mergequeue.merged", map[string]any{"merged_sha": mergedSHA, "proof": e.Detail[detailProof], "proof_source": e.Detail[detailProofSource]})
	owners, err := p.correctEscalatedRunVerdicts(ctx, e, mergedSHA)
	if err != nil {
		return err
	}
	if isExternalCandidate(e) {
		if err := p.settleExternalAdoption(ctx, e, owners, mergedSHA); err != nil {
			return err
		}
	}
	p.logger().Info("merge queue: merged", "run", e.PipelineRunID, "mr", e.MRIID, "project", e.Project, "sha", shortSHA(mergedSHA))
	return nil
}

// RunVerdictKindMergeQueueSettled is the Trustworthy-Verdicts correction
// source for MRs the queue lands AFTER their pipeline run escalated (a stage
// wait that timed out, an eviction whose external re-enqueue merged, an
// external candidate landing an escalated run's MR). It MUST stay equal to
// mills.RunVerdictKindMergeQueueSettled — registered there in
// RunVerdictCorrectionKinds so bulk discounting (storm rule, KPI writer,
// guard reports) sees it; a sync test in pkg/mills pins the pair. Defined as
// a literal here because pkg/mills imports this package.
const RunVerdictKindMergeQueueSettled = "run.verdict.mergequeue_settled"

// correctEscalatedRunVerdicts appends a first-writer verdict correction onto
// every ESCALATED pipeline run that owns the just-merged MR in the entry's
// project. The terminal run row is never mutated (verdict = supersede-chain
// HEAD); consumers resolve the correction through the run.verdict.* contract.
// Lookup and append failures leave the funnel conservative and defer backlog
// closure; the settled-entry replay retries both on the next processor tick.
func (p *Processor) correctEscalatedRunVerdicts(ctx context.Context, e *store.MergeQueueEntry, mergedSHA string) ([]*store.PipelineRun, error) {
	if p.Store == nil || p.Store.Events == nil || p.Store.Pipeline == nil {
		return nil, nil
	}
	runs, err := p.Store.Pipeline.ListByMRIID(ctx, e.MRIID)
	if err != nil {
		p.logger().Warn("merge queue: verdict correction lookup failed", "mr", e.MRIID, "error", err)
		return nil, err
	}
	if isExternalCandidate(e) {
		branchRuns, err := p.externalBranchOwners(ctx, e)
		if err != nil {
			return nil, err
		}
		runs = append(runs, branchRuns...)
	}
	seen := make(map[string]bool)
	owners := make([]*store.PipelineRun, 0, len(runs))
	for _, run := range runs {
		if run == nil || run.State != store.PipelineEscalated || seen[run.ID] {
			continue
		}
		// MR iids are per-project; a foreign run that merely shares the iid
		// must not be corrected. AuthorizedProject resolves the run's durable
		// stage provenance; unknown provenance skips fail-closed.
		project, perr := p.Store.Pipeline.AuthorizedProject(ctx, run.ID)
		if perr != nil || !store.SameRepo(project, e.Project) {
			continue
		}
		seen[run.ID] = true
		owners = append(owners, run)
		appended, verr := p.Store.Events.AppendOnceBySubjectKind(ctx, &store.Event{
			Actor:       eventActor,
			Kind:        RunVerdictKindMergeQueueSettled,
			SubjectKind: "pipeline_run",
			SubjectID:   run.ID,
			Payload: map[string]any{
				"class":       "merged_after_escalation",
				"prior_class": run.EscalationClass,
				"outcome":     "merged",
				"backlog_id":  run.BacklogID,
				"mr_iid":      e.MRIID,
				"project":     e.Project,
				"merged_sha":  mergedSHA,
			},
		})
		if verr != nil {
			p.logger().Warn("merge queue: verdict correction append failed", "run", run.ID, "error", verr)
			return owners, verr
		}
		if appended {
			p.logger().Info("merge queue: superseded escalated run verdict", "run", run.ID, "mr", e.MRIID)
		}
	}
	return owners, nil
}

// settleExternalAdoption closes each backlog item proven to own the external
// candidate by MR or exact canonical branch, authorized against durable run
// project provenance above.
func (p *Processor) settleExternalAdoption(ctx context.Context, e *store.MergeQueueEntry, owners []*store.PipelineRun, mergedSHA string) error {
	seen := make(map[string]struct{}, len(owners))
	for _, run := range owners {
		if run == nil || run.BacklogID == "" {
			continue
		}
		if _, ok := seen[run.BacklogID]; ok {
			continue
		}
		seen[run.BacklogID] = struct{}{}

		item, err := p.Store.Backlog.Get(ctx, run.BacklogID)
		if err != nil {
			return fmt.Errorf("external mq adoption backlog %s: %w", run.BacklogID, err)
		}
		if item.State != store.BacklogEscalated {
			continue
		}
		event := &store.Event{
			Actor: eventActor, Kind: "backlog.settled",
			SubjectKind: "backlog", SubjectID: item.ID,
			Payload: map[string]any{
				"reason": "external_mq_adoption", "backlog_id": item.ID,
				"run_id": run.ID, "mr_iid": e.MRIID, "project": e.Project,
				"merged_sha": mergedSHA,
			},
		}
		_, inserted, err := p.Store.Backlog.TransitionStateWithEventOnce(
			ctx, item.ID, item.ClaimVersion, item.State, store.BacklogMerged, event,
		)
		if err != nil {
			return fmt.Errorf("external mq adoption settle %s: %w", item.ID, err)
		}
		if inserted {
			p.logger().Info("merge queue: settled adopted backlog", "backlog", item.ID, "run", run.ID, "mr", e.MRIID)
		}
	}
	return nil
}

// settleRecentExternalAdoptions is the restart-safe half of adoption: settled
// queue rows remain durable after their active lane disappears, so each tick
// can retry the idempotent backlog transition.
func (p *Processor) settleRecentExternalAdoptions(ctx context.Context) {
	entries, err := p.Store.MergeQueue.ListSettled(ctx, time.Time{}, 100)
	if err != nil {
		p.logger().Warn("merge queue: settled adoption sweep failed", "error", err)
		return
	}
	for _, e := range entries {
		if e != nil {
			p.cleanupSpeculation(ctx, e, e.State == store.MergeQueueEvicted)
		}
		if e == nil || e.State != store.MergeQueueMerged || !isExternalCandidate(e) {
			continue
		}
		owners, err := p.correctEscalatedRunVerdicts(ctx, e, e.MergedSHA)
		if err == nil {
			err = p.settleExternalAdoption(ctx, e, owners, e.MergedSHA)
		}
		if err != nil && ctx.Err() == nil {
			p.logger().Warn("merge queue: external adoption replay failed", "mr", e.MRIID, "error", err)
		}
	}
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
	p.cleanupSpeculation(ctx, e, true)
	EvictionsTotal.WithLabelValues(reason).Inc()
	payload := map[string]any{"reason": reason, "detail": detail}
	p.appendEvent(ctx, e, "mergequeue.evicted", payload)
	p.logger().Warn("merge queue: evicted", "run", e.PipelineRunID, "mr", e.MRIID, "reason", reason, "detail", detail)
	p.maybeRequeueEviction(ctx, e, reason, extra)
	return nil
}

// evictionRequeueProducer marks candidates the eviction hop itself minted; an
// eviction of such a candidate is final (one hop, ever).
const evictionRequeueProducer = "mergequeue_evictor"

// maybeRequeueEviction is the shepherd-A2 hop: a head_moved eviction carries
// the observed successor and a ci_timeout eviction carries a head whose
// pipeline never proved itself in the window — both re-enter ONCE as external
// candidates, which merge only on a terminal successful pipeline for the head
// (driveQueued routes external candidates through awaiting_pipeline). All
// other reasons are genuine dead-ends and stay evicted.
func (p *Processor) maybeRequeueEviction(ctx context.Context, e *store.MergeQueueEntry, reason string, extra map[string]any) {
	if p.External == nil || p.RequeueEvictions == nil || !p.RequeueEvictions() {
		return
	}
	if producer, _ := e.Detail["producer"].(string); producer == evictionRequeueProducer {
		return // the hop's own candidate evicted again: final
	}
	sha := e.CurrentSHA
	switch reason {
	case store.MergeQueueEvictHeadMoved:
		if observed, _ := extra[detailObservedSHA].(string); observed != "" {
			sha = observed
		}
	case store.MergeQueueEvictCITimeout:
		// same head, fresh awaiting_pipeline window
	default:
		return
	}
	if sha == "" {
		return
	}
	res, err := p.External.Enqueue(ctx, ExternalCandidate{
		Producer:       evictionRequeueProducer,
		IdempotencyKey: fmt.Sprintf("%s:%d:%s", e.Project, e.MRIID, sha),
		Project:        e.Project,
		MRIID:          e.MRIID,
		SourceBranch:   e.SourceBranch,
		TargetBranch:   e.TargetBranch,
		ObservedSHA:    sha,
	})
	if err != nil {
		p.logger().Warn("merge queue: eviction requeue failed", "run", e.PipelineRunID, "mr", e.MRIID, "error", err)
		return
	}
	EvictionRequeuesTotal.WithLabelValues(reason, res.Outcome).Inc()
	p.appendEvent(ctx, e, "mergequeue.eviction_requeued", map[string]any{
		"reason": reason, "sha": sha, "outcome": res.Outcome,
	})
	p.logger().Info("merge queue: eviction requeued as external candidate",
		"run", e.PipelineRunID, "mr", e.MRIID, "reason", reason, "sha", shortSHA(sha), "outcome", res.Outcome)
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
