import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import TerminalView from '$components/TerminalView.svelte';
import { copyText } from '$lib/clipboard';
import { initializeSpeech, setSpeechEnabled, setSpeechLanguage } from '$lib/speech';
import { relayStore } from '$lib/store';
import type { Agent, TerminalFrame } from '$lib/types';
import { deferred, makeAgent } from '../support/relay';

vi.mock('$lib/clipboard', () => ({ copyText: vi.fn() }));

const COPY_CAPABILITIES = ['agent_response_copy'];
const FIND_CONTENT = ['alpha one', 'beta', 'alpha two', 'gamma alpha'].join('\n');
const FINAL_RESPONSE_CONTENT = [
  '     ┃  Latest request',
  '     → Read README.md',
  '     The final answer',
  '     ▣ Build · model · 4.5s',
].join('\n');
const VISIBLE_CONTENT = 'plain visible output';
const SELECT_AGENT_RESPONSE = 'Output selected. Use your browser Copy command.';
const SELECT_FINAL_RESPONSE = 'Final response selected. Use your browser Copy command.';

const agent = makeAgent({ status: 'working' });
const questionAgent: Agent = {
  ...agent, status: 'blocked', attention_kind: 'question', attention_capable: true,
  interaction: {
    id: 'find-question', kind: 'single_select', question: 'Choose coverage',
    options: [{ index: 0, label: 'Both' }],
  },
};

function frameOf(content: string): TerminalFrame {
  return { paneId: agent.pane_id, format: 'plain', content };
}

function mountView(options: {
  content?: string; capabilities?: string[]; speechLanguages?: string[]; responding?: boolean;
  target?: Agent;
} = {}) {
  relayStore.connections.set(new Map([[agent.relay_id, {
    status: 'connected',
    capabilities: options.capabilities ?? [],
    speechLanguages: options.speechLanguages ?? [],
  }]]) as never);
  const target = options.target ?? agent;
  return render(TerminalView, {
    agent: target, allAgents: [target],
    responding: new Set<string>(options.responding ? [agent.pane_id] : []),
    readOnly: false, frame: frameOf(options.content ?? VISIBLE_CONTENT),
  });
}

function mainElement() {
  return document.querySelector('main.terminal-view') as HTMLElement;
}

function findInput() {
  return screen.queryByRole('searchbox', { name: 'Find in terminal output' }) as HTMLInputElement | null;
}

function marks() {
  return [...screen.getByRole('log').querySelectorAll<HTMLElement>('mark[data-terminal-find]')];
}

function activeMarkIndex() {
  return marks().findIndex((mark) => mark.classList.contains('active'));
}

function textarea(name: string) {
  return screen.getByRole('textbox', { name, hidden: true }) as HTMLTextAreaElement;
}

function expectSelected(target: HTMLTextAreaElement, text: string) {
  expect(target.value).toBe(text);
  expect(document.activeElement).toBe(target);
  expect(target.selectionStart).toBe(0);
  expect(target.selectionEnd).toBe(text.length);
}

function copyButton() {
  return screen.getByRole('button', { name: /^Copy(ing…)?$/ });
}

function speakButton() {
  return screen.queryByRole('button', { name: 'Read latest response aloud' });
}

function pressKey(init: KeyboardEventInit & { key: string }) {
  return fireEvent.keyDown(window, init);
}

function pressFind(init: KeyboardEventInit) {
  return pressKey({ key: 'f', ...init });
}

async function openFind() {
  await pressFind({ ctrlKey: true });
  await waitFor(() => expect(findInput()).toBeInTheDocument());
}

async function typeQuery(query: string) {
  await fireEvent.input(findInput()!, { target: { value: query } });
}

beforeEach(() => {
  vi.mocked(copyText).mockReset();
  vi.spyOn(relayStore, 'showToast').mockImplementation(() => {});
  vi.spyOn(relayStore, 'sendToAgent');
  for (const method of ['readPane', 'watchPane', 'unwatchPane'] as const) {
    vi.spyOn(relayStore, method).mockImplementation(() => {});
  }
});

afterEach(() => {
  initializeSpeech()();
  setSpeechEnabled(false);
  vi.restoreAllMocks();
  relayStore.connections.set(new Map());
});

