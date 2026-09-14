import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import {
  assertStandalone,
  isQualificationFatal,
  qualificationFatal,
  type QualificationFatalError,
  type RuntimeIdentity,
} from '../support/oracle';
import { DiagnosticRecorder, writeBoundedText, writeSanitizedJson } from '../support/diagnostics';
import { PhaseBudget } from '../support/budget';
import { initializeIOSConfirmationSettings, withIOSConfirmationSettings } from '../support/confirmation-settings';
import { CommandError, command, commandOutput } from '../support/process';
import { requireOwnedDevice } from '../support/device';
import {
  accessibility,
  ariaLabel,
  ariaLabelPrefix,
  AppiumClient,
  isCommandAdmissionError,
  buttonText,
  css,
  delay,
  isFatalDriverError,
  isRetryableElementLookupError,
  minimumDriverRequestMs,
  textLocator,
  type ContextMetadata,
  type Locator,
  WebDriverError,
} from '../support/webdriver';
import { runtimeScript, updateCompletionScript, type MobilePlatform, type PlatformOptions, type UpdateCompletionEvidence } from './types';

const IOS_INSTALLED_BUNDLE_ID = 'com.apple.webapp';
const IOS_SPRINGBOARD_BUNDLE_ID = 'com.apple.springboard';
const IOS_OPENURL_COMMAND_MS = 30_000;
const IOS_WEBVIEW_CONNECT_TIMEOUT_MS = 5_000;
const IOS_WEBVIEW_CONNECT_RETRIES = 1;
const IOS_WEBKIT_DISCOVERY_COMMAND_MS = 20_000;
const IOS_SAFARI_READINESS_PHASE_MS = 46_000;
const IOS_NAVIGATION_PHASE_MS = 86_000;
const IOS_SAFARI_DISCOVERY_RESERVE_MS = 6_000;
const IOS_SAFARI_ATTACH_COMMAND_MS = 15_000;
const IOS_SAFARI_OBSERVATION_MS = IOS_SAFARI_ATTACH_COMMAND_MS + 2_000;
const IOS_INSTALLED_FOREGROUND_MS = 8_000;
const IOS_NATIVE_LOOKUP_ROUND_MS = 5_000;
const IOS_CONFIRMATION_LOOKUP_MS = 8_000;
const IOS_CONFIRMATION_IDENTITY_MS = 12_000;
const IOS_CONFIRMATION_ROUND_MS = 5 * IOS_NATIVE_LOOKUP_ROUND_MS + IOS_CONFIRMATION_LOOKUP_MS + 2 * IOS_CONFIRMATION_IDENTITY_MS;
const IOS_NATIVE_SCROLL_COMMAND_MS = 5_000;
const IOS_NATIVE_HIERARCHY_COMMAND_MS = 8_000;
const IOS_NATIVE_SCROLL_LIMIT = 8;
const IOS_NATIVE_LIST_READINESS_MS = 15_000;
const IOS_NATIVE_ACTION_TIMEOUT_MS = IOS_NATIVE_SCROLL_COMMAND_MS * IOS_NATIVE_SCROLL_LIMIT + 20_000;

export function isIOSSafariBrowserBundle(bundleId?: string): boolean {
  return bundleId?.toLowerCase() === 'com.apple.mobilesafari';
}

export function isIOSSafariViewServiceBundle(bundleId?: string): boolean {
  return bundleId?.toLowerCase() === 'com.apple.safariviewservice';
}

export function isIOSStaleContextError(error: unknown): boolean {
  if (error instanceof WebDriverError && (error.timedOut || error.code === 'APPIUM_SESSION_UNUSABLE')) return false;
  const message = error instanceof Error ? error.message : String(error);
  return /no such (?:window|context|frame)|(?:window|context|webview|page|target).*(?:not found|does not exist|is gone|closed|detached)|(?:stale|invalid).*(?:context|window|webview|page|target)/iu.test(message);
}

function isIOSStaleElementError(error: unknown): boolean {
  if (!(error instanceof WebDriverError) || error.timedOut) return false;
  return error.status === 404 && /stale element|element.*(?:not present|not found|does not exist)|no matches for/iu.test(error.message);
}

export type IOSOpenURLFailureKind = 'transient' | 'terminal' | 'timeout';

export function iosOpenURLFailureKind(error: unknown): IOSOpenURLFailureKind {
  if (error instanceof CommandError && error.timedOut) return 'timeout';
  const message = error instanceof CommandError
    ? `${error.message} ${error.stderr} ${error.stdout}`
    : error instanceof Error ? error.message : String(error);
  if (/timed out|timeout|ETIMEDOUT/iu.test(message)) return 'timeout';
  if (/(?:invalid|unknown|no such|not found|unavailable|permission denied|malformed|could not find|does not exist)/iu.test(message)
    && !/(?:temporar|busy|try again|in progress|connection|launchservices)/iu.test(message)) return 'terminal';
  return 'transient';
}

export function iosOpenURLProcessEvidence(error: unknown): Record<string, unknown> {
  if (error instanceof CommandError) {
    return {
      code: error.code,
      durationMs: error.durationMs,
      normalizedExitCode: error.exitCode,
      timedOut: error.timedOut,
      signal: error.signal,
      stdout: error.stdout,
      stderr: error.stderr,
    };
  }
  return { message: error instanceof Error ? error.message : String(error) };
}

function isIOSContextNotReadyError(error: unknown): boolean {
  const message = error instanceof Error ? error.message : String(error);
  return /^IOS_CONTEXT_NOT_READY:/u.test(message);
}

export function iosInstalledContextRejection(context: ContextMetadata, origin: string): string {
  if (context.id === 'NATIVE_APP') return 'native context is not a web page';
  if (isIOSSafariBrowserBundle(context.bundleId)) return 'Safari browser context is not the installed provider';
  if (context.url) {
    try {
      if (new URL(context.url).origin !== new URL(origin).origin) return `origin ${new URL(context.url).origin} is not ${new URL(origin).origin}`;
    } catch {
      return `context URL is invalid: ${context.url}`;
    }
  }
  return '';
}

function xpathLiteral(value: string): string {
  if (!value.includes("'")) return `'${value}'`;
  if (!value.includes('"')) return `"${value}"`;
  return `concat(${value.split("'").map((part) => `'${part}'`).join(", \"'\", ")})`;
}

function iosActionLabelContains(value: string): Locator {
  const literal = xpathLiteral(value);
  return {
    using: 'xpath',
    value: `//*[ancestor::*[@name='ActivityListView'] and ancestor::*[@name='ShareSheet.RemoteContainerView'] and ancestor::*[@name='activityCollectionView'] and (self::*[@name='actionGroupCell'] or ancestor::*[@name='actionGroupCell']) and (contains(@label, ${literal}) or contains(@name, ${literal}) or contains(@value, ${literal}) or contains(@text, ${literal}))]`,
  };
}

function iosActionCollectionLocator(): Locator {
  return {
    using: 'xpath',
    value: "//*[@name='ActivityListView']//*[@name='ShareSheet.RemoteContainerView']//*[@name='activityCollectionView']",
  };
}

interface NativeBounds {
  x: number;
  y: number;
  width: number;
  height: number;
}

interface NativeScrollContainer {
  element: string;
  bounds: NativeBounds;
  type: string;
}

interface NativeScrollEvidence {
  type: string;
  bounds: NativeBounds;
}

interface NativeActionRow {
  key: string;
  label: string;
  bounds: NativeBounds;
  visible: boolean;
  enabled: boolean;
}

interface NativeActionListEvidence {
  collection: NativeScrollEvidence;
  rows: NativeActionRow[];
  targetRows: NativeActionRow[];
}

function nativeAttribute(node: string, name: string): string | undefined {
  return node.match(new RegExp(`\\b${name}="([^"]*)"`, 'u'))?.[1];
}

function nativeNodeBounds(node: string): NativeBounds | undefined {
  const bounds = nativeAttribute(node, 'bounds')?.match(/^\[(-?\d+),(-?\d+)\]\[(-?\d+),(-?\d+)\]$/u);
  const x = bounds ? Number(bounds[1]) : Number(nativeAttribute(node, 'x'));
  const y = bounds ? Number(bounds[2]) : Number(nativeAttribute(node, 'y'));
  const width = bounds ? Number(bounds[3]) - x : Number(nativeAttribute(node, 'width'));
  const height = bounds ? Number(bounds[4]) - y : Number(nativeAttribute(node, 'height'));
  if (!Number.isFinite(x) || !Number.isFinite(y) || width <= 0 || height <= 0) return undefined;
  return { x, y, width, height };
}

function nativeNodeLabels(node: string): string[] {
  return [nativeAttribute(node, 'name'), nativeAttribute(node, 'label'), nativeAttribute(node, 'value')]
    .filter((value): value is string => Boolean(value));
}

function nativeNodeType(node: string): string {
  return node.match(/^<(XCUIElementType\w+)\b/u)?.[1] || '';
}

