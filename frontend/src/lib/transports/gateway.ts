import { base64UrlDecode, base64UrlEncode } from '../base64url';
import { connectProof, deriveRelayId } from '../gateway-credentials';
import type { E2EEWireFrame } from '../e2ee';
import type { RelayConfig } from '../types';
import { chunk, decodeWireFrame, encodeWireFrame, Reassembler } from './chunking';
import { createEncryptedTransport, type TransportAuthentication } from './encrypted';
import type {
  FrameChannel,
  FrameChannelHandlers,
  RelayTransport,
  TransportHandlers,
} from './types';

/** Gateway protocol version carried in every hello message. */
export const GATEWAY_PROTO = 1;
/**
 * Chunk ceiling on the relay link. The gateway copies one phone frame into one
 * multiplexed payload, so chunks stay well inside `gatewaywire.MaxFramePayload`
 * and a shared gateway never has to buffer a whole upload per client.
 */
export const GATEWAY_MAX_CHUNK_BYTES = 262_144;
/** A silent primary must yield to the next configured gateway promptly. */
export const GATEWAY_HANDSHAKE_TIMEOUT_MS = 10_000;

/**
 * Error codes that will keep failing with the same relay configuration. The
 * path manager stops retrying these instead of backing off forever.
 */
const FATAL_ERROR_CODES: Record<string, true> = { unknown_relay: true, quota_exceeded: true };

const ERROR_MESSAGES: Record<string, string> = {
  unknown_relay: 'That computer is not connected to the gateway.',
  quota_exceeded: 'The gateway relay quota for that computer is used up.',
  rate_limited: 'The gateway is rate limiting this device.',
  too_many_clients: 'That computer already has too many gateway connections.',
  // Deliberately not fatal: a full gateway empties as sessions upgrade to the
  // direct path or end, so retrying is the right instinct and switching
  // transport is the escape hatch.
  at_capacity: 'The gateway is full right now. Try again shortly, or switch to another connection method.',
  relay_busy: 'That computer is busy on the gateway.',
  bad_hello: 'The gateway rejected the connection request.',
  internal: 'The gateway hit an internal error.',
};

/**
 * Reads the STUN port the gateway advertises in its hello. Only a real port
 * number switches address discovery on: absent, zero, out of range, or not a
 * number at all leaves this phone on host candidates plus whatever the relay's
 * port mapping produced, which is exactly the behaviour without discovery.
 */
function advertisedStunPort(hello: Record<string, unknown>): number {
  const port = hello.stun_port;
  if (typeof port !== 'number' || !Number.isInteger(port) || port < 1 || port > 65535) return 0;
  return port;
}

/** Seams for the relayed channel; the transport below fills them in. */
export interface GatewayChannelOptions {
  /**
   * Called with the gateway's advertised STUN port once the hello parses. Only
   * the port ever travels: the caller pairs it with the gateway host it dialed
   * itself, so a gateway cannot point this phone at a third-party server.
   */
  onStunPort?(port: number): void;
}

/**
 * The blind relayed path. The gateway performs a challenge-response exchange
 * it cannot itself verify: it hands us a nonce, we answer with an HMAC over
 * the rendezvous key, and the gateway forwards both to the relay, which is the
 * only party able to check them. After `ready` the socket carries bare
 * encrypted frames in both directions, so the gateway only ever copies opaque
 * bytes.
 */
