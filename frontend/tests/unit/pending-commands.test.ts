import { get } from 'svelte/store';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { relayStore, CommandError } from '$lib/store';
import type { RelayTransport } from '$lib/transports';
import { MockWebSocket, connectRelay, makeAgent } from '../support/relay';

const ACTION_ID = 'pending_action_001';
const REFUSED = { code: 'command_refused' };

type Port = '8871' | '8872';

function endpoint(port: Port = '8871') {
  const relay = get(relayStore.relayConfigs).find(row => new URL(row.url).port === port)!;
  const socket = MockWebSocket.instances.filter(row => new URL(row.url).port === port).at(-1)!;
  return { relay, socket };
}

function track<T>(promise: Promise<T>) {
  const state = { outcomes: [] as ('resolved' | 'rejected')[], value: undefined as any, error: undefined as any };
  promise.then(
    value => { state.outcomes.push('resolved'); state.value = value; },
    error => { state.outcomes.push('rejected'); state.error = error; },
  );
  return state;
}

const settle = () => vi.advanceTimersByTimeAsync(0);

function sendCommand(options: {
  port?: Port; type?: string; actionId?: string | null; timeoutMs?: number; signal?: AbortSignal;
} = {}) {
  const { port = '8871', type = 'tab_reorder', actionId = ACTION_ID, timeoutMs, signal } = options;
  const { relay, socket } = endpoint(port);
  const before = socket.sent.length;
  const payload = { type, pane_id: 'w1:p1', insert_index: 2, ...(actionId ? { intent: { action_id: actionId } } : {}) };
  const outcome = track(relayStore.sendCommand(relay.id, payload, timeoutMs, false, signal));
  const frames = socket.messages().slice(before);
  return { relay, socket, outcome, frames, requestId: frames[0]?.request_id as string, type };
}
type Sent = ReturnType<typeof sendCommand>;

function receipt(requestId: string | undefined, phase: string, extra: Record<string, unknown> = {}, error?: unknown) {
  return {
    type: 'action_receipt',
    ...(requestId === undefined ? {} : { request_id: requestId }),
    receipt: { action_id: ACTION_ID, phase, ...(error === undefined ? {} : { error }) },
    ...extra,
  };
}

function legacy(command: Sent, fields: Record<string, unknown>) {
  return { type: 'command_result', request_id: command.requestId, action: command.type, ...fields };
}

function refuseFrames(port: Port, type: string) {
  const { relay } = endpoint(port);
  const transport = relayStore.connection(relay.id)!.transport!;
  const send = transport.send.bind(transport);
  const refused: Record<string, any>[] = [];
  vi.spyOn(transport, 'send').mockImplementation((frame: Parameters<RelayTransport['send']>[0]) => {
    if (frame.type !== type) return send(frame);
    refused.push(frame);
    return false;
  });
  return refused;
}

beforeEach(() => {
  vi.useFakeTimers();
  MockWebSocket.instances = [];
  vi.stubGlobal('WebSocket', MockWebSocket);
  localStorage.clear();
  sessionStorage.clear();
  relayStore.destroy();
  relayStore.setHidden(false);
  relayStore.relayConfigs.set([]);
  relayStore.addRelay({ label: 'Owner', url: 'ws://127.0.0.1:8871', token: '' });
  relayStore.addRelay({ label: 'Other', url: 'ws://127.0.0.1:8872', token: '' });
  for (const socket of MockWebSocket.instances.slice(-2)) connectRelay({ capabilities: [] }, socket);
});

