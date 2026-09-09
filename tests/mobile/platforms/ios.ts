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
import { CommandError, command, commandOutput } from '../support/process';
import { requireOwnedDevice } from '../support/device';
import {
  accessibility,
  accessibilityPrefix,
  AppiumClient,
  isCommandAdmissionError,
  buttonText,
  css,
  delay,
  isFatalDriverError,
  minimumDriverRequestMs,
  textLocator,
  type ContextMetadata,
  type Locator,
  WebDriverError,
} from '../support/webdriver';
import { runtimeScript, updateCompletionScript, type MobilePlatform, type PlatformOptions, type UpdateCompletionEvidence } from './types';

const IOS_INSTALLED_BUNDLE_ID = 'com.apple.webapp';
const IOS_OPENURL_COMMAND_MS = 30_000;
const IOS_WEBVIEW_CONNECT_TIMEOUT_MS = 15_000;
const IOS_WEBVIEW_CONNECT_RETRIES = 30;
const IOS_WEBKIT_DISCOVERY_COMMAND_MS = 18_000;
const IOS_SAFARI_READINESS_PHASE_MS = IOS_WEBKIT_DISCOVERY_COMMAND_MS * 2 + 10_000;
const IOS_NAVIGATION_PHASE_MS = IOS_OPENURL_COMMAND_MS + IOS_SAFARI_READINESS_PHASE_MS + 10_000;
const IOS_NATIVE_LOOKUP_ROUND_MS = 5_000;
const IOS_NATIVE_SCROLL_COMMAND_MS = 5_000;
const IOS_NATIVE_SCROLL_LIMIT = 8;
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
      exitCode: error.exitCode,
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

