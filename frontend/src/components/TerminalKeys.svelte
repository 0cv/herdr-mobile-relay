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
  import { onDestroy, tick, untrack, type Snippet } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import { terminalKeyRow } from '$lib/preferences';
  import { isDefaultKeyRow, keyRowLabel } from '$lib/terminal-key-row';
  import { targetRefForAgent, targetRefMatchesAgent } from '$lib/resource-id';
  import { relayStore } from '$lib/store';
  import { keyResultMessage } from '$lib/terminal-key-messages';

  let { agent, readOnly = false, blocked = false, sending = $bindable(false), extra, queue }:
    { agent: Agent; readOnly?: boolean; blocked?: boolean; sending?: boolean; extra?: Snippet; queue?: QueuedKeyCommand[] } = $props();
  let modifierInputElement = $state<HTMLInputElement>(null!);
  const modifiers = ['ctrl', 'alt', 'shift'] as const;
  const armed = $state({ ctrl: false, alt: false, shift: false });
  let menu = $state('');
  let trigger: HTMLButtonElement;
  let row: HTMLDivElement;
  let scrolling = $state(false);
  const defaultOrder = $derived(isDefaultKeyRow($terminalKeyRow));
  const inlineExtra = $derived(defaultOrder && !scrolling);
  const controls = $derived.by(() => {
    const entries = $terminalKeyRow.slice();
    entries.splice(defaultOrder ? 2 : 0, 0, '');
    return entries;
  });

  const keyboard = () => row?.querySelector<HTMLButtonElement>('.manual-input-trigger button');

  $effect.pre(() => {
    void inlineExtra;
    menu = '';
    for (const id of modifiers) if (!$terminalKeyRow.includes(id)) armed[id] = false;
    if (keyboard()?.matches(':focus')) void tick().then(() => keyboard()?.focus());
  });

  $effect(() => {
    if (!armedModifierLabel) modifierInputElement?.blur();
  });

  function alignPopup() {
    if (!menu) return;
    const bounds = row.getBoundingClientRect();
    const end = Math.min(bounds.right, trigger.getBoundingClientRect().right) - bounds.left;
    row.style.setProperty('--key-end', `${end}px`);
  }

  function observeStrip(strip: HTMLElement) {
    if (typeof ResizeObserver === 'undefined') return;
    const entries = strip.firstElementChild!;
    function measure() {
      scrolling = entries.getBoundingClientRect().width > strip.getBoundingClientRect().width + .1;
      alignPopup();
    }
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(strip);
    observer.observe(entries);
    return { destroy: () => observer.disconnect() };
  }
  let keyFeedback = $state('');
  const keySending = $derived(sending || blocked);
  const disabled = $derived(readOnly || blocked || !targetRefForAgent(agent));
  const { connections } = relayStore;
  const pagingKeys = $derived($connections.get(agent.relay_id)?.capabilities.includes('paging_keys'));
  const NAVIGATION_KEYS: ({ key: string; text?: string; name?: string; paging?: boolean } | null)[] = [
    { key: 'Home', paging: true },
    { key: 'Up', text: '↑' },
    { key: 'PageUp', text: 'PgUp', name: 'Page up', paging: true },
    { key: 'Left', text: '←' },
    null,
    { key: 'Right', text: '→' },
    { key: 'End', paging: true },
    { key: 'Down', text: '↓' },
    { key: 'PageDown', text: 'PgDn', name: 'Page down', paging: true },
  ];
  const FUNCTION_KEYS = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12];
  let componentMounted = true;
  let keyFeedbackTimer: ReturnType<typeof setTimeout> | undefined;
  let keyReadTimer: ReturnType<typeof setTimeout> | undefined;
  // A queue owned by the parent survives replacement of this row.
  const sharedQueue = untrack(() => queue);
  const keyQueue: QueuedKeyCommand[] = sharedQueue ?? [];
  const armedModifiers = $derived(modifiers.filter((id) => armed[id]));
  const armedModifierLabel = $derived(armedModifiers.map(keyRowLabel).join('+'));
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
        const result = await relayStore.sendToAgent(command.target, {
          type: 'send_keys',
          keys: command.keys,
          activity_label: command.label,
        });
        command.resolve(true);
        const note = keyResultMessage?.(result);
        if (note) relayStore.showToast(note);
        showKeyFeedback(`${command.label} sent`);
        if (keyReadTimer) clearTimeout(keyReadTimer);
        keyReadTimer = setTimeout(() => {
          if (componentMounted && targetRefMatchesAgent(identity, agent)) relayStore.readPane(command.target);
        }, 300);
      } catch (error) {
        command.resolve(false);
        const message = error instanceof Error ? error.message : 'Terminal keys could not be sent.';
        showKeyFeedback('Key send failed');
        relayStore.showToast(message, true);
        for (const queued of keyQueue.splice(0)) queued.resolve(false);
      }
    }
    sending = false;
  }

  function showKeyFeedback(message: string) {
    if (!componentMounted) return;
    if (keyFeedbackTimer) clearTimeout(keyFeedbackTimer);
    keyFeedback = message;
    keyFeedbackTimer = setTimeout(() => {
      keyFeedback = '';
    }, 2_000);
  }

  function flashTappedKeys(node: HTMLElement) {
    const flash = (event: MouseEvent) => {
      const button = (event.target as Element).closest('button');
      if (!button || button.hasAttribute('aria-pressed') || button.hasAttribute('aria-expanded')) return;
      button.classList.remove('key-flash');
      void button.offsetWidth;
      button.classList.add('key-flash');
    };
    node.addEventListener('click', flash);
    return { destroy: () => node.removeEventListener('click', flash) };
  }

  function activate(id: string, modifier: typeof modifiers[number] | undefined, button: HTMLButtonElement) {
    if (modifier) {
      menu = '';
      armed[modifier] = !armed[modifier];
      if (armedModifierLabel) {
        modifierInputElement.value = '';
        modifierInputElement.focus();
      }
    } else if (id === 'arrows' || id === 'fkeys') {
      trigger = button;
      menu = menu === id ? '' : id;
      alignPopup();
    } else if (id.includes('+')) {
      void sendKeys([id], keyRowLabel(id));
    } else {
      sendTerminalKey(id);
    }
  }

  function keepFocus(event: MouseEvent) {
    event.preventDefault();
  }

  function modifierChord(key: string, displayName?: string): { chord: string; label: string } | null {
    if (!armedModifierLabel) return null;
    const name = displayName ?? key[0].toLocaleUpperCase() + key.slice(1).toLocaleLowerCase();
    return { chord: `${armedModifiers.join('+')}+${key.toLocaleLowerCase()}`, label: `${armedModifierLabel}+${name}` };
  }

  function disarmModifiers() {
    for (const id of modifiers) armed[id] = false;
  }

  function modifierInput(event: Event) {
    const target = event.currentTarget as HTMLInputElement;
    const character = Array.from(target.value)[0] || '';
    target.value = '';
    if (!character || /\s/u.test(character)) return;
    sendTerminalKey(character);
  }

  function sendTerminalKey(key: string, displayName?: string) {
    const result = modifierChord(key, displayName);
    const plainLabel = key === 'Escape' ? 'Cancelled prompt' : displayName ?? key;
    void sendKeys([result?.chord ?? key], result?.label ?? plainLabel);
  }

  function sendFunctionKey(number: number) {
    menu = '';
    sendTerminalKey(`f${number}`, `F${number}`);
  }

  function modifierKeydown(event: KeyboardEvent) {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    disarmModifiers();
  }

  function modifierBlur() {
    setTimeout(() => {
      if (document.activeElement !== modifierInputElement) disarmModifiers();
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
  {#if menu === 'arrows'}
    <div class="arrow-popup" role="group" aria-label="Navigation keys">
      {#each NAVIGATION_KEYS as cell (cell?.key)}
        {#if cell && (!cell.paging || pagingKeys)}
          <button
            class:paging-key={cell.paging}
            disabled={disabled}
            aria-label={cell.name ?? cell.key}
            onmousedown={keepFocus}
            onclick={() => sendTerminalKey(cell.key, cell.name)}
          >{cell.text ?? cell.key}</button>
        {:else}
          <span aria-hidden="true"></span>
        {/if}
      {/each}
    </div>
  {/if}
{/snippet}

{#snippet fkeyPopup()}
  {#if menu === 'fkeys'}
    <div class="fkey-popup" role="group" aria-label="Function keys">
      {#each FUNCTION_KEYS as number (number)}
        <button
          disabled={disabled}
          onmousedown={keepFocus}
          onclick={() => sendFunctionKey(number)}
        >F{number}</button>
      {/each}
    </div>
  {/if}
{/snippet}


<div use:flashTappedKeys>
  <p class="sr-only" role="status" aria-live="polite">{keyControlStatus}</p>
  <div class="term-keys" class:scrolling bind:this={row} aria-busy={keySending}>
    <div class="key-strip" use:observeStrip onscroll={alignPopup}>
      <div class="key-entries">
        {#each controls as id (id)}
          {#if !id}
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
          {:else}
            {@const modifier = modifiers.find((name) => name === id)}
            {@const isMenu = id === 'arrows' || id === 'fkeys'}
            {@const label = keyRowLabel(id)}
            {#if defaultOrder && id === 'fkeys'}
              <span class="spacer"></span>
              {#if inlineExtra}{@render extra?.()}{/if}
            {/if}
            <Button
              variant="secondary"
              size="sm"
              {disabled}
              aria-label={id === 'fkeys' ? 'Function keys' : label}
              aria-controls={modifier ? 'modifier-key-input' : undefined}
              aria-pressed={modifier ? armed[modifier] : undefined}
              aria-expanded={isMenu ? menu === id : undefined}
              title={modifier ? `Arm ${label}; combine it with other modifiers` : id === 'Tab' ? 'Send Tab' : undefined}
              onmousedown={keepFocus}
              onclick={(event) => activate(id, modifier, event.currentTarget)}
            >
              {#if id === 'Tab'}{@render tabIcon()}
              {:else if id === 'shift'}{@render shiftIcon()}
              {:else if id === 'ctrl'}<span class="key-caret">^</span>
              {:else if id === 'arrows'}{@render arrowIcon()}
              {:else}{label}{/if}
            </Button>
            {#if id === 'arrows'}{@render arrowPopup()}
            {:else if id === 'fkeys'}{@render fkeyPopup()}{/if}
          {/if}
        {/each}
      </div>
    </div>
    {#if !inlineExtra}{@render extra?.()}{/if}
  </div>
</div>
