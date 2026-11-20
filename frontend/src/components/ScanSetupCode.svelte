<script module lang="ts">
  import type SetupScanner from './SetupScanner.svelte';
  import { APP_ASSET_VERSION } from '$lib/config';

  let loadFailures = 0;

  function loadScanner(): Promise<{ default: typeof SetupScanner }> {
    if (!loadFailures) return import('./SetupScanner.svelte');
    // A browser can keep a failed module import cached until the page is reloaded.
    const chunk = import.meta.env.DEV ? './SetupScanner.svelte' : `./SetupScanner-${APP_ASSET_VERSION}.js`;
    return import(/* @vite-ignore */ `${chunk}?retry=${loadFailures}`);
  }
</script>

<script lang="ts">
  import { onDestroy, tick } from 'svelte';
  import { isIosBrowserTab } from '$lib/config';
  import { securityState } from '$lib/security';
  import { relayStore } from '$lib/store';
  import { AppReloadHeld, importOrReload } from '$lib/updates';

  let Scanner = $state<typeof SetupScanner>();
  let request = $state.raw<{ trigger: HTMLButtonElement }>();

  function cancel() { request = undefined; }
  onDestroy(cancel);
  $effect(() => { if ($securityState.locked) cancel(); });

  function visibilityChanged() {
    if (document.visibilityState === 'hidden') cancel();
  }

  function setOpen(value: boolean) {
    if (value || !request) return;
    const { trigger } = request;
    cancel();
    void tick().then(() => trigger.focus());
  }

  async function scan(event: MouseEvent & { currentTarget: HTMLButtonElement }) {
    if (request || $securityState.locked || document.visibilityState === 'hidden') return;
    const attempt = request = { trigger: event.currentTarget };
    const wanted = () => request === attempt;
    try {
      Scanner ??= (await importOrReload(loadScanner, wanted)).default;
    } catch (error) {
      loadFailures += 1;
      if (!wanted()) return;
      cancel();
      relayStore.showToast(error instanceof AppReloadHeld ? error.message : 'The scanner could not be loaded. Check the connection and try again.', true);
    }
  }
</script>

<svelte:document onvisibilitychange={visibilityChanged} />
<svelte:window onpagehide={cancel} />

{#if !isIosBrowserTab(navigator.standalone, navigator.userAgent, navigator.maxTouchPoints)}
  <button type="button" class="button button-secondary" onclick={scan}>Scan QR Code</button>
  {#if request && Scanner}<Scanner bind:open={() => Boolean(request), setOpen} />{/if}
{/if}
