import { expect, test, type Locator, type Page } from '@playwright/test';
import { scrollConversationToBottom, showOlderConversationEntry } from '../support/journey-conversation';
import { captureClipboard, copiedText, distanceFromBottom, saveDeviceCredential } from '../support/journey-device';
import { boot, closeSocket, commands, composerTarget, connectFedora, fedora, handshake, lastCommand, reply, server, setAutoCommands, setConversationFixture, socketCount, type ConversationFixture } from '../support/journey-relay';
import editFixture from '../../../contracts/fixtures/conversation_edit_diff.json' with { type: 'json' };

test('reads and replies from native conversation history', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_default_agent_view', 'terminal');
    localStorage.setItem('herdr-conversation-view', 'conversation');
  });
  await captureClipboard(page);
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
  await expect.poll(() => copiedText(page))
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

test('a reader reviews edit previews by keyboard only on the advertising relay', async ({ page }) => {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_default_agent_view', 'conversation');
    localStorage.setItem('herdr-conversation-view', 'activity');
  });
  const older = { id: 'older', label: 'Older relay', url: 'ws://127.0.0.1:8872', token: '' };
  await boot(page, [fedora, older]);
  await expect.poll(() => socketCount(page)).toBe(2);
  await saveDeviceCredential(page, { id: 'credential-reader', deviceId: 'device-reader', secret: 'R'.repeat(43), role: 'reader' });
  await page.evaluate(() => {
    const auth = JSON.parse(localStorage.getItem('herdr_device_auth_v1')!);
    auth.relays.older = { ...auth.relays.fedora, id: 'older-reader', deviceId: 'older-device' };
    localStorage.setItem('herdr_device_auth_v1', JSON.stringify(auth));
  });
  await handshake(page, 0, { capabilities: ['conversation_history', 'conversation_activity', 'conversation_edit_diffs'] });
  await handshake(page, 1, { capabilities: ['conversation_history'] });
  const agent = { status: 'working', agent: 'codex', conversation_history_available: true };
  await server(page, 0, { type: 'agents', agents: [{ ...agent, project: 'Edit history', pane_id: 'w1:p1', agent_session_id: 'edit-session' }] });
  await server(page, 1, { type: 'agents', agents: [{ ...agent, project: 'Raw history', pane_id: 'w1:p2', agent_session_id: 'raw-session' }] });
  const edit = { id: 'edit', timestamp: '', role: 'assistant', tools: [{ ...editFixture, state: 'running' }] };
  const written = { id: 'write', timestamp: '', role: 'user', text: 'Record this write', tools: [{ name: 'write', input: '{"path":"src/log.txt","content":"first"}', diff: { kind: 'write', path: 'src/log.txt', added: 300, removed: 0, text: '+first', truncated: true } }] };
  const read = { id: 'read', timestamp: '', role: 'assistant', tools: [{ name: 'Read', output: Array.from({ length: 80 }, (_, index) => `ordinary output ${index}`).join('\n') }] };
  const question = { id: 'question', timestamp: '', role: 'assistant', tools: [{ name: 'AskUserQuestion', input: JSON.stringify({ questions: [{ question: 'Use this change?', options: [{ label: 'Yes' }, { label: 'No' }] }] }), output: 'The user answered: "Use this change?"="Yes". Read the answers carefully.' }] };
  const capabilities = { source: 'recorded_history', active_branch: false };
  await setConversationFixture(page, { entries: [edit, written, read, question], total: 4, sourceRevision: 'edit:1', capabilities, sequence: 1 });
  await page.getByRole('button', { name: 'Open Edit history on Fedora' }).click();
  const card = page.locator('details').filter({ has: page.getByText('apply_patch', { exact: true }) });
  const summary = card.locator('summary');
  await expect(summary).toHaveAccessibleName(/apply_patch main\.go 1 line added, 2 lines removed running/);
  await expect(card.locator('.workspace-diff-line')).toHaveCount(0);
  await summary.focus();
  await summary.press('Enter');
  await expect(card.getByText('src/main.go', { exact: true })).toBeVisible();
  await expect(card.locator('.diff-hunk')).toHaveText('@@');
  await expect(card.locator('.diff-deletion')).toHaveText(['-old', '-gone']);
  await expect(card.getByText('Input', { exact: true })).toHaveCount(0);
  await expect(page.getByRole('combobox', { name: 'Prompt', exact: true })).toBeDisabled();

  const update = await lastCommand(page, 'get_conversation_updates', 0);
  await reply(page, update, { subscription_id: 'fixture-subscription', kind: 'batch', source_revision: 'edit:1', sequence: 2,
    entries: [{ ...edit, tools: [{ ...editFixture, state: 'failed', error: true, output: 'Could not save this edit' }] }], remove: [], state: 'recorded', capabilities });
  await expect(card.getByText('Could not save this edit', { exact: true })).toBeVisible();
  await expect(card.locator('.diff-addition')).toHaveText('+new');

  const search = page.getByRole('searchbox', { name: 'Search loaded conversation' });
  await search.fill('context hint');
  await expect(card).toBeVisible();
  await expect(page.getByText('Use this change?', { exact: true })).toBeHidden();
  await search.clear();
  const writeCard = page.locator('[data-conversation-entry-id="write"] details');
  await writeCard.locator('summary').click();
  await expect(writeCard.getByLabel('Written content')).toBeVisible();
  await expect(page.getByText('Diff truncated', { exact: true })).toBeVisible();
  const rawRead = page.locator('details').filter({ has: page.getByText('Read', { exact: true }) });
  await rawRead.locator('summary').click();
  await rawRead.getByRole('button', { name: 'Show all 80 lines' }).click();
  await expect(rawRead.locator('pre')).toContainText('ordinary output 79');

  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await expect(card).toBeHidden();
  await expect(page.getByText('Use this change?', { exact: true })).toBeVisible();
  await expect(writeCard.locator('summary')).toHaveAccessibleName('write called');
  await expect(writeCard.getByText('Input', { exact: true })).toBeVisible();
  await expect(writeCard.locator('pre')).toContainText('path: src/log.txt');
  await expect(writeCard.locator('.workspace-diff-line')).toHaveCount(0);
  await page.getByRole('button', { name: 'Full history', exact: true }).click();
  await expect(card.getByText('Could not save this edit', { exact: true })).toBeVisible();
  await expect(writeCard.getByLabel('Written content')).toBeVisible();
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await page.getByRole('button', { name: 'Open Raw history on Older relay' }).click();
  await expect(summary).toHaveAccessibleName('apply_patch running');
  await summary.click();
  await expect(card.getByText('Input', { exact: true })).toBeVisible();
  await expect(card.locator('pre')).toContainText('context hint');
  await expect(card.locator('.workspace-diff-line')).toHaveCount(0);
  const readCommands = new Set(['refresh_agents', 'get_conversation_history', 'get_conversation_updates', 'unwatch_conversation', 'read_pane', 'push_viewed_pane']);
  expect((await commands(page)).filter(command => !readCommands.has(String(command.type)))).toEqual([]);
});

