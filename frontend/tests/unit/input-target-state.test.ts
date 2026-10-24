import { expect, it } from 'vitest';
import { freshAgentStatus, inputContextIdentity } from '$lib/input-target-state';
import type { Agent, RelayConnectionView } from '$lib/types';

const agent = { relay_id: 'relay', server_session_id: 'server', raw_pane_id: 'pane', pane_id: 'relay::pane', terminal_id: 'terminal', generation: 1, agent_session_id: 'session', agent: 'pi', status: 'idle', cwd: '/fixture' } as Agent;
const healthy = { status: 'connected', inventory: { state: 'ready', stale: false } } as RelayConnectionView;

it.each(['relay_id', 'server_session_id', 'raw_pane_id', 'terminal_id', 'generation', 'agent_session_id', 'agent', 'cwd'] as const)('fences the input context across %s replacement', field => {
  expect(inputContextIdentity({ ...agent, [field]: field === 'generation' ? 2 : 'replacement' })).not.toBe(inputContextIdentity(agent));
});

it('ignores activity and presentation metadata without weakening target identity', () => {
  expect(inputContextIdentity({ ...agent, status: 'working', project: 'Renamed' })).toBe(inputContextIdentity(agent));
  expect(inputContextIdentity({ ...agent, terminal_id: '' })).not.toBe(inputContextIdentity(agent));
});

it('keeps distinct incomplete input contexts separate without granting target validity', () => {
  const incomplete = { ...agent, terminal_id: '' };
  for (const field of ['relay_id', 'server_session_id', 'raw_pane_id', 'terminal_id', 'generation', 'agent_session_id', 'agent', 'cwd'] as const) {
    const value = field === 'generation' ? 2 : field === 'terminal_id' ? '?' : 'replacement';
    expect(inputContextIdentity({ ...incomplete, [field]: value })).not.toBe(inputContextIdentity(incomplete));
  }
});

it.each([
  undefined,
  { status: 'disconnected' },
  { status: 'connecting' },
  { status: 'connected' },
  { ...healthy, inventory: { state: 'starting', stale: false } },
  { ...healthy, inventory: { state: 'error', stale: true } },
  { ...healthy, inventory: { state: 'ready', stale: true } },
])('requires a fresh connected inventory rather than %j', view => {
  expect(freshAgentStatus(view as RelayConnectionView | undefined)).toBe(false);
});

it('accepts a fresh connected inventory', () => {
  expect(freshAgentStatus(healthy)).toBe(true);
});
