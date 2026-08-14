const GITLAB_BASE = 'https://gitlab.flexinfer.ai';
const DEFAULT_PROJECT = 'services/loom-core';

/** Build a GitLab merge-request URL from the Mills TargetProject wire value. */
export function mrURL(targetProject: string | null | undefined, iid: number | null | undefined): string | null {
  if (iid == null || !Number.isInteger(iid) || iid <= 0) return null;

  const project = (targetProject ?? '').trim().replace(/^\/+|\/+$/g, '');
  const projectPath = project
    ? project.includes('/')
      ? project
      : `services/${project.toLowerCase()}`
    : DEFAULT_PROJECT;

  return `${GITLAB_BASE}/${projectPath}/-/merge_requests/${iid}`;
}
