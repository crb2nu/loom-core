// Pure helpers + types for the Mills Overview "Loom wiring" card.
//
// Split out of the rune-based store (mills.svelte.ts) so the wire shape and
// its normalisation stay runnable under plain Node — fixture/test scripts
// exercise `normalizeWiring` + the badge-variant mappers without a Svelte
// runtime. The panel imports the types + mappers from here; the store owns
// only the fetch + `$state` slot.
//
// Contract: GET /api/mills/wiring, served by the loom-mills-operator via the
// HUD proxy against this exact shape. An operator older than the route answers
// 404 (endpoint absent); the panel then names that instead of rendering
// routing — the same degrade the Telemetry panel uses for its stage feed.

import type { BadgeVariant } from './tokens.ts';

// A single model route: the primary model plus an ordered degrade chain of
// fallbacks the backend tries when the primary is unreachable. `max_tokens`
// and `registry_fallbacks_disabled` are judge-specific (the weaver omits
// them), so they're optional.
export interface WiringModelRoute {
  backend: string;
  model: string;
  fallbacks: string[];
  max_tokens?: number;
  registry_fallbacks_disabled?: boolean;
}

// One council reviewer lens: a named critique pass bound to its own
// backend/model. A "fake" backend (stub lens) is a misconfiguration the card
// flags with a warning chip.
export interface WiringCouncilLens {
  name: string;
  backend: string;
  model: string;
}

export interface WiringCouncil {
  judge_backend: string;
  judge_model: string;
  editor_backend: string;
  editor_model: string;
  lenses: WiringCouncilLens[];
}

// One pipeline stage's agent/model routing. `source` records where the
// binding came from: `policy` (operator policy), `env` (an env override
// shadowing policy), or `default` (unset — the spawn default agent runs it).
export interface WiringStage {
  stage: string;
  agent: string;
  model: string;
  source: string;
}

export interface WiringSpawn {
  default_agent: string;
  env_agent_override: boolean;
  env_model_override: boolean;
}

export interface WiringGates {
  llm_gates_enabled: boolean;
  tiebreaker: string;
}

export interface WiringLiteLLM {
  configured: boolean;
}

export interface WiringPolicy {
  autonomy_enabled: boolean;
}

// MillsWiring is the fully-normalised shape the panel renders. Every field is
// non-optional after normalizeWiring so a panel `$derived` can never hit an
// undefined nested object or spread a null array (the `gates:null` wedge that
// froze the pipeline drawer). Only `generated_at` stays optional — it's
// display-only.
export interface MillsWiring {
  generated_at?: string;
  judge: WiringModelRoute;
  weaver: WiringModelRoute;
  council: WiringCouncil;
  stages: WiringStage[];
  spawn: WiringSpawn;
  gates: WiringGates;
  litellm: WiringLiteLLM;
  policy: WiringPolicy;
}

// Backends/agents that mean "not really wired" — a stub/fake lens or an unset
// route. These render as a warning chip so a misconfiguration reads distinct
// from a healthy-but-different backend.
const FAKE_BACKENDS: ReadonlySet<string> = new Set(['fake', 'mock', 'stub', 'none']);

function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

function bool(v: unknown): boolean {
  return v === true;
}

function strArray(v: unknown): string[] {
  if (!Array.isArray(v)) return [];
  return v.filter((x): x is string => typeof x === 'string');
}

function normalizeRoute(raw: unknown): WiringModelRoute {
  const r = (raw && typeof raw === 'object' ? raw : {}) as Record<string, unknown>;
  const route: WiringModelRoute = {
    backend: str(r.backend),
    model: str(r.model),
    fallbacks: strArray(r.fallbacks),
  };
  if (typeof r.max_tokens === 'number' && Number.isFinite(r.max_tokens)) {
    route.max_tokens = r.max_tokens;
  }
  if (typeof r.registry_fallbacks_disabled === 'boolean') {
    route.registry_fallbacks_disabled = r.registry_fallbacks_disabled;
  }
  return route;
}

function normalizeLenses(raw: unknown): WiringCouncilLens[] {
  if (!Array.isArray(raw)) return [];
  return raw.map((item) => {
    const l = (item && typeof item === 'object' ? item : {}) as Record<string, unknown>;
    return { name: str(l.name), backend: str(l.backend), model: str(l.model) };
  });
}

function normalizeCouncil(raw: unknown): WiringCouncil {
  const c = (raw && typeof raw === 'object' ? raw : {}) as Record<string, unknown>;
  return {
    judge_backend: str(c.judge_backend),
    judge_model: str(c.judge_model),
    editor_backend: str(c.editor_backend),
    editor_model: str(c.editor_model),
    lenses: normalizeLenses(c.lenses),
  };
}