describe('find', () => {
  it.each([
    { name: 'Ctrl+F', init: { ctrlKey: true } },
    { name: 'Cmd+F', init: { metaKey: true } },
  ])('opens the bar and focuses the input with $name', async ({ init }) => {
    const view = mountView({ content: FIND_CONTENT });
    expect(findInput()).not.toBeInTheDocument();
    expect(await pressFind(init)).toBe(false);
    await waitFor(() => expect(findInput()).toHaveFocus());
    expect(screen.getByRole('region', { name: 'Find in terminal' })).toBeInTheDocument();
    view.unmount();
  });

  it('ignores Alt+Ctrl+F and other keys', async () => {
    const view = mountView({ content: FIND_CONTENT });
    expect(await pressFind({ ctrlKey: true, altKey: true })).toBe(true);
    expect(await pressKey({ key: 'g', ctrlKey: true })).toBe(true);
    expect(await pressFind({})).toBe(true);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(findInput()).not.toBeInTheDocument();
    view.unmount();
  });

  it('ignores the shortcut and direct Find action during question mode', async () => {
    const view = mountView({ content: FIND_CONTENT, target: questionAgent });
    expect(await pressFind({ ctrlKey: true })).toBe(true);
    view.component.openFind();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(findInput()).not.toBeInTheDocument();
    view.unmount();
  });

  it('shows the match count and marks every match, with the first active', async () => {
    const view = mountView({ content: FIND_CONTENT });
    await openFind();
    await typeQuery('alpha');
    await waitFor(() => expect(screen.getByText('1 of 3')).toBeInTheDocument());
    await waitFor(() => expect(marks()).toHaveLength(3));
    expect(marks().every((mark) => mark.textContent === 'alpha')).toBe(true);
    expect(activeMarkIndex()).toBe(0);
    view.unmount();
  });

  it('says so when nothing matches', async () => {
    const view = mountView({ content: FIND_CONTENT });
    await openFind();
    await typeQuery('zeta');
    await waitFor(() => expect(screen.getByText('No matches')).toBeInTheDocument());
    expect(marks()).toHaveLength(0);
    view.unmount();
  });

  it('moves the active match with Enter and Shift+Enter, wrapping at the ends', async () => {
    const view = mountView({ content: FIND_CONTENT });
    await openFind();
    await typeQuery('alpha');
    await waitFor(() => expect(marks()).toHaveLength(3));

    await fireEvent.keyDown(findInput()!, { key: 'Enter' });
    await waitFor(() => expect(screen.getByText('2 of 3')).toBeInTheDocument());
    await waitFor(() => expect(activeMarkIndex()).toBe(1));

    await fireEvent.keyDown(findInput()!, { key: 'Enter' });
    await waitFor(() => expect(screen.getByText('3 of 3')).toBeInTheDocument());
    await waitFor(() => expect(activeMarkIndex()).toBe(2));

    await fireEvent.keyDown(findInput()!, { key: 'Enter' });
    await waitFor(() => expect(screen.getByText('1 of 3')).toBeInTheDocument());

    await fireEvent.keyDown(findInput()!, { key: 'Enter', shiftKey: true });
    await waitFor(() => expect(screen.getByText('3 of 3')).toBeInTheDocument());
    await waitFor(() => expect(activeMarkIndex()).toBe(2));
    view.unmount();
  });

  it('closes with Escape, clears the marks and shows the same query when reopened', async () => {
    const view = mountView({ content: FIND_CONTENT });
    await openFind();
    await typeQuery('alpha');
    await waitFor(() => expect(marks()).toHaveLength(3));

    await fireEvent.keyDown(findInput()!, { key: 'Escape' });
    expect(findInput()).not.toBeInTheDocument();
    await waitFor(() => expect(marks()).toHaveLength(0));

    await openFind();
    expect(findInput()).toHaveValue('alpha');
    await waitFor(() => expect(screen.getByText('1 of 3')).toBeInTheDocument());
    await waitFor(() => expect(marks()).toHaveLength(3));
    view.unmount();
  });

  it('closes with the close button', async () => {
    const view = mountView({ content: FIND_CONTENT });
    await openFind();
    await fireEvent.click(screen.getByRole('button', { name: 'Close find' }));
    expect(findInput()).not.toBeInTheDocument();
    view.unmount();
  });

  it('has the find-open class on the main element only while the bar is open', async () => {
    const view = mountView({ content: FIND_CONTENT });
    expect(mainElement()).not.toHaveClass('find-open');
    await openFind();
    expect(mainElement()).toHaveClass('find-open');
    await fireEvent.keyDown(findInput()!, { key: 'Escape' });
    expect(mainElement()).not.toHaveClass('find-open');
    view.unmount();
  });
});

