import { describe, expect, it, vi } from 'vitest';
import { CorrelatedInventoryFixture } from './correlated-inventory-fixture';

describe('correlated synthetic relay deltas', () => {
  it('preserves omitted delta fields while retaining coherent poll state', () => {
    const reply = vi.fn();
    const peer = new CorrelatedInventoryFixture(reply);
    const row = { pane_id: 'pane', server_session_id: 'server', terminal_id: 'terminal', generation: 2,
      agent_session_id: 'session', status: 'blocked', agent: 'claude', attention_kind: 'question',
      interaction: { id: 'question' }, pane_revision: 10 };
    peer.server({ type: 'agents', agents: [row] });
    const delta = peer.server({ type: 'agent_update', pane_id: 'pane', status: 'blocked', pane_revision: 11 });
    expect(delta).toMatchObject({ server_session_id: 'server', terminal_id: 'terminal', generation: 2, agent_session_id: 'session' });
    expect(delta).not.toHaveProperty('attention_kind');
    expect(delta).not.toHaveProperty('interaction');
    expect(delta).not.toHaveProperty('agent');
    peer.flush();
    expect(reply).not.toHaveBeenCalled();
    peer.client(JSON.stringify({ type: 'refresh_agents', snapshot_request_id: 'A'.repeat(22) }));
    peer.flush();
    expect(reply).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({
      type: 'inventory_snapshot', snapshot_request_id: 'A'.repeat(22),
      agents: [expect.objectContaining({ ...row, pane_revision: 11 })],
    }));
  });

  it('does not replace coherent poll state with a stale delta', () => {
    const reply = vi.fn();
    const peer = new CorrelatedInventoryFixture(reply);
    peer.server({ type: 'agents', agents: [{ pane_id: 'pane', status: 'working', pane_revision: 12 }] });
    peer.server({ type: 'agent_update', pane_id: 'pane', status: 'blocked', pane_revision: 11 });
    peer.client(JSON.stringify({ type: 'refresh_agents', snapshot_request_id: 'A'.repeat(22) }));
    peer.flush();
    expect(reply).toHaveBeenCalledExactlyOnceWith(expect.objectContaining({
      agents: [expect.objectContaining({ status: 'working', pane_revision: 12 })],
    }));
  });
});
