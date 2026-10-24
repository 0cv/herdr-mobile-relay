import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import ConversationHistory from '$components/ConversationHistory.svelte';
import TerminalView from '$components/TerminalView.svelte';
import { relayStore } from '$lib/store';
import { securityState } from '$lib/security';
import { targetRefForAgent } from '$lib/resource-id';
import { loadPromptDraft } from '$lib/prompt-drafts';
import type { Agent, CommandResult, QuestionInteraction } from '$lib/types';

const agent: Agent = { relay_id: 'conversation-controls', relay_label: 'Synthetic', raw_pane_id: 'pane', pane_id: 'conversation-controls::pane', server_session_id: 'primary', terminal_id: 'terminal', generation: 1, agent_session_id: ' exact session/界 ', agent: 'pi', project: 'Synthetic', status: 'idle' };
const connection = { status: 'connected', capabilities: ['manual_terminal_input', 'secret_input'], inventory: { state: 'ready', stale: false } };
const receipt = { type: 'command_result', request_id: 'controls', ok: true, phase: 'completed' } as CommandResult;
const frame = (current = agent, noEcho: boolean | undefined = false) => ({ paneId: current.pane_id, target: targetRefForAgent(current), inputTarget: targetRefForAgent(current), content: 'RAW TERMINAL PREVIEW\n> ', format: 'plain' as const, receivedAt: performance.now(), readStartedAt: performance.now(), noEcho });
const button = (name: string) => screen.getByRole('button', { name });
const conversation = (current = agent) => render(ConversationHistory, { agent: current });
const terminal = (current = agent) => render(TerminalView, { agent: current, allAgents: [current], responding: new Set<string>() });

beforeEach(() => {
  localStorage.clear();
  agent.generation! += 1;
  securityState.set({ locked: false } as never);
  relayStore.connections.set(new Map([[agent.relay_id, connection]] as never));
  for (const method of ['readPane', 'watchPane', 'unwatchPane'] as const) vi.spyOn(relayStore, method).mockImplementation(() => {});
  vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({ available: true, entries: [], hasMore: false, total: 0, reason: '' });
  vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue(receipt);
  vi.spyOn(relayStore, 'answerQuestion').mockResolvedValue(receipt);
  vi.spyOn(relayStore, 'respond').mockResolvedValue(true);
  vi.spyOn(relayStore, 'navigateQuestionPrevious').mockResolvedValue(receipt);
  vi.spyOn(relayStore, 'clarifyQuestion').mockResolvedValue(receipt);
});
afterEach(() => { vi.restoreAllMocks(); relayStore.connections.set(new Map()); securityState.set({ locked: false } as never); });

function uncluttered() {
  for (const name of ['Type in terminal', 'Enter', 'Esc', 'Tab', 'Ctrl', 'Send text', 'I checked the terminal']) expect(screen.queryByRole('button', { name })).not.toBeInTheDocument();
  expect(screen.queryByLabelText('Terminal text')).not.toBeInTheDocument();
  expect(screen.queryByText('Current terminal context')).not.toBeInTheDocument();
  expect(screen.queryByText(/RAW TERMINAL PREVIEW/)).not.toBeInTheDocument();
}

for (const status of ['working', 'blocked', 'idle']) it(`Conversation keeps ${status} drafts editable without terminal clutter`, async () => {
  const current = { ...agent, status };
  const view = conversation(current);
  await view.rerender({ frame: frame(current) });
  uncluttered();
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'editable reply' } });
  expect(screen.getByLabelText('Prompt')).toHaveValue('editable reply');
  if (status === 'blocked') {
    expect(button('Send prompt')).toBeDisabled();
    expect(button('Attach files')).toBeDisabled();
    await fireEvent.keyDown(screen.getByLabelText('Prompt'), { key: 'Enter', ctrlKey: true });
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
  } else {
    await fireEvent.click(button('Send prompt'));
    expect(relayStore.sendToAgent).toHaveBeenCalledExactlyOnceWith(current, { type: 'submit_prompt', text: 'editable reply' });
  }
});

it('keeps genuine assistant and tool code blocks, not raw terminal output', async () => {
  vi.mocked(relayStore.getConversationHistory).mockResolvedValue({ available: true, entries: [{ id: 'assistant-code', timestamp: '2026-09-02T10:00:00Z', role: 'assistant', text: '```ts\nconst answer = 42;\n```', tools: [{ name: 'read', input: '```json\n{"file":"test.ts"}\n```', output: '```ts\nexport const kept = true;\n```' }] }], hasMore: false, total: 1, reason: '' });
  const view = conversation();
  await view.rerender({ frame: frame() });
  expect(await screen.findByText('const answer = 42;')).toBeVisible();
  expect(document.querySelector('.conversation-tools details')).toBeNull();
  await fireEvent.click(button('Full history'));
  document.querySelector('.conversation-tools details')!.setAttribute('open', '');
  expect(screen.getByText(/export const kept = true;/)).toBeVisible();
  uncluttered();
});

