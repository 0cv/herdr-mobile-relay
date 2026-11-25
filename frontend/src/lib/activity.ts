import type { Activity } from './types';
import type { StatusAppearance } from './status-marks';

export function activityMatchesSearch(activity: Partial<Activity>, query: string): boolean {
  if (!query) return true;
  const details = activity.details && typeof activity.details === 'object' ? Object.values(activity.details) : [];
  return [
    activity.summary, activity.kind, activity.status, activity.relay_label,
    activity.project, activity.session, activity.agent, activity.host,
    activity.pane_id, activity.request_id, activity.extract, ...details,
  ]
    .filter(Boolean)
    .join(' ')
    .toLowerCase()
    .includes(query.toLowerCase());
}

export function activityForNotification<T extends Pick<Activity, 'details'>>(
  activities: T[],
  notificationId: string,
): T | null {
  if (!notificationId) return null;
  return activities.find((activity) => activity.details
    && (activity.details as { event_id?: string }).event_id === notificationId) || null;
}

export function activityMark(status: unknown): StatusAppearance {
  const value = String(status || '').toLowerCase();
  if (value === 'failed') return { shape: 'square', tone: 'danger' };
  if (['attention', 'unconfirmed', 'completed_with_warning', 'warning'].includes(value)) return { shape: 'triangle', tone: 'warning' };
  if (value === 'working') return { shape: 'chevron', tone: 'warning' };
  if (value === 'idle' || value === 'ready') return { shape: 'ring', tone: 'success' };
  if (['confirmed', 'completed', 'done'].includes(value)) return { shape: 'check', tone: 'success' };
  return { shape: 'dash', tone: 'muted' };
}
