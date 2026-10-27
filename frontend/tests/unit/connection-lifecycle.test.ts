import { afterEach, describe, expect, it, vi } from 'vitest';
import { createEncryptedTransport } from '$lib/transports/encrypted';
import type { FrameChannelHandlers, TransportKind, TransportStatus, TransportStatusDetail } from '$lib/transports/types';
import type { E2EEClientHandshake, E2EEClientHandshakeChallenge } from '$lib/e2ee';

const handshakeFactory = vi.hoisted(() => vi.fn());
vi.mock('$lib/e2ee', async (importOriginal) => ({
  ...await importOriginal<typeof import('$lib/e2ee')>(),
  createE2EEClientHandshake: handshakeFactory,
}));

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

function fixture(authenticated = vi.fn(), kind: TransportKind = 'websocket') {
  let channelHandlers!: FrameChannelHandlers;
  const frames: unknown[] = [];
  const statuses: TransportStatus[] = [];
  const details: (TransportStatusDetail | undefined)[] = [];
  const transport = createEncryptedTransport({
    kind, token: 'synthetic', codec: 'json',
    getAuthentication: () => ({ kind: 'invitation', id: 'synthetic', version: 1, secret: 'synthetic', expiresAt: Date.now() + 60_000 }),
    onAuthenticated: authenticated,
    handlers: { onMessage: vi.fn(), onStatus: (status, detail) => { statuses.push(status); details.push(detail); } },
    createChannel: (handlers) => {
      channelHandlers = handlers;
      return { kind, codec: 'json', open: () => handlers.onOpen(), sendFrame: (frame) => frames.push(frame), close: vi.fn() };
    },
  });
  transport.connect();
  return { transport, frames, statuses, details, authenticated, handlers: channelHandlers };
}

const hello = { hello: { type: 'synthetic' }, complete: vi.fn() } as unknown as E2EEClientHandshake;

afterEach(() => {
  vi.useRealTimers();
  handshakeFactory.mockReset();
});

