<script lang="ts">
  import AppDialog from '$components/ui/AppDialog.svelte';
  import Button from '$components/ui/Button.svelte';
  import Card from '$components/ui/Card.svelte';
  import { navigate } from '$lib/router';
  import { CommandError, relayStore } from '$lib/store';
  import type { RelayWorkspace, WorktreeListing } from '$lib/types';

  const relays = relayStore.relayConfigs;
  const connections = relayStore.connections;
  const workspaces = relayStore.workspaces;
  const agents = relayStore.agents;

  let relayId = $state('');
  let loadedRelay = '';
  let cwd = $state('');
  let label = $state('');
  let directoryOpen = $state(false);
  let busy = $state(false);
  let status = $state('');
  let error = $state(false);
  let renamingId = $state('');
  let renameLabel = $state('');
  let worktreeWorkspaceId = $state('');
  let worktreeListing = $state<WorktreeListing | null>(null);
  let worktreeLoading = $state(false);
  let worktreeError = $state('');
  let branch = $state('');
  let base = $state('');
  let worktreeLabel = $state('');
  let confirmOpen = $state(false);
  let confirming = $state<{ kind: 'close' | 'remove'; workspace: RelayWorkspace; force: boolean } | null>(null);

  const readyRelays = $derived($relays.filter((relay) => {
    const connection = $connections.get(relay.id);
    return connection?.status === 'connected'
      && connection.inventory.state === 'ready'
      && connection.capabilities.includes('workspace_management');
  }));
  const connection = $derived($connections.get(relayId));
  const relayWorkspaces = $derived(
    $workspaces
      .filter((workspace) => workspace.relay_id === relayId)
      .sort((left, right) => left.number - right.number || left.label.localeCompare(right.label)),
  );
  const worktreeWorkspace = $derived(
    relayWorkspaces.find((workspace) => workspace.workspace_id === worktreeWorkspaceId) || null,
  );
  const agentCounts = $derived.by(() => {
    const counts = new Map<string, number>();
    for (const agent of $agents) {
      const key = `${agent.relay_id}\u0000${agent.workspace_id || ''}`;
      counts.set(key, (counts.get(key) || 0) + 1);
    }
    return counts;
  });

  $effect(() => {
    if (!readyRelays.some((relay) => relay.id === relayId)) relayId = readyRelays[0]?.id || '';
    if (!relayId || loadedRelay === relayId) return;
    loadedRelay = relayId;
    cwd = '';
    label = '';
    worktreeWorkspaceId = '';
    worktreeListing = null;
    void loadDirectory('');
  });

  $effect(() => {
    if (worktreeWorkspaceId && !relayWorkspaces.some((workspace) => workspace.workspace_id === worktreeWorkspaceId)) {
      worktreeWorkspaceId = '';
      worktreeListing = null;
    }
  });

  function pathBase(path: string): string {
    return path.replace(/[\\/]+$/, '').split(/[\\/]/).filter(Boolean).at(-1) || 'workspace';
  }

  async function loadDirectory(path: string) {
    if (!relayId || !connection?.capabilities.includes('directory_browser')) return;
    try {
      const listing = await relayStore.listDirectories(relayId, path);
      if (listing.current.path) {
        cwd = listing.current.path;
        if (!label) label = pathBase(cwd);
      }
    } catch (caught) {
      setStatus((caught as Error).message, true);
    }
  }

  function setStatus(message: string, failed = false) {
    status = message;
    error = failed;
    if (message) relayStore.showToast(message, failed);
  }

  async function createWorkspace(event: SubmitEvent) {
    event.preventDefault();
    if (!relayId || !cwd || !label.trim()) return;
    busy = true;
    try {
      await relayStore.createWorkspace(relayId, cwd, label.trim());
      setStatus(`Created workspace ${label.trim()}.`);
      label = '';
    } catch (caught) {
      setStatus((caught as Error).message, true);
    } finally {
      busy = false;
    }
  }

  function beginRename(workspace: RelayWorkspace) {
    renamingId = workspace.workspace_id;
    renameLabel = workspace.label;
  }

  async function renameWorkspace(event: SubmitEvent, workspace: RelayWorkspace) {
    event.preventDefault();
    if (!renameLabel.trim()) return;
    busy = true;
    try {
      await relayStore.renameWorkspace(workspace, renameLabel.trim());
      setStatus(`Renamed workspace to ${renameLabel.trim()}.`);
      renamingId = '';
    } catch (caught) {
      setStatus((caught as Error).message, true);
    } finally {
      busy = false;
    }
  }

  async function moveWorkspace(workspace: RelayWorkspace, index: number, delta: number) {
    if (!relayWorkspaces[index + delta]) return;
    busy = true;
    const insertIndex = delta > 0 ? index + 2 : index - 1;
    try {
      await relayStore.reorderWorkspace(workspace, insertIndex);
      setStatus(`Moved ${workspace.label}.`);
    } catch (caught) {
      setStatus((caught as Error).message, true);
    } finally {
      busy = false;
    }
  }

  function beginConfirm(kind: 'close' | 'remove', workspace: RelayWorkspace, force = false) {
    confirming = { kind, workspace, force };
    confirmOpen = true;
  }

  function cancelConfirm() {
    confirmOpen = false;
    confirming = null;
  }

  async function confirmAction() {
    if (!confirming) return;
    const action = confirming;
    busy = true;
    try {
      if (action.kind === 'close') {
        await relayStore.closeWorkspace(action.workspace);
        setStatus(`Closed workspace ${action.workspace.label}.`);
      } else {
        await relayStore.removeWorktree(action.workspace, action.force);
        setStatus(`Removed worktree ${action.workspace.label}.`);
      }
      cancelConfirm();
    } catch (caught) {
      const commandError = caught as CommandError;
      if (action.kind === 'remove' && !action.force && commandError.data?.force_available === true) {
        confirming = { ...action, force: true };
        return;
      }
      setStatus(commandError.message, true);
      cancelConfirm();
    } finally {
      busy = false;
    }
  }

  function startAgent(workspace: RelayWorkspace) {
    navigate({
      view: 'launch',
      relayId: workspace.relay_id,
      workspaceId: workspace.workspace_id,
      cwd: workspace.cwd || workspace.worktree?.checkout_path || '',
    });
  }

  async function showWorktrees(workspace: RelayWorkspace) {
    worktreeWorkspaceId = workspace.workspace_id;
    worktreeListing = null;
    worktreeError = '';
    worktreeLoading = true;
    try {
      worktreeListing = await relayStore.listWorktrees(workspace);
    } catch (caught) {
      worktreeError = (caught as Error).message;
    } finally {
      worktreeLoading = false;
    }
  }

  async function createWorktree(event: SubmitEvent) {
    event.preventDefault();
    if (!worktreeWorkspace || !branch.trim()) return;
    busy = true;
    try {
      await relayStore.createWorktree(worktreeWorkspace, {
        branch: branch.trim(),
        base: base.trim(),
        label: worktreeLabel.trim(),
      });
      setStatus(`Created worktree ${branch.trim()}.`);
      branch = '';
      base = '';
      worktreeLabel = '';
      await showWorktrees(worktreeWorkspace);
    } catch (caught) {
      setStatus((caught as Error).message, true);
    } finally {
      busy = false;
    }
  }

  async function openWorktree(path: string, labelValue: string) {
    if (!worktreeWorkspace) return;
    busy = true;
    try {
      await relayStore.openWorktree(worktreeWorkspace, { path });
      setStatus(`Opened worktree ${labelValue}.`);
      await showWorktrees(worktreeWorkspace);
    } catch (caught) {
      setStatus((caught as Error).message, true);
    } finally {
      busy = false;
    }
  }

  function agentCount(workspace: RelayWorkspace): number {
    return agentCounts.get(`${workspace.relay_id}\u0000${workspace.workspace_id}`) || 0;
  }
