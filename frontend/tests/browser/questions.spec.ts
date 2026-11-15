import { expect, test } from '@playwright/test';
import { boot, commands, fedora, handshake, server, setAutoCommands, setConversationFixture, setNextInteraction, socketCount } from '../support/journey-relay';

test('handles approvals, chained questions, and notification routing', async ({ page }) => {
  const target = encodeURIComponent(JSON.stringify({
    pane_id: 'w1:p1', host: 'fedora', notification_id: 'notice-1',
  }));
  await boot(page, [fedora], `/#notify=${target}`);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'approval', event_id: 'notice-1', project: 'Approvals', agent: 'claude', options: ['Approve once', 'Deny'] }] });
  await expect(page.getByRole('main', { name: /Terminal for Approvals/ })).toBeVisible();
  expect((await commands(page)).filter((command) => command.type === 'respond')).toHaveLength(0);

  const first = {
    id: 'q1', kind: 'single_select', question: 'Choose deployment scope',
    options: [{ index: 0, label: 'Repository', description: 'All files' }, { index: 1, label: 'Module' }],
    other: { label: 'None of the above', placeholder: 'Optional notes', allow_empty: true },
    submit_label: 'Next', can_go_back: false, can_chat: true, question_index: 1, question_total: 2,
  };
  const second = {
    ...first, id: 'q2', question: 'Choose device coverage', submit_label: 'Submit', can_go_back: true, question_index: 2,
  };
  await setAutoCommands(page, false);
  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', attention_kind: 'question', project: 'Approvals', agent: 'claude', interaction: first });
  await expect(page.getByText('Question 1 of 2')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Chat about this' })).toBeHidden();
  await page.evaluate(() => {
    const trace = window as unknown as {
      __questionUnmounts: number;
      __questionObserver: MutationObserver;
    };
    trace.__questionUnmounts = 0;
    trace.__questionObserver = new MutationObserver(() => {
      if (!document.querySelector('form.question-form')) trace.__questionUnmounts += 1;
    });
    trace.__questionObserver.observe(document.body, { childList: true, subtree: true });
  });
  await page.getByRole('radio', { name: /Repository/ }).click();
  await page.getByRole('button', { name: 'Next' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'answer_question').length).toBe(1);
  const answer = (await commands(page)).find((command) => command.type === 'answer_question')!;

  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Approvals', agent: 'claude' }],
  });
  await expect(page.getByRole('group', { name: 'Choose deployment scope' })).toBeVisible();
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'blocked', attention_kind: 'chat',
      project: 'Approvals', agent: 'claude',
    }],
  });
  await expect(page.getByRole('group', { name: 'Choose deployment scope' })).toBeVisible();
  await expect(page.getByRole('main', { name: /Questions for Approvals/ })).toBeVisible();

  await server(page, 0, {
    type: 'command_result', action: 'answer_question', request_id: answer.request_id,
    ok: true, phase: 'advanced', data: { interaction: second },
  });
  await setAutoCommands(page, true);
  await expect(page.getByRole('group', { name: 'Choose device coverage' })).toBeVisible();
  await expect(page.getByText('Question 2 of 2')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Previous' })).toBeVisible();
  expect(answer).toMatchObject({ selected_indices: [0], other_selected: false, protocol: 3 });
  for (let update = 0; update < 3; update += 1) {
    await server(page, 0, {
      type: 'agent_update', pane_id: 'w1:p1', status: 'blocked', pane_revision: 10 + update,
    });
  }
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi',
    attention_kind: 'question', interaction: second,
  });
  expect(await page.evaluate(() => {
    const trace = window as unknown as {
      __questionUnmounts: number;
      __questionObserver: MutationObserver;
    };
    trace.__questionObserver.disconnect();
    return trace.__questionUnmounts;
  })).toBe(0);

  await setAutoCommands(page, false);
  await page.getByRole('radio', { name: 'Module' }).click();
  await page.getByRole('button', { name: 'Submit' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'answer_question').length).toBe(2);
  const finalAnswer = (await commands(page))
    .filter((command) => command.type === 'answer_question').at(-1)!;
  await server(page, 0, {
    type: 'blocked', pane_id: 'w1:p1', attention_kind: 'unknown',
    project: 'Approvals', agent: 'claude', interaction: null,
  });
  await server(page, 0, {
    type: 'command_result', action: 'answer_question', request_id: finalAnswer.request_id,
    ok: true, phase: 'confirmed',
  });
  await expect(page.getByText('Answers submitted.')).toBeVisible();
  await setAutoCommands(page, true);

  await server(page, 0, {
    type: 'blocked', pane_id: 'w1:p1', attention_kind: 'approval', project: 'Approvals', agent: 'claude',
    interaction: null, options: ['Proceed with plan', 'Cancel'],
  });
  await expect(page.getByRole('group', { name: 'Choose device coverage' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Proceed with plan' })).toBeVisible();
  const composer = page.getByRole('combobox', { name: 'Prompt' });
  await expect(composer).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();

  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Proceed with plan' }).click();
  await expect(page.getByRole('button', { name: 'Copy', exact: true })).toBeDisabled();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'respond').length).toBe(1);
  const approvalResponse = (await commands(page)).filter((command) => command.type === 'respond').at(-1)!;
  await server(page, 0, {
    type: 'command_result', request_id: approvalResponse.request_id, action: 'respond', ok: true, phase: 'accepted',
  });
  await setAutoCommands(page, true);

  const working = { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Approvals', agent: 'claude' }] };
  await server(page, 0, working);
  await server(page, 0, working);
  await expect(composer).toBeEnabled();
});

