<script lang="ts">
  import { tick } from 'svelte';
  import AppDialog from '$components/ui/AppDialog.svelte';
  import Button from '$components/ui/Button.svelte';
  import Card from '$components/ui/Card.svelte';
  import {
    createDeviceInvitation,
    renameDevice,
    resetDevices,
    revokeDevice,
    type CreateInvitationIntent,
    type DeviceIntentHandler,
    type DeviceRole,
    type DeviceSummary,
    type RenameDeviceIntent,
    type ResetDevicesIntent,
    type RevokeDeviceIntent,
  } from '$lib/device-auth';

  let {
    relayId,
    relayLabel,
    devices,
    currentDeviceId = '',
    connected,
    canAdminister,
    canInvite = true,
    busy = false,
    onRename,
    onInvite,
    onRevoke,
    onReset,
    onForgetCurrent,
  }: {
    relayId: string;
    relayLabel: string;
    devices: readonly DeviceSummary[];
    currentDeviceId?: string;
    connected: boolean;
    canAdminister: boolean;
    canInvite?: boolean;
    busy?: boolean;
    onRename: DeviceIntentHandler<RenameDeviceIntent>;
    onInvite: DeviceIntentHandler<CreateInvitationIntent, string>;
    onRevoke: DeviceIntentHandler<RevokeDeviceIntent>;
    onReset: DeviceIntentHandler<ResetDevicesIntent>;
    onForgetCurrent: () => Promise<void>;
  } = $props();

  let renameTarget = $state<DeviceSummary | null>(null);
  let revokeTarget = $state<DeviceSummary | null>(null);
  let renameOpen = $state(false);
  let inviteOpen = $state(false);
  let revokeOpen = $state(false);
  let resetOpen = $state(false);
  let forgetOpen = $state(false);
  let name = $state('');
  let inviteName = $state('');
  let inviteRole = $state<DeviceRole>('reader');
  let resetConfirmation = $state('');
  let actionBusy = $state(false);
  let status = $state('');
  let error = $state(false);
  let invitationLink = $state('');
  let safeButton = $state<HTMLButtonElement | null>(null);
  let nameInput = $state<HTMLInputElement | null>(null);

  const disabled = $derived(busy || actionBusy);
  const currentDevice = $derived(devices.find((device) => device.deviceId === currentDeviceId));
  const sortedDevices = $derived([...devices].sort((left, right) => {
    if (left.deviceId === currentDeviceId) return -1;
    if (right.deviceId === currentDeviceId) return 1;
    return (right.lastSeenAt ?? 0) - (left.lastSeenAt ?? 0) || left.name.localeCompare(right.name);
  }));

  $effect(() => {
    if (!renameOpen) renameTarget = null;
    if (!revokeOpen) revokeTarget = null;
  });

  async function focusSafeButton() {
    await tick();
    safeButton?.focus();
  }

  async function beginRename(device: DeviceSummary) {
    renameTarget = device;
    name = device.name;
    renameOpen = true;
    await tick();
    nameInput?.focus();
    nameInput?.select();
  }

  async function beginInvite() {
    inviteName = '';
    inviteRole = 'reader';
    invitationLink = '';
    inviteOpen = true;
    await tick();
    nameInput?.focus();
  }

  function beginRevoke(device: DeviceSummary) {
    revokeTarget = device;
    revokeOpen = true;
    void focusSafeButton();
  }

  function beginReset() {
    resetConfirmation = '';
    resetOpen = true;
    void focusSafeButton();
  }

  function beginForget() {
    forgetOpen = true;
    void focusSafeButton();
  }

  async function run(action: () => Promise<void>, success: string, close: () => void) {
    actionBusy = true;
    status = '';
    error = false;
    try {
      await action();
      close();
      status = success;
    } catch (cause) {
      error = true;
      status = cause instanceof Error && cause.message ? cause.message : 'The device action failed.';
    } finally {
      actionBusy = false;
    }
  }

  function submitRename(event: SubmitEvent) {
    event.preventDefault();
    const target = renameTarget;
    if (!target) return;
    void run(
      () => renameDevice({ relayId, deviceId: target.deviceId, name }, onRename),
      'Device name saved.',
      () => { renameOpen = false; },
    );
  }

  function submitInvite(event: SubmitEvent) {
    event.preventDefault();
    void run(
      async () => {
        invitationLink = await createDeviceInvitation({ relayId, name: inviteName, role: inviteRole }, onInvite);
      },
      'Invitation created. Share the one-use link below before it expires.',
      () => { inviteOpen = false; },
    );
  }

  function confirmRevoke() {
    const target = revokeTarget;
    if (!target) return;
    void run(
      () => revokeDevice({ relayId, deviceId: target.deviceId }, onRevoke),
      `${target.name} was revoked.`,
      () => { revokeOpen = false; },
    );
  }

  function confirmReset() {
    if (resetConfirmation !== 'RESET') return;
    void run(
      () => resetDevices({ relayId }, onReset),
      'All device credentials were reset.',
      () => { resetOpen = false; },
    );
  }

  function confirmForget() {
    if (!connected) return;
    void run(
      onForgetCurrent,
      'This browser was revoked and its local credential was erased.',
      () => { forgetOpen = false; },
    );
  }

  const dateFormatter = new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'short',
  });

  function formatDate(value: number | undefined): string {
    if (!value) return 'Never';
    return dateFormatter.format(new Date(value));
  }
  async function copyInvitationLink(): Promise<void> {
    if (!invitationLink) return;
    try {
      await navigator.clipboard.writeText(invitationLink);
      status = 'Invitation link copied.';
      error = false;
    } catch {
      status = 'Copy failed. Select and copy the invitation link manually.';
      error = true;
    }
  }
