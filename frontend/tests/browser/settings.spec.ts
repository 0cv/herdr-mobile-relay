import { expect, test } from '@playwright/test';
import { saveDeviceCredential } from '../support/journey-device';
import { boot, commands, commandsForSocket, connectFedora, fedora, handshake, lastCommand, reply, server, setAutoCommands, socketCount } from '../support/journey-relay';

test('downloads a missing relay speech voice from Settings', async ({ page }) => {
  await connectFedora(page, {
    capabilities: ['attention_classification', 'slash_commands', 'speech_voice_management'],
    speech_languages: ['en'],
  });

  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByRole('switch', { name: 'Read Responses Aloud' })).toBeChecked();
  await page.getByLabel('Language').selectOption('fr');
  await expect(page.getByRole('status').filter({ hasText: 'No French voice on Fedora' })).toBeVisible();

  const englishRow = page.getByLabel('English voice on Fedora', { exact: true });
  const frenchRow = page.getByLabel('French voice on Fedora', { exact: true });
  await expect(englishRow).toContainText('Neural voice cached, 63 MB');
  await expect(frenchRow).toContainText('Not downloaded - 63 MB download');

  // A download in flight parks only its own row.
  await setAutoCommands(page, false);
  const germanDownload = page.getByLabel('German voice on Fedora', { exact: true })
    .getByRole('button', { name: 'Download the German voice on Fedora' });
  await germanDownload.click();
  await expect(germanDownload).toBeDisabled();
  await expect(germanDownload).toHaveText('Downloading…');
  await expect(germanDownload).toHaveAttribute('aria-busy', 'true');
  await setAutoCommands(page, true);

  await frenchRow.getByRole('button', { name: 'Download the French voice on Fedora' }).click();
  await expect(frenchRow).toContainText('Neural voice cached, 63 MB');
  await expect(page.getByRole('status').filter({ hasText: 'No French voice on Fedora' })).toHaveCount(0);

  // The language being read aloud is never blocked from removal; the relay
  // falls back to its own system engine.
  await frenchRow.getByRole('button', { name: 'Remove the French voice on Fedora' }).click();
  await expect(frenchRow).toContainText('Not downloaded - 63 MB download');
  await expect(page.getByRole('status').filter({ hasText: 'No French voice on Fedora' })).toBeVisible();
  const sent = await commandsForSocket(page, 0);
  expect(sent.filter((command) => command.type === 'speech_voices_list')).toHaveLength(1);
  expect(sent.filter((command) => command.type === 'speech_voice_install'))
    .toMatchObject([{ language: 'de' }, { language: 'fr' }]);
  expect(sent.filter((command) => command.type === 'speech_voice_remove'))
    .toMatchObject([{ language: 'fr' }]);
});

