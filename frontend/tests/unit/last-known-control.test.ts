import { get } from 'svelte/store';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { base64UrlEncode } from '$lib/base64url';
import type { RelayDeviceCredential } from '$lib/device-auth';
import { LastKnownControl, LAST_KNOWN_CONTROL_KEY } from '$lib/last-known-control';
import { LastKnownSessionCache, LAST_KNOWN_STORAGE_KEY, projectLastKnown } from '$lib/last-known';

class MemoryStorage implements Storage {
  private items = new Map<string, string>();
  get length() { return this.items.size; }
  key(index: number) { return [...this.items.keys()][index] ?? null; }
  getItem(key: string) { return this.items.get(key) ?? null; }
  setItem(key: string, value: string) { this.items.set(key, value); }
  removeItem(key: string) { this.items.delete(key); }
  clear() { this.items.clear(); }
}
const credential: RelayDeviceCredential = {
  kind: 'credential', id: 'fixture-credential', version: 1, deviceId: 'fixture-device',
  secret: base64UrlEncode(new Uint8Array(32).fill(7)), issuedAt: 1,
  role: 'controller', locale: 'en',
};
// Coordination tests do not exercise wall-clock drift. Keep cryptographic
// timestamps deterministic; rollback/expiry are covered by cache tests.
const NOW = Date.UTC(2026, 0, 1);
const notifications: unknown[] = [];
const cleanup: Array<() => void> = [];
let locksDescriptor: PropertyDescriptor | undefined;
let deliverNotifications = false;
const channels = new Set<{ onmessage?: (event: { data: unknown }) => void }>();

function tab(shared: Storage, options: Partial<ConstructorParameters<typeof LastKnownSessionCache>[0]> = {}) {
  const session = new MemoryStorage();
  const cache: LastKnownSessionCache = new LastKnownSessionCache({ storage: session, origin: 'https://app.example',
    credential: () => credential, epoch: () => control.epoch(), now: () => NOW, ...options });
  const control: LastKnownControl = new LastKnownControl(shared, cache);
  cleanup.push(control.initialize());
  cache.setLocked(false);
  return { session, control, cache };
}
const snapshot = () => projectLastKnown('local-relay', [{ name: 'sensitive-label-canary', agent: 'codex', status: 'idle' }], [], NOW);

beforeEach(() => {
  notifications.length = 0;
  deliverNotifications = false;
  channels.clear();
  locksDescriptor = Object.getOwnPropertyDescriptor(navigator, 'locks');
  let queue = Promise.resolve();
  Object.defineProperty(navigator, 'locks', { configurable: true, value: {
    request: (_name: string, callback: () => void) => {
      const result = queue.then(callback);
      queue = result.catch(() => {});
      return result;
    },
  } });
  vi.stubGlobal('BroadcastChannel', class {
    onmessage?: (event: { data: unknown }) => void;
    constructor() { channels.add(this); }
    postMessage(message: unknown) {
      notifications.push(message);
      if (deliverNotifications) {
        for (const peer of channels) {
          if (peer !== this) queueMicrotask(() => peer.onmessage?.({ data: message }));
        }
      }
    }
    close() { channels.delete(this); }
  });
});
afterEach(() => {
  for (const stop of cleanup.splice(0)) stop();
  if (locksDescriptor) Object.defineProperty(navigator, 'locks', locksDescriptor);
  else Reflect.deleteProperty(navigator, 'locks');
  vi.unstubAllGlobals();
});

