import { render, screen, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import AgentList from '$components/AgentList.svelte';
import GlobalJump from '$components/GlobalJump.svelte';
import TerminalView from '$components/TerminalView.svelte';
import { agentStatusGroup } from '$lib/agents';
import { relayStore } from '$lib/store';
import type { Agent, RelayConnectionView } from '$lib/types';

function agent(overrides: Partial<Agent> = {}): Agent {
  return {
    relay_id: 'relay-a',
    relay_label: 'Laptop',
    raw_pane_id: 'pane-1',
    pane_id: 'relay-a::pane-1',
    workspace_id: 'workspace-1',
    project: 'mobile',
    agent: 'codex',
    status: 'idle',
    cwd: '/work/mobile',
    ...overrides,
  };
}

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('adopted navigation and terminal controls', () => {
  it('searches across relays and selects the matching agent', async () => {
    const user = userEvent.setup();
    const laptop = agent();
    const desktop = agent({
      relay_id: 'relay-b', relay_label: 'Desktop', raw_pane_id: 'pane-2', pane_id: 'relay-b::pane-2',
      workspace_id: 'workspace-2', project: 'server', cwd: '/work/server',
    });
    const onselect = vi.fn();
    render(GlobalJump, { open: true, agents: [laptop, desktop], onselect });

    await user.type(screen.getByRole('searchbox', { name: 'Search agents and workspaces' }), 'Desktop');
    expect(screen.queryByRole('button', { name: /mobile/i })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: /server/i }));
    expect(onselect).toHaveBeenCalledWith(desktop);
  });

  it('keeps an opened workspace expanded across agent snapshots', async () => {
    const user = userEvent.setup();
    const now = Date.now();
    const primary = agent({
      tab_id: 'tab-1',
      tab_label: 'Review',
      last_active_at: now - 2 * 60 * 60_000,
      updated_at: now,
    });
    const secondary = agent({
      raw_pane_id: 'pane-2',
      pane_id: 'relay-a::pane-2',
      workspace_id: 'workspace-2',
      project: 'docs',
      cwd: '/work/docs',
    });
    const onopen = vi.fn();
    const view = render(AgentList, {
      agents: [primary, secondary],
      relays: [],
      responding: new Set<string>(),
      onopen,
    });
    const summary = screen.getByText('mobile', { selector: 'summary strong' }).closest('summary');
    const workspace = summary?.closest('details') as HTMLDetailsElement | null;
    expect(workspace?.open).toBe(false);

    await user.click(summary!);
    expect(workspace?.open).toBe(true);
    expect(workspace?.querySelector('.workspace-tab h3')?.textContent).toBe('Review');
    expect(workspace?.querySelector('.agent-meta')?.textContent || '').not.toContain('Review');
    expect(screen.getByRole('button', { name: 'Open mobile on Laptop' }))
      .toHaveAccessibleDescription('Idle · last active 2h ago');
    await view.rerender({
      agents: [{ ...primary, activity_seq: 2 }, { ...secondary }],
      relays: [],
      responding: new Set<string>(),
      onopen,
    });

    expect(workspace?.open).toBe(true);
  });

  it('shows pane names and activity on Home and the exact pane path in jump', async () => {
    const cwd = '/home/user/project';
    const fix = agent({ name: '  fix-bug  ', project: 'project', tab_label: 'main', cwd, status: 'working' });
    const review = agent({
      ...fix, name: 'review-tests', pane_id: 'relay-a::pane-2', raw_pane_id: 'pane-2', status: 'idle',
    });
    const unnamed = agent({ ...fix, name: ' ', pane_id: 'relay-a::pane-3', raw_pane_id: 'pane-3', status: 'unknown' });
    const onselect = vi.fn();
    const connections = new Map([['relay-a', {
      status: 'connected', home: '/home/user', inventory: { state: 'ready' }, capabilities: [],
    } as unknown as RelayConnectionView]]);
    render(AgentList, { agents: [fix, review, unnamed], relays: [], connections, responding: new Set<string>(), onopen: vi.fn() });
    const card = screen.getByText('project', { selector: 'summary strong' }).closest('details')!;
    expect(card.querySelector('summary .path-row')).toHaveTextContent('~/project');
    expect(card.querySelector('summary .host-badge')).toHaveTextContent('@Laptop');
    for (const [label, status] of [['fix-bug', 'Working'], ['review-tests', 'Idle'], ['project', 'Status unknown']]) {
      const row = screen.getByRole('button', { name: `Open ${label} on Laptop` });
      expect(row.querySelector('.agent-project')).toHaveTextContent(label);
      expect(row).toHaveAccessibleDescription(status);
      expect(row).not.toHaveTextContent(cwd);
      expect(row.querySelector('.host-badge')).toBeNull();
    }

    render(GlobalJump, { agents: [fix, review], open: true, onselect });
    const user = userEvent.setup();
    await user.type(screen.getByRole('searchbox'), 'fix-bug');
    const match = screen.getByRole('button', { name: /Working.*fix-bug/ });
    expect(match).toHaveAccessibleDescription(cwd);
    expect(screen.getByText(cwd, { selector: '.jump-pane-path' })).toBeVisible();
    expect(within(screen.getByRole('dialog')).queryByRole('button', { name: /review-tests/ })).toBeNull();
    expect(onselect).not.toHaveBeenCalled();
  });

  it('describes the full quick question once and submits its original interaction', async () => {
    const question = 'Which option should we use when publishing the new search page?';
    const interaction = {
      id: 'question', kind: 'single_select' as const, question,
      options: [{ index: 0, label: 'Preview' }, { index: 1, label: 'Publish' }],
    };
    const waiting = agent({
      status: 'blocked', attention_kind: 'question', attention_capable: true,
      interaction, prompt: question, statusClockGroup: 'blocked', status_since_at: Date.now() - 3 * 60_000,
    });
    const answer = vi.spyOn(relayStore, 'answerQuestion').mockResolvedValue({ phase: 'confirmed' } as never);
    const view = render(AgentList, { agents: [waiting], relays: [], responding: new Set<string>(), onopen: vi.fn() });
    const row = screen.getByRole('button', { name: 'Open mobile on Laptop' });
    expect(row).toHaveAccessibleDescription(`Needs input · 3m ${question}`);
    expect(screen.getAllByText(question)).toHaveLength(1);
    expect(view.container.querySelector('.prompt-full')).toHaveTextContent(question);
    await userEvent.click(screen.getByRole('button', { name: 'Preview' }));
    expect(answer).toHaveBeenCalledWith(waiting, interaction, {
      selected: new Set([0]), otherSelected: false, otherText: '',
    });
  });

  it('formats the clock for the displayed state and updates it on the minute tick', async () => {
    vi.useFakeTimers();
    const now = Date.now();
    const rows = [
      ['working', now - 30_000, 'Working · <1m'],
      ['blocked', now - 3 * 60_000, 'Needs inspection · 3m'],
      ['succeeded', now - 2 * 60_000, 'New result · 2m ago'],
      ['done', now, 'New result · just now'],
      ['idle', now - 12 * 60_000, 'Idle · last active 5m ago'],
      ['working', undefined, 'Working'],
      ['working', NaN, 'Working'],
      ['working', now + 120_000, 'Working'],
    ] as const;
    render(AgentList, {
      agents: rows.map(([status, status_since_at], index) => agent({
        pane_id: `relay-a::clock-${index}`, status, status_since_at, statusClockGroup: agentStatusGroup({ status }),
        name: `clock-${index}`, last_active_at: now - 5 * 60_000,
      })),
      relays: [], responding: new Set<string>(), onopen: vi.fn(),
    });
    const row = (index: number) => screen.getByRole('button', { name: `Open clock-${index} on Laptop` });
    rows.forEach(([, , label], index) => expect(row(index)).toHaveAccessibleDescription(label));
    await vi.advanceTimersByTimeAsync(60_000);
    expect(row(0)).toHaveAccessibleDescription('Working · 1m');
    expect(row(2)).toHaveAccessibleDescription('New result · 3m ago');
  });

  it('sends a detected terminal menu action through the serialized key path', async () => {
    const user = userEvent.setup();
    const current = agent({ server_session_id: 'primary', terminal_id: 'terminal-1', generation: 1 });
    vi.spyOn(relayStore, 'readPane').mockImplementation(() => undefined);
    vi.spyOn(relayStore, 'loadSlashCommands').mockResolvedValue({ commands: [], truncated: false });
    const send = vi.spyOn(relayStore, 'sendToAgent').mockResolvedValue({
      type: 'command_result', request_id: 'keys-1', ok: true,
    });
    render(TerminalView, {
      agent: current,
      allAgents: [current],
      frame: {
        paneId: current.pane_id,
        content: 'Choose a model\n↑/↓ to navigate · Enter to select · Esc to cancel',
        format: 'plain',
      },
      responding: new Set<string>(),
    });

    expect(screen.getByLabelText('Terminal menu: Choose a model')).toBeVisible();
    await user.click(screen.getByRole('button', { name: /Select/ }));
    expect(send).toHaveBeenCalledWith(current, {
      type: 'send_keys', keys: ['Enter'], activity_label: 'Select',
    });
  });
});
