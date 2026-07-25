import type { Agent, AttentionKind, QuestionInteraction } from './types';
import { stripAnsi } from './terminal';

const MENU_LINE_RE = /^\s*[❯›]?\s*\d+\.\s+.+$/;
const COMMAND_LINE_RE = /^\s*(?:[$>❯›])\s+(.+?)\s*$/;
const PROMPT_SKIP_RE = /^(?:bash command|do you want to proceed\??|would you like to run\b.*|environment:\s*\w+|press enter to confirm\b.*|esc to cancel\b.*)$/i;

export function rawBlocked(agent: Partial<Agent> | null | undefined): boolean {
  const status = String(agent?.status || 'unknown').trim().toLowerCase().replace(/[_-]+/g, ' ');
  return status.includes('blocked');
}

export function attentionKind(agent: Partial<Agent> | null | undefined): AttentionKind | '' {
  const kind = String(agent?.attention_kind || '');
  return ['approval', 'question', 'chat', 'unknown'].includes(kind)
    ? kind as AttentionKind
    : '';
}

export function agentNeedsResponse(agent: Partial<Agent> | null | undefined): boolean {
  return rawBlocked(agent) && ['approval', 'question'].includes(attentionKind(agent));
}

export function agentNeedsInspection(agent: Partial<Agent> | null | undefined): boolean {
  const kind = attentionKind(agent);
  return rawBlocked(agent) && !['approval', 'question', 'chat'].includes(kind);
}

export function agentStatusGroup(agent: Partial<Agent> | null | undefined): 'attention' | 'blocked' | 'working' | 'done' | 'ready' | 'other' {
  const status = String(agent?.status || 'unknown').trim().toLowerCase().replace(/[_-]+/g, ' ');
  if (status.includes('blocked')) {
    const kind = attentionKind(agent);
    if (kind === 'approval' || kind === 'question') return 'blocked';
    if (kind === 'chat') return 'ready';
    return 'attention';
  }
  if (/(working|running|progress|busy)/.test(status)) return 'working';
  if (/(done|complete|finish|success|unread)/.test(status)) return 'done';
  if (status === 'idle' || status === 'ready') return 'ready';
  return 'other';
}

export function agentStatusTone(agent: Partial<Agent> | null | undefined): 'danger' | 'warning' | 'success' | 'muted' {
  const group = agentStatusGroup(agent);
  if (group === 'blocked') return 'danger';
  if (group === 'attention' || group === 'working') return 'warning';
  if (group === 'done') return 'success';
  return 'muted';
}

export function hostLabel(agent: Partial<Agent>): string {
  return String(agent.relay_label || agent.host || 'relay');
}

export function tabName(agent: Partial<Agent>): string {
  // The Herdr tab label is the authoritative "tab name" the user manages in the
  // desktop panel, so prefer it; fall back to the pane's own name only when a
  // pane has no labelled tab. This keeps laptop tab renames reflected on-device.
  return String(agent.tab_label || agent.name || '').trim();
}

export function agentContextLabel(agent: Partial<Agent>): string {
  const name = tabName(agent);
  if (name && name !== agent.project) return name;
  return String(agent.cwd || '').split(/[\\/]/).filter(Boolean).pop() || '';
}

export function displayName(agent: Partial<Agent>): string {
  return String(agent.project || agent.name || agent.tab_label || agent.agent || 'agent');
}

export function agentUpdatedAt(agent: Partial<Agent> | null | undefined): number {
  const value = Number(agent?.updated_at);
  return Number.isFinite(value) ? value : 0;
}

export function agentActivitySeq(agent: Partial<Agent> | null | undefined): number {
  const value = Number(agent?.activity_seq);
  return Number.isSafeInteger(value) && value > 0 ? value : 0;
}

export function agentPaneRevision(agent: Partial<Agent> | null | undefined): number {
  const value = Number(agent?.pane_revision);
  return Number.isSafeInteger(value) && value > 0 ? value : 0;
}

export function staleAgentRevision(previous: Agent | undefined, next: Agent): boolean {
  const previousRevision = agentPaneRevision(previous);
  const nextRevision = agentPaneRevision(next);
  return previousRevision > 0 && nextRevision > 0 && nextRevision < previousRevision;
}

export function compareAgentUpdatedAt(a: Agent, b: Agent): number {
  const timestampOrder = agentUpdatedAt(b) - agentUpdatedAt(a);
  if (timestampOrder) return timestampOrder;
  if (a.relay_id !== b.relay_id) return 0;
  return agentActivitySeq(b) - agentActivitySeq(a);
}

export function sortedAgents(agents: Agent[]): Agent[] {
  return [...agents].sort((a, b) =>
    compareAgentUpdatedAt(a, b)
    || hostLabel(a).localeCompare(hostLabel(b))
    || agentContextLabel(a).localeCompare(agentContextLabel(b))
    || String(a.project || a.agent || '').localeCompare(String(b.project || b.agent || ''))
    || String(a.agent || '').localeCompare(String(b.agent || '')),
  );
}

export function normalizeInlineText(text: unknown): string {
  return String(text ?? '').replace(/\s+/g, ' ').trim();
}

export function approvalOptions(agent: Partial<Agent> | null | undefined): string[] {
  if (!rawBlocked(agent) || agent?.attention_capable !== true || attentionKind(agent) !== 'approval') return [];
  const options = Array.isArray(agent?.options) ? agent.options.filter(Boolean) : [];
  return options.length >= 2 ? options : [];
}

