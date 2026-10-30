import { get } from 'svelte/store';

import { fireEvent, render, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import { securityState } from '$lib/security';
import ManualTerminalInput from '$components/ManualTerminalInput.svelte';
import { TerminalInputDelivery } from '$lib/terminal-input-delivery.svelte';
import unavailablePage from '../../../contracts/fixtures/conversation_unavailable.json';
import piCatalogFixture from '../../../contracts/fixtures/pi_command_catalog.json';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { makeRelayId } from '$lib/config';
import { decodeTargetRoute, encodeTargetRoute, targetRefForAgent, targetStoreKey } from '$lib/resource-id';
import { BrowserDeviceCredentialStore } from '$lib/device-auth';
import {
  defaultAgentView,
  paneAgentViewOverrides,
  setPaneAgentView,
  setTerminalHeightLease,
  setTerminalHistoryLines,
  setTerminalRefreshInterval,
  setTerminalResizeSession,
} from '$lib/preferences';
import { paneViewPreferenceKey } from '$lib/agent-view';
import { closeCurrentView, currentView, initializeRouter, navigate } from '$lib/router';
import { SLASH_COMMAND_MAX_ENTRIES } from '$lib/slash-command-limits';
import { normalizeConversationPage, relayStore, CommandError } from '$lib/store';
import type { RelayTransport, TransportAuthentication, TransportHandlers, TransportStatus, TransportStatusDetail } from '$lib/transports';
import type { Agent, RelayConfig, RelayWorkspace } from '$lib/types';
import { pendingRelayUpdate } from '$lib/updates';

type TransportFactory = (relay: RelayConfig, handlers: TransportHandlers, authentication?: TransportAuthentication) => RelayTransport;

/**
 * Lets one test swap in a transport the store cannot otherwise reach, such as
 * a path that reports a fatal failure. Every other test keeps the real
 * WebSocket transport and the MockWebSocket below.
 */
const transportHijack = vi.hoisted(() => ({ current: null as TransportFactory | null }));

vi.mock('$lib/transports', async (importOriginal) => {
  const actual = await importOriginal() as { createRelayTransport: TransportFactory };
  return {
    ...actual,
    createRelayTransport: (relay: RelayConfig, handlers: TransportHandlers, authentication?: TransportAuthentication) =>
      (transportHijack.current ?? actual.createRelayTransport)(relay, handlers, authentication),
  };
});

class MockWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  static instances: MockWebSocket[] = [];
  readyState = MockWebSocket.CONNECTING;
  sent: string[] = [];
  onopen: (() => void) | null = null;
  onclose: ((event?: { code: number }) => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  constructor(readonly url: string, readonly protocols?: string | string[]) { MockWebSocket.instances.push(this); }
  get protocol() { return typeof this.protocols === 'string' ? this.protocols : ''; }
  send(payload: string) { this.sent.push(payload); }
  close() { this.readyState = MockWebSocket.CLOSED; }
  open() { this.readyState = MockWebSocket.OPEN; this.onopen?.(); }
  message(payload: unknown) { this.onmessage?.({ data: JSON.stringify(payload) }); }
  serverClose(code?: number) {
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.(code === undefined ? undefined : { code });
  }
}
function exactAgentFields(paneId = 'w1:p1') {
  return {
    server_session_id: 'primary',
    terminal_id: `terminal-${paneId}`,
    generation: 1,
    agent_session_id: '',
  };
}
function preferenceAgent(relayId: string, rawPaneId: string, terminalId: string): Agent {
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
  };
}

function exactWireScope(paneId: string, relayId: string) {
  return {
    server_session_id: 'primary',
    target: {
      pane_id: paneId,
      relay_id: relayId,
      server_session_id: 'primary',
      terminal_id: `terminal-${paneId}`,
      generation: 1,
    },
  };
}

describe('conversation reason code wire decoding', () => {
  it('consumes the shared Go wire shape and ignores unknown fields', () => {
    expect(normalizeConversationPage({ ...unavailablePage, future_field: { arbitrary: true } }))
      .toMatchObject({ available: false, reasonCode: 'source_missing', reason: unavailablePage.reason });
  });

  it.each([undefined, null, ''])('keeps optional code %s absent', (reason_code) => {
    expect(normalizeConversationPage({ ...unavailablePage, reason_code }).reasonCode).toBeUndefined();
  });

  it('accepts the same 64-character bound as error codes without an enum restriction', () => {
    expect(normalizeConversationPage({ ...unavailablePage, reason_code: 'x'.repeat(64) }).reasonCode).toBe('x'.repeat(64));
  });

  it.each([42, false, {}, [], 'x'.repeat(65)].map(value => [value]))('rejects malformed or oversized code %j', (reason_code) => {
    expect(() => normalizeConversationPage({ ...unavailablePage, reason_code })).toThrow('invalid conversation history');
  });
});