test('keeps code and timestamps readable at phone and desktop widths', async ({ page }) => {
  const externalRequests: string[] = [];
  page.on('request', request => {
    const url = new URL(request.url());
    if ((url.protocol === 'http:' || url.protocol === 'https:') && url.hostname !== '127.0.0.1') externalRequests.push(url.origin);
  });
  await page.setViewportSize({ width: 320, height: 740 });
  await page.emulateMedia({ reducedMotion: 'reduce' });
  const started = '2026-08-10T12:01:00Z';
  const entry = (id: string, text: string, tools?: Record<string, unknown>[], role = 'assistant') => ({ id, text, role, timestamp: started, tools });
  const diff = (path: string) => ({
    kind: 'edit', path, text: '-  const before = "past"; // old\n+  const after = "present"; // new\n+++ const line = 21;', added: 2, removed: 1, truncated: false,
  });
  const source = 'const data = "x"; const longValue = "' + 'x'.repeat(400) + '";';
  await connectFedora(page, { capabilities: ['conversation_history', 'conversation_activity', 'conversation_edit_diffs', 'workspace_inspection'] });
  await setConversationFixture(page, { total: 3, entries: [
    entry('user', 'Update the sample file', [], 'user'),
    entry('edit', '', [{ name: 'Edit', state: 'completed', diff: diff('src/main.ts') }]),
    entry('prose', `Here is the example.\n\n\`\`\`ts\n${source}\n\`\`\``),
  ] });
  await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'working', project: 'Reading', agent: 'pi', cwd: '/work/mobile', conversation_history_available: true }] });
  await page.getByRole('button', { name: 'Open Reading on Fedora' }).click();
  const list = page.locator('.conversation-list');
  const prose = list.locator('[data-conversation-entry-id="prose"]');
  const code = prose.locator('pre code');
  const replyPresentation = async (width: number) => {
    await expect(prose).toHaveCSS('background-color', 'rgba(0, 0, 0, 0)');
    await expect(prose).toHaveCSS('border-top-width', '0px');
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
  };
  await expect(code.locator('.syntax-keyword')).toHaveCount(2);
  await replyPresentation(320);
  await page.getByRole('button', { name: 'Full history' }).click();
  await replyPresentation(320);
  const readingContrast = () => page.evaluate(() => {
    const themes = ['dark', 'light', 'nord', 'solarized', 'rose', 'latte'];
    const root = document.documentElement;
    const context = document.createElement('canvas').getContext('2d')!;
    const luminance = (color: string) => {
      context.fillStyle = color;
      context.fillRect(0, 0, 1, 1);
      const [r, g, b] = Array.from(context.getImageData(0, 0, 1, 1).data).map(channel => {
        const component = channel / 255;
        return component <= .04045 ? component / 12.92 : ((component + .055) / 1.055) ** 2.4;
      });
      return .2126 * r + .7152 * g + .0722 * b;
    };
    const contrast = (text: Element, background: Element) => {
      const a = luminance(getComputedStyle(text).color);
      const b = luminance(getComputedStyle(background).backgroundColor);
      return (Math.max(a, b) + .05) / (Math.min(a, b) + .05);
    };
    return themes.map(theme => {
      root.dataset.theme = theme;
      const ratios = [...document.querySelectorAll('pre [class^="syntax-"]')].map(token =>
        contrast(token, token.closest('.diff-deletion, .diff-addition, pre')!));
      for (const row of document.querySelectorAll('pre .diff-addition, pre .diff-deletion')) ratios.push(contrast(row, row));
      const userTime = document.querySelector('[data-conversation-entry-id="user"] time')!;
      const agentTime = document.querySelector('[data-conversation-entry-id="prose"] time')!;
      return [theme, Math.min(...ratios, contrast(userTime, userTime.closest('article')!), contrast(agentTime, document.body))] as const;
    });
  });
  const card = list.locator('[data-conversation-entry-id="edit"] details');
  const summary = card.locator('summary');
  await expect(summary).toHaveAccessibleName('Edit main.ts 2 lines added, 1 line removed completed');
  await summary.focus();
  await summary.press('Enter');
  const preview = card.getByLabel('Requested change');
  await expect(preview.locator('.diff-addition')).toHaveText(['+  const after = "present"; // new', '+++ const line = 21;']);
  await expect(preview.locator('.diff-deletion .syntax-string')).toHaveText('"past"');
  await expect(preview.locator('.diff-addition .syntax-number')).toHaveText('21');

  await page.setViewportSize({ width: 940, height: 740 });
  await replyPresentation(940);
  const ratios = await readingContrast();
  for (const [theme, ratio] of ratios) expect(ratio, theme).toBeGreaterThanOrEqual(4.5);
  await page.getByRole('button', { name: 'Conversation', exact: true }).click();
  await expect(code.locator('.syntax-keyword')).toHaveCount(2);
  await replyPresentation(940);
  expect((await commands(page)).filter(command => command.type === 'workspace_file' || command.type === 'workspace_git_diff')).toEqual([]);
  await page.getByRole('button', { name: 'Terminal view' }).click();
  const rail = page.getByRole('complementary', { name: 'Agent navigation' });
  await expect(rail).toBeVisible();
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Inspect workspace' }).click();
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  await reply(page, await lastCommand(page, 'workspace_tree'), { root: '/work/mobile', entries: [] });
  await reply(page, await lastCommand(page, 'workspace_git_status'), { available: true, files: [{ path: 'src/main.ts', status: ' M' }] });
  await dialog.getByRole('tab', { name: /^Changes/ }).click();
  await dialog.getByRole('button', { name: /src\/main.ts Modified/ }).click();
  await reply(page, await lastCommand(page, 'workspace_git_diff'), {
    path: 'src/main.ts',
    diff: 'diff --git a/src/main.ts b/src/main.ts\n--- a/src/main.ts\n+++ b/src/main.ts\n@@ -1,2 +1,3 @@\n const context = 1;\n--- const before = "past";\n+  const after = "present";\n+++ const line = 21;\n\\ No newline at end of file',
  });
  const git = dialog.getByLabel('Diff for src/main.ts');
  await expect(git.locator('.diff-file')).toHaveText(['--- a/src/main.ts', '+++ b/src/main.ts']);
  await expect(git.locator('.diff-file [class^="syntax-"], .diff-hunk [class^="syntax-"], .diff-note [class^="syntax-"]')).toHaveCount(0);
  await expect(git.locator('.diff-deletion')).toHaveText('--- const before = "past";');
  await expect(git.locator('.diff-deletion .syntax-string')).toHaveText('"past"');
  await expect(git.locator('.diff-context .syntax-keyword')).toHaveText('const');
  const zoom = dialog.getByRole('button', { name: 'Zoom in diff' });
  await zoom.click();
  const zoomed = await git.evaluate(node => getComputedStyle(node).fontSize);
  await expect(git.locator('.diff-deletion .syntax-string')).toHaveCSS('font-size', zoomed);
  await expect(dialog.getByRole('button', { name: 'Reset diff zoom' })).toHaveText('115%');
  await dialog.getByRole('button', { name: 'Close workspace inspector' }).click();
  await expect(rail).toBeVisible();
  expect(externalRequests).toEqual([]);
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
    const longPath = `src/${'a'.repeat(83)}.ts`;
    await setConversationFixture(page, {
      entries: [{ id: 'workspace:answer', timestamp: '', role: 'assistant', text: `Updated \`src/main.ts:42\` and ${longPath}. See https://example.com/docs/guide.html` }],
      total: 1,
    });
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
    const path = page.getByRole('button', { name: 'src/main.ts:42', exact: true });
    await expect(page.getByText('src/main.ts:42', { exact: true })).toBeVisible();
    await expect(page.getByRole('link', { name: 'https://example.com/docs/guide.html' })).toHaveAttribute('target', '_blank');
    expect((await commands(page)).filter(({ type }) => String(type).startsWith('workspace_'))).toEqual([]);
    if (availability !== 'available') {
      await expect(path).toHaveCount(0);
      await expect(inspect).toBeDisabled();
      await expect(inspect).toHaveAttribute('title', 'Workspace inspection is unavailable');
      return;
    }
    await expect(inspect).toBeEnabled();
    await expect(inspect).toHaveAttribute('title', 'Inspect workspace files and Git changes');
    await expect(path).toBeVisible();
    await page.setViewportSize({ width: 320, height: 740 });
    const wrapped = page.getByRole('button', { name: longPath, exact: true });
    await expect.poll(() => wrapped.evaluate((element) => element.getClientRects().length)).toBeGreaterThan(1);
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(await page.evaluate(() => innerWidth));
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

