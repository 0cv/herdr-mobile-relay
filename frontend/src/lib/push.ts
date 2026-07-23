import { get, writable } from 'svelte/store';
import {
  APP_PROTOCOL_VERSION,
  PUSH_ENABLED_KEY,
  PUSH_FINISHED_KEY,
  PUSH_VAPID_KEY_PREFIX,
  SERVICE_WORKER_URL,
} from './config';
import { relayProtocolError } from './protocol';
import { pushClientId, relayStore } from './store';

let rootRegistration: ServiceWorkerRegistration | null = null;
const relayRegistrations = new Map<string, ServiceWorkerRegistration>();

export interface PushPreferences {
  notificationsEnabled: boolean;
  optedIn: boolean;
  finished: boolean;
}

export function notificationsSupported(): boolean {
  return 'Notification' in window;
}

export function pushSupported(): boolean {
  return notificationsSupported() && 'serviceWorker' in navigator && 'PushManager' in window;
}

export function notificationsEnabled(): boolean {
  return notificationsSupported() && Notification.permission === 'granted';
}

export function pushOptedIn(): boolean {
  return notificationsEnabled() && localStorage.getItem(PUSH_ENABLED_KEY) !== 'false';
}

export function finishedNotificationsEnabled(): boolean {
  return localStorage.getItem(PUSH_FINISHED_KEY) === 'true';
}

function readPushPreferences(): PushPreferences {
  if (typeof window === 'undefined') {
    return { notificationsEnabled: false, optedIn: false, finished: false };
  }
  return {
    notificationsEnabled: notificationsEnabled(),
    optedIn: pushOptedIn(),
    finished: finishedNotificationsEnabled(),
  };
}

export const pushPreferences = writable<PushPreferences>(readPushPreferences());

export function refreshPushPreferences(): void {
  pushPreferences.set(readPushPreferences());
}

export function relayPushScope(relayId: string): string {
  const slug = String(relayId || 'default').toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^-+|-+$/g, '') || 'relay';
  return `./push/${slug}/`;
}

function relayVapidStorageKey(relayId: string): string {
  return `${PUSH_VAPID_KEY_PREFIX}${relayId}`;
}

async function readyServiceWorker(relayId?: string): Promise<ServiceWorkerRegistration | null> {
  if (!('serviceWorker' in navigator)) return null;
  if (relayId) {
    const current = relayRegistrations.get(relayId);
    if (current) return current;
    try {
      const registration = await navigator.serviceWorker.register(SERVICE_WORKER_URL, { scope: relayPushScope(relayId) });
      relayRegistrations.set(relayId, registration);
      return registration;
    } catch {
      return null;
    }
  }
  if (rootRegistration) return rootRegistration;
  try {
    rootRegistration = await navigator.serviceWorker.register(SERVICE_WORKER_URL);
    return rootRegistration;
  } catch {
    return null;
  }
}

export async function showPageNotification(title: string, options: NotificationOptions): Promise<void> {
  try {
    const registration = await readyServiceWorker();
    if (registration?.showNotification) {
      await registration.showNotification(title, options);
      return;
    }
  } catch {
    // Fall back to a page notification below.
  }
  try {
    const notification = new Notification(title, options);
    notification.onclick = () => {
      window.focus();
      const url = (options.data as { url?: string } | undefined)?.url;
      if (url) location.hash = new URL(url, location.href).hash;
      notification.close();
    };
  } catch {
    // Notification permission or platform support can change while the page is open.
  }
}

function urlBase64ToUint8Array(value: string): Uint8Array<ArrayBuffer> {
  const padding = '='.repeat((4 - value.length % 4) % 4);
  const raw = atob((value + padding).replace(/-/g, '+').replace(/_/g, '/'));
  return Uint8Array.from(raw, (character) => character.charCodeAt(0));
}

