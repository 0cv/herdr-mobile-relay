import { get, writable } from 'svelte/store';
import { base64Encode, base64UrlEncode } from './base64url';
import { isRecord } from './record';
import { releasePendingResources, resetPendingDeadline, registerPendingOperation } from './pending-operations';
import { dispatchFlag } from './composer';
import { inputContextIdentity } from './input-target-state';
import { terminalInputDelivery } from './terminal-input-delivery.svelte';
import { piPromptTarget, piPromptDelivery, validPiPromptText, type PiPromptCapability, type PiPromptLease } from './pi-prompt.svelte';
import { clearConversationPreviews, clearConversationPreviewsForRelay } from './conversation-cache-control';
import { normalizeConversationBatch, normalizeConversationPage } from './conversation-page';
import {
  AttachmentBatchController,
  type AttachmentUploadCallbacks,
  type UploadBeginResult,
  type UploadChunkResult,
  type UploadFinishResult,
} from './attachments';
import {
  canInviteFrom,
  importQuickSetup,
  loadRelayConfigs,
  MAX_PANE_SIZE_COLUMNS,
  MAX_PANE_SIZE_ROWS,
  MIN_PANE_SIZE_COLUMNS,
  MIN_PANE_SIZE_ROWS,
  normalizeRelayConfig,
  quickSetupConfig,
  quickSetupInvitation,
  RELAY_KEY_BYTES,
  runningAsInstalledApp,
  saveRelayConfigs,
  shouldDeferPairingConnection,
  shouldRetainSetupFragment,
} from './config';
import { gatewayRendezvous } from './gateway-credentials';
import {
  BrowserDeviceCredentialStore,
  commitDeviceEnrollment,
  ExpiredDeviceInvitation,
  isAuthenticationSecret,
  isAuthenticationVersion,
  isDeviceRole,
  isInvitationId,
  type RelayDeviceCredential,
  type RelayInvitation,
  type CreateInvitationIntent,
  type DeviceSummary,
  type RenameDeviceIntent,
  type ResetDevicesIntent,
  type RevokeDeviceIntent,
} from './device-auth';
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
import { PaneStreams } from './pane-streams';
import { reportAppAnomaly } from './crash-banner';
import { CommandError, dispatchedUnknownError } from './command-error';
import {
  normalizeAgentInventory,
  normalizeAgentTargetFields,
  normalizeHerdrStatus,
  normalizePushPolicy,
  normalizeSlashCommandCatalog,
  normalizeWorkspace,
} from './wire-normalizers';
import {
  parseActionReceipt,
  parseApiError,
  parseCommandErrorDetail,
  commandRequestId,
  relayProtocolError,
  RELAY_PROTOCOL_VERSION,
} from './protocol';
import {
  agentTargetPayload,
  agentTargetsDiffer,
  targetRefForAgent,
  targetRefMatchesAgent,
} from './resource-id';
import { adoptRelaySpeech, isSpeechLanguage, type SpeechRequest } from './speech';
import {
  type DevicePushPolicy,
  type PushTestState,
} from './push-policy';
import { createRelayTransport } from './transports';
import type {
  RelayTransport,
  TransportStatus,
  TransportStatusDetail,
} from './transports';
import {
  clearPaneAgentViewOverridesForRelay,
  pruneHiddenServers,
  terminalRefreshInterval,
  terminalResizeSession,
} from './preferences';
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
  CommandResult,
  ConversationHistoryRequest,
  ConversationPage,
  DirectoryListing,
  QuestionDraft,
  QuestionInteraction,
  RelayConfig,
  RelayConnectionView,
  RelaySpeechVoice,
  RelayWorkspace,
  SlashCommandCatalog,
  TerminalFrame,
  TargetRef,
  ToastMessage,
} from './types';
const COMMAND_TIMEOUT_MS = 15_000;
const BOOTSTRAP_PAIRING_SESSION_PREFIX = 'herdr_browser_pairing:';
const AGENT_START_TIMEOUT_MS = 60_000;
const ACCEPTED_COMMAND_TIMEOUT_MS = 10_000;
const ATTACHMENT_UPLOAD_TIMEOUT_MS = 60_000;
const BACKGROUND_HEALTH_TIMEOUT_MS = 10_000;
const UPDATE_RESTART_RECONNECT_DELAY_MS = 1_000;
const RECONNECT_BASE_DELAY_MS = 1_000;
const RECONNECT_MAX_DELAY_MS = 60_000;
const UTF8_ENCODER = new TextEncoder();
// A connect attempt older than this is replaced when a revalidation event
// (wake, focus, online, network change) arrives. A healthy dial finishes its
// two handshakes in a couple of seconds; one that predates the event usually
// started before the radio was up, was blackholed, and would otherwise sit
// out the full handshake timeout plus backoff — the "dozens of seconds before
// streaming resumes after sleep" a phone sees in gateway mode.
const STALE_CONNECTING_MS = 5_000;
const PAIRING_DEFERRED_MESSAGE = 'Add Herdr to the iPhone or iPad Home Screen, then open it there to finish pairing.';
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
const INVENTORY_REQUIRED_COMMANDS: Record<string, true> = {
  answer_question: true,
  navigate_question: true,
  respond: true,
  clarify_question: true,
  submit_prompt: true,
  send_keys: true,
  send_input: true,
  send_terminal_text: true,
  send_filter_text: true,
  send_secret: true,
  agent_start: true,
  agent_rename: true,
  tab_reorder: true,
  agent_stop: true,
  agent_clear: true,
  workspace_create: true,
  workspace_rename: true,
  workspace_reorder: true,
  workspace_close: true,
  worktree_list: true,
  worktree_create: true,
  worktree_open: true,
  worktree_remove: true,
  acknowledge_pane: true,
  copy_agent_response: true,
};

interface RelayConnection extends RelayConnectionView {
  transport: RelayTransport | null;
  reconnectTimer: ReturnType<typeof setTimeout> | null;
  healthTimer: ReturnType<typeof setTimeout> | null;
  updateRestartTimer: ReturnType<typeof setTimeout> | null;
  delayedPaneReadTimers: Set<ReturnType<typeof setTimeout>>;
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
  cleanup?: () => void;
}

interface PendingRequest extends PendingOperation {
  action: string;
  actionId?: string;
  resolve: (result: CommandResult) => void;
}

interface PendingUpload extends PendingOperation {
  responseType: string;
  resolve: (result: Record<string, any>) => void;
}

interface SlashCommandCacheEntry {
  identity: string;
  catalog: SlashCommandCatalog;
}

interface PendingSlashCommands {
  identity: string;
  promise: Promise<SlashCommandCatalog>;
}

class RelayStore {
  readonly relayConfigs = writable<RelayConfig[]>([]);
  readonly connections = writable<Map<string, RelayConnection>>(new Map());
  readonly agents = writable<Agent[]>([]);
  readonly workspaces = writable<RelayWorkspace[]>([]);
  readonly activities = writable<Activity[]>([]);
  readonly responding = writable<Set<string>>(new Set());
  readonly toast = writable<ToastMessage | null>(null);
  readonly devices = writable<Map<string, DeviceSummary[]>>(new Map());
  readonly notificationBusy = writable(false);
  readonly pushPolicies = writable<Map<string, DevicePushPolicy>>(new Map());
  readonly pushTests = writable<Map<string, PushTestState>>(new Map());

  private connectionsValue = new Map<string, RelayConnection>();
  private readonly panes = new PaneStreams({
    connection: (relayId) => this.connectionsValue.get(relayId),
    agents: () => this.agentsValue,
    sendRaw: (relayId, payload) => this.sendRaw(relayId, payload),
    applyInteraction: (paneId, message) => this.mergePaneInteraction(paneId, message),
  });
  readonly terminalFrames = this.panes.frames;
  private agentsValue: Agent[] = [];
  private workspacesValue: RelayWorkspace[] = [];
  private activitiesValue: Activity[] = [];
  private respondingValue = new Set<string>();
  private devicesValue = new Map<string, DeviceSummary[]>();
  private pushPoliciesValue = new Map<string, DevicePushPolicy>();
  private pushTestsValue = new Map<string, PushTestState>();
  private blockedSnapshotMisses = new Map<string, number>();
  private pendingRequests = new Map<string, PendingRequest>();
  private pendingUploads = new Map<string, PendingUpload>();
  private secretReadAfter = new Map<string, number>();
  // Panes this phone may hold a size lease on, kept outside TerminalView so
  // turning Resize Session off can release panes whose view already closed
  // and are only in the relay's navigation grace.
  private ownedPaneLeases = new Map<string, Agent>();
  private paneLeaseEpoch = 0;
  private slashCommandCache = new Map<string, SlashCommandCacheEntry>();
  private pendingSlashCommands = new Map<string, PendingSlashCommands>();
  private respondingTimers = new Map<string, ReturnType<typeof setTimeout>>();
  private reconnectAttempts = new Map<string, number>();
  private deferredPairingRelays = new Set<string>();
  private reconnectEnabled = true;
  private keepaliveTimer: ReturnType<typeof setInterval> | null = null;
  private hidden = false;
  private hiddenSince = 0;
  private toastId = 0;
  private pushConfigHandler: ((relayId: string) => void) | null = null;
  private readonly deviceCredentials = new BrowserDeviceCredentialStore(localStorage);

  constructor() {
    let previousRefreshInterval = get(terminalRefreshInterval);
    terminalRefreshInterval.subscribe((value) => {
      if (value === previousRefreshInterval) return;
      previousRefreshInterval = value;
      this.panes.restartWatches();
    });
    terminalResizeSession.subscribe((enabled) => {
      if (!enabled) this.releaseAllPaneSizeLeases();
    });
  }

