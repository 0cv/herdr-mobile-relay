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
import { PhaseBudget, timingScale } from '../support/budget';
import { initializeIOSConfirmationSettings, withIOSConfirmationSettings } from '../support/confirmation-settings';
import { CommandError, command, commandOutput } from '../support/process';
import { collectIOSLaunchReceipt, IOSLaunchObservation, iosReceiptWriteStatus, observeIOSReceiptWrite, type IOSLaunchReceipt } from '../support/ios-launch-receipt';
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
  driverRequestAllowanceMs,
  isFatalDriverError,
  isRetryableElementLookupError,
  minimumDriverRequestMs,
  textLocator,
  type ContextMetadata,
  type Locator,
  type WebDriverRequestTiming,
  type WebDriverRequestPolicy,
  WebDriverError,
} from '../support/webdriver';
import { runtimeScript, updateCompletionScript, type MobilePlatform, type PlatformOptions, type UpdateCompletionEvidence } from './types';

const IOS_INSTALLED_BUNDLE_ID = 'com.apple.webapp';
const IOS_SPRINGBOARD_BUNDLE_ID = 'com.apple.springboard';
const IOS_OPENURL_COMMAND_MS = 90_000;
const IOS_WEBVIEW_CONNECT_TIMEOUT_MS = 5_000;
const IOS_WEBVIEW_CONNECT_RETRIES = 1;
const IOS_WEBKIT_DISCOVERY_COMMAND_MS = 20_000;
// Safari's discovery is the session's first: it locates the simulator's Web Inspector socket and connects, which hosted runners have taken more than 20 s to do.
const IOS_SAFARI_DISCOVERY_COMMAND_MS = 45_000;
const IOS_SAFARI_DISCOVERY_RESERVE_MS = 6_000;
const IOS_SAFARI_ATTACH_COMMAND_MS = 15_000;
const IOS_SAFARI_OBSERVATION_MS = IOS_SAFARI_ATTACH_COMMAND_MS + 2_000;
const IOS_SAFARI_READINESS_PHASE_MS = 2 * IOS_SAFARI_DISCOVERY_COMMAND_MS + IOS_SAFARI_OBSERVATION_MS + 3_000;
const IOS_NAVIGATION_PHASE_MS = IOS_OPENURL_COMMAND_MS + IOS_SAFARI_READINESS_PHASE_MS + 10_000;
const IOS_SAFARI_DIAGNOSTIC_CONTEXT_LIMIT = 64;
const IOS_SAFARI_DIAGNOSTIC_STRING_LIMIT = 2_048;
const IOS_INSTALLED_FOREGROUND_MS = 8_000;
const IOS_KEYBOARD_PHASE_MS = 10_000;
const IOS_PAGE_SCRIPT_MS = 5_000;
const IOS_PAGE_KEYBOARD_MIN_COVER_PX = 250;
const IOS_PAGE_KEYBOARD_SCRIPT = "const composer = document.querySelector('textarea[aria-label=\"Prompt\"]'); const viewport = window.visualViewport; if (!viewport) return null; return { focused: Boolean(composer) && document.activeElement === composer, portrait: window.matchMedia('(orientation: portrait)').matches, zoom: viewport.scale, innerHeight: window.innerHeight, screenHeight: screen.height, viewportHeight: viewport.height, cover: Math.round(Math.max(window.innerHeight, screen.height) - viewport.height) };";

const IOS_NATIVE_LOOKUP_ROUND_MS = 5_000;
const IOS_CONFIRMATION_LOOKUP_MS = 8_000;
const IOS_CONFIRMATION_IDENTITY_MS = 12_000;
const IOS_FINAL_ADD_ACKNOWLEDGEMENT_MS = 12_000;
const IOS_ADD_TO_HOME_SCREEN_ACKNOWLEDGEMENT_MS = 12_000;
const IOS_CONFIRMATION_ROUND_MS = 4 * IOS_NATIVE_LOOKUP_ROUND_MS + IOS_CONFIRMATION_LOOKUP_MS + 2 * IOS_CONFIRMATION_IDENTITY_MS + IOS_FINAL_ADD_ACKNOWLEDGEMENT_MS;
const IOS_CONFIRMATION_COMPLETION_MS = 2 * IOS_NATIVE_LOOKUP_ROUND_MS + IOS_CONFIRMATION_IDENTITY_MS + IOS_FINAL_ADD_ACKNOWLEDGEMENT_MS;
// iOS 27 shows the confirmation button labelled "Add" without guaranteeing that its accessibility name is also "Add".
export const IOS_CONFIRMATION_ADD_XPATH = "//XCUIElementTypeNavigationBar[(@name='Add to Home Screen' or @label='Add to Home Screen' or .//XCUIElementTypeStaticText[@name='Add to Home Screen' or @label='Add to Home Screen' or @value='Add to Home Screen']) and @visible='true' and not(ancestor::*[@visible='false'])]//XCUIElementTypeButton[@name='Add' or @label='Add']";
const IOS_NATIVE_SCROLL_COMMAND_MS = 5_000;
const IOS_NATIVE_GESTURE_COMMAND_MS = 8_000;
const IOS_NATIVE_HIERARCHY_COMMAND_MS = 8_000;
const IOS_NATIVE_SCROLL_LIMIT = 8;
const IOS_NATIVE_LIST_READINESS_MS = 15_000;
// The first Share sheet can start the system sharing service cold; a hosted runner took more than 15 s to publish its actions.
const IOS_SHARE_SHEET_READINESS_MS = 30_000;
// Hosted runners have spent more than 70 s reaching Add to Home Screen through the Page Menu, Share sheet and View More, which left less than the Add confirmation's reserved allowance.
const IOS_INSTALL_PHASE_MS = 240_000;
// Launch combines Home observation with up to 30 s for the provider and 60 s for its web view, so slow runners need room beyond those two waits.
const IOS_LAUNCH_PHASE_MS = 180_000;
// A hosted runner took 67 s to start the web view's content process after a cold launch, so the first page can publish late.
// Like other phase limits, this window is tripled on hosted iOS.
const IOS_LAUNCH_ATTACHMENT_MS = 60_000;
const IOS_NATIVE_ACTION_TIMEOUT_MS = IOS_NATIVE_GESTURE_COMMAND_MS * IOS_NATIVE_SCROLL_LIMIT + 20_000;

interface ConfirmationCaptureEvidence {
  status: 'not-requested-product-first';
  acknowledgement?: Readonly<WebDriverRequestTiming>;
}

type SafariDiscoveryResult = 'no-context' | 'native-only' | 'safari-context' | 'expected-origin-context' | 'unrelated-context' | 'inspection-truncated';
type SafariNavigationFailureKind = 'discovery' | 'observation' | 'reserve';
type SafariObservationResult = 'expected-origin' | 'origin-mismatch' | 'missing-url';

interface SafariDiscoveryEvidence {
  result: SafariDiscoveryResult;
  cause: string;
  contextCount: number;
  inspectedContextCount: number;
  safariContextCount: number;
  expectedOriginContextCount: number;
  contextInspectionTruncated: boolean;
  stringInspectionTruncated: boolean;
  observedAt: string;
  elapsedMs: number;
  remainingMs: number;
}

interface SafariObservationEvidence {
  result: SafariObservationResult;
  cause: string;
  observedAt: string;
  elapsedMs: number;
  remainingMs: number;
}

interface SafariReserveEvidence {
  reason: 'discovery-observation' | 'safari-observation';
  requiredMs: number;
  observedAt: string;
  elapsedMs: number;
  remainingMs: number;
}

interface SafariNavigationFailureEvidence {
  kind: SafariNavigationFailureKind;
  cause: string;
  observedAt: string;
  elapsedMs: number;
  remainingMs: number;
}

export function isIOSSafariBrowserBundle(bundleId?: string): boolean {
  return bundleId?.toLowerCase() === 'com.apple.mobilesafari';
}

export function isIOSSafariViewServiceBundle(bundleId?: string): boolean {
  return bundleId?.toLowerCase() === 'com.apple.safariviewservice';
}

