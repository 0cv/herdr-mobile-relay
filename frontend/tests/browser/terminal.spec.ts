import { expect, test } from '@playwright/test';
import { commands, connectFedora, handshake, openAgent, openOmpPane, replyToPaneRead, server, setAutoCommands } from '../support/journey-relay';

test('keeps Cursor filtering text-only across status updates and restores prompts after selecting', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'blocked', attention_kind: 'unknown', project: 'Cursor picker', agent: 'cursor' });
  const footer = 'Type to filter • Enter to select • Tab to edit';
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: `Available models\n${footer}`,
  });
  const input = page.getByPlaceholder('Type filter text…');
  const mutations = async () => (await commands(page)).filter((command) =>
    ['send_filter_text', 'send_input', 'send_text', 'send_keys', 'submit_prompt'].includes(String(command.type)));
  await input.fill('grok');
  expect(await mutations()).toEqual([]);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'done', attention_kind: 'unknown', project: 'Cursor picker', agent: 'cursor' }],
  });
  await expect(input).toBeEnabled();
  await expect(input).toHaveValue('grok');
  await expect(page.getByRole('button', { name: 'Attach files' })).toBeDisabled();
  await page.getByRole('button', { name: 'Send filter text' }).click();
  await expect.poll(mutations).toEqual([
    expect.objectContaining({ type: 'send_filter_text', text: 'grok' }),
  ]);
  expect((await mutations())[0]).not.toHaveProperty('keys');
  await expect(input).toBeEnabled();
  await expect(input).toHaveValue('');
  await page.getByRole('button', { name: 'Arrow keys' }).click();
  await page.getByRole('button', { name: 'Down', exact: true }).click();
  await page.getByRole('button', { name: 'Enter', exact: true }).click();
  await expect.poll(mutations).toEqual([
    expect.objectContaining({ type: 'send_filter_text', text: 'grok' }),
    expect.objectContaining({ type: 'send_keys', keys: ['Down'] }),
    expect.objectContaining({ type: 'send_keys', keys: ['Enter'] }),
  ]);
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain',
    content: `${footer}\nSelected model: Grok Fast\n>`,
  });
  await expect(page.getByRole('combobox', { name: 'Prompt', exact: true })).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'done', attention_kind: 'unknown', project: 'Cursor picker', agent: 'cursor' }],
  });
  const prompt = page.getByPlaceholder('Type a reply…');
  await expect(prompt).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Attach files' })).toBeEnabled();
  await prompt.fill('Continue');
  await page.getByRole('button', { name: 'Send prompt', exact: true }).click();
  await expect.poll(mutations).toEqual([
    expect.objectContaining({ type: 'send_filter_text', text: 'grok' }),
    expect.objectContaining({ type: 'send_keys', keys: ['Down'] }),
    expect.objectContaining({ type: 'send_keys', keys: ['Enter'] }),
    expect.objectContaining({ type: 'submit_prompt', text: 'Continue' }),
  ]);
});

test('centers plan keys and enables text only for the terminal editor', async ({ page }) => {
  await connectFedora(page, {
    capabilities: ['structured_questions', 'slash_commands', 'manual_terminal_input'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'blocked', project: 'Old controls', agent: 'opencode',
      options: ['Approve once', 'Deny'],
    }],
  });
  await expect(page.getByRole('heading', { name: 'Needs inspection' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Approve once' })).toBeHidden();
  await page.getByRole('button', { name: 'Open Old controls on Fedora' }).click();
  const terminal = page.getByRole('log');
  const history = Array.from({ length: 120 }, (_, index) => `question history ${index + 1}`);
  const choiceContent = [
    ...history,
    'Other (type your own)',
    'Enter select · n note · ↑/↓ move · Tab/←/→ · Esc cancel',
  ].join('\n');
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: choiceContent,
  });
  const planInput = page.getByRole('combobox', { name: 'Prompt', exact: true });
  await expect(planInput).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Attach files' })).toBeDisabled();
  const directions = await page.locator('.generic-menu-actions > div > button').evaluateAll((elements) =>
    elements.slice(0, 4).map((element) => ({
      key: element.querySelector('kbd')?.textContent,
      label: element.textContent?.replace(element.querySelector('kbd')?.textContent || '', '').trim(),
    })));
  expect(directions).toEqual([
    { key: 'Left', label: 'Previous' },
    { key: 'Up', label: 'Up' },
    { key: 'Down', label: 'Down' },
    { key: 'Right', label: 'Next' },
  ]);
  const promptContent = [
    ...history,
    'Custom answer: Which weekend?',
    '>',
    'enter or ctrl+q submit  esc cancel  ctrl+g external editor',
  ].join('\n');
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content: promptContent,
  });
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  const beforeManual = (await commands(page)).findLast(command => command.type === 'read_pane')?.request_id;
  await page.getByRole('button', { name: 'Keyboard', exact: true }).click();
  const terminalInput = page.getByRole('textbox', { name: 'Terminal text', exact: true });
  await expect(terminalInput).toBeEnabled();
  await expect.poll(async () => (await commands(page)).findLast(command => command.type === 'read_pane')?.request_id).not.toBe(beforeManual);
  await replyToPaneRead(page, promptContent);
  await terminalInput.fill('custom weekend');
  await expect(page.getByRole('button', { name: 'Send text', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Send text', exact: true }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => ['send_terminal_text', 'send_input', 'send_text', 'send_keys'].includes(String(command.type)))
    .map((command) => ({ type: command.type, text: command.text, keys: command.keys }))).toEqual([
      { type: 'send_terminal_text', text: 'custom weekend', keys: undefined },
    ]);
  await page.getByRole('button', { name: 'Close', exact: true }).click();
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: `${promptContent}\nAccepted custom weekend`,
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Old controls', agent: 'opencode' }],
  });
  await expect.poll(async () => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);
  await expect(page.getByRole('button', { name: 'Jump to latest' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Enter' })).toBeDisabled();

  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'blocked', attention_kind: 'chat',
      project: 'Chat ready', agent: 'codex',
    }],
  });
  await expect(page.getByPlaceholder('Type a reply…')).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Approve once' })).toBeHidden();
});

