import { redactText } from './diagnostics';
import { PhaseBudget, PhaseBudgetError } from './budget';

export interface WebDriverElement {
  [key: string]: string;
}

export type Locator = { using: string; value: string };

interface WebDriverResponse<T = any> {
  value: T;
  sessionId?: string;
}

export interface SessionOptions {
  capabilities: Record<string, unknown>;
  requestTimeoutMs?: number;
  budget?: PhaseBudget;
}

export interface ContextMetadata {
  id: string;
  url?: string;
  title?: string;
  bundleId?: string;
  isKey?: boolean;
  raw: Record<string, unknown>;
}

export interface WebDriverSnapshot {
  sessionId: string;
  selectedContext: string;
  selectedWindow: string;
  unusable: boolean;
  lastCommand?: WebDriverCommandEvidence;
  commands: WebDriverCommandEvidence[];
  lookups: WebDriverLookupEvidence[];
  firstFatal?: WebDriverFatalEvidence;
}

export interface WebDriverFatalEvidence {
  code: string;
  path: string;
  method: string;
  at: number;
  detail: string;
}

export interface WebDriverLookupEvidence {
  locator: Locator;
  sliceMs: number;
  startedAt: number;
  endedAt: number;
  remainingMs: number;
  outcome: 'matched' | 'retryable' | 'fatal';
  error?: string;
}

export interface WebDriverCommandEvidence {
  command: string;
  path: string;
  method: string;
  durationMs: number;
  timeoutMs: number;
  timedOut: boolean;
  selectedContext: string;
  selectedWindow: string;
  error?: string;
}

export class WebDriverError extends Error {
  readonly code: string;
  readonly path: string;
  readonly method: string;
  readonly durationMs: number;
  readonly timedOut: boolean;
  readonly selectedContext: string;
  readonly selectedWindow: string;
  readonly status?: number;
  readonly cause?: unknown;

  constructor(options: {
    code: string;
    message: string;
    path: string;
    method: string;
    durationMs: number;
    timedOut?: boolean;
    selectedContext: string;
    selectedWindow: string;
    status?: number;
    cause?: unknown;
  }) {
    super(`${options.code}: ${redactText(options.message).slice(0, 1_000)}`);
    this.name = 'WebDriverError';
    this.code = options.code;
    this.path = options.path;
    this.method = options.method;
    this.durationMs = options.durationMs;
    this.timedOut = options.timedOut === true;
    this.selectedContext = options.selectedContext;
    this.selectedWindow = options.selectedWindow;
    this.status = options.status;
    this.cause = options.cause;
  }
}

export class ElementLookupError extends Error {
  readonly code = 'APPIUM_ELEMENT';

  constructor(message: string) {
    super(message);
    this.name = 'ElementLookupError';
  }
}

export function isFatalDriverError(error: unknown): boolean {
  if (error instanceof PhaseBudgetError) return true;
  if (error instanceof WebDriverError) {
    return error.timedOut
      || error.code === 'APPIUM_SESSION_UNUSABLE'
      || (error.method === 'DELETE' && /\/session\/[^/]+$/u.test(error.path));
  }
  const message = error instanceof Error ? error.message : String(error);
  return /APPIUM_(?:TIMEOUT|SESSION_UNUSABLE)/u.test(message);
}

export function isRetryableElementLookupError(error: unknown): boolean {
  if (error instanceof ElementLookupError) return true;
  if (!(error instanceof WebDriverError)) return false;
  if (error.code === 'APPIUM_COMMAND_NOT_ADMITTED') return true;
  if (error.code !== 'APPIUM_COMMAND') return false;
  return error.status === 404 || /no such element|stale element reference|element not found|could not be located|unable to find element/iu.test(error.message);
}

export type FetchTransport = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function isTimeoutError(error: unknown): boolean {
  if (error instanceof Error && error.name === 'TimeoutError') return true;
  if (error && typeof error === 'object' && 'name' in error && (error as { name?: unknown }).name === 'TimeoutError') return true;
  return Boolean(error && typeof error === 'object' && 'code' in error && (error as { code?: unknown }).code === 'ETIMEDOUT');
}

const lookupSliceMs = 5_000;
export const minimumDriverRequestMs = 50;
export const driverRequestAllowanceMs = {
  generic: 250,
  lookup: 1_000,
  attribute: 750,
  click: 1_000,
  gesture: 4_000,
  navigation: 5_000,
  source: 5_000,
} as const;