async function openFileLinkConversation(page: Page, conversation: string | ConversationFixture) {
  await connectFedora(page, { capabilities: ['conversation_history', 'workspace_inspection'] });
  await setConversationFixture(page, typeof conversation === 'string'
    ? { entries: [{ id: 'path-answer', role: 'assistant', text: conversation, timestamp: '' }], total: 1 }
    : conversation);
  await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'idle', project: 'File links', agent: 'codex', cwd: '/work/mobile', conversation_history_available: true }] });
  await page.getByRole('button', { name: 'Open File links on Fedora' }).click();
  await expect(page.getByRole('heading', { name: 'Conversation', exact: true })).toBeVisible();
}

const scannerRoute = /\/assets\/syntax-colours-\d+\.js(?:\?|$)/u;

test('colours a saved list fence without replacing selected prose or a focused file link', async ({ page }) => {
  const code = '  const preview = "<b>src/ignored.ts</b>";';
  let resume = () => {};
  const blocked = new Promise<void>(resolve => { resume = resolve; });
  await page.route(scannerRoute, async route => { await blocked; await route.continue(); });
  const requested = page.waitForRequest(scannerRoute);
  const markdown = `Read src/main.ts:2 before changing it.\n\n- Example:\n  \`\`\`TS\n  ${code}\n  \`\`\`\n\n\`\`\`js\nconst unfinished = 1;`;
  await captureClipboard(page);
  await openFileLinkConversation(page, markdown);
  await requested;
  const message = page.locator('.conversation-markdown');
  const fence = message.locator('li pre code');
  const link = page.getByRole('button', { name: 'src/main.ts:2', exact: true });
  const sameLink = await link.elementHandle();
  await page.getByRole('searchbox', { name: 'Search loaded conversation' }).focus();
  await page.keyboard.press('Tab');
  // Message controls precede the body in the reading order.
  for (let index = 0; index < 5 && !await link.evaluate(element => element === document.activeElement); index++) {
    await page.keyboard.press('Tab');
  }
  await expect(link).toBeFocused();
  const select = await fence.evaluate(element => {
    const selection = window.getSelection()!;
    const range = document.createRange();
    range.selectNodeContents(element);
    selection.removeAllRanges();
    selection.addRange(range);
    return selection.toString();
  });
  expect(select).toBe(code);
  const response = page.waitForResponse(scannerRoute);
  resume();
  await response;
  await expect(fence.locator('.syntax-keyword')).toHaveCount(0);
  expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(code);
  await page.evaluate(() => window.getSelection()?.removeAllRanges());
  await expect(fence.locator('.syntax-keyword')).toHaveText('const');
  await expect(fence.locator('.syntax-string')).toHaveText('"<b>src/ignored.ts</b>"');
  expect(await fence.textContent()).toBe(code);
  await expect(fence.locator('a, b')).toHaveCount(0);
  await expect(message.locator('pre').last().locator('[class^="syntax-"]')).toHaveCount(0);
  expect(await sameLink?.evaluate(element => element === document.activeElement)).toBe(true);
  expect((await commands(page)).filter(command => command.type === 'workspace_file')).toEqual([]);
  await page.keyboard.press('Enter');
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  await expect(dialog.locator('.preview-heading')).toContainText('Line 2');
  await dialog.getByRole('button', { name: 'Close workspace inspector' }).click();
  const search = page.getByRole('searchbox', { name: 'Search loaded conversation' });
  await search.fill('const preview');
  await expect(fence.locator('mark')).toHaveText('const preview');
  await expect(fence.locator('.syntax-keyword')).toHaveCount(0);
  await search.clear();
  await expect(fence.locator('.syntax-keyword')).toHaveText('const');
  await page.getByRole('button', { name: 'Copy File links message as Markdown' }).click();
  expect(await copiedText(page)).toBe(markdown);
  await page.getByRole('button', { name: 'Copy code from File links message' }).click();
  expect(await copiedText(page)).toBe('  ' + code);
});

