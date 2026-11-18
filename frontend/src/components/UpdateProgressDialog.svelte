<script lang="ts">
  import { copyText } from '$lib/clipboard';
  import { onMount } from 'svelte';
  import { get } from 'svelte/store';
  import AppDialog from '$components/ui/AppDialog.svelte';
  import Button from '$components/ui/Button.svelte';
  import { relayStore } from '$lib/store';
  import {
    appUpdateStatus,
    clearUpdateProgress,
    MANAGED_UPDATE_COMMAND,
    markUpdateProgressRelayStarted,
    appProgress,
    relayAtTarget,
    relayProgress,
    type RelayProgressRow,
    restoreUpdateProgress,
    setUpdateProgressError,
    updateProgressRelayStartedThisSession,
    updateProgressPlan,
    type UpdateProgressPlan,
  } from '$lib/updates';

  let { readOnlyRelayIds = new Set<string>() }: { readOnlyRelayIds?: Set<string> } = $props();

  const plan = updateProgressPlan;
  const relays = relayStore.relayConfigs;
  const connections = relayStore.connections;
  const appUpdate = appUpdateStatus;

  let busyRelayId = $state('');
  let now = $state(Date.now());
  let assetUnavailable = $state(typeof document !== 'undefined' && (
    document.documentElement.dataset.herdrLoadFailed === '1'
      || document.documentElement.dataset.herdrLoadTimedOut === '1'
  ));

  onMount(() => {
    restoreUpdateProgress();
    const onRequiredAssetFailure = () => {
      if (document.documentElement.dataset.herdrLoadFailed === '1' || document.documentElement.dataset.herdrLoadTimedOut === '1') {
        assetUnavailable = true;
      }
    };
    window.addEventListener('herdr-required-assets', onRequiredAssetFailure);
    const timer = window.setInterval(() => { now = Date.now(); }, 1_000);
    return () => {
      window.clearInterval(timer);
      window.removeEventListener('herdr-required-assets', onRequiredAssetFailure);
    };
  });

  const rows = $derived.by(() => {
    if (!$plan) return [];
    return $plan.relayIds.map((relayId) => {
      const relay = $relays.find((candidate) => candidate.id === relayId);
      const row = relayProgress($plan, relayId, relay, $connections.get(relayId), now);
      return readOnlyRelayIds.has(relayId)
        ? { ...row, canStart: false, detail: 'This paired device has read-only access to this relay.' }
        : row;
    });
  });
  const phoneApp = $derived(appProgress($plan, $connections, $appUpdate));
  const completedItems = $derived(rows.filter((row) => row.tone === 'success').length + (phoneApp?.tone === 'success' ? 1 : 0));
  const totalItems = $derived(rows.length + (phoneApp ? 1 : 0));
  const overallProgress = $derived.by(() => {
    if (!totalItems) return 0;
    const score = rows.reduce((total, row) => total + row.score, 0) + (phoneApp?.score || 0);
    return Math.round((score / totalItems) * 100);
  });
  const active = $derived(Boolean(
    busyRelayId
      || rows.some((row) => row.tone === 'active')
      || phoneApp?.tone === 'active',
  ));
  const failed = $derived(Boolean(
    rows.some((row) => row.tone === 'danger')
      || phoneApp?.tone === 'danger',
  ));
  const relayActive = $derived(Boolean(
    busyRelayId || rows.some((row) => row.tone === 'active'),
  ));
  const relayFailed = $derived(rows.some((row) => row.tone === 'danger'));
  const failureDetail = $derived(
    phoneApp?.tone === 'danger'
      ? phoneApp.detail
      : rows.find((row) => row.tone === 'danger')?.detail || '',
  );
  const complete = $derived(Boolean(totalItems && completedItems === totalItems));
  const focusRow = $derived(rows.find((row) => row.tone === 'active' && row.connection) || null);
  const focusSteps = $derived(focusRow && $plan ? relaySteps(focusRow, $plan) : []);
  const focusStepIndex = $derived(focusRow && $plan ? relayStepIndex(focusRow, $plan) : -1);
  const title = $derived(failed
    ? 'Update needs attention'
    : complete
      ? 'Update complete'
      : active
        ? 'Updating Herdr'
        : 'Continue update');

  $effect(() => {
    const currentPlan = $plan;
    if (!currentPlan || relayActive || relayFailed || complete) return;
    const next = rows.find((row) => (
      row.tone === 'pending'
      && row.canStart
      && (!currentPlan.startedRelayIds.includes(row.id) || !updateProgressRelayStartedThisSession(row.id))
    ));
    if (next) void startRelayUpdate(next);
  });
  async function copyManualCommand(): Promise<void> {
    if (!navigator.clipboard?.writeText) {
      relayStore.showToast('Clipboard access is unavailable. Select the text manually.', true);
      return;
    }
    if (await copyText(MANAGED_UPDATE_COMMAND)) {
      relayStore.showToast('Update command copied.');
    } else {
      relayStore.showToast('Could not copy. Select it manually.', true);
    }
  }


  function relaySteps(row: RelayProgressRow, currentPlan: UpdateProgressPlan): string[] {
    const steps = ['Verify release'];
    if (currentPlan.appRelayId === row.id) steps.push('Publish phone app');
    steps.push('Install relay', 'Reconnect');
    return steps;
  }

  function relayStepIndex(row: RelayProgressRow, currentPlan: UpdateProgressPlan): number {
    const state = row.connection?.update.state;
    const hasAppStep = currentPlan.appRelayId === row.id;
    if (state === 'deploying_app') return hasAppStep ? 1 : 0;
    if (state === 'installing') return hasAppStep ? 2 : 1;
    if (state === 'restarting' || row.connection?.status !== 'connected') return hasAppStep ? 3 : 2;
    return 0;
  }

  async function startRelayUpdate(row: RelayProgressRow): Promise<void> {
    if (!$plan || !row.relay || busyRelayId || readOnlyRelayIds.has(row.id)) return;
    const relayId = row.id;
    busyRelayId = relayId;
    markUpdateProgressRelayStarted(relayId);
    try {
      let connection = get(relayStore.connections).get(relayId);
      if (!connection || !connection.capabilities.includes('self_update')) {
        throw new Error('This relay requires a manual update');
      }
      if (connection.update.state !== 'available' || connection.update.available_version !== $plan.targetVersion) {
        await relayStore.checkRelayUpdate(relayId);
        connection = get(relayStore.connections).get(relayId);
      }
      if (relayAtTarget(connection, $plan.targetVersion)) return;
      if (connection?.update.state !== 'available' || !connection.update.can_install || connection.update.available_version !== $plan.targetVersion) {
        throw new Error(connection?.update.reason || `Relay does not currently offer v${$plan.targetVersion}`);
      }
      await relayStore.installRelayUpdate(relayId);
    } catch (error) {
      setUpdateProgressError(relayId, error);
    } finally {
      busyRelayId = '';
    }
  }

  function closeProgress(): void {
    clearUpdateProgress();
  }