describe('copy', () => {
  it('copies the agent response from the relay', async () => {
    vi.mocked(copyText).mockResolvedValue(true);
    vi.mocked(relayStore.sendToAgent).mockResolvedValue({ data: { text: 'Exact agent response' } } as never);
    const view = mountView({ capabilities: COPY_CAPABILITIES });
    await fireEvent.click(copyButton());
    await waitFor(() => expect(relayStore.showToast).toHaveBeenCalledWith('Agent response copied.'));
    expect(relayStore.sendToAgent).toHaveBeenCalledWith(agent, { type: 'copy_agent_response' }, 15_000);
    expect(copyText).toHaveBeenCalledWith('Exact agent response');
    view.unmount();
  });

  it('copies the final response found in the terminal when the relay cannot copy it', async () => {
    vi.mocked(copyText).mockResolvedValue(true);
    const view = mountView({ content: FINAL_RESPONSE_CONTENT });
    await fireEvent.click(copyButton());
    await waitFor(() => expect(relayStore.showToast).toHaveBeenCalledWith('Final response copied.'));
    expect(copyText).toHaveBeenCalledWith('The final answer');
    expect(relayStore.sendToAgent).not.toHaveBeenCalled();
    view.unmount();
  });

  it('copies the visible output when no response is found', async () => {
    vi.mocked(copyText).mockResolvedValue(true);
    const view = mountView();
    await fireEvent.click(copyButton());
    await waitFor(() => expect(relayStore.showToast).toHaveBeenCalledWith('Copied the visible terminal output.'));
    expect(copyText).toHaveBeenCalledWith(expect.stringContaining(VISIBLE_CONTENT));
    view.unmount();
  });

  it('selects the agent response in its hidden textarea when the clipboard refuses', async () => {
    vi.mocked(copyText).mockResolvedValue(false);
    vi.mocked(relayStore.sendToAgent).mockResolvedValue({ data: { text: 'Exact agent response' } } as never);
    const view = mountView({ capabilities: COPY_CAPABILITIES, content: FINAL_RESPONSE_CONTENT });
    await fireEvent.click(copyButton());
    await waitFor(() => expect(relayStore.showToast).toHaveBeenCalledWith(SELECT_AGENT_RESPONSE));
    expectSelected(textarea('Latest final response'), 'Exact agent response');
    view.unmount();
  });

  it('selects the final response in its hidden textarea when the clipboard refuses', async () => {
    vi.mocked(copyText).mockResolvedValue(false);
    const view = mountView({ content: FINAL_RESPONSE_CONTENT });
    await fireEvent.click(copyButton());
    await waitFor(() => expect(relayStore.showToast).toHaveBeenCalledWith(SELECT_FINAL_RESPONSE));
    expectSelected(textarea('Latest final response'), 'The final answer');
    view.unmount();
  });

  it('selects the visible output in the transcript textarea when the clipboard refuses', async () => {
    vi.mocked(copyText).mockResolvedValue(false);
    const view = mountView();
    await fireEvent.click(copyButton());
    await waitFor(() => expect(relayStore.showToast).toHaveBeenCalledWith(SELECT_AGENT_RESPONSE));
    const transcript = textarea('Full terminal transcript');
    expect(transcript.value).toContain(VISIBLE_CONTENT);
    expectSelected(transcript, transcript.value);
    view.unmount();
  });

  it('shows the relay failure and copies nothing when the relay copy fails', async () => {
    vi.mocked(relayStore.sendToAgent).mockRejectedValue(new Error('copy refused'));
    const view = mountView({ capabilities: COPY_CAPABILITIES });
    await fireEvent.click(copyButton());
    await waitFor(() => expect(relayStore.showToast).toHaveBeenCalledWith('copy refused', true));
    expect(copyText).not.toHaveBeenCalled();
    view.unmount();
  });

  it('disables Copy while the agent is responding', () => {
    const view = mountView({ responding: true });
    expect(copyButton()).toBeDisabled();
    view.unmount();
  });

  it('disables Copy while the response is fetched', async () => {
    const response = deferred<{ data: { text: string } }>();
    vi.mocked(copyText).mockResolvedValue(true);
    vi.mocked(relayStore.sendToAgent).mockReturnValue(response.promise as never);
    const view = mountView({ capabilities: COPY_CAPABILITIES });
    await fireEvent.click(copyButton());
    await waitFor(() => expect(screen.getByRole('button', { name: 'Copying…' })).toBeDisabled());
    response.resolve({ data: { text: 'Exact agent response' } });
    await waitFor(() => expect(copyButton()).toBeEnabled());
    view.unmount();
  });
});

