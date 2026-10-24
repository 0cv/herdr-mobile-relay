import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import ConversationHistory from '$components/ConversationHistory.svelte';
import TerminalView from '$components/TerminalView.svelte';
import ManualTerminalInput from '$components/ManualTerminalInput.svelte';
import { relayStore } from '$lib/store';
import { securityState } from '$lib/security';
import { targetRefForAgent } from '$lib/resource-id';
import { terminalTextError } from '$lib/terminal-input-policy';
import { clearPromptDraft, loadPromptDraft } from '$lib/prompt-drafts';
import { TerminalInputDelivery } from '$lib/terminal-input-delivery.svelte';
import { piPromptDelivery } from '$lib/pi-prompt.svelte';
import type { Agent, CommandResult, QuestionInteraction } from '$lib/types';

const agent: Agent = { relay_id: 'manual', relay_label: 'Manual', raw_pane_id: 'pane', pane_id: 'manual::pane', server_session_id: 'primary', terminal_id: 'terminal', generation: 1, agent_session_id: ' opaque session/界 ', agent: 'pi', project: 'Synthetic', status: 'blocked', attention_kind: 'unknown' };
const connection = { status: 'connected', capabilities: ['manual_terminal_input'], inventory: { state: 'ready', stale: false } };
const receipt = { type: 'command_result', request_id: 'manual', ok: true, phase: 'completed' } as CommandResult;
const frame = (current = agent, noEcho: boolean | undefined = false) => ({ paneId: current.pane_id, target: targetRefForAgent(current), inputTarget: targetRefForAgent(current), content: 'Arbitrary extension: destination\n> ', format: 'plain' as const, receivedAt: performance.now(), readStartedAt: performance.now(), noEcho });
const button = (name: string) => screen.getByRole('button', { name });

beforeEach(() => {
  localStorage.clear();
  agent.generation = (agent.generation || 0) + 1;
  clearPromptDraft(agent);
  securityState.set({ locked: false } as never);
  relayStore.connections.set(new Map([[agent.relay_id, connection]] as never));
  vi.spyOn(relayStore, 'readPane').mockImplementation(() => {});
  vi.spyOn(relayStore, 'watchPane').mockImplementation(() => {});
  vi.spyOn(relayStore, 'unwatchPane').mockImplementation(() => {});
  vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({ available: true, entries: [], hasMore: false, total: 0, reason: '' });
  vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue(receipt);
});
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); relayStore.connections.set(new Map()); securityState.set({ locked: false } as never); });

async function open(view: 'Conversation' | 'Terminal', current = agent) {
  const rendered = view === 'Conversation' ? render(ConversationHistory, { agent: current }) : render(TerminalView, { agent: current, allAgents: [current], responding: new Set<string>() });
  await rendered.rerender({ frame: frame(current) });
  await fireEvent.click(button('Type in terminal'));
  await waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
  await rendered.rerender({ frame: frame(current) });
  await waitFor(() => expect(screen.getByLabelText('Terminal text')).toHaveFocus());
  return rendered;
}

it.each(['\n','\r','\t','\u001b','\u0000','\u007f','\u0085','\u2028','\u2029','\ud800'])('rejects terminal controls %j', text => expect(terminalTextError('text'+text)).toBeTruthy());
it('refreshes read-only context using the normal history budget without inventory-driven loops', async () => {
  vi.useFakeTimers();
  const current = { ...agent };
  const rendered = render(ManualTerminalInput, { agent: current, frame: frame(), active: true, readOnly: false, delivery: new TerminalInputDelivery(), onexit: () => {} });
  expect(relayStore.readPane).toHaveBeenCalledExactlyOnceWith(current, true);
  for (let i = 0; i < 12; i++) relayStore.connections.set(new Map([[agent.relay_id, { ...connection }]] as never));
  await vi.advanceTimersByTimeAsync(2_000);
  expect(relayStore.readPane).toHaveBeenCalledTimes(2);
  await rendered.rerender({ active: false });
  await vi.advanceTimersByTimeAsync(4_000);
  expect(relayStore.readPane).toHaveBeenCalledTimes(2);
  expect(relayStore.sendToAgent).not.toHaveBeenCalled();
});

it('preserves Unicode, spaces and the exact byte ceiling', () => {
  for (const text of ['  /custom-command 界😀  ', ' '.repeat(65_536), '界'.repeat(21_845)+'x']) expect(terminalTextError(text)).toBe('');
  expect(terminalTextError('界'.repeat(21_846))).toContain('65,536');
});