afterEach(() => {
  vi.restoreAllMocks();
  relayStore.destroy();
  relayStore.setHidden(false);
  relayStore.relayConfigs.set([]);
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('how a command ends', () => {
  const endings: {
    name: string;
    reply: (id: string) => Record<string, unknown>;
    resolved?: Record<string, unknown>;
    rejected?: { message: string; data: Record<string, unknown> };
  }[] = [
    {
      name: 'confirmed receipt',
      reply: id => receipt(id, 'confirmed'),
      resolved: { ok: true, phase: 'confirmed', data: { receipt: { action_id: ACTION_ID, phase: 'confirmed' } } },
    },
    {
      name: 'failed_before_dispatch receipt',
      reply: id => receipt(id, 'failed_before_dispatch', {}, REFUSED),
      rejected: {
        message: 'command_refused',
        data: { phase: 'failed_before_dispatch', not_started: true, api_error: REFUSED },
      },
    },
    {
      name: 'dispatched_unknown receipt',
      reply: id => receipt(id, 'dispatched_unknown'),
      rejected: { message: 'Command failed', data: { phase: 'dispatched_unknown', dispatched_unknown: true } },
    },
    {
      name: 'error frame',
      reply: id => ({ type: 'error', request_id: id, error: REFUSED }),
      rejected: { message: 'command_refused', data: { phase: 'failed_before_dispatch', api_error: REFUSED } },
    },
    {
      name: 'legacy ok result',
      reply: id => ({ type: 'command_result', request_id: id, ok: true, data: { exact: 1 } }),
      resolved: { ok: true, data: { exact: 1 } },
    },
    {
      name: 'legacy failed result',
      reply: id => ({ type: 'command_result', request_id: id, ok: false, error: 'legacy failure', data: { exact: 2 } }),
      rejected: { message: 'legacy failure', data: { exact: 2 } },
    },
    {
      name: 'legacy not_started result',
      reply: id => ({
        type: 'command_result', request_id: id, ok: false, phase: 'not_started', error: 'not started', data: { exact: 3 },
      }),
      rejected: { message: 'not started', data: { exact: 3, not_started: true } },
    },
    {
      name: 'legacy dispatched_unknown result',
      reply: id => ({
        type: 'command_result', request_id: id, ok: false, phase: 'dispatched_unknown', error: 'uncertain', data: { exact: 4 },
      }),
      rejected: { message: 'uncertain', data: { exact: 4, dispatched_unknown: true } },
    },
  ];

  it.each(endings)('$name settles once and releases its timer', async ({ reply, resolved, rejected }) => {
    const baseline = vi.getTimerCount();
    const controller = new AbortController();
    const command = sendCommand({ signal: controller.signal });
    expect(command.frames).toHaveLength(1);
    expect(vi.getTimerCount()).toBe(baseline + 1);

    command.socket.message(reply(command.requestId));
    await settle();
    if (resolved) {
      expect(command.outcome.outcomes).toEqual(['resolved']);
      expect(command.outcome.value).toMatchObject(resolved);
    } else {
      expect(command.outcome.outcomes).toEqual(['rejected']);
      expect(command.outcome.error).toBeInstanceOf(CommandError);
      expect(command.outcome.error.message).toBe(rejected!.message);
      expect(command.outcome.error.data).toEqual(rejected!.data);
    }
    const first = resolved ? command.outcome.value : command.outcome.error;
    expect(vi.getTimerCount()).toBe(baseline);

    command.socket.message(reply(command.requestId));
    command.socket.message({ type: 'error', request_id: command.requestId, error: { code: 'late_error' } });
    command.socket.message(legacy(command, { ok: false, error: 'late failure' }));
    controller.abort();
    await vi.advanceTimersByTimeAsync(60_001);
    expect(command.outcome.outcomes).toHaveLength(1);
    expect(resolved ? command.outcome.value : command.outcome.error).toBe(first);
    expect(vi.getTimerCount()).toBe(baseline);
  });
});

describe('command error detail text', () => {
  const details: { name: string; detail: unknown; accepted: boolean }[] = [
    { name: 'short string', detail: 'exact wire detail', accepted: true },
    { name: 'string of exactly 512 bytes', detail: '界'.repeat(170) + 'ab', accepted: true },
    { name: 'string of 513 bytes', detail: 'a'.repeat(513), accepted: false },
    { name: 'number', detail: 42, accepted: false },
    { name: 'record', detail: { text: 'wire' }, accepted: false },
  ];
  const carriers: { name: string; reply: (id: string, detail: unknown) => Record<string, unknown>; fallback: string }[] = [
    {
      name: 'error frame',
      reply: (id, detail) => ({ type: 'error', request_id: id, error: REFUSED, detail }),
      fallback: 'command_refused',
    },
    {
      name: 'receipt with an error code',
      reply: (id, detail) => receipt(id, 'failed_before_dispatch', { detail }, REFUSED),
      fallback: 'command_refused',
    },
    {
      name: 'receipt without an error code',
      reply: (id, detail) => receipt(id, 'dispatched_unknown', { detail }),
      fallback: 'Command failed',
    },
  ];

  it.each(carriers.flatMap(carrier => details.map(detail => ({ carrier, ...detail }))))(
    '$carrier.name: $name',
    async ({ carrier, detail, accepted }) => {
      const command = sendCommand();
      command.socket.message(carrier.reply(command.requestId, detail));
      await settle();
      expect(command.outcome.outcomes).toEqual(['rejected']);
      expect(command.outcome.error.message).toBe(accepted ? detail : carrier.fallback);
    },
  );
});

describe('command deadlines', () => {
  const renewals: { name: string; reply: (id: string) => Record<string, unknown> }[] = [
    { name: 'prepared receipt', reply: id => receipt(id, 'prepared') },
    { name: 'awaiting_evidence receipt', reply: id => receipt(id, 'awaiting_evidence') },
    {
      name: 'legacy accepted result',
      reply: id => ({ type: 'command_result', request_id: id, action: 'tab_reorder', ok: true, phase: 'accepted' }),
    },
  ];

  it.each(renewals)('$name replaces the original deadline with a 10 second one', async ({ reply }) => {
    const command = sendCommand({ timeoutMs: 1_000 });
    await vi.advanceTimersByTimeAsync(900);
    command.socket.message(reply(command.requestId));
    await vi.advanceTimersByTimeAsync(9_999);
    expect(command.outcome.outcomes).toEqual([]);
    await vi.advanceTimersByTimeAsync(1);
    expect(command.outcome.outcomes).toEqual(['rejected']);
    expect(command.outcome.error).toBeInstanceOf(CommandError);
    expect(command.outcome.error.message).toBe('Relay confirmation timed out');
    expect(command.outcome.error.data).toEqual({ dispatched_unknown: true });
  });

  it.each(renewals)('a terminal reply still resolves after $name', async ({ reply }) => {
    const command = sendCommand({ timeoutMs: 1_000 });
    command.socket.message(reply(command.requestId));
    await vi.advanceTimersByTimeAsync(5_000);
    expect(command.outcome.outcomes).toEqual([]);
    command.socket.message(receipt(command.requestId, 'confirmed'));
    await settle();
    expect(command.outcome.outcomes).toEqual(['resolved']);
  });

  it('an accepted agent_start keeps its original deadline', async () => {
    const command = sendCommand({ type: 'agent_start' });
    await vi.advanceTimersByTimeAsync(55_000);
    command.socket.message({
      type: 'command_result', request_id: command.requestId, action: 'agent_start', ok: true, phase: 'accepted',
    });
    await vi.advanceTimersByTimeAsync(4_999);
    expect(command.outcome.outcomes).toEqual([]);
    await vi.advanceTimersByTimeAsync(1);
    expect(command.outcome.outcomes).toEqual(['rejected']);
    expect(command.outcome.error.data).toEqual({ dispatched_unknown: true });
  });
});

describe('reply correlation', () => {
  const idForms: { name: string; fields: Record<string, unknown> }[] = [
    { name: 'missing', fields: {} },
    { name: 'non-string', fields: { request_id: 42 } },
    { name: 'unknown', fields: { request_id: 'no-such-request' } },
  ];

  it.each(idForms)('a receipt with a $name request_id settles the command with its action ID on its own relay only', async ({ fields }) => {
    const owner = sendCommand();
    const other = sendCommand({ port: '8872' });
    const frame = { ...receipt(undefined, 'confirmed'), ...fields };

    owner.socket.message(frame);
    await settle();
    expect(owner.outcome.outcomes).toEqual(['resolved']);
    expect(other.outcome.outcomes).toEqual([]);

    owner.socket.message(frame);
    await settle();
    expect(other.outcome.outcomes).toEqual([]);

    other.socket.message(frame);
    await settle();
    expect(other.outcome.outcomes).toEqual(['resolved']);
  });

  const wrongRelayFrames: { name: string; frame: (command: Sent) => Record<string, unknown> }[] = [
    { name: 'receipt with its request_id', frame: command => receipt(command.requestId, 'confirmed') },
    { name: 'receipt without request_id', frame: () => receipt(undefined, 'confirmed') },
    { name: 'error frame', frame: command => ({ type: 'error', request_id: command.requestId, error: REFUSED }) },
    { name: 'legacy result', frame: command => legacy(command, { ok: true }) },
  ];

  it.each(wrongRelayFrames)('a $name arriving on the other relay changes nothing', async ({ frame }) => {
    const command = sendCommand();
    endpoint('8872').socket.message(frame(command));
    await vi.advanceTimersByTimeAsync(100);
    expect(command.outcome.outcomes).toEqual([]);

    command.socket.message(frame(command));
    await settle();
    expect(command.outcome.outcomes).toHaveLength(1);
  });

  it.each([
    { name: 'without a request_id', fields: {} },
    { name: 'with an unknown request_id', fields: { request_id: 'no-such-request' } },
  ])('a receipt for an unrelated action ID $name changes nothing', async ({ fields }) => {
    const command = sendCommand();
    const unrelated = { ...receipt(undefined, 'confirmed'), ...fields };
    unrelated.receipt = { action_id: 'unrelated_action_9', phase: 'confirmed' };
    command.socket.message(unrelated);
    await vi.advanceTimersByTimeAsync(100);
    expect(command.outcome.outcomes).toEqual([]);

    command.socket.message(receipt(command.requestId, 'confirmed'));
    await settle();
    expect(command.outcome.outcomes).toEqual(['resolved']);
  });

  it('ignores malformed receipts and error frames', async () => {
    const command = sendCommand();
    const id = command.requestId;
    const malformed: Record<string, unknown>[] = [
      { type: 'action_receipt', request_id: id },
      { type: 'action_receipt', request_id: id, receipt: 'confirmed' },
      { type: 'action_receipt', request_id: id, receipt: { action_id: 'short', phase: 'confirmed' } },
      { type: 'action_receipt', request_id: id, receipt: { action_id: ACTION_ID, phase: 'finished' } },
      receipt(id, 'failed_before_dispatch', {}, { code: 'BAD CODE' }),
      { type: 'error', error: REFUSED },
      { type: 'error', request_id: 42, error: REFUSED },
      { type: 'error', request_id: id },
      { type: 'error', request_id: id, error: null },
      { type: 'error', request_id: id, error: { code: 'BAD CODE' } },
    ];
    for (const frame of malformed) command.socket.message(frame);
    await vi.advanceTimersByTimeAsync(100);
    expect(command.outcome.outcomes).toEqual([]);

    command.socket.message(receipt(id, 'confirmed'));
    await settle();
    expect(command.outcome.outcomes).toEqual(['resolved']);
  });

  const firstEndings: { name: string; reply: (id: string) => Record<string, unknown> }[] = [
    { name: 'confirmed receipt', reply: id => receipt(id, 'confirmed') },
    { name: 'failed receipt', reply: id => receipt(id, 'failed_before_dispatch', {}, REFUSED) },
    { name: 'error frame', reply: id => ({ type: 'error', request_id: id, error: REFUSED }) },
    { name: 'legacy ok result', reply: id => ({ type: 'command_result', request_id: id, ok: true }) },
    { name: 'legacy failed result', reply: id => ({ type: 'command_result', request_id: id, ok: false, error: 'failure' }) },
  ];

  it.each(firstEndings)('a command settled by a $name leaves nothing that can take a newer command\'s receipt', async ({ reply }) => {
    const first = sendCommand();
    first.socket.message(reply(first.requestId));
    await settle();
    expect(first.outcome.outcomes).toHaveLength(1);

    const second = sendCommand();
    expect(second.requestId).not.toBe(first.requestId);
    second.socket.message(receipt(undefined, 'confirmed'));
    await settle();
    expect(second.outcome.outcomes).toEqual(['resolved']);
    expect(first.outcome.outcomes).toHaveLength(1);
  });

  it.each(firstEndings)('a late reply to a command settled by a $name cannot settle a newer one', async ({ reply }) => {
    const first = sendCommand();
    first.socket.message(reply(first.requestId));
    await settle();

    const second = sendCommand();
    second.socket.message({ type: 'error', request_id: first.requestId, error: REFUSED });
    second.socket.message(legacy(first, { ok: false, error: 'late failure' }));
    second.socket.message(legacy(first, { ok: true }));
    second.socket.message({ ...receipt(first.requestId, 'confirmed'), receipt: { action_id: 'unrelated_action_9', phase: 'confirmed' } });
    await vi.advanceTimersByTimeAsync(100);
    expect(second.outcome.outcomes).toEqual([]);

    second.socket.message(receipt(second.requestId, 'confirmed'));
    await settle();
    expect(second.outcome.outcomes).toEqual(['resolved']);
  });
});

describe('aborted and unsent commands', () => {
  it('aborting after the send rejects as cancelled and ignores later replies', async () => {
    const baseline = vi.getTimerCount();
    const controller = new AbortController();
    const command = sendCommand({ signal: controller.signal });
    expect(vi.getTimerCount()).toBe(baseline + 1);

    controller.abort();
    await settle();
    expect(command.outcome.outcomes).toEqual(['rejected']);
    expect(command.outcome.error).toBeInstanceOf(CommandError);
    expect(command.outcome.error.code).toBe('request_cancelled');
    expect(vi.getTimerCount()).toBe(baseline);

    command.socket.message(receipt(command.requestId, 'confirmed'));
    command.socket.message(legacy(command, { ok: true }));
    await vi.advanceTimersByTimeAsync(60_001);
    expect(command.outcome.outcomes).toEqual(['rejected']);
    expect(command.outcome.error.code).toBe('request_cancelled');
    expect(vi.getTimerCount()).toBe(baseline);
  });

  it('an already aborted signal sends no frame', async () => {
    const baseline = vi.getTimerCount();
    const controller = new AbortController();
    controller.abort();
    const command = sendCommand({ signal: controller.signal });
    await settle();
    expect(command.frames).toEqual([]);
    expect(command.outcome.outcomes).toEqual(['rejected']);
    expect(command.outcome.error.code).toBe('request_cancelled');
    expect(vi.getTimerCount()).toBe(baseline);
  });

  it('a refused frame rejects as a plain error and ignores later replies', async () => {
    const baseline = vi.getTimerCount();
    const refused = refuseFrames('8871', 'tab_reorder');
    const controller = new AbortController();
    const command = sendCommand({ signal: controller.signal });
    await settle();
    expect(refused).toHaveLength(1);
    expect(command.outcome.outcomes).toEqual(['rejected']);
    expect(command.outcome.error.constructor).toBe(CommandError);
    expect(command.outcome.error.message).toBe('Could not send command to relay');
    expect(command.outcome.error.data).toBeUndefined();
    expect(command.outcome.error.code).toBeUndefined();
    expect(vi.getTimerCount()).toBe(baseline);

    const requestId = refused[0].request_id as string;
    command.socket.message(receipt(requestId, 'confirmed'));
    command.socket.message({ type: 'command_result', request_id: requestId, ok: true });
    controller.abort();
    await vi.advanceTimersByTimeAsync(60_001);
    expect(command.outcome.outcomes).toEqual(['rejected']);
    expect(command.outcome.error.message).toBe('Could not send command to relay');
    expect(vi.getTimerCount()).toBe(baseline);
  });
});

describe('uploads', () => {
  const beginResult = () => ({
    upload_id: 'upload-1',
    chunk_bytes: 3,
    expires_at: new Date(Date.now() + 60_000).toISOString(),
    limits: { max_files: 8, max_file_bytes: 20 * 1024 * 1024, max_batch_bytes: 50 * 1024 * 1024 },
  });

  function batch(port: Port = '8871', content = 'png') {
    const { relay } = endpoint(port);
    const controller = relayStore.attachmentController(makeAgent({
      relay_id: relay.id,
      relay_label: relay.label,
      raw_pane_id: 'w1:p1',
      pane_id: `${relay.id}::w1:p1`,
    }));
    controller.select([new File([content], 'shot.png', { type: 'image/png' })]);
    return controller;
  }

  const framesOf = (type: string, port: Port = '8871') =>
    endpoint(port).socket.messages().filter(frame => frame.type === type);

  it('a begin frame the socket refuses fails the batch and a later begin result is ignored', async () => {
    const refused = refuseFrames('8871', 'upload_begin');
    const controller = batch();
    const upload = track(controller.upload());
    await settle();
    expect(refused).toHaveLength(1);
    expect(upload.outcomes).toEqual(['rejected']);
    expect(upload.error).toEqual({ code: 'attachment_upload_failed' });
    const failed = controller.snapshot();
    expect(failed).toMatchObject({ uploading: false, items: [{ state: 'interrupted' }] });

    endpoint().socket.message({ type: 'upload_begin_result', request_id: refused[0].request_id, result: beginResult() });
    await vi.advanceTimersByTimeAsync(100);
    expect(controller.snapshot()).toEqual(failed);
    expect(framesOf('upload_chunk')).toEqual([]);
    expect(upload.outcomes).toEqual(['rejected']);
  });

  it('ignores a result of the wrong response type for a pending request', async () => {
    const controller = batch();
    const upload = track(controller.upload());
    const begin = framesOf('upload_begin')[0];

    endpoint().socket.message({ type: 'upload_chunk_result', request_id: begin.request_id, result: beginResult() });
    endpoint().socket.message({ type: 'upload_finish_result', request_id: begin.request_id, error: { code: 'attachment_upload_busy' } });
    await vi.advanceTimersByTimeAsync(100);
    expect(upload.outcomes).toEqual([]);
    expect(framesOf('upload_chunk')).toEqual([]);

    endpoint().socket.message({ type: 'upload_begin_result', request_id: begin.request_id, result: beginResult() });
    await vi.waitFor(() => expect(framesOf('upload_chunk')).toHaveLength(1));
    expect(framesOf('upload_chunk')[0]).toMatchObject({ upload_id: 'upload-1', sequence: 0 });
  });

  it('cancelling while the begin reply is outstanding settles only on the matching cancel result', async () => {
    const controller = batch();
    const upload = track(controller.upload());
    const begin = framesOf('upload_begin')[0];
    const cancel = track(controller.cancel());
    await settle();
    expect(framesOf('upload_cancel')).toEqual([]);

    endpoint().socket.message({ type: 'upload_begin_result', request_id: begin.request_id, result: beginResult() });
    await vi.waitFor(() => expect(framesOf('upload_cancel')).toHaveLength(1));
    const [frame] = framesOf('upload_cancel');
    expect(frame).toMatchObject({ upload_id: 'upload-1' });
    expect(framesOf('upload_chunk')).toEqual([]);
    expect(upload.outcomes).toEqual(['resolved']);
    expect(upload.value).toEqual([]);

    endpoint('8872').socket.message({ type: 'upload_cancel_result', request_id: frame.request_id, result: {} });
    endpoint().socket.message({ type: 'upload_begin_result', request_id: frame.request_id, result: {} });
    endpoint().socket.message({ type: 'upload_cancel_result', request_id: 'unrelated_request', result: {} });
    await vi.advanceTimersByTimeAsync(100);
    expect(cancel.outcomes).toEqual([]);

    endpoint().socket.message({ type: 'upload_cancel_result', request_id: frame.request_id, result: {} });
    await settle();
    expect(cancel.outcomes).toEqual(['resolved']);
    expect(controller.snapshot()).toMatchObject({ items: [], uploading: false });
  });

  it('a refused cancel frame surfaces the controller failure and sends no further chunk', async () => {
    const controller = batch('8871', 'pngpng');
    const upload = track(controller.upload());
    const begin = framesOf('upload_begin')[0];
    endpoint().socket.message({ type: 'upload_begin_result', request_id: begin.request_id, result: beginResult() });
    await vi.waitFor(() => expect(framesOf('upload_chunk')).toHaveLength(1));
    const [chunk] = framesOf('upload_chunk');
    const refused = refuseFrames('8871', 'upload_cancel');

    const cancel = track(controller.cancel());
    await settle();
    expect(refused).toHaveLength(1);
    expect(cancel.outcomes).toEqual(['rejected']);
    expect(cancel.error).toEqual({ code: 'attachment_cancel_failed' });
    expect(upload.outcomes).toEqual(['resolved']);
    expect(upload.value).toEqual([]);
    expect(controller.snapshot()).toMatchObject({ items: [], uploading: false });

    endpoint().socket.message({
      type: 'upload_chunk_result',
      request_id: chunk.request_id,
      result: { file_index: 0, next_sequence: 1, received_bytes: 3 },
    });
    endpoint().socket.message({ type: 'upload_cancel_result', request_id: refused[0].request_id, result: {} });
    await vi.advanceTimersByTimeAsync(100);
    expect(framesOf('upload_chunk')).toHaveLength(1);
    expect(framesOf('upload_finish')).toEqual([]);
    expect(cancel.outcomes).toEqual(['rejected']);
  });
});

describe('a relay going away', () => {
  it.each(['closed', 'disconnect', 'retire'] as const)(
    '%s rejects its own pending work as dispatched_unknown and leaves the other relay alone',
    async mode => {
      const owner = endpoint();
      const other = endpoint('8872');
      if (mode === 'retire') {
        relayStore.setHidden(true);
        await vi.advanceTimersByTimeAsync(120_000);
        other.socket.message({ type: 'pong' });
      }
      const command = sendCommand({ timeoutMs: 60_000 });
      const survivor = sendCommand({ port: '8872', timeoutMs: 60_000 });
      const controller = relayStore.attachmentController(makeAgent({
        relay_id: owner.relay.id,
        relay_label: owner.relay.label,
        raw_pane_id: 'w1:p1',
        pane_id: `${owner.relay.id}::w1:p1`,
      }));
      controller.select([new File(['png'], 'shot.png', { type: 'image/png' })]);
      const upload = track(controller.upload());

      if (mode === 'closed') owner.socket.serverClose();
      else if (mode === 'disconnect') relayStore.disconnectRelay(owner.relay.id);
      else await vi.advanceTimersByTimeAsync(10_000);
      await settle();

      expect(command.outcome.outcomes).toEqual(['rejected']);
      expect(command.outcome.error.message).toBe('Relay disconnected');
      expect(command.outcome.error.data).toEqual({ dispatched_unknown: true });
      expect(upload.outcomes).toEqual(['rejected']);
      expect(upload.error).toEqual({ code: 'attachment_upload_failed' });
      expect(survivor.outcome.outcomes).toEqual([]);

      owner.socket.message(receipt(command.requestId, 'confirmed'));
      await vi.advanceTimersByTimeAsync(100);
      expect(command.outcome.outcomes).toEqual(['rejected']);
      expect(survivor.outcome.outcomes).toEqual([]);

      other.socket.message(legacy(survivor, { ok: true }));
      await settle();
      expect(survivor.outcome.outcomes).toEqual(['resolved']);
    },
  );
});