for (const resizeWhileHidden of [false, true]) {
test(resizeWhileHidden
  ? 'preserves terminal position through a delayed hidden-layout scroll'
  : 'hands an active chat to structured questions without losing terminal position', async ({ page }) => {
  const interaction = {
    id: 'live-question', kind: 'single_select', question: 'Choose the delivery style',
    options: [{ index: 0, label: 'Direct' }, { index: 1, label: 'Staged' }],
    submit_label: 'Submit', can_go_back: false, question_index: 1, question_total: 1,
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Live chat', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Live chat on Fedora' }).click();
  const terminal = page.getByRole('log');
  const content = Array.from({ length: 140 }, (_, index) => `conversation line ${index + 1}`).join('\n');
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', format: 'plain', content });
  await expect(terminal).toContainText('conversation line 140');
  await terminal.evaluate(async (element) => {
    await document.fonts.ready;
    let previous = '';
    let settled = 0;
    while (settled < 3) {
      await new Promise(requestAnimationFrame);
      const geometry = JSON.stringify([
        element.scrollTop, element.scrollHeight, element.clientHeight, element.clientWidth,
        ...[...element.querySelectorAll('[data-terminal-row], .terminal-virtual-spacer')]
          .map((row) => row.getBoundingClientRect().height),
      ]);
      settled = geometry === previous ? settled + 1 : 0;
      previous = geometry;
    }
  });
  const before = await terminal.evaluate((element) => {
    const target = Math.max(0, element.scrollHeight - element.clientHeight - 80);
    element.scrollTop = target;
    element.dispatchEvent(new Event('scroll'));
    return target;
  });
  await expect.poll(async () => terminal.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);

  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content,
    attention_kind: 'question',
    interaction,
  });
  await expect(page.getByRole('group', { name: interaction.question })).toBeVisible();
  await expect(page.getByRole('log')).toBeHidden();
  if (resizeWhileHidden) {
    await page.getByRole('log', { includeHidden: true }).evaluate(async (element) => {
      element.style.flex = 'none';
      element.style.height = `${element.scrollHeight - element.scrollTop - 24}px`;
      element.dispatchEvent(new Event('scroll'));
      await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      element.style.removeProperty('height');
      element.style.removeProperty('flex');
      await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    });
  }

  for (let update = 0; update < 2; update += 1) {
    await server(page, 0, {
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Live chat', agent: 'codex' }],
    });
  }
  await expect(page.getByRole('group', { name: interaction.question })).toBeHidden();
  const after = await terminal.evaluate((element) => element.scrollTop);
  expect(Math.abs(after - before), `before=${before} after=${after}`).toBeLessThan(2);
});
}