export function approvalButtonTone(option: string, index: number, total: number): 'approve' | 'trust' | 'deny' {
  const value = normalizeInlineText(option).toLowerCase();
  if (index === total - 1 || /\b(no|deny|reject|cancel|exit)\b/.test(value)) return 'deny';
  if (/\b(always|trust|don't ask|dont ask|configure|edit|amend)\b/.test(value)) return 'trust';
  return 'approve';
}

export function approvalPromptPreview(agent: Partial<Agent> | null | undefined): string {
  const command = normalizeInlineText(agent?.command);
  if (command) return command;
  let commandFallback = '';
  let fallback = '';
  for (const rawLine of String(agent?.prompt || '').split(/\r?\n/)) {
    const line = normalizeInlineText(stripAnsi(rawLine).replace(/^[│|]\s*/, '').replace(/\s*[│|]$/, ''));
    if (!line || MENU_LINE_RE.test(line) || PROMPT_SKIP_RE.test(line)) continue;
    const match = COMMAND_LINE_RE.exec(line);
    if (match) commandFallback = match[1].trim();
    else fallback = line;
  }
  return commandFallback || fallback;
}

export function questionInteraction(agent: Partial<Agent> | null | undefined): QuestionInteraction | null {
  if (!rawBlocked(agent) || agent?.attention_capable !== true || attentionKind(agent) !== 'question') return null;
  const interaction = agent?.interaction;
  if (!interaction || typeof interaction !== 'object') return null;
  if (!['single_select', 'multi_select'].includes(interaction.kind)) return null;
  if (!interaction.id || !interaction.question || !Array.isArray(interaction.options)) return null;
  return interaction;
}

export function clientPaneId(relayId: string, rawPaneId: string): string {
  return `${relayId}::${rawPaneId}`;
}

export function normalizeAgent(
  relayId: string,
  relayLabel: string,
  agent: Partial<Agent>,
  attentionCapable = false,
): Agent {
  const rawPaneId = String(agent.raw_pane_id || agent.pane_id || '');
  return normalizeAgentAttention({
    ...agent,
    relay_id: relayId,
    relay_label: relayLabel,
    raw_pane_id: rawPaneId,
    pane_id: clientPaneId(relayId, rawPaneId),
  } as Agent, attentionCapable);
}

export function normalizeAgentAttention(agent: Agent, capable: boolean): Agent {
  if (!rawBlocked(agent)) {
    return {
      ...agent,
      attention_capable: capable,
      attention_kind: undefined,
      options: undefined,
      interaction: null,
      question_layout: false,
    };
  }
  const kind = capable ? attentionKind(agent) || 'unknown' : 'unknown';
  const next: Agent = {
    ...agent,
    attention_capable: capable,
    attention_kind: kind,
  };
  if (kind !== 'approval') next.options = undefined;
  if (kind !== 'question') {
    next.interaction = null;
    next.question_layout = false;
  }
  return next;
}

export function stabilizeBlockedSnapshot(
  previous: Agent | undefined,
  next: Agent,
  misses: Map<string, number>,
  responding: Set<string>,
): Agent {
  const paneId = next.pane_id;
  if (!paneId || rawBlocked(next)) {
    if (paneId) misses.delete(paneId);
    return next;
  }
  if (!previous || !rawBlocked(previous) || responding.has(paneId)) {
    misses.delete(paneId);
    return next;
  }
  const count = (misses.get(paneId) || 0) + 1;
  if (count >= 2) {
    misses.delete(paneId);
    return next;
  }
  misses.set(paneId, count);
  return { ...next, status: previous.status };
}

export function mergeAgentDetails(previous: Agent | undefined, next: Agent): Agent {
  if (!previous) return next;
  const blocked = rawBlocked(next);
  const hasAttentionKind = Object.prototype.hasOwnProperty.call(next, 'attention_kind');
  const hasInteraction = Object.prototype.hasOwnProperty.call(next, 'interaction');
  const hasQuestionLayout = Object.prototype.hasOwnProperty.call(next, 'question_layout');
  return {
    ...previous,
    ...next,
    tab_id: next.tab_id || previous.tab_id || '',
    tab_label: next.tab_label || previous.tab_label || '',
    tab_number: next.tab_number ?? previous.tab_number,
    workspace_id: next.workspace_id || previous.workspace_id || '',
    updated_at: Math.max(agentUpdatedAt(previous), agentUpdatedAt(next)),
    activity_seq: Object.prototype.hasOwnProperty.call(next, 'activity_seq')
      ? next.activity_seq
      : previous.activity_seq,
    pane_revision: Math.max(agentPaneRevision(previous), agentPaneRevision(next)) || undefined,
    prompt: blocked && !hasAttentionKind ? (next.prompt ?? previous.prompt) : next.prompt,
    command: blocked && !hasAttentionKind ? (next.command ?? previous.command) : next.command,
    options: blocked && !hasAttentionKind ? (next.options ?? previous.options) : next.options,
    interaction: blocked && !hasAttentionKind && !hasInteraction ? previous.interaction : next.interaction,
    question_layout: blocked && !hasAttentionKind && !hasQuestionLayout ? previous.question_layout : next.question_layout,
  };
}

export function mergeAgentList(
  current: Agent[],
  relayId: string,
  incoming: Agent[],
  misses: Map<string, number>,
  responding: Set<string>,
): Agent[] {
  const previous = new Map(current.map((agent) => [agent.pane_id, agent]));
  const retained = current.filter((agent) => agent.relay_id !== relayId);
  const merged = incoming.map((agent) => {
    const before = previous.get(agent.pane_id);
    if (staleAgentRevision(before, agent)) return before!;
    return mergeAgentDetails(before, stabilizeBlockedSnapshot(before, agent, misses, responding));
  });
  const live = new Set(incoming.map((agent) => agent.pane_id));
  for (const paneId of misses.keys()) {
    if (paneId.startsWith(`${relayId}::`) && !live.has(paneId)) misses.delete(paneId);
  }
  return retained.concat(merged);
}
