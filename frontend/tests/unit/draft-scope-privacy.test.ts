import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import TerminalView from '$components/TerminalView.svelte';
import ConversationHistory from '$components/ConversationHistory.svelte';
import { clearPromptDraft, loadPromptDraft, promptDraftIdentity, savePromptDraft } from '$lib/prompt-drafts';
import { targetRefForAgent } from '$lib/resource-id';
import { relayStore } from '$lib/store';
import type { Agent, TerminalFrame } from '$lib/types';

const agent: Agent = {
  relay_id: 'scope-privacy', relay_label: 'Synthetic', raw_pane_id: 'pane', pane_id: 'scope-privacy::pane',
  server_session_id: 'server', terminal_id: 'terminal', generation: 1, agent_session_id: 'session',
  agent: 'codex', cwd: '/synthetic/one', status: 'idle',
};
const sessions = ['session id', 'session\u2002id', 'session\u00a0id', ' padded '];
const secret = 'SYNTHETICSECRET-E27';
const frame = (current: Agent): TerminalFrame => ({
  paneId: current.pane_id, target: targetRefForAgent(current), content: 'Synthetic terminal',
  format: 'plain', noEcho: false, receivedAt: performance.now(),
});
const changes: Array<[string, Partial<Agent>]> = [
  ['provider', { agent: 'claude' }], ['cwd', { cwd: '/synthetic/two' }],
  ['generation', { generation: 2 }], ['terminal', { terminal_id: 'terminal-2' }],
  ['server', { server_session_id: 'server-2' }], ['pane', { raw_pane_id: 'pane-2' }],
];
const captureWrites = () => {
  const writes: Array<{ key: string; value: string }> = [];
  const original = localStorage.setItem.bind(localStorage);
  vi.spyOn(localStorage, 'setItem').mockImplementation((key, value) => { writes.push({ key, value }); original(key, value); });
  return writes;
};

beforeEach(() => {
  vi.spyOn(performance, 'now').mockReturnValue(10_000);
  localStorage.clear();
  for (const current of [agent, ...sessions.map((agent_session_id) => ({ ...agent, agent_session_id })),
    ...changes.map(([, change]) => ({ ...agent, ...change }))]) clearPromptDraft(current);
  relayStore.connections.set(new Map([[agent.relay_id, { status: 'connected', capabilities: ['secret_input'] }]] as never));
  for (const method of ['readPane', 'watchPane', 'unwatchPane'] as const) vi.spyOn(relayStore, method).mockImplementation(() => {});
  vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({ available: false, reason: 'Synthetic unavailable history', entries: [], hasMore: false, total: 0 });
  vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue({ type: 'command_result', request_id: 'synthetic', ok: true });
});
afterEach(() => { vi.restoreAllMocks(); relayStore.connections.set(new Map()); });

describe('opaque draft session domain', () => {
  it.each(sessions)('saves and loads exact route-valid session %s without normalization', (agent_session_id) => {
    const current = { ...agent, agent_session_id };
    expect(targetRefForAgent(current)?.agent_session_id).toBe(agent_session_id);
    expect(savePromptDraft(current, 'safe whitespace draft')).toBe('saved');
    expect(loadPromptDraft(current)).toBe('safe whitespace draft');
    expect(promptDraftIdentity(current)).not.toBeNull();
    expect(loadPromptDraft({ ...current, agent_session_id: agent_session_id.replace(/\s/gu, '') })).toBe('');
    expect(promptDraftIdentity(current)).not.toBe(promptDraftIdentity({ ...current, agent_session_id: agent_session_id.trim() + 'x' }));
  });

  it.each(['\u2002\u00a0', '\u007f', '\udfff', 'a\u0001b', 'x'.repeat(2049)])('denies invalid session %# without storage access', (agent_session_id) => {
    const current = { ...agent, agent_session_id };
    const reads = vi.spyOn(localStorage, 'getItem');
    const writes = captureWrites();
    const removes = vi.spyOn(localStorage, 'removeItem');
    expect(savePromptDraft(current, secret)).toBe('unavailable');
    expect(loadPromptDraft(current)).toBe('');
    clearPromptDraft(current);
    expect(reads).not.toHaveBeenCalled();
    expect(writes).toEqual([]);
    expect(removes).not.toHaveBeenCalled();
  });
});

