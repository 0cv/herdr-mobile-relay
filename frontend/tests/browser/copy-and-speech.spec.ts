import { expect, test } from '@playwright/test';
import { boot, commands, commandsForSocket, connectFedora, fedora, handshake, openAgent, server, setAutoCommands, setConversationFixture, socketCount } from '../support/journey-relay';

test('copies visible terminal output when no completed response is available', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        async writeText(text: string) {
          Reflect.set(window, '__copiedTerminal', text);
        },
      },
    });
  });
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'Shell output', agent: 'codex' });
  const visibleOutput = 'shell output\nsecond line';
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: visibleOutput,
  });
  const fullTranscript = page.getByRole('textbox', { name: 'Full terminal transcript' });
  const responseTranscript = page.getByRole('textbox', { name: 'Latest final response' });
  await expect(fullTranscript).toHaveValue(visibleOutput);
  await expect(responseTranscript).toHaveValue('');
  await page.getByRole('button', { name: 'Copy', exact: true }).click();
  await expect.poll(() => page.evaluate(() =>
    Reflect.get(window, '__copiedTerminal'))).toBe(visibleOutput);
  await expect(page.getByRole('status').filter({ hasText: 'Copied the visible terminal output.' })).toBeVisible();
  await page.evaluate(() => {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined });
  });
  await page.getByRole('button', { name: 'Copy', exact: true }).click();
  await expect(fullTranscript).toBeFocused();
  expect(await fullTranscript.evaluate((element) => {
    if (!(element instanceof HTMLTextAreaElement)) return null;
    return { start: element.selectionStart, end: element.selectionEnd };
  })).toEqual({ start: 0, end: visibleOutput.length });
  await expect(page.getByRole('status').filter({ hasText: 'Output selected.' })).toBeVisible();
});

test('uses relay response copy before parser and surfaces failures', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        async writeText(text: string) {
          Reflect.set(window, '__copiedTerminal', text);
        },
      },
    });
  });
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'Remote copy', agent: 'claude' }, {
    capabilities: [
      'attention_classification',
      'clear_activities',
      'directory_browser',
      'self_update',
      'structured_questions',
      'slash_commands',
      'agent_response_copy',
    ],
  });
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: '     + Thought: 0.1s\n     Parsed terminal response.\n     ▣ Build · model · 1s',
  });
  const copyButton = page.getByRole('button', { name: 'Copy', exact: true });
  const responseTranscript = page.getByRole('textbox', { name: 'Latest final response' });
  await expect(responseTranscript).toHaveValue('Parsed terminal response.');
  expect((await commandsForSocket(page, 0))
    .filter((command) => command.type === 'list_slash_commands')).toHaveLength(0);
  await setAutoCommands(page, false);

  await copyButton.click();
  await expect(page.getByRole('button', { name: 'Copying…', exact: true })).toBeDisabled();
  await expect.poll(async () => (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'copy_agent_response')).toHaveLength(1);
  const failedCopyCommand = (await commandsForSocket(page, 0))
    .find((command) => command.type === 'copy_agent_response') as Record<string, unknown>;
  await server(page, 0, {
    type: 'command_result',
    action: 'copy_agent_response',
    request_id: failedCopyCommand.request_id,
    ok: false,
    phase: 'failed',
    error: 'Agent did not confirm a copied response',
  });
  // The control is an icon, so its idle state shows in the accessible name.
  await expect(copyButton).toBeEnabled();
  await expect(copyButton).toHaveAccessibleName('Copy');
  await expect(page.getByRole('status').filter({ hasText: 'Agent did not confirm a copied response' })).toBeVisible();
  expect(await page.evaluate(() => Reflect.get(window, '__copiedTerminal'))).toBeUndefined();

  await setAutoCommands(page, true);
  await copyButton.click();
  await expect.poll(() => page.evaluate(() => Reflect.get(window, '__copiedTerminal')))
    .toBe('# Remote markdown response\n\n- Exact copied output');
  await expect(responseTranscript).toHaveValue('Parsed terminal response.');
  await expect(page.getByRole('region', { name: 'Copied agent response' })).toBeVisible();
  await expect(page.getByRole('textbox', { name: 'Copied agent response markdown' }))
    .toHaveValue('# Remote markdown response\n\n- Exact copied output');
  await expect(page.getByRole('status').filter({ hasText: 'Agent response copied.' })).toBeVisible();
});

