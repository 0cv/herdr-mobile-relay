import { writable } from 'svelte/store';
import type { RelayDeviceCredential } from './device-auth';
import { decryptLastKnown, encryptLastKnown, parseLastKnownEnvelope } from './last-known-crypto';
import {
  LAST_KNOWN_AGENT_TYPES,
  LAST_KNOWN_MAX_AGE_MS,
  LAST_KNOWN_MAX_AGENTS,
  LAST_KNOWN_MAX_GROUPS,
  LAST_KNOWN_MAX_LABEL_BYTES,
  LAST_KNOWN_MAX_LABEL_CHARACTERS,
  LAST_KNOWN_MAX_PLAINTEXT_BYTES,
  LAST_KNOWN_MAX_RELAYS,
  LAST_KNOWN_MAX_STORED_BYTES,
  LAST_KNOWN_SCHEMA,
  LAST_KNOWN_STATUSES,
  lastKnownIdentifier,
  lastKnownKeys,
  lastKnownRecord,
  lastKnownTimestamp,
  type LastKnownSummary,
} from './last-known-types';

export type { LastKnownSummary, LastKnownRow, LastKnownGroup } from './last-known-types';
export const LAST_KNOWN_STORAGE_KEY = 'herdr_last_known_session_v1';
const encoder = new TextEncoder();

/**
 * Call only with validated, correlated raw snapshots, never mergeAgentList's
 * sticky/optimistic output. Only these individual display fields are read.
 */
export function projectLastKnown(
  relayId: string,
  agents: readonly unknown[],
  workspaces: readonly unknown[],
  lastFreshAt: number,
): LastKnownSummary {
  if (!lastKnownIdentifier(relayId) || !lastKnownTimestamp(lastFreshAt)
    || !Array.isArray(agents) || !Array.isArray(workspaces)) throw unavailable();
  const summary: LastKnownSummary = {
    schema: LAST_KNOWN_SCHEMA, relayId, lastFreshAt,
    truncated: agents.length > LAST_KNOWN_MAX_AGENTS || workspaces.length > LAST_KNOWN_MAX_GROUPS,
    groups: [], rows: [],
  };
  // Remote IDs exist only during projection and are replaced with ordinal
  // display keys. They can never be recovered as operational targets.
  const groups = new Map<string, string>();
  for (const value of workspaces.slice(0, LAST_KNOWN_MAX_GROUPS)) {
    if (!lastKnownRecord(value)) throw unavailable();
    const id = `g${summary.groups.length}`;
    const label = displayLabel(value.label, 'Workspace');
    summary.truncated ||= label.truncated;
    summary.groups.push({ id, label: label.value });
    if (typeof value.workspace_id === 'string') groups.set(value.workspace_id, id);
  }
  for (const value of agents.slice(0, LAST_KNOWN_MAX_AGENTS)) {
    if (!lastKnownRecord(value)) throw unavailable();
    const label = displayLabel(value.name, 'Agent');
    summary.truncated ||= label.truncated;
    const type = LAST_KNOWN_AGENT_TYPES.find((type) => type === value.agent) ?? 'other';
    const status = LAST_KNOWN_STATUSES.find((status) => status === value.status) ?? 'unknown';
    summary.rows.push({
      id: `r${summary.rows.length}`,
      group: typeof value.workspace_id === 'string' ? groups.get(value.workspace_id) ?? null : null,
      label: label.value, type, status,
    });
  }
  while (plaintextBytes(summary) > LAST_KNOWN_MAX_PLAINTEXT_BYTES) {
    summary.truncated = true;
    if (summary.rows.length) summary.rows.pop();
    else if (summary.groups.length) summary.groups.pop();
    else throw unavailable();
  }
  return summary;
}

