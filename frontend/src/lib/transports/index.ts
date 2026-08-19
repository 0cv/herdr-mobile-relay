import type { RelayConfig } from '../types';
import { createHybridTransport } from './path-manager';
import type { RelayTransport, TransportHandlers } from './types';
import { createWebSocketTransport } from './websocket';

export type {
  FrameChannel,
  FrameChannelFactory,
  FrameChannelHandlers,
  RelayTransport,
  TransportHandlers,
  TransportKind,
  TransportStatus,
  TransportStatusDetail,
} from './types';
export { createEncryptedTransport } from './encrypted';
export { createWebSocketTransport } from './websocket';
export { createHybridTransport } from './path-manager';

/**
 * Builds the transport a relay entry asks for. Legacy entries carry no
 * `transport` field and keep the direct browser WebSocket; hybrid entries
 * reach the relay through the blind gateway and upgrade to a direct WebRTC
 * DataChannel when the two peers can find each other.
 */
export function createRelayTransport(
  relay: RelayConfig,
  handlers: TransportHandlers,
): RelayTransport {
  if (relay.transport === 'hybrid') return createHybridTransport(relay, handlers);
  return createWebSocketTransport(relay, handlers);
}