describe('opaque persisted cross-tab last-known invalidation', () => {
  it('writes nothing when default off, including an invalidation before any opt-in', async () => {
    const shared = new MemoryStorage();
    const a = tab(shared);
    a.cache.write(snapshot());
    await a.control.forget();
    expect(shared.length).toBe(0);
    expect(a.session.length).toBe(0);
  });

  it('preserves a correctly tagged same-tab encrypted root on application restoration', async () => {
    const shared = new MemoryStorage();
    const a = tab(shared);
    await a.control.setEnabled(true);
    a.cache.write(snapshot());
    await vi.waitFor(() => expect(a.session.getItem(LAST_KNOWN_STORAGE_KEY)).not.toBeNull());
    const restored = tab(shared, { storage: a.session });
    await restored.cache.restore('local-relay');
    expect(get(restored.cache.summaries).size).toBe(1);
  });

  it('revalidates a suspended tab against persisted epochs without broadcasting labels or secrets', async () => {
    const shared = new MemoryStorage();
    const a = tab(shared);
    await expect(a.control.setEnabled(true)).resolves.toBe(true);
    const b = tab(shared);
    b.cache.write(snapshot());
    await vi.waitFor(() => expect(b.session.getItem(LAST_KNOWN_STORAGE_KEY)).not.toBeNull());
    await b.cache.restore('local-relay');
    expect(get(b.cache.summaries).size).toBe(1);
    // No channel callback is delivered to b: it is suspended.
    await a.control.forget();
    b.control.sync();
    b.cache.revalidate();
    expect(get(b.cache.summaries).size).toBe(0);
    await b.cache.restore('local-relay');
    expect(get(b.cache.summaries).size).toBe(0);
    expect(JSON.stringify(notifications)).not.toContain('sensitive-label-canary');
    expect(JSON.stringify(notifications)).not.toContain(credential.secret);
    expect(notifications.every((message) => JSON.stringify(message) === '{"type":"invalidate"}')).toBe(true);
    expect(shared.getItem(LAST_KNOWN_CONTROL_KEY)).not.toContain('local-relay');
  });

  it('serializes opt-out and Forget so an older control change cannot re-enable persistence', async () => {
    const shared = new MemoryStorage();
    const a = tab(shared);
    await a.control.setEnabled(true);
    const b = tab(shared);
    await Promise.all([b.control.setEnabled(false), a.control.forget()]);
    a.control.sync(); b.control.sync();
    expect(get(a.control.state).enabled).toBe(false);
    expect(get(b.control.state).enabled).toBe(false);
    expect(a.control.epoch()).toBeNull();
    expect(b.control.epoch()).toBeNull();
  });

  it.each(['write denied', 'coordination unavailable', 'lock rejected'] as const)('keeps a failed opt-out disabled after sync and recovery: %s', async (failure) => {
    const shared = new MemoryStorage();
    const a = tab(shared);
    await expect(a.control.setEnabled(true)).resolves.toBe(true);
    a.cache.write(snapshot());
    await vi.waitFor(() => expect(a.session.getItem(LAST_KNOWN_STORAGE_KEY)).not.toBeNull());
    const record = shared.getItem(LAST_KNOWN_CONTROL_KEY);
    const locks = Object.getOwnPropertyDescriptor(navigator, 'locks')!;
    const write = failure === 'write denied'
      ? vi.spyOn(shared, 'setItem').mockImplementation(() => { throw new Error('write denied'); }) : null;
    if (failure === 'coordination unavailable') Reflect.deleteProperty(navigator, 'locks');
    if (failure === 'lock rejected') Object.defineProperty(navigator, 'locks', { configurable: true,
      value: { request: async () => { throw new Error('lock rejected'); } } });
    await expect(a.control.setEnabled(false)).resolves.toBe(false);
    expect(shared.getItem(LAST_KNOWN_CONTROL_KEY)).toBe(record);
    expect(a.session.getItem(LAST_KNOWN_STORAGE_KEY)).toBeNull();
    write?.mockRestore();
    Object.defineProperty(navigator, 'locks', locks);
    // Coordination/readability recovered, but the old persisted opt-in must
    // not override this tab's failed opt-out on any notification/resume path.
    a.control.sync();
    window.dispatchEvent(new StorageEvent('storage', { key: LAST_KNOWN_CONTROL_KEY }));
    window.dispatchEvent(new Event('pageshow'));
    document.dispatchEvent(new Event('resume'));
    a.cache.write(snapshot());
    await a.cache.restore('local-relay');
    expect(get(a.control.state)).toEqual({ enabled: false, unavailable: true });
    expect(a.control.epoch()).toBeNull();
    expect(a.session.getItem(LAST_KNOWN_STORAGE_KEY)).toBeNull();
    expect(get(a.cache.summaries).size).toBe(0);
    const b = tab(shared);
    await b.control.setEnabled(true);
    a.control.sync();
    expect(a.control.epoch()).toBeNull();
    // A successful Forget retry conservatively persists off; it is not opt-in.
    await expect(a.control.forget()).resolves.toBe(true);
    expect(get(a.control.state)).toEqual({ enabled: false, unavailable: false });
    expect(JSON.parse(shared.getItem(LAST_KNOWN_CONTROL_KEY)!).enabled).toBe(false);
    await expect(a.control.setEnabled(true)).resolves.toBe(true);
    a.cache.write(snapshot());
    await vi.waitFor(() => expect(a.session.getItem(LAST_KNOWN_STORAGE_KEY)).not.toBeNull());
    expect(a.control.epoch()).not.toBeNull();
  });

  it.each(['write denied', 'lock rejected'] as const)('withdraws an already-open peer on failed opt-out and fences later sync: %s', async (failure) => {
    const shared = new MemoryStorage();
    const a = tab(shared);
    await a.control.setEnabled(true);
    const b = tab(shared);
    b.cache.write(snapshot());
    await vi.waitFor(() => expect(b.session.getItem(LAST_KNOWN_STORAGE_KEY)).not.toBeNull());
    await b.cache.restore('local-relay');
    expect(get(b.cache.summaries).size).toBe(1);
    const persisted = shared.getItem(LAST_KNOWN_CONTROL_KEY);
    const locks = Object.getOwnPropertyDescriptor(navigator, 'locks')!;
    const write = failure === 'write denied'
      ? vi.spyOn(shared, 'setItem').mockImplementation(() => { throw new Error('write denied'); }) : null;
    if (failure === 'lock rejected') Object.defineProperty(navigator, 'locks', { configurable: true,
      value: { request: async () => { throw new Error('lock rejected'); } } });
    deliverNotifications = true;
    await expect(a.control.setEnabled(false)).resolves.toBe(false);
    await vi.waitFor(() => expect(get(b.cache.summaries).size).toBe(0));
    expect(b.session.getItem(LAST_KNOWN_STORAGE_KEY)).toBeNull();
    expect(shared.getItem(LAST_KNOWN_CONTROL_KEY)).toBe(persisted);
    write?.mockRestore();
    Object.defineProperty(navigator, 'locks', locks);
    b.control.sync();
    window.dispatchEvent(new Event('pageshow'));
    b.cache.write(snapshot());
    await b.cache.restore('local-relay');
    expect(b.control.epoch()).toBeNull();
    expect(get(b.control.state)).toEqual({ enabled: false, unavailable: true });
    expect(b.session.getItem(LAST_KNOWN_STORAGE_KEY)).toBeNull();
    expect(get(b.cache.summaries).size).toBe(0);
    expect(notifications.at(-1)).toEqual({ type: 'invalidation_failed' });
    expect(JSON.stringify(notifications)).not.toContain('sensitive-label-canary');
    expect(JSON.stringify(notifications)).not.toContain(credential.secret);
    expect(JSON.stringify(notifications)).not.toContain('local-relay');
    // Success elsewhere cannot silently override the peer's failure latch.
    await a.control.forget();
    b.control.sync();
    expect(b.control.epoch()).toBeNull();
    await expect(b.control.setEnabled(true)).resolves.toBe(true);
    expect(b.control.epoch()).not.toBeNull();
  });

  it('stays locally fail-closed if failed invalidation cannot be broadcast', async () => {
    const shared = new MemoryStorage();
    const a = tab(shared);
    await a.control.setEnabled(true);
    vi.spyOn(shared, 'setItem').mockImplementation(() => { throw new Error('write denied'); });
    vi.stubGlobal('BroadcastChannel', class {
      postMessage() { throw new Error('channel unavailable'); }
      close() {}
    });
    const b = tab(shared);
    await expect(b.control.forget()).resolves.toBe(false);
    b.control.sync();
    expect(b.control.epoch()).toBeNull();
    expect(get(b.control.state)).toEqual({ enabled: false, unavailable: true });
  });

  it('does not let a sync restore enabled control during a pending opt-out', async () => {
    const shared = new MemoryStorage();
    const a = tab(shared);
    await a.control.setEnabled(true);
    let complete!: () => void;
    Object.defineProperty(navigator, 'locks', { configurable: true, value: {
      request: (_name: string, callback: () => void) => new Promise<void>((resolve) => {
        complete = () => { callback(); resolve(); };
      }),
    } });
    const optOut = a.control.setEnabled(false);
    a.control.sync();
    expect(a.control.epoch()).toBeNull();
    expect(get(a.control.state).enabled).toBe(false);
    a.cache.write(snapshot());
    expect(a.session.length).toBe(0);
    complete();
    await expect(optOut).resolves.toBe(true);
    expect(get(a.control.state)).toEqual({ enabled: false, unavailable: false });
  });

  it('fails closed with unavailable coordination or denied invalidation storage', async () => {
    const shared = new MemoryStorage();
    const a = tab(shared);
    Reflect.deleteProperty(navigator, 'locks');
    await expect(a.control.setEnabled(true)).resolves.toBe(false);
    expect(a.control.epoch()).toBeNull();
    expect(get(a.control.state)).toMatchObject({ enabled: false, unavailable: true });
    Object.defineProperty(navigator, 'locks', { configurable: true, value: {
      request: async (_name: string, callback: () => void) => callback(),
    } });
    const denied = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } } as unknown as Storage;
    const b = tab(denied);
    await expect(b.control.setEnabled(true)).resolves.toBe(false);
    expect(get(b.cache.summaries).size).toBe(0);
  });
});
