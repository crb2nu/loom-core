import { describe, expect, it } from 'vitest';
import { readdirSync, readFileSync } from 'node:fs';
import { dirname, extname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { mrURL } from './gitlabLinks.ts';

describe('mrURL', () => {
  it.each([
    ['loom-flightdeck', 12, 'https://gitlab.flexinfer.ai/services/loom-flightdeck/-/merge_requests/12'],
    ['platform/gitops', 7, 'https://gitlab.flexinfer.ai/platform/gitops/-/merge_requests/7'],
    ['', 3, 'https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/3'],
    [undefined, 3, 'https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/3'],
    ['  /services/Foo/  ', 9, 'https://gitlab.flexinfer.ai/services/Foo/-/merge_requests/9'],
    ['LOOM-CORE', 4, 'https://gitlab.flexinfer.ai/services/loom-core/-/merge_requests/4'],
  ])('routes project %j and IID %j', (project, iid, want) => {
    expect(mrURL(project, iid)).toBe(want);
  });

  it.each([undefined, null, 0, -1, 1.5, Number.NaN])('rejects absent or invalid IID %j', (iid) => {
    expect(mrURL('services/loom-core', iid)).toBeNull();
  });

  it('is the sole frontend source constructor for merge-request paths', () => {
    const lib = join(dirname(fileURLToPath(import.meta.url)), '..');
    const offenders: string[] = [];
    const visit = (dir: string): void => {
      for (const entry of readdirSync(dir, { withFileTypes: true })) {
        const path = join(dir, entry.name);
        if (entry.isDirectory()) visit(path);
        else if (
          ['.ts', '.svelte'].includes(extname(path)) &&
          !path.includes('.test.') &&
          !path.endsWith('gitlabLinks.ts')
        ) {
          if (readFileSync(path, 'utf8').includes('/-/merge_requests/')) offenders.push(path);
        }
      }
    };
    visit(lib);
    expect(offenders).toEqual([]);
  });
});
