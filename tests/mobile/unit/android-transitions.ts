import assert from 'node:assert/strict';
import { chmod, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { AndroidPlatform } from '../platforms/android';
import { PhaseBudget } from '../support/budget';
import { AppiumClient, accessibility } from '../support/webdriver';
import recorded from './fixtures/android/recorded-transitions.json';

const { JSDOM } = createRequire(import.meta.url)('../../../frontend/node_modules/jsdom');
const launcherXML = await readFile(new URL('./fixtures/android/launcher-observed.xml', import.meta.url), 'utf8');
const hypotheticalChromeXML = await readFile(new URL('./fixtures/android/chrome-confirmation-hypothetical.xml', import.meta.url), 'utf8');
const launcherPackage = 'com.google.android.apps.nexuslauncher';
const chromeForeground = 'ResumedActivity: ActivityRecord{recorded u0 com.android.chrome/com.google.android.apps.chrome.Main t10}\n mCurrentFocus=Window{recorded u0 com.android.chrome/com.google.android.apps.chrome.Main}';
const settingsForeground = 'ResumedActivity: ActivityRecord{synthetic u0 com.android.settings/.Settings t9}\n mCurrentFocus=Window{synthetic u0 com.android.settings/com.android.settings.Settings}';
const syntheticPickerXML = '<hierarchy width="1080" height="2400"><android.widget.FrameLayout package="com.google.android.documentsui" bounds="[0,0][1080,2400]" enabled="true" displayed="true"><android.widget.ImageButton package="com.google.android.documentsui" content-desc="Open navigation" clickable="true" enabled="true" displayed="true" bounds="[0,136][140,276]"/></android.widget.FrameLayout></hierarchy>';
const response = (value: unknown, status = 200) => new Response(JSON.stringify({ value, sessionId: 'android-replay' }), { status });
const notFound = () => response({ error: 'no such element', message: 'no such element' }, 404);

interface ReplayOptions {
  xml?: string;
  foregrounds?: string[];
  budgetMs?: number;
  advanceMs?: number;
  sourceDelayMs?: number;
  lookupDelays?: number[];
  attributeOverrides?: Record<string, string | null>;
  hangPath?: RegExp;
  invalidLookup?: boolean;
  browserInstall?: boolean;
  sourceXMLs?: string[];
  chromeXML?: string;
  chromeRemains?: boolean;
  replaceIdentity?: boolean;
}

async function replay(options: ReplayOptions, body: (harness: any) => Promise<void>): Promise<void> {
  const root = await mkdtemp(join(process.env.ANDROID_TEST_OUTPUT || tmpdir(), 'android-transitions-'));
  const statePath = join(root, 'state.json');
  const adb = join(root, 'adb');
  await writeFile(statePath, JSON.stringify({ foregrounds: options.foregrounds || [recorded.launcher.foreground], count: 0, calls: [], clicked: false }));
  await writeFile(adb, `#!${process.execPath}\nconst file = ${JSON.stringify(statePath)};
const state = await Bun.file(file).json();
const args = process.argv.slice(2);
if (args.slice(0, 3).join(' ') !== '-s emulator-5554 shell') process.exit(2);
const cmd = args.slice(3).join(' ');
state.calls.push(cmd);
if (cmd === 'dumpsys activity activities') {
  console.log(state.foregrounds[Math.min(state.count++, state.foregrounds.length - 1)]);
} else if (cmd === 'cmd shortcut get-shortcuts --user 0 --flags 15 com.android.chrome') {
  if (state.clicked) console.log('ShortcutInfo {id=replay-id, flags=0x28a, shortLabel=Herdr Relay, org.chromium.chrome.browser.webapp_name=Herdr Mobile Relay, org.chromium.chrome.browser.webapp_url=https://fixture.test/, org.chromium.chrome.browser.webapp_scope=https://fixture.test/, org.chromium.chrome.browser.webapp_mac=replay-signed-mac}');
} else if (cmd !== 'input keyevent KEYCODE_HOME') process.exit(3);
await Bun.write(file, JSON.stringify(state));\n`);
  await chmod(adb, 0o700);
  const savedAdb = process.env.ADB;
  const realNow = Date.now;
  let offset = 0;
  Date.now = () => realNow() + offset;
  process.env.ADB = adb;
  const budget = new PhaseBudget('android-recorded-replay', { timeoutMs: options.budgetMs || 60_000, recoveryLimit: 0 });
  const requests: Array<{ path: string; body: any; at: number }> = [];
  const elements = new Map<string, any>();
  const chromeControl = (text: string) => text === 'Add' ? options.chromeXML ?? hypotheticalChromeXML : `<hierarchy><android.widget.Button class="android.widget.Button" package="com.android.chrome" text="${text}" content-desc="${text}" clickable="true" enabled="true" displayed="true" bounds="[0,136][400,276]"/></hierarchy>`;
  let xml = options.browserInstall ? chromeControl('More options') : options.xml ?? launcherXML;
  let active = 0;
  let maxActive = 0;
  let clicks = 0;
  let lookupIndex = 0;
  let sourceCount = 0;
  const pending: Promise<unknown>[] = [];
  const client = new AppiumClient('http://android-replay.invalid', 30_000, async (input, init) => {
    const path = new URL(String(input)).pathname.replace('/session/android-replay', '');
    const payload = init?.body ? JSON.parse(String(init.body)) : undefined;
    requests.push({ path, body: payload, at: realNow() });
    active++;
    maxActive = Math.max(maxActive, active);
    try {
      if (options.hangPath?.test(path)) {
        const stalled = new Promise<Response>((resolve) => {
          init?.signal?.addEventListener('abort', () => setTimeout(() => resolve(notFound()), 50), { once: true });
        });
        pending.push(stalled);
        return await stalled;
      }
      if (path === '/session') return response({});
      if (path === '/context') return response(null);
      if (path === '/source') {
        sourceCount++;
        if (options.sourceXMLs) xml = options.sourceXMLs[Math.min(sourceCount - 1, options.sourceXMLs.length - 1)];
        if (options.sourceDelayMs) {
          const waiting = new Promise(resolve => setTimeout(resolve, options.sourceDelayMs));
          pending.push(waiting);
          await waiting;
        }
        offset += options.advanceMs || 0;
        return response(xml);
      }
      if (path === '/window/rect') return response({ x: 0, y: 0, width: 1080, height: 2400 });
      if (path === '/element' || path === '/elements') {
        if (options.invalidLookup) return response({ error: 'invalid selector', message: 'invalid selector' }, 400);
        const milliseconds = options.lookupDelays?.[lookupIndex++] || 0;
        if (milliseconds) {
          const waiting = new Promise(resolve => setTimeout(resolve, milliseconds));
          pending.push(waiting);
          await waiting;
        }
        const document = new JSDOM(xml, { contentType: 'text/xml' }).window.document;
        let nodes: any[] = [];
        if (payload.using === 'xpath') {
          const result = document.evaluate(payload.value, document, null, 7, null);
          nodes = Array.from({ length: result.snapshotLength }, (_, index) => result.snapshotItem(index));
        } else if (payload.using === 'accessibility id') {
          nodes = [...document.querySelectorAll('*')].filter((node: any) => node.getAttribute('content-desc') === payload.value);
        } else {
          return response({ error: 'unsupported operation', message: `Unsupported locator ${payload.using}` }, 500);
        }
        const values = nodes.map((node: any) => {
          const existing = [...elements.entries()].find(([, previous]) => previous.outerHTML === node.outerHTML);
          const id = existing && !options.replaceIdentity ? existing[0] : `element-${elements.size}`;
          elements.set(id, node);
          return { 'element-6066-11e4-a52e-4f735466cecf': id };
        });
        return path === '/elements' ? response(values) : values.length ? response(values[0]) : notFound();
      }
      const match = path.match(/^\/element\/([^/]+)\/(attribute\/([^/]+)|rect|click)$/u);
      if (match) {
        const node = elements.get(match[1]);
        if (!node) return response({ error: 'stale element reference', message: 'unknown element' }, 404);
        if (match[3]) return response(options.attributeOverrides && match[3] in options.attributeOverrides ? options.attributeOverrides[match[3]] : node.getAttribute(match[3]));
        const bounds = [...(node.getAttribute('bounds') || '').matchAll(/-?\d+/gu)].map((item: RegExpMatchArray) => Number(item[0]));
        if (match[2] === 'rect') return response({ x: bounds[0], y: bounds[1], width: bounds[2] - bounds[0], height: bounds[3] - bounds[1] });
        assert.equal(node.getAttribute('enabled'), 'true');
        assert.equal(node.getAttribute('displayed'), 'true');
        assert.equal(node.getAttribute('clickable'), 'true');
        const label = node.getAttribute('text') || node.getAttribute('content-desc');
        const chromeSteps = ['More options', 'Install app', 'Add'];
        if (options.browserInstall && clicks < chromeSteps.length) {
          assert.equal(label, chromeSteps[clicks]);
          xml = clicks === 2 ? options.chromeRemains ? chromeControl('Add') : launcherXML : chromeControl(chromeSteps[clicks + 1]);
        } else {
          assert.ok(label === 'Add to home screen' || label === 'Open navigation');
        }
        clicks++;
        const state = JSON.parse(await readFile(statePath, 'utf8'));
        state.clicked = label === 'Add to home screen';
        await writeFile(statePath, JSON.stringify(state));
        return response(null);
      }
      return response({ error: 'unsupported operation', message: `Unsupported command ${path} ${payload?.script || ''}` }, 500);
    } finally {
      active--;
    }
  });
  const platform = new AndroidPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://android-replay.invalid', outputDir: root,
    certificate: '', setupUrl: '', deviceId: 'emulator-5554', budget,
  });
  (platform as any).driver = client;
  const originalForeground = (platform as any).foregroundEvidence.bind(platform);
  (platform as any).foregroundEvidence = async (...args: unknown[]) => {
    const result = await originalForeground(...args);
    offset += options.advanceMs || 0;
    return result;
  };
  try {
    await client.create({ capabilities: {}, budget });
    await body({
      platform, client, root, requests,
      setXML: (value: string) => { xml = value; },
      advance: (value: number) => { offset += value; },
      state: () => ({ clicks, active, maxActive, sourceCount }),
      adbState: async () => JSON.parse(await readFile(statePath, 'utf8')),
    });
  } finally {
    await Promise.all(pending);
    await writeFile(join(root, 'protocol-result.json'), JSON.stringify({ requests, driver: client.snapshot(), active, maxActive, clicks, sourceCount }, null, 2));
    assert.equal(active, 0);
    assert.equal(maxActive, 1);
    Date.now = realNow;
    if (savedAdb === undefined) delete process.env.ADB;
    else process.env.ADB = savedAdb;
  }
}

