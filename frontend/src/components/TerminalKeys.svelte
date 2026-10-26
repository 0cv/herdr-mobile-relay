<script module lang="ts">
  import type { Agent } from '$lib/types';

  export interface QueuedKeyCommand {
    keys: string[];
    label: string;
    target: Agent;
    resolve: (sent: boolean) => void;
  }
</script>

<script lang="ts">
  import { onDestroy, untrack, type Snippet } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import { targetRefForAgent, targetRefMatchesAgent } from '$lib/resource-id';
  import { relayStore } from '$lib/store';

  let { agent, readOnly = false, blocked = false, sending = $bindable(false), extra, queue }:
    { agent: Agent; readOnly?: boolean; blocked?: boolean; sending?: boolean; extra?: Snippet; queue?: QueuedKeyCommand[] } = $props();
  let modifierInputElement = $state<HTMLInputElement>(null!);
  let arrowsOpen = $state(false);
  let fkeysOpen = $state(false);
  let ctrlArmed = $state(false);
  let shiftArmed = $state(false);
  let altArmed = $state(false);
  let keyFeedback = $state('');
  let keyFeedbackError = $state(false);
  const keySending = $derived(sending || blocked);
  const disabled = $derived(readOnly || blocked || !targetRefForAgent(agent));
  const FUNCTION_KEYS = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12];
  let componentMounted = true;
  let keyFeedbackTimer: ReturnType<typeof setTimeout> | undefined;
  let keyReadTimer: ReturnType<typeof setTimeout> | undefined;
  // A queue owned by the parent survives replacement of this row.
  const sharedQueue = untrack(() => queue);
  const keyQueue: QueuedKeyCommand[] = sharedQueue ?? [];
  const armedModifierLabel = $derived([
    ctrlArmed ? 'Ctrl' : '',
    altArmed ? 'Alt' : '',
    shiftArmed ? 'Shift' : '',
  ].filter(Boolean).join('+'));
  const keyControlStatus = $derived(armedModifierLabel
    ? `${armedModifierLabel} armed${keyFeedback ? ` · ${keyFeedback}` : ' — choose a key or type a character'}`
    : keyFeedback);
  export function sendKeys(keys: string[], activityLabel = ''): Promise<boolean> {
    if (disabled) return Promise.resolve(false);
    return new Promise((resolve) => {
      keyQueue.push({ keys, label: activityLabel || keys.join(', '), target: { ...agent }, resolve });
      void drainKeyQueue();
    });
  }

  // A replacement row can inherit an in-flight send from the question row.
  // Resume its own queue when that shared send (or blocking state) clears.
  $effect(() => {
    if (!keySending && keyQueue.length) void drainKeyQueue();
  });

  async function drainKeyQueue() {
    if (keySending) return;
    sending = true;
    while (keyQueue.length) {
      if (!componentMounted) break;
      const command = keyQueue.shift()!;
      const identity = targetRefForAgent(command.target);
      if (disabled || !identity || !targetRefMatchesAgent(identity, agent)) {
        command.resolve(false);
        continue;
      }
      showKeyFeedback(`Sending ${command.label}…`);
      try {
        await relayStore.sendToAgent(command.target, {
          type: 'send_keys',
          keys: command.keys,
          activity_label: command.label,
        });
        command.resolve(true);
        showKeyFeedback(`${command.label} sent`);
        if (keyReadTimer) clearTimeout(keyReadTimer);
        keyReadTimer = setTimeout(() => {
          if (componentMounted && targetRefMatchesAgent(identity, agent)) relayStore.readPane(command.target);
        }, 300);
      } catch (error) {
        command.resolve(false);
        const message = error instanceof Error ? error.message : 'Terminal keys could not be sent.';
        showKeyFeedback('Key send failed', true);
        relayStore.showToast(message, true);
        for (const queued of keyQueue.splice(0)) queued.resolve(false);
      }
    }
    sending = false;
  }

  function showKeyFeedback(message: string, error = false) {
    if (!componentMounted) return;
    if (keyFeedbackTimer) clearTimeout(keyFeedbackTimer);
    keyFeedback = message;
    keyFeedbackError = error;
    keyFeedbackTimer = setTimeout(() => {
      keyFeedback = '';
      keyFeedbackError = false;
    }, 2_000);
  }

  function toggleModifier(which: 'ctrl' | 'alt' | 'shift') {
    if (disabled) return;
    arrowsOpen = false;
    fkeysOpen = false;
    if (which === 'ctrl') ctrlArmed = !ctrlArmed;
    else if (which === 'alt') altArmed = !altArmed;
    else shiftArmed = !shiftArmed;
    if (ctrlArmed || altArmed || shiftArmed) {
      modifierInputElement.value = '';
      modifierInputElement.focus();
    } else {
      modifierInputElement.blur();
    }
  }

  function toggleCtrl() {
    toggleModifier('ctrl');
  }

  function toggleAlt() {
    toggleModifier('alt');
  }

  function toggleShift() {
    toggleModifier('shift');
  }

  function modifierChord(key: string): { chord: string; label: string } | null {
    const parts: string[] = [];
    const labels: string[] = [];
    if (ctrlArmed) { parts.push('ctrl'); labels.push('Ctrl'); }
    if (altArmed) { parts.push('alt'); labels.push('Alt'); }
    if (shiftArmed) { parts.push('shift'); labels.push('Shift'); }
    if (!parts.length) return null;
    parts.push(key.toLocaleLowerCase());
    labels.push(key.length === 1 ? key.toLocaleUpperCase() : key[0].toLocaleUpperCase() + key.slice(1).toLocaleLowerCase());
    return { chord: parts.join('+'), label: labels.join('+') };
  }

  function disarmModifiers() {
    ctrlArmed = false;
    altArmed = false;
    shiftArmed = false;
    modifierInputElement.blur();
  }

  function modifierInput(event: Event) {
    const target = event.currentTarget as HTMLInputElement;
    const character = Array.from(target.value)[0] || '';
    target.value = '';
    if (!character || /\s/u.test(character)) return;
    sendTerminalKey(character);
  }

  function sendTerminalKey(key: string, plainLabel = key) {
    const result = modifierChord(key);
    void sendKeys([result?.chord || key], result?.label || plainLabel);
  }

  function sendTab() {
    sendTerminalKey('Tab');
  }

  function sendFunctionKey(number: number) {
    fkeysOpen = false;
    sendTerminalKey(`f${number}`, `F${number}`);
  }

  function modifierKeydown(event: KeyboardEvent) {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    disarmModifiers();
  }

  function modifierBlur() {
    setTimeout(() => {
      if (document.activeElement !== modifierInputElement) {
        ctrlArmed = false;
        altArmed = false;
        shiftArmed = false;
      }
    });
  }


  onDestroy(() => {
    componentMounted = false;
    if (keyFeedbackTimer) clearTimeout(keyFeedbackTimer);
    if (keyReadTimer) clearTimeout(keyReadTimer);
    if (!sharedQueue) for (const command of keyQueue.splice(0)) command.resolve(false);
  });