const manualViews: Array<'Conversation' | 'Terminal'> = ['Terminal'];
for (const view of manualViews) describe(view, () => {
  it.each(['working','blocked','idle'])('keeps %s editing separate from dispatch', async status => {
    const current = { ...agent, status };
    const rendered = view === 'Conversation' ? render(ConversationHistory, { agent: current }) : render(TerminalView, { agent: current, allAgents: [current], responding: new Set<string>() });
    const prompt = screen.getByLabelText('Prompt');
    expect(prompt).toBeEnabled();
    await fireEvent.input(prompt, { target: { value: 'local draft' } });
    expect(prompt).toHaveValue('local draft');
    rendered.unmount();
  });

  it('sends literal text once, never Enter, slash handling, prompt history or semantic answers', async () => {
    const rendered = await open(view);
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: '  /unrelated 界😀  ' } });
    await fireEvent.click(button('Send text'));
    expect(relayStore.sendToAgent).toHaveBeenCalledExactlyOnceWith(agent, { type: 'send_terminal_text', text: '  /unrelated 界😀  ' });
    expect(screen.getByText('Text delivered; Enter not sent.')).toBeVisible();
    expect(agent.status).toBe('blocked');
    expect(loadPromptDraft(agent)).toBe('');
    expect(JSON.stringify(localStorage)).not.toContain('/unrelated');
    await fireEvent.click(button('Enter'));
    expect(relayStore.sendToAgent).toHaveBeenLastCalledWith(agent, { type: 'send_keys', keys: ['Enter'], activity_label: 'Manual Enter' });
    await fireEvent.click(button('Back to prompt and structured controls'));
    expect(screen.getByLabelText('Prompt')).toBeVisible();
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(2);
    rendered.unmount();
  });

  it('keeps an unconfirmed Pi bridge send fenced in advanced input without clearing it from pixels', async () => {
    const current = { ...agent };
    const rendered = await open(view, current);
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'another message' } });
    expect(button('Send text')).toBeEnabled();
    const piDelivery = piPromptDelivery(current);
    expect(piDelivery.begin()).toBe(true);
    await tick();
    expect(button('Send text')).toBeDisabled();
    expect(button('Enter')).toBeDisabled();
    piDelivery.finish(true, 5000);
    await rendered.rerender({ frame: frame(current) });
    expect(button('Send text')).toBeDisabled();
    expect(piDelivery.uncertain).toBe(true);
    await fireEvent.click(button('Send text'));
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
    await fireEvent.click(button('Back to prompt and structured controls'));
    await fireEvent.click(button('I checked — allow another send'));
    expect(piDelivery.uncertain).toBe(false);
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
  });

  it('blocks double taps and Enter until text delivery completes', async () => {
    let finish!: (value: CommandResult) => void;
    vi.mocked(relayStore.sendToAgent).mockImplementation(() => new Promise(resolve => { finish = resolve; }));
    await open(view);
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'queued' } });
    const send = button('Send text');
    await fireEvent.click(send); await fireEvent.click(send);
    const enter = button('Enter');
    expect(enter).toBeDisabled(); await fireEvent.click(enter);
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
    finish(receipt);
    await waitFor(() => expect(enter).toBeEnabled());
  });

  it.each(['dispatched_unknown','accepted'])('requires inspection after %s without replay', async phase => {
    const rendered = await open(view, { ...agent, status: 'idle' });
    vi.mocked(relayStore.sendToAgent).mockResolvedValue({ ...receipt, phase });
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'private uncertain' } });
    await fireEvent.click(button('Send text'));
    expect(button('Enter')).toBeDisabled();
    expect(button('I checked the terminal')).toBeDisabled();
    expect(button('Back to prompt and structured controls')).toBeEnabled();
    await fireEvent.click(button('Back to prompt and structured controls'));
    await waitFor(() => expect(button('Type in terminal')).toHaveFocus());
    expect(screen.getByLabelText('Prompt')).toBeVisible();
    expect(screen.getByLabelText('Prompt')).toBeEnabled();
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'unrelated normal draft' } });
    expect(button('Enter')).toBeDisabled();
    expect(button('Send prompt')).toBeDisabled();
    await fireEvent.click(button('Enter'));
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
    await fireEvent.click(button('Type in terminal'));
    await waitFor(() => expect(screen.getByLabelText('Terminal text')).toHaveFocus());
    expect(button('Enter')).toBeDisabled();
    expect(button('I checked the terminal')).toBeDisabled();
    expect(screen.getByLabelText('Terminal text')).toHaveValue('');
    await rendered.rerender({ frame: frame() });
    await fireEvent.click(button('I checked the terminal'));
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
    expect(JSON.stringify(localStorage)).not.toContain('private uncertain');
  });

  it.each((['disconnect', 'lock'] as const).flatMap(transition =>
    (['dispatched_unknown', 'accepted'] as const).flatMap(phase =>
      (['idle', 'question', 'approval'] as const).map(attention => ({ transition, phase, attention })))))('retains $phase guards for $attention across $transition until fresh inspection', async ({ transition, phase, attention }) => {
    const interaction: QuestionInteraction = { id: `recovery-${transition}-${phase}-${attention}`, kind: 'single_select', question: 'Choose a destination', options: [{ index: 0, label: 'North', selected: true }], can_go_back: true, can_chat: true };
    const current: Agent = { ...agent, status: attention === 'idle' ? 'idle' : 'blocked', attention_capable: attention !== 'idle', attention_kind: attention === 'idle' ? 'unknown' : attention, interaction: attention === 'question' ? interaction : undefined, options: ['Allow once', 'Deny'] };
    const rendered = await open(view, current);
    const stale = frame(current);
    await rendered.rerender({ frame: stale });
    vi.mocked(relayStore.sendToAgent).mockResolvedValue({ ...receipt, phase });
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'unconfirmed transient' } });
    await fireEvent.click(button('Send text'));
    if (transition === 'disconnect') relayStore.connections.set(new Map([[agent.relay_id, { ...connection, status: 'disconnected' }]] as never));
    else securityState.set({ locked: true } as never);
    await waitFor(() => expect(button('Type in terminal')).toBeVisible());
    if (transition === 'disconnect') relayStore.connections.set(new Map([[agent.relay_id, connection]] as never));
    else securityState.set({ locked: false } as never);
    await rendered.rerender({ frame: stale });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'normal recovery draft' } });
    expect(screen.getByText('Delivery is uncertain. You can edit drafts; reopen Type in terminal to inspect before sending.')).toBeVisible();
    expect(button('Enter')).toBeDisabled();
    expect(button('Send prompt')).toBeDisabled();
    if (attention === 'question') {
      expect(await screen.findByRole('radio', { name: 'North' })).toBeEnabled();
      for (const label of ['Submit', '← Previous', 'Chat about this']) expect(button(label)).toBeDisabled();
    } else if (attention === 'approval') expect(button('Allow once')).toBeDisabled();
    await fireEvent.click(button('Type in terminal'));
    await waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
    expect(screen.getByLabelText('Terminal text')).toHaveFocus();
    expect(screen.getByLabelText('Terminal text')).toHaveValue('');
    expect(button('I checked the terminal')).toBeDisabled();
    await rendered.rerender({ frame: frame(current) });
    expect(button('Enter')).toBeDisabled();
    expect(button('I checked the terminal')).toBeEnabled();
    await fireEvent.click(button('I checked the terminal'));
    expect(button('Enter')).toBeEnabled();
    await fireEvent.click(button('Back to prompt and structured controls'));
    expect(button('Enter')).toBeEnabled();
    if (attention === 'idle') expect(button('Send prompt')).toBeEnabled();
    else if (attention === 'question') expect(await screen.findByRole('button', { name: 'Submit' })).toBeEnabled();
    else expect(button('Allow once')).toBeEnabled();
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
    expect(JSON.stringify(localStorage)).not.toContain('unconfirmed transient');
  });

  it.each(['idle', 'question', 'approval'] as const)('rejects a post-uncertainty pre-lock read after unlock for %s without replay', async attention => {
    let now = 100;
    vi.spyOn(performance, 'now').mockImplementation(() => now);
    const interaction: QuestionInteraction = { id: 'pre-lock-read', kind: 'single_select', question: 'Choose a destination', options: [{ index: 0, label: 'North', selected: true }], can_go_back: true, can_chat: true };
    const current: Agent = { ...agent, status: attention === 'idle' ? 'idle' : 'blocked', attention_capable: attention !== 'idle', attention_kind: attention === 'idle' ? 'unknown' : attention, interaction: attention === 'question' ? interaction : undefined, options: ['Allow once', 'Deny'] };
    const answer = vi.spyOn(relayStore, 'answerQuestion').mockResolvedValue(receipt);
    const approve = vi.spyOn(relayStore, 'respond').mockResolvedValue(true);
    const navigate = vi.spyOn(relayStore, 'navigateQuestionPrevious').mockResolvedValue(receipt);
    const chat = vi.spyOn(relayStore, 'clarifyQuestion').mockResolvedValue(receipt);
    const rendered = await open(view, current);
    now = 110;
    vi.mocked(relayStore.sendToAgent).mockResolvedValue({ ...receipt, phase: 'dispatched_unknown' });
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'uncertain before lock' } });
    await fireEvent.click(button('Send text'));
    now = 120;
    const delayed = frame(current);
    now = 130;
    securityState.set({ locked: true } as never);
    await tick();
    expect(button('Type in terminal')).toBeVisible();
    now = 140;
    securityState.set({ locked: false } as never);
    await tick();
    now = 150;
    await rendered.rerender({ frame: { ...delayed, receivedAt: now } });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'draft before recovery observation' } });
    expect(button('Send prompt')).toBeDisabled();
    expect(button('Enter')).toBeDisabled();
    await fireEvent.click(button('Send prompt'));
    await fireEvent.click(button('Enter'));
    if (attention === 'question') {
      expect(await screen.findByRole('radio', { name: 'North' })).toBeEnabled();
      for (const label of ['Submit', '← Previous', 'Chat about this']) {
        expect(button(label)).toBeDisabled();
        await fireEvent.click(button(label));
      }
    } else if (attention === 'approval') {
      expect(button('Allow once')).toBeDisabled();
      await fireEvent.click(button('Allow once'));
    }
    expect(JSON.stringify(localStorage)).not.toContain('draft before recovery observation');
    await fireEvent.click(button('Type in terminal'));
    await waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
    expect(screen.getByLabelText('Terminal text')).toHaveValue('');
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'new transient after unlock' } });
    for (const label of ['Send text', 'Enter', 'I checked the terminal']) {
      expect(button(label)).toBeDisabled();
      await fireEvent.click(button(label));
    }
    now = 160;
    await rendered.rerender({ frame: { ...frame(current), readStartedAt: undefined } });
    expect(button('I checked the terminal')).toBeDisabled();
    expect(button('Enter')).toBeDisabled();
    now = 170;
    await rendered.rerender({ frame: { ...frame(current), inputTarget: targetRefForAgent({ ...current, generation: current.generation! + 1 }) } });
    expect(button('I checked the terminal')).toBeDisabled();
    now = 180;
    await rendered.rerender({ frame: frame(current) });
    expect(button('I checked the terminal')).toBeEnabled();
    expect(button('Send text')).toBeDisabled();
    expect(button('Enter')).toBeDisabled();
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
    await fireEvent.click(button('I checked the terminal'));
    expect(button('Send text')).toBeEnabled();
    expect(button('Enter')).toBeEnabled();
    await fireEvent.click(button('Back to prompt and structured controls'));
    expect(button('Enter')).toBeEnabled();
    if (attention === 'idle') expect(button('Send prompt')).toBeEnabled();
    else if (attention === 'question') expect(await screen.findByRole('button', { name: 'Submit' })).toBeEnabled();
    else expect(button('Allow once')).toBeEnabled();
    expect(relayStore.sendToAgent).toHaveBeenCalledExactlyOnceWith(current, { type: 'send_terminal_text', text: 'uncertain before lock' });
    for (const operation of [answer, approve, navigate, chat]) expect(operation).not.toHaveBeenCalled();
    for (const text of ['uncertain before lock', 'new transient after unlock', 'draft before recovery observation']) expect(JSON.stringify(localStorage)).not.toContain(text);
  });

  it.each((['disconnect', 'lock'] as const).flatMap(transition =>
    (['completed', 'dispatched_unknown'] as const).map(phase => ({ transition, phase }))))('keeps pending dispatch guarded across $transition before a late $phase result', async ({ transition, phase }) => {
    const current = { ...agent, status: 'idle' };
    const rendered = await open(view, current);
    let finish!: (result: CommandResult) => void;
    vi.mocked(relayStore.sendToAgent).mockImplementation(() => new Promise(resolve => { finish = resolve; }));
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'pending transient' } });
    await fireEvent.click(button('Send text'));
    if (transition === 'disconnect') relayStore.connections.set(new Map([[agent.relay_id, { ...connection, status: 'disconnected' }]] as never));
    else securityState.set({ locked: true } as never);
    await waitFor(() => expect(button('Type in terminal')).toBeVisible());
    if (transition === 'disconnect') relayStore.connections.set(new Map([[agent.relay_id, connection]] as never));
    else securityState.set({ locked: false } as never);
    await rendered.rerender({ frame: frame(current) });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'normal draft while pending' } });
    expect(button('Enter')).toBeDisabled();
    expect(button('Send prompt')).toBeDisabled();
    finish({ ...receipt, phase });
    if (phase === 'completed') await waitFor(() => expect(button('Send prompt')).toBeEnabled());
    else {
      await waitFor(() => expect(screen.getByText('Delivery is uncertain. You can edit drafts; reopen Type in terminal to inspect before sending.')).toBeVisible());
      expect(button('Send prompt')).toBeDisabled();
      await fireEvent.click(button('Type in terminal'));
      await waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
      expect(screen.getByLabelText('Terminal text')).toHaveFocus();
      expect(button('Enter')).toBeDisabled();
      expect(button('I checked the terminal')).toBeDisabled();
      await rendered.rerender({ frame: frame(current) });
      await fireEvent.click(button('I checked the terminal'));
      expect(button('Enter')).toBeEnabled();
    }
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
  });

  it.each(['uncertain', 'pending'] as const)('preserves $0 delivery guards through a whole-view remount', async state => {
    const current = { ...agent, status: 'idle' };
    const original = await open(view, current);
    let finish!: (result: CommandResult) => void;
    if (state === 'pending') vi.mocked(relayStore.sendToAgent).mockImplementation(() => new Promise(resolve => { finish = resolve; }));
    else vi.mocked(relayStore.sendToAgent).mockResolvedValue({ ...receipt, phase: 'dispatched_unknown' });
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'unmounted transient' } });
    await fireEvent.click(button('Send text'));
    original.unmount();
    const restored = view === 'Conversation' ? render(ConversationHistory, { agent: current }) : render(TerminalView, { agent: current, allAgents: [current], responding: new Set<string>() });
    await restored.rerender({ frame: frame(current) });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'normal draft after remount' } });
    expect(button('Send prompt')).toBeDisabled();
    expect(button('Enter')).toBeDisabled();
    if (state === 'pending') {
      expect(screen.getByText('Waiting for terminal input delivery.')).toBeVisible();
      finish({ ...receipt, phase: 'dispatched_unknown' });
    }
    await waitFor(() => expect(screen.getByText('Delivery is uncertain. You can edit drafts; reopen Type in terminal to inspect before sending.')).toBeVisible());
    await fireEvent.click(button('Type in terminal'));
    await waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
    expect(screen.getByLabelText('Terminal text')).toHaveValue('');
    expect(button('Enter')).toBeDisabled();
    expect(button('I checked the terminal')).toBeDisabled();
    await restored.rerender({ frame: frame(current) });
    await fireEvent.click(button('I checked the terminal'));
    expect(button('Enter')).toBeEnabled();
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
    expect(JSON.stringify(localStorage)).not.toContain('unmounted transient');
  });

  it('keeps hidden-value Enter behind the uncertainty fence after a safe return', async () => {
    relayStore.connections.set(new Map([[agent.relay_id, { ...connection, capabilities: ['manual_terminal_input', 'secret_input'] }]] as never));
    const secret = vi.spyOn(relayStore, 'sendSecret').mockResolvedValue(receipt);
    const rendered = await open(view, { ...agent, status: 'idle' });
    vi.mocked(relayStore.sendToAgent).mockResolvedValue({ ...receipt, phase: 'accepted' });
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'uncertain' } });
    await fireEvent.click(button('Send text'));
    await fireEvent.click(button('Back to prompt and structured controls'));
    await rendered.rerender({ frame: frame(agent, true) });
    await fireEvent.input(screen.getByLabelText('Value for the hidden terminal prompt'), { target: { value: 'transient hidden value' } });
    expect(button('Send hidden value')).toBeDisabled();
    await fireEvent.keyDown(screen.getByLabelText('Value for the hidden terminal prompt'), { key: 'Enter' });
    expect(secret).not.toHaveBeenCalled();
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
  });

  it.each((['cwd', 'agent'] as const).flatMap(field =>
    (['pending-completed', 'pending-unknown', 'uncertain'] as const).flatMap(state =>
      (['idle', 'question', 'approval'] as const).map(attention => ({ field, state, attention })))))('retains $state guards for $attention across same-target $field changes', async ({ field, state, attention }) => {
    const interaction: QuestionInteraction = { id: 'metadata-change', kind: 'single_select', question: 'Choose a destination', options: [{ index: 0, label: 'North', selected: true }], can_go_back: true, can_chat: true };
    const current: Agent = { ...agent, status: attention === 'idle' ? 'idle' : 'blocked', attention_capable: attention !== 'idle', attention_kind: attention === 'idle' ? 'unknown' : attention, interaction: attention === 'question' ? interaction : undefined, options: ['Allow once', 'Deny'] };
    const rendered = await open(view, current);
    let finish!: (result: CommandResult) => void;
    if (state === 'uncertain') vi.mocked(relayStore.sendToAgent).mockResolvedValue({ ...receipt, phase: 'dispatched_unknown' });
    else vi.mocked(relayStore.sendToAgent).mockImplementation(() => new Promise(resolve => { finish = resolve; }));
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'metadata-change transient' } });
    await fireEvent.click(button('Send text'));
    const changed = { ...current, [field]: field === 'agent' ? 'claude' : '/changed-directory' };
    expect(targetRefForAgent(changed)).toEqual(targetRefForAgent(current));
    await rendered.rerender({ agent: changed, frame: frame(changed) });
    await waitFor(() => expect(button('Type in terminal')).toBeVisible());
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'normal metadata draft' } });
    expect(button('Enter')).toBeDisabled();
    expect(button('Send prompt')).toBeDisabled();
    if (attention === 'question') for (const label of ['Submit', '← Previous', 'Chat about this']) expect(await screen.findByRole('button', { name: label })).toBeDisabled();
    else if (attention === 'approval') expect(button('Allow once')).toBeDisabled();
    if (state !== 'uncertain') finish({ ...receipt, phase: state === 'pending-completed' ? 'completed' : 'dispatched_unknown' });
    if (state === 'pending-completed') await waitFor(() => expect(button('Enter')).toBeEnabled());
    else {
      await waitFor(() => expect(screen.getByText('Delivery is uncertain. You can edit drafts; reopen Type in terminal to inspect before sending.')).toBeVisible());
      expect(button('Enter')).toBeDisabled();
      await fireEvent.click(button('Type in terminal'));
      await waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
      expect(button('I checked the terminal')).toBeDisabled();
      await rendered.rerender({ frame: frame(changed) });
      expect(button('I checked the terminal')).toBeEnabled();
      expect(button('Enter')).toBeDisabled();
      await fireEvent.click(button('I checked the terminal'));
      expect(button('Enter')).toBeEnabled();
    }
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
    expect(JSON.stringify(localStorage)).not.toContain('metadata-change transient');
  });

  it('rejects late pre-send observations and uncorrelated frames before explicit inspection', async () => {
    const current = { ...agent, status: 'idle' };
    const rendered = await open(view, current);
    const beforeSend = frame(current);
    vi.mocked(relayStore.sendToAgent).mockResolvedValue({ ...receipt, phase: 'dispatched_unknown' });
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'uncertain observation' } });
    await fireEvent.click(button('Send text'));
    for (const readStartedAt of [beforeSend.readStartedAt, undefined]) {
      await rendered.rerender({ frame: { ...frame(current), readStartedAt } });
      expect(button('I checked the terminal')).toBeDisabled();
      await fireEvent.click(button('I checked the terminal'));
      expect(button('Enter')).toBeDisabled();
    }
    await fireEvent.click(button('Back to prompt and structured controls'));
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'unrelated draft' } });
    expect(button('Send prompt')).toBeDisabled();
    expect(button('Enter')).toBeDisabled();
    await fireEvent.click(button('Type in terminal'));
    await waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
    await rendered.rerender({ frame: frame(current) });
    expect(button('I checked the terminal')).toBeEnabled();
    expect(button('Enter')).toBeDisabled();
    await fireEvent.click(button('I checked the terminal'));
    expect(button('Enter')).toBeEnabled();
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
  });

  it.each(['generation','terminal_id','agent_session_id','server_session_id','agent','relay_id'] as const)('invalidates mode and late callbacks on %s replacement', async field => {
    const rendered = await open(view);
    let finish!: (value: CommandResult) => void;
    vi.mocked(relayStore.sendToAgent).mockImplementation(() => new Promise(resolve => { finish = resolve; }));
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'old transient' } });
    await fireEvent.click(button('Send text'));
    await rendered.rerender({ agent: { ...agent, [field]: field === 'generation' ? 2 : 'replacement' } });
    finish(receipt);
    await waitFor(() => expect(button('Type in terminal')).toBeVisible());
    expect(screen.queryByLabelText('Terminal text')).not.toBeVisible();
    expect(screen.getByLabelText('Prompt')).toHaveValue('');
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
  });

  it('invalidates on disconnect and lock, requiring a fresh post-recovery frame', async () => {
    const rendered = await open(view);
    relayStore.connections.set(new Map([[agent.relay_id, { ...connection, status: 'disconnected' }]] as never));
    await waitFor(() => expect(button('Type in terminal')).toBeVisible());
    relayStore.connections.set(new Map([[agent.relay_id, connection]] as never));
    await fireEvent.click(button('Type in terminal'));
    await waitFor(() => expect(button('Enter')).toBeDisabled());
    await rendered.rerender({ frame: frame() });
    expect(button('Enter')).toBeEnabled();
    securityState.set({ locked: true } as never);
    await waitFor(() => expect(button('Type in terminal')).toBeVisible());
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
  });

  it.each([true, undefined])('keeps hidden/unknown input %s transient across mode and echo changes', async noEcho => {
    const rendered = await open(view);
    await rendered.rerender({ frame: { ...frame(), noEcho } });
    expect(screen.getByLabelText('Terminal text')).toHaveAttribute('type', 'password');
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'transient-secret' } });
    await rendered.rerender({ frame: frame() });
    expect(JSON.stringify(localStorage)).not.toContain('transient-secret');
    await fireEvent.click(button('Back to prompt and structured controls'));
    expect(screen.getByLabelText('Prompt')).toHaveValue('');
    expect(JSON.stringify(localStorage)).not.toContain('transient-secret');
  });

  it('does not infer manual input authorization from an untagged frame', async () => {
    const rendered = await open(view);
    await rendered.rerender({ frame: { ...frame(), inputTarget: undefined } });
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'unbound' } });
    expect(button('Send text')).toBeDisabled();
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
  });

  it.each(['lock', 'disconnect'] as const)('requires a correlated recovery read before accepting deltas after %s', async transition => {
    let now = 100;
    vi.spyOn(performance, 'now').mockImplementation(() => now);
    const rendered = await open(view);
    now = 110;
    const beforeRecovery = frame();
    await rendered.rerender({ frame: beforeRecovery });
    now = 120;
    if (transition === 'lock') securityState.set({ locked: true } as never);
    else relayStore.connections.set(new Map([[agent.relay_id, { ...connection, status: 'disconnected' }]] as never));
    await tick();
    now = 130;
    if (transition === 'lock') securityState.set({ locked: false } as never);
    else relayStore.connections.set(new Map([[agent.relay_id, connection]] as never));
    await tick();
    now = 140;
    const delta = () => ({ ...frame(), readStartedAt: undefined, target: undefined, noEcho: undefined });
    await rendered.rerender({ frame: delta() });
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'unverified recovery draft' } });
    expect(JSON.stringify(localStorage)).not.toContain('unverified recovery draft');
    await fireEvent.click(button('Type in terminal'));
    await waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'transient recovery text' } });
    for (const candidate of [delta(), { ...beforeRecovery, receivedAt: now }, { ...frame(), inputTarget: targetRefForAgent({ ...agent, generation: agent.generation! + 1 }) }]) {
      await rendered.rerender({ frame: candidate });
      for (const label of ['Send text', 'Enter']) {
        expect(button(label)).toBeDisabled();
        await fireEvent.click(button(label));
      }
      expect(relayStore.sendToAgent).not.toHaveBeenCalled();
    }
    now = 150;
    await rendered.rerender({ frame: frame() });
    await tick();
    expect(button('Send text')).toBeEnabled();
    expect(button('Enter')).toBeEnabled();
    now = 160;
    await rendered.rerender({ frame: delta() });
    expect(button('Send text')).toBeEnabled();
    await fireEvent.click(button('Send text'));
    expect(relayStore.sendToAgent).toHaveBeenCalledExactlyOnceWith(agent, { type: 'send_terminal_text', text: 'transient recovery text' });
    await fireEvent.click(button('Enter'));
    expect(relayStore.sendToAgent).toHaveBeenLastCalledWith(agent, { type: 'send_keys', keys: ['Enter'], activity_label: 'Manual Enter' });
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(2);
    expect(JSON.stringify(localStorage)).not.toContain('transient recovery text');
  });

  it('keeps exact-target manual input available on unknown-echo deltas without allowing draft persistence', async () => {
    const rendered = await open(view);
    await rendered.rerender({ frame: { ...frame(), readStartedAt: undefined, target: undefined, noEcho: undefined } });
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'unknown-echo transient' } });
    expect(button('Send text')).toBeEnabled();
    await fireEvent.click(button('Send text'));
    expect(relayStore.sendToAgent).toHaveBeenCalledExactlyOnceWith(agent, { type: 'send_terminal_text', text: 'unknown-echo transient' });
    expect(JSON.stringify(localStorage)).not.toContain('unknown-echo transient');
  });

  it('rejects multiline paste before the browser can silently strip newlines', async () => {
    await open(view);
    const input = screen.getByLabelText('Terminal text');
    const paste = new Event('paste', { bubbles: true, cancelable: true });
    Object.defineProperty(paste, 'clipboardData', { value: { getData: () => 'first\nsecond' } });
    await fireEvent(input, paste);
    expect(paste.defaultPrevented).toBe(true);
    expect(input).toHaveValue('');
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
  });

  it('leaves approval identity and choices intact after manual text delivery', async () => {
    const current = { ...agent, attention_capable: true, attention_kind: 'approval' as const, options: ['Allow once', 'Deny'], event_id: 'approval-identity' };
    await open(view, current);
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'explicit manual text' } });
    await fireEvent.click(button('Send text'));
    await fireEvent.click(button('Back to prompt and structured controls'));
    expect(button('Allow once')).toBeVisible();
    expect(current.event_id).toBe('approval-identity');
    expect(current.status).toBe('blocked');
    expect(relayStore.sendToAgent).toHaveBeenCalledExactlyOnceWith(current, { type: 'send_terminal_text', text: 'explicit manual text' });
  });

  it('supports sessionless targets without enabling draft persistence', async () => {
    const current = { ...agent, agent_session_id: undefined };
    await open(view, current);
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'sessionless' } });
    await fireEvent.click(button('Send text'));
    expect(relayStore.sendToAgent).toHaveBeenCalledWith(current, { type: 'send_terminal_text', text: 'sessionless' });
    expect(JSON.stringify(localStorage)).not.toContain('sessionless');
  });

  it.each(['question', 'approval'] as const)('keeps %s drafts visible but mutations guarded after uncertain exit', async attention_kind => {
    const interaction: QuestionInteraction = { id: 'pending', kind: 'single_select', question: 'Choose a destination', options: [{ index: 0, label: 'North', selected: true }], can_go_back: true, can_chat: true };
    const current = { ...agent, attention_capable: true, attention_kind, interaction: attention_kind === 'question' ? interaction : undefined, options: ['Allow once', 'Deny'] };
    const answer = vi.spyOn(relayStore, 'answerQuestion').mockResolvedValue(receipt);
    const approve = vi.spyOn(relayStore, 'respond').mockResolvedValue(true);
    const navigate = vi.spyOn(relayStore, 'navigateQuestionPrevious').mockResolvedValue(receipt);
    const chat = vi.spyOn(relayStore, 'clarifyQuestion').mockResolvedValue(receipt);
    await open(view, current);
    vi.mocked(relayStore.sendToAgent).mockResolvedValue({ ...receipt, phase: 'accepted' });
    await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'uncertain' } });
    await fireEvent.click(button('Send text'));
    await fireEvent.click(button('Back to prompt and structured controls'));
    if (attention_kind === 'question') {
      expect(await screen.findByRole('radio', { name: 'North' })).toBeEnabled();
      for (const label of ['Submit', '← Previous', 'Chat about this']) {
        expect(button(label)).toBeDisabled();
        await fireEvent.click(button(label));
      }
      await fireEvent.submit(screen.getByRole('form', { name: 'Choose a destination' }));
      expect(screen.getByText('Inspect terminal delivery before sending.')).toBeVisible();
    } else {
      expect(button('Allow once')).toBeVisible();
      expect(button('Allow once')).toBeDisabled();
      await fireEvent.click(button('Allow once'));
    }
    for (const operation of [answer, approve, navigate, chat]) expect(operation).not.toHaveBeenCalled();
    expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
  });

  it.each(['single_select','multi_select'] as const)('recovers %s and genuine Other controls without clearing the interaction', async kind => {
    const interaction: QuestionInteraction = { id: 'genuine', kind, question: 'Choose a destination', options: [{ index: 0, label: 'North' }, { index: 1, label: 'South' }], other: { label: 'Other', placeholder: 'Other answer' }, submit_label: 'Submit' };
    const current = { ...agent, attention_capable: true, attention_kind: 'question' as const, interaction };
    const rendered = view === 'Conversation' ? render(ConversationHistory, { agent: current }) : render(TerminalView, { agent: current, allAgents: [current], responding: new Set<string>() });
    expect(screen.getByLabelText('Prompt')).toBeEnabled();
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'question-local draft' } });
    await fireEvent.click(button('Type in terminal'));
    await waitFor(() => expect(screen.getByLabelText('Terminal text')).toHaveFocus());
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
    await fireEvent.click(button('Back to prompt and structured controls'));
    expect(await screen.findByRole('group', { name: 'Choose a destination' })).toBeVisible();
    expect(screen.getByRole(kind === 'multi_select' ? 'checkbox' : 'radio', { name: 'Other' })).toBeVisible();
    expect(current.interaction.id).toBe('genuine');
    expect(screen.getByLabelText('Prompt')).toHaveValue('question-local draft');
    await waitFor(() => expect(button('Type in terminal')).toHaveFocus());
    rendered.unmount();
  });

  it('never offers manual controls to a reader', () => {
    if (view === 'Conversation') render(ConversationHistory, { agent, readOnly: true });
    else render(TerminalView, { agent, readOnly: true, allAgents: [agent], responding: new Set<string>() });
    expect(screen.queryByRole('button', { name: 'Type in terminal' })).not.toBeInTheDocument();
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
  });
});
