import { expect, test, type Locator, type Page } from '@playwright/test';
import { chooseMore, moreTrigger, openMore } from '../support/journey-chrome';
import { saveDeviceCredential } from '../support/journey-device';
import { createProjectInventory } from '../../src/demo/scene';
import { boot, closeSocket, commands, connectFedora, fedora, handshake, server, socketCount } from '../support/journey-relay';

const themes = ['dark', 'light', 'nord', 'solarized', 'rose', 'latte'];
const useTheme = (page: Page, theme: string) => page.evaluate((value) => {
  document.documentElement.dataset.theme = value;
  void document.body.offsetWidth;
  for (const animation of document.getAnimations()) {
    if (animation instanceof CSSTransition) animation.finish();
  }
}, theme);

test('keeps Home chrome named and reachable at 320px in every theme', async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 740 });
  await page.emulateMedia({ reducedMotion: 'reduce' });
  await page.addInitScript(() => localStorage.setItem('herdr_terminal_font_size', 'compact'));
  await connectFedora(page);
  const header = page.getByRole('banner');
  const more = moreTrigger(page);
  const dialog = page.getByRole('dialog', { name: 'More', exact: true });
  await expect(header.getByRole('button')).toHaveCount(3);
  await expect(header.getByRole('button', { name: 'Start agent' })).toBeVisible();
  await expect(header.getByRole('button', { name: 'Settings' })).toBeVisible();
  await expect(header.getByText('1/1 relays')).toBeVisible();
  for (const button of await header.getByRole('button').all()) {
    const box = (await button.boundingBox())!;
    expect(Math.min(box.width, box.height)).toBeGreaterThanOrEqual(44);
  }
  for (const theme of themes) {
    await useTheme(page, theme);
    await expectTextContrast(more, `${theme}: More trigger`);
    await more.press('Enter');
    await expect(dialog.getByRole('button')).toHaveText(['Search all agents', 'Manage workspaces', 'Activity history', 'Close']);
    await page.keyboard.press('Tab');
    const focused = dialog.getByRole('button', { name: 'Manage workspaces' });
    await expect(focused).toBeFocused();
    await expect(focused).toHaveCSS('outline-style', 'solid');
    const { contrast } = await surfaceContrast(focused, true, 'outlineColor');
    expect.soft(contrast, `${theme}: More focus outline against dialog`).toBeGreaterThanOrEqual(3);
    for (const button of await dialog.getByRole('button').all()) {
      await expectTextContrast(button, `${theme}: ${await button.textContent()}`);
      const box = (await button.boundingBox())!;
      expect(Math.min(box.width, box.height)).toBeGreaterThanOrEqual(44);
    }
    await page.keyboard.press('Escape');
    await expect(more).toBeFocused();
    expect(await more.evaluate(button => getComputedStyle(button).outlineStyle)).toBe('solid');
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(320);
  }
  await handshake(page, 0, { capabilities: ['self_update'] });
  await expect(header.getByRole('button')).toHaveCount(2);
  await chooseMore(page, 'Activity history');
  await expect(header.getByRole('heading', { name: 'Activity' })).toBeFocused();
  await expect(header.getByRole('button')).toHaveCount(2);
  await page.goBack();
  await expect(header.getByRole('button', { name: 'Settings' })).toBeVisible();
  await page.goForward();
  await expect(header.getByRole('heading', { name: 'Activity' })).toBeVisible();
});

test('hands keyboard focus to Settings and Start Agent from Home', async ({ page }) => {
  await connectFedora(page);
  const header = page.getByRole('banner');
  for (const [button, heading] of [['Settings', 'Settings'], ['Start agent', 'Start Agent']]) {
    await header.getByRole('button', { name: button, exact: true }).press('Enter');
    await expect(header.getByRole('heading', { name: heading, exact: true })).toBeFocused();
    await header.getByRole('button', { name: 'Back', exact: true }).press('Enter');
  }
});

test('keeps More focused when the launch relay disconnects', async ({ page }) => {
  await connectFedora(page);
  await chooseMore(page, 'Activity history');
  await moreTrigger(page).press('Enter');
  const more = page.getByRole('dialog', { name: 'More', exact: true });
  const start = more.getByRole('button', { name: 'Start agent', exact: true });
  await start.focus();
  await expect(start).toBeFocused();

  await closeSocket(page, 0);
  await expect(start).toHaveCount(0);
  await expect(more.getByRole('button', { name: 'Search all agents', exact: true })).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(moreTrigger(page)).toBeFocused();
});

