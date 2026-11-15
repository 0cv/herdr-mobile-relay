import { expect, test, type Locator, type Page } from '@playwright/test';
import { resolve } from 'node:path';
import { scrollConversationToBottom, showOlderConversationEntry } from '../support/journey-conversation';
import { APP_METADATA, emulateStandalone } from '../support/journey-device';
import { boot, closeSocket, commands, composerTarget, fedora, handshake, openSharedComposer, replyToPaneRead, server, setAutoCommands, setConversationFixture, socketCount } from '../support/journey-relay';

test('reads and replies from native conversation history', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_default_agent_view', 'terminal');
    localStorage.setItem('herdr-conversation-view', 'conversation');
  });
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        async writeText(text: string) {
          Reflect.set(window, '__copiedConversation', text);
        },
      },
    });
  });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: [
      'attention_classification',
      'clear_activities',
      'directory_browser',
      'self_update',
      'structured_questions',
      'slash_commands',
      'conversation_history',
    ],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'History app',
      agent: 'omp',
      server_session_id: 'session-1',
      terminal_id: 'terminal-1',
      generation: 1,
      session: 'Current session',
      conversation_history_available: true,
    }],
  });

  await page.getByRole('button', { name: 'Open History app on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  expect((await commands(page)).filter(command => command.type === 'get_conversation_history')).toHaveLength(1);
  await page.getByRole('button', { name: 'Load older turns', exact: true }).click();
  await expect(page.getByText('middle retained answer')).toBeVisible();
  await expect(page.getByText('latest retained question')).toBeVisible();
  await page.getByRole('button', { name: 'Copy History app message as Markdown' }).click();
  await expect.poll(() => page.evaluate(() => Reflect.get(window, '__copiedConversation')))
    .toBe('# middle retained answer');
  // The compact view keeps only user prompts and final answers. Full history
  // owns both superseded prose and the tool activity attached to it.
  await expect(page.getByText('intermediate progress update')).toBeHidden();
  await expect(page.getByText('Read', { exact: true })).toBeHidden();
  await page.getByRole('button', { name: 'Full history' }).click();
  await expect(page.getByText('intermediate progress update')).toBeVisible();
  await expect(page.getByText('Read', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await expect(page.getByText('intermediate progress update')).toBeHidden();
  await expect(page.getByText('Read', { exact: true })).toBeHidden();

  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await composer.fill('Review the attached screen');
  const imageInput = page.locator('.conversation-input-area input[type=file][accept="image/*"]');
  await imageInput.setInputFiles({
    name: 'history-shot.png',
    mimeType: 'image/png',
    buffer: Buffer.from('png'),
  });
  const attachedPrompt = [
    'Review the attached screen',
    'Attachment: attachment:test-1',
    '',
  ].join('\n');
  await expect(composer).toHaveValue(attachedPrompt);
  await expect(page.getByText('Attached history-shot.png')).toBeVisible();
  const historyReadsBeforeSend = (await commands(page))
    .filter((command) => command.type === 'get_conversation_history').length;
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => (
    command.type === 'submit_prompt' && command.text === attachedPrompt.replace(/\n$/, '')
  ))).toMatchObject({ pane_id: 'w1:p1' });
  await expect(composer).toHaveValue('');
  await expect(page.getByText('Attached history-shot.png')).toBeHidden();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'get_conversation_history').length).toBeGreaterThan(historyReadsBeforeSend);

  await imageInput.setInputFiles({
    name: 'history-shot.png',
    mimeType: 'image/png',
    buffer: Buffer.from('png'),
  });
  await expect(page.getByText('Attached history-shot.png')).toBeVisible();
  await page.getByRole('button', { name: 'Clear prompt text' }).click();
  await expect(composer).toHaveValue('');
  await expect(page.getByText('Attached history-shot.png')).toBeHidden();

  await setAutoCommands(page, false);
  await composer.fill('restore this known failure');
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).some((command) => (
    command.type === 'submit_prompt' && command.text === 'restore this known failure'
  ))).toBe(true);
  const failedCommand = (await commands(page)).find((command) => (
    command.type === 'submit_prompt' && command.text === 'restore this known failure'
  ))!;
  await server(page, 0, {
    type: 'command_result',
    action: 'submit_prompt',
    request_id: failedCommand.request_id,
    ok: false,
    phase: 'failed',
    error: 'Relay rejected prompt',
  });
  await expect(composer).toHaveValue('restore this known failure');

  await composer.fill('do not send this twice');
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).some((command) => (
    command.type === 'submit_prompt' && command.text === 'do not send this twice'
  ))).toBe(true);
  const ambiguousCommand = (await commands(page)).find((command) => (
    command.type === 'submit_prompt' && command.text === 'do not send this twice'
  ))!;
  await server(page, 0, {
    type: 'command_result',
    action: 'submit_prompt',
    request_id: ambiguousCommand.request_id,
    ok: false,
    phase: 'dispatched_unknown',
    error: 'Command may have executed',
  });
  await expect(composer).toHaveValue('');
  await expect(page.getByText('Command may have executed Check the terminal before sending again.')).toBeVisible();
  await setAutoCommands(page, true);

  await expect(page.getByText('first retained question')).toBeVisible();
  await expect.poll(async () => (await commands(page)).find((command) => (
    command.type === 'get_conversation_history' && command.cursor === 'cursor-1'
  ))).toMatchObject({ pane_id: 'w1:p1', cursor: 'cursor-1' });

  const search = page.getByRole('searchbox', { name: 'Search loaded conversation' });
  await search.fill('first retained');
  await expect(page.getByText('first retained question')).toBeVisible();
  await expect(page.getByText('latest retained question')).toBeHidden();
  await server(page, 0, {
    type: 'agent_update',
    pane_id: 'w1:p1',
    status: 'blocked',
    attention_kind: 'approval',
    options: ['Approve once', 'Reject'],
    server_session_id: 'session-1',
    terminal_id: 'terminal-1',
    generation: 1,
    updated_at: 2,
  });
  await expect(composer).toBeEnabled();
  await composer.fill('approval-local draft');
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Attach files', exact: true })).toBeDisabled();
  const promptsBeforeApprovalEdit = (await commands(page)).filter(command => command.type === 'submit_prompt').length;
  await composer.press('Control+Enter');
  expect((await commands(page)).filter(command => command.type === 'submit_prompt').length).toBe(promptsBeforeApprovalEdit);
  await expect(page.getByRole('button', { name: 'Keyboard', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Enter', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Approve once', exact: true })).toBeVisible();

  await page.getByRole('button', { name: 'Terminal view' }).click();
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toBeVisible();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.getByRole('button', { name: 'Open History app on Fedora' })).toBeVisible();
});

for (const provider of ['claude', 'codex', 'opencode', 'qoder', 'pi', 'omp', 'omo', 'hermes', 'cursor', 'kimi']) {
test(`default agent view: fresh ${provider} openings use clean Conversation`, async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Default conversation',
      agent: provider,
      conversation_history_available: true,
      agent_session_id: 'session-1',
    }],
  });
  const before = await commands(page);
  await setConversationFixture(page, { entries: [
    { id: 'default-user', timestamp: '', role: 'user', text: 'Show the final answer' },
    { id: 'default-tool', timestamp: '', role: 'assistant', text: '', tools: [{ id: 'tool', name: 'Default tool', output: 'done' }] },
    { id: 'default-answer', timestamp: '', role: 'assistant', text: '```ts\nconst visibleAnswer = true;\n```' },
  ], total: 3 });
  await page.getByRole('button', { name: 'Open Default conversation on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Conversation', exact: true })).toHaveAttribute('aria-pressed', 'true');
  await expect(page.getByRole('button', { name: 'Full history', exact: true })).toHaveAttribute('aria-pressed', 'false');
  await expect(page.locator('summary').filter({ hasText: 'Default tool' })).toHaveCount(0);
  await expect(page.getByText('Show the final answer', { exact: true })).toBeVisible();
  await expect(page.locator('.conversation-markdown code')).toHaveText('const visibleAnswer = true;\n');
  expect((await commands(page)).filter(command => command.type === 'get_conversation_history')).toHaveLength(1);
  await page.getByRole('button', { name: 'Full history', exact: true }).click();
  await expect(page.locator('summary').filter({ hasText: 'Default tool' })).toBeVisible();
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await expect(page.locator('summary').filter({ hasText: 'Default tool' })).toHaveCount(0);
  expect((await commands(page)).filter(command => command.type === 'get_conversation_history')).toHaveLength(1);
  await expect(page.getByLabel('Prompt', { exact: true })).toBeEnabled();
  for (const name of ['Keyboard', 'Enter', 'Tab', 'Ctrl', 'Send text']) await expect(page.getByRole('button', { name, exact: true })).toHaveCount(0);
  await expect(page.getByText('Current terminal context', { exact: true })).toHaveCount(0);
  const openingCommands = (await commands(page)).slice(before.length);
  expect(openingCommands.some((command) => command.type === 'get_conversation_history')).toBe(true);
  expect(openingCommands.find(command => command.type === 'read_pane')).toMatchObject({ target: { server_session_id: 'primary', pane_id: 'w1:p1', terminal_id: 'terminal-w1:p1', generation: 1, agent_session_id: 'session-1' } });
  expect(openingCommands.some(command => command.type === 'watch_pane')).toBe(false);
  expect(openingCommands.some(command => String(command.type).startsWith('agent_') || ['submit_prompt', 'send_input', 'send_keys', 'lease_pane_size', 'release_pane_size'].includes(String(command.type)))).toBe(false);
  await page.getByRole('button', { name: 'Terminal view' }).tap();
  await expect(page.getByRole('main', { name: 'Terminal for Default conversation' })).toBeVisible();
  // A working agent accepts prompts, so literal terminal input is not offered.
  await expect(page.getByRole('button', { name: 'Keyboard', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Enter', exact: true })).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem('herdr_default_agent_view'))).toBeNull();
});
}

for (const attention of ['idle', 'question', 'approval', 'pending']) {
  test(`phone Conversation controls: ${attention} delivery stays fenced across view changes`, async ({ page }) => {
    const button = (name: string) => page.getByRole('button', { name, exact: true });
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0, { capabilities: ['conversation_history', 'manual_terminal_input', 'structured_questions', 'attention_classification'] });
    const settled = ['idle', 'pending'].includes(attention);
    const phoneAgents = (inspecting: boolean) => ({ type: 'agents', agents: [{
      pane_id: 'w1:p1', status: settled && !inspecting ? 'idle' : 'blocked', project: 'Phone controls', agent: 'pi',
      conversation_history_available: true, agent_session_id: 'phone-controls-session', attention_kind: inspecting || attention === 'pending' ? 'unknown' : attention,
      options: ['Allow once', 'Deny'], event_id: 'phone-approval',
      interaction: attention === 'question' ? { id: 'phone-question', kind: 'single_select', question: 'Choose destination', options: [{ index: 0, label: 'North', selected: true }], submit_label: 'Submit' } : undefined,
    }] });
    // The Keyboard button is offered only while the composer cannot send, so a settled agent first needs inspection.
    await server(page, 0, phoneAgents(settled));
    await setConversationFixture(page, { entries: [{ id: 'code', timestamp: '', role: 'assistant', text: '```ts\nconst kept = true;\n```' }], total: 1 });
    await button('Open Phone controls on Fedora').click();
    await expect(page.locator('.conversation-markdown code')).toHaveText('const kept = true;\n');
    await expect(button('Keyboard')).toHaveCount(0);
    await button('Terminal view').click();
    await button('Keyboard').click();
    await expect(page.getByLabel('Terminal text', { exact: true })).toBeVisible();
    // The app keeps a blocked state through one non-blocked snapshot, so the agent settles on the second.
    if (settled) for (let snapshot = 0; snapshot < 2; snapshot++) await server(page, 0, phoneAgents(false));
    const observe = async () => {
      const read = (await commands(page)).findLast(command => command.type === 'read_pane');
      expect(read).toBeTruthy();
      await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read!.request_id, target: read!.target, content: 'RAW TERMINAL PREVIEW\n> ', format: 'plain', no_echo: false });
    };
    await observe();
    await expect(button('Enter')).toBeEnabled();
    await setAutoCommands(page, false);
    await page.getByLabel('Terminal text', { exact: true }).fill('private phone terminal attempt');
    await button('Send text').click();
    const attempt = (await commands(page)).findLast(command => command.type === 'send_terminal_text')!;
    expect(attempt).toMatchObject({ pane_id: 'w1:p1', text: 'private phone terminal attempt', target: { server_session_id: 'primary', terminal_id: 'terminal-w1:p1', generation: 1, agent_session_id: 'phone-controls-session' } });
    const unknown = () => server(page, 0, { type: 'command_result', request_id: attempt.request_id, ok: true, phase: 'dispatched_unknown' });
    if (attention !== 'pending') await unknown();
    await setAutoCommands(page, true);
    await button('Conversation history').click();
    if (attention === 'pending') {
      await expect(page.getByText('Waiting for terminal input delivery.', { exact: true })).toBeVisible();
      await unknown();
    }
    await expect(page.getByText('Delivery is uncertain. In Terminal, tap Keyboard to check before sending.', { exact: true })).toBeVisible();
    await page.getByLabel('Prompt', { exact: true }).fill('editable draft after uncertainty');
    await expect(button('Send prompt')).toBeDisabled();
    await expect(button('Attach files')).toBeDisabled();
    await page.getByLabel('Prompt', { exact: true }).press('Control+Enter');
    for (const name of ['Keyboard', 'Enter', 'Tab', 'Ctrl', 'Send text', 'I checked the terminal']) await expect(button(name)).toHaveCount(0);
    await expect(page.getByText('Current terminal context', { exact: true })).toHaveCount(0);
    await expect(page.getByText('RAW TERMINAL PREVIEW', { exact: true })).toHaveCount(0);
    if (attention === 'question') {
      await expect(page.getByRole('radio', { name: 'North' })).toBeEnabled();
      await expect(button('Submit')).toBeDisabled();
    } else if (attention === 'approval') await expect(button('Allow once')).toBeDisabled();
    await button('Terminal view').click();
    await button('Keyboard').click();
    await expect(page.getByLabel('Terminal text', { exact: true })).toHaveValue('');
    await expect(button('I checked the terminal')).toBeDisabled();
    await observe();
    await expect(button('I checked the terminal')).toBeEnabled();
    await expect(button('Enter')).toBeDisabled();
    await button('I checked the terminal').click();
    await expect(button('Enter')).toBeEnabled();
    await button('Conversation history').click();
    await expect(page.getByText(/^Delivery is uncertain\./)).toHaveCount(0);
    if (attention === 'question') await expect(button('Submit')).toBeEnabled();
    else if (attention === 'approval') await expect(button('Allow once')).toBeEnabled();
    else {
      await page.getByLabel('Prompt', { exact: true }).fill('new normal draft');
      await expect(button('Send prompt')).toBeEnabled();
    }
    expect((await commands(page)).filter(command => ['send_terminal_text', 'send_keys', 'submit_prompt', 'answer_question', 'respond', 'navigate_question', 'clarify_question'].includes(String(command.type)))).toEqual([attempt]);
    expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('private phone terminal attempt');
  });
}

test('default agent view: stored Terminal wins across reload', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Terminal', exact: true }).click();
  expect(await page.evaluate(() => localStorage.getItem('herdr_default_agent_view'))).toBe('terminal');
  await page.getByRole('button', { name: 'Back' }).click();
  await page.reload();
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Stored terminal', agent: 'pi', conversation_history_available: true, agent_session_id: 'stored-session' }] });
  const before = await commands(page);
  await page.getByRole('button', { name: 'Open Stored terminal on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Stored terminal' })).toBeVisible();
  expect((await commands(page)).slice(before.length).some(command => command.type === 'get_conversation_history')).toBe(false);
});

test('default agent view: Conversation opens directly and persists across reload', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Default conversation',
      agent: 'codex',
      conversation_history_available: true,
      agent_session_id: 'session-1',
    }],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  const settingsViews = page.getByRole('group', { name: 'Default View' });
  await settingsViews.getByRole('button', { name: 'Conversation' }).click();
  expect(await page.evaluate(() => localStorage.getItem('herdr_default_agent_view'))).toBe('conversation');
  await page.getByRole('button', { name: 'Back' }).click();
  await setConversationFixture(page, {
    entries: [{ id: 'turn-1', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'Direct conversation answer' }],
    total: 1,
  });
  const before = await commands(page);
  await page.getByRole('button', { name: 'Open Default conversation on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByText('Direct conversation answer')).toBeVisible();
  const openingCommands = (await commands(page)).slice(before.length);
  expect(openingCommands.find(command => command.type === 'read_pane')).toMatchObject({ target: { server_session_id: 'primary', pane_id: 'w1:p1', terminal_id: 'terminal-w1:p1', generation: 1, agent_session_id: 'session-1' } });
  expect(openingCommands.some(command => command.type === 'watch_pane')).toBe(false);
  expect(openingCommands.some(command => String(command.type).startsWith('agent_') || ['submit_prompt', 'send_input', 'send_keys', 'lease_pane_size', 'release_pane_size'].includes(String(command.type)))).toBe(false);
  expect(openingCommands.some((command) => command.type === 'get_conversation_history')).toBe(true);

  await page.getByRole('button', { name: 'Back' }).click();
  await page.reload();
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Default conversation',
      agent: 'codex',
      conversation_history_available: true,
      agent_session_id: 'session-1',
    }],
  });
  await page.getByRole('button', { name: 'Open Default conversation on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
});

test('conversation loads while the browser reports offline over a live relay', async ({ page }) => {
  await page.addInitScript(() => {
    Object.defineProperty(Navigator.prototype, 'onLine', { configurable: true, get: () => false });
    localStorage.setItem('herdr_default_agent_view', 'conversation');
  });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Offline flag conversation',
      agent: 'pi',
      conversation_history_available: true,
      agent_session_id: 'session-1',
    }],
  });
  await setConversationFixture(page, {
    entries: [{ id: 'turn-1', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'Answer despite offline flag' }],
    total: 1,
  });
  await page.getByRole('button', { name: 'Open Offline flag conversation on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByText('Answer despite offline flag')).toBeVisible();
});

test('pane view override: both directions, inheritance, and explicit equal values', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [
      {
        pane_id: 'w1:p1', status: 'working', project: 'Override A', agent: 'codex',
        conversation_history_available: true, agent_session_id: 'session-a', terminal_id: 'terminal-a',
      },
      {
        pane_id: 'w1:p2', status: 'working', project: 'Override B', agent: 'codex',
        conversation_history_available: true, agent_session_id: 'session-b', terminal_id: 'terminal-b',
      },
    ],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await setConversationFixture(page, { entries: [{ id: 'turn-1', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'override answer' }], total: 1 });

  await page.getByRole('button', { name: 'Open Override A on Fedora' }).click();
  await page.getByRole('button', { name: 'Manage agent' }).click();
  const manage = page.getByRole('dialog', { name: 'Manage Agent' });
  const viewSelect = manage.getByRole('combobox', { name: 'Default View' });
  await viewSelect.selectOption('terminal');
  await manage.getByRole('button', { name: 'Close' }).click();
  await page.getByRole('button', { name: 'Back' }).click();

  await page.getByRole('button', { name: 'Open Override B on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();

  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Terminal' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Open Override A on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Override A' })).toBeVisible();
  await page.getByRole('button', { name: 'Manage agent' }).click();
  await page.getByRole('dialog', { name: 'Manage Agent' }).getByRole('combobox', { name: 'Default View' }).selectOption('conversation');
  await page.getByRole('dialog', { name: 'Manage Agent' }).getByRole('button', { name: 'Close' }).click();
  await page.getByRole('button', { name: 'Back' }).click();

  await page.getByRole('button', { name: 'Open Override B on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Override B' })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Open Override A on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Manage agent' }).click();
  await page.getByRole('dialog', { name: 'Manage Agent' }).getByRole('combobox', { name: 'Default View' }).selectOption('terminal');
  await page.getByRole('dialog', { name: 'Manage Agent' }).getByRole('button', { name: 'Close' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Open Override A on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Override A' })).toBeVisible();
});

