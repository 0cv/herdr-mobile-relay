import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { get } from 'svelte/store';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import ConversationHistory from '$components/ConversationHistory.svelte';
import TerminalView from '$components/TerminalView.svelte';
import { relayStore } from '$lib/store';
import { BrowserDeviceCredentialStore } from '$lib/device-auth';
import { base64UrlEncode } from '$lib/base64url';
import { securityState } from '$lib/security';
import { terminalInputDelivery } from '$lib/terminal-input-delivery.svelte';
import type { Agent, TerminalFrame } from '$lib/types';
import type { TransportHandlers } from '$lib/transports';

vi.mock('$lib/transports', async (original) => ({
  ...await original<typeof import('$lib/transports')>(),
  createRelayTransport: (_relay: unknown, handlers: TransportHandlers) => {
    const socket = new Socket('synthetic');
    socket.onopen = () => handlers.onStatus('connected');
    socket.onmessage = ({ data }) => handlers.onMessage(JSON.parse(data));
    return {
      kind: 'websocket', connect: () => socket.open(), close: () => socket.close(),
      send: (payload: Record<string, unknown>) => { socket.send(JSON.stringify(payload)); return socket.readyState === Socket.OPEN; },
    };
  },
}));

class Socket {
  static instances: Socket[] = [];
  static OPEN = 1;
  readyState = 0;
  sent: string[] = [];
  onopen?: () => void;
  onmessage?: (event: { data: string }) => void;
  onclose?: () => void;
  onerror?: () => void;
  constructor(readonly url: string) { Socket.instances.push(this); }
  send(value: string) { this.sent.push(value); }
  close() { this.readyState = 3; }
  open() { this.readyState = 1; this.onopen?.(); }
  message(value: unknown) { this.onmessage?.({ data: JSON.stringify(value) }); }
}
let generation = 0;
let agent: Agent;
let socket: Socket;
let now: number;
const commands = () => Socket.instances.flatMap(value => value.sent.map(text => JSON.parse(text)));
const sends = () => commands().filter(command => command.type === 'send_secret');
const secretInput = () => screen.getByLabelText('Value for the hidden terminal prompt');
const sendButton = () => screen.getByRole('button', { name: 'Send hidden value' });
const mount = (view: 'Conversation' | 'Terminal') => view === 'Conversation'
  ? render(ConversationHistory, { agent })
  : render(TerminalView, { agent, allAgents: [agent], responding: new Set<string>() });
type View = ReturnType<typeof mount>;
async function observe(view: View, noEcho = true) {
  await tick();
  now++;
  relayStore.readPane(agent, true);
  const read = commands().findLast(command => command.type === 'read_pane')!;
  socket.message({ type: 'pane_content', pane_id: agent.raw_pane_id, target: read.target, request_id: read.request_id,
    content: noEcho ? 'Password:' : 'Ready', no_echo: noEcho, no_echo_prompt: noEcho ? 'Password:' : undefined });
  const frame = get(relayStore.terminalFrames).get(agent.pane_id)!;
  await view.rerender({ frame });
  return frame;
}
function finish(command = sends().at(-1)) {
  socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'completed' });
}