test('reports when Herdr clips older terminal history', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'Clipped app', agent: 'codex' });
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: 'visible recent output',
    truncated: true,
  });
  const clippedNotice = page.getByRole('status').filter({ hasText: 'Older terminal history is not shown' });
  await expect(clippedNotice).toBeVisible();

  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: 'complete output',
    truncated: false,
  });
  await expect(clippedNotice).toBeHidden();
});

test('applies relay-watched terminal deltas and pauses the watcher when hidden', async ({ page }) => {
  await page.clock.install();
  await openAgent(page, { pane_id: 'w1:p1', status: 'working', project: 'Realtime app', agent: 'codex' }, {
    capabilities: ['attention_classification', 'pane_realtime_delta', 'slash_commands'],
  });
  await expect.poll(async () =>
    (await commands(page)).some((command) => command.type === 'read_pane')).toBe(true);
  const initialRead = (await commands(page)).findLast(command => command.type === 'read_pane')!;
  await server(page, 0, {
    request_id: initialRead.request_id, target: initialRead.target,
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: 'initial terminal output\nstable row\nthird row\n',
    content_fingerprint: 'content-1',
  });
  await expect(page.getByRole('log')).toContainText('initial terminal output');

  await expect.poll(async () =>
    (await commands(page)).findLast((command) => command.type === 'watch_pane')).toMatchObject({
    type: 'watch_pane',
    pane_id: 'w1:p1',
    content_fingerprint: 'content-1',
  });
  await server(page, 0, {
    type: 'pane_delta',
    pane_id: 'w1:p1',
    format: 'ansi',
    base_fingerprint: 'content-1',
    content_fingerprint: 'content-2',
    segments: [
      { copy_lines: 3 },
      { text: 'fresh terminal output\n' },
    ],
  });
  await expect(page.getByRole('log')).toContainText('fresh terminal output');
  await expect.poll(async () =>
    (await commands(page)).findLast((command) => command.type === 'pane_applied')).toMatchObject({
    type: 'pane_applied',
    pane_id: 'w1:p1',
    content_fingerprint: 'content-2',
  });

  const activeCommandCount = (await commands(page)).length;
  await page.clock.runFor(750);
  expect(await commands(page)).toHaveLength(activeCommandCount);
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(async () =>
    (await commands(page)).findLast((command) => command.type === 'unwatch_pane')).toMatchObject({
    type: 'unwatch_pane',
    pane_id: 'w1:p1',
  });
  const hiddenCommandCount = (await commands(page)).length;
  await page.clock.runFor(750);
  expect(await commands(page)).toHaveLength(hiddenCommandCount);
  const terminal = page.getByRole('log');
  const cachedScreen = await terminal.locator('.term-screen').innerHTML();
  await page.evaluate(() => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
  });
  await expect.poll(async () =>
    (await commands(page)).findLast((command) => command.type === 'read_pane')).toMatchObject({
    type: 'read_pane',
    pane_id: 'w1:p1',
    content_fingerprint: 'content-2',
  });
  await expect(terminal.locator('.term-screen')).toHaveJSProperty('innerHTML', cachedScreen);
  const resumedRead = (await commands(page)).findLast(command => command.type === 'read_pane')!;
  await server(page, 0, {
    request_id: resumedRead.request_id, target: resumedRead.target,
    type: 'pane_unchanged',
    pane_id: 'w1:p1',
    content_fingerprint: 'content-2',
  });
  await expect(terminal.locator('.term-screen')).toHaveJSProperty('innerHTML', cachedScreen);
  await expect.poll(async () =>
    (await commands(page)).findLast((command) => command.type === 'watch_pane')).toMatchObject({
    type: 'watch_pane',
    pane_id: 'w1:p1',
    content_fingerprint: 'content-2',
  });
});