function uint8ArrayToUrlBase64(bytes: Uint8Array): string {
  let raw = '';
  for (const byte of bytes) raw += String.fromCharCode(byte);
  return btoa(raw).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '');
}

function subscriptionApplicationServerKey(subscription: PushSubscription): string {
  const key = subscription.options.applicationServerKey;
  return key ? uint8ArrayToUrlBase64(new Uint8Array(key)) : '';
}

async function legacyPushEndpoint(): Promise<string> {
  if (!pushSupported()) return '';
  const registration = await readyServiceWorker();
  try {
    return (await registration?.pushManager.getSubscription())?.endpoint || '';
  } catch {
    return '';
  }
}

export async function registerPushSubscription(relayId: string): Promise<boolean> {
  const connection = relayStore.connection(relayId);
  if (!connection || relayProtocolError(connection)) return false;
  if (!connection.vapidPublicKey) {
    relayStore.setPushStatus(relayId, 'missing-config');
    return false;
  }
  if (!connection.ws || connection.ws.readyState !== 1) {
    relayStore.setPushStatus(relayId, 'failed');
    return false;
  }
  if (!pushSupported() || !notificationsEnabled()) return false;
  relayStore.setPushStatus(relayId, 'syncing');
  const registration = await readyServiceWorker(relayId);
  if (!registration) {
    relayStore.setPushStatus(relayId, 'failed');
    return false;
  }

  try {
    let subscription = await registration.pushManager.getSubscription();
    const replaced: string[] = [];
    const actualKey = subscription ? subscriptionApplicationServerKey(subscription) : '';
    const savedKey = localStorage.getItem(relayVapidStorageKey(relayId)) || '';
    if (subscription && ((actualKey && actualKey !== connection.vapidPublicKey) || (!actualKey && savedKey && savedKey !== connection.vapidPublicKey))) {
      replaced.push(subscription.endpoint);
      await subscription.unsubscribe();
      subscription = null;
    }
    if (!subscription) {
      subscription = await registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: urlBase64ToUint8Array(connection.vapidPublicKey),
      });
    }
    const legacy = await legacyPushEndpoint();
    if (legacy) replaced.push(legacy);
    localStorage.setItem(relayVapidStorageKey(relayId), connection.vapidPublicKey);
    connection.ws.send(JSON.stringify({
      type: 'push_subscribe',
      protocol: APP_PROTOCOL_VERSION,
      subscription: subscription.toJSON(),
      client_id: `${pushClientId()}:${relayId}`,
      replace_endpoints: [...new Set(replaced)],
      notify_finished: finishedNotificationsEnabled(),
      user_agent: navigator.userAgent,
    }));
    relayStore.setPushStatus(relayId, 'sent');
    return true;
  } catch {
    relayStore.setPushStatus(relayId, 'failed');
    return false;
  }
}

export async function refreshPushSubscriptionState(relayId?: string): Promise<boolean> {
  if (!pushSupported() || !notificationsEnabled()) return false;
  const connections = get(relayStore.connections);
  const targets = relayId ? [connections.get(relayId)].filter(Boolean) : [...connections.values()];
  let found = false;
  for (const connection of targets) {
    if (!connection) continue;
    const registration = await readyServiceWorker(connection.relay.id);
    try {
      const subscription = await registration?.pushManager.getSubscription();
      if (!subscription) continue;
      found = true;
      const actualKey = subscriptionApplicationServerKey(subscription);
      const savedKey = localStorage.getItem(relayVapidStorageKey(connection.relay.id)) || '';
      if ((connection.vapidPublicKey && actualKey && actualKey !== connection.vapidPublicKey)
        || (connection.vapidPublicKey && savedKey && savedKey !== connection.vapidPublicKey)) {
        relayStore.setPushStatus(connection.relay.id, 'key-mismatch');
      } else if (!connection.pushStatus || ['missing-config', 'failed'].includes(connection.pushStatus)) {
        relayStore.setPushStatus(connection.relay.id, 'browser-subscribed');
      }
    } catch {
      // One bad relay scope should not prevent the other subscriptions loading.
    }
  }
  return found;
}

