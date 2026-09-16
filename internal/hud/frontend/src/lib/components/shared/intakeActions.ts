// Mills intake requests — onboarding an existing repo, opening its gitops
// policy MR, and creating/importing a project through the HUD daemon's forge
// domain. All mutations ride adminFetch so the Labs access-bar token attaches
// and an empty bar fails fast without a network hit (same contract as
// bootstrapActions.ts / spinActions.ts).
import { adminFetch } from '../../stores/labsAuth.svelte.ts';
import { ForgeNotConnectedError, type ForgeProvider } from '../../stores/forge.svelte.ts';
import type { MillsProjectEntry } from '../../stores/millsProjects.svelte.ts';

export interface OnboardBody {
  project: string;
  web_url?: string;
  note?: string;
}

export interface PolicyMRBody {
  project: string;
  intake_issues?: boolean;
  protected_paths?: string[];
  max_usd_per_run?: number;
  max_runs_per_day?: number;
  reason?: string;
}

export interface PolicyMRResult {
  changed: boolean;
  edited: string[];
  skipped: string[];
  mr_url?: string;
  mr_iid?: number;
  branch?: string;
  checksum?: string;
  restart_required: boolean;
  message: string;
}

export interface CreateProjectBody {
  name: string;
  group: string;
  description?: string;
  visibility?: string;
  template?: string;
  mirror_to_github: boolean;
  github_owner?: string;
  github_visibility?: string;
  dry_run?: boolean;
}

export interface ImportProjectBody {
  github_repo: string;
  group: string;
  name?: string;
  description?: string;
  visibility?: string;
  mirror_back: boolean;
  dry_run?: boolean;
}

export interface StepResult {
  step: string;
  status: 'planned' | 'done' | 'exists' | 'skipped' | 'failed';
  detail?: string;
}

export interface ProjectResult {
  project: string;
  web_url?: string;
  default_branch?: string;
  seed_commit?: string;
  seed_paths?: string[];
  github?: { repo: string; web_url: string; mirror_id?: number; mirror_enabled: boolean };
  steps: StepResult[];
  dry_run: boolean;
}

/** Default protected paths offered for a newly onboarded repo: CI config
 *  (runs with runner credentials) plus the global auth/secret globs. */
export const DEFAULT_PROTECTED_PATHS = ['.gitlab-ci.yml', '**/*auth*.go', '**/secret*.yaml'];

async function failFrom(res: Response, action: string): Promise<never> {
  if (res.status === 401 || res.status === 403) {
    throw new Error('admin token missing or invalid — set it in the Labs access bar');
  }
  const text = (await res.text()).trim();
  let parsed: { error?: string; provider?: ForgeProvider; connect_hint?: string; result?: ProjectResult } | null = null;
  try {
    parsed = JSON.parse(text);
  } catch {
    parsed = null;
  }
  if (res.status === 503 && parsed?.provider && parsed.connect_hint) {
    throw new ForgeNotConnectedError(parsed.provider, parsed.connect_hint);
  }
  if (res.status === 503) {
    throw new Error(text || `${action}: the operator or daemon is not configured for this (503)`);
  }
  if (parsed?.error) {
    const err = new Error(parsed.error) as Error & { result?: ProjectResult };
    err.result = parsed.result;
    throw err;
  }
  throw new Error(text || `${action}: HTTP ${res.status}`);
}

/** Register an existing GitLab repo with the operator at runtime. 201 on
 *  first registration, 409 when already registered (both return the entry). */
export async function onboardProject(body: OnboardBody): Promise<{ entry: MillsProjectEntry; alreadyRegistered: boolean }> {
  const res = await adminFetch('/api/mills/projects/onboard', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
    requireToken: true,
    action: 'Onboarding a repo',
  });
  if (res.status === 409) {
    return { entry: (await res.json()) as MillsProjectEntry, alreadyRegistered: true };
  }
  if (!res.ok) await failFrom(res, 'Onboard');
  return { entry: (await res.json()) as MillsProjectEntry, alreadyRegistered: false };
}

/** Open the gitops MR that writes the repo's Git-policy admission. */
export async function openPolicyMR(body: PolicyMRBody): Promise<PolicyMRResult> {
  const res = await adminFetch('/api/mills/projects/policy-mr', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
    requireToken: true,
    action: 'Opening the policy MR',
  });
  if (!res.ok) await failFrom(res, 'Policy MR');
  return (await res.json()) as PolicyMRResult;
}

