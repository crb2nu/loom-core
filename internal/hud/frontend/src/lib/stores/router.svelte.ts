// Hash-based SPA router for the HUD.
// Supports grouped views with legacy hash compatibility.
// Top-level labels are intentionally operator-oriented while IDs stay stable.

import { embedConfig } from './embedConfig.svelte.ts';

export interface RouteState {
  view: string;
  subView: string;
  detail: string | null;
}

// ---- View definitions ----

/**
 * A sub-view (second-level tab) inside a grouped view.
 *
 * `key` is the single-letter shortcut App.svelte matches on. It is matched by
 * VALUE, never by position — see resolveSubViewKey. Keys must be unique within
 * a view and must avoid the app-wide globals ('o' = Overview, 'r' = refresh),
 * both of which are asserted by router.svelte.test.ts.
 *
 * `group` optionally partitions the sub-tab bar into labelled sections
 * (ViewShell renders a separator + caption between groups). Sub-views carrying
 * the same group must be contiguous in the array; sub-views without a group
 * render ungrouped, exactly as before.
 */
export interface SubViewDef {
  id: string;
  label: string;
  key: string;
  group?: string;
}

export interface ViewDef {
  id: string;
  label: string;
  icon: string;
  key: string;
  subViews: SubViewDef[];
  default: string;
}

/**
 * Single-letter shortcuts reserved app-wide by App.svelte's keydown handler:
 * 'o' jumps to Overview and 'r' refreshes. A sub-view may not declare either —
 * the global handler runs first, so such a sub-view would be unreachable by
 * keyboard while still advertising a <kbd> hint that does nothing.
 */
export const reservedSubViewKeys = ['o', 'r'] as const;

/**
 * resolveSubViewKey maps a pressed key to a sub-view of `view` by matching the
 * DECLARED `key` field. The previous implementation indexed `subViews` by
 * (charCode - 'a'), which only coincidentally worked for views whose keys ran
 * a, b, c… in order and sent every out-of-order view (Mills) to the wrong
 * panel. Returns null when no sub-view claims the key.
 */
export function resolveSubViewKey(view: ViewDef | undefined, key: string): SubViewDef | null {
  if (!view || key.length !== 1) return null;
  return view.subViews.find((sv) => sv.key === key) ?? null;
}

