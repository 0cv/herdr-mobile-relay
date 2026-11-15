import { expect, test } from '@playwright/test';
import { boot, commands, commandsForSocket, connectFedora, fedora, handshake, server, setAutoCommands, socketCount } from '../support/journey-relay';

test('hides backend-dependent launch and tab controls', async ({ page }) => {
  await connectFedora(page, { capabilities: [] });
  await server(page, 0, {
    type: 'agents',
    agents: [{ pane_id: 'w1:p2', status: 'working', project: 'relay', agent: 'codex' }],
  });
  await expect(page.getByRole('button', { name: 'Start agent', exact: true })).toHaveCount(0);
  await page.getByRole('button', { name: 'Open relay on Fedora', exact: true }).click();
  await page.getByRole('button', { name: 'Manage agent' }).click();
  const dialog = page.getByRole('dialog', { name: 'Manage Agent' });
  await expect(dialog.getByRole('button', { name: 'Rename Tab' })).toHaveCount(0);
  await expect(dialog.getByRole('button', { name: /Clear|Restart/ })).toHaveCount(0);
  await expect(dialog.getByRole('button', { name: 'Stop Agent' })).toBeVisible();
  await expect(dialog.getByRole('button', { name: 'Rename Session' })).toBeVisible();
});

test('ignores a directory result after switching computers', async ({ page }) => {
  const mac = { id: 'mac', label: 'Mac', url: 'ws://127.0.0.1:8872', token: '' };
  await boot(page, [fedora, mac]);
  await expect.poll(() => socketCount(page)).toBe(2);
  await handshake(page, 0);
  await handshake(page, 1);
  await page.getByRole('button', { name: 'Start agent' }).click();
  await expect.poll(async () => (await commandsForSocket(page, 0)).filter((command) => command.type === 'list_directories').length).toBe(1);
  const fedoraDirectory = (await commandsForSocket(page, 0)).find((command) => command.type === 'list_directories')!;

  await page.getByLabel('Computer').selectOption('mac');
  await expect.poll(async () => (await commandsForSocket(page, 1)).filter((command) => command.type === 'list_directories').length).toBe(1);
  await page.getByLabel('Agent', { exact: true }).selectOption('codex');
  const macDirectory = (await commandsForSocket(page, 1)).find((command) => command.type === 'list_directories')!;
  await server(page, 1, {
    type: 'command_result', request_id: macDirectory.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/Users/test/mac-project', label: '~/mac-project' },
      parent: '/Users/test', directories: [],
    },
  });
  await expect(page.getByRole('button', { name: '~/mac-project' })).toBeVisible();

  await server(page, 0, {
    type: 'command_result', request_id: fedoraDirectory.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/home/test/fedora-project', label: '~/fedora-project' },
      parent: '/home/test', directories: [],
    },
  });
  await expect(page.getByRole('button', { name: '~/mac-project' })).toBeVisible();
  await expect(page.getByLabel('Name')).toHaveValue('mac-project-codex');

  await page.getByRole('button', { name: 'Start Agent', exact: true }).click();
  await expect.poll(async () => (await commandsForSocket(page, 1)).some((command) => command.type === 'agent_start')).toBe(true);
  expect((await commandsForSocket(page, 1)).find((command) => command.type === 'agent_start')).toMatchObject({
    cwd: '/Users/test/mac-project', name: 'mac-project-codex', profile_id: 'codex',
  });
  expect((await commandsForSocket(page, 0)).filter((command) => command.type === 'agent_start')).toHaveLength(0);
});

