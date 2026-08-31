import type { ApiError, TargetRef } from './types';

export const DEFAULT_ATTACHMENT_MIME_TYPES = new Set([
  'application/json',
  'application/pdf',
  'application/vnd.oasis.opendocument.presentation',
  'application/vnd.oasis.opendocument.spreadsheet',
  'application/vnd.oasis.opendocument.text',
  'application/vnd.openxmlformats-officedocument.presentationml.presentation',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  'image/gif',
  'image/jpeg',
  'image/png',
  'image/webp',
  'text/csv',
  'text/markdown',
  'text/plain',
]);

export interface AttachmentLimits {
  maxFiles: number;
  maxFileBytes: number;
  maxBatchBytes: number;
  maxChunkBytes: number;
}

export interface AttachmentIssue {
  code:
    | 'attachment_batch_limit'
    | 'attachment_batch_too_large'
    | 'attachment_cancel_failed'
    | 'attachment_file_empty'
    | 'attachment_file_too_large'
    | 'attachment_invalid_name'
    | 'attachment_invalid_response'
    | 'attachment_unknown_mime'
    | 'attachment_upload_expired'
    | 'attachment_upload_failed'
    | 'attachment_upload_state_unknown';
  args?: Record<string, string | number | boolean>;
}

export interface AttachmentRef {
  ref: string;
  name: string;
  media_type: string;
  bytes: number;
  sha256: string;
  expires_at: string;
}

export interface UploadBeginRequest {
  target: TargetRef;
  files: Array<{ name: string; media_type: string; bytes: number }>;
}

export interface UploadBeginResult {
  upload_id: string;
  chunk_bytes: number;
  expires_at: string;
  limits: { max_files: number; max_file_bytes: number; max_batch_bytes: number };
}

export interface UploadChunkRequest {
  target: TargetRef;
  upload_id: string;
  file_index: number;
  sequence: number;
  data: Uint8Array;
  sha256: string;
}

export interface UploadChunkResult {
  file_index: number;
  next_sequence: number;
  received_bytes: number;
}

export interface UploadFinishRequest {
  target: TargetRef;
  upload_id: string;
  files: Array<{ file_index: number; sha256: string }>;
}

export interface UploadFinishResult {
  attachments: AttachmentRef[];
}

export interface AttachmentUploadCallbacks {
  begin(request: UploadBeginRequest, signal: AbortSignal): Promise<UploadBeginResult>;
  chunk(request: UploadChunkRequest, signal: AbortSignal): Promise<UploadChunkResult>;
  finish(request: UploadFinishRequest, signal: AbortSignal): Promise<UploadFinishResult>;
  cancel(request: { target: TargetRef; upload_id: string }): Promise<void>;
}

export type AttachmentItemState = 'selected' | 'uploading' | 'ready' | 'rejected' | 'interrupted';

export interface AttachmentItem {
  clientId: string;
  name: string;
  mediaType: string;
  bytes: number;
  order: number;
  state: AttachmentItemState;
  uploadedBytes: number;
  progress: number;
  issue?: AttachmentIssue;
  attachment?: AttachmentRef;
}

export interface AttachmentBatchSnapshot {
  items: AttachmentItem[];
  issue?: AttachmentIssue;
  uploading: boolean;
  canUpload: boolean;
  canRestart: boolean;
}

type InternalItem = AttachmentItem & { file?: File; digest?: string };
type Listener = (snapshot: AttachmentBatchSnapshot) => void;

function normalizedMime(value: string): string {
  return value.split(';', 1)[0].trim().toLowerCase();
}

function validFilename(value: string): boolean {
  return value.length > 0 && value.length <= 255 && value !== '.' && value !== '..'
    && !/[\u0000-\u001f/\\]/u.test(value);
}

function validLimit(value: number): boolean {
  return Number.isSafeInteger(value) && value > 0;
}

function hex(bytes: Uint8Array): string {
  let output = '';
  for (const byte of bytes) output += byte.toString(16).padStart(2, '0');
  return output;
}