export const views: ViewDef[] = [
  {
    // Operator Deck — the unified operator surface and the HUD's landing
    // view. One page that watches everything in flight (Mills runs, tracked
    // MRs/CI, live agent sessions) beside the work queue (plans/tasks) with
    // dispatch controls, so day-to-day operation doesn't require hopping
    // between the seven domain views. A single sub-view: ViewShell hides the
    // one-tab bar, so the deck reads as a standalone page while reusing the
    // grouped-view routing (no router special-cases like Overview's).
    id: 'operator',
    label: 'Deck',
    icon: '⎈', // HELM SYMBOL — the operator at the wheel
    key: '0',
    default: 'deck',
    subViews: [{ id: 'deck', label: 'Deck', key: 'a' }],
  },
  {
    id: 'agents',
    label: 'Operations',
    icon: '\u25C8',
    key: '1',
    default: 'fleet',
    subViews: [
      { id: 'fleet',     label: 'Fleet',     key: 'a' },
      // Unified cross-vendor session browser: claude + codex transcripts
      // from this host and federated Macs, repo-grouped and joined against
      // live fleet agents. 's' for Sessions.
      { id: 'sessions',  label: 'Sessions',  key: 's' },
      { id: 'dispatch',  label: 'Dispatch',  key: 'b' },
      { id: 'presence',  label: 'Presence',  key: 'c' },
      { id: 'topology',  label: 'Topology',  key: 'd' },
      { id: 'lifecycle', label: 'Lifecycle',  key: 'e' },
      // Classified branch→MR registry + the shepherd's auto-action audit log
      // (GET /api/mrwatch/{summary,actions}). 'm' for MRs; 'f' would collide
      // with nothing here but reads as "fleet", which is the tab next door.
      { id: 'mrwatch',   label: 'MRs',       key: 'm' },
      // Pipeline alert engine + auto-fix queue (GET /api/alerts{,/rules},
      // POST /api/alerts/{id}/ack + /api/alerts/diagnose, and the five
      // /api/autofix routes). 'l' for aLerts: 'a' is Fleet and 'r' is the
      // app-wide refresh global.
      { id: 'alerts',    label: 'Alerts',    key: 'l' },
    ],
  },
  {
    id: 'infra',
    label: 'Infrastructure',
    icon: '\u2665',
    key: '2',
    default: 'servers',
    subViews: [
      { id: 'servers', label: 'Servers', key: 'a' },
      { id: 'catalog', label: 'Catalog', key: 'b' },
      { id: 'weaver', label: 'Weaver', key: 'c' },
    ],
  },
  {
    id: 'tasks',
    label: 'Work',
    icon: '\u2611',
    key: '3',
    default: 'tasks',
    subViews: [
      { id: 'tasks',     label: 'Tasks',     key: 'a' },
      { id: 'workflows', label: 'Workflows', key: 'b' },
      { id: 'plans',     label: 'Plans',     key: 'c' },
      // Projects was its own 8th top-level tab until it was folded in here.
      // It reads only /api/plans + /api/sessions — a per-project lens over
      // Plans, not an independent data domain — so it belongs beside them.
      // The legacy `#projects` hash still resolves (see legacyRedirects).
      { id: 'projects',  label: 'Projects',  key: 'd' },
    ],
  },
  {
    id: 'knowledge',
    label: 'Context',
    icon: '\u29BE',
    key: '4',
    default: 'feed',
    subViews: [
      { id: 'feed',      label: 'Feed',      key: 'a' },
      { id: 'memory',    label: 'Memory',    key: 'b' },
      { id: 'graph',     label: 'Graph',     key: 'c' },
      { id: 'reasoning', label: 'Reasoning', key: 'd' },
      // Per-agent context-window budget + compaction control, backed by
      // monitor.ContextHealthMonitor. The id is `context-health`, not `health`:
      // sub-view ids are globally unique and "health" is far too generic to
      // claim across the whole nav.
      { id: 'context-health', label: 'Health', key: 'h' },
    ],
  },
  {
    id: 'activity',
    label: 'Activity',
    icon: '\u2261',
    key: '5',
    default: 'timeline',
    subViews: [
      { id: 'timeline', label: 'Timeline', key: 'a' },
      { id: 'stream',   label: 'Stream',   key: 'b' },
      { id: 'traces',   label: 'Traces',   key: 'c' },
    ],
  },
  {
    id: 'sandbox',
    label: 'Labs',
    icon: '\u2B22',
    key: '6',
    default: 'sandbox',
    subViews: [
      { id: 'sandbox', label: 'Sandbox', key: 'a' },
      { id: 'spawn',   label: 'Spawn',   key: 'b' },
    ],
  },
  {
    id: 'mills',
    label: 'Mills',
    icon: '❖', // BLACK DIAMOND MINUS WHITE X — autonomous-loop control plane
    key: '7',
    default: 'mills-overview',
    // Sixteen sub-tabs is too many to scan as one flat row, so they are split
    // into two contiguous groups that ViewShell renders with a separator:
    //
    //   Mill floor  — the loom motion made functional: where work physically
    //                 moves (overview → factory → warps → shuttles → sparks →
    //                 bolts).
    //   Governance  — the surfaces that decide, judge, and audit that motion.
    //
    // Keys are matched by VALUE (resolveSubViewKey), and none may be 'o' or
    // 'r' — those are app-wide globals (Overview / refresh). Warps and Bolts
    // used to declare exactly those two and were therefore keyboard-
    // unreachable; they now hold the mnemonic 'w' and 'b', freed by moving
    // the Runs tab (formerly mislabelled "Workflows") to 'n'.
    subViews: [
      // The sub-view id is `mills-overview`, not `overview`: the bare id is
      // the standalone top-level Overview ("Now") view, and one id meaning
      // two different panels made every id-keyed lookup ambiguous. Precedent:
      // `mills-workflows`. `#mills/overview` still resolves (legacyRedirects).
      { id: 'mills-overview', label: 'Overview',  key: 'a', group: 'Mill floor' },
      { id: 'factory',        label: 'Factory',   key: 'k', group: 'Mill floor' },
      { id: 'warps',          label: 'Warps',     key: 'w', group: 'Mill floor' },
      { id: 'shuttles',       label: 'Shuttles',  key: 's', group: 'Mill floor' },
      { id: 'sparks',         label: 'Sparks',    key: 'p', group: 'Mill floor' },
      { id: 'bolts',          label: 'Bolts',     key: 'b', group: 'Mill floor' },

      // Mill staff — the three judgment lanes of the factory
      // (docs/FACTORY_MODEL.md §Mill staff): the Drawing Office drafts what
      // to weave (council), Drawing-in binds work to squads (the drawers-in),
      // and the Alley is the overlookers' walk (overseers). Code, API paths,
      // and event actors keep council/squads/overseer — the theme lives in
      // labels and docs only, so ids and hotkeys are unchanged.
      // `staff` leads the group: the three departments side by side plus the
      // staff evidence reports (promotion, judge calibration, regressions,
      // config outcomes, signature candidates). The three tabs after it stay
      // the per-department detail surfaces.
      { id: 'staff',      label: 'Mill Staff',     key: 'm', group: 'Mill staff' },
      { id: 'council',    label: 'Drawing Office', key: 'd', group: 'Mill staff' },
      { id: 'squads',     label: 'Drawing-in',     key: 'f', group: 'Mill staff' },
      { id: 'overseers',  label: 'The Alley',      key: 'v', group: 'Mill staff' },

      { id: 'eval',       label: 'Eval',       key: 'e', group: 'Governance' },
      { id: 'audit',      label: 'Audit',      key: 'g', group: 'Governance' },
      { id: 'policy',     label: 'Policy',     key: 'i', group: 'Governance' },
      { id: 'patterns',   label: 'Patterns',   key: 'j', group: 'Governance' },
      { id: 'cross-repo', label: 'Cross-Repo', key: 'h', group: 'Governance' },
      { id: 'telemetry',  label: 'Telemetry',  key: 't', group: 'Governance' },
      // "Runs" (Mills workflow runs), not "Workflows" — that label collided
      // with Work ▸ Workflows, which is a different surface entirely.
      { id: 'mills-workflows', label: 'Runs',  key: 'n', group: 'Governance' },
    ],
  },
];

