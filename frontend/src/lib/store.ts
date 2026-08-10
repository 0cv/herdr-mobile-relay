import { get, writable } from 'svelte/store';
import {
  APP_PROTOCOL_VERSION,
  importQuickSetup,
  loadRelayConfigs,
  MAX_PANE_SIZE_COLUMNS,
  MIN_PANE_SIZE_COLUMNS,
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
  normalizeAgentAttention,
  rawBlocked,
  staleAgentRevision,
  stabilizeBlockedSnapshot,
} from './agents';
import { relayProtocolError } from './protocol';
import {
  createE2EEClientHandshake,
  E2EE_SUBPROTOCOL,
  type E2EEClientHandshake,
  type E2EESession,
} from './e2ee';
import { terminalHistoryLines, terminalRefreshInterval } from './preferences';
import {
  clearPendingRelayUpdate,
  normalizeAppDeployment,
  normalizeRelayUpdate,
  observeAppUpstreamVersion,
  rememberPendingRelayUpdate,
} from './updates';
import type {
  Activity,
  Agent,
  AgentProfile,
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
const E2EE_HANDSHAKE_TIMEOUT_MS = 10_000;
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
  'copy_agent_response',
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
  e2eeHandshake: E2EEClientHandshake | null;
  e2eeSession: E2EESession | null;
  e2eeTimer: number | null;
  e2eeFailed: boolean;
  sendQueue: Promise<void>;
  receiveQueue: Promise<void>;
  closed: boolean;
  directoryGeneration: number;
}

interface PendingOperation {
  relayId: string;
  reject: (error: CommandError) => void;
  timer: ReturnType<typeof setTimeout>;
}

interface PendingRequest extends PendingOperation {
  action: string;
  resolve: (result: CommandResult) => void;
}

interface PendingUpload extends PendingOperation {
  filename: string;
  resolve: (path: string) => void;
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
  private paneContentFingerprints = new Map<string, string>();
  private watchedPanes = new Map<string, Agent>();
  private paneWatchesStarted = new Set<string>();
  private slashCommandCache = new Map<string, SlashCommandCacheEntry>();
  private pendingSlashCommands = new Map<string, PendingSlashCommands>();
  private respondingTimers = new Map<string, ReturnType<typeof setTimeout>>();
  private reconnectAttempts = new Map<string, number>();
  private reconnectEnabled = true;
  private toastId = 0;
  private pushConfigHandler: ((relayId: string) => void) | null = null;

