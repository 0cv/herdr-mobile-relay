import { base64UrlEncode } from './base64url';

/**
 * Browser mirror of `internal/gatewaywire`. Both identifiers are derived
 * one-way from the relay key, so the QR payload is unchanged and the gateway
 * — which only ever sees the relay id and a challenge answer — learns nothing
 * that lets it impersonate either side or read a frame.
 *
 *   relay_id       = base64url(HKDF-SHA256(relay_key, salt, "herdr-gw-id", 16))
 *   rendezvous_key = HKDF-SHA256(relay_key, salt, "herdr-gw-auth", 32)
 *   proof          = HMAC(rendezvous_key, "herdr-gw-connect\0" || relay_id || nonce)
 *
 * These values must stay byte-identical to the Go helpers; `tests/unit/
 * transports.test.ts` pins a vector that both sides can check.
 */

const encoder = new TextEncoder();
const HKDF_SALT = encoder.encode('herdr-gateway-v1');
const RELAY_ID_INFO = encoder.encode('herdr-gw-id');
const RENDEZVOUS_INFO = encoder.encode('herdr-gw-auth');
const CONNECT_PROOF_TAG = encoder.encode('herdr-gw-connect\0');

/** Raw length of a derived relay id before base64url encoding. */
export const RELAY_ID_BYTES = 16;
/** Encoded relay id length, used for cheap validation. */
export const RELAY_ID_LENGTH = 22;
/** Length of the rendezvous key derived from the relay key. */
export const RENDEZVOUS_KEY_BYTES = 32;
/** Length of the gateway challenge. */
export const GATEWAY_NONCE_BYTES = 32;

async function deriveBytes(token: string, info: Uint8Array<ArrayBuffer>, length: number): Promise<Uint8Array<ArrayBuffer>> {
  if (!token) throw new Error('Gateway connections require a relay key.');
  if (!crypto.subtle) throw new Error('Gateway connections require Web Crypto support.');
  const material = await crypto.subtle.importKey('raw', encoder.encode(token), 'HKDF', false, ['deriveBits']);
  const bits = await crypto.subtle.deriveBits(
    { name: 'HKDF', hash: 'SHA-256', salt: HKDF_SALT, info },
    material,
    length * 8,
  );
  return new Uint8Array(bits);
}

/** Returns the public rendezvous identifier the gateway routes on. */
export async function deriveRelayId(token: string): Promise<string> {
  return base64UrlEncode(await deriveBytes(token, RELAY_ID_INFO, RELAY_ID_BYTES));
}

/**
 * Answers a gateway challenge. The relay id is bound into the tag so a proof
 * captured for one relay cannot be replayed against another. Only the relay
 * can verify the result; the gateway forwards it untouched.
 */
export async function connectProof(token: string, relayId: string, nonce: Uint8Array): Promise<Uint8Array<ArrayBuffer>> {
  if (nonce.length !== GATEWAY_NONCE_BYTES) throw new Error('Gateway sent an invalid challenge.');
  const keyBytes = await deriveBytes(token, RENDEZVOUS_INFO, RENDEZVOUS_KEY_BYTES);
  const key = await crypto.subtle.importKey('raw', keyBytes, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
  const relayIdBytes = encoder.encode(relayId);
  const message = new Uint8Array(new ArrayBuffer(CONNECT_PROOF_TAG.length + relayIdBytes.length + nonce.length));
  message.set(CONNECT_PROOF_TAG, 0);
  message.set(relayIdBytes, CONNECT_PROOF_TAG.length);
  message.set(nonce, CONNECT_PROOF_TAG.length + relayIdBytes.length);
  return new Uint8Array(await crypto.subtle.sign('HMAC', key, message));
}