test('reads a response aloud in the language the relay has a voice for', async ({ page }) => {
  await connectFedora(page, {
    capabilities: ['attention_classification', 'slash_commands', 'agent_response_copy'],
    speech_languages: ['en'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Spoken output', agent: 'codex' }],
  });

  await page.getByRole('button', { name: 'Settings' }).click();
  // A relay that can already read aloud turns the setting on by itself the
  // first time; the phone never has to find it.
  await expect(page.getByRole('switch', { name: 'Read Responses Aloud' })).toBeChecked();
  await page.getByLabel('Language').selectOption('fr');
  await expect(page.getByRole('status').filter({ hasText: 'No French voice on Fedora' })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();

  await page.getByRole('button', { name: 'Open Spoken output on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: '     Parsed terminal response.',
  });
  const speakButton = page.getByRole('button', { name: 'Read latest response aloud' });
  await speakButton.click();
  await expect(page.getByRole('status').filter({ hasText: 'This relay has no French voice' })).toBeVisible();
  expect((await commandsForSocket(page, 0)).filter((command) => command.type === 'speak_text')).toHaveLength(0);

  // The relay gains a French voice; the same button now streams audio for it.
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'slash_commands', 'agent_response_copy'],
    speech_languages: ['en', 'fr'],
  });
  await speakButton.click();
  await expect.poll(async () => (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'speak_text').length).toBe(1);
  expect((await commandsForSocket(page, 0)).find((command) => command.type === 'speak_text'))
    .toMatchObject({ language: 'fr', text: 'Remote markdown response\nExact copied output' });
});

test('speak reports the relay copy failure when no parser can read the pane', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'Spoken omp', agent: 'omp' }, {
    capabilities: ['attention_classification', 'slash_commands', 'agent_response_copy'],
    speech_languages: ['en'],
  });
  // omp draws no response marker the phone can parse, so the relay's /copy
  // transaction is the only source of text.
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: ' Completed omp response.\n\n────────────────────\n\n────────────────────',
  });
  await expect(page.getByRole('button', { name: 'Read latest response aloud' })).toBeVisible();
  expect((await commandsForSocket(page, 0))
    .filter((command) => command.type === 'list_slash_commands')).toHaveLength(0);
  await setAutoCommands(page, false);

  await page.getByRole('button', { name: 'Read latest response aloud' }).click();
  await expect.poll(async () => (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'copy_agent_response')).toHaveLength(1);
  const copyCommand = (await commandsForSocket(page, 0))
    .find((command) => command.type === 'copy_agent_response') as Record<string, unknown>;
  await server(page, 0, {
    type: 'command_result',
    action: 'copy_agent_response',
    request_id: copyCommand.request_id,
    ok: false,
    phase: 'failed',
    error: 'Agent did not confirm a copied response',
  });
  await expect(page.getByRole('status').filter({ hasText: 'Agent did not confirm a copied response' })).toBeVisible();
  expect((await commandsForSocket(page, 0)).filter((command) => command.type === 'speak_text')).toHaveLength(0);
});