test('defers colouring Git diff rows while the reader has selected their signed text', async ({ page }) => {
  let resume = () => {};
  const blocked = new Promise<void>(resolve => { resume = resolve; });
  await page.route(scannerRoute, async route => { await blocked; await route.continue(); });
  const requested = page.waitForRequest(scannerRoute);
  await openFileLinkConversation(page, 'Check the workspace change.');
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Inspect workspace' }).click();
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  await reply(page, await lastCommand(page, 'workspace_tree'), { root: '/work/mobile', entries: [] });
  await reply(page, await lastCommand(page, 'workspace_git_status'), { available: true, files: [{ path: 'src/main.ts', status: ' M' }] });
  await dialog.getByRole('tab', { name: /^Changes/ }).click();
  await dialog.getByRole('button', { name: 'src/main.ts Modified' }).click();
  const added = '+const label = "<example>";';
  await reply(page, await lastCommand(page, 'workspace_git_diff'), { path: 'src/main.ts', diff: `@@ -1,2 +1,2 @@\n\n-const label = "old";\n${added}` });
  const row = dialog.locator('.diff-addition');
  await requested;
  await row.evaluate(node => {
    const range = document.createRange();
    range.selectNodeContents(node);
    window.getSelection()?.removeAllRanges();
    window.getSelection()?.addRange(range);
  });
  const response = page.waitForResponse(scannerRoute);
  resume();
  await response;
  await expect(row.locator('.syntax-keyword')).toHaveCount(0);
  expect(await page.evaluate(() => window.getSelection()?.toString())).toBe(added);
  await page.evaluate(() => window.getSelection()?.removeAllRanges());
  await expect(row.locator('.syntax-keyword')).toHaveText('const');
  expect(await row.textContent()).toBe(added);
  await expect(dialog.locator('.diff-deletion .syntax-keyword')).toHaveText('const');
});

