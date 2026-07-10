const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const html = fs.readFileSync('web/index.html', 'utf8');
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

console.log('Relay protocol compatibility tests passed');