</script>

<AppDialog
  id="update-progress-dialog"
  open={Boolean($plan) && !assetUnavailable}
  {title}
  description={$plan
    ? `${completedItems} of ${totalItems} update items complete for v${$plan.targetVersion}.`
    : ''}
  dismissible={false}
>
  {#if $plan}
    <section class="update-progress" aria-live="polite">
      <div class="update-progress-summary">
        <span>{overallProgress}%</span>
        <progress max="100" value={overallProgress} aria-label={`Overall update progress: ${overallProgress}%`}></progress>
      </div>
      <p class="hint">Herdr follows the safe order automatically and updates relays one at a time. A Terminal command is needed only when a relay says Manual update required.</p>

      {#if phoneApp}
        <article class={`update-progress-item update-progress-${phoneApp.tone}`}>
          <span class="update-progress-marker" aria-hidden="true"></span>
          <div>
            <strong>Phone app</strong>
            <span>{phoneApp.label}</span>
            <small>{phoneApp.detail}</small>
          </div>
        </article>
      {/if}

      {#if focusRow && focusSteps.length}
        <ol class="update-step-list" aria-label={`${focusRow.relay?.label || 'Relay'} update steps`}>
          {#each focusSteps as step, index (step)}
            <li class:complete={index < focusStepIndex} class:active={index === focusStepIndex}>
              <span aria-hidden="true"></span>{step}
            </li>
          {/each}
        </ol>
      {/if}

      <div class="update-progress-list">
        {#each rows as row (row.id)}
          <article class={`update-progress-item update-progress-${row.tone}`}>
            <span class="update-progress-marker" aria-hidden="true"></span>
            <div>
              <strong>{row.relay?.label || 'Unknown relay'}</strong>
              <span>{row.label}</span>
              <small>{row.detail}</small>
              {#if row.manual}
                <pre class="update-command"><code>{MANAGED_UPDATE_COMMAND}</code></pre>
                <Button size="sm" onclick={copyManualCommand}>Copy Update Command</Button>
              {/if}
            </div>
            {#if row.canStart && !active && row.tone === 'danger'}
              <Button size="sm" disabled={Boolean(busyRelayId) || readOnlyRelayIds.has(row.id)} onclick={() => startRelayUpdate(row)}>Try Again</Button>
            {/if}
          </article>
        {/each}
      </div>

      {#if failed}
        <div class="update-progress-error" role="alert">
          <strong>The update stopped.</strong>
          {#if failureDetail}<p>{failureDetail}</p>{/if}
          <p>Failures before cutover leave the current relay running. If replacement fails during cutover, the installer restores and verifies the previous service.</p>
        </div>
      {:else if complete}
        <p class="update-progress-success" role="status">The phone app and all listed relays are ready.</p>
      {/if}
      {#if !active || failed}
        <div class="dialog-actions update-progress-actions">
          <Button onclick={closeProgress}>{complete || failed ? 'Close' : 'Finish Later'}</Button>
        </div>
      {/if}
    </section>
  {/if}
</AppDialog>