test('shows a live plan question immediately without reopening the agent', async ({ page }) => {
  const interaction = {
    id: 'plan-question', kind: 'single_select',
    question: 'Where should the new app live relative to the existing Herdr relay app?',
    options: [
      { index: 0, label: 'Separate package', description: 'Keep the relay release path untouched.' },
      { index: 1, label: 'New route', description: 'Add the questionnaire inside the existing app.' },
    ],
    other: { label: 'None of the above', placeholder: 'Add details' },
    submit_label: 'Next', can_go_back: false, question_index: 1, question_total: 3,
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Plan app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Plan app on Fedora' }).click();
  const questionContent = 'Question 1/3 (3 unanswered)\nWhere should the new app live?';
  await server(page, 0, {
    type: 'pane_content',
    pane_id: 'w1:p1',
    format: 'plain',
    content: questionContent,
    content_fingerprint: 'plan-question-content',
  });
  await expect(page.getByRole('log', { name: 'Agent terminal output' })).toContainText('Question 1/3');
  await server(page, 0, {
    type: 'pane_delta',
    pane_id: 'w1:p1',
    format: 'plain',
    base_fingerprint: 'plan-question-content',
    content_fingerprint: 'plan-question-content',
    segments: [],
    attention_kind: 'question',
    interaction,
  });

  await expect(page.getByRole('group', { name: interaction.question })).toBeVisible();
  await expect(page.getByRole('log', { name: 'Agent terminal output' })).toBeHidden();
  await expect(page.getByRole('radio', { name: 'Separate package' })).toBeVisible();
  await expect(page.getByPlaceholder('Type a reply…')).toBeEnabled();
  await expect(page.getByRole('button', { name: 'Send prompt', exact: true })).toBeDisabled();
});

test('keeps a nonfinal question mounted when a transient frame is confirmed', async ({ page }) => {
  const first = {
    id: 'q1', kind: 'single_select', question: 'Choose deployment scope',
    options: [{ index: 0, label: 'Repository' }, { index: 1, label: 'Module' }],
    other: { label: 'Other', placeholder: 'Other answer' },
    submit_label: 'Next', can_go_back: false, question_index: 1, question_total: 2,
  };
  const second = {
    ...first, id: 'q2', question: 'Choose device coverage',
    submit_label: 'Submit', can_go_back: true, question_index: 2,
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question',
      project: 'Questions', agent: 'claude', interaction: first,
    }],
  });
  await page.getByRole('button', { name: 'Open Questions on Fedora' }).click();
  await setAutoCommands(page, false);
  await page.getByRole('radio', { name: 'Repository' }).click();
  await page.getByRole('button', { name: 'Next' }).click();
  await expect.poll(async () => (await commands(page))
    .filter((command) => command.type === 'answer_question').length).toBe(1);
  const answer = (await commands(page)).find((command) => command.type === 'answer_question')!;

  await server(page, 0, {
    type: 'command_result', action: 'answer_question', request_id: answer.request_id,
    ok: true, phase: 'confirmed',
  });
  await expect(page.getByRole('group', { name: first.question })).toBeVisible();
  await expect(page.getByText('Unexpected question result.')).toBeVisible();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi',
    attention_kind: 'question', interaction: second,
  });
  await expect(page.getByRole('group', { name: second.question })).toBeVisible();
});

test('shows long approval choices in full on touch-sized buttons', async ({ page }) => {
  const long = 'Yes, allow all edits during this session (shift+tab to cycle)';
  const path = 'Yes, and always allow ./frontend/src/components/AgentListWithAVeryLongUnbrokenFileName.svelte';
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'approval', project: 'Long choice', agent: 'claude', options: [long, path, 'No'] }],
  });
  const choice = page.getByRole('button', { name: long, exact: true });
  await expect(choice).toBeVisible();
  // A long unbroken token wraps inside the card instead of widening the page.
  const card = (await page.locator('.agent-actions').boundingBox())!;
  const pathBox = (await page.getByRole('button', { name: path, exact: true }).boundingBox())!;
  expect(pathBox.x + pathBox.width).toBeLessThanOrEqual(card.x + card.width + 1);
  expect(await page.evaluate(() => matchMedia('(pointer: coarse)').matches)).toBe(true);
  expect((await choice.boundingBox())!.height).toBeGreaterThanOrEqual(44);
  expect((await page.getByRole('button', { name: 'No', exact: true }).boundingBox())!.height).toBeGreaterThanOrEqual(44);
});