for (const kind of ['single_select', 'multi_select'] as const) it(`Conversation retains genuine ${kind} and Other answers`, async () => {
  const interaction: QuestionInteraction = { id: 'genuine', kind, question: 'Choose a destination', options: [{ index: 0, label: 'North' }, { index: 1, label: 'South' }], other: { label: 'Other', placeholder: 'Other answer' }, submit_label: 'Submit' };
  const current = { ...agent, status: 'blocked', attention_capable: true, attention_kind: 'question' as const, interaction };
  conversation(current);
  const other = await screen.findByRole(kind === 'single_select' ? 'radio' : 'checkbox', { name: 'Other' });
  await fireEvent.click(other);
  await fireEvent.input(screen.getByPlaceholderText('Other answer'), { target: { value: 'East' } });
  await fireEvent.click(button('Submit'));
  expect(relayStore.answerQuestion).toHaveBeenCalledExactlyOnceWith(current, interaction, { selected: new Set(), otherSelected: true, otherText: 'East' });
  expect(relayStore.sendToAgent).not.toHaveBeenCalled();
  uncluttered();
});

it('Conversation retains real approval choices with exact routing', async () => {
  const current = { ...agent, status: 'blocked', attention_capable: true, attention_kind: 'approval' as const, options: ['Allow once', 'Deny'], event_id: 'approval' };
  conversation(current);
  await fireEvent.click(button('Allow once'));
  expect(relayStore.respond).toHaveBeenCalledExactlyOnceWith(current, 0, 2, 'Allow once');
  uncluttered();
});

const cases = (['pending-completed', 'pending-unknown', 'accepted', 'dispatched_unknown'] as const).flatMap(state =>
  (['idle', 'question', 'approval'] as const).flatMap(attention =>
    (['view', 'cwd', 'agent', 'disconnect', 'lock'] as const).map(transition => ({ state, attention, transition }))));
it.each(cases)('keeps $state $attention delivery fenced in Conversation across $transition', async ({ state, attention, transition }) => {
  const interaction: QuestionInteraction = { id: 'cross-view', kind: 'single_select', question: 'Choose a destination', options: [{ index: 0, label: 'North', selected: true }], can_go_back: true, can_chat: true };
  let current: Agent = { ...agent, status: attention === 'idle' ? 'idle' : 'blocked', attention_capable: attention !== 'idle', attention_kind: attention === 'idle' ? 'unknown' : attention, interaction: attention === 'question' ? interaction : undefined, options: ['Allow once', 'Deny'] };
  const original = terminal(current);
  await fireEvent.click(button('Type in terminal'));
  await waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
  await original.rerender({ frame: frame(current) });
  let finish!: (value: CommandResult) => void;
  if (state.startsWith('pending')) vi.mocked(relayStore.sendToAgent).mockImplementation(() => new Promise(resolve => { finish = resolve; }));
  else vi.mocked(relayStore.sendToAgent).mockResolvedValue({ ...receipt, phase: state });
  await fireEvent.input(screen.getByLabelText('Terminal text'), { target: { value: 'private terminal attempt' } });
  await fireEvent.click(button('Send text'));
  original.unmount();
  const history = conversation(current);
  const stale = frame(current);
  await history.rerender({ frame: stale });
  if (transition === 'cwd' || transition === 'agent') {
    const changed = { ...current, [transition]: transition === 'cwd' ? '/new-directory' : 'claude' };
    expect(targetRefForAgent(changed)).toEqual(targetRefForAgent(current));
    current = changed;
    await history.rerender({ agent: current });
  } else if (transition === 'disconnect' || transition === 'lock') {
    if (transition === 'disconnect') relayStore.connections.set(new Map([[agent.relay_id, { ...connection, status: 'disconnected' }]] as never));
    else securityState.set({ locked: true } as never);
    await tick();
    if (transition === 'disconnect') relayStore.connections.set(new Map([[agent.relay_id, connection]] as never));
    else securityState.set({ locked: false } as never);
    await tick();
    await history.rerender({ frame: stale });
  }
  uncluttered();
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'normal draft while fenced' } });
  expect(screen.getByLabelText('Prompt')).toBeEnabled();
  expect(button('Send prompt')).toBeDisabled();
  expect(button('Attach files')).toBeDisabled();
  await fireEvent.keyDown(screen.getByLabelText('Prompt'), { key: 'Enter', ctrlKey: true });
  if (attention === 'question') {
    expect(await screen.findByRole('radio', { name: 'North' })).toBeEnabled();
    for (const label of ['Submit', '← Previous', 'Chat about this']) {
      expect(button(label)).toBeDisabled();
      await fireEvent.click(button(label));
    }
    await fireEvent.submit(screen.getByRole('form', { name: 'Choose a destination' }));
    expect(screen.getByText('Inspect terminal delivery before sending.')).toBeVisible();
  } else if (attention === 'approval') {
    expect(button('Allow once')).toBeDisabled();
    await fireEvent.click(button('Allow once'));
  }
  if (state.startsWith('pending')) {
    expect(screen.getByText('Waiting for terminal input delivery.')).toBeVisible();
    finish({ ...receipt, phase: state === 'pending-completed' ? 'completed' : 'dispatched_unknown' });
  }
  if (state === 'pending-completed') {
    await waitFor(() => expect(attention === 'idle' ? button('Send prompt') : attention === 'approval' ? button('Allow once') : button('Submit')).toBeEnabled());
  } else {
    await waitFor(() => expect(screen.getByText('Delivery is uncertain. You can edit drafts; open Terminal and use Type in terminal to inspect before sending.')).toBeVisible());
    await history.rerender({ frame: frame(current) });
    expect(button('Send prompt')).toBeDisabled();
    history.unmount();
    const restored = terminal(current);
    await restored.rerender({ frame: stale });
    await fireEvent.click(button('Type in terminal'));
    await waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
    expect(screen.getByLabelText('Terminal text')).toHaveValue('');
    expect(button('I checked the terminal')).toBeDisabled();
    await restored.rerender({ frame: { ...frame(current), readStartedAt: undefined } });
    expect(button('I checked the terminal')).toBeDisabled();
    await restored.rerender({ frame: frame(current) });
    expect(button('I checked the terminal')).toBeEnabled();
    expect(button('Enter')).toBeDisabled();
    await fireEvent.click(button('I checked the terminal'));
    restored.unmount();
    conversation(current);
    await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'draft after inspection' } });
    if (attention === 'question') expect(await screen.findByRole('button', { name: 'Submit' })).toBeEnabled();
    else expect(attention === 'idle' ? button('Send prompt') : button('Allow once')).toBeEnabled();
    uncluttered();
  }
  expect(relayStore.sendToAgent).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({ pane_id: agent.pane_id, generation: agent.generation }), { type: 'send_terminal_text', text: 'private terminal attempt' });
  for (const operation of [relayStore.answerQuestion, relayStore.respond, relayStore.navigateQuestionPrevious, relayStore.clarifyQuestion]) expect(operation).not.toHaveBeenCalled();
  expect(JSON.stringify(localStorage)).not.toContain('private terminal attempt');
});

