import { agentKind, agentStatusGroup, type AgentKind } from './agents';
import { SLASH_COMMAND_NAME } from './wire-normalizers';
import type { Agent, SlashCommand } from './types';

export function promptSlashCommand(text: string): string {
  const command = text.trimStart().split(/\s/, 1)[0];
  return SLASH_COMMAND_NAME.test(command) ? command : '';
}

export function matchSlashCommands(commands: SlashCommand[], query: string | null, recent: readonly string[] = []): SlashCommand[] {
  if (query === null) return [];
  const matches = commands.filter((entry) => !query || entry.command.slice(1).toLocaleLowerCase().startsWith(query));
  if (!recent.length) return matches;
  const rank = (entry: SlashCommand) => recent.indexOf(entry.command) + 1 || recent.length + 1;
  return matches.sort((a, b) => rank(a) - rank(b));
}

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
