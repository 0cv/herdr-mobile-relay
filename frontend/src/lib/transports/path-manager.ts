import type { RelayConfig } from '../types';
import { createGatewayTransport } from './gateway';
import { createWebSocketTransport } from './websocket';
import { createWebRTCTransport, type DirectTransportOptions, type SignalingChannel } from './webrtc';
import type { TransportAuthentication } from './encrypted';
import type {
  RelayTransport,
  TransportHandlers,
  TransportStatusDetail,
} from './types';

/** How long the direct path must hold before the relayed one is released. */
export const DIRECT_STABILITY_MS = 10_000;
/** First direct retry delay; doubles per consecutive failure. */
export const DIRECT_RETRY_BASE_MS = 2_000;
export const DIRECT_RETRY_CAP_MS = 60_000;
/** A direct attempt is never started twice inside this window. */
export const DIRECT_MIN_INTERVAL_MS = 2_000;
/** A direct attempt that never delivers its first message is abandoned. */
export const DIRECT_ATTEMPT_TIMEOUT_MS = 20_000;
/** Test switch that pins the connection to the relayed path. */
export const FORCE_RELAY_KEY = 'herdr_force_relay';

/**
 * Signalling rides the relayed session, so these arrive on the gateway
 * transport as ordinary application messages and must never reach the store.
 */
const SIGNALING_TYPES: Record<string, true> = {
  webrtc_answer: true,
  webrtc_ice: true,
  webrtc_closed: true,
};

/**
 * Builds the direct WebRTC transport. The real implementation is loaded on
 * demand, so its type is named here rather than pulled in from the module.
 */
export type DirectFactory = (
  relay: RelayConfig,
  signal: SignalingChannel,
  handlers: TransportHandlers,
  options: DirectTransportOptions,
) => RelayTransport;

/**
 * Address discovery uses the gateway this phone already dialed: the gateway
 * advertises a port, the host comes from the relay's own configuration. A
 * gateway therefore cannot aim a peer at a server of its choosing, and it
 * learns nothing by reflecting a source address it already sees. No advertised
 * port and no usable gateway host means host candidates only, as before.
 */
function stunServers(gatewayUrl: string, port: number): RTCIceServer[] {
  if (port === 0) return [];
  let host: string;
  try {
    host = new URL(gatewayUrl).hostname;
  } catch {
    return [];
  }
  if (!host) return [];
  return [{ urls: `stun:${host}:${port}` }];
}

/**
 * Gateway addresses to try for one relay, most preferred first. A normalized
 * config keeps `gatewayUrl` at the head of `gatewayUrls`, but a relay that
 * advertised a new address on a live session — and a config that predates the
 * ordered list — does not, so the primary always leads here.
 */
function gatewayTargets(relay: RelayConfig): string[] {
  const targets: string[] = [];
  for (const value of [relay.gatewayUrl, ...(relay.gatewayUrls ?? [])]) {
    const entry = String(value || '').trim();
    if (!entry || targets.includes(entry)) continue;
    targets.push(entry);
  }
  return targets;
}

/** Seams for tests; production uses the real gateway and WebRTC transports. */
export interface HybridTransportOverrides {
  createGateway?(relay: RelayConfig, handlers: TransportHandlers): RelayTransport;
  createDirect?: DirectFactory;
  createLegacy?(relay: RelayConfig, handlers: TransportHandlers): RelayTransport;
}

function forcedRelay(): boolean {
  const flag: unknown = import.meta.env?.VITE_HERDR_FORCE_RELAY;
  if (flag === true || flag === '1' || flag === 'true') return true;
  try {
    return ['1', 'true'].includes(localStorage.getItem(FORCE_RELAY_KEY) || '');
  } catch {
    return false;
  }
}

/**
 * Fallback-first hybrid path. The relayed gateway session connects immediately
 * and carries traffic while a direct DataChannel is negotiated inside it. The
 * direct path only takes over once its own E2EE session has produced a real
 * relay message, so a half-working peer connection can never strand the app;
 * any later direct failure drops straight back to the relayed path.
 */
