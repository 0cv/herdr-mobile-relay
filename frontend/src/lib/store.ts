import { get, writable } from 'svelte/store';
import {
  APP_PROTOCOL_VERSION,
  importQuickSetup,
  loadRelayConfigs,
  MAX_PANE_SIZE_COLUMNS,
  MAX_PANE_SIZE_ROWS,
  MIN_PANE_SIZE_COLUMNS,
  MIN_PANE_SIZE_ROWS,
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
import { createRelayTransport } from './transports';
import type {
  RelayTransport,
  TransportStatus,
  TransportStatusDetail,
} from './transports';
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
  ConversationPage,
  DirectoryListing,
  QuestionDraft,
  QuestionInteraction,
  RelayConfig,
  RelayConnectionView,
  RelayWorkspace,
  SlashCommand,
  SlashCommandCatalog,
  TerminalFrame,
  ToastMessage,
  WorkspaceFile,
  WorkspaceGitDiff,
  WorkspaceGitStatus,
  WorkspaceTree,
  WorktreeListing,
} from './types';
const COMMAND_TIMEOUT_MS = 15_000;
const ACCEPTED_COMMAND_TIMEOUT_MS = 10_000;
const IMAGE_UPLOAD_TIMEOUT_MS = 60_000;
const BACKGROUND_HEALTH_TIMEOUT_MS = 10_000;
const FOREGROUND_HEALTH_TIMEOUT_MS = 2_000;
const UPDATE_RESTART_RECONNECT_DELAY_MS = 1_000;
const RECONNECT_BASE_DELAY_MS = 1_000;
const RECONNECT_MAX_DELAY_MS = 60_000;
const PANE_READ_RETRY_MS = 35_000;
// A connect attempt older than this is replaced when a revalidation event
// (wake, focus, online, network change) arrives. A healthy dial finishes its
// two handshakes in a couple of seconds; one that predates the event usually
// started before the radio was up, was blackholed, and would otherwise sit
// out the full handshake timeout plus backoff — the "dozens of seconds before
// streaming resumes after sleep" a phone sees in gateway mode.
const STALE_CONNECTING_MS = 5_000;
// Proof-of-life cadence for every connected relay. The gateway reaps a quiet
// phone connection after five minutes and never pings one, so a hidden but
// still-running page loses its socket silently and pays a full re-dial (TLS +
// WS + hello + E2EE) on focus. Two minutes sits well under that reaper and is
// coarser than the once-a-minute floor an intensively throttled background
// timer gets, so it still fires wherever the platform keeps the page running.
const KEEPALIVE_INTERVAL_MS = 120_000;
// After this long hidden, holding the connection open stops being worth the
// radio wakeups: the keepalive stops and the connection is left to lapse.
// Becoming visible resets the bound and resumes the keepalive.
const HIDDEN_KEEPALIVE_MAX_MS = 60 * 60_000;
// With the keepalive running, a healthy connection is never silent for longer
// than its cadence plus slack. A longer gap means the page was frozen, and a
// frozen page cannot have kept its path: the remote side's ICE consent lapses
// in about thirty seconds and the gateway reaper fires at five minutes. So a
// stale connection is a corpse, and probing it only delays the dial.
//
// The slack has to cover an intensively throttled hidden tab, where a
// two-minute interval can drift onto the next once-a-minute wakeup and the
// reply still has to arrive: three minutes is boundary-tight and would
// occasionally redial a healthy connection. Four still sits a full minute
// under the reaper, so nothing a live connection does can reach it.
const FRESH_PROOF_MS = 240_000;
const IMAGE_UPLOAD_MAX_BYTES = 10 * 1024 * 1024;
// Gateway-relayed traffic is metered project bandwidth. Cap scrollback while
// honoring the user's refresh rate; acknowledged deltas keep idle frames off
// the wire and make the selected cadence affordable during normal output.
const RELAYED_HISTORY_LINES = 1_000;
const INVENTORY_REQUIRED_COMMANDS: Record<string, true> = {
  answer_question: true,
  navigate_question: true,
  respond: true,
  clarify_question: true,
  submit_prompt: true,
  send_keys: true,
  send_text: true,
  send_secret: true,
  agent_start: true,
  agent_rename: true,
  tab_reorder: true,
  agent_stop: true,
  agent_clear: true,
  agent_restart: true,
  workspace_create: true,
  workspace_rename: true,
  workspace_reorder: true,
  workspace_close: true,
  worktree_list: true,
  worktree_create: true,
  worktree_open: true,
  worktree_remove: true,
  acknowledge_pane: true,
  upload_image: true,
  copy_agent_response: true,
};

/**
 * How long an app-level ping may go unanswered before the socket counts as
 * half dead. A foregrounded page is what the person is looking at, so it pays
 * a short deadline to notice a stalled resume; a hidden page waits longer
 * rather than churning sockets in the background.
 */
