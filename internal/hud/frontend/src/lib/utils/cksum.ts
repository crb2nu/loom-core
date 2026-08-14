// POSIX cksum (CRC-32/CKSUM) — the exact hash the lifecycle hooks use to
// mint agent ids (pkg/generator/configs_hooks.go):
//
//   WS_HASH       = cksum of the git workspace root path
//   SESSION_SCOPE = cksum of the vendor conversation/session id
//   agent_id      = <base>-<WS_HASH>[-<SESSION_SCOPE>]
//
// Being able to recompute both client-side is what links an on-disk vendor
// transcript (which knows its uuid + cwd) to the fleet's live agent rows
// (which know only the hashes) EXACTLY — no fuzzy path matching.
//
// Algorithm (POSIX.1 cksum): CRC-32 with polynomial 0x04C11DB7, MSB-first,
// zero init, then the input LENGTH is fed in as trailing octets (least
// significant first, only while non-zero), and the result is complemented.
// Verified against `/usr/bin/cksum` vectors in cksum.test.ts.

const POLY = 0x04c11db7;

const TABLE: Uint32Array = (() => {
  const t = new Uint32Array(256);
  for (let i = 0; i < 256; i += 1) {
    let c = (i << 24) >>> 0;
    for (let k = 0; k < 8; k += 1) {
      c = c & 0x80000000 ? (((c << 1) ^ POLY) >>> 0) : ((c << 1) >>> 0);
    }
    t[i] = c >>> 0;
  }
  return t;
})();

function step(crc: number, byte: number): number {
  return (((crc << 8) >>> 0) ^ TABLE[((crc >>> 24) ^ byte) & 0xff]) >>> 0;
}

/** cksum returns the POSIX cksum CRC of a string's UTF-8 bytes. */
export function cksum(input: string): number {
  const bytes = new TextEncoder().encode(input);
  let crc = 0;
  for (const b of bytes) crc = step(crc, b);
  let len = bytes.length;
  while (len > 0) {
    crc = step(crc, len & 0xff);
    len = Math.floor(len / 256);
  }
  return ~crc >>> 0;
}

/** cksumString is cksum rendered the way agent ids embed it (decimal). */
export function cksumString(input: string): string {
  return String(cksum(input));
}