export function driverCommandAllowance(path: string, method: string, body?: unknown): number {
  if (path === '/session' && method === 'POST') return 0;
  if ((path === '/element' || path === '/elements') && method === 'POST') return driverRequestAllowanceMs.lookup;
  if (/\/attribute\/|\/text$/u.test(path)) return driverRequestAllowanceMs.attribute;
  if (/\/click$/u.test(path)) return driverRequestAllowanceMs.click;
  if (path === '/source' || path === '/screenshot') return driverRequestAllowanceMs.source;
  if (path === '/window/rect' || path === '/window/handles' || path === '/window') return driverRequestAllowanceMs.attribute;
  if (path === '/url' && method === 'POST') return driverRequestAllowanceMs.navigation;
  if (path === '/execute/sync' && method === 'POST') {
    const script = body && typeof body === 'object' && 'script' in body
      ? String((body as { script?: unknown }).script || '')
      : '';
    if (/^mobile:\s*(?:scrollGesture|scroll)$/u.test(script)) return driverRequestAllowanceMs.gesture;
    if (/^mobile:\s*(?:swipe|tap|pressButton|activateApp|terminateApp|hideKeyboard)$/u.test(script)) return 2_000;
    return driverRequestAllowanceMs.lookup;
  }
  return driverRequestAllowanceMs.generic;
}

export function isCommandAdmissionError(error: unknown): boolean {
  return error instanceof WebDriverError && error.code === 'APPIUM_COMMAND_NOT_ADMITTED';
}

export class AppiumClient {
  private sessionId = '';
  private readonly baseUrl: string;
  private readonly requestTimeoutMs: number;
  private readonly transport: FetchTransport;
  private budget?: PhaseBudget;
  private selectedContext = 'NATIVE_APP';
  private selectedWindow = '';
  private unusable = false;
  private readonly history: WebDriverCommandEvidence[] = [];
  private readonly lookupHistory: WebDriverLookupEvidence[] = [];
  private firstFatal?: WebDriverFatalEvidence;

  constructor(baseUrl = 'http://127.0.0.1:4723', requestTimeoutMs = 30_000, transport: FetchTransport = fetch) {
    this.baseUrl = baseUrl.replace(/\/$/u, '');
    this.requestTimeoutMs = requestTimeoutMs;
    this.transport = transport;
  }

  setBudget(budget: PhaseBudget | undefined): void {
    this.budget = budget;
  }

  snapshot(): WebDriverSnapshot {
    return {
      sessionId: this.sessionId ? '[active]' : '',
      selectedContext: this.selectedContext,
      selectedWindow: this.selectedWindow,
      unusable: this.unusable,
      lastCommand: this.history.at(-1),
      commands: this.history.slice(-50),
      lookups: this.lookupHistory.slice(-100),
      firstFatal: this.firstFatal,
    };
  }

  async create(options: SessionOptions): Promise<Record<string, unknown>> {
    if (this.unusable) {
      throw new WebDriverError({
        code: 'APPIUM_SESSION_UNUSABLE',
        message: 'the previous session operation timed out; bounded teardown is required before replacement',
        path: '/session',
        method: 'POST',
        durationMs: 0,
        selectedContext: this.selectedContext,
        selectedWindow: this.selectedWindow,
      });
    }
    options.budget?.assertAvailable('create session');
    if (options.budget) this.setBudget(options.budget);
    let response: WebDriverResponse<{ value: Record<string, unknown>; sessionId?: string }>;
    try {
      response = await this.request<{ value: Record<string, unknown>; sessionId?: string }>('/session', 'POST', {
        capabilities: {
          alwaysMatch: options.capabilities,
          firstMatch: [{}],
        },
      }, options.requestTimeoutMs || this.requestTimeoutMs, false);
    } catch (error) {
      if (isFatalDriverError(error)) this.recordFatal(error);
      throw error;
    }
    const value = response.value as unknown as WebDriverResponse<Record<string, unknown>>;
    this.sessionId = String(response.sessionId || (value as any)?.sessionId || '');
    const capabilities = (value as any)?.value || value;
    if (!this.sessionId) throw new Error('APPIUM_SESSION: server did not return a session id');
    this.unusable = false;
    this.selectedContext = 'NATIVE_APP';
    this.selectedWindow = '';
    return capabilities as Record<string, unknown>;
  }

