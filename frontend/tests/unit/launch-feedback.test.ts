import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import LaunchView from '$components/LaunchView.svelte';
import { replaceView } from '$lib/router';
import { relayStore } from '$lib/store';
import type { Agent, CommandResult, RelayConnectionView } from '$lib/types';

vi.mock('$lib/router', async original => ({ ...await original<typeof import('$lib/router')>(), replaceView: vi.fn() }));

const ready = { status: 'connected', inventory: { state: 'ready', stale: false }, capabilities: ['directory_browser'], agentProfiles: [{ id: 'hermes', label: 'Hermes' }] } as RelayConnectionView;
const result: CommandResult = { type: 'command_result', request_id: 'launch', ok: true, phase: 'completed', data: { pane_id: 'created', name: 'development-hermes', cwd: '/fixture/Development' } };
let resolve: (result: CommandResult) => void;
let reject: (error: Error) => void;

beforeEach(() => {
  vi.useFakeTimers();
  vi.mocked(replaceView).mockClear();
  relayStore.relayConfigs.set(['computer', 'other'].map(id => ({ id, label: id, url: `wss://${id}.invalid`, token: '' })));
  relayStore.connections.set(new Map([['computer', ready], ['other', ready]]) as never);
  vi.spyOn(relayStore, 'listDirectories').mockResolvedValue({ current: { path: '/fixture/Development', label: 'Development' }, parent: '/fixture', directories: [] });
  vi.spyOn(relayStore, 'showToast').mockImplementation(() => {});
  vi.spyOn(relayStore, 'requestAgents').mockImplementation(() => {});
  vi.spyOn(relayStore, 'sendCommand').mockImplementation(() => new Promise((yes, no) => { resolve = yes; reject = no; }));
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  relayStore.connections.set(new Map());
  relayStore.relayConfigs.set([]);
  relayStore.agents.set([]);
});
async function start() {
  const view = render(LaunchView, { relayId: 'computer', cwd: '/fixture/Development' });
  await vi.advanceTimersByTimeAsync(0);
  await fireEvent.submit(screen.getByRole('button', { name: 'Start Agent' }).closest('form')!);
  return view;
}

it.each(['absent', 'unknown-profile', 'delayed-session', 'wrong-pane', 'wrong-session', 'wrong-terminal'])('returns to inventory on the receipt without guessing a target from %s metadata', async metadata => {
  await start();
  relayStore.agents.set(metadata === 'absent' ? [] : [{ relay_id: 'computer', pane_id: 'computer::created', raw_pane_id: metadata === 'wrong-pane' ? 'unrelated' : 'created', server_session_id: metadata === 'wrong-session' ? 'other-server' : 'primary', terminal_id: metadata === 'wrong-terminal' ? 'replacement' : 'terminal', generation: 1, agent_session_id: metadata === 'delayed-session' ? '' : 'session', name: 'development-hermes', cwd: '/fixture/Development', agent: metadata === 'unknown-profile' ? undefined : 'hermes' } as Agent]);
  resolve(result);
  await vi.advanceTimersByTimeAsync(0);
  expect(replaceView).toHaveBeenCalledExactlyOnceWith({ view: 'agents' });
  expect(relayStore.requestAgents).toHaveBeenCalledOnce();
  expect(relayStore.showToast).toHaveBeenCalledWith('Start confirmed. Refreshing agent list.', false);
  await vi.advanceTimersByTimeAsync(60_000);
  expect(relayStore.sendCommand).toHaveBeenCalledOnce();
});

it('does not submit a duplicate while a launch is pending', async () => {
  await start();
  await fireEvent.submit(screen.getByRole('button', { name: 'Start Agent' }).closest('form')!);
  expect(relayStore.sendCommand).toHaveBeenCalledOnce();
  expect(screen.getByText('Starting agent…')).toBeVisible();
  resolve(result);
});

it.each(['unmount', 'relay-change', 'disconnect', 'reader'] as const)('fences late completion after %s without replay', async change => {
  const view = await start();
  const signal = vi.mocked(relayStore.sendCommand).mock.calls[0][4];
  if (change === 'unmount') view.unmount();
  if (change === 'relay-change') await fireEvent.change(screen.getByLabelText('Computer'), { target: { value: 'other' } });
  if (change === 'disconnect') relayStore.connections.set(new Map([['other', ready]]) as never);
  if (change === 'reader') await view.rerender({ readOnlyRelayIds: new Set(['computer']) });
  await vi.advanceTimersByTimeAsync(0);
  expect(signal?.aborted).toBe(true);
  resolve(result);
  await vi.advanceTimersByTimeAsync(60_000);
  expect(replaceView).not.toHaveBeenCalled();
  expect(relayStore.sendCommand).toHaveBeenCalledOnce();
  if (change !== 'unmount') expect(screen.queryByText('Starting agent…')).not.toBeInTheDocument();
});

it.each([false, true])('retains honest actionable failure (unknown=%s)', async unknown => {
  await start();
  reject(Object.assign(new Error(unknown ? 'Relay confirmation timed out' : 'Synthetic Herdr refusal'), { data: unknown ? { dispatched_unknown: true } : { not_started: true } }));
  await vi.advanceTimersByTimeAsync(0);
  expect(screen.getByText(/Check Agents before starting again/)).toHaveClass('error');
  expect(screen.getByLabelText('Name')).toHaveValue('development-hermes');
  expect(screen.getByRole('button', { name: 'Start Agent' })).not.toBeDisabled();
  expect(replaceView).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole('button', { name: 'View agents' }));
  expect(replaceView).toHaveBeenCalledExactlyOnceWith({ view: 'agents' });
  expect(relayStore.sendCommand).toHaveBeenCalledOnce();
});

it('allows leaving a pending launch without claiming to cancel the desktop launch', async () => {
  await start();
  const signal = vi.mocked(relayStore.sendCommand).mock.calls[0][4];
  await fireEvent.click(screen.getByRole('button', { name: 'View agents' }));
  expect(signal?.aborted).toBe(true);
  expect(replaceView).toHaveBeenCalledExactlyOnceWith({ view: 'agents' });
  resolve(result);
  await vi.advanceTimersByTimeAsync(60_000);
  expect(replaceView).toHaveBeenCalledTimes(1);
  expect(relayStore.sendCommand).toHaveBeenCalledOnce();
});

it('preserves the initial-prompt warning without calling it ready', async () => {
  await start();
  resolve({ ...result, phase: 'completed_with_warning', data: { ...result.data, warning: 'Agent started, but the initial prompt was not confirmed' } });
  await vi.advanceTimersByTimeAsync(0);
  expect(relayStore.showToast).toHaveBeenCalledWith('Agent started, but the initial prompt was not confirmed', true);
  expect(replaceView).toHaveBeenCalledExactlyOnceWith({ view: 'agents' });
});

it('refuses forced read-only submissions', async () => {
  render(LaunchView, { relayId: 'computer', cwd: '/fixture/Development', readOnlyRelayIds: new Set(['computer']) });
  await vi.advanceTimersByTimeAsync(0);
  await fireEvent.submit(screen.getByRole('button', { name: 'Start Agent' }).closest('form')!);
  expect(relayStore.sendCommand).not.toHaveBeenCalled();
});
