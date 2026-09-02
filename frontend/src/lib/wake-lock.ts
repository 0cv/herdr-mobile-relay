import { get, writable } from 'svelte/store';
import { terminalWakeLock } from './preferences';

export type WakeLockState = 'disabled' | 'unsupported' | 'requesting' | 'active' | 'released' | 'failed';


export const wakeLockState = writable<WakeLockState>('disabled');

export function mountTerminalWakeLock(): () => void {
  const wakeLock = navigator.wakeLock;
  let handle: WakeLockSentinel | null = null;
  let acquiring = false;
  let mounted = true;

  const release = async (state: WakeLockState) => {
    const current = handle;
    handle = null;
    if (current && !current.released) await current.release().catch(() => {});
    if (mounted) wakeLockState.set(state);
  };

  const acquire = async () => {
    if (!mounted || acquiring || handle || !get(terminalWakeLock) || document.visibilityState !== 'visible') return;
    if (!wakeLock) {
      wakeLockState.set('unsupported');
      return;
    }
    acquiring = true;
    wakeLockState.set('requesting');
    try {
      const next = await wakeLock.request('screen');
      if (!mounted || !get(terminalWakeLock) || document.visibilityState !== 'visible') {
        await next.release().catch(() => {});
        return;
      }
      handle = next;
      next.addEventListener('release', () => {
        if (handle !== next) return;
        handle = null;
        if (mounted) wakeLockState.set('released');
      });
      wakeLockState.set('active');
    } catch {
      if (mounted) wakeLockState.set('failed');
    } finally {
      acquiring = false;
    }
  };

  const preference = terminalWakeLock.subscribe((enabled) => {
    if (!enabled) void release('disabled');
    else void acquire();
  });
  const visibility = () => {
    if (document.visibilityState === 'visible') void acquire();
    else void release('released');
  };
  const pageshow = () => { void acquire(); };
  document.addEventListener('visibilitychange', visibility);
  window.addEventListener('pageshow', pageshow);

  return () => {
    wakeLockState.set('released');
    mounted = false;
    preference();
    document.removeEventListener('visibilitychange', visibility);
    window.removeEventListener('pageshow', pageshow);
    void release('released');
  };
}
