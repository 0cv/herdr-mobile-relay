import { CommandError } from './command-error';
import { parseWireSafeInteger } from './protocol';
import { isRecord } from './record';
import type { ConversationEditDiff, ConversationPage, OmoTodoState } from './types';

function normalizeOmoTodoState(value: unknown): OmoTodoState | undefined {
  if (!isRecord(value)) return undefined;
  const state = value as Record<string, unknown>;
  let remainingTasks = 1_000;
  const phases = (Array.isArray(state.phases) ? state.phases : [])
    .slice(0, 128)
    .flatMap((candidate) => {
      if (!isRecord(candidate)) return [];
      const phase = candidate as Record<string, unknown>;
      const tasks = (Array.isArray(phase.tasks) ? phase.tasks : [])
        .slice(0, remainingTasks)
        .flatMap((item) => {
          if (!isRecord(item)) return [];
          const task = item as Record<string, unknown>;
          const status = String(task.status || '');
          const content = String(task.content || '').slice(0, 4_096);
          if (!content || !['pending', 'in_progress', 'completed', 'abandoned'].includes(status)) return [];
          return [{
            id: String(task.id || '').slice(0, 160) || undefined,
            content,
            status: status as OmoTodoState['phases'][number]['tasks'][number]['status'],
          }];
        });
      remainingTasks -= tasks.length;
      return [{
        name: String(phase.name || '').slice(0, 4_096) || 'Plan',
        tasks,
      }];
    });
  return {
    available: state.available === true,
    reason_code: String(state.reason_code || '').slice(0, 80) || undefined,
    session_id: String(state.session_id || '').slice(0, 160) || undefined,
    version: parseWireSafeInteger(state.version),
    updated_at: String(state.updated_at || '').slice(0, 80) || undefined,
    phases,
    truncated: state.truncated === true,
  };
}

const BROWSE_CURSOR_LIMIT = 2_048;

function conversationCapabilities(value: unknown): import('./types').ConversationCapabilities | undefined {
  if (value == null) return undefined;
  const raw = browseRecord(value, true)!;
  if (!['pi_events', 'recorded_history'].includes(raw.source)) invalidBrowse();
  return { source: raw.source, active_branch: browseBoolean(raw.active_branch) };
}

export function normalizeConversationBatch(value: unknown, editDiffs = false): import('./types').ConversationBatch {
  const raw = browseRecord(value, true)!;
  if (!['batch', 'reset', 'heartbeat'].includes(raw.kind) || !Array.isArray(raw.remove) || raw.remove.length > 200) invalidBrowse();
  const entries = Array.isArray(raw.entries) ? raw.entries.map((entry) => {
    if (!Array.isArray(entry?.tools)) return entry;
    return { ...entry, tools: entry.tools.map((tool: unknown) => {
      const record = browseRecord(tool);
      if (!record || !('diff' in record)) return tool;
      return { ...record, diff: toolEditDiff(record, editDiffs) };
    }) };
  }) : raw.entries;
  if (encoder.encode(JSON.stringify({ ...raw, entries })).length > 2 * 1024 * 1024) {
    if (editDiffs) return normalizeConversationBatch(value, false);
    invalidBrowse();
  }
  const page = normalizeConversationPage({ available: true, has_more: false, entries: entries || [], state: 'ready', mode: 'recent', source_revision: raw.source_revision }, editDiffs);
  return { subscriptionId: browseString(raw.subscription_id, 128, true), kind: raw.kind, sourceRevision: page.sourceRevision || '', sequence: browseCount(raw.sequence), entries: page.entries, remove: raw.remove.map((id: unknown) => browseString(id, 256, true)), state: browseString(raw.state, 32), capabilities: conversationCapabilities(raw.capabilities) };
}

function invalidBrowse(): never {
  throw new CommandError('Relay returned invalid conversation history');
}

