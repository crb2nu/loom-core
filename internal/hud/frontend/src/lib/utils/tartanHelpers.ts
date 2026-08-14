// Pure helpers for the Factory panel's bolt archive — the "tartan of the
// week". Every woven row on the live loom is a real merged MR; here the
// week's cloth becomes a persistent, exportable artifact. Rune-free so
// vitest can exercise the history→tartan mapping without a Svelte runtime,
// mirroring factoryHelpers.

import type { PipelineRun } from '../stores/mills.svelte.ts';
import type { BoltGrade } from '../stores/mills.svelte.ts';
import { seededPattern } from './factoryHelpers.ts';

/** One run woven into the archive cloth. */
export interface TartanRun {
  kind: 'bolt' | 'spark';
  runID: string;
  backlogID: string;
  costUSD?: number;
  grade?: BoltGrade;
  gradeNote?: string;
}

/** One day-band of the week's cloth, oldest run first. */
export interface TartanDay {
  /** Local calendar day, YYYY-MM-DD. */
  date: string;
  /** Short display label, e.g. "Mon 7/6". */
  label: string;
  runs: TartanRun[];
}

const WEEKDAY = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

function localDayKey(d: Date): string {
  const m = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  return `${d.getFullYear()}-${m}-${day}`;
}

function dayLabel(d: Date): string {
  return `${WEEKDAY[d.getDay()]} ${d.getMonth() + 1}/${d.getDate()}`;
}

/**
 * Group terminal runs into local calendar-day bands covering the `days`
 * days ending at `now` (oldest day first; every day present even when
 * empty, so the strip shows the quiet days too). A run lands on the day
 * it ENDED (fallback: started); runs outside the window, without a
 * usable timestamp, or in non-woven states (paused = held thread, not
 * cloth) are dropped — same weave rule as the live loom.
 */
export function archiveDays(runs: PipelineRun[], days: number, now: Date): TartanDay[] {
  const bands: TartanDay[] = [];
  const byKey = new Map<string, TartanDay>();
  for (let i = days - 1; i >= 0; i--) {
    const d = new Date(now.getFullYear(), now.getMonth(), now.getDate() - i);
    const band: TartanDay = { date: localDayKey(d), label: dayLabel(d), runs: [] };
    bands.push(band);
    byKey.set(band.date, band);
  }
  const stamped: Array<{ t: number; run: TartanRun; key: string }> = [];
  for (const run of runs) {
    if (!run?.ID) continue;
    const state = (run.State ?? '').toLowerCase();
    const kind = state === 'done' || state === 'merged' ? 'bolt'
      : state === 'escalated' ? 'spark'
      : null;
    if (!kind) continue;
    const raw = run.EndedAt ?? run.StartedAt;
    if (!raw) continue;
    const t = Date.parse(raw);
    if (!Number.isFinite(t)) continue;
    const key = localDayKey(new Date(t));
    if (!byKey.has(key)) continue;
    stamped.push({
      t,
      key,
      run: {
        kind, runID: run.ID, backlogID: run.BacklogID ?? '', costUSD: run.CostUSD,
        grade: run.Grade, gradeNote: run.GradeNote,
      },
    });
  }
  stamped.sort((a, b) => a.t - b.t);
  for (const s of stamped) byKey.get(s.key)!.runs.push(s.run);
  return bands;
}

/** Week totals for the caption line. */
export function archiveTotals(days: TartanDay[]): { bolts: number; sparks: number; costUSD: number } {
  let bolts = 0, sparks = 0, costUSD = 0;
  for (const day of days) {
    for (const run of day.runs) {
      if (run.kind === 'bolt') bolts++;
      else sparks++;
      costUSD += run.costUSD ?? 0;
    }
  }
  return { bolts, sparks, costUSD };
}

/** Resolved (self-contained — no CSS vars) colors for the exported file. */
export interface TartanColors {
  bg: string;
  bolt: string;
  spark: string;
  fog: string;
  dim: string;
}

export interface TartanOptions {
  colors: TartanColors;
  title?: string;
  /** Warp thread count (cells per woven row). */
  warpN?: number;
  cellW?: number;
  rowH?: number;
}

