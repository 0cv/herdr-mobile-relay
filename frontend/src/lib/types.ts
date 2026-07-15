export type RelayStatus = 'connecting' | 'connected' | 'disconnected';

export interface RelayConfig {
  id: string;
  label: string;
  url: string;
  token: string;
}

export interface AgentProfile {
  id: string;
  label?: string;
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

export interface QuestionOption {
  index: number;
  label: string;
  description?: string;
  selected?: boolean;
}

export interface QuestionOther {
  label?: string;
  placeholder?: string;
  selected?: boolean;
  text?: string;
  allow_empty?: boolean;
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
  project?: string;
  cwd?: string;
  host?: string;
  updated_at?: number | string;
  prompt?: string;
  command?: string;
  options?: string[];
  interaction?: QuestionInteraction | null;
  question_layout?: boolean;
  event_id?: string;
  tab_id?: string;
  tab_label?: string;
  tab_number?: number;
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
  project?: string;
  agent?: string;
  request_id?: string;
  details?: Record<string, unknown>;
  relay_id: string;
  relay_label: string;
  activity_key: string;
}

export interface RelayConnectionView {
  relay: RelayConfig;
  status: RelayStatus;
  host: string;
  protocol: number;
  version: string;
  capabilities: string[];
  agentProfiles: AgentProfile[];
  directoryBrowser: DirectoryListing | null;
  directoryLoading: boolean;
  directoryError: string;
  pushStatus: string;
  vapidPublicKey: string;
}

export interface CommandResult {
  type: 'command_result';
  request_id: string;
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