test('resets drafts and terminal output when moving to another agent', async ({ page }) => {
  await connectFedora(page);
  await server(page, 0, {
    type: 'agents',
    agents: [
      { pane_id: 'w1:p1', status: 'working', project: 'Working A', agent: 'codex' },
      { pane_id: 'w1:p2', status: 'blocked', attention_kind: 'approval', project: 'Blocked B', agent: 'claude', options: ['Approve once', 'Deny'] },
    ],
  });
  await page.getByRole('button', { name: 'Open Working A on Fedora' }).click();
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'private output from agent A' });
  await expect(page.getByRole('log')).toContainText('private output from agent A');
  await page.getByRole('combobox', { name: 'Prompt' }).fill('draft intended only for A');

  await page.getByRole('button', { name: 'Next blocked' }).click();

  await expect(page.getByRole('main', { name: 'Terminal for Blocked B' })).toBeVisible();
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toHaveValue('');
  await expect(page.getByRole('log')).not.toContainText('private output from agent A');
});

test('keeps Claude desktop prompt and status in the shared terminal output', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'working', project: 'Wrapped status', agent: 'claude' });
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: [
      ...Array.from({ length: 8 }, (_, index) => `Conversation output ${index + 1}`),
      '❯ Try "edit Info.plist to..."',
      '─'.repeat(100),
      'Opus 4.8',
      'ctx: -',
      'main ~16',
      '/rc ⏸ manual mode on · ← for agents',
    ].join('\n'),
  });
  const terminal = page.getByRole('log');
  await expect(terminal).toContainText('Conversation output 8');
  await expect(terminal).toContainText('edit Info.plist');
  await expect(terminal).toContainText('Opus 4.8');
  await expect(terminal).toContainText('ctx: -');
  await expect(terminal).toContainText('manual mode');
});

test('keeps wide controller panes aligned and horizontally scrollable', async ({ page }) => {
  await connectFedora(page, {
    capabilities: ['attention_classification', 'slash_commands'],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Wide pane app', agent: 'claude' }],
  });
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Open Wide pane app on Fedora' }).click();
  const wideRow = `${'the pane is far wider than the phone viewport '.repeat(3)}and keeps going`;
  // A box border padded out to the pane's width. Preserved spaces may hang
  // past the edge instead of wrapping, which WebKit reports as scrollable
  // width, handing the reader a pane that pans sideways.
  const paddedRow = `╰─${' '.repeat(150)}─╯`;
  // Box-drawing rows must retain their desktop-width column alignment.
  const gridRow = `│ ${'aligned cell '.repeat(12)}│`;
  // Long unbroken tokens are routine in agent output.
  const tokenRow = `https://example.test/${'x'.repeat(300)}`;
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: ['first row', wideRow, paddedRow, gridRow, tokenRow, 'last row'].join('\n'),
  });

  const terminal = page.getByRole('log');
  const lines = terminal.locator('.ansi-line');
  await expect(lines).toHaveCount(6);

  // Preserve desktop alignment and permit horizontal scrolling, rather than
  // claiming that browser wrapping creates an independently sized TUI.
  const geometry = await terminal.evaluate((element) => {
    const rows = Array.from(element.querySelectorAll<HTMLElement>('.ansi-line'));
    const lineHeight = Number.parseFloat(getComputedStyle(element).lineHeight);
    const wide = rows[1];
    // A word split across lines reports more than one client rect. A word too
    // long to fit a line has to break; one that would have fit must not.
    let brokenWords = 0;
    const walker = document.createTreeWalker(wide, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const text = node.textContent || '';
      const pattern = /\S+/g;
      for (let match = pattern.exec(text); match; match = pattern.exec(text)) {
        const range = document.createRange();
        range.setStart(node, match.index);
        range.setEnd(node, match.index + match[0].length);
        const rects = Array.from(range.getClientRects());
        const advance = rects.reduce((total, rect) => total + rect.width, 0);
        if (rects.length > 1 && advance <= element.clientWidth) brokenWords += 1;
      }
    }
    return {
      lineHeight,
      wideHeight: wide.getBoundingClientRect().height,
      narrowHeight: rows[0].getBoundingClientRect().height,
      brokenWords,
      scrollWidth: element.scrollWidth,
      clientWidth: element.clientWidth,
    };
  });
  expect(geometry.lineHeight).toBeGreaterThan(0);
  expect(geometry.narrowHeight).toBeLessThan(geometry.lineHeight * 1.5);
  expect(geometry.wideHeight).toBeLessThan(geometry.lineHeight * 1.5);
  expect(geometry.brokenWords).toBe(0);
  expect(geometry.scrollWidth).toBeGreaterThan(geometry.clientWidth);
});

