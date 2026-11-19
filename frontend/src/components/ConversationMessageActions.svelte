<script lang="ts">
  import Button from '$components/ui/Button.svelte';
  import CopyIcon from '$components/ui/CopyIcon.svelte';
  import { copyText } from '$lib/clipboard';
  import { fencedCodeText } from '$lib/markdown';
  import { securityState } from '$lib/security';
  import {
    toggleSpeech as toggleResponseSpeech,
    speechEnabled,
    speechLanguage,
    speechLanguageLabel,
    speechState,
  } from '$lib/speech';
  import { relayStore } from '$lib/store';
  import type { Agent, ConversationEntry } from '$lib/types';

  let { entry, agent, agentName }: { entry: ConversationEntry; agent: Agent; agentName: string } = $props();
  const connections = relayStore.connections;
  const code = $derived(fencedCodeText(entry.text));

  function toggleSpeech(text: string): void {
    void toggleResponseSpeech(
      text,
      $connections.get(agent.relay_id)?.speechLanguages ?? [],
      (chunk, language) => relayStore.speakToAgent(agent, chunk, language),
      (message) => relayStore.showToast(message, true),
    );
  }

  async function copyMarkdown(entry: ConversationEntry) {
    if (!entry.text || !navigator.clipboard?.writeText) {
      relayStore.showToast('Clipboard access is unavailable. Select the text manually.', true);
      return;
    }
    if (await copyText(entry.text)) {
      relayStore.showToast('Markdown copied.');
    } else {
      relayStore.showToast('Could not copy. Select it manually.', true);
    }
  }
  async function copyCode(code: string) {
    if ($securityState.locked || !navigator.clipboard?.writeText) {
      relayStore.showToast('Clipboard access is unavailable while the app is locked.', true);
      return;
    }
    if (await copyText(code)) {
      relayStore.showToast('Code copied.');
    } else {
      relayStore.showToast('Could not copy. Select it manually.', true);
    }
  }
</script>

{#if entry.role === 'assistant' && entry.text && $speechEnabled}
  <Button
    variant="ghost"
    size="sm"
    aria-label={$speechState === 'speaking' ? 'Stop reading response' : 'Read response aloud'}
    title={$speechState === 'speaking' ? 'Stop reading' : `Read aloud in ${speechLanguageLabel($speechLanguage)}`}
    onclick={() => toggleSpeech(entry.text)}
  >{$speechState === 'speaking' ? 'Stop' : 'Speak'}</Button>
{/if}
{#if entry.text}
  <Button
    class="copy-conversation-markdown"
    variant="ghost"
    size="icon"
    aria-label={`Copy ${entry.role === 'user' ? 'your' : agentName} message as Markdown`}
    title="Copy Markdown"
    onclick={() => copyMarkdown(entry)}
  >
    <CopyIcon />
  </Button>
{/if}
{#if code}
  <Button
    class="copy-conversation-code"
    variant="ghost"
    size="icon"
    disabled={$securityState.locked}
    aria-label={`Copy code from ${entry.role === 'user' ? 'your' : agentName} message`}
    title="Copy code"
    onclick={() => copyCode(code)}
  >&lt;/&gt;</Button>
{/if}
