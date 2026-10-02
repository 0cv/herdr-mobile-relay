export const LAST_KNOWN_SCHEMA = 1;
export const LAST_KNOWN_MAX_AGE_MS = 60 * 60_000;
export const LAST_KNOWN_MAX_LABEL_CHARACTERS = 120;
export const LAST_KNOWN_MAX_LABEL_BYTES = 480;
export const LAST_KNOWN_MAX_AGENTS = 200;
export const LAST_KNOWN_MAX_GROUPS = 50;
export const LAST_KNOWN_MAX_PLAINTEXT_BYTES = 64 * 1024;
export const LAST_KNOWN_MAX_ENTRY_BYTES = 96 * 1024;
export const LAST_KNOWN_MAX_STORED_BYTES = 512 * 1024;
export const LAST_KNOWN_MAX_RELAYS = 10;

export const LAST_KNOWN_AGENT_TYPES = [
  'claude', 'codex', 'cursor', 'gemini', 'hermes', 'opencode', 'pi', 'qoder', 'other',
] as const;
export const LAST_KNOWN_STATUSES = ['idle', 'working', 'blocked', 'stopped', 'unknown'] as const;

export interface LastKnownGroup {
  /** Synthetic display key only. Not a workspace ID. */
  id: string;
  label: string;
}

export interface LastKnownRow {
  /** Synthetic display key only. Not a pane, terminal, session or target ID. */
  id: string;
  group: string | null;
  label: string;
  type: typeof LAST_KNOWN_AGENT_TYPES[number];
  status: typeof LAST_KNOWN_STATUSES[number];
}

export interface LastKnownSummary {
  schema: typeof LAST_KNOWN_SCHEMA;
  relayId: string;
  lastFreshAt: number;
  truncated: boolean;
  groups: LastKnownGroup[];
  rows: LastKnownRow[];
}

export function lastKnownRecord(value: unknown): value is Record<string, unknown> {
  return Boolean(value && typeof value === 'object' && !Array.isArray(value));
}

export function lastKnownIdentifier(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0 && value.length <= 256
    && !/[\p{Cc}\p{Cs}]/u.test(value) && new TextEncoder().encode(value).byteLength <= 1024;
}

export function lastKnownTimestamp(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
    && value <= Number.MAX_SAFE_INTEGER - LAST_KNOWN_MAX_AGE_MS;
}

export function lastKnownKeys(value: Record<string, unknown>, keys: readonly string[]): boolean {
  return Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}
