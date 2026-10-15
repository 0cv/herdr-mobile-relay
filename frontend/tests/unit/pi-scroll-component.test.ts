import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import TerminalView from '../../src/components/TerminalView.svelte';
import { relayStore } from '$lib/store';
import type { Agent, ScrollState, TerminalFrame } from '$lib/types';

const agent: Agent = { relay_id: 'fixture', relay_label: 'Fixture', raw_pane_id: 'pane', pane_id: 'fixture::pane', agent: 'pi', status: 'idle', server_session_id: 'primary', terminal_id: 'terminal', generation: 1, agent_session_id: 'session' };
const scrolling: ScrollState = { selection: 'auto', renderer: 'fullscreen', wheel: 'sgr_cell', source: 'pi_runtime', route: 'application', presentation: 'live_screen', available: true, reason: 'pi_fullscreen', rows: 30, columns: 80, geometry_epoch: 1, revision: 'a'.repeat(64) };
const frame: TerminalFrame = { paneId: agent.pane_id, content: 'Fixture screen', format: 'ansi', scrolling };
const props = { agent, allAgents: [agent], frame, responding: new Set<string>() };
type Connection = Parameters<typeof relayStore.connections.set>[0] extends Map<string, infer Value> ? Value : never;

beforeEach(() => {
  vi.spyOn(relayStore, 'readPane').mockImplementation(() => {});
  vi.spyOn(relayStore, 'watchPane').mockImplementation(() => {});
  vi.spyOn(relayStore, 'unwatchPane').mockImplementation(() => {});
  relayStore.connections.set(new Map([['fixture', { status: 'connected', capabilities: [], relay: { id: 'fixture', label: 'Fixture', url: 'ws://fixture.invalid/ws', token: '' } } as unknown as Connection]]));
});
afterEach(() => { cleanup(); relayStore.connections.set(new Map()); vi.restoreAllMocks(); });

describe('Pi scroll component queue', () => {
  it('keeps the last pending click across identical state objects and successful confirmation', async () => {
    let confirm!: () => void;
    const first = new Promise<void>(resolve => { confirm = resolve; });
    const send = vi.spyOn(relayStore, 'scrollAgent').mockImplementationOnce(() => first).mockResolvedValue(undefined);
    const view = render(TerminalView, props);
    const up = view.getByRole('button', { name: 'Scroll agent up' });
    const down = view.getByRole('button', { name: 'Scroll agent down' });
    await fireEvent.click(up); await fireEvent.click(down); await fireEvent.click(up);
    expect(send).toHaveBeenCalledTimes(1);
    await view.rerender({ ...props, frame: { ...frame, content: 'Updated screen', scrolling: { ...scrolling } } });
    await tick(); confirm();
    await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
    expect(send.mock.calls.map(call => call[1])).toEqual(['up', 'up']);
  });

  it('cancels pending intent on revision change', async () => {
    let confirm!: () => void;
    const send = vi.spyOn(relayStore, 'scrollAgent').mockImplementation(() => new Promise<void>(resolve => { confirm = resolve; }));
    const view = render(TerminalView, props);
    await fireEvent.click(view.getByRole('button', { name: 'Scroll agent up' }));
    await fireEvent.click(view.getByRole('button', { name: 'Scroll agent down' }));
    await view.rerender({ ...props, frame: { ...frame, scrolling: { ...scrolling, revision: 'b'.repeat(64) } } });
    await tick(); confirm(); await tick();
    expect(send).toHaveBeenCalledTimes(1);
  });

  it('runs a fresh click after revision cancellation while the prior request drains', async () => {
    let confirm!: () => void;
    const first = new Promise<void>(resolve => { confirm = resolve; });
    const send = vi.spyOn(relayStore, 'scrollAgent').mockImplementationOnce(() => first).mockResolvedValue(undefined);
    const view = render(TerminalView, props);
    await fireEvent.click(view.getByRole('button', { name: 'Scroll agent up' }));
    await fireEvent.click(view.getByRole('button', { name: 'Scroll agent down' }));
    const revision = 'b'.repeat(64);
    await view.rerender({ ...props, frame: { ...frame, scrolling: { ...scrolling, revision } } });
    await tick();
    await fireEvent.click(view.getByRole('button', { name: 'Scroll agent up' }));
    expect(send).toHaveBeenCalledTimes(1);
    confirm();
    await waitFor(() => expect(send).toHaveBeenCalledTimes(2));
    expect(send.mock.calls.map(call => [call[1], call[2]])).toEqual([['up', scrolling.revision], ['up', revision]]);
  });

  it.each(['regular', 'unknown'] as const)('displays prior %s history separately and returns to the latest live screen', async renderer => {
    const prior = { paneId: agent.pane_id, content: 'Genuine prior history', format: 'ansi', scrolling: { ...scrolling, renderer, source: renderer === 'unknown' ? 'unknown' as const : 'pi_runtime' as const, presentation: 'history' as const, available: false, route: 'local' as const } };
    const view = render(TerminalView, { ...props, frame: prior });
    const output = view.getByRole('log', { name: 'Agent terminal output' });
    await waitFor(() => expect(output.textContent).toContain('Genuine prior history'));
    const live = { ...frame, content: 'Live screen one', localHistory: { content: prior.content, format: prior.format } };
    await view.rerender({ ...props, frame: live });
    await waitFor(() => expect(output.textContent).toContain('Live screen one'));
    expect(output.textContent).not.toContain('Genuine prior history');
    const local = { ...live, content: 'Live screen two', scrolling: { ...scrolling, selection: 'local' as const, route: 'local' as const, available: false, reason: 'local_view' } };
    await view.rerender({ ...props, frame: local });
    await waitFor(() => expect(output.textContent).toContain('Genuine prior history'));
    expect(output.textContent).not.toContain('Live screen');
    expect(view.getByText('Prior local history · retained snapshot')).toBeTruthy();
    expect((view.getByRole('button', { name: 'Scroll agent up' }) as HTMLButtonElement).disabled).toBe(true);
    await view.rerender({ ...props, frame: { ...live, content: 'Latest live screen' } });
    await waitFor(() => expect(output.textContent).toContain('Latest live screen'));
    expect(output.textContent).not.toContain('Genuine prior history');
  });

  it.each(['reader', 'disconnected'])('disables mutations for %s', async (mode) => {
    const send = vi.spyOn(relayStore, 'scrollAgent');
    const select = vi.spyOn(relayStore, 'setScrollSelection');
    if (mode === 'disconnected') relayStore.connections.set(new Map());
    const view = render(TerminalView, { ...props, readOnly: mode === 'reader' });
    const up = view.getByRole('button', { name: 'Scroll agent up' }) as HTMLButtonElement;
    const down = view.getByRole('button', { name: 'Scroll agent down' }) as HTMLButtonElement;
    const selection = view.getByLabelText('Pi scroll selection') as HTMLSelectElement;
    expect(up.disabled && down.disabled && selection.disabled).toBe(true);
    await fireEvent.click(up); await fireEvent.change(selection, { target: { value: 'local' } });
    expect(send).not.toHaveBeenCalled(); expect(select).not.toHaveBeenCalled();
  });
});
