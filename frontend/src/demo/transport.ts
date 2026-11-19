import type { RelayTransport, TransportHandlers } from '../lib/transports/types';
import type { RelayConfig } from '../lib/types';
import { createDemoResponder } from './responder';
import { demoEpochKey } from './scene';

const responders = new Map<string, ReturnType<typeof createDemoResponder>>();

export function createRelayTransport(relay: RelayConfig, handlers: TransportHandlers): RelayTransport {
  const epoch = Number(sessionStorage.getItem(demoEpochKey));
  if (!epoch) throw new Error('Missing demo visit time.');
  const responder = responders.get(relay.id) ?? createDemoResponder(relay.id, epoch);
  responders.set(relay.id, responder);
  let state: 'idle' | 'connecting' | 'connected' | 'closed' = 'idle';

  return {
    kind: 'websocket',
    connect() {
      if (state !== 'idle') return;
      state = 'connecting';
      queueMicrotask(() => {
        if (state !== 'connecting') return;
        state = 'connected';
        handlers.onStatus('connected', { path: 'websocket' });
        for (const message of responder.initialMessages()) {
          if (state !== 'connected') break;
          handlers.onMessage(message);
        }
      });
    },
    send(message) {
      if (state !== 'connected') return false;
      const command = structuredClone(message);
      queueMicrotask(() => {
        if (state !== 'connected') return;
        for (const reply of responder.respond(command)) {
          if (state !== 'connected') break;
          handlers.onMessage(reply);
        }
      });
      return true;
    },
    close() {
      state = 'closed';
    },
  };
}