function iosLabelContains(value: string): Locator {
  return {
    using: 'xpath',
    value: `//*[contains(@label, '${value}') or contains(@name, '${value}') or contains(@value, '${value}') or contains(@text, '${value}')]`,
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
  targetAncestor: boolean;
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

function isNativeScrollableType(type: string): boolean {
  return type === 'XCUIElementTypeCollectionView'
    || type === 'XCUIElementTypeTable'
    || type === 'XCUIElementTypeScrollView'
    || type === 'XCUIElementTypeOther';
}

function nativeScrollableEvidence(source: string, target: string): NativeScrollEvidence[] {
  const tokens = source.match(/<\/?[^>]+>/gu) || [];
  const stack: Array<{ type: string; bounds?: NativeBounds; targetAncestor: boolean; evidence?: NativeScrollEvidence }> = [];
  const evidence: NativeScrollEvidence[] = [];
  for (const token of tokens) {
    if (/^<\//u.test(token)) {
      stack.pop();
      continue;
    }
    const type = nativeNodeType(token);
    if (!type) continue;
    const bounds = nativeNodeBounds(token);
    const labels = nativeNodeLabels(token);
    const targetNode = target.length > 0 && labels.some((label) => label.toLowerCase().includes(target.toLowerCase()));
    const stackTarget = stack.some((node) => node.targetAncestor);
    const node: { type: string; bounds?: NativeBounds; targetAncestor: boolean; evidence?: NativeScrollEvidence } = {
      type, bounds, targetAncestor: targetNode || stackTarget,
    };
    if (bounds && isNativeScrollableType(type)) {
      const nodeEvidence = { type, bounds, targetAncestor: node.targetAncestor };
      node.evidence = nodeEvidence;
      evidence.push(nodeEvidence);
    }
    if (targetNode) {
      for (const ancestor of stack) {
        ancestor.targetAncestor = true;
        if (ancestor.evidence) ancestor.evidence.targetAncestor = true;
      }
    }
    if (!token.endsWith('/>')) stack.push(node);
  }
  return evidence;
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

function nativeScrollBarProgress(source: string): string {
  return (source.match(/<XCUIElementTypeOther\b[^>]*(?:name|label)="Vertical scroll bar[^"]*"[^>]*>/giu) || [])
    .map((node) => nativeAttribute(node, 'value') || '')
    .filter(Boolean)
    .join('|');
}

function nativeSourceTargetVisible(source: string, target: string): boolean {
  const nodes = source.match(/<XCUIElementType\w+\b[^>]*>/gu) || [];
  const matching = nodes.filter((node) => nativeNodeLabels(node).some((label) => label.toLowerCase().includes(target.toLowerCase())));
  if (!matching.length) return false;
  return matching.some((node) => nativeAttribute(node, 'visible') !== 'false');
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
  private lastIdentity?: RuntimeIdentity;
  private lastUrl = '';
  private keyboardDraft = '';
  private lastCompletion?: UpdateCompletionEvidence;
  private lastNativeActivity = '';
  private lastNativePid = '';
  private ownershipFailure?: QualificationFatalError;

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
    await command('xcrun', ['simctl', 'bootstatus', this.udid, '-b'], 300_000);
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
  }

  async openSetupURL(url: string): Promise<void> {
    const phase = this.budget.phaseView('ios-navigation', IOS_NAVIGATION_PHASE_MS);
    let lastError: unknown;
    let attempt = 0;
    while (!phase.exhausted) {
      phase.assertAvailable('open setup URL');
      const timeoutMs = Math.min(IOS_OPENURL_COMMAND_MS, phase.remainingMs);
      if (timeoutMs < minimumDriverRequestMs) break;
      attempt += 1;
      try {
        await command('xcrun', ['simctl', 'openurl', this.udid, url], timeoutMs, {
          budget: phase,
          label: 'simctl openurl setup URL',
        });
        this.diagnostics.record({ phase: 'ios-navigation', operation: 'simctl openurl succeeded', detail: { attempt } });
        lastError = undefined;
        break;
      } catch (error) {
        lastError = error;
        this.diagnostics.record({
          phase: 'ios-navigation',
          operation: 'simctl openurl failed',
          durationMs: error instanceof CommandError ? error.durationMs : undefined,
          timeoutMs,
          timedOut: error instanceof CommandError ? error.timedOut : undefined,
          signal: error instanceof CommandError ? error.signal : undefined,
          detail: { attempt, kind: iosOpenURLFailureKind(error), process: iosOpenURLProcessEvidence(error) },
        });
        const kind = iosOpenURLFailureKind(error);
        if (kind !== 'transient') break;
        const waitMs = Math.min(1_000, Math.max(0, phase.remainingMs - minimumDriverRequestMs));
        if (waitMs < minimumDriverRequestMs) break;
        await delay(waitMs, phase);
      }
    }
    if (lastError) {
      const evidence = iosOpenURLProcessEvidence(lastError);
      throw new Error(`IOS_NAVIGATION: could not open setup URL: ${JSON.stringify(evidence)}`, { cause: lastError });
    }
    if (phase.exhausted) throw new Error('IOS_NAVIGATION: setup URL budget expired before navigation completed');
    const readinessTimeout = Math.min(IOS_SAFARI_READINESS_PHASE_MS, phase.remainingMs);
    if (readinessTimeout < minimumDriverRequestMs) throw new Error('IOS_NAVIGATION: no time remained to verify Safari fixture navigation');
    await this.waitForSafariFixturePage(url, readinessTimeout, phase);
  }

  private async waitForSafariFixturePage(url: string, timeoutMs: number, parent: PhaseBudget): Promise<void> {
    const phase = parent.phaseView('ios-safari-readiness', timeoutMs);
    let fallbackAttempted = false;
    let lastError = '';
    while (!phase.exhausted) {
      try {
        if (phase.remainingMs < IOS_WEBKIT_DISCOVERY_COMMAND_MS) {
          lastError = `not enough time for WebKit discovery (${phase.remainingMs}ms remains; ${IOS_WEBKIT_DISCOVERY_COMMAND_MS}ms required)`;
          break;
        }
        const metadata = await this.driver.contextMetadata(IOS_WEBKIT_DISCOVERY_COMMAND_MS);
        const context = metadata.find((candidate) => isIOSSafariBrowserBundle(candidate.bundleId)
          || (candidate.url !== undefined && this.isExpectedOrigin(candidate.url)));
        if (!context) {
          lastError = 'Safari did not publish a web context';
        } else {
          const switchTimeout = phase.remainingMs;
          if (switchTimeout < minimumDriverRequestMs) break;
          await this.driver.switchContext(context.id, switchTimeout);
          const currentUrlTimeout = phase.remainingMs;
          if (currentUrlTimeout < minimumDriverRequestMs) break;
          const currentUrl = await this.driver.currentUrl(currentUrlTimeout);
          if (this.isExpectedOrigin(currentUrl)) {
            this.lastUrl = currentUrl;
            const nativeTimeout = phase.remainingMs;
            if (nativeTimeout < minimumDriverRequestMs) break;
            await this.driver.switchContext('NATIVE_APP', nativeTimeout);
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
    const phase = this.budget.phaseView('ios-install', 120_000);
    phase.assertAvailable('start iOS installation');
    await this.driver.switchContext('NATIVE_APP', Math.max(minimumDriverRequestMs, phase.remainingMs));
    const appInfo = await this.driver.activeAppInfo(Math.max(minimumDriverRequestMs, phase.remainingMs));
    const bundleId = String(appInfo?.bundleId || appInfo?.bundleID || '');
    if (!isIOSSafariBrowserBundle(bundleId)) throw new Error(`IOS_SHARE: Safari is not foreground (${bundleId || 'unknown'})`);
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
      await this.driver.click(share, Math.max(minimumDriverRequestMs, phase.remainingMs));
    } else {
      const bounds = nativeShareBounds(source);
      if (!bounds) throw new Error('IOS_SHARE: no verified enabled Share control was exposed by Safari');
      phase.assertAvailable('tap verified Safari Share control');
      await this.driver.mobile('tap', {
        x: Math.round(bounds.x + bounds.width / 2),
        y: Math.round(bounds.y + bounds.height / 2),
      }, Math.max(minimumDriverRequestMs, phase.remainingMs));
    }
    await this.clickNativeScrollable([
      iosLabelContains('Add to Home Screen'),
      textLocator('Add to Home Screen'),
      accessibility('Add to Home Screen'),
      textLocator('Add to Home Screen…'),
      accessibility('Add to Home Screen…'),
    ], 'Add to Home Screen', Math.min(IOS_NATIVE_ACTION_TIMEOUT_MS, phase.remainingMs));
    const openAsWebApp = phase.remainingMs < minimumDriverRequestMs
      ? ''
      : await this.driver.findAnyOnce([
        iosLabelContains('Open as Web App'),
        textLocator('Open as Web App'),
        accessibility('Open as Web App'),
        textLocator('Open as Web App…'),
        accessibility('Open as Web App…'),
      ], Math.min(5_000, phase.remainingMs)).catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
        return '';
      });
    if (openAsWebApp) {
      await this.assertNativeControl(openAsWebApp, 'Open as Web App', phase);
      await this.driver.click(openAsWebApp, Math.max(minimumDriverRequestMs, phase.remainingMs));
    }
    const addTimeout = Math.min(15_000, phase.remainingMs);
    if (addTimeout < minimumDriverRequestMs) throw new Error('IOS_SHARE: Add: insufficient time to find control');
    const addButton = await this.driver.findAnyOnce([
      textLocator('Add'),
      accessibility('Add'),
    ], addTimeout);
    await this.assertNativeControl(addButton, 'Add', phase);
    await this.driver.click(addButton, Math.max(minimumDriverRequestMs, phase.remainingMs));
    await delay(Math.min(1_500, phase.remainingMs), phase);
  }

  async launchInstalledApp(): Promise<void> {
    await requireOwnedDevice('ios', this.udid);
    this.selectedInstalledContext = '';
    const phase = this.budget.phaseView('ios-launch', 120_000);
    phase.assertAvailable('launch installed provider');
    await this.driver.switchContext('NATIVE_APP', Math.max(2, phase.remainingMs));
    await this.driver.mobile('pressButton', { name: 'home' }, Math.max(2, phase.remainingMs));
    await this.ensureSpringBoardForeground();
    await delay(Math.min(750, phase.remainingMs), phase);
    for (let page = 0; page < 8; page += 1) {
      phase.assertAvailable('show first SpringBoard page');
      await this.driver.mobile('swipe', { direction: 'right' }, Math.max(2, phase.remainingMs));
    }
    for (let page = 0; page < 8; page += 1) {
      phase.assertAvailable('find installed provider icon');
      await this.ensureSpringBoardForeground();
      const icon = await this.findHittableHomeIcon(Math.min(5_000, phase.remainingMs));
      if (icon) {
        const clickTimeout = phase.remainingMs;
        if (clickTimeout <= 1) break;
        await this.driver.click(icon, clickTimeout);
        const providerTimeout = Math.min(5_000, phase.remainingMs);
        if (providerTimeout <= 1) break;
        if (await this.waitForInstalledProvider(providerTimeout)) {
          const activateTimeout = phase.remainingMs;
          if (activateTimeout <= 1) break;
          await this.driver.mobile('activateApp', { bundleId: this.installedBundleId }, activateTimeout);
          const foregroundTimeout = phase.remainingMs;
          if (foregroundTimeout <= 1) break;
          await this.requireInstalledProviderForeground(foregroundTimeout);
          await delay(Math.min(750, phase.remainingMs), phase);
          await this.attachToInstalledView();
          return;
        }
        const homeTimeout = phase.remainingMs;
        if (homeTimeout <= 1) break;
        await this.driver.mobile('pressButton', { name: 'home' }, homeTimeout);
      }
      if (page < 7) {
        await this.ensureSpringBoardForeground();
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
      const contexts = await this.driver.contextMetadata(Math.max(1, phase.remainingMs));
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
          if (rejection) {
            if (this.installedBundleId && isIOSSafariViewServiceBundle(context.bundleId) && context.url && !this.isExpectedOrigin(context.url)) {
              this.failOwnership('IOS_CONTEXT_OWNERSHIP', rejection);
            }
            continue;
          }
          try {
            phase.assertAvailable('validate installed page provider');
            await this.driver.switchContext('NATIVE_APP', Math.max(1, phase.remainingMs));
            await this.requireInstalledProviderForeground(Math.max(1, phase.remainingMs));
            await this.driver.switchContext(context.id, Math.max(1, phase.remainingMs));
            const url = await this.driver.currentUrl(Math.max(1, phase.remainingMs));
            this.lastUrl = url;
            if (!this.isExpectedOrigin(url)) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `document origin ${url} is not ${this.origin}`);
            await this.validateInstalledDocument(Math.max(1, phase.remainingMs));
            this.selectedInstalledContext = context.id;
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
      if (this.installedBundleId) {
        phase.recovery('reactivate installed provider');
        phase.assertAvailable('reactivate installed provider');
        await this.driver.switchContext('NATIVE_APP', Math.max(1, phase.remainingMs));
        await this.driver.mobile('activateApp', { bundleId: this.installedBundleId }, Math.max(1, phase.remainingMs));
      }
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
    await requireOwnedDevice('ios', this.udid);
    await this.driver.switchContext('NATIVE_APP').catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
    });
    await this.driver.mobile('pressButton', { name: 'home' });
    await delay(500);
  }

  async relaunchInstalledApp(): Promise<void> {
    await this.launchInstalledApp();
  }

  async terminateInstalledApp(): Promise<void> {
    await requireOwnedDevice('ios', this.udid);
    await this.driver.switchContext('NATIVE_APP').catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
    });
    const bundleId = this.installedBundleId;
    if (!bundleId) throw new Error('IOS_TERMINATE: the installed Home Screen app did not expose a native provider id');
    await this.driver.mobile('terminateApp', { bundleId });
    await this.driver.mobile('pressButton', { name: 'home' });
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
        const element = await this.driver.findAny([buttonText(text), accessibility(text), accessibilityPrefix(text), textLocator(text)], findTimeout);
        const attributeTimeout = deadline - Date.now();
        if (attributeTimeout < minimumDriverRequestMs) break;
        if ((await this.driver.attribute(element, 'disabled', attributeTimeout)) === null) {
          const clickTimeout = deadline - Date.now();
          if (clickTimeout < minimumDriverRequestMs) break;
          await this.driver.click(element, clickTimeout);
          return;
        }
        lastError = `${text} is disabled`;
      } catch (error) {
        if (isFatalDriverError(error) || isQualificationFatal(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
      }
      const waitMs = Math.min(250, Math.max(0, deadline - Date.now() - minimumDriverRequestMs));
      if (waitMs > 0) await delay(waitMs);
    }
    throw new Error(`APPIUM_BUTTON: ${text}: ${lastError}`);
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
      ownershipFailure: this.ownershipFailure?.snapshot(),
      lastUrl: this.lastUrl,
      lastIdentity: this.lastIdentity,
      lastCompletion: this.lastCompletion,
      nativeActivity: this.lastNativeActivity,
      nativePid: this.lastNativePid,
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
    let source = '';
    try {
      const sourceTimeout = deadline - Date.now();
      if (sourceTimeout >= minimumDriverRequestMs) {
        source = await this.driver.pageSource(sourceTimeout);
        await writeBoundedText(join(this.outputDir, `${name}-hierarchy.xml`), source);
      }
    } catch (error) {
      if (isFatalDriverError(error)) throw error;
      this.diagnostics.record({ phase: 'ios-install', operation: 'native-hierarchy', detail: error instanceof Error ? error.message : String(error) });
    }
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

  private async nativeControlState(element: string, deadline: number): Promise<'ready' | 'hidden' | 'disabled' | 'not-hittable' | 'indeterminate'> {
    const enabledTimeout = deadline - Date.now();
    if (enabledTimeout < minimumDriverRequestMs) return 'indeterminate';
    const enabled = await this.driver.attribute(element, 'enabled', enabledTimeout);
    if (enabled !== 'true') return enabled === 'false' ? 'disabled' : 'indeterminate';
    const visibleTimeout = deadline - Date.now();
    if (visibleTimeout < minimumDriverRequestMs) return 'indeterminate';
    const visible = await this.driver.attribute(element, 'visible', visibleTimeout);
    if (visible !== 'true') return visible === 'false' ? 'hidden' : 'indeterminate';
    const hittableTimeout = deadline - Date.now();
    if (hittableTimeout < minimumDriverRequestMs) return 'indeterminate';
    const hittable = await this.driver.attribute(element, 'hittable', hittableTimeout);
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

  private async captureNativeShareHierarchy(scroll: number, deadline: number): Promise<string> {
    const timeout = Math.min(5_000, deadline - Date.now());
    if (timeout < minimumDriverRequestMs) return '';
    try {
      const source = await this.driver.pageSource(timeout);
      await writeBoundedText(join(this.outputDir, `ios-share-${scroll}-hierarchy.xml`), source);
      return source;
    } catch (error) {
      if (isFatalDriverError(error)) throw error;
      this.diagnostics.record({ phase: 'ios-install', operation: 'share-hierarchy', detail: error instanceof Error ? error.message : String(error) });
      return '';
    }
  }

  private async findNativeScrollContainer(
    source: string,
    target: string,
    targetBounds: NativeBounds | undefined,
    deadline: number,
  ): Promise<NativeScrollContainer | undefined> {
    const evidence = nativeScrollableEvidence(source, target);
    const typedEvidence = evidence.filter((entry) => entry.type !== 'XCUIElementTypeOther');
    const types = ['XCUIElementTypeCollectionView', 'XCUIElementTypeTable', 'XCUIElementTypeScrollView'];
    if (evidence.some((entry) => entry.type === 'XCUIElementTypeOther' && entry.targetAncestor)
      && !typedEvidence.some((entry) => entry.targetAncestor)) types.push('XCUIElementTypeOther');
    const candidates: Array<NativeScrollContainer & { score: number }> = [];
    for (const type of types) {
      const timeout = Math.min(2_000, deadline - Date.now());
      if (timeout < minimumDriverRequestMs) break;
      const elements = await this.driver.findAll({ using: 'class name', value: type }, timeout).catch((error: unknown) => {
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
        const visibleTimeout = Math.min(750, deadline - Date.now());
        if (visibleTimeout < minimumDriverRequestMs) break;
        const visible = await this.driver.attribute(element, 'visible', visibleTimeout).catch((error: unknown) => {
          if (isFatalDriverError(error)) throw error;
          return null;
        });
        if (visible === 'false' || visible === null) continue;
        const matchingEvidence = evidence.filter((entry) => entry.type === type);
        const evidenceMatch = matchingEvidence.find((entry) => nativeBoundsChanged(entry.bounds, bounds) === false
          && Math.abs(entry.bounds.x - bounds.x) <= 2
          && Math.abs(entry.bounds.y - bounds.y) <= 2
          && Math.abs(entry.bounds.width - bounds.width) <= 2
          && Math.abs(entry.bounds.height - bounds.height) <= 2);
        if (!evidenceMatch) continue;
        const horizontalOverlap = targetBounds
          ? Math.max(0, Math.min(bounds.x + bounds.width, targetBounds.x + targetBounds.width) - Math.max(bounds.x, targetBounds.x))
          : 0;
        const typeScore = type === 'XCUIElementTypeCollectionView' ? 4 : type === 'XCUIElementTypeTable' ? 3 : type === 'XCUIElementTypeScrollView' ? 2 : 1;
        const score = (evidenceMatch?.targetAncestor ? 10_000_000 : 0)
          + typeScore * 1_000_000
          + (evidenceMatch ? 10_000 : 0)
          + (horizontalOverlap > 0 ? 1_000 : 0)
          - bounds.width * bounds.height / 1_000_000;
        candidates.push({ element, bounds, type, score });
      }
    }
    return candidates.sort((left, right) => right.score - left.score)[0];
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
    let lastError = '';
    let scrolls = 0;
    while (Date.now() < deadline) {
      const remaining = deadline - Date.now();
      if (remaining < minimumDriverRequestMs) break;
      const candidates = await this.findNativeMatches(locators, remaining);
      const element = candidates[0] || '';
      const states: Array<Awaited<ReturnType<IOSPlatform['nativeControlState']>>> = [];
      for (const candidate of candidates) {
        const state = await this.nativeControlState(candidate, deadline);
        states.push(state);
        if (state === 'ready') return candidate;
        if (state === 'disabled') lastError = `${description}: control is disabled`;
        else if (state === 'not-hittable') lastError = `${description}: control is not hittable`;
        else if (state === 'indeterminate') lastError = `${description}: control readiness is indeterminate`;
        else lastError = `${description}: control is not visible`;
      }
      if (states.length > 0 && (states.every((state) => state === 'disabled') || states.every((state) => state === 'indeterminate'))) break;
      const afterLookup = deadline - Date.now();
      if (afterLookup < minimumDriverRequestMs || scrolls >= IOS_NATIVE_SCROLL_LIMIT) break;
      const beforeSource = await this.captureNativeShareHierarchy(scrolls, deadline);
      const targetRectTimeout = Math.min(750, deadline - Date.now());
      const targetBounds = element && targetRectTimeout >= minimumDriverRequestMs
        ? await this.driver.elementRect(element, targetRectTimeout).catch((error: unknown) => {
          if (isFatalDriverError(error)) throw error;
          return undefined;
        })
        : undefined;
      const container = await this.findNativeScrollContainer(beforeSource, description, targetBounds, deadline);
      if (!container) {
        lastError = `${description}: no verified native action-list container`;
        break;
      }
      this.diagnostics.record({ phase: 'ios-install', operation: 'share-scroll-container', detail: { scroll: scrolls, type: container.type, element: container.element, bounds: container.bounds, targetBounds } });
      const gestureTimeout = Math.min(IOS_NATIVE_SCROLL_COMMAND_MS, deadline - Date.now());
      if (gestureTimeout < minimumDriverRequestMs) break;
      let scrolled = false;
      try {
        await this.driver.mobile('scroll', { element: container.element, direction: 'up', distance: 0.75 }, gestureTimeout);
        scrolled = true;
      } catch (error) {
        if (isFatalDriverError(error)) throw error;
        lastError = error instanceof Error ? error.message : String(error);
        if (isCommandAdmissionError(error)) break;
        const fallbackTimeout = Math.min(IOS_NATIVE_SCROLL_COMMAND_MS, deadline - Date.now());
        if (fallbackTimeout < minimumDriverRequestMs) break;
        try {
          await this.driver.mobile('swipe', { element: container.element, direction: 'up' }, fallbackTimeout);
          scrolled = true;
        } catch (fallbackError) {
          if (isFatalDriverError(fallbackError)) throw fallbackError;
          lastError = fallbackError instanceof Error ? fallbackError.message : String(fallbackError);
          if (isCommandAdmissionError(fallbackError)) break;
        }
      }
      if (!scrolled) break;
      const waitMs = Math.min(250, Math.max(0, deadline - Date.now() - minimumDriverRequestMs));
      if (waitMs > 0) await delay(waitMs);
      const afterSource = await this.captureNativeShareHierarchy(scrolls + 1, deadline);
      const afterTargetRectTimeout = Math.min(750, deadline - Date.now());
      const afterTargetBounds = element && afterTargetRectTimeout >= minimumDriverRequestMs
        ? await this.driver.elementRect(element, afterTargetRectTimeout).catch((error: unknown) => {
          if (isFatalDriverError(error)) throw error;
          return undefined;
        })
        : undefined;
      const afterContainerRectTimeout = Math.min(750, deadline - Date.now());
      const afterContainerBounds = afterContainerRectTimeout >= minimumDriverRequestMs
        ? await this.driver.elementRect(container.element, afterContainerRectTimeout).catch((error: unknown) => {
          if (isFatalDriverError(error)) throw error;
          return undefined;
        })
        : undefined;
      const progressed = nativeBoundsChanged(targetBounds, afterTargetBounds)
        || (nativeScrollBarProgress(beforeSource) !== nativeScrollBarProgress(afterSource)
          && nativeScrollBarProgress(afterSource) !== '')
        || (nativeSourceTargetVisible(beforeSource, description) === false
          && nativeSourceTargetVisible(afterSource, description) === true);
      this.diagnostics.record({ phase: 'ios-install', operation: 'share-scroll-progress', detail: {
        scroll: scrolls,
        beforeTargetBounds: targetBounds,
        afterTargetBounds,
        beforeContainerBounds: container.bounds,
        afterContainerBounds,
        beforeScrollbar: nativeScrollBarProgress(beforeSource),
        afterScrollbar: nativeScrollBarProgress(afterSource),
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
      const appInfo = await this.driver.activeAppInfo(Math.max(1, phase.remainingMs));
      const bundleId = String(appInfo?.bundleId || appInfo?.bundleID || '');
      if (bundleId && !isIOSSafariBrowserBundle(bundleId) && !isIOSSafariViewServiceBundle(bundleId) && !/springboard/iu.test(bundleId)) {
        if (bundleId !== IOS_INSTALLED_BUNDLE_ID) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `foreground provider ${bundleId} is not ${IOS_INSTALLED_BUNDLE_ID}`);
        this.installedBundleId = bundleId;
        return true;
      }
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

  private async ensureSpringBoardForeground(): Promise<void> {
    await this.driver.switchContext('NATIVE_APP');
    const info = await this.driver.activeAppInfo();
    const bundleId = String(info?.bundleId || info?.bundleID || '');
    if (!/springboard/iu.test(bundleId)) {
      await this.driver.mobile('activateApp', { bundleId: 'com.apple.springboard' });
    }
    const foreground = await this.driver.activeAppInfo();
    const active = String(foreground?.bundleId || foreground?.bundleID || '');
    if (!/springboard/iu.test(active)) throw new Error(`IOS_NATIVE: SpringBoard is not foreground (${active || 'unknown'})`);
  }

  private async requireInstalledProviderForeground(timeoutMs?: number): Promise<void> {
    if (!this.installedBundleId) throw new Error('IOS_CONTEXT: installed provider identity is unavailable');
    if (this.installedBundleId !== IOS_INSTALLED_BUNDLE_ID) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `installed provider ${this.installedBundleId} is not ${IOS_INSTALLED_BUNDLE_ID}`);
    const info = await this.driver.activeAppInfo(timeoutMs);
    const active = String(info?.bundleId || info?.bundleID || '');
    this.lastNativeActivity = String(info?.activity || info?.appActivity || '');
    this.lastNativePid = String(info?.pid || '');
    if (active !== this.installedBundleId) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `installed provider ${this.installedBundleId} is not foreground (${active || 'unknown'})`);
  }

  private async validateInstalledDocument(timeoutMs?: number): Promise<void> {
    const document = await this.driver.execute<{ origin: string; standalone: boolean; applicationInitialized?: boolean }>(`return {
      origin: location.origin,
      standalone: window.matchMedia('(display-mode: standalone)').matches || navigator.standalone === true,
      applicationInitialized: Boolean(document.getElementById('app')?.childNodes.length),
    };`, [], timeoutMs);
    if (document.origin !== this.origin) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `document origin ${document.origin} is not ${this.origin}`);
    if (document.standalone !== true) {
      if (document.applicationInitialized === false) throw new Error('IOS_CONTEXT_NOT_READY: installed page has not entered standalone display mode');
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
