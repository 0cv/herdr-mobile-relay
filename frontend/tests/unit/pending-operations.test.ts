import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { releasePendingResources, resetPendingDeadline, registerPendingOperation } from '$lib/pending-operations';

// Supplemental helper controls: appended-consumer proof was frozen green before extraction.
describe('releasePendingResources supplemental controls', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('clears a current timer with absent cleanup', async () => {
    const fired = vi.fn();
    const timer = setTimeout(() => { fired(); }, 10);
    const clear = vi.spyOn(globalThis, 'clearTimeout');
    expect(releasePendingResources({ timer })).toBeUndefined();
    expect(clear).toHaveBeenCalledExactlyOnceWith(timer);
    await vi.advanceTimersByTimeAsync(10);
    expect(fired).not.toHaveBeenCalled();
  });

  it('clears before a method-receiver cleanup and ignores its return value', () => {
    const clear = vi.spyOn(globalThis, 'clearTimeout');
    const pending = {
      timer: setTimeout(() => {}, 10),
      cleanup: vi.fn(function (this: { timer: ReturnType<typeof setTimeout> }) {
        expect(this).toBe(pending);
        expect(clear).toHaveBeenCalledExactlyOnceWith(this.timer);
        return 'ignored result';
      }),
    };
    expect(releasePendingResources(pending)).toBeUndefined();
    expect(pending.cleanup).toHaveBeenCalledTimes(1);
    expect(clear.mock.invocationCallOrder[0]).toBeLessThan(pending.cleanup.mock.invocationCallOrder[0]);
  });

  it('propagates the exact clear exception and does not attempt cleanup', () => {
    const error = new Error('clear failure');
    const cleanup = vi.fn();
    const timer = setTimeout(() => {}, 10);
    vi.spyOn(globalThis, 'clearTimeout').mockImplementation(() => { throw error; });
    expect(() => releasePendingResources({ timer, cleanup })).toThrow(error);
    expect(cleanup).not.toHaveBeenCalled();
  });

  it('propagates the exact cleanup exception after canceling the timer', async () => {
    const error = new Error('cleanup failure');
    const fired = vi.fn();
    const timer = setTimeout(() => { fired(); }, 10);
    const cleanup = vi.fn(() => { throw error; });
    expect(() => releasePendingResources({ timer, cleanup })).toThrow(error);
    expect(cleanup).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(10);
    expect(fired).not.toHaveBeenCalled();
  });

  it('does not add an idempotence guard or remember prior disposal', () => {
    const clear = vi.spyOn(globalThis, 'clearTimeout');
    const pending = { timer: setTimeout(() => {}, 10), cleanup: vi.fn() };
    releasePendingResources(pending);
    releasePendingResources(pending);
    expect(clear).toHaveBeenCalledTimes(2);
    expect(pending.cleanup).toHaveBeenCalledTimes(2);
  });

  it('reads the current timer at each call rather than capturing an original one', async () => {
    const original = vi.fn();
    const replacement = vi.fn();
    const timer = setTimeout(() => { original(); }, 10);
    const pending = { timer };
    pending.timer = setTimeout(() => { replacement(); }, 10);
    const clear = vi.spyOn(globalThis, 'clearTimeout');
    releasePendingResources(pending);
    expect(clear).toHaveBeenCalledExactlyOnceWith(pending.timer);
    expect(clear).not.toHaveBeenCalledWith(timer);
    await vi.advanceTimersByTimeAsync(10);
    expect(original).toHaveBeenCalledTimes(1);
    expect(replacement).not.toHaveBeenCalled();
  });

  it('propagates a cleanup getter exception with timer cancellation already complete', async () => {
    const error = new Error('cleanup getter');
    const fired = vi.fn();
    const pending = {
      timer: setTimeout(() => { fired(); }, 10),
      get cleanup(): () => void { throw error; },
    };
    expect(() => releasePendingResources(pending)).toThrow(error);
    await vi.advanceTimersByTimeAsync(10);
    expect(fired).not.toHaveBeenCalled();
  });
});

