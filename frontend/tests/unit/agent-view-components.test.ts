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

  it('hides a leading older fragment while showing the usable latest exchange', async () => {
    const history = vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue(page({
      entries: [
        { id: 'orphan', timestamp: '2026-01-01', role: 'assistant', text: 'older fragment' },
        { id: 'latest-user', timestamp: '2026-01-01', role: 'user', text: 'latest question' },
        { id: 'latest-answer', timestamp: '2026-01-01', role: 'assistant', text: 'latest answer' },
      ],
      nextCursor: 'older-cursor', hasMore: true, total: 3, state: 'ready', mode: 'recent', sourceRevision: 'source-1',
    }));
    const view = render(ConversationHistory, { agent: agent('fedora', 'pending-pane', 'pending-terminal') });
    try {
      await waitFor(() => expect(screen.getByText('latest answer')).toBeVisible());
      expect(screen.getByText('latest question')).toBeVisible();
      expect(screen.queryByText('older fragment')).not.toBeInTheDocument();
    } finally {
      view.unmount();
      history.mockRestore();
    }
  });

  it('does not call the hook for older pages, later polls, or initial errors', async () => {
    const callback = vi.fn();
    const initial = page({ entries: [{ id: 'turn-1', timestamp: '2026-01-01', role: 'user', text: 'hello' }], nextCursor: 'cursor-1', hasMore: true, total: 1 });
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

  it('keeps a stable snapshot visible after sending and returns explicitly to latest', async () => {
    const user = userEvent.setup();
    const current = agent();
    const history = vi.spyOn(relayStore, 'getConversationHistory')
      .mockResolvedValueOnce(page({
        entries: [{ id: 'turn-1', timestamp: '2026-01-01', role: 'user', text: 'question' }],
        nextCursor: 'older-cursor', hasMore: true, total: 2, state: 'ready', mode: 'recent', sourceRevision: 'source-1',
      }))
      .mockResolvedValueOnce(page({
        entries: [{ id: 'turn-0', timestamp: '2025-12-31', role: 'assistant', text: 'older answer' }],
        hasMore: false, total: 2, state: 'ready', mode: 'snapshot', snapshotId: 'snapshot-1', sourceRevision: 'source-1',
      }))
      .mockResolvedValueOnce(page({
        entries: [{ id: 'turn-2', timestamp: '2026-01-01T00:01:00Z', role: 'assistant', text: 'new latest answer' }],
        hasMore: false, total: 3, state: 'ready', mode: 'recent', sourceRevision: 'source-2',
      }));
    const send = vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue({
      type: 'command_result', request_id: 'prompt-1', ok: true,
    });
    try {
      render(ConversationHistory, { agent: current });
      await waitFor(() => expect(screen.getByRole('button', { name: 'Load older turns' })).toBeVisible());
      await user.click(screen.getByRole('button', { name: 'Load older turns' }));
      await waitFor(() => expect(screen.getByText('Viewing a stable snapshot of older history. New messages are not included; return to latest to view replies.')).toBeVisible());
      await user.type(screen.getByRole('textbox', { name: 'Prompt' }), 'send while browsing');
      await user.click(screen.getByRole('button', { name: 'Send prompt' }));
      await waitFor(() => expect(screen.getByText('Prompt sent. Return to latest to view the new reply.')).toBeVisible());
      expect(screen.getByText('older answer')).toBeInTheDocument();
      await user.click(screen.getByRole('button', { name: 'Return to latest' }));
      await waitFor(() => expect(screen.getByText('new latest answer')).toBeInTheDocument());
      expect(screen.queryByText('Viewing a stable snapshot of older history. New messages are not included; return to latest to view replies.')).not.toBeInTheDocument();
      expect(send).toHaveBeenCalledWith(current, { type: 'submit_prompt', text: 'send while browsing' });
      expect(history).toHaveBeenCalledTimes(3);
    } finally {
      history.mockRestore();
      send.mockRestore();
    }
  });

  it('preserves loaded turns for invalid cursors and reloads only after an explicit action', async () => {
    const user = userEvent.setup();
    const initial = page({
      entries: [{ id: 'turn-1', timestamp: '2026-01-01', role: 'user', text: 'retained question' }],
      nextCursor: 'stale-cursor', hasMore: true, total: 2, state: 'ready', mode: 'recent', sourceRevision: 'source-1',
    });
    const history = vi.spyOn(relayStore, 'getConversationHistory')
      .mockResolvedValueOnce(initial)
      .mockResolvedValueOnce(page({
        state: 'failed', mode: 'recent', sourceRevision: 'source-1',
        error: { code: 'invalid_cursor', message: 'This history cursor is invalid.', retryable: false },
      }))
      .mockResolvedValueOnce(page({
        entries: [{ id: 'turn-2', timestamp: '2026-01-02', role: 'assistant', text: 'reloaded answer' }],
        hasMore: false, total: 1, state: 'ready', mode: 'recent', sourceRevision: 'source-2',
      }));
    try {
      render(ConversationHistory, { agent: agent() });
      await waitFor(() => expect(screen.getByRole('button', { name: 'Load older turns' })).toBeVisible());
      await user.click(screen.getByRole('button', { name: 'Load older turns' }));
      await waitFor(() => expect(screen.getByRole('button', { name: 'Reload history' })).toBeVisible());
      expect(screen.getByText('retained question')).toBeInTheDocument();
      await user.click(screen.getByRole('button', { name: 'Reload history' }));
      await waitFor(() => expect(screen.getByText('reloaded answer')).toBeInTheDocument());
      expect(screen.queryByRole('button', { name: 'Reload history' })).not.toBeInTheDocument();
      expect(history).toHaveBeenCalledTimes(3);
    } finally {
      history.mockRestore();
    }
  });

  it('pauses preparation while hidden work is canceled and resumes it once', async () => {
    vi.useFakeTimers();
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    const history = vi.spyOn(relayStore, 'getConversationHistory')
      .mockResolvedValueOnce(page({
        state: 'preparing', mode: 'recent', nextCursor: 'prepare-cursor', hasMore: false, total: null,
      }))
      .mockResolvedValueOnce(page({
        state: 'ready', mode: 'snapshot', snapshotId: 'snapshot-1', sourceRevision: 'source-1',
        entries: [{ id: 'turn-0', timestamp: '2025-12-31', role: 'user', text: 'prepared question' }],
        hasMore: false, total: 1,
      }));
    try {
      render(ConversationHistory, { agent: agent() });
      await vi.waitFor(() => expect(screen.getByRole('button', { name: 'Cancel' })).toBeVisible());
      await user.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(screen.getByRole('button', { name: 'Continue' })).toBeVisible();
      await vi.advanceTimersByTimeAsync(2_000);
      expect(history).toHaveBeenCalledTimes(1);
      await user.click(screen.getByRole('button', { name: 'Continue' }));
      await vi.advanceTimersByTimeAsync(1_000);
      await vi.waitFor(() => expect(history).toHaveBeenCalledTimes(2));
      await vi.waitFor(() => expect(screen.getByText('prepared question')).toBeVisible());
    } finally {
      history.mockRestore();
      vi.useRealTimers();
    }
  });

  it('ignores an older page that settles after the pane target changes', async () => {
    const current = agent();
    const replacement = agent('fedora', 'pane-2', 'terminal-2');
    let releaseOlder!: (value: ConversationPage) => void;
    const history = vi.spyOn(relayStore, 'getConversationHistory')
      .mockResolvedValueOnce(page({
        entries: [{ id: 'turn-1', timestamp: '2026-01-01', role: 'user', text: 'current question' }],
        nextCursor: 'older-cursor', hasMore: true, total: 2, state: 'ready', mode: 'recent', sourceRevision: 'source-1',
      }))
      .mockImplementationOnce(() => new Promise((resolve) => { releaseOlder = resolve; }))
      .mockResolvedValueOnce(page({
        entries: [{ id: 'replacement', timestamp: '2026-01-02', role: 'assistant', text: 'replacement answer' }],
        state: 'ready', mode: 'recent', sourceRevision: 'source-2',
      }));
    try {
      const view = render(ConversationHistory, { agent: current });
      await waitFor(() => expect(screen.getByRole('button', { name: 'Load older turns' })).toBeVisible());
      await userEvent.setup().click(screen.getByRole('button', { name: 'Load older turns' }));
      await view.rerender({ agent: replacement });
      releaseOlder(page({ entries: [{ id: 'stale', timestamp: '2025-01-01', role: 'assistant', text: 'stale older answer' }], state: 'ready', mode: 'recent' }));
      await waitFor(() => expect(history).toHaveBeenCalledTimes(3));
      expect(screen.queryByText('stale older answer')).not.toBeInTheDocument();
      await waitFor(() => expect(screen.getByText('replacement answer')).toBeInTheDocument());
      view.unmount();
    } finally {
      history.mockRestore();
    }
  });

  it('does not start an overlapping latest request while the current one is pending', async () => {
    vi.useFakeTimers();
    const callback = vi.fn();
    const resolves: ((value: ConversationPage) => void)[] = [];
    vi.spyOn(relayStore, 'getConversationHistory').mockImplementation(() => new Promise((resolve) => {
      resolves.push(resolve);
    }));
    render(ConversationHistory, { agent: agent(), onInitialPage: callback });
    expect(resolves).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(5_000);
    expect(resolves).toHaveLength(1);
    resolves[0](page({ available: false }));
    await vi.waitFor(() => expect(callback).toHaveBeenCalledOnce());
    vi.useRealTimers();
  });
});
