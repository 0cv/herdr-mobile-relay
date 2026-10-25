<script lang="ts">
  import { onDestroy, onMount, tick, untrack } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import { relayStore } from '$lib/store';
  import { dispatchFlag } from '$lib/composer';
  import { piPromptDelivery } from '$lib/pi-prompt.svelte';
  import { terminalTextError } from '$lib/terminal-input-policy';
  import { freshAgentStatus } from '$lib/input-target-state';
  import { guardReload } from '$lib/reload-guard';
  import { securityState } from '$lib/security';
  import { terminalDraftState } from '$lib/terminal-draft-state.svelte';
  import { stripAnsi } from '$lib/terminal';
  import type { Agent, TerminalFrame } from '$lib/types';
  import type { TerminalInputDelivery } from '$lib/terminal-input-delivery.svelte';

  let { agent, frame, active, readOnly, delivery, onexit }:
    { agent: Agent; frame?: TerminalFrame; active: boolean; readOnly: boolean; delivery: TerminalInputDelivery; onexit: () => void } = $props();
  const sending = $derived(delivery.pending);
  const uncertain = $derived(delivery.uncertain);
  let panel = $state<HTMLElement>();
  let input = $state<HTMLInputElement>();
  const connections = relayStore.connections;
  const inputState = terminalDraftState(() => agent, () => frame);
  const connection = $derived($connections.get(agent.relay_id));
  const canRead = $derived(active && !readOnly && freshAgentStatus(connection) && !$securityState.locked);
  const ready = $derived(canRead && inputState.ready && Boolean(connection?.capabilities.includes('manual_terminal_input')));
  $effect(() => {
    if (!canRead) return;
    const target = untrack(() => ({ ...agent }));
    const refresh = () => relayStore.readPane(target, true);
    refresh();
    const timer = setInterval(refresh, 2_000);
    return () => clearInterval(timer);
  });
  let text = $state('');
  let feedback = $state('');
  let mounted = true;
  const masked = $derived(frame?.noEcho !== false);
  const piDelivery = $derived(piPromptDelivery(agent));
  const disabled = $derived(!active || !ready || sending || uncertain || piDelivery.pending || piDelivery.uncertain);
  $effect(() => {
    if (!active) { text = ''; return; }
    void tick().then(() => { if (mounted && active) input?.focus(); });
  });
  onDestroy(() => { mounted = false; });
  onMount(() => guardReload(() => (active && text ? 'draft' : null)));
  onMount(() => {
    const changed = () => { if (document.visibilityState === 'hidden') onexit(); };
    document.addEventListener('visibilitychange', changed);
    return () => document.removeEventListener('visibilitychange', changed);
  });

  async function leave() {
    if (sending) return;
    onexit();
    await tick();
    if (mounted && !active && !$securityState.locked && document.visibilityState !== 'hidden') {
      panel?.previousElementSibling?.querySelector<HTMLButtonElement>('button')?.focus();
    }
  }

  function paste(event: ClipboardEvent) {
    const error = terminalTextError(event.clipboardData?.getData('text/plain') || '');
    if (error) { event.preventDefault(); feedback = error; }
  }

  async function send(enter = false) {
    if (disabled) return;
    const submitted = text;
    const error = enter ? '' : terminalTextError(submitted);
    if (error) { feedback = error; return; }
    const attempt = delivery;
    if (!attempt.begin()) return;
    let unconfirmed = true;
    feedback = '';
    try {
      const result = await relayStore.sendToAgent({ ...agent }, enter
        ? { type: 'send_keys', keys: ['Enter'], activity_label: 'Manual Enter' }
        : { type: 'send_terminal_text', text: submitted });
      unconfirmed = result.phase !== 'completed' || !result.ok;
      if (!mounted) return;
      if (!unconfirmed) {
        if (!enter && text === submitted) text = '';
        feedback = enter ? 'Enter delivered.' : 'Text delivered; Enter not sent.';
      }
    } catch (error) {
      unconfirmed = !dispatchFlag(error, 'not_started');
      if (mounted) feedback = 'Delivery could not be confirmed.';
    } finally {
      attempt.finish(unconfirmed);
    }
  }
</script>

<section bind:this={panel} hidden={!active} aria-label="Type in terminal">
  <p id="terminal-input-effect" class="hint">Writes to the terminal’s currently focused control. Characters may act as hotkeys or filters, including in approval menus. Focus can change on the computer. Nothing here is saved on this phone.</p>
  <details>
    <summary>Current terminal context</summary>
    <pre style="max-height:12rem;overflow:auto;white-space:pre-wrap;overflow-wrap:anywhere">{frame ? stripAnsi(frame.content).slice(-12_000) : 'Waiting for terminal context…'}</pre>
  </details>
  <label>
    Terminal text
    <input bind:this={input} aria-describedby="terminal-input-effect" style="width:100%;min-height:44px" type={masked ? 'password' : 'text'} bind:value={text} autocomplete="off" autocorrect="off" autocapitalize="none" spellcheck="false"
      onpaste={paste}
      onbeforeinput={(event) => { if (event.data && terminalTextError(event.data)) { event.preventDefault(); feedback = 'Use one line without control characters. Send Enter separately.'; } }}
      onkeydown={(event) => { if (event.key === 'Enter') event.preventDefault(); }} />
  </label>
  {#if masked}<p class="hint">Input visibility is hidden or unknown; text is masked and transient.</p>{/if}
  <div class="quick-actions">
    <Button disabled={disabled || !text} onclick={() => send()}>Send text</Button>
    <Button disabled={disabled} onclick={() => send(true)}>Enter</Button>
    <Button variant="secondary" disabled={sending} onclick={leave}>Back to prompt and structured controls</Button>
  </div>
  {#if !connection?.capabilities.includes('manual_terminal_input')}
    <p role="status">This relay does not support manual terminal text. No fallback command will be used.</p>
  {:else if !ready}<p role="status">Sending needs a connected controller and fresh exact-target terminal context. Wait for a fresh frame after reconnecting or unlocking.</p>{/if}
  {#if uncertain}
    <p role="alert">Delivery is uncertain. No retry or Enter was sent automatically. Check the terminal before continuing.</p>
    <Button variant="secondary" disabled={!ready || !delivery.canInspect(frame?.readStartedAt)} onclick={() => { if (ready && delivery.inspect(frame?.readStartedAt)) feedback = ''; }}>I checked the terminal</Button>
  {/if}
  {#if feedback}<p role="status">{feedback}</p>{/if}
</section>
