import type { Agent, QuestionInteraction } from './types';
import { stripAnsi } from './terminal';

const MENU_LINE_RE = /^\s*[❯›]?\s*\d+\.\s+.+$/;
const COMMAND_LINE_RE = /^\s*(?:[$>❯›])\s+(.+?)\s*$/;
const PROMPT_SKIP_RE = /^(?:bash command|do you want to proceed\??|would you like to run\b.*|environment:\s*\w+|press enter to confirm\b.*|esc to cancel\b.*)$/i;

export function agentStatusGroup(agent: Partial<Agent> | null | undefined): 'blocked' | 'working' | 'done' | 'ready' | 'other' {
  const status = String(agent?.status || 'unknown').trim().toLowerCase().replace(/[_-]+/g, ' ');
  if (status.includes('blocked')) return 'blocked';
  if (/(working|running|progress|busy)/.test(status)) return 'working';
  if (/(done|complete|finish|success|unread)/.test(status)) return 'done';
  if (status === 'idle' || status === 'ready') return 'ready';
  return 'other';
}

export function agentStatusTone(agent: Partial<Agent> | null | undefined): 'danger' | 'warning' | 'success' | 'muted' {
  const group = agentStatusGroup(agent);
  if (group === 'blocked') return 'danger';
  if (group === 'working') return 'warning';
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

export function compareAgentUpdatedAt(a: Agent, b: Agent): number {
  return agentUpdatedAt(b) - agentUpdatedAt(a);
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
  const options = Array.isArray(agent?.options) ? agent.options.filter(Boolean) : [];
  if (options.length) return options;
  if (agent?.question_layout) return [];
  return agentStatusGroup(agent) === 'blocked'
    ? ['yes, single permission', 'trust, always allow', 'no (tab to edit)']
    : [];
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
  const interaction = agent?.interaction;
  if (!interaction || typeof interaction !== 'object') return null;
  if (!['single_select', 'multi_select'].includes(interaction.kind)) return null;
  if (!interaction.id || !interaction.question || !Array.isArray(interaction.options)) return null;
  return interaction;
}

export function clientPaneId(relayId: string, rawPaneId: string): string {
  return `${relayId}::${rawPaneId}`;
}

export function normalizeAgent(relayId: string, relayLabel: string, agent: Partial<Agent>): Agent {
  const rawPaneId = String(agent.raw_pane_id || agent.pane_id || '');
  return {
    ...agent,
    relay_id: relayId,
    relay_label: relayLabel,
    raw_pane_id: rawPaneId,
    pane_id: clientPaneId(relayId, rawPaneId),
  } as Agent;
}

export function stabilizeBlockedSnapshot(
  previous: Agent | undefined,
  next: Agent,
  misses: Map<string, number>,
  responding: Set<string>,
): Agent {
  const paneId = next.pane_id;
  if (!paneId || agentStatusGroup(next) === 'blocked') {
    if (paneId) misses.delete(paneId);
    return next;
  }
  if (!previous || agentStatusGroup(previous) !== 'blocked' || responding.has(paneId)) {
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
  const blocked = next.status === 'blocked';
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
    prompt: blocked ? (next.prompt ?? previous.prompt) : next.prompt,
    command: blocked ? (next.command ?? previous.command) : next.command,
    options: blocked ? (next.options ?? previous.options) : next.options,
    interaction: blocked && !hasInteraction ? previous.interaction : next.interaction,
    question_layout: blocked && !hasQuestionLayout ? previous.question_layout : next.question_layout,
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
    return mergeAgentDetails(before, stabilizeBlockedSnapshot(before, agent, misses, responding));
  });
  const live = new Set(incoming.map((agent) => agent.pane_id));
  for (const paneId of misses.keys()) {
    if (paneId.startsWith(`${relayId}::`) && !live.has(paneId)) misses.delete(paneId);
  }
  return retained.concat(merged);
}
