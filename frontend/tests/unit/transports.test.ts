import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { base64UrlDecode, base64UrlEncode } from '$lib/base64url';
import type { E2EEWireFrame } from '$lib/e2ee';
import {
  importQuickSetup,
  loadRelayConfigs,
  makeRelayId,
  normalizeRelayConfig,
  quickSetupConfig,
  saveRelayConfigs,
} from '$lib/config';
import { connectProof, deriveRelayId, RELAY_ID_LENGTH } from '$lib/gateway-credentials';
import {
  chunk,
  decodeWireFrame,
  encodeWireFrame,
  Reassembler,
  CHUNK_HEADER_BYTES,
  CHUNK_LENGTH_BYTES,
  CHUNK_STALL_TIMEOUT_MS,
  CHUNK_VERSION,
  MAX_LOGICAL_BYTES,
} from '$lib/transports/chunking';
import {
  createGatewayChannel,
  GATEWAY_HANDSHAKE_TIMEOUT_MS,
  GATEWAY_MAX_CHUNK_BYTES,
} from '$lib/transports/gateway';
import {
  createWebRTCChannel,
  BUFFERED_AMOUNT_HIGH,
  BUFFERED_AMOUNT_LOW,
  DATA_CHANNEL_LABEL,
  DATA_CHANNEL_MAX_CHUNK_BYTES,
  ICE_RESTART_GRACE_MS,
  type DirectTransportOptions,
  type SignalingChannel,
} from '$lib/transports/webrtc';
import {
  createHybridTransport,
  DIRECT_RETRY_BASE_MS,
  DIRECT_STABILITY_MS,
  FORCE_RELAY_KEY,
} from '$lib/transports/path-manager';
import type {
  FrameChannelHandlers,
  RelayTransport,
  TransportHandlers,
  TransportStatus,
  TransportStatusDetail,
} from '$lib/transports/types';
import type { RelayConfig } from '$lib/types';

/**
 * Cross-language vector. The same values are produced by the Go helpers in
 * `internal/gatewaywire`:
 *
 *   relay_key      = "0123456789abcdef0123456789abcdef"
 *   nonce          = bytes 0x00..0x1f
 *   relay_id       = "Ccy3nT9AULlAceTEnhTvoQ"
 *   rendezvous_key = "xvT5VptkJHebIfy8b9PSGTJMkdRb-J_P2SXrtNRoLyA"
 *   proof          = "Hwp4L_KZVzHeZz5Sgm-gqvpRee6Io4M-jqaIJqm8zpc"
 */
const VECTOR_TOKEN = '0123456789abcdef0123456789abcdef';
const VECTOR_RELAY_ID = 'Ccy3nT9AULlAceTEnhTvoQ';
const VECTOR_NONCE = 'AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8';
const VECTOR_PROOF = 'Hwp4L_KZVzHeZz5Sgm-gqvpRee6Io4M-jqaIJqm8zpc';

const HYBRID_RELAY: RelayConfig = {
  id: 'gw-fedora',
  label: 'Fedora',
  url: '',
  token: VECTOR_TOKEN,
  transport: 'hybrid',
  gatewayUrl: 'wss://gw.example.com',
};

class MockGatewaySocket {
  static instances: MockGatewaySocket[] = [];
  binaryType = 'blob';
  sent: (string | ArrayBufferView)[] = [];
  closeCode: number | null = null;
  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: string | ArrayBuffer }) => void) | null = null;

  constructor(readonly url: string) { MockGatewaySocket.instances.push(this); }

  send(payload: string | ArrayBufferView): void { this.sent.push(payload); }
  close(code?: number): void { this.closeCode = code ?? 1000; }
  text(payload: unknown): void { this.onmessage?.({ data: JSON.stringify(payload) }); }
  binary(frame: Uint8Array): void {
    this.onmessage?.({ data: frame.buffer.slice(frame.byteOffset, frame.byteOffset + frame.byteLength) as ArrayBuffer });
  }
}

interface ChannelRecorder extends FrameChannelHandlers {
  opens: number;
  frames: E2EEWireFrame[];
  closes: (TransportStatusDetail | undefined)[];
}

function channelRecorder(): ChannelRecorder {
  const recorder: ChannelRecorder = {
    opens: 0,
    frames: [],
    closes: [],
    onOpen(): void { recorder.opens += 1; },
    onFrame(frame): void { recorder.frames.push(frame); },
    onClose(detail): void { recorder.closes.push(detail); },
  };
  return recorder;
}

describe('gateway credentials', () => {
  it('matches the Go relay id and connect proof vector', async () => {
    const relayId = await deriveRelayId(VECTOR_TOKEN);
    expect(relayId).toBe(VECTOR_RELAY_ID);
    expect(relayId).toHaveLength(RELAY_ID_LENGTH);

    const proof = await connectProof(VECTOR_TOKEN, relayId, base64UrlDecode(VECTOR_NONCE));
    expect(base64UrlEncode(proof)).toBe(VECTOR_PROOF);
    expect(proof).toHaveLength(32);
  });

  it('binds the proof to the relay id and rejects malformed challenges', async () => {
    const other = await connectProof(VECTOR_TOKEN, 'Ccy3nT9AULlAceTEnhTvoR', base64UrlDecode(VECTOR_NONCE));
    expect(base64UrlEncode(other)).not.toBe(VECTOR_PROOF);
    await expect(connectProof(VECTOR_TOKEN, VECTOR_RELAY_ID, new Uint8Array(8))).rejects.toThrow(/challenge/);
    await expect(deriveRelayId('')).rejects.toThrow(/relay key/);
  });
});

