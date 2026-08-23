import type { TransportKind } from './transports/types';

export type RelayStatus = 'connecting' | 'connected' | 'disconnected';
export type AttentionKind = 'approval' | 'question' | 'chat' | 'unknown';

export type AgentInventoryState = 'starting' | 'ready' | 'error';

export interface AgentInventoryStatus {
  state: AgentInventoryState;
  errorCode: string;
  message: string;
  lastAttemptAt: number;
  lastSuccessAt: number;
  stale: boolean;
}

export interface RelayConfig {
  id: string;
  label: string;
  /** Direct relay WebSocket URL. Empty for gateway-reachable computers. */
  url: string;
  token: string;
  /**
   * Which path reaches this computer. A missing value means the legacy setup:
   * a plain WSS URL in `url`. `'hybrid'` means the computer is reachable
   * through `gatewayUrl`, with a direct WebRTC upgrade attempted on top.
   */
  transport?: 'websocket' | 'hybrid';
  /** Preferred gateway address, e.g. `wss://gw.example.com`. Hybrid relays only. */
  gatewayUrl?: string;
  /**
   * Every gateway this computer answers on, most preferred first, and always
   * starting with `gatewayUrl`. A config stored before the list existed
   * normalizes to its single primary, so both fields always agree.
   */
  gatewayUrls?: string[];
}

export interface AgentProfile {
  id: string;
  label?: string;
}

export interface WorkspaceWorktree {
  repo_key: string;
  repo_name: string;
  repo_root: string;
  checkout_path: string;
  is_linked_worktree: boolean;
}

export interface RelayWorkspace {
  relay_id: string;
  relay_label: string;
  workspace_id: string;
  number: number;
  label: string;
  focused: boolean;
  pane_count: number;
  tab_count: number;
  active_tab_id: string;
  agent_status: string;
  cwd: string;
  worktree?: WorkspaceWorktree | null;
}

export interface WorktreeInfo {
  path: string;
  branch: string | null;
  is_bare: boolean;
  is_detached: boolean;
  is_prunable: boolean;
  is_linked_worktree: boolean;
  label: string;
  open_workspace_id: string | null;
}

export interface WorktreeSource {
  repo_key: string;
  repo_name: string;
  repo_root: string;
  source_checkout_path: string;
  source_workspace_id: string | null;
}

export interface WorktreeListing {
  source: WorktreeSource;
  worktrees: WorktreeInfo[];
}

export interface DirectoryEntry {
  name: string;
  path: string;
}

export interface DirectoryListing {
  current: { path: string; label: string };
  parent: string;
  directories: DirectoryEntry[];
}

export interface SlashCommand {
  command: string;
  description: string;
  argument_hint?: string;
  source: 'builtin' | 'personal' | 'project';
}

export interface SlashCommandCatalog {
  commands: SlashCommand[];
  truncated: boolean;
}

export interface WorkspaceTreeEntry {
  path: string;
  name: string;
  kind: 'directory' | 'file';
  size?: number;
}

export interface WorkspaceTree {
  root: string;
  entries: WorkspaceTreeEntry[];
  truncated?: boolean;
}

export interface WorkspaceFile {
  path: string;
  media_type: string;
  kind: 'text' | 'image';
  text?: string;
  data_url?: string;
  size: number;
}

export interface WorkspaceGitFile {
  path: string;
  original_path?: string;
  status: string;
}

export interface WorkspaceGitStatus {
  available: boolean;
  branch?: string;
  files: WorkspaceGitFile[];
  truncated?: boolean;
}

export interface WorkspaceGitDiff {
  path: string;
  diff: string;
}

export interface QuestionOption {
  index: number;
  label: string;
  description?: string;
  selected?: boolean;
  summary?: { q: string; a: string }[];
}

export interface QuestionOther {
  label?: string;
  placeholder?: string;
  selected?: boolean;
  text?: string;
  allow_empty?: boolean;
  hidden?: boolean;
}

export interface QuestionInteraction {
  id: string;
  kind: 'single_select' | 'multi_select';
  question: string;
  options: QuestionOption[];
  other?: QuestionOther;
  submit_label?: string;
  can_go_back?: boolean;
  can_chat?: boolean;
  question_index?: number;
  question_total?: number;
}

export interface Agent {
  relay_id: string;
  relay_label: string;
  raw_pane_id: string;
  pane_id: string;
  agent?: string;
  name?: string;
  status?: string;
  session?: string;
  session_name?: string;
  project?: string;
  cwd?: string;
  host?: string;
  updated_at?: number | string;
  last_active_at?: number | string;
  last_seen_at?: number | string;
  activity_seq?: number | string;
  pane_revision?: number;
  prompt?: string;
  command?: string;
  options?: string[];
  interaction?: QuestionInteraction | null;
  question_layout?: boolean;
  event_id?: string;
  attention_kind?: AttentionKind;
  attention_capable?: boolean;
  terminal_id?: string;
  conversation_history_available?: boolean;
  tab_id?: string;
  tab_label?: string;
  tab_number?: number;
  tab_order?: number;
  workspace_id?: string;
  [key: string]: unknown;
}

