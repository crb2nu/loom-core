// Spawn-terminal-status filter in buildUnifiedAgents.
//
// Demonstrates four cases that the original buildUnifiedAgents got
// wrong by hardcoding `status: 'active'` on every spawn row:
//
//   1. spawn-only + completed status            → row is offline
//   2. spawn-only + running status              → row is active
//   3. presence-backed (active) + completed spawn → row downgraded to offline
//   4. presence-backed (active) + running spawn → row stays active

import { describe, expect, it } from 'vitest';
import { buildUnifiedAgents } from './agents.ts';

describe('agents — spawn terminal status in buildUnifiedAgents', () => {
  it('spawn-only completed → offline', () => {
    // 1. spawn-only + completed → offline (the original bug — was always 'active')
    const c1 = buildUnifiedAgents({
      sessions: [],
      agents: [],
      spawns: [{
        spawn_id: 'spawn-A',
        agent_id: 'spawn-claude-code-aaaaaaaaaaaa',
        status: 'completed',
        request: { project: 'loom-core', task_description: 'CI pipeline 9839 failed on fix/x' },
      }],
    });
    expect(c1[0]?.status).toBe('offline');
  });

  it('spawn-only running → active', () => {
    const c2 = buildUnifiedAgents({
      sessions: [],
      agents: [],
      spawns: [{
        spawn_id: 'spawn-B',
        agent_id: 'spawn-claude-code-bbbbbbbbbbbb',
        status: 'running',
        request: { project: 'loom-core' },
      }],
    });
    expect(c2[0]?.status).toBe('active');
  });

  it('presence+terminal-spawn → offline (downgrade)', () => {
    // 3. presence-backed (active) + completed spawn → downgraded to offline
    const c3 = buildUnifiedAgents({
      sessions: [],
      agents: [{
        agent_id: 'spawn-claude-code-cccccccccccc',
        status: 'active',
        has_presence: true,
        heartbeat_age_seconds: 30, // recent
      }],
      spawns: [{
        spawn_id: 'spawn-C',
        agent_id: 'spawn-claude-code-cccccccccccc',
        status: 'failed',
      }],
    });
    expect(c3[0]?.status).toBe('offline');
  });

  it('presence+running-spawn → active', () => {
    // 4. presence-backed (active) + running spawn → stays active
    const c4 = buildUnifiedAgents({
      sessions: [],
      agents: [{
        agent_id: 'spawn-claude-code-dddddddddddd',
        status: 'active',
        has_presence: true,
        heartbeat_age_seconds: 30,
      }],
      spawns: [{
        spawn_id: 'spawn-D',
        agent_id: 'spawn-claude-code-dddddddddddd',
        status: 'running',
      }],
    });
    expect(c4[0]?.status).toBe('active');
  });
});