export const androidTransitionTests: Array<[string, () => Promise<void>]> = [];
const test = (name: string, body: () => Promise<void>) => androidTransitionTests.push([name, body]);

test('Android recorded class-tagged launcher transition uses one standard click and signed postcondition', async () => {
  await replay({ foregrounds: [chromeForeground, chromeForeground, recorded.launcher.foreground], advanceMs: 1_000 }, async ({ platform, client, state, requests, adbState }) => {
    assert.equal(await platform.confirmLauncherShortcut(), true);
    assert.equal(state().clicks, 1);
    assert.ok((await adbState()).count >= recorded.launcher.precedingChromeObservations + 1);
    assert.equal((await platform.waitForChromeShortcut(2_000)).mac, 'replay-signed-mac');
    assert.ok(requests.some((item: any) => /\/element\/[^/]+\/click$/u.test(item.path)));
    assert.equal(requests.filter((item: any) => item.path === '/execute/sync').length, 0);
    assert.equal(client.snapshot().unusable, false);
  });
});

for (const [name, transform] of [
  ['wrong hierarchy owner', (xml: string) => xml.replaceAll(launcherPackage, 'unrelated.package')],
  ['wrong preview', (xml: string) => xml.replaceAll('Herdr Relay', 'Unrelated')],
  ['title only', (xml: string) => xml.replaceAll('android.widget.Button', 'android.widget.TextView').replace('text="Add to home screen" checkable="false" checked="false" clickable="true"', 'text="Add to home screen" checkable="false" checked="false" clickable="false"')],
  ['unrelated install label', (xml: string) => xml.replaceAll('Add to home screen', 'Install')],
  ['non-clickable', (xml: string) => xml.replaceAll('clickable="true"', 'clickable="false"')],
  ['disabled', (xml: string) => xml.replaceAll('enabled="true"', 'enabled="false"')],
  ['hidden', (xml: string) => xml.replaceAll('displayed="true"', 'displayed="false"')],
  ['indeterminate', (xml: string) => xml.replaceAll('enabled="true"', '')],
  ['invalid bounds', (xml: string) => xml.replace('[637,2190][1051,2316]', '[1051,2190][637,2316]')],
  ['off-screen bounds', (xml: string) => xml.replace('[637,2190][1051,2316]', '[637,2390][1051,2516]')],
  ['absent confirmation', (_xml: string) => '<hierarchy/>'],
] as const) {
  test(`Android launcher rejects ${name}`, async () => {
    await replay({ xml: transform(launcherXML), advanceMs: 6_000 }, async ({ platform, state }) => {
      assert.equal(await platform.confirmLauncherShortcut(), false);
      assert.equal(state().clicks, 0);
    });
  });
}

