import { X509Certificate } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { basename, join } from 'node:path';
import {
  assertStandalone,
  isAndroidPersistentWebAppActivity,
  isQualificationFatal,
  qualificationFatal,
  type QualificationFatalError,
  type RuntimeIdentity,
} from '../support/oracle';
import { DiagnosticRecorder, redactText, writeBoundedText, writeSanitizedJson } from '../support/diagnostics';
import { PhaseBudget, PhaseBudgetError } from '../support/budget';
import { CommandError, command, commandOutput, type CommandResult } from '../support/process';
import { requireOwnedDevice } from '../support/device';
import type { AndroidEnvironmentMeasurement } from '../android-measurement';
import { isAndroidTerminationPackage } from '../android-events';
import {
  accessibility,
  androidTextLocator,
  ariaLabel,
  ariaLabelPrefix,
  AppiumClient,
  buttonText,
  css,
  delay,
  isCommandAdmissionError,
  isFatalDriverError,
  isRetryableElementLookupError,
  minimumDriverRequestMs,
  textLocator,
  type Locator,
  WebDriverError,
} from '../support/webdriver';
import { runtimeScript, updateCompletionScript, type MobilePlatform, type PlatformOptions, type UpdateCompletionEvidence } from './types';

function androidShellQuote(value: string): string {
  return `'${value.replaceAll("'", "'\"'\"'")}'`;
}

const ANDROID_NATIVE_IDLE_TIMEOUT_MS = 500;
const ANDROID_NATIVE_SELECTOR_TIMEOUT_MS = 0;
const ANDROID_NATIVE_LOOKUP_COMMAND_MS = 5_000;
const ANDROID_PICKER_SOURCE_COMMAND_MS = 6_000;
const ANDROID_LAUNCHER_PACKAGE = 'com.google.android.apps.nexuslauncher';
const ANDROID_LAUNCHER_ACTIVITY = 'com.android.launcher3.dragndrop.AddItemActivity';
const ANDROID_PICKER_PACKAGE = 'com.google.android.documentsui';
const ANDROID_PICKER_ACTIVITY = 'com.android.documentsui.picker.PickActivity';
const ANDROID_NATIVE_SCROLL_COMMAND_MS = 5_000;
const ANDROID_NATIVE_SCROLL_LIMIT = 8;
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

export type AndroidLaunchFailureKind = 'terminal' | 'timeout';

export function androidLaunchFailureKind(error: unknown): AndroidLaunchFailureKind {
  if (error instanceof CommandError && error.timedOut) return 'timeout';
  const message = error instanceof Error ? error.message : String(error);
  return /timed out|timeout|ETIMEDOUT/iu.test(message) ? 'timeout' : 'terminal';
}

export interface AndroidChromeShortcut {
  id: string;
  flags?: string;
  shortLabel: string;
  name: string;
  url: string;
  scope: string;
  mac: string;
  source?: string;
  displayMode?: string;
  orientation?: string;
}

function redactShortcutValue(value: string, shortcut: AndroidChromeShortcut): string {
  let redacted = redactText(value);
  for (const secret of [shortcut.url, shortcut.mac]) {
    if (secret) redacted = redacted.replaceAll(secret, '[REDACTED]');
  }
  return redacted;
}

function shortcutField(block: string, name: string): string | undefined {
  const escaped = name.replace(/[.*+?^${}()|[\]\\]/gu, '\\$&');
  const match = block.match(new RegExp(`(?:^|[,{[]|\\s)${escaped}=([^,}\\]\\r\\n]+)`, 'u'));
  return match?.[1]?.trim();
}

