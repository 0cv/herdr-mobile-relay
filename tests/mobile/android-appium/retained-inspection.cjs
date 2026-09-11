'use strict';

const {AsyncLocalStorage} = require('node:async_hooks');
const http = require('node:http');
const {createAdbInspection} = require('./adb-inspection.cjs');
const {isAbsolute} = require('node:path');
const {inspectTargets, associateBrowserProcess} = require('./target-inspection.cjs');

const COMMAND = 'mobile: inspectRetainedChromeTargets';
const SCRIPT = "return JSON.stringify({href:location.href,origin:location.origin,standalone:matchMedia('(display-mode: standalone)').matches || navigator.standalone === true,provider:matchMedia('(display-mode: standalone)').matches || navigator.standalone === true ? 'android-standalone' : 'browser',timeOrigin:performance.timeOrigin})";
const installed = new WeakSet();

async function installRetainedInspection(driver, owner, requireOwner, quarantine) {
  if (installed.has(driver)) throw new Error('Inspection guard already installed');
  installed.add(driver);
  const adb = driver.adb;
  const serial = adb.curDeviceId;
  const executable = structuredClone(adb.executable);
  const executableIdentity = JSON.stringify(executable);
  let adbClient;
  const session = driver.sessionId;
  const token = owner.sessionId();
  const proxy = owner.jwproxy;
  const proxyEndpoint = JSON.stringify([proxy.server, proxy.port, proxy.scheme, proxy.base, proxy.reqBasePath]);
  const originalCommand = proxy.command;
  const nativeServer = driver.uiautomator2;
  const nativeProxy = nativeServer?.jwproxy;
  if (!nativeProxy || nativeProxy === proxy || typeof nativeProxy.sessionId !== 'string' || !nativeProxy.sessionId ||
      ['command', 'proxyCommand', 'request', 'cancelActiveRequests'].some(name => typeof nativeProxy[name] !== 'function')) throw new Error('Original native transport unavailable');
  const nativeToken = nativeProxy.sessionId;
  const nativeEndpoint = JSON.stringify([nativeProxy.server, nativeProxy.port, nativeProxy.scheme, nativeProxy.base, nativeProxy.reqBasePath]);
  const nativeCommand = nativeProxy.command;
  const nativeProxyCommand = nativeProxy.proxyCommand.bind(nativeProxy);
  const nativeRequest = nativeProxy.request.bind(nativeProxy);
  const nativeCancel = nativeProxy.cancelActiveRequests.bind(nativeProxy);
  let guardedNativeProxyCommand;
  let guardedNativeRequest;
  const stop = owner.stop.bind(owner);
  let guardedProxyCommand;
  let guardedRequest;
  let guardedExecute;
  let guardedDispatch;
  const execute = driver.execute;
  const dispatch = driver.executeCommand;
  const notifyContext = driver.notifyBiDiContextChange;
  const proxyReq = owner.proxyReq.bind(owner);
  const request = proxy.request.bind(proxy);
  const command = proxy.command.bind(proxy);
  const proxyCommand = proxy.proxyCommand.bind(proxy);
  const storage = new AsyncLocalStorage();
  let active;
  let failed;
  let original;
  let association;
  let boundHandle;
  let dispatching = false;
  const resources = new Set();
  const fail = (error) => {
    if (!failed) {
      failed = error instanceof Error ? error : new Error('Retained inspection refused');
      quarantine();
      proxy.cancelActiveRequests();
      nativeCancel();
      for (const resource of resources) resource.destroy(failed);
      adbClient?.cancel(failed);
      active?.reject(failed);
    }
    return failed;
  };
  const check = () => {
    if (failed) throw failed;
    try {
      const closing = active?.closing && storage.getStore() === active;
      const retained = closing ? driver.sessionChromedrivers.CHROMIUM : requireOwner();
      const currentToken = closing ? proxy.sessionId : owner.sessionId();
      if (retained !== owner || driver.sessionId !== session || driver.adb !== adb ||
          adb.curDeviceId !== serial || JSON.stringify(adb.executable) !== executableIdentity || owner.adb !== adb || owner.jwproxy !== proxy || currentToken !== token ||
          proxy.command !== originalCommand || (guardedProxyCommand && proxy.proxyCommand !== guardedProxyCommand) ||
          (guardedRequest && proxy.request !== guardedRequest) ||
          (guardedExecute && driver.execute !== guardedExecute) ||
          (guardedDispatch && driver.executeCommand !== guardedDispatch) ||
          driver.uiautomator2 !== nativeServer || nativeServer.jwproxy !== nativeProxy || nativeProxy.sessionId !== nativeToken ||
          nativeProxy.command !== nativeCommand || (guardedNativeProxyCommand && nativeProxy.proxyCommand !== guardedNativeProxyCommand) ||
          (guardedNativeRequest && nativeProxy.request !== guardedNativeRequest) ||
          JSON.stringify([nativeProxy.server, nativeProxy.port, nativeProxy.scheme, nativeProxy.base, nativeProxy.reqBasePath]) !== nativeEndpoint ||
          JSON.stringify([proxy.server, proxy.port, proxy.scheme, proxy.base, proxy.reqBasePath]) !== proxyEndpoint) {
        throw new Error('Original inspection owner changed');
      }
      if (active && Date.now() >= active.deadline) throw new Error('Inspection deadline');
    } catch (error) { throw fail(error); }
  };
  const run = async (deadline, operation, inspection = false) => {
    check();
    if (active) throw fail(new Error('Overlapping retained command'));
    let reject;
    const refusal = new Promise((_, no) => { reject = no; });
    refusal.catch(() => {});
    const scope = {deadline, reject, inspection, closing: false};
    active = scope;
    const timer = setTimeout(() => fail(new Error('Retained command deadline')), Math.max(1, deadline - Date.now()));
    try {
      return await storage.run(scope, async () => {
        check();
        const value = await Promise.race([operation(), refusal]);
        check();
        return value;
      });
    } catch (error) {
      const commandError = typeof error?.getActualError === 'function' ? error.getActualError() : error;
      if (!inspection && !scope.closing && ['no such element', 'stale element reference'].includes(commandError?.error)) {
        check();
        throw error;
      }
      throw fail(error);
    } finally { clearTimeout(timer); active = undefined; }
  };
  const read = async (operation) => {
    check();
    if (!active || storage.getStore() !== active) throw fail(new Error('Missing retained read scope'));
    const value = await operation(Math.max(1, active.deadline - Date.now()));
    check();
    return value;
  };
  const adbRead = (args) => read(() => adbClient.read(args, active.deadline));
  const shell = (args) => adbRead(['shell', ...args]);
  const wd = (url, method = 'GET', body) => read(async (timeout) => {
    const previous = proxy.timeout;
    proxy.timeout = timeout;
    try { return await command(url, method, body); }
    finally { proxy.timeout = previous; }
  });
  guardedProxyCommand = proxy.proxyCommand = async function (...args) {
    if (active?.closing && (args[0] !== '' || args[1] !== 'DELETE')) throw fail(new Error('Only original explicit owner disposal is allowed while closing'));
    const invoke = async () => { check(); const value = await proxyCommand(...args); check(); return value; };
    if (active && storage.getStore() === active) return await invoke();
    return await run(Date.now() + 30_000, invoke);
  };
  guardedRequest = proxy.request = async function (config) {
    check();
    if (active && storage.getStore() !== active) throw fail(new Error('Request outside retained command scope'));
    const bounded = active?.inspection && storage.getStore() === active;
    const value = await request(bounded ? {...config, maxContentLength: 65536, maxBodyLength: 65536} : config);
    check();
    return value;
  };
  guardedNativeProxyCommand = nativeProxy.proxyCommand = async function (...args) {
    check();
    if (this !== nativeProxy || (storage.getStore() && storage.getStore() !== active)) throw fail(new Error('Wrong or expired native command scope'));
    const invoke = async () => { check(); const value = await nativeProxyCommand(...args); check(); return value; };
    if (active && storage.getStore() === active) return await invoke();
    return await run(Date.now() + 30_000, invoke);
  };
  guardedNativeRequest = nativeProxy.request = async function (config) {
    check();
    if (this !== nativeProxy || !active || storage.getStore() !== active) throw fail(new Error('Native request outside retained command scope'));
    const timeout = Math.min(active.deadline - Date.now(), Number(config.timeout) || 30_000);
    if (timeout <= 0) throw fail(new Error('Native request deadline'));
    const value = await nativeRequest({...config, timeout});
    check();
    return value;
  };
  guardedDispatch = driver.executeCommand = async function (...args) {
    check();
    if (this !== driver || dispatching || active) throw fail(new Error('Wrong or overlapping retained dispatcher'));
    dispatching = true;
    try {
      if (args[0] === 'setContext') {
        const name = args[1];
        if (!['CHROMIUM', 'NATIVE_APP'].includes(name)) throw fail(new Error('Retained context must be CHROMIUM or NATIVE_APP'));
        return await run(Date.now() + 1_000, async () => {
          check();
          if (!['CHROMIUM', 'NATIVE_APP'].includes(driver.curContext) || typeof notifyContext !== 'function') throw new Error('Retained context state unavailable');
          if (driver.curContext === name) return;
          driver._bidiProxyUrl = null;
          driver.chromedriver = name === 'CHROMIUM' ? owner : undefined;
          driver.proxyReqRes = name === 'CHROMIUM' ? proxyReq : undefined;
          driver.proxyCommand = name === 'CHROMIUM' ? command : undefined;
          driver.jwpProxyActive = name === 'CHROMIUM';
          driver.curContext = name;
          await notifyContext.call(driver);
          check();
        });
      }
      const invoke = async () => { const result = await dispatch.apply(this, args); check(); return result; };
      if ((args[0] === 'execute' && args[1] === COMMAND) || args[0] === 'deleteSession') return await invoke();
      return await run(Date.now() + 30_000, invoke);
    } catch (error) { check(); throw error; }
    finally { dispatching = false; }
  };
  owner.stop = async function (...args) {
    if (this !== owner) throw fail(new Error('Wrong disposal owner'));
    const result = await run(Date.now() + 30_000, async () => {
      active.closing = true;
      return await stop(...args);
    });
    fail(new Error('Original Chrome owner explicitly disposed'));
    return result;
  };
  const processIdentity = async () => {
    const startedAt = Date.now();
    const pid = (await shell(['pidof', 'com.android.chrome'])).trim();
    if (!/^[1-9]\d*$/.test(pid) || Number(pid) > 2147483647) throw new Error('Sole Chrome PID unavailable');
    if (original && original.pid !== pid) throw new Error('Original native Chrome process changed');
    const stat = (await shell(['cat', `/proc/${pid}/stat`])).trim().match(/^(\d+) \(.+\) ([A-Za-z]) (.+)$/);
    const startTime = stat?.[3].split(/\s+/)[18];
    if (stat?.[1] !== pid || !/^[1-9]\d*$/.test(startTime || '') || /[ZXx]/.test(stat[2])) throw new Error('Live Chrome starttime unavailable');
    if (original && (original.pid !== pid || original.startTime !== startTime)) throw new Error('Original native Chrome process changed');
    // Linux lists NSpid from the procfs mount's namespace to the task's active namespace.
    // Both reads require the same trusted, stable procfs view; this is not a namespace identifier.
    const namespaceStatus = (text, expectedPid) => {
      if (text.length > 65536) throw new Error('Native namespace metadata bound');
      const field = (name) => {
        const values = text.split(/\r?\n/).filter(line => line.startsWith(`${name}:`));
        if (values.length !== 1) throw new Error('Native namespace metadata unavailable');
        return values[0].slice(name.length + 1).trim();
      };
      const visiblePid = field('Pid');
      const namespacePid = field('NSpid');
      if (!/^[1-9]\d*$/.test(visiblePid) || Number(visiblePid) > 2147483647 ||
          namespacePid !== visiblePid || (expectedPid && visiblePid !== expectedPid)) throw new Error('Native PID namespace is nested, differing or unknown');
    };
    namespaceStatus(await shell(['cat', `/proc/${pid}/status`]), pid);
    namespaceStatus(await shell(['cat', '/proc/self/status']));
    const bootId = (await shell(['cat', '/proc/sys/kernel/random/boot_id'])).trim();
    if (!/^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/.test(bootId)) throw new Error('Native boot identity unavailable');
    const namespace = 'reader-and-browser-active-in-procfs-mount-pid-namespace';
    if (original && (original.pid !== pid || original.startTime !== startTime || original.bootId !== bootId || original.namespace !== namespace)) throw new Error('Original native Chrome process changed');
    return {pid, startTime, bootId, namespace, startedAt, finishedAt: Date.now()};
  };
  const native = async () => {
    const process = await processIdentity();
    const activities = await shell(['dumpsys', 'activity', 'activities']);
    const inventory = activities.split(/\r?\n/).map(line => line.match(/^\s*(mResumedActivity|ResumedActivity|topResumedActivity|mTopResumedActivity|mFocusedApp|Resumed)\s*[:=]\s*(.*)$/)).filter(Boolean);
    if (!inventory.some(entry => entry[1] === 'Resumed' || /ResumedActivity$/.test(entry[1]))) throw new Error('Native resumed activity unavailable');
    const records = inventory.map(entry => {
      const record = entry[2].match(/^ActivityRecord\{([0-9a-f]+)\s+(u\d+)\s+([A-Za-z0-9_.]+)\/([A-Za-z0-9_.$]+)(?:\s+[^{}]*)?\}$/);
      if (!record || record[3] !== 'com.android.chrome') throw new Error('Unqualified or competing native foreground');
      return record;
    });
    if (new Set(records.map(record => record[0])).size !== 1) throw new Error('Chrome native foreground unavailable or ambiguous');
    const activity = records[0][4];
    const processLines = activities.split(/\r?\n/).filter((line) => line.includes(':com.android.chrome/'));
    const pids = new Set(processLines.map((line) => line.match(/ProcessRecord\{[^}\n]*\s(\d+):/)?.[1]).filter(Boolean));
    const resumedPid = records[0][0].match(/\bpid=(\d+)/)?.[1];
    if (resumedPid ? resumedPid !== process.pid : pids.size !== 1 || !pids.has(process.pid)) throw new Error('Foreground process association unavailable');
    const standalone = /(?:^|[.$])(?:Webapp|WebApk)[A-Za-z0-9_.-]*Activity$/.test(activity) && !/(?:^|[.$])(?:Webapp|WebApk)LauncherActivity$/.test(activity);
    if (!standalone && !/^(?:com\.google\.android\.apps\.chrome\.Main|org\.chromium\.chrome\.browser\.ChromeTabbedActivity)$/.test(activity)) throw new Error('Unqualified native provider');
    if (boundHandle && !standalone) throw new Error('Bound native installed provider lost');
    return {...process, activity, provider: standalone ? 'android-standalone' : 'browser', finishedAt: Date.now()};
  };
  const forward = async () => {
    const caps = driver._chromedriverCapsCache.get('CHROMIUM');
    const address = caps?.['goog:chromeOptions']?.debuggerAddress;
    const match = typeof address === 'string' ? address.match(/^(?:localhost|127\.0\.0\.1):([1-9]\d*)$/) : null;
    if (!match) throw new Error('Original debugger address unavailable');
    const port = Number(match[1]);
    if (!Number.isInteger(port) || port > 65535 || caps?.browserVersion !== '131.0.6778.200' || caps?.chrome?.chromedriverVersion !== '131.0.6778.264 (2d05e31515360f4da764174f7c448b33e36da871-refs/branch-heads/6778@{#4323})') throw new Error('Pinned original debugger endpoint unavailable');
    const lines = (await adbRead(['forward', '--list'])).trim().split(/\r?\n/).map((line) => line.trim().split(/\s+/));
    const forwards = lines.filter((line) => line[1] === `tcp:${port}`);
    if (forwards.length !== 1 || forwards[0].length !== 3 || forwards[0][0] !== serial || forwards[0][2] !== 'localabstract:chrome_devtools_remote') throw new Error('Live owned Chrome forward changed');
    const sockets = (await shell(['cat', '/proc/net/unix'])).split(/\r?\n/).map((line) => line.trim().split(/\s+/)).filter((line) => line[7] === '@chrome_devtools_remote');
    if (sockets.length !== 1 || !/^[1-9]\d*$/.test(sockets[0][6])) throw new Error('Live Chrome socket unavailable');
    const inode = sockets[0][6];
    const value = {serial, port, socket: 'chrome_devtools_remote', inode, browserVersion: `Chrome/${caps.browserVersion}`};
    if (association && JSON.stringify(value) !== JSON.stringify(association)) throw new Error('Original browser/forward association changed');
    return value;
  };
  const version = (port) => read((timeout) => new Promise((resolve, reject) => {
    const req = http.get({hostname: '127.0.0.1', port, path: '/json/version', agent: false, maxHeaderSize: 8192, timeout}, (res) => {
      let size = 0;
      const chunks = [];
      res.on('error', reject);
      res.on('aborted', () => reject(new Error('Partial browser association')));
      if (res.statusCode !== 200) { res.destroy(); reject(new Error('Browser association status')); return; }
      res.on('data', (chunk) => { size += chunk.length; if (size > 65536) { res.destroy(); reject(new Error('Browser association size')); return; } chunks.push(chunk); });
      res.on('end', () => { req.destroy(); try { resolve(JSON.parse(Buffer.concat(chunks).toString('utf8'))); } catch (error) { reject(error); } });
    });
    resources.add(req);
    const timer = setTimeout(() => req.destroy(new Error('Browser association deadline')), timeout);
    req.on('timeout', () => req.destroy(new Error('Browser association timeout')));
    req.on('error', reject);
    req.on('close', () => { clearTimeout(timer); resources.delete(req); });
  }));
  let browserPath;
  const browser = async (currentForward) => {
    const discovered = await version(currentForward.port);
    const prefix = `ws://127.0.0.1:${currentForward.port}`;
    const path = typeof discovered.webSocketDebuggerUrl === 'string' && discovered.webSocketDebuggerUrl.slice(prefix.length);
    if (discovered.Browser !== currentForward.browserVersion || !discovered.webSocketDebuggerUrl.startsWith(prefix) || !/^\/devtools\/browser(?:\/[A-Za-z0-9-]+)?$/.test(path) || (browserPath && path !== browserPath)) throw new Error('Live original browser endpoint changed');
    if (!browserPath) browserPath = path;
  };
  const snapshot = async () => {
    const startedAt = Date.now();
    const beforeNative = await native();
    const currentForward = await forward();
    await browser(currentForward);
    const handles = await wd('/window/handles');
    const selectedHandle = await wd('/window');
    if (!Array.isArray(handles) || !handles.length || handles.length > 32 || handles.some((handle) => typeof handle !== 'string' || !/^[A-Fa-f0-9]{32}$/.test(handle)) || new Set(handles).size !== handles.length || !handles.includes(selectedHandle) || (boundHandle && selectedHandle !== boundHandle)) throw new Error('Current retained window changed');
    const first = await wd('/goog/cdp/execute', 'POST', {cmd: 'DOM.getDocument', params: {depth: 0, pierce: false}});
    const document = JSON.parse(await wd('/execute/sync', 'POST', {script: SCRIPT, args: []}));
    const last = await wd('/goog/cdp/execute', 'POST', {cmd: 'DOM.getDocument', params: {depth: 0, pierce: false}});
    if (first.root?.nodeType !== 9 || last.root?.nodeType !== 9 || !Number.isSafeInteger(first.root.backendNodeId) || first.root.backendNodeId <= 0 || first.root.backendNodeId !== last.root.backendNodeId || first.root.documentURL !== document.href || last.root.documentURL !== document.href || typeof document.standalone !== 'boolean' || document.provider !== (document.standalone ? 'android-standalone' : 'browser') || typeof document.origin !== 'string' || !Number.isFinite(document.timeOrigin)) throw new Error('Current WebDriver document changed');
    if (await wd('/window') !== selectedHandle) throw new Error('Selected handle changed during document read');
    const afterNative = await native();
    if (beforeNative.activity !== afterNative.activity || beforeNative.provider !== afterNative.provider) throw new Error('Native provider changed during read');
    if (boundHandle && !document.standalone) throw new Error('Bound document provider lost');
    return {endpoint: Object.freeze({host: '127.0.0.1', port: currentForward.port, browserPath}), browserVersion: currentForward.browserVersion, handles, selectedHandle, document: {...document, backendNodeId: first.root.backendNodeId}, native: afterNative, nativeBefore: beforeNative, forward: currentForward, startedAt, finishedAt: Date.now()};
  };
  guardedExecute = driver.execute = async function (script, args) {
    check();
    if (script !== COMMAND) {
      if (typeof script === 'string' && script.includes('inspectRetainedChromeTargets')) throw fail(new Error('Noncanonical inspection command'));
      return await execute.call(this, script, args);
    }
    if (this !== driver) throw fail(new Error('Wrong inspection driver object'));
    const input = Array.isArray(args) && args.length === 1 ? args[0] : undefined;
    if (!input || Object.getPrototypeOf(input) !== Object.prototype || Object.keys(input).length !== 1 || !Object.hasOwn(input, 'deadline') || !Number.isSafeInteger(input.deadline) || input.deadline - Date.now() < 7500 || input.deadline - Date.now() > 30_000) throw fail(new Error('Inspection requires only a fully admitted deadline (7500..30000ms)'));
    return await run(input.deadline, async () => {
      if (driver.curContext !== 'CHROMIUM' || driver.chromedriver !== owner) throw new Error('Inspection requires original current CHROMIUM owner');
      const snapshots = [];
      const result = await inspectTargets(owner, {
        deadline: input.deadline,
        expectedBrowserPid: Number(original.pid),
        assertOwner: (candidate) => { check(); if (candidate !== owner) throw fail(new Error('Wrong inspection owner')); },
        failure: fail,
        snapshot: async () => { const value = await snapshot(); snapshots.push(value); return value; },
        validateSnapshot: async (_, first, last) => {
          check();
          const currentNative = await native();
          if (currentNative.activity !== last.native.activity || currentNative.provider !== last.native.provider) throw new Error('Native provider changed at validation bound');
          await browser(await forward());
          if (first.selectedHandle !== last.selectedHandle || JSON.stringify([...first.handles].sort()) !== JSON.stringify([...last.handles].sort()) || JSON.stringify(first.document) !== JSON.stringify(last.document) || first.native.activity !== last.native.activity || first.native.provider !== last.native.provider) throw new Error('Observation association changed');
          check();
        },
        validateSelectedDocument: async (_, result, first, last) => {
          check();
          const selected = result.observations.find((entry) => entry.targetId === result.selectedHandle);
          if (!selected || JSON.stringify(selected.document) !== JSON.stringify(first.document) || JSON.stringify(selected.document) !== JSON.stringify(last.document)) throw new Error('Cross-protocol selected document mismatch');
          check();
        },
      });
      const [before, after] = snapshots;
      if (snapshots.length !== 2) throw new Error('Missing bounded owner observations');
      if (after.document.standalone) {
        if (after.native.provider !== 'android-standalone') throw new Error('Standalone DOM is not installed native ownership');
        boundHandle = after.selectedHandle;
      }
      return {...result, phase: boundHandle ? 'installed-selected' : 'initial-browser-selected', original: {...original, serial, sessionId: session, chromeSessionId: token}, before, after};
    }, true);
  };
  await run(Date.now() + 10_000, async () => {
    if (typeof serial !== 'string' || !serial || typeof session !== 'string' || !session || !token || typeof execute !== 'function' || typeof dispatch !== 'function') throw new Error('Original driver admission unavailable');
    if (!executable || !isAbsolute(executable.path) || !Array.isArray(executable.defaultArgs)) throw new Error('Original absolute ADB executable unavailable');
    const args = executable.defaultArgs;
    if (args.length !== 4 || args[0] !== '-P' || !/^[1-9]\d*$/.test(args[1]) || Number(args[1]) > 65535 || args[2] !== '-s' || args[3] !== serial) throw new Error('Only the original local ADB server and serial are admitted');
    adbClient = createAdbInspection(adb, check, fail);
    original = await processIdentity();
    const captureSnapshot = async () => {
      const startedAt = Date.now();
      const beforeNative = await native();
      const currentForward = await forward();
      if (!association) association = currentForward;
      await browser(currentForward);
      const afterNative = await native();
      if (beforeNative.activity !== afterNative.activity || beforeNative.provider !== afterNative.provider) throw new Error('Native provider changed during startup');
      return {endpoint: Object.freeze({host: '127.0.0.1', port: currentForward.port, browserPath}), browserVersion: currentForward.browserVersion, native: afterNative, startedAt, finishedAt: Date.now()};
    };
    await associateBrowserProcess(owner, {
      deadline: active.deadline,
      expectedBrowserPid: Number(original.pid),
      assertOwner: candidate => { check(); if (candidate !== owner) throw fail(new Error('Wrong association owner')); },
      failure: fail,
      snapshot: captureSnapshot,
      validateSnapshot: async (_, first, last) => {
        const current = await captureSnapshot();
        if (first.native.activity !== last.native.activity || first.native.provider !== last.native.provider || current.native.activity !== last.native.activity || current.native.provider !== last.native.provider) throw new Error('Startup native provider changed');
        check();
      },
    });
  }, true);
}

module.exports = {installRetainedInspection};
