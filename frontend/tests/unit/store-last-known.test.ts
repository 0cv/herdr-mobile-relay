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
function boot(id = 'relay-a', supported = true, role: 'reader' | 'controller' = 'controller', capabilities: string[] = []) {
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
    capabilities: supported ? ['inventory_snapshot_v1', ...capabilities] : capabilities });
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
afterEach(() => { relayStore.destroy(); vi.useRealTimers(); vi.restoreAllMocks(); });

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

  it.each([
    ['error', false], ['timeout', false], ['error', true], ['timeout', true],
  ] as const)('retains exact upload cleanup authority for retry after %s (replacement %s)', async (failure, reconnect) => {
    const f = boot('relay-a', true, 'controller', ['pane_size_lease']); f.reply();
    const other = boot('relay-b');
    const agent = get(relayStore.agents).find((item) => item.relay_id === f.id)!;
    const target = targetRefForAgent(agent);
    let connection = relayStore.connection(f.id)!;
    relayStore.sendRaw(f.id, { type: 'lease_pane_size', target, columns: 80 });
    const leaseGrant = [...connection.cleanupGrants][0];
    let cancels = 0;
    vi.spyOn(connection.transport!, 'send').mockImplementation((payload) => {
      f.session.sent.push(payload);
      if (payload.type === 'upload_begin') queueMicrotask(() => f.session.handlers.onMessage({ type: 'upload_begin_result',
        request_id: payload.request_id, result: { upload_id: 'retry-owned', chunk_bytes: 262144,
          expires_at: new Date(Date.now() + 120_000).toISOString(),
          limits: { max_files: 8, max_file_bytes: 20971520, max_batch_bytes: 52428800 } } }));
      if (payload.type === 'upload_chunk') queueMicrotask(() => f.session.handlers.onMessage({ type: 'upload_chunk_result',
        request_id: payload.request_id, error: { code: 'attachment_upload_failed' } }));
      if (payload.type === 'upload_cancel' && ++cancels === 1 && failure === 'error') {
        queueMicrotask(() => f.session.handlers.onMessage({ type: 'upload_cancel_result',
          request_id: payload.request_id, error: { code: 'attachment_cancel_failed' } }));
      }
      return true;
    });
    const controller = relayStore.attachmentController(agent);
    controller.select([new File(['png'], 'shot.png', { type: 'image/png' })]);
    await expect(controller.upload()).rejects.toMatchObject({ code: 'attachment_upload_failed' });
    expect(connection.cleanupGrants.size).toBe(2);
    relayStore.suspendAuthority();
    expect(relayStore.relayActionsFresh(f.id)).toBe(false);
    if (failure === 'timeout') vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const first = expect(controller.cancel()).rejects.toMatchObject({ code: 'attachment_cancel_failed' });
    const firstRequest = f.session.sent.findLast((message) => message.type === 'upload_cancel')!;
    if (failure === 'timeout') await vi.advanceTimersByTimeAsync(60_000);
    await first;
    expect(controller.hasPendingCleanup()).toBe(true);
    expect(controller.snapshot().issue?.code).toBe('attachment_cancel_failed');
    expect(() => controller.select([new File(['png'], 'new.png', { type: 'image/png' })])).toThrow('attachment_batch_locked');
    expect(connection.cleanupGrants.size).toBe(2);
    if (reconnect) {
      relayStore.connectRelay(get(relayStore.relayConfigs).find((relay) => relay.id === f.id)!);
      f.session = sessions.get(f.id)!;
      connection = relayStore.connection(f.id)!;
      expect(connection.cleanupGrants.size).toBe(1);
      expect([...connection.cleanupGrants][0]).toMatch(/^cancel:upload:/);
      expect(relayStore.relayActionsFresh(f.id)).toBe(false);
      f.session.handlers.onMessage({ type: 'push_config', protocol: 3, inventory: { state: 'ready', stale: false },
        capabilities: ['inventory_snapshot_v1'] });
      f.session.handlers.onMessage({ type: 'inventory_snapshot',
        snapshot_request_id: f.session.sent.findLast((message) => message.snapshot_request_id)!.snapshot_request_id,
        inventory: { state: 'ready', stale: false }, agents: [rawAgent()], workspaces: [] });
      expect(relayStore.relayActionsFresh(f.id)).toBe(true);
      vi.spyOn(connection.transport!, 'send').mockImplementation((payload) => {
        f.session.sent.push(payload);
        if (payload.type === 'upload_cancel') cancels++;
        return true;
      });
      for (const type of ['upload_chunk', 'upload_finish']) {
        expect(relayStore.sendRaw(f.id, { type, target, upload_id: 'retry-owned' })).toBe(false);
      }
    }
    expect(relayStore.sendRaw(f.id, { type: 'upload_cancel', target: { ...target, generation: 2 }, upload_id: 'retry-owned' })).toBe(false);
    expect(relayStore.sendRaw(other.id, { type: 'upload_cancel', target, upload_id: 'retry-owned' })).toBe(false);
    expect(relayStore.sendRaw(f.id, { type: 'upload_cancel', target, upload_id: 'unowned' })).toBe(false);
    const retry = controller.cancel();
    const request = f.session.sent.findLast((message) => message.type === 'upload_cancel')!;
    expect(request.upload_id).toBe('retry-owned');
    expect(request.request_id).not.toBe(firstRequest.request_id);
    other.session.handlers.onMessage({ type: 'upload_cancel_result', request_id: request.request_id, result: {} });
    f.session.handlers.onMessage({ type: 'upload_cancel_result', request_id: firstRequest.request_id, result: {} });
    f.session.handlers.onMessage({ type: 'upload_finish_result', request_id: request.request_id, result: {} });
    expect(connection.cleanupGrants.size).toBe(reconnect ? 1 : 2);
    f.session.handlers.onMessage({ type: 'upload_cancel_result', request_id: request.request_id, result: {} });
    await expect(retry).resolves.toBeUndefined();
    expect(controller.hasPendingCleanup()).toBe(false);
    expect([...connection.cleanupGrants]).toEqual(reconnect ? [] : [leaseGrant]);
    expect(cancels).toBe(2);
  });

  it.each(['finish', 'cancel'] as const)('reserves the 512th cleanup slot before begin, preserving %s after competing admission', async (completion) => {
    const f = boot('relay-a', true, 'controller', ['pane_size_lease', 'pane_realtime_delta']);
    f.reply(1, { agents: Array.from({ length: 511 }, (_, index) => ({ ...rawAgent(), pane_id: `pane-${index}`, terminal_id: `terminal-${index}` })) });
    const agents = get(relayStore.agents);
    const connection = relayStore.connection(f.id)!;
    for (const agent of agents) {
      expect(relayStore.sendRaw(f.id, { type: 'lease_pane_size', target: targetRefForAgent(agent), columns: 80 })).toBe(true);
    }
    expect(connection.cleanupGrants.size).toBe(511);
    vi.spyOn(connection.transport!, 'send').mockImplementation((payload) => {
      f.session.sent.push(payload);
      if (payload.type === 'upload_begin') {
        expect(connection.cleanupGrants.size).toBe(512);
        expect(relayStore.sendRaw(f.id, { type: 'watch_pane', target: targetRefForAgent(agents[0]) })).toBe(false);
        queueMicrotask(() => f.session.handlers.onMessage({ type: 'upload_begin_result', request_id: payload.request_id,
          result: { upload_id: 'reserved-upload', chunk_bytes: 262144, expires_at: new Date(Date.now() + 120_000).toISOString(),
            limits: { max_files: 8, max_file_bytes: 20971520, max_batch_bytes: 52428800 } } }));
      } else if (payload.type === 'upload_chunk') {
        expect([...connection.cleanupGrants].some((key) => key.endsWith(':reserved-upload'))).toBe(true);
        queueMicrotask(() => f.session.handlers.onMessage({ type: 'upload_chunk_result', request_id: payload.request_id,
          ...(completion === 'cancel' ? { error: { code: 'attachment_upload_failed' } }
            : { result: { file_index: 0, next_sequence: 1, received_bytes: 3 } }) }));
      } else if (payload.type === 'upload_finish') {
        queueMicrotask(() => f.session.handlers.onMessage({ type: 'upload_finish_result', request_id: payload.request_id,
          result: { attachments: [{ ref: 'attachment:reserved', name: 'shot.png', media_type: 'image/png', bytes: 3,
            sha256: (payload.files as Array<{ sha256: string }>)[0].sha256,
            expires_at: new Date(Date.now() + 60_000).toISOString() }] } }));
      } else if (payload.type === 'upload_cancel') {
        queueMicrotask(() => f.session.handlers.onMessage({ type: 'upload_cancel_result', request_id: payload.request_id, result: {} }));
      }
      return true;
    });
    const controller = relayStore.attachmentController(agents[0]);
    controller.select([new File(['png'], 'shot.png', { type: 'image/png' })]);
    if (completion === 'cancel') {
      await expect(controller.upload()).rejects.toMatchObject({ code: 'attachment_upload_failed' });
      relayStore.suspendAuthority();
      await expect(controller.cancel()).resolves.toBeUndefined();
    } else await expect(controller.upload()).resolves.toHaveLength(1);
    expect(connection.cleanupGrants.size).toBe(511);
    expect([...connection.cleanupGrants].every((key) => key.startsWith('lease_pane_size:'))).toBe(true);
  });

  it.each(['write', 'error', 'timeout', 'abort', 'disconnect'] as const)('releases pending begin reservations after %s', async (failure) => {
    const f = boot(); f.reply();
    const connection = relayStore.connection(f.id)!;
    const signal = new AbortController();
    if (failure === 'write') vi.spyOn(connection.transport!, 'send').mockReturnValue(false);
    if (failure === 'timeout') vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const request = (relayStore as any).sendUploadRequest(f.id, 'upload_begin', 'upload_begin_result',
      { target: targetRefForAgent(get(relayStore.agents)[0]), files: [] }, signal.signal);
    const rejected = expect(request).rejects.toBeDefined();
    if (failure !== 'write') expect(connection.cleanupGrants.size).toBe(1);
    if (failure === 'error') f.session.handlers.onMessage({ type: 'upload_begin_result',
      request_id: f.session.sent.findLast((message) => message.type === 'upload_begin')!.request_id,
      error: { code: 'attachment_upload_failed' } });
    if (failure === 'timeout') await vi.advanceTimersByTimeAsync(60_000);
    if (failure === 'abort') signal.abort();
    if (failure === 'disconnect') relayStore.disconnectRelay(f.id);
    await rejected;
    expect(connection.cleanupGrants.size).toBe(0);
  });

  it.each(['credential', 'endpoint', 'removal', 'auth-rejected', 'enrollment'] as const)('does not carry upload cancellation into a changed %s scope', async (change) => {
    const f = boot(); f.reply();
    const connection = relayStore.connection(f.id)!;
    vi.spyOn(connection.transport!, 'send').mockImplementation((payload) => {
      f.session.sent.push(payload);
      if (payload.type === 'upload_begin') queueMicrotask(() => f.session.handlers.onMessage({ type: 'upload_begin_result',
        request_id: payload.request_id, result: { upload_id: 'bound-cleanup' } }));
      return true;
    });
    const target = targetRefForAgent(get(relayStore.agents)[0]);
    await (relayStore as any).sendUploadRequest(f.id, 'upload_begin', 'upload_begin_result', { target, files: [] });
    const relay = get(relayStore.relayConfigs)[0];
    if (change === 'credential') {
      new BrowserDeviceCredentialStore(localStorage).updateCredential(f.id, {
        deviceId: `device-${f.id}`, credentialId: `credential-${f.id}`, credentialVersion: 2, role: 'controller', locale: 'en' });
      expect(relayStore.sendRaw(f.id, { type: 'upload_cancel', target, upload_id: 'bound-cleanup' })).toBe(false);
    }
    if (change === 'endpoint') relay.url = 'wss://different.example';
    if (change === 'removal') relayStore.removeRelay(f.id);
    if (change === 'auth-rejected') f.session.handlers.onStatus('closed', { code: 'device_unauthorized' });
    relayStore.connectRelay(relay);
    if (change === 'enrollment') {
      const authentication = sessions.get(f.id)!.authentication!;
      authentication.onAuthenticated!(authentication.getAuthentication!()!, {
        deviceId: `device-${f.id}`, credentialId: `credential-${f.id}`, credentialVersion: 2, role: 'controller', locale: 'en',
      });
    }
    expect(relayStore.sendRaw(f.id, { type: 'upload_cancel', target, upload_id: 'bound-cleanup' })).toBe(false);
  });

  it.each([
    ['credential', true], ['credential', false], ['endpoint', true], ['endpoint', false],
    ['removal', true], ['removal', false], ['auth-rejected', true], ['auth-rejected', false],
    ['enrollment', true], ['enrollment', false], ['detached-credential', true], ['detached-credential', false],
  ] as const)('allows a fresh upload after %s invalidates retained cleanup (teardown first %s)', async (change, teardownFirst) => {
    const f = boot(); f.reply();
    const other = boot('relay-b');
    const otherScope = relayStore.attachmentCleanupScope(other.id);
    const agent = get(relayStore.agents).find((item) => item.relay_id === f.id)!;
    const oldScope = relayStore.attachmentCleanupScope(f.id);
    const old = relayStore.attachmentController(agent);
    const connection = relayStore.connection(f.id)!;
    vi.spyOn(connection.transport!, 'send').mockImplementation((payload) => {
      f.session.sent.push(payload);
      if (payload.type === 'upload_begin') queueMicrotask(() => f.session.handlers.onMessage({ type: 'upload_begin_result',
        request_id: payload.request_id, result: { upload_id: 'invalidated-cleanup', chunk_bytes: 262144,
          expires_at: new Date(Date.now() + 120_000).toISOString(),
          limits: { max_files: 8, max_file_bytes: 20971520, max_batch_bytes: 52428800 } } }));
      if (payload.type === 'upload_chunk') queueMicrotask(() => f.session.handlers.onMessage({ type: 'upload_chunk_result',
        request_id: payload.request_id, error: { code: 'attachment_upload_failed' } }));
      if (payload.type === 'upload_cancel') queueMicrotask(() => f.session.handlers.onMessage({ type: 'upload_cancel_result',
        request_id: payload.request_id, error: { code: 'attachment_cancel_failed' } }));
      return true;
    });
    old.select([new File(['png'], 'shot.png', { type: 'image/png' })]);
    await expect(old.upload()).rejects.toMatchObject({ code: 'attachment_upload_failed' });
    await expect(old.cancel()).rejects.toMatchObject({ code: 'attachment_cancel_failed' });
    if (teardownFirst) {
      relayStore.retainAttachmentController(agent, old, oldScope);
      expect(relayStore.recoverAttachmentController(agent)).toBe(old);
    }
    const relay = get(relayStore.relayConfigs).find((item) => item.id === f.id)!;
    if (change === 'credential' || change === 'detached-credential') {
      if (change === 'detached-credential') relayStore.disconnectRelay(f.id);
      new BrowserDeviceCredentialStore(localStorage).updateCredential(f.id, {
        deviceId: `device-${f.id}`, credentialId: `credential-${f.id}`, credentialVersion: 2, role: 'controller', locale: 'en' });
    }
    if (change === 'endpoint') relay.url = 'wss://changed-scope.example';
    if (change === 'auth-rejected') f.session.handlers.onStatus('closed', { code: 'device_unauthorized' });
    if (change === 'removal') {
      relayStore.removeRelay(f.id);
      boot(f.id);
    } else relayStore.connectRelay(relay);
    const session = sessions.get(f.id)!;
    if (change === 'enrollment') {
      const authentication = session.authentication!;
      authentication.onAuthenticated!(authentication.getAuthentication!()!, {
        deviceId: `device-${f.id}`, credentialId: `credential-${f.id}`, credentialVersion: 2, role: 'controller', locale: 'en' });
    }
    session.handlers.onMessage({ type: 'push_config', protocol: 3, inventory: { state: 'ready', stale: false },
      capabilities: ['inventory_snapshot_v1'] });
    session.handlers.onMessage({ type: 'inventory_snapshot',
      snapshot_request_id: session.sent.findLast((message) => message.snapshot_request_id)!.snapshot_request_id,
      inventory: { state: 'ready', stale: false }, agents: [rawAgent(2)], workspaces: [] });
    expect(relayStore.relayActionsFresh(f.id)).toBe(true);
    // A late destroy callback cannot reinsert the old scope even after fresh recovery.
    relayStore.retainAttachmentController(agent, old, oldScope);
    const current = get(relayStore.agents).find((item) => item.relay_id === f.id)!;
    expect(relayStore.recoverAttachmentController(current)).toBeNull();
    expect(relayStore.attachmentCleanupScope(other.id)).toBe(otherScope);
    expect(relayStore.sendRaw(f.id, { type: 'upload_cancel', target: targetRefForAgent(agent), upload_id: 'invalidated-cleanup' })).toBe(false);
    const fresh = relayStore.attachmentController(current);
    expect(fresh).not.toBe(old);
    vi.spyOn(relayStore.connection(f.id)!.transport!, 'send').mockImplementation((payload) => {
      session.sent.push(payload);
      let result: Record<string, unknown> = {};
      if (payload.type === 'upload_begin') result = { upload_id: 'fresh-scope-upload', chunk_bytes: 262144,
        expires_at: new Date(Date.now() + 120_000).toISOString(),
        limits: { max_files: 8, max_file_bytes: 20971520, max_batch_bytes: 52428800 } };
      if (payload.type === 'upload_chunk') result = { file_index: 0, next_sequence: 1, received_bytes: 3 };
      if (payload.type === 'upload_finish') result = { attachments: [{ ref: 'attachment:fresh-scope', name: 'shot.png',
        media_type: 'image/png', bytes: 3, sha256: (payload.files as Array<{ sha256: string }>)[0].sha256,
        expires_at: new Date(Date.now() + 60_000).toISOString() }] };
      if (['upload_begin', 'upload_chunk', 'upload_finish'].includes(String(payload.type))) {
        queueMicrotask(() => session.handlers.onMessage({ type: `${payload.type}_result`, request_id: payload.request_id, result }));
      }
      return true;
    });
    fresh.select([new File(['png'], 'shot.png', { type: 'image/png' })]);
    await expect(fresh.upload()).resolves.toHaveLength(1);
    expect(session.sent.filter((message) => message.type === 'upload_begin')).toHaveLength(1);
    expect(session.sent.some((message) => message.upload_id === 'invalidated-cleanup')).toBe(false);
    expect(old.hasPendingCleanup()).toBe(true);
    expect(fresh.hasPendingCleanup()).toBe(false);
  });

  it('releases completed upload grants past the 512-grant budget without releasing other resources', async () => {
    const f = boot('relay-a', true, 'controller', ['pane_size_lease', 'pane_realtime_delta']); f.reply();
    const agent = get(relayStore.agents)[0];
    const target = targetRefForAgent(agent);
    const connection = relayStore.connection(f.id)!;
    expect(relayStore.sendRaw(f.id, { type: 'lease_pane_size', target, columns: 80 })).toBe(true);
    const leaseGrant = [...connection.cleanupGrants][0];
    let uploads = 0;
    vi.spyOn(connection.transport!, 'send').mockImplementation((payload) => {
      f.session.sent.push(payload);
      let type = '';
      let result: Record<string, unknown> = {};
      if (payload.type === 'upload_begin') {
        type = 'upload_begin_result';
        result = { upload_id: `completed-${++uploads}`, chunk_bytes: 262144,
          expires_at: new Date(Date.now() + 60_000).toISOString(),
          limits: { max_files: 8, max_file_bytes: 20971520, max_batch_bytes: 52428800 } };
      } else if (payload.type === 'upload_chunk') {
        type = 'upload_chunk_result';
        result = { file_index: 0, next_sequence: 1, received_bytes: 3 };
      } else if (payload.type === 'upload_finish') {
        type = 'upload_finish_result';
        result = { attachments: [{ ref: `attachment:completed-${uploads}`, name: 'shot.png', media_type: 'image/png', bytes: 3,
          sha256: (payload.files as Array<{ sha256: string }>)[0].sha256,
          expires_at: new Date(Date.now() + 60_000).toISOString() }] };
      }
      if (type) queueMicrotask(() => f.session.handlers.onMessage({ type, request_id: payload.request_id, result }));
      return true;
    });
    const file = new File(['png'], 'shot.png', { type: 'image/png' });
    for (let index = 0; index < 513; index++) {
      await expect(relayStore.uploadAttachments(agent, [file])).resolves.toHaveLength(1);
      expect([...connection.cleanupGrants]).toEqual([leaseGrant]);
    }
    expect(uploads).toBe(513);
    expect(relayStore.sendRaw(f.id, { type: 'watch_pane', target })).toBe(true);
    expect(relayStore.sendRaw(f.id, { type: 'lease_pane_size', target, columns: 80 })).toBe(true);
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