function issueFrom(error: unknown, fallback: AttachmentIssue['code']): AttachmentIssue {
  if (error && typeof error === 'object') {
    const api = error as Partial<ApiError>;
    if (typeof api.code === 'string' && api.code.startsWith('attachment_')) {
      return { code: api.code as AttachmentIssue['code'], args: api.args };
    }
    const nested = (error as { error?: Partial<ApiError> }).error;
    if (nested && typeof nested.code === 'string' && nested.code.startsWith('attachment_')) {
      return { code: nested.code as AttachmentIssue['code'], args: nested.args };
    }
  }
  return { code: fallback };
}

function publicItem(item: InternalItem): AttachmentItem {
  const value = { ...item };
  delete value.file;
  delete value.digest;
  return { ...value, issue: value.issue ? { ...value.issue, args: value.issue.args ? { ...value.issue.args } : undefined } : undefined };
}

/** Incremental SHA-256 keeps whole files out of JS memory while still hashing the final body. */
export class IncrementalSha256 {
  private state = new Uint32Array([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
    0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ]);
  private buffer = new Uint8Array(64);
  private buffered = 0;
  private totalBytes = 0;
  private finished = false;

  update(input: Uint8Array): void {
    if (this.finished) throw new Error('sha256_already_finished');
    this.totalBytes += input.byteLength;
    let offset = 0;
    while (offset < input.byteLength) {
      const amount = Math.min(64 - this.buffered, input.byteLength - offset);
      this.buffer.set(input.subarray(offset, offset + amount), this.buffered);
      this.buffered += amount;
      offset += amount;
      if (this.buffered === 64) {
        this.compress(this.buffer);
        this.buffered = 0;
      }
    }
  }

  digestHex(): string {
    if (this.finished) throw new Error('sha256_already_finished');
    this.finished = true;
    const bitHigh = Math.floor(this.totalBytes / 0x20000000);
    const bitLow = (this.totalBytes << 3) >>> 0;
    this.buffer[this.buffered++] = 0x80;
    if (this.buffered > 56) {
      this.buffer.fill(0, this.buffered);
      this.compress(this.buffer);
      this.buffered = 0;
    }
    this.buffer.fill(0, this.buffered, 56);
    const view = new DataView(this.buffer.buffer);
    view.setUint32(56, bitHigh, false);
    view.setUint32(60, bitLow, false);
    this.compress(this.buffer);
    const result = new Uint8Array(32);
    const resultView = new DataView(result.buffer);
    for (let index = 0; index < this.state.length; index += 1) resultView.setUint32(index * 4, this.state[index], false);
    return hex(result);
  }

  private compress(block: Uint8Array): void {
    const constants = SHA256_CONSTANTS;
    const words = new Uint32Array(64);
    const view = new DataView(block.buffer, block.byteOffset, block.byteLength);
    for (let index = 0; index < 16; index += 1) words[index] = view.getUint32(index * 4, false);
    for (let index = 16; index < 64; index += 1) {
      const x = words[index - 15];
      const y = words[index - 2];
      const s0 = ((x >>> 7) | (x << 25)) ^ ((x >>> 18) | (x << 14)) ^ (x >>> 3);
      const s1 = ((y >>> 17) | (y << 15)) ^ ((y >>> 19) | (y << 13)) ^ (y >>> 10);
      words[index] = (words[index - 16] + s0 + words[index - 7] + s1) >>> 0;
    }
    let [a, b, c, d, e, f, g, h] = this.state;
    for (let index = 0; index < 64; index += 1) {
      const s1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
      const choice = (e & f) ^ (~e & g);
      const temp1 = (h + s1 + choice + constants[index] + words[index]) >>> 0;
      const s0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
      const majority = (a & b) ^ (a & c) ^ (b & c);
      const temp2 = (s0 + majority) >>> 0;
      h = g; g = f; f = e; e = (d + temp1) >>> 0;
      d = c; c = b; b = a; a = (temp1 + temp2) >>> 0;
    }
    this.state[0] = (this.state[0] + a) >>> 0;
    this.state[1] = (this.state[1] + b) >>> 0;
    this.state[2] = (this.state[2] + c) >>> 0;
    this.state[3] = (this.state[3] + d) >>> 0;
    this.state[4] = (this.state[4] + e) >>> 0;
    this.state[5] = (this.state[5] + f) >>> 0;
    this.state[6] = (this.state[6] + g) >>> 0;
    this.state[7] = (this.state[7] + h) >>> 0;
  }
}