  initialize(connect = true): void {
    let relays = loadRelayConfigs();
    this.deferredPairingRelays.clear();
    const setup = quickSetupConfig(location);
    const invitation = quickSetupInvitation(location);
    const imported = importQuickSetup(relays, location);
    if (imported) {
      clearChangedRelayPreviews(relays, imported);
      relays = imported;
      const relay = setup ? this.relayForSetup(relays, setup) : undefined;
      const pairingAccepted = !!relay && this.setupPairingAccepted(relay, location, invitation);
      if (relay) {
        const deferred = this.shouldDeferPairing(location, pairingAccepted);
        this.applyPairingDeferral(relay, location, deferred);
        if (deferred) {
          this.showToast(PAIRING_DEFERRED_MESSAGE);
        } else if (invitation && !pairingAccepted) {
          try {
            this.deviceCredentials.saveInvitation(relay.id, invitation);
          } catch {
            // A malformed or expired link must not remove a working credential.
          }
        }
      }
      saveRelayConfigs(relays);
      if (pairingAccepted || !shouldRetainSetupFragment(location, navigator.standalone)) {
        history.replaceState(history.state, '', location.pathname + location.search);
      }
    }
    this.relayConfigs.set(relays);
    if (connect) this.connectAll();
  }

  private shouldDeferPairing(locationValue: Pick<Location, 'hash' | 'protocol' | 'host'>, pairingAccepted: boolean): boolean {
    return !pairingAccepted && shouldDeferPairingConnection(
      locationValue,
      navigator.standalone,
      navigator.userAgent,
      navigator.maxTouchPoints,
    );
  }

  /**
   * Records whether this browser must leave the link's one-use secret for the
   * installed Home Screen copy to redeem.
   */
  private applyPairingDeferral(relay: RelayConfig, locationValue: Pick<Location, 'hash'>, deferred: boolean): void {
    if (deferred) {
      if (!quickSetupInvitation(locationValue) && locationValue.hash === location.hash
        && typeof history.state?.bootstrapPairingFlow !== 'string') {
        history.replaceState({ ...history.state, bootstrapPairingFlow: commandRequestId() }, '',
          location.pathname + location.search + location.hash);
      }
      this.deferredPairingRelays.add(relay.id);
      this.markPairingDeferred(relay);
    } else {
      this.deferredPairingRelays.delete(relay.id);
    }
  }

  /** A deferred relay shows as a disconnected row that explains why it never dials. */
  private markPairingDeferred(relay: RelayConfig): void {
    this.disconnectRelay(relay.id);
    const connection = this.newConnection(relay);
    connection.status = 'disconnected';
    connection.closed = true;
    connection.pairingDeferred = true;
    this.connectionsValue.set(relay.id, connection);
    this.emitConnections();
  }
  private relayForSetup(relays: RelayConfig[], setup: Omit<RelayConfig, 'id'>): RelayConfig | undefined {
    if (setup.transport === 'hybrid') {
      const gateways = setup.gatewayUrls ?? [setup.gatewayUrl ?? ''];
      return relays.find((relay) => relay.transport === 'hybrid'
        && relay.label === setup.label
        && (relay.gatewayUrls ?? [relay.gatewayUrl ?? '']).some((gateway) => gateways.includes(gateway)));
    }
    return relays.find((relay) => relay.url === setup.url);
  }
  private shouldSaveInvitation(relayId: string, invitation: Omit<RelayInvitation, 'kind'>): boolean {
    const stored = this.deviceCredentials.get(relayId, false);
    if (!stored) return true;
    if (stored.kind === 'invitation') {
      return stored.id !== invitation.id || stored.version !== invitation.version;
    }
    return stored.invitationId !== invitation.id && invitation.expiresAt > stored.issuedAt;
  }

  private setupPairingAccepted(relay: RelayConfig, locationValue: Pick<Location, 'hash'>, invitation: Omit<RelayInvitation, 'kind'> | null): boolean {
    if (invitation) return !this.shouldSaveInvitation(relay.id, invitation);
    if (locationValue.hash !== location.hash || !this.deviceCredentials.get(relay.id)) return false;
    const flow = history.state?.bootstrapPairingFlow;
    if (typeof flow !== 'string') return false;
    try {
      return sessionStorage.getItem(BOOTSTRAP_PAIRING_SESSION_PREFIX + flow) === relay.id;
    } catch {
      return false;
    }
  }

  pairDeferredRelay(relayId: string): boolean {
    const setup = quickSetupConfig(location);
    const relay = setup ? this.relayForSetup(get(this.relayConfigs), setup) : undefined;
    if (!this.deferredPairingRelays.has(relayId) || relay?.id !== relayId) {
      this.showToast('Open the invitation link again.', true);
      return false;
    }
    this.deferredPairingRelays.delete(relayId);
    const invitation = quickSetupInvitation(location);
    try {
      if (invitation && this.shouldSaveInvitation(relayId, invitation)) {
        this.deviceCredentials.saveInvitation(relayId, invitation);
      } else if (!invitation) {
        const flow = history.state?.bootstrapPairingFlow;
        if (typeof flow !== 'string') throw new Error('Open the invitation link again.');
        sessionStorage.setItem(BOOTSTRAP_PAIRING_SESSION_PREFIX + flow, relayId);
      }
    } catch (error) {
      this.deferredPairingRelays.add(relayId);
      this.showToast(error instanceof Error ? error.message : 'Could not save browser pairing.', true);
      return false;
    }
    history.replaceState(history.state, '', location.pathname + location.search);
    this.connectRelay(relay);
    this.showToast('Pairing this browser as its own device.');
    return true;
  }

  importSetupLink(
    locationValue: Pick<Location, 'hash' | 'protocol' | 'host'> & Partial<Pick<Location, 'pathname' | 'search'>> = location,
    connect = true,
    clearAddress = true,
  ): boolean | 'expired' {
    const setup = quickSetupConfig(locationValue);
    const invitation = quickSetupInvitation(locationValue);
    if (!clearAddress && invitation && invitation.expiresAt <= Date.now()) return 'expired';
    const currentRelays = get(this.relayConfigs);
    const imported = importQuickSetup(currentRelays, locationValue);
    if (!imported || !setup) return false;
    const relay = this.relayForSetup(imported, setup);
    if (!relay) return false;
    const pairingAccepted = this.setupPairingAccepted(relay, locationValue, invitation);
    const connection = this.connectionsValue.get(relay.id);
    const preserveConnection = pairingAccepted && connection && !connection.closed
      && !relayConnectionIdentityChanged(connection.relay, relay);
    const deferred = this.shouldDeferPairing(locationValue, pairingAccepted);
    const saveRelays = () => saveRelayConfigs(imported);
    try {
      if (!deferred && invitation && !pairingAccepted) this.deviceCredentials.saveInvitation(relay.id, invitation, saveRelays);
      else saveRelays();
    } catch (error) {
      return error instanceof ExpiredDeviceInvitation ? 'expired' : false;
    }
    clearChangedRelayPreviews(currentRelays, imported);
    this.applyPairingDeferral(relay, locationValue, deferred);
    this.relayConfigs.set(imported);
    if (clearAddress && (pairingAccepted || !shouldRetainSetupFragment(locationValue, navigator.standalone))) {
      history.replaceState(history.state, '', (locationValue.pathname ?? location.pathname) + (locationValue.search ?? location.search));
    }
    if (connect && !preserveConnection) this.connectAll(true);
    if (!preserveConnection) {
      this.showToast(deferred
        ? PAIRING_DEFERRED_MESSAGE
        : invitation ? 'Device invitation imported.' : 'Relay added from the setup link.');
    }
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
    this.panes.reset();
    clearConversationPreviews();
    this.ownedPaneLeases.clear();
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
    // Re-adding or editing an entry must not forget that this relay was paired:
    // the flag is what keeps a credential-less invitation relay off the
    // plaintext path.
    const updated = existing
      ? relays.map((relay) => (relay.id === existing.id
        ? normalizeRelayConfig({ ...next, id: existing.id, paired: next.paired || existing.paired })
        : relay))
      : [...relays, next];
    if (existing && relayConnectionIdentityChanged(existing, next)) clearConversationPreviewsForRelay(existing.id);
    this.relayConfigs.set(updated);
    saveRelayConfigs(updated);
    this.connectAll();
  }

  removeRelay(id: string): void {
    this.disconnectRelay(id);
    this.reconnectAttempts.delete(id);
    this.deferredPairingRelays.delete(id);
    this.deviceCredentials.remove(id);
    clearConversationPreviewsForRelay(id);
    this.devicesValue.delete(id);
    this.devices.set(new Map(this.devicesValue));
    this.pushPoliciesValue.delete(id);
    this.pushPolicies.set(new Map(this.pushPoliciesValue));
    this.pushTestsValue.delete(id);
    this.pushTests.set(new Map(this.pushTestsValue));
    const relays = get(this.relayConfigs).filter((relay) => relay.id !== id);
    this.relayConfigs.set(relays);
    saveRelayConfigs(relays);
    this.removeAgentsForRelay(id);
    this.removeWorkspacesForRelay(id);
    this.activitiesValue = this.activitiesValue.filter((activity) => activity.relay_id !== id);
    this.activities.set(this.activitiesValue);
    if (clearPaneAgentViewOverridesForRelay(id) === 'unavailable') {
      this.showToast('Could not clear saved pane view preferences on this device.', true);
    }
    // Server ids derive from label and URL, so a re-paired computer would
    // otherwise come back hidden.
    pruneHiddenServers(relays.map((relay) => relay.id));
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
    for (const relay of get(this.relayConfigs)) {
      if (this.deferredPairingRelays.has(relay.id)) this.markPairingDeferred(relay);
      else this.connectRelay(relay);
    }
  }

  private relayAuthentication(relay: RelayConfig): RelayInvitation | RelayDeviceCredential | undefined {
    const stored = this.deviceCredentials.get(relay.id);
    if (stored) return stored;
    const secret = UTF8_ENCODER.encode(relay.token);
    if (secret.byteLength !== RELAY_KEY_BYTES) return undefined;
    return this.deviceCredentials.saveInvitation(relay.id, {
      id: 'bootstrap',
      version: 1,
      secret: base64UrlEncode(secret),
      expiresAt: Date.now() + 10 * 60_000,
    });
  }

