import { fireEvent, render, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import PromptComposer from '$components/PromptComposer.svelte';
import { relayStore } from '$lib/store';
import { AttachmentBatchController, type AttachmentRef } from '$lib/attachments';
import { clearPromptDraft, loadPromptDraft } from '$lib/prompt-drafts';
import type { Agent, RelayConnectionView, TerminalFrame } from '$lib/types';

const agent: Agent = { relay_id: 'footer', relay_label: 'Footer', raw_pane_id: 'pane', pane_id: 'footer::pane', server_session_id: 'server', terminal_id: 'terminal', generation: 1, agent_session_id: 'session', agent: 'pi', status: 'idle', cwd: '/fixture' };
const healthy = { status: 'connected', protocol: 3, inventory: { state: 'ready', stale: false }, capabilities: ['secret_input'] } as RelayConnectionView;
const hiddenInputFrame = (target: Agent): TerminalFrame => ({ paneId: target.pane_id, content: '', noEcho: true, format: 'plain', receivedAt: performance.now() });
async function send() {
  await fireEvent.input(screen.getByLabelText('Prompt'), { target: { value: 'New prompt' } });
  await fireEvent.submit(screen.getByRole('form', { name: 'Send a prompt' }));
}

beforeEach(() => {
  vi.useFakeTimers();
  relayStore.connections.set(new Map([[agent.relay_id, healthy]]) as never);
  vi.spyOn(relayStore, 'readPane').mockImplementation(() => {});
  vi.spyOn(relayStore, 'showToast').mockImplementation(() => {});
  clearPromptDraft(agent);
});
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); relayStore.connections.set(new Map()); localStorage.clear(); });

it.each([
  ['codex', 'working', 'Codex'],
  ['Pi Coding Agent', 'working', 'Pi'],
  ['omp', 'working', 'Oh My Pi'],
  ['opencode', 'working', 'OpenCode'],
  ['pi', 'idle', ''],
  ['pi', 'done', ''],
  ['claude', 'working', ''],
])('uses the right prompt wording for %s (%s)', async (kind, status, steeringAgent) => {
  const target = { ...agent, agent: kind, status };
  vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue({ type: 'command_result', request_id: 'sent', ok: true });
  render(PromptComposer, { agent: target });
  const prompt = screen.getByLabelText('Prompt');
  expect(prompt).toHaveValue('');
  expect(prompt).toHaveAttribute('placeholder', steeringAgent ? 'Joins this task' : 'Type a reply…');
  expect(prompt).toHaveAccessibleDescription(steeringAgent ? `${steeringAgent} is working. Send adds this to its current task.` : '');
  await send();
  expect(relayStore.sendToAgent).toHaveBeenCalledExactlyOnceWith(target, { type: 'submit_prompt', text: 'New prompt' });
  expect(relayStore.showToast).toHaveBeenCalledWith(steeringAgent ? `Sent. ${steeringAgent} will read it during its current task.` : 'Prompt sent.');
});

it.each([
  ['/new', false],
  ['  /skill:example/nested arg', false],
  ['// TODO: handle EOF', true],
  ['/ path', true],
])('uses the right busy-send wording for draft %j', async (text, steer) => {
  const target = { ...agent, status: 'working' };
  vi.spyOn(relayStore, 'loadSlashCommands').mockResolvedValue({ commands: [], truncated: false });
  vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue({ type: 'command_result', request_id: 'sent', ok: true });
  render(PromptComposer, { agent: target });
  const prompt = screen.getByLabelText('Prompt');
  await fireEvent.input(prompt, { target: { value: text } });
  expect(prompt).toHaveAttribute('placeholder', steer ? 'Joins this task' : 'Type a reply…');
  expect(prompt).toHaveAccessibleDescription(steer ? 'Pi is working. Send adds this to its current task.' : '');
  await fireEvent.submit(screen.getByRole('form', { name: 'Send a prompt' }));
  expect(relayStore.sendToAgent).toHaveBeenCalledExactlyOnceWith(target, { type: 'submit_prompt', text });
  expect(relayStore.showToast).toHaveBeenCalledWith(steer ? 'Sent. Pi will read it during its current task.' : 'Prompt sent.');
});

