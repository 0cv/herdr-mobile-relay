import { writable } from 'svelte/store';
import { base64UrlEncode } from './base64url';
import { LastKnownSessionCache } from './last-known';

export const LAST_KNOWN_CONTROL_KEY = 'herdr_last_known_control_v1';
const CHANNEL = 'herdr:last-known:invalidate:v1';
interface Control { schema: 1; enabled: boolean; epoch: string }

/** Origin-wide, content-free invalidation. A purge conservatively covers all relays. */
export class LastKnownControl {
  private control: Control | null = null;
  private blocked = true;
  private failedControlChange = false;
  private pendingControlChanges = 0;
  generation = 0;
  private channel: BroadcastChannel | null = null;
  private readonly stateStore = writable({ enabled: false, unavailable: false });
  readonly state = { subscribe: this.stateStore.subscribe };

  constructor(private readonly storage: Storage | null, private readonly cache: LastKnownSessionCache) {}

  epoch = (): string | null => {
    if (this.blocked || this.failedControlChange || this.pendingControlChanges) return null;
    const current = this.read();
    return current?.enabled ? current.epoch : null;
  };

  initialize(): () => void {
    this.sync();
    const changed = (event: StorageEvent) => {
      if (event.key === LAST_KNOWN_CONTROL_KEY || event.key === null) this.sync();
    };
    const resumed = () => { this.sync(); this.cache.revalidate(); };
    window.addEventListener('storage', changed);
    window.addEventListener('pageshow', resumed);
    document.addEventListener('resume', resumed);
    try {
      this.channel = new BroadcastChannel(CHANNEL);
      this.channel.onmessage = (event) => {
        // A failed durable change is a deny-only hint, never authority to
        // restore consent from an old readable enabled record.
        if (event.data?.type === 'invalidation_failed') this.failClosed();
        else this.sync();
      };
    } catch {
      // Persisted epochs, storage events and resume revalidation remain required;
      // notifications alone are never the authority for a restored tab copy.
    }
    return () => {
      window.removeEventListener('storage', changed);
      window.removeEventListener('pageshow', resumed);
      document.removeEventListener('resume', resumed);
      this.channel?.close();
      this.channel = null;
      this.blocked = true;
      this.cache.dispose();
    };
  }

  sync(): void {
    // Notifications/resume cannot undo a local invalidation that failed to
    // persist, nor restore authority while a serialized change is pending.
    if (this.failedControlChange || this.pendingControlChanges) {
      this.blocked = true;
      this.cache.setEnabled(false);
      this.cache.invalidate();
      this.stateStore.set({ enabled: false, unavailable: this.failedControlChange });
      return;
    }
    const current = this.read();
    if (!current || !navigator.locks?.request) {
      this.blocked = true;
      this.control = null;
      this.cache.setEnabled(false);
      this.cache.invalidate();
      this.stateStore.set({ enabled: false, unavailable: this.storage === null || !navigator.locks?.request });
      return;
    }
    if (this.control && (current.epoch !== this.control.epoch || current.enabled !== this.control.enabled)) {
      this.generation++;
      this.cache.forget();
      this.cache.setEnabled(current.enabled);
    }
    this.cache.setEnabled(current.enabled);
    this.control = current;
    this.blocked = false;
    this.stateStore.set({ enabled: current.enabled, unavailable: false });
  }

  setEnabled(enabled: boolean): Promise<boolean> { return this.rotate(enabled); }
  forget(): Promise<boolean> { return this.rotate(); }

  /** Fence immediately; Web Locks serialize concurrent control changes, not data. */
  async rotate(enabled?: boolean): Promise<boolean> {
    this.generation++;
    this.pendingControlChanges++;
    this.blocked = true;
    this.cache.forget();
    try {
      if (!this.storage || !navigator.locks?.request) throw new Error('Coordination unavailable');
      await navigator.locks.request(CHANNEL, () => {
        const previous = this.read();
        this.cache.forget();
        if (!previous && enabled === undefined) return;
        const current: Control = {
          // Retrying Forget after a failed change conservatively persists off;
          // only a new explicit opt-in can restore consent in that state.
          schema: 1, enabled: enabled ?? (this.failedControlChange ? false : previous?.enabled ?? false),
          epoch: base64UrlEncode(crypto.getRandomValues(new Uint8Array(32))),
        };
        this.storage!.setItem(LAST_KNOWN_CONTROL_KEY, JSON.stringify(current));
        if (this.read()?.epoch !== current.epoch) throw new Error('Invalidation storage unavailable');
        this.control = current;
        this.failedControlChange = false;
        // No labels, relay identifiers, credentials or ciphertext cross tabs.
        this.channel?.postMessage({ type: 'invalidate' });
      });
      return true;
    } catch {
      this.failClosed();
      try {
        this.channel?.postMessage({ type: 'invalidation_failed' });
      } catch {
        // Unreachable/suspended peers and durable erasure remain uncertain;
        // the local failure latch must survive notification failure too.
      }
      return false;
    } finally {
      this.pendingControlChanges--;
      this.sync();
    }
  }

  private failClosed(): void {
    this.generation++;
    this.failedControlChange = true;
    this.blocked = true;
    this.cache.setEnabled(false);
    this.cache.forget();
    this.stateStore.set({ enabled: false, unavailable: true });
  }

  private read(): Control | null {
    try {
      const raw = this.storage?.getItem(LAST_KNOWN_CONTROL_KEY);
      if (!raw || raw.length > 256) return null;
      const value = JSON.parse(raw);
      if (value?.schema !== 1 || typeof value.enabled !== 'boolean'
        || typeof value.epoch !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(value.epoch)
        || Object.keys(value).length !== 3) return null;
      return { schema: 1, enabled: value.enabled, epoch: value.epoch };
    } catch {
      return null;
    }
  }
}
