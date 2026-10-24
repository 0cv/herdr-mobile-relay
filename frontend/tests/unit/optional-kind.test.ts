import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, expect, it, vi } from 'vitest';
import AgentList from '$components/AgentList.svelte';
import GlobalJump from '$components/GlobalJump.svelte';
import { agentOpeningView } from '$lib/agent-view';
import { targetRefForAgent } from '$lib/resource-id';
import { setHomeLayout } from '$lib/preferences';
import { workspaceGroups } from '$lib/workspaces';
import type { Agent, RelayConnectionView } from '$lib/types';

const agent = { relay_id: 'relay', relay_label: 'Laptop', raw_pane_id: 'pane', pane_id: 'relay::pane', server_session_id: 'server', terminal_id: 'terminal', generation: 1, agent_session_id: '', name: 'Named agent', project: 'Development', agent: '', status: 'unknown', workspace_id: 'workspace', tab_id: 'tab', cwd: '/home/example/Development', conversation_history_available: false } as Agent;
const connection = { status: 'connected', inventory: { state: 'ready', stale: false }, capabilities: [], agentProfiles: [] } as unknown as RelayConnectionView;
afterEach(() => { setHomeLayout('mixed'); });

it('includes the unknown-kind row in an expanded synthetic workspace without Git or workspace metadata', async () => {
  setHomeLayout('mixed');
  const groups = workspaceGroups([agent], []);
  expect(groups).toHaveLength(1);
  expect(groups[0].agents).toEqual([agent]);
  expect(groups[0].cwd).toBe('/home/example/Development');
  const onopen = vi.fn();
  const view = render(AgentList, { agents: [agent], relays: [], workspaces: [], connections: new Map([['relay', connection]]), responding: new Set<string>(), onopen });
  const group = view.container.querySelector<HTMLDetailsElement>('details.workspace-card')!;
  group.open = true;
  await fireEvent(group, new Event('toggle'));
  await fireEvent.click(screen.getByRole('button', { name: 'Open Development on Laptop' }));
  expect(onopen).toHaveBeenCalledWith(agent);
  expect(view.container.querySelector('.agent-logo')).toBeTruthy();
});

it('keeps blank-query Jump inclusive and chooses Terminal rather than guessing native history', async () => {
  const onselect = vi.fn();
  render(GlobalJump, { open: true, agents: [agent], onselect });
  const option = screen.getByRole('button', { name: /Development/ });
  await fireEvent.click(option);
  expect(onselect).toHaveBeenCalledWith(agent);
  expect(agentOpeningView(agent, connection, 'conversation', {})).toMatchObject({ view: 'terminal', paneId: agent.pane_id });
  expect(targetRefForAgent(agent)).not.toBeNull();
  expect(targetRefForAgent({ ...agent, terminal_id: '' })).toBeNull();
});
