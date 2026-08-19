import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { createE2EEClientHandshake, E2EESession } from '$lib/e2ee';

const encoder = new TextEncoder();
const decoder = new TextDecoder();
type Bytes = Uint8Array<ArrayBuffer>;

interface VectorPeer {
  private_key: string;
  nonce: string;
  hello: {
    type: 'e2ee_client_hello' | 'e2ee_server_hello';
    version: 1;
    nonce: string;
    public_key: string;
    proof: string;
  };
}

interface E2EEVector {
  relay_key: string;
  client: VectorPeer;
  server: VectorPeer;
  records: {
    client_finish: { plaintext: string; frame: Record<string, unknown> };
    c2s: { plaintext: string; frame: Record<string, unknown> };
    s2c: { plaintext: string; frame: Record<string, unknown> };
  };
}

const vector = JSON.parse(
  readFileSync(resolve(process.cwd(), '../contracts/fixtures/e2ee/v1.json'), 'utf8'),
) as E2EEVector;

describe('relay end-to-end encryption', () => {
  it('encrypts both directions and rejects replayed frames', async () => {
    const clientKeyBytes = crypto.getRandomValues(new Uint8Array(new ArrayBuffer(32)));
    const serverKeyBytes = crypto.getRandomValues(new Uint8Array(new ArrayBuffer(32)));
    const clientKey = await crypto.subtle.importKey(
      'raw', clientKeyBytes, 'AES-GCM', false, ['encrypt', 'decrypt'],
    );
    const serverKey = await crypto.subtle.importKey(
      'raw', serverKeyBytes, 'AES-GCM', false, ['encrypt', 'decrypt'],
    );
    const session = new E2EESession(clientKey, serverKey);

    const plaintext = JSON.stringify({ type: 'submit_prompt', text: 'private prompt' });
    const encrypted = await session.encrypt(plaintext);
    expect(encrypted).not.toContain('submit_prompt');
    expect(encrypted).not.toContain('private prompt');
    const clientFrame = JSON.parse(String(encrypted)) as Record<string, unknown>;
    const decryptedClient = await crypto.subtle.decrypt({
      name: 'AES-GCM',
      iv: frameNonce(0),
      additionalData: frameAAD('c2s', 0),
      tagLength: 128,
    }, clientKey, base64UrlDecode(String(clientFrame.ciphertext)));
    expect(decoder.decode(decryptedClient)).toBe(plaintext);

    const reply = JSON.stringify({ type: 'pane_content', content: 'private terminal output' });
    const encryptedReply = new Uint8Array(await crypto.subtle.encrypt({
      name: 'AES-GCM',
      iv: frameNonce(0),
      additionalData: frameAAD('s2c', 0),
      tagLength: 128,
    }, serverKey, encoder.encode(reply)));
    const serverFrame = JSON.stringify({
      type: 'e2ee',
      version: 1,
      sequence: 0,
      ciphertext: base64UrlEncode(encryptedReply),
    });
    await expect(session.decrypt(serverFrame)).resolves.toBe(reply);
    await expect(session.decrypt(serverFrame)).rejects.toThrow(/sequence/);
  });

  it('rejects relay keys shorter than 16 UTF-8 bytes', async () => {
    await expect(createE2EEClientHandshake('predictable')).rejects.toThrow(/16 bytes/);
  });

  it('keeps the relay key out of the hello and authenticates the server proof', async () => {
    const token = '0123456789abcdef0123456789abcdef';
    const handshake = await createE2EEClientHandshake(token);
    expect(JSON.stringify(handshake.hello)).not.toContain(token);
    expect(handshake.hello).toMatchObject({ type: 'e2ee_client_hello', version: 1 });

    await expect(handshake.complete({
      type: 'e2ee_server_hello',
      version: 1,
      nonce: base64UrlEncode(new Uint8Array(new ArrayBuffer(32))),
      public_key: base64UrlEncode(new Uint8Array(new ArrayBuffer(65))),
      proof: base64UrlEncode(new Uint8Array(new ArrayBuffer(32))),
    })).rejects.toThrow(/verification failed/);
  });

  it('matches the shared deterministic version one vector', async () => {
    const keyPair = await importVectorKeyPair(vector.client);
    const handshake = await createE2EEClientHandshake(vector.relay_key, {
      keyPair,
      nonce: base64UrlDecode(vector.client.nonce),
    });
    expect(handshake.hello).toEqual(vector.client.hello);

    const completed = await handshake.complete(vector.server.hello);
    expect(JSON.parse(String(completed.finish))).toEqual(vector.records.client_finish.frame);
    const { session } = completed;
    const frame = await session.encrypt(vector.records.c2s.plaintext);
    expect(JSON.parse(String(frame))).toEqual(vector.records.c2s.frame);
    await expect(session.decrypt(JSON.stringify(vector.records.s2c.frame)))
      .resolves.toBe(vector.records.s2c.plaintext);
  });

  it('rejects a malformed server-hello mutation corpus', async () => {
    const valid = vector.server.hello;
    const mutations: unknown[] = [
      null,
      [],
      {},
      { ...valid, type: 'e2ee_client_hello' },
      { ...valid, version: 0 },
      { ...valid, version: '1' },
      { ...valid, nonce: '' },
      { ...valid, public_key: '' },
      { ...valid, proof: '' },
    ];
    for (const field of ['type', 'version', 'nonce', 'public_key', 'proof'] as const) {
      const missing = { ...valid } as Record<string, unknown>;
      delete missing[field];
      mutations.push(missing);
    }
    for (const field of ['nonce', 'public_key', 'proof'] as const) {
      const encoded = valid[field];
      const replacement = encoded[0] === 'A' ? 'B' : 'A';
      mutations.push(
        { ...valid, [field]: `${replacement}${encoded.slice(1)}` },
        { ...valid, [field]: encoded.slice(0, -1) },
        { ...valid, [field]: `${encoded}=` },
        { ...valid, [field]: `${encoded}%` },
      );
    }

    const keyPair = await importVectorKeyPair(vector.client);
    for (const serverHello of mutations) {
      const handshake = await createE2EEClientHandshake(vector.relay_key, {
        keyPair,
        nonce: base64UrlDecode(vector.client.nonce),
      });
      await expect(handshake.complete(serverHello)).rejects.toThrow();
    }
  });
});