</script>
{#snippet arrowIcon()}
  <svg class="button-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="M12 2v20M2 12h20"></path>
    <path d="m8 6 4-4 4 4M8 18l4 4 4-4M6 8l-4 4 4 4M18 8l4 4-4 4"></path>
  </svg>
{/snippet}

{#snippet tabIcon()}
  <svg class="key-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="M3 12h12M11 8l4 4-4 4M20 6v12"></path>
  </svg>
{/snippet}

{#snippet shiftIcon()}
  <svg class="key-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
    <path d="M12 4.5 4 12.5h4v7h8v-7h4z"></path>
  </svg>
{/snippet}

{#snippet arrowPopup()}
  {#if arrowsOpen}
    <div class="arrow-popup">
      <span aria-hidden="true"></span>
      <button disabled={disabled} aria-label="Up" onpointerdown={(event) => event.preventDefault()} onclick={() => sendTerminalKey('Up')}>↑</button>
      <span aria-hidden="true"></span>
      <button disabled={disabled} aria-label="Left" onpointerdown={(event) => event.preventDefault()} onclick={() => sendTerminalKey('Left')}>←</button>
      <span aria-hidden="true"></span>
      <button disabled={disabled} aria-label="Right" onpointerdown={(event) => event.preventDefault()} onclick={() => sendTerminalKey('Right')}>→</button>
      <span aria-hidden="true"></span>
      <button disabled={disabled} aria-label="Down" onpointerdown={(event) => event.preventDefault()} onclick={() => sendTerminalKey('Down')}>↓</button>
      <span aria-hidden="true"></span>
    </div>
  {/if}
{/snippet}

{#snippet fkeyPopup()}
  {#if fkeysOpen}
    <div class="fkey-popup" role="group" aria-label="Function keys">
      {#each FUNCTION_KEYS as number (number)}
        <button
          disabled={disabled}
          onpointerdown={(event) => event.preventDefault()}
          onclick={() => sendFunctionKey(number)}
        >F{number}</button>
      {/each}
    </div>
  {/if}
{/snippet}


<div>
    {#if keyControlStatus}
      <p class:error={keyFeedbackError} class="key-feedback" role="status" aria-live="polite">{keyControlStatus}</p>
    {/if}
    <div class="term-keys" aria-busy={keySending}>
      <Button variant="secondary" size="sm" disabled={disabled} onpointerdown={(event) => event.preventDefault()} onclick={() => sendTerminalKey('Escape', 'Cancelled prompt')}>Esc</Button>
      <Button variant="secondary" size="sm" disabled={disabled} aria-label="Tab" title="Send Tab" onpointerdown={(event) => event.preventDefault()} onclick={sendTab}>{@render tabIcon()}</Button>
      <div class="modifier-menu">
        <input
          id="modifier-key-input"
          class="modifier-key-input"
          bind:this={modifierInputElement}
          disabled={disabled}
          aria-label="Modifier shortcut character"
          aria-hidden={!armedModifierLabel}
          tabindex={armedModifierLabel ? 0 : -1}
          autocomplete="off"
          autocapitalize="none"
          maxlength="1"
          spellcheck="false"
          oninput={modifierInput}
          onkeydown={modifierKeydown}
          onblur={modifierBlur}
        />
        <Button
          variant="secondary"
          size="sm"
          disabled={disabled}
          aria-controls="modifier-key-input"
          aria-pressed={shiftArmed}
          aria-label="Shift"
          title="Arm Shift; combine it with Ctrl or Alt"
          onpointerdown={(event) => event.preventDefault()}
          onclick={toggleShift}
        >{@render shiftIcon()}</Button>
        <Button
          variant="secondary"
          size="sm"
          disabled={disabled}
          aria-controls="modifier-key-input"
          aria-pressed={ctrlArmed}
          aria-label="Ctrl"
          title="Arm Ctrl; combine it with Shift or Alt"
          onpointerdown={(event) => event.preventDefault()}
          onclick={toggleCtrl}
        ><span class="key-caret">^</span></Button>
        <Button
          variant="secondary"
          size="sm"
          disabled={disabled}
          aria-controls="modifier-key-input"
          aria-pressed={altArmed}
          title="Arm Alt; combine it with Ctrl or Shift"
          onpointerdown={(event) => event.preventDefault()}
          onclick={toggleAlt}
        >Alt</Button>
      </div>
      <span class="spacer"></span>
      <!-- Contextual controls sit in the spacer so the fixed keys never move when they appear. -->
      {@render extra?.()}
      <div class="fkey-menu">
        <Button
          variant="secondary"
          size="sm"
          disabled={disabled}
          aria-label="Function keys"
          aria-expanded={fkeysOpen}
          onpointerdown={(event) => event.preventDefault()}
          onclick={() => { fkeysOpen = !fkeysOpen; arrowsOpen = false; }}
        >F keys</Button>
        {@render fkeyPopup()}
      </div>
      <div class="arrow-menu">
        <Button
          variant="secondary"
          size="sm"
          disabled={disabled}
          aria-label="Arrow keys"
          aria-expanded={arrowsOpen}
          onpointerdown={(event) => event.preventDefault()}
          onclick={() => { arrowsOpen = !arrowsOpen; fkeysOpen = false; }}
        >
          {@render arrowIcon()}
        </Button>
        {@render arrowPopup()}
      </div>
      <Button variant="secondary" size="sm" disabled={disabled} aria-label="Enter" onpointerdown={(event) => event.preventDefault()} onclick={() => sendTerminalKey('Enter')}>Enter</Button>
    </div>

</div>
