import type { E2EECodec, E2EEWireFrame } from '../e2ee';

/** Which physical path a transport uses to reach the relay. */
export type TransportKind = 'websocket' | 'gateway' | 'webrtc';

/** Stable close reason used by transports that cannot carry WebSocket code 4401. */
export const DEVICE_UNAUTHORIZED_CODE = 'device_unauthorized';
/** Lifecycle of one transport attempt. */
export type TransportStatus = 'connecting' | 'connected' | 'closed';

export interface TransportStatusDetail {
  /** Human-readable reason surfaced to the store for pending-request rejection. */
  reason?: string;
  /**
   * A fatal transport will not succeed on retry with the same configuration
   * (relay key rejected, unsupported transport). The path manager stops
   * retrying this path instead of backing off.
   */
  fatal?: boolean;
  /**
   * Gateway refusal code behind a close, when one exists. `unknown_relay` is
   * the one the store treats specially: it is what a gateway answers while a
   * relay is restarting, so it must keep the normal reconnect cadence even
   * though it is fatal for the current attempt.
   */
  code?: string;
  /**
   * Which physical path is carrying traffic now. A hybrid transport reports
   * `gateway` while relayed and `webrtc` once the direct upgrade takes over,
   * so the store can lower terminal fidelity on the metered relayed path.
   */
  path?: TransportKind;
  /**
   * STUN port the relayed gateway advertises for address discovery, absent when
   * the gateway has it switched off. Only the port crosses the wire: the path
   * manager pairs it with the gateway host it dialed itself.
   */
  stunPort?: number;
  /**
   * Gateway the live session was opened against. Reported for the relayed path
   * and kept on the direct one, whose signalling used that same gateway, so the
   * app can name the candidate actually in use out of a configured list.
   */
  gatewayUrl?: string;
}

/**
 * A relay transport carries authenticated application messages. Every
 * implementation owns its own E2EE session: the paths are independent
 * sessions by design, so a path switch behaves like a fast reconnect rather
 * than an in-flight migration.
 */
export interface RelayTransport {
  readonly kind: TransportKind;
  /** Begin connecting. Idempotent while a connection attempt is in flight. */
  connect(): void;
  /** Queue one application message. Returns false when not connected. */
  send(payload: Record<string, unknown>): boolean;
  /** Close permanently; no further callbacks fire. */
  close(): void;
}

export interface TransportHandlers {
  onMessage(message: Record<string, any>): void;
  onStatus(status: TransportStatus, detail?: TransportStatusDetail): void;
}

/**
 * A raw duplex of encrypted frames. Channels know nothing about Herdr
 * messages; they open a path, move opaque frames, and report closure. The
 * encrypted-session layer above them is shared by every path.
 */
export interface FrameChannel {
  readonly kind: TransportKind;
  readonly codec: E2EECodec;
  open(): void;
  sendFrame(frame: E2EEWireFrame): void;
  close(): void;
}

export interface FrameChannelHandlers {
  onOpen(): void;
  onFrame(frame: E2EEWireFrame): void;
  onClose(detail?: TransportStatusDetail): void;
}

/**
 * Observable handshake milestones, reported only for local resume timing.
 * Channels report the raw dial/open and gateway steps; the encrypted session
 * reports the E2EE steps; the path manager reports direct-upgrade promotion.
 * DNS, TCP and TLS happen inside the browser's WebSocket and are not
 * observable, so they have no phase here.
 */
export type TransportPhase =
  | 'dial'
  | 'open'
  | 'gateway-hello'
  | 'gateway-proof'
  | 'gateway-ready'
  | 'e2ee-hello'
  | 'e2ee-server-hello'
  | 'e2ee-confirm'
  | 'authenticated'
  | 'offer'
  | 'answer'
  | 'ice-connected'
  | 'promoted'
  | 'timeout'
  | 'failed';

export type TransportObserver = (phase: TransportPhase, path: TransportKind) => void;

/**
 * Reports a phase without ever letting measurement affect the connection: an
 * observer that throws is ignored.
 */
export function observePhase(
  observe: TransportObserver | undefined,
  phase: TransportPhase,
  path: TransportKind,
): void {
  try {
    observe?.(phase, path);
  } catch {
    // Measurement is best effort and must not change transport behaviour.
  }
}

/** Factory shape used by the path manager to build a channel on demand. */
export type FrameChannelFactory = (handlers: FrameChannelHandlers, encrypted: boolean) => FrameChannel;