describe('resetPendingDeadline supplemental controls', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('clears then schedules the identical callback/duration before writing back its returned handle', async () => {
    const oldCallback = vi.fn();
    const callback = vi.fn();
    const pending = { timer: setTimeout(() => { oldCallback(); }, 10) };
    const old = pending.timer;
    const clear = vi.spyOn(globalThis, 'clearTimeout');
    const nativeSchedule = globalThis.setTimeout;
    const schedule = vi.spyOn(globalThis, 'setTimeout').mockImplementation((handler, duration) => {
      expect(clear).toHaveBeenCalledExactlyOnceWith(old);
      expect(pending.timer).toBe(old); // Assignment has not happened inside scheduling.
      expect(handler).toBe(callback);
      expect(duration).toBe(34);
      return nativeSchedule(handler, duration);
    });
    expect(resetPendingDeadline(pending, callback, 34)).toBeUndefined();
    expect(schedule).toHaveBeenCalledExactlyOnceWith(callback, 34);
    expect(clear.mock.invocationCallOrder[0]).toBeLessThan(schedule.mock.invocationCallOrder[0]);
    expect(pending.timer).toBe(schedule.mock.results[0].value);
    expect(pending.timer).not.toBe(old);
    await vi.advanceTimersByTimeAsync(33);
    expect(oldCallback).not.toHaveBeenCalled();
    expect(callback).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(callback).toHaveBeenCalledTimes(1);
  });

  it('repeated replacement reads the current handle and fires only the latest callback', async () => {
    const original = vi.fn();
    const superseded = vi.fn();
    const latest = vi.fn();
    const pending = { timer: setTimeout(() => { original(); }, 10) };
    const first = pending.timer;
    const clear = vi.spyOn(globalThis, 'clearTimeout');
    resetPendingDeadline(pending, superseded, 20);
    const second = pending.timer;
    await vi.advanceTimersByTimeAsync(5);
    resetPendingDeadline(pending, latest, 30);
    expect(clear.mock.calls).toEqual([[first], [second]]);
    expect(pending.timer).not.toBe(second);
    await vi.advanceTimersByTimeAsync(29);
    expect(original).not.toHaveBeenCalled();
    expect(superseded).not.toHaveBeenCalled();
    expect(latest).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(latest).toHaveBeenCalledTimes(1);
  });

  it('retains releasePendingResources cancellation of the replacement and method cleanup', async () => {
    const callback = vi.fn();
    const pending = { timer: setTimeout(() => {}, 10), cleanup: vi.fn() };
    resetPendingDeadline(pending, callback, 20);
    const latest = pending.timer;
    const clear = vi.spyOn(globalThis, 'clearTimeout');
    releasePendingResources(pending);
    expect(clear).toHaveBeenCalledExactlyOnceWith(latest);
    expect(pending.cleanup).toHaveBeenCalledTimes(1);
    expect(pending.cleanup.mock.contexts[0]).toBe(pending);
    await vi.advanceTimersByTimeAsync(20);
    expect(callback).not.toHaveBeenCalled();
  });

  it('propagates exact injected clear failure without scheduling, cancellation or assignment', async () => {
    const failure = { stage: 'clear' };
    const original = vi.fn();
    const callback = vi.fn();
    const pending = { timer: setTimeout(() => { original(); }, 10) };
    const old = pending.timer;
    const schedule = vi.spyOn(globalThis, 'setTimeout');
    vi.spyOn(globalThis, 'clearTimeout').mockImplementation(() => { throw failure; });
    let caught: unknown;
    try { resetPendingDeadline(pending, callback, 20); } catch (error) { caught = error; }
    expect(caught).toBe(failure);
    expect(schedule).not.toHaveBeenCalled();
    expect(pending.timer).toBe(old);
    await vi.advanceTimersByTimeAsync(20);
    expect(original).toHaveBeenCalledTimes(1);
    expect(callback).not.toHaveBeenCalled();
  });

  it('propagates exact injected schedule failure after cancellation but before assignment', async () => {
    const failure = { stage: 'schedule' };
    const original = vi.fn();
    const callback = vi.fn();
    const pending = { timer: setTimeout(() => { original(); }, 10) };
    const old = pending.timer;
    const clear = vi.spyOn(globalThis, 'clearTimeout');
    const schedule = vi.spyOn(globalThis, 'setTimeout').mockImplementation(() => { throw failure; });
    let caught: unknown;
    try { resetPendingDeadline(pending, callback, 20); } catch (error) { caught = error; }
    expect(caught).toBe(failure);
    expect(clear).toHaveBeenCalledExactlyOnceWith(old);
    expect(schedule).toHaveBeenCalledExactlyOnceWith(callback, 20);
    expect(clear.mock.invocationCallOrder[0]).toBeLessThan(schedule.mock.invocationCallOrder[0]);
    expect(pending.timer).toBe(old);
    await vi.advanceTimersByTimeAsync(20);
    expect(original).not.toHaveBeenCalled();
    expect(callback).not.toHaveBeenCalled();
  });
});