const SHA256_CONSTANTS = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

export class AttachmentBatchController {
  private items: InternalItem[] = [];
  private listeners = new Set<Listener>();
  private batchIssue?: AttachmentIssue;
  private active?: { uploadId: string; expiresAt: number; abort: AbortController };
  private epoch = 0;
  private uploading = false;

  constructor(
    private readonly target: TargetRef,
    private readonly callbacks: AttachmentUploadCallbacks,
    private readonly limits: AttachmentLimits,
    private readonly allowedMimes: ReadonlySet<string> = DEFAULT_ATTACHMENT_MIME_TYPES,
  ) {
    if (![limits.maxFiles, limits.maxFileBytes, limits.maxBatchBytes, limits.maxChunkBytes].every(validLimit)) {
      throw new Error('attachment_invalid_limits');
    }
  }

  subscribe(listener: Listener): () => void {
    this.listeners.add(listener);
    listener(this.snapshot());
    return () => this.listeners.delete(listener);
  }

  snapshot(): AttachmentBatchSnapshot {
    const selected = this.items.filter((item) => item.state === 'selected');
    return {
      items: this.items.map(publicItem),
      issue: this.batchIssue ? { ...this.batchIssue, args: this.batchIssue.args ? { ...this.batchIssue.args } : undefined } : undefined,
      uploading: this.uploading,
      canUpload: !this.uploading && !this.active && selected.length > 0,
      canRestart: !this.uploading && Boolean(this.active) && this.items.some((item) => item.state === 'interrupted' && item.file),
    };
  }

  select(files: FileList | readonly File[]): AttachmentBatchSnapshot {
    if (this.uploading || this.active) throw new Error('attachment_batch_locked');
    this.items = [];
    this.batchIssue = undefined;
    const selected = Array.from(files);
    if (selected.length > this.limits.maxFiles) {
      this.batchIssue = { code: 'attachment_batch_limit', args: { selected: selected.length, max: this.limits.maxFiles } };
    }
    let batchBytes = 0;
    for (let order = 0; order < Math.min(selected.length, this.limits.maxFiles); order += 1) {
      const file = selected[order];
      const mediaType = normalizedMime(file.type);
      let issue: AttachmentIssue | undefined;
      if (!validFilename(file.name)) issue = { code: 'attachment_invalid_name' };
      else if (!mediaType || !this.allowedMimes.has(mediaType)) issue = { code: 'attachment_unknown_mime', args: { mime: mediaType || 'unknown' } };
      else if (file.size === 0) issue = { code: 'attachment_file_empty' };
      else if (file.size > this.limits.maxFileBytes) issue = { code: 'attachment_file_too_large', args: { bytes: file.size, max: this.limits.maxFileBytes } };
      else if (batchBytes + file.size > this.limits.maxBatchBytes) issue = { code: 'attachment_batch_too_large', args: { max: this.limits.maxBatchBytes } };
      if (!issue) batchBytes += file.size;
      this.items.push({
        clientId: `${this.epoch}-${order}-${crypto.randomUUID()}`,
        name: file.name,
        mediaType,
        bytes: file.size,
        order,
        state: issue ? 'rejected' : 'selected',
        uploadedBytes: 0,
        progress: 0,
        issue,
        file: issue ? undefined : file,
      });
    }
    this.epoch += 1;
    this.notify();
    return this.snapshot();
  }

  remove(clientId: string): void {
    if (this.uploading || this.active) throw new Error('attachment_batch_locked');
    this.items = this.items.filter((item) => item.clientId !== clientId);
    this.items.forEach((item, index) => { item.order = index; });
    this.notify();
  }

  orderedAttachments(): AttachmentRef[] {
    return this.items
      .filter((item): item is InternalItem & { attachment: AttachmentRef } => item.state === 'ready' && Boolean(item.attachment))
      .sort((left, right) => left.order - right.order)
      .map((item) => ({ ...item.attachment }));
  }

