import { describe, expect, it } from 'vitest';
import { base64UrlDecode, base64UrlEncode } from '$lib/base64url';
import type { RelayDeviceAuthentication } from '$lib/device-auth';
import { createE2EEClientHandshake, E2EESession, type E2EEClientHello } from '$lib/e2ee';
import { authenticationBinding, concatenate, createServer, encryptServerFinish, frameAAD, frameNonce } from '../support/e2ee-server';

const encoder = new TextEncoder();
const decoder = new TextDecoder();

const credential: RelayDeviceAuthentication = {
  kind: 'credential',
  id: 'credential-7',
  version: 3,
  secret: base64UrlEncode(new Uint8Array(new ArrayBuffer(32)).fill(7)),
};

const invitation: RelayDeviceAuthentication = {
  kind: 'invitation',
  id: 'invitation-9',
  version: 1,
  secret: base64UrlEncode(new Uint8Array(new ArrayBuffer(32)).fill(9)),
};

describe('relay end-to-end encryption v2', () => {
  it('encrypts both directions with version two frame binding and rejects replay', async () => {
    const clientKeyBytes = crypto.getRandomValues(new Uint8Array(new ArrayBuffer(32)));
    const serverKeyBytes = crypto.getRandomValues(new Uint8Array(new ArrayBuffer(32)));
    const clientKey = await crypto.subtle.importKey('raw', clientKeyBytes, 'AES-GCM', false, ['encrypt', 'decrypt']);
    const serverKey = await crypto.subtle.importKey('raw', serverKeyBytes, 'AES-GCM', false, ['encrypt', 'decrypt']);
    const session = new E2EESession(clientKey, serverKey);

    const plaintext = JSON.stringify({ type: 'submit_prompt', text: 'private prompt' });
    const encrypted = String(await session.encrypt(plaintext));
    expect(encrypted).not.toContain('private prompt');
    expect(JSON.parse(encrypted)).toMatchObject({ type: 'e2ee', version: 2, sequence: 0 });
    const clientFrame = JSON.parse(encrypted) as Record<string, unknown>;
    const decryptedClient = await crypto.subtle.decrypt({
      name: 'AES-GCM', iv: frameNonce(0), additionalData: frameAAD('c2s', 0), tagLength: 128,
    }, clientKey, base64UrlDecode(String(clientFrame.ciphertext)));
    expect(decoder.decode(decryptedClient)).toBe(plaintext);

    const reply = JSON.stringify({ type: 'pane_content', content: 'private terminal output' });
    const encryptedReply = new Uint8Array(await crypto.subtle.encrypt({
      name: 'AES-GCM', iv: frameNonce(0), additionalData: frameAAD('s2c', 0), tagLength: 128,
    }, serverKey, encoder.encode(reply)));
    const serverFrame = JSON.stringify({
      type: 'e2ee', version: 2, sequence: 0, ciphertext: base64UrlEncode(encryptedReply),
    });
    await expect(session.decrypt(serverFrame)).resolves.toBe(reply);
    await expect(session.decrypt(serverFrame)).rejects.toThrow(/sequence/);
  });

  it('binds credential kind, id, and version into the client proof', async () => {
    const handshake = await createE2EEClientHandshake(credential);
    expect(handshake.hello).toMatchObject({
      type: 'e2ee_client_hello',
      version: 2,
      auth_kind: 'credential',
      auth_id: credential.id,
      auth_version: credential.version,
    });
    expect(JSON.stringify(handshake.hello)).not.toContain(credential.secret);

    const key = await crypto.subtle.importKey(
      'raw', base64UrlDecode(credential.secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign'],
    );
    const expected = await crypto.subtle.sign('HMAC', key, concatenate(
      encoder.encode('herdr-e2ee-v2 client\0'),
      authenticationBinding(credential),
      base64UrlDecode(handshake.hello.nonce),
      base64UrlDecode(handshake.hello.public_key),
    ));
    expect(handshake.hello.proof).toBe(base64UrlEncode(new Uint8Array(expected)));

    const changed = await createE2EEClientHandshake({ ...credential, version: credential.version + 1 }, {
      keyPair: await importClientKeyPair(handshake.hello),
      nonce: base64UrlDecode(handshake.hello.nonce),
    });
    expect(changed.hello.proof).not.toBe(handshake.hello.proof);
  });

  it('authenticates the encrypted server finish before returning a credential identity', async () => {
    const handshake = await createE2EEClientHandshake(credential);
    const server = await createServer(handshake.hello, credential);
    const challenge = await handshake.complete(server.hello);
    const finish = await encryptServerFinish(server.sendKey, {
      type: 'e2ee_server_finish',
      version: 2,
      device_id: 'device-1',
      credential_id: credential.id,
      credential_version: credential.version,
      role: 'controller',
      locale: 'en',
    });
    const completed = await challenge.complete(finish);

    expect(completed.enrollment).toEqual({
      deviceId: 'device-1',
      credentialId: credential.id,
      credentialVersion: credential.version,
      role: 'controller',
      locale: 'en',
    });
    await expect(challenge.complete(finish)).rejects.toThrow(/already complete/);
  });

  it('accepts an issued secret only for invitation redemption', async () => {
    const handshake = await createE2EEClientHandshake(invitation);
    const server = await createServer(handshake.hello, invitation);
    const challenge = await handshake.complete(server.hello);
    const issuedSecret = base64UrlEncode(new Uint8Array(new ArrayBuffer(32)).fill(4));
    const completed = await challenge.complete(await encryptServerFinish(server.sendKey, {
      type: 'e2ee_server_finish',
      version: 2,
      device_id: 'device-new',
      credential_id: 'credential-new',
      credential_version: 1,
      credential_secret: issuedSecret,
      role: 'reader',
      locale: 'zh-CN',
    }));
    expect(completed.enrollment.credentialSecret).toBe(issuedSecret);

    const credentialHandshake = await createE2EEClientHandshake(credential);
    const credentialServer = await createServer(credentialHandshake.hello, credential);
    const credentialChallenge = await credentialHandshake.complete(credentialServer.hello);
    await expect(credentialChallenge.complete(await encryptServerFinish(credentialServer.sendKey, {
      type: 'e2ee_server_finish',
      version: 2,
      device_id: 'device-1',
      credential_id: credential.id,
      credential_version: credential.version,
      credential_secret: issuedSecret,
      role: 'controller',
      locale: 'en',
    }))).rejects.toThrow(/unexpectedly replaced/);
  });

  it('rejects secrets that are not exactly 32 decoded bytes', async () => {
    await expect(createE2EEClientHandshake({
      ...credential,
      secret: base64UrlEncode(new Uint8Array(new ArrayBuffer(31))),
    })).rejects.toThrow(/32 bytes/);
  });
});

async function importClientKeyPair(hello: E2EEClientHello): Promise<CryptoKeyPair> {
  const rawPublic = base64UrlDecode(hello.public_key);
  const publicKey = await crypto.subtle.importKey(
    'raw', rawPublic, { name: 'ECDH', namedCurve: 'P-256' }, true, [],
  );
  const generated = await crypto.subtle.generateKey(
    { name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits'],
  ) as CryptoKeyPair;
  return { privateKey: generated.privateKey, publicKey };
}