test('a reader speaks the latest transcript turn without the relay copy command', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await page.evaluate(() => {
    localStorage.setItem('herdr_device_auth_v1', JSON.stringify({
      version: 1,
      relays: {
        fedora: {
          kind: 'credential', id: 'credential-reader', version: 1, secret: 'R'.repeat(43),
          deviceId: 'device-reader', role: 'reader', locale: 'en', issuedAt: Date.now(),
        },
      },
    }));
  });
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'slash_commands', 'agent_response_copy'],
    speech_languages: ['en'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Read aloud', agent: 'omp', conversation_history_available: true }],
  });
  await setConversationFixture(page, {
    entries: [
      { id: 'turn-1', timestamp: '2026-09-02T10:00:00Z', role: 'user', text: 'Summarise the build' },
      { id: 'turn-2', timestamp: '2026-09-02T10:00:05Z', role: 'assistant', text: 'The build passed with no warnings.' },
    ],
    total: 2,
  });
  await page.getByRole('button', { name: 'Open Read aloud on Fedora' }).click();
  const reader = page.getByRole('main', { name: 'Terminal for Read aloud', exact: true });
  const readOnlyFeedback = reader.getByRole('status').filter({ hasText: 'Reader access is read only' });
  await expect(readOnlyFeedback).toContainText('Reader access is read only');
  await expect(readOnlyFeedback).toBeVisible();
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: ' Completed omp response.' });

  await page.getByRole('button', { name: 'Read latest response aloud' }).click();
  await expect.poll(async () => (await commandsForSocket(page, 0))
    .filter((command) => command.type === 'speak_text').length).toBe(1);
  const commands = await commandsForSocket(page, 0);
  // The relay copy command types into the agent's terminal, which a reader may
  // not do; the agent's transcript is the read-only source instead.
  expect(commands.filter((command) => command.type === 'copy_agent_response')).toHaveLength(0);
  expect(commands.find((command) => command.type === 'speak_text'))
    .toMatchObject({ text: 'The build passed with no warnings.' });
});

test('uses relay response copy and allows dismissing the markdown preview', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'Terminal app', agent: 'codex' }, {
    capabilities: [
      'attention_classification', 'clear_activities', 'directory_browser', 'self_update',
      'structured_questions', 'slash_commands', 'agent_response_copy',
    ],
  });
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'remote copy fixture',
  });
  const copied = '# Remote markdown response\n\n- Exact copied output';
  await page.evaluate(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        writeText: async (text: string) => {
          (window as typeof window & { __copiedTerminal?: string }).__copiedTerminal = text;
        },
      },
    });
  });
  const copyOutput = page.getByRole('button', { name: 'Copy', exact: true });
  await expect(copyOutput).toBeVisible();
  await copyOutput.click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'copy_agent_response')).toHaveLength(1);
  await expect.poll(() => page.evaluate(
    () => (window as typeof window & { __copiedTerminal?: string }).__copiedTerminal,
  )).toBe(copied);
  const preview = page.getByRole('region', { name: 'Copied agent response' });
  await expect(preview).toBeVisible();
  await expect(page.getByRole('textbox', { name: 'Copied agent response markdown' })).toHaveValue(copied);
  await page.getByRole('button', { name: 'Dismiss' }).click();
  await expect(preview).toBeHidden();
});

test('surfaces relay response-copy failures instead of parsing terminal output', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'Terminal app', agent: 'codex' }, {
    capabilities: [
      'attention_classification', 'clear_activities', 'directory_browser', 'self_update',
      'structured_questions', 'slash_commands', 'agent_response_copy',
    ],
  });
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'parser fallback must not run',
  });
  await setAutoCommands(page, false);
  const copyOutput = page.getByRole('button', { name: 'Copy', exact: true });
  await expect(copyOutput).toBeVisible();
  await copyOutput.click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'copy_agent_response')).toHaveLength(1);
  const command = (await commands(page)).find((entry) => entry.type === 'copy_agent_response')!;
  await server(page, 0, {
    type: 'command_result',
    action: 'copy_agent_response',
    request_id: command.request_id,
    ok: false,
    phase: 'failed',
    error: 'The agent did not confirm a copied response; try again',
  });
  await expect(page.getByRole('status').filter({
    hasText: 'The agent did not confirm a copied response; try again',
  })).toBeVisible();
});
