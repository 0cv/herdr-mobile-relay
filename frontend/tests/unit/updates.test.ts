import { get } from 'svelte/store';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { APP_ASSET_VERSION, APP_BUILD_ID, APP_VERSION } from '$lib/config';
import {
  acknowledgePhoneUpdate,
  appUpdateAvailable,
  beginUpdateProgress,
  appUpdateStatus,
  cacheBustedAppUrl,
  checkAppUpdate,
  initializeAppUpdates,
  clearPendingRelayUpdate,
  clearUpdateProgress,
  newerBundle,
  newerVersion,
  normalizeAppDeployment,
  normalizeReloadedAppUrl,
  normalizeRelayUpdate,
  markUpdateProgressRelayStarted,
  queueUpdateProgressForReload,
  observeAppUpstreamVersion,
  pendingRelayUpdate,
  phoneTargetMatchesCurrent,
  rememberPendingRelayUpdate,
  relayNeedsManualBootstrap,
  reloadUpdatedSameOriginApp,
  semverTuple,
  restoreUpdateProgress,
  setPhoneUpdateError,
  setPhoneUpdateTarget,
  setUpdateProgressError,
  waitForDeployedApp,
  updateProgressPlan,
} from '$lib/updates';

describe('release updates', () => {
  afterEach(() => {
    sessionStorage.clear();
    clearUpdateProgress();
    delete document.documentElement.dataset.herdrLoadFailed;
    delete document.documentElement.dataset.herdrLoadTimedOut;
    delete document.documentElement.dataset.herdrCssReady;
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

  it('newerBundle compares version, assets, and same-version build identity', () => {
    expect(newerBundle({ version: '0.9.0', assets: 0 }, { version: '0.8.0', assets: 99 })).toBe(true);
    expect(newerBundle({ version: '0.8.0', assets: 5 }, { version: '0.8.0', assets: 4 })).toBe(true);
    expect(newerBundle({ version: '0.8.0', assets: 4 }, { version: '0.8.0', assets: 4 })).toBe(false);
    expect(newerBundle({ version: '0.8.0', assets: 4, build: 'new' }, { version: '0.8.0', assets: 4, build: 'old' })).toBe(true);
    expect(newerBundle({ version: '0.8.0', assets: 4, build: 'same' }, { version: '0.8.0', assets: 4, build: 'same' })).toBe(false);
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

  it('offers a reload for a distinct same-version build', async () => {
    const fetcher = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ version: APP_VERSION, assets: APP_ASSET_VERSION, build: 'different-build' }),
    });

    expect(await checkAppUpdate(fetcher, 127)).toMatchObject({
      state: 'reload-ready',
      deployedVersion: APP_VERSION,
      deployedBuild: 'different-build',
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
    const canonicalEntry = new URL(next);
    canonicalEntry.pathname = '/builds/0.20.10-363-build/';
    expect(normalizeReloadedAppUrl(canonicalEntry.toString()))
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

  it('tracks the phone independently from its deployment owner', () => {
    beginUpdateProgress('1.2.3', ['fedora', 'mac'], 'fedora', 'fedora', {
      phoneAppRequired: true,
      phoneTarget: { version: '1.2.3', assets: 7, build: 'new-build' },
      phoneState: 'publishing',
    });
    expect(get(updateProgressPlan)).toMatchObject({
      appRelayId: 'fedora',
      phoneAppRequired: true,
      phoneTarget: { version: '1.2.3', assets: 7, build: 'new-build' },
      phoneState: 'publishing',
      phoneAcknowledged: false,
    });

    updateProgressPlan.set(null);
    restoreUpdateProgress();
    expect(get(updateProgressPlan)).toMatchObject({ phoneAppRequired: true, phoneState: 'publishing' });

    beginUpdateProgress('1.2.3', ['fedora'], 'fedora');
    expect(get(updateProgressPlan)).toMatchObject({ phoneAppRequired: false, phoneTarget: null });
  });

  it('accepts a compatible newer running app for an older phone target', () => {
    expect(phoneTargetMatchesCurrent({ version: '0.0.1', assets: 1, build: '' })).toBe(true);
    expect(phoneTargetMatchesCurrent({ version: APP_VERSION, assets: APP_ASSET_VERSION - 1, build: '' })).toBe(true);
    expect(phoneTargetMatchesCurrent({ version: APP_VERSION, assets: APP_ASSET_VERSION, build: '' })).toBe(false);
  });

  it('acknowledges only the exact running phone build', () => {
    queueUpdateProgressForReload(APP_VERSION, [], {
      version: APP_VERSION,
      assets: APP_ASSET_VERSION,
      build: APP_BUILD_ID,
    });
    expect(acknowledgePhoneUpdate()).toBe(true);
    expect(get(updateProgressPlan)).toMatchObject({ phoneState: 'loaded', phoneAcknowledged: true });

    queueUpdateProgressForReload(APP_VERSION, [], {
      version: APP_VERSION,
      assets: APP_ASSET_VERSION,
      build: 'different-build',
    });
    expect(acknowledgePhoneUpdate()).toBe(false);
    setPhoneUpdateTarget({ version: APP_VERSION, assets: APP_ASSET_VERSION, build: 'different-build' });
    setPhoneUpdateError(new Error('integrity check failed'));
    expect(get(updateProgressPlan)).toMatchObject({ phoneState: 'failed', phoneAcknowledged: false, phoneError: 'integrity check failed' });
  });

  it('does not trust a bootstrap readiness hint for an unloaded stylesheet', () => {
    queueUpdateProgressForReload(APP_VERSION, [], {
      version: APP_VERSION,
      assets: APP_ASSET_VERSION,
      build: APP_BUILD_ID,
    });
    const stylesheet = document.createElement('link');
    stylesheet.rel = 'stylesheet';
    stylesheet.href = '/assets/app-test.css';
    document.head.append(stylesheet);

    document.documentElement.dataset.herdrCssReady = '1';
    expect(acknowledgePhoneUpdate()).toBe(false);
    stylesheet.remove();
  });

  it('does not acknowledge a phone update after a required stylesheet failure', () => {
    queueUpdateProgressForReload(APP_VERSION, [], {
      version: APP_VERSION,
      assets: APP_ASSET_VERSION,
      build: APP_BUILD_ID,
    });
    document.documentElement.dataset.herdrLoadFailed = '1';

    expect(acknowledgePhoneUpdate()).toBe(false);
    expect(get(updateProgressPlan)).toMatchObject({ phoneState: 'loading', phoneAcknowledged: false });
    const stop = initializeAppUpdates();
    stop();
    expect(get(updateProgressPlan)).toMatchObject({ phoneState: 'failed', phoneAcknowledged: false });
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

  it('accepts a newer asset revision without requiring its unrelated build identity', async () => {
    const target = {
      version: APP_VERSION,
      assets: APP_ASSET_VERSION,
      build: 'old-build',
    };
    const deployed = {
      ok: true,
      json: async () => ({
        version: APP_VERSION,
        assets: APP_ASSET_VERSION + 1,
        build: 'new-build',
      }),
    };
    const fetcher = vi.fn().mockResolvedValue(deployed);

    const status = await waitForDeployedApp(APP_VERSION, {
      fetcher,
      target,
      attempts: 1,
      intervalMs: 0,
    });

    expect(status).toMatchObject({
      deployedAssets: APP_ASSET_VERSION + 1,
      deployedBuild: 'new-build',
    });
  });

  it('revalidates the final metadata response before publishing a reload target', async () => {
    const [major, minor, patch] = semverTuple(APP_VERSION)!;
    const targetVersion = `${major}.${minor + 1}.${patch}`;
    const target = { version: targetVersion, assets: 12, build: 'target-build' };
    const response = (version: string, assets: number, build: string) => ({
      ok: true,
      json: async () => ({ version, assets, build }),
    });
    const fetcher = vi.fn()
      .mockResolvedValueOnce(response(targetVersion, target.assets, target.build))
      .mockResolvedValueOnce(response(APP_VERSION, APP_ASSET_VERSION, APP_BUILD_ID))
      .mockResolvedValueOnce(response(targetVersion, target.assets, target.build))
      .mockResolvedValueOnce(response(targetVersion, target.assets, target.build));

    const status = await waitForDeployedApp(targetVersion, {
      fetcher,
      target,
      attempts: 2,
      intervalMs: 0,
      sleep: vi.fn().mockResolvedValue(undefined),
    });

    expect(status).toMatchObject({ deployedVersion: targetVersion, deployedBuild: target.build });
    expect(fetcher).toHaveBeenCalledTimes(4);
  });

  it('does not restore a stale verification snapshot over a newer app check', async () => {
    const [major, minor, patch] = semverTuple(APP_VERSION)!;
    const targetVersion = `${major}.${minor + 1}.${patch}`;
    const response = (version: string) => ({
      ok: true,
      json: async () => ({ version, assets: APP_ASSET_VERSION, build: APP_BUILD_ID }),
    });
    let releaseFinal: (value: ReturnType<typeof response>) => void = () => {};
    const finalResponse = new Promise<ReturnType<typeof response>>((resolve) => { releaseFinal = resolve; });
    const verificationFetcher = vi.fn()
      .mockResolvedValueOnce(response(targetVersion))
      .mockImplementationOnce(() => finalResponse);
    const verification = waitForDeployedApp(targetVersion, {
      fetcher: verificationFetcher,
      target: { version: targetVersion, assets: APP_ASSET_VERSION, build: APP_BUILD_ID },
      attempts: 1,
      intervalMs: 0,
    });
    await new Promise((resolve) => setTimeout(resolve, 0));

    const checkFetcher = vi.fn().mockResolvedValue(response(APP_VERSION));
    await checkAppUpdate(checkFetcher, 123);
    releaseFinal(response(APP_VERSION));
    await verification;

    expect(get(appUpdateStatus).checkedAt).toBe(123);
    expect(get(appUpdateStatus).state).not.toBe('checking');
  });

  it('does not restore a stale verification snapshot after its deadline', async () => {
    const [major, minor, patch] = semverTuple(APP_VERSION)!;
    const targetVersion = `${major}.${minor + 1}.${patch}`;
    const response = (version: string) => ({
      ok: true,
      json: async () => ({ version, assets: APP_ASSET_VERSION, build: APP_BUILD_ID }),
    });
    const verificationFetcher = vi.fn()
      .mockResolvedValueOnce(response(targetVersion))
      .mockImplementationOnce(() => new Promise(() => {}));
    const verification = waitForDeployedApp(targetVersion, {
      fetcher: verificationFetcher,
      target: { version: targetVersion, assets: APP_ASSET_VERSION, build: APP_BUILD_ID },
      attempts: 1,
      intervalMs: 0,
      deadlineMs: 25,
    });
    await new Promise((resolve) => setTimeout(resolve, 0));

    const checkFetcher = vi.fn().mockResolvedValue(response(APP_VERSION));
    await checkAppUpdate(checkFetcher, 456);
    await verification;

    expect(get(appUpdateStatus).checkedAt).toBe(456);
    expect(get(appUpdateStatus).state).not.toBe('checking');
  });

  it('does not downgrade a persisted phone target from a stale response', () => {
    queueUpdateProgressForReload('1.2.3', [], { version: '1.2.3', assets: 12, build: 'target-build' });
    setPhoneUpdateTarget({ version: '1.2.2', assets: 99, build: 'stale-build' });
    expect(get(updateProgressPlan)?.phoneTarget).toEqual({
      version: '1.2.3',
      assets: 12,
      build: 'target-build',
    });
  });

  it('fails an exhausted phone load from persisted state without relay events', () => {
    sessionStorage.setItem('herdr_update_progress', JSON.stringify({
      targetVersion: '1.2.3',
      relayIds: [],
      startedRelayIds: [],
      relayStartedAt: {},
      appRelayId: '',
      phoneAppRequired: true,
      phoneTarget: { version: '1.2.3', assets: 12, build: 'target-build' },
      phoneState: 'loading',
      phoneAcknowledged: false,
      phoneReloadAttempts: 2,
      phoneError: '',
      errors: {},
      startedAt: Date.now(),
    }));
    const fetcher = vi.fn();
    vi.stubGlobal('fetch', fetcher);
    const stop = initializeAppUpdates();
    stop();

    expect(get(updateProgressPlan)).toMatchObject({
      phoneState: 'failed',
      phoneAcknowledged: false,
    });
    expect(fetcher).toHaveBeenCalledTimes(1);
  });

  it('aborts a hung metadata request at the verification deadline', async () => {
    let signal: AbortSignal | undefined;
    const fetcher = vi.fn((_url: string, init?: RequestInit) => {
      signal = init?.signal || undefined;
      return new Promise<Response>((_resolve, reject) => {
        signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true });
      });
    }) as unknown as typeof fetch;

    const status = await waitForDeployedApp(APP_VERSION, {
      fetcher,
      attempts: 100,
      intervalMs: 0,
      deadlineMs: 10,
    });

    expect(status).toBeNull();
    expect(signal?.aborted).toBe(true);
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