</script>

<main class="page workspace-manager-page" aria-labelledby="workspace-manager-title">
  <div class="workspace-manager-heading">
    <div>
      <h2 id="workspace-manager-title">Workspaces</h2>
      <p>Manage Herdr workspaces and their Git worktrees without changing desktop focus.</p>
    </div>
  </div>

  <Card>
    <div class="form-stack">
      <label for="workspace-relay">Computer</label>
      <select id="workspace-relay" bind:value={relayId}>
        {#if !readyRelays.length}<option value="">No compatible relays</option>{/if}
        {#each readyRelays as relay (relay.id)}<option value={relay.id}>{relay.label}</option>{/each}
      </select>
    </div>
  </Card>

  <Card>
    <details class="workspace-create" open={!relayWorkspaces.length}>
      <summary>Create workspace</summary>
      <form class="form-stack" onsubmit={createWorkspace}>
        <span id="workspace-cwd-label" class="field-label">Working directory</span>
        <div class:open={directoryOpen} class="directory-browser" aria-labelledby="workspace-cwd-label">
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
              aria-controls="workspace-directory-list"
              onclick={() => { directoryOpen = !directoryOpen; }}
            >
              <span>{connection?.directoryBrowser?.current.label || cwd || (connection?.directoryLoading ? 'Loading…' : 'Unavailable')}</span>
              <span aria-hidden="true">⌄</span>
            </button>
          </div>
          {#if directoryOpen}
            <div id="workspace-directory-list" class="directory-list" aria-label="Subdirectories">
              {#if connection?.directoryLoading}
                <p>Loading folders…</p>
              {:else if connection?.directoryError}
                <p role="alert">{connection.directoryError}</p>
              {:else}
                {#if connection?.directoryBrowser?.parent}
                  <button type="button" onclick={() => loadDirectory(connection.directoryBrowser?.parent || '')}>↰ Parent folder</button>
                {/if}
                {#each connection?.directoryBrowser?.directories || [] as directory (directory.path)}
                  <button type="button" onclick={() => loadDirectory(directory.path)}>📁 {directory.name}</button>
                {/each}
              {/if}
            </div>
          {/if}
        </div>
        <label for="workspace-label">Label</label>
        <input id="workspace-label" bind:value={label} maxlength="128" required autocomplete="off" />
        <Button type="submit" disabled={busy || !relayId || !cwd || !label.trim()}>Create Workspace</Button>
      </form>
    </details>
  </Card>

  <section class="workspace-management-list" aria-label="Herdr workspaces">
    {#if relayId && !relayWorkspaces.length}
      <p class="empty-state">No workspaces are open on this computer.</p>
    {/if}
    {#each relayWorkspaces as workspace, index (workspace.workspace_id)}
      <Card>
        <article class="workspace-management-card">
          {#if renamingId === workspace.workspace_id}
            <form class="form-stack" onsubmit={(event) => renameWorkspace(event, workspace)}>
              <label for={`workspace-rename-${workspace.workspace_id}`}>Workspace label</label>
              <input id={`workspace-rename-${workspace.workspace_id}`} bind:value={renameLabel} maxlength="128" required autocomplete="off" />
              <div class="button-row">
                <Button type="submit" disabled={busy || !renameLabel.trim()}>Save</Button>
                <Button variant="ghost" disabled={busy} onclick={() => { renamingId = ''; }}>Cancel</Button>
              </div>
            </form>
          {:else}
            <header>
              <span>
                <strong>{workspace.label}</strong>
                <small>{workspace.cwd || workspace.worktree?.checkout_path || 'Working directory unavailable'}</small>
              </span>
              <span class="workspace-position">{index + 1}</span>
            </header>
            <p class="workspace-management-meta">
              {workspace.tab_count} {workspace.tab_count === 1 ? 'tab' : 'tabs'} ·
              {workspace.pane_count} {workspace.pane_count === 1 ? 'pane' : 'panes'} ·
              {agentCount(workspace)} {agentCount(workspace) === 1 ? 'agent' : 'agents'}
            </p>
            {#if workspace.worktree}
              <p class="workspace-management-meta">
                {workspace.worktree.is_linked_worktree ? 'Linked worktree' : 'Repository'} · {workspace.worktree.repo_name}
              </p>
            {/if}
            <div class="workspace-management-actions">
              <Button size="sm" disabled={busy || !(workspace.cwd || workspace.worktree?.checkout_path)} onclick={() => startAgent(workspace)}>Start Agent</Button>
              <Button size="sm" variant="secondary" disabled={busy} onclick={() => beginRename(workspace)}>Rename</Button>
              <Button size="sm" variant="secondary" disabled={busy || index === 0} aria-label={`Move ${workspace.label} up`} onclick={() => moveWorkspace(workspace, index, -1)}>Up</Button>
              <Button size="sm" variant="secondary" disabled={busy || index === relayWorkspaces.length - 1} aria-label={`Move ${workspace.label} down`} onclick={() => moveWorkspace(workspace, index, 1)}>Down</Button>
              <Button size="sm" variant="secondary" disabled={busy || !connection?.capabilities.includes('worktree_management')} onclick={() => showWorktrees(workspace)}>Worktrees</Button>
              <Button size="sm" variant="danger" disabled={busy} onclick={() => beginConfirm('close', workspace)}>Close</Button>
              {#if workspace.worktree?.is_linked_worktree}
                <Button size="sm" variant="danger" disabled={busy} onclick={() => beginConfirm('remove', workspace)}>Remove Worktree</Button>
              {/if}
            </div>
          {/if}
        </article>
      </Card>
    {/each}
  </section>

  {#if worktreeWorkspace}
    <Card>
      <section class="worktree-manager" aria-labelledby="worktree-manager-title">
        <header>
          <div>
            <h3 id="worktree-manager-title">{worktreeWorkspace.label} worktrees</h3>
            <p>{worktreeListing?.source.repo_root || worktreeWorkspace.cwd}</p>
          </div>
          <Button size="sm" variant="ghost" onclick={() => { worktreeWorkspaceId = ''; worktreeListing = null; }}>Close</Button>
        </header>
        {#if worktreeLoading}
          <p role="status">Loading worktrees…</p>
        {:else if worktreeError}
          <p class="error" role="alert">{worktreeError}</p>
        {:else if worktreeListing}
          <div class="worktree-list">
            {#each worktreeListing.worktrees as worktree (worktree.path)}
              <article>
                <span>
                  <strong>{worktree.branch || worktree.label}</strong>
                  <small>{worktree.path}</small>
                </span>
                {#if worktree.open_workspace_id}
                  <span class="worktree-state">Open</span>
                {:else if worktree.is_bare || worktree.is_prunable}
                  <span class="worktree-state">Unavailable</span>
                {:else}
                  <Button size="sm" variant="secondary" disabled={busy} onclick={() => openWorktree(worktree.path, worktree.branch || worktree.label)}>Open</Button>
                {/if}
              </article>
            {/each}
          </div>
          <form class="form-stack worktree-create-form" onsubmit={createWorktree}>
            <h4>Create worktree</h4>
            <label for="worktree-branch">Branch</label>
            <input id="worktree-branch" bind:value={branch} maxlength="512" required autocomplete="off" placeholder="fix/issue-14" />
            <label for="worktree-base">Base ref <span class="optional">(optional, defaults to HEAD)</span></label>
            <input id="worktree-base" bind:value={base} maxlength="512" autocomplete="off" placeholder="main" />
            <label for="worktree-label">Workspace label <span class="optional">(optional)</span></label>
            <input id="worktree-label" bind:value={worktreeLabel} maxlength="128" autocomplete="off" />
            <Button type="submit" disabled={busy || !branch.trim()}>Create Worktree</Button>
          </form>
        {/if}
      </section>
    </Card>
  {/if}

  {#if status}<p class:error class="form-status" role="status">{status}</p>{/if}
</main>

<AppDialog
  id="workspace-destructive-dialog"
  bind:open={confirmOpen}
  title={confirming?.kind === 'remove'
    ? confirming.force ? `Force remove ${confirming.workspace.label}?` : `Remove ${confirming.workspace.label}?`
    : `Close ${confirming?.workspace.label || 'workspace'}?`}
  description={confirming?.kind === 'remove'
    ? confirming.force
      ? 'The checkout has uncommitted changes. Force removal permanently discards those checkout changes; the Git branch is retained.'
      : 'This closes the Herdr workspace and removes its linked checkout. The Git branch is retained.'
    : 'Every pane in this workspace will close. Git checkouts are not removed.'}
>
  <div class="button-row">
    <Button variant="danger" disabled={busy} onclick={confirmAction}>
      {confirming?.kind === 'remove' ? confirming.force ? 'Force Remove' : 'Remove Worktree' : 'Close Workspace'}
    </Button>
    <Button variant="ghost" disabled={busy} onclick={cancelConfirm}>Cancel</Button>
  </div>
</AppDialog>