describe('relay command store', () => {
  beforeEach(() => {
    transportHijack.current = null;
    MockWebSocket.instances = [];
    localStorage.clear();
    sessionStorage.clear();
    defaultAgentView.set('terminal');
    paneAgentViewOverrides.set({});
    vi.stubGlobal('WebSocket', MockWebSocket);
    relayStore.destroy();
    relayStore.toast.set(null);
    setTerminalRefreshInterval(250);
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({ label: 'Fedora', url: 'ws://127.0.0.1:8871', token: '' });
  });

  afterEach(() => {
    transportHijack.current = null;
    relayStore.destroy();
    setTerminalResizeSession(false);
    setTerminalHeightLease(false);
    setTerminalHistoryLines(1_000);
    relayStore.relayConfigs.set([]);
    defaultAgentView.set('terminal');
    paneAgentViewOverrides.set({});
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('retains authoritative unknown-kind agents without profiles, sessions or workspace records', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' }, capabilities: [], agent_profiles: [] });
    socket.message({ type: 'agents', agents: [{ ...exactAgentFields(), pane_id: 'w1:p1', terminal_id: 'terminal', workspace_id: 'workspace', tab_id: 'tab', name: 'Named agent', agent: '', status: 'unknown', cwd: '/home/example/Development', conversation_history_available: false }] });
    const row = get(relayStore.agents)[0];
    expect(row).toMatchObject({ raw_pane_id: 'w1:p1', terminal_id: 'terminal', name: 'Named agent', agent: '', cwd: '/home/example/Development' });
    expect(row.agent_session_id || '').toBe('');
    expect(get(relayStore.workspaces)).toHaveLength(0);
    const relayId = get(relayStore.relayConfigs)[0].id;
    expect(get(relayStore.connections).get(relayId)?.agentProfiles).toEqual([]);
    socket.message({ type: 'agents', agents: [] });
    expect(get(relayStore.agents)).toHaveLength(0);
  });

  it.each(['agents', 'blocked', 'agent_update'].flatMap(ingress => [
    { label: 'padded', session: ' padded ' },
    { label: 'Unicode', session: '  opaque/session 界😀  ' },
    { label: 'internal whitespace', session: 'session\u2002id' },
    { label: 'nonbreaking space', session: '\u00a0' },
    { label: 'spaces only', session: '   ' },
    { label: 'maximum length', session: 'x'.repeat(2048) },
  ].map(value => ({ ingress, ...value }))))('preserves $label opaque session bytes through $ingress ingress and read/send round trips', async ({ ingress, session }) => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: ['manual_terminal_input'], inventory: { state: 'ready' } });
    const initial = { ...exactAgentFields(), pane_id: 'w1:p1', agent: 'pi', status: 'idle', pane_revision: 1, agent_session_id: 'previous-session' };
    socket.message({ type: 'agents', agents: [initial] });
    const incoming = { ...initial, pane_revision: 2, agent_session_id: session };
    socket.message(ingress === 'agents' ? { type: ingress, agents: [incoming] } : { ...incoming, type: ingress });
    const agent = get(relayStore.agents)[0];
    expect(agent.agent_session_id).toBe(session);
    const target = { ...exactWireScope('w1:p1', agent.relay_id).target, agent_session_id: session };
    expect(targetRefForAgent(agent)).toEqual(target);
    expect(decodeTargetRoute(encodeTargetRoute(target)!)).toEqual(target);
    expect(targetStoreKey(target)).not.toBe(targetStoreKey({ ...target, agent_session_id: session + 'different' }));
    relayStore.readPane(agent, true);
    const read = JSON.parse(socket.sent.at(-1)!);
    expect(read).toMatchObject({ type: 'read_pane', target });
    const frame = () => get(relayStore.terminalFrames).get(agent.pane_id);
    const beforeResponse = frame();
    socket.message({ type: 'pane_content', request_id: read.request_id, pane_id: 'w1:p1', target: { ...target, agent_session_id: 'different-session' }, content: 'wrong session', format: 'ansi', no_echo: false });
    expect(frame()).toBe(beforeResponse);
    socket.message({ type: 'pane_content', request_id: read.request_id, pane_id: 'w1:p1', target, content: 'exact session', format: 'ansi', no_echo: false });
    expect(frame()).toMatchObject({ inputTarget: target, target, content: 'exact session', readStartedAt: expect.any(Number) });
    const pending = relayStore.sendToAgent(agent, { type: 'send_terminal_text', text: '  /literal 界  ' });
    const command = JSON.parse(socket.sent.at(-1)!);
    expect(command).toMatchObject({ type: 'send_terminal_text', pane_id: 'w1:p1', target, text: '  /literal 界  ' });
    expect(command.keys).toBeUndefined();
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'completed' });
    await expect(pending).resolves.toMatchObject({ ok: true, phase: 'completed' });
  });

  it.each([' invalid\n', '\tinvalid', 'x'.repeat(2049)])('does not turn an invalid opaque session into a routable identity', async session => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    socket.message({ type: 'agents', agents: [{ ...exactAgentFields(), pane_id: 'w1:p1', agent_session_id: session }] });
    const agent = get(relayStore.agents)[0];
    expect(targetRefForAgent(agent)).toBeNull();
    const before = socket.sent.length;
    await expect(relayStore.sendToAgent(agent, { type: 'send_terminal_text', text: 'not sent' })).rejects.toThrow('exact terminal identity');
    expect(socket.sent).toHaveLength(before);
  });

  it('preserves relay URLs, protocol v3 command shapes, and confirmations', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    expect(socket.url).toBe('ws://127.0.0.1:8871');
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const pending = relayStore.sendCommand(relayId, { type: 'agent_rename', pane_id: 'w1:p1', name: '123' });
    const command = JSON.parse(socket.sent.at(-1)!);
    expect(command).toMatchObject({ type: 'agent_rename', pane_id: 'w1:p1', name: '123', protocol: 3 });
    expect(command.client_id).toBeTruthy();
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'confirmed' });
    await expect(pending).resolves.toMatchObject({ ok: true, phase: 'confirmed' });
  });

  it('keeps only well-formed Hermes profile names from push_config', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    const relayId = get(relayStore.relayConfigs)[0].id;
    // A relay that predates the picker omits the field entirely.
    socket.message({ type: 'push_config', protocol: 3, host: 'fedora', capabilities: [], agent_profiles: [] });
    expect(relayStore.connection(relayId)?.hermesProfiles).toEqual([]);
    socket.message({
      type: 'push_config',
      protocol: 3,
      host: 'fedora',
      capabilities: [],
      agent_profiles: [],
      hermes_profiles: ['default', 'apollo', '', 7, null],
    });
    expect(relayStore.connection(relayId)?.hermesProfiles).toEqual(['default', 'apollo']);
  });

  it.each(['completed', 'completed_with_warning'])('retains a launch until delayed %s', async (phase) => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const pending = relayStore.sendCommand(relayId, { type: 'agent_start', profile_id: 'claude', name: 'project', cwd: '/tmp' });
    const command = JSON.parse(socket.sent.at(-1)!);
    const settled = vi.fn();
    void pending.then(settled, settled);
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'accepted' });
    await vi.advanceTimersByTimeAsync(52_000);
    expect(settled).not.toHaveBeenCalled();
    const data = phase === 'completed_with_warning' ? { warning: 'Initial prompt was not confirmed' } : {};
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase, data });
    await expect(pending).resolves.toMatchObject({ ok: true, phase, data });
  });

  it.each(['missing', 'repeated-accepted', 'disconnect', 'abort'] as const)('bounds Hermes launch confirmation with %s results and never replays it', async outcome => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const controller = new AbortController();
    const pending = relayStore.sendCommand(relayId, { type: 'agent_start', profile_id: 'hermes', name: 'development-hermes', cwd: '/fixture/Development' }, undefined, false, controller.signal);
    const caught = pending.catch(error => error as CommandError);
    const settled = vi.fn();
    void caught.then(settled);
    const command = JSON.parse(socket.sent.at(-1)!);
    if (outcome === 'disconnect') socket.serverClose();
    if (outcome === 'abort') controller.abort();
    for (let elapsed = 0; elapsed < 60_000; elapsed += 5_000) {
      if (outcome === 'repeated-accepted') socket.message({ type: 'command_result', request_id: command.request_id, action: 'agent_start', ok: true, phase: 'accepted' });
      await vi.advanceTimersByTimeAsync(5_000);
    }
    expect(settled).toHaveBeenCalledOnce();
    const error = await caught;
    expect(error).toBeInstanceOf(Error);
    if (outcome !== 'abort') expect(error.data).toMatchObject({ dispatched_unknown: true });
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'completed', data: { pane_id: 'created' } });
    expect(socket.sent.map(value => JSON.parse(value)).filter(value => value.type === 'agent_start')).toHaveLength(1);
    expect(settled).toHaveBeenCalledOnce();
  });

  it('bounds conversation history payloads and requires a usable preparation cursor', async () => {
    const agent = preferenceAgent('fedora', 'w1:p1', 'terminal-w1:p1');
    const send = vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue({
      type: 'command_result',
      request_id: 'history-1',
      ok: true,
      data: {
        available: true,
        state: 'ready',
        mode: 'recent',
        has_more: false,
        entries: [{
          id: 'entry-1',
          timestamp: '2026-01-01T00:00:00Z',
          role: 'assistant',
          text: 'x'.repeat(1_200_000),
          tools: [{ id: 'tool-1', name: '', input: 'i'.repeat(1_200_000), output: 'o'.repeat(1_200_000) }],
        }],
        diagnostics: {},
      },
    });
    const result = await relayStore.getConversationHistory(agent);
    expect(result.entries).toHaveLength(1);
    expect(result.entries[0].text).toHaveLength(1_048_576);
    expect(result.entries[0].tools?.[0]).toMatchObject({ name: 'Tool', input: 'i'.repeat(1_048_576), output: 'o'.repeat(1_048_576) });

    send.mockResolvedValueOnce({
      type: 'command_result', request_id: 'history-2', ok: true,
      data: { available: true, state: 'preparing', mode: 'recent', has_more: false, entries: [], diagnostics: {} },
    });
    await expect(relayStore.getConversationHistory(agent)).rejects.toThrow('Relay returned invalid conversation history');

    send.mockResolvedValueOnce({
      type: 'command_result', request_id: 'history-3', ok: true,
      data: {
        available: true, state: 'ready', mode: 'recent', has_more: false, entries: [],
        diagnostics: { continuation_incomplete: true, continuation_reason: 'partial_link' },
      },
    });
    await expect(relayStore.getConversationHistory(agent)).resolves.toMatchObject({
      diagnostics: { continuation_incomplete: true, continuation_reason: 'partial_link' },
    });
  });

  it('aborts a conversation request without leaving a pending relay handler', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [] });
    const controller = new AbortController();
    const relayId = get(relayStore.relayConfigs)[0].id;
    const current = preferenceAgent(relayId, 'w1:p1', 'terminal-w1:p1');
    const pending = relayStore.getConversationHistory(current, { signal: controller.signal });
    expect(socket.sent.some((payload) => JSON.parse(payload).type === 'get_conversation_history')).toBe(true);
    controller.abort();
    await expect(pending).rejects.toMatchObject({ code: 'request_cancelled' });
    const request = JSON.parse(socket.sent.at(-1)!);
    socket.message({ type: 'command_result', request_id: request.request_id, ok: true, phase: 'completed', data: {} });
    await expect(relayStore.getConversationHistory(current, { signal: AbortSignal.abort() })).rejects.toMatchObject({ code: 'request_cancelled' });
  });

  it('cleans pane view overrides only when a relay is explicitly removed', () => {
    const fedoraId = get(relayStore.relayConfigs)[0].id;
    relayStore.addRelay({ label: 'Mac', url: 'ws://127.0.0.1:8872', token: '' });
    const macId = get(relayStore.relayConfigs).find((relay) => relay.label === 'Mac')!.id;
    const fedoraPane = preferenceAgent(fedoraId, 'pane-1', 'terminal-1');
    const macPane = preferenceAgent(macId, 'pane-1', 'terminal-1');
    defaultAgentView.set('conversation');
    setPaneAgentView(fedoraPane, 'terminal');
    setPaneAgentView(macPane, 'conversation');
    relayStore.disconnectRelay(fedoraId);
    expect(get(paneAgentViewOverrides)).toEqual({
      [paneViewPreferenceKey(fedoraPane)!]: 'terminal',
      [paneViewPreferenceKey(macPane)!]: 'conversation',
    });
    relayStore.removeRelay(fedoraId);
    expect(get(paneAgentViewOverrides)).toEqual({
      [paneViewPreferenceKey(macPane)!]: 'conversation',
    });
    expect(get(defaultAgentView)).toBe('conversation');
  });

  it('does not block relay removal when pane preference cleanup fails', () => {
    const relayId = get(relayStore.relayConfigs)[0].id;
    const pane = preferenceAgent(relayId, 'pane-1', 'terminal-1');
    setPaneAgentView(pane, 'conversation');
    const nativeRemove = localStorage.removeItem.bind(localStorage);
    const remove = vi.spyOn(localStorage, 'removeItem').mockImplementation((key) => {
      if (key === 'herdr_pane_agent_view_overrides') throw new Error('read only');
      nativeRemove(key);
    });
    const toast = vi.spyOn(relayStore, 'showToast');
    relayStore.removeRelay(relayId);
    expect(get(relayStore.relayConfigs).some((relay) => relay.id === relayId)).toBe(false);
    expect(get(paneAgentViewOverrides)).toEqual({ [paneViewPreferenceKey(pane)!]: 'conversation' });
    expect(toast).toHaveBeenCalledWith('Could not clear saved pane view preferences on this device.', true);
    remove.mockRestore();
  });

  it('applies live Herdr status updates without accepting stale generations', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 3,
      host: 'fedora',
      capabilities: ['workspace_management', 'pane_realtime_delta'],
      herdr_status: {
        generation: 4,
        installed_client_version: '0.9.0',
        server_version: '0.8.0',
        server_protocol: 1,
        server_protocol_known: true,
        features: {
          'pane.read': { state: 'supported', reason: 'ping', generation: 4 },
        },
      },
      agent_profiles: [],
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    socket.message({
      type: 'herdr_status',
      status: {
        generation: 3,
        server_version: 'old',
        features: {
          'pane.read': { state: 'unsupported', reason: 'method_not_supported', generation: 3 },
        },
      },
      capabilities: ['workspace_management'],
    });
    expect(relayStore.connection(relayId)?.herdrStatus.server_version).toBe('0.8.0');
    expect(relayStore.connection(relayId)?.capabilities).toContain('pane_realtime_delta');
    socket.message({
      type: 'herdr_status',
      status: {
        generation: 5,
        server_version: '0.9.0',
        server_protocol: 2,
        server_protocol_known: true,
        features: {
          'pane.read': { state: 'unsupported', reason: 'method_not_supported', generation: 5 },
        },
      },
      capabilities: ['workspace_management'],
    });
    expect(relayStore.connection(relayId)?.herdrStatus.server_version).toBe('0.9.0');
    expect(relayStore.connection(relayId)?.capabilities).toEqual(['workspace_management']);
  });

  it('stores empty workspaces and sends workspace and worktree commands', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 3,
      version: 'abc123',
      host: 'fedora',
      capabilities: ['workspace_management', 'workspace_reorder_block', 'worktree_management'],
      agent_profiles: [],
      inventory: { state: 'ready' },
    });
    socket.message({
      type: 'workspaces',
      workspaces: [{
        workspace_id: 'w1',
        number: 1,
        label: 'Project',
        pane_count: 1,
        tab_count: 1,
        cwd: '/home/user/project',
        worktree: {
          repo_key: 'repo',
          repo_name: 'project',
          repo_root: '/home/user/project',
          checkout_path: '/home/user/project',
          is_linked_worktree: false,
        },
      }, {
        workspace_id: 'w2',
        number: 2,
        label: 'Empty',
        pane_count: 1,
        tab_count: 1,
        cwd: '/home/user/empty',
      }],
    });
    expect(get(relayStore.workspaces)).toEqual([
      expect.objectContaining({ workspace_id: 'w1', label: 'Project', cwd: '/home/user/project' }),
      expect.objectContaining({ workspace_id: 'w2', label: 'Empty', cwd: '/home/user/empty' }),
    ]);

    const workspace = get(relayStore.workspaces)[0];
    const rename = relayStore.renameWorkspace(workspace, 'Renamed');
    const renameCommand = JSON.parse(socket.sent.at(-1)!);
    expect(renameCommand).toMatchObject({
      type: 'workspace_rename',
      workspace_id: 'w1',
      label: 'Renamed',
      protocol: 3,
    });
    socket.message({
      type: 'command_result',
      request_id: renameCommand.request_id,
      action: 'workspace_rename',
      ok: true,
      phase: 'completed',
    });
    await expect(rename).resolves.toMatchObject({ ok: true });

    const reorder = relayStore.reorderWorkspaceBlock(workspace.relay_id, ['w1', 'w2'], 'w5', 2);
    const reorderCommand = JSON.parse(socket.sent.at(-1)!);
    expect(reorderCommand).toMatchObject({
      type: 'workspace_reorder',
      workspace_ids: ['w1', 'w2'],
      before_workspace_id: 'w5',
      protocol: 3,
    });
    socket.message({
      type: 'command_result',
      request_id: reorderCommand.request_id,
      action: 'workspace_reorder',
      ok: true,
      phase: 'completed',
    });
    await expect(reorder).resolves.toMatchObject({ ok: true });

    socket.message({
      type: 'push_config',
      protocol: 3,
      version: 'abc123',
      host: 'fedora',
      capabilities: ['workspace_management', 'worktree_management'],
      agent_profiles: [],
      inventory: { state: 'ready' },
    });
    const legacyReorder = relayStore.reorderWorkspaceBlock(workspace.relay_id, ['w1'], '', 2);
    const legacyCommand = JSON.parse(socket.sent.at(-1)!);
    expect(legacyCommand).toMatchObject({
      type: 'workspace_reorder',
      workspace_id: 'w1',
      insert_index: 2,
      protocol: 3,
    });
    expect(legacyCommand).not.toHaveProperty('workspace_ids');
    socket.message({
      type: 'command_result',
      request_id: legacyCommand.request_id,
      action: 'workspace_reorder',
      ok: true,
      phase: 'completed',
    });
    await expect(legacyReorder).resolves.toMatchObject({ ok: true });

    const listing = relayStore.listWorktrees(workspace);
    const listCommand = JSON.parse(socket.sent.at(-1)!);
    expect(listCommand).toMatchObject({ type: 'worktree_list', workspace_id: 'w1', protocol: 3 });
    socket.message({
      type: 'command_result',
      request_id: listCommand.request_id,
      action: 'worktree_list',
      ok: true,
      phase: 'completed',
      data: {
        source: {
          repo_key: 'repo',
          repo_name: 'project',
          repo_root: '/home/user/project',
          source_checkout_path: '/home/user/project',
          source_workspace_id: 'w1',
        },
        worktrees: [],
      },
    });
    await expect(listing).resolves.toMatchObject({ source: { repo_name: 'project' }, worktrees: [] });
  });

  it('keeps relay keys out of the WebSocket URL and waits for encrypted authentication', async () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    relayStore.addRelay({
      label: 'Fedora',
      url: 'wss://fedora.example/ws?region=test',
      token: '0123456789abcdef0123456789abcdef',
    });
    const socket = MockWebSocket.instances.at(-1)!;
    expect(socket.url).toBe('wss://fedora.example/ws?region=test');
    expect(socket.protocols).toBe('herdr-e2ee-v2');
    socket.open();
    await vi.waitFor(() => expect(socket.sent).toHaveLength(1));
    const hello = JSON.parse(socket.sent[0]);
    expect(hello).toMatchObject({
      type: 'e2ee_client_hello',
      version: 2,
      auth_kind: 'invitation',
      auth_id: 'bootstrap',
      auth_version: 1,
    });
    expect(socket.sent[0]).not.toContain('0123456789abcdef0123456789abcdef');
    const relayId = get(relayStore.relayConfigs)[0].id;
    expect(get(relayStore.connections).get(relayId)?.status).toBe('connecting');
    expect(relayStore.sendRaw(relayId, { type: 'refresh_agents' })).toBe(false);
  });
  it('negotiates encrypted WebSocket transport for a one-use device invitation', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    const relay = {
      label: 'Invited',
      url: 'wss://invited.example/ws',
      token: '',
    };
    const relayId = makeRelayId(relay.label, relay.url);
    new BrowserDeviceCredentialStore(localStorage).saveInvitation(relayId, {
      id: 'invitation-credential',
      version: 1,
      secret: 'A'.repeat(43),
      expiresAt: Date.now() + 60_000,
    });
    relayStore.addRelay(relay);
    expect(MockWebSocket.instances.at(-1)?.protocols).toBe('herdr-e2ee-v2');
  });

  it('persists an iOS invitation only after the standalone app opens', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    const expiresAt = Date.now() + 60_000;
    const hash = `#setup=${'A'.repeat(43)}&invite=invitation-123456&invite_version=1`
      + `&invite_expires=${expiresAt}&label=Invited&relay=${encodeURIComponent('wss://invited.example')}`;
    const setupLocation = {
      hash,
      protocol: 'https:',
      host: 'app.example',
      pathname: '/',
      search: '',
    };
    vi.stubGlobal('navigator', {
      standalone: false,
      userAgent: 'Mozilla/5.0 (iPhone)',
      maxTouchPoints: 5,
    });
    expect(relayStore.importSetupLink(setupLocation, true)).toBe(true);
    const relayId = makeRelayId('Invited', 'wss://invited.example');
    const store = new BrowserDeviceCredentialStore(localStorage);
    expect(store.get(relayId)).toBeNull();
    expect(MockWebSocket.instances).toHaveLength(0);

    vi.stubGlobal('navigator', {
      standalone: true,
      userAgent: 'Mozilla/5.0 (iPhone)',
      maxTouchPoints: 5,
    });
    expect(relayStore.importSetupLink(setupLocation, true)).toBe(true);
    expect(store.get(relayId)).toMatchObject({
      kind: 'invitation',
      id: 'invitation-123456',
    });
    expect(MockWebSocket.instances).toHaveLength(1);
  });

  describe('explicit iOS browser pairing', () => {
    const relayId = makeRelayId('Invited', 'wss://invited.example');

    function deferBrowserPairing(invitation = true) {
      relayStore.destroy();
      relayStore.relayConfigs.set([]);
      localStorage.clear();
      MockWebSocket.instances = [];
      vi.stubGlobal('navigator', {
        standalone: false,
        userAgent: 'Mozilla/5.0 (iPhone)',
        maxTouchPoints: 5,
      });
      const secret = invitation ? 'A'.repeat(43) : '0123456789abcdef0123456789abcdef';
      const hash = `#setup=${secret}&label=Invited&relay=${encodeURIComponent('wss://invited.example')}`
        + (invitation ? `&invite=invitation-browser01&invite_version=1&invite_expires=${Date.now() + 60_000}` : '');
      history.replaceState({ retained: true }, '', `/pair?source=test${hash}`);
      relayStore.initialize();
      expect(get(relayStore.connections).get(relayId)?.pairingDeferred).toBe(true);
      expect(new BrowserDeviceCredentialStore(localStorage).get(relayId)).toBeNull();
      expect(MockWebSocket.instances).toHaveLength(0);
      expect(location.hash).toBe(hash);
      return hash;
    }

    it('saves the invitation, clears the fragment, and dials only after opting in', () => {
      deferBrowserPairing();
      const replace = vi.spyOn(history, 'replaceState');
      expect(relayStore.pairDeferredRelay(relayId)).toBe(true);
      expect(new BrowserDeviceCredentialStore(localStorage).get(relayId)).toMatchObject({
        kind: 'invitation', id: 'invitation-browser01', secret: 'A'.repeat(43),
      });
      expect(MockWebSocket.instances).toHaveLength(1);
      expect(MockWebSocket.instances[0].protocols).toBe('herdr-e2ee-v2');
      expect(get(relayStore.connections).get(relayId)?.pairingDeferred).toBe(false);
      expect(replace).toHaveBeenCalledWith({ retained: true }, '', '/pair?source=test');
      expect(location.hash).toBe('');
      expect(get(relayStore.toast)?.message).toBe('Pairing this browser as its own device.');

      relayStore.initialize();
      expect(get(relayStore.connections).get(relayId)?.pairingDeferred).toBe(false);
      expect(MockWebSocket.instances).toHaveLength(2);
    });

    it('connects a bootstrap-key link using the imported relay token', async () => {
      deferBrowserPairing(false);
      expect(get(relayStore.relayConfigs)[0].token).toBe('0123456789abcdef0123456789abcdef');
      expect(relayStore.pairDeferredRelay(relayId)).toBe(true);
      expect(MockWebSocket.instances).toHaveLength(1);
      const socket = MockWebSocket.instances[0];
      expect(socket.protocols).toBe('herdr-e2ee-v2');
      socket.open();
      await vi.waitFor(() => expect(socket.sent).toHaveLength(1));
      expect(JSON.parse(socket.sent[0])).toMatchObject({ auth_kind: 'invitation', auth_id: 'bootstrap' });
      expect(get(relayStore.connections).get(relayId)?.pairingDeferred).toBe(false);
      expect(location.hash).toBe('');
    });

    it.each([
      { invitation: true, enrolled: false },
      { invitation: true, enrolled: true },
      { invitation: false, enrolled: false },
      { invitation: false, enrolled: true },
    ])('does not re-defer on Back/hashchange or reload: invitation=$invitation, enrolled=$enrolled', async ({ invitation, enrolled }) => {
      const hash = deferBrowserPairing(invitation);
      vi.spyOn(window, 'scrollTo').mockImplementation(() => {});
      const stopRouter = initializeRouter();
      const importOnHashChange = vi.fn(() => relayStore.importSetupLink());
      window.addEventListener('hashchange', importOnHashChange);
      try {
        navigate({ view: 'activity' });
        navigate({ view: 'settings' });
        expect(location.hash).toBe(hash);
        expect(relayStore.pairDeferredRelay(relayId)).toBe(true);
        const socket = MockWebSocket.instances[0];
        socket.open();
        await vi.waitFor(() => expect(socket.sent).toHaveLength(1));
        const credentials = new BrowserDeviceCredentialStore(localStorage);
        if (enrolled) {
          credentials.replaceInvitation(relayId, invitation ? 'invitation-browser01' : 'bootstrap', {
            deviceId: 'device-browser01', credentialId: 'credential-browser01', credentialVersion: 1,
            credentialSecret: 'B'.repeat(43), role: 'controller', locale: 'en',
          });
        }
        const authentication = credentials.get(relayId);
        const connection = relayStore.connection(relayId);
        const pairingToast = get(relayStore.toast);
        expect(pairingToast?.message).toBe('Pairing this browser as its own device.');
        closeCurrentView();
        await vi.waitFor(() => expect(importOnHashChange).toHaveBeenCalledOnce());
        expect(get(currentView)).toMatchObject({ view: 'activity' });
        expect(get(relayStore.toast)).toBe(pairingToast);
        closeCurrentView();
        await vi.waitFor(() => expect(importOnHashChange).toHaveBeenCalledTimes(2));
        expect(importOnHashChange).toHaveReturnedWith(true);
        expect(get(currentView)).toMatchObject({ view: 'agents' });
        expect(relayStore.connection(relayId)).toBe(connection);
        expect(get(relayStore.connections).get(relayId)?.pairingDeferred).toBe(false);
        expect(socket.readyState).toBe(MockWebSocket.OPEN);
        expect(MockWebSocket.instances).toHaveLength(1);
        expect(credentials.get(relayId)).toEqual(authentication);
        expect(location.hash).toBe('');
        expect(get(relayStore.toast)).toBe(pairingToast);
        history.forward();
        await vi.waitFor(() => expect(get(currentView)).toMatchObject({ view: 'activity' }));
        expect(get(relayStore.toast)).toBe(pairingToast);

        history.replaceState(history.state, '', `/pair?source=test${hash}`);
        relayStore.destroy();
        relayStore.initialize();
        expect(get(relayStore.connections).get(relayId)?.pairingDeferred).toBe(false);
        expect(MockWebSocket.instances).toHaveLength(2);
        expect(credentials.get(relayId)).toEqual(authentication);
        expect(location.hash).toBe('');
      } finally {
        window.removeEventListener('hashchange', importOnHashChange);
        stopRouter();
        currentView.set({ view: 'agents' });
      }
    });

    it.each([
      { entry: 'fresh', load: 'initialize' },
      { entry: 'fresh', load: 'import' },
      { entry: 'new session', load: 'initialize' },
      { entry: 'new session', load: 'import' },
    ])('defers a bootstrap link after an untrusted transport close on a $entry entry via $load', ({ entry, load }) => {
      const hash = deferBrowserPairing(false);
      expect(relayStore.pairDeferredRelay(relayId)).toBe(true);
      const credentials = new BrowserDeviceCredentialStore(localStorage);
      credentials.replaceInvitation(relayId, 'bootstrap', {
        deviceId: 'device-browser01', credentialId: 'credential-browser01', credentialVersion: 1,
        credentialSecret: 'B'.repeat(43), role: 'controller', locale: 'en',
      });
      const authentication = credentials.get(relayId);
      const acceptedHistoryState = history.state;
      MockWebSocket.instances[0].serverClose(4401);
      expect(get(relayStore.connections).get(relayId)?.authRejected).toBe(false);
      expect(credentials.get(relayId)).toEqual(authentication);
      expect(sessionStorage.length).toBeGreaterThan(0);

      if (entry === 'new session') sessionStorage.clear();
      history.replaceState(entry === 'fresh' ? null : acceptedHistoryState, '', `/pair?source=test${hash}`);
      if (load === 'initialize') {
        relayStore.destroy();
        relayStore.initialize();
      } else {
        expect(relayStore.importSetupLink()).toBe(true);
      }
      expect(get(relayStore.connections).get(relayId)?.pairingDeferred).toBe(true);
      expect(MockWebSocket.instances).toHaveLength(1);
      expect(credentials.get(relayId)).toEqual(authentication);
      expect(location.hash).toBe(hash);
      expect(get(relayStore.toast)?.message).toContain('Home Screen');
    });

    it('keeps bootstrap pairing deferred when its session opt-in cannot be saved', () => {
      const hash = deferBrowserPairing(false);
      vi.spyOn(sessionStorage, 'setItem').mockImplementation(() => { throw new Error('Session storage unavailable.'); });
      expect(relayStore.pairDeferredRelay(relayId)).toBe(false);
      expect(get(relayStore.connections).get(relayId)?.pairingDeferred).toBe(true);
      expect(MockWebSocket.instances).toHaveLength(0);
      expect(location.hash).toBe(hash);
      expect(get(relayStore.toast)).toMatchObject({ message: 'Session storage unavailable.', error: true });
    });

    it.each([false, true])('still defers a new invitation after accepting an earlier one: enrolled=%s', (enrolled) => {
      const hash = deferBrowserPairing();
      expect(relayStore.pairDeferredRelay(relayId)).toBe(true);
      const credentials = new BrowserDeviceCredentialStore(localStorage);
      if (enrolled) {
        credentials.replaceInvitation(relayId, 'invitation-browser01', {
          deviceId: 'device-browser01', credentialId: 'credential-browser01', credentialVersion: 1,
          credentialSecret: 'B'.repeat(43), role: 'controller', locale: 'en',
        });
      }
      const authentication = credentials.get(relayId);
      const nextHash = hash.replace('invitation-browser01', 'invitation-browser02');
      history.replaceState(history.state, '', `/pair?source=test${nextHash}`);
      expect(relayStore.importSetupLink()).toBe(true);
      expect(get(relayStore.connections).get(relayId)?.pairingDeferred).toBe(true);
      expect(credentials.get(relayId)).toEqual(authentication);
      expect(MockWebSocket.instances).toHaveLength(1);
      expect(MockWebSocket.instances[0].readyState).toBe(MockWebSocket.CLOSED);
      expect(location.hash).toBe(nextHash);
    });

    it.each(['missing', 'another relay'])('keeps deferring when the fragment is %s', (fragment) => {
      const hash = deferBrowserPairing();
      history.replaceState(history.state, '', fragment === 'missing'
        ? '/pair?source=test'
        : `/pair?source=test${hash.replace('invited.example', 'other.example')}`);
      const replace = vi.spyOn(history, 'replaceState');
      expect(relayStore.pairDeferredRelay(relayId)).toBe(false);
      expect(get(relayStore.connections).get(relayId)?.pairingDeferred).toBe(true);
      expect(new BrowserDeviceCredentialStore(localStorage).get(relayId)).toBeNull();
      expect(MockWebSocket.instances).toHaveLength(0);
      expect(replace).not.toHaveBeenCalled();
      expect(get(relayStore.toast)?.message).toBe('Open the invitation link again.');
      relayStore.connectAll();
      expect(MockWebSocket.instances).toHaveLength(0);
    });

    it('restores deferral and keeps the fragment when saving an invitation fails', () => {
      const hash = deferBrowserPairing();
      const save = vi.spyOn(BrowserDeviceCredentialStore.prototype, 'saveInvitation')
        .mockImplementation(() => { throw new Error('The device invitation has expired.'); });
      const replace = vi.spyOn(history, 'replaceState');
      expect(relayStore.pairDeferredRelay(relayId)).toBe(false);
      expect(save).toHaveBeenCalledWith(relayId, expect.objectContaining({ id: 'invitation-browser01' }));
      expect(get(relayStore.connections).get(relayId)?.pairingDeferred).toBe(true);
      expect(new BrowserDeviceCredentialStore(localStorage).get(relayId)).toBeNull();
      expect(MockWebSocket.instances).toHaveLength(0);
      expect(location.hash).toBe(hash);
      expect(replace).not.toHaveBeenCalled();
      expect(get(relayStore.toast)).toMatchObject({ message: 'The device invitation has expired.', error: true });
      relayStore.connectAll();
      expect(MockWebSocket.instances).toHaveLength(0);
      save.mockRestore();
      expect(relayStore.pairDeferredRelay(relayId)).toBe(true);
    });

    it('does not pair a relay that is not deferred', () => {
      deferBrowserPairing();
      expect(relayStore.pairDeferredRelay('another-relay')).toBe(false);
      expect(MockWebSocket.instances).toHaveLength(0);
      expect(relayStore.pairDeferredRelay(relayId)).toBe(true);
      expect(relayStore.pairDeferredRelay(relayId)).toBe(false);
      expect(MockWebSocket.instances).toHaveLength(1);
    });
  });

  it('keeps an enrolled credential when the setup link is imported again', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    const relay = { label: 'Paired', url: 'ws://paired.example:8810', token: '' };
    const relayId = makeRelayId(relay.label, relay.url);
    const store = new BrowserDeviceCredentialStore(localStorage);
    store.remove(relayId);
    relayStore.addRelay(relay);
    store.saveInvitation(relayId, {
      id: 'invitation-spent',
      version: 1,
      secret: 'B'.repeat(43),
      expiresAt: Date.now() + 60_000,
    });
    // Pairing turns the one-use invitation into a credential.
    const credential = {
      version: 1,
      relays: {
        [relayId]: {
          kind: 'credential',
          id: 'credential-issued',
          version: 1,
          secret: 'C'.repeat(43),
          deviceId: 'device-issued',
          role: 'reader',
          locale: 'en',
          issuedAt: Date.now(),
          invitationId: 'invitation-spent',
        },
      },
    };
    localStorage.setItem('herdr_device_auth_v1', JSON.stringify(credential));

    // Safari tabs keep the setup fragment so an installed copy can reuse it, so
    // a reload or a back step re-imports it. Restoring the spent invitation
    // here is what made the relay refuse the phone for good.
    const hash = `#setup=${'B'.repeat(43)}&invite=invitation-spent&invite_version=1`
      + `&invite_expires=${Date.now() + 60_000}&label=${relay.label}&relay=${encodeURIComponent(relay.url)}`;
    relayStore.importSetupLink({
      hash,
      protocol: 'http:',
      host: 'paired.example:8810',
      pathname: '/',
      search: '',
    }, false);

    expect(store.get(relayId)).toMatchObject({ kind: 'credential', id: 'credential-issued' });
  });

  it('saves a first invitation on a cold start and dials with it', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    const relayUrl = 'wss://cold.example';
    const relayId = makeRelayId('Cold', relayUrl);
    const store = new BrowserDeviceCredentialStore(localStorage);
    store.remove(relayId);
    vi.stubGlobal('navigator', { standalone: undefined, userAgent: 'Mozilla/5.0 (X11; Linux x86_64)', maxTouchPoints: 0 });
    const hash = `#setup=${'D'.repeat(43)}&invite=invitation-cold01&invite_version=1`
      + `&invite_expires=${Date.now() + 60_000}&label=Cold&relay=${encodeURIComponent(relayUrl)}`;

    expect(relayStore.importSetupLink({
      hash, protocol: 'https:', host: 'app.example', pathname: '/', search: '',
    }, true)).toBe(true);

    expect(store.get(relayId)).toMatchObject({ kind: 'invitation', id: 'invitation-cold01', version: 1 });
    expect(MockWebSocket.instances).toHaveLength(1);
    expect(MockWebSocket.instances[0].protocols).toBe('herdr-e2ee-v2');
  });

  it('replaces an enrolled credential only with an invitation minted after it', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    const relayUrl = 'wss://renewed.example';
    const relayId = makeRelayId('Renewed', relayUrl);
    const store = new BrowserDeviceCredentialStore(localStorage);
    const issuedAt = Date.now();
    const storeCredential = () => localStorage.setItem('herdr_device_auth_v1', JSON.stringify({
      version: 1,
      relays: {
        [relayId]: {
          kind: 'credential', id: 'credential-live', version: 1, secret: 'E'.repeat(43),
          deviceId: 'device-live', role: 'controller', locale: 'en',
          issuedAt, invitationId: 'invitation-spent1',
        },
      },
    }));
    const importInvitation = (id: string, expiresAt: number) => relayStore.importSetupLink({
      hash: `#setup=${'F'.repeat(43)}&invite=${id}&invite_version=1`
        + `&invite_expires=${expiresAt}&label=Renewed&relay=${encodeURIComponent(relayUrl)}`,
      protocol: 'https:',
      host: 'app.example',
      pathname: '/',
      search: '',
    }, false);

    // A different invitation that expired before this credential was issued is
    // an older link resurfacing — importing it would trade a working
    // credential for material the relay has already retired.
    storeCredential();
    expect(importInvitation('invitation-stale01', issuedAt - 1_000)).toBe(true);
    expect(store.get(relayId)).toMatchObject({ kind: 'credential', id: 'credential-live' });

    // A newer invitation is a deliberate re-pair and takes over.
    expect(importInvitation('invitation-fresh01', issuedAt + 60_000)).toBe(true);
    expect(store.get(relayId)).toMatchObject({ kind: 'invitation', id: 'invitation-fresh01' });
  });

  it('stops dialing an invitation-paired relay once this browser is forgotten', async () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    const relayUrl = 'wss://forgotten.example';
    const relayId = makeRelayId('Forgotten', relayUrl);
    const store = new BrowserDeviceCredentialStore(localStorage);
    store.remove(relayId);
    vi.stubGlobal('navigator', { standalone: undefined, userAgent: 'Mozilla/5.0 (X11; Linux x86_64)', maxTouchPoints: 0 });
    // The real encrypted transport cannot complete its handshake against a mock
    // socket, and this test is about what happens after pairing, so the path
    // itself is stubbed and its dials counted.
    let dials = 0;
    let report: (status: TransportStatus, detail?: TransportStatusDetail) => void = () => {};
    let deliver: (message: Record<string, unknown>) => void = () => {};
    const sent: Record<string, string>[] = [];
    transportHijack.current = (_relay, handlers) => ({
      kind: 'websocket',
      connect: () => {
        dials += 1;
        report = handlers.onStatus;
        deliver = handlers.onMessage;
        handlers.onStatus('connecting');
      },
      send: (payload) => {
        sent.push(payload as unknown as Record<string, string>);
        return true;
      },
      close: () => {},
    });
    expect(relayStore.importSetupLink({
      hash: `#setup=${'G'.repeat(43)}&invite=invitation-forget01&invite_version=1`
        + `&invite_expires=${Date.now() + 60_000}&label=Forgotten&relay=${encodeURIComponent(relayUrl)}`,
      protocol: 'https:',
      host: 'app.example',
      pathname: '/',
      search: '',
    }, true)).toBe(true);

    // An invitation-paired entry keeps no relay key, so the entry itself has to
    // remember that this relay speaks the encrypted handshake.
    const stored = JSON.parse(localStorage.getItem('herdr_relays')!) as RelayConfig[];
    expect(stored.find((relay) => relay.id === relayId)).toMatchObject({ token: '', paired: true });
    expect(dials).toBe(1);

    report('connected', { path: 'websocket' });
    deliver({ type: 'push_config', protocol: 3, host: 'forgotten', capabilities: [], agent_profiles: [] });
    // Pairing turned the invitation into a credential.
    localStorage.setItem('herdr_device_auth_v1', JSON.stringify({
      version: 1,
      relays: {
        [relayId]: {
          kind: 'credential', id: 'credential-forget', version: 1, secret: 'H'.repeat(43),
          deviceId: 'device-forget', role: 'controller', locale: 'en',
          issuedAt: Date.now(), invitationId: 'invitation-forget01',
        },
      },
    }));

    const forgotten = relayStore.forgetCurrentDevice(relayId);
    const revoke = await vi.waitFor(() => {
      const command = sent.find((entry) => entry.type === 'revoke_device');
      expect(command).toBeTruthy();
      return command as { request_id: string };
    });
    deliver({ type: 'command_result', request_id: revoke.request_id, action: 'revoke_device', ok: true, phase: 'completed' });
    // The device refresh that follows finds no connection to ask, which must
    // still leave the forget itself successful rather than reported as failed.
    await forgotten;
    expect(sent.some((entry) => entry.type === 'device_list')).toBe(false);

    expect(store.get(relayId)).toBeNull();
    expect(get(relayStore.toast)?.message).toContain('Forgotten needs pairing');
    expect(get(relayStore.connections).get(relayId)?.pairingRequired).toBe(true);

    // The bug this guards: a plaintext ladder against an encrypted relay, with
    // no credential, no relay key, and nothing said to the person holding it.
    const dialsAtForget = dials;
    relayStore.revalidateConnections();
    relayStore.resetReconnectBackoff();
    relayStore.connectAll(true);
    expect(dials).toBe(dialsAtForget);
  });


  function openPaneSizeRelay(capabilities: string[]) {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 3,
      version: 'abc123',
      host: 'fedora',
      capabilities,
      agent_profiles: [],
    });
    socket.message({
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'working', agent: 'omp', ...exactAgentFields() }],
    });
    const sent = () => socket.sent.map((payload) => JSON.parse(payload));
    return { socket, agent: get(relayStore.agents)[0], sent };
  }

  it('never leases against a relay that only advertises the legacy capability', async () => {
    setTerminalResizeSession(true);
    setTerminalHeightLease(true);
    const { agent, sent } = openPaneSizeRelay(['pane_size_lease', 'pane_size_lease_rows']);

    await expect(relayStore.leasePaneSize(agent, 83, 32)).rejects.toThrow(/unavailable on this relay/);
    await expect(relayStore.leasePaneSize(agent, 80, 0)).rejects.toThrow(/unavailable on this relay/);
    expect(sent().filter((message) => message.type === 'lease_pane_size')).toHaveLength(0);
  });

  it('does not lease while Resize Session is off, even on a capable relay', async () => {
    const { agent, sent } = openPaneSizeRelay(['pane_size_opt_in', 'pane_size_opt_in_rows']);
    await expect(relayStore.leasePaneSize(agent, 83, 32)).rejects.toThrow(/Resize Session is off/);
    expect(sent().filter((message) => message.type === 'lease_pane_size')).toHaveLength(0);
  });

  it('leases with opt_in and releases immediately when Resize Session is turned off', async () => {
    setTerminalResizeSession(true);
    const { socket, agent, sent } = openPaneSizeRelay(['pane_size_opt_in', 'pane_size_opt_in_rows']);

    const lease = relayStore.leasePaneSize(agent, 83, 32);
    const leaseCommand = sent().findLast((message) => message.type === 'lease_pane_size');
    expect(leaseCommand).toMatchObject({
      type: 'lease_pane_size', pane_id: 'w1:p1', protocol: 3, columns: 83, rows: 32, opt_in: true,
    });
    expect(leaseCommand).not.toHaveProperty('client_id');
    socket.message({
      type: 'command_result', action: 'lease_pane_size', request_id: leaseCommand.request_id,
      ok: true, data: { columns: 83, rows: 32 },
    });
    await expect(lease).resolves.toEqual({ columns: 83, rows: 32 });

    // Leaving the terminal keeps the relay's navigation grace.
    const release = relayStore.releasePaneSize(agent);
    const graceRelease = sent().findLast((message) => message.type === 'release_pane_size');
    expect(graceRelease).toMatchObject({ type: 'release_pane_size', pane_id: 'w1:p1', protocol: 3 });
    expect(graceRelease).not.toHaveProperty('immediate');
    expect(graceRelease).not.toHaveProperty('client_id');
    socket.message({ type: 'command_result', action: 'release_pane_size', request_id: graceRelease.request_id, ok: true });
    await expect(release).resolves.toBeUndefined();

    // Turning the switch off ends that grace at once, view or no view.
    setTerminalResizeSession(false);
    const releases = sent().filter((message) => message.type === 'release_pane_size');
    expect(releases).toHaveLength(2);
    expect(releases[1]).toMatchObject({ type: 'release_pane_size', pane_id: 'w1:p1', immediate: true });
    socket.message({ type: 'command_result', action: 'release_pane_size', request_id: releases[1].request_id, ok: true });

    // Nothing is owned any more: turning it on and off again sends nothing.
    setTerminalResizeSession(true);
    setTerminalResizeSession(false);
    expect(sent().filter((message) => message.type === 'release_pane_size')).toHaveLength(2);
  });

  it('ignores a lease reply that arrives after Resize Session was turned off', async () => {
    setTerminalResizeSession(true);
    const { socket, agent, sent } = openPaneSizeRelay(['pane_size_opt_in']);
    const lease = relayStore.leasePaneSize(agent, 83, 32);
    const leaseCommand = sent().findLast((message) => message.type === 'lease_pane_size');
    expect(leaseCommand).not.toHaveProperty('rows');

    setTerminalResizeSession(false);
    const release = sent().findLast((message) => message.type === 'release_pane_size');
    expect(release).toMatchObject({ pane_id: 'w1:p1', immediate: true });
    socket.message({
      type: 'command_result', action: 'lease_pane_size', request_id: leaseCommand.request_id,
      ok: true, data: { columns: 83, rows: 40 },
    });
    await expect(lease).rejects.toThrow(/turned off/);
  });

  it('replaces a stale connecting attempt on revalidation but keeps a young one', () => {
    // The socket never opens: the connection sits in 'connecting', like a
    // dial started before the phone's radio came back after sleep.
    expect(MockWebSocket.instances).toHaveLength(1);
    vi.useFakeTimers();

    // Focus events fire on every app switch; a young dial must not be churned.
    relayStore.revalidateConnections();
    expect(MockWebSocket.instances).toHaveLength(1);

    // Once the revalidation event postdates the dial by the stale threshold,
    // the blackholed attempt is replaced instead of waiting out its own
    // handshake timeout plus reconnect backoff.
    vi.advanceTimersByTime(5_000);
    relayStore.revalidateConnections();
    expect(MockWebSocket.instances).toHaveLength(2);
  });

  it('binds approvals to their blocked event and coalesces pane polling', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora',
      capabilities: ['attention_classification'], agent_profiles: [],
    });
    socket.message({
      type: 'agents',
      agents: [{
        pane_id: 'w1:p1', status: 'blocked', event_id: 'event-1', agent: 'codex',
        ...exactAgentFields(),
        attention_kind: 'approval', options: ['Approve once', 'Deny'],
        approval_fingerprint: 'approval-fingerprint-1',
      }],
    });
    const agent = get(relayStore.agents)[0];

    relayStore.readPane(agent);
    relayStore.readPane(agent);
    expect(socket.sent.map((payload) => JSON.parse(payload)).filter((message) => message.type === 'read_pane')).toHaveLength(1);
    socket.message({ type: 'pane_content', request_id: JSON.parse(socket.sent.at(-1)!).request_id, pane_id: 'w1:p1', target: exactWireScope('w1:p1', agent.relay_id).target, content: 'blocked', format: 'ansi' });
    relayStore.readPane(agent);
    expect(socket.sent.map((payload) => JSON.parse(payload)).filter((message) => message.type === 'read_pane')).toHaveLength(2);

    const approval = relayStore.respond(agent, 0, 2, 'Approve once');
    const command = socket.sent.map((payload) => JSON.parse(payload)).findLast((message) => message.type === 'respond');
    expect(command).toMatchObject({
      pane_id: 'w1:p1',
      event_id: 'event-1',
      approval_fingerprint: 'approval-fingerprint-1',
      choice: 'Approve once',
      index: 0,
      total: 2,
    });
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'confirmed' });
    await expect(approval).resolves.toBe(true);
    await vi.advanceTimersByTimeAsync(500);
    expect(socket.sent.map((payload) => JSON.parse(payload)).filter((message) => message.type === 'read_pane')).toHaveLength(2);
  });

  it('does not let an old pane generation suppress a replacement read', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora',
      capabilities: [], agent_profiles: [],
    });
    socket.message({
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'working', agent: 'codex', ...exactAgentFields() }],
    });
    relayStore.readPane(get(relayStore.agents)[0]);
    socket.message({
      type: 'agents',
      agents: [{
        pane_id: 'w1:p1',
        status: 'working',
        agent: 'codex',
        server_session_id: 'primary',
        terminal_id: 'terminal-replacement',
        generation: 2,
        agent_session_id: 'session-replacement',
      }],
    });
    relayStore.readPane(get(relayStore.agents)[0]);
    const reads = socket.sent.map((payload) => JSON.parse(payload))
      .filter((message) => message.type === 'read_pane');
    expect(reads).toHaveLength(2);
    expect(reads[1].target).toMatchObject({
      pane_id: 'w1:p1',
      terminal_id: 'terminal-replacement',
      generation: 2,
      agent_session_id: 'session-replacement',
    });
  });

  it('does not trust blocked controls from an old relay', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'old', host: 'fedora',
      capabilities: ['structured_questions'], agent_profiles: [],
    });
    socket.message({
      type: 'agents',
      agents: [{
        pane_id: 'w1:p1', status: 'blocked', event_id: 'event-1', agent: 'codex',
        options: ['Approve once', 'Deny'],
        ...exactAgentFields(),
        interaction: {
          id: 'old-question', kind: 'single_select', question: 'Trust this?',
          options: [{ index: 0, label: 'Yes' }],
        },
        question_layout: true,
      }],
    });
    const agent = get(relayStore.agents)[0];
    expect(agent).toMatchObject({
      attention_kind: 'unknown',
      attention_capable: false,
      interaction: null,
      question_layout: false,
    });
    expect(agent.options).toBeUndefined();
  });

  it('deletes persisted activity through its relay and clears the merged view', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, host: 'fedora',
      capabilities: ['clear_activities'], agent_profiles: [],
    });
    socket.message({
      type: 'activity_history',
      activities: [{ id: 'activity-1', timestamp: 123, summary: 'Prompt sent' }],
    });
    expect(get(relayStore.activities)).toHaveLength(1);

    const pending = relayStore.clearActivities();
    const command = JSON.parse(socket.sent.at(-1)!);
    expect(command).toMatchObject({ type: 'clear_activities', protocol: 3 });
    socket.message({
      type: 'command_result', request_id: command.request_id,
      action: 'clear_activities', ok: true, phase: 'completed',
    });

    await expect(pending).resolves.toBeUndefined();
    expect(get(relayStore.activities)).toEqual([]);
  });

  it('rejects mutations on protocol mismatch', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 1, host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    await expect(relayStore.sendCommand(relayId, { type: 'agent_stop', pane_id: 'w1:p1' })).rejects.toThrow(/protocol v1/);
  });

  it('retains agents, blocks mutations, and recovers when inventory becomes ready', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 3,
      host: 'fedora',
      capabilities: [],
      agent_profiles: [],
      inventory: { state: 'ready', last_attempt_at: 100, last_success_at: 100 },
    });
    socket.message({
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Relay', agent: 'codex', ...exactAgentFields() }],
    });
    socket.message({
      type: 'inventory_status',
      state: 'error',
      error_code: 'protocol_mismatch',
      message: 'Run `herdr server live-handoff` on this computer, then refresh.',
      last_attempt_at: 123,
      last_success_at: 100,
      stale: true,
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const before = socket.sent.length;

    await expect(
      relayStore.sendCommand(relayId, { type: 'agent_stop', pane_id: 'w1:p1' }),
    ).rejects.toThrow(/live-handoff/);
    expect(socket.sent).toHaveLength(before);
    expect(get(relayStore.agents)).toHaveLength(1);
    expect(get(relayStore.connections).get(relayId)?.inventory).toMatchObject({
      state: 'error',
      errorCode: 'protocol_mismatch',
      stale: true,
    });

    socket.message({
      type: 'inventory_status',
      state: 'error',
      error_code: 'protocol_mismatch',
      message: 'Run `herdr server live-handoff` on this computer, then refresh.',
      last_attempt_at: 124,
      last_success_at: 0,
      stale: false,
    });
    socket.message({ type: 'agents', agents: [] });
    expect(get(relayStore.agents)).toHaveLength(1);

    socket.message({
      type: 'inventory_status',
      state: 'ready',
      error_code: '',
      message: '',
      last_attempt_at: 200,
      last_success_at: 200,
      stale: false,
    });
    socket.message({ type: 'agents', agents: [] });
    expect(get(relayStore.agents)).toHaveLength(0);
    const pending = relayStore.sendCommand(relayId, { type: 'agent_stop', pane_id: 'w1:p1' });
    const command = JSON.parse(socket.sent.at(-1)!);
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true });
    await expect(pending).resolves.toMatchObject({ ok: true });
    expect(get(relayStore.connections).get(relayId)?.inventory.state).toBe('ready');
  });

  it('checks and schedules an exact relay update across a protocol mismatch', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 99,
      version: 'abc123',
      release_version: '0.7.0',
      revision: 'abc123',
      capabilities: ['self_update'],
      agent_profiles: [],
      update: {
        state: 'available',
        current_version: '0.7.0',
        current_revision: 'abc123',
        available_version: '0.8.0',
        available_revision: 'f'.repeat(12),
        target_revision: 'f'.repeat(40),
        can_install: true,
        mode: 'local',
      },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;

    const checking = relayStore.checkRelayUpdate(relayId);
    const checkCommand = JSON.parse(socket.sent.at(-1)!);
    expect(checkCommand).toMatchObject({ type: 'check_update', protocol: 3 });
    socket.message({
      type: 'command_result',
      request_id: checkCommand.request_id,
      ok: true,
      phase: 'confirmed',
      data: {
        update: {
          state: 'available',
          current_version: '0.7.0',
          available_version: '0.8.0',
          target_revision: 'f'.repeat(40),
          can_install: true,
          mode: 'local',
        },
      },
    });
    await checking;

    const installing = relayStore.installRelayUpdate(relayId);
    const installCommand = JSON.parse(socket.sent.at(-1)!);
    expect(installCommand).toMatchObject({
      type: 'install_update',
      expected_version: '0.8.0',
      expected_revision: 'f'.repeat(40),
      expected_origin: location.origin,
      protocol: 3,
    });
    socket.message({
      type: 'command_result',
      request_id: installCommand.request_id,
      ok: true,
      phase: 'scheduled',
      data: { update: { state: 'scheduled', target_revision: 'f'.repeat(40) } },
    });
    await installing;

    expect(pendingRelayUpdate(relayId)).toEqual({
      version: '0.8.0',
      revision: 'f'.repeat(40),
    });
  });

  it('clears a pending relay update after an explicit scheduling failure', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 3,
      release_version: '0.7.0',
      revision: 'abc123',
      capabilities: ['self_update'],
      agent_profiles: [],
      update: {
        state: 'available',
        available_version: '0.8.0',
        target_revision: 'e'.repeat(40),
        can_install: true,
      },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const installing = relayStore.installRelayUpdate(relayId);
    const command = JSON.parse(socket.sent.at(-1)!);

    socket.message({
      type: 'command_result',
      request_id: command.request_id,
      ok: false,
      phase: 'failed',
      error: 'Update changed',
      data: { update: { state: 'failed', error: 'Update changed' } },
    });

    await expect(installing).rejects.toThrow('Update changed');
    expect(pendingRelayUpdate(relayId)).toBeNull();
  });

  it('requests a fresh agent snapshot on connect and on demand', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    expect(JSON.parse(socket.sent.at(-1)!)).toEqual({ type: 'refresh_agents' });

    relayStore.requestAgents();
    expect(socket.sent.map((payload) => JSON.parse(payload).type)).toEqual([
      'refresh_agents',
      'refresh_agents',
    ]);
  });

  it('privately registers the hosting origin only for an enrolled controller', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    vi.stubGlobal('matchMedia', vi.fn(() => ({
      matches: true,
      media: '(display-mode: standalone)',
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })));
    const relay = { label: 'Fedora', url: 'ws://127.0.0.1:8871', token: '' };
    const relayId = makeRelayId(relay.label, relay.url);
    relayStore.addRelay(relay);
    localStorage.setItem('herdr_device_auth_v1', JSON.stringify({
      version: 1,
      relays: {
        [relayId]: {
          kind: 'credential',
          id: 'credential-controller',
          version: 1,
          secret: 'C'.repeat(43),
          deviceId: 'device-controller',
          role: 'controller',
          locale: 'en',
          issuedAt: Date.now(),
          invitationId: 'invitation-controller',
        },
      },
    }));

    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    const sent = socket.sent.map((payload) => JSON.parse(payload));

    expect(sent).toEqual([
      {
        type: 'register_app_origin',
        origin: location.origin,
        protocol: 3,
      },
      { type: 'refresh_agents' },
    ]);
  });

  it('does not let an installed reader credential register an app origin', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    vi.stubGlobal('matchMedia', vi.fn(() => ({
      matches: true,
      media: '(display-mode: standalone)',
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })));
    const relay = { label: 'Fedora', url: 'ws://127.0.0.1:8871', token: '' };
    const relayId = makeRelayId(relay.label, relay.url);
    relayStore.addRelay(relay);
    localStorage.setItem('herdr_device_auth_v1', JSON.stringify({
      version: 1,
      relays: {
        [relayId]: {
          kind: 'credential',
          id: 'credential-reader',
          version: 1,
          secret: 'R'.repeat(43),
          deviceId: 'device-reader',
          role: 'reader',
          locale: 'en',
          issuedAt: Date.now(),
          invitationId: 'invitation-reader',
        },
      },
    }));

    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    expect(socket.sent.map((payload) => JSON.parse(payload))).toEqual([
      { type: 'refresh_agents' },
    ]);
  });

  it('requests the default and configured numbers of terminal history lines', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId,
      relay_label: 'Fedora',
      raw_pane_id: 'w1:p1',
      ...exactAgentFields(),
      pane_id: `${relayId}::w1:p1`,
    };

    relayStore.readPane(agent);
    expect(JSON.parse(socket.sent.at(-1)!)).toEqual({
      type: 'read_pane',
      request_id: expect.any(String),
      pane_id: 'w1:p1',
      lines: 1_000,
      format: 'ansi',
      content_fingerprint: '',
      ...exactWireScope('w1:p1', relayId),
    });

    setTerminalHistoryLines(500);
    relayStore.readPane(agent, true);
    expect(JSON.parse(socket.sent.at(-1)!)).toEqual({
      type: 'read_pane',
      request_id: expect.any(String),
      pane_id: 'w1:p1',
      lines: 500,
      format: 'ansi',
      content_fingerprint: '',
      ...exactWireScope('w1:p1', relayId),
    });
    setTerminalHistoryLines(1_000);
  });

  it.each([100, 500, 1_000, 10_000] as const)('preserves %i-line history during repeated manual context refreshes', async lines => {
    vi.useFakeTimers();
    setTerminalHistoryLines(lines);
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: ['manual_terminal_input'], inventory: { state: 'ready' } });
    socket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'idle', agent: 'pi', ...exactAgentFields() }] });
    const agent = get(relayStore.agents)[0];
    const target = exactWireScope('w1:p1', agent.relay_id).target;
    const history = Array.from({ length: 200 }, (_, index) => `history-entry-${index}`).join('\n');
    const expected = history.split('\n').slice(-lines).join('\n');
    socket.message({ type: 'pane_content', pane_id: 'w1:p1', target, content: expected, format: 'ansi', no_echo: false });
    const panel = render(ManualTerminalInput, { agent, frame: get(relayStore.terminalFrames).get(agent.pane_id), active: true, readOnly: false, delivery: new TerminalInputDelivery(), onexit: () => {} });
    for (let iteration = 0; iteration < 3; iteration++) {
      if (iteration) await vi.advanceTimersByTimeAsync(2_000);
      const reads = socket.sent.map(payload => JSON.parse(payload)).filter(message => message.type === 'read_pane');
      expect(reads).toHaveLength(iteration + 1);
      const request = reads.at(-1)!;
      expect(request).toMatchObject({ lines, content_fingerprint: '', target });
      socket.message({ type: 'pane_content', request_id: request.request_id, pane_id: 'w1:p1', target, content: history.split('\n').slice(-request.lines).join('\n'), format: 'ansi', no_echo: false });
      const refreshed = get(relayStore.terminalFrames).get(agent.pane_id)!;
      expect(refreshed.content).toBe(expected);
      expect(refreshed.content.split('\n').length).toBeGreaterThan(30);
      await panel.rerender({ frame: refreshed });
    }
    panel.unmount();
    expect(socket.sent.map(payload => JSON.parse(payload)).filter(message => message.type.startsWith('send_'))).toHaveLength(0);
  });

  it('correlates inspection freshness with read issuance, not a delayed response arrival', () => {
    let now = 10;
    vi.spyOn(performance, 'now').mockImplementation(() => now);
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    socket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', ...exactAgentFields() }] });
    const agent = get(relayStore.agents)[0];
    const target = exactWireScope('w1:p1', agent.relay_id).target;
    relayStore.readPane(agent, true);
    const oldRead = JSON.parse(socket.sent.at(-1)!);
    const delivery = new TerminalInputDelivery();
    delivery.begin();
    now = 20;
    delivery.finish(true);
    now = 30;
    const oldResponse = { type: 'pane_content', request_id: oldRead.request_id, pane_id: 'w1:p1', target, content: 'before-send observation', format: 'ansi', no_echo: false };
    socket.message(oldResponse);
    const frame = () => get(relayStore.terminalFrames).get(agent.pane_id)!;
    expect(frame()).toMatchObject({ readStartedAt: 10, receivedAt: 30 });
    expect(delivery.inspect(frame().readStartedAt)).toBe(false);
    now = 40;
    relayStore.readPane(agent, true);
    const newRead = JSON.parse(socket.sent.at(-1)!);
    expect(newRead.request_id).not.toBe(oldRead.request_id);
    now = 50;
    socket.message({ ...oldResponse, content: 'superseded response' });
    expect(frame().content).toBe('before-send observation');
    expect(delivery.inspect(frame().readStartedAt)).toBe(false);
    now = 60;
    socket.message({ ...oldResponse, request_id: newRead.request_id, content: 'post-outcome observation' });
    expect(frame()).toMatchObject({ readStartedAt: 40, receivedAt: 60, content: 'post-outcome observation' });
    expect(delivery.canInspect(frame().readStartedAt)).toBe(true);
    expect(delivery.uncertain).toBe(true);
    socket.message(oldResponse);
    expect(frame().content).toBe('post-outcome observation');
    expect(delivery.inspect(frame().readStartedAt)).toBe(true);
    expect(socket.sent.map(payload => JSON.parse(payload)).filter(message => message.type.startsWith('send_'))).toHaveLength(0);
  });

  it('requires a post-unlock correlated read before a pane delta can authorize manual input', async () => {
    let now = 10;
    vi.spyOn(performance, 'now').mockImplementation(() => now);
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: ['manual_terminal_input'], inventory: { state: 'ready' } });
    socket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'idle', agent: 'pi', ...exactAgentFields() }] });
    const agent = get(relayStore.agents)[0];
    const target = exactWireScope('w1:p1', agent.relay_id).target;
    const security = get(securityState);
    securityState.set({ ...security, locked: false });
    const delivery = new TerminalInputDelivery();
    const panel = render(ManualTerminalInput, { agent, active: true, readOnly: false, delivery, onexit: () => {} });
    const frame = () => get(relayStore.terminalFrames).get(agent.pane_id)!;
    const reads = () => socket.sent.map(payload => JSON.parse(payload)).filter(message => message.type === 'read_pane');
    try {
      const initial = reads().at(-1)!;
      now = 20;
      const response = { type: 'pane_content', request_id: initial.request_id, pane_id: 'w1:p1', target, content: 'terminal context', content_fingerprint: 'same-screen', format: 'ansi', no_echo: false };
      socket.message(response);
      await panel.rerender({ frame: frame() });
      expect(screen.getByRole('button', { name: 'Enter' })).toBeEnabled();
      now = 30;
      securityState.set({ ...security, locked: true });
      await tick();
      now = 40;
      securityState.set({ ...security, locked: false });
      await tick();
      const recovery = reads().at(-1)!;
      expect(recovery.request_id).not.toBe(initial.request_id);
      const delta = { type: 'pane_delta', pane_id: 'w1:p1', target, base_fingerprint: 'same-screen', content_fingerprint: 'same-screen', segments: null, format: 'ansi' };
      now = 50;
      socket.message(delta);
      expect(frame()).toMatchObject({ receivedAt: 50 });
      expect(frame().readStartedAt).toBeUndefined();
      await panel.rerender({ frame: frame() });
      await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'unverified delta draft' } });
      for (const label of ['Send text', 'Enter']) {
        const button = screen.getByRole('button', { name: label });
        expect(button).toBeDisabled();
        await fireEvent.click(button);
      }
      expect(delivery.uncertain).toBe(false);
      expect(socket.sent.map(payload => JSON.parse(payload)).filter(message => message.type.startsWith('send_'))).toHaveLength(0);
      now = 60;
      socket.message({ ...response, request_id: recovery.request_id });
      expect(frame()).toMatchObject({ readStartedAt: 40, receivedAt: 60 });
      await panel.rerender({ frame: frame() });
      await tick();
      expect(screen.getByRole('button', { name: 'Send text' })).toBeEnabled();
      now = 70;
      socket.message(delta);
      expect(frame().readStartedAt).toBeUndefined();
      await panel.rerender({ frame: frame() });
      expect(screen.getByRole('button', { name: 'Send text' })).toBeEnabled();
      expect(screen.getByRole('button', { name: 'Enter' })).toBeEnabled();
      expect(JSON.stringify(localStorage)).not.toContain('unverified delta draft');
    } finally {
      panel.unmount();
      securityState.set(security);
    }
  });

  it('keeps a correlated pre-lock response fenced until a post-unlock read and explicit inspection', async () => {
    let now = 10;
    vi.spyOn(performance, 'now').mockImplementation(() => now);
    vi.spyOn(Date, 'now').mockImplementation(() => now);
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: ['manual_terminal_input'], inventory: { state: 'ready' } });
    socket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'idle', agent: 'pi', ...exactAgentFields() }] });
    const agent = get(relayStore.agents)[0];
    const target = exactWireScope('w1:p1', agent.relay_id).target;
    const delivery = new TerminalInputDelivery();
    expect(delivery.begin()).toBe(true);
    now = 20;
    delivery.finish(true);
    const security = get(securityState);
    securityState.set({ ...security, locked: false });
    const panel = render(ManualTerminalInput, { agent, active: false, readOnly: false, delivery, onexit: () => {} });
    const frame = () => get(relayStore.terminalFrames).get(agent.pane_id)!;
    const reads = () => socket.sent.map(payload => JSON.parse(payload)).filter(message => message.type === 'read_pane');
    try {
      now = 30;
      relayStore.readPane(agent, true);
      const delayed = reads().at(-1)!;
      now = 40;
      securityState.set({ ...security, locked: true });
      await tick();
      now = 50;
      securityState.set({ ...security, locked: false });
      await tick();
      now = 60;
      relayStore.readPane(agent);
      expect(reads()).toHaveLength(1);
      const response = { type: 'pane_content', request_id: delayed.request_id, pane_id: 'w1:p1', target, content: 'delayed pre-lock observation', format: 'ansi', no_echo: false };
      socket.message(response);
      expect(frame()).toMatchObject({ readStartedAt: 30, receivedAt: 60, content: 'delayed pre-lock observation' });
      expect(delivery.canInspect(frame().readStartedAt)).toBe(true);
      await panel.rerender({ active: true, frame: frame() });
      const recovery = reads().at(-1)!;
      expect(recovery.request_id).not.toBe(delayed.request_id);
      await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'transient recovery text' } });
      for (const label of ['Send text', 'Enter', 'I checked the terminal']) {
        const button = screen.getByRole('button', { name: label });
        expect(button).toBeDisabled();
        await fireEvent.click(button);
      }
      expect(delivery.uncertain).toBe(true);
      socket.message({ ...response, content: 'superseded pre-lock observation' });
      expect(frame().content).toBe('delayed pre-lock observation');
      now = 70;
      socket.message({ ...response, request_id: recovery.request_id, content: 'post-unlock observation' });
      expect(frame()).toMatchObject({ readStartedAt: 60, receivedAt: 70, content: 'post-unlock observation' });
      await panel.rerender({ frame: frame() });
      expect(screen.getByRole('button', { name: 'I checked the terminal' })).toBeEnabled();
      expect(screen.getByRole('button', { name: 'Enter' })).toBeDisabled();
      expect(delivery.uncertain).toBe(true);
      await fireEvent.click(screen.getByRole('button', { name: 'I checked the terminal' }));
      expect(delivery.uncertain).toBe(false);
      expect(screen.getByRole('button', { name: 'Enter' })).toBeEnabled();
      expect(screen.getByRole('button', { name: 'Send text' })).toBeEnabled();
      expect(socket.sent.map(payload => JSON.parse(payload)).filter(message => message.type.startsWith('send_'))).toHaveLength(0);
      expect(JSON.stringify(localStorage)).not.toContain('transient recovery text');
    } finally {
      panel.unmount();
      securityState.set(security);
    }
  });

  it.each(['untagged', 'unknown-id', 'error', 'replacement', 'disconnected'] as const)('does not authorize inspection from a %s read response', kind => {
    let now = 10;
    vi.spyOn(performance, 'now').mockImplementation(() => now);
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    socket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', ...exactAgentFields() }] });
    const agent = get(relayStore.agents)[0];
    const target = exactWireScope('w1:p1', agent.relay_id).target;
    const delivery = new TerminalInputDelivery();
    delivery.begin();
    delivery.finish(true);
    now = 20;
    relayStore.readPane(agent, true);
    const request = JSON.parse(socket.sent.at(-1)!);
    if (kind === 'disconnected') relayStore.disconnectRelay(agent.relay_id);
    now = 30;
    socket.message({ type: 'pane_content', request_id: kind === 'untagged' ? undefined : kind === 'unknown-id' ? 'unsolicited' : request.request_id, pane_id: 'w1:p1', target: kind === 'replacement' ? { ...target, generation: 2 } : target, error: kind === 'error' ? 'read failed' : undefined, content: 'untrusted observation', format: 'ansi', no_echo: false });
    const frame = get(relayStore.terminalFrames).get(agent.pane_id);
    expect(frame?.readStartedAt).toBeUndefined();
    expect(delivery.inspect(frame?.readStartedAt)).toBe(false);
    if (kind === 'error') expect(frame?.inputTarget).toBeUndefined();
  });

  it('applies acknowledged terminal deltas and resynchronizes mismatched bases', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 3,
      capabilities: ['pane_realtime_delta'],
      inventory: { state: 'ready' },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId,
      relay_label: 'Fedora',
      raw_pane_id: 'w1:p1',
      ...exactAgentFields(),
      pane_id: `${relayId}::w1:p1`,
    };
    relayStore.watchPane(agent);
    socket.message({
      type: 'pane_content',
      pane_id: 'w1:p1',
      content: 'one\ntwo\nthree\nfour\n',
      format: 'ansi',
      content_fingerprint: 'content-1',
    });
    socket.message({
      type: 'pane_content',
      pane_id: 'w1:p1',
      content: 'stale replacement output\n',
      format: 'ansi',
      content_fingerprint: 'stale-content',
      target: {
        ...exactWireScope('w1:p1', relayId).target,
        generation: 0,
      },
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)?.content)
      .toBe('one\ntwo\nthree\nfour\n');

    expect(JSON.parse(socket.sent.at(-1)!)).toEqual({
      type: 'watch_pane',
      pane_id: 'w1:p1',
      lines: 1_000,
      interval_ms: 250,
      format: 'ansi',
      content_fingerprint: 'content-1',
      ...exactWireScope('w1:p1', relayId),
    });
    setTerminalRefreshInterval(100);
    expect(JSON.parse(socket.sent.at(-1)!)).toMatchObject({
      type: 'watch_pane',
      pane_id: 'w1:p1',
      interval_ms: 100,
      content_fingerprint: 'content-1',
    });
    setTerminalRefreshInterval(250);
    socket.message({
      type: 'pane_delta',
      pane_id: 'w1:p1',
      format: 'ansi',
      base_fingerprint: 'content-1',
      content_fingerprint: 'content-1',
      segments: null,
      question_layout: true,
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)?.content)
      .toBe('one\ntwo\nthree\nfour\n');
    expect(JSON.parse(socket.sent.at(-1)!)).toEqual({
      type: 'pane_applied',
      pane_id: 'w1:p1',
      content_fingerprint: 'content-1',
      ...exactWireScope('w1:p1', relayId),
    });

    socket.message({
      type: 'pane_delta',
      pane_id: 'w1:p1',
      format: 'ansi',
      base_fingerprint: 'content-1',
      content_fingerprint: 'content-2',
      segments: [
        { copy_lines: 4 },
        { text: 'five\n' },
      ],
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)?.content)
      .toBe('one\ntwo\nthree\nfour\nfive\n');
    expect(JSON.parse(socket.sent.at(-1)!)).toEqual({
      type: 'pane_applied',
      pane_id: 'w1:p1',
      content_fingerprint: 'content-2',
      ...exactWireScope('w1:p1', relayId),
    });

    socket.message({
      type: 'pane_delta',
      pane_id: 'w1:p1',
      base_fingerprint: 'missing-base',
      content_fingerprint: 'content-3',
      segments: [{ text: 'invalid' }],
    });
    expect(JSON.parse(socket.sent.at(-1)!)).toMatchObject({
      type: 'read_pane',
      pane_id: 'w1:p1',
      content_fingerprint: '',
    });
    socket.message({
      type: 'pane_content',
      request_id: JSON.parse(socket.sent.at(-1)!).request_id,
      pane_id: 'w1:p1',
      target: exactWireScope('w1:p1', relayId).target,
      content: 'resynchronized\n',
      format: 'ansi',
      content_fingerprint: 'content-3',
    });
    expect(JSON.parse(socket.sent.at(-1)!)).toMatchObject({
      type: 'watch_pane',
      pane_id: 'w1:p1',
      content_fingerprint: 'content-3',
    });
  });

  it('merges agents from independent relays without pane id collisions', () => {
    relayStore.addRelay({ label: 'Mac', url: 'ws://127.0.0.1:8872', token: '' });
    const [fedora, mac] = MockWebSocket.instances.slice(-2);
    fedora.open();
    mac.open();
    fedora.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    mac.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    fedora.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Fedora app', ...exactAgentFields() }] });
    mac.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'blocked', project: 'Mac app', ...exactAgentFields() }] });
    const agents = get(relayStore.agents);
    expect(agents).toHaveLength(2);
    expect(new Set(agents.map((agent) => agent.pane_id)).size).toBe(2);
    expect(agents.map((agent) => agent.project).sort()).toEqual(['Fedora app', 'Mac app']);
  });

  it('survives a corrupted snapshot that repeats a pane and reports it', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    // The home view keys agent cards by pane_id: a duplicate from a corrupted
    // relay snapshot previously crashed Svelte's flush and every control in
    // the app silently died. The newest copy wins and the anomaly is visible.
    socket.message({
      type: 'agents',
      agents: [
        { pane_id: 'w1:p1', status: 'working', project: 'First copy', ...exactAgentFields() },
        { pane_id: 'w1:p1', status: 'blocked', project: 'Second copy', ...exactAgentFields() },
      ],
    });
    const agents = get(relayStore.agents);
    expect(agents).toHaveLength(1);
    expect(agents[0].project).toBe('Second copy');
    const banner = document.querySelector('[role=alert]');
    expect(banner?.textContent).toContain('Duplicate agent identity');
    expect(banner?.textContent).toContain('w1:p1');
    banner?.remove();
  });

  it('ignores blocked and agent update frames older than the pane revision', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    socket.message({
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'working', pane_revision: 12, project: 'Current', ...exactAgentFields() }],
    });

    socket.message({
      type: 'blocked', pane_id: 'w1:p1', status: 'blocked',
      pane_revision: 11, event_id: 'stale-blocked',
    });
    socket.message({
      type: 'agent_update', pane_id: 'w1:p1', status: 'idle', pane_revision: 10,
    });

    expect(get(relayStore.agents)[0]).toMatchObject({
      status: 'working',
      pane_revision: 12,
      project: 'Current',
    });
  });

  it('accepts a fresh low pane revision after a relay reconnect', () => {
    const first = MockWebSocket.instances.at(-1)!;
    first.open();
    first.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    first.message({
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'working', pane_revision: 100, ...exactAgentFields() }],
    });

    relayStore.connectAll();
    const replacement = MockWebSocket.instances.at(-1)!;
    replacement.open();
    replacement.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    replacement.message({
      type: 'agents',
      agents: [{ pane_id: 'w1:p1', status: 'blocked', pane_revision: 1, event_id: 'fresh', ...exactAgentFields() }],
    });

    expect(get(relayStore.agents)[0]).toMatchObject({
      status: 'blocked',
      pane_revision: 1,
      event_id: 'fresh',
    });
  });

  it('stores terminal frames without agent-specific relay metadata', () => {
    vi.spyOn(performance, 'now').mockReturnValue(123);
    const socket = MockWebSocket.instances.at(-1)!;
    const relayId = get(relayStore.relayConfigs)[0].id;
    socket.message({
      type: 'pane_content', pane_id: 'w1:p1', content: 'output', format: 'ansi',
      desktop_footer_lines: 6, desktop_prompt_lines: 2,
    });

    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)).toEqual({
      paneId: `${relayId}::w1:p1`,
      content: 'output',
      format: 'ansi',
      receivedAt: 123,
      target: undefined,
      inputTarget: undefined,
      noEcho: undefined,
    });
  });

  it('bounds Conversation pane reads and records only received, explicit echo state', () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    socket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', ...exactAgentFields() }] });
    const agent = get(relayStore.agents)[0];
    relayStore.readPane(agent, false, 30);
    relayStore.readPane(agent, false, 30);
    expect(socket.sent.map(payload => JSON.parse(payload)).filter(message => message.type === 'read_pane')).toHaveLength(1);
    expect(JSON.parse(socket.sent.at(-1)!)).toMatchObject({ type: 'read_pane', lines: 30, content_fingerprint: '', target: expect.objectContaining({ generation: 1 }) });
    const target = exactWireScope('w1:p1', agent.relay_id).target;
    socket.message({ type: 'pane_content', pane_id: 'w1:p1', target, content: 'Ready', format: 'ansi', no_echo: false });
    expect(get(relayStore.terminalFrames).get(agent.pane_id)).toMatchObject({ noEcho: false, receivedAt: performance.now(), target });
    vi.advanceTimersByTime(4_000);
    expect(get(relayStore.terminalFrames).get(agent.pane_id)?.receivedAt).toBe(performance.now() - 4_000);
  });

  it('never attributes untagged or inherited echo state to the current pane target', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, inventory: { state: 'ready' } });
    socket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', ...exactAgentFields() }] });
    const agent = get(relayStore.agents)[0];
    const target = exactWireScope('w1:p1', agent.relay_id).target;
    const content = { type: 'pane_content', pane_id: 'w1:p1', content: 'Ready', content_fingerprint: 'ready', no_echo: false };
    socket.message({ ...content, target });
    expect(get(relayStore.terminalFrames).get(agent.pane_id)?.target).toEqual(target);
    const delta = { type: 'pane_delta', pane_id: 'w1:p1', base_fingerprint: 'ready', content_fingerprint: 'ready', segments: [] };
    socket.message({ ...delta, target, no_echo: false });
    expect(get(relayStore.terminalFrames).get(agent.pane_id)).toMatchObject({ target, noEcho: false });
    socket.message({ ...delta, target });
    expect(get(relayStore.terminalFrames).get(agent.pane_id)?.target).toBeUndefined();
    socket.message({ ...delta, no_echo: false });
    expect(get(relayStore.terminalFrames).get(agent.pane_id)?.target).toBeUndefined();
    socket.message({ ...content, target });
    socket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', ...exactAgentFields(), generation: 2 }] });
    socket.message({ ...content, target, content: 'Late old output' });
    expect(get(relayStore.terminalFrames).get(agent.pane_id)).toMatchObject({ target, content: 'Ready' });
    socket.message(content);
    expect(get(relayStore.terminalFrames).get(agent.pane_id)?.target).toBeUndefined();
    socket.message({ ...content, no_echo: true });
    expect(get(relayStore.terminalFrames).get(agent.pane_id)).toMatchObject({ noEcho: true, target: undefined });
  });

  it('preserves Herdr scrollback truncation on terminal frames', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    const relayId = get(relayStore.relayConfigs)[0].id;
    socket.message({
      type: 'pane_content', pane_id: 'w1:p1', content: 'clipped output', format: 'ansi',
      truncated: true,
    });

    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)).toMatchObject({
      content: 'clipped output',
      truncated: true,
    });
  });

  it('updates Herdr scrollback truncation on terminal deltas', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    const relayId = get(relayStore.relayConfigs)[0].id;
    socket.message({
      type: 'pane_content', pane_id: 'w1:p1', content: 'one\n', format: 'ansi',
      content_fingerprint: 'content-1', truncated: false, viewport_only: true,
      viewport_rows: 46,
    });
    socket.message({
      type: 'pane_delta', pane_id: 'w1:p1', format: 'ansi',
      base_fingerprint: 'content-1', content_fingerprint: 'content-2',
      truncated: true, segments: [{ copy_lines: 1 }, { text: 'two\n' }],
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)).toMatchObject({
      truncated: true,
      viewportOnly: true,
      viewportRows: 46,
    });

    socket.message({
      type: 'pane_delta', pane_id: 'w1:p1', format: 'ansi',
      base_fingerprint: 'content-2', content_fingerprint: 'content-3',
      truncated: false, segments: [{ copy_lines: 1 }, { text: 'three\n' }],
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)).toMatchObject({
      truncated: false,
      viewportOnly: true,
      viewportRows: 46,
    });
  });

  it('carries a recognized no-echo prompt on full frames and deltas', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    const relayId = get(relayStore.relayConfigs)[0].id;
    socket.message({
      type: 'pane_content', pane_id: 'w1:p1', content: '[sudo] password for cv: ', format: 'ansi',
      content_fingerprint: 'secret-1', no_echo: true, no_echo_prompt: '[sudo] password for cv:',
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)).toMatchObject({
      noEcho: true,
      noEchoPrompt: '[sudo] password for cv:',
    });

    socket.message({
      type: 'pane_delta', pane_id: 'w1:p1', format: 'ansi',
      base_fingerprint: 'secret-1', content_fingerprint: 'secret-2',
      no_echo: false, segments: [{ text: 'done\n' }],
    });
    expect(get(relayStore.terminalFrames).get(`${relayId}::w1:p1`)).toMatchObject({
      noEcho: false,
      noEchoPrompt: undefined,
    });
  });

  it('sends a secret only to a relay that advertises secret input', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      ...exactAgentFields(),
    };
    await expect(relayStore.sendSecret(agent, 'hunter2', undefined!)).rejects.toThrow(/does not support password prompts/);

    socket.message({ type: 'push_config', protocol: 3, host: 'fedora', capabilities: ['secret_input'], agent_profiles: [] });
    await expect(relayStore.sendSecret(agent, '', undefined!)).rejects.toThrow(/Enter the password/);

    socket.message({ type: 'agents', agents: [{ ...exactAgentFields(), pane_id: 'w1:p1' }] });
    relayStore.readPane(agent, true);
    const read = JSON.parse(socket.sent.at(-1)!);
    socket.message({ type: 'pane_content', pane_id: 'w1:p1', request_id: read.request_id, target: read.target, content: 'Password:', no_echo: true });
    const frame = get(relayStore.terminalFrames).get(agent.pane_id)!;
    const pending = relayStore.sendSecret(agent, 'hunter2', frame);
    const command = JSON.parse(socket.sent.at(-1)!);
    expect(command).toMatchObject({ type: 'send_secret', pane_id: 'w1:p1', text: 'hunter2' });
    expect(typeof command.request_id).toBe('string');
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'completed' });
    await expect(pending).resolves.toMatchObject({ ok: true });
  });

  it('ignores late events from a socket that has already been replaced', async () => {
    const oldSocket = MockWebSocket.instances.at(-1)!;
    oldSocket.open();
    oldSocket.message({ type: 'push_config', protocol: 3, version: 'old', host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;

    relayStore.connectAll();
    const currentSocket = MockWebSocket.instances.at(-1)!;
    currentSocket.open();
    currentSocket.message({ type: 'push_config', protocol: 3, version: 'new', host: 'fedora', capabilities: [], agent_profiles: [] });
    currentSocket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Current agent', ...exactAgentFields() }] });
    const pending = relayStore.sendCommand(relayId, { type: 'agent_stop', pane_id: 'w1:p1' });
    const command = JSON.parse(currentSocket.sent.at(-1)!);

    oldSocket.message({ type: 'agents', agents: [] });
    oldSocket.serverClose();

    expect(get(relayStore.agents).map((agent) => agent.project)).toEqual(['Current agent']);
    currentSocket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'confirmed' });
    await expect(pending).resolves.toMatchObject({ ok: true });
  });

  it('keeps the last agent snapshot until a reconnected relay sends a fresh one', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, version: 'old', host: 'fedora', capabilities: [], agent_profiles: [] });
    socket.message({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', project: 'Resume safely', ...exactAgentFields() }] });

    socket.serverClose();
    expect(get(relayStore.agents).map((agent) => agent.project)).toEqual(['Resume safely']);

    await vi.advanceTimersByTimeAsync(3_000);
    const replacement = MockWebSocket.instances.at(-1)!;
    replacement.open();
    replacement.message({ type: 'push_config', protocol: 3, version: 'new', host: 'fedora', capabilities: [], agent_profiles: [] });
    expect(get(relayStore.agents).map((agent) => agent.project)).toEqual(['Resume safely']);

    replacement.message({ type: 'agents', agents: [] });
    expect(get(relayStore.agents)).toEqual([]);
  });

  it('settles a pending health probe when a newly authenticated path becomes ready', async () => {
    vi.useFakeTimers();
    let current!: TransportHandlers;
    let connects = 0;
    transportHijack.current = (_relay, handlers) => {
      current = handlers;
      return {
        kind: 'gateway', connect() { connects += 1; handlers.onStatus('connected', { path: 'gateway' }); },
        close() {}, send() { return true; },
      };
    };
    relayStore.connectAll();
    relayStore.revalidateConnections(2_000);
    await vi.advanceTimersByTimeAsync(1_000);
    current.onStatus('connected', { path: 'webrtc' });
    await vi.advanceTimersByTimeAsync(1_001);
    expect(connects).toBe(1);
    expect([...get(relayStore.connections).values()][0].path).toBe('webrtc');
  });

  it('does not persist enrollment from a replaced connection', async () => {
    const callbacks: TransportAuthentication[] = [];
    transportHijack.current = (_relay, _handlers, authentication) => {
      callbacks.push(authentication!);
      return { kind: 'websocket', connect() {}, close() {}, send() { return false; } };
    };
    relayStore.connectAll();
    const relayId = get(relayStore.relayConfigs)[0].id;
    const credentials = new BrowserDeviceCredentialStore(localStorage);
    const invitation = credentials.saveInvitation(relayId, {
      id: 'synthetic-invitation', version: 1, secret: 'A'.repeat(43), expiresAt: Date.now() + 60_000,
    });
    relayStore.connectAll();
    await callbacks[0].onAuthenticated?.(invitation, {
      deviceId: 'synthetic-device', credentialId: 'synthetic-credential', credentialVersion: 1,
      credentialSecret: 'A'.repeat(43), role: 'controller', locale: 'en',
    });
    expect(credentials.get(relayId)).toEqual(invitation);
    expect(get(relayStore.relayConfigs)[0].paired).not.toBe(true);
  });

  it('keeps a responsive connection while inventory and history handlers are slow', async () => {
    vi.useFakeTimers();
    const sends: Record<string, unknown>[] = [];
    let connects = 0;
    transportHijack.current = (_relay, handlers) => ({
      kind: 'websocket',
      connect() { connects += 1; handlers.onStatus('connected'); },
      close() {},
      send(payload) {
        sends.push(payload);
        if (payload.type === 'ping') setTimeout(() => handlers.onMessage({ type: 'pong' }), 10);
        if (payload.type === 'refresh_agents') setTimeout(() => handlers.onMessage({ type: 'inventory_status', state: 'ready' }), 20_000);
        return true;
      },
    });
    relayStore.connectAll();
    relayStore.revalidateConnections(2_000);
    await vi.advanceTimersByTimeAsync(2_001);
    expect(connects).toBe(1);
    expect(get(relayStore.connections).values().next().value?.status).toBe('connected');
    expect(sends.filter((payload) => payload.type === 'ping')).toHaveLength(1);
    expect(sends.filter((payload) => payload.type === 'refresh_agents')).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(20_000);
    expect(connects).toBe(1);
    expect(sends.every((payload) => ['ping', 'refresh_agents'].includes(String(payload.type)))).toBe(true);
  });

  it('probes a responsive long-idle socket instead of treating elapsed time as loss', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    vi.setSystemTime(Date.now() + 300_000);
    relayStore.revalidateConnections(2_000);
    expect(MockWebSocket.instances).toHaveLength(1);
    expect(JSON.parse(socket.sent.at(-1)!).type).toBe('ping');
    socket.message({ type: 'pong' });
    await vi.advanceTimersByTimeAsync(2_000);
    expect(MockWebSocket.instances).toHaveLength(1);
  });

  it('ignores late traffic from a retired connection while locked', async () => {
    vi.useFakeTimers();
    let abandoned!: TransportHandlers;
    transportHijack.current = (_relay, handlers) => {
      abandoned = handlers;
      return { kind: 'websocket', connect() { handlers.onStatus('connected'); }, close() {}, send() { return true; } };
    };
    relayStore.connectAll();
    const relayId = get(relayStore.relayConfigs)[0].id;
    relayStore.setHidden(true);
    securityState.update((state) => ({ ...state, locked: true }));
    await vi.advanceTimersByTimeAsync(130_000);
    const connection = relayStore.connection(relayId)!;
    expect(connection.closed).toBe(true);
    const lastMessageAt = connection.lastMessageAt;
    abandoned.onMessage({ type: 'agents', agents: [{ pane_id: 'w1:p1', status: 'working', ...exactAgentFields() }] });
    abandoned.onStatus('connected');
    expect(get(relayStore.agents)).toEqual([]);
    expect(connection.lastMessageAt).toBe(lastMessageAt);
    expect(connection.status).toBe('disconnected');
    securityState.update((state) => ({ ...state, locked: false }));
    relayStore.setHidden(false);
  });

  it('replaces a half-open socket when its foreground health probe receives no traffic', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [] });

    relayStore.revalidateConnections(25);
    expect(JSON.parse(socket.sent.at(-1)!).type).toBe('ping');
    await vi.advanceTimersByTimeAsync(24);
    expect(MockWebSocket.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(2);
  });

  it('gives a foregrounded page two seconds to answer its app-level ping', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, host: 'fedora', capabilities: [], agent_profiles: [] });

    relayStore.revalidateConnections();
    expect(JSON.parse(socket.sent.at(-1)!).type).toBe('ping');
    await vi.advanceTimersByTimeAsync(1_999);
    expect(MockWebSocket.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(2);
  });

  it('waits longer for the ping while the page is hidden', async () => {
    vi.useFakeTimers();
    const visibility = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    try {
      const socket = MockWebSocket.instances.at(-1)!;
      socket.open();
      socket.message({ type: 'push_config', protocol: 3, host: 'fedora', capabilities: [], agent_profiles: [] });

      relayStore.revalidateConnections();
      await vi.advanceTimersByTimeAsync(9_999);
      expect(MockWebSocket.instances).toHaveLength(1);
      await vi.advanceTimersByTimeAsync(1);
      expect(MockWebSocket.instances).toHaveLength(2);
    } finally {
      if (visibility) Object.defineProperty(document, 'visibilityState', visibility);
      else Reflect.deleteProperty(document, 'visibilityState');
    }
  });

  it('keeps every connected relay warm with a proof-of-life ping', async () => {
    vi.useFakeTimers();
    relayStore.addRelay({ label: 'Debian', url: 'ws://127.0.0.1:8873', token: '' });
    // Adding a relay redials both; keep only the two live sockets so a later
    // count means "nothing was redialed".
    const [connected, dialing] = MockWebSocket.instances.slice(-2);
    MockWebSocket.instances = [connected, dialing];
    expect(dialing.url).toBe('ws://127.0.0.1:8873');
    connected.open();
    connected.message({ type: 'push_config', protocol: 3, host: 'fedora', capabilities: [], agent_profiles: [] });
    const sentTypes = () => connected.sent.map((message) => JSON.parse(message).type);
    const typesOnConnect = sentTypes();

    await vi.advanceTimersByTimeAsync(119_999);
    expect(sentTypes()).toEqual(typesOnConnect);
    await vi.advanceTimersByTimeAsync(1);
    expect(sentTypes()).toEqual([...typesOnConnect, 'ping']);
    // The keepalive proves a live path; it never dials one, so the relay still
    // completing its handshake hears nothing.
    expect(dialing.sent).toEqual([]);

    // The reply doubles as the health signal: an answered ping keeps the socket
    // past the gateway's five-minute idle reaper.
    connected.message({ type: 'inventory_status', state: 'ready' });
    await vi.advanceTimersByTimeAsync(120_000);
    expect(sentTypes()).toEqual([...typesOnConnect, 'ping', 'ping']);
    expect(MockWebSocket.instances).toHaveLength(2);
  });

  it('drops a connection that misses its keepalive while hidden and leaves the dial to resume', async () => {
    vi.useFakeTimers();
    const visibility = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    try {
      const socket = MockWebSocket.instances.at(-1)!;
      socket.open();
      socket.message({ type: 'push_config', protocol: 3, host: 'fedora', capabilities: [], agent_profiles: [] });
      const relayId = get(relayStore.relayConfigs)[0].id;

      Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
      relayStore.setHidden(true);
      await vi.advanceTimersByTimeAsync(120_000);
      expect(JSON.parse(socket.sent.at(-1)!).type).toBe('ping');

      // Ten seconds of silence after the ping: the path is gone.
      await vi.advanceTimersByTimeAsync(10_000);
      expect(socket.readyState).toBe(MockWebSocket.CLOSED);
      expect(get(relayStore.connections).get(relayId)?.status).toBe('disconnected');
      // No socket churn and no radio churn for a page nobody is looking at.
      expect(MockWebSocket.instances).toHaveLength(1);
      await vi.advanceTimersByTimeAsync(600_000);
      expect(MockWebSocket.instances).toHaveLength(1);

      // Resume dials at once: the relay is not connected, so there is nothing
      // to probe.
      Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
      relayStore.setHidden(false);
      relayStore.revalidateConnections(2_000);
      expect(MockWebSocket.instances).toHaveLength(2);
    } finally {
      if (visibility) Object.defineProperty(document, 'visibilityState', visibility);
      else Reflect.deleteProperty(document, 'visibilityState');
    }
  });

  it('stops pinging after an hour hidden and starts again when the app comes back', async () => {
    vi.useFakeTimers();
    const visibility = Object.getOwnPropertyDescriptor(document, 'visibilityState');
    try {
      const socket = MockWebSocket.instances.at(-1)!;
      socket.open();
      socket.message({ type: 'push_config', protocol: 3, host: 'fedora', capabilities: [], agent_profiles: [] });
      const pings = () => socket.sent.filter((payload) => JSON.parse(payload).type === 'ping').length;
      const onConnect = pings();

      Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
      relayStore.setHidden(true);
      // Answer every ping: a healthy hidden page holds its socket for the hour.
      for (let tick = 0; tick < 29; tick += 1) {
        await vi.advanceTimersByTimeAsync(120_000);
        socket.message({ type: 'inventory_status', state: 'ready' });
      }
      expect(pings()).toBe(onConnect + 29);

      // An hour hidden: the radio wakeups stop being worth it and the socket
      // is left to lapse on its own.
      await vi.advanceTimersByTimeAsync(120_000);
      await vi.advanceTimersByTimeAsync(600_000);
      expect(pings()).toBe(onConnect + 29);

      Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
      relayStore.setHidden(false);
      await vi.advanceTimersByTimeAsync(120_000);
      expect(pings()).toBe(onConnect + 30);
    } finally {
      if (visibility) Object.defineProperty(document, 'visibilityState', visibility);
      else Reflect.deleteProperty(document, 'visibilityState');
    }
  });

  it('probes resumed connections and replaces only the one that stops answering', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;

    for (const idle of [0, 240_000, 240_001]) {
      vi.setSystemTime(Date.now() + idle);
      relayStore.revalidateConnections(2_000);
      expect(JSON.parse(socket.sent.at(-1)!).type).toBe('ping');
      expect(MockWebSocket.instances).toHaveLength(1);
      socket.message({ type: 'pong' });
      expect(relayStore.connection(relayId)?.healthTimer).toBeNull();
    }

    relayStore.revalidateConnections(2_000);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(MockWebSocket.instances).toHaveLength(2);
    expect(socket.readyState).toBe(MockWebSocket.CLOSED);
  });

  it('stamps the last-heard time on inbound traffic and clears the pending probe', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    const relayId = get(relayStore.relayConfigs)[0].id;
    socket.open();
    // A completed handshake is itself proof of life.
    expect(relayStore.connection(relayId)?.lastMessageAt).toBe(Date.now());

    await vi.advanceTimersByTimeAsync(1_000);
    relayStore.revalidateConnections(2_000);
    expect(relayStore.connection(relayId)?.healthTimer).not.toBeNull();
    socket.message({ type: 'push_config', protocol: 3, host: 'fedora', capabilities: [], agent_profiles: [] });
    expect(relayStore.connection(relayId)?.lastMessageAt).toBe(Date.now());
    expect(relayStore.connection(relayId)?.healthTimer).toBeNull();
    await vi.advanceTimersByTimeAsync(2_000);
    expect(MockWebSocket.instances).toHaveLength(1);
  });

  it('replaces a half-open socket after the relay announces its update restart', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 3,
      release_version: '0.7.0',
      revision: 'abc123',
      capabilities: ['self_update'],
      agent_profiles: [],
      update: {
        state: 'installing',
        available_version: '0.8.0',
        target_revision: 'f'.repeat(40),
      },
    });
    socket.message({
      type: 'update_status',
      update: {
        state: 'restarting',
        available_version: '0.8.0',
        target_revision: 'f'.repeat(40),
      },
    });

    await vi.advanceTimersByTimeAsync(999);
    expect(MockWebSocket.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(2);
    expect(socket.readyState).toBe(MockWebSocket.CLOSED);
  });

  it('does not restart a WebSocket handshake that is still connecting', () => {
    relayStore.revalidateConnections(25);

    expect(MockWebSocket.instances).toHaveLength(1);
    expect(MockWebSocket.instances[0].readyState).toBe(MockWebSocket.CONNECTING);
    expect(MockWebSocket.instances[0].sent).toEqual([]);
  });

  it('keeps retrying a tokenless relay whose socket fails', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    // A tokenless relay takes the unencrypted path, so a socket error is a
    // relay that is down or restarting — never proof that pairing is missing.
    socket.onerror?.();

    expect(get(relayStore.toast)?.message ?? '').not.toContain('needs pairing');
    await vi.advanceTimersByTimeAsync(1_000);
    expect(MockWebSocket.instances).toHaveLength(2);
    const connection = get(relayStore.connections).values().next().value;
    expect(connection?.pairingRequired).toBe(false);
  });

  it('never dials a relay whose key cannot authenticate and guides pairing', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    // A relay key is configured but is not a usable 32-byte key, and no
    // credential is enrolled: the handshake has nothing to present, so the
    // decision is made before the dial rather than guessed from a close.
    relayStore.addRelay({ label: 'Truncated', url: 'wss://truncated.example', token: 'short-key' });

    expect(MockWebSocket.instances).toHaveLength(0);
    expect(get(relayStore.toast)?.message).toContain('Truncated needs pairing');
    const connection = get(relayStore.connections).get(makeRelayId('Truncated', 'wss://truncated.example'));
    expect(connection?.pairingRequired).toBe(true);
    expect(connection?.status).toBe('disconnected');

    relayStore.revalidateConnections();
    relayStore.connectAll(true);
    expect(MockWebSocket.instances).toHaveLength(0);
  });

  it('backs repeated reconnect attempts off after the first retry', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const first = MockWebSocket.instances.at(-1)!;
    first.open();
    first.serverClose();

    await vi.advanceTimersByTimeAsync(999);
    expect(MockWebSocket.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(2);

    const second = MockWebSocket.instances.at(-1)!;
    second.serverClose();
    await vi.advanceTimersByTimeAsync(1_999);
    expect(MockWebSocket.instances).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(3);
  });

  it.each(['ws://127.0.0.1:8871', 'wss://localhost:8871'])('retains credentials and foreground recovery after an untrusted tokenless close at %s', async url => {
    vi.useFakeTimers();
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    relayStore.addRelay({ label: 'Development', url, token: '' });
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, version: 'abc1234' });
    const credentials = localStorage.getItem('herdr_device_auth_v1');
    socket.serverClose(4401);
    const connection = get(relayStore.connections).values().next().value;
    expect(connection?.authRejected).toBe(false);
    expect(connection?.status).toBe('disconnected');
    expect(localStorage.getItem('herdr_device_auth_v1')).toBe(credentials);
    expect(get(relayStore.toast)?.message || '').not.toContain('refused this device');
    relayStore.revalidateConnections();
    expect(MockWebSocket.instances).toHaveLength(2);
    MockWebSocket.instances[1].open();
    expect(get(relayStore.connections).values().next().value?.status).toBe('connected');
    expect(MockWebSocket.instances[1].sent.map(value => JSON.parse(value)).every(value => !['submit_prompt', 'send_terminal_text', 'send_keys'].includes(value.type))).toBe(true);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(MockWebSocket.instances).toHaveLength(2);
  });

  it.each([
    { code: 4401, url: 'wss://relay.example', opened: true, refused: true },
    { code: 1006, url: 'wss://relay.example', opened: true, refused: false },
    { code: 1000, url: 'wss://relay.example', opened: true, refused: false },
    { code: 4401, url: 'ws://127.0.0.1:8871', opened: true, refused: false },
    { code: 4401, url: 'wss://relay.example', opened: false, refused: false },
  ])('carries close $code at $url (opened=$opened) through the production transport into credential retry policy', async ({ code, url, opened, refused }) => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    MockWebSocket.instances = [];
    const relay = { label: 'Paired TLS', url, token: '' };
    const relayId = makeRelayId(relay.label, relay.url);
    const credentials = new BrowserDeviceCredentialStore(localStorage);
    credentials.saveInvitation(relayId, { id: 'synthetic-invite', version: 1, secret: 'A'.repeat(43), expiresAt: Date.now() + 60_000 });
    credentials.replaceInvitation(relayId, 'synthetic-invite', {
      deviceId: 'synthetic-device', credentialId: 'synthetic-credential', credentialVersion: 1,
      credentialSecret: 'A'.repeat(43), role: 'controller', locale: 'en',
    });
    const before = localStorage.getItem('herdr_device_auth_v1');
    relayStore.addRelay(relay);
    const socket = MockWebSocket.instances[0];
    expect(socket.protocols).toBe('herdr-e2ee-v2');
    if (opened) {
      socket.open();
      await vi.waitFor(() => expect(socket.sent).toHaveLength(1));
      expect(JSON.parse(socket.sent[0]).type).toBe('e2ee_client_hello');
    }
    expect(get(relayStore.connections).get(relayId)?.status).toBe('connecting');
    socket.serverClose(code);
    const connection = get(relayStore.connections).get(relayId);
    expect(connection?.authRejected).toBe(refused);
    expect(connection?.status).toBe('disconnected');
    expect(localStorage.getItem('herdr_device_auth_v1')).toBe(before);
    vi.useFakeTimers();
    if (refused) {
      expect(get(relayStore.toast)?.message).toContain('refused this device. Import a new invitation');
      await vi.advanceTimersByTimeAsync(120_000);
      relayStore.resetReconnectBackoff();
      relayStore.revalidateConnections();
      expect(MockWebSocket.instances).toHaveLength(1);
    } else {
      expect(get(relayStore.toast)?.message || '').not.toContain('refused this device');
      relayStore.revalidateConnections();
      expect(MockWebSocket.instances).toHaveLength(2);
    }
    expect(socket.sent.map(value => JSON.parse(value).type)).toEqual(opened ? ['e2ee_client_hello'] : []);
  });

  it('drops the reconnect backoff so a resumed page retries at once', async () => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const first = MockWebSocket.instances.at(-1)!;
    first.open();
    first.serverClose();
    await vi.advanceTimersByTimeAsync(1_000);
    MockWebSocket.instances.at(-1)!.serverClose();
    expect(MockWebSocket.instances).toHaveLength(2);

    relayStore.resetReconnectBackoff();
    await vi.advanceTimersByTimeAsync(2_000);
    expect(MockWebSocket.instances).toHaveLength(2);

    relayStore.revalidateConnections();
    expect(MockWebSocket.instances).toHaveLength(3);

    // The cleared attempt counter also puts the next failure back on the base delay.
    MockWebSocket.instances.at(-1)!.serverClose();
    await vi.advanceTimersByTimeAsync(999);
    expect(MockWebSocket.instances).toHaveLength(3);
    await vi.advanceTimersByTimeAsync(1);
    expect(MockWebSocket.instances).toHaveLength(4);
  });

  it('retries a fatal failure at the slow cadence instead of stranding', async () => {
    vi.useFakeTimers();
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    let attempts = 0;
    let report: (status: TransportStatus, detail?: TransportStatusDetail) => void = () => {};
    transportHijack.current = (_relay, handlers) => ({
      kind: 'gateway',
      connect: () => {
        attempts += 1;
        report = handlers.onStatus;
        handlers.onStatus('connecting');
      },
      send: () => false,
      close: () => {},
    });
    relayStore.addRelay({ label: 'Gateway', url: 'wss://gateway.example', token: '' });
    const relayId = get(relayStore.relayConfigs)[0].id;
    expect(attempts).toBe(1);

    // A fatal close waits out the full minute: retrying sooner only burns
    // battery, but never retrying strands the phone until a manual reload.
    report('closed', { reason: 'Relay key rejected', fatal: true });
    expect(get(relayStore.connections).get(relayId)?.status).toBe('disconnected');
    await vi.advanceTimersByTimeAsync(59_999);
    expect(attempts).toBe(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(attempts).toBe(2);
  });

  it('stops a multi-gateway relay refused on one gateway from trying the others', async () => {
    vi.useFakeTimers();
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    let attempts = 0;
    let report: (status: TransportStatus, detail?: TransportStatusDetail) => void = () => {};
    transportHijack.current = (_relay, handlers) => ({
      kind: 'gateway',
      connect: () => {
        attempts += 1;
        report = handlers.onStatus;
        handlers.onStatus('connecting');
      },
      send: () => false,
      close: () => {},
    });
    relayStore.addRelay({
      label: 'Gateway',
      url: '',
      token: '0123456789abcdef0123456789abcdef',
      transport: 'hybrid',
      gatewayUrl: 'wss://a.example',
      gatewayUrls: ['wss://a.example', 'wss://b.example'],
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    expect(attempts).toBe(1);

    // The credential, not the path, is what the relay refused: walking the
    // remaining gateways replays the same dead material against the same relay.
    report('closed', { reason: 'Device credential refused', fatal: true, code: 'device_unauthorized' });
    expect(get(relayStore.connections).get(relayId)?.authRejected).toBe(true);
    await vi.advanceTimersByTimeAsync(120_000);
    relayStore.revalidateConnections();
    relayStore.resetReconnectBackoff();
    relayStore.revalidateConnections();
    expect(attempts).toBe(1);
  });

  it('keeps the normal cadence when the gateway does not know the relay yet', async () => {
    vi.useFakeTimers();
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    let attempts = 0;
    let report: (status: TransportStatus, detail?: TransportStatusDetail) => void = () => {};
    transportHijack.current = (_relay, handlers) => ({
      kind: 'gateway',
      connect: () => {
        attempts += 1;
        report = handlers.onStatus;
        handlers.onStatus('connecting');
      },
      send: () => false,
      close: () => {},
    });
    relayStore.addRelay({
      label: 'Gateway', url: '', token: '0123456789abcdef0123456789abcdef',
      transport: 'hybrid', gatewayUrl: 'wss://gw.example',
    });
    expect(attempts).toBe(1);

    // `unknown_relay` is what a gateway answers while a relay restarts and its
    // registration lapses; the phone must be back the moment it re-registers.
    report('closed', {
      reason: 'That computer is not connected to the gateway.',
      fatal: true,
      code: 'unknown_relay',
    });
    await vi.advanceTimersByTimeAsync(1_000);
    expect(attempts).toBe(2);
  });

  it('rereads watched panes as soon as the relay reconnects', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: ['pane_realtime_delta'], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId,
      relay_label: 'Fedora',
      raw_pane_id: 'w1:p1',
      ...exactAgentFields(),
      pane_id: `${relayId}::w1:p1`,
    };
    relayStore.watchPane(agent as never);
    socket.message({
      type: 'pane_content',
      pane_id: 'w1:p1',
      content: 'before the drop\n',
      format: 'ansi',
      content_fingerprint: 'content-1',
    });
    expect(JSON.parse(socket.sent.at(-1)!)).toMatchObject({ type: 'watch_pane', pane_id: 'w1:p1' });

    // The connection dies while the terminal stays open; reads and watches
    // sent in the gap are lost. The reconnect must revive the stream itself,
    // not leave it to the fifteen-second resync interval.
    socket.serverClose();
    await vi.advanceTimersByTimeAsync(1_000);
    const replacement = MockWebSocket.instances.at(-1)!;
    expect(replacement).not.toBe(socket);
    replacement.open();
    const sent = replacement.sent.map((payload) => JSON.parse(payload) as Record<string, unknown>);
    expect(sent.some((message) => message.type === 'read_pane' && message.pane_id === 'w1:p1')).toBe(true);
  });

  it('honors terminal refresh while traffic is relayed and caps its history', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    setTerminalRefreshInterval(100);
    let report: (status: TransportStatus, detail?: TransportStatusDetail) => void = () => {};
    let deliver: (message: Record<string, any>) => void = () => {};
    const sent: Record<string, unknown>[] = [];
    transportHijack.current = (_relay, handlers) => ({
      kind: 'gateway',
      connect: () => {
        report = handlers.onStatus;
        deliver = handlers.onMessage;
        handlers.onStatus('connecting');
      },
      send: (payload) => {
        sent.push(payload);
        return true;
      },
      close: () => {},
    });
    relayStore.addRelay({ label: 'Gateway', url: '', token: '', transport: 'hybrid', gatewayUrl: 'wss://gw.example' });
    const relayId = get(relayStore.relayConfigs)[0].id;

    report('connected', { path: 'gateway' });
    deliver({ type: 'push_config', protocol: 3, capabilities: ['pane_realtime_delta'], agent_profiles: [] });
    expect(get(relayStore.connections).get(relayId)?.path).toBe('gateway');

    const agent = {
      relay_id: relayId,
      relay_label: 'Gateway',
      raw_pane_id: 'w1:p1',
      ...exactAgentFields(),
      pane_id: `${relayId}::w1:p1`,
    };
    relayStore.watchPane(agent as never);
    deliver({
      type: 'pane_content',
      pane_id: 'w1:p1',
      content: 'one\n',
      format: 'ansi',
      content_fingerprint: 'content-1',
    });
    // Acknowledged deltas make the selected cadence affordable on the metered
    // path; only scrollback remains capped.
    expect(sent.at(-1)).toMatchObject({ type: 'watch_pane', interval_ms: 100, lines: 1_000 });

    // Promotion to the direct path keeps the same user-selected cadence.
    report('connected', { path: 'webrtc' });
    expect(get(relayStore.connections).get(relayId)?.path).toBe('webrtc');
    expect(sent.at(-1)).toMatchObject({ type: 'watch_pane', interval_ms: 100 });
  });

  it('records the gateway that answered and drops it on the relay URL path', () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    let report: (status: TransportStatus, detail?: TransportStatusDetail) => void = () => {};
    transportHijack.current = (_relay, handlers) => ({
      kind: 'gateway',
      connect: () => {
        report = handlers.onStatus;
        handlers.onStatus('connecting');
      },
      send: () => true,
      close: () => {},
    });
    relayStore.addRelay({
      label: 'Gateway',
      url: 'wss://fedora.example',
      token: '',
      transport: 'hybrid',
      gatewayUrl: 'wss://a.example',
      gatewayUrls: ['wss://a.example', 'wss://b.example'],
    });
    const relayId = get(relayStore.relayConfigs)[0].id;

    // The head of the list was skipped, so the answer is the dialed entry.
    report('connected', { path: 'gateway', gatewayUrl: 'wss://b.example' });
    expect(get(relayStore.connections).get(relayId)?.activeGatewayUrl).toBe('wss://b.example');

    report('connected', { path: 'webrtc', gatewayUrl: 'wss://b.example' });
    expect(get(relayStore.connections).get(relayId)?.activeGatewayUrl).toBe('wss://b.example');

    // The legacy relay URL carries no gateway, so nothing may still name one.
    report('connected', { path: 'websocket' });
    expect(get(relayStore.connections).get(relayId)?.activeGatewayUrl).toBe('');
  });

  it('adopts an advertised hybrid descriptor without a QR re-scan', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    const relayId = get(relayStore.relayConfigs)[0].id;
    expect(get(relayStore.relayConfigs)[0].transport).toBeUndefined();

    socket.message({
      type: 'push_config',
      protocol: 3,
      capabilities: [],
      agent_profiles: [],
      release_version: '0.17.0',
      update: { state: 'current', upstream_version: '0.17.1' },
      hybrid: {
        transport: 'herdr-hybrid-v2',
        gateway_url: 'wss://gw.example',
        gateway_urls: [
          'wss://gw.example',
          'wss://backup.example',
          'https://not-a-websocket.example',
          'wss://backup.example',
        ],
        gateway_version: '0.17.0',
        gateway_revision: 'gateway-revision',
        gateway_available_version: '0.17.1',
        relay_id: 'Ccy3nT9AULlAceTEnhTvoQ',
        direct: true,
      },
    });

    const stored = get(relayStore.relayConfigs).find((entry) => entry.id === relayId)!;
    expect(stored.transport).toBe('hybrid');
    expect(stored.gatewayUrl).toBe('wss://gw.example');
    expect(stored.gatewayUrls).toEqual(['wss://gw.example', 'wss://backup.example']);
    expect(get(relayStore.connections).get(relayId)).toMatchObject({
      gatewayVersion: '0.17.0',
      gatewayAvailableVersion: '0.17.1',
    });
    // The legacy URL survives so the hybrid path can fall back to it.
    expect(stored.url).toBe('ws://127.0.0.1:8871');
    expect(JSON.parse(localStorage.getItem('herdr_relays')!)).toContainEqual(
      expect.objectContaining({
        transport: 'hybrid',
        gatewayUrl: 'wss://gw.example',
        gatewayUrls: ['wss://gw.example', 'wss://backup.example'],
      }),
    );

    socket.message({
      type: 'push_config',
      protocol: 3,
      capabilities: [],
      agent_profiles: [],
      hybrid: {
        transport: 'herdr-hybrid-v2',
        gateway_url: 'wss://backup.example',
        gateway_urls: ['wss://backup.example', 'wss://gw.example'],
        relay_id: 'Ccy3nT9AULlAceTEnhTvoQ',
        direct: true,
      },
    });
    const reordered = get(relayStore.relayConfigs).find((entry) => entry.id === relayId)!;
    expect(reordered.gatewayUrl).toBe('wss://backup.example');
    expect(reordered.gatewayUrls).toEqual(['wss://backup.example', 'wss://gw.example']);
    expect(MockWebSocket.instances).toHaveLength(1);
  });

  it('ignores a hybrid descriptor that does not name a WebSocket gateway', () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config',
      protocol: 3,
      capabilities: [],
      agent_profiles: [],
      hybrid: { transport: 'herdr-hybrid-v2', gateway_url: 'https://gw.example' },
    });
    expect(get(relayStore.relayConfigs)[0].transport).toBeUndefined();
  });

  it('rejects an attachment batch when its relay disconnects before begin confirmation', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const upload = relayStore.uploadAttachments({
      relay_id: relayId,
      relay_label: 'Fedora',
      raw_pane_id: 'w1:p1',
      pane_id: `${relayId}::w1:p1`,
      server_session_id: 'session-1',
      terminal_id: 'terminal-1',
      generation: 1,
    }, [new File(['png'], 'shot.png', { type: 'image/png' })]);
    await vi.waitFor(() => expect(socket.sent.some((payload) => JSON.parse(payload).type === 'upload_begin')).toBe(true));

    socket.serverClose();
    await expect(upload).rejects.toMatchObject({ code: 'attachment_upload_failed' });
  });

  it('marks an attachment batch failed when begin confirmation times out', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const upload = relayStore.uploadAttachments({
      relay_id: relayId,
      relay_label: 'Fedora',
      raw_pane_id: 'w1:p1',
      pane_id: `${relayId}::w1:p1`,
      server_session_id: 'session-1',
      terminal_id: 'terminal-1',
      generation: 1,
    }, [new File(['png'], 'shot.png', { type: 'image/png' })]);

    const rejection = expect(upload).rejects.toMatchObject({ code: 'attachment_upload_failed' });
    await vi.advanceTimersByTimeAsync(60_000);
    await rejection;
  });

  it('streams an attachment only through its target relay and returns an opaque reference', async () => {
    relayStore.addRelay({ label: 'Mac', url: 'ws://127.0.0.1:8872', token: '' });
    const [fedoraSocket, macSocket] = MockWebSocket.instances.slice(-2);
    fedoraSocket.open();
    macSocket.open();
    fedoraSocket.message({ type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [] });
    macSocket.message({ type: 'push_config', protocol: 3, version: 'abc123', host: 'mac', capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs).find((relay) => relay.label === 'Fedora')!.id;
    const upload = relayStore.uploadAttachments({
      relay_id: relayId,
      relay_label: 'Fedora',
      raw_pane_id: 'w1:p1',
      pane_id: `${relayId}::w1:p1`,
      server_session_id: 'session-1',
      terminal_id: 'terminal-1',
      generation: 1,
    }, [new File(['png'], 'shot.png', { type: 'image/png' })]);
    await vi.waitFor(() => expect(fedoraSocket.sent.some((payload) => JSON.parse(payload).type === 'upload_begin')).toBe(true));
    const begin = fedoraSocket.sent.map((payload) => JSON.parse(payload)).find((message) => message.type === 'upload_begin');
    macSocket.message({
      type: 'upload_begin_result',
      request_id: begin.request_id,
      result: {
        upload_id: 'wrong',
        chunk_bytes: 262144,
        expires_at: new Date(Date.now() + 60_000).toISOString(),
        limits: { max_files: 8, max_file_bytes: 20971520, max_batch_bytes: 52428800 },
      },
    });
    await Promise.resolve();
    expect(fedoraSocket.sent.some((payload) => JSON.parse(payload).type === 'upload_chunk')).toBe(false);

    fedoraSocket.message({
      type: 'upload_begin_result',
      request_id: begin.request_id,
      result: {
        upload_id: 'upload-1',
        chunk_bytes: 262144,
        expires_at: new Date(Date.now() + 60_000).toISOString(),
        limits: { max_files: 8, max_file_bytes: 20971520, max_batch_bytes: 52428800 },
      },
    });
    await vi.waitFor(() => expect(fedoraSocket.sent.some((payload) => JSON.parse(payload).type === 'upload_chunk')).toBe(true));
    const chunk = fedoraSocket.sent.map((payload) => JSON.parse(payload)).find((message) => message.type === 'upload_chunk');
    expect(chunk).toMatchObject({
      upload_id: 'upload-1',
      data: 'cG5n',
      sha256: '8f8cbb7dcf46e0bc7d53265749a6c17d116093a6ba95e442764060c76fd4a86c',
      target: {
        server_session_id: 'session-1',
        pane_id: 'w1:p1',
        terminal_id: 'terminal-1',
        generation: 1,
      },
      protocol: 3,
    });
    fedoraSocket.message({
      type: 'upload_chunk_result',
      request_id: chunk.request_id,
      result: { file_index: 0, next_sequence: 1, received_bytes: 3 },
    });
    await vi.waitFor(() => expect(fedoraSocket.sent.some((payload) => JSON.parse(payload).type === 'upload_finish')).toBe(true));
    const finish = fedoraSocket.sent.map((payload) => JSON.parse(payload)).find((message) => message.type === 'upload_finish');
    fedoraSocket.message({
      type: 'upload_finish_result',
      request_id: finish.request_id,
      result: {
        attachments: [{
          ref: 'attachment:opaque-1',
          name: 'shot.png',
          media_type: 'image/png',
          bytes: 3,
          sha256: '8f8cbb7dcf46e0bc7d53265749a6c17d116093a6ba95e442764060c76fd4a86c',
          expires_at: new Date(Date.now() + 60_000).toISOString(),
        }],
      },
    });
    await expect(upload).resolves.toMatchObject([{ ref: 'attachment:opaque-1', name: 'shot.png' }]);
  });

  it('does not apply a directory result to a replacement connection', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: ['directory_browser'], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const listing = relayStore.listDirectories(relayId, '/home/test');
    const request = JSON.parse(socket.sent.at(-1)!);
    socket.message({
      type: 'command_result', request_id: request.request_id, ok: true, phase: 'confirmed',
      data: { current: { path: '/home/test', label: '~' }, parent: '/home', directories: [] },
    });
    relayStore.connectAll();

    await expect(listing).rejects.toThrow('Relay reconnected while loading directories');
    expect(get(relayStore.connections).get(relayId)?.directoryBrowser).toBeNull();
  });

  it('rejects directory results without a usable current path', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora',
      capabilities: ['directory_browser'], agent_profiles: [],
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const listing = relayStore.listDirectories(relayId);
    const request = JSON.parse(socket.sent.at(-1)!);
    socket.message({
      type: 'command_result', request_id: request.request_id, ok: true, phase: 'confirmed',
      data: { current: { path: '', label: '' }, parent: '', directories: [] },
    });

    await expect(listing).rejects.toThrow('Relay returned an invalid directory listing');
    const connection = get(relayStore.connections).get(relayId);
    expect(connection?.directoryBrowser).toBeNull();
    expect(connection?.directoryError).toBe('Relay returned an invalid directory listing');
    expect(connection?.directoryLoading).toBe(false);
  });

  it('keeps the newest directory listing when responses arrive out of order', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: ['directory_browser'], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;

    const older = relayStore.listDirectories(relayId, '/home/test/older');
    const olderRequest = JSON.parse(socket.sent.at(-1)!);
    const newer = relayStore.listDirectories(relayId, '/home/test/newer');
    const newerRequest = JSON.parse(socket.sent.at(-1)!);

    socket.message({
      type: 'command_result', request_id: newerRequest.request_id, ok: true, phase: 'confirmed',
      data: { current: { path: '/home/test/newer', label: 'newer' }, parent: '/home/test', directories: null },
    });
    socket.message({
      type: 'command_result', request_id: olderRequest.request_id, ok: true, phase: 'confirmed',
      data: { current: { path: '/home/test/older', label: 'older' }, parent: '/home/test', directories: [] },
    });

    await expect(newer).resolves.toMatchObject({ current: { path: '/home/test/newer' }, directories: [] });
    await expect(older).resolves.toMatchObject({ current: { path: '/home/test/older' } });
    expect(get(relayStore.connections).get(relayId)?.directoryBrowser?.current.path).toBe('/home/test/newer');
    expect(get(relayStore.connections).get(relayId)?.directoryLoading).toBe(false);
  });

  it('keeps waiting for the final result once the relay accepts a command', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [],
      inventory: { state: 'ready', last_attempt_at: 100, last_success_at: 100 },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;

    const pending = relayStore.sendCommand(relayId, { type: 'respond', pane_id: 'w1:p1', index: 0, total: 2 }, 12_000);
    const command = JSON.parse(socket.sent.at(-1)!);
    await vi.advanceTimersByTimeAsync(9_000);
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'accepted' });

    // Past the original 12 second send timeout the request must still be open.
    await vi.advanceTimersByTimeAsync(5_000);
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'confirmed' });
    await expect(pending).resolves.toMatchObject({ ok: true, phase: 'confirmed' });
  });

  it('retains native OpenCode command paths without accepting malformed commands', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: ['slash_commands'], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      agent: 'opencode', cwd: '/fixture', ...exactAgentFields(),
    };
    const pending = relayStore.loadSlashCommands(agent);
    const request = JSON.parse(socket.sent.at(-1)!);
    socket.message({
      type: 'command_result', request_id: request.request_id, ok: true,
      data: { commands: ['/git/review', '/valid', '/bad//path', '/bad/', '/bad/ space', '/bad\nline'].map(command => ({ command, source: 'project' })) },
    });
    await expect(pending).resolves.toMatchObject({ commands: [{ command: '/git/review' }, { command: '/valid' }] });
  });

  it('loads and caches slash commands for one agent identity', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora',
      capabilities: ['slash_commands'], agent_profiles: [],
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      agent: 'codex', cwd: '/home/test/project',
      ...exactAgentFields(),
    };

    const first = relayStore.loadSlashCommands(agent);
    const duplicate = relayStore.loadSlashCommands(agent);
    const requests = socket.sent.map((payload) => JSON.parse(payload))
      .filter((message) => message.type === 'list_slash_commands');
    expect(requests).toHaveLength(1);
    expect(requests[0]).toMatchObject({ pane_id: 'w1:p1', protocol: 3 });
    socket.message({
      type: 'command_result', request_id: requests[0].request_id, ok: true, phase: 'completed',
      data: {
        commands: [
          { command: '/zeta', description: 'Last command', source: 'builtin' },
          { command: '/Alpha', description: 'First command', source: 'builtin' },
          { command: '/model', description: 'Choose model', source: 'builtin' },
        ],
        truncated: false,
      },
    });

    const alphabeticalCommands = [{ command: '/Alpha' }, { command: '/model' }, { command: '/zeta' }];
    await expect(first).resolves.toMatchObject({ commands: alphabeticalCommands });
    await expect(duplicate).resolves.toMatchObject({ commands: alphabeticalCommands });
    await expect(relayStore.loadSlashCommands(agent)).resolves.toMatchObject({ commands: alphabeticalCommands });
    expect(socket.sent.map((payload) => JSON.parse(payload))
      .filter((message) => message.type === 'list_slash_commands')).toHaveLength(1);

    const changed = relayStore.loadSlashCommands({ ...agent, cwd: '/home/test/other' });
    const changedRequest = socket.sent.map((payload) => JSON.parse(payload)).at(-1)!;
    expect(changedRequest.type).toBe('list_slash_commands');
    socket.message({
      type: 'command_result', request_id: changedRequest.request_id, ok: true, phase: 'completed',
      data: { commands: [], truncated: false },
    });
    await expect(changed).resolves.toEqual({ commands: [], truncated: false });
  });
  it.each([
    { server_session_id: 'replacement-server' },
    { terminal_id: 'replacement-terminal' },
    { generation: 99 },
    { agent_session_id: 'replacement-agent-session' },
  ])('invalidates slash commands when target identity changes: %j', async (replacement) => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: ['slash_commands'], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      agent: 'pi', cwd: '/home/test/project', ...exactAgentFields(),
    };
    const older = relayStore.loadSlashCommands(agent);
    const olderRequest = JSON.parse(socket.sent.at(-1)!);
    const currentAgent = { ...agent, ...replacement };
    const current = relayStore.loadSlashCommands(currentAgent);
    const currentRequest = JSON.parse(socket.sent.at(-1)!);
    expect(currentRequest.request_id).not.toBe(olderRequest.request_id);
    socket.message({
      type: 'command_result', request_id: currentRequest.request_id, ok: true,
      data: { commands: [{ command: '/current', source: 'builtin' }], truncated: false },
    });
    await current;
    socket.message({
      type: 'command_result', request_id: olderRequest.request_id, ok: true,
      data: { commands: [{ command: '/old', source: 'builtin' }], truncated: false },
    });
    await older;
    await expect(relayStore.loadSlashCommands(currentAgent)).resolves.toMatchObject({
      commands: [{ command: '/current' }],
    });
    expect(socket.sent.map((payload) => JSON.parse(payload))
      .filter((message) => message.type === 'list_slash_commands')).toHaveLength(2);
  });

  it('refreshes slash commands and prevents superseded requests from repopulating the cache', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: ['slash_commands'], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      agent: 'pi', cwd: '/home/test/project', ...exactAgentFields(),
    };
    const initial = relayStore.loadSlashCommands(agent);
    const initialRequest = JSON.parse(socket.sent.at(-1)!);
    socket.message({
      type: 'command_result', request_id: initialRequest.request_id, ok: true,
      data: { commands: [{ command: '/initial', source: 'builtin' }], truncated: false },
    });
    await initial;
    const older = relayStore.loadSlashCommands(agent, true);
    const olderRequest = JSON.parse(socket.sent.at(-1)!);
    const current = relayStore.loadSlashCommands(agent, true);
    const currentRequest = JSON.parse(socket.sent.at(-1)!);
    const duplicate = relayStore.loadSlashCommands(agent);
    socket.message({
      type: 'command_result', request_id: currentRequest.request_id, ok: true,
      data: { commands: [{ command: '/reloaded', source: 'builtin' }], truncated: false },
    });
    await expect(current).resolves.toMatchObject({ commands: [{ command: '/reloaded' }] });
    await expect(duplicate).resolves.toMatchObject({ commands: [{ command: '/reloaded' }] });
    socket.message({
      type: 'command_result', request_id: olderRequest.request_id, ok: true,
      data: { commands: [{ command: '/stale', source: 'builtin' }], truncated: false },
    });
    await older;
    await expect(relayStore.loadSlashCommands(agent)).resolves.toMatchObject({
      commands: [{ command: '/reloaded' }],
    });
    expect(socket.sent.map((payload) => JSON.parse(payload))
      .filter((message) => message.type === 'list_slash_commands')).toHaveLength(3);
  });

  it('retains the frontend cap warning and late entries after normalization', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora',
      capabilities: ['slash_commands'], agent_profiles: [],
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      agent: 'codex', cwd: '/home/test/project',
      ...exactAgentFields(),
    };
    const rawCommands = Array.from({ length: SLASH_COMMAND_MAX_ENTRIES + 1 }, (_, index) => ({
      command: `/bulk-${String(index).padStart(4, '0')}`,
      description: `Bulk command ${index}`,
      source: 'project',
    }));
    rawCommands[350] = { command: '/late-command', description: 'Late command', source: 'project' };

    const pending = relayStore.loadSlashCommands(agent);
    const request = JSON.parse(socket.sent.at(-1)!);
    socket.message({
      type: 'command_result', request_id: request.request_id, ok: true, phase: 'completed',
      data: { commands: rawCommands, truncated: false },
    });

    const catalog = await pending;
    expect(catalog.commands).toHaveLength(SLASH_COMMAND_MAX_ENTRIES);
    expect(catalog.truncated).toBe(true);
    expect(catalog.commands.some((entry) => entry.command === '/late-command')).toBe(true);
    expect(catalog.commands[0].command).toBe('/bulk-0000');
    const cached = await relayStore.loadSlashCommands(agent);
    expect(cached.truncated).toBe(true);
    expect(cached.commands.some((entry) => entry.command === '/late-command')).toBe(true);
    expect(socket.sent.map((payload) => JSON.parse(payload))
      .filter((message) => message.type === 'list_slash_commands')).toHaveLength(1);
  });

  it('preserves runtime catalog status, revision, kind, and canonical provenance', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: ['slash_commands'], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      agent: 'pi', cwd: '/fixture/project', ...exactAgentFields(),
    };
    const pending = relayStore.loadSlashCommands(agent);
    const request = JSON.parse(socket.sent.at(-1)!);
    socket.message({ type: 'command_result', request_id: request.request_id, ok: true, data: piCatalogFixture });
    const catalog = await pending;
    expect(catalog.status).toBe('available');
    expect(catalog.revision).toBe(piCatalogFixture.revision);
    expect(catalog.commands.find(command => command.command === '/review:2')).toMatchObject({
      source: 'temporary', kind: 'extension', provenance: { source: 'cli', scope: 'temporary', origin: 'top-level' },
    });
    expect(catalog.commands.find(command => command.command === '/summary')?.kind).toBe('prompt');
    expect(catalog.truncated).toBe(false);
  });

  it('normalizes slash command results without data', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora',
      capabilities: ['slash_commands'], agent_profiles: [],
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      agent: 'codex', cwd: '/home/test/project',
      ...exactAgentFields(),
    };

    const pending = relayStore.loadSlashCommands(agent);
    const request = JSON.parse(socket.sent.at(-1)!);
    socket.message({ type: 'command_result', request_id: request.request_id, ok: true, phase: 'completed' });

    await expect(pending).resolves.toEqual({ commands: [], truncated: false });
  });

  it('invalidates slash-command caches on reconnect and rejects unsupported relays', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: [], agent_profiles: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agent = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      agent: 'claude', cwd: '/home/test/project',
      ...exactAgentFields(),
    };
    await expect(relayStore.loadSlashCommands(agent)).rejects.toThrow(/does not provide/);

    socket.message({ type: 'push_config', protocol: 3, capabilities: ['slash_commands'], agent_profiles: [] });
    const pending = relayStore.loadSlashCommands(agent);
    const request = socket.sent.map((payload) => JSON.parse(payload)).at(-1)!;
    socket.message({
      type: 'command_result', request_id: request.request_id, ok: true,
      data: { commands: [{ command: '/help', description: 'Help', source: 'builtin' }], truncated: false },
    });
    await pending;

    relayStore.connectAll();
    const replacement = MockWebSocket.instances.at(-1)!;
    replacement.open();
    replacement.message({ type: 'push_config', protocol: 3, capabilities: ['slash_commands'], agent_profiles: [] });
    const refreshed = relayStore.loadSlashCommands(agent);
    const refreshedRequest = JSON.parse(replacement.sent.at(-1)!);
    expect(refreshedRequest.type).toBe('list_slash_commands');
    replacement.message({
      type: 'command_result', request_id: refreshedRequest.request_id, ok: true,
      data: { commands: [], truncated: false },
    });
    await refreshed;
  });

  it('does not let an older slash-command response overwrite a newer identity cache', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora',
      capabilities: ['slash_commands'], agent_profiles: [],
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const agentOld = {
      relay_id: relayId, relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: `${relayId}::w1:p1`,
      agent: 'codex', cwd: '/home/test/old',
      ...exactAgentFields(),
    };
    const agentNew = { ...agentOld, cwd: '/home/test/new' };

    const olderPromise = relayStore.loadSlashCommands(agentOld);
    const olderRequest = socket.sent.map((p) => JSON.parse(p)).at(-1)!;
    const newerPromise = relayStore.loadSlashCommands(agentNew);
    const newerRequest = socket.sent.map((p) => JSON.parse(p)).at(-1)!;
    expect(olderRequest.request_id).not.toBe(newerRequest.request_id);

    socket.message({
      type: 'command_result', request_id: newerRequest.request_id, ok: true, phase: 'completed',
      data: { commands: [{ command: '/new', description: 'New', source: 'builtin' }], truncated: false },
    });
    await newerPromise;

    socket.message({
      type: 'command_result', request_id: olderRequest.request_id, ok: true, phase: 'completed',
      data: { commands: [{ command: '/old', description: 'Old', source: 'builtin' }], truncated: false },
    });
    await olderPromise;

    const cached = relayStore.loadSlashCommands(agentNew);
    await expect(cached).resolves.toMatchObject({ commands: [{ command: '/new' }] });
    const allRequests = socket.sent.map((p) => JSON.parse(p))
      .filter((m) => m.type === 'list_slash_commands');
    expect(allRequests).toHaveLength(2);
  });

  it('keeps a newer responding window when an older timer is cleared', async () => {
    vi.useFakeTimers();
    relayStore.markResponding('fedora::w1:p1');
    await vi.advanceTimersByTimeAsync(1_000);
    relayStore.clearResponding('fedora::w1:p1');
    relayStore.markResponding('fedora::w1:p1');

    await vi.advanceTimersByTimeAsync(9_000);
    expect(get(relayStore.responding).has('fedora::w1:p1')).toBe(true);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(get(relayStore.responding).has('fedora::w1:p1')).toBe(false);
  });

  it('tags a command lost to a disconnect after its frame was written as dispatched_unknown', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [],
      inventory: { state: 'ready' },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const pending = relayStore.sendCommand(relayId, { type: 'submit_prompt', pane_id: 'w1:p1', text: 'ship it' });
    expect(JSON.parse(socket.sent.at(-1)!).type).toBe('submit_prompt');

    socket.serverClose();
    const error = await pending.then(() => null, (caught) => caught as CommandError);
    expect(error?.message).toBe('Relay disconnected');
    expect(error?.data).toMatchObject({ dispatched_unknown: true });
  });

  it('tags an accepted command whose confirmation never arrives as dispatched_unknown', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [],
      inventory: { state: 'ready' },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const pending = relayStore.sendCommand(relayId, { type: 'submit_prompt', pane_id: 'w1:p1', text: 'ship it' });
    const command = JSON.parse(socket.sent.at(-1)!);
    socket.message({ type: 'command_result', request_id: command.request_id, ok: true, phase: 'accepted' });
    const outcome = pending.then(() => null, (caught) => caught as CommandError);

    await vi.advanceTimersByTimeAsync(10_000);
    const error = await outcome;
    expect(error?.message).toBe('Relay confirmation timed out');
    expect(error?.data).toMatchObject({ dispatched_unknown: true });
  });

  it('tags a written command that never hears back at all as dispatched_unknown', async () => {
    vi.useFakeTimers();
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [],
      inventory: { state: 'ready' },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const outcome = relayStore.sendCommand(relayId, { type: 'agent_rename', pane_id: 'w1:p1', name: 'renamed' })
      .then(() => null, (caught) => caught as CommandError);

    await vi.advanceTimersByTimeAsync(15_000);
    const error = await outcome;
    expect(error?.message).toBe('Relay confirmation timed out');
    expect(error?.data).toMatchObject({ dispatched_unknown: true });
  });

  it('keeps definitive pre-send failures plain and names the missing capability', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora', capabilities: [], agent_profiles: [],
      inventory: { state: 'ready' },
    });
    const relayId = get(relayStore.relayConfigs)[0].id;
    const workspace = {
      relay_id: relayId, relay_label: 'Fedora', workspace_id: 'w1', number: 1, label: 'Project',
      focused: false, pane_count: 1, tab_count: 1, active_tab_id: '', agent_status: '',
      cwd: '/home/user/project',
    } as RelayWorkspace;

    const workspaceError = await relayStore.createWorkspace(relayId, '/home/user/project', 'Project')
      .then(() => null, (caught) => caught as CommandError);
    expect(workspaceError?.message).toBe('This relay does not support workspace management');
    expect(workspaceError?.data?.dispatched_unknown).toBeUndefined();

    const worktreeError = await relayStore.listWorktrees(workspace)
      .then(() => null, (caught) => caught as CommandError);
    expect(worktreeError?.message).toBe('This relay does not support worktree management');
    expect(worktreeError?.data?.dispatched_unknown).toBeUndefined();

    relayStore.addRelay({ label: 'Mac', url: 'wss://mac.example', token: '' });
    const macId = get(relayStore.relayConfigs).find((relay) => relay.label === 'Mac')!.id;
    const offlineError = await relayStore.sendCommand(macId, { type: 'refresh_agents' })
      .then(() => null, (caught) => caught as CommandError);
    expect(offlineError?.message).toBe('Relay is not connected');
    expect(offlineError?.data?.dispatched_unknown).toBeUndefined();
  });

  it('adopts relay speech voice cache state and gates management on the capability', async () => {
    const socket = MockWebSocket.instances.at(-1)!;
    socket.open();
    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora',
      capabilities: ['speech_voice_management'], agent_profiles: [],
      inventory: { state: 'ready' }, speech_languages: ['en'],
    });
    const relayId = get(relayStore.relayConfigs)[0].id;

    socket.message({
      type: 'speech_voices',
      cache_dir: '/home/user/.cache/herdr-mobile-relay/speech',
      engine_installed: true,
      languages: ['en', 'fr', 'ja', 7],
      voices: [
        { language: 'en', name: 'en_US-lessac-medium', installed: true, bytes: 63206179, engine: 'piper' },
        { language: 'fr', name: 'fr_FR-siwis-medium', installed: false, bytes: '63206169', engine: 'espeak-ng' },
        { language: 'ja', name: 'ja_JP-unknown-medium', installed: true, bytes: 41000000, engine: 'piper' },
        'not a voice',
      ],
    });
    const connection = get(relayStore.connections).get(relayId)!;
    expect(connection.speechCacheDir).toBe('/home/user/.cache/herdr-mobile-relay/speech');
    expect(connection.speechEngineInstalled).toBe(true);
    // A language the app cannot offer is dropped from both views of the cache.
    expect(connection.speechLanguages).toEqual(['en', 'fr']);
    expect(connection.speechVoices).toEqual([
      { language: 'en', name: 'en_US-lessac-medium', installed: true, bytes: 63206179, engine: 'piper' },
      { language: 'fr', name: 'fr_FR-siwis-medium', installed: false, bytes: 63206169, engine: 'espeak-ng' },
    ]);

    const install = relayStore.installSpeechVoice(relayId, 'fr');
    const command = JSON.parse(socket.sent.at(-1)!);
    expect(command).toMatchObject({ type: 'speech_voice_install', language: 'fr', protocol: 3 });
    socket.message({
      type: 'command_result', action: 'speech_voice_install', request_id: command.request_id,
      ok: true, phase: 'completed',
      data: {
        cache_dir: '/home/user/.cache/herdr-mobile-relay/speech',
        engine_installed: true,
        languages: ['en', 'fr'],
        voices: [{ language: 'fr', name: 'fr_FR-siwis-medium', installed: true, bytes: 63206169, engine: 'piper' }],
      },
    });
    await expect(install).resolves.toEqual([
      { language: 'fr', name: 'fr_FR-siwis-medium', installed: true, bytes: 63206169, engine: 'piper' },
    ]);
    expect(get(relayStore.connections).get(relayId)!.speechLanguages).toEqual(['en', 'fr']);

    socket.message({
      type: 'push_config', protocol: 3, version: 'abc123', host: 'fedora',
      capabilities: [], agent_profiles: [], inventory: { state: 'ready' },
    });
    const error = await relayStore.installSpeechVoice(relayId, 'fr')
      .then(() => null, (caught) => caught as CommandError);
    expect(error?.message).toBe('This relay does not support phone-managed speech voices yet');
  });
});