  async upload(): Promise<AttachmentRef[]> {
    if (this.uploading || this.active) throw new Error('attachment_batch_locked');
    const uploadItems = this.items.filter((item) => item.state === 'selected' && item.file);
    if (!uploadItems.length) return [];
    return this.startUpload(uploadItems);
  }

  async restart(): Promise<AttachmentRef[]> {
    if (this.uploading || !this.active) throw new Error('attachment_batch_not_restartable');
    const uploadItems = this.items.filter((item) => item.state === 'interrupted' && item.file);
    if (!uploadItems.length) throw new Error('attachment_batch_not_restartable');
    const old = this.active;
    this.active = undefined;
    old.abort.abort();
    try {
      await this.callbacks.cancel({ target: this.target, upload_id: old.uploadId });
    } catch {
      this.markInterrupted(uploadItems, { code: 'attachment_cancel_failed' });
      throw { code: 'attachment_cancel_failed' } satisfies AttachmentIssue;
    }
    for (const item of uploadItems) {
      item.state = 'selected';
      item.uploadedBytes = 0;
      item.progress = 0;
      item.issue = undefined;
      item.digest = undefined;
    }
    return this.startUpload(uploadItems);
  }

  async cancel(): Promise<void> {
    const active = this.active;
    this.epoch += 1;
    this.active = undefined;
    this.uploading = false;
    this.items = [];
    this.batchIssue = undefined;
    active?.abort.abort();
    this.notify();
    if (!active) return;
    try {
      await this.callbacks.cancel({ target: this.target, upload_id: active.uploadId });
    } catch {
      throw { code: 'attachment_cancel_failed' } satisfies AttachmentIssue;
    }
  }

  clear(): void {
    if (this.uploading || this.active) throw new Error('attachment_batch_locked');
    this.items = [];
    this.batchIssue = undefined;
    this.notify();
  }

