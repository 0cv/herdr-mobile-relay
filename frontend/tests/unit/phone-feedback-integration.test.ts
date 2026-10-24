import { fireEvent, render, screen } from '@testing-library/svelte';
import { get } from 'svelte/store';
import { tick } from 'svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import ConversationHistory from '$components/ConversationHistory.svelte';
import TerminalView from '$components/TerminalView.svelte';
import { relayStore } from '$lib/store';
import { securityState } from '$lib/security';
import { clearConversationPreviews } from '$lib/conversation-cache';
import { piPromptDelivery } from '$lib/pi-prompt.svelte';
import { readTerminalResizeSession, terminalHeightLease } from '$lib/preferences';
import type { TransportHandlers } from '$lib/transports';
import type { Agent, TerminalFrame } from '$lib/types';

const attempts = vi.hoisted(() => [] as {
  handlers: TransportHandlers;
  sent: Record<string, unknown>[];
  answerPing: boolean;
  closed: boolean;
}[]);
vi.mock('$lib/transports', async (original) => ({
  ...await original<typeof import('$lib/transports')>(),
  createRelayTransport: (_relay: unknown, handlers: TransportHandlers) => {
    const attempt = { handlers, sent: [] as Record<string, unknown>[], answerPing: true, closed: false };
    attempts.push(attempt);
    return {
      kind: 'websocket',
      connect: () => handlers.onStatus('connected'),
      close: () => { attempt.closed = true; },
      send: (payload: Record<string, unknown>) => {
        attempt.sent.push(payload);
        if (payload.type === 'ping' && attempt.answerPing) queueMicrotask(() => handlers.onMessage({ type: 'pong' }));
        return !attempt.closed;
      },
    };
  },
}));