test('default agent view: metadata fallback stays silent and does not switch later', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: [] });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Unavailable metadata', agent: 'unknown', agent_session_id: '' }],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  const before = await commands(page);
  await page.getByRole('button', { name: 'Open Unavailable metadata on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Unavailable metadata' })).toBeVisible();
  const openingCommands = (await commands(page)).slice(before.length);
  expect(openingCommands.some((command) => command.type === 'get_conversation_history')).toBe(false);
  expect(await page.getByRole('alert').allTextContents()).not.toContain('unavailable transcript');
  await server(page, 0, {
    type: 'agent_update', pane_id: 'w1:p1', status: 'working',
    conversation_history_available: true, updated_at: 2,
  });
  await page.waitForTimeout(50);
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toHaveCount(0);
});

test('default agent view: unavailable initial page replaces history without an extra entry', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'working', project: 'Unavailable transcript', agent: 'codex',
      conversation_history_available: true, agent_session_id: 'session-1', terminal_id: 'terminal-1',
    }],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Unavailable transcript on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  expect(page.locator('.terminal-layout')).toHaveCount(0);
  const historyRequest = (await commands(page)).find((command) => command.type === 'get_conversation_history');
  expect(historyRequest).toBeTruthy();
  await server(page, 0, {
    type: 'command_result',
    request_id: historyRequest!.request_id,
    action: 'get_conversation_history',
    ok: true,
    phase: 'completed',
    data: { available: false, state: 'ready', mode: 'recent', entries: [], has_more: false, total: null, diagnostics: {}, reason: 'Native transcript unavailable' },
  });
  await expect(page.getByRole('main', { name: 'Terminal for Unavailable transcript' })).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem('herdr_default_agent_view'))).toBe('conversation');
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.getByRole('button', { name: 'Open Unavailable transcript on Fedora' })).toBeVisible();
  await setAutoCommands(page, true);
});

test('default agent view: stale automatic responses cannot redirect Settings', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'working', project: 'Stale opening', agent: 'codex',
      conversation_history_available: true, agent_session_id: 'session-1', terminal_id: 'terminal-1',
    }],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Stale opening on Fedora' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'get_conversation_history')).toBeTruthy();
  const historyRequest = (await commands(page)).find((command) => command.type === 'get_conversation_history');
  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByRole('heading', { name: 'Settings', exact: true }).last()).toBeVisible();
  await server(page, 0, {
    type: 'command_result',
    request_id: historyRequest!.request_id,
    action: 'get_conversation_history',
    ok: true,
    phase: 'completed',
    data: { available: false, state: 'ready', mode: 'recent', entries: [], has_more: false, total: null, diagnostics: {}, reason: 'Native transcript unavailable' },
  });
  await page.waitForTimeout(50);
  await expect(page.getByRole('heading', { name: 'Settings', exact: true }).last()).toBeVisible();
  await setAutoCommands(page, true);
});

test('default agent view: manual switching ignores preferences and keeps explicit unavailable history', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'working', project: 'Manual switching', agent: 'codex',
      conversation_history_available: true, agent_session_id: 'session-1', terminal_id: 'terminal-1',
    }],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await setConversationFixture(page, { entries: [{ id: 'turn-1', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'manual answer' }], total: 1 });
  await page.getByRole('button', { name: 'Open Manual switching on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Terminal view' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Manual switching' })).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem('herdr_default_agent_view'))).toBe('conversation');
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Conversation history' }).click();
  const historyRequest = (await commands(page)).filter((command) => command.type === 'get_conversation_history').at(-1);
  expect(historyRequest).toBeTruthy();
  await server(page, 0, {
    type: 'command_result',
    request_id: historyRequest!.request_id,
    action: 'get_conversation_history',
    ok: true,
    phase: 'completed',
    data: { available: false, state: 'ready', mode: 'recent', entries: [], has_more: false, total: null, diagnostics: {}, reason: 'Manual transcript unavailable' },
  });
  await expect(page.getByText('Manual transcript unavailable')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Terminal view' })).toBeVisible();
  await setAutoCommands(page, true);
});

test('default agent view: empty and failed pages do not trigger fallback', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'working', project: 'Empty transcript', agent: 'codex',
      conversation_history_available: true, agent_session_id: 'session-1', terminal_id: 'terminal-1',
    }],
  });
  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('group', { name: 'Default View' }).getByRole('button', { name: 'Conversation' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await setConversationFixture(page, { entries: [], total: 0 });
  await page.getByRole('button', { name: 'Open Empty transcript on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByText('No conversation messages have been recorded yet.')).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();

  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Empty transcript on Fedora' }).click();
  const historyRequest = (await commands(page)).filter((command) => command.type === 'get_conversation_history').at(-1);
  expect(historyRequest).toBeTruthy();
  await server(page, 0, {
    type: 'command_result',
    request_id: historyRequest!.request_id,
    action: 'get_conversation_history',
    ok: false,
    phase: 'failed',
    error: 'History read failed',
  });
  await expect(page.getByRole('alert')).toContainText('History read failed');
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Terminal view' })).toBeVisible();
  await setAutoCommands(page, true);
});

test('pane view override: readers can change it while mutation actions stay disabled', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history', 'tab_rename', 'agent_launch'] });
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
  await handshake(page, 0, { capabilities: ['conversation_history', 'tab_rename', 'agent_launch'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'working', project: 'Reader preference', agent: 'codex',
      conversation_history_available: true, agent_session_id: 'session-1', terminal_id: 'terminal-1',
    }],
  });
  await page.getByRole('button', { name: 'Open Reader preference on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Terminal for Reader preference' })).toBeVisible();
  await page.getByRole('button', { name: 'Manage agent' }).click();
  const dialog = page.getByRole('dialog', { name: 'Manage Agent' });
  await expect(dialog.getByRole('combobox', { name: 'Default View' })).toBeEnabled();
  await expect(dialog.getByRole('button', { name: 'Rename Tab' })).toBeDisabled();
  await expect(dialog.getByRole('button', { name: 'Clear Agent' })).toBeDisabled();
  const before = await commands(page);
  await dialog.getByRole('combobox', { name: 'Default View' }).selectOption('conversation');
  expect(await page.evaluate(() => localStorage.getItem('herdr_pane_agent_view_overrides'))).toContain('conversation');
  expect((await commands(page)).slice(before.length).some((command) => String(command.type).startsWith('agent_'))).toBe(false);
  await dialog.getByRole('button', { name: 'Close' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Manage agent' }).click();
  await expect(page.getByRole('dialog', { name: 'Manage Agent' }).getByRole('combobox', { name: 'Default View' })).toHaveValue('conversation');
});

test('shows tool-only agent turns only in full history and decodes their arguments', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_default_agent_view', 'terminal');
    localStorage.setItem('herdr-conversation-view', 'conversation');
  });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: [
      'attention_classification',
      'clear_activities',
      'directory_browser',
      'self_update',
      'structured_questions',
      'slash_commands',
      'conversation_history',
    ],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Tool history',
      agent: 'claude',
      session: 'Current session',
      conversation_history_available: true,
    }],
  });
  // Claude Code records one text-less assistant turn per tool call and
  // serialises the arguments as JSON. Conversation omits that activity; Full
  // history keeps it available for inspection.
  const output = Array.from({ length: 200 }, (_, index) => `line-${index}`).join('\n');
  await setConversationFixture(page, {
    entries: [
      { id: 'turn-1', timestamp: '2026-08-12T09:00:00Z', role: 'user', text: 'run the probe' },
      {
        id: 'turn-2',
        timestamp: '2026-08-12T09:00:01Z',
        role: 'assistant',
        text: '',
        tools: [{
          id: 'tool-1',
          name: 'Bash',
          input: JSON.stringify({
            command: 'python3 /tmp/band-sample.py',
            description: 'List the band',
            timeout: 300000,
          }),
          output,
        }],
      },
      { id: 'turn-3', timestamp: '2026-08-12T09:00:02Z', role: 'assistant', text: 'Probe finished.' },
    ],
    total: 3,
  });

  await page.getByRole('button', { name: 'Open Tool history on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
  await expect(page.getByText('Probe finished.')).toBeVisible();

  const card = page.getByLabel(/^Conversation with/).locator('details').filter({ hasText: 'Bash' });
  await expect(card).toBeHidden();
  await page.getByRole('button', { name: 'Full history' }).click();
  await expect(card).toBeVisible();
  const search = page.getByRole('searchbox', { name: 'Search loaded conversation' });
  await search.fill('Bash');
  await expect(card).toBeVisible();
  await search.fill('');
  await card.locator('summary').click();

  const panels = card.locator('pre');
  await expect(panels.first()).toContainText('command: python3 /tmp/band-sample.py');
  await expect(panels.first()).toContainText('timeout: 300000');
  await expect(panels.first()).not.toContainText('{"command"');

  await expect(panels.nth(1)).not.toContainText('line-199');
  await card.getByRole('button', { name: 'Show all 200 lines' }).click();
  await expect(panels.nth(1)).toContainText('line-199');
  await card.getByRole('button', { name: 'Show less' }).click();
  await expect(panels.nth(1)).not.toContainText('line-199');
});

async function openFollowingConversation(page: Page, mode: 'conversation' | 'activity' = 'activity') {
  await page.addInitScript(value => localStorage.setItem('herdr-conversation-view', value), mode);
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity'] });
  const capabilities = { source: 'pi_events', active_branch: true };
  const entry = (id: string, text: string, role = 'assistant') => ({ id, text, role, timestamp: '' });
  const entries = Array.from({ length: 24 }, (_, index) => entry(`follow:${index}`, `Follow seed ${index}\n\n${'wrapped history '.repeat(index === 0 ? 500 : 12)}`, index % 2 ? 'assistant' : 'user'));
  await setConversationFixture(page, { entries, total: 24, sourceRevision: 'stream:1', sequence: 0, capabilities });
  await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'working', agent: 'pi', project: 'Streaming follow', conversation_history_available: true }] });
  await page.getByRole('button', { name: 'Open Streaming follow on Fedora' }).click();
  const list = page.locator('.conversation-list');
  const gap = () => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight);
  await expect(list.locator('article').last()).toContainText('Follow seed 23');
  await expect.poll(gap).toBeLessThan(2);
  let sequence = 0;
  const deliver = async (incoming: Record<string, unknown>[], remove: string[] = []) => {
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === sequence).length).toBeGreaterThan(0);
    const request = (await commands(page)).findLast(command => command.type === 'get_conversation_updates' && command.after === sequence)!;
    await server(page, 0, { type: 'command_result', request_id: request.request_id, ok: true, phase: 'completed', data: { kind: 'batch', subscription_id: 'fixture-subscription', source_revision: 'stream:1', sequence: ++sequence, capabilities, state: 'generating', entries: incoming, remove } });
  };
  const latestInView = async () => {
    await expect.poll(gap).toBeLessThan(2);
    expect(await list.locator('article').last().evaluate(element => element.getBoundingClientRect().bottom <= element.closest('.conversation-list')!.getBoundingClientRect().bottom + 2)).toBe(true);
  };
  return { list, gap, entry, deliver, latestInView };
}

for (const interaction of ['untouched', 'touch', 'return-to-end'] as const) {
  test(`conversation follow-the-end streams growing partials after ${interaction}`, async ({ page }) => {
    await page.clock.install();
    const { list, entry, deliver, latestInView } = await openFollowingConversation(page);
    await page.clock.runFor(350);
    if (interaction !== 'untouched') {
      await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
      if (interaction === 'return-to-end') {
        await list.evaluate(element => { element.scrollTop -= 140; element.dispatchEvent(new Event('scroll')); });
        await list.evaluate(element => { element.scrollTop = element.scrollHeight - element.clientHeight - 18; element.dispatchEvent(new Event('scroll')); });
        expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
        await scrollConversationToBottom(page);
      }
      await list.dispatchEvent('pointerup', { pointerType: 'touch' });
    }
    for (let index = 1; index <= 12; index++) {
      await deliver([entry('live:following', `${'Streaming paragraph with wrapped words. '.repeat(index * 6)}\n\nSTREAM END ${index}`)]);
      await expect(list.locator('[data-conversation-entry-id="live:following"]')).toContainText(`STREAM END ${index}`);
      await latestInView();
    }
    await deliver([{ ...entry('live:following', ''), tools: [{ id: 'following-tool', name: 'Streaming tool', output: 'First tool line', state: 'running' }] }]);
    const tool = list.locator('[data-conversation-entry-id="live:following"] details');
    await tool.locator('summary').click();
    for (let index = 1; index <= 6; index++) {
      await deliver([{ ...entry('live:following', ''), tools: [{ id: 'following-tool', name: 'Streaming tool', output: 'Tool output wraps on the phone. '.repeat(index * 4), state: 'running' }] }]);
      await expect(tool).toHaveAttribute('open', '');
      await latestInView();
    }
  });
}

for (const heightChange of [-80, 80]) {
  test(`conversation follow-the-end releases on scroll-up during a viewport resize (${heightChange})`, async ({ page }) => {
    const { list, entry, deliver, latestInView } = await openFollowingConversation(page);
    await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
    const reading = await list.evaluate((element, change) => {
      const previousTop = element.scrollTop;
      const previousHeight = element.clientHeight;
      element.style.flex = `0 0 ${previousHeight + change}px`;
      const height = element.clientHeight;
      element.scrollTop = Math.min(previousTop, element.scrollHeight - height) - 120;
      const top = element.scrollTop;
      element.dispatchEvent(new Event('scroll'));
      return { top, height, previousHeight };
    }, heightChange);
    expect(reading.height - reading.previousHeight).toBe(heightChange);
    await list.dispatchEvent('pointerup', { pointerType: 'touch' });
    await list.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    expect(await list.evaluate(element => element.scrollTop)).toBeCloseTo(reading.top, 0);
    await deliver([entry('follow:23', 'Streaming after a simultaneous resize and upward scroll\n\n' + 'more streamed content '.repeat(100))]);
    await expect(list.locator('[data-conversation-entry-id="follow:23"]')).toContainText('Streaming after a simultaneous resize');
    expect(await list.evaluate(element => element.scrollTop)).toBeCloseTo(reading.top, 0);
    await scrollConversationToBottom(page);
    await latestInView();
  });
}

test('conversation follow-the-end leaves selection and restored reading positions alone', async ({ page }) => {
  const { list, gap, entry, deliver, latestInView } = await openFollowingConversation(page);
  await deliver([entry('live:selection', 'Initial selection output')]);
  await expect(list.locator('[data-conversation-entry-id="live:selection"]')).toBeAttached();
  await latestInView();
  const selectedEntry = list.locator('[data-conversation-entry-id="follow:23"]');
  await selectedEntry.locator('.conversation-markdown').evaluate(element => {
    const range = document.createRange();
    range.selectNodeContents(element);
    window.getSelection()?.removeAllRanges();
    window.getSelection()?.addRange(range);
  });
  const selected = await page.evaluate(() => window.getSelection()!.toString());
  expect(selected.length).toBeGreaterThan(0);
  const top = await list.evaluate(element => element.scrollTop);
  await deliver([entry('live:selection', 'Do not move a selection\n\n' + 'more output '.repeat(80))]);
  await expect(list.locator('[data-conversation-entry-id="live:selection"]')).toBeAttached();
  expect(await page.evaluate(() => window.getSelection()!.toString())).toBe(selected);
  expect(await list.evaluate(element => element.scrollTop)).toBeCloseTo(top, 0);
  expect(await gap()).toBeGreaterThan(100);
  await page.evaluate(() => window.getSelection()?.removeAllRanges());
  await scrollConversationToBottom(page);
  await latestInView();
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollTop -= 160; element.dispatchEvent(new Event('scroll')); });
  await list.dispatchEvent('pointerup', { pointerType: 'touch' });
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await page.getByRole('button', { name: 'Full history', exact: true }).click();
  await list.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  const restored = await list.evaluate(element => element.scrollTop);
  await deliver([entry('live:selection', 'Output after view switch\n\n' + 'more output '.repeat(120))]);
  await expect(list.locator('[data-conversation-entry-id="live:selection"]')).toContainText('Output after view switch');
  expect(await list.evaluate(element => element.scrollTop)).toBeCloseTo(restored, 0);
});

for (const mode of ['conversation', 'activity'] as const) {
  test(`conversation follow-the-end advances the bounded ${mode} window and follows layout`, async ({ page }) => {
    test.setTimeout(60_000);
    const { list, entry, deliver, latestInView } = await openFollowingConversation(page, mode);
    for (let index = 1; index <= 6; index++) {
      await deliver([entry('follow:23', `${'Growing recorded answer. '.repeat(index * 12)}\n\nRECORDED END ${index}`)]);
      await expect(list.locator('article').last()).toContainText(`RECORDED END ${index}`);
      await latestInView();
    }
    for (let index = 24; index < 54; index += 2) {
      await deliver([entry(`follow:${index}`, `New prompt ${index}`, 'user'), entry(`follow:${index + 1}`, `New answer ${index + 1}\n\n\`\`\`ts\n${'const line = true;\n'.repeat(index % 8 + 1)}\`\`\``)]);
      await expect(list.locator('article').last()).toContainText(`New answer ${index + 1}`);
      await latestInView();
      expect(await list.locator('article').count()).toBe(24);
    }
    await deliver([{ ...entry('follow:notice', 'Notice at the latest end'), kind: 'notice' }]);
    await expect(list.locator('article').last()).toContainText('Notice at the latest end');
    await latestInView();
    const composer = page.getByRole('combobox', { name: 'Prompt', exact: true });
    await composer.fill('Composer grows\n'.repeat(8));
    await latestInView();
    const size = page.viewportSize()!;
    await page.setViewportSize({ width: size.width, height: size.height - 180 });
    await latestInView();
    await list.locator('article').last().evaluate(async element => {
      const image = document.createElement('img');
      image.alt = 'Late layout fixture';
      image.src = 'data:image/svg+xml,%3Csvg xmlns="http://www.w3.org/2000/svg" width="120" height="320"%3E%3C/svg%3E';
      element.append(image);
      await image.decode();
    });
    await latestInView();
    await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
    await list.evaluate(element => { element.scrollTop -= 180; element.dispatchEvent(new Event('scroll')); });
    await list.dispatchEvent('pointerup', { pointerType: 'touch' });
    const anchor = await list.locator('article').evaluateAll(elements => {
      const top = elements[0].closest('.conversation-list')!.getBoundingClientRect().top;
      const visible = elements.find(element => element.getBoundingClientRect().bottom > top)!;
      return { id: visible.getAttribute('data-conversation-entry-id')!, y: visible.getBoundingClientRect().top };
    });
    await deliver([entry('follow:later', 'Do not pull the reader down')]);
    const retained = list.locator(`[data-conversation-entry-id="${anchor.id}"]`);
    await expect.poll(async () => Math.abs((await retained.boundingBox())!.y - anchor.y)).toBeLessThanOrEqual(2);
    await scrollConversationToBottom(page);
    await deliver([entry('follow:resumed', 'Following again')]);
    await expect(list.locator('article').last()).toContainText('Following again');
    await latestInView();
  });
}

