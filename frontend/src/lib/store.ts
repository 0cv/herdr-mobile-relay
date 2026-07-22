import { get, writable } from 'svelte/store';
import {
  APP_PROTOCOL_VERSION,
  importQuickSetup,
  loadRelayConfigs,
  normalizeRelayConfig,
  saveRelayConfigs,
} from './config';
import {
  agentStatusGroup,
  approvalOptions,
  clientPaneId,
  mergeAgentDetails,
  mergeAgentList,
  normalizeAgent,
  stabilizeBlockedSnapshot,
} from './agents';
import { relayProtocolError } from './protocol';
import { terminalHistoryLines } from './preferences';
import {
  clearPendingAppDeploy,
  clearPendingRelayUpdate,
  normalizeAppDeployment,
  normalizeRelayUpdate,
  rememberPendingAppDeploy,
  rememberPendingRelayUpdate,
} from './updates';
import type {
  Activity,
  Agent,
  AgentInventoryStatus,
  CommandResult,
  DirectoryListing,
  QuestionDraft,
  QuestionInteraction,
  RelayConfig,
  RelayConnectionView,
  SlashCommand,
  SlashCommandCatalog,
  TerminalFrame,
  ToastMessage,
} from './types';

const COMMAND_TIMEOUT_MS = 15_000;
const ACCEPTED_COMMAND_TIMEOUT_MS = 10_000;
const IMAGE_UPLOAD_TIMEOUT_MS = 60_000;
const CONNECTION_HEALTH_TIMEOUT_MS = 10_000;
const UPDATE_RESTART_RECONNECT_DELAY_MS = 1_000;
const RECONNECT_BASE_DELAY_MS = 3_000;
const RECONNECT_MAX_DELAY_MS = 60_000;
const PANE_READ_RETRY_MS = 35_000;
const IMAGE_UPLOAD_MAX_BYTES = 10 * 1024 * 1024;
const INVENTORY_REQUIRED_COMMANDS = new Set([
  'answer_question',
  'navigate_question',
  'respond',
  'clarify_question',
  'submit_prompt',
  'send_keys',
  'send_text',
  'agent_start',
  'agent_rename',
  'agent_stop',
  'agent_clear',
  'agent_restart',
  'acknowledge_pane',
  'upload_image',
]);

