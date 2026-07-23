import { get, writable } from 'svelte/store';
import { APP_ASSET_VERSION, APP_VERSION, UPSTREAM_APP_VERSION_URL } from './config';
import type { AppDeploymentStatus, AppUpdateStatus, RelayUpdateStatus } from './types';

const APP_UPDATE_INTERVAL_MS = 24 * 60 * 60 * 1_000;
const APP_RECHECK_INTERVAL_MS = 60 * 1_000;
const PENDING_RELAY_UPDATES_KEY = 'herdr_pending_relay_updates';
const PENDING_APP_DEPLOY_KEY = 'herdr_pending_app_deploy';
const AUTO_RELOAD_VERSION_KEY = 'herdr_auto_reload_version';
const RELAY_UPDATE_STATES = new Set([
  'checking',
  'current',
  'available',
  'blocked',
  'scheduled',
  'installing',
  'restarting',
  'succeeded',
  'failed',
  'rolled_back',
]);

export const appUpdateStatus = writable<AppUpdateStatus>({
  state: 'checking',
  currentVersion: APP_VERSION,
  currentAssets: APP_ASSET_VERSION,
  deployedVersion: '',
  deployedAssets: 0,
  upstreamVersion: '',
  upstreamAssets: 0,
  checkedAt: 0,
  error: '',
});

let checking: Promise<AppUpdateStatus> | null = null;

export function semverTuple(value: string): [number, number, number] | null {
  const match = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.exec(value);
  return match ? [Number(match[1]), Number(match[2]), Number(match[3])] : null;
}

export function newerVersion(candidate: string, current: string): boolean {
  const next = semverTuple(candidate);
  const installed = semverTuple(current);
  if (!next || !installed) return false;
  for (let index = 0; index < next.length; index += 1) {
    if (next[index] === installed[index]) continue;
    return next[index] > installed[index];
  }
  return false;
}

export function newerBundle(
  candidate: { version: string; assets: number },
  current: { version: string; assets: number },
): boolean {
  return newerVersion(candidate.version, current.version)
    || (candidate.version === current.version && candidate.assets > current.assets);
}

export function appUpdateAvailable(deployed: { version: string; assets: number }): boolean {
  return newerBundle(deployed, { version: APP_VERSION, assets: APP_ASSET_VERSION });
}

export function normalizeRelayUpdate(
  value: unknown,
  currentVersion = '',
  currentRevision = '',
): RelayUpdateStatus {
  const update = value && typeof value === 'object' ? value as Record<string, unknown> : {};
  const state = typeof update.state === 'string' && RELAY_UPDATE_STATES.has(update.state)
    ? update.state as RelayUpdateStatus['state']
    : 'unsupported';
  return {
    state,
    current_version: String(update.current_version || currentVersion).slice(0, 32),
    current_revision: String(update.current_revision || currentRevision).slice(0, 40),
    available_version: String(update.available_version || '').slice(0, 32),
    available_revision: String(update.available_revision || '').slice(0, 40),
    target_revision: String(update.target_revision || '').slice(0, 40),
    upstream_version: String(update.upstream_version || update.available_version || '').slice(0, 32),
    upstream_revision: String(update.upstream_revision || update.target_revision || '').slice(0, 40),
    checked_at: Number.isFinite(Number(update.checked_at)) ? Number(update.checked_at) : 0,
    can_install: update.can_install === true,
    mode: String(update.mode || '').slice(0, 20),
    reason: String(update.reason || '').slice(0, 500),
    error: String(update.error || '').slice(0, 500),
  };
}

export function normalizeAppDeployment(value: unknown): AppDeploymentStatus {
  const deployment = value && typeof value === 'object' ? value as Record<string, unknown> : {};
  const state = ['idle', 'scheduled', 'deploying', 'succeeded', 'failed'].includes(String(deployment.state))
    ? String(deployment.state) as AppDeploymentStatus['state']
    : 'idle';
  return {
    configured: deployment.configured === true,
    origin: String(deployment.origin || '').slice(0, 300),
    project: String(deployment.project || '').slice(0, 80),
    branch: String(deployment.branch || '').slice(0, 120),
    revision: String(deployment.revision || '').slice(0, 40),
    reason: String(deployment.reason || '').slice(0, 500),
    state,
    target_version: String(deployment.target_version || '').slice(0, 32),
    target_revision: String(deployment.target_revision || '').slice(0, 40),
    checked_at: Number.isFinite(Number(deployment.checked_at)) ? Number(deployment.checked_at) : 0,
    error: String(deployment.error || '').slice(0, 500),
  };
}

async function versionMetadata(
  fetcher: typeof fetch,
  url: string,
): Promise<{ version: string; assets: number }> {
  const response = await fetcher(url, { cache: 'no-store' });
  if (!response.ok) throw new Error(`version check returned HTTP ${response.status}`);
  const payload = await response.json() as Record<string, unknown>;
  const version = String(payload.version || '');
  if (!semverTuple(version)) throw new Error('version metadata is invalid');
  const assets = Number(payload.assets);
  return { version, assets: Number.isInteger(assets) ? assets : 0 };
}

