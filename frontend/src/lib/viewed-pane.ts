import { RELAY_PROTOCOL_VERSION } from './protocol';
import { targetRefForAgent } from './resource-id';
import type { ViewState } from './router';
import { relayStore } from './store';
import type { Agent, RelayConnectionView } from './types';

export interface ViewedPaneState {
  view: ViewState;
  agent: Agent | null;
  locked: boolean;
  connections: Map<string, RelayConnectionView>;
}

export function createViewedPaneReporter() {
  let reportedRelayId = '';
  let reportedSignature = '';

  return function report({ view, agent, locked, connections }: ViewedPaneState): void {
    const visible = document.visibilityState === 'visible' && document.hasFocus();
    const target = view.view === 'terminal' && agent && !locked
      && agent.server_session_id === 'primary'
      && agent.terminal_id
      && Number.isSafeInteger(agent.generation)
      ? targetRefForAgent(agent)
      : null;
    const relayId = visible && target && agent
      && connections.get(agent.relay_id)?.status === 'connected'
      ? agent.relay_id
      : '';
    const signature = relayId && target
      ? `${relayId}:${target.pane_id}:${target.terminal_id}:${target.agent_session_id || ''}:${target.generation}`
      : '';
    if (signature === reportedSignature) return;
    if (reportedRelayId) {
      relayStore.sendRaw(reportedRelayId, {
        type: 'push_viewed_pane',
        protocol: RELAY_PROTOCOL_VERSION,
        visible: false,
        unlocked: !locked,
      });
    }
    reportedRelayId = relayId;
    reportedSignature = signature;
    if (relayId && target) {
      relayStore.sendRaw(relayId, {
        type: 'push_viewed_pane',
        protocol: RELAY_PROTOCOL_VERSION,
        visible: true,
        unlocked: true,
        target,
      });
    }
  };
}