test('waits for a dotted directory selection before launching', async ({ page }) => {
  await connectFedora(page, {
    agent_profiles: [{ id: 'claude', label: 'Claude Code' }],
  });
  await page.getByRole('button', { name: 'Start agent' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'list_directories').length).toBe(1);
  const initialDirectory = (await commands(page)).find((command) => command.type === 'list_directories')!;
  await server(page, 0, {
    type: 'command_result', request_id: initialDirectory.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/home/cv/Development', label: '~/Development' },
      parent: '/home/cv',
      directories: [
        { name: 'test.com', path: '/home/cv/Development/test.com' },
        { name: 'testcom', path: '/home/cv/Development/testcom' },
      ],
    },
  });

  await page.getByRole('button', { name: '~/Development' }).click();
  await page.getByRole('button', { name: /test\.com/ }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'list_directories').length).toBe(2);
  const dottedDirectory = (await commands(page)).filter((command) => command.type === 'list_directories').at(-1)!;
  expect(dottedDirectory).toMatchObject({ path: '/home/cv/Development/test.com' });
  const startAgent = page.getByRole('button', { name: 'Start Agent', exact: true });
  await expect(startAgent).toBeDisabled();

  await server(page, 0, {
    type: 'command_result', request_id: dottedDirectory.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/home/cv/Development/test.com', label: '~/Development/test.com' },
      parent: '/home/cv/Development',
      directories: [],
    },
  });
  await expect(page.getByRole('button', { name: '~/Development/test.com' })).toBeVisible();
  await expect(page.getByLabel('Name')).toHaveValue('test-com-claude');
  await expect(startAgent).toBeEnabled();
  expect(await page.getByLabel('Name').evaluate((input: HTMLInputElement) => input.checkValidity())).toBe(true);

  await startAgent.click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'agent_start')).toBe(true);
  expect((await commands(page)).find((command) => command.type === 'agent_start')).toMatchObject({
    profile_id: 'claude', cwd: '/home/cv/Development/test.com', name: 'test-com-claude',
  });
});

test('keeps a newly launched agent open when its native session is discovered', async ({ page }) => {
  await connectFedora(page, { agent_profiles: [{ id: 'pi', label: 'Pi' }] });
  await page.getByRole('button', { name: 'Start agent' }).click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'list_directories')).toBe(true);
  const listing = (await commands(page)).find((command) => command.type === 'list_directories')!;
  await server(page, 0, {
    type: 'command_result', request_id: listing.request_id, ok: true, phase: 'completed',
    data: { current: { path: '/home/test/project', label: '~/project' }, parent: '/home/test', directories: [] },
  });
  await setAutoCommands(page, false);
  await page.getByRole('button', { name: 'Start Agent', exact: true }).click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'agent_start')).toBe(true);
  const launch = (await commands(page)).find((command) => command.type === 'agent_start')!;
  const launched = {
    pane_id: 'w1:p2', terminal_id: 'terminal-new', generation: 1, agent_session_id: '',
    status: 'idle', project: 'project', cwd: '/home/test/project', name: 'project-pi', agent: 'pi',
  };
  await server(page, 0, { type: 'agents', agents: [launched] });
  await server(page, 0, {
    type: 'command_result', request_id: launch.request_id, ok: true, phase: 'completed',
    data: { pane_id: launched.pane_id, name: launched.name, cwd: launched.cwd },
  });
  await page.getByRole('button', { name: 'Open project on Fedora', exact: true }).click();
  await expect(page.getByRole('main', { name: 'Terminal for project' })).toBeVisible();
  await server(page, 0, { type: 'agents', agents: [{ ...launched, generation: 2, agent_session_id: 'native-session' }] });
  await expect(page.getByRole('main', { name: 'Terminal for project' })).toBeVisible();
  await expect.poll(async () => (await commands(page)).findLast((command) => command.type === 'read_pane')?.target)
    .toMatchObject({ terminal_id: 'terminal-new', generation: 2, agent_session_id: 'native-session' });
  await expect(page.getByText('This agent is not available yet.')).toBeHidden();
});

