import { get } from 'svelte/store';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { BrowserDeviceCredentialStore } from '$lib/device-auth';
import { base64UrlEncode } from '$lib/base64url';
import { relayStore } from '$lib/store';
import { targetRefForAgent } from '$lib/resource-id';
import type { RelayConfig } from '$lib/types';
import type { RelayTransport, TransportHandlers } from '$lib/transports';

const sessions = vi.hoisted(() => new Map<string, { handlers: TransportHandlers; sent: Record<string, unknown>[] }>());
vi.mock('$lib/transports', async (original) => ({
  ...await original(),
  createRelayTransport: (relay: RelayConfig, handlers: TransportHandlers): RelayTransport => {
    const sent: Record<string, unknown>[] = [];
    sessions.set(relay.id, { handlers, sent });
    return {
      kind: 'websocket', connect: () => handlers.onStatus('connected', { path: 'websocket' }),
      close: () => {}, send: (payload) => { sent.push(payload); return true; },
    };
  },
}));

function rawAgent(generation = 1) {
  return { pane_id: 'pane-1', server_session_id: 'primary', terminal_id: 'terminal-1', generation,
    agent_session_id: '', name: 'Safe label', agent: 'codex', status: 'idle', workspace_id: 'workspace-1' };
}
function boot(id = 'relay-a', supported = true, role: 'reader' | 'controller' = 'controller') {
  const credentials = new BrowserDeviceCredentialStore(localStorage);
  credentials.saveInvitation(id, { id: 'fixture-invitation', version: 1,
    secret: base64UrlEncode(new Uint8Array(32).fill(7)), expiresAt: Date.now() + 60_000 });
  credentials.replaceInvitation(id, 'fixture-invitation', { deviceId: `device-${id}`, credentialId: `credential-${id}`,
    credentialVersion: 1, credentialSecret: base64UrlEncode(new Uint8Array(32).fill(8)), role, locale: 'en' });
  const relay: RelayConfig = { id, label: id, url: `wss://${id}.example`, token: '', paired: true };
  relayStore.relayConfigs.update((relays) => [...relays, relay]);
  relayStore.connectRelay(relay);
  const session = sessions.get(id)!;
  session.handlers.onMessage({ type: 'push_config', protocol: 3, inventory: { state: 'ready', stale: false },
    capabilities: supported ? ['inventory_snapshot_v1'] : [] });
  const request = () => session.sent.findLast((payload) => payload.snapshot_request_id)!;
  const reply = (generation = 1, overrides: Record<string, unknown> = {}) => {
    session.handlers.onMessage({ type: 'inventory_snapshot', snapshot_request_id: request().snapshot_request_id,
      inventory: { state: 'ready', stale: false }, agents: [rawAgent(generation)],
      workspaces: [{ workspace_id: 'workspace-1', label: 'Workspace' }], ...overrides });
  };
  return { session, request, reply, id };
}

beforeEach(() => {
  relayStore.destroy(); sessions.clear(); localStorage.clear(); sessionStorage.clear();
  relayStore.relayConfigs.set([]);
  relayStore.setActionLocked(false);
});
afterEach(() => { relayStore.destroy(); vi.restoreAllMocks(); });