describe('gateway frame channel', () => {
  beforeEach(() => {
    MockGatewaySocket.instances = [];
    vi.stubGlobal('WebSocket', MockGatewaySocket);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('answers the challenge, opens on ready, and chunks frames both ways', async () => {
    const recorder = channelRecorder();
    const channel = createGatewayChannel(HYBRID_RELAY, recorder);
    channel.open();

    const socket = MockGatewaySocket.instances[0];
    expect(socket.url).toBe('wss://gw.example.com/connect');
    expect(socket.binaryType).toBe('arraybuffer');

    socket.text({ type: 'gateway_hello', proto: 1, nonce: VECTOR_NONCE });
    await vi.waitFor(() => expect(socket.sent).toHaveLength(1));
    expect(JSON.parse(String(socket.sent[0]))).toEqual({
      type: 'connect',
      proto: 1,
      relay_id: VECTOR_RELAY_ID,
      proof: VECTOR_PROOF,
    });
    expect(recorder.opens).toBe(0);

    socket.text({ type: 'ready', proto: 1 });
    expect(recorder.opens).toBe(1);

    for (const piece of chunk(Uint8Array.from([1, 0, 9]), GATEWAY_MAX_CHUNK_BYTES)) socket.binary(piece);
    expect(recorder.frames).toHaveLength(1);
    expect(Array.from(recorder.frames[0] as Uint8Array)).toEqual([1, 0, 9]);

    channel.sendFrame(Uint8Array.from([7, 7]));
    const single = socket.sent[1] as Uint8Array;
    expect(single[1]).toBe(3);
    expect(Array.from(single.subarray(CHUNK_HEADER_BYTES + CHUNK_LENGTH_BYTES))).toEqual([7, 7]);

    // An image upload is far past the gateway's 1 MiB per-frame budget, so it
    // must leave as several bounded WebSocket messages.
    const upload = new Uint8Array(new ArrayBuffer(1_500_000));
    for (let index = 0; index < upload.length; index += 1) upload[index] = index % 251;
    const before = socket.sent.length;
    channel.sendFrame(upload);
    const pieces = socket.sent.slice(before) as Uint8Array[];
    expect(pieces.length).toBeGreaterThan(1);
    for (const piece of pieces) expect(piece.byteLength).toBeLessThanOrEqual(GATEWAY_MAX_CHUNK_BYTES);

    const reassembler = new Reassembler({ onStall: (reason) => { throw new Error(reason); } });
    const rebuilt = pieces.map((piece) => reassembler.push(piece)).filter((frame) => frame !== null);
    reassembler.close();
    expect(rebuilt).toHaveLength(1);
    expect(rebuilt[0].every((byte, index) => byte === upload[index])).toBe(true);

    channel.close();
    expect(socket.closeCode).toBe(1000);
  });

  it.each([
    ['unknown_relay', true],
    ['quota_exceeded', true],
    ['rate_limited', false],
    ['too_many_clients', false],
    // A full shared gateway empties again, so this must not poison the relay
    // config the way quota exhaustion does.
    ['at_capacity', false],
  ])('maps the %s error to fatal=%s', async (code, fatal) => {
    const recorder = channelRecorder();
    const channel = createGatewayChannel(HYBRID_RELAY, recorder);
    channel.open();
    const socket = MockGatewaySocket.instances[0];
    socket.text({ type: 'gateway_hello', proto: 1, nonce: VECTOR_NONCE });
    await vi.waitFor(() => expect(socket.sent).toHaveLength(1));

    socket.text({ type: 'error', code, message: 'rejected' });
    expect(recorder.closes).toHaveLength(1);
    expect(recorder.closes[0]?.fatal ?? false).toBe(fatal);
    expect(recorder.closes[0]?.reason).toBeTruthy();
    expect(recorder.opens).toBe(0);
    // A late close from the same socket must not double-report.
    socket.onclose?.();
    expect(recorder.closes).toHaveLength(1);
  });

  it('rejects an unsupported protocol version and a missing gateway address', async () => {
    const recorder = channelRecorder();
    createGatewayChannel({ ...HYBRID_RELAY, gatewayUrl: '' }, recorder).open();
    expect(recorder.closes[0]).toEqual({ reason: expect.stringContaining('gateway address'), fatal: true });
    expect(MockGatewaySocket.instances).toHaveLength(0);

    const second = channelRecorder();
    createGatewayChannel(HYBRID_RELAY, second).open();
    MockGatewaySocket.instances[0].text({ type: 'gateway_hello', proto: 9, nonce: VECTOR_NONCE });
    await vi.waitFor(() => expect(second.closes).toHaveLength(1));
    expect(second.closes[0]?.reason).toMatch(/protocol version/);
  });

  it('closes a silent gateway handshake so the next candidate can run', () => {
    vi.useFakeTimers();
    try {
      const recorder = channelRecorder();
      createGatewayChannel(HYBRID_RELAY, recorder).open();

      vi.advanceTimersByTime(GATEWAY_HANDSHAKE_TIMEOUT_MS - 1);
      expect(recorder.closes).toHaveLength(0);
      vi.advanceTimersByTime(1);
      expect(recorder.closes).toEqual([{ reason: 'The gateway handshake took too long.' }]);
      expect(MockGatewaySocket.instances[0].closeCode).toBe(1000);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('binary transport chunk framing', () => {
  function roundTrip(payload: Uint8Array, maxChunkBytes: number): { frames: Uint8Array[]; chunks: number } {
    const frames: Uint8Array[] = [];
    const reassembler = new Reassembler({ onStall: (reason) => { throw new Error(reason); } });
    const chunks = chunk(payload, maxChunkBytes);
    for (const piece of chunks) {
      expect(piece.length).toBeLessThanOrEqual(maxChunkBytes);
      expect(piece[0]).toBe(CHUNK_VERSION);
      const frame = reassembler.push(piece);
      if (frame) frames.push(frame);
    }
    reassembler.close();
    return { frames, chunks: chunks.length };
  }

  it('round trips one byte, an exact chunk boundary, and a megabyte', () => {
    const single = Uint8Array.from([42]);
    const oneByte = roundTrip(single, DATA_CHANNEL_MAX_CHUNK_BYTES);
    expect(oneByte.chunks).toBe(1);
    expect(oneByte.frames[0]).toEqual(single);

    const headroom = CHUNK_HEADER_BYTES + CHUNK_LENGTH_BYTES;
    const boundary = new Uint8Array(new ArrayBuffer(DATA_CHANNEL_MAX_CHUNK_BYTES - headroom));
    for (let index = 0; index < boundary.length; index += 1) boundary[index] = index % 251;
    const exact = roundTrip(boundary, DATA_CHANNEL_MAX_CHUNK_BYTES);
    expect(exact.chunks).toBe(1);
    expect(exact.frames[0]).toEqual(boundary);

    const overflow = new Uint8Array(new ArrayBuffer(boundary.length + 1));
    overflow.set(boundary);
    overflow[boundary.length] = 7;
    const split = roundTrip(overflow, DATA_CHANNEL_MAX_CHUNK_BYTES);
    expect(split.chunks).toBe(2);
    expect(split.frames[0]).toEqual(overflow);

    const large = new Uint8Array(new ArrayBuffer(1024 * 1024));
    for (let index = 0; index < large.length; index += 1) large[index] = index % 253;
    const megabyte = roundTrip(large, DATA_CHANNEL_MAX_CHUNK_BYTES);
    expect(megabyte.chunks).toBe(65);
    expect(megabyte.frames[0]).toHaveLength(large.length);
    expect(megabyte.frames[0].every((byte, index) => byte === large[index])).toBe(true);

    // The same format at the gateway ceiling needs far fewer chunks.
    const relayed = roundTrip(large, GATEWAY_MAX_CHUNK_BYTES);
    expect(relayed.chunks).toBe(5);
    expect(relayed.frames[0].every((byte, index) => byte === large[index])).toBe(true);
  });

  it('refuses to send more than the logical message cap or into a useless chunk size', () => {
    const oversized = { length: MAX_LOGICAL_BYTES + 1, subarray: () => new Uint8Array() } as unknown as Uint8Array;
    expect(() => chunk(oversized, DATA_CHANNEL_MAX_CHUNK_BYTES)).toThrow(/too large/);
    expect(() => chunk(Uint8Array.from([1]), CHUNK_HEADER_BYTES + CHUNK_LENGTH_BYTES)).toThrow(/too small/);
  });

  it('rejects an oversized declared length, a stray end chunk, and a stall', () => {
    const declaredTooLarge = new Uint8Array(new ArrayBuffer(CHUNK_HEADER_BYTES + CHUNK_LENGTH_BYTES));
    declaredTooLarge[0] = CHUNK_VERSION;
    declaredTooLarge[1] = 1;
    new DataView(declaredTooLarge.buffer).setUint32(CHUNK_HEADER_BYTES, MAX_LOGICAL_BYTES + 1, false);
    const stalls: string[] = [];
    const reassembler = new Reassembler({ onStall: (reason) => stalls.push(reason) });
    expect(() => reassembler.push(declaredTooLarge)).toThrow(/oversized/);

    const stray = new Reassembler({ onStall: (reason) => stalls.push(reason) });
    expect(() => stray.push(Uint8Array.from([CHUNK_VERSION, 2, 1, 2, 3]))).toThrow(/without a start chunk/);

    const twice = new Reassembler({ onStall: (reason) => stalls.push(reason) });
    const [head] = chunk(new Uint8Array(new ArrayBuffer(DATA_CHANNEL_MAX_CHUNK_BYTES * 2)), DATA_CHANNEL_MAX_CHUNK_BYTES);
    expect(twice.push(head)).toBeNull();
    expect(() => twice.push(head)).toThrow(/before finishing the previous one/);
    expect(stalls).toEqual([]);

    vi.useFakeTimers();
    try {
      const stalling = new Reassembler({ onStall: (reason) => stalls.push(reason) });
      expect(stalling.push(head)).toBeNull();
      vi.advanceTimersByTime(CHUNK_STALL_TIMEOUT_MS - 1);
      expect(stalls).toEqual([]);
      vi.advanceTimersByTime(1);
      expect(stalls).toHaveLength(1);
      expect(stalls[0]).toMatch(/stalled/);
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps handshake frames textual and data frames binary', () => {
    const hello = '{"type":"e2ee_server_hello"}';
    expect(decodeWireFrame(encodeWireFrame(hello))).toBe(hello);
    const data = Uint8Array.from([1, 0, 0, 0, 0, 0, 0, 0, 0, 7]);
    expect(decodeWireFrame(encodeWireFrame(data))).toBe(data);
  });

  /**
   * Cross-language vector against Go `internal/framing`. For a 600000-byte
   * payload of `index % 251`, the SHA-256 of every emitted chunk concatenated
   * in order is identical on both sides, which pins the header layout, the
   * length prefix endianness, the flag bits and the split points.
   */
  it.each([
    [DATA_CHANNEL_MAX_CHUNK_BYTES, 37, 'a7fe0733850aa24e8a09d89608a6324f9eda7ad301a6dd420fce5a772c6c88ab'],
    [GATEWAY_MAX_CHUNK_BYTES, 3, '80bd86466da304d108e6302c8e41cc2538d49b80879c87920b7a89b240077600'],
  ])('matches the Go chunk stream at %i bytes', async (maxChunkBytes, count, digest) => {
    const payload = new Uint8Array(new ArrayBuffer(600_000));
    for (let index = 0; index < payload.length; index += 1) payload[index] = index % 251;
    const chunks = chunk(payload, maxChunkBytes);
    expect(chunks).toHaveLength(count);
    const stream = new Uint8Array(new ArrayBuffer(chunks.reduce((total, piece) => total + piece.length, 0)));
    let offset = 0;
    for (const piece of chunks) {
      stream.set(piece, offset);
      offset += piece.length;
    }
    const hashed = new Uint8Array(await crypto.subtle.digest('SHA-256', stream));
    expect([...hashed].map((byte) => byte.toString(16).padStart(2, '0')).join('')).toBe(digest);
  });
});

class FakeDataChannel {
  binaryType = 'blob';
  bufferedAmount = 0;
  bufferedAmountLowThreshold = 0;
  closed = false;
  sent: Uint8Array[] = [];
  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onbufferedamountlow: (() => void) | null = null;
  onmessage: ((event: { data: ArrayBuffer | string }) => void) | null = null;

  constructor(readonly label: string, readonly options?: { ordered?: boolean }) {}

  send(chunk: Uint8Array): void {
    this.sent.push(chunk);
    this.bufferedAmount += chunk.length;
  }

  close(): void { this.closed = true; }

  /** Mimics the browser draining the send buffer below the low threshold. */
  drain(): void {
    this.bufferedAmount = 0;
    this.onbufferedamountlow?.();
  }

  receive(chunk: Uint8Array): void {
    this.onmessage?.({ data: chunk.buffer.slice(chunk.byteOffset, chunk.byteOffset + chunk.byteLength) as ArrayBuffer });
  }
}

class FakePeerConnection {
  static instances: FakePeerConnection[] = [];
  connectionState = 'new';
  localDescription: { type: string; sdp: string } | null = null;
  remoteDescription: { type: string; sdp: string } | null = null;
  channels: FakeDataChannel[] = [];
  candidates: RTCIceCandidateInit[] = [];
  restarts = 0;
  closed = false;
  onicecandidate: ((event: { candidate: RTCIceCandidate | null }) => void) | null = null;
  onconnectionstatechange: (() => void) | null = null;

  constructor(readonly config: RTCConfiguration) { FakePeerConnection.instances.push(this); }

  createDataChannel(label: string, options?: { ordered?: boolean }): FakeDataChannel {
    const channel = new FakeDataChannel(label, options);
    this.channels.push(channel);
    return channel;
  }

  createOffer(options?: { iceRestart?: boolean }): Promise<{ type: string; sdp: string }> {
    return Promise.resolve({ type: 'offer', sdp: options?.iceRestart ? 'v=0 restart' : 'v=0 offer' });
  }

  setLocalDescription(description: { type: string; sdp: string }): Promise<void> {
    this.localDescription = description;
    return Promise.resolve();
  }

  setRemoteDescription(description: { type: string; sdp: string }): Promise<void> {
    this.remoteDescription = description;
    return Promise.resolve();
  }

  addIceCandidate(candidate: RTCIceCandidateInit): Promise<void> {
    this.candidates.push(candidate);
    return Promise.resolve();
  }

  restartIce(): void { this.restarts += 1; }
  close(): void { this.closed = true; }

  setState(state: string): void {
    this.connectionState = state;
    this.onconnectionstatechange?.();
  }
}

interface FakeSignal extends SignalingChannel {
  sent: Record<string, any>[];
  deliver(message: Record<string, unknown>): void;
  reachable: boolean;
}

function fakeSignal(): FakeSignal {
  const subscribers = new Set<(message: Record<string, any>) => void>();
  const signal: FakeSignal = {
    sent: [],
    reachable: true,
    send(payload): boolean {
      if (!signal.reachable) return false;
      signal.sent.push(payload);
      return true;
    },
    onMessage(handler): () => void {
      subscribers.add(handler);
      return () => { subscribers.delete(handler); };
    },
    deliver(message): void {
      for (const handler of [...subscribers]) handler(message as Record<string, any>);
    },
  };
  return signal;
}

describe('webrtc data channel', () => {
  beforeEach(() => {
    FakePeerConnection.instances = [];
    vi.stubGlobal('RTCPeerConnection', FakePeerConnection);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('offers, trickles candidates, and moves chunked frames with backpressure', async () => {
    const signal = fakeSignal();
    const recorder = channelRecorder();
    const channel = createWebRTCChannel(signal, recorder);
    channel.open();

    const peer = FakePeerConnection.instances[0];
    expect(peer.config).toEqual({ iceServers: [] });
    const data = peer.channels[0];
    expect(data.label).toBe(DATA_CHANNEL_LABEL);
    expect(data.options).toEqual({ ordered: true });
    expect(data.binaryType).toBe('arraybuffer');
    expect(data.bufferedAmountLowThreshold).toBe(BUFFERED_AMOUNT_LOW);

    await vi.waitFor(() => expect(signal.sent).toHaveLength(1));
    const offer = signal.sent[0];
    expect(offer).toMatchObject({ type: 'webrtc_offer', sdp: 'v=0 offer' });
    const requestId = String(offer.request_id);
    expect(requestId).toBeTruthy();

    peer.onicecandidate?.({ candidate: { candidate: 'candidate:1 1 udp', sdpMid: '0', sdpMLineIndex: 0 } as RTCIceCandidate });
    expect(signal.sent[1]).toEqual({
      type: 'webrtc_ice',
      request_id: requestId,
      candidate: 'candidate:1 1 udp',
      sdp_mid: '0',
      sdp_mline_index: 0,
    });
    peer.onicecandidate?.({ candidate: null });
    expect(signal.sent).toHaveLength(2);

    // Candidates that arrive before the answer are queued, not dropped.
    signal.deliver({ type: 'webrtc_ice', request_id: requestId, candidate: 'candidate:2 1 udp', sdp_mid: '0', sdp_mline_index: 0 });
    expect(peer.candidates).toHaveLength(0);
    signal.deliver({ type: 'webrtc_answer', request_id: requestId, sdp: 'v=0 answer' });
    await vi.waitFor(() => expect(peer.candidates).toHaveLength(1));
    expect(peer.remoteDescription).toEqual({ type: 'answer', sdp: 'v=0 answer' });

    // Another relay's signalling on the same session is ignored.
    signal.deliver({ type: 'webrtc_answer', request_id: 'someone-else', sdp: 'v=0 other' });
    expect(peer.remoteDescription).toEqual({ type: 'answer', sdp: 'v=0 answer' });

    data.onopen?.();
    expect(recorder.opens).toBe(1);

    // The E2EE handshake is JSON even on a binary path: it must go out as a
    // chunked binary message, not as a DataChannel string.
    channel.sendFrame('{"type":"e2ee_client_hello"}');
    expect(data.sent).toHaveLength(1);
    expect(data.sent[0][1]).toBe(3);
    data.drain();

    const payload = new Uint8Array(new ArrayBuffer(5 * 1024 * 1024));
    for (let index = 0; index < payload.length; index += 1) payload[index] = index % 251;
    const expectedChunks = chunk(payload, DATA_CHANNEL_MAX_CHUNK_BYTES).length + 1;
    channel.sendFrame(payload);
    expect(data.bufferedAmount).toBeGreaterThanOrEqual(BUFFERED_AMOUNT_HIGH);
    const paused = data.sent.length;
    expect(paused).toBeLessThan(expectedChunks);
    data.drain();
    expect(data.sent.length).toBeGreaterThan(paused);
    while (data.sent.length < expectedChunks) data.drain();

    const echoed: Uint8Array[] = [];
    const reassembler = new Reassembler({ onStall: (reason) => { throw new Error(reason); } });
    for (const piece of data.sent) {
      expect(piece.length).toBeLessThanOrEqual(DATA_CHANNEL_MAX_CHUNK_BYTES);
      const frame = reassembler.push(piece);
      if (frame) echoed.push(frame);
    }
    reassembler.close();
    expect(echoed).toHaveLength(2);
    expect(new TextDecoder().decode(echoed[0])).toBe('{"type":"e2ee_client_hello"}');
    expect(echoed[1]).toHaveLength(payload.length);
    expect(echoed[1].every((byte, index) => byte === payload[index])).toBe(true);

    // Inbound: a JSON server hello surfaces as text, a data frame as bytes.
    for (const piece of chunk(encodeWireFrame('{"type":"e2ee_server_hello"}'), DATA_CHANNEL_MAX_CHUNK_BYTES)) {
      data.receive(piece);
    }
    expect(recorder.frames[0]).toBe('{"type":"e2ee_server_hello"}');
    for (const piece of chunk(Uint8Array.from([1, 0, 5, 5]), DATA_CHANNEL_MAX_CHUNK_BYTES)) data.receive(piece);
    expect(recorder.frames).toHaveLength(2);
    expect(Array.from(recorder.frames[1] as Uint8Array)).toEqual([1, 0, 5, 5]);

    signal.deliver({ type: 'webrtc_closed', request_id: requestId, reason: 'relay closed it' });
    expect(recorder.closes).toEqual([{ reason: 'relay closed it' }]);
    expect(peer.closed).toBe(true);
    expect(data.closed).toBe(true);
    // A remote close is not echoed back through the relayed session.
    expect(signal.sent.filter((message) => message.type === 'webrtc_close')).toHaveLength(0);
  });

  it('gives the peer connection the ICE servers it was handed', () => {
    // Off LAN the host candidates alone are unreachable, so the direct path
    // needs the address the gateway reflects back through address discovery.
    const iceServers: RTCIceServer[] = [{ urls: 'stun:gw.example.com:3478' }];
    createWebRTCChannel(fakeSignal(), channelRecorder(), iceServers).open();
    expect(FakePeerConnection.instances[0].config).toEqual({ iceServers });
  });

  it('restarts ICE once and then gives up', async () => {
    vi.useFakeTimers();
    try {
      const signal = fakeSignal();
      const recorder = channelRecorder();
      createWebRTCChannel(signal, recorder).open();
      const peer = FakePeerConnection.instances[0];
      await vi.waitFor(() => expect(signal.sent).toHaveLength(1));
      const requestId = String(signal.sent[0].request_id);
      peer.channels[0].onopen?.();

      peer.setState('disconnected');
      expect(peer.restarts).toBe(1);
      await vi.waitFor(() => expect(signal.sent).toHaveLength(2));
      expect(signal.sent[1]).toMatchObject({ type: 'webrtc_offer', request_id: requestId, sdp: 'v=0 restart' });

      // Recovery inside the grace window keeps the channel alive.
      peer.setState('connected');
      vi.advanceTimersByTime(ICE_RESTART_GRACE_MS + 1);
      expect(recorder.closes).toHaveLength(0);

      // A second failure is terminal, and the relay is told to tear down.
      peer.setState('failed');
      expect(peer.restarts).toBe(1);
      expect(recorder.closes).toHaveLength(1);
      expect(recorder.closes[0]?.fatal).toBeUndefined();
      expect(signal.sent.at(-1)).toEqual({ type: 'webrtc_close', request_id: requestId });
    } finally {
      vi.useRealTimers();
    }
  });

  it('gives up permanently when the relay refuses or the browser cannot offer', async () => {
    const signal = fakeSignal();
    const refused = channelRecorder();
    createWebRTCChannel(signal, refused).open();
    await vi.waitFor(() => expect(signal.sent).toHaveLength(1));
    signal.deliver({
      type: 'command_result',
      request_id: String(signal.sent[0].request_id),
      ok: false,
      error: 'Direct connections are disabled.',
    });
    expect(refused.closes).toEqual([{ reason: 'Direct connections are disabled.', fatal: true }]);

    vi.unstubAllGlobals();
    const unsupported = channelRecorder();
    createWebRTCChannel(fakeSignal(), unsupported).open();
    expect(unsupported.closes).toEqual([{ reason: expect.stringContaining('cannot open a direct connection'), fatal: true }]);
  });
});

interface FakeTransport extends RelayTransport {
  handlers: TransportHandlers;
  sent: Record<string, unknown>[];
  connects: number;
  closed: boolean;
}

interface StatusEvent {
  status: TransportStatus;
  detail?: TransportStatusDetail;
}

function fakeTransport(kind: FakeTransport['kind'], handlers: TransportHandlers): FakeTransport {
  const transport: FakeTransport = {
    kind,
    handlers,
    sent: [],
    connects: 0,
    closed: false,
    connect(): void {
      transport.connects += 1;
      handlers.onStatus('connecting');
    },
    send(payload): boolean {
      if (transport.closed) return false;
      transport.sent.push(payload);
      return true;
    },
    close(): void { transport.closed = true; },
  };
  return transport;
}

describe('hybrid path manager', () => {
  let statuses: StatusEvent[];
  let messages: Record<string, any>[];
  let gateways: FakeTransport[];
  let directs: FakeTransport[];
  let directOptions: DirectTransportOptions[];
  let signals: SignalingChannel[];
  let handlers: TransportHandlers;

  function build(): RelayTransport {
    return createHybridTransport(HYBRID_RELAY, handlers, {
      createGateway: (_relay, gatewayHandlers) => {
        const transport = fakeTransport('gateway', gatewayHandlers);
        gateways.push(transport);
        return transport;
      },
      createDirect: (_relay, signal, directHandlers, options) => {
        const transport = fakeTransport('webrtc', directHandlers);
        signals.push(signal);
        directs.push(transport);
        directOptions.push(options);
        return transport;
      },
    });
  }

  beforeEach(() => {
    vi.useFakeTimers();
    statuses = [];
    messages = [];
    gateways = [];
    directs = [];
    directOptions = [];
    signals = [];
    handlers = {
      onMessage: (message) => messages.push(message),
      onStatus: (status, detail) => statuses.push({ status, detail }),
    };
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('upgrades to direct on its first message and releases the gateway after ten seconds', () => {
    const transport = build();
    transport.connect();
    expect(statuses).toEqual([{ status: 'connecting', detail: undefined }]);
    expect(directs).toHaveLength(0);

    gateways[0].handlers.onStatus('connected');
    expect(statuses.at(-1)?.status).toBe('connected');
    expect(directs).toHaveLength(1);
    expect(directs[0].connects).toBe(1);
    // A gateway that advertises no STUN port leaves the direct path on host
    // candidates, exactly as it behaved before address discovery existed.
    expect(directOptions[0]).toEqual({ iceServers: [] });

    // Signalling rides the relayed session and never reaches the store.
    signals[0].send({ type: 'webrtc_offer', request_id: 'r1', sdp: 'v=0' });
    expect(gateways[0].sent.at(-1)).toMatchObject({ type: 'webrtc_offer' });
    gateways[0].handlers.onMessage({ type: 'webrtc_answer', request_id: 'r1', sdp: 'v=0' });
    gateways[0].handlers.onMessage({ type: 'agents', agents: [] });
    expect(messages).toEqual([{ type: 'agents', agents: [] }]);

    // A direct session that has handshaked but not spoken yet is not trusted.
    directs[0].handlers.onStatus('connected');
    expect(statuses.filter((event) => event.status === 'connected')).toHaveLength(1);
    expect(transport.send({ type: 'refresh_agents' })).toBe(true);
    expect(gateways[0].sent.at(-1)).toMatchObject({ type: 'refresh_agents' });

    directs[0].handlers.onMessage({ type: 'push_config', protocol: 2 });
    expect(statuses.filter((event) => event.status === 'connected')).toHaveLength(2);
    expect(messages.at(-1)).toMatchObject({ type: 'push_config' });
    expect(transport.send({ type: 'refresh_agents' })).toBe(true);
    expect(directs[0].sent.at(-1)).toMatchObject({ type: 'refresh_agents' });

    // The relayed session stays up for ten seconds of direct stability.
    vi.advanceTimersByTime(DIRECT_STABILITY_MS - 1);
    expect(gateways[0].closed).toBe(false);
    vi.advanceTimersByTime(1);
    expect(gateways[0].closed).toBe(true);

    // A message from the drained gateway must not reach the store any more.
    gateways[0].handlers.onMessage({ type: 'agents', agents: ['stale'] });
    expect(messages.at(-1)).toMatchObject({ type: 'push_config' });
  });

  it('falls back to a fresh gateway when the direct path dies and retries with backoff', () => {
    const transport = build();
    transport.connect();
    gateways[0].handlers.onStatus('connected');
    directs[0].handlers.onStatus('connected');
    directs[0].handlers.onMessage({ type: 'push_config', protocol: 2 });
    vi.advanceTimersByTime(DIRECT_STABILITY_MS);
    expect(gateways[0].closed).toBe(true);

    directs[0].handlers.onStatus('closed', { reason: 'The direct connection closed.' });
    expect(statuses.at(-1)?.status).toBe('connecting');
    expect(gateways).toHaveLength(2);
    expect(gateways[1].connects).toBe(1);
    expect(directs[0].closed).toBe(true);

    gateways[1].handlers.onStatus('connected');
    expect(statuses.at(-1)?.status).toBe('connected');
    expect(transport.send({ type: 'refresh_agents' })).toBe(true);
    expect(gateways[1].sent.at(-1)).toMatchObject({ type: 'refresh_agents' });

    // The retry is deferred by the backoff, never fired inline.
    expect(directs).toHaveLength(1);
    vi.advanceTimersByTime(DIRECT_RETRY_BASE_MS - 1);
    expect(directs).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(directs).toHaveLength(2);

    // A second failure doubles the delay.
    directs[1].handlers.onStatus('closed', { reason: 'ICE failed' });
    vi.advanceTimersByTime(DIRECT_RETRY_BASE_MS * 2 - 1);
    expect(directs).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(directs).toHaveLength(3);

    // A fatal direct failure stops the direct path for good.
    directs[2].handlers.onStatus('closed', { reason: 'No WebRTC on the relay', fatal: true });
    vi.advanceTimersByTime(10 * 60_000);
    expect(directs).toHaveLength(3);
    expect(statuses.at(-1)?.status).toBe('connected');

    transport.close();
    expect(gateways[1].closed).toBe(true);
  });

  it('holds the direct retry until the replacement relayed session is up', () => {
    const transport = build();
    transport.connect();
    gateways[0].handlers.onStatus('connected');
    directs[0].handlers.onStatus('connected');
    directs[0].handlers.onMessage({ type: 'push_config', protocol: 2 });
    vi.advanceTimersByTime(DIRECT_STABILITY_MS);
    directs[0].handlers.onStatus('closed', { reason: 'The direct connection closed.' });

    // The replacement gateway is still handshaking, so signalling has nowhere
    // to go and the retry must not burn an attempt.
    expect(gateways).toHaveLength(2);
    vi.advanceTimersByTime(60_000);
    expect(directs).toHaveLength(1);

    gateways[1].handlers.onStatus('connected');
    expect(directs).toHaveLength(2);
    expect(statuses.at(-1)?.status).toBe('connected');
    expect(transport.send({ type: 'refresh_agents' })).toBe(true);
    expect(gateways[1].sent.at(-1)).toMatchObject({ type: 'refresh_agents' });
  });

  it('reports a dead relayed session as closed while it is the active path', () => {
    const transport = build();
    transport.connect();
    gateways[0].handlers.onStatus('connected');
    gateways[0].handlers.onStatus('closed', { reason: 'The gateway connection closed.' });
    expect(statuses.at(-1)).toEqual({ status: 'closed', detail: { reason: 'The gateway connection closed.' } });
    expect(directs[0].closed).toBe(true);
    expect(transport.send({ type: 'refresh_agents' })).toBe(false);
  });

  it('skips the direct attempt entirely when forced onto the relayed path', () => {
    localStorage.setItem(FORCE_RELAY_KEY, '1');
    const transport = build();
    transport.connect();
    gateways[0].handlers.onStatus('connected');
    vi.advanceTimersByTime(10 * 60_000);
    expect(directs).toHaveLength(0);
    expect(transport.send({ type: 'refresh_agents' })).toBe(true);
    expect(gateways[0].sent).toHaveLength(1);
  });

  it('falls back to the legacy relay URL when a migrated relay cannot reach its gateway', () => {
    const legacies: FakeTransport[] = [];
    const transport = createHybridTransport(
      { ...HYBRID_RELAY, url: 'wss://fedora.example' },
      handlers,
      {
        createGateway: (_relay, gatewayHandlers) => {
          const fake = fakeTransport('gateway', gatewayHandlers);
          gateways.push(fake);
          return fake;
        },
        createDirect: (_relay, signal, directHandlers) => {
          const fake = fakeTransport('webrtc', directHandlers);
          signals.push(signal);
          directs.push(fake);
          return fake;
        },
        createLegacy: (_relay, legacyHandlers) => {
          const fake = fakeTransport('websocket', legacyHandlers);
          legacies.push(fake);
          return fake;
        },
      },
    );
    transport.connect();
    // A bridge-window relay advertised its gateway, but this phone's network
    // cannot reach it. The path that was working before the migration must
    // still carry traffic instead of stranding the user.
    gateways[0].handlers.onStatus('closed', { reason: 'unknown relay', fatal: true });
    expect(legacies).toHaveLength(1);
    expect(statuses.at(-1)?.status).toBe('connecting');

    legacies[0].handlers.onStatus('connected', { path: 'websocket' });
    expect(statuses.at(-1)).toEqual({ status: 'connected', detail: { path: 'websocket' } });
    expect(transport.send({ type: 'refresh_agents' })).toBe(true);
    expect(legacies[0].sent.at(-1)).toMatchObject({ type: 'refresh_agents' });

    legacies[0].handlers.onMessage({ type: 'agents', agents: [] });
    expect(messages).toEqual([{ type: 'agents', agents: [] }]);
  });

  it('does not fall back when a hybrid-only relay has no legacy URL', () => {
    const legacies: FakeTransport[] = [];
    const transport = createHybridTransport(HYBRID_RELAY, handlers, {
      createGateway: (_relay, gatewayHandlers) => {
        const fake = fakeTransport('gateway', gatewayHandlers);
        gateways.push(fake);
        return fake;
      },
      createDirect: (_relay, signal, directHandlers) => {
        const fake = fakeTransport('webrtc', directHandlers);
        signals.push(signal);
        directs.push(fake);
        return fake;
      },
      createLegacy: (_relay, legacyHandlers) => {
        const fake = fakeTransport('websocket', legacyHandlers);
        legacies.push(fake);
        return fake;
      },
    });
    transport.connect();
    gateways[0].handlers.onStatus('closed', { reason: 'unknown relay', fatal: true });
    expect(legacies).toHaveLength(0);
    expect(statuses.at(-1)).toEqual({ status: 'closed', detail: { reason: 'unknown relay', fatal: true } });
  });

  /**
   * Ordered gateway list. Every entry is dialed in turn, so a phone whose
   * preferred gateway is down still reaches its computer, and the entry that
   * answered — never the configured primary — is the one address discovery may
   * hand to the direct path.
   */
  it('moves to the next gateway when the preferred one fails, and wraps after a drop', () => {
    const dialed: string[] = [];
    const transport = createHybridTransport(
      { ...HYBRID_RELAY, gatewayUrl: 'wss://a.example', gatewayUrls: ['wss://a.example', 'wss://b.example'] },
      handlers,
      {
        createGateway: (relay, gatewayHandlers) => {
          dialed.push(String(relay.gatewayUrl));
          const fake = fakeTransport('gateway', gatewayHandlers);
          gateways.push(fake);
          return fake;
        },
        createDirect: (_relay, signal, directHandlers, options) => {
          const fake = fakeTransport('webrtc', directHandlers);
          signals.push(signal);
          directs.push(fake);
          directOptions.push(options);
          return fake;
        },
      },
    );
    transport.connect();
    expect(dialed).toEqual(['wss://a.example']);

    // The preferred gateway is unreachable, so the next entry takes the attempt
    // instead of the phone waiting out a whole reconnect cycle.
    gateways[0].handlers.onStatus('closed', { reason: 'The gateway refused the connection.' });
    expect(dialed).toEqual(['wss://a.example', 'wss://b.example']);
    // The switch is reported with the reason, then the replacement session
    // announces its own handshake.
    expect(statuses.slice(-2)).toEqual([
      { status: 'connecting', detail: { reason: 'The gateway refused the connection.' } },
      { status: 'connecting', detail: undefined },
    ]);

    gateways[1].handlers.onStatus('connected', { path: 'gateway', stunPort: 3478 });
    expect(statuses.at(-1)?.status).toBe('connected');
    expect(transport.send({ type: 'refresh_agents' })).toBe(true);
    expect(gateways[1].sent.at(-1)).toMatchObject({ type: 'refresh_agents' });
    expect(directOptions[0]).toEqual({ iceServers: [{ urls: 'stun:b.example:3478' }] });

    // A session that came up earns the list a fresh pass, so the drop wraps
    // back to the preferred entry rather than ending the transport.
    gateways[1].handlers.onStatus('closed', { reason: 'The gateway connection closed.' });
    expect(dialed).toEqual(['wss://a.example', 'wss://b.example', 'wss://a.example']);
    expect(statuses.at(-1)?.status).toBe('connecting');
  });

  it('tries every gateway before the legacy relay URL and before giving up', () => {
    const dialed: string[] = [];
    const legacies: FakeTransport[] = [];
    const overrides = {
      createGateway: (relay: RelayConfig, gatewayHandlers: TransportHandlers) => {
        dialed.push(String(relay.gatewayUrl));
        const fake = fakeTransport('gateway', gatewayHandlers);
        gateways.push(fake);
        return fake;
      },
      createDirect: (_relay: RelayConfig, signal: SignalingChannel, directHandlers: TransportHandlers) => {
        const fake = fakeTransport('webrtc', directHandlers);
        signals.push(signal);
        directs.push(fake);
        return fake;
      },
      createLegacy: (_relay: RelayConfig, legacyHandlers: TransportHandlers) => {
        const fake = fakeTransport('websocket', legacyHandlers);
        legacies.push(fake);
        return fake;
      },
    };
    const gatewayUrls = ['wss://a.example', 'wss://b.example'];
    const migrated = createHybridTransport(
      { ...HYBRID_RELAY, url: 'wss://fedora.example', gatewayUrl: gatewayUrls[0], gatewayUrls },
      handlers,
      overrides,
    );
    migrated.connect();
    // A gateway that does not know this computer is fatal for that gateway
    // alone: the next one may well have the relay registered.
    const unknown = { reason: 'That computer is not registered with this gateway.', fatal: true };
    gateways[0].handlers.onStatus('closed', unknown);
    expect(dialed).toEqual(gatewayUrls);
    expect(legacies).toHaveLength(0);

    // Once the list is exhausted the migrated relay's own URL is the last hope.
    gateways[1].handlers.onStatus('closed', unknown);
    expect(legacies).toHaveLength(1);
    legacies[0].handlers.onStatus('connected', { path: 'websocket' });
    expect(statuses.at(-1)).toEqual({ status: 'connected', detail: { path: 'websocket' } });
    migrated.close();

    gateways.length = 0;
    dialed.length = 0;
    statuses.length = 0;
    const hybridOnly = createHybridTransport({ ...HYBRID_RELAY, gatewayUrl: gatewayUrls[0], gatewayUrls }, handlers, overrides);
    hybridOnly.connect();
    gateways[0].handlers.onStatus('closed', { reason: 'The gateway connection closed.' });
    gateways[1].handlers.onStatus('closed', { reason: 'The gateway connection closed.' });
    expect(dialed).toEqual(gatewayUrls);
    expect(statuses.at(-1)).toEqual({ status: 'closed', detail: { reason: 'The gateway connection closed.' } });
    expect(hybridOnly.send({ type: 'refresh_agents' })).toBe(false);
  });

  /**
   * Address discovery, hello to peer connection. Only the port is advertised:
   * the host is always the one this phone dialed, so a gateway can neither aim
   * a peer at a server of its choosing nor learn an address it does not
   * already see as the source of the relayed socket.
   */
  const DISCOVERY_CASES: [string, Record<string, unknown>, string, RTCIceServer[]][] = [
    ['pairs an advertised port with the dialed gateway host', { stun_port: 3478 }, 'wss://gw.example.com', [{ urls: 'stun:gw.example.com:3478' }]],
    ['ignores a host smuggled into the hello', { stun_port: 3478, stun_host: 'evil.example.com', stun_addr: 'evil.example.com:3478' }, 'wss://gw.example.com', [{ urls: 'stun:gw.example.com:3478' }]],
    ['takes the host from a gateway URL with its own port and path', { stun_port: 19302 }, 'wss://relay.example.net:8443/gw', [{ urls: 'stun:relay.example.net:19302' }]],
    ['stays on host candidates without an advertised port', {}, 'wss://gw.example.com', []],
    ['stays on host candidates for a zero port', { stun_port: 0 }, 'wss://gw.example.com', []],
    ['stays on host candidates for an out-of-range port', { stun_port: 70000 }, 'wss://gw.example.com', []],
    ['stays on host candidates for a negative port', { stun_port: -1 }, 'wss://gw.example.com', []],
    ['stays on host candidates for a port sent as a string', { stun_port: '3478' }, 'wss://gw.example.com', []],
    ['stays on host candidates for a fractional port', { stun_port: 3478.5 }, 'wss://gw.example.com', []],
  ];

  it.each(DISCOVERY_CASES)('%s', async (_label, hello, gatewayUrl, expected) => {
    // The real relayed channel parses the hello here, so these cases cover the
    // wire format rather than a hand-written status detail.
    vi.useRealTimers();
    MockGatewaySocket.instances = [];
    vi.stubGlobal('WebSocket', MockGatewaySocket);
    const transport = createHybridTransport({ ...HYBRID_RELAY, gatewayUrl }, handlers, {
      createGateway: (relay, gatewayHandlers) => {
        // Stands in for the encrypted transport: the relayed session becomes
        // usable once the gateway says ready, and the port read out of the
        // hello rides the status the path manager already waits for.
        let stunPort = 0;
        const channel = createGatewayChannel(relay, {
          onOpen: () => gatewayHandlers.onStatus('connected', { path: 'gateway', stunPort }),
          onFrame: () => {},
          onClose: (detail) => gatewayHandlers.onStatus('closed', detail),
        }, { onStunPort: (port) => { stunPort = port; } });
        return {
          kind: 'gateway',
          connect: () => channel.open(),
          send: () => true,
          close: () => channel.close(),
        };
      },
      createDirect: (_relay, signal, directHandlers, options) => {
        const fake = fakeTransport('webrtc', directHandlers);
        signals.push(signal);
        directs.push(fake);
        directOptions.push(options);
        return fake;
      },
    });
    transport.connect();

    const socket = MockGatewaySocket.instances[0];
    socket.text({ type: 'gateway_hello', proto: 1, nonce: VECTOR_NONCE, ...hello });
    await vi.waitFor(() => expect(socket.sent).toHaveLength(1));
    socket.text({ type: 'ready', proto: 1 });

    expect(directs).toHaveLength(1);
    expect(directOptions[0]).toEqual({ iceServers: expected });

    transport.close();
    vi.unstubAllGlobals();
  });
});

describe('hybrid relay configuration', () => {
  it('imports a gateway setup link and keeps every secret in the fragment', () => {
    const setup = quickSetupConfig({
      hash: `#setup=${VECTOR_TOKEN}&label=Fedora&gateway=wss%3A%2F%2Fgw.example.com`,
      protocol: 'https:',
      host: 'app.example.com',
    } as Location);
    expect(setup).toEqual({
      label: 'Fedora',
      url: '',
      token: VECTOR_TOKEN,
      transport: 'hybrid',
      gatewayUrl: 'wss://gw.example.com',
      gatewayUrls: ['wss://gw.example.com'],
    });

    const imported = importQuickSetup([], {
      hash: `#setup=${VECTOR_TOKEN}&label=Fedora&gateway=wss%3A%2F%2Fgw.example.com%2F`,
      protocol: 'https:',
      host: 'app.example.com',
    } as Location);
    expect(imported).toHaveLength(1);
    expect(imported?.[0]).toMatchObject({
      label: 'Fedora',
      url: '',
      transport: 'hybrid',
      gatewayUrl: 'wss://gw.example.com',
      token: VECTOR_TOKEN,
    });
    expect(imported?.[0].id).toBe('fedora-wss-gw-example-com');

    // Re-scanning the same code updates in place instead of duplicating.
    const again = importQuickSetup(imported!, {
      hash: `#setup=${VECTOR_TOKEN}&label=Fedora&gateway=wss%3A%2F%2Fgw.example.com`,
      protocol: 'https:',
      host: 'app.example.com',
    } as Location);
    expect(again).toHaveLength(1);
    expect(again?.[0].id).toBe(imported?.[0].id);
  });

  it.each([
    'gateway=http%3A%2F%2Fgw.example.com',
    'gateway=ws%3A%2F%2Fgw.example.com',
    'gateway=wss%3A%2F%2Fuser%3Apass%40gw.example.com',
    'gateway=wss%3A%2F%2Fgw.example.com%2Fconnect',
    'gateway=wss%3A%2F%2Fgw.example.com%3Fkey%3Dleak',
    'gateway=wss%3A%2F%2Fgw.example.com%23leak',
    'gateway=javascript%3Aalert(1)',
  ])('rejects the hostile gateway parameter %s', (fragment) => {
    expect(quickSetupConfig({
      hash: `#setup=${VECTOR_TOKEN}&${fragment}`,
      protocol: 'https:',
      host: 'app.example.com',
    } as Location)).toBeNull();
  });

  it('keeps legacy websocket entries untouched and round trips hybrid ones', () => {
    const legacy = normalizeRelayConfig({ label: 'Mac', url: 'wss://mac.example', token: 'abc' });
    expect(legacy).toEqual({ id: 'mac-wss-mac-example', label: 'Mac', url: 'wss://mac.example', token: 'abc' });
    expect('transport' in legacy).toBe(false);

    const hybrid = normalizeRelayConfig({ label: 'Fedora', url: '', token: 'abc', gatewayUrl: 'wss://gw.example.com' });
    expect(hybrid.transport).toBe('hybrid');
    expect(hybrid.id).toBe(makeRelayId('Fedora', '', 'wss://gw.example.com'));

    saveRelayConfigs([legacy, hybrid]);
    expect(loadRelayConfigs()).toEqual([legacy, hybrid]);

    // Entries with neither address are still dropped.
    localStorage.setItem('herdr_relays', JSON.stringify([{ label: 'Broken', token: 'abc' }, legacy]));
    expect(loadRelayConfigs()).toEqual([legacy]);
  });
});
