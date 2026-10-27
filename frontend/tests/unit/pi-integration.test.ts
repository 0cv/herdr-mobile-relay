import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import PiIntegration from '$components/PiIntegration.svelte';
import ConversationHistory from '$components/ConversationHistory.svelte';
import { relayStore } from '$lib/store';
import { securityState } from '$lib/security';
import type { Agent, RelayConnectionView } from '$lib/types';

const connection = { status: 'connected', capabilities: ['pi_integration'] } as RelayConnectionView;
const state = (enabled = false): { instance: string; enabled: boolean; profiles: { path: string; status: string; reason?: string }[] } => ({ instance: 'synthetic-relay', enabled, profiles: [{ path: '/synthetic/agent', status: enabled ? 'up to date' : 'not installed' }] });
const response = (data = state()) => ({ type: 'command_result' as const, request_id: 'test', ok: true, data });

beforeEach(() => {
  securityState.set({ locked: false, busy: false, reason: 'open', status: '', hint: '' });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); relayStore.connections.set(new Map()); });

it('reads relay disk state and toggles only that computer without phone paths', async () => {
  const send = vi.spyOn(relayStore, 'sendCommand').mockResolvedValueOnce(response(state(true))).mockResolvedValueOnce(response(state(false)));
  render(PiIntegration, { relayId: 'computer', label: 'Computer', connection });
  const toggle = await screen.findByRole('switch', { name: 'Pi Integration' });
  await waitFor(() => expect(toggle).toBeChecked());
  expect(send).toHaveBeenCalledWith('computer', { type: 'get_pi_integration' });
  expect(screen.getByText(/Reload Pi to activate changes/)).toBeInTheDocument();
  await fireEvent.click(toggle);
  await waitFor(() => expect(toggle).not.toBeChecked());
  expect(send).toHaveBeenLastCalledWith('computer', { type: 'set_pi_integration', instance: 'synthetic-relay', enabled: false });
});

it('defaults to off until disk state arrives and reports per-profile refusals', async () => {
  let complete!: (value: ReturnType<typeof response>) => void;
  vi.spyOn(relayStore, 'sendCommand').mockReturnValue(new Promise(resolve => { complete = resolve; }));
  render(PiIntegration, { relayId: 'computer', label: 'Computer', connection });
  const toggle = screen.getByRole('switch', { name: 'Pi Integration' });
  expect(toggle).not.toBeChecked();
  expect(toggle).toBeDisabled();
  complete(response({ ...state(), profiles: [{ path: '/synthetic/agent', status: 'refused', reason: 'Unowned directory' }] }));
  await waitFor(() => expect(toggle).toBeEnabled());
  expect(screen.getByText('/synthetic/agent: refused — Unowned directory')).toBeInTheDocument();
});

it('enables all relay-resolved profiles with an explicit controller toggle', async () => {
  const send = vi.spyOn(relayStore, 'sendCommand').mockResolvedValueOnce(response()).mockResolvedValueOnce(response(state(true)));
  render(PiIntegration, { relayId: 'computer', label: 'Computer', connection });
  const toggle = screen.getByRole('switch', { name: 'Pi Integration' });
  await waitFor(() => expect(toggle).toBeEnabled());
  await fireEvent.click(toggle);
  await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
  expect(send).toHaveBeenLastCalledWith('computer', { type: 'set_pi_integration', instance: 'synthetic-relay', enabled: true });
  expect(toggle).toBeChecked();
});

it('shows readers the relay state without enabling computer-side writes', async () => {
  const send = vi.spyOn(relayStore, 'sendCommand').mockResolvedValue(response(state(true)));
  render(PiIntegration, { relayId: 'computer', label: 'Computer', connection, readOnly: true });
  const toggle = screen.getByRole('switch', { name: 'Pi Integration' });
  await waitFor(() => expect(toggle).toBeChecked());
  expect(toggle).toBeDisabled();
  await fireEvent.click(toggle);
  expect(send).toHaveBeenCalledTimes(1);
  expect(screen.getByText('Controller access required to change this setting.')).toBeInTheDocument();
});

it('keeps refused installs off rather than showing the requested state', async () => {
  vi.spyOn(relayStore, 'sendCommand').mockResolvedValueOnce(response()).mockResolvedValueOnce(response({ ...state(), profiles: [{ path: '/synthetic/agent', status: 'refused', reason: 'Unowned directory' }] }));
  render(PiIntegration, { relayId: 'computer', label: 'Computer', connection });
  const toggle = screen.getByRole('switch', { name: 'Pi Integration' });
  await waitFor(() => expect(toggle).toBeEnabled());
  await fireEvent.click(toggle);
  await waitFor(() => expect(toggle).not.toBeChecked());
  expect(screen.getByText('/synthetic/agent: refused — Unowned directory')).toBeInTheDocument();
});