it('hides busy-send guidance and blocks submission while an attachment uploads', async () => {
  let finish!: (attachments: AttachmentRef[]) => void;
  vi.spyOn(AttachmentBatchController.prototype, 'upload').mockReturnValue(new Promise(resolve => { finish = resolve; }));
  const dispatch = vi.spyOn(relayStore, 'sendToAgent');
  const { container } = render(PromptComposer, { agent: { ...agent, agent: 'codex', status: 'working' } });
  const prompt = screen.getByLabelText('Prompt');
  const description = 'Codex is working. Send adds this to its current task.';
  expect(prompt).toHaveAccessibleDescription(description);
  await fireEvent.change(container.querySelector('input[type="file"]')!, {
    target: { files: [new File(['fixture'], 'fixture.png', { type: 'image/png' })] },
  });
  expect(prompt).toHaveAttribute('placeholder', 'Type a reply…');
  expect(prompt).not.toHaveAccessibleDescription();
  await send();
  expect(screen.getByRole('button', { name: 'Send prompt' })).toBeDisabled();
  expect(dispatch).not.toHaveBeenCalled();
  finish([]);
  await vi.advanceTimersByTimeAsync(0);
  expect(prompt).toHaveAccessibleDescription(description);
  expect(screen.getByRole('button', { name: 'Send prompt' })).toBeEnabled();
});

it('omits the busy hint without a stable input target', async () => {
  render(PromptComposer, { agent: { ...agent, status: 'working', terminal_id: '' } });
  const prompt = screen.getByLabelText('Prompt');
  await fireEvent.input(prompt, { target: { value: 'Draft' } });
  expect(screen.getByRole('button', { name: 'Send prompt' })).toBeDisabled();
  expect(prompt).toHaveAttribute('placeholder', 'Type a reply…');
  expect(prompt).not.toHaveAccessibleDescription();
  const dispatch = vi.spyOn(relayStore, 'sendToAgent');
  await fireEvent.submit(screen.getByRole('form', { name: 'Send a prompt' }));
  expect(dispatch).not.toHaveBeenCalled();
});

it.each([
  ['disconnected relay', { ...healthy, status: 'disconnected' }],
  ['incompatible protocol', { ...healthy, protocol: 99 }],
  ['unavailable inventory', { ...healthy, inventory: { ...healthy.inventory, state: 'starting' } }],
] as const)('removes the busy hint for %s', async (_reason, unavailable) => {
  render(PromptComposer, { agent: { ...agent, status: 'working' } });
  const prompt = screen.getByLabelText('Prompt');
  expect(prompt).toHaveAccessibleDescription('Pi is working. Send adds this to its current task.');
  relayStore.connections.set(new Map([[agent.relay_id, unavailable]]) as never);
  await tick();
  expect(prompt).toHaveAttribute('placeholder', 'Type a reply…');
  expect(prompt).not.toHaveAccessibleDescription();
});

it('keeps the form busy only until acknowledgement without a reply deadline or replay', async () => {
  let resolve!: () => void;
  vi.spyOn(relayStore, 'sendToAgent').mockImplementation(() => new Promise(yes => { resolve = () => yes({ type: 'command_result', request_id: 'sent', ok: true }); }));
  const view = render(PromptComposer, { agent });
  await send();
  expect(screen.getByRole('form', { name: 'Send a prompt' })).toHaveAttribute('aria-busy', 'true');
  expect(screen.getByRole('button', { name: 'Submitting input' })).toBeDisabled();
  await fireEvent.submit(screen.getByRole('form', { name: 'Send a prompt' }));
  resolve();
  await vi.advanceTimersByTimeAsync(0);
  expect(screen.getByRole('form', { name: 'Send a prompt' })).toHaveAttribute('aria-busy', 'false');
  await view.rerender({ agent: { ...agent, status: 'working' } });
  await vi.advanceTimersByTimeAsync(30_000);
  expect(relayStore.sendToAgent).toHaveBeenCalledExactlyOnceWith(agent, { type: 'submit_prompt', text: 'New prompt' });
  expect(relayStore.showToast).toHaveBeenCalledWith('Prompt sent.');
});