/** Strict whole-schema validation: extra fields are not silently accepted. */
export function validateLastKnown(value: unknown, relayId: string, now = Date.now()): LastKnownSummary | null {
  if (!lastKnownRecord(value) || !lastKnownKeys(value, ['schema', 'relayId', 'lastFreshAt', 'truncated', 'groups', 'rows'])
    || value.schema !== LAST_KNOWN_SCHEMA || value.relayId !== relayId || !lastKnownIdentifier(relayId)
    || !lastKnownTimestamp(value.lastFreshAt) || !lastKnownTimestamp(now)
    || value.lastFreshAt > now || now - value.lastFreshAt >= LAST_KNOWN_MAX_AGE_MS
    || typeof value.truncated !== 'boolean' || !Array.isArray(value.groups) || !Array.isArray(value.rows)
    || value.groups.length > LAST_KNOWN_MAX_GROUPS || value.rows.length > LAST_KNOWN_MAX_AGENTS) return null;
  const groupIds = new Set<string>();
  for (const [index, group] of value.groups.entries()) {
    if (!lastKnownRecord(group) || !lastKnownKeys(group, ['id', 'label'])
      || group.id !== `g${index}` || !validLabel(group.label)) return null;
    groupIds.add(group.id as string);
  }
  for (const [index, row] of value.rows.entries()) {
    if (!lastKnownRecord(row) || !lastKnownKeys(row, ['id', 'group', 'label', 'type', 'status'])
      || row.id !== `r${index}` || !validLabel(row.label)
      || (row.group !== null && (typeof row.group !== 'string' || !groupIds.has(row.group)))
      || !LAST_KNOWN_AGENT_TYPES.some((type) => type === row.type)
      || !LAST_KNOWN_STATUSES.some((status) => status === row.status)) return null;
  }
  if (encoder.encode(JSON.stringify(value)).byteLength > LAST_KNOWN_MAX_PLAINTEXT_BYTES) return null;
  return value as unknown as LastKnownSummary;
}

function displayLabel(value: unknown, fallback: string): { value: string; truncated: boolean } {
  if (typeof value !== 'string' || !value.trim()) return { value: fallback, truncated: false };
  const normalized = value.replace(/\p{Cc}/gu, ' ').trim();
  // Lone surrogates cannot survive UTF-8 round-tripping; reject rather than
  // silently publish a label different from the validated string.
  if (/\p{Cs}/u.test(normalized)) return { value: fallback, truncated: true };
  const characters = Array.from(normalized);
  return {
    value: characters.slice(0, LAST_KNOWN_MAX_LABEL_CHARACTERS).join('') || fallback,
    truncated: characters.length > LAST_KNOWN_MAX_LABEL_CHARACTERS,
  };
}

function validLabel(value: unknown): value is string {
  return typeof value === 'string' && value.trim().length > 0
    && Array.from(value).length <= LAST_KNOWN_MAX_LABEL_CHARACTERS
    && !/[\p{Cc}\p{Cs}]/u.test(value)
    && encoder.encode(value).byteLength <= LAST_KNOWN_MAX_LABEL_BYTES;
}

function plaintextBytes(summary: LastKnownSummary): number {
  return encoder.encode(JSON.stringify(summary)).byteLength;
}

interface StoredEntries {
  schema: 1;
  /** Opaque persisted invalidation identity provided by lifecycle integration. */
  epoch: string;
  lastObservedAt: number;
  entries: string[];
}

interface PendingWrite {
  summary: LastKnownSummary;
  credential: RelayDeviceCredential;
  sequence: number;
  generation: number;
  epoch: string;
}

export interface LastKnownCacheOptions {
  storage: Storage | null;
  origin: string;
  /** Must read persisted same-origin invalidation state, not an in-memory copy. */
  epoch: () => string | null;
  credential: (relayId: string) => RelayDeviceCredential | null;
  crypto?: Crypto;
  now?: () => number;
  monotonic?: () => number;
  encrypt?: typeof encryptLastKnown;
  decrypt?: typeof decryptLastKnown;
}

/**
 * Session-only storage, default off and locked. Independent action guards never
 * consult these display summaries. The mandatory persisted epoch provider fails
 * closed and must be revalidated on resume before making summaries visible.
 */
export class LastKnownSessionCache {
  private readonly summariesStore = writable<ReadonlyMap<string, LastKnownSummary>>(new Map());
  readonly summaries = { subscribe: this.summariesStore.subscribe };
  private readonly availabilityStore = writable({ unavailable: false, persistenceUncertain: false });
  readonly availability = { subscribe: this.availabilityStore.subscribe };
  private enabled = false;
  private locked = true;
  private generation = 0;
  private sequence = 0;
  private lastWall = 0;
  private suppressed = false;
  private values = new Map<string, LastKnownSummary>();
  private valuesEpoch: string | null = null;
  private visibleDeadlines = new Map<string, number>();
  private publishedCredentials = new Map<string, RelayDeviceCredential>();
  private pending = new Map<string, PendingWrite>();
  private restoring = new Map<string, object>();
  private latest = new Map<string, number>();
  private latestFreshAt = new Map<string, number>();
  private draining = false;
  private expiryTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(private readonly options: LastKnownCacheOptions) {}

