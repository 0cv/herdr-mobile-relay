import { expect, test, type Page } from '@playwright/test';

const relay = { id: 'fixture', label: 'Fixture', url: 'ws://fixture.invalid/ws', token: '' };
const scrollRevision = 'a'.repeat(64);
const scrollState = {
  selection: 'auto', renderer: 'fullscreen', wheel: 'sgr_cell', source: 'pi_runtime',
  route: 'application', presentation: 'live_screen', available: true, reason: 'pi_fullscreen',
  rows: 30, columns: 80, geometry_epoch: 1, revision: scrollRevision,
};

async function server(page: Page, payload: Record<string, unknown>) {
  await page.evaluate((message) => (window as any).__relayServer(message), payload);
}

async function commands(page: Page): Promise<Record<string, unknown>[]> {
  return page.evaluate(() => (window as any).__relayCommands);
}

async function setRole(page: Page, role: 'controller' | 'reader') {
  await page.evaluate((nextRole) => {
    localStorage.setItem('herdr_device_auth_v1', JSON.stringify({
      version: 1,
      relays: {
        fixture: {
          kind: 'credential', id: `credential-${nextRole}`, version: 1,
          secret: 'R'.repeat(43), deviceId: `device-${nextRole}`, role: nextRole,
          locale: 'en', issuedAt: Date.now(),
        },
      },
    }));
  }, role);
}