test('wraps wide terminal output for a paired reader credential', async ({ page }) => {
  // A reader never leases, even opted in on a relay that supports it.
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'true'));
  await connectFedora(page, { capabilities: ['attention_classification', 'pane_size_opt_in'] });
  await page.evaluate(() => {
    localStorage.setItem('herdr_device_auth_v1', JSON.stringify({
      version: 1,
      relays: {
        fedora: {
          kind: 'credential',
          id: 'credential-reader',
          version: 1,
          secret: 'R'.repeat(43),
          deviceId: 'device-reader',
          role: 'reader',
          locale: 'en',
          issuedAt: Date.now(),
        },
      },
    }));
  });
  // Push a fresh connection snapshot so App's derived reader-role state observes
  // the persisted credential before the agent view is rendered.
  await handshake(page, 0, { capabilities: ['attention_classification', 'pane_size_opt_in'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Reader pane', agent: 'claude' }],
  });
  await page.getByRole('button', { name: 'Open Reader pane on Fedora' }).click();
  const wideRow = `https://example.test/${'reader-output'.repeat(40)}`;
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: wideRow,
  });

  const reader = page.getByRole('main', { name: 'Terminal for Reader pane', exact: true });
  const readOnlyFeedback = reader.getByRole('status').filter({ hasText: 'Reader access is read only' });
  await expect(readOnlyFeedback).toContainText('Reader access is read only');
  await expect(readOnlyFeedback).toBeVisible();
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toBeDisabled();
  const terminal = page.getByRole('log');
  const geometry = await terminal.evaluate((element) => {
    const row = element.querySelector<HTMLElement>('.ansi-line')!;
    return {
      rowHeight: row.getBoundingClientRect().height,
      lineHeight: Number.parseFloat(getComputedStyle(row).lineHeight),
      scrollWidth: element.scrollWidth,
      clientWidth: element.clientWidth,
    };
  });
  expect(geometry.rowHeight).toBeGreaterThan(geometry.lineHeight * 1.5);
  expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.clientWidth + 1);
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await page.getByRole('navigation', { name: 'Application' })
    .getByRole('button', { name: /^Settings(?:,|$)/ }).click();
  expect((await commands(page)).filter((command) => command.type === 'lease_pane_size')).toHaveLength(0);
  await expect(page.getByRole('switch', { name: 'Resize Session', exact: true }))
    .toHaveAccessibleDescription(/controller terminals keep the desktop width and scroll horizontally; read-only reader panes wrap to the phone width\./);
});

test('does not repopulate prompts after an unsafe dispatch failure', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'Unsafe retry app', agent: 'codex' });
  const prompt = page.getByRole('combobox', { name: 'Prompt' });
  await prompt.fill('run this only once');
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).some((entry) => entry.type === 'submit_prompt')).toBe(true);
  const command = (await commands(page)).find((entry) => entry.type === 'submit_prompt')!;
  await server(page, 0, {
    type: 'command_result',
    request_id: (command as Record<string, unknown>).request_id,
    action: 'submit_prompt',
    ok: false,
    phase: 'dispatched_unknown',
    error: 'Command may have executed; review the agent before retrying',
    data: { dispatched_unknown: true },
  });
  await expect(prompt).toHaveValue('');
});

test('preserves unsafe prompt state for older relays without error data', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'Legacy retry app', agent: 'codex' });
  const prompt = page.getByRole('combobox', { name: 'Prompt' });
  await prompt.fill('run this only once');
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).some((entry) => entry.type === 'submit_prompt')).toBe(true);
  const command = (await commands(page)).find((entry) => entry.type === 'submit_prompt')!;
  await server(page, 0, {
    type: 'command_result',
    request_id: (command as Record<string, unknown>).request_id,
    action: 'submit_prompt',
    ok: false,
    phase: 'dispatched_unknown',
    error: 'Command may have executed; review the agent before retrying',
  });
  await expect(prompt).toHaveValue('');
});

