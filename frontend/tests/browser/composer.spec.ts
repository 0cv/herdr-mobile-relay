import { expect, test, type Page } from '@playwright/test';
import { APP_METADATA, emulateStandalone } from '../support/journey-device';
import { boot, closeSocket, commands, composerTarget, connectFedora, fedora, handshake, openAgent, openSharedComposer, replyToPaneRead, server, setAutoCommands, setConversationFixture, socketCount } from '../support/journey-relay';

test('discovers slash commands per terminal and fills them before sending', async ({ page }) => {
  await connectFedora(page);
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
  await openAgent(page, { pane_id: 'w1:p1', status: 'working', project: 'Large catalog app', agent: 'codex', cwd: '/home/test/large' });
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
  await openAgent(page, { pane_id: 'w1:p1', status: 'idle', project: 'Composer app', agent: 'codex' });

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
  await connectFedora(page, { capabilities: ['conversation_history', 'pi_integration', 'manual_terminal_input'] });
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