  setEnabled(enabled: boolean): void {
    if (this.enabled === enabled) return;
    this.enabled = enabled;
    if (enabled) this.fence();
    else this.forget();
  }

  setLocked(locked: boolean): void {
    if (this.locked === locked) return;
    this.locked = locked;
    this.fence();
  }

  /** Caller must rotate the persisted epoch before Forget/opt-out/invalidation. */
  forget(): void {
    this.fence();
    try {
      if (!this.options.storage) throw unavailable();
      this.options.storage.removeItem(LAST_KNOWN_STORAGE_KEY);
      this.suppressed = false;
    } catch {
      this.suppressed = true;
      this.availabilityStore.set({ unavailable: true, persistenceUncertain: true });
    }
  }

  /** Content-free cross-tab notification or resumed-tab revalidation. */
  invalidate(): void {
    this.fence();
  }

  /** One in-flight encryption and at most one pending snapshot per relay. */
  write(summary: LastKnownSummary): void {
    try {
      this.enqueue(summary);
    } catch {
      this.availabilityStore.update((state) => ({ ...state, unavailable: true }));
    }
  }

  private enqueue(summary: LastKnownSummary): void {
    const now = this.clock();
    const epoch = this.epoch();
    if (now === null || !this.enabled || this.locked || this.suppressed || !epoch
      || !validateLastKnown(summary, summary.relayId, now)) return;
    if (this.valuesEpoch !== null && this.valuesEpoch !== epoch) this.fence();
    let credential: RelayDeviceCredential | null;
    try {
      credential = this.options.credential(summary.relayId);
    } catch {
      return;
    }
    if (!credential || credential.kind !== 'credential'
      || summary.lastFreshAt < (this.latestFreshAt.get(summary.relayId) ?? 0)) return;
    if (!this.pending.has(summary.relayId) && this.pending.size >= LAST_KNOWN_MAX_RELAYS) {
      const oldest = this.pending.keys().next().value;
      if (oldest !== undefined) this.pending.delete(oldest);
    }
    const sequence = ++this.sequence;
    this.latest.set(summary.relayId, sequence);
    this.latestFreshAt.set(summary.relayId, summary.lastFreshAt);
    // Bound sequence bookkeeping as well as work; only cache scopes matter.
    if (this.latest.size > LAST_KNOWN_MAX_RELAYS + 1) {
      const oldest = this.latest.keys().next().value;
      if (oldest !== undefined) {
        this.latest.delete(oldest);
        this.latestFreshAt.delete(oldest);
      }
    }
    this.pending.set(summary.relayId, {
      summary: JSON.parse(JSON.stringify(summary)) as LastKnownSummary,
      credential: { ...credential }, sequence, generation: this.generation, epoch,
    });
    void this.drain();
  }

  async restore(relayId: string): Promise<void> {
    const now = this.clock();
    const epoch = this.epoch();
    if (now === null || !this.canUse(epoch) || !lastKnownIdentifier(relayId)) return;
    if (this.valuesEpoch !== null && this.valuesEpoch !== epoch) this.fence();
    if (this.restoring.has(relayId) || this.restoring.size >= LAST_KNOWN_MAX_RELAYS) return;
    const token = {};
    this.restoring.set(relayId, token);
    const generation = this.generation;
    const sequence = this.latest.get(relayId);
    try {
      const entries = this.read(epoch!, now);
      const raw = entries.entries.find((entry) => parseLastKnownEnvelope(entry, now).relayId === relayId);
      if (!raw) { this.dropVisible(relayId); return; }
      const envelope = parseLastKnownEnvelope(raw, now);
      const credential = this.options.credential(relayId);
      if (!credential) { this.dropVisible(relayId); return; }
      const plaintext = await (this.options.decrypt ?? decryptLastKnown)(raw, {
        origin: this.options.origin, relayId, credential, lastFreshAt: envelope.lastFreshAt,
      }, this.options.crypto ?? globalThis.crypto, now);
      // Validate the whole plaintext before publishing any rows.
      const currentNow = this.clock();
      if (currentNow === null) return;
      const summary = validateLastKnown(JSON.parse(plaintext), relayId, currentNow);
      if (generation !== this.generation || sequence !== this.latest.get(relayId)) return;
      if (!summary || summary.lastFreshAt !== envelope.lastFreshAt || !this.canUse(epoch)
        || !this.sameCredential(relayId, credential)) { this.dropVisible(relayId); return; }
      this.valuesEpoch = epoch;
      const deadline = (this.options.monotonic?.() ?? performance.now())
        + Math.max(0, summary.lastFreshAt + LAST_KNOWN_MAX_AGE_MS - currentNow);
      this.visibleDeadlines.set(relayId, Math.min(this.visibleDeadlines.get(relayId) ?? deadline, deadline));
      this.values.set(relayId, summary);
      this.publishedCredentials.set(relayId, { ...credential });
      if (this.values.size > LAST_KNOWN_MAX_RELAYS) {
        const oldest = Array.from(this.values).sort((left, right) => left[1].lastFreshAt - right[1].lastFreshAt)[0][0];
        this.values.delete(oldest);
        this.visibleDeadlines.delete(oldest);
        this.publishedCredentials.delete(oldest);
      }
      this.publish();
    } catch {
      if (generation === this.generation && sequence === this.latest.get(relayId)
        && this.restoring.get(relayId) === token) this.dropVisible(relayId);
      this.availabilityStore.update((state) => ({ ...state, unavailable: true }));
    } finally {
      if (this.restoring.get(relayId) === token) this.restoring.delete(relayId);
    }
  }

