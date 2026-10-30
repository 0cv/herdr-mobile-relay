import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { get } from 'svelte/store';
import { tick } from 'svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import PromptComposer from '$components/PromptComposer.svelte';
import { relayStore } from '$lib/store';
import { securityState } from '$lib/security';
import { PiPromptDelivery, piPromptDelivery, piPromptTarget, validPiPromptText } from '$lib/pi-prompt.svelte';
import { promptDispatchRestriction } from '$lib/terminal-input';
import { targetRefForAgent, targetStoreKey } from '$lib/resource-id';
import type { Agent } from '$lib/types';

class Socket {
  static instances: Socket[] = [];
  static OPEN = 1;
  readyState = 0;
  sent: Record<string, any>[] = [];
  onopen?: () => void;
  onmessage?: (event: { data: string }) => void;
  onclose?: () => void;
  constructor() { Socket.instances.push(this); }
  send(value: string) {
    const command = JSON.parse(value);
    this.sent.push(command);
    if (command.type === 'get_pi_prompt_capability') queueMicrotask(() => {
      now++;
      this.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'completed', data: {
        capability: 'pi_prompt_unconfirmed', lease: { instance: 'a'.repeat(64), pane: 'pane', session: agent.agent_session_id,
          pid: 42, incarnation: 'b'.repeat(36), token: 'c'.repeat(36), connection: 'connection', target_key: 'exact-target', expires_at: Date.now() + 15000 },
      } });
    });
    if (command.type === 'read_pane') queueMicrotask(() => this.message({ type: 'pane_content', pane_id: 'pane', request_id: command.request_id,
      target: command.target, content: 'Unrecognized dialog', no_echo: false }));
  }
  close() { this.readyState = 3; }
  open() { this.readyState = 1; this.onopen?.(); }
  message(value: unknown) { this.onmessage?.({ data: JSON.stringify(value) }); }
}
let agent: Agent;
let socket: Socket;
let now = 1000;
let generation = 100;
const commands = (type: string) => socket.sent.filter(command => command.type === type);
const sends = () => commands('send_pi_prompt_unconfirmed');
const input = () => screen.getByLabelText('Prompt');
const submit = () => fireEvent.submit(screen.getByRole('form', { name: 'Send a prompt' }));
async function mount() {
  const view = render(PromptComposer, { agent, dispatchBlocked: promptDispatchRestriction(agent), draftPersistenceAllowed: true });
  await vi.advanceTimersByTimeAsync(0);
  let frame = get(relayStore.terminalFrames).get(agent.pane_id)!;
  await view.rerender({ frame });
  await vi.advanceTimersByTimeAsync(0);
  frame = get(relayStore.terminalFrames).get(agent.pane_id)!;
  await view.rerender({ frame });
  await tick();
  return { view, frame };
}
function result(phase = 'unconfirmed', ok = true) {
  socket.message({ type: 'command_result', request_id: sends().at(-1)!.request_id, ok, phase, error: ok ? '' : 'Synthetic refusal', data: { sent_at: 5000 } });
}

