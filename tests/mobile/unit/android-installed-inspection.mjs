import assert from 'node:assert/strict';
import {test} from 'node:test';
import {createServer as createNetServer} from 'node:net';
import {createServer} from 'node:http';
import {createRequire} from 'node:module';
import {pathToFileURL, fileURLToPath} from 'node:url';
import {resultValidator} from './android-inspection-types.mjs';

assert.ok(process.env.APPIUM_HOME, 'Explicit owned installed fixture required');
const home = pathToFileURL(`${process.env.APPIUM_HOME}/package.json`).href;
const uiEntry = import.meta.resolve('appium-uiautomator2-driver', home);
const androidEntry = import.meta.resolve('appium-android-driver', uiEntry);
const wrapperEntry = import.meta.resolve('appium-chromedriver', androidEntry);
const {AndroidUiautomator2Driver} = await import(uiEntry);
const {UiAutomator2Server} = await import(new URL('./uiautomator2-server/core.js', uiEntry));
const {default: Chromedriver} = await import(wrapperEntry);
const {JWProxy} = await import(import.meta.resolve('@appium/base-driver', wrapperEntry));
const validateResult = resultValidator(fileURLToPath(new URL('./commands/context/retained-inspection.d.cts', androidEntry)));
const {WebSocketServer} = createRequire(new URL('./commands/context/target-inspection.cjs', androidEntry))('ws');
const BOOT = 'A'.repeat(32);
const APP = 'B'.repeat(32);
const COMMAND = 'mobile: inspectRetainedChromeTargets';
const EXPRESSION = "JSON.stringify({href:location.href,origin:location.origin,standalone:matchMedia('(display-mode: standalone)').matches || navigator.standalone === true,provider:matchMedia('(display-mode: standalone)').matches || navigator.standalone === true ? 'android-standalone' : 'browser',timeOrigin:performance.timeOrigin})";
const nativeActivity = 'org.chromium.chrome.browser.webapps.WebappActivity';
async function fixture(run, initialMode = '') {
  const calls = [];
  let adbFixture;
  const adbSockets = new Set();
  const adbServer = createNetServer((socket) => {
    adbSockets.add(socket);
    socket.on("close", () => adbSockets.delete(socket));
    socket.on("error", () => {});
    let pending = Buffer.alloc(0);
    socket.on("data", async (chunk) => {
      pending = Buffer.concat([pending, chunk]);
      if (pending.length < 4) return;
      const length = parseInt(pending.subarray(0, 4).toString(), 16);
      if (pending.length < length + 4) return;
      const service = pending.subarray(4, length + 4).toString();
      pending = pending.subarray(length + 4);
      assert.equal(pending.length, 0);
      if (state.mode === 'adb-loss') { socket.destroy(); return; }
      if (state.mode === 'adb-version') { socket.end('OKAY00040028'); return; }
      if (state.mode === 'adb-exit' && service.startsWith('shell,')) { socket.end(Buffer.from([79, 75, 65, 89, 3, 1, 0, 0, 0, 1])); return; }
      if (service === "host:transport:fixture") { socket.write("OKAY"); return; }
      if (service === "host:version") { socket.end("OKAY00040029"); return; }
      const shell = service.startsWith("shell,v2,raw:");
      assert.ok(shell || service === "host:list-forward");
      try {
        const value = await adbFixture(shell ? ["shell", ...service.slice(13).split(" ")] : ["forward", "--list"], {timeout: 30000});
        if (socket.destroyed) return;
        const body = Buffer.from(value);
        if (!shell) { socket.end(Buffer.concat([Buffer.from("OKAY" + body.length.toString(16).padStart(4, "0")), body])); return; }
        const header = Buffer.alloc(5); header[0] = 1; header.writeUInt32LE(body.length, 1);
        socket.end(Buffer.concat([Buffer.from("OKAY"), header, body, Buffer.from([3, 1, 0, 0, 0, 0])]));
      } catch { socket.end("FAIL0007refused"); }
    });
  });
  await new Promise(resolve => adbServer.listen(0, "127.0.0.1", resolve));
  const sockets = new Set();
  let driver;
  const state = {selected: APP, pid: '5301', startTime: '123456', serial: 'fixture', inode: '4321', activity: nativeActivity, mode: initialMode, count: 0, pageURL: 'https://app/#settings', extras: [], onRead: undefined};
  const nativeCalls = [];
  const nativeSockets = new Set();
  const nativeServer = createServer((req, res) => {
    nativeCalls.push(req.url);
    const path = req.url.replace('/session/native-token', '');
    assert.ok(['/window/current/size', '/appium/device/system_bars'].includes(path));
    const body = JSON.stringify({value: path === '/window/current/size' ? {width: 400, height: 800} : {statusBar: 20}});
    res.on('close', () => state.onNativeClose?.());
    if (state.holdNative && path === '/window/current/size') {
      if (state.holdNative === 'body') {
        res.writeHead(200, {'content-type': 'application/json', 'content-length': Buffer.byteLength(body)});
        res.write(body.slice(0, 5));
        state.releaseNative = () => res.end(body.slice(5));
      } else state.releaseNative = () => res.end(body);
      state.onNativeRequest?.();
      return;
    }
    res.setHeader('content-type', 'application/json');
    res.end(body);
  });
  nativeServer.on('connection', socket => { nativeSockets.add(socket); socket.on('close', () => nativeSockets.delete(socket)); });
  await new Promise(resolve => nativeServer.listen(0, '127.0.0.1', resolve));
  const document = (target) => ({href: target === APP ? state.pageURL : 'https://app/', origin: 'https://app', standalone: target === APP || state.mode === 'ambiguous', provider: target === APP || state.mode === 'ambiguous' ? 'android-standalone' : 'browser', timeOrigin: target === APP ? 100 : 50});
  const root = (target) => ({nodeType: 9, backendNodeId: target === APP ? 2 : 1, documentURL: document(target).href});
  const target = (id) => ({targetId: id, type: 'page', url: document(id).href, title: 'same', attached: true});
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const part of req) body += part;
    const payload = body ? JSON.parse(body) : undefined;
    calls.push({method: req.method, url: req.url, payload});
    state.count++;
    if (state.onRead) await state.onRead(req.url);
    if (state.mode === 'wd-stall' && req.url.endsWith('/window')) return;
    if (state.mode === 'wd-late' && req.url.endsWith('/window')) await new Promise((resolve) => setTimeout(resolve, 8050));
    let value;
    const path = req.url.replace('/session/original-token', '');
    if (path === '/json/version') {
      res.end(JSON.stringify({Browser: 'Chrome/131.0.6778.200', webSocketDebuggerUrl: `ws://127.0.0.1:${server.address().port}/devtools/browser${state.mode === 'endpoint' ? '/foreign' : ''}`}));
      return;
    }
    if ((path === '/element' && state.mode === 'lookup-miss') || (path === '/window/handles' && state.mode === 'inspection-miss')) {
      res.writeHead(404, {'content-type': 'application/json'});
      res.end(JSON.stringify({value: {error: 'no such element', message: 'Controlled ordinary lookup miss'}}));
      return;
    }
    if (path === '' && req.method === 'DELETE') value = null;
    else if (path === '/window/handles') value = state.mode === 'handles' ? [BOOT, BOOT] : [BOOT, APP];
    else if (path === '/window') value = state.selected;
    else if (path === '/goog/cdp/execute') {
      assert.equal(req.method, 'POST');
      assert.deepEqual(payload, {cmd: 'DOM.getDocument', params: {depth: 0, pierce: false}});
      value = {root: root(state.selected)};
      if (state.mode === 'wd-document') value.root.backendNodeId = 999;
    } else if (path === '/execute/sync') {
      assert.equal(req.method, 'POST');
      assert.deepEqual(payload.args, []);
      assert.equal(payload.script, `return ${EXPRESSION}`);
      value = JSON.stringify(document(state.selected));
      if (state.mode === 'wd-timeOrigin') value = JSON.stringify({...document(state.selected), timeOrigin: 999});
    } else if (path === '/url') value = document(state.selected).href;
    else throw new Error(`Unexpected owned WebDriver command ${req.method} ${req.url}`);
    if (state.mode === 'wd-large') { res.end(JSON.stringify({value: 'x'.repeat(70000)})); return; }
    res.setHeader('content-type', 'application/json');
    res.end(JSON.stringify({value}));
  });
  server.on('connection', (socket) => { sockets.add(socket); socket.on('close', () => sockets.delete(socket)); });
  const wss = new WebSocketServer({noServer: true});
  server.on('upgrade', (req, socket, head) => {
    assert.equal(req.url, '/devtools/browser');
    wss.handleUpgrade(req, socket, head, (ws) => wss.emit('connection', ws));
  });
  let connection = 0;
  wss.on('connection', (ws) => {
    const connectionId = ++connection;
    let processReads = 0;
    ws.on('message', (raw) => {
    const message = JSON.parse(raw);
    calls.push({...message, connectionId});
    let result;
    if (state.mode === 'cdp-wrong-id') { ws.send(JSON.stringify({id: message.id + 1, result: {}})); return; }
    switch (message.method) {
      case 'SystemInfo.getProcessInfo':
        processReads++;
        if (state.mode === 'process-final-loss' && processReads === 2) {
          ws.send(JSON.stringify({id: message.id, result: {processInfo: [{type: 'browser', id: 5301, cpuTime: 0}]}}));
          ws.close();
          return;
        }
        assert.equal(message.sessionId, undefined);
        result = {processInfo: [{type: 'browser', id: state.mode === 'process-string' ? '5301' : state.mode === 'process-wrong' ? 999 : 5301, cpuTime: 0}]};
        if (state.mode === 'process-missing') result.processInfo = [];
        if (state.mode === 'process-duplicate') result.processInfo.push({...result.processInfo[0]});
        if (state.mode === 'process-cpu') result.processInfo[0].cpuTime = -1;
        if (state.mode === 'process-protocol') { ws.send(JSON.stringify({id: message.id, error: {code: -32601, message: 'unsupported'}})); return; }
        break;
      case 'Target.getTargets': result = {targetInfos: [target(BOOT), target(APP), ...state.extras]}; break;
      case 'Target.attachToTarget': result = {sessionId: message.params.targetId}; break;
      case 'DOM.getDocument': result = {root: root(message.sessionId)}; break;
      case 'Runtime.evaluate':
        assert.equal(message.params.expression, EXPRESSION);
        assert.equal(message.params.throwOnSideEffect, true);
        result = {result: {type: 'string', value: JSON.stringify(document(message.sessionId))}};
        break;
      default: throw new Error(`Unexpected CDP command ${message.method}`);
    }
    ws.send(JSON.stringify({id: message.id, result, ...(message.sessionId ? {sessionId: message.sessionId} : {})}));
    });
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const port = server.address().port;
  const start = Chromedriver.prototype.start;
  const lifecycle = [];
  Chromedriver.prototype.start = async function () {
    lifecycle.push('fixture-initial-create');
    this.state = Chromedriver.STATE_ONLINE;
    this.jwproxy.sessionId = 'original-token';
    this.jwproxy.downstreamProtocol = 'W3C';
    return {browserVersion: '131.0.6778.200', chrome: {chromedriverVersion: '131.0.6778.264 (2d05e31515360f4da764174f7c448b33e36da871-refs/branch-heads/6778@{#4323})'}, 'goog:chromeOptions': {debuggerAddress: `localhost:${port}`}};
  };
  try {
    driver = new AndroidUiautomator2Driver();
    driver.sessionId = 'outer-token';
    driver.opts = {...driver.opts, browserName: 'Chrome', appPackage: 'com.android.chrome', chromedriverPort: port};
    driver.adb = {
      curDeviceId: 'fixture',
      executable: {path: '/owned/fixture-adb', defaultArgs: ['-P', String(adbServer.address().port), '-s', 'fixture']},
      async shell(args, options) {
        assert.ok(options.timeout > 0 && options.timeout <= 30000);
        calls.push({adb: args});
        if (state.onShell) await state.onShell(args);
        if (args[0] === 'pidof') return state.pid;
        if (args[0] === 'cat' && args[1].endsWith('/stat')) return `${state.pid} (chrome) S ${Array(18).fill('0').join(' ')} ${state.startTime} 0`;
        if (args[0] === 'dumpsys') return `mResumedActivity: ActivityRecord{a u0 com.android.chrome/${state.activity} t10 pid=${state.pid}}${state.extraActivities || ''}`;
        if (args[1] === '/proc/net/unix') return `000: 000 000 000 000 000 ${state.inode} @chrome_devtools_remote`;
        if (args[1] === '/proc/sys/kernel/random/boot_id') return state.mode === 'boot' ? '22222222-2222-2222-2222-222222222222' : '11111111-1111-1111-1111-111111111111';
        if (args[1]?.endsWith('/status')) {
          const pid = args[1] === '/proc/self/status' ? String(6000 + calls.length) : state.pid;
          if (state.mode === 'namespace-absent') return `Pid:\t${pid}\n`;
          if (state.mode === 'namespace-duplicate') return `Pid:\t${pid}\nNSpid:\t${pid}\nNSpid:\t${pid}\n`;
          return `Pid:\t${pid}\nNSpid:\t${state.mode === 'namespace-different' ? '1' : pid}${state.mode === 'namespace-nested' ? '\t1' : ''}\n`;
        }
        throw new Error(`Unexpected ADB shell ${args}`);
      },
      async adbExec(args, options) {
        assert.deepEqual(args, ['forward', '--list']);
        assert.ok(options.timeout > 0 && options.timeout <= 30000);
        calls.push({adb: args});
        return `${state.mode === 'serial-forward' ? 'foreign' : state.serial} tcp:${state.mode === 'forward' ? port + 1 : port} localabstract:chrome_devtools_remote`;
      },
    };
    driver.uiautomator2 = new UiAutomator2Server(driver.log, {adb: driver.adb, host: '127.0.0.1', systemPort: nativeServer.address().port, disableWindowAnimation: false});
    driver.uiautomator2.jwproxy.sessionId = 'native-token';
    driver.uiautomator2.jwproxy.downstreamProtocol = 'W3C';
    adbFixture = (args, options) => args[0] === 'shell' ? driver.adb.shell(args.slice(1), options) : driver.adb.adbExec(args, options);
    if (initialMode) {
      await assert.rejects(driver.startChromeSession());
      const count = calls.length;
      await assert.rejects(driver.startChromeSession());
      await assert.rejects(driver.executeCommand('execute', COMMAND, [{deadline: Date.now() + 8000}]));
      assert.equal(calls.length, count);
      return;
    }
    const notify = driver.notifyBiDiContextChange;
    driver.notifyBiDiContextChange = async function () {
      if (state.onContext) await state.onContext();
      return await notify.call(this);
    };
    await driver.startChromeSession();
    assert.ok(driver.chromedriver instanceof Chromedriver);
    assert.ok(driver.chromedriver.jwproxy instanceof JWProxy);
    assert.ok(driver.getProxyAvoidList().some(([method, pattern]) => method === 'POST' && pattern.test('/session/outer-token/execute/sync')), 'Real CHROMIUM dispatcher must not proxy the mobile route');
    const inspect = async () => {
      const result = await driver.executeCommand('execute', COMMAND, [{deadline: Date.now() + 8000}]);
      validateResult(result);
      assert.equal(result.processAssociation.before.pid, 5301);
      assert.equal(result.processAssociation.after.pid, 5301);
      assert.equal(result.processAssociation.before.connectionId, result.processAssociation.after.connectionId);
      const connectionCalls = calls.filter(call => call.connectionId === connection);
      assert.equal(connectionCalls[0].method, 'SystemInfo.getProcessInfo');
      assert.equal(connectionCalls.at(-1).method, 'SystemInfo.getProcessInfo');
      const malformed = structuredClone(result);
      delete malformed.before.native.startTime;
      assert.throws(() => validateResult(malformed), /declared fields/);
      return result;
    };
    const refused = async () => {
      const count = calls.length;
      await assert.rejects(inspect());
      await assert.rejects(driver.executeCommand('getScreenshot'));
      assert.equal(await driver.chromedriver.hasWorkingWebview(), false);
      await assert.rejects(driver.proxyCommand('/url', 'GET'));
      let status;
      let response;
      const res = {setHeader() {}, status(value) { status = value; return this; }, json(value) { response = value; }};
      await driver.proxyReqRes({method: 'GET', originalUrl: '/session/outer-token/url'}, res);
      assert.ok(status >= 400 && response.value.error, 'Actual JWProxy HTTP forwarding refuses quarantine');
      assert.equal(calls.length, count, 'Quarantine must stop later route, health and forwarding sends');
    };
    assert.deepEqual(calls.filter(call => call.id).map(call => call.method), ['SystemInfo.getProcessInfo', 'SystemInfo.getProcessInfo']);
    assert.equal(calls.filter(call => call.adb).length, 75);
    assert.equal(calls.filter(call => call.url).length, 6);
    assert.ok(calls.every(call => !call.adb || !call.adb.includes('ls')));
    assert.ok(!calls.some(call => call.url?.includes('/window') || call.url?.includes('/execute')));
    calls.length = 0;
    await run({driver, state, calls, nativeCalls, inspect, refused});
    assert.deepEqual(lifecycle, ['fixture-initial-create']);
    assert.ok(calls.every((call) => !call.url || call.method === 'GET' || ['/session/original-token/execute/sync', '/session/original-token/goog/cdp/execute'].includes(call.url) || (state.mode === 'dispose' && call.method === 'DELETE' && call.url === '/session/original-token') || (state.mode === 'lookup-miss' && call.method === 'POST' && call.url === '/session/original-token/element')), 'No selection/navigation/owner mutations outside explicit disposal');
  } finally {
    Chromedriver.prototype.start = start;
    if (driver) {
      await driver.clearNewCommandTimeout();
      driver.uiautomator2.jwproxy.cancelActiveRequests();
      driver.uiautomator2.jwproxy.httpAgent.destroy();
      driver.uiautomator2.jwproxy.httpsAgent.destroy();
    }
    for (const socket of nativeSockets) socket.destroy();
    await new Promise(resolve => nativeServer.close(resolve));
    for (const client of wss.clients) client.terminate();
    for (const socket of sockets) socket.destroy();
    await new Promise((resolve) => server.close(resolve));
    wss.close();
    for (const socket of adbSockets) socket.destroy();
    await new Promise(resolve => adbServer.close(resolve));
  }
}

for (const mode of ['namespace-absent', 'namespace-duplicate', 'namespace-nested', 'namespace-different', 'process-string', 'process-wrong', 'process-missing', 'process-duplicate', 'process-cpu', 'process-protocol', 'process-final-loss', 'adb-loss', 'adb-version', 'adb-exit']) {
  test(`startup association failure permanently refuses original owner: ${mode}`, () => fixture(undefined, mode));
}

for (const mode of ['adb-loss', 'adb-version', 'adb-exit']) test(`actual ADB protocol failure poisons original dispatch and JWProxy: ${mode}`, async () => fixture(async ({inspect, state, refused}) => {
  state.mode = mode;
  await assert.rejects(inspect());
  await refused();
}));

test('actual installed dispatcher: repeated bounded two-page reads and same-scope navigation', async () => fixture(async ({inspect, state, calls}) => {
  for (const path of ['#settings', '#pairing', '#complete']) {
    const count = calls.length;
    state.pageURL = `https://app/${path}`;
    const result = await inspect();
    const route = calls.slice(count);
    assert.equal(route.filter(call => call.adb).length, 52);
    assert.equal(route.filter(call => call.url).length, 18);
    assert.equal(route.filter(call => call.id).length, 18);
    assert.equal(result.phase, 'installed-selected');
    assert.equal(result.selectedHandle, APP);
    assert.equal(result.observations.length, 2);
    assert.equal(result.original.pid, '5301');
    assert.equal(result.original.startTime, '123456');
    assert.equal(result.before.native.provider, 'android-standalone');
    assert.ok(result.before.startedAt <= result.after.finishedAt);
  }
}));

test('initial browser selection is observational, installed selection is separately latched', async () => fixture(async ({inspect, state, refused}) => {
  state.selected = BOOT;
  state.activity = 'com.google.android.apps.chrome.Main';
  const initial = await inspect();
  assert.equal(initial.phase, 'initial-browser-selected');
  assert.equal(initial.before.document.provider, 'browser');
  assert.equal(initial.before.native.provider, 'browser');
  state.activity = nativeActivity;
  assert.equal((await inspect()).phase, 'initial-browser-selected');
  state.selected = APP;
  assert.equal((await inspect()).phase, 'installed-selected');
  state.selected = BOOT;
  await assert.rejects(inspect());
  await refused();
}));

for (const mode of ['ambiguous', 'unknown']) test(`consumer receives ${mode} rather than sole proof`, async () => fixture(async ({inspect, state}) => {
  state.mode = mode;
  if (mode === 'unknown') state.extras = [{targetId: 'opaque', type: 'new_document_kind', url: 'https://app/', title: 'same', attached: false}];
  const result = await inspect();
  if (mode === 'ambiguous') assert.equal(result.observations.filter((entry) => entry.document.standalone).length, 2);
  else assert.equal(result.targets.at(-1).type, 'new_document_kind');
}));

for (const mode of ['handles', 'forward', 'serial-forward', 'namespace-absent', 'namespace-duplicate', 'namespace-nested', 'namespace-different', 'boot', 'process-string', 'process-wrong', 'process-missing', 'process-duplicate', 'process-cpu', 'process-protocol', 'process-final-loss', 'endpoint', 'wd-document', 'wd-timeOrigin', 'wd-large', 'cdp-wrong-id', 'pid', 'startTime', 'inode', 'native-browser', 'current-handle', 'outer-session', 'inner-session', 'owner-object', 'adb-object', 'adb-serial', 'caps-endpoint', 'proxy-endpoint', 'wrapper-adb', 'adb-executable', 'wrong-context', 'wrong-current-owner', 'wrong-dispatch', 'inspection-miss']) {
  test(`installed rejection is sticky: ${mode}`, async () => fixture(async ({driver, inspect, state, refused}) => {
    await inspect();
    if (mode === 'pid') state.pid = '5302';
    else if (mode === 'startTime') state.startTime = '123457';
    else if (mode === 'inode') state.inode = '4322';
    else if (mode === 'native-browser') state.activity = 'com.google.android.apps.chrome.Main';
    else if (mode === 'current-handle') state.selected = BOOT;
    else if (mode === 'outer-session') driver.sessionId = 'wrong';
    else if (mode === 'inner-session') driver.chromedriver.jwproxy.sessionId = 'wrong';
    else if (mode === 'owner-object') driver.sessionChromedrivers.CHROMIUM = {};
    else if (mode === 'adb-object') driver.adb = {...driver.adb};
    else if (mode === 'adb-serial') driver.adb.curDeviceId = 'wrong';
    else if (mode === 'caps-endpoint') driver._chromedriverCapsCache.get('CHROMIUM')['goog:chromeOptions'].debuggerAddress = 'http://evil/';
    else if (mode === 'proxy-endpoint') driver.chromedriver.jwproxy.port++;
    else if (mode === 'wrapper-adb') driver.chromedriver.adb = {...driver.adb};
    else if (mode === 'adb-executable') driver.adb.executable.defaultArgs[3] = 'foreign';
    else if (mode === 'wrong-context') driver.curContext = 'NATIVE_APP';
    else if (mode === 'wrong-current-owner') driver.chromedriver = Object.create(driver.chromedriver);
    else if (mode === 'wrong-dispatch') driver.chromedriver.jwproxy.command = async () => { throw new Error('Replaced command must never run'); };
    else state.mode = mode;
    await assert.rejects(inspect());
    await refused();
  }));
}

for (const args of [[], [{}], [{deadline: Date.now() + 100000}], [{deadline: 1}], [{deadline: '8000'}], [{deadline: 0, target: APP}], [{url: 'http://evil/'}], [{script: 'location=1'}], [{targets: [APP]}], [{deadline: 0}, {}]]) {
  test(`route rejects arbitrary or unadmitted args ${JSON.stringify(args)}`, async () => fixture(async ({driver, calls, refused}) => {
    await assert.rejects(driver.executeCommand('execute', COMMAND, args));
    assert.equal(calls.length, 0);
    await refused();
  }));
}

test('wrong original driver object cannot invoke installed route', async () => fixture(async ({driver, refused}) => {
  await assert.rejects(driver.execute.call({...driver}, COMMAND, [{deadline: Date.now() + 8000}]));
  await refused();
}));

test('actual dispatcher overlap refuses before queued work or late callback can send', async () => fixture(async ({inspect, driver, state, calls, refused}) => {
  let release;
  let entered;
  const enteredPromise = new Promise((resolve) => { entered = resolve; });
  const wait = new Promise((resolve) => { release = resolve; });
  state.onShell = async () => { entered(); await wait; };
  const first = inspect();
  const rejected = assert.rejects(first);
  await enteredPromise;
  await assert.rejects(driver.executeCommand('execute', COMMAND, [{deadline: Date.now() + 8000}]));
  const count = calls.length;
  release();
  await rejected;
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(calls.length, count);
  await refused();
}));

test('native PID loss during selected observation fails before further sends', async () => fixture(async ({inspect, state, refused}) => {
  state.onRead = async (path) => { if (path.endsWith('/execute/sync')) state.pid = '5302'; };
  await assert.rejects(inspect());
  await refused();
}));

for (const mode of ['wd-stall', 'wd-late']) test(`parent deadline quarantines ${mode}`, async () => fixture(async ({inspect, state, calls, refused}) => {
  state.mode = mode;
  const started = Date.now();
  await assert.rejects(inspect());
  assert.ok(Date.now() - started < 8500);
  const count = calls.length;
  await new Promise((resolve) => setTimeout(resolve, 100));
  assert.equal(calls.length, count);
  await refused();
}));

test('explicit original disposal remains possible but cannot recreate on the same driver', async () => fixture(async ({driver, inspect, state, calls}) => {
  await inspect();
  state.mode = 'dispose';
  await driver.stopChromedriverProxies();
  assert.equal(calls.filter((call) => call.method === 'DELETE').length, 1);
  await assert.rejects(driver.startChromeSession());
  await assert.rejects(inspect());
}));

test('actual setContext uses only original local routing, never stock discovery', async () => fixture(async ({driver, state, calls, inspect}) => {
  const owner = driver.chromedriver;
  state.onShell = async () => { throw new Error('Stock discovery escaped'); };
  await driver.executeCommand('setContext', 'NATIVE_APP');
  assert.equal(driver.curContext, 'NATIVE_APP');
  assert.equal(driver.jwpProxyActive, false);
  await driver.executeCommand('setContext', 'CHROMIUM');
  assert.equal(driver.curContext, 'CHROMIUM');
  assert.equal(driver.chromedriver, owner);
  assert.equal(driver.jwpProxyActive, true);
  assert.equal(calls.length, 0);
  state.onShell = undefined;
  await inspect();
}));

for (const mode of ['deadline', 'proxy-overlap', 'native-overlap']) test(`actual retained context transition refuses ${mode} without late sends`, async () => fixture(async ({driver, state, calls, refused}) => {
  const owner = driver.chromedriver;
  await driver.executeCommand('setContext', 'NATIVE_APP');
  let release;
  let entered;
  const ready = new Promise(resolve => { entered = resolve; });
  state.onContext = async () => { entered(); await new Promise(resolve => { release = resolve; }); };
  const transition = driver.executeCommand('setContext', 'CHROMIUM');
  const rejection = assert.rejects(transition);
  await ready;
  if (mode === 'proxy-overlap') await assert.rejects(owner.jwproxy.command('/url', 'GET'));
  if (mode === 'native-overlap') await assert.rejects(driver.executeCommand('getScreenshot'));
  await rejection;
  release();
  await new Promise(resolve => setTimeout(resolve, 20));
  assert.equal(calls.length, 0);
  await refused();
}));

test('native dispatch shares one flight and cannot send a late proxy request after overlap', async () => fixture(async ({driver, calls, refused}) => {
  const owner = driver.chromedriver;
  let release;
  let entered;
  const ready = new Promise(resolve => { entered = resolve; });
  driver.getScreenshot = async () => {
    entered();
    await new Promise(resolve => { release = resolve; });
    return await owner.jwproxy.command('/url', 'GET');
  };
  const native = driver.executeCommand('getScreenshot');
  const rejection = assert.rejects(native);
  await ready;
  await assert.rejects(driver.executeCommand('setContext', 'NATIVE_APP'));
  await rejection;
  release();
  await new Promise(resolve => setTimeout(resolve, 20));
  assert.equal(calls.length, 0);
  await refused();
}));

for (const field of ['sessionId', 'port', 'request']) test(`original native transport ${field} drift refuses before dispatch`, async () => fixture(async ({driver, nativeCalls, refused}) => {
  const proxy = driver.uiautomator2.jwproxy;
  if (field === 'sessionId') proxy.sessionId = 'replacement-native';
  if (field === 'port') proxy.port++;
  if (field === 'request') proxy.request = async () => { throw new Error('Unverified native request executed'); };
  await assert.rejects(driver.executeCommand('execute', 'mobile: viewportRect', []));
  await refused();
  assert.deepEqual(nativeCalls, []);
}));

test('actual installed native viewport uses both original native HTTP requests', async () => fixture(async ({driver, nativeCalls, inspect}) => {
  const value = await driver.executeCommand('execute', 'mobile: viewportRect', []);
  assert.deepEqual(value, {left: 0, top: 20, width: 400, height: 780});
  assert.deepEqual(nativeCalls, ['/session/native-token/window/current/size', '/session/native-token/appium/device/system_bars']);
  assert.equal(driver.uiautomator2.jwproxy.getActiveRequestsCount(), 0);
  await inspect();
}));

for (const phase of ['headers', 'body']) for (const fault of ['overlap', 'deadline']) test(`actual installed native viewport ${phase} ${fault} cancels HTTP before a second send`, async () => fixture(async ({driver, state, calls, nativeCalls, refused}) => {
  state.holdNative = phase;
  const ready = new Promise(resolve => { state.onNativeRequest = resolve; });
  const closed = new Promise(resolve => { state.onNativeClose = resolve; });
  const operation = driver.executeCommand('execute', 'mobile: viewportRect', []);
  const rejection = assert.rejects(operation);
  await ready;
  assert.equal(driver.uiautomator2.jwproxy.getActiveRequestsCount(), 1);
  if (fault === 'overlap') await assert.rejects(driver.executeCommand('setContext', 'NATIVE_APP'));
  await rejection;
  await closed;
  assert.equal(driver.uiautomator2.jwproxy.getActiveRequestsCount(), 0);
  const count = calls.length;
  state.releaseNative();
  await new Promise(resolve => setTimeout(resolve, 25));
  assert.deepEqual(nativeCalls, ['/session/native-token/window/current/size']);
  assert.equal(calls.length, count);
  await assert.rejects(driver.uiautomator2.jwproxy.command('/appium/device/system_bars', 'GET'));
  await refused();
  assert.deepEqual(nativeCalls, ['/session/native-token/window/current/size']);
}));

for (const extra of [
  '\nResumedActivity: ActivityRecord{b u0 other.package/.Main t11 pid=999}',
  '\nResumed: ActivityRecord{b u0 other.package/.Main t11 pid=999}',
  '\ntopResumedActivity=ActivityRecord{b u0 other.package/.Main t11 pid=999}',
  '\nmFocusedApp=ActivityRecord{b u0 other.package/.Main t11 pid=999}',
  `\nResumedActivity: ActivityRecord{b u0 com.android.chrome/${nativeActivity} t11 pid=5301}`,
  '\nmResumedActivity: null',
  '\nmResumedActivity: ActivityRecord{malformed}',
]) test(`complete native foreground inventory refuses competing or malformed record ${extra.trim()}`, async () => fixture(async ({state, inspect, refused}) => {
  await inspect();
  const selected = state.selected;
  state.extraActivities = extra;
  await assert.rejects(inspect());
  assert.equal(state.pid, '5301');
  assert.equal(state.selected, selected);
  await refused();
}));

test('duplicate native summary for the same original ActivityRecord is admitted', async () => fixture(async ({state, inspect}) => {
  state.extraActivities = `\ntopResumedActivity=ActivityRecord{a u0 com.android.chrome/${nativeActivity} t10 pid=5301}\nmFocusedApp=ActivityRecord{a u0 com.android.chrome/${nativeActivity} t10 pid=5301}\nResumed: ActivityRecord{a u0 com.android.chrome/${nativeActivity} t10 pid=5301}`;
  await inspect();
}));

test('ordinary element lookup misses retain existing nonfatal semantics outside inspection', async () => fixture(async ({driver, inspect, state}) => {
  state.mode = 'lookup-miss';
  await assert.rejects(driver.proxyCommand('/element', 'POST', {using: 'css selector', value: '#missing'}), /Controlled ordinary lookup miss/);
  assert.equal((await inspect()).phase, 'installed-selected');
}));