describe('B2 store freshness and dispatch boundary', () => {
  it('does not accept old relay or uncorrelated inventory as authority with persistence/metrics disabled', () => {
    const old = boot('old-relay', false);
    old.session.handlers.onMessage({ type: 'agents', agents: [rawAgent()] });
    expect(get(relayStore.agents)).toEqual([]);
    expect(relayStore.relayActionsFresh(old.id)).toBe(false);
    expect(relayStore.sendRaw(old.id, { type: 'install_update' })).toBe(false);
    const current = boot();
    current.session.handlers.onMessage({ type: 'agents', agents: [rawAgent()] });
    expect(relayStore.relayActionsFresh(current.id)).toBe(false);
    expect(relayStore.sendRaw(current.id, { type: 'check_update' })).toBe(false);
    current.reply();
    expect(relayStore.relayActionsFresh(current.id)).toBe(true);
    expect(get(relayStore.agents)).toHaveLength(1);
  });

  it('guards raw, command, upload, watches/leases and unknown commands before freshness', async () => {
    const f = boot();
    const target = { ...rawAgent(), relay_id: f.id };
    for (const type of ['send_keys', 'upload_begin', 'watch_pane', 'lease_pane_size', 'install_update',
      'device_list', 'push_open_ref', 'unknown_future_write']) {
      expect(relayStore.sendRaw(f.id, { type, target })).toBe(false);
    }
    await expect(relayStore.sendCommand(f.id, { type: 'install_update' })).rejects.toThrow('Waiting for current');
    expect(relayStore.sendRaw(f.id, { type: 'refresh_agents' })).toBe(true);
    f.reply();
    expect(relayStore.sendRaw(f.id, { type: 'unknown_future_write' })).toBe(false);
  });

  it('re-resolves exact generations, never promotes a retained target by matching pane ID', async () => {
    const f = boot(); f.reply();
    const old = get(relayStore.agents)[0];
    relayStore.requestAgents(true);
    f.reply(2);
    await expect(relayStore.sendToAgent(old, { type: 'send_keys', keys: ['ENTER'] })).rejects.toThrow();
    const current = get(relayStore.agents)[0];
    expect(relayStore.sendRaw(f.id, { type: 'send_keys', target: targetRefForAgent(current), keys: ['ENTER'] })).toBe(true);
  });

  it('keeps mixed relays, workspace validation and reader/controller roles separate', () => {
    const a = boot('relay-a', true, 'reader');
    const b = boot('relay-b');
    a.reply(1, { workspaces: 'malformed' });
    expect(relayStore.relayActionsFresh(a.id)).toBe(true);
    expect(relayStore.relayActionsFresh(b.id)).toBe(false);
    expect(relayStore.sendRaw(a.id, { type: 'workspace_create', label: 'Unsafe' })).toBe(false);
    expect(relayStore.sendRaw(a.id, { type: 'reset_devices' })).toBe(false);
    expect(relayStore.sendRaw(a.id, { type: 'revoke_device', device_id: `device-${a.id}` })).toBe(true);
    expect(relayStore.sendRaw(a.id, { type: 'revoke_device', device_id: `device-${b.id}` })).toBe(false);
    expect(get(relayStore.agents).every((agent) => agent.relay_id === a.id)).toBe(true);
  });

  it('allows only cleanup of an existing watch while locked, never a new watch/lease', () => {
    const f = boot(); f.reply();
    const target = targetRefForAgent(get(relayStore.agents)[0]);
    expect(relayStore.sendRaw(f.id, { type: 'watch_pane', target })).toBe(true);
    relayStore.setActionLocked(true);
    expect(get(relayStore.agents)).toEqual([]);
    expect(relayStore.sendRaw(f.id, { type: 'watch_pane', target })).toBe(false);
    expect(relayStore.sendRaw(f.id, { type: 'release_pane_size', target })).toBe(false);
    expect(relayStore.sendRaw(f.id, { type: 'unwatch_pane', target })).toBe(true);
    expect(relayStore.sendRaw(f.id, { type: 'unwatch_pane', target })).toBe(false);
  });

  it('rejects old callbacks on wake, path promotion and replacement connections; empty is authoritative', () => {
    const f = boot();
    const oldNonce = f.request().snapshot_request_id;
    f.session.handlers.onStatus('connected', { path: 'webrtc' });
    f.reply(1, { snapshot_request_id: oldNonce });
    expect(relayStore.relayActionsFresh(f.id)).toBe(false);
    f.reply(1, { agents: [], workspaces: [] });
    expect(relayStore.relayActionsFresh(f.id)).toBe(true);
    expect(get(relayStore.agents)).toEqual([]);
    relayStore.revalidateConnections();
    expect(relayStore.relayActionsFresh(f.id)).toBe(false);
    f.reply(1, { inventory: { state: 'error', stale: true } });
    expect(relayStore.relayActionsFresh(f.id)).toBe(false);
  });
});
