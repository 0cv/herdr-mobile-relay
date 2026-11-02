import { render, screen, within, fireEvent } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import WorkspaceManager from '$components/WorkspaceManager.svelte';
import { CommandError, relayStore } from '$lib/store';
import type { CommandResult, RelayConnectionView, RelayWorkspace } from '$lib/types';

beforeEach(() => {
  relayStore.relayConfigs.set([{ id: 'test', label: 'Test', url: 'wss://test', token: '' }]);
  relayStore.connections.set(new Map([['test', {
    status: 'connected', inventory: { state: 'ready' },
    capabilities: ['workspace_management', 'worktree_management'],
  } as RelayConnectionView as never]]));
  relayStore.workspaces.set([false, true].map((linked, i) => ({
    relay_id: 'test', relay_label: 'Test', workspace_id: `w${i + 1}`, number: i + 1,
    label: linked ? 'Fix' : 'Project', focused: false, pane_count: 1, tab_count: 1,
    active_tab_id: '', agent_status: '', cwd: `/work/${i}`,
    worktree: { repo_key: 'repo', repo_name: 'project', repo_root: '/work/0', checkout_path: `/work/${i}`, is_linked_worktree: linked },
  } as RelayWorkspace)));
});
afterEach(() => {
  vi.restoreAllMocks();
  relayStore.workspaces.set([]);
  relayStore.connections.set(new Map());
  relayStore.relayConfigs.set([]);
});

it.each(['Close', 'Close Group', 'Remove Worktree'])('%s opens with safe Cancel focus and Enter does not mutate', async name => {
  const user = userEvent.setup();
  const close = vi.spyOn(relayStore, 'closeWorkspace').mockRejectedValue(Object.assign(new CommandError('Group consent required'), { data: { code: 'workspace_group_close_required' } }));
  const remove = vi.spyOn(relayStore, 'removeWorktree');
  render(WorkspaceManager);
  await user.click(screen.getAllByRole('button', { name: name === 'Close Group' ? 'Close' : name })[0]);
  if (name === 'Close Group') {
    await user.click(screen.getByRole('button', { name: 'Close Workspace' }));
    await vi.waitFor(() => expect(screen.getByRole('dialog')).toHaveAccessibleName('Close Project group?'));
  }
  const dialog = screen.getByRole('dialog');
  expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus();
  await user.keyboard('{Enter}');
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  expect(close).toHaveBeenCalledTimes(name === 'Close Group' ? 1 : 0);
  expect(remove).not.toHaveBeenCalled();
});

it('focuses enabled Cancel after a busy removal refusal, without force consent', async () => {
  const user = userEvent.setup();
  let reject!: (reason: unknown) => void;
  const remove = vi.spyOn(relayStore, 'removeWorktree').mockReturnValue(new Promise<CommandResult>((_, fail) => { reject = fail; }));
  render(WorkspaceManager);
  await user.click(screen.getByRole('button', { name: 'Remove Worktree' }));
  const dialog = screen.getByRole('dialog');
  await user.click(within(dialog).getByRole('button', { name: 'Remove Worktree' }));
  expect(within(dialog).getByRole('button', { name: 'Cancel' })).toBeDisabled();
  reject(Object.assign(new CommandError('Dirty checkout'), { data: { force_available: true } }));
  await vi.waitFor(() => expect(within(dialog).getByRole('button', { name: 'Force Remove' })).toBeEnabled());
  expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus();
  await user.keyboard('{Enter}');
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  expect(remove).toHaveBeenCalledTimes(1);
  expect(remove.mock.calls[0][1]).toBe(false);
});

it.each(['cancel', 'click'])('dismissal by %s while busy fences a late refusal', async event => {
  const user = userEvent.setup();
  let reject!: (reason: unknown) => void;
  const remove = vi.spyOn(relayStore, 'removeWorktree').mockReturnValue(new Promise<CommandResult>((_, fail) => { reject = fail; }));
  render(WorkspaceManager);
  await user.click(screen.getByRole('button', { name: 'Remove Worktree' }));
  const dialog = screen.getByRole('dialog');
  await user.click(within(dialog).getByRole('button', { name: 'Remove Worktree' }));
  await fireEvent(dialog, new Event(event, { bubbles: true }));
  reject(Object.assign(new CommandError('Dirty checkout'), { data: { force_available: true } }));
  await vi.waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  expect(remove).toHaveBeenCalledTimes(1);
});

it('keeps destructive entry controls disabled for readers', () => {
  render(WorkspaceManager, { readOnlyRelayIds: new Set(['test']) });
  for (const name of ['Close', 'Remove Worktree']) {
    for (const button of screen.getAllByRole('button', { name })) expect(button).toBeDisabled();
  }
});