  private async startUpload(uploadItems: InternalItem[]): Promise<AttachmentRef[]> {
    const run = ++this.epoch;
    const abort = new AbortController();
    this.uploading = true;
    this.batchIssue = undefined;
    for (const item of uploadItems) {
      item.state = 'uploading';
      item.uploadedBytes = 0;
      item.progress = 0;
      item.issue = undefined;
    }
    this.notify();

    let began = false;
    try {
      const begin = await this.callbacks.begin({
        target: this.target,
        files: uploadItems.map((item) => ({ name: item.name, media_type: item.mediaType, bytes: item.bytes })),
      }, abort.signal);
      if (run !== this.epoch) return [];
      const expiresAt = Date.parse(begin.expires_at);
      if (!begin.upload_id || !Number.isFinite(expiresAt) || expiresAt <= Date.now()) {
        const issue: AttachmentIssue = Number.isFinite(expiresAt) && expiresAt <= Date.now()
          ? { code: 'attachment_upload_expired' }
          : { code: 'attachment_invalid_response' };
        this.markInterrupted(uploadItems, issue);
        throw issue;
      }
      if (!validLimit(begin.chunk_bytes) || begin.chunk_bytes > this.limits.maxChunkBytes
        || uploadItems.length > begin.limits.max_files
        || uploadItems.some((item) => item.bytes > begin.limits.max_file_bytes)
        || uploadItems.reduce((sum, item) => sum + item.bytes, 0) > begin.limits.max_batch_bytes) {
        this.active = { uploadId: begin.upload_id, expiresAt, abort };
        this.markInterrupted(uploadItems, { code: 'attachment_invalid_response' });
        throw { code: 'attachment_invalid_response' } satisfies AttachmentIssue;
      }
      began = true;
      this.active = { uploadId: begin.upload_id, expiresAt, abort };

      for (let fileIndex = 0; fileIndex < uploadItems.length; fileIndex += 1) {
        const item = uploadItems[fileIndex];
        const file = item.file!;
        const wholeDigest = new IncrementalSha256();
        let sequence = 0;
        let offset = 0;
        while (offset < file.size) {
          if (Date.now() >= expiresAt) throw { code: 'attachment_upload_expired' } satisfies AttachmentIssue;
          const bytes = new Uint8Array(await file.slice(offset, offset + begin.chunk_bytes).arrayBuffer());
          if (run !== this.epoch) return [];
          wholeDigest.update(bytes);
          const chunkSha256 = hex(new Uint8Array(await crypto.subtle.digest('SHA-256', bytes)));
          const response = await this.callbacks.chunk({
            target: this.target,
            upload_id: begin.upload_id,
            file_index: fileIndex,
            sequence,
            data: bytes,
            sha256: chunkSha256,
          }, abort.signal);
          const expectedBytes = offset + bytes.byteLength;
          if (response.file_index !== fileIndex || response.next_sequence !== sequence + 1 || response.received_bytes !== expectedBytes) {
            throw { code: 'attachment_upload_state_unknown' } satisfies AttachmentIssue;
          }
          offset = expectedBytes;
          sequence += 1;
          item.uploadedBytes = offset;
          item.progress = file.size === 0 ? 1 : offset / file.size;
          this.notify();
        }
        item.digest = wholeDigest.digestHex();
      }

      if (Date.now() >= expiresAt) throw { code: 'attachment_upload_expired' } satisfies AttachmentIssue;
      const finished = await this.callbacks.finish({
        target: this.target,
        upload_id: begin.upload_id,
        files: uploadItems.map((item, fileIndex) => ({ file_index: fileIndex, sha256: item.digest! })),
      }, abort.signal);
      if (run !== this.epoch) return [];
      if (finished.attachments.length !== uploadItems.length) throw { code: 'attachment_invalid_response' } satisfies AttachmentIssue;
      for (let index = 0; index < uploadItems.length; index += 1) {
        const item = uploadItems[index];
        const attachment = finished.attachments[index];
        if (!attachment.ref || attachment.name !== item.name || normalizedMime(attachment.media_type) !== item.mediaType
          || attachment.bytes !== item.bytes || attachment.sha256 !== item.digest) {
          throw { code: 'attachment_invalid_response' } satisfies AttachmentIssue;
        }
        item.state = 'ready';
        item.progress = 1;
        item.uploadedBytes = item.bytes;
        item.attachment = { ...attachment };
        item.issue = undefined;
        item.file = undefined;
        item.digest = undefined;
      }
      this.active = undefined;
      this.uploading = false;
      this.notify();
      return this.orderedAttachments();
    } catch (error) {
      if (run !== this.epoch) return [];
      this.uploading = false;
      const issue = issueFrom(error, began ? 'attachment_upload_state_unknown' : 'attachment_upload_failed');
      this.markInterrupted(uploadItems, issue);
      throw issue;
    }
  }

  private markInterrupted(items: InternalItem[], issue: AttachmentIssue): void {
    this.uploading = false;
    for (const item of items) {
      if (item.state === 'ready') continue;
      item.state = 'interrupted';
      item.issue = issue;
    }
    this.notify();
  }

  private notify(): void {
    const snapshot = this.snapshot();
    for (const listener of this.listeners) listener(snapshot);
  }
}

export function attachmentIssueText(issue: AttachmentIssue): string {
  switch (issue.code) {
    case 'attachment_batch_limit': return `Select at most ${issue.args?.max ?? 'the allowed number of'} files.`;
    case 'attachment_batch_too_large': return 'These files exceed the attachment batch limit.';
    case 'attachment_file_too_large': return 'This file exceeds the attachment size limit.';
    case 'attachment_file_empty': return 'Empty files cannot be attached.';
    case 'attachment_unknown_mime': return 'This file type is not supported.';
    case 'attachment_invalid_name': return 'This file name is not supported.';
    case 'attachment_upload_expired': return 'The upload expired. Restart it to upload from the beginning.';
    case 'attachment_upload_state_unknown': return 'Upload progress is uncertain. Restart it from the beginning; it will not resume automatically.';
    case 'attachment_cancel_failed': return 'The local upload was cleared, but the relay could not confirm cancellation.';
    case 'attachment_invalid_response': return 'The relay returned an invalid upload response.';
    default: return 'The attachment could not be uploaded.';
  }
}
