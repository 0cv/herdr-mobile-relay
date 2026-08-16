<script lang="ts">
  import { onMount } from 'svelte';
  import AppDialog from '$components/ui/AppDialog.svelte';
  import AppSwitch from '$components/ui/AppSwitch.svelte';
  import Button from '$components/ui/Button.svelte';
  import Card from '$components/ui/Card.svelte';
  import {
    APP_VERSION,
    HOME_LAYOUTS,
    HOME_LAYOUT_LABELS,
    INTERFACE_SIZES,
    TERMINAL_HISTORY_OPTIONS,
    TERMINAL_REFRESH_LABELS,
    TERMINAL_REFRESH_OPTIONS,
    THEMES,
    type HomeLayout,
    type InterfaceSize,
    type TerminalHistoryLines,
    type TerminalRefreshInterval,
    type Theme,
  } from '$lib/config';
  import {
    homeLayout,
    interfaceSize,
    setHomeLayout,
    setInterfaceSize,
    setTerminalHistoryLines,
    setTerminalRefreshInterval,
    setTheme,
    terminalHistoryLines,
    terminalRefreshInterval,
    theme,
  } from '$lib/preferences';
  import { relayVersionMeta } from '$lib/protocol';
  import {
    finishedNotificationsEnabled,
    notificationsSupported,
    pushPreferences,
    pushSupported,
    refreshPushPreferences,
    removeRelayPushSubscription,
    setFinishedNotifications,
    toggleNotifications,
  } from '$lib/push';
  import {
    deviceVerificationEnabled,
    deviceVerificationSupported,
    securityState,
    setDeviceVerificationRequired,
  } from '$lib/security';
  import { relayStore } from '$lib/store';
  import {
    appUpdateStatus,
    CHECKOUT_UPDATE_COMMAND,
    beginUpdateProgress,
    checkAppUpdate,
    MANAGED_UPDATE_COMMAND,
    relayNeedsManualBootstrap,
    queueUpdateProgressForReload,
    reloadApp,
    setUpdateProgressError,
  } from '$lib/updates';
  import type { AppUpdateStatus, RelayConnectionView } from '$lib/types';

  const APP_DEPLOY_SETUP_COMMAND = 'herdr plugin action invoke configure-app-deploy --plugin herdr-mobile-relay.events';

  type SafeUpdateAction =
    | {
      kind: 'deploy_app' | 'install_relay';
      relayId: string;
      targetVersion: string;
      appRelayId: string;
      description: string;
    }
    | {
      kind: 'reload_app';
      targetVersion: string;
      description: string;
    };

  const relays = relayStore.relayConfigs;
  const connections = relayStore.connections;
  const agents = relayStore.agents;
  const notificationBusy = relayStore.notificationBusy;
  const appUpdate = appUpdateStatus;
  let previousAppUpdate = $state<AppUpdateStatus | null>(null);
  let checkingUpdates = $state(false);
  const appUpdateChecking = $derived(checkingUpdates || $appUpdate.state === 'checking');
  const appUpdateForLayout = $derived(
    appUpdateChecking && previousAppUpdate ? previousAppUpdate : $appUpdate,
  );

  $effect(() => {
    if (!appUpdateChecking) previousAppUpdate = $appUpdate;
  });

  onMount(refreshPushPreferences);

  let relayLabel = $state('');
  let relayUrl = $state('');
  let relayToken = $state('');
  let finished = $state(finishedNotificationsEnabled());
  let deviceLock = $state(deviceVerificationEnabled());
  let updateOpen = $state(false);
  let pendingUpdateAction = $state<SafeUpdateAction | null>(null);
  let manualRelayId = $state('');
  let manualOpen = $state(false);
  let removalRelayId = $state('');
  let removalOpen = $state(false);
  let busyRelayId = $state('');

  const relayRows = $derived($relays.map((relay) => ({
    relay,
    connection: $connections.get(relay.id),
  })));
  const connectedCount = $derived([...$connections.values()].filter((connection) => connection.status === 'connected').length);
  const degradedCount = $derived([...$connections.values()].filter(
    (connection) => connection.status === 'connected' && connection.inventory.state !== 'ready',
  ).length);
  const manualRow = $derived(relayRows.find(({ relay }) => relay.id === manualRelayId));
  const removalRow = $derived(relayRows.find(({ relay }) => relay.id === removalRelayId));
  const appDeploymentOwner = $derived(relayRows.find(({ connection }) => (
    connection?.status === 'connected'
    && connection.capabilities.includes('app_deploy')
    && connection.appDeploy.configured
    && connection.appDeploy.origin === location.origin
  )));
  // The owner relay is behind the released app version but can self-update to
  // exactly that version, so one action can deploy the app before updating it.
  const ownerUpdateReady = $derived.by(() => {
    const connection = appDeploymentOwner?.connection;
    if (!connection
      || connection.releaseVersion === $appUpdate.upstreamVersion
      || relayNeedsManualBootstrap(connection)) return false;
    const update = connection.update;
    return connection.capabilities.includes('self_update')
      && update.state === 'available'
      && update.can_install
      && Boolean(update.target_revision)
      && update.available_version === $appUpdate.upstreamVersion;
  });
  const safeUpdateAction = $derived.by((): SafeUpdateAction | null => {
    if (appUpdateChecking || $appUpdate.state === 'failed') return null;
    const targetVersion = $appUpdate.upstreamVersion;
    if ($appUpdate.state === 'reload-ready') {
      return {
        kind: 'reload_app',
        targetVersion: $appUpdate.deployedVersion,
        description: `Load the verified phone app v${$appUpdate.deployedVersion}.`,
      };
    }
    if ($appUpdate.state === 'deployment-required') {
      const owner = appDeploymentOwner;
      if (!owner?.connection || !targetVersion) return null;
      if (owner.connection.releaseVersion === targetVersion
        && ['scheduled', 'deploying'].includes(owner.connection.appDeploy.state)) return null;
      if (owner.connection.releaseVersion === targetVersion) {
        return {
          kind: 'deploy_app',
          relayId: owner.relay.id,
          targetVersion,
          appRelayId: owner.relay.id,
          description: `Publish the phone app from ${owner.relay.label}, then continue with any remaining relay updates.`,
        };
      }
      if (!ownerUpdateReady) return null;
      return {
        kind: 'install_relay',
        relayId: owner.relay.id,
        targetVersion,
        appRelayId: owner.relay.id,
        description: `Publish the phone app first, then update ${owner.relay.label} and continue with the remaining relays.`,
      };
    }
    const installable = relayRows.filter(({ connection }) => (
      connection?.status === 'connected'
      && connection.capabilities.includes('self_update')
      && !relayNeedsManualBootstrap(connection)
      && connection.update.state === 'available'
      && connection.update.can_install
      && Boolean(connection.update.target_revision)
    ));
    const selected = installable.find(({ relay }) => relay.id === appDeploymentOwner?.relay.id) || installable[0];
    if (!selected?.connection) return null;
    return {
      kind: 'install_relay',
      relayId: selected.relay.id,
      targetVersion: selected.connection.update.available_version,
      appRelayId: '',
      description: `Update ${selected.relay.label} first, then continue safely with each remaining relay.`,
    };
  });
  const relayUpdateCount = $derived(relayRows.filter(
    ({ connection }) => connection?.update.state === 'available',
  ).length);
  const blockedRelayUpdateCount = $derived(relayRows.filter(
    ({ connection }) => connection?.update.state === 'blocked',
  ).length);
  const manualRelayUpdateCount = $derived(relayRows.filter(
    ({ connection }) => connection?.status === 'connected'
      && relayNeedsManualBootstrap(connection),
  ).length);
  const updatePending = $derived(
    $appUpdate.state === 'deployment-required'
      || $appUpdate.state === 'reload-ready'
      || relayRows.some(({ connection }) => connection?.update.state === 'available'),
  );
  const notification = $derived.by(() => notificationMeta(
    [...$connections.values()],
    $notificationBusy,
    $pushPreferences,
  ));

  function updateActionLabel(action: SafeUpdateAction | null): string {
    if (action?.kind === 'reload_app') return 'Load Update';
    if (action?.kind === 'install_relay' && !action.appRelayId) return 'Update Relays';
    if (!action && $appUpdate.state !== 'deployment-required') return 'Update Relays';
    return 'Update Herdr';
  }

  function addRelay(event: SubmitEvent) {
    event.preventDefault();
    relayStore.addRelay({ label: relayLabel, url: relayUrl, token: relayToken });
    relayLabel = '';
    relayUrl = '';
    relayToken = '';
  }

  function requestRelayRemoval(id: string) {
    removalRelayId = id;
    removalOpen = true;
  }

  async function confirmRelayRemoval() {
    if (!removalRelayId) return;
    const relayId = removalRelayId;
    removalOpen = false;
    await removeRelayPushSubscription(relayId);
    relayStore.removeRelay(relayId);
    removalRelayId = '';
  }

  async function changeFinished(value: boolean) {
    finished = value;
    await setFinishedNotifications(value);
  }

  async function changeDeviceLock(value: boolean) {
    const changed = await setDeviceVerificationRequired(value);
    deviceLock = value && changed;
  }

  function relayUpdateMeta(connection?: RelayConnectionView) {
    if (!connection || connection.status !== 'connected') {
      return {
        label: 'Update status unavailable',
        detail: 'Connect this relay to check its version.',
        warning: false,
      };
    }
    if (relayNeedsManualBootstrap(connection)) {
      return {
        label: 'Manual update required',
        detail: 'Open Update Help for the one-time Terminal bootstrap.',
        warning: true,
      };
    }
    const update = connection.update;
    if (update.state === 'checking') return { label: 'Checking for updates…', detail: '', warning: false };
    if (update.state === 'available') {
      return {
        label: `Update v${update.available_version} available`,
        detail: `Revision ${update.available_revision}`,
        warning: true,
      };
    }
    if (update.state === 'blocked') {
      return {
        label: `Update v${update.available_version} needs attention`,
        detail: update.reason,
        warning: true,
      };
    }
    if (update.state === 'scheduled') {
      return { label: 'Update scheduled…', detail: 'Preparing the verified release.', warning: true };
    }
    if (update.state === 'preparing') {
      return { label: 'Verifying update…', detail: 'Checking release identity and transport compatibility.', warning: true };
    }
    if (update.state === 'deploying_app') {
      return { label: 'Publishing phone app…', detail: 'Waiting for the app origin to serve the verified bundle.', warning: true };
    }
    if (update.state === 'installing') {
      return { label: 'Installing update…', detail: 'The phone connection may briefly disconnect.', warning: true };
    }
    if (update.state === 'restarting') {
      return { label: 'Restarting relay…', detail: 'The phone connection may briefly disconnect.', warning: true };
    }
    if (update.state === 'succeeded') {
      return { label: 'Update installed', detail: `Running v${update.current_version}`, warning: false };
    }
    if (update.state === 'rolled_back') {
      return { label: 'Update rolled back', detail: update.error, warning: true };
    }
    if (update.state === 'failed') {
      return { label: 'Update operation failed', detail: update.error, warning: true };
    }
    const checked = update.checked_at
      ? `Checked ${new Date(update.checked_at * 1_000).toLocaleString()}`
      : 'Update check pending';
    return { label: 'Up to date', detail: checked, warning: false };
  }

  async function checkRelayUpdate(relayId: string) {
    busyRelayId = relayId;
    try {
      await relayStore.checkRelayUpdate(relayId);
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
    } finally {
      busyRelayId = '';
    }
  }

  async function checkAppAndRelays() {
    if (checkingUpdates) return;
    checkingUpdates = true;
    try {
      const checks: Promise<unknown>[] = [checkAppUpdate()];
      for (const { relay, connection } of relayRows) {
        if (connection?.status === 'connected' && connection.capabilities.includes('self_update')) {
          checks.push(relayStore.checkRelayUpdate(relay.id));
        }
      }
      const results = await Promise.allSettled(checks);
      const failure = results.find((result) => result.status === 'rejected');
      if (failure?.status === 'rejected') {
        relayStore.showToast((failure.reason as Error).message, true);
      }
    } finally {
      checkingUpdates = false;
    }
  }

  function requestSafeUpdate() {
    if (!safeUpdateAction) return;
    pendingUpdateAction = safeUpdateAction;
    updateOpen = true;
  }

  function showManualUpdate(relayId: string) {
    manualRelayId = relayId;
    manualOpen = true;
  }

  async function copyUpdateCommand(command: string, installation: string) {
    if (!navigator.clipboard?.writeText) {
      relayStore.showToast('Clipboard access is unavailable. Select the text manually.', true);
      return;
    }
    try {
      await navigator.clipboard.writeText(command);
      relayStore.showToast(`${installation} update command copied.`);
    } catch {
      relayStore.showToast('Could not copy. Select it manually.', true);
    }
  }

  async function startSafeUpdate() {
    const action = pendingUpdateAction;
    pendingUpdateAction = null;
    updateOpen = false;
    if (!action) return;
    if (action.kind === 'reload_app') {
      const relayIds = relayRows.map(({ relay }) => relay.id);
      if (relayIds.length) queueUpdateProgressForReload(action.targetVersion, relayIds);
      reloadApp(action.targetVersion);
      return;
    }
    const relayIds = [
      action.relayId,
      ...relayRows.map(({ relay }) => relay.id).filter((relayId) => relayId !== action.relayId),
    ];
    beginUpdateProgress(action.targetVersion, relayIds, action.relayId, action.appRelayId);
    busyRelayId = action.relayId;
    try {
      if (action.kind === 'deploy_app') {
        await relayStore.deployAppUpdate(action.relayId, action.targetVersion);
        relayStore.showToast('Publishing the phone app. This screen will resume after it reloads.');
      } else {
        await relayStore.installRelayUpdate(action.relayId);
        relayStore.showToast(action.appRelayId
          ? 'Publishing the phone app before safely updating its relay.'
          : 'Update scheduled. Remaining relays will follow.');
      }
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
      setUpdateProgressError(action.relayId, error);
    } finally {
      busyRelayId = '';
    }
  }

  function notificationMeta(all: RelayConnectionView[], busy: boolean, preferences: { notificationsEnabled: boolean; optedIn: boolean }) {
    if (!notificationsSupported()) return { label: 'Notifications Unavailable', hint: 'This browser does not support page notifications.', disabled: true };
    if (Notification.permission === 'denied') return { label: 'Notifications Blocked', hint: 'Enable notifications in this browser site settings.', disabled: true };
    if (!preferences.notificationsEnabled) return { label: 'Enable Notifications', hint: pushSupported() ? 'Required before closed-app push notifications can work.' : 'Required before background tabs can notify.', disabled: false };
    if (!pushSupported()) return { label: 'Notifications Enabled', hint: 'Background tabs can notify while this browser keeps the page alive.', disabled: true };
    const connected = all.filter((connection) => connection.status === 'connected');
    const synced = connected.filter((connection) => connection.pushStatus === 'subscribed').length;
    const syncing = connected.some((connection) => ['syncing', 'sent'].includes(connection.pushStatus));
    if (busy || syncing) return { label: 'Syncing Push…', hint: 'Updating this browser subscription on connected relays.', disabled: true };
    if (!connected.length) return { label: 'Sync Push Subscription', hint: 'Connect a relay before syncing push notifications.', disabled: true };
    if (!preferences.optedIn) return { label: 'Enable Push Notifications', hint: 'Push is stopped for this browser; site permission remains allowed.', disabled: false };
    if (synced === connected.length) return { label: 'Stop Push Notifications', hint: `Push subscription synced with ${synced} relay${synced === 1 ? '' : 's'}.`, disabled: false };
    if (connected.some((connection) => connection.pushStatus === 'key-mismatch')) return { label: 'Sync Push Subscription', hint: 'A relay changed its push key. Sync again to refresh this device.', disabled: false };
    if (connected.some((connection) => connection.pushStatus === 'failed')) return { label: 'Sync Push Subscription', hint: 'Push subscription sync failed. Reconnect and try again.', disabled: false };
    return { label: 'Sync Push Subscription', hint: synced ? `Push synced with ${synced}/${connected.length} connected relays.` : 'Push can wake this app when an agent blocks.', disabled: false };
  }

  function pushStatusLabel(connection?: RelayConnectionView): string {
    if (!connection) return 'not connected';
    if (!pushSupported()) return 'unavailable';
    if (connection.pushStatus === 'subscribed') return 'synced';
    if (['syncing', 'sent'].includes(connection.pushStatus)) return 'syncing…';
    if (connection.pushStatus === 'browser-subscribed') return 'browser subscription found';
    if (connection.pushStatus === 'missing-config') return 'relay push unavailable';
    if (connection.pushStatus === 'key-mismatch') return 'key changed';
    if (connection.pushStatus === 'failed') return 'sync failed';
    if (connection.status === 'connecting') return 'waiting for relay…';
    if (connection.status === 'connected' && $pushPreferences.optedIn) return 'checking…';
    return 'not synced';
  }
