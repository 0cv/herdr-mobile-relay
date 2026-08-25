import { get, writable } from 'svelte/store';
import { APP_ASSET_VERSION, APP_VERSION } from './config';
import type { AppDeploymentStatus, AppUpdateStatus, RelayConnectionView, RelayUpdateStatus } from './types';

const APP_UPDATE_INTERVAL_MS = 24 * 60 * 60 * 1_000;
const APP_RECHECK_INTERVAL_MS = 60 * 1_000;
const PENDING_RELAY_UPDATES_KEY = 'herdr_pending_relay_updates';
const UPDATE_PROGRESS_KEY = 'herdr_update_progress';
const APP_RELOAD_TARGET_KEY = 'herdr_app_reload_target';
const APP_DEPLOY_SELF_UPDATE_MIN_VERSION = '0.13.3';
export const MANAGED_UPDATE_COMMAND = 'HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1 herdr plugin install 0cv/herdr-mobile-relay --yes';
export const CHECKOUT_UPDATE_COMMAND = 'git pull --ff-only && make service-install';
const RELAY_UPDATE_STATES = new Set([
  'checking',
  'current',
  'available',
  'blocked',
  'scheduled',
  'preparing',
  'deploying_app',
  'installing',
  'restarting',
  'succeeded',
  'failed',
  'rolled_back',
]);

export interface UpdateProgressPlan {
  targetVersion: string;
  relayIds: string[];
  startedRelayIds: string[];
  relayStartedAt: Record<string, number>;
  appRelayId: string;
  errors: Record<string, string>;
  startedAt: number;
}

export const appUpdateStatus = writable<AppUpdateStatus>({
  state: 'checking',
  currentVersion: APP_VERSION,
  currentAssets: APP_ASSET_VERSION,
  deployedVersion: '',
  deployedAssets: 0,
  upstreamVersion: APP_VERSION,
  upstreamAssets: 0,
  checkedAt: 0,
  error: '',
});

export const updateProgressPlan = writable<UpdateProgressPlan | null>(null);

