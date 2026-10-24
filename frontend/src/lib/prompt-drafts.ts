import type { Agent, FrontendTargetRef } from './types';
import { normalizeFrontendTargetRef, targetRefForAgent } from './resource-id';

const DRAFT_PREFIX = 'herdr_prompt_draft_v1:';
const DRAFT_VERSION = 1;
const DRAFT_MAX_AGE_MS = 48 * 60 * 60 * 1_000;
const DRAFT_MAX_BYTES = 64 * 1_024;
const DRAFT_MAX_ENTRIES = 64;

interface PromptDraftRecord {
  version: number;
  identity: string;
  text: string;
  updatedAt: number;
}

/**
 * Newest live value per pane identity. Oversize drafts and drafts written
 * while storage is unavailable never reach localStorage, so this tier is what
 * keeps them alive across the remount a pane switch performs. Bounded like the
 * persisted set; lost on reload, which the composer warning states.
 */
const memoryDrafts = new Map<string, { text: string; updatedAt: number }>();

function readMemoryDraft(identity: string, now: number): string {
  const draft = memoryDrafts.get(identity);
  if (!draft) return '';
  if (now - draft.updatedAt > DRAFT_MAX_AGE_MS) {
    memoryDrafts.delete(identity);
    return '';
  }
  return draft.text;
}

function writeMemoryDraft(identity: string, text: string, updatedAt: number): void {
  memoryDrafts.delete(identity);
  memoryDrafts.set(identity, { text, updatedAt });
  while (memoryDrafts.size > DRAFT_MAX_ENTRIES) {
    const oldest = memoryDrafts.keys().next();
    if (oldest.done) return;
    memoryDrafts.delete(oldest.value);
  }
}

export type PromptDraftSaveResult = 'saved' | 'cleared' | 'too-large' | 'unavailable';

export function promptDraftTarget(value: unknown): FrontendTargetRef | null {
  const target = normalizeFrontendTargetRef(value);
  const session = target?.agent_session_id;
  if (typeof session !== 'string' || !session.trim()
    || /[\uD800-\uDFFF]/u.test(session)) return null;
  return target;
}

export function promptComposerScope(agent: Agent): string {
  return JSON.stringify([
    agent.relay_id, agent.server_session_id, agent.raw_pane_id, agent.pane_id,
    agent.terminal_id, agent.generation, agent.agent_session_id, agent.agent, agent.cwd,
  ]);
}

export function promptDraftIdentity(agent: Agent): string | null {
  const target = promptDraftTarget(targetRefForAgent(agent));
  if (!target) return null;
  return JSON.stringify([
    target,
    String(agent.agent || ''),
    String(agent.cwd || ''),
  ]);
}

function promptDraftKey(identity: string): string {
  return `${DRAFT_PREFIX}${encodeURIComponent(identity)}`;
}

function parseDraft(raw: string | null): PromptDraftRecord | null {
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as Partial<PromptDraftRecord>;
    if (parsed.version !== DRAFT_VERSION
      || typeof parsed.identity !== 'string'
      || typeof parsed.text !== 'string'
      || typeof parsed.updatedAt !== 'number'
      || !Number.isFinite(parsed.updatedAt)
      || parsed.updatedAt <= 0) return null;
    return parsed as PromptDraftRecord;
  } catch {
    return null;
  }
}

export function loadPromptDraft(agent: Agent, now = Date.now()): string {
  const identity = promptDraftIdentity(agent);
  if (!identity) return '';
  const key = promptDraftKey(identity);
  const live = readMemoryDraft(identity, now);
  if (live) return live;
  try {
    const draft = parseDraft(localStorage.getItem(key));
    if (!draft || draft.identity !== identity || now - draft.updatedAt > DRAFT_MAX_AGE_MS) {
      localStorage.removeItem(key);
      return '';
    }
    return draft.text;
  } catch {
    return '';
  }
}

export function savePromptDraft(agent: Agent, text: string, now = Date.now()): PromptDraftSaveResult {
  const identity = promptDraftIdentity(agent);
  if (!identity) return 'unavailable';
  const key = promptDraftKey(identity);
  if (text) writeMemoryDraft(identity, text, now);
  else memoryDrafts.delete(identity);
  try {
    if (!text) {
      localStorage.removeItem(key);
      return 'cleared';
    }
    if (new TextEncoder().encode(text).byteLength > DRAFT_MAX_BYTES) {
      // An oversize draft never reaches storage, so any shorter earlier record
      // for this pane would be served back as the current draft on remount.
      localStorage.removeItem(key);
      return 'too-large';
    }
    const draft: PromptDraftRecord = { version: DRAFT_VERSION, identity, text, updatedAt: now };
    localStorage.setItem(key, JSON.stringify(draft));
    prunePromptDrafts(now);
    return 'saved';
  } catch {
    return 'unavailable';
  }
}

export function clearPromptDraft(agent: Agent): void {
  const identity = promptDraftIdentity(agent);
  if (!identity) return;
  memoryDrafts.delete(identity);
  try {
    localStorage.removeItem(promptDraftKey(identity));
  } catch {
    // Storage can be unavailable in browser private modes; the live composer still clears normally.
  }
}

export function prunePromptDrafts(now = Date.now()): void {
  try {
    const drafts: Array<{ key: string; updatedAt: number }> = [];
    for (let index = 0; index < localStorage.length; index += 1) {
      const key = localStorage.key(index);
      if (!key?.startsWith(DRAFT_PREFIX)) continue;
      const draft = parseDraft(localStorage.getItem(key));
      if (!draft || now - draft.updatedAt > DRAFT_MAX_AGE_MS) {
        localStorage.removeItem(key);
        index -= 1;
        continue;
      }
      drafts.push({ key, updatedAt: draft.updatedAt });
    }
    drafts.sort((left, right) => right.updatedAt - left.updatedAt);
    for (const draft of drafts.slice(DRAFT_MAX_ENTRIES)) localStorage.removeItem(draft.key);
  } catch {
    // Persistence is best-effort. The current textarea remains the source of truth.
  }
}