  async close(): Promise<void> {
    if (!this.sessionId) return;
    const session = this.sessionId;
    try {
      await this.request(`/session/${encodeURIComponent(session)}`, 'DELETE', undefined, this.requestTimeoutMs, false, false, true);
      this.sessionId = '';
      this.unusable = false;
    } catch (error) {
      if (isFatalDriverError(error)) this.recordFatal(error);
      if (error instanceof WebDriverError && (error.status === 404
        || (error.code === 'APPIUM_HTTP' && error.status !== undefined && error.status >= 200 && error.status < 300))) {
        this.sessionId = '';
        this.unusable = false;
        return;
      }
      this.unusable = true;
      throw error;
    }
  }

  async contexts(timeoutMs?: number): Promise<string[]> {
    return this.command<string[]>('/contexts', 'GET', undefined, timeoutMs);
  }

  async contextMetadataRaw(timeoutMs?: number): Promise<unknown> {
    return this.mobile('getContexts', {}, timeoutMs);
  }

  async contextMetadata(timeoutMs?: number): Promise<ContextMetadata[]> {
    const value = await this.contextMetadataRaw(timeoutMs);
    const entries = Array.isArray(value)
      ? value
      : value && typeof value === 'object' && Array.isArray((value as any).contexts)
        ? (value as any).contexts
        : [];
    return entries.flatMap((entry: unknown) => {
      if (typeof entry === 'string') return [{ id: entry, raw: { id: entry } }];
      if (!entry || typeof entry !== 'object') return [];
      const raw = entry as Record<string, unknown>;
      const id = String(raw.id || raw.context || raw.name || '');
      if (!id) return [];
      return [{
        id,
        url: typeof raw.url === 'string' ? raw.url : undefined,
        title: typeof raw.title === 'string' ? raw.title : undefined,
        bundleId: typeof raw.bundleId === 'string' ? raw.bundleId : typeof raw.bundleID === 'string' ? raw.bundleID : undefined,
        isKey: raw.isKey === true || raw.isKeyWindow === true,
        raw,
      }];
    });
  }

  async switchContext(name: string, timeoutMs?: number): Promise<void> {
    await this.command('/context', 'POST', { name }, timeoutMs);
    this.selectedContext = name;
  }

  async currentUrl(timeoutMs?: number): Promise<string> {
    return this.command<string>('/url', 'GET', undefined, timeoutMs);
  }

  async navigate(url: string, timeoutMs?: number): Promise<void> {
    await this.command('/url', 'POST', { url }, timeoutMs);
  }

  async pageSource(timeoutMs?: number): Promise<string> {
    return this.command<string>('/source', 'GET', undefined, timeoutMs);
  }

  async windowHandles(timeoutMs?: number): Promise<string[]> {
    return this.command<string[]>('/window/handles', 'GET', undefined, timeoutMs);
  }

  async currentWindow(timeoutMs?: number): Promise<string> {
    return this.command<string>('/window', 'GET', undefined, timeoutMs);
  }

  async switchWindow(handle: string, timeoutMs?: number): Promise<void> {
    await this.command('/window', 'POST', { handle }, timeoutMs);
    this.selectedWindow = handle;
  }

  async activeAppInfo(timeoutMs?: number): Promise<Record<string, unknown> | null> {
    const value = await this.mobile('activeAppInfo', {}, timeoutMs);
    return value && typeof value === 'object' ? value as Record<string, unknown> : null;
  }

  async settings(timeoutMs?: number): Promise<Record<string, unknown>> {
    return this.command<Record<string, unknown>>('/appium/settings', 'GET', undefined, timeoutMs);
  }

  async updateSettings(settings: Record<string, unknown>, timeoutMs?: number): Promise<Record<string, unknown>> {
    return this.command<Record<string, unknown>>('/appium/settings', 'POST', { settings }, timeoutMs);
  }