</script>

<main class="page settings-page" aria-labelledby="settings-title">
  <h2 id="settings-title">Settings</h2>

  <Card>
    <h3>Relays</h3>
    <form class="form-stack" onsubmit={addRelay}>
      <label for="relay-label">Relay Name</label>
      <input id="relay-label" bind:value={relayLabel} placeholder="Fedora" />
      <label for="relay-url">Relay URL</label>
      <input id="relay-url" bind:value={relayUrl} type="url" required placeholder="wss://relay-fedora.example.com" />
      <label for="relay-token">Relay key</label>
      <input id="relay-token" bind:value={relayToken} type="password" placeholder="HERDR_RELAY_TOKEN" />
      <div class="form-actions">
        <Button type="submit">Add Relay</Button>
        <Button variant="secondary" onclick={() => relayStore.connectAll()}>Reconnect All</Button>
      </div>
    </form>
    <div class="relay-list">
      {#if !$relays.length}<p class="hint">No relays configured.</p>{/if}
      {#each relayRows as { relay, connection } (relay.id)}
        {@const connectionStatus = connection?.status || 'disconnected'}
        {@const version = relayVersionMeta(connection)}
        {@const update = relayUpdateMeta(connection)}
        {@const manualUpdate = Boolean(connection && relayNeedsManualBootstrap(connection))}
        <article class="relay-row">
          <span
            class={`status-dot status-${connectionStatus === 'connected' && connection?.inventory.state === 'ready' ? 'success' : connectionStatus === 'connecting' || connectionStatus === 'connected' ? 'warning' : 'danger'}`}
            role="img"
            aria-label={`${relay.label} relay ${connectionStatus}`}
          ></span>
          <div class="relay-info">
            <strong>{relay.label}</strong>
            <span>{relay.url}</span>
            <small>Push: {pushStatusLabel(connection)}</small>
            {#if connectionStatus === 'connected' && connection?.inventory.state !== 'ready'}
              <small class="warning" role="status">
                {connection?.inventory.state === 'error'
                  ? connection.inventory.message || 'Herdr agent inventory unavailable.'
                  : 'Loading Herdr agent inventory…'}
              </small>
            {/if}
            {#if version}<small class:warning={version.tone === 'warning'} title={version.title}>{version.label}</small>{/if}
            <small class:warning={update.warning} role="status">{update.label}</small>
            {#if update.detail}<small class:warning={update.warning} title={update.detail}>{update.detail}</small>{/if}
          </div>
          <div class="relay-actions">
            {#if connectionStatus === 'connected' && manualUpdate}
              <Button
                variant="secondary"
                size="sm"
                aria-label={`How to update ${relay.label}`}
                onclick={() => showManualUpdate(relay.id)}
              >Update Help</Button>
            {:else if connection?.capabilities.includes('self_update')}
              <Button
                variant="secondary"
                size="sm"
                disabled={connectionStatus !== 'connected' || busyRelayId === relay.id || ['scheduled', 'preparing', 'deploying_app', 'installing', 'restarting'].includes(connection.update.state)}
                aria-label={`Check ${relay.label} for updates`}
                onclick={() => checkRelayUpdate(relay.id)}
              >Check</Button>
            {/if}
            <Button variant="danger" size="sm" aria-label={`Remove ${relay.label}`} onclick={() => requestRelayRemoval(relay.id)}>Remove</Button>
          </div>
        </article>
      {/each}
    </div>
    <p class="hint">Use one relay URL per computer. Relay keys stay in this browser’s local storage and encrypt relay messages end to end.</p>
  </Card>

  <Card>
    <h3>Appearance</h3>
    <fieldset class="choice-grid">
      <legend>Theme</legend>
      {#each THEMES as item (item)}
        <button class:active={$theme === item} type="button" aria-pressed={$theme === item} onclick={() => setTheme(item as Theme)}>{item}</button>
      {/each}
    </fieldset>
    <fieldset class="choice-grid compact-grid">
      <legend>Interface Size</legend>
      {#each INTERFACE_SIZES as item (item)}
        <button class:active={$interfaceSize === item} type="button" aria-pressed={$interfaceSize === item} onclick={() => setInterfaceSize(item as InterfaceSize)}>{item.charAt(0).toUpperCase() + item.slice(1)}</button>
      {/each}
    </fieldset>
    <fieldset class="choice-grid compact-grid">
      <legend>Home Workspaces</legend>
      {#each HOME_LAYOUTS as item (item)}
        <button
          class:active={$homeLayout === item}
          type="button"
          aria-pressed={$homeLayout === item}
          onclick={() => setHomeLayout(item as HomeLayout)}
        >{HOME_LAYOUT_LABELS[item]}</button>
      {/each}
    </fieldset>
    <p class="hint">By State separates Done, Working, and Idle workspace sections. Mixed shows each workspace once with a dot for its most notable session: done, then working, then idle. Agents needing input always stay on top.</p>
    <fieldset class="choice-grid history-grid">
      <legend>Terminal History</legend>
      {#each TERMINAL_HISTORY_OPTIONS as item (item)}
        <button
          class:active={$terminalHistoryLines === item}
          type="button"
          aria-pressed={$terminalHistoryLines === item}
          onclick={() => setTerminalHistoryLines(item as TerminalHistoryLines)}
        >{item}</button>
      {/each}
    </fieldset>
    <p class="hint">Lines kept in the terminal view. History shows the newest rows Herdr serves per read (up to 1,000), rendered at the current width. Use Copy or Conversation History for clean response text.</p>
    <fieldset class="choice-grid history-grid refresh-grid">
      <legend>Terminal Refresh</legend>
      {#each TERMINAL_REFRESH_OPTIONS as item (item)}
        <button
          class:active={$terminalRefreshInterval === item}
          type="button"
          aria-pressed={$terminalRefreshInterval === item}
          onclick={() => setTerminalRefreshInterval(item as TerminalRefreshInterval)}
        >{TERMINAL_REFRESH_LABELS[item]}</button>
      {/each}
    </fieldset>
    <p class="hint">How often the relay checks the visible pane. 250 ms is balanced; faster refresh uses more computer and phone CPU during active output.</p>
    <p class="hint">Resize Session automatically leases the shared terminal at the phone width while it is open, so the laptop view changes too. The previous width is restored when the terminal closes or disconnects.</p>
  </Card>

  <Card>
    <h3>Notifications</h3>
    <Button disabled={notification.disabled} onclick={() => toggleNotifications()}>{notification.label}</Button>
    <AppSwitch
      checked={finished}
      disabled={!pushSupported() || !$pushPreferences.optedIn || !connectedCount || $notificationBusy}
      label="Notify When Agents Finish"
      descriptionId="finished-notification-hint"
      onchange={changeFinished}
    />
    <p class="hint" id="finished-notification-hint">Optional. Blocked-agent notifications remain enabled whenever push is active.</p>
    <p class="hint" role="status">{notification.hint}</p>
  </Card>

  <Card>
    <h3>Security</h3>
    <AppSwitch checked={deviceLock} disabled={$securityState.busy} label="Require Fingerprint / Device Unlock" onchange={changeDeviceLock} />
    <p class="hint">{deviceVerificationSupported() ? $securityState.hint : 'Device verification needs HTTPS and WebAuthn support.'}</p>
  </Card>

  <Card>
    <h3>Status</h3>
    <p><span class={`status-dot status-${degradedCount ? 'warning' : connectedCount ? 'success' : 'danger'}`}></span> {connectedCount}/{$relays.length} relays connected · {$agents.length} agents</p>
    {#if degradedCount}<p class="warning" role="status">{degradedCount} connected {degradedCount === 1 ? 'relay has' : 'relays have'} unavailable agent inventory.</p>{/if}
  </Card>

  <Card>
    <h3>About</h3>
    <p>Phone app version {APP_VERSION}</p>
    <div class="app-update-status" aria-busy={appUpdateChecking}>
      <div class:app-update-status-hidden={appUpdateChecking} aria-hidden={appUpdateChecking}>
        {#if appUpdateForLayout.state === 'reload-ready'}
          <p class="warning" role="status">Version {appUpdateForLayout.deployedVersion} is deployed to this app origin and ready to load.</p>
        {:else if appUpdateForLayout.state === 'deployment-required'}
          <p class="warning" role="status">
            Version {appUpdateForLayout.upstreamVersion} is released, but this app origin still serves {appUpdateForLayout.deployedVersion}.
          </p>
          {#if appDeploymentOwner}
            {#if ['scheduled', 'preparing', 'deploying_app', 'installing', 'restarting'].includes(appDeploymentOwner.connection?.update.state || '')}
              <p class="hint" role="status">Publishing v{appUpdateForLayout.upstreamVersion} and waiting for this app origin to update. This can take up to two minutes; the relay remains online.</p>
            {:else if ['scheduled', 'deploying'].includes(appDeploymentOwner.connection?.appDeploy.state || '')}
              <p class="hint" role="status">Publishing v{appUpdateForLayout.upstreamVersion} from {appDeploymentOwner.relay.label} and waiting for this app origin to update. This can take up to two minutes.</p>
            {:else if appDeploymentOwner.connection?.appDeploy.state === 'failed'}
              <p class="warning" role="status">Deployment failed: {appDeploymentOwner.connection.appDeploy.error}</p>
            {:else if appDeploymentOwner.connection?.releaseVersion !== appUpdateForLayout.upstreamVersion}
              {#if appDeploymentOwner.connection && relayNeedsManualBootstrap(appDeploymentOwner.connection)}
                <p class="warning" role="status">{appDeploymentOwner.relay.label} needs the one-time Terminal bootstrap shown in Update Help before it can deploy this app version.</p>
              {:else if ownerUpdateReady}
                <p class="hint">{appDeploymentOwner.relay.label} can deploy the app and update to {appUpdateForLayout.upstreamVersion} in one safe step.</p>
              {:else}
                <p class="hint">No installable v{appUpdateForLayout.upstreamVersion} relay update is available from {appDeploymentOwner.relay.label} yet.</p>
              {/if}
            {:else}
              <p class="hint">{appDeploymentOwner.relay.label} is authorized to deploy this app origin.</p>
            {/if}
          {:else}
            <p class="hint">This is a separately hosted app. Configure one relay as its deployment owner:</p>
            <pre class="update-command"><code>{APP_DEPLOY_SETUP_COMMAND}</code></pre>
          {/if}
        {:else if appUpdateForLayout.state === 'checking'}
          <p class="hint" role="status">Checking this app origin and the upstream release…</p>
        {:else if appUpdateForLayout.state === 'failed'}
          <p class="hint" role="status">Could not verify app updates: {appUpdateForLayout.error}</p>
        {:else}
          <p class="hint" role="status">Phone app is current at v{appUpdateForLayout.upstreamVersion || APP_VERSION}.</p>
          {#if relayUpdateCount}
            <p class="warning" role="status">{relayUpdateCount} {relayUpdateCount === 1 ? 'relay update is' : 'relay updates are'} available.</p>
          {/if}
          {#if blockedRelayUpdateCount}
            <p class="warning" role="status">{blockedRelayUpdateCount} {blockedRelayUpdateCount === 1 ? 'relay update needs' : 'relay updates need'} attention.</p>
          {/if}
          {#if manualRelayUpdateCount}
            <p class="warning" role="status">{manualRelayUpdateCount} {manualRelayUpdateCount === 1 ? 'relay requires' : 'relays require'} a one-time manual update.</p>
          {/if}
        {/if}
      </div>
      {#if appUpdateChecking}
        <p class="hint app-update-status-checking" role="status">Checking this app origin and the upstream release…</p>
      {/if}
    </div>
    <div class="form-actions">
      <Button
        class="update-check-button"
        variant="secondary"
        aria-busy={appUpdateChecking}
        disabled={appUpdateChecking}
        onclick={checkAppAndRelays}
      >Check for Updates</Button>
      {#if updatePending}
        <Button disabled={!safeUpdateAction || Boolean(busyRelayId)} onclick={requestSafeUpdate}>
          {updateActionLabel(safeUpdateAction)}
        </Button>
      {/if}
    </div>
    <p class="hint">Relay-hosted apps update with their relay. A separately hosted Pages app can be deployed only by its configured owner relay.</p>
  </Card>
</main>

<AppDialog
  id="update-herdr-dialog"
  bind:open={updateOpen}
  title={updateActionLabel(pendingUpdateAction)}
  description={pendingUpdateAction?.description || 'No safe update path is currently available.'}
>
  <p class="hint">Herdr selects the safe order automatically: publish the phone app first when required, then update each relay one at a time while preserving running agents.</p>
  <div class="dialog-actions">
    <Button disabled={!pendingUpdateAction || Boolean(busyRelayId)} onclick={startSafeUpdate}>
      {pendingUpdateAction?.kind === 'reload_app' ? 'Load Update' : 'Start Update'}
    </Button>
    <Button variant="ghost" onclick={() => { updateOpen = false; pendingUpdateAction = null; }}>Cancel</Button>
  </div>
</AppDialog>

<AppDialog
  id="manual-relay-update-dialog"
  bind:open={manualOpen}
  title={manualRow ? `Update ${manualRow.relay.label}` : 'Update Relay'}
  description="This relay needs a one-time Terminal update before phone-driven updates can continue."
>
  <p>On the computer running this relay, open Terminal and run:</p>
  <pre class="update-command"><code>{MANAGED_UPDATE_COMMAND}</code></pre>
  <p class="hint">This updates the Marketplace plugin, preserves the configuration used by an existing stable service, and restarts the relay.</p>
  <div class="dialog-actions">
    <Button onclick={() => copyUpdateCommand(MANAGED_UPDATE_COMMAND, 'Marketplace')}>Copy Command</Button>
    <Button variant="ghost" onclick={() => { manualOpen = false; }}>Close</Button>
  </div>
  <details class="checkout-update">
    <summary>Prefer to keep using a source checkout?</summary>
    <p class="hint">Run this from the checkout directory:</p>
    <pre class="update-command"><code>{CHECKOUT_UPDATE_COMMAND}</code></pre>
    <Button variant="secondary" size="sm" onclick={() => copyUpdateCommand(CHECKOUT_UPDATE_COMMAND, 'Source checkout')}>Copy Checkout Command</Button>
  </details>
</AppDialog>

<AppDialog
  id="remove-relay-dialog"
  bind:open={removalOpen}
  title={removalRow ? `Remove ${removalRow.relay.label}?` : 'Remove Relay?'}
  description="This removes the saved relay connection and its push subscription from this phone."
>
  {#if removalRow}
    <p class="hint">{removalRow.relay.url}</p>
  {/if}
  <p>Agents on the computer keep running. You will need its setup link or connection details to add it again.</p>
  <div class="dialog-actions">
    <Button variant="danger" disabled={!removalRow} onclick={confirmRelayRemoval}>Remove Relay</Button>
    <Button variant="ghost" onclick={() => { removalOpen = false; }}>Cancel</Button>
  </div>
</AppDialog>