export function nativeActionListEvidence(source: string, target: string): NativeActionListEvidence | undefined {
  const tokens = source.match(/<\/?[^>]+>/gu) || [];
  const stack: Array<{
    type: string;
    name: string;
    visible: boolean;
    activityList: boolean;
    remoteContainer: boolean;
    actionCollection: boolean;
  }> = [];
  let collection: NativeScrollEvidence | undefined;
  const rows: NativeActionRow[] = [];
  const occurrences = new Map<string, number>();
  for (const token of tokens) {
    if (/^<\//u.test(token)) {
      if (/^<\/XCUIElementType\w+>/u.test(token)) stack.pop();
      continue;
    }
    const type = nativeNodeType(token);
    if (!type) continue;
    const name = nativeAttribute(token, 'name') || '';
    const bounds = nativeNodeBounds(token);
    const visible = nativeAttribute(token, 'visible') !== 'false';
    const activityList = stack.some((node) => node.activityList) || (visible && /(?:^|\.)ActivityListView$/u.test(name));
    const remoteContainer = stack.some((node) => node.remoteContainer) || (visible && name === 'ShareSheet.RemoteContainerView');
    const inActionCollection = stack.some((node) => node.actionCollection);
    const isActionCollection = activityList && remoteContainer
      && (type === 'XCUIElementTypeCollectionView' || type === 'XCUIElementTypeTable')
      && name === 'activityCollectionView'
      && visible
      && Boolean(bounds);
    const node = {
      type,
      name,
      visible,
      activityList,
      remoteContainer,
      actionCollection: inActionCollection || isActionCollection,
    };
    if (isActionCollection && bounds && !collection) {
      collection = { type, bounds };
    }
    if (inActionCollection && type === 'XCUIElementTypeCell' && name === 'actionGroupCell' && bounds) {
      const label = nativeNodeLabels(token).find((value) => value !== name) || name;
      const normalized = label.toLowerCase();
      const occurrence = occurrences.get(normalized) || 0;
      occurrences.set(normalized, occurrence + 1);
      rows.push({
        key: `${normalized}#${occurrence}`,
        label,
        bounds,
        visible,
        enabled: nativeAttribute(token, 'enabled') !== 'false',
      });
    }
    if (!token.endsWith('/>')) stack.push(node);
  }
  if (!collection || rows.length === 0) return undefined;
  const targetText = target.toLowerCase();
  return {
    collection,
    rows,
    targetRows: rows.filter((row) => row.label.toLowerCase().includes(targetText)),
  };
}

export function iosNativeScrollDirection(target: NativeBounds, viewport: NativeBounds): 'up' | 'down' | undefined {
  if (target.y < viewport.y - 1) return 'up';
  if (target.y + target.height > viewport.y + viewport.height + 1) return 'down';
  return undefined;
}

export function iosNativeSwipeDirection(scrollDirection: 'up' | 'down'): 'up' | 'down' {
  return scrollDirection === 'down' ? 'up' : 'down';
}

function nativeActionRowsMoved(before: NativeActionRow[], after: NativeActionRow[]): boolean {
  const afterByKey = new Map(after.map((row) => [row.key, row]));
  return before.some((row) => {
    const next = afterByKey.get(row.key);
    return Boolean(next && nativeBoundsChanged(row.bounds, next.bounds));
  });
}

function nativeShareBounds(source: string): NativeBounds | undefined {
  const nodes = source.match(/<node\b[^>]*\/>|<XCUIElementType\w+\b[^>]*>/gu) || [];
  for (const node of nodes) {
    const labels = nativeNodeLabels(node);
    if (!labels.some((label) => /^share(?: button)?$/iu.test(label.trim()))) continue;
    if (nativeAttribute(node, 'enabled') === 'false' || nativeAttribute(node, 'visible') === 'false') continue;
    const bounds = nativeNodeBounds(node);
    if (bounds) return bounds;
  }
  return undefined;
}

function nativeBoundsChanged(before: NativeBounds | undefined, after: NativeBounds | undefined): boolean {
  if (!before || !after) return false;
  return Math.abs(before.x - after.x) > 1
    || Math.abs(before.y - after.y) > 1
    || Math.abs(before.width - after.width) > 1
    || Math.abs(before.height - after.height) > 1;
}

function nativeBoundsOverlap(left: NativeBounds, right: NativeBounds): boolean {
  return left.x < right.x + right.width
    && right.x < left.x + left.width
    && left.y < right.y + right.height
    && right.y < left.y + left.height;
}

export class IOSPlatform implements MobilePlatform {
  readonly name = 'ios' as const;
  readonly driver: AppiumClient;
  private readonly udid: string;
  private readonly origin: string;
  private readonly outputDir: string;
  private readonly budget: PhaseBudget;
  private readonly diagnostics: DiagnosticRecorder;
  private installedBundleId = '';
  private selectedInstalledContext = '';
  private installedBindingState: 'unselected' | 'inspecting' | 'bound' = 'unselected';
  private lastIdentity?: RuntimeIdentity;
  private lastUrl = '';
  private keyboardDraft = '';
  private lastCompletion?: UpdateCompletionEvidence;
  private lastNativeActivity = '';
  private lastNativePid = '';
  private simulatorReadyAt = '';
  private ownershipFailure?: QualificationFatalError;
  private nativeObservationFailure?: unknown;
  private nativeObservationTarget = 'auto';
  private springBoardRoot = '';
  private navigationCommand = command;

  constructor(private readonly options: PlatformOptions) {
    this.udid = options.deviceId || process.env.IOS_SIMULATOR_UDID || '';
    this.origin = options.origin.replace(/\/$/, '');
    this.outputDir = options.outputDir;
    this.budget = options.budget || new PhaseBudget('ios-run', { timeoutMs: 30 * 60_000, recoveryLimit: 4 });
    this.diagnostics = options.diagnostics || new DiagnosticRecorder();
    this.driver = new AppiumClient(options.appiumUrl);
    this.driver.setBudget(this.budget);
  }

  async startFreshDevice(): Promise<void> {
    if (!this.udid) throw new Error('IOS_TARGET: IOS_SIMULATOR_UDID is required');
    await requireOwnedDevice('ios', this.udid);
    const available = await commandOutput('xcrun', ['simctl', 'list', 'devices', 'available']);
    if (!available.includes(this.udid)) throw new Error(`IOS_TARGET: simulator ${this.udid} is not an available simulator`);
    await command('xcrun', ['simctl', 'boot', this.udid]).catch(() => undefined);
    const bootStatus = await command('xcrun', ['simctl', 'bootstatus', this.udid, '-b'], 300_000);
    this.simulatorReadyAt = new Date().toISOString();
    this.diagnostics.record({
      phase: 'ios-device', operation: 'simulator-boot-ready', durationMs: bootStatus.durationMs,
      detail: { outcome: 'collected', observedAt: this.simulatorReadyAt, source: 'simctl bootstatus', process: { ...bootStatus, normalizedExitCode: bootStatus.code, code: undefined } },
    });
    await command('xcrun', ['simctl', 'keychain', this.udid, 'add-root-cert', this.options.certificate], 30_000);
    await this.driver.create({
      capabilities: {
        platformName: 'iOS',
        // Run Safari as a native AUT. browserName=Safari makes XCUITest
        // activate WebKit during createSession, before the simulator has a
        // debuggable page, and can fail the whole session on hosted runners.
        // We open the URL explicitly below and attach to WEBVIEW only after it
        // has been published by Web Inspector.
        'appium:bundleId': 'com.apple.mobilesafari',
        'appium:automationName': 'XCUITest',
        ...(process.env.IOS_PLATFORM_VERSION
          ? { 'appium:platformVersion': process.env.IOS_PLATFORM_VERSION }
          : {}),
        'appium:udid': this.udid,
        'appium:noReset': true,
        'appium:fullReset': false,
        'appium:newCommandTimeout': 1_200,
        'appium:includeSafariInWebviews': true,
        'appium:additionalWebviewBundleIds': ['com.apple.webapp'],
        'appium:autoWebview': false,
        // Allow Web Inspector time to publish a page after simctl openurl.
        'appium:webviewConnectTimeout': IOS_WEBVIEW_CONNECT_TIMEOUT_MS,
        'appium:webviewConnectRetries': IOS_WEBVIEW_CONNECT_RETRIES,
        ...(process.env.IOS_WDA_PREBUILT_PATH
          ? {
            'appium:usePreinstalledWDA': true,
            'appium:prebuiltWDAPath': process.env.IOS_WDA_PREBUILT_PATH,
          }
          : {}),
        // A fresh hosted runner may need several minutes to build and launch WDA.
        'appium:showXcodeLog': true,
        'appium:wdaLaunchTimeout': 300_000,
        'appium:wdaStartupRetries': 1,
        'appium:wdaStartupRetryInterval': 10_000,
      },
      // Keep the client request alive while Appium installs/launches the
      // prebuilt WebDriverAgent and creates the Safari session.
      requestTimeoutMs: 360_000,
      budget: this.budget,
    });
    await initializeIOSConfirmationSettings(this.driver, this.budget);
  }