test('opens a long conversation at its newest turn and holds the pin', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  // The view polls for new turns every five seconds. Shortening that interval
  // keeps the streaming assertions below deterministic instead of sleeping
  // through real intervals, exactly as boot() does for reconnect timers.
  await page.addInitScript(() => {
    const nativeSetInterval = window.setInterval.bind(window);
    window.setInterval = ((handler: TimerHandler, timeout?: number, ...args: unknown[]) =>
      nativeSetInterval(handler, timeout === 5000 ? 60 : timeout, ...args)) as typeof window.setInterval;
  });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: [
      'attention_classification',
      'structured_questions',
      'slash_commands',
      'conversation_history',
    ],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Long conversation',
      agent: 'claude',
      session: 'Current session',
      conversation_history_available: true,
    }],
  });

  // Tables and fenced code are the reported trigger: they lay out taller than
  // the frame that first measures the list, so a scroll written once against
  // an early scrollHeight lands short of the end (issue #12).
  const heavyTurn = (index: number) => [
    `### Turn ${index} heading`,
    '',
    `Prose that wraps several times on a phone: ${'detail '.repeat(24).trim()}.`,
    '',
    '| Field | Value |',
    '| --- | --- |',
    `| index | ${index} |`,
    `| note | ${'wide cell content '.repeat(6).trim()} |`,
    '',
    '```ts',
    ...Array.from({ length: 6 }, (_, line) => `const value${line} = compute(${index}, ${line});`),
    '```',
  ].join('\n');
  const entries = Array.from({ length: 60 }, (_, index) => ({
    id: `turn-${index + 1}`,
    timestamp: `2026-08-12T09:${String(index).padStart(2, '0')}:00Z`,
    role: index % 2 ? 'assistant' : 'user',
    text: index % 2 ? heavyTurn(index + 1) : `question ${index + 1}`,
  }));
  await setConversationFixture(page, { entries, total: entries.length });

  await page.getByRole('button', { name: 'Open Long conversation on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  const list = page.locator('.conversation-list');
  const bottomGap = () => list.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight);

  await expect(page.getByText('Turn 60 heading')).toBeVisible();
  // A pin assertion is only meaningful while the transcript really overflows.
  expect(await list.evaluate((element) => element.scrollHeight > element.clientHeight * 3)).toBe(true);
  await expect.poll(bottomGap).toBeLessThan(2);

  // Turns arriving while the reader sits at the end keep the end in view.
  const streamed = [...entries, {
    id: 'turn-61', timestamp: '2026-08-12T10:00:00Z', role: 'user', text: 'newest streamed question',
  }];
  await setConversationFixture(page, { entries: streamed, total: streamed.length });
  await expect(page.getByText('newest streamed question')).toBeVisible();
  await expect.poll(bottomGap).toBeLessThan(2);

  // Reading history wins over the pin: later turns must not yank the view.
  await list.evaluate(async (element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
    await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
  });
  const readingTop = await list.evaluate(element => element.scrollTop);
  const readingAnchor = await list.locator('article').first().getAttribute('data-conversation-entry-id');
  const later = [...streamed, {
    id: 'turn-62', timestamp: '2026-08-12T10:01:00Z', role: 'user', text: 'later streamed question',
  }];
  await setConversationFixture(page, { entries: later, total: later.length });
  await expect(page.getByText('later streamed question')).toHaveCount(0);
  await expect(page.locator('.conversation-source-status').filter({ hasText: 'of 62 loaded messages.' })).toBeAttached();
  await expect(list.locator('article').first()).toHaveAttribute('data-conversation-entry-id', readingAnchor!);
  expect(await list.evaluate(element => element.scrollTop)).toBeCloseTo(readingTop, 0);
  expect(await bottomGap()).toBeGreaterThan(200);

  // Scrolling back to the end restores the pin.
  await scrollConversationToBottom(page);
  const final = [...later, {
    id: 'turn-63', timestamp: '2026-08-12T10:02:00Z', role: 'user', text: 'final streamed question',
  }];
  await setConversationFixture(page, { entries: final, total: final.length });
  await expect(page.getByText('final streamed question')).toBeVisible();
  await expect.poll(bottomGap).toBeLessThan(2);
});

test('loads older conversation automatically when scrolled near the top', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'structured_questions', 'slash_commands', 'conversation_history'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Automatic history',
      agent: 'codex',
      server_session_id: 'session-1',
      terminal_id: 'terminal-1',
      generation: 1,
      conversation_history_available: true,
      agent_session_id: 'automatic-session',
    }],
  });
  const latestEntries = Array.from({ length: 100 }, (_, index) => [
    { id: `latest-user-${index}`, timestamp: `2026-08-12T09:${String(index).padStart(2, '0')}:00Z`, role: 'user', text: `latest question ${index}` },
    { id: `latest-answer-${index}`, timestamp: `2026-08-12T09:${String(index).padStart(2, '0')}:01Z`, role: 'assistant', text: `latest answer ${index}` },
  ]).flat();
  const olderEntries = Array.from({ length: 12 }, (_, index) => [
    { id: `older-user-${index}`, timestamp: `2026-08-12T08:${String(index).padStart(2, '0')}:00Z`, role: 'user', text: `older automatic question ${index}` },
    { id: `older-answer-${index}`, timestamp: `2026-08-12T08:${String(index).padStart(2, '0')}:01Z`, role: 'assistant', text: `older automatic answer ${index}` },
  ]).flat();
  await setConversationFixture(page, {
    entries: latestEntries,
    total: latestEntries.length + olderEntries.length,
    nextCursor: 'older-1',
    hasMore: true,
    pages: { 'older-1': { entries: olderEntries } },
  });

  await page.getByRole('button', { name: 'Open Automatic history on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  const list = page.locator('.conversation-list');
  await expect(page.getByText('latest answer 99')).toBeVisible();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'get_conversation_history').length).toBe(1);
  expect(await list.evaluate((element) => element.scrollHeight > element.clientHeight)).toBe(true);

  await list.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll', { bubbles: true }));
  });
  await showOlderConversationEntry(page, 'older-user-0');
  await expect(page.getByText('older automatic answer 11')).toBeAttached();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'get_conversation_history' && command.cursor === 'older-1').length).toBe(1);
  await expect(page.getByText('Beginning of conversation reached.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Return to latest' })).toHaveCount(0);

  const readingTop = await list.evaluate((element) => element.scrollTop);
  await setConversationFixture(page, {
    entries: [...latestEntries.slice(-199), {
      id: 'live-reply', timestamp: '2026-08-12T12:00:00Z', role: 'user', text: 'live message while reading history',
    }],
    total: latestEntries.length + olderEntries.length + 1,
  });
  await expect(page.locator('.conversation-source-status').filter({ hasText: 'of 225 loaded messages.' })).toBeAttached({ timeout: 10_000 });
  await expect(page.getByText('live message while reading history')).toHaveCount(0);
  await expect(page.getByText('older automatic question 0', { exact: true })).toBeAttached();
  expect(await list.locator('article').count()).toBeLessThanOrEqual(24);
  expect(await list.evaluate((element) => element.scrollTop)).toBeCloseTo(readingTop, 0);

  await scrollConversationToBottom(page);
  await expect(page.getByText('live message while reading history')).toBeVisible();
  expect(await list.locator('article').count()).toBeLessThanOrEqual(24);
});

test('retains a Claude continuation cursor through repeated preparation', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['attention_classification', 'conversation_history'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      status: 'working',
      project: 'Claude continuation',
      agent: 'claude',
      conversation_history_available: true,
      agent_session_id: 'claude-chain-session',
    }],
  });
  const segmentBEntries = Array.from({ length: 24 }, (_, index) => ({
    id: `segment-b-${index}`,
    timestamp: `2026-09-02T11:${String(index).padStart(2, '0')}:00Z`,
    role: index % 2 ? 'assistant' : 'user',
    text: `complete child history ${index}`,
  }));
  const segmentAEntries = Array.from({ length: 24 }, (_, index) => ({
    id: `segment-a-${index}`,
    timestamp: `2026-09-02T10:${String(index).padStart(2, '0')}:00Z`,
    role: index % 2 ? 'assistant' : 'user',
    text: `complete parent history ${index}`,
  }));
  await setConversationFixture(page, {
    entries: [{ id: 'latest', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'latest continuation answer' }],
    total: 49,
    nextCursor: 'chain-prepare-b',
    hasMore: true,
    diagnostics: {
      source_truncated: false, corrupt_records: 0, oversized_records: 0,
      continuation_incomplete: true, continuation_reason: 'missing_source',
    },
    pages: {
      'chain-prepare-b': {
        entries: [], state: 'preparing', snapshotId: 'chain-snapshot', nextCursor: 'chain-ready-b', hasMore: true,
      },
      'chain-ready-b': {
        entries: segmentBEntries,
        state: 'ready', snapshotId: 'chain-snapshot', nextCursor: 'chain-prepare-a', hasMore: true,
      },
      'chain-prepare-a': {
        entries: [], state: 'preparing', snapshotId: 'chain-snapshot', nextCursor: 'chain-ready-a', hasMore: true,
      },
      'chain-ready-a': {
        entries: segmentAEntries,
        state: 'ready', snapshotId: 'chain-snapshot', hasMore: false,
      },
    },
  });
  await page.getByRole('button', { name: 'Open Claude continuation on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await page.getByRole('button', { name: 'Full history' }).click();
  const list = page.locator('.conversation-list');
  await expect(page.getByText('latest continuation answer')).toBeVisible();
  await list.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll', { bubbles: true }));
  });
  await page.getByRole('button', { name: 'Load older turns', exact: true }).click();
  await showOlderConversationEntry(page, 'segment-b-0');
  await expect(page.getByText('complete child history 0')).toHaveCount(1);
  await expect(page.getByText('complete child history 23')).toHaveCount(1);
  await showOlderConversationEntry(page, 'segment-a-0');
  await expect(page.getByText('complete parent history 0')).toHaveCount(1);
  await expect(page.getByText('complete parent history 23')).toHaveCount(1);
  await expect(page.getByRole('status').filter({ hasText: 'part of that history is unavailable' })).toBeVisible();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'get_conversation_history')
    .map((command) => command.cursor || '')).toEqual(['', 'chain-prepare-b', 'chain-ready-b', 'chain-prepare-a', 'chain-ready-a']);
  await setConversationFixture(page, {
    entries: [
      { id: 'latest', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'latest continuation answer' },
      { id: 'recovered', timestamp: '2026-09-02T12:01:00Z', role: 'assistant', text: 'appended C answer' },
    ],
    total: 2,
    diagnostics: { source_truncated: false, corrupt_records: 0, oversized_records: 0 },
  });
  await page.getByRole('button', { name: 'Reload history' }).click();
  await expect(page.getByText('appended C answer')).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'part of that history is unavailable' })).toBeHidden();
});

test('accepts the first Claude continuation and rejects a replacement cursor', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'working', project: 'Claude acceptance', agent: 'claude',
      conversation_history_available: true, agent_session_id: 'claude-acceptance-session',
    }],
  });
  const segmentA = Array.from({ length: 24 }, (_, index) => ({
    id: `accept-a-${index}`,
    timestamp: `2026-09-02T10:${String(index).padStart(2, '0')}:00Z`,
    role: index % 2 ? 'assistant' : 'user',
    text: `A continuation history ${index}`,
  }));
  const segmentB = Array.from({ length: 24 }, (_, index) => ({
    id: `accept-b-${index}`,
    timestamp: `2026-09-02T11:${String(index).padStart(2, '0')}:00Z`,
    role: index % 2 ? 'assistant' : 'user',
    text: `B continuation history ${index}`,
  }));
  await setConversationFixture(page, {
    entries: segmentA,
    total: segmentA.length,
    sourceRevision: 'revision-a',
    diagnostics: {
      source_truncated: false, corrupt_records: 0, oversized_records: 0,
      continuation_incomplete: true, continuation_reason: 'missing_source',
    },
  });
  await page.getByRole('button', { name: 'Open Claude acceptance on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await page.getByRole('button', { name: 'Full history' }).click();
  await expect(page.getByText('A continuation history 23')).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'part of that history is unavailable' })).toBeVisible();

  await setConversationFixture(page, {
    entries: [...segmentA, ...segmentB],
    total: segmentA.length + segmentB.length,
    nextCursor: 'replacement-cursor',
    hasMore: true,
    sourceRevision: 'revision-a',
    diagnostics: { source_truncated: false, corrupt_records: 0, oversized_records: 0 },
    pages: {
      'replacement-cursor': {
        entries: [{ id: 'replacement', timestamp: '2026-09-02T12:00:00Z', role: 'assistant', text: 'replacement history must not enter the old lane' }],
        sourceRevision: 'revision-replacement',
        hasMore: false,
      },
    },
  });
  await page.getByRole('button', { name: 'Reload history' }).click();
  await expect(page.getByText('B continuation history 23')).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'part of that history is unavailable' })).toBeHidden();

  const list = page.locator('.conversation-list');
  await list.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll', { bubbles: true }));
  });
  await showOlderConversationEntry(page, 'accept-a-0');
  await page.getByRole('button', { name: 'Load older turns', exact: true }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'get_conversation_history' && command.cursor === 'replacement-cursor').length).toBe(1);
  await expect(page.getByRole('alert').filter({ hasText: 'The conversation source changed while history was being browsed.' })).toBeVisible();
  await scrollConversationToBottom(page);
  await expect(page.getByText('B continuation history 23')).toBeVisible();
  await expect(page.getByText('replacement history must not enter the old lane')).toBeHidden();
});

for (const availability of ['available', 'no-capability', 'no-directory'] as const) {
  test(`Conversation workspace button is compact and ${availability}`, async ({ page }) => {
    await page.setViewportSize({ width: 360, height: 740 });
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0, { capabilities: ['conversation_history', ...(availability !== 'no-capability' ? ['workspace_inspection'] : [])] });
    await setConversationFixture(page, { entries: [{ id: 'workspace:answer', timestamp: '', role: 'assistant', text: 'Workspace conversation' }], total: 1 });
    await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'idle', project: 'Conversation workspace with a long title', agent: 'pi', cwd: availability === 'no-directory' ? '' : '/work/mobile', conversation_history_available: true }] });
    await page.getByRole('button', { name: 'Open Conversation workspace with a long title on Fedora' }).click();
    await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
    const header = page.locator('.app-header');
    const inspect = header.getByRole('button', { name: 'Inspect workspace', exact: true });
    await expect(inspect).toBeVisible();
    for (const name of ['Terminal view', 'Inspect workspace', 'Manage agent']) {
      const box = await header.getByRole('button', { name, exact: true }).boundingBox();
      expect(box!.x).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width).toBeLessThanOrEqual(360);
    }
    expect((await header.boundingBox())!.height).toBeLessThanOrEqual(50);
    if (availability !== 'available') {
      await expect(inspect).toBeDisabled();
      await expect(inspect).toHaveAttribute('title', 'Workspace inspection is unavailable');
      return;
    }
    await expect(inspect).toBeEnabled();
    await expect(inspect).toHaveAttribute('title', 'Inspect workspace files and Git changes');
    await inspect.click();
    const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
    await expect(dialog).toBeVisible();
    await dialog.locator('aside').getByRole('button', { name: 'README.md', exact: true }).click();
    await expect(dialog.getByRole('textbox', { name: 'Contents of README.md' })).toHaveValue('# Workspace preview\n\nRead-only file contents.');
    await dialog.getByRole('tab', { name: /Changes/ }).click();
    await dialog.locator('aside').getByRole('button', { name: /README\.md/ }).click();
    await expect(dialog.getByLabel('Diff for README.md')).toContainText('+Read-only change');
  });
}

test('inspects workspace files and Git changes without write controls', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: [
      'attention_classification',
      'clear_activities',
      'directory_browser',
      'self_update',
      'structured_questions',
      'slash_commands',
      'workspace_inspection',
    ],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1',
      workspace_id: 'workspace-mobile',
      status: 'working',
      project: 'Workspace app',
      agent: 'codex',
      cwd: '/work/mobile',
    }],
  });

  await page.getByRole('button', { name: 'Open Workspace app on Fedora' }).click();
  await page.getByRole('button', { name: 'Inspect workspace' }).click();
  const dialog = page.getByRole('dialog', { name: 'Workspace' });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByText('/work/mobile', { exact: true })).toBeVisible();
  await expect(dialog.getByText('feature/mobile', { exact: true })).toBeVisible();
  const sectionTabsBox = await dialog.getByRole('tablist', { name: 'Workspace sections' }).boundingBox();
  const branchBox = await dialog.getByText('feature/mobile', { exact: true }).boundingBox();
  expect(sectionTabsBox).not.toBeNull();
  expect(branchBox).not.toBeNull();
  expect(Math.abs(
    sectionTabsBox!.y + sectionTabsBox!.height / 2
    - (branchBox!.y + branchBox!.height / 2),
  )).toBeLessThan(2);

  await dialog.locator('aside').getByRole('button', { name: 'README.md', exact: true }).click();
  await expect(dialog.getByRole('textbox', { name: 'Contents of README.md' }))
    .toHaveValue('# Workspace preview\n\nRead-only file contents.');

  await dialog.getByRole('tab', { name: /Changes/ }).click();
  await dialog.locator('aside').getByRole('button', { name: /README\.md/ }).click();
  const diff = dialog.getByLabel('Diff for README.md');
  await expect(diff).toContainText('diff --git a/README.md b/README.md');
  await expect(diff.locator('.diff-hunk')).toHaveText('@@ -1 +1 @@');
  await expect(diff.locator('.diff-deletion')).toHaveText('-Old text');
  await expect(diff.locator('.diff-addition')).toHaveText('+Read-only change');
  const initialDiffFontSize = await diff.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize));
  await diff.dispatchEvent('pointerdown', {
    pointerType: 'touch', pointerId: 11, isPrimary: true, clientX: 80, clientY: 180,
  });
  await diff.dispatchEvent('pointerdown', {
    pointerType: 'touch', pointerId: 12, isPrimary: false, clientX: 180, clientY: 180,
  });
  await diff.dispatchEvent('pointermove', {
    pointerType: 'touch', pointerId: 12, isPrimary: false, clientX: 240, clientY: 180,
  });
  await diff.dispatchEvent('pointerup', {
    pointerType: 'touch', pointerId: 12, isPrimary: false, clientX: 240, clientY: 180,
  });
  await diff.dispatchEvent('pointerup', {
    pointerType: 'touch', pointerId: 11, isPrimary: true, clientX: 80, clientY: 180,
  });
  await expect.poll(() => diff.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize)))
    .toBeGreaterThan(initialDiffFontSize * 1.4);
  const changedFiles = dialog.locator('aside');
  await changedFiles.dispatchEvent('pointerdown', {
    pointerType: 'touch', pointerId: 7, isPrimary: true, clientX: 150, clientY: 220,
  });
  await changedFiles.dispatchEvent('pointerup', {
    pointerType: 'touch', pointerId: 7, isPrimary: true, clientX: 40, clientY: 224,
  });
  await expect(changedFiles).toHaveCount(0);
  const showChanges = dialog.getByRole('button', { name: 'Show changed-file list' });
  await expect(showChanges).toBeVisible();
  await showChanges.click();
  await expect(dialog.locator('aside')).toBeVisible();

  await expect(dialog.getByRole('button', { name: /save|write|apply|commit/i })).toHaveCount(0);

  await expect.poll(async () => (await commands(page))
    .map((command) => String(command.type))
    .filter((type) => type.startsWith('workspace_'))
    .sort()).toEqual([
    'workspace_file',
    'workspace_git_diff',
    'workspace_git_status',
    'workspace_tree',
  ]);
});