export async function checkAppUpdate(
  fetcher: typeof fetch = fetch,
  now = Date.now(),
): Promise<AppUpdateStatus> {
  if (checking) return checking;
  appUpdateStatus.update((status) => ({ ...status, state: 'checking', error: '' }));
  checking = (async () => {
    try {
      const [deployedResult, upstreamResult] = await Promise.allSettled([
        versionMetadata(fetcher, `/version.json?check=${now}`),
        versionMetadata(fetcher, `${UPSTREAM_APP_VERSION_URL}?check=${now}`),
      ]);
      if (deployedResult.status === 'rejected') throw deployedResult.reason;
      const deployed = deployedResult.value;
      if (upstreamResult.status === 'rejected') {
        const error = upstreamResult.reason instanceof Error
          ? upstreamResult.reason.message
          : 'Could not check the upstream app version';
        const status: AppUpdateStatus = {
          state: appUpdateAvailable(deployed) ? 'reload-ready' : 'failed',
          currentVersion: APP_VERSION,
          currentAssets: APP_ASSET_VERSION,
          deployedVersion: deployed.version,
          deployedAssets: deployed.assets,
          upstreamVersion: '',
          upstreamAssets: 0,
          checkedAt: now,
          error,
        };
        appUpdateStatus.set(status);
        return status;
      }
      const upstream = upstreamResult.value;
      const state = appUpdateAvailable(deployed)
        ? 'reload-ready'
        : newerBundle(upstream, deployed)
          ? 'deployment-required'
          : 'current';
      const status: AppUpdateStatus = {
        state,
        currentVersion: APP_VERSION,
        currentAssets: APP_ASSET_VERSION,
        deployedVersion: deployed.version,
        deployedAssets: deployed.assets,
        upstreamVersion: upstream.version,
        upstreamAssets: upstream.assets,
        checkedAt: now,
        error: '',
      };
      appUpdateStatus.set(status);
      return status;
    } catch (error) {
      const status: AppUpdateStatus = {
        ...get(appUpdateStatus),
        state: 'failed',
        checkedAt: now,
        error: error instanceof Error ? error.message : 'Could not check the app version',
      };
      appUpdateStatus.set(status);
      return status;
    } finally {
      checking = null;
    }
  })();
  return checking;
}

export function initializeAppUpdates(): () => void {
  void checkAppUpdate();
  const checkWhenDue = (minElapsed: number) => () => {
    const elapsed = Date.now() - get(appUpdateStatus).checkedAt;
    if (document.visibilityState === 'visible' && elapsed >= minElapsed) {
      void checkAppUpdate();
    }
  };
  const recheckWhenVisible = checkWhenDue(APP_RECHECK_INTERVAL_MS);
  const timer = window.setInterval(checkWhenDue(APP_UPDATE_INTERVAL_MS), APP_UPDATE_INTERVAL_MS);
  document.addEventListener('visibilitychange', recheckWhenVisible);
  window.addEventListener('pageshow', recheckWhenVisible);
  return () => {
    window.clearInterval(timer);
    document.removeEventListener('visibilitychange', recheckWhenVisible);
    window.removeEventListener('pageshow', recheckWhenVisible);
  };
}

interface PendingRelayUpdate {
  version: string;
  revision: string;
}

function pendingRelayUpdates(): Record<string, PendingRelayUpdate> {
  try {
    const parsed = JSON.parse(sessionStorage.getItem(PENDING_RELAY_UPDATES_KEY) || '{}');
    return parsed && typeof parsed === 'object' ? parsed : {};
  } catch {
    return {};
  }
}

export function rememberPendingRelayUpdate(relayId: string, target: PendingRelayUpdate): void {
  const pending = pendingRelayUpdates();
  pending[relayId] = target;
  sessionStorage.setItem(PENDING_RELAY_UPDATES_KEY, JSON.stringify(pending));
}

export function pendingRelayUpdate(relayId: string): PendingRelayUpdate | null {
  return pendingRelayUpdates()[relayId] || null;
}

export function clearPendingRelayUpdate(relayId: string): void {
  const pending = pendingRelayUpdates();
  delete pending[relayId];
  sessionStorage.setItem(PENDING_RELAY_UPDATES_KEY, JSON.stringify(pending));
}

// Records that an app deployment should fire from a relay once it finishes a
// relay update and reconnects at the target version. Stored in sessionStorage
// so the intent survives the relay's restart-and-reconnect cycle. Keyed by
// relay id, valued by the app version to deploy.
function pendingAppDeploys(): Record<string, string> {
  try {
    const parsed = JSON.parse(sessionStorage.getItem(PENDING_APP_DEPLOY_KEY) || '{}');
    return parsed && typeof parsed === 'object' ? parsed : {};
  } catch {
    return {};
  }
}

export function rememberPendingAppDeploy(relayId: string, version: string): void {
  const pending = pendingAppDeploys();
  pending[relayId] = version;
  sessionStorage.setItem(PENDING_APP_DEPLOY_KEY, JSON.stringify(pending));
}

export function pendingAppDeploy(relayId: string): string | null {
  return pendingAppDeploys()[relayId] || null;
}

export function clearPendingAppDeploy(relayId: string): void {
  const pending = pendingAppDeploys();
  delete pending[relayId];
  sessionStorage.setItem(PENDING_APP_DEPLOY_KEY, JSON.stringify(pending));
}

export function relayServesCurrentOrigin(relayUrl: string): boolean {
  try {
    const relay = new URL(relayUrl);
    const relayOrigin = `${relay.protocol === 'wss:' ? 'https:' : 'http:'}//${relay.host}`;
    return relayOrigin === location.origin;
  } catch {
    return false;
  }
}

export async function reloadUpdatedSameOriginApp(version: string): Promise<boolean> {
  if (sessionStorage.getItem(AUTO_RELOAD_VERSION_KEY) === version) return false;
  const status = await checkAppUpdate();
  if (status.state !== 'reload-ready' || status.deployedVersion !== version) return false;
  sessionStorage.setItem(AUTO_RELOAD_VERSION_KEY, version);
  location.reload();
  return true;
}

export function reloadApp(): void {
  location.reload();
}
