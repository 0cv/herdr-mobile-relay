import assert from 'node:assert/strict';
import {test} from 'node:test';
import {gzipSync} from 'node:zlib';
import {readFileSync} from 'node:fs';
const disabledSelf = readFileSync(new URL('./fixtures/android/kernel-disabled-self-status.txt', import.meta.url), 'utf8');
const disabledChrome = readFileSync(new URL('./fixtures/android/kernel-disabled-chrome-status.txt', import.meta.url), 'utf8');
import {createServer as createNetServer} from 'node:net';
import {createServer} from 'node:http';
import {createRequire} from 'node:module';
import {spawnSync} from 'node:child_process';
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
const SCRIPT = "JSON.stringify({href:location.href,origin:location.origin,standalone:matchMedia('(display-mode: standalone)').matches || navigator.standalone === true,provider:matchMedia('(display-mode: standalone)').matches || navigator.standalone === true ? 'android-standalone' : 'browser',timeOrigin:performance.timeOrigin})";
const CORE_EXPRESSION = "JSON.stringify({href:location.href,origin:location.origin,timeOrigin:performance.timeOrigin})";
const nativeActivity = 'org.chromium.chrome.browser.webapps.WebappActivity';
async function fixture(run, initialMode = '') {
  const calls = [];
  const initialPid = initialMode === 'disabled' ? 2863 : 5301;
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
  let directSocket;
  const state = {selected: APP, pid: String(initialPid), startTime: '123456', serial: 'fixture', inode: '4321', activity: nativeActivity, mode: initialMode, count: 0, pageURL: 'https://app/#settings', extras: [], onRead: undefined, eventSent: false};
  const diagnosticEvent = (sessionId = APP) => {
    const topLayer = state.mode.startsWith('late-top-layer-');
    const event = {
      method: state.mode === 'unknown-event' ? 'DOM.secretNotification' : topLayer ? 'DOM.topLayerElementsUpdated' : 'DOM.childNodeCountUpdated',
      ...(sessionId ? {sessionId} : {}),
    };
    if (!state.mode.endsWith('-missing-params')) {
      event.params = topLayer
        ? state.mode.endsWith('-malformed') ? {unexpected: true} : {}
        : state.mode === 'late-event-secrets'
          ? {nodeId: 'SESSION_SECRET', childNodeCount: 2, secret: 'CREDENTIAL_SECRET', url: 'https://attacker.invalid/'}
          : {nodeId: 7, childNodeCount: 2};
    }
    if (state.mode.endsWith('-id')) event.id = 19;
    if (state.mode.endsWith('-result')) event.result = {};
    if (state.mode.endsWith('-error')) event.error = {code: -32000};
    if (state.mode.endsWith('-extra-field')) event.unexpected = true;
    return JSON.stringify(event);
  };
  const nativeCalls = [];
  const nativeSockets = new Set();
  const nativeServer = createServer((req, res) => {
    nativeCalls.push(req.url);
    const path = req.url.replace('/session/native-token', '');
    let value;
    if (path === '/window/current/size') value = {width: 400, height: 800};
    else if (path === '/appium/device/system_bars') value = {statusBar: 20};
    else if (path === '/source') value = '<hierarchy><node/></hierarchy>';
    else if (path === '/element/element-2/rect') value = {x: 10, y: 20, width: 100, height: 40};
    else assert.fail(`Unexpected native WebDriver command ${req.method} ${req.url}`);
    const body = JSON.stringify({value});
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
      res.end(JSON.stringify({Browser: 'Chrome/131.0.6778.200', webSocketDebuggerUrl: `ws://127.0.0.1:${port}/devtools/browser${state.mode === 'endpoint' ? '/foreign' : ''}`}));
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
      assert.equal(payload.script, `return ${SCRIPT}`);
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
    directSocket = ws;
    const connectionId = ++connection;
    let processReads = 0;
    let evaluationCount = 0;
    ws.on('message', (raw) => {
    const message = JSON.parse(raw);
    calls.push({...message, connectionId});
    let result;
    if (state.mode === 'cdp-wrong-id') { ws.send(JSON.stringify({id: message.id + 1, result: {}})); return; }
    if (state.mode === 'frame-partial') { ws._socket.write(Buffer.from([0x81, 126, 0, 100, 123])); return; }
    if (state.mode === 'frame-large') { ws.send('x'.repeat(70000)); return; }
    if (state.mode === 'frame-malformed') { ws.send('{'); return; }
    if (state.mode === 'frame-binary') {
      ws.send(Buffer.from([0x7b]));
      ws.send(JSON.stringify({method: 'Target.secretNotification', params: {secret: 'CREDENTIAL_SECRET'}}));
      return;
    }
    if (state.mode === 'frame-flood') {
      ws.send(JSON.stringify({method: 'Target.targetCreated', params: {}}));
      for (let index = 0; index < 401; index++) ws.send(JSON.stringify({method: 'Target.secretNotification', params: {secret: 'CREDENTIAL_SECRET'}}));
      return;
    }
    if (state.mode === 'unknown-event' && message.id === 1) {
      ws.send(JSON.stringify({method: 'Target.secretNotification', params: {secret: 'CREDENTIAL_SECRET', url: 'https://attacker.invalid/'}}));
      return;
    }
    if (state.mode === 'id-event' && message.id === 1) {
      ws.send(JSON.stringify({id: message.id, method: 'DOM.childNodeCountUpdated', params: {nodeId: 7, childNodeCount: 2}}));
      return;
    }
    if ((state.mode === 'event-error' || state.mode === 'event-result') && message.id === 1) {
      ws.send(JSON.stringify({method: 'DOM.childNodeCountUpdated', params: {nodeId: 7, childNodeCount: 2}, ...(state.mode === 'event-error' ? {error: {code: -32000, message: 'not a reply'}} : {result: {ignored: true}})}));
      return;
    }
    if ((state.mode === 'pending-event' && message.id === 4) || (state.mode === 'late-top-layer-pending' && message.id === 18)) ws.send(diagnosticEvent());
    switch (message.method) {
      case 'SystemInfo.getProcessInfo':
        processReads++;
        if (state.mode === 'process-final-loss' && processReads === 2) {
          ws.send(JSON.stringify({id: message.id, result: {processInfo: [{type: 'browser', id: initialPid, cpuTime: 0}]}}));
          ws.close();
          return;
        }
        assert.equal(message.sessionId, undefined);
        result = {processInfo: [{type: 'browser', id: state.mode === 'process-string' ? '5301' : state.mode === 'process-wrong' ? 999 : initialPid, cpuTime: 0}]};
        if (state.mode === 'process-missing') result.processInfo = [];
        if (state.mode === 'process-duplicate') result.processInfo.push({...result.processInfo[0]});
        if (state.mode === 'process-cpu') result.processInfo[0].cpuTime = -1;
        if (state.mode === 'process-protocol') { ws.send(JSON.stringify({id: message.id, error: {code: -32601, message: 'unsupported'}})); return; }
        break;
      case 'Target.getTargets': result = {targetInfos: [target(BOOT), target(APP), ...state.extras]}; break;
      case 'Target.attachToTarget':
        result = {sessionId: message.params.targetId};
        if (state.mode === 'late-top-layer-event') {
          ws.send(JSON.stringify({
            method: 'Target.attachedToTarget',
            params: {sessionId: message.params.targetId, targetInfo: target(message.params.targetId), waitingForDebugger: false},
          }));
        }
        break;
      case 'DOM.getDocument': result = {root: root(message.sessionId)}; break;
      case 'Runtime.evaluate': {
        assert.equal(message.params.expression, CORE_EXPRESSION);
        assert.equal(message.params.throwOnSideEffect, true);
        evaluationCount++;
        const selectedDocument = document(message.sessionId);
        result = {result: {type: 'string', value: JSON.stringify({ href: selectedDocument.href, origin: selectedDocument.origin, timeOrigin: selectedDocument.timeOrigin })}};
        const sideEffect = 'EvalError: Possible side-effect in debug-evaluate';
        const secretStack = `${sideEffect}\n    at read (https://attacker.invalid/session=SESSION_SECRET/pid=1234/token=CREDENTIAL_SECRET/${APP}:1:2)`;
        const rejectInitial = evaluationCount === 1;
        const rejectRepeat = evaluationCount === 3;
        const exception = (className, description) => {
          result.result = {type: 'object', subtype: 'error', className, description};
          result.exceptionDetails = {exception: {className, description}};
        };
        if ((state.mode === 'observation-exception' && rejectInitial)
          || (state.mode === 'observation-exception-stack' && rejectInitial)
          || (state.mode === 'observation-hostile' && rejectInitial)) {
          exception('EvalError', state.mode === 'observation-exception-stack' ? `${sideEffect}\n    at evaluate (https://fixture.test/app.js:1:2)` : state.mode === 'observation-hostile' ? secretStack : sideEffect);
        } else if ((state.mode === 'observation-repeat-exception' && rejectRepeat)
          || (state.mode === 'observation-repeat-exception-stack' && rejectRepeat)) {
          exception('EvalError', state.mode === 'observation-repeat-exception-stack' ? secretStack : sideEffect);
        } else if (state.mode === 'observation-old-message' && rejectInitial) {
          exception('EvalError', 'EvalError: Possible side-effect in debuggee detected');
        } else if (state.mode === 'observation-same-line-stack' && rejectInitial) {
          exception('EvalError', `${sideEffect} at evaluate`);
        } else if (state.mode === 'observation-wrong-class' && rejectInitial) {
          exception('Error', sideEffect);
        } else if (state.mode === 'observation-malformed-stack' && rejectInitial) {
          exception('EvalError', `${sideEffect}\n    at`);
        } else if (state.mode === 'observation-nonstring') {
          result.result = {type: 'object', value: {untrusted: 'expression-result'}};
        } else if (state.mode === 'observation-both') {
          result.result = {type: 'object', value: {untrusted: 'expression-result'}};
          result.exceptionDetails = {exception: {className: 'TypeError', description: 'arbitrary exception text'}};
        } else if (state.mode === 'observation-malformed') {
          result.result = {type: null};
          result.exceptionDetails = {exception: {className: ['EvalError'], description: {untrusted: 'malformed'}}};
        }
        break;
      }
      default: throw new Error(`Unexpected CDP command ${message.method}`);
    }
    const response = {id: message.id, result, ...(message.sessionId ? {sessionId: message.sessionId} : {})};
    if (state.mode === 'result-malformed') response.result = [];
    if ((state.mode === 'between-passes' || state.mode === 'late-top-layer-between-passes') && message.id === 10) {
      ws._socket.cork();
      try { ws.send(JSON.stringify(response)); ws.send(diagnosticEvent(BOOT)); }
      finally { ws._socket.uncork(); }
      return;
    }
    ws.send(JSON.stringify(response));
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
        if (args[1] === '/proc/config.gz') {
          if (state.mode === 'config-unreadable') throw new Error('Fixture config unreadable');
          const configs = {'config-missing': 'CONFIG_NAMESPACES=y\n', 'config-malformed': 'CONFIG_PID_NS=n\n',
            'config-duplicate': 'CONFIG_PID_NS=y\nCONFIG_PID_NS=y\n', 'config-conflicting': 'CONFIG_PID_NS=y\n# CONFIG_PID_NS is not set\n'};
          return gzipSync('CONFIG_IKCONFIG=y\nCONFIG_IKCONFIG_PROC=y\n' + (configs[state.mode] ?? (['disabled', 'disabled-contradictory'].includes(state.mode) ? 'CONFIG_NAMESPACES=y\n# CONFIG_PID_NS is not set\n' : 'CONFIG_PID_NS=y\n')));
        }
        if (args[0] === 'cat' && args[1].endsWith('/stat')) return `${state.pid} (chrome) S ${Array(18).fill('0').join(' ')} ${state.startTime} 0`;
        if (args[0] === 'dumpsys') return `mResumedActivity: ActivityRecord{a u0 com.android.chrome/${state.activity} t10 pid=${state.pid}}${state.extraActivities || ''}`;
        if (args[1] === '/proc/net/unix') {
          const listening = (inode) => `0000000000000000: 00000002 00000000 00010000 0001 01 ${inode} @chrome_devtools_remote`;
          const connected = (index = 1) => `${index.toString(16).padStart(16, '0')}: 00000001 00000000 00000000 0001 03 ${9000 + index} @chrome_devtools_remote`;
          if (state.mode === 'socket-connected' || state.mode === 'socket-connected-drift') return [...[1, 2, 3, 4].map(connected), listening(state.inode)].join('\n');
          if (state.mode === 'socket-connected-only') return connected();
          if (state.mode === 'socket-duplicate-listen') return [listening(state.inode), listening('4322'), listening('4323'), listening('4324')].join('\n');
          if (state.mode === 'socket-zero-inode') return listening('0');
          if (state.mode === 'socket-malformed') return `0000000000000000: 00000002 00000000 00010000 0001 01 malformed @chrome_devtools_remote`;
          if (state.mode === 'socket-wrong-type') return '0000000000000000: 00000002 00000000 00010000 0002 01 4321 @chrome_devtools_remote';
          if (state.mode === 'socket-wrong-state') return '0000000000000000: 00000002 00000000 00010000 0001 03 4321 @chrome_devtools_remote';
          return listening(state.inode);
        }
        if (args[1] === '/proc/sys/kernel/random/boot_id') {
          state.bootReads = (state.bootReads || 0) + 1;
          return state.mode === 'boot' || (state.mode === 'boot-acquisition' && state.bootReads === 2) ? '22222222-2222-2222-2222-222222222222' : '11111111-1111-1111-1111-111111111111';
        }
        if (args[1]?.endsWith('/status')) {
          const pid = args[1] === '/proc/self/status' ? String(6000 + calls.length) : state.pid;
          if (state.mode === 'disabled') return args[1] === '/proc/self/status' ? disabledSelf : disabledChrome;
          if (state.mode === 'pid-duplicate') return `Pid:\t${pid}\nPid:\t${pid}\nNSpid:\t${pid}\n`;
          if (state.mode === 'namespace-same-number-nested') return `Pid:\t${pid}\nNSpid:\t${pid}\t${pid}\n`;
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
    if (initialMode && !['disabled', 'socket-connected'].includes(initialMode)) {
      let firstError;
      try { await driver.startChromeSession(); } catch (error) { firstError = error; }
      assert.ok(firstError);
      if (initialMode.startsWith('socket-')) {
        assert.match(String(firstError), /Live Chrome socket unavailable/u);
        assert.match(String(firstError), /failurePredicate/u);
      }
      if (run) await run({initialError: firstError});
      const count = calls.length;
      state.mode = '';
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
    state.onRead = async () => {
      if ((['late-event', 'late-event-secrets'].includes(state.mode) || (state.mode.startsWith('late-top-layer-') && !['late-top-layer-pending', 'late-top-layer-between-passes'].includes(state.mode)))
        && !state.eventSent && calls.filter(call => call.connectionId === connection && call.id).at(-1)?.id === 18) {
        state.eventSent = true;
        const sessionId = state.mode === 'late-top-layer-root' ? null : state.mode === 'late-top-layer-unselected' ? BOOT : state.mode === 'late-top-layer-unknown' ? 'unowned' : APP;
        const count = state.mode === 'late-top-layer-flood' ? 401 : 1;
        for (let index = 0; index < count; index++) directSocket.send(diagnosticEvent(sessionId));
        if (state.mode === 'late-top-layer-target-created') {
          state.extras = [target('new-target')];
          directSocket.send(JSON.stringify({method: 'Target.targetCreated', params: {targetInfo: target('new-target')}}));
        }
        if (state.mode === 'late-top-layer-document-updated') {
          state.pageURL = 'https://changed.invalid/';
          directSocket.send(JSON.stringify({method: 'DOM.documentUpdated', sessionId: APP, params: {}}));
        }
        if (state.mode === 'late-top-layer-document-change') state.pageURL = 'https://changed.invalid/';
        if (state.mode === 'late-top-layer-owner-after') state.pid = String(initialPid + 1);
        await new Promise(resolve => setTimeout(resolve, state.mode === 'late-top-layer-deadline' ? 8100 : 20));
      }
    };
    const inspect = async () => {
      const result = await driver.executeCommand('execute', COMMAND, [{deadline: Date.now() + 8000}]);
      validateResult(result);
      assert.equal(result.processAssociation.before.pid, initialPid);
      assert.equal(result.processAssociation.after.pid, initialPid);
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
    assert.equal(calls.filter(call => call.adb).length, 78);
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

for (const mode of ['boot-acquisition', 'config-unreadable', 'config-missing', 'config-malformed', 'config-duplicate', 'config-conflicting', 'disabled-contradictory', 'pid-duplicate', 'namespace-same-number-nested', 'namespace-absent', 'namespace-duplicate', 'namespace-nested', 'namespace-different', 'process-string', 'process-wrong', 'process-missing', 'process-duplicate', 'process-cpu', 'process-protocol', 'process-final-loss', 'adb-loss', 'adb-version', 'adb-exit', 'socket-connected-only', 'socket-duplicate-listen', 'socket-zero-inode', 'socket-malformed', 'socket-wrong-type', 'socket-wrong-state']) {
  test(`startup association failure permanently refuses original owner: ${mode}`, () => fixture(undefined, mode));
}

test('installed producer selects the LISTEN socket after four connected rows retain the same abstract name', async () => fixture(async ({inspect}) => {
  const result = await inspect();
  assert.equal(result.before.forward.inode, '4321');
  assert.equal(result.after.forward.inode, '4321');
}, 'socket-connected'));

test('installed producer selected-listener facts survive four connected rows during association drift', async () => fixture(async ({inspect, state}) => {
  await inspect();
  state.mode = 'socket-connected-drift';
  state.inode = '4322';
  await assert.rejects(inspect, error => {
    assert.match(String(error), /"failurePredicate":"browser-forward-association-drift"/u);
    assert.match(String(error), /"inode":"4322"/u);
    return true;
  });
}, 'socket-connected'));

test('installed producer diagnostic survives the actual WebDriver 500-character consumer boundary', async () => fixture(async ({initialError}) => {
  const source = new URL('../support/webdriver.ts', import.meta.url).href;
  const script = `import {AppiumClient, WebDriverError} from ${JSON.stringify(source)};
const client = new AppiumClient('http://fixture', 1000, async () => new Response(JSON.stringify({value: {error: 'unknown error', message: process.env.PRODUCER_ERROR}}), {status: 500}));
try { await client.create({capabilities: {}}); process.exit(2); }
catch (error) { if (!(error instanceof WebDriverError)) throw error; process.stdout.write(error.message); }
`;
  const result = spawnSync('bun', ['-e', script], {env: {...process.env, PRODUCER_ERROR: String(initialError)}, encoding: 'utf8'});
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /failurePredicate.*unique-listening-row-ambiguous/u);
}, 'socket-duplicate-listen'));

for (const mode of ['adb-loss', 'adb-version', 'adb-exit']) test(`actual ADB protocol failure poisons original dispatch and JWProxy: ${mode}`, async () => fixture(async ({inspect, state, refused}) => {
  state.mode = mode;
  await assert.rejects(inspect());
  await refused();
}));

test('saved native disabled statuses with constructed gzip pass installed producer and contradictory later fields fail sticky', async () => fixture(async ({inspect, state, calls, refused}) => {
  const result = await inspect();
  assert.equal(result.original.namespace, 'kernel-pid-namespaces-disabled');
  assert.equal(result.original.kernelCapability.mode, 'disabled');
  assert.equal(result.original.kernelCapability.source, '/proc/config.gz');
  assert.match(result.original.kernelCapability.sha256, /^[0-9a-f]{64}$/);
  assert.deepEqual(result.before.native.kernelCapability, result.original.kernelCapability);
  assert.equal(calls.filter(call => call.adb?.includes('/proc/config.gz')).length, 0, 'Boot-bound built-in config is acquired once, not relearned');
  state.mode = '';
  await assert.rejects(inspect(), /mode=disabled/);
  state.mode = 'disabled';
  await refused();
}, 'disabled'));

for (const [mode, expected] of [
  ['observation-exception', {failurePredicate: 'document-observation-unavailable', targetOrdinal: 1, observationPass: 'initial', exceptionDetails: true, exceptionClass: 'EvalError', exceptionCause: 'known-side-effect-rejection', remoteType: 'object'}],
  ['observation-exception-stack', {failurePredicate: 'document-observation-unavailable', targetOrdinal: 1, observationPass: 'initial', exceptionDetails: true, exceptionClass: 'EvalError', exceptionCause: 'known-side-effect-rejection', remoteType: 'object'}],
  ['observation-repeat-exception', {failurePredicate: 'document-observation-unavailable', targetOrdinal: 1, observationPass: 'repeat', exceptionDetails: true, exceptionClass: 'EvalError', exceptionCause: 'known-side-effect-rejection', remoteType: 'object'}],
  ['observation-repeat-exception-stack', {failurePredicate: 'document-observation-unavailable', targetOrdinal: 1, observationPass: 'repeat', exceptionDetails: true, exceptionClass: 'EvalError', exceptionCause: 'known-side-effect-rejection', remoteType: 'object'}],
  ['observation-old-message', {failurePredicate: 'document-observation-unavailable', targetOrdinal: 1, observationPass: 'initial', exceptionDetails: true, exceptionClass: 'EvalError', exceptionCause: 'unknown', remoteType: 'object'}],
  ['observation-same-line-stack', {failurePredicate: 'document-observation-unavailable', targetOrdinal: 1, observationPass: 'initial', exceptionDetails: true, exceptionClass: 'EvalError', exceptionCause: 'unknown', remoteType: 'object'}],
  ['observation-wrong-class', {failurePredicate: 'document-observation-unavailable', targetOrdinal: 1, observationPass: 'initial', exceptionDetails: true, exceptionClass: 'Error', exceptionCause: 'unknown', remoteType: 'object'}],
  ['observation-malformed-stack', {failurePredicate: 'document-observation-unavailable', targetOrdinal: 1, observationPass: 'initial', exceptionDetails: true, exceptionClass: 'EvalError', exceptionCause: 'unknown', remoteType: 'object'}],
  ['observation-nonstring', {failurePredicate: 'document-observation-unavailable', targetOrdinal: 1, observationPass: 'initial', exceptionDetails: false, exceptionClass: 'unknown', exceptionCause: 'unknown', remoteType: 'object'}],
  ['observation-both', {failurePredicate: 'document-observation-unavailable', targetOrdinal: 1, observationPass: 'initial', exceptionDetails: true, exceptionClass: 'TypeError', exceptionCause: 'unknown', remoteType: 'object'}],
  ['observation-malformed', {failurePredicate: 'document-observation-unavailable', targetOrdinal: 1, observationPass: 'initial', exceptionDetails: true, exceptionClass: 'unknown', exceptionCause: 'unknown', remoteType: 'unknown'}],
  ['observation-hostile', {failurePredicate: 'document-observation-unavailable', targetOrdinal: 1, observationPass: 'initial', exceptionDetails: true, exceptionClass: 'EvalError', exceptionCause: 'known-side-effect-rejection', remoteType: 'object'}],
]) test(`document observation rejection is bounded and sanitized: ${mode}`, async () => fixture(async ({inspect, state, calls, refused}) => {
  state.mode = mode;
  let firstMessage;
  await assert.rejects(inspect(), error => {
    firstMessage = String(error);
    const match = firstMessage.match(/Document observation unavailable \((\{.*\})\)$/u);
    assert.ok(match);
    assert.deepEqual(JSON.parse(match[1]), expected);
    assert.ok(firstMessage.length < 1_000);
    assert.equal(firstMessage.includes('SESSION_SECRET'), false);
    assert.equal(firstMessage.includes('CREDENTIAL_SECRET'), false);
    assert.equal(firstMessage.includes('attacker.invalid'), false);
    assert.equal(firstMessage.includes('https://fixture.test'), false);
    assert.equal(firstMessage.includes('at evaluate'), false);
    assert.equal(firstMessage.includes('EvalError: Possible side-effect'), false);
    assert.equal(firstMessage.includes(APP), false);
    return true;
  });
  const evaluation = calls.at(-1);
  assert.equal(evaluation.method, 'Runtime.evaluate');
  assert.equal(evaluation.params.throwOnSideEffect, true);
  const count = calls.length;
  await assert.rejects(inspect(), error => {
    assert.equal(String(error), firstMessage);
    return true;
  });
  assert.equal(calls.length, count);
  await refused();
}));

function readUncorrelatedDiagnostic(error) {
  const match = String(error).match(/Uncorrelated CDP reply or event \((\{.*\})\)/u);
  assert.ok(match, String(error));
  return JSON.parse(match[1]);
}

test('installed producer exposes the bounded late-event discriminator without changing quarantine', async () => fixture(async ({inspect, state, refused, calls}) => {
  state.mode = 'late-event';
  let original;
  await assert.rejects(inspect(), error => {
    original = error;
    assert.deepEqual(readUncorrelatedDiagnostic(error), {
      failurePredicate: 'uncorrelated-cdp-message', classification: 'event', method: 'DOM.childNodeCountUpdated',
      idPresence: 'absent', idType: 'absent', idValue: 'none',
      sessionRelation: 'known-local-ordinal', sessionOrdinal: 2, targetOrdinal: 2,
      lastSequence: 18, pendingId: 'none', pendingMethod: 'none', pendingSessionOrdinal: 'none',
      phase: 'post-cdp-completion', messageCount: 19, params: {nodeId: 7, childNodeCount: 2},
    });
    assert.ok(String(error).length < 1_000);
    return true;
  });
  const count = calls.length;
  await refused();
  assert.equal(calls.length, count);
  await assert.rejects(inspect(), error => error === original);
}));

test('installed producer redacts untrusted late-event params', async () => fixture(async ({inspect, state}) => {
  state.mode = 'late-event-secrets';
  await assert.rejects(inspect(), error => {
    const text = String(error);
    assert.deepEqual(readUncorrelatedDiagnostic(error).params, {nodeId: 'nonmatching', childNodeCount: 2});
    assert.equal(text.includes('SESSION_SECRET'), false);
    assert.equal(text.includes('CREDENTIAL_SECRET'), false);
    assert.equal(text.includes('attacker.invalid'), false);
    return true;
  });
}));

for (const mode of ['late-top-layer-event', 'late-top-layer-missing-params']) {
  test(`installed consumer admits the protocol-valid late top-layer notification: ${mode}`, async () => fixture(async ({inspect, state, calls}) => {
    state.mode = mode;
    const result = await inspect();
    assert.equal(state.eventSent, true);
    assert.equal(result.processAssociation.after.pid, 5301);
    assert.equal(calls.filter(call => call.id).at(-1).method, 'SystemInfo.getProcessInfo');
    assert.equal(calls.filter(call => call.id).length, 18);
  }));
}

for (const mode of ['late-top-layer-root', 'late-top-layer-unselected', 'late-top-layer-unknown', 'late-top-layer-malformed', 'late-top-layer-id', 'late-top-layer-result', 'late-top-layer-error', 'late-top-layer-extra-field', 'late-top-layer-pending', 'late-top-layer-between-passes']) {
  test(`installed consumer quarantines ${mode}`, async () => fixture(async ({inspect, state, refused}) => {
    state.mode = mode;
    let original;
    await assert.rejects(inspect(), error => {
      original = error;
      const diagnostic = readUncorrelatedDiagnostic(error);
      assert.equal(diagnostic.method, 'DOM.topLayerElementsUpdated');
      assert.equal(diagnostic.phase, mode === 'late-top-layer-pending' ? 'final-snapshot' : mode === 'late-top-layer-between-passes' ? 'initial' : 'post-cdp-completion');
      assert.equal(Object.hasOwn(diagnostic, 'params'), false);
      if (mode.endsWith('-id')) assert.equal(diagnostic.classification, 'other');
      else assert.equal(diagnostic.classification, 'event');
      return true;
    });
    await refused();
    await assert.rejects(inspect(), error => error === original);
  }));
}

test('installed consumer rejects target and document changes after an admitted notification', async () => {
  for (const [mode, method] of [['late-top-layer-target-created', 'Target.targetCreated'], ['late-top-layer-document-updated', 'DOM.documentUpdated']]) {
    await fixture(async ({inspect, state}) => {
      state.mode = mode;
      await assert.rejects(inspect(), error => {
        const diagnostic = readUncorrelatedDiagnostic(error);
        assert.equal(diagnostic.method, method);
        assert.equal(diagnostic.phase, 'post-cdp-completion');
        return true;
      });
    });
  }
});

test('installed consumer revalidates cross-protocol document identity after an admitted notification', async () => fixture(async ({inspect, state}) => {
  state.mode = 'late-top-layer-document-change';
  await assert.rejects(inspect());
  assert.equal(state.eventSent, true);
}));

test('installed consumer validates final native ownership after an admitted notification', async () => fixture(async ({inspect, state}) => {
  state.mode = 'late-top-layer-owner-after';
  await assert.rejects(inspect());
  assert.equal(state.eventSent, true);
}));

test('installed consumer enforces frame and deadline bounds after an admitted notification', async () => {
  for (const mode of ['late-top-layer-flood', 'late-top-layer-deadline']) {
    await fixture(async ({inspect, state}) => {
      state.mode = mode;
      let failure;
      await assert.rejects(inspect(), error => { failure = String(error); return true; });
      assert.equal(state.eventSent, true, `${mode}: ${failure}`);
    });
  }
});

for (const [mode, expected] of [
  ['pending-event', {classification: 'event', method: 'DOM.childNodeCountUpdated', idPresence: 'absent', idType: 'absent', idValue: 'none', sessionRelation: 'unknown', sessionOrdinal: 'unknown', targetOrdinal: 'unknown', lastSequence: 4, pendingId: 4, pendingMethod: 'DOM.getDocument', pendingSessionOrdinal: 1, phase: 'initial', messageCount: 4}],
  ['between-passes', {classification: 'event', method: 'DOM.childNodeCountUpdated', idPresence: 'absent', idType: 'absent', idValue: 'none', sessionRelation: 'known-local-ordinal', sessionOrdinal: 1, targetOrdinal: 1, lastSequence: 10, pendingId: 'none', pendingMethod: 'none', pendingSessionOrdinal: 'none', phase: 'initial', messageCount: 11}],
  ['event-error', {classification: 'event', method: 'DOM.childNodeCountUpdated', idPresence: 'absent', idType: 'absent', idValue: 'none', sessionRelation: 'root', sessionOrdinal: 'none', targetOrdinal: 'none', lastSequence: 1, pendingId: 1, pendingMethod: 'SystemInfo.getProcessInfo', pendingSessionOrdinal: 'root', phase: 'initial', messageCount: 1, params: {nodeId: 7, childNodeCount: 2}}],
  ['event-result', {classification: 'event', method: 'DOM.childNodeCountUpdated', idPresence: 'absent', idType: 'absent', idValue: 'none', sessionRelation: 'root', sessionOrdinal: 'none', targetOrdinal: 'none', lastSequence: 1, pendingId: 1, pendingMethod: 'SystemInfo.getProcessInfo', pendingSessionOrdinal: 'root', phase: 'initial', messageCount: 1, params: {nodeId: 7, childNodeCount: 2}}],
  ['frame-flood', {classification: 'event', method: 'Target.targetCreated', idPresence: 'absent', idType: 'absent', idValue: 'none', sessionRelation: 'root', sessionOrdinal: 'none', targetOrdinal: 'none', lastSequence: 1, pendingId: 1, pendingMethod: 'SystemInfo.getProcessInfo', pendingSessionOrdinal: 'root', phase: 'initial', messageCount: 1}],
  ['unknown-event', {classification: 'event', method: 'unknown', idPresence: 'absent', idType: 'absent', idValue: 'none', sessionRelation: 'root', sessionOrdinal: 'none', targetOrdinal: 'none', lastSequence: 1, pendingId: 1, pendingMethod: 'SystemInfo.getProcessInfo', pendingSessionOrdinal: 'root', phase: 'initial', messageCount: 1}],
  ['id-event', {classification: 'other', method: 'DOM.childNodeCountUpdated', idPresence: 'present', idType: 'number', idValue: 1, sessionRelation: 'root', sessionOrdinal: 'none', targetOrdinal: 'none', lastSequence: 1, pendingId: 1, pendingMethod: 'SystemInfo.getProcessInfo', pendingSessionOrdinal: 'root', phase: 'initial', messageCount: 1}],
]) test(`installed producer keeps ${mode} fatal`, async () => fixture(async ({inspect, state, refused, calls}) => {
  state.mode = mode;
  let original;
  await assert.rejects(inspect(), error => {
    original = error;
    const diagnostic = readUncorrelatedDiagnostic(error);
    assert.equal(diagnostic.failurePredicate, 'uncorrelated-cdp-message');
    for (const [field, value] of Object.entries(expected)) assert.deepEqual(diagnostic[field], value);
    return true;
  });
  const count = calls.length;
  await refused();
  assert.equal(calls.length, count);
  await assert.rejects(inspect(), error => error === original);
  assert.equal(calls.length, count);
}));

function assertExactSelectedInspectionTrace(route) {
  assert.deepEqual(route.filter(call => call.url).map(call => [call.method, call.url]), [
    ['GET', '/json/version'],
    ['GET', '/session/original-token/window/handles'],
    ['GET', '/session/original-token/window'],
    ['POST', '/session/original-token/goog/cdp/execute'],
    ['POST', '/session/original-token/execute/sync'],
    ['POST', '/session/original-token/goog/cdp/execute'],
    ['GET', '/session/original-token/window'],
    ['GET', '/json/version'],
    ['GET', '/json/version'],
    ['GET', '/json/version'],
    ['GET', '/json/version'],
    ['GET', '/session/original-token/window/handles'],
    ['GET', '/session/original-token/window'],
    ['POST', '/session/original-token/goog/cdp/execute'],
    ['POST', '/session/original-token/execute/sync'],
    ['POST', '/session/original-token/goog/cdp/execute'],
    ['GET', '/session/original-token/window'],
    ['GET', '/json/version'],
  ]);
  assert.deepEqual(route.filter(call => call.id).map(call => [call.id, call.method, call.sessionId]), [
    [1, 'SystemInfo.getProcessInfo', undefined],
    [2, 'Target.getTargets', undefined],
    [3, 'Target.attachToTarget', undefined],
    [4, 'DOM.getDocument', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'],
    [5, 'Runtime.evaluate', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'],
    [6, 'DOM.getDocument', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'],
    [7, 'Target.attachToTarget', undefined],
    [8, 'DOM.getDocument', 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'],
    [9, 'Runtime.evaluate', 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'],
    [10, 'DOM.getDocument', 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'],
    [11, 'DOM.getDocument', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'],
    [12, 'Runtime.evaluate', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'],
    [13, 'DOM.getDocument', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'],
    [14, 'DOM.getDocument', 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'],
    [15, 'Runtime.evaluate', 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'],
    [16, 'DOM.getDocument', 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'],
    [17, 'Target.getTargets', undefined],
    [18, 'SystemInfo.getProcessInfo', undefined],
  ]);
  for (const call of route.filter(call => call.url && call.url.endsWith('/execute/sync'))) {
    assert.deepEqual(call.payload.args, []);
    assert.equal(call.payload.script, `return ${SCRIPT}`);
  }
  for (const call of route.filter(call => call.url && call.url.endsWith('/goog/cdp/execute'))) {
    assert.deepEqual(call.payload, {cmd: 'DOM.getDocument', params: {depth: 0, pierce: false}});
  }
}

test('actual installed dispatcher: repeated bounded two-page reads and same-scope navigation', async () => fixture(async ({inspect, state, calls}) => {
  for (const path of ['#settings', '#pairing', '#complete']) {
    const count = calls.length;
    state.pageURL = `https://app/${path}`;
    const result = await inspect();
    const route = calls.slice(count);
    assert.equal(route.filter(call => call.adb).length, 52);
    assert.equal(route.filter(call => call.url).length, 18);
    assert.equal(route.filter(call => call.id).length, 18);
    assertExactSelectedInspectionTrace(route);
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

test('accepted lost coverage: an unselected standalone page is not part of mode proof', async () => fixture(async ({inspect, state}) => {
  const baseline = await inspect();
  assert.equal(state.mode, '');
  state.mode = 'ambiguous';
  const changed = await inspect();
  assert.equal(state.mode, 'ambiguous');
  assert.deepEqual(changed.targets, baseline.targets);
  assert.deepEqual(changed.observations, baseline.observations);
  assert.equal(changed.phase, 'installed-selected');
  assert.equal(changed.selectedHandle, APP);
  assert.ok(changed.observations.every((entry) => !Object.hasOwn(entry.document, 'standalone') && !Object.hasOwn(entry.document, 'provider')));
}));

test('unknown target kinds remain inventoried rather than becoming mode proof', async () => fixture(async ({inspect, state}) => {
  state.mode = 'unknown';
  state.extras = [{targetId: 'opaque', type: 'new_document_kind', url: 'https://app/', title: 'same', attached: false}];
  const result = await inspect();
  assert.equal(result.targets.at(-1).type, 'new_document_kind');
}));

for (const mode of ['handles', 'forward', 'serial-forward', 'namespace-absent', 'namespace-duplicate', 'namespace-nested', 'namespace-different', 'boot', 'process-string', 'process-wrong', 'process-missing', 'process-duplicate', 'process-cpu', 'process-protocol', 'process-final-loss', 'endpoint', 'wd-document', 'wd-timeOrigin', 'wd-large', 'cdp-wrong-id', 'result-malformed', 'frame-binary', 'frame-large', 'frame-partial', 'frame-malformed', 'pid', 'startTime', 'inode', 'native-browser', 'current-handle', 'outer-session', 'inner-session', 'owner-object', 'adb-object', 'adb-serial', 'caps-endpoint', 'proxy-endpoint', 'wrapper-adb', 'adb-executable', 'wrong-context', 'wrong-current-owner', 'wrong-dispatch', 'inspection-miss']) {
  test(`installed rejection is sticky: ${mode}`, async () => fixture(async ({driver, inspect, state, refused, calls}) => {
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
    let original;
    await assert.rejects(inspect(), error => { original = error; return true; });
    const count = calls.length;
    await refused();
    assert.equal(calls.length, count);
    await assert.rejects(inspect(), error => error === original);
    assert.equal(calls.length, count);
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
  assert.equal(driver.jwpProxyActive, true);
  assert.equal(driver.proxyReqRes, driver.uiautomator2.proxyReqRes);
  assert.equal(driver.proxyCommand, driver.uiautomator2.proxyCommand);
  await driver.executeCommand('setContext', 'CHROMIUM');
  assert.equal(driver.curContext, 'CHROMIUM');
  assert.equal(driver.chromedriver, owner);
  assert.equal(driver.jwpProxyActive, true);
  assert.equal(calls.length, 0);
  state.onShell = undefined;
  await inspect();
}));

test('actual native context routes source and rect through the original UiAutomator2 transport', async () => fixture(async ({driver, nativeCalls}) => {
  await driver.executeCommand('setContext', 'NATIVE_APP');
  const forward = async (originalUrl) => {
    let status;
    let response;
    const res = {setHeader() {}, status(value) { status = value; return this; }, json(value) { response = value; }};
    await driver.executeCommand('proxyReqRes', {method: 'GET', originalUrl}, res, 'outer-token');
    assert.equal(status, 200);
    return response.value;
  };
  assert.equal(await forward('/session/outer-token/source'), '<hierarchy><node/></hierarchy>');
  assert.deepEqual(await forward('/session/outer-token/element/element-2/rect'), {x: 10, y: 20, width: 100, height: 40});
  assert.deepEqual(nativeCalls, ['/session/native-token/source', '/session/native-token/element/element-2/rect']);
}));

test('replaced native routing is refused before source dispatch', async () => fixture(async ({driver, nativeCalls}) => {
  await driver.executeCommand('setContext', 'NATIVE_APP');
  driver.proxyReqRes = async () => { throw new Error('Unverified native route executed'); };
  await assert.rejects(driver.executeCommand('proxyReqRes', {method: 'GET', originalUrl: '/session/outer-token/source'}, {}, 'outer-token'), /Original native route changed/u);
  assert.deepEqual(nativeCalls, []);
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