function esc(s: string): string {
  return s.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
}

/**
 * Render the week's cloth as one deterministic SVG string. Each run is
 * woven with the SAME seededPattern the live loom uses — the archive is
 * the loom's fabric, not a new chart. Colors arrive resolved so the
 * downloaded file renders identically outside the HUD (standup slides,
 * office TV). Same days + same options → byte-identical output.
 */
export function tartanSVG(days: TartanDay[], opts: TartanOptions): string {
  const warpN = opts.warpN ?? 48;
  const cellW = opts.cellW ?? 12;
  const rowH = opts.rowH ?? 10;
  const gutter = 86;
  const pad = 16;
  const headerH = 46;
  const bandGap = 10;
  const emptyH = 18;
  const C = opts.colors;
  const clothW = warpN * cellW;
  const width = gutter + clothW + pad * 2;
  const mono = 'ui-monospace, SFMono-Regular, Menlo, monospace';

  let y = headerH;
  const bands: string[] = [];
  let weaveRow = 0; // global parity so the twill texture runs through day joins
  for (const day of days) {
    const bandTop = y;
    const parts: string[] = [];
    if (day.runs.length === 0) {
      parts.push(
        `<line x1="${gutter + pad}" y1="${y + emptyH / 2}" x2="${gutter + pad + clothW}" y2="${y + emptyH / 2}" stroke="${esc(C.dim)}" stroke-width="1" stroke-dasharray="2 6"/>`,
        `<text x="${gutter + pad + clothW / 2}" y="${y + emptyH / 2 - 4}" text-anchor="middle" font-family="${mono}" font-size="8" fill="${esc(C.dim)}">no cloth</text>`,
      );
      y += emptyH;
    } else {
      for (const run of day.runs) {
        const cells = seededPattern(run.runID, warpN);
        const tone = run.kind === 'bolt' ? C.bolt : C.spark;
        const rects: string[] = [];
        for (let i = 0; i < warpN; i++) {
          const over = Number(cells[i]) ^ (weaveRow % 2);
          rects.push(
            `<rect x="${gutter + pad + i * cellW}" y="${y}" width="${cellW - 1.5}" height="${rowH - 1.5}" fill="${esc(tone)}" fill-opacity="${over ? 0.85 : 0.32}"/>`,
          );
        }
        const id = run.backlogID || run.runID;
        parts.push(
          `<g><title>${esc(`${run.kind} ${id} · ${run.kind === 'bolt' ? 'merged on green' : 'escalated'}`)}</title>${rects.join('')}</g>`,
        );
        y += rowH;
        weaveRow++;
      }
    }
    parts.unshift(
      `<text x="${gutter + pad - 10}" y="${bandTop + 9}" text-anchor="end" font-family="${mono}" font-size="9" fill="${esc(C.fog)}" fill-opacity="0.75">${esc(day.label)}</text>`,
    );
    bands.push(parts.join(''));
    y += bandGap;
  }
  const height = y - bandGap + pad;

  const totals = archiveTotals(days);
  const range = days.length > 0 ? `${days[0].label} – ${days[days.length - 1].label}` : '';
  const cost = totals.costUSD > 0 ? ` · $${totals.costUSD.toFixed(2)}` : '';
  const caption = `${range} · ${totals.bolts} bolt${totals.bolts === 1 ? '' : 's'} · ${totals.sparks} spark${totals.sparks === 1 ? '' : 's'}${cost}`;

  return [
    `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="0 0 ${width} ${height}" role="img" aria-label="${esc(`Bolt archive: ${caption}`)}">`,
    `<rect width="${width}" height="${height}" fill="${esc(C.bg)}"/>`,
    `<text x="${pad}" y="${pad + 8}" font-family="${mono}" font-size="11" font-weight="700" fill="${esc(C.fog)}">${esc(opts.title ?? 'tartan of the week')}</text>`,
    `<text x="${width - pad}" y="${pad + 8}" text-anchor="end" font-family="${mono}" font-size="9" fill="${esc(C.fog)}" fill-opacity="0.7">${esc(caption)}</text>`,
    ...bands,
    `</svg>`,
  ].join('');
}