it.each([false, true])('keeps send-failure feedback and safe restoration (unknown=%s)', async unknown => {
  vi.spyOn(relayStore, 'sendToAgent').mockRejectedValue(Object.assign(new Error('Synthetic refusal'), { data: unknown ? { dispatched_unknown: true } : { not_started: true } }));
  render(PromptComposer, { agent });
  await send();
  await vi.advanceTimersByTimeAsync(30_000);
  expect(screen.getByLabelText('Prompt')).toHaveValue(unknown ? '' : 'New prompt');
  expect(loadPromptDraft(agent)).toBe('');
  expect(relayStore.showToast).toHaveBeenCalledWith(unknown ? 'Synthetic refusal Check the terminal before sending again.' : 'Synthetic refusal', true);
  expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
});

it.each(['relay_id', 'server_session_id', 'raw_pane_id', 'terminal_id', 'generation', 'agent_session_id'] as const)('does not restore a late failure into a different %s', async field => {
  let reject!: (error: Error) => void;
  vi.spyOn(relayStore, 'sendToAgent').mockImplementation(() => new Promise((_yes, no) => { reject = no; }));
  const view = render(PromptComposer, { agent });
  await send();
  await view.rerender({ agent: { ...agent, [field]: field === 'generation' ? 2 : 'replacement' } });
  reject(Object.assign(new Error('Late refusal'), { data: { not_started: true } }));
  await vi.advanceTimersByTimeAsync(30_000);
  expect(screen.getByLabelText('Prompt')).toHaveValue('');
  expect(relayStore.sendToAgent).toHaveBeenCalledTimes(1);
});

it('keeps reader denial for normal, hidden and attachment input', async () => {
  const sendToAgent = vi.spyOn(relayStore, 'sendToAgent');
  const sendSecret = vi.spyOn(relayStore, 'sendSecret');
  const upload = vi.spyOn(relayStore, 'attachmentController');
  const readerAgent = { ...agent, status: 'working' };
  const view = render(PromptComposer, { agent: readerAgent, readOnly: true });
  expect(screen.getByLabelText('Prompt')).toBeDisabled();
  expect(screen.getByLabelText('Prompt')).toHaveAttribute('placeholder', 'Type a reply…');
  expect(screen.getByLabelText('Prompt')).not.toHaveAccessibleDescription();
  await send();
  await view.rerender({ frame: hiddenInputFrame(readerAgent) });
  await fireEvent.input(screen.getByLabelText('Value for the hidden terminal prompt'), { target: { value: 'synthetic-secret' } });
  await fireEvent.keyDown(screen.getByLabelText('Value for the hidden terminal prompt'), { key: 'Enter' });
  const input = document.querySelector<HTMLInputElement>('input[type="file"]')!;
  await fireEvent.change(input, { target: { files: [new File(['fixture'], 'fixture.txt', { type: 'text/plain' })] } });
  expect(sendToAgent).not.toHaveBeenCalled();
  expect(sendSecret).not.toHaveBeenCalled();
  expect(upload).not.toHaveBeenCalled();
});

it('keeps terminal adapters separate and hidden input disabled without fresh context', async () => {
  const adapter = { send: vi.fn().mockResolvedValue(undefined), filter: true, sentMessage: 'Filter sent', failureMessage: 'Filter refused' };
  const sendSecret = vi.spyOn(relayStore, 'sendSecret');
  const working = { ...agent, status: 'working' };
  const view = render(PromptComposer, { agent: working, submitAdapter: adapter, placeholder: 'Type filter text…' });
  expect(screen.getByLabelText('Prompt')).toHaveAttribute('placeholder', 'Type filter text…');
  expect(screen.getByLabelText('Prompt')).not.toHaveAccessibleDescription();
  await send();
  expect(adapter.send).toHaveBeenCalledExactlyOnceWith(working, 'New prompt');
  expect(relayStore.showToast).toHaveBeenCalledWith('Filter sent');
  await view.rerender({ submitAdapter: undefined, placeholder: 'Type a reply…', frame: hiddenInputFrame(working) });
  expect(screen.getByLabelText('Prompt')).toHaveAttribute('placeholder', 'Type a reply…');
  expect(screen.getByLabelText('Prompt')).not.toHaveAccessibleDescription();
  const input = screen.getByLabelText('Value for the hidden terminal prompt');
  await fireEvent.input(input, { target: { value: 'synthetic-secret' } });
  await fireEvent.keyDown(input, { key: 'Enter' });
  expect(input).toBeDisabled();
  expect(sendSecret).not.toHaveBeenCalled();
  await vi.advanceTimersByTimeAsync(30_000);
});
