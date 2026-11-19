<script lang="ts">
  import AppDialog from '$components/ui/AppDialog.svelte';
  import Button from '$components/ui/Button.svelte';
  import Card from '$components/ui/Card.svelte';
  import type { DeviceRole, DeviceSummary } from '$lib/device-auth';
  import { qrBitmap, type QrBitmap } from '$lib/qr';
  import { relayStore } from '$lib/store';

  let { relayId, label, devices, currentDeviceId, canShowQr }: {
    relayId: string;
    label: string;
    devices: readonly DeviceSummary[];
    currentDeviceId: string;
    canShowQr: boolean;
  } = $props();

  const formatter = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' });
  const roleLabel = (role: DeviceRole) => role === 'controller' ? 'Controller' : 'Reader';
  const formatDate = (value?: number) => value ? formatter.format(value) : 'Never';
  const current = $derived(devices.find((device) => device.deviceId === currentDeviceId));
  const sorted = $derived([...devices].sort((left, right) => {
    if (left.deviceId === currentDeviceId) return -1;
    if (right.deviceId === currentDeviceId) return 1;
    return (right.lastSeenAt ?? 0) - (left.lastSeenAt ?? 0) || left.name.localeCompare(right.name);
  }));

  let open = $state(false);
  let role = $state<DeviceRole>('reader');
  let qr = $state<QrBitmap | null>(null);
  let error = $state(false);
  let busy = $state(false);
  const qrLabel = 'Sample QR — Herdr demo (not a setup link)';

  function openPreview() {
    role = 'reader';
    qr = null;
    error = false;
    open = true;
  }

  async function showSampleQr() {
    if (busy || !canShowQr) return;
    busy = true;
    error = false;
    try {
      const { size, modules } = await relayStore.qrCode(relayId, 'Herdr demo');
      qr = qrBitmap(size, modules);
      error = !qr;
    } catch {
      error = true;
    } finally {
      busy = false;
    }
  }
</script>

<Card aria-labelledby={`device-settings-${relayId}`}>
  <div class="device-settings">
    <header class="device-heading">
      <div>
        <h3 id={`device-settings-${relayId}`}>Devices</h3>
        <p class="hint">{label}</p>
      </div>
      {#if canShowQr}<Button variant="secondary" size="sm" onclick={openPreview}>Invite Device</Button>{/if}
    </header>
    <p class="hint storage-notice">These are sample devices. Pairing and device administration are unavailable in the demo.</p>
    {#if current}
      <section class="current-summary" aria-label="Current device">
        <span>Current device</span>
        <strong>{current.name}</strong>
        <span class="role role-{current.role}">{roleLabel(current.role)}</span>
      </section>
    {/if}
    <div class="device-list-heading"><h4>Paired devices</h4><span>{devices.length}</span></div>
    <ul class="device-list">
      {#each sorted as device (device.deviceId)}
        <li>
          <article class="device-row" aria-label={`${device.name}, ${device.role}`}>
            <div class="device-name-line">
              <strong>{device.name}</strong>
              {#if device.deviceId === currentDeviceId}<span class="current-label">This browser</span>{/if}
            </div>
            <dl>
              <div><dt>Role</dt><dd>{roleLabel(device.role)}</dd></div>
              <div><dt>Paired</dt><dd>{formatDate(device.pairedAt)}</dd></div>
              <div><dt>Last seen</dt><dd>{formatDate(device.lastSeenAt)}</dd></div>
            </dl>
          </article>
        </li>
      {/each}
    </ul>
  </div>
</Card>

<AppDialog id={`invite-device-${relayId}`} bind:open title="Invite Device" description="Preview an invitation. No invitation is created in the demo.">
  <div class="form-stack">
    <label for={`invite-device-role-${relayId}`}>Role</label>
    <select id={`invite-device-role-${relayId}`} bind:value={role}>
      <option value="reader">Reader — view only</option>
      <option value="controller">Controller — may perform allowed actions</option>
    </select>
    {#if qr}
      <svg class="invitation-qr" viewBox={`-4 -4 ${qr.size + 8} ${qr.size + 8}`} role="img" aria-label={qrLabel} shape-rendering="crispEdges">
        <rect x={-4} y={-4} width={qr.size + 8} height={qr.size + 8} fill="#fff" />
        <path d={qr.path} fill="#000" />
      </svg>
      <p class="hint">{qrLabel}.</p>
    {:else if error}
      <p class="error" role="alert">The sample QR could not be shown.</p>
    {/if}
    <div class="dialog-actions">
      <Button disabled={busy || !canShowQr} onclick={showSampleQr}>Show sample QR</Button>
      <Button variant="ghost" onclick={() => { open = false; }}>{qr ? 'Close' : 'Cancel'}</Button>
    </div>
  </div>
</AppDialog>

<style>
  .device-settings { display: grid; gap: 1rem; }
  .device-heading, .device-list-heading, .device-name-line { display: flex; align-items: center; gap: .65rem; }
  .device-heading, .device-list-heading { justify-content: space-between; }
  .device-heading h3, .device-list-heading h4, .device-heading p, .storage-notice { margin: 0; }
  .device-list-heading h4 { font-size: .82rem; }
  .device-list-heading span, .current-label { color: var(--muted); font-size: .74rem; }
  .current-summary, .device-row { background: var(--secondary); border: 1px solid var(--border); border-radius: .6rem; padding: .65rem; }
  .current-summary { display: flex; gap: .55rem; align-items: center; flex-wrap: wrap; font-size: .8rem; }
  .role { margin-left: auto; font-size: .72rem; }
  .role-controller { color: var(--primary); }
  .device-list { display: grid; gap: .55rem; margin: 0; padding: 0; list-style: none; }
  .device-row dl { margin: .45rem 0 0; font-size: .72rem; }
  .device-row dl div { display: flex; gap: .5rem; }
  .device-row dd { margin: 0; }
  .device-row dt { color: var(--muted); }
  .invitation-qr { width: min(15rem, 100%); height: auto; justify-self: center; background: white; }
</style>
