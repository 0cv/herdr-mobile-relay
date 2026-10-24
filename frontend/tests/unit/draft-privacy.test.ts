import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import TerminalView from '$components/TerminalView.svelte';
import ConversationHistory from '$components/ConversationHistory.svelte';
import PromptComposer from '$components/PromptComposer.svelte';
import { clearPromptDraft, loadPromptDraft, savePromptDraft } from '$lib/prompt-drafts';
import { reloadRisk } from '$lib/reload-guard';
import { targetRefForAgent } from '$lib/resource-id';
import { relayStore } from '$lib/store';
import type { Agent, TerminalFrame } from '$lib/types';

const agent: Agent = {
  relay_id: 'draft-privacy', relay_label: 'Synthetic', raw_pane_id: 'pane', pane_id: 'draft-privacy::pane',
  server_session_id: 'server', terminal_id: 'terminal', generation: 1, agent_session_id: 'session', agent: 'codex', status: 'idle',
};
const secret = 'SYNTHETICSECRET-E24';
let now = 10_000;
const frame = (target = agent, noEcho = false, receivedAt = now): TerminalFrame => ({
  paneId: target.pane_id, target: targetRefForAgent(target), content: 'Synthetic terminal', format: 'plain', noEcho, receivedAt,
});
const connect = (status = 'connected') => relayStore.connections.set(new Map([[agent.relay_id, { status, capabilities: ['secret_input'] }]] as never));

beforeEach(() => {
  vi.useFakeTimers();
  now = 10_000;
  vi.spyOn(performance, 'now').mockImplementation(() => now);
  localStorage.clear();
  clearPromptDraft(agent);
  clearPromptDraft({ ...agent, generation: 2 });
  connect();
  for (const method of ['readPane', 'watchPane', 'unwatchPane'] as const) vi.spyOn(relayStore, method).mockImplementation(() => {});
  vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({ available: true, reason: '', entries: [], hasMore: false, total: 0 });
});
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); relayStore.connections.set(new Map()); });

it('holds automatic app reloads for text it does not save, without saving it', async () => {
  const view = render(TerminalView, { agent, allAgents: [agent], responding: new Set<string>() });
  await vi.advanceTimersByTimeAsync(0);
  expect(reloadRisk()).toBeNull();
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: secret } });
  expect(screen.getByText(/Not saved on this phone/)).toBeVisible();
  expect(reloadRisk()).toBe('draft');
  expect(JSON.stringify(localStorage)).not.toContain(secret);
  view.unmount();
  expect(reloadRisk()).toBeNull();
});

for (const component of [TerminalView, ConversationHistory]) {
  describe(component === TerminalView ? 'Terminal draft privacy' : 'Conversation draft privacy', () => {
    it.each(['absent', 'stale', 'mismatch', 'generation', 'disconnect', 'resume', 'expiry'] as const)('never writes uncertain %s text, including after hidden then safe frames', async (state) => {
      const writes: string[] = [];
      const original = localStorage.setItem.bind(localStorage);
      vi.spyOn(localStorage, 'setItem').mockImplementation((key, value) => {
        writes.push(`${key}:${value}`);
        original(key, value);
      });
      const view = render(component as typeof TerminalView, { agent, allAgents: [agent], responding: new Set<string>() });
      await vi.advanceTimersByTimeAsync(0);
      if (['disconnect', 'resume', 'expiry'].includes(state)) {
        await view.rerender({ frame: frame() });
        await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'safe baseline' } });
        expect(loadPromptDraft(agent)).toBe('safe baseline');
      }
      if (state === 'stale') await view.rerender({ frame: frame(agent, false, now - 4_000) });
      if (state === 'mismatch') await view.rerender({ frame: frame({ ...agent, server_session_id: 'foreign' }) });
      if (state === 'generation') await view.rerender({ agent: { ...agent, generation: 2 }, frame: frame() });
      if (state === 'disconnect') connect('disconnected');
      if (state === 'resume') {
        Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' });
        await fireEvent(document, new Event('visibilitychange'));
        Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' });
        await fireEvent(document, new Event('visibilitychange'));
      }
      if (state === 'expiry') { now += 3_001; await vi.advanceTimersByTimeAsync(3_001); }
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: secret } });
      const current = state === 'generation' ? { ...agent, generation: 2 } : agent;
      connect();
      await vi.advanceTimersByTimeAsync(0);
      await view.rerender({ frame: frame(current, true) });
      await view.rerender({ frame: frame(current, false, now - 4_000) });
      await view.rerender({ frame: frame(current) });
      expect(writes.join('\n')).not.toContain(secret);
      expect(loadPromptDraft(current)).not.toContain(secret);
      expect(screen.getByText(/Not saved on this phone/)).toBeVisible();
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: '' } });
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'new safe draft' } });
      expect(loadPromptDraft(current)).toBe('new safe draft');
    });

    it('restores safe drafts only after fresh permission and never for a new exact target', async () => {
      savePromptDraft(agent, 'saved safe draft');
      const view = render(component as typeof TerminalView, { agent, allAgents: [agent], responding: new Set<string>() });
      await vi.advanceTimersByTimeAsync(0);
      expect(screen.getByLabelText('Prompt')).toHaveValue('');
      await view.rerender({ frame: frame() });
      expect(screen.getByLabelText('Prompt')).toHaveValue('saved safe draft');
      await view.rerender({ agent: { ...agent, generation: 2 } });
      await view.rerender({ frame: frame({ ...agent, generation: 2 }) });
      expect(screen.getByLabelText('Prompt')).toHaveValue('');
      expect(loadPromptDraft({ ...agent, generation: 2 })).toBe('');
      expect(loadPromptDraft(agent)).toBe('saved safe draft');
    });
  });
}

it('denies persistence when a caller omits permission', async () => {
  render(PromptComposer, { agent });
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: secret } });
  expect(loadPromptDraft(agent)).toBe('');
});