test('scales the whole interface from accessible settings', async ({ page }) => {
  await boot(page, [fedora]);
  await page.getByRole('button', { name: 'Settings' }).click();
  const sizes = page.getByRole('group', { name: 'Interface Size' });
  const history = page.getByRole('group', { name: 'Terminal History' });
  const refresh = page.getByRole('group', { name: 'Terminal Refresh' });
  const heading = page.getByRole('heading', { name: 'Settings', level: 2 });
  expect(await refresh.getByRole('button', { name: '250 ms' })
    .evaluate((element) => getComputedStyle(element).textTransform)).toBe('none');

  await sizes.getByRole('button', { name: 'Compact' }).click();
  const compactHeadingSize = await heading.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize));
  const compactInputSize = await page.getByLabel('Relay Name').evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize));
  await page.getByLabel('Relay Name').focus();
  expect(compactInputSize).toBeGreaterThanOrEqual(16);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
  await sizes.getByRole('button', { name: 'Large' }).click();
  const largeHeadingSize = await heading.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize));

  expect(largeHeadingSize).toBeGreaterThan(compactHeadingSize);
  expect(await page.evaluate(() => document.documentElement.dataset.interfaceSize)).toBe('large');
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_font_size'))).toBe('large');
  await expect(page.getByRole('group', { name: 'Terminal Width' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Fit to Phone' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Original Columns' })).toHaveCount(0);
  await expect(page.getByRole('switch', { name: 'Resize Session', exact: true })).toBeChecked();
  await expect(page.getByText(/laptop and SSH views of that pane look garbled/)).toBeVisible();
  await expect(page.getByRole('switch', { name: 'Resize Session', exact: true }))
    .toHaveAccessibleDescription(/Resizing may truncate scrollback and lose older output\./);

  await history.getByRole('button', { name: '500' }).click();
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_history_lines'))).toBe('500');
  await refresh.getByRole('button', { name: '100 ms' }).click();
  expect(await page.evaluate(() => localStorage.getItem('herdr_terminal_refresh_ms'))).toBe('100');
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'pane_realtime_delta', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'History app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Open History app on Fedora' }).click();
  await expect.poll(async () => (await commands(page))
    .some((command) => command.type === 'read_pane' && command.lines === 500)).toBe(true);
  const read = (await commands(page)).findLast(command => command.type === 'read_pane')!;
  await server(page, 0, {
    request_id: read.request_id, target: read.target,
    type: 'pane_content',
    pane_id: 'w1:p1',
    content: 'History output',
    format: 'ansi',
    content_fingerprint: 'history-content',
  });
  await expect.poll(async () => (await commands(page)).findLast((command) => command.type === 'watch_pane'))
    .toMatchObject({ pane_id: 'w1:p1', interval_ms: 100 });
});

for (const role of ['controller', 'reader']) test(`followup Pi Integration setting is ${role === 'reader' ? 'read-only for readers' : 'controller-controlled with per-profile results'}`, async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await saveDeviceCredential(page, { id: 'synthetic-setting', deviceId: 'synthetic-setting', secret: 'S'.repeat(43), role });
  await setAutoCommands(page, false);
  await handshake(page, 0, { capabilities: ['pi_integration'] });
  await page.getByRole('button', { name: /^Settings(?:,|$)/ }).click();
  const respond = (request: Record<string, unknown>, enabled: boolean) => reply(page, request, {
    instance: 'synthetic-setting-manager', enabled,
    profiles: [{ path: '/synthetic/pi/agent', status: enabled ? 'installed' : 'not installed' },
      { path: '/synthetic/refused/agent', status: 'refused', reason: 'Unowned directory' }],
  });
  await respond(await lastCommand(page, 'get_pi_integration'), role === 'reader');
  const toggle = page.getByRole('switch', { name: 'Pi Integration', exact: true });
  await expect(page.getByText(/Reload Pi to activate changes/)).toBeVisible();
  await expect(page.getByText('/synthetic/refused/agent: refused — Unowned directory', { exact: true })).toBeVisible();
  if (role === 'reader') {
    await expect(toggle).toBeChecked();
    await expect(toggle).toBeDisabled();
    await toggle.evaluate(element => (element as HTMLButtonElement).click());
    await expect(page.getByText('Controller access required to change this setting.', { exact: true })).toBeVisible();
    expect((await commands(page)).filter(command => command.type === 'set_pi_integration')).toHaveLength(0);
  } else {
    await expect(toggle).not.toBeChecked();
    await expect(toggle).toBeEnabled();
    await toggle.click();
    const install = await lastCommand(page, 'set_pi_integration');
    expect(install.enabled).toBe(true);
    expect(install.instance).toBe('synthetic-setting-manager');
    expect(Object.keys(install).sort()).toEqual(['client_id', 'enabled', 'instance', 'protocol', 'request_id', 'type']);
    await respond(install, true);
    await expect(toggle).toBeChecked();
    await expect(page.getByText('/synthetic/pi/agent: installed', { exact: true })).toBeVisible();
    await toggle.click();
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'set_pi_integration').length).toBe(2);
    const remove = await lastCommand(page, 'set_pi_integration');
    expect(remove.enabled).toBe(false);
    await respond(remove, false);
    await expect(toggle).not.toBeChecked();
  }
  expect((await commands(page)).filter(command => ['submit_prompt', 'send_pi_prompt_unconfirmed', 'send_keys', 'send_terminal_text'].includes(String(command.type)))).toHaveLength(0);
});