test('keeps the Pi desktop UI separate from the generic mobile composer', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'working', project: 'Pi app', agent: 'pi' });
  const rule = '─'.repeat(120);
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: [
      '\u001b[38;2;166;227;161mConversation output\u001b[0m', '', rule, '@\u001b[7m \u001b[0m', rule,
      '  .gitattributes                   .gitattributes',
      '→ AGENTS.md                        AGENTS.md',
      '  frontend/                        frontend', '  (4/20)',
      '\u001b[2m~/Development/herdr-mobile-relay (main)\u001b[0m',
      `\u001b[2m$0.000 (sub) 0.0%/272k (auto)${' '.repeat(80)}gpt-5.6-sol • xhigh\u001b[0m`,
    ].join('\n'),
  });

  const terminal = page.getByRole('log');
  const prompt = page.getByRole('combobox', { name: 'Prompt' });
  await expect(terminal).not.toHaveClass(/bottom-ui-terminal/);
  await expect(prompt).toHaveValue('');
  await expect(terminal).toContainText('Conversation output');
  await expect(terminal).toContainText('@');
  await expect(terminal).toContainText('.gitattributes');
  await expect(terminal).toContainText('AGENTS.md');
  await expect(terminal).toContainText('~/Development/herdr-mobile-relay');
  await expect(terminal).toContainText('gpt-5.6-sol');
  await expect(terminal.locator('.agent-current-ui-start')).toHaveCount(0);

  await prompt.fill('@AGENTS.md');
  await page.getByRole('button', { name: 'Send prompt' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'submit_prompt')).toMatchObject({
    pane_id: 'w1:p1', text: '@AGENTS.md',
  });
  expect((await commands(page)).filter((command) => (
    command.type === 'send_keys' && JSON.stringify(command.keys) === JSON.stringify(['ctrl+c'])
  ))).toHaveLength(0);
});

test('keeps the Codex picker in the shared terminal with generic controls', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'Codex placeholder', agent: 'codex' });
  const background = '\u001b[48;2;61;64;64m                    \u001b[0m';
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: [
      'Completed output', background,
      '\u001b[1;48;2;61;64;64m›\u001b[0m\u001b[48;2;61;64;64m @\u001b[0m',
      background,
      '> Default templates          Default templates for documents and presentations       Plugin',
      '  Analytics Dashboard        Create spreadsheets with the dashboard template         Skill',
      '  enter insert · esc close · ←/→ switch search modes                      [All Results]   Filesystem Only    Plugins',
    ].join('\n'),
  });

  const terminal = page.getByRole('log');
  const prompt = page.getByRole('combobox', { name: 'Prompt' });
  await expect(terminal).not.toHaveClass(/bottom-ui-terminal/);
  await expect(terminal).toContainText('Completed output');
  await expect(terminal).toContainText('Default templates');
  await expect(terminal).toContainText('Analytics Dashboard');
  await expect(terminal).toContainText('All Results');
  await expect(terminal.locator('.codex-picker-item')).toHaveCount(0);
  await expect(prompt).toHaveValue('');
  await expect(page.getByRole('button', { name: 'Previous result' })).toHaveCount(0);

  await page.getByRole('button', { name: 'Arrow keys' }).click();
  await page.getByRole('button', { name: 'Right', exact: true }).click();
  await expect.poll(async () => (await commands(page)).find((command) => (
    command.type === 'send_keys' && JSON.stringify(command.keys) === JSON.stringify(['Right'])
  ))).toMatchObject({ pane_id: 'w1:p1', keys: ['Right'] });
});

test('finds and highlights matches across virtualized terminal output', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'working', project: 'Search app', agent: 'codex' });
  const lines = Array.from({ length: 320 }, (_, index) => {
    if (index === 24) return 'needle early result';
    if (index === 286) return 'needle late result';
    return `terminal row ${index}`;
  });
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'ansi',
    content: lines.join('\n'),
  });

  await page.getByRole('button', { name: 'Find in terminal', exact: true }).click();
  await expect(page.getByText('Type to find', { exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Previous match' }).locator('svg.find-action-symbol')).toHaveCount(1);
  await expect(page.getByRole('button', { name: 'Next match' }).locator('svg.find-action-symbol')).toHaveCount(1);
  await expect(page.getByRole('button', { name: 'Close find' }).locator('svg.find-action-symbol')).toHaveCount(1);
  const search = page.getByRole('searchbox', { name: 'Find in terminal output' });
  await search.fill('needle');
  await expect(page.getByText('1 of 2', { exact: true })).toBeVisible();
  await expect(page.locator('mark.terminal-find-match.active')).toHaveText('needle');
  await expect(page.getByRole('log')).toContainText('needle early result');

  await page.getByRole('button', { name: 'Next match' }).click();
  await expect(page.getByText('2 of 2', { exact: true })).toBeVisible();
  await expect(page.locator('mark.terminal-find-match.active')).toHaveText('needle');
  await expect(page.getByRole('log')).toContainText('needle late result');

  await search.press('Escape');
  await expect(search).toBeHidden();
  await expect(page.locator('mark.terminal-find-match')).toHaveCount(0);
});

test('keeps the terminal key row compact on touch screens', async ({ page }) => {
  await openOmpPane(page, 'Key row');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'ready' });
  const esc = page.getByRole('button', { name: 'Esc', exact: true }).last();
  await expect(esc).toBeVisible();
  expect(await page.evaluate(() => matchMedia('(pointer: coarse)').matches)).toBe(true);
  expect((await esc.boundingBox())!.height).toBeLessThan(44);
});