  constructor() {
    let previousRefreshInterval = get(terminalRefreshInterval);
    terminalRefreshInterval.subscribe((value) => {
      if (value === previousRefreshInterval) return;
      previousRefreshInterval = value;
      this.restartPaneWatches();
    });
  }

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
    this.paneContentFingerprints.clear();
    this.watchedPanes.clear();
    this.paneWatchesStarted.clear();
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
      e2eeHandshake: null,
      e2eeSession: null,
      e2eeTimer: null,
      e2eeFailed: false,
      sendQueue: Promise.resolve(),
      receiveQueue: Promise.resolve(),
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
    try {
      connection.ws = relay.token
        ? new WebSocket(relay.url, E2EE_SUBPROTOCOL)
        : new WebSocket(relay.url);
    } catch {
      if (!this.isCurrentConnection(relay.id, connection)) return;
      connection.status = 'disconnected';
      this.emitConnections();
      this.scheduleReconnect(relay, connection);
      return;
    }
    connection.ws.onopen = () => {
      if (!this.isCurrentConnection(relay.id, connection)) return;
      if (!relay.token) {
        this.markConnectionReady(relay.id, connection);
        return;
      }
      if (typeof connection.ws?.protocol === 'string'
        && connection.ws.protocol !== E2EE_SUBPROTOCOL) {
        this.failEncryptedConnection(relay, connection, 'Relay did not negotiate encrypted transport');
        return;
      }
      connection.e2eeTimer = window.setTimeout(() => {
        this.failEncryptedConnection(relay, connection, 'Encrypted relay handshake timed out');
      }, E2EE_HANDSHAKE_TIMEOUT_MS);
      void createE2EEClientHandshake(relay.token).then((handshake) => {
        if (!this.isCurrentConnection(relay.id, connection)
          || connection.ws?.readyState !== WebSocket.OPEN) return;
        connection.e2eeHandshake = handshake;
        connection.ws.send(JSON.stringify(handshake.hello));
      }).catch(() => {
        this.failEncryptedConnection(relay, connection, 'Could not start encrypted relay handshake');
      });
    };
    connection.ws.onclose = () => {
      if (!this.isCurrentConnection(relay.id, connection)) return;
      this.clearE2EETimer(connection);
      this.clearHealthTimer(connection);
      connection.status = 'disconnected';
      this.rejectPendingOperations(relay.id, 'Relay disconnected');
      this.emitConnections();
      this.scheduleReconnect(relay, connection);
    };
    connection.ws.onerror = () => {
      if (!this.isCurrentConnection(relay.id, connection)) return;
      this.clearE2EETimer(connection);
      connection.status = 'disconnected';
      this.rejectPendingOperations(relay.id, 'Relay connection failed');
      this.emitConnections();
      this.scheduleReconnect(relay, connection);
    };
    connection.ws.onmessage = (event) => {
      if (!this.isCurrentConnection(relay.id, connection)) return;
      if (connection.e2eeFailed) return;
      const rawMessage = String(event.data);
      if (!relay.token) {
        this.clearHealthTimer(connection);
        this.reconnectAttempts.delete(relay.id);
        try {
          this.handleMessage(relay.id, JSON.parse(rawMessage) as Record<string, any>);
        } catch {
          // Ignore malformed plaintext frames used by tokenless loopback development.
        }
        return;
      }
      connection.receiveQueue = connection.receiveQueue.then(async () => {
        if (!this.isCurrentConnection(relay.id, connection) || connection.e2eeFailed) return;
        if (!connection.e2eeSession) {
          if (!connection.e2eeHandshake) throw new Error('Encrypted server hello arrived before client hello');
          const completed = await connection.e2eeHandshake.complete(JSON.parse(rawMessage));
          if (!this.isCurrentConnection(relay.id, connection)
            || connection.e2eeFailed
            || connection.ws?.readyState !== WebSocket.OPEN) return;
          connection.e2eeSession = completed.session;
          connection.e2eeHandshake = null;
          connection.ws?.send(completed.finish);
          this.markConnectionReady(relay.id, connection);
          return;
        }
        const plaintext = await connection.e2eeSession.decrypt(rawMessage);
        this.handleMessage(relay.id, JSON.parse(plaintext) as Record<string, any>);
        this.clearHealthTimer(connection);
        this.reconnectAttempts.delete(relay.id);
      }).catch(() => {
        this.failEncryptedConnection(relay, connection, 'Encrypted relay connection failed');
      });
    };
  }

  private markConnectionReady(relayId: string, connection: RelayConnection): void {
    if (!this.isCurrentConnection(relayId, connection)) return;
    this.clearE2EETimer(connection);
    connection.status = 'connected';
    this.emitConnections();
    if (runningAsInstalledApp()) {
      this.sendRaw(relayId, {
        type: 'register_app_origin',
        origin: location.origin,
        protocol: APP_PROTOCOL_VERSION,
      });
    }
    this.sendRaw(relayId, { type: 'refresh_agents' });
  }

  private failEncryptedConnection(
    relay: RelayConfig,
    connection: RelayConnection,
    reason: string,
  ): void {
    if (!this.isCurrentConnection(relay.id, connection) || connection.closed) return;
    connection.e2eeFailed = true;
    connection.e2eeHandshake = null;
    connection.e2eeSession = null;
    this.clearE2EETimer(connection);
    this.clearHealthTimer(connection);
    connection.status = 'disconnected';
    this.rejectPendingOperations(relay.id, reason);
    this.emitConnections();
    connection.ws?.close();
    this.scheduleReconnect(relay, connection);
  }

  private clearE2EETimer(connection: RelayConnection): void {
    if (!connection.e2eeTimer) return;
    window.clearTimeout(connection.e2eeTimer);
    connection.e2eeTimer = null;
  }

  disconnectRelay(id: string): void {
    this.clearSlashCommandCacheForRelay(id);
    for (const paneId of this.pendingPaneReads.keys()) {
      if (paneId.startsWith(`${id}::`)) this.pendingPaneReads.delete(paneId);
    }
    for (const paneId of this.paneWatchesStarted) {
      if (paneId.startsWith(`${id}::`)) this.paneWatchesStarted.delete(paneId);
    }
    const connection = this.connectionsValue.get(id);
    if (!connection) return;
    connection.closed = true;
    if (connection.reconnectTimer) clearTimeout(connection.reconnectTimer);
    this.clearHealthTimer(connection);
    this.clearE2EETimer(connection);
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
      if (!this.sendRaw(relay.id, { type: 'refresh_agents' })) {
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
      // Pane revisions are monotonic only for one relay process. A new socket
      // handshake may follow a relay restart, so discard the retained
      // process-local baseline before its fresh snapshot arrives.
      this.agentsValue = this.agentsValue.map((agent) => {
        if (agent.relay_id !== relayId || agent.pane_revision === undefined) return agent;
        const withoutRevision = { ...agent };
        delete withoutRevision.pane_revision;
        return withoutRevision;
      });
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
      observeAppUpstreamVersion(connection.update.upstream_version);
      this.syncUpdateRestartReconnect(relayId, connection);
      connection.appDeploy = normalizeAppDeployment(message.app_deploy);
      connection.inventory = normalizeAgentInventory(message.inventory, 'ready');
      connection.capabilities = Array.isArray(message.capabilities) ? message.capabilities.filter(Boolean) : [];
      const attentionCapable = connection.capabilities.includes('attention_classification');
      this.agentsValue = this.agentsValue.map((agent) =>
        agent.relay_id === relayId ? normalizeAgentAttention(agent, attentionCapable) : agent,
      );
      this.agents.set(this.agentsValue);
      connection.agentProfiles = Array.isArray(message.agent_profiles)
        ? message.agent_profiles
          .filter((profile: unknown): profile is AgentProfile => {
            if (!profile || typeof profile !== 'object' || !('id' in profile)) return false;
            if (typeof profile.id !== 'string' || !profile.id) return false;
            return !('label' in profile) || profile.label === undefined || typeof profile.label === 'string';
          })
          .sort((left, right) => (
            String(left.label || left.id).localeCompare(
              String(right.label || right.id),
              undefined,
              { sensitivity: 'base' },
            )
            || String(left.id).localeCompare(String(right.id), undefined, { sensitivity: 'base' })
          ))
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
      observeAppUpstreamVersion(connection.update.upstream_version);
      this.syncUpdateRestartReconnect(relayId, connection);
      if (['failed', 'rolled_back'].includes(connection.update.state)) {
        clearPendingRelayUpdate(relayId);
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
      const attentionCapable = Boolean(connection?.capabilities.includes('attention_classification'));
      const incoming = (Array.isArray(message.agents) ? message.agents : [])
        .map((agent: Partial<Agent>) => normalizeAgent(relayId, label, agent, attentionCapable));
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
      const attentionCapable = Boolean(connection?.capabilities.includes('attention_classification'));
      const next = normalizeAgent(relayId, label, { ...message, status: 'blocked' }, attentionCapable);
      const index = this.agentsValue.findIndex((agent) => agent.pane_id === next.pane_id);
      const before = index >= 0 ? this.agentsValue[index] : undefined;
      if (staleAgentRevision(before, next)) return;
      this.blockedSnapshotMisses.delete(next.pane_id);
      if (index >= 0) {
        const copy = [...this.agentsValue];
        copy[index] = mergeAgentDetails(before, next);
        this.agentsValue = copy;
      } else this.agentsValue = [...this.agentsValue, next];
      this.respondingValue.delete(next.pane_id);
      this.responding.set(new Set(this.respondingValue));
      this.agents.set(this.agentsValue);
      return;
    }
    if (message.type === 'agent_update' && message.pane_id) {
      const label = get(this.relayConfigs).find((relay) => relay.id === relayId)?.label || 'relay';
      const attentionCapable = Boolean(connection?.capabilities.includes('attention_classification'));
      const next = normalizeAgent(relayId, label, message, attentionCapable);
      const index = this.agentsValue.findIndex((agent) => agent.pane_id === next.pane_id);
      const before = index >= 0 ? this.agentsValue[index] : undefined;
      if (staleAgentRevision(before, next)) return;
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
    if (message.type === 'pane_unchanged') {
      const paneId = clientPaneId(relayId, String(message.pane_id || ''));
      this.pendingPaneReads.delete(paneId);
      if (typeof message.content_fingerprint === 'string' && message.content_fingerprint) {
        this.paneContentFingerprints.set(paneId, message.content_fingerprint);
      }
      this.startPaneWatch(paneId);
      return;
    }
    if (message.type === 'pane_resync') {
      const paneId = clientPaneId(relayId, String(message.pane_id || ''));
      const watched = this.watchedPanes.get(paneId);
      if (watched) this.readPane(watched, true);
      return;
    }
    if (message.type === 'pane_delta') {
      const paneId = clientPaneId(relayId, String(message.pane_id || ''));
      const frame = this.terminalFramesValue.get(paneId);
      const baseFingerprint = this.paneContentFingerprints.get(paneId);
      const nextContent = frame && baseFingerprint === message.base_fingerprint
        ? applyPaneDelta(frame.content, message.segments)
        : null;
      const watched = this.watchedPanes.get(paneId);
      if (nextContent === null || typeof message.content_fingerprint !== 'string') {
        if (watched) this.readPane(watched, true);
        return;
      }
      this.paneContentFingerprints.set(paneId, message.content_fingerprint);
      this.terminalFramesValue.set(paneId, {
        paneId,
        content: nextContent,
        format: String(message.format || frame?.format || 'plain'),
        truncated: typeof message.truncated === 'boolean' ? message.truncated : frame?.truncated,
        viewportOnly: typeof message.viewport_only === 'boolean' ? message.viewport_only : frame?.viewportOnly,
        viewportRows: typeof message.viewport_rows === 'number' ? message.viewport_rows : frame?.viewportRows,
      });
      this.terminalFrames.set(new Map(this.terminalFramesValue));
      this.mergePaneInteraction(paneId, message);
      if (watched) this.acknowledgePaneFrame(watched, message.content_fingerprint);
      return;
    }
    if (message.type === 'pane_content') {
      const paneId = clientPaneId(relayId, String(message.pane_id || ''));
      this.pendingPaneReads.delete(paneId);
      if (typeof message.content_fingerprint === 'string' && message.content_fingerprint) {
        this.paneContentFingerprints.set(paneId, message.content_fingerprint);
      }
      const nextFrame: TerminalFrame = {
        paneId,
        content: typeof message.content === 'string' ? message.content : '(empty)',
        format: String(message.format || 'plain'),
      };
      if (message.truncated === true) nextFrame.truncated = true;
      if (message.viewport_only === true) nextFrame.viewportOnly = true;
      if (typeof message.viewport_rows === 'number') nextFrame.viewportRows = message.viewport_rows;
      this.terminalFramesValue.set(paneId, nextFrame);
      this.terminalFrames.set(new Map(this.terminalFramesValue));
      this.mergePaneInteraction(paneId, message);
      const watched = this.watchedPanes.get(paneId);
      const contentFingerprint = typeof message.content_fingerprint === 'string'
        ? message.content_fingerprint
        : '';
      if (watched && message.ack_required && contentFingerprint) {
        this.acknowledgePaneFrame(watched, contentFingerprint);
      }
      this.startPaneWatch(paneId);
    }
  }

  private acknowledgePaneFrame(agent: Agent, contentFingerprint: string): void {
    this.sendRaw(agent.relay_id, {
      type: 'pane_applied',
      pane_id: agent.raw_pane_id,
      content_fingerprint: contentFingerprint,
    });
  }

  private mergePaneInteraction(paneId: string, message: Record<string, any>): void {
    if (!Object.prototype.hasOwnProperty.call(message, 'attention_kind')
      && !Object.prototype.hasOwnProperty.call(message, 'interaction')) return;
    const index = this.agentsValue.findIndex((agent) => agent.pane_id === paneId);
    if (index < 0) return;
    const agent = this.agentsValue[index];
    if (!rawBlocked(agent)) return;
    const connection = this.connectionsValue.get(agent.relay_id);
    const attentionCapable = Boolean(connection?.capabilities.includes('attention_classification'));
    const interaction = (message.interaction || null) as QuestionInteraction | null;
    const questionLayout = Boolean(message.question_layout || interaction);
    const next = normalizeAgentAttention({
      ...agent,
      attention_kind: message.attention_kind,
      prompt: message.prompt,
      command: message.command,
      interaction: questionLayout ? interaction : null,
      question_layout: questionLayout,
      options: Array.isArray(message.options) ? message.options : undefined,
    }, attentionCapable);
    this.blockedSnapshotMisses.delete(paneId);
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
    const socket = connection?.ws;
    if (!connection || !socket || socket.readyState !== WebSocket.OPEN || connection.status !== 'connected') {
      return false;
    }
    const plaintext = JSON.stringify(payload);
    if (!connection.relay.token) {
      socket.send(plaintext);
      return true;
    }
    const session = connection.e2eeSession;
    if (!session) return false;
    connection.sendQueue = connection.sendQueue.then(async () => {
      const encrypted = await session.encrypt(plaintext);
      if (!this.isCurrentConnection(relayId, connection)
        || connection.ws !== socket
        || socket.readyState !== WebSocket.OPEN) return;
      socket.send(encrypted);
    }).catch(() => {
      this.failEncryptedConnection(connection.relay, connection, 'Could not encrypt relay message');
    });
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
      const command: Record<string, unknown> = {
        ...payload,
        request_id: requestId,
        protocol: APP_PROTOCOL_VERSION,
      };
      if (payload.type !== 'lease_pane_size' && payload.type !== 'release_pane_size') {
        command.client_id = pushClientId();
      }
      if (!this.sendRaw(relayId, command)) {
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
      observeAppUpstreamVersion(connection.update.upstream_version);
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
        expected_origin: location.origin,
      }, 30_000, true);
      if (result.data?.update && connection === this.connectionsValue.get(relayId)) {
        connection.update = normalizeRelayUpdate(
          result.data.update,
          connection.releaseVersion,
          connection.revision,
        );
        observeAppUpstreamVersion(connection.update.upstream_version);
        this.emitConnections();
      }
    } catch (error) {
      if (error instanceof CommandError && error.data?.update) {
        clearPendingRelayUpdate(relayId);
        if (connection === this.connectionsValue.get(relayId)) {
          connection.update = normalizeRelayUpdate(
            error.data.update,
            connection.releaseVersion,
            connection.revision,
          );
          observeAppUpstreamVersion(connection.update.upstream_version);
          this.emitConnections();
        }
      }
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
      if (result.phase === 'dispatched_unknown') {
        error.data = { ...(result.data || {}), dispatched_unknown: true };
      }
      pending.reject(error);
    }
  }

  private rejectPending<T extends PendingOperation>(
    operations: Map<string, T>,
    relayId: string,
    message: string,
  ): void {
    for (const [requestId, pending] of operations) {
      if (pending.relayId !== relayId) continue;
      clearTimeout(pending.timer);
      operations.delete(requestId);
      pending.reject(new CommandError(message));
    }
  }

  private rejectPendingOperations(relayId: string, message: string): void {
    this.rejectPending(this.pendingRequests, relayId, message);
    this.rejectPending(this.pendingUploads, relayId, message);
  }

  async leasePaneSize(agent: Agent, columns: number): Promise<number> {
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('pane_size_lease')) {
      throw new CommandError('Resize Session requires a relay with pane-size lease support');
    }
    if (!Number.isInteger(columns) || columns < MIN_PANE_SIZE_COLUMNS || columns > MAX_PANE_SIZE_COLUMNS) {
      throw new CommandError(
        `Terminal columns must be between ${MIN_PANE_SIZE_COLUMNS} and ${MAX_PANE_SIZE_COLUMNS}`,
      );
    }
    const result = await this.sendToAgent(agent, { type: 'lease_pane_size', columns });
    if (result.action && result.action !== 'lease_pane_size') {
      throw new CommandError('Relay returned the wrong pane-size lease confirmation');
    }
    const appliedColumns = Number(result.data?.columns);
    if (!Number.isInteger(appliedColumns)
      || appliedColumns < MIN_PANE_SIZE_COLUMNS
      || appliedColumns > MAX_PANE_SIZE_COLUMNS) {
      throw new CommandError('Relay did not confirm the applied terminal columns');
    }
    return appliedColumns;
  }

  async releasePaneSize(agent: Agent): Promise<void> {
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('pane_size_lease')) {
      throw new CommandError('Resize Session requires a relay with pane-size lease support');
    }
    const result = await this.sendToAgent(agent, { type: 'release_pane_size' });
    if (result.action && result.action !== 'release_pane_size') {
      throw new CommandError('Relay returned the wrong pane-size release confirmation');
    }
  }

  readPane(agent: Agent, force = false): void {
    const requestedAt = this.pendingPaneReads.get(agent.pane_id);
    if (!force && requestedAt && Date.now() - requestedAt < PANE_READ_RETRY_MS) return;
    this.paneWatchesStarted.delete(agent.pane_id);
    const sent = this.sendRaw(agent.relay_id, {
      type: 'read_pane',
      pane_id: agent.raw_pane_id,
      lines: get(terminalHistoryLines),
      format: 'ansi',
      content_fingerprint: force ? '' : this.paneContentFingerprints.get(agent.pane_id) || '',
    });
    if (sent) this.pendingPaneReads.set(agent.pane_id, Date.now());
  }

  watchPane(agent: Agent): void {
    this.watchedPanes.set(agent.pane_id, agent);
    this.startPaneWatch(agent.pane_id);
  }

  unwatchPane(agent: Agent): void {
    this.watchedPanes.delete(agent.pane_id);
    this.paneWatchesStarted.delete(agent.pane_id);
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('pane_realtime_delta')) return;
    this.sendRaw(agent.relay_id, {
      type: 'unwatch_pane',
      pane_id: agent.raw_pane_id,
    });
  }

  private restartPaneWatches(): void {
    for (const paneId of [...this.paneWatchesStarted]) {
      this.paneWatchesStarted.delete(paneId);
      this.startPaneWatch(paneId);
    }
  }

  private startPaneWatch(paneId: string): void {
    const agent = this.watchedPanes.get(paneId);
    if (!agent || this.paneWatchesStarted.has(paneId) || this.pendingPaneReads.has(paneId)) return;
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('pane_realtime_delta')) return;
    const contentFingerprint = this.paneContentFingerprints.get(paneId);
    if (!contentFingerprint) return;
    const sent = this.sendRaw(agent.relay_id, {
      type: 'watch_pane',
      pane_id: agent.raw_pane_id,
      lines: get(terminalHistoryLines),
      interval_ms: get(terminalRefreshInterval),
      format: 'ansi',
      content_fingerprint: contentFingerprint,
    });
    if (sent) this.paneWatchesStarted.add(paneId);
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
    for (const relayId of this.connectionsValue.keys()) {
      this.sendRaw(relayId, { type: 'get_activity', limit: 500 });
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
    if (!this.isCurrentConnection(agent.relay_id, connection)
      || connection.ws?.readyState !== WebSocket.OPEN) {
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
      if (!this.sendRaw(agent.relay_id, {
        type: 'upload_image',
        protocol: APP_PROTOCOL_VERSION,
        request_id: requestId,
        client_id: pushClientId(),
        pane_id: agent.raw_pane_id,
        filename: file.name || 'image',
        mime: file.type || 'application/octet-stream',
        data,
      })) {
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

function applyPaneDelta(previous: string, value: unknown): string | null {
  if (!Array.isArray(value)) return null;
  const boundaries = [0];
  for (let index = 0; index < previous.length; index += 1) {
    if (previous.charCodeAt(index) === 10) boundaries.push(index + 1);
  }
  if (boundaries.at(-1) !== previous.length) boundaries.push(previous.length);
  else boundaries.push(previous.length);

  const chunks: string[] = [];
  for (const candidate of value) {
    if (!candidate || typeof candidate !== 'object') return null;
    const segment = candidate as Record<string, unknown>;
    if (segment.copy_lines !== undefined) {
      if (!Number.isInteger(segment.copy_lines) || Number(segment.copy_lines) <= 0) return null;
      const copyLines = Number(segment.copy_lines);
      const copyStart = segment.copy_start === undefined ? 0 : segment.copy_start;
      if (!Number.isInteger(copyStart) || Number(copyStart) < 0) return null;
      const copyEnd = Number(copyStart) + copyLines;
      if (copyEnd > boundaries.length - 1) return null;
      chunks.push(previous.slice(boundaries[Number(copyStart)], boundaries[copyEnd]));
      continue;
    }
    if (typeof segment.text !== 'string') return null;
    chunks.push(segment.text);
  }
  return chunks.join('');
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