test('discovers slash commands per terminal and fills them before sending', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [
      { pane_id: 'w1:p1', status: 'working', project: 'Codex app', agent: 'codex', cwd: '/home/test/codex', agent_session_id: 'slash-codex-session' },
      { pane_id: 'w1:p2', status: 'idle', project: 'Claude app', agent: 'claude', cwd: '/home/test/claude', agent_session_id: 'slash-claude-session' },
    ],
  });

  await page.getByRole('button', { name: 'Search all agents' }).click();
  const jump = page.getByRole('dialog', { name: 'Jump to agent' });
  await jump.getByRole('searchbox', { name: 'Search agents and workspaces' }).fill('Codex app');
  await jump.getByRole('button', { name: /Codex app/ }).click();
  const codexComposer = page.getByRole('combobox', { name: 'Prompt' });
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'read_pane').length).toBeGreaterThan(0);
  const slashRead = (await commands(page)).findLast(command => command.type === 'read_pane')!;
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: slashRead.request_id, target: slashRead.target, format: 'plain', content: 'Ready for normal input', no_echo: false });
  await expect(page.getByRole('log')).toContainText('Ready for normal input');
  const restingComposerBox = await codexComposer.boundingBox();
  await codexComposer.fill('/');
  const popover = page.getByRole('region', { name: 'Command suggestions' });
  await expect(popover).toBeVisible();
  const [popoverBox, composerBox, viewport] = await Promise.all([
    popover.boundingBox(),
    codexComposer.boundingBox(),
    page.evaluate(() => ({ width: innerWidth, height: innerHeight })),
  ]);
  expect(restingComposerBox).not.toBeNull();
  expect(popoverBox).not.toBeNull();
  expect(composerBox).not.toBeNull();
  expect(composerBox!.y).toBeCloseTo(restingComposerBox!.y, 0);
  expect(composerBox!.height).toBeCloseTo(restingComposerBox!.height, 0);
  expect(popoverBox!.y + popoverBox!.height).toBeLessThan(composerBox!.y);
  expect(popoverBox!.height).toBeLessThanOrEqual(viewport.height * 0.5);
  await expect(popover.getByRole('option')).toHaveCount(22);
  const description = popover.getByText('Show the full command reference and explain every available action');
  await expect(description).toBeVisible();
  expect(await description.evaluate((element) => ({
    overflow: getComputedStyle(element).overflow,
    textOverflow: getComputedStyle(element).textOverflow,
    whiteSpace: getComputedStyle(element).whiteSpace,
  }))).toEqual({ overflow: 'visible', textOverflow: 'clip', whiteSpace: 'normal' });
  expect(await page.getByRole('listbox', { name: 'Slash commands' }).evaluate((element) => (
    element.scrollHeight > element.clientHeight && getComputedStyle(element).overflowY === 'auto'
  ))).toBe(true);

  await codexComposer.fill('/pl');
  const menu = page.getByRole('listbox', { name: 'Slash commands' });
  await expect(menu).toBeVisible();
  await expect(menu.getByRole('option', { name: /\/plan/ })).toBeVisible();
  await expect(menu.getByRole('option', { name: /\/model/ })).toBeHidden();
  await menu.getByRole('option', { name: /\/plan/ }).click();
  await expect(codexComposer).toHaveValue('/plan ');
  expect((await commands(page)).filter((command) => command.type === 'submit_prompt')).toHaveLength(0);
  await codexComposer.pressSequentially('Review the release');
  await page.getByRole('button', { name: 'Send prompt' }).click();
  expect((await commands(page)).find((command) => command.type === 'submit_prompt')).toMatchObject({
    pane_id: 'w1:p1', text: '/plan Review the release',
  });

  await page.getByRole('button', { name: 'Back' }).click();
  await page.getByRole('button', { name: 'Search all agents' }).click();
  await jump.getByRole('searchbox', { name: 'Search agents and workspaces' }).fill('Claude app');
  await jump.getByRole('button', { name: /Claude app/ }).click();
  const claudeComposer = page.getByRole('combobox', { name: 'Prompt' });
  await claudeComposer.fill('/he');
  await expect(page.getByRole('option', { name: /\/help/ })).toBeVisible();
  await claudeComposer.press('Enter');
  await expect(claudeComposer).toHaveValue('/help');
  expect((await commands(page)).filter((command) => command.type === 'list_slash_commands')).toHaveLength(2);
  expect((await commands(page)).filter((command) => command.type === 'submit_prompt')).toHaveLength(1);
});

test('keeps a large slash catalog responsive in the installed PWA', async ({ page }) => {
  await emulateStandalone(page, { navigatorStandalone: true });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Large catalog app', agent: 'codex', cwd: '/home/test/large' }],
  });
  await page.getByRole('button', { name: 'Open Large catalog app on Fedora' }).click();
  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await setAutoCommands(page, false);
  await composer.fill('/');
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'list_slash_commands').length).toBe(1);
  const catalogRequest = (await commands(page)).find(command => command.type === 'list_slash_commands')!;
  const catalog: Record<string, unknown>[] = Array.from({ length: 4096 }, (_, index) => ({
    command: `/catalog-${String(index).padStart(4, '0')}`,
    description: 'D'.repeat(240),
    argument_hint: 'H'.repeat(120),
    source: 'personal',
  }));
  catalog[350] = { command: '/late-command', description: 'Late command', argument_hint: 'H'.repeat(120), source: 'personal' };
  await server(page, 0, { type: 'command_result', request_id: catalogRequest.request_id, ok: true, phase: 'completed', data: { commands: catalog, truncated: false } });
  const popover = page.getByRole('region', { name: 'Command suggestions' });
  await expect(popover).toBeVisible();
  await expect(popover.getByRole('option')).toHaveCount(200);
  await expect(popover.getByText('200+ matching', { exact: true })).toBeVisible();
  await expect(popover.getByText('More matching commands are hidden; keep typing to narrow the list.', { exact: true })).toBeVisible();
  await page.keyboard.press('ArrowUp');
  await expect(composer).toHaveAttribute('aria-activedescendant', 'slash-command-option-199');
  await page.keyboard.press('Enter');
  await expect(composer).toHaveValue('/catalog-0199 ');
  expect((await commands(page)).filter((command) => command.type === 'submit_prompt')).toHaveLength(0);

  const filterStarted = await page.evaluate(() => performance.now());
  await composer.fill('/late');
  await expect(popover.getByRole('option', { name: /\/late-command/ })).toBeVisible();
  await expect(popover.getByRole('option')).toHaveCount(1);
  const filterMs = await page.evaluate((started) => performance.now() - started, filterStarted);
  console.log(`large slash catalog filter: ${filterMs.toFixed(2)}ms`);
  expect(filterMs).toBeLessThan(1_000);
  await popover.getByRole('option', { name: /\/late-command/ }).click();
  await expect(composer).toHaveValue('/late-command ');
  expect((await commands(page)).filter((command) => command.type === 'submit_prompt')).toHaveLength(0);

  const loadedVersion = await page.evaluate(async () => (await fetch('/version.json')).json());
  expect(loadedVersion.assets).toBe(APP_METADATA.assets);
  expect(loadedVersion.build).toBe(APP_METADATA.build);
});

test('scales and expands the prompt composer for multiline text', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Composer app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Composer app on Fedora' }).click();

  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await composer.fill('One line');
  const compactHeight = (await composer.boundingBox())!.height;
  await composer.fill('Line one\nLine two\nLine three\nLine four');
  await expect.poll(async () => (await composer.boundingBox())!.height).toBeGreaterThan(compactHeight + 20);

  await composer.fill(Array.from({ length: 30 }, (_, index) => `Line ${index + 1}`).join('\n'));
  expect(await composer.evaluate((element) => {
    const textarea = element as HTMLTextAreaElement;
    return textarea.scrollHeight > textarea.clientHeight && getComputedStyle(textarea).overflowY === 'auto';
  })).toBe(true);

  await page.getByRole('button', { name: 'Settings' }).click();
  const sizes = page.getByRole('group', { name: 'Interface Size' });
  await sizes.getByRole('button', { name: 'Regular' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await composer.fill('One line');
  const regularHeight = (await composer.boundingBox())!.height;

  await page.getByRole('button', { name: 'Settings' }).click();
  await sizes.getByRole('button', { name: 'Large' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await composer.fill('One line');
  const largeHeight = (await composer.boundingBox())!.height;

  expect(regularHeight).toBeGreaterThan(compactHeight);
  expect(largeHeight).toBeGreaterThan(regularHeight);
});

test('live conversation deltas preserve local reading, selection, tool expansion and table offsets', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity'] });
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'fixture-terminal', generation: 1, agent_session_id: 'fixture-session', status: 'working', project: 'Live reading', agent: 'pi', cwd: '/fixture', conversation_history_available: true }] });
  const capabilities = { source: 'pi_events', active_branch: true };
  const entries = Array.from({ length: 100 }, (_, index) => ({ id: `pi:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Paragraph ${index}: ${'stable historical reading '.repeat(12)}` }));
  entries[99].text = '| Field | Wide value |\n| --- | --- |\n| Fixture | ' + 'wide '.repeat(90) + ' |';
  const toolEntry = { ...entries[99], tools: [{ id: 'tool', name: 'Fixture tool', input: 'synthetic input', output: 'synthetic output' }] };
  await setConversationFixture(page, { entries: [...entries.slice(0, 99), toolEntry], total: 100, sourceRevision: 'live-source:1', capabilities, sequence: 0 });
  await page.getByRole('button', { name: 'Open Live reading on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await page.getByRole('button', { name: 'Full history' }).click();
  await expect(page.locator('.conversation-page > .conversation-source-status')).toHaveCount(0);
  await expect(page.locator('.conversation-toolbar .conversation-branch-trust')).toHaveCount(0);
  await expect(page.locator('[data-conversation-entry-id="pi:99"] > small')).toHaveCount(0);
  const list = page.locator('.conversation-list');
  const tool = list.locator('details').filter({ hasText: 'Fixture tool' });
  await tool.locator('summary').click();
  await expect(tool).toHaveAttribute('open', '');
  const table = list.locator('.conversation-table');
  await table.evaluate(element => { element.scrollLeft = 180; });
  const horizontal = await table.evaluate(element => element.scrollLeft);
  await list.evaluate(element => { element.scrollTop = element.scrollHeight / 2; element.dispatchEvent(new Event('scroll')); });
  const paragraph = list.locator('.conversation-entry').filter({ hasText: 'Paragraph 90:' });
  const anchorBefore = await paragraph.evaluate(element => element.getBoundingClientRect().top);
  await paragraph.evaluate(element => { const selection = window.getSelection(); const range = document.createRange(); range.selectNodeContents(element.querySelector('.conversation-message') || element); selection?.removeAllRanges(); selection?.addRange(range); });
  const selected = await page.evaluate(() => window.getSelection()?.toString());
  const baseline = (await commands(page)).length;
  for (const status of ['idle', 'working', 'done']) {
    await server(page, 0, { type: 'agent_update', pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'fixture-terminal', generation: 1, agent_session_id: 'fixture-session', status });
    expect(Math.abs(await paragraph.evaluate(element => element.getBoundingClientRect().top) - anchorBefore)).toBeLessThanOrEqual(2);
    expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(selected);
    await expect(tool).toHaveAttribute('open', '');
    expect(await table.evaluate(element => element.scrollLeft)).toBe(horizontal);
  }
  const deliver = async (after: number, batch: Record<string, unknown>) => {
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).length).toBeGreaterThan(0);
    const request = (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).at(-1)!;
    await server(page, 0, { type: 'command_result', action: 'get_conversation_updates', request_id: request.request_id, ok: true, phase: 'completed', data: { subscription_id: 'fixture-subscription', kind: 'batch', source_revision: 'live-source:1', sequence: after + 1, entries: [], remove: [], state: 'generating', capabilities, ...batch } });
  };
  await deliver(0, { entries: [{ id: 'live:message', role: 'assistant', text: 'First partial', timestamp: '' }] });
  await expect(list.getByText('First partial', { exact: true })).toHaveCount(0);
  await expect(page.locator('.conversation-page > .conversation-source-status')).toHaveCount(0);
  await deliver(1, { entries: [{ id: 'live:message', role: 'assistant', text: 'Second partial', timestamp: '' }] });
  await expect(list.getByText('Second partial', { exact: true })).toHaveCount(0);
  expect(Math.abs(await paragraph.evaluate(element => element.getBoundingClientRect().top) - anchorBefore)).toBeLessThanOrEqual(2);
  expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(selected);
  await expect(tool).toHaveAttribute('open', '');
  expect(await table.evaluate(element => element.scrollLeft)).toBe(horizontal);
  await page.evaluate(() => window.getSelection()?.removeAllRanges());
  await scrollConversationToBottom(page);
  await expect(list.getByText('Second partial', { exact: true })).toBeVisible();
  await expect(list.locator('[data-conversation-entry-id="live:message"] > small')).toHaveText('Live · not saved yet');
  await deliver(2, { entries: [{ id: 'live:tools', role: 'assistant', text: '', timestamp: '', tools: [{ id: 'call', name: 'Progress tool', input: '{"path":"live-argument"}', output: 'progress one', state: 'running' }] }] });
  const progress = list.locator('details').filter({ hasText: 'Progress tool' });
  await expect(progress.locator('summary')).toContainText('running');
  await progress.locator('summary').click();
  await expect(progress.getByText('progress one', { exact: true })).toBeVisible();
  const liveSearch = page.getByRole('searchbox', { name: 'Search loaded conversation' });
  for (const query of ['Progress tool', 'live-argument', 'progress one']) {
    await liveSearch.fill(query);
    await expect(progress).toBeVisible();
  }
  await deliver(3, { entries: [{ id: 'live:tools', role: 'assistant', text: '', timestamp: '', tools: [{ id: 'call', name: 'Progress tool', input: '{"path":"live-argument"}', output: 'progress two', state: 'failed', error: true }] }] });
  await expect(progress).toBeHidden();
  await liveSearch.fill('progress two');
  await expect(progress).toBeVisible();
  await liveSearch.fill('no matching live tool');
  await expect(progress).toBeHidden();
  await liveSearch.fill('');
  await expect(progress.locator('summary')).toContainText('failed');
  await expect(progress).toHaveAttribute('open', '');
  await list.evaluate(element => { element.scrollTop = element.scrollHeight / 2; element.dispatchEvent(new Event('scroll')); });
  await deliver(4, { entries: [{ id: 'pi:final', role: 'assistant', text: 'Final response', timestamp: '' }], remove: ['live:message', 'live:tools'], state: 'ready' });
  await expect(list.getByText('Second partial', { exact: true })).toBeHidden();
  await expect(list.getByText('Final response', { exact: true })).toHaveCount(1);
  const emitted = (await commands(page)).slice(baseline);
  expect(emitted.filter(command => ['send_keys', 'send_input', 'lease_pane_size'].includes(String(command.type)))).toEqual([]);
  expect((await commands(page)).filter(command => command.type === 'get_conversation_history')).toHaveLength(1);
  await page.evaluate(() => window.getSelection()?.removeAllRanges());
  await scrollConversationToBottom(page);
  await expect.poll(() => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  await closeSocket(page, 0);
  await expect(list.getByText('Final response', { exact: true })).toHaveCount(1);
  const firstBeforeScroll = await list.locator('article').first().getAttribute('data-conversation-entry-id');
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
  await expect(list.locator('article').first()).not.toHaveAttribute('data-conversation-entry-id', firstBeforeScroll!);
});

