import type { Agent, FrontendTargetRef, TargetRef } from './types';

const ROUTE_PREFIX = 'r3.';
const ID_PATTERN = /^[A-Za-z0-9._:@%+-]{1,160}$/u;
const TARGET_TUPLE_LENGTH = 7;

type TargetTuple = [3, string, string, string, string, number, string];

export function isResourceId(value: unknown): value is string {
  return typeof value === 'string' && ID_PATTERN.test(value);
}

function validGeneration(value: unknown): value is number {
  return Number.isSafeInteger(value) && Number(value) >= 0;
}
function validAgentSessionId(value: unknown): value is string {
  return typeof value === 'string'
    && value.length > 0
    && value.length <= 2048
    && !/[\u0000-\u001f\u007f]/u.test(value);
}

function normalizedTarget(value: unknown, frontend: true): FrontendTargetRef | null;
function normalizedTarget(value: unknown, frontend?: false): TargetRef | null;
function normalizedTarget(value: unknown, frontend = false): TargetRef | FrontendTargetRef | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const candidate = value as Record<string, unknown>;
  if (frontend && !isResourceId(candidate.relay_id)) return null;
  if (!isResourceId(candidate.server_session_id)
    || !isResourceId(candidate.pane_id)
    || !isResourceId(candidate.terminal_id)
    || !validGeneration(candidate.generation)
    || candidate.agent_session_id !== undefined
      && candidate.agent_session_id !== ''
      && !validAgentSessionId(candidate.agent_session_id)) return null;
  return {
    ...(frontend ? { relay_id: candidate.relay_id as string } : {}),
    server_session_id: candidate.server_session_id,
    pane_id: candidate.pane_id,
    terminal_id: candidate.terminal_id,
    generation: candidate.generation,
    ...(candidate.agent_session_id ? { agent_session_id: candidate.agent_session_id } : {}),
  } as TargetRef | FrontendTargetRef;
}

export function normalizeTargetRef(value: unknown): TargetRef | null {
  return normalizedTarget(value);
}

export function normalizeFrontendTargetRef(value: unknown): FrontendTargetRef | null {
  return normalizedTarget(value, true);
}

export function targetRefForAgent(agent: Partial<Agent>): FrontendTargetRef | null {
  return normalizeFrontendTargetRef({
    relay_id: agent.relay_id,
    server_session_id: agent.server_session_id,
    pane_id: agent.raw_pane_id,
    terminal_id: agent.terminal_id,
    generation: agent.generation,
    agent_session_id: agent.agent_session_id,
  });
}
export function targetRefMatchesAgent(target: FrontendTargetRef, agent: Partial<Agent>): boolean {
  const normalized = normalizeFrontendTargetRef(target);
  const current = targetRefForAgent(agent);
  return Boolean(normalized
    && current
    && normalized.relay_id === current.relay_id
    && normalized.server_session_id === current.server_session_id
    && normalized.pane_id === current.pane_id
    && normalized.terminal_id === current.terminal_id
    && normalized.generation === current.generation
    && (normalized.agent_session_id || '') === (current.agent_session_id || ''));
}

export function targetStoreKey(target: FrontendTargetRef): string | null {
  const normalized = normalizeFrontendTargetRef(target);
  if (!normalized) return null;
  return JSON.stringify([
    normalized.relay_id,
    normalized.server_session_id,
    normalized.pane_id,
    normalized.terminal_id,
    normalized.generation,
    normalized.agent_session_id || '',
  ]);
}

export function encodeTargetRoute(target: FrontendTargetRef): string | null {
  const normalized = normalizeFrontendTargetRef(target);
  if (!normalized) return null;
  const tuple: TargetTuple = [
    3,
    normalized.relay_id,
    normalized.server_session_id,
    normalized.pane_id,
    normalized.terminal_id,
    normalized.generation,
    normalized.agent_session_id || '',
  ];
  const bytes = new TextEncoder().encode(JSON.stringify(tuple));
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return ROUTE_PREFIX + btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/u, '');
}

export function decodeTargetRoute(token: string): FrontendTargetRef | null {
  if (typeof token !== 'string' || !token.startsWith(ROUTE_PREFIX)) return null;
  const encoded = token.slice(ROUTE_PREFIX.length);
  if (!encoded || !/^[A-Za-z0-9_-]+$/u.test(encoded)) return null;
  try {
    const padded = encoded.replaceAll('-', '+').replaceAll('_', '/') + '='.repeat((4 - encoded.length % 4) % 4);
    const binary = atob(padded);
    const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
    const tuple = JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes)) as unknown;
    if (!Array.isArray(tuple) || tuple.length !== TARGET_TUPLE_LENGTH || tuple[0] !== 3) return null;
    return normalizeFrontendTargetRef({
      relay_id: tuple[1],
      server_session_id: tuple[2],
      pane_id: tuple[3],
      terminal_id: tuple[4],
      generation: tuple[5],
      agent_session_id: tuple[6],
    });
  } catch {
    return null;
  }
}