describe('device invitation links', () => {
  const invitation = {
    invitation_id: 'C'.repeat(24), version: 1, secret: 'S'.repeat(43), expires_at: '2033-01-01T00:00:00Z',
  };

  beforeEach(() => {
    localStorage.clear();
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
  });

  afterEach(() => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    vi.restoreAllMocks();
  });

  it('carries the derived gateway rendezvous, never the relay key', async () => {
    relayStore.relayConfigs.set([{
      id: 'gw-fedora', label: 'Fedora', url: '', token: '0123456789abcdef0123456789abcdef',
      transport: 'hybrid', gatewayUrl: 'wss://a.example', gatewayUrls: ['wss://a.example', 'wss://b.example'],
    }]);
    const send = vi.spyOn(relayStore, 'sendCommand').mockResolvedValue({
      ok: true, phase: 'confirmed', data: { invitation },
    } as never);
    const link = await relayStore.createDeviceInvitation({ relayId: 'gw-fedora', name: 'Tablet', role: 'reader' });
    expect(send).toHaveBeenCalledWith('gw-fedora', { type: 'create_device_invitation', name: 'Tablet', role: 'reader' });
    const params = new URLSearchParams(new URL(link).hash.slice(1));
    expect(params.get('gateways')).toBe('wss://a.example,wss://b.example');
    // The Go vector for this relay key: routable id plus the challenge key.
    expect(params.get('relay_id')).toBe('Ccy3nT9AULlAceTEnhTvoQ');
    expect(params.get('rendezvous')).toBe('xvT5VptkJHebIfy8b9PSGTJMkdRb-J_P2SXrtNRoLyA');
    expect(params.get('setup')).toBe('S'.repeat(43));
    expect(params.has('relay')).toBe(false);
    expect(link).not.toContain('0123456789abcdef0123456789abcdef');
  });

  it('lets an invited controller invite further devices from its own rendezvous', async () => {
    relayStore.relayConfigs.set([{
      id: 'gw-fedora', label: 'Fedora', url: '', token: '', paired: true,
      transport: 'hybrid', gatewayUrl: 'wss://a.example', gatewayUrls: ['wss://a.example'],
      gatewayRelayId: 'Ccy3nT9AULlAceTEnhTvoQ', rendezvousKey: 'xvT5VptkJHebIfy8b9PSGTJMkdRb-J_P2SXrtNRoLyA',
    }]);
    vi.spyOn(relayStore, 'sendCommand').mockResolvedValue({ ok: true, phase: 'confirmed', data: { invitation } } as never);
    const link = await relayStore.createDeviceInvitation({ relayId: 'gw-fedora', name: 'Tablet', role: 'reader' });
    const params = new URLSearchParams(new URL(link).hash.slice(1));
    expect(params.get('relay_id')).toBe('Ccy3nT9AULlAceTEnhTvoQ');
    expect(params.get('rendezvous')).toBe('xvT5VptkJHebIfy8b9PSGTJMkdRb-J_P2SXrtNRoLyA');
  });

  it('refuses before minting when the computer has no address to carry', async () => {
    relayStore.relayConfigs.set([{
      id: 'gw-fedora', label: 'Fedora', url: '', token: '', paired: true,
      transport: 'hybrid', gatewayUrl: 'wss://a.example', gatewayUrls: ['wss://a.example'],
    }]);
    const send = vi.spyOn(relayStore, 'sendCommand');
    await expect(relayStore.createDeviceInvitation({ relayId: 'gw-fedora', name: 'Tablet', role: 'reader' }))
      .rejects.toThrow(/no address/);
    expect(send).not.toHaveBeenCalled();
  });
});


