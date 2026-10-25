<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import Card from '$components/ui/Card.svelte';
  import { suggestedLaunchName } from '$lib/launch';
  import { replaceView } from '$lib/router';
  import { relayStore } from '$lib/store';

  let {
    relayId: requestedRelayId = '',
    workspaceId = '',
    cwd: requestedCwd = '',
    readOnlyRelayIds = new Set<string>(),
  }: {
    relayId?: string;
    workspaceId?: string;
    cwd?: string;
    readOnlyRelayIds?: Set<string>;
  } = $props();
  const relays = relayStore.relayConfigs;
  const connections = relayStore.connections;
  const workspaces = relayStore.workspaces;

  let relayId = $state('');
  let profileId = $state('');
  let hermesProfile = $state('');
  let cwd = $state('');
  let name = $state('');
  let prompt = $state('');
  let directoryOpen = $state(false);
  let status = $state('');
  let error = $state(false);
  let submitting = $state(false);
  let activeLaunch = $state.raw<{ relayId: string; controller: AbortController } | null>(null);
  let loadedRelay = '';
  // A deep link's target relay may connect after a faster sibling. Until the
  // reader picks a relay by hand, the requested one may still claim the form
  // when it becomes ready; without this the first-connected relay wins the
  // race and the link's workspace and directory are silently dropped.
  let requestedRelayPending = untrack(() => Boolean(requestedRelayId));
  let directoryLoadGeneration = 0;
  let directoryRelayId = $state('');
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
  // The relay refuses the home folder for agents; a terminal profile may start
  // there because it only opens a shell.
  // Only the listing of the selected folder counts: a failed reload keeps an older listing.
  const atHome = $derived(Boolean(
    profileId !== 'terminal'
    && connection?.directoryBrowser
    && connection.directoryBrowser.current.path === cwd
    && !connection.directoryBrowser.parent,
  ));
  const profiles = $derived(connection?.agentProfiles || []);
  // The builtin Hermes profile id is "hermes". The resolver never serializes a
  // kind for an agent profile, so the id is the only reliable signal that the
  // start command accepts hermes_profile at all.
  const isHermes = $derived(profileId === 'hermes');
  // Empty first option = "use Hermes sticky active profile" (no -p flag).
  // This preserves the behaviour of `hermes profile use <name>` on the computer.
  const hermesProfiles = $derived(
    connection?.hermesProfiles?.length
      ? ['', ...connection.hermesProfiles]
      : [''],
  );
  const targetWorkspace = $derived(
    $workspaces.find((workspace) => (
      workspace.relay_id === relayId && workspace.workspace_id === workspaceId
    )) || null,
  );
  const readOnly = $derived(readOnlyRelayIds.has(relayId));

  // Both the empty choice and the literal "default" id would read as "default",
  // but they start different agents: empty omits -p so Hermes follows its sticky
  // active_profile, while "default" pins the root hermes home. Label them apart
  // and leave the option values alone, so the payload stays a plain
  // "empty means omit" check.
  function hermesProfileLabel(name: string): string {
    if (name === '') return 'Use my usual setup';
    // Hermes lets the user give the root profile a display name, so the id
    // stays visible for anyone who knows it by `default`.
    if (name === 'default') return 'Main profile (default)';
    return name;
  }

  $effect(() => {
    if (requestedRelayPending && connectedRelays.some((relay) => relay.id === requestedRelayId)) {
      requestedRelayPending = false;
      relayId = requestedRelayId;
    }
    if (!connectedRelays.some((relay) => relay.id === relayId)) {
      relayId = connectedRelays.some((relay) => relay.id === requestedRelayId)
        ? requestedRelayId
        : connectedRelays[0]?.id || '';
    }
    if (!profiles.some((profile) => profile.id === profileId)) profileId = profiles[0]?.id || '';
    // A profile can disappear from the relay (renamed, or the connection moved
    // to another computer), so the picker falls back instead of submitting a
    // name the start validation would reject.
    if (!hermesProfiles.includes(hermesProfile)) hermesProfile = hermesProfiles[0];
    if (relayId && relayId !== loadedRelay) {
      loadedRelay = relayId;
      directoryRelayId = '';
      const initialPath = relayId === requestedRelayId ? requestedCwd : '';
      cwd = initialPath;
      if (initialPath) {
        directoryRelayId = relayId;
        name = suggestedLaunchName(initialPath, profileId);
      }
      void loadDirectory(initialPath);
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
      directoryOpen = false;
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

  function abandonLaunch() {
    const launch = activeLaunch;
    activeLaunch = null;
    submitting = false;
    launch?.controller.abort();
  }

  function viewAgents() {
    abandonLaunch();
    replaceView({ view: 'agents' });
  }

  $effect(() => {
    const launch = activeLaunch;
    if (!launch || (launch.relayId === relayId && connection?.status === 'connected' && !readOnly)) return;
    abandonLaunch();
    status = 'Launch confirmation stopped. Check Agents before starting again; the desktop launch may still finish.';
    error = true;
  });

  onDestroy(() => {
    ++directoryLoadGeneration;
    abandonLaunch();
  });

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (submitting || !relayId || readOnly || atHome || connection?.directoryLoading || directoryRelayId !== relayId || !profileId || !cwd || !name) return;
    const launch = { relayId, controller: new AbortController() };
    activeLaunch = launch;
    submitting = true;
    error = false;
    status = 'Starting agent…';
    try {
      const result = await relayStore.sendCommand(launch.relayId, {
        type: 'agent_start',
        profile_id: profileId,
        name: name.trim(),
        cwd: cwd.trim(),
        prompt,
        workspace_id: targetWorkspace?.workspace_id || '',
        ...(isHermes && hermesProfile !== '' ? { hermes_profile: hermesProfile } : {}),
      }, undefined, false, launch.controller.signal);
      if (activeLaunch !== launch) return;
      const warning = String(result.data?.warning || '');
      status = warning || 'Start confirmed. Refreshing agent list.';
      error = Boolean(warning);
      prompt = '';
      name = '';
      relayStore.showToast(status, error);
      relayStore.requestAgents();
      replaceView({ view: 'agents' });
    } catch (caught) {
      if (activeLaunch !== launch) return;
      status = `${(caught as Error).message}. Check Agents before starting again; a tab may already exist.`;
      error = true;
      relayStore.showToast(status, true);
    } finally {
      if (activeLaunch === launch) {
        activeLaunch = null;
        submitting = false;
      }
    }
  }
</script>

<main class="page launch-page" aria-labelledby="launch-title">
  <h2 id="launch-title">Start Agent</h2>
  <Card>
    <form class="form-stack" onfocusin={closeDirectoryForOtherField} onsubmit={submit}>
      <label for="launch-relay">Computer</label>
      <select id="launch-relay" bind:value={relayId} onchange={() => { requestedRelayPending = false; }} required>
        {#if !connectedRelays.length}<option value="">No ready relays</option>{/if}
        {#each connectedRelays as relay (relay.id)}<option value={relay.id}>{relay.label}</option>{/each}
      </select>
      {#if unavailableRelays.length}
        <p class="warning" role="status">Agent inventory is unavailable on {unavailableRelays.map((relay) => relay.label).join(', ')}.</p>
      {/if}
      {#if readOnly}<p class="warning" role="status">This paired device has read-only access to the selected relay.</p>{/if}
      {#if targetWorkspace}
        <p class="hint">New tab in workspace <strong>{targetWorkspace.label}</strong>. The desktop keeps its current focus.</p>
      {/if}

      <label for="launch-profile">Agent</label>
      <select id="launch-profile" bind:value={profileId} onchange={updateName} required>
        {#if !profiles.length}<option value="">No agent profiles available</option>{/if}
        {#each profiles as profile (profile.id)}<option value={profile.id}>{profile.label || profile.id}</option>{/each}
      </select>

      {#if isHermes}
        <label for="launch-hermes-profile">Hermes profile</label>
        <select id="launch-hermes-profile" bind:value={hermesProfile}>
          {#each hermesProfiles as p (p)}
            <option value={p}>{hermesProfileLabel(p)}</option>
          {/each}
        </select>
        <p class="hint">Leave it on “Use my usual setup” and Hermes starts the profile it uses on your desktop, as set by <code>hermes profile use</code>. Pick a profile only if this agent should run somewhere else.</p>
      {/if}

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
      {#if atHome}
        <p id="launch-cwd-hint" class="hint" role="status">Agents cannot start in the home folder itself. Tap the folder above to pick a project folder.</p>
      {:else}
        <p class="hint">The folder shown above is selected. Tap it to browse; use ↑ or Parent folder to go back.</p>
      {/if}

      <label for="launch-name">Name</label>
      <input id="launch-name" bind:value={name} required maxlength="32" pattern={'[a-z][a-z0-9_-]{0,31}'} title="Start with a lowercase letter; use lowercase letters, numbers, underscores, or dashes." placeholder="project-codex" autocomplete="off" />

      <label for="launch-prompt">Initial task <span class="optional">(optional)</span></label>
      <textarea id="launch-prompt" bind:value={prompt} maxlength="100000" placeholder="Describe the task to start…"></textarea>
      <p class="hint">Sent to the agent as its first prompt after it starts.</p>
      <Button type="submit" aria-describedby={atHome ? 'launch-cwd-hint' : undefined} disabled={submitting || readOnly || atHome || connection?.directoryLoading || directoryRelayId !== relayId || !relayId || !profileId || !cwd || !name}>Start Agent</Button>
      {#if status}
        <p class:error class="form-status" role="status">{status}</p>
        <Button type="button" variant="secondary" onclick={viewAgents}>View agents</Button>
      {/if}
    </form>
  </Card>
</main>
