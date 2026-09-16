import { describe, expect, it } from 'vitest';
import type {
  BacklogItemLike,
  CouncilRunLike,
  MillsStatusLike,
  PipelineRunLike,
} from './mills.systemHealth.ts';
import { computeSystemHealth } from './mills.systemHealth.ts';

// Unit coverage for the Mills Overview "System Health" banner derivation.
//
// What the core cases demonstrate:
//   1. broken    — escalations happened, zero merges in last 24h
//   2. in_flight — at least one active pipeline run
//   3. idle      — council has never run, no backlog, no merges
//   4. healthy   — merges in 24h, no escalations, nothing active

// Aliases keep the cases readable while still typechecking against the
// pure-helper shapes.
type BacklogItem = BacklogItemLike & { Title?: string; State?: string; Priority?: string };
type CouncilRun = CouncilRunLike & { Trigger?: string; Outcome?: string };
type MillsStatus = MillsStatusLike;
type PipelineRun = PipelineRunLike & { BacklogID?: string; Template?: string; Attempts?: number };

const NOW = Date.parse('2026-05-16T12:00:00Z');
const HOUR_AGO = new Date(NOW - 60 * 60 * 1000).toISOString();
const TWO_DAYS_AGO = new Date(NOW - 48 * 60 * 60 * 1000).toISOString();

const baseStatus: MillsStatus = {
  active_pipeline_runs: 0,
  queue_depth: 0,
};

describe('mills.systemHealth: computeSystemHealth', () => {
  // 1. broken: 3 escalations in 24h, zero merges.
  it('broken', () => {
    const broken = computeSystemHealth({
      pipelineRuns: [
        { ID: 'r1', BacklogID: 'b1', Template: 't', State: 'escalated', Attempts: 1, EndedAt: HOUR_AGO },
        { ID: 'r2', BacklogID: 'b2', Template: 't', State: 'escalated', Attempts: 1, EndedAt: HOUR_AGO },
        { ID: 'r3', BacklogID: 'b3', Template: 't', State: 'escalated', Attempts: 1, EndedAt: HOUR_AGO },
      ] as PipelineRun[],
      status: baseStatus,
      councilRuns: [{ ID: 'c1', Trigger: 'cron', Outcome: 'ok' } as CouncilRun],
      backlog: [{ ID: 'b1', Title: 't', State: 'escalated', Priority: 'P1' } as BacklogItem],
      now: NOW,
    });
    expect(broken).toMatchObject({ state: 'broken', escalations_24h: 3, merges_24h: 0 });
  });

  // 2. in_flight: two active runs, none escalated, no merges yet.
  it('in_flight', () => {
    const inFlight = computeSystemHealth({
      pipelineRuns: [
        { ID: 'r1', BacklogID: 'b1', Template: 't', State: 'implementing', Attempts: 1, StartedAt: HOUR_AGO },
        { ID: 'r2', BacklogID: 'b2', Template: 't', State: 'ci', Attempts: 1, StartedAt: HOUR_AGO },
      ] as PipelineRun[],
      status: { ...baseStatus, active_pipeline_runs: 2 },
      councilRuns: [{ ID: 'c1', Trigger: 'cron', Outcome: 'ok' } as CouncilRun],
      backlog: [{ ID: 'b1', Title: 't', State: 'ready', Priority: 'P1' } as BacklogItem],
      now: NOW,
    });
    expect(inFlight).toMatchObject({ state: 'in_flight', active_runs: 2, escalations_24h: 0 });
  });

  // 3. idle: council has never run, backlog empty, no merges, nothing active.
  it('idle', () => {
    const idle = computeSystemHealth({
      pipelineRuns: [],
      status: baseStatus,
      councilRuns: [],
      backlog: [],
      now: NOW,
    });
    expect(idle).toMatchObject({ state: 'idle', council_runs_total: 0, merges_24h: 0 });
  });

  // 4. healthy: a merge inside the 24h window, no escalations, nothing active.
  it('healthy', () => {
    const healthy = computeSystemHealth({
      pipelineRuns: [
        { ID: 'r1', BacklogID: 'b1', Template: 't', State: 'done', Attempts: 1, EndedAt: HOUR_AGO },
      ] as PipelineRun[],
      status: baseStatus,
      councilRuns: [{ ID: 'c1', Trigger: 'cron', Outcome: 'ok' } as CouncilRun],
      backlog: [],
      now: NOW,
    });
    expect(healthy).toMatchObject({ state: 'healthy', merges_24h: 1, escalations_24h: 0 });
  });

  // 5. negative case: an old merge (>24h) should NOT count, falls to idle.
  it('old-merge-no-count', () => {
    const oldMerge = computeSystemHealth({
      pipelineRuns: [
        { ID: 'r1', BacklogID: 'b1', Template: 't', State: 'done', Attempts: 1, EndedAt: TWO_DAYS_AGO },
      ] as PipelineRun[],
      status: baseStatus,
      councilRuns: [],
      backlog: [],
      now: NOW,
    });
    expect(oldMerge).toMatchObject({ merges_24h: 0 });
    // last_successful_merge_at still exposed for "last merged X days ago" copy
    expect(oldMerge.last_successful_merge_at).toBe(TWO_DAYS_AGO);
  });

  // 6. blind active-only list (real-world shape): the operator endpoint
  // returns ONLY active runs, so the list has no terminal `done` row — the
  // run-list derivation yields last_successful_merge_at=null. The
  // authoritative `lastMergeAt` override (from status.last_merge_at) must
  // win so the broken banner can report a real "last successful merge".
  it('blind-list-override', () => {
    const blindWithOverride = computeSystemHealth({
      pipelineRuns: [
        { ID: 'r1', BacklogID: 'b1', Template: 't', State: 'escalated', Attempts: 1, EndedAt: HOUR_AGO },
      ] as PipelineRun[],
      status: baseStatus,
      councilRuns: [{ ID: 'c1', Trigger: 'cron', Outcome: 'ok' } as CouncilRun],
      backlog: [],
      now: NOW,
      escalatedRuns24h: 1,
      mergedRuns24h: 0,
      lastMergeAt: TWO_DAYS_AGO,
    });
    expect(blindWithOverride).toMatchObject({ state: 'broken', escalations_24h: 1, merges_24h: 0 });
    // The lastMergeAt override must win over the blind null derivation.
    expect(blindWithOverride.last_successful_merge_at).toBe(TWO_DAYS_AGO);
  });
});