  /**
   * Reports that a dial cannot authenticate, decided from local state alone.
   * There is no credential to present, no usable relay key to bootstrap one
   * from, and the relay is known to speak the encrypted handshake — because it
   * was entered through an invitation or has already enrolled a credential, or
   * because a relay key is configured but is not a usable one.
   *
   * A relay whose key can still bootstrap is excluded: `reset_devices` re-arms
   * the relay's bootstrap invitation, so that dial is how a controller
   * re-enrols. A tokenless relay that never paired is excluded too — it takes
   * the unencrypted path, and a failure there is indistinguishable from a relay
   * that is simply down, so it stays on the reconnect ladder.
   */
  private pairingRequired(relay: RelayConfig): boolean {
    if (this.deviceCredentials.get(relay.id)) return false;
    if (UTF8_ENCODER.encode(relay.token).byteLength === RELAY_KEY_BYTES) return false;
    return Boolean(relay.paired) || Boolean(relay.token);
  }

  /**
   * Records that this relay speaks the encrypted handshake, the moment a
   * credential is enrolled against it. An invitation-paired entry stores no
   * relay key, so this flag is what survives the credential being revoked,
   * reset, or dropped with an expired invitation.
   */
  private markRelayPaired(relayId: string): void {
    const relays = get(this.relayConfigs);
    const existing = relays.find((relay) => relay.id === relayId);
    if (!existing || existing.paired) return;
    const paired = { ...existing, paired: true } as const;
    const updated = relays.map((relay) => (relay.id === relayId ? paired : relay));
    this.relayConfigs.set(updated);
    saveRelayConfigs(updated);
    const connection = this.connectionsValue.get(relayId);
    if (connection) connection.relay = paired;
  }

  private markPairingRequired(relay: RelayConfig): void {
    if (this.connectionsValue.get(relay.id)?.pairingRequired) return;
    this.disconnectRelay(relay.id);
    const connection = this.newConnection(relay);
    connection.status = 'disconnected';
    connection.closed = true;
    connection.pairingRequired = true;
    this.connectionsValue.set(relay.id, connection);
    this.emitConnections();
    this.showToast(`${relay.label} needs pairing. Import a device invitation link.`, true);
  }

  private newConnection(relay: RelayConfig): RelayConnection {
    return {
      relay,
      transport: null,
      status: 'connecting',
      path: '',
      activeGatewayUrl: '',
      reconnectTimer: null,
      healthTimer: null,
      updateRestartTimer: null,
      delayedPaneReadTimers: new Set(),
      closed: false,
      connectingSince: Date.now(),
      lastMessageAt: 0,
      agentProfiles: [],
      hermesProfiles: [],
      capabilities: [],
      speechLanguages: [],
      speechVoices: [],
      speechCacheDir: '',
      speechEngineInstalled: false,
      directoryBrowser: null,
      directoryLoading: false,
      directoryError: '',
      directoryGeneration: 0,
      host: '',
      home: '',
      protocol: 0,
      authRejected: false,
      pairingRequired: false,
      pairingDeferred: false,
      version: '',
      releaseVersion: '',
      revision: '',
      update: normalizeRelayUpdate(null),
      appDeploy: normalizeAppDeployment(null),
      inventory: normalizeAgentInventory(null),
      pushStatus: '',
      vapidPublicKey: '',
      herdrStatus: normalizeHerdrStatus(null),
    };
  }