function browseString(value: unknown, max: number, required = false): string {
  if (value == null && !required) return '';
  if (typeof value !== 'string' || value.length > max || (required && !value)) invalidBrowse();
  return value;
}

function browseText(value: unknown, max: number): string {
  return typeof value === 'string' ? value.slice(0, max) : '';
}

function browseIdentity(value: unknown, max: number): string {
  return typeof value === 'string' && value.length <= max ? value : '';
}

function browseCount(value: unknown): number {
  if (value == null) return 0;
  const count = Number(value);
  if (!Number.isSafeInteger(count) || count < 0) invalidBrowse();
  return count;
}

function browseRecord(value: unknown, required = false): Record<string, any> | null {
  const record = isRecord(value) ? value as Record<string, any> : null;
  if (!record && required) invalidBrowse();
  return record;
}

function browseBoolean(value: unknown, optional = false): boolean {
  if (value == null && optional) return false;
  if (typeof value !== 'boolean') invalidBrowse();
  return value;
}

const encoder = new TextEncoder();

function toolEditDiff(tool: Record<string, unknown>, enabled: boolean): ConversationEditDiff | undefined {
  return enabled && tool.truncated !== true ? normalizeEditDiff(tool.diff) : undefined;
}

function normalizeEditDiff(value: unknown): ConversationEditDiff | undefined {
  const diff = browseRecord(value);
  if (!diff) return;
  const { kind, path, added, removed, text, truncated } = diff;
  if ((kind !== 'edit' && kind !== 'write') || typeof path !== 'string' || !path.trim() || path.length > 1024
    || typeof text !== 'string' || text.length > 8192 || typeof truncated !== 'boolean'
    || !Number.isSafeInteger(added) || added < 0 || !Number.isSafeInteger(removed) || removed < 0
    || /\p{Cc}|\p{Cs}/u.test(path) || /[^\P{Cc}\n\t]|\p{Cs}/u.test(text)
    || encoder.encode(path).length > 1024 || encoder.encode(text).length > 8192) return;
  if (!text) return kind === 'write' && added === 0 && removed === 0 && !truncated ? { kind, path, added, removed, text, truncated } : undefined;
  const rows = text.split('\n');
  if (rows.length > 200) return;
  let plus = 0;
  let minus = 0;
  let gap = true;
  for (const row of rows) {
    if (row[0] === '+') plus++;
    else if (row[0] === '-') minus++;
    else if (row !== '@@' || gap) return;
    gap = row === '@@';
  }
  if (gap || (kind === 'write' && removed !== 0) || plus > added || minus > removed || (!truncated && (plus !== added || minus !== removed))) return;
  return { kind, path, added, removed, text, truncated };
}

