import { afterEach, describe, expect, it, vi } from 'vitest';
import { base64UrlEncode } from '$lib/base64url';
import type { RelayDeviceCredential } from '$lib/device-auth';
import { decryptLastKnown, deriveLastKnownAssociation, encryptLastKnown, parseLastKnownEnvelope, type LastKnownCryptoContext } from '$lib/last-known-crypto';
import { LAST_KNOWN_MAX_AGE_MS, LAST_KNOWN_MAX_ENTRY_BYTES, LAST_KNOWN_MAX_PLAINTEXT_BYTES } from '$lib/last-known-types';

const now = 1_800_000_000_000;
const credential: RelayDeviceCredential = {
  kind: 'credential', id: 'fixture-credential', version: 1, deviceId: 'fixture-device',
  secret: base64UrlEncode(new Uint8Array(32).fill(7)), role: 'controller', locale: 'en', issuedAt: now - 100,
};
const context: LastKnownCryptoContext = { origin: 'https://app.example', relayId: 'local-relay', credential, lastFreshAt: now };
const encrypt = (text: string, ctx = context) => encryptLastKnown(text, ctx, crypto, now);
const decrypt = (raw: string, ctx = context) => decryptLastKnown(raw, ctx, crypto, now);

afterEach(() => vi.restoreAllMocks());

