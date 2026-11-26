import { agentKind, agentStatusGroup, type AgentKind } from './agents';
import type { Agent } from './types';

const steeringKinds = new Set<AgentKind>(['claude', 'codex', 'pi', 'omp', 'opencode']);

export function busySendHint(agent: Agent, agentName: string) {
  const kind = agentKind(agent.agent);
  if (!kind || !steeringKinds.has(kind) || agentStatusGroup(agent) !== 'working') return;
  return {
    placeholder: 'Adds to task',
    description: `${agentName} is working. Send adds this to its current task.`,
    sentMessage: `Sent. ${agentName} will read it during its current task.`,
  };
}

export interface ComposerSubmitAdapter {
  filter: boolean;
  validate?: (text: string) => string;
  send: (target: Agent, text: string) => Promise<unknown>;
  sentMessage: string;
  failureMessage: string;
}

export function dispatchFlag(error: unknown, flag: string): boolean {
  if (!error || typeof error !== 'object' || !('data' in error)) return false;
  const data = error.data;
  return Boolean(data && typeof data === 'object' && flag in data && (data as Record<string, unknown>)[flag] === true);
}
