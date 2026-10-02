/** Synthetic upgraded relay: replies to an admitted request with its current
 * inventory, never infers authority from an unrelated frame's arrival time.
 * Negative correlation/old-peer tests use their own non-auto-reply transport.
 */
export class CorrelatedInventoryFixture {
  private agents: Record<string, unknown>[] = [];
  private workspaces: unknown[] = [];
  private inventory: Record<string, unknown> = { state: 'ready', stale: false };
  private nonce: string | null = null;

  constructor(private readonly reply: (message: Record<string, unknown>) => void) {}

  client(payload: string): void {
    const message = JSON.parse(payload);
    if (message.type === 'refresh_agents' && typeof message.snapshot_request_id === 'string') {
      this.nonce = message.snapshot_request_id;
    }
  }

  /** A server poll explicitly completes; a silent/half-open path never replies. */
  flush(): void {
    if (!this.nonce) return;
    const nonce = this.nonce;
    this.nonce = null;
    this.reply({ type: 'inventory_snapshot', snapshot_request_id: nonce,
      inventory: this.inventory, agents: this.agents, workspaces: this.workspaces });
  }

  server(payload: unknown): unknown {
    if (!payload || typeof payload !== 'object') return payload;
    const message = { ...payload } as Record<string, any>;
    if (message.type === 'push_config') {
      this.inventory = { state: 'ready', stale: false, ...(message.inventory || {}) };
      message.inventory = this.inventory;
      message.capabilities = [...new Set([...(message.capabilities || []), 'inventory_snapshot_v1'])];
    }
    if (message.type === 'inventory_status') {
      this.inventory = { stale: false, ...message };
    }
    if (message.type === 'agents' && Array.isArray(message.agents)) {
      this.agents = message.agents.map((agent: Record<string, unknown>) => this.identity(agent));
      message.agents = this.agents;
    }
    if (message.type === 'workspaces' && Array.isArray(message.workspaces)) this.workspaces = message.workspaces;
    if (['agent_update', 'blocked'].includes(message.type) && typeof message.pane_id === 'string') {
      const index = this.agents.findIndex((agent) => agent.pane_id === message.pane_id);
      Object.assign(message, this.identity({ ...(index < 0 ? {} : this.agents[index]), ...message }));
      const agent: Record<string, any> = { ...message, status: message.type === 'blocked' ? 'blocked' : message.status };
      if (index < 0) this.agents.push(agent);
      else if (typeof agent.pane_revision !== 'number' || typeof this.agents[index].pane_revision !== 'number'
        || agent.pane_revision >= (this.agents[index].pane_revision as number)) this.agents[index] = agent;
    }
    return message;
  }

  private identity(agent: Record<string, unknown>): Record<string, unknown> {
    return { server_session_id: 'primary', terminal_id: `terminal-${agent.pane_id}`, generation: 1,
      agent_session_id: '', ...agent };
  }
}