export function parseAndroidChromeShortcuts(output: string): AndroidChromeShortcut[] {
  return output.split(/(?=^ShortcutInfo \{)/mu).flatMap((block) => {
    const id = shortcutField(block, 'id');
    const flags = shortcutField(block, 'flags');
    const shortLabel = shortcutField(block, 'shortLabel');
    const name = shortcutField(block, CHROME_WEBAPP_NAME);
    const url = shortcutField(block, CHROME_WEBAPP_URL);
    const scope = shortcutField(block, CHROME_WEBAPP_SCOPE);
    const mac = shortcutField(block, CHROME_WEBAPP_MAC);
    if (!id || !shortLabel || !name || !url || !scope || !mac) return [];
    return [{
      id,
      flags,
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
  environmentMeasurement?: AndroidEnvironmentMeasurement;
  readonly driver: AppiumClient;
  private readonly serial: string;
  private readonly origin: string;
  private readonly outputDir: string;
  private readonly budget: PhaseBudget;
  private readonly diagnostics: DiagnosticRecorder;
  private installedPackage = '';
  private installedTarget?: { packageName: string; activity: string; shortcut: AndroidChromeShortcut };
  private selectedInstalledWindow = '';
  private selectedInstalledWindowValid = false;
  private ownershipFailure?: QualificationFatalError;
  private lastIdentity?: RuntimeIdentity;
  private lastUrl = '';
  private keyboardDraft = '';
  private lastCompletion?: UpdateCompletionEvidence;
  private lastForeground?: { packageName: string; activity: string; pid: string };
  private lastNativeSettings?: Record<string, unknown>;
  private lastLaunch?: { shortcut: Record<string, unknown>; transitions: Record<string, unknown>[] };

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
    await this.configureNativeSettings();
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
    await this.driver.switchContext('NATIVE_APP').catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
    });
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

    const confirmed = await this.confirmLauncherShortcut();
    if (!confirmed) throw new Error('ANDROID_LAUNCHER: confirmation control was not exposed by the automation hierarchy');
    // Do not proceed merely because the launcher overlay disappeared. Chrome
    // publishes the signed ShortcutInfo asynchronously, and that record is
    // the durable install evidence when no WebAPK package or icon exists.
    await this.waitForChromeShortcut(30_000);
    await delay(1_500);
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
  }

  async launchInstalledApp(): Promise<void> {
    await requireOwnedDevice('android', this.serial);
    this.selectedInstalledWindow = '';
    this.selectedInstalledWindowValid = false;
    const shortcut = await this.waitForChromeShortcut(30_000);
    this.installedTarget = {
      packageName: 'com.android.chrome',
      activity: CHROME_WEBAPP_COMPONENT.split('/')[1],
      shortcut,
    };
    this.lastLaunch = { shortcut: this.shortcutEvidence(shortcut), transitions: [] };
    this.diagnostics.record({ phase: 'android-launch', operation: 'shortcut-observed', detail: this.lastLaunch.shortcut });
    await this.driver.close();
    await this.recordLaunchForeground('before-command');
    try {
      await this.launchChromeShortcut(shortcut);
    } catch (error) {
      await this.recordLaunchForeground('after-command-failure');
      throw error;
    }
    await this.recordLaunchForeground('after-command');
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

  private nativeTransactionAvailable(deadline: number, timeoutMs = ANDROID_NATIVE_LOOKUP_COMMAND_MS): boolean {
    return Math.min(deadline - Date.now(), this.budget.remainingMs) >= timeoutMs;
  }

  private async nativeConfirmationForeground(packageName: string, activity: string, deadline: number): Promise<boolean> {
    if (!this.nativeTransactionAvailable(deadline)) return false;
    const foreground = await this.foregroundEvidence(ANDROID_NATIVE_LOOKUP_COMMAND_MS);
    this.diagnostics.record({ phase: 'android-native-transition', operation: 'focused-activity', detail: {
      packageName: foreground.packageName, activity: foreground.activity,
      focusedPackage: foreground.focusedPackage, focusedActivity: foreground.focusedActivity,
    } });
    return foreground.packageName === packageName && foreground.activity === activity
      && foreground.focusedPackage === packageName && foreground.focusedActivity === activity;
  }

  private async nativeControlReady(element: string, deadline: number): Promise<boolean> {
    for (const attribute of ['enabled', 'displayed', 'clickable']) {
      if (!this.nativeTransactionAvailable(deadline)) return false;
      if (await this.driver.attribute(element, attribute, ANDROID_NATIVE_LOOKUP_COMMAND_MS) !== 'true') return false;
    }
    if (!this.nativeTransactionAvailable(deadline)) return false;
    const rect = await this.driver.elementRect(element, ANDROID_NATIVE_LOOKUP_COMMAND_MS);
    if (!this.nativeTransactionAvailable(deadline)) return false;
    const size = await this.driver.windowSize(ANDROID_NATIVE_LOOKUP_COMMAND_MS);
    return [rect.x, rect.y, rect.width, rect.height, size.width, size.height].every(Number.isFinite)
      && rect.x >= 0 && rect.y >= 0 && rect.width > 0 && rect.height > 0
      && rect.x + rect.width <= size.width && rect.y + rect.height <= size.height;
  }

  private async confirmLauncherShortcut(): Promise<boolean> {
    const deadline = Date.now() + Math.min(30_000, this.budget.remainingMs);
    const locator = { using: 'xpath', value:
      `//*[@resource-id='${ANDROID_LAUNCHER_PACKAGE}:id/add_item_bottom_sheet_content']`
      + `[.//*[@resource-id='${ANDROID_LAUNCHER_PACKAGE}:id/widget_name' and @text='Herdr Relay' and @displayed='true']]`
      + `//android.widget.Button[@package='${ANDROID_LAUNCHER_PACKAGE}' and @text='Add to home screen' and @clickable='true' and @enabled='true' and @displayed='true']`,
    };
    while (this.nativeTransactionAvailable(deadline)) {
      try {
        const foreground = await this.nativeConfirmationForeground(ANDROID_LAUNCHER_PACKAGE, ANDROID_LAUNCHER_ACTIVITY, deadline);
        if (foreground && this.nativeTransactionAvailable(deadline)) {
          const element = await this.driver.findAnyOnce([locator], ANDROID_NATIVE_LOOKUP_COMMAND_MS);
          if (await this.nativeControlReady(element, deadline)
            && await this.nativeConfirmationForeground(ANDROID_LAUNCHER_PACKAGE, ANDROID_LAUNCHER_ACTIVITY, deadline)
            && this.nativeTransactionAvailable(deadline)) {
            await this.driver.click(element, ANDROID_NATIVE_LOOKUP_COMMAND_MS);
            this.diagnostics.record({ phase: 'android-launcher', operation: 'confirmation-clicked' });
            return true;
          }
        }
      } catch (error) {
        if (isFatalDriverError(error) || !isRetryableElementLookupError(error)) throw error;
      }
      if (this.nativeTransactionAvailable(deadline)) await delay(250);
    }
    return false;
  }

  private async launchChromeShortcut(shortcut?: AndroidChromeShortcut): Promise<CommandResult> {
    const launchShortcut = shortcut || await this.waitForChromeShortcut(30_000);
    const args = androidChromeShortcutArgs(this.serial, launchShortcut);
    try {
      const result = await command(process.env.ADB || 'adb', args, 30_000);
      this.recordLaunchCommand('succeeded', result, launchShortcut);
      return result;
    } catch (error) {
      this.recordLaunchCommand(androidLaunchFailureKind(error), error, launchShortcut);
      throw error;
    }
  }

  private shortcutEvidence(shortcut: AndroidChromeShortcut): Record<string, unknown> {
    const safe = (value: string): string => redactShortcutValue(value, shortcut);
    return {
      id: safe(shortcut.id),
      flags: shortcut.flags,
      shortLabel: safe(shortcut.shortLabel),
      name: safe(shortcut.name),
      url: safe(shortcut.url),
      scope: safe(shortcut.scope),
      fields: {
        id: { type: 'string', present: true },
        flags: { type: 'string', present: shortcut.flags !== undefined },
        shortLabel: { type: 'string', present: true },
        name: { type: 'string', present: true },
        url: { type: 'string', present: true },
        scope: { type: 'string', present: true },
        mac: { type: 'string', present: true, redacted: true, length: shortcut.mac.length },
        source: { type: 'string', present: shortcut.source !== undefined },
        displayMode: { type: 'string', present: shortcut.displayMode !== undefined },
        orientation: { type: 'string', present: shortcut.orientation !== undefined },
      },
      mac: '[REDACTED]',
      source: shortcut.source,
      displayMode: shortcut.displayMode,
      orientation: shortcut.orientation,
    };
  }

  private recordLaunchCommand(status: string, result: CommandResult | CommandError | unknown, shortcut?: AndroidChromeShortcut): void {
    const safe = (value: string): string => shortcut ? redactShortcutValue(value, shortcut) : redactText(value);
    const detail = result instanceof CommandError
      ? {
        status,
        exitCode: result.exitCode,
        durationMs: result.durationMs,
        timedOut: result.timedOut,
        signal: result.signal,
        stdout: safe(result.stdout),
        stderr: safe(result.stderr),
      }
      : result && typeof result === 'object' && 'code' in result
        ? {
          status,
          exitCode: Number((result as CommandResult).code),
          durationMs: Number((result as CommandResult).durationMs),
          timedOut: (result as CommandResult).timedOut,
          signal: (result as CommandResult).signal,
          stdout: safe((result as CommandResult).stdout),
          stderr: safe((result as CommandResult).stderr),
        }
        : { status, error: safe(result instanceof Error ? result.message : String(result)) };
    this.diagnostics.record({ phase: 'android-launch', operation: 'launch-command', durationMs: detail.durationMs as number | undefined, timedOut: detail.timedOut as boolean | undefined, signal: detail.signal as string | undefined, detail });
  }

  private async recordLaunchForeground(label: string): Promise<void> {
    try {
      const evidence = await this.foregroundEvidence(5_000);
      const detail = {
        state: label,
        packageName: evidence.packageName,
        activity: evidence.activity,
        pid: evidence.pid,
        raw: evidence.raw,
      };
      this.lastForeground = evidence;
      this.lastLaunch?.transitions.push(detail);
      this.diagnostics.record({ phase: 'android-launch', operation: 'foreground-transition', detail });
    } catch (error) {
      if (isFatalDriverError(error)) throw error;
      const detail = { state: label, error: error instanceof Error ? error.message : String(error) };
      this.lastLaunch?.transitions.push(detail);
      this.diagnostics.record({ phase: 'android-launch', operation: 'foreground-transition', detail });
    }
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
      if (isFatalDriverError(homeError)) throw homeError;
      const size = await this.driver.windowSize().catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
        return { width: 1_080, height: 2_400 };
      });
      await this.driver.mobile('swipeGesture', {
        left: 0,
        top: 100,
        width: size.width,
        height: Math.max(1, size.height - 200),
        direction: 'up',
        percent: 0.75,
      }).catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
      });
      try {
        return await this.driver.findAny(locators, 30_000);
      } catch (drawerError) {
        if (isFatalDriverError(drawerError)) throw drawerError;
        throw new Error(`ANDROID_LAUNCHER: home screen and app drawer did not expose Herdr Relay (${drawerError instanceof Error ? drawerError.message : String(homeError)})`, { cause: drawerError });
      }
    }
  }

  async attachToInstalledView(timeoutMs = 30_000): Promise<void> {
    this.assertOwnershipClear();
    if (!this.installedTarget) throw new Error('ANDROID_CONTEXT: no native installed-app launch has been verified');
    const phase = this.budget.phaseView('android-attachment', timeoutMs);
    let lastError = '';
    while (!phase.exhausted) {
      phase.assertAvailable('discover installed target');
      const foregroundTimeout = Math.min(5_000, phase.remainingMs);
      let targetForeground = false;
      try {
        targetForeground = foregroundTimeout >= minimumDriverRequestMs
          && await this.isInstalledTargetForeground(foregroundTimeout, phase);
      } catch (error) {
        if (isFatalDriverError(error) || isQualificationFatal(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
      }
      if (!targetForeground) {
        lastError ||= 'installed WebappActivity is not foreground';
        try {
          await delay(250, phase);
        } catch (error) {
          if (phase.exhausted) break;
          throw error;
        }
        continue;
      }
      let contextIds: string[];
      try {
        const contextsTimeout = phase.remainingMs;
        if (contextsTimeout < minimumDriverRequestMs) break;
        contextIds = (await this.driver.contexts(contextsTimeout)).filter((context) => context !== 'NATIVE_APP');
      } catch (error) {
        if (isFatalDriverError(error) || isQualificationFatal(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
        try {
          await delay(250, phase);
        } catch (delayError) {
          if (phase.exhausted) break;
          throw delayError;
        }
        continue;
      }
      if (phase.exhausted) break;
      const metadataTimeout = phase.remainingMs;
      if (metadataTimeout < minimumDriverRequestMs) break;
      const metadata = await this.driver.contextMetadataRaw(metadataTimeout).catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
        this.diagnostics.record({ phase: 'android-attachment', operation: 'context-metadata', detail: error instanceof Error ? error.message : String(error) });
        return undefined;
      });
      if (metadata !== undefined) {
        this.diagnostics.record({ phase: 'android-attachment', operation: 'context-metadata-observed', detail: metadata });
      }
      if (!contextIds.length) {
        lastError = 'Chromium context IDs are unavailable';
        try {
          await delay(250, phase);
        } catch (error) {
          if (phase.exhausted) break;
          throw error;
        }
        continue;
      }
      for (const contextId of contextIds) {
        if (phase.exhausted) break;
        try {
          const contextTimeout = phase.remainingMs;
          if (contextTimeout < minimumDriverRequestMs) break;
          await this.driver.switchContext(contextId, contextTimeout);
          const handlesTimeout = phase.remainingMs;
          if (handlesTimeout < minimumDriverRequestMs) break;
          const handles = await this.driver.windowHandles(handlesTimeout);
          const windows = handles.length ? handles : [''];
          windows.sort((left, right) => Number(right === this.selectedInstalledWindow && this.selectedInstalledWindowValid)
            - Number(left === this.selectedInstalledWindow && this.selectedInstalledWindowValid));
          for (const handle of windows) {
            const windowTimeoutMs = phase.remainingMs;
            if (windowTimeoutMs < minimumDriverRequestMs) break;
            if (handle) await this.driver.switchWindow(handle, windowTimeoutMs);
            const urlTimeout = phase.remainingMs;
            if (urlTimeout < minimumDriverRequestMs) break;
            const url = await this.driver.currentUrl(urlTimeout);
            this.lastUrl = url;
            if (!this.isExpectedOrigin(url)) {
              if (this.selectedInstalledWindowValid) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', `document origin ${url} is not ${this.origin}`);
              lastError = `window ${handle || 'current'} has origin ${url || 'unknown'}`;
              continue;
            }
            const proofTimeout = phase.remainingMs;
            if (proofTimeout < minimumDriverRequestMs) break;
            const proof = await this.installedDocumentState(proofTimeout);
            if (!this.isExpectedOrigin(proof.origin)) {
              if (this.selectedInstalledWindowValid) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', `document origin ${proof.origin} is not ${this.origin}`);
              lastError = `window ${handle || 'current'} has origin ${proof.origin || 'unknown'}`;
              continue;
            }
            if (proof.standalone !== true || proof.provider !== 'android-standalone') {
              if (this.selectedInstalledWindowValid) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', `window ${handle || 'current'} is not the installed standalone document`);
              lastError = `window ${handle || 'current'} is ${proof.provider || 'unknown'} and standalone=${proof.standalone}`;
              continue;
            }
            this.selectedInstalledWindow = handle;
            this.selectedInstalledWindowValid = true;
            return;
          }
        } catch (error) {
          if (isFatalDriverError(error) || isQualificationFatal(error)) throw error;
          lastError = error instanceof Error ? error.message : String(error);
        }
      }
      lastError ||= `no installed Chromium window for ${this.origin}`;
      const waitMs = Math.min(250, Math.max(0, phase.remainingMs - minimumDriverRequestMs));
      if (waitMs < minimumDriverRequestMs) break;
      try {
        await delay(waitMs, phase);
      } catch (error) {
        if (phase.exhausted) break;
        throw error;
      }
    }
    if (!phase.exhausted) {
      await this.driver.switchContext('NATIVE_APP').catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
      });
    }
    if (!lastError) this.budget.assertAvailable('discover installed page');
    throw new Error(`ANDROID_CONTEXT: no installed web context for ${this.origin}: ${lastError || 'phase budget expired'}`);
  }

  async readRunningIdentity(): Promise<RuntimeIdentity> {
    await this.attachToInstalledView();
    const identity = await this.driver.execute<RuntimeIdentity>(runtimeScript());
    this.lastIdentity = {
      ...identity,
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
    await requireOwnedDevice('android', this.serial);
    await this.attachToInstalledView();
    const currentUrl = await this.driver.currentUrl().catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
      return '';
    });
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
            if (isFatalDriverError(error)) throw error;
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
        if (isFatalDriverError(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250);
    }
    throw new Error(`APPIUM_AGENT: ${relayName}: ${lastError}`);
  }

  async backgroundApp(): Promise<void> {
    await requireOwnedDevice('android', this.serial);
    this.selectedInstalledWindow = '';
    this.selectedInstalledWindowValid = false;
    await this.driver.switchContext('NATIVE_APP').catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
    });
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
    await delay(500);
  }

  async relaunchInstalledApp(): Promise<void> {
    await this.launchInstalledApp();
  }

  async terminateInstalledApp(): Promise<void> {
    await requireOwnedDevice('android', this.serial);
    this.selectedInstalledWindow = '';
    this.selectedInstalledWindowValid = false;
    await this.driver.switchContext('NATIVE_APP').catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
    });
    const foreground = await this.foregroundEvidence();
    if (!this.environmentMeasurement || foreground.packageName !== this.installedPackage || !isAndroidTerminationPackage(foreground.packageName)
      || !isAndroidPersistentWebAppActivity(foreground.activity) || !foreground.pid) {
      throw new Error('ANDROID_TERMINATE: measured installed PWA process is not independently identified');
    }
    await this.environmentMeasurement.terminate(foreground.packageName, foreground.pid);
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
  }

  async showKeyboardOnComposer(): Promise<void> {
    await this.attachToInstalledView();
    let composer = await this.driver.find(css('textarea[aria-label="Prompt"]'), 5_000).catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
      return '';
    });
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
    const deadline = Date.now() + Math.min(30_000, this.budget.remainingMs);
    let lastError = '';
    while (Date.now() < deadline) {
      const remaining = deadline - Date.now();
      if (remaining < minimumDriverRequestMs) break;
      try {
        await this.attachToInstalledView(remaining);
        const findTimeout = deadline - Date.now();
        if (findTimeout < minimumDriverRequestMs) break;
        const locators = [buttonText(text), ariaLabel(text), ariaLabelPrefix(text), textLocator(text)];
        for (const locator of locators) {
          const locatorTimeout = deadline - Date.now();
          if (locatorTimeout < minimumDriverRequestMs) break;
          try {
            const element = await this.driver.findAnyOnce([locator], locatorTimeout);
            const controlTimeout = deadline - Date.now();
            if (controlTimeout < minimumDriverRequestMs) break;
            if (await this.webControlReady(element, deadline)) {
              const clickTimeout = deadline - Date.now();
              if (clickTimeout < minimumDriverRequestMs) break;
              await this.driver.click(element, clickTimeout);
              return;
            }
            lastError = `${text} is disabled, hidden, or empty`;
          } catch (error) {
            if (isFatalDriverError(error) || isQualificationFatal(error)) throw error;
            if (error instanceof WebDriverError && !isRetryableElementLookupError(error)) throw error;
            lastError = error instanceof Error ? error.message : String(error);
          }
        }
      } catch (error) {
        if (isFatalDriverError(error) || isQualificationFatal(error)) throw error;
        if (error instanceof WebDriverError && !isRetryableElementLookupError(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
      }
      const waitMs = Math.min(250, Math.max(0, deadline - Date.now() - minimumDriverRequestMs));
      if (waitMs > 0) await delay(waitMs);
    }
    throw new Error(`APPIUM_BUTTON: ${text}: ${lastError || 'control was not usable before the deadline'}`);
  }

  private async webControlReady(element: string, deadline: number): Promise<boolean> {
    const disabledTimeout = deadline - Date.now();
    if (disabledTimeout < minimumDriverRequestMs) return false;
    const disabled = await this.driver.attribute(element, 'disabled', disabledTimeout);
    if (disabled !== null && disabled !== 'false') return false;
    const ariaDisabledTimeout = deadline - Date.now();
    if (ariaDisabledTimeout < minimumDriverRequestMs) return false;
    const ariaDisabled = await this.driver.attribute(element, 'aria-disabled', ariaDisabledTimeout);
    if (ariaDisabled === 'true') return false;
    const hiddenTimeout = deadline - Date.now();
    if (hiddenTimeout < minimumDriverRequestMs) return false;
    const hidden = await this.driver.attribute(element, 'hidden', hiddenTimeout);
    if (hidden !== null && hidden !== 'false') return false;
    const ariaHiddenTimeout = deadline - Date.now();
    if (ariaHiddenTimeout < minimumDriverRequestMs) return false;
    const ariaHidden = await this.driver.attribute(element, 'aria-hidden', ariaHiddenTimeout);
    if (ariaHidden === 'true') return false;
    const rectTimeout = deadline - Date.now();
    if (rectTimeout < minimumDriverRequestMs) return false;
    const rect = await this.driver.elementRect(element, rectTimeout);
    return rect.width > 0 && rect.height > 0;
  }

  async clickDialogText(dialogId: string, text: string): Promise<void> {
    const deadline = Date.now() + Math.min(30_000, this.budget.remainingMs);
    let lastError = '';
    while (Date.now() < deadline) {
      const remaining = deadline - Date.now();
      if (remaining < minimumDriverRequestMs) break;
      try {
        await this.attachToInstalledView(remaining);
        const findTimeout = deadline - Date.now();
        if (findTimeout < minimumDriverRequestMs) break;
        const buttons = await this.driver.findAll(css(`#${dialogId} button`), findTimeout);
        for (const button of buttons) {
          const textTimeout = deadline - Date.now();
          if (textTimeout < minimumDriverRequestMs) break;
          if ((await this.driver.text(button, textTimeout)).trim() === text) {
            const clickTimeout = deadline - Date.now();
            if (clickTimeout < minimumDriverRequestMs) break;
            await this.driver.click(button, clickTimeout);
            return;
          }
        }
        lastError = `${text} is not visible in ${dialogId}`;
      } catch (error) {
        if (isFatalDriverError(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
      }
      const waitMs = Math.min(250, Math.max(0, deadline - Date.now() - minimumDriverRequestMs));
      if (waitMs > 0) await delay(waitMs);
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
      selectedInstalledWindow: this.selectedInstalledWindow,
      selectedInstalledWindowValid: this.selectedInstalledWindowValid,
      ownershipFailure: this.ownershipFailure?.snapshot(),
      lastUrl: this.lastUrl,
      lastIdentity: this.lastIdentity,
      lastCompletion: this.lastCompletion,
      lastForeground: this.lastForeground,
      lastLaunch: this.lastLaunch,
      nativeSettings: this.lastNativeSettings,
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
    await this.configureNativeSettings();
  }

  private async configureNativeSettings(): Promise<void> {
    const timeoutMs = Math.min(10_000, this.budget.remainingMs);
    if (timeoutMs < minimumDriverRequestMs) throw new Error('ANDROID_SETTINGS: insufficient time to configure native settings');
    const requested = {
      waitForIdleTimeout: ANDROID_NATIVE_IDLE_TIMEOUT_MS,
      waitForSelectorTimeout: ANDROID_NATIVE_SELECTOR_TIMEOUT_MS,
    };
    await this.driver.updateSettings(requested, timeoutMs);
    const readbackTimeout = Math.min(timeoutMs, this.budget.remainingMs);
    if (readbackTimeout < minimumDriverRequestMs) throw new Error('ANDROID_SETTINGS: insufficient time to read back native settings');
    const response = await this.driver.settings(readbackTimeout);
    const observed = response.settings && typeof response.settings === 'object'
      ? response.settings as Record<string, unknown>
      : response;
    if (Number(observed.waitForIdleTimeout) !== ANDROID_NATIVE_IDLE_TIMEOUT_MS
      || Number(observed.waitForSelectorTimeout) !== ANDROID_NATIVE_SELECTOR_TIMEOUT_MS) {
      throw new Error(`ANDROID_SETTINGS: Appium did not apply bounded native settings (${JSON.stringify(observed)})`);
    }
    this.lastNativeSettings = {
      waitForIdleTimeout: Number(observed.waitForIdleTimeout),
      waitForSelectorTimeout: Number(observed.waitForSelectorTimeout),
    };
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
    await command(adb, ['-s', this.serial, 'shell', 'am', 'start', '-a', 'com.android.settings.MORE_SECURITY_PRIVACY_SETTINGS'], 30_000);
    await this.driver.switchContext('NATIVE_APP').catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
    });
    await this.waitForSettingsPage(30_000);
    await this.captureNativeSettingsEvidence('settings-more-security-privacy');

    await this.clickNative([
      accessibility('Encryption & credentials'),
      androidTextLocator('Encryption & credentials'),
      androidTextLocator('Encryption & Credentials'),
    ], 'Encryption & credentials');
    await this.clickNative([
      accessibility('Install a certificate'),
      accessibility('Install from device storage'),
      accessibility('Install from storage'),
      androidTextLocator('Install a certificate'),
      androidTextLocator('Install from device storage'),
      androidTextLocator('Install from storage'),
    ], 'Install a certificate');
    await this.clickNative([
      accessibility('CA certificate'),
      androidTextLocator('CA certificate'),
      androidTextLocator('CA Certificate'),
    ], 'CA certificate');
    await this.clickNative([
      accessibility('Install anyway'),
      accessibility('INSTALL ANYWAY'),
      androidTextLocator('Install anyway'),
      androidTextLocator('INSTALL ANYWAY'),
    ], 'Install anyway');

    await this.openCertificatePicker();
    await this.clickNative([
      accessibility('Downloads'),
      androidTextLocator('Downloads'),
    ], 'Downloads');
    await this.clickNative([
      accessibility(remoteName),
      androidTextLocator(remoteName),
    ], remoteName);
    await delay(1_000);
    await this.verifyCertificate(remoteName, commonName);
    await command(adb, ['-s', this.serial, 'shell', 'rm', '-f', remote], 30_000);
  }

  private async openCertificatePicker(timeoutMs = 30_000): Promise<void> {
    const startedAt = Date.now();
    const deadline = startedAt + Math.min(timeoutMs, this.budget.remainingMs);
    const usableControl = { using: 'xpath', value:
      `//*[@package='${ANDROID_PICKER_PACKAGE}' and @clickable='true' and @enabled='true' and @displayed='true']`,
    };
    while (this.nativeTransactionAvailable(deadline)) {
      const foreground = await this.nativeConfirmationForeground(ANDROID_PICKER_PACKAGE, ANDROID_PICKER_ACTIVITY, deadline);
      if (foreground && this.nativeTransactionAvailable(deadline, ANDROID_PICKER_SOURCE_COMMAND_MS)) {
        const sourceStartedAt = Date.now();
        const source = await this.driver.pageSource(ANDROID_PICKER_SOURCE_COMMAND_MS);
        const sourceDurationMs = Date.now() - sourceStartedAt;
        this.diagnostics.record({ phase: 'android-certificate', operation: 'picker-hierarchy', detail: {
          elapsedMs: Date.now() - startedAt, sourceDurationMs,
        } });
        if (!this.nativeTransactionAvailable(deadline)) break;
        const elements = await this.driver.findAll(usableControl, ANDROID_NATIVE_LOOKUP_COMMAND_MS);
        for (const element of elements) {
          if (!await this.nativeControlReady(element, deadline)) continue;
          if (!await this.nativeConfirmationForeground(ANDROID_PICKER_PACKAGE, ANDROID_PICKER_ACTIVITY, deadline)) break;
          await mkdir(this.outputDir, { recursive: true });
          await writeBoundedText(join(this.outputDir, 'certificate-picker-ready-hierarchy.xml'), source);
          this.diagnostics.record({ phase: 'android-certificate', operation: 'picker-ready', detail: {
            elapsedMs: Date.now() - startedAt, sourceDurationMs,
          } });
          await this.clickNative([
            accessibility('Show roots'),
            accessibility('Open navigation drawer'),
            accessibility('Open navigation'),
          ], 'certificate picker navigation', deadline - Date.now());
          return;
        }
      }
      if (this.nativeTransactionAvailable(deadline)) await delay(250);
    }
    throw new Error('ANDROID_CERTIFICATE: focused certificate picker with usable controls was not ready within the operation budget');
  }

  private async waitForSettingsPage(timeoutMs: number): Promise<void> {
    const deadline = Date.now() + Math.min(timeoutMs, this.budget.remainingMs);
    let last = '';
    while (Date.now() < deadline) {
      const remaining = deadline - Date.now();
      if (remaining < minimumDriverRequestMs) break;
      try {
        const foreground = await this.foregroundEvidence(Math.min(5_000, remaining));
        last = `${foreground.packageName}/${foreground.activity} pid=${foreground.pid}`;
        if (foreground.packageName !== 'com.android.settings' || !/MoreSecurityPrivacySettingsActivity$/u.test(foreground.activity)) {
          await delay(Math.min(250, deadline - Date.now()));
          continue;
        }
        const sourceTimeout = deadline - Date.now();
        if (sourceTimeout < minimumDriverRequestMs) break;
        const source = await this.driver.pageSource(sourceTimeout);
        if (/More security|Security & privacy/iu.test(source)) return;
        last = 'Settings activity hierarchy did not expose its landing page';
      } catch (error) {
        if (isFatalDriverError(error)) throw error;
        last = error instanceof Error ? error.message : String(error);
      }
      const waitMs = Math.min(250, deadline - Date.now());
      if (waitMs <= 0) break;
      await delay(waitMs);
    }
    throw new Error(`ANDROID_CERTIFICATE: Settings landing page was not ready (${last || 'no activity evidence'})`);
  }

  private async captureNativeSettingsEvidence(name: string): Promise<void> {
    await mkdir(this.outputDir, { recursive: true });
    const capture = async (operation: string, action: () => Promise<void>): Promise<void> => {
      try {
        await action();
      } catch (error) {
        if (isFatalDriverError(error)) {
          await this.captureNativeSettingsAdbEvidence(name);
          throw error;
        }
        this.diagnostics.record({ phase: 'android-certificate', operation, detail: error instanceof Error ? error.message : String(error) });
      }
    };
    await capture('settings-hierarchy', async () => {
      const source = await this.driver.pageSource(Math.min(5_000, this.budget.remainingMs));
      await writeBoundedText(join(this.outputDir, `${name}-hierarchy.xml`), source);
    });
    await capture('settings-screenshot', async () => {
      const screenshot = Buffer.from(await this.driver.screenshot(Math.min(5_000, this.budget.remainingMs)), 'base64');
      if (screenshot.byteLength <= 20 * 1024 * 1024) await writeFile(join(this.outputDir, `${name}.png`), screenshot, { mode: 0o600 });
    });
    await capture('settings-activity', async () => {
      await writeSanitizedJson(join(this.outputDir, `${name}-activity.json`), await this.foregroundEvidence(Math.min(5_000, this.budget.remainingMs)));
    });
  }

  private async captureNativeSettingsAdbEvidence(name: string): Promise<void> {
    try {
      await mkdir(this.outputDir, { recursive: true });
      const adb = process.env.ADB || 'adb';
      const captures: Array<[string, string[]]> = [
        ['logcat', ['-s', this.serial, 'logcat', '-d', '-t', '600']],
        ['activity-adb', ['-s', this.serial, 'shell', 'dumpsys', 'activity', 'activities']],
        ['window-adb', ['-s', this.serial, 'shell', 'dumpsys', 'window', 'windows']],
      ];
      for (const [suffix, args] of captures) {
        const output = await commandOutput(adb, args, 10_000).catch((error) => error instanceof Error ? error.message : String(error));
        await writeBoundedText(join(this.outputDir, `${name}-${suffix}.log`), output);
      }
    } catch (error) {
      this.diagnostics.record({ phase: 'android-certificate', operation: 'settings-adb-evidence', detail: error instanceof Error ? error.message : String(error) });
    }
  }

  private async nativeElementEnabled(element: string, deadline: number): Promise<boolean> {
    for (const attribute of ['enabled', 'displayed']) {
      if (!this.nativeTransactionAvailable(deadline)) return false;
      if (await this.driver.attribute(element, attribute, ANDROID_NATIVE_LOOKUP_COMMAND_MS) !== 'true') return false;
    }
    return true;
  }

  private async clickNative(locators: Locator[], description: string, timeoutMs = 30_000): Promise<void> {
    const deadline = Date.now() + Math.min(timeoutMs, this.budget.remainingMs);
    let lastError = '';
    while (this.nativeTransactionAvailable(deadline)) {
      try {
        const element = await this.findNative(locators, deadline - Date.now());
        if (!await this.nativeElementEnabled(element, deadline)) {
          lastError = `${description} is not enabled and displayed`;
          continue;
        }
        if (!this.nativeTransactionAvailable(deadline)) break;
        await this.driver.click(element, ANDROID_NATIVE_LOOKUP_COMMAND_MS);
        return;
      } catch (error) {
        if (isFatalDriverError(error) || !isRetryableElementLookupError(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
      }
      const waitMs = Math.min(250, deadline - Date.now());
      if (waitMs <= 0) break;
      await delay(waitMs);
    }
    throw new Error(`ANDROID_CERTIFICATE: ${description}: ${lastError || 'insufficient time for a complete native transaction'}`);
  }

  private async findNative(locators: Locator[], timeoutMs: number): Promise<string> {
    const deadline = Date.now() + Math.min(timeoutMs, this.budget.remainingMs);
    let lastError = 'insufficient time for a complete native lookup';
    let scrolls = 0;
    let size = { width: 1_080, height: 2_400 };
    const windowTimeout = Math.min(2_000, timeoutMs, this.budget.remainingMs);
    if (windowTimeout >= minimumDriverRequestMs) {
      size = await this.driver.windowSize(windowTimeout).catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
        return size;
      });
    }
    while (this.nativeTransactionAvailable(deadline)) {
      for (const locator of locators) {
        if (!this.nativeTransactionAvailable(deadline)) break;
        try {
          return await this.driver.findAnyOnce([locator], ANDROID_NATIVE_LOOKUP_COMMAND_MS);
        } catch (error) {
          if (isFatalDriverError(error) || !isRetryableElementLookupError(error)) throw error;
          lastError = error instanceof Error ? error.message : String(error);
        }
      }
      const afterLookup = deadline - Date.now();
      if (afterLookup < minimumDriverRequestMs || scrolls >= ANDROID_NATIVE_SCROLL_LIMIT) break;
      if (afterLookup < ANDROID_NATIVE_SCROLL_COMMAND_MS) break;
      const scrollTimeout = Math.min(ANDROID_NATIVE_SCROLL_COMMAND_MS, afterLookup);
      if (scrollTimeout < minimumDriverRequestMs) break;
      try {
        await this.driver.mobile('scrollGesture', {
          left: 0,
          top: 100,
          width: size.width,
          height: Math.max(1, size.height - 200),
          direction: 'down',
          percent: 0.75,
        }, scrollTimeout);
      } catch (error) {
        if (isFatalDriverError(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
        if (isCommandAdmissionError(error)) break;
        break;
      }
      scrolls += 1;
      const waitMs = Math.min(250, Math.max(0, deadline - Date.now() - minimumDriverRequestMs));
      if (waitMs > 0) await delay(waitMs);
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
    await this.driver.switchContext('NATIVE_APP').catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
    });
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
    const probeScript = `return fetch(location.href, { cache: 'no-store' }).then(async response => ({
      httpStatus: response.status,
      url: response.url,
      body: (await response.text()).slice(0, 8192),
    }));`;
    while (!phase.exhausted) {
      try {
        phase.assertAvailable('verify fixture certificate');
        const contextsTimeout = phase.remainingMs;
        if (contextsTimeout < minimumDriverRequestMs) break;
        const webContext = (await this.driver.contexts(contextsTimeout)).find((context) => context !== 'NATIVE_APP');
        if (!webContext) throw new Error('Chrome web context is unavailable');
        const contextTimeout = phase.remainingMs;
        if (contextTimeout < minimumDriverRequestMs) break;
        await this.driver.switchContext(webContext, contextTimeout);
        const navigationTimeout = phase.remainingMs;
        if (navigationTimeout < minimumDriverRequestMs) break;
        await this.driver.navigate(`${this.origin}/version.json`, navigationTimeout);
        const executeTimeout = phase.remainingMs;
        if (executeTimeout < minimumDriverRequestMs) break;
        const observed = await this.driver.execute<{ httpStatus: number; url: string; body: string }>(probeScript, [], executeTimeout);
        const responseDetail = `status=${observed.httpStatus} url=${observed.url} body=${observed.body.slice(0, 200)}`;
        if (/ERR_CERT|NET::ERR|privacy error|not private/iu.test(observed.body)) {
          lastError = responseDetail;
        } else {
          const metadata = JSON.parse(observed.body) as Record<string, unknown>;
          if (observed.httpStatus === 200 && this.isExpectedOrigin(observed.url)
            && typeof metadata.version === 'string'
            && Number.isInteger(Number(metadata.assets))
            && Number(metadata.assets) > 0) return;
          lastError = responseDetail;
        }
      } catch (error) {
        if (error instanceof PhaseBudgetError) {
          if (lastError) break;
          throw error;
        }
        if (isFatalDriverError(error)) throw error;
        const detail = error instanceof Error ? error.message : String(error);
        if (!isCommandAdmissionError(error) || !lastError) lastError = detail;
        if (isCommandAdmissionError(error) || phase.exhausted) break;
      }
      if (phase.exhausted) break;
      const waitMs = Math.min(250, Math.max(0, phase.remainingMs - minimumDriverRequestMs));
      if (waitMs < minimumDriverRequestMs) break;
      try {
        await delay(waitMs, phase);
      } catch (error) {
        if (phase.exhausted) break;
        throw error;
      }
    }
    throw new Error(`ANDROID_CERTIFICATE: fixture HTTPS response identity was not trusted (${lastError || 'no response observed'})`);
  }

  private async foregroundEvidence(timeoutMs = 10_000, budget?: PhaseBudget): Promise<{ packageName: string; activity: string; focusedPackage: string; focusedActivity: string; pid: string; raw: string }> {
    const output = await commandOutput(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'dumpsys', 'activity', 'activities'], timeoutMs, {
      budget,
      label: 'read Android foreground activity',
    });
    const component = output.match(/(?:mResumedActivity|ResumedActivity): ActivityRecord\{[^}]+\s([A-Za-z0-9_.]+)\/([A-Za-z0-9_.$]+)/u);
    const packageName = component?.[1] || '';
    const processLine = packageName
      ? output.split(/\r?\n/u).find((line) => line.includes(`:${packageName}/`)) || ''
      : '';
    const pid = output.match(/(?:mResumedActivity|ResumedActivity): ActivityRecord\{[^}]+\s+pid=(\d+)/u)?.[1]
      || processLine.match(/ProcessRecord\{[^}\n]*\s(\d+):/u)?.[1]
      || '';
    const focus = output.match(/mCurrentFocus=Window\{[^}\n]+\s([A-Za-z0-9_.]+)\/([A-Za-z0-9_.$]+)/u);
    return {
      packageName, activity: component?.[2] || '',
      focusedPackage: focus?.[1] || '', focusedActivity: focus?.[2] || '',
      pid, raw: output.slice(-20_000),
    };
  }

  private async isInstalledTargetForeground(timeoutMs = 10_000, budget?: PhaseBudget): Promise<boolean> {
    if (!this.installedTarget) return false;
    const foreground = await this.foregroundEvidence(timeoutMs, budget);
    this.lastForeground = foreground;
    const packageMatches = foreground.packageName === this.installedTarget.packageName
      || /webapk/iu.test(foreground.packageName);
    const matches = packageMatches && isAndroidPersistentWebAppActivity(foreground.activity);
    if (!matches && this.selectedInstalledWindowValid) {
      this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', `installed window lost its native provider (${foreground.packageName || 'unknown'}/${foreground.activity || 'unknown'})`);
    }
    return matches;
  }

  private async waitForInstalledTarget(timeoutMs: number, launchError?: unknown): Promise<void> {
    const phase = this.budget.phaseView('android-launch-proof', timeoutMs);
    let last = launchError instanceof Error ? launchError.message : '';
    while (!phase.exhausted) {
      phase.assertAvailable('verify installed launch');
      try {
        const evidence = await this.foregroundEvidence(Math.min(5_000, phase.remainingMs), phase);
        this.lastForeground = evidence;
        last = `${evidence.packageName}/${evidence.activity} pid=${evidence.pid}`;
        this.diagnostics.record({ phase: 'android-launch-proof', operation: 'foreground-observation', detail: { packageName: evidence.packageName, activity: evidence.activity, pid: evidence.pid } });
        if (isAndroidPersistentWebAppActivity(evidence.activity)
          && (evidence.packageName === this.installedTarget?.packageName || /webapk/iu.test(evidence.packageName))) {
          this.installedPackage = evidence.packageName;
          return;
        }
      } catch (error) {
        if (isFatalDriverError(error)) throw error;
        last = error instanceof Error ? error.message : String(error);
      }
      try {
        await delay(250, phase);
      } catch (error) {
        if (phase.exhausted) break;
        throw error;
      }
    }
    throw new Error(`ANDROID_TARGET: signed installed target was not foreground (${last || 'none'})`, { cause: launchError });
  }

  private isExpectedOrigin(value: string): boolean {
    try {
      return new URL(value).origin === new URL(this.origin).origin;
    } catch {
      return false;
    }
  }

  private async installedDocumentState(timeoutMs: number): Promise<{ origin: string; standalone: boolean; provider: string }> {
    return this.driver.execute<{ origin: string; standalone: boolean; provider: string }>(`return {
      origin: location.origin,
      standalone: window.matchMedia('(display-mode: standalone)').matches || navigator.standalone === true,
      provider: window.matchMedia('(display-mode: standalone)').matches || navigator.standalone === true ? 'android-standalone' : 'browser',
    };`, [], timeoutMs);
  }

  private assertOwnershipClear(): void {
    if (this.ownershipFailure) throw this.ownershipFailure;
  }

  private failOwnership(code: string, detail: string): never {
    this.ownershipFailure ||= qualificationFatal(code, detail, 'ownership');
    throw this.ownershipFailure;
  }

  private async currentForegroundPackage(timeoutMs = 10_000, budget?: PhaseBudget): Promise<string> {
    return (await this.foregroundEvidence(timeoutMs, budget)).packageName;
  }
}
