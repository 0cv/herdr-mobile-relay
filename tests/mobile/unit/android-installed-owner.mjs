import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
import {pathToFileURL} from 'node:url';
assert.ok(process.env.APPIUM_HOME, 'Owned installed fixture required');
const home = pathToFileURL(`${process.env.APPIUM_HOME}/package.json`);
const {WebSocketServer} = createRequire(home)('ws');
import {createServer} from 'node:http';
import {once} from 'node:events';
import {writeFile} from 'node:fs/promises';
import {createServer as createNetServer} from "node:net";
const adbServer = createNetServer(socket => {
  socket.on("error", () => {});
  let pending = Buffer.alloc(0);
  socket.on("data", chunk => {
    pending = Buffer.concat([pending, chunk]);
    if (pending.length < 4) return;
    const length = parseInt(pending.subarray(0, 4).toString(), 16);
    if (pending.length < length + 4) return;
    const service = pending.subarray(4, length + 4).toString();
    pending = pending.subarray(length + 4);
    if (service === "host:version") { socket.end("OKAY00040029"); return; }
    if (service === "host:transport:fixture") { socket.write("OKAY"); return; }
    const shell = service.startsWith("shell,v2,raw:");
    assert.ok(shell || service === "host:list-forward");
    const command = shell ? ["shell", ...service.slice(13).split(" ")] : ["forward", "--list"];
  let stdout;
  if (command[0] === 'forward') stdout = 'fixture tcp:' + port + ' localabstract:chrome_devtools_remote';
  else if (command[1] === 'pidof') stdout = '5301';
  else if (command[2]?.endsWith('/stat')) stdout = '5301 (chrome) S ' + Array(18).fill('0').join(' ') + ' 123456 0';
  else if (command[2] === '/proc/net/unix') stdout = '0: 0 0 0 0 0 4321 @chrome_devtools_remote';
  else if (command[2]?.endsWith('/status')) stdout = 'Pid:\t5301\nNSpid:\t5301\n';
  else if (command[2] === '/proc/sys/kernel/random/boot_id') stdout = '11111111-1111-1111-1111-111111111111';
  else if (command[1] === 'dumpsys') stdout = 'mResumedActivity: ActivityRecord{a u0 com.android.chrome/com.google.android.apps.chrome.Main t1 pid=5301}';
  else throw new Error('Unexpected fixture native read ' + command);

    const body = Buffer.from(stdout);
    if (!shell) { socket.end(Buffer.concat([Buffer.from("OKAY" + body.length.toString(16).padStart(4, "0")), body])); return; }
    const header = Buffer.alloc(5); header[0] = 1; header.writeUInt32LE(body.length, 1);
    socket.end(Buffer.concat([Buffer.from("OKAY"), header, body, Buffer.from([3,1,0,0,0,0])]));
  });
});
adbServer.listen(0, "127.0.0.1");
await once(adbServer, "listening");
const original = process.argv.includes('--original');
const ui = import.meta.resolve('appium-uiautomator2-driver', home.href);
const producer = import.meta.resolve('appium-android-driver', ui);
const wrapper = import.meta.resolve('appium-chromedriver', producer);
const base = import.meta.resolve('@appium/base-driver', wrapper);
await import(ui);
const {default: Chromedriver} = await import(wrapper);
const {JWProxy} = await import(base);
const context = await import(new URL('./commands/context/exports.js', producer));
const requests = [];
let responseError = false;
let duringHealth;
const server = createServer(async (req, res) => {
  if (req.url === '/json/version') {
    res.end(JSON.stringify({Browser:'Chrome/131.0.6778.200', webSocketDebuggerUrl:'ws://127.0.0.1:' + port + '/devtools/browser/owned'}));
    return;
  }
  requests.push(`${req.method} ${req.url}`);
  if (duringHealth) await duringHealth();
  res.writeHead(responseError ? 500 : 200, {'content-type': 'application/json'});
  res.end(JSON.stringify({value: responseError ? {error: 'invalid session id', message: 'fixture owner died'} : 'https://fixture.test/'}));
});
const wss = new WebSocketServer({server});
wss.on('connection', ws => ws.on('message', raw => {
  const message = JSON.parse(raw);
  assert.equal(message.method, 'SystemInfo.getProcessInfo');
  ws.send(JSON.stringify({id: message.id, result: {processInfo: [{type: 'browser', id: 5301, cpuTime: 0}]}}));
}));
server.listen(0, '127.0.0.1');
await once(server, 'listening');
const port = server.address().port;
let initialization = false;
let lifecycle = [];

