import { expect, test, type Page } from '@playwright/test';
import { scrollConversationToBottom, showOlderConversationEntry } from '../support/journey-conversation';
import { distanceFromBottom, saveDeviceCredential } from '../support/journey-device';
import { boot, closeSocket, commands, composerTarget, connectFedora, fedora, handshake, server, setConversationFixture, socketCount } from '../support/journey-relay';

interface ConversationBatch {
  after: number;
  entries: Record<string, unknown>[];
  sourceRevision: string;
  capabilities: Record<string, unknown>;
  remove?: string[];
  state?: string;
}

async function deliverConversationBatch(page: Page, { after, entries, sourceRevision, capabilities, remove = [], state = 'generating' }: ConversationBatch) {
  const updates = async () => (await commands(page)).filter(command => command.type === 'get_conversation_updates' && command.after === after);
  await expect.poll(async () => (await updates()).length).toBeGreaterThan(0);
  const request = (await updates()).at(-1)!;
  await server(page, 0, { type: 'command_result', action: 'get_conversation_updates', request_id: request.request_id, ok: true, phase: 'completed', data: { subscription_id: 'fixture-subscription', kind: 'batch', source_revision: sourceRevision, sequence: after + 1, entries, remove, state, capabilities } });
}

async function openFollowingConversation(page: Page, mode: 'conversation' | 'activity' = 'activity') {
  await page.addInitScript(value => localStorage.setItem('herdr-conversation-view', value), mode);
  await connectFedora(page, { capabilities: ['conversation_history', 'conversation_activity'] });
  const capabilities = { source: 'pi_events', active_branch: true };
  const entry = (id: string, text: string, role = 'assistant') => ({ id, text, role, timestamp: '' });
  const entries = Array.from({ length: 24 }, (_, index) => entry(`follow:${index}`, `Follow seed ${index}\n\n${'wrapped history '.repeat(index === 0 ? 500 : 12)}`, index % 2 ? 'assistant' : 'user'));
  await setConversationFixture(page, { entries, total: 24, sourceRevision: 'stream:1', sequence: 0, capabilities });
  await server(page, 0, { type: 'agents', agents: [{ ...composerTarget, status: 'working', agent: 'pi', project: 'Streaming follow', conversation_history_available: true }] });
  await page.getByRole('button', { name: 'Open Streaming follow on Fedora' }).click();
  const list = page.locator('.conversation-list');
  await expect(list.locator('article').last()).toContainText('Follow seed 23');
  await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
  let sequence = 0;
  const deliver = (incoming: Record<string, unknown>[], remove: string[] = []) => deliverConversationBatch(page, { after: sequence++, entries: incoming, remove, sourceRevision: 'stream:1', capabilities });
  const latestInView = async () => {
    await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
    expect(await list.locator('article').last().evaluate(element => element.getBoundingClientRect().bottom <= element.closest('.conversation-list')!.getBoundingClientRect().bottom + 2)).toBe(true);
  };
  return { list, entry, deliver, latestInView };
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
        expect(await distanceFromBottom(list)).toBeGreaterThan(2);
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
  const { list, entry, deliver, latestInView } = await openFollowingConversation(page);
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
  expect(await distanceFromBottom(list)).toBeGreaterThan(100);
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

test('live conversation deltas preserve local reading, selection, tool expansion and table offsets', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await connectFedora(page, { capabilities: ['conversation_history', 'conversation_activity'] });
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
  const deliver = (after: number, batch: { entries: Record<string, unknown>[]; remove?: string[]; state?: string }) => deliverConversationBatch(page, { after, sourceRevision: 'live-source:1', capabilities, ...batch });
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
  await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
  await closeSocket(page, 0);
  await expect(list.getByText('Final response', { exact: true })).toHaveCount(1);
  const firstBeforeScroll = await list.locator('article').first().getAttribute('data-conversation-entry-id');
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
  await expect(list.locator('article').first()).not.toHaveAttribute('data-conversation-entry-id', firstBeforeScroll!);
});