test('answers a short single-choice question from its card in one tap', async ({ page }) => {
  const pageErrors: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  const short = {
    id: 'short-question', kind: 'single_select',
    question: `Which database should the migration target? ${'It rewrites every table. '.repeat(12)}Production data is dropped.`,
    options: [{ index: 0, label: 'Postgres' }, { index: 1, label: 'SQLite' }, { index: 2, label: 'MySQL' }],
    submit_label: 'Submit', question_index: 1, question_total: 1,
  };
  const long = {
    ...short, id: 'long-question',
    options: ['A', 'B', 'C', 'D', 'E'].map((label, index) => ({ index, label })),
  };
  const described = {
    ...short, id: 'described-question',
    options: [{ index: 0, label: 'Keep' }, { index: 1, label: 'Drop', description: 'Deletes the table' }],
  };
  const essay = {
    ...short, id: 'essay-question', question: `Pick one. ${'Background detail. '.repeat(30)}`,
    options: [{ index: 0, label: 'One' }, { index: 1, label: 'Two' }, { index: 2, label: 'Three' }],
  };
  const followUp = { ...short, id: 'follow-up', question: 'Which region?', options: [{ index: 0, label: 'Asia' }, { index: 1, label: 'Europe' }] };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [
      { pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Short', agent: 'claude', options: [], interaction: short },
      { pane_id: 'w1:p2', status: 'blocked', attention_kind: 'question', project: 'Long', agent: 'claude', options: [], interaction: long },
      { pane_id: 'w1:p3', status: 'blocked', attention_kind: 'question', project: 'Described', agent: 'claude', options: [], interaction: described },
      { pane_id: 'w1:p4', status: 'blocked', attention_kind: 'question', project: 'Essay', agent: 'claude', options: [], interaction: essay },
    ],
  });
  // Five options, options with descriptions, or a very long question keep the full form;
  // nothing is preselected.
  await expect(page.getByRole('button', { name: 'Choose answer (5)' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Choose answer (2)' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Choose answer (3)' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'More…' })).toBeVisible();
  // The whole question is visible next to its one-tap answers.
  const shortCard = page.locator('.agent-card', { has: page.getByRole('button', { name: 'SQLite', exact: true }) });
  const shortPrompt = shortCard.locator('.prompt-preview');
  expect(await shortPrompt.evaluate((element) => element.scrollHeight - element.clientHeight)).toBeLessThanOrEqual(1);

  // Replies come from the test, shaped like the relay's finishQuestionWatch.
  await setAutoCommands(page, false);
  const answers = async () => (await commands(page)).filter((command) => command.type === 'answer_question');
  const reply = async (result: Record<string, unknown>) => {
    const last = (await answers()).at(-1)!;
    await server(page, 0, { type: 'command_result', action: 'answer_question', request_id: last.request_id, ...result });
  };
  const unexpected = 'The agent opened an unexpected question; the screen was refreshed';

  // A reply carrying a malformed question neither crashes the card nor replaces it.
  await page.getByRole('button', { name: 'SQLite', exact: true }).click();
  await expect.poll(async () => (await answers()).length).toBe(1);
  expect((await answers())[0]).toMatchObject({ interaction_id: 'short-question', selected_indices: [1], other_selected: false });
  await reply({ ok: false, phase: 'failed', error: unexpected, data: { interaction: { id: 'broken', question: 'No options' } } });
  await expect(page.getByRole('button', { name: 'SQLite', exact: true })).toBeVisible();
  await expect(page.locator('.toast')).toHaveText('The agent asked another question; it will appear with the next update.');
  await expect(page.locator('.toast.error')).toHaveCount(0);

  // A real follow-up arrives as a failed reply with the new question: the card shows it,
  // without an error and without leaving the list.
  await page.getByRole('button', { name: 'SQLite', exact: true }).click();
  await expect.poll(async () => (await answers()).length).toBe(2);
  await reply({ ok: false, phase: 'failed', error: unexpected, data: { interaction: followUp } });
  await expect(page.getByRole('button', { name: 'Asia', exact: true })).toBeVisible();
  await expect(page.locator('.toast.error')).toHaveCount(0);
  await expect(page.getByRole('main', { name: /Terminal for/ })).toHaveCount(0);

  // A plain failure is reported where the user is.
  await page.getByRole('button', { name: 'Asia', exact: true }).click();
  await expect.poll(async () => (await answers()).length).toBe(3);
  await reply({ ok: false, phase: 'failed', error: 'Pane changed before the answer landed' });
  await expect(page.locator('.toast.error')).toContainText('Pane changed before the answer landed');
  await expect(page.getByRole('main', { name: /Terminal for/ })).toHaveCount(0);
  expect(pageErrors).toEqual([]);
});

test('treats a follow-up question from the answer form like the card does', async ({ page }) => {
  const lone = {
    id: 'lone-question', kind: 'single_select', question: 'Which database?',
    options: [{ index: 0, label: 'Postgres' }, { index: 1, label: 'SQLite' }],
    submit_label: 'Submit', question_index: 1, question_total: 1,
  };
  const followUp = { ...lone, id: 'follow-up', question: 'Which region?', options: [{ index: 0, label: 'Asia' }, { index: 1, label: 'Europe' }] };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Form', agent: 'claude', options: [], interaction: lone }],
  });
  await page.getByRole('button', { name: 'More…' }).click();
  await expect(page.getByRole('group', { name: 'Which database?' })).toBeVisible();
  await setAutoCommands(page, false);
  await page.getByRole('radio', { name: /SQLite/ }).click();
  await page.getByRole('button', { name: 'Submit' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'answer_question').length).toBe(1);
  const answer = (await commands(page)).find((command) => command.type === 'answer_question')!;
  await server(page, 0, {
    type: 'command_result', action: 'answer_question', request_id: answer.request_id, ok: false, phase: 'failed',
    error: 'The agent opened an unexpected question; the screen was refreshed', data: { interaction: followUp },
  });
  await expect(page.getByRole('group', { name: 'Which region?' })).toBeVisible();
  await expect(page.locator('.toast')).toHaveText('The agent asked another question.');
  await expect(page.locator('.toast.error')).toHaveCount(0);

  // In a questionnaire, landing on an unexpected question is still reported as an error.
  const third = { ...followUp, id: 'third', question: 'Which zone?', question_index: 3, question_total: 3, submit_label: 'Next' };
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Form', agent: 'claude', options: [],
      interaction: { ...followUp, id: 'first-of-three', question: 'Which tier?', question_index: 1, question_total: 3, submit_label: 'Next' } }],
  });
  await expect(page.getByRole('group', { name: 'Which tier?' })).toBeVisible();
  await page.getByRole('radio', { name: /Asia/ }).click();
  await page.getByRole('button', { name: 'Next' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'answer_question').length).toBe(2);
  const second = (await commands(page)).filter((command) => command.type === 'answer_question').at(-1)!;
  // An agent update lands before the rejection; the toast still judges the submitted question.
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Form', agent: 'claude', options: [], interaction: lone }],
  });
  await server(page, 0, {
    type: 'command_result', action: 'answer_question', request_id: second.request_id, ok: false, phase: 'failed',
    error: 'The agent opened an unexpected question; the screen was refreshed', data: { interaction: third },
  });
  await expect(page.getByRole('group', { name: 'Which zone?' })).toBeVisible();
  await expect(page.locator('.toast.error')).toHaveText('The agent opened an unexpected question; the screen was refreshed');
});