test('loaded windows retain tool expansion and table offsets without leaking state to another target', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  const target = { pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'terminal-1', generation: 1, agent_session_id: 'state-session', status: 'working', project: 'Window A', agent: 'pi', cwd: '/fixture', conversation_history_available: true };
  await server(page, 0, { type: 'agents', agents: [target, { ...target, pane_id: 'w1:p2', terminal_id: 'terminal-2', agent_session_id: 'other-session', project: 'Window B' }] });
  const entries = Array.from({ length: 240 }, (_, index) => ({ id: `pi:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Window message ${index}` }));
  const tableText = '| Field | Wide value |\n| --- | --- |\n| Fixture | ' + 'wide '.repeat(90) + ' |';
  const content = entries.map((entry, index) => index === 220 ? { ...entry, text: tableText, tools: [{ id: 'tool', name: 'Window tool', input: '{}', output: 'done' }] } : entry);
  await setConversationFixture(page, { entries: content.slice(40), total: 240, sourceRevision: 'state:1', hasMore: true, nextCursor: 'older-state', pages: { 'older-state': { entries: content.slice(0, 40) } } });
  await page.getByRole('button', { name: 'Open Window A on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await page.getByRole('button', { name: 'Full history' }).click();
  const list = page.locator('.conversation-list');
  await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
  await showOlderConversationEntry(page, 'pi:0');
  await expect(page.locator('.conversation-source-status').filter({ hasText: 'of 240 loaded messages.' })).toBeAttached();
  await scrollConversationToBottom(page);
  const article = page.locator('[data-conversation-entry-id="pi:220"]');
  const tool = article.locator('details');
  await tool.locator('summary').click();
  await expect(tool).toHaveAttribute('open', '');
  const table = article.locator('.conversation-table');
  await table.evaluate(element => { element.scrollLeft = 180; element.dispatchEvent(new Event('scroll')); });
  const offset = await table.evaluate(element => element.scrollLeft);
  expect(offset).toBeGreaterThan(0);
  await page.getByRole('button', { name: 'Show earlier loaded messages', exact: true }).evaluate(element => (element as HTMLElement).click());
  await showOlderConversationEntry(page, 'pi:180');
  await expect(article).toHaveCount(0);
  expect(await page.locator('.conversation-entry').count()).toBeLessThanOrEqual(24);
  await scrollConversationToBottom(page);
  await expect(tool).toHaveAttribute('open', '');
  await expect.poll(() => table.evaluate(element => element.scrollLeft)).toBe(offset);
  await expect(page.getByText('Tool detail was truncated by the relay.')).toHaveCount(0);
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await page.getByRole('button', { name: 'Open Window B on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  await expect(tool).not.toHaveAttribute('open', '');
  expect(await table.evaluate(element => element.scrollLeft)).toBe(0);
});

for (const source of ['recorded_history', 'pi_events']) {
  test(`scroll to bottom follows staggered ${source} appends after older paging`, async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity'] });
    await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'fixture-terminal', generation: 1, agent_session_id: 'fixture-session', status: 'working', project: 'Follow window', agent: source === 'pi_events' ? 'pi' : 'codex', cwd: '/fixture', conversation_history_available: true }] });
    const capabilities = { source, active_branch: source === 'pi_events' };
    const entries = Array.from({ length: 240 }, (_, index) => ({ id: `entry:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Seed message ${index}` }));
    await setConversationFixture(page, { entries: entries.slice(40), total: 240, sourceRevision: 'follow:1', capabilities, sequence: 0, hasMore: true, nextCursor: 'older-follow', pages: { 'older-follow': { entries: entries.slice(0, 40) } } });
    await page.getByRole('button', { name: 'Open Follow window on Fedora' }).click();
    await page.getByRole('button', { name: 'Conversation history' }).click();
    await page.getByRole('button', { name: 'Full history', exact: true }).click();
    const list = page.locator('.conversation-list');
    await expect(list.getByText('Seed message 239', { exact: true })).toBeAttached();
    await expect(page.locator('.conversation-page > .conversation-source-status')).toHaveCount(0);
    const branchTrust = page.locator('.conversation-toolbar .conversation-branch-trust');
    if (source === 'recorded_history') {
      await expect(branchTrust).toBeVisible();
      await expect(branchTrust).toContainText('Branch unverified');
      await expect(branchTrust.locator('.sr-only')).toHaveText(': saved history may not reflect the current branch.');
    } else {
      await expect(branchTrust).toHaveCount(0);
    }
    await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
    await showOlderConversationEntry(page, 'entry:0');
    await expect(list.getByText('Seed message 0', { exact: true })).toBeAttached();
    await scrollConversationToBottom(page);
    await expect.poll(() => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
    const deliver = async (after: number, incoming: Record<string, unknown>[]) => {
      await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).length).toBeGreaterThan(0);
      const request = (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).at(-1)!;
      await server(page, 0, { type: 'command_result', action: 'get_conversation_updates', request_id: request.request_id, ok: true, phase: 'completed', data: { subscription_id: 'fixture-subscription', kind: 'batch', source_revision: 'follow:1', sequence: after + 1, entries: incoming, remove: [], state: 'recorded', capabilities } });
    };
    await deliver(0, [{ id: 'entry:240', role: 'user', text: 'Appended prompt', timestamp: '' }, { id: 'entry:241', role: 'assistant', text: 'Appended intermediate', timestamp: '' }]);
    await expect(list.getByText('Appended intermediate', { exact: true })).toBeVisible();
    await deliver(1, [{ id: 'entry:242', role: 'assistant', text: '', timestamp: '', tools: [{ id: 'tail-tool', name: 'Appended tool', input: '{}', output: 'done' }] }]);
    await expect(list.locator('summary').filter({ hasText: 'Appended tool' })).toBeVisible();
    await expect.poll(() => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
    expect(await list.locator('article').count()).toBeLessThanOrEqual(24);
    await list.evaluate(element => { element.dispatchEvent(new Event('pointerdown', { bubbles: true })); element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
    const anchor = await list.locator('article').first().getAttribute('data-conversation-entry-id');
    await deliver(2, [{ id: 'entry:243', role: 'assistant', text: 'While reading older', timestamp: '' }]);
    await expect(list.locator('article').first()).toHaveAttribute('data-conversation-entry-id', anchor!);
    await expect(list.getByText('While reading older', { exact: true })).toHaveCount(0);
  });

  test(`bottom scrolling traverses multiple bounded loaded windows (${source})`, async ({ page }) => {
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity'] });
    const capabilities = { source, active_branch: source === 'pi_events' };
    const entries = Array.from({ length: 640 }, (_, index) => ({ id: `window:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Bounded message ${index} ` + 'reading anchor '.repeat(8) }));
    await setConversationFixture(page, { entries: entries.slice(440), total: 640, sourceRevision: 'windows:1', capabilities, sequence: 0, hasMore: true, nextCursor: 'window-240', pages: {
      'window-240': { entries: entries.slice(240, 440), hasMore: true, nextCursor: 'window-40' },
      'window-40': { entries: entries.slice(40, 240), hasMore: true, nextCursor: 'window-0' },
      'window-0': { entries: entries.slice(0, 40), hasMore: false },
    } });
    await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'working', project: 'Bounded windows', agent: source === 'pi_events' ? 'pi' : 'codex', conversation_history_available: true }] });
    await page.getByRole('button', { name: 'Open Bounded windows on Fedora' }).click();
    const list = page.locator('.conversation-list');
    await expect(list.locator('[data-conversation-entry-id="window:639"]')).toBeAttached();
    await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
    await expect.poll(async () => {
      const earlier = page.getByRole('button', { name: 'Show earlier loaded messages', exact: true });
      if (await earlier.count()) await earlier.evaluate(element => (element as HTMLElement).click());
      await list.evaluate(async element => {
        element.scrollTop = 0;
        element.dispatchEvent(new Event('scroll'));
        await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      });
      return list.locator('article').first().getAttribute('data-conversation-entry-id');
    }, { timeout: 15_000, intervals: [16] }).toBe('window:0');
    await expect(list.locator('article')).toHaveCount(24);
    await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Show newer loaded messages' })).toHaveCount(0);
    const anchor = await list.locator('article').last().getAttribute('data-conversation-entry-id');
    await list.evaluate(async element => {
      element.scrollTop = element.scrollHeight;
      (window as any).__bottomAnchorTop = [...element.querySelectorAll('article')].at(-1)!.getBoundingClientRect().top;
      element.dispatchEvent(new Event('scroll'));
      await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    });
    await expect(list.locator('article').first()).toHaveAttribute('data-conversation-entry-id', 'window:12');
    const retained = list.locator(`[data-conversation-entry-id="${anchor}"]`);
    expect(Math.abs(await retained.evaluate(element => element.getBoundingClientRect().top) - await page.evaluate(() => (window as any).__bottomAnchorTop))).toBeLessThanOrEqual(2);
    expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
    await scrollConversationToBottom(page);
    await expect(list.locator('[data-conversation-entry-id="window:639"]')).toBeVisible();
    await expect(list.locator('article')).toHaveCount(24);
  });

  test(`reader Conversation and Full history keep ${source} updates read-only`, async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
    await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'true'));
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await page.evaluate(() => localStorage.setItem('herdr_device_auth_v1', JSON.stringify({ version: 1, relays: { fedora: { kind: 'credential', id: 'credential-reader', version: 1, secret: 'R'.repeat(43), deviceId: 'device-reader', role: 'reader', locale: 'en', issuedAt: Date.now() } } })));
    await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity', 'pane_size_opt_in', 'pane_size_opt_in_rows'] });
    await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'fixture-terminal', generation: 1, agent_session_id: 'reader-session', status: 'working', project: 'Reader conversation', agent: 'pi', cwd: '/fixture', conversation_history_available: true }] });
    const capabilities = { source, active_branch: source === 'pi_events' };
    await setConversationFixture(page, { entries: [{ id: 'pi:seed', timestamp: '', role: 'assistant', text: 'Readable seed' }], total: 1, sourceRevision: 'reader:1', capabilities, sequence: 0 });
    await page.getByRole('button', { name: 'Open Reader conversation on Fedora' }).click();
    await page.getByRole('button', { name: 'Conversation history' }).click();
    const composer = page.getByRole('combobox', { name: 'Prompt', exact: true });
    const assertConversationLocked = async () => {
      await expect(composer).toBeDisabled();
      await expect(composer).toHaveAttribute('placeholder', 'Reader access is read only');
      for (const name of ['Attach photos', 'Attach files', 'Send prompt']) await expect(page.getByRole('button', { name, exact: true })).toBeDisabled();
    };
    let after = 0;
    for (const mode of ['Conversation', 'Full history']) {
      await page.getByRole('button', { name: mode, exact: true }).click();
      await assertConversationLocked();
      await expect(page.getByText('Readable seed', { exact: true })).toBeVisible();
      await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).length).toBeGreaterThan(0);
      const request = (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).at(-1)!;
      after += 1;
      await server(page, 0, { type: 'command_result', action: 'get_conversation_updates', request_id: request.request_id, ok: true, phase: 'completed', data: { subscription_id: 'fixture-subscription', kind: 'batch', source_revision: 'reader:1', sequence: after, entries: [{ id: `pi:prompt:${after}`, timestamp: '', role: 'user', text: `Reader prompt ${mode}` }, { id: `pi:answer:${after}`, timestamp: '', role: 'assistant', text: `Reader update ${mode}` }, ...(source === 'pi_events' ? [{ id: 'live:reader', timestamp: '', role: 'assistant', text: 'Reader provisional update' }] : [])], remove: [], state: source === 'pi_events' ? 'generating' : 'recorded', capabilities } });
      await expect(page.getByText(`Reader update ${mode}`, { exact: true })).toBeVisible();
      if (source === 'pi_events' && mode === 'Full history') {
        await expect(page.getByText('Reader provisional update', { exact: true })).toBeVisible();
        await expect(page.locator('[data-conversation-entry-id="live:reader"] > small')).toHaveText('Live · not saved yet');
      }
      await expect(page.locator('.conversation-page > .conversation-source-status')).toHaveCount(0);
      if (source === 'recorded_history') await expect(page.locator('.conversation-toolbar .conversation-branch-trust')).toContainText('Branch unverified');
      else await expect(page.locator('.conversation-toolbar .conversation-branch-trust')).toHaveCount(0);
      await assertConversationLocked();
    }
    await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
    await expect(page.getByRole('status').filter({ hasText: 'Reader access is read only' })).toBeVisible();
    await expect(page.getByRole('combobox', { name: 'Prompt', exact: true })).toBeDisabled();
    for (const name of ['Attach photos', 'Attach files', 'Enter', 'Tab', 'Ctrl', 'Shift', 'Arrow keys', 'Function keys']) await expect(page.getByRole('button', { name, exact: true })).toBeDisabled();
    expect(await page.locator('.terminal-find button:enabled, .terminal-find select:enabled').count()).toBe(0);
    await page.getByRole('log').dispatchEvent('wheel', { deltaY: -200 });
    await page.setViewportSize({ width: 390, height: 740 });
    await page.getByRole('button', { name: 'Back', exact: true }).click();
    const mutations = ['acknowledge_pane', 'send_input', 'send_keys', 'lease_pane_size', 'release_pane_size', 'resize_pane', 'submit_prompt', 'upload_begin', 'upload_chunk', 'upload_finish', 'copy_agent_response'];
    expect((await commands(page)).filter(command => String(command.type).startsWith('agent_') || mutations.includes(String(command.type)))).toEqual([]);
  });
}

for (const source of ['recorded_history', 'pi_events']) {
  test(`scroll intent: small touch movements stay unpinned until the actual bottom (${source})`, async ({ page }) => {
    await page.addInitScript(() => localStorage.setItem("herdr_default_agent_view", "terminal"));
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity'] });
    await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'scroll-terminal', generation: 1, agent_session_id: 'scroll-session', status: 'working', project: 'Scroll intent', agent: source === 'pi_events' ? 'pi' : 'codex', conversation_history_available: true }] });
    const capabilities = { source, active_branch: source === 'pi_events' };
    const entries = Array.from({ length: 120 }, (_, index) => ({ id: `scroll:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Scroll message ${index} ` + 'wrapped history '.repeat(12) }));
    await setConversationFixture(page, { entries, total: 120, sourceRevision: 'scroll:1', capabilities, sequence: 0 });
    await page.getByRole('button', { name: 'Open Scroll intent on Fedora' }).click();
    await page.getByRole('button', { name: 'Conversation history' }).click();
    await page.getByRole('button', { name: 'Full history', exact: true }).click();
    const list = page.locator('.conversation-list');

    await expect(list.locator('article')).toHaveCount(24);
    await expect.poll(() => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
    await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
    await expect(page.locator('.conversation-jump')).toHaveCount(0);
    for (const delta of [-8, -8, -8, 5, -4, 7]) {
      const before = await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight);
      await list.evaluate((element, value) => { element.scrollBy(0, value); element.dispatchEvent(new Event('scroll')); }, delta);
      await expect(page.locator('.conversation-jump')).toHaveCount(0);
      expect(Math.abs(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight) - (before - delta))).toBeLessThanOrEqual(2);
    }
    await list.dispatchEvent('pointerup', { pointerType: 'touch' });
    await list.evaluate(element => { element.scrollBy(0, -160); element.dispatchEvent(new Event('scroll')); });
    expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
    await scrollConversationToBottom(page);
    await expect.poll(() => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  });

  test(`scroll intent: wheel window and bottom arrival preserve reading and resume (${source})`, async ({ page, browserName }) => {
    await page.addInitScript(() => localStorage.setItem("herdr_default_agent_view", "terminal"));
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity'] });
    await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'scroll-terminal', generation: 1, agent_session_id: 'scroll-session', status: 'working', project: 'Scroll window', agent: source === 'pi_events' ? 'pi' : 'codex', conversation_history_available: true }] });
    const capabilities = { source, active_branch: source === 'pi_events' };
    const entries = Array.from({ length: 280 }, (_, index) => ({ id: `scroll:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Scroll message ${index} ` + 'wrapped history '.repeat(12) }));
    await setConversationFixture(page, { entries: entries.slice(80), total: 280, sourceRevision: 'scroll:1', capabilities, sequence: 0, hasMore: true, nextCursor: 'scroll-older', pages: { 'scroll-older': { entries: entries.slice(0, 80) } } });
    await page.getByRole('button', { name: 'Open Scroll window on Fedora' }).click();
    await page.getByRole('button', { name: 'Conversation history' }).click();
    await page.getByRole('button', { name: 'Full history', exact: true }).click();
    const list = page.locator('.conversation-list');
    const gap = () => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight);

    await expect.poll(gap).toBeLessThan(2);
    await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
    await showOlderConversationEntry(page, 'scroll:0');
    await expect(list.locator('[data-conversation-entry-id="scroll:0"]')).toBeAttached();
    await scrollConversationToBottom(page);
    await expect.poll(gap).toBeLessThan(2);
    await page.waitForTimeout(350);
    const box = (await list.boundingBox())!;
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    for (const delta of [-150, -110, -75, -50, -30]) {
      if (browserName === 'webkit') {
        await list.evaluate((element, value) => {
          element.dispatchEvent(new WheelEvent('wheel', { deltaY: value, bubbles: true }));
          element.scrollBy(0, value);
          element.dispatchEvent(new Event('scroll'));
        }, delta);
      } else await page.mouse.wheel(0, delta);
    }
    expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
    await page.waitForTimeout(100);
    const anchor = await list.locator('article').first().getAttribute('data-conversation-entry-id');
    const top = await list.evaluate(element => element.scrollTop);
    const deliver = async (after: number, incoming: Record<string, unknown>[]) => {
      await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).length).toBeGreaterThan(0);
      const request = (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after).at(-1)!;
      await server(page, 0, { type: 'command_result', action: 'get_conversation_updates', request_id: request.request_id, ok: true, phase: 'completed', data: { subscription_id: 'fixture-subscription', kind: 'batch', source_revision: 'scroll:1', sequence: after + 1, entries: incoming, remove: [], state: 'generating', capabilities } });
    };
    await deliver(0, [{ id: 'live:scroll', role: 'assistant', text: 'Provisional while reading', timestamp: '' }]);
    await expect(list.locator('article').first()).toHaveAttribute('data-conversation-entry-id', anchor!);
    expect(Math.abs(await list.evaluate(element => element.scrollTop) - top)).toBeLessThanOrEqual(2);
    expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
    await deliver(1, [{ id: 'scroll:280', role: 'assistant', text: 'Committed while reading', timestamp: '' }]);
    await expect(list.locator('article').first()).toHaveAttribute('data-conversation-entry-id', anchor!);
    await scrollConversationToBottom(page);
    await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
    await list.evaluate(element => { element.scrollBy(0, -400); element.dispatchEvent(new Event('scroll')); });
    await list.dispatchEvent('pointerup', { pointerType: 'touch' });
    await list.evaluate(element => {
      (window as any).__scrollTail = new Promise<void>(resolve => {
        let count = 0;
        const timer = setInterval(() => {
          element.scrollBy(0, -35);
          element.dispatchEvent(new Event('scroll'));
          if (++count === 20) { clearInterval(timer); resolve(); }
        }, 16);
      });
    });
    await list.evaluate(element => { element.scrollTop = element.scrollHeight; element.dispatchEvent(new Event('scroll')); });
    await page.evaluate(() => (window as any).__scrollTail);
    await expect.poll(gap).toBeGreaterThan(2);
    await scrollConversationToBottom(page);
    await expect.poll(gap).toBeLessThan(2);
    await expect(page.locator('.conversation-jump')).toHaveCount(0);
    await deliver(2, [{ id: 'scroll:281', role: 'assistant', text: 'Following after momentum', timestamp: '' }]);
    await expect(list.getByText('Following after momentum', { exact: true })).toBeVisible();
    await expect.poll(gap).toBeLessThan(2);
    expect(await list.locator('article').count()).toBeLessThanOrEqual(24);
    for (let after = 3; after <= 6; after++) {
      await page.waitForTimeout(100);
      await deliver(after, [{ id: `scroll:${279 + after}`, role: 'assistant', text: `Stream continues ${after}`, timestamp: '' }]);
      await expect(list.getByText(`Stream continues ${after}`, { exact: true })).toBeVisible();
    }
    await page.waitForTimeout(350);
    await list.evaluate(element => {
      element.dispatchEvent(new WheelEvent('wheel', { deltaY: -240, bubbles: true }));
      element.scrollBy(0, -240);
      element.dispatchEvent(new Event('scroll'));
    });
    expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  });
}

test('scroll intent: upward movement releases following and explicit bottom arrival restores it', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Momentum', agent: 'pi', conversation_history_available: true, agent_session_id: 'momentum-session' }] });
  await setConversationFixture(page, { entries: Array.from({ length: 80 }, (_, index) => ({ id: `momentum:${index}`, role: index % 2 ? 'assistant' : 'user', text: `Momentum ${index} ` + 'wrapped text '.repeat(15), timestamp: '' })), total: 80 });
  await page.getByRole('button', { name: 'Open Momentum on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  const list = page.locator('.conversation-list');
  const gap = () => list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight);
  await expect.poll(gap).toBeLessThan(2);
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollBy(0, -400); element.dispatchEvent(new Event('scroll')); });
  await list.dispatchEvent('pointerup', { pointerType: 'touch' });

  expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await list.evaluate(element => {
    (window as any).__momentumDone = new Promise<void>(resolve => {
      let count = 0;
      const timer = setInterval(() => {
        element.scrollBy(0, -35);
        element.dispatchEvent(new Event('scroll'));
        if (++count === 20) { clearInterval(timer); resolve(); }
      }, 16);
    });
  });
  await list.evaluate(element => { element.scrollTop = element.scrollHeight; element.dispatchEvent(new Event('scroll')); });
  await page.evaluate(() => (window as any).__momentumDone);
  await expect.poll(gap).toBeGreaterThan(2);
  await scrollConversationToBottom(page);
  await expect.poll(gap).toBeLessThan(2);
  await expect(page.locator('.conversation-jump')).toHaveCount(0);
  await page.clock.runFor(350);
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollBy(0, -200); element.dispatchEvent(new Event('scroll')); });
  expect(await list.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
});

const composerQuestion = {
  id: 'composer-question', kind: 'single_select', question: 'Choose composer coverage',
  options: [{ index: 0, label: 'Both views' }, { index: 1, label: 'Neither' }],
  other: { hidden: true }, submit_label: 'Submit', can_go_back: false,
};

