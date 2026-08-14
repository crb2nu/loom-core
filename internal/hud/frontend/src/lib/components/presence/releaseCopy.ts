// Confirmation copy for the three force-release routes on the File Claims tab.
//
// Extracted from PresenceClaimsTab so the branches are testable: every route
// here hands a file another agent is holding to whoever grabs it next, and the
// dialog copy is the only thing that tells the operator which blast radius they
// are about to accept. Getting the wrong branch's message in front of them is
// the failure mode worth a test.

import type { FileClaimInfo } from '../../stores/fleet.svelte.ts';
import { truncatePath } from '../../utils/format.ts';

export type PendingRelease =
  | { kind: 'selected' }
  | { kind: 'conflicts' }
  | { kind: 'single'; claim: FileClaimInfo };

export interface ReleaseCopy {
  title: string;
  message: string;
}

export interface ReleaseCopyContext {
  /** Rows ticked in the table — drives the "selected" count. */
  selectedCount: number;
  /** Pre-joined, already-truncated conflicting paths. */
  conflictPathList: string;
}

export function shortClaimPath(path: string): string {
  return truncatePath(path, 50);
}

/** Empty strings when nothing is pending — the dialog is closed in that state. */
export function releaseConfirmCopy(
  pending: PendingRelease | null,
  { selectedCount, conflictPathList }: ReleaseCopyContext,
): ReleaseCopy {
  switch (pending?.kind) {
    case 'selected':
      return {
        title: 'Release selected file claims?',
        message: `Release ${selectedCount} file claim(s)? Other agents may immediately begin editing these files.`,
      };
    case 'conflicts':
      return {
        title: 'Release all conflicting claims?',
        message: `This releases every agent's claim on every conflicting path: ${conflictPathList}.`,
      };
    case 'single':
      return {
        title: 'Release this file claim?',
        message: `Release ${pending.claim.agent_id}'s claim on ${shortClaimPath(pending.claim.file_path)}? That agent may be mid-edit, and other agents may immediately begin editing the file.`,
      };
    default:
      return { title: '', message: '' };
  }
}
