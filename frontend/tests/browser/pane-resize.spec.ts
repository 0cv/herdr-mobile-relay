import { expect, test, type Page } from '@playwright/test';
import { boot, closeSocket, commands, commandsForSocket, fedora, handshake, server, setAutoCommands, socketCount } from '../support/journey-relay';

const optInCapabilities = ['attention_classification', 'pane_size_opt_in', 'pane_size_opt_in_rows', 'slash_commands'];

async function leaseCommands(page: Page) {
  return (await commands(page)).filter((command) => command.type === 'lease_pane_size');
}

async function releaseCommands(page: Page) {
  return (await commands(page)).filter((command) => command.type === 'release_pane_size');
}

async function openSizedPane(page: Page, project: string, capabilities = optInCapabilities) {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project, agent: 'omp' }],
  });
  await page.getByRole('button', { name: `Open ${project} on Fedora` }).click();
  await showWidePane(page, 0);
}

async function showWidePane(page: Page, socket: number) {
  const read = (await commands(page)).findLast(command => command.type === 'read_pane');
  await server(page, socket, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    request_id: read?.request_id, target: read?.target,
    content: ['resize session baseline', `${'desktop width output '.repeat(12)}end`].join('\n'),
  });
  await expect(page.getByRole('log')).toContainText('resize session baseline');
}

async function settingsFromTerminal(page: Page) {
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('navigation', { name: 'Application' })
    .getByRole('button', { name: /^Settings(?:,|$)/ }).click();
}

async function expectDesktopWidthLayout(page: Page) {
  const terminal = page.getByRole('log');
  await expect(terminal).not.toHaveClass(/resize-(layout|pending)/);
  await expect.poll(() => terminal.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(true);
}

test('followup defaults: Resize Session starts on with height off', async ({ page }) => {
  await openSizedPane(page, 'Default on');
  await expect.poll(async () => (await leaseCommands(page)).length).toBe(1);
  expect((await leaseCommands(page))[0]).not.toHaveProperty('rows');
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_resize_session'))).toBeNull();
  await settingsFromTerminal(page);
  await expect(page.getByRole('switch', { name: 'Resize Session', exact: true })).toBeChecked();
  await expect(page.getByRole('switch', { name: 'Lease Terminal Height', exact: true })).not.toBeChecked();
});

test('followup defaults: explicitly saved Resize Session off stays off', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'false'));
  await openSizedPane(page, 'Default off');
  await page.clock.runFor(300);
  expect(await leaseCommands(page)).toHaveLength(0);
  await expectDesktopWidthLayout(page);
  await expect(page.getByRole('alert')).toHaveCount(0);

  await settingsFromTerminal(page);
  await expect(page.getByRole('switch', { name: 'Resize Session', exact: true })).not.toBeChecked();
  await expect(page.getByRole('switch', { name: 'Lease Terminal Height', exact: true })).toBeDisabled();
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_resize_session'))).toBe('false');
  expect(await releaseCommands(page)).toHaveLength(0);
});

test('never leases on the legacy capability alone, even with consent saved', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => {
    localStorage.setItem('herdr_terminal_resize_session', 'true');
    localStorage.setItem('herdr_terminal_height_lease', 'true');
  });
  await openSizedPane(page, 'Legacy relay', ['attention_classification', 'pane_size_lease', 'pane_size_lease_rows', 'slash_commands']);
  await page.clock.runFor(500);
  expect(await leaseCommands(page)).toHaveLength(0);
  await expectDesktopWidthLayout(page);
  await expect(page.getByRole('alert')).toHaveCount(0);
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toBeEnabled();

  await settingsFromTerminal(page);
  await expect(page.getByRole('switch', { name: 'Resize Session', exact: true })).toBeChecked();
  const height = page.getByRole('switch', { name: 'Lease Terminal Height', exact: true });
  await expect(height).toBeChecked();
  await expect(height).toBeDisabled();
  expect(await releaseCommands(page)).toHaveLength(0);
});

