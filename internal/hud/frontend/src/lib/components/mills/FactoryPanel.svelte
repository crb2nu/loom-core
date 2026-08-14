<script lang="ts">
  /**
   * FactoryPanel — the dark factory, live. A canvas Jacquard loom where
   * every element is fed by real Mills state:
   *
   *   warp threads   ← backlog items strung on the beam (queued/running)
   *   shuttle        ← active pipeline runs (parks when the floor idles)
   *   green rows     ← runs reaching done/merged (pipelineHistory diff)
   *   amber rows     ← runs escalating
   *   gauges         ← KPI window counts (north star: merged 24h)
   *   floor log      ← active runs @ stage + recent terminal runs
   *
   * The weave-event mapping lives rune-free in utils/factoryHelpers.ts.
   */
  import { untrack } from 'svelte';
  import { millsStore } from '../../stores/mills.svelte.ts';
  import { fleetStore } from '../../stores/fleet.svelte.ts';
  import { router } from '../../stores/router.svelte.ts';
  import PanelHeader from '../shared/PanelHeader.svelte';
  import ErrorBanner from '../shared/ErrorBanner.svelte';
  import EmptyState from '../shared/EmptyState.svelte';
  import Badge from '../../widgets/Badge.svelte';
  import RollingNumber from '../../widgets/RollingNumber.svelte';
  import PipelineRunDetail from './PipelineRunDetail.svelte';
  import BoltArchive from './BoltArchive.svelte';
  import ShiftReport from './ShiftReport.svelte';
  import AndonMode from './AndonMode.svelte';
  import DepartureBoard from './DepartureBoard.svelte';
  import {
    diffStagePicks,
    diffTerminalRuns,
    fuelReading,
    policyTapeSeed,
    seededPattern,
    stageLabel,
    tapeHole,
    warpCountFor,
    type StagePick,
    type WeaveEvent,
  } from '../../utils/factoryHelpers.ts';
  import {
    departureRows,
    nextStageSince,
    suppressionRows,
    type StageObservation,
  } from '../../utils/departureHelpers.ts';
  import { rootAgentId } from '../../utils/agents.ts';
  import { createLoomSounds } from '../../utils/factorySounds.ts';
  import { patternsStore } from '../../stores/patterns.svelte.ts';
  import { bookNamesByRun, patternBooks } from '../../utils/patternBooks.ts';
  import PatternShelf from './PatternShelf.svelte';
  import MillEfficiencyStrip from './MillEfficiencyStrip.svelte';

  const fleetPollingOwner = Symbol('FactoryPanel');

  $effect(() => {
    millsStore.startPolling(15000);
    millsStore.historyActive = true; // history refreshes on the same tick
    void millsStore.fetchPipelineHistory();
    // Demand decisions move at council cadence (hours), so a mount-time
    // fetch plus the archive strip's 60s poller rhythm is honest without
    // adding load to the 15s tick.
    void millsStore.fetchDemandLog();
    fleetStore.startPolling(60000, fleetPollingOwner);
    // One-shot catalog fetch for the pattern shelf: the catalog is
    // near-static and the patterns poller has no owner refcount, so a
    // second poller here would race PatternsPanel's stop.
    void patternsStore.fetch();
    return () => {
      millsStore.historyActive = false;
      millsStore.stopPolling();
      fleetStore.stopPolling(fleetPollingOwner);
    };
  });

  let activeRuns = $derived(millsStore.pipelineRuns ?? []);
  let metrics = $derived(millsStore.kpis?.metrics ?? null);
  let backlogActive = $derived.by(() => {
    const byState = millsStore.backlogByState;
    // Threads strung on the beam: anything not yet woven off (merged) or
    // abandoned. Escalated stays strung — it's a flagged thread, not cloth.
    return (byState['queued'] ?? 0) + (byState['ready'] ?? 0) +
      (byState['running'] ?? 0) + (byState['escalated'] ?? 0) + (byState['paused'] ?? 0);
  });
  // Departure board (concept 4): stage-entry observations advance per
  // poll; DELAYED is "observed sitting in this stage past the fuse".
  // The previous map is read untracked — the effect keys off activeRuns
  // only, or writing stageSinceMap would retrigger it in a loop.
  let stageSinceMap = $state<Map<string, StageObservation>>(new Map());
  $effect(() => {
    const runs = activeRuns;
    stageSinceMap = nextStageSince(untrack(() => stageSinceMap), runs, Date.now());
  });
  // The floor log carries both sides of the ledger: departures (execution)
  // plus the council's recent refusals (demand). Suppressions cap at 2 so
  // the board stays a log of motion with a note of judgment, not the
  // reverse.
  let boardRows = $derived([
    ...departureRows(activeRuns, millsStore.pipelineHistory, stageSinceMap, Date.now()),
    ...suppressionRows(millsStore.demandLog, 2),
  ]);
  // Pattern shelf (concept 8): the catalog as books, with run counts
  // attributed run → backlog PlanID → pattern slug.
  let books = $derived(
    patternBooks(patternsStore.patterns, millsStore.backlog, activeRuns, millsStore.pipelineHistory),
  );
  // Paused = the autonomy kill-switch is off. The operator's status payload
  // uses snake_case (`policy_enabled`, handlers_status.go) — there is no
  // `Enabled` field, so the previous PascalCase read could never fire.
  // Same status-then-policy fallback OverviewPanel uses.
  let millsPaused = $derived(
    !(millsStore.status?.policy_enabled ?? millsStore.policy?.enabled ?? true),
  );
  // Bolt archive modal (concept 3): the week's cloth as an exportable
  // artifact. Local UX state — the archive owns its own history fetch.
  let showArchive = $state(false);
  let showShift = $state(false);
  // Andon mode (concept 6): the fullscreen office-TV board. Mode state
  // IS the router's third hash segment so #mills/factory/andon can be
  // bookmarked on a TV and survives reload.
  let showAndon = $derived(router.detail === 'andon');

  // Loom sounds (concept 9): opt-in, synthesized, and bound by the same
  // honesty rule as the canvas — audio fires only for a real event
  // (pick / bolt / spark), never for a reed beating air.
  const SOUND_PREF_KEY = 'loom-hud-factory-sound';
  const sounds = createLoomSounds();
  let soundOn = $state(false);
  $effect(() => {
    try {
      soundOn = sounds.supported && localStorage.getItem(SOUND_PREF_KEY) === '1';
    } catch {
      soundOn = false;
    }
    return () => sounds.dispose();
  });
  $effect(() => {
    sounds.setEnabled(soundOn);
  });
  function toggleSound(): void {
    soundOn = !soundOn;
    try {
      localStorage.setItem(SOUND_PREF_KEY, soundOn ? '1' : '0');
    } catch {
      /* private-mode storage denial — the toggle still works this session */
    }
  }

  function pct(v: number | undefined): string {
    return typeof v === 'number' ? `${Math.round(v * 100)}%` : '—';
  }
  // Fuel gauge (research 7's deferred third dial): the rolling-24h
  // pipeline budget window, straight off the operator's status payload.
  let fuel = $derived(fuelReading(millsStore.status?.budget?.pipeline));
  // Needle sweep for the pass-rate dial: 0 -> -90deg, 1 -> +90deg.
  let needleDeg = $derived(
    typeof metrics?.gate_pass_rate === 'number'
      ? Math.max(-90, Math.min(90, metrics.gate_pass_rate * 180 - 90))
      : -90,
  );

  /* ── simulation state shared with the RAF loop (plain object: the draw
     loop reads it every frame; $effects below push updates in) ── */
  const sim = {
    queue: [] as WeaveEvent[],
    picks: [] as StagePick[],
    activeCount: 0,
    warpTarget: 24,
    paused: false,
    stale: false,
    policySeed: 0,
    policyVersion: undefined as number | undefined,
    // The creel (concept 5): logical fleet agents as bobbins. A bobbin
    // spins only while its agent is actually active — and never on a
    // stale feed.
    weaversTotal: 0,
    weaversActive: 0,
    // Pattern shelf (concept 8): which book each active run was
    // stamped from, for truthful pick labels.
    bookByRun: new Map<string, string>(),
  };
  let seenRuns = new Set<string>();
  let stageMap = new Map<string, string>();

  $effect(() => {
    const { events, seen } = diffTerminalRuns(seenRuns, millsStore.pipelineHistory);
    if (events.length > 0) sim.queue.push(...events);
    seenRuns = seen;
  });
  $effect(() => {
    // Real shuttle picks: an active run advancing into a new stage.
    const { picks, stages } = diffStagePicks(stageMap, activeRuns);
    if (picks.length > 0) sim.picks.push(...picks);
    stageMap = stages;
  });
  $effect(() => {
    sim.activeCount = activeRuns.length;
    sim.paused = millsPaused;
    // A loom must never weave on a dead feed: stale poll or fetch error
    // halts the floor mid-pick instead of animating fiction.
    sim.stale = millsStore.isStale || millsStore.error != null;
  });
  $effect(() => {
    sim.policySeed = policyTapeSeed(millsStore.policy);
    sim.policyVersion = millsStore.policy?.version;
  });
  $effect(() => {
    // Collapse per-conversation agent rows to workspace roots (the same
    // honest count liveAgentCount uses); a root is active if any of its
    // conversations is.
    const roots = new Map<string, boolean>();
    for (const agent of fleetStore.liveAgents) {
      const root = rootAgentId(agent.agent_id);
      roots.set(root, (roots.get(root) ?? false) || agent.status === 'active');
    }
    sim.weaversTotal = roots.size;
    sim.weaversActive = [...roots.values()].filter(Boolean).length;
  });
  $effect(() => {
    sim.bookByRun = bookNamesByRun(patternsStore.patterns, millsStore.backlog, activeRuns);
  });

  /* ── cloth inspection: hover/click state for woven rows ── */
  interface RowHover {
    runID: string;
    backlogID: string;
    kind: 'plain' | 'bolt' | 'spark';
    x: number;
    y: number;
  }
  let hover = $state<RowHover | null>(null);
  $effect(() => {
    sim.warpTarget = warpCountFor(backlogActive, Math.floor((stageW || 900) / 22));
  });

  /* ── canvas loom ── */
  let canvas = $state<HTMLCanvasElement | null>(null);
  let stageEl = $state<HTMLDivElement | null>(null);
  let stageW = $state(0);

  $effect(() => {
    if (!canvas || !stageEl) return;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;
    const reduced = matchMedia('(prefers-reduced-motion: reduce)').matches;

    // Palette from the live theme tokens (channel triplets like "0, 200, 255").
    const css = getComputedStyle(document.documentElement);
    const C = {
      warp: css.getPropertyValue('--info-rgb').trim() || '0, 200, 255',
      weft: css.getPropertyValue('--accent-rgb').trim() || '255, 107, 53',
      bolt: css.getPropertyValue('--success-rgb').trim() || '34, 224, 118',
      spark: css.getPropertyValue('--warning-rgb').trim() || '255, 184, 48',
      fog: css.getPropertyValue('--fg-rgb').trim() || '212, 238, 244',
    };
    const hue = (rgb: string, a: number) => `rgba(${rgb},${a})`;

    let W = 0, H = 0, warpN = sim.warpTarget;
    let warpX: number[] = [], fellY = 0, clothBottom = 0, tapeW = 0;

    interface Row {
      kind: 'plain' | 'bolt' | 'spark';
      cells: boolean[];
      born: number;
      seed: string;
      runID?: string;
      backlogID?: string;
    }
    const rows: Row[] = [];
    const ROW_H = 7;
    let pass = 0, shuttleT = 0, creelT = 0;
    let tapeSeed = sim.policySeed, tapeVer = sim.policyVersion;
    let sparks: Array<{ x: number; y: number; vx: number; vy: number; life: number; hue: string }> = [];
    let labels: Array<{ text: string; x: number; y: number; life: number; max: number; hue: string }> = [];
    let raf = 0;

    function layout() {
      const dpr = Math.min(devicePixelRatio || 1, 2);
      W = stageEl!.clientWidth; H = stageEl!.clientHeight;
      canvas!.width = W * dpr; canvas!.height = H * dpr;
      ctx!.setTransform(dpr, 0, 0, dpr, 0, 0);
      tapeW = Math.max(40, Math.min(58, W * 0.05));
      warpN = Math.max(16, Math.min(sim.warpTarget, Math.floor((W - tapeW) / 20)));
      const left = tapeW + (W - tapeW) * 0.05, right = W * 0.97;
      warpX = Array.from({ length: warpN }, (_, i) => left + (right - left) * i / (warpN - 1));
      fellY = H * 0.56;
      clothBottom = H * 0.96;
      for (const r of rows) resampleRow(r);
    }

    function rowFor(kind: Row['kind'], seed: string, runID?: string, backlogID?: string): Row {
      return { kind, seed, cells: seededPattern(seed, warpN), born: performance.now(), runID, backlogID };
    }
    function resampleRow(r: Row) {
      if (r.cells.length === warpN) return;
      r.cells = seededPattern(r.seed, warpN);
    }

    function spawnLabel(text: string, x: number, y: number, h: string) {
      labels.push({ text, x, y, life: 0, max: 260, hue: h });
      if (labels.length > 5) labels.shift();
    }

    function completePass(now: number) {
      pass++;
      // A pass lays cloth only for a real event: a terminal run (bolt or
      // spark) or a shuttle pick (an active run advancing a stage). With no
      // event pending, the reed beats air — motion without fabrication.
      let laid: Row | null = null;
      const ev = sim.queue.shift();
      if (ev) {
        laid = rowFor(ev.kind, ev.runID, ev.runID, ev.backlogID);
        const short = (ev.backlogID || ev.runID).slice(0, 10);
        spawnLabel(
          ev.kind === 'bolt' ? `bolt ${short} · merged on green` : `spark ${short} · escalated`,
          W * (0.45 + Math.random() * 0.25), fellY - 56 - Math.random() * 30,
          ev.kind === 'bolt' ? C.bolt : C.spark,
        );
      } else {
        const pick = sim.picks.shift();
        if (pick) {
          laid = rowFor('plain', `${pick.runID}/${pick.stage}`, pick.runID, pick.backlogID);
          const short = (pick.backlogID || pick.runID).slice(0, 10);
          // A pattern-stamped run names its book — the card chain being read.
          const book = sim.bookByRun.get(pick.runID);
          spawnLabel(
            `shuttle ${short} · ${stageLabel(pick.stage)}${book ? ` · book «${book}»` : ''}`,
            W * (0.35 + Math.random() * 0.35), fellY - 44 - Math.random() * 50, C.weft,
          );
        }
      }
      if (laid) {
        rows.unshift(laid);
        const maxRows = Math.floor((clothBottom - fellY) / ROW_H);
        while (rows.length > maxRows) rows.pop();
        // Audio mirrors the cloth: only a laid row makes a sound.
        if (laid.kind === 'bolt') sounds.chime();
        else if (laid.kind === 'spark') sounds.klaxon();
        else sounds.clack();
      }
      const sx = pass % 2 ? warpX[0] : warpX[warpN - 1];
      for (let i = 0; i < 6; i++) {
        sparks.push({
          x: sx + (Math.random() - 0.5) * 26, y: fellY + (Math.random() - 0.5) * 6,
          vx: (Math.random() - 0.5) * 1.5, vy: -Math.random() * 1.3 - 0.3,
          life: 1, hue: laid?.kind === 'spark' ? C.spark : C.weft,
        });
      }
      void now;
    }

    function draw(now: number) {
      ctx!.clearRect(0, 0, W, H);
      const t = now / 1000;

      // punch-card tape: the jacquard program feeding in. Hole pattern is
      // a pure function of the live policy — a version bump or kill-switch
      // flip visibly splices in a freshly punched chain.
      if (sim.policySeed !== tapeSeed) {
        // Suppress the label on the first policy fetch (tape was seeded
        // from "no policy yet", not from a real prior program).
        if (tapeVer !== undefined) {
          spawnLabel(`policy spliced · v${sim.policyVersion ?? '?'}`, tapeW + 16, H * 0.22, C.fog);
        }
        tapeSeed = sim.policySeed;
        tapeVer = sim.policyVersion;
      }
      const holeR = 2.2, cols = 4, tapeX = 7;
      const colGap = (tapeW - 14) / (cols - 1);
      const scroll = sim.paused || sim.stale ? 0 : (t * 20) % 24;
      ctx!.fillStyle = hue(C.warp, 0.045);
      ctx!.fillRect(0, 0, tapeW, H);
      for (let y = -24 + scroll; y < H + 24; y += 24) {
        const rowSeed = Math.floor((y - scroll) / 24 + 1e5);
        for (let c = 0; c < cols; c++) {
          const on = tapeHole(tapeSeed, rowSeed, c);
          ctx!.beginPath();
          ctx!.arc(tapeX + c * colGap + holeR, y, holeR, 0, Math.PI * 2);
          if (on) { ctx!.fillStyle = hue(C.warp, 0.55); ctx!.fill(); }
          else { ctx!.strokeStyle = hue(C.warp, 0.14); ctx!.stroke(); }
        }
      }

      // the creel (concept 5): each bobbin is one logical fleet agent —
      // the "weavers on the floor" gauge, embodied. An active agent's
      // bobbin spins (orbiting pirn dot); idle bobbins sit dim; a stale
      // feed freezes the lot mid-turn like everything else on the floor.
      if (sim.weaversTotal > 0) {
        const maxBobbins = Math.min(sim.weaversTotal, 8);
        const bobY = 26, bobX0 = tapeW + 26, bobGap = 34;
        if (!sim.stale && !reduced) creelT = t;
        ctx!.font = '8px ' + (css.getPropertyValue('--font-mono') || 'monospace');
        /* 0.55, not 0.35: at 8px mono the creel caption was effectively
           invisible against the stage wash, leaving the bobbin column an
           unlabeled dot grid. */
        ctx!.fillStyle = hue(C.fog, 0.55);
        ctx!.fillText('creel · weavers', bobX0 - 9, bobY - 13);
        for (let i = 0; i < maxBobbins; i++) {
          const x = bobX0 + i * bobGap;
          const active = i < sim.weaversActive;
          const dim = active ? 1 : 0.4;
          // spindle + flanges
          ctx!.strokeStyle = hue(C.fog, 0.3 * dim);
          ctx!.lineWidth = 1;
          ctx!.beginPath();
          ctx!.moveTo(x - 12, bobY); ctx!.lineTo(x + 12, bobY);
          ctx!.moveTo(x - 10, bobY - 6); ctx!.lineTo(x - 10, bobY + 6);
          ctx!.moveTo(x + 10, bobY - 6); ctx!.lineTo(x + 10, bobY + 6);
          ctx!.stroke();
          // wound thread
          ctx!.beginPath();
          ctx!.ellipse(x, bobY, 8, 4.5, 0, 0, Math.PI * 2);
          ctx!.fillStyle = hue(C.weft, 0.28 * dim);
          ctx!.fill();
          // the pirn dot: orbit = the bobbin is winding
          if (active) {
            const a = creelT * 3.4 + i * 1.7;
            ctx!.beginPath();
            ctx!.arc(x + Math.cos(a) * 8, bobY + Math.sin(a) * 4.5, 1.4, 0, Math.PI * 2);
            ctx!.fillStyle = hue(C.weft, 0.9);
            ctx!.fill();
          }
        }
        if (sim.weaversTotal > maxBobbins) {
          ctx!.fillStyle = hue(C.fog, 0.5);
          ctx!.fillText(`+${sim.weaversTotal - maxBobbins}`, bobX0 + maxBobbins * bobGap - 14, bobY + 3);
        }
      }

      // shuttle: weaves while runs are active OR events are queued; parks
      // at the selvage when the floor idles; halts mid-pick on a dead feed.
      const working = !sim.paused && !sim.stale &&
        (sim.activeCount > 0 || sim.queue.length > 0 || sim.picks.length > 0);
      if (working && !reduced) {
        shuttleT += 0.006 + Math.min(sim.activeCount, 6) * 0.0015;
        if (shuttleT >= 1) { shuttleT = 0; completePass(now); }
      }
      const dir = pass % 2 === 0 ? 1 : -1;
      const ease = shuttleT < 0.5 ? 2 * shuttleT * shuttleT : 1 - Math.pow(-2 * shuttleT + 2, 2) / 2;
      const sx0 = dir === 1 ? warpX[0] : warpX[warpN - 1];
      const sx1 = dir === 1 ? warpX[warpN - 1] : warpX[0];
      // Stale feed freezes the shuttle wherever it was (halted mid-pick);
      // a genuinely idle floor racks it at the selvage.
      const shuttleX = working || sim.stale ? sx0 + (sx1 - sx0) * ease : warpX[0];

      // warp threads with lifted shed
      const shedAmp = working ? 14 : 4;
      for (let i = 0; i < warpN; i++) {
        const x = warpX[i];
        const lift = ((i + pass) % 2 === 0 ? -1 : 1) * shedAmp;
        const sway = Math.sin(t * 1.2 + i * 0.55) * 1.1;
        const bright = 0.12 + 0.09 * Math.abs(Math.sin(t * 0.8 + i * 0.9));
        ctx!.beginPath();
        ctx!.moveTo(x + sway, 0);
        ctx!.bezierCurveTo(x + sway, fellY - 130, x, fellY - 80 + lift, x, fellY - 24 + lift * 0.6);
        ctx!.quadraticCurveTo(x, fellY - 8, x, fellY);
        ctx!.strokeStyle = hue(C.warp, bright);
        ctx!.lineWidth = 1;
        ctx!.stroke();
      }

      // weft in flight + shuttle body
      const wy = fellY - 12;
      if (working) {
        ctx!.beginPath();
        ctx!.moveTo(sx0, wy); ctx!.lineTo(shuttleX, wy);
        ctx!.strokeStyle = hue(C.weft, 0.75); ctx!.lineWidth = 1.5;
        ctx!.shadowColor = hue(C.weft, 0.8); ctx!.shadowBlur = 7;
        ctx!.stroke(); ctx!.shadowBlur = 0;
      }
      ctx!.save();
      ctx!.translate(shuttleX, wy);
      ctx!.scale(dir, 1);
      const grad = ctx!.createLinearGradient(-15, 0, 15, 0);
      const idleDim = working ? 1 : 0.35 + 0.15 * Math.sin(t * 1.6);
      grad.addColorStop(0, hue(C.weft, 0.12 * idleDim));
      grad.addColorStop(0.65, hue(C.weft, idleDim));
      grad.addColorStop(1, hue(C.fog, idleDim));
      ctx!.beginPath();
      ctx!.moveTo(-15, 0); ctx!.quadraticCurveTo(-4, -5, 11, -2.4);
      ctx!.quadraticCurveTo(16, 0, 11, 2.4); ctx!.quadraticCurveTo(-4, 5, -15, 0);
      ctx!.fillStyle = grad;
      ctx!.shadowColor = hue(C.weft, 0.9 * idleDim); ctx!.shadowBlur = working ? 16 : 8;
      ctx!.fill();
      ctx!.restore();

      // fell line
      ctx!.fillStyle = hue(C.fog, 0.09);
      ctx!.fillRect(warpX[0] - 6, fellY - 1, warpX[warpN - 1] - warpX[0] + 12, 2);

      // woven cloth
      for (let r = 0; r < rows.length; r++) {
        const row = rows[r];
        const y = fellY + 3 + r * ROW_H;
        if (y > clothBottom) break;
        const fade = 1 - (y - fellY) / (clothBottom - fellY);
        const fresh = Math.max(0, 1 - (now - row.born) / 900);
        const base = row.kind === 'bolt' ? C.bolt : row.kind === 'spark' ? C.spark : C.warp;
        const glow = row.kind === 'plain' ? 0.15 : 0.34;
        for (let i = 0; i < warpN; i++) {
          const over = Number(row.cells[i]) ^ (r % 2);
          const a = (over ? glow + 0.13 : glow * 0.45) * (0.25 + 0.75 * fade) + fresh * 0.25;
          ctx!.fillStyle = hue(base, Math.min(a, 0.85));
          ctx!.fillRect(warpX[i] - 7, y, 14, ROW_H - 1.5);
        }
      }

      // sparks
      sparks = sparks.filter((s) => s.life > 0);
      for (const s of sparks) {
        s.x += s.vx; s.y += s.vy; s.vy += 0.05; s.life -= 0.025;
        ctx!.beginPath(); ctx!.arc(s.x, s.y, 1.3 * s.life + 0.4, 0, Math.PI * 2);
        ctx!.fillStyle = hue(s.hue, s.life);
        ctx!.fill();
      }

      // floating telemetry labels
      ctx!.font = '10px ' + (css.getPropertyValue('--font-mono') || 'monospace');
      labels = labels.filter((l) => l.life < l.max);
      for (const l of labels) {
        l.life++;
        const k = l.life / l.max;
        const a = k < 0.12 ? k / 0.12 : k > 0.75 ? (1 - k) / 0.25 : 1;
        const y = l.y - k * 12;
        ctx!.fillStyle = hue(l.hue, a * 0.9);
        ctx!.fillText('▸ ' + l.text, l.x, y);
      }

      // idle / halted caption
      if (!working) {
        ctx!.font = '10px ' + (css.getPropertyValue('--font-mono') || 'monospace');
        ctx!.fillStyle = hue(sim.stale ? C.spark : C.fog, 0.4 + 0.1 * Math.sin(t * 1.6));
        ctx!.fillText(
          sim.stale ? '⚠ feed stale — loom halted mid-pick'
            : sim.paused ? '◼ mills paused — jacquard halted'
            : '◌ floor idle — shuttle racked',
          warpX[0], wy - 14,
        );
      }

      if (!reduced) raf = requestAnimationFrame(draw);
    }

    /* ── cloth inspection: hit-test woven rows for hover/click ── */
    function hitRow(evt: MouseEvent): Row | null {
      const rect = canvas!.getBoundingClientRect();
      const x = evt.clientX - rect.left, y = evt.clientY - rect.top;
      if (y < fellY + 3 || y > clothBottom) return null;
      if (x < warpX[0] - 7 || x > warpX[warpN - 1] + 7) return null;
      return rows[Math.floor((y - fellY - 3) / ROW_H)] ?? null;
    }
    function onMove(evt: MouseEvent) {
      const row = hitRow(evt);
      if (row?.runID) {
        const rect = stageEl!.getBoundingClientRect();
        hover = {
          runID: row.runID,
          backlogID: row.backlogID ?? '',
          kind: row.kind,
          x: evt.clientX - rect.left,
          y: evt.clientY - rect.top,
        };
        canvas!.style.cursor = 'pointer';
      } else {
        hover = null;
        canvas!.style.cursor = '';
      }
    }
    function onLeave() { hover = null; canvas!.style.cursor = ''; }
    function onClick(evt: MouseEvent) {
      const row = hitRow(evt);
      if (row?.runID) millsStore.openRunDetail(row.runID);
    }
    canvas.addEventListener('mousemove', onMove);
    canvas.addEventListener('mouseleave', onLeave);
    canvas.addEventListener('click', onClick);

    layout();
    // Pre-weave recent history so the first frame shows fabric, not void.
    // Seeded by run ID, so the same history always weaves the same cloth
    // — and every preloaded row stays inspectable.
    const preload = sim.queue.splice(0, sim.queue.length);
    for (const ev of preload.slice(-24)) rows.unshift(rowFor(ev.kind, ev.runID, ev.runID, ev.backlogID));
    while (rows.length < 14) rows.push(rowFor('plain', `warmup-${rows.length}`));

    const ro = new ResizeObserver(() => { stageW = stageEl!.clientWidth; layout(); });
    ro.observe(stageEl);
    raf = requestAnimationFrame(draw);
    return () => {
      cancelAnimationFrame(raf);
      ro.disconnect();
      canvas?.removeEventListener('mousemove', onMove);
      canvas?.removeEventListener('mouseleave', onLeave);
      canvas?.removeEventListener('click', onClick);
    };
  });