describe('last-known domain-separated authenticated encryption', () => {
  it('derives non-extractable, domain-separated opaque associations without persisting descriptive IDs', async () => {
    const derive = vi.spyOn(crypto.subtle, 'deriveKey');
    const basis = 'sensitive-host-and-label';
    const alias = await deriveLastKnownAssociation(context, basis, crypto, now);
    expect(alias).toHaveLength(43);
    expect(alias).not.toContain(basis);
    expect(derive.mock.calls[0][2]).toEqual({ name: 'HMAC', hash: 'SHA-256', length: 256 });
    expect((await derive.mock.results[0].value).extractable).toBe(false);
    expect(new TextDecoder().decode((derive.mock.calls[0][0] as HkdfParams).info)).toContain('herdr:last-known:association:v1');
    await expect(deriveLastKnownAssociation(context, basis, crypto, now)).resolves.toBe(alias);
    for (const changed of [
      { ...context, origin: 'https://other.example' },
      { ...context, credential: { ...credential, version: 2 } },
      { ...context, credential: { ...credential, deviceId: 'other-device' } },
      { ...context, credential: { ...credential, role: 'reader' as const } },
    ]) expect(await deriveLastKnownAssociation(changed, basis, crypto, now)).not.toBe(alias);
    expect(await deriveLastKnownAssociation(context, 'other-configuration', crypto, now)).not.toBe(alias);
  });

  it('round-trips ciphertext only with random salt and fresh 96-bit IV per write', async () => {
    const text = JSON.stringify({ canary: 'private-label-canary' });
    const first = await encrypt(text);
    const second = await encrypt(text);
    expect(first).not.toContain('private-label-canary');
    expect(first).not.toContain(credential.secret);
    const one = parseLastKnownEnvelope(first, now);
    const two = parseLastKnownEnvelope(second, now);
    expect(one.salt).not.toEqual(two.salt);
    expect(one.iv).not.toEqual(two.iv);
    expect(one.iv).toHaveLength(16);
    expect(one.salt).toHaveLength(43);
    expect(one.expiresAt).toBe(now + LAST_KNOWN_MAX_AGE_MS);
    await expect(decrypt(first)).resolves.toBe(text);
  });

  it('uses HKDF-SHA-256 and non-extractable AES-256-GCM keys, never traffic keys', async () => {
    const derive = vi.spyOn(crypto.subtle, 'deriveKey');
    await encrypt('display label');
    expect(derive).toHaveBeenCalledOnce();
    const [parameters, material, algorithm, extractable, usages] = derive.mock.calls[0];
    expect(parameters).toMatchObject({ name: 'HKDF', hash: 'SHA-256' });
    expect(new TextDecoder().decode((parameters as HkdfParams).info)).toContain('herdr:last-known:hkdf:v1');
    expect(material.extractable).toBe(false);
    expect(algorithm).toEqual({ name: 'AES-GCM', length: 256 });
    expect(extractable).toBe(false);
    expect(usages).toEqual(['encrypt', 'decrypt']);
    const key = await derive.mock.results[0].value;
    expect(key.extractable).toBe(false);
    await expect(crypto.subtle.exportKey('raw', key)).rejects.toThrow();
  });

  it.each(['origin', 'relay', 'credential', 'version', 'secret', 'last-fresh'] as const)(
    'rejects wrong %s context', async (field) => {
      const raw = await encrypt('label');
      const wrong: LastKnownCryptoContext = { ...context, credential: { ...credential } };
      if (field === 'origin') wrong.origin = 'https://other.example';
      if (field === 'relay') wrong.relayId = 'other-local-relay';
      if (field === 'credential') wrong.credential.id = 'other-credential';
      if (field === 'version') wrong.credential.version++;
      if (field === 'secret') wrong.credential.secret = base64UrlEncode(new Uint8Array(32).fill(8));
      if (field === 'last-fresh') wrong.lastFreshAt--;
      await expect(decrypt(raw, wrong)).rejects.toThrow();
    },
  );

  it.each(['salt', 'iv', 'ciphertext', 'lastFreshAt', 'expiresAt', 'origin', 'relayId', 'credentialId', 'credentialVersion'])(
    'rejects tampering with authenticated %s', async (field) => {
      const envelope = JSON.parse(await encrypt('label')) as Record<string, unknown>;
      if (field === 'lastFreshAt') envelope.lastFreshAt = now - 1;
      else if (field === 'expiresAt') envelope.expiresAt = now + LAST_KNOWN_MAX_AGE_MS - 1;
      else if (field === 'credentialVersion') envelope.credentialVersion = 2;
      else if (['salt', 'iv', 'ciphertext'].includes(field)) {
        const value = String(envelope[field]);
        envelope[field] = `${value[0] === 'A' ? 'B' : 'A'}${value.slice(1)}`;
      } else envelope[field] = field === 'origin' ? 'https://other.example' : 'wrong';
      await expect(decrypt(JSON.stringify(envelope))).rejects.toThrow();
    },
  );

  it('rejects oversized and malformed envelopes before decrypting or deriving a key', async () => {
    const valid = JSON.parse(await encrypt('label')) as Record<string, unknown>;
    const derive = vi.spyOn(crypto.subtle, 'deriveKey');
    const decryptSpy = vi.spyOn(crypto.subtle, 'decrypt');
    for (const raw of [
      'x'.repeat(LAST_KNOWN_MAX_ENTRY_BYTES + 1), JSON.stringify({ ...valid, schema: 2 }),
      JSON.stringify({ ...valid, extra: 'forbidden' }), JSON.stringify({ ...valid, iv: 'short' }),
      JSON.stringify({ ...valid, ciphertext: 'x'.repeat(90_000) }),
      JSON.stringify({ ...valid, lastFreshAt: now + 1, expiresAt: now + 1 + LAST_KNOWN_MAX_AGE_MS }),
    ]) await expect(decrypt(raw)).rejects.toThrow();
    expect(derive).not.toHaveBeenCalled();
    expect(decryptSpy).not.toHaveBeenCalled();
  });

  it('rejects expiry, invitations, malformed secrets and unavailable crypto without plaintext fallback', async () => {
    const raw = await encrypt('label');
    await expect(decryptLastKnown(raw, context, crypto, now + LAST_KNOWN_MAX_AGE_MS)).rejects.toThrow();
    await expect(encrypt('label', { ...context, credential: { ...credential, kind: 'invitation' } as unknown as RelayDeviceCredential })).rejects.toThrow();
    await expect(encrypt('label', { ...context, credential: { ...credential, secret: 'invalid' } })).rejects.toThrow();
    await expect(encryptLastKnown('label', context, {} as Crypto, now)).rejects.toThrow();
    await expect(encrypt('x'.repeat(LAST_KNOWN_MAX_PLAINTEXT_BYTES + 1))).rejects.toThrow();
  });
});
