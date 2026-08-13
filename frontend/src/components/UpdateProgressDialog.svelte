<script lang="ts">
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
    newerVersion,
    relayNeedsManualBootstrap,
    restoreUpdateProgress,
    setUpdateProgressError,
    updateProgressPlan,
    type UpdateProgressPlan,
  } from '$lib/updates';
  import type { RelayConfig, RelayConnectionView, RelayUpdateState } from '$lib/types';

  type ProgressTone = 'pending' | 'active' | 'success' | 'danger';

  interface RelayProgressRow {
    id: string;
    relay?: RelayConfig;
    connection?: RelayConnectionView;
    label: string;
    detail: string;
    tone: ProgressTone;
    score: number;
    canStart: boolean;
    manual: boolean;
  }

  interface AppProgress {
    label: string;
    detail: string;
    tone: ProgressTone;
    score: number;
  }

  const ACTIVE_STATES = new Set<RelayUpdateState>([
    'checking',
    'scheduled',
    'preparing',
    'deploying_app',
    'installing',
    'restarting',
  ]);
  const OPERATION_TIMEOUT_MS = 5 * 60 * 1_000;

  const plan = updateProgressPlan;
  const relays = relayStore.relayConfigs;
  const connections = relayStore.connections;
  const appUpdate = appUpdateStatus;

  let busyRelayId = $state('');
  let now = $state(Date.now());

  onMount(() => {
    restoreUpdateProgress();
    const timer = window.setInterval(() => { now = Date.now(); }, 1_000);
    return () => window.clearInterval(timer);
  });

  const rows = $derived.by(() => {
    if (!$plan) return [];
    return $plan.relayIds.map((relayId) => {
      const relay = $relays.find((candidate) => candidate.id === relayId);
      return relayProgress($plan, relayId, relay, $connections.get(relayId), now);
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
    if (!currentPlan || active || failed || complete || busyRelayId) return;
    const next = rows.find((row) => (
      row.tone === 'pending'
      && row.canStart
      && !currentPlan.startedRelayIds.includes(row.id)
    ));
    if (next) void startRelayUpdate(next);
  });
  async function copyManualCommand(): Promise<void> {
    if (!navigator.clipboard?.writeText) {
      relayStore.showToast('Clipboard access is unavailable. Select the text manually.', true);
      return;
    }
    try {
      await navigator.clipboard.writeText(MANAGED_UPDATE_COMMAND);
      relayStore.showToast('Update command copied.');
    } catch {
      relayStore.showToast('Could not copy. Select it manually.', true);
    }
  }


  function relayAtTarget(connection: RelayConnectionView | undefined, targetVersion: string): boolean {
    const current = connection?.releaseVersion || connection?.update.current_version || '';
    return current === targetVersion || newerVersion(current, targetVersion);
  }

  function relayProgress(
    currentPlan: UpdateProgressPlan,
    relayId: string,
    relay: RelayConfig | undefined,
    connection: RelayConnectionView | undefined,
    timestamp: number,
  ): RelayProgressRow {
    const base = { id: relayId, relay, connection };
    if (relayAtTarget(connection, currentPlan.targetVersion)) {
      return {
        ...base,
        label: `Updated to v${connection?.releaseVersion || currentPlan.targetVersion}`,
        detail: 'This relay is ready.',
        tone: 'success',
        score: 1,
        canStart: false,
        manual: false,
      };
    }
    const clientError = currentPlan.errors[relayId];
    if (clientError) {
      const manual = Boolean(connection && relayNeedsManualBootstrap(connection, clientError));
      return {
        ...base,
        label: manual ? 'Manual update required' : 'Update could not start',
        detail: manual
          ? 'Run the one-time Terminal command below to restart the relay and enable phone-driven updates.'
          : clientError,
        tone: 'danger',
        score: 0,
        canStart: !manual && Boolean(connection?.capabilities.includes('self_update')),
        manual,
      };
    }
    const started = currentPlan.startedRelayIds.includes(relayId);
    if (!connection || connection.status !== 'connected') {
      if (started && timestamp - (currentPlan.relayStartedAt[relayId] || currentPlan.startedAt) < OPERATION_TIMEOUT_MS) {
        return { ...base, label: 'Waiting for relay to reconnect…', detail: 'The connection normally returns after the service restarts.', tone: 'active', score: .85, canStart: false, manual: false };
      }
      return {
        ...base,
        label: started ? 'Relay did not reconnect' : 'Relay unavailable',
        detail: started ? 'Check the relay service on this computer, then close this screen.' : 'Connect this relay before updating it.',
        tone: started ? 'danger' : 'pending',
        score: 0,
        canStart: false,
        manual: false,
      };
    }
    if (relayNeedsManualBootstrap(connection)) {
      return {
        ...base,
        label: 'Manual update required',
        detail: 'Run the one-time Terminal command below to restart the relay and enable phone-driven updates.',
        tone: 'danger',
        score: 0,
        canStart: false,
        manual: true,
      };
    }
    const update = connection.update;
    if (update.state === 'failed') {
      return { ...base, label: 'Update failed', detail: update.error || 'The relay reported an update failure.', tone: 'danger', score: 0, canStart: true, manual: false };
    }
    if (update.state === 'rolled_back') {
      return { ...base, label: 'Previous version restored', detail: update.error || 'The replacement failed and the previous relay was restored.', tone: 'danger', score: 0, canStart: true, manual: false };
    }
    if (update.state === 'blocked') {
      return { ...base, label: 'Update blocked', detail: update.reason || 'This update needs attention before it can continue.', tone: 'danger', score: 0, canStart: false, manual: false };
    }
    if (ACTIVE_STATES.has(update.state)) {
      const status = activeRelayStatus(update.state);
      return { ...base, ...status, canStart: false, manual: false };
    }
    if (started && update.state === 'available' && update.available_version === currentPlan.targetVersion && update.can_install) {
      return { ...base, label: 'Starting update…', detail: 'Sending the verified target to this relay.', tone: 'active', score: .05, canStart: false, manual: false };
    }
    if (update.state === 'available' && update.available_version === currentPlan.targetVersion && update.can_install) {
      return { ...base, label: `Ready for v${currentPlan.targetVersion}`, detail: started ? 'Ready to retry from this phone.' : 'Start this relay when the current relay is ready.', tone: 'pending', score: 0, canStart: true, manual: false };
    }
    if (update.state === 'available') {
      return { ...base, label: 'Different update advertised', detail: update.reason || `This relay does not currently offer v${currentPlan.targetVersion}.`, tone: 'danger', score: 0, canStart: false, manual: false };
    }
    if (started) {
      return { ...base, label: 'Checking release…', detail: `Confirming that v${currentPlan.targetVersion} is available for this relay.`, tone: 'active', score: .05, canStart: false, manual: false };
    }
    return {
      ...base,
      label: `Waiting for v${currentPlan.targetVersion}`,
      detail: 'Herdr will verify and start this relay automatically when its turn begins.',
      tone: 'pending',
      score: 0,
      canStart: true,
      manual: false,
    };
  }

  function activeRelayStatus(state: RelayUpdateState): Pick<RelayProgressRow, 'label' | 'detail' | 'tone' | 'score'> {
    if (state === 'checking') return { label: 'Checking release…', detail: 'Looking for the exact verified target.', tone: 'active', score: .05 };
    if (state === 'scheduled') return { label: 'Update scheduled…', detail: 'The background update worker is starting.', tone: 'active', score: .1 };
    if (state === 'preparing') return { label: 'Verifying release…', detail: 'Downloading and checking the signed release bundle.', tone: 'active', score: .25 };
    if (state === 'deploying_app') return { label: 'Publishing phone app…', detail: 'Waiting for the app origin to serve the verified bundle.', tone: 'active', score: .45 };
    if (state === 'installing') return { label: 'Installing relay…', detail: 'Running agents and saved configuration remain intact.', tone: 'active', score: .65 };
    return { label: 'Restarting relay…', detail: 'The phone connection may briefly disconnect.', tone: 'active', score: .85 };
  }

  function appProgress(
    currentPlan: UpdateProgressPlan | null,
    currentConnections: Map<string, RelayConnectionView>,
    currentApp: typeof $appUpdate,
  ): AppProgress | null {
    if (!currentPlan?.appRelayId) return null;
    if (currentApp.currentVersion === currentPlan.targetVersion || newerVersion(currentApp.currentVersion, currentPlan.targetVersion)) {
      return { label: `Phone app v${currentApp.currentVersion} loaded`, detail: 'This screen resumed after the app update.', tone: 'success', score: 1 };
    }
    if (currentPlan.errors[currentPlan.appRelayId]) {
      return { label: 'Phone app deployment could not start', detail: currentPlan.errors[currentPlan.appRelayId], tone: 'danger', score: 0 };
    }
    const owner = currentConnections.get(currentPlan.appRelayId);
    if (owner?.appDeploy.state === 'failed') {
      return { label: 'Phone app deployment failed', detail: owner.appDeploy.error || 'The public app could not be verified.', tone: 'danger', score: 0 };
    }
    if (owner?.update.state === 'failed' && owner.appDeploy.state !== 'succeeded') {
      return { label: 'Phone app deployment failed', detail: owner.update.error || 'The update stopped before publishing the app.', tone: 'danger', score: 0 };
    }
    if (owner?.appDeploy.state === 'succeeded' || currentApp.deployedVersion === currentPlan.targetVersion) {
      return { label: 'Loading updated phone app…', detail: 'The verified bundle is public. This page will reload once.', tone: 'active', score: .85 };
    }
    return { label: 'Publishing phone app…', detail: 'Cloudflare Pages can take up to two minutes to converge.', tone: 'active', score: .4 };
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
    if (!$plan || !row.relay || busyRelayId) return;
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
  open={Boolean($plan)}
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
              <Button size="sm" disabled={Boolean(busyRelayId)} onclick={() => startRelayUpdate(row)}>Try Again</Button>
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