beforeEach(() => {
  vi.useFakeTimers();
  now += 10000;
  vi.spyOn(performance, 'now').mockImplementation(() => now);
  localStorage.clear(); sessionStorage.clear();
  Socket.instances = [];
  vi.stubGlobal('WebSocket', Socket);
  relayStore.destroy(); relayStore.setHidden(false); relayStore.relayConfigs.set([]);
  securityState.set({ locked: false, busy: false, reason: 'open', status: '', hint: '' });
  vi.spyOn(relayStore, 'deviceCredential').mockReturnValue({ role: 'controller' } as never);
  vi.spyOn(relayStore, 'showToast');
  relayStore.addRelay({ id: 'pi-prompt', label: 'Synthetic', url: 'ws://127.0.0.1:8871', token: '' });
  socket = Socket.instances.at(-1)!; socket.open();
  socket.message({ type: 'push_config', protocol: 3, capabilities: ['pi_prompt_unconfirmed', 'secret_input', 'conversation_activity'], inventory: { state: 'ready' } });
  const relay = get(relayStore.relayConfigs)[0];
  agent = { relay_id: relay.id, relay_label: relay.label, pane_id: `${relay.id}::pane`, raw_pane_id: 'pane', server_session_id: 'primary', terminal_id: 'terminal',
    generation: ++generation, agent_session_id: `session-${generation}`, agent: 'pi', status: 'blocked', attention_kind: 'unknown' };
  socket.message({ type: 'agents', agents: [{ ...agent, pane_id: 'pane' }] });
});
afterEach(() => { cleanup(); relayStore.destroy(); relayStore.setHidden(false); vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it('only enables exact blocked/unknown Pi targets with a live capability and fresh normal input', async () => {
  const { view, frame } = await mount();
  await fireEvent.input(input(), { target: { value: 'normal reply' } });
  expect(screen.getByRole('button', { name: 'Send prompt' })).toBeEnabled();
  for (const variant of [{ ...agent, agent_session_id: 'other' }, { ...agent, agent: 'codex' }, { ...agent, attention_kind: 'approval' as const }]) {
    await view.rerender({ agent: variant, dispatchBlocked: promptDispatchRestriction(variant) });
    expect(screen.getByRole('button', { name: 'Send prompt' })).toBeDisabled();
    await submit();
  }
  await view.rerender({ agent, dispatchBlocked: promptDispatchRestriction(agent), frame: { ...frame, noEcho: true }, noEchoActive: true });
  await submit();
  expect(sends()).toHaveLength(0);
  expect(commands('submit_prompt')).toHaveLength(0);
  expect(commands('send_keys')).toHaveLength(0);
});

it('stays draft-only without a live bridge capability and never falls back to pane input', async () => {
  vi.spyOn(relayStore, 'loadPiPromptCapability').mockRejectedValue(new Error('Bridge not loaded'));
  await mount();
  await fireEvent.input(input(), { target: { value: '/orchestrate-answer synthetic guidance' } });
  expect(screen.getByRole('button', { name: 'Send prompt' })).toBeDisabled();
  await submit();
  expect(sends()).toHaveLength(0);
  expect(commands('submit_prompt')).toHaveLength(0);
  expect(commands('send_keys')).toHaveLength(0);
  expect(commands('send_text')).toHaveLength(0);
});

it('shows one honest notice, clears once, fences through view/metadata/reconnect/lock, never resends', async () => {
  const { view, frame } = await mount();
  await fireEvent.input(input(), { target: { value: '/orchestrate-answer synthetic guidance' } });
  await submit();
  expect(sends()).toHaveLength(1);
  expect(String(sends()[0].request_id)).toMatch(/^b{36}_[A-Za-z0-9-]+$/);
  expect(input()).toHaveValue('');
  result(); await tick();
  expect(screen.getAllByText('Sent to Pi — not confirmed')).toHaveLength(1);
  expect(relayStore.showToast).not.toHaveBeenCalled();
  await submit();
  await view.rerender({ agent: { ...agent, name: 'renamed', status: 'working' }, dispatchBlocked: '' });
  await fireEvent.input(input(), { target: { value: 'next draft' } });
  await submit();
  expect(commands('submit_prompt')).toHaveLength(0);
  view.unmount();
  const second = render(PromptComposer, { agent, frame, draftPersistenceAllowed: true, dispatchBlocked: promptDispatchRestriction(agent) });
  await tick();
  await second.rerender({ locked: true });
  expect(screen.getByRole('button', { name: 'I checked — allow another send' })).toBeDisabled();
  await second.rerender({ locked: false });
  relayStore.setHidden(true); relayStore.setHidden(false);
  socket.close(); socket.onclose?.();
  await vi.advanceTimersByTimeAsync(5000);
  expect(piPromptDelivery(agent).uncertain).toBe(true);
  expect(sends()).toHaveLength(1);
  expect(commands('send_keys')).toHaveLength(0);
  await fireEvent.click(screen.getByRole('button', { name: 'I checked — allow another send' }));
  expect(piPromptDelivery(agent).uncertain).toBe(false);
});

it.each(['not_started', 'dispatched_unknown'])('preserves existing unknown-dispatch draft semantics for %s', async phase => {
  await mount();
  await fireEvent.input(input(), { target: { value: 'reply' } });
  await submit(); result(phase, false); await vi.advanceTimersByTimeAsync(0);
  expect(input()).toHaveValue(phase === 'not_started' ? 'reply' : '');
  expect(piPromptDelivery(agent).uncertain).toBe(phase === 'dispatched_unknown');
  await vi.advanceTimersByTimeAsync(1000);
  expect(sends()).toHaveLength(1);
});

it('excludes attachments, hidden/tainted drafts, reader devices and expired capabilities', async () => {
  const { view, frame } = await mount();
  expect(screen.getByRole('button', { name: 'Attach files' })).toBeDisabled();
  for (const text of ['Attachment: ref', 'x'.repeat(100001)]) {
    await fireEvent.input(input(), { target: { value: text } }); await submit();
  }
  vi.mocked(relayStore.deviceCredential).mockReturnValue({ role: 'reader' } as never);
  await view.rerender({ frame: { ...frame } });
  await fireEvent.input(input(), { target: { value: 'secret-marker' } }); await submit();
  vi.mocked(relayStore.deviceCredential).mockReturnValue({ role: 'controller' } as never);
  await view.rerender({ draftPersistenceAllowed: false });
  await fireEvent.input(input(), { target: { value: 'tainted-marker' } });
  await view.rerender({ draftPersistenceAllowed: true, frame }); await submit();
  now += 15000;
  await view.rerender({ frame: { ...frame } }); await submit();
  expect(sends()).toHaveLength(0);
  expect(commands('submit_prompt')).toHaveLength(0);
  expect(JSON.stringify(localStorage)).not.toContain('tainted-marker');
});

it.each(['idle', 'working'])('retains normal submit_prompt routing for %s panes', async status => {
  render(PromptComposer, { agent: { ...agent, status }, draftPersistenceAllowed: true });
  await fireEvent.input(input(), { target: { value: 'ready reply' } }); await submit();
  expect(commands('submit_prompt')).toHaveLength(1);
  expect(sends()).toHaveLength(0);
});

it.each([
  ['recent', 'unconfirmed'], ['older', 'unconfirmed'], ['live', 'unconfirmed'],
  ['recent', 'dispatched_unknown'], ['older', 'dispatched_unknown'], ['live', 'dispatched_unknown'],
])('retains the %s transcript fence after %s even for a later identical native user entry', async (kind, phase) => {
  await mount();
  await fireEvent.input(input(), { target: { value: 'identical desktop reply' } });
  await submit(); result(phase, phase === 'unconfirmed'); await tick();
  now++;
  const pending = kind === 'live'
    ? relayStore.getConversationUpdates(agent, 'revision', 1, new AbortController().signal, 'subscription')
    : relayStore.getConversationHistory(agent, { limit: 24, cursor: kind === 'older' ? 'older' : '' });
  const request = commands(kind === 'live' ? 'get_conversation_updates' : 'get_conversation_history').at(-1)!;
  socket.message({ type: 'command_result', request_id: request.request_id, ok: true, phase: 'completed', data: {
    available: true, has_more: false, total: null, mode: 'native',
    subscription_id: 'subscription', kind: 'batch', source_revision: 'revision', sequence: 2, state: kind === 'live' ? 'live' : 'ready', remove: [],
    capabilities: { source: 'pi_events', active_branch: true },
    entries: [{ id: 'pi:unrelated-user', role: 'user', text: 'identical desktop reply', timestamp: new Date(5001).toISOString() }],
  } });
  await pending; await tick();
  expect(piPromptDelivery(agent).uncertain).toBe(true);
  expect(screen.getByText(phase === 'unconfirmed' ? 'Sent to Pi — not confirmed' : 'Pi send not confirmed')).toBeVisible();
  await fireEvent.input(input(), { target: { value: 'another reply' } });
  expect(screen.getByRole('button', { name: 'Send prompt' })).toBeDisabled();
  await submit();
  expect(sends()).toHaveLength(1);
  await fireEvent.click(screen.getByRole('button', { name: 'I checked — allow another send' }));
  expect(piPromptDelivery(agent).uncertain).toBe(false);
  expect(sends()).toHaveLength(1);
  for (const type of ['submit_prompt', 'send_text', 'send_keys', 'send_terminal_text']) expect(commands(type)).toHaveLength(0);
});

it('keeps pending sends and uncertain state until an explicit acknowledgement without replay', async () => {
  const idle = vi.fn();
  const delivery = new PiPromptDelivery(null, idle);
  delivery.retain();
  expect(delivery.begin()).toBe(true);
  await delivery.acknowledge();
  expect(delivery.pending).toBe(true);
  expect(delivery.begin()).toBe(false);
  delivery.finish(true, 5000);
  delivery.release();
  expect(idle).not.toHaveBeenCalled();
  expect(delivery.uncertain).toBe(true);
  expect(delivery.begin()).toBe(false);
  await delivery.acknowledge();
  expect(delivery.uncertain).toBe(false);
  expect(delivery.pending).toBe(false);
  expect(idle).toHaveBeenCalledOnce();
});

it.each(['unconfirmed', 'dispatched_unknown', 'pending'])('restores a text-free fence after %s when page state is recreated', async phase => {
  await mount();
  await fireEvent.input(input(), { target: { value: 'private reply that must not be checkpointed' } });
  await submit();
  if (phase !== 'pending') result(phase, phase === 'unconfirmed');
  else relayStore.destroy();
  await vi.advanceTimersByTimeAsync(0);
  const key = `herdr_pi_send:${targetStoreKey(targetRefForAgent(agent)!)}`;
  expect(localStorage.getItem(key)).toBe(phase === 'unconfirmed' ? 'sent' : 'unknown');
  const stored = Array.from({ length: localStorage.length }, (_, index) => localStorage.getItem(localStorage.key(index)!));
  expect(JSON.stringify(stored)).not.toContain('private reply that must not be checkpointed');
  const restored = new PiPromptDelivery(key);
  expect(restored.pending).toBe(false);
  expect(restored.uncertain).toBe(true);
  expect(restored.handed).toBe(phase === 'unconfirmed');
  expect(restored.begin()).toBe(false);
  await restored.acknowledge();
  expect(restored.uncertain).toBe(false);
  expect(localStorage.getItem(key)).toBeNull();
  expect(new PiPromptDelivery(key).uncertain).toBe(false);
  expect(sends()).toHaveLength(1);
});

it('does not dispatch if the pre-send checkpoint cannot be written and keeps the draft', async () => {
  await mount();
  const setItem = localStorage.setItem.bind(localStorage);
  vi.spyOn(localStorage, 'setItem').mockImplementation((key, value) => {
    if (key.startsWith('herdr_pi_send:')) throw new Error('Synthetic quota failure');
    setItem(key, value);
  });
  await fireEvent.input(input(), { target: { value: 'unsent reply' } });
  await submit(); await tick();
  expect(sends()).toHaveLength(0);
  expect(input()).toHaveValue('unsent reply');
  expect(piPromptDelivery(agent).uncertain).toBe(true);
  expect(screen.getByText('Pi sends need storage and Web Locks.')).toBeVisible();
  expect(screen.getByRole('button', { name: 'Send prompt' })).toBeDisabled();
});

it('fails closed when stored status cannot be read and needs explicit acknowledgement after recovery', async () => {
  const read = vi.spyOn(localStorage, 'getItem').mockImplementation(() => { throw new Error('Synthetic unavailable storage'); });
  const restored = new PiPromptDelivery('herdr_pi_send:unreadable');
  expect(restored.uncertain).toBe(true);
  expect(restored.begin()).toBe(false);
  await restored.acknowledge();
  expect(restored.uncertain).toBe(true);
  expect(restored.error).toBeTruthy();
  read.mockRestore();
  expect(restored.uncertain).toBe(true);
  await restored.acknowledge();
  expect(restored.uncertain).toBe(false);
  expect(restored.error).toBe('');
});

it.each(['throw', 'ignored'])('does not clear the fence when acknowledgement removal is %s', async failure => {
  const key = 'herdr_pi_send:removal';
  const delivery = new PiPromptDelivery(key);
  expect(delivery.begin()).toBe(true);
  await delivery.acknowledge();
  expect(localStorage.getItem(key)).toBe('unknown');
  delivery.finish(true, 5000);
  const remove = vi.spyOn(localStorage, 'removeItem').mockImplementation(() => {
    if (failure === 'throw') throw new Error('Synthetic removal failure');
  });
  await delivery.acknowledge();
  expect(delivery.uncertain).toBe(true);
  expect(delivery.error).toBeTruthy();
  expect(new PiPromptDelivery(key).uncertain).toBe(true);
  remove.mockRestore();
  await delivery.acknowledge();
  expect(delivery.uncertain).toBe(false);
  expect(new PiPromptDelivery(key).uncertain).toBe(false);
});

it('clears only the exact target checkpoint and removes known not-started attempts', async () => {
  const original = piPromptDelivery(agent);
  expect(original.begin()).toBe(true);
  original.finish(true);
  const key = `herdr_pi_send:${targetStoreKey(targetRefForAgent(agent)!)}`;
  const unrelated = 'herdr_pi_send:unrelated';
  localStorage.setItem(unrelated, 'sent');
  for (const variant of [
    { relay_id: 'other' }, { server_session_id: 'other' }, { raw_pane_id: 'other' },
    { terminal_id: 'other' }, { generation: agent.generation! + 1 }, { agent_session_id: 'other' },
  ]) expect(piPromptDelivery({ ...agent, ...variant }).uncertain).toBe(false);
  expect(new PiPromptDelivery(key).uncertain).toBe(true);
  await original.acknowledge();
  expect(localStorage.getItem(unrelated)).toBe('sent');
  const next = piPromptDelivery(agent);
  expect(next.begin()).toBe(true);
  next.finish(false);
  expect(new PiPromptDelivery(key).uncertain).toBe(false);
});

it('rechecks another page checkpoint before dispatch and does not infer clearing from a missing marker', async () => {
  const key = 'herdr_pi_send:another-page';
  const first = new PiPromptDelivery(key);
  const second = new PiPromptDelivery(key);
  expect(first.begin()).toBe(true);
  expect(second.begin()).toBe(false);
  expect(second.uncertain).toBe(true);
  localStorage.removeItem(key);
  expect(second.begin()).toBe(false);
  await second.acknowledge();
  expect(second.uncertain).toBe(false);
});

it.each(['', 'corrupt', 'null'])('retains an unrecognized stored checkpoint %j until acknowledgement', async value => {
  const key = 'herdr_pi_send:corrupt';
  localStorage.setItem(key, value);
  const restored = new PiPromptDelivery(key);
  expect(restored.uncertain).toBe(true);
  expect(restored.begin()).toBe(false);
  await restored.acknowledge();
  expect(localStorage.getItem(key)).toBeNull();
});

it('holds an exclusive target claim through dispatch and refuses acknowledgement from another page while pending', async () => {
  const key = 'herdr_pi_send:concurrent';
  const first = new PiPromptDelivery(key);
  const second = new PiPromptDelivery(key);
  let finish!: () => void;
  const pending = new Promise<void>(resolve => { finish = resolve; });
  const invoked = vi.fn();
  const send = first.withLock(async () => {
    expect(first.begin()).toBe(true);
    invoked();
    await pending;
    first.finish(true);
  });
  await expect(second.withLock(() => { invoked(); second.begin(); })).resolves.toBeUndefined();
  expect(invoked).toHaveBeenCalledOnce();
  await second.acknowledge();
  expect(localStorage.getItem(key)).toBe('unknown');
  expect(first.pending).toBe(true);
  finish(); await send;
  expect(new PiPromptDelivery(key).uncertain).toBe(true);
  await second.acknowledge();
  expect(localStorage.getItem(key)).toBeNull();
  expect(invoked).toHaveBeenCalledOnce();
});

it.each(['missing', 'rejected', 'expired'])('rechecks admission and does not dispatch when lock acquisition is %s', async mode => {
  await mount();
  vi.stubGlobal('navigator', { locks: mode === 'missing' ? undefined : {
    request: async (_name: string, _options: LockOptions, action: (lock: Lock) => unknown) => {
      if (mode === 'rejected') throw new Error('Synthetic lock failure');
      now += 15000;
      return action({ name: 'synthetic', mode: 'exclusive' });
    },
  } });
  await fireEvent.input(input(), { target: { value: 'unsent reply' } });
  await submit(); await vi.advanceTimersByTimeAsync(0);
  expect(sends()).toHaveLength(0);
  expect(input()).toHaveValue('unsent reply');
  expect(piPromptDelivery(agent).uncertain).toBe(mode !== 'expired');
  for (const type of ['submit_prompt', 'send_text', 'send_keys', 'send_terminal_text']) expect(commands(type)).toHaveLength(0);
});

it('rejects nonblocked targets and invalid bridge inputs', () => {
  expect(piPromptTarget({ ...agent, status: 'working' })).toBe(false);
  expect(piPromptTarget({ ...agent, status: 'unblocked' })).toBe(false);
  expect(relayStore.piPromptReady(agent, null)).toBe(false);
  expect(validPiPromptText('\x1b[A')).toBe(false);
});
