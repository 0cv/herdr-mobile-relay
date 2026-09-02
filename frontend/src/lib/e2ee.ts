import { base64UrlDecode, base64UrlEncode, type Base64UrlBytes } from './base64url';
import type {
  DeviceEnrollmentResult,
  RelayDeviceAuthentication,
} from './device-auth';

export const E2EE_SUBPROTOCOL = 'herdr-e2ee-v2';

const E2EE_VERSION = 2;
const NONCE_BYTES = 32;
const PUBLIC_KEY_BYTES = 65;
const AUTH_SECRET_BYTES = 32;
const BINARY_FRAME_VERSION = 2;
const BINARY_FRAME_KIND_DATA = 0;
const BINARY_FRAME_HEADER_BYTES = 10;
const encoder = new TextEncoder();
const decoder = new TextDecoder('utf-8', { fatal: true });
const clientProofLabel = encoder.encode('herdr-e2ee-v2 client\0');
const serverProofLabel = encoder.encode('herdr-e2ee-v2 server\0');
const keySaltLabel = encoder.encode('herdr-e2ee-v2 key\0');
const clientKeyInfo = encoder.encode('herdr-e2ee-v2 c2s');
const serverKeyInfo = encoder.encode('herdr-e2ee-v2 s2c');

/**
 * Encrypted-frame encoding. `json` is the original text envelope spoken by the
 * browser WebSocket path. `binary` prefixes raw ciphertext with a fixed header
 * and is used by the gateway and WebRTC paths, where dropping base64 (+33 %)
 * and the JSON envelope directly reduces relayed bandwidth.
 */
export type E2EECodec = 'json' | 'binary';

/** One encrypted frame as handed to a transport. */
export type E2EEWireFrame = string | Uint8Array<ArrayBuffer>;

type Bytes = Base64UrlBytes;

export interface E2EEClientHello {
  type: 'e2ee_client_hello';
  version: 2;
  auth_kind: 'credential' | 'invitation';
  auth_id: string;
  auth_version: number;
  locale: string;
  nonce: string;
  public_key: string;
  proof: string;
}

export interface E2EEClientHandshake {
  hello: E2EEClientHello;
  complete(serverHello: unknown): Promise<E2EEClientHandshakeChallenge>;
}

export interface E2EEClientHandshakeChallenge {
  finish: E2EEWireFrame;
  complete(serverFinish: E2EEWireFrame): Promise<E2EEClientHandshakeResult>;
}

export interface E2EEClientHandshakeResult {
  session: E2EESession;
  enrollment: DeviceEnrollmentResult;
}

export interface E2EEClientEphemeral {
  keyPair: CryptoKeyPair;
  nonce: Uint8Array<ArrayBuffer>;
}

export class E2EESession {
  private sendSequence = 0;
  private receiveSequence = 0;
  private sendQueue: Promise<void> = Promise.resolve();
  private receiveQueue: Promise<void> = Promise.resolve();

  constructor(
    private readonly sendKey: CryptoKey,
    private readonly receiveKey: CryptoKey,
    readonly codec: E2EECodec = 'json',
  ) {}

  encrypt(plaintext: string): Promise<E2EEWireFrame> {
    const operation = this.sendQueue.then(() => this.encryptNext(plaintext));
    this.sendQueue = operation.then(() => undefined, () => undefined);
    return operation;
  }

  decrypt(rawFrame: E2EEWireFrame): Promise<string> {
    const operation = this.receiveQueue.then(() => this.decryptNext(rawFrame));
    this.receiveQueue = operation.then(() => undefined, () => undefined);
    return operation;
  }

  private async encryptNext(plaintext: string): Promise<E2EEWireFrame> {
    if (!Number.isSafeInteger(this.sendSequence)) throw new Error('Encrypted send sequence exhausted.');
    const sequence = this.sendSequence;
    const ciphertext = new Uint8Array(await crypto.subtle.encrypt({
      name: 'AES-GCM',
      iv: frameNonce(sequence),
      additionalData: frameAAD('c2s', sequence),
      tagLength: 128,
    }, this.sendKey, encoder.encode(plaintext)));
    this.sendSequence += 1;
    if (this.codec === 'binary') return encodeBinaryFrame(sequence, ciphertext);
    return JSON.stringify({
      type: 'e2ee',
      version: E2EE_VERSION,
      sequence,
      ciphertext: base64UrlEncode(ciphertext),
    });
  }

