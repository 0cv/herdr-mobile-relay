import { X509Certificate } from 'node:crypto';
import { AndroidStartupLog } from '../support/android-startup-log';
import { AndroidProductRecorder } from '../support/android-product';
import type { MeasurementIdentity } from '../support/mobile-result';
import { decodeRetainedInspection, type RetainedNativeIdentity } from '../support/android-retained-decoder';
import { createAdbInspection } from '../android-appium/adb-inspection.cjs';
import { acquireKernelCapability, namespaceForCapability, validateNamespaceStatus, sameKernelCapability, type KernelCapability } from '../android-appium/kernel-namespace.cjs';
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
  isFatalDriverError as isFatalAppiumError,
  isRetryableElementLookupError,
  minimumDriverRequestMs,
  textLocator,
  type Locator,
  WebDriverError,
} from '../support/webdriver';
import { runtimeScript, updateCompletionScript, type MobilePlatform, type PlatformOptions, type UpdateCompletionEvidence } from './types';

function isFatalDriverError(error: unknown): boolean {
  return isFatalAppiumError(error) || isQualificationFatal(error);
}

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
  intentId?: string;
  action?: string;
  packageName?: string;
  component?: string;
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
      intentId: shortcutField(block, CHROME_WEBAPP_ID),
      action: block.match(/\bact=([^\s}]+)/u)?.[1],
      packageName: block.match(/\bpkg=([^\s}]+)/u)?.[1],
      component: block.match(/\bcmp=([^\s}]+)/u)?.[1],
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
    'appium:androidDeviceSocket': 'chrome_devtools_remote',
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
  private readonly productRecorder = new AndroidProductRecorder();
  private lastProductInspection = 0;
  private coldProductHandoff = false;

  activateProductRecorder(identity: MeasurementIdentity): void {
    this.assertRetainedSession();
    this.productRecorder.activate(identity, this.retainedOwner!, this.retainedOwner!);
  }

  productCheckpoint(name: string): void {
    this.assertRetainedSession();
    this.productRecorder.checkpoint(name, this.retainedOwner!);
  }
  readonly driver: AppiumClient;
  private readonly serial: string;
  private readonly origin: string;
  private readonly outputDir: string;
  private readonly budget: PhaseBudget;
  private readonly diagnostics: DiagnosticRecorder;
  private startupLog?: AndroidStartupLog;
  private setupAttempted = false;
  private installedPackage = '';
  private initialLaunchAttempted = false;
  private retainedOwner?: RetainedNativeIdentity & { driver: AppiumClient; assertSession: () => void };
  private kernelReader?: ReturnType<typeof createAdbInspection>;
  private kernelCapability?: KernelCapability;
  private installedTarget?: { packageName: string; activity: string; shortcut: AndroidChromeShortcut };
  private selectedInstalledWindow = '';
  private selectedInstalledWindowValid = false;
  private inspectedDocument = '';
  private inspectedNavigationId = '';
  private ownershipFailure?: QualificationFatalError;
  private lifecycleFailure?: Error;
  private lifecycleActive = false;
  private lifecycleSettled?: Promise<void>;
  private settleLifecycle?: () => void;
  private ownedSessionClose?: Promise<void>;
  private ownedResourcesStop?: Promise<void>;
  private lifecycleBinding?: () => void;
  private coldHandoff?: {
    owner: NonNullable<AndroidPlatform['retainedOwner']>;
    target: NonNullable<AndroidPlatform['installedTarget']>;
    measurement: AndroidEnvironmentMeasurement;
    measurementId: string;
  };
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
    this.assertOwnershipClear();
    if (this.setupAttempted || this.startupLog || this.retainedOwner || this.driver.snapshot().sessionId) {
      this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'fresh setup requires an unused platform');
    }
    this.beginLifecycle();
    this.setupAttempted = true;
    this.guardRetainedCommands();
    try {
      await this.prepareFreshDevice();
      this.assertDriverClear();
    } catch (error) {
      this.failLifecycle(error);
    } finally {
      this.endLifecycle();
    }
  }

  private async prepareFreshDevice(): Promise<void> {
    if (!/^emulator-\d+$/.test(this.serial)) throw new Error('ANDROID_TARGET: refusing a non-emulator or ambiguous device');
    await requireOwnedDevice('android', this.serial);
    this.assertDriverClear();
    if (this.startupLog) throw new Error('ANDROID_TARGET: startup diagnostic already owned');
    this.startupLog = new AndroidStartupLog(this.outputDir);
    this.startupLog.start(this.serial);
    await this.startupLog.waitForHandshake();
    this.assertDriverClear();
    const devices = await commandOutput(process.env.ADB || 'adb', ['devices']);
    this.assertDriverClear();
    const matching = devices.split(/\r?\n/).filter((line) => line.startsWith(`${this.serial}\t`));
    if (matching.length !== 1 || !matching[0].endsWith('\tdevice')) throw new Error(`ANDROID_TARGET: ${this.serial} is not the only ready emulator`);
    const adb = process.env.ADB || 'adb';
    await command(adb, ['-s', this.serial, 'wait-for-device'], 60_000);
    this.assertDriverClear();
    await command(adb, ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME']);
    this.assertDriverClear();
    await command(adb, ['-s', this.serial, 'shell', 'pm', 'clear', 'com.android.chrome']).catch(() => undefined);
    this.assertDriverClear();
    const installedPackages = await commandOutput(adb, ['-s', this.serial, 'shell', 'pm', 'list', 'packages']);
    this.assertDriverClear();
    for (const packageName of installedPackages.split(/\r?\n/u).map((line) => line.replace(/^package:/u, '').trim()).filter((value) => /webapk|herdr/iu.test(value))) {
      await command(adb, ['-s', this.serial, 'uninstall', packageName]).catch(() => undefined);
      this.assertDriverClear();
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
    this.assertDriverClear();
    await this.configureNativeSettings();
    await this.installCertificate();
    this.assertDriverClear();
    await this.closeOwnedSession();
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
    if (this.budget.remainingMs < 41_000) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'insufficient navigation and retained postcheck allowance');
    this.productRecorder.transition('pairing-navigation', this.retainedOwner!);
    const operation = this.productRecorder.begin('mutation', this.retainedOwner!, this.lastProductInspection);
    await this.driver.navigate(url, 30_000);
    await this.attachToInstalledView();
    this.productRecorder.end('mutation', this.retainedOwner!, operation, this.lastProductInspection);
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
    await this.observeChromeConfirmation(confirm);

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
    this.assertOwnershipClear();
    if (this.initialLaunchAttempted) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'initial signed launch was already attempted');
    this.beginLifecycle();
    this.initialLaunchAttempted = true;
    this.guardRetainedCommands();
    try {
      await this.assertRetainedOwner();
      await this.driver.switchContext('NATIVE_APP');
      this.assertRetainedSession();
      await this.driver.switchContext('CHROMIUM');
      this.assertRetainedSession();
      const current = await this.driver.currentWindow();
      const handles = await this.driver.windowHandles();
      if (!current || !handles.includes(current)) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'original current window is unavailable');
      const url = await this.driver.currentUrl();
      if (!url) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'original current document is unavailable');
      this.productRecorder.transition('initial-install', this.retainedOwner!);
      await this.launchInstalledTarget('initial');
    } catch (error) {
      this.failLifecycle(error);
    } finally {
      this.endLifecycle();
    }
  }

  private async launchInstalledTarget(mode: 'initial' | 'warm' | 'cold'): Promise<void> {
    const selectionTail = mode === 'warm' ? 13_000 : 26_000;
    const attachmentTail = (mode === 'cold' ? 70_000 + 20_000 : 0) + 5_000 + selectionTail;
    const contextTail = mode === 'initial' ? 2_000 : 0;
    const intentTail = 5_000 + 10_000 + 10_000 + attachmentTail;
    this.requireLifecycleAllowance(30_000 + contextTail + 5_000 + 5_000 + 30_000 + intentTail);
    await requireOwnedDevice('android', this.serial);
    this.requireLifecycleAllowance(30_000 + contextTail + 5_000 + 5_000 + 30_000 + intentTail);
    const shortcut = await this.waitForChromeShortcut(30_000, contextTail + 5_000 + 5_000 + 30_000 + intentTail,
      mode === 'initial' ? undefined : this.installedTarget?.shortcut);
    this.requireLifecycleAllowance(5_000 + 5_000 + 30_000 + intentTail);
    if (mode === 'initial') {
      this.installedTarget = Object.freeze({
        packageName: 'com.android.chrome',
        activity: CHROME_WEBAPP_COMPONENT.split('/')[1],
        shortcut: Object.freeze(shortcut),
      });
    }
    this.lastLaunch = { shortcut: this.shortcutEvidence(shortcut), transitions: [] };
    this.diagnostics.record({ phase: 'android-launch', operation: 'shortcut-observed', detail: this.lastLaunch.shortcut });
    if (mode === 'initial') {
      this.requireLifecycleAllowance(2_000 + 5_000 + 5_000 + 30_000 + intentTail);
      await this.driver.switchContext('NATIVE_APP', 2_000);
    }
    await this.recordLaunchForeground('before-command');
    this.requireLifecycleAllowance(5_000 + 30_000 + intentTail);
    if (mode === 'cold') await this.assertChromeStopped();
    else await this.assertRetainedOwner();
    this.requireLifecycleAllowance(30_000 + intentTail);
    await this.launchChromeShortcut(shortcut, intentTail);
    this.requireLifecycleAllowance(intentTail);
    await this.recordLaunchForeground('after-command');
    this.requireLifecycleAllowance(10_000 + 10_000 + attachmentTail);
    await this.waitForInstalledTarget(30_000, undefined, 10_000 + attachmentTail);
    this.requireLifecycleAllowance(10_000 + attachmentTail);
    await this.waitForChromeDevTools(30_000, attachmentTail);
    this.requireLifecycleAllowance(attachmentTail);
    if (mode === 'cold') await this.createChromeSession(true, 26_000);
    else await this.assertRetainedOwner();
    this.requireLifecycleAllowance(selectionTail);
    await this.inspectInstalledView();
    this.requireLifecycleAllowance(0);
  }

  private beginLifecycle(allowHandoff = false): void {
    this.assertOwnershipClear();
    if (this.lifecycleActive || (!allowHandoff && this.coldHandoff)) {
      this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'overlapping or incompatible installed lifecycle');
    }
    this.assertDriverClear();
    this.lifecycleActive = true;
    this.lifecycleSettled = new Promise(resolve => { this.settleLifecycle = resolve; });
  }

  private endLifecycle(): void {
    this.lifecycleBinding = undefined;
    this.lifecycleActive = false;
    this.settleLifecycle?.();
    this.settleLifecycle = undefined;
    this.lifecycleSettled = undefined;
  }

  private assertDriverClear(): void {
    this.assertOwnershipClear();
    const snapshot = this.driver.snapshot();
    if (snapshot.firstFatal || snapshot.unusable) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'driver has a terminal lifecycle failure');
  }

  private requireLifecycleAllowance(milliseconds: number): void {
    this.assertDriverClear();
    if (this.budget.exhausted || this.budget.remainingMs < milliseconds) {
      this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'insufficient installed lifecycle tail allowance');
    }
  }

  private failLifecycle(error: unknown): never {
    this.coldHandoff = undefined;
    this.lifecycleFailure ||= this.ownershipFailure || (error instanceof Error ? error : new Error(String(error)));
    throw this.lifecycleFailure;
  }

  private async assertChromeStopped(): Promise<void> {
    this.requireLifecycleAllowance(5_000);
    try {
      await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'pidof', 'com.android.chrome'], 5_000, { budget: this.budget });
    } catch (error) {
      this.assertDriverClear();
      if (error instanceof CommandError && error.exitCode === 1 && !error.timedOut && !error.signal
        && !error.stdout.trim() && !error.stderr.trim()) return;
      throw error;
    }
    this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'Chrome reappeared before the owned cold launch');
  }

  async assertStandalone(origin: string): Promise<RuntimeIdentity> {
    const identity = await this.readRunningIdentity();
    assertStandalone(identity, origin);
    return identity;
  }

  private async observeChromeConfirmation(element: string): Promise<void> {
    const phase = this.budget.phaseView('android-chrome-confirmation', 50_000);
    const readMs = 2_000;
    const actionMs = 5_000;
    const requiredMs = 10 * readMs + 2 * ANDROID_NATIVE_LOOKUP_COMMAND_MS + actionMs;
    let remainingAllowance = requiredMs;
    const observe = async <T>(allowance: number, operation: () => Promise<T>): Promise<T> => {
      if (phase.remainingMs < remainingAllowance) throw new Error('ANDROID_CHROME: insufficient confirmation observation allowance');
      remainingAllowance -= allowance;
      return operation();
    };
    const identity: Record<string, unknown> = { element };
    for (const attribute of ['class', 'resource-id', 'package', 'text', 'enabled', 'displayed', 'clickable']) {
      identity[attribute] = await observe(readMs, () => this.driver.attribute(element, attribute, readMs));
    }
    const rect = await observe(readMs, () => this.driver.elementRect(element, readMs));
    const size = await observe(readMs, () => this.driver.windowSize(readMs));
    const foreground = await observe(ANDROID_NATIVE_LOOKUP_COMMAND_MS, () => this.foregroundEvidence(ANDROID_NATIVE_LOOKUP_COMMAND_MS));
    this.diagnostics.record({ phase: 'android-chrome-confirmation', operation: 'selected-control', detail: { ...identity, rect, size, foreground } });
    if (identity.class !== 'android.widget.Button' || identity.package !== 'com.android.chrome'
      || !['Add', 'Install'].includes(String(identity.text))
      || ['enabled', 'displayed', 'clickable'].some(attribute => identity[attribute] !== 'true')
      || foreground.packageName !== 'com.android.chrome' || foreground.focusedPackage !== 'com.android.chrome'
      || ![rect.x, rect.y, rect.width, rect.height, size.width, size.height].every(Number.isFinite)
      || rect.x < 0 || rect.y < 0 || rect.width <= 0 || rect.height <= 0
      || rect.x + rect.width > size.width || rect.y + rect.height > size.height) {
      throw new Error('ANDROID_CHROME: selected confirmation control is not a ready Chrome button');
    }
    const matches = await observe(readMs, () => this.driver.command<unknown>('/elements', 'POST', {
      using: 'xpath', value: "//android.widget.Button[@package='com.android.chrome' and (@text='Add' or @text='Install')]",
    }, readMs));
    if (!Array.isArray(matches) || matches.length !== 1
      || matches[0]?.['element-6066-11e4-a52e-4f735466cecf'] !== element) {
      throw new Error('ANDROID_CHROME: confirmation control is ambiguous or replaced');
    }
    if (phase.remainingMs < actionMs + ANDROID_NATIVE_LOOKUP_COMMAND_MS) throw new Error('ANDROID_CHROME: insufficient click and post-observation allowance');
    await observe(actionMs, () => this.driver.click(element, actionMs));
    const afterForeground = await observe(ANDROID_NATIVE_LOOKUP_COMMAND_MS, () => this.foregroundEvidence(ANDROID_NATIVE_LOOKUP_COMMAND_MS));
    this.diagnostics.record({ phase: 'android-chrome-confirmation', operation: 'settled-click-observation', detail: {
      foreground: afterForeground,
      chromeRemainsForeground: afterForeground.packageName === 'com.android.chrome',
      launcherForeground: afterForeground.packageName === ANDROID_LAUNCHER_PACKAGE && afterForeground.focusedPackage === ANDROID_LAUNCHER_PACKAGE,
    } });
  }

  private nativeTransactionAvailable(deadline: number, timeoutMs = ANDROID_NATIVE_LOOKUP_COMMAND_MS): boolean {
    this.assertOwnershipClear();
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

  private async launchChromeShortcut(shortcut?: AndroidChromeShortcut, reserveMs = 0): Promise<CommandResult> {
    const launchShortcut = shortcut || await this.waitForChromeShortcut(30_000, reserveMs);
    const phase = this.budget.phaseView('android-signed-intent', 30_000, reserveMs);
    this.requireLifecycleAllowance(30_000 + reserveMs);
    const args = androidChromeShortcutArgs(this.serial, launchShortcut);
    try {
      const result = await command(process.env.ADB || 'adb', args, 30_000, { budget: phase });
      phase.assertAvailable('signed intent settlement');
      this.requireLifecycleAllowance(reserveMs);
      if (result.stderr.trim() || !/^\s*Status:\s*ok\s*$/mu.test(result.stdout) || /(?:^|\n)\s*Error:/u.test(result.stdout)) {
        throw new Error('ANDROID_LAUNCH: signed launch did not report a successful status');
      }
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
      const evidence = await this.foregroundEvidence(5_000, this.budget);
      this.assertOwnershipClear();
      this.budget.assertAvailable('foreground settlement');
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
      if (this.retainedOwner) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', String(error));
      if (isFatalDriverError(error)) throw error;
      const detail = { state: label, error: error instanceof Error ? error.message : String(error) };
      this.lastLaunch?.transitions.push(detail);
      this.diagnostics.record({ phase: 'android-launch', operation: 'foreground-transition', detail });
    }
  }

  private async waitForChromeShortcut(timeoutMs: number, reserveMs = 0, expected?: AndroidChromeShortcut): Promise<AndroidChromeShortcut> {
    const adb = process.env.ADB || 'adb';
    const phase = this.budget.phaseView('android-shortcut', timeoutMs, reserveMs);
    const deadline = Date.now() + phase.remainingMs;
    let lastError = 'Chrome did not publish a matching Herdr Relay ShortcutInfo';
    while (Date.now() < deadline) {
      try {
        const output = await commandOutput(adb, [
          '-s', this.serial, 'shell', 'cmd', 'shortcut', 'get-shortcuts',
          '--user', '0', '--flags', '15', 'com.android.chrome',
        ], Math.min(30_000, phase.remainingMs), { budget: phase });
        phase.assertAvailable('shortcut settlement');
        this.assertOwnershipClear();
        const expectedOrigin = new URL(this.origin).origin;
        const shortcuts = parseAndroidChromeShortcuts(output).filter((candidate) => {
          const labels = [candidate.shortLabel, candidate.name];
          if (!labels.some((label) => /herdr(?: mobile)? relay/iu.test(label))) return false;
          try {
            return new URL(candidate.url).origin === expectedOrigin
              && new URL(candidate.scope).origin === expectedOrigin;
          } catch {
            return false;
          }
        });
        if (shortcuts.length > 1) this.failOwnership('ANDROID_SHORTCUT', 'signed installed shortcut is ambiguous');
        const shortcut = shortcuts[0];
        if (shortcut) {
          const url = new URL(shortcut.url);
          const scope = new URL(shortcut.scope);
          if (shortcut.intentId !== shortcut.id || shortcut.action !== CHROME_WEBAPP_ACTION
            || (shortcut.packageName !== 'com.android.chrome' && shortcut.component !== CHROME_WEBAPP_COMPONENT)
            || (shortcut.component !== undefined && shortcut.component !== CHROME_WEBAPP_COMPONENT)
            || !/^[A-Za-z0-9+/]+={0,2}$/u.test(shortcut.mac)
            || url.protocol !== 'https:' || url.username || url.password || scope.username || scope.password
            || scope.search || scope.hash || !url.pathname.startsWith(scope.pathname)
            || [shortcut.source, shortcut.displayMode, shortcut.orientation].some(value => value !== undefined && !/^-?\d+$/u.test(value))) {
            this.failOwnership('ANDROID_SHORTCUT', 'signed installed shortcut fields are invalid');
          }
          if (expected && JSON.stringify(shortcut) !== JSON.stringify(expected)) this.failOwnership('ANDROID_SHORTCUT', 'signed installed shortcut identity changed');
          return shortcut;
        }
        if (expected) this.failOwnership('ANDROID_SHORTCUT', 'retained signed installed shortcut is missing');
      } catch (error) {
        this.assertOwnershipClear();
        if (expected || isQualificationFatal(error) || phase.exhausted) throw error;
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250, phase);
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
    this.beginLifecycle();
    try {
      await this.inspectInstalledView(timeoutMs);
    } catch (error) {
      this.failLifecycle(error);
    } finally {
      this.endLifecycle();
    }
  }

  private async inspectInstalledView(timeoutMs = 30_000): Promise<void> {
    this.assertOwnershipClear();
    this.guardRetainedCommands();
    try {
      if (!this.installedTarget) throw new Error('No verified installed launch');
      const phase = this.budget.phaseView('android-attachment', timeoutMs);
      const routeMs = 10_000;
      const httpMs = 1_000;
      const switchMs = 2_000;
      const required = this.selectedInstalledWindowValid ? routeMs + httpMs : 2 * (routeMs + httpMs) + switchMs;
      if (phase.remainingMs < required + (this.driver.snapshot().selectedContext === 'CHROMIUM' ? 0 : switchMs)) {
        throw new Error('Insufficient parent allowance for complete retained inspection');
      }
      this.assertRetainedSession();
      if (this.driver.snapshot().selectedContext !== 'CHROMIUM') await this.driver.switchContext('CHROMIUM', switchMs);
      const inspect = async (selectedHandle: string) => {
        this.assertRetainedSession();
        if (phase.remainingMs < routeMs + httpMs) throw new Error('Retained inspection parent allowance exhausted');
        const startedAt = Date.now();
        const deadline = startedAt + routeMs;
        const value = await this.driver.execute('mobile: inspectRetainedChromeTargets', [{ deadline }], routeMs + httpMs);
        this.assertRetainedSession();
        const decoded = decodeRetainedInspection(value, this.retainedOwner!, {
          startedAt, deadline, finishedAt: Date.now(), serial: this.serial, scope: this.installedTarget!.shortcut.scope,
          selectedHandle, assertSession: session => this.driver.assertSessionIdentity(session),
        });
        this.lastProductInspection = this.productRecorder.inspection(this.retainedOwner!, decoded.result, startedAt, Date.now(), deadline);
        const { original, before, after, processAssociation } = decoded.result;
        this.diagnostics.record({ phase: 'android-attachment', operation: 'bounded-retained-inspection', detail: {
          pid: original.pid, startTime: original.startTime, bootId: original.bootId, namespace: original.namespace, kernelCapability: original.kernelCapability,
          processAssociation: { kind: processAssociation.kind,
            before: { pid: processAssociation.before.pid, requestId: processAssociation.before.requestId, connectionId: processAssociation.before.connectionId, startedAt: processAssociation.before.startedAt, completedAt: processAssociation.before.completedAt },
            after: { pid: processAssociation.after.pid, requestId: processAssociation.after.requestId, connectionId: processAssociation.after.connectionId, startedAt: processAssociation.after.startedAt, completedAt: processAssociation.after.completedAt } },
          startedAt, deadline, original: { startedAt: original.startedAt, finishedAt: original.finishedAt },
          bounds: [before, after].map(snapshot => ({ startedAt: snapshot.startedAt, finishedAt: snapshot.finishedAt,
            native: [snapshot.nativeBefore, snapshot.native].map(native => ({ pid: native.pid, startTime: native.startTime,
              bootId: native.bootId, namespace: native.namespace, kernelCapability: native.kernelCapability, activity: native.activity, provider: native.provider,
              startedAt: native.startedAt, finishedAt: native.finishedAt })) })),
        } });
        return decoded;
      };
      const bindInstalledWindow = (decoded: ReturnType<typeof decodeRetainedInspection>): void => {
        this.assertDriverClear();
        phase.assertAvailable('retained selection settlement');
        const { after } = decoded.result;
        this.selectedInstalledWindow = decoded.result.selectedHandle;
        this.selectedInstalledWindowValid = true;
        this.lastForeground = { packageName: 'com.android.chrome', activity: after.native.activity, pid: after.native.pid };
        this.lastUrl = after.document.href;
        this.inspectedDocument = JSON.stringify([decoded.result.selectedHandle, after.document.href, after.document.origin, after.document.backendNodeId, after.document.timeOrigin]);
        this.inspectedNavigationId = String(after.document.timeOrigin);
      };
      const first = await inspect(this.selectedInstalledWindowValid ? this.selectedInstalledWindow : '');
      if (this.selectedInstalledWindowValid) {
        bindInstalledWindow(first);
        return;
      }
      if (first.result.phase === 'installed-selected') {
        bindInstalledWindow(first);
        return;
      }
      const provisional = first.result.observations.find((entry) => entry.targetId === first.candidate)?.document;
      if (!provisional) throw new Error('Missing provisional installed core observation');
      await this.driver.switchWindow(first.candidate, switchMs);
      const confirmed = await inspect(first.candidate);
      const selected = confirmed.result.observations.find((entry) => entry.targetId === confirmed.result.selectedHandle)?.document;
      if (confirmed.result.phase !== 'installed-selected' || confirmed.result.selectedHandle !== first.candidate || confirmed.candidate !== first.candidate
        || !selected || JSON.stringify(provisional) !== JSON.stringify(selected)) throw new Error('Provisional installed document identity was not confirmed');
      bindInstalledWindow(confirmed);
    } catch {
      this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'retained installed observation or selection failed');
    }
  }

  private async readInstalledDocument<T>(script: string, detail: 'identity' | 'completion' | 'preference' | 'keyboard', deadline = Date.now() + 52_000): Promise<T> {
    this.beginLifecycle();
    try {
      return await this.readInstalledDocumentWithinLifecycle<T>(script, detail, deadline);
    } catch (error) {
      this.failLifecycle(error);
    } finally {
      this.endLifecycle();
    }
  }

  private async readInstalledDocumentWithinLifecycle<T>(script: string, detail: 'identity' | 'completion' | 'preference' | 'keyboard', deadline: number): Promise<T> {
    const remaining = () => Math.min(deadline - Date.now(), this.budget.remainingMs);
    const initialInspection = this.selectedInstalledWindowValid ? 11_000 : 24_000;
    const context = this.driver.snapshot().selectedContext === 'CHROMIUM' ? 0 : 2_000;
    if (remaining() < initialInspection + context + 13_000) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'insufficient read and retained bounds allowance');
    await this.inspectInstalledView(remaining() - 13_000);
    const document = this.inspectedDocument;
    const commandMs = Math.min(30_000, remaining() - 11_000);
    if (commandMs < 2_000) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'insufficient read and retained postcheck allowance');
    const operation = this.productRecorder.begin('read', this.retainedOwner!, this.lastProductInspection, detail);
    const value = await this.driver.execute<T>(script, [], commandMs);
    this.assertDriverClear();
    if (remaining() < 11_000) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'insufficient retained read postcheck allowance');
    await this.inspectInstalledView(remaining());
    if (remaining() <= 0 || document !== this.inspectedDocument) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'read document changed or parent deadline expired');
    this.productRecorder.end('read', this.retainedOwner!, operation, this.lastProductInspection);
    return value;
  }

  async readRunningIdentity(deadline?: number): Promise<RuntimeIdentity> {
    const identity = await this.readInstalledDocument<RuntimeIdentity>(runtimeScript(), 'identity', deadline);
    this.assertOwnershipClear();
    if (!identity || typeof identity !== 'object' || Array.isArray(identity) || identity.navigationId !== this.inspectedNavigationId) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'runtime identity is missing or malformed');
    this.lastIdentity = {
      ...identity,
      nativeProvider: this.installedPackage ? `android:${this.installedPackage}` : undefined,
      nativeActivity: this.lastForeground?.activity,
      nativePid: this.lastForeground?.pid,
    };
    return this.lastIdentity;
  }

  async readUpdateCompletion(deadline?: number): Promise<UpdateCompletionEvidence> {
    const completion = await this.readInstalledDocument<UpdateCompletionEvidence>(updateCompletionScript(true), 'completion', deadline);
    this.assertOwnershipClear();
    this.lastCompletion = completion;
    if (!this.lastCompletion || typeof this.lastCompletion !== 'object' || Array.isArray(this.lastCompletion) || this.lastCompletion.navigationId !== this.inspectedNavigationId) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'completion evidence is missing or malformed');
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
        await this.attachToInstalledView(deadline - Date.now());
        const agent = await this.driver.find(css(`button.agent-open[aria-label="Open mobile-ci on ${relayName}"]`), 2_000);
        // The card remains in the DOM while the relay's inventory reconnects,
        // but its button is disabled until that inventory is ready.
        if ((await this.driver.attribute(agent, 'disabled', 2_000)) !== null) {
          lastError = `agent ${relayName} is waiting for inventory`;
        } else {
          await this.mutateInstalledDocument(() => this.driver.click(agent, 2_000), deadline);
          return;
        }
      } catch (error) {
        if (isFatalDriverError(error) || isQualificationFatal(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250);
    }
    throw new Error(`APPIUM_AGENT: ${relayName}: ${lastError}`);
  }

  async backgroundApp(): Promise<void> {
    this.beginLifecycle();
    try {
      this.requireLifecycleAllowance(5_000 + 2_000 + 30_000 + 500);
      await requireOwnedDevice('android', this.serial);
      await this.assertRetainedOwner();
      this.requireLifecycleAllowance(2_000 + 30_000 + 500);
      await this.driver.switchContext('NATIVE_APP', 2_000);
      this.requireLifecycleAllowance(30_000 + 500);
      await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME'], 30_000, { budget: this.budget });
      this.requireLifecycleAllowance(500);
      await delay(500, this.budget);
      this.requireLifecycleAllowance(0);
    } catch (error) {
      this.failLifecycle(error);
    } finally {
      this.endLifecycle();
    }
  }

  async relaunchInstalledApp(): Promise<void> {
    this.beginLifecycle(true);
    const handoff = this.coldHandoff;
    this.coldHandoff = undefined;
    try {
      if (!this.selectedInstalledWindow || !this.selectedInstalledWindowValid || !this.installedTarget) {
        this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'explicit relaunch requires a selected installed lifecycle');
      }
      if (!handoff) {
        await this.assertRetainedOwner();
        await this.assertRetainedSelection();
        this.productRecorder.transition('warm-launch', this.retainedOwner!);
        await this.launchInstalledTarget('warm');
        return;
      }
      if (handoff.owner !== this.retainedOwner || handoff.target !== this.installedTarget
        || handoff.measurement !== this.environmentMeasurement || handoff.measurementId !== this.environmentMeasurement?.id) {
        this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'owned cold handoff identity changed');
      }
      this.lifecycleBinding = () => {
        if (handoff.target !== this.installedTarget || handoff.measurement !== this.environmentMeasurement
          || handoff.measurementId !== handoff.measurement.id) {
          this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'consumed cold handoff binding changed');
        }
      };
      this.assertRetainedSession();
      this.requireLifecycleAllowance(5_000 + 30_000 + 30_000 + 5_000 + 5_000 + 30_000 + 146_000);
      await this.assertChromeStopped();
      this.assertRetainedSession();
      this.requireLifecycleAllowance(30_000 + 30_000 + 5_000 + 5_000 + 30_000 + 146_000);
      await this.closeOwnedSession();
      this.requireLifecycleAllowance(30_000 + 5_000 + 5_000 + 30_000 + 146_000);
      if (this.retainedOwner !== handoff.owner) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'retired cold owner changed');
      this.retainedOwner = undefined;
      this.coldProductHandoff = true;
      this.kernelReader = undefined;
      this.kernelCapability = undefined;
      this.selectedInstalledWindow = '';
      this.selectedInstalledWindowValid = false;
      await this.launchInstalledTarget('cold');
    } catch (error) {
      this.failLifecycle(error);
    } finally {
      this.endLifecycle();
    }
  }

  async terminateInstalledApp(): Promise<void> {
    this.beginLifecycle();
    try {
      this.requireLifecycleAllowance(175_000);
      await requireOwnedDevice('android', this.serial);
      await this.assertRetainedOwner();
      await this.inspectInstalledView();
      this.requireLifecycleAllowance(157_000);
      await this.driver.switchContext('NATIVE_APP', 2_000);
      this.requireLifecycleAllowance(155_000);
      const foreground = await this.foregroundEvidence(5_000, this.budget);
      this.requireLifecycleAllowance(150_000);
      const owner = this.retainedOwner!;
      const target = this.installedTarget!;
      const measurement = this.environmentMeasurement;
      if (!measurement || foreground.packageName !== this.installedPackage || !isAndroidTerminationPackage(foreground.packageName)
        || !isAndroidPersistentWebAppActivity(foreground.activity) || foreground.pid !== owner.pid) {
        throw new Error('ANDROID_TERMINATE: measured installed PWA process is not independently identified');
      }
      const measurementId = measurement.id;
      this.lifecycleBinding = () => {
        if (owner !== this.retainedOwner || target !== this.installedTarget || measurement !== this.environmentMeasurement || measurementId !== measurement.id) {
          this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'termination owner or measurement changed');
        }
      };
      this.assertRetainedSession();
      await measurement.terminate(foreground.packageName, foreground.pid);
      this.requireLifecycleAllowance(30_000);
      this.assertRetainedSession();
      const home = await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_HOME'], 30_000, { budget: this.budget });
      this.requireLifecycleAllowance(0);
      if (home.stdout.trim() || home.stderr.trim()) throw new Error('ANDROID_TERMINATE: HOME returned unexpected output');
      this.assertRetainedSession();
      this.coldHandoff = Object.freeze({ owner, target, measurement, measurementId });
    } catch (error) {
      this.failLifecycle(error);
    } finally {
      this.endLifecycle();
    }
  }

  private async mutateInstalledDocument(operation: () => Promise<unknown>, deadline: number): Promise<void> {
    if (Math.min(deadline - Date.now(), this.budget.remainingMs) < 13_000) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'insufficient mutation and postcheck allowance');
    try {
      const receipt = this.productRecorder.begin('mutation', this.retainedOwner!, this.lastProductInspection);
      await operation();
      await this.attachToInstalledView(deadline - Date.now());
      if (Date.now() >= deadline) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'mutation parent deadline expired');
      this.productRecorder.end('mutation', this.retainedOwner!, receipt, this.lastProductInspection);
    } catch {
      this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'mutation or retained postcheck failed');
    }
  }

  async showKeyboardOnComposer(): Promise<void> {
    const deadline = Date.now() + Math.min(60_000, this.budget.remainingMs);
    await this.attachToInstalledView(deadline - Date.now());
    let composer = await this.driver.find(css('textarea[aria-label="Prompt"]'), 5_000).catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
      return '';
    });
    if (!composer) {
      const open = await this.driver.find(css('button[aria-label^="Open "]'), Math.min(30_000, deadline - Date.now() - 26_000));
      await this.mutateInstalledDocument(() => this.driver.click(open, 2_000), deadline);
      composer = await this.driver.find(css('textarea[aria-label="Prompt"]'), Math.min(30_000, deadline - Date.now() - 26_000));
    }
    await this.mutateInstalledDocument(() => this.driver.click(composer, 2_000), deadline);
    this.keyboardDraft = 'mobile-device-ci draft';
    await this.mutateInstalledDocument(() => this.driver.sendKeys(composer, this.keyboardDraft, 2_000), deadline);
    if (Math.min(deadline - Date.now(), this.budget.remainingMs) < 21_000) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'insufficient keyboard observation and postcheck allowance');
    await this.waitForKeyboard(true);
    await this.attachToInstalledView(deadline - Date.now());
  }

  async hideKeyboard(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP');
    await command(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'input', 'keyevent', 'KEYCODE_BACK']);
    await this.waitForKeyboard(false);
    await this.attachToInstalledView();
    if (this.keyboardDraft) {
      const value = await this.readInstalledDocument<string>("return document.querySelector('textarea[aria-label=\\\"Prompt\\\"]')?.value || ''", 'keyboard');
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
              if (clickTimeout < 13_000) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'insufficient click and retained postcheck allowance');
              const receipt = this.productRecorder.begin('mutation', this.retainedOwner!, this.lastProductInspection);
              await this.driver.click(element, 2_000);
              await this.attachToInstalledView(deadline - Date.now());
              this.productRecorder.end('mutation', this.retainedOwner!, receipt, this.lastProductInspection);
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
            if (clickTimeout < 13_000) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'insufficient dialog click and retained postcheck allowance');
            if (dialogId === 'update-herdr-dialog' && text === 'Load Update') this.productRecorder.transition('update-activation', this.retainedOwner!);
            const receipt = this.productRecorder.begin('mutation', this.retainedOwner!, this.lastProductInspection);
            await this.driver.click(button, 2_000);
            await this.attachToInstalledView(deadline - Date.now());
            this.productRecorder.end('mutation', this.retainedOwner!, receipt, this.lastProductInspection);
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
    return this.readInstalledDocument<string>("return localStorage.getItem('herdr_home_workspace_layout') || ''", 'preference');
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
      productObservations: this.productRecorder.snapshot(),
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

  stopOwnedResources(): Promise<void> {
    if (this.ownedResourcesStop) return this.ownedResourcesStop;
    this.coldHandoff = undefined;
    this.selectedInstalledWindowValid = false;
    this.lifecycleFailure ||= this.ownershipFailure || qualificationFatal('ANDROID_CONTEXT_OWNERSHIP', 'installed lifecycle stopped', 'ownership');
    this.ownedResourcesStop = this.finishOwnedResources();
    return this.ownedResourcesStop;
  }

  private closeOwnedSession(): Promise<void> {
    if (!this.ownedSessionClose && !this.driver.snapshot().sessionId) return this.driver.close();
    this.ownedSessionClose ||= this.driver.close();
    return this.ownedSessionClose;
  }

  private async finishOwnedResources(): Promise<void> {
    await this.lifecycleSettled;
    let closeError: unknown;
    try {
      await this.closeOwnedSession();
    } catch (error) { closeError = error; }
    try {
      await this.startupLog?.finish();
    } catch (error) {
      if (closeError) throw new AggregateError([closeError, error], 'Android session teardown and diagnostic retention failed', { cause: error });
      throw error;
    }
    if (closeError) throw closeError;
  }

  private async createChromeSession(attachToRunningApp: boolean, reserveMs = 0): Promise<void> {
    this.assertDriverClear();
    if (reserveMs) this.requireLifecycleAllowance(70_000 + 20_000 + 5_000 + reserveMs);
    if (this.budget.remainingMs < 70_000) throw new Error('ANDROID_SESSION: insufficient parent allowance including retained producer startup');
    if (this.driver.snapshot().sessionId) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'previous session was not retired before creation');
    this.ownedSessionClose = undefined;
    await this.driver.create({
      capabilities: androidChromeCapabilities(this.serial, attachToRunningApp),
      requestTimeoutMs: 70_000,
      budget: this.budget,
    });
    this.assertDriverClear();
    if (reserveMs) this.requireLifecycleAllowance(20_000 + 5_000 + reserveMs);
    await this.configureNativeSettings(reserveMs + (reserveMs ? 5_000 : 0));
    this.assertDriverClear();
    if (reserveMs) this.requireLifecycleAllowance(5_000 + reserveMs);
    const assertSession = this.driver.retainSessionOwner();
    try {
      const process = await this.readChromeProcess();
      this.assertDriverClear();
      if (reserveMs) this.requireLifecycleAllowance(reserveMs);
      assertSession();
      this.retainedOwner = { driver: this.driver, assertSession, ...process };
      this.productRecorder.owner(this.retainedOwner);
      if (this.coldProductHandoff) {
        this.productRecorder.handoff(this.retainedOwner, () => this.environmentMeasurement!.plannedOperations());
        this.coldProductHandoff = false;
      }
    } catch (error) { this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', String(error)); }
  }

  private async readChromeProcess(budget = this.budget): Promise<RetainedNativeIdentity> {
    this.assertOwnershipClear();
    const observationStartedAt = new Date().toISOString();
    const deadline = Date.now() + Math.min(5_000, budget.remainingMs);
    const check = () => {
      this.assertOwnershipClear();
      budget.assertAvailable('native process observation');
      if (Date.now() >= deadline) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'native process observation expired');
    };
    this.kernelReader ||= createAdbInspection({curDeviceId: this.serial, executable: {defaultArgs: ['-P', '5037', '-s', this.serial]}}, () => this.assertOwnershipClear(), error => {
      this.ownershipFailure ||= qualificationFatal('ANDROID_CONTEXT_OWNERSHIP', error.message, 'ownership');
      return this.ownershipFailure;
    });
    const shell = async (args: Parameters<NonNullable<typeof this.kernelReader>['read']>[0]) => {
      check();
      const value = await this.kernelReader!.read(args, deadline);
      check();
      return value;
    };
    try {
      this.kernelCapability ||= await acquireKernelCapability(this.kernelReader, deadline, check);
    } catch (error) {
      this.kernelReader.cancel(error instanceof Error ? error : new Error('Kernel capability refused'));
      this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', String(error));
    }
    const pid = await shell(['shell', 'pidof', 'com.android.chrome']);
    if (!/^[1-9]\d*$/u.test(pid) || Number(pid) > 2147483647) throw new Error('ANDROID_PROCESS: sole Chrome browser PID is unavailable');
    const stat = await shell(['shell', 'cat', `/proc/${Number(pid)}/stat`]);
    const match = stat.trim().match(/^(\d+) \(.+\) ([A-Za-z]) (.+)$/u);
    const startTime = match?.[3]?.split(/\s+/u)[18] || '';
    if (match?.[1] !== pid || !/^[1-9]\d*$/u.test(startTime) || ['Z', 'X', 'x'].includes(match?.[2] || '')) {
      throw new Error('ANDROID_PROCESS: live Chrome process start time is unavailable');
    }
    this.diagnostics.record({
      phase: 'android-process',
      operation: 'chrome-process-observed',
      detail: { pid, startTime, observationStartedAt, observationFinishedAt: new Date().toISOString() },
    });
    if (this.retainedOwner && (pid !== this.retainedOwner.pid || startTime !== this.retainedOwner.startTime)) throw new Error('ANDROID_PROCESS: original native process changed');
    const status = await shell(['shell', 'cat', `/proc/${Number(pid)}/status`]);
    const self = await shell(['shell', 'cat', '/proc/self/status']);
    validateNamespaceStatus(status, pid, this.kernelCapability);
    validateNamespaceStatus(self, undefined, this.kernelCapability);
    const bootId = await shell(['shell', 'cat', '/proc/sys/kernel/random/boot_id']);
    if (!/^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/u.test(bootId)) throw new Error('ANDROID_PROCESS: native boot identity unavailable');
    if (bootId !== this.kernelCapability.bootId) throw new Error('ANDROID_PROCESS: original kernel capability boot changed');
    return { pid, startTime, bootId, namespace: namespaceForCapability(this.kernelCapability), kernelCapability: this.kernelCapability };
  }

  private assertRetainedSession(): void {
    this.assertOwnershipClear();
    try {
      if (!this.retainedOwner || this.retainedOwner.driver !== this.driver) throw new Error('original Chrome session owner is unavailable');
      this.retainedOwner.assertSession();
    } catch (error) {
      this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', String(error));
    }
  }

  private async assertRetainedSelection(): Promise<void> {
    this.requireLifecycleAllowance(6_000);
    this.assertRetainedSession();
    if (this.driver.snapshot().selectedContext !== 'CHROMIUM') await this.driver.switchContext('CHROMIUM', 2_000);
    this.requireLifecycleAllowance(4_000);
    const current = await this.driver.currentWindow(2_000);
    this.requireLifecycleAllowance(2_000);
    const handles = await this.driver.windowHandles(2_000);
    this.requireLifecycleAllowance(0);
    this.assertRetainedSession();
    if (!this.selectedInstalledWindowValid || current !== this.selectedInstalledWindow || !handles.includes(current)) {
      this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'original installed selection is unavailable');
    }
  }

  private async assertRetainedOwner(budget = this.budget): Promise<void> {
    this.assertOwnershipClear();
    const owner = this.retainedOwner;
    try {
      if (!owner || owner.driver !== this.driver) throw new Error('original Chrome session owner is unavailable');
      owner.assertSession();
      const current = await this.readChromeProcess(budget);
      owner.assertSession();
      if (this.retainedOwner !== owner || owner.driver !== this.driver || current.pid !== owner.pid || current.startTime !== owner.startTime || current.bootId !== owner.bootId || current.namespace !== owner.namespace || !sameKernelCapability(current.kernelCapability, owner.kernelCapability)) {
        throw new Error('original Chrome browser process or session was replaced');
      }
    } catch (error) {
      this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', error instanceof Error ? error.message : String(error));
    }
  }

  private async configureNativeSettings(reserveMs = 0): Promise<void> {
    if (reserveMs) this.requireLifecycleAllowance(20_000 + reserveMs);
    const timeoutMs = Math.min(10_000, this.budget.remainingMs);
    if (timeoutMs < minimumDriverRequestMs) throw new Error('ANDROID_SETTINGS: insufficient time to configure native settings');
    const requested = {
      waitForIdleTimeout: ANDROID_NATIVE_IDLE_TIMEOUT_MS,
      waitForSelectorTimeout: ANDROID_NATIVE_SELECTOR_TIMEOUT_MS,
    };
    await this.driver.updateSettings(requested, timeoutMs);
    if (reserveMs) this.requireLifecycleAllowance(10_000 + reserveMs);
    const readbackTimeout = Math.min(timeoutMs, this.budget.remainingMs);
    if (readbackTimeout < minimumDriverRequestMs) throw new Error('ANDROID_SETTINGS: insufficient time to read back native settings');
    const response = await this.driver.settings(readbackTimeout);
    this.assertOwnershipClear();
    if (reserveMs) this.requireLifecycleAllowance(reserveMs);
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

  private async waitForChromeDevTools(timeoutMs: number, reserveMs = 0): Promise<void> {
    const phase = this.budget.phaseView('android-devtools', timeoutMs, reserveMs);
    const adb = process.env.ADB || 'adb';
    let last = '';
    while (!phase.exhausted) {
      phase.assertAvailable('discover Chrome DevTools socket');
      try {
        if (this.retainedOwner) await this.assertRetainedOwner(phase);
        const sockets = await commandOutput(adb, ['-s', this.serial, 'shell', 'cat', '/proc/net/unix'], 5_000, {
          budget: phase,
          label: 'discover Chrome DevTools socket',
        });
        phase.assertAvailable('DevTools settlement');
        this.assertDriverClear();
        if (hasAndroidChromeDevToolsSocket(sockets)) return;
        last = 'chrome_devtools_remote socket is not published';
      } catch (error) {
        this.assertOwnershipClear();
        if (phase.exhausted || isFatalDriverError(error)) throw error;
        if (this.retainedOwner) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', String(error));
        last = error instanceof Error ? error.message : String(error);
      }
      await delay(250, phase);
    }
    throw new Error(`ANDROID_CONTEXT: Chrome DevTools was not ready (${last})`);
  }

  private async certificateCommand(args: string[]): Promise<CommandResult> {
    this.assertDriverClear();
    try {
      const result = await command(process.env.ADB || 'adb', args, 30_000);
      this.assertDriverClear();
      return result;
    } catch (error) {
      this.assertDriverClear();
      throw error;
    }
  }

  private async installCertificate(): Promise<void> {
    this.assertDriverClear();
    const remote = '/sdcard/Download/herdr-mobile-ci-ca.crt';
    const remoteName = basename(remote);
    const commonName = await this.certificateCommonName();

    this.assertDriverClear();
    await this.certificateCommand(['-s', this.serial, 'push', this.options.certificate, remote]);
    // adb push does not update MediaProvider, so DocumentsUI may omit the
    // freshly copied file from Downloads until the exact path is scanned.
    await this.certificateCommand([
      '-s', this.serial, 'shell', 'content', 'call', '--uri', 'content://media',
      '--method', 'scan_file', '--arg', remote,
    ]).catch(async () => {
      this.assertDriverClear();
      await this.certificateCommand([
        '-s', this.serial, 'shell', 'am', 'broadcast', '--receiver-include-background',
        '-a', 'android.intent.action.MEDIA_SCANNER_SCAN_FILE', '-d', `file://${remote}`,
      ]).catch(() => { this.assertDriverClear(); });
    });
    this.assertDriverClear();
    await delay(500);
    this.assertDriverClear();
    await this.certificateCommand(['-s', this.serial, 'shell', 'am', 'force-stop', 'com.google.android.documentsui']).catch(() => { this.assertDriverClear(); });
    await this.certificateCommand(['-s', this.serial, 'shell', 'am', 'force-stop', 'com.android.settings']);
    await this.certificateCommand(['-s', this.serial, 'shell', 'am', 'start', '-a', 'com.android.settings.MORE_SECURITY_PRIVACY_SETTINGS']);
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
    this.assertDriverClear();
    await delay(1_000);
    this.assertDriverClear();
    await this.verifyCertificate(remoteName, commonName);
    await this.certificateCommand(['-s', this.serial, 'shell', 'rm', '-f', remote]);
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
        this.assertOwnershipClear();
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
        this.assertOwnershipClear();
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
        this.assertOwnershipClear();
        const output = await commandOutput(adb, args, 10_000).catch((error) => error instanceof Error ? error.message : String(error));
        this.assertOwnershipClear();
        await writeBoundedText(join(this.outputDir, `${name}-${suffix}.log`), output);
      }
    } catch (error) {
      this.assertOwnershipClear();
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
    this.assertDriverClear();
    const bytes = await readFile(this.options.certificate);
    this.assertDriverClear();
    const certificate = new X509Certificate(bytes);
    const commonName = certificate.subject.match(/CN\s*=\s*([^,\n/]+)/u)?.[1]?.trim();
    if (!commonName) throw new Error('ANDROID_CERTIFICATE: supplied certificate has no common name');
    return commonName;
  }

  private async verifyCertificate(certificateName: string, commonName: string): Promise<void> {
    this.assertDriverClear();
    await this.certificateCommand(['-s', this.serial, 'shell', 'am', 'start', '-a', 'com.android.settings.TRUSTED_CREDENTIALS_USER']);
    await this.driver.switchContext('NATIVE_APP').catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
    });
    await this.waitForForegroundPackage('com.android.settings');
    this.assertDriverClear();
    await delay(500);
    this.assertDriverClear();
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
    this.assertOwnershipClear();
    const output = await commandOutput(process.env.ADB || 'adb', ['-s', this.serial, 'shell', 'dumpsys', 'activity', 'activities'], timeoutMs, {
      budget,
      label: 'read Android foreground activity',
    });
    this.assertOwnershipClear();
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

  private async waitForInstalledTarget(timeoutMs: number, launchError?: unknown, reserveMs = 0): Promise<void> {
    const phase = this.budget.phaseView('android-launch-proof', timeoutMs, reserveMs);
    let last = launchError instanceof Error ? launchError.message : '';
    while (!phase.exhausted) {
      phase.assertAvailable('verify installed launch');
      try {
        if (this.retainedOwner) await this.assertRetainedOwner(phase);
        const evidence = await this.foregroundEvidence(Math.min(5_000, phase.remainingMs), phase);
        phase.assertAvailable('installed foreground settlement');
        this.assertDriverClear();
        this.lastForeground = evidence;
        last = `${evidence.packageName}/${evidence.activity} pid=${evidence.pid}`;
        this.diagnostics.record({ phase: 'android-launch-proof', operation: 'foreground-observation', detail: { packageName: evidence.packageName, activity: evidence.activity, pid: evidence.pid } });
        if (isAndroidPersistentWebAppActivity(evidence.activity)
          && evidence.packageName === this.installedTarget?.packageName
          && (!this.retainedOwner || evidence.pid === this.retainedOwner.pid)) {
          this.installedPackage = evidence.packageName;
          return;
        }
      } catch (error) {
        if (isFatalDriverError(error) || isQualificationFatal(error)) throw error;
        if (this.retainedOwner) this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', String(error));
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

  private guardRetainedCommands(): void {
    this.driver.setCommandGuard({
      before: () => this.assertOwnershipClear(),
      failure: (error, path) => {
        if (/^\/elements?$/u.test(path) && error instanceof WebDriverError
          && error.code === 'APPIUM_COMMAND' && error.status === 404
          && /no such element|stale element reference/iu.test(error.message)) return;
        this.failOwnership('ANDROID_CONTEXT_OWNERSHIP', 'retained command failed');
      },
    });
  }

  private assertOwnershipClear(): void {
    if (this.lifecycleFailure) throw this.lifecycleFailure;
    if (this.ownershipFailure) throw this.ownershipFailure;
    this.lifecycleBinding?.();
  }

  private failOwnership(code: string, detail: string): never {
    this.productRecorder.fail('OWNERSHIP_LOSS');
    this.coldHandoff = undefined;
    if (this.lifecycleFailure) throw this.lifecycleFailure;
    this.ownershipFailure ||= qualificationFatal(code, detail, 'ownership');
    throw this.ownershipFailure;
  }

  private async currentForegroundPackage(timeoutMs = 10_000, budget?: PhaseBudget): Promise<string> {
    return (await this.foregroundEvidence(timeoutMs, budget)).packageName;
  }
}
