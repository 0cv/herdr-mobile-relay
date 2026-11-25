import type { RelayConnectionView } from './types';

export type StatusAppearance = {
  shape: 'square' | 'triangle' | 'chevron' | 'check' | 'ring' | 'dash';
  tone: 'danger' | 'warning' | 'success' | 'muted';
};

export const connectionMarks = {
  connected: { shape: 'check', tone: 'success' },
  loading: { shape: 'chevron', tone: 'warning' },
  degraded: { shape: 'triangle', tone: 'warning' },
  disconnected: { shape: 'square', tone: 'danger' },
} as const satisfies Record<string, StatusAppearance>;

export function relayStatusMark(connection?: RelayConnectionView | null) {
  if (connection?.status === 'connecting') {
    return { ...connectionMarks.loading, label: 'connecting' };
  }
  if (connection?.status !== 'connected') {
    return { ...connectionMarks.disconnected, label: 'disconnected' };
  }
  if (connection.inventory.state === 'starting') {
    return { ...connectionMarks.loading, label: 'connected; agent inventory loading' };
  }
  if (connection.inventory.state === 'error') {
    return { ...connectionMarks.degraded, label: 'connected; agent inventory unavailable' };
  }
  return { ...connectionMarks.connected, label: 'connected' };
}
