import { get } from 'svelte/store';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { APP_ASSET_VERSION, APP_VERSION } from '$lib/config';
import {
  appUpdateAvailable,
  appUpdateStatus,
  checkAppUpdate,
  clearPendingAppDeploy,
  clearPendingRelayUpdate,
  newerBundle,
  newerVersion,
  normalizeAppDeployment,
  normalizeRelayUpdate,
  observeAppUpstreamVersion,
  pendingAppDeploy,
  pendingRelayUpdate,
  rememberPendingAppDeploy,
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

  it('treats a same-version asset bump as an available update', () => {
    expect(appUpdateAvailable({ version: APP_VERSION, assets: APP_ASSET_VERSION + 1 })).toBe(true);
    expect(appUpdateAvailable({ version: APP_VERSION, assets: APP_ASSET_VERSION })).toBe(false);
    const [major, minor, patch] = semverTuple(APP_VERSION)!;
    expect(appUpdateAvailable({ version: `${major}.${minor + 1}.${patch}`, assets: 0 })).toBe(true);
  });

  it('newerBundle compares version first then assets', () => {
    expect(newerBundle({ version: '0.9.0', assets: 0 }, { version: '0.8.0', assets: 99 })).toBe(true);
    expect(newerBundle({ version: '0.8.0', assets: 5 }, { version: '0.8.0', assets: 4 })).toBe(true);
    expect(newerBundle({ version: '0.8.0', assets: 4 }, { version: '0.8.0', assets: 4 })).toBe(false);
    expect(newerBundle({ version: '0.7.0', assets: 99 }, { version: '0.8.0', assets: 0 })).toBe(false);
  });

  it('does not let stale relay metadata downgrade the running app version', () => {
    expect(get(appUpdateStatus).upstreamVersion).toBe(APP_VERSION);
    observeAppUpstreamVersion('0.0.1');
    expect(get(appUpdateStatus).upstreamVersion).toBe(APP_VERSION);
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

  it('combines no-cache origin metadata with credential-safe relay release metadata', async () => {
    const [major, minor, patch] = semverTuple(APP_VERSION)!;
    const available = `${major}.${minor + 1}.${patch}`;
    const fetcher = vi.fn().mockImplementation(async () => ({
      ok: true,
      json: async () => ({ version: APP_VERSION, assets: 68 }),
    }));

    await checkAppUpdate(fetcher, 123);
    observeAppUpstreamVersion(available);
    const status = get(appUpdateStatus);

    expect(fetcher).toHaveBeenCalledWith('/version.json?check=123', { cache: 'no-store' });
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(status).toMatchObject({
      state: 'deployment-required',
      deployedVersion: APP_VERSION,
      upstreamVersion: available,
      upstreamAssets: 0,
      checkedAt: 123,
    });
    expect(get(appUpdateStatus).state).toBe('deployment-required');
  });

  it('offers reload for an assets-only deploy at the same version', async () => {
    const fetcher = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ version: APP_VERSION, assets: APP_ASSET_VERSION + 1 }),
    });

    expect(await checkAppUpdate(fetcher, 126)).toMatchObject({
      state: 'reload-ready',
      deployedVersion: APP_VERSION,
      deployedAssets: APP_ASSET_VERSION + 1,
    });
  });

  it('only offers reload after the app origin has published the upstream bundle', async () => {
    const [major, minor, patch] = semverTuple(APP_VERSION)!;
    const available = `${major}.${minor + 1}.${patch}`;
    const fetcher = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ version: available, assets: 999 }),
    });

    expect(await checkAppUpdate(fetcher, 124)).toMatchObject({
      state: 'reload-ready',
      deployedVersion: available,
      upstreamVersion: available,
    });
  });

  it('reloads a newer origin bundle without browser access to private GitHub', async () => {
    const [major, minor, patch] = semverTuple(APP_VERSION)!;
    const available = `${major}.${minor + 1}.${patch}`;
    const fetcher = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ version: available, assets: 999 }),
    });

    expect(await checkAppUpdate(fetcher, 125)).toMatchObject({
      state: 'reload-ready',
      deployedVersion: available,
      upstreamVersion: available,
      error: '',
    });
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it('normalizes app deployment metadata without exposing unknown fields', () => {
    expect(normalizeAppDeployment({
      configured: true,
      origin: 'https://app.example.test',
      project: 'herdr-app',
      branch: 'main',
      revision: 'f'.repeat(40),
      state: 'deploying',
      secret: 'do-not-copy',
    })).toEqual(expect.objectContaining({
      configured: true,
      origin: 'https://app.example.test',
      state: 'deploying',
    }));
    expect(normalizeAppDeployment({ state: 'anything' }).state).toBe('idle');
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

  it('remembers a queued app deploy across a relay update reconnect', () => {
    rememberPendingAppDeploy('fedora', '0.8.3');
    expect(pendingAppDeploy('fedora')).toBe('0.8.3');
    expect(pendingAppDeploy('other')).toBeNull();
    clearPendingAppDeploy('fedora');
    expect(pendingAppDeploy('fedora')).toBeNull();
  });
});
