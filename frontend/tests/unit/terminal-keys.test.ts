import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import TerminalKeys from '$components/TerminalKeys.svelte';
import TerminalView from '$components/TerminalView.svelte';
import { relayStore } from '$lib/store';
import type { Agent, CommandResult } from '$lib/types';

const agent: Agent = {
  relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: 'fedora::w1:p1',
  server_session_id: 'primary', terminal_id: 'terminal', generation: 1,
  project: 'relay', agent: 'claude', status: 'working',
};

function commandResult(requestId: string): CommandResult {
  return { type: 'command_result', request_id: requestId, ok: true };
}

describe('TerminalKeys', () => {
  afterEach(() => {
    vi.restoreAllMocks();
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
