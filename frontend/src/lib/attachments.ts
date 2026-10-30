import { IncrementalSha256 } from './incremental-sha256';
import type { ApiError, TargetRef } from './types';

export { IncrementalSha256 } from './incremental-sha256';

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
  'image/heic',
  'image/heif',
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
    | 'attachment_upload_busy'
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
interface HashWorkerResponse {
  id: string;
  digest?: string;
  error?: string;
}

class BackgroundFileHasher {
  private worker?: Worker;
  private readonly pending = new Map<string, (digest?: string) => void>();

  constructor() {
    if (typeof Worker === 'undefined') return;
    try {
      this.worker = new Worker(new URL('./attachment-hash.worker.ts', import.meta.url), { type: 'module' });
    } catch {
      return;
    }
    this.worker.addEventListener('message', (event: MessageEvent<HashWorkerResponse>) => {
      const settle = this.pending.get(event.data.id);
      if (!settle) return;
      this.pending.delete(event.data.id);
      settle(event.data.digest);
    });
    this.worker.addEventListener('error', () => this.cancel());
  }

  digest(file: File): Promise<string | undefined> | undefined {
    if (!this.worker) return undefined;
    const id = crypto.randomUUID();
    return new Promise((resolve) => {
      this.pending.set(id, resolve);
      try {
        this.worker!.postMessage({ id, file });
      } catch {
        this.pending.delete(id);
        resolve(undefined);
      }
    });
  }

  cancel(): void {
    this.worker?.terminate();
    this.worker = undefined;
    for (const settle of this.pending.values()) settle(undefined);
    this.pending.clear();
  }
}

async function incrementalFileDigest(
  file: File,
  chunkBytes: number,
  cancelled: () => boolean,
  signal: AbortSignal,
): Promise<string | undefined> {
  if (cancelled()) return undefined;
  const digest = new IncrementalSha256();
  for (let offset = 0; offset < file.size; offset += chunkBytes) {
    if (cancelled()) return undefined;
    let cancelRead!: () => void;
    const cancelledRead = new Promise<undefined>((resolve) => { cancelRead = () => resolve(undefined); });
    signal.addEventListener('abort', cancelRead, { once: true });
    let bytes: ArrayBuffer | undefined;
    try {
      // The native read may finish later; cancellation must not keep the composer locked.
      bytes = await Promise.race([file.slice(offset, offset + chunkBytes).arrayBuffer(), cancelledRead]);
    } finally {
      signal.removeEventListener('abort', cancelRead);
    }
    if (!bytes || cancelled()) return undefined;
    digest.update(new Uint8Array(bytes));
  }
  return cancelled() ? undefined : digest.digestHex();
}


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

export class AttachmentBatchController {
  private items: InternalItem[] = [];
  private listeners = new Set<Listener>();
  private batchIssue?: AttachmentIssue;
  private active?: { uploadId: string; expiresAt: number; abort: AbortController; cancelHashing?: () => void };
  private pendingBegin?: Promise<UploadBeginResult>;
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
      canRestart: !this.uploading && this.items.some((item) => item.state === 'interrupted' && item.file),
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
    for (let order = 0; order < selected.length; order += 1) {
      const file = selected[order];
      const mediaType = normalizedMime(file.type);
      let issue: AttachmentIssue | undefined;
      if (order >= this.limits.maxFiles) issue = { code: 'attachment_batch_limit', args: { max: this.limits.maxFiles } };
      else if (!validFilename(file.name)) issue = { code: 'attachment_invalid_name' };
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
    if (this.uploading) throw new Error('attachment_batch_not_restartable');
    const uploadItems = this.items.filter((item) => item.state === 'interrupted' && item.file);
    if (!uploadItems.length) throw new Error('attachment_batch_not_restartable');
    const old = this.active;
    this.active = undefined;
    if (old) {
      old.abort.abort();
      old.cancelHashing?.();
      try {
        await this.callbacks.cancel({ target: this.target, upload_id: old.uploadId });
      } catch {
        this.markInterrupted(uploadItems, { code: 'attachment_cancel_failed' });
        throw { code: 'attachment_cancel_failed' } satisfies AttachmentIssue;
      }
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
    const pendingBegin = this.pendingBegin;
    this.epoch += 1;
    this.active = undefined;
    this.uploading = false;
    this.items = [];
    this.batchIssue = undefined;
    active?.abort.abort();
    active?.cancelHashing?.();
    this.notify();
    let uploadId = active?.uploadId;
    if (!uploadId && pendingBegin) {
      try {
        const result = await pendingBegin;
        if (this.pendingBegin !== pendingBegin) return;
        this.pendingBegin = undefined;
        uploadId = result.upload_id;
      } catch {
        return;
      }
    }
    if (!uploadId) return;
    try {
      await this.callbacks.cancel({ target: this.target, upload_id: uploadId });
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
    let beginPromise: Promise<UploadBeginResult> | undefined;
    let hashing: BackgroundFileHasher | undefined;
    try {
      beginPromise = this.callbacks.begin({
        target: this.target,
        files: uploadItems.map((item) => ({ name: item.name, media_type: item.mediaType, bytes: item.bytes })),
      }, abort.signal);
      this.pendingBegin = beginPromise;
      const begin = await beginPromise;
      const expiresAt = Date.parse(begin.expires_at);
      if (run !== this.epoch) return [];
      if (this.pendingBegin === beginPromise) this.pendingBegin = undefined;
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
      hashing = new BackgroundFileHasher();
      this.active = {
        uploadId: begin.upload_id, expiresAt, abort, cancelHashing: () => hashing?.cancel(),
      };
      const backgroundDigests = uploadItems.map((item) => hashing!.digest(item.file!));
      let sequence = 0;

      for (let fileIndex = 0; fileIndex < uploadItems.length; fileIndex += 1) {
        const item = uploadItems[fileIndex];
        const file = item.file!;
        const backgroundDigest = backgroundDigests[fileIndex];
        let offset = 0;
        while (offset < file.size) {
          if (Date.now() >= expiresAt) throw { code: 'attachment_upload_expired' } satisfies AttachmentIssue;
          const bytes = new Uint8Array(await file.slice(offset, offset + begin.chunk_bytes).arrayBuffer());
          if (run !== this.epoch) return [];
          // The worker owns the whole-file hash; chunks stay on the UI thread only for transport.
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
        const digest = backgroundDigest ? await backgroundDigest : undefined;
        if (run !== this.epoch) return [];
        item.digest = digest ?? await incrementalFileDigest(file, begin.chunk_bytes, () => run !== this.epoch, abort.signal);
        if (!item.digest) return [];
      }
      hashing.cancel();
      hashing = undefined;
      if (this.active) this.active.cancelHashing = undefined;

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
      hashing?.cancel();
      if (this.pendingBegin === beginPromise) this.pendingBegin = undefined;
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
    case 'attachment_upload_busy': return 'This device already has the maximum number of active uploads. Finish or cancel one, then retry.';
    case 'attachment_upload_state_unknown': return 'Upload progress is uncertain. Restart it from the beginning; it will not resume automatically.';
    case 'attachment_cancel_failed': return 'The local upload was cleared, but the relay could not confirm cancellation.';
    case 'attachment_invalid_response': return 'The relay returned an invalid upload response.';
    default: return 'The attachment could not be uploaded.';
  }
}