/** Create a GitLab project (seeded, optionally mirrored to GitHub). */
export async function createForgeProject(body: CreateProjectBody): Promise<ProjectResult> {
  const res = await adminFetch('/api/forge/projects', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
    requireToken: true,
    action: 'Creating a project',
  });
  if (!res.ok) await failFrom(res, 'Create project');
  return (await res.json()) as ProjectResult;
}

/** Import a GitHub repo into GitLab, optionally mirroring back. */
export async function importForgeProject(body: ImportProjectBody): Promise<ProjectResult> {
  const res = await adminFetch('/api/forge/projects/import', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
    requireToken: true,
    action: 'Importing a repo',
  });
  if (!res.ok) await failFrom(res, 'Import');
  return (await res.json()) as ProjectResult;
}

/** Bucket rule from the workspace branding policy: libs/* are public by
 *  default, everything else private unless allowlisted. */
export function defaultVisibilityFor(group: string): 'public' | 'private' {
  return group.trim().replace(/\/.*$/, '') === 'libs' ? 'public' : 'private';
}

export interface OnboardingOptions {
  note?: string;
  webURL?: string;
  /** Open the gitops policy MR after registering. */
  policyMR: boolean;
  intakeIssues?: boolean;
  protectedPaths?: string[];
  maxUSDPerRun?: number;
  maxRunsPerDay?: number;
  reason?: string;
}

export interface OnboardingLedger {
  steps: StepResult[];
  entry?: MillsProjectEntry;
  policy?: PolicyMRResult;
}

/**
 * The two-step onboarding of a GitLab repo: register at runtime (live now),
 * then optionally open the Git-policy MR (takes effect after the operator
 * restarts on the checksum bump). Each step lands in the ledger as it
 * completes; a failed step stops the sequence and is reported, never hidden.
 */
export async function runOnboarding(
  project: string,
  opts: OnboardingOptions,
  onStep?: (ledger: OnboardingLedger) => void,
): Promise<OnboardingLedger> {
  const ledger: OnboardingLedger = { steps: [] };
  const push = (s: StepResult) => {
    ledger.steps = [...ledger.steps, s];
    onStep?.(ledger);
  };
  try {
    const { entry, alreadyRegistered } = await onboardProject({ project, web_url: opts.webURL, note: opts.note });
    ledger.entry = entry;
    push({
      step: 'register',
      status: alreadyRegistered ? 'exists' : 'done',
      detail: alreadyRegistered ? 'already in the runtime registry' : 'registered — live now',
    });
  } catch (e) {
    push({ step: 'register', status: 'failed', detail: e instanceof Error ? e.message : String(e) });
    return ledger;
  }
  if (!opts.policyMR) {
    push({ step: 'policy_mr', status: 'skipped', detail: 'not requested' });
    return ledger;
  }
  try {
    const policy = await openPolicyMR({
      project,
      intake_issues: opts.intakeIssues,
      protected_paths: opts.protectedPaths,
      max_usd_per_run: opts.maxUSDPerRun,
      max_runs_per_day: opts.maxRunsPerDay,
      reason: opts.reason,
    });
    ledger.policy = policy;
    push({
      step: 'policy_mr',
      status: policy.changed ? 'done' : 'exists',
      detail: policy.changed ? `${policy.mr_url ?? 'MR opened'} — merge, then the operator restarts` : policy.message,
    });
  } catch (e) {
    push({ step: 'policy_mr', status: 'failed', detail: e instanceof Error ? e.message : String(e) });
  }
  return ledger;
}

/**
 * GitLab twins of a GitHub repo, by exact repo name: the workspace convention
 * is one canonical GitLab project per name with GitHub as its push mirror, so
 * crb2nu/fi-fhir almost always means libs/fi-fhir already exists. Registry
 * entries come first (Mills already knows them), then GitLab search hits,
 * de-duplicated and in a stable order. A name match is a hint the dialog
 * surfaces for the operator to confirm — never an automatic decision.
 */
export function twinCandidates(
  githubName: string,
  registryProjects: readonly string[],
  gitlabRepos: readonly { path: string; name: string }[],
): string[] {
  const key = githubName.trim().replace(/\.git$/, '').toLowerCase();
  if (!key) return [];
  const out: string[] = [];
  const seen = new Set<string>();
  const add = (p: string) => {
    if (!seen.has(p)) {
      seen.add(p);
      out.push(p);
    }
  };
  for (const p of registryProjects) {
    if ((p.split('/').pop() ?? '').toLowerCase() === key) add(p);
  }
  for (const r of gitlabRepos) {
    if (r.name.trim().toLowerCase() === key) add(r.path);
  }
  return out;
}

/** GitLab path slug from a free-text name. */
export function slugify(s: string): string {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 48);
}
