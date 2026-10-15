import { describe, expect, it } from 'vitest';
import { retainPaneHistory } from '$lib/terminal-history';
import type { FrontendTargetRef, TerminalFrame } from '$lib/types';

const target: FrontendTargetRef = { relay_id: 'relay', server_session_id: 'primary', pane_id: 'pane', terminal_id: 'terminal', generation: 1 };
const history: TerminalFrame = { paneId: 'relay::pane', content: 'Prior history', format: 'ansi' };

describe('retained local terminal history', () => {
  it('keeps one genuine snapshot rather than concatenating redraws', () => {
    const prior = retainPaneHistory(undefined, target, history);
    let retained = prior;
    for (let i = 0; i < 10; i++) retained = retainPaneHistory(retained, target, { ...history, content: `Redraw ${i}`, viewportOnly: true });
    expect(retained).toBe(prior);
    expect(retained?.snapshot?.content).toBe('Prior history');
  });
  it('retains unknown ordinary history but does not replace it with uncertain redraws after fullscreen', () => {
    const unknown: TerminalFrame = { ...history, scrolling: { selection: 'auto', renderer: 'unknown', wheel: 'unknown', source: 'unknown', route: 'local', presentation: 'history', available: false, reason: 'runtime_unavailable', rows: 0, columns: 0, geometry_epoch: 0, revision: 'a'.repeat(64) } };
    const prior = retainPaneHistory(undefined, target, unknown);
    expect(retainPaneHistory(prior, target, { ...unknown, content: 'Ambiguous mode-transition redraw' })).toBe(prior);
    const live = { ...unknown, content: 'Live redraw', scrolling: { ...unknown.scrolling!, renderer: 'fullscreen' as const, presentation: 'live_screen' as const } };
    const retained = retainPaneHistory(prior, target, live);
    expect(retained?.snapshot?.content).toBe('Prior history');
    expect(retainPaneHistory(retained, target, { ...unknown, content: 'Uncertain redraw', viewportOnly: true })).toBe(retained);
    expect(retainPaneHistory(retained, target, { ...unknown, content: 'Uncertain redraw' })).toBe(retained);
    const regular = { ...unknown, content: 'New ordinary history', scrolling: { ...unknown.scrolling!, renderer: 'regular' as const } };
    const resumed = retainPaneHistory(retained, target, regular);
    expect(resumed?.snapshot?.content).toBe('New ordinary history');
    expect(retainPaneHistory(resumed, target, unknown)?.snapshot?.content).toBe('New ordinary history');
    expect(retainPaneHistory(retainPaneHistory(undefined, target, live), target, unknown)?.snapshot).toBeUndefined();
  });
  it.each(['viewportOnly', 'resizeSettling', 'noEcho'] as const)('does not retain %s as genuine history', field => {
    expect(retainPaneHistory(undefined, target, { ...history, [field]: true })).toBeUndefined();
  });
  it('preserves genuine same-terminal history across agent-generation changes without carrying authority', () => {
    const prior = retainPaneHistory(undefined, target, history);
    const retained = retainPaneHistory(prior, { ...target, generation: 2, agent_session_id: 'new-session' }, { ...history, content: 'Live screen', viewportOnly: true });
    expect(retained).toBe(prior);
    expect(retained?.snapshot).toEqual({ content: 'Prior history', format: 'ansi', truncated: undefined });
  });
  it.each(['terminal', 'server', 'missing'])('does not carry history across %s identity replacement', kind => {
    const prior = retainPaneHistory(undefined, target, history);
    const next = kind === 'missing' ? null : { ...target, terminal_id: kind === 'terminal' ? 'replacement' : target.terminal_id, generation: 1, server_session_id: kind === 'server' ? 'replacement' : target.server_session_id };
    expect(retainPaneHistory(prior, next, { ...history, content: 'Live screen', viewportOnly: true })).toBeUndefined();
  });
});
