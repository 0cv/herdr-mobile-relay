import { redactText } from './diagnostics';

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
}

export class AppiumClient {
  private sessionId = '';
  private readonly baseUrl: string;
  private readonly requestTimeoutMs: number;

  constructor(baseUrl = 'http://127.0.0.1:4723', requestTimeoutMs = 30_000) {
    this.baseUrl = baseUrl.replace(/\/$/, '');
    this.requestTimeoutMs = requestTimeoutMs;
  }

  async create(options: SessionOptions): Promise<Record<string, unknown>> {
    const response = await this.request<{ value: Record<string, unknown>; sessionId?: string }>('/session', 'POST', {
      capabilities: {
        alwaysMatch: options.capabilities,
        firstMatch: [{}],
      },
    }, options.requestTimeoutMs || this.requestTimeoutMs);
    const value = response.value as unknown as WebDriverResponse<Record<string, unknown>>;
    this.sessionId = String(response.sessionId || (value as any)?.sessionId || '');
    const capabilities = (value as any)?.value || value;
    if (!this.sessionId) throw new Error('APPIUM_SESSION: server did not return a session id');
    return capabilities as Record<string, unknown>;
  }

  async close(): Promise<void> {
    if (!this.sessionId) return;
    const session = this.sessionId;
    this.sessionId = '';
    await this.request(`/session/${encodeURIComponent(session)}`, 'DELETE').catch(() => undefined);
  }

  async contexts(): Promise<string[]> {
    return this.command<string[]>('/contexts', 'GET');
  }

  async switchContext(name: string): Promise<void> {
    await this.command('/context', 'POST', { name });
  }

  async currentUrl(): Promise<string> {
    return this.command<string>('/url', 'GET');
  }

  async navigate(url: string): Promise<void> {
    await this.command('/url', 'POST', { url });
  }

  async pageSource(): Promise<string> {
    return this.command<string>('/source', 'GET');
  }

  async find(locator: Locator, timeoutMs = 30_000): Promise<string> {
    const deadline = Date.now() + timeoutMs;
    let lastError = 'element not found';
    while (Date.now() < deadline) {
      try {
        const value = await this.command<Record<string, string>>('/element', 'POST', locator);
        const element = value['element-6066-11e4-a52e-4f735466cecf'] || value.ELEMENT;
        if (element) return element;
        lastError = 'element response did not contain an id';
      } catch (error) {
        lastError = error instanceof Error ? error.message : String(error);
      }
      await delay(250);
    }
    throw new Error(`APPIUM_ELEMENT: ${locator.using}=${locator.value}: ${lastError}`);
  }

  async findAll(locator: Locator): Promise<string[]> {
    const values = await this.command<Record<string, string>[]>('/elements', 'POST', locator);
    return values.map((value) => value['element-6066-11e4-a52e-4f735466cecf'] || value.ELEMENT).filter(Boolean);
  }

  async click(element: string): Promise<void> {
    await this.command(`/element/${encodeURIComponent(element)}/click`, 'POST');
  }

  async sendKeys(element: string, text: string): Promise<void> {
    await this.command(`/element/${encodeURIComponent(element)}/value`, 'POST', {
      text,
      value: [...text],
    });
  }

  async text(element: string): Promise<string> {
    return this.command<string>(`/element/${encodeURIComponent(element)}/text`, 'GET');
  }

  async attribute(element: string, name: string): Promise<string | null> {
    return this.command<string | null>(`/element/${encodeURIComponent(element)}/attribute/${encodeURIComponent(name)}`, 'GET');
  }

  async execute<T = unknown>(script: string, args: unknown[] = []): Promise<T> {
    return this.command<T>('/execute/sync', 'POST', { script, args });
  }

  async screenshot(): Promise<string> {
    return this.command<string>('/screenshot', 'GET');
  }

  async back(): Promise<void> {
    await this.command('/back', 'POST');
  }

  async performActions(actions: unknown[]): Promise<void> {
    await this.command('/actions', 'POST', { actions });
  }

  async mobile(command: string, args: Record<string, unknown> = {}): Promise<unknown> {
    return this.command('/execute/sync', 'POST', { script: `mobile: ${command}`, args });
  }

  async command<T = unknown>(path: string, method: string, body?: unknown): Promise<T> {
    return this.request<T>(this.sessionPath(path), method, body).then((response) => response.value as T);
  }

  async findAny(locators: Locator[], timeoutMs = 30_000): Promise<string> {
    const deadline = Date.now() + timeoutMs;
    let lastError = '';
    while (Date.now() < deadline) {
      for (const locator of locators) {
        try {
          return await this.find(locator, Math.min(750, Math.max(1, deadline - Date.now())));
        } catch (error) {
          lastError = error instanceof Error ? error.message : String(error);
        }
      }
      await delay(100);
    }
    throw new Error(`APPIUM_ELEMENT_ANY: ${lastError}`);
  }

  private sessionPath(path: string): string {
    if (!this.sessionId) throw new Error('APPIUM_SESSION: no active session');
    return `/session/${encodeURIComponent(this.sessionId)}${path}`;
  }

  private async request<T>(path: string, method: string, body?: unknown, timeoutMs = this.requestTimeoutMs): Promise<WebDriverResponse<T>> {
    const response = await fetch(`${this.baseUrl}${path}`, {
      method,
      headers: body === undefined ? undefined : { 'content-type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(timeoutMs),
    });
    const text = await response.text();
    let parsed: WebDriverResponse<T>;
    try {
      parsed = JSON.parse(text) as WebDriverResponse<T>;
    } catch {
      throw new Error(`APPIUM_HTTP: HTTP ${response.status}`);
    }
    if (!response.ok || (parsed as any).value?.error) {
      const detail = typeof (parsed as any).value === 'object'
        ? JSON.stringify((parsed as any).value)
        : String((parsed as any).value || text);
      throw new Error(`APPIUM_COMMAND: HTTP ${response.status}: ${redactText(detail).slice(0, 500)}`);
    }
    return parsed;
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

export async function delay(milliseconds: number): Promise<void> {
  await new Promise<void>((resolve) => setTimeout(resolve, milliseconds));
}

export async function waitFor<T>(read: () => Promise<T>, ready: (value: T) => boolean, timeoutMs = 30_000): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  let last: T;
  while (Date.now() < deadline) {
    last = await read();
    if (ready(last)) return last;
    await delay(250);
  }
  throw new Error('APPIUM_WAIT: condition was not met before the deadline');
}