  /** Revalidate before render after suspension; expiry also runs while mounted. */
  revalidate(): void {
    const now = this.clock();
    const epoch = this.epoch();
    if (now === null || !this.canUse(epoch) || (this.valuesEpoch !== null && this.valuesEpoch !== epoch)) {
      this.fence();
      return;
    }
    const purged = new Set<string>();
    for (const [relayId, summary] of this.values) {
      const credential = this.publishedCredentials.get(relayId);
      let eligible = false;
      try {
        eligible = Boolean(credential && this.sameCredential(relayId, credential));
      } catch {
        // Denied credential storage is not permission to retain decrypted data.
      }
      if (!eligible || !validateLastKnown(summary, relayId, now)
        || (this.options.monotonic?.() ?? performance.now()) >= (this.visibleDeadlines.get(relayId) ?? 0)) {
        this.values.delete(relayId);
        this.visibleDeadlines.delete(relayId);
        this.publishedCredentials.delete(relayId);
        purged.add(relayId);
      }
    }
    this.publish();
    try {
      const entries = this.read(epoch!, now);
      entries.entries = entries.entries.filter((entry) => !purged.has(parseLastKnownEnvelope(entry, now).relayId));
      if (entries.entries.length) this.options.storage!.setItem(LAST_KNOWN_STORAGE_KEY, JSON.stringify(entries));
      else this.options.storage!.removeItem(LAST_KNOWN_STORAGE_KEY);
    } catch {
      this.fence();
      this.availabilityStore.set({ unavailable: true, persistenceUncertain: true });
    }
  }

  private dropVisible(relayId: string): void {
    if (!this.values.delete(relayId)) return;
    this.visibleDeadlines.delete(relayId);
    this.publishedCredentials.delete(relayId);
    this.publish();
  }

  dispose(): void {
    this.enabled = false;
    this.fence();
  }

  private async drain(): Promise<void> {
    if (this.draining) return;
    this.draining = true;
    try {
      while (this.pending.size) {
        const [relayId, work] = this.pending.entries().next().value!;
        this.pending.delete(relayId);
        try {
          const raw = await (this.options.encrypt ?? encryptLastKnown)(JSON.stringify(work.summary), {
            origin: this.options.origin, relayId, credential: work.credential,
            lastFreshAt: work.summary.lastFreshAt,
          }, this.options.crypto ?? globalThis.crypto, this.options.now?.() ?? Date.now());
          const now = this.clock();
          if (now === null || !this.validWork(relayId, work)
            || !validateLastKnown(work.summary, relayId, now)) continue;
          const entries = this.read(work.epoch, now);
          if (entries.entries.some((entry) => {
            const envelope = parseLastKnownEnvelope(entry, now);
            return envelope.relayId === relayId && envelope.lastFreshAt > work.summary.lastFreshAt;
          })) continue;
          entries.entries = entries.entries.filter((entry) => parseLastKnownEnvelope(entry, now).relayId !== relayId);
          entries.lastObservedAt = now;
          entries.entries.push(raw);
          entries.entries.sort((left, right) => parseLastKnownEnvelope(left, now).lastFreshAt
            - parseLastKnownEnvelope(right, now).lastFreshAt);
          let serialized = JSON.stringify(entries);
          while (entries.entries.length > LAST_KNOWN_MAX_RELAYS
            || encoder.encode(serialized).byteLength > LAST_KNOWN_MAX_STORED_BYTES) {
            entries.entries.shift();
            serialized = JSON.stringify(entries);
          }
          // There is no await between the final generation/epoch check and the
          // atomic synchronous sessionStorage write. A later invalidation must
          // delete/revalidate this scope, not rely on storage events alone.
          if (!this.validWork(relayId, work)) continue;
          if (!this.options.storage) throw unavailable();
          this.options.storage.setItem(LAST_KNOWN_STORAGE_KEY, serialized);
        } catch {
          this.availabilityStore.update((state) => ({ ...state, unavailable: true }));
        }
      }
    } finally {
      this.draining = false;
    }
  }

