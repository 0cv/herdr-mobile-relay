import {
  createE2EEClientHandshake,
  type E2EECodec,
  type E2EEClientHandshake,
  type E2EEClientHandshakeChallenge,
  type E2EESession,
  type E2EEWireFrame,
} from '../e2ee';
import type {
  DeviceEnrollmentResult,
  RelayDeviceCredential,
  RelayInvitation,
} from '../device-auth';
import {
  type FrameChannel,
  type FrameChannelFactory,
  type RelayTransport,
  type TransportHandlers,
  type TransportKind,
  type TransportStatusDetail,
} from './types';

export const E2EE_HANDSHAKE_TIMEOUT_MS = 10_000;

export interface EncryptedTransportOptions {
  kind: TransportKind;
  /** Relay bootstrap/rendezvous key. Empty enables tokenless loopback development. */
  token: string;
  /** Resolves the current credential when this path actually opens. */
  getAuthentication?: () => RelayDeviceCredential | RelayInvitation | undefined;
  /**
   * Persists authenticated identity before the connection becomes visible.
   * Invitation handlers must atomically replace the invitation with the issued
   * credential here.
   */
  onAuthenticated?: (
    authentication: RelayDeviceCredential | RelayInvitation,
    enrollment: DeviceEnrollmentResult,
  ) => void | Promise<void>;
  codec: E2EECodec;
  createChannel: FrameChannelFactory;
  handlers: TransportHandlers;
  handshakeTimeoutMs?: number;
}

export type TransportAuthentication = Pick<
  EncryptedTransportOptions,
  'getAuthentication' | 'onAuthenticated'
>;

/**
 * Wraps a raw frame channel in the Herdr E2EE session and the JSON message
 * layer. Every path — browser WebSocket, gateway-relayed, WebRTC DataChannel —
 * runs its own independent handshake through this one implementation, so the
 * authorization model and replay protection are identical on all of them.
 */
export function createEncryptedTransport(options: EncryptedTransportOptions): RelayTransport {
  const {
    kind,
    token,
    codec,
    handlers,
  } = options;
  let encrypted = false;
  const handshakeTimeoutMs = options.handshakeTimeoutMs ?? E2EE_HANDSHAKE_TIMEOUT_MS;

  let channel: FrameChannel | null = null;
  let handshake: E2EEClientHandshake | null = null;
  let challenge: E2EEClientHandshakeChallenge | null = null;
  let session: E2EESession | null = null;
  let presentedAuthentication: RelayDeviceCredential | RelayInvitation | undefined;
  let handshakeTimer: ReturnType<typeof setTimeout> | null = null;
  let sendQueue: Promise<void> = Promise.resolve();
  let receiveQueue: Promise<void> = Promise.resolve();
  let ready = false;
  let finished = false;

  function clearHandshakeTimer(): void {
    if (handshakeTimer === null) return;
    clearTimeout(handshakeTimer);
    handshakeTimer = null;
  }

  function finish(detail?: TransportStatusDetail): void {
    if (finished) return;
    finished = true;
    ready = false;
    handshake = null;
    challenge = null;
    session = null;
    clearHandshakeTimer();
    channel?.close();
    handlers.onStatus('closed', detail);
  }

  function markReady(): void {
    if (finished || ready) return;
    ready = true;
    clearHandshakeTimer();
    handlers.onStatus('connected', { path: kind });
  }

  function deliver(frame: E2EEWireFrame): void {
    // The tokenless loopback path runs no crypto, so it must stay synchronous:
    // the promise hop would only defer plaintext delivery by a microtask.
    if (!encrypted) {
      if (finished) return;
      let message: Record<string, any>;
      try {
        message = JSON.parse(String(frame)) as Record<string, any>;
      } catch {
        return; // Ignore malformed plaintext frames used by loopback development.
      }
      handlers.onMessage(message);
      return;
    }
    receiveQueue = receiveQueue.then(async () => {
      if (finished) return;
      if (!session) {
        if (challenge) {
          if (!presentedAuthentication) throw new Error('No device authentication is available for this relay.');
          const completed = await challenge.complete(frame);
          if (finished) return;
          await options.onAuthenticated?.(presentedAuthentication, completed.enrollment);
          session = completed.session;
          challenge = null;
          markReady();
          return;
        }
        if (!handshake) throw new Error('Encrypted server hello arrived before the client hello.');
        challenge = await handshake.complete(JSON.parse(String(frame)));
        if (finished) return;
        handshake = null;
        channel?.sendFrame(challenge.finish);
        return;
      }
      const plaintext = await session.decrypt(frame);
      if (finished) return;
      handlers.onMessage(JSON.parse(plaintext) as Record<string, any>);
    }).catch((error: unknown) => {
      const reason = error instanceof Error && error.message
        ? error.message
        : 'Encrypted relay connection failed';
      finish({ reason });
    });
  }

  return {
    kind,
    connect(): void {
      if (channel || finished) return;
      handlers.onStatus('connecting');
      presentedAuthentication = options.getAuthentication?.();
      encrypted = Boolean(token || presentedAuthentication);
      channel = options.createChannel({
        onOpen(): void {
          if (finished) return;
          if (!encrypted) {
            markReady();
            return;
          }
          if (!presentedAuthentication) {
            finish({ reason: 'Pair this browser before connecting to the relay' });
            return;
          }
          handshakeTimer = setTimeout(() => {
            finish({ reason: 'Encrypted relay handshake timed out' });
          }, handshakeTimeoutMs);
          void createE2EEClientHandshake(presentedAuthentication, undefined, codec).then((created) => {
            handshake = created;
            channel?.sendFrame(JSON.stringify(created.hello));
          }).catch(() => {
            finish({ reason: 'Could not start encrypted relay handshake' });
          });
        },
        onFrame: deliver,
        onClose(detail?: TransportStatusDetail): void {
          finish(detail ?? { reason: 'Relay disconnected' });
        },
      }, encrypted);
      channel.open();
    },
    send(payload: Record<string, unknown>): boolean {
      if (!ready || finished || !channel) return false;
      const plaintext = JSON.stringify(payload);
      if (!encrypted) {
        channel.sendFrame(plaintext);
        return true;
      }
      const active = session;
      const activeChannel = channel;
      if (!active) return false;
      sendQueue = sendQueue.then(async () => {
        const frame = await active.encrypt(plaintext);
        if (finished || channel !== activeChannel) return;
        activeChannel.sendFrame(frame);
      }).catch(() => {
        finish({ reason: 'Could not encrypt relay message' });
      });
      return true;
    },
    close(): void {
      if (finished) {
        channel?.close();
        return;
      }
      finished = true;
      ready = false;
      handshake = null;
      challenge = null;
      session = null;
      clearHandshakeTimer();
      channel?.close();
    },
  };
}
