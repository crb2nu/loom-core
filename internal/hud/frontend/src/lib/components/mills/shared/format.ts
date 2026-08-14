/**
 * Canonical display formatters for Mills surfaces.
 *
 * Durations passed to fmtDuration are milliseconds. Timestamp parsing for
 * running work belongs in elapsedMs so every caller shares the same null and
 * invalid-date behavior.
 */
import { formatDateTime, truncateId } from '../../../utils/format.ts';

const EMPTY = '—';

function finite(value: number | null | undefined): value is number {
  return value != null && Number.isFinite(value);
}

/** Format USD with enough detail for normal operational cost summaries. */
export function fmtCost(value: number | null | undefined): string {
  if (!finite(value)) return EMPTY;
  const sign = value < 0 ? '-' : '';
  const amount = Math.abs(value);
  if (amount === 0) return '$0';
  if (amount < 0.01) return `${sign}<$0.01`;
  if (amount >= 100) return `${sign}$${amount.toFixed(0)}`;
  if (amount >= 10) return `${sign}$${amount.toFixed(1)}`;
  return `${sign}$${amount.toFixed(2)}`;
}

/** Format USD at fixed precision for per-run journal and audit cells. */
export function fmtCostExact(value: number | null | undefined, decimalPlaces = 4): string {
  if (!finite(value)) return EMPTY;
  return `$${value.toFixed(decimalPlaces)}`;
}

/** Format a 0–1 ratio as a one-decimal percentage. */
export function fmtPct(value: number | null | undefined): string {
  return finite(value) ? `${(value * 100).toFixed(1)}%` : EMPTY;
}

/** Format a non-negative elapsed duration expressed in milliseconds. */
export function fmtDuration(milliseconds: number | null | undefined): string {
  if (!finite(milliseconds) || milliseconds < 0) return EMPTY;
  if (milliseconds < 1_000) return `${Math.round(milliseconds)}ms`;
  if (milliseconds < 60_000) return `${(milliseconds / 1_000).toFixed(1)}s`;
  const minutes = Math.floor(milliseconds / 60_000);
  const seconds = Math.floor((milliseconds % 60_000) / 1_000);
  if (minutes < 60) return `${minutes}m ${seconds}s`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${minutes % 60}m`;
}

/** Return elapsed milliseconds, using now for an unfinished run. */
export function elapsedMs(start?: string | null, end?: string | null): number | undefined {
  if (!start) return undefined;
  const started = new Date(start).getTime();
  const ended = end ? new Date(end).getTime() : Date.now();
  if (!Number.isFinite(started) || !Number.isFinite(ended) || ended < started) return undefined;
  return ended - started;
}

/** Format a Mills run timestamp using the HUD's shared date-time formatter. */
export function fmtRunTime(timestamp: string | number | Date | null | undefined): string {
  const formatted = formatDateTime(timestamp);
  return formatted === '---' ? EMPTY : formatted;
}

/** Compact run ID display delegated to the HUD's common ID truncator. */
export function shortRunID(id: string | null | undefined): string {
  const formatted = truncateId(id);
  return formatted === '---' ? EMPTY : formatted;
}
