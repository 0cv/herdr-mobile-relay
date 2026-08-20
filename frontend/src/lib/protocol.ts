import { APP_PROTOCOL_VERSION } from './config';
import type { NotificationTarget, RelayConnectionView } from './types';

export function relayProtocolError(connection: Pick<RelayConnectionView, 'protocol'> | null | undefined): string {
  if (!connection?.protocol) return 'Waiting for the relay protocol handshake.';
  if (connection.protocol === APP_PROTOCOL_VERSION) return '';
  return `Incompatible relay protocol v${connection.protocol}; this app requires v${APP_PROTOCOL_VERSION}.`;
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
  if (connection.protocol < APP_PROTOCOL_VERSION) {
    return {
      label: '⚠ Relay outdated — update this computer',
      tone: 'warning' as const,
      title: `This relay speaks protocol v${connection.protocol} but the app expects v${APP_PROTOCOL_VERSION}. On that computer: git pull, then restart the relay.`,
    };
  }
  if (connection.protocol > APP_PROTOCOL_VERSION) {
    return {
      label: '⚠ App outdated — update the app',
      tone: 'warning' as const,
      title: `This relay speaks protocol v${connection.protocol} but the app only knows v${APP_PROTOCOL_VERSION}. Reload the app, or redeploy it if separately hosted.`,
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