  private async decryptNext(rawFrame: E2EEWireFrame): Promise<string> {
    const { sequence, ciphertext } = this.codec === 'binary'
      ? decodeBinaryFrame(rawFrame)
      : decodeJsonFrame(rawFrame);
    if (sequence !== this.receiveSequence) {
      throw new Error('Relay sent an invalid encrypted frame sequence.');
    }
    const plaintext = await crypto.subtle.decrypt({
      name: 'AES-GCM',
      iv: frameNonce(sequence),
      additionalData: frameAAD('s2c', sequence),
      tagLength: 128,
    }, this.receiveKey, ciphertext);
    this.receiveSequence += 1;
    return decoder.decode(plaintext);
  }
}

interface ParsedFrame {
  sequence: number;
  ciphertext: Bytes;
}

function encodeBinaryFrame(sequence: number, ciphertext: Bytes): Bytes {
  const frame = new Uint8Array(new ArrayBuffer(BINARY_FRAME_HEADER_BYTES + ciphertext.length));
  frame[0] = BINARY_FRAME_VERSION;
  frame[1] = BINARY_FRAME_KIND_DATA;
  new DataView(frame.buffer).setBigUint64(2, BigInt(sequence), false);
  frame.set(ciphertext, BINARY_FRAME_HEADER_BYTES);
  return frame;
}

function decodeBinaryFrame(rawFrame: E2EEWireFrame): ParsedFrame {
  if (typeof rawFrame === 'string') throw new Error('Relay sent a text frame on a binary transport.');
  if (rawFrame.length < BINARY_FRAME_HEADER_BYTES) throw new Error('Relay sent a truncated encrypted frame.');
  if (rawFrame[0] !== BINARY_FRAME_VERSION || rawFrame[1] !== BINARY_FRAME_KIND_DATA) {
    throw new Error('Relay sent an unsupported encrypted frame.');
  }
  const sequence = Number(new DataView(rawFrame.buffer, rawFrame.byteOffset).getBigUint64(2, false));
  if (!Number.isSafeInteger(sequence) || sequence < 0) {
    throw new Error('Relay sent an invalid encrypted frame sequence.');
  }
  return { sequence, ciphertext: rawFrame.slice(BINARY_FRAME_HEADER_BYTES) as Bytes };
}

function decodeJsonFrame(rawFrame: E2EEWireFrame): ParsedFrame {
  if (typeof rawFrame !== 'string') throw new Error('Relay sent a binary frame on a text transport.');
  const frame = asRecord(JSON.parse(rawFrame));
  if (frame.type !== 'e2ee' || frame.version !== E2EE_VERSION) {
    throw new Error('Relay sent an unsupported encrypted frame.');
  }
  const sequence = Number(frame.sequence);
  if (!Number.isSafeInteger(sequence) || sequence < 0) {
    throw new Error('Relay sent an invalid encrypted frame sequence.');
  }
  return { sequence, ciphertext: base64UrlDecode(String(frame.ciphertext || '')) };
}

