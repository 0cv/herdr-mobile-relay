import { rename, rm, writeFile } from 'node:fs/promises';

export interface DownloadOptions {
  maxAttempts?: number;
  totalTimeoutMs?: number;
  maxBytes?: number;
  fetchImpl?: typeof fetch;
  sleep?: (milliseconds: number) => Promise<void>;
  validate?: (data: Uint8Array) => void | Promise<void>;
}

const defaultMaxAttempts = 3;
const defaultTotalTimeoutMs = 90_000;
const defaultMaxBytes = 512 * 1024 * 1024;

function retryableStatus(status: number): boolean {
  return status === 408 || status === 425 || status === 429 || status >= 500;
}

function messageFor(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

export async function downloadWithRetry(url: string, filename: string, options: DownloadOptions = {}): Promise<void> {
  const maxAttempts = Math.max(1, Math.floor(options.maxAttempts ?? defaultMaxAttempts));
  const totalTimeoutMs = Math.max(1, options.totalTimeoutMs ?? defaultTotalTimeoutMs);
  const maxBytes = Math.max(1, options.maxBytes ?? defaultMaxBytes);
  const fetchImpl = options.fetchImpl || fetch;
  const sleep = options.sleep || ((milliseconds: number) => new Promise<void>((resolve) => setTimeout(resolve, milliseconds)));
  const deadline = Date.now() + totalTimeoutMs;
  let lastError = '';

  for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
    const remainingMs = deadline - Date.now();
    if (remainingMs <= 0) break;
    const temporary = `${filename}.part-${process.pid}-${attempt}`;
    let networkPhase = true;
    try {
      const response = await fetchImpl(url, {
        redirect: 'follow',
        signal: AbortSignal.timeout(Math.min(30_000, remainingMs)),
      });
      if (!response.ok) {
        lastError = `baseline download returned HTTP ${response.status}`;
        if (!retryableStatus(response.status)) throw new Error(lastError);
      } else {
        const contentLength = Number(response.headers.get('content-length') || 0);
        if (contentLength > maxBytes) throw new Error('baseline archive is larger than the allowed limit');
        const data = new Uint8Array(await response.arrayBuffer());
        if (data.byteLength > maxBytes) throw new Error('baseline archive is larger than the allowed limit');
        networkPhase = false;
        await options.validate?.(data);
        await writeFile(temporary, data, { mode: 0o600 });
        await rename(temporary, filename);
        return;
      }
    } catch (error) {
      if (!networkPhase) throw error;
      const detail = messageFor(error);
      if (/baseline archive is larger than the allowed limit/u.test(detail)) throw error;
      const status = detail.match(/^baseline download returned HTTP (\d+)$/u)?.[1];
      if (status && !retryableStatus(Number(status))) throw error;
      lastError = status ? detail : `baseline download failed: ${detail}`;
    } finally {
      await rm(temporary, { force: true });
    }
    if (attempt + 1 >= maxAttempts || Date.now() >= deadline) break;
    await sleep(Math.min(500, 100 * (attempt + 1), Math.max(0, deadline - Date.now())));
  }

  throw new Error(lastError || 'baseline download timed out');
}