for (const mode of ['Conversation', 'Full history']) {
  test(`shared composer ${mode}: activity stays in the header without layout or focus changes`, async ({ page }) => {
    await page.clock.install();
    await page.setViewportSize({ width: 320, height: 740 });
    await page.emulateMedia({ reducedMotion: 'reduce' });
    const prompt = await openSharedComposer(page, mode);
    await page.evaluate(() => { document.documentElement.style.fontSize = '20px'; });
    await prompt.focus();
    await page.evaluate(async () => {
      await document.fonts.ready;
      await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
    });
    // The composer can still settle a few pixels after fonts and frames are ready.
    let settledY = Number.NaN;
    await expect.poll(async () => {
      const y = (await prompt.boundingBox())!.y;
      const stable = y === settledY;
      settledY = y;
      return stable;
    }, { intervals: [100] }).toBe(true);
    const initialPrompt = (await prompt.boundingBox())!;
    const requests = (await commands(page)).filter(command => command.type !== 'read_pane');
    for (const status of ['working', 'unknown', 'done', 'idle']) {
      await server(page, 0, { ...composerTarget, type: 'agent_update', status });
      await expect(page.locator('.app-header > .status-dot')).toBeVisible();
      expect(Math.abs((await prompt.boundingBox())!.y - initialPrompt.y)).toBeLessThanOrEqual(2);
      await expect(prompt).toBeFocused();
    }
    await page.clock.runFor(350);
    expect((await commands(page)).filter(command => command.type !== 'read_pane')).toEqual(requests);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
    await server(page, 0, { ...composerTarget, type: 'agent_update', status: 'working' });
    await expect(page.locator('.app-header > .status-dot')).toBeVisible();
  });

  test(`shared composer ${mode}: slash selection fills without submitting and keyboard navigation works`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await prompt.fill('/');
    await expect(page.getByRole('option', { name: /\/copy/ })).toHaveAttribute('aria-selected', 'true');
    await prompt.press('ArrowDown');
    await prompt.press('Enter');
    await expect(prompt).toHaveValue('/help');
    expect((await commands(page)).filter(command => command.type === 'submit_prompt')).toEqual([]);
    await prompt.fill('/pl');
    await prompt.press('Tab');
    await expect(prompt).toHaveValue('/plan ');
    await prompt.fill('/plan shared coverage');
    await prompt.press('Control+Enter');
    await expect.poll(async () => (await commands(page)).find(command => command.type === 'submit_prompt')).toMatchObject({ target: composerTarget, text: '/plan shared coverage' });
  });

  test(`shared composer ${mode}: slash discovery settles during frequent live inventory updates`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await setAutoCommands(page, false);
    await prompt.fill('/');
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'list_slash_commands').length).toBe(1);
    const request = (await commands(page)).find(command => command.type === 'list_slash_commands')!;
    for (let update = 0; update < 12; update++) {
      await server(page, 0, { ...composerTarget, type: 'agent_update', status: 'working', updated_at: update });
      await server(page, 0, { type: 'pong' });
    }
    await server(page, 0, { type: 'command_result', request_id: request.request_id, ok: true, phase: 'completed', data: { commands: [{ command: '/help', description: 'Help', source: 'builtin' }], truncated: false } });
    await expect(page.getByRole('listbox')).toHaveAttribute('aria-busy', 'false');
    expect((await commands(page)).filter(command => command.type === 'list_slash_commands')).toHaveLength(1);
    await page.getByRole('button', { name: 'Refresh commands' }).click();
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'list_slash_commands').length).toBe(2);
  });

  test(`shared composer ${mode}: stale pane state suspends drafts without a terminal subscription`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await prompt.fill('safe fresh draft');
    await expect.poll(async () => JSON.stringify(await page.evaluate(() => ({ ...localStorage })))).toContain('safe fresh draft');
    const reads = (await commands(page)).filter(command => command.type === 'read_pane');
    expect(reads.at(-1)).toMatchObject({ lines: 30, content_fingerprint: '', target: composerTarget });
    expect((await commands(page)).filter(command => command.type === 'watch_pane')).toHaveLength(0);
    const draftStatus = page.locator('.prompt-composer > .draft-status');
    await expect(draftStatus).toHaveText('Not saved on this phone: waiting for fresh terminal input state.');
    await expect(draftStatus).toBeVisible();
    await expect(draftStatus).not.toHaveClass(/error/);
    await prompt.fill('unchecked-secret');
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', target: composerTarget, format: 'plain', content: 'Ready', no_echo: false });
    await expect(draftStatus).toHaveText('Not saved on this phone: terminal input was hidden or could not be checked when this text was typed.');
    await expect(draftStatus).toBeVisible();
    await expect(draftStatus).not.toHaveClass(/error/);
    expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('unchecked-secret');
    await page.getByRole('button', { name: 'Terminal view' }).click();
    await replyToPaneRead(page);
    await expect(page.getByRole('combobox', { name: 'Prompt' })).toHaveValue('safe fresh draft');
  });

  test(`shared composer ${mode}: photo upload inserts a ref before exact-target prompt dispatch`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await prompt.fill('Review picture');
    await page.locator('input[type=file][accept="image/*"]').setInputFiles({ name: 'conversation.png', mimeType: 'image/png', buffer: Buffer.from('png') });
    await expect(prompt).toHaveValue('Review picture\nAttachment: attachment:test-1\n');
    await expect(page.getByText('Attached conversation.png')).toBeVisible();
    const upload = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
    expect(upload.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_finish']);
    for (const command of upload) expect(command.target).toEqual(composerTarget);
    await prompt.press('Meta+Enter');
    await expect.poll(async () => (await commands(page)).find(command => command.type === 'submit_prompt')).toMatchObject({ target: composerTarget, text: 'Review picture\nAttachment: attachment:test-1' });
    await expect(prompt).toHaveValue('');
  });

  test(`shared composer ${mode}: upload failure remains visible while locked and restart is guarded`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await prompt.fill('Wait for upload');
    await setAutoCommands(page, false);
    await page.locator('input[type=file][accept="image/*"]').setInputFiles({ name: 'retry.png', mimeType: 'image/png', buffer: Buffer.from('png') });
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'upload_begin').length).toBe(1);
    await prompt.press('Control+Enter');
    await prompt.press('Meta+Enter');
    expect((await commands(page)).filter(command => command.type === 'submit_prompt')).toEqual([]);
    const begin = (await commands(page)).find(command => command.type === 'upload_begin')!;
    await server(page, 0, { type: 'upload_begin_result', request_id: begin.request_id, error: { code: 'attachment_upload_expired' } });
    const restart = page.getByRole('button', { name: 'Restart interrupted files' });
    await expect(restart).toBeEnabled();
    await server(page, 0, { ...composerTarget, type: 'agent_update', status: 'blocked', attention_kind: 'unknown' });
    await expect(restart).toBeDisabled();
    await expect(page.getByRole('alert').filter({ hasText: 'The upload expired.' })).toBeVisible();
    await restart.dispatchEvent('click');
    expect((await commands(page)).filter(command => command.type === 'upload_begin')).toHaveLength(1);
    for (let snapshot = 0; snapshot < 2; snapshot += 1) await server(page, 0, { ...composerTarget, type: 'agent_update', status: 'idle' });
    await expect(restart).toBeEnabled();
    await setAutoCommands(page, true);
    await restart.click();
    await expect(prompt).toHaveValue('Wait for upload\nAttachment: attachment:test-1\n');
    expect((await commands(page)).filter(command => command.type === 'upload_begin')).toHaveLength(2);
  });

  test(`shared composer ${mode}: hidden values and tainted drafts stay out of storage across views`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await prompt.fill('safe shared draft');
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: 'Password:', no_echo: true, no_echo_prompt: 'Password:' });
    const hidden = page.getByLabel('Value for the hidden terminal prompt');
    await expect(hidden).toHaveAttribute('type', 'password');
    const hiddenRead = await replyToPaneRead(page, 'Password:', true);
    await prompt.fill('secret-in-wrong-field');
    await hidden.fill('masked-secret');
    await hidden.press('Enter');
    await expect.poll(async () => (await commands(page)).find(command => command.type === 'send_secret')).toMatchObject({ target: composerTarget, text: 'masked-secret' });
    await expect(hidden).toHaveValue('');
    expect((await commands(page)).filter(command => command.type === 'submit_prompt')).toEqual([]);
    await expect.poll(async () => (await commands(page)).findLast(command => command.type === 'read_pane')?.request_id).not.toBe(hiddenRead);
    await replyToPaneRead(page);
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: 'Ready', no_echo: false });
    const draftStatus = page.locator('.prompt-composer > .draft-status');
    await expect(draftStatus).toContainText('Not saved on this phone');
    await expect(draftStatus).toBeVisible();
    await expect(draftStatus).not.toHaveClass(/error/);
    expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('secret-in-wrong-field');
    expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('masked-secret');
    await page.getByRole('button', { name: 'Terminal view' }).click();
    await replyToPaneRead(page);
    await expect(page.getByRole('combobox', { name: 'Prompt' })).toHaveValue('safe shared draft');
    await page.getByRole('button', { name: 'Conversation history' }).click();
    await expect(page.locator('.conversation-page')).toBeVisible();
    await replyToPaneRead(page);
    await expect(prompt).toHaveValue('safe shared draft');
  });

  test(`shared composer ${mode}: terminal-only keys send arrows function keys and armed chords`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await expect(page.getByRole('button', { name: 'Keys', exact: true })).toHaveCount(0);
    await expect(page.getByRole('textbox', { name: 'Modifier shortcut character' })).toHaveCount(0);
    const assertRow = async () => {
      const row = page.locator('.term-keys:visible');
      const boxes = await row.locator('button').evaluateAll(elements => elements.map(element => {
        const rect = element.getBoundingClientRect();
        return { top: rect.top, bottom: rect.bottom, left: rect.left, right: rect.right };
      }));
      const rowBox = (await row.boundingBox())!;
      const promptBox = (await prompt.boundingBox())!;
      expect(boxes).toHaveLength(8);
      expect(Math.max(...boxes.map(box => box.top)) - Math.min(...boxes.map(box => box.top))).toBeLessThan(2);
      expect(Math.max(...boxes.map(box => box.bottom))).toBeLessThanOrEqual(promptBox.y);
      expect(Math.min(...boxes.map(box => box.left))).toBeGreaterThanOrEqual(rowBox.x);
      expect(Math.max(...boxes.map(box => box.right))).toBeLessThanOrEqual(rowBox.x + rowBox.width);
    };
    await expect(page.locator('.term-keys')).toHaveCount(0);
    await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
    await assertRow();
    for (const name of ['Esc', 'Tab', 'Enter']) {
      const key = page.getByRole('button', { name, exact: true });
      await key.focus();
      await key.press('Enter');
    }
    await page.getByRole('button', { name: 'Arrow keys' }).click();
    await page.getByRole('button', { name: 'Down', exact: true }).click();
    await page.getByRole('button', { name: 'Function keys' }).click();
    await page.getByRole('button', { name: 'F5', exact: true }).click();
    await page.getByRole('button', { name: 'Ctrl', exact: true }).click();
    await page.getByRole('textbox', { name: 'Modifier shortcut character' }).press('c');
    await expect.poll(async () => (await commands(page)).filter(command => command.type === 'send_keys').map(command => command.keys)).toEqual([['Escape'], ['Tab'], ['Enter'], ['Down'], ['f5'], ['ctrl+c']]);
    for (const command of (await commands(page)).filter(command => command.type === 'send_keys')) expect(command.target).toEqual({ relay_id: 'fedora', ...composerTarget });
  });

  test(`shared composer ${mode}: structured questions and approvals respond without leaving history`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode);
    await server(page, 0, { ...composerTarget, type: 'blocked', attention_kind: 'question', interaction: composerQuestion });
    await expect(prompt).toBeEnabled();
    await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
    await page.getByRole('radio', { name: 'Both views' }).check();
    expect((await commands(page)).filter(command => command.type === 'answer_question')).toEqual([]);
    await page.getByRole('button', { name: 'Submit', exact: true }).click();
    await expect.poll(async () => (await commands(page)).find(command => command.type === 'answer_question')).toMatchObject({ target: composerTarget, interaction_id: 'composer-question' });
    await server(page, 0, { ...composerTarget, type: 'blocked', attention_kind: 'approval', options: ['Approve once', 'Deny'], command: 'Run composer checks?' });
    await page.getByRole('button', { name: 'Approve once', exact: true }).click();
    await expect.poll(async () => (await commands(page)).find(command => command.type === 'respond')).toMatchObject({ target: composerTarget, index: 0 });
    await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
    await expect(page.getByText('Composer history stays readable')).toBeVisible();
  });

  test(`shared composer ${mode}: reader denial matrix includes slash uploads hidden values keys and responses`, async ({ page }) => {
    const prompt = await openSharedComposer(page, mode, true);
    await expect(prompt).toBeDisabled();
    await server(page, 0, { ...composerTarget, type: 'agent_update', status: 'working' });
    for (const name of ['Attach photos', 'Attach files', 'Send prompt']) await expect(page.getByRole('button', { name, exact: true })).toBeDisabled();
    await prompt.evaluate(element => { (element as HTMLTextAreaElement).value = '/plan'; element.dispatchEvent(new Event('input', { bubbles: true })); });
    await prompt.dispatchEvent('keydown', { key: 'Enter', ctrlKey: true });
    await page.locator('input[type=file][accept="image/*"]').setInputFiles({ name: 'reader.png', mimeType: 'image/png', buffer: Buffer.from('png') });
    await expect(page.locator('.term-keys')).toHaveCount(0);
    await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
    for (const name of ['Esc', 'Tab', 'Enter', 'Ctrl', 'Shift', 'Alt', 'Function keys', 'Arrow keys']) await expect(page.getByRole('button', { name, exact: true })).toBeDisabled();
    await page.locator('.term-keys button').first().dispatchEvent('click');
    await page.getByRole('button', { name: 'Conversation history', exact: true }).click();
    await server(page, 0, { ...composerTarget, type: 'blocked', attention_kind: 'question', interaction: composerQuestion });
    await expect(page.getByRole('radio')).toHaveCount(0);
    await server(page, 0, { ...composerTarget, type: 'blocked', attention_kind: 'approval', options: ['Approve once', 'Deny'] });
    await expect(page.getByRole('button', { name: 'Approve once' })).toHaveCount(0);
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: 'Password:', no_echo: true });
    const hidden = page.getByLabel('Value for the hidden terminal prompt');
    await expect(hidden).toBeDisabled();
    await expect(page.getByRole('button', { name: 'Send hidden value' })).toBeDisabled();
    await hidden.dispatchEvent('keydown', { key: 'Enter' });
    await expect(page.getByRole('listbox')).toHaveCount(0);
    const denied = ['submit_prompt', 'send_input', 'send_text', 'send_keys', 'send_secret', 'respond', 'answer_question', 'navigate_question', 'list_slash_commands', 'upload_begin', 'upload_chunk', 'upload_finish', 'upload_cancel'];
    expect((await commands(page)).filter(command => String(command.type).startsWith('agent_') || denied.includes(String(command.type)))).toEqual([]);
  });
}

test('phone feedback integrated history loads on upward demand and explicit action with a stable anchor across view changes', async ({ page }) => {
  await page.clock.install();
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history'] });
  const entries = Array.from({ length: 72 }, (_, index) => ({ id: `integrated-${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Integrated message ${index} ` + 'anchor content '.repeat(30) }));
  await setConversationFixture(page, { entries: entries.slice(48), total: 72, sourceRevision: 'integrated:1', hasMore: true, nextCursor: 'older-24', pages: {
    'older-24': { entries: entries.slice(24, 48), hasMore: true, nextCursor: 'older-0' },
    'older-0': { entries: entries.slice(0, 24), hasMore: false },
  } });
  await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'idle', project: 'Integrated history', agent: 'codex', conversation_history_available: true }] });
  await page.getByRole('button', { name: 'Open Integrated history on Fedora' }).click();
  const list = page.locator('.conversation-list');
  await expect(list.locator('[data-conversation-entry-id="integrated-71"]')).toBeVisible();
  const reads = async () => (await commands(page)).filter(command => command.type === 'get_conversation_history');
  expect(await reads()).toHaveLength(1);
  expect((await reads())[0].limit).toBe(24);
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await page.getByRole('button', { name: 'Full history', exact: true }).click();
  expect(await reads()).toHaveLength(1);
  await setAutoCommands(page, false);
  await page.clock.runFor(300);
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
  await expect.poll(async () => (await reads()).length).toBe(2);
  const older = (await reads())[1];
  expect(older.cursor).toBe('older-24');
  const anchor = list.locator('[data-conversation-entry-id="integrated-48"]');
  const offset = async () => (await anchor.boundingBox())!.y - (await list.boundingBox())!.y;
  const before = await offset();
  await server(page, 0, { type: 'command_result', request_id: older.request_id, ok: true, phase: 'completed', data: { available: true, entries: entries.slice(24, 48), has_more: true, next_cursor: 'older-0', state: 'ready', mode: 'snapshot', snapshot_id: 'snapshot-1', source_revision: 'integrated:1' } });
  await expect(list.locator('[data-conversation-entry-id="integrated-36"]')).toBeAttached();
  await expect.poll(async () => Math.abs(await offset() - before)).toBeLessThanOrEqual(2);
  await expect(list.locator('.conversation-entry')).toHaveCount(24);
  expect(await reads()).toHaveLength(2);
  await setAutoCommands(page, true);
  await page.getByRole('button', { name: 'Load older turns', exact: true }).click();
  expect(await reads()).toHaveLength(2);
  await page.getByRole('button', { name: 'Load older turns', exact: true }).click();
  await expect.poll(async () => (await reads()).length).toBe(3);
  expect((await reads())[2].cursor).toBe('older-0');
  await expect(page.getByRole('button', { name: 'Load older turns', exact: true })).toHaveCount(0);
  await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
  await page.getByRole('button', { name: 'Conversation history', exact: true }).click();
  await expect(list.locator('[data-conversation-entry-id="integrated-71"]')).toBeVisible();
  await expect.poll(async () => (await reads()).length).toBe(4);
  const ids = await list.locator('.conversation-entry').evaluateAll(elements => elements.map(element => element.getAttribute('data-conversation-entry-id')));
  expect(new Set(ids).size).toBe(ids.length);
  expect(ids).toHaveLength(24);
  expect((await commands(page)).filter(command => ['submit_prompt', 'send_terminal_text', 'send_keys'].includes(String(command.type)))).toHaveLength(0);
});