test('keeps one-tap answers working with missing option indexes and explains disabled ones', async ({ page }) => {
  const pageErrors: string[] = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  const unindexed = {
    id: 'unindexed', kind: 'single_select', question: 'Which side?',
    options: [{ label: 'Left' }, { label: 'Right' }], submit_label: 'Submit', question_index: 1, question_total: 1,
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    inventory: { state: 'error', error_code: 'topology_churn', message: 'Agent inventory is changing too quickly.', last_attempt_at: 1, last_success_at: 0, stale: true },
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Sides', agent: 'claude', options: [], interaction: unindexed }],
  });
  const left = page.getByRole('button', { name: 'Left', exact: true });
  await expect(left).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Right', exact: true })).toBeVisible();
  await expect(left).toHaveAttribute('title', 'Agent controls are unavailable until Herdr inventory recovers.');

  // Once inventory recovers, an option without an index answers by its position.
  await server(page, 0, { type: 'inventory_status', state: 'ready', error_code: '', message: '', last_attempt_at: 2, last_success_at: 2, stale: false });
  await page.getByRole('button', { name: 'Right', exact: true }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'answer_question').length).toBe(1);
  expect((await commands(page)).find((command) => command.type === 'answer_question')).toMatchObject({ interaction_id: 'unindexed', selected_indices: [1] });
  expect(pageErrors).toEqual([]);
});

test('restores structured questions from the cached agent snapshot after reload', async ({ page }) => {
  const interaction = {
    id: 'reload-question', kind: 'single_select', question: 'Choose reconnect behavior',
    options: [{ index: 0, label: 'Backoff' }, { index: 1, label: 'Fixed retry' }],
    other: { label: 'Other', placeholder: 'Other answer', selected: false, text: '' },
    submit_label: 'Next', can_go_back: false,
  };
  const snapshot = {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude',
      prompt: interaction.question, command: interaction.question, options: [],
      interaction,
    }],
  };

  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, snapshot);
  await expect(page.getByRole('button', { name: 'Choose answer (2)' })).toBeVisible();

  await page.reload();
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, snapshot);

  await expect(page.getByText('Choose reconnect behavior')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Choose answer (2)' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'yes, single permission' })).toBeHidden();
});

test('restores a confirmed choice after navigating away from an incomplete draft', async ({ page }) => {
  const first = {
    id: 'confirmed-reconnect', kind: 'single_select', question: 'Choose reconnect strategy',
    options: [{ index: 0, label: 'Backoff' }, { index: 1, label: 'Signals' }],
    other: { label: 'Other', placeholder: 'Other answer', selected: false, text: '' },
    submit_label: 'Next', can_go_back: false,
  };
  const second = {
    id: 'confirmed-offline', kind: 'multi_select', question: 'Choose offline scope',
    options: [{ index: 0, label: 'App shell' }], submit_label: 'Next', can_go_back: true,
  };

  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude' }] });
  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude', interaction: first });
  await page.getByRole('button', { name: 'Open Questions on Fedora' }).click();
  await page.getByRole('textbox', { name: 'Other answer' }).focus();
  await expect(page.getByRole('radio', { name: 'Other' })).toBeChecked();

  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude', interaction: second });
  await expect(page.getByRole('group', { name: 'Choose offline scope' })).toBeVisible();
  const confirmed = {
    ...first,
    options: first.options.map((option) => ({ ...option, selected: option.index === 1 })),
  };
  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude', interaction: confirmed });

  await expect(page.getByRole('radio', { name: 'Signals' })).toBeChecked();
  await expect(page.getByRole('radio', { name: 'Other' })).not.toBeChecked();
});

