import { fireEvent, render, screen, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import AgentList from '$components/AgentList.svelte';
import ConversationHistory from '$components/ConversationHistory.svelte';
import WorkspaceManager from '$components/WorkspaceManager.svelte';
import ActivityView from '$components/ActivityView.svelte';
import QuestionForm from '$components/QuestionForm.svelte';
import TerminalView from '$components/TerminalView.svelte';
import LaunchView from '$components/LaunchView.svelte';
import { relayStore } from '$lib/store';
import { clearPromptDraft } from '$lib/prompt-drafts';
import { setHomeLayout } from '$lib/preferences';
import type { Agent, CommandResult, QuestionInteraction, RelayConnectionView, RelayWorkspace, WorktreeListing } from '$lib/types';

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

  it('hides generic working transitions from the activity list', () => {
    relayStore.activities.set([
      {
        timestamp: 123,
        kind: 'working',
        status: 'working',
        summary: 'omp started working',
        relay_id: 'fedora',
        relay_label: 'Fedora',
        activity_key: 'fedora:working',
      },
      {
        timestamp: 124,
        kind: 'finished',
        status: 'completed',
        summary: 'omp completed',
        extract: 'The complete response',
        relay_id: 'fedora',
        relay_label: 'Fedora',
        activity_key: 'fedora:finished',
      },
    ]);
    render(ActivityView);

    expect(screen.queryByText('omp started working')).not.toBeInTheDocument();
    expect(screen.getByText('omp completed')).toBeInTheDocument();
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
    expect(screen.getByRole('heading', { name: 'Needs input' })).toBeInTheDocument();
    expect(screen.getByText('Run make check?')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Approve once' }));
    expect(respond).toHaveBeenCalledWith(blockedAgent, 0, 3, 'Approve once');
    await user.click(screen.getByRole('button', { name: /Open relay on Fedora/ }));
    expect(onopen).toHaveBeenCalledWith(blockedAgent);
    respond.mockRestore();
  });

  it('enables blocked terminal text only while its editor is active', async () => {
    const user = userEvent.setup();
    vi.spyOn(relayStore, 'readPane').mockImplementation(() => undefined);
    vi.spyOn(relayStore, 'loadSlashCommands').mockResolvedValue({ commands: [], truncated: false });
    const send = vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue({
      type: 'command_result', request_id: 'text-1', ok: true,
    });
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
    const choiceView = render(TerminalView, {
      agent: unknown,
      allAgents: [unknown],
      frame: {
        paneId: unknown.pane_id,
        content: 'Other (type your own)\nEnter select · ↑/↓ move · Tab/←/→ · Esc cancel',
        format: 'plain',
      },
      responding: new Set<string>(),
    });
    expect(screen.getByPlaceholderText('Needs inspection — use terminal controls')).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Attach image' })).toBeDisabled();
    choiceView.unmount();

    const unknownView = render(TerminalView, {
      agent: unknown,
      allAgents: [unknown],
      frame: {
        paneId: unknown.pane_id,
        content: 'Custom answer: Which weekend?\n>\nenter or ctrl+q submit  esc cancel  ctrl+g external editor',
        format: 'plain',
      },
      responding: new Set<string>(),
    });
    const terminalInput = screen.getByPlaceholderText('Type terminal input…');
    expect(terminalInput).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Attach image' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Enter' })).toBeEnabled();
    expect(screen.queryByText('must not render')).not.toBeInTheDocument();
    await user.type(terminalInput, 'custom weekend');
    await user.click(screen.getByRole('button', { name: 'Submit terminal text' }));
    expect(send).toHaveBeenNthCalledWith(1, unknown, {
      type: 'send_text', text: 'custom weekend',
    });
    expect(send).toHaveBeenNthCalledWith(2, unknown, {
      type: 'send_keys', keys: ['Enter'], activity_label: 'Submitted terminal text',
    });
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

  it('groups working agents by workspace while keeping inactive workspaces separate', () => {
    // The by-state sections are opt-in since 0.17.10; this defends their
    // grouping when selected.
    setHomeLayout('state');
    const named: Agent = {
      relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w2:p1', pane_id: 'fedora::w2:p1',
      workspace_id: 'work-1', project: 'relay', agent: 'codex', status: 'working',
      tab_id: 'tab-1', tab_number: 1, tab_label: 'my-tab', session: 'my-session',
    };
    const peer: Agent = {
      ...named, raw_pane_id: 'w2:p2', pane_id: 'fedora::w2:p2', session: '',
      tab_id: 'tab-2', tab_number: 2, tab_label: 'review',
    };
    const ready: Agent = {
      relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w3:p1', pane_id: 'fedora::w3:p1',
      workspace_id: 'work-2', project: 'docs', agent: 'codex', status: 'idle',
    };
    const { container } = render(AgentList, { agents: [named, peer, ready], relays: [], responding: new Set<string>(), onopen: vi.fn() });
    const working = screen.getByRole('heading', { name: 'Working' }).closest('section')!;
    const workspaces = screen.getByRole('heading', { name: 'Idle' }).closest('section')!;
    expect(within(working).getAllByText('relay', { selector: 'summary strong' })).toHaveLength(1);
    expect(within(working).getByRole('heading', { name: 'my-tab' })).toBeInTheDocument();
    expect(within(working).getByRole('heading', { name: 'review' })).toBeInTheDocument();
    expect(within(working).getByText('my-session')).toBeInTheDocument();
    expect(within(workspaces).getByText('docs', { selector: 'summary strong' })).toBeInTheDocument();
    expect(container.querySelectorAll('.agent-logo')).toHaveLength(3);
    setHomeLayout('mixed');
  });

  it('orders tabs by Herdr position and reorders with Alt+arrow keys on a card', async () => {
    // tab_number contradicts tab_order on purpose: numbers are stable Herdr
    // identities while tab_order carries the visual position.
    const first: Agent = {
      relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w2:p1', pane_id: 'fedora::w2:p1',
      workspace_id: 'work-1', project: 'relay', agent: 'codex', status: 'working',
      tab_id: 'tab-1', tab_number: 7, tab_order: 1, tab_label: 'First',
    };
    const second: Agent = {
      ...first, raw_pane_id: 'w2:p2', pane_id: 'fedora::w2:p2',
      tab_id: 'tab-2', tab_number: 3, tab_order: 2, tab_label: 'Second',
    };
    // AgentList reads only connection readiness and capabilities in this fixture.
    const connection = {
      status: 'connected', inventory: { state: 'ready' }, capabilities: ['tab_reorder'],
    } as unknown as RelayConnectionView;
    const connections = new Map([['fedora', connection]]);
    const reorder = vi.spyOn(relayStore, 'reorderTab').mockResolvedValue({
      type: 'command_result', request_id: 'move-1', ok: true,
    });
    render(AgentList, {
      agents: [second, first], relays: [], connections,
      responding: new Set<string>(), onopen: vi.fn(),
    });
    const headings = screen.getAllByRole('heading', { level: 3 }).map((heading) => heading.textContent);
    expect(headings).toEqual(['First', 'Second']);
    const cards = screen.getAllByRole('button', { name: 'Open relay on Fedora' });
    await fireEvent.keyDown(cards[0], { key: 'ArrowDown', altKey: true });
    expect(reorder).toHaveBeenCalledWith(first, 2);
    // The new arrangement shows immediately, before the relay confirms.
    expect(screen.getAllByRole('heading', { level: 3 }).map((heading) => heading.textContent))
      .toEqual(['Second', 'First']);
    await fireEvent.keyDown(cards[0], { key: 'ArrowDown' });
    expect(reorder).toHaveBeenCalledTimes(1);
    reorder.mockRestore();
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

  it('keeps an unoccupied linked worktree visible when its repository parent is absent', () => {
    const orphan = {
      relay_id: 'fedora', relay_label: 'Fedora', workspace_id: 'w9', number: 9, label: 'fix-auth',
      pane_count: 1, tab_count: 1, cwd: '/repos/mobile-fix-auth',
      worktree: {
        repo_key: 'repo', repo_name: 'mobile', repo_root: '/repos/mobile',
        checkout_path: '/repos/mobile-fix-auth', is_linked_worktree: true,
      },
    } as RelayWorkspace;
    // The same repository open on another computer must not hide the orphan.
    const foreignParent = {
      relay_id: 'mac', relay_label: 'Mac', workspace_id: 'w1', number: 1, label: 'mobile',
      pane_count: 1, tab_count: 1, cwd: '/repos/mobile',
      worktree: {
        repo_key: 'repo', repo_name: 'mobile', repo_root: '/repos/mobile',
        checkout_path: '/repos/mobile', is_linked_worktree: false,
      },
    } as RelayWorkspace;
    render(AgentList, {
      agents: [], relays: [], workspaces: [orphan, foreignParent],
      responding: new Set<string>(), onopen: vi.fn(),
    });
    expect(screen.getByText('fix-auth', { selector: 'summary strong' })).toBeInTheDocument();
    expect(screen.getByText('mobile', { selector: 'summary strong' })).toBeInTheDocument();
  });

  it('drops an optimistic workspace order once an authoritative snapshot changes membership', async () => {
    const workspace = (id: string, label: string): RelayWorkspace => ({
      relay_id: 'fedora', relay_label: 'Fedora', workspace_id: id, number: 1, label,
      focused: false, pane_count: 1, tab_count: 1, active_tab_id: '', agent_status: '',
      cwd: `/home/user/${id}`,
    });
    relayStore.relayConfigs.set([{ id: 'fedora', label: 'Fedora', url: 'wss://fedora', token: '' }]);
    // RelayConnection is store-internal; the rendered manager reads only
    // status, inventory readiness, and capabilities — all on the view type.
    const connection = {
      status: 'connected', inventory: { state: 'ready' },
      capabilities: ['workspace_management', 'workspace_reorder_block'],
    } as unknown as RelayConnectionView;
    relayStore.connections.set(new Map([['fedora', connection as never]]));
    relayStore.workspaces.set([workspace('w1', 'One'), workspace('w2', 'Two')]);
    // A never-settling reorder keeps the optimistic order pending under test.
    const reorder = vi.spyOn(relayStore, 'reorderWorkspaceBlock')
      .mockReturnValue(new Promise<CommandResult>(() => {}));
    try {
      const { container } = render(WorkspaceManager);
      const labels = () => [...container.querySelectorAll('.workspace-management-slot header strong')]
        .map((element) => element.textContent);
      expect(labels()).toEqual(['One', 'Two']);

      await fireEvent.keyDown(screen.getByRole('button', { name: 'Reorder One' }), { key: 'ArrowDown', altKey: true });
      expect(reorder).toHaveBeenCalledWith('fedora', ['w1'], '', 2);
      // The optimistic arrangement shows while the relay has not confirmed.
      expect(labels()).toEqual(['Two', 'One']);

      // A snapshot with different membership invalidates the optimism.
      relayStore.workspaces.set([workspace('w1', 'One'), workspace('w2', 'Two'), workspace('w3', 'Three')]);
      await vi.waitFor(() => expect(labels()).toEqual(['One', 'Two', 'Three']));
    } finally {
      reorder.mockRestore();
      relayStore.workspaces.set([]);
      relayStore.connections.set(new Map());
      relayStore.relayConfigs.set([]);
    }
  });

  it('hands the launch form to a deep link relay that connects after a faster sibling', async () => {
    relayStore.relayConfigs.set([
      { id: 'fast', label: 'Fast', url: 'wss://fast', token: '' },
      { id: 'slow', label: 'Slow', url: 'wss://slow', token: '' },
    ]);
    const ready = {
      status: 'connected', inventory: { state: 'ready' }, capabilities: [], agentProfiles: [],
    } as unknown as RelayConnectionView;
    relayStore.connections.set(new Map([['fast', ready as never]]));
    try {
      render(LaunchView, { relayId: 'slow', cwd: '/home/user/project' });
      const select = screen.getByRole('combobox', { name: 'Computer' }) as HTMLSelectElement;
      // The faster sibling wins only while the requested relay is absent.
      await vi.waitFor(() => expect(select.value).toBe('fast'));
      relayStore.connections.set(new Map([['fast', ready as never], ['slow', ready as never]]));
      await vi.waitFor(() => expect(select.value).toBe('slow'));
    } finally {
      relayStore.connections.set(new Map());
      relayStore.relayConfigs.set([]);
    }
  });

  it('drops a stale worktree listing that resolves under another workspace dialog', async () => {
    const user = userEvent.setup();
    const workspace = (id: string, label: string): RelayWorkspace => ({
      relay_id: 'fedora', relay_label: 'Fedora', workspace_id: id, number: 1, label,
      focused: false, pane_count: 1, tab_count: 1, active_tab_id: '', agent_status: '',
      cwd: `/repos/${id}`,
    });
    const listing = (repo: string, branch: string): WorktreeListing => ({
      source: { repo_key: repo, repo_name: repo, repo_root: `/repos/${repo}` },
      worktrees: [{ path: `/repos/${repo}-${branch}`, branch, label: branch, is_linked_worktree: true }],
    } as unknown as WorktreeListing);
    relayStore.relayConfigs.set([{ id: 'fedora', label: 'Fedora', url: 'wss://fedora', token: '' }]);
    const connection = {
      status: 'connected', inventory: { state: 'ready' },
      capabilities: ['workspace_management', 'worktree_management'],
    } as unknown as RelayConnectionView;
    relayStore.connections.set(new Map([['fedora', connection as never]]));
    relayStore.workspaces.set([workspace('w1', 'One'), workspace('w2', 'Two')]);
    let releaseFirst = (_: WorktreeListing) => {};
    const first = new Promise<WorktreeListing>((resolve) => { releaseFirst = resolve; });
    const list = vi.spyOn(relayStore, 'listWorktrees')
      .mockImplementationOnce(() => first)
      .mockImplementationOnce(() => Promise.resolve(listing('two', 'feature-two')));
    try {
      render(WorkspaceManager);
      const buttons = screen.getAllByRole('button', { name: 'Worktrees' });
      await user.click(buttons[0]);
      await user.click(buttons[1]);
      // The second dialog's listing arrives first; the first workspace's
      // slower response must not replace it afterwards.
      await vi.waitFor(() => expect(screen.getByText('feature-two')).toBeInTheDocument());
      releaseFirst(listing('one', 'feature-one'));
      await first;
      expect(list).toHaveBeenCalledTimes(2);
      expect(screen.queryByText('feature-one')).not.toBeInTheDocument();
      expect(screen.getByText('feature-two')).toBeInTheDocument();
    } finally {
      list.mockRestore();
      relayStore.workspaces.set([]);
      relayStore.connections.set(new Map());
      relayStore.relayConfigs.set([]);
    }
  });

  it('persists the conversation composer draft across remounts and clears it on send', async () => {
    const user = userEvent.setup();
    const agent: Agent = {
      relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w1:p9', pane_id: 'fedora::w1:p9',
      project: 'relay', agent: 'codex', status: 'working', cwd: '/home/test/relay',
    };
    vi.spyOn(relayStore, 'getConversationHistory').mockResolvedValue({
      available: true, reason: '', entries: [], hasMore: false, total: 0, fileTruncated: false,
    });
    const send = vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue({
      type: 'command_result', request_id: 'prompt-1', ok: true,
    });
    try {
      const first = render(ConversationHistory, { agent });
      await user.type(screen.getByRole('textbox', { name: 'Prompt' }), 'Keep me around');
      first.unmount();

      const second = render(ConversationHistory, { agent });
      expect(screen.getByRole('textbox', { name: 'Prompt' })).toHaveValue('Keep me around');
      await user.click(screen.getByRole('button', { name: 'Send prompt' }));
      expect(send).toHaveBeenCalledWith(agent, { type: 'submit_prompt', text: 'Keep me around' });
      second.unmount();

      render(ConversationHistory, { agent });
      expect(screen.getByRole('textbox', { name: 'Prompt' })).toHaveValue('');
    } finally {
      clearPromptDraft(agent);
      vi.restoreAllMocks();
    }
  });
});
