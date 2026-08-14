# Iteration Plan: Factory Loom Sounds — py-chiptune Sound Pack

**Date:** 2026-07-08
**Source:** operator request ("use our libs/py-chiptune to really punch up
the sound effects") — upgrades !1015's hand-synthesized oscillator voices.
**Prior art:** `.loom/174` concept 9 noted `libs/py-chiptune` as in-workspace
prior art from day one.

## Riskiest assumption + kill-test

**Load-bearing assumption**: `libs/py-chiptune` (flexinfer-chiptune 0.5.3)
can render suitable short SFX as WAV from its CLI, small enough to bundle.

**Kill test**: ran 2026-07-08 — `uv run chiptune sfx {land,success,error}
--format wav` produced valid 44.1 kHz/16-bit WAVs of 42–104 ms at 7–18 KB
each (inspected with Python `wave`); Vite bundles them (`?url`) and vitest
still resolves the module.

**Status**: passed 2026-07-08.

## Slices (one MR, stacked on the fuel-gauge branch)

1. **Assets**: `src/lib/assets/sfx/{clack,chime,klaxon}.wav` — mapping
   land→clack (reed beat), success→chime (bolt merged), error→klaxon
   (spark). ~40 KB total, checked in. `scripts/gen-sfx.sh` documents the
   exact regeneration commands (evidence-first: assets are reproducible,
   not mystery blobs).
2. **factorySounds.ts**: chiptune-first playback — buffers fetched +
   decoded lazily on first enable (inside the toggle's user gesture, so
   autoplay policy stays satisfied), per-voice gain trim into the same
   master bus; the original oscillator voices remain as per-voice
   fallback so a failed fetch/decode degrades to the old sound, never to
   silence-with-errors. No API change — the honesty contract (sound only
   for a laid row) and the no-op guard are untouched.

## Verification

- vitest: existing no-op guard still passes (`?url` resolves under
  vitest's Vite pipeline); 121/121.
- `pnpm build` bundles the three WAVs (7.4/18.4/14.7 KB).
- Preview MCP: toggle-enable fetches all three WAVs (network-observed),
  a freshly merged run weaves a bolt through the real poll path, zero
  console errors.