export interface Activity {
  id?: string;
  timestamp: number | string;
  summary?: string;
  kind?: string;
  status?: string;
  host?: string;
  pane_id?: string;
  project?: string;
  session?: string;
  agent?: string;
  request_id?: string;
  extract?: string;
  details?: Record<string, unknown>;
  relay_id: string;
  relay_label: string;

  activity_key: string;
}

export interface ConversationTool {
  id?: string;
  name: string;
  input?: string;
  output?: string;
  error?: boolean;
  truncated?: boolean;
}

export interface ConversationEntry {
  id: string;
  timestamp: string;
  role: 'user' | 'assistant';
  text: string;
  truncated?: boolean;
  tools?: ConversationTool[];
}

export interface ConversationPage {
  available: boolean;
  reason: string;
  entries: ConversationEntry[];
  hasMore: boolean;
  total: number;
  fileTruncated: boolean;
}

export interface RelayConnectionView {
  relay: RelayConfig;
  status: RelayStatus;
  /**
   * Physical path currently carrying traffic. `gateway` means the blind WSS
   * fallback, `webrtc` the direct DataChannel, `websocket` the legacy relay
   * URL. Empty until the first successful connection.
   */
  path: TransportKind | '';
  /**
   * Gateway carrying the session, or the one that signalled a direct path.
   * Empty on the relay-URL path and before the first successful connection.
   */
  activeGatewayUrl: string;
  host: string;
  protocol: number;
  version: string;
  releaseVersion: string;
  revision: string;
  /** Build version reported by the active gateway through the encrypted relay. */
  gatewayVersion?: string;
  gatewayAvailableVersion?: string;
  update: RelayUpdateStatus;
  appDeploy: AppDeploymentStatus;
  inventory: AgentInventoryStatus;
  capabilities: string[];
  agentProfiles: AgentProfile[];
  directoryBrowser: DirectoryListing | null;
  directoryLoading: boolean;
  directoryError: string;
  pushStatus: string;
  vapidPublicKey: string;
}

export type RelayUpdateState =
  | 'checking'
  | 'current'
  | 'available'
  | 'blocked'
  | 'scheduled'
  | 'preparing'
  | 'deploying_app'
  | 'installing'
  | 'restarting'
  | 'succeeded'
  | 'failed'
  | 'rolled_back'
  | 'unsupported';

export interface RelayUpdateStatus {
  state: RelayUpdateState;
  current_version: string;
  current_revision: string;
  available_version: string;
  available_revision: string;
  target_revision: string;
  upstream_version: string;
  upstream_revision: string;
  checked_at: number;
  can_install: boolean;
  mode: string;
  reason: string;
  error: string;
}

export interface AppDeploymentStatus {
  configured: boolean;
  origin: string;
  project: string;
  branch: string;
  revision: string;
  reason: string;
  state: 'idle' | 'scheduled' | 'deploying' | 'succeeded' | 'failed';
  target_version: string;
  target_revision: string;
  checked_at: number;
  error: string;
}

export interface AppUpdateStatus {
  state: 'checking' | 'current' | 'reload-ready' | 'deployment-required' | 'failed';
  currentVersion: string;
  currentAssets: number;
  deployedVersion: string;
  deployedAssets: number;
  upstreamVersion: string;
  upstreamAssets: number;
  checkedAt: number;
  error: string;
}

export interface CommandResult {
  type: 'command_result';
  request_id: string;
  action?: string;
  ok: boolean;
  phase?: string;
  error?: string;
  data?: Record<string, any>;
}

export interface NotificationTarget {
  pane_id: string;
  host: string;
  action: 'approve' | 'deny' | '';
  index: number | null;
  total: number | null;
  notification_id: string;
}

export interface TerminalFrame {
  paneId: string;
  content: string;
  format: string;
  truncated?: boolean;
  viewportOnly?: boolean;
  viewportRows?: number;
  /** Pane is asking for a secret with terminal echo disabled. */
  noEcho?: boolean;
  /** The recognized secret prompt line, when noEcho is set. */
  noEchoPrompt?: string;
  /** Frame read inside the post-resize settle window; its rows must not be committed to history. */
  resizeSettling?: boolean;
}

export interface ToastMessage {
  id: number;
  message: string;
  error: boolean;
}

export interface QuestionDraft {
  selected: Set<number>;
  otherSelected: boolean;
  otherText: string;
}
