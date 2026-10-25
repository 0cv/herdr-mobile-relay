import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import TerminalView from '$components/TerminalView.svelte';
import ConversationHistory from '$components/ConversationHistory.svelte';
import { clearPromptDraft, loadPromptDraft, promptDraftIdentity, savePromptDraft } from '$lib/prompt-drafts';
import { targetRefForAgent } from '$lib/resource-id';
import { relayStore } from '$lib/store';
import type { Agent, FrontendTargetRef, TerminalFrame } from '$lib/types';

const agent: Agent = {
  relay_id: 'session-privacy', relay_label: 'Synthetic', raw_pane_id: 'pane', pane_id: 'session-privacy::pane',
  server_session_id: 'server', terminal_id: 'terminal', generation: 1, agent_session_id: 'session', agent: 'codex', status: 'idle',
};
const secret = 'SYNTHETICSECRET-E25';
const invalidSessions = [undefined, '', ' ', '\t', '\u2002\u00a0', 'line\nbreak', 'nul\0byte', '\ud800', 'x'.repeat(2049), null, 42, {}];
const withSession = (value: unknown): Agent => ({ ...agent, agent_session_id: value as string });
const frame = (target: Agent): TerminalFrame => ({
  paneId: target.pane_id, target: {
    relay_id: target.relay_id, server_session_id: target.server_session_id, pane_id: target.raw_pane_id,
    terminal_id: target.terminal_id, generation: target.generation, agent_session_id: target.agent_session_id,
  } as FrontendTargetRef, content: 'Synthetic terminal', format: 'plain', noEcho: false, receivedAt: performance.now(),
});
const captureWrites = () => {
  const writes: Array<{ key: string; value: string }> = [];
  const original = localStorage.setItem.bind(localStorage);
  vi.spyOn(localStorage, 'setItem').mockImplementation((key, value) => { writes.push({ key, value }); original(key, value); });
  return writes;
};

beforeEach(() => {
  vi.spyOn(performance, 'now').mockReturnValue(10_000);
  localStorage.clear();
  clearPromptDraft(agent);
  clearPromptDraft({ ...agent, generation: 2 });
  relayStore.connections.set(new Map([[agent.relay_id, { status: 'connected', capabilities: ['secret_input'] }]] as never));
  for (const method of ['readPane', 'watchPane', 'unwatchPane'] as const) vi.spyOn(relayStore, method).mockImplementation(() => {});
  vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({ available: true, reason: '', entries: [], hasMore: false, total: 0 });
});
afterEach(() => { vi.restoreAllMocks(); relayStore.connections.set(new Map()); });

describe('complete draft identity', () => {
  it.each(invalidSessions)('denies invalid session %# without storage access or exceptions', (session) => {
    const current = withSession(session);
    const writes = captureWrites();
    const reads = vi.spyOn(localStorage, 'getItem');
    const removes = vi.spyOn(localStorage, 'removeItem');
    const saved = savePromptDraft(current, secret);
    const restored = loadPromptDraft(current);
    clearPromptDraft(current);
    expect(writes).toEqual([]);
    expect(saved).toBe('unavailable');
    expect(restored).toBe('');
    expect(promptDraftIdentity(current)).toBeNull();
    expect(reads).not.toHaveBeenCalled();
    expect(removes).not.toHaveBeenCalled();
  });

  it.each(['relay_id', 'server_session_id', 'raw_pane_id', 'terminal_id', 'generation'] as const)('denies missing %s', (field) => {
    const current = { ...agent, [field]: undefined } as unknown as Agent;
    const writes = captureWrites();
    expect(savePromptDraft(current, secret)).toBe('unavailable');
    expect(loadPromptDraft(current)).toBe('');
    clearPromptDraft(current);
    expect(writes).toEqual([]);
  });

  it('keeps optional-session routing unchanged', () => {
    expect(targetRefForAgent(withSession(undefined))).not.toBeNull();
  });
});

for (const component of [TerminalView, ConversationHistory]) {
  describe(component === TerminalView ? 'Terminal session permission' : 'Conversation session permission', () => {
    it.each(invalidSessions)('never writes uncertain input when agent and frame session are invalid %#', async (session) => {
      const current = withSession(session);
      const writes = captureWrites();
      const view = render(component as typeof TerminalView, { agent: current, allAgents: [current], responding: new Set<string>() });
      await view.rerender({ frame: frame(current) });
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: secret } });
      await view.rerender({ frame: { ...frame(current), noEcho: true } });
      await view.rerender({ frame: frame(current) });
      expect(JSON.stringify(writes)).not.toContain(secret);
      expect(loadPromptDraft(current)).toBe('');
      expect(screen.getByText(/Not saved on this phone/)).toBeVisible();
    });

    it.each(['missing-agent', 'missing-frame', 'blank-frame', 'malformed-frame', 'different-session', 'different-generation'] as const)('denies %s and does not untaint on a later safe frame', async (state) => {
      const current = state === 'missing-agent' ? withSession(undefined) : agent;
      const target = state === 'missing-frame' ? withSession(undefined)
        : state === 'blank-frame' ? withSession(' ')
        : state === 'malformed-frame' ? withSession({})
        : state === 'different-session' ? withSession('other-session')
        : state === 'different-generation' ? { ...agent, generation: 2 } : agent;
      const writes = captureWrites();
      const view = render(component as typeof TerminalView, { agent: current, allAgents: [current], responding: new Set<string>() });
      await view.rerender({ frame: frame(target) });
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: secret } });
      await view.rerender({ frame: frame(current) });
      expect(JSON.stringify(writes)).not.toContain(secret);
      expect(loadPromptDraft(current)).not.toContain(secret);
      expect(screen.getByText(/Not saved on this phone/)).toBeVisible();
    });

    it('restores and saves only a fresh complete exact session, preserving scoped cleanup', async () => {
      savePromptDraft(agent, 'safe existing draft');
      const view = render(component as typeof TerminalView, { agent, allAgents: [agent], responding: new Set<string>() });
      expect(screen.getByLabelText('Prompt')).toHaveValue('');
      await view.rerender({ frame: frame(withSession(undefined)) });
      expect(screen.getByLabelText('Prompt')).toHaveValue('');
      await view.rerender({ frame: frame(agent) });
      expect(screen.getByLabelText('Prompt')).toHaveValue('safe existing draft');
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'fresh safe draft' } });
      expect(loadPromptDraft(agent)).toBe('fresh safe draft');
      expect(loadPromptDraft(withSession('other-session'))).toBe('');
      expect(loadPromptDraft({ ...agent, generation: 2 })).toBe('');
      clearPromptDraft(withSession(undefined));
      expect(loadPromptDraft(agent)).toBe('fresh safe draft');
    });
  });
}