export async function createE2EEClientHandshake(
  authentication: RelayDeviceAuthentication,
  ephemeral?: E2EEClientEphemeral,
  codec: E2EECodec = 'json',
): Promise<E2EEClientHandshake> {
  if (!crypto.subtle) throw new Error('Encrypted relay connections require Web Crypto support.');
  const authId = validateAuthenticationId(authentication.id);
  const authVersion = validateAuthenticationVersion(authentication.version);
  const authSecret = base64UrlDecode(authentication.secret);
  if (authSecret.byteLength !== AUTH_SECRET_BYTES) {
    throw new Error('Device authentication secrets must be 32 bytes.');
  }
  const locale = validatedDeviceLocale(navigator.language);
  const authBinding = authenticationBinding(authentication.kind, authId, authVersion);
  const authKey = await crypto.subtle.importKey(
    'raw',
    authSecret,
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign'],
  );
  authSecret.fill(0);
  let keyPair = ephemeral?.keyPair;
  if (!keyPair) {
    keyPair = await crypto.subtle.generateKey(
      { name: 'ECDH', namedCurve: 'P-256' },
      true,
      ['deriveBits'],
    ) as CryptoKeyPair;
  }
  const clientNonce = ephemeral
    ? new Uint8Array(ephemeral.nonce)
    : crypto.getRandomValues(new Uint8Array(new ArrayBuffer(NONCE_BYTES)));
  if (clientNonce.length !== NONCE_BYTES) throw new Error('Encrypted relay handshake nonce must be 32 bytes.');
  const clientPublic = new Uint8Array(await crypto.subtle.exportKey('raw', keyPair.publicKey));
  const clientProof = await authTag(authKey, clientProofLabel, authBinding, clientNonce, clientPublic);
  let helloCompleted = false;

  return {
    hello: {
      type: 'e2ee_client_hello',
      version: E2EE_VERSION,
      auth_kind: authentication.kind,
      auth_id: authId,
      auth_version: authVersion,
      locale,
      nonce: base64UrlEncode(clientNonce),
      public_key: base64UrlEncode(clientPublic),
      proof: base64UrlEncode(clientProof),
    },
    async complete(rawServerHello: unknown): Promise<E2EEClientHandshakeChallenge> {
      if (helloCompleted) throw new Error('Encrypted relay handshake is already complete.');
      const serverHello = asRecord(rawServerHello);
      if (serverHello.type !== 'e2ee_server_hello' || serverHello.version !== E2EE_VERSION) {
        throw new Error('Relay did not provide a supported encrypted handshake.');
      }
      const serverNonce = decodeSizedField(serverHello.nonce, NONCE_BYTES);
      const serverPublic = decodeSizedField(serverHello.public_key, PUBLIC_KEY_BYTES);
      const serverProof = decodeSizedField(serverHello.proof, 32);
      const transcript = concatenate(authBinding, clientNonce, clientPublic, serverNonce, serverPublic);
      const expectedProof = await authTag(authKey, serverProofLabel, transcript);
      if (!constantTimeEqual(serverProof, expectedProof)) {
        throw new Error('Relay device authentication failed.');
      }

      const importedServerPublic = await crypto.subtle.importKey(
        'raw',
        serverPublic,
        { name: 'ECDH', namedCurve: 'P-256' },
        false,
        [],
      );
      const sharedSecret = new Uint8Array(await crypto.subtle.deriveBits(
        { name: 'ECDH', public: importedServerPublic },
        keyPair.privateKey,
        256,
      ));
      const keySalt = await authTag(authKey, keySaltLabel, transcript);
      const keyMaterial = await crypto.subtle.importKey('raw', sharedSecret, 'HKDF', false, ['deriveKey']);
      const [sendKey, receiveKey] = await Promise.all([
        crypto.subtle.deriveKey(
          { name: 'HKDF', hash: 'SHA-256', salt: keySalt, info: clientKeyInfo },
          keyMaterial,
          { name: 'AES-GCM', length: 256 },
          false,
          ['encrypt'],
        ),
        crypto.subtle.deriveKey(
          { name: 'HKDF', hash: 'SHA-256', salt: keySalt, info: serverKeyInfo },
          keyMaterial,
          { name: 'AES-GCM', length: 256 },
          false,
          ['decrypt'],
        ),
      ]);
      sharedSecret.fill(0);
      helloCompleted = true;
      const session = new E2EESession(sendKey, receiveKey, codec);
      const finish = await session.encrypt(JSON.stringify({
        type: 'e2ee_client_finish',
        version: E2EE_VERSION,
      }));
      let finishCompleted = false;
      return {
        finish,
        async complete(rawServerFinish: E2EEWireFrame): Promise<E2EEClientHandshakeResult> {
          if (finishCompleted) throw new Error('Encrypted relay handshake is already complete.');
          const plaintext = await session.decrypt(rawServerFinish);
          const enrollment = parseServerFinish(plaintext, authentication.kind);
          finishCompleted = true;
          return { session, enrollment };
        },
      };
    },
  };
}


