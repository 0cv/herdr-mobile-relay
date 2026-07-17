import { get, writable } from 'svelte/store';
import { APP_ASSET_VERSION, APP_VERSION } from './config';
import type { AppUpdateStatus, RelayUpdateStatus } from './types';

const APP_UPDATE_INTERVAL_MS = 24 * 60 * 60 * 1_000;
const PENDING_RELAY_UPDATES_KEY = 'herdr_pending_relay_updates';
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
  availableVersion: '',
  availableAssets: 0,
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
    checked_at: Number.isFinite(Number(update.checked_at)) ? Number(update.checked_at) : 0,
    can_install: update.can_install === true,
    mode: String(update.mode || '').slice(0, 20),
    reason: String(update.reason || '').slice(0, 500),
    error: String(update.error || '').slice(0, 500),
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
      const response = await fetcher(`/version.json?check=${now}`, { cache: 'no-store' });
      if (!response.ok) throw new Error(`version check returned HTTP ${response.status}`);
      const payload = await response.json() as Record<string, unknown>;
      const availableVersion = String(payload.version || '');
      if (!semverTuple(availableVersion)) throw new Error('version metadata is invalid');
      const availableAssets = Number(payload.assets);
      const status: AppUpdateStatus = {
        state: newerVersion(availableVersion, APP_VERSION) ? 'available' : 'current',
        currentVersion: APP_VERSION,
        currentAssets: APP_ASSET_VERSION,
        availableVersion,
        availableAssets: Number.isInteger(availableAssets) ? availableAssets : 0,
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
  const checkWhenDue = () => {
    const elapsed = Date.now() - get(appUpdateStatus).checkedAt;
    if (document.visibilityState === 'visible' && elapsed >= APP_UPDATE_INTERVAL_MS) {
      void checkAppUpdate();
    }
  };
  const timer = window.setInterval(checkWhenDue, APP_UPDATE_INTERVAL_MS);
  document.addEventListener('visibilitychange', checkWhenDue);
  window.addEventListener('pageshow', checkWhenDue);
  return () => {
    window.clearInterval(timer);
    document.removeEventListener('visibilitychange', checkWhenDue);
    window.removeEventListener('pageshow', checkWhenDue);
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

export async function reloadUpdatedSameOriginApp(version: string): Promise<boolean> {
  if (sessionStorage.getItem(AUTO_RELOAD_VERSION_KEY) === version) return false;
  const status = await checkAppUpdate();
  if (status.state !== 'available' || status.availableVersion !== version) return false;
  sessionStorage.setItem(AUTO_RELOAD_VERSION_KEY, version);
  location.reload();
  return true;
}

export function reloadApp(): void {
  location.reload();
}