// Overview is standalone (no sub-views)
export const overviewId = 'overview';

// ---- Legacy hash redirect map (old flat panel id -> new view/subView) ----

const legacyRedirects: Record<string, { view: string; subView: string }> = {};
for (const v of views) {
  for (const sv of v.subViews) {
    legacyRedirects[sv.id] = { view: v.id, subView: sv.id };
  }
}
// Only REAL redirects belong below — an id whose target differs from what the
// loop above already generated. (Fifteen self-referential aliases such as
// `fleet -> agents/fleet` used to live here; every one was byte-identical to
// the loop's own entry.)
//
// `#mills/overview` -> the renamed `mills-overview` sub-view. Registered as a
// bare `overview` key so parseHash's same-view alias branch catches it; the
// standalone `#overview` hash is answered earlier by the `raw === overviewId`
// short-circuit, so this entry can never hijack the top-level Overview.
legacyRedirects['overview'] = { view: 'mills', subView: 'mills-overview' };
// (`#projects`, the retired 8th top-level view, needs no entry here: Projects
// is now a Work sub-view, so the loop above already maps it to tasks/projects.)
// Mill-floor retirement (spec S0): the generic Backlog + Pipelines tabs were
// replaced by the Warps ▸ Shuttles ▸ Sparks ▸ Bolts spine. These aliases keep
// old hashes, bookmarks, and cross-links (e.g. Factory's #mills/pipelines/<id>)
// resolving to the new views. They override the loop's default self-redirects
// for these two ids and, because parseHash consults legacyRedirects when a
// valid view carries an unknown sub-view, they also catch #mills/backlog and
// #mills/pipelines/<id>.
legacyRedirects['backlog'] = { view: 'mills', subView: 'warps' };
legacyRedirects['pipelines'] = { view: 'mills', subView: 'shuttles' };

