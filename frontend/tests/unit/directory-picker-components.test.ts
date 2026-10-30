import { render, screen, waitFor, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { afterEach, expect, it, vi } from 'vitest';
import LaunchView from '$components/LaunchView.svelte';
import WorkspaceManager from '$components/WorkspaceManager.svelte';
import { relayStore } from '$lib/store';
import type { DirectoryListing, RelayConnectionView } from '$lib/types';

const work: DirectoryListing = {
  current: { path: '/home/test/work', label: '~/work' },
  parent: '/home/test',
  directories: [{ name: 'project', path: '/home/test/work/project' }],
};
const project: DirectoryListing = {
  current: { path: '/home/test/work/project', label: '~/work/project' },
  parent: '/home/test/work',
  directories: [],
};
const home: DirectoryListing = {
  current: { path: '/home/test', label: '~' },
  parent: '',
  directories: [{ name: 'work', path: '/home/test/work' }],
};
const unsupportedNotice = 'Update and restart this computer’s relay to browse directories.';
const emptyNotice = 'This folder has no subdirectories. It remains selected.';

function fixture(patch: Partial<RelayConnectionView> = {}) {
  let connection = {
    status: 'connected', inventory: { state: 'ready' },
    capabilities: ['directory_browser', 'workspace_management'],
    agentProfiles: [{ id: 'codex', label: 'Codex' }], hermesProfiles: [],
    directoryBrowser: null, directoryLoading: false, directoryError: '',
    ...patch,
  } as RelayConnectionView;
  relayStore.relayConfigs.set([{ id: 'fedora', label: 'Fedora', url: 'wss://fedora', token: '' }]);
  function publish(update: Partial<RelayConnectionView>) {
    connection = { ...connection, ...update };
    relayStore.connections.set(new Map([['fedora', connection as never]]));
  }
  publish({});
  const requests: Array<{
    path: string;
    resolve: (listing: DirectoryListing) => void;
    reject: (error: Error) => void;
  }> = [];
  const list = vi.spyOn(relayStore, 'listDirectories').mockImplementation((_relay, path = '') => {
    publish({ directoryLoading: true, directoryError: '' });
    return new Promise<DirectoryListing>((resolve, reject) => requests.push({ path, resolve, reject }));
  });
  vi.spyOn(relayStore, 'showToast').mockImplementation(() => {});
  function succeed(index: number, listing: DirectoryListing) {
    publish({ directoryBrowser: listing, directoryLoading: false, directoryError: '' });
    requests[index].resolve(listing);
  }
  function fail(index: number, message: string) {
    publish({ directoryLoading: false, directoryError: message });
    requests[index].reject(new Error(message));
  }
  return { list, requests, publish, succeed, fail };
}

afterEach(() => {
  vi.restoreAllMocks();
  relayStore.connections.set(new Map());
  relayStore.relayConfigs.set([]);
  relayStore.workspaces.set([]);
  relayStore.agents.set([]);
});

it('Launch owns selection, focus and submission while navigating the directory picker', async () => {
  const user = userEvent.setup();
  const dirs = fixture();
  const send = vi.spyOn(relayStore, 'sendCommand').mockReturnValue(new Promise(() => {}));
  const { container } = render(LaunchView);
  await waitFor(() => expect(dirs.requests).toHaveLength(1));
  expect(dirs.list).toHaveBeenCalledWith('fedora', '');
  const current = container.querySelector<HTMLButtonElement>('.directory-current')!;
  const start = screen.getByRole('button', { name: 'Start Agent' });
  expect(current).toHaveAttribute('aria-controls', 'launch-directory-list');
  expect(current).toHaveAttribute('aria-expanded', 'false');
  expect(screen.getByRole('button', { name: 'Open parent directory' })).toBeDisabled();
  await user.click(current);
  expect(current).toHaveAttribute('aria-expanded', 'true');
  expect(screen.getByLabelText('Subdirectories')).toHaveAttribute('id', 'launch-directory-list');
  expect(screen.getByText('Loading folders…')).toBeInTheDocument();
  expect(start).toBeDisabled();
  dirs.succeed(0, work);
  await waitFor(() => expect(current).toHaveAttribute('aria-expanded', 'false'));
  expect(screen.getByLabelText('Name')).toHaveValue('work-codex');

  await user.click(current);
  const parent = screen.getByRole('button', { name: '↰ Parent folder' });
  parent.focus();
  expect(parent).toHaveFocus();
  expect(current).toHaveAttribute('aria-expanded', 'true');
  expect(parent).toHaveAttribute('type', 'button');
  expect(screen.getByRole('button', { name: '📁 project' })).toHaveAttribute('type', 'button');
  await user.click(screen.getByRole('button', { name: '📁 project' }));
  expect(dirs.requests[1].path).toBe('/home/test/work/project');
  expect(screen.getByText('Loading folders…')).toBeInTheDocument();
  expect(start).toBeDisabled();
  dirs.succeed(1, project);
  await waitFor(() => expect(current).toHaveAttribute('aria-expanded', 'false'));
  expect(screen.getByLabelText('Name')).toHaveValue('project-codex');
  await user.click(current);
  expect(screen.getByText(emptyNotice)).toBeInTheDocument();
  screen.getByLabelText('Name').focus();
  await waitFor(() => expect(current).toHaveAttribute('aria-expanded', 'false'));

  await user.click(current);
  await user.click(screen.getByRole('button', { name: '↰ Parent folder' }));
  expect(dirs.requests[2].path).toBe('/home/test/work');
  dirs.succeed(2, work);
  await waitFor(() => expect(current).toHaveAttribute('aria-expanded', 'false'));
  await user.click(screen.getByRole('button', { name: 'Open parent directory' }));
  expect(dirs.requests[3].path).toBe('/home/test');
  dirs.succeed(3, home);
  await waitFor(() => expect(screen.getByText(/Agents cannot start in the home folder itself/)).toBeInTheDocument());
  expect(start).toBeDisabled();
  await user.click(current);
  await user.click(screen.getByRole('button', { name: '📁 work' }));
  dirs.succeed(4, work);
  await waitFor(() => expect(start).toBeEnabled());
  await user.click(start);
  expect(send).toHaveBeenCalledOnce();
  expect(send.mock.calls[0][0]).toBe('fedora');
  expect(send.mock.calls[0][1]).toMatchObject({ type: 'agent_start', cwd: '/home/test/work', name: 'work-codex', profile_id: 'codex' });
});

it('Workspace keeps the directory picker open and owns label and create submission', async () => {
  const user = userEvent.setup();
  const dirs = fixture();
  const create = vi.spyOn(relayStore, 'createWorkspace').mockResolvedValue({} as never);
  const { container } = render(WorkspaceManager);
  await waitFor(() => expect(dirs.requests).toHaveLength(1));
  dirs.succeed(0, work);
  await user.click(screen.getByRole('button', { name: 'Create Workspace' }));
  const dialog = screen.getByRole('dialog', { name: 'Create Workspace' });
  const current = container.querySelector<HTMLButtonElement>('.directory-current')!;
  const label = within(dialog).getByLabelText('Label');
  await waitFor(() => expect(label).toHaveValue('work'));
  expect(current).toHaveAttribute('aria-controls', 'workspace-directory-list');
  expect(current).toHaveAttribute('aria-expanded', 'false');
  await user.click(current);
  expect(within(dialog).getByLabelText('Subdirectories')).toHaveAttribute('id', 'workspace-directory-list');
  expect(current).toHaveAttribute('aria-expanded', 'true');
  await user.clear(label);
  expect(current).toHaveAttribute('aria-expanded', 'true');
  await user.click(within(dialog).getByRole('button', { name: '📁 project' }));
  expect(dirs.requests[1].path).toBe('/home/test/work/project');
  expect(within(dialog).getByText('Loading folders…')).toBeInTheDocument();
  dirs.succeed(1, project);
  await waitFor(() => expect(label).toHaveValue('project'));
  expect(current).toHaveAttribute('aria-expanded', 'true');
  expect(within(dialog).queryByText(emptyNotice)).not.toBeInTheDocument();
  await user.clear(label);
  await user.type(label, 'My workspace');
  expect(current).toHaveAttribute('aria-expanded', 'true');
  await user.click(within(dialog).getByRole('button', { name: '↰ Parent folder' }));
  expect(dirs.requests[2].path).toBe('/home/test/work');
  dirs.succeed(2, work);
  await waitFor(() => expect(current).toHaveTextContent('~/work'));
  expect(label).toHaveValue('My workspace');
  expect(current).toHaveAttribute('aria-expanded', 'true');
  await user.click(within(dialog).getByRole('button', { name: 'Open parent directory' }));
  expect(dirs.requests[3].path).toBe('/home/test');
  dirs.succeed(3, home);
  await waitFor(() => expect(current).toHaveTextContent('~'));
  expect(current).toHaveAttribute('aria-expanded', 'true');
  await user.click(current);
  expect(current).toHaveAttribute('aria-expanded', 'false');
  await user.click(within(dialog).getByRole('button', { name: 'Confirm' }));
  await waitFor(() => expect(create).toHaveBeenCalledExactlyOnceWith('fedora', '/home/test', 'My workspace'));
});

it.each(['Launch', 'Workspace'] as const)('%s presents the existing loading and error states', async (caller) => {
  const user = userEvent.setup();
  const dirs = fixture();
  const { container } = render(caller === 'Launch' ? LaunchView : WorkspaceManager);
  await waitFor(() => expect(dirs.requests).toHaveLength(1));
  if (caller === 'Workspace') await user.click(screen.getByRole('button', { name: 'Create Workspace' }));
  const current = container.querySelector<HTMLButtonElement>('.directory-current')!;
  await user.click(current);
  expect(screen.getByText('Loading folders…')).toBeInTheDocument();
  dirs.fail(0, 'Folder unavailable');
  await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Folder unavailable'));
  expect(current).toHaveAttribute('aria-expanded', 'true');
});

it.each(['Launch', 'Workspace'] as const)('%s preserves its unsupported and empty notice policy', async (caller) => {
  const user = userEvent.setup();
  const dirs = fixture({
    capabilities: ['workspace_management'], directoryBrowser: project,
    directoryLoading: true, directoryError: 'Folder unavailable',
  });
  const { container } = render(caller === 'Launch' ? LaunchView : WorkspaceManager);
  if (caller === 'Workspace') await user.click(screen.getByRole('button', { name: 'Create Workspace' }));
  const current = container.querySelector<HTMLButtonElement>('.directory-current')!;
  await user.click(current);
  expect(dirs.list).not.toHaveBeenCalled();
  if (caller === 'Launch') {
    expect(screen.getByText(unsupportedNotice)).toBeInTheDocument();
    expect(screen.queryByText('Loading folders…')).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  } else {
    expect(screen.queryByText(unsupportedNotice)).not.toBeInTheDocument();
    expect(screen.getByText('Loading folders…')).toBeInTheDocument();
    dirs.publish({ directoryLoading: false });
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Folder unavailable'));
    dirs.publish({ directoryError: '' });
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
    expect(screen.queryByText(emptyNotice)).not.toBeInTheDocument();
  }
});
