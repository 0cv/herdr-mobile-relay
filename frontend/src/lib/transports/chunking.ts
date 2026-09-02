import { hasBinaryFrameHeader, type E2EEWireFrame } from '../e2ee';

/**
 * Chunk framing shared by every binary-codec transport.
 *
 *   chunk = [u8 version][u8 flags][u32be logical_len when START][payload]
 *
 * Both carriers are reliable and ordered, so there is no message id and
 * exactly one logical message is in flight per direction: START opens an
 * assembly, END closes it, and a single-chunk message carries both flags.
 * Only the chunk ceiling differs — the DataChannel is bounded by SCTP, the
 * gateway link by the relay's per-frame copy budget.
 */
export const CHUNK_VERSION = 1;
export const CHUNK_HEADER_BYTES = 2;
export const CHUNK_LENGTH_BYTES = 4;
/** Mirrors the relay's `wsMaxReadBytes` cap on one logical message. */
export const MAX_LOGICAL_BYTES = 21 * 1024 * 1024;
/** An assembly that makes no progress for this long is abandoned. */
export const CHUNK_STALL_TIMEOUT_MS = 30_000;

const FLAG_START = 1;
const FLAG_END = 2;
const encoder = new TextEncoder();
const decoder = new TextDecoder('utf-8', { fatal: true });

/** Splits one logical frame into wire chunks of at most `maxChunkBytes`. */
export function chunk(logical: Uint8Array, maxChunkBytes: number): Uint8Array<ArrayBuffer>[] {
  if (maxChunkBytes <= CHUNK_HEADER_BYTES + CHUNK_LENGTH_BYTES) {
    throw new Error('Chunk size is too small to carry a message.');
  }
  if (logical.length > MAX_LOGICAL_BYTES) throw new Error('Message is too large to send.');
  const chunks: Uint8Array<ArrayBuffer>[] = [];
  let offset = 0;
  let start = true;
  do {
    const header = start ? CHUNK_HEADER_BYTES + CHUNK_LENGTH_BYTES : CHUNK_HEADER_BYTES;
    const take = Math.min(maxChunkBytes - header, logical.length - offset);
    const end = offset + take >= logical.length;
    const piece = new Uint8Array(new ArrayBuffer(header + take));
    piece[0] = CHUNK_VERSION;
    piece[1] = (start ? FLAG_START : 0) | (end ? FLAG_END : 0);
    if (start) new DataView(piece.buffer).setUint32(CHUNK_HEADER_BYTES, logical.length, false);
    piece.set(logical.subarray(offset, offset + take), header);
    chunks.push(piece);
    offset += take;
    start = false;
  } while (offset < logical.length);
  return chunks;
}

export interface ReassemblerOptions {
  /**
   * Reports an assembly that made no progress in time. A stall cannot be
   * raised from `push`, so the transport learns about it here and closes.
   */
  onStall(reason: string): void;
  stallTimeoutMs?: number;
}

/**
 * Rebuilds logical frames from wire chunks. `push` returns the completed frame
 * or null while a message is still arriving, and throws on any framing
 * violation — an oversized declaration, a chunk out of START/END order, or
 * more data than was declared. Every violation is fatal for the carrier.
 */
export class Reassembler {
  private buffer: Uint8Array<ArrayBuffer> | null = null;
  private received = 0;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private failed = false;

  constructor(private readonly options: ReassemblerOptions) {}

  push(piece: Uint8Array): Uint8Array<ArrayBuffer> | null {
    if (this.failed) return null;
    if (piece.length < CHUNK_HEADER_BYTES) this.reject('The peer sent a truncated chunk.');
    if (piece[0] !== CHUNK_VERSION) this.reject('The peer sent an unsupported chunk.');
    const flags = piece[1];
    if ((flags & ~(FLAG_START | FLAG_END)) !== 0) this.reject('The peer sent unknown chunk flags.');
    let body: Uint8Array;
    if ((flags & FLAG_START) !== 0) {
      if (this.buffer) this.reject('The peer started a message before finishing the previous one.');
      if (piece.length < CHUNK_HEADER_BYTES + CHUNK_LENGTH_BYTES) this.reject('The peer sent a truncated chunk.');
      const declared = new DataView(piece.buffer, piece.byteOffset).getUint32(CHUNK_HEADER_BYTES, false);
      if (declared > MAX_LOGICAL_BYTES) this.reject('The peer sent an oversized message.');
      this.buffer = new Uint8Array(new ArrayBuffer(declared));
      this.received = 0;
      body = piece.subarray(CHUNK_HEADER_BYTES + CHUNK_LENGTH_BYTES);
    } else {
      if (!this.buffer) this.reject('The peer sent a chunk without a start chunk.');
      body = piece.subarray(CHUNK_HEADER_BYTES);
    }
    const buffer = this.buffer!;
    if (this.received + body.length > buffer.length) this.reject('The peer sent more data than it declared.');
    buffer.set(body, this.received);
    this.received += body.length;
    clearTimeout(this.timer ?? undefined);
    this.timer = setTimeout(() => {
      this.failed = true;
      this.discard();
      this.options.onStall('The peer stalled halfway through a message.');
    }, this.options.stallTimeoutMs ?? CHUNK_STALL_TIMEOUT_MS);
    if ((flags & FLAG_END) === 0) return null;
    if (this.received !== buffer.length) this.reject('The peer sent less data than it declared.');
    this.discard();
    return buffer;
  }

  close(): void {
    this.failed = true;
    this.discard();
  }

  private discard(): void {
    this.buffer = null;
    this.received = 0;
    clearTimeout(this.timer ?? undefined);
    this.timer = null;
  }

  private reject(reason: string): never {
    this.failed = true;
    this.discard();
    throw new Error(reason);
  }
}

/**
 * The E2EE handshake is JSON on every transport, including the binary-codec
 * ones: the relay writes its server hello as raw JSON bytes and reads the
 * client hello the same way. Encrypted data frames always start with the
 * binary codec header, so the two shapes are unambiguous.
 */
export function encodeWireFrame(frame: E2EEWireFrame): Uint8Array<ArrayBuffer> {
  return typeof frame === 'string' ? encoder.encode(frame) : frame;
}

export function decodeWireFrame(payload: Uint8Array<ArrayBuffer>): E2EEWireFrame {
  return hasBinaryFrameHeader(payload) ? payload : decoder.decode(payload);
}
