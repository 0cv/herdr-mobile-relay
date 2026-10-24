import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import AgentList from '$components/AgentList.svelte';
import type { Agent } from '$lib/types';

it('hides and restores servers including attention sessions, and remembers the choice', async () => {
  const relays = ['Mac', 'Spark'].map((label) => ({ id: label, label, url: '', token: '' }));
  const agents: Agent[] = relays.map((relay) => ({
    relay_id: relay.id, relay_label: relay.label, raw_pane_id: 'p1',
    pane_id: `${relay.id}::p1`, project: `${relay.label} project`, agent: 'codex',
    status: 'blocked', attention_kind: 'approval', attention_capable: true,
    command: 'Continue?', options: ['Approve', 'Deny'],
  }));
  const props = { agents, relays, responding: new Set<string>(), onopen: vi.fn() };
  const view = render(AgentList, props);
  await fireEvent.click(screen.getByRole('button', { name: /^Mac/ }));
  expect(screen.queryByText('Mac project')).not.toBeInTheDocument();
  expect(screen.getByText('Spark project')).toBeInTheDocument();
  view.unmount();
  render(AgentList, props);
  expect(screen.getByRole('button', { name: /^Mac/ })).toHaveAttribute('aria-pressed', 'false');
  expect(screen.getByRole('img', { name: '1 need input while hidden' })).toBeInTheDocument();
  expect(screen.queryByText('Mac project')).not.toBeInTheDocument();
  await fireEvent.click(screen.getByRole('button', { name: /^Spark/ }));
  expect(screen.getByText('All servers are hidden. Select a server above to show its sessions.')).toBeInTheDocument();
  await fireEvent.click(screen.getByRole('button', { name: /^Mac/ }));
  expect(screen.getByText('Mac project')).toBeInTheDocument();
});
