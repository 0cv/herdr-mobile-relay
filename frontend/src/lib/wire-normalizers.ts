import { parseWireSafeInteger } from './protocol';
import { PUSH_CATEGORIES, defaultDevicePushPolicy, type DevicePushPolicy } from './push-policy';
import { isRecord } from './record';
import { SLASH_COMMAND_MAX_ENTRIES } from './slash-command-limits';
import type {
  Agent,
  AgentInventoryStatus,
  HerdrStatus,
  RelayWorkspace,
  SlashCommand,
  SlashCommandCatalog,
} from './types';

export function normalizeAgentInventory(
  value: unknown,
  fallbackState: AgentInventoryStatus['state'] = 'starting',
): AgentInventoryStatus {
  const inventory = value && typeof value === 'object' ? value as Record<string, unknown> : {};
  const state = ['starting', 'ready', 'error'].includes(String(inventory.state))
    ? String(inventory.state) as AgentInventoryStatus['state']
    : fallbackState;
  return {
    state,
    errorCode: String(inventory.error_code || '').slice(0, 80),
    message: String(inventory.message || '').slice(0, 500),
    lastAttemptAt: Number(inventory.last_attempt_at) || 0,
    lastSuccessAt: Number(inventory.last_success_at) || 0,
    stale: inventory.stale === true,
  };
}
export function normalizeHerdrStatus(value: unknown): HerdrStatus {
  const status = isRecord(value)
    ? value as Record<string, unknown>
    : {};
  const features: Record<string, HerdrStatus['features'][string]> = {};
  const rawFeatures = isRecord(status.features)
    ? status.features as Record<string, unknown>
    : {};
  for (const [name, rawFeature] of Object.entries(rawFeatures).slice(0, 64)) {
    if (!isRecord(rawFeature)) continue;
    const feature = rawFeature as Record<string, unknown>;
    const state = String(feature.state || '');
    if (!['supported', 'unsupported', 'unknown'].includes(state)) continue;
    features[name.slice(0, 96)] = {
      state: state as HerdrStatus['features'][string]['state'],
      reason: String(feature.reason || '').slice(0, 160),
    };
  }
  const endpoint = Number(status.endpoint_protocol_generation);
  return {
    installed_client_version: String(status.installed_client_version || '').slice(0, 64),
    server_version: String(status.server_version || '').slice(0, 64),
    server_protocol: parseWireSafeInteger(status.server_protocol) ?? 0,
    server_protocol_known: status.server_protocol_known === true,
    endpoint_protocol_generation: Number.isSafeInteger(endpoint) && endpoint > 0 ? endpoint : null,
    generation: parseWireSafeInteger(status.generation) ?? 0,
    features,
  };
}

export function normalizeAgentTargetFields(agent: Partial<Agent>): Partial<Agent> {
  const generation = Number(agent.generation);
  return {
    ...agent,
    server_session_id: typeof agent.server_session_id === 'string' && agent.server_session_id.trim()
      ? agent.server_session_id.trim()
      : undefined,
    terminal_id: typeof agent.terminal_id === 'string' && agent.terminal_id.trim()
      ? agent.terminal_id.trim()
      : undefined,
    generation: Number.isSafeInteger(generation) && generation >= 0 ? generation : undefined,
    agent_session_id: typeof agent.agent_session_id === 'string' ? agent.agent_session_id : undefined,
  };
}

export function normalizeWorkspace(
  relayId: string,
  relayLabel: string,
  value: Record<string, unknown>,
): RelayWorkspace | null {
  const workspaceId = String(value.workspace_id || '');
  if (!workspaceId) return null;
  const worktreeValue = value.worktree && typeof value.worktree === 'object'
    ? value.worktree as Record<string, unknown>
    : null;
  return {
    relay_id: relayId,
    relay_label: relayLabel,
    workspace_id: workspaceId,
    number: Number(value.number) || 0,
    label: String(value.label || 'Workspace').slice(0, 256),
    focused: value.focused === true,
    pane_count: Number(value.pane_count) || 0,
    tab_count: Number(value.tab_count) || 0,
    active_tab_id: String(value.active_tab_id || ''),
    agent_status: String(value.agent_status || ''),
    cwd: String(value.cwd || ''),
    worktree: worktreeValue ? {
      repo_key: String(worktreeValue.repo_key || ''),
      repo_name: String(worktreeValue.repo_name || ''),
      repo_root: String(worktreeValue.repo_root || ''),
      checkout_path: String(worktreeValue.checkout_path || ''),
      is_linked_worktree: worktreeValue.is_linked_worktree === true,
    } : null,
  };
}