async function connect(page: Page, role: 'controller' | 'reader' = 'controller') {
  await page.addInitScript((savedRelay) => {
    localStorage.setItem('herdr_relays', JSON.stringify([savedRelay]));
    const sockets: MockSocket[] = [];
    const received: Record<string, unknown>[] = [];

    class MockSocket {
      static CONNECTING = 0;
      static OPEN = 1;
      static CLOSING = 2;
      static CLOSED = 3;
      readyState = MockSocket.CONNECTING;
      onopen: (() => void) | null = null;
      onclose: (() => void) | null = null;
      onerror: (() => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;

      constructor(readonly url: string) {
        sockets.push(this);
        queueMicrotask(() => {
          this.readyState = MockSocket.OPEN;
          this.onopen?.();
        });
      }

      send(serialized: string) {
        received.push(JSON.parse(serialized) as Record<string, unknown>);
      }

      close() {
        this.readyState = MockSocket.CLOSED;
      }

      server(message: Record<string, unknown>) {
        if (message.type === 'agents' && Array.isArray(message.agents)) {
          message = {
            ...message,
            agents: (message.agents as Record<string, unknown>[]).map((agent) => ({
              server_session_id: 'primary', terminal_id: 'fixture-terminal', generation: 1,
              agent_session_id: 'pi-session', ...agent,
            })),
          };
        }
        this.onmessage?.({ data: JSON.stringify(message) } as MessageEvent);
      }
    }

    Object.defineProperty(window, 'WebSocket', { configurable: true, value: MockSocket });
    Object.assign(window, {
      __relaySockets: sockets,
      __relayCommands: received,
      __relayServer(message: Record<string, unknown>) { sockets[0]?.server(message); },
    });
  }, relay);
  await page.goto('/');
  await expect.poll(() => page.evaluate(() => (window as any).__relaySockets.length)).toBe(1);
  const handshake = () => server(page, {
    type: 'push_config', protocol: 3, version: 'fixture', host: 'Fixture', home: '/tmp',
    capabilities: ['attention_classification'],
  });
  await handshake();
  await setRole(page, role);
  await handshake();
  await server(page, {
    type: 'workspaces',
    workspaces: [{ workspace_id: 'w1', number: 1, label: 'Fixture project', pane_count: 1, tab_count: 1, cwd: '/tmp' }],
  });
  await server(page, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', workspace_id: 'w1', tab_id: 'w1:t1', tab_number: 1,
      cwd: '/tmp', tab_label: 'Pi', status: 'idle', project: 'Pi scroll fixture', agent: 'pi',
    }],
  });
  await page.getByRole('button', { name: 'Open Pi scroll fixture on Fixture' }).click();
  await server(page, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'Disposable Pi screen', scrolling: scrollState,
  });
  await expect(page.getByRole('main', { name: 'Terminal for Pi scroll fixture' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Scroll agent up' })).toBeVisible();
}

async function scrollCommands(page: Page) {
  return (await commands(page)).filter((message) => message.type === 'scroll_agent');
}

async function confirmScroll(page: Page, message: Record<string, unknown>) {
  await server(page, {
    type: 'command_result', action: 'scroll_agent', request_id: message.request_id,
    ok: true, phase: 'completed', data: { scrolling: scrollState },
  });
}

test.describe('Pi scroll buttons', () => {
  test('sends exact-target up and down intents for a confirmed live Pi fullscreen route', async ({ page }) => {
    await connect(page);
    const up = page.getByRole('button', { name: 'Scroll agent up' });
    const down = page.getByRole('button', { name: 'Scroll agent down' });
    await expect(up).toBeEnabled();
    await expect(down).toBeEnabled();

    await up.click();
    await expect.poll(async () => (await scrollCommands(page)).length).toBe(1);
    let sent = (await scrollCommands(page))[0];
    expect(sent).toMatchObject({
      type: 'scroll_agent', direction: 'up', pane_id: 'w1:p1', state_revision: scrollRevision,
      target: {
        server_session_id: 'primary', pane_id: 'w1:p1', terminal_id: 'fixture-terminal',
        generation: 1, agent_session_id: 'pi-session',
      },
    });
    await confirmScroll(page, sent);

    await down.click();
    await expect.poll(async () => (await scrollCommands(page)).length).toBe(2);
    sent = (await scrollCommands(page))[1];
    expect(sent).toMatchObject({ type: 'scroll_agent', direction: 'down', state_revision: scrollRevision });
    await confirmScroll(page, sent);
  });

  test('keeps only the newest pending button intent until the active request is confirmed', async ({ page }) => {
    await connect(page);
    const up = page.getByRole('button', { name: 'Scroll agent up' });
    const down = page.getByRole('button', { name: 'Scroll agent down' });
    await up.click();
    await expect.poll(async () => (await scrollCommands(page)).length).toBe(1);
    await down.click();
    await up.click();
    expect(await scrollCommands(page)).toHaveLength(1);

    await confirmScroll(page, (await scrollCommands(page))[0]);
    await expect.poll(async () => (await scrollCommands(page)).length).toBe(2);
    const newest = (await scrollCommands(page))[1];
    expect(newest).toMatchObject({ type: 'scroll_agent', direction: 'up', state_revision: scrollRevision });
    await confirmScroll(page, newest);
  });

  test('keeps Pi scroll buttons read-only for a paired reader', async ({ page }) => {
    await connect(page, 'reader');
    const up = page.getByRole('button', { name: 'Scroll agent up' });
    const down = page.getByRole('button', { name: 'Scroll agent down' });
    const selection = page.getByRole('combobox', { name: 'Pi scroll selection' });
    await expect(up).toBeDisabled();
    await expect(down).toBeDisabled();
    await expect(selection).toBeDisabled();
    await expect(page.getByText('Reader access is read only')).toBeVisible();
    expect(await scrollCommands(page)).toHaveLength(0);
  });

  test('does not enable buttons when Pi runtime scrolling is unavailable', async ({ page }) => {
    await connect(page);
    await server(page, {
      type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'Regular terminal',
      scrolling: {
        ...scrollState, renderer: 'regular', wheel: 'unavailable', source: 'unknown',
        route: 'local', presentation: 'history', available: false, reason: 'regular_terminal',
      },
    });
    await expect(page.getByRole('button', { name: 'Scroll agent up' })).toBeDisabled();
    await expect(page.getByRole('button', { name: 'Scroll agent down' })).toBeDisabled();
    expect(await scrollCommands(page)).toHaveLength(0);
  });
});