export function createGatewayChannel(
  relay: RelayConfig,
  handlers: FrameChannelHandlers,
  options: GatewayChannelOptions = {},
): FrameChannel {
  const base = String(relay.gatewayUrl || '').replace(/\/+$/, '');
  let socket: WebSocket | null = null;
  let phase: 'idle' | 'hello' | 'ready' | 'open' | 'closed' = 'idle';
  let handshakeTimer: number | null = null;

  function clearHandshakeTimer(): void {
    window.clearTimeout(handshakeTimer ?? undefined);
    handshakeTimer = null;
  }
  const reassembler = new Reassembler({ onStall: (reason) => fail(reason) });

  function fail(reason: string, fatal?: boolean, code?: string): void {
    clearHandshakeTimer();
    if (phase === 'closed') return;
    phase = 'closed';
    reassembler.close();
    socket?.close();
    socket = null;
    handlers.onClose(fatal ? { reason, fatal, code } : { reason });
  }

  async function answerChallenge(hello: Record<string, unknown>): Promise<void> {
    if (hello.type !== 'gateway_hello') throw new Error('The gateway sent an unexpected greeting.');
    if (Number(hello.proto) !== GATEWAY_PROTO) throw new Error('The gateway speaks an unsupported protocol version.');
    const stunPort = advertisedStunPort(hello);
    if (stunPort > 0) options.onStunPort?.(stunPort);
    const relayId = await deriveRelayId(relay.token);
    const proof = await connectProof(relay.token, relayId, base64UrlDecode(String(hello.nonce || '')));
    if (phase !== 'hello') return;
    phase = 'ready';
    socket?.send(JSON.stringify({
      type: 'connect',
      proto: GATEWAY_PROTO,
      relay_id: relayId,
      proof: base64UrlEncode(proof),
    }));
  }

  function handleText(raw: string): void {
    if (phase === 'hello') {
      answerChallenge(JSON.parse(raw) as Record<string, unknown>).catch((error: unknown) => {
        fail(error instanceof Error && error.message ? error.message : 'Could not answer the gateway challenge.');
      });
      return;
    }
    if (phase !== 'ready') {
      fail('The gateway sent a text frame on an established connection.');
      return;
    }
    const message = JSON.parse(raw) as Record<string, unknown>;
    if (message.type === 'error') {
      const code = String(message.code || 'internal');
      fail(ERROR_MESSAGES[code] || String(message.message || 'The gateway rejected the connection.'), FATAL_ERROR_CODES[code], code);
      return;
    }
    if (message.type !== 'ready') {
      fail('The gateway sent an unexpected handshake message.');
      return;
    }
    clearHandshakeTimer();
    phase = 'open';
    handlers.onOpen();
  }

  return {
    kind: 'gateway',
    codec: 'binary',
    open(): void {
      if (phase !== 'idle') return;
      if (!base) {
        fail('This computer has no gateway address configured.', true);
        return;
      }
      if (!relay.token) {
        fail('Gateway connections require a relay key.', true);
        return;
      }
      phase = 'hello';
      try {
        socket = new WebSocket(`${base}/connect`);
      } catch {
        fail('Could not reach the gateway.');
        return;
      }
      socket.binaryType = 'arraybuffer';
      socket.onmessage = (event: MessageEvent) => {
        if (phase === 'closed') return;
        if (typeof event.data === 'string') {
          try {
            handleText(event.data);
          } catch {
            fail('The gateway sent a malformed handshake message.');
          }
          return;
        }
        if (phase !== 'open') {
          fail('The gateway sent data before the connection was ready.');
          return;
        }
        try {
          const logical = reassembler.push(new Uint8Array(event.data as ArrayBuffer));
          if (logical) handlers.onFrame(decodeWireFrame(logical));
        } catch (error: unknown) {
          fail(error instanceof Error && error.message ? error.message : 'The gateway sent an invalid frame.');
        }
      };
      socket.onclose = () => {
        fail(phase === 'open' ? 'The gateway connection closed.' : 'The gateway refused the connection.');
      };
      socket.onerror = () => {
        fail('The gateway connection failed.');
      };
      handshakeTimer = window.setTimeout(() => {
        fail('The gateway handshake took too long.');
      }, GATEWAY_HANDSHAKE_TIMEOUT_MS);
    },
    sendFrame(frame: E2EEWireFrame): void {
      if (phase !== 'open' || !socket) return;
      // The relay link copies each frame into one multiplexed payload, so a
      // logical message is split before it ever reaches the gateway.
      try {
        for (const piece of chunk(encodeWireFrame(frame), GATEWAY_MAX_CHUNK_BYTES)) socket.send(piece);
      } catch (error: unknown) {
        fail(error instanceof Error && error.message ? error.message : 'The message could not be framed.');
      }
    },
    close(): void {
      if (phase === 'closed') return;
      clearHandshakeTimer();
      phase = 'closed';
      reassembler.close();
      socket?.close(1000);
      socket = null;
    },
  };
}

/** The relayed fallback path: one E2EE session carried by the blind gateway. */
export function createGatewayTransport(
  relay: RelayConfig,
  handlers: TransportHandlers,
  authentication: TransportAuthentication = {},
): RelayTransport {
  // The hello is parsed long before the E2EE handshake finishes, so the port is
  // known by the time this session reports itself usable — which is also the
  // moment the path manager starts the direct attempt that needs it.
  let stunPort = 0;
  return createEncryptedTransport({
    kind: 'gateway',
    token: relay.token,
    codec: 'binary',
    ...authentication,
    handlers: {
      onMessage: (message) => handlers.onMessage(message),
      onStatus(status, detail): void {
        if (status !== 'connected' || stunPort === 0) {
          handlers.onStatus(status, detail);
          return;
        }
        handlers.onStatus(status, { ...detail, stunPort });
      },
    },
    createChannel: (channelHandlers) => createGatewayChannel(relay, channelHandlers, {
      onStunPort: (port) => { stunPort = port; },
    }),
  });
}