describe('hidden reconnect retirement', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    MockWebSocket.instances = [];
    vi.stubGlobal('WebSocket', MockWebSocket);
    localStorage.clear();
    sessionStorage.clear();
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({ label: 'Hidden relay', url: 'ws://127.0.0.1:8874', token: '' });
  });

  afterEach(() => {
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('leaves a socket closed after the hidden keepalive cap until one foreground revalidation', async () => {
    const socket = MockWebSocket.instances[0];
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: [] });
    const relayId = get(relayStore.relayConfigs)[0].id;
    relayStore.setHidden(true);
    for (let tick = 0; tick < 29; tick += 1) {
      await vi.advanceTimersByTimeAsync(120_000);
      socket.message({ type: 'pong' });
    }
    await vi.advanceTimersByTimeAsync(120_000);
    const pings = socket.sent.filter(payload => JSON.parse(payload).type === 'ping').length;
    await vi.advanceTimersByTimeAsync(300_000);
    socket.serverClose(1000);
    expect(relayStore.connection(relayId)?.status).toBe('disconnected');
    expect(relayStore.connection(relayId)?.reconnectTimer).toBeNull();
    await vi.advanceTimersByTimeAsync(3_600_000);
    relayStore.revalidateConnections();
    expect(MockWebSocket.instances).toHaveLength(1);
    expect(socket.sent.filter(payload => JSON.parse(payload).type === 'ping')).toHaveLength(pings);
    relayStore.setHidden(false);
    relayStore.revalidateConnections(2_000);
    relayStore.revalidateConnections(2_000);
    expect(MockWebSocket.instances).toHaveLength(2);
    const resumed = MockWebSocket.instances[1];
    resumed.open();
    resumed.message({ type: 'push_config', protocol: 3, capabilities: [] });
    socket.serverClose(1000);
    expect(relayStore.connection(relayId)?.status).toBe('connected');
    await vi.advanceTimersByTimeAsync(1_000);
    expect(MockWebSocket.instances).toHaveLength(2);
    expect(resumed.sent.map(payload => JSON.parse(payload).type)).not.toContain('send_prompt');
  });

  it('cancels a visible reconnect timer on hide and suppresses closes before the cap too', async () => {
    const socket = MockWebSocket.instances[0];
    socket.open();
    socket.serverClose();
    const relayId = get(relayStore.relayConfigs)[0].id;
    expect(relayStore.connection(relayId)?.reconnectTimer).not.toBeNull();
    relayStore.setHidden(true);
    expect(relayStore.connection(relayId)?.reconnectTimer).toBeNull();
    socket.serverClose();
    await vi.advanceTimersByTimeAsync(600_000);
    expect(MockWebSocket.instances).toHaveLength(1);
    relayStore.setHidden(false);
    relayStore.revalidateConnections(2_000);
    expect(MockWebSocket.instances).toHaveLength(2);
  });

  it('cancels update-restart redials and a pending foreground health probe cannot dial while hidden', async () => {
    const socket = MockWebSocket.instances[0];
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: [], update: { state: 'restarting' } });
    const relayId = get(relayStore.relayConfigs)[0].id;
    expect(relayStore.connection(relayId)?.updateRestartTimer).not.toBeNull();
    relayStore.revalidateConnections(2_000);
    relayStore.setHidden(true);
    expect(relayStore.connection(relayId)?.updateRestartTimer).toBeNull();
    await vi.advanceTimersByTimeAsync(60_000);
    expect(MockWebSocket.instances).toHaveLength(1);
    relayStore.setHidden(false);
    relayStore.revalidateConnections(2_000);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(MockWebSocket.instances).toHaveLength(2);
  });
});