  async openSetupURL(url: string): Promise<void> {
    this.assertOwnershipClear();
    await this.captureNavigationState('before');
    const phase = this.budget.phaseView('ios-navigation', IOS_NAVIGATION_PHASE_MS);
    phase.assertAvailable('open setup URL');
    const timeoutMs = Math.min(IOS_OPENURL_COMMAND_MS, phase.remainingMs);
    if (timeoutMs < IOS_OPENURL_COMMAND_MS) throw new Error('IOS_NAVIGATION: insufficient time for a complete openurl command');
    const startedAt = new Date().toISOString();
    this.diagnostics.record({ phase: 'ios-navigation', operation: 'simctl openurl started', timeoutMs, detail: { attempt: 1, startedAt } });
    try {
      const result = await this.navigationCommand('xcrun', ['simctl', 'openurl', this.udid, url], timeoutMs, {
        budget: phase,
        label: 'simctl openurl setup URL',
      });
      this.diagnostics.record({
        phase: 'ios-navigation', operation: 'simctl openurl succeeded', durationMs: result.durationMs, timeoutMs,
        detail: { attempt: 1, startedAt, settledAt: new Date().toISOString(), process: { ...result, normalizedExitCode: result.code, code: undefined } },
      });
    } catch (error) {
      const settledAt = new Date().toISOString();
      const evidence = iosOpenURLProcessEvidence(error);
      this.diagnostics.record({
        phase: 'ios-navigation',
        operation: 'simctl openurl failed',
        durationMs: error instanceof CommandError ? error.durationMs : undefined,
        timeoutMs,
        timedOut: error instanceof CommandError ? error.timedOut : undefined,
        signal: error instanceof CommandError ? error.signal : undefined,
        detail: { attempt: 1, startedAt, settledAt, kind: iosOpenURLFailureKind(error), process: evidence },
      });
      try {
        await this.captureNavigationState('after');
      } catch (diagnosticError) {
        this.diagnostics.record({ phase: 'ios-navigation', operation: 'openurl-diagnostics-stopped', detail: iosOpenURLProcessEvidence(diagnosticError) });
      }
      try {
        await this.captureNavigationHostLog(startedAt, settledAt);
      } catch (diagnosticError) {
        this.diagnostics.record({ phase: 'ios-navigation', operation: 'openurl-host-log-stopped', detail: iosOpenURLProcessEvidence(diagnosticError) });
      }
      throw new Error(`IOS_NAVIGATION: could not open setup URL: ${JSON.stringify(evidence)}`, { cause: error });
    }
    if (phase.exhausted) throw new Error('IOS_NAVIGATION: setup URL budget expired before navigation completed');
    const readinessTimeout = Math.min(IOS_SAFARI_READINESS_PHASE_MS, phase.remainingMs);
    if (readinessTimeout < minimumDriverRequestMs) throw new Error('IOS_NAVIGATION: no time remained to verify Safari fixture navigation');
    await this.waitForSafariFixturePage(url, readinessTimeout, phase);
  }

  private async navigationDiagnostic(
    phase: PhaseBudget,
    operation: string,
    timeoutMs: number,
    collect: () => Promise<unknown>,
  ): Promise<void> {
    if (phase.remainingMs < timeoutMs) {
      this.diagnostics.record({ phase: 'ios-navigation', operation, timeoutMs, detail: { outcome: 'skipped', reason: 'insufficient diagnostic budget', remainingMs: phase.remainingMs } });
      return;
    }
    const startedAt = Date.now();
    try {
      const result = await collect();
      this.diagnostics.record({ phase: 'ios-navigation', operation, timeoutMs, durationMs: Date.now() - startedAt, detail: { outcome: 'collected', result } });
    } catch (error) {
      const fatal = isFatalDriverError(error);
      this.diagnostics.record({
        phase: 'ios-navigation',
        operation,
        timeoutMs,
        durationMs: Date.now() - startedAt,
        detail: { outcome: fatal ? 'failed' : 'unavailable', process: iosOpenURLProcessEvidence(error) },
      });
      if (fatal) throw error;
    }
  }

  private async captureNavigationState(stage: 'before' | 'after'): Promise<void> {
    const phase = this.budget.phaseView(`ios-${stage}-openurl`, stage === 'before' ? 10_000 : 30_000);
    if (stage === 'before' && this.simulatorReadyAt) {
      this.diagnostics.record({
        phase: 'ios-navigation', operation: 'before-openurl-boot-state',
        detail: { outcome: 'reused', observedAt: this.simulatorReadyAt, source: 'ios-device/simctl bootstatus' },
      });
      return;
    }
    await this.navigationDiagnostic(phase, `${stage}-openurl-boot-state`, 3_000, async () => {
      const result = await this.navigationCommand('xcrun', ['simctl', 'list', 'devices', 'available', '--json'], 3_000, { budget: phase });
      const listing = JSON.parse(result.stdout) as { devices: Record<string, Array<{ udid: string; state: string; isAvailable: boolean }>> };
      const device = Object.values(listing.devices).flat().find((device) => device.udid === this.udid);
      if (!device) throw new Error('IOS_NAVIGATION_DIAGNOSTIC: owned simulator is absent from the available device listing');
      return device;
    });
    if (stage === 'before') return;
    let nativeContext = false;
    await this.navigationDiagnostic(phase, `${stage}-openurl-native-context`, 1_000, async () => {
      await this.driver.switchContext('NATIVE_APP', 1_000);
      nativeContext = true;
    });
    if (!nativeContext) return;
    await this.navigationDiagnostic(phase, `${stage}-openurl-foreground`, 1_000, () => this.driver.activeAppInfo(1_000));
    await this.navigationDiagnostic(phase, `${stage}-openurl-hierarchy`, 5_000, async () => {
      const source = await this.driver.pageSource(5_000);
      const filename = join(this.outputDir, `ios-${stage}-openurl-hierarchy.xml`);
      await writeBoundedText(filename, source, 1024 * 1024);
      return { filename };
    });
    if (stage === 'after') {
      await this.navigationDiagnostic(phase, 'after-openurl-pages', IOS_WEBKIT_DISCOVERY_COMMAND_MS, () => this.driver.contextMetadata(IOS_WEBKIT_DISCOVERY_COMMAND_MS));
    }
  }

  private async captureNavigationHostLog(startedAt: string, settledAt: string): Promise<void> {
    const phase = this.budget.phaseView('ios-openurl-host-log', 6_000);
    const start = `${startedAt.slice(0, 19).replace('T', ' ')}+0000`;
    const end = `${new Date(Date.parse(settledAt) + 1_000).toISOString().slice(0, 19).replace('T', ' ')}+0000`;
    await this.navigationDiagnostic(phase, 'openurl-host-log', 5_000, async () => {
      const result = await this.navigationCommand('/usr/bin/log', [
        'show', '--style', 'compact', '--start', start, '--end', end,
        '--predicate', 'process == "simctl" OR process CONTAINS[c] "Simulator" OR process CONTAINS[c] "Safari" OR process CONTAINS[c] "WebKit" OR process == "lsd"',
      ], 5_000, { budget: phase });
      const filename = join(this.outputDir, 'ios-openurl-host.log');
      await writeBoundedText(filename, result.stdout, 1024 * 1024);
      return { filename, scope: 'host unified log', startedAt, settledAt, start, end, durationMs: result.durationMs };
    });
  }

