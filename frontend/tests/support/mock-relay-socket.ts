import type { Page } from '@playwright/test';

export type MockRelayOptions = {
  capabilities: string[];
  workspaces?: Record<string, unknown>[];
  agents?: Record<string, unknown>[];
};

export type MockRelayCommandHandler = (command: Record<string, any>, socket: { server(message: unknown): void }) => void;

declare global {
  interface Window {
    mockRelay: { commands: Record<string, any>[]; server(message: unknown): void };
  }
}

function installInPage(options: MockRelayOptions, onCommand?: MockRelayCommandHandler) {
  localStorage.setItem('herdr_relays', JSON.stringify([{ id: 'test', label: 'Test', url: 'ws://127.0.0.1:8871', token: '' }]));
  class MockRelaySocket {
    static OPEN = 1;
    static CONNECTING = 0;
    static CLOSED = 3;
    readyState = 0;
    commands: Record<string, any>[] = [];
    onopen: (() => void) | null = null;
    onmessage: ((event: MessageEvent) => void) | null = null;
    constructor(url: string) {
      if (new URL(url).href !== 'ws://127.0.0.1:8871/') throw new Error('Unexpected mock transport');
      window.mockRelay = this;
      queueMicrotask(() => {
        this.readyState = 1;
        this.onopen?.();
        this.server({ type: 'push_config', protocol: 3, host: 'test', capabilities: options.capabilities });
        if (options.workspaces) this.server({ type: 'workspaces', workspaces: options.workspaces });
        if (options.agents) this.server({ type: 'agents', agents: options.agents });
      });
    }
    server(message: unknown) { this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(message) })); }
    send(payload: string) {
      const command = JSON.parse(payload);
      this.commands.push(command);
      onCommand?.(command, this);
    }
    close() { this.readyState = 3; }
  }
  Object.defineProperty(window, 'WebSocket', { value: MockRelaySocket });
}

export async function installMockRelay(page: Page, options: MockRelayOptions, onCommand?: MockRelayCommandHandler) {
  await page.addInitScript({ content: `(${installInPage})(${JSON.stringify(options)}, ${onCommand ?? 'undefined'})` });
}

export const mockRelayCommands = (page: Page) => page.evaluate(() => window.mockRelay.commands);

export const mockRelayServer = (page: Page, message: unknown) =>
  page.evaluate(message => window.mockRelay.server(message), message);
