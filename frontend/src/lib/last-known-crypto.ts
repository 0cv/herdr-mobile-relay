import { base64UrlDecode, base64UrlEncode } from './base64url';
import type { RelayDeviceCredential } from './device-auth';
import {
  LAST_KNOWN_MAX_AGE_MS,
  LAST_KNOWN_MAX_ENTRY_BYTES,
  LAST_KNOWN_MAX_PLAINTEXT_BYTES,
  LAST_KNOWN_SCHEMA,
  lastKnownIdentifier,
  lastKnownKeys,
  lastKnownRecord,
  lastKnownTimestamp,
} from './last-known-types';

const DOMAIN = 'herdr:last-known:hkdf:v1';
const encoder = new TextEncoder();
const MAX_CIPHERTEXT_CHARACTERS = Math.ceil((LAST_KNOWN_MAX_PLAINTEXT_BYTES + 16) * 4 / 3);

export interface LastKnownCryptoContext {
  origin: string;
  relayId: string;
  credential: RelayDeviceCredential;
  lastFreshAt: number;
}

export interface LastKnownEnvelope {
  schema: typeof LAST_KNOWN_SCHEMA;
  origin: string;
  relayId: string;
  credentialId: string;
  credentialVersion: number;
  lastFreshAt: number;
  expiresAt: number;
  salt: string;
  iv: string;
  ciphertext: string;
}

/** Bounded, structural validation happens before base64 decoding or WebCrypto. */
export function parseLastKnownEnvelope(raw: string, now = Date.now()): LastKnownEnvelope {
  if (typeof raw !== 'string' || raw.length > LAST_KNOWN_MAX_ENTRY_BYTES
    || encoder.encode(raw).byteLength > LAST_KNOWN_MAX_ENTRY_BYTES) throw unavailable();
  const value: unknown = JSON.parse(raw);
  if (!lastKnownRecord(value) || !lastKnownKeys(value, [
    'schema', 'origin', 'relayId', 'credentialId', 'credentialVersion',
    'lastFreshAt', 'expiresAt', 'salt', 'iv', 'ciphertext',
  ]) || value.schema !== LAST_KNOWN_SCHEMA || !validOrigin(value.origin)
    || !lastKnownIdentifier(value.relayId) || !lastKnownIdentifier(value.credentialId)
    || !validVersion(value.credentialVersion) || !lastKnownTimestamp(value.lastFreshAt)
    || !lastKnownTimestamp(value.expiresAt) || !lastKnownTimestamp(now)
    || value.lastFreshAt > now || value.expiresAt <= now
    || value.expiresAt !== value.lastFreshAt + LAST_KNOWN_MAX_AGE_MS
    || typeof value.salt !== 'string' || !/^[A-Za-z0-9_-]{42}[AEIMQUYcgkosw048]$/.test(value.salt)
    || typeof value.iv !== 'string' || !/^[A-Za-z0-9_-]{16}$/.test(value.iv)
    || typeof value.ciphertext !== 'string' || value.ciphertext.length < 22
    || value.ciphertext.length > MAX_CIPHERTEXT_CHARACTERS
    || value.ciphertext.length % 4 === 1 || !/^[A-Za-z0-9_-]+$/.test(value.ciphertext)) throw unavailable();
  return value as unknown as LastKnownEnvelope;
}

export async function encryptLastKnown(
  plaintext: string,
  context: LastKnownCryptoContext,
  webCrypto: Crypto = globalThis.crypto,
  now = Date.now(),
): Promise<string> {
  validateContext(context, now);
  const bytes = encoder.encode(plaintext);
  if (bytes.byteLength > LAST_KNOWN_MAX_PLAINTEXT_BYTES || !webCrypto?.subtle) throw unavailable();
  const salt = webCrypto.getRandomValues(new Uint8Array(32));
  const iv = webCrypto.getRandomValues(new Uint8Array(12));
  const envelope: LastKnownEnvelope = {
    schema: LAST_KNOWN_SCHEMA,
    origin: context.origin,
    relayId: context.relayId,
    credentialId: context.credential.id,
    credentialVersion: context.credential.version,
    lastFreshAt: context.lastFreshAt,
    expiresAt: context.lastFreshAt + LAST_KNOWN_MAX_AGE_MS,
    salt: base64UrlEncode(salt),
    iv: base64UrlEncode(iv),
    ciphertext: '',
  };
  const aad = authenticatedContext(envelope);
  const key = await deriveKey(context.credential, salt, aad, webCrypto);
  envelope.ciphertext = base64UrlEncode(await webCrypto.subtle.encrypt(
    { name: 'AES-GCM', iv, additionalData: aad, tagLength: 128 }, key, bytes,
  ));
  const raw = JSON.stringify(envelope);
  parseLastKnownEnvelope(raw, now);
  return raw;
}

