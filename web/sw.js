importScripts('notification-icons.js?v=4');

const PUSH_CATEGORIES = Object.freeze({
  attention: true,
  question: true,
  brief: true,
  finished: true,
  update: true,
  test: true,
});
const FIXED_ANDROID_ACTION = 'approve_once';

self.addEventListener('install', event => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener('activate', event => {
  event.waitUntil(self.clients.claim());
});

function validPushEventKey(value) {
  if (!value
    || typeof value !== 'object'
    || typeof value.device_id !== 'string' || !value.device_id
    || typeof value.event_id !== 'string' || !value.event_id
    || !Number.isSafeInteger(value.interaction_revision) || value.interaction_revision < 0
    || !PUSH_CATEGORIES[value.category]) return false;
  if (value.category === 'update' || value.category === 'test') return true;
  return Boolean(typeof value.server_session_id === 'string' && value.server_session_id
    && typeof value.pane_id === 'string' && value.pane_id
    && typeof value.terminal_id === 'string' && value.terminal_id
    && typeof value.agent_session_id === 'string'
    && Number.isSafeInteger(value.generation) && value.generation >= 0);
}

function notificationTag(key) {
  return [
    key.device_id,
    key.server_session_id,
    key.pane_id,
    key.terminal_id,
    key.agent_session_id || '',
    String(key.generation),
    key.event_id,
    String(key.interaction_revision),
    key.category,
  ].map(encodeURIComponent).join('|');
}

function safeAppURL(candidate) {
  try {
    const url = new URL(typeof candidate === 'string' && candidate ? candidate : './', self.location.origin + '/');
    if (url.origin === self.location.origin) return url;
  } catch (_err) {}
  return new URL('./', self.location.origin + '/');
}

function androidNotificationActions(payload) {
  const userAgent = self.navigator && self.navigator.userAgent || '';
  const notificationAPI = self.Notification;
  if (!/Android/i.test(userAgent)
    || !notificationAPI
    || !Number.isFinite(notificationAPI.maxActions)
    || notificationAPI.maxActions < 1) return [];

  const actionRef = payload.action_refs && payload.action_refs[FIXED_ANDROID_ACTION];
  const action = Array.isArray(payload.actions)
    ? payload.actions.find(candidate => candidate && candidate.action === FIXED_ANDROID_ACTION)
    : null;
  if (!action || typeof actionRef !== 'string' || !actionRef || typeof action.title !== 'string' || !action.title) {
    return [];
  }
  return [{action: FIXED_ANDROID_ACTION, title: action.title}];
}

async function closeExactNotification(tag) {
  const notifications = await self.registration.getNotifications({tag});
  for (const notification of notifications) {
    if (notification.tag === tag) notification.close();
  }
}
function legacyNotificationPayload(payload) {
  if (!payload || typeof payload !== 'object'
    || typeof payload.title !== 'string' || !payload.title
    || typeof payload.url !== 'string') return null;
  const actions = [];
  return {
    title: payload.title,
    options: {
      body: typeof payload.body === 'string' ? payload.body : '',
      tag: typeof payload.tag === 'string' ? payload.tag : '',
      icon: HERDR_NOTIFICATION_ICON,
      badge: HERDR_NOTIFICATION_BADGE,
      actions,
      data: {
        legacyURL: safeAppURL(payload.url).href,
      },
    },
  };
}


self.addEventListener('push', event => {
  let payload;
  try {
    payload = event.data ? event.data.json() : null;
  } catch (_err) {
    payload = null;
  }
  if (!payload || payload.v !== 1 || !validPushEventKey(payload.key)) {
    const legacy = legacyNotificationPayload(payload);
    if (legacy) event.waitUntil(self.registration.showNotification(legacy.title, legacy.options));
    return;
  }

  const tag = notificationTag(payload.key);
  event.waitUntil((async () => {
    // Replacement and resolution are both scoped to the complete event key.
    // Never close a prefix or another revision's notification.
    await closeExactNotification(tag);
    if (payload.retract === true) return;

    const options = {
      body: typeof payload.body === 'string' ? payload.body : '',
      tag,
      renotify: false,
      icon: HERDR_NOTIFICATION_ICON,
      badge: HERDR_NOTIFICATION_BADGE,
      actions: androidNotificationActions(payload),
      data: {
        key: payload.key,
        eventRef: typeof payload.event_ref === 'string' ? payload.event_ref : '',
        actionRefs: payload.action_refs && typeof payload.action_refs === 'object' ? payload.action_refs : {},
        url: safeAppURL(payload.url).href,
      },
    };
    await self.registration.showNotification(
      typeof payload.title === 'string' && payload.title ? payload.title : 'Herdr',
      options,
    );
  })());
});

function notificationOpenRequest(data, action) {
  const eventRef = typeof data.eventRef === 'string' ? data.eventRef : '';
  const actionRef = action === FIXED_ANDROID_ACTION
    && data.actionRefs && typeof data.actionRefs[action] === 'string'
    ? data.actionRefs[action]
    : '';
  const safeAction = actionRef ? FIXED_ANDROID_ACTION : '';
  const url = safeAppURL(data.url);
  const targeted = data.key && data.key.category !== 'update' && data.key.category !== 'test';
  if (targeted && eventRef && typeof data.key.device_id === 'string') {
    url.hash = `push=${encodeURIComponent(eventRef)}&device=${encodeURIComponent(data.key.device_id)}`;
  } else if (!targeted) {
    url.hash = 'settings';
  }
  url.searchParams.set('push_event_key', JSON.stringify(data.key));
  if (eventRef) url.searchParams.set('push_event_ref', eventRef);
  url.searchParams.set('push_unlock', 'required');
  url.searchParams.set('push_fallback', 'read_only_thread');
  if (safeAction) {
    url.searchParams.set('push_action', safeAction);
    url.searchParams.set('push_action_ref', actionRef);
  }
  return {
    // 0.19.1 only recognizes this name. Newer clients accept both this legacy
    // envelope and the typed fields below, so a just-activated worker cannot
    // lose a click while an older page is still controlled.
    type: 'herdr_notification_click',
    mode: safeAction ? 'revalidate_action' : 'thread',
    key: data.key,
    event_ref: eventRef,
    action: safeAction ? {id: safeAction, ref: actionRef} : null,
    fallback: 'read_only_thread',
    url: url.href,
  };
}
async function openOrFocusApp(request) {
  const windows = await self.clients.matchAll({type: 'window', includeUncontrolled: true});
  const appWindow = windows.find(client => client.url.startsWith(self.location.origin));
  if (appWindow) {
    let posted = false;
    try {
      appWindow.postMessage(request);
      posted = true;
    } catch (_err) {}
    try {
      const focused = await appWindow.focus();
      if (posted && focused) return;
    } catch (_err) {}
  }
  await self.clients.openWindow(request.url);
}


self.addEventListener('notificationclick', event => {
  event.notification.close();
  const data = event.notification.data || {};
  if (typeof data.legacyURL === 'string') {
    const url = safeAppURL(data.legacyURL).href;
    event.waitUntil(openOrFocusApp({type: 'herdr_notification_click', url}));
    return;
  }
  if (!validPushEventKey(data.key)) return;
  event.waitUntil(openOrFocusApp(notificationOpenRequest(data, event.action)));
});
