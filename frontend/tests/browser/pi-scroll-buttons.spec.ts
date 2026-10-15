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
    capabilities: ['attention_classification', 'pi_scroll_buttons', 'conversation_history'],
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
      cwd: '/tmp', tab_label: 'Pi', status: 'idle', project: 'Pi scroll fixture', agent: 'pi', conversation_history_available: true,
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
    expect(Object.keys(sent).sort()).toEqual(['client_id', 'direction', 'pane_id', 'protocol', 'request_id', 'server_session_id', 'state_revision', 'target', 'type']);
    expect(typeof sent.request_id).toBe('string');
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
    await expect(up).toBeEnabled();
    await expect(down).toBeEnabled();
    await page.evaluate((state) => {
      const buttons = [...document.querySelectorAll('button')];
      const up = buttons.find(button => button.textContent === 'Scroll agent up')!;
      const down = buttons.find(button => button.textContent === 'Scroll agent down')!;
      up.click(); down.click(); up.click();
      const sent = (window as any).__relayCommands.filter((message: any) => message.type === 'scroll_agent');
      if (sent.length !== 1) throw new Error('unbounded button dispatch');
      (window as any).__relayServer({ type: 'command_result', action: 'scroll_agent', request_id: sent[0].request_id, ok: true, data: { scrolling: state } });
    }, scrollState);
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

test('Local selection cancels pending input and Automatic restores the live screen', async ({ page }) => {
  await connect(page);
  await page.getByRole('button', { name: 'Scroll agent up' }).click();
  await page.getByRole('button', { name: 'Scroll agent down' }).click();
  const active = (await scrollCommands(page))[0];
  await page.getByRole('combobox', { name: 'Pi scroll selection' }).selectOption('local');
  const selection = (await commands(page)).find(message => message.type === 'set_scroll_selection')!;
  expect(selection).toMatchObject({ selection: 'local', state_revision: scrollRevision });
  await server(page, {
    type: 'command_result', action: 'set_scroll_selection', request_id: selection.request_id, ok: true,
    data: { scrolling: { ...scrollState, selection: 'local', route: 'local', available: false, reason: 'local_view', revision: 'b'.repeat(64) } },
  });
  await expect(page.getByRole('button', { name: 'Scroll agent up' })).toBeDisabled();
  await confirmScroll(page, active);
  await expect(page.getByRole('combobox', { name: 'Pi scroll selection' })).toHaveValue('local');
  expect(await scrollCommands(page)).toHaveLength(1);
  await page.getByRole('combobox', { name: 'Pi scroll selection' }).selectOption('auto');
  const automatic = (await commands(page)).filter(message => message.type === 'set_scroll_selection').at(-1)!;
  expect(automatic).toMatchObject({ selection: 'auto', state_revision: 'b'.repeat(64) });
  await server(page, { type: 'command_result', action: 'set_scroll_selection', request_id: automatic.request_id, ok: true, data: { scrolling: { ...scrollState, revision: 'c'.repeat(64) } } });
  await expect(page.getByRole('button', { name: 'Scroll agent up' })).toBeEnabled();
  expect(await scrollCommands(page)).toHaveLength(1);
});

test('a stale refusal publishes current state without replaying the pending click', async ({ page }) => {
  await connect(page);
  await page.getByRole('button', { name: 'Scroll agent up' }).click();
  await page.getByRole('button', { name: 'Scroll agent down' }).click();
  const active = (await scrollCommands(page))[0];
  await server(page, {
    type: 'command_result', action: 'scroll_agent', request_id: active.request_id, ok: false, phase: 'not_started',
    error: 'Scroll state changed', data: { scrolling: { ...scrollState, rows: 22, geometry_epoch: 2, revision: 'b'.repeat(64), reason: 'Scroll state changed' } },
  });
  await expect(page.getByText('Pi runtime · Scroll state changed')).toBeVisible();
  expect(await scrollCommands(page)).toHaveLength(1);
  await page.getByRole('button', { name: 'Scroll agent down' }).click();
  await expect.poll(async () => (await scrollCommands(page)).length).toBe(2);
  const fresh = (await scrollCommands(page))[1];
  expect(fresh.state_revision).toBe('b'.repeat(64));
  await server(page, { type: 'command_result', action: 'scroll_agent', request_id: fresh.request_id, ok: true, data: { scrolling: { ...scrollState, revision: 'b'.repeat(64) } } });
});

test('disconnect cancels pending input and disables mutations', async ({ page }) => {
  await connect(page);
  await page.getByRole('button', { name: 'Scroll agent up' }).click();
  await page.getByRole('button', { name: 'Scroll agent down' }).click();
  const active = (await scrollCommands(page))[0];
  await page.evaluate(() => {
    const socket = (window as any).__relaySockets[0];
    socket.readyState = 3;
    socket.onclose?.();
  });
  await expect(page.getByRole('button', { name: 'Scroll agent up' })).toBeDisabled();
  await confirmScroll(page, active);
  expect(await scrollCommands(page)).toHaveLength(1);
});

test('preserves prior local history across redraws while DOM and jump scrolling send no input', async ({ page }) => {
  await connect(page);
  const prior = Array.from({ length: 600 }, (_, i) => `Genuine history row ${i + 1}`).join('\n');
  await server(page, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: prior,
    scrolling: { ...scrollState, renderer: 'regular', wheel: 'unavailable', route: 'local', presentation: 'history', available: false },
  });
  await server(page, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'Live redraw one', scrolling: scrollState });
  await server(page, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'Live redraw two', scrolling: scrollState });
  await page.getByRole('combobox', { name: 'Pi scroll selection' }).selectOption('local');
  const selection = (await commands(page)).find(message => message.type === 'set_scroll_selection')!;
  await server(page, {
    type: 'command_result', action: 'set_scroll_selection', request_id: selection.request_id, ok: true,
    data: { scrolling: { ...scrollState, selection: 'local', route: 'local', available: false, revision: 'b'.repeat(64) } },
  });
  await expect(page.getByText('Prior local history · retained snapshot')).toBeVisible();
  const log = page.getByRole('log', { name: 'Agent terminal output' });
  await expect(log).toContainText('Genuine history row 600');
  await expect(log).not.toContainText('Live redraw');
  await log.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toBeVisible();
  await page.getByRole('button', { name: 'Jump to latest' }).click();
  await expect(log).toContainText('Genuine history row 600');
  expect(await scrollCommands(page)).toHaveLength(0);
});

test('Conversation History is independent of application scroll buttons', async ({ page }) => {
  await connect(page);
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect.poll(async () => (await commands(page)).filter(message => message.type === 'get_conversation_history').length).toBe(1);
  const request = (await commands(page)).find(message => message.type === 'get_conversation_history')!;
  await server(page, {
    type: 'command_result', action: 'get_conversation_history', request_id: request.request_id, ok: true,
    data: { available: true, state: 'ready', mode: 'recent', source_revision: 'fixture', snapshot_id: '', next_cursor: '', entries: [{ id: 'fixture-message', timestamp: '2026-10-15T00:00:00Z', role: 'user', text: 'Offline retained conversation' }], total: 1, has_more: false },
  });
  await expect(page.getByText('Offline retained conversation')).toBeVisible();
  expect(await scrollCommands(page)).toHaveLength(0);
});