test('loaded windows retain tool expansion and table offsets without leaking state to another target', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await connectFedora(page, { capabilities: ['conversation_history'] });
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
    await connectFedora(page, { capabilities: ['conversation_history', 'conversation_activity'] });
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
    await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
    const deliver = (after: number, incoming: Record<string, unknown>[]) => deliverConversationBatch(page, { after, entries: incoming, sourceRevision: 'follow:1', capabilities, state: 'recorded' });
    await deliver(0, [{ id: 'entry:240', role: 'user', text: 'Appended prompt', timestamp: '' }, { id: 'entry:241', role: 'assistant', text: 'Appended intermediate', timestamp: '' }]);
    await expect(list.getByText('Appended intermediate', { exact: true })).toBeVisible();
    await deliver(1, [{ id: 'entry:242', role: 'assistant', text: '', timestamp: '', tools: [{ id: 'tail-tool', name: 'Appended tool', input: '{}', output: 'done' }] }]);
    await expect(list.locator('summary').filter({ hasText: 'Appended tool' })).toBeVisible();
    await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
    expect(await list.locator('article').count()).toBeLessThanOrEqual(24);
    await list.evaluate(element => { element.dispatchEvent(new Event('pointerdown', { bubbles: true })); element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
    const anchor = await list.locator('article').first().getAttribute('data-conversation-entry-id');
    await deliver(2, [{ id: 'entry:243', role: 'assistant', text: 'While reading older', timestamp: '' }]);
    await expect(list.locator('article').first()).toHaveAttribute('data-conversation-entry-id', anchor!);
    await expect(list.getByText('While reading older', { exact: true })).toHaveCount(0);
  });

  test(`bottom scrolling traverses multiple bounded loaded windows (${source})`, async ({ page }) => {
    await connectFedora(page, { capabilities: ['conversation_history', 'conversation_activity'] });
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
    expect(await distanceFromBottom(list)).toBeGreaterThan(2);
    await scrollConversationToBottom(page);
    await expect(list.locator('[data-conversation-entry-id="window:639"]')).toBeVisible();
    await expect(list.locator('article')).toHaveCount(24);
  });

  test(`reader Conversation and Full history keep ${source} updates read-only`, async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
    await page.addInitScript(() => localStorage.setItem('herdr_terminal_resize_session', 'true'));
    await boot(page, [fedora]);
    await expect.poll(() => socketCount(page)).toBe(1);
    await saveDeviceCredential(page, { id: 'credential-reader', deviceId: 'device-reader', secret: 'R'.repeat(43), role: 'reader' });
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
    await connectFedora(page, { capabilities: ['conversation_history', 'conversation_activity'] });
    await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'scroll-terminal', generation: 1, agent_session_id: 'scroll-session', status: 'working', project: 'Scroll intent', agent: source === 'pi_events' ? 'pi' : 'codex', conversation_history_available: true }] });
    const capabilities = { source, active_branch: source === 'pi_events' };
    const entries = Array.from({ length: 120 }, (_, index) => ({ id: `scroll:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Scroll message ${index} ` + 'wrapped history '.repeat(12) }));
    await setConversationFixture(page, { entries, total: 120, sourceRevision: 'scroll:1', capabilities, sequence: 0 });
    await page.getByRole('button', { name: 'Open Scroll intent on Fedora' }).click();
    await page.getByRole('button', { name: 'Conversation history' }).click();
    await page.getByRole('button', { name: 'Full history', exact: true }).click();
    const list = page.locator('.conversation-list');

    await expect(list.locator('article')).toHaveCount(24);
    await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
    await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
    await expect(page.locator('.conversation-jump')).toHaveCount(0);
    for (const delta of [-8, -8, -8, 5, -4, 7]) {
      const before = await distanceFromBottom(list);
      await list.evaluate((element, value) => { element.scrollBy(0, value); element.dispatchEvent(new Event('scroll')); }, delta);
      await expect(page.locator('.conversation-jump')).toHaveCount(0);
      expect(Math.abs(await distanceFromBottom(list) - (before - delta))).toBeLessThanOrEqual(2);
    }
    await list.dispatchEvent('pointerup', { pointerType: 'touch' });
    await list.evaluate(element => { element.scrollBy(0, -160); element.dispatchEvent(new Event('scroll')); });
    expect(await distanceFromBottom(list)).toBeGreaterThan(2);
    await scrollConversationToBottom(page);
    await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
  });

  test(`scroll intent: wheel window and bottom arrival preserve reading and resume (${source})`, async ({ page, browserName }) => {
    await page.addInitScript(() => localStorage.setItem("herdr_default_agent_view", "terminal"));
    await connectFedora(page, { capabilities: ['conversation_history', 'conversation_activity'] });
    await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', server_session_id: 'primary', terminal_id: 'scroll-terminal', generation: 1, agent_session_id: 'scroll-session', status: 'working', project: 'Scroll window', agent: source === 'pi_events' ? 'pi' : 'codex', conversation_history_available: true }] });
    const capabilities = { source, active_branch: source === 'pi_events' };
    const entries = Array.from({ length: 280 }, (_, index) => ({ id: `scroll:${index}`, timestamp: '', role: index % 2 ? 'assistant' : 'user', text: `Scroll message ${index} ` + 'wrapped history '.repeat(12) }));
    await setConversationFixture(page, { entries: entries.slice(80), total: 280, sourceRevision: 'scroll:1', capabilities, sequence: 0, hasMore: true, nextCursor: 'scroll-older', pages: { 'scroll-older': { entries: entries.slice(0, 80) } } });
    await page.getByRole('button', { name: 'Open Scroll window on Fedora' }).click();
    await page.getByRole('button', { name: 'Conversation history' }).click();
    await page.getByRole('button', { name: 'Full history', exact: true }).click();
    const list = page.locator('.conversation-list');

    await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
    await list.evaluate(element => { element.scrollTop = 0; element.dispatchEvent(new Event('scroll')); });
    await showOlderConversationEntry(page, 'scroll:0');
    await expect(list.locator('[data-conversation-entry-id="scroll:0"]')).toBeAttached();
    await scrollConversationToBottom(page);
    await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
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
    expect(await distanceFromBottom(list)).toBeGreaterThan(2);
    await page.waitForTimeout(100);
    const anchor = await list.locator('article').first().getAttribute('data-conversation-entry-id');
    const top = await list.evaluate(element => element.scrollTop);
    const deliver = (after: number, incoming: Record<string, unknown>[]) => deliverConversationBatch(page, { after, entries: incoming, sourceRevision: 'scroll:1', capabilities });
    await deliver(0, [{ id: 'live:scroll', role: 'assistant', text: 'Provisional while reading', timestamp: '' }]);
    await expect(list.locator('article').first()).toHaveAttribute('data-conversation-entry-id', anchor!);
    expect(Math.abs(await list.evaluate(element => element.scrollTop) - top)).toBeLessThanOrEqual(2);
    expect(await distanceFromBottom(list)).toBeGreaterThan(2);
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
    await expect.poll(() => distanceFromBottom(list)).toBeGreaterThan(2);
    await scrollConversationToBottom(page);
    await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
    await expect(page.locator('.conversation-jump')).toHaveCount(0);
    await deliver(2, [{ id: 'scroll:281', role: 'assistant', text: 'Following after momentum', timestamp: '' }]);
    await expect(list.getByText('Following after momentum', { exact: true })).toBeVisible();
    await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
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
    expect(await distanceFromBottom(list)).toBeGreaterThan(2);
  });
}