test('keeps the third single choice checked across live pane transitions', async ({ page }) => {
  const first = {
    id: 'live-reconnect', kind: 'single_select', question: 'When the relay connection drops, how should the client attempt to reconnect?',
    options: [
      { index: 0, label: 'Exponential backoff', description: 'Retry on a growing delay.' },
      { index: 1, label: 'Fixed short interval', description: 'Retry every few seconds.' },
      { index: 2, label: 'Backoff plus signals', description: 'Reset when connectivity returns.', selected: true },
    ],
    other: { label: 'Other', placeholder: 'Other answer', selected: false, text: '' },
    submit_label: 'Next', can_go_back: false,
  };
  const second = {
    id: 'live-offline', kind: 'multi_select', question: 'Which capabilities should remain available offline?',
    options: [
      { index: 0, label: 'App shell', selected: true },
      { index: 1, label: 'Queued prompts' },
      { index: 2, label: 'Activity cache', selected: true },
      { index: 3, label: 'Notification handoff' },
    ],
    other: { label: 'Other', placeholder: 'Other answer', selected: false, text: '' },
    submit_label: 'Next', can_go_back: true,
  };
  const agent = {
    pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude',
    interaction: first,
  };

  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, { type: 'agents', agents: [agent] });
  await page.getByRole('button', { name: 'Open Questions on Fedora' }).click();
  await expect(page.getByRole('main', { name: 'Questions for Questions' })).toBeVisible();
  await expect(page.getByRole('log', { name: 'Agent terminal output' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Refresh terminal' })).toHaveCount(0);
  await expect(page.getByRole('button', { name: /Terminal width:/ })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Attach files' })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Arrow keys' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Tab', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Enter', exact: true })).toBeVisible();
  await expect(page.getByRole('radio', { name: /Backoff plus signals/ })).toBeChecked();

  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Next' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'answer_question').length).toBe(1);
  const answer = (await commands(page)).find((command) => command.type === 'answer_question')!;
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi', attention_kind: 'question', interaction: second });
  await server(page, 0, { type: 'command_result', request_id: answer.request_id, ok: true, phase: 'advanced', data: { interaction: second } });
  await expect(page.getByRole('group', { name: second.question })).toBeVisible();

  await page.getByRole('button', { name: /Previous/ }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'navigate_question').length).toBe(1);
  const navigation = (await commands(page)).find((command) => command.type === 'navigate_question')!;
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi', attention_kind: 'question', interaction: first });
  await server(page, 0, { type: 'command_result', request_id: navigation.request_id, ok: true, phase: 'navigated', data: { interaction: first } });
  await server(page, 0, { type: 'agents', agents: [agent] });
  for (let refresh = 0; refresh < 20; refresh += 1) {
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', content: '', format: 'ansi', attention_kind: 'question', interaction: first });
    await page.waitForTimeout(5);
  }

  await expect(page.getByRole('radio', { name: /Backoff plus signals/ })).toBeChecked();
  await expect(page.getByRole('button', { name: 'Next' })).toBeEnabled();
  expect((await commands(page)).filter((command) => command.type === 'read_pane').length).toBeLessThan(6);
});

test('keeps normal single-select answers across repeated question navigation', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  const first = {
    id: 'stable-reconnect', kind: 'single_select', question: 'Choose reconnect behavior',
    options: [
      { index: 0, label: 'Backoff' },
      { index: 1, label: 'Fixed retry' },
      { index: 2, label: 'Backoff plus signals' },
    ],
    other: { label: 'Other', placeholder: 'Other answer', selected: false, text: '' },
    submit_label: 'Next', can_go_back: false,
  };
  const second = {
    id: 'offline-scope', kind: 'multi_select', question: 'Choose offline scope',
    options: [{ index: 0, label: 'App shell' }, { index: 1, label: 'Activity cache' }],
    other: { label: 'Other', placeholder: 'Other answer', selected: false, text: '' },
    submit_label: 'Next', can_go_back: true,
  };
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'claude' }] });
  await server(page, 0, { type: 'blocked', pane_id: 'w1:p1', attention_kind: 'question', project: 'Questions', agent: 'claude', interaction: first });
  await page.getByRole('button', { name: 'Open Questions on Fedora' }).click();
  const questionForm = page.getByRole('form', { name: 'Choose reconnect behavior' });
  const formHeight = await questionForm.evaluate((element) => element.getBoundingClientRect().height);
  expect(formHeight / await page.evaluate(() => innerHeight)).toBeGreaterThan(0.65);

  await page.getByRole('textbox', { name: 'Other answer' }).fill('Hello');
  await expect(page.getByRole('radio', { name: 'Other' })).toBeChecked();
  await setNextInteraction(page, second);
  await page.getByRole('button', { name: 'Next' }).click();
  await expect(page.getByRole('group', { name: 'Choose offline scope' })).toBeVisible();
  await setNextInteraction(page, {
    ...first, can_go_back: false, other: { ...first.other, selected: true, text: 'Hello' },
  });
  await page.getByRole('button', { name: 'Previous' }).click();
  await expect(page.getByRole('group', { name: first.question })).toBeVisible();
  await page.getByRole('radio', { name: 'Backoff plus signals' }).click();
  await expect(page.getByRole('textbox', { name: 'Other answer' })).toHaveValue('');

  await setNextInteraction(page, second);
  await page.getByRole('button', { name: 'Next' }).click();
  await expect(page.getByRole('group', { name: second.question })).toBeVisible();
  await setNextInteraction(page, {
    ...first,
    options: first.options.map((option) => ({ ...option, selected: option.index === 2 })),
    other: { ...first.other, selected: false, text: '' },
  });
  await page.getByRole('button', { name: 'Previous' }).click();
  await expect(page.getByRole('group', { name: first.question })).toBeVisible();
  await expect(page.getByRole('radio', { name: 'Backoff plus signals' })).toBeChecked();
  await expect(page.getByRole('radio', { name: 'Other' })).not.toBeChecked();
  await expect(page.getByRole('textbox', { name: 'Other answer' })).toHaveValue('');
  expect((await commands(page)).filter((command) => command.type === 'navigate_question')).toHaveLength(2);
});

