import { render, screen, waitFor, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import ConversationHistory from '$components/ConversationHistory.svelte';
import ManageDialog from '$components/ManageDialog.svelte';
import { paneViewPreferenceKey } from '$lib/agent-view';
import {
  defaultAgentView,
  paneAgentViewOverrides,
  setDefaultAgentView,
  setPaneAgentView,
} from '$lib/preferences';
import { currentView } from '$lib/router';
import { relayStore } from '$lib/store';
import type { Agent, ConversationPage } from '$lib/types';

function agent(relayId = 'fedora', rawPaneId = 'pane-1', terminalId = 'terminal-1'): Agent {
  return {
    relay_id: relayId,
    relay_label: relayId,
    raw_pane_id: rawPaneId,
    pane_id: `${relayId}::${rawPaneId}`,
    server_session_id: 'primary',
    terminal_id: terminalId,
    generation: 1,
    agent_session_id: 'session-1',
    agent: 'codex',
    project: `${relayId} project`,
    status: 'working',
  };
}

function page(overrides: Partial<ConversationPage> = {}): ConversationPage {
  return {
    available: true,
    reason: '',
    entries: [],
    hasMore: false,
    total: 0,
    fileTruncated: false,
    sourceCorrupt: false,
    ...overrides,
  };
}

describe('agent view controls and conversation loading hook', () => {
  beforeEach(() => {
    localStorage.clear();
    defaultAgentView.set('terminal');
    paneAgentViewOverrides.set({});
    currentView.set({ view: 'agents' });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
    localStorage.clear();
    defaultAgentView.set('terminal');
    paneAgentViewOverrides.set({});
    currentView.set({ view: 'agents' });
  });

  it('shows the inherited pane choice reactively and saves explicit choices without navigation', async () => {
    const user = userEvent.setup();
    const current = agent();
    render(ManageDialog, { open: true, agent: current });
    const select = screen.getByRole('combobox', { name: 'Default View' });
    expect(select).toHaveValue('default');
    expect(within(select).getByRole('option', { name: 'Use default (Terminal)' })).toBeInTheDocument();

    setDefaultAgentView('conversation');
    await waitFor(() => expect(within(select).getByRole('option', { name: 'Use default (Conversation)' })).toBeInTheDocument());
    await user.selectOptions(select, 'conversation');
    expect(select).toHaveValue('conversation');
    expect(paneAgentViewOverrides).toBeDefined();
    expect(localStorage.getItem('herdr_pane_agent_view_overrides')).toContain('conversation');
    expect(currentView).toBeDefined();
    expect(screen.getByRole('dialog', { name: 'Manage Agent' })).toBeVisible();
    expect(screen.getByRole('button', { name: 'Close' })).toBeVisible();
  });

  it('uses inheritance by removing the pane entry and keeps an explicit equal choice', async () => {
    const user = userEvent.setup();
    const current = agent();
    setDefaultAgentView('conversation');
    setPaneAgentView(current, 'conversation');
    render(ManageDialog, { open: true, agent: current });
    const select = screen.getByRole('combobox', { name: 'Default View' });
    expect(select).toHaveValue('conversation');
    await user.selectOptions(select, 'default');
    expect(select).toHaveValue('default');
    expect(localStorage.getItem('herdr_pane_agent_view_overrides')).toBeNull();
    await user.selectOptions(select, 'conversation');
    expect(JSON.parse(localStorage.getItem('herdr_pane_agent_view_overrides')!)[paneViewPreferenceKey(current)!]).toBe('conversation');
  });

  it('does not leak a pane choice when the dialog changes agents', async () => {
    const first = agent();
    const second = agent('fedora', 'pane-2', 'terminal-2');
    setPaneAgentView(first, 'conversation');
    const view = render(ManageDialog, { open: true, agent: first });
    expect(screen.getByRole('combobox', { name: 'Default View' })).toHaveValue('conversation');
    await view.rerender({ open: true, agent: second });
    expect(screen.getByRole('combobox', { name: 'Default View' })).toHaveValue('default');
  });

  it('allows readers to change only the local preference', async () => {
    const user = userEvent.setup();
    render(ManageDialog, { open: true, agent: agent(), readOnly: true });
    const dialog = screen.getByRole('dialog', { name: 'Manage Agent' });
    const select = within(dialog).getByRole('combobox', { name: 'Default View' });
    expect(select).toBeEnabled();
    await user.selectOptions(select, 'conversation');
    expect(select).toHaveValue('conversation');
    expect(within(dialog).getByRole('button', { name: 'Rename Tab' })).toBeDisabled();
    expect(within(dialog).getByRole('button', { name: 'Clear Agent' })).toBeDisabled();
    expect(within(dialog).getByRole('button', { name: 'Stop Agent' })).toBeDisabled();
  });

  it('disables the pane choice and explains missing stable identity', () => {
    render(ManageDialog, {
      open: true,
      agent: agent('fedora', 'pane-1', ''),
      readOnly: true,
    });
    const dialog = screen.getByRole('dialog', { name: 'Manage Agent' });
    expect(within(dialog).getByRole('combobox', { name: 'Default View' })).toBeDisabled();
    expect(within(dialog).getByText(/stable pane identity is unavailable/)).toBeInTheDocument();
  });

  it('restores a native select when its pane save fails', async () => {
    const user = userEvent.setup();
    const setItem = vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('storage full');
    });
    render(ManageDialog, { open: true, agent: agent() });
    const select = screen.getByRole('combobox', { name: 'Default View' });
    await user.selectOptions(select, 'conversation');
    expect(setItem).toHaveBeenCalled();
    expect(select).toHaveValue('default');
    expect(paneAgentViewOverrides).toBeDefined();
  });

  it('calls the initial-page hook once for the first settled latest load', async () => {
    const callback = vi.fn();
    vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue(page({ available: false, reason: 'No transcript' }));
    render(ConversationHistory, { agent: agent(), onInitialPage: callback });
    await waitFor(() => expect(callback).toHaveBeenCalledWith(expect.objectContaining({ available: false })));
    expect(callback).toHaveBeenCalledOnce();
  });

  it('does not call the hook for older pages, later polls, or initial errors', async () => {
    const callback = vi.fn();
    const initial = page({ entries: [{ id: 'turn-1', timestamp: '2026-01-01', role: 'user', text: 'hello' }], hasMore: true, total: 1 });
    const older = page({ entries: [{ id: 'turn-0', timestamp: '2025-12-31', role: 'user', text: 'older' }] });
    const history = vi.spyOn(relayStore, 'getConversationHistory')
      .mockResolvedValueOnce(initial)
      .mockResolvedValueOnce(older);
    const initialView = render(ConversationHistory, { agent: agent(), onInitialPage: callback });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Load older turns' })).toBeVisible());
    await userEvent.setup().click(screen.getByRole('button', { name: 'Load older turns' }));
    await waitFor(() => expect(history).toHaveBeenCalledTimes(2));
    expect(callback).toHaveBeenCalledOnce();
    initialView.unmount();

    history.mockReset().mockRejectedValueOnce(new Error('read failed')).mockResolvedValueOnce(page({ available: false }));
    const errorCallback = vi.fn();
    const view = render(ConversationHistory, { agent: agent('fedora', 'pane-2'), onInitialPage: errorCallback });
    await waitFor(() => expect(screen.getByText('read failed')).toBeInTheDocument());
    expect(errorCallback).not.toHaveBeenCalled();
    view.unmount();
  });

  it('lets the first latest request to settle consume the fallback window', async () => {
    vi.useFakeTimers();
    const callback = vi.fn();
    const resolves: ((value: ConversationPage) => void)[] = [];
    vi.spyOn(relayStore, 'getConversationHistory').mockImplementation(() => new Promise((resolve) => {
      resolves.push(resolve);
    }));
    render(ConversationHistory, { agent: agent(), onInitialPage: callback });
    expect(resolves).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(5_000);
    expect(resolves).toHaveLength(2);
    resolves[1](page({ available: false }));
    await Promise.resolve();
    await Promise.resolve();
    expect(callback).toHaveBeenCalledOnce();
    resolves[0](page({ available: false }));
    await Promise.resolve();
    expect(callback).toHaveBeenCalledOnce();
    vi.useRealTimers();
  });
});