function frameNonce(sequence: number): Bytes {
  const nonce = new Uint8Array(new ArrayBuffer(12));
  new DataView(nonce.buffer).setBigUint64(4, BigInt(sequence), false);
  return nonce;
}

function frameAAD(direction: 'c2s' | 's2c', sequence: number): Bytes {
  const prefix = encoder.encode(`herdr-e2ee-v1 ${direction}`);
  const aad = new Uint8Array(new ArrayBuffer(prefix.length + 9));
  aad.set(prefix);
  new DataView(aad.buffer).setBigUint64(prefix.length + 1, BigInt(sequence), false);
  return aad;
}

function base64UrlEncode(value: Bytes): string {
  return btoa(String.fromCharCode(...value)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '');
}

function base64UrlDecode(value: string): Bytes {
  const padded = value.replace(/-/g, '+').replace(/_/g, '/').padEnd(Math.ceil(value.length / 4) * 4, '=');
  const binary = atob(padded);
  const result = new Uint8Array(new ArrayBuffer(binary.length));
  for (let index = 0; index < binary.length; index += 1) result[index] = binary.charCodeAt(index);
  return result;
}

async function importVectorKeyPair(peer: VectorPeer): Promise<CryptoKeyPair> {
  const rawPublic = base64UrlDecode(peer.hello.public_key);
  const publicJWK: JsonWebKey = {
    kty: 'EC',
    crv: 'P-256',
    x: base64UrlEncode(rawPublic.slice(1, 33)),
    y: base64UrlEncode(rawPublic.slice(33, 65)),
    ext: true,
  };
  const [privateKey, publicKey] = await Promise.all([
    crypto.subtle.importKey(
      'jwk',
      { ...publicJWK, d: peer.private_key },
      { name: 'ECDH', namedCurve: 'P-256' },
      true,
      ['deriveBits'],
    ),
    crypto.subtle.importKey(
      'jwk',
      publicJWK,
      { name: 'ECDH', namedCurve: 'P-256' },
      true,
      [],
    ),
  ]);
  return { privateKey, publicKey };
}
