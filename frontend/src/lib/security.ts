import { get, writable } from 'svelte/store';
import { DEVICE_CREDENTIAL_KEY, DEVICE_LOCK_KEY } from './config';
import { relayStore } from './store';

export interface SecurityState {
  locked: boolean;
  busy: boolean;
  reason: 'open' | 'resume';
  status: string;
  hint: string;
}

export const securityState = writable<SecurityState>({
  locked: false,
  busy: false,
  reason: 'open',
  status: '',
  hint: "Uses this browser's platform authenticator. Requires HTTPS.",
});

let unlockInProgress = false;
let automaticUnlockPending = false;
const FORCE_RECONNECT_AFTER_BACKGROUND_MS = 3_000;
const RESUME_RECONNECT_DEDUP_MS = 1_000;
const RESUME_HEALTH_TIMEOUT_MS = 2_000;

export function deviceVerificationSupported(): boolean {
  return Boolean(window.PublicKeyCredential && navigator.credentials && window.isSecureContext);
}

export function deviceVerificationEnabled(): boolean {
  return localStorage.getItem(DEVICE_LOCK_KEY) === 'true'
    && Boolean(localStorage.getItem(DEVICE_CREDENTIAL_KEY));
}

export function initializeDeviceSecurity(): () => void {
  automaticUnlockPending = false;
  relayStore.initialize(false);
  if (deviceVerificationEnabled()) {
    securityState.update((state) => ({ ...state, locked: true, reason: 'open' }));
    void unlockWithDevice('open');
  } else relayStore.connectAll();

  let backgroundedAt: number | null = null;
  let lastForcedReconnectAt: number | null = null;
  const markBackgrounded = () => {
    if (backgroundedAt === null) backgroundedAt = Date.now();
  };
  const reconnectAfterBackground = (force = false) => {
    if (document.visibilityState !== 'visible') return;
    const now = Date.now();
    const backgroundDuration = backgroundedAt === null ? 0 : Math.max(0, now - backgroundedAt);
    backgroundedAt = null;
    if (!force && backgroundDuration < FORCE_RECONNECT_AFTER_BACKGROUND_MS) {
      relayStore.revalidateConnections(RESUME_HEALTH_TIMEOUT_MS);
      return;
    }
    if (lastForcedReconnectAt !== null && now - lastForcedReconnectAt < RESUME_RECONNECT_DEDUP_MS) return;
    lastForcedReconnectAt = now;
    relayStore.connectAll(true);
  };
  const onVisibility = () => {
    if (document.visibilityState === 'hidden') {
      markBackgrounded();
      if (deviceVerificationEnabled()) lockForDevice('resume');
      return;
    }
    if (deviceVerificationEnabled()) {
      backgroundedAt = null;
      unlockAfterResume();
      return;
    }
    reconnectAfterBackground();
  };
  const onPageShow = (event: PageTransitionEvent) => {
    if (!event.persisted) return;
    if (deviceVerificationEnabled()) {
      lockForDevice('resume');
      setTimeout(unlockAfterResume, 150);
      return;
    }
    reconnectAfterBackground(true);
  };
  const onFocus = () => {
    if (document.visibilityState !== 'visible') return;
    if (deviceVerificationEnabled()) {
      setTimeout(unlockAfterResume, 150);
      return;
    }
    reconnectAfterBackground();
  };
  const onOnline = () => {
    if (document.visibilityState !== 'visible') return;
    if (deviceVerificationEnabled() && get(securityState).locked) {
      unlockAfterResume();
      return;
    }
    reconnectAfterBackground(true);
  };
  const onFreeze = () => {
    markBackgrounded();
    if (deviceVerificationEnabled()) lockForDevice('resume');
  };
  const onResume = () => {
    if (document.visibilityState !== 'visible') return;
    if (deviceVerificationEnabled()) {
      backgroundedAt = null;
      if (get(securityState).locked) setTimeout(unlockAfterResume, 150);
      return;
    }
    reconnectAfterBackground(true);
  };
  document.addEventListener('visibilitychange', onVisibility);
  document.addEventListener('freeze', onFreeze);
  document.addEventListener('resume', onResume);
  window.addEventListener('pageshow', onPageShow);
  window.addEventListener('focus', onFocus);
  window.addEventListener('online', onOnline);
  return () => {
    document.removeEventListener('visibilitychange', onVisibility);
    document.removeEventListener('freeze', onFreeze);
    document.removeEventListener('resume', onResume);
    window.removeEventListener('pageshow', onPageShow);
    window.removeEventListener('focus', onFocus);
    window.removeEventListener('online', onOnline);
  };
}

export function lockForDevice(reason: 'open' | 'resume' = 'resume'): void {
  if (!deviceVerificationEnabled() || get(securityState).locked) return;
  relayStore.destroy();
  automaticUnlockPending = true;
  securityState.update((state) => ({ ...state, locked: true, reason, status: '' }));
}

