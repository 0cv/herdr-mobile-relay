import { describe, expect, it, vi } from 'vitest';
import { normalizeScrollState, PiScrollQueue } from '$lib/pi-scroll';

const state = { selection: 'auto', renderer: 'fullscreen', wheel: 'sgr_cell', source: 'pi_runtime', route: 'application', presentation: 'live_screen', available: true, reason: 'pi_fullscreen', revision: 'a'.repeat(64), rows: 30, columns: 79, geometry_epoch: 1 };
function deferred() {
  let resolve!: () => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<void>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
const settle = async () => { await Promise.resolve(); await Promise.resolve(); };

describe('Pi state admission', () => {
  it('accepts valid runtime evidence but rejects invented routes and dimensions', () => {
    expect(normalizeScrollState(state)).toEqual(state);
    for (const patch of [{ renderer: 'regular' }, { available: false }, { rows: 0 }, { columns: 65536 }, { rows: 1.5 }, { geometry_epoch: -1 }, { revision: 'old' }, { source: 'settings' }, { selection: 'local' }]) {
      expect(normalizeScrollState({ ...state, ...patch })).toBeUndefined();
    }
    expect(normalizeScrollState({ ...state, selection: 'local', available: false, route: 'local' })).toBeDefined();
  });
});

describe('Pi button queue', () => {
  it('keeps one in flight and only the newest pending intent', async () => {
    const first = deferred(), failed = vi.fn(), second = vi.fn(async () => {}), third = vi.fn(async () => {});
    const queue = new PiScrollQueue(failed, () => 0);
    queue.offer({ target: 'pane', revision: 'a', run: () => first.promise });
    queue.offer({ target: 'pane', revision: 'a', run: second });
    queue.offer({ target: 'pane', revision: 'a', run: third });
    expect(third).not.toHaveBeenCalled();
    first.resolve(); await settle();
    expect(second).not.toHaveBeenCalled(); expect(third).toHaveBeenCalledOnce(); expect(failed).not.toHaveBeenCalled();
  });
  it.each(['same', 'changed'])('dispatches a fresh %s binding after cancelled active success', async binding => {
    const first = deferred(), stale = vi.fn(async () => {}), fresh = vi.fn(async () => {}), failed = vi.fn();
    const queue = new PiScrollQueue(failed, () => 0);
    queue.offer({ target: 'pane', revision: 'a', run: () => first.promise });
    queue.offer({ target: 'pane', revision: 'a', run: stale });
    queue.cancel();
    queue.offer({ target: binding === 'same' ? 'pane' : 'new-pane', revision: binding === 'same' ? 'a' : 'b', run: fresh });
    expect(fresh).not.toHaveBeenCalled();
    first.resolve(); await settle();
    expect(stale).not.toHaveBeenCalled(); expect(fresh).toHaveBeenCalledOnce(); expect(failed).not.toHaveBeenCalled();
  });
  it.each(['cancel-again', 'expired', 'uncertain'])('refuses a fresh post-cancellation intent on %s', async kind => {
    let now = 0;
    const first = deferred(), fresh = vi.fn(async () => {}), failed = vi.fn();
    const queue = new PiScrollQueue(failed, () => now);
    queue.offer({ target: 'pane', revision: 'a', run: () => first.promise });
    queue.cancel();
    queue.offer({ target: 'new-pane', revision: 'b', run: fresh });
    if (kind === 'cancel-again') queue.cancel();
    if (kind === 'expired') now = 250;
    if (kind === 'uncertain') first.reject(new Error('dispatched_unknown')); else first.resolve();
    await settle();
    expect(fresh).not.toHaveBeenCalled();
    expect(failed).toHaveBeenCalledTimes(kind === 'uncertain' ? 1 : 0);
  });
  it.each(['cancel', 'expired', 'revision', 'target', 'uncertain'])('drops pending on %s', async kind => {
    let now = 0;
    const first = deferred(), pending = vi.fn(async () => {}), failed = vi.fn();
    const queue = new PiScrollQueue(failed, () => now);
    queue.offer({ target: 'pane', revision: 'a', run: () => first.promise });
    queue.offer({ target: kind === 'target' ? 'other' : 'pane', revision: kind === 'revision' ? 'b' : 'a', run: pending });
    if (kind === 'cancel') queue.cancel();
    if (kind === 'expired') now = 250;
    if (kind === 'uncertain') first.reject(new Error('dispatched_unknown')); else first.resolve();
    await settle();
    expect(pending).not.toHaveBeenCalled();
    expect(failed).toHaveBeenCalledTimes(kind === 'uncertain' ? 1 : 0);
  });
});
