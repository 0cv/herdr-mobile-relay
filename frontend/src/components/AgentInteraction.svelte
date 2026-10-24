<script lang="ts">
  import Button from '$components/ui/Button.svelte';
  import { agentNeedsResponse, attentionKind, approvalButtonTone, approvalOptions, questionInteraction } from '$lib/agents';
  import { relayStore } from '$lib/store';
  import type { Agent } from '$lib/types';

  let { agent, readOnly = false, responding = false, blocked = false }:
    { agent: Agent; readOnly?: boolean; responding?: boolean; blocked?: boolean } = $props();
  const pending = $derived(!readOnly && agentNeedsResponse(agent));
  const interaction = $derived(questionInteraction(agent));
  const options = $derived(approvalOptions(agent));
</script>

{#if pending && attentionKind(agent) === 'question' && interaction}
  {#await import('./QuestionForm.svelte')}
    <p role="status">Loading question controls…</p>
  {:then { default: Form }}
    <Form {agent} {interaction} {responding} {blocked} />
  {:catch}
    <p role="alert">Question controls could not load. Open Terminal to use Type in terminal.</p>
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
