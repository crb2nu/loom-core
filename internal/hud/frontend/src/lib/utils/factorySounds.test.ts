import { describe, expect, it } from 'vitest';
import { createLoomSounds } from './factorySounds.ts';

// vitest's node environment has no Web Audio — exactly the degraded
// environment the module must survive. Every method must be a safe
// no-op so the panel never needs to guard.
describe('createLoomSounds without Web Audio', () => {
  it('reports unsupported and every method is a safe no-op', () => {
    const s = createLoomSounds();
    expect(s.supported).toBe(false);
    expect(() => {
      s.setEnabled(true);
      s.clack();
      s.chime();
      s.klaxon();
      s.setEnabled(false);
      s.dispose();
    }).not.toThrow();
  });
});
