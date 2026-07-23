<script lang="ts">
  import Button from '$components/ui/Button.svelte';
  import Card from '$components/ui/Card.svelte';
  import { clientPaneId } from '$lib/agents';
  import { suggestedLaunchName } from '$lib/launch';
  import { replaceView } from '$lib/router';
  import { relayStore } from '$lib/store';

  const relays = relayStore.relayConfigs;
  const connections = relayStore.connections;

  let relayId = $state('');
  let profileId = $state('');
  let cwd = $state('');
  let name = $state('');
  let prompt = $state('');
  let directoryOpen = $state(false);
  let status = $state('');
  let error = $state(false);
  let submitting = $state(false);
  let loadedRelay = '';
  let directoryLoadGeneration = 0;
  let directoryRelayId = '';
  let directoryBrowser: HTMLDivElement;

  const connectedRelays = $derived($relays.filter((relay) => {
    const connection = $connections.get(relay.id);
    return connection?.status === 'connected' && connection.inventory.state === 'ready';
  }));
  const unavailableRelays = $derived($relays.filter((relay) => {
    const connection = $connections.get(relay.id);
    return connection?.status === 'connected' && connection.inventory.state !== 'ready';
  }));
  const connection = $derived($connections.get(relayId));
  const profiles = $derived(connection?.agentProfiles || []);

  $effect(() => {
    if (!connectedRelays.some((relay) => relay.id === relayId)) relayId = connectedRelays[0]?.id || '';
    if (!profiles.some((profile) => profile.id === profileId)) profileId = profiles[0]?.id || '';
    if (relayId && relayId !== loadedRelay) {
      loadedRelay = relayId;
      cwd = '';
      directoryRelayId = '';
      void loadDirectory('');
    }
  });

  async function loadDirectory(path: string) {
    const loadRelayId = relayId;
    const loadConnection = connection;
    const generation = ++directoryLoadGeneration;
    if (!loadRelayId || !loadConnection?.capabilities.includes('directory_browser')) return;
    try {
      const listing = await relayStore.listDirectories(loadRelayId, path);
      if (generation !== directoryLoadGeneration || relayId !== loadRelayId) return;
      cwd = listing.current.path;
      directoryRelayId = loadRelayId;
      name = suggestedLaunchName(cwd, profileId);
    } catch {
      // The store exposes the relay error next to the directory browser.
    }
  }

  function updateName() {
    name = suggestedLaunchName(cwd, profileId);
  }

  function closeDirectoryForOtherField(event: FocusEvent) {
    if (event.target instanceof Node && !directoryBrowser.contains(event.target)) directoryOpen = false;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!relayId || directoryRelayId !== relayId || !profileId || !cwd || !name) return;
    submitting = true;
    error = false;
    status = 'Starting agent…';
    try {
      const launchName = name.trim();
      const launchCwd = cwd.trim();
      const result = await relayStore.sendCommand(relayId, {
        type: 'agent_start', profile_id: profileId, name: launchName, cwd: launchCwd, prompt,
      }, 45_000);
      const warning = String(result.data?.warning || '');
      status = warning || 'Agent started.';
      error = Boolean(warning);
      prompt = '';
      name = '';
      relayStore.showToast(status, error);
      const rawPaneId = String(result.data?.pane_id || '');
      const launchedAgent = await relayStore.waitForAgent(relayId, {
        rawPaneId,
        name: launchName,
        cwd: launchCwd,
      });
      const paneId = launchedAgent?.pane_id || (rawPaneId ? clientPaneId(relayId, rawPaneId) : '');
      replaceView(paneId
        ? { view: 'terminal', paneId }
        : { view: 'agents' });
    } catch (caught) {
      status = (caught as Error).message;
      error = true;
      relayStore.showToast(status, true);
    } finally {
      submitting = false;
    }
  }
</script>

