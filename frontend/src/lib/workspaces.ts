import {
  agentLastActiveAt,
  agentActivitySeq,
  agentStatusGroup,
  displayName,
  hostLabel,
  sortedAgents,
  tabName,
} from './agents';
import type { Agent } from './types';

export interface WorkspaceTab {
  id: string;
  label: string;
  number: number;
  agents: Agent[];
}

export interface WorkspaceGroup {
  key: string;
  relayId: string;
  relayLabel: string;
  workspaceId: string;
  label: string;
  cwd: string;
  host: string;
  agents: Agent[];
  tabs: WorkspaceTab[];
  attentionCount: number;
  workingCount: number;
  readyCount: number;
  lastActiveAt: number;
  lastActivitySeq: number;
}

function pathBase(path: string): string {
  const normalized = path.replace(/[\\/]+$/, '');
  return normalized.split(/[\\/]/).filter(Boolean).at(-1) || '';
}

export function workspaceIdentity(agent: Agent): string {
  const identity = String(agent.workspace_id || agent.cwd || agent.raw_pane_id || agent.pane_id);
  return `${agent.relay_id}\u0000${identity}`;
}

function groupLabel(agents: Agent[]): string {
  const projects = [...new Set(agents.map((agent) => String(agent.project || '')).filter(Boolean))];
  if (projects.length === 1) return projects[0];
  const cwdNames = [...new Set(agents.map((agent) => pathBase(String(agent.cwd || ''))).filter(Boolean))];
  if (cwdNames.length === 1) return cwdNames[0];
  const firstTab = agents.map(tabName).find(Boolean);
  return firstTab || 'Workspace';
}

function groupCwd(agents: Agent[]): string {
  const paths = [...new Set(agents.map((agent) => String(agent.cwd || '')).filter(Boolean))];
  if (!paths.length) return '';
  return paths.sort((left, right) => left.length - right.length || left.localeCompare(right))[0];
}

export function workspaceGroups(agents: Agent[]): WorkspaceGroup[] {
  const grouped = new Map<string, Agent[]>();
  for (const agent of agents) {
    const key = workspaceIdentity(agent);
    grouped.set(key, [...(grouped.get(key) || []), agent]);
  }

  const groups = [...grouped].map(([key, members]) => {
    const ordered = sortedAgents(members);
    const first = ordered[0];
    const tabs = new Map<string, Agent[]>();
    for (const agent of ordered) {
      const id = String(agent.tab_id || agent.pane_id);
      tabs.set(id, [...(tabs.get(id) || []), agent]);
    }
    const tabGroups = [...tabs].map(([id, tabAgents]): WorkspaceTab => ({
      id,
      label: tabName(tabAgents[0]) || displayName(tabAgents[0]),
      number: Number(tabAgents[0].tab_number) || Number.MAX_SAFE_INTEGER,
      agents: sortedAgents(tabAgents),
    })).sort((left, right) => left.number - right.number || left.label.localeCompare(right.label));
    const statuses = ordered.map(agentStatusGroup);
    return {
      key,
      relayId: first.relay_id,
      relayLabel: first.relay_label,
      workspaceId: String(first.workspace_id || ''),
      label: groupLabel(ordered),
      cwd: groupCwd(ordered),
      host: hostLabel(first),
      agents: ordered,
      tabs: tabGroups,
      attentionCount: statuses.filter((status) => status === 'blocked' || status === 'attention').length,
      workingCount: statuses.filter((status) => status === 'working').length,
      readyCount: statuses.filter((status) => status === 'ready' || status === 'done').length,
      lastActiveAt: Math.max(0, ...ordered.map(agentLastActiveAt)),
      lastActivitySeq: Math.max(0, ...ordered.map(agentActivitySeq)),
    };
  });

  return groups.sort((left, right) =>
    right.attentionCount - left.attentionCount
    || right.lastActiveAt - left.lastActiveAt
    || (left.relayId === right.relayId ? right.lastActivitySeq - left.lastActivitySeq : 0)
    || left.label.localeCompare(right.label, undefined, { sensitivity: 'base' })
    || left.host.localeCompare(right.host, undefined, { sensitivity: 'base' }));
}

export function workspaceMetadataSearchText(group: WorkspaceGroup): string {
  return [
    group.label,
    group.cwd,
    group.host,
    group.relayLabel,
    group.workspaceId,
  ].join(' ').toLocaleLowerCase();
}

export function workspaceSearchText(group: WorkspaceGroup): string {
  return [
    group.label,
    group.cwd,
    group.host,
    group.relayLabel,
    group.workspaceId,
    ...group.tabs.flatMap((tab) => [tab.label, ...tab.agents.flatMap((agent) => [
      displayName(agent),
      String(agent.agent || ''),
      String(agent.session || ''),
    ])]),
  ].join(' ').toLocaleLowerCase();
}

export function agentSearchText(agent: Agent): string {
  return [
    displayName(agent),
    String(agent.agent || ''),
    String(agent.project || ''),
    String(agent.cwd || ''),
    String(agent.session || ''),
    String(agent.tab_label || ''),
    String(agent.workspace_id || ''),
    hostLabel(agent),
    agent.relay_label,
  ].join(' ').toLocaleLowerCase();
}
