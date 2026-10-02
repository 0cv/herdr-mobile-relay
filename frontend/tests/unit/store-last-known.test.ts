import { get } from 'svelte/store';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { BrowserDeviceCredentialStore } from '$lib/device-auth';
import { base64UrlEncode } from '$lib/base64url';
import { relayStore } from '$lib/store';
import { targetRefForAgent } from '$lib/resource-id';
import type { RelayConfig } from '$lib/types';
import type { RelayTransport, TransportHandlers, TransportAuthentication } from '$lib/transports';

const sessions = vi.hoisted(() => new Map<string, { handlers: TransportHandlers; sent: Record<string, unknown>[];
  authentication?: TransportAuthentication }>());
vi.mock('$lib/transports', async (original) => ({
  ...await original(),
  createRelayTransport: (relay: RelayConfig, handlers: TransportHandlers, authentication?: TransportAuthentication): RelayTransport => {
    const sent: Record<string, unknown>[] = [];
    sessions.set(relay.id, { handlers, sent, authentication });
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
  relayStore.initialize(false);
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

  it('does not promote an old session into a replaced or missing credential scope', () => {
    const f = boot('relay-a', true, 'reader'); f.reply();
    const target = targetRefForAgent(get(relayStore.agents)[0]);
    const credentials = new BrowserDeviceCredentialStore(localStorage);
    credentials.updateCredential(f.id, { deviceId: `device-${f.id}`, credentialId: `credential-${f.id}`,
      credentialVersion: 2, role: 'controller', locale: 'en' });
    window.dispatchEvent(new StorageEvent('storage', { key: 'herdr_device_auth_v1' }));
    expect(get(relayStore.agents)).toEqual([]);
    expect(relayStore.relayActionsFresh(f.id)).toBe(false);
    expect(relayStore.sendRaw(f.id, { type: 'send_keys', target, keys: ['ENTER'] })).toBe(false);
    relayStore.requestAgents(true); f.reply();
    expect(relayStore.relayActionsFresh(f.id)).toBe(false);
    credentials.remove(f.id);
    expect(relayStore.sendRaw(f.id, { type: 'reset_devices' })).toBe(false);
  });

  it('changes the deferred UI fence on replacement, suspension and credential role/version changes', () => {
    const f = boot(); f.reply();
    const first = relayStore.deferredUiContext();
    relayStore.suspendAuthority();
    expect(relayStore.deferredUiContext()).not.toBe(first);
    const suspended = relayStore.deferredUiContext();
    relayStore.connectRelay(get(relayStore.relayConfigs)[0]);
    expect(relayStore.deferredUiContext()).not.toBe(suspended);
    const beforeRole = relayStore.deferredUiContext();
    new BrowserDeviceCredentialStore(localStorage).updateCredential(f.id, {
      deviceId: `device-${f.id}`, credentialId: `credential-${f.id}`,
      credentialVersion: 2, role: 'reader', locale: 'en',
    });
    expect(relayStore.deferredUiContext()).not.toBe(beforeRole);
    expect(relayStore.deferredUiContext()).not.toContain(base64UrlEncode(new Uint8Array(32).fill(8)));
  });

  it('cannot commit a late authenticated finish over a newer stored credential', () => {
    const f = boot('relay-a', true, 'reader');
    const presented = f.session.authentication?.getAuthentication?.();
    expect(presented?.kind).toBe('credential');
    const credentials = new BrowserDeviceCredentialStore(localStorage);
    credentials.updateCredential(f.id, { deviceId: `device-${f.id}`, credentialId: `credential-${f.id}`,
      credentialVersion: 2, role: 'reader', locale: 'en' });
    expect(() => f.session.authentication?.onAuthenticated?.(presented!, {
      deviceId: `device-${f.id}`, credentialId: `credential-${f.id}`,
      credentialVersion: 1, role: 'controller', locale: 'en',
    })).toThrow('Authentication changed');
    expect(credentials.get(f.id)).toMatchObject({ version: 2, role: 'reader' });
  });

  it('invalidates pre-suspension replies even when measurement/persistence are off', () => {
    const f = boot();
    const oldNonce = f.request().snapshot_request_id;
    relayStore.setHidden(true);
    f.reply(1, { snapshot_request_id: oldNonce });
    expect(relayStore.relayActionsFresh(f.id)).toBe(false);
    relayStore.setHidden(false);
    relayStore.requestAgents(true); f.reply();
    expect(relayStore.relayActionsFresh(f.id)).toBe(true);
  });

  it('guards raw, command, upload, watches/leases and unknown commands before freshness', async () => {
    const f = boot();
    const target = { ...rawAgent(), relay_id: f.id };
    for (const type of ['send_keys', 'upload_begin', 'watch_pane', 'lease_pane_size', 'install_update',
      'device_list', 'push_open_ref', 'unknown_future_write']) {
      expect(relayStore.sendRaw(f.id, { type, target })).toBe(false);
    }
    await expect(relayStore.sendCommand(f.id, { type: 'install_update' })).rejects.toThrow('Waiting for current');
    relayStore.watchPane({ ...rawAgent(), relay_id: f.id, relay_label: f.id,
      raw_pane_id: 'pane-1', pane_id: `${f.id}::pane-1` });
    expect(relayStore.sendRaw(f.id, { type: 'refresh_agents' })).toBe(true);
    expect(relayStore.sendRaw(f.id, { type: 'push_subscribe', subscription: {} })).toBe(true);
    expect(relayStore.relayActionsFresh(f.id)).toBe(false);
    f.reply();
    expect(f.session.sent.some((message) => ['read_pane', 'watch_pane', 'install_update'].includes(String(message.type)))).toBe(false);
    expect(relayStore.sendRaw(f.id, { type: 'unknown_future_write' })).toBe(false);
  });

  it('rejects stale upload initiation and fences a retained controller across lock/unlock generations', async () => {
    const f = boot();
    const agent = { ...rawAgent(), relay_id: f.id, relay_label: f.id,
      raw_pane_id: 'pane-1', pane_id: `${f.id}::pane-1` };
    await expect(relayStore.uploadAttachments(agent, [new File(['png'], 'shot.png', { type: 'image/png' })])).rejects.toThrow('attachments are unavailable');
    f.reply();
    const controller = relayStore.attachmentController(get(relayStore.agents)[0]);
    controller.select([new File(['png'], 'shot.png', { type: 'image/png' })]);
    relayStore.setActionLocked(true); relayStore.setActionLocked(false);
    relayStore.requestAgents(true); f.reply();
    expect(relayStore.relayActionsFresh(f.id)).toBe(true);
    await expect(controller.upload()).rejects.toMatchObject({ code: 'attachment_upload_failed' });
    expect(f.session.sent.some((message) => message.type === 'upload_begin')).toBe(false);
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

  it('clears only already-owned viewing suppression while locked, without acquiring a new target', () => {
    const f = boot(); f.reply();
    const target = targetRefForAgent(get(relayStore.agents)[0]);
    expect(relayStore.sendRaw(f.id, { type: 'push_viewed_pane', target, visible: true, unlocked: true })).toBe(true);
    relayStore.setActionLocked(true);
    expect(relayStore.sendRaw(f.id, { type: 'push_viewed_pane', target, visible: true, unlocked: true })).toBe(false);
    expect(relayStore.sendRaw(f.id, { type: 'push_viewed_pane', visible: false, unlocked: false })).toBe(true);
    expect(relayStore.sendRaw(f.id, { type: 'push_viewed_pane', visible: false, unlocked: false })).toBe(false);
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
