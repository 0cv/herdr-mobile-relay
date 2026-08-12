import { displayName, hostLabel } from './agents';
import type { Activity, Agent } from './types';

export interface DailyAgentSummary {
  key: string;
  label: string;
  host: string;
  workingMs: number;
  attention: number;
  completions: number;
  actions: number;
}

export interface DailyActivitySummary {
  since: number;
  workingMs: number;
  attention: number;
  completions: number;
  actions: number;
  relays: number;
  agents: DailyAgentSummary[];
}

interface MutableAgentSummary extends DailyAgentSummary {
  workingSince: number | null;
}

function timestampOf(activity: Activity): number {
  const numeric = Number(activity.timestamp);
  if (Number.isFinite(numeric) && numeric > 0) return numeric;
  const parsed = Date.parse(String(activity.timestamp || ''));
  return Number.isFinite(parsed) ? parsed : 0;
}

function activityKey(activity: Activity): string {
  return `${activity.relay_id}\u0000${activity.pane_id || ''}`;
}

function agentKey(agent: Agent): string {
  return `${agent.relay_id}\u0000${agent.raw_pane_id || agent.pane_id}`;
}

function stopsWorking(activity: Activity): boolean {
  return ['blocked', 'question', 'finished'].includes(String(activity.kind || '').toLocaleLowerCase());
}

function transitionEvent(activity: Activity): boolean {
  return ['working', 'blocked', 'question', 'finished'].includes(String(activity.kind || '').toLocaleLowerCase());
}

export function dailyActivitySummary(
  activities: Activity[],
  agents: Agent[],
  now = Date.now(),
): DailyActivitySummary {
  const since = now - 24 * 60 * 60 * 1000;
  const currentAgents = new Map(agents.map((agent) => [agentKey(agent), agent]));
  const summaries = new Map<string, MutableAgentSummary>();
  let attention = 0;
  let completions = 0;
  let actions = 0;

  const summaryFor = (activity: Activity): MutableAgentSummary => {
    const key = activityKey(activity);
    const current = currentAgents.get(key);
    const existing = summaries.get(key);
    if (existing) return existing;
    const summary: MutableAgentSummary = {
      key,
      label: activity.project || activity.agent || (current ? displayName(current) : 'Agent'),
      host: activity.host || activity.relay_label || (current ? hostLabel(current) : ''),
      workingMs: 0,
      workingSince: null,
      attention: 0,
      completions: 0,
      actions: 0,
    };
    summaries.set(key, summary);
    return summary;
  };

  const ordered = [...activities]
    .map((activity) => ({ activity, timestamp: timestampOf(activity) }))
    .filter(({ timestamp }) => timestamp > 0 && timestamp <= now)
    .sort((left, right) => left.timestamp - right.timestamp);

  for (const { activity, timestamp } of ordered) {
    const kind = String(activity.kind || '').toLocaleLowerCase();
    const summary = summaryFor(activity);
    if (kind === 'working') {
      if (summary.workingSince === null) summary.workingSince = Math.max(timestamp, since);
    } else if (stopsWorking(activity) && summary.workingSince !== null) {
      if (timestamp >= since) summary.workingMs += Math.max(0, timestamp - summary.workingSince);
      summary.workingSince = null;
    }
    if (timestamp < since) continue;
    if (kind === 'blocked' || kind === 'question') {
      attention += 1;
      summary.attention += 1;
    } else if (kind === 'finished') {
      completions += 1;
      summary.completions += 1;
    } else if (!transitionEvent(activity)) {
      actions += 1;
      summary.actions += 1;
    }
  }

  for (const summary of summaries.values()) {
    if (summary.workingSince === null) continue;
    const current = currentAgents.get(summary.key);
    if (current?.status === 'working') summary.workingMs += Math.max(0, now - summary.workingSince);
    summary.workingSince = null;
  }

  const agentSummaries = [...summaries.values()]
    .filter((summary) => summary.workingMs || summary.attention || summary.completions || summary.actions)
    .sort((left, right) => right.workingMs - left.workingMs
      || right.attention - left.attention
      || right.completions - left.completions
      || left.label.localeCompare(right.label, undefined, { sensitivity: 'base' }))
    .map(({ workingSince: _, ...summary }) => summary);
  const relayCount = new Set(agentSummaries.map((summary) => summary.key.split('\u0000', 1)[0])).size;

  return {
    since,
    workingMs: agentSummaries.reduce((total, summary) => total + summary.workingMs, 0),
    attention,
    completions,
    actions,
    relays: relayCount,
    agents: agentSummaries,
  };
}

export function formatWorkingDuration(milliseconds: number): string {
  const minutes = Math.max(0, Math.round(milliseconds / 60_000));
  if (milliseconds > 0 && minutes === 0) return '<1m';
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const remainder = minutes % 60;
  return remainder ? `${hours}h ${remainder}m` : `${hours}h`;
}
