import { get } from 'svelte/store';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  DEFAULT_AGENT_VIEW_KEY,
  PANE_AGENT_VIEW_OVERRIDES_KEY,
} from '$lib/config';
import { paneViewPreferenceKey } from '$lib/agent-view';
import {
  clearPaneAgentViewOverridesForRelay,
  defaultAgentView,
  paneAgentViewOverrides,
  readDefaultAgentView,
  readPaneAgentViewOverrides,
  setDefaultAgentView,
  setPaneAgentView,
} from '$lib/preferences';
import type { Agent } from '$lib/types';

function agent(relayId = 'fedora', rawPaneId = 'pane-1', terminalId = 'terminal-1'): Agent {
  return {
    relay_id: relayId,
    relay_label: relayId,
    raw_pane_id: rawPaneId,
    pane_id: `${relayId}::${rawPaneId}`,
    server_session_id: 'primary',
    terminal_id: terminalId,
    generation: 1,
    agent_session_id: 'session-1',
    agent: 'codex',
  };
}

function throwingStorage(): Pick<Storage, 'getItem'> {
  return {
    getItem() {
      throw new Error('storage unavailable');
    },
  };
}

describe('agent view preference storage', () => {
  beforeEach(() => {
    localStorage.clear();
    defaultAgentView.set('terminal');
    paneAgentViewOverrides.set({});
  });

  afterEach(() => {
    vi.restoreAllMocks();
    localStorage.clear();
    defaultAgentView.set('terminal');
    paneAgentViewOverrides.set({});
  });

  it('loads Terminal when the global preference is missing or invalid', () => {
    expect(readDefaultAgentView()).toBe('terminal');
    localStorage.setItem(DEFAULT_AGENT_VIEW_KEY, 'history');
    expect(readDefaultAgentView()).toBe('terminal');
    localStorage.setItem(DEFAULT_AGENT_VIEW_KEY, 'terminal');
    expect(readDefaultAgentView()).toBe('terminal');
    localStorage.setItem(DEFAULT_AGENT_VIEW_KEY, 'conversation');
    expect(readDefaultAgentView()).toBe('conversation');
  });

  it('handles global preference read failures without throwing', () => {
    expect(readDefaultAgentView(throwingStorage())).toBe('terminal');
  });

  it.each([
    null,
    '',
    'not-json',
    'null',
    '[]',
    '"primitive"',
    '1',
  ])('loads no overrides for malformed data %s', (raw) => {
    const storage = { getItem: () => raw } satisfies Pick<Storage, 'getItem'>;
    expect(readPaneAgentViewOverrides(storage)).toEqual({});
  });

  it('keeps valid override siblings while discarding invalid entries', () => {
    const validKey = paneViewPreferenceKey(agent())!;
    const otherKey = paneViewPreferenceKey(agent('mac', 'pane-2', 'terminal-2'))!;
    localStorage.setItem(PANE_AGENT_VIEW_OVERRIDES_KEY, JSON.stringify({
      [validKey]: 'conversation',
      [otherKey]: 'terminal',
      [JSON.stringify(['fedora', 'primary', 'pane-1'])]: 'terminal',
      [JSON.stringify(['fedora', 'primary', 'pane-1', 'terminal-1']) + ' ']: 'conversation',
      bad: 'conversation',
      [validKey + '-different']: 'history',
      [JSON.stringify(['fedora', 'primary', 'pane-3', 'terminal-3'])]: 'history',
    }));
    expect(readPaneAgentViewOverrides()).toEqual({
      [validKey]: 'conversation',
      [otherKey]: 'terminal',
    });
  });

  it('persists a global choice before publishing it', () => {
    expect(setDefaultAgentView('conversation')).toBe('saved');
    expect(localStorage.getItem(DEFAULT_AGENT_VIEW_KEY)).toBe('conversation');
    expect(get(defaultAgentView)).toBe('conversation');
  });

  it('saves only the requested pane and preserves siblings', () => {
    const first = agent();
    const second = agent('mac', 'pane-2', 'terminal-2');
    const firstKey = paneViewPreferenceKey(first)!;
    const secondKey = paneViewPreferenceKey(second)!;
    expect(setPaneAgentView(first, 'conversation')).toBe('saved');
    expect(setPaneAgentView(second, 'terminal')).toBe('saved');
    expect(get(paneAgentViewOverrides)).toEqual({ [firstKey]: 'conversation', [secondKey]: 'terminal' });
    expect(JSON.parse(localStorage.getItem(PANE_AGENT_VIEW_OVERRIDES_KEY)!)).toEqual({
      [firstKey]: 'conversation',
      [secondKey]: 'terminal',
    });
  });

  it('removes one override and removes the storage key when none remain', () => {
    const first = agent();
    const second = agent('mac', 'pane-2', 'terminal-2');
    const firstKey = paneViewPreferenceKey(first)!;
    const secondKey = paneViewPreferenceKey(second)!;
    setPaneAgentView(first, 'conversation');
    setPaneAgentView(second, 'terminal');
    expect(setPaneAgentView(first, null)).toBe('saved');
    expect(get(paneAgentViewOverrides)).toEqual({ [secondKey]: 'terminal' });
    expect(setPaneAgentView(second, null)).toBe('saved');
    expect(get(paneAgentViewOverrides)).toEqual({});
    expect(localStorage.getItem(PANE_AGENT_VIEW_OVERRIDES_KEY)).toBeNull();
    expect(firstKey).not.toBe(secondKey);
  });

  it('keeps an explicit choice when it equals the current global value', () => {
    const current = agent();
    const key = paneViewPreferenceKey(current)!;
    setDefaultAgentView('terminal');
    setPaneAgentView(current, 'terminal');
    setDefaultAgentView('conversation');
    expect(get(paneAgentViewOverrides)).toEqual({ [key]: 'terminal' });
    expect(JSON.parse(localStorage.getItem(PANE_AGENT_VIEW_OVERRIDES_KEY)!)).toEqual({ [key]: 'terminal' });
  });

  it('does not rewrite pane overrides when the global value changes', () => {
    const current = agent();
    setPaneAgentView(current, 'conversation');
    const saved = localStorage.getItem(PANE_AGENT_VIEW_OVERRIDES_KEY);
    setDefaultAgentView('conversation');
    expect(localStorage.getItem(PANE_AGENT_VIEW_OVERRIDES_KEY)).toBe(saved);
    expect(get(paneAgentViewOverrides)).toEqual({ [paneViewPreferenceKey(current)!]: 'conversation' });
  });

  it('reports save failures without publishing new values', () => {
    const current = agent();
    vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new Error('full');
    });
    expect(setDefaultAgentView('conversation')).toBe('unavailable');
    expect(get(defaultAgentView)).toBe('terminal');
    expect(setPaneAgentView(current, 'conversation')).toBe('unavailable');
    expect(get(paneAgentViewOverrides)).toEqual({});
  });

  it('reports removal failures without publishing new values', () => {
    const current = agent();
    setPaneAgentView(current, 'conversation');
    vi.spyOn(localStorage, 'removeItem').mockImplementation(() => {
      throw new Error('read only');
    });
    expect(setPaneAgentView(current, null)).toBe('unavailable');
    expect(get(paneAgentViewOverrides)).toEqual({ [paneViewPreferenceKey(current)!]: 'conversation' });
    expect(localStorage.getItem(PANE_AGENT_VIEW_OVERRIDES_KEY)).toContain('conversation');
  });

  it('refuses to create an override without stable identity', () => {
    const invalid = agent('fedora', 'pane-1', '');
    const setItem = vi.spyOn(localStorage, 'setItem');
    expect(setPaneAgentView(invalid, 'conversation')).toBe('invalid-target');
    expect(setItem).not.toHaveBeenCalledWith(PANE_AGENT_VIEW_OVERRIDES_KEY, expect.anything());
    expect(get(paneAgentViewOverrides)).toEqual({});
  });

  it('cleans only the explicitly removed relay', () => {
    const fedora = agent('fedora', 'pane-1', 'terminal-1');
    const otherFedoraPane = agent('fedora', 'pane-2', 'terminal-2');
    const mac = agent('mac', 'pane-1', 'terminal-1');
    setDefaultAgentView('conversation');
    setPaneAgentView(fedora, 'terminal');
    setPaneAgentView(otherFedoraPane, 'conversation');
    setPaneAgentView(mac, 'terminal');
    expect(clearPaneAgentViewOverridesForRelay('fedora')).toBe('saved');
    expect(get(defaultAgentView)).toBe('conversation');
    expect(get(paneAgentViewOverrides)).toEqual({
      [paneViewPreferenceKey(mac)!]: 'terminal',
    });
  });

  it('leaves the store unchanged when relay cleanup cannot persist', () => {
    const current = agent('fedora');
    setPaneAgentView(current, 'conversation');
    vi.spyOn(localStorage, 'removeItem').mockImplementation(() => {
      throw new Error('read only');
    });
    expect(clearPaneAgentViewOverridesForRelay('fedora')).toBe('unavailable');
    expect(get(paneAgentViewOverrides)).toEqual({ [paneViewPreferenceKey(current)!]: 'conversation' });
  });
});