  async find(locator: Locator, timeoutMs = 30_000): Promise<string> {
    const budget = this.phaseBudget(timeoutMs, `find ${locator.using}`);
    const deadline = Date.now() + Math.min(timeoutMs, budget.remainingMs);
    const allowance = driverCommandAllowance('/element', 'POST', locator);
    let lastError = 'element not found';
    while (!budget.exhausted && Date.now() < deadline) {
      budget.assertAvailable(`find ${locator.using}`);
      const remaining = Math.min(deadline - Date.now(), budget.remainingMs);
      if (remaining <= 1 || (this.budget && remaining < allowance)) break;
      const sliceMs = Math.min(lookupSliceMs, remaining);
      const startedAt = Date.now();
      try {
        const element = await this.findOnce(locator, sliceMs);
        this.recordLookup(locator, sliceMs, startedAt, deadline, 'matched');
        return element;
      } catch (error) {
        const retryable = isRetryableElementLookupError(error);
        this.recordLookup(locator, sliceMs, startedAt, deadline, isFatalDriverError(error) || !retryable ? 'fatal' : 'retryable', error);
        if (!retryable) throw error;
        lastError = error instanceof Error ? error.message : String(error);
        if (isCommandAdmissionError(error)) break;
      }
      const waitMs = Math.min(100, deadline - Date.now(), budget.remainingMs);
      if (waitMs <= 0) break;
      await delay(waitMs);
      if (this.budget?.exhausted) this.budget.assertAvailable(`find ${locator.using}`);
    }
    if (this.budget?.exhausted) this.budget.assertAvailable(`find ${locator.using}`);
    throw new ElementLookupError(`APPIUM_ELEMENT: ${locator.using}=${redactText(locator.value)}: ${lastError}`);
  }

  private async findOnce(locator: Locator, timeoutMs: number): Promise<string> {
    const value = await this.command<Record<string, string>>('/element', 'POST', locator, timeoutMs);
    const element = value['element-6066-11e4-a52e-4f735466cecf'] || value.ELEMENT;
    if (element) return element;
    throw new ElementLookupError(`APPIUM_ELEMENT: ${locator.using}=${redactText(locator.value)}: element response did not contain an id`);
  }

  async findAll(locator: Locator, timeoutMs = this.requestTimeoutMs): Promise<string[]> {
    const values = await this.command<Record<string, string>[]>('/elements', 'POST', locator, timeoutMs);
    return values.map((value) => value['element-6066-11e4-a52e-4f735466cecf'] || value.ELEMENT).filter(Boolean);
  }

  async click(element: string, timeoutMs?: number): Promise<void> {
    await this.command(`/element/${encodeURIComponent(element)}/click`, 'POST', undefined, timeoutMs);
  }

  async sendKeys(element: string, text: string, timeoutMs?: number): Promise<void> {
    await this.command(`/element/${encodeURIComponent(element)}/value`, 'POST', {
      text,
      value: [...text],
    }, timeoutMs);
  }

  async text(element: string, timeoutMs?: number): Promise<string> {
    return this.command<string>(`/element/${encodeURIComponent(element)}/text`, 'GET', undefined, timeoutMs);
  }

  async attribute(element: string, name: string, timeoutMs?: number): Promise<string | null> {
    return this.command<string | null>(`/element/${encodeURIComponent(element)}/attribute/${encodeURIComponent(name)}`, 'GET', undefined, timeoutMs);
  }

  async elementRect(element: string, timeoutMs?: number): Promise<{ x: number; y: number; width: number; height: number }> {
    return this.command(`/element/${encodeURIComponent(element)}/rect`, 'GET', undefined, timeoutMs);
  }

  async execute<T = unknown>(script: string, args: unknown[] = [], timeoutMs?: number): Promise<T> {
    return this.command<T>('/execute/sync', 'POST', { script, args }, timeoutMs);
  }

  async screenshot(timeoutMs?: number): Promise<string> {
    return this.command<string>('/screenshot', 'GET', undefined, timeoutMs);
  }

  async windowSize(timeoutMs?: number): Promise<{ width: number; height: number }> {
    const rect = await this.command<{ width: number; height: number }>('/window/rect', 'GET', undefined, timeoutMs);
    return { width: rect.width, height: rect.height };
  }

  async back(timeoutMs?: number): Promise<void> {
    await this.command('/back', 'POST', undefined, timeoutMs);
  }

  async performActions(actions: unknown[], timeoutMs?: number): Promise<void> {
    await this.command('/actions', 'POST', { actions }, timeoutMs);
  }

  async mobile(command: string, args: Record<string, unknown> = {}, timeoutMs?: number): Promise<unknown> {
    return this.command('/execute/sync', 'POST', { script: `mobile: ${command}`, args }, timeoutMs);
  }