test('manages workspace modals, grouped worktrees, and drag ordering', async ({ page }) => {
  await page.clock.install();
  await connectFedora(page, {
    capabilities: ['tab_rename', 'agent_launch', 'attention_classification', 'directory_browser', 'workspace_management', 'workspace_reorder_block', 'worktree_management'],
  });
  const { project: parent, linked, projectAgent } = createProjectInventory();
  const shellOnly = {
    workspace_id: 'w2', number: 2, label: 'Shell Only', pane_count: 1, tab_count: 1,
    cwd: '/home/test/shell-only',
  };
  await server(page, 0, { type: 'workspaces', workspaces: [parent, shellOnly] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      ...projectAgent, tab_label: 'Agent', status: 'working', project: 'fallback-project',
    }],
  });

  await expect(page.getByText('Project', { exact: true }).first()).toBeVisible();
  await expect(page.getByText('Shell Only', { exact: true }).first()).toBeVisible();
  // Idle-only cards start collapsed in the mixed default; a tap expands them.
  await expect(page.getByText('No agents are running in this workspace.')).toBeHidden();
  await page.locator('.workspace-card').filter({ hasText: 'Shell Only' }).locator('summary').click();
  await expect(page.getByText('No agents are running in this workspace.')).toBeVisible();
  // The workspace keeps its directory; the agent row uses that space for activity.
  const projectWorkspace = page.locator('.workspace-card').filter({ hasText: 'Project' }).first();
  await expect(projectWorkspace.locator('summary .workspace-card-title .host-badge')).toHaveText('@Fedora');
  await expect(projectWorkspace.locator('summary .path-row')).toHaveText('/work/project');
  await expect(projectWorkspace.locator('.compact-agent-card .agent-activity')).toHaveText('Working');
  await expect(projectWorkspace.locator('.compact-agent-card .host-badge')).toHaveCount(0);
  await expect(page.locator('.workspace-card').filter({ hasText: 'Shell Only' }).locator('summary .path-row'))
    .toHaveText('~/shell-only');

  await chooseMore(page, 'Manage workspaces');
  await expect(page.locator('#workspace-manager-title')).toBeVisible();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'list_directories')).toBe(true);
  const workspaceDirectory = (await commands(page)).find((command) => command.type === 'list_directories')!;
  await server(page, 0, {
    type: 'command_result',
    request_id: workspaceDirectory.request_id,
    action: 'list_directories',
    ok: true,
    phase: 'completed',
    data: {
      current: { path: '/work/mobile', label: 'mobile' },
      parent: '/work',
      directories: [],
    },
  });

  await page.getByRole('button', { name: 'Create Workspace' }).click();
  const createDialog = page.locator('#workspace-create-dialog');
  await expect(createDialog.getByRole('heading', { name: 'Create Workspace' })).toBeVisible();
  await createDialog.getByLabel('Label').fill('Phone Workspace');
  await createDialog.getByRole('button', { name: 'Confirm' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'workspace_create')).toMatchObject({
    cwd: '/work/mobile',
    label: 'Phone Workspace',
  });
  await expect(createDialog).toBeHidden();

  const phoneWorkspace = {
    workspace_id: 'w3', number: 3, label: 'Phone Workspace', pane_count: 1, tab_count: 1,
    cwd: '/work/mobile',
  };
  await server(page, 0, { type: 'workspaces', workspaces: [parent, shellOnly, phoneWorkspace] });
  const phoneCard = page.locator('.workspace-management-card').filter({ hasText: 'Phone Workspace' });
  await expect(phoneCard).toContainText('1 tab');
  await expect(phoneCard).toContainText('0 agents');

  const projectCard = page.locator('.workspace-management-card').first();
  await projectCard.getByRole('button', { name: 'Rename' }).click();
  await projectCard.getByLabel('Workspace label').fill('Renamed Project');
  await projectCard.getByRole('button', { name: 'Save' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'workspace_rename')).toMatchObject({
    workspace_id: 'w1',
    label: 'Renamed Project',
  });

  await projectCard.getByRole('button', { name: 'Worktrees' }).click();
  const worktreeDialog = page.locator('#worktree-manager-dialog');
  await expect(worktreeDialog.getByRole('heading', { name: 'Project Worktrees' })).toBeVisible();
  await expect(worktreeDialog.getByText('fix/one', { exact: true })).toBeVisible();
  await worktreeDialog.locator('.worktree-list').getByRole('button', { name: 'Open' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'worktree_open')).toMatchObject({
    workspace_id: 'w1',
    path: '/work/worktrees/project/fix-one',
  });

  await server(page, 0, { type: 'workspaces', workspaces: [parent, linked, shellOnly, phoneWorkspace] });
  await worktreeDialog.getByLabel('Branch').fill('fix/issue-14');
  await worktreeDialog.getByLabel('Base ref').fill('main');
  await worktreeDialog.getByRole('button', { name: 'Confirm' }).click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'worktree_create')).toMatchObject({
    workspace_id: 'w1',
    branch: 'fix/issue-14',
    base: 'main',
  });
  await worktreeDialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(worktreeDialog).toBeHidden();

  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.locator('.workspace-worktree-card')).toContainText('fix/one');
  await chooseMore(page, 'Manage workspaces');
  await expect(page.locator('#workspace-manager-title')).toBeVisible();

  const projectSlot = page.locator('.workspace-management-slot').first();
  await expect(projectSlot.locator('.workspace-management-card')).toHaveCount(2);
  await expect(projectSlot.locator('.nested-workspace')).toContainText('fix/one');
  const targetSlot = page.locator('.workspace-management-slot').nth(1);
  const sourceHeader = projectSlot.locator('.workspace-management-card').first().locator('header');
  const sourceBox = await sourceHeader.boundingBox();
  const targetBox = await targetSlot.boundingBox();
  expect(sourceBox).not.toBeNull();
  expect(targetBox).not.toBeNull();
  await page.mouse.move(sourceBox!.x + sourceBox!.width / 2, sourceBox!.y + sourceBox!.height / 2);
  await page.mouse.down();
  await page.clock.runFor(750);
  await expect(projectSlot).toHaveClass(/workspace-dragging/);
  await page.mouse.move(targetBox!.x + targetBox!.width / 2, targetBox!.y + targetBox!.height * .8, { steps: 4 });
  await page.mouse.up();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'workspace_reorder')).toMatchObject({
    workspace_ids: ['w1', 'w4'],
    before_workspace_id: 'w3',
  });

  await phoneCard.getByRole('button', { name: 'Start Agent' }).click();
  await expect(page.getByText('New tab in workspace Phone Workspace.')).toBeVisible();
  await page.getByLabel('Name').fill('phone-workspace-codex');
  await page.getByRole('button', { name: 'Start Agent' }).last().click();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'agent_start')).toMatchObject({
    workspace_id: 'w3',
    cwd: '/work/mobile',
  });
  await server(page, 0, {
    type: 'workspaces',
    workspaces: [parent, linked, shellOnly, { ...phoneWorkspace, pane_count: 2, tab_count: 2 }],
  });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w3:p2', workspace_id: 'w3', tab_id: 'w3:t2', tab_number: 2,
      tab_label: 'phone-workspace-codex', status: 'working', project: 'mobile', agent: 'codex',
      cwd: '/work/mobile',
    }],
  });
});

test('groups working agents and synchronizes tab order in both directions', async ({ page }) => {
  await page.clock.install();
  await connectFedora(page, {
    capabilities: ['tab_rename', 'agent_launch', 'attention_classification', 'tab_reorder'],
  });
  const agents = [
    {
      pane_id: 'w1:p1', workspace_id: 'w1', tab_id: 'w1:t1', tab_number: 1, tab_order: 1,
      tab_label: 'First', status: 'working', project: 'mobile', agent: 'codex',
    },
    {
      pane_id: 'w1:p2', workspace_id: 'w1', tab_id: 'w1:t2', tab_number: 2, tab_order: 2,
      tab_label: 'Second', status: 'working', project: 'mobile', agent: 'claude',
    },
  ];
  await server(page, 0, { type: 'agents', agents });

  const working = page.locator('section.workspace-section');
  await expect(working.getByText('mobile', { exact: true }).first()).toBeVisible();
  await expect(working.locator('summary strong')).toHaveCount(1);
  await expect(working.locator('.workspace-tab-header h3')).toHaveText(['First', 'Second']);

  // Long-press the first tab's agent card, then drag below the second tab.
  const source = working.locator('.workspace-tab').filter({ hasText: 'First' }).getByRole('button', { name: /^Open mobile/ });
  const target = working.locator('.workspace-tab').filter({ hasText: 'Second' });
  const sourceBox = await source.boundingBox();
  const targetBox = await target.boundingBox();
  expect(sourceBox).not.toBeNull();
  expect(targetBox).not.toBeNull();
  await page.mouse.move(sourceBox!.x + sourceBox!.width / 2, sourceBox!.y + sourceBox!.height / 2);
  await page.mouse.down();
  await page.clock.runFor(750);
  await expect(working.locator('.workspace-tab.tab-dragging')).toHaveCount(1);
  await page.mouse.move(targetBox!.x + targetBox!.width / 2, targetBox!.y + targetBox!.height * .8, { steps: 4 });
  await page.mouse.up();
  await expect.poll(async () => (await commands(page)).find((command) => command.type === 'tab_reorder')).toMatchObject({
    pane_id: 'w1:p1',
    insert_index: 2,
  });

  // Desktop-side move arrives as refreshed visual positions; numbers stay.
  await server(page, 0, {
    type: 'agents',
    agents: [
      { ...agents[0], tab_order: 2 },
      { ...agents[1], tab_order: 1 },
    ],
  });
  await expect(working.locator('.workspace-tab-header h3')).toHaveText(['Second', 'First']);

  // The long press must not have opened the agent terminal.
  await expect(page.locator('section.workspace-section')).toBeVisible();
});

