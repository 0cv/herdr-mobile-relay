<script lang="ts">
  import { untrack } from 'svelte';
  import AppSwitch from '$components/ui/AppSwitch.svelte';
  import Card from '$components/ui/Card.svelte';
  import { relayStore } from '$lib/store';
  import type { RelayConnectionView } from '$lib/types';

  let { relayId, label, connection, readOnly = false }: {
    relayId: string; label: string; connection?: RelayConnectionView; readOnly?: boolean;
  } = $props();
  type State = { instance: string; enabled: boolean; profiles: { path: string; status: string; reason?: string }[] };
  let integration = $state<State | null>(null);
  let checked = $derived(integration?.enabled || false);
  let busy = $state(false);
  let error = $state('');
  let generation = 0;
  const connected = $derived(connection?.status === 'connected' && connection.capabilities.includes('pi_integration'));
  const hintId = $props.id();

  $effect(() => {
    const target = relayId;
    generation++;
    integration = null;
    if (!connected) return;
    const refresh = async () => {
      const request = generation;
      try {
        const result = await relayStore.sendCommand(target, { type: 'get_pi_integration' });
        if (request === generation) { integration = result.data as unknown as State; error = ''; }
      } catch (problem) { if (request === generation) error = (problem as Error).message; }
    };
    void untrack(refresh);
    const timer = setInterval(() => { if (!busy) void refresh(); }, 10_000);
    return () => { generation++; clearInterval(timer); };
  });

  async function change(enabled: boolean) {
    if (readOnly || busy || !integration || !connected) return;
    const instance = integration.instance;
    const request = ++generation;
    busy = true;
    error = '';
    try {
      const result = await relayStore.sendCommand(relayId, { type: 'set_pi_integration', instance, enabled });
      if (request === generation) integration = result.data as unknown as State;
    } catch (problem) {
      if (request === generation) { integration = null; error = (problem as Error).message; }
    } finally { busy = false; }
  }
</script>

<Card>
  <h3>{label}</h3>
  <AppSwitch label="Pi Integration" bind:checked disabled={readOnly || busy || !integration || !connected}
    descriptionId={hintId} onchange={change} />
  <p class="hint" id={hintId}>Installs on this computer for all known Pi profiles. Keeps live commands, progress, notices and replies current. Reload Pi to activate changes.</p>
  {#if readOnly}<p class="hint">Controller access required to change this setting.</p>{/if}
  {#if error}<p role="alert">{error}</p>{/if}
  {#each integration?.profiles || [] as profile (profile.path)}
    <p class="hint">{profile.path}: {profile.status}{profile.reason ? ` — ${profile.reason}` : ''}</p>
  {/each}
</Card>
