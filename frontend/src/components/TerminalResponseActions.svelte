<script lang="ts">
  import Button from '$components/ui/Button.svelte';
  import { agentKind } from '$lib/agents';
  import { copyText } from '$lib/clipboard';
  import { relayStore } from '$lib/store';
  import {
    toggleSpeech,
    speechEnabled,
    speechLanguage,
    speechLanguageLabel,
    speechState,
  } from '$lib/speech';
  import { latestCompletedResponse } from '$lib/terminal';
  import type { Agent, TerminalFrame } from '$lib/types';

  const connections = relayStore.connections;

  let {
    agent,
    readOnly,
    frame,
    plainText,
    busy,
  }: {
    agent: Agent;
    readOnly: boolean;
    frame?: TerminalFrame;
    plainText: string;
    busy: boolean;
  } = $props();

  let transcriptElement = $state<HTMLTextAreaElement>(null!);
  let responseElement = $state<HTMLTextAreaElement>(null!);
  let agentResponsePreviewElement = $state<HTMLTextAreaElement>(null!);
  let copiedAgentResponseText = $state('');
  let copyingAgentResponse = $state(false);
  let fetchingSpeechText = $state(false);
  const RESPONSE_COPY_KINDS = new Set(['hermes', 'claude', 'codex', 'kimi', 'omp', 'pi', 'qoder']);
  const agentResponseCopySupported = $derived.by(() => {
    const connection = $connections.get(agent.relay_id);
    return Boolean(
      connection?.capabilities.includes('agent_response_copy')
      && RESPONSE_COPY_KINDS.has(agentKind(agent.agent)),
    );
  });
  const relaySpeechLanguages = $derived($connections.get(agent.relay_id)?.speechLanguages ?? []);
  const terminalCopyText = $derived(latestCompletedResponse(frame?.content || ''));

  interface AgentResponseSource {
    text: string;
    /** The agent's own text (relay copy or its transcript), not a terminal parse. */
    exact: boolean;
    failure: string;
  }

  /**
   * The agent's latest complete response, from the most exact source this
   * device may use. A controller runs the relay's copy transaction, which types
   * into the agent's terminal; a reader may not, so it reads the agent's own
   * transcript instead. The terminal parse is the last resort for both.
   * `haltOnCopyFailure` returns the relay's failure without a fallback.
   */
  async function latestAgentResponse(haltOnCopyFailure = false): Promise<AgentResponseSource> {
    let failure = '';
    if (!readOnly && agentResponseCopySupported) {
      try {
        const result = await relayStore.sendToAgent(agent, { type: 'copy_agent_response' }, 15_000);
        const text = String(result.data?.text || '');
        if (text.trim()) return { text, exact: true, failure: '' };
      } catch (error) {
        failure = error instanceof Error && error.message ? error.message : 'Could not copy the agent response.';
        if (haltOnCopyFailure) return { text: '', exact: false, failure };
      }
    }
    if (agent.conversation_history_available) {
      try {
        const page = await relayStore.getConversationHistory(agent, { limit: 8 });
        const latest = page.entries.findLast((entry) => entry.role === 'assistant' && entry.text.trim());
        if (latest) return { text: latest.text, exact: true, failure: '' };
      } catch (error) {
        failure ||= error instanceof Error && error.message ? error.message : 'Could not read the conversation.';
      }
    }
    return { text: terminalCopyText, exact: false, failure };
  }

  async function speakTerminalResponse() {
    if (fetchingSpeechText && $speechState !== 'speaking') return;
    try {
      await toggleSpeech(
        () => {
          fetchingSpeechText = true;
          return latestAgentResponse();
        },
        relaySpeechLanguages,
        (chunk, language) => relayStore.speakToAgent(agent, chunk, language),
        (message) => relayStore.showToast(message, true),
      );
    } finally {
      fetchingSpeechText = false;
    }
  }

  async function copyTerminalOutput() {
    copiedAgentResponseText = '';
    let text = '';
    let copiedAgentResponse = false;
    copyingAgentResponse = true;
    try {
      const response = await latestAgentResponse(true);
      if (response.failure && !response.text.trim()) {
        relayStore.showToast(response.failure, true);
        return;
      }
      if (response.exact) {
        text = response.text;
        copiedAgentResponse = true;
        copiedAgentResponseText = response.text;
      }
    } finally {
      copyingAgentResponse = false;
    }
    if (!text) text = terminalCopyText || plainText;
    if (!text.trim()) {
      relayStore.showToast('No terminal output is available to copy.', true);
      return;
    }
    const hasCompletedResponse = copiedAgentResponse || Boolean(terminalCopyText.trim());
    const target = hasCompletedResponse ? responseElement : transcriptElement;
    const copiedMessage = copiedAgentResponse
      ? 'Agent response copied.'
      : hasCompletedResponse
        ? 'Final response copied.'
        : 'Copied the visible terminal output.';
    const selectedMessage = copiedAgentResponse
      ? 'Output selected. Use your browser Copy command.'
      : hasCompletedResponse
        ? 'Final response selected. Use your browser Copy command.'
        : 'Output selected. Use your browser Copy command.';
    if (await copyText(text)) {
      relayStore.showToast(copiedMessage);
    } else {
      target.value = text;
      target.focus({ preventScroll: true });
      target.select();
      relayStore.showToast(selectedMessage);
    }
  }

  async function copyDisplayedAgentResponse() {
    const text = copiedAgentResponseText;
    if (!text.trim()) return;
    if (await copyText(text)) {
      relayStore.showToast('Agent response copied.');
    } else {
      agentResponsePreviewElement.focus({ preventScroll: true });
      agentResponsePreviewElement.select();
      relayStore.showToast('Output selected. Use your browser Copy command.');
    }
  }

  function dismissCopiedAgentResponse() {
    copiedAgentResponseText = '';
  }
