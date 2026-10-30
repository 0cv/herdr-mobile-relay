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
  const connectionDescriptor = Object.getOwnPropertyDescriptor(navigator, 'connection');
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
    vi.spyOn(relayStore, 'resetReconnectBackoff').mockImplementation(() => {});
    vi.spyOn(relayStore, 'setHidden').mockImplementation(() => {});
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
    restoreProperty(navigator, 'connection', connectionDescriptor);
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
      status: 'Verification failed. Tap Unlock to try again.',
    });
    stopSecurity();
  });

  it('keeps the existing session across a lock and revalidates it after resume verification', async () => {
    getCredential.mockResolvedValue({});
    const stopSecurity = initializeDeviceSecurity();
    // Settle the open-time verification so the app is unlocked and connected,
    // which is the state a resume lock actually starts from.
    await vi.advanceTimersByTimeAsync(200);
    expect(get(securityState)).toMatchObject({ locked: false });
    vi.mocked(relayStore.connectAll).mockClear();

    // Hiding the app locks the interface. It must not drop the transport: the
    // relay key is in localStorage and the unlock redials with it either way,
    // so tearing the socket down only costs the warm resume.
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
    expect(get(securityState)).toMatchObject({ locked: true, reason: 'resume' });
    expect(relayStore.destroy).not.toHaveBeenCalled();
    expect(relayStore.setHidden).toHaveBeenLastCalledWith(true);

    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    await expect(unlockWithDevice('resume')).resolves.toBe(true);

    // Revalidation, not a redial: a warm socket is kept and only a dead one is
    // replaced, so the agent snapshot survives untouched.
    expect(relayStore.connectAll).not.toHaveBeenCalled();
    expect(relayStore.resetReconnectBackoff).toHaveBeenCalled();
    expect(relayStore.revalidateConnections).toHaveBeenCalledWith(2_000);
    stopSecurity();
  });

  it('dials from scratch when verification succeeds at app open', async () => {
    getCredential.mockResolvedValue({});
    securityState.update((state) => ({ ...state, locked: true, reason: 'open' }));

    await expect(unlockWithDevice('open')).resolves.toBe(true);

    // Nothing was ever connected, so there is nothing to revalidate.
    expect(relayStore.connectAll).toHaveBeenCalledWith(false);
    expect(relayStore.revalidateConnections).not.toHaveBeenCalled();
  });

  it('probes after foreground and network return without discarding the current path', () => {
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
    expect(relayStore.revalidateConnections).toHaveBeenNthCalledWith(1, 2_000);
    expect(relayStore.revalidateConnections).toHaveBeenNthCalledWith(2, 2_000);
    // Backoff must be cleared before every probe, or a relay that failed while
    // the phone slept can keep waiting out a stale one-minute delay.
    expect(relayStore.resetReconnectBackoff).toHaveBeenCalledTimes(2);
    expect(vi.mocked(relayStore.resetReconnectBackoff).mock.invocationCallOrder[0])
      .toBeLessThan(vi.mocked(relayStore.revalidateConnections).mock.invocationCallOrder[0]);
    stopSecurity();
  });

  it('probes a visible connection when the browser reports a network change', () => {
    localStorage.removeItem(DEVICE_LOCK_KEY);
    localStorage.removeItem(DEVICE_CREDENTIAL_KEY);
    const connection = new EventTarget();
    Object.defineProperty(navigator, 'connection', { configurable: true, value: connection });
    const stopSecurity = initializeDeviceSecurity();

    connection.dispatchEvent(new Event('change'));

    expect(relayStore.resetReconnectBackoff).toHaveBeenCalledOnce();
    expect(relayStore.revalidateConnections).toHaveBeenCalledWith(2_000);
    expect(vi.mocked(relayStore.resetReconnectBackoff).mock.invocationCallOrder[0])
      .toBeLessThan(vi.mocked(relayStore.revalidateConnections).mock.invocationCallOrder[0]);

    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    connection.dispatchEvent(new Event('change'));
    expect(relayStore.revalidateConnections).toHaveBeenCalledOnce();
    stopSecurity();
  });

  it('probes the existing direct path after a meaningful background interval', async () => {
    localStorage.removeItem(DEVICE_LOCK_KEY);
    localStorage.removeItem(DEVICE_CREDENTIAL_KEY);
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
    const stopSecurity = initializeDeviceSecurity();

    document.dispatchEvent(new Event('visibilitychange'));
    await vi.advanceTimersByTimeAsync(3_000);
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
    document.dispatchEvent(new Event('visibilitychange'));

    expect(relayStore.connectAll).toHaveBeenCalledOnce();
    expect(relayStore.resetReconnectBackoff).toHaveBeenCalledOnce();
    expect(relayStore.revalidateConnections).toHaveBeenCalledWith(2_000);
    stopSecurity();
  });

  it('probes the existing direct path when an installed app resumes', () => {
    localStorage.removeItem(DEVICE_LOCK_KEY);
    localStorage.removeItem(DEVICE_CREDENTIAL_KEY);
    const stopSecurity = initializeDeviceSecurity();

    document.dispatchEvent(new Event('resume'));

    expect(relayStore.connectAll).toHaveBeenCalledOnce();
    expect(relayStore.resetReconnectBackoff).toHaveBeenCalledOnce();
    expect(relayStore.revalidateConnections).toHaveBeenCalledWith(2_000);
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
