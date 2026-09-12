import { describe, expect, it } from 'vitest';
import {
  agentOpeningView,
  effectiveAgentView,
  hasConversationHistory,
  isAgentView,
  paneViewPreferenceKey,
  parsePaneViewPreferenceKey,
} from '$lib/agent-view';
import type { Agent, RelayConnectionView } from '$lib/types';

type Connection = Pick<RelayConnectionView, 'status' | 'inventory' | 'capabilities'>;

function agent(overrides: Partial<Agent> = {}): Agent {
  return {
    relay_id: 'fedora',
    relay_label: 'Fedora',
    raw_pane_id: 'pane-1',
    pane_id: 'fedora::pane-1',
    server_session_id: 'primary',
    terminal_id: 'terminal-1',
    generation: 1,
    agent_session_id: 'session-1',
    conversation_history_available: true,
    agent: 'codex',
    ...overrides,
  };
}

function connection(overrides: Partial<Connection> = {}): Connection {
  return {
    status: 'connected',
    inventory: {
      state: 'ready',
      errorCode: '',
      message: '',
      lastAttemptAt: 1,
      lastSuccessAt: 1,
      stale: false,
    },
    capabilities: ['conversation_history'],
    ...overrides,
  };
}

describe('agent view policy', () => {
  it('accepts only the two opening views', () => {
    expect(isAgentView('terminal')).toBe(true);
    expect(isAgentView('conversation')).toBe(true);
    expect(isAgentView('history')).toBe(false);
    expect(isAgentView('default')).toBe(false);
    expect(isAgentView(null)).toBe(false);
  });

  it('uses a canonical four-field preference identity', () => {
    const original = agent();
    const key = paneViewPreferenceKey(original);
    expect(key).toBe(JSON.stringify(['fedora', 'primary', 'pane-1', 'terminal-1']));
    expect(parsePaneViewPreferenceKey(key)).toEqual(['fedora', 'primary', 'pane-1', 'terminal-1']);

    for (const field of ['relay_id', 'server_session_id', 'raw_pane_id', 'terminal_id'] as const) {
      const changed = agent({ [field]: `${original[field]}-changed` });
      expect(paneViewPreferenceKey(changed)).not.toBe(key);
    }
    expect(paneViewPreferenceKey(agent({ generation: 2, agent_session_id: 'session-2' }))).toBe(key);
    expect(paneViewPreferenceKey(agent({ relay_label: 'Renamed', session: 'new label', tab_label: 'new tab', cwd: '/other', project: 'other', status: 'done', agent: 'claude' }))).toBe(key);
  });

  it('rejects missing identities and malformed noncanonical keys', () => {
    expect(paneViewPreferenceKey(agent({ terminal_id: '' }))).toBeNull();
    expect(paneViewPreferenceKey(agent({ raw_pane_id: 'pane with spaces' }))).toBeNull();
    expect(parsePaneViewPreferenceKey(JSON.stringify(['fedora', 'primary', 'pane-1']))).toBeNull();
    expect(parsePaneViewPreferenceKey(JSON.stringify(['fedora', 'primary', 'pane-1', 'terminal-1', 'extra']))).toBeNull();
    expect(parsePaneViewPreferenceKey(JSON.stringify(['fedora', 'primary', 'pane-1', 'terminal-1']).replace('fedora', '"fedora"'))).toBeNull();
    expect(parsePaneViewPreferenceKey(JSON.stringify(['fedora', 'primary', 'pane-1', 'terminal-1']) + ' ')).toBeNull();
    expect(parsePaneViewPreferenceKey(JSON.stringify(['fedora', 'primary', 'pane with spaces', 'terminal-1']))).toBeNull();
    expect(parsePaneViewPreferenceKey('not-json')).toBeNull();
    expect(parsePaneViewPreferenceKey(null)).toBeNull();
  });

  it('resolves pane overrides before the global default', () => {
    const first = agent();
    const second = agent({ raw_pane_id: 'pane-2', pane_id: 'fedora::pane-2', terminal_id: 'terminal-2' });
    const firstKey = paneViewPreferenceKey(first)!;
    expect(effectiveAgentView(first, 'terminal', {})).toBe('terminal');
    expect(effectiveAgentView(first, 'conversation', {})).toBe('conversation');
    expect(effectiveAgentView(first, 'terminal', { [firstKey]: 'conversation' })).toBe('conversation');
    expect(effectiveAgentView(first, 'conversation', { [firstKey]: 'terminal' })).toBe('terminal');
    expect(effectiveAgentView(second, 'conversation', { [firstKey]: 'terminal' })).toBe('conversation');
    expect(effectiveAgentView(first, 'conversation', { [firstKey]: 'history' as never })).toBe('conversation');
  });

  it('keeps manual history eligibility separate from automatic readiness', () => {
    expect(hasConversationHistory(agent(), { capabilities: ['conversation_history'] })).toBe(true);
    expect(hasConversationHistory(agent({ conversation_history_available: false }), { capabilities: ['conversation_history'] })).toBe(false);
    expect(hasConversationHistory(agent(), { capabilities: [] })).toBe(false);
    expect(hasConversationHistory(null, { capabilities: ['conversation_history'] })).toBe(false);
    expect(hasConversationHistory(agent(), null)).toBe(false);
  });

  it.each([
    ['terminal', undefined, true, 'terminal'],
    ['conversation', undefined, true, 'history'],
    ['terminal', 'conversation', true, 'history'],
    ['conversation', 'terminal', true, 'terminal'],
    ['conversation', undefined, false, 'terminal'],
    ['terminal', 'conversation', false, 'terminal'],
  ] as const)('applies preference row %s / %s with transcript %s', (defaultView, override, transcript, expected) => {
    const current = agent({ conversation_history_available: transcript });
    const key = paneViewPreferenceKey(current)!;
    const route = agentOpeningView(
      current,
      connection(),
      defaultView,
      override ? { [key]: override } : {},
    );
    if (expected === 'history') {
      expect(route).toMatchObject({
        view: 'history',
        paneId: current.pane_id,
        target: {
          relay_id: 'fedora',
          server_session_id: 'primary',
          pane_id: 'pane-1',
          terminal_id: 'terminal-1',
          generation: 1,
          agent_session_id: 'session-1',
        },
        fallbackToTerminalOnInitialUnavailable: true,
      });
    } else {
      expect(route).toEqual(expect.objectContaining({ view: 'terminal', paneId: current.pane_id }));
      expect(route).not.toHaveProperty('fallbackToTerminalOnInitialUnavailable');
    }
  });

  it('requires connected ready inventory, metadata, and exact routing for automatic history', () => {
    const current = agent();
    const invalidCases: Connection[] = [
      connection({ status: 'connecting' }),
      connection({ status: 'disconnected' }),
      connection({ inventory: { ...connection().inventory, state: 'starting' } }),
      connection({ inventory: { ...connection().inventory, state: 'error' } }),
      connection({ capabilities: [] }),
    ];
    for (const currentConnection of invalidCases) {
      expect(agentOpeningView(current, currentConnection, 'conversation', {})).toMatchObject({ view: 'terminal' });
    }
    expect(agentOpeningView(agent({ conversation_history_available: false }), connection(), 'conversation', {})).toMatchObject({ view: 'terminal' });
    expect(agentOpeningView(agent({ server_session_id: '' }), connection(), 'conversation', {})).toMatchObject({ view: 'terminal' });
    expect(agentOpeningView(agent({ agent: 'opencode', conversation_history_available: undefined }), connection(), 'conversation', {})).toMatchObject({ view: 'terminal' });
    expect(agentOpeningView(agent({ agent: 'unknown-looking-harness' }), connection(), 'conversation', {})).toMatchObject({ view: 'history' });
  });

  it('preserves exact targets for terminal openings and marks only automatic history', () => {
    const current = agent();
    const terminal = agentOpeningView(current, connection(), 'terminal', {});
    expect(terminal).toEqual({
      view: 'terminal',
      paneId: current.pane_id,
      target: {
        relay_id: 'fedora',
        server_session_id: 'primary',
        pane_id: 'pane-1',
        terminal_id: 'terminal-1',
        generation: 1,
        agent_session_id: 'session-1',
      },
    });
    const history = agentOpeningView(current, connection(), 'conversation', {});
    expect(history).toHaveProperty('fallbackToTerminalOnInitialUnavailable', true);
  });
});
