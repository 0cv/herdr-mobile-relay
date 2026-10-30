import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import ConversationHistory from '$components/ConversationHistory.svelte';
import TerminalView from '$components/TerminalView.svelte';
import { clearPromptDraft, loadPromptDraft, savePromptDraft } from '$lib/prompt-drafts';
import { relayStore } from '$lib/store';
import { targetRefForAgent } from '$lib/resource-id';
import type { AttachmentBatchController, AttachmentBatchSnapshot } from '$lib/attachments';
import type { Agent, SlashCommandCatalog } from '$lib/types';

const agent: Agent = {
  relay_id: 'composer', relay_label: 'Composer', raw_pane_id: 'pane', pane_id: 'composer::pane',
  server_session_id: 'primary', terminal_id: 'terminal', generation: 1, agent_session_id: 'session',
  project: 'Composer', agent: 'codex', status: 'idle',
};
const frame = { paneId: agent.pane_id, content: 'ready', format: 'plain' as const };

function uploadFixture(interrupted = false) {
  let finish: (refs: { name: string; ref: string }[]) => void = () => {};
  const snapshot = {
    uploading: false,
    items: interrupted ? [{ clientId: 'file', name: 'picture.png', state: 'interrupted', progress: 0, bytes: 3, uploadedBytes: 0 }] : [],
  } as unknown as AttachmentBatchSnapshot;
  const restart = vi.fn().mockResolvedValue([]);
  const controller = {
    subscribe: (listener: (value: AttachmentBatchSnapshot) => void) => { listener(snapshot); return () => {}; },
    snapshot: () => snapshot, hasPendingCleanup: () => false,
    select: vi.fn(), cancel: vi.fn().mockResolvedValue(undefined), restart,
    upload: vi.fn(() => interrupted ? Promise.resolve([]) : new Promise((resolve) => { finish = resolve; })),
  } as unknown as AttachmentBatchController;
  vi.spyOn(relayStore, 'attachmentController').mockReturnValue(controller);
  return { finish: () => finish([{ name: 'picture.png', ref: 'attachment:picture' }]), restart };
}