  private validWork(relayId: string, work: PendingWrite): boolean {
    return work.generation === this.generation && this.latest.get(relayId) === work.sequence
      && this.canUse(work.epoch) && this.sameCredential(relayId, work.credential);
  }

  private sameCredential(relayId: string, expected: RelayDeviceCredential): boolean {
    const current = this.options.credential(relayId);
    return current?.kind === 'credential' && current.id === expected.id && current.version === expected.version
      && current.secret === expected.secret && current.deviceId === expected.deviceId;
  }

  private read(epoch: string, now: number): StoredEntries {
    if (!this.options.storage) throw unavailable();
    const raw = this.options.storage.getItem(LAST_KNOWN_STORAGE_KEY);
    if (!raw) return { schema: 1, epoch, lastObservedAt: now, entries: [] };
    if (raw.length > LAST_KNOWN_MAX_STORED_BYTES
      || encoder.encode(raw).byteLength > LAST_KNOWN_MAX_STORED_BYTES) throw unavailable();
    const value: unknown = JSON.parse(raw);
    if (!lastKnownRecord(value) || !lastKnownKeys(value, ['schema', 'epoch', 'lastObservedAt', 'entries'])
      || value.schema !== 1 || !lastKnownTimestamp(value.lastObservedAt) || value.lastObservedAt > now || !Array.isArray(value.entries)
      || value.entries.length > LAST_KNOWN_MAX_RELAYS) throw unavailable();
    if (value.epoch !== epoch) return { schema: 1, epoch, lastObservedAt: now, entries: [] };
    const seen = new Set<string>();
    const entries: string[] = [];
    for (const entry of value.entries) {
      if (typeof entry !== 'string') throw unavailable();
      try {
        const envelope = parseLastKnownEnvelope(entry, now);
        if (seen.has(envelope.relayId)) throw unavailable();
        seen.add(envelope.relayId);
        entries.push(entry);
      } catch {
        // A malformed, tampered, expired or future entry never reaches decrypt.
        // Discard independently so a bad relay cannot prevent other misses/writes.
      }
    }
    return { schema: 1, epoch, lastObservedAt: now, entries };
  }

  private canUse(epoch: string | null): boolean {
    return this.enabled && !this.locked && !this.suppressed && epoch !== null && this.epoch() === epoch;
  }

  private epoch(): string | null {
    try {
      const epoch = this.options.epoch();
      return typeof epoch === 'string' && /^[A-Za-z0-9_-]{43}$/.test(epoch) ? epoch : null;
    } catch {
      return null;
    }
  }

  private clock(): number | null {
    const now = this.options.now?.() ?? Date.now();
    if (!lastKnownTimestamp(now) || now < this.lastWall) {
      this.fence();
      this.suppressed = true;
      return null;
    }
    this.lastWall = now;
    return now;
  }

  private fence(): void {
    this.generation++;
    this.pending.clear();
    this.latest.clear();
    this.latestFreshAt.clear();
    this.values.clear();
    this.visibleDeadlines.clear();
    this.publishedCredentials.clear();
    this.valuesEpoch = null;
    this.publish();
  }

  private publish(): void {
    if (this.expiryTimer !== null) clearTimeout(this.expiryTimer);
    this.expiryTimer = null;
    this.summariesStore.set(new Map(this.values));
    if (!this.values.size) return;
    const expiry = Math.min(...Array.from(this.values.values(), (summary) => summary.lastFreshAt + LAST_KNOWN_MAX_AGE_MS));
    const monotonicRemaining = Math.min(...this.visibleDeadlines.values()) - (this.options.monotonic?.() ?? performance.now());
    this.expiryTimer = setTimeout(() => this.revalidate(), Math.max(0, Math.min(30_000,
      monotonicRemaining, expiry - (this.options.now?.() ?? Date.now()))));
  }
}

function unavailable(): Error {
  return new Error('Last-known cache is unavailable.');
}
