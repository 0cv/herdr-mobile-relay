import { render, screen, waitFor, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import TerminalKeys from '$components/TerminalKeys.svelte';
import TerminalView from '$components/TerminalView.svelte';
import { setTerminalKeyRow } from '$lib/preferences';
import { defaultKeyRow } from '$lib/terminal-key-row';
import { targetRefForAgent } from '$lib/resource-id';
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
    setTerminalKeyRow(defaultKeyRow);
  });

  it('sends fixed chords exactly and does not consume Alt', async () => {
    setTerminalKeyRow(['alt', 'ctrl+c', 'shift+tab']);
    const user = userEvent.setup();
    const send = vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue(commandResult('fixed'));
    render(TerminalKeys, { agent });
    const alt = screen.getByRole('button', { name: 'Alt' });
    await user.click(alt);
    await user.click(screen.getByRole('button', { name: 'Ctrl+C' }));
    expect(send).toHaveBeenCalledExactlyOnceWith(agent, { type: 'send_keys', keys: ['ctrl+c'], activity_label: 'Ctrl+C' });
    expect(alt).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('textbox', { name: 'Modifier shortcut character' })).toHaveFocus();

    await user.click(screen.getByRole('button', { name: 'Shift+Tab' }));
    expect(send).toHaveBeenCalledTimes(2);
    expect(send).toHaveBeenLastCalledWith(agent, { type: 'send_keys', keys: ['shift+tab'], activity_label: 'Shift+Tab' });
    expect(alt).toHaveAttribute('aria-pressed', 'true');
  });

  it('sends a direct arrow with armed modifiers and disarms a removed toggle', async () => {
    setTerminalKeyRow(['Up', 'ctrl']);
    const user = userEvent.setup();
    const send = vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue(commandResult('arrow'));
    render(TerminalKeys, { agent });
    const up = screen.getByRole('button', { name: 'Up' });
    await user.click(screen.getByRole('button', { name: 'Ctrl' }));
    await user.click(up);
    expect(send).toHaveBeenCalledExactlyOnceWith(agent, { type: 'send_keys', keys: ['ctrl+up'], activity_label: 'Ctrl+Up' });

    setTerminalKeyRow(['Up']);
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Ctrl' })).not.toBeInTheDocument());
    await user.click(up);
    expect(send).toHaveBeenCalledTimes(2);
    expect(send).toHaveBeenLastCalledWith(agent, { type: 'send_keys', keys: ['Up'], activity_label: 'Up' });
  });

  it('still sends detected key hints with an empty row', async () => {
    setTerminalKeyRow([]);
    const send = vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue(commandResult('hint'));
    render(TerminalView, {
      agent, allAgents: [agent], responding: new Set<string>(),
      frame: {
        paneId: agent.pane_id, target: targetRefForAgent(agent), format: 'plain', receivedAt: performance.now(),
        content: 'Choose a model\nCurrent: balanced\n↑/↓ to navigate · Enter to select · Esc to cancel',
      },
    });
    expect(document.querySelectorAll('.term-keys button')).toHaveLength(0);
    const hints = screen.getByRole('region', { name: /^Terminal menu:/ });
    await userEvent.click(within(hints).getByRole('button', { name: /Select/ }));
    expect(send).toHaveBeenCalledExactlyOnceWith(agent, { type: 'send_keys', keys: ['Enter'], activity_label: 'Select' });
  });

  it.each([{ readOnly: true }, { blocked: true }])('disables all entries with %j', async (restriction) => {
    setTerminalKeyRow([...defaultKeyRow, 'Up', 'ctrl+c', 'shift+tab']);
    const send = vi.spyOn(relayStore, 'sendToAgent');
    const user = userEvent.setup();
    render(TerminalKeys, { agent, ...restriction });
    const buttons = screen.getAllByRole('button');
    expect(buttons).toHaveLength(11);
    for (const button of buttons) {
      expect(button).toBeDisabled();
      await user.click(button);
    }
    expect(screen.queryByRole('group')).not.toBeInTheDocument();
    expect(send).not.toHaveBeenCalled();
  });

  it('closes an open menu when the saved key order changes', async () => {
    render(TerminalKeys, { agent });
    const trigger = screen.getByRole('button', { name: 'Function keys' });
    await userEvent.click(trigger);
    expect(screen.getByRole('group', { name: 'Function keys' })).toBeVisible();

    setTerminalKeyRow(['fkeys', ...defaultKeyRow.filter((id) => id !== 'fkeys')]);
    await waitFor(() => expect(trigger).toHaveAttribute('aria-expanded', 'false'));
    expect(screen.getAllByRole('button')[0]).toBe(trigger);
    expect(screen.queryByRole('group', { name: 'Function keys' })).not.toBeInTheDocument();
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

  it('applies the device layout to questions and keeps Keyboard with an empty row', async () => {
    setTerminalKeyRow(['ctrl+c']);
    const question = questionAgent();
    render(TerminalView, { agent: question, allAgents: [question], responding: new Set<string>() });
    const row = within(document.querySelector<HTMLElement>('.term-keys')!);
    expect(row.getByRole('button', { name: 'Ctrl+C' })).toBeVisible();
    expect(row.getByRole('button', { name: 'Keyboard' })).toBeVisible();
    setTerminalKeyRow([]);
    await waitFor(() => expect(row.getAllByRole('button')).toHaveLength(1));
    expect(row.getByRole('button', { name: 'Keyboard' })).toBeVisible();
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
