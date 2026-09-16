import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { labsAuthStore } from '../../stores/labsAuth.svelte.ts';
import { ForgeNotConnectedError } from '../../stores/forge.svelte.ts';
import {
  DEFAULT_PROTECTED_PATHS,
  createForgeProject,
  defaultVisibilityFor,
  onboardProject,
  runOnboarding,
  slugify,
  twinCandidates,
} from './intakeActions.ts';

const fetchMock = vi.fn();

function reply(status: number, body: unknown): Response {
  return new Response(typeof body === 'string' ? body : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

beforeEach(() => {
  labsAuthStore.setAdminToken('tok');
  vi.stubGlobal('fetch', fetchMock);
  fetchMock.mockReset();
});

afterEach(() => {
  vi.unstubAllGlobals();
  labsAuthStore.clearAdminToken();
});

describe('intake helpers', () => {
  it('applies the bucket visibility rule and slugifies names', () => {
    expect(defaultVisibilityFor('libs')).toBe('public');
    expect(defaultVisibilityFor('libs/sub')).toBe('public');
    expect(defaultVisibilityFor('services')).toBe('private');
    expect(slugify('My New Service!')).toBe('my-new-service');
    expect(DEFAULT_PROTECTED_PATHS).toContain('.gitlab-ci.yml');
  });

  it('finds a GitHub repo’s GitLab twins by exact name, registry first, de-duplicated', () => {
    const registry = ['services/loom-core', 'libs/fi-fhir'];
    const search = [
      { path: 'libs/fi-fhir', name: 'fi-fhir' },
      { path: 'labs/fi-fhir', name: 'fi-fhir' },
      { path: 'libs/fi-fhir-old', name: 'fi-fhir-old' },
    ];
    expect(twinCandidates('fi-fhir', registry, search)).toEqual(['libs/fi-fhir', 'labs/fi-fhir']);
    expect(twinCandidates('FI-FHIR.git', registry, search)).toEqual(['libs/fi-fhir', 'labs/fi-fhir']);
    expect(twinCandidates('brand-new', registry, search)).toEqual([]);
    expect(twinCandidates('', registry, search)).toEqual([]);
  });
});

describe('onboardProject', () => {
  it('sends the admin token and distinguishes first registration from a repeat', async () => {
    const entry = { project: 'labs/x', sources: ['bootstrapped'], protected_paths: 'global', protected_path_count: 1, ready: true, blockers: [], pending: ['git_policy_missing'] };
    fetchMock.mockResolvedValueOnce(reply(201, entry)).mockResolvedValueOnce(reply(409, entry));
    const first = await onboardProject({ project: 'labs/x' });
    expect(first.alreadyRegistered).toBe(false);
    expect(first.entry.project).toBe('labs/x');
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/mills/projects/onboard');
    expect(new Headers(init.headers as HeadersInit).get('X-Admin-Token')).toBe('tok');
    const second = await onboardProject({ project: 'labs/x' });
    expect(second.alreadyRegistered).toBe(true);
  });

  it('rejects without a network hit when the bar is empty, and maps 401', async () => {
    labsAuthStore.clearAdminToken();
    await expect(onboardProject({ project: 'labs/x' })).rejects.toThrow();
    expect(fetchMock).not.toHaveBeenCalled();
    labsAuthStore.setAdminToken('tok');
    fetchMock.mockResolvedValueOnce(reply(401, 'nope'));
    await expect(onboardProject({ project: 'labs/x' })).rejects.toThrow(/Labs access bar/);
  });
});

describe('createForgeProject', () => {
  it('surfaces the daemon connect hint as a typed error', async () => {
    fetchMock.mockResolvedValueOnce(reply(503, { error: 'github is not connected', provider: 'github', connect_hint: 'gh auth login' }));
    await expect(createForgeProject({ name: 'x', group: 'services', mirror_to_github: true })).rejects.toBeInstanceOf(ForgeNotConnectedError);
  });
  it('attaches the partial step ledger to a failed create', async () => {
    fetchMock.mockResolvedValueOnce(reply(502, { error: 'seed failed', result: { project: 'services/x', steps: [{ step: 'gitlab_project', status: 'done' }, { step: 'seed_commit', status: 'failed', detail: 'boom' }], dry_run: false } }));
    try {
      await createForgeProject({ name: 'x', group: 'services', mirror_to_github: false });
      throw new Error('expected rejection');
    } catch (e) {
      const err = e as Error & { result?: { steps: unknown[] } };
      expect(err.message).toBe('seed failed');
      expect(err.result?.steps).toHaveLength(2);
    }
  });
});

describe('runOnboarding', () => {
  it('registers then opens the policy MR, reporting each step', async () => {
    const entry = { project: 'labs/x', sources: ['bootstrapped'], protected_paths: 'global', protected_path_count: 1, ready: true, blockers: [], pending: ['git_policy_missing'] };
    fetchMock
      .mockResolvedValueOnce(reply(201, entry))
      .mockResolvedValueOnce(reply(200, { changed: true, edited: ['cross_repo.demand_projects'], skipped: [], mr_url: 'https://gl/mr/1', mr_iid: 1, restart_required: true, message: 'opened' }));
    const seen: number[] = [];
    const ledger = await runOnboarding('labs/x', { policyMR: true, protectedPaths: ['x'] }, (l) => seen.push(l.steps.length));
    expect(ledger.steps.map((s) => `${s.step}:${s.status}`)).toEqual(['register:done', 'policy_mr:done']);
    expect(ledger.policy?.mr_url).toBe('https://gl/mr/1');
    expect(seen).toEqual([1, 2]);
    const policyBody = JSON.parse((fetchMock.mock.calls[1] as [string, RequestInit])[1].body as string);
    expect(policyBody.protected_paths).toEqual(['x']);
  });

  it('stops after a failed registration and never opens an MR', async () => {
    fetchMock.mockResolvedValueOnce(reply(404, 'project "labs/x" not found on GitLab'));
    const ledger = await runOnboarding('labs/x', { policyMR: true });
    expect(ledger.steps).toHaveLength(1);
    expect(ledger.steps[0].status).toBe('failed');
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