test('does not report failed question navigation as opened', async ({ page }) => {
  const second = {
    id: 'failed-navigation', kind: 'single_select', question: 'Choose release scope',
    options: [{ index: 0, label: 'Backend' }, { index: 1, label: 'Everything' }],
    submit_label: 'Submit', can_go_back: true, question_index: 2, question_total: 2,
  };
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', status: 'blocked', attention_kind: 'question', project: 'Questions', agent: 'codex',
      interaction: second,
    }],
  });
  await page.getByRole('button', { name: 'Open Questions on Fedora' }).click();
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Previous' }).click();
  const navigation = (await commands(page)).find((command) => command.type === 'navigate_question')!;
  await server(page, 0, {
    type: 'command_result', request_id: navigation.request_id, action: 'navigate_question',
    ok: true, phase: 'accepted',
  });
  await server(page, 0, {
    type: 'command_result', request_id: navigation.request_id, action: 'navigate_question',
    ok: false, phase: 'unconfirmed', error: 'The agent still shows the same question; try again',
  });
  await expect(page.getByRole('status').filter({ hasText: 'still shows the same question' })).toBeVisible();
  await expect(page.getByRole('status').filter({ hasText: 'Opened previous question' })).toBeHidden();
  await expect(page.getByRole('group', { name: second.question })).toBeVisible();
});

test('answers a hidden terminal prompt without storing the value anywhere', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['attention_classification', 'secret_input'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Secret app', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Secret app on Fedora' }).click();
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'read_pane').length).toBeGreaterThan(0);
  const secretRead = (await commands(page)).findLast(command => command.type === 'read_pane');
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi',
    request_id: secretRead!.request_id, target: secretRead!.target,
    content: 'make install\n[sudo] password for cv:',
    no_echo: true, no_echo_prompt: '[sudo] password for cv:',
  });

  const prompt = page.getByRole('region', { name: 'Hidden terminal prompt' });
  await expect(prompt).toContainText('[sudo] password for cv:');
  const secret = prompt.getByLabel('Value for the hidden terminal prompt');
  await expect(secret).toHaveAttribute('type', 'password');
  await secret.fill('hunter2');
  await prompt.getByRole('button', { name: 'Send hidden value' }).click();

  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'send_secret'))
    .toMatchObject({ pane_id: 'w1:p1', text: 'hunter2' });
  // The secret must never travel as ordinary text, nor be persisted on the phone.
  const sent = await commands(page);
  expect(sent.some((command) => command.type === 'send_text' || command.type === 'submit_prompt')).toBe(false);
  expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('hunter2');
  await expect(secret).toHaveValue('');

  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'installed', no_echo: false,
  });
  await expect(prompt).toBeHidden();
});

