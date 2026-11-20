import { base64UrlDecode, base64UrlEncode, type Base64UrlBytes as Bytes } from '../../src/lib/base64url';
import type { RelayDeviceAuthentication } from '../../src/lib/device-auth';
import type { E2EEClientHello } from '../../src/lib/e2ee';

const encoder = new TextEncoder();

export async function createServer(hello: Pick<E2EEClientHello, 'nonce' | 'public_key'>, authentication: RelayDeviceAuthentication) {
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

export async function encryptServerFinish(sendKey: CryptoKey, finish: Record<string, unknown>): Promise<string> {
  const ciphertext = new Uint8Array(await crypto.subtle.encrypt({
    name: 'AES-GCM', iv: frameNonce(0), additionalData: frameAAD('s2c', 0), tagLength: 128,
  }, sendKey, encoder.encode(JSON.stringify(finish))));
  return JSON.stringify({ type: 'e2ee', version: 2, sequence: 0, ciphertext: base64UrlEncode(ciphertext) });
}

export function authenticationBinding(authentication: RelayDeviceAuthentication): Bytes {
  return encoder.encode(
    `herdr-e2ee-v2 auth\0${authentication.kind}\0${authentication.id}\0${authentication.version}\0`,
  );
}

export function frameNonce(sequence: number): Bytes {
  const nonce = new Uint8Array(new ArrayBuffer(12));
  new DataView(nonce.buffer).setBigUint64(4, BigInt(sequence), false);
  return nonce;
}

export function frameAAD(direction: 'c2s' | 's2c', sequence: number): Bytes {
  const prefix = encoder.encode(`herdr-e2ee-v2 ${direction}`);
  const aad = new Uint8Array(new ArrayBuffer(prefix.length + 9));
  aad.set(prefix);
  new DataView(aad.buffer).setBigUint64(prefix.length + 1, BigInt(sequence), false);
  return aad;
}

export function concatenate(...parts: Bytes[]): Bytes {
  const result = new Uint8Array(new ArrayBuffer(parts.reduce((size, part) => size + part.length, 0)));
  let offset = 0;
  for (const part of parts) {
    result.set(part, offset);
    offset += part.length;
  }
  return result;
}