</script>

<div class="panel factory-panel">
  <PanelHeader title="Factory" icon={'❖'} count={activeRuns.length}>
    {#snippet stats()}
      {#if millsStore.status}
        <Badge text={millsPaused ? 'paused' : 'weaving'} variant={millsPaused ? 'warning' : 'success'} />
      {/if}
      <span class="text-muted text-xs">lights-out software manufacture, live</span>
    {/snippet}
    {#snippet actions()}
      <!-- Text-only actions: the HUD's chrome is monochrome glyphs and mono
           type; color emoji read as foreign objects on the toolbar. -->
      <button
        type="button"
        class="btn btn-sm"
        onclick={() => (showArchive = true)}
        title="The week's cloth as one exportable strip — every row a real run"
      >bolt archive</button>
      <button
        type="button"
        class="btn btn-sm"
        onclick={() => (showShift = true)}
        title="The last 24 hours told straight — bolts, sparks, stamps, spend; copy as markdown for the standup"
      >shift report</button>
      <button
        type="button"
        class="btn btn-sm"
        onclick={() => router.navigateDetail('andon')}
        title="Fullscreen glance board for the office TV — bookmarkable at #mills/factory/andon"
      >⛶ andon</button>
    {/snippet}
  </PanelHeader>

  {#if millsStore.error}
    <ErrorBanner prefix="Factory feed failed" message={millsStore.error} />
  {/if}

  {#if millsStore.disabled}
    <EmptyState
      icon={'❖'}
      heading="The factory floor is dark"
      description="Mills operator is not configured on this daemon. Set the operator URL to light the looms."
    />
  {:else}
    <MillEfficiencyStrip />

    <div class="loom-stage" bind:this={stageEl}>
      <canvas bind:this={canvas} aria-label="Live loom: warp threads are backlog items, the shuttle is active pipeline runs, green rows are merged runs, amber rows are escalations. Click a woven row to inspect that run's stages and gates."></canvas>
      <div class="stage-legend">
        <span><i class="sw sw-warp"></i>warp · backlog</span>
        <span><i class="sw sw-weft"></i>shuttle · active runs</span>
        <span><i class="sw sw-bolt"></i>bolt · merged</span>
        <span><i class="sw sw-spark"></i>spark · escalated</span>
        <span class="legend-hint">click cloth to inspect</span>
      </div>
      {#if sounds.supported}
        <button
          type="button"
          class="sound-toggle"
          aria-pressed={soundOn}
          onclick={toggleSound}
          title={soundOn
            ? 'Loom sounds on — clack per pick, chime per bolt, low horn per spark'
            : 'Loom sounds off — enable clack/chime/horn for real events only'}
        >sfx {soundOn ? 'on' : 'off'}</button>
      {/if}
      {#if hover}
        <div class="row-tip" style="left: {Math.min(hover.x + 12, stageW - 200)}px; top: {hover.y - 8}px">
          <span class="tip-kind tip-{hover.kind}">
            {hover.kind === 'bolt' ? 'bolt · merged on green'
              : hover.kind === 'spark' ? 'spark · escalated'
              : 'pick · woven in progress'}
          </span>
          <span class="tip-id">{hover.backlogID || hover.runID}</span>
          <span class="tip-hint">click for stages & gates</span>
        </div>
      {/if}
    </div>

    <!-- The machine's control rail: live-now readings docked to the loom
         they describe, one hairline-divided strip instead of a second card
         grid competing with the seven-day economics above. -->
    <div class="instrument-rail" aria-label="Live floor instruments">
      <div class="inst" title="Active pipeline runs — the shuttles crossing the cloth above">
        <span class="inst-label">shuttles</span>
        <span class="inst-value tone-accent"><RollingNumber value={activeRuns.length} /></span>
      </div>
      <div class="inst" title="Runs merged in the KPI window">
        <span class="inst-label">bolts · 24h</span>
        <span class="inst-value tone-success"><RollingNumber value={metrics?.pipeline_merged_runs} /></span>
      </div>
      <div class="inst" title="Runs escalated in the KPI window">
        <span class="inst-label">sparks · 24h</span>
        <span class="inst-value tone-warning"><RollingNumber value={metrics?.pipeline_escalated_runs} /></span>
      </div>
      <div class="inst" title="Backlog strung on the beam: queued, ready, running, escalated, paused">
        <span class="inst-label">on the beam</span>
        <span class="inst-value tone-info"><RollingNumber value={backlogActive} /></span>
      </div>
      <div class="inst" title="Gate pass rate across the KPI window">
        <span class="inst-label">inspection</span>
        <span class="inst-value tone-info dial-row">
          <svg class="dial" viewBox="0 0 40 22" aria-hidden="true">
            <path d="M 4 20 A 16 16 0 0 1 36 20" fill="none" stroke="var(--border)" stroke-width="2.5" stroke-linecap="round" />
            <g class="needle" style="--deg: {needleDeg}deg">
              <line x1="20" y1="20" x2="20" y2="7" stroke="var(--info)" stroke-width="2" stroke-linecap="round" />
            </g>
            <circle cx="20" cy="20" r="2" fill="var(--fg-muted)" />
          </svg>
          {pct(metrics?.gate_pass_rate)}
        </span>
      </div>
      <!-- liveAgentCount collapses per-conversation rows to workspace
           roots — the honest count, and the same one the creel draws. -->
      <div class="inst" title="Logical fleet agents on the floor — the creel's bobbins">
        <span class="inst-label">weavers</span>
        <span class="inst-value tone-info">{fleetStore.liveAgentCount}</span>
      </div>
      <div class="inst inst-fuel fuel-{fuel.tone}" title="Rolling 24-hour pipeline budget: spent / ceiling">
        <span class="inst-label">fuel · 24h</span>
        <span class="inst-value fuel-row">
          {#if fuel.frac !== null}
            <span class="tank" role="img" aria-label="{Math.round(fuel.frac * 100)}% of the 24-hour pipeline budget remaining">
              <span class="tank-fill" style="width: {fuel.frac * 100}%"></span>
            </span>
          {/if}
          <span class="fuel-label">{fuel.label}</span>
        </span>
      </div>
    </div>

    <!-- News above ambience: the floor log sits directly under the machine
         it narrates; the near-static pattern library closes the page. -->
    <div class="floor" aria-label="Factory floor log">
      <span class="floor-tag">departures</span>
      <DepartureBoard rows={boardRows} />
    </div>

    <PatternShelf {books} />
  {/if}
</div>

<PipelineRunDetail />

{#if showArchive}
  <BoltArchive onclose={() => (showArchive = false)} />
{/if}

{#if showShift}
  <ShiftReport onclose={() => (showShift = false)} />
{/if}

{#if showAndon}
  <AndonMode onclose={() => router.navigateDetail(null)} />
{/if}

<style>
  .factory-panel { display: flex; flex-direction: column; overflow: hidden; }

  .loom-stage {
    position: relative;
    flex: 1;
    min-height: 300px;
    background:
      linear-gradient(180deg, color-mix(in srgb, var(--info) 3%, var(--bg-primary)), var(--bg-primary));
    border: 1px solid var(--border);
    /* Square bottom corners: the instrument rail docks flush beneath, so
       loom + rail read as one machine. */
    border-radius: var(--radius-md) var(--radius-md) 0 0;
    overflow: hidden;
  }
  .loom-stage canvas { position: absolute; inset: 0; width: 100%; height: 100%; display: block; }

  .stage-legend {
    position: absolute;
    right: var(--space-3);
    top: var(--space-2);
    display: flex;
    gap: var(--space-3);
    font-size: var(--text-2xs);
    color: var(--fg-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    pointer-events: none;
  }
  .sw { display: inline-block; width: 7px; height: 7px; border-radius: 1px; margin-right: 5px; }
  .legend-hint { color: var(--fg-dim); font-style: italic; text-transform: none; letter-spacing: normal; }
  /* At phone widths the absolute legend wraps across the creel and shuttle
     lanes — worse than no legend. The canvas aria-label and the row tooltip
     still carry the mapping. */
  @media (max-width: 640px) {
    .stage-legend { display: none; }
  }

  .row-tip {
    position: absolute;
    display: flex;
    flex-direction: column;
    gap: 2px;
    max-width: 220px;
    padding: var(--space-2) var(--space-3);
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    pointer-events: none;
    z-index: 2;
  }
  .tip-kind { text-transform: uppercase; letter-spacing: var(--tracking-wide); }
  .tip-bolt { color: var(--success); }
  .tip-spark { color: var(--warning); }
  .tip-plain { color: var(--info); }
  .tip-id { color: var(--fg-primary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .tip-hint { color: var(--fg-muted); }
  .sound-toggle {
    position: absolute;
    right: var(--space-2);
    bottom: var(--space-2);
    background: none;
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    padding: 2px 6px;
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    color: var(--fg-secondary);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    cursor: pointer;
    opacity: 0.55;
    z-index: 2;
  }
  .sound-toggle:hover,
  .sound-toggle[aria-pressed='true'] { opacity: 1; }

  .sw-warp { background: var(--info); box-shadow: var(--glow-shadow-md) var(--glow-info); }
  .sw-weft { background: var(--accent); box-shadow: var(--glow-shadow-md) var(--glow-accent); }
  .sw-bolt { background: var(--success); box-shadow: var(--glow-shadow-md) var(--glow-success); }
  .sw-spark { background: var(--warning); box-shadow: var(--glow-shadow-md) var(--glow-warning); }

  /* ── instrument rail: the loom's control strip ── */
  .instrument-rail {
    display: flex;
    flex-wrap: wrap;
    align-items: stretch;
    border: 1px solid var(--border);
    border-top: 0;
    border-radius: 0 0 var(--radius-md) var(--radius-md);
    background: var(--bg-secondary);
    flex-shrink: 0;
  }
  .inst {
    display: flex;
    flex-direction: column;
    justify-content: center;
    gap: 2px;
    flex: 1 1 0;
    min-width: 92px;
    padding: var(--space-2) var(--space-3);
    border-right: 1px solid var(--border-subtle);
  }
  .inst:last-child { border-right: 0; }
  .inst-label {
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-dim);
    white-space: nowrap;
  }
  .inst-value {
    font-size: var(--text-lg);
    font-weight: 700;
    font-family: var(--font-mono);
    font-variant-numeric: tabular-nums;
    color: var(--fg-primary);
    line-height: 1.1;
  }
  /* Fuel wants the most room (tank + spent/ceiling); let it take it. */
  .inst-fuel { flex: 1.6 1 0; min-width: 168px; }
  .tone-info { color: var(--info); text-shadow: var(--glow-shadow-lg) var(--glow-info); }
  .tone-accent { color: var(--accent); text-shadow: var(--glow-shadow-lg) var(--glow-accent); }
  .tone-success { color: var(--success); text-shadow: var(--glow-shadow-lg) var(--glow-success); }
  .tone-warning { color: var(--warning); text-shadow: var(--glow-shadow-lg) var(--glow-warning); }
  .fuel-row { display: flex; align-items: center; gap: var(--space-2); white-space: nowrap; }
  .fuel-label { font-size: var(--text-lg); font-family: var(--font-mono); }
  .tank {
    display: inline-block;
    width: 56px;
    height: 10px;
    flex-shrink: 0;
    border: 1px solid var(--border);
    border-radius: var(--radius-xs);
    overflow: hidden;
    background: var(--bg-primary);
  }
  .tank-fill {
    display: block;
    height: 100%;
    background: var(--fuel-tone, var(--info));
    transition: width 0.8s cubic-bezier(0.22, 1, 0.36, 1);
  }
  .fuel-ok { --fuel-tone: var(--success); color: var(--success); }
  .fuel-wr { --fuel-tone: var(--warning); color: var(--warning); }
  .fuel-er { --fuel-tone: var(--error); color: var(--error); }
  .fuel-cy { color: var(--fg-secondary); }
  .dial-row { display: flex; align-items: center; gap: var(--space-2); }
  .dial { width: 38px; height: 22px; overflow: visible; }
  .needle {
    transform-origin: 20px 20px;
    transform: rotate(var(--deg));
    transition: transform 0.9s cubic-bezier(0.34, 1.56, 0.64, 1);
  }
  @media (prefers-reduced-motion: reduce) {
    .tank-fill,
    .needle { transition: none; }
  }

  .floor {
    display: flex;
    align-items: stretch;
    margin-top: var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
    overflow: hidden;
    flex-shrink: 0;
  }
  .floor-tag {
    display: flex;
    align-items: center;
    padding: 0 var(--space-3);
    font-size: var(--text-2xs);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
    color: var(--accent);
    border-right: 1px solid var(--border);
    background: var(--bg-primary);
    white-space: nowrap;
  }
</style>
