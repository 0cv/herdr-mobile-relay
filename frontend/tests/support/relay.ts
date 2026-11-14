import type { Agent } from '$lib/types';

export class MockWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  static instances: MockWebSocket[] = [];
  readyState = MockWebSocket.CONNECTING;
  sent: string[] = [];
  onopen: (() => void) | null = null;
  onclose: ((event?: { code: number }) => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  constructor(readonly url: string, readonly protocols?: string | string[]) { MockWebSocket.instances.push(this); }
  get protocol() { return typeof this.protocols === 'string' ? this.protocols : ''; }
  send(payload: string) { this.sent.push(payload); }
  close() { this.readyState = MockWebSocket.CLOSED; }
  open() { this.readyState = MockWebSocket.OPEN; this.onopen?.(); }
  message(payload: unknown) { this.onmessage?.({ data: JSON.stringify(payload) }); }
  messages(): Record<string, any>[] { return this.sent.map(payload => JSON.parse(payload)); }
  serverClose(code?: number) {
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.(code === undefined ? undefined : { code });
  }
}

export function connectRelay(config: Record<string, unknown> = {}, socket = MockWebSocket.instances.at(-1)!): MockWebSocket {
  socket.open();
  socket.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' }, ...config });
  return socket;
}

export function makeAgent(overrides: Partial<Agent> = {}): Agent {
  const relay_id = overrides.relay_id ?? 'fedora';
  const raw_pane_id = overrides.raw_pane_id ?? 'pane-1';
  return {
    relay_id,
    relay_label: 'Fedora',
    raw_pane_id,
    pane_id: `${relay_id}::${raw_pane_id}`,
    server_session_id: 'primary',
    terminal_id: 'terminal-1',
    generation: 1,
    agent_session_id: 'session-1',
    agent: 'codex',
    ...overrides,
  };
}

export function deferred<T = void>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