Chromedriver.prototype.start = async function () {
  lifecycle.push(initialization ? 'fixture-create' : 'recovery-create');
  this.state = Chromedriver.STATE_ONLINE;
  this.jwproxy.sessionId = 'original-token';
  this.jwproxy.downstreamProtocol = 'W3C';
  return {browserVersion:'131.0.6778.200', chrome:{chromedriverVersion:'131.0.6778.264 (2d05e31515360f4da764174f7c448b33e36da871-refs/branch-heads/6778@{#4323})'}, 'goog:chromeOptions':{debuggerAddress:'localhost:' + port}};
};
Chromedriver.prototype.restart = async function () { lifecycle.push('restart'); return {}; };

const log = {debug() {}, info() {}, warn() {}, error() {}};
const results = [];
const scenarios = original ? ['unhealthy', 'missing', 'delete-during'] : [
  'healthy', 'caps-missing', 'unhealthy', 'throwing', 'missing', 'alternate',
  'delete-during', 'replace-during', 'replace-before', 'token-before', 'token-during',
  'background-stop', 'stopped-before', 'death-after-health', 'http-forward-failure',
  'non-chrome-healthy', 'non-chrome-unhealthy', 'non-chrome-missing', 'explicit-close-create',
];
try {
  for (const scenario of scenarios) {
    responseError = false;
    duringHealth = undefined;
    lifecycle = [];
    requests.length = 0;
    const driver = {sessionId: 'outer-token', execute: async function () {}, executeCommand: async function () {}, isChromeSession: true, opts: {browserName: 'Chrome', appPackage: 'com.android.chrome', chromedriverPort: port},
      adb: {curDeviceId: 'fixture', executable:{path:'/owned/fixture-adb',defaultArgs:['-P',String(adbServer.address().port),'-s','fixture']}}, sessionChromedrivers: {}, _chromedriverCapsCache: new Map(), log,
      uiautomator2: {jwproxy: new JWProxy({server: '127.0.0.1', port, sessionId: 'native-token'})},
      isFeatureEnabled() { return false; },
    };
    driver.onChromedriverStop = context.onChromedriverStop.bind(driver);
    driver.suspendChromedriverProxy = context.suspendChromedriverProxy.bind(driver);
    initialization = true;
    await context.startChromeSession.call(driver);
    initialization = false;
    const owner = driver.chromedriver;
    assert.ok(owner.jwproxy instanceof JWProxy);
    assert.equal(owner.sessionId(), 'original-token');
    assert.deepEqual(lifecycle, ['fixture-create']);
    lifecycle.length = 0;
    driver.curContext = 'NATIVE_APP';
    context.suspendChromedriverProxy.call(driver);
    if (scenario.startsWith('non-chrome-')) {
      driver.isChromeSession = false;
      driver.sessionChromedrivers.WEBVIEW_fixture = owner;
      if (scenario === 'non-chrome-unhealthy') responseError = true;
      if (scenario === 'non-chrome-missing') delete driver.sessionChromedrivers.WEBVIEW_fixture;
      await context.startChromedriverProxy.call(driver, 'WEBVIEW_fixture', []);
      assert.equal(driver.jwpProxyActive, true);
      assert.deepEqual(lifecycle, scenario === 'non-chrome-unhealthy' ? ['restart'] : scenario === 'non-chrome-missing' ? ['recovery-create'] : []);
      results.push({scenario, lifecycle: [...lifecycle], requests: [...requests]});
      continue;
    }
    if (scenario === 'explicit-close-create') {

      await context.stopChromedriverProxies.call(driver);
      assert.deepEqual(requests, ['DELETE /session/original-token']);
      assert.equal(driver.chromedriver, undefined);
      assert.deepEqual(driver.sessionChromedrivers, {});
      await assert.rejects(() => context.startChromeSession.call(driver));
      const next = {...driver, sessionChromedrivers: {}, _chromedriverCapsCache: new Map(), uiautomator2: {jwproxy: new JWProxy({server: '127.0.0.1', port, sessionId: 'next-native-token'})}};
      next.onChromedriverStop = context.onChromedriverStop.bind(next);
      initialization = true;
      await context.startChromeSession.call(next);
      initialization = false;
      assert.notEqual(next.chromedriver, owner);
      assert.deepEqual(lifecycle, ['fixture-create']);
      await context.startChromedriverProxy.call(next, 'CHROMIUM', []);
      assert.equal(next.jwpProxyActive, true);
      results.push({scenario, lifecycle: [...lifecycle], requests: [...requests]});
      continue;
    }
    const caps = {fixture: true};
    if (scenario === 'caps-missing') driver._chromedriverCapsCache.delete('CHROMIUM');
    if (scenario !== 'caps-missing') driver._chromedriverCapsCache.set('CHROMIUM', caps);
    if (scenario === 'unhealthy') responseError = true;
    if (scenario === 'throwing') owner.hasWorkingWebview = async () => { throw Error('fixture health throw'); };
    if (scenario === 'missing') delete driver.sessionChromedrivers.CHROMIUM;
    const replacement = new Chromedriver({port: String(port)});
    replacement.state = Chromedriver.STATE_ONLINE;
    replacement.jwproxy.sessionId = 'original-token';
    if (scenario === 'replace-before') driver.sessionChromedrivers.CHROMIUM = replacement;
    if (scenario === 'token-before') owner.jwproxy.sessionId = 'replacement-token';
    if (scenario === 'stopped-before') owner.state = Chromedriver.STATE_STOPPED;
    if (scenario === 'background-stop') await driver.onChromedriverStop('CHROMIUM');
    if (scenario === 'delete-during') duringHealth = async () => { delete driver.sessionChromedrivers.CHROMIUM; };
    if (scenario === 'replace-during') duringHealth = async () => { driver.sessionChromedrivers.CHROMIUM = replacement; };
    if (scenario === 'token-during') duringHealth = async () => { owner.jwproxy.sessionId = 'replacement-token'; };
    let error;
    try { await context.startChromedriverProxy.call(driver, scenario === 'alternate' ? 'WEBVIEW_alternate' : 'CHROMIUM', []); }
    catch (caught) { error = caught; }
    const healthy = ['healthy', 'caps-missing', 'death-after-health', 'http-forward-failure'].includes(scenario);
    if (original) {
      if (scenario === 'unhealthy') assert.deepEqual(lifecycle, ['restart']);
      if (scenario === 'missing') assert.deepEqual(lifecycle, ['recovery-create']);
      if (scenario === 'delete-during') {
        assert.equal(driver.jwpProxyActive, true);
        assert.equal(driver.sessionChromedrivers.CHROMIUM, undefined);
      }
    } else if (healthy) {
      assert.equal(error, undefined);
      assert.equal(driver.chromedriver, owner);
      assert.equal(owner.sessionId(), 'original-token');
      assert.equal(driver.jwpProxyActive, true);
      if (scenario !== 'caps-missing') assert.equal(driver._chromedriverCapsCache.get('CHROMIUM'), caps);
      if (scenario === 'death-after-health' || scenario === 'http-forward-failure') {
        responseError = true;
        await assert.rejects(() => owner.sendCommand('/url', 'GET'));
        if (scenario === 'death-after-health') owner.state = Chromedriver.STATE_STOPPED;
        context.suspendChromedriverProxy.call(driver);
        await assert.rejects(() => context.startChromedriverProxy.call(driver, 'CHROMIUM', []));
      }
    } else {
      assert.ok(error, scenario);
      assert.equal(driver.jwpProxyActive, false);
      assert.equal(driver.chromedriver, undefined);
      const count = requests.length;
      responseError = false;
      duringHealth = undefined;
      driver.sessionChromedrivers.CHROMIUM = owner;
      owner.state = Chromedriver.STATE_ONLINE;
      owner.jwproxy.sessionId = 'original-token';
      await assert.rejects(() => context.startChromedriverProxy.call(driver, 'CHROMIUM', []));
      assert.equal(requests.length, count, 'refused owner must not be rehabilitated');
    }
    if (!original) assert.deepEqual(lifecycle, []);
    assert.ok(requests.every(request => request === 'GET /session/original-token/url'));
    results.push({scenario, refused: !!error, lifecycle: [...lifecycle], requests: [...requests]});
  }
  await writeFile(new URL(original ? 'original-integration.json' : 'guarded-integration.json', import.meta.url), JSON.stringify({ui, producer, wrapper, base, results}, null, 2));
  console.log(`${original ? 'original' : 'guarded'} installed Node ESM + loopback JWProxy: ${results.length} cases passed`);
} finally {
  server.closeAllConnections();
  await new Promise(resolve => server.close(resolve));
  await new Promise(resolve => adbServer.close(resolve));
}
