import { createHash } from 'node:crypto';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AttachmentBatchController } from '$lib/attachments';
import { IncrementalSha256 } from '$lib/incremental-sha256';
import type { AttachmentUploadCallbacks, UploadChunkRequest, UploadFinishRequest } from '$lib/attachments';

const target = {
  server_session_id: 'server', pane_id: 'pane', terminal_id: 'terminal', agent_session_id: 'session', generation: 7,
};
const wholeDigest = 'b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9';
const expiresAt = '2099-01-01T00:00:00.000Z';

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

function fixture() {
  const chunks: UploadChunkRequest[] = [];
  const finish = vi.fn(async (request: UploadFinishRequest) => ({
    attachments: [{
      ref: 'attachment:hello', name: 'hello.txt', media_type: 'text/plain', bytes: 11,
      sha256: request.files[0].sha256, expires_at: expiresAt,
    }],
  }));
  const cancel = vi.fn(async () => undefined);
  const callbacks: AttachmentUploadCallbacks = {
    begin: vi.fn(async () => ({
      upload_id: 'upload-1', chunk_bytes: 4, expires_at: expiresAt,
      limits: { max_files: 1, max_file_bytes: 100, max_batch_bytes: 100 },
    })),
    chunk: vi.fn(async (request) => {
      chunks.push(request);
      return { file_index: request.file_index, next_sequence: request.sequence + 1, received_bytes: Math.min((request.sequence + 1) * 4, 11) };
    }),
    finish,
    cancel,
  };
  const controller = new AttachmentBatchController(target, callbacks, {
    maxFiles: 1, maxFileBytes: 100, maxBatchBytes: 100, maxChunkBytes: 4,
  });
  const file = new File(['hello world'], 'hello.txt', { type: 'text/plain' });
  const fullRead = vi.fn(() => { throw new Error('whole-file read on main thread'); });
  Object.defineProperty(file, 'arrayBuffer', { configurable: true, value: fullRead });
  return { callbacks, chunks, controller, file, finish, cancel, fullRead };
}

function assertUploaded(f: ReturnType<typeof fixture>) {
  expect(f.callbacks.begin).toHaveBeenCalledWith({
    target, files: [{ name: 'hello.txt', media_type: 'text/plain', bytes: 11 }],
  }, expect.any(AbortSignal));
  expect(f.chunks.map(({ sequence, file_index, upload_id, target: chunkTarget, data, sha256 }) => ({
    sequence, file_index, upload_id, target: chunkTarget, text: new TextDecoder().decode(data), sha256,
  }))).toEqual(['hell', 'o wo', 'rld'].map((text, sequence) => ({
    sequence, file_index: 0, upload_id: 'upload-1', target, text,
    sha256: createHash('sha256').update(text).digest('hex'),
  })));
  expect(f.finish).toHaveBeenCalledOnce();
  expect(f.finish).toHaveBeenCalledWith({
    target, upload_id: 'upload-1', files: [{ file_index: 0, sha256: wholeDigest }],
  }, expect.any(AbortSignal));
  expect(f.controller.orderedAttachments()).toMatchObject([{ ref: 'attachment:hello', sha256: wholeDigest }]);
  expect(f.fullRead).not.toHaveBeenCalled();
}

afterEach(() => vi.unstubAllGlobals());

