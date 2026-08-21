import { get, writable } from 'svelte/store';
import { base64UrlDecode, base64UrlEncode } from './base64url';
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
  // The store keeps connections warm while hidden and skips the resume probe
  // once one has gone stale, so it needs the visibility truth from every path
  // that can flip it — not just `visibilitychange`, which some engines skip on
  // a bfcache restore or an unfreeze.
  const syncVisibility = () => {
    relayStore.setHidden(document.visibilityState === 'hidden');
  };
  syncVisibility();
  relayStore.initialize(false);
  if (deviceVerificationEnabled()) {
    securityState.update((state) => ({ ...state, locked: true, reason: 'open' }));
    void unlockWithDevice('open');
  } else relayStore.connectAll();

  const networkConnection = (navigator as Navigator & { connection?: EventTarget }).connection;
  const revalidateAfterResume = () => {
    if (document.visibilityState !== 'visible') return;
    // Preserve a healthy direct WebRTC session across sleep. A response keeps
    // it alive without gateway traffic; a stale path gets two seconds before
    // the normal reconnect creates a fresh gateway-assisted direct session.
    relayStore.resetReconnectBackoff();
    relayStore.revalidateConnections(RESUME_HEALTH_TIMEOUT_MS);
  };
  const onVisibility = () => {
    syncVisibility();
    if (document.visibilityState === 'hidden') {
      if (deviceVerificationEnabled()) lockForDevice('resume');
      return;
    }
    if (deviceVerificationEnabled()) {
      unlockAfterResume();
      return;
    }
    revalidateAfterResume();
  };
  const onPageShow = (event: PageTransitionEvent) => {
    syncVisibility();
    if (!event.persisted) return;
    if (deviceVerificationEnabled()) {
      lockForDevice('resume');
      setTimeout(unlockAfterResume, 150);
      return;
    }
    revalidateAfterResume();
  };
  const onFocus = () => {
    syncVisibility();
    if (document.visibilityState !== 'visible') return;
    if (deviceVerificationEnabled()) {
      setTimeout(unlockAfterResume, 150);
      return;
    }
    revalidateAfterResume();
  };
  const onOnline = () => {
    if (document.visibilityState !== 'visible') return;
    if (deviceVerificationEnabled() && get(securityState).locked) {
      unlockAfterResume();
      return;
    }
    revalidateAfterResume();
  };
  const onNetworkChange = () => {
    if (document.visibilityState !== 'visible') return;
    if (deviceVerificationEnabled() && get(securityState).locked) return;
    // Wi-Fi/cellular handoffs often stay "online", so the window event never
    // fires. Probe the current application path before deciding to replace it:
    // a healthy connection answers and stays open; a half-open one gets two
    // seconds before the existing reconnect path takes over.
    relayStore.resetReconnectBackoff();
    relayStore.revalidateConnections(RESUME_HEALTH_TIMEOUT_MS);
  };
  const onFreeze = () => {
    if (deviceVerificationEnabled()) lockForDevice('resume');
  };
  const onResume = () => {
    syncVisibility();
    if (document.visibilityState !== 'visible') return;
    if (deviceVerificationEnabled()) {
      if (get(securityState).locked) setTimeout(unlockAfterResume, 150);
      return;
    }
    revalidateAfterResume();
  };
  document.addEventListener('visibilitychange', onVisibility);
  document.addEventListener('freeze', onFreeze);
  document.addEventListener('resume', onResume);
  window.addEventListener('pageshow', onPageShow);
  window.addEventListener('focus', onFocus);
  window.addEventListener('online', onOnline);
  networkConnection?.addEventListener('change', onNetworkChange);
  return () => {
    document.removeEventListener('visibilitychange', onVisibility);
    document.removeEventListener('freeze', onFreeze);
    document.removeEventListener('resume', onResume);
    window.removeEventListener('pageshow', onPageShow);
    window.removeEventListener('focus', onFocus);
    window.removeEventListener('online', onOnline);
    networkConnection?.removeEventListener('change', onNetworkChange);
  };
}

export function lockForDevice(reason: 'open' | 'resume' = 'resume'): void {
  if (!deviceVerificationEnabled() || get(securityState).locked) return;
  // Locking gates the interface, not the transport. Dropping the connection
  // here used to be free — the app was going idle anyway — but it now costs
  // every verification user the warm-resume path, and it buys no secrecy: the
  // relay key sits in localStorage and the unlock redials with it unchanged,
  // so a live socket grants an attacker nothing a dial would not. Pane traffic
  // stops separately (TerminalView treats a locked app as not visible), so
  // nothing streams behind the unlock dialog.
  automaticUnlockPending = true;
  securityState.update((state) => ({ ...state, locked: true, reason, status: '' }));
}

/**
 * Restores traffic once verification succeeds. A resume unlock finds the
 * connections the lock left in place, so it revalidates: a warm socket is kept
 * and only a dead one is replaced. An unlock at app open has nothing to keep
 * and dials.
 */
function resumeAfterUnlock(reason: 'open' | 'resume'): void {
  if (reason !== 'resume') {
    relayStore.connectAll(false);
    return;
  }
  relayStore.resetReconnectBackoff();
  relayStore.revalidateConnections(RESUME_HEALTH_TIMEOUT_MS);
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
    resumeAfterUnlock(reason);
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
    resumeAfterUnlock(reason);
    return true;
  } catch {
    securityState.update((state) => ({
      ...state,
      locked: true,
      busy: false,
      status: 'Verification failed. Tap Unlock to try again.',
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
