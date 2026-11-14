import { createSubscriber } from 'svelte/reactivity';
import { fromStore } from 'svelte/store';
import { relayStore } from './store';
import { securityState } from './security';
import type { Agent } from './types';

const PANE_POLL_MS = 2_000;

const security = fromStore(securityState);
const watchVisibility = createSubscriber((update) => {
  document.addEventListener('visibilitychange', update);
  return () => document.removeEventListener('visibilitychange', update);
});

// A locked app shows a pane to nobody who has verified, so it counts as hidden.
export const paneActive = {
  get current() {
    watchVisibility();
    return document.visibilityState !== 'hidden' && !security.current.locked;
  },
};

// Call inside untrack so the effect does not depend on every field of the agent.
export function pollPane(agent: Agent, { lines, forceRepeats = true }: { lines?: number; forceRepeats?: boolean } = {}): () => void {
  const target = { ...agent };
  relayStore.readPane(target, true, lines);
  const timer = setInterval(() => relayStore.readPane(target, forceRepeats, lines), PANE_POLL_MS);
  return () => clearInterval(timer);
}