let checking: Promise<AppUpdateStatus> | null = null;
let relayUpstreamVersion = APP_VERSION;

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
export function relayNeedsManualBootstrap(
  connection: Pick<RelayConnectionView, 'appDeploy' | 'capabilities' | 'releaseVersion' | 'update'>,
  failure = '',
): boolean {
  if (!connection.capabilities.includes('self_update')) return true;
  const legacyVersion = newerVersion(APP_DEPLOY_SELF_UPDATE_MIN_VERSION, connection.releaseVersion);
  if (!legacyVersion) return false;
  if (connection.appDeploy.configured) return true;
  const error = (failure || connection.update.error || connection.appDeploy.reason).toLowerCase();
  return (error.includes('deploy target app before relay')
    && error.includes('app deployment origin'))
    || error.includes('no https app deployment origin is configured');
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

export function observeAppUpstreamVersion(value: string): void {
  if (!semverTuple(value)) return;
  if (!relayUpstreamVersion || newerVersion(value, relayUpstreamVersion)) {
    relayUpstreamVersion = value;
  }
  appUpdateStatus.update((current) => {
    if (!current.deployedVersion) return current;
    const state = appUpdateAvailable({
      version: current.deployedVersion,
      assets: current.deployedAssets,
    })
      ? 'reload-ready'
      : newerVersion(relayUpstreamVersion, current.deployedVersion)
        ? 'deployment-required'
        : 'current';
    return {
      ...current,
      state,
      upstreamVersion: relayUpstreamVersion,
      upstreamAssets: 0,
      error: '',
    };
  });
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
      const deployed = await versionMetadata(fetcher, `/version.json?check=${now}`);
      const state = appUpdateAvailable(deployed)
        ? 'reload-ready'
        : relayUpstreamVersion && newerVersion(relayUpstreamVersion, deployed.version)
          ? 'deployment-required'
          : 'current';
      const status: AppUpdateStatus = {
        state,
        currentVersion: APP_VERSION,
        currentAssets: APP_ASSET_VERSION,
        deployedVersion: deployed.version,
        deployedAssets: deployed.assets,
        upstreamVersion: relayUpstreamVersion,
        upstreamAssets: 0,
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
  try {
    const attempted = sessionStorage.getItem(APP_RELOAD_TARGET_KEY) || '';
    if (attempted && !newerVersion(attempted, APP_VERSION)) {
      sessionStorage.removeItem(APP_RELOAD_TARGET_KEY);
    }
  } catch {
    // Storage can be unavailable in a hardened browser.
  }
  const normalizedUrl = normalizeReloadedAppUrl(location.href);
  if (normalizedUrl) history.replaceState(history.state, '', normalizedUrl);
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


export function relayServesCurrentOrigin(relayUrl: string): boolean {
  try {
    const relay = new URL(relayUrl);
    const relayOrigin = `${relay.protocol === 'wss:' ? 'https:' : 'http:'}//${relay.host}`;
    return relayOrigin === location.origin;
  } catch {
    return false;
  }
}

export function cacheBustedAppUrl(currentUrl: string, version: string, nonce = Date.now()): string {
  const url = new URL(currentUrl);
  const cacheKey = version || 'current';
  url.pathname = '/index.html';
  url.searchParams.set('herdr_reload', `${cacheKey}-${nonce}`);
  return url.toString();
}

export function normalizeReloadedAppUrl(currentUrl: string): string | null {
  const url = new URL(currentUrl);
  if (!url.searchParams.has('herdr_reload')) return null;
  // Cloudflare Pages canonically redirects /index.html to / while preserving
  // the query. Accept both so a successful reload never leaves its cache-bust
  // marker in the installed app's address.
  if (url.pathname !== '/index.html' && url.pathname !== '/') return null;
  url.pathname = '/';
  url.searchParams.delete('herdr_reload');
  return url.toString();
}

export interface DeployedAppWaitOptions {
  fetcher?: typeof fetch;
  attempts?: number;
  intervalMs?: number;
  sleep?: (milliseconds: number) => Promise<void>;
}

/**
 * Waits for the app origin to serve `version` or newer. Polling is silent —
 * it reads `/version.json` directly instead of running `checkAppUpdate`, so a
 * deployment that is still propagating (or a stale success announcement that
 * will never converge) cannot flip the visible update status to `checking`
 * once a second. Only the final landing publishes a status.
 */
export async function waitForDeployedApp(
  version: string,
  options: DeployedAppWaitOptions = {},
): Promise<AppUpdateStatus | null> {
  const fetcher = options.fetcher || fetch;
  const attempts = Math.max(1, options.attempts || 120);
  const intervalMs = Math.max(0, options.intervalMs ?? 1_000);
  const sleep = options.sleep || ((milliseconds: number) =>
    new Promise<void>((resolve) => window.setTimeout(resolve, milliseconds)));
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    try {
      const deployed = await versionMetadata(fetcher, `/version.json?check=${Date.now()}`);
      if (!newerVersion(version, deployed.version)) return checkAppUpdate(fetcher);
    } catch {
      // A propagating deployment can serve errors briefly; keep waiting.
    }
    if (attempt + 1 < attempts) await sleep(intervalMs);
  }
  return null;
}

let automaticReload: Promise<boolean> | null = null;

export function reloadUpdatedSameOriginApp(version: string): Promise<boolean> {
  if (!newerVersion(version, APP_VERSION)) return Promise.resolve(false);
  try {
    const attempted = sessionStorage.getItem(APP_RELOAD_TARGET_KEY) || '';
    // One automatic attempt covers that version and any older deployment
    // announcement. A genuinely newer target still gets its own attempt.
    if (attempted && !newerVersion(version, attempted)) return Promise.resolve(false);
  } catch {
    // Storage can be unavailable in a hardened browser; navigation still works.
  }
  if (automaticReload) return automaticReload;
  automaticReload = (async () => {
    const status = await waitForDeployedApp(version);
    if (!status) return false;
    reloadApp(status.deployedVersion);
    return true;
  })().finally(() => {
    automaticReload = null;
  });
  return automaticReload;
}

export function reloadApp(version = ''): void {
  // A versioned navigation bypasses a stale document retained by a sleeping
  // PWA or the back-forward cache. Replace avoids leaving that document behind
  // as the Back destination. Persist the target first: if the old document
  // survives the cutover, its next instance must not reload in a tight loop.
  const target = version || get(appUpdateStatus).deployedVersion;
  if (target) {
    try {
      sessionStorage.setItem(APP_RELOAD_TARGET_KEY, target);
    } catch {
      // Storage can be unavailable in a hardened browser; navigation still works.
    }
  }
  location.replace(cacheBustedAppUrl(location.href, target));
}

function saveUpdateProgress(plan: UpdateProgressPlan | null): void {
  updateProgressPlan.set(plan);
  try {
    if (plan) sessionStorage.setItem(UPDATE_PROGRESS_KEY, JSON.stringify(plan));
    else sessionStorage.removeItem(UPDATE_PROGRESS_KEY);
  } catch {
    // The in-memory screen still works when storage is unavailable.
  }
}

function normalizeUpdateProgress(value: unknown): UpdateProgressPlan | null {
  if (!value || typeof value !== 'object') return null;
  const candidate = value as Record<string, unknown>;
  const targetVersion = String(candidate.targetVersion || '');
  if (!semverTuple(targetVersion)) return null;
  const relayIds = Array.isArray(candidate.relayIds)
    ? [...new Set(candidate.relayIds.map(String).filter(Boolean))].slice(0, 32)
    : [];
  if (!relayIds.length) return null;
  const startedRelayIds = Array.isArray(candidate.startedRelayIds)
    ? [...new Set(candidate.startedRelayIds.map(String).filter((id) => relayIds.includes(id)))].slice(0, 32)
    : [];
  const startedAt = Number.isFinite(Number(candidate.startedAt)) ? Number(candidate.startedAt) : Date.now();
  const rawRelayStartedAt = candidate.relayStartedAt && typeof candidate.relayStartedAt === 'object'
    ? candidate.relayStartedAt as Record<string, unknown>
    : {};
  const relayStartedAt = Object.fromEntries(startedRelayIds.map((relayId) => {
    const timestamp = Number(rawRelayStartedAt[relayId]);
    return [relayId, Number.isFinite(timestamp) ? timestamp : startedAt];
  }));
  const rawErrors = candidate.errors && typeof candidate.errors === 'object'
    ? candidate.errors as Record<string, unknown>
    : {};
  const errors = Object.fromEntries(
    Object.entries(rawErrors)
      .filter(([relayId]) => relayIds.includes(relayId))
      .map(([relayId, error]) => [relayId, String(error).slice(0, 500)]),
  );
  const appRelayId = relayIds.includes(String(candidate.appRelayId || ''))
    ? String(candidate.appRelayId)
    : '';
  return {
    targetVersion,
    relayIds,
    startedRelayIds,
    relayStartedAt,
    appRelayId,
    errors,
    startedAt,
  };
}

export function restoreUpdateProgress(): void {
  try {
    saveUpdateProgress(normalizeUpdateProgress(JSON.parse(sessionStorage.getItem(UPDATE_PROGRESS_KEY) || 'null')));
  } catch {
    saveUpdateProgress(null);
  }
}

function newUpdateProgress(
  targetVersion: string,
  relayIds: string[],
  startedRelayId: string,
  appRelayId = '',
): UpdateProgressPlan | null {
  return normalizeUpdateProgress({
    targetVersion,
    relayIds,
    startedRelayIds: [startedRelayId],
    relayStartedAt: { [startedRelayId]: Date.now() },
    appRelayId,
    errors: {},
    startedAt: Date.now(),
  });
}

export function beginUpdateProgress(
  targetVersion: string,
  relayIds: string[],
  startedRelayId: string,
  appRelayId = '',
): void {
  const plan = newUpdateProgress(targetVersion, relayIds, startedRelayId, appRelayId);
  if (plan) saveUpdateProgress(plan);
}

export function queueUpdateProgressForReload(targetVersion: string, relayIds: string[]): void {
  const plan = newUpdateProgress(targetVersion, relayIds, '');
  if (!plan) return;
  try {
    sessionStorage.setItem(UPDATE_PROGRESS_KEY, JSON.stringify(plan));
  } catch {
    // Reloading the app still succeeds when session storage is unavailable.
  }
}

export function markUpdateProgressRelayStarted(relayId: string): void {
  const plan = get(updateProgressPlan);
  if (!plan || !plan.relayIds.includes(relayId)) return;
  saveUpdateProgress({
    ...plan,
    startedRelayIds: [...new Set([...plan.startedRelayIds, relayId])],
    relayStartedAt: { ...plan.relayStartedAt, [relayId]: Date.now() },
    errors: Object.fromEntries(Object.entries(plan.errors).filter(([id]) => id !== relayId)),
  });
}

export function setUpdateProgressError(relayId: string, error: unknown): void {
  const plan = get(updateProgressPlan);
  if (!plan || !plan.relayIds.includes(relayId)) return;
  saveUpdateProgress({
    ...plan,
    errors: {
      ...plan.errors,
      [relayId]: error instanceof Error ? error.message : String(error || 'Update command failed'),
    },
  });
}

export function clearUpdateProgress(): void {
  saveUpdateProgress(null);
}