  private async waitForSafariFixturePage(url: string, timeoutMs: number, parent: PhaseBudget): Promise<void> {
    const phase = parent.phaseView('ios-safari-readiness', timeoutMs);
    let fallbackAttempted = false;
    let lastError = '';
    while (!phase.exhausted) {
      try {
        if (phase.remainingMs < IOS_WEBKIT_DISCOVERY_COMMAND_MS + IOS_SAFARI_DISCOVERY_RESERVE_MS) {
          lastError = `not enough time for WebKit discovery and Safari observation (${phase.remainingMs}ms remains)`;
          break;
        }
        const metadata = await this.driver.contextMetadata(IOS_WEBKIT_DISCOVERY_COMMAND_MS);
        const context = metadata.find((candidate) => isIOSSafariBrowserBundle(candidate.bundleId)
          || (candidate.url !== undefined && this.isExpectedOrigin(candidate.url)));
        if (!context) {
          lastError = 'Safari did not publish a web context';
        } else {
          if (phase.remainingMs < IOS_SAFARI_OBSERVATION_MS) break;
          await this.driver.switchContext(context.id, IOS_SAFARI_ATTACH_COMMAND_MS);
          if (phase.remainingMs < 2_000) break;
          const currentUrl = await this.driver.currentUrl(1_000);
          if (this.isExpectedOrigin(currentUrl)) {
            this.lastUrl = currentUrl;
            if (phase.remainingMs < 1_000) break;
            await this.driver.switchContext('NATIVE_APP', 1_000);
            return;
          }
          if (!fallbackAttempted) {
            fallbackAttempted = true;
            const navigateTimeout = phase.remainingMs;
            if (navigateTimeout < minimumDriverRequestMs) break;
            await this.driver.navigate(url, navigateTimeout);
            continue;
          }
          lastError = `Safari page is ${currentUrl || 'not navigated to the fixture origin'}`;
        }
      } catch (error) {
        if (isFatalDriverError(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
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
    throw new Error(`IOS_NAVIGATION: Safari fixture page was not ready (${lastError || 'no page observed'})`);
  }

  async openSetupURLInInstalledApp(url: string): Promise<void> {
    await this.attachToInstalledView();
    await this.driver.navigate(url);
    await delay(1_000);
  }

  async installFromBrowser(): Promise<void> {
    this.assertOwnershipClear();
    const phase = this.budget.phaseView('ios-install', 120_000);
    phase.assertAvailable('start iOS installation');
    await this.driver.switchContext('NATIVE_APP', Math.max(minimumDriverRequestMs, phase.remainingMs));
    await this.setNativeObservationTarget('auto', phase);
    await this.observeNativeForeground('com.apple.mobilesafari', phase);
    const source = await this.captureNativeReadiness('ios-before-share', phase.remainingMs);
    const findTimeout = Math.min(5_000, phase.remainingMs);
    if (findTimeout < minimumDriverRequestMs) throw new Error('IOS_SHARE: insufficient time to inspect Safari toolbar');
    const share = await this.driver.findAnyOnce([
      accessibility('ShareButton'),
      accessibility('Share'),
      accessibility('Share button'),
      textLocator('Share'),
    ], findTimeout).catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
      return '';
    });
    if (share) {
      await this.assertNativeControl(share, 'Share', phase);
      if (phase.remainingMs < IOS_NATIVE_SCROLL_COMMAND_MS + IOS_NATIVE_HIERARCHY_COMMAND_MS) {
        throw new Error('IOS_SHARE: insufficient time to open and observe the Share sheet');
      }
      await this.driver.click(share, IOS_NATIVE_SCROLL_COMMAND_MS);
    } else {
      const bounds = nativeShareBounds(source);
      if (!bounds) throw new Error('IOS_SHARE: no verified enabled Share control was exposed by Safari');
      if (phase.remainingMs < IOS_NATIVE_SCROLL_COMMAND_MS + IOS_NATIVE_HIERARCHY_COMMAND_MS) {
        throw new Error('IOS_SHARE: insufficient time to open and observe the Share sheet');
      }
      await this.driver.mobile('tap', {
        x: Math.round(bounds.x + bounds.width / 2),
        y: Math.round(bounds.y + bounds.height / 2),
      }, IOS_NATIVE_SCROLL_COMMAND_MS);
    }
    await this.clickNativeScrollable([
      iosActionLabelContains('Add to Home Screen'),
    ], 'Add to Home Screen', Math.min(IOS_NATIVE_ACTION_TIMEOUT_MS, phase.remainingMs));
    await withIOSConfirmationSettings(this.driver, phase, IOS_CONFIRMATION_ROUND_MS, async (confirmation) => {
      const addButton = await this.waitForInstallConfirmation(confirmation);
      if (confirmation.remainingMs < IOS_NATIVE_LOOKUP_ROUND_MS) throw new Error('IOS_SHARE: Add: insufficient time to complete confirmation click');
      await this.driver.click(addButton, IOS_NATIVE_LOOKUP_ROUND_MS);
    });
    await delay(Math.min(1_500, phase.remainingMs), phase);
  }

  private async waitForInstallConfirmation(parent: PhaseBudget): Promise<string> {
    const phase = parent.phaseView('ios-install-confirmation', 75_000);
    let lastState = 'missing';
    while (phase.remainingMs >= IOS_CONFIRMATION_ROUND_MS) {
      try {
        if (this.driver.snapshot().selectedContext !== 'NATIVE_APP') throw new Error('IOS_SHARE: Add: confirmation is not in the native context');
        const appInfo = await this.driver.activeAppInfo(IOS_NATIVE_LOOKUP_ROUND_MS);
        const bundleId = String(appInfo?.bundleId || appInfo?.bundleID || '');
        if (!isIOSSafariBrowserBundle(bundleId) && !isIOSSafariViewServiceBundle(bundleId)) {
          throw new Error(`IOS_SHARE: Add: Safari confirmation is not foreground (${bundleId || 'unknown'})`);
        }
        if (phase.remainingMs < IOS_CONFIRMATION_ROUND_MS - IOS_NATIVE_LOOKUP_ROUND_MS) break;
        const response = await this.driver.command<unknown>('/element', 'POST', accessibility('Add'), IOS_CONFIRMATION_LOOKUP_MS);
        const element = this.installConfirmationElementId(response);
        if (phase.remainingMs < IOS_CONFIRMATION_ROUND_MS - IOS_NATIVE_LOOKUP_ROUND_MS - IOS_CONFIRMATION_LOOKUP_MS) break;
        if (await this.installConfirmationIdentity() !== element) {
          lastState = 'confirmation identity is missing or replaced';
        } else {
          if (phase.remainingMs < 4 * IOS_NATIVE_LOOKUP_ROUND_MS + IOS_CONFIRMATION_IDENTITY_MS) break;
          const readiness = phase.phaseView('ios-confirmation-readiness', phase.remainingMs, IOS_CONFIRMATION_IDENTITY_MS + IOS_NATIVE_LOOKUP_ROUND_MS);
          const state = await this.nativeControlState(element, readiness);
          if (state === 'indeterminate' && readiness.remainingMs < IOS_NATIVE_LOOKUP_ROUND_MS) break;
          if (state === 'ready') {
            if (phase.remainingMs < IOS_CONFIRMATION_IDENTITY_MS + IOS_NATIVE_LOOKUP_ROUND_MS) break;
            const currentElement = await this.installConfirmationIdentity();
            if (!currentElement) throw new Error('IOS_SHARE: Add: confirmation identity was replaced before click');
            if (currentElement === element) {
              if (phase.remainingMs < IOS_NATIVE_LOOKUP_ROUND_MS) throw new Error('IOS_SHARE: Add: insufficient time to complete confirmation click');
              return element;
            }
            lastState = 'confirmation Add control was replaced before click';
          } else {
            lastState = state;
          }
        }
      } catch (error) {
        if (!(error instanceof WebDriverError)
          || error.code !== 'APPIUM_COMMAND' || error.timedOut || error.status !== 404
          || error.selectedContext !== 'NATIVE_APP'
          || !/\/elements?$|\/element\/[^/]+\/attribute\/(?:enabled|visible|hittable)$/u.test(error.path)
          || !/"error":"(?:stale element reference|no such element)"/u.test(error.message)) throw error;
        lastState = error.message;
      }
      this.diagnostics.record({ phase: 'ios-install-confirmation', operation: 'confirmation-pending', detail: { state: lastState, remainingMs: phase.remainingMs } });
      if (phase.remainingMs < IOS_NATIVE_LOOKUP_ROUND_MS + 250) break;
      await delay(250, phase);
    }
    throw new Error(`IOS_SHARE: Add: confirmation control was not ready within the complete native lookup/read allowance (${lastState})`);
  }

  private installConfirmationElementId(response: unknown): string {
    const id = response && typeof response === 'object' && 'element-6066-11e4-a52e-4f735466cecf' in response
      ? response['element-6066-11e4-a52e-4f735466cecf'] : undefined;
    if (typeof id !== 'string' || !id.trim()) throw new Error('IOS_SHARE: Add: malformed native element response');
    return id;
  }

  private async installConfirmationIdentity(): Promise<string | undefined> {
    const response = await this.driver.command<unknown>('/elements', 'POST', {
      using: 'xpath',
      value: "//XCUIElementTypeNavigationBar[@name='Add to Home Screen' and @visible='true' and not(ancestor::*[@visible='false'])]//XCUIElementTypeButton[@name='Add']",
    }, IOS_CONFIRMATION_IDENTITY_MS);
    if (!Array.isArray(response)) throw new Error('IOS_SHARE: Add: malformed confirmation identity response');
    const matches = response.map((candidate: unknown) => this.installConfirmationElementId(candidate));
    return matches.length === 1 ? matches[0] : undefined;
  }

  async launchInstalledApp(): Promise<void> {
    this.assertOwnershipClear();
    await requireOwnedDevice('ios', this.udid);
    this.selectedInstalledContext = '';
    const phase = this.budget.phaseView('ios-launch', 120_000);
    phase.assertAvailable('launch installed provider');
    await this.driver.switchContext('NATIVE_APP', Math.max(2, phase.remainingMs));
    await this.setNativeObservationTarget(IOS_SPRINGBOARD_BUNDLE_ID, phase);
    await this.driver.mobile('pressButton', { name: 'home' }, Math.max(2, phase.remainingMs));
    await this.ensureSpringBoardForeground(phase);
    await delay(Math.min(750, phase.remainingMs), phase);
    for (let page = 0; page < 8; page += 1) {
      phase.assertAvailable('show first SpringBoard page');
      await this.driver.mobile('swipe', { direction: 'right' }, Math.max(2, phase.remainingMs));
    }
    for (let page = 0; page < 8; page += 1) {
      phase.assertAvailable('find installed provider icon');
      await this.ensureSpringBoardForeground(phase);
      const icon = await this.findHittableHomeIcon(Math.min(5_000, phase.remainingMs));
      if (icon) {
        await this.setNativeObservationTarget(IOS_INSTALLED_BUNDLE_ID, phase);
        const clickTimeout = phase.remainingMs;
        if (clickTimeout <= 1) break;
        await this.driver.click(icon, clickTimeout);
        const providerTimeout = Math.min(30_000, phase.remainingMs);
        if (providerTimeout <= 1) break;
        if (await this.waitForInstalledProvider(providerTimeout)) {
          const foregroundTimeout = phase.remainingMs;
          if (foregroundTimeout <= 1) break;
          await this.requireInstalledProviderForeground(foregroundTimeout);
          await delay(Math.min(750, phase.remainingMs), phase);
          await this.attachToInstalledView(Math.min(30_000, phase.remainingMs));
          return;
        }
        if (this.installedBindingState === 'bound') this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'installed provider did not return after the planned launch');
        await this.setNativeObservationTarget(IOS_SPRINGBOARD_BUNDLE_ID, phase);
        const homeTimeout = phase.remainingMs;
        if (homeTimeout <= 1) break;
        await this.driver.mobile('pressButton', { name: 'home' }, homeTimeout);
      }
      if (page < 7) {
        await this.ensureSpringBoardForeground(phase);
        const swipeTimeout = phase.remainingMs;
        if (swipeTimeout <= 1) break;
        await this.driver.mobile('swipe', { direction: 'left' }, swipeTimeout);
        await delay(Math.min(500, phase.remainingMs), phase);
      }
    }
    phase.assertAvailable('launch installed provider');
    throw new Error('IOS_CONTEXT: native launch did not identify the installed provider');
  }

  async assertStandalone(origin: string): Promise<RuntimeIdentity> {
    const identity = await this.readRunningIdentity();
    assertStandalone(identity, origin);
    return identity;
  }

  async attachToInstalledView(timeoutMs = 30_000): Promise<void> {
    this.assertOwnershipClear();
    const phase = this.budget.phaseView('ios-attachment', timeoutMs);
    let lastError = '';
    while (!phase.exhausted) {
      phase.assertAvailable('discover installed page');
      if (this.selectedInstalledContext) {
        try {
          phase.assertAvailable('validate cached installed page');
          await this.driver.switchContext('NATIVE_APP', Math.max(1, phase.remainingMs));
          await this.requireInstalledProviderForeground(Math.max(1, phase.remainingMs));
          await this.driver.switchContext(this.selectedInstalledContext, Math.max(1, phase.remainingMs));
          const url = await this.driver.currentUrl(Math.max(1, phase.remainingMs));
          this.lastUrl = url;
          if (!this.isExpectedOrigin(url)) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `cached document origin ${url} is not ${this.origin}`);
          await this.validateInstalledDocument(Math.max(1, phase.remainingMs));
          return;
        } catch (error) {
          const detail = error instanceof Error ? error.message : String(error);
          this.diagnostics.record({ phase: 'ios-attachment', operation: 'cached-context', detail });
          if (!isIOSStaleContextError(error)) throw error;
          this.selectedInstalledContext = '';
          lastError = `cached context: ${detail}`;
        }
      }
      phase.assertAvailable('discover installed page metadata');
      if (phase.remainingMs < IOS_WEBKIT_DISCOVERY_COMMAND_MS + IOS_INSTALLED_FOREGROUND_MS) {
        lastError ||= `not enough time for WebKit discovery and foreground observation (${phase.remainingMs}ms remains)`;
        break;
      }
      const contexts = await this.driver.contextMetadata(IOS_WEBKIT_DISCOVERY_COMMAND_MS);
      phase.assertAvailable('validate installed discovery foreground');
      if (phase.remainingMs < IOS_INSTALLED_FOREGROUND_MS) break;
      await this.requireInstalledProviderForeground(IOS_INSTALLED_FOREGROUND_MS);
      let candidateError: unknown;
      if (!contexts.length) {
        lastError = 'installed page metadata is unavailable';
      } else {
        for (const context of contexts) {
          phase.assertAvailable('select installed page');
          const rejection = iosInstalledContextRejection(context, this.origin);
          this.diagnostics.record({
            phase: 'ios-attachment',
            operation: 'context-candidate',
            context: context.id,
            detail: { bundleId: context.bundleId, url: context.url, title: context.title, raw: context.raw, rejection: rejection || undefined },
          });
          if (context.id === 'NATIVE_APP' || isIOSSafariBrowserBundle(context.bundleId)) continue;
          if (!isIOSSafariViewServiceBundle(context.bundleId) && context.bundleId !== this.installedBundleId) {
            this.failOwnership('IOS_CONTEXT_OWNERSHIP', `page provider ${context.bundleId || 'unknown'} is not an installed provider`);
          }
          if (rejection) {
            if (this.installedBindingState === 'unselected' && !this.selectedInstalledContext
              && isIOSSafariViewServiceBundle(context.bundleId)
              && /^WEBVIEW_\d+\.\d+$/u.test(context.id)
              && context.url === 'about:blank' && context.title === '') {
              lastError = `${context.id}: initial page publication is pending`;
              this.diagnostics.record({
                phase: 'ios-attachment', operation: 'initial-publication-pending', context: context.id,
                nativeProvider: this.installedBundleId, detail: { nativePid: this.lastNativePid, bundleId: context.bundleId },
              });
              continue;
            }
            this.failOwnership('IOS_CONTEXT_OWNERSHIP', rejection);
          }
          try {
            phase.assertAvailable('validate installed page provider');
            await this.driver.switchContext('NATIVE_APP', Math.max(1, phase.remainingMs));
            await this.requireInstalledProviderForeground(Math.max(1, phase.remainingMs));
            await this.driver.switchContext(context.id, Math.max(1, phase.remainingMs));
            if (this.installedBindingState !== 'bound') this.installedBindingState = 'inspecting';
            const url = await this.driver.currentUrl(Math.max(1, phase.remainingMs));
            this.lastUrl = url;
            if (!this.isExpectedOrigin(url)) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `document origin ${url} is not ${this.origin}`);
            await this.validateInstalledDocument(Math.max(1, phase.remainingMs));
            this.selectedInstalledContext = context.id;
            this.installedBindingState = 'bound';
            return;
          } catch (error) {
            if (isFatalDriverError(error) || isQualificationFatal(error)) throw error;
            const detail = error instanceof Error ? error.message : String(error);
            this.diagnostics.record({ phase: 'ios-attachment', operation: 'context-rejected', context: context.id, detail });
            lastError = `${context.id}: ${detail}`;
            if (!isIOSStaleContextError(error) && !isIOSContextNotReadyError(error)) candidateError ||= error;
          }
        }
        if (!lastError) lastError = `no installed page for ${this.origin}`;
      }
      if (candidateError) throw candidateError;
      try {
        await delay(250, phase);
      } catch (error) {
        if (phase.exhausted) {
          this.budget.assertAvailable('discover installed page');
          break;
        }
        throw error;
      }
    }
    this.budget.assertAvailable('discover installed page');
    throw new Error(`IOS_CONTEXT: no installed Home Screen web context for ${this.origin}: ${lastError}`);
  }

  async readRunningIdentity(): Promise<RuntimeIdentity> {
    await this.attachToInstalledView();
    const identity = await this.driver.execute<RuntimeIdentity>(runtimeScript());
    this.lastIdentity = {
      ...identity,
      nativeProvider: this.installedBundleId ? `ios:${this.installedBundleId}` : undefined,
      nativeActivity: this.lastNativeActivity || undefined,
      nativePid: this.lastNativePid || undefined,
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
    await requireOwnedDevice('ios', this.udid);
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
            // Chrome-backed web views can report a visible card button as not
            // interactable after a standalone relaunch. Dispatch the same DOM
            // click only after confirming that the matching enabled button is
            // visible.
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
        if (isFatalDriverError(error) || isQualificationFatal(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250);
    }
    throw new Error(`APPIUM_AGENT: ${relayName}: ${lastError}`);
  }

  async backgroundApp(): Promise<void> {
    this.assertOwnershipClear();
    await requireOwnedDevice('ios', this.udid);
    await this.attachToInstalledView();
    this.selectedInstalledContext = '';
    await this.driver.switchContext('NATIVE_APP');
    await this.setNativeObservationTarget(IOS_SPRINGBOARD_BUNDLE_ID, this.budget);
    await this.driver.mobile('pressButton', { name: 'home' });
    await this.ensureSpringBoardForeground();
  }

  async relaunchInstalledApp(): Promise<void> {
    await this.launchInstalledApp();
  }

  async terminateInstalledApp(): Promise<void> {
    this.assertOwnershipClear();
    await requireOwnedDevice('ios', this.udid);
    await this.attachToInstalledView();
    this.selectedInstalledContext = '';
    await this.driver.switchContext('NATIVE_APP');
    const bundleId = this.installedBundleId;
    if (!bundleId) throw new Error('IOS_TERMINATE: the installed Home Screen app did not expose a native provider id');
    await this.setNativeObservationTarget(IOS_SPRINGBOARD_BUNDLE_ID, this.budget);
    await this.driver.mobile('terminateApp', { bundleId });
    const state = await this.driver.mobile('queryAppState', { bundleId });
    if (state !== 1) this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'installed provider did not terminate');
    await this.driver.mobile('pressButton', { name: 'home' });
    await this.ensureSpringBoardForeground();
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
    await this.driver.mobile('hideKeyboard');
    await this.waitForKeyboard(false);
    await this.attachToInstalledView();
    if (this.keyboardDraft) {
      const value = await this.driver.execute<string>("return document.querySelector('textarea[aria-label=\\\"Prompt\\\"]')?.value || ''");
      if (value !== this.keyboardDraft) throw new Error('IOS_KEYBOARD: draft was not preserved after dismissal');
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
        if (isFatalDriverError(error) || isQualificationFatal(error)) throw error;
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
    const captures: Array<[string, string[]]> = [
      ['simulator-log', ['simctl', 'spawn', this.udid, 'log', 'show', '--last', '10m', '--style', 'compact']],
      ['webkit-safari', ['simctl', 'spawn', this.udid, 'log', 'show', '--last', '10m', '--style', 'compact', '--predicate', 'process CONTAINS[c] "WebKit" OR process CONTAINS[c] "Safari"']],
      ['apps', ['simctl', 'listapps', this.udid]],
      ['wda', ['simctl', 'spawn', this.udid, 'launchctl', 'print', 'system']],
    ];
    for (const [suffix, args] of captures) {
      const output = await commandOutput('xcrun', args, 20_000).catch((error) => error instanceof Error ? error.message : String(error));
      await writeBoundedText(join(this.outputDir, `${name}-ios-${suffix}.log`), output);
    }
  }

  evidenceSnapshot(): Record<string, unknown> {
    return {
      platform: this.name,
      device: this.udid,
      origin: this.origin,
      installedBundleId: this.installedBundleId,
      selectedInstalledContext: this.selectedInstalledContext,
      installedBindingState: this.installedBindingState,
      installedDocumentBound: this.installedBindingState === 'bound',
      ownershipFailure: this.ownershipFailure?.snapshot(),
      nativeObservationTarget: this.nativeObservationTarget,
      springBoardObservationRoot: this.springBoardRoot,
      nativeObservationFailure: this.nativeObservationFailure === undefined ? undefined : String(this.nativeObservationFailure),
      lastUrl: this.lastUrl,
      lastIdentity: this.lastIdentity,
      lastCompletion: this.lastCompletion,
      nativeActivity: this.lastNativeActivity,
      nativePid: this.lastNativePid,
      simulatorReadyAt: this.simulatorReadyAt,
      driver: this.driver.snapshot(),
      events: this.diagnostics.snapshot(),
    };
  }

  async stopOwnedResources(): Promise<void> {
    await this.driver.close();
  }

  private async captureNativeReadiness(name: string, timeoutMs: number): Promise<string> {
    await mkdir(this.outputDir, { recursive: true });
    const deadline = Date.now() + Math.min(timeoutMs, this.budget.remainingMs);
    const phase = this.budget.phaseView('ios-native-readiness', timeoutMs);
    const source = await this.captureNativeHierarchy(name, phase);
    try {
      const screenshotTimeout = deadline - Date.now();
      if (screenshotTimeout >= minimumDriverRequestMs) {
        const screenshot = Buffer.from(await this.driver.screenshot(screenshotTimeout), 'base64');
        if (screenshot.byteLength <= 20 * 1024 * 1024) await writeFile(join(this.outputDir, `${name}.png`), screenshot, { mode: 0o600 });
      }
    } catch (error) {
      if (isFatalDriverError(error)) throw error;
      this.diagnostics.record({ phase: 'ios-install', operation: 'native-screenshot', detail: error instanceof Error ? error.message : String(error) });
    }
    return source;
  }

  private async nativeControlState(element: string, deadline: number | PhaseBudget): Promise<'ready' | 'hidden' | 'disabled' | 'not-hittable' | 'indeterminate'> {
    const read = async (name: string): Promise<string | null> => {
      const timeout = typeof deadline === 'number' ? deadline - Date.now()
        : deadline.remainingMs >= IOS_NATIVE_LOOKUP_ROUND_MS ? IOS_NATIVE_LOOKUP_ROUND_MS : 0;
      if (timeout < minimumDriverRequestMs) return null;
      const response = await this.driver.attribute(element, name, timeout);
      if (deadline instanceof PhaseBudget && response !== null && typeof response !== 'string') {
        throw new Error('IOS_SHARE: Add: malformed native attribute response');
      }
      return response;
    };
    const enabled = await read('enabled');
    if (enabled !== 'true') return enabled === 'false' ? 'disabled' : 'indeterminate';
    const visible = await read('visible');
    if (visible !== 'true') return visible === 'false' ? 'hidden' : 'indeterminate';
    const hittable = await read('hittable');
    if (hittable !== 'true') return hittable === 'false' ? 'not-hittable' : 'indeterminate';
    return 'ready';
  }

  private async assertNativeControl(element: string, description: string, phase: PhaseBudget): Promise<void> {
    const state = await this.nativeControlState(element, Date.now() + phase.remainingMs);
    if (state === 'ready') return;
    if (state === 'disabled') throw new Error(`IOS_SHARE: ${description}: control is disabled`);
    if (state === 'hidden') throw new Error(`IOS_SHARE: ${description}: control is not visible`);
    if (state === 'not-hittable') throw new Error(`IOS_SHARE: ${description}: control is not hittable`);
    if (phase.remainingMs < minimumDriverRequestMs) throw new Error(`IOS_SHARE: ${description}: insufficient time to inspect control`);
    throw new Error(`IOS_SHARE: ${description}: control readiness is indeterminate`);
  }

  private async nativeElementBelongsToRows(element: string, rows: NativeActionRow[], deadline: number): Promise<boolean> {
    const timeout = Math.min(750, deadline - Date.now());
    if (timeout < minimumDriverRequestMs) return false;
    const bounds = await this.driver.elementRect(element, timeout).catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
      return undefined;
    });
    return Boolean(bounds && rows.some((row) => nativeBoundsOverlap(bounds, row.bounds)));
  }

  private async captureNativeHierarchy(name: string, phase: PhaseBudget): Promise<string> {
    if (phase.remainingMs < IOS_NATIVE_HIERARCHY_COMMAND_MS) {
      throw new Error('IOS_SHARE: insufficient time to complete native hierarchy observation');
    }
    const source = await this.driver.pageSource(IOS_NATIVE_HIERARCHY_COMMAND_MS);
    if (typeof source !== 'string' || !source.includes('<AppiumAUT>') || !source.trimEnd().endsWith('</AppiumAUT>')) {
      throw new Error('IOS_SHARE: incomplete native hierarchy response');
    }
    await writeBoundedText(join(this.outputDir, `${name}-hierarchy.xml`), source);
    return source;
  }

  private async captureNativeShareHierarchy(scroll: number, phase: PhaseBudget): Promise<string> {
    return this.captureNativeHierarchy(`ios-share-${scroll}`, phase);
  }

  private async findNativeScrollContainer(
    source: string,
    target: string,
    deadline: number,
  ): Promise<NativeScrollContainer | undefined> {
    const actionList = nativeActionListEvidence(source, target);
    if (!actionList) return undefined;
    const timeout = Math.min(2_000, deadline - Date.now());
    if (timeout < minimumDriverRequestMs) return undefined;
    const elements = await this.driver.findAll(iosActionCollectionLocator(), timeout).catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
      return [];
    });
    for (const element of elements) {
      const rectTimeout = Math.min(750, deadline - Date.now());
      if (rectTimeout < minimumDriverRequestMs) break;
      const bounds = await this.driver.elementRect(element, rectTimeout).catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
        return undefined;
      });
      if (!bounds || bounds.width <= 0 || bounds.height <= 0) continue;
      if (Math.abs(actionList.collection.bounds.x - bounds.x) > 2
        || Math.abs(actionList.collection.bounds.y - bounds.y) > 2
        || Math.abs(actionList.collection.bounds.width - bounds.width) > 2
        || Math.abs(actionList.collection.bounds.height - bounds.height) > 2) continue;
      const enabledTimeout = Math.min(750, deadline - Date.now());
      if (enabledTimeout < minimumDriverRequestMs) break;
      const enabled = await this.driver.attribute(element, 'enabled', enabledTimeout).catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
        return null;
      });
      if (enabled !== 'true' && enabled !== null) continue;
      const visibleTimeout = Math.min(750, deadline - Date.now());
      if (visibleTimeout < minimumDriverRequestMs) break;
      const visible = await this.driver.attribute(element, 'visible', visibleTimeout).catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
        return null;
      });
      if (visible !== 'true' && visible !== null) continue;
      return { element, bounds, type: actionList.collection.type };
    }
    return undefined;
  }

  private async clickNativeScrollable(locators: Locator[], description: string, timeoutMs: number): Promise<void> {
    if (timeoutMs < minimumDriverRequestMs) throw new Error(`IOS_SHARE: ${description}: insufficient time to find control`);
    const deadline = Date.now() + Math.min(timeoutMs, this.budget.remainingMs);
    const findTimeout = deadline - Date.now();
    if (findTimeout < minimumDriverRequestMs) throw new Error(`IOS_SHARE: ${description}: insufficient time to find control`);
    const element = await this.findNativeScrollable(locators, description, findTimeout);
    const clickTimeout = deadline - Date.now();
    if (clickTimeout < minimumDriverRequestMs) throw new Error(`IOS_SHARE: ${description}: insufficient time to click control`);
    await this.driver.click(element, clickTimeout);
  }

  private async findNativeMatches(locators: Locator[], timeoutMs: number): Promise<string[]> {
    const deadline = Date.now() + timeoutMs;
    const matches: string[] = [];
    for (const locator of locators) {
      const remaining = deadline - Date.now();
      if (remaining < minimumDriverRequestMs) break;
      try {
        matches.push(...await this.driver.findAll(locator, Math.min(IOS_NATIVE_LOOKUP_ROUND_MS, remaining)));
      } catch (error) {
        if (isFatalDriverError(error)) throw error;
        if (isCommandAdmissionError(error)) break;
      }
    }
    return [...new Set(matches)];
  }

  private async findNativeScrollable(locators: Locator[], description: string, timeoutMs: number): Promise<string> {
    if (timeoutMs < minimumDriverRequestMs) throw new Error(`IOS_SHARE: ${description}: insufficient time to find control`);
    const deadline = Date.now() + Math.min(timeoutMs, this.budget.remainingMs);
    const phase = this.budget.phaseView('ios-share-search', timeoutMs);
    const readiness = phase.phaseView('ios-share-publication', IOS_NATIVE_LIST_READINESS_MS);
    let lastError = '';
    let source: string | undefined;
    let actionList: NativeActionListEvidence | undefined;
    while (!actionList && readiness.remainingMs >= IOS_NATIVE_HIERARCHY_COMMAND_MS) {
      const currentSource = await this.captureNativeShareHierarchy(0, readiness);
      actionList = nativeActionListEvidence(currentSource, description);
      if (actionList) {
        source = currentSource;
        break;
      }
      lastError = currentSource
        ? `${description}: native action list is not ready`
        : `${description}: native hierarchy is unavailable`;
      if (readiness.remainingMs < IOS_NATIVE_HIERARCHY_COMMAND_MS + 250) break;
      await delay(250, readiness);
    }
    if (!actionList || source === undefined) throw new Error(`IOS_SHARE: ${description}: ${lastError || 'native action list was not ready'}`);

    let scrolls = 0;
    while (phase.remainingMs >= IOS_NATIVE_HIERARCHY_COMMAND_MS) {
      const remaining = phase.remainingMs;
      source = await this.captureNativeShareHierarchy(scrolls, phase);
      actionList = nativeActionListEvidence(source, description);
      if (!actionList) {
        lastError = `${description}: native action list was dismissed or replaced`;
        break;
      }
      const candidates = await this.findNativeMatches(locators, Math.min(remaining, deadline - Date.now()));
      const states: Array<Awaited<ReturnType<IOSPlatform['nativeControlState']>>> = [];
      let readyElement = '';
      for (const candidate of candidates) {
        if (!await this.nativeElementBelongsToRows(candidate, actionList.targetRows, deadline)) continue;
        let state: Awaited<ReturnType<IOSPlatform['nativeControlState']>>;
        try {
          state = await this.nativeControlState(candidate, deadline);
        } catch (error) {
          if (isFatalDriverError(error)) throw error;
          if (error instanceof WebDriverError && !isRetryableElementLookupError(error)) throw error;
          lastError = error instanceof Error ? error.message : String(error);
          continue;
        }
        states.push(state);
        if (state === 'ready') {
          readyElement = candidate;
          break;
        }
        if (state === 'disabled') lastError = `${description}: control is disabled`;
        else if (state === 'not-hittable') lastError = `${description}: control is not hittable`;
        else if (state === 'indeterminate') lastError = `${description}: control readiness is indeterminate`;
        else lastError = `${description}: control is not visible`;
      }
      if (readyElement) return readyElement;
      if (states.length > 0 && states.every((state) => state === 'disabled' || state === 'indeterminate')) break;
      if (scrolls >= IOS_NATIVE_SCROLL_LIMIT) {
        lastError = `${description}: native scroll limit ${IOS_NATIVE_SCROLL_LIMIT} reached`;
        break;
      }
      const targetRow = actionList.targetRows[0];
      if (!targetRow) {
        lastError = `${description}: action list did not expose the requested row`;
        break;
      }
      const direction = iosNativeScrollDirection(targetRow.bounds, actionList.collection.bounds);
      if (!direction) {
        lastError = `${description}: target row has no verified movement remaining`;
        break;
      }
      const container = await this.findNativeScrollContainer(source, description, deadline);
      if (!container) {
        lastError = `${description}: no verified native action-list container`;
        break;
      }
      this.diagnostics.record({ phase: 'ios-install', operation: 'share-scroll-container', detail: {
        scroll: scrolls,
        type: container.type,
        element: container.element,
        bounds: container.bounds,
        direction,
        targetBounds: targetRow.bounds,
        actionRows: actionList.rows,
      } });
      const gestureTimeout = IOS_NATIVE_SCROLL_COMMAND_MS;
      if (phase.remainingMs < IOS_NATIVE_SCROLL_COMMAND_MS + IOS_NATIVE_HIERARCHY_COMMAND_MS) {
        lastError = `${description}: insufficient time to complete native scroll and hierarchy verification`;
        break;
      }
      let scrolled = false;
      try {
        await this.driver.mobile('scroll', { element: container.element, direction, distance: 0.75 }, gestureTimeout);
        scrolled = true;
      } catch (error) {
        if (isFatalDriverError(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
        if (isIOSStaleElementError(error) || isCommandAdmissionError(error)) break;
        if (phase.remainingMs < IOS_NATIVE_HIERARCHY_COMMAND_MS) break;
        const fallbackSource = await this.captureNativeShareHierarchy(scrolls, phase);
        const fallbackList = nativeActionListEvidence(fallbackSource, description);
        if (!fallbackList) {
          lastError = `${description}: native action list was dismissed or replaced after scroll failure`;
          break;
        }
        const fallbackContainer = await this.findNativeScrollContainer(fallbackSource, description, deadline);
        if (!fallbackContainer) {
          lastError = `${description}: native action-list container was replaced after scroll failure`;
          break;
        }
        const fallbackDirection = iosNativeSwipeDirection(direction);
        const fallbackTimeout = IOS_NATIVE_SCROLL_COMMAND_MS;
        if (phase.remainingMs < IOS_NATIVE_SCROLL_COMMAND_MS + IOS_NATIVE_HIERARCHY_COMMAND_MS) {
          lastError = `${description}: insufficient time to complete fallback swipe and hierarchy verification`;
          break;
        }
        try {
          await this.driver.mobile('swipe', { element: fallbackContainer.element, direction: fallbackDirection }, fallbackTimeout);
          scrolled = true;
        } catch (fallbackError) {
          if (isFatalDriverError(fallbackError)) throw fallbackError;
          lastError = fallbackError instanceof Error ? fallbackError.message : String(fallbackError);
          if (isCommandAdmissionError(fallbackError) || isIOSStaleElementError(fallbackError)) break;
        }
      }
      if (!scrolled) break;
      const afterSource = await this.captureNativeShareHierarchy(scrolls + 1, phase);
      const afterActionList = nativeActionListEvidence(afterSource, description);
      if (!afterActionList) {
        lastError = `${description}: native action list was dismissed or replaced after scroll`;
        this.diagnostics.record({ phase: 'ios-install', operation: 'share-scroll-progress', detail: {
          scroll: scrolls,
          beforeRows: actionList.rows,
          afterRows: [],
          progressed: false,
          direction,
          modal: false,
        } });
        break;
      }
      const afterMatches = await this.findNativeMatches(locators, Math.min(deadline - Date.now(), 5_000));
      const afterTargetReady = afterActionList.targetRows.some((row) => row.visible && row.enabled);
      let afterReady = false;
      if (afterTargetReady) {
        for (const candidate of afterMatches) {
          if (!await this.nativeElementBelongsToRows(candidate, afterActionList.targetRows, deadline)) continue;
          try {
            if (await this.nativeControlState(candidate, deadline) === 'ready') {
              afterReady = true;
              break;
            }
          } catch (error) {
            if (isFatalDriverError(error)) throw error;
            if (error instanceof WebDriverError && !isRetryableElementLookupError(error)) throw error;
          }
        }
      }
      const progressed = (afterTargetReady && afterReady) || nativeActionRowsMoved(actionList.rows, afterActionList.rows);
      this.diagnostics.record({ phase: 'ios-install', operation: 'share-scroll-progress', detail: {
        scroll: scrolls,
        beforeRows: actionList.rows,
        afterRows: afterActionList.rows,
        beforeTargetRows: actionList.targetRows,
        afterTargetRows: afterActionList.targetRows,
        direction,
        targetReady: afterReady,
        progressed,
      } });
      if (!progressed) {
        lastError = `${description}: native action-list scroll made no verified progress`;
        break;
      }
      scrolls += 1;
    }
    throw new Error(`IOS_SHARE: ${description}: ${lastError || 'control was not usable before the deadline'}`);
  }

  private async findHittableHomeIcon(timeoutMs: number): Promise<string> {
    const locators = [
      accessibility('Herdr Mobile Relay'),
      textLocator('Herdr Mobile Relay'),
      accessibility('Herdr Relay'),
      textLocator('Herdr Relay'),
      {
        using: 'xpath',
        value: "//*[@name='Home screen icons']//*[contains(@name, 'Herdr Mobile Relay') or contains(@name, 'Herdr Relay') or contains(@label, 'Herdr Mobile Relay') or contains(@label, 'Herdr Relay')]",
      },
    ];
    const deadline = Date.now() + Math.min(timeoutMs, this.budget.remainingMs);
    for (const locator of locators) {
      const remaining = deadline - Date.now();
      if (remaining <= 1) break;
      const elements = await this.driver.findAll(locator, remaining).catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
        return [];
      });
      for (const element of elements) {
        const hittableTimeout = deadline - Date.now();
        if (hittableTimeout <= 1) return '';
        const hittable = await this.driver.attribute(element, 'hittable', hittableTimeout).catch((error: unknown) => {
          if (isFatalDriverError(error)) throw error;
          return null;
        });
        if (hittable === 'true') return element;
        const visibleTimeout = deadline - Date.now();
        if (visibleTimeout <= 1) return '';
        const visible = await this.driver.attribute(element, 'visible', visibleTimeout).catch((error: unknown) => {
          if (isFatalDriverError(error)) throw error;
          return null;
        });
        if (hittable === null && visible === 'true') return element;
      }
    }
    return '';
  }

  private async waitForInstalledProvider(timeoutMs: number): Promise<boolean> {
    const phase = this.budget.phaseView('ios-provider', timeoutMs);
    while (!phase.exhausted) {
      phase.assertAvailable('discover installed provider');
      await this.driver.switchContext('NATIVE_APP', Math.max(1, phase.remainingMs));
      const state = await this.driver.mobile('queryAppState', { bundleId: IOS_INSTALLED_BUNDLE_ID }, phase.remainingMs);
      if (state === 4) {
        await this.observeNativeForeground(IOS_INSTALLED_BUNDLE_ID, phase);
        this.installedBundleId = IOS_INSTALLED_BUNDLE_ID;
        return true;
      }
      if (state !== 1 && state !== 2 && state !== 3) this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'installed provider state is missing or invalid');
      const homeState = await this.driver.mobile('queryAppState', { bundleId: IOS_SPRINGBOARD_BUNDLE_ID }, phase.remainingMs);
      if (homeState !== 4) this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'neither the launch screen nor installed provider is foreground');
      if (phase.exhausted) {
        this.budget.assertAvailable('discover installed provider');
        return false;
      }
      try {
        await delay(250, phase);
      } catch (error) {
        if (phase.exhausted) {
          this.budget.assertAvailable('discover installed provider');
          return false;
        }
        throw error;
      }
    }
    this.budget.assertAvailable('discover installed provider');
    return false;
  }

  private async setNativeObservationTarget(target: string, phase: PhaseBudget): Promise<void> {
    this.assertOwnershipClear();
    const settings = { defaultActiveApplication: target, respectSystemAlerts: true };
    try {
      if (phase.remainingMs < IOS_NATIVE_LOOKUP_ROUND_MS * 2) throw new Error('IOS_NATIVE_SETTINGS: insufficient time to apply and read back the observation target');
      const response = await this.driver.updateSettings(settings, IOS_NATIVE_LOOKUP_ROUND_MS);
      if (response !== null) throw new Error('IOS_NATIVE_SETTINGS: malformed settings acknowledgement');
      if (phase.remainingMs < IOS_NATIVE_LOOKUP_ROUND_MS) throw new Error('IOS_NATIVE_SETTINGS: insufficient time to read back the observation target');
      const actual = await this.driver.settings(IOS_NATIVE_LOOKUP_ROUND_MS);
      if (!actual || Object.entries(settings).some(([key, value]) => actual[key] !== value)) {
        throw new Error('IOS_NATIVE_SETTINGS: observation target readback did not match');
      }
      this.nativeObservationTarget = target;
      this.diagnostics.record({ phase: phase.phase, operation: 'native-observation-target', detail: settings });
    } catch (error) {
      if (!isFatalDriverError(error)) this.nativeObservationFailure ??= error;
      throw error;
    }
  }

  private async observeNativeForeground(expected: string, phase: PhaseBudget): Promise<void> {
    this.assertOwnershipClear();
    try {
      if (this.driver.snapshot().selectedContext !== 'NATIVE_APP') await this.driver.switchContext('NATIVE_APP', phase.remainingMs);
      const state = await this.driver.mobile('queryAppState', { bundleId: expected }, phase.remainingMs);
      if (state !== 4) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `native provider ${expected} is not foreground (state ${String(state)})`);
      if (expected === IOS_INSTALLED_BUNDLE_ID) await this.requireUnobscuredSystem(phase);
      try {
        await this.driver.command('/alert/text', 'GET', undefined, phase.remainingMs);
        this.failOwnership('IOS_CONTEXT_OWNERSHIP', `native dialog obscures ${expected}`);
      } catch (error) {
        if (!(error instanceof WebDriverError) || error.code !== 'APPIUM_COMMAND' || error.status !== 404
          || !/"error":"no such alert"/u.test(error.message)) throw error;
      }
      const info = await this.driver.activeAppInfo(phase.remainingMs);
      const active = String(info?.bundleId || info?.bundleID || '');
      const pid = info?.pid;
      if (active !== expected || (typeof pid !== 'number' && typeof pid !== 'string')
        || !/^[1-9]\d*$/u.test(String(pid)) || !Number.isSafeInteger(Number(pid))) {
        this.failOwnership('IOS_CONTEXT_OWNERSHIP', `native provider ${expected} is not identified in foreground (${active || 'unknown'}, PID ${String(pid)})`);
      }
      this.lastNativeActivity = String(info?.activity || info?.appActivity || '');
      this.lastNativePid = String(pid);
    } catch (error) {
      if (!isFatalDriverError(error)) this.nativeObservationFailure ??= error;
      throw error;
    }
  }

  private async requireUnobscuredSystem(phase: PhaseBudget): Promise<void> {
    if (!this.springBoardRoot) this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'independent system observation root is unavailable');
    const response = await this.driver.command<unknown>(`/element/${encodeURIComponent(this.springBoardRoot)}/elements`, 'POST', {
      using: 'xpath',
      value: "self::XCUIElementTypeApplication | .//XCUIElementTypeAlert | .//*[@name='SBTransientOverlayWindow' or @name='NotificationShortLookView']",
    }, phase.remainingMs);
    if (!Array.isArray(response) || response.length !== 1 || this.installConfirmationElementId(response[0]) !== this.springBoardRoot) {
      this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'system observation is missing, replaced, or contains an overlay');
    }
  }

  private async ensureSpringBoardForeground(parent = this.budget): Promise<void> {
    this.assertOwnershipClear();
    const phase = parent.phaseView('ios-springboard', 30_000);
    try {
      await this.driver.switchContext('NATIVE_APP', phase.remainingMs);
      await this.observeNativeForeground(IOS_SPRINGBOARD_BUNDLE_ID, phase);
      const roots = await this.driver.command<unknown>('/elements', 'POST', {
        using: 'xpath', value: '//XCUIElementTypeApplication',
      }, phase.remainingMs);
      if (!Array.isArray(roots) || roots.length !== 1) this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'SpringBoard observation root is not unique');
      this.springBoardRoot = this.installConfirmationElementId(roots[0]);
      await this.requireUnobscuredSystem(phase);
    } catch (error) {
      if (!isFatalDriverError(error)) this.nativeObservationFailure ??= error;
      throw error;
    }
  }

  private async requireInstalledProviderForeground(timeoutMs = 30_000): Promise<void> {
    if (!this.installedBundleId) throw new Error('IOS_CONTEXT: installed provider identity is unavailable');
    if (this.installedBundleId !== IOS_INSTALLED_BUNDLE_ID) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `installed provider ${this.installedBundleId} is not ${IOS_INSTALLED_BUNDLE_ID}`);
    await this.observeNativeForeground(this.installedBundleId, this.budget.phaseView('ios-native-foreground', Math.min(30_000, timeoutMs)));
  }

  private async validateInstalledDocument(timeoutMs?: number): Promise<void> {
    const document = await this.driver.execute<{ origin: string; standalone: boolean; applicationInitialized?: boolean }>(`return {
      origin: location.origin,
      standalone: window.matchMedia('(display-mode: standalone)').matches || navigator.standalone === true,
      applicationInitialized: Boolean(document.getElementById('app')?.childNodes.length),
    };`, [], timeoutMs);
    if (document.origin !== this.origin) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `document origin ${document.origin} is not ${this.origin}`);
    if (document.standalone !== true) {
      if (this.installedBindingState !== 'bound' && !this.selectedInstalledContext && document.applicationInitialized === false) {
        throw new Error('IOS_CONTEXT_NOT_READY: installed page has not entered standalone display mode');
      }
      this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'selected page is not standalone');
    }
  }

  private isExpectedOrigin(value: string): boolean {
    try {
      return new URL(value).origin === new URL(this.origin).origin;
    } catch {
      return false;
    }
  }

  private async waitForKeyboard(expected: boolean): Promise<void> {
    const phase = this.budget.phaseView('ios-keyboard', 10_000);
    let last = '';
    while (!phase.exhausted) {
      phase.assertAvailable(`keyboard ${expected ? 'show' : 'hide'}`);
      await this.driver.switchContext('NATIVE_APP', Math.max(2, phase.remainingMs));
      const findTimeout = Math.min(2_000, phase.remainingMs);
      if (findTimeout <= 1) break;
      const elements = await this.driver.findAll({ using: 'class name', value: 'XCUIElementTypeKeyboard' }, findTimeout).catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
        return [];
      });
      let visible = false;
      for (const element of elements) {
        const rectTimeout = Math.min(1_000, phase.remainingMs);
        if (rectTimeout <= 1) break;
        const rect = await this.driver.elementRect(element, rectTimeout).catch((error: unknown) => {
          if (isFatalDriverError(error)) throw error;
          return { x: 0, y: 0, width: 0, height: 0 };
        });
        const shownTimeout = Math.min(1_000, phase.remainingMs);
        if (shownTimeout <= 1) break;
        const shown = await this.driver.attribute(element, 'visible', shownTimeout).catch((error: unknown) => {
          if (isFatalDriverError(error)) throw error;
          return null;
        });
        if (rect.width > 0 && rect.height > 0 && shown !== 'false') visible = true;
      }
      last = `visible=${visible}`;
      if (visible === expected) return;
      await delay(250, phase);
    }
    throw new Error(`IOS_KEYBOARD: expected ${expected ? 'visible' : 'hidden'} native keyboard (${last})`);
  }

  private assertOwnershipClear(): void {
    if (this.ownershipFailure) throw this.ownershipFailure;
    if (this.nativeObservationFailure !== undefined) throw this.nativeObservationFailure;
  }

  private failOwnership(code: string, detail: string): never {
    this.ownershipFailure ||= qualificationFatal(code, detail, 'ownership');
    throw this.ownershipFailure;
  }

  private async currentForegroundPackage(): Promise<string> {
    const info = await this.driver.activeAppInfo();
    return String(info?.bundleId || info?.bundleID || '');
  }
}