describe('IncrementalSha256 facade', () => {
  it.each([
    ['empty', '', 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'],
    ['abc', 'abc', 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad'],
    ['million-a', 'a'.repeat(1_000_000), 'cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0'],
  ])('matches fixed %s vector', (_name, input, expected) => {
    const hash = new IncrementalSha256();
    hash.update(new TextEncoder().encode(input));
    expect(hash.digestHex()).toBe(expected);
  });

  it('matches node:crypto around block and padding boundaries for varied partitions and offset views', () => {
    for (const size of [0, 1, 55, 56, 63, 64, 65, 127, 128, 129, 255]) {
      const storage = Uint8Array.from({ length: size + 13 }, (_, i) => (i * 73 + 19) & 255);
      const input = storage.subarray(5, 5 + size);
      const expected = createHash('sha256').update(input).digest('hex');
      for (const partition of [1, 7, 64, 97, 1024]) {
        const hash = new IncrementalSha256();
        hash.update(input.subarray(0, 0));
        for (let offset = 0; offset < input.length; offset += partition) {
          hash.update(input.subarray(offset, offset + partition));
          hash.update(input.subarray(offset, offset));
        }
        const digest = hash.digestHex();
        expect(digest, `size=${size} partition=${partition}`).toBe(expected);
        expect(digest).toMatch(/^[0-9a-f]{64}$/);
      }
    }
  });

  it('rejects both updates and another digest after finalization', () => {
    const hash = new IncrementalSha256();
    hash.update(new TextEncoder().encode('abc'));
    expect(hash.digestHex()).toBe('ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
    expect(() => hash.update(new Uint8Array())).toThrow('sha256_already_finished');
    expect(() => hash.digestHex()).toThrow('sha256_already_finished');
  });
});

describe('AttachmentBatchController whole-file hashing', () => {
  it('hashes the file from the chunks it uploads, reading each chunk once', async () => {
    const worker = vi.fn();
    vi.stubGlobal('Worker', worker);
    const f = fixture();
    const slices = vi.spyOn(f.file, 'slice');
    f.controller.select([f.file]);
    await expect(f.controller.upload()).resolves.toHaveLength(1);
    assertUploaded(f);
    expect(slices.mock.calls.map(([start, end]) => [start, end])).toEqual([[0, 4], [4, 8], [8, 12]]);
    expect(worker).not.toHaveBeenCalled();
  });

  it('hashes many chunks into one whole-file digest', async () => {
    const input = 'a'.repeat(160);
    const expected = 'bf18b43b61652b5d73f41ebf3d72e5e43aebf5076f497dde31ea3de9de4998ef';
    const chunks: Uint8Array[] = [];
    const finish = vi.fn(async (request: UploadFinishRequest) => ({ attachments: [{
      ref: 'attachment:many', name: 'many.txt', media_type: 'text/plain', bytes: input.length,
      sha256: request.files[0].sha256, expires_at: expiresAt,
    }] }));
    const controller = new AttachmentBatchController(target, {
      begin: async () => ({ upload_id: 'many', chunk_bytes: 4, expires_at: expiresAt,
        limits: { max_files: 1, max_file_bytes: 200, max_batch_bytes: 200 } }),
      chunk: async (request) => {
        chunks.push(request.data);
        return { file_index: request.file_index, next_sequence: request.sequence + 1, received_bytes: chunks.length * 4 };
      },
      finish,
      cancel: async () => undefined,
    }, { maxFiles: 1, maxFileBytes: 200, maxBatchBytes: 200, maxChunkBytes: 4 });
    controller.select([new File([input], 'many.txt', { type: 'text/plain' })]);
    await expect(controller.upload()).resolves.toHaveLength(1);
    expect(chunks).toHaveLength(40);
    expect(Buffer.concat(chunks.map(chunk => Buffer.from(chunk)))).toEqual(Buffer.from(input));
    expect(createHash('sha256').update(input).digest('hex')).toBe(expected);
    expect(finish.mock.calls[0][0]).toEqual({ target, upload_id: 'many', files: [{ file_index: 0, sha256: expected }] });
  });

  it('rejects a finish response that does not match the computed digest', async () => {
    const f = fixture();
    f.finish.mockImplementationOnce(async () => ({ attachments: [{
      ref: 'attachment:wrong', name: 'hello.txt', media_type: 'text/plain', bytes: 11,
      sha256: '0'.repeat(64), expires_at: expiresAt,
    }] }));
    f.controller.select([f.file]);
    await expect(f.controller.upload()).rejects.toMatchObject({ code: 'attachment_invalid_response' });
    expect(f.finish.mock.calls[0][0].files).toEqual([{ file_index: 0, sha256: wholeDigest }]);
    expect(f.controller.snapshot().items[0]).toMatchObject({ state: 'interrupted', issue: { code: 'attachment_invalid_response' } });
  });

  it('cancels while a chunk is being read and never finishes late', async () => {
    const f = fixture();
    const reading = deferred<void>();
    const release = deferred<ArrayBuffer>();
    const originalSlice = f.file.slice.bind(f.file);
    const slices = vi.spyOn(f.file, 'slice').mockImplementation((start, end, contentType) => {
      const blob = originalSlice(start, end, contentType);
      if (slices.mock.calls.length === 2) {
        Object.defineProperty(blob, 'arrayBuffer', { value: () => { reading.resolve(); return release.promise; } });
      }
      return blob;
    });
    f.controller.select([f.file]);
    const upload = f.controller.upload();
    await reading.promise;
    expect(f.chunks).toHaveLength(1);
    await expect(f.controller.cancel()).resolves.toBeUndefined();
    expect(f.cancel).toHaveBeenCalledWith({ target, upload_id: 'upload-1' });
    release.resolve(new TextEncoder().encode('o wo').buffer);
    await expect(upload).resolves.toEqual([]);
    expect(f.chunks).toHaveLength(1);
    expect(f.finish).not.toHaveBeenCalled();
    expect(f.fullRead).not.toHaveBeenCalled();
    expect(f.controller.snapshot()).toMatchObject({ items: [], uploading: false });
  });

  it('ignores a chunk read that fails after cancellation', async () => {
    const f = fixture();
    const reading = deferred<void>();
    let rejectRead!: (error: Error) => void;
    const held = new Promise<ArrayBuffer>((_resolve, reject) => { rejectRead = reject; });
    const originalSlice = f.file.slice.bind(f.file);
    const slices = vi.spyOn(f.file, 'slice').mockImplementation((start, end, type) => {
      const part = originalSlice(start, end, type);
      if (slices.mock.calls.length === 2) Object.defineProperty(part, 'arrayBuffer', {
        value: () => { reading.resolve(); return held; },
      });
      return part;
    });
    f.controller.select([f.file]);
    const oldUpload = f.controller.upload();
    await reading.promise;
    await f.controller.cancel();
    f.controller.select([new File(['hello world'], 'hello.txt', { type: 'text/plain' })]);
    slices.mockRestore();
    const current = f.controller.snapshot();
    rejectRead(new Error('late native read failure'));
    await expect(oldUpload).resolves.toEqual([]);
    expect(f.finish).not.toHaveBeenCalled();
    expect(f.controller.snapshot()).toEqual(current);
  });
});
