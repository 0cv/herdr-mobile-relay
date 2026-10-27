import { agentNeedsInspection } from './agents';
import { targetRefForAgent, targetStoreKey } from './resource-id';
import type { Agent, FrontendTargetRef } from './types';

export interface PiPromptLease {
  instance: string;
  pane: string;
  session: string;
  pid: number;
  incarnation: string;
  connection: string;
  target_key: string;
  token: string;
  expires_at: number;
}

export interface PiPromptCapability {
  lease: PiPromptLease;
  target: FrontendTargetRef;
  observedAt: number;
  connection: object;
  transport: unknown;
}

export function piPromptTarget(agent: Agent): boolean {
  return agent.agent === 'pi' && agent.status?.trim().toLowerCase() === 'blocked' && agentNeedsInspection(agent) && Boolean(agent.agent_session_id);
}

export function validPiPromptText(text: string): boolean {
  const bytes = new TextEncoder().encode(text);
  return Boolean(text.trim()) && bytes.length <= 100_000 && new TextDecoder().decode(bytes) === text
    && !/[\x00-\x08\x0b-\x1f\x7f]/.test(text) && !text.includes('Attachment: ') && !text.includes('[attachment:');
}

export class PiPromptDelivery {
  private phase = $state<'idle' | 'pending' | 'unknown' | 'sent' | 'unavailable'>('idle');
  get pending() { return this.phase === 'pending'; }
  get uncertain() { return this.phase !== 'idle' && !this.pending; }
  get handed() { return this.phase === 'sent'; }
  get error() { return this.phase === 'unavailable' ? 'Pi sends need storage and Web Locks.' : ''; }
  private users = 0;

  constructor(private readonly key: string | null = null, private readonly onIdle: () => void = () => {}) {
    this.syncStorage();
  }

  private syncStorage(value?: string | null): boolean {
    if (!this.key) return true;
    try {
      if (value === null) localStorage.removeItem(this.key);
      else if (value !== undefined) localStorage.setItem(this.key, value);
      const saved = localStorage.getItem(this.key);
      if (value === undefined) {
        this.phase = saved === null ? 'idle' : saved === 'sent' ? 'sent' : 'unknown';
      } else if (saved !== value) throw new Error();
      return true;
    } catch {
      this.phase = 'unavailable';
      return false;
    }
  }
  retain() { this.users += 1; }
  release() { this.users -= 1; this.prune(); }
  private prune() { if (!this.users && this.phase === 'idle') this.onIdle(); }

  begin(): boolean {
    if (this.phase !== 'idle') return false;
    this.syncStorage();
    if (this.uncertain || !this.syncStorage('unknown')) return false;
    this.phase = 'pending';
    return true;
  }

  finish(uncertain: boolean, sentAt = 0) {
    this.phase = uncertain ? sentAt > 0 ? 'sent' : 'unknown' : 'idle';
    if (!uncertain || this.handed) this.syncStorage(uncertain ? 'sent' : null);
    this.prune();
  }

  async withLock<T>(action: () => T | Promise<T>): Promise<T | undefined> {
    if (!this.key) return action();
    let entered = false;
    try {
      return await navigator.locks.request(this.key, { ifAvailable: true }, lock => {
        if (lock) { entered = true; return action(); }
        this.phase = 'unknown';
      });
    } catch (error) {
      if (entered) throw error;
      this.phase = 'unavailable';
    }
  }

  async acknowledge() {
    if (this.pending) return;
    await this.withLock(() => {
      if (!this.syncStorage(null)) return;
      this.phase = 'idle';
      this.prune();
    });
  }
}

const deliveries = new Map<string, PiPromptDelivery>();
export function piPromptDelivery(agent: Agent): PiPromptDelivery {
  const target = targetRefForAgent(agent);
  const key = target && targetStoreKey(target);
  if (!key) return new PiPromptDelivery();
  let delivery = deliveries.get(key);
  if (!delivery) {
    delivery = new PiPromptDelivery(agent.agent === 'pi' && agent.agent_session_id ? `herdr_pi_send:${key}` : null, () => { if (deliveries.get(key) === delivery) deliveries.delete(key); });
    deliveries.set(key, delivery);
  }
  return delivery;
}
