import type { ViewState } from './router';
import { isResourceId, targetRefForAgent } from './resource-id';
import type { Agent, RelayConnectionView } from './types';
import type { AgentView } from './config';

export type PaneAgentViewOverrides = Readonly<Record<string, AgentView>>;
export type PaneViewPreferenceIdentity = [string, string, string, string];
export type AgentOpeningRoute = Extract<ViewState, { view: 'terminal' | 'history' }>;
export type AgentViewConnection = Pick<RelayConnectionView, 'status' | 'inventory' | 'capabilities'>;

export function isAgentView(value: unknown): value is AgentView {
  return value === 'terminal' || value === 'conversation';
}

export function paneViewPreferenceKey(agent: Partial<Agent>): string | null {
  const identity = [agent.relay_id, agent.server_session_id, agent.raw_pane_id, agent.terminal_id];
  if (!identity.every(isResourceId)) return null;
  return JSON.stringify(identity as PaneViewPreferenceIdentity);
}

export function parsePaneViewPreferenceKey(value: unknown): PaneViewPreferenceIdentity | null {
  if (typeof value !== 'string') return null;
  try {
    const parsed: unknown = JSON.parse(value);
    if (!Array.isArray(parsed) || parsed.length !== 4 || !parsed.every(isResourceId)) return null;
    const identity = parsed as PaneViewPreferenceIdentity;
    return JSON.stringify(identity) === value ? identity : null;
  } catch {
    return null;
  }
}

export function effectiveAgentView(
  agent: Agent,
  defaultView: AgentView,
  overrides: PaneAgentViewOverrides,
): AgentView {
  const inherited = isAgentView(defaultView) ? defaultView : 'terminal';
  const key = paneViewPreferenceKey(agent);
  if (!key || !Object.prototype.hasOwnProperty.call(overrides, key)) return inherited;
  return isAgentView(overrides[key]) ? overrides[key] : inherited;
}

export function hasConversationHistory(
  agent: Agent | null,
  connection: Pick<RelayConnectionView, 'capabilities'> | null | undefined,
): boolean {
  return Boolean(agent?.conversation_history_available === true && connection?.capabilities?.includes('conversation_history'));
}

export function agentOpeningView(
  agent: Agent,
  connection: AgentViewConnection | null | undefined,
  defaultView: AgentView,
  overrides: PaneAgentViewOverrides,
): AgentOpeningRoute {
  const target = targetRefForAgent(agent) || undefined;
  const requested = effectiveAgentView(agent, defaultView, overrides);
  if (
    requested === 'conversation'
    && connection?.status === 'connected'
    && connection.inventory?.state === 'ready'
    && hasConversationHistory(agent, connection)
    && target
  ) {
    return {
      view: 'history',
      paneId: agent.pane_id,
      target,
      fallbackToTerminalOnInitialUnavailable: true,
    };
  }
  return { view: 'terminal', paneId: agent.pane_id, target };
}