export async function decryptLastKnown(
  raw: string,
  context: LastKnownCryptoContext,
  webCrypto: Crypto = globalThis.crypto,
  now = Date.now(),
): Promise<string> {
  const envelope = parseLastKnownEnvelope(raw, now);
  validateContext(context, now);
  if (!webCrypto?.subtle || envelope.origin !== context.origin || envelope.relayId !== context.relayId
    || envelope.credentialId !== context.credential.id
    || envelope.credentialVersion !== context.credential.version
    || envelope.lastFreshAt !== context.lastFreshAt) throw unavailable();
  const salt = canonicalDecode(envelope.salt, 32);
  const iv = canonicalDecode(envelope.iv, 12);
  const ciphertext = canonicalDecode(envelope.ciphertext);
  if (ciphertext.byteLength > LAST_KNOWN_MAX_PLAINTEXT_BYTES + 16) throw unavailable();
  const aad = authenticatedContext(envelope);
  const key = await deriveKey(context.credential, salt, aad, webCrypto);
  const plaintext = await webCrypto.subtle.decrypt(
    { name: 'AES-GCM', iv, additionalData: aad, tagLength: 128 }, key, ciphertext,
  );
  if (plaintext.byteLength > LAST_KNOWN_MAX_PLAINTEXT_BYTES) throw unavailable();
  return new TextDecoder('utf-8', { fatal: true }).decode(plaintext);
}

async function deriveKey(
  credential: RelayDeviceCredential,
  salt: Uint8Array<ArrayBuffer>,
  context: Uint8Array<ArrayBuffer>,
  webCrypto: Crypto,
): Promise<CryptoKey> {
  const secret = canonicalDecode(credential.secret, 32);
  try {
    const material = await webCrypto.subtle.importKey('raw', secret, 'HKDF', false, ['deriveKey']);
    return await webCrypto.subtle.deriveKey(
      { name: 'HKDF', hash: 'SHA-256', salt, info: context }, material,
      { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt'],
    );
  } finally {
    // Best effort for this owned buffer only, not a secure-erasure claim.
    secret.fill(0);
  }
}

function authenticatedContext(envelope: LastKnownEnvelope): Uint8Array<ArrayBuffer> {
  // An ordered tuple avoids object-key-order ambiguity. The domain is also HKDF
  // info; origin/credential/session keys cannot be swapped between contexts.
  return encoder.encode(JSON.stringify([
    DOMAIN, envelope.origin, envelope.relayId, envelope.credentialId,
    envelope.credentialVersion, envelope.schema, envelope.lastFreshAt, envelope.expiresAt,
  ]));
}

function validateContext(context: LastKnownCryptoContext, now: number): void {
  const credential = context?.credential;
  if (!validOrigin(context?.origin) || !lastKnownIdentifier(context?.relayId)
    || !lastKnownTimestamp(context?.lastFreshAt) || !lastKnownTimestamp(now)
    || context.lastFreshAt > now || context.lastFreshAt + LAST_KNOWN_MAX_AGE_MS <= now
    || !credential || credential.kind !== 'credential' || !lastKnownIdentifier(credential.id)
    || !lastKnownIdentifier(credential.deviceId) || !validVersion(credential.version)
    || !['reader', 'controller'].includes(credential.role) || !lastKnownTimestamp(credential.issuedAt)
    || credential.issuedAt > now || typeof credential.locale !== 'string' || credential.locale.length > 35
    || !/^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$/.test(credential.locale)
    || typeof credential.secret !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(credential.secret)) throw unavailable();
  canonicalDecode(credential.secret, 32).fill(0);
}

function canonicalDecode(value: string, bytes?: number): Uint8Array<ArrayBuffer> {
  const decoded = base64UrlDecode(value);
  if ((bytes !== undefined && decoded.byteLength !== bytes) || base64UrlEncode(decoded) !== value) throw unavailable();
  return decoded;
}

function validVersion(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0;
}

function validOrigin(value: unknown): value is string {
  if (typeof value !== 'string' || value.length > 2048) return false;
  try {
    const url = new URL(value);
    return ['http:', 'https:'].includes(url.protocol) && url.origin === value;
  } catch {
    return false;
  }
}

function unavailable(): Error {
  return new Error('Last-known cache is unavailable.');
}