</script>

{#snippet copyIcon()}
  <svg class="action-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <rect x="9" y="9" width="11" height="12" rx="2"></rect>
    <path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"></path>
  </svg>
{/snippet}

<textarea
  class="sr-only"
  aria-label="Full terminal transcript"
  readonly
  tabindex="-1"
  bind:this={transcriptElement}
  value={plainText}
></textarea>
<textarea
  class="sr-only"
  aria-label="Latest final response"
  readonly
  tabindex="-1"
  bind:this={responseElement}
  value={terminalCopyText}
></textarea>
{#if copiedAgentResponseText}
  <section class="agent-response-preview" aria-label="Copied agent response">
    <div class="agent-response-preview-header">
      <strong>Markdown response</strong>
      <div class="agent-response-preview-actions">
        <Button variant="secondary" size="sm" onclick={copyDisplayedAgentResponse}>Copy markdown</Button>
        <Button variant="ghost" size="sm" onclick={dismissCopiedAgentResponse}>Dismiss</Button>
      </div>
    </div>
    <textarea
      aria-label="Copied agent response markdown"
      readonly
      bind:this={agentResponsePreviewElement}
      value={copiedAgentResponseText}
    ></textarea>
  </section>
{/if}
<div class="terminal-copy">
  <Button
    variant="secondary"
    size="sm"
    aria-label={copyingAgentResponse ? 'Copying…' : 'Copy'}
    aria-busy={copyingAgentResponse}
    title={copyingAgentResponse ? 'Copying…' : 'Copy output'}
    disabled={copyingAgentResponse || busy}
    onclick={copyTerminalOutput}
  >{@render copyIcon()}</Button>
  {#if $speechEnabled}
    <Button
      variant="secondary"
      size="sm"
      aria-label={$speechState === 'speaking' ? 'Stop reading response' : 'Read latest response aloud'}
      title={$speechState === 'speaking' ? 'Stop reading' : `Read latest response in ${speechLanguageLabel($speechLanguage)}`}
      aria-busy={fetchingSpeechText}
      disabled={fetchingSpeechText}
      onclick={() => { void speakTerminalResponse(); }}
    >{$speechState === 'speaking' ? 'Stop' : 'Speak'}</Button>
  {/if}
</div>
