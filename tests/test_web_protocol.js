const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const html = fs.readFileSync('web/index.html', 'utf8');
const serviceWorker = fs.readFileSync('web/sw.js', 'utf8');
const versionMatch = html.match(/const APP_PROTOCOL_VERSION = (\d+);/);
const start = html.indexOf('function relayVersionMeta');
const end = html.indexOf('function pushStatusMeta', start);

assert.ok(versionMatch, 'App protocol constant not found');
assert.ok(start >= 0 && end > start, 'Relay protocol helpers not found');

const sandbox = {};
vm.runInNewContext(`
const APP_PROTOCOL_VERSION = ${versionMatch[1]};
${html.slice(start, end)}
this.APP_PROTOCOL_VERSION = APP_PROTOCOL_VERSION;
this.relayVersionMeta = relayVersionMeta;
this.relayProtocolError = relayProtocolError;
`, sandbox);

assert.equal(sandbox.APP_PROTOCOL_VERSION, 1);
assert.equal(sandbox.relayProtocolError({protocol: 1}), '');
assert.match(sandbox.relayProtocolError({protocol: 0}), /Waiting for the relay protocol handshake/);
assert.match(sandbox.relayProtocolError({protocol: 2}), /Incompatible relay protocol v2/);
assert.equal(sandbox.relayVersionMeta({status: 'connected', protocol: 1, version: 'abc1234'}).label, 'relay abc1234');
assert.match(sandbox.relayVersionMeta({status: 'connected', protocol: 2, version: 'future'}).label, /App outdated/);

assert.match(
  html,
  /function sendCommand[\s\S]*?const protocolError = relayProtocolError\(conn\);[\s\S]*?Promise\.reject\(new Error\(protocolError\)\)/
);
assert.match(html, /type: 'upload_image',[\s\S]*?protocol: APP_PROTOCOL_VERSION/);
assert.match(html, /type: 'push_subscribe',[\s\S]*?protocol: APP_PROTOCOL_VERSION/);
assert.match(html, /type: 'push_subscribe',[\s\S]*?notify_finished: finishedNotificationsEnabled\(\)/);
assert.match(html, /id="finishedNotificationToggle"[\s\S]*?onchange="setFinishedNotificationsEnabled\(this\.checked\)"/);
assert.match(serviceWorker, /const actions = Array\.isArray\(payload\.actions\)/);
assert.match(serviceWorker, /actions,/);
assert.match(serviceWorker, /notificationActions\.length === 1 && notificationActions\[0\]\.action === 'approve'/);
assert.doesNotMatch(serviceWorker, /\{action: 'deny', title: 'Deny'\}/);
assert.match(html, /const SW_SCRIPT_URL = 'sw\.js\?v=7'/);

const handlers = {};
let routedNotificationUrl = '';
const visibleClient = {
  url: 'https://relay.example/app',
  visibilityState: 'visible',
  postMessage: message => { routedNotificationUrl = message.url; },
  focus: async () => {},
};
const workerSandbox = {
  URL,
  importScripts: () => {},
  self: {
    location: {origin: 'https://relay.example'},
    addEventListener: (name, handler) => { handlers[name] = handler; },
    clients: {
      matchAll: async () => [visibleClient],
      openWindow: async url => { routedNotificationUrl = url; },
      claim: async () => {},
    },
    registration: {showNotification: async () => {}},
    skipWaiting: async () => {},
  },
};
vm.runInNewContext(serviceWorker, workerSandbox);

(async () => {
  let clickPromise;
  handlers.notificationclick({
    action: 'deny',
    notification: {
      actions: [{action: 'approve', title: 'Approve once'}],
      data: {
        url: './#open',
        actionUrls: {approve: './#approve', deny: './#deny'},
      },
      close: () => {},
    },
    waitUntil: promise => { clickPromise = promise; },
  });
  await clickPromise;
  assert.match(routedNotificationUrl, /#approve$/);
  console.log('Relay protocol compatibility tests passed');
})().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