test('opts in, leases with opt_in, and releases immediately when turned off in Settings', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'false'));
  await openSizedPane(page, 'Opt-in pane');
  expect(await leaseCommands(page)).toHaveLength(0);
  await settingsFromTerminal(page);
  const resize = page.getByRole('switch', { name: 'Resize Session', exact: true });
  await resize.click();
  await expect(resize).toBeChecked();
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_resize_session'))).toBe('true');
  await expect(page.getByRole('switch', { name: 'Lease Terminal Height', exact: true })).toBeEnabled();
  expect(await leaseCommands(page)).toHaveLength(0);

  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await page.getByRole('button', { name: 'Open Opt-in pane on Fedora' }).click();
  await expect.poll(async () => (await leaseCommands(page)).length).toBe(1);
  const [lease] = await leaseCommands(page);
  expect(lease).toMatchObject({ type: 'lease_pane_size', pane_id: 'w1:p1', protocol: 3, opt_in: true });
  expect(lease).not.toHaveProperty('rows');
  expect(lease).not.toHaveProperty('client_id');
  expect(Number(lease.columns)).toBeGreaterThanOrEqual(40);
  expect(Number(lease.columns)).toBeLessThanOrEqual(240);
  await showWidePane(page, 0);
  const terminal = page.getByRole('log');
  await expect(terminal).toHaveClass(/resize-layout/);
  await expect.poll(() => terminal.evaluate((element) => element.scrollWidth <= element.clientWidth + 1)).toBe(true);

  // Leaving the pane keeps the relay's short navigation grace...
  await settingsFromTerminal(page);
  await expect.poll(async () => (await releaseCommands(page)).length).toBe(1);
  expect((await releaseCommands(page))[0]).not.toHaveProperty('immediate');
  // ...which turning the switch off ends at once, with no terminal open.
  await resize.click();
  await expect(resize).not.toBeChecked();
  await expect.poll(async () => (await releaseCommands(page)).length).toBe(2);
  expect((await releaseCommands(page))[1]).toMatchObject({ type: 'release_pane_size', pane_id: 'w1:p1', immediate: true });
  await expect(page.getByRole('switch', { name: 'Lease Terminal Height', exact: true })).toBeDisabled();

  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await page.getByRole('button', { name: 'Open Opt-in pane on Fedora' }).click();
  await showWidePane(page, 0);
  await page.clock.runFor(300);
  expect(await leaseCommands(page)).toHaveLength(1);
  await expectDesktopWidthLayout(page);
  await expect(page.getByRole('alert')).toHaveCount(0);
});

test('leases the height only when the relay supports it and the height switch is on', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_terminal_resize_session', 'true');
    localStorage.setItem('herdr_terminal_height_lease', 'true');
  });
  await openSizedPane(page, 'Height pane', ['attention_classification', 'pane_size_opt_in', 'slash_commands']);
  await expect.poll(async () => (await leaseCommands(page)).length).toBeGreaterThan(0);
  expect((await leaseCommands(page))[0]).not.toHaveProperty('rows');

  await settingsFromTerminal(page);
  const height = page.getByRole('switch', { name: 'Lease Terminal Height', exact: true });
  await expect(height).toBeChecked();
  await expect(height).toBeDisabled();
  await handshake(page, 0, { capabilities: optInCapabilities });
  await expect(height).toBeEnabled();

  await page.getByRole('button', { name: 'Back', exact: true }).click();
  const beforeRows = (await leaseCommands(page)).length;
  await page.getByRole('button', { name: 'Open Height pane on Fedora' }).click();
  await expect.poll(async () => (await leaseCommands(page)).length).toBeGreaterThan(beforeRows);
  const withRows = (await leaseCommands(page)).at(-1)!;
  expect(withRows).toMatchObject({ opt_in: true });
  expect(Number(withRows.rows)).toBeGreaterThanOrEqual(10);
  expect(Number(withRows.rows)).toBeLessThanOrEqual(120);

  await settingsFromTerminal(page);
  await height.click();
  await expect(height).not.toBeChecked();
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  const beforeWidthOnly = (await leaseCommands(page)).length;
  await page.getByRole('button', { name: 'Open Height pane on Fedora' }).click();
  await expect.poll(async () => (await leaseCommands(page)).length).toBeGreaterThan(beforeWidthOnly);
  expect((await leaseCommands(page)).at(-1)).not.toHaveProperty('rows');
});

test('ignores a lease reply that lands after Resize Session was turned off', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'true'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: optInCapabilities });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Late reply', agent: 'omp' }],
  });
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Late reply on Fedora' }).click();
  await showWidePane(page, 0);
  await expect.poll(async () => (await leaseCommands(page)).length).toBe(1);
  const [lease] = await leaseCommands(page);

  await settingsFromTerminal(page);
  const resize = page.getByRole('switch', { name: 'Resize Session', exact: true });
  await resize.click();
  await expect(resize).not.toBeChecked();
  await expect.poll(async () => (await releaseCommands(page))
    .some((command) => command.immediate === true && command.pane_id === 'w1:p1')).toBe(true);
  await server(page, 0, {
    type: 'command_result', action: 'lease_pane_size', request_id: lease.request_id,
    ok: true, phase: 'completed', data: { columns: lease.columns, rows: 46 },
  });

  await setAutoCommands(page, true);
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await page.getByRole('button', { name: 'Open Late reply on Fedora' }).click();
  await showWidePane(page, 0);
  await page.clock.runFor(300);
  expect(await leaseCommands(page)).toHaveLength(1);
  await expectDesktopWidthLayout(page);
  await expect(page.getByRole('alert')).toHaveCount(0);
});