</script>

<Card aria-labelledby={`device-settings-${relayId}`}>
  <div class="device-settings">
    <header class="device-heading">
      <div>
        <h3 id={`device-settings-${relayId}`}>Devices</h3>
        <p class="hint">{relayLabel}</p>
      </div>
      {#if canAdminister}
        <Button
          variant="secondary"
          size="sm"
          disabled={disabled || !connected || !canInvite}
          title={canInvite ? 'Invite another device' : 'Configure a direct encrypted WebSocket endpoint to create invitation links'}
          onclick={beginInvite}
        >
          Invite Device
        </Button>
      {/if}
    </header>

    <p class="hint storage-notice">
      Device credentials are stored in this browser's local storage. They are revocable, but are not hardware-backed.
    </p>

    {#if status}
      <p class:error role={error ? 'alert' : 'status'} aria-live="polite">{status}</p>
    {/if}
    {#if invitationLink}
      <div class="invitation-link">
        <label for={`device-invitation-${relayId}`}>One-use invitation link</label>
        <input id={`device-invitation-${relayId}`} readonly value={invitationLink} />
        <Button variant="secondary" size="sm" onclick={copyInvitationLink}>Copy link</Button>
      </div>
    {/if}

    {#if currentDevice}
      <section class="current-summary" aria-label="Current device">
        <span>Current device</span>
        <strong>{currentDevice.name}</strong>
        <span class="role role-{currentDevice.role}">{currentDevice.role === 'controller' ? 'Controller' : 'Reader'}</span>
      </section>
    {/if}

    <div class="device-list-heading">
      <h4>Paired devices</h4>
      <span>{devices.length}</span>
    </div>

    {#if sortedDevices.length}
      <ul class="device-list">
        {#each sortedDevices as device (device.deviceId)}
          <li>
            <article class="device-row" aria-label={`${device.name}, ${device.role}`}>
              <div class="device-primary">
                <div class="device-name-line">
                  <strong>{device.name}</strong>
                  {#if device.deviceId === currentDeviceId}<span class="current-label">This browser</span>{/if}
                </div>
                <dl>
                  <div><dt>Role</dt><dd>{device.role === 'controller' ? 'Controller' : 'Reader'}</dd></div>
                  <div><dt>Paired</dt><dd>{formatDate(device.pairedAt)}</dd></div>
                  <div><dt>Last seen</dt><dd>{formatDate(device.lastSeenAt)}</dd></div>
                </dl>
              </div>
              {#if canAdminister}
                <div class="device-actions">
                  <Button variant="ghost" size="sm" disabled={disabled || !connected} onclick={() => beginRename(device)}>
                    Rename
                  </Button>
                  <Button variant="danger" size="sm" disabled={disabled || !connected} onclick={() => beginRevoke(device)}>
                    Revoke
                  </Button>
                </div>
              {/if}
            </article>
          </li>
        {/each}
      </ul>
    {:else}
      <p class="hint">No paired devices were returned by this relay.</p>
    {/if}

    <div class="danger-actions">
      {#if currentDeviceId}
        <Button variant="secondary" disabled={disabled} onclick={beginForget}>Forget This Browser</Button>
      {/if}
      {#if canAdminister}
        <Button variant="danger" disabled={disabled || !connected || devices.length === 0} onclick={beginReset}>
          Reset All Devices
        </Button>
      {/if}
    </div>
  </div>
</Card>

<AppDialog id={`rename-device-${relayId}`} bind:open={renameOpen} title="Rename Device" description="Names identify paired browsers on this relay.">
  <form class="form-stack" onsubmit={submitRename}>
    <label for={`rename-device-name-${relayId}`}>Device name</label>
    <input bind:this={nameInput} id={`rename-device-name-${relayId}`} bind:value={name} maxlength="64" autocomplete="off" required />
    <div class="dialog-actions">
      <Button type="submit" disabled={disabled}>Save</Button>
      <Button variant="ghost" disabled={disabled} onclick={() => { renameOpen = false; }}>Cancel</Button>
    </div>
  </form>
</AppDialog>

<AppDialog id={`invite-device-${relayId}`} bind:open={inviteOpen} title="Invite Device" description="Create a short-lived, one-use enrollment invitation.">
  <form class="form-stack" onsubmit={submitInvite}>
    <label for={`invite-device-name-${relayId}`}>Device name</label>
    <input bind:this={nameInput} id={`invite-device-name-${relayId}`} bind:value={inviteName} maxlength="64" autocomplete="off" required />
    <label for={`invite-device-role-${relayId}`}>Role</label>
    <select id={`invite-device-role-${relayId}`} bind:value={inviteRole}>
      <option value="reader">Reader — view only</option>
      <option value="controller">Controller — may perform allowed actions</option>
    </select>
    <p class="hint">The generated link carries the one-use secret in its URL fragment. Share it only with the intended device.</p>
    <div class="dialog-actions">
      <Button type="submit" disabled={disabled || !connected}>Create Invitation</Button>
      <Button variant="ghost" disabled={disabled} onclick={() => { inviteOpen = false; }}>Cancel</Button>
    </div>
  </form>
</AppDialog>

<AppDialog id={`revoke-device-${relayId}`} bind:open={revokeOpen} title="Revoke Device" description={revokeTarget ? `Revoke ${revokeTarget.name}?` : 'Revoke this device?'}>
  <div class="form-stack">
    <p class="warning" role="alert">This immediately closes that device's relay connection and removes its push endpoints and pending authorization.</p>
    <div class="dialog-actions">
      <Button variant="danger" disabled={disabled || !connected} onclick={confirmRevoke}>Confirm Revoke</Button>
      <button class="safe-cancel" bind:this={safeButton} disabled={disabled} onclick={() => { revokeOpen = false; }}>Cancel</button>
    </div>
  </div>
</AppDialog>

<AppDialog id={`reset-devices-${relayId}`} bind:open={resetOpen} title="Reset All Devices" description="Revoke every issued device credential for this relay.">
  <div class="form-stack">
    <p class="warning" role="alert">Every paired browser, including this one, will lose access. This cannot be undone.</p>
    <label for={`reset-device-confirmation-${relayId}`}>Type RESET to continue</label>
    <input id={`reset-device-confirmation-${relayId}`} bind:value={resetConfirmation} autocomplete="off" />
    <div class="dialog-actions">
      <Button variant="danger" disabled={disabled || !connected || resetConfirmation !== 'RESET'} onclick={confirmReset}>Confirm Reset</Button>
      <button class="safe-cancel" bind:this={safeButton} disabled={disabled} onclick={() => { resetOpen = false; }}>Cancel</button>
    </div>
  </div>
</AppDialog>

<AppDialog id={`forget-device-${relayId}`} bind:open={forgetOpen} title="Forget This Browser" description="Revoke this browser before erasing its local credential.">
  <div class="form-stack">
    {#if connected}
      <p class="warning" role="alert">This browser will be revoked at the relay first. Local material is erased only after revocation succeeds.</p>
    {:else}
      <p class="warning" role="alert">Connect to the relay to revoke this browser. Its local credential has not been erased, because offline erasure would leave an active credential behind.</p>
    {/if}
    <div class="dialog-actions">
      <Button variant="danger" disabled={disabled || !connected} onclick={confirmForget}>Confirm Forget</Button>
      <button class="safe-cancel" bind:this={safeButton} disabled={disabled} onclick={() => { forgetOpen = false; }}>Cancel</button>
    </div>
  </div>
</AppDialog>

<style>
  .device-settings { display: grid; gap: 1rem; }
  .device-heading, .device-list-heading, .device-name-line, .device-actions, .danger-actions { display: flex; align-items: center; gap: .65rem; }
  .device-heading, .device-list-heading { justify-content: space-between; }
  .device-heading h3, .device-list-heading h4, .device-heading p { margin: 0; }
  .device-heading p, .hint { color: var(--muted); }
  .device-list-heading h4 { font-size: .82rem; }
  .device-list-heading span { color: var(--muted); font-size: .74rem; }
  .storage-notice { margin: 0; }
  .current-summary { display: flex; align-items: center; flex-wrap: wrap; gap: .5rem; padding: .75rem; border-radius: .75rem; background: var(--secondary); }
  .current-summary > span:first-child { color: var(--muted); }
  .role, .current-label { border: 1px solid var(--border); border-radius: 999px; padding: .15rem .45rem; font-size: .75rem; }
  .role-controller { border-color: var(--primary); }
  .device-list { display: grid; gap: .65rem; margin: 0; padding: 0; list-style: none; }
  .device-row { display: flex; align-items: flex-start; justify-content: space-between; gap: .8rem; padding: .8rem 0; border-top: 1px solid var(--border); }
  .device-primary { min-width: 0; flex: 1; }
  .device-name-line strong { overflow-wrap: anywhere; }
  dl { display: flex; flex-wrap: wrap; gap: .4rem 1rem; margin: .55rem 0 0; }
  dl div { display: flex; gap: .3rem; font-size: .8rem; }
  dt { color: var(--muted); }
  dd { margin: 0; }
  .device-actions, .danger-actions { flex-wrap: wrap; }
  .danger-actions { justify-content: flex-end; padding-top: .25rem; }
  .error, .warning { color: var(--danger); }
  .safe-cancel { min-height: 2.5rem; padding: 0 1rem; border: 0; border-radius: .5rem; background: transparent; color: inherit; font: inherit; cursor: pointer; }
  .safe-cancel:focus-visible { outline: 2px solid var(--primary); outline-offset: 2px; }
  .safe-cancel:disabled { cursor: default; opacity: .5; }
  @media (max-width: 36rem) {
    .device-row { display: grid; }
    .device-actions { justify-content: flex-start; }
  }
</style>