  async command<T = unknown>(path: string, method: string, body?: unknown, timeoutMs?: number): Promise<T> {
    this.assertUsable(path);
    try {
      const response = await this.request<T>(this.sessionPath(path), method, body, timeoutMs);
      return response.value as T;
    } catch (error) {
      if (isFatalDriverError(error)) this.recordFatal(error);
      if (error instanceof WebDriverError && error.timedOut && path !== '/status') this.unusable = true;
      throw error;
    }
  }

  private phaseBudget(timeoutMs: number, operation: string): PhaseBudget {
    return this.budget?.phaseView(operation, timeoutMs) || new PhaseBudget(operation, { timeoutMs, recoveryLimit: 0 });
  }

  private assertUsable(path: string): void {
    if (this.unusable && path !== '/session' && !path.endsWith('/status')) {
      const error = new WebDriverError({
        code: 'APPIUM_SESSION_UNUSABLE',
        message: 'the previous command timed out; session replacement is required',
        path,
        method: 'COMMAND',
        durationMs: 0,
        selectedContext: this.selectedContext,
        selectedWindow: this.selectedWindow,
      });
      this.recordFatal(error);
      throw error;
    }
  }

  private sessionPath(path: string): string {
    if (!this.sessionId) throw new Error('APPIUM_SESSION: no active session');
    return `/session/${encodeURIComponent(this.sessionId)}${path}`;
  }

  private async request<T>(
    path: string,
    method: string,
    body?: unknown,
    timeoutMs = this.requestTimeoutMs,
    checkSession = true,
    enforceBudget = true,
    allowEmptyResponse = false,
  ): Promise<WebDriverResponse<T>> {
    if (checkSession) this.assertUsable(path);
    const operation = `${method} ${path}`;
    if (enforceBudget) this.budget?.assertAvailable(operation);
    const operationTimeoutMs = timeoutMs ?? this.requestTimeoutMs;
    const budgetRemainingMs = enforceBudget ? this.budget?.remainingMs ?? operationTimeoutMs : operationTimeoutMs;
    const requestTimeoutMs = Math.max(1, Math.min(operationTimeoutMs, budgetRemainingMs));
    const commandPath = path.replace(/^\/session\/[^/]+(?=\/)/u, '');
    const allowance = driverCommandAllowance(commandPath, method, body);
    const startedAt = Date.now();
    if (enforceBudget && this.budget && requestTimeoutMs < allowance) {
      const error = new WebDriverError({
        code: 'APPIUM_COMMAND_NOT_ADMITTED',
        message: `${operation} has ${requestTimeoutMs}ms available; ${allowance}ms is required to complete the command`,
        path,
        method,
        durationMs: 0,
        selectedContext: this.selectedContext,
        selectedWindow: this.selectedWindow,
      });
      this.recordCommand(operation, path, method, startedAt, requestTimeoutMs, false, error.message);
      throw error;
    }
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const timeoutPromise = new Promise<never>((_, reject) => {
      timer = setTimeout(() => {
        const error = new DOMException(`${operation} timed out`, 'TimeoutError');
        controller.abort(error);
        reject(error);
      }, requestTimeoutMs);
    });
    let response: Response | undefined;
    let text: string;
    try {
      response = await Promise.race([
        this.transport(`${this.baseUrl}${path}`, {
          method,
          headers: body === undefined ? undefined : { 'content-type': 'application/json' },
          body: body === undefined ? undefined : JSON.stringify(body),
          signal: controller.signal,
        }),
        timeoutPromise,
      ]);
      text = await Promise.race([response.text(), timeoutPromise]);
    } catch (error) {
      const timedOut = isTimeoutError(error) || controller.signal.aborted || (enforceBudget && this.budget?.exhausted === true);
      if (timer !== undefined) clearTimeout(timer);
      if (timedOut) this.unusable = true;
      const command = this.recordCommand(operation, path, method, startedAt, requestTimeoutMs, timedOut, error instanceof Error ? error.message : String(error));
      throw new WebDriverError({
        code: timedOut ? 'APPIUM_TIMEOUT' : 'APPIUM_HTTP',
        message: error instanceof Error ? error.message : String(error),
        path,
        method,
        durationMs: command.durationMs,
        timedOut,
        selectedContext: this.selectedContext,
        selectedWindow: this.selectedWindow,
        status: response?.status,
        cause: error,
      });
    } finally {
      if (timer !== undefined) clearTimeout(timer);
    }
    if (allowEmptyResponse && response.ok && text.trim() === '') {
      this.recordCommand(operation, path, method, startedAt, requestTimeoutMs, false);
      return { value: undefined as T };
    }
    let parsed: WebDriverResponse<T>;
    try {
      parsed = JSON.parse(text) as WebDriverResponse<T>;
    } catch (error) {
      const command = this.recordCommand(operation, path, method, startedAt, requestTimeoutMs, false, `HTTP ${response.status}`);
      throw new WebDriverError({
        code: 'APPIUM_HTTP',
        message: `HTTP ${response.status}`,
        path,
        method,
        durationMs: command.durationMs,
        selectedContext: this.selectedContext,
        selectedWindow: this.selectedWindow,
        status: response.status,
        cause: error,
      });
    }
    if (!response.ok || (parsed as any).value?.error) {
      const detail = typeof (parsed as any).value === 'object'
        ? JSON.stringify((parsed as any).value)
        : String((parsed as any).value || text);
      const command = this.recordCommand(operation, path, method, startedAt, requestTimeoutMs, false, detail);
      throw new WebDriverError({
        code: 'APPIUM_COMMAND',
        message: `HTTP ${response.status}: ${redactText(detail).slice(0, 500)}`,
        path,
        method,
        durationMs: command.durationMs,
        selectedContext: this.selectedContext,
        selectedWindow: this.selectedWindow,
        status: response.status,
      });
    }
    this.recordCommand(operation, path, method, startedAt, requestTimeoutMs, false);
    return parsed;
  }

