import { expect, test } from '@playwright/test';
import { saveDeviceCredential } from '../support/journey-device';
import { commands, connectFedora, handshake, reply, server, setAutoCommands, setConversationFixture, socketCount } from '../support/journey-relay';

for (const provider of ['claude', 'codex', 'opencode', 'qoder', 'pi', 'omp', 'omo', 'hermes', 'cursor', 'kimi']) {
test(`default agent view: fresh ${provider} openings use clean Conversation`, async ({ page }) => {
  await connectFedora(page, { capabilities: ['conversation_history'] });
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
    await connectFedora(page, { capabilities: ['conversation_history', 'manual_terminal_input', 'structured_questions', 'attention_classification'] });
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
    const unknown = () => reply(page, attempt, undefined, { phase: 'dispatched_unknown' });
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
  await connectFedora(page, { capabilities: ['conversation_history'] });
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
  await connectFedora(page, { capabilities: ['conversation_history'] });
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
  await connectFedora(page, { capabilities: ['conversation_history'] });
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
  await connectFedora(page, { capabilities: ['conversation_history'] });
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
  await connectFedora(page, { capabilities: [] });
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
  await connectFedora(page, { capabilities: ['conversation_history'] });
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
  await connectFedora(page, { capabilities: ['conversation_history'] });
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
  await connectFedora(page, { capabilities: ['conversation_history'] });
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
  await connectFedora(page, { capabilities: ['conversation_history'] });
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
  await connectFedora(page, { capabilities: ['conversation_history', 'tab_rename', 'agent_launch'] });
  await saveDeviceCredential(page, { id: 'credential-reader', deviceId: 'device-reader', secret: 'R'.repeat(43), role: 'reader' });
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
