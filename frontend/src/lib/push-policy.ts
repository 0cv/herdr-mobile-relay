export const PUSH_CATEGORIES = [
  'attention',
  'question',
  'brief',
  'finished',
  'update',
  'test',
] as const;

export type PushCategory = typeof PUSH_CATEGORIES[number];
export type PushPreviewMode = 'hidden' | 'question' | 'brief';

export interface PushEventKey {
  device_id: string;
  server_session_id: string;
  pane_id: string;
  agent_session_id: string;
  terminal_id: string;
  generation: number;
  event_id: string;
  interaction_revision: number;
  category: PushCategory;
}

export interface PushTargetRef {
  relay_id: string;
  server_session_id: string;
  pane_id: string;
  terminal_id: string;
  generation: number;
  agent_session_id: string;
}

export interface ViewedPaneSuppressionInput extends PushTargetRef {
  visible: boolean;
  unlocked: boolean;
}

export interface DevicePushPolicy {
  device_id: string;
  locale: string;
  categories: Record<PushCategory, boolean>;
  settle_ms: number;
  cooldown_ms: number;
  snoozed: boolean;
  snooze_until?: string;
  update_once: boolean;
  last_update_version?: string;
}

export interface NotificationPolicyScope {
  relay_id: string;
  relay_label: string;
  device_id: string;
  device_label: string;
  current_device: boolean;
  policy: DevicePushPolicy;
}

export interface PushPolicyEvaluationInput {
  relay_id: string;
  key: PushEventKey;
  policy: DevicePushPolicy;
  viewed_pane?: ViewedPaneSuppressionInput | null;
  update_version?: string;
  now?: Date;
}

export type PushSuppressionReason =
  | 'allowed'
  | 'wrong_device'
  | 'category_disabled'
  | 'snoozed'
  | 'viewed_pane'
  | 'update_already_seen';

export interface PushPolicyEvaluation {
  deliver: boolean;
  reason: PushSuppressionReason;
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

export function validPushEventKey(value: unknown): value is PushEventKey {
  if (!value || typeof value !== 'object') return false;
  const key = value as Record<string, unknown>;
  return typeof key.device_id === 'string' && key.device_id.length > 0
    && typeof key.server_session_id === 'string' && key.server_session_id.length > 0
    && typeof key.pane_id === 'string' && key.pane_id.length > 0
    && typeof key.terminal_id === 'string' && key.terminal_id.length > 0
    && Number.isSafeInteger(key.generation) && Number(key.generation) >= 0
    && typeof key.event_id === 'string' && key.event_id.length > 0
    && Number.isSafeInteger(key.interaction_revision) && Number(key.interaction_revision) >= 0
    && PUSH_CATEGORIES.includes(key.category as PushCategory);
}

export function pushNotificationTag(key: PushEventKey): string {
  return [
    key.device_id,
    key.server_session_id,
    key.pane_id,
    key.terminal_id,
    String(key.generation),
    key.event_id,
    String(key.interaction_revision),
    key.category,
  ].map((part) => encodeURIComponent(part)).join('|');
}

export function exactViewedPaneMatch(
  relayId: string,
  key: PushEventKey,
  viewedPane?: ViewedPaneSuppressionInput | null,
): boolean {
  return Boolean(viewedPane?.visible
    && viewedPane.unlocked
    && viewedPane.relay_id === relayId
    && viewedPane.server_session_id === key.server_session_id
    && viewedPane.pane_id === key.pane_id
    && viewedPane.terminal_id === key.terminal_id
    && viewedPane.generation === key.generation);
}

export function evaluatePushPolicy(input: PushPolicyEvaluationInput): PushPolicyEvaluation {
  const { key, policy } = input;
  if (key.device_id !== policy.device_id) return { deliver: false, reason: 'wrong_device' };
  if (!policy.categories[key.category]) return { deliver: false, reason: 'category_disabled' };

  if (policy.snoozed) {
    if (!policy.snooze_until) return { deliver: false, reason: 'snoozed' };
    const until = Date.parse(policy.snooze_until);
    if (Number.isFinite(until) && until > (input.now || new Date()).getTime()) {
      return { deliver: false, reason: 'snoozed' };
    }
  }

  if (exactViewedPaneMatch(input.relay_id, key, input.viewed_pane)) {
    return { deliver: false, reason: 'viewed_pane' };
  }
  if (key.category === 'update'
    && policy.update_once
    && input.update_version
    && input.update_version === policy.last_update_version) {
    return { deliver: false, reason: 'update_already_seen' };
  }
  return { deliver: true, reason: 'allowed' };
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

export function recordUpdateNotification(policy: DevicePushPolicy, version: string): DevicePushPolicy {
  if (!policy.update_once || !version) return policy;
  return { ...policy, last_update_version: version };
}