  connectRelay(relay: RelayConfig): void {
    if (this.hidden) return;
    if (this.deferredPairingRelays.has(relay.id)) return;
    if (this.pairingRequired(relay)) {
      this.markPairingRequired(relay);
      return;
    }
    this.disconnectRelay(relay.id);
    const connection = this.newConnection(relay);
    this.connectionsValue.set(relay.id, connection);
    this.emitConnections();
    connection.transport = createRelayTransport(relay, {
      onMessage: (message) => {
        if (!this.isCurrentConnection(relay.id, connection) || connection.closed) return;
        connection.lastMessageAt = Date.now();
        this.clearHealthTimer(connection);
        this.reconnectAttempts.delete(relay.id);
        this.handleMessage(relay.id, message);
      },
      onStatus: (status, detail) => {
        this.applyTransportStatus(relay, connection, status, detail);
      },
    }, {
      getAuthentication: () => this.relayAuthentication(relay),
      onAuthenticated: (presented, enrollment) => {
        if (!this.isCurrentConnection(relay.id, connection) || connection.closed) return;
        commitDeviceEnrollment(this.deviceCredentials, relay.id, presented, enrollment);
        this.markRelayPaired(relay.id);
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
        this.panes.rereadWatched(relay.id);
      }
      // A path switch changes the relayed-fidelity budget, so panes have to be
      // rewatched at the interval the new path can afford.
      if (previousPath && previousPath !== connection.path) this.panes.restartWatches();
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
    this.clearDelayedPaneReads(connection);
    this.rejectPendingOperations(relay.id, detail?.reason || 'Relay disconnected');
    // The relay refuses this device's credential. Keep it until a confirmed,
    // newer invitation replaces it: gateway close reasons are not authenticated.
    if (detail?.code === 'device_unauthorized') {
      clearConversationPreviewsForRelay(relay.id);
      connection.authRejected = true;
      connection.closed = true;
      clearTimeout(connection.reconnectTimer ?? undefined);
      connection.reconnectTimer = null;
      this.emitConnections();
      this.showToast(`${relay.label} refused this device. Import a new invitation link.`, true);
      return;
    }
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
    this.clearHealthTimer(connection);
    connection.lastMessageAt = Date.now();
    connection.status = 'connected';
    this.emitConnections();
    const authentication = this.deviceCredentials.get(relayId);
    if (runningAsInstalledApp()
      && authentication?.kind === 'credential'
      && authentication.role === 'controller') {
      this.sendRaw(relayId, {
        type: 'register_app_origin',
        origin: location.origin,
        protocol: RELAY_PROTOCOL_VERSION,
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
    this.panes.forgetRelay(id);
    const connection = this.connectionsValue.get(id);
    if (!connection) return;
    connection.closed = true;
    this.clearDelayedPaneReads(connection);
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
    if (hidden) {
      for (const connection of this.connectionsValue.values()) {
        clearTimeout(connection.reconnectTimer ?? undefined);
        connection.reconnectTimer = null;
        this.clearUpdateRestartTimer(connection);
      }
    }
    this.syncKeepalive();
  }

  revalidateConnections(timeoutMs: number): void {
    if (!this.reconnectEnabled || this.hidden) return;
    const relays = get(this.relayConfigs);
    for (const relay of relays) {
      const connection = this.connectionsValue.get(relay.id);
      if (connection?.authRejected || connection?.pairingRequired) continue;
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
      if (connection.healthTimer) continue;
      connection.healthTimer = setTimeout(() => {
        if (!this.isCurrentConnection(relay.id, connection)) return;
        connection.healthTimer = null;
        this.connectRelay(relay);
      }, timeoutMs);
      this.sendRaw(relay.id, { type: 'refresh_agents' });
      if (!this.sendRaw(relay.id, { type: 'ping' })) {
        this.clearHealthTimer(connection);
        this.connectRelay(relay);
      }
    }
  }

  private isCurrentConnection(relayId: string, connection: RelayConnection): boolean {
    return this.connectionsValue.get(relayId) === connection;
  }

  private scheduleDelayedPaneRead(
    agent: Agent, owner: RelayConnection | undefined, delay: number,
  ): void {
    if (!owner) return;
    const current = () => !owner.closed
      && owner.status !== 'disconnected'
      && this.isCurrentConnection(agent.relay_id, owner);
    if (!current()) return;
    const timer = setTimeout(() => {
      if (!owner.delayedPaneReadTimers.delete(timer) || !current()) return;
      this.readPane(agent);
    }, delay);
    if (!current()) {
      clearTimeout(timer);
      return;
    }
    owner.delayedPaneReadTimers.add(timer);
  }

  private clearDelayedPaneReads(owner: RelayConnection): void {
    for (const timer of owner.delayedPaneReadTimers) clearTimeout(timer);
    owner.delayedPaneReadTimers.clear();
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
      if (this.sendRaw(relayId, { type: 'ping' })) continue;
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
    this.clearDelayedPaneReads(connection);
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
    if (this.hidden || connection.closed || connection.updateRestartTimer) return;
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
    if (this.hidden || connection.closed || !this.reconnectEnabled || connection.reconnectTimer) return;
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
      if (this.hidden || connection.closed || !this.reconnectEnabled) return;
      this.connectRelay(relay);
    }, delay);
  }

  private readonly messageHandlers = new Map<string, (relayId: string, message: Record<string, any>) => void>([
    ['push_config', (relayId, message) => this.handlePushConfig(relayId, message)],
    ['herdr_status', (relayId, message) => this.handleHerdrStatus(relayId, message)],
    ['inventory_status', (relayId, message) => this.handleInventoryStatus(relayId, message)],
    ['update_status', (relayId, message) => this.handleUpdateStatus(relayId, message)],
    ['app_deploy_status', (relayId, message) => this.handleAppDeployStatus(relayId, message)],
    ['speech_voices', (relayId, message) => this.handleSpeechVoices(relayId, message)],
    ['push_policy', (relayId, message) => this.handlePushPolicy(relayId, message)],
    ['push_policy_result', (relayId, message) => this.handlePushPolicy(relayId, message)],
    ['push_test_result', (relayId, message) => this.handlePushTestResult(relayId, message)],
    ['push_subscribed', (relayId, message) => this.handlePushSubscribed(relayId, message)],
    ['push_unsubscribed', (relayId, message) => this.handlePushUnsubscribed(relayId, message)],
    ['action_receipt', (relayId, message) => this.handleActionReceipt(relayId, message)],
    ['error', (relayId, message) => this.handleApiError(relayId, message)],
    ['command_result', (relayId, message) => this.handleCommandResult(relayId, message as CommandResult)],
    ['upload_begin_result', (relayId, message) => this.handleUploadResult(relayId, message)],
    ['upload_chunk_result', (relayId, message) => this.handleUploadResult(relayId, message)],
    ['upload_finish_result', (relayId, message) => this.handleUploadResult(relayId, message)],
    ['upload_cancel_result', (relayId, message) => this.handleUploadResult(relayId, message)],
    ['activity_history', (relayId, message) => this.mergeActivityHistory(relayId, message.activities || [])],
    ['activity', (relayId, message) => this.handleActivity(relayId, message)],
    ['workspaces', (relayId, message) => this.handleWorkspaces(relayId, message)],
    ['agents', (relayId, message) => this.handleAgents(relayId, message)],
    ['blocked', (relayId, message) => this.handleBlocked(relayId, message)],
    ['agent_update', (relayId, message) => this.handleAgentUpdate(relayId, message)],
    ['pane_unchanged', (relayId, message) => this.panes.handleUnchanged(relayId, message)],
    ['pane_resync', (relayId, message) => this.panes.handleResync(relayId, message)],
    ['pane_delta', (relayId, message) => this.panes.handleDelta(relayId, message)],
    ['pane_content', (relayId, message) => this.panes.handleContent(relayId, message)],
  ]);

  private handleMessage(relayId: string, message: Record<string, any>): void {
    this.messageHandlers.get(String(message.type))?.(relayId, message);
  }

  private handlePushConfig(relayId: string, message: Record<string, any>): void {
    const connection = this.connectionsValue.get(relayId);
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
    connection.home = String(message.home || '').slice(0, 1024);
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
    connection.herdrStatus = normalizeHerdrStatus(message.herdr_status);
    connection.appDeploy = normalizeAppDeployment(message.app_deploy);
    connection.inventory = normalizeAgentInventory(message.inventory, 'ready');
    connection.capabilities = Array.isArray(message.capabilities) ? message.capabilities.filter(Boolean) : [];
    connection.speechLanguages = (Array.isArray(message.speech_languages) ? message.speech_languages : [])
      .filter(isSpeechLanguage);
    adoptRelaySpeech(connection.speechLanguages);
    this.adoptHybridDescriptor(connection, message.hybrid);
    const attentionCapable = connection.capabilities.includes('attention_classification');
    this.agentsValue = this.agentsValue.map((agent) =>
      agent.relay_id === relayId
        ? normalizeAgentAttention(agent, attentionCapable, connection.capabilities.includes('idle_question_probe'))
        : agent,
    );
    this.publishAgents('push_config');
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
    // A relay that predates the picker omits the field entirely, and a
    // malformed entry would otherwise reach `-p` as a non-string.
    connection.hermesProfiles = Array.isArray(message.hermes_profiles)
      ? message.hermes_profiles.filter((name): name is string => typeof name === 'string' && name.length > 0)
      : [];
    this.emitConnections();
    this.pushConfigHandler?.(relayId);
    if (connection.capabilities.includes('push_policy')) {
      this.sendRaw(relayId, { type: 'push_policy_get', protocol: RELAY_PROTOCOL_VERSION });
    }
    if (connection.capabilities.includes('device_management')) {
      void this.refreshDevices(relayId);
    }
  }

  private handleHerdrStatus(relayId: string, message: Record<string, any>): void {
    const connection = this.connectionsValue.get(relayId);
    if (!connection) return;
    const status = normalizeHerdrStatus(message.status);
    if (status.generation <= connection.herdrStatus.generation) return;
    connection.herdrStatus = status;
    if (Array.isArray(message.capabilities)) {
      connection.capabilities = message.capabilities.filter(Boolean);
    }
    this.emitConnections();
  }

  private handleInventoryStatus(relayId: string, message: Record<string, any>): void {
    const connection = this.connectionsValue.get(relayId);
    if (!connection) return;
    connection.inventory = normalizeAgentInventory(message);
    this.emitConnections();
  }

  private handleUpdateStatus(relayId: string, message: Record<string, any>): void {
    const connection = this.connectionsValue.get(relayId);
    if (!connection) return;
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
    if (connection.update.state === 'failed') {
      clearPendingRelayUpdate(relayId);
    }
    this.emitConnections();
  }

  private handleAppDeployStatus(relayId: string, message: Record<string, any>): void {
    const connection = this.connectionsValue.get(relayId);
    if (!connection) return;
    connection.appDeploy = normalizeAppDeployment(message.app_deploy);
    this.emitConnections();
  }

  private handleSpeechVoices(relayId: string, message: Record<string, any>): void {
    const connection = this.connectionsValue.get(relayId);
    if (!connection) return;
    this.adoptSpeechVoices(connection, message);
    this.emitConnections();
  }

  private handlePushPolicy(relayId: string, message: Record<string, any>): void {
    if (message.ok === false) return;
    const policy = normalizePushPolicy(message.policy);
    if (policy) {
      this.pushPoliciesValue.set(relayId, policy);
      this.pushPolicies.set(new Map(this.pushPoliciesValue));
    }
  }

  private handlePushTestResult(relayId: string, message: Record<string, any>): void {
    const stage = String(message.stage || 'dropped');
    const state: PushTestState = stage === 'service_accepted'
      ? { status: 'accepted', result: 'accepted' }
      : stage === 'queued' || stage === 'retrying'
        ? { status: 'accepted', result: 'queued' }
        : { status: 'rejected', code: stage };
    this.pushTestsValue.set(relayId, state);
    this.pushTests.set(new Map(this.pushTestsValue));
  }

  private handlePushSubscribed(relayId: string, message: Record<string, any>): void {
    const connection = this.connectionsValue.get(relayId);
    if (!connection) return;
    connection.pushStatus = message.ok ? 'subscribed' : 'failed';
    this.emitConnections();
  }

  private handlePushUnsubscribed(relayId: string, message: Record<string, any>): void {
    const connection = this.connectionsValue.get(relayId);
    if (!connection) return;
    if (!message.ok) return;
    connection.pushStatus = '';
    this.emitConnections();
  }

  private handleActivity(relayId: string, message: Record<string, any>): void {
    if (!message.activity) return;
    this.upsertActivity(relayId, message.activity);
  }

  private handleWorkspaces(relayId: string, message: Record<string, any>): void {
    const connection = this.connectionsValue.get(relayId);
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
  }

  private handleAgents(relayId: string, message: Record<string, any>): void {
    const connection = this.connectionsValue.get(relayId);
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
      .map((agent: Partial<Agent>) => normalizeAgent(
        relayId,
        label,
        normalizeAgentTargetFields(agent),
        attentionCapable,
        connection?.capabilities.includes('idle_question_probe'),
      ));
    const previous = this.agentsValue;
    this.agentsValue = mergeAgentList(
      this.agentsValue,
      relayId,
      incoming,
      this.blockedSnapshotMisses,
      this.respondingValue,
    );
    this.reconcileResponding(previous);
    this.publishAgents('agents snapshot');
  }

  private handleBlocked(relayId: string, message: Record<string, any>): void {
    const connection = this.connectionsValue.get(relayId);
    const label = get(this.relayConfigs).find((relay) => relay.id === relayId)?.label || 'relay';
    const attentionCapable = Boolean(connection?.capabilities.includes('attention_classification'));
    const next = normalizeAgent(
      relayId,
      label,
      normalizeAgentTargetFields({ ...message, status: 'blocked' }),
      attentionCapable,
      connection?.capabilities.includes('idle_question_probe'),
    );
    const index = this.agentsValue.findIndex((agent) => agent.pane_id === next.pane_id);
    const before = index >= 0 ? this.agentsValue[index] : undefined;
    if (staleAgentRevision(before, next)) return;
    this.blockedSnapshotMisses.delete(next.pane_id);
    if (index >= 0) {
      const copy = [...this.agentsValue];
      copy[index] = mergeAgentDetails(before, next);
      this.agentsValue = copy;
    } else this.agentsValue = [...this.agentsValue, next];
    this.clearResponding(next.pane_id);
    this.publishAgents('blocked event');
  }

  private handleAgentUpdate(relayId: string, message: Record<string, any>): void {
    if (!message.pane_id) return;
    const connection = this.connectionsValue.get(relayId);
    const label = get(this.relayConfigs).find((relay) => relay.id === relayId)?.label || 'relay';
    const attentionCapable = Boolean(connection?.capabilities.includes('attention_classification'));
    const paneId = clientPaneId(relayId, message.pane_id);
    const index = this.agentsValue.findIndex((agent) => agent.pane_id === paneId);
    const before = index >= 0 ? this.agentsValue[index] : undefined;
    const idleProbeCapable = connection?.capabilities.includes('idle_question_probe');
    const source = !idleProbeCapable && before && rawBlocked(message)
      && !Object.prototype.hasOwnProperty.call(message, 'attention_kind')
      ? { ...before, ...message }
      : message;
    const next = normalizeAgent(relayId, label, normalizeAgentTargetFields(source), attentionCapable, idleProbeCapable);
    if (staleAgentRevision(before, next)) return;
    const previous = this.agentsValue;
    const stabilized = stabilizeBlockedSnapshot(before, next, this.blockedSnapshotMisses, this.respondingValue);
    if (index >= 0) {
      const copy = [...this.agentsValue];
      copy[index] = mergeAgentDetails(before, stabilized);
      this.agentsValue = copy;
    } else this.agentsValue = [...this.agentsValue, stabilized];
    this.reconcileResponding(previous);
    this.publishAgents('agent update');
  }

  /**
   * Every agents publish flows through here because the home view keys its
   * cards by pane_id: one duplicate anywhere wedges Svelte's flush and every
   * control silently dies. A duplicate is upstream data corruption, so it is
   * reported loudly, and the newest copy wins so the app stays alive.
   */
  private publishAgents(source: string): void {
    const byPane = new Map<string, Agent>();
    let duplicate = '';
    for (const agent of this.agentsValue) {
      if (byPane.has(agent.pane_id)) duplicate = agent.pane_id;
      byPane.set(agent.pane_id, agent);
    }
    if (duplicate) {
      reportAppAnomaly(`Duplicate agent identity from ${source}: ${duplicate}`);
      this.agentsValue = [...byPane.values()];
    }
    this.agents.set(this.agentsValue);
  }
  private mergePaneInteraction(paneId: string, message: Record<string, any>): void {
    const index = this.agentsValue.findIndex((agent) => agent.pane_id === paneId);
    if (index < 0) return;
    const agent = this.agentsValue[index];
    if (!agent.attention_capable || message.attention_kind !== 'question' || !message.interaction) return;
    if (this.connectionsValue.get(agent.relay_id)?.capabilities.includes('idle_question_probe')
      && (!rawBlocked(agent) || agent.screen_attention === 'waiting')) return;
    this.agentsValue[index] = {
      ...agent,
      status: 'blocked',
      attention_kind: 'question',
      interaction: message.interaction as QuestionInteraction,
    };
    this.blockedSnapshotMisses.delete(paneId);
    this.publishAgents('pane interaction');
  }

  private removeAgentsForRelay(relayId: string): void {
    this.agentsValue = this.agentsValue.filter((agent) => agent.relay_id !== relayId);
    for (const paneId of this.blockedSnapshotMisses.keys()) {
      if (paneId.startsWith(`${relayId}::`)) this.blockedSnapshotMisses.delete(paneId);
    }
    this.panes.clearPendingReads();
    this.publishAgents('relay removal');
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

  private reconcileResponding(previous: Agent[]): void {
    if (this.respondingValue.size === 0) return;
    const blocked = new Map(this.agentsValue.filter((agent) => agentStatusGroup(agent) === 'blocked').map((agent) => [agent.pane_id, agent]));
    const byPane = new Map(previous.map((agent) => [agent.pane_id, agent]));
    let changed = false;
    for (const paneId of this.respondingValue) {
      const current = blocked.get(paneId);
      if (!current || agentTargetsDiffer(byPane.get(paneId), current)) {
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

  beginPushTest(relayId: string): void {
    this.pushTestsValue.set(relayId, { status: 'sending' });
    this.pushTests.set(new Map(this.pushTestsValue));
  }

  rejectPushTest(relayId: string, code: string): void {
    this.pushTestsValue.set(relayId, { status: 'rejected', code });
    this.pushTests.set(new Map(this.pushTestsValue));
  }

  sendRaw(relayId: string, payload: Record<string, unknown>): boolean {
    const connection = this.connectionsValue.get(relayId);
    if (!connection || connection.status !== 'connected') return false;
    return connection.transport?.send(payload) ?? false;
  }

  sendCommand(
    relayId: string,
    payload: Record<string, any>,
    timeoutMs = payload.type === 'agent_start' ? AGENT_START_TIMEOUT_MS : COMMAND_TIMEOUT_MS,
    allowProtocolMismatch = false,
    signal?: AbortSignal,
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
    if (signal?.aborted) {
      const error = new CommandError('Conversation history request was cancelled.');
      error.code = 'request_cancelled';
      return Promise.reject(error);
    }
    const requestId = payload.type === 'send_pi_prompt_unconfirmed'
      ? `${String(payload.pi_prompt_lease?.incarnation || '')}_${commandRequestId()}`
      : commandRequestId();
    return this.sendPending<PendingRequest, CommandResult>({
      operations: this.pendingRequests,
      relayId,
      requestId,
      frame: () => {
        const command: Record<string, unknown> = {
          ...payload,
          request_id: requestId,
          protocol: RELAY_PROTOCOL_VERSION,
        };
        if (payload.type !== 'lease_pane_size' && payload.type !== 'release_pane_size') {
          command.client_id = pushClientId();
        }
        return command;
      },
      signal,
      timeoutMs,
      timeoutMessage: 'Relay confirmation timed out',
      sendFailureMessage: 'Could not send command to relay',
      abortError: () => {
        const error = new CommandError('Conversation history request was cancelled.');
        error.code = 'request_cancelled';
        return error;
      },
      createPending: (base) => {
        const actionId = payload.intent && typeof payload.intent === 'object'
          ? String(payload.intent.action_id || '')
          : '';
        return {
          ...base,
          action: String(payload.action || payload.type),
          actionId: actionId || undefined,
        };
      },
    });
  }

  private sendPending<TPending extends PendingOperation, TResult>(options: {
    operations: Map<string, TPending>;
    relayId: string;
    requestId: string;
    frame: () => Record<string, unknown>;
    signal: AbortSignal | undefined;
    timeoutMs: number;
    timeoutMessage: string;
    sendFailureMessage: string;
    abortError: () => CommandError;
    createPending: (base: PendingOperation & { resolve: (result: TResult) => void }) => TPending;
  }): Promise<TResult> {
    const { operations, relayId, requestId, signal } = options;
    return new Promise<TResult>((resolve, reject) => {
      const abort = () => {
        clearTimeout(operations.get(requestId)?.timer ?? timer);
        if (!operations.delete(requestId)) return;
        cleanup();
        reject(options.abortError());
      };
      const cleanup = () => signal?.removeEventListener('abort', abort);
      const timer = setTimeout(() => {
        cleanup();
        operations.delete(requestId);
        reject(dispatchedUnknownError(options.timeoutMessage));
      }, options.timeoutMs);
      registerPendingOperation<TPending>(
        operations, requestId, signal, abort,
        () => options.createPending({ relayId, resolve, reject, timer, cleanup }),
      );
      if (!this.sendRaw(relayId, options.frame())) {
        releasePendingResources({ timer, cleanup });
        operations.delete(requestId);
        reject(new CommandError(options.sendFailureMessage));
      }
    });
  }

  deviceCredential(relayId: string): RelayDeviceCredential | null {
    const authentication = this.deviceCredentials.get(relayId);
    return authentication?.kind === 'credential' ? authentication : null;
  }

  async refreshDevices(relayId: string): Promise<DeviceSummary[]> {
    try {
      const result = await this.sendCommand(relayId, { type: 'device_list' });
      const values = Array.isArray(result.data?.devices) ? result.data.devices : [];
      const devices = values.flatMap((value: unknown): DeviceSummary[] => {
        if (!value || typeof value !== 'object') return [];
        const record = value as Record<string, unknown>;
        const deviceId = String(record.device_id || '');
        const credentialId = String(record.credential_id || '');
        const role = record.role;
        const pairedAt = Date.parse(String(record.paired_at || ''));
        const lastSeenAt = Date.parse(String(record.last_seen_at || ''));
        if (!deviceId || !credentialId || !isDeviceRole(role) || !Number.isFinite(pairedAt)) {
          return [];
        }
        return [{
          deviceId,
          credentialId,
          name: String(record.name || 'Device').slice(0, 80),
          role,
          pairedAt,
          ...(Number.isFinite(lastSeenAt) ? { lastSeenAt } : {}),
          current: record.current === true,
        }];
      });
      this.devicesValue.set(relayId, devices);
      this.devices.set(new Map(this.devicesValue));
      return devices;
    } catch (error) {
      if (this.connectionsValue.get(relayId)?.status === 'connected') throw error;
      return this.devicesValue.get(relayId) || [];
    }
  }

  async renameDevice(intent: RenameDeviceIntent): Promise<void> {
    await this.sendCommand(intent.relayId, {
      type: 'rename_device',
      device_id: intent.deviceId,
      name: intent.name,
    });
    await this.refreshDevices(intent.relayId);
  }

  async createDeviceInvitation(intent: CreateInvitationIntent): Promise<string> {
    const relay = get(this.relayConfigs).find((entry) => entry.id === intent.relayId);
    if (!relay) throw new CommandError('Relay configuration is unavailable');
    if (!canInviteFrom(relay)) {
      throw new CommandError('This computer has no address an invitation could carry');
    }
    // Resolved before the relay mints anything, so a failure here leaves no
    // orphaned one-use invitation behind.
    const rendezvous = relay.url ? null : await gatewayRendezvous(relay);
    const result = await this.sendCommand(intent.relayId, {
      type: 'create_device_invitation',
      name: intent.name,
      role: intent.role,
    });
    const invitation = result.data?.invitation as Record<string, unknown> | undefined;
    const id = String(invitation?.invitation_id || '');
    const version = Number(invitation?.version);
    const secret = String(invitation?.secret || '');
    const expiresAt = Date.parse(String(invitation?.expires_at || ''));
    if (!isInvitationId(id)
      || !isAuthenticationVersion(version)
      || !isAuthenticationSecret(secret)
      || !Number.isFinite(expiresAt)) {
      throw new CommandError('Relay returned an invalid device invitation');
    }
    const params = new URLSearchParams({
      setup: secret,
      invite: id,
      invite_version: String(version),
      invite_expires: String(expiresAt),
      label: relay.label,
    });
    if (rendezvous) {
      // The invited device reaches this computer through its gateways with
      // the derived rendezvous, never with the relay key itself.
      params.set('gateways', (relay.gatewayUrls ?? [relay.gatewayUrl ?? '']).join(','));
      params.set('relay_id', rendezvous.relayId);
      params.set('rendezvous', rendezvous.rendezvousKey);
    } else {
      params.set('relay', relay.url);
    }
    const link = new URL(location.href);
    link.hash = params.toString();
    return link.toString();
  }

  // The relay owns the QR encoder, so the app asks it to draw the pairing
  // link it just built.
  async qrCode(relayId: string, text: string): Promise<{ size: unknown; modules: unknown }> {
    const result = await this.sendCommand(relayId, { type: 'qr_code', text });
    return { size: result.data?.size, modules: result.data?.modules };
  }

  async revokeDevice(intent: RevokeDeviceIntent): Promise<void> {
    await this.sendCommand(intent.relayId, {
      type: 'revoke_device',
      device_id: intent.deviceId,
    });
    const current = this.deviceCredential(intent.relayId);
    if (current?.deviceId === intent.deviceId) {
      this.deviceCredentials.remove(intent.relayId);
      clearConversationPreviewsForRelay(intent.relayId);
      this.surfacePairingRequirement(intent.relayId);
    }
    await this.refreshDevices(intent.relayId);
  }

  async resetDevices(intent: ResetDevicesIntent): Promise<void> {
    await this.sendCommand(intent.relayId, { type: 'reset_devices' });
    this.deviceCredentials.remove(intent.relayId);
    clearConversationPreviewsForRelay(intent.relayId);
    this.devicesValue.delete(intent.relayId);
    this.devices.set(new Map(this.devicesValue));
    this.surfacePairingRequirement(intent.relayId);
  }

  /**
   * Answers the question the phone would otherwise spend a reconnect ladder on:
   * once this browser's own credential is gone, an invitation-paired relay has
   * nothing left to present, so say so now instead of retrying in the dark.
   */
  private surfacePairingRequirement(relayId: string): void {
    const relay = get(this.relayConfigs).find((entry) => entry.id === relayId);
    if (relay && this.pairingRequired(relay)) this.markPairingRequired(relay);
  }

  async forgetCurrentDevice(relayId: string): Promise<void> {
    const current = this.deviceCredential(relayId);
    if (!current) throw new CommandError('No enrolled device credential is stored for this relay');
    await this.revokeDevice({ relayId, deviceId: current.deviceId });
  }


  sendToAgent(agent: Agent, payload: Record<string, any>, timeoutMs?: number, signal?: AbortSignal): Promise<CommandResult> {
    const identity = agentTargetPayload(agent);
    if (!identity) return Promise.reject(new CommandError('This agent no longer has an exact terminal identity'));
    return this.sendCommand(agent.relay_id, {
      ...payload,
      pane_id: agent.raw_pane_id,
      ...identity,
    }, timeoutMs, false, signal);
  }

  async loadPiPromptCapability(agent: Agent): Promise<PiPromptCapability> {
    const connection = this.connectionsValue.get(agent.relay_id);
    const target = targetRefForAgent(agent);
    const observedAt = performance.now();
    if (!target || !piPromptTarget(agent) || connection?.status !== 'connected'
      || !connection.capabilities.includes('pi_prompt_unconfirmed') || this.deviceCredential(agent.relay_id)?.role !== 'controller') {
      throw new CommandError('Pi prompt capability is unavailable');
    }
    const transport = connection.transport;
    const result = await this.sendToAgent(agent, { type: 'get_pi_prompt_capability' });
    const lease = result.data?.lease as PiPromptLease | undefined;
    if (connection !== this.connectionsValue.get(agent.relay_id) || connection.status !== 'connected' || transport !== connection.transport
      || result.data?.capability !== 'pi_prompt_unconfirmed' || !lease || lease.pane !== target.pane_id
      || lease.session !== target.agent_session_id || !/^[a-f0-9]{64}$/.test(lease.instance) || !Number.isInteger(lease.pid) || lease.pid <= 0
      || typeof lease.connection !== 'string' || !lease.connection || typeof lease.target_key !== 'string'
      || typeof lease.token !== 'string' || lease.token.length !== 36 || typeof lease.incarnation !== 'string' || lease.incarnation.length !== 36) {
      throw new CommandError('Pi prompt capability changed');
    }
    return { lease, target, observedAt, connection, transport };
  }

  piPromptReady(agent: Agent, capability: PiPromptCapability | null, frame?: TerminalFrame): boolean {
    const connection = this.connectionsValue.get(agent.relay_id);
    const now = performance.now();
    const age = now - (frame?.receivedAt ?? NaN);
    const capabilityAge = now - (capability?.observedAt ?? NaN);
    return Boolean(capability && piPromptTarget(agent) && targetRefMatchesAgent(capability.target, agent)
      && capabilityAge >= 0 && capabilityAge < 10_000 && capability.connection === connection
      && connection?.status === 'connected' && capability.transport === connection.transport
      && connection.capabilities.includes('pi_prompt_unconfirmed') && this.deviceCredential(agent.relay_id)?.role === 'controller'
      && !this.hidden && document.visibilityState !== 'hidden' && frame && frame.noEcho === false
      && frame === this.panes.currentFrame(agent.pane_id) && this.panes.observedConnection(frame) === connection
      && frame.target && targetRefMatchesAgent(frame.target, agent) && age >= 0 && age < 3_000
      && Number.isFinite(frame.readStartedAt) && frame.readStartedAt! > capability.observedAt);
  }

  async sendPiPrompt(agent: Agent, text: string, capability: PiPromptCapability, frame: TerminalFrame): Promise<CommandResult> {
    const delivery = piPromptDelivery(agent);
    const result = await delivery.withLock(async () => {
      if (!this.piPromptReady(agent, capability, frame) || !validPiPromptText(text)) {
        throw Object.assign(new CommandError('Wait for fresh Pi input state; attachments and hidden input are excluded.'), { data: { not_started: true } });
      }
      if (!delivery.begin()) return;
      let uncertain = true;
      let sentAt = 0;
      try {
        const result = await this.sendToAgent(agent, { type: 'send_pi_prompt_unconfirmed', text, pi_prompt_lease: capability.lease });
        sentAt = result.phase === 'unconfirmed' && Number.isFinite(result.data?.sent_at) ? result.data!.sent_at : 0;
        return result;
      } catch (error) {
        uncertain = dispatchFlag(error, 'dispatched_unknown');
        throw error;
      } finally {
        delivery.finish(uncertain, sentAt);
      }
    });
    if (!result) throw Object.assign(new CommandError(delivery.error || 'Check the previous Pi send first.'), { data: { not_started: true } });
    return result;
  }

  speakToAgent(agent: Agent, text: string, language: string): SpeechRequest {
    const speechRequestId = commandRequestId();
    const promise = this.sendToAgent(agent, {
      type: 'speak_text',
      text,
      language,
      speech_request_id: speechRequestId,
    }, 20_000) as SpeechRequest;
    promise.cancel = () => {
      const identity = agentTargetPayload(agent);
      if (!identity) return;
      this.sendRaw(agent.relay_id, {
        type: 'cancel_speech',
        pane_id: agent.raw_pane_id,
        speech_request_id: speechRequestId,
        protocol: RELAY_PROTOCOL_VERSION,
        ...identity,
      });
    };
    return promise;
  }

  /**
   * Answers a recognized no-echo prompt. The relay types the secret as single
   * runes so no bracketed-paste wrapper reaches a termios noecho read, and it
   * never journals the text.
   */
  secretInputReady(agent: Agent, frame?: TerminalFrame): boolean {
    const connection = this.connectionsValue.get(agent.relay_id);
    const age = performance.now() - (frame?.receivedAt ?? NaN);
    return Boolean(frame && connection?.status === 'connected' && !this.hidden && document.visibilityState !== 'hidden'
      && frame === this.panes.currentFrame(agent.pane_id) && this.panes.observedConnection(frame) === connection
      && frame.noEcho === true && frame.target && targetRefMatchesAgent(frame.target, agent)
      && Number.isFinite(age) && age >= 0 && age < 3_000 && Number.isFinite(frame.readStartedAt)
      && frame.readStartedAt! > (this.secretReadAfter.get(agent.pane_id) ?? -Infinity));
  }

  async sendSecret(agent: Agent, text: string, frame: TerminalFrame): Promise<CommandResult> {
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('secret_input')) {
      return Promise.reject(new CommandError('This relay does not support password prompts'));
    }
    if (!text) return Promise.reject(new CommandError('Enter the password first'));
    if (!this.secretInputReady(agent, frame)) {
      throw new CommandError('Wait for a fresh exact-target hidden prompt.');
    }
    const delivery = terminalInputDelivery(agent);
    if (!delivery.begin()) throw new CommandError('Check terminal input delivery before sending.');
    this.panes.forgetObservation(frame);
    let uncertain = true;
    try {
      const result = await this.sendToAgent(agent, { type: 'send_secret', text });
      uncertain = result.phase !== 'completed' || !result.ok;
      return result;
    } catch (error) {
      uncertain = !dispatchFlag(error, 'not_started');
      throw error;
    } finally {
      this.secretReadAfter.set(agent.pane_id, performance.now());
      delivery.finish(uncertain);
    }
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

  private handleActionReceipt(relayId: string, message: Record<string, unknown>): void {
    const receipt = parseActionReceipt(message);
    if (!receipt) return;
    let requestId = typeof message.request_id === 'string' ? message.request_id : '';
    let pending = requestId ? this.pendingRequests.get(requestId) : undefined;
    if (!pending) {
      for (const [candidateRequestId, candidate] of this.pendingRequests) {
        if (candidate.relayId === relayId && candidate.actionId === receipt.action_id) {
          requestId = candidateRequestId;
          pending = candidate;
          break;
        }
      }
    }
    if (!pending || pending.relayId !== relayId) return;
    if (receipt.phase === 'prepared' || receipt.phase === 'awaiting_evidence') {
      resetPendingDeadline(pending, () => {
        pending.cleanup?.();
        this.pendingRequests.delete(requestId);
        pending?.reject(dispatchedUnknownError('Relay confirmation timed out'));
      }, ACCEPTED_COMMAND_TIMEOUT_MS);
      return;
    }
    releasePendingResources(pending);
    this.pendingRequests.delete(requestId);
    if (receipt.phase === 'confirmed') {
      pending.resolve({
        type: 'command_result',
        request_id: requestId,
        action: pending.action,
        ok: true,
        phase: receipt.phase,
        data: { receipt },
      });
      return;
    }
    const detail = parseCommandErrorDetail(message.detail);
    const error = new CommandError(detail || receipt.error?.code || 'Command failed');
    error.data = {
      phase: receipt.phase,
      ...(receipt.phase === 'failed_before_dispatch' ? { not_started: true } : {}),
      ...(receipt.error ? { api_error: receipt.error } : {}),
      ...(receipt.phase === 'dispatched_unknown' ? { dispatched_unknown: true } : {}),
    };
    pending.reject(error);
  }

  private handleApiError(relayId: string, message: Record<string, unknown>): void {
    const requestId = typeof message.request_id === 'string' ? message.request_id : '';
    const pending = requestId ? this.pendingRequests.get(requestId) : undefined;
    const apiError = parseApiError(message.error);
    if (!pending || pending.relayId !== relayId || !apiError) return;
    releasePendingResources(pending);
    this.pendingRequests.delete(requestId);
    const detail = parseCommandErrorDetail(message.detail);
    const error = new CommandError(detail || apiError.code);
    error.data = { phase: 'failed_before_dispatch', api_error: apiError };
    pending.reject(error);
  }

  private handleCommandResult(relayId: string, result: CommandResult): void {
    const pending = this.pendingRequests.get(result.request_id);
    if (!pending || pending.relayId !== relayId) return;
    if (result.phase === 'accepted') {
      if (pending.action === 'agent_start') {
        this.showToast('Start accepted; waiting for confirmation…');
        return;
      }
      // The relay already acted; give the final confirmation its own window
      // instead of counting it against the original send timeout.
      resetPendingDeadline(pending, () => {
        pending.cleanup?.();
        this.pendingRequests.delete(result.request_id);
        pending.reject(dispatchedUnknownError('Relay confirmation timed out'));
      }, ACCEPTED_COMMAND_TIMEOUT_MS);
      this.showToast('Command accepted; waiting for agent…');
      return;
    }
    releasePendingResources(pending);
    this.pendingRequests.delete(result.request_id);
    if (result.ok) pending.resolve(result);
    else {
      const error = new CommandError(result.error || 'Command failed');
      error.data = { ...(result.data || {}), ...(result.phase === 'not_started' ? { not_started: true } : {}) };
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
      releasePendingResources(pending);
      operations.delete(requestId);
      // Every entry here survived its write, so the frame is already on the
      // wire and the relay may act on it after this rejection.
      pending.reject(dispatchedUnknownError(message));
    }
  }

  private rejectPendingOperations(relayId: string, message: string): void {
    this.rejectPending(this.pendingRequests, relayId, message);
    this.rejectPending(this.pendingUploads, relayId, message);
    // The relay releases a departed connection's size leases itself, and a
    // reconnect is a new lease owner, so this phone owns none there any more.
    for (const [key, agent] of this.ownedPaneLeases) {
      if (agent.relay_id === relayId) this.ownedPaneLeases.delete(key);
    }
  }

  async leasePaneSize(
    agent: Agent,
    columns: number,
    rows = 0,
  ): Promise<{ columns: number; rows: number }> {
    if (!get(terminalResizeSession)) {
      throw new CommandError('Resize Session is off');
    }
    // Only a relay that understands opt-in leases is ever asked. Older relays
    // advertise pane_size_lease and resize on any lease; they never receive
    // one from this app.
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('pane_size_opt_in')) {
      throw new CommandError('Resize Session is unavailable on this relay');
    }
    if (!Number.isInteger(columns) || columns < MIN_PANE_SIZE_COLUMNS || columns > MAX_PANE_SIZE_COLUMNS) {
      throw new CommandError(
        `Terminal columns must be between ${MIN_PANE_SIZE_COLUMNS} and ${MAX_PANE_SIZE_COLUMNS}`,
      );
    }
    // Rows ride the same lease only when the relay understands them; the
    // client must not believe a height was applied otherwise.
    const leaseRows = connection.capabilities.includes('pane_size_opt_in_rows') ? rows : 0;
    if (leaseRows !== 0
      && (!Number.isInteger(leaseRows) || leaseRows < MIN_PANE_SIZE_ROWS || leaseRows > MAX_PANE_SIZE_ROWS)) {
      throw new CommandError(
        `Terminal rows must be between ${MIN_PANE_SIZE_ROWS} and ${MAX_PANE_SIZE_ROWS}`,
      );
    }
    const payload: Record<string, any> = { type: 'lease_pane_size', columns, opt_in: true };
    if (leaseRows) payload.rows = leaseRows;
    const epoch = this.paneLeaseEpoch;
    // Recorded before the request leaves: turning Resize Session off while
    // the reply is outstanding still releases this pane, and the relay
    // handles that release after the lease.
    this.ownedPaneLeases.set(paneLeaseKey(agent), agent);
    const result = await this.sendToAgent(agent, payload);
    if (epoch !== this.paneLeaseEpoch || !get(terminalResizeSession)) {
      // A late reply after the switch was turned off must not reactivate
      // resizing; the pane has already been released.
      throw new CommandError('Resize Session was turned off');
    }
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

  async releasePaneSize(agent: Agent, options: { immediate?: boolean } = {}): Promise<void> {
    // Cleanup does not require consent or capability advertisement. Without
    // immediate the relay keeps the size for a short navigation grace.
    const payload: Record<string, any> = { type: 'release_pane_size' };
    if (options.immediate) {
      payload.immediate = true;
      this.ownedPaneLeases.delete(paneLeaseKey(agent));
    }
    const result = await this.sendToAgent(agent, payload);
    if (result.action && result.action !== 'release_pane_size') {
      throw new CommandError('Relay returned the wrong pane-size release confirmation');
    }
  }

  // Turning Resize Session off ends every lease this phone may hold at once,
  // without the navigation grace, and invalidates replies still in flight.
  releaseAllPaneSizeLeases(): void {
    this.paneLeaseEpoch += 1;
    const owned = [...this.ownedPaneLeases.values()];
    for (const agent of owned) {
      void this.releasePaneSize(agent, { immediate: true }).catch(() => {
        // The relay also releases a disconnected phone's leases, and a lapsed
        // lease is restored by its TTL; there is nothing left to retry here.
      });
    }
  }

  async getConversationHistory(agent: Agent, request: ConversationHistoryRequest = {}): Promise<ConversationPage> {
    const limit = Math.max(1, Math.min(200, Math.trunc(request.limit || 80)));
    const cursor = request.cursor || '';
    const result = await this.sendToAgent(agent, {
      type: 'get_conversation_history',
      cursor,
      limit,
      retry: request.retry,
      observe: this.connectionsValue.get(agent.relay_id)?.capabilities.includes('conversation_activity') === true,
    }, 20_000, request.signal);
    return normalizeConversationPage(result.data, this.connectionsValue.get(agent.relay_id)?.capabilities.includes('conversation_edit_diffs'));
  }

  async getConversationUpdates(agent: Agent, sourceRevision: string, after: number, signal: AbortSignal, subscriptionId: string): Promise<import('./types').ConversationBatch> {
    const result = await this.sendToAgent(agent, { type: 'get_conversation_updates', source_revision: sourceRevision, after, subscription_id: subscriptionId }, 15_000, signal);
    return normalizeConversationBatch(result.data, this.connectionsValue.get(agent.relay_id)?.capabilities.includes('conversation_edit_diffs'));
  }

  stopConversationUpdates(agent: Agent, subscriptionId: string): void {
    if (!this.connectionsValue.get(agent.relay_id)?.capabilities.includes('conversation_activity')) return;
    void this.sendToAgent(agent, { type: 'unwatch_conversation', subscription_id: subscriptionId }, 3000).catch(() => {});
  }

  readPane(agent: Agent, force = false, lines?: number): void {
    this.panes.readPane(agent, force, lines);
  }

  watchPane(agent: Agent): void {
    this.panes.watchPane(agent);
  }

  unwatchPane(agent: Agent): void {
    this.panes.unwatchPane(agent);
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
    if (this.deviceCredential(agent.relay_id)?.role === 'reader') return;
    await this.sendToAgent(agent, { type: 'acknowledge_pane' }).catch((error) => this.showToast(error.message, true));
  }

  async respond(agent: Agent, index: number, total: number, choice?: string, source = 'App'): Promise<boolean> {
    if (index < 0) return false;
    const label = choice || approvalOptions(agent)[index] || '';
    if (!label || !agent.approval_fingerprint) {
      this.showToast('This approval no longer has an exact verified identity.', true);
      return false;
    }
    this.markResponding(agent.pane_id);
    const owner = this.connectionsValue.get(agent.relay_id);
    try {
      const result = await this.sendToAgent(agent, {
        type: 'respond',
        index,
        total,
        choice: label,
        source,
        event_id: agent.event_id || '',
        approval_fingerprint: agent.approval_fingerprint,
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
      this.scheduleDelayedPaneRead(agent, owner, 500);
    }
  }

  async answerQuestion(agent: Agent, interaction: QuestionInteraction, draft: QuestionDraft): Promise<CommandResult> {
    this.markResponding(agent.pane_id);
    const owner = this.connectionsValue.get(agent.relay_id);
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
      this.scheduleDelayedPaneRead(agent, owner, 400);
    }
  }

  async navigateQuestionPrevious(agent: Agent, interaction: QuestionInteraction): Promise<CommandResult> {
    this.markResponding(agent.pane_id);
    const owner = this.connectionsValue.get(agent.relay_id);
    try {
      return await this.sendToAgent(agent, {
        type: 'navigate_question',
        interaction_id: interaction.id,
        direction: 'previous',
        source: 'App',
      }, 20_000);
    } finally {
      this.scheduleDelayedPaneRead(agent, owner, 400);
    }
  }

  async clarifyQuestion(agent: Agent, interaction: QuestionInteraction): Promise<CommandResult> {
    this.markResponding(agent.pane_id);
    const owner = this.connectionsValue.get(agent.relay_id);
    try {
      return await this.sendToAgent(agent, {
        type: 'clarify_question',
        interaction_id: interaction.id,
        source: 'App',
      }, 20_000);
    } finally {
      this.scheduleDelayedPaneRead(agent, owner, 400);
    }
  }

  applyQuestionInteraction(agent: Agent, interaction: QuestionInteraction | null): void {
    const current = this.agentsValue.find((item) => item.pane_id === agent.pane_id);
    if (!current || agentTargetsDiffer(agent, current)) return;
    this.clearResponding(agent.pane_id);
    if (this.connectionsValue.get(agent.relay_id)?.capabilities.includes('idle_question_probe')
      && (!rawBlocked(current) || current.screen_attention === 'waiting')) return;
    this.blockedSnapshotMisses.delete(agent.pane_id);
    this.agentsValue = this.agentsValue.map((item) => item.pane_id === agent.pane_id
      ? {
          ...item,
          status: interaction ? 'blocked' : 'working',
          attention_kind: interaction ? 'question' : undefined,
          interaction,
        }
      : item);
    this.publishAgents('pane content interaction');
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
      const rawListing = result.data;
      if (!rawListing || typeof rawListing !== 'object') {
        throw new CommandError('Relay returned an invalid directory listing');
      }
      const listing = rawListing as unknown as DirectoryListing;
      const currentPath = (listing.current as { path?: unknown } | null | undefined)?.path;
      if (typeof currentPath !== 'string' || !currentPath.trim()) {
        throw new CommandError('Relay returned an invalid directory listing');
      }
      listing.directories = Array.isArray(listing.directories) ? listing.directories : [];
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

  private adoptSpeechVoices(connection: RelayConnection, payload: Record<string, unknown>): void {
    connection.speechCacheDir = String(payload.cache_dir || '');
    connection.speechEngineInstalled = payload.engine_installed === true;
    connection.speechLanguages = (Array.isArray(payload.languages) ? payload.languages : [])
      .filter(isSpeechLanguage);
    connection.speechVoices = (Array.isArray(payload.voices) ? payload.voices : [])
      .flatMap((value: unknown): RelaySpeechVoice[] => {
        if (!value || typeof value !== 'object') return [];
        const record = value as Record<string, unknown>;
        const language = record.language;
        if (!isSpeechLanguage(language)) return [];
        const bytes = Number(record.bytes);
        return [{
          language,
          name: String(record.name || ''),
          installed: record.installed === true,
          bytes: Number.isFinite(bytes) && bytes > 0 ? bytes : 0,
          engine: String(record.engine || ''),
        }];
      });
    adoptRelaySpeech(connection.speechLanguages);
  }

  private applySpeechVoices(
    relayId: string,
    connection: RelayConnection,
    payload: Record<string, unknown> | undefined,
  ): RelaySpeechVoice[] {
    if (!this.isCurrentConnection(relayId, connection)) {
      throw new CommandError('Relay reconnected while managing its speech voices');
    }
    this.adoptSpeechVoices(connection, payload || {});
    this.emitConnections();
    return connection.speechVoices;
  }

  private speechVoiceConnection(relayId: string): RelayConnection {
    const connection = this.connectionsValue.get(relayId);
    if (!connection?.capabilities.includes('speech_voice_management')) {
      throw new CommandError('This relay does not support phone-managed speech voices yet');
    }
    return connection;
  }

  async listSpeechVoices(relayId: string): Promise<RelaySpeechVoice[]> {
    const connection = this.speechVoiceConnection(relayId);
    const result = await this.sendCommand(relayId, { type: 'speech_voices_list' });
    return this.applySpeechVoices(relayId, connection, result.data);
  }

  // A voice is tens of megabytes and the first one installs the speech engine
  // too, so this waits far longer than an ordinary command.
  async installSpeechVoice(relayId: string, language: string): Promise<RelaySpeechVoice[]> {
    const connection = this.speechVoiceConnection(relayId);
    const result = await this.sendCommand(relayId, { type: 'speech_voice_install', language }, 300_000);
    return this.applySpeechVoices(relayId, connection, result.data);
  }

  async removeSpeechVoice(relayId: string, language: string): Promise<RelaySpeechVoice[]> {
    const connection = this.speechVoiceConnection(relayId);
    const result = await this.sendCommand(relayId, { type: 'speech_voice_remove', language });
    return this.applySpeechVoices(relayId, connection, result.data);
  }


  async loadSlashCommands(agent: Agent, refresh = false): Promise<SlashCommandCatalog> {
    const connection = this.connectionsValue.get(agent.relay_id);
    if (!connection?.capabilities.includes('slash_commands')) {
      throw new CommandError('This relay does not provide slash-command suggestions.');
    }
    const identity = inputContextIdentity(agent);
    const pending = this.pendingSlashCommands.get(agent.pane_id);
    if (!refresh && pending?.identity === identity) return pending.promise;
    const cached = this.slashCommandCache.get(agent.pane_id);
    if (!refresh && cached?.identity === identity) return cached.catalog;
    this.slashCommandCache.delete(agent.pane_id);

    const promise = this.sendToAgent(agent, { type: 'list_slash_commands' }, 10_000)
      .then((result) => {
        const data = result.data && typeof result.data === 'object' ? result.data : {};
        const catalog = normalizeSlashCommandCatalog(data);
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

  attachmentController(agent: Agent): AttachmentBatchController {
    const target = targetRefForAgent(agent);
    if (!target) throw new CommandError('This terminal does not have a stable attachment target.');
    const backendTarget: TargetRef = {
      server_session_id: target.server_session_id,
      pane_id: target.pane_id,
      terminal_id: target.terminal_id,
      generation: target.generation,
      ...(target.agent_session_id ? { agent_session_id: target.agent_session_id } : {}),
    };
    return new AttachmentBatchController(backendTarget, this.attachmentUploadCallbacks(agent.relay_id), {
      maxFiles: 8,
      maxFileBytes: 20 * 1024 * 1024,
      maxBatchBytes: 50 * 1024 * 1024,
      maxChunkBytes: 256 * 1024,
    });
  }

  private attachmentUploadCallbacks(relayId: string): AttachmentUploadCallbacks {
    return {
      begin: (request, signal) => this.sendUploadRequest<UploadBeginResult>(
        relayId, 'upload_begin', 'upload_begin_result', request, signal,
      ),
      chunk: (request, signal) => this.sendUploadRequest<UploadChunkResult>(
        relayId,
        'upload_chunk',
        'upload_chunk_result',
        { ...request, data: base64Encode(request.data) },
        signal,
      ),
      finish: (request, signal) => this.sendUploadRequest<UploadFinishResult>(
        relayId, 'upload_finish', 'upload_finish_result', request, signal,
      ),
      cancel: (request) => this.sendUploadRequest<Record<string, any>>(
        relayId, 'upload_cancel', 'upload_cancel_result', request,
      ).then(() => undefined),
    };
  }

  private sendUploadRequest<TResult extends Record<string, any>>(
    relayId: string,
    type: string,
    responseType: string,
    request: unknown,
    signal?: AbortSignal,
  ): Promise<TResult> {
    const connection = this.connectionsValue.get(relayId);
    if (!connection || connection.status !== 'connected') {
      return Promise.reject(new CommandError('Relay is not connected.'));
    }
    const protocolError = relayProtocolError(connection);
    if (protocolError) return Promise.reject(new CommandError(protocolError));
    if (signal?.aborted) return Promise.reject(new CommandError('Attachment upload was cancelled.'));
    const requestId = commandRequestId();
    return this.sendPending<PendingUpload, TResult>({
      operations: this.pendingUploads,
      relayId,
      requestId,
      frame: () => ({
        type,
        protocol: RELAY_PROTOCOL_VERSION,
        request_id: requestId,
        client_id: pushClientId(),
        ...(request as Record<string, unknown>),
      }),
      signal,
      timeoutMs: ATTACHMENT_UPLOAD_TIMEOUT_MS,
      timeoutMessage: 'Attachment upload did not finish in time.',
      sendFailureMessage: 'Could not send attachment upload request.',
      abortError: () => {
        const error = new CommandError('Attachment upload was interrupted.');
        error.code = 'attachment_upload_interrupted';
        return error;
      },
      createPending: (base) => ({
        ...base,
        responseType,
        resolve: (result) => base.resolve(result as TResult),
      }),
    });
  }

  private handleUploadResult(relayId: string, message: Record<string, any>): void {
    const requestId = String(message.request_id || '');
    const pending = this.pendingUploads.get(requestId);
    if (!pending || pending.relayId !== relayId || pending.responseType !== message.type) return;
    releasePendingResources(pending);
    this.pendingUploads.delete(requestId);
    const apiError = parseApiError(message.error);
    if (message.error !== undefined) {
      const error = new CommandError(apiError?.code || 'Attachment upload failed.');
      error.code = apiError?.code;
      error.args = apiError?.args;
      pending.reject(error);
      return;
    }
    if (!isRecord(message.result)) {
      const error = new CommandError('Relay returned an invalid attachment upload result.');
      error.code = 'attachment_invalid_response';
      pending.reject(error);
      return;
    }
    pending.resolve(message.result);
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

function clearChangedRelayPreviews(before: RelayConfig[], after: RelayConfig[]): void {
  const previous = new Map(before.map((relay) => [relay.id, relay]));
  for (const relay of after) {
    const old = previous.get(relay.id);
    if (old && relayConnectionIdentityChanged(old, relay)) clearConversationPreviewsForRelay(relay.id);
  }
}

function relayConnectionIdentityChanged(before: RelayConfig, after: RelayConfig): boolean {
  return before.url !== after.url
    || before.token !== after.token
    || before.transport !== after.transport
    || before.gatewayUrl !== after.gatewayUrl
    || (before.gatewayUrls || []).join('\u0000') !== (after.gatewayUrls || []).join('\u0000')
    || before.gatewayRelayId !== after.gatewayRelayId
    || before.rendezvousKey !== after.rendezvousKey;
}

function paneLeaseKey(agent: Agent): string {
  return `${agent.relay_id}\u0000${agent.pane_id}`;
}

export function pushClientId(): string {
  let value = localStorage.getItem('herdr_push_client_id');
  if (value) return value;
  value = crypto.randomUUID ? crypto.randomUUID() : `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
  localStorage.setItem('herdr_push_client_id', value);
  return value;
}

export const relayStore = new RelayStore();
