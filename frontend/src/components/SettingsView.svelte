<script lang="ts">
  import { onMount } from 'svelte';
  import AppDialog from '$components/ui/AppDialog.svelte';
  import AppSwitch from '$components/ui/AppSwitch.svelte';
  import Button from '$components/ui/Button.svelte';
  import Card from '$components/ui/Card.svelte';
  import {
    APP_VERSION,
    INTERFACE_SIZES,
    TERMINAL_HISTORY_OPTIONS,
    THEMES,
    type InterfaceSize,
    type TerminalHistoryLines,
    type Theme,
  } from '$lib/config';
  import {
    interfaceSize,
    setInterfaceSize,
    setShowAgentStatusLine,
    setTerminalHistoryLines,
    setTheme,
    showAgentStatusLine,
    terminalHistoryLines,
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
    checkAppUpdate,
    reloadApp,
  } from '$lib/updates';
  import type { RelayConnectionView } from '$lib/types';

  const MANAGED_UPDATE_COMMAND = 'HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1 herdr plugin install 0cv/herdr-mobile-relay --yes && herdr plugin action invoke install-service --plugin herdr-mobile-relay.events';
  const CHECKOUT_UPDATE_COMMAND = 'git pull --ff-only && make service-install';
  const APP_DEPLOY_SETUP_COMMAND = 'herdr plugin action invoke configure-app-deploy --plugin herdr-mobile-relay.events';

  const relays = relayStore.relayConfigs;
  const connections = relayStore.connections;
  const agents = relayStore.agents;
  const notificationBusy = relayStore.notificationBusy;
  const appUpdate = appUpdateStatus;

  onMount(refreshPushPreferences);

  let relayLabel = $state('');
  let relayUrl = $state('');
  let relayToken = $state('');
  let finished = $state(finishedNotificationsEnabled());
  let deviceLock = $state(deviceVerificationEnabled());
  let confirmRelayId = $state('');
  let confirmOpen = $state(false);
  let manualRelayId = $state('');
  let manualOpen = $state(false);
  let removalRelayId = $state('');
  let removalOpen = $state(false);
  let deployRelayId = $state('');
  let deployOpen = $state(false);
  let updateDeployOpen = $state(false);
  let busyRelayId = $state('');

  const relayRows = $derived($relays.map((relay) => ({
    relay,
    connection: $connections.get(relay.id),
  })));
  const connectedCount = $derived([...$connections.values()].filter((connection) => connection.status === 'connected').length);
  const confirmRow = $derived(relayRows.find(({ relay }) => relay.id === confirmRelayId));
  const manualRow = $derived(relayRows.find(({ relay }) => relay.id === manualRelayId));
  const removalRow = $derived(relayRows.find(({ relay }) => relay.id === removalRelayId));
  const deployRow = $derived(relayRows.find(({ relay }) => relay.id === deployRelayId));
  const appDeploymentOwner = $derived(relayRows.find(({ connection }) => (
    connection?.status === 'connected'
    && connection.capabilities.includes('app_deploy')
    && connection.appDeploy.configured
    && connection.appDeploy.origin === location.origin
  )));
  // The owner relay is behind the released app version but can self-update to
  // exactly that version, so a single action can update it and then deploy.
  const ownerUpdateReady = $derived.by(() => {
    const connection = appDeploymentOwner?.connection;
    if (!connection || connection.releaseVersion === $appUpdate.upstreamVersion) return false;
    const update = connection.update;
    return connection.capabilities.includes('self_update')
      && update.state === 'available'
      && update.can_install
      && update.available_version === $appUpdate.upstreamVersion;
  });
  const notification = $derived.by(() => notificationMeta(
    [...$connections.values()],
    $notificationBusy,
    $pushPreferences,
  ));

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
    if (!connection?.capabilities.includes('self_update')) {
      return {
        label: 'Manual update required',
        detail: 'Open Update Help for the one-time setup.',
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
    if (['scheduled', 'installing', 'restarting'].includes(update.state)) {
      const label = update.state === 'scheduled'
        ? 'Update scheduled…'
        : update.state === 'installing' ? 'Installing update…' : 'Restarting relay…';
      return { label, detail: 'The phone connection may briefly disconnect.', warning: true };
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
  }

  function requestRelayUpdate(relayId: string) {
    confirmRelayId = relayId;
    confirmOpen = true;
  }

  function showManualUpdate(relayId: string) {
    manualRelayId = relayId;
    manualOpen = true;
  }

  async function copyUpdateCommand(command: string, installation: string) {
    if (!navigator.clipboard?.writeText) {
      relayStore.showToast('Clipboard access is unavailable. Select the command manually.', true);
      return;
    }
    try {
      await navigator.clipboard.writeText(command);
      relayStore.showToast(`${installation} update command copied.`);
    } catch {
      relayStore.showToast('Could not copy the command. Select it manually.', true);
    }
  }

  async function installRelayUpdate() {
    if (!confirmRelayId) return;
    const relayId = confirmRelayId;
    confirmOpen = false;
    busyRelayId = relayId;
    try {
      await relayStore.installRelayUpdate(relayId);
      relayStore.showToast('Update scheduled. The relay will reconnect after verification.');
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
    } finally {
      busyRelayId = '';
    }
  }

  function requestAppDeployment(relayId: string) {
    deployRelayId = relayId;
    deployOpen = true;
  }

  async function deployAppUpdate() {
    if (!deployRelayId || !$appUpdate.upstreamVersion) return;
    const relayId = deployRelayId;
    deployOpen = false;
    busyRelayId = relayId;
    try {
      await relayStore.deployAppUpdate(relayId, $appUpdate.upstreamVersion);
      relayStore.showToast('App deployment scheduled. Herdr will reload after the public origin is verified.');
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
    } finally {
      busyRelayId = '';
    }
  }

  function requestUpdateAndDeploy() {
    updateDeployOpen = true;
  }

  async function updateRelayAndDeploy() {
    const owner = appDeploymentOwner;
    updateDeployOpen = false;
    if (!owner || !$appUpdate.upstreamVersion) return;
    busyRelayId = owner.relay.id;
    try {
      await relayStore.updateRelayAndDeploy(owner.relay.id, $appUpdate.upstreamVersion);
      relayStore.showToast(`Updating ${owner.relay.label}; the app will deploy once it reconnects.`);
    } catch (error) {
      relayStore.showToast((error as Error).message, true);
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
      <label for="relay-token">Token</label>
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
        <article class="relay-row">
          <span
            class={`status-dot status-${connectionStatus === 'connected' ? 'success' : connectionStatus === 'connecting' ? 'warning' : 'danger'}`}
            role="img"
            aria-label={`${relay.label} relay ${connectionStatus}`}
          ></span>
          <div class="relay-info">
            <strong>{relay.label}</strong>
            <span>{relay.url}</span>
            <small>Push: {pushStatusLabel(connection)}</small>
            {#if version}<small class:warning={version.tone === 'warning'} title={version.title}>{version.label}</small>{/if}
            <small class:warning={update.warning} role="status">{update.label}</small>
            {#if update.detail}<small class:warning={update.warning} title={update.detail}>{update.detail}</small>{/if}
          </div>
          <div class="relay-actions">
            {#if connection?.capabilities.includes('self_update')}
              <Button
                variant="secondary"
                size="sm"
                disabled={connectionStatus !== 'connected' || busyRelayId === relay.id || ['scheduled', 'installing', 'restarting'].includes(connection.update.state)}
                aria-label={`Check ${relay.label} for updates`}
                onclick={() => checkRelayUpdate(relay.id)}
              >Check</Button>
              {#if connection.update.state === 'available'}
                <Button
                  size="sm"
                  disabled={!connection.update.can_install || busyRelayId === relay.id}
                  aria-label={`Update ${relay.label} to version ${connection.update.available_version}`}
                  onclick={() => requestRelayUpdate(relay.id)}
                >Update</Button>
              {/if}
            {:else if connectionStatus === 'connected'}
              <Button
                variant="secondary"
                size="sm"
                aria-label={`How to update ${relay.label}`}
                onclick={() => showManualUpdate(relay.id)}
              >Update Help</Button>
            {/if}
            <Button variant="danger" size="sm" aria-label={`Remove ${relay.label}`} onclick={() => requestRelayRemoval(relay.id)}>Remove</Button>
          </div>
        </article>
      {/each}
    </div>
    <p class="hint">Use one relay URL per computer. Relay tokens remain in this browser’s local storage.</p>
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
    <p class="hint">Lines requested per terminal. 5,000–10,000 lines can use substantially more network data and rendering work.</p>
    <AppSwitch checked={$showAgentStatusLine} label="Show Agent Status Line" onchange={setShowAgentStatusLine} />
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
    <p class="hint">{deviceVerificationSupported() ? $securityState.hint : 'Device verification needs HTTPS and a browser with WebAuthn support.'}</p>
  </Card>

  <Card>
    <h3>Status</h3>
    <p><span class={`status-dot status-${connectedCount ? 'success' : 'danger'}`}></span> {connectedCount}/{$relays.length} relays connected · {$agents.length} agents</p>
  </Card>

  <Card>
    <h3>About</h3>
    <p>Phone app version {APP_VERSION}</p>
    {#if $appUpdate.state === 'reload-ready'}
      <p class="warning" role="status">Version {$appUpdate.deployedVersion} is deployed to this app origin and ready to load.</p>
    {:else if $appUpdate.state === 'deployment-required'}
      <p class="warning" role="status">
        Version {$appUpdate.upstreamVersion} is released, but this app origin still serves {$appUpdate.deployedVersion}.
      </p>
      {#if appDeploymentOwner}
        {#if ['scheduled', 'deploying'].includes(appDeploymentOwner.connection?.appDeploy.state || '')}
          <p class="hint" role="status">Deploying from {appDeploymentOwner.relay.label}…</p>
        {:else if appDeploymentOwner.connection?.appDeploy.state === 'failed'}
          <p class="warning" role="status">Deployment failed: {appDeploymentOwner.connection.appDeploy.error}</p>
        {:else if appDeploymentOwner.connection?.releaseVersion !== $appUpdate.upstreamVersion}
          {#if ownerUpdateReady}
            <p class="hint">{appDeploymentOwner.relay.label} can update to {$appUpdate.upstreamVersion} and deploy in one step.</p>
          {:else}
            <p class="hint">Update {appDeploymentOwner.relay.label} to {$appUpdate.upstreamVersion} first, then deploy the app.</p>
          {/if}
        {:else}
          <p class="hint">{appDeploymentOwner.relay.label} is authorized to deploy this app origin.</p>
        {/if}
      {:else}
        <p class="hint">This is a separately hosted app. Configure one relay as its deployment owner:</p>
        <pre class="update-command"><code>{APP_DEPLOY_SETUP_COMMAND}</code></pre>
      {/if}
    {:else if $appUpdate.state === 'checking'}
      <p class="hint" role="status">Checking this app origin and the upstream release…</p>
    {:else if $appUpdate.state === 'failed'}
      <p class="hint" role="status">Could not verify app updates: {$appUpdate.error}</p>
    {:else}
      <p class="hint" role="status">This app matches upstream version {$appUpdate.upstreamVersion || APP_VERSION}.</p>
    {/if}
    <div class="form-actions">
      <Button variant="secondary" disabled={$appUpdate.state === 'checking'} onclick={checkAppAndRelays}>Check App</Button>
      {#if $appUpdate.state === 'deployment-required' && appDeploymentOwner?.connection?.releaseVersion === $appUpdate.upstreamVersion}
        <Button
          disabled={['scheduled', 'deploying'].includes(appDeploymentOwner.connection.appDeploy.state)}
          onclick={() => requestAppDeployment(appDeploymentOwner.relay.id)}
        >Deploy App</Button>
      {:else if $appUpdate.state === 'deployment-required' && ownerUpdateReady}
        <Button
          disabled={busyRelayId === appDeploymentOwner?.relay.id}
          onclick={requestUpdateAndDeploy}
        >Update relay &amp; deploy</Button>
      {/if}
      <Button disabled={$appUpdate.state !== 'reload-ready'} onclick={reloadApp}>Reload App</Button>
    </div>
    <p class="hint">Relay-hosted apps update with their relay. A separately hosted Pages app can be deployed only by its configured owner relay.</p>
  </Card>
</main>

<AppDialog
  id="update-and-deploy-dialog"
  bind:open={updateDeployOpen}
  title="Update relay &amp; deploy"
  description={appDeploymentOwner
    ? `Update ${appDeploymentOwner.relay.label} to v${$appUpdate.upstreamVersion || 'unknown'}, then deploy the app to ${appDeploymentOwner.connection?.appDeploy.origin || 'the configured origin'}?`
    : 'The deployment relay is unavailable.'}
>
  <p class="hint">The phone disconnects briefly while the relay updates and restarts. Once it reconnects at v{$appUpdate.upstreamVersion}, the app deploys automatically and Herdr reloads after the public origin is verified.</p>
  <div class="dialog-actions">
    <Button disabled={!appDeploymentOwner} onclick={updateRelayAndDeploy}>Update &amp; Deploy</Button>
    <Button variant="ghost" onclick={() => { updateDeployOpen = false; }}>Cancel</Button>
  </div>
</AppDialog>

<AppDialog
  id="manual-relay-update-dialog"
  bind:open={manualOpen}
  title={manualRow ? `Update ${manualRow.relay.label}` : 'Update Relay'}
  description="Version 0.7.0 is a one-time manual update. Later relay updates can be installed from this screen."
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
  id="app-deploy-dialog"
  bind:open={deployOpen}
  title="Deploy Phone App"
  description={deployRow
    ? `Deploy app version ${$appUpdate.upstreamVersion || 'unknown'} from ${deployRow.relay.label} to ${deployRow.connection?.appDeploy.origin || 'the configured origin'}?`
    : 'The deployment relay is unavailable.'}
>
  <p class="hint">The relay deploys only its verified committed web bundle to its configured Cloudflare Pages project. Cloudflare credentials remain on that computer.</p>
  <div class="dialog-actions">
    <Button disabled={!deployRow} onclick={deployAppUpdate}>Deploy App</Button>
    <Button variant="ghost" onclick={() => { deployOpen = false; }}>Cancel</Button>
  </div>
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

<AppDialog
  id="relay-update-dialog"
  bind:open={confirmOpen}
  title="Update Relay"
  description={confirmRow
    ? `Update ${confirmRow.relay.label} from v${confirmRow.connection?.update.current_version || 'unknown'} to v${confirmRow.connection?.update.available_version || 'unknown'}?`
    : 'The selected relay is unavailable.'}
>
  <p class="hint">The phone will disconnect briefly. Running agents and saved relay configuration remain intact.</p>
  <div class="dialog-actions">
    <Button disabled={!confirmRow} onclick={installRelayUpdate}>Update Relay</Button>
    <Button variant="ghost" onclick={() => { confirmOpen = false; }}>Cancel</Button>
  </div>
</AppDialog>