test('keeps the selected linked line on a missing scanner without another workspace read', async ({ page }) => {
  await page.route(scannerRoute, route => route.fulfill({ status: 404, body: 'missing' }));
  await openFileLinkConversation(page, 'See src/long.ts:42.');
  const response = page.waitForResponse(scannerRoute);
  await page.getByRole('button', { name: 'src/long.ts:42', exact: true }).tap();
  expect((await response).status()).toBe(404);
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  const textarea = dialog.getByRole('textbox', { name: 'Contents of src/long.ts' });
  await expect(textarea).toBeVisible();
  await expect(dialog.locator('.workspace-code-mirror')).toHaveCount(0);
  expect(await textarea.evaluate((node: HTMLTextAreaElement) => node.value.slice(node.selectionStart, node.selectionEnd))).toBe('// Line 42');
  expect((await commands(page)).filter(command => command.type === 'workspace_file')).toHaveLength(1);
  await expect(dialog.locator('.preview-heading')).toContainText('Line 42');
});

test('mirrors an eligible file across both scroll axes and leaves oversized files plain', async ({ page, browserName }) => {
  await openFileLinkConversation(page, 'See src/main.ts:42 and src/big.py');
  await setAutoCommands(page, false);
  const text = Array.from({ length: 60 }, (_, index) => `\tconst value${index + 1} = "${'x'.repeat(150)}";`).join('\n');
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  await page.getByRole('button', { name: 'src/main.ts:42', exact: true }).tap();
  await reply(page, await lastCommand(page, 'workspace_file'), { path: 'src/main.ts', kind: 'text', text, size: text.length });
  const box = dialog.getByRole('textbox', { name: 'Contents of src/main.ts' });
  const mirror = dialog.locator('.workspace-code-mirror');
  await expect(mirror).toHaveAttribute('aria-hidden', 'true');
  await expect(box).toHaveValue(text);
  expect(await mirror.textContent()).toBe(text);
  await expect(mirror.locator('.syntax-keyword')).toHaveCount(60);
  const focusPreview = async (preview: Locator) => {
    await dialog.getByRole('button', { name: 'Show file list' }).focus();
    for (let i = 0; i < 5 && !await preview.evaluate(node => node === document.activeElement); i++) {
      await page.keyboard.press('Tab');
    }
    await expect(preview).toBeFocused();
    await expect(preview).toHaveCSS('outline-width', '2px');
    await expect(preview).toHaveCSS('outline-offset', '-2px');
  };
  const selected = await box.evaluate((node: HTMLTextAreaElement) => node.value.slice(node.selectionStart, node.selectionEnd));
  expect(selected).toBe(text.split('\n')[41]);
  const left = await box.evaluate(node => { node.scrollLeft = 100; node.scrollTop = 200; return [node.scrollLeft, node.scrollTop]; });
  expect(left).toEqual([100, 200]);
  await expect.poll(() => mirror.evaluate(node => getComputedStyle(node).transform)).toBe('matrix(1, 0, 0, 1, -100, -200)');
  await focusPreview(box);
  if (browserName === 'chromium') {
    await page.emulateMedia({ forcedColors: 'active' });
    await expect(mirror).toBeHidden();
    expect(await box.evaluate(node => getComputedStyle(node).color)).not.toBe('rgba(0, 0, 0, 0)');
    await page.emulateMedia({ forcedColors: 'none' });
  }
  await dialog.getByRole('button', { name: 'Close workspace inspector' }).click();
  const big = 'return ' + 'x'.repeat(17 * 1024);
  await page.getByRole('button', { name: 'src/big.py', exact: true }).tap();
  await reply(page, await lastCommand(page, 'workspace_file'), { path: 'src/big.py', kind: 'text', text: big, size: big.length });
  const plain = dialog.getByRole('textbox', { name: 'Contents of src/big.py' });
  await expect(plain).toHaveValue(big);
  await expect(mirror).toHaveCount(0);
  await focusPreview(plain);
});