test('stays unleased across a reconnect after Resize Session is turned off', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'true'));
  await openSizedPane(page, 'Reconnect pane');
  await expect.poll(async () => (await leaseCommands(page)).length).toBe(1);
  await settingsFromTerminal(page);
  await page.getByRole('switch', { name: 'Resize Session', exact: true }).click();
  await expect.poll(async () => (await releaseCommands(page))
    .some((command) => command.immediate === true)).toBe(true);
  await page.getByRole('button', { name: 'Back', exact: true }).click();

  await closeSocket(page, 0);
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 1, { capabilities: optInCapabilities });
  await server(page, 1, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Reconnect pane', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Reconnect pane on Fedora' }).click();
  await showWidePane(page, 1);
  await page.clock.runFor(500);
  expect((await commandsForSocket(page, 1)).filter((command) => command.type === 'lease_pane_size')).toHaveLength(0);
  await expectDesktopWidthLayout(page);
});

test('keeps renewing an opted-in lease briefly while hidden and re-leases on return', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'true'));
  await openSizedPane(page, 'Hidden pane', ['attention_classification', 'pane_size_opt_in', 'pane_realtime_delta']);
  await expect.poll(async () => (await leaseCommands(page)).length).toBe(1);

  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  // Desktop Safari reports an occluded window as hidden, so a hidden page
  // keeps renewing within the grace window instead of lapsing the lease on
  // every app switch. Grace expiry is unit-tested on the pure policy helper.
  await page.clock.runFor(11_000);
  await expect.poll(async () => (await leaseCommands(page)).length).toBeGreaterThanOrEqual(2);
  expect(await releaseCommands(page)).toHaveLength(0);

  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(async () => (await leaseCommands(page)).length).toBeGreaterThanOrEqual(3);
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThanOrEqual(2);
});

test('keeps pane resizing off through hidden timers and refocus', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'false'));
  await openSizedPane(page, 'Sleeping pane', ['attention_classification', 'pane_size_opt_in', 'pane_realtime_delta']);
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  // Cross the ten-second renewal cadence. Off means no timer acquisition.
  await page.clock.runFor(11_000);
  expect(await leaseCommands(page)).toHaveLength(0);
  expect(await releaseCommands(page)).toHaveLength(0);
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'read_pane').length).toBeGreaterThanOrEqual(2);
  expect(await leaseCommands(page)).toHaveLength(0);
});

test('estimates fixed row heights for a controller pane', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Wide pane app', agent: 'omp' }],
  });
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Wide pane app on Fedora' }).click();
  // Desktop-width rows remain single lines on a controller pane. The
  // virtualizer must estimate that fixed-grid geometry correctly.
  const content = Array.from({ length: 400 }, (_, index) =>
    `row ${String(index + 1).padStart(4, '0')} ${'wide content '.repeat(14)}`).join('\n');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content });

  const terminal = page.getByRole('log');
  await expect(terminal.locator('.ansi-line').first()).toBeVisible();
  const estimated = await terminal.evaluate((element) => element.scrollHeight);
  // Walking the log mounts and measures every row, so the height afterwards is
  // the truth the estimate was predicting.
  const step = await terminal.evaluate((element) => Math.max(1, element.clientHeight - 20));
  for (let top = 0; top <= estimated; top += step) {
    await terminal.evaluate((element, offset) => {
      element.scrollTop = offset;
      element.dispatchEvent(new Event('scroll'));
    }, top);
    await page.waitForTimeout(50);
  }
  await expect.poll(async () => {
    const measured = await terminal.evaluate((element) => element.scrollHeight);
    return Math.abs(measured - estimated) / measured < 0.1;
  }).toBe(true);
});

test('keeps box-drawn controller rows aligned at the desktop width', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Older relay app', agent: 'omp' }],
  });
  await page.getByRole('button', { name: 'Open Older relay app on Fedora' }).click();
  const border = `╭${'─'.repeat(98)}╮`;
  const rightBorder = (content: string) => `${content.padEnd(99)}│`;
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: [
      border,
      `\u001b[48;2;15;18;22m${rightBorder(' Welcome back!')}\u001b[0m`,
      rightBorder('  prewalk    Switch model'),
      rightBorder('  dump       Copy session'),
    ].join('\n'),
  });

  const terminal = page.getByRole('log');
  const lines = terminal.locator('.ansi-line');
  await expect(lines).toHaveCount(4);
  expect(await lines.first().textContent()).toBe(border);
  await expect(lines.nth(1)).toHaveClass(/ansi-line-background/);
  const lineWidths = await lines.evaluateAll((elements) => (
    elements.map((element) => Math.round(element.getBoundingClientRect().width))
  ));
  expect(new Set(lineWidths).size).toBe(1);
  const rightEdges = await lines.evaluateAll((elements) => elements.map((element) => {
    const cells = element.querySelectorAll<HTMLElement>('.terminal-cell');
    return cells[cells.length - 1]?.getBoundingClientRect().right || 0;
  }));
  expect(Math.max(...rightEdges) - Math.min(...rightEdges)).toBeLessThan(0.1);
  expect(await terminal.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(true);
  expect(await page.evaluate(() =>
    document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
  await expect(page.getByRole('alert')).toHaveCount(0);
  await expect(page.getByRole('navigation', { name: 'Application' })
    .getByRole('button', { name: /Terminal width:/ })).toHaveCount(0);
});