async function unsubscribePushSubscription(relayId: string): Promise<boolean> {
  const connection = relayStore.connection(relayId);
  const registration = await readyServiceWorker(relayId);
  const endpoints: string[] = [];
  try {
    const subscription = await registration?.pushManager.getSubscription();
    if (subscription) {
      endpoints.push(subscription.endpoint);
      await subscription.unsubscribe();
    }
  } catch {
    // Relay-side cleanup still proceeds for any endpoint we could collect.
  }
  if (connection?.ws?.readyState === 1 && !relayProtocolError(connection)) {
    connection.ws.send(JSON.stringify({
      type: 'push_unsubscribe',
      protocol: APP_PROTOCOL_VERSION,
      client_id: `${pushClientId()}:${relayId}`,
      endpoints: [...new Set(endpoints)],
    }));
  }
  localStorage.removeItem(relayVapidStorageKey(relayId));
  relayStore.setPushStatus(relayId, '');
  return Boolean(endpoints.length);
}

export async function removeRelayPushSubscription(relayId: string): Promise<void> {
  await unsubscribePushSubscription(relayId);
  if (!('serviceWorker' in navigator)) return;
  try {
    const registration = relayRegistrations.get(relayId)
      || await navigator.serviceWorker.getRegistration(relayPushScope(relayId));
    relayRegistrations.delete(relayId);
    await registration?.unregister();
  } catch {
    // Removing the relay configuration remains safe even if worker cleanup fails.
  }
}

export async function enableNotifications(): Promise<void> {
  if (!notificationsSupported() || get(relayStore.notificationBusy)) return;
  relayStore.notificationBusy.set(true);
  try {
    await Notification.requestPermission();
    if (!notificationsEnabled()) return;
    localStorage.setItem(PUSH_ENABLED_KEY, 'true');
    await readyServiceWorker();
    await Promise.all([...get(relayStore.connections).keys()].map(registerPushSubscription));
  } finally {
    refreshPushPreferences();
    relayStore.notificationBusy.set(false);
  }
}

export async function stopPushNotifications(): Promise<void> {
  if (!pushSupported() || get(relayStore.notificationBusy)) return;
  relayStore.notificationBusy.set(true);
  try {
    localStorage.setItem(PUSH_ENABLED_KEY, 'false');
    await Promise.all([...get(relayStore.connections).keys()].map(unsubscribePushSubscription));
    const legacy = await legacyPushEndpoint();
    if (legacy) {
      const registration = await readyServiceWorker();
      await (await registration?.pushManager.getSubscription())?.unsubscribe().catch(() => false);
    }
  } finally {
    refreshPushPreferences();
    relayStore.notificationBusy.set(false);
  }
}

export async function toggleNotifications(): Promise<void> {
  const connected = [...get(relayStore.connections).values()].filter((connection) => connection.status === 'connected');
  const allSynced = connected.length > 0 && connected.every((connection) => connection.pushStatus === 'subscribed');
  if (allSynced) await stopPushNotifications();
  else await enableNotifications();
}

export async function setFinishedNotifications(enabled: boolean): Promise<void> {
  if (!pushSupported() || !pushOptedIn()) return;
  localStorage.setItem(PUSH_FINISHED_KEY, enabled ? 'true' : 'false');
  refreshPushPreferences();
  relayStore.notificationBusy.set(true);
  try {
    await Promise.all([...get(relayStore.connections).keys()].map(registerPushSubscription));
  } finally {
    relayStore.notificationBusy.set(false);
  }
}

export function initializePush(): void {
  refreshPushPreferences();
  relayStore.setPushConfigHandler((relayId) => {
    if (pushOptedIn()) void registerPushSubscription(relayId);
    else void refreshPushSubscriptionState(relayId);
  });
}