function healthTimeoutMs(): number {
  return document.visibilityState === 'visible'
    ? FOREGROUND_HEALTH_TIMEOUT_MS
    : BACKGROUND_HEALTH_TIMEOUT_MS;
}

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

function normalizeWorkspace(
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

interface RelayConnection extends RelayConnectionView {
  transport: RelayTransport | null;
  reconnectTimer: ReturnType<typeof setTimeout> | null;
  healthTimer: ReturnType<typeof setTimeout> | null;
  updateRestartTimer: ReturnType<typeof setTimeout> | null;
  closed: boolean;
  connectingSince: number;
  /**
   * When the app last heard anything from this relay, including the handshake
   * that made it ready. The keepalive bounds it, and a resume compares it
   * against FRESH_PROOF_MS to tell a resumable path from a dead one.
   */
  lastMessageAt: number;
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

/**
 * A rejection for a frame that was already written to the transport. The
 * relay may have received and applied the command even though no result came
 * back, so callers must never treat a retry as safe (the retry-safety
 * doctrine's `dispatched_unknown` phase). Definitive pre-send failures —
 * capability checks, validation, a relay that never connected, a write the
 * transport refused — stay plain CommandErrors.
 */
function dispatchedUnknownError(message: string): CommandError {
  const error = new CommandError(message);
  error.data = { dispatched_unknown: true };
  return error;
}

class RelayStore {
  readonly relayConfigs = writable<RelayConfig[]>([]);
  readonly connections = writable<Map<string, RelayConnection>>(new Map());
  readonly agents = writable<Agent[]>([]);
  readonly workspaces = writable<RelayWorkspace[]>([]);
  readonly activities = writable<Activity[]>([]);
  readonly terminalFrames = writable<Map<string, TerminalFrame>>(new Map());
  readonly responding = writable<Set<string>>(new Set());
  readonly toast = writable<ToastMessage | null>(null);
  readonly notificationBusy = writable(false);

  private connectionsValue = new Map<string, RelayConnection>();
  private agentsValue: Agent[] = [];
  private workspacesValue: RelayWorkspace[] = [];
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
  private keepaliveTimer: ReturnType<typeof setInterval> | null = null;
  private hidden = false;
  private hiddenSince = 0;
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
    this.stopKeepalive();
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
    // A hybrid relay is addressed by its gateway, not by a relay URL.
    if (!next.url && !next.gatewayUrl) return;
    const relays = get(this.relayConfigs);
    const existing = relays.find((relay) => (next.url
      ? relay.url === next.url
      : relay.gatewayUrl === next.gatewayUrl && relay.token === next.token));
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
    this.removeWorkspacesForRelay(id);
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
      this.workspacesValue = [];
      this.workspaces.set([]);
    }
    this.blockedSnapshotMisses.clear();
    for (const relay of get(this.relayConfigs)) this.connectRelay(relay);
  }

  connectRelay(relay: RelayConfig): void {
    this.disconnectRelay(relay.id);
    const connection: RelayConnection = {
      relay,
      transport: null,
      status: 'connecting',
      path: '',
      activeGatewayUrl: '',
      reconnectTimer: null,
      healthTimer: null,
      updateRestartTimer: null,
      closed: false,
      connectingSince: Date.now(),
      lastMessageAt: 0,
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
    connection.transport = createRelayTransport(relay, {
      onMessage: (message) => {
        if (!this.isCurrentConnection(relay.id, connection)) return;
        // Inbound traffic proves the path is alive, whatever the message says.
        connection.lastMessageAt = Date.now();
        this.clearHealthTimer(connection);
        this.reconnectAttempts.delete(relay.id);
        this.handleMessage(relay.id, message);
      },
      onStatus: (status, detail) => {
        this.applyTransportStatus(relay, connection, status, detail);
      },
    });
    connection.transport.connect();
  }

  private applyTransportStatus(
    relay: RelayConfig,
    connection: RelayConnection,
    status: TransportStatus,
    detail?: TransportStatusDetail,
  ): void {
    if (!this.isCurrentConnection(relay.id, connection) || connection.closed) return;
    if (status === 'connected') {
      const previousPath = connection.path;
      connection.path = detail?.path || connection.transport?.kind || 'websocket';
      // Which candidate answered matters with a list: the app names it rather
      // than the configured head, which may be a gateway that was skipped.
      connection.activeGatewayUrl = connection.path === 'websocket' ? '' : detail?.gatewayUrl || '';
      this.markConnectionReady(relay.id, connection);
      if (!previousPath) {
        // First ready of a fresh session: the relay dropped its watches with
        // the old one, and reads sent while the transport was down were lost
        // silently. Re-read every watched pane so an open terminal streams
        // again within one round trip, not at the next resync interval.
        for (const watched of this.watchedPanes.values()) {
          if (watched.relay_id === relay.id) this.readPane(watched);
        }
      }
      // A path switch changes the relayed-fidelity budget, so panes have to be
      // rewatched at the interval the new path can afford.
      if (previousPath && previousPath !== connection.path) this.restartPaneWatches();
      return;
    }
    if (status === 'connecting') {
      if (connection.status === 'connecting') return;
      connection.status = 'connecting';
      this.emitConnections();
      return;
    }
    this.clearHealthTimer(connection);
    connection.status = 'disconnected';
    this.rejectPendingOperations(relay.id, detail?.reason || 'Relay disconnected');
    this.emitConnections();
    // A fatal close would previously never retry, which stranded phones until
    // a manual reload. `unknown_relay` is a restarting relay nine times out of
    // ten — its registration lapses during every update — so it keeps the
    // normal cadence; every other fatal failure retries at the slowest one.
    const slow = detail?.fatal && detail.code !== 'unknown_relay';
    this.scheduleReconnect(relay, connection, slow ? RECONNECT_MAX_DELAY_MS : 0);
  }

  private markConnectionReady(relayId: string, connection: RelayConnection): void {
    if (!this.isCurrentConnection(relayId, connection) || connection.closed) return;
    // A completed handshake is the freshest proof of life there is.
    connection.lastMessageAt = Date.now();
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

  /**
   * Drops the reconnect backoff so a resumed page retries at once. The caller
   * decides how to reconnect afterwards — this only removes the delay that
   * earlier failures imposed.
   */
  resetReconnectBackoff(): void {
    this.reconnectAttempts.clear();
    for (const connection of this.connectionsValue.values()) {
      if (!connection.reconnectTimer) continue;
      clearTimeout(connection.reconnectTimer);
      connection.reconnectTimer = null;
    }
  }

  /**
   * Bridge-window migration. A relay reached over its legacy WSS URL announces
   * that it also speaks the hybrid transport; the app records its selected
   * gateway first and the remaining cold fallbacks. The legacy relay URL is
   * kept, and the live connection is never interrupted — no QR re-scan or
   * reconnect merely because the relay selected a different gateway.
   */
  private adoptHybridDescriptor(connection: RelayConnection, descriptor: unknown): void {
    if (!descriptor || typeof descriptor !== 'object') return;
    const advertised = descriptor as Record<string, unknown>;
    const gatewayUrl = String(advertised.gateway_url || '').trim();
    if (!gatewayUrl.startsWith('wss://') && !gatewayUrl.startsWith('ws://')) return;
    connection.gatewayVersion = String(advertised.gateway_version || '').slice(0, 32);
    connection.gatewayAvailableVersion = String(
      advertised.gateway_available_version || connection.gatewayAvailableVersion || '',
    ).slice(0, 32);
    const gatewayUrls = Array.isArray(advertised.gateway_urls)
      ? advertised.gateway_urls as string[]
      : [];
    const relay = connection.relay;
    const upgraded = normalizeRelayConfig({
      ...relay,
      transport: 'hybrid',
      gatewayUrl,
      gatewayUrls,
    });
    const currentGateways = relay.gatewayUrls ?? (relay.gatewayUrl ? [relay.gatewayUrl] : []);
    const nextGateways = upgraded.gatewayUrls ?? (upgraded.gatewayUrl ? [upgraded.gatewayUrl] : []);
    if (
      relay.transport === 'hybrid'
      && relay.gatewayUrl === upgraded.gatewayUrl
      && currentGateways.join() === nextGateways.join()
    ) return;
    connection.relay = upgraded;
    const relays = get(this.relayConfigs).map((entry) => (entry.id === relay.id ? upgraded : entry));
    this.relayConfigs.set(relays);
    saveRelayConfigs(relays);
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
    this.clearUpdateRestartTimer(connection);
    connection.transport?.close();
    this.rejectPendingOperations(id, 'Relay disconnected');
    this.connectionsValue.delete(id);
    this.emitConnections();
  }

  /**
   * Records whether the page is hidden. The visibility lifecycle lives in
   * `security.ts`; the store owns the keepalive timer so no component has to.
   */
  setHidden(hidden: boolean): void {
    if (this.hidden === hidden) return;
    this.hidden = hidden;
    this.hiddenSince = hidden ? Date.now() : 0;
    this.syncKeepalive();
  }

  revalidateConnections(timeoutMs = healthTimeoutMs()): void {
    if (!this.reconnectEnabled) return;
    const relays = get(this.relayConfigs);
    for (const relay of relays) {
      const connection = this.connectionsValue.get(relay.id);
      if (connection?.status === 'connecting') {
        // Replace a dial that predates the event this revalidation reacts to:
        // it likely started before the network came back and is blackholed.
        // A young attempt keeps going — focus events fire on every app
        // switch and must not churn healthy connects.
        if (Date.now() - connection.connectingSince >= STALE_CONNECTING_MS) {
          this.connectRelay(relay);
        }
        continue;
      }
      if (connection?.status !== 'connected') {
        this.connectRelay(relay);
        continue;
      }
      // The keepalive gives every healthy connection a freshness bound, so a
      // silence longer than FRESH_PROOF_MS is a corpse: dial now instead of
      // spending the probe timeout proving what the silence already said.
      if (Date.now() - connection.lastMessageAt > FRESH_PROOF_MS) {
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

  /**
   * Keeps the keepalive interval running exactly while there is a connection
   * worth holding open. Driven from `emitConnections`, so every status change
   * is covered without each call site having to remember this.
   */
  private syncKeepalive(): void {
    const wanted = this.reconnectEnabled && !this.keepaliveExhausted() && this.hasConnectedRelay();
    if (wanted === (this.keepaliveTimer !== null)) return;
    if (!wanted) {
      this.stopKeepalive();
      return;
    }
    this.keepaliveTimer = setInterval(() => this.sendKeepalive(), KEEPALIVE_INTERVAL_MS);
  }

  private stopKeepalive(): void {
    if (!this.keepaliveTimer) return;
    clearInterval(this.keepaliveTimer);
    this.keepaliveTimer = null;
  }

  /** True once a hidden page has held its connections open long enough. */
  private keepaliveExhausted(): boolean {
    return this.hidden && Date.now() - this.hiddenSince >= HIDDEN_KEEPALIVE_MAX_MS;
  }

  private hasConnectedRelay(): boolean {
    for (const connection of this.connectionsValue.values()) {
      if (connection.status === 'connected') return true;
    }
    return false;
  }

  /**
   * Sends one proof-of-life frame per connected relay and arms the health
   * timer for the reply. `refresh_agents` is the ping: every deployed relay
   * answers it with a small inventory snapshot, the reply doubles as the
   * health signal, and traffic in either direction resets the gateway's idle
   * clock. A dedicated ping type would need protocol versioning and
   * capability gating to save a handful of bytes every two minutes.
   */
  private sendKeepalive(): void {
    if (this.keepaliveExhausted()) {
      this.stopKeepalive();
      return;
    }
    for (const [relayId, connection] of this.connectionsValue) {
      // The keepalive proves a path is alive; it never dials one. Relays that
      // are disconnected or still dialing belong to the reconnect paths.
      if (connection.status !== 'connected' || connection.healthTimer) continue;
      connection.healthTimer = setTimeout(() => {
        if (!this.isCurrentConnection(relayId, connection)) return;
        connection.healthTimer = null;
        this.failKeepalive(relayId, connection);
      }, BACKGROUND_HEALTH_TIMEOUT_MS);
      if (this.sendRaw(relayId, { type: 'refresh_agents' })) continue;
      this.clearHealthTimer(connection);
      this.failKeepalive(relayId, connection);
    }
  }

  /**
   * An unanswered keepalive means the socket is gone. A visible page replaces
   * it at once: someone is watching. A hidden page must not — redialing in the
   * background churns the socket and the radio for nobody, and the resume
   * revalidation dials anything not connected the moment the app comes back.
   */
  private failKeepalive(relayId: string, connection: RelayConnection): void {
    if (!this.hidden) {
      this.connectRelay(connection.relay);
      return;
    }
    this.retireConnection(relayId, connection, 'Relay disconnected');
  }

  /**
   * Drops a dead transport without scheduling a dial, leaving the relay
   * `disconnected` for the next revalidation to pick up. Marking the
   * connection closed also suppresses the transport's own disconnect status,
   * which would otherwise put the relay straight back on the reconnect ladder.
   */
  private retireConnection(relayId: string, connection: RelayConnection, reason: string): void {
    this.clearHealthTimer(connection);
    connection.closed = true;
    connection.transport?.close();
    connection.status = 'disconnected';
    this.rejectPendingOperations(relayId, reason);
    this.emitConnections();
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

  private scheduleReconnect(relay: RelayConfig, connection: RelayConnection, floorMs = 0): void {
    if (connection.closed || !this.reconnectEnabled || connection.reconnectTimer) return;
    if (!this.isCurrentConnection(relay.id, connection)) return;
    const attempt = (this.reconnectAttempts.get(relay.id) || 0) + 1;
    this.reconnectAttempts.set(relay.id, attempt);
    const baseDelay = Math.max(floorMs, Math.min(
      RECONNECT_MAX_DELAY_MS,
      RECONNECT_BASE_DELAY_MS * 2 ** Math.min(attempt - 1, 5),
    ));
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
      connection.gatewayAvailableVersion = connection.update.available_version || connection.releaseVersion;
      this.syncUpdateRestartReconnect(relayId, connection);
      connection.appDeploy = normalizeAppDeployment(message.app_deploy);
      connection.inventory = normalizeAgentInventory(message.inventory, 'ready');
      connection.capabilities = Array.isArray(message.capabilities) ? message.capabilities.filter(Boolean) : [];
      this.adoptHybridDescriptor(connection, message.hybrid);
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
      if (connection.update.available_version) {
        connection.gatewayAvailableVersion = connection.update.available_version;
      }
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
    if (message.type === 'workspaces') {
      if (
        connection
        && connection.inventory.state !== 'ready'
        && !connection.inventory.stale
      ) return;
      const relayLabel = get(this.relayConfigs).find((relay) => relay.id === relayId)?.label || 'relay';
      const incoming = (Array.isArray(message.workspaces) ? message.workspaces : [])
        .map((workspace: unknown) => workspace && typeof workspace === 'object'
          ? normalizeWorkspace(relayId, relayLabel, workspace as Record<string, unknown>)
          : null)
        .filter((workspace: RelayWorkspace | null): workspace is RelayWorkspace => workspace !== null);
      this.workspacesValue = [
        ...this.workspacesValue.filter((workspace) => workspace.relay_id !== relayId),
        ...incoming,
      ];
      this.workspaces.set(this.workspacesValue);
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
      const paneId = clientPaneId(relayId, message.pane_id);
      const index = this.agentsValue.findIndex((agent) => agent.pane_id === paneId);
      const before = index >= 0 ? this.agentsValue[index] : undefined;
      const source = before && rawBlocked(message)
        && !Object.prototype.hasOwnProperty.call(message, 'attention_kind')
        ? { ...before, ...message }
        : message;
      const next = normalizeAgent(relayId, label, source, attentionCapable);
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
      // Released relays encoded metadata-only deltas as null. Preserve the
      // terminal bytes when both fingerprints prove the content is unchanged.
      const metadataOnly = baseFingerprint === message.base_fingerprint
        && message.base_fingerprint === message.content_fingerprint
        && (message.segments === null
          || (Array.isArray(message.segments) && message.segments.length === 0));
      const nextContent = frame && baseFingerprint === message.base_fingerprint
        ? metadataOnly ? frame.content : applyPaneDelta(frame.content, message.segments)
        : null;
      const watched = this.watchedPanes.get(paneId);
      if (nextContent === null || typeof message.content_fingerprint !== 'string') {
        if (watched) this.readPane(watched, true);
        return;
      }
      this.paneContentFingerprints.set(paneId, message.content_fingerprint);
      const deltaNoEcho = typeof message.no_echo === 'boolean' ? message.no_echo : frame?.noEcho;
      this.terminalFramesValue.set(paneId, {
        paneId,
        content: nextContent,
        format: String(message.format || frame?.format || 'plain'),
        truncated: typeof message.truncated === 'boolean' ? message.truncated : frame?.truncated,
        viewportOnly: typeof message.viewport_only === 'boolean' ? message.viewport_only : frame?.viewportOnly,
        viewportRows: typeof message.viewport_rows === 'number' ? message.viewport_rows : frame?.viewportRows,
        noEcho: deltaNoEcho,
        noEchoPrompt: deltaNoEcho
          ? typeof message.no_echo_prompt === 'string' ? message.no_echo_prompt : frame?.noEchoPrompt
          : undefined,
        resizeSettling: message.resize_settling === true,
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
      if (message.no_echo === true) {
        nextFrame.noEcho = true;
        if (typeof message.no_echo_prompt === 'string') nextFrame.noEchoPrompt = message.no_echo_prompt;
      }
      if (message.resize_settling === true) nextFrame.resizeSettling = true;
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
    const index = this.agentsValue.findIndex((agent) => agent.pane_id === paneId);
    if (index < 0) return;
    const agent = this.agentsValue[index];
    if (!agent.attention_capable || message.attention_kind !== 'question' || !message.interaction) return;
    this.agentsValue[index] = {
      ...agent,
      status: 'blocked',
      attention_kind: 'question',
      interaction: message.interaction as QuestionInteraction,
    };
    this.blockedSnapshotMisses.delete(paneId);
    this.agents.set(this.agentsValue);
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

  private removeWorkspacesForRelay(relayId: string): void {
    this.workspacesValue = this.workspacesValue.filter((workspace) => workspace.relay_id !== relayId);
    this.workspaces.set(this.workspacesValue);
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
    if (!connection || connection.status !== 'connected') return false;
    return connection.transport?.send(payload) ?? false;
  }

  sendCommand(
    relayId: string,
    payload: Record<string, any>,
    timeoutMs = COMMAND_TIMEOUT_MS,
    allowProtocolMismatch = false,
  ): Promise<CommandResult> {
    const connection = this.connectionsValue.get(relayId);
    if (!connection || connection.status !== 'connected') {
      return Promise.reject(new CommandError('Relay is not connected'));
    }
    const protocolError = relayProtocolError(connection);
    if (protocolError && !allowProtocolMismatch) return Promise.reject(new CommandError(protocolError));
    if (INVENTORY_REQUIRED_COMMANDS[String(payload.type)] && connection.inventory.state !== 'ready') {
      return Promise.reject(new CommandError(
        connection.inventory.message || 'Herdr agent inventory is not ready on this computer',
      ));
    }
    const requestId = commandRequestId();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pendingRequests.delete(requestId);
        reject(dispatchedUnknownError('Relay confirmation timed out'));
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

  /**
   * Answers a recognized no-echo prompt. The relay types the secret as single
   * runes so no bracketed-paste wrapper reaches a termios noecho read, and it
   * never journals the text.
   */
  sendSecret(agent: Agent, text: string): Promise<CommandResult> {
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('secret_input')) {
      return Promise.reject(new CommandError('This relay does not support password prompts'));
    }
    if (!text) return Promise.reject(new CommandError('Enter the password first'));
    return this.sendToAgent(agent, { type: 'send_secret', text });
  }

  reorderTab(agent: Agent, insertIndex: number): Promise<CommandResult> {
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('tab_reorder')) {
      return Promise.reject(new CommandError('This relay does not support tab ordering'));
    }
    if (!Number.isInteger(insertIndex) || insertIndex < 0) {
      return Promise.reject(new CommandError('Tab position is invalid'));
    }
    return this.sendToAgent(agent, { type: 'tab_reorder', insert_index: insertIndex });
  }

  private workspaceManagementAvailable(relayId: string, capability = 'workspace_management'): RelayConnection {
    const connection = this.connectionsValue.get(relayId);
    if (!connection?.capabilities.includes(capability)) {
      throw new CommandError(capability === 'worktree_management'
        ? 'This relay does not support worktree management'
        : 'This relay does not support workspace management');
    }
    return connection;
  }

  async createWorkspace(relayId: string, cwd: string, label: string): Promise<CommandResult> {
    this.workspaceManagementAvailable(relayId);
    const result = await this.sendCommand(relayId, { type: 'workspace_create', cwd, label }, 45_000);
    this.requestAgents();
    return result;
  }

  async renameWorkspace(workspace: RelayWorkspace, label: string): Promise<CommandResult> {
    this.workspaceManagementAvailable(workspace.relay_id);
    const result = await this.sendCommand(workspace.relay_id, {
      type: 'workspace_rename', workspace_id: workspace.workspace_id, label,
    });
    this.requestAgents();
    return result;
  }

  async reorderWorkspaceBlock(
    relayId: string,
    workspaceIds: string[],
    beforeWorkspaceId: string,
    legacyInsertIndex: number,
  ): Promise<CommandResult> {
    const connection = this.workspaceManagementAvailable(relayId);
    if (!workspaceIds.length || new Set(workspaceIds).size !== workspaceIds.length) {
      throw new CommandError('Workspace selection is invalid');
    }
    const payload = connection.capabilities.includes('workspace_reorder_block')
      ? {
          type: 'workspace_reorder',
          workspace_ids: workspaceIds,
          before_workspace_id: beforeWorkspaceId,
        }
      : workspaceIds.length === 1 && Number.isInteger(legacyInsertIndex) && legacyInsertIndex >= 0
        ? {
            type: 'workspace_reorder',
            workspace_id: workspaceIds[0],
            insert_index: legacyInsertIndex,
          }
        : null;
    if (!payload) {
      throw new CommandError('Update Herdr to reorder a workspace with linked worktrees');
    }
    const result = await this.sendCommand(relayId, payload);
    this.requestAgents();
    return result;
  }

  async closeWorkspace(workspace: RelayWorkspace): Promise<CommandResult> {
    this.workspaceManagementAvailable(workspace.relay_id);
    const result = await this.sendCommand(workspace.relay_id, {
      type: 'workspace_close', workspace_id: workspace.workspace_id,
    }, 30_000);
    this.requestAgents();
    return result;
  }

  async listWorktrees(workspace: RelayWorkspace): Promise<WorktreeListing> {
    this.workspaceManagementAvailable(workspace.relay_id, 'worktree_management');
    const result = await this.sendCommand(workspace.relay_id, {
      type: 'worktree_list', workspace_id: workspace.workspace_id,
    }, 30_000);
    const listing = result.data as unknown as WorktreeListing;
    if (!listing?.source || !Array.isArray(listing.worktrees)) {
      throw new CommandError('Relay returned an invalid worktree listing');
    }
    return listing;
  }

  async createWorktree(
    workspace: RelayWorkspace,
    options: { branch: string; base?: string; label?: string },
  ): Promise<CommandResult> {
    this.workspaceManagementAvailable(workspace.relay_id, 'worktree_management');
    const result = await this.sendCommand(workspace.relay_id, {
      type: 'worktree_create',
      workspace_id: workspace.workspace_id,
      branch: options.branch,
      base: options.base || '',
      label: options.label || '',
    }, 75_000);
    this.requestAgents();
    return result;
  }

  async openWorktree(
    workspace: RelayWorkspace,
    options: { path?: string; branch?: string; label?: string },
  ): Promise<CommandResult> {
    this.workspaceManagementAvailable(workspace.relay_id, 'worktree_management');
    const result = await this.sendCommand(workspace.relay_id, {
      type: 'worktree_open',
      workspace_id: workspace.workspace_id,
      path: options.path || '',
      branch: options.branch || '',
      label: options.label || '',
    }, 75_000);
    this.requestAgents();
    return result;
  }

  async removeWorktree(workspace: RelayWorkspace, force = false): Promise<CommandResult> {
    this.workspaceManagementAvailable(workspace.relay_id, 'worktree_management');
    const result = await this.sendCommand(workspace.relay_id, {
      type: 'worktree_remove', workspace_id: workspace.workspace_id, force,
    }, 75_000);
    this.requestAgents();
    return result;
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
        pending.reject(dispatchedUnknownError('Relay confirmation timed out'));
      }, ACCEPTED_COMMAND_TIMEOUT_MS);
      this.showToast('Command accepted; waiting for agent…');
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
      // Every entry here survived its write, so the frame is already on the
      // wire and the relay may act on it after this rejection.
      pending.reject(dispatchedUnknownError(message));
    }
  }

  private rejectPendingOperations(relayId: string, message: string): void {
    this.rejectPending(this.pendingRequests, relayId, message);
    this.rejectPending(this.pendingUploads, relayId, message);
  }

  async leasePaneSize(
    agent: Agent,
    columns: number,
    rows = 0,
  ): Promise<{ columns: number; rows: number }> {
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('pane_size_lease')) {
      throw new CommandError('Relay lacks pane-size lease support');
    }
    if (!Number.isInteger(columns) || columns < MIN_PANE_SIZE_COLUMNS || columns > MAX_PANE_SIZE_COLUMNS) {
      throw new CommandError(
        `Terminal columns must be between ${MIN_PANE_SIZE_COLUMNS} and ${MAX_PANE_SIZE_COLUMNS}`,
      );
    }
    // Rows ride the same lease only when the relay understands them; an old
    // relay would silently ignore the field and the client must not believe
    // a height was applied.
    const leaseRows = connection.capabilities.includes('pane_size_lease_rows') ? rows : 0;
    if (leaseRows !== 0
      && (!Number.isInteger(leaseRows) || leaseRows < MIN_PANE_SIZE_ROWS || leaseRows > MAX_PANE_SIZE_ROWS)) {
      throw new CommandError(
        `Terminal rows must be between ${MIN_PANE_SIZE_ROWS} and ${MAX_PANE_SIZE_ROWS}`,
      );
    }
    const payload: Record<string, any> = { type: 'lease_pane_size', columns };
    if (leaseRows) payload.rows = leaseRows;
    const result = await this.sendToAgent(agent, payload);
    if (result.action && result.action !== 'lease_pane_size') {
      throw new CommandError('Wrong pane-size lease confirmation');
    }
    const appliedColumns = Number(result.data?.columns);
    if (!Number.isInteger(appliedColumns)
      || appliedColumns < MIN_PANE_SIZE_COLUMNS
      || appliedColumns > MAX_PANE_SIZE_COLUMNS) {
      throw new CommandError('Relay did not confirm the applied terminal columns');
    }
    let appliedRows = 0;
    if (leaseRows) {
      appliedRows = Number(result.data?.rows);
      if (!Number.isInteger(appliedRows) || appliedRows < 1) {
        throw new CommandError('Relay did not confirm the applied terminal rows');
      }
    }
    return { columns: appliedColumns, rows: appliedRows };
  }

  async releasePaneSize(agent: Agent): Promise<void> {
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('pane_size_lease')) {
      throw new CommandError('Relay lacks pane-size lease support');
    }
    const result = await this.sendToAgent(agent, { type: 'release_pane_size' });
    if (result.action && result.action !== 'release_pane_size') {
      throw new CommandError('Relay returned the wrong pane-size release confirmation');
    }
  }

  async getConversationHistory(agent: Agent, before = '', limit = 80): Promise<ConversationPage> {
    const result = await this.sendToAgent(agent, {
      type: 'get_conversation_history',
      before,
      limit,
    }, 20_000);
    const data = result.data || {};
    const entries = Array.isArray(data.entries)
      ? data.entries
        .filter((entry: unknown): entry is Record<string, unknown> => Boolean(entry && typeof entry === 'object'))
        .map((entry) => {
          const tools = Array.isArray(entry.tools)
            ? entry.tools
              .filter((tool: unknown): tool is Record<string, unknown> => Boolean(tool && typeof tool === 'object'))
              .map((tool) => ({
                id: String(tool.id || ''),
                name: String(tool.name || 'Tool').slice(0, 160),
                input: String(tool.input || ''),
                output: String(tool.output || ''),
                error: tool.error === true,
                truncated: tool.truncated === true,
              }))
            : [];
          return {
            id: String(entry.id || ''),
            timestamp: String(entry.timestamp || ''),
            role: entry.role === 'assistant' ? 'assistant' as const : 'user' as const,
            text: String(entry.text || ''),
            tools,
            truncated: entry.truncated === true,
          };
        })
        .filter((entry) => entry.id && (entry.text || entry.tools.length))
      : [];
    return {
      available: data.available === true,
      reason: String(data.reason || ''),
      entries,
      hasMore: data.has_more === true,
      total: Math.max(0, Number(data.total) || 0),
      fileTruncated: data.file_truncated === true,
    };
  }

  /**
   * Terminal history the active path can afford. Gateway-relayed traffic is
   * metered project bandwidth, so it caps scrollback; every path honors the
   * user's selected refresh interval.
   */
  private paneBudget(relayId: string): { lines: number; intervalMs: number } {
    const lines = get(terminalHistoryLines);
    const intervalMs = get(terminalRefreshInterval);
    if (this.connectionsValue.get(relayId)?.path !== 'gateway') return { lines, intervalMs };
    return { lines: Math.min(lines, RELAYED_HISTORY_LINES), intervalMs };
  }

  readPane(agent: Agent, force = false): void {
    const requestedAt = this.pendingPaneReads.get(agent.pane_id);
    if (!force && requestedAt && Date.now() - requestedAt < PANE_READ_RETRY_MS) return;
    this.paneWatchesStarted.delete(agent.pane_id);
    const sent = this.sendRaw(agent.relay_id, {
      type: 'read_pane',
      pane_id: agent.raw_pane_id,
      lines: this.paneBudget(agent.relay_id).lines,
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
    const budget = this.paneBudget(agent.relay_id);
    const sent = this.sendRaw(agent.relay_id, {
      type: 'watch_pane',
      pane_id: agent.raw_pane_id,
      lines: budget.lines,
      interval_ms: budget.intervalMs,
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
    if (agentStatusGroup(agent) === 'done') {
      this.agentsValue = this.agentsValue.map((item) => item.pane_id === agent.pane_id ? { ...item, status: 'idle' } : item);
      this.agents.set(this.agentsValue);
    }
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
        ? 'Accepted; agent still appears blocked.'
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
        other_text: draft.otherSelected ? draft.otherText : '',
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
      ? {
          ...item,
          status: interaction ? 'blocked' : 'working',
          attention_kind: interaction ? 'question' : undefined,
          interaction,
          question_layout: Boolean(interaction),
        }
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

  private workspaceInspectionAvailable(agent: Agent): void {
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('workspace_inspection')) {
      throw new CommandError('This relay does not support workspace inspection.');
    }
    if (!String(agent.cwd || '').trim()) {
      throw new CommandError('This agent does not report a workspace path.');
    }
  }

  async loadWorkspaceTree(agent: Agent): Promise<WorkspaceTree> {
    this.workspaceInspectionAvailable(agent);
    const result = await this.sendToAgent(agent, { type: 'workspace_tree' }, 20_000);
    const data = result.data as unknown as WorkspaceTree;
    if (!data || typeof data.root !== 'string' || !Array.isArray(data.entries)) {
      throw new CommandError('Relay returned an invalid workspace tree.');
    }
    return data;
  }

  async loadWorkspaceFile(agent: Agent, path: string): Promise<WorkspaceFile> {
    this.workspaceInspectionAvailable(agent);
    const result = await this.sendToAgent(agent, { type: 'workspace_file', path }, 20_000);
    const data = result.data as unknown as WorkspaceFile;
    if (!data || data.path !== path || !['text', 'image'].includes(data.kind)) {
      throw new CommandError('Relay returned an invalid workspace preview.');
    }
    return data;
  }

  async loadWorkspaceGitStatus(agent: Agent): Promise<WorkspaceGitStatus> {
    this.workspaceInspectionAvailable(agent);
    const result = await this.sendToAgent(agent, { type: 'workspace_git_status' }, 20_000);
    const data = result.data as unknown as WorkspaceGitStatus;
    if (!data || typeof data.available !== 'boolean' || !Array.isArray(data.files)) {
      throw new CommandError('Relay returned an invalid Git status.');
    }
    return data;
  }

  async loadWorkspaceGitDiff(agent: Agent, path: string): Promise<WorkspaceGitDiff> {
    this.workspaceInspectionAvailable(agent);
    const result = await this.sendToAgent(agent, { type: 'workspace_git_diff', path }, 20_000);
    const data = result.data as unknown as WorkspaceGitDiff;
    if (!data || data.path !== path || typeof data.diff !== 'string') {
      throw new CommandError('Relay returned an invalid Git diff.');
    }
    return data;
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
    if (!connection || connection.status !== 'connected') throw new CommandError('Relay is not connected.');
    const protocolError = relayProtocolError(connection);
    if (protocolError) throw new CommandError(protocolError);
    const requestId = `upload-${Date.now()}-${Math.random().toString(36).slice(2)}`;
    const data = await readFileAsDataUrl(file);
    if (!this.isCurrentConnection(agent.relay_id, connection)
      || connection.status !== 'connected') {
      throw new CommandError('Relay disconnected before the image could be uploaded.');
    }
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pendingUploads.delete(requestId);
        reject(dispatchedUnknownError('Image upload did not finish in time.'));
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
    // Every status change funnels through here, so this is the one place the
    // keepalive has to be told that a relay came up or went away.
    this.syncKeepalive();
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
