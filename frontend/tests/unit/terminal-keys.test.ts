import { render, screen, waitFor, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import TerminalKeys from '$components/TerminalKeys.svelte';
import TerminalView from '$components/TerminalView.svelte';
import { relayStore } from '$lib/store';
import type { Agent, CommandResult } from '$lib/types';
import { deferred } from '../support/relay';

const agent: Agent = {
  relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: 'fedora::w1:p1',
  server_session_id: 'primary', terminal_id: 'terminal', generation: 1,
  project: 'relay', agent: 'claude', status: 'working',
};

function commandResult(requestId: string): CommandResult {
  return { type: 'command_result', request_id: requestId, ok: true };
}

function setPagingCapability(relayId = agent.relay_id) {
  relayStore.connections.set(new Map([[relayId, { capabilities: ['paging_keys'] }]] as never));
}

describe('TerminalKeys', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    relayStore.connections.set(new Map());
  });

  it('only shows arrows when the agent relay does not advertise paging keys', async () => {
    setPagingCapability('another-relay');
    render(TerminalKeys, { agent });

    await userEvent.click(screen.getByRole('button', { name: 'Arrow keys' }));
    const menu = within(screen.getByRole('group', { name: 'Navigation keys' }));
    expect(menu.getAllByRole('button')).toHaveLength(4);
    for (const name of ['Up', 'Left', 'Right', 'Down']) {
      expect(menu.getByRole('button', { name })).toBeVisible();
    }
  });

  it.each([
    { name: 'Page down', modifiers: [], key: 'PageDown', label: 'Page down' },
    { name: 'Home', modifiers: ['Ctrl', 'Shift'], key: 'ctrl+shift+home', label: 'Ctrl+Shift+Home' },
    { name: 'Page down', modifiers: ['Ctrl'], key: 'ctrl+pagedown', label: 'Ctrl+Page down' },
  ])('sends $label and keeps the arrow menu open', async ({ name, modifiers, key, label }) => {
    setPagingCapability();
    const user = userEvent.setup();
    vi.spyOn(relayStore, 'readPane').mockImplementation(() => undefined);
    const send = vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue(commandResult('paging-1'));
    render(TerminalKeys, { agent });
    for (const modifier of modifiers) {
      await user.click(screen.getByRole('button', { name: modifier }));
    }
    await user.click(screen.getByRole('button', { name: 'Arrow keys' }));
    await user.click(screen.getByRole('button', { name }));

    expect(send).toHaveBeenCalledExactlyOnceWith(agent, { type: 'send_keys', keys: [key], activity_label: label });
    expect(screen.getByRole('status')).toHaveTextContent(`${label} sent`);
    expect(screen.getByRole('group', { name: 'Navigation keys' })).toBeVisible();
    for (const modifier of modifiers) {
      expect(screen.getByRole('button', { name: modifier })).toHaveAttribute('aria-pressed', 'true');
    }
  });

  it('flashes a tapped key but not a modifier or menu toggle', async () => {
    const user = userEvent.setup();
    vi.spyOn(relayStore, 'readPane').mockImplementation(() => undefined);
    vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue(commandResult('flash-1'));
    render(TerminalKeys, { agent });

    await user.click(screen.getByRole('button', { name: 'Esc' }));
    expect(screen.getByRole('button', { name: 'Esc' })).toHaveClass('key-flash');
    await user.click(screen.getByRole('button', { name: 'Ctrl' }));
    await user.click(screen.getByRole('button', { name: 'Arrow keys' }));
    expect(screen.getByRole('button', { name: 'Ctrl' })).not.toHaveClass('key-flash');
    expect(screen.getByRole('button', { name: 'Arrow keys' })).not.toHaveClass('key-flash');
    await user.click(screen.getByRole('button', { name: 'Up' }));
    expect(screen.getByRole('button', { name: 'Up' })).toHaveClass('key-flash');
  });

  it('reports a rejected paging key and drops the next queued key', async () => {
    setPagingCapability();
    const user = userEvent.setup();
    const pending = deferred<CommandResult>();
    const send = vi.spyOn(relayStore, 'sendToAgent').mockReturnValue(pending.promise);
    const toast = vi.spyOn(relayStore, 'showToast').mockImplementation(() => undefined);
    render(TerminalKeys, { agent });

    await user.click(screen.getByRole('button', { name: 'Arrow keys' }));
    await user.click(screen.getByRole('button', { name: 'Page down' }));
    await user.click(screen.getByRole('button', { name: 'Page up' }));
    pending.reject(new Error('Relay refused the key'));

    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('Key send failed'));
    expect(toast).toHaveBeenCalledWith('Relay refused the key', true);
    expect(send).toHaveBeenCalledOnce();
  });

  it('queues a second Escape press while the first key send is in flight', async () => {
    const user = userEvent.setup();
    vi.spyOn(relayStore, 'readPane').mockImplementation(() => undefined);
    const pending: Array<(result: CommandResult) => void> = [];
    const send = vi.spyOn(relayStore, 'sendToAgent').mockImplementation(() => (
      new Promise((resolve) => { pending.push(resolve); })
    ));
    const view = render(TerminalKeys, { agent });
    const escape = screen.getByRole('button', { name: 'Esc' });

    await user.click(escape);
    expect(send).toHaveBeenCalledOnce();

    await user.click(escape);
    expect(send).toHaveBeenCalledOnce();

    pending[0](commandResult('escape-1'));
    await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
    expect(send.mock.calls.map(([, command]) => command)).toEqual([
      expect.objectContaining({ type: 'send_keys', keys: ['Escape'] }),
      expect.objectContaining({ type: 'send_keys', keys: ['Escape'] }),
    ]);

    pending[1](commandResult('escape-2'));
    await waitFor(() => expect(escape.closest('.term-keys')).toHaveAttribute('aria-busy', 'false'));
    view.unmount();
  });

  it('resumes replacement-row keys when the question closes before the previous ACK', async () => {
    const user = userEvent.setup();
    vi.spyOn(relayStore, 'readPane').mockImplementation(() => undefined);
    const pending: Array<(result: CommandResult) => void> = [];
    const send = vi.spyOn(relayStore, 'sendToAgent').mockImplementation(() => (
      new Promise((resolve) => { pending.push(resolve); })
    ));
    const question: Agent = {
      ...agent, status: 'blocked', attention_kind: 'question', attention_capable: true,
      interaction: {
        id: 'question-1', kind: 'single_select', question: 'Choose an option',
        options: [{ index: 0, label: 'First' }], submit_label: 'Next',
      },
    };
    const view = render(TerminalView, { agent: question, allAgents: [question], responding: new Set<string>() });
    const firstEscape = screen.getByRole('button', { name: 'Esc' });
    await user.click(firstEscape);
    expect(send).toHaveBeenCalledOnce();

    await view.rerender({ agent, allAgents: [agent] });
    const secondEscape = screen.getByRole('button', { name: 'Esc' });
    expect(secondEscape).not.toBe(firstEscape);
    expect(secondEscape).toBeEnabled();
    await user.click(secondEscape);
    expect(send).toHaveBeenCalledOnce();
    expect(secondEscape.closest('.term-keys')).toHaveAttribute('aria-busy', 'true');

    pending[0](commandResult('escape-1'));
    await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
    expect(send.mock.calls).toEqual([
      [question, { type: 'send_keys', keys: ['Escape'], activity_label: 'Cancelled prompt' }],
      [agent, { type: 'send_keys', keys: ['Escape'], activity_label: 'Cancelled prompt' }],
    ]);
    pending[1](commandResult('escape-2'));
    await waitFor(() => expect(secondEscape.closest('.term-keys')).toHaveAttribute('aria-busy', 'false'));
    expect(send).toHaveBeenCalledTimes(2);
    view.unmount();
  });

  const questionAgent = (): Agent => ({
    ...agent, status: 'blocked', attention_kind: 'question', attention_capable: true,
    interaction: {
      id: 'question-1', kind: 'single_select', question: 'Choose an option',
      options: [{ index: 0, label: 'First' }], submit_label: 'Next',
    },
  });

  it('keeps a second Esc queued on the question row when the question closes before the first ACK', async () => {
    const user = userEvent.setup();
    vi.spyOn(relayStore, 'readPane').mockImplementation(() => undefined);
    const pending: Array<(result: CommandResult) => void> = [];
    const send = vi.spyOn(relayStore, 'sendToAgent').mockImplementation(() => (
      new Promise((resolve) => { pending.push(resolve); })
    ));
    const question = questionAgent();
    const view = render(TerminalView, { agent: question, allAgents: [question], responding: new Set<string>() });
    const escape = screen.getByRole('button', { name: 'Esc' });
    await user.click(escape);
    await user.click(escape);
    expect(send).toHaveBeenCalledOnce();

    await view.rerender({ agent, allAgents: [agent] });
    pending[0](commandResult('escape-1'));
    await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
    expect(send.mock.calls[1][1]).toEqual({ type: 'send_keys', keys: ['Escape'], activity_label: 'Cancelled prompt' });
    pending[1](commandResult('escape-2'));
    await waitFor(() => expect(document.querySelector('.term-keys')).toHaveAttribute('aria-busy', 'false'));
    view.unmount();
  });

  it('drops queued keys when the terminal view unmounts', async () => {
    const user = userEvent.setup();
    vi.spyOn(relayStore, 'readPane').mockImplementation(() => undefined);
    const pending: Array<(result: CommandResult) => void> = [];
    const send = vi.spyOn(relayStore, 'sendToAgent').mockImplementation(() => (
      new Promise((resolve) => { pending.push(resolve); })
    ));
    const question = questionAgent();
    const view = render(TerminalView, { agent: question, allAgents: [question], responding: new Set<string>() });
    const escape = screen.getByRole('button', { name: 'Esc' });
    await user.click(escape);
    await user.click(escape);
    view.unmount();
    pending[0](commandResult('escape-1'));
    await new Promise((r) => setTimeout(r, 100));
    expect(send).toHaveBeenCalledOnce();
  });

  it('arms a modifier and queues its chord while a key send is in flight', async () => {
    const user = userEvent.setup();
    vi.spyOn(relayStore, 'readPane').mockImplementation(() => undefined);
    const pending: Array<(result: CommandResult) => void> = [];
    const send = vi.spyOn(relayStore, 'sendToAgent').mockImplementation(() => (
      new Promise((resolve) => { pending.push(resolve); })
    ));
    const view = render(TerminalKeys, { agent });

    await user.click(screen.getByRole('button', { name: 'Esc' }));
    expect(send).toHaveBeenCalledOnce();

    await user.click(screen.getByRole('button', { name: 'Ctrl' }));
    await user.type(screen.getByRole('textbox', { name: 'Modifier shortcut character' }), 'c');
    expect(send).toHaveBeenCalledOnce();

    pending[0](commandResult('escape-1'));
    await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
    expect(send.mock.calls[1][1]).toEqual(expect.objectContaining({ type: 'send_keys', keys: ['ctrl+c'] }));

    pending[1](commandResult('chord-1'));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Esc' }).closest('.term-keys')).toHaveAttribute('aria-busy', 'false'));
    view.unmount();
  });
});
