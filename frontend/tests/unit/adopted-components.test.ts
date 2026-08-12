import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import AgentList from '$components/AgentList.svelte';
import GlobalJump from '$components/GlobalJump.svelte';
import TerminalView from '$components/TerminalView.svelte';
import { relayStore } from '$lib/store';
import type { Agent } from '$lib/types';

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
    expect(workspace?.querySelector('.agent-age')?.textContent).toBe('2h');
    await view.rerender({
      agents: [{ ...primary, activity_seq: 2 }, { ...secondary }],
      relays: [],
      responding: new Set<string>(),
      onopen,
    });

    expect(workspace?.open).toBe(true);
  });

  it('sends a detected terminal menu action through the serialized key path', async () => {
    const user = userEvent.setup();
    const current = agent();
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
