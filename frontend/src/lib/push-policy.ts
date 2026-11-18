export const PUSH_CATEGORIES = [
  'attention',
  'question',
  'brief',
  'finished',
  'update',
  'test',
] as const;

export type PushCategory = typeof PUSH_CATEGORIES[number];

export interface DevicePushPolicy {
  device_id: string;
  locale: string;
  categories: Record<PushCategory, boolean>;
  settle_ms: number;
  cooldown_ms: number;
  snoozed: boolean;
  snooze_until?: string;
  update_once: boolean;
}

export interface NotificationPolicyScope {
  relay_id: string;
  relay_label: string;
  device_id: string;
  device_label: string;
  current_device: boolean;
  policy: DevicePushPolicy;
}

export interface PushPolicyChange {
  relay_id: string;
  device_id: string;
  policy: DevicePushPolicy;
}

export interface PushTestRequest {
  relay_id: string;
}

export type PushTestState =
  | { status: 'idle' }
  | { status: 'sending' }
  | { status: 'accepted'; result: 'accepted' | 'queued' }
  | { status: 'rejected'; code: string };

export interface NotificationPlatformInfo {
  platform: 'ios' | 'android' | 'other';
  installed: boolean;
  supports_push: boolean;
  permission: NotificationPermission | 'unavailable';
}

export const DEFAULT_PUSH_CATEGORIES: Record<PushCategory, boolean> = {
  attention: true,
  question: true,
  brief: true,
  finished: false,
  update: true,
  test: true,
};

export function defaultDevicePushPolicy(deviceId: string, locale = 'en'): DevicePushPolicy {
  return {
    device_id: deviceId,
    locale,
    categories: { ...DEFAULT_PUSH_CATEGORIES },
    settle_ms: 2_000,
    cooldown_ms: 30_000,
    snoozed: false,
    update_once: true,
  };
}

export function pushPolicyScopeKey(relayId: string, deviceId: string): string {
  return `${encodeURIComponent(relayId)}::${encodeURIComponent(deviceId)}`;
}

export function withTimedSnooze(policy: DevicePushPolicy, durationMs: number, now = new Date()): DevicePushPolicy {
  if (!Number.isFinite(durationMs) || durationMs <= 0) return clearSnooze(policy);
  return {
    ...policy,
    snoozed: true,
    snooze_until: new Date(now.getTime() + durationMs).toISOString(),
  };
}

export function withGlobalSnooze(policy: DevicePushPolicy): DevicePushPolicy {
  const updated = { ...policy, snoozed: true };
  delete updated.snooze_until;
  return updated;
}

export function clearSnooze(policy: DevicePushPolicy): DevicePushPolicy {
  const updated = { ...policy, snoozed: false };
  delete updated.snooze_until;
  return updated;
}