function normalizeStages(raw: unknown): WiringStage[] {
  if (!Array.isArray(raw)) return [];
  return raw.map((item) => {
    const s = (item && typeof item === 'object' ? item : {}) as Record<string, unknown>;
    return {
      stage: str(s.stage),
      agent: str(s.agent),
      model: str(s.model),
      source: str(s.source),
    };
  });
}

// normalizeWiring folds the operator's (possibly partial / null-slice) payload
// into the fully-populated MillsWiring shape. Every array is coerced to `[]`
// and every nested object to a defaulted struct at THIS boundary so no panel
// `$derived` ever spreads a Go nil slice or reads an undefined nested field.
// Accepts `unknown` so it can guard against a malformed body, not just the
// happy path.
export function normalizeWiring(raw: unknown): MillsWiring {
  const w = (raw && typeof raw === 'object' ? raw : {}) as Record<string, unknown>;
  const spawn = (w.spawn && typeof w.spawn === 'object' ? w.spawn : {}) as Record<string, unknown>;
  const gates = (w.gates && typeof w.gates === 'object' ? w.gates : {}) as Record<string, unknown>;
  const litellm = (w.litellm && typeof w.litellm === 'object' ? w.litellm : {}) as Record<string, unknown>;
  const policy = (w.policy && typeof w.policy === 'object' ? w.policy : {}) as Record<string, unknown>;
  return {
    generated_at: typeof w.generated_at === 'string' ? w.generated_at : undefined,
    judge: normalizeRoute(w.judge),
    weaver: normalizeRoute(w.weaver),
    council: normalizeCouncil(w.council),
    stages: normalizeStages(w.stages),
    spawn: {
      default_agent: str(spawn.default_agent),
      env_agent_override: bool(spawn.env_agent_override),
      env_model_override: bool(spawn.env_model_override),
    },
    gates: {
      llm_gates_enabled: bool(gates.llm_gates_enabled),
      tiebreaker: str(gates.tiebreaker),
    },
    litellm: { configured: bool(litellm.configured) },
    policy: { autonomy_enabled: bool(policy.autonomy_enabled) },
  };
}

// isFakeBackend reports whether a backend/agent id is a stub placeholder
// rather than a real routable backend. Used to flag a "fake" council lens.
export function isFakeBackend(name: string | undefined | null): boolean {
  const n = (name ?? '').toLowerCase().trim();
  return n === '' || FAKE_BACKENDS.has(n);
}

// backendVariant maps a backend OR agent id onto a Badge variant from the
// BadgeVariant union — the ONLY variants the shared Badge widget renders.
// Calm + distinct within each column: backends (litellm/flexinfer/anthropic)
// never collide, and agents (codex/claude-code) never collide. A stub/fake
// backend is `warning` so a misconfiguration stands out from a healthy route.
// warning/error are reserved for problems, so an unknown backend degrades to
// `muted` rather than borrowing an alarm colour.
export function backendVariant(name: string | undefined | null): BadgeVariant {
  const n = (name ?? '').toLowerCase().trim();
  if (n === '') return 'muted';
  if (FAKE_BACKENDS.has(n)) return 'warning';
  switch (n) {
    case 'litellm':
      return 'accent';
    case 'flexinfer':
      return 'info';
    case 'anthropic':
    case 'claude':
    case 'claude-code':
      return 'success';
    case 'codex':
    case 'openai':
      return 'info';
    case 'gemini':
    case 'google':
      return 'accent';
    default:
      return 'muted';
  }
}

// sourceVariant maps a stage's routing source onto a Badge variant.
// policy = authoritative config (info); env = an override shadowing policy,
// notable enough to stand out (accent); default = unset/fallthrough (muted).
export function sourceVariant(source: string | undefined | null): BadgeVariant {
  switch ((source ?? '').toLowerCase().trim()) {
    case 'policy':
      return 'info';
    case 'env':
      return 'accent';
    default:
      return 'muted';
  }
}

// routeChain renders a model route as its degrade list: the primary model
// first, then each fallback in try-order. The panel joins these with a "→"
// so the fallback chain reads as a visible degrade path. Blank models are
// dropped so an unset route renders empty rather than a lone arrow.
export function routeChain(route: Pick<WiringModelRoute, 'model' | 'fallbacks'>): string[] {
  const chain: string[] = [];
  if (route.model.trim()) chain.push(route.model.trim());
  for (const f of route.fallbacks) {
    if (f.trim()) chain.push(f.trim());
  }
  return chain;
}