function authenticationBinding(
  kind: RelayDeviceAuthentication['kind'],
  id: string,
  version: number,
): Bytes {
  if (kind !== 'credential' && kind !== 'invitation') {
    throw new Error('Unsupported device authentication kind.');
  }
  return encoder.encode(`herdr-e2ee-v2 auth\0${kind}\0${id}\0${version}\0`);
}

function validatedDeviceLocale(value: unknown): string {
  if (typeof value !== 'string' || !/^[A-Za-z0-9-]{1,32}$/u.test(value)) return 'en';
  return value;
}

function validateAuthenticationId(value: unknown): string {
  if (typeof value !== 'string' || !value || value.length > 256 || /[\u0000-\u001f\u007f]/.test(value)) {
    throw new Error('Invalid device authentication id.');
  }
  return value;
}

function validateAuthenticationVersion(value: unknown): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 1) {
    throw new Error('Invalid device authentication version.');
  }
  return value;
}

function parseServerFinish(
  plaintext: string,
  authenticationKind: RelayDeviceAuthentication['kind'],
): DeviceEnrollmentResult {
  const finish = asRecord(JSON.parse(plaintext));
  if (finish.type !== 'e2ee_server_finish' || finish.version !== E2EE_VERSION) {
    throw new Error('Relay did not finish device authentication.');
  }
  const credentialSecret = finish.credential_secret;
  if (authenticationKind === 'invitation' && typeof credentialSecret !== 'string') {
    throw new Error('Relay did not issue a device credential.');
  }
  if (authenticationKind === 'credential' && credentialSecret !== undefined) {
    throw new Error('Relay unexpectedly replaced an enrolled device credential.');
  }
  if (credentialSecret !== undefined) {
    if (typeof credentialSecret !== 'string' || base64UrlDecode(credentialSecret).byteLength !== AUTH_SECRET_BYTES) {
      throw new Error('Relay issued an invalid device credential.');
    }
  }
  const role = finish.role;
  if (role !== 'reader' && role !== 'controller') throw new Error('Relay returned an invalid device role.');
  const locale = String(finish.locale ?? '');
  if (!/^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$/.test(locale) || locale.length > 35) {
    throw new Error('Relay returned an invalid device locale.');
  }
  return {
    deviceId: validateAuthenticationId(finish.device_id),
    credentialId: validateAuthenticationId(finish.credential_id),
    credentialVersion: validateAuthenticationVersion(finish.credential_version),
    ...(credentialSecret === undefined ? {} : { credentialSecret }),
    role,
    locale,
  };
}

async function authTag(key: CryptoKey, ...parts: Bytes[]): Promise<Bytes> {
  const signature = await crypto.subtle.sign('HMAC', key, concatenate(...parts));
  return new Uint8Array(signature);
}

function frameNonce(sequence: number): Bytes {
  const nonce = new Uint8Array(new ArrayBuffer(12));
  new DataView(nonce.buffer).setBigUint64(4, BigInt(sequence), false);
  return nonce;
}

function frameAAD(direction: 'c2s' | 's2c', sequence: number): Bytes {
  const prefix = encoder.encode(`herdr-e2ee-v2 ${direction}`);
  const aad = new Uint8Array(new ArrayBuffer(prefix.length + 1 + 8));
  aad.set(prefix);
  new DataView(aad.buffer).setBigUint64(prefix.length + 1, BigInt(sequence), false);
  return aad;
}


function concatenate(...parts: Bytes[]): Bytes {
  const result = new Uint8Array(new ArrayBuffer(parts.reduce((total, part) => total + part.length, 0)));
  let offset = 0;
  for (const part of parts) {
    result.set(part, offset);
    offset += part.length;
  }
  return result;
}

function decodeSizedField(value: unknown, size: number): Bytes {
  const decoded = base64UrlDecode(String(value || ''));
  if (decoded.length !== size) throw new Error('Relay sent an invalid encrypted handshake field.');
  return decoded;
}


function constantTimeEqual(left: Bytes, right: Bytes): boolean {
  if (left.length !== right.length) return false;
  let difference = 0;
  for (let index = 0; index < left.length; index += 1) difference |= left[index] ^ right[index];
  return difference === 0;
}

function asRecord(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('Relay sent an invalid encrypted message.');
  }
  return value as Record<string, unknown>;
}