test('opens a Conversation file link by touch and keyboard, without changing the toolbar inspector', async ({ page }) => {
  await openFileLinkConversation(page, 'Updated src/main.ts');
  const link = page.getByRole('button', { name: 'src/main.ts', exact: true });
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  const preview = dialog.getByRole('main', { name: 'Workspace preview' });
  const close = dialog.getByRole('button', { name: 'Close workspace inspector' });
  await link.tap();
  await expect(preview.locator('.preview-heading')).toContainText('src/main.ts');
  await expect(dialog.getByRole('complementary', { name: 'Workspace files' })).toHaveCount(0);
  const reads = (await commands(page)).filter(({ type }) => String(type).startsWith('workspace_'));
  expect(reads.map(({ type }) => type).sort()).toEqual(['workspace_file', 'workspace_git_status', 'workspace_tree']);
  expect(reads.find(({ type }) => type === 'workspace_file')).toMatchObject({ pane_id: 'w1:p1', path: 'src/main.ts' });
  await close.click();
  for (const key of ['Enter', 'Space']) {
    await link.focus();
    await page.keyboard.press(key);
    await expect(preview.getByRole('textbox', { name: 'Contents of src/main.ts' })).toBeVisible();
    await close.click();
  }
  await page.getByRole('button', { name: 'Inspect workspace' }).click();
  await expect(dialog.getByRole('complementary', { name: 'Workspace files' })).toBeVisible();
  await expect(preview).toContainText('Choose a file to preview it.');
});

