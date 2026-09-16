import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import TelemetryPanel from './TelemetryPanel.svelte';
import { millsStore, type FleetGateWaiver } from '../../stores/mills.svelte.ts';

let target: HTMLElement;
let component: Record<string, unknown>;

const waiver = (over: Partial<FleetGateWaiver>): FleetGateWaiver => ({
  benchmark: 'BenchmarkFleetMillsEventAppend',
  max_time_percent: 60,
  until: '2026-09-15',
  reason: 'migration 029 event read indexes cost +52% per append',
  days_remaining: 30,
  expired: false,
  ...over,
});

beforeEach(() => {
  vi.spyOn(millsStore, 'startPolling').mockImplementation(() => {});
  vi.spyOn(millsStore, 'stopPolling').mockImplementation(() => {});
  vi.spyOn(millsStore, 'refreshTelemetry').mockResolvedValue(undefined);
  vi.spyOn(millsStore, 'fetchWaivers').mockResolvedValue(undefined);
  millsStore.waivers = null;
  millsStore.waiversError = null;
  millsStore.disabled = false;
  target = document.createElement('div');
  document.body.appendChild(target);
});

afterEach(() => {
  void unmount(component);
  vi.restoreAllMocks();
  millsStore.waivers = null;
  target.remove();
});

function render(): void {
  component = mount(TelemetryPanel, { target }) as Record<string, unknown>;
  flushSync();
}

describe('TelemetryPanel benchmark waivers', () => {
  it('shows the countdown, the raised cap, and the scale it is raised from', () => {
    // The countdown is the whole reason the card exists: past `until` the gate
    // snaps back to the global threshold on its own.
    millsStore.waivers = {
      waivers: [waiver({})],
      global_time_percent: 25,
      suite_version: 7,
    };
    render();

    const card = target.querySelector('.waivers')!;
    expect(card).not.toBeNull();
    expect(card.textContent).toContain('BenchmarkFleetMillsEventAppend');
    expect(card.textContent).toContain('cap 60%');
    expect(card.textContent).toContain('global cap 25%');
    expect(card.textContent).toContain('30d left');
    // The reason is what makes a waiver reviewable rather than a magic number.
    expect(card.textContent).toContain('migration 029');
  });

  it('orders nearest expiry first and tones by urgency', () => {
    millsStore.waivers = {
      waivers: [
        waiver({ benchmark: 'BenchFar', days_remaining: 200 }),
        waiver({ benchmark: 'BenchGone', days_remaining: -3, expired: true }),
        waiver({ benchmark: 'BenchSoon', days_remaining: 5 }),
      ],
      global_time_percent: 25,
      suite_version: 7,
    };
    render();

    const rows = [...target.querySelectorAll('.waiver')];
    expect(rows.map((r) => r.querySelector('.waiver-name')?.textContent)).toEqual([
      'BenchGone',
      'BenchSoon',
      'BenchFar',
    ]);
    expect(rows[0].className).toContain('tone-bad');
    expect(rows[0].textContent).toContain('expired 3d ago');
    expect(rows[1].className).toContain('tone-warn');
    expect(rows[2].className).toContain('tone-ok');
  });

  it('reads "expires today" on the inclusive last day', () => {
    millsStore.waivers = {
      waivers: [waiver({ days_remaining: 0 })],
      global_time_percent: 25,
      suite_version: 7,
    };
    render();
    expect(target.querySelector('.waiver-when')?.textContent?.trim()).toBe('expires today');
  });

  it('renders nothing when there are no waivers', () => {
    // An empty card would be noise on every ordinary day.
    millsStore.waivers = { waivers: [], global_time_percent: 25, suite_version: 7 };
    render();
    expect(target.querySelector('.waivers')).toBeNull();
  });

  it('renders nothing when the operator cannot read its manifest', () => {
    millsStore.waivers = null;
    render();
    expect(target.querySelector('.waivers')).toBeNull();
  });
});
