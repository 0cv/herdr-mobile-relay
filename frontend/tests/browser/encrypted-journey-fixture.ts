export interface JourneyCredential {
  kind: 'credential';
  id: string;
  version: number;
  secret: string;
  deviceId: string;
  role: 'reader';
  locale: string;
  issuedAt: number;
}

export interface JourneyPeer {
  receive(frame: string): void;
  send(message: Record<string, unknown>): void;
}

// Serialized by Playwright into the browser. This is a real v2 encrypted peer,
// not a plaintext shortcut that fabricates the client's authenticated role.
export function installEncryptedJourneyPeer() {
  const encoder = new TextEncoder();
  const decoder = new TextDecoder();
  const encode = (bytes: Uint8Array) => btoa(String.fromCharCode(...bytes)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  const decode = (value: string) => Uint8Array.from(atob(value.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - value.length % 4) % 4)), (char) => char.charCodeAt(0));
  const concat = (...parts: Uint8Array[]) => {
    const result = new Uint8Array(parts.reduce((length, part) => length + part.length, 0));
    let offset = 0;
    for (const part of parts) { result.set(part, offset); offset += part.length; }
    return result;
  };
  const nonce = (sequence: number) => {
    const result = new Uint8Array(12);
    new DataView(result.buffer).setBigUint64(4, BigInt(sequence), false);
    return result;
  };
  const aad = (direction: string, sequence: number) => {
    const prefix = encoder.encode(`herdr-e2ee-v2 ${direction}`);
    const result = new Uint8Array(prefix.length + 9);
    result.set(prefix);
    new DataView(result.buffer).setBigUint64(prefix.length + 1, BigInt(sequence), false);
    return result;
  };

  Object.assign(window, {
    __createJourneyPeer(credential: JourneyCredential, emit: (frame: string) => void, command: (message: Record<string, unknown>) => void): JourneyPeer {
      let sendKey: CryptoKey | null = null;
      let receiveKey: CryptoKey | null = null;
      let receiveSequence = 0;
      let sendSequence = 0;
      let ready = false;
      let failed = false;
      let inbound = Promise.resolve();
      let outbound = Promise.resolve();
      const pending: Record<string, unknown>[] = [];
      const fail = (error: unknown) => { failed = true; console.error('Encrypted journey peer failed', error); };
      const send = (message: Record<string, unknown>) => {
        outbound = outbound.then(async () => {
          if (failed || !sendKey) return;
          const sequence = sendSequence++;
          const ciphertext = new Uint8Array(await crypto.subtle.encrypt(
            { name: 'AES-GCM', iv: nonce(sequence), additionalData: aad('s2c', sequence), tagLength: 128 },
            sendKey, encoder.encode(JSON.stringify(message)),
          ));
          emit(JSON.stringify({ type: 'e2ee', version: 2, sequence, ciphertext: encode(ciphertext) }));
        }).catch(fail);
      };
      return {
        receive(frame) {
          inbound = inbound.then(async () => {
            if (failed) return;
            const message = JSON.parse(frame) as Record<string, unknown>;
            if (!receiveKey) {
              if (message.type !== 'e2ee_client_hello' || message.version !== 2 || message.auth_kind !== 'credential'
                || message.auth_id !== credential.id || message.auth_version !== credential.version) throw new Error('Unexpected credential hello');
              const authKey = await crypto.subtle.importKey('raw', decode(credential.secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
              const pair = await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, false, ['deriveBits']);
              const serverNonce = crypto.getRandomValues(new Uint8Array(32));
              const serverPublic = new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey));
              const clientPublic = decode(String(message.public_key));
              const transcript = concat(encoder.encode(`herdr-e2ee-v2 auth\0credential\0${credential.id}\0${credential.version}\0`), decode(String(message.nonce)), clientPublic, serverNonce, serverPublic);
              const tag = async (label: string) => new Uint8Array(await crypto.subtle.sign('HMAC', authKey, concat(encoder.encode(label), transcript)));
              const proof = await tag('herdr-e2ee-v2 server\0');
              const salt = await tag('herdr-e2ee-v2 key\0');
              const publicKey = await crypto.subtle.importKey('raw', clientPublic, { name: 'ECDH', namedCurve: 'P-256' }, false, []);
              const shared = await crypto.subtle.deriveBits({ name: 'ECDH', public: publicKey }, pair.privateKey, 256);
              const material = await crypto.subtle.importKey('raw', shared, 'HKDF', false, ['deriveKey']);
              const derive = (info: string, usage: KeyUsage) => crypto.subtle.deriveKey(
                { name: 'HKDF', hash: 'SHA-256', salt, info: encoder.encode(info) }, material, { name: 'AES-GCM', length: 256 }, false, [usage],
              );
              sendKey = await derive('herdr-e2ee-v2 s2c', 'encrypt');
              receiveKey = await derive('herdr-e2ee-v2 c2s', 'decrypt');
              emit(JSON.stringify({ type: 'e2ee_server_hello', version: 2, nonce: encode(serverNonce), public_key: encode(serverPublic), proof: encode(proof) }));
              return;
            }
            if (message.type !== 'e2ee' || message.version !== 2 || message.sequence !== receiveSequence) throw new Error('Unexpected encrypted frame');
            const sequence = receiveSequence++;
            const application = JSON.parse(decoder.decode(await crypto.subtle.decrypt(
              { name: 'AES-GCM', iv: nonce(sequence), additionalData: aad('c2s', sequence), tagLength: 128 }, receiveKey, decode(String(message.ciphertext)),
            ))) as Record<string, unknown>;
            if (!ready) {
              if (application.type !== 'e2ee_client_finish') throw new Error('Unexpected client finish');
              send({ type: 'e2ee_server_finish', version: 2, device_id: credential.deviceId, credential_id: credential.id, credential_version: credential.version, role: credential.role, locale: credential.locale });
              ready = true;
              for (const queued of pending.splice(0)) send(queued);
              return;
            }
            command(application);
          }).catch(fail);
        },
        send(message) {
          if (ready) send(message);
          else pending.push(message);
        },
      };
    },
  });
}
