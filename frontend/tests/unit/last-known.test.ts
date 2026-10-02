import { get } from 'svelte/store';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { base64UrlEncode } from '$lib/base64url';
import type { RelayDeviceCredential } from '$lib/device-auth';
import { decryptLastKnown, deriveLastKnownAssociation, encryptLastKnown } from '$lib/last-known-crypto';
import { LAST_KNOWN_STORAGE_KEY, LastKnownSessionCache, projectLastKnown, validateLastKnown } from '$lib/last-known';
import {
  LAST_KNOWN_MAX_AGE_MS, LAST_KNOWN_MAX_PLAINTEXT_BYTES, LAST_KNOWN_MAX_STORED_BYTES,
  type LastKnownSummary,
} from '$lib/last-known-types';

const NOW = 1_800_000_000_000;
const credential: RelayDeviceCredential = {
  kind: 'credential', id: 'fixture-credential', version: 1, deviceId: 'fixture-device',
  secret: base64UrlEncode(new Uint8Array(32).fill(17)), role: 'reader', locale: 'en', issuedAt: NOW - 100,
};
const caches: LastKnownSessionCache[] = [];

function summary(relayId = 'local-relay', label = 'Safe label', at = NOW): LastKnownSummary {
  return projectLastKnown(relayId, [{ name: label, agent: 'codex', status: 'idle', workspace_id: 'remote-workspace' }],
    [{ workspace_id: 'remote-workspace', label: 'Safe workspace' }], at);
}

function fixture(options: Partial<ConstructorParameters<typeof LastKnownSessionCache>[0]> = {}) {
  let epoch: string | null = 'A'.repeat(43);
  let current: RelayDeviceCredential | null = credential;
  let now = NOW;
  const cache = new LastKnownSessionCache({
    storage: sessionStorage, origin: 'https://app.example', epoch: () => epoch,
    credential: () => current, now: () => now, ...options,
  });
  caches.push(cache);
  return {
    cache, unlock: () => { cache.setEnabled(true); cache.setLocked(false); },
    rotate: () => { epoch = 'B'.repeat(42) + 'A'; },
    unavailableEpoch: () => { epoch = null; },
    replaceCredential: () => { current = { ...credential, version: 2 }; },
    revoke: () => { current = null; },
    advance: (ms: number) => { now += ms; },
  };
}

async function stored(cache: LastKnownSessionCache, value = summary()): Promise<void> {
  cache.write(value);
  await vi.waitFor(() => expect(sessionStorage.getItem(LAST_KNOWN_STORAGE_KEY)).not.toBeNull());
}

async function opaqueId(relayId: string): Promise<string> {
  return deriveLastKnownAssociation({ origin: 'https://app.example', relayId, credential, lastFreshAt: NOW }, relayId, crypto, NOW);
}

async function encrypted(value: LastKnownSummary): Promise<string> {
  const relayId = await opaqueId(value.relayId);
  return encryptLastKnown(JSON.stringify({ ...value, relayId }), {
    origin: 'https://app.example', relayId, credential, lastFreshAt: value.lastFreshAt,
  }, crypto, NOW);
}