it('does not resend a failed toggle and disables stale state on disconnect', async () => {
  const send = vi.spyOn(relayStore, 'sendCommand').mockResolvedValueOnce(response()).mockRejectedValueOnce(new Error('Connection lost'));
  const view = render(PiIntegration, { relayId: 'computer', label: 'Computer', connection });
  const toggle = screen.getByRole('switch', { name: 'Pi Integration' });
  await waitFor(() => expect(toggle).toBeEnabled());
  await fireEvent.click(toggle);
  expect(await screen.findByRole('alert')).toHaveTextContent('Connection lost');
  expect(toggle).toBeDisabled();
  await view.rerender({ connection: { ...connection, status: 'disconnected' } });
  expect(toggle).not.toBeChecked();
  expect(send).toHaveBeenCalledTimes(2);
});

it('ignores a pre-toggle disk read that settles after the mutation', async () => {
  vi.useFakeTimers();
  try {
    let complete!: (value: ReturnType<typeof response>) => void;
    const send = vi.spyOn(relayStore, 'sendCommand').mockResolvedValueOnce(response())
      .mockReturnValueOnce(new Promise(resolve => { complete = resolve; }))
      .mockResolvedValueOnce(response(state(true)));
    render(PiIntegration, { relayId: 'computer', label: 'Computer', connection });
    await vi.advanceTimersByTimeAsync(0);
    const toggle = screen.getByRole('switch', { name: 'Pi Integration' });
    expect(toggle).toBeEnabled();
    await vi.advanceTimersByTimeAsync(10_000);
    await fireEvent.click(toggle);
    await vi.advanceTimersByTimeAsync(0);
    expect(toggle).toBeChecked();
    complete(response());
    await vi.advanceTimersByTimeAsync(0);
    expect(toggle).toBeChecked();
    expect(send).toHaveBeenCalledTimes(3);
  } finally { vi.useRealTimers(); }
});

it('discards old-manager reads when the relay target changes', async () => {
  let complete!: (value: ReturnType<typeof response>) => void;
  const send = vi.spyOn(relayStore, 'sendCommand')
    .mockReturnValueOnce(new Promise(resolve => { complete = resolve; }))
    .mockResolvedValueOnce(response({ ...state(), instance: 'second-manager' }));
  const view = render(PiIntegration, { relayId: 'computer', label: 'Computer', connection });
  await waitFor(() => expect(send).toHaveBeenCalledTimes(1));
  await view.rerender({ relayId: 'second', label: 'Second' });
  const toggle = screen.getByRole('switch', { name: 'Pi Integration' });
  await waitFor(() => expect(toggle).toBeEnabled());
  complete(response(state(true)));
  await fireEvent.focus(toggle);
  expect(toggle).not.toBeChecked();
  expect(send).toHaveBeenLastCalledWith('second', { type: 'get_pi_integration' });
});

it('does not accept a read from a retired connection', async () => {
  let complete!: (value: ReturnType<typeof response>) => void;
  const send = vi.spyOn(relayStore, 'sendCommand').mockReturnValueOnce(new Promise(resolve => { complete = resolve; }));
  const view = render(PiIntegration, { relayId: 'computer', label: 'Computer', connection });
  await waitFor(() => expect(send).toHaveBeenCalledTimes(1));
  await view.rerender({ connection: { ...connection, status: 'disconnected' } });
  complete(response(state(true)));
  const toggle = screen.getByRole('switch', { name: 'Pi Integration' });
  await fireEvent.focus(toggle);
  expect(toggle).not.toBeChecked();
  expect(toggle).toBeDisabled();
  expect(send).toHaveBeenCalledTimes(1);
});

it('points a blocked Pi Conversation without capability to the setting without sending', async () => {
  const agent: Agent = { relay_id: 'computer', relay_label: 'Computer', raw_pane_id: 'pane', pane_id: 'computer::pane', server_session_id: 'primary', terminal_id: 'terminal', generation: 1, agent_session_id: 'session', agent: 'pi', status: 'blocked', attention_kind: 'unknown', attention_capable: true };
  relayStore.connections.set(new Map([['computer', connection]]) as never);
  vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({ available: true, reason: '', entries: [], hasMore: false, total: 0 });
  vi.spyOn(relayStore, 'loadSlashCommands').mockResolvedValue({ commands: [], truncated: false });
  vi.spyOn(relayStore, 'readPane').mockImplementation(() => {});
  const send = vi.spyOn(relayStore, 'sendToAgent');
  render(ConversationHistory, { agent });
  expect(await screen.findByText('For live Pi replies, enable Settings → Pi Integration, then /reload Pi.')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Send prompt' })).toBeDisabled();
  expect(send).not.toHaveBeenCalled();
});