test('closes a linked file preview without issuing new reads when the working directory changes', async ({ page }) => {
  const path = '/work/mobile/src/main.ts';
  let resume = () => {};
  const blocked = new Promise<void>(resolve => { resume = resolve; });
  await page.route(scannerRoute, async route => { await blocked; await route.continue(); });
  const requested = page.waitForRequest(scannerRoute);
  await openFileLinkConversation(page, `Updated ${path}`);
  const link = page.getByRole('button', { name: path, exact: true });
  await link.tap();
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  await expect(dialog.getByLabel('Contents of src/main.ts')).toBeVisible();
  const workspaceReads = async () => (await commands(page)).filter(({ type }) => String(type).startsWith('workspace_'));
  const reads = await workspaceReads();
  expect(reads.find(({ type }) => type === 'workspace_file')).toMatchObject({ pane_id: 'w1:p1', path: 'src/main.ts' });
  await requested;

  await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'idle', project: 'File links', agent: 'codex', cwd: '/work/other', conversation_history_available: true }] });
  await expect(dialog).toBeHidden();
  const response = page.waitForResponse(scannerRoute);
  resume();
  await response;
  await expect(page.getByText(`Updated ${path}`, { exact: true })).toBeVisible();
  await expect(link).toHaveCount(0);
  expect(await workspaceReads()).toEqual(reads);

  await page.getByRole('button', { name: 'Inspect workspace' }).click();
  await expect(dialog.getByText('/work/other', { exact: true })).toBeVisible();
  await expect(dialog.getByRole('complementary', { name: 'Workspace files' })).toBeVisible();
  await expect(dialog.getByText('Choose a file to preview it.')).toBeVisible();
  await expect(dialog.locator('.workspace-code-mirror')).toHaveCount(0);
});

test('scrolls a file link preview to its line, clamps near the end, and ignores unavailable lines', async ({ page }) => {
  await openFileLinkConversation(page, 'See src/long.ts:42 src/long.ts:295 src/long.ts:999 and docs/home.png:3');
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  const heading = dialog.locator('.preview-heading');
  const textarea = dialog.getByRole('textbox', { name: 'Contents of src/long.ts' });
  const close = dialog.getByRole('button', { name: 'Close workspace inspector' });
  await page.getByRole('button', { name: 'src/long.ts:42', exact: true }).tap();
  await expect(heading).toContainText('Line 42');
  await expect(textarea).toHaveAttribute('readonly', '');
  const position = await textarea.evaluate((element: HTMLTextAreaElement) => {
    const { paddingTop, lineHeight } = getComputedStyle(element);
    return {
      scrollTop: element.scrollTop,
      expected: parseFloat(paddingTop) + 41 * parseFloat(lineHeight),
      selection: element.value.slice(element.selectionStart, element.selectionEnd),
    };
  });
  expect(Math.abs(position.scrollTop - position.expected)).toBeLessThanOrEqual(1);
  expect(position.selection).toBe('// Line 42');
  await close.click();

  await page.getByRole('button', { name: 'src/long.ts:295', exact: true }).tap();
  await expect(heading).toContainText('Line 295');
  await expect.poll(() => distanceFromBottom(textarea)).toBeLessThanOrEqual(1);
  await close.click();

  await page.getByRole('button', { name: 'src/long.ts:999', exact: true }).tap();
  await expect(heading).toContainText('src/long.ts');
  await expect(heading).not.toContainText('Line');
  expect(await textarea.evaluate((element) => element.scrollTop)).toBe(0);
  await close.click();

  await page.getByRole('button', { name: 'docs/home.png:3', exact: true }).tap();
  const image = dialog.getByRole('img', { name: 'Preview of docs/home.png' });
  await expect(image).toBeVisible();
  await expect.poll(() => image.evaluate((element: HTMLImageElement) => element.naturalWidth)).toBe(1);
  await expect(dialog).not.toContainText('Line 3');
});

test('previews an empty workspace file but rejects non-string text', async ({ page }) => {
  await openFileLinkConversation(page, 'See src/main.ts and src/other.ts');
  await setAutoCommands(page, false);
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  await page.getByRole('button', { name: 'src/main.ts', exact: true }).tap();
  await reply(page, await lastCommand(page, 'workspace_file'), { path: 'src/main.ts', kind: 'text', text: 42, size: 0 });
  await expect(dialog.getByRole('alert')).toHaveText('Could not open src/main.ts: Relay returned an invalid workspace preview.');
  await expect(dialog.locator('textarea, .workspace-code-mirror')).toHaveCount(0);
  await dialog.getByRole('button', { name: 'Close workspace inspector' }).click();
  await page.getByRole('button', { name: 'src/other.ts', exact: true }).tap();
  await reply(page, await lastCommand(page, 'workspace_file'), { path: 'src/other.ts', media_type: 'text/plain', kind: 'text', size: 0 });
  const preview = dialog.getByRole('textbox', { name: 'Contents of src/other.ts' });
  await expect(preview).toBeVisible();
  await expect(preview).toHaveValue('');
  await expect(dialog.getByRole('alert')).toHaveCount(0);
});

