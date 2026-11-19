<script lang="ts">
  import AgentList from '$components/AgentList.svelte';
  import { normalizeAgent } from '$lib/agents';
  import { normalizeAgentInventory, normalizeAgentTargetFields, normalizeWorkspace } from '$lib/wire-normalizers';
  import type { Agent, RelayWorkspace } from '$lib/types';
  import { createDemoResponder } from './responder';
  import { createDemoScene } from './scene';

  const relays = createDemoScene().map(({ id, label }) => ({ id, label, url: '', token: '' }));
  const agents: Agent[] = [];
  const workspaces: RelayWorkspace[] = [];
  const connections = new Map(relays.map(({ id, label }) => {
    const [hello, ...inventory] = createDemoResponder(id).initialMessages();
    if (hello.type !== 'push_config') throw new Error('Missing sample computer configuration.');
    for (const message of inventory) {
      if (message.type === 'agents') {
        agents.push(...message.agents.map((agent) => normalizeAgent(
          id, label, normalizeAgentTargetFields(agent), hello.capabilities.includes('attention_classification'),
        )));
      } else if (message.type === 'workspaces') {
        for (const row of message.workspaces) {
          const workspace = normalizeWorkspace(id, label, row);
          if (workspace) workspaces.push(workspace);
        }
      }
    }
    return [id, {
      status: 'connected' as const,
      home: hello.home,
      capabilities: hello.capabilities,
      inventory: normalizeAgentInventory(hello.inventory),
      pairingDeferred: false,
    }];
  }));
  const responding = new Set<string>();
</script>

<div class="app-shell">
  <header class="app-header">
    <div class="header-title">
      <h1>Demo — sample data</h1>
      <span>Notification delivery: Off (unavailable in demo)</span>
    </div>
  </header>
  <AgentList {agents} {workspaces} {relays} {connections} {responding} />
</div>