test('followup combined notices and Pi bridge stay fenced across paging, modes and reconnect', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await page.evaluate(() => localStorage.setItem('herdr_device_auth_v1', JSON.stringify({ version: 1, relays: { fedora: { kind: 'credential', id: 'synthetic-controller', version: 1, secret: 'C'.repeat(43), deviceId: 'synthetic-controller', role: 'controller', locale: 'en', issuedAt: Date.now() } } })));
  const capabilities = ['conversation_history', 'conversation_activity', 'pi_prompt_unconfirmed', 'manual_terminal_input'];
  await handshake(page, 0, { capabilities });
  const target = { ...composerTarget, agent_session_id: 'followup-pi' };
  const agent = { ...target, status: 'blocked', attention_kind: 'unknown', project: 'Followup Pi', agent: 'pi', conversation_history_available: true };
  await server(page, 0, { type: 'agents', agents: [agent] });
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Followup Pi on Fedora' }).click();
  const last = async (type: string) => {
    await expect.poll(async () => (await commands(page)).filter(command => command.type === type).length).toBeGreaterThan(0);
    return (await commands(page)).findLast(command => command.type === type)!;
  };
  const respond = (request: Record<string, unknown>, data: unknown, phase = 'completed') => server(page, 0, { type: 'command_result', request_id: request.request_id, ok: true, phase, data });
  const notice = { id: 'pi:notice', role: 'assistant', kind: 'notice', timestamp: '', text: 'Generic extension request <img src=x onerror=alert(1)>' };
  const base = { available: true, total: null, state: 'ready', mode: 'native', snapshot_id: 'snapshot', source_revision: 'followup:1', capabilities: { source: 'pi_events', active_branch: true } };
  await respond(await last('get_conversation_history'), { ...base, entries: [notice], has_more: true, next_cursor: 'older', subscription_id: 'subscription', sequence: 0 });
  await expect(page.getByText(notice.text)).toBeVisible();
  await expect(page.locator('.conversation-notice img')).toHaveCount(0);
  await expect(page.locator('.app-header > .status-dot')).toBeVisible();
  await respond(await last('get_pi_prompt_capability'), { capability: 'pi_prompt_unconfirmed', lease: { instance: 'a'.repeat(64), pane: 'w1:p1', session: 'followup-pi', pid: 42, incarnation: 'b'.repeat(36), token: 'c'.repeat(36), connection: 'synthetic', target_key: 'target', expires_at: Date.now() + 15000 } });
  const read = await last('read_pane');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read.request_id, target: read.target, format: 'plain', content: 'Unrecognized Pi dialog', no_echo: false });
  const prompt = page.getByRole('combobox', { name: 'Prompt', exact: true });
  await prompt.fill('/orchestrate-answer synthetic guidance');
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Send prompt', exact: true }).click();
  const sentAt = Date.now();
  await respond(await last('send_pi_prompt_unconfirmed'), { sent_at: sentAt }, 'unconfirmed');
  await expect(page.getByText('Sent to Pi — not confirmed', { exact: true })).toBeVisible();
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_history' && !command.cursor).length).toBe(2);
  const unrelated = { id: 'pi:unrelated-user', role: 'user', timestamp: new Date(sentAt + 1).toISOString(), text: '/orchestrate-answer synthetic guidance' };
  await respond(await last('get_conversation_history'), { ...base, entries: [notice, unrelated], has_more: true, next_cursor: 'older', subscription_id: 'subscription', sequence: 0 });
  await expect(page.getByText(unrelated.text, { exact: true })).toBeVisible();
  await expect(page.getByText('Sent to Pi — not confirmed', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Load older turns', exact: true }).click();
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'get_conversation_history' && command.cursor === 'older').length).toBe(1);
  const older = (await commands(page)).findLast(command => command.type === 'get_conversation_history' && command.cursor === 'older')!;
  expect(older.cursor).toBe('older');
  await respond(older, { ...base, entries: [{ ...notice, id: 'pi:older', text: 'Older generic notice' }], has_more: false, next_cursor: '' });
  await expect(page.getByText('Older generic notice', { exact: true })).toBeVisible();
  const updates = await last('get_conversation_updates');
  await respond(updates, { kind: 'batch', subscription_id: 'subscription', source_revision: 'followup:1', sequence: 1, state: 'live', capabilities: base.capabilities, remove: [], entries: [{ ...notice, id: 'pi:live', text: 'Live generic notice' }, { ...unrelated, id: 'pi:unrelated-live-user' }] });
  await page.getByRole('button', { name: 'Full history', exact: true }).click();
  await expect(page.getByText('Live generic notice', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await expect(page.getByText('Live generic notice', { exact: true })).toBeVisible();
  await expect(page.getByText('Sent to Pi — not confirmed', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
  const mockCredential = await page.evaluate(() => {
    const saved = localStorage.getItem('herdr_device_auth_v1');
    localStorage.removeItem('herdr_device_auth_v1');
    return saved;
  });
  await closeSocket(page, 0);
  await expect.poll(() => socketCount(page)).toBe(2);
  await page.evaluate(saved => localStorage.setItem('herdr_device_auth_v1', saved!), mockCredential);
  await handshake(page, 1, { capabilities });
  await server(page, 1, { type: 'agents', agents: [agent] });
  await expect(page.getByText('Sent to Pi — not confirmed', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
  await page.getByRole('button', { name: 'Keyboard', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Send text', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Conversation history', exact: true }).click();
  await page.getByRole('button', { name: 'I checked — allow another send', exact: true }).click();
  expect((await commands(page)).filter(command => command.type === 'send_pi_prompt_unconfirmed')).toHaveLength(1);
  expect((await commands(page)).filter(command => ['submit_prompt', 'send_terminal_text', 'send_keys'].includes(String(command.type)))).toHaveLength(0);
});

test('followup Pi sends are exclusive across pages', async ({ page, context }) => {
  const capabilities = ['conversation_history', 'conversation_activity', 'pi_prompt_unconfirmed', 'manual_terminal_input'];
  const target = { ...composerTarget, agent_session_id: 'atomic-pi' };
  const agent = { ...target, status: 'blocked', attention_kind: 'unknown', project: 'Atomic Pi', agent: 'pi', conversation_history_available: true };
  const second = await context.newPage();
  const pages = [page, second];
  const last = async (view: Page, type: string) => {
    await expect.poll(async () => (await commands(view)).filter(command => command.type === type).length).toBeGreaterThan(0);
    return (await commands(view)).findLast(command => command.type === type)!;
  };
  const respond = (view: Page, request: Record<string, unknown>, data: unknown, phase = 'completed') => server(view, 0, { type: 'command_result', request_id: request.request_id, ok: true, phase, data });
  for (const view of pages) {
    if (view === second) await page.evaluate(() => localStorage.removeItem('herdr_device_auth_v1'));
    await view.addInitScript(() => Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' }));
    await boot(view, [fedora]);
    await expect.poll(() => socketCount(view)).toBe(1);
    await view.evaluate(() => localStorage.setItem('herdr_device_auth_v1', JSON.stringify({ version: 1, relays: { fedora: { kind: 'credential', id: 'synthetic-controller', version: 1, secret: 'C'.repeat(43), deviceId: 'synthetic-controller', role: 'controller', locale: 'en', issuedAt: Date.now() } } })));
    await setAutoCommands(view, false);
    await handshake(view, 0, { capabilities });
    await server(view, 0, { type: 'agents', agents: [agent] });
    await view.getByRole('button', { name: 'Open Atomic Pi on Fedora' }).click();
    await respond(view, await last(view, 'get_conversation_history'), { available: true, total: null, entries: [], has_more: false, state: 'ready', mode: 'native', source_revision: 'atomic:1' });
    await respond(view, await last(view, 'get_pi_prompt_capability'), { capability: 'pi_prompt_unconfirmed', lease: { instance: 'a'.repeat(64), pane: 'w1:p1', session: 'atomic-pi', pid: 42, incarnation: 'b'.repeat(36), token: 'c'.repeat(36), connection: 'synthetic', target_key: 'target', expires_at: Date.now() + 15000 } });
    const read = await last(view, 'read_pane');
    await server(view, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read.request_id, target: read.target, format: 'plain', content: 'Unrecognized Pi dialog', no_echo: false });
    await view.getByRole('combobox', { name: 'Prompt', exact: true }).fill('one simultaneous reply');
    await expect(view.getByRole('button', { name: 'Send prompt', exact: true })).toBeEnabled();
  }
  await Promise.all(pages.map(view => view.getByRole('button', { name: 'Send prompt', exact: true }).click()));
  const counts = async () => Promise.all(pages.map(async view => (await commands(view)).filter(command => command.type === 'send_pi_prompt_unconfirmed').length));
  await expect.poll(async () => (await counts()).reduce((sum, count) => sum + count, 0)).toBe(1);
  const winner = pages[(await counts()).indexOf(1)];
  const loser = pages.find(view => view !== winner)!;
  const markers = () => page.evaluate(() => Object.keys(localStorage).filter(key => key.startsWith('herdr_pi_send:')).map(key => localStorage.getItem(key)));
  const held = () => page.evaluate(async () => (await navigator.locks.query()).held?.filter(lock => lock.name?.startsWith('herdr_pi_send:')).length);
  await expect(loser.getByText('Check the previous Pi send first.', { exact: true })).toBeVisible();
  await expect.poll(held).toBe(1);
  await loser.getByRole('button', { name: 'I checked — allow another send', exact: true }).click();
  await expect.poll(markers).toEqual(['unknown']);
  await expect.poll(held).toBe(1);
  await respond(winner, await last(winner, 'send_pi_prompt_unconfirmed'), { sent_at: Date.now() }, 'unconfirmed');
  await expect.poll(held).toBe(0);
  await loser.getByRole('button', { name: 'I checked — allow another send', exact: true }).click();
  await expect.poll(markers).toEqual([]);
  expect((await counts()).reduce((sum, count) => sum + count, 0)).toBe(1);
  for (const view of pages) expect((await commands(view)).filter(command => ['submit_prompt', 'send_terminal_text', 'send_keys'].includes(String(command.type)))).toHaveLength(0);
  await second.close();
});

for (const phase of ['unconfirmed', 'dispatched_unknown', 'pending']) test(`followup Pi send survives reload after ${phase}`, async ({ page }) => {
  await boot(page, [fedora]);
  const capabilities = ['conversation_history', 'conversation_activity', 'pi_prompt_unconfirmed', 'manual_terminal_input'];
  const target = { ...composerTarget, agent_session_id: `reload-pi-${phase}` };
  const agent = { ...target, status: 'blocked', attention_kind: 'unknown', project: 'Reload Pi', agent: 'pi', conversation_history_available: true };
  const credential = JSON.stringify({ version: 1, relays: { fedora: { kind: 'credential', id: 'synthetic-controller', version: 1, secret: 'C'.repeat(43), deviceId: 'synthetic-controller', role: 'controller', locale: 'en', issuedAt: Date.now() } } });
  const last = async (type: string) => {
    await expect.poll(async () => (await commands(page)).filter(command => command.type === type).length).toBeGreaterThan(0);
    return (await commands(page)).findLast(command => command.type === type)!;
  };
  const respond = (request: Record<string, unknown>, data: unknown, resultPhase = 'completed', ok = true) => server(page, 0, { type: 'command_result', request_id: request.request_id, ok, phase: resultPhase, error: ok ? '' : 'Synthetic uncertain delivery', data });
  const admit = async () => {
    await expect.poll(() => socketCount(page)).toBe(1);
    await page.evaluate(saved => localStorage.setItem('herdr_device_auth_v1', saved), credential);
    await setAutoCommands(page, false);
    await handshake(page, 0, { capabilities });
    await server(page, 0, { type: 'agents', agents: [agent] });
  };
  const ready = async () => {
    await respond(await last('get_conversation_history'), { available: true, total: null, entries: [{ id: 'pi:notice', role: 'assistant', kind: 'notice', timestamp: '', text: 'Reload-safe extension request' }], has_more: false, state: 'ready', mode: 'native', source_revision: 'reload:1' });
    await respond(await last('get_pi_prompt_capability'), { capability: 'pi_prompt_unconfirmed', lease: { instance: 'a'.repeat(64), pane: 'w1:p1', session: target.agent_session_id, pid: 42, incarnation: 'b'.repeat(36), token: 'c'.repeat(36), connection: 'synthetic', target_key: 'target', expires_at: Date.now() + 15000 } });
    const read = await last('read_pane');
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read.request_id, target: read.target, format: 'plain', content: 'Unrecognized Pi dialog', no_echo: false });
  };
  const reload = async () => {
    await page.evaluate(() => localStorage.removeItem('herdr_device_auth_v1'));
    await page.reload();
    await admit();
    await ready();
  };
  const markers = () => page.evaluate(() => Object.keys(localStorage).filter(key => key.startsWith('herdr_pi_send:')).map(key => localStorage.getItem(key)));
  const send = page.getByRole('button', { name: 'Send prompt', exact: true });
  const prompt = page.getByRole('combobox', { name: 'Prompt', exact: true });
  await admit();
  await page.getByRole('button', { name: 'Open Reload Pi on Fedora' }).click();
  await ready();
  await prompt.fill('one private reply');
  await expect(send).toBeEnabled();
  await send.click();
  const request = await last('send_pi_prompt_unconfirmed');
  expect(await markers()).toEqual(['unknown']);
  if (phase !== 'pending') {
    await respond(request, { sent_at: Date.now() }, phase, phase === 'unconfirmed');
    await expect(page.getByRole('button', { name: 'I checked — allow another send', exact: true })).toBeVisible();
  }
  expect((await commands(page)).filter(command => command.type === 'send_pi_prompt_unconfirmed')).toHaveLength(1);
  expect(await markers()).toEqual([phase === 'unconfirmed' ? 'sent' : 'unknown']);
  await reload();
  await expect(page.getByText(phase === 'unconfirmed' ? 'Sent to Pi — not confirmed' : 'Pi send not confirmed', { exact: true })).toBeVisible();
  await prompt.fill('another manual reply');
  await expect(send).toBeDisabled();
  await prompt.press('Control+Enter');
  await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
  await page.getByRole('button', { name: 'Keyboard', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Send text', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Conversation history', exact: true }).click();
  await page.getByRole('button', { name: 'I checked — allow another send', exact: true }).click();
  await expect.poll(markers).toEqual([]);
  expect((await commands(page)).filter(command => ['send_pi_prompt_unconfirmed', 'submit_prompt', 'send_terminal_text', 'send_keys'].includes(String(command.type)))).toHaveLength(0);
  await reload();
  await expect(page.getByRole('button', { name: 'I checked — allow another send', exact: true })).toHaveCount(0);
  await prompt.fill('new draft after acknowledgement');
  await expect(send).toBeEnabled();
  expect((await commands(page)).filter(command => ['send_pi_prompt_unconfirmed', 'submit_prompt', 'send_terminal_text', 'send_keys'].includes(String(command.type)))).toHaveLength(0);
});

test('followup blocked Pi without a bridge points to Settings without fallback input', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['conversation_history', 'pi_integration', 'manual_terminal_input'] });
  await setConversationFixture(page, { entries: [{ id: 'setting-notice', role: 'assistant', kind: 'notice', text: 'Synthetic extension question', timestamp: '' }], total: 1 });
  await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, agent_session_id: 'setting-pi', status: 'blocked', attention_kind: 'unknown', project: 'Setting Pi', agent: 'pi', conversation_history_available: true }] });
  await page.getByRole('button', { name: 'Open Setting Pi on Fedora', exact: true }).click();
  await expect(page.getByText('For live Pi replies, enable Settings → Pi Integration, then /reload Pi.', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Keyboard', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
  await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
  await expect(page.getByText('For live Pi replies, enable Settings → Pi Integration, then /reload Pi.', { exact: true })).toBeVisible();
  expect((await commands(page)).filter(command => ['submit_prompt', 'send_pi_prompt_unconfirmed', 'send_keys', 'send_terminal_text'].includes(String(command.type)))).toHaveLength(0);
});

