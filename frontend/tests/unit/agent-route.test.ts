import { get, writable } from 'svelte/store';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { targetRefForAgent, targetRefMatchesAgent } from '$lib/resource-id';
import { currentView, followInitialAgentSession, navigate, replaceView, stateFromLocation, viewUrl } from '$lib/router';
import type { Agent } from '$lib/types';

function agent(overrides: Partial<Agent> = {}): Agent {
  return {
    relay_id: 'relay', relay_label: 'Relay', pane_id: 'relay::pane-1', raw_pane_id: 'pane-1',
    server_session_id: 'primary', terminal_id: 'terminal-1', generation: 1, agent_session_id: '',
    agent: 'pi', status: 'idle', ...overrides,
  };
}

let stopFollowing = () => {};
afterEach(() => {
  stopFollowing();
  currentView.set({ view: 'agents' });
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('deferred setup navigation', () => {
  it.each([
    { standalone: false, userAgent: 'Mozilla/5.0 (iPhone)', deferred: true },
    { standalone: true, userAgent: 'Mozilla/5.0 (iPhone)', deferred: false },
    { standalone: false, userAgent: 'Mozilla/5.0 (X11; Linux x86_64)', deferred: false },
  ])('retains the link only when pairing is deferred: $standalone / $userAgent', ({ standalone, userAgent, deferred }) => {
    vi.stubGlobal('navigator', { standalone, userAgent, maxTouchPoints: 0 });
    vi.spyOn(window, 'scrollTo').mockImplementation(() => {});
    const hash = `#setup=${'a'.repeat(32)}&label=Invited&relay=${encodeURIComponent('wss://invited.example')}`;
    history.replaceState(null, '', `/pair?source=test${hash}`);
    navigate({ view: 'settings' });
    expect(get(currentView)).toEqual({ view: 'settings' });
    expect(location.hash).toBe(deferred ? hash : '#settings');
    replaceView({ view: 'agents' });
    expect(location.hash).toBe(deferred ? hash : '');
    history.replaceState(history.state, '', '/pair?source=test');
    navigate({ view: 'settings' });
    expect(location.hash).toBe('#settings');
  });
});

describe('notification deep links', () => {
  it('round-trips a URL-encoded pane and host through the router', () => {
    const target = { pane_id: 'workspace:tab/pane & 1', host: 'host name/é' };
    const hash = `#notify=${encodeURIComponent(JSON.stringify(target))}`;
    const state = stateFromLocation({ hash });
    expect(state).toEqual({
      view: 'notification',
      target: { ...target, action: '', index: null, total: null, notification_id: '' },
    });
    expect(viewUrl(state)).toBe(`#notify=${encodeURIComponent(JSON.stringify({
      ...target, action: '', index: null, total: null, notification_id: '',
    }))}`);
    expect(stateFromLocation({ hash: viewUrl(state) })).toEqual(state);
  });
});

describe('initial native session discovery navigation', () => {
  it.each(['terminal', 'history'] as const)('refreshes an open %s route without adding history or authorizing the old target', (view) => {
    const before = agent();
    const agents = writable([before]);
    const target = targetRefForAgent(before)!;
    replaceView({ view, paneId: before.pane_id, target });
    const push = vi.spyOn(history, 'pushState');
    stopFollowing = followInitialAgentSession(agents);
    const after = agent({ generation: 2, agent_session_id: 'native-session' });
    agents.set([after]);
    expect(get(currentView)).toEqual({ view, paneId: before.pane_id, target: targetRefForAgent(after) });
    expect(push).not.toHaveBeenCalled();
    expect(targetRefMatchesAgent(target, after)).toBe(false);
    expect(targetRefMatchesAgent(history.state.target, after)).toBe(true);
  });

  it.each([
    ['terminal replacement', { terminal_id: 'another-terminal' }],
    ['server replacement', { server_session_id: 'another-server' }],
    ['pane replacement', { raw_pane_id: 'another-pane' }],
    ['relay replacement', { relay_id: 'another-relay' }],
    ['agent replacement', { agent: 'codex' }],
    ['skipped generation', { generation: 3 }],
    ['unchanged generation', { generation: 1 }],
    ['missing session', { agent_session_id: '' }],
  ] satisfies [string, Partial<Agent>][])('does not follow %s', (_, override) => {
    const before = agent();
    const agents = writable([before]);
    const view = { view: 'terminal' as const, paneId: before.pane_id, target: targetRefForAgent(before)! };
    replaceView(view);
    stopFollowing = followInitialAgentSession(agents);
    agents.set([agent({ generation: 2, agent_session_id: 'native-session', ...override })]);
    expect(get(currentView)).toEqual(view);
  });

  it('does not follow a later native session change', () => {
    const before = agent();
    const agents = writable([before]);
    replaceView({ view: 'terminal', paneId: before.pane_id, target: targetRefForAgent(before)! });
    stopFollowing = followInitialAgentSession(agents);
    agents.set([agent({ generation: 2, agent_session_id: 'session-1' })]);
    const discoveredView = get(currentView);
    agents.set([agent({ generation: 3, agent_session_id: 'session-2' })]);
    expect(get(currentView)).toEqual(discoveredView);
  });

  it('does not revive a route whose pane disappeared', () => {
    const before = agent();
    const agents = writable([before]);
    const view = { view: 'terminal' as const, paneId: before.pane_id, target: targetRefForAgent(before)! };
    replaceView(view);
    stopFollowing = followInitialAgentSession(agents);
    agents.set([]);
    agents.set([agent({ generation: 2, agent_session_id: 'native-session' })]);
    expect(get(currentView)).toEqual(view);
  });

  it('retains only prior live route proof during revalidation, not authority', () => {
    const before = agent();
    const agents = writable([before]);
    const view = { view: 'terminal' as const, paneId: before.pane_id, target: targetRefForAgent(before)! };
    replaceView(view);
    let revalidating = true;
    stopFollowing = followInitialAgentSession(agents, () => revalidating);
    agents.set([]);
    expect(get(currentView)).toEqual(view);
    expect(get(agents)).toEqual([]);
    revalidating = false;
    const after = agent({ generation: 2, agent_session_id: 'native-session' });
    agents.set([after]);
    expect(get(currentView)).toEqual({ ...view, target: targetRefForAgent(after) });
    expect(targetRefMatchesAgent(view.target, after)).toBe(false);
  });

  it('clears retained route proof when fresh inventory confirms absence', () => {
    const before = agent();
    const agents = writable([before]);
    const view = { view: 'terminal' as const, paneId: before.pane_id, target: targetRefForAgent(before)! };
    replaceView(view);
    let revalidating = true;
    stopFollowing = followInitialAgentSession(agents, () => revalidating);
    agents.set([]);
    revalidating = false;
    agents.set([]);
    agents.set([agent({ generation: 2, agent_session_id: 'native-session' })]);
    expect(get(currentView)).toEqual(view);
  });

  it('rejects replacement identity even after authority withdrawal', () => {
    const before = agent();
    const agents = writable([before]);
    const view = { view: 'terminal' as const, paneId: before.pane_id, target: targetRefForAgent(before)! };
    replaceView(view);
    stopFollowing = followInitialAgentSession(agents, () => true);
    agents.set([]);
    agents.set([agent({ generation: 2, agent_session_id: 'native-session', terminal_id: 'replacement' })]);
    expect(get(currentView)).toEqual(view);
  });

  it('does not retarget a stale bookmark from its first snapshot', () => {
    const before = agent();
    const agents = writable<Agent[]>([]);
    const view = { view: 'terminal' as const, paneId: before.pane_id, target: targetRefForAgent(before)! };
    replaceView(view);
    stopFollowing = followInitialAgentSession(agents);
    agents.set([agent({ generation: 2, agent_session_id: 'native-session' })]);
    expect(get(currentView)).toEqual(view);
  });

  it('does not redirect after the user navigates away', () => {
    const before = agent();
    const agents = writable([before]);
    replaceView({ view: 'terminal', paneId: before.pane_id, target: targetRefForAgent(before)! });
    stopFollowing = followInitialAgentSession(agents);
    replaceView({ view: 'agents' });
    agents.set([agent({ generation: 2, agent_session_id: 'native-session' })]);
    expect(get(currentView)).toEqual({ view: 'agents' });
  });

  it('stops following when the subscription is disposed', () => {
    const before = agent();
    const agents = writable([before]);
    const view = { view: 'terminal' as const, paneId: before.pane_id, target: targetRefForAgent(before)! };
    replaceView(view);
    stopFollowing = followInitialAgentSession(agents);
    stopFollowing();
    agents.set([agent({ generation: 2, agent_session_id: 'native-session' })]);
    expect(get(currentView)).toEqual(view);
  });
});
