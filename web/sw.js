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
  const iconUrl = new URL('icons/icon-512.png', self.location.origin + '/').href;
  const badgeUrl = new URL('icons/notification-badge.png', self.location.origin + '/').href;
  const options = {
    body: payload.body || 'An agent needs approval.',
    tag: payload.tag || 'herdr-blocked',
    renotify: true,
    icon: iconUrl,
    badge: badgeUrl,
    data: {
      url: payload.url || './',
    },
  };

  event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener('notificationclick', event => {
  event.notification.close();
  const url = new URL(event.notification.data && event.notification.data.url || './', self.location.origin + '/').href;

  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({type: 'window', includeUncontrolled: true});
    for (const client of windows) {
      if (client.url.startsWith(self.location.origin)) {
        let targetClient = client;
        if ('navigate' in client && client.url !== url) {
          try {
            targetClient = await client.navigate(url) || client;
          } catch (_err) {
            targetClient = client;
          }
        }
        try {
          targetClient.postMessage({type: 'herdr_notification_click', url});
        } catch (_err) {}
        return targetClient.focus();
      }
    }
    return self.clients.openWindow(url);
  })());
});