describe('command error detail through public transport ingress', () => {
  const actionId = 'detail_action_001';
  const apiError = { code: 'command_refused', args: { reason: 'wire', retryable: false, count: 2 } };
  const details: { name: string; fields: Record<string, unknown>; accepted: string }[] = [
    { name: 'missing', fields: {}, accepted: '' },
    { name: 'undefined omitted by JSON', fields: { detail: undefined }, accepted: '' },
    { name: 'null', fields: { detail: null }, accepted: '' },
    { name: 'number', fields: { detail: 42 }, accepted: '' },
    { name: 'boolean', fields: { detail: false }, accepted: '' },
    { name: 'array', fields: { detail: ['wire'] }, accepted: '' },
    { name: 'record', fields: { detail: { text: 'wire' } }, accepted: '' },
    { name: 'nonempty', fields: { detail: '  exact wire detail  ' }, accepted: '  exact wire detail  ' },
    { name: 'whitespace', fields: { detail: ' \t\n' }, accepted: ' \t\n' },
    { name: 'empty', fields: { detail: '' }, accepted: '' },
    { name: 'ASCII 512 bytes', fields: { detail: 'a'.repeat(512) }, accepted: 'a'.repeat(512) },
    { name: 'ASCII 513 bytes', fields: { detail: 'a'.repeat(513) }, accepted: '' },
    { name: 'two-byte 512 bytes', fields: { detail: 'é'.repeat(256) }, accepted: 'é'.repeat(256) },
    { name: 'two-byte 513 bytes', fields: { detail: 'é'.repeat(256) + 'a' }, accepted: '' },
    { name: 'three-byte 512 bytes', fields: { detail: '界'.repeat(170) + 'ab' }, accepted: '界'.repeat(170) + 'ab' },
    { name: 'three-byte 513 bytes', fields: { detail: '界'.repeat(171) }, accepted: '' },
    { name: 'emoji 512 bytes', fields: { detail: '😀'.repeat(128) }, accepted: '😀'.repeat(128) },
    { name: 'emoji 516 bytes', fields: { detail: '😀'.repeat(129) }, accepted: '' },
    { name: 'lone high surrogate 512 replacement bytes', fields: { detail: 'a'.repeat(509) + '\ud800' }, accepted: 'a'.repeat(509) + '\ud800' },
    { name: 'lone high surrogate 513 replacement bytes', fields: { detail: 'a'.repeat(510) + '\ud800' }, accepted: '' },
    { name: 'lone low surrogate 512 replacement bytes', fields: { detail: 'a'.repeat(509) + '\udc00' }, accepted: 'a'.repeat(509) + '\udc00' },
    { name: 'lone low surrogate 513 replacement bytes', fields: { detail: 'a'.repeat(510) + '\udc00' }, accepted: '' },
    { name: 'decomposed Unicode preserved', fields: { detail: 'e\u0301' }, accepted: 'e\u0301' },
  ];

  beforeEach(() => {
    vi.useFakeTimers();
    transportHijack.current = null;
    MockWebSocket.instances = [];
    vi.stubGlobal('WebSocket', MockWebSocket);
    localStorage.clear();
    sessionStorage.clear();
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({ label: 'Detail relay', url: 'ws://127.0.0.1:8871', token: '' });
    const socket = MockWebSocket.instances[0];
    socket.open();
    socket.message({ type: 'push_config', protocol: 3, capabilities: [], inventory: { state: 'ready' } });
  });

  afterEach(() => {
    transportHijack.current = null;
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  function command() {
    const socket = MockWebSocket.instances.filter(socket => new URL(socket.url).port === '8871').at(-1)!;
    const relayId = get(relayStore.relayConfigs)[0].id;
    const controller = new AbortController();
    const add = vi.spyOn(controller.signal, 'addEventListener');
    const remove = vi.spyOn(controller.signal, 'removeEventListener');
    const before = socket.sent.length;
    const payload = {
      type: 'tab_reorder', pane_id: 'w1:p1', ...exactWireScope('w1:p1', relayId),
      insert_index: 2, intent: { action_id: actionId },
    };
    const resolved = vi.fn();
    const rejected = vi.fn();
    const promise = relayStore.sendCommand(relayId, payload, 1_000, false, controller.signal)
      .then(result => { resolved(result); return result; }, error => { rejected(error); return error; });
    const frames = socket.sent.slice(before).map(frame => JSON.parse(frame));
    expect(frames).toEqual([{ ...payload, request_id: expect.any(String), protocol: 3, client_id: expect.any(String) }]);
    const requestId = frames[0].request_id as string;
    expect(requestId.length).toBeGreaterThan(0);
    expect(frames[0].client_id.length).toBeGreaterThan(0);
    expect(add).toHaveBeenCalledExactlyOnceWith('abort', expect.any(Function), { once: true });
    return { socket, relayId, controller, remove, add, requestId, resolved, rejected, promise };
  }

  function cleaned(pending: ReturnType<typeof command>) {
    expect(pending.remove).toHaveBeenCalledExactlyOnceWith('abort', pending.add.mock.calls[0][1]);
  }

  function failure(error: unknown, message: string, data: Record<string, unknown>) {
    expect(error).toBeInstanceOf(CommandError);
    expect((error as Error).constructor).toBe(CommandError);
    expect((error as Error).message).toBe(message);
    expect((error as CommandError).data).toStrictEqual(data);
    expect((error as CommandError).code).toBeUndefined();
    expect((error as CommandError).args).toBeUndefined();
    expect(Object.keys((error as CommandError).data!).sort()).toEqual(Object.keys(data).sort());
  }

  async function noLaterOwnership(pending: ReturnType<typeof command>) {
    cleaned(pending);
    pending.socket.message({ type: 'action_receipt', request_id: pending.requestId,
      receipt: { action_id: actionId, phase: 'confirmed' }, detail: 'late receipt' });
    pending.socket.message({ type: 'error', request_id: pending.requestId, error: apiError, detail: 'late API error' });
    await vi.advanceTimersByTimeAsync(1_001);
    pending.controller.abort();
    await Promise.resolve();
    cleaned(pending);
    expect(pending.resolved.mock.calls.length + pending.rejected.mock.calls.length).toBe(1);
  }

  for (const phase of ['failed_before_dispatch', 'dispatched_unknown'] as const) {
    for (const withCode of [true, false]) {
      it.each(details)(`receipt ${phase} code=${withCode}: $name`, async ({ fields, accepted }) => {
        const pending = command();
        const receipt = { action_id: actionId, phase, ...(withCode ? { error: apiError } : {}) };
        pending.socket.message({ type: 'action_receipt', request_id: pending.requestId, receipt, ...fields });
        cleaned(pending); // Terminal cleanup is synchronous, before promise rejection observers.
        const error = await pending.promise;
        failure(error, accepted || (withCode ? apiError.code : 'Command failed'), {
          phase, ...(phase === 'failed_before_dispatch' ? { not_started: true } : {}),
          ...(withCode ? { api_error: apiError } : {}),
          ...(phase === 'dispatched_unknown' ? { dispatched_unknown: true } : {}),
        });
        expect(pending.rejected).toHaveBeenCalledExactlyOnceWith(error);
        expect(pending.resolved).not.toHaveBeenCalled();
        await noLaterOwnership(pending);
      });
    }
  }

  it.each(details)('API error: $name', async ({ fields, accepted }) => {
    const pending = command();
    pending.socket.message({ type: 'error', request_id: pending.requestId, error: apiError, ...fields });
    cleaned(pending);
    const error = await pending.promise;
    failure(error, accepted || apiError.code, { phase: 'failed_before_dispatch', api_error: apiError });
    expect(pending.rejected).toHaveBeenCalledExactlyOnceWith(error);
    expect(pending.resolved).not.toHaveBeenCalled();
    await noLaterOwnership(pending);
  });

  it('confirmed receipts resolve despite malformed detail and ignore duplicate failures', async () => {
    const pending = command();
    const receipt = { action_id: actionId, phase: 'confirmed', error: apiError };
    pending.socket.message({ type: 'action_receipt', request_id: pending.requestId, receipt, detail: { invalid: true } });
    cleaned(pending);
    const result = await pending.promise;
    expect(result).toStrictEqual({ type: 'command_result', request_id: pending.requestId,
      action: 'tab_reorder', ok: true, phase: 'confirmed', data: { receipt } });
    expect(pending.resolved).toHaveBeenCalledExactlyOnceWith(result);
    expect(pending.rejected).not.toHaveBeenCalled();
    await noLaterOwnership(pending);
  });

  it.each(['prepared', 'awaiting_evidence'])('%s with malformed detail replaces the original deadline and waits for a terminal receipt', async phase => {
    const pending = command();
    await vi.advanceTimersByTimeAsync(500);
    pending.socket.message({ type: 'action_receipt', request_id: pending.requestId,
      receipt: { action_id: actionId, phase }, detail: ['invalid'] });
    await vi.advanceTimersByTimeAsync(9_999);
    expect(pending.resolved).not.toHaveBeenCalled();
    expect(pending.rejected).not.toHaveBeenCalled();
    expect(pending.remove).not.toHaveBeenCalled();
    const receipt = { action_id: actionId, phase: 'confirmed' };
    pending.socket.message({ type: 'action_receipt', request_id: pending.requestId, receipt, detail: 'a'.repeat(513) });
    expect(await pending.promise).toStrictEqual({ type: 'command_result', request_id: pending.requestId,
      action: 'tab_reorder', ok: true, phase: 'confirmed', data: { receipt } });
    await noLaterOwnership(pending);
  });

  it.each(['prepared', 'awaiting_evidence'])('%s times out at its own ten-second deadline, then releases abort and terminal ownership', async phase => {
    const pending = command();
    await vi.advanceTimersByTimeAsync(500);
    pending.socket.message({ type: 'action_receipt', request_id: pending.requestId,
      receipt: { action_id: actionId, phase }, detail: null });
    await vi.advanceTimersByTimeAsync(9_999);
    expect(pending.rejected).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    const error = await pending.promise;
    failure(error, 'Relay confirmation timed out', { dispatched_unknown: true });
    expect(pending.rejected).toHaveBeenCalledExactlyOnceWith(error);
    await noLaterOwnership(pending);
  });

  it.each(['action_receipt', 'error'])('%s rejects neither malformed envelopes nor wrong request/relay controls before a valid failure', async type => {
    // addRelay reconnects all configured relays; establish both before sending.
    relayStore.addRelay({ label: 'Other detail relay', url: 'ws://127.0.0.1:8872', token: '' });
    const owner = MockWebSocket.instances.filter(socket => new URL(socket.url).port === '8871').at(-1)!;
    owner.open();
    owner.message({ type: 'push_config', protocol: 3, capabilities: [], inventory: { state: 'ready' } });
    const other = MockWebSocket.instances.at(-1)!;
    other.open();
    other.message({ type: 'push_config', protocol: 3, capabilities: [], inventory: { state: 'ready' } });
    const pending = command();
    const valid = type === 'action_receipt'
      ? { type, receipt: { action_id: actionId, phase: 'failed_before_dispatch', error: apiError }, detail: 'first valid failure' }
      : { type, error: apiError, detail: 'first valid failure' };
    other.message({ ...valid, request_id: pending.requestId });
    other.message({ ...valid });
    const malformed = type === 'action_receipt' ? [
      { receipt: null }, { receipt: [] }, { receipt: { action_id: 'bad', phase: 'confirmed' } },
      { receipt: { action_id: actionId, phase: 'invalid' } },
      { receipt: { action_id: actionId, phase: 'confirmed', error: { code: 'BAD CODE' } } },
    ] : [
      { error: null }, { error: [] }, { error: { code: 'BAD CODE' } },
      { error: { code: 'valid_code', args: [] } }, { error: { code: 'valid_code', args: { nested: {} } } },
    ];
    for (const envelope of malformed) pending.socket.message({ type, request_id: pending.requestId, ...envelope });
    // An unrelated receipt action cannot fall back from an unmatched/malformed request ID.
    const unrelated = type === 'action_receipt'
      ? { ...valid, receipt: { action_id: 'unrelated_action', phase: 'confirmed' } }
      : valid;
    for (const request_id of [undefined, null, 42, 'wrong_request']) pending.socket.message({ ...unrelated, request_id });
    await vi.advanceTimersByTimeAsync(100);
    expect(pending.resolved).not.toHaveBeenCalled();
    expect(pending.rejected).not.toHaveBeenCalled();
    expect(pending.remove).not.toHaveBeenCalled();
    pending.socket.message({ ...valid, request_id: pending.requestId });
    const error = await pending.promise;
    failure(error, 'first valid failure', { phase: 'failed_before_dispatch',
      ...(type === 'action_receipt' ? { not_started: true } : {}), api_error: apiError });
    expect(pending.rejected).toHaveBeenCalledExactlyOnceWith(error);
    await noLaterOwnership(pending);
  });

  it.each([undefined, 42, 'unknown_request'])('correlates receipt action-ID fallback for request %s on the owning relay only', async request_id => {
    const pending = command();
    pending.socket.message({ type: 'action_receipt', request_id,
      receipt: { action_id: actionId, phase: 'dispatched_unknown', error: apiError }, detail: 'fallback matched' });
    const error = await pending.promise;
    failure(error, 'fallback matched', { phase: 'dispatched_unknown', api_error: apiError, dispatched_unknown: true });
    expect(pending.rejected).toHaveBeenCalledExactlyOnceWith(error);
    await noLaterOwnership(pending);
    const next = command();
    expect(next.requestId).not.toBe(pending.requestId);
    // A late request ID must not steal a new command unless its action also matches.
    next.socket.message({ type: 'error', request_id: pending.requestId, error: apiError, detail: 'late prior API' });
    await Promise.resolve();
    expect(next.rejected).not.toHaveBeenCalled();
    next.socket.message({ type: 'error', request_id: next.requestId, error: apiError, detail: 'next command' });
    failure(await next.promise, 'next command', { phase: 'failed_before_dispatch', api_error: apiError });
    await noLaterOwnership(next);
  });
});


describe('pending resource release through public consumers', () => {
  type Pending = {
    timer: ReturnType<typeof setTimeout>;
    cleanup?: () => void;
    resolve: (value: any) => void;
    reject: (error: CommandError) => void;
  };
  const registries = relayStore as unknown as {
    pendingRequests: Map<string, Pending>;
    pendingUploads: Map<string, Pending>;
  };
  const digest = '8f8cbb7dcf46e0bc7d53265749a6c17d116093a6ba95e442764060c76fd4a86c';
  const actionId = 'release_action_001';
  function instrumentation() {
    return {
      clear: vi.spyOn(globalThis, 'clearTimeout'),
      add: vi.spyOn(AbortSignal.prototype, 'addEventListener'),
      remove: vi.spyOn(AbortSignal.prototype, 'removeEventListener'),
    };
  }
  let clear: ReturnType<typeof instrumentation>['clear'];
  let add: ReturnType<typeof instrumentation>['add'];
  let remove: ReturnType<typeof instrumentation>['remove'];
  const cleanupOrder: string[] = [];

  beforeEach(() => {
    vi.useFakeTimers();
    transportHijack.current = null;
    MockWebSocket.instances = [];
    vi.stubGlobal('WebSocket', MockWebSocket);
    localStorage.clear();
    sessionStorage.clear();
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    // Adding a relay reconnects all: both must exist before any pending work.
    relayStore.addRelay({ label: 'Release owner', url: 'ws://127.0.0.1:8871', token: '' });
    relayStore.addRelay({ label: 'Release other', url: 'ws://127.0.0.1:8872', token: '' });
    for (const socket of MockWebSocket.instances.slice(-2)) {
      socket.open();
      socket.message({ type: 'push_config', protocol: 3, capabilities: [], inventory: { state: 'ready' } });
    }
    cleanupOrder.length = 0;
    ({ clear, add, remove } = instrumentation());
  });

  afterEach(() => {
    // Restore call-through instrumentation before store teardown drains leftovers.
    vi.restoreAllMocks();
    transportHijack.current = null;
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  function endpoint(port = '8871') {
    const relay = get(relayStore.relayConfigs).find(relay => new URL(relay.url).port === port)!;
    const socket = MockWebSocket.instances.filter(socket => new URL(socket.url).port === port).at(-1)!;
    return { relay, socket };
  }

  function observe(map: Map<string, Pending>, key: string, registered = true) {
    const entry = map.get(key)!;
    expect(entry).toBeDefined();
    const timer = entry.timer;
    const registration = registered ? add.mock.calls.findLastIndex(call => call[0] === 'abort') : -1;
    expect(registration >= 0).toBe(registered);
    const listener = registration < 0 ? undefined : add.mock.calls[registration][1];
    const signal = registration < 0 ? undefined : add.mock.contexts[registration] as AbortSignal;
    if (signal) expect(signal).toBeInstanceOf(AbortSignal);
    const originalCleanup = entry.cleanup!;
    const cleanup = vi.spyOn(entry, 'cleanup').mockImplementation(function (this: Pending) {
      expect(this).toBe(entry);
      expect(map.get(key)).toBe(entry); // Deletion/key evaluation remains caller-local, after cleanup.
      expect(clear).toHaveBeenCalledWith(entry.timer);
      cleanupOrder.push(key);
      return originalCleanup.call(this);
    });
    const resolve = vi.spyOn(entry, 'resolve'); // Actual registered callbacks, not Promise-only counters.
    const reject = vi.spyOn(entry, 'reject');
    const deletion = vi.spyOn(map, 'delete');
    return { map, key, entry, timer, signal, listener, originalCleanup, cleanup, resolve, reject, deletion };
  }
  type Observation = ReturnType<typeof observe>;

  function released(pending: Observation, outcome: 'resolve' | 'reject', timer = pending.timer) {
    expect(pending.map.has(pending.key)).toBe(false);
    expect(pending.cleanup).toHaveBeenCalledTimes(1);
    expect(clear.mock.calls.filter(call => call[0] === timer)).toHaveLength(1);
    const clearIndex = clear.mock.calls.findIndex(call => call[0] === timer);
    const deleteIndex = pending.deletion.mock.calls.findLastIndex(call => call[0] === pending.key);
    const callback = pending[outcome];
    expect(callback).toHaveBeenCalledTimes(1);
    expect(pending[outcome === 'resolve' ? 'reject' : 'resolve']).not.toHaveBeenCalled();
    expect(clear.mock.invocationCallOrder[clearIndex]).toBeLessThan(pending.cleanup.mock.invocationCallOrder[0]);
    expect(pending.cleanup.mock.invocationCallOrder[0]).toBeLessThan(pending.deletion.mock.invocationCallOrder[deleteIndex]);
    expect(pending.deletion.mock.invocationCallOrder[deleteIndex]).toBeLessThan(callback.mock.invocationCallOrder[0]);
    if (pending.signal) {
      const removals = remove.mock.calls.filter((call, index) => remove.mock.contexts[index] === pending.signal && call[1] === pending.listener);
      expect(removals).toEqual([['abort', pending.listener]]);
    }
  }

  function command(port = '8871', type = 'tab_reorder', timeout = 60_000) {
    const { relay, socket } = endpoint(port);
    const controller = new AbortController();
    const payload = { type, pane_id: 'w1:p1', ...exactWireScope('w1:p1', relay.id),
      insert_index: 2, intent: { action_id: actionId } };
    const promise = relayStore.sendCommand(relay.id, payload, timeout, false, controller.signal)
      .then(result => result, error => error);
    const frame = JSON.parse(socket.sent.at(-1)!);
    expect(frame).toEqual({ ...payload, request_id: expect.any(String), protocol: 3, client_id: expect.any(String) });
    expect(frame.request_id).not.toBe('');
    return { ...observe(registries.pendingRequests, frame.request_id), frame, promise, socket, relay, controller };
  }

  function legacy(pending: ReturnType<typeof command>, fields: Record<string, unknown> = { ok: true }) {
    return { type: 'command_result', request_id: pending.key, action: pending.frame.type, ...fields };
  }

  async function noLateCommandOwnership(pending: ReturnType<typeof command>) {
    const calls = [pending.cleanup.mock.calls.length, pending.resolve.mock.calls.length, pending.reject.mock.calls.length];
    pending.socket.message(legacy(pending, { ok: false, error: 'duplicate' }));
    pending.socket.message({ type: 'error', request_id: pending.key, error: { code: 'late_error' } });
    pending.controller.abort();
    await vi.advanceTimersByTimeAsync(60_001);
    expect([pending.cleanup.mock.calls.length, pending.resolve.mock.calls.length, pending.reject.mock.calls.length]).toEqual(calls);
  }

  it.each([
    { ok: true, phase: undefined, data: { exact: 1 } },
    { ok: false, phase: undefined, error: 'legacy failure', data: { exact: 2 } },
    { ok: false, phase: 'not_started', error: 'not started', data: { exact: 3 } },
    { ok: false, phase: 'dispatched_unknown', error: 'uncertain', data: { exact: 4 } },
  ])('legacy terminal $ok/$phase releases actual resources before the unchanged outcome', async fields => {
    const pending = command();
    const frame = legacy(pending, fields);
    pending.socket.message(frame);
    released(pending, fields.ok ? 'resolve' : 'reject');
    const outcome = await pending.promise;
    if (fields.ok) expect(outcome).toStrictEqual(JSON.parse(JSON.stringify(frame)));
    else {
      expect(outcome).toBeInstanceOf(CommandError);
      expect(outcome.constructor).toBe(CommandError);
      expect(outcome.message).toBe(fields.error);
      expect(outcome.data).toEqual({ ...fields.data,
        ...(fields.phase === 'not_started' ? { not_started: true } : {}),
        ...(fields.phase === 'dispatched_unknown' ? { dispatched_unknown: true } : {}) });
    }
    await noLateCommandOwnership(pending);
  });

  it.each(['receipt', 'api'] as const)('%s terminal observes current timer, map, listener and raw rejection order', async kind => {
    const pending = command();
    const error = { code: 'release_refused', args: { count: 2 } };
    pending.socket.message(kind === 'receipt'
      ? { type: 'action_receipt', request_id: pending.key, receipt: { action_id: actionId, phase: 'failed_before_dispatch', error } }
      : { type: 'error', request_id: pending.key, error });
    released(pending, 'reject');
    const outcome = await pending.promise;
    expect(outcome.constructor).toBe(CommandError);
    expect(outcome.message).toBe(error.code);
    expect(outcome.data).toEqual({ phase: 'failed_before_dispatch', api_error: error,
      ...(kind === 'receipt' ? { not_started: true } : {}) });
    await noLateCommandOwnership(pending);
  });

  it('keeps conditional action-ID fallback and found-entry behavior, including wrong-relay lookup', async () => {
    const first = command();
    const other = command('8872');
    const receipt = { action_id: actionId, phase: 'confirmed' };
    // A found wrong-relay entry prevents fallback to the otherwise matching owner entry.
    first.socket.message({ type: 'action_receipt', request_id: other.key, receipt });
    first.socket.message({ type: 'action_receipt', request_id: 'missing', receipt: { ...receipt, action_id: 'unrelated_action' } });
    first.socket.message({ type: 'command_result', request_id: other.key, ok: true });
    expect(first.cleanup).not.toHaveBeenCalled();
    expect(other.cleanup).not.toHaveBeenCalled();
    first.socket.message({ type: 'action_receipt', request_id: 'missing', receipt });
    released(first, 'resolve');
    expect(await first.promise).toMatchObject({ ok: true, request_id: first.key });
    // A found owning entry is not additionally action-ID checked.
    other.socket.message({ type: 'action_receipt', request_id: other.key, receipt: { ...receipt, action_id: 'different_action' } });
    released(other, 'resolve');
    expect(await other.promise).toMatchObject({ ok: true, request_id: other.key });
  });

  it.each(['prepared', 'awaiting_evidence', 'accepted'])('%s repeatedly replaces deadlines; terminal clears the latest timer', async phase => {
    const pending = command('8871', 'tab_reorder', 1_000);
    let timer = pending.timer;
    for (let index = 0; index < 2; index++) {
      await vi.advanceTimersByTimeAsync(400);
      pending.socket.message(phase === 'accepted' ? legacy(pending, { phase, ok: true })
        : { type: 'action_receipt', request_id: pending.key, receipt: { action_id: actionId, phase } });
      expect(clear).toHaveBeenCalledWith(timer);
      expect(pending.entry.timer).not.toBe(timer);
      timer = pending.entry.timer;
      expect(pending.cleanup).not.toHaveBeenCalled();
      expect(pending.resolve).not.toHaveBeenCalled();
      expect(pending.reject).not.toHaveBeenCalled();
    }
    await vi.advanceTimersByTimeAsync(1_001); // Both original deadlines would have elapsed.
    expect(pending.cleanup).not.toHaveBeenCalled();
    pending.socket.message(legacy(pending));
    released(pending, 'resolve', timer);
    expect(await pending.promise).toMatchObject({ ok: true });
    await noLateCommandOwnership(pending);
  });

  it('agent_start accepted retains its original timer and toast, then terminal releases it', async () => {
    const pending = command('8871', 'agent_start', 1_000);
    const toast = vi.spyOn(relayStore, 'showToast');
    pending.socket.message(legacy(pending, { phase: 'accepted', ok: true }));
    expect(pending.entry.timer).toBe(pending.timer);
    expect(clear).not.toHaveBeenCalledWith(pending.timer);
    expect(toast).toHaveBeenCalledExactlyOnceWith('Start accepted; waiting for confirmation…');
    await vi.advanceTimersByTimeAsync(999);
    expect(pending.cleanup).not.toHaveBeenCalled();
    pending.socket.message(legacy(pending));
    released(pending, 'resolve');
    expect(await pending.promise).toMatchObject({ ok: true });
    await noLateCommandOwnership(pending);
  });

  it.each(['prepared', 'awaiting_evidence', 'accepted'])('cancels the active command deadline on abort after %s without claiming universal cleanup-once', async phase => {
    const pending = command('8871', 'tab_reorder', 1_000);
    pending.socket.message(phase === 'accepted' ? legacy(pending, { phase, ok: true })
      : { type: 'action_receipt', request_id: pending.key, receipt: { action_id: actionId, phase } });
    const replacement = pending.entry.timer;
    // A surviving replacement callback would run after abort removed the entry; keep this diagnostic guard.
    const original = pending.cleanup.getMockImplementation()!;
    pending.cleanup.mockImplementation(function (this: Pending) {
      expect(this).toBe(pending.entry);
      expect(pending.map.has(pending.key)).toBe(false);
      return pending.originalCleanup.call(this);
    });
    pending.controller.abort();
    const first = await pending.promise;
    expect(first.constructor).toBe(CommandError);
    expect(first.message).toBe('Conversation history request was cancelled.');
    expect(first.code).toBe('request_cancelled');
    expect(first.data).toBeUndefined();
    expect(pending.map.has(pending.key)).toBe(false);
    expect(pending.cleanup).not.toHaveBeenCalled(); // Abort calls captured closures, not record methods.
    expect(pending.reject).not.toHaveBeenCalled();
    expect(remove.mock.calls.filter(call => call[1] === pending.listener)).toHaveLength(1);
    expect(clear.mock.calls.filter(call => call[0] === replacement)).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(pending.cleanup).not.toHaveBeenCalled();
    expect(pending.reject).not.toHaveBeenCalled();
    expect(remove.mock.calls.filter(call => call[1] === pending.listener)).toHaveLength(1);
    expect(await pending.promise).toBe(first); // Promise first outcome is distinct from raw callbacks.
    pending.cleanup.mockImplementation(original);
  });

  it('bounded reentrant terminal cleanup preserves first settlement and outer continuation', async () => {
    const pending = command();
    const original = pending.cleanup.getMockImplementation()!;
    let entered = false;
    pending.cleanup.mockImplementation(function (this: Pending) {
      original.call(this);
      if (entered) return;
      entered = true;
      pending.socket.message(legacy(pending, { ok: true, data: { first: true } }));
    });
    pending.socket.message(legacy(pending, { ok: false, error: 'outer continues' }));
    expect(pending.cleanup).toHaveBeenCalledTimes(2);
    expect(pending.resolve).toHaveBeenCalledTimes(1);
    expect(pending.reject).toHaveBeenCalledTimes(1);
    expect(pending.reject.mock.calls[0][0]).toBeInstanceOf(CommandError);
    expect(pending.reject.mock.calls[0][0].message).toBe('outer continues');
    expect(await pending.promise).toMatchObject({ ok: true, data: { first: true } });
    expect(pending.map.has(pending.key)).toBe(false);
  });

  function agent(port = '8871'): Agent {
    const { relay } = endpoint(port);
    return { relay_id: relay.id, relay_label: relay.label, raw_pane_id: 'w1:p1', pane_id: `${relay.id}::w1:p1`,
      server_session_id: 'release-session', terminal_id: 'release-terminal', generation: 7, agent_session_id: 'release-agent' };
  }
  const target = { server_session_id: 'release-session', pane_id: 'w1:p1', terminal_id: 'release-terminal', generation: 7, agent_session_id: 'release-agent' };
  function beginResult() {
    return { upload_id: 'release-upload', chunk_bytes: 3, expires_at: new Date(Date.now() + 60_000).toISOString(),
      limits: { max_files: 8, max_file_bytes: 20 * 1024 * 1024, max_batch_bytes: 50 * 1024 * 1024 } };
  }
  function finishResult() {
    return { attachments: [{ ref: 'attachment:release-opaque', name: 'shot.png', media_type: 'image/png', bytes: 3,
      sha256: digest, expires_at: new Date(Date.now() + 60_000).toISOString() }] };
  }
  function uploadedFrame(socket: MockWebSocket, type: string) {
    return socket.sent.map(body => JSON.parse(body)).findLast(frame => frame.type === type);
  }
  async function uploadFrame(socket: MockWebSocket, type: string) {
    await vi.waitFor(() => expect(uploadedFrame(socket, type)).toBeDefined());
    const frame = uploadedFrame(socket, type);
    const fields = type === 'upload_begin' ? { files: [{ name: 'shot.png', media_type: 'image/png', bytes: 3 }] }
      : type === 'upload_chunk' ? { upload_id: 'release-upload', file_index: 0, sequence: 0, data: 'cG5n', sha256: digest }
      : type === 'upload_finish' ? { upload_id: 'release-upload', files: [{ file_index: 0, sha256: digest }] }
      : { upload_id: 'release-upload' };
    expect(frame).toEqual({ type, target, ...fields, protocol: 3, request_id: expect.any(String), client_id: expect.any(String) });
    expect(frame.request_id).not.toBe('');
    return { ...observe(registries.pendingUploads, frame.request_id, type !== 'upload_cancel'), frame, socket };
  }
  type UploadObservation = Awaited<ReturnType<typeof uploadFrame>>;
  function reply(pending: UploadObservation, fields: Record<string, unknown>) {
    pending.socket.message({ type: pending.frame.type + '_result', request_id: pending.key, ...fields });
  }
  function ignoredUploadControls(pending: UploadObservation) {
    endpoint('8872').socket.message({ type: pending.frame.type + '_result', request_id: pending.key, result: {} });
    pending.socket.message({ type: pending.frame.type + '_result', request_id: 'unrelated_request', error: { code: 'attachment_upload_busy' } });
    pending.socket.message({ type: pending.frame.type === 'upload_begin' ? 'upload_chunk_result' : 'upload_begin_result', request_id: pending.key, result: {} });
    expect(pending.map.get(pending.key)).toBe(pending.entry);
    expect(pending.cleanup).not.toHaveBeenCalled();
    expect(clear).not.toHaveBeenCalledWith(pending.timer);
    expect(pending.resolve).not.toHaveBeenCalled();
    expect(pending.reject).not.toHaveBeenCalled();
  }
  async function noLateUploadOwnership(pending: UploadObservation) {
    const counts = [pending.cleanup.mock.calls.length, pending.resolve.mock.calls.length, pending.reject.mock.calls.length];
    reply(pending, { result: {} });
    reply(pending, { error: { code: 'attachment_upload_busy' } });
    if (pending.signal) {
      // AbortSignal is native Node here, not jsdom's Event realm. Deliver a native abort event after its dispatch finishes.
      const trigger = new AbortController();
      let event!: Event;
      trigger.signal.addEventListener('abort', value => { event = value; }, { once: true });
      trigger.abort();
      pending.signal.dispatchEvent(event);
    }
    await vi.advanceTimersByTimeAsync(60_001);
    expect([pending.cleanup.mock.calls.length, pending.resolve.mock.calls.length, pending.reject.mock.calls.length]).toEqual(counts);
  }

  it('public uploadAttachments preserves exact target/session/digests/progression and normal raw once-only release', async () => {
    const { socket } = endpoint();
    const upload = relayStore.uploadAttachments(agent(), [new File(['png'], 'shot.png', { type: 'image/png' })]);
    const begin = await uploadFrame(socket, 'upload_begin');
    ignoredUploadControls(begin);
    reply(begin, { result: beginResult() });
    released(begin, 'resolve');
    const chunk = await uploadFrame(socket, 'upload_chunk');
    ignoredUploadControls(chunk);
    reply(chunk, { result: { file_index: 0, next_sequence: 1, received_bytes: 3 } });
    released(chunk, 'resolve');
    const finish = await uploadFrame(socket, 'upload_finish');
    ignoredUploadControls(finish);
    const result = finishResult();
    reply(finish, { result });
    released(finish, 'resolve');
    expect(await upload).toEqual(result.attachments);
    expect(new Set([begin.key, chunk.key, finish.key]).size).toBe(3);
    for (const pending of [begin, chunk, finish]) await noLateUploadOwnership(pending);
  });

  async function atUploadStage(stage: string) {
    const { socket } = endpoint();
    const controller = relayStore.attachmentController(agent());
    controller.select([new File(['png'], 'shot.png', { type: 'image/png' })]);
    let promise = controller.upload().then(value => value, error => error);
    let pending = await uploadFrame(socket, 'upload_begin');
    if (stage === 'upload_begin') return { pending, promise, controller };
    reply(pending, { result: beginResult() });
    released(pending, 'resolve');
    pending = await uploadFrame(socket, 'upload_chunk');
    if (stage === 'upload_chunk') return { pending, promise, controller };
    if (stage === 'upload_cancel') {
      const upload = promise;
      promise = controller.cancel().then(value => value, error => error);
      expect(await upload).toEqual([]);
      pending = await uploadFrame(socket, 'upload_cancel');
      return { pending, promise, controller };
    }
    reply(pending, { result: { file_index: 0, next_sequence: 1, received_bytes: 3 } });
    released(pending, 'resolve');
    pending = await uploadFrame(socket, 'upload_finish');
    return { pending, promise, controller };
  }

  const uploadFailures = [
    { name: 'parsed error with args', fields: { error: { code: 'attachment_upload_busy', args: { count: 2, retryable: false, reason: 'wire' } } }, message: 'attachment_upload_busy', code: 'attachment_upload_busy', args: { count: 2, retryable: false, reason: 'wire' } },
    { name: 'malformed present null error', fields: { error: null }, message: 'Attachment upload failed.', code: undefined, args: undefined },
    { name: 'malformed present code error', fields: { error: { code: 'BAD CODE' } }, message: 'Attachment upload failed.', code: undefined, args: undefined },
    { name: 'missing result', fields: {}, message: 'Relay returned an invalid attachment upload result.', code: 'attachment_invalid_response', args: undefined },
    { name: 'null result', fields: { result: null }, message: 'Relay returned an invalid attachment upload result.', code: 'attachment_invalid_response', args: undefined },
    { name: 'array result', fields: { result: [] }, message: 'Relay returned an invalid attachment upload result.', code: 'attachment_invalid_response', args: undefined },
  ];
  for (const stage of ['upload_begin', 'upload_chunk', 'upload_finish', 'upload_cancel']) {
    it.each(uploadFailures)(`${stage}: $name releases actual pending entry and preserves public issue translation`, async scenario => {
      const { pending, promise, controller } = await atUploadStage(stage);
      ignoredUploadControls(pending);
      reply(pending, scenario.fields);
      released(pending, 'reject');
      const raw = pending.reject.mock.calls[0][0];
      expect(raw).toBeInstanceOf(CommandError);
      expect(raw.constructor).toBe(CommandError);
      expect(raw.message).toBe(scenario.message);
      expect(raw.code).toBe(scenario.code);
      expect(raw.args).toEqual(scenario.args);
      expect(raw.data).toBeUndefined();
      const code = stage === 'upload_cancel' ? 'attachment_cancel_failed'
        : scenario.code || (stage === 'upload_begin' ? 'attachment_upload_failed' : 'attachment_upload_state_unknown');
      expect(await promise).toEqual({ code, ...(stage !== 'upload_cancel' && scenario.code === 'attachment_upload_busy' ? { args: scenario.args } : {}) });
      if (stage === 'upload_cancel') expect(controller.snapshot()).toMatchObject({ items: [], uploading: false });
      else expect(controller.snapshot()).toMatchObject({ uploading: false, items: [{ state: 'interrupted', issue: { code } }] });
      await noLateUploadOwnership(pending);
    });
  }

  it('public controller cancellation settles only its matching result; absent listener cleanup remains harmless', async () => {
    const { pending, promise, controller } = await atUploadStage('upload_cancel');
    ignoredUploadControls(pending);
    const removals = remove.mock.calls.length;
    reply(pending, { result: {} });
    released(pending, 'resolve');
    expect(remove.mock.calls).toHaveLength(removals);
    expect(await promise).toBeUndefined();
    expect(controller.snapshot()).toMatchObject({ items: [], uploading: false });
    await noLateUploadOwnership(pending);
  });

  it.each(['closed', 'disconnect', 'retire'])('%s drains only its owning relay in live command-before-upload order', async mode => {
    const { relay, socket } = endpoint();
    const other = endpoint('8872');
    if (mode === 'retire') {
      relayStore.setHidden(true);
      await vi.advanceTimersByTimeAsync(120_000); // Establish health deadline before pending60s uploads.
      other.socket.message({ type: 'pong' });
    }
    const first = command();
    const second = command();
    const third = command();
    const survivor = command('8872');
    const upload = relayStore.uploadAttachments(agent(), [new File(['png'], 'shot.png', { type: 'image/png' })]).catch(error => error);
    const ownerUpload = await uploadFrame(socket, 'upload_begin');
    const liveUpload = relayStore.uploadAttachments(agent('8872'), [new File(['png'], 'shot.png', { type: 'image/png' })]).catch(error => error);
    const otherUpload = await uploadFrame(other.socket, 'upload_begin');
    const original = first.cleanup.getMockImplementation()!;
    first.cleanup.mockImplementation(function (this: Pending) {
      original.call(this);
      // Reorder an ACTUAL publicly registered record during cleanup. A snapshot iterator would retain the old order.
      first.map.delete(second.key);
      first.map.set(second.key, second.entry);
    });
    if (mode === 'closed') socket.serverClose();
    else if (mode === 'disconnect') relayStore.disconnectRelay(relay.id);
    else await vi.advanceTimersByTimeAsync(10_000);
    expect(cleanupOrder).toEqual([first.key, third.key, second.key, ownerUpload.key]);
    for (const pending of [first, second, third, ownerUpload]) released(pending, 'reject');
    for (const pending of [first, second, third]) {
      const error = await pending.promise;
      expect(error.constructor).toBe(CommandError);
      expect(error.message).toBe('Relay disconnected');
      expect(error.data).toEqual({ dispatched_unknown: true });
    }
    expect(await upload).toEqual({ code: 'attachment_upload_failed' });
    expect(ownerUpload.reject.mock.calls[0][0].data).toEqual({ dispatched_unknown: true });
    expect(survivor.map.get(survivor.key)).toBe(survivor.entry);
    expect(otherUpload.map.get(otherUpload.key)).toBe(otherUpload.entry);
    expect(survivor.cleanup).not.toHaveBeenCalled();
    expect(otherUpload.cleanup).not.toHaveBeenCalled();
    other.socket.message(legacy(survivor));
    reply(otherUpload, { error: { code: 'attachment_upload_busy', args: { owner: 'other' } } });
    released(survivor, 'resolve');
    released(otherUpload, 'reject');
    expect(await survivor.promise).toMatchObject({ ok: true });
    expect(await liveUpload).toEqual({ code: 'attachment_upload_busy', args: { owner: 'other' } });
    socket.message(legacy(first));
    reply(ownerUpload, { result: beginResult() });
    expect(first.resolve).not.toHaveBeenCalled();
    expect(ownerUpload.resolve).not.toHaveBeenCalled();
  });

  it('connected-path promotion keeps real pending command/upload and settles through actual transport ingress', async () => {
    relayStore.destroy();
    relayStore.relayConfigs.set([]);
    let ingress!: TransportHandlers;
    const sent: Record<string, unknown>[] = [];
    transportHijack.current = (_relay, handlers) => ({
      kind: 'gateway',
      connect() { ingress = handlers; handlers.onStatus('connecting'); },
      send(payload) { sent.push(JSON.parse(JSON.stringify(payload))); return true; },
      close() {},
    });
    relayStore.addRelay({ label: 'Promotion', url: 'ws://127.0.0.1:8871', token: '', transport: 'hybrid', gatewayUrl: 'wss://gateway.invalid' });
    ingress.onStatus('connected', { path: 'gateway' });
    ingress.onMessage({ type: 'push_config', protocol: 3, capabilities: [], inventory: { state: 'ready' } });
    const relay = get(relayStore.relayConfigs)[0];
    const abort = new AbortController();
    const commandPromise = relayStore.sendCommand(relay.id, { type: 'tab_reorder', insert_index: 2 }, 60_000, false, abort.signal);
    const frame = sent.at(-1)!;
    expect(frame).toEqual({ type: 'tab_reorder', insert_index: 2, request_id: expect.any(String), protocol: 3, client_id: expect.any(String) });
    const pending = observe(registries.pendingRequests, String(frame.request_id));
    const upload = relayStore.uploadAttachments(agent(), [new File(['png'], 'shot.png', { type: 'image/png' })]).catch(error => error);
    const uploadFrame = sent.find(frame => frame.type === 'upload_begin')!;
    expect(uploadFrame).toEqual({ type: 'upload_begin', target, files: [{ name: 'shot.png', media_type: 'image/png', bytes: 3 }], request_id: expect.any(String), protocol: 3, client_id: expect.any(String) });
    const pendingUpload = observe(registries.pendingUploads, String(uploadFrame.request_id));
    ingress.onStatus('connected', { path: 'webrtc' });
    expect(get(relayStore.connections).get(relay.id)?.path).toBe('webrtc');
    for (const entry of [pending, pendingUpload]) {
      expect(entry.map.get(entry.key)).toBe(entry.entry);
      expect(entry.cleanup).not.toHaveBeenCalled();
      expect(clear).not.toHaveBeenCalledWith(entry.timer);
    }
    ingress.onMessage({ type: 'command_result', request_id: pending.key, ok: true, data: { promoted: true } });
    ingress.onMessage({ type: 'upload_begin_result', request_id: pendingUpload.key, error: { code: 'attachment_upload_busy', args: { promoted: true } } });
    released(pending, 'resolve');
    released(pendingUpload, 'reject');
    expect(await commandPromise).toMatchObject({ ok: true, data: { promoted: true } });
    expect(await upload).toEqual({ code: 'attachment_upload_busy', args: { promoted: true } });
  });
});

describe('pending deadline replacement through public ingress', () => {
  type Pending = {
    timer: ReturnType<typeof setTimeout>;
    cleanup?: () => void;
    resolve: (value: any) => void;
    reject: (error: CommandError) => void;
  };
  const requests = (relayStore as unknown as { pendingRequests: Map<string, Pending> }).pendingRequests;
  const actionId = 'deadline_action_034';
  function instrumentation() {
    return {
      clear: vi.spyOn(globalThis, 'clearTimeout'),
      schedule: vi.spyOn(globalThis, 'setTimeout'),
      add: vi.spyOn(AbortSignal.prototype, 'addEventListener'),
      remove: vi.spyOn(AbortSignal.prototype, 'removeEventListener'),
      toast: vi.spyOn(relayStore, 'showToast'),
    };
  }
  let timers: ReturnType<typeof instrumentation>;

  beforeEach(() => {
    vi.useFakeTimers();
    transportHijack.current = null;
    MockWebSocket.instances = [];
    vi.stubGlobal('WebSocket', MockWebSocket);
    localStorage.clear();
    sessionStorage.clear();
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({ label: 'Deadline owner', url: 'ws://127.0.0.1:8871', token: '' });
    relayStore.addRelay({ label: 'Deadline other', url: 'ws://127.0.0.1:8872', token: '' });
    // addRelay reconnects all: establish both before registering any command.
    for (const socket of MockWebSocket.instances.slice(-2)) {
      socket.open();
      socket.message({ type: 'push_config', protocol: 3, capabilities: [], inventory: { state: 'ready' } });
    }
    relayStore.toast.set(null);
    timers = instrumentation(); // All spies call through to the installed fake/native APIs.
  });

  afterEach(() => {
    vi.restoreAllMocks();
    transportHijack.current = null;
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  function command(timeoutMs?: number, type = 'tab_reorder') {
    const relay = get(relayStore.relayConfigs).find(relay => new URL(relay.url).port === '8871')!;
    const socket = MockWebSocket.instances.filter(socket => new URL(socket.url).port === '8871').at(-1)!;
    const controller = new AbortController();
    const outcomes: string[] = [];
    const payload = { type, pane_id: 'w1:p1', ...exactWireScope('w1:p1', relay.id),
      insert_index: 2, intent: { action_id: actionId } };
    const promise = relayStore.sendCommand(relay.id, payload, timeoutMs, false, controller.signal)
      .then(value => { outcomes.push('resolved'); return value; },
        error => { outcomes.push('rejected'); return error; });
    const frame = JSON.parse(socket.sent.at(-1)!);
    expect(frame).toEqual({ ...payload, request_id: expect.any(String), protocol: 3, client_id: expect.any(String) });
    const key: string = frame.request_id;
    const entry = requests.get(key)!;
    expect(entry).toBeDefined();
    const originalTimer = entry.timer;
    const initial = timers.schedule.mock.results.findIndex(result => result.value === originalTimer);
    expect(timers.schedule.mock.calls[initial][1]).toBe(timeoutMs ?? (type === 'agent_start' ? 60_000 : 15_000));
    const registration = timers.add.mock.contexts.findIndex(signal => signal === controller.signal);
    expect(timers.add.mock.calls[registration]).toEqual(['abort', expect.any(Function), { once: true }]);
    const listener = timers.add.mock.calls[registration][1];
    const originalCleanup = entry.cleanup!;
    const cleanup = vi.spyOn(entry, 'cleanup').mockImplementation(function (this: Pending) {
      expect(this).toBe(entry);
      expect(requests.get(key)).toBe(entry);
      return originalCleanup.call(this);
    });
    const deletion = vi.spyOn(requests, 'delete');
    const resolve = vi.spyOn(entry, 'resolve');
    const reject = vi.spyOn(entry, 'reject');
    return { socket, controller, promise, outcomes, key, entry, originalTimer, listener,
      cleanup, deletion, resolve, reject };
  }
  type Observation = ReturnType<typeof command>;

  function ingress(pending: Observation, phase: string) {
    pending.socket.message(phase === 'accepted'
      ? { type: 'command_result', request_id: pending.key, action: 'tab_reorder', phase, ok: true }
      : { type: 'action_receipt', request_id: pending.key, receipt: { action_id: actionId, phase } });
  }

  function renew(pending: Observation, phase: string) {
    const old = pending.entry.timer;
    const scheduledBefore = timers.schedule.mock.calls.length;
    const clearedBefore = timers.clear.mock.calls.length;
    ingress(pending, phase);
    const schedules = timers.schedule.mock.calls.map((call, index) => ({ call, index }))
      .filter(({ call, index }) => index >= scheduledBefore && call[1] === 10_000);
    expect(schedules).toHaveLength(1);
    const scheduled = schedules[0].index;
    const cleared = timers.clear.mock.calls.findIndex((call, index) => index >= clearedBefore && call[0] === old);
    expect(cleared).toBeGreaterThanOrEqual(clearedBefore);
    expect(timers.clear.mock.invocationCallOrder[cleared]).toBeLessThan(timers.schedule.mock.invocationCallOrder[scheduled]);
    expect(timers.clear.mock.calls.filter(call => call[0] === old)).toHaveLength(1);
    expect(pending.entry.timer).toBe(timers.schedule.mock.results[scheduled].value);
    expect(pending.entry.timer).not.toBe(old);
    expect(requests.get(pending.key)).toBe(pending.entry);
    expect(pending.cleanup).not.toHaveBeenCalled();
    expect(pending.resolve).not.toHaveBeenCalled();
    expect(pending.reject).not.toHaveBeenCalled();
    expect(pending.outcomes).toEqual([]);
    if (phase === 'accepted') expect(timers.toast).toHaveBeenLastCalledWith('Command accepted; waiting for agent…');
    return pending.entry.timer;
  }

  function settled(pending: Observation, outcome: 'resolve' | 'reject') {
    expect(pending.cleanup).toHaveBeenCalledTimes(1);
    expect(requests.has(pending.key)).toBe(false);
    const deletion = pending.deletion.mock.calls.findIndex(call => call[0] === pending.key);
    expect(deletion).toBeGreaterThanOrEqual(0);
    expect(pending.cleanup.mock.invocationCallOrder[0]).toBeLessThan(pending.deletion.mock.invocationCallOrder[deletion]);
    expect(pending.deletion.mock.invocationCallOrder[deletion]).toBeLessThan(pending[outcome].mock.invocationCallOrder[0]);
    expect(pending[outcome]).toHaveBeenCalledTimes(1);
    expect(pending[outcome === 'resolve' ? 'reject' : 'resolve']).not.toHaveBeenCalled();
    expect(timers.remove.mock.calls.filter((call, index) =>
      timers.remove.mock.contexts[index] === pending.controller.signal && call[1] === pending.listener))
      .toEqual([['abort', pending.listener]]);
  }

  async function timedOut(pending: Observation) {
    const error = await pending.promise;
    expect(error).toBeInstanceOf(CommandError);
    expect(error.constructor).toBe(CommandError);
    expect(error.message).toBe('Relay confirmation timed out');
    expect(error.data).toStrictEqual({ dispatched_unknown: true });
    expect(error.code).toBeUndefined();
    expect(error.args).toBeUndefined();
    expect(pending.reject).toHaveBeenCalledExactlyOnceWith(error);
    expect(pending.outcomes).toEqual(['rejected']);
    settled(pending, 'reject'); // Fired timeout needs no normal-terminal clear assertion.
  }

  async function noLaterOwnership(pending: Observation) {
    const before = [pending.cleanup.mock.calls.length, pending.resolve.mock.calls.length, pending.reject.mock.calls.length];
    ingress(pending, 'confirmed');
    pending.socket.message({ type: 'command_result', request_id: pending.key, ok: false, error: 'late duplicate' });
    pending.controller.abort(); // Native signal; no jsdom Event/native AbortSignal mixing.
    await vi.advanceTimersByTimeAsync(60_001);
    expect([pending.cleanup.mock.calls.length, pending.resolve.mock.calls.length, pending.reject.mock.calls.length]).toEqual(before);
    expect(pending.outcomes).toHaveLength(1);
    expect(requests.has(pending.key)).toBe(false);
  }

  it.each(['prepared', 'awaiting_evidence', 'accepted'].flatMap(phase =>
    [undefined, 1_000].map(timeoutMs => ({ phase, timeoutMs }))))('$phase renewals replace a $timeoutMs initial deadline until their first actual timeout', async ({ phase, timeoutMs }) => {
    const pending = command(timeoutMs);
    await vi.advanceTimersByTimeAsync(400);
    renew(pending, phase);
    await vi.advanceTimersByTimeAsync(400);
    const latest = renew(pending, phase);
    await vi.advanceTimersByTimeAsync(9_999); // Past original and first-renewal deadlines.
    expect(pending.outcomes).toEqual([]);
    expect(pending.cleanup).not.toHaveBeenCalled();
    expect(requests.get(pending.key)).toBe(pending.entry);
    await vi.advanceTimersByTimeAsync(1);
    await timedOut(pending);
    expect(timers.clear).not.toHaveBeenCalledWith(latest);
    if (phase === 'accepted') expect(timers.toast.mock.calls).toEqual([
      ['Command accepted; waiting for agent…'], ['Command accepted; waiting for agent…'],
    ]);
    else expect(timers.toast).not.toHaveBeenCalled();
    await noLaterOwnership(pending);
  });

  it.each([true, false])('alternating receipt/legacy deadlines retain the first final outcome: success=%s', async success => {
    const pending = command(1_000);
    let latest = pending.originalTimer;
    for (const phase of success ? ['prepared', 'accepted', 'awaiting_evidence'] : ['accepted', 'prepared', 'accepted']) {
      await vi.advanceTimersByTimeAsync(400);
      latest = renew(pending, phase);
    }
    await vi.advanceTimersByTimeAsync(9_999);
    expect(pending.outcomes).toEqual([]);
    if (success) {
      ingress(pending, 'confirmed');
      expect(await pending.promise).toStrictEqual({ type: 'command_result', request_id: pending.key,
        action: 'tab_reorder', ok: true, phase: 'confirmed', data: { receipt: { action_id: actionId, phase: 'confirmed' } } });
    } else {
      pending.socket.message({ type: 'command_result', request_id: pending.key, ok: false,
        phase: 'not_started', error: 'first final refusal', data: { exact: 34 } });
      const error = await pending.promise;
      expect(error.constructor).toBe(CommandError);
      expect(error.message).toBe('first final refusal');
      expect(error.data).toStrictEqual({ exact: 34, not_started: true });
      expect(pending.reject).toHaveBeenCalledExactlyOnceWith(error);
    }
    settled(pending, success ? 'resolve' : 'reject');
    const cleared = timers.clear.mock.calls.findIndex(call => call[0] === latest);
    expect(timers.clear.mock.calls.filter(call => call[0] === latest)).toHaveLength(1);
    expect(timers.clear.mock.invocationCallOrder[cleared]).toBeLessThan(pending.cleanup.mock.invocationCallOrder[0]);
    expect(pending.outcomes).toEqual([success ? 'resolved' : 'rejected']);
    expect(timers.toast.mock.calls).toEqual(Array.from({ length: success ? 1 : 2 },
      () => ['Command accepted; waiting for agent…']));
    await noLaterOwnership(pending);
  });

  it('agent_start legacy acceptance leaves original/replacement timers alone while a prepared receipt renews', async () => {
    const pending = command(undefined, 'agent_start');
    await vi.advanceTimersByTimeAsync(1_000);
    ingress(pending, 'accepted');
    expect(pending.entry.timer).toBe(pending.originalTimer);
    expect(timers.clear).not.toHaveBeenCalledWith(pending.originalTimer);
    expect(timers.schedule.mock.calls.filter(call => call[1] === 10_000)).toHaveLength(0);
    expect(timers.toast).toHaveBeenLastCalledWith('Start accepted; waiting for confirmation…');
    await vi.advanceTimersByTimeAsync(1_000);
    const latest = renew(pending, 'prepared');
    await vi.advanceTimersByTimeAsync(1_000);
    ingress(pending, 'accepted');
    expect(pending.entry.timer).toBe(latest);
    expect(timers.clear).not.toHaveBeenCalledWith(latest);
    expect(timers.schedule.mock.calls.filter(call => call[1] === 10_000)).toHaveLength(1);
    expect(timers.toast.mock.calls).toEqual([
      ['Start accepted; waiting for confirmation…'], ['Start accepted; waiting for confirmation…'],
    ]);
    await vi.advanceTimersByTimeAsync(8_999); // 9999ms from prepared, not from the second legacy acceptance.
    expect(pending.outcomes).toEqual([]);
    await vi.advanceTimersByTimeAsync(1);
    await timedOut(pending);
    expect(timers.clear).not.toHaveBeenCalledWith(latest);
    await noLaterOwnership(pending);
  });
});

describe('pending registration through genuine public consumers', () => {
  type Pending = {
    relayId: string; action?: string; actionId?: string; responseType?: string;
    timer: ReturnType<typeof setTimeout>; cleanup: () => void;
    resolve: (value: any) => void; reject: (error: any) => void;
  };
  const maps = relayStore as unknown as {
    pendingRequests: Map<string, Pending>; pendingUploads: Map<string, Pending>;
  };
  function instrumentation() {
    return {
      schedule: vi.spyOn(globalThis, 'setTimeout'),
      clear: vi.spyOn(globalThis, 'clearTimeout'),
      add: vi.spyOn(AbortSignal.prototype, 'addEventListener'),
      remove: vi.spyOn(AbortSignal.prototype, 'removeEventListener'),
    };
  }
  let calls: ReturnType<typeof instrumentation>;

  beforeEach(() => {
    vi.useFakeTimers();
    transportHijack.current = null;
    MockWebSocket.instances = [];
    vi.stubGlobal('WebSocket', MockWebSocket);
    localStorage.clear();
    sessionStorage.clear();
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({ label: 'Registration owner', url: 'ws://127.0.0.1:8871', token: '' });
    relayStore.addRelay({ label: 'Registration other', url: 'ws://127.0.0.1:8872', token: '' });
    for (const socket of MockWebSocket.instances.slice(-2)) {
      socket.open();
      socket.message({ type: 'push_config', protocol: 3, capabilities: [], inventory: { state: 'ready' } });
    }
    calls = instrumentation();
  });

  afterEach(() => {
    vi.restoreAllMocks(); // Remove observers before real store teardown.
    transportHijack.current = null;
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  function endpoint(port = '8871') {
    const relay = get(relayStore.relayConfigs).find(row => new URL(row.url).port === port)!;
    const socket = MockWebSocket.instances.filter(row => new URL(row.url).port === port).at(-1)!;
    return { relay, socket, transport: relayStore.connection(relay.id)!.transport! };
  }
  function payload() {
    return { type: 'tab_reorder', pane_id: 'w1:p1', insert_index: 2,
      ...exactWireScope('w1:p1', endpoint().relay.id), intent: { action_id: 'registration_action_035' } };
  }
  function controller() {
    const { relay } = endpoint();
    const agent: Agent = { ...exactAgentFields(), relay_id: relay.id, relay_label: relay.label,
      raw_pane_id: 'w1:p1', pane_id: `${relay.id}::w1:p1` };
    const value = relayStore.attachmentController(agent);
    value.select([new File(['png'], 'shot.png', { type: 'image/png' })]);
    return value;
  }
  function first<T>(promise: Promise<T>) {
    const outcomes: string[] = [];
    const settled = promise.then(
      value => { outcomes.push('resolved'); return { status: 'resolved' as const, value }; },
      error => { outcomes.push('rejected'); return { status: 'rejected' as const, error }; },
    );
    return { settled, outcomes };
  }
  function capture(map: Map<string, Pending>) {
    function methods(entry: Pending) {
      return { cleanup: vi.spyOn(entry, 'cleanup'), resolve: vi.spyOn(entry, 'resolve'), reject: vi.spyOn(entry, 'reject') };
    }
    const rows: Array<{ key: string; entry: Pending; timer: Pending['timer'] } & ReturnType<typeof methods>> = [];
    const set = vi.spyOn(map, 'set').mockImplementation((key, entry) => {
      // Capture the actual record before a refused send can remove it.
      rows.push({ key, entry, timer: entry.timer, ...methods(entry) });
      return Map.prototype.set.call(map, key, entry);
    });
    const deletion = vi.spyOn(map, 'delete');
    return { rows, set, deletion, map };
  }
  type Capture = ReturnType<typeof capture>;
  function registered(observed: Capture, signal?: AbortSignal, duration = 60_000) {
    const row = observed.rows.at(-1)!;
    expect(row).toBeDefined();
    expect(observed.map.get(row.key)).toBe(row.entry);
    const scheduled = calls.schedule.mock.results.findIndex(result => result.value === row.timer);
    expect(scheduled).toBeGreaterThanOrEqual(0);
    expect(calls.schedule.mock.calls[scheduled]).toEqual([expect.any(Function), duration]);
    expect(row.entry.timer).toBe(row.timer);
    const insertion = observed.set.mock.calls.findIndex(call => call[0] === row.key);
    expect(calls.schedule.mock.invocationCallOrder[scheduled]).toBeLessThan(observed.set.mock.invocationCallOrder[insertion]);
    if (signal) {
      const registration = calls.add.mock.contexts.findIndex(context => context === signal);
      expect(registration).toBeGreaterThanOrEqual(0);
      expect(calls.add.mock.calls[registration]).toEqual(['abort', expect.any(Function), { once: true }]);
      expect(calls.add.mock.contexts.filter(context => context === signal)).toHaveLength(1);
      expect(calls.schedule.mock.invocationCallOrder[scheduled]).toBeLessThan(calls.add.mock.invocationCallOrder[registration]);
      expect(calls.add.mock.invocationCallOrder[registration]).toBeLessThan(observed.set.mock.invocationCallOrder[insertion]);
    }
    return row;
  }
  function signalForLastRegistration() {
    const index = calls.add.mock.calls.findLastIndex(call => (call[2] as AddEventListenerOptions | undefined)?.once === true);
    const signal = calls.add.mock.contexts[index] as AbortSignal;
    expect(signal).toBeInstanceOf(AbortSignal);
    return signal;
  }
  function removal(signal: AbortSignal) {
    const added = calls.add.mock.contexts.findIndex(context => context === signal);
    const listener = calls.add.mock.calls[added][1];
    const indexes = calls.remove.mock.contexts.flatMap((context, index) =>
      context === signal && calls.remove.mock.calls[index][1] === listener ? [index] : []);
    expect(indexes).toHaveLength(1);
    expect(calls.remove.mock.calls[indexes[0]]).toEqual(['abort', listener]);
    return indexes[0];
  }
  function reply(row: Capture['rows'][number], upload: boolean, socket = endpoint().socket) {
    socket.message(upload
      ? { type: 'upload_begin_result', request_id: row.key, result: beginResult() }
      : { type: 'command_result', request_id: row.key, ok: true, phase: 'confirmed' });
  }
  function beginResult() {
    return { upload_id: 'registration-upload', chunk_bytes: 3, expires_at: new Date(Date.now() + 60_000).toISOString(),
      limits: { max_files: 8, max_file_bytes: 20 * 1024 * 1024, max_batch_bytes: 50 * 1024 * 1024 } };
  }
  function frames(type: string) {
    return endpoint().socket.sent.map(body => JSON.parse(body)).filter(frame => frame.type === type);
  }
  async function noLaterOwnership(observed: Capture, upload: boolean) {
    const row = observed.rows[0];
    const before = [row.cleanup.mock.calls.length, row.resolve.mock.calls.length, row.reject.mock.calls.length];
    reply(row, upload);
    endpoint().socket.message({ type: upload ? 'upload_begin_result' : 'command_result', request_id: row.key,
      ok: false, error: upload ? { code: 'attachment_upload_busy' } : 'late refusal' });
    await vi.advanceTimersByTimeAsync(60_001);
    expect(observed.map.has(row.key)).toBe(false);
    expect([row.cleanup.mock.calls.length, row.resolve.mock.calls.length, row.reject.mock.calls.length]).toEqual(before);
    if (upload) {
      expect(frames('upload_chunk')).toEqual([]);
      expect(frames('upload_finish')).toEqual([]);
    }
  }

  it.each([
    { upload: false, mode: 'false' }, { upload: false, mode: 'throw' },
    { upload: true, mode: 'false' }, { upload: true, mode: 'throw' },
  ])('retains lexical first settlement and registered resources for upload=$upload send=$mode', async ({ upload, mode }) => {
    const { relay, socket, transport } = endpoint();
    const observed = capture(upload ? maps.pendingUploads : maps.pendingRequests);
    const type = upload ? 'upload_begin' : 'tab_reorder';
    const sentinel = new Error('registration socket sentinel');
    const abort = new AbortController();
    const batch = upload ? controller() : undefined;
    const attempted: Record<string, any>[] = [];
    if (mode === 'false') {
      const nativeSend = transport.send;
      vi.spyOn(transport, 'send').mockImplementation(function (this: RelayTransport, frame) {
        if (frame.type !== type) return nativeSend.call(this, frame);
        attempted.push(frame);
        return false; // Actual boolean transport boundary, not discarded socket.send return.
      });
    } else {
      const nativeSend = socket.send;
      vi.spyOn(socket, 'send').mockImplementation(function (this: MockWebSocket, body) {
        const frame = JSON.parse(body);
        if (frame.type !== type) return nativeSend.call(this, body);
        attempted.push(frame);
        throw sentinel;
      });
    }
    const publicOutcome = first<unknown>(batch ? batch.upload() : relayStore.sendCommand(relay.id, payload(), 60_000, false, abort.signal));
    const pendingBegin = batch && (batch as unknown as { pendingBegin?: Promise<Record<string, any>> }).pendingBegin;
    if (upload) expect(pendingBegin).toBeInstanceOf(Promise);
    const raw = pendingBegin ? first(pendingBegin) : publicOutcome;
    const row = observed.rows[0];
    expect(attempted).toHaveLength(1);
    expect(attempted[0]).toMatchObject({ type, request_id: row.key, protocol: 3, client_id: expect.any(String) });
    expect(row.entry.relayId).toBe(relay.id);
    const signal = upload ? signalForLastRegistration() : abort.signal;
    const outcome = await raw.settled;
    expect(outcome.status).toBe('rejected');
    if (outcome.status !== 'rejected') throw new Error('Expected real first rejection');
    expect(row.reject).not.toHaveBeenCalled(); // Executor/lexical reject, not record method.
    expect(row.resolve).not.toHaveBeenCalled();
    expect(row.cleanup).not.toHaveBeenCalled();
    if (mode === 'false') {
      const error = outcome.error;
      expect(error.constructor).toBe(CommandError);
      expect(error.message).toBe(upload ? 'Could not send attachment upload request.' : 'Could not send command to relay');
      expect(error.code).toBeUndefined();
      expect(error.data).toBeUndefined();
      expect(observed.map.has(row.key)).toBe(false);
      const cleared = calls.clear.mock.calls.findIndex(call => call[0] === row.timer);
      const removed = removal(signal);
      expect(calls.clear.mock.calls.filter(call => call[0] === row.timer)).toHaveLength(1);
      expect(calls.clear.mock.invocationCallOrder[cleared]).toBeLessThan(calls.remove.mock.invocationCallOrder[removed]);
      expect(calls.remove.mock.invocationCallOrder[removed]).toBeLessThan(observed.deletion.mock.invocationCallOrder[0]);
      expect(observed.deletion).toHaveBeenCalledExactlyOnceWith(row.key);
    } else {
      expect(outcome.error).toBe(sentinel);
      registered(observed, signal);
      expect(calls.clear).not.toHaveBeenCalledWith(row.timer);
      expect(calls.remove.mock.contexts).not.toContain(signal);
      reply(row, upload, endpoint('8872').socket);
      socket.message({ type: upload ? 'upload_begin_result' : 'command_result', request_id: 'wrong-registration', ok: true, result: {} });
      if (upload) socket.message({ type: 'upload_chunk_result', request_id: row.key, result: {} });
      expect(observed.map.get(row.key)).toBe(row.entry);
      expect(row.resolve).not.toHaveBeenCalled();
      expect(row.reject).not.toHaveBeenCalled();
      expect(row.cleanup).not.toHaveBeenCalled();
      reply(row, upload); // Real matching JSON invokes the still-owned record callback.
      expect(row.resolve).toHaveBeenCalledTimes(1);
      expect(row.reject).not.toHaveBeenCalled();
      expect(row.cleanup).toHaveBeenCalledTimes(1);
      expect(calls.clear).toHaveBeenCalledWith(row.timer);
      removal(signal);
      expect(await raw.settled).toBe(outcome);
    }
    const translated = await publicOutcome.settled;
    expect(translated.status).toBe('rejected');
    if (upload && translated.status === 'rejected') {
      expect(translated.error).toEqual({ code: 'attachment_upload_failed' });
      expect(batch!.snapshot()).toMatchObject({ uploading: false, items: [{ state: 'interrupted', issue: translated.error }] });
      await batch!.cancel();
    } else abort.abort();
    await noLaterOwnership(observed, upload);
    expect(raw.outcomes).toEqual(['rejected']);
    expect(publicOutcome.outcomes).toEqual(['rejected']);
  });

  it.each([true, false])('registers command listener then metadata then record before send: signal=%s', async withSignal => {
    const { relay, transport } = endpoint();
    const abort = new AbortController();
    const observed = capture(maps.pendingRequests);
    const metadata = vi.fn(() => ({ action_id: 'registration_action_035' }));
    const command = { ...payload(), get intent() { return metadata(); } };
    const send = vi.spyOn(transport, 'send');
    const additions = calls.add.mock.calls.length;
    const result = first(relayStore.sendCommand(relay.id, command, 60_000, false, withSignal ? abort.signal : undefined));
    const row = registered(observed, withSignal ? abort.signal : undefined);
    expect(row.entry).toMatchObject({ relayId: relay.id, action: 'tab_reorder', actionId: 'registration_action_035',
      resolve: expect.any(Function), reject: expect.any(Function), cleanup: expect.any(Function), timer: row.timer });
    expect(observed.set.mock.invocationCallOrder[0]).toBeLessThan(send.mock.invocationCallOrder[0]);
    expect(metadata.mock.invocationCallOrder[0]).toBeLessThan(observed.set.mock.invocationCallOrder[0]);
    if (withSignal) {
      const registration = calls.add.mock.contexts.indexOf(abort.signal);
      expect(calls.add.mock.invocationCallOrder[registration]).toBeLessThan(metadata.mock.invocationCallOrder[0]);
      abort.abort();
      const outcome = await result.settled;
      expect(outcome).toMatchObject({ status: 'rejected', error: { message: 'Conversation history request was cancelled.', code: 'request_cancelled' } });
      expect(row.reject).not.toHaveBeenCalled();
      expect(row.cleanup).not.toHaveBeenCalled();
      removal(abort.signal);
    } else {
      expect(calls.add.mock.calls).toHaveLength(additions);
      reply(row, false);
      expect(await result.settled).toMatchObject({ status: 'resolved', value: { ok: true, phase: 'confirmed' } });
      expect(row.resolve).toHaveBeenCalledTimes(1);
      expect(calls.remove).not.toHaveBeenCalled();
    }
    expect(calls.clear).toHaveBeenCalledWith(row.timer);
    await noLaterOwnership(observed, false);
  });

  it('registers typed public upload begin before send and native controller abort retains real public outcome', async () => {
    const observed = capture(maps.pendingUploads);
    const send = vi.spyOn(endpoint().transport, 'send');
    const batch = controller();
    const signalRead = vi.spyOn(AbortController.prototype, 'signal', 'get');
    const publicOutcome = first(batch.upload());
    const raw = first((batch as unknown as { pendingBegin: Promise<Record<string, any>> }).pendingBegin);
    const signal = signalForLastRegistration();
    const row = registered(observed, signal);
    expect(row.entry).toMatchObject({ relayId: endpoint().relay.id, responseType: 'upload_begin_result',
      timer: row.timer, resolve: expect.any(Function), reject: expect.any(Function), cleanup: expect.any(Function) });
    expect(observed.set.mock.invocationCallOrder[0]).toBeLessThan(send.mock.invocationCallOrder[0]);
    // Passively recover the genuine controller from its call-through native signal getter.
    const abort = signalRead.mock.contexts.find((_, index) => signalRead.mock.results[index].value === signal)! as AbortController;
    expect(abort).toBeInstanceOf(AbortController);
    abort.abort();
    expect(signal.aborted).toBe(true);
    expect(await raw.settled).toMatchObject({ status: 'rejected',
      error: { message: 'Attachment upload was interrupted.', code: 'attachment_upload_interrupted' } });
    expect(await publicOutcome.settled).toEqual({ status: 'rejected', error: { code: 'attachment_upload_interrupted' } });
    await batch.cancel();
    expect(row.reject).not.toHaveBeenCalled();
    expect(row.cleanup).not.toHaveBeenCalled();
    expect(calls.clear).toHaveBeenCalledWith(row.timer);
    removal(signal);
    await noLaterOwnership(observed, true);
  });

  it('public cancellation registers signal-less upload_cancel before send and waits for matching JSON', async () => {
    const batch = controller();
    const upload = first(batch.upload());
    const begin = frames('upload_begin')[0];
    endpoint().socket.message({ type: 'upload_begin_result', request_id: begin.request_id, result: beginResult() });
    await vi.waitFor(() => expect(frames('upload_chunk')).toHaveLength(1));
    const observed = capture(maps.pendingUploads);
    const send = vi.spyOn(endpoint().transport, 'send');
    const additions = calls.add.mock.calls.length;
    const cancellation = first(batch.cancel());
    expect(await upload.settled).toEqual({ status: 'resolved', value: [] });
    const row = registered(observed);
    expect(calls.add.mock.calls).toHaveLength(additions);
    expect(row.entry.responseType).toBe('upload_cancel_result');
    const frame = frames('upload_cancel')[0];
    expect(frame).toMatchObject({ request_id: row.key, upload_id: 'registration-upload' });
    expect(observed.set.mock.invocationCallOrder[0]).toBeLessThan(send.mock.invocationCallOrder.at(-1)!);
    endpoint('8872').socket.message({ type: 'upload_cancel_result', request_id: row.key, result: {} });
    endpoint().socket.message({ type: 'upload_begin_result', request_id: row.key, result: {} });
    expect(observed.map.get(row.key)).toBe(row.entry);
    expect(cancellation.outcomes).toEqual([]);
    const removals = calls.remove.mock.calls.length;
    endpoint().socket.message({ type: 'upload_cancel_result', request_id: row.key, result: {} });
    expect(await cancellation.settled).toEqual({ status: 'resolved', value: undefined });
    expect(row.resolve).toHaveBeenCalledExactlyOnceWith({});
    expect(row.cleanup).toHaveBeenCalledTimes(1);
    expect(calls.remove.mock.calls).toHaveLength(removals);
    expect(calls.clear).toHaveBeenCalledWith(row.timer);
    endpoint().socket.message({ type: 'upload_cancel_result', request_id: row.key, error: { code: 'attachment_upload_busy' } });
    await vi.advanceTimersByTimeAsync(60_001);
    expect(row.resolve).toHaveBeenCalledTimes(1);
    expect(row.reject).not.toHaveBeenCalled();
    expect(observed.map.has(row.key)).toBe(false);
  });

  it('pre-aborted command exits before request identity, resources, insertion and send', async () => {
    const abort = new AbortController();
    abort.abort();
    const id = vi.spyOn(crypto, 'randomUUID');
    const random = vi.spyOn(crypto, 'getRandomValues');
    const observed = capture(maps.pendingRequests);
    const send = vi.spyOn(endpoint().transport, 'send');
    const additions = calls.add.mock.calls.length; // Framework-owned test signal is outside this command.
    const result = first(relayStore.sendCommand(endpoint().relay.id, payload(), 60_000, false, abort.signal));
    expect(calls.add.mock.calls).toHaveLength(additions);
    expect(await result.settled).toMatchObject({ status: 'rejected',
      error: { message: 'Conversation history request was cancelled.', code: 'request_cancelled' } });
    for (const spy of [id, random, calls.schedule, observed.set, send]) expect(spy).not.toHaveBeenCalled();
    expect(calls.clear).not.toHaveBeenCalled();
    expect(calls.remove).not.toHaveBeenCalled();
    expect(observed.map.size).toBe(0);
  });

  it.each(['abort', 'throw'])('metadata getter %s retains post-listener pre-insertion partial progress', async mode => {
    const abort = new AbortController();
    const observed = capture(maps.pendingRequests);
    const send = vi.spyOn(endpoint().transport, 'send');
    const sentinel = new Error('registration metadata sentinel');
    const metadata = vi.fn(() => {
      expect(calls.add.mock.contexts).toContain(abort.signal);
      expect(observed.rows).toEqual([]);
      if (mode === 'throw') throw sentinel;
      abort.abort();
      return { action_id: 'registration_action_035' };
    });
    // Only the first metadata access is the pre-insertion seam; spread reads it again later.
    const command = { ...payload(), get intent() {
      return observed.rows.length ? { action_id: 'registration_action_035' } : metadata();
    } };
    const outcome = first(relayStore.sendCommand(endpoint().relay.id, command, 60_000, false, abort.signal));
    expect(calls.add.mock.invocationCallOrder[0]).toBeLessThan(metadata.mock.invocationCallOrder[0]);
    const timer = calls.schedule.mock.results[0].value;
    if (mode === 'abort') {
      const row = registered(observed, abort.signal);
      expect(abort.signal.aborted).toBe(true);
      expect(calls.clear).toHaveBeenCalledExactlyOnceWith(row.timer);
      expect(observed.deletion.mock.results[0].value).toBe(false);
      expect(calls.remove).not.toHaveBeenCalled(); // Command absent-entry abort returns before cleanup/reject.
      expect(send).toHaveBeenCalledTimes(1);
      expect(outcome.outcomes).toEqual([]);
      reply(row, false);
      expect(await outcome.settled).toMatchObject({ status: 'resolved', value: { ok: true } });
      expect(row.resolve).toHaveBeenCalledTimes(1);
      expect(row.reject).not.toHaveBeenCalled();
      removal(abort.signal);
      await noLaterOwnership(observed, false);
    } else {
      expect(await outcome.settled).toEqual({ status: 'rejected', error: sentinel });
      expect(observed.rows).toEqual([]);
      expect(send).not.toHaveBeenCalled();
      expect(calls.clear).not.toHaveBeenCalledWith(timer);
      expect(calls.remove).not.toHaveBeenCalled();
      expect(observed.map.size).toBe(0);
      // Explicit fixture cleanup of real pre-insertion resources, not production hardening.
      const listener = calls.add.mock.calls[0][1]!;
      abort.signal.removeEventListener('abort', listener);
      clearTimeout(timer);
      await vi.advanceTimersByTimeAsync(60_001);
      expect(outcome.outcomes).toEqual(['rejected']);
      expect(observed.map.size).toBe(0);
    }
  });
});

describe('explicit false-send release through genuine public consumers', () => {
  const nativeRemove = AbortSignal.prototype.removeEventListener;
  type Pending = {
    relayId: string; responseType?: string;
    timer: ReturnType<typeof setTimeout>; cleanup: () => void;
    resolve: (value: any) => void; reject: (error: any) => void;
  };
  const maps = relayStore as unknown as {
    pendingRequests: Map<string, Pending>; pendingUploads: Map<string, Pending>;
  };
  function instrumentation() {
    return {
      schedule: vi.spyOn(globalThis, 'setTimeout'),
      clear: vi.spyOn(globalThis, 'clearTimeout'),
      add: vi.spyOn(AbortSignal.prototype, 'addEventListener'),
      remove: vi.spyOn(AbortSignal.prototype, 'removeEventListener'),
    };
  }
  let calls: ReturnType<typeof instrumentation>;
  beforeEach(() => {
    vi.useFakeTimers();
    transportHijack.current = null;
    MockWebSocket.instances = [];
    vi.stubGlobal('WebSocket', MockWebSocket);
    localStorage.clear();
    sessionStorage.clear();
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    relayStore.addRelay({ label: 'False owner', url: 'ws://127.0.0.1:8871', token: '' });
    relayStore.addRelay({ label: 'False other', url: 'ws://127.0.0.1:8872', token: '' });
    for (const socket of MockWebSocket.instances.slice(-2)) {
      socket.open();
      socket.message({ type: 'push_config', protocol: 3, capabilities: [], inventory: { state: 'ready' } });
    }
    calls = instrumentation();
  });
  afterEach(() => {
    vi.restoreAllMocks(); // Disarm injections and observers before genuine store teardown.
    transportHijack.current = null;
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });
  function endpoint(port = '8871') {
    const relay = get(relayStore.relayConfigs).find(row => new URL(row.url).port === port)!;
    const socket = MockWebSocket.instances.filter(row => new URL(row.url).port === port).at(-1)!;
    return { relay, socket, transport: relayStore.connection(relay.id)!.transport! };
  }
  function payload() {
    return { type: 'tab_reorder', pane_id: 'w1:p1', insert_index: 2,
      ...exactWireScope('w1:p1', endpoint().relay.id), intent: { action_id: 'false_release_action_036' } };
  }
  function controller() {
    const { relay } = endpoint();
    const agent: Agent = { ...exactAgentFields(), relay_id: relay.id, relay_label: relay.label,
      raw_pane_id: 'w1:p1', pane_id: `${relay.id}::w1:p1` };
    const batch = relayStore.attachmentController(agent);
    batch.select([new File(['png'], 'shot.png', { type: 'image/png' })]);
    return batch;
  }
  function first<T>(promise: Promise<T>) {
    const outcomes: string[] = [];
    const settled = promise.then(
      value => { outcomes.push('resolved'); return { status: 'resolved' as const, value }; },
      error => { outcomes.push('rejected'); return { status: 'rejected' as const, error }; },
    );
    return { settled, outcomes };
  }
  function capture(map: Map<string, Pending>) {
    function methods(entry: Pending) {
      return { cleanup: vi.spyOn(entry, 'cleanup'), resolve: vi.spyOn(entry, 'resolve'), reject: vi.spyOn(entry, 'reject') };
    }
    const rows: Array<{ key: string; entry: Pending; timer: Pending['timer'] } & ReturnType<typeof methods>> = [];
    const set = vi.spyOn(map, 'set').mockImplementation((key, entry) => {
      rows.push({ key, entry, timer: entry.timer, ...methods(entry) });
      return Map.prototype.set.call(map, key, entry);
    });
    return { rows, set, deletion: vi.spyOn(map, 'delete'), map };
  }
  type Row = ReturnType<typeof capture>['rows'][number];
  function listenerFor(signal: AbortSignal) {
    const index = calls.add.mock.contexts.indexOf(signal);
    expect(index).toBeGreaterThanOrEqual(0);
    expect(calls.add.mock.calls[index]).toEqual(['abort', expect.any(Function), { once: true }]);
    expect(calls.add.mock.contexts.filter(context => context === signal)).toHaveLength(1);
    return calls.add.mock.calls[index][1]!;
  }
  function originalTimer(row: Row, duration: number) {
    const index = calls.schedule.mock.results.findIndex(result => result.value === row.timer);
    expect(index).toBeGreaterThanOrEqual(0);
    expect(calls.schedule.mock.calls[index]).toEqual([expect.any(Function), duration]);
  }
  function definitive(error: any) {
    expect(error.constructor).toBe(CommandError);
    expect(error.message).toBe('Could not send command to relay');
    expect(error.code).toBeUndefined();
    expect(error.data).toBeUndefined();
  }
  function beginResult() {
    return { upload_id: 'false-release-upload', chunk_bytes: 3, expires_at: new Date(Date.now() + 60_000).toISOString(),
      limits: { max_files: 8, max_file_bytes: 20 * 1024 * 1024, max_batch_bytes: 50 * 1024 * 1024 } };
  }
  function frames(type: string) {
    return endpoint().socket.sent.map(body => JSON.parse(body)).filter(frame => frame.type === type);
  }
  function terminal(row: Row, upload = false, socket = endpoint().socket) {
    socket.message(upload
      ? { type: 'upload_begin_result', request_id: row.key, result: beginResult() }
      : { type: 'command_result', request_id: row.key, ok: true, phase: 'confirmed' });
  }
  function wrongTerminals(row: Row, upload = false) {
    terminal(row, upload, endpoint('8872').socket);
    endpoint().socket.message({ type: upload ? 'upload_begin_result' : 'command_result', request_id: 'wrong-false-release', ok: true, result: {} });
    endpoint().socket.message({ type: upload ? 'upload_chunk_result' : 'upload_begin_result', request_id: row.key, result: {} });
    expect(row.cleanup).not.toHaveBeenCalled();
    expect(row.resolve).not.toHaveBeenCalled();
    expect(row.reject).not.toHaveBeenCalled();
  }
  async function late(row: Row, upload = false) {
    const before = [row.cleanup.mock.calls.length, row.resolve.mock.calls.length, row.reject.mock.calls.length];
    terminal(row, upload);
    endpoint().socket.message({ type: upload ? 'upload_begin_result' : 'command_result', request_id: row.key,
      ok: false, error: upload ? { code: 'attachment_upload_busy' } : 'late false-release refusal' });
    await vi.advanceTimersByTimeAsync(60_001);
    expect([row.cleanup.mock.calls.length, row.resolve.mock.calls.length, row.reject.mock.calls.length]).toEqual(before);
    expect((upload ? maps.pendingUploads : maps.pendingRequests).has(row.key)).toBe(false);
  }

  it('signal-less command refusal clears its original timer before deletion and rejects definitively without a phantom listener', async () => {
    const { relay, transport } = endpoint();
    const observed = capture(maps.pendingRequests);
    const attempted: Record<string, any>[] = [];
    const send = transport.send;
    vi.spyOn(transport, 'send').mockImplementation(function (this: RelayTransport, frame) {
      if (frame.type !== 'tab_reorder') return send.call(this, frame);
      attempted.push(frame);
      expect(observed.map.get(String(frame.request_id))).toBe(observed.rows[0].entry);
      return false;
    });
    const additions = calls.add.mock.calls.length;
    const outcome = first(relayStore.sendCommand(relay.id, payload()));
    const row = observed.rows[0];
    expect(attempted).toEqual([expect.objectContaining({ type: 'tab_reorder', request_id: row.key, protocol: 3, client_id: expect.any(String) })]);
    originalTimer(row, 15_000);
    expect(calls.add.mock.calls).toHaveLength(additions);
    expect(calls.remove).not.toHaveBeenCalled();
    expect(calls.clear).toHaveBeenCalledExactlyOnceWith(row.timer);
    expect(observed.deletion).toHaveBeenCalledExactlyOnceWith(row.key);
    expect(calls.clear.mock.invocationCallOrder[0]).toBeLessThan(observed.deletion.mock.invocationCallOrder[0]);
    expect(observed.map.has(row.key)).toBe(false);
    const firstOutcome = await outcome.settled;
    if (firstOutcome.status !== 'rejected') throw new Error('Expected actual executor rejection');
    definitive(firstOutcome.error);
    wrongTerminals(row);
    await late(row);
    expect(outcome.outcomes).toEqual(['rejected']);
    expect(await outcome.settled).toBe(firstOutcome);
  });

  it('signal-less public upload_cancel refusal releases the actual cancel record and translates failure without continued upload', async () => {
    const batch = controller();
    const observed = capture(maps.pendingUploads);
    // Passive call-through observation of the genuine chunk Promise, never a private request invocation.
    const callbacks = (batch as unknown as { callbacks: import('$lib/attachments').AttachmentUploadCallbacks }).callbacks;
    const chunk = vi.spyOn(callbacks, 'chunk');
    const upload = first(batch.upload());
    const begin = observed.rows[0];
    terminal(begin, true);
    await vi.waitFor(() => expect(frames('upload_chunk')).toHaveLength(1));
    expect(chunk).toHaveBeenCalledTimes(1);
    const rawChunk = first(chunk.mock.results[0].value);
    const chunkRow = observed.rows[1];
    expect(frames('upload_chunk')[0]).toMatchObject({ request_id: chunkRow.key, upload_id: 'false-release-upload', sequence: 0, data: 'cG5n' });
    const { transport } = endpoint();
    const send = transport.send;
    const attempted: Record<string, any>[] = [];
    vi.spyOn(transport, 'send').mockImplementation(function (this: RelayTransport, frame) {
      if (frame.type !== 'upload_cancel') return send.call(this, frame);
      attempted.push(frame);
      expect(observed.map.get(String(frame.request_id))).toBe(observed.rows[2].entry);
      return false;
    });
    const additions = calls.add.mock.calls.length;
    const cancellation = first(batch.cancel());
    const row = observed.rows[2];
    expect(attempted).toEqual([expect.objectContaining({ type: 'upload_cancel', request_id: row.key,
      protocol: 3, client_id: expect.any(String), upload_id: 'false-release-upload' })]);
    expect(row.entry.responseType).toBe('upload_cancel_result');
    originalTimer(row, 60_000);
    expect(calls.add.mock.calls).toHaveLength(additions);
    expect(calls.clear.mock.calls.filter(call => call[0] === row.timer)).toHaveLength(1);
    const clearIndex = calls.clear.mock.calls.findIndex(call => call[0] === row.timer);
    const deleteIndex = observed.deletion.mock.calls.findIndex(call => call[0] === row.key);
    expect(deleteIndex).toBeGreaterThanOrEqual(0);
    expect(calls.clear.mock.invocationCallOrder[clearIndex]).toBeLessThan(observed.deletion.mock.invocationCallOrder[deleteIndex]);
    expect(observed.map.has(row.key)).toBe(false);
    expect(await cancellation.settled).toEqual({ status: 'rejected', error: { code: 'attachment_cancel_failed' } });
    expect(await rawChunk.settled).toMatchObject({ status: 'rejected', error: { code: 'attachment_upload_interrupted' } });
    expect(await upload.settled).toEqual({ status: 'resolved', value: [] });
    expect(batch.snapshot()).toMatchObject({ uploading: false, items: [] });
    expect(row.cleanup).not.toHaveBeenCalled();
    expect(row.resolve).not.toHaveBeenCalled();
    expect(row.reject).not.toHaveBeenCalled(); // Public cancellation does not expose the raw cancel error.
    const removals = calls.remove.mock.calls.length;
    for (const socket of [endpoint('8872').socket, endpoint().socket]) {
      socket.message({ type: 'upload_cancel_result', request_id: row.key, result: {} });
      socket.message({ type: 'upload_begin_result', request_id: row.key, result: beginResult() });
      socket.message({ type: 'upload_cancel_result', request_id: 'wrong-cancel', result: {} });
    }
    endpoint().socket.message({ type: 'upload_chunk_result', request_id: chunkRow.key,
      result: { file_index: 0, next_sequence: 1, received_bytes: 3 } });
    await vi.advanceTimersByTimeAsync(60_001);
    expect(observed.map.size).toBe(0);
    expect(row.cleanup).not.toHaveBeenCalled();
    expect(row.resolve).not.toHaveBeenCalled();
    expect(row.reject).not.toHaveBeenCalled();
    expect(calls.remove.mock.calls).toHaveLength(removals);
    expect(frames('upload_chunk')).toHaveLength(1);
    expect(frames('upload_finish')).toEqual([]);
    expect(cancellation.outcomes).toEqual(['rejected']);
    expect(rawChunk.outcomes).toEqual(['rejected']);
    expect(upload.outcomes).toEqual(['resolved']);
  });

  it('controlled synchronous prepared JSON renewal preserves lexical original release and later raw replacement timeout', async () => {
    const { relay, transport, socket } = endpoint();
    const abort = new AbortController();
    const observed = capture(maps.pendingRequests);
    const send = transport.send;
    const attempted: Record<string, any>[] = [];
    vi.spyOn(transport, 'send').mockImplementation(function (this: RelayTransport, frame) {
      if (frame.type !== 'tab_reorder') return send.call(this, frame);
      attempted.push(frame);
      const row = observed.rows[0];
      expect(observed.map.get(row.key)).toBe(row.entry);
      const receipt = { action_id: 'false_release_action_036', phase: 'prepared' };
      endpoint('8872').socket.message({ type: 'action_receipt', request_id: row.key, receipt });
      socket.message({ type: 'action_receipt', request_id: 'missing', receipt: { ...receipt, action_id: 'unrelated_action_036' } });
      socket.message({ type: 'upload_begin_result', request_id: row.key, result: {} });
      expect(row.entry.timer).toBe(row.timer);
      expect(calls.clear).not.toHaveBeenCalledWith(row.timer);
      socket.message({ type: 'action_receipt', request_id: row.key, receipt });
      expect(row.entry.timer).not.toBe(row.timer);
      return false; // Deliberate synchronous reentrancy, not natural WebSocket scheduling evidence.
    });
    const result = first(relayStore.sendCommand(relay.id, payload(), 60_000, false, abort.signal));
    const row = observed.rows[0];
    const replacement = row.entry.timer;
    const listener = listenerFor(abort.signal);
    originalTimer(row, 60_000);
    expect(attempted).toHaveLength(1);
    expect(attempted[0]).toMatchObject({ type: 'tab_reorder', request_id: row.key, protocol: 3, client_id: expect.any(String) });
    expect(calls.clear.mock.calls.filter(call => call[0] === row.timer)).toHaveLength(2);
    expect(calls.clear).not.toHaveBeenCalledWith(replacement);
    const scheduled = calls.schedule.mock.results.findIndex(value => value.value === replacement);
    expect(calls.schedule.mock.calls[scheduled]).toEqual([expect.any(Function), 10_000]);
    expect(calls.remove).toHaveBeenCalledExactlyOnceWith('abort', listener);
    const lastClear = calls.clear.mock.calls.findLastIndex(call => call[0] === row.timer);
    expect(calls.clear.mock.invocationCallOrder[lastClear]).toBeLessThan(calls.remove.mock.invocationCallOrder[0]);
    expect(calls.remove.mock.invocationCallOrder[0]).toBeLessThan(observed.deletion.mock.invocationCallOrder[0]);
    expect(observed.deletion).toHaveBeenCalledExactlyOnceWith(row.key);
    expect(observed.map.has(row.key)).toBe(false);
    const firstOutcome = await result.settled;
    if (firstOutcome.status !== 'rejected') throw new Error('Expected lexical rejection');
    definitive(firstOutcome.error);
    expect(row.cleanup).not.toHaveBeenCalled();
    expect(row.reject).not.toHaveBeenCalled();
    terminal(row);
    await vi.advanceTimersByTimeAsync(9_999);
    expect(row.cleanup).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(row.cleanup).toHaveBeenCalledTimes(1);
    expect(row.reject).toHaveBeenCalledTimes(1);
    expect(row.resolve).not.toHaveBeenCalled();
    const laterError = row.reject.mock.calls[0][0];
    expect(laterError.constructor).toBe(CommandError);
    expect(laterError.message).toBe('Relay confirmation timed out');
    expect(laterError.data).toEqual({ dispatched_unknown: true });
    expect(laterError).not.toBe(firstOutcome.error);
    expect(calls.clear).not.toHaveBeenCalledWith(replacement);
    expect(calls.remove.mock.calls).toEqual([['abort', listener], ['abort', listener]]);
    expect(calls.remove.mock.contexts).toEqual([abort.signal, abort.signal]);
    expect(await result.settled).toBe(firstOutcome);
    expect(result.outcomes).toEqual(['rejected']);
    await late(row);
  });

  it('command false cleanup removes the exact native listener then throws, retaining entry until matching terminal JSON', async () => {
    const { relay, transport } = endpoint();
    const abort = new AbortController();
    const observed = capture(maps.pendingRequests);
    const sentinel = new Error('command false cleanup acted then threw');
    let armed = true;
    let listener: EventListenerOrEventListenerObject;
    calls.remove.mockImplementation(function (this: AbortSignal, type, callback, options) {
      nativeRemove.call(this, type, callback, options);
      if (armed && this === abort.signal && callback === listener) throw sentinel;
    });
    const send = transport.send;
    const attempted: Record<string, any>[] = [];
    vi.spyOn(transport, 'send').mockImplementation(function (this: RelayTransport, frame) {
      if (frame.type !== 'tab_reorder') return send.call(this, frame);
      attempted.push(frame);
      listener = listenerFor(abort.signal);
      return false;
    });
    const result = first(relayStore.sendCommand(relay.id, payload(), 60_000, false, abort.signal));
    const row = observed.rows[0];
    originalTimer(row, 60_000);
    expect(attempted).toHaveLength(1);
    expect(attempted[0]).toMatchObject({ type: 'tab_reorder', request_id: row.key, protocol: 3, client_id: expect.any(String) });
    const firstOutcome = await result.settled;
    expect(firstOutcome).toEqual({ status: 'rejected', error: sentinel });
    if (firstOutcome.status !== 'rejected') throw new Error('Expected executor exception');
    expect(firstOutcome.error).toBe(sentinel);
    expect(calls.clear).toHaveBeenCalledExactlyOnceWith(row.timer);
    expect(calls.remove).toHaveBeenCalledExactlyOnceWith('abort', listener!);
    expect(calls.remove.mock.contexts[0]).toBe(abort.signal);
    expect(calls.clear.mock.invocationCallOrder[0]).toBeLessThan(calls.remove.mock.invocationCallOrder[0]);
    expect(observed.map.get(row.key)).toBe(row.entry);
    expect(observed.deletion).not.toHaveBeenCalled();
    armed = false;
    abort.abort(); // Native removal already acted: abort no longer deletes this retained entry.
    expect(observed.map.get(row.key)).toBe(row.entry);
    wrongTerminals(row);
    terminal(row);
    expect(row.cleanup).toHaveBeenCalledTimes(1);
    expect(row.resolve).toHaveBeenCalledTimes(1);
    expect(row.reject).not.toHaveBeenCalled();
    expect(observed.map.has(row.key)).toBe(false);
    expect(calls.clear.mock.calls).toEqual([[row.timer], [row.timer]]);
    expect(calls.remove.mock.calls).toEqual([['abort', listener!], ['abort', listener!]]);
    expect(await result.settled).toBe(firstOutcome);
    await late(row);
    expect(result.outcomes).toEqual(['rejected']);
  });

  it('public upload begin false cleanup throws raw sentinel but translates public failure and later releases through matching JSON', async () => {
    const batch = controller();
    const observed = capture(maps.pendingUploads);
    const signalRead = vi.spyOn(AbortController.prototype, 'signal', 'get');
    const sentinel = new Error('upload false cleanup acted then threw');
    let armed = true;
    let signal: AbortSignal;
    let listener: EventListenerOrEventListenerObject;
    calls.remove.mockImplementation(function (this: AbortSignal, type, callback, options) {
      nativeRemove.call(this, type, callback, options);
      if (armed && this === signal && callback === listener) throw sentinel;
    });
    const { transport } = endpoint();
    const send = transport.send;
    const attempted: Record<string, any>[] = [];
    vi.spyOn(transport, 'send').mockImplementation(function (this: RelayTransport, frame) {
      if (frame.type !== 'upload_begin') return send.call(this, frame);
      attempted.push(frame);
      signal = calls.add.mock.contexts.at(-1)! as AbortSignal;
      listener = listenerFor(signal);
      expect(signal).toBeInstanceOf(AbortSignal);
      return false;
    });
    const publicOutcome = first(batch.upload());
    const pendingBegin = (batch as unknown as { pendingBegin: Promise<Record<string, any>> }).pendingBegin;
    expect(pendingBegin).toBeInstanceOf(Promise); // Observed immediately, before the first await continuation.
    const raw = first(pendingBegin);
    const row = observed.rows[0];
    const nativeController = signalRead.mock.contexts.find((_, index) => signalRead.mock.results[index].value === signal!)! as AbortController;
    expect(nativeController).toBeInstanceOf(AbortController);
    expect(nativeController.signal).toBe(signal!);
    expect(attempted).toHaveLength(1);
    expect(attempted[0]).toMatchObject({ type: 'upload_begin', request_id: row.key, protocol: 3, client_id: expect.any(String) });
    expect(row.entry.responseType).toBe('upload_begin_result');
    originalTimer(row, 60_000);
    const firstOutcome = await raw.settled;
    expect(firstOutcome).toEqual({ status: 'rejected', error: sentinel });
    if (firstOutcome.status !== 'rejected') throw new Error('Expected raw executor exception');
    expect(firstOutcome.error).toBe(sentinel);
    expect(await publicOutcome.settled).toEqual({ status: 'rejected', error: { code: 'attachment_upload_failed' } });
    expect(batch.snapshot()).toMatchObject({ uploading: false, items: [{ state: 'interrupted', issue: { code: 'attachment_upload_failed' } }] });
    expect(calls.clear).toHaveBeenCalledExactlyOnceWith(row.timer);
    expect(calls.remove).toHaveBeenCalledExactlyOnceWith('abort', listener!);
    expect(calls.remove.mock.contexts[0]).toBe(signal!);
    expect(calls.clear.mock.invocationCallOrder[0]).toBeLessThan(calls.remove.mock.invocationCallOrder[0]);
    expect(observed.map.get(row.key)).toBe(row.entry);
    expect(observed.deletion).not.toHaveBeenCalled();
    armed = false;
    wrongTerminals(row, true);
    terminal(row, true);
    expect(row.cleanup).toHaveBeenCalledTimes(1);
    expect(row.resolve).toHaveBeenCalledExactlyOnceWith(beginResult());
    expect(row.reject).not.toHaveBeenCalled();
    expect(observed.map.has(row.key)).toBe(false);
    expect(calls.clear.mock.calls).toEqual([[row.timer], [row.timer]]);
    expect(calls.remove.mock.calls).toEqual([['abort', listener!], ['abort', listener!]]);
    expect(await raw.settled).toBe(firstOutcome);
    await batch.cancel();
    await late(row, true);
    expect(frames('upload_chunk')).toEqual([]);
    expect(frames('upload_finish')).toEqual([]);
    expect(raw.outcomes).toEqual(['rejected']);
    expect(publicOutcome.outcomes).toEqual(['rejected']);
  });
});

describe('three-field safe-integer wire consumers on decoded JSON', () => {
  const wireScalars: Array<{ name: string; fields: Record<string, unknown>; expected: number | undefined }> = [
    { name: "missing", fields: {}, expected: undefined },
    { name: "null", fields: {"server_protocol":null,"generation":null,"version":null}, expected: 0 },
    { name: "false", fields: {"server_protocol":false,"generation":false,"version":false}, expected: 0 },
    { name: "true", fields: {"server_protocol":true,"generation":true,"version":true}, expected: 1 },
    { name: "empty-string", fields: {"server_protocol":"","generation":"","version":""}, expected: 0 },
    { name: "whitespace", fields: {"server_protocol":" \t ","generation":" \t ","version":" \t "}, expected: 0 },
    { name: "padded-decimal", fields: {"server_protocol":" 42 ","generation":" 42 ","version":" 42 "}, expected: 42 },
    { name: "negative-zero-string", fields: {"server_protocol":"-0","generation":"-0","version":"-0"}, expected: -0 },
    { name: "hex-string", fields: {"server_protocol":"0x10","generation":"0x10","version":"0x10"}, expected: 16 },
    { name: "exponent-string", fields: {"server_protocol":"1e2","generation":"1e2","version":"1e2"}, expected: 100 },
    { name: "nan-string", fields: {"server_protocol":"NaN","generation":"NaN","version":"NaN"}, expected: undefined },
    { name: "positive-infinity-string", fields: {"server_protocol":"Infinity","generation":"Infinity","version":"Infinity"}, expected: undefined },
    { name: "negative-infinity-string", fields: {"server_protocol":"-Infinity","generation":"-Infinity","version":"-Infinity"}, expected: undefined },
    { name: "negative-integer", fields: {"server_protocol":-5,"generation":-5,"version":-5}, expected: -5 },
    { name: "zero", fields: {"server_protocol":0,"generation":0,"version":0}, expected: 0 },
    { name: "positive-integer", fields: {"server_protocol":7,"generation":7,"version":7}, expected: 7 },
    { name: "fraction", fields: {"server_protocol":1.5,"generation":1.5,"version":1.5}, expected: undefined },
    { name: "max-safe", fields: {"server_protocol":9007199254740991,"generation":9007199254740991,"version":9007199254740991}, expected: 9007199254740991 },
    { name: "min-safe", fields: {"server_protocol":-9007199254740991,"generation":-9007199254740991,"version":-9007199254740991}, expected: -9007199254740991 },
    { name: "above-safe", fields: {"server_protocol":9007199254740992,"generation":9007199254740992,"version":9007199254740992}, expected: undefined },
    { name: "below-safe", fields: {"server_protocol":-9007199254740992,"generation":-9007199254740992,"version":-9007199254740992}, expected: undefined },
    { name: "empty-array", fields: {"server_protocol":[],"generation":[],"version":[]}, expected: 0 },
    { name: "null-array", fields: {"server_protocol":[null],"generation":[null],"version":[null]}, expected: 0 },
    { name: "numeric-array", fields: {"server_protocol":[2],"generation":[2],"version":[2]}, expected: 2 },
    { name: "string-array", fields: {"server_protocol":["3"],"generation":["3"],"version":["3"]}, expected: 3 },
    { name: "multiple-array", fields: {"server_protocol":[1,2],"generation":[1,2],"version":[1,2]}, expected: undefined },
    { name: "ordinary-record", fields: {"server_protocol":{},"generation":{},"version":{}}, expected: undefined },
  ];
  type Page = Awaited<ReturnType<typeof relayStore.getConversationHistory>>;

  beforeEach(() => {
    vi.useFakeTimers();
    transportHijack.current = null;
    MockWebSocket.instances = [];
    vi.stubGlobal('WebSocket', MockWebSocket);
    localStorage.clear();
    sessionStorage.clear();
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    // Adding a relay reconnects all: establish both before starting pending work.
    relayStore.addRelay({ label: 'Wire owner', url: 'ws://127.0.0.1:8871', token: '' });
    relayStore.addRelay({ label: 'Wire other', url: 'ws://127.0.0.1:8872', token: '' });
    for (const socket of MockWebSocket.instances.slice(-2)) {
      socket.open();
      socket.message({ type: 'push_config', protocol: 3, capabilities: [], inventory: { state: 'ready' } });
      socket.message({ type: 'agents', agents: [
        { ...exactAgentFields(), pane_id: 'w1:p1', name: 'Wire agent', agent: 'codex', status: 'idle' },
      ] });
    }
  });

  afterEach(() => {
    vi.restoreAllMocks();
    transportHijack.current = null;
    relayStore.destroy();
    relayStore.setHidden(false);
    relayStore.relayConfigs.set([]);
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  function endpoint(port = '8871') {
    const relay = get(relayStore.relayConfigs).find(row => new URL(row.url).port === port)!;
    const socket = MockWebSocket.instances.filter(row => new URL(row.url).port === port).at(-1)!;
    return { relay, socket };
  }
  function status(fields: Record<string, unknown>, marker: string) {
    return {
      installed_client_version: '0.9.0', server_version: marker, server_protocol_known: true,
      endpoint_protocol_generation: 2,
      features: { 'pane.read': { state: 'supported', reason: marker } },
      ...fields,
    };
  }
  function snapshot(fields: Record<string, unknown>, marker: string, port = '8871') {
    endpoint(port).socket.message({
      type: 'push_config', protocol: 3, capabilities: ['wire-snapshot'], inventory: { state: 'ready' },
      herdr_status: status(fields, marker),
    });
  }
  function live(fields: Record<string, unknown>, marker: string, port = '8871') {
    endpoint(port).socket.message({
      type: 'herdr_status', status: status(fields, marker), capabilities: ['wire-live', marker],
    });
  }
  function current(port = '8871') {
    return relayStore.connection(endpoint(port).relay.id)!;
  }
  function assertStatus(fields: { server_protocol: number; generation: number }, marker: string, port = '8871') {
    expect(current(port).herdrStatus).toEqual({
      installed_client_version: '0.9.0', server_version: marker,
      server_protocol: fields.server_protocol, generation: fields.generation,
      server_protocol_known: true, endpoint_protocol_generation: 2,
      features: { 'pane.read': { state: 'supported', reason: marker } },
    });
    expect(Object.is(current(port).herdrStatus.server_protocol, -0)).toBe(Object.is(fields.server_protocol, -0));
    expect(Object.is(current(port).herdrStatus.generation, -0)).toBe(Object.is(fields.generation, -0));
  }
  function plan(fields: Record<string, unknown> = {}) {
    return {
      available: true, session_id: 'wire-session', updated_at: 'wire-time',
      phases: [{ name: 'Wire phase', tasks: [{ id: 'wire-task', content: 'Preserve this task', status: 'in_progress' }] }],
      truncated: false, ...fields,
    };
  }
  function page(marker: string, extra: Record<string, unknown> = {}) {
    return {
      available: true, has_more: false, state: 'ready', mode: 'recent',
      entries: [{ id: 'wire-entry', role: 'assistant', text: marker }], ...extra,
    };
  }
  function history(signal?: AbortSignal, port = '8871') {
    const { relay, socket } = endpoint(port);
    const agent = get(relayStore.agents).find(row => row.relay_id === relay.id)!;
    expect(agent).toBeDefined();
    const outcomes: string[] = [];
    const values: Page[] = [];
    const errors: unknown[] = [];
    const promise = relayStore.getConversationHistory(agent, { signal });
    void promise.then(value => { values.push(value); outcomes.push('resolved'); },
      error => { errors.push(error); outcomes.push('rejected'); });
    const frame = socket.sent.map(body => JSON.parse(body)).filter(row => row.type === 'get_conversation_history').at(-1)!;
    expect(frame).toMatchObject({
      type: 'get_conversation_history', protocol: 3, request_id: expect.any(String),
      pane_id: 'w1:p1', target: { relay_id: relay.id, terminal_id: 'terminal-w1:p1', generation: 1 },
    });
    expect(frame.request_id).not.toBe('');
    return { promise, outcomes, values, errors, frame, socket };
  }
  type History = ReturnType<typeof history>;
  function result(request: History, data: Record<string, unknown>, socket = request.socket, extra: Record<string, unknown> = {}) {
    socket.message({ type: 'command_result', request_id: request.frame.request_id, ok: true, data, ...extra });
  }
  function poisonedPage() {
    return page('poison must not be interpreted', { omo_plan: plan({ version: { toString: null } }) });
  }
  async function subsequent() {
    const next = history();
    result(next, page('subsequent genuine history', { omo_plan: plan({ version: '12' }) }));
    const value = await next.promise;
    expect(value.entries[0].text).toBe('subsequent genuine history');
    expect(value.omoPlan?.version).toBe(12);
    expect(next.outcomes).toEqual(['resolved']);
  }

  it.each(wireScalars)('status $name reaches push snapshot and the live generation fence', ({ fields, expected }) => {
    const statusFields = Object.fromEntries(Object.entries(fields).filter(([key]) => key !== 'version'));
    const numeric = expected ?? 0;
    snapshot(statusFields, 'scalar snapshot');
    assertStatus({ server_protocol: numeric, generation: numeric }, 'scalar snapshot');
    snapshot({ server_protocol: 91, generation: Number.MIN_SAFE_INTEGER }, 'minimum seed');
    live(statusFields, 'scalar live');
    if (numeric === Number.MIN_SAFE_INTEGER) {
      // The minimum safe endpoint cannot be freshly admitted below a safe seed.
      assertStatus({ server_protocol: 91, generation: Number.MIN_SAFE_INTEGER }, 'minimum seed');
      expect(current().capabilities).toEqual(['wire-snapshot']);
    } else {
      assertStatus({ server_protocol: numeric, generation: numeric }, 'scalar live');
      expect(current().capabilities).toEqual(['wire-live', 'scalar live']);
    }
  });

  it.each(wireScalars)('history $name decodes the correlated plan without dropping its content', async ({ fields, expected }) => {
    const versionFields = Object.hasOwn(fields, 'version') ? { version: fields.version } : {};
    const data = page('scalar history', { omo_plan: plan(versionFields) });
    const request = history();
    result(request, data);
    const value = await request.promise;
    expect(value.entries[0].text).toBe('scalar history');
    expect(value.omoPlan).toEqual({
      available: true, reason_code: undefined, session_id: 'wire-session', updated_at: 'wire-time',
      version: expected, phases: [{ name: 'Wire phase', tasks: [
        { id: 'wire-task', content: 'Preserve this task', status: 'in_progress' },
      ] }], truncated: false,
    });
    expect(Object.hasOwn(value.omoPlan!, 'version')).toBe(true);
    expect(Object.is(value.omoPlan!.version, -0)).toBe(Object.is(expected, -0));
    // Supplemental exported surface, after the genuine public request/JSON consumer.
    expect(normalizeConversationPage(JSON.parse(JSON.stringify(data))).omoPlan).toEqual(value.omoPlan);
    expect(request.outcomes).toEqual(['resolved']);
  });

  it('fresh connections and missing/null/array/nonrecord status containers retain numeric defaults', () => {
    const { relay } = endpoint();
    relayStore.connectRelay(relay);
    expect(current().herdrStatus).toMatchObject({ server_protocol: 0, generation: 0, features: {} });
    endpoint().socket.open();
    for (const fields of [{}, { herdr_status: null }, { herdr_status: [] }, { herdr_status: 7 }, { herdr_status: 'status' }, { herdr_status: false }]) {
      endpoint().socket.message({ type: 'push_config', protocol: 3, ...fields });
      expect(current().herdrStatus).toEqual({
        installed_client_version: '', server_version: '', server_protocol: 0, server_protocol_known: false,
        endpoint_protocol_generation: null, generation: 0, features: {},
      });
      snapshot({ server_protocol: 9, generation: Number.MIN_SAFE_INTEGER }, 'container seed');
      const liveFields = Object.hasOwn(fields, 'herdr_status') ? { status: fields.herdr_status } : {};
      endpoint().socket.message({ type: 'herdr_status', ...liveFields });
      expect(current().herdrStatus).toMatchObject({ server_protocol: 0, generation: 0, features: {} });
    }
  });

  it('live generations reject stale/equal/duplicate/coercively-equal and invalid-zero repeats before next valid progress', () => {
    snapshot({ server_protocol: 2, generation: -2 }, 'ordering seed');
    live({ server_protocol: 3, generation: -1 }, 'negative next');
    assertStatus({ server_protocol: 3, generation: -1 }, 'negative next');
    for (const generation of [-2, -1, -1, '-1']) {
      live({ server_protocol: 99, generation }, 'must stay ignored');
      assertStatus({ server_protocol: 3, generation: -1 }, 'negative next');
      expect(current().capabilities).toEqual(['wire-live', 'negative next']);
    }
    live({ server_protocol: 4, generation: 'NaN' }, 'invalid becomes zero');
    assertStatus({ server_protocol: 4, generation: 0 }, 'invalid becomes zero');
    for (const generation of ['Infinity', {}, 0, '0']) {
      live({ server_protocol: 99, generation }, 'zero repeat ignored');
      assertStatus({ server_protocol: 4, generation: 0 }, 'invalid becomes zero');
    }
    live({ server_protocol: '5', generation: '1' }, 'valid next');
    assertStatus({ server_protocol: 5, generation: 1 }, 'valid next');
  });

  it('unsolicited status stays relay-local and a retired socket cannot affect its public replacement', () => {
    snapshot({ server_protocol: 2, generation: 2 }, 'owner');
    snapshot({ server_protocol: 7, generation: 7 }, 'other', '8872');
    live({ server_protocol: '8', generation: '8' }, 'other next', '8872');
    assertStatus({ server_protocol: 2, generation: 2 }, 'owner');
    assertStatus({ server_protocol: 8, generation: 8 }, 'other next', '8872');
    const retired = endpoint();
    relayStore.connectRelay(retired.relay);
    const replacement = endpoint().socket;
    expect(replacement).not.toBe(retired.socket);
    replacement.open();
    snapshot({ server_protocol: 3, generation: 3 }, 'replacement');
    expect(() => retired.socket.message({
      type: 'herdr_status', status: status({ server_protocol: { toString: null }, generation: 99 }, 'retired poison'),
    })).not.toThrow();
    assertStatus({ server_protocol: 3, generation: 3 }, 'replacement');
    live({ server_protocol: 4, generation: 4 }, 'replacement next');
    assertStatus({ server_protocol: 4, generation: 4 }, 'replacement next');
    assertStatus({ server_protocol: 8, generation: 8 }, 'other next', '8872');
  });

  it('history missing/null/array/nonrecord plan containers remain absent', async () => {
    for (const extra of [{}, { omo_plan: null }, { omo_plan: [] }, { omo_plan: 7 }, { omo_plan: 'plan' }, { omo_plan: false }]) {
      const request = history();
      result(request, page('malformed container history', extra));
      const value = await request.promise;
      expect(value.entries[0].text).toBe('malformed container history');
      expect(value.omoPlan).toBeUndefined();
      expect(request.outcomes).toEqual(['resolved']);
    }
  });

  it('history ignores wrong relay/request/type and legacy accepted remains nonterminal without action validation', async () => {
    const request = history();
    result(request, poisonedPage(), endpoint('8872').socket);
    result(request, poisonedPage(), request.socket, { request_id: 'unrelated-wire-request' });
    result(request, poisonedPage(), request.socket, { type: 'unrecognized_history_result' });
    result(request, poisonedPage(), request.socket, { phase: 'accepted' });
    await Promise.resolve();
    expect(request.outcomes).toEqual([]);
    result(request, page('matching final', { omo_plan: plan({ version: '6' }) }), request.socket, { action: 'different_legacy_action' });
    const value = await request.promise;
    expect(value.entries[0].text).toBe('matching final');
    expect(value.omoPlan?.version).toBe(6);
    expect(request.outcomes).toEqual(['resolved']);
  });

  it('history success ignores duplicate/late poisoned finals and permits a later genuine request', async () => {
    const request = history();
    result(request, page('first success', { omo_plan: plan({ version: 3 }) }));
    const value = await request.promise;
    result(request, poisonedPage());
    await vi.advanceTimersByTimeAsync(20_001);
    result(request, poisonedPage());
    expect(value.entries[0].text).toBe('first success');
    expect(value.omoPlan?.version).toBe(3);
    expect(request.outcomes).toEqual(['resolved']);
    await subsequent();
  });

  it('history cancellation ignores late poisoned finals and permits a later genuine request', async () => {
    const controller = new AbortController();
    const request = history(controller.signal);
    controller.abort();
    await expect(request.promise).rejects.toMatchObject({ code: 'request_cancelled' });
    result(request, poisonedPage());
    await vi.advanceTimersByTimeAsync(20_001);
    result(request, poisonedPage());
    expect(request.outcomes).toEqual(['rejected']);
    expect(request.values).toEqual([]);
    await subsequent();
  });

  it('history timeout ignores late poisoned finals and permits a later genuine request', async () => {
    const request = history();
    await vi.advanceTimersByTimeAsync(20_001);
    await expect(request.promise).rejects.toMatchObject({
      message: 'Relay confirmation timed out', data: { dispatched_unknown: true },
    });
    result(request, poisonedPage());
    expect(request.outcomes).toEqual(['rejected']);
    expect(request.values).toEqual([]);
    await subsequent();
  });

  it('status native conversion errors at both fields including stale poison throw synchronously before the fence', () => {
    for (const fields of [
      { server_protocol: { toString: null }, generation: 4 },
      { server_protocol: 4, generation: { toString: null } },
      { server_protocol: { toString: null }, generation: 2 },
    ]) {
      snapshot({ server_protocol: 3, generation: 3 }, 'before error');
      expect(() => live(fields, 'conversion error')).toThrow(TypeError);
      assertStatus({ server_protocol: 3, generation: 3 }, 'before error');
      live({ server_protocol: 4, generation: 4 }, 'after error');
      assertStatus({ server_protocol: 4, generation: 4 }, 'after error');
    }
  });

  it('history native conversion errors reject asynchronously only after actual command resources are released', async () => {
    const controller = new AbortController();
    const add = vi.spyOn(controller.signal, 'addEventListener');
    const remove = vi.spyOn(controller.signal, 'removeEventListener');
    const clear = vi.spyOn(globalThis, 'clearTimeout');
    type Entry = { relayId: string; action: string; timer: ReturnType<typeof setTimeout>; cleanup: () => void };
    let observed: { map: Map<string, Entry>; key: string; entry: Entry } | undefined;
    const nativeSet = Map.prototype.set;
    const capture = vi.spyOn(Map.prototype, 'set').mockImplementation(function (this: Map<string, Entry>, key: string, entry: Entry) {
      const answer = nativeSet.call(this, key, entry);
      if (entry?.relayId === endpoint().relay.id && entry?.action === 'get_conversation_history') {
        observed = { map: this, key, entry }; // Passive observation of the genuine registered entry.
      }
      return answer;
    });
    const request = history(controller.signal);
    expect(observed).toBeDefined();
    const row = observed!;
    expect(row.key).toBe(request.frame.request_id);
    expect(row.map.get(row.key)).toBe(row.entry);
    const listener = add.mock.calls.find(call => call[0] === 'abort')![1];
    expect(() => result(request, poisonedPage())).not.toThrow();
    expect(row.map.has(row.key)).toBe(false);
    expect(clear.mock.calls.filter(call => call[0] === row.entry.timer)).toHaveLength(1);
    expect(remove).toHaveBeenCalledExactlyOnceWith('abort', listener);
    expect(request.outcomes).toEqual([]); // Command release is synchronous; plan projection resumes after await.
    await expect(request.promise).rejects.toBeInstanceOf(TypeError);
    expect(request.errors[0]).not.toBeInstanceOf(CommandError);
    expect(request.outcomes).toEqual(['rejected']);
    capture.mockRestore();
    await subsequent();
  });
});

describe('command assigned-deadline abort regressions', () => {
  type Pending = {
    timer: ReturnType<typeof setTimeout>;
    cleanup: () => void;
    resolve: (value: unknown) => void;
    reject: (error: CommandError) => void;
  };

  it.each([
    { name: 'prepared repeated renewal', phase: 'prepared', agentStart: false },
    { name: 'awaiting_evidence repeated renewal', phase: 'awaiting_evidence', agentStart: false },
    { name: 'legacy accepted repeated renewal', phase: 'accepted', agentStart: false },
    { name: 'agent_start prepared then legacy accepted', phase: 'prepared', agentStart: true },
  ] as const)('$name', async ({ name, phase, agentStart }) => {
    vi.useFakeTimers();
    const handles: Array<Parameters<typeof clearTimeout>[0]> = [];
    const controllers: AbortController[] = [];
    const promises: Array<Promise<unknown>> = [];
    try {
      transportHijack.current = null;
      MockWebSocket.instances = [];
      vi.stubGlobal('WebSocket', MockWebSocket);
      localStorage.clear();
      sessionStorage.clear();
      relayStore.destroy();
      relayStore.setHidden(false);
      relayStore.relayConfigs.set([]);
      relayStore.addRelay({ label: 'Assigned A', url: 'ws://127.0.0.1:8871', token: '' });
      relayStore.addRelay({ label: 'Assigned B', url: 'ws://127.0.0.1:8872', token: '' });
      for (const socket of MockWebSocket.instances.slice(-2)) {
        socket.open();
        socket.message({ type: 'push_config', protocol: 3, capabilities: [], inventory: { state: 'ready' } });
      }
      const map = (relayStore as unknown as { pendingRequests: Map<string, Pending> }).pendingRequests;
      const nativeClear = globalThis.clearTimeout;
      const nativeDelete = map.delete;
      const nativeRemove = AbortSignal.prototype.removeEventListener;
      const events: Array<{ kind: string; timer?: Parameters<typeof clearTimeout>[0]; key?: string; signal?: AbortSignal; mapped: boolean }> = [];
      let ownerKey = '';
      const clear = vi.spyOn(globalThis, 'clearTimeout').mockImplementation(function (this: unknown, ...args) {
        events.push({ kind: 'clear', timer: args[0], mapped: map.has(ownerKey) });
        return nativeClear.apply(this, args);
      });
      const deletion = vi.spyOn(map, 'delete').mockImplementation(function (this: Map<string, Pending>, key) {
        const answer = nativeDelete.call(this, key);
        events.push({ kind: 'delete', key, mapped: this.has(key) });
        return answer;
      });
      const add = vi.spyOn(AbortSignal.prototype, 'addEventListener');
      const remove = vi.spyOn(AbortSignal.prototype, 'removeEventListener').mockImplementation(function (this: AbortSignal, ...args) {
        events.push({ kind: 'remove', signal: this, mapped: map.has(ownerKey) });
        return nativeRemove.apply(this, args);
      });
      const schedule = vi.spyOn(globalThis, 'setTimeout');
      function begin(port: string, start = false) {
        const relay = get(relayStore.relayConfigs).find(row => new URL(row.url).port === port)!;
        const socket = MockWebSocket.instances.filter(row => new URL(row.url).port === port).at(-1)!;
        const controller = new AbortController();
        controllers.push(controller);
        const outcomes: string[] = [];
        const promise = relayStore.sendCommand(relay.id, {
          type: start ? 'agent_start' : 'tab_reorder', pane_id: 'w1:p1',
          ...exactWireScope('w1:p1', relay.id), insert_index: 1,
          intent: { action_id: crypto.randomUUID() },
        }, undefined, false, controller.signal).then(
          value => { outcomes.push('resolved'); return { status: 'resolved' as const, value }; },
          error => { outcomes.push('rejected'); return { status: 'rejected' as const, error }; },
        );
        promises.push(promise); // Handler is installed in the same turn as the genuine public call.
        const frame = JSON.parse(socket.sent.at(-1)!);
        const entry = map.get(frame.request_id)!;
        expect(entry).toBeDefined();
        const timer = entry.timer;
        handles.push(timer);
        const registration = add.mock.calls.findIndex((call, index) => call[0] === 'abort' && add.mock.contexts[index] === controller.signal);
        expect(registration).toBeGreaterThanOrEqual(0);
        const listener = add.mock.calls[registration][1];
        const cleanup = vi.spyOn(entry, 'cleanup');
        const resolve = vi.spyOn(entry, 'resolve');
        const reject = vi.spyOn(entry, 'reject');
        return { relay, socket, controller, promise, outcomes, frame, entry, timer, listener, cleanup, resolve, reject };
      }
      type Request = ReturnType<typeof begin>;
      const legacy = (row: Request, fields: Record<string, unknown> = { ok: true, phase: 'completed' }) =>
        ({ type: 'command_result', request_id: row.frame.request_id, action: row.frame.type, ...fields });
      const receipt = (row: Request, receiptPhase: string) =>
        ({ type: 'action_receipt', request_id: row.frame.request_id, receipt: { action_id: row.frame.intent.action_id, phase: receiptPhase } });
      const counts = (row: Request) => ({
        cleanup: row.cleanup.mock.calls.length, resolve: row.resolve.mock.calls.length, reject: row.reject.mock.calls.length,
        removals: remove.mock.calls.filter((call, index) => call[1] === row.listener && remove.mock.contexts[index] === row.controller.signal).length,
        clears: clear.mock.calls.filter(call => handles.includes(call[0]!) && (call[0] === row.timer || call[0] === row.entry.timer)).length,
      });
      const a = begin('8871', agentStart);
      ownerKey = a.frame.request_id;
      const b = begin('8872');
      const initialSchedule = schedule.mock.results.findIndex(row => row.value === a.timer);
      expect(schedule.mock.calls[initialSchedule][1]).toBe(agentStart ? 60_000 : 15_000);
      const renewals: Array<{ previous: ReturnType<typeof setTimeout>; assigned: ReturnType<typeof setTimeout>; at: number }> = [];
      for (const delay of [100, 200]) {
        await vi.advanceTimersByTimeAsync(delay);
        const previous = a.entry.timer;
        a.socket.message(agentStart && renewals.length === 1 ? legacy(a, { ok: true, phase: 'accepted' })
          : phase === 'accepted' ? legacy(a, { ok: true, phase }) : receipt(a, phase));
        const assigned = a.entry.timer;
        if (assigned !== previous) handles.push(assigned);
        renewals.push({ previous, assigned, at: Date.now() });
      }
      const current = a.entry.timer;
      const bBefore = counts(b);
      const clearBefore = clear.mock.calls.length;
      const deleteBefore = deletion.mock.calls.length;
      await vi.advanceTimersByTimeAsync(100);
      const eventBefore = events.length;
      a.controller.abort();
      const first = await a.promise;
      const atAbort = counts(a);
      const bAfterAbort = counts(b);
      const bEntryAfterAbort = map.get(b.frame.request_id);
      const abortClears = clear.mock.calls.slice(clearBefore).map(call => call[0]);
      const abortDeletes = deletion.mock.calls.slice(deleteBefore).map(call => call[0]);
      const abortEvents = events.slice(eventBefore);
      b.socket.message(legacy(b));
      const bResult = await b.promise; // B completes by matching JSON before its 15s deadline.
      const clock: Array<{ at: number; cleanup: number; reject: number; removals: number }> = [];
      for (const elapsed of [9_699, 201, agentStart ? 49_701 : 4_701]) {
        await vi.advanceTimersByTimeAsync(elapsed);
        const observed = counts(a);
        clock.push({ at: Date.now(), cleanup: observed.cleanup, reject: observed.reject, removals: observed.removals });
      }
      const afterClock = counts(a);
      const beforeLate = counts(a);
      for (const frame of [
        receipt(a, 'prepared'), receipt(a, 'awaiting_evidence'), receipt(a, 'confirmed'), receipt(a, 'confirmed'),
        legacy(a), legacy(a, { ok: false, error: 'late duplicate' }),
        { type: 'error', request_id: a.frame.request_id, error: { code: 'late_abort_error' } },
      ]) a.socket.message(frame);
      const afterLate = counts(a);
      for (const port of ['8871', '8872']) {
        const fresh = begin(port);
        fresh.socket.message(legacy(fresh));
        expect(await fresh.promise).toMatchObject({ status: 'resolved', value: { ok: true, request_id: fresh.frame.request_id } });
      }
      // No new-policy assertion precedes ordinary clock delivery, late JSON, or fresh requests.
      expect(renewals[0].assigned).not.toBe(a.timer);
      expect(renewals[1].at).toBeGreaterThan(renewals[0].at);
      expect(renewals[1].assigned === renewals[0].assigned).toBe(agentStart);
      for (const row of renewals.filter(row => row.assigned !== row.previous)) {
        expect(clear.mock.calls.filter(call => call[0] === row.previous).length).toBeGreaterThanOrEqual(1);
        const scheduled = schedule.mock.results.findIndex(result => result.value === row.assigned);
        expect(schedule.mock.calls[scheduled]).toEqual([expect.any(Function), 10_000]);
      }
      expect(first.status).toBe('rejected');
      if (first.status !== 'rejected') throw new Error('Expected native command cancellation');
      expect(first.error.constructor).toBe(CommandError);
      expect(first.error.message).toBe('Conversation history request was cancelled.');
      expect(first.error.code).toBe('request_cancelled');
      expect(first.error.data).toBeUndefined();
      expect(a.outcomes).toEqual(['rejected']);
      expect(await a.promise).toBe(first);
      expect([atAbort.cleanup, atAbort.resolve, atAbort.reject, atAbort.removals]).toEqual([0, 0, 0, 1]);
      expect(map.has(a.frame.request_id)).toBe(false);
      expect(abortDeletes).toEqual([a.frame.request_id]);
      expect(abortEvents.map(row => row.kind)).toEqual(['clear', 'delete', 'remove']);
      expect(abortEvents.map(row => row.mapped)).toEqual([true, false, false]);
      expect(abortEvents[2].signal).toBe(a.controller.signal);
      expect(bEntryAfterAbort).toBe(b.entry);
      expect(b.entry.timer).toBe(b.timer);
      expect(bAfterAbort).toEqual(bBefore);
      expect(bBefore).toEqual({ cleanup: 0, resolve: 0, reject: 0, removals: 0, clears: 0 });
      expect(bResult).toMatchObject({ status: 'resolved', value: { ok: true, request_id: b.frame.request_id } });
      expect(b.cleanup).toHaveBeenCalledTimes(1);
      expect(b.resolve).toHaveBeenCalledTimes(1);
      expect(b.reject).not.toHaveBeenCalled();
      expect(afterLate).toEqual(beforeLate);
      expect(counts(a)).toEqual(afterLate);
      const physical = {
        currentClearAtAbort: abortClears.filter(timer => timer === current).length,
        initialClearAtAbort: abortClears.filter(timer => timer === a.timer).length,
        laterCleanup: afterClock.cleanup, laterReject: afterClock.reject, totalRemovals: afterClock.removals,
      };
      console.info('T038_ABORT_OBSERVATION ' + JSON.stringify({
        name, requestId: a.frame.request_id, actionId: a.frame.intent.action_id,
        initial: String(a.timer), assigned: String(current),
        renewals: renewals.map(row => ({ previous: String(row.previous), assigned: String(row.assigned), at: row.at })),
        abortClears: abortClears.map(String), physical, clock,
        firstOutcome: first.status, bUntouchedAtAbort: JSON.stringify(bBefore) === JSON.stringify(bAfterAbort),
      }));
      expect(physical, 'assigned deadline physical cancellation/nonexecution').toEqual({
        currentClearAtAbort: 1, initialClearAtAbort: 0, laterCleanup: 0, laterReject: 0, totalRemovals: 1,
      });
    } finally {
      vi.restoreAllMocks();
      for (const controller of controllers) controller.abort();
      relayStore.destroy();
      for (const timer of handles) clearTimeout(timer); // Every captured genuine handle, including expected-red survivors.
      await Promise.all(promises);
      relayStore.setHidden(false);
      relayStore.relayConfigs.set([]);
      transportHijack.current = null;
      vi.clearAllTimers();
      vi.useRealTimers();
      vi.unstubAllGlobals();
    }
  });
});
