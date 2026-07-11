importScripts('notification-icons.js?v=4');

self.addEventListener('install', event => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener('activate', event => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('push', event => {
  let payload = {};
  try {
    payload = event.data ? event.data.json() : {};
  } catch (_err) {
    payload = {};
  }

  const title = payload.title || 'Herdr agent blocked';
  const actionUrls = payload.action_urls || {};
  const actions = Array.isArray(payload.actions)
    ? payload.actions
    : [
        {action: 'approve', title: 'Approve once'},
        {action: 'deny', title: 'Deny'},
      ];
  const options = {
    body: payload.body || 'An agent needs approval.',
    tag: payload.tag || 'herdr-blocked',
    renotify: true,
    icon: HERDR_NOTIFICATION_ICON,
    badge: HERDR_NOTIFICATION_BADGE,
    actions,
    data: {
      url: payload.url || './',
      actionUrls,
    },
  };

  event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener('notificationclick', event => {
  event.notification.close();
  const data = event.notification.data || {};
  const actionUrls = data.actionUrls || data.action_urls || {};
  const actionUrl = event.action && actionUrls[event.action];
  const url = new URL(actionUrl || data.url || './', self.location.origin + '/').href;

  // One action per click: the click's user activation only reliably funds a
  // single focus/openWindow call, so never chain attempts. A visible window
  // gets focused and routed in place; everything else (hidden, frozen, or no
  // client) goes straight to openWindow, which launches/raises the installed
  // PWA on Android.
  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({type: 'window', includeUncontrolled: true});
    const visible = windows.find(client =>
      client.url.startsWith(self.location.origin) && client.visibilityState === 'visible');
    if (visible) {
      try {
        visible.postMessage({type: 'herdr_notification_click', url});
      } catch (_err) {}
      try {
        await visible.focus();
      } catch (_err) {}
      return;
    }
    await self.clients.openWindow(url);
  })());
});
