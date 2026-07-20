import { fireEvent, render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import AgentList from '$components/AgentList.svelte';
import QuestionForm from '$components/QuestionForm.svelte';
import TerminalView from '$components/TerminalView.svelte';
import { relayStore } from '$lib/store';
import type { Agent, QuestionInteraction } from '$lib/types';

const blockedAgent: Agent = {
  relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: 'fedora::w1:p1',
  project: 'relay', agent: 'codex', status: 'blocked', command: 'Run make check?', options: ['Approve once', 'Always allow', 'Deny'],
};

describe('accessible Svelte interactions', () => {
  it('filters slash commands and fills the composer without submitting', async () => {
    const user = userEvent.setup();
    const agent: Agent = {
      relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w1:p2', pane_id: 'fedora::w1:p2',
      project: 'relay', agent: 'codex', status: 'working', cwd: '/home/test/relay',
    };
    vi.spyOn(relayStore, 'readPane').mockImplementation(() => undefined);
    vi.spyOn(relayStore, 'loadSlashCommands').mockResolvedValue({
      commands: [
        { command: '/model', description: 'Choose the active model', source: 'builtin' },
        { command: '/plan', description: 'Enter plan mode', argument_hint: '[prompt]', source: 'builtin' },
      ],
      truncated: false,
    });
    const send = vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue({
      type: 'command_result', request_id: 'prompt-1', ok: true,
    });
    render(TerminalView, {
      agent,
      allAgents: [agent],
      frame: { paneId: agent.pane_id, content: 'ready', format: 'plain' },
      responding: new Set<string>(),
    });

    const composer = screen.getByRole('combobox', { name: 'Prompt' });
    await user.type(composer, '/pl');
    expect(screen.getByRole('listbox', { name: 'Slash commands' })).toBeVisible();
    expect(screen.getByRole('option', { name: /\/plan/ })).toBeVisible();
    expect(screen.queryByRole('option', { name: /\/model/ })).not.toBeInTheDocument();
    await user.keyboard('{Enter}');
    expect(composer).toHaveValue('/plan ');
    expect(send).not.toHaveBeenCalled();

    await user.type(composer, 'Review the migration');
    await user.click(screen.getByRole('button', { name: 'Send prompt' }));
    expect(send).toHaveBeenCalledWith(agent, {
      type: 'submit_prompt', text: '/plan Review the migration',
    });
    vi.restoreAllMocks();
  });

  it('opens agents and submits approval buttons by role', async () => {
    const user = userEvent.setup();
    const onopen = vi.fn();
    const respond = vi.spyOn(relayStore, 'respond').mockResolvedValue(true);
    render(AgentList, { agents: [blockedAgent], relays: [{ id: 'fedora', label: 'Fedora', url: 'wss://fedora', token: '' }], responding: new Set<string>(), onopen });
    expect(screen.getByRole('heading', { name: 'Blocked' })).toBeInTheDocument();
    expect(screen.getByText('Run make check?')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Approve once' }));
    expect(respond).toHaveBeenCalledWith(blockedAgent, 0, 3, 'Approve once');
    await user.click(screen.getByRole('button', { name: /Open relay on Fedora/ }));
    expect(onopen).toHaveBeenCalledWith(blockedAgent);
    respond.mockRestore();
  });

  it('shows the Herdr tab name in the card meta line', () => {
    const named: Agent = {
      relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w2:p1', pane_id: 'fedora::w2:p1',
      project: 'relay', agent: 'codex', status: 'working', tab_label: 'my-tab',
    };
    const { container } = render(AgentList, { agents: [named], relays: [], responding: new Set<string>(), onopen: vi.fn() });
    expect(container.querySelector('.agent-meta')?.textContent).toBe('my-tab · codex · working');
    expect(container.querySelector('.agent-project')?.textContent).toContain('relay');
  });

  it('omits the meta name segment when no tab or pane name is set', () => {
    const plain: Agent = {
      relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w2:p2', pane_id: 'fedora::w2:p2',
      project: 'relay', agent: 'codex', status: 'working',
    };
    const { container } = render(AgentList, { agents: [plain], relays: [], responding: new Set<string>(), onopen: vi.fn() });
    expect(container.querySelector('.agent-meta')?.textContent).toBe('codex · working');
  });

  it('keeps a structured answer local until Submit', async () => {
    const interaction: QuestionInteraction = {
      id: 'question-1', kind: 'single_select', question: 'Where should the adapter live?',
      options: [
        { index: 0, label: 'Domain port', description: 'Transport agnostic.' },
        { index: 1, label: 'Protocol boundary' },
      ],
      other: { label: 'None of the above', placeholder: 'Optional notes', allow_empty: true },
      submit_label: 'Next', can_go_back: true, can_chat: true, question_index: 2, question_total: 4,
    };
    const answer = vi.spyOn(relayStore, 'answerQuestion').mockResolvedValue({ type: 'command_result', request_id: '1', ok: true, phase: 'submitted' });
    vi.spyOn(relayStore, 'navigateQuestionPrevious').mockResolvedValue({ type: 'command_result', request_id: '2', ok: true });
    render(QuestionForm, { agent: { ...blockedAgent, interaction }, interaction, responding: false });
    expect(screen.getByRole('group', { name: interaction.question })).toBeInTheDocument();
    expect(screen.getByText('Question 2 of 4')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Next' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Chat about this' })).not.toBeInTheDocument();
    await fireEvent.click(screen.getByRole('radio', { name: /Domain port/ }));
    expect(answer).not.toHaveBeenCalled();
    await fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(answer).toHaveBeenCalledOnce();
    const draft = answer.mock.calls[0][2];
    expect([...draft.selected]).toEqual([0]);
    answer.mockRestore();
    vi.restoreAllMocks();
  });

  it('does not restore Other after selecting a normal answer across navigation', async () => {
    const first: QuestionInteraction = {
      id: 'question-1', kind: 'single_select', question: 'Choose reconnect behavior',
      options: [{ index: 0, label: 'Backoff' }, { index: 1, label: 'Fixed retry' }],
      other: { label: 'Other', placeholder: 'Other answer' }, submit_label: 'Next',
    };
    const second: QuestionInteraction = {
      id: 'question-2', kind: 'multi_select', question: 'Choose offline scope',
      options: [{ index: 0, label: 'App shell' }, { index: 1, label: 'Activity cache' }],
      other: { label: 'Other', placeholder: 'Other answer' }, submit_label: 'Next', can_go_back: true,
    };
    const view = render(QuestionForm, {
      agent: { ...blockedAgent, interaction: first }, interaction: first, responding: false,
    });

    const otherInput = screen.getByRole('textbox', { name: 'Other answer' });
    await fireEvent.input(otherInput, { target: { value: 'Hello' } });
    expect(screen.getByRole('radio', { name: 'Other' })).toBeChecked();
    await view.rerender({ agent: { ...blockedAgent, interaction: second }, interaction: second, responding: false });
    await view.rerender({ agent: { ...blockedAgent, interaction: first }, interaction: first, responding: false });
    await fireEvent.click(screen.getByRole('radio', { name: 'Fixed retry' }));
    expect(screen.getByRole('radio', { name: 'Other' })).not.toBeChecked();
    expect(screen.getByRole('textbox', { name: 'Other answer' })).toHaveValue('');

    await view.rerender({ agent: { ...blockedAgent, interaction: second }, interaction: second, responding: false });
    const restored = {
      ...first,
      options: first.options.map((option) => ({ ...option, selected: option.index === 1 })),
      other: { ...first.other, selected: false, text: 'Hello' },
    };
    await view.rerender({ agent: { ...blockedAgent, interaction: restored }, interaction: restored, responding: false });
    expect(screen.getByRole('radio', { name: 'Fixed retry' })).toBeChecked();
    expect(screen.getByRole('radio', { name: 'Other' })).not.toBeChecked();
    expect(screen.getByRole('textbox', { name: 'Other answer' })).toHaveValue('');
  });

  it('restores a confirmed choice instead of an incomplete stale draft', async () => {
    const first: QuestionInteraction = {
      id: 'confirmed-reconnect', kind: 'single_select', question: 'Choose reconnect strategy',
      options: [{ index: 0, label: 'Backoff' }, { index: 1, label: 'Signals' }],
      other: { label: 'Other', placeholder: 'Other answer' }, submit_label: 'Next',
    };
    const second: QuestionInteraction = {
      id: 'confirmed-offline', kind: 'multi_select', question: 'Choose offline scope',
      options: [{ index: 0, label: 'App shell' }], submit_label: 'Next', can_go_back: true,
    };
    const view = render(QuestionForm, {
      agent: { ...blockedAgent, interaction: first }, interaction: first, responding: false,
    });

    await fireEvent.focus(screen.getByRole('textbox', { name: 'Other answer' }));
    expect(screen.getByRole('radio', { name: 'Other' })).toBeChecked();
    expect(screen.getByRole('button', { name: 'Next' })).toBeDisabled();
    await view.rerender({ agent: { ...blockedAgent, interaction: second }, interaction: second, responding: false });

    const confirmed = {
      ...first,
      options: first.options.map((option) => ({ ...option, selected: option.index === 1 })),
    };
    await view.rerender({ agent: { ...blockedAgent, interaction: confirmed }, interaction: confirmed, responding: false });

    expect(screen.getByRole('radio', { name: 'Signals' })).toBeChecked();
    expect(screen.getByRole('radio', { name: 'Other' })).not.toBeChecked();
  });
});
