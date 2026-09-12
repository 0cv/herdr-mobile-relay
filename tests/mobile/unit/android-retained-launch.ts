import assert from 'node:assert/strict';
import { chmod, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { AndroidPlatform } from '../platforms/android';
import { PhaseBudget } from '../support/budget';
import { AppiumClient } from '../support/webdriver';
import { retainedFixture, kernelFixture } from './android-retained-fixture';
import { gzipSync } from 'node:zlib';
import { kernelReaderFixture } from './android-kernel-reader-fixture';

export async function runAndroidRetainedLaunchRegressions(scenarios?: string[]): Promise<void> {
  for (const scenario of scenarios ?? ['boot', 'kernel-boot-acquisition', 'kernel-disabled', 'kernel-unreadable', 'kernel-missing', 'kernel-malformed', 'kernel-duplicate', 'kernel-conflicting', 'kernel-enabled-absent', 'kernel-disabled-fields', 'kernel-pid-duplicate', 'kernel-nspid-duplicate', 'kernel-nested', 'kernel-same-number-nested', 'healthy', 'pid-before', 'start-before', 'selected-window-loss', 'pid', 'start-time', 'missing-process', 'dead-process', 'missing-owner', 'replaced-session', 'missing-current', 'missing-handles', 'missing-context', 'refusal', 'timeout', 'malformed', 'wrong-id', 'wrong-component', 'wrong-scope', 'wrong-mac', 'ambiguous-shortcut', 'uncertain-launch', 'wrong-origin', 'stale-browser', 'wrong-provider', 'ambiguous-document', 'late-document', 'recorded-two-page']) {
    const root = await mkdtemp(join(tmpdir(), 'android-retained-launch-'));
    const saved = { adb: process.env.ADB, ownership: process.env.MOBILE_DEVICE_OWNERSHIP_FILE };
    const file = join(root, 'state.json');
    const adb = join(root, 'adb');
    const shortcut = 'ShortcutInfo {id=installed-id, shortLabel=Herdr Relay, org.chromium.chrome.browser.webapp_name=Herdr Mobile Relay, org.chromium.chrome.browser.webapp_url=https://fixture.test/, org.chromium.chrome.browser.webapp_scope=https://fixture.test/, org.chromium.chrome.browser.webapp_mac=c2lnbmVk, org.chromium.chrome.browser.webapp_id=installed-id, intents=[Intent { act=com.google.android.apps.chrome.webapps.WebappManager.ACTION_START_WEBAPP pkg=com.android.chrome cmp=com.android.chrome/org.chromium.chrome.browser.webapps.WebappLauncherActivity }]}';
    const initialPid = scenario === 'kernel-disabled' ? '2863' : '123';
    await writeFile(file, JSON.stringify({ scenario, shortcut, launched: 0, calls: [], pid: initialPid, startTime: '456' }));
    const kernel = await kernelReaderFixture(async command => {
      const s = JSON.parse(await readFile(file, 'utf8'));
      s.calls.push(`shell ${command}`);
      await writeFile(file, JSON.stringify(s));
      if (command === 'cat /proc/config.gz') {
        if (s.scenario === 'kernel-unreadable') throw new Error('Fixture unreadable config');
        const invalid: Record<string, string> = {'kernel-missing': 'CONFIG_NAMESPACES=y\n', 'kernel-malformed': 'CONFIG_PID_NS=n\n',
          'kernel-duplicate': 'CONFIG_PID_NS=y\nCONFIG_PID_NS=y\n', 'kernel-conflicting': 'CONFIG_PID_NS=y\n# CONFIG_PID_NS is not set\n'};
        return gzipSync('CONFIG_IKCONFIG=y\nCONFIG_IKCONFIG_PROC=y\n' + (invalid[s.scenario] ?? (['kernel-disabled', 'kernel-disabled-fields'].includes(s.scenario) ? 'CONFIG_NAMESPACES=y\n# CONFIG_PID_NS is not set\n' : 'CONFIG_PID_NS=y\n')));
      }
      if (command === 'pidof com.android.chrome') return s.launched && s.scenario === 'missing-process' ? '' : s.pid;
      if (command.endsWith('/stat')) return `${s.pid} (chrome) ${s.launched && s.scenario === 'dead-process' ? 'Z' : 'S'} ${[...Array(18).fill('0'), s.startTime, '0'].join(' ')}`;
      if (command === 'cat /proc/sys/kernel/random/boot_id') {
        const changed = (s.scenario === 'boot' && s.launched) || (s.scenario === 'kernel-boot-acquisition' && s.calls.filter((call: string) => call.endsWith('/boot_id')).length === 2);
        return changed ? '22222222-2222-2222-2222-222222222222' : '11111111-1111-1111-1111-111111111111';
      }
      if (command.endsWith('/status')) {
        if (s.scenario === 'kernel-disabled') return await readFile(new URL(`./fixtures/android/kernel-disabled-${command.includes('/self/') ? 'self' : 'chrome'}-status.txt`, import.meta.url), 'utf8');
        const pid = command.includes('/self/') ? '987' : s.pid;
        const visible = `Pid:\t${pid}\n`;
        const ns = `NSpid:\t${pid}\n`;
        if (s.scenario === 'kernel-enabled-absent') return visible;
        if (s.scenario === 'kernel-pid-duplicate') return visible + visible + ns;
        if (s.scenario === 'kernel-nspid-duplicate') return visible + ns + ns;
        if (s.scenario === 'kernel-nested') return visible + `NSpid:\t${pid}\t1\n`;
        if (s.scenario === 'kernel-same-number-nested') return visible + `NSpid:\t${pid}\t${pid}\n`;
        return visible + ns;
      }
      throw new Error(`Unexpected fixed reader request: ${command}`);
    });
    await writeFile(adb, `#!${process.execPath}
const file = ${JSON.stringify(file)};
const s = await Bun.file(file).json();
const cmd = process.argv.slice(4).join(' ');
s.calls.push(cmd);
let output = '';
if (cmd === 'shell pidof com.android.chrome') output = s.launched && s.scenario === 'missing-process' ? '' : s.pid;
else if (cmd.startsWith('shell cat /proc/') && cmd.endsWith('/stat')) output = s.pid + ' (chrome) ' + (s.launched && s.scenario === 'dead-process' ? 'Z' : 'S') + ' ' + [...Array(18).fill('0'), s.startTime, '0'].join(' ');
else if (cmd === 'shell cat /proc/sys/kernel/random/boot_id') output = '11111111-1111-1111-1111-111111111111';
else if (cmd.endsWith('/status')) output = 'Pid:\\t' + (cmd.includes('/self/') ? '987' : s.pid) + '\\nNSpid:\\t' + (cmd.includes('/self/') ? '987' : s.pid);
else if (cmd === 'shell cat /proc/net/unix') output = '0000000000000000: 00000002 00000000 00010000 0001 01 4321 @chrome_devtools_remote';
else if (cmd === 'shell dumpsys activity activities') output = 'ResumedActivity: ActivityRecord{x u0 com.android.chrome/' + (s.launched ? 'org.chromium.chrome.browser.webapps.WebappActivity' : 'com.google.android.apps.chrome.Main') + ' pid=' + s.pid + '}';
else if (cmd.startsWith('shell cmd shortcut')) {
  output = s.shortcut;
  if (s.scenario === 'wrong-id') output = output.replace('webapp_id=installed-id', 'webapp_id=other');
  if (s.scenario === 'wrong-component') output = output.replace('cmp=com.android.chrome/', 'cmp=wrong.package/');
  if (s.scenario === 'wrong-scope') output = output.replace('webapp_scope=https://fixture.test/', 'webapp_scope=https://fixture.test/other/');
  if (s.scenario === 'wrong-mac') output = output.replace('webapp_mac=c2lnbmVk', 'webapp_mac=not!signed');
  if (s.scenario === 'ambiguous-shortcut') output += '\\n' + output;
} else if (cmd.includes("'am' 'start'")) {
  s.launched++;
  if (s.scenario === 'pid') s.pid = '124';
  if (s.scenario === 'start-time') s.startTime = '457';
  output = s.scenario === 'uncertain-launch' ? 'Error: uncertain launch' : 'Status: ok';
} else throw new Error('unexpected adb command: ' + cmd);
await Bun.write(file, JSON.stringify(s));
console.log(output);
`);
    await chmod(adb, 0o700);
    await writeFile(join(root, 'owned'), 'android:emulator-5554\n');
    process.env.ADB = adb;
    process.env.MOBILE_DEVICE_OWNERSHIP_FILE = join(root, 'owned');
    const calls: Array<{ path: string; method: string; body: any; launched: number }> = [];
    let active = 0;
    let maxActive = 0;
    const bootstrap = '2E26E8C2C4CFF68B69AA865CD8132F98';
    const installed = '753D4398F5ABC414D3DAABBF0B329743';
    const originalStartedAt = Date.now();
    let selected = bootstrap;
    let failed = false;
    let lostSelected = false;
    let documents = 0;
    const pending: Promise<unknown>[] = [];
    const budget = new PhaseBudget('retained-launch-test', { timeoutMs: 180_000, recoveryLimit: 0 });
    const response = (value: unknown, status = 200) => Response.json({ value, sessionId: 'original' }, { status });
    const client = new AppiumClient('http://retained.invalid', 1_500, async (input, init) => {
      const path = new URL(String(input)).pathname.replace('/session/original', '');
      const body = init?.body ? JSON.parse(String(init.body)) : {};
      const native = JSON.parse(await readFile(file, 'utf8'));
      calls.push({ path, method: init?.method || 'GET', body, launched: native.launched });
      active++;
      maxActive = Math.max(maxActive, active);
      try {
        if (path === '/session') { Object.assign(platform, {kernelReader: kernel.create()}); return response({}); }
        if (path === '/appium/settings') return response({ waitForIdleTimeout: 500, waitForSelectorTimeout: 0 });
        if (path === '/context' && !failed && ['refusal', 'timeout', 'malformed'].includes(scenario)) {
          failed = true;
          if (scenario === 'refusal') return response({ error: 'unknown error', message: 'retained producer refuses' }, 500);
          if (scenario === 'malformed') return new Response('{');
          const late = new Promise<Response>(resolve => init?.signal?.addEventListener('abort', () => setTimeout(() => resolve(response(null)), 30), { once: true }));
          pending.push(late);
          return await late;
        }
        if (path === '/contexts') return response(scenario === 'missing-context' ? ['NATIVE_APP'] : ['NATIVE_APP', 'CHROMIUM', 'WEBVIEW_other']);
        if (path === '/window/handles') return response(scenario === 'missing-handles' ? [] : [bootstrap, lostSelected ? 'replacement' : installed, ...(scenario === 'ambiguous-document' ? ['second'] : [])]);
        if (path === '/window') {
          if (init?.method === 'POST') selected = body.handle;
          return response(scenario === 'missing-current' ? '' : selected);
        }
        if (path === '/url') return response(scenario === 'wrong-origin' ? 'https://wrong.test/' : 'https://fixture.test/');
        if (path === '/execute/sync') {
          if (body.script === 'mobile: getContexts') return response([]);
          if (body.script === 'mobile: inspectRetainedChromeTargets') {
            assert.equal(body.args.length, 1);
            assert.ok(body.args[0].deadline - Date.now() >= 7500);
            const result = retainedFixture([bootstrap, installed], selected, installed, originalStartedAt);
            if (scenario === 'kernel-disabled') {
              for (const identity of [result.original, result.before.native, result.before.nativeBefore, result.after.native, result.after.nativeBefore]) {
                identity.pid = initialPid;
                identity.namespace = 'kernel-pid-namespaces-disabled';
                identity.kernelCapability = kernelFixture('disabled', originalStartedAt);
              }
              result.processAssociation.before.pid = Number(initialPid);
              result.processAssociation.after.pid = Number(initialPid);
            }
            if (lostSelected) result.after.handles = [bootstrap];
            if (scenario === 'missing-context') return response({ error: 'unknown error', message: 'original context missing' }, 500);
            if (scenario === 'wrong-origin') result.observations[1].document.origin = 'https://wrong.test';
            if (scenario === 'stale-browser' || scenario === 'late-document') result.observations[1].document.standalone = false;
            if (scenario === 'wrong-provider') result.observations[1].document.provider = 'browser';
            if (scenario === 'ambiguous-document') {
              result.observations[0].document.standalone = true;
              result.observations[0].document.provider = 'android-standalone';
            }
            return response(result);
          }
          documents++;
          const standalone = selected !== bootstrap && scenario !== 'stale-browser' && !(scenario === 'late-document' && documents < 3);
          return response({ origin: 'https://fixture.test', standalone, provider: standalone && scenario !== 'wrong-provider' ? 'android-standalone' : 'browser' });
        }
        return response(null);
      } finally { active--; }
    });
    const platform = new AndroidPlatform({ origin: 'https://fixture.test', appiumUrl: 'http://retained.invalid', outputDir: root, certificate: '', setupUrl: '', deviceId: 'emulator-5554', budget });
    Object.assign(platform, { driver: client });
    try {
      const captureStartedAt = Date.now();
      if (scenario.startsWith('kernel-') && scenario !== 'kernel-disabled') {
        let first: unknown;
        await assert.rejects(() => (platform as any).createChromeSession(false), error => { first = error; return /ANDROID_CONTEXT_OWNERSHIP/u.test(String(error)); });
        const state = JSON.parse(await readFile(file, 'utf8'));
        state.scenario = 'healthy';
        await writeFile(file, JSON.stringify(state));
        await assert.rejects(() => (platform as any).readChromeProcess(), error => error === first);
        await assert.rejects(() => platform.launchInstalledApp(), error => error === first);
        assert.deepEqual(JSON.parse(await readFile(file, 'utf8')), state);
        console.log(`PASS retained kernel acquisition ${scenario}`);
        continue;
      }
      await (platform as any).createChromeSession(false);
      const captureFinishedAt = Date.now();
      const processEvents = () => (platform.evidenceSnapshot().events as Array<{ operation: string; detail: Record<string, string> }>)
        .filter(event => event.operation === 'chrome-process-observed');
      assert.equal(processEvents().length, 1);
      assert.deepEqual(Object.keys(processEvents()[0].detail).sort(), ['observationFinishedAt', 'observationStartedAt', 'pid', 'startTime']);
      assert.equal(processEvents()[0].detail.pid, initialPid);
      assert.equal(processEvents()[0].detail.startTime, '456');
      assert.ok(Date.parse(processEvents()[0].detail.observationStartedAt) >= captureStartedAt);
      assert.ok(Date.parse(processEvents()[0].detail.observationFinishedAt) <= captureFinishedAt);
      assert.ok(Date.parse(processEvents()[0].detail.observationStartedAt) <= Date.parse(processEvents()[0].detail.observationFinishedAt));
      if (['pid-before', 'start-before'].includes(scenario)) {
        const state = JSON.parse(await readFile(file, 'utf8'));
        if (scenario === 'pid-before') state.pid = '124'; else state.startTime = '457';
        await writeFile(file, JSON.stringify(state));
      }
      if (scenario === 'missing-owner') Object.assign(platform, { retainedOwner: undefined });
      if (scenario === 'replaced-session') await client.create({ capabilities: {} });
      if (['kernel-disabled', 'healthy', 'selected-window-loss', 'recorded-two-page'].includes(scenario)) {
        await platform.launchInstalledApp();
        assert.equal((platform as any).selectedInstalledWindow, installed);
        if (scenario === 'recorded-two-page') {
          await platform.attachToInstalledView();
          const switches = calls.filter(call => call.path === '/window' && call.method === 'POST' && call.launched === 1);
          assert.deepEqual(switches.map(call => call.body.handle), [installed],
            'Recorded two-page attachment must select the installed handle exactly once, never activate bootstrap or restore the bound handle');
          assert.ok(calls.some(call => call.body.script === 'mobile: inspectRetainedChromeTargets'),
            'Installed uniqueness must use the guarded nonactivating producer route');
        }
        assert.equal(calls.filter(call => call.path === '/session').length, 1);
        assert.equal(calls.some(call => call.method === 'DELETE'), false);
        const native = JSON.parse(await readFile(file, 'utf8'));
        assert.equal(native.launched, 1);
        assert.deepEqual(calls.filter(call => call.path === '/context').map(call => [call.body.name, call.launched]), [
          ['NATIVE_APP', 0], ['CHROMIUM', 0], ['NATIVE_APP', 0],
          ...calls.filter(call => call.path === '/context' && call.launched === 1).map(() => ['CHROMIUM', 1]),
        ]);
        assert.ok(calls.some(call => call.path === '/window' && call.method === 'GET' && call.launched === 0));
        assert.ok(calls.some(call => call.path === '/url' && call.launched === 0));
        assert.ok(native.calls.filter((call: string) => call.endsWith(`/proc/${initialPid}/stat`)).length >= 4);
        assert.equal(processEvents().length, native.calls.filter((call: string) => call.endsWith(`/proc/${initialPid}/stat`)).length);
        assert.ok(processEvents().every(event => event.detail.pid === initialPid && event.detail.startTime === '456'));
        assert.ok(native.calls.find((call: string) => call.includes("'am' 'start'"))?.includes("'--es' 'org.chromium.chrome.browser.webapp_mac' 'c2lnbmVk'"));
        if (scenario === 'selected-window-loss') {
          lostSelected = true;
          let first: unknown;
          await assert.rejects(() => platform.attachToInstalledView(), error => { first = error; return /retained installed observation or selection failed/u.test(String(error)); });
          const count = calls.length;
          await assert.rejects(() => platform.attachToInstalledView(), error => error === first);
          assert.equal(calls.length, count);
        }
        if (scenario === 'healthy') {
          await platform.relaunchInstalledApp();
          assert.equal(calls.filter(call => call.method === 'DELETE').length, 1);
          assert.equal(calls.filter(call => call.path === '/session').length, 2);
          assert.equal(JSON.parse(await readFile(file, 'utf8')).launched, 2);
        }
      } else {
        let first: unknown;
        await assert.rejects(() => platform.launchInstalledApp(), error => { first = error; return /ANDROID_(?:CONTEXT_OWNERSHIP|SHORTCUT)/u.test(String(error)); });
        await Promise.all(pending);
        const count = calls.length;
        const native = await readFile(file, 'utf8');
        const fatal = client.snapshot().firstFatal;
        await assert.rejects(() => platform.launchInstalledApp(), error => error === first);
        await assert.rejects(() => platform.relaunchInstalledApp(), error => error === first);
        await assert.rejects(() => platform.attachToInstalledView(), error => error === first);
        assert.equal(calls.length, count);
        assert.equal(await readFile(file, 'utf8'), native);
        assert.deepEqual(client.snapshot().firstFatal, fatal);
        const afterLaunch = ['boot', 'pid', 'start-time', 'missing-process', 'dead-process', 'missing-context', 'uncertain-launch', 'wrong-origin', 'stale-browser', 'wrong-provider', 'ambiguous-document', 'late-document'].includes(scenario);
        assert.equal(JSON.parse(native).launched, afterLaunch ? 1 : 0);
        assert.equal(calls.some(call => call.method === 'DELETE'), false);
        assert.equal(calls.filter(call => call.path === '/session').length, scenario === 'replaced-session' ? 2 : 1);
      }
      assert.equal(calls.some(call => call.path === '/url' && call.method === 'POST'), false);
      assert.equal(calls.some(call => call.body.name === 'WEBVIEW_other'), false);
      assert.equal(maxActive, 1);
      console.log(`PASS retained signed launch ${scenario}`);
    } finally {
      await Promise.all(pending);
      assert.equal(active, 0);
      if (saved.adb === undefined) delete process.env.ADB; else process.env.ADB = saved.adb;
      if (saved.ownership === undefined) delete process.env.MOBILE_DEVICE_OWNERSHIP_FILE; else process.env.MOBILE_DEVICE_OWNERSHIP_FILE = saved.ownership;
      await kernel.close();
      await rm(root, { recursive: true, force: true });
    }
  }
}

if (import.meta.main) await runAndroidRetainedLaunchRegressions(process.argv.includes('--recorded-two-page') ? ['recorded-two-page'] : undefined);
