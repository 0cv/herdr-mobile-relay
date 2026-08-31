import { E2EE_SUBPROTOCOL, type E2EEWireFrame } from '../e2ee';
import type { RelayConfig } from '../types';
import { createEncryptedTransport, type TransportAuthentication } from './encrypted';
import type {
  FrameChannel,
  FrameChannelHandlers,
  RelayTransport,
  TransportHandlers,
} from './types';

/**
 * The original direct browser WebSocket path: the phone reaches the relay over
 * whatever URL the relay published (a Cloudflare tunnel, a LAN address). The
 * encrypted session rides the legacy JSON text envelope so relays that predate
 * the binary codec keep working unchanged.
 */
export function createWebSocketTransport(
  relay: RelayConfig,
  handlers: TransportHandlers,
  authentication: TransportAuthentication = {},
): RelayTransport {
  return createEncryptedTransport({
    kind: 'websocket',
    token: relay.token,
    codec: 'json',
    handlers,
    ...authentication,
    createChannel: (channelHandlers) => createWebSocketChannel(
      relay,
      channelHandlers,
      Boolean(relay.token || authentication.authentication),
    ),
  });
}

function createWebSocketChannel(
  relay: RelayConfig,
  handlers: FrameChannelHandlers,
  encrypted: boolean,
): FrameChannel {
  let socket: WebSocket | null = null;
  let closed = false;

  function fail(reason: string): void {
    if (closed) return;
    closed = true;
    socket?.close();
    handlers.onClose({ reason });
  }

  return {
    kind: 'websocket',
    codec: 'json',
    open(): void {
      if (closed || socket) return;
      try {
        socket = encrypted
          ? new WebSocket(relay.url, E2EE_SUBPROTOCOL)
          : new WebSocket(relay.url);
      } catch {
        fail('Relay connection failed');
        return;
      }
      socket.onopen = () => {
        if (closed) return;
        // A relay that ignores the encrypted subprotocol would otherwise get a
        // plaintext hello, so refuse the socket before anything is sent.
        if (encrypted
          && typeof socket?.protocol === 'string'
          && socket.protocol !== E2EE_SUBPROTOCOL) {
          fail('Relay did not negotiate encrypted transport');
          return;
        }
        handlers.onOpen();
      };
      socket.onmessage = (event) => {
        if (closed) return;
        handlers.onFrame(String(event.data));
      };
      socket.onerror = () => {
        fail('Relay connection failed');
      };
      socket.onclose = () => {
        if (closed) return;
        closed = true;
        handlers.onClose({ reason: 'Relay disconnected' });
      };
    },
    sendFrame(frame: E2EEWireFrame): void {
      if (closed || socket?.readyState !== WebSocket.OPEN) return;
      socket.send(frame);
    },
    close(): void {
      closed = true;
      socket?.close();
    },
  };
}
