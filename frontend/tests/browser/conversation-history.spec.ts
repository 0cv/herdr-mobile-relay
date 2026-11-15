import { expect, test } from '@playwright/test';
import { scrollConversationToBottom, showOlderConversationEntry } from '../support/journey-conversation';
import { distanceFromBottom } from '../support/journey-device';
import { commands, composerTarget, connectFedora, reply, server, setAutoCommands, setConversationFixture } from '../support/journey-relay';

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
  await connectFedora(page, {
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

test('shows tool-only agent turns only in full history and decodes their arguments', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_default_agent_view', 'terminal');
    localStorage.setItem('herdr-conversation-view', 'conversation');
  });
  await connectFedora(page, {
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
  await connectFedora(page, {
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

  await expect(page.getByText('Turn 60 heading')).toBeVisible();
  // A pin assertion is only meaningful while the transcript really overflows.
  expect(await list.evaluate((element) => element.scrollHeight > element.clientHeight * 3)).toBe(true);
  await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);

  // Turns arriving while the reader sits at the end keep the end in view.
  const streamed = [...entries, {
    id: 'turn-61', timestamp: '2026-08-12T10:00:00Z', role: 'user', text: 'newest streamed question',
  }];
  await setConversationFixture(page, { entries: streamed, total: streamed.length });
  await expect(page.getByText('newest streamed question')).toBeVisible();
  await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);

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
  expect(await distanceFromBottom(list)).toBeGreaterThan(200);

  // Scrolling back to the end restores the pin.
  await scrollConversationToBottom(page);
  const final = [...later, {
    id: 'turn-63', timestamp: '2026-08-12T10:02:00Z', role: 'user', text: 'final streamed question',
  }];
  await setConversationFixture(page, { entries: final, total: final.length });
  await expect(page.getByText('final streamed question')).toBeVisible();
  await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
});

test('loads older conversation automatically when scrolled near the top', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await connectFedora(page, {
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
  await connectFedora(page, {
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
  await connectFedora(page, { capabilities: ['conversation_history'] });
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
    await connectFedora(page, { capabilities: ['conversation_history', ...(availability !== 'no-capability' ? ['workspace_inspection'] : [])] });
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
  await connectFedora(page, {
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

test('phone feedback integrated history loads on upward demand and explicit action with a stable anchor across view changes', async ({ page }) => {
  await page.clock.install();
  await connectFedora(page, { capabilities: ['conversation_history'] });
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
  await reply(page, older, { available: true, entries: entries.slice(24, 48), has_more: true, next_cursor: 'older-0', state: 'ready', mode: 'snapshot', snapshot_id: 'snapshot-1', source_revision: 'integrated:1' });
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
