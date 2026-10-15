import { afterEach, describe, expect, it, vi } from 'vitest';
import { relayStore } from '$lib/store';
import type { Agent } from '$lib/types';

const agent: Agent = {
  relay_id: 'relay', relay_label: 'Fixture', raw_pane_id: 'pane', pane_id: 'relay::pane',
  agent: 'pi', status: 'idle', server_session_id: 'primary', terminal_id: 'terminal', generation: 1,
  agent_session_id: '/tmp/disposable-session.jsonl',
};
const credential = {
  kind: 'credential' as const, id: 'credential-fixture', version: 1, secret: 'C'.repeat(43),
  deviceId: 'device-fixture', role: 'controller' as const, locale: 'en', issuedAt: 0, invitationId: 'invitation-fixture',
};

afterEach(() => vi.restoreAllMocks());

describe('Pi scroll store admission', () => {
  it.each(['reader', 'absent'] as const)('refuses %s before transport', async (role) => {
    vi.spyOn(relayStore, 'deviceCredential').mockReturnValue(role === 'reader' ? { ...credential, role } : null);
    const send = vi.spyOn(relayStore, 'sendCommand');
    await expect(relayStore.scrollAgent(agent, 'up', 'revision')).rejects.toThrow(/controller/);
    await expect(relayStore.setScrollSelection(agent, 'local', 'revision')).rejects.toThrow(/controller/);
    expect(send).not.toHaveBeenCalled();
  });

  it('sends exact native target and captured revision without coordinates or keys', async () => {
    vi.spyOn(relayStore, 'deviceCredential').mockReturnValue(credential);
    const send = vi.spyOn(relayStore, 'sendCommand').mockResolvedValue({ type: 'command_result', request_id: 'fixture', ok: true, phase: 'confirmed' });
    await relayStore.scrollAgent(agent, 'down', 'wheel-revision');
    await relayStore.setScrollSelection(agent, 'local', 'selection-revision');
    const target = { relay_id: 'relay', server_session_id: 'primary', pane_id: 'pane', terminal_id: 'terminal', generation: 1, agent_session_id: agent.agent_session_id };
    expect(send.mock.calls).toEqual([
      ['relay', { type: 'scroll_agent', direction: 'down', state_revision: 'wheel-revision', pane_id: 'pane', target, server_session_id: 'primary' }, 1000, false, undefined],
      ['relay', { type: 'set_scroll_selection', selection: 'local', state_revision: 'selection-revision', pane_id: 'pane', target, server_session_id: 'primary' }, 1000, false, undefined],
    ]);
  });

  it('does not silently substitute another target for an incomplete identity', async () => {
    vi.spyOn(relayStore, 'deviceCredential').mockReturnValue(credential);
    const send = vi.spyOn(relayStore, 'sendCommand');
    await expect(relayStore.scrollAgent({ ...agent, terminal_id: undefined }, 'up', 'revision')).rejects.toThrow(/exact terminal identity/);
    expect(send).not.toHaveBeenCalled();
  });
});
