import { base64UrlEncode } from '../base64url';
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

/** DataChannel label negotiated with the relay. */
export const DATA_CHANNEL_LABEL = 'herdr-dc-v1';
/** SCTP keeps DataChannel messages small, so chunks stay at 16 KiB. */
export const DATA_CHANNEL_MAX_CHUNK_BYTES = 16_384;
/** Stop feeding the DataChannel above this much buffered data. */
export const BUFFERED_AMOUNT_HIGH = 4 * 1024 * 1024;
/** Resume feeding it once the browser has drained back to this level. */
export const BUFFERED_AMOUNT_LOW = 1024 * 1024;
/** One ICE restart is attempted before the direct path is given up. */
export const ICE_RESTART_GRACE_MS = 10_000;

/**
 * Relay-independent signalling seam. The path manager backs this with the live
 * gateway transport, so offers, answers and candidates ride inside the
 * established E2EE session and the gateway never sees SDP.
 */
export interface SignalingChannel {
  send(payload: Record<string, unknown>): boolean;
  onMessage(handler: (message: Record<string, any>) => void): () => void;
}

/** Configuration for one direct attempt. */
export interface DirectTransportOptions {
  /**
   * ICE servers used for address discovery. An empty list means host
   * candidates only, which reaches a peer on the same LAN and nowhere else.
   */
  iceServers: RTCIceServer[];
}

/**
 * The direct path. The browser is the offerer: it creates the DataChannel,
 * offers, and trickles candidates through the already-authenticated relayed
 * session. A fresh E2EE handshake runs on top of the DataChannel, so the
 * direct path is a full peer of the relayed one rather than a migration of it.
 */