// The Operator Deck is the primary operator surface, so a bare/unknown hash
// lands there (it was agents/fleet before the deck existed).
const DEFAULT_VIEW = 'operator';
const DEFAULT_SUB = 'deck';

// ---- Hash parsing ----

function findViewDef(id: string): ViewDef | undefined {
  return views.find(v => v.id === id);
}

function parseHash(): RouteState {
  const raw = globalThis.location?.hash?.replace(/^#\/?/, '') ?? '';
  if (!raw || raw === overviewId) {
    return { view: raw === overviewId ? overviewId : DEFAULT_VIEW, subView: DEFAULT_SUB, detail: null };
  }

  const parts = raw.split('/');

  // Check for legacy single-segment hash (e.g., #fleet, #tasks)
  if (parts.length === 1) {
    const legacy = legacyRedirects[parts[0]];
    if (legacy) {
      return { view: legacy.view, subView: legacy.subView, detail: null };
    }
    // Could be a view id (e.g., #agents)
    const vd = findViewDef(parts[0]);
    if (vd) {
      return { view: vd.id, subView: vd.default, detail: null };
    }
    return { view: DEFAULT_VIEW, subView: DEFAULT_SUB, detail: null };
  }

  // Two or three segments: view/subView or view/subView/detail
  const viewId = parts[0];
  const subViewId = parts[1];
  const detailId = parts[2] || null;

  const vd = findViewDef(viewId);
  if (!vd) {
    // Try legacy redirect on first segment
    const legacy = legacyRedirects[viewId];
    if (legacy) {
      // The second segment is a detail id UNLESS it just restates the target
      // sub-view — `#projects/projects` (the retired view's own default hash)
      // would otherwise resolve to tasks/projects with detail="projects" and
      // open a drawer for a record that does not exist.
      const detail = subViewId && subViewId !== legacy.subView ? subViewId : null;
      return { view: legacy.view, subView: legacy.subView, detail };
    }
    return { view: DEFAULT_VIEW, subView: DEFAULT_SUB, detail: null };
  }

  // Validate subView belongs to this view
  const validSub = vd.subViews.some(sv => sv.id === subViewId);
  if (!validSub) {
    // A retired sub-view id under a still-valid view (e.g. #mills/backlog or
    // the cross-linked #mills/pipelines/<id>): redirect within the same view,
    // preserving any detail segment. Only fires for same-view aliases so an
    // unrelated legacy id can't hijack the sub-view.
    const legacy = legacyRedirects[subViewId];
    if (legacy && legacy.view === vd.id) {
      return { view: vd.id, subView: legacy.subView, detail: detailId };
    }
  }
  return {
    view: vd.id,
    subView: validSub ? subViewId : vd.default,
    detail: detailId,
  };
}

// ---- Router class ----

class Router {
  view = $state(DEFAULT_VIEW);
  subView = $state(DEFAULT_SUB);
  detail = $state<string | null>(null);

  // Legacy alias: panels that read router.panel still work
  get panel(): string {
    return this.subView;
  }

  private listening = false;

  /** Initialize from current URL hash and start listening. */
  init(): void {
    const state = parseHash();
    this.view = state.view;
    this.subView = state.subView;
    this.detail = state.detail;

    // Rewrite legacy hashes to new format
    this._syncHash();

    if (!this.listening && typeof globalThis.addEventListener === 'function') {
      globalThis.addEventListener('hashchange', () => {
        const s = parseHash();
        this.view = s.view;
        this.subView = s.subView;
        this.detail = s.detail;
      });
      this.listening = true;
    }
  }

  /** Navigate to a view + subView, optionally with a detail ID. */
  navigate(view: string, subView?: string, detail?: string | null): void {
    // Handle legacy single-arg calls: navigate('fleet') -> navigate('agents', 'fleet')
    const legacy = legacyRedirects[view];
    if (!findViewDef(view) && view !== overviewId && legacy) {
      this.view = legacy.view;
      this.subView = subView ?? legacy.subView;
    } else if (view === overviewId) {
      this.view = overviewId;
      this.subView = '';
    } else {
      const vd = findViewDef(view);
      this.view = view;
      let sub = subView ?? vd?.default ?? '';
      // Resolve a retired sub-view id (e.g. 'backlog' -> 'warps') the same way
      // parseHash does. _syncHash writes the new hash with replaceState, which
      // fires NO hashchange, so parseHash never gets a second chance: an
      // unresolved id would sit in router.subView with no registered panel and
      // render a blank pane until the operator reloaded.
      if (sub && vd && !vd.subViews.some((s) => s.id === sub)) {
        const alias = legacyRedirects[sub];
        if (alias && alias.view === vd.id) sub = alias.subView;
      }
      this.subView = sub;
    }
    // Embed subset guard (Slice B5): redirect to Overview when the requested
    // view is outside the operator allowlist. Sub-views fall back to the
    // first allowed sub-view of the parent view.
    if (!embedConfig.isViewAllowed(this.view)) {
      this.view = overviewId;
      this.subView = '';
    } else if (this.subView && !embedConfig.isSubViewAllowed(this.view, this.subView)) {
      const allowed = embedConfig.allowedSubViews[this.view];
      const vd = findViewDef(this.view);
      this.subView = (allowed && allowed.length > 0 ? allowed[0] : vd?.default ?? '');
    }
    this.detail = detail ?? null;
    this._syncHash();
  }

  /** Switch sub-view within the current view. */
  navigateSub(subView: string, detail?: string | null): void {
    // Embed subset guard: refuse to switch into a hidden sub-view; stay
    // on the current sub-view rather than dropping to overview, which
    // would be jarring during in-panel tab switches.
    if (!embedConfig.isSubViewAllowed(this.view, subView)) {
      return;
    }
    this.subView = subView;
    this.detail = detail ?? null;
    this._syncHash();
  }

  /** Navigate to detail within current view/subView. */
  navigateDetail(detail: string | null): void {
    this.detail = detail;
    this._syncHash();
  }

  /**
   * Open the Plans compare/merge editor over 2+ competing draft plans. Encodes
   * the ids as a `+`-joined detail segment (#tasks/plans/id1+id2); App.svelte
   * routes a `+`-bearing plans detail to PlansComparePanel instead of the
   * normal board drawer.
   */
  navigateCompare(ids: string[]): void {
    this.navigate('tasks', 'plans', ids.join('+'));
  }

  /** Navigate back: clear detail first, then sub-view. */
  back(): void {
    if (this.detail) {
      this.detail = null;
      this._syncHash();
    }
  }

  /** Get the ViewDef for the current view. */
  get currentViewDef(): ViewDef | undefined {
    return findViewDef(this.view);
  }

  private _syncHash(): void {
    let hash: string;
    if (this.view === overviewId) {
      hash = `#${overviewId}`;
    } else if (this.detail) {
      hash = `#${this.view}/${this.subView}/${this.detail}`;
    } else {
      hash = `#${this.view}/${this.subView}`;
    }
    if (globalThis.location && globalThis.location.hash !== hash) {
      globalThis.history.replaceState(null, '', hash);
    }
  }
}

export const router = new Router();
