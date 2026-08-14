// Loom sounds for the Factory panel — opt-in, chiptune-first. The three
// effects are authored with libs/py-chiptune (see scripts/gen-sfx.sh for
// the exact regeneration commands) and shipped as tiny WAVs; the original
// hand-synthesized oscillator voices remain as a fallback so a failed
// fetch/decode degrades to the old sound, never to silence-with-errors.
//
// The audio contract mirrors the visual one: a sound fires only for a
// real event (a pick laid, a bolt rolled off, a spark), never as
// ambience. Everything degrades to a silent no-op where Web Audio is
// unavailable (vitest / odd embeds), so callers never need to guard.

import clackUrl from '../assets/sfx/clack.wav?url';
import chimeUrl from '../assets/sfx/chime.wav?url';
import klaxonUrl from '../assets/sfx/klaxon.wav?url';

interface AudioContextCtor {
  new (): AudioContext;
}

function audioCtor(): AudioContextCtor | null {
  const g = globalThis as { AudioContext?: AudioContextCtor; webkitAudioContext?: AudioContextCtor };
  return g.AudioContext ?? g.webkitAudioContext ?? null;
}

export interface LoomSounds {
  /** Web Audio is available in this environment. */
  readonly supported: boolean;
  /** Flip the gate. Enabling from a user gesture also resumes the context. */
  setEnabled(on: boolean): void;
  /** Reed beat laying a real pick. */
  clack(): void;
  /** Bolt rolled off the beam — merged. */
  chime(): void;
  /** Spark — a run escalated. */
  klaxon(): void;
  /** Tear down the context on unmount. */
  dispose(): void;
}

const MASTER_GAIN = 0.09;
type Voice = 'clack' | 'chime' | 'klaxon';
const VOICE_URL: Record<Voice, string> = { clack: clackUrl, chime: chimeUrl, klaxon: klaxonUrl };
// Per-voice trim into the master bus — the chiptune renders are
// full-scale, the desk is not a chip-tune arcade.
const VOICE_GAIN: Record<Voice, number> = { clack: 0.5, chime: 0.45, klaxon: 0.55 };

export function createLoomSounds(): LoomSounds {
  const maybeCtor = audioCtor();
  if (!maybeCtor) {
    const noop = () => {};
    return { supported: false, setEnabled: noop, clack: noop, chime: noop, klaxon: noop, dispose: noop };
  }
  // Rebind with a non-null declared type: flow narrowing doesn't survive
  // into the closures below (ensure/dispose), a declared type does.
  const Ctor: AudioContextCtor = maybeCtor;

  let ctx: AudioContext | null = null;
  let master: GainNode | null = null;
  let enabled = false;
  const buffers = new Map<Voice, AudioBuffer>();
  let loadKicked = false;

  // The context is created lazily inside setEnabled(true) — a user
  // gesture — so autoplay policy never leaves it wedged in "suspended".
  function ensure(): AudioContext | null {
    if (!enabled) return null;
    if (!ctx) {
      ctx = new Ctor();
      master = ctx.createGain();
      master.gain.value = MASTER_GAIN;
      master.connect(ctx.destination);
    }
    if (ctx.state === 'suspended') void ctx.resume();
    if (!loadKicked) {
      loadKicked = true;
      void loadBuffers(ctx);
    }
    return ctx;
  }

  // Best-effort chiptune load: each voice independently; a failure
  // simply leaves that voice on its synthesized fallback.
  async function loadBuffers(c: AudioContext): Promise<void> {
    await Promise.all(
      (Object.keys(VOICE_URL) as Voice[]).map(async (voice) => {
        try {
          const res = await fetch(VOICE_URL[voice]);
          if (!res.ok) return;
          const buf = await c.decodeAudioData(await res.arrayBuffer());
          buffers.set(voice, buf);
        } catch {
          /* fall back to the oscillator voice */
        }
      }),
    );
  }

  /** Play the chiptune render if loaded; false → caller uses the fallback. */
  function playBuffer(voice: Voice): boolean {
    const c = ensure();
    if (!c || !master) return false;
    const buf = buffers.get(voice);
    if (!buf) return false;
    const src = c.createBufferSource();
    src.buffer = buf;
    const g = c.createGain();
    g.gain.value = VOICE_GAIN[voice];
    src.connect(g);
    g.connect(master);
    src.start();
    return true;
  }

  function env(at: number, peak: number, decay: number): GainNode {
    const g = ctx!.createGain();
    g.gain.setValueAtTime(0.0001, at);
    g.gain.exponentialRampToValueAtTime(peak, at + 0.008);
    g.gain.exponentialRampToValueAtTime(0.0001, at + decay);
    g.connect(master!);
    return g;
  }

  return {
    supported: true,
    setEnabled(on: boolean) {
      enabled = on;
      if (on) ensure();
    },
    clack() {
      if (playBuffer('clack')) return;
      const c = ensure();
      if (!c) return;
      // Fallback: a short filtered noise burst — the reed striking the fell.
      const t = c.currentTime;
      const len = Math.floor(c.sampleRate * 0.035);
      const buf = c.createBuffer(1, len, c.sampleRate);
      const data = buf.getChannelData(0);
      for (let i = 0; i < len; i++) data[i] = (Math.random() * 2 - 1) * (1 - i / len);
      const src = c.createBufferSource();
      src.buffer = buf;
      const bp = c.createBiquadFilter();
      bp.type = 'bandpass';
      bp.frequency.value = 2200;
      bp.Q.value = 1.2;
      src.connect(bp);
      bp.connect(env(t, 0.9, 0.06));
      src.start(t);
    },
    chime() {
      if (playBuffer('chime')) return;
      const c = ensure();
      if (!c) return;
      // Fallback: two soft partials, a fifth apart.
      const t = c.currentTime;
      for (const [freq, peak] of [[880, 0.5], [1318.5, 0.3]] as const) {
        const o = c.createOscillator();
        o.type = 'sine';
        o.frequency.value = freq;
        o.connect(env(t, peak, 0.55));
        o.start(t);
        o.stop(t + 0.6);
      }
    },
    klaxon() {
      if (playBuffer('klaxon')) return;
      const c = ensure();
      if (!c) return;
      // Fallback: a low sawtooth dropping a third — desk volume trouble.
      const t = c.currentTime;
      const o = c.createOscillator();
      o.type = 'sawtooth';
      o.frequency.setValueAtTime(196, t);
      o.frequency.exponentialRampToValueAtTime(147, t + 0.45);
      const lp = c.createBiquadFilter();
      lp.type = 'lowpass';
      lp.frequency.value = 700;
      o.connect(lp);
      lp.connect(env(t, 0.7, 0.5));
      o.start(t);
      o.stop(t + 0.55);
    },
    dispose() {
      enabled = false;
      void ctx?.close();
      ctx = null;
      master = null;
      buffers.clear();
      loadKicked = false;
    },
  };
}
