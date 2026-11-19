<script lang="ts">
  import Card from '$components/ui/Card.svelte';
  import {
    AGENT_VIEWS, AGENT_VIEW_LABELS, THEMES, INTERFACE_SIZES, HOME_LAYOUTS, HOME_LAYOUT_LABELS,
    TERMINAL_HISTORY_OPTIONS, TERMINAL_REFRESH_OPTIONS, TERMINAL_REFRESH_LABELS,
  } from '$lib/config';
  import {
    defaultAgentView, homeLayout, interfaceSize, terminalHistoryLines, terminalRefreshInterval, theme,
    setDefaultAgentView, setHomeLayout, setInterfaceSize, setTerminalHistoryLines, setTerminalRefreshInterval, setTheme,
  } from '$lib/preferences';
  import { relayStore } from '$lib/store';
  import SampleDevices from './SampleDevices.svelte';

  const { connections, relayConfigs, devices } = relayStore;
  const voices = $derived([...$connections.values()].flatMap(({ speechVoices }) => speechVoices || []));
</script>

{#snippet choices<T extends string | number>(title: string, items: readonly T[], current: T, select: (item: T) => void, label: (item: T) => string | number, layout = 'compact-grid')}
  <fieldset class="choice-grid {layout}">
    <legend>{title}</legend>
    {#each items as item (item)}
      <button class:active={current === item} type="button" aria-pressed={current === item} onclick={() => select(item)}>{label(item)}</button>
    {/each}
  </fieldset>
{/snippet}

<main class="page settings-page" aria-labelledby="settings-title">
  <h2 id="settings-title">Settings</h2>
  <Card>
    <h3>Sample computers</h3>
    <ul>
      {#each $relayConfigs as relay (relay.id)}
        {@const role = relayStore.deviceCredential(relay.id)?.role}
        <li>{relay.label} — {role === 'controller' ? 'Controller' : 'Reader'}</li>
      {/each}
    </ul>
    <p class="hint">Pairing and relay setup are unavailable in the demo. Reload the page to reset the sample data.</p>
  </Card>

  {#each $relayConfigs as relay (relay.id)}
    {@const credential = relayStore.deviceCredential(relay.id)}
    {@const connection = $connections.get(relay.id)}
    {#if credential?.kind === 'credential' && connection?.capabilities.includes('device_management')}
      <SampleDevices
        relayId={relay.id} label={relay.label} devices={$devices.get(relay.id) ?? []} currentDeviceId={credential.deviceId}
        canShowQr={credential.role === 'controller' && connection.status === 'connected' && connection.capabilities.includes('invitation_qr')}
      />
    {/if}
  {/each}

  <Card>
    <h3>Agents</h3>
    {@render choices('Default View', AGENT_VIEWS, $defaultAgentView, setDefaultAgentView, (item) => AGENT_VIEW_LABELS[item])}
    <p class="hint">Applies for this visit. Conversation falls back to Terminal when no sample history is available.</p>
  </Card>
  <Card>
    <h3>Appearance</h3>
    {@render choices('Theme', THEMES, $theme, setTheme, (item) => item, '')}
    {@render choices('Interface Size', INTERFACE_SIZES, $interfaceSize, setInterfaceSize, (item) => item.charAt(0).toUpperCase() + item.slice(1))}
    {@render choices('Home Workspaces', HOME_LAYOUTS, $homeLayout, setHomeLayout, (item) => HOME_LAYOUT_LABELS[item])}
    {@render choices('Terminal History', TERMINAL_HISTORY_OPTIONS, $terminalHistoryLines, setTerminalHistoryLines, (item) => item, 'history-grid')}
    {@render choices('Terminal Refresh', TERMINAL_REFRESH_OPTIONS, $terminalRefreshInterval, setTerminalRefreshInterval, (item) => TERMINAL_REFRESH_LABELS[item], 'history-grid refresh-grid')}
    <p class="hint">Appearance changes apply for this visit. Resize Session, terminal height leases and Keep Screen Awake are unavailable in the demo.</p>
  </Card>
  <Card aria-labelledby="sample-speech-title">
    <h3 id="sample-speech-title">Read Aloud</h3>
    <p>Voice downloads, removal and playback are unavailable in the demo.</p>
    <ul aria-label="Sample voices">
      {#each voices as voice (voice.language)}<li>{voice.name}</li>{/each}
    </ul>
  </Card>
  <Card aria-labelledby="sample-notifications-title">
    <h3 id="sample-notifications-title">Notifications</h3>
    <p role="status">Notification delivery: Off (unavailable in demo)</p>
    <p class="hint">The demo does not use your notification permission or send notifications.</p>
  </Card>
  <Card aria-labelledby="sample-updates-title">
    <h3 id="sample-updates-title">Updates</h3>
    <p>App and relay updates are unavailable in the demo.</p>
  </Card>
</main>
