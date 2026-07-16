import { get } from 'svelte/store';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { DEVICE_CREDENTIAL_KEY, DEVICE_LOCK_KEY } from '$lib/config';
import {
  initializeDeviceSecurity,
  securityState,
  unlockWithDevice,
} from '$lib/security';
import { relayStore } from '$lib/store';

describe('device verification lifecycle', () => {
  const credentialsDescriptor = Object.getOwnPropertyDescriptor(navigator, 'credentials');
  const secureContextDescriptor = Object.getOwnPropertyDescriptor(window, 'isSecureContext');
  const visibilityDescriptor = Object.getOwnPropertyDescriptor(document, 'visibilityState');
  const publicKeyCredentialDescriptor = Object.getOwnPropertyDescriptor(window, 'PublicKeyCredential');
  let getCredential: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.useFakeTimers();
    getCredential = vi.fn();
    Object.defineProperty(navigator, 'credentials', {
      configurable: true,
      value: { get: getCredential },
    });
    Object.defineProperty(window, 'isSecureContext', { configurable: true, value: true });
    Object.defineProperty(window, 'PublicKeyCredential', { configurable: true, value: class {} });
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    vi.spyOn(relayStore, 'initialize').mockImplementation(() => {});
    vi.spyOn(relayStore, 'connectAll').mockImplementation(() => {});
    vi.spyOn(relayStore, 'destroy').mockImplementation(() => {});
    vi.spyOn(relayStore, 'revalidateConnections').mockImplementation(() => {});
    localStorage.setItem(DEVICE_LOCK_KEY, 'true');
    localStorage.setItem(DEVICE_CREDENTIAL_KEY, 'AQID');
    securityState.set({
      locked: false,
      busy: false,
      reason: 'open',
      status: '',
      hint: 'Device verification is enabled.',
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
    restoreProperty(navigator, 'credentials', credentialsDescriptor);
    restoreProperty(window, 'isSecureContext', secureContextDescriptor);
    restoreProperty(window, 'PublicKeyCredential', publicKeyCredentialDescriptor);
    restoreProperty(document, 'visibilityState', visibilityDescriptor);
  });

  it('does not verify again when the authenticator returns focus to an unlocked app', async () => {
    await expect(unlockWithDevice('resume')).resolves.toBe(true);
    expect(getCredential).not.toHaveBeenCalled();
  });

  it('does not queue another prompt when focus returns from a failed verification', async () => {
    let rejectVerification: (reason?: unknown) => void = () => {};
    getCredential.mockReturnValue(new Promise((_, reject) => {
      rejectVerification = reject;
    }));

    const stopSecurity = initializeDeviceSecurity();
    expect(getCredential).toHaveBeenCalledOnce();
    window.dispatchEvent(new Event('focus'));
    rejectVerification(new Error('cancelled'));
    await vi.advanceTimersByTimeAsync(200);

    expect(getCredential).toHaveBeenCalledOnce();
    expect(get(securityState)).toMatchObject({
      locked: true,
      busy: false,
      status: 'Verification was cancelled or failed. Tap Unlock to try again.',
    });
    stopSecurity();
  });

  it('preserves the current agent snapshot after successful resume verification', async () => {
    getCredential.mockResolvedValue({});
    securityState.update((state) => ({ ...state, locked: true, reason: 'resume' }));

    await expect(unlockWithDevice('resume')).resolves.toBe(true);

    expect(relayStore.connectAll).toHaveBeenCalledWith(true);
  });

  it('revalidates relay connections on foreground and online events without device verification', () => {
    localStorage.removeItem(DEVICE_LOCK_KEY);
    localStorage.removeItem(DEVICE_CREDENTIAL_KEY);
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    const stopSecurity = initializeDeviceSecurity();

    document.dispatchEvent(new Event('visibilitychange'));
    expect(relayStore.revalidateConnections).not.toHaveBeenCalled();
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));
    window.dispatchEvent(new Event('online'));

    expect(relayStore.connectAll).toHaveBeenCalledOnce();
    expect(relayStore.revalidateConnections).toHaveBeenCalledTimes(2);
    stopSecurity();
  });
});

function restoreProperty(
  target: object,
  property: string,
  descriptor: PropertyDescriptor | undefined,
): void {
  if (descriptor) Object.defineProperty(target, property, descriptor);
  else Reflect.deleteProperty(target, property);
}
