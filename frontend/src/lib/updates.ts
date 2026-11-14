import { get, writable } from 'svelte/store';
import { APP_ASSET_VERSION, APP_BUILD_ID, APP_VERSION } from './config';
import { reloadRisk, reloadRiskMessage, type ReloadRisk } from './reload-guard';
import { shortRevision } from './protocol';
import type { AppDeploymentStatus, AppUpdateStatus, RelayConfig, RelayConnectionView, RelayUpdateState, RelayUpdateStatus } from './types';

const APP_UPDATE_INTERVAL_MS = 24 * 60 * 60 * 1_000;
const APP_RECHECK_INTERVAL_MS = 60 * 1_000;
const PENDING_RELAY_UPDATES_KEY = 'herdr_pending_relay_updates';
const UPDATE_PROGRESS_KEY = 'herdr_update_progress';
const APP_RELOAD_TARGET_KEY = 'herdr_app_reload_target';
const APP_RELOAD_ATTEMPTS_KEY = 'herdr_app_reload_attempts';
const MAX_AUTOMATIC_RELOAD_ATTEMPTS = 2;
const sessionStartedRelayIds = new Set<string>();
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
]);

export interface PhoneAppTarget {
  version: string;
  assets: number;
  build: string;
}

export function deployedAppTarget(
  status: Pick<AppUpdateStatus, 'deployedVersion' | 'deployedAssets' | 'deployedBuild'>,
  version = status.deployedVersion,
): PhoneAppTarget {
  return {
    version,
    assets: status.deployedVersion === version ? status.deployedAssets : 0,
    build: status.deployedVersion === version ? (status.deployedBuild || '') : '',
  };
}

export type PhoneUpdateState = 'publishing' | 'loading' | 'loaded' | 'failed';

export interface UpdateProgressPlan {
  targetVersion: string;
  relayIds: string[];
  startedRelayIds: string[];
  relayStartedAt: Record<string, number>;
  /** Deployment owner only; this is deliberately not the phone item. */
  appRelayId: string;
  phoneAppRequired: boolean;
  phoneTarget: PhoneAppTarget | null;
  phoneState: PhoneUpdateState;
  phoneAcknowledged: boolean;
  phoneReloadAttempts: number;
  phoneError: string;
  errors: Record<string, string>;
  startedAt: number;
}

