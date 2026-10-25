import { onMount, untrack } from 'svelte';
import { fromStore } from 'svelte/store';
import { targetRefForAgent, targetRefMatchesAgent } from './resource-id';
import { promptDraftTarget } from './prompt-drafts';
import { inputContextIdentity } from './input-target-state';
import { relayStore } from './store';
import { securityState } from './security';
import type { Agent, TerminalFrame } from './types';

export function terminalDraftState(agent: () => Agent, frame: () => TerminalFrame | undefined) {
  const connections = fromStore(relayStore.connections);
  const security = fromStore(securityState);
  let visible = $state(document.visibilityState !== 'hidden');
  let epoch = $state.raw<{ identity: string; startedAt: number; initialFrame?: TerminalFrame }>();
  let confirmedEpoch = $state.raw<typeof epoch>();
  let expiredFrame = $state.raw<TerminalFrame>();
  const target = $derived(targetRefForAgent(agent()));
  const identity = $derived(target ? inputContextIdentity(agent()) : null);
  const status = $derived(connections.current.get(agent().relay_id)?.status);
  const locked = $derived(security.current.locked);

  $effect(() => {
    const target = identity;
    const active = visible && !locked && status === 'connected';
    epoch = active && target ? { identity: target, startedAt: performance.now(), initialFrame: untrack(frame) } : undefined;
  });

  onMount(() => {
    const changed = () => {
      epoch = undefined;
      visible = document.visibilityState !== 'hidden';
      if (identity && visible && !security.current.locked && connections.current.get(agent().relay_id)?.status === 'connected') {
        epoch = { identity, startedAt: performance.now(), initialFrame: frame() };
      }
    };
    document.addEventListener('visibilitychange', changed);
    return () => document.removeEventListener('visibilitychange', changed);
  });

  $effect(() => {
    const current = frame();
    const receivedAt = current?.receivedAt;
    if (!current || !Number.isFinite(receivedAt)) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const expire = () => {
      const remaining = receivedAt! + 3_000 - performance.now();
      if (remaining > 3_000) return;
      if (remaining > 0) timer = setTimeout(expire, remaining);
      else expiredFrame = current;
    };
    expire();
    return () => clearTimeout(timer);
  });

  $effect(() => {
    const current = frame();
    if (Number.isFinite(current?.readStartedAt) && ready(current?.inputTarget)) confirmedEpoch = epoch;
  });

  function ready(inputTarget: TerminalFrame['target']) {
    const current = frame();
    const age = performance.now() - (current?.receivedAt ?? NaN);
    return Boolean(target && visible && !locked && status === 'connected' && epoch && epoch.identity === identity
      && current && current !== epoch.initialFrame && current !== expiredFrame && current.paneId === agent().pane_id
      && inputTarget && targetRefMatchesAgent(inputTarget, agent()) && Number.isFinite(current.receivedAt)
      && current.receivedAt! >= epoch.startedAt && age >= 0 && age < 3_000
      && (current.readStartedAt === undefined || (Number.isFinite(current.readStartedAt) && current.readStartedAt >= epoch.startedAt)));
  }

  return {
    get ready() {
      return ready(frame()?.inputTarget) && (Number.isFinite(frame()?.readStartedAt) || confirmedEpoch === epoch);
    },
    get secretReady() {
      return ready(frame()?.target) && ready(frame()?.inputTarget) && relayStore.secretInputReady(agent(), frame());
    },
    get allowed() {
      return ready(frame()?.target) && Boolean(promptDraftTarget(target) && promptDraftTarget(frame()?.target) && frame()?.noEcho === false);
    },
  };
}
