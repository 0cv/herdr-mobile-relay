import { base64UrlEncode } from './base64url';

export const INVENTORY_SNAPSHOT_CAPABILITY = 'inventory_snapshot_v1';
export const INVENTORY_SNAPSHOT_TIMEOUT_MS = 15_000;

/** These are local authority identities, never persisted cache identifiers. */
export interface InventoryBinding {
  connection: object;
  path: object;
  wake: number;
}

export interface AuthoritativeInventory<TAgent, TWorkspace> {
  agents: readonly TAgent[];
  workspaces: readonly TWorkspace[] | null;
}

/**
 * Independent of metrics and persistence. One nonce, bounded lifetime, no
 * timing-based legacy fallback. The caller must additionally validate targets
 * and roles again at actual dispatch and invalidate on every lifecycle edge.
 */
export class InventoryFreshness<TAgent, TWorkspace> {
  private binding: InventoryBinding | null = null;
  private pending: { nonce: string; expiresAt: number } | null = null;
  private inventory: AuthoritativeInventory<TAgent, TWorkspace> | null = null;
  private lastWall = 0;

  constructor(
    private readonly parseAgents: (value: unknown) => readonly TAgent[] | null,
    private readonly parseWorkspaces: (value: unknown) => readonly TWorkspace[] | null,
    private readonly now: () => number = Date.now,
    private readonly nonce: () => string = randomNonce,
  ) {}

  invalidate(): void {
    this.binding = null;
    this.pending = null;
    this.inventory = null;
  }

  /** Unsupported peers fail closed rather than satisfying a stronger claim. */
  request(binding: InventoryBinding, supported: boolean): Record<string, unknown> | null {
    this.invalidate();
    const now = this.clock();
    if (!supported || now === null || !Number.isSafeInteger(binding.wake) || binding.wake < 0) return null;
    try {
      const nonce = this.nonce();
      if (!/^[A-Za-z0-9_-]{21}[AQgw]$/.test(nonce)) return null;
      this.binding = { connection: binding.connection, path: binding.path, wake: binding.wake };
      this.pending = { nonce, expiresAt: now + INVENTORY_SNAPSHOT_TIMEOUT_MS };
      return { type: 'refresh_agents', snapshot_request_id: nonce };
    } catch {
      return null;
    }
  }

  accept(binding: InventoryBinding, message: unknown): boolean {
    const now = this.clock();
    if (now === null || !this.matches(binding) || !this.pending) return false;
    if (now >= this.pending.expiresAt) {
      this.invalidate();
      return false;
    }
    if (!record(message) || message.type !== 'inventory_snapshot'
      || message.snapshot_request_id !== this.pending.nonce) return false;
    // Consume exactly once. An old callback can neither promote nor revoke a
    // newer generation because the binding and nonce were checked first.
    this.pending = null;
    if (!record(message.inventory) || message.inventory.state !== 'ready'
      || message.inventory.stale !== false) {
      this.inventory = null;
      return false;
    }
    try {
      const agents = this.parseAgents(message.agents);
      if (!agents) return false;
      this.inventory = { agents, workspaces: this.parseWorkspaces(message.workspaces) };
      return true;
    } catch {
      this.inventory = null;
      return false;
    }
  }

  current(binding: InventoryBinding): AuthoritativeInventory<TAgent, TWorkspace> | null {
    if (this.clock() === null || !this.matches(binding)) return null;
    return this.inventory;
  }

  private matches(binding: InventoryBinding): boolean {
    return this.binding !== null && this.binding.connection === binding.connection
      && this.binding.path === binding.path && this.binding.wake === binding.wake;
  }

  private clock(): number | null {
    const now = this.now();
    if (!Number.isSafeInteger(now) || now < 0 || now < this.lastWall) {
      this.invalidate();
      return null;
    }
    this.lastWall = now;
    return now;
  }
}

function randomNonce(): string {
  return base64UrlEncode(crypto.getRandomValues(new Uint8Array(16)));
}

function record(value: unknown): value is Record<string, unknown> {
  return Boolean(value && typeof value === 'object' && !Array.isArray(value));
}