it('does not transfer or clear an unresolved delivery when Conversation changes targets', async () => {
  const original = terminal();
  await fireEvent.click(button('Type in terminal'));
  await waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
  await original.rerender({ frame: frame() });
  vi.mocked(relayStore.sendToAgent).mockResolvedValue({ ...receipt, phase: 'accepted' });
  await fireEvent.click(button('Enter'));
  original.unmount();
  const history = conversation();
  const replacement = { ...agent, generation: agent.generation! + 1 };
  await history.rerender({ agent: replacement, frame: frame(replacement) });
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'new target draft' } });
  expect(button('Send prompt')).toBeEnabled();
  await history.rerender({ agent, frame: frame() });
  expect(button('Send prompt')).toBeDisabled();
  expect(screen.getByRole('alert')).toHaveTextContent('Delivery is uncertain.');
  expect(relayStore.sendToAgent).toHaveBeenCalledExactlyOnceWith(agent, { type: 'send_keys', keys: ['Enter'], activity_label: 'Manual Enter' });
});

it.each([true, undefined])('keeps Conversation hidden/unknown %s edits transient', async noEcho => {
  const view = conversation();
  await view.rerender({ frame: { ...frame(), noEcho } });
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'private conversation draft' } });
  await view.rerender({ frame: frame() });
  expect(loadPromptDraft(agent)).not.toContain('private conversation draft');
  uncluttered();
});

it.each(['pending', 'uncertain'] as const)('keeps Conversation hidden-value Enter behind a %s delivery fence', async state => {
  const secret = vi.spyOn(relayStore, 'sendSecret').mockResolvedValue(receipt);
  const original = terminal();
  await fireEvent.click(button('Type in terminal'));
  await waitFor(() => expect(screen.getByLabelText('Terminal text')).toBeVisible());
  await original.rerender({ frame: frame() });
  let finish!: (value: CommandResult) => void;
  if (state === 'pending') vi.mocked(relayStore.sendToAgent).mockImplementation(() => new Promise(resolve => { finish = resolve; }));
  else vi.mocked(relayStore.sendToAgent).mockResolvedValue({ ...receipt, phase: 'accepted' });
  await fireEvent.click(button('Enter'));
  original.unmount();
  const history = conversation();
  await history.rerender({ frame: frame(agent, true) });
  const input = screen.getByLabelText('Value for the hidden terminal prompt');
  await fireEvent.input(input, { target: { value: 'private hidden reply' } });
  expect(button('Send hidden value')).toBeDisabled();
  await fireEvent.keyDown(input, { key: 'Enter' });
  expect(secret).not.toHaveBeenCalled();
  expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
  expect(JSON.stringify(localStorage)).not.toContain('private hidden reply');
  if (state === 'pending') finish({ ...receipt, phase: 'accepted' });
  uncluttered();
});

it('does not offer Conversation mutations to a reader', () => {
  render(ConversationHistory, { agent, readOnly: true });
  expect(screen.getByLabelText('Prompt')).toBeDisabled();
  uncluttered();
});