test('defaults to the mixed workspace layout and separates state sections on demand', async ({ page }) => {
  await connectFedora(page, { capabilities: ['tab_rename', 'agent_launch', 'attention_classification'] });
  await server(page, 0, {
    type: 'agents',
    agents: [
      { pane_id: 'w1:p1', workspace_id: 'w1', status: 'done', project: 'alpha', agent: 'codex' },
      { pane_id: 'w1:p2', workspace_id: 'w1', status: 'idle', project: 'alpha', agent: 'claude' },
      { pane_id: 'w2:p1', workspace_id: 'w2', status: 'working', project: 'beta', agent: 'codex' },
      { pane_id: 'w2:p2', workspace_id: 'w2', status: 'idle', project: 'beta', agent: 'claude' },
      { pane_id: 'w4:p1', workspace_id: 'w4', status: 'idle', project: 'delta', agent: 'codex' },
      {
        pane_id: 'w3:p1', workspace_id: 'w3', status: 'blocked', attention_kind: 'approval',
        project: 'gamma', agent: 'codex', prompt: 'Approve the plan?', options: ['Yes', 'No'],
      },
    ],
  });

  // Default: one headingless mixed list, one card per workspace with a state
  // mark; blocked stays on top.
  await expect(page.locator('section.agent-section').first()).toContainText('Needs input');
  await expect(page.getByRole('region', { name: 'Workspaces' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Workspaces' })).toBeHidden();
  await expect(page.getByRole('heading', { name: 'Done' })).toBeHidden();
  await expect(page.getByRole('heading', { name: 'Working' })).toBeHidden();
  await expect(page.getByRole('heading', { name: 'Idle' })).toBeHidden();
  const card = (project: string) => page.locator('.workspace-card').filter({ hasText: project });
  await expect(card('alpha').locator('summary')).toHaveAccessibleName(/1 new result/);
  await expect(card('beta').getByRole('img', { name: 'Has a working session' })).toBeVisible();
  await expect(card('delta').getByRole('img', { name: 'Has an idle session' })).toBeVisible();
  await expect(card('delta').locator('.workspace-state-dot')).toHaveClass(/status-ring/);
  await expect(card('alpha').locator('.workspace-state-dot')).toHaveClass(/status-check/);
  await expect(page.locator('.workspace-card summary strong')).toHaveText(['alpha', 'beta', 'delta']);
  // Active workspaces start expanded; idle-only cards stay collapsed.
  await expect(card('alpha')).toHaveAttribute('open', '');
  await expect(card('beta')).toHaveAttribute('open', '');
  await expect(card('delta')).not.toHaveAttribute('open', '');

  // By State: sections separated by state, done first after the input queue.
  await page.getByRole('button', { name: /Settings/ }).click();
  await page.getByRole('button', { name: 'By State' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  const done = page.locator('.done-section');
  await expect(done.getByRole('heading', { name: 'Done 1 new result' })).toBeVisible();
  for (const theme of themes) {
    await useTheme(page, theme);
    await expectTextContrast(done.locator('.section-count'), `${theme}: Done count`);
  }
  await expect(done.locator('summary strong')).toHaveText(['alpha']);
  await expect(done.locator('.workspace-done-count')).toHaveText('1 new result');
  const stateWorking = page.locator('.working-section');
  await expect(stateWorking.locator('summary strong')).toHaveText(['beta']);
  await expect(page.getByRole('heading', { name: 'Idle' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Workspaces' })).toBeHidden();
  await expect(page.locator('section.agent-section').first()).toContainText('Needs input');

  // Back to the mixed default.
  await page.getByRole('button', { name: /Settings/ }).click();
  await page.getByRole('button', { name: 'Mixed' }).click();
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.getByRole('heading', { name: 'Done' })).toBeHidden();
  await expect(page.getByRole('region', { name: 'Workspaces' })).toBeVisible();
});

test('keeps activity cards inside the page and confirms permanent deletion', async ({ page }) => {
  await connectFedora(page);
  await server(page, 0, {
    type: 'activity_history',
    activities: [
      {
        id: 'activity-failure', timestamp: Date.now(), kind: 'action', summary: 'Could not send keys',
        status: 'failed',
      },
      {
        id: 'activity-working', timestamp: Date.now(), kind: 'working', summary: 'omp working',
        pane_id: 'w1:p1', project: 'herdr-mobile-relay', agent: 'omp', status: 'working',
      },
      {
        id: 'activity-1', timestamp: Date.now(), kind: 'finished', summary: 'codex completed',
        pane_id: 'w1:p1', project: 'herdr-mobile-relay', agent: 'codex', status: 'completed',
      },
    ],
  });

  await chooseMore(page, 'Activity history');
  const activity = page.getByRole('button', { name: /codex completed/ });
  await expect(page.getByRole('heading', { name: 'Last 24 hours' })).toBeVisible();
  await expect(page.locator('.activity-summary-metrics > div').filter({ hasText: 'Completed' })).toContainText('1');
  await expect(activity).toBeVisible();
  await expect(page.getByRole('button', { name: /omp working/ })).toHaveCount(0);
  const failure = page.getByRole('button', { name: /Could not send keys.*failed/ });
  await expect(failure.locator('.status-square')).toBeVisible();
  await failure.click();
  await expect(page.locator('.activity-detail-head .status-square')).toBeVisible();
  await expect(page.locator('.activity-detail-meta')).toHaveText('Fedora · failed');
  await page.getByRole('button', { name: 'Back' }).click();

  const headingBox = await page.getByRole('heading', { name: 'Activity', level: 2 }).boundingBox();
  const deleteBox = await page.getByRole('button', { name: 'Delete all' }).boundingBox();
  const box = await activity.boundingBox();
  const chevronBox = await activity.locator('.activity-chevron').boundingBox();
  const viewport = page.viewportSize();
  expect(headingBox).not.toBeNull();
  expect(deleteBox).not.toBeNull();
  expect(box).not.toBeNull();
  expect(chevronBox).not.toBeNull();
  expect(viewport).not.toBeNull();
  const headingCenter = headingBox!.y + headingBox!.height / 2;
  const deleteCenter = deleteBox!.y + deleteBox!.height / 2;
  expect(Math.abs(headingCenter - deleteCenter)).toBeLessThanOrEqual(2);
  expect(viewport!.width - (box!.x + box!.width)).toBeGreaterThanOrEqual(10);
  expect(box!.x + box!.width - (chevronBox!.x + chevronBox!.width)).toBeGreaterThanOrEqual(10);

  await page.getByRole('button', { name: 'Delete all' }).click();
  const dialog = page.getByRole('dialog', { name: 'Delete all activity?' });
  await expect(dialog).toContainText('Running agents and their conversations are not affected.');
  await dialog.getByRole('button', { name: 'Delete all' }).click();
  await expect(page.getByText('No activity yet.')).toBeVisible();
  expect((await commands(page)).find((command) => command.type === 'clear_activities'))
    .toMatchObject({ type: 'clear_activities', protocol: 3 });
});

test('sizes captured activity text with terminal typography', async ({ page }) => {
  await connectFedora(page);
  const fullResponse = Array.from({ length: 16 }, (_, index) => `Response line ${index + 1}`).join('\n');
  await server(page, 0, {
    type: 'activity_history',
    activities: [{
      id: 'activity-typography',
      kind: 'finished',
      timestamp: Date.now(),
      summary: 'omp completed',
      agent: 'omp',
      status: 'completed',
      extract: `\ue0b0 status\n${fullResponse}`,
    }],
  });

  await chooseMore(page, 'Activity history');
  await page.getByRole('button', { name: /omp completed/ }).click();
  const extract = page.getByRole('region', { name: 'Captured response' }).locator('pre');
  await expect(extract).toContainText('Response line 16');

  const typography = await extract.evaluate((element) => {
    const root = document.documentElement;
    const measure = (size: 'compact' | 'regular' | 'large') => {
      root.dataset.interfaceSize = size;
      return {
        root: Number.parseFloat(getComputedStyle(root).fontSize),
        extract: Number.parseFloat(getComputedStyle(element).fontSize),
      };
    };
    return {
      compact: measure('compact'),
      regular: measure('regular'),
      large: measure('large'),
      family: getComputedStyle(element).fontFamily,
    };
  });
  expect(typography.compact.extract).toBeCloseTo(typography.compact.root * .72, 2);
  expect(typography.regular.extract).toBeCloseTo(typography.regular.root * .82, 2);
  expect(typography.large.extract).toBeCloseTo(typography.large.root * .94, 2);
  expect(typography.family).toContain('Herdr Nerd Symbols');
});

test('sorts a cold idle snapshot by the latest Herdr activity', async ({ page }) => {
  await connectFedora(page);
  await server(page, 0, {
    type: 'agents',
    agents: [
      {
        pane_id: 'w1:p1',
        status: 'idle',
        project: 'herdr-mobile-relay',
        tab_label: 'codex_dummy',
        agent: 'codex',
        updated_at: 0,
        activity_seq: 735,
      },
      {
        pane_id: 'w1:p2',
        status: 'idle',
        project: 'herdr-mobile-relay',
        tab_label: 'codex_review_bugs',
        agent: 'codex',
        updated_at: 0,
        activity_seq: 794,
      },
    ],
  });

  await expect(page.locator('.workspace-card .workspace-tab h3').first()).toHaveText('codex_review_bugs');
});

test('keeps an opened workspace expanded across tab navigation and inventory refreshes', async ({ page }) => {
  await connectFedora(page);
  const agents = [
    {
      pane_id: 'w1:p1',
      workspace_id: 'workspace-mobile',
      status: 'idle',
      project: 'Mobile app',
      agent: 'codex',
      cwd: '/work/mobile',
    },
    {
      pane_id: 'w2:p1',
      workspace_id: 'workspace-docs',
      status: 'idle',
      project: 'Docs',
      agent: 'claude',
      cwd: '/work/docs',
    },
  ];
  await server(page, 0, { type: 'agents', agents });

  const workspace = page.locator('details.workspace-card').filter({ hasText: 'Mobile app' });
  await expect(workspace).toHaveCount(1);
  await workspace.locator('summary').click();
  await expect(workspace).toHaveAttribute('open', '');
  await workspace.getByRole('button', { name: 'Open Mobile app on Fedora' }).click();
  await expect(page.getByRole('combobox', { name: 'Prompt' })).toBeVisible();
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(workspace).toHaveAttribute('open', '');


  await server(page, 0, {
    type: 'agents',
    agents: [{ ...agents[0], activity_seq: 2 }, { ...agents[1] }],
  });

  await expect(workspace).toHaveAttribute('open', '');
  await expect(workspace.getByRole('button', { name: 'Open Mobile app on Fedora' })).toBeVisible();
});

test('shows inventory failure instead of zero agents and recovers without reconnecting', async ({ page }) => {
  await connectFedora(page, {
    inventory: {
      state: 'error',
      error_code: 'topology_churn',
      message: 'Agent inventory is changing too quickly to produce a stable snapshot.',
      last_attempt_at: 123,
      last_success_at: 0,
      stale: true,
    },
  });
  const staleAgents = [{ pane_id: 'w1:p1', status: 'working', project: 'Existing relay', agent: 'codex' }];
  await server(page, 0, { type: 'agents', agents: staleAgents });

  await expect(page.getByRole('status', { name: 'Fedora agent inventory unavailable' })).toContainText('changing too quickly');
  await expect(page.getByRole('button', { name: 'Open Existing relay on Fedora' })).toBeVisible();
  await expect(page.getByRole('img', { name: /agent inventory unavailable/ })).toHaveClass(/status-triangle/);

  await server(page, 0, {
    type: 'inventory_status',
    state: 'ready',
    error_code: '',
    message: '',
    last_attempt_at: 200,
    last_success_at: 200,
    stale: false,
  });
  await server(page, 0, { type: 'agents', agents: staleAgents });

  await expect(page.getByRole('status', { name: 'Fedora agent inventory unavailable' })).toBeHidden();
  await expect(page.getByRole('button', { name: 'Open Existing relay on Fedora' })).toBeVisible();
  expect(await socketCount(page)).toBe(1);
});

test('keeps a stale result when a controller tries to open it from jump', async ({ page }) => {
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await saveDeviceCredential(page, { id: 'credential-controller', deviceId: 'device-controller', secret: 'C'.repeat(43), role: 'controller' });
  await handshake(page, 0, { capabilities: [] });
  const { projectAgent } = createProjectInventory();
  await server(page, 0, { type: 'agents', agents: [{ ...projectAgent, name: 'fix-bug', status: 'succeeded' }] });
  const summary = page.locator('.workspace-card summary');
  await expect(summary).toHaveAccessibleName(/1 new result/);
  await server(page, 0, { type: 'inventory_status', state: 'error', stale: true });
  const row = page.getByRole('button', { name: 'Open fix-bug on Fedora' });
  await expect(row).toBeDisabled();

  await chooseMore(page, 'Search all agents');
  await page.getByRole('dialog', { name: 'Jump to agent' }).getByRole('button', { name: /New result.*fix-bug/ }).click();
  await expect(page.getByRole('main', { name: 'Agent inventory unavailable' })).toBeVisible();
  await page.getByRole('button', { name: 'Back to agents' }).click();
  await expect(summary).toHaveAccessibleName(/1 new result/);
  await expect(row).toHaveAccessibleDescription('New result');
  expect((await commands(page)).filter(({ type }) => type === 'acknowledge_pane')).toHaveLength(0);
});

test('shows the relay clock on Home without acknowledging a reader’s result', async ({ page }) => {
  const mounted = new Date('2026-08-12T12:00:00Z');
  await page.clock.setFixedTime(mounted);
  const mac = { ...fedora, id: 'mac', label: 'Mac', url: 'ws://127.0.0.1:8872' };
  await boot(page, [fedora, mac]);
  await expect.poll(() => socketCount(page)).toBe(2);
  await saveDeviceCredential(page, { id: 'credential-reader', deviceId: 'device-reader', secret: 'R'.repeat(43), role: 'reader' });
  await handshake(page, 0, { capabilities: [] });
  await handshake(page, 1, { capabilities: [] });
  const working = { pane_id: 'w1:p1', name: 'fix-bug', agent: 'codex', status: 'working' };
  const row = page.getByRole('button', { name: 'Open fix-bug on Fedora' });
  await server(page, 0, { type: 'agents', agents: [{ ...working, status_since_at: mounted.getTime() - 12 * 60_000 }] });
  await server(page, 1, { type: 'agents', agents: [working] });
  await expect(row).toHaveAccessibleDescription('Working · 12m');
  await expect(page.getByRole('button', { name: 'Open fix-bug on Mac' })).toHaveAccessibleDescription('Working');

  const now = mounted.getTime() + 10_000;
  await page.clock.setFixedTime(now);
  const done = { ...working, type: 'agent_update', status: 'succeeded' };
  const summary = page.locator('.workspace-card summary').filter({ hasText: '@Fedora' });
  const time = summary.locator('time');
  await server(page, 0, { ...done, last_active_at: now, status_since_at: now });
  await expect(row).toHaveAccessibleDescription('New result · just now');
  await expect(time).toHaveText('now');
  await expect(time).toHaveAttribute('datetime', new Date(now).toISOString());

  await row.click();
  await expect(page.getByRole('banner').getByText('fix-bug', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await expect(row).toHaveAccessibleDescription('New result · just now');
  expect((await commands(page)).filter(({ type }) => type === 'acknowledge_pane')).toHaveLength(0);

  await server(page, 0, { ...done, last_active_at: now + 10_000, status_since_at: now + 10_000 });
  await expect(row).toHaveAccessibleDescription('New result');
  await expect(time).toHaveCount(0);

  await server(page, 0, { ...done, last_active_at: now });
  await expect(time).toHaveText('now');
  await server(page, 0, { ...done, last_active_at: 1e16 });
  await expect(row).toHaveAccessibleDescription('New result');
  await expect(time).toHaveCount(0);
});

test('waits for fresh agent frames after inventory recovery or reconnect', async ({ page }) => {
  const now = Date.parse('2026-08-12T12:00:00Z');
  await page.clock.setFixedTime(now);
  await connectFedora(page);
  const result = {
    pane_id: 'w1:p1', workspace_id: 'w1', name: 'fix-bug', agent: 'codex', status: 'succeeded',
    pane_revision: 10, status_since_at: now - 120_000, last_active_at: now - 120_000,
  };
  const idle = {
    pane_id: 'w1:p2', workspace_id: 'w1', name: 'review-tests', agent: 'codex', status: 'idle',
    last_active_at: now - 300_000,
  };
  const resultRow = page.getByRole('button', { name: 'Open fix-bug on Fedora' });
  const idleRow = page.getByRole('button', { name: 'Open review-tests on Fedora' });
  await server(page, 0, { type: 'agents', agents: [result, idle] });
  await expect(resultRow).toHaveAccessibleDescription('New result · 2m ago');

  await server(page, 0, { type: 'inventory_status', state: 'error', stale: true });
  await expect(resultRow).toBeDisabled();
  await expect(resultRow).toHaveAccessibleDescription('New result');
  await expect(idleRow).toHaveAccessibleDescription('Idle · last active 5m ago');
  await server(page, 0, { type: 'agents', agents: [result, idle] });
  await server(page, 0, { type: 'inventory_status', state: 'ready' });
  await expect(resultRow).toBeEnabled();
  await expect(resultRow).toHaveAccessibleDescription('New result');
  await server(page, 0, { type: 'agent_update', ...result });
  await expect(resultRow).toHaveAccessibleDescription('New result · 2m ago');

  await closeSocket(page, 0);
  await expect(resultRow).toHaveAccessibleDescription('New result');
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 1, { inventory: { state: 'ready' } });
  await expect(resultRow).toBeEnabled();
  await expect(resultRow).toHaveAccessibleDescription('New result');
  await expect(idleRow).toHaveAccessibleDescription('Idle · last active 5m ago');

  await server(page, 1, { type: 'agents', agents: [
    { ...result, status: 'idle', pane_revision: 11, status_since_at: undefined },
    { ...idle, status: 'working', status_since_at: now - 60_000 },
  ] });
  await expect(resultRow).toHaveAccessibleDescription('Idle · last active 2m ago');
  await expect(idleRow).toHaveAccessibleDescription('Working · 1m');
});

test('counts new results across a closed workspace and its linked worktree', async ({ page }) => {
  const mac = { ...fedora, id: 'mac', label: 'Mac', url: 'ws://127.0.0.1:8872' };
  await boot(page, [fedora, mac]);
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 0, { capabilities: [] });
  await handshake(page, 1, { capabilities: [] });
  const { project, linked, projectAgent } = createProjectInventory();
  const first = { ...projectAgent, name: 'fix-bug', project: 'project', tab_label: 'main', tab_order: 1, status: 'working' };
  const second = {
    ...projectAgent, pane_id: 'w1:p2', tab_id: 'w1:t2', name: 'review-tests', project: 'project',
    tab_label: 'review', tab_order: 2, status: 'idle',
  };
  const child = {
    ...projectAgent, pane_id: 'w4:p1', tab_id: 'w4:t1', workspace_id: 'w4', cwd: linked.cwd,
    name: 'linked-checks', tab_label: 'fix', status: 'done',
  };
  await server(page, 0, { type: 'workspaces', workspaces: [project, linked] });
  await server(page, 0, { type: 'agents', agents: [first, second, child] });
  await server(page, 1, { type: 'agents', agents: [{ ...first, name: 'other-result', status: 'done' }] });
  await page.getByRole('button', { name: 'Mac', exact: true }).click();
  const workspace = page.locator('details.workspace-card').filter({
    has: page.locator('summary strong').filter({ hasText: /^Project$/ }),
  });
  const summary = workspace.locator('summary');
  const fix = page.getByRole('button', { name: 'Open fix-bug on Fedora' });
  const review = page.getByRole('button', { name: 'Open review-tests on Fedora' });
  const assertTabOrder = () => expect(workspace.locator('.workspace-tab h3')).toHaveText(['main', 'review', 'fix']);
  await expect(fix).toHaveAccessibleDescription('Working');
  await expect(review).toHaveAccessibleDescription('Idle');
  await expect(summary).toHaveAccessibleName(/1 new result\b/);
  await assertTabOrder();
  await server(page, 0, { type: 'agent_update', ...first, status: 'succeeded' });
  await expect(fix).toHaveAccessibleDescription('New result');
  await summary.click();
  await expect(workspace).not.toHaveAttribute('open');
  await expect(summary).toHaveAccessibleName(/2 new results.*1 worktree/);
  await summary.click();
  await assertTabOrder();
  await expect(fix).toHaveAccessibleDescription('New result');
  await expect(fix.locator('.status-check')).toBeVisible();
  await expect(review.locator('.status-ring')).toBeVisible();
  await expect(workspace.getByRole('button', { name: 'Open linked-checks on Fedora' }))
    .toHaveAccessibleDescription('New result');

  const layout = async (name: string) => {
    await page.getByRole('button', { name: 'Settings' }).click();
    await page.getByRole('button', { name, exact: true }).click();
    await page.getByRole('button', { name: 'Back' }).click();
  };
  await layout('By State');
  const doneHeading = page.locator('.done-section h2');
  await expect(doneHeading).toHaveAccessibleName('Done 2 new results');
  await expect(page.locator('.done-section summary')).toHaveAccessibleName(/2 new results/);
  await expect(page.getByRole('button', { name: /Open other-result/ })).toHaveCount(0);
  await fix.click();
  await expect(page.getByRole('main', { name: 'Terminal for project' })).toBeVisible();
  await expect.poll(async () => (await commands(page)).filter(({ type }) => type === 'acknowledge_pane'))
    .toMatchObject([{ pane_id: first.pane_id }]);
  await server(page, 0, { type: 'agent_update', ...first, status: 'idle' });
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(doneHeading).toHaveAccessibleName('Done 1 new result');
  await layout('Mixed');
  await expect(fix).toHaveAccessibleDescription('Idle');
  await page.getByRole('button', { name: 'Open linked-checks on Fedora' }).click();
  await expect.poll(async () => (await commands(page)).filter(({ type }) => type === 'acknowledge_pane'))
    .toMatchObject([{ pane_id: first.pane_id }, { pane_id: child.pane_id }]);
  await server(page, 0, { type: 'agent_update', ...child, status: 'idle' });
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(summary.locator('.workspace-done-count')).toHaveCount(0);
  await assertTabOrder();
  await layout('By State');
  await expect(doneHeading).toHaveCount(0);
});

test('a reader finds the exact pane path in jump and sends no acknowledgement from Activity', async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 720 });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await saveDeviceCredential(page, { id: 'credential-reader', deviceId: 'device-reader', secret: 'R'.repeat(43), role: 'reader' });
  await handshake(page, 0, { capabilities: [] });
  const { project, projectAgent } = createProjectInventory();
  const cwd = `/home/user/project/${'unbroken-path-segment'.repeat(4)}/a  b/test-fixtures`;
  const fix = { ...projectAgent, cwd, name: 'fix-bug', project: 'project', tab_label: 'main', status: 'succeeded' };
  await server(page, 0, { type: 'workspaces', workspaces: [project] });
  await server(page, 0, { type: 'agents', agents: [fix] });
  await expect(page.locator('.workspace-card summary .path-row')).toHaveText('/work/project');
  await chooseMore(page, 'Search all agents');
  const jump = page.getByRole('dialog', { name: 'Jump to agent' });
  const search = jump.getByRole('searchbox');
  await search.fill('fix-bug');
  const result = jump.getByRole('button', { name: /New result.*fix-bug/ });
  await result.focus();
  await expect(result).toBeFocused();
  await expect(result).toHaveAccessibleDescription(cwd.replace('  ', ' '));
  const path = jump.locator('.jump-pane-path');
  await expect(path).toBeVisible();
  expect(await path.evaluate((element) => ({
    fits: element.scrollWidth <= element.clientWidth,
    selection: getComputedStyle(element).userSelect,
    renderedPath: (element as HTMLElement).innerText,
  }))).toEqual({ fits: true, selection: 'text', renderedPath: cwd });
  await expect(page).toHaveURL(/\/$/);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(320);
  await page.keyboard.press('Escape');

  await server(page, 0, { type: 'activity_history', activities: [
    {
      id: 'result', kind: 'finished', status: 'completed', summary: 'Fixture checks completed',
      pane_id: fix.pane_id, timestamp: Date.now(), session: '/fixtures/.pi/agent/sessions/retained.jsonl',
    },
    {
      id: 'named', kind: 'finished', status: 'completed', summary: 'Pi completed',
      pane_id: fix.pane_id, timestamp: Date.now(), agent: 'pi', session: 'Repair truncated events.jsonl',
    },
  ] });
  await chooseMore(page, 'Activity history');
  const namedActivity = page.getByRole('button', { name: /Pi completed/ });
  await expect(namedActivity.getByText('Repair truncated events.jsonl')).toBeVisible();
  await namedActivity.click();
  await expect(page.getByRole('main', { name: 'Pi completed' }).getByText('Repair truncated events.jsonl')).toBeVisible();
  await page.getByRole('button', { name: 'Back', exact: true }).click();

  const activity = page.getByRole('button', { name: /Fixture checks completed/ });
  await expect(activity).not.toContainText('retained.jsonl');
  await activity.click();
  const detail = page.getByRole('main', { name: 'Fixture checks completed' });
  await expect(detail).not.toContainText('retained.jsonl');
  await detail.getByRole('button', { name: /Go to thread/ }).click();
  await expect(page.getByRole('main', { name: 'Terminal for project' })).toBeVisible();
  await expect(page.getByRole('banner').getByRole('img', { name: 'New result' })).toHaveClass(/status-check/);
  for (const url of [/#activity=/, /#activity$/, /\/$/]) {
    await page.getByRole('button', { name: 'Back' }).click();
    await expect(page).toHaveURL(url);
  }
  const row = page.getByRole('button', { name: 'Open fix-bug on Fedora' });
  await expect(row).toHaveAccessibleDescription('New result');
  await expect(page.locator('.workspace-card summary')).toHaveAccessibleName(/1 new result/);
  expect((await commands(page)).filter(({ type }) => type === 'acknowledge_pane')).toHaveLength(0);
});

async function markGeometry(mark: Locator) {
  return mark.evaluate((element) => {
    const { width, height } = element.getBoundingClientRect();
    const ink = getComputedStyle(element, '::before');
    return {
      width, height, clip: ink.clipPath, top: ink.top, border: ink.borderTopWidth,
      corner: ink.borderTopLeftRadius, animation: ink.animationName,
    };
  });
}

async function surfaceContrast(
  target: Locator, includeAncestors = false, paint: 'color' | '::placeholder' | 'outlineColor' | 'borderTopColor' | 'backgroundColor' = 'color',
) {
  return target.evaluate((element, { includeAncestors, paint }) => {
    const canvas = document.createElement('canvas');
    const context = canvas.getContext('2d')!;
    const fill = (color: string) => {
      context.fillStyle = color;
      context.fillRect(0, 0, 1, 1);
    };
    const luminance = () => {
      const [r, g, b, a] = context.getImageData(0, 0, 1, 1).data;
      const linear = [r, g, b].map((value) => value / 255)
        .map((channel) => channel <= .04045 ? channel / 12.92 : ((channel + .055) / 1.055) ** 2.4);
      return { light: .2126 * linear[0] + .7152 * linear[1] + .0722 * linear[2], a };
    };
    const backgrounds = [];
    let undimmed = true;
    const indicator = paint !== 'color' && paint !== '::placeholder';
    for (let surface = indicator ? element.parentElement : element; surface; surface = includeAncestors ? surface.parentElement : null) {
      const style = getComputedStyle(surface);
      backgrounds.push(style.backgroundColor);
      undimmed &&= style.opacity === '1';
    }
    backgrounds.reverse().forEach(fill);
    const backing = luminance();
    const pseudo = paint === '::placeholder' ? paint : null;
    const inkStyle = getComputedStyle(element, pseudo);
    if (pseudo) context.globalAlpha = Number(inkStyle.opacity);
    fill(inkStyle[indicator ? paint : 'color']);
    const ink = luminance();
    const [dark, light] = [ink.light, backing.light].sort((a, b) => a - b);
    return { contrast: (light + .05) / (dark + .05), opaque: backing.a === 255, undimmed };
  }, { includeAncestors, paint });
}

async function expectTextContrast(text: Locator, label: string, pseudo?: '::placeholder') {
  await expect(text).toBeVisible();
  const { contrast, opaque, undimmed } = await surfaceContrast(text, true, pseudo);
  expect({ opaque, undimmed }, `${label} is undimmed on an opaque backing`).toEqual({ opaque: true, undimmed: true });
  expect.soft(contrast, `${label} text contrast`).toBeGreaterThanOrEqual(4.5);
}

test('keeps the new Terminal, Conversation and Settings labels legible in six themes', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.addInitScript(() => {
    localStorage.setItem('herdr_default_agent_view', 'terminal');
    localStorage.setItem('herdr-conversation-view', 'conversation');
  });
  await connectFedora(page, { capabilities: ['agent_launch', 'attention_classification', 'conversation_history'] });
  await server(page, 0, { type: 'agents', agents: [
    { pane_id: 'w1:p1', project: 'Reading', agent: 'codex', status: 'working', conversation_history_available: true },
    { pane_id: 'w1:p2', project: 'Waiting', agent: 'codex', status: 'blocked', attention_kind: 'approval' },
  ] });
  await page.getByRole('button', { name: 'Open Reading on Fedora' }).click();
  await expect(moreTrigger(page)).toContainText('Next blocked');

  const badge = page.getByRole('banner').locator('.nav-update-badge');
  await expect(badge).toBeVisible();
  await openMore(page);
  const more = page.getByRole('dialog', { name: 'More', exact: true });
  await expect(more.getByRole('heading', { name: 'More', exact: true })).toHaveCSS('clip', 'rect(0px, 0px, 0px, 0px)');
  await expect(more.getByRole('button', { name: 'Next blocked: Waiting on Fedora' })).toBeVisible();
  for (const theme of themes) {
    await useTheme(page, theme);
    const { contrast } = await surfaceContrast(badge, true, 'backgroundColor');
    expect.soft(contrast, `${theme}: update badge against header`).toBeGreaterThanOrEqual(3);
    for (const button of await more.getByRole('button').all()) {
      await expectTextContrast(button, `${theme}: ${await button.textContent()}`);
    }
  }
  await page.keyboard.press('Escape');

  await page.getByRole('button', { name: 'Attach', exact: true }).press('Enter');
  const attach = page.getByRole('dialog', { name: 'Attach', exact: true });
  await expect(attach.getByRole('heading', { name: 'Attach', exact: true })).toHaveCSS('clip', 'rect(0px, 0px, 0px, 0px)');
  for (const theme of themes) {
    await useTheme(page, theme);
    for (const name of ['Attach photos', 'Attach files', 'Close']) {
      await expectTextContrast(attach.getByRole('button', { name, exact: true }), `${theme}: ${name}`);
    }
  }
  await page.keyboard.press('Escape');

  const header = page.getByRole('banner');
  await header.getByRole('button', { name: 'Conversation history' }).click();
  await expect(page.getByText('latest retained question')).toBeVisible();
  for (const mode of ['Conversation', 'Full history']) {
    await chooseMore(page, mode);
    const subtitle = header.getByText(new RegExp(`^${mode} ·`));
    for (const theme of themes) {
      await useTheme(page, theme);
      await expectTextContrast(subtitle, `${theme}: ${mode} subtitle`);
    }
  }
  await openMore(page);
  const searchAction = more.getByRole('button', { name: 'Search loaded conversation', exact: true });
  await expect(searchAction).toBeVisible();
  const selectedMode = more.getByRole('button', { name: 'Full history', exact: true, pressed: true });
  for (const theme of themes) {
    await useTheme(page, theme);
    const { contrast } = await surfaceContrast(selectedMode, true, 'borderTopColor');
    expect.soft(contrast, `${theme}: selected mode border against dialog`).toBeGreaterThanOrEqual(3);
    for (const button of await more.getByRole('button').all()) {
      await expectTextContrast(button, `${theme}: ${await button.textContent()}`);
    }
  }
  await searchAction.click();
  const search = page.getByRole('searchbox', { name: 'Search loaded conversation' });
  await expect(search).toHaveAttribute('placeholder', 'Search loaded conversation');
  const close = page.getByRole('button', { name: 'Close conversation search' });
  for (const theme of themes) {
    await useTheme(page, theme);
    await expectTextContrast(search, `${theme}: conversation search placeholder`, '::placeholder');
    await expectTextContrast(close, `${theme}: Close conversation search`);
  }
  await close.click();

  await chooseMore(page, /^Settings,/);
  const settingsSearch = page.getByRole('searchbox', { name: 'Search settings' });
  const searchLabel = page.getByText('Search settings', { exact: true });
  const notice = page.getByText(/Update notice/);
  for (const theme of themes) {
    await useTheme(page, theme);
    await expectTextContrast(searchLabel, `${theme}: Search settings`);
    await expectTextContrast(notice, `${theme}: Update notice`);
  }
  await settingsSearch.fill('no matching setting');
  for (const theme of themes) {
    await useTheme(page, theme);
    await expectTextContrast(page.getByText('No settings match.', { exact: true }), `${theme}: No settings match`);
  }
});

test('keeps a truncated terminal hyperlink out of Home and its accessible description', async ({ page }) => {
  await connectFedora(page, { capabilities: ['attention_classification'] });
  await server(page, 0, { type: 'agents', agents: [{
    pane_id: 'w1:p1', name: 'approve', agent: 'codex', status: 'blocked', attention_kind: 'approval',
    command: 'view \x1b]8;;https://example.test/?token=sample',
  }] });
  const row = page.getByRole('button', { name: 'Open approve on Fedora' });
  await expect(row.locator('.prompt-preview')).toHaveText('view');
  await expect(row).toHaveAccessibleDescription('Needs input view');
});

test('keeps the compact Pi logo clear of the status mark at 390px', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await connectFedora(page);
  await useTheme(page, 'dark');
  await server(page, 0, { type: 'agents', agents: [{
    pane_id: 'w1:p1', workspace_id: 'w1', name: 'Fixture checks', project: 'Project', agent: 'pi', status: 'succeeded',
  }] });
  const row = page.locator('.compact-agent-card').getByRole('button', { name: 'Open Fixture checks on Fedora' });
  await expect(row).toHaveAccessibleDescription('New result');
  const logo = row.getByRole('img', { name: 'Pi' });
  const mark = row.locator('.agent-activity .status-check');
  const text = row.getByText('New result', { exact: true });
  await expect(logo).toBeVisible();
  await expect(mark).toBeVisible();
  const [logoBox, markBox, textBox] = await Promise.all([logo.boundingBox(), mark.boundingBox(), text.boundingBox()]);
  expect(markBox!.x).toBeGreaterThan(logoBox!.x + logoBox!.width);
  expect(textBox!.x).toBeGreaterThan(markBox!.x + markBox!.width);
  expect(markBox!.y + markBox!.height / 2).toBeCloseTo(textBox!.y + textBox!.height / 2, 0);
});

test('hides a Pi session file path from agent rows but keeps it searchable', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await connectFedora(page);
  const filename = '2026-11-24T13-11-24-871Z_fixture.jsonl';
  await server(page, 0, { type: 'agents', agents: [{
    pane_id: 'w1:p1', workspace_id: 'w1', name: 'Fixture checks', project: 'Project', agent: 'pi', status: 'working',
    session: `/fixtures/.pi/agent/sessions/--work-project--/${filename}`,
  }] });
  const home = page.getByRole('button', { name: 'Open Fixture checks on Fedora' });
  await expect(home).toBeVisible();
  await expect(home).not.toContainText(filename);

  await chooseMore(page, 'Search all agents');
  const jump = page.getByRole('dialog', { name: 'Jump to agent' });
  await jump.getByRole('searchbox').fill(filename);
  const result = jump.getByRole('button', { name: /Working.*Fixture checks/ });
  await expect(result).toBeVisible();
  await expect(result).not.toContainText(filename);
  await result.click();

  await page.setViewportSize({ width: 1024, height: 768 });
  const rail = page.getByRole('complementary', { name: 'Agent navigation' });
  const active = rail.getByRole('button', { name: /Working.*Project/ });
  await expect(active).toBeVisible();
  await expect(active).not.toContainText(filename);
});

test('keeps static 12px marks, activity and counts legible in six themes at 320px and in the rail', async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 820 });
  await connectFedora(page, { capabilities: ['attention_classification'] });
  const states = [
    ['approve', 'blocked', 'approval', 'Needs input', 'square'],
    ['inspect', 'blocked', 'unknown', 'Needs inspection', 'triangle'],
    ['work', 'working', undefined, 'Working', 'chevron'],
    ['result', 'succeeded', undefined, 'New result', 'check'],
    ['idle', 'idle', undefined, 'Idle', 'ring'],
    ['unknown', 'exited', undefined, 'Status unknown', 'dash'],
  ] as const;
  const command = 'printf "<a>&|"; ' + 'long-unbroken-command-argument'.repeat(20);
  const age = Date.now() - 330_000;
  const cwd = '/home/test/' + 'a-long-workspace-name-'.repeat(10);
  await server(page, 0, { type: 'agents', agents: states.map(([name, status, attention_kind], index) => ({
    pane_id: `w1:p${index + 1}`, workspace_id: 'w1', agent: 'codex', project: 'Status sample',
    name, cwd, status, attention_kind, ...(name === 'approve' ? { command } : {}),
    status_since_at: name === 'work' ? age : undefined,
  })) });
  const row = (name: string) => page.getByRole('button', { name: `Open ${name} on Fedora`, exact: true });
  const resultCount = page.locator('.workspace-done-count');
  await expect(resultCount).toHaveText('1 new result');
  await expect(row('work')).toHaveAccessibleDescription('Working · 5m');
  const preview = row('approve').locator('.prompt-preview');
  await expect(row('approve')).toHaveAccessibleDescription(/^Needs input printf "<a>&\|"; .*…$/);
  expect([...(await preview.textContent())!]).toHaveLength(400);
  for (const theme of themes) {
    await useTheme(page, theme);
    await expectTextContrast(row('result').getByText('New result', { exact: true }), `${theme}: New result`);
    await expectTextContrast(row('idle').getByText('Idle', { exact: true }), `${theme}: Idle`);
    await expectTextContrast(resultCount, `${theme}: workspace count`);
  }
  await page.emulateMedia({ reducedMotion: 'reduce' });
  const geometries = [];
  for (const theme of themes) {
    await useTheme(page, theme);
    const current = [];
    for (const [name, , , label, shape] of states) {
      const mark = row(name).locator(`.agent-activity .status-${shape}`);
      await expect(row(name).locator('.agent-activity')).toContainText(label);
      const geometry = await markGeometry(mark);
      expect(geometry).toMatchObject({ width: 12, height: 12, animation: 'none' });
      const contrast = await surfaceContrast(mark, true);
      expect(contrast.opaque).toBe(true);
      expect(contrast.contrast).toBeGreaterThanOrEqual(3);
      current.push(geometry);
    }
    geometries.push(current);
    for (const text of [resultCount, row('work').locator('.agent-activity'), preview]) {
      expect(await text.evaluate((element) => {
        const rect = element.getBoundingClientRect();
        const card = element.closest('article, details')!.getBoundingClientRect();
        return rect.left >= card.left && rect.right <= card.right && element.scrollWidth <= element.clientWidth;
      })).toBe(true);
    }
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBe(320);
  }
  expect(new Set(geometries[0].map((shape) => JSON.stringify(shape))).size).toBe(6);
  for (const theme of geometries) expect(theme).toEqual(geometries[0]);
  expect(geometries[0][0]).toMatchObject({ top: '1px', clip: 'none' });
  expect(geometries[0][1].clip).toContain('polygon(50%');
  expect(geometries[0][2].clip).toContain('80% 50%');
  expect(geometries[0][3].clip).toContain('40% 63%');
  expect(geometries[0][4]).toMatchObject({ border: '2px', corner: '50%' });
  expect(geometries[0][5]).toMatchObject({ top: '5px', clip: 'none' });

  await chooseMore(page, 'Search all agents');
  const jump = page.getByRole('dialog', { name: 'Jump to agent' });
  await expect(jump.getByRole('button', { name: /New result.*result/ }).locator('.status-check')).toBeVisible();
  await expect(jump.getByRole('button', { name: /Idle.*idle/ }).locator('.status-ring')).toBeVisible();
  await page.keyboard.press('Escape');

  await row('work').click();
  const rail = page.getByRole('complementary', { name: 'Agent navigation' });
  await expect(rail).toBeHidden();
  for (const [theme, index] of themes.map((theme, index) => [theme, index] as const)) {
    await page.setViewportSize({ width: index % 2 ? 1024 : 900, height: 820 });
    await useTheme(page, theme);
    await expect(rail).toBeVisible();
    await expectTextContrast(moreTrigger(page).getByText('Next blocked', { exact: true }), `${theme}: Next blocked`);
    const active = rail.getByRole('button', { name: /Working.*work/ });
    await expect(active).toHaveAttribute('aria-current', 'page');
    const selectedMark = active.locator('.status-chevron');
    expect(await markGeometry(selectedMark)).toEqual(geometries[0][2]);
    const { contrast, opaque } = await surfaceContrast(selectedMark, true);
    expect(opaque).toBe(true);
    expect(contrast).toBeGreaterThanOrEqual(3);
    await expect(rail.getByRole('button', { name: /New result.*result/ }).locator('.status-check')).toBeVisible();
    await expect(rail.getByRole('button', { name: /Idle.*idle/ }).locator('.status-ring')).toBeVisible();
    await expect(rail.locator('.agent-activity')).toHaveCount(0);
  }
});

