import { targetRefForAgent, targetStoreKey } from './resource-id';
import type { Agent } from './types';

export class TerminalInputDelivery {
  pending = $state(false);
  uncertainAt = $state<number | null>(null);
  private users = 0;

  constructor(private readonly onIdle: () => void = () => {}) {}

  get uncertain() { return this.uncertainAt !== null; }

  retain() { this.users += 1; }

  release() {
    this.users -= 1;
    this.prune();
  }

  begin(): boolean {
    if (this.pending || this.uncertain) return false;
    this.pending = true;
    return true;
  }

  finish(uncertain: boolean) {
    if (uncertain) this.uncertainAt = performance.now();
    this.pending = false;
    this.prune();
  }

  canInspect(readStartedAt?: number): boolean {
    return !this.pending && this.uncertainAt !== null && Number.isFinite(readStartedAt) && readStartedAt! > this.uncertainAt;
  }

  inspect(readStartedAt?: number): boolean {
    if (!this.canInspect(readStartedAt)) return false;
    this.uncertainAt = null;
    this.prune();
    return true;
  }

  private prune() {
    if (!this.users && !this.pending && !this.uncertain) this.onIdle();
  }
}

const deliveries = new Map<string, TerminalInputDelivery>();

/** True while any send to a terminal is pending or unconfirmed. */
export function terminalInputUnsettled(): boolean {
  for (const delivery of deliveries.values()) if (delivery.pending || delivery.uncertain) return true;
  return false;
}

export function terminalInputDelivery(agent: Agent): TerminalInputDelivery {
  const target = targetRefForAgent(agent);
  const key = target && targetStoreKey(target);
  if (!key) return new TerminalInputDelivery();
  const current = deliveries.get(key);
  if (current) return current;
  const delivery = new TerminalInputDelivery(() => {
    if (deliveries.get(key) === delivery) deliveries.delete(key);
  });
  deliveries.set(key, delivery);
  return delivery;
}