test('scroll intent: upward movement releases following and explicit bottom arrival restores it', async ({ page }) => {
  await page.clock.install();
  await page.addInitScript(() => localStorage.setItem('herdr_default_agent_view', 'terminal'));
  await connectFedora(page, { capabilities: ['conversation_history'] });
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Momentum', agent: 'pi', conversation_history_available: true, agent_session_id: 'momentum-session' }] });
  await setConversationFixture(page, { entries: Array.from({ length: 80 }, (_, index) => ({ id: `momentum:${index}`, role: index % 2 ? 'assistant' : 'user', text: `Momentum ${index} ` + 'wrapped text '.repeat(15), timestamp: '' })), total: 80 });
  await page.getByRole('button', { name: 'Open Momentum on Fedora' }).click();
  await page.getByRole('button', { name: 'Conversation history' }).click();
  const list = page.locator('.conversation-list');
  await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollBy(0, -400); element.dispatchEvent(new Event('scroll')); });
  await list.dispatchEvent('pointerup', { pointerType: 'touch' });

  expect(await distanceFromBottom(list)).toBeGreaterThan(2);
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
  await expect.poll(() => distanceFromBottom(list)).toBeGreaterThan(2);
  await scrollConversationToBottom(page);
  await expect.poll(() => distanceFromBottom(list)).toBeLessThan(2);
  await expect(page.locator('.conversation-jump')).toHaveCount(0);
  await page.clock.runFor(350);
  await list.dispatchEvent('pointerdown', { pointerType: 'touch' });
  await list.evaluate(element => { element.scrollBy(0, -200); element.dispatchEvent(new Event('scroll')); });
  expect(await distanceFromBottom(list)).toBeGreaterThan(2);
});
