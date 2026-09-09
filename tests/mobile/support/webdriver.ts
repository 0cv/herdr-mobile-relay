import { redactText } from './diagnostics';
import { PhaseBudget } from './budget';

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

export type FetchTransport = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function isTimeoutError(error: unknown): boolean {
  if (error instanceof Error && error.name === 'TimeoutError') return true;
  if (error && typeof error === 'object' && 'name' in error && (error as { name?: unknown }).name === 'TimeoutError') return true;
  return Boolean(error && typeof error === 'object' && 'code' in error && (error as { code?: unknown }).code === 'ETIMEDOUT');
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
    const response = await this.request<{ value: Record<string, unknown>; sessionId?: string }>('/session', 'POST', {
      capabilities: {
        alwaysMatch: options.capabilities,
        firstMatch: [{}],
      },
    }, options.requestTimeoutMs || this.requestTimeoutMs, false);
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
    this.sessionId = '';
    this.unusable = false;
    await this.request(`/session/${encodeURIComponent(session)}`, 'DELETE', undefined, this.requestTimeoutMs, false).catch(() => undefined);
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

  async find(locator: Locator, timeoutMs = 30_000): Promise<string> {
    const budget = this.phaseBudget(timeoutMs, `find ${locator.using}`);
    const deadline = Date.now() + Math.min(timeoutMs, budget.remainingMs);
    let lastError = 'element not found';
    while (Date.now() < deadline) {
      budget.assertAvailable(`find ${locator.using}`);
      try {
        const requestTimeoutMs = Math.max(1, Math.min(deadline - Date.now(), budget.remainingMs));
        const value = await this.command<Record<string, string>>('/element', 'POST', locator, requestTimeoutMs);
        const element = value['element-6066-11e4-a52e-4f735466cecf'] || value.ELEMENT;
        if (element) return element;
        lastError = 'element response did not contain an id';
      } catch (error) {
        if (error instanceof WebDriverError && (error.timedOut || error.code === 'APPIUM_SESSION_UNUSABLE')) throw error;
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250, budget);
    }
    throw new Error(`APPIUM_ELEMENT: ${locator.using}=${redactText(locator.value)}: ${lastError}`);
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
      if (error instanceof WebDriverError && error.timedOut && path !== '/status') this.unusable = true;
      throw error;
    }
  }

  private phaseBudget(timeoutMs: number, operation: string): PhaseBudget {
    return this.budget || new PhaseBudget(operation, { timeoutMs, recoveryLimit: 0 });
  }

  private assertUsable(path: string): void {
    if (this.unusable && path !== '/session' && !path.endsWith('/status')) {
      throw new WebDriverError({
        code: 'APPIUM_SESSION_UNUSABLE',
        message: 'the previous command timed out; session replacement is required',
        path,
        method: 'COMMAND',
        durationMs: 0,
        selectedContext: this.selectedContext,
        selectedWindow: this.selectedWindow,
      });
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
  ): Promise<WebDriverResponse<T>> {
    if (checkSession) this.assertUsable(path);
    const operation = `${method} ${path}`;
    this.budget?.assertAvailable(operation);
    const operationTimeoutMs = timeoutMs ?? this.requestTimeoutMs;
    const requestTimeoutMs = Math.max(1, Math.min(operationTimeoutMs, this.budget?.remainingMs ?? operationTimeoutMs));
    const startedAt = Date.now();
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
      const timedOut = isTimeoutError(error) || controller.signal.aborted || this.budget?.exhausted === true;
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

  async findAny(locators: Locator[], timeoutMs = 30_000): Promise<string> {
    const budget = this.phaseBudget(timeoutMs, 'find any');
    const deadline = Date.now() + Math.min(timeoutMs, budget.remainingMs);
    let lastError = '';
    while (!budget.exhausted && Date.now() < deadline) {
      for (const locator of locators) {
        budget.assertAvailable(`find ${locator.using}`);
        try {
          const remaining = Math.max(1, Math.min(deadline - Date.now(), budget.remainingMs));
          return await this.find(locator, Math.min(750, remaining));
        } catch (error) {
          if (error instanceof WebDriverError && (error.timedOut || error.code === 'APPIUM_SESSION_UNUSABLE')) throw error;
          lastError = error instanceof Error ? error.message : String(error);
        }
      }
      await delay(100, budget);
    }
    throw new Error(`APPIUM_ELEMENT_ANY: ${lastError}`);
  }
}

export function css(value: string): Locator {
  return { using: 'css selector', value };
}

export function textLocator(value: string): Locator {
  return { using: 'xpath', value: `//*[normalize-space(@text)=${xpathLiteral(value)} or normalize-space(.)=${xpathLiteral(value)}]` };
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