afterEach(() => {
  for (const cache of caches.splice(0)) cache.dispose();
  sessionStorage.clear();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('allowlisted last-known summary projection', () => {
  it('does not spread live objects or fall back to cwd/project/session labels', () => {
    const canary = 'excluded-secret-path-prompt-canary';
    const projected = projectLastKnown('local-relay', [{
      agent: 'codex', status: 'idle', cwd: canary, foreground_cwd: canary, project: canary, session_name: canary,
      pane_id: canary, terminal_id: canary, server_session_id: canary, agent_session_id: canary,
      session: canary, command: canary, prompt: canary, interaction: { prompt: canary },
      options: [canary], conversation: canary, attachments: [canary], process: canary,
      capabilities: [canary], action_receipt: canary, event_ref: canary, token: canary,
      workspace_id: canary,
    }], [{ workspace_id: canary, cwd: canary, worktree: { repo_root: canary } }], NOW);
    expect(JSON.stringify(projected)).not.toContain(canary);
    expect(projected.groups).toEqual([{ id: 'g0', label: 'Workspace' }]);
    expect(projected.rows).toEqual([{ id: 'r0', group: 'g0', label: 'Agent', type: 'codex', status: 'idle' }]);
    expect(validateLastKnown(projected, 'local-relay', NOW)).toEqual(projected);
  });

  it('bounds Unicode code points, UTF-8 bytes, rows, groups and total plaintext with explicit truncation', () => {
    const projected = projectLastKnown('local-relay', Array.from({ length: 250 }, () => ({
      name: '🐑'.repeat(121), agent: 'not-allowlisted-secret', status: 'not-allowlisted-secret',
    })), Array.from({ length: 60 }, () => ({ label: '🐑'.repeat(121) })), NOW);
    expect(projected.truncated).toBe(true);
    expect(projected.groups).toHaveLength(50);
    expect(projected.rows.length).toBeLessThanOrEqual(200);
    expect(new TextEncoder().encode(JSON.stringify(projected)).byteLength).toBeLessThanOrEqual(LAST_KNOWN_MAX_PLAINTEXT_BYTES);
    for (const row of projected.rows) {
      expect(Array.from(row.label)).toHaveLength(120);
      expect(new TextEncoder().encode(row.label).byteLength).toBe(480);
      expect(row.type).toBe('other');
      expect(row.status).toBe('unknown');
    }
    expect(validateLastKnown(projected, 'local-relay', NOW)).not.toBeNull();
  });

  it('rejects malformed schemas, forbidden fields, remote IDs and unsafe labels as a whole', () => {
    const valid = summary();
    for (const value of [
      { ...valid, schema: 2 }, { ...valid, cwd: '/secret' }, { ...valid, lastFreshAt: NOW + 1 },
      { ...valid, relayId: 'wrong' }, { ...valid, truncated: 'false' },
      { ...valid, groups: Array(51).fill(valid.groups[0]) }, { ...valid, rows: Array(201).fill(valid.rows[0]) },
      { ...valid, rows: [{ ...valid.rows[0], id: 'remote-pane-id' }] },
      { ...valid, rows: [{ ...valid.rows[0], group: 'remote-workspace-id' }] },
      { ...valid, rows: [{ ...valid.rows[0], label: '\ud800' }] },
      { ...valid, rows: [{ ...valid.rows[0], label: 'x'.repeat(121) }] },
      { ...valid, rows: [{ ...valid.rows[0], prompt: 'secret' }] },
    ]) expect(validateLastKnown(value, 'local-relay', NOW)).toBeNull();
  });
});

describe('failure-tolerant session cache foundation', () => {
  it('is default off, locked and writes nothing', async () => {
    const { cache } = fixture();
    const encrypt = vi.spyOn(crypto.subtle, 'encrypt');
    cache.write(summary());
    await cache.restore('local-relay');
    expect(sessionStorage.getItem(LAST_KNOWN_STORAGE_KEY)).toBeNull();
    expect(encrypt).not.toHaveBeenCalled();
    expect(get(cache.summaries).size).toBe(0);
  });

  it('round-trips ciphertext only and restores on the same session without a network dependency', async () => {
    const first = fixture();
    first.unlock();
    await stored(first.cache);
    const raw = sessionStorage.getItem(LAST_KNOWN_STORAGE_KEY)!;
    expect(raw).not.toContain('Safe label');
    expect(raw).not.toContain('Safe workspace');
    expect(raw).not.toContain(credential.secret);
    const restored = fixture();
    restored.unlock();
    await restored.cache.restore('local-relay');
    expect(get(restored.cache.summaries).get('local-relay')).toEqual({ ...summary(), relayId: await opaqueId('local-relay') });
    expect(raw).not.toContain('local-relay');
    sessionStorage.removeItem(LAST_KNOWN_STORAGE_KEY);
    const missing = fixture();
    missing.unlock();
    await missing.cache.restore('local-relay');
    expect(get(missing.cache.summaries).size).toBe(0);
  });

  it('does not expose legacy descriptive relay IDs and binds the full configuration identity', async () => {
    const relayId = 'sensitive-computer-label-and-host-example';
    const writer = fixture({ association: () => 'configuration-a' }); writer.unlock();
    await stored(writer.cache, summary(relayId));
    expect(sessionStorage.getItem(LAST_KNOWN_STORAGE_KEY)).not.toContain(relayId);
    expect(sessionStorage.getItem(LAST_KNOWN_STORAGE_KEY)).not.toContain('configuration-a');
    const reader = fixture({ association: () => 'configuration-b' }); reader.unlock();
    await reader.cache.restore(relayId);
    expect(get(reader.cache.summaries).size).toBe(0);
  });

  it('fences a suspended restore even after bounded scope-marker eviction, retaining the last-good blob', async () => {
    const writer = fixture(); writer.unlock(); await stored(writer.cache);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const decrypt = vi.fn(async (...args: Parameters<typeof decryptLastKnown>) => { await gate; return decryptLastKnown(...args); });
    const reader = fixture({ decrypt }); reader.unlock();
    const restoring = reader.cache.restore('local-relay');
    await vi.waitFor(() => expect(decrypt).toHaveBeenCalledOnce());
    reader.cache.invalidateScope('local-relay');
    for (let index = 0; index < 20; index++) reader.cache.invalidateScope(`other-${index}`);
    release(); await restoring;
    expect(get(reader.cache.summaries).size).toBe(0);
    expect(sessionStorage.getItem(LAST_KNOWN_STORAGE_KEY)).not.toBeNull();
    await reader.cache.restore('local-relay');
    expect(get(reader.cache.summaries).size).toBe(1);
  });

  it('coalesces explicit restoration behind an invalidated decrypt without publishing its stale result', async () => {
    const writer = fixture(); writer.unlock(); await stored(writer.cache);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const decrypt = vi.fn(async (...args: Parameters<typeof decryptLastKnown>) => { await gate; return decryptLastKnown(...args); });
    const reader = fixture({ decrypt }); reader.unlock();
    const restoring = reader.cache.restore('local-relay');
    await vi.waitFor(() => expect(decrypt).toHaveBeenCalledOnce());
    reader.cache.invalidateScope('local-relay');
    for (let index = 0; index < 20; index++) await reader.cache.restore('local-relay');
    expect(get(reader.cache.summaries).size).toBe(0);
    release(); await restoring;
    expect(decrypt).toHaveBeenCalledTimes(2);
    expect(get(reader.cache.summaries).size).toBe(1);
  });

  it('a queued restoration still cannot publish after Forget', async () => {
    const writer = fixture(); writer.unlock(); await stored(writer.cache);
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const decrypt = vi.fn(async (...args: Parameters<typeof decryptLastKnown>) => { await gate; return decryptLastKnown(...args); });
    const reader = fixture({ decrypt }); reader.unlock();
    const restoring = reader.cache.restore('local-relay');
    await vi.waitFor(() => expect(decrypt).toHaveBeenCalledOnce());
    reader.cache.invalidateScope('local-relay');
    await reader.cache.restore('local-relay');
    reader.rotate(); reader.cache.forget();
    release(); await restoring;
    expect(decrypt).toHaveBeenCalledOnce();
    expect(get(reader.cache.summaries).size).toBe(0);
  });

  it('clears an already visible summary when the stored root becomes invalid', async () => {
    const f = fixture(); f.unlock();
    await stored(f.cache);
    await f.cache.restore('local-relay');
    expect(get(f.cache.summaries).size).toBe(1);
    sessionStorage.setItem(LAST_KNOWN_STORAGE_KEY, '{malformed');
    f.cache.revalidate();
    expect(get(f.cache.summaries).size).toBe(0);
    expect(get(f.cache.availability).unavailable).toBe(true);
  });

  it('expires while mounted and after suspension without extending retention', async () => {
    const value = summary();
    const raw = await encrypted(value);
    sessionStorage.setItem(LAST_KNOWN_STORAGE_KEY, JSON.stringify({ schema: 1, epoch: 'A'.repeat(43), lastObservedAt: NOW, entries: [raw] }));
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
    const { cache, unlock } = fixture({ now: () => Date.now() });
    unlock();
    await cache.restore('local-relay');
    expect(get(cache.summaries).size).toBe(1);
    vi.advanceTimersByTime(LAST_KNOWN_MAX_AGE_MS);
    expect(get(cache.summaries).size).toBe(0);
    await cache.restore('local-relay');
    expect(get(cache.summaries).size).toBe(0);
  });

  it('clears visible references and refuses old data on clock rollback', async () => {
    const f = fixture();
    f.unlock();
    await stored(f.cache);
    await f.cache.restore('local-relay');
    f.advance(-1);
    f.cache.revalidate();
    expect(get(f.cache.summaries).size).toBe(0);
    f.advance(1);
    await f.cache.restore('local-relay');
    expect(get(f.cache.summaries).size).toBe(0);
  });

  it.each(['lock', 'forget', 'opt-out', 'credential', 'epoch', 'revocation'] as const)(
    'cannot publish delayed decrypt after %s', async (event) => {
      let release!: () => void;
      const gate = new Promise<void>((resolve) => { release = resolve; });
      const value = summary();
      const raw = await encrypted(value);
      sessionStorage.setItem(LAST_KNOWN_STORAGE_KEY, JSON.stringify({ schema: 1, epoch: 'A'.repeat(43), lastObservedAt: NOW, entries: [raw] }));
      const decrypt = vi.fn(async (...args: Parameters<typeof decryptLastKnown>) => {
        await gate; return decryptLastKnown(...args);
      });
      const f = fixture({ decrypt });
      f.unlock();
      const restoring = f.cache.restore('local-relay');
      await vi.waitFor(() => expect(decrypt).toHaveBeenCalledOnce());
      if (event === 'lock') f.cache.setLocked(true);
      if (event === 'forget') { f.rotate(); f.cache.forget(); }
      if (event === 'opt-out') { f.rotate(); f.cache.setEnabled(false); }
      if (event === 'credential') f.replaceCredential();
      if (event === 'epoch') f.rotate();
      if (event === 'revocation') f.revoke();
      release();
      await restoring;
      expect(get(f.cache.summaries).size).toBe(0);
    },
  );

  it.each(['forget', 'opt-out', 'lock', 'epoch', 'credential'] as const)(
    'cannot store a late encryption after %s', async (event) => {
      let release!: () => void;
      const gate = new Promise<void>((resolve) => { release = resolve; });
      const raw = await encrypted(summary());
      const encrypt = vi.fn(async () => { await gate; return raw; });
      const f = fixture({ encrypt });
      f.unlock();
      f.cache.write(summary());
      await vi.waitFor(() => expect(encrypt).toHaveBeenCalledOnce());
      if (event === 'forget') { f.rotate(); f.cache.forget(); }
      if (event === 'opt-out') { f.rotate(); f.cache.setEnabled(false); }
      if (event === 'lock') f.cache.setLocked(true);
      if (event === 'epoch') f.rotate();
      if (event === 'credential') f.replaceCredential();
      release();
      await vi.waitFor(() => expect((f.cache as unknown as { draining: boolean }).draining).toBe(false));
      expect(sessionStorage.getItem(LAST_KNOWN_STORAGE_KEY)).toBeNull();
    },
  );

  it('coalesces a busy inventory to one active encryption and the latest pending snapshot', async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const encrypt = vi.fn(async (plaintext: string, context: Parameters<typeof encryptLastKnown>[1]) => {
      await gate;
      return encryptLastKnown(plaintext, context, crypto, NOW);
    });
    const f = fixture({ encrypt });
    f.unlock();
    f.cache.write(summary('local-relay', 'first'));
    await vi.waitFor(() => expect(encrypt).toHaveBeenCalledOnce());
    for (let index = 0; index < 100; index++) f.cache.write(summary('local-relay', `newest-${index}`));
    release();
    await vi.waitFor(() => expect(sessionStorage.getItem(LAST_KNOWN_STORAGE_KEY)).not.toBeNull());
    expect(encrypt).toHaveBeenCalledTimes(2);
    await f.cache.restore('local-relay');
    expect(get(f.cache.summaries).get('local-relay')?.rows[0].label).toBe('newest-99');
  });

  it('evicts oldest entries at ten relays and counts encoded/envelope overhead', async () => {
    const f = fixture();
    f.unlock();
    for (let index = 0; index < 11; index++) {
      f.cache.write(summary(`local-${index}`, 'label', NOW - 11 + index));
      const association = await opaqueId(`local-${index}`);
      await vi.waitFor(() => {
        const raw = sessionStorage.getItem(LAST_KNOWN_STORAGE_KEY);
        expect(raw).not.toBeNull();
        expect(JSON.parse(raw!).entries.some((entry: string) => JSON.parse(entry).relayId === association)).toBe(true);
      });
    }
    const raw = sessionStorage.getItem(LAST_KNOWN_STORAGE_KEY)!;
    expect(new TextEncoder().encode(raw).byteLength).toBeLessThanOrEqual(LAST_KNOWN_MAX_STORED_BYTES);
    const entries = JSON.parse(raw).entries as string[];
    expect(entries).toHaveLength(10);
    const evicted = await opaqueId('local-0');
    expect(entries.some((entry) => JSON.parse(entry).relayId === evicted)).toBe(false);
  });

  it('revalidates persisted opaque invalidation state before showing a suspended tab copy', async () => {
    const f = fixture();
    f.unlock();
    await stored(f.cache);
    await f.cache.restore('local-relay');
    expect(get(f.cache.summaries).size).toBe(1);
    f.rotate();
    f.cache.revalidate();
    expect(get(f.cache.summaries).size).toBe(0);
    await f.cache.restore('local-relay');
    expect(get(f.cache.summaries).size).toBe(0);
    f.unavailableEpoch();
    f.cache.write(summary());
    expect(sessionStorage.getItem(LAST_KNOWN_STORAGE_KEY)).toBeNull();
  });

  it('treats denied/quota storage and crypto failures as unavailability, never plaintext fallback', async () => {
    const denied = {
      getItem() { throw new Error('denied'); },
      setItem() { throw new Error('quota'); },
      removeItem() { throw new Error('denied'); },
    } as unknown as Storage;
    const f = fixture({ storage: denied });
    f.unlock();
    expect(() => f.cache.write(summary())).not.toThrow();
    await f.cache.restore('local-relay');
    expect(get(f.cache.summaries).size).toBe(0);
    f.cache.forget();
    expect(get(f.cache.availability)).toEqual({ unavailable: true, persistenceUncertain: true });
    const unavailableCrypto = fixture({ crypto: {} as Crypto });
    unavailableCrypto.unlock();
    unavailableCrypto.cache.write(summary());
    await vi.waitFor(() => expect(get(unavailableCrypto.cache.availability).unavailable).toBe(true));
    expect(sessionStorage.getItem(LAST_KNOWN_STORAGE_KEY)).toBeNull();
  });
});