test('refreshes agents on return home and preserves shared terminal behavior', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'working', project: 'Terminal app', agent: 'opencode', server_session_id: 'session-1', terminal_id: 'terminal-1', generation: 1 });
  await expect(page.getByRole('img', { name: 'Agent working' })).toBeVisible();
  const attachFiles = page.getByRole('button', { name: 'Attach files' });
  const arrowKeys = page.getByRole('button', { name: 'Arrow keys' });
  const enterKey = page.getByRole('button', { name: 'Enter' });
  const tabKey = page.getByRole('button', { name: 'Tab', exact: true });
  const ctrlKey = page.getByRole('button', { name: 'Ctrl', exact: true });
  const shiftKey = page.getByRole('button', { name: 'Shift', exact: true });
  const altKey = page.getByRole('button', { name: 'Alt', exact: true });
  const modifierLetter = page.getByRole('textbox', { name: 'Modifier shortcut character' });
  const copyOutput = page.getByRole('button', { name: 'Copy', exact: true });
  await expect(attachFiles.locator('svg')).toBeVisible();
  await expect(arrowKeys.locator('svg')).toBeVisible();
  await expect(enterKey).toBeVisible();
  await expect(ctrlKey).toBeVisible();
  await expect(shiftKey).toBeVisible();
  await expect(altKey).toBeVisible();
  await expect(copyOutput).toBeVisible();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'false');
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'false');
  await expect(altKey).toHaveAttribute('aria-pressed', 'false');
  await expect(attachFiles).not.toContainText('▧');
  await expect(arrowKeys).not.toContainText('⌨');
  await arrowKeys.click();
  await expect(page.getByRole('button', { name: 'Up', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: /^(Home|End|Page up|Page down)$/ })).toHaveCount(0);
  await expect(page.locator('.arrow-popup').getByRole('button', { name: 'Enter' })).toHaveCount(0);
  await enterKey.click();
  expect((await commands(page)).find((command) => command.type === 'send_keys')).toMatchObject({
    pane_id: 'w1:p1', keys: ['Enter'],
  });
  await shiftKey.click();
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await modifierLetter.press('c');
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['shift+c'])).length).toBe(1);
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await modifierLetter.press('d');
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['shift+d'])).length).toBe(1);
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await ctrlKey.click();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await modifierLetter.press('c');
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['ctrl+shift+c'])).length).toBe(1);
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await shiftKey.click();
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'false');
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await modifierLetter.press('o');
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['ctrl+o'])).length).toBe(1);
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await ctrlKey.click();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'false');
  await expect(page.locator('#modifier-key-input')).not.toBeFocused();
  await expect(modifierLetter).toHaveCount(0);
  await tabKey.click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['Tab'])).length).toBe(1);
  await shiftKey.click();
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await tabKey.click();
  await tabKey.click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['shift+tab'])).length).toBe(2);
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await ctrlKey.click();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await tabKey.click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['ctrl+shift+tab'])).length).toBe(1);
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await arrowKeys.click();
  await page.getByRole('button', { name: 'Up', exact: true }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['ctrl+shift+up'])).length).toBe(1);
  await altKey.click();
  await expect(altKey).toHaveAttribute('aria-pressed', 'true');
  await modifierLetter.press('x');
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['ctrl+alt+shift+x'])).length).toBe(1);
  await altKey.click();
  await enterKey.click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'send_keys'
      && JSON.stringify(command.keys) === JSON.stringify(['ctrl+shift+enter'])).length).toBe(1);
  await expect(page.getByRole('status').filter({ hasText: 'Ctrl+Shift+Enter sent' })).toBeVisible();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await ctrlKey.click();
  await shiftKey.click();
  await expect(page.locator('#modifier-key-input')).not.toBeFocused();
  await expect(modifierLetter).toHaveCount(0);
  await shiftKey.click();
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'true');
  await expect(modifierLetter).toBeFocused();
  await page.getByRole('combobox', { name: 'Prompt' }).focus();
  await expect(shiftKey).toHaveAttribute('aria-pressed', 'false');
  await expect(page.locator('#modifier-key-input')).not.toBeFocused();
  await expect(modifierLetter).toHaveCount(0);
  const refreshesBeforeBack = (await commands(page)).filter((command) => command.type === 'refresh_agents').length;
  await page.getByRole('button', { name: 'Back' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'refresh_agents').length)
    .toBe(refreshesBeforeBack + 1);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Terminal app', agent: 'opencode', server_session_id: 'session-1', terminal_id: 'terminal-1', generation: 1 }] });
  await expect(page.getByRole('button', { name: 'Open Terminal app on Fedora' })).toBeVisible();
  await page.getByRole('button', { name: 'Open Terminal app on Fedora' }).click();
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: '     + Thought: 1.0s\n\u001b[38;5;6m     Safe <img src=x onerror=alert(1)>\u001b[0m\n     Docs https://example.com/report?q=1\n     Details\n     ▣ Build · model · 1s' });
  const terminal = page.getByRole('log');
  await expect(terminal).toContainText('Safe <img src=x onerror=alert(1)>');
  expect(await terminal.locator('img').count()).toBe(0);
  const terminalLink = terminal.getByRole('link', { name: 'https://example.com/report?q=1' });
  await expect(terminalLink).toHaveAttribute('target', '_blank');
  await expect(terminalLink).toHaveAttribute('rel', 'noopener noreferrer');
  await expect(terminalLink).toHaveAttribute('referrerpolicy', 'no-referrer');
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
  await copyOutput.click();
  await expect.poll(() => page.evaluate(
    () => (window as typeof window & { __copiedTerminal?: string }).__copiedTerminal,
  )).toBe('Safe <img src=x onerror=alert(1)>\nDocs https://example.com/report?q=1\nDetails');
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: [
      '     + Thought: 0.08s',
      '     Updated the canonical plan to:',
      '',
      '     - Final plan consistency assertions pass.',
      '     ▣ Build · model · 0.08s',
      '     ※ recap: Goal: implement the strategy.',
    ].join('\n'),
  });
  await copyOutput.click();
  await expect.poll(() => page.evaluate(
    () => (window as typeof window & { __copiedTerminal?: string }).__copiedTerminal,
  )).toBe('Updated the canonical plan to:\n\n- Final plan consistency assertions pass.');

  const permissionHeader = terminal.locator('.ansi-line', { hasText: 'Permissions ·' });
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: 'Permissions ·  \u001b[48;2;36;74;50mAllow\u001b[0m Ask Deny',
  });
  await expect(permissionHeader.locator('span', { hasText: 'Allow' }))
    .toHaveCSS('background-color', 'rgb(36, 74, 50)');
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: 'Permissions ·  Allow Ask \u001b[48;2;36;74;50mDeny\u001b[0m',
  });
  await expect(permissionHeader.locator('span', { hasText: 'Deny' }))
    .toHaveCSS('background-color', 'rgb(36, 74, 50)');

  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: ['Before', '', '', '', '', '----------------', '', '————————', '', '________________', '', '', '', 'After'].join('\n'),
  });
  expect(await terminal.evaluate((element) => {
    const rows = element.querySelector('.term-screen')?.children || element.children;
    let blankRun = 0;
    let maximumBlankRun = 0;
    for (const row of rows) {
      if (row.classList.contains('ansi-line') && !row.textContent?.trim()) {
        blankRun += 1;
        maximumBlankRun = Math.max(maximumBlankRun, blankRun);
      } else blankRun = 0;
    }
    return {
      maximumBlankRun,
      separators: element.querySelectorAll('.term-separator').length,
    };
  })).toEqual({ maximumBlankRun: 4, separators: 0 });

  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: `─ Worked for 1m 46s ${'─'.repeat(120)}`,
  });
  await expect(terminal.locator('.ansi-line')).toHaveText(`─ Worked for 1m 46s ${'─'.repeat(120)}`);

  const claudeRule = '─'.repeat(120);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Terminal app', agent: 'claude', server_session_id: 'session-1', terminal_id: 'terminal-1', generation: 1 }],
  });
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: [
      `\u001b[36m│\u001b[0m  Claude result${' '.repeat(80)}\u001b[36m│\u001b[0m`,
      '\u001b[36m│\u001b[0m',
      `\u001b[36m╰${claudeRule}╯\u001b[0m`,
      `\u001b[36m${claudeRule}\u001b[0m`,
      `\u001b[36m╭${claudeRule}╮\u001b[0m`,
      `\u001b[38;5;147m${'▔'.repeat(150)}\u001b[0m`,
      `\u001b[2m${claudeRule} Opus 4.8 | ctx: 20%\u001b[0m`,
    ].join('\n'),
  });
  await expect(terminal.locator('.ansi-line').filter({ hasText: 'Claude result' })).toContainText('Claude result');
  await expect(terminal.locator('.ansi-line').filter({ hasText: 'Opus 4.8' })).toContainText('Opus 4.8 | ctx: 20%');
  await expect(terminal.locator('.term-separator')).toHaveCount(0);

  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: `${'.'.repeat(120)} [29%]`,
  });
  await expect(terminal.locator('.ansi-line')).toHaveText(`${'.'.repeat(120)} [29%]`);

  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    content: '\u001b[48;2;250;250;250;38;2;20;20;20mMac light terminal\u001b[0m',
  });
  const normalizedMacRow = terminal.locator('.ansi-line', { hasText: 'Mac light terminal' });
  await expect(normalizedMacRow).toHaveCSS('background-color', 'rgb(61, 64, 64)');
  await expect(normalizedMacRow.locator('span')).toHaveCSS('color', 'rgb(236, 239, 244)');

  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await composer.focus();
  await composer.fill('draft prompt');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'newest live frame' });
  await expect(page.getByRole('log')).toContainText('newest live frame');
  await expect(composer).toBeFocused();
  await expect(composer).toHaveValue('draft prompt');
  await composer.evaluate((element) => (element as HTMLTextAreaElement).blur());

  const longFrame = Array.from({ length: 120 }, (_, index) => `terminal line ${index}`).join('\n');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: longFrame });
  await terminal.evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event('scroll'));
  });
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: `${longFrame}\nlatest output` });
  expect(await terminal.evaluate(element => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeGreaterThan(2);
  await terminal.evaluate(element => { element.scrollTop = element.scrollHeight; element.dispatchEvent(new Event('scroll')); });
  await expect.poll(() => terminal.evaluate((element) =>
    element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThan(2);

  await page.locator('input[type=file][accept="image/*"]').setInputFiles({ name: 'shot.png', mimeType: 'image/png', buffer: Buffer.from('png') });
  await expect(composer).toHaveValue(/Attachment: attachment:test-1/);
  expect((await commands(page)).find((command) => command.type === 'upload_begin')).toMatchObject({
    files: [{ name: 'shot.png', media_type: 'image/png', bytes: 3 }],
    protocol: 3,
    target: {
      server_session_id: 'session-1',
      pane_id: 'w1:p1',
      terminal_id: 'terminal-1',
      generation: 1,
    },
  });
  expect((await commands(page)).find((command) => command.type === 'upload_chunk')).toMatchObject({
    data: 'cG5n',
    sha256: '8f8cbb7dcf46e0bc7d53265749a6c17d116093a6ba95e442764060c76fd4a86c',
    protocol: 3,
  });

  await composer.fill('send this');
  await page.getByRole('button', { name: 'Send prompt' }).click();
  expect((await commands(page)).find((command) => command.type === 'submit_prompt')).toMatchObject({ text: 'send this', pane_id: 'w1:p1' });
});

test('interrupts and sends function keys from the terminal pad', async ({ page }) => {
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'Pad app', agent: 'codex' });
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'a shell at rest',
  });

  const keysSent = async (keys: string[]) => (await commands(page)).filter((command) =>
    command.type === 'send_keys' && JSON.stringify(command.keys) === JSON.stringify(keys)).length;

  // The pad carries no dedicated Ctrl+C button: interrupting is the armed Ctrl
  // chord, which keeps the whole pad on one row.
  await expect(page.getByRole('button', { name: 'Ctrl+C' })).toHaveCount(0);
  await page.getByRole('button', { name: 'Ctrl', exact: true }).click();
  await page.getByRole('textbox', { name: 'Modifier shortcut character' }).press('c');
  await expect.poll(() => keysSent(['ctrl+c'])).toBe(1);

  // Ctrl stays armed for repeated chords, so the pad must be disarmed before an
  // unmodified key: otherwise F5 would leave as ctrl+f5.
  const ctrlKey = page.getByRole('button', { name: 'Ctrl', exact: true });
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'true');
  await ctrlKey.click();
  await expect(ctrlKey).toHaveAttribute('aria-pressed', 'false');

  await page.getByRole('button', { name: 'Function keys' }).first().click();
  await page.getByRole('button', { name: 'F5', exact: true }).click();
  await expect.poll(() => keysSent(['f5'])).toBe(1);
});