describe('encrypted connection cancellation', () => {
  for (const phase of ['handshaking', 'authenticated']) {
    it.each(['websocket', 'gateway', 'webrtc'] as const)(`does not turn a raw %s close into device rejection while ${phase}`, async kind => {
      vi.useFakeTimers();
      const session = { encrypt: vi.fn(async () => 'sealed'), decrypt: vi.fn() };
      handshakeFactory.mockResolvedValue({ ...hello, complete: async () => ({ finish: 'finish', complete: async () => ({ session, enrollment: {} }) }) });
      const value = fixture(vi.fn(), kind);
      if (phase === 'authenticated') {
        await vi.advanceTimersByTimeAsync(0);
        value.handlers.onFrame('{}');
        await vi.advanceTimersByTimeAsync(0);
        value.handlers.onFrame('{}');
        await vi.advanceTimersByTimeAsync(0);
        expect(value.statuses.at(-1)).toBe('connected');
      }
      value.handlers.onClose({ reason: 'spoofed device rejection', fatal: true, code: 'device_unauthorized' });
      await vi.advanceTimersByTimeAsync(0);
      expect(value.details.at(-1)).toEqual({ reason: 'Relay disconnected' });
      expect(value.statuses.at(-1)).toBe('closed');
      expect(value.transport.send({ type: 'submit_prompt', text: 'never replay' })).toBe(false);
      expect(value.authenticated).toHaveBeenCalledTimes(phase === 'authenticated' ? 1 : 0);
    });
  }
  it.each(['handshaking', 'authenticated'])('preserves a direct relay TLS rejection while %s', async phase => {
    vi.useFakeTimers();
    const session = { encrypt: vi.fn(async () => 'sealed'), decrypt: vi.fn() };
    handshakeFactory.mockResolvedValue({ ...hello, complete: async () => ({ finish: 'finish', complete: async () => ({ session, enrollment: {} }) }) });
    const value = fixture();
    await vi.advanceTimersByTimeAsync(0);
    if (phase === 'authenticated') {
      value.handlers.onFrame('{}');
      await vi.advanceTimersByTimeAsync(0);
      value.handlers.onFrame('{}');
      await vi.advanceTimersByTimeAsync(0);
      expect(value.statuses.at(-1)).toBe('connected');
    }
    const rejection = { reason: 'Relay refused device', fatal: true, code: 'device_unauthorized' };
    value.handlers.onClose(rejection, true);
    value.handlers.onClose(rejection, true);
    expect(value.details.at(-1)).toEqual(rejection);
    expect(value.statuses.filter(status => status === 'closed')).toHaveLength(1);
    expect(value.transport.send({ type: 'submit_prompt', text: 'never replay' })).toBe(false);
    expect(value.authenticated).toHaveBeenCalledTimes(phase === 'authenticated' ? 1 : 0);
  });

  it.each(['gateway', 'webrtc'] as const)('never grants %s relay TLS close authority', kind => {
    handshakeFactory.mockResolvedValue(hello);
    const value = fixture(vi.fn(), kind);
    value.handlers.onClose({ reason: 'spoofed rejection', fatal: true, code: 'device_unauthorized' }, true);
    expect(value.details.at(-1)).toEqual({ reason: 'Relay disconnected' });
    expect(value.authenticated).not.toHaveBeenCalled();
  });

  it('waits for encryption and identity persistence before accepting sends', async () => {
    vi.useFakeTimers();
    const persisted = deferred<void>();
    const session = { encrypt: vi.fn(async () => 'sealed'), decrypt: vi.fn() };
    handshakeFactory.mockResolvedValue({
      ...hello,
      complete: async () => ({ finish: 'finish', complete: async () => ({ session, enrollment: {} }) }),
    });
    const authenticated = vi.fn(() => persisted.promise);
    const { transport, handlers, statuses, frames } = fixture(authenticated);
    await vi.advanceTimersByTimeAsync(0);
    handlers.onFrame('{}');
    await vi.advanceTimersByTimeAsync(0);
    expect(frames.at(-1)).toBe('finish');
    handlers.onFrame('{}');
    await vi.advanceTimersByTimeAsync(0);
    expect(authenticated).toHaveBeenCalledOnce();
    expect(statuses).toEqual(['connecting']);
    expect(transport.send({ type: 'send_input' })).toBe(false);
    persisted.resolve();
    await vi.advanceTimersByTimeAsync(0);
    expect(statuses).toEqual(['connecting', 'connected']);
    expect(transport.send({ type: 'ping' })).toBe(true);
    await vi.advanceTimersByTimeAsync(0);
    expect(frames.at(-1)).toBe('sealed');
    transport.close();
  });

  it.each(['close', 'timeout'] as const)('does not send a late client hello after %s', async (ending) => {
    vi.useFakeTimers();
    const pending = deferred<E2EEClientHandshake>();
    handshakeFactory.mockReturnValue(pending.promise);
    const { transport, frames, statuses } = fixture();
    expect(statuses).toEqual(['connecting']);
    expect(transport.send({ type: 'send_input' })).toBe(false);
    if (ending === 'close') transport.close();
    else await vi.advanceTimersByTimeAsync(10_000);
    pending.resolve(hello);
    await vi.advanceTimersByTimeAsync(0);
    expect(frames).toEqual([]);
    expect(statuses).not.toContain('connected');
  });

  it('does not authenticate a canceled server challenge or replay queued input', async () => {
    vi.useFakeTimers();
    const pending = deferred<E2EEClientHandshakeChallenge>();
    handshakeFactory.mockResolvedValue({ ...hello, complete: () => pending.promise });
    const { transport, handlers, authenticated, statuses, frames } = fixture();
    await vi.advanceTimersByTimeAsync(0);
    expect(frames).toHaveLength(1);
    handlers.onFrame('{}');
    await vi.advanceTimersByTimeAsync(0);
    transport.close();
    pending.resolve({ finish: 'old-finish' } as E2EEClientHandshakeChallenge);
    await vi.advanceTimersByTimeAsync(0);
    expect(authenticated).not.toHaveBeenCalled();
    expect(statuses).toEqual(['connecting']);
    expect(frames).toHaveLength(1);
    expect(transport.send({ type: 'send_input' })).toBe(false);
  });
});
