import { X509Certificate } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { basename, join } from 'node:path';
import { assertStandalone, type RuntimeIdentity } from '../support/oracle';
import { DiagnosticRecorder, writeBoundedText, writeSanitizedJson } from '../support/diagnostics';
import { PhaseBudget } from '../support/budget';
import { command, commandOutput } from '../support/process';
import { requireOwnedDevice } from '../support/device';
import {
  accessibility,
  accessibilityPrefix,
  AppiumClient,
  buttonText,
  css,
  delay,
  textLocator,
  type Locator,
} from '../support/webdriver';
import { runtimeScript, updateCompletionScript, type MobilePlatform, type PlatformOptions, type UpdateCompletionEvidence } from './types';

function androidShellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\"'\"'")}'`;
}

const CHROME_WEBAPP_ACTION = 'com.google.android.apps.chrome.webapps.WebappManager.ACTION_START_WEBAPP';
const CHROME_WEBAPP_COMPONENT = 'com.android.chrome/org.chromium.chrome.browser.webapps.WebappLauncherActivity';
const CHROME_WEBAPP_ID = 'org.chromium.chrome.browser.webapp_id';
const CHROME_WEBAPP_URL = 'org.chromium.chrome.browser.webapp_url';
const CHROME_WEBAPP_SCOPE = 'org.chromium.chrome.browser.webapp_scope';
const CHROME_WEBAPP_NAME = 'org.chromium.chrome.browser.webapp_name';
const CHROME_WEBAPP_SHORT_NAME = 'org.chromium.chrome.browser.webapp_short_name';
const CHROME_WEBAPP_MAC = 'org.chromium.chrome.browser.webapp_mac';
const CHROME_WEBAPP_SOURCE = 'org.chromium.chrome.browser.webapp_source';
const CHROME_WEBAPP_DISPLAY_MODE = 'org.chromium.chrome.browser.webapp_display_mode';
const CHROME_WEBAPP_ORIENTATION = 'org.chromium.content_public.common.orientation';

export interface AndroidChromeShortcut {
  id: string;
  shortLabel: string;
  name: string;
  url: string;
  scope: string;
  mac: string;
  source?: string;
  displayMode?: string;
  orientation?: string;
}

function shortcutField(block: string, name: string): string | undefined {
  const escaped = name.replace(/[.*+?^${}()|[\]\\]/gu, '\\$&');
  const match = block.match(new RegExp(`(?:^|[,{[]|\\s)${escaped}=([^,}\\]\\r\\n]+)`, 'u'));
  return match?.[1]?.trim();
}

