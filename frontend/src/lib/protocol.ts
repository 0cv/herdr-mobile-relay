import type {
  ActionReceipt,
  ActionReceiptPhase,
  ApiError,
  ApiErrorArg,
  FrontendTargetRef,
  NotificationTarget,
  RelayConnectionView,
} from './types';
import { normalizeFrontendTargetRef } from './resource-id';

export const RELAY_PROTOCOL_VERSION = 3;

const ACTION_RECEIPT_PHASES: Record<ActionReceiptPhase, true> = {
  prepared: true,
  failed_before_dispatch: true,
  awaiting_evidence: true,
  confirmed: true,
  dispatched_unknown: true,
};
const API_ERROR_CODE = /^[a-z0-9_]{1,64}$/u;
const API_ERROR_ARG_KEY = /^[a-z0-9_]{1,32}$/u;
const ACTION_ID = /^[A-Za-z0-9_-]{8,128}$/u;
const encoder = new TextEncoder();

export function parseApiError(value: unknown): ApiError | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const candidate = value as Record<string, unknown>;
  if (typeof candidate.code !== 'string' || !API_ERROR_CODE.test(candidate.code)) return null;
  if (candidate.args === undefined) return { code: candidate.code };
  if (!candidate.args || typeof candidate.args !== 'object' || Array.isArray(candidate.args)) return null;
  const entries = Object.entries(candidate.args);
  if (entries.length > 8) return null;
  const args: Record<string, ApiErrorArg> = {};
  for (const [key, argument] of entries) {
    if (!API_ERROR_ARG_KEY.test(key)) return null;
    if (typeof argument === 'string') {
      if (encoder.encode(argument).byteLength > 256) return null;
      args[key] = argument;
    } else if (typeof argument === 'boolean' || Number.isSafeInteger(argument)) {
      args[key] = argument as number | boolean;
    } else return null;
  }
  if (encoder.encode(JSON.stringify(args)).byteLength > 1024) return null;
  return { code: candidate.code, args };
}

export function parseActionReceipt(value: unknown): ActionReceipt | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const envelope = value as Record<string, unknown>;
  const candidate = envelope.type === 'action_receipt' ? envelope.receipt : value;
  if (!candidate || typeof candidate !== 'object' || Array.isArray(candidate)) return null;
  const receipt = candidate as Record<string, unknown>;
  if (typeof receipt.action_id !== 'string' || !ACTION_ID.test(receipt.action_id)) return null;
  if (typeof receipt.phase !== 'string' || !ACTION_RECEIPT_PHASES[receipt.phase as ActionReceiptPhase]) return null;
  const error = receipt.error === undefined ? undefined : parseApiError(receipt.error);
  if (receipt.error !== undefined && !error) return null;
  return {
    action_id: receipt.action_id,
    phase: receipt.phase as ActionReceiptPhase,
    ...(error ? { error } : {}),
  };
}

export function relayProtocolError(connection: Pick<RelayConnectionView, 'protocol'> | null | undefined): string {
  if (!connection?.protocol) return 'Waiting for the relay protocol handshake.';
  if (connection.protocol === RELAY_PROTOCOL_VERSION) return '';
  return `Incompatible relay protocol v${connection.protocol}; this app requires v${RELAY_PROTOCOL_VERSION}.`;
}

/**
 * Git revisions are 40 characters and only the leading few carry meaning, so a
 * phone row shows the short form and the tooltip keeps the full hash. Anything
 * that is not a hash — a bare version, a placeholder — is left alone, and a
 * `-dirty` working tree stays marked.
 */
export function shortRevision(revision: string): string {
  const dirty = revision.endsWith('-dirty');
  const base = dirty ? revision.slice(0, -6) : revision;
  if (!/^[0-9a-f]{12,40}$/i.test(base)) return revision;
  return dirty ? `${base.slice(0, 7)}-dirty` : base.slice(0, 7);
}

export function relayVersionMeta(
  connection: (
    Pick<RelayConnectionView, 'status' | 'protocol' | 'version'>
    & Partial<Pick<RelayConnectionView, 'releaseVersion' | 'revision'>>
  ) | null | undefined,
) {
  if (!connection || connection.status !== 'connected' || !connection.protocol) return null;
  if (connection.protocol < RELAY_PROTOCOL_VERSION) {
    return {
      label: '⚠ Relay outdated — update this computer',
      tone: 'warning' as const,
      title: `This relay speaks protocol v${connection.protocol} but the app expects v${RELAY_PROTOCOL_VERSION}. On that computer: git pull, then restart the relay.`,
    };
  }
  if (connection.protocol > RELAY_PROTOCOL_VERSION) {
    return {
      label: '⚠ App outdated — update the app',
      tone: 'warning' as const,
      title: `This relay speaks protocol v${connection.protocol} but the app only knows v${RELAY_PROTOCOL_VERSION}. Reload the app, or redeploy it if separately hosted.`,
    };
  }
  if (!connection.version || connection.version === 'unknown') return null;
  const revision = connection.revision || connection.version;
  if (connection.releaseVersion) {
    return {
      label: `v${connection.releaseVersion} · ${shortRevision(revision)}`,
      tone: 'muted' as const,
      title: `Relay product version and Git revision ${revision}.`,
    };
  }
  return {
    label: `relay ${shortRevision(revision)}`,
    tone: 'muted' as const,
    title: `Relay Git revision ${revision}.`,
  };
}

export function parseNotificationTarget(value: string): NotificationTarget | null {
  try {
    const parsed = JSON.parse(decodeURIComponent(value)) as Record<string, unknown>;
    const paneId = String(parsed?.pane_id || '').trim();
    const host = String(parsed?.host || '').trim();
    const action = parsed?.action === 'approve' || parsed?.action === 'deny' ? parsed.action : '';
    const index = Number.isInteger(parsed?.index) ? Number(parsed.index) : null;
    const total = Number.isInteger(parsed?.total) ? Number(parsed.total) : null;
    const notificationId = String(parsed?.notification_id || '').trim().slice(0, 120);
    if (!paneId) return null;
    if (action && (index === null || total === null || index < 0 || index >= total || total < 2 || total > 20)) {
      return null;
    }
    return { pane_id: paneId, host, action, index, total, notification_id: notificationId };
  } catch {
    return null;
  }
}

export function parsePushOpenTarget(value: unknown, relayId: string): FrontendTargetRef | null {
  if (!value || typeof value !== 'object' || Array.isArray(value) || !relayId) return null;
  const target = value as Record<string, unknown>;
  return normalizeFrontendTargetRef({
    relay_id: relayId,
    server_session_id: target.server_session_id,
    pane_id: target.pane_id,
    terminal_id: target.terminal_id,
    generation: target.generation,
    agent_session_id: target.agent_session_id,
  });
}
