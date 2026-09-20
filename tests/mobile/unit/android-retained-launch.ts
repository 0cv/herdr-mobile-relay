import assert from 'node:assert/strict';
import { appendFile, chmod, cp, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { existsSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { rootCertificates } from 'node:tls';
import { join } from 'node:path';
import { AndroidPlatform, androidChromeShortcutArgs, parseAndroidChromeShortcuts } from '../platforms/android';
import { AndroidEnvironmentMeasurement } from '../android-measurement';
import { AndroidStartupLog } from '../support/android-startup-log';
import { CommandError } from '../support/process';
import { repositoryPath } from '../support/paths';
import { PhaseBudget } from '../support/budget';
import { AppiumClient } from '../support/webdriver';
import { retainedFixture, kernelFixture } from './android-retained-fixture';
import { gzipSync } from 'node:zlib';
import { kernelReaderFixture } from './android-kernel-reader-fixture';

export async function runAndroidRetainedLaunchRegressions(scenarios?: string[]): Promise<void> {
  for (const scenario of scenarios ?? ['boot', 'kernel-boot-acquisition', 'kernel-disabled', 'kernel-unreadable', 'kernel-missing', 'kernel-malformed', 'kernel-duplicate', 'kernel-conflicting', 'kernel-enabled-absent', 'kernel-disabled-fields', 'kernel-pid-duplicate', 'kernel-nspid-duplicate', 'kernel-nested', 'kernel-same-number-nested', 'healthy', 'pid-before', 'start-before', 'selected-window-loss', 'pid', 'start-time', 'missing-process', 'dead-process', 'missing-owner', 'replaced-session', 'missing-current', 'missing-handles', 'missing-context', 'refusal', 'timeout', 'malformed', 'wrong-id', 'wrong-component', 'wrong-scope', 'wrong-mac', 'ambiguous-shortcut', 'uncertain-launch', 'wrong-origin', 'stale-browser', 'wrong-provider', 'ambiguous-document', 'late-document', 'zero-candidate', 'still-browser-confirmation', 'recorded-two-page']) {
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
    await writeFile(adb, `#!${process.execPath} --no-env-file
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
            if (scenario === 'zero-candidate') {
              result.observations[1].document.href = 'https://other.test/';
              result.observations[1].document.origin = 'https://other.test';
              result.targets[1].url = 'https://other.test/';
            }
            if (scenario === 'still-browser-confirmation') {
              result.phase = 'initial-browser-selected';
              for (const snapshot of [result.before, result.after]) {
                snapshot.document.standalone = false;
                snapshot.document.provider = 'browser';
              }
            }
            if (scenario === 'stale-browser' || scenario === 'late-document') {
              result.before.document.standalone = false;
              result.before.document.provider = 'browser';
              result.after.document.standalone = false;
              result.after.document.provider = 'browser';
            }
            if (scenario === 'wrong-provider') result.after.document.provider = 'browser';
            if (scenario === 'ambiguous-document') {
              const second = 'C'.repeat(32);
              result.targets.push({ targetId: second, type: 'page', url: 'https://fixture.test/', title: '' });
              result.observations.push({ targetId: second, document: { href: 'https://fixture.test/', origin: 'https://fixture.test', timeOrigin: originalStartedAt - 1, backendNodeId: 3 } });
              result.before.handles.push(second);
              result.after.handles.push(second);
              result.processAssociation.after.requestId = 4 + 7 * result.before.handles.length;
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
          assert.equal(calls.filter(call => call.method === 'DELETE').length, 0);
          assert.equal(calls.filter(call => call.path === '/session').length, 1);
          assert.equal(JSON.parse(await readFile(file, 'utf8')).launched, 2);
        }
      } else {
        let first: unknown;
        await assert.rejects(() => platform.launchInstalledApp(), error => { first = error; return /ANDROID_(?:CONTEXT_OWNERSHIP|SHORTCUT|LAUNCH)/u.test(String(error)); });
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
        const afterLaunch = ['boot', 'pid', 'start-time', 'missing-process', 'dead-process', 'missing-context', 'uncertain-launch', 'wrong-origin', 'stale-browser', 'wrong-provider', 'ambiguous-document', 'late-document', 'zero-candidate', 'still-browser-confirmation'].includes(scenario);
        assert.equal(JSON.parse(native).launched, afterLaunch ? 1 : 0);
        assert.equal(calls.some(call => call.method === 'DELETE'), false);
        assert.equal(calls.filter(call => call.path === '/session').length, scenario === 'replaced-session' ? 2 : 1);
        if (scenario === 'zero-candidate' || scenario === 'still-browser-confirmation') {
          assert.deepEqual(calls.filter(call => call.path === '/window' && call.method === 'POST').map(call => call.body.handle), scenario === 'zero-candidate' ? [] : [installed]);
        }
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

export const androidLifecycleCases = {
  happy: ['initial-then-final-warm-then-background-warm', 'measured-cold-then-warm', 'same-owner-same-handle-navigation-between-activations'],
  warmOwner: ['missing-owner', 'changed-driver-session', 'changed-native-pid', 'changed-native-start', 'changed-boot', 'changed-kernel-capability', 'missing-selected-handle', 'replaced-selected-handle', 'missing-shortcut', 'duplicate-shortcut', 'changed-shortcut-id', 'changed-shortcut-scope', 'changed-shortcut-url', 'changed-shortcut-mac', 'wrong-activity', 'wrong-provider', 'wrong-origin', 'document-change-within-read'],
  coldHandoff: ['no-measurement-termination', 'termination-command-failure', 'termination-removal-readback-failure', 'home-failure-after-successful-stop', 'changed-measurement-object', 'changed-measurement-id', 'changed-target', 'changed-owner', 'replaced-old-session', 'prior-platform-fatal', 'prior-driver-fatal-after-cleanup', 'reused-consumed-handoff', 'intervening-incompatible-operation', 'premature-native-resurrection', 'concurrent-cold-consumers', 'close-transport-failure', 'close-404-resolves-with-firstFatal', 'close-malformed-2xx-resolves-with-firstFatal', 'create-refusal', 'create-timeout'],
  readinessAndBudget: ['warm-foreground-not-ready', 'warm-devtools-not-ready', 'cold-foreground-not-ready', 'cold-devtools-not-ready', 'insufficient-shortcut-intent-tail', 'insufficient-cold-close-tail', 'insufficient-warm-attachment-tail', 'insufficient-cold-create-settings-owner-attachment-tail', 'insufficient-read-postinspection-reserve', 'late-readiness-success', 'late-transport-success-after-fatal', 'late-body-success-after-fatal', 'lifecycle-overlap-before-handoff-armed'],
} as const;

interface LifecycleFixtureInput {
  freshSetup?: boolean;
  realCertificate?: boolean;
  root: string;
  fixtureDirectory: string;
  log: string;
  environment: NodeJS.ProcessEnv;
}

export async function withAndroidRetainedLifecycleFixture(fixture: LifecycleFixtureInput, run: (control: {
  platform: AndroidPlatform;
  client: AppiumClient;
  measurement: AndroidEnvironmentMeasurement;
  calls: Array<{ path: string; requestPath: string; method: string; body: any }>;
  state: () => Promise<any>;
  change: (value: Record<string, unknown>) => Promise<void>;
  remaining: (milliseconds: number) => Promise<void>;
  operations: () => Promise<any[]>;
  hold: (point: 'delete' | 'inspection-transport' | 'inspection-body' | 'settings-create' | 'chrome-create', onEnter?: () => void) => { entered: Promise<void>; release: () => void };
  assertTerminal: (first: unknown) => Promise<void>;
}) => Promise<void>): Promise<void> {
  const saved = { ...process.env };
  const cwd = process.cwd();
  const startupStart = AndroidStartupLog.prototype.start;
  const certificate = fixture.realCertificate ? join(fixture.root, 'certificate.crt') : '';
  if (fixture.realCertificate) {
    assert.ok(rootCertificates[0]?.startsWith('-----BEGIN CERTIFICATE-----'));
    await writeFile(certificate, rootCertificates[0]);
  }
  const file = join(fixture.root, 'lifecycle.json');
  const stopped = join(fixture.fixtureDirectory, 'chrome-stopped');
  const adb = join(fixture.root, 'bin', 'adb');
  const shortcut = 'ShortcutInfo {id=installed-id, shortLabel=Herdr Relay, org.chromium.chrome.browser.webapp_name=Herdr Mobile Relay, org.chromium.chrome.browser.webapp_url=https://fixture.test/, org.chromium.chrome.browser.webapp_scope=https://fixture.test/, org.chromium.chrome.browser.webapp_mac=c2lnbmVk, org.chromium.chrome.browser.webapp_id=installed-id, org.chromium.chrome.browser.webapp_source=6, org.chromium.chrome.browser.webapp_display_mode=1, org.chromium.content_public.common.orientation=-1, intents=[Intent { act=com.google.android.apps.chrome.webapps.WebappManager.ACTION_START_WEBAPP pkg=com.android.chrome cmp=com.android.chrome/org.chromium.chrome.browser.webapps.WebappLauncherActivity }]}';
  const state = async () => JSON.parse(await readFile(file, 'utf8'));
  const change = async (value: Record<string, unknown>) => writeFile(file, JSON.stringify({ ...await state(), ...value }));
  await writeFile(file, JSON.stringify({ shortcut, fault: '', launches: 0, foreground: 'browser', startTime: '456', pid: '6538', timeOffset: 0, signed: [], navigation: Date.now() - 1 }));
  await writeFile(adb, `#!${process.execPath} --no-env-file
import { existsSync, readFileSync, writeFileSync, appendFileSync, unlinkSync } from 'node:fs';
const file = ${JSON.stringify(file)};
const stopped = ${JSON.stringify(stopped)};
const log = ${JSON.stringify(fixture.log)};
const s = JSON.parse(readFileSync(file, 'utf8'));
const args = process.argv.slice(2);
const request = args.slice(2).join(' ');
const save = () => writeFileSync(file, JSON.stringify(s));
const output = value => process.stdout.write(value);
const expire = () => { s.timeOffset += 31000; save(); };
if (args.join(' ') === 'devices') output('List of devices attached\\nemulator-5554\\tdevice\\n');
else if (request === 'wait-for-device' || request === 'shell pm clear com.android.chrome') output('');
else if (request === 'shell pm list packages') {
  appendFileSync(log, request + '\\n');
  if (s.fault === 'setup-before-settings') {
    writeFileSync(file + '.setup-wait', 'ready');
    while (!existsSync(file + '.setup-release')) await Bun.sleep(5);
  }
  output('package:com.android.chrome\\n');
} else if (request.startsWith('push ') || request.startsWith('shell content call')
  || request.startsWith('shell am broadcast') || request === 'shell am force-stop com.google.android.documentsui'
  || request === 'shell am force-stop com.android.settings' || request.startsWith('shell am start -a com.android.settings.')
  || request.startsWith('shell rm -f /sdcard/Download/')) {
  appendFileSync(log, 'CERTIFICATE ' + request + '\\n');
  const point = request.startsWith('push ') ? 'push' : request.startsWith('shell content call') ? 'scan' : '';
  if (point && s.fault.startsWith('certificate-' + point + '-')) {
    writeFileSync(file + '.certificate-wait', 'ready');
    while (!existsSync(file + '.certificate-release')) await Bun.sleep(5);
    appendFileSync(log, 'CERTIFICATE_SETTLED ' + point + '\\n');
    if (s.fault.includes('-failure-')) { process.stderr.write('synthetic certificate command failure\\n'); process.exit(1); }
  }
} else if (request.startsWith('shell cmd shortcut')) {
  appendFileSync(log, request + '\\n');
  let value = s.shortcut;
  if (s.fault === 'missing-shortcut') value = '';
  if (s.fault === 'duplicate-shortcut') value += '\\n' + value;
  if (s.fault === 'changed-shortcut-id') value = value.replaceAll('installed-id', 'different-id');
  if (s.fault === 'changed-shortcut-scope') value = value.replace('webapp_scope=https://fixture.test/', 'webapp_scope=https://fixture.test/narrow/').replace('webapp_url=https://fixture.test/', 'webapp_url=https://fixture.test/narrow/');
  if (s.fault === 'changed-shortcut-url') value = value.replace('webapp_url=https://fixture.test/', 'webapp_url=https://fixture.test/changed');
  if (s.fault === 'changed-shortcut-mac' || s.fault === 'changed-target') value = value.replace('c2lnbmVk', 'Y2hhbmdlZA==');
  output(value);
} else if (request.includes("'am' 'start'")) {
  appendFileSync(log, 'SIGNED ' + request + '\\n');
  s.signed.push(args);
  s.launches++;
  s.foreground = 'installed';
  if (existsSync(stopped)) { unlinkSync(stopped); s.startTime = String(Number(s.startTime) + 1); }
  save();
  output('Status: ok\\n');
} else if (request === 'shell cat /proc/net/unix') {
  appendFileSync(log, request + '\\n');
  if (s.launches > 1 && s.fault.includes('devtools-not-ready')) { expire(); output(''); }
  else {
    if (s.launches > 1 && s.fault === 'wrong-activity') s.foreground = 'browser';
    if (s.launches > 1 && s.fault === 'insufficient-warm-attachment-tail') s.timeOffset = 600000 - 17000;
    if (s.launches > 1 && s.fault === 'insufficient-cold-create-settings-owner-attachment-tail') s.timeOffset = 600000 - 120000;
    save();
    output('@chrome_devtools_remote\\n');
  }
} else if (request === 'shell dumpsys activity activities') {
  appendFileSync(log, request + '\\n');
  let activity = s.foreground === 'installed' ? 'org.chromium.chrome.browser.webapps.WebappActivity' : 'com.google.android.apps.chrome.Main';
  if (s.launches > 1 && s.fault.includes('foreground-not-ready')) { activity = 'com.google.android.apps.chrome.Main'; expire(); }
  if (s.launches > 1 && s.fault === 'late-readiness-success') expire();
  const component = s.foreground === 'launcher' ? 'com.google.android.apps.nexuslauncher/.NexusLauncherActivity' : 'com.android.chrome/' + activity;
  output('ResumedActivity: ActivityRecord{x u0 ' + component + ' pid=' + (s.foreground === 'launcher' ? '2000' : s.pid) + '}\\n');
} else if (request === 'shell input keyevent KEYCODE_HOME') {
  appendFileSync(log, existsSync(stopped) ? 'HOME_AFTER_STOP\\n' : 'HOME_BACKGROUND\\n');
  if (s.fault === 'home-failure-after-successful-stop') { process.stderr.write('HOME refused\\n'); process.exit(1); }
  if (s.fault === 'lifecycle-overlap-before-handoff-armed') {
    writeFileSync(file + '.home-wait', 'ready');
    while (!existsSync(file + '.home-release')) await Bun.sleep(5);
  }
  s.foreground = 'launcher';
  save();
} else if (request === 'shell pidof com.android.chrome' && s.fault === 'premature-native-resurrection') {
  appendFileSync(log, request + '\\n'); output(s.pid + '\\n');
} else {
  await import(${JSON.stringify(repositoryPath('tests/mobile/unit/android-fake-adb.ts'))});
}
`);
  await chmod(adb, 0o700);
  const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
  const budget = new PhaseBudget('retained-lifecycle', { timeoutMs: 600_000, recoveryLimit: 0, now: () => Date.now() + JSON.parse(readFileSync(file, 'utf8')).timeOffset });
  const kernel = await kernelReaderFixture(async command => {
    const s = await state();
    const pid = s.fault === 'changed-native-pid' ? '6539' : s.pid;
    if (command === 'cat /proc/config.gz') return gzipSync('CONFIG_IKCONFIG=y\nCONFIG_IKCONFIG_PROC=y\nCONFIG_PID_NS=y\n');
    if (command === 'pidof com.android.chrome') return existsSync(stopped) ? '' : pid;
    if (command.endsWith('/stat')) return `${pid} (chrome) S ${[...Array(18).fill('0'), s.fault === 'changed-native-start' ? '999' : s.startTime, '0'].join(' ')}`;
    if (command.endsWith('/status')) { const n = command.includes('/self/') ? '987' : pid; return `Pid:\t${n}\nNSpid:\t${n}\n`; }
    if (command.endsWith('/boot_id')) return s.fault === 'changed-boot' ? '22222222-2222-2222-2222-222222222222' : '11111111-1111-1111-1111-111111111111';
    throw new Error(`Unexpected kernel fixture request: ${command}`);
  });
  const bootstrap = '2E26E8C2C4CFF68B69AA865CD8132F98';
  const installed = '753D4398F5ABC414D3DAABBF0B329743';
  let selected = bootstrap;
  let session = '';
  let generation = 0;
  let sessionStartedAt = Date.now();
  let active = 0;
  let maxActive = 0;
  let gate: { point: string; entered: () => void; wait: Promise<void>; release: () => void; onEnter?: () => void } | undefined;
  const calls: Array<{ path: string; requestPath: string; method: string; body: any }> = [];
  const client = new AppiumClient('http://retained.invalid', 30_000, async (input, init) => {
    const requestPath = new URL(String(input)).pathname;
    const path = requestPath.replace(/^\/session\/[^/]+/u, '');
    const method = init?.method || 'GET';
    const body = init?.body ? JSON.parse(String(init.body)) : {};
    calls.push({ path, requestPath, method, body });
    const inspection = body.script === 'mobile: inspectRetainedChromeTargets';
    const settings = body.capabilities?.alwaysMatch?.['appium:appPackage'] === 'com.android.settings';
    const held = gate && ((gate.point === 'delete' && method === 'DELETE')
      || (gate.point.startsWith('inspection-') && inspection)
      || (path === '/session' && gate.point === (settings ? 'settings-create' : 'chrome-create'))) ? gate : undefined;
    held?.onEnter?.();
    await appendFile(fixture.log, `HTTP ${method} ${path}\n`);
    const s = await state();
    active++;
    maxActive = Math.max(maxActive, active);
    const response = (value: unknown, status = 200) => Response.json({ value, sessionId: session }, { status });
    try {
      if (held && held.point !== 'inspection-body') { held.entered(); await held.wait; }
      if (method === 'DELETE') {
        if (s.fault === 'close-timeout') throw new DOMException('synthetic DELETE timeout', 'TimeoutError');
        if (s.fault === 'close-transport-failure') throw new Error('synthetic DELETE transport failure');
        if (s.fault === 'close-404-resolves-with-firstFatal') return response({ error: 'invalid session id' }, 404);
        if (s.fault === 'close-malformed-2xx-resolves-with-firstFatal') return new Response('{', { status: 200 });
        return response(null);
      }
      if (path === '/session') {
        if (settings && s.fault === 'setup-settings-timeout') throw new DOMException('synthetic Settings create timeout', 'TimeoutError');
        if (generation && s.fault === 'create-refusal') return response({ error: 'session not created' }, 500);
        if (generation && s.fault === 'create-timeout') throw new DOMException('synthetic create timeout', 'TimeoutError');
        generation++;
        session = settings ? 'settings-1' : generation === 1 ? 'original' : `cold-${generation}`;
        sessionStartedAt = Date.now();
        selected = bootstrap;
        Object.assign(platform, { kernelReader: kernel.create() });
        return response({});
      }
      if (path === '/appium/settings') return response({ waitForIdleTimeout: 500, waitForSelectorTimeout: 0 });
      if (path === '/window/handles') return response(s.fault === 'missing-selected-handle' ? [bootstrap] : [bootstrap, installed]);
      if (path === '/window') { if (method === 'POST') selected = body.handle; return response(s.fault === 'replaced-selected-handle' ? bootstrap : selected); }
      if (path === '/url') return response('https://fixture.test/');
      if (path === '/contexts') return response(['NATIVE_APP', 'CHROMIUM']);
      if (inspection) {
        await appendFile(fixture.log, `INSPECT ${s.foreground}\n`);
        const result = retainedFixture([bootstrap, installed], selected, installed, sessionStartedAt);
        for (const bound of [result.before, result.after]) {
          for (const native of [bound.nativeBefore, bound.native]) {
            native.activity = s.foreground === 'installed' ? 'org.chromium.chrome.browser.webapps.WebappActivity'
              : s.foreground === 'launcher' ? '.NexusLauncherActivity' : 'com.google.android.apps.chrome.Main';
            native.provider = s.foreground === 'installed' ? 'android-standalone' : 'browser';
          }
        }
        result.original.sessionId = s.fault === 'changed-owner' ? 'replaced-owner' : session;
        for (const identity of [result.original, result.before.native, result.before.nativeBefore, result.after.native, result.after.nativeBefore]) {
          identity.pid = s.pid;
          identity.startTime = s.startTime;
          if (s.fault === 'changed-kernel-capability') identity.kernelCapability.sha256 = 'f'.repeat(64);
        }
        result.processAssociation.before.pid = Number(s.pid);
        result.processAssociation.after.pid = Number(s.pid);
        for (const observation of result.observations) observation.document.timeOrigin = s.navigation;
        for (const bound of [result.before, result.after]) bound.document.timeOrigin = s.navigation;
        if (s.fault === 'missing-selected-handle') result.after.handles = [bootstrap];
        if (s.fault === 'replaced-selected-handle') result.after.selectedHandle = bootstrap;
        if (s.fault === 'wrong-provider') result.after.document.provider = 'browser';
        if (s.fault === 'wrong-origin') result.after.document.origin = 'https://wrong.test';
        const reply = response(result);
        if (held?.point === 'inspection-body') {
          const text = await reply.text();
          Object.defineProperty(reply, 'text', { value: async () => { held.entered(); await held.wait; return text; } });
        }
        return reply;
      }
      if (path === '/execute/sync') {
        if (s.fault === 'prior-driver-fatal-after-cleanup') throw new DOMException('synthetic original execute timeout', 'TimeoutError');
        if (s.fault === 'document-change-within-read') await change({ navigation: s.navigation + 1 });
        if (s.fault === 'insufficient-read-postinspection-reserve') await change({ timeOffset: 600_000 - 10_000 });
        return response({ url: 'https://fixture.test/', origin: 'https://fixture.test', standalone: true, provider: 'android-standalone', navigationId: String(s.navigation), version: '0.21.0', assets: 380, build: 'fixture-candidate', buildFromApplication: true, entry: '/index.html', script: '/assets/main.js', style: '/assets/main.css', requiredAssetsReady: true, applicationInitialized: true });
      }
      return response(null);
    } finally { active--; }
  });
  const platform = new AndroidPlatform({ origin: 'https://fixture.test', appiumUrl: 'http://retained.invalid', outputDir: fixture.root, certificate, setupUrl: '', deviceId: 'emulator-5554', budget });
  Object.assign(platform, { driver: client });
  if (fixture.realCertificate) {
    assert.equal((platform as any).installCertificate, (AndroidPlatform.prototype as any).installCertificate);
    assert.equal((platform as any).certificateCommonName, (AndroidPlatform.prototype as any).certificateCommonName);
  } else if (fixture.freshSetup) Object.assign(platform, { installCertificate: async () => {
    client.assertSessionIdentity('settings-1');
    await client.execute('return "fixture-certificate-installed";');
    await appendFile(fixture.log, 'CERTIFICATE_INSTALLED\n');
  } });
  const operations = async () => JSON.parse(await readFile(join(fixture.root, 'android-environment-operations.json'), 'utf8'));
  try {
    Object.assign(process.env, fixture.environment, { ADB: adb, PATH: `${join(fixture.root, 'bin')}:${fixture.environment.PATH || ''}` });
    process.chdir(fixture.root);
    assert.equal(Boolean(process.env.ADB_SERVER_SOCKET || process.env.ANDROID_ADB_SERVER_ADDRESS || process.env.ANDROID_ADB_SERVER_PORT), false);
    if (fixture.freshSetup) AndroidStartupLog.prototype.start = function (serial) {
      const endpoint = process.env.ADB_SERVER_SOCKET;
      process.env.ADB_SERVER_SOCKET = 'disabled-for-setup-fixture';
      try { startupStart.call(this, serial); }
      finally {
        if (endpoint === undefined) delete process.env.ADB_SERVER_SOCKET;
        else process.env.ADB_SERVER_SOCKET = endpoint;
      }
    };
    await measurement.begin();
    platform.environmentMeasurement = measurement;
    if (!fixture.freshSetup) {
      await (platform as any).createChromeSession(false);
      await platform.launchInstalledApp();
    }
    const retained = {
      owner: (platform as any).retainedOwner,
      reader: (platform as any).kernelReader,
      capability: (platform as any).kernelCapability,
    };
    await run({ platform, client, measurement, calls, state, change, operations,
      remaining: milliseconds => change({ timeOffset: 600_000 - milliseconds }),
      hold: (point, onEnter) => {
        let entered!: () => void;
        let release!: () => void;
        const entry = new Promise<void>(resolve => { entered = resolve; });
        const wait = new Promise<void>(resolve => { release = resolve; });
        gate = { point, entered, wait, release, onEnter };
        return { entered: entry, release };
      },
      assertTerminal: async first => {
        assert.ok(first instanceof Error);
        const count = calls.length;
        const native = await readFile(fixture.log, 'utf8');
        const fatal = client.snapshot().firstFatal;
        if ((await state()).fault === 'prior-driver-fatal-after-cleanup') {
          assert.ok(fatal);
          assert.equal(fatal.code, 'APPIUM_TIMEOUT');
          assert.equal(fatal.path, '/session/original/execute/sync');
          assert.equal(fatal.method, 'POST');
          assert.match(String(first), /ANDROID_CONTEXT_OWNERSHIP/u);
          assert.notEqual(first, fatal);
        }
        const receipt = structuredClone(fatal);
        const selectedBefore = platform.evidenceSnapshot().selectedInstalledWindow;
        for (const operation of [() => platform.launchInstalledApp(), () => platform.relaunchInstalledApp(), () => platform.terminateInstalledApp(), () => platform.backgroundApp(), () => platform.attachToInstalledView(), () => platform.readRunningIdentity(), () => platform.readUpdateCompletion()]) {
          await assert.rejects(operation, error => error === first);
          assert.equal(client.snapshot().firstFatal, fatal);
          assert.deepEqual(client.snapshot().firstFatal, receipt);
          assert.equal(calls.length, count);
        }
        assert.equal(calls.length, count);
        assert.equal(await readFile(fixture.log, 'utf8'), native);
        assert.equal(client.snapshot().firstFatal, fatal);
        assert.deepEqual(client.snapshot().firstFatal, receipt);
        assert.equal(platform.evidenceSnapshot().selectedInstalledWindow, selectedBefore);
      },
    });
    assert.equal(maxActive, calls.length ? 1 : 0);
    if (!calls.some(call => call.method === 'DELETE') && calls.filter(call => call.path === '/session').length === 1) {
      assert.equal((platform as any).retainedOwner, retained.owner);
      assert.equal((platform as any).kernelReader, retained.reader);
      assert.equal((platform as any).kernelCapability, retained.capability);
    }
    assert.equal(calls.some(call => call.path === '/url' && call.method === 'POST'), false);
    for (const args of (await state()).signed) assert.deepEqual(args, androidChromeShortcutArgs('emulator-5554', parseAndroidChromeShortcuts(shortcut)[0]));
  } finally {
    try {
      gate?.release();
      await writeFile(file + '.home-release', 'released');
      await writeFile(file + '.setup-release', 'released');
      await writeFile(file + '.certificate-release', 'released');
      await measurement.finish().catch(() => undefined);
    } finally {
      try { await kernel.close(); }
      finally {
        AndroidStartupLog.prototype.start = startupStart;
        process.chdir(cwd);
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    }
    assert.equal(active, 0);
  }
}

async function runAndroidCertificateTeardownControls(fixture: LifecycleFixtureInput): Promise<void> {
  for (const variant of ['push-success-after-stop', 'push-failure-after-stop', 'scan-success-after-stop', 'scan-failure-after-stop', 'push-failure-before-stop']) {
    const root = await mkdtemp(join(fixture.root, 'certificate-'));
    const fixtureDirectory = join(root, 'fixture');
    const log = join(root, 'requests.log');
    await cp(fixture.fixtureDirectory, fixtureDirectory, { recursive: true });
    await mkdir(join(root, 'bin'));
    const isolated = { root, fixtureDirectory, log, freshSetup: true, realCertificate: true,
      environment: { ...fixture.environment, FAKE_ANDROID_FIXTURE_DIR: fixtureDirectory, FAKE_ANDROID_LOG: log } };
    await withAndroidRetainedLifecycleFixture(isolated, async ({ platform, client, calls, change, hold, assertTerminal }) => {
      await change({ fault: `certificate-${variant}` });
      const close = hold('delete');
      let first: unknown;
      let stopping: Promise<void> | undefined;
      let stopSettled = false;
      const pending = platform.startFreshDevice().then(() => undefined, (error: unknown) => error);
      let callCount: number;
      let native: string;
      try {
        const deadline = Date.now() + 5_000;
        while (!existsSync(join(root, 'lifecycle.json.certificate-wait')) && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 5));
        assert.ok(existsSync(join(root, 'lifecycle.json.certificate-wait')));
        client.assertSessionIdentity('settings-1');
        assert.equal(client.snapshot().firstFatal, undefined);
        assert.equal((platform as any).retainedOwner, undefined);
        callCount = calls.length;
        native = await readFile(log, 'utf8');
        assert.equal(calls.filter(call => call.path === '/session').length, 1);
        assert.equal(calls.some(call => call.method === 'DELETE'), false);
        assert.equal(native.split('\n').filter(line => line.startsWith('CERTIFICATE ')).length, variant.startsWith('scan-') ? 2 : 1);
        if (variant.endsWith('before-stop')) {
          await writeFile(join(root, 'lifecycle.json.certificate-release'), 'released');
          first = await pending;
          assert.ok(first instanceof CommandError);
          assert.equal(first.exitCode, 1);
          assert.equal(first.timedOut, false);
          assert.match(first.stderr, /synthetic certificate command failure/u);
        }
        stopping = platform.stopOwnedResources();
        void stopping.then(() => { stopSettled = true; }, () => { stopSettled = true; });
        assert.equal(platform.stopOwnedResources(), stopping);
        if (variant.endsWith('before-stop')) await Promise.race([close.entered, stopping.then(() => { throw new Error('Stop settled before DELETE barrier'); })]);
        await assert.rejects(() => platform.startFreshDevice(), error => {
          if (first) return error === first;
          first = error;
          return /installed lifecycle stopped/u.test(String(error));
        });
        await assertTerminal(first);
        assert.equal(stopSettled, false);
        await writeFile(join(root, 'lifecycle.json.certificate-release'), 'released');
        assert.equal(await pending, first);
        await Promise.race([close.entered, stopping.then(() => { throw new Error('Stop settled before DELETE barrier'); })]);
        assert.equal(stopSettled, false);
        client.assertSessionIdentity('settings-1');
        assert.deepEqual(calls.slice(callCount).map(call => [call.method, call.requestPath]), [['DELETE', '/session/settings-1']]);
        close.release();
        await stopping;
      } finally {
        stopping ||= platform.stopOwnedResources();
        close.release();
        await writeFile(join(root, 'lifecycle.json.certificate-release'), 'released');
        await pending;
        await stopping;
      }
      assert.equal(stopSettled, true);
      assert.equal(platform.stopOwnedResources(), stopping);
      assert.equal(client.snapshot().sessionId, '');
      assert.equal(client.snapshot().firstFatal, undefined);
      assert.equal((platform as any).retainedOwner, undefined);
      assert.equal(platform.evidenceSnapshot().selectedInstalledWindowValid, false);
      const diagnostic = JSON.parse(await readFile(join(root, 'android-startup-logcat.json'), 'utf8'));
      assert.equal(diagnostic.error, 'unsupported ADB endpoint');
      assert.ok(diagnostic.endedAt);
      assert.deepEqual(calls.slice(callCount).map(call => [call.method, call.requestPath]), [['DELETE', '/session/settings-1']]);
      assert.equal(await readFile(log, 'utf8'), native + `CERTIFICATE_SETTLED ${variant.startsWith('scan-') ? 'scan' : 'push'}\nHTTP DELETE \n`);
      await assertTerminal(first);
      await assert.rejects(() => platform.startFreshDevice(), error => error === first);
      assert.equal(calls.filter(call => call.method === 'DELETE').length, 1);
    });
    console.log(`PASS SM56 certificate admission ${variant}`);
  }
}

async function runAndroidCreationTeardownControl(fixture: LifecycleFixtureInput, variant: string): Promise<void> {
  const cold = variant === 'cold-replacement-post';
  const chrome = cold || variant === 'setup-chrome-post';
  const beforeSettings = variant === 'setup-before-settings';
  const timeout = variant === 'setup-settings-timeout';
  await withAndroidRetainedLifecycleFixture({ ...fixture, freshSetup: !cold }, async ({ platform, client, calls, state, change, hold, assertTerminal, operations }) => {
    if (cold) await platform.terminateInstalledApp();
    await change({ fault: beforeSettings || timeout ? variant : '' });
    let stopPromise: Promise<void> | undefined;
    let stopping: Promise<void> | undefined;
    let stopSettled = false;
    let first: unknown;
    const expectedDeletes = beforeSettings || timeout ? []
      : chrome ? [`/session/${cold ? 'original' : 'settings-1'}`, '/session/cold-2'] : ['/session/settings-1'];
    const stop = () => {
      stopPromise = platform.stopOwnedResources();
      stopping = stopPromise.then(() => {
        stopSettled = true;
        assert.equal(client.snapshot().sessionId, '');
        assert.equal(client.snapshot().unusable, timeout);
        assert.deepEqual(calls.filter(call => call.method === 'DELETE').map(call => call.requestPath), expectedDeletes);
        assert.equal((platform as any).retainedOwner, undefined);
        assert.equal(platform.evidenceSnapshot().selectedInstalledWindowValid, false);
      });
      void stopping.catch(() => undefined);
    };
    const gate = hold(chrome ? 'chrome-create' : 'settings-create', variant === 'setup-synchronous-stop' ? stop : undefined);
    const pending = (cold ? platform.relaunchInstalledApp() : platform.startFreshDevice())
      .then(() => undefined, (error: unknown) => error);
    let callCount: number;
    let launches: number;
    let native: string;
    try {
      if (beforeSettings) {
        const deadline = Date.now() + 5_000;
        while (!existsSync(join(fixture.root, 'lifecycle.json.setup-wait')) && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 5));
        assert.ok(existsSync(join(fixture.root, 'lifecycle.json.setup-wait')));
      } else {
        await Promise.race([gate.entered, pending.then(() => { throw new Error('Creation settled before POST barrier'); })]);
      }
      assert.equal(client.snapshot().firstFatal, undefined);
      assert.equal(client.snapshot().sessionId, '');
      assert.equal((platform as any).retainedOwner, undefined);
      callCount = calls.length;
      launches = (await state()).launches;
      native = await readFile(fixture.log, 'utf8');
      assert.equal(calls.filter(call => call.path === '/session').length, beforeSettings ? 0 : chrome ? 2 : 1);
      assert.deepEqual(calls.filter(call => call.method === 'DELETE').map(call => call.requestPath), chrome ? [expectedDeletes[0]] : []);
      if (chrome && !cold) assert.ok(native.includes('CERTIFICATE_INSTALLED\n'));
      if (variant === 'setup-overlap') {
        await assert.rejects(() => platform.startFreshDevice(), error => { first = error; return /fresh setup requires an unused platform/u.test(String(error)); });
      }
      if (!stopPromise) stop();
      assert.equal(platform.stopOwnedResources(), stopPromise);
      assert.equal(platform.evidenceSnapshot().selectedInstalledWindowValid, false);
      await assert.rejects(() => platform.startFreshDevice(), error => {
        if (first) return error === first;
        first = error;
        return /installed lifecycle stopped/u.test(String(error));
      });
      await assertTerminal(first);
      assert.equal(stopSettled, false);
      assert.equal(calls.length, callCount);
    } finally {
      if (!stopPromise) stop();
      gate.release();
      await writeFile(join(fixture.root, 'lifecycle.json.setup-release'), 'released');
      const settled = await pending;
      await stopping;
      if (first) assert.equal(settled, first);
    }
    assert.equal(stopSettled, true);
    assert.equal(platform.stopOwnedResources(), stopPromise);
    await platform.stopOwnedResources();
    const fatal = client.snapshot().firstFatal;
    if (timeout) {
      assert.ok(fatal);
      assert.equal(fatal.code, 'APPIUM_TIMEOUT');
      assert.equal(fatal.path, '/session');
      assert.equal(fatal.method, 'POST');
      assert.notEqual(first, fatal);
    } else assert.equal(fatal, undefined);
    const receipt = structuredClone(fatal);
    await assertTerminal(first);
    await assert.rejects(() => platform.startFreshDevice(), error => error === first);
    await platform.stopOwnedResources();
    assert.equal(client.snapshot().firstFatal, fatal);
    assert.deepEqual(client.snapshot().firstFatal, receipt);
    assert.ok(calls.slice(callCount).every(call => call.method === 'DELETE'));
    const creations = calls.filter(call => call.path === '/session');
    assert.equal(creations.length, beforeSettings ? 0 : chrome ? 2 : 1);
    const receipts = client.snapshot().commands.filter(call => call.path === '/session' && call.method === 'POST');
    assert.equal(receipts.length, creations.length);
    for (const [index, receipt] of receipts.entries()) {
      assert.equal(receipt.timeoutMs, creations[index].body.capabilities.alwaysMatch.browserName === 'Chrome' ? 70_000 : 60_000);
      assert.equal(receipt.timedOut, timeout);
      if (!timeout) assert.ok(receipt.durationMs < receipt.timeoutMs);
    }
    assert.equal((await state()).launches, launches);
    assert.equal(await readFile(fixture.log, 'utf8'), native + (beforeSettings || timeout ? '' : 'HTTP DELETE \n'));
    assert.equal((await operations()).length, cold ? 1 : 0);
  });
}

async function runAndroidTeardownControls(fixture: LifecycleFixtureInput): Promise<void> {
  for (const variant of ['held-successful-delete', 'held-timeout-delete', 'held-resolved-fatal-delete', 'warm-inspection', 'termination-home', 'setup-settings-post', 'setup-chrome-post', 'setup-settings-timeout', 'setup-before-settings', 'setup-overlap', 'setup-synchronous-stop', 'cold-replacement-post']) {
    const root = await mkdtemp(join(fixture.root, 'teardown-'));
    const fixtureDirectory = join(root, 'fixture');
    const log = join(root, 'requests.log');
    await cp(fixture.fixtureDirectory, fixtureDirectory, { recursive: true });
    await mkdir(join(root, 'bin'));
    const isolated = { root, fixtureDirectory, log, environment: { ...fixture.environment, FAKE_ANDROID_FIXTURE_DIR: fixtureDirectory, FAKE_ANDROID_LOG: log } };
    if (variant.startsWith('setup-') || variant === 'cold-replacement-post') {
      await runAndroidCreationTeardownControl(isolated, variant);
      if (variant === 'setup-settings-post') await runAndroidCertificateTeardownControls(isolated);
      console.log(`PASS SM56 lifecycle concurrent-cold-consumers/${variant}`);
      continue;
    }
    await withAndroidRetainedLifecycleFixture(isolated, async ({ platform, client, calls, state, change, hold, assertTerminal, operations }) => {
      const owner = client.retainSessionOwner();
      const nativeOwner = (platform as any).retainedOwner;
      const kernel = (platform as any).kernelReader;
      const selected = platform.evidenceSnapshot().selectedInstalledWindow;
      const cold = variant.startsWith('held-');
      if (cold) await platform.terminateInstalledApp();
      await change({ fault: variant === 'held-timeout-delete' ? 'close-timeout'
        : variant === 'held-resolved-fatal-delete' ? 'close-404-resolves-with-firstFatal'
          : variant === 'termination-home' ? 'lifecycle-overlap-before-handoff-armed' : '' });
      const gate = hold(cold ? 'delete' : 'inspection-transport');
      const pending = (variant === 'termination-home' ? platform.terminateInstalledApp() : platform.relaunchInstalledApp())
        .then(() => undefined, (error: unknown) => error);
      let stopping: Promise<unknown> | undefined;
      let closeError: unknown;
      let stopPromise: Promise<void> | undefined;
      let first: unknown;
      let callCount: number;
      let launches: number;
      try {
        if (variant === 'termination-home') {
          gate.release();
          const deadline = Date.now() + 5_000;
          while (!existsSync(join(root, 'lifecycle.json.home-wait')) && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 5));
          assert.ok(existsSync(join(root, 'lifecycle.json.home-wait')));
        } else {
          await Promise.race([gate.entered, pending.then(() => { throw new Error('Lifecycle settled before teardown barrier'); })]);
        }
        assert.equal(client.snapshot().firstFatal, undefined);
        callCount = calls.length;
        launches = (await state()).launches;
        stopPromise = platform.stopOwnedResources();
        assert.equal(platform.stopOwnedResources(), stopPromise);
        let stopSettled = false;
        stopping = stopPromise.then(() => { stopSettled = true; }, (error: unknown) => { stopSettled = true; return error; });
        assert.equal(platform.evidenceSnapshot().selectedInstalledWindowValid, false);
        await assert.rejects(() => platform.readRunningIdentity(), error => { first = error; return /installed lifecycle stopped/u.test(String(error)); });
        await assertTerminal(first);
        assert.equal(stopSettled, false);
        assert.equal(calls.length, callCount);
      } finally {
        gate.release();
        await writeFile(join(root, 'lifecycle.json.home-release'), 'released');
        const settled = await pending;
        closeError = await stopping;
        if (first) assert.equal(settled, first);
      }
      assert.ok(stopPromise);
      assert.equal(platform.stopOwnedResources(), stopPromise);
      if (variant === 'held-timeout-delete') {
        assert.ok(closeError instanceof Error);
        await assert.rejects(() => platform.stopOwnedResources(), error => error === closeError);
      } else {
        assert.equal(closeError, undefined);
        await platform.stopOwnedResources();
      }
      const fatal = client.snapshot().firstFatal;
      if (variant === 'held-timeout-delete' || variant === 'held-resolved-fatal-delete') {
        assert.ok(fatal);
        assert.equal(fatal.code, variant === 'held-timeout-delete' ? 'APPIUM_TIMEOUT' : 'APPIUM_COMMAND');
        assert.equal(fatal.path, '/session/original');
        assert.equal(fatal.method, 'DELETE');
        assert.notEqual(first, closeError);
      } else assert.equal(fatal, undefined);
      if (variant === 'held-resolved-fatal-delete') {
        assert.equal(client.snapshot().sessionId, '');
        assert.equal(client.snapshot().unusable, false);
      }
      await assertTerminal(first);
      assert.equal(client.snapshot().firstFatal, fatal);
      assert.equal(calls.filter(call => call.method === 'DELETE').length, 1);
      assert.equal(calls.filter(call => call.path === '/session').length, 1);
      assert.ok(calls.slice(callCount).every(call => call.method === 'DELETE'));
      assert.equal((await state()).launches, launches);
      assert.equal(platform.evidenceSnapshot().selectedInstalledWindow, selected);
      assert.equal(platform.evidenceSnapshot().selectedInstalledWindowValid, false);
      assert.equal((platform as any).retainedOwner, nativeOwner);
      assert.equal((platform as any).kernelReader, kernel);
      assert.throws(owner);
      assert.equal((await operations()).length, variant === 'warm-inspection' ? 0 : 1);
    });
    console.log(`PASS SM56 lifecycle concurrent-cold-consumers/${variant}`);
  }
}

export async function runAndroidLifecycleCase(fixture: LifecycleFixtureInput, group: keyof typeof androidLifecycleCases, name: string): Promise<void> {
  if (name === 'concurrent-cold-consumers') await runAndroidTeardownControls(fixture);
  await withAndroidRetainedLifecycleFixture(fixture, async control => {
    const { platform, client, measurement, calls, state, change, operations, assertTerminal } = control;
    const beforeOwner = client.retainSessionOwner();
    const originalNativeOwner = (platform as any).retainedOwner;
    const originalKernelReader = (platform as any).kernelReader;
    const beforeWindow = platform.evidenceSnapshot().selectedInstalledWindow;
    const beforeProcess = (platform.evidenceSnapshot().events as any[]).filter(event => event.operation === 'chrome-process-observed').at(-1).detail;
    const deletes = () => calls.filter(call => call.method === 'DELETE').length;
    const creates = () => calls.filter(call => call.path === '/session').length;
    const capture = async (operation: () => Promise<unknown>) => {
      let first: unknown;
      await assert.rejects(operation, error => { first = error; return error instanceof Error; });
      return first;
    };
    if (group === 'happy') {
      if (name === 'measured-cold-then-warm') {
        await platform.terminateInstalledApp();
        const owned = await operations();
        assert.equal(owned.length, 1);
        assert.equal(owned[0].succeeded, true);
        assert.equal(owned[0].measurementId, measurement.id);
        assert.equal(owned[0].pid, '6538');
        assert.equal(owned[0].packageName, 'com.android.chrome');
        assert.equal(deletes(), 0);
        await platform.relaunchInstalledApp();
        assert.equal(deletes(), 1);
        assert.equal(creates(), 2);
        assert.throws(beforeOwner);
        assert.notEqual((platform as any).retainedOwner, originalNativeOwner);
        assert.notEqual((platform as any).kernelReader, originalKernelReader);
        const log = await readFile(fixture.log, 'utf8');
        const begin = log.indexOf(' OP_BEGIN ');
        const stop = log.indexOf('shell am force-stop --user 0 com.android.chrome');
        const removal = log.indexOf('shell ps -A -o PID,NAME', stop);
        const end = log.indexOf(' OP_END ');
        const home = log.indexOf('HOME_AFTER_STOP');
        const retirement = log.indexOf('HTTP DELETE');
        assert.ok(begin >= 0 && begin < stop && stop < removal && removal < end && end < home && home < retirement);
        assert.equal((await state()).startTime, '457');
      }
      if (name === 'same-owner-same-handle-navigation-between-activations') await change({ navigation: (await state()).navigation + 1 });
      const warmOwner = client.retainSessionOwner();
      await platform.relaunchInstalledApp();
      warmOwner();
      if (name === 'initial-then-final-warm-then-background-warm') {
        await platform.backgroundApp();
        assert.equal((await state()).foreground, 'launcher');
        const inspections = calls.filter(call => call.body.script === 'mobile: inspectRetainedChromeTargets').length;
        await platform.relaunchInstalledApp();
        assert.equal((await state()).foreground, 'installed');
        assert.equal(calls.filter(call => call.body.script === 'mobile: inspectRetainedChromeTargets').length, inspections + 1);
        const log = await readFile(fixture.log, 'utf8');
        const home = log.lastIndexOf('HOME_BACKGROUND');
        const handle = log.indexOf('HTTP GET /window', home);
        const signed = log.indexOf('SIGNED ', home);
        const devtools = log.indexOf('shell cat /proc/net/unix', signed);
        const strict = log.indexOf('INSPECT installed', home);
        assert.ok(home < handle && handle < signed && signed < devtools && devtools < strict);
        assert.equal(log.includes('INSPECT launcher'), false);
        beforeOwner();
        const process = (platform.evidenceSnapshot().events as any[]).filter(event => event.operation === 'chrome-process-observed').at(-1).detail;
        assert.equal(process.pid, beforeProcess.pid);
        assert.equal(process.startTime, beforeProcess.startTime);
      }
      assert.equal(platform.evidenceSnapshot().selectedInstalledWindow, beforeWindow);
      assert.equal(deletes(), name === 'measured-cold-then-warm' ? 1 : 0);
      assert.equal(creates(), name === 'measured-cold-then-warm' ? 2 : 1);
      assert.equal(calls.filter(call => call.path === '/window' && call.method === 'POST').length, creates());
      assert.ok(calls.filter(call => call.path === '/window' && call.method === 'POST').every(call => call.body.handle === beforeWindow));
      assert.equal((await operations()).length, name === 'measured-cold-then-warm' ? 1 : 0);
      await measurement.finish();
      return;
    }
    let first: unknown;
    if (group === 'warmOwner') {
      if (name === 'missing-owner') {
        const unowned = new AndroidPlatform({ origin: 'https://fixture.test', appiumUrl: 'http://retained.invalid', outputDir: fixture.root, certificate: '', setupUrl: '', deviceId: 'emulator-5554' });
        first = await capture(() => unowned.relaunchInstalledApp());
        await assert.rejects(() => unowned.readRunningIdentity(), error => error === first);
        assert.equal(deletes(), 0);
        assert.equal(creates(), 1);
        return;
      }
      if (['changed-native-pid', 'changed-native-start', 'changed-boot', 'missing-selected-handle', 'replaced-selected-handle'].includes(name)) {
        await platform.backgroundApp();
        assert.equal((await state()).foreground, 'launcher');
      }
      if (name === 'changed-driver-session') await client.create({ capabilities: {} });
      else await change({ fault: name });
      const inspections = calls.filter(call => call.body.script === 'mobile: inspectRetainedChromeTargets').length;
      first = await capture(() => name === 'document-change-within-read' ? platform.readRunningIdentity() : platform.relaunchInstalledApp());
      if ((await state()).foreground === 'launcher') {
        assert.equal((await state()).launches, 1);
        assert.equal(calls.filter(call => call.body.script === 'mobile: inspectRetainedChromeTargets').length, inspections);
        assert.equal(calls.filter(call => call.path === '/window' && call.method === 'POST').length, 1);
      }
    } else if (group === 'coldHandoff') {
      const beforeTermination = ['no-measurement-termination', 'termination-command-failure', 'termination-removal-readback-failure', 'home-failure-after-successful-stop', 'prior-platform-fatal', 'prior-driver-fatal-after-cleanup'];
      if (beforeTermination.includes(name)) {
        if (name === 'no-measurement-termination') {
          await writeFile(join(fixture.fixtureDirectory, 'chrome-stopped'), 'unowned stop');
          first = await capture(() => platform.relaunchInstalledApp());
        } else if (name === 'prior-platform-fatal') {
          await change({ fault: 'wrong-provider' });
          first = await capture(() => platform.readRunningIdentity());
          await change({ fault: '' });
          assert.equal(client.snapshot().firstFatal, undefined);
          await platform.stopOwnedResources();
          await platform.stopOwnedResources();
          assert.equal(deletes(), 1);
          assert.equal(client.snapshot().firstFatal, undefined);
        } else if (name === 'prior-driver-fatal-after-cleanup') {
          await change({ fault: name });
          first = await capture(() => client.execute('return true;'));
          const fatal = client.snapshot().firstFatal;
          assert.ok(fatal);
          assert.equal(fatal.code, 'APPIUM_TIMEOUT');
          assert.equal(fatal.path, '/session/original/execute/sync');
          assert.equal(fatal.method, 'POST');
          assert.match(String(first), /ANDROID_CONTEXT_OWNERSHIP/u);
          const receipt = structuredClone(fatal);
          await assertTerminal(first);
          await platform.stopOwnedResources();
          await platform.stopOwnedResources();
          assert.equal(deletes(), 1);
          assert.equal(client.snapshot().sessionId, '');
          assert.equal(client.snapshot().unusable, false);
          assert.equal(client.snapshot().firstFatal, fatal);
          assert.deepEqual(client.snapshot().firstFatal, receipt);
          await assertTerminal(first);
          assert.throws(beforeOwner);
        } else {
          await change({ fault: name });
          if (name !== 'home-failure-after-successful-stop') await writeFile(join(fixture.fixtureDirectory, 'valid-vending.json'), JSON.stringify({ absent: true,
            ...(name === 'termination-command-failure' ? { forceStopFail: true } : { remainingChildren: { '6700': 'com.android.chrome:renderer' } }),
          }));
          first = await capture(() => platform.terminateInstalledApp());
          const planned = await operations();
          assert.equal(planned.length, 1);
          assert.equal(planned[0].succeeded, name === 'home-failure-after-successful-stop');
          assert.equal(deletes(), 0);
        }
      } else {
        await platform.terminateInstalledApp();
        if (name === 'changed-measurement-id') Object.defineProperty(measurement, 'id', { value: 'different-measurement' });
        if (name === 'replaced-old-session') await client.create({ capabilities: {} });
        if (name === 'changed-owner') {
          await client.close();
          first = await capture(() => platform.relaunchInstalledApp());
        } else if (name === 'reused-consumed-handoff') {
          await platform.relaunchInstalledApp();
          await measurement.terminate('com.android.chrome', '6538');
          first = await capture(() => platform.relaunchInstalledApp());
          assert.equal(deletes(), 1);
          assert.equal(creates(), 2);
        } else if (name === 'intervening-incompatible-operation') {
          first = await capture(() => platform.backgroundApp());
        } else if (name === 'concurrent-cold-consumers' || name === 'changed-measurement-object') {
          const gate = control.hold('delete');
          const pending = capture(() => platform.relaunchInstalledApp());
          try {
            await Promise.race([gate.entered, pending.then(() => { throw new Error('Cold lifecycle settled before DELETE barrier'); })]);
            if (name === 'changed-measurement-object') {
              platform.environmentMeasurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
            } else first = await capture(() => platform.relaunchInstalledApp());
          } finally {
            gate.release();
            const settled = await pending;
            if (name === 'changed-measurement-object') first = settled;
            else assert.equal(settled, first);
          }
          assert.equal(platform.evidenceSnapshot().selectedInstalledWindow, beforeWindow);
          assert.equal(deletes(), 1);
          assert.equal(creates(), 1);
        } else {
          await change({ fault: name });
          first = await capture(() => platform.relaunchInstalledApp());
          if (name.startsWith('close-')) {
            assert.ok(client.snapshot().firstFatal);
            if (name !== 'close-transport-failure') {
              assert.equal(client.snapshot().sessionId, '');
              assert.equal(client.snapshot().unusable, false);
            }
            assert.equal(platform.evidenceSnapshot().selectedInstalledWindow, beforeWindow);
            assert.equal(deletes(), 1);
            assert.equal(creates(), 1);
            assert.equal((await state()).launches, 1);
          }
          if (name.startsWith('create-')) {
            assert.equal(deletes(), 1);
            assert.equal(creates(), 2);
            assert.equal((await state()).launches, 2);
          }
        }
      }
    } else {
      const cold = name.startsWith('cold-') || name.includes('cold-close') || name.includes('cold-create');
      if (cold) await platform.terminateInstalledApp();
      if (name === 'insufficient-shortcut-intent-tail') await control.remaining(100_000);
      if (name === 'insufficient-cold-close-tail') await control.remaining(200_000);
      await change({ fault: name });
      if (name === 'late-transport-success-after-fatal' || name === 'late-body-success-after-fatal') {
        const gate = control.hold(name === 'late-transport-success-after-fatal' ? 'inspection-transport' : 'inspection-body');
        const pending = capture(() => platform.relaunchInstalledApp());
        try {
          await Promise.race([gate.entered, pending.then(() => { throw new Error('Warm lifecycle settled before inspection barrier'); })]);
          first = await capture(() => platform.terminateInstalledApp());
        } finally {
          gate.release();
          assert.equal(await pending, first);
        }
      } else if (name === 'lifecycle-overlap-before-handoff-armed') {
        const pending = capture(() => platform.terminateInstalledApp());
        void pending.catch(() => undefined);
        try {
          const deadline = Date.now() + 5_000;
          while (!existsSync(join(fixture.root, 'lifecycle.json.home-wait')) && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 5));
          assert.ok(existsSync(join(fixture.root, 'lifecycle.json.home-wait')));
          first = await capture(() => platform.relaunchInstalledApp());
        } finally {
          await writeFile(join(fixture.root, 'lifecycle.json.home-release'), 'released');
          assert.equal(await pending, first);
        }
        assert.equal((await operations())[0].succeeded, true);
        assert.equal(deletes(), 0);
      } else {
        first = await capture(() => name === 'insufficient-read-postinspection-reserve' ? platform.readRunningIdentity() : platform.relaunchInstalledApp());
      }
      assert.equal(creates(), 1);
      assert.equal(deletes(), cold && name !== 'insufficient-cold-close-tail' ? 1 : 0);
      const dispatched = ['warm-foreground-not-ready', 'warm-devtools-not-ready', 'cold-foreground-not-ready', 'cold-devtools-not-ready', 'insufficient-warm-attachment-tail', 'insufficient-cold-create-settings-owner-attachment-tail', 'late-readiness-success', 'late-transport-success-after-fatal', 'late-body-success-after-fatal'].includes(name);
      assert.equal((await state()).launches, dispatched ? 2 : 1);
    }
    await assertTerminal(first);
    if (group === 'warmOwner') {
      assert.equal((await state()).launches, ['changed-kernel-capability', 'wrong-activity', 'wrong-provider', 'wrong-origin'].includes(name) ? 2 : 1);
      if (name === 'wrong-activity') {
        assert.equal((await state()).foreground, 'browser');
        assert.match(await readFile(fixture.log, 'utf8'), /INSPECT browser/u);
      }
      assert.equal(deletes(), 0);
      assert.equal(creates(), name === 'changed-driver-session' ? 2 : 1);
      assert.deepEqual(await operations(), []);
    }
    if (group === 'coldHandoff') {
      const retired = ['changed-measurement-object', 'changed-target', 'changed-owner', 'prior-platform-fatal', 'prior-driver-fatal-after-cleanup', 'reused-consumed-handoff', 'concurrent-cold-consumers', 'close-transport-failure', 'close-404-resolves-with-firstFatal', 'close-malformed-2xx-resolves-with-firstFatal', 'create-refusal', 'create-timeout'].includes(name);
      const created = ['replaced-old-session', 'reused-consumed-handoff', 'create-refusal', 'create-timeout'].includes(name);
      const launched = ['reused-consumed-handoff', 'create-refusal', 'create-timeout'].includes(name);
      assert.equal(deletes(), retired ? 1 : 0);
      assert.equal(creates(), created ? 2 : 1);
      assert.equal((await state()).launches, launched ? 2 : 1);
    }
  });
}

if (import.meta.main) await runAndroidRetainedLaunchRegressions(process.argv.includes('--recorded-two-page') ? ['recorded-two-page'] : undefined);