export const appUpdateStatus = writable<AppUpdateStatus>({
  state: 'checking',
  currentVersion: APP_VERSION,
  currentAssets: APP_ASSET_VERSION,
  currentBuild: APP_BUILD_ID,
  deployedVersion: '',
  deployedAssets: 0,
  deployedBuild: '',
  upstreamVersion: APP_VERSION,
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
  connection: Pick<RelayConnectionView, 'capabilities'>,
): boolean {
  return !connection.capabilities.includes('self_update');
}


interface BundleIdentity {
  version: string;
  assets: number;
  build?: string;
}

function satisfiesTarget(
  candidate: BundleIdentity,
  target: BundleIdentity,
  emptyBuildSatisfies: boolean,
): boolean {
  if (newerVersion(candidate.version, target.version)) return true;
  if (candidate.version !== target.version) return false;
  if (candidate.assets !== target.assets) return candidate.assets > target.assets;
  if (!target.build) return emptyBuildSatisfies;
  return candidate.build === target.build;
}

export function newerBundle(candidate: BundleIdentity, current: BundleIdentity): boolean {
  if (candidate.version !== current.version) return newerVersion(candidate.version, current.version);
  return !satisfiesTarget(current, candidate, true);
}

export function appUpdateAvailable(deployed: BundleIdentity): boolean {
  return newerBundle(deployed, {
    version: APP_VERSION,
    assets: APP_ASSET_VERSION,
    build: APP_BUILD_ID,
  });
}

export function observeAppUpstreamVersion(value: string): void {
  if (!semverTuple(value)) return;
  if (!relayUpstreamVersion || newerVersion(value, relayUpstreamVersion)) {
    relayUpstreamVersion = value;
  }
  appUpdateStatus.update((current) => {
    if (!current.deployedVersion) return current;
    const state = appUpdateAvailable(deployedAppTarget(current))
      ? 'reload-ready'
      : newerVersion(relayUpstreamVersion, current.deployedVersion)
        ? 'deployment-required'
        : 'current';
    return {
      ...current,
      state,
      upstreamVersion: relayUpstreamVersion,
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

interface AppOriginMetadata {
  version: string;
  assets: number;
  build: string;
  entry: string;
  script: string;
  style: string;
}

function metadataString(value: unknown, limit = 300): string {
  return typeof value === 'string' ? value.slice(0, limit) : '';
}

async function versionMetadata(
  fetcher: typeof fetch,
  url: string,
): Promise<AppOriginMetadata> {
  const response = await fetcher(url, { cache: 'no-store' });
  if (!response.ok) throw new Error(`version check returned HTTP ${response.status}`);
  const payload = await response.json() as Record<string, unknown>;
  const version = String(payload.version || '');
  if (!semverTuple(version)) throw new Error('version metadata is invalid');
  const assets = Number(payload.assets);
  return {
    version,
    assets: Number.isInteger(assets) ? assets : 0,
    build: metadataString(payload.build, 100),
    entry: metadataString(payload.entry, 500),
    script: metadataString(payload.script, 500),
    style: metadataString(payload.style, 500),
  };
}

function appStatusFromMetadata(deployed: AppOriginMetadata, checkedAt: number): AppUpdateStatus {
  const state = appUpdateAvailable(deployed)
    ? 'reload-ready'
    : relayUpstreamVersion && newerVersion(relayUpstreamVersion, deployed.version)
      ? 'deployment-required'
      : 'current';
  return {
    state,
    currentVersion: APP_VERSION,
    currentAssets: APP_ASSET_VERSION,
    currentBuild: APP_BUILD_ID,
    deployedVersion: deployed.version,
    deployedAssets: deployed.assets,
    deployedBuild: deployed.build,
    upstreamVersion: relayUpstreamVersion,
    checkedAt,
    error: '',
  };
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
      const status = appStatusFromMetadata(deployed, now);
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

function requiredAssetPending(): boolean {
  // A cached bootstrap can omit its readiness flag even after the new CSS
  // loads. The browser attaches a sheet only after accepting the resource,
  // including its integrity check, so use that result for acknowledgement.
  const stylesheet = document.querySelector<HTMLLinkElement>('link[rel="stylesheet"][href*="/assets/app-"]');
  return !document.documentElement.dataset.herdrLoadFailed && Boolean(stylesheet && !stylesheet.sheet);
}

export function initializeAppUpdates(): () => void {
  const requiredAssetChange = (event?: Event) => {
    const dataset = document.documentElement.dataset;
    if (event?.type === 'error' || dataset.herdrLoadFailed || dataset.herdrLoadTimedOut) {
      setPhoneUpdateError('The required app asset did not load. Use Load Update to try again.');
      return false;
    }
    return acknowledgePhoneUpdate();
  };
  const stylesheet = document.querySelector<HTMLLinkElement>('link[rel="stylesheet"][href*="/assets/app-"]');
  stylesheet?.addEventListener('load', requiredAssetChange);
  stylesheet?.addEventListener('error', requiredAssetChange);
  window.addEventListener('herdr-required-assets', requiredAssetChange);
  restoreUpdateProgress();
  const acknowledged = requiredAssetChange();
  try {
    const attempted = sessionStorage.getItem(APP_RELOAD_TARGET_KEY) || '';
    const attemptedVersion = reloadMarkerVersion(attempted);
    const attemptedBuild = attempted.includes(':') ? attempted.slice(attempted.indexOf(':') + 1) : '';
    const obsolete = attempted
      && (!newerVersion(attemptedVersion, APP_VERSION)
        || attemptedVersion === APP_VERSION && attemptedBuild === APP_BUILD_ID);
    if (obsolete) sessionStorage.removeItem(APP_RELOAD_TARGET_KEY);
  } catch {
    // Storage can be unavailable in a hardened browser.
  }
  const normalizedUrl = normalizeReloadedAppUrl(location.href);
  if (normalizedUrl) history.replaceState(history.state, '', normalizedUrl);
  if (!acknowledged) resumePendingPhoneUpdate();
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
    window.removeEventListener('herdr-required-assets', requiredAssetChange);
    stylesheet?.removeEventListener('load', requiredAssetChange);
    stylesheet?.removeEventListener('error', requiredAssetChange);
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
  const buildEntry = /^\/builds\/[A-Za-z0-9._-]+\/(?:index\.html)?$/.test(url.pathname);
  if (!url.searchParams.has('herdr_reload') && !buildEntry) return null;
  // Cloudflare Pages and relay-hosted apps both preserve the old /index.html
  // contract while routing the document to a digest-specific entry. Replace
  // that implementation path so it cannot become a second PWA route or leak a
  // reload marker into the installed app's address.
  if (url.pathname !== '/index.html' && url.pathname !== '/' && !buildEntry) return null;
  url.pathname = '/';
  url.searchParams.delete('herdr_reload');
  return url.toString();
}

export interface DeployedAppWaitOptions {
  fetcher?: typeof fetch;
  attempts?: number;
  intervalMs?: number;
  sleep?: (milliseconds: number) => Promise<void>;
  deadlineMs?: number;
  target?: PhoneAppTarget | null;
}

function deployedTargetReady(
  deployed: Pick<AppOriginMetadata, 'version' | 'assets' | 'build'>,
  target: PhoneAppTarget | null,
): boolean {
  if (!target) return false;
  return satisfiesTarget(deployed, target, true);
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
  const signal = AbortSignal.timeout(Math.max(1, options.deadlineMs ?? 120_000));
  const timedFetcher: typeof fetch = (url) => fetcher(url, { cache: 'no-store', signal });
  const deadline = new Promise<null>((resolve) => { signal.onabort = () => resolve(null); });
  const read = (url: string) => Promise.race([versionMetadata(timedFetcher, url), deadline]);
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    try {
      const deployed = await read(`/version.json?check=${Date.now()}`);
      if (!deployed) return null;
      if (deployedTargetReady(deployed, options.target || { version, assets: 0, build: '' })) {
        // The first response only proves that the origin was ready once. The
        // status check below is a separate request and must satisfy the same
        // target before its metadata can drive a reload. A CDN can otherwise
        // answer the probe with the new release and the status request with a
        // stale older representation, downgrading the pending phone target.
        const previous = get(appUpdateStatus);
        const checkingStatus = { ...previous, state: 'checking' as const, error: '' };
        const restoreIfCurrent = () => {
          if (get(appUpdateStatus) === checkingStatus) appUpdateStatus.set(previous);
        };
        appUpdateStatus.set(checkingStatus);
        try {
          const final = await read(`/version.json?check=${Date.now()}`);
          if (!final) {
            restoreIfCurrent();
            return null;
          }
          if (deployedTargetReady(final, options.target || { version, assets: 0, build: '' })) {
            const status = appStatusFromMetadata(final, Date.now());
            appUpdateStatus.set(status);
            return status;
          }
          // Do not let an older edge response replace the pending target or
          // the visible deployment identity. The next poll gets another
          // chance.
          restoreIfCurrent();
        } catch {
          restoreIfCurrent();
        }
      }
    } catch {
      // A propagating deployment can serve errors briefly; keep waiting.
    }
    if (attempt + 1 < attempts) await sleep(intervalMs);
  }
  return null;
}

/** A newer build is ready, but reloading into it now would lose work that only this page holds. */
export class AppReloadHeld extends Error {}

function heldReloadMessage(risk: ReloadRisk): string {
  return `A newer app version is ready but was not loaded automatically. ${reloadRiskMessage(risk)} Use Load Update in Settings when you are ready.`;
}

let automaticReload: Promise<boolean> | null = null;
let heldReload = '';
// Every caller waiting on the running reload adds its own check; the reload goes ahead if any still wants it.
let automaticReloadWanted: Array<() => boolean> = [];

function currentPhoneTarget(): PhoneAppTarget {
  return { version: APP_VERSION, assets: APP_ASSET_VERSION, build: APP_BUILD_ID };
}

function requestedPhoneTarget(version: string, target: PhoneAppTarget | null): PhoneAppTarget {
  const status = get(appUpdateStatus);
  const requested = target || deployedAppTarget(status, version);
  const pending = get(updateProgressPlan);
  if (pending?.phoneAppRequired && pending.phoneTarget
    && !satisfiesTarget(requested, pending.phoneTarget, true)) {
    return pending.phoneTarget;
  }
  return requested;
}

function phoneTargetIsNewer(target: PhoneAppTarget): boolean {
  return newerBundle(target, currentPhoneTarget());
}

function reloadTargetKey(target: PhoneAppTarget): string {
  return target.build ? `${target.version}:${target.build}` : target.version;
}

function reloadMarkerVersion(value: string): string {
  return value.split(':', 1)[0] || '';
}

export function reloadUpdatedSameOriginApp(
  version: string,
  target: PhoneAppTarget | null = null,
  stillWanted: () => boolean = () => true,
): Promise<boolean> {
  if (!automaticReload) heldReload = '';
  const requested = requestedPhoneTarget(version, target);
  if (!phoneTargetIsNewer(requested)) return Promise.resolve(false);
  const key = reloadTargetKey(requested);
  try {
    const attempted = sessionStorage.getItem(APP_RELOAD_TARGET_KEY) || '';
    if (attempted === key) {
      const attempts = automaticReloadAttempts(key);
      // A legacy test or an interrupted write can leave only the marker. Do
      // not start an untracked navigation in that case; a real navigation
      // always records its attempt before replacing the document.
      if (attempts === 0 && !get(updateProgressPlan)?.phoneAppRequired) return Promise.resolve(false);
      if (attempts >= MAX_AUTOMATIC_RELOAD_ATTEMPTS) {
        setPhoneUpdateError(`The phone app did not acknowledge build v${requested.version} after ${MAX_AUTOMATIC_RELOAD_ATTEMPTS} automatic attempts. Use Load Update to try again.`);
        return Promise.resolve(false);
      }
    } else if (attempted && reloadMarkerVersion(attempted) === requested.version && !requested.build) {
      return Promise.resolve(false);
    }
  } catch {
    // Storage can be unavailable in a hardened browser; navigation still works.
  }
  if (automaticReloadAttempts(key) >= MAX_AUTOMATIC_RELOAD_ATTEMPTS) {
    setPhoneUpdateError(`The phone app did not acknowledge build v${requested.version} after ${MAX_AUTOMATIC_RELOAD_ATTEMPTS} automatic attempts. Use Load Update to try again.`);
    return Promise.resolve(false);
  }
  automaticReloadWanted.push(stillWanted);
  if (automaticReload) return automaticReload;
  automaticReload = (async () => {
    const status = await waitForDeployedApp(requested.version, { target: requested });
    if (!status) {
      setPhoneUpdateError(`The phone app release v${requested.version} did not become available at this origin before the verification deadline.`);
      return false;
    }
    const actualTarget = deployedAppTarget(status);
    if (!automaticReloadWanted.some((wanted) => wanted())) return false;
    const risk = reloadRisk();
    if (risk) {
      heldReload = heldReloadMessage(risk);
      setPhoneUpdateError(heldReload);
      return false;
    }
    setPhoneUpdateTarget(actualTarget);
    reloadApp(status.deployedVersion, actualTarget);
    return true;
  })().finally(() => {
    automaticReload = null;
    automaticReloadWanted = [];
  });
  return automaticReload;
}

/** Why the last automatic reload waited instead of navigating, or '' if it did not wait for that reason. */
export function reloadHeldMessage(): string {
  return heldReload;
}

/**
 * A deploy removes the previous build's lazily loaded files. When one fails to
 * load, reload into the newer deployed build if there is one and it is still wanted.
 */
export async function importOrReload<T>(load: () => Promise<T>, stillWanted: () => boolean): Promise<T> {
  try {
    return await load();
  } catch (error) {
    // Read the version quietly: offline, the load error alone is the useful message.
    let status: AppUpdateStatus;
    try {
      status = appStatusFromMetadata(await versionMetadata(fetch, `/version.json?check=${Date.now()}`), Date.now());
    } catch {
      throw error;
    }
    appUpdateStatus.set(status);
    const deployed = deployedAppTarget(status);
    if (status.state === 'reload-ready' && stillWanted()) {
      if (await reloadUpdatedSameOriginApp(deployed.version, deployed, stillWanted)) {
        // The page is now navigating to the deployed build, so this load never finishes.
        return new Promise<T>(() => {});
      }
      if (heldReload) throw new AppReloadHeld(heldReload);
    }
    throw error;
  }
}

export function reloadApp(version = '', target: PhoneAppTarget | null = null): void {
  // A versioned navigation bypasses a stale document retained by a sleeping
  // PWA or the back-forward cache. Replace avoids leaving that document behind
  // as the Back destination. Persist the target first: if the old document
  // survives the cutover, its next instance must not reload in a tight loop.
  const status = get(appUpdateStatus);
  const targetVersion = version || status.deployedVersion;
  const targetIdentity = requestedPhoneTarget(targetVersion, target);
  if (targetVersion) {
    const key = reloadTargetKey(targetIdentity);
    recordAutomaticReloadAttempt(key, targetVersion);
    try {
      sessionStorage.setItem(APP_RELOAD_TARGET_KEY, key);
    } catch {
      // Storage can be unavailable in a hardened browser; navigation still works.
    }
  }
  location.replace(cacheBustedAppUrl(location.href, targetVersion));
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
  const legacyAppRelayId = String(candidate.appRelayId || '');
  const phoneAppRequired = candidate.phoneAppRequired === true
    // Plans written before the phone item was independent used appRelayId as
    // an accidental proxy. Preserve that pending work, but never infer a
    // phone item for an explicit relay-only plan.
    || Boolean(legacyAppRelayId);
  if (!relayIds.length && !phoneAppRequired) return null;
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
  const appRelayId = relayIds.includes(legacyAppRelayId) ? legacyAppRelayId : '';
  const rawPhoneTarget = candidate.phoneTarget && typeof candidate.phoneTarget === 'object'
    ? candidate.phoneTarget as Record<string, unknown>
    : {};
  const rawPhoneVersion = String(rawPhoneTarget.version || targetVersion);
  const phoneVersion = semverTuple(rawPhoneVersion) ? rawPhoneVersion : targetVersion;
  const rawPhoneAssets = Number(rawPhoneTarget.assets);
  const phoneAssets = Number.isInteger(rawPhoneAssets) && rawPhoneAssets >= 0
    ? Math.min(rawPhoneAssets, 1_000_000)
    : 0;
  const phoneTarget = phoneAppRequired
    ? {
      version: phoneVersion,
      assets: phoneAssets,
      build: String(rawPhoneTarget.build || '').slice(0, 100),
    }
    : null;
  const rawPhoneState = String(candidate.phoneState || 'loading');
  const phoneState: PhoneUpdateState = ['publishing', 'loading', 'loaded', 'failed'].includes(rawPhoneState)
    ? rawPhoneState as PhoneUpdateState
    : 'loading';
  return {
    targetVersion,
    relayIds,
    startedRelayIds,
    relayStartedAt,
    appRelayId,
    phoneAppRequired,
    phoneTarget,
    phoneState,
    phoneAcknowledged: candidate.phoneAcknowledged === true,
    phoneReloadAttempts: Math.max(0, Math.min(
      MAX_AUTOMATIC_RELOAD_ATTEMPTS,
      Number.isFinite(Number(candidate.phoneReloadAttempts)) ? Number(candidate.phoneReloadAttempts) : 0,
    )),
    phoneError: String(candidate.phoneError || '').slice(0, 500),
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

interface NewUpdateProgressOptions {
  phoneAppRequired?: boolean;
  phoneTarget?: PhoneAppTarget | null;
  phoneState?: PhoneUpdateState;
}

function newUpdateProgress(
  targetVersion: string,
  relayIds: string[],
  startedRelayId: string,
  appRelayId = '',
  options: NewUpdateProgressOptions = {},
): UpdateProgressPlan | null {
  const now = Date.now();
  return normalizeUpdateProgress({
    targetVersion,
    relayIds,
    startedRelayIds: startedRelayId ? [startedRelayId] : [],
    relayStartedAt: startedRelayId ? { [startedRelayId]: now } : {},
    appRelayId,
    phoneAppRequired: options.phoneAppRequired === true,
    phoneTarget: options.phoneTarget || null,
    phoneState: options.phoneState || (options.phoneAppRequired ? 'publishing' : 'loaded'),
    phoneAcknowledged: false,
    phoneReloadAttempts: 0,
    phoneError: '',
    errors: {},
    startedAt: now,
  });
}

export function beginUpdateProgress(
  targetVersion: string,
  relayIds: string[],
  startedRelayId: string,
  appRelayId = '',
  options: NewUpdateProgressOptions = {},
): void {
  const plan = newUpdateProgress(targetVersion, relayIds, startedRelayId, appRelayId, options);
  if (!plan) return;
  if (startedRelayId) sessionStartedRelayIds.add(startedRelayId);
  saveUpdateProgress(plan);
}

export function queueUpdateProgressForReload(
  targetVersion: string,
  relayIds: string[],
  phoneTarget: PhoneAppTarget | null = null,
): void {
  const plan = newUpdateProgress(
    targetVersion,
    relayIds,
    '',
    '',
    {
      phoneAppRequired: true,
      phoneTarget: phoneTarget || { version: targetVersion, assets: 0, build: '' },
      phoneState: 'loading',
    },
  );
  if (!plan) return;
  saveUpdateProgress(plan);
}

export function phoneTargetMatchesCurrent(target: PhoneAppTarget | null): boolean {
  if (!target) return false;
  // A target without a build stays pending at the same release identity, so
  // an old bundle can never acknowledge itself after navigation.
  return satisfiesTarget(currentPhoneTarget(), target, false);
}

export function setPhoneUpdateTarget(target: PhoneAppTarget): void {
  const plan = get(updateProgressPlan);
  if (!plan?.phoneAppRequired || !semverTuple(target.version)) return;
  const next = {
    version: target.version,
    assets: Number.isInteger(target.assets) && target.assets >= 0 ? target.assets : 0,
    build: String(target.build || '').slice(0, 100),
  };
  // A late response from an older edge must never replace the target that was
  // persisted for the phone. It is safe to learn a more advanced identity,
  // but not to move the plan backwards or sideways at the same revision.
  if (plan.phoneTarget && !satisfiesTarget(next, plan.phoneTarget, true)) return;
  saveUpdateProgress({
    ...plan,
    phoneTarget: next,
    phoneState: plan.phoneAcknowledged ? 'loaded' : plan.phoneState,
  });
}

function resumePendingPhoneUpdate(): void {
  const plan = get(updateProgressPlan);
  if (!plan?.phoneAppRequired || plan.phoneState === 'failed') return;
  const dataset = document.documentElement.dataset;
  if (dataset.herdrLoadTimedOut) {
    setPhoneUpdateError('The required app asset did not load. Use Load Update to try again.');
    return;
  }
  if (requiredAssetPending()) return;
  const target = plan.phoneTarget;
  if (!target) {
    setPhoneUpdateError('The pending phone app target is missing. Use Load Update to start it again.');
    return;
  }
  if (plan.phoneReloadAttempts >= MAX_AUTOMATIC_RELOAD_ATTEMPTS) {
    setPhoneUpdateError(`The phone app did not acknowledge build v${target.version} after ${MAX_AUTOMATIC_RELOAD_ATTEMPTS} automatic attempts. Use Load Update to try again.`);
    return;
  }
  if (!phoneTargetIsNewer(target)) {
    setPhoneUpdateError(`The running phone app could not verify the expected build v${target.version}. Use Load Update to try again.`);
    return;
  }
  void reloadUpdatedSameOriginApp(target.version, target);
}

export function acknowledgePhoneUpdate(): boolean {
  const plan = get(updateProgressPlan);
  if (!plan?.phoneAppRequired || !phoneTargetMatchesCurrent(plan.phoneTarget)) return false;
  if (document.documentElement.dataset.herdrLoadFailed || requiredAssetPending()) return false;
  saveUpdateProgress({
    ...plan,
    phoneState: 'loaded',
    phoneAcknowledged: true,
    phoneError: '',
  });
  try {
    sessionStorage.removeItem(APP_RELOAD_TARGET_KEY);
    sessionStorage.removeItem(APP_RELOAD_ATTEMPTS_KEY);
  } catch {
    // The acknowledgement remains useful in memory when storage is blocked.
  }
  return true;
}

export function setPhoneUpdateError(error: unknown): void {
  const plan = get(updateProgressPlan);
  if (!plan?.phoneAppRequired) return;
  saveUpdateProgress({
    ...plan,
    phoneState: 'failed',
    phoneAcknowledged: false,
    phoneError: error instanceof Error ? error.message : String(error || 'Phone app failed to load'),
  });
}

function automaticReloadAttempts(target: string): number {
  try {
    const raw = JSON.parse(sessionStorage.getItem(APP_RELOAD_ATTEMPTS_KEY) || '{}');
    const attempts = Number(raw && typeof raw === 'object' ? raw[target] : 0);
    return Number.isFinite(attempts) ? Math.max(0, attempts) : 0;
  } catch {
    return 0;
  }
}

function recordAutomaticReloadAttempt(key: string, targetVersion: string): number {
  const attempts = automaticReloadAttempts(key) + 1;
  try {
    const raw = JSON.parse(sessionStorage.getItem(APP_RELOAD_ATTEMPTS_KEY) || '{}');
    const values = raw && typeof raw === 'object' ? raw as Record<string, unknown> : {};
    values[key] = attempts;
    sessionStorage.setItem(APP_RELOAD_ATTEMPTS_KEY, JSON.stringify(values));
  } catch {
    // Navigation still gets the cache-busted URL when storage is unavailable.
  }
  const plan = get(updateProgressPlan);
  if (plan?.phoneAppRequired && plan.phoneTarget?.version === targetVersion) {
    saveUpdateProgress({
      ...plan,
      phoneReloadAttempts: Math.min(attempts, MAX_AUTOMATIC_RELOAD_ATTEMPTS),
      phoneState: 'loading',
      phoneError: '',
    });
  }
  return attempts;
}

export function markUpdateProgressRelayStarted(relayId: string): void {
  const plan = get(updateProgressPlan);
  if (!plan || !plan.relayIds.includes(relayId)) return;
  sessionStartedRelayIds.add(relayId);
  saveUpdateProgress({
    ...plan,
    startedRelayIds: [...new Set([...plan.startedRelayIds, relayId])],
    relayStartedAt: { ...plan.relayStartedAt, [relayId]: Date.now() },
    errors: Object.fromEntries(Object.entries(plan.errors).filter(([id]) => id !== relayId)),
  });
}

export function updateProgressRelayStartedThisSession(relayId: string): boolean {
  return sessionStartedRelayIds.has(relayId);
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
  try {
    sessionStorage.removeItem(APP_RELOAD_TARGET_KEY);
    sessionStorage.removeItem(APP_RELOAD_ATTEMPTS_KEY);
  } catch {
    // Nothing else is required when session storage is unavailable.
  }
}

export const MANUAL_UPDATE_LABEL = 'Manual update required';

type ProgressTone = 'pending' | 'active' | 'success' | 'danger';

interface RelayUpdateProgressText {
  label: string;
  detail: string;
  score: number;
}

const RELAY_UPDATE_PROGRESS: Partial<Record<RelayUpdateState, RelayUpdateProgressText>> = {
  checking: { label: 'Checking release…', detail: 'Looking for the exact verified target.', score: .05 },
  scheduled: { label: 'Update scheduled…', detail: 'The background update worker is starting.', score: .1 },
  preparing: { label: 'Verifying release…', detail: 'Downloading and checking the signed release bundle.', score: .25 },
  deploying_app: { label: 'Publishing phone app…', detail: 'Waiting for the app origin to serve the verified bundle.', score: .45 },
  installing: { label: 'Installing relay…', detail: 'Running agents and saved configuration remain intact.', score: .65 },
  restarting: { label: 'Restarting relay…', detail: 'The phone connection may briefly disconnect.', score: .85 },
};

export function relayUpdateRunning(state: RelayUpdateState): boolean {
  return state !== 'checking' && Boolean(RELAY_UPDATE_PROGRESS[state]);
}

export interface RelayProgressRow {
  id: string;
  relay?: RelayConfig;
  connection?: RelayConnectionView;
  label: string;
  detail: string;
  tone: ProgressTone;
  score: number;
  canStart: boolean;
  manual: boolean;
}

export interface AppProgress {
  label: string;
  detail: string;
  tone: ProgressTone;
  score: number;
}

const RELAY_OPERATION_TIMEOUT_MS = 5 * 60 * 1_000;

export function relayAtTarget(connection: RelayConnectionView | undefined, targetVersion: string): boolean {
  const current = connection?.releaseVersion || connection?.update.current_version || '';
  return current === targetVersion || newerVersion(current, targetVersion);
}

export function relayProgress(
  currentPlan: UpdateProgressPlan,
  relayId: string,
  relay: RelayConfig | undefined,
  connection: RelayConnectionView | undefined,
  timestamp: number,
): RelayProgressRow {
  const base = { id: relayId, relay, connection };
  if (relayAtTarget(connection, currentPlan.targetVersion)) {
    return {
      ...base,
      label: `Updated to v${connection?.releaseVersion || currentPlan.targetVersion}`,
      detail: 'This relay is ready.',
      tone: 'success',
      score: 1,
      canStart: false,
      manual: false,
    };
  }
  const clientError = currentPlan.errors[relayId];
  if (clientError) {
    const manual = Boolean(connection && relayNeedsManualBootstrap(connection));
    return {
      ...base,
      label: manual ? MANUAL_UPDATE_LABEL : 'Update could not start',
      detail: manual
        ? 'Run the one-time Terminal command below to restart the relay and enable phone-driven updates.'
        : clientError,
      tone: 'danger',
      score: 0,
      canStart: !manual && Boolean(connection?.capabilities.includes('self_update')),
      manual,
    };
  }
  const started = currentPlan.startedRelayIds.includes(relayId);
  if (!connection || connection.status !== 'connected') {
    if (started && timestamp - (currentPlan.relayStartedAt[relayId] || currentPlan.startedAt) < RELAY_OPERATION_TIMEOUT_MS) {
      return { ...base, label: 'Waiting for relay to reconnect…', detail: 'The connection normally returns after the service restarts.', tone: 'active', score: .85, canStart: false, manual: false };
    }
    return {
      ...base,
      label: started ? 'Relay did not reconnect' : 'Relay unavailable',
      detail: started ? 'Check the relay service on this computer, then close this screen.' : 'Connect this relay before updating it.',
      tone: started ? 'danger' : 'pending',
      score: 0,
      canStart: false,
      manual: false,
    };
  }
  if (relayNeedsManualBootstrap(connection)) {
    return {
      ...base,
      label: MANUAL_UPDATE_LABEL,
      detail: 'Run the one-time Terminal command below to restart the relay and enable phone-driven updates.',
      tone: 'danger',
      score: 0,
      canStart: false,
      manual: true,
    };
  }
  const update = connection.update;
  if (update.state === 'failed') {
    return { ...base, label: 'Update failed', detail: update.error || 'The relay reported an update failure.', tone: 'danger', score: 0, canStart: true, manual: false };
  }
  if (update.state === 'blocked') {
    return { ...base, label: 'Update blocked', detail: update.reason || 'This update needs attention before it can continue.', tone: 'danger', score: 0, canStart: false, manual: false };
  }
  const activeText = RELAY_UPDATE_PROGRESS[update.state];
  if (activeText) {
    return { ...base, ...activeText, tone: 'active', canStart: false, manual: false };
  }
  if (update.state === 'available' && update.available_version === currentPlan.targetVersion && update.can_install) {
    if (started && updateProgressRelayStartedThisSession(relayId)) {
      return { ...base, label: 'Starting update…', detail: 'Sending the verified target to this relay.', tone: 'active', score: .05, canStart: false, manual: false };
    }
    return {
      ...base,
      label: started ? `Ready to retry v${currentPlan.targetVersion}` : `Ready for v${currentPlan.targetVersion}`,
      detail: started ? 'The previous phone session did not finish sending this update; retrying is safe.' : 'Start this relay when the current relay is ready.',
      tone: 'pending',
      score: started ? .05 : 0,
      canStart: true,
      manual: false,
    };
  }
  if (update.state === 'available') {
    return { ...base, label: 'Different update advertised', detail: update.reason || `This relay does not currently offer v${currentPlan.targetVersion}.`, tone: 'danger', score: 0, canStart: false, manual: false };
  }
  if (started) {
    return { ...base, label: 'Checking release…', detail: `Confirming that v${currentPlan.targetVersion} is available for this relay.`, tone: 'active', score: .05, canStart: false, manual: false };
  }
  return {
    ...base,
    label: `Waiting for v${currentPlan.targetVersion}`,
    detail: 'Herdr will verify and start this relay automatically when its turn begins.',
    tone: 'pending',
    score: 0,
    canStart: true,
    manual: false,
  };
}

export function appProgress(
  currentPlan: UpdateProgressPlan | null,
  currentConnections: Map<string, RelayConnectionView>,
  currentApp: AppUpdateStatus,
): AppProgress | null {
  if (!currentPlan?.phoneAppRequired) return null;
  const target = currentPlan.phoneTarget;
  const acknowledged = currentPlan.phoneAcknowledged && phoneTargetMatchesCurrent(target);
  if (acknowledged) {
    return { label: `Phone app v${currentApp.currentVersion} loaded`, detail: 'The new bundle initialized and acknowledged its verified build identity.', tone: 'success', score: 1 };
  }
  if (currentPlan.phoneState === 'failed' || currentPlan.phoneError) {
    return { label: 'Phone app failed to load', detail: currentPlan.phoneError || 'The new bundle did not acknowledge its expected build identity.', tone: 'danger', score: 0 };
  }
  if (currentPlan.appRelayId && currentPlan.errors[currentPlan.appRelayId]) {
    return { label: 'Phone app deployment could not start', detail: currentPlan.errors[currentPlan.appRelayId], tone: 'danger', score: 0 };
  }
  const owner = currentPlan.appRelayId ? currentConnections.get(currentPlan.appRelayId) : undefined;
  if (owner?.appDeploy.state === 'failed') {
    return { label: 'Phone app deployment failed', detail: owner.appDeploy.error || 'The public app could not be verified.', tone: 'danger', score: 0 };
  }
  if (owner?.update.state === 'failed' && owner.appDeploy.state !== 'succeeded') {
    return { label: 'Phone app deployment failed', detail: owner.update.error || 'The update stopped before publishing the app.', tone: 'danger', score: 0 };
  }
  if (owner?.appDeploy.state === 'succeeded' || currentApp.deployedVersion === currentPlan.targetVersion) {
    return { label: 'Loading updated phone app…', detail: 'The verified bundle is public. This page will acknowledge it only after initialization.', tone: 'active', score: .85 };
  }
  return { label: 'Publishing phone app…', detail: 'The release is being published before the phone can load it.', tone: 'active', score: .4 };
}

export function relayUpdateMeta(connection?: RelayConnectionView) {
  if (!connection || connection.status !== 'connected') {
    return {
      label: 'Update status unavailable',
      detail: 'Connect this relay to check its version.',
      warning: false,
    };
  }
  if (relayNeedsManualBootstrap(connection)) {
    return {
      label: MANUAL_UPDATE_LABEL,
      detail: 'Open Update Help for the one-time Terminal bootstrap.',
      warning: true,
    };
  }
  const update = connection.update;
  if (update.state === 'checking') return { label: 'Checking for updates…', detail: '', warning: false };
  if (update.state === 'available') {
    return {
      label: `Update v${update.available_version} available`,
      detail: `Revision ${shortRevision(update.available_revision)}`,
      warning: true,
    };
  }
  if (update.state === 'blocked') {
    return {
      label: `Update v${update.available_version} needs attention`,
      detail: update.reason,
      warning: true,
    };
  }
  if (update.state === 'scheduled') {
    return { label: 'Update scheduled…', detail: 'Preparing the verified release.', warning: true };
  }
  if (update.state === 'preparing') {
    return { label: 'Verifying update…', detail: 'Checking release identity and transport compatibility.', warning: true };
  }
  if (update.state === 'deploying_app') {
    return { label: 'Publishing phone app…', detail: 'Waiting for the app origin to serve the verified bundle.', warning: true };
  }
  if (update.state === 'installing') {
    return { label: 'Installing update…', detail: 'The phone connection may briefly disconnect.', warning: true };
  }
  if (update.state === 'restarting') {
    return { label: 'Restarting relay…', detail: 'The phone connection may briefly disconnect.', warning: true };
  }
  if (update.state === 'succeeded') {
    return { label: 'Update installed', detail: `Running v${update.current_version}`, warning: false };
  }
  if (update.state === 'failed') {
    return { label: 'Update operation failed', detail: update.error, warning: true };
  }
  const checked = update.checked_at
    ? `Checked ${new Date(update.checked_at * 1_000).toLocaleString()}`
    : 'Update check pending';
  return { label: 'Up to date', detail: checked, warning: false };
}

export type SafeUpdateAction =
  | {
    kind: 'deploy_app' | 'install_relay';
    relayId: string;
    targetVersion: string;
    appRelayId: string;
    phoneAppRequired: boolean;
    phoneTarget: PhoneAppTarget | null;
    description: string;
  }
  | {
    kind: 'reload_app';
    targetVersion: string;
    phoneTarget: PhoneAppTarget | null;
    description: string;
  };

export interface RelayRow {
  relay: RelayConfig;
  connection?: RelayConnectionView;
}

// The owner relay is behind the released app version but can self-update to
// exactly that version, so one action can deploy the app before updating it.
export function ownerUpdateReady(connection: RelayConnectionView | undefined, upstreamVersion: string): boolean {
  if (!connection
    || connection.releaseVersion === upstreamVersion
    || relayNeedsManualBootstrap(connection)) return false;
  const update = connection.update;
  return connection.capabilities.includes('self_update')
    && update.state === 'available'
    && update.can_install
    && Boolean(update.target_revision)
    && update.available_version === upstreamVersion;
}

export function safeUpdateAction(
  appUpdate: AppUpdateStatus,
  appUpdateChecking: boolean,
  appDeploymentOwner: RelayRow | undefined,
  relayRows: RelayRow[],
  readOnlyRelayIds: Set<string>,
): SafeUpdateAction | null {
  if (appUpdateChecking || appUpdate.state === 'failed') return null;
  const targetVersion = appUpdate.upstreamVersion;
  if (appUpdate.state === 'reload-ready') {
    return {
      kind: 'reload_app',
      targetVersion: appUpdate.deployedVersion,
      phoneTarget: deployedAppTarget(appUpdate),
      description: `Load the verified phone app v${appUpdate.deployedVersion}.`,
    };
  }
  if (appUpdate.state === 'deployment-required') {
    const owner = appDeploymentOwner;
    if (!owner?.connection || !targetVersion) return null;
    if (owner.connection.releaseVersion === targetVersion
      && ['scheduled', 'deploying'].includes(owner.connection.appDeploy.state)) return null;
    if (owner.connection.releaseVersion === targetVersion) {
      return {
        kind: 'deploy_app',
        relayId: owner.relay.id,
        targetVersion,
        appRelayId: owner.relay.id,
        phoneAppRequired: true,
        phoneTarget: deployedAppTarget(appUpdate, targetVersion),
        description: `Publish the phone app from ${owner.relay.label}, then continue with any remaining relay updates.`,
      };
    }
    if (!ownerUpdateReady(owner.connection, targetVersion)) return null;
    return {
      kind: 'install_relay',
      relayId: owner.relay.id,
      targetVersion,
      appRelayId: owner.relay.id,
      phoneAppRequired: true,
      phoneTarget: deployedAppTarget(appUpdate, targetVersion),
      description: `Publish the phone app first, then update ${owner.relay.label} and continue with the remaining relays.`,
    };
  }
  const installable = relayRows.filter(({ relay, connection }) => (
    !readOnlyRelayIds.has(relay.id)
    && connection?.status === 'connected'
    && connection.capabilities.includes('self_update')
    && !relayNeedsManualBootstrap(connection)
    && connection.update.state === 'available'
    && connection.update.can_install
    && Boolean(connection.update.target_revision)
  ));
  const selected = installable.find(({ relay }) => relay.id === appDeploymentOwner?.relay.id) || installable[0];
  if (!selected?.connection) return null;
  return {
    kind: 'install_relay',
    relayId: selected.relay.id,
    targetVersion: selected.connection.update.available_version,
    appRelayId: '',
    phoneAppRequired: false,
    phoneTarget: null,
    description: `Update ${selected.relay.label} first, then continue safely with each remaining relay.`,
  };
}
