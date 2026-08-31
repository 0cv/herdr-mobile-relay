import { describe, expect, it } from 'vitest';
import { base64UrlDecode, base64UrlEncode } from '$lib/base64url';
import type { RelayDeviceAuthentication } from '$lib/device-auth';
import { createE2EEClientHandshake, E2EESession, type E2EEClientHello } from '$lib/e2ee';

const encoder = new TextEncoder();
const decoder = new TextDecoder();
type Bytes = Uint8Array<ArrayBuffer>;

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

async function createServer(hello: E2EEClientHello, authentication: RelayDeviceAuthentication) {
  const serverPair = await crypto.subtle.generateKey(
    { name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits'],
  ) as CryptoKeyPair;
  const serverNonce = crypto.getRandomValues(new Uint8Array(new ArrayBuffer(32)));
  const serverPublic = new Uint8Array(await crypto.subtle.exportKey('raw', serverPair.publicKey));
  const clientPublic = base64UrlDecode(hello.public_key);
  const importedClientPublic = await crypto.subtle.importKey(
    'raw', clientPublic, { name: 'ECDH', namedCurve: 'P-256' }, false, [],
  );
  const sharedSecret = new Uint8Array(await crypto.subtle.deriveBits(
    { name: 'ECDH', public: importedClientPublic }, serverPair.privateKey, 256,
  ));
  const authKey = await crypto.subtle.importKey(
    'raw', base64UrlDecode(authentication.secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign'],
  );
  const transcript = concatenate(
    authenticationBinding(authentication),
    base64UrlDecode(hello.nonce),
    clientPublic,
    serverNonce,
    serverPublic,
  );
  const proof = new Uint8Array(await crypto.subtle.sign(
    'HMAC', authKey, concatenate(encoder.encode('herdr-e2ee-v2 server\0'), transcript),
  ));
  const salt = new Uint8Array(await crypto.subtle.sign(
    'HMAC', authKey, concatenate(encoder.encode('herdr-e2ee-v2 key\0'), transcript),
  ));
  const material = await crypto.subtle.importKey('raw', sharedSecret, 'HKDF', false, ['deriveKey']);
  const sendKey = await crypto.subtle.deriveKey(
    { name: 'HKDF', hash: 'SHA-256', salt, info: encoder.encode('herdr-e2ee-v2 s2c') },
    material,
    { name: 'AES-GCM', length: 256 },
    false,
    ['encrypt'],
  );
  return {
    hello: {
      type: 'e2ee_server_hello',
      version: 2,
      nonce: base64UrlEncode(serverNonce),
      public_key: base64UrlEncode(serverPublic),
      proof: base64UrlEncode(proof),
    },
    sendKey,
  };
}

async function encryptServerFinish(sendKey: CryptoKey, finish: Record<string, unknown>): Promise<string> {
  const ciphertext = new Uint8Array(await crypto.subtle.encrypt({
    name: 'AES-GCM', iv: frameNonce(0), additionalData: frameAAD('s2c', 0), tagLength: 128,
  }, sendKey, encoder.encode(JSON.stringify(finish))));
  return JSON.stringify({ type: 'e2ee', version: 2, sequence: 0, ciphertext: base64UrlEncode(ciphertext) });
}

function authenticationBinding(authentication: RelayDeviceAuthentication): Bytes {
  return encoder.encode(
    `herdr-e2ee-v2 auth\0${authentication.kind}\0${authentication.id}\0${authentication.version}\0`,
  );
}

function frameNonce(sequence: number): Bytes {
  const nonce = new Uint8Array(new ArrayBuffer(12));
  new DataView(nonce.buffer).setBigUint64(4, BigInt(sequence), false);
  return nonce;
}

function frameAAD(direction: 'c2s' | 's2c', sequence: number): Bytes {
  const prefix = encoder.encode(`herdr-e2ee-v2 ${direction}`);
  const aad = new Uint8Array(new ArrayBuffer(prefix.length + 9));
  aad.set(prefix);
  new DataView(aad.buffer).setBigUint64(prefix.length + 1, BigInt(sequence), false);
  return aad;
}

function concatenate(...parts: Bytes[]): Bytes {
  const result = new Uint8Array(new ArrayBuffer(parts.reduce((size, part) => size + part.length, 0)));
  let offset = 0;
  for (const part of parts) {
    result.set(part, offset);
    offset += part.length;
  }
  return result;
}

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
