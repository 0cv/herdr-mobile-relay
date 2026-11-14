import { get } from 'svelte/store';
import { parsePushOpenTarget } from './protocol';
import { targetRefMatchesAgent } from './resource-id';
import { replaceView, currentView } from './router';
import type { ViewState } from './router';
import { relayStore } from './store';
import type { RelayConfig, RelayConnectionView } from './types';

const CONNECT_TIMEOUT_MS = 15_000;
const AGENT_LOOKUP_TIMEOUT_MS = 5_000;

export interface PushOpenState {
  view: ViewState;
  locked: boolean;
  relays: RelayConfig[];
  connections: Map<string, RelayConnectionView>;
}

export function createPushOpenRouter() {
  const opens = new Set<string>();
  const timeouts = new Map<string, ReturnType<typeof setTimeout>>();

  function showingPush(eventRef: string): boolean {
    const current = get(currentView);
    return current.view === 'push' && current.eventRef === eventRef;
  }

  async function resolve(eventRef: string, relayId: string): Promise<void> {
    try {
      const result = await relayStore.sendCommand(relayId, { type: 'push_open_ref', event_ref: eventRef });
      const target = parsePushOpenTarget(result.data?.target, relayId);
      if (!target) throw new Error('Invalid notification target');
      const findAgent = () => get(relayStore.agents).find(agent =>
        agent.relay_id === relayId && targetRefMatchesAgent(target, agent),
      );
      let agent = findAgent();
      const deadline = Date.now() + AGENT_LOOKUP_TIMEOUT_MS;
      while (!agent && Date.now() < deadline) {
        relayStore.requestAgents();
        await new Promise(resolve => setTimeout(resolve, 100));
        agent = findAgent();
      }
      if (!showingPush(eventRef)) return;
      if (!agent) {
        replaceView({ view: 'push_unavailable' });
        return;
      }
      replaceView({ view: 'terminal', paneId: agent.pane_id, target });
    } catch {
      if (showingPush(eventRef)) replaceView({ view: 'push_unavailable' });
    }
  }

  function route({ view, locked, relays, connections }: PushOpenState): void {
    if (view.view !== 'push' || locked) return;
    const { eventRef, deviceId } = view;
    const matchingRelays = relays.filter((relay) =>
      relayStore.deviceCredential(relay.id)?.deviceId === deviceId,
    );
    if (matchingRelays.length !== 1) {
      replaceView({ view: 'push_unavailable' });
      return;
    }
    const relayId = matchingRelays[0].id;
    const connection = connections.get(relayId);
    if (connection?.status !== 'connected' || connection.inventory.state !== 'ready') {
      if (!timeouts.has(eventRef)) {
        timeouts.set(eventRef, setTimeout(() => {
          timeouts.delete(eventRef);
          if (showingPush(eventRef)) replaceView({ view: 'push_unavailable' });
        }, CONNECT_TIMEOUT_MS));
      }
      return;
    }
    const timeout = timeouts.get(eventRef);
    if (timeout) clearTimeout(timeout);
    timeouts.delete(eventRef);
    if (!connection.capabilities.includes('typed_push')) {
      replaceView({ view: 'push_unavailable' });
      return;
    }
    if (opens.has(eventRef)) return;
    opens.add(eventRef);
    void resolve(eventRef, relayId).finally(() => opens.delete(eventRef));
  }

  function stop(): void {
    for (const timeout of timeouts.values()) clearTimeout(timeout);
    timeouts.clear();
  }

  return { route, stop };
}