export function createWebRTCChannel(
  signal: SignalingChannel,
  handlers: FrameChannelHandlers,
  iceServers: RTCIceServer[] = [],
): FrameChannel {
  const requestId = base64UrlEncode(crypto.getRandomValues(new Uint8Array(new ArrayBuffer(12))));
  const pendingCandidates: RTCIceCandidateInit[] = [];
  const outbound: Uint8Array<ArrayBuffer>[] = [];
  let connection: RTCPeerConnection | null = null;
  let channel: RTCDataChannel | null = null;
  let unsubscribe: (() => void) | null = null;
  let reassembler: Reassembler | null = null;
  let restartTimer: ReturnType<typeof setTimeout> | null = null;
  let restarted = false;
  let opened = false;
  let offered = false;
  let closed = false;

  function teardown(notifyPeer: boolean): void {
    clearTimeout(restartTimer ?? undefined);
    restartTimer = null;
    unsubscribe?.();
    unsubscribe = null;
    reassembler?.close();
    reassembler = null;
    outbound.length = 0;
    if (notifyPeer && offered) signal.send({ type: 'webrtc_close', request_id: requestId });
    channel?.close();
    channel = null;
    connection?.close();
    connection = null;
  }

  function fail(reason: string, options?: { fatal?: boolean; notifyPeer?: boolean }): void {
    if (closed) return;
    closed = true;
    teardown(options?.notifyPeer ?? true);
    handlers.onClose(options?.fatal ? { reason, fatal: true } : { reason });
  }

  function flush(): void {
    const active = channel;
    if (!active) return;
    while (outbound.length > 0) {
      if (active.bufferedAmount >= BUFFERED_AMOUNT_HIGH) return;
      active.send(outbound.shift()!);
    }
  }

  async function negotiate(iceRestart: boolean): Promise<void> {
    const peer = connection;
    if (!peer) return;
    const offer = await peer.createOffer(iceRestart ? { iceRestart: true } : undefined);
    await peer.setLocalDescription(offer);
    if (closed || connection !== peer) return;
    offered = true;
    const sdp = peer.localDescription?.sdp || offer.sdp || '';
    if (signal.send({ type: 'webrtc_offer', request_id: requestId, sdp })) return;
    fail('The relayed connection dropped before the direct connection was set up.', { notifyPeer: false });
  }

  async function applyAnswer(sdp: string): Promise<void> {
    const peer = connection;
    if (!peer) return;
    await peer.setRemoteDescription({ type: 'answer', sdp });
    if (closed || connection !== peer) return;
    while (pendingCandidates.length > 0) await peer.addIceCandidate(pendingCandidates.shift()!);
  }

  function handleSignal(message: Record<string, any>): void {
    if (closed || message.request_id !== requestId) return;
    if (message.type === 'webrtc_answer') {
      applyAnswer(String(message.sdp || '')).catch(() => {
        fail('The computer sent an unusable direct connection answer.');
      });
      return;
    }
    if (message.type === 'webrtc_ice') {
      const candidate: RTCIceCandidateInit = {
        candidate: String(message.candidate || ''),
        sdpMid: message.sdp_mid === undefined || message.sdp_mid === null ? null : String(message.sdp_mid),
        sdpMLineIndex: message.sdp_mline_index === undefined || message.sdp_mline_index === null
          ? null
          : Number(message.sdp_mline_index),
      };
      if (!connection?.remoteDescription) {
        pendingCandidates.push(candidate);
        return;
      }
      connection.addIceCandidate(candidate).catch(() => {
        // A rejected candidate only costs one path; ICE continues on the rest.
      });
      return;
    }
    if (message.type === 'webrtc_closed') {
      fail(String(message.reason || 'The computer closed the direct connection.'), { notifyPeer: false });
      return;
    }
    if (message.type === 'command_result' && message.ok === false) {
      fail(String(message.error || 'The computer refused a direct connection.'), { fatal: true, notifyPeer: false });
    }
  }

  function handleStateChange(): void {
    const peer = connection;
    if (!peer || closed) return;
    if (peer.connectionState === 'connected') {
      clearTimeout(restartTimer ?? undefined);
      restartTimer = null;
      return;
    }
    if (peer.connectionState === 'closed') {
      fail('The direct connection closed.', { notifyPeer: false });
      return;
    }
    if (peer.connectionState !== 'disconnected' && peer.connectionState !== 'failed') return;
    if (restarted) {
      fail('The direct connection could not be re-established.');
      return;
    }
    // A Wi-Fi to cellular flip only invalidates the candidate pair; one ICE
    // restart usually recovers the same DataChannel in a few hundred ms.
    restarted = true;
    peer.restartIce();
    restartTimer = setTimeout(() => {
      fail('The direct connection could not be re-established.');
    }, ICE_RESTART_GRACE_MS);
    negotiate(true).catch(() => {
      fail('The direct connection could not be re-established.');
    });
  }

  return {
    kind: 'webrtc',
    codec: 'binary',
    open(): void {
      if (closed || connection) return;
      if (typeof RTCPeerConnection === 'undefined') {
        fail('This browser cannot open a direct connection.', { fatal: true, notifyPeer: false });
        return;
      }
      reassembler = new Reassembler({ onStall: (reason) => fail(reason) });
      unsubscribe = signal.onMessage(handleSignal);
      connection = new RTCPeerConnection({ iceServers });
      connection.onicecandidate = (event) => {
        if (!event.candidate || closed) return;
        signal.send({
          type: 'webrtc_ice',
          request_id: requestId,
          candidate: event.candidate.candidate,
          sdp_mid: event.candidate.sdpMid,
          sdp_mline_index: event.candidate.sdpMLineIndex,
        });
      };
      connection.onconnectionstatechange = handleStateChange;
      channel = connection.createDataChannel(DATA_CHANNEL_LABEL, { ordered: true });
      channel.binaryType = 'arraybuffer';
      channel.bufferedAmountLowThreshold = BUFFERED_AMOUNT_LOW;
      channel.onbufferedamountlow = flush;
      channel.onopen = () => {
        if (closed || opened) return;
        opened = true;
        handlers.onOpen();
      };
      channel.onclose = () => fail('The direct connection closed.', { notifyPeer: false });
      channel.onerror = () => fail('The direct connection failed.');
      channel.onmessage = (event: MessageEvent) => {
        if (closed) return;
        if (typeof event.data === 'string') {
          fail('The computer sent text on the direct connection.');
          return;
        }
        try {
          const logical = reassembler?.push(new Uint8Array(event.data as ArrayBuffer));
          if (logical) handlers.onFrame(decodeWireFrame(logical));
        } catch (error: unknown) {
          fail(error instanceof Error && error.message ? error.message : 'The direct connection sent an invalid frame.');
        }
      };
      negotiate(false).catch(() => {
        fail('Could not offer a direct connection.');
      });
    },
    sendFrame(frame: E2EEWireFrame): void {
      if (closed || !channel || !opened) return;
      try {
        for (const piece of chunk(encodeWireFrame(frame), DATA_CHANNEL_MAX_CHUNK_BYTES)) outbound.push(piece);
      } catch (error: unknown) {
        fail(error instanceof Error && error.message ? error.message : 'Direct message could not be framed.');
        return;
      }
      flush();
    },
    close(): void {
      if (closed) return;
      closed = true;
      teardown(true);
    },
  };
}

/** The direct path: its own E2EE session over a browser-created DataChannel. */
export function createWebRTCTransport(
  relay: RelayConfig,
  signal: SignalingChannel,
  handlers: TransportHandlers,
  options: DirectTransportOptions = { iceServers: [] },
  authentication: TransportAuthentication = {},
): RelayTransport {
  return createEncryptedTransport({
    kind: 'webrtc',
    token: relay.token,
    codec: 'binary',
    ...authentication,
    handlers,
    createChannel: (channelHandlers) => createWebRTCChannel(signal, channelHandlers, options.iceServers),
  });
}
