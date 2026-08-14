/**
 * Design system tokens — JS-side constants that mirror CSS custom properties.
 * Use these for values needed in JS (animation durations, breakpoints, polling intervals).
 * CSS values should be consumed via var(--token-name) in stylesheets.
 */

// ---- Timing (ms) ----

export const DURATION_FAST = 100;
export const DURATION_NORMAL = 200;
export const DURATION_SLOW = 350;

// ---- Polling & SSE (ms) ----

export const POLL_INTERVAL_HEALTH = 5_000;
export const POLL_INTERVAL_FLEET = 15_000;
export const POLL_INTERVAL_MEMORY = 10_000;
export const POLL_INTERVAL_WORKFLOWS = 5_000;
export const POLL_INTERVAL_STREAM = 5_000;
export const POLL_FALLBACK = 30_000;

export const SSE_RETRY_INITIAL = 1_000;
export const SSE_RETRY_MAX = 30_000;
export const SSE_CIRCUIT_THRESHOLD = 5;

// ---- Layout (px) ----

export const HEADER_HEIGHT = 40;
export const STATUSBAR_HEIGHT = 28;
export const DRAWER_WIDTH = 400;
export const DRAWER_MIN_WIDTH = 320;
export const DRAWER_MAX_WIDTH = 600;

// ---- Breakpoints (px) ----

export const BREAKPOINT_SM = 640;
export const BREAKPOINT_MD = 1024;
export const BREAKPOINT_LG = 1440;
export const BREAKPOINT_XL = 1920;

// ---- Lists ----

export const VIRTUAL_SCROLL_THRESHOLD = 50;
export const VIRTUAL_ITEM_HEIGHT = 32;
export const VIRTUAL_BUFFER = 10;

// ---- Data limits ----

export const TOKEN_HISTORY_SIZE = 20;
export const TIMELINE_RING_SIZE = 200;
export const ACTIVITY_FEED_LIMIT = 10;
export const SPARKLINE_HISTORY = 20;

// ---- Agent type colors (for JS-side usage) ----

export const AGENT_COLORS: Record<string, string> = {
  claude: 'var(--agent-claude)',
  codex: 'var(--agent-codex)',
  gemini: 'var(--agent-gemini)',
  copilot: 'var(--agent-copilot)',
};

// ---- Status variant mapping ----

export type BadgeVariant = 'info' | 'success' | 'warning' | 'error' | 'accent' | 'muted';

/**
 * The one status→tone map for the whole HUD. Consumed via statusVariant() /
 * statusColor() in utils/format.ts — no panel may hand-roll its own.
 *
 * The semantic, fixed so a status means one thing wherever it is scanned:
 *   info    — work in flight (running, in_progress, building, creating)
 *   success — work that landed or is healthy (completed, done, merged,
 *             resolved, active, approved, healthy)
 *   error   — work that broke or is unreachable (failed, error, blocked,
 *             down, rejected)
 *   warning — work waiting on a human or degrading (pending, waiting,
 *             waiting_approval, paused, escalated, degraded)
 *   muted   — work not happening (queued, idle, offline, cancelled, stopped)
 *
 * Absent keys fall through to 'info' in statusVariant(), which reads as "in
 * flight" — every state a row can actually carry must be spelled out here.
 */
export const STATUS_VARIANTS: Record<string, BadgeVariant> = {
  // In flight
  in_progress: 'info',
  running: 'info',
  // Spawn startup states. Previously absent, so a booting spawn fell through
  // to the default tone instead of reading as work in flight.
  creating: 'info',
  building: 'info',
  // Landed / healthy
  active: 'success',
  completed: 'success',
  resolved: 'success',
  healthy: 'success',
  approved: 'success',
  merged: 'success',
  done: 'success',
  // Broken / unreachable
  failed: 'error',
  error: 'error',
  blocked: 'error',
  down: 'error',
  rejected: 'error',
  // Waiting on a human / degrading
  pending: 'warning',
  waiting: 'warning',
  waiting_approval: 'warning',
  degraded: 'warning',
  // Mills run/backlog states. Absent keys left the Warps State column
  // permanently grey — every mills state a row can carry is spelled out.
  paused: 'warning',
  escalated: 'warning',
  // Not happening
  queued: 'muted',
  idle: 'muted',
  offline: 'muted',
  cancelled: 'muted',
  stopped: 'muted',
  // Memory-specific
  compressed: 'accent',
  expired: 'error',
};
