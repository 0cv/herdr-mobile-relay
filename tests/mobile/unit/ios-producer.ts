import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { readFile, realpath, writeFile, rename, symlink, unlink, mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { AppiumClient } from '../support/webdriver';
import { IOSPlatform } from '../platforms/ios';
import { PhaseBudget } from '../support/budget';
import { IOSWebKitOutputDiagnostics } from '../support/ios-webkit-output';

const contract = 'herdr-installed-contexts-v1';
const target = Object.freeze({ bundleId: 'com.apple.webapp' as const, nativePid: 97962 });
const G = 'PID:96010', S = 'PID:95890', W = 'PID:97230', N = 'PID:97962';
const runtime = process.env.IOS_PRODUCER_HOME;
const original = process.env.IOS_PRODUCER_ORIGINAL_HOME;
const value = (data: unknown) => Response.json({ value: data });
const page = (host?: unknown, url = 'about:blank') => ({ '2': {
  WIRPageIdentifierKey: 2, WIRTitleKey: '', WIRURLKey: url,
  ...(host === undefined ? {} : { WIRHostApplicationIdentifierKey: host }),
} });
type Test = [string, () => Promise<void>];

async function modules(home: string) {
  const root = join(await realpath(home), 'node_modules/appium-xcuitest-driver');
  const remote = join(root, 'node_modules/appium-remote-debugger/build/lib');
  return {
    RemoteDebugger: (await import(pathToFileURL(join(remote, 'remote-debugger.js')).href)).RemoteDebugger,
    inspector: await import(pathToFileURL(join(remote, 'utils/inspector.js')).href),
    context: await import(pathToFileURL(join(root, 'build/lib/commands/context.js')).href),
    executeMap: (await import(pathToFileURL(join(root, 'build/lib/execute-method-map.js')).href)).executeMethodMap,
  };
}

async function fixture(home: string, options: { generic?: string; noProvider?: boolean; silent?: string; settleGeneric?: boolean } = {}) {
  const mod = await modules(home);
  const logs: string[] = [], calls: string[] = [];
  const beforeRPC: string[][] = [];
  const log = Object.fromEntries(['info', 'debug', 'warn', 'error', 'trace'].map(level => [level, (text: unknown) => logs.push(String(text))]));
  const remote = new mod.RemoteDebugger({ bundleId: 'com.apple.mobilesafari', includeSafari: true, additionalBundleIds: ['com.apple.webapp'], log });
  const rpc = new EventEmitter() as EventEmitter & { selectApp: (id: string) => Promise<unknown> };
  let reply: (id: string) => Promise<unknown> = async id => {
    if (id === options.silent || (id === G && !options.settleGeneric)) return new Promise(() => {});
    if (id === G && options.settleGeneric) throw new Error('synthetic not connectable');
    return [id, page(N)];
  };
  rpc.selectApp = async id => { beforeRPC.push(logs.filter(line => line.startsWith('Installed candidate'))); calls.push(id); return reply(id); };
  remote._rpcClient = rpc;
  rpc.on('update', dict => { void remote.onAppUpdate(null, dict); });
  rpc.on('disconnect-app', dict => remote.onAppDisconnect(null, dict));
  rpc.on('pages', (id, pages) => { void remote.onPageChange(null, id, pages); });
  const raw = (id: string, bundleId: string, extra: Record<string, unknown> = {}) => ({
    WIRApplicationIdentifierKey: id, WIRApplicationBundleIdentifierKey: bundleId,
    WIRApplicationNameKey: 'private-label-sentinel', WIRIsApplicationActiveKey: 1, WIRRemoteAutomationEnabledKey: true, ...extra,
  });
  const add = (id: string, bundleId: string, extra: Record<string, unknown> = {}) => {
    const [key, info] = mod.inspector.appInfoFromDict(raw(id, bundleId, extra)); remote._appDict[key] = info;
  };
  add(G, options.generic ?? 'com.apple.WebKit.WebContent');
  add(S, 'com.apple.mobilesafari');
  remote._appDict[S].pageArray = mod.inspector.pageArrayFromDict(page(undefined, 'https://safari.invalid/private-sentinel'));
  if (!options.noProvider) add(W, 'com.apple.SafariViewService');
  return { ...mod, remote, rpc, calls, logs, beforeRPC, raw, add, reply: (fn: typeof reply) => { reply = fn; } };
}

async function pending(promise: Promise<unknown>): Promise<boolean> {
  // Microtask turn, not a producer timeout or cancellation; deliberately silent RPCs stay live.
  let settled = false;
  void promise.then(() => { settled = true; }, () => { settled = true; });
  await new Promise(resolve => setTimeout(resolve, 10));
  return !settled;
}

export const iosProducerOriginalTests: Test[] = [
  ['iOS producer installed pin regression: original hangs on G before W and separately leaks cached Safari', async () => {
    if (!original) throw new Error('IOS_PRODUCER_ORIGINAL_HOME required for actual original producer regression');
    const old = await fixture(original);
    assert.equal(await pending(old.remote.selectApp(null, 1, false)), true);
    assert.deepEqual(old.calls, [G], 'actual original source dispatches the silent generic RPC, never W');
    const completed = await fixture(original, { settleGeneric: true });
    const pages = await completed.remote.selectApp(null, 1, false);
    assert.deepEqual(completed.calls, [G, W]);
    assert.equal(completed.remote._appIdKey, S, 'original searchForPage selects the primed Safari cache');
    assert.ok(pages.some((p: any) => p.id.startsWith('95890.')));
  }],
];

export const iosProducerTests: Test[] = [
  ['iOS producer installed scope skips G/S before RPC and cannot leak cached Safari', async () => {
    assert.ok(runtime, 'IOS_PRODUCER_HOME required');
    for (const settleGeneric of [false, true]) {
      const f = await fixture(runtime, { settleGeneric });
      const before = { ...f.remote._appDict };
      const request = f.remote.selectApp(null, 1, false, target);
      assert.equal(await pending(request), false, 'scoped discovery must not dispatch a pending unowned generic RPC');
      const pages = await request;
      assert.deepEqual(f.calls, [W]);
      assert.deepEqual(pages.map((p: any) => p.id), ['97230.2']);
      assert.equal(f.remote._appIdKey, W);
      assert.equal(pages[0].url, 'about:blank', 'valid blank publication is not URL-prefiltered');
      assert.equal(pages[0].hostId, N);
      assert.deepEqual(Object.keys(f.remote._appDict), Object.keys(before), 'no pruning/global opts mutation');
      const evidence = f.logs.filter(line => line.startsWith('Installed candidate'));
      assert.ok(evidence.includes('Installed candidate app=96010 host=0 native=97962 role=generic reason=unowned action=skip'));
      assert.ok(evidence.includes('Installed candidate app=97230 host=0 native=97962 role=service reason=provider action=list'));
      assert.equal(f.beforeRPC[0].at(-1), 'Installed candidate app=97230 host=0 native=97962 role=service reason=provider action=list', 'ownership evidence precedes the listing RPC');
    }
  }],
  ['iOS producer installed scope after settling generic failure still probes only W and excludes Safari cache', async () => {
    assert.ok(runtime);
    const f = await fixture(runtime, { settleGeneric: true });
    const pages = await f.remote.selectApp(null, 1, false, target);
    assert.deepEqual(f.calls, [W], 'the original dispatches G then W even when G settles');
    assert.equal(f.remote._appIdKey, W, 'a completed allowed-provider listing must not select cached Safari');
    assert.deepEqual(pages.map((p: any) => p.id), ['97230.2']);
  }],
  ['iOS producer installed scope matrix: absence, services, proxies, wrong hosts, malformed and page ownership', async () => {
    assert.ok(runtime);
    const scenarios: Array<{ name: string; mutate: (f: Awaited<ReturnType<typeof fixture>>) => void; expected: string[] }> = [
      { name: 'no provider', mutate: f => { delete f.remote._appDict[W]; }, expected: [] },
      { name: 'latest connected list withdraws scoped eligibility without pruning', mutate: f => { void f.remote.onConnectedApplicationList(null, {}); }, expected: [] },
      { name: 'unknown bundle metadata is not OTHER or a PID blacklist', mutate: f => { f.remote._appDict[W].bundleId = undefined; }, expected: [] },
      { name: 'arbitrary bundle', mutate: f => { f.add(W, 'private.unsupported.bundle'); }, expected: [] },
      { name: 'wildcard is not a provider', mutate: f => { f.add(W, '*'); }, expected: [] },
      { name: 'known service without application host and different native PID', mutate: () => {}, expected: [W] },
      { name: 'webapp role', mutate: f => { f.add(W, 'com.apple.webapp'); }, expected: [W] },
      { name: 'valid proxy through service PID distinct from native PID', mutate: f => { f.add(G, 'com.apple.WebKit.WebContent', { WIRIsApplicationProxyKey: true, WIRHostApplicationIdentifierKey: W }); }, expected: [G] },
      { name: 'positive native provider proxy', mutate: f => { delete f.remote._appDict[W]; f.add(N, 'com.apple.webapp'); f.add(G, 'process-com.apple.WebKit.WebContent', { WIRIsApplicationProxyKey: true, WIRHostApplicationIdentifierKey: N }); }, expected: [G] },
      { name: 'missing proxy flag', mutate: f => { delete f.remote._appDict[W]; f.add(G, 'com.apple.WebKit.WebContent', { WIRHostApplicationIdentifierKey: N }); }, expected: [] },
      { name: 'wrong known host', mutate: f => { f.add(W, 'com.apple.SafariViewService', { WIRHostApplicationIdentifierKey: S }); }, expected: [] },
      { name: 'missing is not malformed', mutate: f => { f.add(W, 'com.apple.SafariViewService'); }, expected: [W] },
      { name: 'malformed host', mutate: f => { f.add(W, 'com.apple.SafariViewService', { WIRHostApplicationIdentifierKey: 'PID:97962.secret' }); }, expected: [] },
      { name: 'null host is malformed', mutate: f => { f.add(W, 'com.apple.SafariViewService', { WIRHostApplicationIdentifierKey: null }); }, expected: [] },
      { name: 'inactive provider', mutate: f => { f.add(W, 'com.apple.SafariViewService', { WIRIsApplicationActiveKey: 0 }); }, expected: [] },
    ];
    for (const scenario of scenarios) {
      const f = await fixture(runtime, { settleGeneric: true }); scenario.mutate(f);
      f.reply(async id => [id, page(N)]);
      const pages = await f.remote.selectApp(null, 1, false, target);
      assert.deepEqual(f.calls, scenario.expected, scenario.name);
      assert.ok(pages.every((p: any) => !p.id.startsWith('95890.')), scenario.name);
    }
    for (const host of [S, null, 'PID:97962.private', 'PID:99999']) {
      const f = await fixture(runtime);
      f.reply(async id => [id, page(host)]);
      await assert.rejects(() => f.remote.selectApp(null, 1, false, target));
      assert.deepEqual(f.calls, [W]);
    }
    const redirect = await fixture(runtime);
    redirect.add('PID:97231', 'com.apple.SafariViewService');
    redirect.reply(async () => ['PID:97231', page(N)]);
    assert.deepEqual((await redirect.remote.selectApp(null, 1, false, target)).map((p: any) => p.id), ['97231.2']);
    assert.deepEqual(redirect.calls, [W], 'eligible redirected app IDs are revalidated, not equated to the native PID');
  }],
  ['iOS producer L1 rejects contradictory native provider hosts before proxy RPC and page publication', async () => {
    assert.ok(runtime);
    for (const mode of ['healthy', 'missing', 'wrong-host', 'malformed', 'inactive', 'wrong-role'] as const) {
      const extra = mode === 'wrong-host' ? { WIRHostApplicationIdentifierKey: S } : mode === 'malformed' ? { WIRHostApplicationIdentifierKey: null } : mode === 'inactive' ? { WIRIsApplicationActiveKey: 0 } : {};
      for (const route of ['proxy', 'page'] as const) {
        const f = await fixture(runtime, { settleGeneric: true });
        if (mode !== 'missing') f.add(N, mode === 'wrong-role' ? 'com.apple.mobilesafari' : 'com.apple.webapp', extra);
        if (route === 'proxy') {
          delete f.remote._appDict[W];
          f.add(G, 'com.apple.WebKit.WebContent', { WIRIsApplicationProxyKey: true, WIRHostApplicationIdentifierKey: N });
        }
        f.reply(async id => [id, page(N)]);
        const allowed = mode === 'healthy' || (mode === 'missing' && route === 'page');
        if (allowed) assert.ok((await f.remote.selectApp(null, 1, false, target)).length > 0);
        else if (route === 'page') await assert.rejects(() => f.remote.selectApp(null, 1, false, target));
        else assert.deepEqual(await f.remote.selectApp(null, 1, false, target), []);
        if (route === 'proxy') assert.deepEqual(f.calls, allowed ? [G] : [], mode);
        else assert.ok(f.calls.includes(W), mode);
      }
    }
  }],
  ['iOS producer L2 invalidates every dependent cached page across host identity events without title starvation', async () => {
    assert.ok(runtime);
    const H = 'PID:97232', T = 'PID:97231';
    for (const change of ['native-reuse', 'connected-withdrawal', 'second-page-reuse', 'provider-native-change', 'unrelated-title'] as const) {
      const f = await fixture(runtime);
      f.add(N, 'com.apple.webapp');
      f.add(H, 'com.apple.SafariViewService', { WIRHostApplicationIdentifierKey: N });
      const publication = { ...page(N), '3': { ...page(H)['2'], WIRPageIdentifierKey: 3 } };
      f.reply(async id => [id, publication]);
      assert.equal((await f.remote.selectApp(null, 1, false, target)).length, 2);
      if (change === 'native-reuse' || change === 'second-page-reuse') {
        const id = change === 'native-reuse' ? N : H;
        f.rpc.emit('disconnect-app', { WIRApplicationIdentifierKey: id });
        f.add(id, id === N ? 'com.apple.webapp' : 'com.apple.SafariViewService');
      }
      if (change === 'connected-withdrawal') {
        await f.remote.onConnectedApplicationList(null, Object.fromEntries([G, S, W, H].map(id => [id, f.raw(id, f.remote._appDict[id].bundleId, id === H ? { WIRHostApplicationIdentifierKey: N } : {})])));
        f.add(N, 'com.apple.webapp');
      }
      if (change === 'provider-native-change') f.rpc.emit('update', f.raw(H, 'com.apple.SafariViewService'));
      if (change === 'unrelated-title') f.rpc.emit('update', f.raw(N, 'com.apple.webapp', { WIRApplicationNameKey: 'unrelated-private-title' }));
      f.add(T, 'com.apple.SafariViewService');
      const other = f.remote._appDict[T]; delete f.remote._appDict[T];
      f.remote._appDict = { [T]: other, ...f.remote._appDict };
      f.calls.length = 0;
      f.reply(async id => [id, page(undefined)]);
      const interleaved = await f.remote.selectApp(null, 1, false, target);
      assert.deepEqual(f.calls, [T]);
      assert.equal(interleaved.filter((p: any) => p.id.startsWith('97230.')).length, change === 'unrelated-title' ? 2 : 0, change);
      const owner = f.remote._appDict[W]; delete f.remote._appDict[W];
      f.remote._appDict = { [W]: owner, ...f.remote._appDict };
      f.reply(async id => [id, publication]);
      const fresh = await f.remote.selectApp(null, 1, false, target);
      assert.equal(fresh.filter((p: any) => p.id.startsWith('97230.')).length, 2, `${change}: fresh listing is accepted`);
      assert.equal(owner.pageArray.length, 2, 'no global cache pruning');
    }
    const proxy = await fixture(runtime);
    proxy.add(N, 'com.apple.webapp');
    proxy.add(G, 'com.apple.WebKit.WebContent', { WIRIsApplicationProxyKey: true, WIRHostApplicationIdentifierKey: W });
    proxy.add(W, 'com.apple.SafariViewService', { WIRHostApplicationIdentifierKey: N });
    proxy.reply(async id => [id, page(N)]);
    await proxy.remote.selectApp(null, 1, false, target);
    proxy.rpc.emit('update', proxy.raw(N, 'com.apple.webapp', { WIRHostApplicationIdentifierKey: S }));
    proxy.calls.length = 0;
    assert.deepEqual(await proxy.remote.selectApp(null, 1, false, target), []);
    assert.deepEqual(proxy.calls, [], 'referenced provider native-host contradiction withdraws proxy eligibility');
  }],
  ['iOS producer scoped discovery populates actual setContext without hidden unscoped refresh', async () => {
    assert.ok(runtime);
    const f = await fixture(runtime);
    const driver = { ...f.context, opts: { webviewConnectRetries: 1 }, log: f.remote.log, _remote: f.remote, remote: f.remote,
      eventEmitter: new EventEmitter(), isRealDevice: () => false, getCurrentUrl: () => undefined,
      getContexts: async () => { throw new Error('Unexpected unscoped discovery'); } } as any;
    const before = { ...driver.opts };
    const pages = await f.context.mobileGetInstalledContexts.call(driver, target);
    assert.deepEqual(driver.contexts, ['NATIVE_APP', '97230.2']);
    const selections: unknown[] = [];
    f.remote.selectPage = async (...args: unknown[]) => { selections.push(args); };
    await f.context.setContext.call(driver, pages[1].id);
    assert.deepEqual(selections, [[97230, 2, false]]);
    assert.deepEqual(f.calls, [W]);
    assert.deepEqual(driver.opts, before);
    assert.equal(driver.curContext, '97230.2');
  }],
  ['iOS producer installed scope rechecks redirects, disconnect/reuse, identity changes and unrelated updates', async () => {
    assert.ok(runtime);
    for (const change of ['redirect-safari', 'disconnect', 'reuse', 'owner-change', 'unrelated'] as const) {
      const f = await fixture(runtime);
      f.reply(async id => {
        if (change === 'redirect-safari') return [S, page(N)];
        if (change === 'disconnect' || change === 'reuse') f.rpc.emit('disconnect-app', { WIRApplicationIdentifierKey: id });
        if (change === 'reuse') f.add(id, 'com.apple.SafariViewService');
        if (change === 'owner-change') f.rpc.emit('update', f.raw(id, 'com.apple.SafariViewService', { WIRHostApplicationIdentifierKey: S }));
        if (change === 'unrelated') f.rpc.emit('update', f.raw(id, 'com.apple.SafariViewService', { WIRApplicationNameKey: 'changed-private-label' }));
        return [id, page(N)];
      });
      if (change === 'unrelated') assert.equal((await f.remote.selectApp(null, 1, false, target)).length, 1);
      else await assert.rejects(() => f.remote.selectApp(null, 1, false, target), () => true, change);
      assert.deepEqual(f.calls, [W]);
    }
    const f = await fixture(runtime);
    f.reply(async id => [id, page(N)]);
    await f.remote.selectApp(null, 1, false, target);
    // Preserve stock pageArray on updates, but never trust pages published under an old identity.
    f.rpc.emit('update', f.raw(W, 'com.apple.webapp'));
    f.add('PID:97231', 'com.apple.SafariViewService');
    // Probe another eligible app before the old owner's still-cached pageArray.
    const oldOwner = f.remote._appDict[W]; delete f.remote._appDict[W]; f.remote._appDict[W] = oldOwner;
    f.reply(async id => [id, page(N)]);
    const next = await f.remote.selectApp(null, 1, false, target);
    assert.deepEqual(next.map((p: any) => p.id), ['97231.2']);
    assert.equal(f.remote._appIdKey, 'PID:97231');
    assert.equal(oldOwner.pageArray.length, 1, 'stock cache retained, scoped identity invalidated');
    // Connected-application-list updates are also identity evidence, not just individual update events.
    await f.remote.onConnectedApplicationList(null, { [W]: f.raw(W, 'com.apple.SafariViewService', { WIRHostApplicationIdentifierKey: S }) });
    assert.equal(f.remote._appDict[W].hostId, S);
  }],
  ['iOS producer installed scope rejects unsupported contracts/targets before any RPC; stock Safari stays unscoped', async () => {
    assert.ok(runtime);
    const f = await fixture(runtime);
    const invalid: unknown[] = [null, {}, { bundleId: '*', nativePid: 97962 }, { ...target, nativePid: '97962' }, { ...target, nativePid: 0 }, { ...target, nativePid: NaN }, { ...target, nativePid: 1.5 }, { ...target, extra: true }];
    for (const scope of invalid) await assert.rejects(() => f.remote.selectApp(null, 1, false, scope));
    assert.deepEqual(f.calls, []);
    const driver = { ...f.context, opts: {}, log: f.remote.log, _remote: f.remote } as any;
    driver.remote = f.remote;
    assert.deepEqual(f.context.mobileInstalledContextContract.call(driver), { contract, selector: contract });
    const selectorContract = f.remote.installedContextContract;
    f.remote.installedContextContract = () => contract;
    assert.throws(() => f.context.mobileInstalledContextContract.call(driver), /selector contract/u, 'a driver-only echo is not a loaded selector contract');
    f.remote.installedContextContract = selectorContract;
    const select = f.remote.selectApp;
    f.remote.selectApp = () => { throw new Error('shadow old selector'); };
    assert.throws(() => f.context.mobileInstalledContextContract.call(driver), /selector contract/u);
    for (const scope of invalid) await assert.rejects(() => f.context.mobileGetInstalledContexts.call(driver, scope));
    await assert.rejects(() => f.context.mobileGetInstalledContexts.call(driver, target), /selector contract/u);
    assert.deepEqual(f.calls, []);
    f.remote.selectApp = select;
    driver.isRealDevice = () => false;
    driver.getCurrentUrl = () => undefined;
    driver.opts = { webviewConnectRetries: 1, ignoreAboutBlankUrl: true };
    f.reply(async id => [id, page(N)]);
    const installedPages = await f.context.mobileGetInstalledContexts.call(driver, target);
    assert.deepEqual(installedPages.map((p: any) => p.id), ['NATIVE_APP', 'WEBVIEW_97230.2']);
    assert.equal(installedPages[1].hostId, target.nativePid, 'numeric public projection retains page-level host evidence');
    assert.deepEqual(f.calls, [W]);
    assert.deepEqual(driver.opts, { webviewConnectRetries: 1, ignoreAboutBlankUrl: true }, 'scope never mutates options');
    const oldRemote = new f.RemoteDebugger({ log: f.remote.log });
    delete oldRemote.installedContextContract;
    let oldConnects = 0;
    oldRemote.connect = async () => { oldConnects++; return {}; };
    driver._remote = undefined;
    driver.getNewRemoteDebugger = async () => oldRemote;
    await assert.rejects(() => f.context.mobileGetInstalledContexts.call(driver, target), /selector contract/u);
    assert.equal(oldConnects, 0, 'a newly created unsupported downstream fails before setup/listing RPCs');
    driver._remote = f.remote;
    assert.deepEqual(f.executeMap['mobile: getContexts'].params, { optional: ['waitForWebviewMs'] });
    assert.deepEqual(f.executeMap['mobile: getInstalledContexts'].params, { required: ['targetApp'] });
    const safari = await fixture(runtime, { settleGeneric: true });
    const pages = await safari.remote.selectApp(null, 1, false);
    assert.deepEqual(safari.calls, [G, W]);
    assert.equal(safari.remote._appIdKey, S);
    assert.ok(pages.some((p: any) => p.id.startsWith('95890.')));
  }],
  ['iOS producer eligible 96010 is not blacklisted; silence remains client fatal with zero post-timeout HTTP', async () => {
    assert.ok(runtime);
    const f = await fixture(runtime, { generic: 'com.apple.SafariViewService', silent: G });
    let release!: () => void;
    const late = new Promise<void>(resolve => { release = resolve; });
    f.reply(async id => { await late; return [id, page(N)]; });
    let timer!: () => void;
    let transports = 0;
    const client = new AppiumClient('http://synthetic.invalid', 20_000, async (_input, init) => {
      transports++;
      const body = init?.body ? JSON.parse(String(init.body)) : {};
      if (!body.script) return Response.json({ value: {}, sessionId: 'fake' });
      return value(await f.remote.selectApp(null, 1, false, target));
    }, () => 0, { now: () => 0, timer: callback => { timer = callback; return 1 as unknown as ReturnType<typeof setTimeout>; }, clear: () => {} });
    await client.create({ capabilities: {} });
    const request = client.installedContextMetadata(target, 20_000);
    assert.equal(await pending(request), true);
    assert.deepEqual(f.calls, [G]);
    timer();
    await assert.rejects(request, /APPIUM_TIMEOUT/u);
    const frozen = structuredClone(client.snapshot());
    const fatal = frozen.firstFatal;
    const count = transports;
    for (const command of [() => client.close(), () => client.contextMetadata(), () => client.installedContextMetadata(target), () => client.installedContextContract()]) {
      await assert.rejects(command, /APPIUM_SESSION_UNUSABLE/u);
    }
    assert.equal(transports, count);
    assert.deepEqual(client.snapshot().firstFatal, fatal);
    assert.deepEqual(f.calls, [G], 'no healthy continuation to W after abandoned pending eligible RPC');
    release(); await late; await new Promise(resolve => setTimeout(resolve, 10));
    assert.equal(transports, count);
    assert.deepEqual(client.snapshot(), frozen, 'late producer response cannot rewrite terminal client evidence');
  }],
  ['iOS producer actual protocol integration retains passive blank publication and native/document fences', async () => {
    assert.ok(runtime);
    const origin = 'https://installed.synthetic.invalid';
    const directory = await mkdtemp(join(tmpdir(), 'producer-protocol-'));
    try {
      for (const mode of ['ready', 'wrong-native', 'overlay', 'wrong-url', 'not-standalone', 'pid-race', 'unsupported'] as const) {
        const f = await fixture(runtime);
        let discoveries = 0, attachments = 0, scripts = 0, nativePid = target.nativePid;
        const producer = { ...f.context, opts: { webviewConnectRetries: 1 }, log: f.remote.log, _remote: f.remote, remote: f.remote,
          isRealDevice: () => false, getCurrentUrl: () => undefined } as any;
        if (mode === 'unsupported') f.remote.selectApp = async () => { throw new Error('stock selector'); };
        f.reply(async id => [id, page(N, mode === 'ready' && discoveries === 1 ? 'about:blank' : `${origin}/`)]);
        const budget = new PhaseBudget('producer-protocol', { timeoutMs: 120_000, recoveryLimit: 0 });
        const platform = new IOSPlatform({ origin, appiumUrl: 'http://synthetic.invalid', outputDir: directory, certificate: '', setupUrl: '', deviceId: 'fake', budget });
        const transportCommands: string[] = [];
        const element = { 'element-6066-11e4-a52e-4f735466cecf': 'root' };
        const client = new AppiumClient('http://synthetic.invalid', 20_000, async (input, init) => {
          const path = new URL(String(input)).pathname, body = JSON.parse(String(init?.body || '{}'));
          transportCommands.push(body.script || path);
          if (path === '/session') return Response.json({ value: {}, sessionId: 'fake' });
          if (body.script === 'mobile: installedContextContract') return value(f.context.mobileInstalledContextContract.call(producer));
          if (body.script === 'mobile: activeAppInfo') return value({ bundleId: mode === 'wrong-native' ? 'com.apple.mobilesafari' : 'com.apple.webapp', pid: nativePid });
          if (body.script === 'mobile: queryAppState') return value(4);
          if (path.endsWith('/element/root/elements')) return value(mode === 'overlay' ? [element, element] : [element]);
          if (path.endsWith('/alert/text')) return Response.json({ value: { error: 'no such alert' } }, { status: 404 });
          if (body.script === 'mobile: getInstalledContexts') {
            discoveries++;
            if (discoveries === 2) { assert.equal(attachments, 0); assert.equal(scripts, 0, 'blank publication must stay passive'); }
            assert.deepEqual(body.args.targetApp, target);
            const contexts = await f.context.mobileGetInstalledContexts.call(producer, body.args.targetApp);
            if (mode === 'pid-race') nativePid++;
            return value(contexts);
          }
          if (path.endsWith('/context')) { if (body.name !== 'NATIVE_APP') attachments++; return value(null); }
          if (path.endsWith('/url')) return value(mode === 'wrong-url' ? 'https://wrong.synthetic.invalid/' : `${origin}/`);
          if (body.script?.startsWith('return {')) { scripts++; return value({ origin, standalone: mode !== 'not-standalone', applicationInitialized: true }); }
          throw new Error('Unexpected synthetic protocol operation');
        });
        await client.create({ capabilities: {} }); client.setBudget(budget);
        Object.assign(platform, { driver: client, installedBundleId: 'com.apple.webapp', springBoardRoot: 'root' });
        if (mode === 'ready') {
          await platform.attachToInstalledView();
          assert.equal(platform.evidenceSnapshot().installedDocumentBound, true);
          assert.equal(discoveries, 2); assert.equal(attachments, 1); assert.equal(scripts, 1);
          assert.deepEqual(f.calls, [W, W]);
        } else {
          await assert.rejects(() => platform.attachToInstalledView(), mode === 'unsupported' ? /selector contract/u : /IOS_CONTEXT_OWNERSHIP/u);
          assert.deepEqual(f.calls, mode === 'wrong-native' || mode === 'unsupported' ? [] : [W]);
          assert.equal(platform.evidenceSnapshot().installedDocumentBound, false);
        }
        assert.equal(budget.recoveryCount, 0);
        assert.equal(transportCommands.some(command => /getContexts|activateApp|launchApp|reload/u.test(command)), false);
      }
    } finally { await rm(directory, { recursive: true, force: true }); }
  }],
];

export const iosProducerPrivacyTests: Test[] = [
  ['iOS producer candidate diagnostics enforce fixed enums and bounds against adversarial private sentinels', async () => {
    const frame = (text: string) => `2026-10-29 11:04:34:000 - [RemoteDebugger] ${text}\n`;
    const line = 'Installed candidate app=96010 host=97230 native=97962 role=generic reason=proxy action=list';
    const trace = new IOSWebKitOutputDiagnostics();
    trace.observe(frame(line));
    for (const secret of ['private.bundle', 'https://private.invalid/?secret=token', 'private-title', 'body-secret', '\r', '\uFFFD']) {
      for (const field of ['role=generic', 'reason=proxy', 'action=list', 'app=96010', 'host=97230', 'native=97962']) trace.observe(frame(line.replace(field, field.split('=')[0] + '=' + secret)));
      trace.observe(frame(line + ' ' + secret));
    }
    trace.observe(frame(line).repeat(300)); trace.observe(frame(line + 'x'.repeat(5000))); trace.finish();
    assert.equal(trace.data.events.length, 256); assert.equal(trace.data.droppedEvents, 45);
    assert.equal(trace.data.oversizedRecords, 1);
    assert.equal(trace.data.recognizedRecords, 301);
    const out = JSON.stringify(trace.data);
    assert.doesNotMatch(out, /private\.|secret|token|title|body-secret|\uFFFD/u);
    assert.equal(trace.data.events[0].stage, 'installed-candidate');
  }],
];

export const iosInstalledClientTests: Test[] = [
  ['Appium installed metadata uses a distinct typed command, with no Safari fallback', async () => {
    const sent: any[] = [];
    const client = new AppiumClient('http://synthetic.invalid', 20_000, async (_url, init) => {
      const body = JSON.parse(String(init?.body || '{}')); sent.push(body);
      if (!body.script) return Response.json({ value: {}, sessionId: 'fake' });
      if (body.script === 'mobile: installedContextContract') return value({ contract, selector: contract });
      return value([{ id: 'WEBVIEW_97230.2', bundleId: 'com.apple.SafariViewService', hostId: 97962 }]);
    });
    await client.create({ capabilities: {} });
    await client.installedContextContract();
    const pages = await client.installedContextMetadata(target);
    assert.equal(pages[0].raw.hostId, target.nativePid);
    await client.contextMetadata();
    assert.deepEqual(sent.slice(1).map(body => [body.script, body.args]), [
      ['mobile: installedContextContract', {}], ['mobile: getInstalledContexts', { targetApp: target }], ['mobile: getContexts', {}],
    ]);
  }],
  ['Appium installed handshake rejects stock, partial, driver-only and shadow downstream contracts without discovery', async () => {
    for (const response of [null, {}, { contract }, { contract, selector: 'stock' }, { contract, selector: contract, private: 'sentinel' }]) {
      const sent: any[] = [];
      const client = new AppiumClient('http://synthetic.invalid', 20_000, async (_url, init) => {
        const body = JSON.parse(String(init?.body || '{}')); sent.push(body);
        return body.script ? value(response) : Response.json({ value: {}, sessionId: 'fake' });
      });
      await client.create({ capabilities: {} });
      await assert.rejects(() => client.installedContextContract(), /IOS_INSTALLED_CONTEXT_UNSUPPORTED/u);
      assert.deepEqual(sent.slice(1).map(body => body.script), ['mobile: installedContextContract']);
    }
  }],
];

// Run the actual gate in a fresh process so cached imports cannot hide changed producer bytes.
export const iosProducerGateTests: Test[] = [
  ['iOS producer integrity gate rejects versions, partial/drift/symlink/runtime paths before startup', async () => {
    assert.ok(runtime);
    const gate = new URL('../ios-appium/gate.ts', import.meta.url);
    const node = process.env.IOS_PRODUCER_NODE;
    assert.ok(node, 'Pinned IOS_PRODUCER_NODE required');
    const run = (mode: 'verify' | 'patch' = 'verify') => execFileSync(node, ['--experimental-import-meta-resolve', gate.pathname, mode, runtime], { stdio: 'pipe', env: { ...process.env, NODE_OPTIONS: '', NODE_PATH: '' } });
    run();
    const root = join(runtime, 'node_modules/appium-xcuitest-driver');
    const postimage = await readFile(join(root, 'build/lib/commands/context.js'));
    assert.throws(() => run('patch'), 'a partial/already-transformed preimage is never blindly patched again');
    assert.deepEqual(await readFile(join(root, 'build/lib/commands/context.js')), postimage);
    for (const file of ['package.json', 'build/lib/commands/context.js', 'lib/commands/context.ts', 'node_modules/appium-remote-debugger/package.json', 'node_modules/appium-webdriveragent/package.json', 'node_modules/appium-remote-debugger/build/lib/mixins/connect.js', '../appium/package.json']) {
      const path = join(root, file), before = await readFile(path);
      try {
        await writeFile(path, file.endsWith('package.json') ? JSON.stringify({ ...JSON.parse(before.toString()), version: '0.0.0' }) : Buffer.concat([before, Buffer.from('\n// drift\n')]));
        assert.throws(run, () => true, file);
      } finally { await writeFile(path, before); }
    }
    const path = join(root, 'build/lib/commands/context.js');
    await rename(path, `${path}.private-original`);
    assert.throws(run, 'missing runtime patch cannot pass startup verification');
    try { await symlink(`${path}.private-original`, path); assert.throws(run); }
    finally { await unlink(path); await rename(`${path}.private-original`, path); }
    const evidencePath = join(runtime, 'installed-context-integrity.json'), evidence = await readFile(evidencePath);
    const manifestPath = new URL('../ios-appium/producer-manifest.json', import.meta.url);
    const manifestBytes = await readFile(manifestPath), runtimeBytes = await readFile(path);
    try {
      const changedRuntime = Buffer.concat([runtimeBytes, Buffer.from('\n')]);
      const changedManifest = JSON.parse(manifestBytes.toString());
      changedManifest.files['xcuitest/build/lib/commands/context.js'].after = createHash('sha256').update(changedRuntime).digest('hex');
      await writeFile(path, changedRuntime);
      await writeFile(manifestPath, JSON.stringify(changedManifest));
      assert.throws(run, /captured installation hashes changed/u, 'paired manifest and runtime mutation cannot replace captured installation identity');
    } finally { await writeFile(path, runtimeBytes); await writeFile(manifestPath, manifestBytes); }
    try {
      const legacy = JSON.parse(evidence.toString()); delete legacy.attestation;
      await writeFile(evidencePath, JSON.stringify(legacy));
      assert.throws(run, /captured installation hashes changed/u, 'old records require explicit installation, never silent regeneration');
    } finally { await writeFile(evidencePath, evidence); }
    try {
      const changed = JSON.parse(evidence.toString()); changed.main = '/wrong/shadow/appium.js';
      await writeFile(evidencePath, JSON.stringify(changed)); assert.throws(run);
    } finally { await writeFile(evidencePath, evidence); }
    run();
  }],
];
