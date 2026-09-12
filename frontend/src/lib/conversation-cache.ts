import { registerConversationCacheClearer } from './conversation-cache-control';
import type { ConversationBrowseDiagnostics, ConversationBrowseMode, ConversationEntry, OmoTodoState } from './types';

/** Maximum number of conversation identities retained in the in-memory preview. */
export const CONVERSATION_CACHE_MAX_IDENTITIES = 6;
/** A preview is stale after ten minutes without a successful accepted update. */
export const CONVERSATION_CACHE_TTL_MS = 10 * 60_000;
/** Per-preview UTF-16 accounting budget. */
export const CONVERSATION_CACHE_ITEM_BYTES = 4 * 1024 * 1024;
/** Aggregate UTF-16 accounting budget. */
export const CONVERSATION_CACHE_TOTAL_BYTES = 24 * 1024 * 1024;
/** Do not retain unbounded raw history in a warm phone preview. */
export const CONVERSATION_CACHE_MAX_ENTRIES = 1_000;

export interface ConversationPreview {
  identity: string;
  /** Relay identity is kept separately so removal can clear without parsing keys. */
  relayId: string;
  savedAt: number;
  sourceRevision: string;
  historical: boolean;
  mode?: ConversationBrowseMode;
  diagnostics?: ConversationBrowseDiagnostics;
  plan?: OmoTodoState | null;
  entries: ConversationEntry[];
}

export interface ConversationCacheLimits {
  maxIdentities: number;
  ttlMs: number;
  itemBytes: number;
  totalBytes: number;
  maxEntries: number;
}

interface CacheRecord {
  preview: ConversationPreview;
  bytes: number;
  lastUsed: number;
}

const cache = new Map<string, CacheRecord>();
let limits: ConversationCacheLimits = defaultConversationCacheLimits();
registerConversationCacheClearer((relayId) => {
  if (!relayId) clearConversationPreviews();
  else clearConversationPreviewsForRelay(relayId);
});

export function defaultConversationCacheLimits(): ConversationCacheLimits {
  return {
    maxIdentities: CONVERSATION_CACHE_MAX_IDENTITIES,
    ttlMs: CONVERSATION_CACHE_TTL_MS,
    itemBytes: CONVERSATION_CACHE_ITEM_BYTES,
    totalBytes: CONVERSATION_CACHE_TOTAL_BYTES,
    maxEntries: CONVERSATION_CACHE_MAX_ENTRIES,
  };
}

/**
 * Test-only limits. The cache remains memory-only; changing limits also drops
 * existing values so a test cannot accidentally observe a previous policy.
 */
export function configureConversationCache(next: Partial<ConversationCacheLimits>): void {
  limits = {
    ...limits,
    ...next,
    maxIdentities: Math.max(1, Math.trunc(next.maxIdentities ?? limits.maxIdentities)),
    ttlMs: Math.max(1, Math.trunc(next.ttlMs ?? limits.ttlMs)),
    itemBytes: Math.max(1, Math.trunc(next.itemBytes ?? limits.itemBytes)),
    totalBytes: Math.max(1, Math.trunc(next.totalBytes ?? limits.totalBytes)),
    maxEntries: Math.max(1, Math.trunc(next.maxEntries ?? limits.maxEntries)),
  };
  clearConversationPreviews();
}

export function resetConversationCacheLimits(): void {
  limits = defaultConversationCacheLimits();
  clearConversationPreviews();
}

export function getConversationPreview(identity: string, now = Date.now()): ConversationPreview | null {
  const record = cache.get(identity);
  if (!record) return null;
  if (now - record.preview.savedAt >= limits.ttlMs) {
    cache.delete(identity);
    return null;
  }
  record.lastUsed = now;
  return clonePreview(record.preview);
}

export function putConversationPreview(preview: ConversationPreview, now = Date.now()): boolean {
  if (!preview.identity || !preview.relayId || !Number.isFinite(now)) return false;
  const candidate = boundedPreview({ ...preview, savedAt: now });
  if (!candidate) return false;
  const bytes = estimatePreviewBytes(candidate);
  if (bytes > limits.itemBytes || bytes > limits.totalBytes) return false;
  const previous = cache.get(candidate.identity);
  cache.set(candidate.identity, { preview: candidate, bytes, lastUsed: now });
  evictToLimits(now, previous?.bytes || 0);
  return cache.has(candidate.identity);
}

export function deleteConversationPreview(identity: string): void {
  cache.delete(identity);
}

export function clearConversationPreviews(): void {
  cache.clear();
}

export function clearConversationPreviewsForRelay(relayId: string): void {
  if (!relayId) return;
  for (const [identity, record] of cache) {
    if (record.preview.relayId === relayId) cache.delete(identity);
  }
}

/** A small introspection helper useful to lifecycle tests and diagnostics. */
export function conversationPreviewCacheSize(): { identities: number; bytes: number } {
  let bytes = 0;
  for (const record of cache.values()) bytes += record.bytes;
  return { identities: cache.size, bytes };
}

