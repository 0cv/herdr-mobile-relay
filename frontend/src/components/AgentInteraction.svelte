<script lang="ts">
  import { onDestroy } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import KeyboardIcon from '$components/ui/KeyboardIcon.svelte';
  import { approvalButtonVariant, approvalOptions, questionInteraction } from '$lib/agents';
  import { relayStore } from '$lib/store';
  import { AppReloadHeld, importOrReload } from '$lib/updates';
  import type { Agent } from '$lib/types';

  let { agent, readOnly = false, responding = false, blocked = false }:
    { agent: Agent; readOnly?: boolean; responding?: boolean; blocked?: boolean } = $props();
  const interaction = $derived(readOnly ? null : questionInteraction(agent));
  const options = $derived(readOnly ? [] : approvalOptions(agent));
  let destroyed = false;
  onDestroy(() => { destroyed = true; });
  const questionPending = () => !destroyed && Boolean(interaction);
</script>

{#if interaction}
  {#await importOrReload(() => import('./QuestionForm.svelte'), questionPending)}
    <p role="status">Loading question controls…</p>
  {:then { default: Form }}
    <Form {agent} {interaction} {responding} {blocked} />
  {:catch error}
    <p role="alert">{#if error instanceof AppReloadHeld}{error.message}{:else}Question controls could not load. In Terminal, tap <KeyboardIcon label="Keyboard" inline /> to type an answer.{/if}</p>
  {/await}
{:else if options.length}
  <div class="quick-actions" aria-label="Approval choices">
    {#each options as option, index (`${index}:${option}`)}
      <Button disabled={responding || blocked}
        variant={approvalButtonVariant(option, index, options.length)}
        onclick={() => { if (!readOnly && !responding && !blocked) relayStore.respond(agent, index, options.length, option); }}
      >{option}</Button>
    {/each}
  </div>
{/if}