test('launches and manages agent lifecycle commands', async ({ page }) => {
  await connectFedora(page, {
    agent_profiles: [
      { id: 'qodercli', label: 'Qoder' },
      { id: 'opencode', label: 'OpenCode' },
      { id: 'codex', label: 'Codex' },
      { id: 'pi', label: 'Pi' },
      { id: 'kimi', label: 'Kimi' },
      { id: 'claude', label: 'Claude Code' },
      { id: 'omp', label: 'Oh My Pi' },
    ],
  });
  await page.getByRole('button', { name: 'Start agent' }).click();
  const agentType = page.getByLabel('Agent', { exact: true });
  await expect(agentType.locator('option')).toHaveText([
    'Claude Code', 'Codex', 'Kimi', 'Oh My Pi', 'OpenCode', 'Pi', 'Qoder',
  ]);
  await expect(agentType).toHaveValue('claude');
  await agentType.selectOption('codex');
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'list_directories')).toBe(true);
  const directoryCommand = (await commands(page)).find((command) => command.type === 'list_directories')!;
  await server(page, 0, {
    type: 'command_result', request_id: directoryCommand.request_id, ok: true, phase: 'confirmed',
    data: {
      current: { path: '/home/test/Development/relay', label: '~/Development/relay' },
      parent: '/home/test/Development',
      directories: [{ name: 'frontend', path: '/home/test/Development/relay/frontend' }],
    },
  });
  await expect(page.getByLabel('Name')).toHaveValue('relay-codex');
  const currentDirectory = page.getByRole('button', { name: '~/Development/relay' });
  const directoryList = page.getByLabel('Subdirectories');
  await currentDirectory.click();
  await expect(directoryList).toBeVisible();
  await page.getByLabel('Name').focus();
  await expect(directoryList).toBeHidden();
  await currentDirectory.click();
  await page.getByRole('button', { name: /frontend/ }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'list_directories').length).toBe(2);
  const childDirectoryCommand = (await commands(page)).filter((command) => command.type === 'list_directories').at(-1)!;
  expect(childDirectoryCommand).toMatchObject({ path: '/home/test/Development/relay/frontend' });
  await server(page, 0, {
    type: 'command_result', request_id: childDirectoryCommand.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/home/test/Development/relay/frontend', label: '~/Development/relay/frontend' },
      parent: '/home/test/Development/relay', directories: [],
    },
  });
  await expect(page.getByRole('button', { name: '~/Development/relay/frontend' })).toBeVisible();
  await expect(page.getByLabel('Name')).toHaveValue('frontend-codex');
  await page.getByRole('button', { name: 'Open parent directory' }).click();
  await expect.poll(async () => (await commands(page)).filter((command) => command.type === 'list_directories').length).toBe(3);
  const parentDirectoryCommand = (await commands(page)).filter((command) => command.type === 'list_directories').at(-1)!;
  expect(parentDirectoryCommand).toMatchObject({ path: '/home/test/Development/relay' });
  await server(page, 0, {
    type: 'command_result', request_id: parentDirectoryCommand.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/home/test/Development/relay', label: '~/Development/relay' },
      parent: '/home/test/Development', directories: [{ name: 'frontend', path: '/home/test/Development/relay/frontend' }],
    },
  });
  await expect(page.getByRole('button', { name: '~/Development/relay' })).toBeVisible();
  await expect(page.getByLabel('Name')).toHaveValue('relay-codex');
  await page.getByLabel(/Initial task/).fill('Run the migration');
  await page.getByRole('button', { name: 'Start Agent', exact: true }).click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'agent_start')).toBe(true);
  expect((await commands(page)).find((command) => command.type === 'agent_start')).toMatchObject({
    profile_id: 'codex', cwd: '/home/test/Development/relay', name: 'relay-codex', prompt: 'Run the migration',
  });
  await expect(page.getByRole('heading', { name: 'Idle' })).toBeHidden();

  await server(page, 0, {
    type: 'agents',
    agents: [{
      pane_id: 'w1:p2', status: 'working', project: 'relay',
      cwd: '/home/test/Development/relay', name: 'relay-codex', agent: 'codex',
      session: 'Current Session Review',
    }],
  });
  await page.getByRole('button', { name: 'Open relay on Fedora', exact: true }).click();
  await expect(page.getByRole('main', { name: 'Terminal for relay' })).toBeVisible();
  await expect.poll(() => page.evaluate(() => location.hash))
    .toBe('#pane=r3.WzMsImZlZG9yYSIsInByaW1hcnkiLCJ3MTpwMiIsInRlcm1pbmFsLXcxOnAyIiwxLCIiXQ');
  await page.getByRole('button', { name: 'Manage agent' }).click();
  const manageDialog = page.getByRole('dialog', { name: 'Manage Agent' });
  await expect(manageDialog).toBeVisible();
  await expect(manageDialog.getByLabel('New tab name')).toHaveCount(0);
  await expect(manageDialog.getByLabel('New session name')).toHaveCount(0);
  const renameTabAction = manageDialog.getByRole('button', { name: 'Rename Tab' });
  await expect(renameTabAction).toBeVisible();
  await expect(manageDialog.getByRole('button', { name: 'Rename Session' })).toBeVisible();
  await renameTabAction.click();
  await expect(manageDialog.getByLabel('New tab name')).toBeFocused();
  await expect(manageDialog.getByLabel('New tab name')).toHaveValue('relay-codex');
  await setAutoCommands(page, false);
  await manageDialog.getByLabel('New tab name').fill('Sol-fhsh');
  await manageDialog.getByRole('button', { name: 'Rename', exact: true }).click();
  await expect.poll(async () => (await commands(page)).some((command) =>
    command.type === 'agent_rename' && command.name === 'Sol-fhsh')).toBe(true);
  const failedRename = (await commands(page)).find((command) =>
    command.type === 'agent_rename' && command.name === 'Sol-fhsh')!;
  await server(page, 0, {
    type: 'command_result', action: 'agent_rename', request_id: failedRename.request_id,
    ok: false, phase: 'failed', error: 'Could not rename tab',
  });
  const renameError = page.getByRole('status').filter({ hasText: 'Could not rename tab' });
  await expect(renameError).toBeVisible();
  expect(await renameError.evaluate((element) => element.matches(':popover-open'))).toBe(true);
  await expect(manageDialog).toBeVisible();
  await setAutoCommands(page, true);
  await manageDialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(renameTabAction).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(manageDialog).toBeHidden();

  await page.getByRole('button', { name: 'Manage agent' }).click();
  await renameTabAction.click();
  await manageDialog.getByLabel('New tab name').fill('123');
  await server(page, 0, { type: 'agent_update', pane_id: 'w1:p2', status: 'working', updated_at: 2 });
  await expect(manageDialog.getByLabel('New tab name')).toHaveValue('123');
  await manageDialog.getByRole('button', { name: 'Rename', exact: true }).click();
  await expect.poll(async () => (await commands(page)).some((command) =>
    command.type === 'agent_rename' && command.name === '123')).toBe(true);

  await page.getByRole('button', { name: 'Manage agent' }).click();
  const renameSessionAction = manageDialog.getByRole('button', { name: 'Rename Session' });
  await renameSessionAction.click();
  await expect(manageDialog.getByLabel('New session name')).toBeFocused();
  await expect(manageDialog.getByLabel('New session name')).toHaveValue('Current Session Review');
  await manageDialog.getByRole('button', { name: 'Rename', exact: true }).click();
  await expect.poll(async () => (await commands(page)).some((command) =>
    command.type === 'submit_prompt' && command.text === '/rename Current Session Review')).toBe(true);
  await expect(manageDialog).toBeHidden();

  await page.getByRole('button', { name: 'Manage agent' }).click();
  await renameSessionAction.click();
  await expect(manageDialog.getByLabel('New session name')).toHaveValue('Current Session Review');
  await manageDialog.getByLabel('New session name').fill('cancelled-session');
  await manageDialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(renameSessionAction).toBeFocused();
  expect((await commands(page)).some((command) =>
    command.type === 'submit_prompt' && command.text === '/rename cancelled-session')).toBe(false);
  await server(page, 0, {
    type: 'agent_update', pane_id: 'w1:p2', status: 'working', session_name: '', updated_at: 3,
  });
  await renameSessionAction.click();
  await expect(manageDialog.getByLabel('New session name')).toHaveValue('');
  await manageDialog.getByLabel('New session name').fill('renamed-session');
  await manageDialog.getByRole('button', { name: 'Rename', exact: true }).click();
  await expect.poll(async () => (await commands(page)).some((command) =>
    command.type === 'submit_prompt' && command.text === '/rename renamed-session')).toBe(true);

  await server(page, 0, {
    type: 'agent_update', pane_id: 'w1:p2', status: 'working', agent: 'opencode', updated_at: 4,
  });
  await page.getByRole('button', { name: 'Manage agent' }).click();
  await expect(manageDialog.getByRole('button', { name: 'Rename Tab' })).toBeVisible();
  await expect(manageDialog.getByRole('button', { name: 'Rename Session' })).toHaveCount(0);
  await page.keyboard.press('Escape');
  await server(page, 0, {
    type: 'agent_update', pane_id: 'w1:p2', status: 'working', agent: 'codex', updated_at: 5,
  });

  await page.getByRole('button', { name: 'Manage agent' }).click();
  await page.getByRole('button', { name: 'Clear Agent' }).click();
  await server(page, 0, { type: 'agent_update', pane_id: 'w1:p2', status: 'working', updated_at: 3 });
  await expect(page.getByRole('button', { name: 'Confirm Clear' })).toBeVisible();
  await page.getByRole('button', { name: 'Confirm Clear' }).click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'agent_clear')).toBe(true);
  await server(page, 0, { type: 'agents', agents: [{ pane_id: 'w1:p3', status: 'working', project: 'relay', cwd: '/home/test/Development/relay', name: 'clear-codex-123', agent: 'codex' }] });
  await expect.poll(() => page.evaluate(() => {
    const token = location.hash.match(/^#pane=r3\.(.+)$/)?.[1] || '';
    if (!token) return null;
    const padded = token.replaceAll('-', '+').replaceAll('_', '/') + '='.repeat((4 - token.length % 4) % 4);
    return JSON.parse(atob(padded));
  })).toEqual([3, 'fedora', 'primary', 'w1:p3', 'terminal-w1:p3', 1, '']);
  await expect(page.getByRole('main', { name: 'Terminal for relay' })).toBeVisible();

  await page.getByRole('button', { name: 'Manage agent' }).click();
  await page.getByRole('button', { name: 'Stop Agent' }).click();
  await server(page, 0, { type: 'agent_update', pane_id: 'w1:p3', status: 'working', updated_at: 4 });
  await expect(page.getByRole('button', { name: 'Confirm Stop' })).toBeVisible();
  await page.getByRole('button', { name: 'Confirm Stop' }).click();
  await expect.poll(async () => (await commands(page)).some((command) => command.type === 'agent_stop')).toBe(true);
});