export function normalizeConversationPage(value: unknown, editDiffs = false): ConversationPage {
  const data = browseRecord(value, true)!;
  if (typeof data.available !== 'boolean' || typeof data.has_more !== 'boolean' || !Array.isArray(data.entries) || data.entries.length > 200) invalidBrowse();
  const entries = data.entries.flatMap((candidate: unknown) => {
    const entry = browseRecord(candidate);
    if (!entry || (entry.role !== 'user' && entry.role !== 'assistant')) return [];
    const text = browseText(entry.text, 1_048_576);
    const tools = (Array.isArray(entry.tools) ? entry.tools : []).slice(0, 128).flatMap((candidateTool: unknown) => {
      const tool = browseRecord(candidateTool);
      if (!tool) return [];
      const diff = toolEditDiff(tool, editDiffs);
      return [{
        ...(diff ? { diff } : {}),
        id: browseIdentity(tool.id, 256), name: browseText(tool.name, 160) || 'Tool',
        input: browseText(tool.input, 1_048_576), output: browseText(tool.output, 1_048_576),
        ...(tool.state === 'running' || tool.state === 'completed' || tool.state === 'failed' ? { state: tool.state } : {}),
        ...(typeof tool.parent_id === 'string' ? { parentId: browseIdentity(tool.parent_id, 256) } : {}),
        error: tool.error === true, truncated: tool.truncated === true,
      }];
    });
    const normalized = {
      ...(entry.kind === 'compaction' || entry.kind === 'notice' ? { kind: entry.kind } : {}),
      id: browseIdentity(entry.id, 256), timestamp: browseText(entry.timestamp, 128),
      role: entry.role, text, tools, truncated: entry.truncated === true,
    };
    return normalized.id && (text || tools.length) ? [normalized] : [];
  });
  const cursor = browseString(data.next_cursor, BROWSE_CURSOR_LIMIT);
  const state = browseString(data.state, 32, true);
  const mode = browseString(data.mode, 32, true);
  if (!['ready', 'preparing', 'failed'].includes(state) || !['recent', 'snapshot', 'native'].includes(mode)
    || (data.has_more && !cursor) || (state === 'preparing' && !cursor)) invalidBrowse();
  const progress = data.progress == null ? undefined : (() => {
    const raw = browseRecord(data.progress, true)!;
    const scanned = browseCount(raw.scanned_bytes);
    const source = browseCount(raw.source_bytes);
    if (scanned > source) invalidBrowse();
    return { phase: browseString(raw.phase, 32, true), scanned_bytes: scanned, source_bytes: source };
  })();
  const rawDiagnostics = data.diagnostics == null ? {} : browseRecord(data.diagnostics, true)!;
  const continuationIncomplete = browseBoolean(rawDiagnostics.continuation_incomplete, true);
  const continuationReason = rawDiagnostics.continuation_reason == null
    ? undefined
    : browseString(rawDiagnostics.continuation_reason, 64);
  const continuationReasons = new Set(['missing_source', 'invalid_link', 'ambiguous_link', 'cycle', 'resolution_limit', 'partial_link']);
  if (continuationIncomplete && (!continuationReason || !continuationReasons.has(continuationReason))) invalidBrowse();
  if (!continuationIncomplete && continuationReason) invalidBrowse();
  const diagnostics = {
    oversized_records: browseCount(rawDiagnostics.oversized_records), corrupt_records: browseCount(rawDiagnostics.corrupt_records),
    omitted_tools: browseCount(rawDiagnostics.omitted_tools), omitted_payloads: browseCount(rawDiagnostics.omitted_payloads),
    plan_corrupt: browseBoolean(rawDiagnostics.plan_corrupt, true),
    source_truncated: browseBoolean(rawDiagnostics.source_truncated, true),
    continuation_incomplete: continuationIncomplete,
    continuation_reason: continuationReason,
  };
  const rawError = data.error == null ? null : browseRecord(data.error, true)!;
  const error = rawError ? {
    code: browseString(rawError.code, 64, true), message: browseString(rawError.message, 512, true),
    retryable: browseBoolean(rawError.retryable),
  } : undefined;
  if (state === 'failed' && !error) invalidBrowse();
  if (data.provisional != null && (!Array.isArray(data.provisional) || data.provisional.length > 16)) invalidBrowse();
  return {
    subscriptionId: browseString(data.subscription_id, 128),
    provisional: data.provisional ? normalizeConversationPage({ available: true, has_more: false, entries: data.provisional, mode: 'recent', state: 'ready' }, editDiffs).entries : undefined,
    capabilities: conversationCapabilities(data.capabilities), sequence: browseCount(data.sequence), liveState: browseString(data.live_state, 32),
    available: data.available, reason: browseString(data.reason, 512), reasonCode: browseString(data.reason_code, 64) || undefined, entries,
    nextCursor: cursor, hasMore: data.has_more, total: data.total == null ? null : browseCount(data.total),
    state: state as ConversationPage['state'], mode: mode as ConversationPage['mode'],
    sourceRevision: browseString(data.source_revision, 256), snapshotId: browseString(data.snapshot_id, 256), progress, diagnostics, error,
    omoPlan: normalizeOmoTodoState(data.omo_plan),
  };
}
