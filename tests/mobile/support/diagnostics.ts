import { appendFile, mkdir, readFile, writeFile } from 'node:fs/promises';
import { dirname } from 'node:path';

const MAX_DIAGNOSTIC_BYTES = 100 * 1024 * 1024;
const REDACTED = '[REDACTED]';
const secretPatterns = [
  /([?#&](?:setup|invite|invite_version|invite_expires|token|secret|credential|control)(?:=|%3D))[^&#\s"'}]+/giu,
  /\b[A-Za-z0-9_-]{43}\b/g,
  /Bearer\s+[A-Za-z0-9._~-]+/giu,
];

export function redactText(value: string): string {
  return secretPatterns.reduce((result, pattern) => result.replace(pattern, (match, prefix: string) => {
    if (prefix) return `${prefix}${REDACTED}`;
    return REDACTED;
  }), value);
}

export function sanitizeValue(value: unknown): unknown {
  if (typeof value === 'string') return redactText(value);
  if (Array.isArray(value)) return value.map(sanitizeValue);
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, sanitizeValue(item)]));
  }
  return value;
}

export function assertNoKnownSecret(text: string, secrets: readonly string[]): void {
  const normalized = text.replaceAll('\\/', '/');
  for (const secret of secrets) {
    if (!secret) continue;
    if (normalized.includes(secret) || normalized.includes(encodeURIComponent(secret))) {
      throw new Error('DIAGNOSTIC_SECRET: evidence contains a generated secret');
    }
  }
}

export async function writeSanitizedJson(filename: string, value: unknown): Promise<void> {
  const content = `${JSON.stringify(sanitizeValue(value), null, 2)}\n`;
  if (Buffer.byteLength(content) > MAX_DIAGNOSTIC_BYTES) throw new Error('DIAGNOSTIC_LIMIT: JSON evidence exceeds the limit');
  await mkdir(dirname(filename), { recursive: true });
  await writeFile(filename, content, { mode: 0o600 });
}

export async function appendSanitized(filename: string, value: string): Promise<void> {
  const content = redactText(value);
  const existing = await readFile(filename).catch(() => Buffer.alloc(0));
  if (existing.byteLength + Buffer.byteLength(content) > MAX_DIAGNOSTIC_BYTES) {
    throw new Error('DIAGNOSTIC_LIMIT: text evidence exceeds the limit');
  }
  await mkdir(dirname(filename), { recursive: true });
  await appendFile(filename, content, { mode: 0o600 });
}