describe('registerPendingOperation supplemental controls', () => {
  afterEach(() => vi.restoreAllMocks());

  it('installs the native once listener before factory then inserts the identical record and returns void', () => {
    const controller = new AbortController();
    const signal = controller.signal;
    const operations = new Map<string, object>();
    const abort = vi.fn();
    const pending = { retained: 'identity' };
    const order: string[] = [];
    const nativeAdd = signal.addEventListener;
    const add = vi.spyOn(signal, 'addEventListener').mockImplementation((type, listener, options) => {
      order.push('listener');
      nativeAdd.call(signal, type, listener, options);
    });
    const set = vi.spyOn(operations, 'set').mockImplementation((key, value) => {
      order.push('insert');
      return Map.prototype.set.call(operations, key, value);
    });
    const factory = vi.fn(() => {
      order.push('factory');
      expect(add).toHaveBeenCalledExactlyOnceWith('abort', abort, { once: true });
      expect(operations.has('request')).toBe(false);
      return pending;
    });
    try {
      expect(registerPendingOperation(operations, 'request', signal, abort, factory)).toBeUndefined();
      expect(order).toEqual(['listener', 'factory', 'insert']);
      expect(factory).toHaveBeenCalledExactlyOnceWith();
      expect(set).toHaveBeenCalledExactlyOnceWith('request', pending);
      expect(operations.get('request')).toBe(pending);
      controller.abort();
      controller.abort();
      expect(abort).toHaveBeenCalledTimes(1);
    } finally { signal.removeEventListener('abort', abort); }
  });

  it('undefined signal creates then inserts without a phantom listener or return policy', () => {
    const operations = new Map<string, object>();
    const pending = { retained: true };
    const add = vi.spyOn(AbortSignal.prototype, 'addEventListener');
    const abort = vi.fn();
    const factory = vi.fn(() => pending);
    expect(registerPendingOperation(operations, 'request', undefined, abort, factory)).toBeUndefined();
    expect(factory).toHaveBeenCalledExactlyOnceWith();
    expect(operations.get('request')).toBe(pending);
    expect(add).not.toHaveBeenCalled();
    expect(abort).not.toHaveBeenCalled();
  });

  it('propagates exact listener failure after native installation without factory, insertion or unwind', () => {
    const controller = new AbortController();
    const signal = controller.signal;
    const operations = new Map([['previous', { retained: true }]]);
    const previous = operations.get('previous');
    const abort = vi.fn();
    const factory = vi.fn(() => ({ retained: false }));
    const sentinel = new Error('listener acted then threw');
    const nativeAdd = signal.addEventListener;
    const remove = vi.spyOn(signal, 'removeEventListener');
    vi.spyOn(signal, 'addEventListener').mockImplementation((type, listener, options) => {
      nativeAdd.call(signal, type, listener, options);
      throw sentinel;
    });
    let caught: unknown;
    try {
      try { registerPendingOperation(operations, 'request', signal, abort, factory); } catch (error) { caught = error; }
      expect(caught).toBe(sentinel);
      expect(factory).not.toHaveBeenCalled();
      expect([...operations]).toEqual([['previous', previous]]);
      expect(remove).not.toHaveBeenCalled();
      controller.abort(); // Earlier native installation remains effective after the exception.
      expect(abort).toHaveBeenCalledTimes(1);
    } finally { signal.removeEventListener('abort', abort); }
  });

  it('propagates exact factory failure retaining the installed listener and existing map', () => {
    const controller = new AbortController();
    const signal = controller.signal;
    const operations = new Map([['previous', { retained: true }]]);
    const previous = operations.get('previous');
    const abort = vi.fn();
    const sentinel = new Error('factory failure');
    const set = vi.spyOn(operations, 'set');
    const remove = vi.spyOn(signal, 'removeEventListener');
    let caught: unknown;
    try {
      try {
        registerPendingOperation(operations, 'request', signal, abort, () => { throw sentinel; });
      } catch (error) { caught = error; }
      expect(caught).toBe(sentinel);
      expect(set).not.toHaveBeenCalled();
      expect([...operations]).toEqual([['previous', previous]]);
      expect(remove).not.toHaveBeenCalled();
      controller.abort();
      expect(abort).toHaveBeenCalledTimes(1);
    } finally { signal.removeEventListener('abort', abort); }
  });

  it('propagates exact Map failure after insertion without rolling back the record or listener', () => {
    const controller = new AbortController();
    const signal = controller.signal;
    const operations = new Map<string, object>();
    const pending = { retained: 'inserted before throw' };
    const abort = vi.fn();
    const sentinel = new Error('Map.set acted then threw');
    const factory = vi.fn(() => pending);
    const remove = vi.spyOn(signal, 'removeEventListener');
    const set = vi.spyOn(operations, 'set').mockImplementation((key, value) => {
      Map.prototype.set.call(operations, key, value);
      throw sentinel;
    });
    let caught: unknown;
    try {
      try { registerPendingOperation(operations, 'request', signal, abort, factory); } catch (error) { caught = error; }
      expect(caught).toBe(sentinel);
      expect(factory).toHaveBeenCalledExactlyOnceWith();
      expect(set).toHaveBeenCalledExactlyOnceWith('request', pending);
      expect(operations.get('request')).toBe(pending);
      expect(remove).not.toHaveBeenCalled();
      controller.abort();
      expect(abort).toHaveBeenCalledTimes(1);
    } finally { signal.removeEventListener('abort', abort); }
  });
});