for (const mode of ['Conversation', 'Full history']) {
  test(`shared composer ${mode}: chunked upload hashes uploads for the exact target`, async ({ page }) => {
    await page.addInitScript(() => {
      const slices: Array<{ name: string; start: number | undefined; end: number | undefined }> = [];
      Object.defineProperty(window, '__uploadSlices', { value: slices });
      const original = Blob.prototype.slice;
      Blob.prototype.slice = function (start, end, type) {
        if (this instanceof File) slices.push({ name: this.name, start, end });
        return original.call(this, start, end, type);
      };
    });
    const prompt = await openSharedComposer(page, mode);
    expect(await page.evaluate(() => typeof crypto.subtle.digest)).toBe('function');
    await prompt.fill('Review upload');
    await page.locator('input[type=file][accept="image/*"]').setInputFiles([
      { name: 'upload-one.png', mimeType: 'image/png', buffer: Buffer.from('png') },
      { name: 'upload-two.png', mimeType: 'image/png', buffer: Buffer.from('abc') },
    ]);
    await expect(prompt).toHaveValue('Review upload\nAttachment: attachment:test-1\nAttachment: attachment:test-2\n');
    const upload = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
    expect(upload.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_chunk', 'upload_finish']);
    for (const command of upload) expect(command.target).toEqual(composerTarget);
    expect(upload[0]).toMatchObject({ files: [
      { name: 'upload-one.png', media_type: 'image/png', bytes: 3 },
      { name: 'upload-two.png', media_type: 'image/png', bytes: 3 },
    ] });
    const uploadId = `upload-${String(upload[0].request_id)}`;
    const png = '8f8cbb7dcf46e0bc7d53265749a6c17d116093a6ba95e442764060c76fd4a86c';
    const abc = 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad';
    expect(upload[1]).toMatchObject({ upload_id: uploadId, file_index: 0, sequence: 0, data: 'cG5n', sha256: png });
    expect(upload[2]).toMatchObject({ upload_id: uploadId, file_index: 1, sequence: 1, data: 'YWJj', sha256: abc });
    expect(upload[3]).toMatchObject({ upload_id: uploadId, files: [
      { file_index: 0, sha256: png }, { file_index: 1, sha256: abc },
    ] });
    expect(await page.evaluate(() => (window as unknown as { __uploadSlices: unknown[] }).__uploadSlices)).toEqual([
      { name: 'upload-one.png', start: 0, end: 262144 },
      { name: 'upload-two.png', start: 0, end: 262144 },
    ]);
    await prompt.press('Meta+Enter');
    await expect.poll(async () => (await commands(page)).find(command => command.type === 'submit_prompt')).toMatchObject({
      target: composerTarget, text: 'Review upload\nAttachment: attachment:test-1\nAttachment: attachment:test-2',
    });
    await expect(prompt).toHaveValue('');
  });
}

type UploadReadProbe = {
  slices: Array<{ start: number | undefined; end: number | undefined }>;
  reads: number;
  waiting: boolean;
  nativeFinished: boolean;
  settled: boolean;
  settlement?: Promise<void>;
  release(): void;
  restore(): void;
};

async function instrumentUploadRead(page: Page, behavior: 'fail' | 'hold') {
  await page.addInitScript((behavior) => {
    const slice = Blob.prototype.slice;
    const read = Blob.prototype.arrayBuffer;
    let armed = true;
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    const probe: UploadReadProbe = {
      slices: [], reads: 0, waiting: false, nativeFinished: false, settled: false,
      release: () => release(),
      restore: () => { armed = false; Blob.prototype.slice = slice; },
    };
    Object.defineProperty(window, '__uploadReadProbe', { value: probe });
    Blob.prototype.slice = function (start, end, type) {
      const part = slice.call(this, start, end, type);
      if (!(this instanceof File) || this.name !== 'upload-old.png') return part;
      probe.slices.push({ start, end });
      if (probe.slices.length === 2 && armed) {
        Object.defineProperty(part, 'arrayBuffer', { value: () => {
          probe.reads += 1;
          if (behavior === 'fail') {
            armed = false;
            return Promise.reject(new Error('injected upload read failure'));
          }
          const nativeRead = read.call(part);
          probe.waiting = true;
          const result = (async () => {
            const bytes = await nativeRead;
            probe.nativeFinished = true;
            await gate;
            return bytes;
          })();
          probe.settlement = result.then(() => { probe.settled = true; });
          return result;
        } });
      }
      return part;
    };
  }, behavior);
}

const uploadOldBytes = Buffer.concat([Buffer.alloc(262144, 97), Buffer.from('bc')]);

const uploadOldDigest = 'cc485ca6b1916f3751159b548835893e6b1651a4008ded0ed73e0d13513a5a20';

const uploadFirstDigest = 'dd3dde87623d9a6b354c68c943d189c89c63652d945e7bbdf0986cae91a49521';

const uploadLastDigest = '1e0bbd6c686ba050b8eb03ffeedc64fdc9d80947fce821abbe5d6dc8d252c5ac';

const uploadNewDigest = 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad';

function assertOldFirstChunk(chunk: Record<string, unknown>, uploadId: string) {
  expect(chunk).toMatchObject({ type: 'upload_chunk', target: composerTarget, upload_id: uploadId, file_index: 0, sequence: 0, sha256: uploadFirstDigest });
  expect(Buffer.from(String(chunk.data), 'base64')).toEqual(uploadOldBytes.subarray(0, 262144));
}

function assertOldUploadChunks(chunks: Record<string, unknown>[], uploadId: string) {
  expect(chunks).toHaveLength(2);
  expect(chunks[0]).toMatchObject({ type: 'upload_chunk', target: composerTarget, upload_id: uploadId, file_index: 0, sequence: 0, sha256: uploadFirstDigest });
  expect(chunks[1]).toMatchObject({ type: 'upload_chunk', target: composerTarget, upload_id: uploadId, file_index: 0, sequence: 1, sha256: uploadLastDigest });
  expect(Buffer.from(String(chunks[0].data), 'base64')).toEqual(uploadOldBytes.subarray(0, 262144));
  expect(Buffer.from(String(chunks[1].data), 'base64')).toEqual(uploadOldBytes.subarray(262144));
}

for (const mode of ['Conversation', 'Full history']) {
  test(`shared composer ${mode}: chunked upload restarts the retained file after a slice read failure`, async ({ page }) => {
    await instrumentUploadRead(page, 'fail');
    const prompt = await openSharedComposer(page, mode);
    const laterScripts: string[] = [];
    page.on('request', request => { if (request.resourceType() === 'script') laterScripts.push(request.url()); });
    expect(await page.evaluate(() => typeof crypto.subtle.digest)).toBe('function');
    await prompt.fill('Keep recovery draft');
    await page.locator('input[type=file][accept="image/*"]').setInputFiles({ name: 'upload-old.png', mimeType: 'image/png', buffer: uploadOldBytes });
    const restart = page.getByRole('button', { name: 'Restart interrupted files' });
    await expect(restart).toBeEnabled();
    await expect(page.getByRole('alert').filter({ hasText: 'Upload progress is uncertain.' })).toBeVisible();
    await expect(prompt).toHaveValue('Keep recovery draft');
    const before = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
    expect(before.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk']);
    expect(before[0]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-old.png', media_type: 'image/png', bytes: uploadOldBytes.length }] });
    const oldId = `upload-${String(before[0].request_id)}`;
    assertOldFirstChunk(before[1], oldId);
    expect(await page.evaluate(() => {
      const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
      return { reads: probe.reads, slices: probe.slices };
    })).toEqual({ reads: 1, slices: [
      { start: 0, end: 262144 }, { start: 262144, end: 524288 },
    ] });

    // Restore the injected reader, then use Restart without selecting or reloading the file.
    await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.restore());
    await restart.click();
    await expect(prompt).toHaveValue('Keep recovery draft\nAttachment: attachment:test-1\n');
    await expect(page.getByText('Attached upload-old.png')).toBeVisible();
    const after = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
    expect(after.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_cancel', 'upload_begin', 'upload_chunk', 'upload_chunk', 'upload_finish']);
    expect(after[2]).toMatchObject({ target: composerTarget, upload_id: oldId });
    expect(after[3]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-old.png', media_type: 'image/png', bytes: uploadOldBytes.length }] });
    const newId = `upload-${String(after[3].request_id)}`;
    expect(newId).not.toBe(oldId);
    assertOldUploadChunks(after.slice(4, 6), newId);
    expect(after[6]).toMatchObject({ target: composerTarget, upload_id: newId, files: [{ file_index: 0, sha256: uploadOldDigest }] });
    expect(after.filter(command => command.type === 'upload_finish' && command.upload_id === oldId)).toHaveLength(0);
    expect((await prompt.inputValue()).match(/Attachment: attachment:test-1/g)).toHaveLength(1);
    expect(laterScripts).toEqual([]);
  });

  test(`shared composer ${mode}: chunked upload cancellation fences a held read while a new upload succeeds`, async ({ page }) => {
    await instrumentUploadRead(page, 'hold');
    const prompt = await openSharedComposer(page, mode);
    const laterScripts: string[] = [];
    page.on('request', request => { if (request.resourceType() === 'script') laterScripts.push(request.url()); });
    await prompt.fill('Keep canceled draft');
    try {
      await page.locator('input[type=file][accept="image/*"]').setInputFiles({ name: 'upload-old.png', mimeType: 'image/png', buffer: uploadOldBytes });
      await expect.poll(() => page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.waiting)).toBe(true);
      const before = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
      expect(before.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk']);
      expect(before[0]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-old.png', media_type: 'image/png', bytes: uploadOldBytes.length }] });
      const oldId = `upload-${String(before[0].request_id)}`;
      assertOldFirstChunk(before[1], oldId);
      await page.getByRole('button', { name: 'Cancel upload' }).click();
      await expect.poll(async () => (await commands(page)).find(command => command.type === 'upload_cancel')).toMatchObject({ target: composerTarget, upload_id: oldId });
      expect(await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.settled)).toBe(false);
      await page.evaluate(async () => {
        const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
        probe.release();
        await probe.settlement;
        await Promise.resolve();
        await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
      });
      await expect(page.getByText('Attachment upload canceled.')).toBeVisible();
      const input = page.locator('input[type=file][accept="image/*"]');
      await expect(input).toBeEnabled();
      expect((await commands(page)).filter(command => String(command.type).startsWith('upload_')).map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_cancel']);

      await input.setInputFiles({ name: 'upload-new.png', mimeType: 'image/png', buffer: Buffer.from('abc') });
      await expect(prompt).toHaveValue('Keep canceled draft\nAttachment: attachment:test-1\n');
      await expect(page.getByText('Attached upload-new.png')).toBeVisible();
      const after = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
      expect(after.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_cancel', 'upload_begin', 'upload_chunk', 'upload_finish']);
      expect(after[3]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-new.png', media_type: 'image/png', bytes: 3 }] });
      const newId = `upload-${String(after[3].request_id)}`;
      expect(newId).not.toBe(oldId);
      expect(after[4]).toMatchObject({ target: composerTarget, upload_id: newId, file_index: 0, sequence: 0, data: 'YWJj', sha256: uploadNewDigest });
      expect(after[5]).toMatchObject({ target: composerTarget, upload_id: newId, files: [{ file_index: 0, sha256: uploadNewDigest }] });
      expect(await page.evaluate(() => {
        const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
        return { reads: probe.reads, nativeFinished: probe.nativeFinished, settled: probe.settled, slices: probe.slices };
      })).toEqual({ reads: 1, nativeFinished: true, settled: true, slices: [
        { start: 0, end: 262144 }, { start: 262144, end: 524288 },
      ] });
      expect((await commands(page)).filter(command => String(command.type).startsWith('upload_'))).toEqual(after);
      expect(after.filter(command => command.type === 'upload_finish' && command.upload_id === oldId)).toHaveLength(0);
      await expect(prompt).toHaveValue('Keep canceled draft\nAttachment: attachment:test-1\n');
      await expect(page.getByText('Attached upload-new.png')).toBeVisible();
      expect((await prompt.inputValue()).match(/Attachment: attachment:test-1/g)).toHaveLength(1);
      expect(laterScripts).toEqual([]);
    } finally {
      await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.release()).catch(() => {});
    }
  });
}

test('chunked upload distributes the complete pinned upstream license', async ({ page }) => {
  const { readFile } = await import('node:fs/promises');
  const upstream = await readFile(resolve('node_modules/@noble/hashes/LICENSE'));
  const metadata = JSON.parse(await readFile(resolve('node_modules/@noble/hashes/package.json'), 'utf8')) as { name: string; version: string };
  expect(metadata).toMatchObject({ name: '@noble/hashes', version: '2.4.0' });
  expect(upstream.length).toBeGreaterThan(1000);
  const notice = upstream.toString('utf8');
  expect(notice).toContain('Copyright (c) 2022 Paul Miller');
  expect(notice).toContain('Permission is hereby granted');
  expect(notice).toContain('WITHOUT WARRANTY OF ANY KIND');
  await page.goto('/');
  const result = await page.evaluate(async () => {
    const response = await fetch('/licenses/noble-hashes.txt');
    return { ok: response.ok, status: response.status, bytes: Array.from(new Uint8Array(await response.arrayBuffer())) };
  });
  expect(result.status).toBe(200);
  expect(result.ok).toBe(true);
  expect(Buffer.from(result.bytes)).toEqual(upstream);
});

// Hold selected actual mock-wire replies, without changing the shared mock or its defaults.
type OverlapReplyGate = {
  cancel: Record<string, unknown> | null;
  chunk: Record<string, unknown> | null;
  holdChunk: boolean;
  deliver(kind: 'cancel' | 'chunk', fail: boolean): void;
};

async function installOverlapReplyGate(page: Page) {
  await page.evaluate(() => {
    const socket = window.__relaySockets[0];
    const prototype = Object.getPrototypeOf(socket) as { server(message: Record<string, unknown>): void };
    const original = prototype.server;
    let heldCancel = false;
    const gate: OverlapReplyGate = {
      cancel: null, chunk: null, holdChunk: false,
      deliver(kind, fail) {
        const response = gate[kind];
        if (!response) throw new Error(`No held ${kind} response`);
        gate[kind] = null;
        original.call(socket, fail ? { ...response, error: { code: 'attachment_upload_state_unknown' } } : response);
      },
    };
    prototype.server = function (message) {
      if (message.type === 'upload_cancel_result' && !heldCancel) {
        heldCancel = true;
        gate.cancel = message;
        return;
      }
      if (message.type === 'upload_chunk_result' && gate.holdChunk) {
        gate.holdChunk = false;
        gate.chunk = message;
        return;
      }
      original.call(this, message);
    };
    Object.defineProperty(window, '__overlapReplyGate', { value: gate });
  });
}

async function deliverOverlapReply(page: Page, kind: 'cancel' | 'chunk', fail: boolean) {
  await page.evaluate(async ({ kind, fail }) => {
    (window as unknown as { __overlapReplyGate: OverlapReplyGate }).__overlapReplyGate.deliver(kind, fail);
    await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
  }, { kind, fail });
}

async function startHeldOldCancellation(page: Page, mode: string) {
  await instrumentUploadRead(page, 'hold');
  const prompt = await openSharedComposer(page, mode);
  await installOverlapReplyGate(page);
  await prompt.fill('Keep overlap draft');
  const input = page.locator('input[type=file][accept="image/*"]');
  await input.setInputFiles({ name: 'upload-old.png', mimeType: 'image/png', buffer: uploadOldBytes });
  await expect.poll(() => page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.waiting)).toBe(true);
  const before = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
  expect(before.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk']);
  expect(before[0]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-old.png', media_type: 'image/png', bytes: uploadOldBytes.length }] });
  const oldId = `upload-${String(before[0].request_id)}`;
  assertOldFirstChunk(before[1], oldId);
  await page.getByRole('button', { name: 'Cancel upload' }).click();
  await expect.poll(() => page.evaluate(() => (window as unknown as { __overlapReplyGate: OverlapReplyGate }).__overlapReplyGate.cancel !== null)).toBe(true);
  await expect.poll(async () => (await commands(page)).find(command => command.type === 'upload_cancel')).toMatchObject({ target: composerTarget, upload_id: oldId });
  expect(await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.settled)).toBe(false);
  return { prompt, input, oldId };
}

async function releaseCanceledUpload(page: Page, input: Locator) {
  await settleOverlapOldRead(page);
  await expect(input).toBeEnabled();
  await expect(page.locator('.upload-status')).toHaveText('Attachment upload canceled.');
}

async function settleOverlapOldRead(page: Page) {
  await page.evaluate(async () => {
    const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
    probe.release();
    await probe.settlement;
    await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
  });
  expect(await page.evaluate(() => {
    const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
    return { reads: probe.reads, nativeFinished: probe.nativeFinished, settled: probe.settled, slices: probe.slices };
  })).toEqual({ reads: 1, nativeFinished: true, settled: true, slices: [
    { start: 0, end: 262144 }, { start: 262144, end: 524288 },
  ] });
}

for (const mode of ['Conversation', 'Full history']) {
  for (const oldReply of ['success', 'error'] as const) {
    test(`shared composer ${mode}: chunked upload old cancel ${oldReply} preserves a pending new upload and restart`, async ({ page }) => {
      const { prompt, input, oldId } = await startHeldOldCancellation(page, mode);
      try {
        await releaseCanceledUpload(page, input);
        await page.evaluate(() => { (window as unknown as { __overlapReplyGate: OverlapReplyGate }).__overlapReplyGate.holdChunk = true; });
        await input.setInputFiles({ name: 'upload-new.png', mimeType: 'image/png', buffer: Buffer.from('abc') });
        await expect.poll(() => page.evaluate(() => (window as unknown as { __overlapReplyGate: OverlapReplyGate }).__overlapReplyGate.chunk !== null)).toBe(true);
        await expect(page.getByRole('progressbar', { name: 'upload-new.png upload progress' })).toBeVisible();
        await expect(page.getByRole('button', { name: 'Cancel upload' })).toBeEnabled();
        await expect(page.locator('.upload-status')).toHaveText('Uploading 1 attachment…');
        const pending = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
        expect(pending.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_cancel', 'upload_begin', 'upload_chunk']);
        expect(pending[3]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-new.png', media_type: 'image/png', bytes: 3 }] });
        const newId = `upload-${String(pending[3].request_id)}`;
        expect(newId).not.toBe(oldId);
        expect(pending[4]).toMatchObject({ target: composerTarget, upload_id: newId, file_index: 0, sequence: 0, data: 'YWJj', sha256: uploadNewDigest });

        await deliverOverlapReply(page, 'cancel', oldReply === 'error');
        await expect(page.getByRole('progressbar', { name: 'upload-new.png upload progress' })).toBeVisible();
        await expect(page.getByRole('button', { name: 'Cancel upload' })).toBeEnabled();
        await expect(page.locator('.upload-status')).toHaveText('Uploading 1 attachment…');
        await deliverOverlapReply(page, 'chunk', true);
        await expect(page.getByRole('alert').filter({ hasText: 'Upload progress is uncertain.' })).toBeVisible();
        const restart = page.getByRole('button', { name: 'Restart interrupted files' });
        await expect(restart).toBeEnabled();
        await expect(prompt).toHaveValue('Keep overlap draft');
        await restart.click();
        await expect(prompt).toHaveValue('Keep overlap draft\nAttachment: attachment:test-1\n');
        await expect(page.locator('.upload-status')).toHaveText('Attached upload-new.png');
        const after = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
        expect(after.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_cancel', 'upload_begin', 'upload_chunk', 'upload_cancel', 'upload_begin', 'upload_chunk', 'upload_finish']);
        expect(after[5]).toMatchObject({ target: composerTarget, upload_id: newId });
        expect(after[6]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-new.png', media_type: 'image/png', bytes: 3 }] });
        const restartId = `upload-${String(after[6].request_id)}`;
        expect(new Set([oldId, newId, restartId]).size).toBe(3);
        expect(after[7]).toMatchObject({ target: composerTarget, upload_id: restartId, file_index: 0, sequence: 0, data: 'YWJj', sha256: uploadNewDigest });
        expect(after[8]).toMatchObject({ target: composerTarget, upload_id: restartId, files: [{ file_index: 0, sha256: uploadNewDigest }] });
        expect(after.filter(command => command.type === 'upload_finish')).toHaveLength(1);
        expect((await commands(page)).filter(command => String(command.type).startsWith('upload_'))).toEqual(after);
        await expect(prompt).toHaveValue('Keep overlap draft\nAttachment: attachment:test-1\n');
        await expect(page.locator('.upload-status')).toHaveText('Attached upload-new.png');
        expect((await prompt.inputValue()).match(/Attachment: attachment:test-1/g)).toHaveLength(1);
      } finally {
        await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.release()).catch(() => {});
      }
    });

    test(`shared composer ${mode}: chunked upload old cancel ${oldReply} cannot overwrite a completed newer upload`, async ({ page }) => {
      const { prompt, input, oldId } = await startHeldOldCancellation(page, mode);
      try {
        await releaseCanceledUpload(page, input);
        await input.setInputFiles({ name: 'upload-new.png', mimeType: 'image/png', buffer: Buffer.from('abc') });
        await expect(prompt).toHaveValue('Keep overlap draft\nAttachment: attachment:test-1\n');
        await expect(page.locator('.upload-status')).toHaveText('Attached upload-new.png');
        await expect(input).toBeEnabled();
        const after = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
        expect(after.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_cancel', 'upload_begin', 'upload_chunk', 'upload_finish']);
        expect(after[3]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-new.png', media_type: 'image/png', bytes: 3 }] });
        const newId = `upload-${String(after[3].request_id)}`;
        expect(newId).not.toBe(oldId);
        expect(after[4]).toMatchObject({ target: composerTarget, upload_id: newId, file_index: 0, sequence: 0, data: 'YWJj', sha256: uploadNewDigest });
        expect(after[5]).toMatchObject({ target: composerTarget, upload_id: newId, files: [{ file_index: 0, sha256: uploadNewDigest }] });
        await deliverOverlapReply(page, 'cancel', oldReply === 'error');
        await expect(page.locator('.upload-status')).toHaveText('Attached upload-new.png');
        expect((await commands(page)).filter(command => String(command.type).startsWith('upload_'))).toEqual(after);
        await expect(prompt).toHaveValue('Keep overlap draft\nAttachment: attachment:test-1\n');
        await expect(page.locator('.upload-status')).toHaveText('Attached upload-new.png');
      } finally {
        await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.release()).catch(() => {});
      }
    });
  }

  test(`shared composer ${mode}: chunked upload current cancel error remains visible after local release`, async ({ page }) => {
    const { prompt } = await startHeldOldCancellation(page, mode);
    try {
      await deliverOverlapReply(page, 'cancel', true);
      await expect(page.locator('.upload-status')).toHaveText('The relay could not confirm attachment cancellation.');
      await expect(page.locator('.upload-status')).toHaveClass(/error/);
      await settleOverlapOldRead(page);
      await expect(prompt).toHaveValue('Keep overlap draft');
      await expect(page.locator('.upload-status')).toHaveText('The relay could not confirm attachment cancellation.');
      expect((await commands(page)).filter(command => command.type === 'upload_finish')).toHaveLength(0);
    } finally {
      await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.release()).catch(() => {});
    }
  });
}

// Fail the actual mock socket send before its server receives a cancellation.
type ImmediateCancelSendFault = {
  attempts: Record<string, unknown>[];
  restore(): void;
};

for (const mode of ['Conversation', 'Full history']) {
  test(`shared composer ${mode}: chunked upload immediate cancel send failure retains uncertainty`, async ({ page }) => {
    await instrumentUploadRead(page, 'hold');
    const prompt = await openSharedComposer(page, mode);
    await page.evaluate(() => {
      const socket = window.__relaySockets[0];
      const original = socket.send;
      const fault: ImmediateCancelSendFault = {
        attempts: [],
        restore: () => { socket.send = original; },
      };
      socket.send = function (serialized) {
        const packet = JSON.parse(serialized) as Record<string, unknown>;
        if (packet.type === 'upload_cancel') {
          fault.attempts.push(packet);
          throw new Error('injected immediate upload_cancel send failure');
        }
        original.call(this, serialized);
      };
      Object.defineProperty(window, '__immediateCancelSendFault', { value: fault });
    });
    await prompt.fill('Keep immediate cancel draft');
    const input = page.locator('input[type=file][accept="image/*"]');
    const form = page.getByRole('form', { name: 'Send a prompt' });
    const status = page.locator('.upload-status');
    try {
      await input.setInputFiles({ name: 'upload-old.png', mimeType: 'image/png', buffer: uploadOldBytes });
      await expect.poll(() => page.evaluate(() => {
        const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
        return probe.waiting && probe.nativeFinished && !probe.settled;
      })).toBe(true);
      await expect(form).toHaveAttribute('aria-busy', 'true');
      const before = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
      expect(before.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk']);
      expect(before[0]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-old.png', media_type: 'image/png', bytes: uploadOldBytes.length }] });
      expect(typeof before[0].request_id).toBe('string');
      const oldId = `upload-${String(before[0].request_id)}`;
      assertOldFirstChunk(before[1], oldId);
      await page.getByRole('button', { name: 'Cancel upload' }).click();
      const attempts = await page.evaluate(() => (window as unknown as { __immediateCancelSendFault: ImmediateCancelSendFault }).__immediateCancelSendFault.attempts);
      expect(attempts).toHaveLength(1);
      expect(attempts[0]).toMatchObject({ type: 'upload_cancel', target: composerTarget, upload_id: oldId });
      expect(typeof attempts[0].request_id).toBe('string');
      expect(attempts[0].request_id).not.toBe(before[0].request_id);
      expect((await commands(page)).filter(command => String(command.type).startsWith('upload_'))).toEqual(before);
      const held = await page.evaluate(() => {
        const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
        return { waiting: probe.waiting, nativeFinished: probe.nativeFinished, settled: probe.settled, reads: probe.reads, slices: probe.slices };
      });
      expect(held).toEqual({ waiting: true, nativeFinished: true, settled: false, reads: 1, slices: [
        { start: 0, end: 262144 }, { start: 262144, end: 524288 },
      ] });
      console.log('immediate-cancel observation', JSON.stringify({ mode, oldId, attemptedCancel: attempts[0], held, status: await status.textContent(), statusClass: await status.getAttribute('class') }));
      await expect(status).toHaveText('The relay could not confirm attachment cancellation.');
      await expect(status).toHaveClass(/error/);
      await expect(prompt).toHaveValue('Keep immediate cancel draft');

      await settleOverlapOldRead(page);
      await expect(form).toHaveAttribute('aria-busy', 'false');
      await expect(page.getByRole('button', { name: 'Attach photos' })).toBeEnabled();
      await expect(page.getByRole('button', { name: 'Cancel upload' })).toHaveCount(0);
      await expect(status).toHaveText('The relay could not confirm attachment cancellation.');
      await expect(status).toHaveClass(/error/);
      expect((await commands(page)).filter(command => String(command.type).startsWith('upload_'))).toEqual(before);
      await expect(prompt).toHaveValue('Keep immediate cancel draft');

      await page.evaluate(() => (window as unknown as { __immediateCancelSendFault: ImmediateCancelSendFault }).__immediateCancelSendFault.restore());
      await input.setInputFiles({ name: 'upload-new.png', mimeType: 'image/png', buffer: Buffer.from('abc') });
      await expect(prompt).toHaveValue('Keep immediate cancel draft\nAttachment: attachment:test-1\n');
      await expect(status).toHaveText('Attached upload-new.png');
      await expect(status).not.toHaveClass(/error/);
      await expect(form).toHaveAttribute('aria-busy', 'false');
      const after = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
      expect(after.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_begin', 'upload_chunk', 'upload_finish']);
      expect(after.slice(0, 2)).toEqual(before);
      expect(after[2]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-new.png', media_type: 'image/png', bytes: 3 }] });
      const newId = `upload-${String(after[2].request_id)}`;
      expect(newId).not.toBe(oldId);
      expect(after[3]).toMatchObject({ target: composerTarget, upload_id: newId, file_index: 0, sequence: 0, data: 'YWJj', sha256: uploadNewDigest });
      expect(after[4]).toMatchObject({ target: composerTarget, upload_id: newId, files: [{ file_index: 0, sha256: uploadNewDigest }] });
      expect(after.filter(command => command.type === 'upload_finish')).toHaveLength(1);
      expect((await prompt.inputValue()).match(/Attachment: attachment:test-1/g)).toHaveLength(1);
    } finally {
      await page.evaluate(() => {
        (window as unknown as { __immediateCancelSendFault: ImmediateCancelSendFault }).__immediateCancelSendFault.restore();
        (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.release();
      });
    }
  });
}