test('directory picker in Launch keeps internal focus open and closes after selection', async ({ page }) => {
  await connectFedora(page);
  await page.getByRole('button', { name: 'Start agent' }).click();
  await page.getByLabel('Agent', { exact: true }).selectOption('codex');
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'list_directories').length).toBe(1);
  const initial = (await commands(page)).find(command => command.type === 'list_directories')!;
  await server(page, 0, {
    type: 'command_result', request_id: initial.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/home/test/work', label: '~/work' }, parent: '/home/test',
      directories: [{ name: 'project', path: '/home/test/work/project' }],
    },
  });
  const current = page.locator('.launch-page .directory-current');
  await expect(current).toHaveText('~/work ⌄');
  await expect(current).toHaveAttribute('aria-controls', 'launch-directory-list');
  await current.click();
  await expect(current).toHaveAttribute('aria-expanded', 'true');
  const list = page.locator('#launch-directory-list');
  const parent = list.getByRole('button', { name: '↰ Parent folder' });
  await parent.focus();
  await expect(parent).toBeFocused();
  await expect(current).toHaveAttribute('aria-expanded', 'true');
  await list.getByRole('button', { name: '📁 project' }).click();
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'list_directories').length).toBe(2);
  const child = (await commands(page)).findLast(command => command.type === 'list_directories')!;
  expect(child).toMatchObject({ path: '/home/test/work/project' });
  await expect(list.getByText('Loading folders…')).toBeVisible();
  await server(page, 0, {
    type: 'command_result', request_id: child.request_id, ok: true, phase: 'completed',
    data: { current: { path: '/home/test/work/project', label: '~/work/project' }, parent: '/home/test/work', directories: [] },
  });
  await expect(current).toHaveAttribute('aria-expanded', 'false');
  await expect(page.getByLabel('Name')).toHaveValue('project-codex');
  await current.click();
  await expect(list.getByText('This folder has no subdirectories. It remains selected.')).toBeVisible();
  await page.getByLabel('Name').focus();
  await expect(current).toHaveAttribute('aria-expanded', 'false');
  await current.click();
  await list.getByRole('button', { name: '↰ Parent folder' }).click();
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'list_directories').length).toBe(3);
  const back = (await commands(page)).findLast(command => command.type === 'list_directories')!;
  expect(back).toMatchObject({ path: '/home/test/work' });
  await server(page, 0, {
    type: 'command_result', request_id: back.request_id, ok: true, phase: 'completed',
    data: { current: { path: '/home/test/work', label: '~/work' }, parent: '/home/test', directories: [] },
  });
  await expect(current).toHaveAttribute('aria-expanded', 'false');
  await expect(page.getByLabel('Name')).toHaveValue('work-codex');
});