export function parseAndroidChromeShortcuts(output: string): AndroidChromeShortcut[] {
  return output.split(/(?=^ShortcutInfo \{)/mu).flatMap((block) => {
    const id = shortcutField(block, 'id');
    const shortLabel = shortcutField(block, 'shortLabel');
    const name = shortcutField(block, CHROME_WEBAPP_NAME);
    const url = shortcutField(block, CHROME_WEBAPP_URL);
    const scope = shortcutField(block, CHROME_WEBAPP_SCOPE);
    const mac = shortcutField(block, CHROME_WEBAPP_MAC);
    if (!id || !shortLabel || !name || !url || !scope || !mac) return [];
    return [{
      id,
      shortLabel,
      name,
      url,
      scope,
      mac,
      source: shortcutField(block, CHROME_WEBAPP_SOURCE),
      displayMode: shortcutField(block, CHROME_WEBAPP_DISPLAY_MODE),
      orientation: shortcutField(block, CHROME_WEBAPP_ORIENTATION),
    }];
  });
}

export function androidOpenUrlArgs(serial: string, url: string): string[] {
  const remoteCommand = ['am', 'start', '-a', 'android.intent.action.VIEW', '-d', url, 'com.android.chrome']
    .map(androidShellQuote)
    .join(' ');
  return ['-s', serial, 'shell', remoteCommand];
}

export function hasAndroidChromeDevToolsSocket(output: string): boolean {
  return /(?:^|\s)@?chrome_devtools_remote(?:_\d+)?(?:\s|$)/u.test(output);
}

export function androidChromeCapabilities(serial: string, attachToRunningApp = false): Record<string, unknown> {
  const chromeOptions: Record<string, unknown> = { androidPackage: 'com.android.chrome' };
  if (attachToRunningApp) chromeOptions.androidUseRunningApp = true;
  return {
    platformName: 'Android',
    browserName: 'Chrome',
    'appium:automationName': 'UiAutomator2',
    'appium:udid': serial,
    'appium:noReset': true,
    'appium:fullReset': false,
    'appium:newCommandTimeout': 1_200,
    'appium:skipDeviceInitialization': false,
    'appium:skipServerInstallation': false,
    'goog:chromeOptions': chromeOptions,
  };
}

export function androidChromeShortcutArgs(serial: string, shortcut: AndroidChromeShortcut): string[] {
  const args = [
    'am', 'start', '-W', '--user', '0',
    '-a', CHROME_WEBAPP_ACTION,
    '-n', CHROME_WEBAPP_COMPONENT,
    '--es', CHROME_WEBAPP_ID, shortcut.id,
    '--es', CHROME_WEBAPP_URL, shortcut.url,
    '--es', CHROME_WEBAPP_SCOPE, shortcut.scope,
    '--es', CHROME_WEBAPP_NAME, shortcut.name,
    '--es', CHROME_WEBAPP_SHORT_NAME, shortcut.shortLabel,
    '--es', CHROME_WEBAPP_MAC, shortcut.mac,
  ];
  if (shortcut.source !== undefined) args.push('--ei', CHROME_WEBAPP_SOURCE, shortcut.source);
  if (shortcut.displayMode !== undefined) args.push('--ei', CHROME_WEBAPP_DISPLAY_MODE, shortcut.displayMode);
  if (shortcut.orientation !== undefined) args.push('--ei', CHROME_WEBAPP_ORIENTATION, shortcut.orientation);
  const remoteCommand = args.map(androidShellQuote).join(' ');
  return ['-s', serial, 'shell', remoteCommand];
}

export class AndroidPlatform implements MobilePlatform {
  readonly name = 'android' as const;
  readonly driver: AppiumClient;
  private readonly serial: string;
  private readonly origin: string;
  private readonly outputDir: string;
  private readonly budget: PhaseBudget;
  private readonly diagnostics: DiagnosticRecorder;
  private installedPackage = '';
  private installedTarget?: { packageName: string; activity: string; shortcut: AndroidChromeShortcut };
  private lastIdentity?: RuntimeIdentity;
  private lastUrl = '';
  private keyboardDraft = '';
  private lastCompletion?: UpdateCompletionEvidence;
  private lastForeground?: { packageName: string; activity: string; pid: string };

  constructor(private readonly options: PlatformOptions) {
    this.serial = options.deviceId || process.env.ANDROID_SERIAL || '';
    this.origin = options.origin.replace(/\/$/, '');
    this.outputDir = options.outputDir;
    this.budget = options.budget || new PhaseBudget('android-run', { timeoutMs: 30 * 60_000, recoveryLimit: 4 });
    this.diagnostics = options.diagnostics || new DiagnosticRecorder();
    this.driver = new AppiumClient(options.appiumUrl);
    this.driver.setBudget(this.budget);
  }

  async startFreshDevice(): Promise<void> {
    if (!/^emulator-\d+$/.test(this.serial)) throw new Error('ANDROID_TARGET: refusing a non-emulator or ambiguous device');
    await requireOwnedDevice('android', this.serial);
    const devices = await commandOutput(process.env.ADB || 'adb', ['devices']);
    const matching = devices.split(/\r?\n/).filter((line) => line.startsWith(`${this.serial}\t`));
    if (matching.length !== 1 || !matching[0].endsWith('\tdevice')) throw new Error(`ANDROID_TARGET: ${this.serial} is not the only ready emulator`);
    const adb = process.env.ADB || 'adb';
    await command(adb, ['-s', this.serial, 'wait-for-device'], 60_000);
    await command(adb, ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
    await command(adb, ['-s', this.serial, 'shell', 'pm', 'clear', 'com.android.chrome']).catch(() => undefined);
    const installedPackages = await commandOutput(adb, ['-s', this.serial, 'shell', 'pm', 'list', 'packages']);
    for (const packageName of installedPackages.split(/\r?\n/u).map((line) => line.replace(/^package:/u, '').trim()).filter((value) => /webapk|herdr/iu.test(value))) {
      await command(adb, ['-s', this.serial, 'uninstall', packageName]).catch(() => undefined);
    }
    // Install the user CA before starting Chrome. Chromium caches its platform
    // trust configuration during process startup, so installing it after a
    // browser session has already launched can leave the current target unable
    // to use the newly trusted certificate.
    await this.driver.create({
      capabilities: {
        platformName: 'Android',
        'appium:automationName': 'UiAutomator2',
        'appium:udid': this.serial,
        'appium:appPackage': 'com.android.settings',
        'appium:appActivity': 'com.android.settings.Settings$SecurityDashboardActivity',
        'appium:noReset': true,
        'appium:fullReset': false,
        'appium:newCommandTimeout': 1_200,
        'appium:skipDeviceInitialization': false,
        'appium:skipServerInstallation': false,
      },
      requestTimeoutMs: 60_000,
    });
    await this.installCertificate();
    await this.driver.close();
    await this.createChromeSession(false);
    await this.verifyFixtureEndpoint();
  }

  async openSetupURL(url: string): Promise<void> {
    const webContext = (await this.driver.contexts()).find((context) => context !== 'NATIVE_APP');
    if (webContext) {
      await this.driver.switchContext(webContext);
      await this.driver.execute('window.location.href = arguments[0]; return true;', [url]);
      await delay(1_000);
      return;
    }
    await command(process.env.ADB || 'adb', androidOpenUrlArgs(this.serial, url), 30_000);
    await delay(1_000);
  }

  async openSetupURLInInstalledApp(url: string): Promise<void> {
    await this.attachToInstalledView();
    await this.driver.navigate(url);
    await delay(1_000);
  }

  async installFromBrowser(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    const menu = await this.driver.findAny([
      { using: 'xpath', value: "//*[@resource-id='com.android.chrome:id/menu_button' or contains(@content-desc, 'More options') or @content-desc='Customize and control Google Chrome']" },
      accessibility('More options'),
      accessibility('Customize and control Google Chrome'),
      textLocator('More options'),
      textLocator('Customize and control Google Chrome'),
    ], 30_000);
    await this.driver.click(menu);
    const install = await this.driver.findAny([
      textLocator('Install app'),
      accessibility('Install app'),
      textLocator('Add to Home screen'),
      accessibility('Add to Home screen'),
    ], 15_000);
    await this.driver.click(install);
    const confirm = await this.driver.findAny([
      textLocator('Add'),
      textLocator('Install'),
    ], 15_000);
    await this.driver.click(confirm);

    // The Android 15 launcher asks for a second confirmation when Chrome is
    // adding a shortcut rather than installing a WebAPK.
    const launcherConfirm = await this.driver.findAny([
      accessibility('Add to home screen'),
      textLocator('Add to home screen'),
      accessibility('Add'),
      textLocator('Add'),
    ], 2_000).catch(() => '');
    if (launcherConfirm) {
      await this.driver.click(launcherConfirm);
    } else {
      // Nexus Launcher's Android 15 confirmation is sometimes outside the
      // UiAutomator2 window exposed to Appium. Read the device hierarchy and
      // tap the visible button by its reported bounds instead of dismissing it
      // with HOME.
      await this.confirmLauncherShortcut();
    }
    // Do not proceed merely because the launcher overlay disappeared. Chrome
    // publishes the signed ShortcutInfo asynchronously, and that record is
    // the durable install evidence when no WebAPK package or icon exists.
    await this.waitForChromeShortcut(30_000);
    await delay(1_500);
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
  }

  async launchInstalledApp(): Promise<void> {
    const shortcut = await this.waitForChromeShortcut(30_000);
    this.installedTarget = {
      packageName: 'com.android.chrome',
      activity: CHROME_WEBAPP_COMPONENT.split('/')[1],
      shortcut,
    };
    await this.driver.close();
    await this.launchChromeShortcut();
    await this.waitForInstalledTarget(30_000);
    await this.waitForChromeDevTools(30_000);
    await this.createChromeSession(true);
    await this.attachToInstalledView();
  }

  async assertStandalone(origin: string): Promise<RuntimeIdentity> {
    const identity = await this.readRunningIdentity();
    assertStandalone(identity, origin);
    return identity;
  }

  private async confirmLauncherShortcut(): Promise<boolean> {
    const adb = process.env.ADB || 'adb';
    const dumpPath = `/sdcard/herdr-mobile-ci-ui-${process.pid}.xml`;
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      let xml = '';
      try {
        await command(adb, ['-s', this.serial, 'shell', 'uiautomator', 'dump', dumpPath], 10_000);
        xml = await commandOutput(adb, ['-s', this.serial, 'shell', 'cat', dumpPath], 10_000);
      } catch {
        // The hierarchy may be unavailable while the launcher window is
        // changing. Activity inspection below is an independent fallback.
      }
      const nodes = xml.match(/<node\b[^>]*\/>/gu) || [];
      for (const node of nodes) {
        if (!/add to home screen/iu.test(node)) continue;
        const bounds = node.match(/bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"/u);
        if (!bounds) continue;
        const [, left, top, right, bottom] = bounds;
        await command(adb, [
          '-s', this.serial, 'shell', 'input', 'tap',
          String(Math.round((Number(left) + Number(right)) / 2)),
          String(Math.round((Number(top) + Number(bottom)) / 2)),
        ], 10_000);
        return true;
      }

      try {
        // Some hosted Android images expose AddItemActivity before its
        // accessibility tree is ready. If it is the top activity, the Android
        // 15 launcher confirmation button is consistently the lower-right
        // action; use the device-reported display size rather than a fixed
        // pixel coordinate.
        const activities = await commandOutput(adb, [
          '-s', this.serial, 'shell', 'dumpsys', 'activity', 'activities',
        ], 10_000);
        if (/\.dragndrop\.AddItemActivity\b/u.test(activities)) {
          const size = await commandOutput(adb, ['-s', this.serial, 'shell', 'wm', 'size'], 10_000);
          const match = [...size.matchAll(/(\d+)x(\d+)/gu)].at(-1);
          const width = Number(match?.[1] || 1_080);
          const height = Number(match?.[2] || 2_400);
          await command(adb, [
            '-s', this.serial, 'shell', 'input', 'tap',
            String(Math.round(width * 0.78)),
            String(Math.round(height * 0.94)),
          ], 10_000);
          return true;
        }
      } catch {
        // The system overlay can be between activity transitions; retry until
        // the bounded confirmation deadline rather than treating that as a
        // successful install.
      }
      await delay(250);
    }
    return false;
  }

  private async launchChromeShortcut(): Promise<void> {
    const shortcut = await this.waitForChromeShortcut(30_000);
    await command(process.env.ADB || 'adb', androidChromeShortcutArgs(this.serial, shortcut), 30_000);
  }

  private async waitForChromeShortcut(timeoutMs: number): Promise<AndroidChromeShortcut> {
    const adb = process.env.ADB || 'adb';
    const deadline = Date.now() + timeoutMs;
    let lastError = 'Chrome did not publish a matching Herdr Relay ShortcutInfo';
    while (Date.now() < deadline) {
      try {
        const output = await commandOutput(adb, [
          '-s', this.serial, 'shell', 'cmd', 'shortcut', 'get-shortcuts',
          '--user', '0', '--flags', '15', 'com.android.chrome',
        ], 30_000);
        const expectedOrigin = new URL(this.origin).origin;
        const shortcut = parseAndroidChromeShortcuts(output).find((candidate) => {
          const labels = [candidate.shortLabel, candidate.name];
          if (!labels.some((label) => /herdr(?: mobile)? relay/iu.test(label))) return false;
          try {
            return new URL(candidate.url).origin === expectedOrigin
              && new URL(candidate.scope).origin === expectedOrigin;
          } catch {
            return false;
          }
        });
        if (shortcut) return shortcut;
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250);
    }
    throw new Error(`ANDROID_SHORTCUT: ${lastError}`);
  }

  private async findLauncherIcon(): Promise<string> {
    const locators = [
      textLocator('Herdr Mobile Relay'),
      accessibility('Herdr Mobile Relay'),
      textLocator('Herdr Relay'),
      accessibility('Herdr Relay'),
    ];
    try {
      // A shortcut installation normally places the icon on the current home
      // screen. A WebAPK, however, can be registered in the launcher app
      // drawer without being pinned to that screen (notably on hosted Android
      // 15 images), so inspect both launcher surfaces before failing.
      return await this.driver.findAny(locators, 5_000);
    } catch (homeError) {
      const size = await this.driver.windowSize().catch(() => ({ width: 1_080, height: 2_400 }));
      await this.driver.mobile('swipeGesture', {
        left: 0,
        top: 100,
        width: size.width,
        height: Math.max(1, size.height - 200),
        direction: 'up',
        percent: 0.75,
      }).catch(() => undefined);
      try {
        return await this.driver.findAny(locators, 30_000);
      } catch (drawerError) {
        throw new Error(`ANDROID_LAUNCHER: home screen and app drawer did not expose Herdr Relay (${drawerError instanceof Error ? drawerError.message : String(homeError)})`, { cause: drawerError });
      }
    }
  }

  async attachToInstalledView(): Promise<void> {
    if (!this.installedTarget) throw new Error('ANDROID_CONTEXT: no native installed-app launch has been verified');
    const phase = this.budget.phaseView('android-attachment', 30_000);
    let lastError = '';
    while (!phase.exhausted) {
      phase.assertAvailable('discover installed target');
      if (!(await this.isInstalledTargetForeground())) {
        lastError = 'installed WebappActivity is not foreground';
        await delay(250, phase);
        continue;
      }
      let contextIds: string[];
      try {
        contextIds = (await this.driver.contexts(Math.max(1, phase.remainingMs))).filter((context) => context !== 'NATIVE_APP');
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
        await delay(250, phase);
        continue;
      }
      phase.assertAvailable('record Android context metadata');
      const metadata = await this.driver.contextMetadataRaw(Math.max(1, phase.remainingMs)).catch((error: unknown) => {
        this.diagnostics.record({ phase: 'android-attachment', operation: 'context-metadata', detail: error instanceof Error ? error.message : String(error) });
        return undefined;
      });
      if (metadata !== undefined) {
        this.diagnostics.record({ phase: 'android-attachment', operation: 'context-metadata-observed', detail: metadata });
      }
      if (!contextIds.length) {
        lastError = 'Chromium context IDs are unavailable';
        await delay(250, phase);
        continue;
      }
      for (const contextId of contextIds) {
        phase.assertAvailable('select installed Chromium context');
        const timeoutMs = Math.max(1, phase.remainingMs);
        await this.driver.switchContext(contextId, timeoutMs);
        const handles = await this.driver.windowHandles(Math.max(1, phase.remainingMs));
        const windows = handles.length ? handles : [''];
        for (const handle of windows) {
          const windowTimeoutMs = Math.max(1, phase.remainingMs);
          if (handle) await this.driver.switchWindow(handle, windowTimeoutMs);
          const url = await this.driver.currentUrl(Math.max(1, phase.remainingMs));
          this.lastUrl = url;
          if (this.isExpectedOrigin(url) && await this.isInstalledTargetForeground()) return;
        }
      }
      lastError = `no installed Chromium window for ${this.origin}`;
      await delay(250, phase);
    }
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    throw new Error(`ANDROID_CONTEXT: no installed web context for ${this.origin}: ${lastError}`);
  }

  async readRunningIdentity(): Promise<RuntimeIdentity> {
    await this.attachToInstalledView();
    const identity = await this.driver.execute<RuntimeIdentity>(runtimeScript());
    this.lastIdentity = {
      ...identity,
      standalone: identity.standalone || Boolean(this.installedTarget && await this.isInstalledTargetForeground()),
      provider: identity.standalone ? identity.provider : 'android-standalone',
      nativeProvider: this.installedPackage ? `android:${this.installedPackage}` : undefined,
      nativeActivity: this.lastForeground?.activity,
      nativePid: this.lastForeground?.pid,
    };
    return this.lastIdentity;
  }

  async readUpdateCompletion(): Promise<UpdateCompletionEvidence> {
    await this.attachToInstalledView();
    this.lastCompletion = await this.driver.execute<UpdateCompletionEvidence>(updateCompletionScript());
    return this.lastCompletion;
  }

  async openFixtureAgent(relayName: string): Promise<void> {
    if (!/^[A-Za-z0-9_.-]+$/u.test(relayName)) throw new Error(`APPIUM_AGENT: invalid fixture relay name ${relayName}`);
    await this.attachToInstalledView();
    const currentUrl = await this.driver.currentUrl().catch(() => '');
    if (currentUrl.includes('#settings')) await this.clickWebText('Back');
    const deadline = Date.now() + 30_000;
    let lastError = '';
    while (Date.now() < deadline) {
      try {
        await this.attachToInstalledView();
        const agent = await this.driver.find(css(`button.agent-open[aria-label="Open mobile-ci on ${relayName}"]`), 2_000);
        // The card remains in the DOM while the relay's inventory reconnects,
        // but its button is disabled until that inventory is ready.
        if ((await this.driver.attribute(agent, 'disabled')) !== null) {
          lastError = `agent ${relayName} is waiting for inventory`;
        } else {
          try {
            await this.driver.click(agent);
          } catch (error) {
            // Chrome 131 can report a visible card button as not interactable
            // after a standalone relaunch. Dispatch the same DOM click only
            // after confirming that the matching, enabled button is visible.
            const selector = `button.agent-open[aria-label="Open mobile-ci on ${relayName}"]`;
            const result = await this.driver.execute<{ clicked: boolean; reason?: string }>(
              `return (() => {
                const buttons = [...document.querySelectorAll(arguments[0])];
                const button = buttons.find((candidate) => {
                  const rect = candidate.getBoundingClientRect();
                  const style = getComputedStyle(candidate);
                  return !candidate.disabled && rect.width > 0 && rect.height > 0
                    && style.display !== 'none' && style.visibility !== 'hidden';
                });
                if (!button) return { clicked: false, reason: 'no visible enabled agent button' };
                button.scrollIntoView({ block: 'center', inline: 'center' });
                button.click();
                return { clicked: true };
              })();`,
              [selector],
            );
            if (!result.clicked) throw error;
          }
          await delay(1_000);
          return;
        }
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250);
    }
    throw new Error(`APPIUM_AGENT: ${relayName}: ${lastError}`);
  }

  async backgroundApp(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
    await delay(500);
  }

  async relaunchInstalledApp(): Promise<void> {
    await this.launchInstalledApp();
  }

  async terminateInstalledApp(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    const packageName = this.installedPackage || await this.currentForegroundPackage();
    if (!packageName || packageName === 'com.android.launcher3' || packageName === 'com.google.android.apps.nexuslauncher') {
      throw new Error('ANDROID_TERMINATE: could not identify the installed PWA process');
    }
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'am', 'force-stop', packageName]);
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
  }

  async showKeyboardOnComposer(): Promise<void> {
    await this.attachToInstalledView();
    let composer = await this.driver.find(css('textarea[aria-label="Prompt"]'), 5_000).catch(() => '');
    if (!composer) {
      const open = await this.driver.find(css('button[aria-label^="Open "]'), 30_000);
      await this.driver.click(open);
      composer = await this.driver.find(css('textarea[aria-label="Prompt"]'), 30_000);
    }
    await this.driver.click(composer);
    this.keyboardDraft = 'mobile-device-ci draft';
    await this.driver.sendKeys(composer, this.keyboardDraft);
    await this.waitForKeyboard(true);
  }

  async hideKeyboard(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP');
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_BACK']);
    await this.waitForKeyboard(false);
    await this.attachToInstalledView();
    if (this.keyboardDraft) {
      const value = await this.driver.execute<string>("return document.querySelector('textarea[aria-label=\\\"Prompt\\\"]')?.value || ''");
      if (value !== this.keyboardDraft) throw new Error('ANDROID_KEYBOARD: draft was not preserved after dismissal');
    }
  }

  async clickWebText(text: string): Promise<void> {
    const deadline = Date.now() + 30_000;
    let lastError = '';
    while (Date.now() < deadline) {
      try {
        await this.attachToInstalledView();
        const element = await this.driver.findAny([buttonText(text), accessibility(text), accessibilityPrefix(text), textLocator(text)], 2_000);
        if ((await this.driver.attribute(element, 'disabled')) === null) {
          await this.driver.click(element);
          return;
        }
        lastError = `${text} is disabled`;
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250);
    }
    throw new Error(`APPIUM_BUTTON: ${text}: ${lastError}`);
  }

  async clickDialogText(dialogId: string, text: string): Promise<void> {
    const deadline = Date.now() + 30_000;
    let lastError = '';
    while (Date.now() < deadline) {
      try {
        await this.attachToInstalledView();
        const buttons = await this.driver.findAll(css(`#${dialogId} button`));
        for (const button of buttons) {
          if ((await this.driver.text(button)).trim() === text) {
            await this.driver.click(button);
            return;
          }
        }
        lastError = `${text} is not visible in ${dialogId}`;
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250);
    }
    throw new Error(`APPIUM_DIALOG_BUTTON: ${dialogId}/${text}: ${lastError}`);
  }

  async setPreference(preference: string): Promise<void> {
    await this.clickWebText('Settings');
    await this.clickWebText(preference === 'state' ? 'By State' : 'Mixed');
  }

  async preferenceValue(): Promise<string> {
    await this.attachToInstalledView();
    return this.driver.execute<string>("return localStorage.getItem('herdr_home_workspace_layout') || ''");
  }

  async captureSanitizedEvidence(name: string): Promise<void> {
    await mkdir(this.outputDir, { recursive: true });
    try {
      const screenshot = Buffer.from(await this.driver.screenshot(), 'base64');
      if (screenshot.byteLength <= 20 * 1024 * 1024) await writeFile(join(this.outputDir, `${name}.png`), screenshot, { mode: 0o600 });
    } catch (error) {
      this.diagnostics.record({ phase: 'evidence', operation: 'screenshot', detail: error instanceof Error ? error.message : String(error) });
    }
    await writeSanitizedJson(join(this.outputDir, `${name}-appium.json`), this.evidenceSnapshot());
    await this.diagnostics.write(join(this.outputDir, `${name}-events.json`));
    const adb = process.env.ADB || 'adb';
    const captures: Array<[string, string[]]> = [
      ['logcat', ['-s', this.serial, 'logcat', '-d', '-t', '1200']],
      ['crash', ['-s', this.serial, 'logcat', '-b', 'crash', '-d', '-t', '600']],
      ['activity', ['-s', this.serial, 'shell', 'dumpsys', 'activity', 'activities']],
      ['window', ['-s', this.serial, 'shell', 'dumpsys', 'window', 'windows']],
      ['packages', ['-s', this.serial, 'shell', 'dumpsys', 'package', 'com.android.chrome']],
    ];
    for (const [suffix, args] of captures) {
      const output = await commandOutput(adb, args, 10_000).catch((error) => error instanceof Error ? error.message : String(error));
      await writeBoundedText(join(this.outputDir, `${name}-android-${suffix}.log`), output);
    }
  }

  evidenceSnapshot(): Record<string, unknown> {
    return {
      platform: this.name,
      device: this.serial,
      origin: this.origin,
      installedTarget: this.installedTarget ? {
        packageName: this.installedTarget.packageName,
        activity: this.installedTarget.activity,
        shortcutId: this.installedTarget.shortcut.id,
        scope: this.installedTarget.shortcut.scope,
      } : undefined,
      lastUrl: this.lastUrl,
      lastIdentity: this.lastIdentity,
      lastCompletion: this.lastCompletion,
      lastForeground: this.lastForeground,
      driver: this.driver.snapshot(),
      events: this.diagnostics.snapshot(),
    };
  }

  async stopOwnedResources(): Promise<void> {
    await this.driver.close();
  }

  private async createChromeSession(attachToRunningApp: boolean): Promise<void> {
    await this.driver.create({
      capabilities: androidChromeCapabilities(this.serial, attachToRunningApp),
      requestTimeoutMs: 60_000,
      budget: this.budget,
    });
  }

  private async waitForChromeDevTools(timeoutMs: number): Promise<void> {
    const phase = this.budget.phaseView('android-devtools', timeoutMs);
    const adb = process.env.ADB || 'adb';
    let last = '';
    while (!phase.exhausted) {
      phase.assertAvailable('discover Chrome DevTools socket');
      try {
        const sockets = await commandOutput(adb, ['-s', this.serial, 'shell', 'cat', '/proc/net/unix'], 5_000, {
          budget: phase,
          label: 'discover Chrome DevTools socket',
        });
        if (hasAndroidChromeDevToolsSocket(sockets)) return;
        last = 'chrome_devtools_remote socket is not published';
      } catch (error) {
        last = error instanceof Error ? error.message : String(error);
      }
      await delay(250, phase);
    }
    throw new Error(`ANDROID_CONTEXT: Chrome DevTools was not ready (${last})`);
  }

  private async installCertificate(): Promise<void> {
    const adb = process.env.ADB || 'adb';
    const remote = '/sdcard/Download/herdr-mobile-ci-ca.crt';
    const remoteName = basename(remote);
    const commonName = await this.certificateCommonName();

    await command(adb, ['-s', this.serial, 'push', this.options.certificate, remote], 30_000);
    // adb push does not update MediaProvider, so DocumentsUI may omit the
    // freshly copied file from Downloads until the exact path is scanned.
    await command(adb, [
      '-s', this.serial, 'shell', 'content', 'call', '--uri', 'content://media',
      '--method', 'scan_file', '--arg', remote,
    ], 30_000).catch(async () => {
      await command(adb, [
        '-s', this.serial, 'shell', 'am', 'broadcast', '--receiver-include-background',
        '-a', 'android.intent.action.MEDIA_SCANNER_SCAN_FILE', '-d', `file://${remote}`,
      ], 30_000).catch(() => undefined);
    });
    await delay(500);
    await command(adb, ['-s', this.serial, 'shell', 'am', 'force-stop', 'com.google.android.documentsui']).catch(() => undefined);
    await command(adb, ['-s', this.serial, 'shell', 'am', 'force-stop', 'com.android.settings']);
    await command(adb, ['-s', this.serial, 'shell', 'am', 'start', '-a', 'android.settings.SECURITY_SETTINGS'], 30_000);
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    await this.waitForForegroundPackage('com.android.settings');
    await delay(500);

    // Android 11+ only permits CA installation when the user starts it from
    // Settings. The generic credential intent is deliberately rejected and
    // cannot select the file pushed above on the Android 35 image.
    await this.clickNative([
      accessibility('More security settings'),
      accessibility('More security & privacy'),
      accessibility('More security and privacy'),
      textLocator('More security settings'),
      textLocator('More security & privacy'),
      textLocator('More security and privacy'),
    ], 'More security settings');
    await this.clickNative([
      accessibility('Encryption & credentials'),
      textLocator('Encryption & credentials'),
      textLocator('Encryption & Credentials'),
    ], 'Encryption & credentials');
    await this.clickNative([
      accessibility('Install a certificate'),
      accessibility('Install from device storage'),
      accessibility('Install from storage'),
      textLocator('Install a certificate'),
      textLocator('Install from device storage'),
      textLocator('Install from storage'),
    ], 'Install a certificate');
    await this.clickNative([
      accessibility('CA certificate'),
      textLocator('CA certificate'),
      textLocator('CA Certificate'),
    ], 'CA certificate');
    await this.clickNative([
      accessibility('Install anyway'),
      accessibility('INSTALL ANYWAY'),
      textLocator('Install anyway'),
      textLocator('INSTALL ANYWAY'),
    ], 'Install anyway');

    // The supported flow now opens DocumentsUI. Explicitly open Downloads and
    // choose the pushed file; do not continue after a missing control.
    await this.clickNative([
      accessibility('Show roots'),
      accessibility('Open navigation drawer'),
      accessibility('Open navigation'),
    ], 'certificate picker navigation');
    await this.clickNative([
      accessibility('Downloads'),
      textLocator('Downloads'),
    ], 'Downloads');
    await this.clickNative([
      accessibility(remoteName),
      textLocator(remoteName),
    ], remoteName);
    // Android 15 installs a CA certificate immediately after the file is
    // selected; there is no certificate-name dialog or Done button.
    await delay(1_000);
    await this.verifyCertificate(remoteName, commonName);
    await command(adb, ['-s', this.serial, 'shell', 'rm', '-f', remote], 30_000);
  }

  private async clickNative(locators: Locator[], description: string, timeoutMs = 30_000): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    let lastError = '';
    while (Date.now() < deadline) {
      try {
        const element = await this.findNative(locators, Math.min(2_000, Math.max(1, deadline - Date.now())));
        await this.driver.click(element);
        return;
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250);
    }
    throw new Error(`ANDROID_CERTIFICATE: ${description}: ${lastError}`);
  }

  private async findNative(locators: Locator[], timeoutMs: number): Promise<string> {
    const deadline = Date.now() + timeoutMs;
    let lastError = '';
    let scrolls = 0;
    const size = await this.driver.windowSize().catch(() => ({ width: 1_080, height: 2_400 }));
    while (Date.now() < deadline) {
      try {
        return await this.driver.findAny(locators, Math.min(2_000, Math.max(1, deadline - Date.now())));
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      if (scrolls >= 8) {
        await delay(250);
        continue;
      }
      await this.driver.mobile('scrollGesture', {
        left: 0,
        top: 100,
        width: size.width,
        height: Math.max(1, size.height - 200),
        direction: 'down',
        percent: 0.75,
      }).catch((error) => {
        lastError = error instanceof Error ? error.message : String(error);
      });
      scrolls += 1;
      await delay(250);
    }
    throw new Error(`APPIUM_NATIVE: ${lastError}`);
  }

  private async waitForForegroundPackage(packageName: string, timeoutMs = 30_000): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    const accepted = packageName === 'com.android.settings'
      ? [packageName, 'com.google.android.permissioncontroller']
      : [packageName];
    let current = '';
    while (Date.now() < deadline) {
      current = await this.currentForegroundPackage();
      if (accepted.includes(current)) return;
      await delay(250);
    }
    throw new Error(`ANDROID_CERTIFICATE: expected ${accepted.join(' or ')} in foreground, found ${current || 'none'}`);
  }

  private async waitForKeyboard(expected: boolean): Promise<void> {
    const phase = this.budget.phaseView('android-keyboard', 10_000);
    let last = '';
    while (!phase.exhausted) {
      phase.assertAvailable(`keyboard ${expected ? 'show' : 'hide'}`);
      const inputMethod = await commandOutput(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'dumpsys', 'input_method'], 5_000);
      const windowState = await commandOutput(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'dumpsys', 'window', 'windows'], 5_000).catch(() => '');
      const visible = /mInputShown=true|isInputShown=true/u.test(inputMethod);
      const rectangles = [...windowState.matchAll(/\[(\d+),(\d+)\]\[(\d+),(\d+)\]/gu)]
        .some((match) => Number(match[4]) > Number(match[2]) && Number(match[3]) > Number(match[1]));
      last = `visible=${visible} geometry=${rectangles}`;
      if (visible === expected && (!expected || rectangles)) return;
      await delay(250, phase);
    }
    throw new Error(`ANDROID_KEYBOARD: expected ${expected ? 'visible' : 'hidden'} software keyboard (${last})`);
  }

  private async certificateCommonName(): Promise<string> {
    const certificate = new X509Certificate(await readFile(this.options.certificate));
    const commonName = certificate.subject.match(/CN\s*=\s*([^,\n/]+)/u)?.[1]?.trim();
    if (!commonName) throw new Error('ANDROID_CERTIFICATE: supplied certificate has no common name');
    return commonName;
  }

  private async verifyCertificate(certificateName: string, commonName: string): Promise<void> {
    const adb = process.env.ADB || 'adb';
    await command(adb, ['-s', this.serial, 'shell', 'am', 'start', '-a', 'com.android.settings.TRUSTED_CREDENTIALS_USER'], 30_000);
    await this.driver.switchContext('NATIVE_APP').catch(() => undefined);
    await this.waitForForegroundPackage('com.android.settings');
    await delay(500);
    const nameWithoutExtension = certificateName.replace(/\.[^.]+$/u, '');
    await this.driver.findAny([
      accessibility(commonName),
      textLocator(commonName),
      accessibility(certificateName),
      textLocator(certificateName),
      accessibility(nameWithoutExtension),
      textLocator(nameWithoutExtension),
    ], 20_000);

  }

  private async verifyFixtureEndpoint(): Promise<void> {
    const phase = this.budget.phaseView('android-certificate', 30_000);
    let lastError = '';
    while (!phase.exhausted) {
      phase.assertAvailable('verify fixture certificate');
      try {
        const webContext = (await this.driver.contexts()).find((context) => context !== 'NATIVE_APP');
        if (!webContext) throw new Error('Chrome web context is unavailable');
        await this.driver.switchContext(webContext);
        await this.driver.navigate(`${this.origin}/version.json`);
        const observed = await this.driver.execute<{ status: number; url: string; body: string }>(`return fetch(location.href, { cache: 'no-store' }).then(async response => ({
          status: response.status,
          url: response.url,
          body: (await response.text()).slice(0, 8192),
        }));`);
        const metadata = JSON.parse(observed.body) as Record<string, unknown>;
        if (observed.status === 200 && this.isExpectedOrigin(observed.url)
          && typeof metadata.version === 'string'
          && Number.isInteger(Number(metadata.assets))
          && Number(metadata.assets) > 0
          && !/ERR_CERT|NET::ERR|privacy error|not private/iu.test(observed.body)) return;
        lastError = `status=${observed.status} url=${observed.url} body=${observed.body.slice(0, 200)}`;
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250, phase);
    }
    throw new Error(`ANDROID_CERTIFICATE: fixture HTTPS response identity was not trusted (${lastError})`);
  }

  private async foregroundEvidence(): Promise<{ packageName: string; activity: string; pid: string; raw: string }> {
    const output = await commandOutput(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'dumpsys', 'activity', 'activities'], 10_000);
    const component = output.match(/(?:mResumedActivity|ResumedActivity): ActivityRecord\{[^}]+\s([A-Za-z0-9_.]+)\/([A-Za-z0-9_.$]+)/u);
    const pid = output.match(/(?:mResumedActivity|ResumedActivity): ActivityRecord\{[^}]+\s+pid=(\d+)/u)?.[1] || '';
    return { packageName: component?.[1] || '', activity: component?.[2] || '', pid, raw: output.slice(-20_000) };
  }

  private async isInstalledTargetForeground(): Promise<boolean> {
    if (!this.installedTarget) return false;
    const foreground = await this.foregroundEvidence();
    this.lastForeground = foreground;
    const packageMatches = foreground.packageName === this.installedTarget.packageName
      || /webapk/iu.test(foreground.packageName);
    if (!packageMatches) return false;
    if (foreground.activity === this.installedTarget.activity) return true;
    return /Webapp|WebApk/iu.test(foreground.activity) && !/ChromeTabbedActivity|LauncherActivity$/u.test(foreground.activity);
  }

  private async waitForInstalledTarget(timeoutMs: number, launchError?: unknown): Promise<void> {
    const phase = this.budget.phaseView('android-launch-proof', timeoutMs);
    let last = '';
    while (!phase.exhausted) {
      phase.assertAvailable('verify installed launch');
      try {
        const evidence = await this.foregroundEvidence();
        last = `${evidence.packageName}/${evidence.activity} pid=${evidence.pid}`;
        if (await this.isInstalledTargetForeground()) {
          this.installedPackage = evidence.packageName;
          return;
        }
      } catch (error) {
        last = error instanceof Error ? error.message : String(error);
      }
      await delay(250, phase);
    }
    const detail = launchError instanceof Error ? `; launch=${launchError.message}` : '';
    throw new Error(`ANDROID_TARGET: signed installed target was not foreground (${last || 'none'})${detail}`);
  }

  private isExpectedOrigin(value: string): boolean {
    try {
      return new URL(value).origin === new URL(this.origin).origin;
    } catch {
      return false;
    }
  }

  private async currentForegroundPackage(): Promise<string> {
    return (await this.foregroundEvidence()).packageName;
  }
}
