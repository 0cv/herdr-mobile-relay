import { get } from 'svelte/store';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { APP_ASSET_VERSION, APP_VERSION } from '$lib/config';
import {
  appUpdateAvailable,
  beginUpdateProgress,
  appUpdateStatus,
  cacheBustedAppUrl,
  checkAppUpdate,
  clearPendingRelayUpdate,
  clearUpdateProgress,
  newerBundle,
  newerVersion,
  normalizeAppDeployment,
  normalizeReloadedAppUrl,
  normalizeRelayUpdate,
  markUpdateProgressRelayStarted,
  observeAppUpstreamVersion,
  pendingRelayUpdate,
  rememberPendingRelayUpdate,
  relayNeedsManualBootstrap,
  reloadUpdatedSameOriginApp,
  semverTuple,
  restoreUpdateProgress,
  setUpdateProgressError,
  waitForDeployedApp,
  updateProgressPlan,
} from '$lib/updates';

describe('release updates', () => {
  afterEach(() => {
    sessionStorage.clear();
    clearUpdateProgress();
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
  it('routes legacy app deployment owners through the one-time Terminal bootstrap', () => {
    const connection = {
      capabilities: ['self_update', 'app_deploy'],
      releaseVersion: '0.13.2',
      appDeploy: normalizeAppDeployment({ configured: true }),
      update: normalizeRelayUpdate({ state: 'available' }),
    };
    connection.capabilities = [];
    connection.releaseVersion = '0.13.3';
    expect(relayNeedsManualBootstrap(connection)).toBe(true);
    connection.capabilities = ['self_update', 'app_deploy'];
    connection.releaseVersion = '0.13.2';


    expect(relayNeedsManualBootstrap(connection)).toBe(true);
    connection.releaseVersion = '0.13.3';
    expect(relayNeedsManualBootstrap(connection)).toBe(false);
    connection.releaseVersion = '0.13.2';
    connection.appDeploy = normalizeAppDeployment({
      configured: false,
      reason: 'No HTTPS app deployment origin is configured',
    });
    expect(relayNeedsManualBootstrap(connection)).toBe(true);
    connection.appDeploy = normalizeAppDeployment({ configured: false });
    expect(relayNeedsManualBootstrap(
      connection,
      'deploy target app before relay: No HTTPS app deployment origin is configured',
    )).toBe(true);
    expect(relayNeedsManualBootstrap(connection, 'Release signature did not match')).toBe(false);
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
    expect(normalizeRelayUpdate({ state: 'preparing' }).state).toBe('preparing');
    expect(normalizeRelayUpdate({ state: 'deploying_app' }).state).toBe('deploying_app');
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

  it('cache-busts update reloads without dropping the current route', () => {
    const next = new URL(cacheBustedAppUrl(
      'https://app.example.test/?setup=preserved#settings',
      '0.13.8',
      42,
    ));
    expect(next.pathname).toBe('/index.html');
    expect(next.searchParams.get('setup')).toBe('preserved');
    expect(next.searchParams.get('herdr_reload')).toBe('0.13.8-42');
    expect(next.hash).toBe('#settings');
    expect(normalizeReloadedAppUrl(next.toString()))
      .toBe('https://app.example.test/?setup=preserved#settings');
    const redirected = new URL(next);
    redirected.pathname = '/';
    expect(normalizeReloadedAppUrl(redirected.toString()))
      .toBe('https://app.example.test/?setup=preserved#settings');
    expect(normalizeReloadedAppUrl('https://app.example.test/index.html#settings')).toBeNull();
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

  it('persists fleet update progress across reloads with per-relay start times', () => {
    beginUpdateProgress('1.2.3', ['fedora', 'mac'], 'fedora', 'fedora');
    const first = get(updateProgressPlan)!;
    expect(first).toMatchObject({
      targetVersion: '1.2.3',
      relayIds: ['fedora', 'mac'],
      startedRelayIds: ['fedora'],
      appRelayId: 'fedora',
      errors: {},
    });
    expect(first.relayStartedAt.fedora).toEqual(expect.any(Number));

    markUpdateProgressRelayStarted('mac');
    setUpdateProgressError('mac', new Error('network unavailable'));
    const started = get(updateProgressPlan)!;
    expect(started.startedRelayIds).toEqual(['fedora', 'mac']);
    expect(started.relayStartedAt.fedora).toBe(first.relayStartedAt.fedora);
    expect(started.relayStartedAt.mac).toEqual(expect.any(Number));
    expect(started.errors.mac).toBe('network unavailable');

    updateProgressPlan.set(null);
    restoreUpdateProgress();
    expect(get(updateProgressPlan)).toEqual(started);
  });

  it('does not poll again for an already loaded deployment target', async () => {
    const fetcher = vi.fn();
    vi.stubGlobal('fetch', fetcher);

    await expect(reloadUpdatedSameOriginApp(APP_VERSION)).resolves.toBe(false);
    expect(fetcher).not.toHaveBeenCalled();

    vi.unstubAllGlobals();
  });

  it('attempts an automatic reload target only once per browser session', async () => {
    const [major, minor, patch] = semverTuple(APP_VERSION)!;
    const target = `${major}.${minor + 1}.${patch}`;
    sessionStorage.setItem('herdr_app_reload_target', target);
    const fetcher = vi.fn();
    vi.stubGlobal('fetch', fetcher);

    // A new app instance sees the persistent deployment announcement again,
    // but the previous navigation already tried this target. It must stay put
    // instead of creating the connect/reload loop seen on the phone.
    await expect(reloadUpdatedSameOriginApp(target)).resolves.toBe(false);
    expect(fetcher).not.toHaveBeenCalled();

    vi.unstubAllGlobals();
  });

  it('waits for the deployed origin bundle to converge before reloading', async () => {
    const [major, minor, patch] = semverTuple(APP_VERSION)!;
    const target = `${major}.${minor + 1}.${patch}`;
    observeAppUpstreamVersion(target);
    const converged = {
      ok: true,
      json: async () => ({ version: target, assets: APP_ASSET_VERSION + 1 }),
    };
    const fetcher = vi.fn()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ version: APP_VERSION, assets: APP_ASSET_VERSION }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ version: APP_VERSION, assets: APP_ASSET_VERSION }),
      })
      // The converging poll, then the one status check that publishes it.
      .mockResolvedValueOnce(converged)
      .mockResolvedValueOnce(converged);
    const sleep = vi.fn().mockResolvedValue(undefined);
    const states: string[] = [];
    const unsubscribe = appUpdateStatus.subscribe((status) => states.push(status.state));

    const status = await waitForDeployedApp(target, {
      fetcher,
      attempts: 3,
      intervalMs: 0,
      sleep,
    });
    unsubscribe();

    expect(status).toMatchObject({ state: 'reload-ready', deployedVersion: target });
    expect(fetcher).toHaveBeenCalledTimes(4);
    expect(sleep).toHaveBeenCalledTimes(2);
    // Polling stays silent: only the final landing may publish a check, so a
    // stale deployment target cannot flicker the update status once a second.
    expect(states.filter((state) => state === 'checking')).toHaveLength(1);
  });

  it('gives up silently when the origin never serves the target', async () => {
    const [major, minor, patch] = semverTuple(APP_VERSION)!;
    const target = `${major}.${minor + 2}.${patch}`;
    const fetcher = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ version: APP_VERSION, assets: APP_ASSET_VERSION }),
    });
    const states: string[] = [];
    const unsubscribe = appUpdateStatus.subscribe((status) => states.push(status.state));

    const status = await waitForDeployedApp(target, {
      fetcher,
      attempts: 5,
      intervalMs: 0,
      sleep: vi.fn().mockResolvedValue(undefined),
    });
    unsubscribe();

    expect(status).toBeNull();
    expect(fetcher).toHaveBeenCalledTimes(5);
    expect(states.filter((state) => state === 'checking')).toHaveLength(0);
  });

});