beforeEach(() => {
  vi.useFakeTimers();
  now = 100_000 + (++generation) * 100_000;
  vi.spyOn(performance, 'now').mockImplementation(() => now);
  Socket.instances = [];
  localStorage.clear();
  sessionStorage.clear();
  relayStore.destroy();
  relayStore.setHidden(false);
  relayStore.relayConfigs.set([]);
  securityState.set({ locked: false, busy: false, reason: 'open', status: '', hint: '' });
  const credentials = new BrowserDeviceCredentialStore(localStorage);
  credentials.saveInvitation('secret-fixture', { id: 'fixture-invitation', version: 1,
    secret: base64UrlEncode(new Uint8Array(32).fill(7)), expiresAt: Date.now() + 60_000 });
  credentials.replaceInvitation('secret-fixture', 'fixture-invitation', { deviceId: 'fixture-device',
    credentialId: 'fixture-credential', credentialVersion: 1, credentialSecret: base64UrlEncode(new Uint8Array(32).fill(8)),
    role: 'controller', locale: 'en' });
  relayStore.addRelay({ id: 'secret-fixture', label: 'Synthetic', url: 'ws://127.0.0.1:8872', token: '' });
  socket = Socket.instances.at(-1)!;
  socket.message({ type: 'push_config', protocol: 3, capabilities: ['inventory_snapshot_v1', 'secret_input', 'manual_terminal_input'], inventory: { state: 'ready' } });
  const relay = get(relayStore.relayConfigs)[0];
  agent = { relay_id: relay.id, relay_label: relay.label, pane_id: `${relay.id}::pane`, raw_pane_id: 'pane',
    server_session_id: 'primary', terminal_id: 'terminal', generation, agent_session_id: `session-${generation}`, agent: 'codex', status: 'idle' };
  socket.message({ type: 'inventory_snapshot', snapshot_request_id: commands().findLast(command => command.type === 'refresh_agents')!.snapshot_request_id,
    inventory: { state: 'ready', stale: false }, agents: [{ ...agent, pane_id: 'pane' }], workspaces: [] });
  vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({ available: true, reason: '', entries: [], hasMore: false, total: 0 });
});
afterEach(() => {
  cleanup();
  relayStore.destroy();
  relayStore.setHidden(false);
  relayStore.relayConfigs.set([]);
  securityState.update(value => ({ ...value, locked: false }));
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

for (const name of ['Conversation', 'Terminal'] as const) {
  describe(`${name} correlated secret input`, () => {
    it('requires an actual correlated read, clears the value on dispatch and consumes the observation', async () => {
      const view = mount(name);
      const frame = await observe(view);
      expect(secretInput()).toBeEnabled();
      await fireEvent.input(secretInput(), { target: { value: 'synthetic-hidden-value' } });
      await fireEvent.click(sendButton());
      expect(sends()).toHaveLength(1);
      expect(secretInput()).toHaveValue('');
      expect(sendButton()).toBeDisabled();
      finish();
      await tick();
      expect(secretInput()).toBeDisabled();
      await expect(relayStore.sendSecret(agent, 'new-value', frame)).rejects.toThrow(/fresh exact-target/);
      expect(sends()).toHaveLength(1);
      await observe(view);
      expect(secretInput()).toBeEnabled();
      expect(JSON.stringify(localStorage)).not.toContain('synthetic-hidden-value');
      expect(JSON.stringify(sessionStorage)).not.toContain('synthetic-hidden-value');
    });

    it.each(['untagged', 'unrequested', 'expired', 'generation', 'session', 'terminal', 'server'] as const)('never authorizes disclosure from a %s frame', async kind => {
      const view = mount(name);
      const valid = await observe(view);
      let frame: TerminalFrame = valid;
      if (kind === 'unrequested' || kind === 'untagged') {
        now++;
        socket.message({ type: 'pane_content', pane_id: 'pane', target: kind === 'untagged' ? undefined : valid.target,
          content: 'Password:', no_echo: true });
        frame = get(relayStore.terminalFrames).get(agent.pane_id)!;
      } else if (kind === 'expired') now += 3_001;
      else {
        const changed = { ...agent, ...(kind === 'generation' ? { generation: generation + 1 }
          : kind === 'session' ? { agent_session_id: 'other-session' }
            : kind === 'terminal' ? { terminal_id: 'other-terminal' } : { server_session_id: 'other-server' }) };
        await view.rerender({ agent: changed });
      }
      await view.rerender({ frame });
      await fireEvent(document, new Event('visibilitychange'));
      const input = screen.queryByLabelText('Value for the hidden terminal prompt');
      if (input) {
        expect(input).toBeDisabled();
        await fireEvent.input(input, { target: { value: 'must-not-disclose' } });
        await fireEvent.keyDown(input, { key: 'Enter' });
        await fireEvent.click(sendButton());
      }
      expect(sends()).toHaveLength(0);
    });

    it.each(['lock', 'hidden', 'reconnect'] as const)('clears typed secrets on %s and requires a new post-recovery read', async recovery => {
      const view = mount(name);
      const old = await observe(view);
      await fireEvent.input(secretInput(), { target: { value: 'transient-value' } });
      if (recovery === 'lock') {
        securityState.update(value => ({ ...value, locked: true }));
        await tick();
        securityState.update(value => ({ ...value, locked: false }));
      } else if (recovery === 'hidden') {
        const descriptor = Object.getOwnPropertyDescriptor(document, 'visibilityState');
        Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
        await fireEvent(document, new Event('visibilitychange'));
        if (descriptor) Object.defineProperty(document, 'visibilityState', descriptor);
        else Reflect.deleteProperty(document, 'visibilityState');
        await fireEvent(document, new Event('visibilitychange'));
      } else {
        relayStore.connectRelay(get(relayStore.relayConfigs)[0]);
        await tick();
        socket = Socket.instances.at(-1)!;
        socket.message({ type: 'push_config', protocol: 3, capabilities: ['inventory_snapshot_v1', 'secret_input', 'manual_terminal_input'], inventory: { state: 'ready' } });
        socket.message({ type: 'inventory_snapshot', snapshot_request_id: commands().findLast(command => command.type === 'refresh_agents')!.snapshot_request_id,
          inventory: { state: 'ready', stale: false }, agents: [{ ...agent, pane_id: 'pane' }], workspaces: [] });
      }
      await tick();
      expect(secretInput()).toHaveValue('');
      expect(secretInput()).toBeDisabled();
      await fireEvent.keyDown(secretInput(), { key: 'Enter' });
      expect(sends()).toHaveLength(0);
      await view.rerender({ frame: old });
      expect(secretInput()).toBeDisabled();
      await observe(view);
      expect(secretInput()).toBeEnabled();
    });

    it('retains pending and uncertain delivery through unmount, fences all views and rejects a pre-outcome inspection', async () => {
      let view = mount(name);
      await observe(view);
      await fireEvent.input(secretInput(), { target: { value: 'one-shot-value' } });
      await fireEvent.click(sendButton());
      const command = sends()[0];
      expect(terminalInputDelivery(agent).pending).toBe(true);
      await expect(relayStore.sendSecret(agent, 'one-shot-value', get(relayStore.terminalFrames).get(agent.pane_id)!)).rejects.toThrow();
      expect(sends()).toHaveLength(1);
      view.unmount();
      view = mount('Terminal');
      await observe(view);
      expect(secretInput()).toHaveValue('');
      expect(sendButton()).toBeDisabled();
      now++;
      relayStore.readPane(agent, true);
      const staleRead = commands().findLast(value => value.type === 'read_pane')!;
      const reads = vi.spyOn(relayStore, 'readPane').mockImplementation(() => {});
      now += 15_001;
      await vi.advanceTimersByTimeAsync(15_001);
      expect(terminalInputDelivery(agent).uncertain).toBe(true);
      finish(command);
      await tick();
      expect(terminalInputDelivery(agent).uncertain).toBe(true);
      socket.message({ type: 'pane_content', pane_id: 'pane', target: staleRead.target, request_id: staleRead.request_id, content: 'Password:', no_echo: true });
      await view.rerender({ frame: get(relayStore.terminalFrames).get(agent.pane_id)! });
      await fireEvent.click(screen.getByRole('button', { name: 'Keyboard' }));
      await vi.waitFor(() => expect(screen.getByRole('button', { name: 'I checked the terminal' })).toBeVisible());
      const inspect = screen.getByRole('button', { name: 'I checked the terminal' });
      expect(inspect).toBeDisabled();
      await fireEvent.click(inspect);
      expect(terminalInputDelivery(agent).uncertain).toBe(true);
      reads.mockRestore();
      await observe(view);
      expect(inspect).toBeEnabled();
      await fireEvent.click(inspect);
      expect(terminalInputDelivery(agent).uncertain).toBe(false);
      await fireEvent.click(screen.getByRole('button', { name: 'Close' }));
      expect(secretInput()).toHaveValue('');
      expect(sends()).toHaveLength(1);
      expect(JSON.stringify(localStorage)).not.toContain('one-shot-value');
    });
  });
}

it('consumes and clears a secret even on a definitive pre-dispatch refusal', async () => {
  const view = mount('Conversation');
  await observe(view);
  vi.spyOn(relayStore, 'sendToAgent').mockRejectedValue({ data: { not_started: true } });
  await fireEvent.input(secretInput(), { target: { value: 'refused-value' } });
  await fireEvent.click(sendButton());
  await tick();
  expect(secretInput()).toHaveValue('');
  expect(secretInput()).toBeDisabled();
  expect(terminalInputDelivery(agent).uncertain).toBe(false);
  expect(sends()).toHaveLength(0);
});
