import { createDemoScene, DEMO_TIME_MS } from './scene';

export function createDemoResponder(computerId: string) {
  const computer = createDemoScene().find(({ id }) => id === computerId);
  if (!computer) throw new Error('Unknown demo computer.');

  function inventoryStatus() {
    return {
      state: 'ready' as const,
      error_code: '',
      message: '',
      last_attempt_at: DEMO_TIME_MS / 1_000,
      last_success_at: DEMO_TIME_MS / 1_000,
      stale: false,
    };
  }

  const inventory = () => [
    { type: 'agents' as const, agents: structuredClone(computer.agents) },
    { type: 'workspaces' as const, workspaces: structuredClone(computer.workspaces) },
  ];

  return {
    initialMessages() {
      return [{
        type: 'push_config' as const,
        protocol: 3,
        host: computer.label,
        home: computer.home,
        capabilities: [...computer.capabilities],
        inventory: inventoryStatus(),
      }, ...inventory()];
    },
    respond({ type, request_id }: Record<string, unknown>) {
      if (type === 'ping') return [{ type: 'pong' as const }];
      if (type === 'refresh_agents') return [{ type: 'inventory_status' as const, ...inventoryStatus() }, ...inventory()];
      return [{
        type: 'error' as const,
        error: { code: 'unknown_action' },
        detail: 'This action is not available in the demo.',
        ...(typeof request_id === 'string' && request_id ? { request_id } : {}),
      }];
    },
  };
}