  private recordCommand(command: string, path: string, method: string, startedAt: number, timeoutMs: number, timedOut: boolean, error?: string): WebDriverCommandEvidence {
    const evidence: WebDriverCommandEvidence = {
      command,
      path,
      method,
      durationMs: Date.now() - startedAt,
      timeoutMs,
      timedOut,
      selectedContext: this.selectedContext,
      selectedWindow: this.selectedWindow,
      ...(error ? { error: redactText(error).slice(0, 500) } : {}),
    };
    this.history.push(evidence);
    if (this.history.length > 100) this.history.shift();
    return evidence;
  }

  async findAnyOnce(locators: Locator[], timeoutMs = 30_000): Promise<string> {
    const budget = this.phaseBudget(timeoutMs, 'find any once');
    const deadline = Date.now() + Math.min(timeoutMs, budget.remainingMs);
    let lastError = 'no locator matched';
    for (const locator of locators) {
      if (budget.exhausted || Date.now() >= deadline) break;
      budget.assertAvailable(`find ${locator.using}`);
      const remaining = Math.min(deadline - Date.now(), budget.remainingMs);
      const allowance = driverCommandAllowance('/element', 'POST', locator);
      if (remaining < minimumDriverRequestMs || (this.budget && remaining < allowance)) break;
      const sliceMs = Math.min(lookupSliceMs, remaining);
      const startedAt = Date.now();
      try {
        const element = await this.findOnce(locator, sliceMs);
        this.recordLookup(locator, sliceMs, startedAt, deadline, 'matched');
        return element;
      } catch (error) {
        const retryable = isRetryableElementLookupError(error);
        this.recordLookup(locator, sliceMs, startedAt, deadline, isFatalDriverError(error) || !retryable ? 'fatal' : 'retryable', error);
        if (!retryable) throw error;
        lastError = error instanceof Error ? error.message : String(error);
        if (isCommandAdmissionError(error)) break;
      }
    }
    if (this.budget?.exhausted) this.budget.assertAvailable('find any once');
    throw new ElementLookupError(`APPIUM_ELEMENT_ANY: ${lastError}`);
  }

