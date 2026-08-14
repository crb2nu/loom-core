// Pure helpers for the Sandbox (Labs) panel. Extracted from
// SandboxPanel.svelte during the Slice B2.4 panel decomp.

import { statusVariant } from './format.ts';
import type { BadgeVariant } from './tokens.ts';

export function formatUptime(seconds: number | null | undefined): string {
  if (!seconds || seconds <= 0) return '---';
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

export function eventIcon(type: string): string {
  switch (type) {
    case 'exec':         return '▶';
    case 'build':        return '⚒';
    case 'start':        return '◉';
    case 'stop':         return '○';
    case 'quality_gate': return '◎';
    default:             return '◈';
  }
}

import type { SandboxLanguage } from '../stores/sandbox.svelte.ts';

/** "go 1.26" | "go" — a compact label for one detected language runtime. */
export function languageLabel(lang: SandboxLanguage): string {
  const name = (lang.language || '').trim();
  const version = (lang.version || '').trim();
  if (name && version && version !== 'unknown') return `${name} ${version}`;
  return name || 'unknown';
}

/** Short 7-char fingerprint hash for display (matches devbox_build). */
export function shortHash(hash: string | null | undefined): string {
  if (!hash) return '';
  return hash.length > 7 ? hash.slice(0, 7) : hash;
}

/** Tone for a single quality-gate check based on pass/fail. */
export function checkTone(passed: boolean): 'success' | 'error' {
  return passed ? 'success' : 'error';
}

export function formatExecDuration(ms: number | null | undefined): string {
  if (!ms || ms <= 0) return 'pending';
  if (ms < 1000) return `${ms}ms`;
  const seconds = Math.floor(ms / 1000);
  const minutes = Math.floor(seconds / 60);
  if (minutes > 0) return `${minutes}m ${seconds % 60}s`;
  return `${seconds}s`;
}

// Delegate to the canonical status→tone map rather than a fourth local copy.
// Kept as a named wrapper because SandboxLive reads it as `execStatusTone`.
export function execStatusTone(status: string): BadgeVariant {
  return statusVariant(status);
}
