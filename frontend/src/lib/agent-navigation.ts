import { get } from 'svelte/store';
import { agentOpeningView } from './agent-view';
import { defaultAgentView, paneAgentViewOverrides } from './preferences';
import { relayStore } from './store';
import type { Agent } from './types';

export function agentOpeningRoute(agent: Agent) {
  return agentOpeningView(
    agent,
    get(relayStore.connections).get(agent.relay_id),
    get(defaultAgentView),
    get(paneAgentViewOverrides),
  );
}