for (const foreground of [chromeForeground, recorded.launcher.foreground.replace('mCurrentFocus=Window{', 'unfocused=Window{')]) {
  test('Android launcher rejects wrong activity or missing focus despite matching hierarchy', async () => {
    await replay({ foregrounds: [foreground], advanceMs: 6_000 }, async ({ platform, state }) => {
      assert.equal(await platform.confirmLauncherShortcut(), false);
      assert.equal(state().clicks, 0);
    });
  });
}

for (const value of [null, 'false', 'unknown']) {
  test(`Android launcher rechecks indeterminate/live-disabled state (${value})`, async () => {
    await replay({ attributeOverrides: { enabled: value }, advanceMs: 6_000 }, async ({ platform, state }) => {
      assert.equal(await platform.confirmLauncherShortcut(), false);
      assert.equal(state().clicks, 0);
    });
  });
}

test('Android launcher interrupted click stops all later actions and retains quarantine', async () => {
  await replay({ hangPath: /\/click$/u }, async ({ platform, client, requests }) => {
    await assert.rejects(() => platform.confirmLauncherShortcut(), /APPIUM_TIMEOUT/u);
    const failure = client.snapshot().firstFatal;
    const count = requests.length;
    await assert.rejects(() => client.pageSource(), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(requests.length, count);
    assert.deepEqual(client.snapshot().firstFatal, failure);
  });
});

test('Android picker waits for recorded native transition and synthetic usable UI before drawer lookup', async () => {
  assert.equal(recorded.picker.readyHierarchyRecorded, false);
  await replay({ xml: syntheticPickerXML, foregrounds: [settingsForeground, recorded.picker.foreground], sourceDelayMs: recorded.picker.delayedResponseReplayMs }, async ({ platform, client, state, requests, root }) => {
    await platform.openCertificatePicker();
    assert.equal(state().clicks, 1);
    const source = requests.findIndex((item: any) => item.path === '/source');
    const drawer = requests.findIndex((item: any) => item.body?.using === 'accessibility id');
    assert.ok(source >= 0 && drawer > source);
    assert.ok(requests[drawer].at - requests[source].at >= recorded.picker.delayedResponseReplayMs);
    assert.equal(await readFile(join(root, 'certificate-picker-ready-hierarchy.xml'), 'utf8'), syntheticPickerXML);
    assert.equal(client.snapshot().unusable, false);
  });
});

test('Android native locator transactions do not inherit the preceding not-found residual budget', async () => {
  await replay({ xml: syntheticPickerXML, lookupDelays: [recorded.picker.firstNotFoundMs, 4_900] }, async ({ platform, client, state }) => {
    const locators = ['Show roots', 'Open navigation drawer', 'Open navigation'].map(accessibility);
    await platform.clickNative(locators, 'certificate picker navigation');
    assert.equal(state().clicks, 1);
    assert.equal(client.snapshot().unusable, false);
    const lookups = client.snapshot().lookups;
    assert.deepEqual(lookups.map((lookup: any) => lookup.outcome), ['retryable', 'retryable', 'matched']);
    assert.ok(lookups.every((lookup: any) => lookup.sliceMs >= 4_995));
  });
});

for (const [name, foreground, xml] of [
  ['wrong foreground', settingsForeground, syntheticPickerXML],
  ['unfocused picker', recorded.picker.foreground.replace('mCurrentFocus=Window{', 'unfocused=Window{'), syntheticPickerXML],
  ['splash-only UI', recorded.picker.foreground, '<hierarchy><android.widget.FrameLayout package="com.google.android.documentsui" enabled="true" displayed="true"/></hierarchy>'],
  ['foreign UI', recorded.picker.foreground, syntheticPickerXML.replaceAll('com.google.android.documentsui', 'com.android.settings')],
  ['hidden UI', recorded.picker.foreground, syntheticPickerXML.replaceAll('displayed="true"', 'displayed="false"')],
] as const) {
  test(`Android picker does not query drawer with ${name}`, async () => {
    await replay({ xml, foregrounds: [foreground], advanceMs: 6_000 }, async ({ platform, requests, state }) => {
      await assert.rejects(() => platform.openCertificatePicker(), /ANDROID_CERTIFICATE/u);
      assert.equal(requests.some((item: any) => item.body?.using === 'accessibility id'), false);
      assert.equal(state().clicks, 0);
    });
  });
}

test('Android native lookup refuses a partial transaction and preserves the prior ordinary failure', async () => {
  await replay({ xml: '<hierarchy/>', budgetMs: 6_000, lookupDelays: [1_200] }, async ({ platform, client, requests }) => {
    await assert.rejects(() => platform.findNative([accessibility('Show roots'), accessibility('Open navigation drawer')], 6_000), /no such element/u);
    assert.equal(requests.filter((item: any) => item.path === '/element').length, 1);
    assert.equal(client.snapshot().unusable, false);
  });
});

test('Android picker refuses readiness without a complete operation allowance', async () => {
  await replay({ xml: syntheticPickerXML, budgetMs: 500 }, async ({ platform, requests, state }) => {
    await assert.rejects(() => platform.openCertificatePicker(), /ANDROID_CERTIFICATE/u);
    assert.equal(requests.length, 1);
    assert.equal(state().clicks, 0);
  });
});

test('Android picker genuine hung hierarchy retains first failure and never navigates', async () => {
  await replay({ foregrounds: [recorded.picker.foreground], hangPath: /\/source$/u }, async ({ platform, client, requests }) => {
    await assert.rejects(() => platform.openCertificatePicker(), /APPIUM_TIMEOUT/u);
    const failure = client.snapshot().firstFatal;
    const count = requests.length;
    await assert.rejects(() => client.pageSource(), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(requests.length, count);
    assert.deepEqual(client.snapshot().firstFatal, failure);
    assert.equal(requests.some((item: any) => item.body?.using === 'accessibility id'), false);
  });
});

for (const [name, overrides] of [
  ['wrong class', { class: 'android.widget.TextView' }],
  ['wrong owner', { package: 'unrelated.package' }],
  ['hidden', { displayed: 'false' }],
  ['disabled', { enabled: 'false' }],
  ['not clickable', { clickable: 'false' }],
  ['missing readiness', { enabled: null }],
] as const) {
  test(`Android hypothetical Chrome confirmation rejects ${name} before the sole click`, async () => {
    await replay({ browserInstall: true, foregrounds: [chromeForeground], attributeOverrides: overrides }, async ({ platform, state, root }) => {
      await assert.rejects(() => platform.installFromBrowser(), /selected confirmation control is not a ready Chrome button/u);
      assert.equal(state().clicks, 2);
      assert.equal(await readFile(join(root, 'android-chrome-before-confirmation.xml'), 'utf8'), hypotheticalChromeXML);
    });
  });
}

for (const replaceIdentity of [false, true]) {
  test(`Android hypothetical Chrome confirmation rejects ${replaceIdentity ? 'replaced' : 'ambiguous'} identity`, async () => {
    const chromeXML = replaceIdentity ? hypotheticalChromeXML : hypotheticalChromeXML.replace('text="Cancel"', 'text="Install"');
    await replay({ browserInstall: true, foregrounds: [chromeForeground], chromeXML, replaceIdentity }, async ({ platform, state }) => {
      await assert.rejects(() => platform.installFromBrowser(), /ambiguous or replaced/u);
      assert.equal(state().clicks, 2);
    });
  });
}

test('Android hypothetical Chrome confirmation refuses an incomplete whole operation', async () => {
  await replay({ browserInstall: true, budgetMs: 44_999 }, async ({ platform, state, requests }) => {
    await assert.rejects(() => platform.installFromBrowser(), /insufficient confirmation observation allowance/u);
    assert.equal(state().clicks, 2);
    assert.equal(requests.some((request: any) => request.path === '/source'), false);
  });
});

test('Android hypothetical Chrome confirmation does not issue a doomed observation after phase-tail consumption', async () => {
  await replay({ browserInstall: true, foregrounds: [chromeForeground], advanceMs: 10_001 }, async ({ platform, state, requests }) => {
    await assert.rejects(() => platform.installFromBrowser(), /insufficient confirmation observation allowance/u);
    assert.equal(state().clicks, 2);
    assert.equal(requests.at(-1).path, '/source');
  });
});

test('Android hypothetical Chrome confirmation hung hierarchy quarantines without a click or post-observation', async () => {
  await replay({ browserInstall: true, hangPath: /\/source$/u }, async ({ platform, client, state, requests }) => {
    await assert.rejects(() => platform.installFromBrowser(), /APPIUM_TIMEOUT/u);
    assert.equal(state().clicks, 2);
    assert.equal(requests.at(-1).path, '/source');
    assert.equal(client.snapshot().unusable, true);
  });
});

for (const [name, chromeXML] of [
  ['off-screen', hypotheticalChromeXML.replace('[600,1250][950,1400]', '[600,2300][950,2500]')],
  ['missing bounds', hypotheticalChromeXML.replace('bounds="[600,1250][950,1400]"', '')],
] as const) {
  test(`Android hypothetical Chrome confirmation rejects ${name} bounds`, async () => {
    await replay({ browserInstall: true, foregrounds: [chromeForeground], chromeXML }, async ({ platform, state }) => {
      await assert.rejects(() => platform.installFromBrowser(), /selected confirmation control is not a ready Chrome button/u);
      assert.equal(state().clicks, 2);
    });
  });
}

test('Android hypothetical Chrome late click cannot authorize post-observation or another action', async () => {
  await replay({ browserInstall: true, foregrounds: [chromeForeground], hangPath: /\/element\/element-2\/click$/u }, async ({ platform, client, requests, root }) => {
    await assert.rejects(() => platform.installFromBrowser(), /APPIUM_TIMEOUT/u);
    assert.equal(requests.at(-1).path, '/element/element-2/click');
    assert.equal(requests.filter((request: any) => request.path === '/source').length, 1);
    await assert.rejects(() => readFile(join(root, 'android-chrome-after-confirmation.xml')), /ENOENT/u);
    assert.equal(client.snapshot().unusable, true);
  });
});

test('Android hypothetical Chrome remaining dialog is captured but never retried or accepted as installation', async () => {
  await replay({ browserInstall: true, chromeRemains: true, foregrounds: [chromeForeground] }, async ({ platform, state, root }) => {
    await assert.rejects(() => platform.installFromBrowser(), /ANDROID_LAUNCHER/u);
    assert.equal(state().clicks, 3);
    assert.equal(await readFile(join(root, 'android-chrome-after-confirmation.xml'), 'utf8'), hypotheticalChromeXML);
  });
});

test('Android browser installation requires the scoped launcher click before the signed postcondition and HOME', async () => {
  await replay({ browserInstall: true, foregrounds: [chromeForeground, chromeForeground, recorded.launcher.foreground] }, async ({ platform, state, adbState }) => {
    await platform.installFromBrowser();
    assert.equal(state().clicks, 4);
    const { calls } = await adbState();
    assert.match(calls.at(-2), /^cmd shortcut get-shortcuts/u);
    assert.equal(calls.at(-1), 'input keyevent KEYCODE_HOME');
  });
});

test('Android picker waits past an initialized window whose UI is not usable yet', async () => {
  await replay({ foregrounds: [recorded.picker.foreground], sourceXMLs: ['<hierarchy/>', syntheticPickerXML] }, async ({ platform, requests, state }) => {
    await platform.openCertificatePicker();
    assert.equal(state().sourceCount, 2);
    const drawerIndex = requests.findIndex((item: any) => item.body?.using === 'accessibility id');
    assert.equal(requests.slice(0, drawerIndex).filter((item: any) => item.path === '/source').length, 2);
  });
});

test('Android hung native lookup stops later selectors, scrolling, diagnostics and keeps first failure', async () => {
  await replay({ hangPath: /^\/element$/u }, async ({ platform, client, requests }) => {
    await assert.rejects(() => platform.clickNative([accessibility('Show roots'), accessibility('Open navigation drawer')], 'picker navigation'), /APPIUM_TIMEOUT/u);
    const first = client.snapshot().firstFatal;
    assert.equal(requests.filter((item: any) => item.path === '/element').length, 1);
    const count = requests.length;
    await assert.rejects(() => client.screenshot(), /APPIUM_SESSION_UNUSABLE/u);
    assert.equal(requests.length, count);
    assert.deepEqual(client.snapshot().firstFatal, first);
  });
});

test('Android native invalid-command lookup does not scroll or try another selector', async () => {
  await replay({ invalidLookup: true }, async ({ platform, requests }) => {
    await assert.rejects(() => platform.clickNative([accessibility('first'), accessibility('second')], 'invalid picker selector'), /invalid selector/u);
    assert.equal(requests.filter((item: any) => item.path === '/element').length, 1);
    assert.equal(requests.some((item: any) => item.path === '/execute/sync'), false);
  });
});

if (import.meta.main) {
  let failures = 0;
  const selected = androidTransitionTests.filter(([name]) => !process.env.ANDROID_TEST_FILTER || name.includes(process.env.ANDROID_TEST_FILTER));
  for (const [name, body] of selected) {
    try {
      await body();
      console.log(`PASS ${name}`);
    } catch (error) {
      failures++;
      console.error(`FAIL ${name}`, error);
    }
  }
  console.log(`${selected.length - failures} passed; ${failures} failed. Host protocol replays are not native qualification. Picker-ready UI is synthetic, not recorded.`);
  process.exitCode = failures ? 1 : 0;
}
