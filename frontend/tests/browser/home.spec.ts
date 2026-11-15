import { expect, test } from '@playwright/test';
import { boot, commands, fedora, handshake, server, socketCount } from '../support/journey-relay';

test('manages workspace modals, grouped worktrees, and drag ordering', async ({ page }) => {
  await page.clock.install();
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
    capabilities: ['tab_rename', 'agent_launch', 'attention_classification', 'directory_browser', 'workspace_management', 'workspace_reorder_block', 'worktree_management'],
  });
  const parent = {
    workspace_id: 'w1', number: 1, label: 'Project', pane_count: 1, tab_count: 1,
    cwd: '/work/project',
    worktree: {
      repo_key: 'repo', repo_name: 'project', repo_root: '/work/project',
      checkout_path: '/work/project', is_linked_worktree: false,
    },
  };
  const shellOnly = {
    workspace_id: 'w2', number: 2, label: 'Shell Only', pane_count: 1, tab_count: 1,
    cwd: '/home/test/shell-only',
  };
  await server(page, 0, { type: 'workspaces', workspaces: [parent, shellOnly] });
  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p1', workspace_id: 'w1', tab_id: 'w1:t1', tab_number: 1, cwd: '/work/project',
      tab_label: 'Agent', status: 'working', project: 'fallback-project', agent: 'codex',
    }],
  });

  await expect(page.getByText('Project', { exact: true }).first()).toBeVisible();
  await expect(page.getByText('Shell Only', { exact: true }).first()).toBeVisible();
  // Idle-only cards start collapsed in the mixed default; a tap expands them.
  await expect(page.getByText('No agents are running in this workspace.')).toBeHidden();
  await page.locator('.workspace-card').filter({ hasText: 'Shell Only' }).locator('summary').click();
  await expect(page.getByText('No agents are running in this workspace.')).toBeVisible();
  // The computer is named on the workspace card, once, and every row under it
  // spends its width on the directory instead of repeating that name.
  const projectWorkspace = page.locator('.workspace-card').filter({ hasText: 'Project' }).first();
  await expect(projectWorkspace.locator('summary .workspace-card-title .host-badge')).toHaveText('@Fedora');
  await expect(projectWorkspace.locator('summary .path-row')).toHaveText('/work/project');
  await expect(projectWorkspace.locator('.compact-agent-card .agent-path')).toHaveText('/work/project');
  await expect(projectWorkspace.locator('.compact-agent-card .host-badge')).toHaveCount(0);
  await expect(page.locator('.workspace-card').filter({ hasText: 'Shell Only' }).locator('summary .path-row'))
    .toHaveText('~/shell-only');

  await page.getByRole('button', { name: 'Manage workspaces' }).click();
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

  const linked = {
    workspace_id: 'w4', number: 4, label: 'fix/one', pane_count: 1, tab_count: 1,
    cwd: '/work/worktrees/project/fix-one',
    worktree: {
      repo_key: 'repo', repo_name: 'project', repo_root: '/work/project',
      checkout_path: '/work/worktrees/project/fix-one', is_linked_worktree: true,
    },
  };
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
  await page.getByRole('button', { name: 'Manage workspaces' }).click();
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
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
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
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, { capabilities: ['tab_rename', 'agent_launch', 'attention_classification'] });
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
  // dot, blocked stays on top.
  await expect(page.locator('section.agent-section').first()).toContainText('Needs input');
  await expect(page.getByRole('region', { name: 'Workspaces' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Workspaces' })).toBeHidden();
  await expect(page.getByRole('heading', { name: 'Done' })).toBeHidden();
  await expect(page.getByRole('heading', { name: 'Working' })).toBeHidden();
  await expect(page.getByRole('heading', { name: 'Idle' })).toBeHidden();
  const card = (project: string) => page.locator('.workspace-card').filter({ hasText: project });
  await expect(card('alpha').getByRole('img', { name: 'Has a done session' })).toBeVisible();
  await expect(card('beta').getByRole('img', { name: 'Has a working session' })).toBeVisible();
  await expect(card('delta').getByRole('img', { name: 'All sessions idle and ready for input' })).toBeVisible();
  // Idle-and-ready reads as a green ring, distinct from the filled unread-done
  // dot and from the grey used only for unknown states.
  await expect(card('delta').locator('.workspace-state-dot')).toHaveClass(/status-ready/);
  await expect(card('alpha').locator('.workspace-state-dot')).toHaveClass(/status-success/);
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
  await expect(done.getByRole('heading', { name: 'Done' })).toBeVisible();
  await expect(done.locator('summary strong')).toHaveText(['alpha']);
  await expect(done.locator('.workspace-done-count')).toHaveText('1 done');
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
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
  await server(page, 0, {
    type: 'activity_history',
    activities: [
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

  await page.getByRole('button', { name: 'Activity history' }).click();
  const activity = page.getByRole('button', { name: /codex completed/ });
  await expect(page.getByRole('heading', { name: 'Last 24 hours' })).toBeVisible();
  await expect(page.locator('.activity-summary-metrics > div').filter({ hasText: 'Completed' })).toContainText('1');
  await expect(activity).toBeVisible();
  await expect(page.getByRole('button', { name: /omp working/ })).toHaveCount(0);
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
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
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

  await page.getByRole('button', { name: 'Activity history' }).click();
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
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
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
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
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
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0, {
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
  await expect(page.getByRole('img', { name: /agent inventory unavailable/ })).toBeVisible();

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

test('uses a workspace agent rail in a tablet terminal', async ({ page }) => {
  await page.setViewportSize({ width: 1024, height: 768 });
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
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

  await page.getByRole('button', { name: 'Search all agents' }).click();
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
  await boot(page, [fedora]);
  await expect.poll(() => socketCount(page)).toBe(1);
  await handshake(page, 0);
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
