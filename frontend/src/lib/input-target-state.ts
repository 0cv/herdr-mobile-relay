import type { Agent, RelayConnectionView } from './types';

export function inputContextIdentity(agent: Agent): string {
  return JSON.stringify([
    agent.relay_id, agent.server_session_id, agent.raw_pane_id,
    agent.terminal_id, agent.generation, agent.agent_session_id, agent.agent, agent.cwd,
  ]);
}

export function freshAgentStatus(connection: RelayConnectionView | undefined): boolean {
  return connection?.status === 'connected' && connection.inventory?.state === 'ready' && !connection.inventory.stale;
}
