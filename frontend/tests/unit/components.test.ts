import { fireEvent, render, screen, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import AgentList from '$components/AgentList.svelte';
import ActivityView from '$components/ActivityView.svelte';
import QuestionForm from '$components/QuestionForm.svelte';
import TerminalView from '$components/TerminalView.svelte';
import { relayStore } from '$lib/store';
import type { Agent, QuestionInteraction } from '$lib/types';

const blockedAgent: Agent = {
  relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: 'fedora::w1:p1',
  project: 'relay', agent: 'codex', status: 'blocked',
  attention_kind: 'approval', attention_capable: true,
  command: 'Run make check?', options: ['Approve once', 'Always allow', 'Deny'],
};

describe('accessible Svelte interactions', () => {
  it('requires confirmation before deleting all activity', async () => {
    const user = userEvent.setup();
    relayStore.activities.set([{
      id: 'activity-1', timestamp: 123, summary: 'Prompt sent',
      relay_id: 'fedora', relay_label: 'Fedora', activity_key: 'fedora:activity-1',
    }]);
    const clear = vi.spyOn(relayStore, 'clearActivities').mockResolvedValue();
    render(ActivityView);

    await user.click(screen.getByRole('button', { name: 'Delete all' }));
    const dialog = screen.getByRole('dialog', { name: 'Delete all activity?' });
    expect(dialog).toHaveTextContent('permanently deletes the activity history');
    expect(clear).not.toHaveBeenCalled();
    await user.click(within(dialog).getByRole('button', { name: 'Delete all' }));
    expect(clear).toHaveBeenCalledOnce();

    relayStore.activities.set([]);
  });

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

  it('keeps chat replies enabled and unknown blocked panes terminal-only', () => {
    vi.spyOn(relayStore, 'readPane').mockImplementation(() => undefined);
    vi.spyOn(relayStore, 'loadSlashCommands').mockResolvedValue({ commands: [], truncated: false });
    const chat: Agent = {
      ...blockedAgent,
      attention_kind: 'chat',
      options: undefined,
    };
    const { unmount } = render(TerminalView, {
      agent: chat,
      allAgents: [chat],
      frame: { paneId: chat.pane_id, content: 'Hello!', format: 'plain' },
      responding: new Set<string>(),
    });
    expect(screen.getByRole('combobox', { name: 'Prompt' })).toBeEnabled();
    expect(screen.getByPlaceholderText('Type a reply…')).toBeEnabled();
    expect(screen.queryByRole('button', { name: 'Approve once' })).not.toBeInTheDocument();
    unmount();

    const unknown: Agent = {
      ...blockedAgent,
      attention_kind: 'unknown',
      options: ['must not render', 'reject'],
    };
    const unknownView = render(TerminalView, {
      agent: unknown,
      allAgents: [unknown],
      frame: { paneId: unknown.pane_id, content: 'Inspect this pane', format: 'plain' },
      responding: new Set<string>(),
    });
    expect(screen.getByPlaceholderText('Needs inspection — use terminal keys')).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Enter' })).toBeEnabled();
    expect(screen.queryByText('must not render')).not.toBeInTheDocument();
    unknownView.unmount();
    vi.restoreAllMocks();
  });

  it('shows degraded inventory, keeps stale agents visible, and disables approvals', () => {
    const connections = new Map([['fedora', {
      status: 'connected',
      inventory: {
        state: 'error',
        errorCode: 'protocol_mismatch',
        message: 'Run `herdr server live-handoff` on this computer, then refresh.',
        lastAttemptAt: 123,
        lastSuccessAt: 100,
        stale: true,
      },
    } as any]]);
    const { container } = render(AgentList, {
      agents: [blockedAgent],
      relays: [{ id: 'fedora', label: 'Fedora', url: 'wss://fedora', token: '' }],
      connections,
      responding: new Set<string>(),
      onopen: vi.fn(),
    });

    expect(screen.getByRole('status', { name: 'Fedora agent inventory unavailable' })).toHaveTextContent('live-handoff');
    expect(screen.getByRole('button', { name: 'Approve once' })).toBeDisabled();
    expect(screen.getByRole('button', { name: /Open relay on Fedora/ })).toBeDisabled();
    expect(container.querySelector('.agent-card')).toHaveClass('stale');
    expect(screen.queryByText('No chat agents are running.')).not.toBeInTheDocument();
  });

  it('distinguishes successful empty inventory from loading inventory', () => {
    const relay = { id: 'fedora', label: 'Fedora', url: 'wss://fedora', token: '' };
    const readyConnections = new Map([['fedora', {
      status: 'connected', inventory: { state: 'ready' },
    } as any]]);
    const { unmount } = render(AgentList, {
      agents: [], relays: [relay], connections: readyConnections,
      responding: new Set<string>(), onopen: vi.fn(),
    });
    expect(screen.getByText('No chat agents are running.')).toBeInTheDocument();
    unmount();

    const loadingConnections = new Map([['fedora', {
      status: 'connected', inventory: { state: 'starting' },
    } as any]]);
    render(AgentList, {
      agents: [], relays: [relay], connections: loadingConnections,
      responding: new Set<string>(), onopen: vi.fn(),
    });
    expect(screen.getByText('Loading agents…')).toBeInTheDocument();
  });

  it('shows the Herdr tab name, session, and agent logo in the card', () => {
    const named: Agent = {
      relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w2:p1', pane_id: 'fedora::w2:p1',
      project: 'relay', agent: 'codex', status: 'working', tab_label: 'my-tab', session: 'my-session',
    };
    const { container } = render(AgentList, { agents: [named], relays: [], responding: new Set<string>(), onopen: vi.fn() });
    expect(container.querySelector('.agent-meta')?.textContent).toBe('my-tab · my-session');
    expect(screen.getByRole('img', { name: 'Codex' })).toBeInTheDocument();
    expect(container.querySelector('.agent-project')?.textContent).toContain('relay');
  });

  it('uses the logo instead of an agent text suffix when card metadata is empty', () => {
    const plain: Agent = {
      relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w2:p2', pane_id: 'fedora::w2:p2',
      project: 'relay', agent: 'codex', status: 'working',
    };
    const { container } = render(AgentList, { agents: [plain], relays: [], responding: new Set<string>(), onopen: vi.fn() });
    expect(container.querySelector('.agent-meta')).not.toBeInTheDocument();
    expect(screen.getByRole('img', { name: 'Codex' })).toBeInTheDocument();
    expect(screen.queryByText('codex')).not.toBeInTheDocument();
  });

  it('maps supported agent aliases to logos and labels custom fallbacks', () => {
    const identities = [
      ['claude-code', 'Claude Code'],
      ['codex', 'Codex'],
      ['open_code', 'OpenCode'],
      ['pi-coding-agent', 'Pi'],
      ['oh my pi', 'Oh My Pi'],
      ['kimi-code', 'Kimi'],
      ['qodercli', 'Qoder'],
      ['custom-agent', 'custom-agent'],
    ] as const;
    const agents: Agent[] = identities.map(([agent], index) => ({
      relay_id: 'fedora',
      relay_label: 'Fedora',
      raw_pane_id: `w3:p${index}`,
      pane_id: `fedora::w3:p${index}`,
      project: `project-${index}`,
      agent,
      status: 'working',
    }));
    const { container } = render(AgentList, { agents, relays: [], responding: new Set<string>(), onopen: vi.fn() });
    for (const [, label] of identities) {
      expect(screen.getByRole('img', { name: label })).toBeInTheDocument();
    }
    expect(container.querySelectorAll('.agent-logo')).toHaveLength(identities.length);
    expect(container.querySelectorAll('.agent-meta')).toHaveLength(1);
    expect(container.querySelector('.agent-meta')).toHaveTextContent('custom-agent');
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
    const answer = vi.spyOn(relayStore, 'answerQuestion').mockResolvedValue({ type: 'command_result', request_id: '1', ok: true, phase: 'confirmed' });
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

  it('renders a Qoder review without a custom-answer input', async () => {
    const interaction: QuestionInteraction = {
      id: 'qoder-review', kind: 'single_select',
      question: 'Review your answers and choose what to do',
      options: [
        { index: 0, label: 'Submit answers', description: 'Vibe: Relaxation · Budget: Mid-range' },
        { index: 1, label: 'Cancel ask' },
      ],
      other: { hidden: true },
      submit_label: 'Continue', can_go_back: true, question_index: 5, question_total: 5,
    };
    const answer = vi.spyOn(relayStore, 'answerQuestion').mockResolvedValue({
      type: 'command_result', request_id: 'review', ok: true, phase: 'confirmed',
    });
    render(QuestionForm, {
      agent: { ...blockedAgent, interaction }, interaction, responding: false,
    });

    expect(screen.getByText('Question 5 of 5')).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: /Submit answers/ })).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: /Cancel ask/ })).toBeInTheDocument();
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
    await fireEvent.click(screen.getByRole('radio', { name: /Submit answers/ }));
    await fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    expect(answer).toHaveBeenCalledOnce();
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