test('keeps a saved draft across a hidden prompt but never persists what it hides', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['attention_classification', 'secret_input'] });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Draft app', agent: 'codex', agent_session_id: 'draft-session' }],
  });
  const openTerminal = async () => {
    await page.getByRole('button', { name: 'Open Draft app on Fedora' }).click();
    return page.getByRole('combobox', { name: 'Prompt' });
  };
  const paneContent = async (extra: Record<string, unknown>) => {
    const read = (await commands(page)).findLast(command => command.type === 'read_pane')!;
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', ...(extra.no_echo === true ? {} : { request_id: read.request_id, target: read.target }), format: 'ansi', ...extra });
  };

  let prompt = await openTerminal();
  await paneContent({ content: 'a shell at rest', no_echo: false });
  await prompt.fill('half-written instructions');

  // Recognition pauses persistence; it must not delete a draft it did not author.
  await paneContent({
    content: 'sudo make install\n[sudo] password for cv:',
    no_echo: true, no_echo_prompt: '[sudo] password for cv:',
  });
  await expect(page.getByRole('region', { name: 'Hidden terminal prompt' })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  prompt = await openTerminal();
  await expect(prompt).toHaveValue('');
  await paneContent({ content: 'Ready', no_echo: false });
  await expect(prompt).toHaveValue('half-written instructions');

  // Text authored while the prompt is up stays out of storage, even after the
  // prompt clears: it may be the secret typed into the wrong field.
  await paneContent({
    content: 'sudo make install\n[sudo] password for cv:',
    no_echo: true, no_echo_prompt: '[sudo] password for cv:',
  });
  await prompt.fill('hunter2');
  await paneContent({ content: 'installed', no_echo: false });
  await expect(page.getByText('Not saved on this phone')).toBeVisible();
  expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('hunter2');

  await page.getByRole('button', { name: 'Back' }).click();
  prompt = await openTerminal();
  await expect(prompt).toHaveValue('');
  await paneContent({ content: 'Ready', no_echo: false });
  await expect(prompt).toHaveValue('half-written instructions');
});

test('names a hidden prompt a relay cannot answer', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Old relay', agent: 'codex' }],
  });
  await page.getByRole('button', { name: 'Open Old relay on Fedora' }).click();
  await server(page, 0, {
    type: 'pane_content', pane_id: 'w1:p1', format: 'ansi', content: 'Enter passphrase:',
    no_echo: true, no_echo_prompt: 'Enter passphrase:',
  });

  const prompt = page.getByRole('region', { name: 'Hidden terminal prompt' });
  await expect(prompt).toContainText('Enter passphrase:');
  await expect(prompt).toContainText('answer at the computer');
  await expect(prompt.getByLabel('Value for the hidden terminal prompt')).toHaveCount(0);
});

test('hidden secret requires correlated observation and never retries uncertain delivery across views', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['secret_input', 'manual_terminal_input', 'conversation_history'] });
  await setConversationFixture(page, { entries: [{ id: 'secret-history', role: 'assistant', timestamp: '', text: 'Synthetic secret input history' }], total: 1 });
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'idle', project: 'Secret safety', agent: 'codex', agent_session_id: 'secret-safety-session', conversation_history_available: true }] });
  await page.getByRole('button', { name: 'Open Secret safety on Fedora' }).click();
  const input = page.getByLabel('Value for the hidden terminal prompt');
  const send = page.getByRole('button', { name: 'Send hidden value' });
  await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', content: 'Password:', no_echo: true });
  await expect(input).toBeDisabled();
  expect((await commands(page)).filter(command => command.type === 'send_secret')).toHaveLength(0);
  const observe = async () => {
    const previous = (await commands(page)).findLast(command => command.type === 'read_pane')?.request_id;
    await expect.poll(async () => (await commands(page)).findLast(command => command.type === 'read_pane')?.request_id).not.toBe(previous);
    const read = (await commands(page)).findLast(command => command.type === 'read_pane')!;
    await server(page, 0, { type: 'pane_content', pane_id: 'w1:p1', request_id: read.request_id, target: read.target, content: 'Password:', no_echo: true });
  };
  await observe();
  await expect(input).toBeEnabled();
  await input.fill('synthetic-browser-value');
  await setAutoCommands(page, false);
  await send.click();
  await expect(input).toHaveValue('');
  const secret = (await commands(page)).find(command => command.type === 'send_secret')!;
  await page.getByRole('button', { name: 'Terminal view', exact: true }).click();
  await expect(send).toBeDisabled();
  await expect(page.getByText(/Delivery is uncertain/)).toBeVisible({ timeout: 20_000 });
  await server(page, 0, { type: 'command_result', request_id: secret.request_id, phase: 'completed', ok: true });
  await page.getByRole('button', { name: 'Keyboard', exact: true }).click();
  await observe();
  await expect(page.getByRole('button', { name: 'I checked the terminal', exact: true })).toBeEnabled();
  await page.getByRole('button', { name: 'Close', exact: true }).click();
  await expect(send).toBeDisabled();
  await expect(input).toHaveValue('');
  await input.evaluate(element => { element.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })); });
  await page.getByRole('button', { name: 'Conversation history', exact: true }).click();
  await expect(page.getByText(/Delivery is uncertain/)).toBeVisible();
  await expect(send).toBeDisabled();
  expect((await commands(page)).filter(command => command.type === 'send_secret')).toHaveLength(1);
  expect(await page.evaluate(() => JSON.stringify(localStorage) + JSON.stringify(sessionStorage))).not.toContain('synthetic-browser-value');
});