function isIOSAllowedLaunchForeground(bundleId: string): boolean {
  return bundleId === IOS_SPRINGBOARD_BUNDLE_ID
    || bundleId === IOS_INSTALLED_BUNDLE_ID
    || isIOSSafariBrowserBundle(bundleId)
    || isIOSSafariViewServiceBundle(bundleId);
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

const IOS_HOME_ICON_LABELS = ['Herdr Mobile Relay', 'Herdr Relay'] as const;
const IOS_HOME_PAGE_INDICATOR_MISSING = 'Home page indicator is missing';
type PageKeyboardMeasure = {
  focused: boolean;
  portrait: boolean;
  zoom: number;
  innerHeight: number;
  screenHeight: number;
  viewportHeight: number;
  cover: number;
};

const IOS_NATIVE_COMPOSER_LOCATOR: Locator = {
  using: '-ios predicate string',
  value: "type IN {'XCUIElementTypeTextField', 'XCUIElementTypeTextView'} AND label == 'Prompt' AND visible == 1",
};

interface IOSHomePage {
  current: number;
  total: number;
  raw: string;
}

interface IOSHomeObservation {
  state: 'pending' | 'missing' | 'not-ready' | 'ready';
  container?: string;
  icon?: string;
  page?: IOSHomePage;
  attributes?: Record<string, string | null>;
  reason: string;
  classificationCounts?: { container: number; page: number; icon: number };
}

class IOSHomeObservationShortfall extends WebDriverError {
  constructor(readonly phase: PhaseBudget) {
    super({
      code: 'IOS_HOME_OBSERVATION_SHORTFALL', message: 'Home observation allowance is insufficient before dispatch',
      path: '', method: 'LOCAL', durationMs: 0, selectedContext: 'NATIVE_APP', selectedWindow: '',
    });
  }
}

// An XPath with visibility tests made WebDriverAgent serialize all of SpringBoard (over 16 s on hosted runners).
function iosHomeObservationLocator(): Locator {
  const labels = `{${IOS_HOME_ICON_LABELS.map((label) => `'${label}'`).join(', ')}}`;
  return {
    using: '-ios predicate string',
    value: [
      "(type == 'XCUIElementTypeOther' AND name == 'Home screen icons' AND visible == 1)",
      `(type == 'XCUIElementTypeIcon' AND (name IN ${labels} OR label IN ${labels}) AND visible == 1)`,
      "(type == 'XCUIElementTypePageIndicator' AND (name == 'Page control' OR label == 'Page control') AND visible == 1)",
    ].join(' OR '),
  };
}

function iosHomeElementKind(attributes: Record<string, string | null>): 'container' | 'icon' | 'page' | undefined {
  if (attributes.type === 'XCUIElementTypeOther' && attributes.name === 'Home screen icons') return 'container';
  if (attributes.type === 'XCUIElementTypeIcon'
    && IOS_HOME_ICON_LABELS.some((label) => attributes.name === label || attributes.label === label)) return 'icon';
  if (attributes.type === 'XCUIElementTypePageIndicator'
    && (attributes.name === 'Page control' || attributes.label === 'Page control')) return 'page';
  return undefined;
}

function parseIOSHomePage(value: string): IOSHomePage | undefined {
  const match = value.trim().match(/^Page\s+(\d+)\s+of\s+(\d+)$/u);
  if (!match) return undefined;
  const current = Number(match[1]);
  const total = Number(match[2]);
  return Number.isSafeInteger(current) && Number.isSafeInteger(total) && current > 0 && total >= current
    ? { current, total, raw: value }
    : undefined;
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
  private pageKeyboardBaseline: PageKeyboardMeasure | undefined;
  private keyboardShownByPage = false;
  private lastCompletion?: UpdateCompletionEvidence;
  private lastNativeActivity = '';
  private lastNativePid = '';
  private installedContextContractReady = false;
  private simulatorReadyAt = '';
  private ownershipFailure?: QualificationFatalError;
  private nativeObservationFailure?: unknown;
  private nativeObservationTarget = 'auto';
  private springBoardRoot = '';
  private safariLastDiscovery?: SafariDiscoveryEvidence;
  private safariLastObservation?: SafariObservationEvidence;
  private safariReserveExhaustion?: SafariReserveEvidence;
  private safariFirstFailure?: SafariNavigationFailureEvidence;
  private attachmentFirstRefusal?: Readonly<WebDriverRequestTiming>;
  private navigationCommand = command;
  private launchObservation = new IOSLaunchObservation();
  private launchCollector = collectIOSLaunchReceipt;
  private launchWrite = writeFile;
  private launchMkdir = mkdir;
  private diagnosticCommand = commandOutput;
  private failureCapture?: Promise<void>;
  private launchReceipt?: IOSLaunchReceipt;
  private nativeDiagnosticsBlocked = false;
  private confirmationCapture?: ConfirmationCaptureEvidence;
  private installationAddRequested = false;
  private installationAddFailure?: unknown;
  private omitLaunchLogs = false;
  private readonly launchPersistence = {
    directory: iosReceiptWriteStatus('directory'), events: iosReceiptWriteStatus('events'), receipt: iosReceiptWriteStatus('receipt'),
  };

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
    const { managedWdaCapabilities } = await import('../support/ios-xctest');
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
        ...await managedWdaCapabilities(this.udid),
        'appium:showXcodeLog': true,
        'appium:wdaLaunchTimeout': 300_000,
        'appium:wdaStartupRetries': 1,
        'appium:wdaStartupRetryInterval': 10_000,
      },
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

  private safariTiming(phase: PhaseBudget, timeoutMs: number): Pick<SafariDiscoveryEvidence, 'observedAt' | 'elapsedMs' | 'remainingMs'> {
    const remainingMs = Math.max(0, Math.floor(phase.remainingMs));
    return {
      observedAt: new Date().toISOString(),
      elapsedMs: Math.max(0, Math.floor(timeoutMs - remainingMs)),
      remainingMs,
    };
  }

  private safariDiscoveryEvidence(metadata: ContextMetadata[], phase: PhaseBudget, timeoutMs: number): SafariDiscoveryEvidence {
    const inspectedContextCount = Math.min(metadata.length, IOS_SAFARI_DIAGNOSTIC_CONTEXT_LIMIT);
    const contextInspectionTruncated = metadata.length > inspectedContextCount;
    let safariContextCount = 0;
    let expectedOriginContextCount = 0;
    let nativeOnly = metadata.length > 0;
    let stringInspectionTruncated = false;
    for (let index = 0; index < inspectedContextCount; index += 1) {
      const candidate = metadata[index];
      if (candidate.id !== 'NATIVE_APP') nativeOnly = false;
      if (candidate.bundleId !== undefined) {
        if (candidate.bundleId.length > IOS_SAFARI_DIAGNOSTIC_STRING_LIMIT) stringInspectionTruncated = true;
        else if (isIOSSafariBrowserBundle(candidate.bundleId)) safariContextCount += 1;
      }
      if (candidate.url !== undefined) {
        if (candidate.url.length > IOS_SAFARI_DIAGNOSTIC_STRING_LIMIT) stringInspectionTruncated = true;
        else if (this.isExpectedOrigin(candidate.url)) expectedOriginContextCount += 1;
      }
    }
    let result: SafariDiscoveryResult;
    let cause: string;
    if (contextInspectionTruncated || stringInspectionTruncated) {
      result = 'inspection-truncated';
      cause = contextInspectionTruncated
        ? 'WebKit context inspection was truncated'
        : 'WebKit context string inspection was truncated';
    } else if (metadata.length === 0) {
      result = 'no-context';
      cause = 'WebKit returned no contexts';
    } else if (nativeOnly) {
      result = 'native-only';
      cause = 'Safari did not publish a web context';
    } else if (safariContextCount > 0) {
      result = 'safari-context';
      cause = 'Safari web context was published';
    } else if (expectedOriginContextCount > 0) {
      result = 'expected-origin-context';
      cause = 'fixture origin context was published';
    } else {
      result = 'unrelated-context';
      cause = 'no Safari or fixture origin context was published';
    }
    return {
      result,
      cause,
      contextCount: inspectedContextCount,
      inspectedContextCount,
      safariContextCount,
      expectedOriginContextCount,
      contextInspectionTruncated,
      stringInspectionTruncated,
      ...this.safariTiming(phase, timeoutMs),
    };
  }

  private safariObservationEvidence(
    result: SafariObservationResult,
    cause: string,
    phase: PhaseBudget,
    timeoutMs: number,
  ): SafariObservationEvidence {
    return { result, cause, ...this.safariTiming(phase, timeoutMs) };
  }

  private safariFailureEvidence(
    phase: PhaseBudget,
    timeoutMs: number,
    kind: SafariNavigationFailureKind,
    cause: string,
  ): SafariNavigationFailureEvidence {
    return { kind, cause, ...this.safariTiming(phase, timeoutMs) };
  }

  private retainSafariReserve(
    phase: PhaseBudget,
    timeoutMs: number,
    reason: SafariReserveEvidence['reason'],
    requiredMs: number,
  ): void {
    if (this.safariReserveExhaustion) return;
    this.safariReserveExhaustion = { reason, requiredMs, ...this.safariTiming(phase, timeoutMs) };
  }

  private retainSafariFirstFailure(receipt: SafariNavigationFailureEvidence): void {
    if (this.safariFirstFailure) return;
    this.safariFirstFailure = receipt;
    this.diagnostics.record({
      phase: 'ios-navigation',
      operation: 'safari-navigation-first-failure',
      detail: {
        failure: receipt,
        lastDiscovery: this.safariLastDiscovery,
        lastObservation: this.safariLastObservation,
        reserveExhaustion: this.safariReserveExhaustion,
      },
    });
  }

  private async waitForSafariFixturePage(url: string, timeoutMs: number, parent: PhaseBudget): Promise<void> {
    const phase = parent.phaseView('ios-safari-readiness', timeoutMs);
    let fallbackAttempted = false;
    let lastError = '';
    let firstFailure: SafariNavigationFailureEvidence | undefined;
    while (!phase.exhausted) {
      try {
        if (phase.remainingMs < IOS_SAFARI_DISCOVERY_COMMAND_MS + IOS_SAFARI_DISCOVERY_RESERVE_MS) {
          this.retainSafariReserve(phase, timeoutMs, 'discovery-observation', IOS_SAFARI_DISCOVERY_COMMAND_MS + IOS_SAFARI_DISCOVERY_RESERVE_MS);
          lastError = `not enough time for WebKit discovery and Safari observation (${phase.remainingMs}ms remains)`;
          firstFailure ||= this.safariFailureEvidence(phase, timeoutMs, 'reserve', 'WebKit discovery and Safari observation reserve was unavailable');
          break;
        }
        const metadata = await this.driver.contextMetadata(IOS_SAFARI_DISCOVERY_COMMAND_MS);
        this.safariLastDiscovery = this.safariDiscoveryEvidence(metadata, phase, timeoutMs);
        const context = metadata.find((candidate) => isIOSSafariBrowserBundle(candidate.bundleId)
          || (candidate.url !== undefined && this.isExpectedOrigin(candidate.url)));
        if (!context) {
          lastError = 'Safari did not publish a web context';
          firstFailure ||= this.safariFailureEvidence(phase, timeoutMs, 'discovery', this.safariLastDiscovery.cause);
        } else {
          if (phase.remainingMs < IOS_SAFARI_OBSERVATION_MS) {
            this.retainSafariReserve(phase, timeoutMs, 'safari-observation', IOS_SAFARI_OBSERVATION_MS);
            lastError = `not enough time for Safari observation (${phase.remainingMs}ms remains)`;
            firstFailure ||= this.safariFailureEvidence(phase, timeoutMs, 'reserve', 'Safari observation reserve was unavailable');
            break;
          }
          await this.driver.switchContext(context.id, IOS_SAFARI_ATTACH_COMMAND_MS);
          if (phase.remainingMs < 2_000) break;
          const currentUrl = await this.driver.currentUrl(1_000);
          if (this.isExpectedOrigin(currentUrl)) {
            this.safariLastObservation = this.safariObservationEvidence('expected-origin', 'Safari page reported the fixture origin', phase, timeoutMs);
            this.lastUrl = currentUrl;
            if (phase.remainingMs < 1_000) break;
            await this.driver.switchContext('NATIVE_APP', 1_000);
            return;
          }
          const observation = this.safariObservationEvidence(
            currentUrl ? 'origin-mismatch' : 'missing-url',
            currentUrl ? 'Safari page did not report the fixture origin' : 'Safari page did not report a URL',
            phase,
            timeoutMs,
          );
          this.safariLastObservation = observation;
          firstFailure ||= this.safariFailureEvidence(phase, timeoutMs, 'observation', observation.cause);
          if (!fallbackAttempted) {
            fallbackAttempted = true;
            const navigateTimeout = phase.remainingMs;
            if (navigateTimeout < minimumDriverRequestMs) break;
            await this.driver.navigate(url, navigateTimeout);
            continue;
          }
          lastError = currentUrl ? 'Safari page is not at the fixture origin' : 'Safari page is not navigated to the fixture origin';
        }
      } catch (error) {
        if (isFatalDriverError(error)) throw error;
        lastError = isCommandAdmissionError(error)
          ? 'Safari discovery command was not admitted'
          : error instanceof WebDriverError ? `${error.code}: Safari discovery failed` : 'Safari discovery failed';
        firstFailure ||= this.safariFailureEvidence(phase, timeoutMs, 'discovery', lastError.split(':', 1)[0]);
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
    const failure = firstFailure || this.safariFailureEvidence(phase, timeoutMs, 'reserve', 'Safari fixture page was not observed');
    this.retainSafariFirstFailure(failure);
    const details = [
      lastError || 'no page observed',
      this.safariLastDiscovery ? `last discovery ${this.safariLastDiscovery.result}: ${this.safariLastDiscovery.cause}` : '',
      this.safariLastObservation ? `last observation ${this.safariLastObservation.result}: ${this.safariLastObservation.cause}` : '',
      this.safariReserveExhaustion ? `reserve ${this.safariReserveExhaustion.reason} (${this.safariReserveExhaustion.remainingMs}ms remains)` : '',
    ].filter(Boolean).join('; ');
    throw new Error(`IOS_NAVIGATION: Safari fixture page was not ready (${details})`);
  }

  async openSetupURLInInstalledApp(url: string): Promise<void> {
    await this.attachToInstalledView();
    await this.driver.navigate(url);
    await delay(1_000);
  }

  async installFromBrowser(): Promise<void> {
    this.assertOwnershipClear();
    if (this.installationAddRequested) throw new Error('IOS_SHARE: final Add was already requested');
    const phase = this.budget.phaseView('ios-install', IOS_INSTALL_PHASE_MS);
    phase.assertAvailable('start iOS installation');
    await this.driver.switchContext('NATIVE_APP', Math.max(minimumDriverRequestMs, phase.remainingMs));
    await this.setNativeObservationTarget('auto', phase);
    await this.observeNativeForeground('com.apple.mobilesafari', phase);
    let source = await this.captureNativeReadiness('ios-before-share', phase.remainingMs);
    const findShare = () => {
      const findTimeout = Math.min(5_000, phase.remainingMs);
      if (findTimeout < minimumDriverRequestMs) throw new Error('IOS_SHARE: insufficient time to inspect Safari toolbar');
      return this.driver.findAnyOnce([
        accessibility('ShareButton'),
        accessibility('Share'),
        accessibility('Share button'),
        textLocator('Share'),
      ], findTimeout).catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
        return '';
      });
    };
    let share = await findShare();
    if (!share && !nativeShareBounds(source)) {
      const pageMenu = await this.driver.findAnyOnce([{
        using: 'xpath',
        value: "//XCUIElementTypeApplication[@name='Safari']//XCUIElementTypeButton[@name='MoreMenuButton' and @enabled='true' and @visible='true' and not(ancestor::XCUIElementTypeWebView)]",
      }], Math.min(5_000, phase.remainingMs)).catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
        return '';
      });
      if (pageMenu) {
        await this.assertNativeControl(pageMenu, 'Safari Page Menu', phase);
        if (phase.remainingMs < IOS_NATIVE_SCROLL_COMMAND_MS + IOS_NATIVE_HIERARCHY_COMMAND_MS) {
          throw new Error('IOS_SHARE: insufficient time to open and observe Safari Page Menu');
        }
        await this.driver.click(pageMenu, IOS_NATIVE_SCROLL_COMMAND_MS);
        await this.observeNativeForeground('com.apple.mobilesafari', phase);
        source = await this.captureNativeReadiness('ios-after-page-menu', phase.remainingMs);
        share = await findShare();
      }
    }
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
    const addToHomeScreen = await this.findNativeScrollable([
      iosActionLabelContains('Add to Home Screen'),
    ], 'Add to Home Screen', Math.min(IOS_NATIVE_ACTION_TIMEOUT_MS, phase.remainingMs));
    const transaction = phase.phaseView('ios-confirmation-transaction', phase.remainingMs, 1_500);
    const policy = this.driver.nativeRequestPolicy(transaction);
    try {
      await withIOSConfirmationSettings(this.driver, transaction, IOS_ADD_TO_HOME_SCREEN_ACKNOWLEDGEMENT_MS + IOS_CONFIRMATION_ROUND_MS, async (operation) => {
        const operationPolicy = Object.freeze({ ...policy, budget: operation });
        if (operation.remainingMs < IOS_ADD_TO_HOME_SCREEN_ACKNOWLEDGEMENT_MS) throw new Error('IOS_SHARE: Add: insufficient time to click Add to Home Screen');
        this.diagnostics.record({ phase: 'ios-install', operation: 'activity-click-started', timeoutMs: IOS_ADD_TO_HOME_SCREEN_ACKNOWLEDGEMENT_MS });
        try {
          await this.driver.click(addToHomeScreen, IOS_ADD_TO_HOME_SCREEN_ACKNOWLEDGEMENT_MS, operationPolicy);
        } catch (error) {
          this.diagnostics.record({ phase: 'ios-install', operation: 'activity-click-failed', detail: { fatal: isFatalDriverError(error) } });
          throw error;
        }
        this.diagnostics.record({ phase: 'ios-install', operation: 'activity-click-acknowledged' });
        const acknowledgement = this.driver.snapshot().lastCommand?.timing;
        this.confirmationCapture = { status: 'not-requested-product-first', acknowledgement: acknowledgement && { ...acknowledgement } };
        const confirmation = operation.phaseView('ios-install-confirmation', 75_000);
        const confirmationPolicy = Object.freeze({ ...operationPolicy, budget: confirmation });
        const restoreTiming = this.driver.setRequestTimingScope('ios-install-confirmation', confirmation);
        try {
          const addButton = await this.waitForInstallConfirmation(confirmation, confirmationPolicy);
          if (confirmation.remainingMs < IOS_FINAL_ADD_ACKNOWLEDGEMENT_MS || operation.remainingMs < IOS_FINAL_ADD_ACKNOWLEDGEMENT_MS) {
            throw new Error('IOS_SHARE: Add: insufficient time to complete confirmation click');
          }
          if (this.installationAddRequested) throw new Error('IOS_SHARE: final Add was already requested');
          this.installationAddRequested = true;
          try {
            await this.driver.click(addButton, IOS_FINAL_ADD_ACKNOWLEDGEMENT_MS, Object.freeze({ ...confirmationPolicy, finalAction: true }));
          } catch (error) {
            this.installationAddFailure ??= error;
            this.nativeDiagnosticsBlocked = true;
            throw error;
          }
        } finally {
          restoreTiming();
        }
      }, policy);
    } catch (error) {
      if (this.driver.snapshot().unusable || this.driver.snapshot().firstFatal) this.nativeDiagnosticsBlocked = true;
      throw error;
    }
    await delay(Math.min(1_500, phase.remainingMs), phase);
  }

  private async waitForInstallConfirmation(phase: PhaseBudget, policy: WebDriverRequestPolicy): Promise<string> {
    const snapshot = this.driver.snapshot();
    if (snapshot.unusable || snapshot.firstFatal) {
      this.nativeDiagnosticsBlocked = true;
      this.installationAddFailure ??= new WebDriverError({
        code: 'APPIUM_SESSION_UNUSABLE', message: 'confirmation requires the original healthy session',
        path: '/execute/sync', method: 'POST', durationMs: 0, selectedContext: 'NATIVE_APP', selectedWindow: '',
      });
      throw this.installationAddFailure;
    }
    let lastState = 'missing';
    while (phase.remainingMs >= IOS_NATIVE_LOOKUP_ROUND_MS) {
      try {
        if (this.driver.snapshot().selectedContext !== 'NATIVE_APP') throw new Error('IOS_SHARE: Add: confirmation is not in the native context');
        const appInfo = await this.driver.activeAppInfo(IOS_NATIVE_LOOKUP_ROUND_MS, policy);
        const bundleId = String(appInfo?.bundleId || appInfo?.bundleID || '');
        if (!isIOSSafariBrowserBundle(bundleId) && !isIOSSafariViewServiceBundle(bundleId)) {
          throw new Error(`IOS_SHARE: Add: Safari confirmation is not foreground (${bundleId || 'unknown'})`);
        }
        if (phase.remainingMs < IOS_CONFIRMATION_LOOKUP_MS) break;
        const response = await this.driver.command<unknown>('/element', 'POST', { using: 'xpath', value: IOS_CONFIRMATION_ADD_XPATH }, IOS_CONFIRMATION_LOOKUP_MS, policy);
        const element = this.installConfirmationElementId(response);
        if (phase.remainingMs < IOS_CONFIRMATION_IDENTITY_MS) break;
        const identity = await this.installConfirmationIdentity(policy);
        if (identity !== element) {
          lastState = 'confirmation identity is missing or replaced';
        } else {
          if (phase.remainingMs < IOS_NATIVE_LOOKUP_ROUND_MS) break;
          const enabled = await this.readNativeControlAttribute(element, 'enabled', IOS_NATIVE_LOOKUP_ROUND_MS, true, policy);
          if (enabled === 'false') {
            lastState = 'disabled';
          } else if (enabled !== 'true') {
            lastState = 'indeterminate';
          } else if (phase.remainingMs < IOS_CONFIRMATION_COMPLETION_MS) {
            break;
          } else {
            const visible = await this.readNativeControlAttribute(element, 'visible', IOS_NATIVE_LOOKUP_ROUND_MS, true, policy);
            if (visible !== 'true') {
              lastState = visible === 'false' ? 'hidden' : 'indeterminate';
            } else {
              if (phase.remainingMs < IOS_NATIVE_LOOKUP_ROUND_MS + IOS_CONFIRMATION_IDENTITY_MS + IOS_FINAL_ADD_ACKNOWLEDGEMENT_MS) break;
              const hittable = await this.readNativeControlAttribute(element, 'hittable', IOS_NATIVE_LOOKUP_ROUND_MS, true, policy);
              if (hittable !== 'true') {
                lastState = hittable === 'false' ? 'not-hittable' : 'indeterminate';
              } else {
                if (phase.remainingMs < IOS_CONFIRMATION_IDENTITY_MS + IOS_FINAL_ADD_ACKNOWLEDGEMENT_MS) break;
                const currentElement = await this.installConfirmationIdentity(policy);
                if (!currentElement) throw new Error('IOS_SHARE: Add: confirmation identity was replaced before click');
                if (currentElement === element) {
                  if (phase.remainingMs < IOS_FINAL_ADD_ACKNOWLEDGEMENT_MS) throw new Error('IOS_SHARE: Add: insufficient time to complete confirmation click');
                  return element;
                }
                lastState = 'confirmation Add control was replaced before click';
              }
            }
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

  private async installConfirmationIdentity(policy: WebDriverRequestPolicy): Promise<string | undefined> {
    const response = await this.driver.command<unknown>('/elements', 'POST', {
      using: 'xpath',
      value: IOS_CONFIRMATION_ADD_XPATH,
    }, IOS_CONFIRMATION_IDENTITY_MS, policy);
    if (!Array.isArray(response)) throw new Error('IOS_SHARE: Add: malformed confirmation identity response');
    const matches = response.map((candidate: unknown) => this.installConfirmationElementId(candidate));
    return matches.length === 1 ? matches[0] : undefined;
  }

  async launchInstalledApp(): Promise<void> {
    if (this.installationAddFailure !== undefined) throw this.installationAddFailure;
    this.launchObservation.begin();
    try {
      await this.launchInstalledAppObserved();
    } catch (error) {
      this.launchObservation.freeze(error, this.driver.snapshot());
      throw error;
    }
  }

  private async launchInstalledAppObserved(): Promise<void> {
    this.assertOwnershipClear();
    await requireOwnedDevice('ios', this.udid);
    this.selectedInstalledContext = '';
    const phase = this.budget.phaseView('ios-launch', IOS_LAUNCH_PHASE_MS);
    phase.assertAvailable('launch installed provider');
    await this.driver.switchContext('NATIVE_APP', Math.max(2, phase.remainingMs));
    const foreground = await this.observeCurrentNativeForeground(phase);
    await this.setNativeObservationTarget(IOS_SPRINGBOARD_BUNDLE_ID, phase);
    if (foreground !== IOS_SPRINGBOARD_BUNDLE_ID) {
      await this.driver.mobile('pressButton', { name: 'home' }, Math.max(2, phase.remainingMs));
    }
    await this.ensureSpringBoardForeground(phase);
    await delay(Math.min(750, phase.remainingMs), phase);
    let observation = await this.observeHomeIcon(phase);
    observation = await this.waitForHomeIconReadiness(observation, phase, true);
    observation = await this.leaveTodayView(observation, phase);
    if (observation.state === 'pending' || observation.state === 'not-ready') {
      throw new Error(`IOS_CONTEXT: current SpringBoard page was not positively observed (${observation.reason})`);
    }
    const currentPageReady = observation.state === 'ready';
    if (observation.state === 'missing') {
      for (let page = 0; page < 8; page += 1) {
        phase.assertAvailable('show first SpringBoard page');
        await this.driver.mobile('swipe', { direction: 'right' }, Math.max(2, phase.remainingMs));
      }
    }
    for (let page = 0; page < 8; page += 1) {
      phase.assertAvailable('find installed provider icon');
      if (!currentPageReady || page > 0) {
        await this.ensureSpringBoardForeground(phase);
        observation = await this.observeHomeIcon(phase);
        observation = await this.waitForHomeIconReadiness(observation, phase);
        observation = await this.leaveTodayView(observation, phase);
      }
      if (observation.state === 'pending' || observation.state === 'not-ready') {
        throw new Error(`IOS_CONTEXT: current SpringBoard page was not positively observed (${observation.reason})`);
      }
      if (observation.state === 'ready' && observation.icon) {
        await this.setNativeObservationTarget(IOS_INSTALLED_BUNDLE_ID, phase);
        const clickTimeout = phase.remainingMs;
        if (clickTimeout <= 1) break;
        this.launchObservation.arm(this.driver.snapshot(), observation.page?.current ?? 0, clickTimeout);
        await this.driver.click(observation.icon, clickTimeout);
        this.launchObservation.acknowledge(this.driver.snapshot(), observation.icon);
        this.launchObservation.enter('provider');
        const providerTimeout = Math.min(30_000, phase.remainingMs);
        if (providerTimeout <= 1) break;
        if (await this.waitForInstalledProvider(providerTimeout, phase)) {
          const foregroundTimeout = phase.remainingMs;
          if (foregroundTimeout <= 1) break;
          this.launchObservation.enter('foreground');
          phase.assertAvailable('verify installed provider foreground');
          await this.requireInstalledProviderForeground(foregroundTimeout, phase);
          phase.assertAvailable('attach installed provider');
          this.launchObservation.enter('attachment');
          await delay(Math.min(750, phase.remainingMs), phase);
          phase.assertAvailable('attach installed provider');
          await this.attachToInstalledView(Math.min(IOS_LAUNCH_ATTACHMENT_MS, Math.floor(phase.remainingMs / timingScale)));
          this.launchObservation.enter('complete');
          return;
        }
        this.launchObservation.enter('other');
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
    // Observe this original attachment deadline; never install it as a new
    // driver budget or change either native ownership bracket's allowance.
    const restoreTimingScope = this.driver.setRequestTimingScope('ios-attachment-discovery', phase);
    try {
      let lastError = '';
      while (!phase.exhausted) {
        phase.assertAvailable('discover installed page');
        if (this.selectedInstalledContext) {
          try {
            phase.assertAvailable('validate cached installed page');
            this.driver.setRequestTimingScope('ios-attachment-pre-attachment-native', phase);
            await this.driver.switchContext('NATIVE_APP', Math.max(1, phase.remainingMs));
            await this.requireInstalledProviderForeground(Math.max(1, phase.remainingMs));
            this.driver.setRequestTimingScope('ios-attachment-validation', phase);
            await this.driver.switchContext(this.selectedInstalledContext, Math.max(1, phase.remainingMs));
            const url = await this.driver.currentUrl(Math.max(1, phase.remainingMs));
            this.lastUrl = url;
            if (!this.isExpectedOrigin(url)) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `cached document origin ${url} is not ${this.origin}`);
            await this.validateInstalledDocument(Math.max(1, phase.remainingMs));
            return;
          } catch (error) {
            if (isCommandAdmissionError(error)) this.attachmentFirstRefusal ??= this.driver.snapshot().lastCommand?.timing;
            const detail = error instanceof Error ? error.message : String(error);
            this.diagnostics.record({ phase: 'ios-attachment', operation: 'cached-context', detail });
            if (!isIOSStaleContextError(error)) throw error;
            this.selectedInstalledContext = '';
            lastError = `cached context: ${detail}`;
          }
        }
        phase.assertAvailable('discover installed page metadata');
        if (phase.remainingMs < (IOS_WEBKIT_DISCOVERY_COMMAND_MS + IOS_INSTALLED_FOREGROUND_MS) * timingScale) {
          lastError ||= `not enough time for WebKit discovery and foreground observation (${phase.remainingMs}ms remains)`;
          break;
        }
        if (!this.installedContextContractReady) {
          this.driver.setRequestTimingScope('ios-attachment-contract', phase);
          await this.driver.installedContextContract(IOS_INSTALLED_FOREGROUND_MS);
          this.installedContextContractReady = true;
        }
        this.driver.setRequestTimingScope('ios-attachment-pre-discovery-native', phase);
        const foreground = await this.observeCurrentNativeForeground(phase, IOS_INSTALLED_FOREGROUND_MS);
        if (foreground !== IOS_INSTALLED_BUNDLE_ID) {
          this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'installed provider is not foreground before discovery');
        }
        // These observations consume the original phase, never a new discovery clock.
        if (phase.remainingMs < (IOS_WEBKIT_DISCOVERY_COMMAND_MS + IOS_INSTALLED_FOREGROUND_MS) * timingScale) break;
        const targetApp = Object.freeze({ bundleId: IOS_INSTALLED_BUNDLE_ID, nativePid: Number(this.lastNativePid) });
        this.driver.setRequestTimingScope('ios-attachment-discovery', phase);
        const contexts = await this.driver.installedContextMetadata(targetApp, IOS_WEBKIT_DISCOVERY_COMMAND_MS);
        phase.assertAvailable('validate installed discovery foreground');
        if (phase.remainingMs < IOS_INSTALLED_FOREGROUND_MS) break;
        this.driver.setRequestTimingScope('ios-attachment-discovery-native', phase);
        await this.requireInstalledProviderForeground(IOS_INSTALLED_FOREGROUND_MS);
        if (Number(this.lastNativePid) !== targetApp.nativePid) this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'native provider changed during scoped discovery');
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
              this.driver.setRequestTimingScope('ios-attachment-pre-attachment-native', phase);
              await this.driver.switchContext('NATIVE_APP', Math.max(1, phase.remainingMs));
              await this.requireInstalledProviderForeground(Math.max(1, phase.remainingMs));
              if (Number(this.lastNativePid) !== targetApp.nativePid) this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'native provider changed before scoped attachment');
              this.driver.setRequestTimingScope('ios-attachment-validation', phase);
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
              if (isCommandAdmissionError(error)) this.attachmentFirstRefusal ??= this.driver.snapshot().lastCommand?.timing;
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
    } catch (error) {
      if (isCommandAdmissionError(error)) this.attachmentFirstRefusal ??= this.driver.snapshot().lastCommand?.timing;
      throw error;
    } finally {
      restoreTimingScope();
    }
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
    const selector = `button.agent-open[aria-label="Open mobile-ci on ${relayName}"]`;
    let lastError = '';
    while (Date.now() < deadline) {
      try {
        await this.attachToInstalledView();
        // Several idle workspaces start as collapsed cards whose agent buttons cannot be clicked until opened.
        const summarySelector = `details.workspace-card:not([open]):has(${selector}) > summary`;
        const collapsedCard = (await this.driver.findAll(css(summarySelector), 2_000))[0];
        if (collapsedCard) {
          try {
            await this.driver.click(collapsedCard);
          } catch (error) {
            if (isFatalDriverError(error)) throw error;
            // After a standalone relaunch the web view can refuse native clicks just like the agent button below.
            await this.driver.execute('document.querySelector(arguments[0])?.click();', [summarySelector]);
          }
          lastError = `agent ${relayName} was inside a collapsed workspace card`;
          await delay(250);
          continue;
        }
        const agent = await this.driver.find(css(selector), 2_000);
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
    this.installedBindingState = 'unselected';
    await this.driver.mobile('pressButton', { name: 'home' });
    await this.ensureSpringBoardForeground();
  }

  async showKeyboardOnComposer(): Promise<void> {
    await this.attachToInstalledView();
    const existing = await this.driver.find(css('textarea[aria-label="Prompt"]'), 5_000).catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
      return '';
    });
    if (!existing) {
      const open = await this.driver.find(css('button[aria-label^="Open "]'), 30_000);
      await this.driver.click(open);
      await this.driver.find(css('textarea[aria-label="Prompt"]'), 30_000);
    }
    this.keyboardShownByPage = false;
    this.pageKeyboardBaseline = await this.measurePageKeyboard();
    await this.tapComposerNatively();
    await this.attachToInstalledView();
    const composer = await this.driver.find(css('textarea[aria-label="Prompt"]'), 5_000);
    this.keyboardDraft = 'mobile-device-ci draft';
    await this.driver.sendKeys(composer, this.keyboardDraft);
    await this.waitForKeyboard(true, () => this.pageShowsKeyboard());
  }

  private async measurePageKeyboard(timeoutMs = IOS_PAGE_SCRIPT_MS): Promise<PageKeyboardMeasure | undefined> {
    try {
      const measured = await this.driver.execute<unknown>(IOS_PAGE_KEYBOARD_SCRIPT, [], timeoutMs);
      if (!measured || typeof measured !== 'object') return undefined;
      const { focused, portrait, zoom, innerHeight, screenHeight, viewportHeight, cover } = measured as Record<string, unknown>;
      const numbers = [zoom, innerHeight, screenHeight, viewportHeight, cover];
      if (typeof focused !== 'boolean' || typeof portrait !== 'boolean' || !numbers.every((value) => typeof value === 'number' && Number.isFinite(value))) return undefined;
      return { focused, portrait, zoom, innerHeight, screenHeight, viewportHeight, cover } as PageKeyboardMeasure;
    } catch (error) {
      if (isFatalDriverError(error)) throw error;
      return undefined;
    }
  }

  private describePageKeyboard(measure: PageKeyboardMeasure | undefined): string {
    if (!measure) return 'unmeasured';
    const { focused, portrait, zoom, innerHeight, screenHeight, viewportHeight, cover } = measure;
    return `focused=${focused} portrait=${portrait} zoom=${zoom} inner=${innerHeight} screen=${screenHeight} visual=${viewportHeight} cover=${cover} baseline=${this.pageKeyboardBaseline?.cover ?? 'unmeasured'}`;
  }

  private pageKeyboardCover(measure: PageKeyboardMeasure | undefined): number | undefined {
    if (!measure?.portrait || Math.abs(measure.zoom - 1) > 0.05) return undefined;
    return measure.cover;
  }

  // On hosted iOS the keyboard shown for an installed web app is sometimes missing from the accessibility tree.
  private async pageShowsKeyboard(): Promise<string | true> {
    await this.attachToInstalledView();
    const measure = await this.measurePageKeyboard();
    const cover = this.pageKeyboardCover(measure);
    const shown = measure?.focused === true && cover !== undefined && cover >= IOS_PAGE_KEYBOARD_MIN_COVER_PX;
    this.diagnostics.record({
      phase: 'ios-keyboard',
      operation: shown ? 'keyboard-shown-by-page-viewport' : 'keyboard-not-shown-by-page-viewport',
      detail: { baseline: this.pageKeyboardBaseline ?? null, measure: measure ?? null },
    });
    if (!shown) return this.describePageKeyboard(measure);
    this.keyboardShownByPage = true;
    return true;
  }

  private async waitForPageKeyboardHidden(): Promise<void> {
    const phase = this.budget.phaseView('ios-page-keyboard', IOS_KEYBOARD_PHASE_MS);
    let measure: PageKeyboardMeasure | undefined;
    while (phase.remainingMs >= driverRequestAllowanceMs.lookup) {
      measure = await this.measurePageKeyboard(Math.min(IOS_PAGE_SCRIPT_MS, phase.remainingMs));
      const cover = this.pageKeyboardCover(measure);
      if (cover !== undefined && cover < IOS_PAGE_KEYBOARD_MIN_COVER_PX) return;
      if (phase.remainingMs < driverRequestAllowanceMs.lookup + 250) break;
      await delay(250, phase);
    }
    this.diagnostics.record({ phase: 'ios-keyboard', operation: 'keyboard-not-hidden-by-page-viewport', detail: { baseline: this.pageKeyboardBaseline ?? null, measure: measure ?? null } });
    throw new Error(`IOS_KEYBOARD: page viewport stayed covered after dismissal; page ${this.describePageKeyboard(measure)}`);
  }

  // A scripted web click does not count as a user gesture, so iOS may focus the composer without showing the keyboard.
  private async tapComposerNatively(): Promise<void> {
    const phase = this.budget.phaseView('ios-composer-tap', IOS_NATIVE_LIST_READINESS_MS);
    const roundMs = driverRequestAllowanceMs.lookup + driverRequestAllowanceMs.click;
    while (phase.remainingMs >= roundMs) {
      await this.requireInstalledProviderForeground(IOS_INSTALLED_FOREGROUND_MS);
      if (phase.remainingMs < driverRequestAllowanceMs.lookup) break;
      const matches = await this.driver.findAll(IOS_NATIVE_COMPOSER_LOCATOR, Math.min(IOS_NATIVE_LOOKUP_ROUND_MS, phase.remainingMs));
      if (matches.length > 1) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `native composer is ambiguous (${matches.length} matches)`);
      if (matches.length === 1) {
        await this.requireInstalledProviderForeground(IOS_INSTALLED_FOREGROUND_MS);
        await this.driver.click(matches[0], Math.max(driverRequestAllowanceMs.click, phase.remainingMs));
        return;
      }
      if (phase.remainingMs < roundMs + 250) break;
      await delay(250, phase);
    }
    throw new Error('IOS_KEYBOARD: composer was not exposed to native input');
  }

  async hideKeyboard(): Promise<void> {
    // WebDriverAgent cannot dismiss an iPhone web view keyboard because its Done button sits in Safari's accessory bar, so blur the composer as a tap elsewhere would.
    await this.attachToInstalledView();
    const blurred = await this.driver.execute<boolean>("const composer = document.querySelector('textarea[aria-label=\\\"Prompt\\\"]'); if (!composer || document.activeElement !== composer) return false; composer.blur(); return true;");
    if (blurred !== true) throw new Error('IOS_KEYBOARD: composer was not focused before dismissal');
    await this.waitForKeyboard(false);
    await this.attachToInstalledView();
    if (this.keyboardShownByPage) await this.waitForPageKeyboardHidden();
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

  captureSanitizedEvidence(name: string): Promise<void> {
    this.refreshNativeDiagnosticsBlock();
    if (this.failureCapture) return this.failureCapture;
    if (name !== 'failure') return this.captureEvidence(name);
    this.omitLaunchLogs = this.launchObservation.failure !== undefined;
    this.failureCapture = Promise.resolve().then(() => this.captureEvidence(name));
    return this.failureCapture;
  }

  private refreshNativeDiagnosticsBlock(): boolean {
    const snapshot = this.driver.snapshot();
    if (snapshot.unusable || snapshot.firstFatal) this.nativeDiagnosticsBlocked = true;
    return this.nativeDiagnosticsBlocked;
  }

  private async captureLaunchReceipt(): Promise<boolean> {
    const blocked = this.refreshNativeDiagnosticsBlock();
    const failure = this.launchObservation.failure;
    if (!failure) return true;
    this.omitLaunchLogs = true;
    try {
      this.launchReceipt = await this.launchCollector(this.udid, failure, this.origin, {
        clock: this.launchObservation.clock, queryEligible: !blocked && this.launchObservation.queryEligible,
      });
    } catch {
      this.nativeDiagnosticsBlocked = true;
      this.diagnostics.record({ phase: 'evidence', operation: 'ios-launch-receipt', detail: 'collector-unavailable-withheld' });
      return false;
    }
    this.refreshNativeDiagnosticsBlock();
    this.nativeDiagnosticsBlocked ||= this.launchReceipt.blockNativeDiagnostics;
    const clock = this.launchObservation.clock;
    if (!await observeIOSReceiptWrite(this.launchPersistence.directory, clock, 0, () => this.launchMkdir(this.outputDir, { recursive: true }))) return false;
    const { eventsText, receiptText } = this.launchReceipt;
    if (!await observeIOSReceiptWrite(this.launchPersistence.events, clock, Buffer.byteLength(eventsText),
      () => this.launchWrite(join(this.outputDir, 'ios-launch-owner-events.jsonl'), eventsText, { flag: 'wx', mode: 0o600 }))) return false;
    return observeIOSReceiptWrite(this.launchPersistence.receipt, clock, Buffer.byteLength(receiptText),
      () => this.launchWrite(join(this.outputDir, 'ios-launch-receipt.json'), receiptText, { flag: 'wx', mode: 0o600 }));
  }

  private async captureEvidence(name: string): Promise<void> {
    if (name === 'failure' && !await this.captureLaunchReceipt()) return;
    await mkdir(this.outputDir, { recursive: true });
    if (!this.refreshNativeDiagnosticsBlock()) {
      try {
        const screenshot = Buffer.from(await this.driver.screenshot(), 'base64');
        if (screenshot.byteLength <= 20 * 1024 * 1024) await writeFile(join(this.outputDir, `${name}.png`), screenshot, { mode: 0o600 });
      } catch (error) {
        this.refreshNativeDiagnosticsBlock();
        this.diagnostics.record({ phase: 'evidence', operation: 'screenshot', detail: error instanceof Error ? error.message : String(error) });
      }
      if (name === 'failure' && !this.omitLaunchLogs && !this.refreshNativeDiagnosticsBlock()) {
        try {
          const source = await this.driver.pageSource(IOS_NATIVE_HIERARCHY_COMMAND_MS);
          if (typeof source !== 'string') throw new Error('IOS_EVIDENCE: malformed hierarchy response');
          await writeBoundedText(join(this.outputDir, 'failure-hierarchy.xml'), source);
        } catch (error) {
          this.refreshNativeDiagnosticsBlock();
          this.diagnostics.record({ phase: 'evidence', operation: 'hierarchy', detail: error instanceof Error ? error.message : String(error) });
        }
      }
    }
    this.refreshNativeDiagnosticsBlock();
    await writeSanitizedJson(join(this.outputDir, `${name}-appium.json`), this.evidenceSnapshot());
    await this.diagnostics.write(join(this.outputDir, `${name}-events.json`));
    const captures: Array<[string, string[]]> = [
      ['simulator-log', ['simctl', 'spawn', this.udid, 'log', 'show', '--last', '10m', '--style', 'compact']],
      ['webkit-safari', ['simctl', 'spawn', this.udid, 'log', 'show', '--last', '10m', '--style', 'compact', '--predicate', 'process CONTAINS[c] "WebKit" OR process CONTAINS[c] "Safari"']],
      ...(name === 'failure'
        ? [['springboard', ['simctl', 'spawn', this.udid, 'log', 'show', '--last', '10m', '--style', 'compact', '--predicate', 'process == "SpringBoard" OR process == "CoreSimulatorBridge" OR process == "lsd"']]] as Array<[string, string[]]>
        : []),
      ['apps', ['simctl', 'listapps', this.udid]],
      ['wda', ['simctl', 'spawn', this.udid, 'launchctl', 'print', 'system']],
    ];
    for (const [suffix, args] of captures) {
      if (this.refreshNativeDiagnosticsBlock()) break;
      if (this.omitLaunchLogs && (suffix === 'simulator-log' || suffix === 'webkit-safari' || suffix === 'springboard')) continue;
      const output = await this.diagnosticCommand('xcrun', args, 20_000).catch((error) => error instanceof Error ? error.message : String(error));
      await writeBoundedText(join(this.outputDir, `${name}-ios-${suffix}.log`), output);
    }
  }

  evidenceSnapshot(): Record<string, unknown> {
    this.refreshNativeDiagnosticsBlock();
    const persistenceTerminal = Object.values(this.launchPersistence).find(status => status.state === 'rejected')
      ?? (this.launchPersistence.receipt.state === 'fulfilled' ? this.launchPersistence.receipt : undefined);
    const captureElapsedMs = persistenceTerminal?.settled && this.launchReceipt
      ? persistenceTerminal.settled.monoMs - this.launchReceipt.receipt.collection.started.monoMs : null;
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
      attachmentFirstRefusal: this.attachmentFirstRefusal,
      launchReceipt: this.launchReceipt?.receipt,
      launchPersistence: structuredClone(this.launchPersistence),
      launchCollectionAndPersistenceElapsedMs: captureElapsedMs,
      nativeDiagnosticsBlocked: this.nativeDiagnosticsBlocked,
      confirmationCapture: this.confirmationCapture && structuredClone(this.confirmationCapture),
      safariNavigation: {
        lastDiscovery: this.safariLastDiscovery,
        lastObservation: this.safariLastObservation,
        reserveExhaustion: this.safariReserveExhaustion,
        firstFailure: this.safariFirstFailure,
      },
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

  private async readNativeControlAttribute(element: string, name: string, timeout: number, validateResponse = true, policy?: WebDriverRequestPolicy): Promise<string | null> {
    const response = await this.driver.attribute(element, name, timeout, policy);
    if (validateResponse && response !== null && typeof response !== 'string') {
      throw new Error('IOS_SHARE: Add: malformed native attribute response');
    }
    return response;
  }

  private async nativeControlState(element: string, deadline: number | PhaseBudget): Promise<'ready' | 'hidden' | 'disabled' | 'not-hittable' | 'indeterminate'> {
    const read = async (name: string): Promise<string | null> => {
      const timeout = typeof deadline === 'number' ? deadline - Date.now()
        : deadline.remainingMs >= IOS_NATIVE_LOOKUP_ROUND_MS ? IOS_NATIVE_LOOKUP_ROUND_MS : 0;
      if (timeout < minimumDriverRequestMs) return null;
      return this.readNativeControlAttribute(element, name, timeout, deadline instanceof PhaseBudget);
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
    const timeout = Math.min(IOS_NATIVE_LOOKUP_ROUND_MS, deadline - Date.now());
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
    const timeout = Math.min(IOS_NATIVE_LOOKUP_ROUND_MS, deadline - Date.now());
    if (timeout < minimumDriverRequestMs) return undefined;
    const elements = await this.driver.findAll(iosActionCollectionLocator(), timeout).catch((error: unknown) => {
      if (isFatalDriverError(error)) throw error;
      return [];
    });
    for (const element of elements) {
      const rectTimeout = Math.min(IOS_NATIVE_LOOKUP_ROUND_MS, deadline - Date.now());
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
      const enabledTimeout = Math.min(IOS_NATIVE_LOOKUP_ROUND_MS, deadline - Date.now());
      if (enabledTimeout < minimumDriverRequestMs) break;
      const enabled = await this.driver.attribute(element, 'enabled', enabledTimeout).catch((error: unknown) => {
        if (isFatalDriverError(error)) throw error;
        return null;
      });
      if (enabled !== 'true' && enabled !== null) continue;
      const visibleTimeout = Math.min(IOS_NATIVE_LOOKUP_ROUND_MS, deadline - Date.now());
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

  private nativeGestureRequestPolicy(phase: PhaseBudget): WebDriverRequestPolicy {
    const gestureBudget = phase.phaseView('ios-share-gesture', phase.remainingMs, IOS_NATIVE_HIERARCHY_COMMAND_MS);
    return this.driver.nativeRequestPolicy(gestureBudget);
  }

  private async expandNativeShareActions(source: string, phase: PhaseBudget): Promise<boolean> {
    const commandBudget = 4 * IOS_NATIVE_LOOKUP_ROUND_MS + IOS_NATIVE_SCROLL_COMMAND_MS;
    if (phase.remainingMs < commandBudget + IOS_NATIVE_HIERARCHY_COMMAND_MS) {
      throw new Error('IOS_SHARE: insufficient time to expand and observe Share actions');
    }
    const expansion = phase.phaseView('ios-share-expansion', commandBudget, IOS_NATIVE_HIERARCHY_COMMAND_MS);
    const policy = this.driver.nativeRequestPolicy(expansion);
    const matches = await this.driver.findAll({
      using: 'xpath',
      value: "//XCUIElementTypeApplication[@name='Safari']//*[@name='ActivityListView' and @visible='true']//*[@name='ShareSheet.RemoteContainerView' and @visible='true']//*[@name='activityCollectionView']//XCUIElementTypeCell[@name='actionGroupCell' and @label='View More' and @visible='true' and @enabled='true' and not(ancestor::XCUIElementTypeWebView)]",
    }, IOS_NATIVE_LOOKUP_ROUND_MS, policy);
    if (matches.length === 0) return false;
    if (matches.length !== 1) throw new Error('IOS_SHARE: ambiguous View More action');
    for (const attribute of ['enabled', 'visible', 'hittable']) {
      if (await this.readNativeControlAttribute(matches[0], attribute, IOS_NATIVE_LOOKUP_ROUND_MS, true, policy) !== 'true') {
        throw new Error(`IOS_SHARE: View More action is not ${attribute}`);
      }
    }
    if (expansion.remainingMs < IOS_NATIVE_SCROLL_COMMAND_MS) throw new Error('IOS_SHARE: insufficient time to click View More');
    await writeBoundedText(join(this.outputDir, 'ios-share-collapsed-hierarchy.xml'), source);
    await this.driver.click(matches[0], IOS_NATIVE_SCROLL_COMMAND_MS, policy);
    return true;
  }

  private async findNativeScrollable(locators: Locator[], description: string, timeoutMs: number): Promise<string> {
    if (timeoutMs < minimumDriverRequestMs) throw new Error(`IOS_SHARE: ${description}: insufficient time to find control`);
    const deadline = Date.now() + Math.min(timeoutMs, this.budget.remainingMs);
    const phase = this.budget.phaseView('ios-share-search', timeoutMs);
    let readiness = phase.phaseView('ios-share-publication', IOS_SHARE_SHEET_READINESS_MS);
    let expanded = false;
    let lastError = '';
    let source: string | undefined;
    let actionList: NativeActionListEvidence | undefined;
    while (!actionList && readiness.remainingMs >= IOS_NATIVE_HIERARCHY_COMMAND_MS) {
      const currentSource = await this.captureNativeShareHierarchy(0, readiness);
      actionList = nativeActionListEvidence(currentSource, description);
      if (!expanded && !actionList?.targetRows.length && currentSource.includes('label="View More"')
        && await this.expandNativeShareActions(currentSource, phase)) {
        expanded = true;
        actionList = undefined;
        readiness = phase.phaseView('ios-share-expanded-publication', IOS_NATIVE_LIST_READINESS_MS);
        continue;
      }
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
      const gestureTimeout = IOS_NATIVE_GESTURE_COMMAND_MS;
      if (phase.remainingMs < IOS_NATIVE_GESTURE_COMMAND_MS + IOS_NATIVE_HIERARCHY_COMMAND_MS) {
        lastError = `${description}: insufficient time to complete native scroll and hierarchy verification`;
        break;
      }
      let scrolled = false;
      try {
        await this.driver.mobile('scroll', { element: container.element, direction, distance: 0.75 }, gestureTimeout,
          this.nativeGestureRequestPolicy(phase));
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
        const fallbackTimeout = IOS_NATIVE_GESTURE_COMMAND_MS;
        if (phase.remainingMs < IOS_NATIVE_GESTURE_COMMAND_MS + IOS_NATIVE_HIERARCHY_COMMAND_MS) {
          lastError = `${description}: insufficient time to complete fallback swipe and hierarchy verification`;
          break;
        }
        try {
          await this.driver.mobile('swipe', { element: fallbackContainer.element, direction: fallbackDirection }, fallbackTimeout,
            this.nativeGestureRequestPolicy(phase));
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

  private homeObservationRequest(phase: PhaseBudget, cap: number, allowance: number) {
    const nativePolicy = this.driver.nativeRequestPolicy(phase);
    const timeout = Math.max(Math.ceil(allowance / timingScale), Math.min(cap, Math.floor(phase.remainingMs / timingScale)));
    const policy: WebDriverRequestPolicy = {
      ...nativePolicy,
      beforeDispatch: () => {
        nativePolicy.beforeDispatch();
        if (phase.remainingMs < Math.max(timeout, allowance)) throw new IOSHomeObservationShortfall(phase);
      },
    };
    const assertComplete = () => {
      nativePolicy.beforeDispatch();
      phase.assertAvailable('complete Home observation request');
    };
    return { timeout, policy, assertComplete };
  }

  private async readNativeHomeAttribute(element: string, name: string, phase: PhaseBudget): Promise<string | null> {
    const request = this.homeObservationRequest(phase, IOS_NATIVE_LOOKUP_ROUND_MS, driverRequestAllowanceMs.attribute);
    const value = await this.driver.attribute(element, name, request.timeout, request.policy);
    request.assertComplete();
    if (value !== null && typeof value !== 'string') {
      this.failOwnership('IOS_CONTEXT_OWNERSHIP', `Home ${name} attribute response was malformed`);
    }
    return value;
  }

  // On arrival SpringBoard can still be animating, so the first observation also waits for an icon that is not yet visible.
  private async waitForHomeIconReadiness(observation: IOSHomeObservation, parent: PhaseBudget, firstObservation = false): Promise<IOSHomeObservation> {
    const settling = (current: IOSHomeObservation) => current.state === 'pending' || current.state === 'not-ready'
      || (firstObservation && current.state === 'missing');
    let phase: PhaseBudget | undefined;
    let attemptedRounds = 1;
    let completedRounds = 1;
    let lastCompleteAttemptOrdinal = this.driver.snapshot().lastCommand?.timing?.attemptId ?? null;
    let outcome: 'complete' | 'no-budget' | 'local-shortfall' | 'error' = 'error';
    try {
      const owner = this.driver.nativeRequestPolicy(parent);
      owner.beforeDispatch();
      parent.assertAvailable('settle Home observation');
      if (!settling(observation)) {
        outcome = 'complete';
        return observation;
      }
      phase = parent.phaseView('ios-home-readiness', IOS_NATIVE_LIST_READINESS_MS);
      while (settling(observation) && phase.remainingMs >= IOS_NATIVE_HIERARCHY_COMMAND_MS + minimumDriverRequestMs) {
        if (phase.remainingMs < 250 + minimumDriverRequestMs) break;
        await delay(250, phase);
        attemptedRounds += 1;
        try {
          owner.beforeDispatch();
          parent.assertAvailable('observe Home readiness');
          const complete = await this.observeHomeIcon(phase);
          owner.beforeDispatch();
          parent.assertAvailable('accept Home readiness');
          observation = complete;
          completedRounds += 1;
          lastCompleteAttemptOrdinal = this.driver.snapshot().lastCommand?.timing?.attemptId ?? null;
        } catch (error) {
          if (!(error instanceof IOSHomeObservationShortfall) || error.phase !== phase) throw error;
          owner.beforeDispatch();
          parent.assertAvailable('retain complete Home observation');
          outcome = 'local-shortfall';
          return observation;
        }
      }
      owner.beforeDispatch();
      parent.assertAvailable('retain Home readiness');
      outcome = settling(observation) ? 'no-budget' : 'complete';
      return observation;
    } finally {
      this.diagnostics.record({ phase: 'ios-home-readiness', operation: 'terminal-observation', detail: {
        outcome, state: observation.state, pageCurrent: observation.page?.current ?? null, pageTotal: observation.page?.total ?? null,
        classificationCounts: observation.classificationCounts ?? null, attemptedRounds, completedRounds,
        localRemainingMs: phase?.remainingMs ?? null, parentRemainingMs: parent.remainingMs, lastCompleteAttemptOrdinal,
        attemptOrdinal: outcome === 'complete' || outcome === 'no-budget' ? lastCompleteAttemptOrdinal : null,
      } });
    }
  }

  // The Today View left of the first page keeps the icon container published but shows no page indicator or icons.
  private async leaveTodayView(observation: IOSHomeObservation, phase: PhaseBudget): Promise<IOSHomeObservation> {
    if (observation.state !== 'pending' || !observation.container || observation.reason !== IOS_HOME_PAGE_INDICATOR_MISSING) return observation;
    phase.assertAvailable('leave SpringBoard Today View');
    await this.ensureSpringBoardForeground(phase);
    await this.driver.mobile('swipe', { direction: 'left' }, Math.max(2, phase.remainingMs));
    await this.ensureSpringBoardForeground(phase);
    return this.waitForHomeIconReadiness(await this.observeHomeIcon(phase), phase, true);
  }

  private async homeObservationElementIds(phase: PhaseBudget): Promise<string[]> {
    const request = this.homeObservationRequest(phase, IOS_NATIVE_HIERARCHY_COMMAND_MS, driverRequestAllowanceMs.lookup);
    const response = await this.driver.command<unknown>('/elements', 'POST', iosHomeObservationLocator(), request.timeout, request.policy);
    request.assertComplete();
    if (!Array.isArray(response)) this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'Home observation element response was not an array');
    const rawIds = response.map((candidate: unknown, index: number) => {
      if (!candidate || typeof candidate !== 'object' || Array.isArray(candidate)) {
        this.failOwnership('IOS_CONTEXT_OWNERSHIP', `Home observation element reference ${index} was malformed`);
      }
      const record = candidate as Record<string, unknown>;
      const references = ['element-6066-11e4-a52e-4f735466cecf', 'ELEMENT']
        .filter((key) => Object.hasOwn(record, key))
        .map((key) => record[key]);
      if (!references.length || references.some((reference) => typeof reference !== 'string' || !reference.trim())
        || new Set(references).size !== 1) {
        this.failOwnership('IOS_CONTEXT_OWNERSHIP', `Home observation element reference ${index} was malformed`);
      }
      return references[0] as string;
    });
    return [...new Set(rawIds)];
  }

  private async observeHomeIcon(phase: PhaseBudget): Promise<IOSHomeObservation> {
    const ids = await this.homeObservationElementIds(phase);
    if (!ids.length) return { state: 'pending', classificationCounts: { container: 0, page: 0, icon: 0 }, reason: 'Home page elements are not published' };

    const classified: Array<{ id: string; kind: 'container' | 'icon' | 'page' }> = [];
    for (const id of ids) {
      const attributes: Record<string, string | null> = {};
      for (const name of ['type', 'name', 'label']) {
        attributes[name] = await this.readNativeHomeAttribute(id, name, phase);
      }
      const kind = iosHomeElementKind(attributes);
      if (!kind) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `Home observation returned an unclassified element ${id}`);
      classified.push({ id, kind });
    }

    const containers = classified.filter((element) => element.kind === 'container');
    if (containers.length > 1) {
      this.failOwnership('IOS_CONTEXT_OWNERSHIP', `Home screen icon container is ambiguous (${containers.map((element) => element.id).join(', ')})`);
    }
    const pages = classified.filter((element) => element.kind === 'page');
    if (pages.length > 1) {
      this.failOwnership('IOS_CONTEXT_OWNERSHIP', `Home page indicator is ambiguous (${pages.map((element) => element.id).join(', ')})`);
    }
    const icons = classified.filter((element) => element.kind === 'icon');
    const classificationCounts = { container: containers.length, page: pages.length, icon: icons.length };
    if (!containers.length) return { state: 'pending', classificationCounts, reason: 'Home screen icon container is missing' };
    if (!pages.length) return { state: 'pending', classificationCounts, container: containers[0].id, reason: IOS_HOME_PAGE_INDICATOR_MISSING };

    const pageValue = await this.readNativeHomeAttribute(pages[0].id, 'value', phase);
    const page = typeof pageValue === 'string' ? parseIOSHomePage(pageValue) : undefined;
    if (!page) return { state: 'pending', classificationCounts, container: containers[0].id, reason: 'Home page indicator value is invalid' };

    if (icons.length > 1) {
      this.failOwnership('IOS_CONTEXT_OWNERSHIP', `Home screen icon is ambiguous (${icons.map((element) => element.id).join(', ')})`);
    }
    if (!icons.length) return { state: 'missing', classificationCounts, container: containers[0].id, page, reason: 'installed provider icon is not on the current page' };

    const icon = icons[0].id;
    const attributes: Record<string, string | null> = {};
    for (const name of ['enabled', 'visible', 'hittable']) {
      attributes[name] = await this.readNativeHomeAttribute(icon, name, phase);
    }
    const ready = attributes.enabled === 'true' && attributes.visible === 'true' && attributes.hittable === 'true';
    return {
      state: ready ? 'ready' : 'not-ready',
      classificationCounts,
      container: containers[0].id,
      icon,
      page,
      attributes,
      reason: ready ? 'installed provider icon is ready' : 'installed provider icon is not ready',
    };
  }

  private async waitForInstalledProvider(timeoutMs: number, parent = this.budget): Promise<boolean> {
    const phase = parent.phaseView('ios-provider', timeoutMs);
    const policy = this.driver.nativeRequestPolicy(phase);
    while (!phase.exhausted) {
      phase.assertAvailable('discover installed provider');
      if (phase.remainingMs < driverRequestAllowanceMs.generic) break;
      await this.driver.switchContext('NATIVE_APP', phase.remainingMs);
      const requestTimeout = phase.remainingMs >= IOS_NATIVE_LOOKUP_ROUND_MS * timingScale
        ? IOS_NATIVE_LOOKUP_ROUND_MS : Math.min(driverRequestAllowanceMs.lookup, Math.floor(phase.remainingMs / timingScale));
      const state = await this.driver.mobile('queryAppState', { bundleId: IOS_INSTALLED_BUNDLE_ID }, requestTimeout, policy);
      policy.beforeDispatch();
      if (state === 4) {
        this.installedBundleId = IOS_INSTALLED_BUNDLE_ID;
        return true;
      }
      if (state !== 1 && state !== 2 && state !== 3) this.failOwnership('IOS_CONTEXT_OWNERSHIP', 'installed provider state is missing or invalid');
      if (phase.remainingMs < requestTimeout) break;
      const homeTimeout = Math.min(requestTimeout, Math.floor(phase.remainingMs / timingScale));
      const homeState = await this.driver.mobile('queryAppState', { bundleId: IOS_SPRINGBOARD_BUNDLE_ID }, homeTimeout, policy);
      policy.beforeDispatch();
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

  private async observeCurrentNativeForeground(phase: PhaseBudget, timeoutMs?: number): Promise<string> {
    this.assertOwnershipClear();
    try {
      if (this.driver.snapshot().selectedContext !== 'NATIVE_APP') {
        await this.driver.switchContext('NATIVE_APP', timeoutMs ?? phase.remainingMs);
      }
      phase.assertAvailable('observe current native foreground');
      const info = await this.driver.activeAppInfo(timeoutMs ?? phase.remainingMs);
      const activeValue = info?.bundleId ?? info?.bundleID;
      const active = typeof activeValue === 'string' ? activeValue : '';
      const pid = info?.pid;
      if (!active.trim() || active !== active.trim() || (typeof pid !== 'number' && typeof pid !== 'string')
        || !/^[1-9]\d*$/u.test(String(pid)) || !Number.isSafeInteger(Number(pid))) {
        this.failOwnership('IOS_CONTEXT_OWNERSHIP', `native foreground is not positively identified (${active || String(activeValue || 'unknown')}, PID ${String(pid)})`);
      }
      if (!isIOSAllowedLaunchForeground(active)) {
        this.failOwnership('IOS_CONTEXT_OWNERSHIP', `native foreground ${active} is not an allowed iOS lifecycle identity`);
      }
      this.lastNativeActivity = String(info?.activity || info?.appActivity || '');
      this.lastNativePid = String(pid);
      return active;
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

  private async requireInstalledProviderForeground(timeoutMs = 30_000, parent = this.budget): Promise<void> {
    if (!this.installedBundleId) throw new Error('IOS_CONTEXT: installed provider identity is unavailable');
    if (this.installedBundleId !== IOS_INSTALLED_BUNDLE_ID) this.failOwnership('IOS_CONTEXT_OWNERSHIP', `installed provider ${this.installedBundleId} is not ${IOS_INSTALLED_BUNDLE_ID}`);
    await this.observeNativeForeground(this.installedBundleId, parent.phaseView('ios-native-foreground', Math.min(30_000, timeoutMs)));
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

  private async waitForKeyboard(expected: boolean, pageEvidence?: () => Promise<string | true>): Promise<void> {
    const phase = this.budget.phaseView('ios-keyboard', IOS_KEYBOARD_PHASE_MS);
    let last = '';
    let polls = 0;
    let keyboardElementSeen = false;
    while (!phase.exhausted) {
      phase.assertAvailable(`keyboard ${expected ? 'show' : 'hide'}`);
      if (phase.remainingMs < driverRequestAllowanceMs.generic + driverRequestAllowanceMs.lookup) break;
      await this.driver.switchContext('NATIVE_APP', Math.max(2, phase.remainingMs));
      const findTimeout = Math.min(IOS_NATIVE_LOOKUP_ROUND_MS, phase.remainingMs);
      if (findTimeout < driverRequestAllowanceMs.lookup) break;
      const elements = await this.driver.findAll({ using: 'class name', value: 'XCUIElementTypeKeyboard' }, findTimeout);
      polls++;
      if (elements.length) keyboardElementSeen = true;
      let visible = false;
      for (const element of elements) {
        const rectTimeout = Math.min(IOS_NATIVE_LOOKUP_ROUND_MS, phase.remainingMs);
        if (rectTimeout <= 1) break;
        const rect = await this.driver.elementRect(element, rectTimeout).catch((error: unknown) => {
          if (isFatalDriverError(error)) throw error;
          return { x: 0, y: 0, width: 0, height: 0 };
        });
        const shownTimeout = Math.min(IOS_NATIVE_LOOKUP_ROUND_MS, phase.remainingMs);
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
    let pageDetail = '';
    if (expected && polls > 0 && !keyboardElementSeen && pageEvidence) {
      try {
        const evidence = await pageEvidence();
        if (evidence === true) return;
        pageDetail = `; page ${evidence}`;
      } catch (error) {
        if (isFatalDriverError(error)) throw error;
        pageDetail = `; page unmeasured (${error instanceof Error ? error.name : 'unknown'})`;
      }
    }
    throw new Error(`IOS_KEYBOARD: expected ${expected ? 'visible' : 'hidden'} native keyboard (${last}${expected && keyboardElementSeen ? ', element seen' : ''})${pageDetail}`);
  }

  private assertOwnershipClear(): void {
    if (this.ownershipFailure) throw this.ownershipFailure;
    if (this.nativeObservationFailure !== undefined) throw this.nativeObservationFailure;
    if (this.installationAddFailure !== undefined) throw this.installationAddFailure;
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
