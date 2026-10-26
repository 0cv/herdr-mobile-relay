<script lang="ts">
  import { onDestroy } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import KeyboardIcon from '$components/ui/KeyboardIcon.svelte';
  import { agentNeedsResponse, attentionKind, approvalButtonTone, approvalOptions, questionInteraction } from '$lib/agents';
  import { relayStore } from '$lib/store';
  import { AppReloadHeld, importOrReload } from '$lib/updates';
  import type { Agent } from '$lib/types';

  let { agent, readOnly = false, responding = false, blocked = false }:
    { agent: Agent; readOnly?: boolean; responding?: boolean; blocked?: boolean } = $props();
  const pending = $derived(!readOnly && agentNeedsResponse(agent));
  const interaction = $derived(questionInteraction(agent));
  const options = $derived(approvalOptions(agent));
  let destroyed = false;
  onDestroy(() => { destroyed = true; });
  const questionPending = () => !destroyed && pending && attentionKind(agent) === 'question';
</script>

{#if pending && attentionKind(agent) === 'question' && interaction}
  {#await importOrReload(() => import('./QuestionForm.svelte'), questionPending)}
    <p role="status">Loading question controls…</p>
  {:then { default: Form }}
    <Form {agent} {interaction} {responding} {blocked} />
  {:catch error}
    <p role="alert">{#if error instanceof AppReloadHeld}{error.message}{:else}Question controls could not load. In Terminal, tap <KeyboardIcon label="Keyboard" inline /> to type an answer.{/if}</p>
  {/await}
{:else if pending && attentionKind(agent) === 'approval'}
  <div class="quick-actions" aria-label="Approval choices">
    {#each options as option, index (`${index}:${option}`)}
      <Button disabled={responding || blocked}
        variant={approvalButtonTone(option, index, options.length) === 'deny' ? 'danger' : approvalButtonTone(option, index, options.length) === 'trust' ? 'trust' : 'default'}
        onclick={() => { if (!readOnly && !responding && !blocked) relayStore.respond(agent, index, options.length, option); }}
      >{option}</Button>
    {/each}
  </div>
{/if}
