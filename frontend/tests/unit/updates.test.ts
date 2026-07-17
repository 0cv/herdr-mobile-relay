import { get } from 'svelte/store';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { APP_VERSION } from '$lib/config';
import {
  appUpdateStatus,
  checkAppUpdate,
  clearPendingRelayUpdate,
  newerVersion,
  normalizeRelayUpdate,
  pendingRelayUpdate,
  rememberPendingRelayUpdate,
  semverTuple,
} from '$lib/updates';

describe('release updates', () => {
  afterEach(() => {
    sessionStorage.clear();
    vi.restoreAllMocks();
  });

  it('compares only strict semantic versions', () => {
    expect(semverTuple('1.2.3')).toEqual([1, 2, 3]);
    expect(semverTuple('1.2')).toBeNull();
    expect(semverTuple('01.2.3')).toBeNull();
    expect(newerVersion('0.8.0', '0.7.9')).toBe(true);
    expect(newerVersion('0.7.10', '0.8.0')).toBe(false);
    expect(newerVersion('0.7.0', '0.7.0')).toBe(false);
  });

  it('normalizes relay update data without trusting arbitrary states', () => {
    expect(normalizeRelayUpdate({
      state: 'available',
      available_version: '0.8.0',
      target_revision: 'a'.repeat(40),
      can_install: true,
    }, '0.7.0', 'abc123')).toMatchObject({
      state: 'available',
      current_version: '0.7.0',
      current_revision: 'abc123',
      available_version: '0.8.0',
      can_install: true,
    });
    expect(normalizeRelayUpdate({ state: 'anything' }).state).toBe('unsupported');
  });

  it('detects a newer bundle from no-cache version metadata', async () => {
    const [major, minor, patch] = semverTuple(APP_VERSION)!;
    const available = `${major}.${minor + 1}.${patch}`;
    const fetcher = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ version: available, assets: 999 }),
    });

    const status = await checkAppUpdate(fetcher, 123);

    expect(fetcher).toHaveBeenCalledWith('/version.json?check=123', { cache: 'no-store' });
    expect(status).toMatchObject({
      state: 'available',
      availableVersion: available,
      availableAssets: 999,
      checkedAt: 123,
    });
    expect(get(appUpdateStatus).state).toBe('available');
  });

  it('keeps relay update targets across a deliberate reconnect', () => {
    rememberPendingRelayUpdate('fedora', { version: '0.8.0', revision: 'a'.repeat(40) });
    expect(pendingRelayUpdate('fedora')).toEqual({
      version: '0.8.0',
      revision: 'a'.repeat(40),
    });
    clearPendingRelayUpdate('fedora');
    expect(pendingRelayUpdate('fedora')).toBeNull();
  });
});