describe('response preview', () => {
  async function copyExactResponse(copied: boolean) {
    vi.mocked(copyText).mockResolvedValue(copied);
    vi.mocked(relayStore.sendToAgent).mockResolvedValue({ data: { text: '# Heading\nBody' } } as never);
    const view = mountView({ capabilities: COPY_CAPABILITIES });
    await fireEvent.click(copyButton());
    await screen.findByRole('region', { name: 'Copied agent response' });
    vi.mocked(copyText).mockClear();
    vi.mocked(relayStore.showToast).mockClear();
    return view;
  }

  it('does not show for a response that is not exact', async () => {
    vi.mocked(copyText).mockResolvedValue(true);
    const view = mountView({ content: FINAL_RESPONSE_CONTENT });
    await fireEvent.click(copyButton());
    await waitFor(() => expect(relayStore.showToast).toHaveBeenCalledWith('Final response copied.'));
    expect(screen.queryByRole('region', { name: 'Copied agent response' })).not.toBeInTheDocument();
    view.unmount();
  });

  it('shows an exact agent response and copies it again', async () => {
    const view = await copyExactResponse(true);
    expect(screen.getByRole('textbox', { name: 'Copied agent response markdown' })).toHaveValue('# Heading\nBody');
    await fireEvent.click(screen.getByRole('button', { name: 'Copy markdown' }));
    await waitFor(() => expect(relayStore.showToast).toHaveBeenCalledWith('Agent response copied.'));
    expect(copyText).toHaveBeenCalledWith('# Heading\nBody');
    view.unmount();
  });

  it('selects the preview text when the clipboard refuses the second copy', async () => {
    const view = await copyExactResponse(false);
    await fireEvent.click(screen.getByRole('button', { name: 'Copy markdown' }));
    await waitFor(() => expect(relayStore.showToast).toHaveBeenCalledWith(SELECT_AGENT_RESPONSE));
    expectSelected(screen.getByRole('textbox', { name: 'Copied agent response markdown' }) as HTMLTextAreaElement, '# Heading\nBody');
    view.unmount();
  });

  it('dismisses', async () => {
    const view = await copyExactResponse(true);
    await fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByRole('region', { name: 'Copied agent response' })).not.toBeInTheDocument();
    view.unmount();
  });
});

describe('speak', () => {
  beforeEach(() => {
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined);
    vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => {});
    vi.stubGlobal('URL', class extends URL {
      static createObjectURL = () => 'blob:speech';
      static revokeObjectURL = vi.fn();
    });
    setSpeechLanguage('en');
  });

  afterEach(() => vi.unstubAllGlobals());

  it('is hidden while speech is disabled', () => {
    setSpeechEnabled(false);
    const view = mountView({ speechLanguages: ['en'] });
    expect(speakButton()).not.toBeInTheDocument();
    view.unmount();
  });

  it('is shown when speech is enabled', () => {
    setSpeechEnabled(true);
    const view = mountView({ speechLanguages: ['en'] });
    expect(speakButton()).toBeEnabled();
    view.unmount();
  });

  it('is disabled while the text is fetched', async () => {
    setSpeechEnabled(true);
    const response = deferred<{ data: { text: string } }>();
    vi.mocked(relayStore.sendToAgent).mockReturnValue(response.promise as never);
    const view = mountView({ speechLanguages: ['en'], capabilities: COPY_CAPABILITIES });
    await fireEvent.click(speakButton()!);
    await waitFor(() => expect(speakButton()).toBeDisabled());
    expect(speakButton()).toHaveAttribute('aria-busy', 'true');

    response.resolve({ data: { text: '' } });
    await waitFor(() => expect(speakButton()).toBeEnabled());
    expect(relayStore.showToast).toHaveBeenCalledWith('No completed agent response is available to read aloud.', true);
    view.unmount();
  });
});