test('names a missing file and lets the reader choose another file from the list', async ({ page }) => {
  await openFileLinkConversation(page, 'Removed missing/gone.ts');
  await page.getByRole('button', { name: 'missing/gone.ts', exact: true }).tap();
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  await expect(dialog.getByRole('alert')).toHaveText('Could not open missing/gone.ts: Workspace file was not found');
  await expect(dialog.locator('textarea')).toHaveCount(0);
  await dialog.getByRole('button', { name: 'Show file list' }).click();
  await dialog.getByRole('complementary', { name: 'Workspace files' }).getByRole('button', { name: 'README.md', exact: true }).click();
  await expect(dialog.getByLabel('Contents of README.md')).toHaveValue('# Workspace preview\n\nRead-only file contents.');
});

test('names the file when the relay is disconnected and the tree also fails', async ({ page }) => {
  await openFileLinkConversation(page, 'Updated src/main.ts');
  const link = page.getByRole('button', { name: 'src/main.ts', exact: true });
  await expect(link).toBeVisible();
  await closeSocket(page, 0);
  await link.tap();
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  await expect(dialog.getByRole('alert')).toHaveText('Could not open src/main.ts: Relay is not connected');
  await expect(dialog.locator('textarea')).toHaveCount(0);
  await dialog.getByRole('button', { name: 'Show file list' }).click();
  await expect(dialog.getByRole('complementary').getByRole('alert')).toHaveText('Relay is not connected');
  await expect(dialog.getByRole('main', { name: 'Workspace preview' }).getByRole('alert')).toHaveText('Could not open src/main.ts: Relay is not connected');
  await dialog.getByRole('button', { name: 'Close workspace inspector' }).click();
  await expect(link).toBeVisible();
});

test('keeps the linked file visible when a later tree request fails', async ({ page }) => {
  await openFileLinkConversation(page, 'Updated src/main.ts');
  const link = page.getByRole('button', { name: 'src/main.ts', exact: true });
  await expect(link).toBeVisible();
  await setAutoCommands(page, false);
  await link.tap();
  const file = await lastCommand(page, 'workspace_file');
  const tree = await lastCommand(page, 'workspace_tree');
  const git = await lastCommand(page, 'workspace_git_status');
  await reply(page, file, { path: 'src/main.ts', kind: 'text', text: 'File ready', size: 10 });
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  const preview = dialog.getByLabel('Contents of src/main.ts');
  await expect(preview).toHaveValue('File ready');
  await reply(page, tree, undefined, { ok: false, phase: 'failed', error: 'Could not read tree' });
  await reply(page, git, { available: false, files: [] });
  await dialog.getByRole('button', { name: 'Show file list' }).click();
  await expect(dialog.getByRole('alert')).toHaveText('Could not read tree');
  await expect(preview).toHaveValue('File ready');
});

test('returns from a file link to the same Conversation scroll, older turns and draft', async ({ page }) => {
  const entries = Array.from({ length: 24 }, (_, index) => ({
    id: `file-turn-${index}`, role: index % 2 ? 'assistant' : 'user', timestamp: '',
    text: index === 5 ? 'Updated src/main.ts' : `Conversation turn ${index}`,
  }));
  await openFileLinkConversation(page, {
    entries: entries.slice(12), total: entries.length, hasMore: true, nextCursor: 'older-files',
    pages: { 'older-files': { entries: entries.slice(0, 12) } },
  });
  await page.getByRole('button', { name: 'Load older turns' }).click();
  const list = page.locator('.conversation-list');
  await expect(list.locator('article')).toHaveCount(entries.length);
  const link = list.getByRole('button', { name: 'src/main.ts', exact: true });
  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await composer.fill('A draft to keep');
  await link.evaluate((element) => element.scrollIntoView({ block: 'center' }));
  await expect(link).toBeInViewport({ ratio: 1 });
  expect(await distanceFromBottom(list)).toBeGreaterThan(200);
  const scrollTop = await list.evaluate((element) => element.scrollTop);
  expect(scrollTop).toBeGreaterThan(0);
  const historyLength = await page.evaluate(() => history.length);

  await link.tap();
  const dialog = page.getByRole('dialog', { name: 'Workspace', exact: true });
  await expect(dialog.getByLabel('Contents of src/main.ts')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
  await expect(list.locator('article')).toHaveCount(entries.length);
  await expect(composer).toHaveValue('A draft to keep');
  expect(await list.evaluate((element) => element.scrollTop)).toBe(scrollTop);
  expect(await page.evaluate(() => history.length)).toBe(historyLength);
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Open File links on Fedora' })).toBeVisible();
});

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