  async findAny(locators: Locator[], timeoutMs = 30_000): Promise<string> {
    const budget = this.phaseBudget(timeoutMs, 'find any');
    const deadline = Date.now() + Math.min(timeoutMs, budget.remainingMs);
    let lastError = 'no locator matched';
    let nextLocator = 0;
    while (locators.length > 0 && !budget.exhausted && Date.now() < deadline) {
      const locator = locators[nextLocator % locators.length];
      nextLocator += 1;
      budget.assertAvailable(`find ${locator.using}`);
      const remaining = Math.min(deadline - Date.now(), budget.remainingMs);
      const allowance = driverCommandAllowance('/element', 'POST', locator);
      if (remaining <= 1 || (this.budget && remaining < allowance)) break;
      const sliceMs = Math.min(lookupSliceMs, remaining);
      const startedAt = Date.now();
      try {
        const element = await this.findOnce(locator, sliceMs);
        this.recordLookup(locator, sliceMs, startedAt, deadline, 'matched');
        return element;
      } catch (error) {
        const retryable = isRetryableElementLookupError(error);
        this.recordLookup(locator, sliceMs, startedAt, deadline, isFatalDriverError(error) || !retryable ? 'fatal' : 'retryable', error);
        if (!retryable) throw error;
        lastError = error instanceof Error ? error.message : String(error);
        if (isCommandAdmissionError(error)) break;
      }
      if (nextLocator % locators.length === 0) {
        const waitMs = Math.min(100, deadline - Date.now(), budget.remainingMs);
        if (waitMs <= 0) break;
        await delay(waitMs);
        if (this.budget?.exhausted) this.budget.assertAvailable('find any');
      }
    }
    if (this.budget?.exhausted) this.budget.assertAvailable('find any');
    throw new ElementLookupError(`APPIUM_ELEMENT_ANY: ${lastError}`);
  }

  private recordFatal(error: unknown): void {
    if (this.firstFatal || !isFatalDriverError(error)) return;
    if (error instanceof WebDriverError) {
      this.firstFatal = {
        code: error.code,
        path: error.path,
        method: error.method,
        at: Date.now(),
        detail: redactText(error.message).slice(0, 500),
      };
      return;
    }
    this.firstFatal = {
      code: error instanceof Error && 'code' in error ? String((error as { code?: unknown }).code) : 'APPIUM_FATAL',
      path: '',
      method: '',
      at: Date.now(),
      detail: redactText(error instanceof Error ? error.message : String(error)).slice(0, 500),
    };
  }

  private recordLookup(locator: Locator, sliceMs: number, startedAt: number, operationDeadline: number, outcome: WebDriverLookupEvidence['outcome'], error?: unknown): void {
    this.lookupHistory.push({
      locator: { using: locator.using, value: redactText(locator.value).slice(0, 500) },
      sliceMs,
      startedAt,
      endedAt: Date.now(),
      remainingMs: Math.max(0, operationDeadline - Date.now()),
      outcome,
      ...(error === undefined ? {} : { error: redactText(error instanceof Error ? error.message : String(error)).slice(0, 500) }),
    });
    if (this.lookupHistory.length > 200) this.lookupHistory.shift();
  }
}

export function css(value: string): Locator {
  return { using: 'css selector', value };
}

export function textLocator(value: string): Locator {
  return { using: 'xpath', value: `//*[normalize-space(@text)=${xpathLiteral(value)} or normalize-space(.)=${xpathLiteral(value)}]` };
}

export function androidTextLocator(value: string): Locator {
  return { using: '-android uiautomator', value: `new UiSelector().text(${JSON.stringify(value)})` };
}

export function buttonText(value: string): Locator {
  return { using: 'xpath', value: `//button[normalize-space(.)=${xpathLiteral(value)}]` };
}

export function accessibility(value: string): Locator {
  return { using: 'accessibility id', value };
}

export function accessibilityPrefix(value: string): Locator {
  return { using: 'xpath', value: `//*[@aria-label and starts-with(@aria-label,${xpathLiteral(value)})]` };
}

function xpathLiteral(value: string): string {
  if (!value.includes("'")) return `'${value}'`;
  if (!value.includes('"')) return `"${value}"`;
  return `concat(${value.split("'").map((part) => `'${part}'`).join(", \"'\", ")})`;
}

export async function delay(milliseconds: number, budget?: PhaseBudget): Promise<void> {
  if (budget) {
    await budget.wait(milliseconds);
    return;
  }
  await new Promise<void>((resolve) => setTimeout(resolve, milliseconds));
}

export async function waitFor<T>(read: () => Promise<T>, ready: (value: T) => boolean, timeoutMs = 30_000, budget?: PhaseBudget): Promise<T> {
  const phase = budget || new PhaseBudget('wait', { timeoutMs, recoveryLimit: 0 });
  const deadline = Date.now() + Math.min(timeoutMs, phase.remainingMs);
  let last: T | undefined;
  while (!phase.exhausted && Date.now() < deadline) {
    phase.assertAvailable('poll');
    last = await read();
    if (ready(last)) return last;
    await delay(250, phase);
  }
  throw new Error(`APPIUM_WAIT: condition was not met before the deadline (${last === undefined ? 'no value' : 'last value observed'})`);
}