export function createHybridTransport(
  relay: RelayConfig,
  handlers: TransportHandlers,
  overrides: HybridTransportOverrides = {},
  authentication: TransportAuthentication = {},
): RelayTransport {
  const makeGateway = overrides.createGateway
    ?? ((target, targetHandlers) => createGatewayTransport(target, targetHandlers, authentication));
  const makeDirect = overrides.createDirect
    ?? ((target, signal, targetHandlers, options) => createWebRTCTransport(target, signal, targetHandlers, options, authentication));
  const makeLegacy = overrides.createLegacy
    ?? ((target, targetHandlers) => createWebSocketTransport(target, targetHandlers, authentication));
  const forceRelay = forcedRelay();
  const signalHandlers = new Set<(message: Record<string, any>) => void>();

  const targets = gatewayTargets(relay);
  /** Entry the next relayed attempt dials; wraps around the list. */
  let gatewayIndex = 0;
  /**
   * Relayed attempts made since the last usable session. One full pass over
   * the list is the limit, so a relay whose gateways are all down reports the
   * failure and lets the store's reconnect backoff pace the next pass.
   */
  let gatewaysTried = 0;
  /**
   * The relay entry the live attempt was opened with: its `gatewayUrl` is the
   * address actually dialed, which is also the only host address discovery may
   * hand to the direct path.
   */
  let dialed: RelayConfig = relay;
  let gateway: RelayTransport | null = null;
  let gatewayReady = false;
  /** Port the live relayed session advertised; 0 disables address discovery. */
  let stunPort = 0;
  let direct: RelayTransport | null = null;
  let active: 'gateway' | 'direct' | 'legacy' = 'gateway';
  let directReady = false;
  let directBlocked = false;
  let legacy: RelayTransport | null = null;
  let legacyTried = false;
  let directFailures = 0;
  let directAttempt = 0;
  let lastDirectStart = 0;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  let attemptTimer: ReturnType<typeof setTimeout> | null = null;
  let stabilityTimer: ReturnType<typeof setTimeout> | null = null;
  let closed = false;

  const signal: SignalingChannel = {
    send(payload: Record<string, unknown>): boolean {
      return gateway?.send(payload) ?? false;
    },
    onMessage(handler: (message: Record<string, any>) => void): () => void {
      signalHandlers.add(handler);
      return () => {
        signalHandlers.delete(handler);
      };
    },
  };

  /** The gateway carrying, or having signalled, the live session. */
  function dialedGateway(): string {
    return String(dialed.gatewayUrl || '');
  }

  function scheduleDirectRetry(): void {
    if (closed || forceRelay || directBlocked || direct || retryTimer !== null) return;
    const delay = Math.min(DIRECT_RETRY_BASE_MS * 2 ** Math.max(0, directFailures - 1), DIRECT_RETRY_CAP_MS);
    retryTimer = setTimeout(() => {
      retryTimer = null;
      startDirect();
    }, delay);
  }

  function startDirect(): void {
    if (closed || forceRelay || directBlocked || direct || retryTimer !== null) return;
    // Signalling needs a live relayed session; without one the offer cannot
    // even be sent, so wait for the gateway to come back first.
    if (active === 'direct' || !gatewayReady) return;
    const sinceLast = Date.now() - lastDirectStart;
    if (lastDirectStart > 0 && sinceLast < DIRECT_MIN_INTERVAL_MS) {
      retryTimer = setTimeout(() => {
        retryTimer = null;
        startDirect();
      }, DIRECT_MIN_INTERVAL_MS - sinceLast);
      return;
    }
    lastDirectStart = Date.now();
    directReady = false;
    // Callbacks from an attempt that has already been replaced are ignored.
    directAttempt += 1;
    const attempt = directAttempt;
    attemptTimer = setTimeout(() => {
      attemptTimer = null;
      failDirect({ reason: 'The direct connection took too long to come up.' });
    }, DIRECT_ATTEMPT_TIMEOUT_MS);
    direct = makeDirect(dialed, signal, directHandlers(attempt), {
      iceServers: stunServers(String(dialed.gatewayUrl || ''), stunPort),
    });
    direct.connect();
  }

  function directHandlers(attempt: number): TransportHandlers {
    return {
      onMessage(message: Record<string, any>): void {
        if (closed || attempt !== directAttempt) return;
        // The first message after the direct handshake is the relay snapshot:
        // proof the whole path works end to end, and the moment to switch.
        if (active !== 'direct') {
          if (!directReady) return;
          promoteDirect();
        }
        handlers.onMessage(message);
      },
      onStatus(status, detail): void {
        if (closed || attempt !== directAttempt) return;
        if (status === 'connecting') return;
        if (status === 'connected') {
          directReady = true;
          return;
        }
        failDirect(detail);
      },
    };
  }

  function promoteDirect(): void {
    active = 'direct';
    directFailures = 0;
    clearTimeout(attemptTimer ?? undefined);
    attemptTimer = null;
    handlers.onStatus('connected', { path: 'webrtc', gatewayUrl: dialedGateway() });
    stabilityTimer = setTimeout(() => {
      stabilityTimer = null;
      gatewayReady = false;
      gateway?.close();
      gateway = null;
    }, DIRECT_STABILITY_MS);
  }

  function failDirect(detail?: TransportStatusDetail): void {
    if (closed || !direct) return;
    const wasActive = active === 'direct';
    clearTimeout(attemptTimer ?? undefined);
    attemptTimer = null;
    clearTimeout(stabilityTimer ?? undefined);
    stabilityTimer = null;
    direct.close();
    direct = null;
    directReady = false;
    directFailures += 1;
    if (detail?.fatal) directBlocked = true;
    if (wasActive) {
      active = 'gateway';
      handlers.onStatus('connecting', detail?.reason ? { reason: detail.reason } : undefined);
      if (gatewayReady) handlers.onStatus('connected', { path: 'gateway', gatewayUrl: dialedGateway() });
      else if (!gateway) openGateway();
    }
    scheduleDirectRetry();
  }

  function openGateway(): void {
    gatewaysTried += 1;
    dialed = targets.length ? { ...relay, gatewayUrl: targets[gatewayIndex] } : relay;
    gateway = makeGateway(dialed, {
      onMessage(message: Record<string, any>): void {
        if (closed) return;
        for (const handler of [...signalHandlers]) handler(message);
        if (SIGNALING_TYPES[String(message.type)]) return;
        if (active !== 'gateway') return;
        handlers.onMessage(message);
      },
      onStatus(status, detail): void {
        if (closed) return;
        if (status === 'closed') {
          gatewayReady = false;
          gateway = null;
          // The entry that just died never gets the next attempt: the list is
          // walked in order and wraps, so one unreachable gateway — or one that
          // does not know this relay — cannot strand a phone that has others.
          if (targets.length > 1) gatewayIndex = (gatewayIndex + 1) % targets.length;
          if (active !== 'gateway') return;
          // A single-gateway relay keeps its old behaviour: a dead session ends
          // the transport and the store's backoff decides when to try again.
          if (targets.length > 1 && gatewaysTried < targets.length) {
            handlers.onStatus('connecting', detail?.reason ? { reason: detail.reason } : undefined);
            openGateway();
            return;
          }
          // Every entry has failed in this pass. The legacy WSS URL is the last
          // resort; otherwise a relayed session that dies while it is carrying
          // traffic ends the whole transport and the store reconnects.
          if (detail?.fatal && startLegacy(detail)) return;
          closed = true;
          stop();
          handlers.onStatus('closed', detail);
          return;
        }
        gatewayReady = status === 'connected';
        if (status === 'connected') {
          // A session that came up earns the list a fresh pass, so a later drop
          // can walk every entry again. The port belongs to the live session,
          // so a replacement gateway that stops advertising it disables
          // address discovery again.
          gatewaysTried = 0;
          stunPort = detail?.stunPort ?? 0;
        }
        if (active !== 'gateway') return;
        handlers.onStatus(
          status,
          status === 'connected' ? { ...detail, gatewayUrl: dialedGateway() } : detail,
        );
        if (status === 'connected') startDirect();
      },
    });
    gateway.connect();
  }

  /**
   * Bridge-window safety net. A config migrated from the legacy WSS URL keeps
   * that URL, so a phone that cannot reach the gateway at all — blocked
   * network, gateway outage, relay not registered — falls back to the path
   * that was working before the migration instead of being stranded.
   */
  function startLegacy(detail: TransportStatusDetail): boolean {
    if (legacyTried || !relay.url) return false;
    legacyTried = true;
    active = 'legacy';
    handlers.onStatus('connecting', { reason: detail.reason });
    legacy = makeLegacy(relay, {
      onMessage(message: Record<string, any>): void {
        if (closed || active !== 'legacy') return;
        handlers.onMessage(message);
      },
      onStatus(status, legacyDetail): void {
        if (closed || active !== 'legacy') return;
        if (status === 'closed') {
          legacy = null;
          closed = true;
          stop();
        }
        handlers.onStatus(status, legacyDetail);
      },
    });
    legacy.connect();
    return true;
  }

  function stop(): void {
    clearTimeout(retryTimer ?? undefined);
    retryTimer = null;
    clearTimeout(attemptTimer ?? undefined);
    attemptTimer = null;
    clearTimeout(stabilityTimer ?? undefined);
    stabilityTimer = null;
    signalHandlers.clear();
    direct?.close();
    direct = null;
    legacy?.close();
    legacy = null;
    gatewayReady = false;
    gateway?.close();
    gateway = null;
  }

  return {
    kind: 'gateway',
    connect(): void {
      if (closed || gateway) return;
      openGateway();
    },
    send(payload: Record<string, unknown>): boolean {
      if (closed) return false;
      if (active === 'legacy') return legacy?.send(payload) ?? false;
      if (active === 'direct') return direct?.send(payload) ?? false;
      return gateway?.send(payload) ?? false;
    },
    close(): void {
      if (closed) return;
      closed = true;
      stop();
    },
  };
}