test('directory picker in Workspace stays open across navigation and label focus', async ({ page }) => {
  await connectFedora(page, { capabilities: ['directory_browser', 'workspace_management'] });
  await server(page, 0, { type: 'workspaces', workspaces: [] });
  await server(page, 0, { type: 'agents', agents: [] });
  await page.getByRole('button', { name: 'Manage workspaces' }).click();
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'list_directories').length).toBe(1);
  const initial = (await commands(page)).find(command => command.type === 'list_directories')!;
  await server(page, 0, {
    type: 'command_result', request_id: initial.request_id, ok: true, phase: 'completed',
    data: {
      current: { path: '/home/test/work', label: '~/work' }, parent: '/home/test',
      directories: [{ name: 'project', path: '/home/test/work/project' }],
    },
  });
  await page.getByRole('button', { name: 'Create Workspace' }).click();
  const dialog = page.locator('#workspace-create-dialog');
  const current = dialog.locator('.directory-current');
  const label = dialog.getByLabel('Label');
  await expect(label).toHaveValue('work');
  await expect(current).toHaveAttribute('aria-controls', 'workspace-directory-list');
  await current.click();
  await expect(current).toHaveAttribute('aria-expanded', 'true');
  await label.focus();
  await expect(current).toHaveAttribute('aria-expanded', 'true');
  const list = dialog.locator('#workspace-directory-list');
  await list.getByRole('button', { name: '📁 project' }).click();
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'list_directories').length).toBe(2);
  const child = (await commands(page)).findLast(command => command.type === 'list_directories')!;
  expect(child).toMatchObject({ path: '/home/test/work/project' });
  await expect(list.getByText('Loading folders…')).toBeVisible();
  await server(page, 0, {
    type: 'command_result', request_id: child.request_id, ok: true, phase: 'completed',
    data: { current: { path: '/home/test/work/project', label: '~/work/project' }, parent: '/home/test/work', directories: [] },
  });
  await expect(current).toHaveText('~/work/project ⌄');
  await expect(current).toHaveAttribute('aria-expanded', 'true');
  await expect(label).toHaveValue('work');
  await expect(list.getByText('This folder has no subdirectories. It remains selected.')).toHaveCount(0);
  await list.getByRole('button', { name: '↰ Parent folder' }).click();
  await expect.poll(async () => (await commands(page)).filter(command => command.type === 'list_directories').length).toBe(3);
  const back = (await commands(page)).findLast(command => command.type === 'list_directories')!;
  expect(back).toMatchObject({ path: '/home/test/work' });
  await server(page, 0, {
    type: 'command_result', request_id: back.request_id, ok: true, phase: 'completed',
    data: { current: { path: '/home/test/work', label: '~/work' }, parent: '/home/test', directories: [] },
  });
  await expect(current).toHaveText('~/work ⌄');
  await expect(current).toHaveAttribute('aria-expanded', 'true');
  await label.fill('Phone workspace');
  await dialog.getByRole('button', { name: 'Confirm' }).click();
  await expect.poll(async () => (await commands(page)).find(command => command.type === 'workspace_create')).toMatchObject({
    cwd: '/home/test/work', label: 'Phone workspace',
  });
});
