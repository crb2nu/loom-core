import { describe, expect, it } from 'vitest';
import {
  backendVariant,
  isFakeBackend,
  normalizeWiring,
  routeChain,
  sourceVariant,
} from './millsWiringHelpers.ts';

describe('normalizeWiring', () => {
  it('folds the full contract into the normalised shape', () => {
    const w = normalizeWiring({
      generated_at: '2026-07-17T04:12:00Z',
      judge: {
        backend: 'litellm',
        model: 'or/kimi-k3',
        fallbacks: ['or/kimi-k2.7-code'],
        max_tokens: 1024,
        registry_fallbacks_disabled: true,
      },
      weaver: {
        backend: 'flexinfer',
        model: 'qwen35-35b-clean-gptq-workhorse',
        fallbacks: ['qwen35-35b-clean-gptq-workhorse-128k', 'gemma4-e4b-radeonvii'],
      },
      council: {
        judge_backend: 'litellm',
        judge_model: 'or/kimi-k3',
        editor_backend: 'flexinfer',
        editor_model: 'ed',
        lenses: [{ name: 'frontier', backend: 'litellm', model: 'or/kimi-k3' }],
      },
      stages: [{ stage: 'implement', agent: 'codex', model: 'gpt-5.6-terra', source: 'policy' }],
      spawn: { default_agent: 'claude-code', env_agent_override: false, env_model_override: false },
      gates: { llm_gates_enabled: true, tiebreaker: 'anthropic' },
      litellm: { configured: true },
      policy: { autonomy_enabled: true },
    });
    expect(w.generated_at).toBe('2026-07-17T04:12:00Z');
    expect(w.judge.max_tokens).toBe(1024);
    expect(w.judge.registry_fallbacks_disabled).toBe(true);
    expect(w.weaver.fallbacks).toHaveLength(2);
    expect(w.council.lenses).toHaveLength(1);
    expect(w.stages[0]?.source).toBe('policy');
    expect(w.gates.tiebreaker).toBe('anthropic');
    expect(w.litellm.configured).toBe(true);
  });

  it('coerces Go nil slices and missing nested objects to safe defaults', () => {
    // The operator can encode empty slices as JSON null and omit whole
    // blocks — normalizeWiring must default every array to [] and every
    // nested struct so no panel $derived ever spreads null or reads undefined.
    const w = normalizeWiring({
      judge: { backend: 'litellm', model: 'm', fallbacks: null },
      // weaver, council, stages, spawn, gates, litellm, policy all missing
    });
    expect(w.judge.fallbacks).toEqual([]);
    expect(w.weaver).toEqual({ backend: '', model: '', fallbacks: [] });
    expect(w.council.lenses).toEqual([]);
    expect(w.stages).toEqual([]);
    expect(w.spawn.default_agent).toBe('');
    expect(w.spawn.env_agent_override).toBe(false);
    expect(w.gates.llm_gates_enabled).toBe(false);
    expect(w.litellm.configured).toBe(false);
    expect(w.policy.autonomy_enabled).toBe(false);
    // Spreading the normalised arrays must never throw.
    expect([...w.stages]).toEqual([]);
    expect([...w.council.lenses]).toEqual([]);
  });

  it('is total on garbage input', () => {
    expect(normalizeWiring(null).stages).toEqual([]);
    expect(normalizeWiring('nope').council.lenses).toEqual([]);
    expect(normalizeWiring(42).judge.fallbacks).toEqual([]);
  });
});

describe('backendVariant', () => {
  it('maps known backends to distinct calm variants', () => {
    expect(backendVariant('litellm')).toBe('accent');
    expect(backendVariant('flexinfer')).toBe('info');
    expect(backendVariant('anthropic')).toBe('success');
  });

  it('maps agents distinctly within the stage column', () => {
    expect(backendVariant('codex')).toBe('info');
    expect(backendVariant('claude-code')).toBe('success');
  });

  it('flags fake/stub backends as warning and unknown as muted', () => {
    expect(backendVariant('fake')).toBe('warning');
    expect(backendVariant('stub')).toBe('warning');
    expect(backendVariant('')).toBe('muted');
    expect(backendVariant('something-new')).toBe('muted');
  });
});

describe('sourceVariant', () => {
  it('reads policy/env/default distinctly', () => {
    expect(sourceVariant('policy')).toBe('info');
    expect(sourceVariant('env')).toBe('accent');
    expect(sourceVariant('default')).toBe('muted');
    expect(sourceVariant('')).toBe('muted');
  });
});

describe('isFakeBackend', () => {
  it('treats empty + stub aliases as fake', () => {
    expect(isFakeBackend('')).toBe(true);
    expect(isFakeBackend('fake')).toBe(true);
    expect(isFakeBackend('mock')).toBe(true);
    expect(isFakeBackend('litellm')).toBe(false);
  });
});

describe('routeChain', () => {
  it('renders primary then fallbacks, dropping blanks', () => {
    expect(routeChain({ model: 'a', fallbacks: ['b', 'c'] })).toEqual(['a', 'b', 'c']);
    expect(routeChain({ model: '', fallbacks: ['', ' '] })).toEqual([]);
    expect(routeChain({ model: ' m ', fallbacks: ['x'] })).toEqual(['m', 'x']);
  });
});