describe('shared composer safety', () => {
  beforeEach(() => {
    clearPromptDraft(agent);
    relayStore.connections.set(new Map([[agent.relay_id, { status: 'connected', capabilities: ['secret_input', 'slash_commands'] }]] as never));
    vi.spyOn(relayStore, 'readPane').mockImplementation(() => {});
    vi.spyOn(relayStore, 'watchPane').mockImplementation(() => {});
    vi.spyOn(relayStore, 'unwatchPane').mockImplementation(() => {});
    vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({ available: true, reason: '', entries: [], hasMore: false, total: 0 });
    vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue({ type: 'command_result', request_id: 'sent', ok: true });
  });
  afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); clearPromptDraft(agent); relayStore.connections.set(new Map()); });

  it.each([1_500, 64_000])('does not revive expired echo state after a %i ms wall-clock rollback', async (rollback) => {
    vi.useFakeTimers();
    vi.setSystemTime(100_000);
    let monotonicNow = 100_000;
    vi.spyOn(performance, 'now').mockImplementation(() => monotonicNow);
    const view = render(ConversationHistory, { agent });
    await vi.advanceTimersByTimeAsync(0);
    await view.rerender({ frame: { ...frame, target: targetRefForAgent(agent), noEcho: false, receivedAt: monotonicNow } });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'safe draft' } });
    expect(loadPromptDraft(agent)).toBe('safe draft');
    monotonicNow += 4_000;
    await vi.advanceTimersByTimeAsync(4_000);
    vi.setSystemTime(Date.now() - rollback);
    monotonicNow += 250;
    await vi.advanceTimersByTimeAsync(250);
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'rollback-secret' } });
    expect(loadPromptDraft(agent)).toBe('safe draft');
    expect(JSON.stringify(localStorage)).not.toContain('rollback-secret');
  });

  it('rechecks echo expiry when its timer fires before the monotonic deadline', async () => {
    vi.useFakeTimers();
    let monotonicNow = 100_000;
    vi.spyOn(performance, 'now').mockImplementation(() => monotonicNow);
    const view = render(ConversationHistory, { agent });
    await vi.advanceTimersByTimeAsync(0);
    await view.rerender({ frame: { ...frame, target: targetRefForAgent(agent), noEcho: false, receivedAt: monotonicNow } });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'safe draft' } });
    expect(loadPromptDraft(agent)).toBe('safe draft');
    monotonicNow += 2_999;
    await vi.advanceTimersByTimeAsync(3_000);
    monotonicNow += 2;
    await vi.advanceTimersByTimeAsync(2);
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'expired-echo-secret' } });
    expect(loadPromptDraft(agent)).toBe('safe draft');
    expect(JSON.stringify(localStorage)).not.toContain('expired-echo-secret');
  });

  it.each([1, 2, 3, 4, 5, 6, 7, 8])('expires echo state when the deadline passes after clock read %i of the expiry update', async (crossing) => {
    vi.useFakeTimers();
    let monotonicNow = 100_000;
    let step = 0;
    vi.spyOn(performance, 'now').mockImplementation(() => (monotonicNow += step));
    const view = render(ConversationHistory, { agent });
    await vi.advanceTimersByTimeAsync(0);
    await view.rerender({ frame: { ...frame, target: targetRefForAgent(agent), noEcho: false, receivedAt: monotonicNow } });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'safe draft' } });
    expect(loadPromptDraft(agent)).toBe('safe draft');
    monotonicNow = 103_000 - crossing * 0.01 - 0.005;
    step = 0.01;
    await vi.advanceTimersByTimeAsync(3_000);
    step = 0;
    monotonicNow = 104_000;
    await vi.advanceTimersByTimeAsync(5_000);
    expect(screen.getByText('Not saved on this phone: waiting for fresh terminal input state.')).toBeInTheDocument();
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'stale-echo-secret' } });
    expect(loadPromptDraft(agent)).toBe('safe draft');
    expect(JSON.stringify(localStorage)).not.toContain('stale-echo-secret');
  });

  it.each(['untagged', 'generation', 'terminal', 'session', 'server'] as const)('rejects late %s echo-on frames after a target switch', async (kind) => {
    vi.useFakeTimers();
    vi.setSystemTime(100_000);
    vi.spyOn(performance, 'now').mockReturnValue(100_000);
    const next = { ...agent, generation: 2 };
    const view = render(ConversationHistory, { agent });
    await vi.advanceTimersByTimeAsync(0);
    await view.rerender({ agent: next });
    const target = kind === 'untagged' ? undefined : targetRefForAgent({ ...next,
      ...(kind === 'generation' ? { generation: 1 } : {}),
      ...(kind === 'terminal' ? { terminal_id: 'old-terminal' } : {}),
      ...(kind === 'session' ? { agent_session_id: 'old-session' } : {}),
      ...(kind === 'server' ? { server_session_id: 'old-server' } : {}),
    });
    await view.rerender({ frame: { ...frame, target, noEcho: false, receivedAt: 100_000 } });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'late-frame-secret' } });
    expect(loadPromptDraft(next)).toBe('');
    expect(JSON.stringify(localStorage)).not.toContain('late-frame-secret');
  });

  it.each([undefined, NaN, Infinity, 100_001])('fails closed on unknown or negative frame age %s', async (receivedAt) => {
    vi.useFakeTimers();
    vi.setSystemTime(100_000);
    vi.spyOn(performance, 'now').mockReturnValue(100_000);
    const view = render(ConversationHistory, { agent });
    await vi.advanceTimersByTimeAsync(0);
    await view.rerender({ frame: { ...frame, target: targetRefForAgent(agent), noEcho: false, receivedAt } });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'unknown-age-secret' } });
    expect(loadPromptDraft(agent)).toBe('');
  });

  it('rechecks freshness on visibility changes without waiting for a timer', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(100_000);
    const now = vi.spyOn(performance, 'now').mockReturnValue(100_000);
    const view = render(ConversationHistory, { agent });
    await vi.advanceTimersByTimeAsync(0);
    await view.rerender({ frame: { ...frame, target: targetRefForAgent(agent), noEcho: false, receivedAt: 100_000 } });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'safe draft' } });
    expect(loadPromptDraft(agent)).toBe('safe draft');
    now.mockReturnValue(104_000);
    await fireEvent(document, new Event('visibilitychange'));
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'resumed-secret' } });
    expect(loadPromptDraft(agent)).toBe('safe draft');
  });

  it.each(['Conversation', 'Terminal'] as const)('%s masks untagged no-echo frames without persisting secrets', async (viewName) => {
    const props = { agent, frame: { ...frame, noEcho: true }, allAgents: [agent], responding: new Set<string>() };
    if (viewName === 'Conversation') render(ConversationHistory, props);
    else render(TerminalView, props);
    const secret = screen.getByLabelText('Value for the hidden terminal prompt');
    expect(secret).toHaveAttribute('type', 'password');
    expect(secret).toBeDisabled();
    await fireEvent.input(secret, { target: { value: 'masked-secret' } });
    await fireEvent.keyDown(secret, { key: 'Enter' });
    expect(screen.getByRole('button', { name: 'Send hidden value' })).toBeDisabled();
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
    expect(loadPromptDraft(agent)).toBe('');
    expect(JSON.stringify(localStorage)).not.toContain('masked-secret');
  });

  it('persists Terminal drafts only after a fresh exact-target echo-on frame', async () => {
    const view = render(TerminalView, { agent, frame, allAgents: [agent], responding: new Set<string>() });
    await view.rerender({ frame: { ...frame, target: targetRefForAgent(agent), noEcho: false, receivedAt: performance.now() } });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'terminal safe draft' } });
    expect(loadPromptDraft(agent)).toBe('terminal safe draft');
  });

  for (const viewName of ['Conversation', 'Terminal'] as const) {
    it(`${viewName} settles slash discovery despite unrelated connection and agent updates`, async () => {
      let finish: (catalog: SlashCommandCatalog) => void = () => {};
      const load = vi.spyOn(relayStore, 'loadSlashCommands').mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
      const view = viewName === 'Conversation'
        ? render(ConversationHistory, { agent, frame })
        : render(TerminalView, { agent, frame, allAgents: [agent], responding: new Set<string>() });
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: '/' } });
      expect(load).toHaveBeenCalledTimes(1);
      for (let update = 0; update < 12; update++) {
        relayStore.connections.update((connections) => new Map(connections));
        await view.rerender({ agent: { ...agent, updated_at: update } });
      }
      finish({ commands: [{ command: '/help', description: 'Help', source: 'builtin' }], truncated: false });
      await waitFor(() => expect(screen.getByRole('listbox')).toHaveAttribute('aria-busy', 'false'));
      expect(load).toHaveBeenCalledTimes(1);
      expect(load).toHaveBeenLastCalledWith(expect.objectContaining({ generation: 1 }), true);
      load.mockResolvedValue({ commands: [], truncated: false });
      await fireEvent.click(screen.getByRole('button', { name: 'Refresh commands' }));
      expect(load).toHaveBeenLastCalledWith(expect.objectContaining({ generation: 1 }), true);
      await view.rerender({ agent: { ...agent, generation: 2 } });
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: '/' } });
      await waitFor(() => expect(load).toHaveBeenCalledTimes(3));
      relayStore.connections.set(new Map([[agent.relay_id, { status: 'disconnected', capabilities: [] }]] as never));
      await waitFor(() => expect(screen.getByText(/Suggestions unavailable/)).toBeVisible());
      relayStore.connections.set(new Map([[agent.relay_id, { status: 'connected', capabilities: [] }]] as never));
      await waitFor(() => expect(load).toHaveBeenCalledTimes(4));
    });
  }

  it('polls only a bounded input-state tail, independent of inventory emissions', async () => {
    vi.useFakeTimers();
    const view = render(ConversationHistory, { agent });
    await vi.advanceTimersByTimeAsync(0);
    expect(relayStore.readPane).toHaveBeenCalledExactlyOnceWith(agent, true, 30);
    expect(relayStore.watchPane).not.toHaveBeenCalled();
    for (let update = 0; update < 12; update++) {
      relayStore.connections.update((connections) => new Map(connections));
      await view.rerender({ agent: { ...agent, updated_at: update } });
    }
    expect(relayStore.readPane).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(relayStore.readPane).toHaveBeenCalledTimes(2);
    expect(relayStore.readPane).toHaveBeenLastCalledWith(agent, false, 30);
    view.unmount();
    await vi.advanceTimersByTimeAsync(4_000);
    expect(relayStore.readPane).toHaveBeenCalledTimes(2);
  });

  it.each(['unknown', 'stale', 'disconnected', 'replacement'] as const)('never persists Conversation text with %s pane state, even after recovery', async (state) => {
    vi.useFakeTimers();
    savePromptDraft(agent, 'safe draft');
    const current = state === 'replacement' ? { ...agent, generation: 2 } : agent;
    const view = render(ConversationHistory, { agent });
    await vi.advanceTimersByTimeAsync(0);
    if (state !== 'unknown') {
      await view.rerender({ frame: { ...frame, target: targetRefForAgent(agent), noEcho: false, receivedAt: performance.now() } });
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'fresh safe draft' } });
      expect(loadPromptDraft(agent)).toBe('fresh safe draft');
      if (state === 'stale') await vi.advanceTimersByTimeAsync(3_250);
      if (state === 'disconnected') relayStore.connections.set(new Map([[agent.relay_id, { status: 'disconnected', capabilities: [] }]] as never));
      if (state === 'replacement') {
        await vi.advanceTimersByTimeAsync(1);
        await view.rerender({ agent: { ...agent, generation: 2 } });
      }
    }
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'unchecked-secret' } });
    expect(loadPromptDraft(agent)).not.toContain('unchecked-secret');
    relayStore.connections.set(new Map([[agent.relay_id, { status: 'connected', capabilities: [] }]] as never));
    await view.rerender({ frame: { ...frame, target: targetRefForAgent(current), noEcho: false, receivedAt: performance.now() } });
    expect(loadPromptDraft(agent)).not.toContain('unchecked-secret');
    expect(screen.getByText(/Not saved on this phone/)).toBeVisible();
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: '' } });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'new safe draft' } });
    expect(screen.queryByText(/Not saved on this phone/)).not.toBeInTheDocument();
    expect(loadPromptDraft(current)).toBe('new safe draft');
  });

  it('never persists text typed in Conversation at a hidden prompt across view switches', async () => {
    savePromptDraft(agent, 'safe draft');
    const history = render(ConversationHistory, { agent, frame: { ...frame, noEcho: true } } as never);
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'conversation-secret' } });
    await waitFor(() => expect(loadPromptDraft(agent)).toBe('safe draft'));
    history.unmount();
    const terminal = render(TerminalView, { agent, frame, allAgents: [agent], responding: new Set<string>() });
    expect(screen.getByLabelText('Prompt')).toHaveValue('');
    await terminal.rerender({ frame: { ...frame, target: targetRefForAgent(agent), noEcho: false, receivedAt: performance.now() } });
    expect(screen.getByLabelText('Prompt')).toHaveValue('safe draft');
    expect(JSON.stringify(localStorage)).not.toContain('conversation-secret');
  });

  it.each(['ctrlKey', 'metaKey'])('blocks Terminal %s+Enter until the uploaded ref is appended', async (modifier) => {
    const upload = uploadFixture();
    render(TerminalView, { agent, frame, allAgents: [agent], responding: new Set<string>() });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'Look at this' } });
    await fireEvent.change(document.querySelector('input[type=file]')!, { target: { files: [new File(['png'], 'picture.png', { type: 'image/png' })] } });
    await fireEvent.keyDown(screen.getByLabelText('Prompt'), { key: 'Enter', [modifier]: true });
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
    upload.finish();
    await waitFor(() => expect(screen.getByLabelText('Prompt')).toHaveValue('Look at this\nAttachment: attachment:picture\n'));
    await fireEvent.keyDown(screen.getByLabelText('Prompt'), { key: 'Enter', [modifier]: true });
    expect(relayStore.sendToAgent).toHaveBeenCalledWith(expect.objectContaining({ generation: 1 }), { type: 'submit_prompt', text: 'Look at this\nAttachment: attachment:picture' });
  });

  it('exposes the question key character field only after arming a modifier', async () => {
    const blocked = { ...agent, status: 'blocked', attention_kind: 'question' as const, attention_capable: true, interaction: {
      id: 'key-question', kind: 'single_select' as const, question: 'Choose coverage',
      options: [{ index: 0, label: 'Both views' }], other: { hidden: true },
    } };
    render(TerminalView, { agent: blocked, allAgents: [blocked], frame, responding: new Set<string>() });
    expect(screen.queryByRole('textbox', { name: 'Modifier shortcut character' })).not.toBeInTheDocument();
    await fireEvent.click(screen.getByRole('button', { name: 'Ctrl' }));
    expect(screen.getByRole('textbox', { name: 'Modifier shortcut character' })).toHaveFocus();
    await fireEvent.input(screen.getByRole('textbox', { name: 'Modifier shortcut character' }), { target: { value: 'c' } });
    expect(relayStore.sendToAgent).toHaveBeenCalledWith(blocked, expect.objectContaining({ type: 'send_keys', keys: ['ctrl+c'] }));
  });

  it('keeps live-screen filter pickers in Terminal even when inventory reports done', async () => {
    render(ConversationHistory, { agent: { ...agent, status: 'done' }, frame: { ...frame, content: 'Available models\nType to filter • Enter to select • Tab to edit' } });
    expect(screen.getByLabelText('Prompt')).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Send prompt' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Attach photos' })).toBeDisabled();
    expect(screen.queryByRole('button', { name: 'Keyboard' })).not.toBeInTheDocument();
    expect(screen.getByText('Draft only — use structured controls or advanced input in Terminal.')).toBeVisible();
    await fireEvent.keyDown(screen.getByLabelText('Prompt'), { key: 'Enter', ctrlKey: true });
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
  });

  it('guards Conversation restart after input becomes locked and keeps upload status visible', async () => {
    const upload = uploadFixture(true);
    const view = render(ConversationHistory, { agent });
    await fireEvent.change(document.querySelector('input[type=file]')!, { target: { files: [new File(['png'], 'picture.png', { type: 'image/png' })] } });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Restart interrupted files' })).toBeEnabled());
    await view.rerender({ agent: { ...agent, status: 'blocked', attention_kind: 'unknown' } });
    const restart = screen.getByRole('button', { name: 'Restart interrupted files' });
    expect(restart).toBeDisabled();
    restart.removeAttribute('disabled');
    await fireEvent.click(restart);
    expect(upload.restart).not.toHaveBeenCalled();
    expect(screen.getByText('No attachments were uploaded.')).toBeVisible();
  });

  for (const viewName of ['Conversation', 'Terminal'] as const) {
    const mount = (current: Agent = agent, readOnly = false) => viewName === 'Conversation'
      ? render(ConversationHistory, { agent: current, readOnly })
      : render(TerminalView, { agent: current, frame, readOnly, allAgents: [current], responding: new Set<string>() });

    it(`${viewName} denies attachments and sending without an exact target`, async () => {
      mount({ ...agent, terminal_id: '' });
      expect(screen.getByRole('button', { name: 'Attach photos' })).toBeDisabled();
      expect(screen.getByText(/Attachments and sending need a stable target/)).toBeVisible();
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'unsafe' } });
      await fireEvent.keyDown(screen.getByLabelText('Prompt'), { key: 'Enter', ctrlKey: true });
      expect(relayStore.sendToAgent).not.toHaveBeenCalled();
    });

    it(`${viewName} refuses picker and paste uploads while a prompt is sending`, async () => {
      let finish: () => void = () => {};
      vi.mocked(relayStore.sendToAgent).mockImplementation(() => new Promise((resolve) => {
        finish = () => resolve({ type: 'command_result', request_id: 'sent', ok: true });
      }));
      const upload = vi.spyOn(relayStore, 'attachmentController');
      mount();
      const prompt = screen.getByLabelText('Prompt');
      await fireEvent.input(prompt, { target: { value: 'pending' } });
      await fireEvent.keyDown(prompt, { key: 'Enter', metaKey: true });
      expect(screen.getByRole('button', { name: 'Attach photos' })).toBeDisabled();
      const file = new File(['png'], 'picture.png', { type: 'image/png' });
      await fireEvent.change(document.querySelector('input[type=file]')!, { target: { files: [file] } });
      await fireEvent.paste(prompt, { clipboardData: { items: [{ kind: 'file', type: file.type, getAsFile: () => file }] } });
      expect(upload).not.toHaveBeenCalled();
      finish();
    });

    it(`${viewName} inserts pasted file refs and permits sending only after completion`, async () => {
      const upload = uploadFixture();
      mount();
      const prompt = screen.getByLabelText('Prompt');
      const file = new File(['notes'], 'notes.txt', { type: 'text/plain' });
      await fireEvent.paste(prompt, { clipboardData: { items: [{ kind: 'file', type: file.type, getAsFile: () => file }] } });
      expect(relayStore.attachmentController).toHaveBeenCalledWith(agent);
      expect(screen.getByRole('button', { name: 'Send prompt' })).toBeDisabled();
      upload.finish();
      await waitFor(() => expect(prompt).toHaveValue('Attachment: attachment:picture\n'));
      await fireEvent.keyDown(prompt, { key: 'Enter', ctrlKey: true });
      expect(relayStore.sendToAgent).toHaveBeenCalledWith(agent, { type: 'submit_prompt', text: 'Attachment: attachment:picture' });
    });

    it(`${viewName} never restores a draft after unknown dispatch`, async () => {
      vi.mocked(relayStore.sendToAgent).mockRejectedValue({ data: { dispatched_unknown: true } });
      mount();
      const prompt = screen.getByLabelText('Prompt');
      await fireEvent.input(prompt, { target: { value: 'do not repeat' } });
      await fireEvent.keyDown(prompt, { key: 'Enter', ctrlKey: true });
      expect(prompt).toHaveValue('');
      expect(loadPromptDraft(agent)).toBe('');
    });

    it(`${viewName} does not restore a refused prompt into a replacement target`, async () => {
      let reject: (error: Error) => void = () => {};
      vi.mocked(relayStore.sendToAgent).mockImplementation(() => new Promise((_, fail) => { reject = fail; }));
      const view = mount();
      await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'old target only' } });
      await fireEvent.keyDown(screen.getByLabelText('Prompt'), { key: 'Enter', ctrlKey: true });
      await view.rerender({ agent: { ...agent, generation: 2 } });
      reject(new Error('Refused'));
      await waitFor(() => expect(screen.getByLabelText('Prompt')).toHaveValue(''));
      expect(loadPromptDraft(agent)).toBe('');
    });

    it(`${viewName} reader denies forced prompt, picker, paste and key events`, async () => {
      const upload = vi.spyOn(relayStore, 'attachmentController');
      mount(agent, true);
      const prompt = screen.getByLabelText('Prompt');
      const file = new File(['png'], 'picture.png', { type: 'image/png' });
      await fireEvent.input(prompt, { target: { value: '/plan' } });
      await fireEvent.keyDown(prompt, { key: 'Enter', ctrlKey: true });
      await fireEvent.change(document.querySelector('input[type=file]')!, { target: { files: [file] } });
      await fireEvent.paste(prompt, { clipboardData: { items: [{ kind: 'file', type: file.type, getAsFile: () => file }] } });
      if (viewName === 'Terminal') await fireEvent.click(screen.getByRole('button', { name: 'Enter', hidden: true }));
      else expect(screen.queryByRole('button', { name: 'Enter', hidden: true })).not.toBeInTheDocument();
      expect(relayStore.sendToAgent).not.toHaveBeenCalled();
      expect(upload).not.toHaveBeenCalled();
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    });
  }
});