function boundedPreview(input: ConversationPreview): ConversationPreview | null {
  const entries = cloneEntries(input.entries);
  if (!entries.length) return null;

  // Group the raw records into whole exchanges. A leading assistant-only
  // fragment is part of the first group and is discarded together with that
  // group when the newest suffix is selected.
  const groups: ConversationEntry[][] = [];
  let current: ConversationEntry[] = [];
  for (const entry of entries) {
    if (entry.role === 'user' && current.length) {
      groups.push(current);
      current = [];
    }
    current.push(entry);
  }
  if (current.length) groups.push(current);
  if (!groups.length) return null;

  const retained: ConversationEntry[][] = [];
  let retainedCount = 0;
  let retainedBytes = 0;
  for (let index = groups.length - 1; index >= 0; index--) {
    const group = groups[index];
    const groupBytes = estimateEntriesBytes(group);
    if (group.length > limits.maxEntries || groupBytes > limits.itemBytes) {
      if (!retained.length) return null;
      break;
    }
    if (retainedCount + group.length > limits.maxEntries || retainedBytes + groupBytes > limits.itemBytes) break;
    retained.unshift(group);
    retainedCount += group.length;
    retainedBytes += groupBytes;
  }
  if (!retained.length) return null;
  const keptEntries = retained.flatMap((group) => group);
  return {
    identity: input.identity,
    relayId: input.relayId,
    savedAt: Number.isFinite(input.savedAt) ? input.savedAt : Date.now(),
    sourceRevision: input.sourceRevision || '',
    historical: input.historical === true,
    ...(input.mode ? { mode: input.mode } : {}),
    ...(input.diagnostics ? { diagnostics: cloneDiagnostics(input.diagnostics) } : {}),
    ...(input.plan ? { plan: clonePlan(input.plan) } : {}),
    entries: keptEntries,
  };
}

function evictToLimits(now: number, replacedBytes: number): void {
  // `replacedBytes` is intentionally unused in the map scan below; retaining it
  // in the signature documents that a replacement is not double-counted.
  void replacedBytes;
  for (const [identity, record] of cache) {
    if (now - record.preview.savedAt >= limits.ttlMs) cache.delete(identity);
  }
  while (cache.size > limits.maxIdentities || totalCacheBytes() > limits.totalBytes) {
    const oldest = [...cache.entries()].sort((left, right) => left[1].lastUsed - right[1].lastUsed)[0];
    if (!oldest) break;
    cache.delete(oldest[0]);
  }
}

function totalCacheBytes(): number {
  let total = 0;
  for (const record of cache.values()) total += record.bytes;
  return total;
}

function clonePreview(preview: ConversationPreview): ConversationPreview {
  return {
    ...preview,
    ...(preview.diagnostics ? { diagnostics: cloneDiagnostics(preview.diagnostics) } : {}),
    ...(preview.plan ? { plan: clonePlan(preview.plan) } : {}),
    entries: cloneEntries(preview.entries),
  };
}

function cloneEntries(entries: ConversationEntry[]): ConversationEntry[] {
  return entries.map((entry) => ({
    ...entry,
    ...(entry.tools ? { tools: entry.tools.map((tool) => ({ ...tool })) } : {}),
  }));
}

function cloneDiagnostics(diagnostics: ConversationBrowseDiagnostics): ConversationBrowseDiagnostics {
  return { ...diagnostics };
}

function clonePlan(plan: OmoTodoState): OmoTodoState {
  return {
    ...plan,
    phases: plan.phases.map((phase) => ({
      ...phase,
      tasks: phase.tasks.map((task) => ({ ...task })),
    })),
  };
}

/** Conservative UTF-16 heap accounting, not a serialized wire-size estimate. */
function estimatePreviewBytes(preview: ConversationPreview): number {
  return estimateEntriesBytes(preview.entries)
    + estimateString(preview.identity)
    + estimateString(preview.relayId)
    + estimateString(preview.sourceRevision)
    + estimateObjectBytes(preview.diagnostics)
    + estimateObjectBytes(preview.plan);
}

function estimateEntriesBytes(entries: ConversationEntry[]): number {
  return entries.reduce((total, entry) => total + 96 + estimateString(entry.id)
    + estimateString(entry.timestamp) + estimateString(entry.text)
    + (entry.tools || []).reduce((toolTotal, tool) => toolTotal + 64
      + estimateString(tool.id) + estimateString(tool.name) + estimateString(tool.input)
      + estimateString(tool.output), 0), 0);
}

function estimateString(value: unknown): number {
  return typeof value === 'string' ? value.length * 2 : 0;
}

function estimateObjectBytes(value: unknown): number {
  if (value === undefined || value === null) return 0;
  try {
    return JSON.stringify(value).length * 2;
  } catch {
    return 0;
  }
}