test('uses a workspace agent rail in a tablet terminal', async ({ page }) => {
  await page.setViewportSize({ width: 1024, height: 768 });
  await connectFedora(page);
  await server(page, 0, {
    type: 'agents',
    agents: [
      {
        pane_id: 'w1:p1',
        workspace_id: 'workspace-mobile',
        status: 'working',
        project: 'Mobile workspace',
        agent: 'codex',
        cwd: '/work/mobile',
      },
      {
        pane_id: 'w2:p1',
        workspace_id: 'workspace-api',
        status: 'idle',
        project: 'API workspace',
        agent: 'claude',
        cwd: '/work/api',
      },
    ],
  });

  await chooseMore(page, 'Search all agents');
  const jump = page.getByRole('dialog', { name: 'Jump to agent' });
  await jump.getByRole('searchbox', { name: 'Search agents and workspaces' }).fill('Mobile workspace');
  await jump.getByRole('button', { name: /Mobile workspace/ }).click();

  const rail = page.getByRole('complementary', { name: 'Agent navigation' });
  const terminal = page.getByRole('main', { name: 'Terminal for Mobile workspace' });
  await expect(rail).toBeVisible();
  await expect(terminal).toBeVisible();
  const [railBox, terminalBox] = await Promise.all([rail.boundingBox(), terminal.boundingBox()]);
  expect(railBox).not.toBeNull();
  expect(terminalBox).not.toBeNull();
  expect(railBox!.x + railBox!.width).toBeLessThanOrEqual(terminalBox!.x + 1);
  expect(terminalBox!.width).toBeGreaterThan(600);

  await rail.getByRole('button', { name: /API workspace/ }).click();
  await expect(page.getByRole('main', { name: 'Terminal for API workspace' })).toBeVisible();
});

test('resets the home page scroll offset before opening a terminal', async ({ page }) => {
  await connectFedora(page);
  await server(page, 0, {
    type: 'agents',
    agents: Array.from({ length: 20 }, (_, index) => ({
      pane_id: `w1:p${index + 1}`,
      status: 'working',
      project: `Scrollable agent ${index + 1}`,
      agent: 'codex',
    })),
  });

  // Working workspace cards render expanded, so the last agent is reachable
  // without toggling its card.
  const lastAgent = page.getByRole('button', { name: 'Open Scrollable agent 20 on Fedora' });
  await expect(lastAgent).toBeVisible();
  await page.evaluate(() => {
    document.documentElement.style.minHeight = '300vh';
    window.scrollTo(0, 100);
  });
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBeGreaterThan(0);
  await page.evaluate(() => { window.scrollTo = () => {}; });
  await lastAgent.click();

  const terminal = page.getByRole('main', { name: 'Terminal for Scrollable agent 20' });
  await expect(terminal).toBeVisible();
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBeGreaterThan(0);
  await expect.poll(async () => terminal.evaluate((element) => {
    const bounds = element.getBoundingClientRect();
    return Math.round(window.innerHeight - bounds.bottom);
  })).toBe(0);
});