for (const component of [TerminalView, ConversationHistory]) {
  describe(component === TerminalView ? 'Terminal composer scope' : 'Conversation composer scope without history', () => {
    it.each(sessions)('grants and restores only the exact fresh whitespace session %s', async (agent_session_id) => {
      const current = { ...agent, agent_session_id };
      expect(savePromptDraft(current, 'safe matching draft')).toBe('saved');
      const view = render(component as typeof TerminalView, { agent: current, allAgents: [current], responding: new Set<string>() });
      expect(screen.getByLabelText('Prompt')).toHaveValue('');
      await view.rerender({ frame: frame({ ...current, agent_session_id: agent_session_id.replace(/\s/gu, '') }) });
      expect(screen.getByLabelText('Prompt')).toHaveValue('');
      await view.rerender({ frame: frame(current) });
      expect(screen.getByLabelText('Prompt')).toHaveValue('safe matching draft');
      const writes = captureWrites();
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'fresh safe text' } });
      expect(loadPromptDraft(current)).toBe('fresh safe text');
      expect(JSON.stringify(writes)).toContain('fresh safe text');
    });

    it.each(changes)('clears unsaved text and taint across null-to-null %s changes at the real caller boundary', async (_, change) => {
      const current = { ...agent, agent_session_id: undefined };
      const next = { ...current, ...change };
      const writes = captureWrites();
      const reads = vi.spyOn(localStorage, 'getItem');
      const removes = vi.spyOn(localStorage, 'removeItem');
      const view = render(component as typeof TerminalView, { agent: current, allAgents: [current], responding: new Set<string>() });
      expect(reads.mock.calls).toEqual(component === ConversationHistory ? [['herdr-conversation-view']] : []);
      reads.mockClear();
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: secret } });
      expect(screen.getByText(/Not saved on this phone/)).toBeVisible();
      expect(promptDraftIdentity(current)).toBeNull();
      expect(promptDraftIdentity(next)).toBeNull();
      await view.rerender({ agent: next, allAgents: [next] });
      expect(screen.getByLabelText('Prompt')).toHaveValue('');
      expect(screen.queryByText(/Not saved on this phone/)).toBeNull();
      expect(writes).toEqual([]);
      expect(reads).not.toHaveBeenCalled();
      expect(removes).not.toHaveBeenCalled();
      await fireEvent.click(screen.getByRole('button', { name: /^Send prompt$/ }));
      expect(relayStore.sendToAgent).not.toHaveBeenCalled();
    });

    it('retains an unchanged sessionless scope and permits ordinary manual routing without storage', async () => {
      const current = { ...agent, agent_session_id: undefined };
      const writes = captureWrites();
      const reads = vi.spyOn(localStorage, 'getItem');
      const removes = vi.spyOn(localStorage, 'removeItem');
      const view = render(component as typeof TerminalView, { agent: current, allAgents: [current], responding: new Set<string>() });
      expect(reads.mock.calls).toEqual(component === ConversationHistory ? [['herdr-conversation-view']] : []);
      reads.mockClear();
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'ordinary manual prompt' } });
      await view.rerender({ agent: { ...current, updated_at: 2, status: 'idle' } });
      expect(screen.getByLabelText('Prompt')).toHaveValue('ordinary manual prompt');
      expect(writes).toEqual([]);
      expect(reads).not.toHaveBeenCalled();
      expect(removes).not.toHaveBeenCalled();
      await fireEvent.click(screen.getByRole('button', { name: /^Send prompt$/ }));
      expect(relayStore.sendToAgent).toHaveBeenCalledWith(expect.objectContaining({ agent_session_id: undefined }), { type: 'submit_prompt', text: 'ordinary manual prompt' });
      expect(writes).toEqual([]);
      expect(reads).not.toHaveBeenCalled();
      expect(removes).not.toHaveBeenCalled();
    });

    it.each(changes.slice(0, 2))('isolates persisted %s drafts until a new matching frame after scope reset', async (_, change) => {
      const next = { ...agent, ...change };
      savePromptDraft(agent, 'old safe draft');
      savePromptDraft(next, 'other safe draft');
      const view = render(component as typeof TerminalView, { agent, allAgents: [agent], responding: new Set<string>() });
      const previous = frame(agent);
      await view.rerender({ frame: previous });
      expect(screen.getByLabelText('Prompt')).toHaveValue('old safe draft');
      await view.rerender({ agent: next, allAgents: [next] });
      expect(screen.getByLabelText('Prompt')).toHaveValue('');
      await view.rerender({ frame: { ...previous, receivedAt: 6_000 } });
      expect(screen.getByLabelText('Prompt')).toHaveValue('');
      await view.rerender({ frame: frame(next) });
      expect(screen.getByLabelText('Prompt')).toHaveValue('other safe draft');
      expect(loadPromptDraft(agent)).toBe('old safe draft');
    });

    it('does not restore another safe draft over new uncertain text after a sessionless scope change', async () => {
      const current = { ...agent, agent_session_id: undefined };
      const next = { ...agent, agent: 'claude', cwd: '/synthetic/two' };
      clearPromptDraft(next);
      savePromptDraft(next, 'isolated safe draft');
      const writes = captureWrites();
      const view = render(component as typeof TerminalView, { agent: current, allAgents: [current], responding: new Set<string>() });
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'old unsaved text' } });
      await view.rerender({ agent: next, allAgents: [next], frame: frame(current) });
      expect(screen.getByLabelText('Prompt')).toHaveValue('');
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: secret } });
      await view.rerender({ frame: frame(next) });
      expect(screen.getByLabelText('Prompt')).toHaveValue(secret);
      expect(JSON.stringify(writes)).not.toContain(secret);
      expect(loadPromptDraft(next)).toBe('isolated safe draft');
      await fireEvent.click(screen.getByRole('button', { name: /^Clear prompt text$/ }));
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'new safe scope text' } });
      expect(loadPromptDraft(next)).toBe('new safe scope text');
    });
  });
}