let agent: Agent;
const wireAgent = (generation = 1) => ({ pane_id: 'pane', server_session_id: 'primary', terminal_id: 'terminal', generation, agent_session_id: 'session', agent: 'pi', project: 'Integrated', cwd: '/synthetic', status: 'idle' });
const rows = (start: number, end: number) => Array.from({ length: end - start }, (_, index) => ({ id: `row-${start + index}`, role: (start + index) % 2 ? 'assistant' : 'user', timestamp: '', text: `record ${start + index}` }));
const current = () => attempts.at(-1)!;
const requests = (type: string, attempt = current()) => attempt.sent.filter(payload => payload.type === type);
function complete(request: Record<string, unknown>, data: unknown, attempt = current()) {
  attempt.handlers.onMessage({ type: 'command_result', request_id: request.request_id, ok: true, phase: 'completed', data });
}
function history(request: Record<string, unknown>, start = 24, end = 48, cursor = 'older', attempt = current()) {
  complete(request, { available: true, reason: '', entries: rows(start, end), has_more: Boolean(cursor), next_cursor: cursor, total: null, source_revision: 'revision', state: 'ready', mode: 'recent' }, attempt);
}
function admit(generation = 1) {
  current().handlers.onMessage({ type: 'push_config', protocol: 3, capabilities: ['manual_terminal_input', 'conversation_history'], inventory: { state: 'ready' } });
  current().handlers.onMessage({ type: 'agents', agents: [wireAgent(generation)] });
  return get(relayStore.agents)[0];
}
function frame(): TerminalFrame {
  relayStore.readPane(agent, true);
  const request = requests('read_pane').at(-1)!;
  current().handlers.onMessage({ type: 'pane_content', request_id: request.request_id, pane_id: 'pane', target: request.target, content: '> ', format: 'ansi', no_echo: false });
  return get(relayStore.terminalFrames).get(agent.pane_id)!;
}
async function flush() { await vi.advanceTimersByTimeAsync(0); await tick(); }
function fenced() {
  expect(screen.getByLabelText('Prompt')).toBeEnabled();
  expect(screen.getByRole('button', { name: 'Send prompt' })).toBeDisabled();
  expect(screen.getByRole('button', { name: 'Attach files' })).toBeDisabled();
  expect(screen.queryByRole('button', { name: 'Type in terminal' })).not.toBeInTheDocument();
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal('requestAnimationFrame', () => 1);
  vi.stubGlobal('cancelAnimationFrame', () => {});
  relayStore.destroy();
  relayStore.relayConfigs.set([]);
  attempts.length = 0;
  localStorage.clear();
  securityState.set({ locked: false } as never);
  clearConversationPreviews();
  relayStore.addRelay({ label: 'Integrated', url: 'wss://synthetic.invalid', token: '' });
  agent = admit();
});
afterEach(() => {
  relayStore.destroy();
  relayStore.relayConfigs.set([]);
  clearConversationPreviews();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

it('keeps pending and uncertain literal delivery fenced while paging and switching views without blocking edits or health', async () => {
  const terminal = render(TerminalView, { agent, allAgents: [agent], responding: new Set<string>() });
  await fireEvent.click(screen.getByRole('button', { name: 'Type in terminal' }));
  await vi.waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
  await terminal.rerender({ frame: frame() });
  await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'literal draft' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Send text' }));
  expect(requests('send_terminal_text')).toHaveLength(1);
  const literal = requests('send_terminal_text')[0];
  expect(literal).toMatchObject({ text: 'literal draft', target: { generation: 1, agent_session_id: 'session' } });
  terminal.unmount();
  const conversation = render(ConversationHistory, { agent });
  await flush();
  const initial = requests('get_conversation_history').at(-1)!;
  expect(initial.limit).toBe(24);
  history(initial);
  await flush();
  expect(screen.getByText('record 47')).toBeVisible();
  fenced();
  expect(screen.getByText('Waiting for terminal input delivery.')).toBeVisible();
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'editable during delivery' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Load older turns' }));
  const older = requests('get_conversation_history').at(-1)!;
  expect(older.cursor).toBe('older');
  relayStore.revalidateConnections(2_000);
  await vi.advanceTimersByTimeAsync(2_001);
  expect(attempts).toHaveLength(1);
  expect(requests('ping')).toHaveLength(1);
  current().handlers.onMessage({ type: 'command_result', request_id: literal.request_id, ok: false, phase: 'dispatched_unknown', error: 'unknown outcome' });
  history(older, 0, 24, '');
  await flush();
  expect(screen.getByText('record 12')).toBeVisible();
  expect(conversation.container.querySelectorAll('.conversation-entry')).toHaveLength(24);
  expect(screen.getByRole('alert')).toHaveTextContent('Delivery is uncertain.');
  expect(screen.getByLabelText('Prompt')).toHaveValue('editable during delivery');
  fenced();
  await fireEvent.keyDown(screen.getByLabelText('Prompt'), { key: 'Enter', ctrlKey: true });
  expect(requests('submit_prompt')).toHaveLength(0);
  conversation.unmount();
  const restored = render(TerminalView, { agent, allAgents: [agent], responding: new Set<string>() });
  await fireEvent.click(screen.getByRole('button', { name: 'Type in terminal' }));
  await vi.waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
  expect(screen.getByLabelText('Terminal text')).toHaveValue('');
  expect(screen.getByRole('button', { name: 'Enter' })).toBeDisabled();
  await restored.rerender({ frame: frame() });
  expect(screen.getByRole('button', { name: 'I checked the terminal' })).toBeEnabled();
  expect(requests('send_terminal_text')).toHaveLength(1);
  expect(requests('send_keys')).toHaveLength(0);
  expect(JSON.stringify(localStorage)).not.toContain('literal draft');
});

it('retains Pi uncertainty through notice paging, mode/view switches, lock and reconnect without a footer or resend', async () => {
  expect(readTerminalResizeSession()).toBe(true);
  expect(get(terminalHeightLease)).toBe(false);
  const delivery = piPromptDelivery(agent);
  expect(delivery.begin()).toBe(true);
  delivery.finish(true, 5000);
  const view = render(ConversationHistory, { agent });
  await flush();
  const request = requests('get_conversation_history').at(-1)!;
  const notice = { id: 'pi:notice', role: 'assistant' as const, kind: 'notice' as const, timestamp: '', text: 'A generic extension asks for input' };
  const unrelated = { id: 'pi:unrelated-user', role: 'user' as const, timestamp: new Date(5001).toISOString(), text: '/answer once' };
  const capabilities = { source: 'pi_events', active_branch: true };
  complete(request, { available: true, capabilities, entries: [notice, unrelated, ...rows(24, 45)], has_more: true, next_cursor: 'older', total: null, source_revision: 'revision', state: 'ready', mode: 'recent' });
  await flush();
  expect(screen.getByText(notice.text)).toBeVisible();
  expect(screen.getByText(unrelated.text)).toBeVisible();
  expect(delivery.uncertain).toBe(true);
  expect(view.container.querySelector('.agent-progress')).toBeNull();
  fenced();
  await fireEvent.click(screen.getByRole('button', { name: 'Load older turns' }));
  const older = requests('get_conversation_history').at(-1)!;
  complete(older, { available: true, entries: [{ ...notice, id: 'pi:older', text: 'Earlier generic notice' }, ...rows(0, 22)], has_more: false, next_cursor: '', total: null, source_revision: 'revision', snapshot_id: 'snapshot', state: 'ready', mode: 'snapshot' });
  await flush();
  expect(screen.getByText('Earlier generic notice')).toBeVisible();
  await fireEvent.click(screen.getByRole('button', { name: /^Full history$/ })); await flush();
  expect(screen.getByText(notice.text)).toBeVisible();
  expect(delivery.uncertain).toBe(true);
  view.unmount();
  const terminal = render(TerminalView, { agent, allAgents: [agent], responding: new Set<string>() });
  await fireEvent.click(screen.getByRole('button', { name: 'Type in terminal' }));
  await flush();
  expect(screen.getByRole('button', { name: 'Send text' })).toBeDisabled();
  securityState.set({ locked: true } as never); await flush();
  relayStore.setHidden(true); relayStore.setHidden(false);
  expect(delivery.uncertain).toBe(true);
  expect(piPromptDelivery({ ...agent, generation: 2 }).uncertain).toBe(false);
  expect(delivery.uncertain).toBe(true);
  terminal.unmount();
  securityState.set({ locked: false } as never);
  render(ConversationHistory, { agent }); await flush();
  await fireEvent.click(screen.getByRole('button', { name: 'I checked — allow another send' }));
  expect(delivery.uncertain).toBe(false);
  expect(requests('send_pi_prompt_unconfirmed')).toHaveLength(0);
  expect(requests('send_terminal_text')).toHaveLength(0);
  expect(requests('submit_prompt')).toHaveLength(0);
  expect(requests('send_keys')).toHaveLength(0);
  await fireEvent.click(screen.getByRole('button', { name: /^Conversation$/ }));
});

it('recovers genuine health loss while rejecting retired history, callbacks and literal replay after target replacement', async () => {
  const view = render(ConversationHistory, { agent });
  await flush();
  const retired = current();
  const oldHistory = requests('get_conversation_history').at(-1)!;
  retired.answerPing = false;
  relayStore.revalidateConnections(2_000);
  await vi.advanceTimersByTimeAsync(2_001);
  expect(retired.closed).toBe(true);
  expect(attempts).toHaveLength(2);
  const replacement = admit(2);
  await view.rerender({ agent: replacement });
  await flush();
  const fresh = requests('get_conversation_history').at(-1)!;
  expect(fresh.target).toMatchObject({ generation: 2 });
  history(oldHistory, 0, 24, '', retired);
  retired.handlers.onStatus('connected');
  retired.handlers.onMessage({ type: 'agents', agents: [wireAgent()] });
  history(fresh, 100, 124, '');
  await flush();
  expect(screen.getByText('record 123')).toBeVisible();
  expect(screen.queryByText('record 0')).not.toBeInTheDocument();
  expect(get(relayStore.agents)[0].generation).toBe(2);
  expect(requests('send_terminal_text')).toHaveLength(0);
  expect(requests('submit_prompt')).toHaveLength(0);
  expect(get(relayStore.connections).get(replacement.relay_id)?.status).toBe('connected');
});
