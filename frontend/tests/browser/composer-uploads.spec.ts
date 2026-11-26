import { expect, test, type Locator, type Page } from '@playwright/test';
import { resolve } from 'node:path';
import { commands, composerTarget, openSharedComposer, server } from '../support/journey-relay';

for (const mode of ['Conversation', 'Full history']) {
  test(`shared composer ${mode}: chunked upload hashes uploads for the exact target`, async ({ page }) => {
    await page.addInitScript(() => {
      const slices: Array<{ name: string; start: number | undefined; end: number | undefined }> = [];
      Object.defineProperty(window, '__uploadSlices', { value: slices });
      const original = Blob.prototype.slice;
      Blob.prototype.slice = function (start, end, type) {
        if (this instanceof File) slices.push({ name: this.name, start, end });
        return original.call(this, start, end, type);
      };
    });
    const prompt = await openSharedComposer(page, mode);
    expect(await page.evaluate(() => typeof crypto.subtle.digest)).toBe('function');
    await prompt.fill('Review upload');
    const attach = page.getByRole('button', { name: 'Attach', exact: true });
    const picker = page.getByRole('dialog', { name: 'Attach', exact: true });
    await expect(picker).toHaveCount(0);
    await attach.press('Enter');
    await expect(picker.getByRole('button', { name: 'Attach photos' })).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(picker).toHaveCount(0);
    await expect(attach).toBeFocused();

    await attach.click();
    await expect(picker.getByRole('button', { name: 'Attach files' })).toBeEnabled();
    await server(page, 0, {
      type: 'agent_update', ...composerTarget, status: 'working', project: 'Shared composer', agent: 'codex', conversation_history_available: true,
    });
    await expect(page.getByRole('banner').getByRole('img', { name: 'Working' })).toBeVisible();
    await expect(picker).toBeVisible();
    const choice = page.waitForEvent('filechooser');
    await picker.getByRole('button', { name: 'Attach photos' }).click();
    const chooser = await choice;
    expect(await chooser.element().getAttribute('accept')).toBe('image/*');
    await expect(attach).toBeFocused();
    await chooser.setFiles([
      { name: 'upload-one.png', mimeType: 'image/png', buffer: Buffer.from('png') },
      { name: 'upload-two.png', mimeType: 'image/png', buffer: Buffer.from('abc') },
    ]);
    await expect(prompt).toHaveValue('Review upload\nAttachment: attachment:test-1\nAttachment: attachment:test-2\n');
    const upload = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
    expect(upload.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_chunk', 'upload_finish']);
    for (const command of upload) expect(command.target).toEqual(composerTarget);
    expect(upload[0]).toMatchObject({ files: [
      { name: 'upload-one.png', media_type: 'image/png', bytes: 3 },
      { name: 'upload-two.png', media_type: 'image/png', bytes: 3 },
    ] });
    const uploadId = `upload-${String(upload[0].request_id)}`;
    const png = '8f8cbb7dcf46e0bc7d53265749a6c17d116093a6ba95e442764060c76fd4a86c';
    const abc = 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad';
    expect(upload[1]).toMatchObject({ upload_id: uploadId, file_index: 0, sequence: 0, data: 'cG5n', sha256: png });
    expect(upload[2]).toMatchObject({ upload_id: uploadId, file_index: 1, sequence: 1, data: 'YWJj', sha256: abc });
    expect(upload[3]).toMatchObject({ upload_id: uploadId, files: [
      { file_index: 0, sha256: png }, { file_index: 1, sha256: abc },
    ] });
    expect(await page.evaluate(() => (window as unknown as { __uploadSlices: unknown[] }).__uploadSlices)).toEqual([
      { name: 'upload-one.png', start: 0, end: 262144 },
      { name: 'upload-two.png', start: 0, end: 262144 },
    ]);
    await prompt.press('Meta+Enter');
    await expect.poll(async () => (await commands(page)).find(command => command.type === 'submit_prompt')).toMatchObject({
      target: composerTarget, text: 'Review upload\nAttachment: attachment:test-1\nAttachment: attachment:test-2',
    });
    await expect(prompt).toHaveValue('');
  });
}

type UploadReadProbe = {
  slices: Array<{ start: number | undefined; end: number | undefined }>;
  reads: number;
  waiting: boolean;
  nativeFinished: boolean;
  settled: boolean;
  settlement?: Promise<void>;
  release(): void;
  restore(): void;
};

async function instrumentUploadRead(page: Page, behavior: 'fail' | 'hold') {
  await page.addInitScript((behavior) => {
    const slice = Blob.prototype.slice;
    const read = Blob.prototype.arrayBuffer;
    let armed = true;
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    const probe: UploadReadProbe = {
      slices: [], reads: 0, waiting: false, nativeFinished: false, settled: false,
      release: () => release(),
      restore: () => { armed = false; Blob.prototype.slice = slice; },
    };
    Object.defineProperty(window, '__uploadReadProbe', { value: probe });
    Blob.prototype.slice = function (start, end, type) {
      const part = slice.call(this, start, end, type);
      if (!(this instanceof File) || this.name !== 'upload-old.png') return part;
      probe.slices.push({ start, end });
      if (probe.slices.length === 2 && armed) {
        Object.defineProperty(part, 'arrayBuffer', { value: () => {
          probe.reads += 1;
          if (behavior === 'fail') {
            armed = false;
            return Promise.reject(new Error('injected upload read failure'));
          }
          const nativeRead = read.call(part);
          probe.waiting = true;
          const result = (async () => {
            const bytes = await nativeRead;
            probe.nativeFinished = true;
            await gate;
            return bytes;
          })();
          probe.settlement = result.then(() => { probe.settled = true; });
          return result;
        } });
      }
      return part;
    };
  }, behavior);
}

const uploadOldBytes = Buffer.concat([Buffer.alloc(262144, 97), Buffer.from('bc')]);

const uploadOldDigest = 'cc485ca6b1916f3751159b548835893e6b1651a4008ded0ed73e0d13513a5a20';

const uploadFirstDigest = 'dd3dde87623d9a6b354c68c943d189c89c63652d945e7bbdf0986cae91a49521';

const uploadLastDigest = '1e0bbd6c686ba050b8eb03ffeedc64fdc9d80947fce821abbe5d6dc8d252c5ac';

const uploadNewDigest = 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad';

function assertOldFirstChunk(chunk: Record<string, unknown>, uploadId: string) {
  expect(chunk).toMatchObject({ type: 'upload_chunk', target: composerTarget, upload_id: uploadId, file_index: 0, sequence: 0, sha256: uploadFirstDigest });
  expect(Buffer.from(String(chunk.data), 'base64')).toEqual(uploadOldBytes.subarray(0, 262144));
}

function assertOldUploadChunks(chunks: Record<string, unknown>[], uploadId: string) {
  expect(chunks).toHaveLength(2);
  expect(chunks[0]).toMatchObject({ type: 'upload_chunk', target: composerTarget, upload_id: uploadId, file_index: 0, sequence: 0, sha256: uploadFirstDigest });
  expect(chunks[1]).toMatchObject({ type: 'upload_chunk', target: composerTarget, upload_id: uploadId, file_index: 0, sequence: 1, sha256: uploadLastDigest });
  expect(Buffer.from(String(chunks[0].data), 'base64')).toEqual(uploadOldBytes.subarray(0, 262144));
  expect(Buffer.from(String(chunks[1].data), 'base64')).toEqual(uploadOldBytes.subarray(262144));
}

for (const mode of ['Conversation', 'Full history']) {
  test(`shared composer ${mode}: chunked upload restarts the retained file after a slice read failure`, async ({ page }) => {
    await instrumentUploadRead(page, 'fail');
    const prompt = await openSharedComposer(page, mode);
    const laterScripts: string[] = [];
    page.on('request', request => { if (request.resourceType() === 'script') laterScripts.push(request.url()); });
    expect(await page.evaluate(() => typeof crypto.subtle.digest)).toBe('function');
    await prompt.fill('Keep recovery draft');
    await page.locator('input[type=file][accept="image/*"]').setInputFiles({ name: 'upload-old.png', mimeType: 'image/png', buffer: uploadOldBytes });
    const restart = page.getByRole('button', { name: 'Restart interrupted files' });
    await expect(restart).toBeEnabled();
    await expect(page.getByRole('alert').filter({ hasText: 'Upload progress is uncertain.' })).toBeVisible();
    await expect(prompt).toHaveValue('Keep recovery draft');
    const before = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
    expect(before.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk']);
    expect(before[0]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-old.png', media_type: 'image/png', bytes: uploadOldBytes.length }] });
    const oldId = `upload-${String(before[0].request_id)}`;
    assertOldFirstChunk(before[1], oldId);
    expect(await page.evaluate(() => {
      const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
      return { reads: probe.reads, slices: probe.slices };
    })).toEqual({ reads: 1, slices: [
      { start: 0, end: 262144 }, { start: 262144, end: 524288 },
    ] });

    // Restore the injected reader, then use Restart without selecting or reloading the file.
    await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.restore());
    await restart.click();
    await expect(prompt).toHaveValue('Keep recovery draft\nAttachment: attachment:test-1\n');
    await expect(page.getByText('Attached upload-old.png')).toBeVisible();
    const after = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
    expect(after.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_cancel', 'upload_begin', 'upload_chunk', 'upload_chunk', 'upload_finish']);
    expect(after[2]).toMatchObject({ target: composerTarget, upload_id: oldId });
    expect(after[3]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-old.png', media_type: 'image/png', bytes: uploadOldBytes.length }] });
    const newId = `upload-${String(after[3].request_id)}`;
    expect(newId).not.toBe(oldId);
    assertOldUploadChunks(after.slice(4, 6), newId);
    expect(after[6]).toMatchObject({ target: composerTarget, upload_id: newId, files: [{ file_index: 0, sha256: uploadOldDigest }] });
    expect(after.filter(command => command.type === 'upload_finish' && command.upload_id === oldId)).toHaveLength(0);
    expect((await prompt.inputValue()).match(/Attachment: attachment:test-1/g)).toHaveLength(1);
    expect(laterScripts).toEqual([]);
  });

  test(`shared composer ${mode}: chunked upload cancellation fences a held read while a new upload succeeds`, async ({ page }) => {
    await instrumentUploadRead(page, 'hold');
    const prompt = await openSharedComposer(page, mode);
    const laterScripts: string[] = [];
    page.on('request', request => { if (request.resourceType() === 'script') laterScripts.push(request.url()); });
    await prompt.fill('Keep canceled draft');
    try {
      await page.locator('input[type=file][accept="image/*"]').setInputFiles({ name: 'upload-old.png', mimeType: 'image/png', buffer: uploadOldBytes });
      await expect.poll(() => page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.waiting)).toBe(true);
      const before = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
      expect(before.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk']);
      expect(before[0]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-old.png', media_type: 'image/png', bytes: uploadOldBytes.length }] });
      const oldId = `upload-${String(before[0].request_id)}`;
      assertOldFirstChunk(before[1], oldId);
      await page.getByRole('button', { name: 'Cancel upload' }).click();
      await expect.poll(async () => (await commands(page)).find(command => command.type === 'upload_cancel')).toMatchObject({ target: composerTarget, upload_id: oldId });
      expect(await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.settled)).toBe(false);
      await page.evaluate(async () => {
        const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
        probe.release();
        await probe.settlement;
        await Promise.resolve();
        await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
      });
      await expect(page.getByText('Attachment upload canceled.')).toBeVisible();
      const input = page.locator('input[type=file][accept="image/*"]');
      await expect(input).toBeEnabled();
      expect((await commands(page)).filter(command => String(command.type).startsWith('upload_')).map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_cancel']);

      await input.setInputFiles({ name: 'upload-new.png', mimeType: 'image/png', buffer: Buffer.from('abc') });
      await expect(prompt).toHaveValue('Keep canceled draft\nAttachment: attachment:test-1\n');
      await expect(page.getByText('Attached upload-new.png')).toBeVisible();
      const after = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
      expect(after.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_cancel', 'upload_begin', 'upload_chunk', 'upload_finish']);
      expect(after[3]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-new.png', media_type: 'image/png', bytes: 3 }] });
      const newId = `upload-${String(after[3].request_id)}`;
      expect(newId).not.toBe(oldId);
      expect(after[4]).toMatchObject({ target: composerTarget, upload_id: newId, file_index: 0, sequence: 0, data: 'YWJj', sha256: uploadNewDigest });
      expect(after[5]).toMatchObject({ target: composerTarget, upload_id: newId, files: [{ file_index: 0, sha256: uploadNewDigest }] });
      expect(await page.evaluate(() => {
        const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
        return { reads: probe.reads, nativeFinished: probe.nativeFinished, settled: probe.settled, slices: probe.slices };
      })).toEqual({ reads: 1, nativeFinished: true, settled: true, slices: [
        { start: 0, end: 262144 }, { start: 262144, end: 524288 },
      ] });
      expect((await commands(page)).filter(command => String(command.type).startsWith('upload_'))).toEqual(after);
      expect(after.filter(command => command.type === 'upload_finish' && command.upload_id === oldId)).toHaveLength(0);
      await expect(prompt).toHaveValue('Keep canceled draft\nAttachment: attachment:test-1\n');
      await expect(page.getByText('Attached upload-new.png')).toBeVisible();
      expect((await prompt.inputValue()).match(/Attachment: attachment:test-1/g)).toHaveLength(1);
      expect(laterScripts).toEqual([]);
    } finally {
      await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.release()).catch(() => {});
    }
  });
}

test('chunked upload distributes the complete pinned upstream license', async ({ page }) => {
  const { readFile } = await import('node:fs/promises');
  const upstream = await readFile(resolve('node_modules/@noble/hashes/LICENSE'));
  const metadata = JSON.parse(await readFile(resolve('node_modules/@noble/hashes/package.json'), 'utf8')) as { name: string; version: string };
  expect(metadata).toMatchObject({ name: '@noble/hashes', version: '2.4.0' });
  expect(upstream.length).toBeGreaterThan(1000);
  const notice = upstream.toString('utf8');
  expect(notice).toContain('Copyright (c) 2022 Paul Miller');
  expect(notice).toContain('Permission is hereby granted');
  expect(notice).toContain('WITHOUT WARRANTY OF ANY KIND');
  await page.goto('/');
  const result = await page.evaluate(async () => {
    const response = await fetch('/licenses/noble-hashes.txt');
    return { ok: response.ok, status: response.status, bytes: Array.from(new Uint8Array(await response.arrayBuffer())) };
  });
  expect(result.status).toBe(200);
  expect(result.ok).toBe(true);
  expect(Buffer.from(result.bytes)).toEqual(upstream);
});

// Hold selected actual mock-wire replies, without changing the shared mock or its defaults.
type OverlapReplyGate = {
  cancel: Record<string, unknown> | null;
  chunk: Record<string, unknown> | null;
  holdChunk: boolean;
  deliver(kind: 'cancel' | 'chunk', fail: boolean): void;
};

async function installOverlapReplyGate(page: Page) {
  await page.evaluate(() => {
    const socket = window.__relaySockets[0];
    const prototype = Object.getPrototypeOf(socket) as { server(message: Record<string, unknown>): void };
    const original = prototype.server;
    let heldCancel = false;
    const gate: OverlapReplyGate = {
      cancel: null, chunk: null, holdChunk: false,
      deliver(kind, fail) {
        const response = gate[kind];
        if (!response) throw new Error(`No held ${kind} response`);
        gate[kind] = null;
        original.call(socket, fail ? { ...response, error: { code: 'attachment_upload_state_unknown' } } : response);
      },
    };
    prototype.server = function (message) {
      if (message.type === 'upload_cancel_result' && !heldCancel) {
        heldCancel = true;
        gate.cancel = message;
        return;
      }
      if (message.type === 'upload_chunk_result' && gate.holdChunk) {
        gate.holdChunk = false;
        gate.chunk = message;
        return;
      }
      original.call(this, message);
    };
    Object.defineProperty(window, '__overlapReplyGate', { value: gate });
  });
}

async function deliverOverlapReply(page: Page, kind: 'cancel' | 'chunk', fail: boolean) {
  await page.evaluate(async ({ kind, fail }) => {
    (window as unknown as { __overlapReplyGate: OverlapReplyGate }).__overlapReplyGate.deliver(kind, fail);
    await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
  }, { kind, fail });
}

async function startHeldOldCancellation(page: Page, mode: string) {
  await instrumentUploadRead(page, 'hold');
  const prompt = await openSharedComposer(page, mode);
  await installOverlapReplyGate(page);
  await prompt.fill('Keep overlap draft');
  const input = page.locator('input[type=file][accept="image/*"]');
  await input.setInputFiles({ name: 'upload-old.png', mimeType: 'image/png', buffer: uploadOldBytes });
  await expect.poll(() => page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.waiting)).toBe(true);
  const before = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
  expect(before.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk']);
  expect(before[0]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-old.png', media_type: 'image/png', bytes: uploadOldBytes.length }] });
  const oldId = `upload-${String(before[0].request_id)}`;
  assertOldFirstChunk(before[1], oldId);
  await page.getByRole('button', { name: 'Cancel upload' }).click();
  await expect.poll(() => page.evaluate(() => (window as unknown as { __overlapReplyGate: OverlapReplyGate }).__overlapReplyGate.cancel !== null)).toBe(true);
  await expect.poll(async () => (await commands(page)).find(command => command.type === 'upload_cancel')).toMatchObject({ target: composerTarget, upload_id: oldId });
  expect(await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.settled)).toBe(false);
  return { prompt, input, oldId };
}

async function releaseCanceledUpload(page: Page, input: Locator) {
  await settleOverlapOldRead(page);
  await expect(input).toBeEnabled();
  await expect(page.locator('.upload-status')).toHaveText('Attachment upload canceled.');
}

async function settleOverlapOldRead(page: Page) {
  await page.evaluate(async () => {
    const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
    probe.release();
    await probe.settlement;
    await new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve())));
  });
  expect(await page.evaluate(() => {
    const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
    return { reads: probe.reads, nativeFinished: probe.nativeFinished, settled: probe.settled, slices: probe.slices };
  })).toEqual({ reads: 1, nativeFinished: true, settled: true, slices: [
    { start: 0, end: 262144 }, { start: 262144, end: 524288 },
  ] });
}

for (const mode of ['Conversation', 'Full history']) {
  for (const oldReply of ['success', 'error'] as const) {
    test(`shared composer ${mode}: chunked upload old cancel ${oldReply} preserves a pending new upload and restart`, async ({ page }) => {
      const { prompt, input, oldId } = await startHeldOldCancellation(page, mode);
      try {
        await releaseCanceledUpload(page, input);
        await page.evaluate(() => { (window as unknown as { __overlapReplyGate: OverlapReplyGate }).__overlapReplyGate.holdChunk = true; });
        await input.setInputFiles({ name: 'upload-new.png', mimeType: 'image/png', buffer: Buffer.from('abc') });
        await expect.poll(() => page.evaluate(() => (window as unknown as { __overlapReplyGate: OverlapReplyGate }).__overlapReplyGate.chunk !== null)).toBe(true);
        await expect(page.getByRole('progressbar', { name: 'upload-new.png upload progress' })).toBeVisible();
        await expect(page.getByRole('button', { name: 'Cancel upload' })).toBeEnabled();
        await expect(page.locator('.upload-status')).toHaveText('Uploading 1 attachment…');
        const pending = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
        expect(pending.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_cancel', 'upload_begin', 'upload_chunk']);
        expect(pending[3]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-new.png', media_type: 'image/png', bytes: 3 }] });
        const newId = `upload-${String(pending[3].request_id)}`;
        expect(newId).not.toBe(oldId);
        expect(pending[4]).toMatchObject({ target: composerTarget, upload_id: newId, file_index: 0, sequence: 0, data: 'YWJj', sha256: uploadNewDigest });

        await deliverOverlapReply(page, 'cancel', oldReply === 'error');
        await expect(page.getByRole('progressbar', { name: 'upload-new.png upload progress' })).toBeVisible();
        await expect(page.getByRole('button', { name: 'Cancel upload' })).toBeEnabled();
        await expect(page.locator('.upload-status')).toHaveText('Uploading 1 attachment…');
        await deliverOverlapReply(page, 'chunk', true);
        await expect(page.getByRole('alert').filter({ hasText: 'Upload progress is uncertain.' })).toBeVisible();
        const restart = page.getByRole('button', { name: 'Restart interrupted files' });
        await expect(restart).toBeEnabled();
        await expect(prompt).toHaveValue('Keep overlap draft');
        await restart.click();
        await expect(prompt).toHaveValue('Keep overlap draft\nAttachment: attachment:test-1\n');
        await expect(page.locator('.upload-status')).toHaveText('Attached upload-new.png');
        const after = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
        expect(after.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_cancel', 'upload_begin', 'upload_chunk', 'upload_cancel', 'upload_begin', 'upload_chunk', 'upload_finish']);
        expect(after[5]).toMatchObject({ target: composerTarget, upload_id: newId });
        expect(after[6]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-new.png', media_type: 'image/png', bytes: 3 }] });
        const restartId = `upload-${String(after[6].request_id)}`;
        expect(new Set([oldId, newId, restartId]).size).toBe(3);
        expect(after[7]).toMatchObject({ target: composerTarget, upload_id: restartId, file_index: 0, sequence: 0, data: 'YWJj', sha256: uploadNewDigest });
        expect(after[8]).toMatchObject({ target: composerTarget, upload_id: restartId, files: [{ file_index: 0, sha256: uploadNewDigest }] });
        expect(after.filter(command => command.type === 'upload_finish')).toHaveLength(1);
        expect((await commands(page)).filter(command => String(command.type).startsWith('upload_'))).toEqual(after);
        await expect(prompt).toHaveValue('Keep overlap draft\nAttachment: attachment:test-1\n');
        await expect(page.locator('.upload-status')).toHaveText('Attached upload-new.png');
        expect((await prompt.inputValue()).match(/Attachment: attachment:test-1/g)).toHaveLength(1);
      } finally {
        await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.release()).catch(() => {});
      }
    });

    test(`shared composer ${mode}: chunked upload old cancel ${oldReply} cannot overwrite a completed newer upload`, async ({ page }) => {
      const { prompt, input, oldId } = await startHeldOldCancellation(page, mode);
      try {
        await releaseCanceledUpload(page, input);
        await input.setInputFiles({ name: 'upload-new.png', mimeType: 'image/png', buffer: Buffer.from('abc') });
        await expect(prompt).toHaveValue('Keep overlap draft\nAttachment: attachment:test-1\n');
        await expect(page.locator('.upload-status')).toHaveText('Attached upload-new.png');
        await expect(input).toBeEnabled();
        const after = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
        expect(after.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_cancel', 'upload_begin', 'upload_chunk', 'upload_finish']);
        expect(after[3]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-new.png', media_type: 'image/png', bytes: 3 }] });
        const newId = `upload-${String(after[3].request_id)}`;
        expect(newId).not.toBe(oldId);
        expect(after[4]).toMatchObject({ target: composerTarget, upload_id: newId, file_index: 0, sequence: 0, data: 'YWJj', sha256: uploadNewDigest });
        expect(after[5]).toMatchObject({ target: composerTarget, upload_id: newId, files: [{ file_index: 0, sha256: uploadNewDigest }] });
        await deliverOverlapReply(page, 'cancel', oldReply === 'error');
        await expect(page.locator('.upload-status')).toHaveText('Attached upload-new.png');
        expect((await commands(page)).filter(command => String(command.type).startsWith('upload_'))).toEqual(after);
        await expect(prompt).toHaveValue('Keep overlap draft\nAttachment: attachment:test-1\n');
        await expect(page.locator('.upload-status')).toHaveText('Attached upload-new.png');
      } finally {
        await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.release()).catch(() => {});
      }
    });
  }

  test(`shared composer ${mode}: chunked upload current cancel error remains visible after local release`, async ({ page }) => {
    const { prompt } = await startHeldOldCancellation(page, mode);
    try {
      await deliverOverlapReply(page, 'cancel', true);
      await expect(page.locator('.upload-status')).toHaveText('The relay could not confirm attachment cancellation.');
      await expect(page.locator('.upload-status')).toHaveClass(/error/);
      await settleOverlapOldRead(page);
      await expect(prompt).toHaveValue('Keep overlap draft');
      await expect(page.locator('.upload-status')).toHaveText('The relay could not confirm attachment cancellation.');
      expect((await commands(page)).filter(command => command.type === 'upload_finish')).toHaveLength(0);
    } finally {
      await page.evaluate(() => (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.release()).catch(() => {});
    }
  });
}

// Fail the actual mock socket send before its server receives a cancellation.
type ImmediateCancelSendFault = {
  attempts: Record<string, unknown>[];
  restore(): void;
};

for (const mode of ['Conversation', 'Full history']) {
  test(`shared composer ${mode}: chunked upload immediate cancel send failure retains uncertainty`, async ({ page }) => {
    await instrumentUploadRead(page, 'hold');
    const prompt = await openSharedComposer(page, mode);
    await page.evaluate(() => {
      const socket = window.__relaySockets[0];
      const original = socket.send;
      const fault: ImmediateCancelSendFault = {
        attempts: [],
        restore: () => { socket.send = original; },
      };
      socket.send = function (serialized) {
        const packet = JSON.parse(serialized) as Record<string, unknown>;
        if (packet.type === 'upload_cancel') {
          fault.attempts.push(packet);
          throw new Error('injected immediate upload_cancel send failure');
        }
        original.call(this, serialized);
      };
      Object.defineProperty(window, '__immediateCancelSendFault', { value: fault });
    });
    await prompt.fill('Keep immediate cancel draft');
    const input = page.locator('input[type=file][accept="image/*"]');
    const form = page.getByRole('form', { name: 'Send a prompt' });
    const status = page.locator('.upload-status');
    try {
      await input.setInputFiles({ name: 'upload-old.png', mimeType: 'image/png', buffer: uploadOldBytes });
      await expect.poll(() => page.evaluate(() => {
        const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
        return probe.waiting && probe.nativeFinished && !probe.settled;
      })).toBe(true);
      await expect(form).toHaveAttribute('aria-busy', 'true');
      const before = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
      expect(before.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk']);
      expect(before[0]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-old.png', media_type: 'image/png', bytes: uploadOldBytes.length }] });
      expect(typeof before[0].request_id).toBe('string');
      const oldId = `upload-${String(before[0].request_id)}`;
      assertOldFirstChunk(before[1], oldId);
      await page.getByRole('button', { name: 'Cancel upload' }).click();
      const attempts = await page.evaluate(() => (window as unknown as { __immediateCancelSendFault: ImmediateCancelSendFault }).__immediateCancelSendFault.attempts);
      expect(attempts).toHaveLength(1);
      expect(attempts[0]).toMatchObject({ type: 'upload_cancel', target: composerTarget, upload_id: oldId });
      expect(typeof attempts[0].request_id).toBe('string');
      expect(attempts[0].request_id).not.toBe(before[0].request_id);
      expect((await commands(page)).filter(command => String(command.type).startsWith('upload_'))).toEqual(before);
      const held = await page.evaluate(() => {
        const probe = (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe;
        return { waiting: probe.waiting, nativeFinished: probe.nativeFinished, settled: probe.settled, reads: probe.reads, slices: probe.slices };
      });
      expect(held).toEqual({ waiting: true, nativeFinished: true, settled: false, reads: 1, slices: [
        { start: 0, end: 262144 }, { start: 262144, end: 524288 },
      ] });
      console.log('immediate-cancel observation', JSON.stringify({ mode, oldId, attemptedCancel: attempts[0], held, status: await status.textContent(), statusClass: await status.getAttribute('class') }));
      await expect(status).toHaveText('The relay could not confirm attachment cancellation.');
      await expect(status).toHaveClass(/error/);
      await expect(prompt).toHaveValue('Keep immediate cancel draft');

      await settleOverlapOldRead(page);
      await expect(form).toHaveAttribute('aria-busy', 'false');
      await expect(page.getByRole('button', { name: 'Attach' })).toBeEnabled();
      await expect(page.getByRole('button', { name: 'Cancel upload' })).toHaveCount(0);
      await expect(status).toHaveText('The relay could not confirm attachment cancellation.');
      await expect(status).toHaveClass(/error/);
      expect((await commands(page)).filter(command => String(command.type).startsWith('upload_'))).toEqual(before);
      await expect(prompt).toHaveValue('Keep immediate cancel draft');

      await page.evaluate(() => (window as unknown as { __immediateCancelSendFault: ImmediateCancelSendFault }).__immediateCancelSendFault.restore());
      await input.setInputFiles({ name: 'upload-new.png', mimeType: 'image/png', buffer: Buffer.from('abc') });
      await expect(prompt).toHaveValue('Keep immediate cancel draft\nAttachment: attachment:test-1\n');
      await expect(status).toHaveText('Attached upload-new.png');
      await expect(status).not.toHaveClass(/error/);
      await expect(form).toHaveAttribute('aria-busy', 'false');
      const after = (await commands(page)).filter(command => String(command.type).startsWith('upload_'));
      expect(after.map(command => command.type)).toEqual(['upload_begin', 'upload_chunk', 'upload_begin', 'upload_chunk', 'upload_finish']);
      expect(after.slice(0, 2)).toEqual(before);
      expect(after[2]).toMatchObject({ target: composerTarget, files: [{ name: 'upload-new.png', media_type: 'image/png', bytes: 3 }] });
      const newId = `upload-${String(after[2].request_id)}`;
      expect(newId).not.toBe(oldId);
      expect(after[3]).toMatchObject({ target: composerTarget, upload_id: newId, file_index: 0, sequence: 0, data: 'YWJj', sha256: uploadNewDigest });
      expect(after[4]).toMatchObject({ target: composerTarget, upload_id: newId, files: [{ file_index: 0, sha256: uploadNewDigest }] });
      expect(after.filter(command => command.type === 'upload_finish')).toHaveLength(1);
      expect((await prompt.inputValue()).match(/Attachment: attachment:test-1/g)).toHaveLength(1);
    } finally {
      await page.evaluate(() => {
        (window as unknown as { __immediateCancelSendFault: ImmediateCancelSendFault }).__immediateCancelSendFault.restore();
        (window as unknown as { __uploadReadProbe: UploadReadProbe }).__uploadReadProbe.release();
      });
    }
  });
}