function normalizeAgentInventory(
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

interface RelayConnection extends RelayConnectionView {
  ws: WebSocket | null;
  reconnectTimer: ReturnType<typeof setTimeout> | null;
  healthTimer: ReturnType<typeof setTimeout> | null;
  updateRestartTimer: ReturnType<typeof setTimeout> | null;
  closed: boolean;
  directoryGeneration: number;
}

interface PendingRequest {
  relayId: string;
  action: string;
  resolve: (result: CommandResult) => void;
  reject: (error: CommandError) => void;
  timer: ReturnType<typeof setTimeout>;
}

interface PendingUpload {
  relayId: string;
  filename: string;
  resolve: (path: string) => void;
  reject: (error: CommandError) => void;
  timer: ReturnType<typeof setTimeout>;
}

interface SlashCommandCacheEntry {
  identity: string;
  catalog: SlashCommandCatalog;
}

interface PendingSlashCommands {
  identity: string;
  promise: Promise<SlashCommandCatalog>;
}

export class CommandError extends Error {
  data?: Record<string, unknown>;
}

class RelayStore {
  readonly relayConfigs = writable<RelayConfig[]>([]);
  readonly connections = writable<Map<string, RelayConnection>>(new Map());
  readonly agents = writable<Agent[]>([]);
  readonly activities = writable<Activity[]>([]);
  readonly terminalFrames = writable<Map<string, TerminalFrame>>(new Map());
  readonly responding = writable<Set<string>>(new Set());
  readonly toast = writable<ToastMessage | null>(null);
  readonly notificationBusy = writable(false);

  private connectionsValue = new Map<string, RelayConnection>();
  private agentsValue: Agent[] = [];
  private activitiesValue: Activity[] = [];
  private terminalFramesValue = new Map<string, TerminalFrame>();
  private respondingValue = new Set<string>();
  private blockedSnapshotMisses = new Map<string, number>();
  private pendingRequests = new Map<string, PendingRequest>();
  private pendingUploads = new Map<string, PendingUpload>();
  private pendingPaneReads = new Map<string, number>();
  private slashCommandCache = new Map<string, SlashCommandCacheEntry>();
  private pendingSlashCommands = new Map<string, PendingSlashCommands>();
  private respondingTimers = new Map<string, ReturnType<typeof setTimeout>>();
  private reconnectAttempts = new Map<string, number>();
  private reconnectEnabled = true;
  private toastId = 0;
  private pushConfigHandler: ((relayId: string) => void) | null = null;

  initialize(connect = true): void {
    let relays = loadRelayConfigs();
    const imported = importQuickSetup(relays, location);
    if (imported) {
      relays = imported;
      saveRelayConfigs(relays);
      history.replaceState(history.state, '', location.pathname + location.search);
    }
    this.relayConfigs.set(relays);
    if (connect) this.connectAll();
  }

  importSetupLink(locationValue: Pick<Location, 'hash' | 'protocol' | 'host' | 'pathname' | 'search'> = location, connect = true): boolean {
    const imported = importQuickSetup(get(this.relayConfigs), locationValue);
    if (!imported) return false;
    this.relayConfigs.set(imported);
    saveRelayConfigs(imported);
    history.replaceState(history.state, '', locationValue.pathname + locationValue.search);
    if (connect) this.connectAll(true);
    this.showToast('Relay added from the setup link.');
    return true;
  }

  destroy(): void {
    this.reconnectEnabled = false;
    for (const id of [...this.connectionsValue.keys()]) this.disconnectRelay(id);
    this.reconnectAttempts.clear();
    for (const timer of this.respondingTimers.values()) clearTimeout(timer);
    this.respondingTimers.clear();
    this.respondingValue.clear();
    this.responding.set(new Set());
    this.slashCommandCache.clear();
    this.pendingSlashCommands.clear();
    this.pendingPaneReads.clear();
  }

  setPushConfigHandler(handler: ((relayId: string) => void) | null): void {
    this.pushConfigHandler = handler;
  }

  addRelay(input: Partial<RelayConfig>): void {
    const next = normalizeRelayConfig(input);
    if (!next.url) return;
    const relays = get(this.relayConfigs);
    const existing = relays.find((relay) => relay.url === next.url);
    const updated = existing
      ? relays.map((relay) => (relay.id === existing.id ? { ...next, id: existing.id } : relay))
      : [...relays, next];
    this.relayConfigs.set(updated);
    saveRelayConfigs(updated);
    this.connectAll();
  }

  removeRelay(id: string): void {
    this.disconnectRelay(id);
    this.reconnectAttempts.delete(id);
    const relays = get(this.relayConfigs).filter((relay) => relay.id !== id);
    this.relayConfigs.set(relays);
    saveRelayConfigs(relays);
    this.removeAgentsForRelay(id);
    this.activitiesValue = this.activitiesValue.filter((activity) => activity.relay_id !== id);
    this.activities.set(this.activitiesValue);
  }

  connectAll(preserveAgents = false): void {
    this.reconnectEnabled = true;
    this.reconnectAttempts.clear();
    for (const id of [...this.connectionsValue.keys()]) this.disconnectRelay(id);
    this.connectionsValue.clear();
    this.connections.set(new Map());
    if (!preserveAgents) {
      this.agentsValue = [];
      this.agents.set([]);
    }
    this.blockedSnapshotMisses.clear();
    for (const relay of get(this.relayConfigs)) this.connectRelay(relay);
  }

  connectRelay(relay: RelayConfig): void {
    this.disconnectRelay(relay.id);
    const connection: RelayConnection = {
      relay,
      ws: null,
      status: 'connecting',
      reconnectTimer: null,
      healthTimer: null,
      updateRestartTimer: null,
      closed: false,
      agentProfiles: [],
      capabilities: [],
      directoryBrowser: null,
      directoryLoading: false,
      directoryError: '',
      directoryGeneration: 0,
      host: '',
      protocol: 0,
      version: '',
      releaseVersion: '',
      revision: '',
      update: normalizeRelayUpdate(null),
      appDeploy: normalizeAppDeployment(null),
      inventory: normalizeAgentInventory(null),
      pushStatus: '',
      vapidPublicKey: '',
    };
    this.connectionsValue.set(relay.id, connection);
    this.emitConnections();
    const separator = relay.url.includes('?') ? '&' : '?';
    const url = relay.token ? `${relay.url}${separator}token=${encodeURIComponent(relay.token)}` : relay.url;
    try {
      connection.ws = new WebSocket(url);
    } catch {
      if (!this.isCurrentConnection(relay.id, connection)) return;
      connection.status = 'disconnected';
      this.emitConnections();
      this.scheduleReconnect(relay, connection);
      return;
    }
    connection.ws.onopen = () => {
      if (!this.isCurrentConnection(relay.id, connection)) return;
      connection.status = 'connected';
      this.emitConnections();
      if (runningAsInstalledApp()) {
        this.sendRaw(relay.id, {
          type: 'register_app_origin',
          origin: location.origin,
          protocol: APP_PROTOCOL_VERSION,
        });
      }
      this.sendRaw(relay.id, { type: 'refresh_agents' });
    };
    connection.ws.onclose = () => {
      if (!this.isCurrentConnection(relay.id, connection)) return;
      this.clearHealthTimer(connection);
      connection.status = 'disconnected';
      this.rejectPendingOperations(relay.id, 'Relay disconnected');
      this.emitConnections();
      this.scheduleReconnect(relay, connection);
    };
    connection.ws.onerror = () => {
      if (!this.isCurrentConnection(relay.id, connection)) return;
      connection.status = 'disconnected';
      this.rejectPendingOperations(relay.id, 'Relay connection failed');
      this.emitConnections();
      this.scheduleReconnect(relay, connection);
    };
    connection.ws.onmessage = (event) => {
      if (!this.isCurrentConnection(relay.id, connection)) return;
      this.clearHealthTimer(connection);
      this.reconnectAttempts.delete(relay.id);
      try {
        this.handleMessage(relay.id, JSON.parse(String(event.data)) as Record<string, any>);
      } catch {
        // Ignore malformed relay frames without taking down other connections.
      }
    };
  }

  disconnectRelay(id: string): void {
    this.clearSlashCommandCacheForRelay(id);
    for (const paneId of this.pendingPaneReads.keys()) {
      if (paneId.startsWith(`${id}::`)) this.pendingPaneReads.delete(paneId);
    }
    const connection = this.connectionsValue.get(id);
    if (!connection) return;
    connection.closed = true;
    if (connection.reconnectTimer) clearTimeout(connection.reconnectTimer);
    this.clearHealthTimer(connection);
    this.clearUpdateRestartTimer(connection);
    connection.ws?.close();
    this.rejectPendingOperations(id, 'Relay disconnected');
    this.connectionsValue.delete(id);
    this.emitConnections();
  }

  revalidateConnections(timeoutMs = CONNECTION_HEALTH_TIMEOUT_MS): void {
    if (!this.reconnectEnabled) return;
    const relays = get(this.relayConfigs);
    for (const relay of relays) {
      const connection = this.connectionsValue.get(relay.id);
      if (connection?.ws?.readyState === WebSocket.CONNECTING) continue;
      if (!connection?.ws || connection.ws.readyState !== WebSocket.OPEN) {
        this.connectRelay(relay);
        continue;
      }
      if (connection.healthTimer) continue;
      connection.healthTimer = setTimeout(() => {
        if (!this.isCurrentConnection(relay.id, connection)) return;
        connection.healthTimer = null;
        this.connectRelay(relay);
      }, timeoutMs);
      try {
        connection.ws.send(JSON.stringify({ type: 'refresh_agents' }));
      } catch {
        this.clearHealthTimer(connection);
        this.connectRelay(relay);
      }
    }
  }

  private isCurrentConnection(relayId: string, connection: RelayConnection): boolean {
    return this.connectionsValue.get(relayId) === connection;
  }

  private clearHealthTimer(connection: RelayConnection): void {
    if (!connection.healthTimer) return;
    clearTimeout(connection.healthTimer);
    connection.healthTimer = null;
  }

  private syncUpdateRestartReconnect(relayId: string, connection: RelayConnection): void {
    if (connection.update.state !== 'restarting') {
      this.clearUpdateRestartTimer(connection);
      return;
    }
    if (connection.closed || connection.updateRestartTimer) return;
    if (!this.isCurrentConnection(relayId, connection)) return;
    connection.updateRestartTimer = setTimeout(() => {
      if (!this.isCurrentConnection(relayId, connection)) return;
      connection.updateRestartTimer = null;
      this.connectRelay(connection.relay);
    }, UPDATE_RESTART_RECONNECT_DELAY_MS);
  }

  private clearUpdateRestartTimer(connection: RelayConnection): void {
    if (!connection.updateRestartTimer) return;
    clearTimeout(connection.updateRestartTimer);
    connection.updateRestartTimer = null;
  }

  private scheduleReconnect(relay: RelayConfig, connection: RelayConnection): void {
    if (connection.closed || !this.reconnectEnabled || connection.reconnectTimer) return;
    if (!this.isCurrentConnection(relay.id, connection)) return;
    const attempt = (this.reconnectAttempts.get(relay.id) || 0) + 1;
    this.reconnectAttempts.set(relay.id, attempt);
    const baseDelay = Math.min(
      RECONNECT_MAX_DELAY_MS,
      RECONNECT_BASE_DELAY_MS * 2 ** Math.min(attempt - 1, 5),
    );
    const jitter = attempt === 1 ? 1 : 0.8 + Math.random() * 0.4;
    const delay = Math.round(baseDelay * jitter);
    connection.reconnectTimer = setTimeout(() => {
      if (!this.isCurrentConnection(relay.id, connection)) return;
      connection.reconnectTimer = null;
      this.connectRelay(relay);
    }, delay);
  }

  private handleMessage(relayId: string, message: Record<string, any>): void {
    const connection = this.connectionsValue.get(relayId);
    if (message.type === 'push_config') {
      if (!connection) return;
      connection.vapidPublicKey = String(message.vapid_public_key || '');
      connection.host = String(message.host || '');
      connection.protocol = Number.isInteger(message.protocol) && message.protocol > 0 ? message.protocol : 1;
      connection.version = typeof message.version === 'string' ? message.version.slice(0, 40) : '';
      connection.releaseVersion = String(message.release_version || '').slice(0, 32);
      connection.revision = String(message.revision || message.version || '').slice(0, 40);
      connection.update = normalizeRelayUpdate(
        message.update,
        connection.releaseVersion,
        connection.revision,
      );
      this.syncUpdateRestartReconnect(relayId, connection);
      connection.appDeploy = normalizeAppDeployment(message.app_deploy);
      connection.inventory = normalizeAgentInventory(message.inventory, 'ready');
      connection.capabilities = Array.isArray(message.capabilities) ? message.capabilities.filter(Boolean) : [];
      connection.agentProfiles = Array.isArray(message.agent_profiles)
        ? message.agent_profiles.filter((profile: any) => profile?.id)
        : [];
      this.emitConnections();
      this.pushConfigHandler?.(relayId);
      return;
    }
    if (message.type === 'inventory_status' && connection) {
      connection.inventory = normalizeAgentInventory(message);
      this.emitConnections();
      return;
    }
    if (message.type === 'update_status' && connection) {
      connection.update = normalizeRelayUpdate(
        message.update,
        connection.releaseVersion,
        connection.revision,
      );
      this.syncUpdateRestartReconnect(relayId, connection);
      if (['failed', 'rolled_back'].includes(connection.update.state)) {
        clearPendingRelayUpdate(relayId);
        clearPendingAppDeploy(relayId);
      }
      this.emitConnections();
      return;
    }
    if (message.type === 'app_deploy_status' && connection) {
      connection.appDeploy = normalizeAppDeployment(message.app_deploy);
      this.emitConnections();
      return;
    }
    if (message.type === 'push_subscribed' && connection) {
      connection.pushStatus = message.ok ? 'subscribed' : 'failed';
      this.emitConnections();
      return;
    }
    if (message.type === 'push_unsubscribed' && connection && message.ok) {
      connection.pushStatus = '';
      this.emitConnections();
      return;
    }
    if (message.type === 'command_result') {
      this.handleCommandResult(relayId, message as CommandResult);
      return;
    }
    if (message.type === 'upload_result') {
      this.handleUploadResult(relayId, message);
      return;
    }
    if (message.type === 'activity_history') {
      this.mergeActivityHistory(relayId, message.activities || []);
      return;
    }
    if (message.type === 'activity' && message.activity) {
      this.upsertActivity(relayId, message.activity);
      return;
    }
    if (message.type === 'agents') {
      // Starting/error snapshots are not authoritative. In particular, a relay
      // restart has no in-memory pane cache yet; accepting its placeholder []
      // would erase the phone's last useful snapshot and recreate the original
      // "no agents" failure. The first ready transition is followed by a fresh
      // authoritative agents frame.
      if (
        connection
        && connection.inventory.state !== 'ready'
        && !connection.inventory.stale
      ) return;
      const label = get(this.relayConfigs).find((relay) => relay.id === relayId)?.label || 'relay';
      const incoming = (Array.isArray(message.agents) ? message.agents : [])
        .map((agent: Partial<Agent>) => normalizeAgent(relayId, label, agent));
      this.agentsValue = mergeAgentList(
        this.agentsValue,
        relayId,
        incoming,
        this.blockedSnapshotMisses,
        this.respondingValue,
      );
      this.reconcileResponding();
      this.agents.set(this.agentsValue);
      return;
    }
    if (message.type === 'blocked') {
      const label = get(this.relayConfigs).find((relay) => relay.id === relayId)?.label || 'relay';
      const next = normalizeAgent(relayId, label, { ...message, status: 'blocked' });
      this.blockedSnapshotMisses.delete(next.pane_id);
      const index = this.agentsValue.findIndex((agent) => agent.pane_id === next.pane_id);
      if (index >= 0) {
        const copy = [...this.agentsValue];
        copy[index] = mergeAgentDetails(copy[index], next);
        this.agentsValue = copy;
      } else this.agentsValue = [...this.agentsValue, next];
      this.respondingValue.delete(next.pane_id);
      this.responding.set(new Set(this.respondingValue));
      this.agents.set(this.agentsValue);
      return;
    }
    if (message.type === 'agent_update' && message.pane_id) {
      const label = get(this.relayConfigs).find((relay) => relay.id === relayId)?.label || 'relay';
      const next = normalizeAgent(relayId, label, message);
      const index = this.agentsValue.findIndex((agent) => agent.pane_id === next.pane_id);
      const before = index >= 0 ? this.agentsValue[index] : undefined;
      const stabilized = stabilizeBlockedSnapshot(before, next, this.blockedSnapshotMisses, this.respondingValue);
      if (index >= 0) {
        const copy = [...this.agentsValue];
        copy[index] = mergeAgentDetails(before, stabilized);
        this.agentsValue = copy;
      } else this.agentsValue = [...this.agentsValue, stabilized];
      this.reconcileResponding();
      this.agents.set(this.agentsValue);
      return;
    }
    if (message.type === 'pane_content') {
      const paneId = clientPaneId(relayId, String(message.pane_id || ''));
      this.pendingPaneReads.delete(paneId);
      const desktopFooterLines = Number(message.desktop_footer_lines);
      const desktopPromptLines = Number(message.desktop_prompt_lines);
      this.terminalFramesValue.set(paneId, {
        paneId,
        content: String(message.content || '(empty)'),
        format: String(message.format || 'plain'),
        desktopFooterLines: Number.isInteger(desktopFooterLines) && desktopFooterLines > 0
          ? desktopFooterLines
          : undefined,
        desktopPromptLines: Number.isInteger(desktopPromptLines) && desktopPromptLines >= 0
          ? desktopPromptLines
          : undefined,
      });
      this.terminalFrames.set(new Map(this.terminalFramesValue));
      this.mergePaneInteraction(paneId, message);
    }
  }

  private mergePaneInteraction(paneId: string, message: Record<string, any>): void {
    if (!Object.prototype.hasOwnProperty.call(message, 'interaction')) return;
    const index = this.agentsValue.findIndex((agent) => agent.pane_id === paneId);
    if (index < 0) return;
    const agent = this.agentsValue[index];
    const interaction = (message.interaction || null) as QuestionInteraction | null;
    const questionLayout = Boolean(message.question_layout || interaction);
    let next: Agent | null = null;
    if (questionLayout) {
      this.blockedSnapshotMisses.delete(paneId);
      next = { ...agent, status: 'blocked', interaction: interaction || agent.interaction || null, question_layout: true };
    } else if (agentStatusGroup(agent) !== 'blocked') {
      next = { ...agent, interaction: null, question_layout: false };
    }
    if (!next) return;
    const copy = [...this.agentsValue];
    copy[index] = next;
    this.agentsValue = copy;
    this.agents.set(copy);
  }

  private removeAgentsForRelay(relayId: string): void {
    this.agentsValue = this.agentsValue.filter((agent) => agent.relay_id !== relayId);
    for (const paneId of this.blockedSnapshotMisses.keys()) {
      if (paneId.startsWith(`${relayId}::`)) this.blockedSnapshotMisses.delete(paneId);
    }
    for (const paneId of this.pendingPaneReads.keys()) {
      if (paneId.startsWith(`${relayId}::`)) this.pendingPaneReads.delete(paneId);
    }
    this.agents.set(this.agentsValue);
  }

  private clearSlashCommandCacheForRelay(relayId: string): void {
    const prefix = `${relayId}::`;
    for (const paneId of this.slashCommandCache.keys()) {
      if (paneId.startsWith(prefix)) this.slashCommandCache.delete(paneId);
    }
    for (const paneId of this.pendingSlashCommands.keys()) {
      if (paneId.startsWith(prefix)) this.pendingSlashCommands.delete(paneId);
    }
  }

  private reconcileResponding(): void {
    const blocked = new Set(this.agentsValue.filter((agent) => agentStatusGroup(agent) === 'blocked').map((agent) => agent.pane_id));
    let changed = false;
    for (const paneId of this.respondingValue) {
      if (!blocked.has(paneId)) {
        const timer = this.respondingTimers.get(paneId);
        if (timer) clearTimeout(timer);
        this.respondingTimers.delete(paneId);
        this.respondingValue.delete(paneId);
        changed = true;
      }
    }
    if (changed) this.responding.set(new Set(this.respondingValue));
  }

  markResponding(paneId: string): void {
    this.respondingValue.add(paneId);
    this.responding.set(new Set(this.respondingValue));
    const previous = this.respondingTimers.get(paneId);
    if (previous) clearTimeout(previous);
    const timer = setTimeout(() => {
      if (this.respondingTimers.get(paneId) !== timer) return;
      this.respondingTimers.delete(paneId);
      if (!this.respondingValue.delete(paneId)) return;
      this.responding.set(new Set(this.respondingValue));
    }, 10_000);
    this.respondingTimers.set(paneId, timer);
  }

  clearResponding(paneId: string): void {
    const timer = this.respondingTimers.get(paneId);
    if (timer) clearTimeout(timer);
    this.respondingTimers.delete(paneId);
    this.respondingValue.delete(paneId);
    this.responding.set(new Set(this.respondingValue));
  }

  sendRaw(relayId: string, payload: Record<string, unknown>): boolean {
    const connection = this.connectionsValue.get(relayId);
    if (!connection?.ws || connection.ws.readyState !== 1) return false;
    connection.ws.send(JSON.stringify(payload));
    return true;
  }

  sendCommand(
    relayId: string,
    payload: Record<string, any>,
    timeoutMs = COMMAND_TIMEOUT_MS,
    allowProtocolMismatch = false,
  ): Promise<CommandResult> {
    const connection = this.connectionsValue.get(relayId);
    if (!connection?.ws || connection.ws.readyState !== 1) {
      return Promise.reject(new CommandError('Relay is not connected'));
    }
    const protocolError = relayProtocolError(connection);
    if (protocolError && !allowProtocolMismatch) return Promise.reject(new CommandError(protocolError));
    if (INVENTORY_REQUIRED_COMMANDS.has(String(payload.type)) && connection.inventory.state !== 'ready') {
      return Promise.reject(new CommandError(
        connection.inventory.message || 'Herdr agent inventory is not ready on this computer',
      ));
    }
    const requestId = commandRequestId();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pendingRequests.delete(requestId);
        reject(new CommandError('Relay did not confirm the command in time'));
      }, timeoutMs);
      this.pendingRequests.set(requestId, { relayId, action: payload.type, resolve, reject, timer });
      try {
        connection.ws?.send(JSON.stringify({
          ...payload,
          request_id: requestId,
          client_id: pushClientId(),
          protocol: APP_PROTOCOL_VERSION,
        }));
      } catch {
        clearTimeout(timer);
        this.pendingRequests.delete(requestId);
        reject(new CommandError('Could not send command to relay'));
      }
    });
  }

  sendToAgent(agent: Agent, payload: Record<string, any>, timeoutMs?: number): Promise<CommandResult> {
    return this.sendCommand(agent.relay_id, { ...payload, pane_id: agent.raw_pane_id }, timeoutMs);
  }

  async checkRelayUpdate(relayId: string): Promise<void> {
    const connection = this.connectionsValue.get(relayId);
    if (!connection?.capabilities.includes('self_update')) {
      throw new CommandError('This relay does not support phone-driven updates yet');
    }
    const result = await this.sendCommand(relayId, { type: 'check_update' }, 30_000, true);
    if (result.data?.update && connection === this.connectionsValue.get(relayId)) {
      connection.update = normalizeRelayUpdate(
        result.data.update,
        connection.releaseVersion,
        connection.revision,
      );
      this.emitConnections();
    }
  }

  async installRelayUpdate(relayId: string): Promise<void> {
    const connection = this.connectionsValue.get(relayId);
    if (!connection?.capabilities.includes('self_update')) {
      throw new CommandError('This relay does not support phone-driven updates yet');
    }
    const update = connection.update;
    if (update.state !== 'available' || !update.can_install || !update.target_revision) {
      throw new CommandError(update.reason || 'No installable update is available');
    }
    rememberPendingRelayUpdate(relayId, {
      version: update.available_version,
      revision: update.target_revision,
    });
    try {
      const result = await this.sendCommand(relayId, {
        type: 'install_update',
        expected_version: update.available_version,
        expected_revision: update.target_revision,
      }, 30_000, true);
      if (result.data?.update && connection === this.connectionsValue.get(relayId)) {
        connection.update = normalizeRelayUpdate(
          result.data.update,
          connection.releaseVersion,
          connection.revision,
        );
        this.emitConnections();
      }
    } catch (error) {
      if (error instanceof CommandError && error.data?.update) {
        clearPendingRelayUpdate(relayId);
        clearPendingAppDeploy(relayId);
        if (connection === this.connectionsValue.get(relayId)) {
          connection.update = normalizeRelayUpdate(
            error.data.update,
            connection.releaseVersion,
            connection.revision,
          );
          this.emitConnections();
        }
      }
      throw error;
    }
  }

  // Update the deployment-owner relay and, once it reconnects at the target
  // version, deploy the app from it (continued in App.svelte on reconnect via
  // the pending-app-deploy marker). Lets the phone drive both steps with one tap.
  async updateRelayAndDeploy(relayId: string, targetVersion: string): Promise<void> {
    rememberPendingAppDeploy(relayId, targetVersion);
    try {
      await this.installRelayUpdate(relayId);
    } catch (error) {
      clearPendingAppDeploy(relayId);
      throw error;
    }
  }

  async deployAppUpdate(relayId: string, expectedVersion: string): Promise<void> {
    const connection = this.connectionsValue.get(relayId);
    if (!connection?.capabilities.includes('app_deploy') || !connection.appDeploy.configured) {
      throw new CommandError(connection?.appDeploy.reason || 'This relay cannot deploy the phone app');
    }
    if (!connection.appDeploy.revision || connection.releaseVersion !== expectedVersion) {
      throw new CommandError('Update this deployment relay to the upstream release first');
    }
    const result = await this.sendCommand(relayId, {
      type: 'deploy_app_update',
      expected_version: connection.releaseVersion,
      expected_revision: connection.appDeploy.revision,
      expected_origin: location.origin,
    }, 30_000);
    if (result.data?.app_deploy && connection === this.connectionsValue.get(relayId)) {
      connection.appDeploy = normalizeAppDeployment(result.data.app_deploy);
      this.emitConnections();
    }
  }

  private handleCommandResult(relayId: string, result: CommandResult): void {
    const pending = this.pendingRequests.get(result.request_id);
    if (!pending || pending.relayId !== relayId) return;
    if (result.phase === 'accepted') {
      // The relay already acted; give the final confirmation its own window
      // instead of counting it against the original send timeout.
      clearTimeout(pending.timer);
      pending.timer = setTimeout(() => {
        this.pendingRequests.delete(result.request_id);
        pending.reject(new CommandError('Relay did not confirm the command in time'));
      }, ACCEPTED_COMMAND_TIMEOUT_MS);
      this.showToast('Command accepted; waiting for agent state…');
      return;
    }
    clearTimeout(pending.timer);
    this.pendingRequests.delete(result.request_id);
    if (result.ok) pending.resolve(result);
    else {
      const error = new CommandError(result.error || 'Command failed');
      error.data = result.data;
      pending.reject(error);
    }
  }

  private rejectPendingRequests(relayId: string, message: string): void {
    for (const [requestId, pending] of this.pendingRequests) {
      if (pending.relayId !== relayId) continue;
      clearTimeout(pending.timer);
      this.pendingRequests.delete(requestId);
      pending.reject(new CommandError(message));
    }
  }

  private rejectPendingUploads(relayId: string, message: string): void {
    for (const [requestId, pending] of this.pendingUploads) {
      if (pending.relayId !== relayId) continue;
      clearTimeout(pending.timer);
      this.pendingUploads.delete(requestId);
      pending.reject(new CommandError(message));
    }
  }

  private rejectPendingOperations(relayId: string, message: string): void {
    this.rejectPendingRequests(relayId, message);
    this.rejectPendingUploads(relayId, message);
  }

  readPane(agent: Agent): void {
    const requestedAt = this.pendingPaneReads.get(agent.pane_id);
    if (requestedAt && Date.now() - requestedAt < PANE_READ_RETRY_MS) return;
    const sent = this.sendRaw(agent.relay_id, {
      type: 'read_pane',
      pane_id: agent.raw_pane_id,
      lines: get(terminalHistoryLines),
      format: 'ansi',
    });
    if (sent) this.pendingPaneReads.set(agent.pane_id, Date.now());
  }

  requestAgents(): void {
    for (const relayId of this.connectionsValue.keys()) {
      this.sendRaw(relayId, { type: 'refresh_agents' });
    }
  }

  waitForAgent(
    relayId: string,
    identity: { rawPaneId?: string; name?: string; cwd?: string },
    timeoutMs = 6_000,
  ): Promise<Agent | null> {
    const match = (agent: Agent): boolean => {
      if (agent.relay_id !== relayId) return false;
      if (identity.rawPaneId && agent.raw_pane_id === identity.rawPaneId) return true;
      if (!identity.name || ![agent.name, agent.tab_label].includes(identity.name)) return false;
      return !identity.cwd || !agent.cwd || agent.cwd === identity.cwd;
    };
    const current = this.agentsValue.find(match);
    if (current) return Promise.resolve(current);

    this.requestAgents();
    return new Promise((resolve) => {
      let settled = false;
      const cleanup: {
        timer?: ReturnType<typeof setTimeout>;
        stop?: () => void;
      } = {};
      const finish = (agent: Agent | null) => {
        if (settled) return;
        settled = true;
        if (cleanup.timer) clearTimeout(cleanup.timer);
        cleanup.stop?.();
        resolve(agent);
      };
      cleanup.stop = this.agents.subscribe((agents) => {
        const agent = agents.find(match);
        if (agent) finish(agent);
      });
      if (settled) {
        cleanup.stop();
        return;
      }
      cleanup.timer = setTimeout(() => finish(null), timeoutMs);
    });
  }

  async acknowledgePane(agent: Agent): Promise<void> {
    if (agentStatusGroup(agent) !== 'done') return;
    this.agentsValue = this.agentsValue.map((item) => item.pane_id === agent.pane_id ? { ...item, status: 'idle' } : item);
    this.agents.set(this.agentsValue);
    await this.sendToAgent(agent, { type: 'acknowledge_pane' }).catch((error) => this.showToast(error.message, true));
  }

  async respond(agent: Agent, index: number, total: number, choice?: string, source = 'App'): Promise<boolean> {
    if (index < 0) return false;
    const label = choice || approvalOptions(agent)[index] || `option ${index + 1}`;
    this.markResponding(agent.pane_id);
    try {
      const result = await this.sendToAgent(agent, {
        type: 'respond', index, total, choice: label, source, event_id: agent.event_id || '',
      }, 12_000);
      this.showToast(result.phase === 'unconfirmed'
        ? 'Approval was accepted but the agent still appears blocked.'
        : `Confirmed: ${label}`,
      );
      return true;
    } catch (error) {
      this.clearResponding(agent.pane_id);
      this.showToast((error as Error).message, true);
      return false;
    } finally {
      setTimeout(() => this.readPane(agent), 500);
    }
  }

  async answerQuestion(agent: Agent, interaction: QuestionInteraction, draft: QuestionDraft): Promise<CommandResult> {
    this.markResponding(agent.pane_id);
    try {
      return await this.sendToAgent(agent, {
        type: 'answer_question',
        interaction_id: interaction.id,
        selected_indices: [...draft.selected].sort((a, b) => a - b),
        other_selected: draft.otherSelected,
        other_text: draft.otherText,
        source: 'App',
      }, 20_000);
    } finally {
      setTimeout(() => this.readPane(agent), 400);
    }
  }

  async navigateQuestionPrevious(agent: Agent, interaction: QuestionInteraction): Promise<CommandResult> {
    this.markResponding(agent.pane_id);
    try {
      return await this.sendToAgent(agent, {
        type: 'navigate_question',
        interaction_id: interaction.id,
        direction: 'previous',
        source: 'App',
      }, 20_000);
    } finally {
      setTimeout(() => this.readPane(agent), 400);
    }
  }

  async clarifyQuestion(agent: Agent, interaction: QuestionInteraction): Promise<CommandResult> {
    this.markResponding(agent.pane_id);
    try {
      return await this.sendToAgent(agent, {
        type: 'clarify_question',
        interaction_id: interaction.id,
        source: 'App',
      }, 20_000);
    } finally {
      setTimeout(() => this.readPane(agent), 400);
    }
  }

  applyQuestionInteraction(agent: Agent, interaction: QuestionInteraction | null): void {
    this.clearResponding(agent.pane_id);
    this.blockedSnapshotMisses.delete(agent.pane_id);
    this.agentsValue = this.agentsValue.map((item) => item.pane_id === agent.pane_id
      ? { ...item, interaction, question_layout: Boolean(interaction), status: interaction ? 'blocked' : 'working' }
      : item);
    this.agents.set(this.agentsValue);
  }

  requestActivities(): void {
    for (const connection of this.connectionsValue.values()) {
      if (connection.ws?.readyState === 1) connection.ws.send(JSON.stringify({ type: 'get_activity', limit: 500 }));
    }
  }

  async clearActivities(): Promise<void> {
    const relayIds = [...this.connectionsValue.keys()];
    const cleared = new Set<string>();
    const failures: string[] = [];
    await Promise.all(relayIds.map(async (relayId) => {
      const connection = this.connectionsValue.get(relayId);
      const label = connection?.relay.label || relayId;
      if (!connection?.capabilities.includes('clear_activities')) {
        failures.push(`${label} needs an update`);
        return;
      }
      try {
        await this.sendCommand(relayId, { type: 'clear_activities' });
        cleared.add(relayId);
      } catch {
        failures.push(`${label} is unavailable`);
      }
    }));
    if (cleared.size) {
      this.activitiesValue = this.activitiesValue.filter((activity) => !cleared.has(activity.relay_id));
      this.activities.set(this.activitiesValue);
    }
    if (failures.length) {
      throw new CommandError(`Some activity could not be deleted: ${failures.join(', ')}.`);
    }
  }

  private normalizeActivity(relayId: string, activity: Record<string, any>): Activity {
    const relay = get(this.relayConfigs).find((item) => item.id === relayId);
    return {
      ...activity,
      relay_id: relayId,
      relay_label: relay?.label || activity.host || 'relay',
      activity_key: `${relayId}:${activity.id || `${activity.timestamp}:${activity.kind}:${activity.request_id || ''}`}`,
    } as Activity;
  }

  private mergeActivityHistory(relayId: string, incoming: Record<string, any>[]): void {
    const retained = this.activitiesValue.filter((activity) => activity.relay_id !== relayId);
    const normalized = incoming.filter((activity) => activity?.timestamp).map((activity) => this.normalizeActivity(relayId, activity));
    this.activitiesValue = retained.concat(normalized)
      .sort((a, b) => Number(b.timestamp) - Number(a.timestamp)).slice(0, 500);
    this.activities.set(this.activitiesValue);
  }

  private upsertActivity(relayId: string, activity: Record<string, any>): void {
    const next = this.normalizeActivity(relayId, activity);
    this.activitiesValue = [next, ...this.activitiesValue.filter((item) => item.activity_key !== next.activity_key)]
      .sort((a, b) => Number(b.timestamp) - Number(a.timestamp)).slice(0, 500);
    this.activities.set(this.activitiesValue);
  }

  async listDirectories(relayId: string, path = ''): Promise<DirectoryListing> {
    const connection = this.connectionsValue.get(relayId);
    if (!connection) throw new CommandError('Relay is not connected');
    const generation = ++connection.directoryGeneration;
    connection.directoryLoading = true;
    connection.directoryError = '';
    this.emitConnections();
    try {
      const result = await this.sendCommand(relayId, { type: 'list_directories', path }, 10_000);
      const listing = result.data as unknown as DirectoryListing;
      if (!listing?.current || !Array.isArray(listing.directories)) throw new CommandError('Relay returned an invalid directory listing');
      if (!this.isCurrentConnection(relayId, connection)) {
        throw new CommandError('Relay reconnected while loading directories');
      }
      if (generation === connection.directoryGeneration) {
        connection.directoryBrowser = listing;
      }
      return listing;
    } catch (error) {
      if (this.isCurrentConnection(relayId, connection) && generation === connection.directoryGeneration) {
        connection.directoryError = (error as Error).message;
      }
      throw error;
    } finally {
      if (this.isCurrentConnection(relayId, connection) && generation === connection.directoryGeneration) {
        connection.directoryLoading = false;
        this.emitConnections();
      }
    }
  }

  async loadSlashCommands(agent: Agent): Promise<SlashCommandCatalog> {
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('slash_commands')) {
      throw new CommandError('This relay does not provide slash-command suggestions.');
    }
    const identity = `${String(agent.agent || '')}\u0000${String(agent.cwd || '')}`;
    const cached = this.slashCommandCache.get(agent.pane_id);
    if (cached?.identity === identity) return cached.catalog;
    const pending = this.pendingSlashCommands.get(agent.pane_id);
    if (pending?.identity === identity) return pending.promise;

    const promise = this.sendToAgent(agent, { type: 'list_slash_commands' }, 10_000)
      .then((result) => {
        if (!Array.isArray(result.data?.commands)) {
          throw new CommandError('Relay returned an invalid slash-command catalog.');
        }
        const sources = new Set(['builtin', 'personal', 'project']);
        const commands = result.data.commands
          .filter((entry: Record<string, unknown>) => typeof entry?.command === 'string'
            && /^\/[A-Za-z0-9][A-Za-z0-9._:-]{0,119}$/.test(entry.command))
          .slice(0, 300)
          .map((entry: Record<string, unknown>): SlashCommand => ({
            command: String(entry.command),
            description: String(entry.description || entry.command).slice(0, 240),
            ...(entry.argument_hint ? { argument_hint: String(entry.argument_hint).slice(0, 120) } : {}),
            source: sources.has(String(entry.source))
              ? entry.source as SlashCommand['source']
              : 'builtin',
          }))
          .sort((left, right) => left.command.localeCompare(right.command, undefined, { sensitivity: 'base' }));
        const catalog = { commands, truncated: Boolean(result.data.truncated) };
        if (this.pendingSlashCommands.get(agent.pane_id)?.promise === promise) {
          this.slashCommandCache.set(agent.pane_id, { identity, catalog });
        }
        return catalog;
      });
    this.pendingSlashCommands.set(agent.pane_id, { identity, promise });
    try {
      return await promise;
    } finally {
      if (this.pendingSlashCommands.get(agent.pane_id)?.promise === promise) {
        this.pendingSlashCommands.delete(agent.pane_id);
      }
    }
  }

  async uploadImage(agent: Agent, file: File, timeoutMs = IMAGE_UPLOAD_TIMEOUT_MS): Promise<string> {
    if (file.size > IMAGE_UPLOAD_MAX_BYTES) throw new CommandError('Image is larger than 10 MB.');
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.ws || connection.ws.readyState !== 1) throw new CommandError('Relay is not connected.');
    const protocolError = relayProtocolError(connection);
    if (protocolError) throw new CommandError(protocolError);
    const requestId = `upload-${Date.now()}-${Math.random().toString(36).slice(2)}`;
    const data = await readFileAsDataUrl(file);
    const uploadSocket = connection.ws;
    if (!this.isCurrentConnection(agent.relay_id, connection)
      || !uploadSocket
      || uploadSocket.readyState !== WebSocket.OPEN) {
      throw new CommandError('Relay disconnected before the image could be uploaded.');
    }
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pendingUploads.delete(requestId);
        reject(new CommandError('Image upload did not finish in time.'));
      }, timeoutMs);
      this.pendingUploads.set(requestId, {
        relayId: agent.relay_id,
        filename: file.name || 'image',
        resolve,
        reject,
        timer,
      });
      try {
        uploadSocket.send(JSON.stringify({
          type: 'upload_image',
          protocol: APP_PROTOCOL_VERSION,
          request_id: requestId,
          client_id: pushClientId(),
          pane_id: agent.raw_pane_id,
          filename: file.name || 'image',
          mime: file.type || 'application/octet-stream',
          data,
        }));
      } catch {
        clearTimeout(timer);
        this.pendingUploads.delete(requestId);
        reject(new CommandError('Could not send image to relay.'));
      }
    });
  }

  private handleUploadResult(relayId: string, message: Record<string, any>): void {
    const pending = this.pendingUploads.get(String(message.request_id || ''));
    if (!pending || pending.relayId !== relayId) return;
    clearTimeout(pending.timer);
    this.pendingUploads.delete(String(message.request_id));
    if (!message.ok) pending.reject(new CommandError(message.error || 'Image upload failed.'));
    else pending.resolve(String(message.path || pending.filename));
  }

  setPushStatus(relayId: string, status: string): void {
    const connection = this.connectionsValue.get(relayId);
    if (!connection || connection.pushStatus === status) return;
    connection.pushStatus = status;
    this.emitConnections();
  }

  connection(relayId: string): RelayConnection | undefined {
    return this.connectionsValue.get(relayId);
  }

  showToast(message: string, error = false): void {
    this.toast.set({ id: ++this.toastId, message, error });
  }

  private emitConnections(): void {
    this.connections.set(new Map(
      [...this.connectionsValue].map(([relayId, connection]) => [relayId, { ...connection }]),
    ));
  }
}

function commandRequestId(): string {
  if (crypto.randomUUID) return crypto.randomUUID();
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return [...bytes].map((value) => value.toString(16).padStart(2, '0')).join('');
}

function runningAsInstalledApp(): boolean {
  return Boolean(
    window.matchMedia?.('(display-mode: standalone)').matches
    || (navigator as Navigator & { standalone?: boolean }).standalone,
  );
}

export function pushClientId(): string {
  let value = localStorage.getItem('herdr_push_client_id');
  if (value) return value;
  value = crypto.randomUUID ? crypto.randomUUID() : `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
  localStorage.setItem('herdr_push_client_id', value);
  return value;
}

function readFileAsDataUrl(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result || ''));
    reader.onerror = () => reject(reader.error || new Error('Image upload failed.'));
    reader.readAsDataURL(file);
  });
}

export const relayStore = new RelayStore();