function unlockAfterResume(): void {
  if (!automaticUnlockPending || document.visibilityState !== 'visible') return;
  if (!deviceVerificationEnabled() || !get(securityState).locked) {
    automaticUnlockPending = false;
    return;
  }
  automaticUnlockPending = false;
  void unlockWithDevice('resume');
}

export async function setDeviceVerificationRequired(required: boolean): Promise<boolean> {
  if (!required) {
    automaticUnlockPending = false;
    localStorage.removeItem(DEVICE_LOCK_KEY);
    localStorage.removeItem(DEVICE_CREDENTIAL_KEY);
    securityState.set({
      locked: false,
      busy: false,
      reason: 'open',
      status: '',
      hint: "Uses this browser's platform authenticator. Requires HTTPS.",
    });
    return true;
  }
  return enrollDeviceVerification();
}

export async function enrollDeviceVerification(): Promise<boolean> {
  if (!deviceVerificationSupported()) {
    securityState.update((state) => ({ ...state, hint: 'Device verification needs HTTPS and WebAuthn support.' }));
    return false;
  }
  securityState.update((state) => ({ ...state, busy: true, hint: 'Creating a device verification credential...' }));
  try {
    const credential = await navigator.credentials.create({
      publicKey: {
        challenge: randomBytes(32),
        rp: { name: 'Herdr Mobile Relay' },
        user: { id: randomBytes(16), name: 'local-device', displayName: 'This device' },
        pubKeyCredParams: [{ type: 'public-key', alg: -7 }, { type: 'public-key', alg: -257 }],
        authenticatorSelection: { authenticatorAttachment: 'platform', userVerification: 'required' },
        timeout: 60_000,
        attestation: 'none',
      },
    }) as PublicKeyCredential | null;
    if (!credential?.rawId) throw new Error('No credential returned');
    localStorage.setItem(DEVICE_CREDENTIAL_KEY, base64UrlEncode(credential.rawId));
    localStorage.setItem(DEVICE_LOCK_KEY, 'true');
    securityState.update((state) => ({ ...state, busy: false, hint: 'Device verification is enabled.' }));
    return true;
  } catch {
    localStorage.removeItem(DEVICE_LOCK_KEY);
    localStorage.removeItem(DEVICE_CREDENTIAL_KEY);
    securityState.update((state) => ({ ...state, busy: false, hint: 'Device verification was cancelled or failed.' }));
    return false;
  }
}

export async function unlockWithDevice(reason: 'open' | 'resume' = 'open'): Promise<boolean> {
  if (!deviceVerificationEnabled()) {
    automaticUnlockPending = false;
    securityState.update((state) => ({ ...state, locked: false, busy: false, status: '' }));
    relayStore.connectAll(reason === 'resume');
    return true;
  }
  if (!get(securityState).locked) {
    automaticUnlockPending = false;
    return true;
  }
  automaticUnlockPending = false;
  if (!deviceVerificationSupported()) {
    securityState.update((state) => ({
      ...state,
      locked: true,
      reason,
      status: 'Device verification needs HTTPS and WebAuthn support.',
    }));
    return false;
  }
  if (unlockInProgress) return false;
  const credentialId = localStorage.getItem(DEVICE_CREDENTIAL_KEY);
  if (!credentialId) {
    securityState.update((state) => ({ ...state, locked: true, reason, status: 'No device credential is enrolled.' }));
    return false;
  }
  unlockInProgress = true;
  securityState.update((state) => ({ ...state, locked: true, busy: true, reason, status: 'Waiting for device verification...' }));
  try {
    const assertion = await navigator.credentials.get({
      publicKey: {
        challenge: randomBytes(32),
        allowCredentials: [{ type: 'public-key', id: base64UrlDecode(credentialId) }],
        userVerification: 'required',
        timeout: 60_000,
      },
    });
    if (!assertion) throw new Error('No assertion returned');
    securityState.update((state) => ({ ...state, locked: false, busy: false, status: '' }));
    relayStore.connectAll(reason === 'resume');
    return true;
  } catch {
    securityState.update((state) => ({
      ...state,
      locked: true,
      busy: false,
      status: 'Verification was cancelled or failed. Tap Unlock to try again.',
    }));
    return false;
  } finally {
    unlockInProgress = false;
  }
}

function randomBytes(length: number): Uint8Array<ArrayBuffer> {
  const bytes = new Uint8Array(length);
  crypto.getRandomValues(bytes);
  return bytes;
}

function base64UrlEncode(buffer: ArrayBuffer): string {
  let binary = '';
  for (const byte of new Uint8Array(buffer)) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/g, '');
}

function base64UrlDecode(value: string): ArrayBuffer {
  const padded = value.replace(/-/g, '+').replace(/_/g, '/').padEnd(Math.ceil(value.length / 4) * 4, '=');
  const binary = atob(padded);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0)).buffer;
}