<main class="page launch-page" aria-labelledby="launch-title">
  <h2 id="launch-title">Start Agent</h2>
  <Card>
    <form class="form-stack" onfocusin={closeDirectoryForOtherField} onsubmit={submit}>
      <label for="launch-relay">Computer</label>
      <select id="launch-relay" bind:value={relayId} required>
        {#if !connectedRelays.length}<option value="">No ready relays</option>{/if}
        {#each connectedRelays as relay (relay.id)}<option value={relay.id}>{relay.label}</option>{/each}
      </select>
      {#if unavailableRelays.length}
        <p class="warning" role="status">Agent inventory is unavailable on {unavailableRelays.map((relay) => relay.label).join(', ')}.</p>
      {/if}

      <label for="launch-profile">Agent</label>
      <select id="launch-profile" bind:value={profileId} onchange={updateName} required>
        {#if !profiles.length}<option value="">No agent profiles available</option>{/if}
        {#each profiles as profile (profile.id)}<option value={profile.id}>{profile.label || profile.id}</option>{/each}
      </select>

      <span id="launch-cwd-label" class="field-label">Working Directory</span>
      <div bind:this={directoryBrowser} class:open={directoryOpen} class="directory-browser" aria-labelledby="launch-cwd-label">
        <div class="directory-toolbar">
          <Button
            size="icon"
            variant="secondary"
            aria-label="Open parent directory"
            disabled={!connection?.directoryBrowser?.parent}
            onclick={() => connection?.directoryBrowser?.parent && loadDirectory(connection.directoryBrowser.parent)}
          >↑</Button>
          <button
            class="directory-current"
            type="button"
            aria-expanded={directoryOpen}
            aria-controls="launch-directory-list"
            onclick={() => { directoryOpen = !directoryOpen; }}
          >
            <span>{connection?.directoryBrowser?.current.label || cwd || (connection?.directoryLoading ? 'Loading…' : 'Unavailable')}</span>
            <span aria-hidden="true">⌄</span>
          </button>
        </div>
        {#if directoryOpen}
          <div id="launch-directory-list" class="directory-list" aria-label="Subdirectories">
            {#if !connection?.capabilities.includes('directory_browser')}
              <p>Update and restart this computer’s relay to browse directories.</p>
            {:else if connection.directoryLoading}
              <p>Loading folders…</p>
            {:else if connection.directoryError}
              <p role="alert">{connection.directoryError}</p>
            {:else}
              {#if connection?.directoryBrowser?.parent}
                <button type="button" onclick={() => loadDirectory(connection.directoryBrowser?.parent || '')}>↰ Parent folder</button>
              {/if}
              {#each connection?.directoryBrowser?.directories || [] as directory (directory.path)}
                <button type="button" onclick={() => loadDirectory(directory.path)}>📁 {directory.name}</button>
              {/each}
              {#if connection?.directoryBrowser && !connection.directoryBrowser.directories.length}
                <p>This folder has no subdirectories. It remains selected.</p>
              {/if}
            {/if}
          </div>
        {/if}
      </div>
      <p class="hint">The folder shown above is selected. Tap it to browse; use ↑ or Parent folder to go back.</p>

      <label for="launch-name">Name</label>
      <input id="launch-name" bind:value={name} required maxlength="32" pattern={'[a-z][a-z0-9_-]{0,31}'} title="Start with a lowercase letter; use lowercase letters, numbers, underscores, or dashes." placeholder="project-codex" autocomplete="off" />

      <label for="launch-prompt">Initial task <span class="optional">(optional)</span></label>
      <textarea id="launch-prompt" bind:value={prompt} maxlength="100000" placeholder="Describe the task to start…"></textarea>
      <p class="hint">Sent to the agent as its first prompt after it starts.</p>
      <Button type="submit" disabled={submitting || !relayId || !profileId || !cwd || !name}>Start Agent</Button>
      {#if status}<p class:error class="form-status" role="status">{status}</p>{/if}
    </form>
  </Card>
</main>