export function normalizePushPolicy(value: unknown): DevicePushPolicy | null {
  if (!value || typeof value !== 'object') return null;
  const record = value as Record<string, unknown>;
  const deviceId = String(record.device_id || '').trim();
  if (!deviceId) return null;
  const policy = defaultDevicePushPolicy(deviceId, String(record.locale || 'en'));
  const categories = record.categories && typeof record.categories === 'object'
    ? record.categories as Record<string, unknown>
    : {};
  for (const category of PUSH_CATEGORIES) {
    if (typeof categories[category] === 'boolean') policy.categories[category] = Boolean(categories[category]);
  }
  const settle = Number(record.settle_ms);
  const cooldown = Number(record.cooldown_ms);
  if (Number.isSafeInteger(settle) && settle >= 0) policy.settle_ms = settle;
  if (Number.isSafeInteger(cooldown) && cooldown >= 0) policy.cooldown_ms = cooldown;
  policy.snoozed = record.snoozed === true;
  policy.update_once = record.update_once !== false;
  const snoozeUntil = String(record.snooze_until || '');
  if (snoozeUntil && Number.isFinite(Date.parse(snoozeUntil))) policy.snooze_until = snoozeUntil;
  return policy;
}

export function normalizeSlashCommandCatalog(data: Record<string, any>): SlashCommandCatalog {
  const rawCommands = data.commands;
  const commandsList = Array.isArray(rawCommands) ? rawCommands : [];
  const sources = new Set(['builtin', 'personal', 'project', 'temporary']);
  const metadata = data.metadata && typeof data.metadata === 'object'
    ? data.metadata as Record<string, Record<string, unknown>> : {};
  const commandMetadata = (name: string): Pick<SlashCommand, 'kind' | 'provenance'> => {
    const value = metadata[name];
    if (!value || !['builtin', 'extension', 'prompt', 'skill'].includes(String(value.kind))) return {};
    const kind = value.kind as SlashCommand['kind'];
    const p = value.provenance as Record<string, unknown> | undefined;
    if (!p || !['user', 'project', 'temporary'].includes(String(p.scope))
      || !['package', 'top-level'].includes(String(p.origin))) return { kind };
    return { kind, provenance: {
      path: String(p.path || '').slice(0, 1024), source: String(p.source || '').slice(0, 256),
      scope: p.scope as NonNullable<SlashCommand['provenance']>['scope'],
      origin: p.origin as NonNullable<SlashCommand['provenance']>['origin'],
      ...(p.base_dir ? { base_dir: String(p.base_dir).slice(0, 1024) } : {}),
    } };
  };
  const validCommands = commandsList
    .filter((entry: Record<string, unknown>) => typeof entry?.command === 'string'
      && entry.command.length <= 121 && /^\/[A-Za-z0-9][A-Za-z0-9._:-]*(?:\/[A-Za-z0-9][A-Za-z0-9._:-]*)*$/.test(entry.command));
  const commands = validCommands
    .slice(0, SLASH_COMMAND_MAX_ENTRIES)
    .map((entry: Record<string, unknown>): SlashCommand => ({
      command: String(entry.command),
      description: String(entry.description || entry.command).slice(0, 240),
      ...(entry.argument_hint ? { argument_hint: String(entry.argument_hint).slice(0, 120) } : {}),
      source: sources.has(String(entry.source))
        ? entry.source as SlashCommand['source']
        : 'temporary',
      ...commandMetadata(String(entry.command)),
    }))
    .sort((left, right) => left.command.localeCompare(right.command, undefined, { sensitivity: 'base' }));
  return {
    commands,
    truncated: Boolean(data.truncated) || validCommands.length > SLASH_COMMAND_MAX_ENTRIES,
    ...(['loading', 'available', 'unavailable', 'partial'].includes(String(data.status))
      ? { status: data.status as SlashCommandCatalog['status'] } : {}),
    ...(typeof data.revision === 'string' && /^[a-f0-9]{64}$/.test(data.revision)
      ? { revision: data.revision } : {}),
  };
}
