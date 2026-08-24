<script lang="ts">
  import AppDialog from '$components/ui/AppDialog.svelte';
  import Button from '$components/ui/Button.svelte';
  import Card from '$components/ui/Card.svelte';
  import { navigate } from '$lib/router';
  import { CommandError, relayStore } from '$lib/store';
  import { informativePath, relayWorkspaceTrees, workspaceProvenance, type RelayWorkspaceTree } from '$lib/workspaces';
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
  let createOpen = $state(false);
  let busy = $state(false);
  let status = $state('');
  let error = $state(false);
  let renamingId = $state('');
  let renameLabel = $state('');
  let worktreeWorkspaceId = $state('');
  let worktreeOpen = $state(false);
  let worktreeListing = $state<WorktreeListing | null>(null);
  let worktreeLoading = $state(false);
  let worktreeError = $state('');
  let branch = $state('');
  let base = $state('');
  let worktreeLabel = $state('');
  let confirmOpen = $state(false);
  let confirming = $state<{ kind: 'close' | 'remove'; workspace: RelayWorkspace; force: boolean } | null>(null);
  interface WorkspaceSlot {
    key: string;
    top: number;
    height: number;
  }
  let movingWorkspace = $state('');
  let workspaceDrag = $state<{
    sourceKey: string;
    pointerId: number;
    startY: number;
    deltaY: number;
    insertIdx: number;
    items: WorkspaceSlot[];
    gap: number;
  } | null>(null);
  let pendingWorkspaceOrder = $state<{ relayId: string; order: string[] } | null>(null);
  let directoryLoadGeneration = 0;
  let worktreeLoadGeneration = 0;

  const readyRelays = $derived($relays.filter((relay) => {
    const connection = $connections.get(relay.id);
    return connection?.status === 'connected'
      && connection.inventory.state === 'ready'
      && connection.capabilities.includes('workspace_management');
  }));
  const connection = $derived($connections.get(relayId));
  const worktreeManagementAvailable = $derived(connection?.capabilities.includes('worktree_management') ?? false);
  const relayWorkspaces = $derived(
    $workspaces.filter((workspace) => workspace.relay_id === relayId),
  );
  const workspaceTrees = $derived(relayWorkspaceTrees(relayWorkspaces));
  const displayWorkspaceTrees = $derived.by(() => {
    if (pendingWorkspaceOrder?.relayId !== relayId) return workspaceTrees;
    const rank = new Map(pendingWorkspaceOrder.order.map((id, index) => [id, index]));
    return [...workspaceTrees].sort((left, right) =>
      (rank.get(left.workspace.workspace_id) ?? Number.MAX_SAFE_INTEGER)
      - (rank.get(right.workspace.workspace_id) ?? Number.MAX_SAFE_INTEGER));
  });
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
    renamingId = '';
    renameLabel = '';
    createOpen = false;
    worktreeWorkspaceId = '';
    worktreeOpen = false;
    worktreeListing = null;
    void loadDirectory('');
  });

  $effect(() => {
    if (worktreeWorkspaceId && !relayWorkspaces.some((workspace) => workspace.workspace_id === worktreeWorkspaceId)) {
      worktreeWorkspaceId = '';
      worktreeOpen = false;
      worktreeListing = null;
    }
  });

  $effect(() => {
    const pending = pendingWorkspaceOrder;
    if (!pending || pending.relayId !== relayId) return;
    const current = workspaceTrees.map((tree) => tree.workspace.workspace_id);
    if (current.join('\u0000') === pending.order.join('\u0000')) {
      pendingWorkspaceOrder = null;
      return;
    }
    // An authoritative snapshot whose membership differs from the optimistic
    // array invalidates the optimism outright; only a pure reorder of the
    // same set keeps waiting for the relay's confirmation.
    const optimistic = new Set(pending.order);
    if (current.length !== pending.order.length || current.some((id) => !optimistic.has(id))) {
      pendingWorkspaceOrder = null;
    }
  });

  function pathBase(path: string): string {
    return path.replace(/[\\/]+$/, '').split(/[\\/]/).filter(Boolean).at(-1) || 'workspace';
  }

  async function loadDirectory(path: string) {
    const loadRelayId = relayId;
    const generation = ++directoryLoadGeneration;
    if (!loadRelayId || !connection?.capabilities.includes('directory_browser')) return;
    try {
      const listing = await relayStore.listDirectories(loadRelayId, path);
      if (generation !== directoryLoadGeneration || relayId !== loadRelayId) return;
      if (listing.current.path) {
        cwd = listing.current.path;
        if (!label) label = pathBase(cwd);
      }
    } catch (caught) {
      if (generation !== directoryLoadGeneration || relayId !== loadRelayId) return;
      setStatus((caught as Error).message, true);
    }
  }

  function setStatus(message: string, failed = false) {
    status = message;
    error = failed;
    if (message) relayStore.showToast(message, failed);
  }

  /**
   * True when the relay may have applied the mutation even though no
   * confirmation came back (the store's `dispatched_unknown` phase).
   * Retrying such a create blindly can duplicate the workspace or worktree,
   * so callers steer the user to check the current list first.
   */
  function ambiguousOutcome(caught: unknown): boolean {
    return caught instanceof CommandError && caught.data?.dispatched_unknown === true;
  }

  function openCreateWorkspace() {
    label ||= pathBase(cwd);
    directoryOpen = false;
    createOpen = true;
  }

  function closeCreateWorkspace() {
    createOpen = false;
    directoryOpen = false;
  }

  async function createWorkspace(event: SubmitEvent) {
    event.preventDefault();
    if (!relayId || !cwd || !label.trim()) return;
    busy = true;
    try {
      await relayStore.createWorkspace(relayId, cwd, label.trim());
      setStatus(`Created workspace ${label.trim()}.`);
      label = '';
      closeCreateWorkspace();
    } catch (caught) {
      if (ambiguousOutcome(caught)) {
        // Leaving the dialog primed with the same values invites a blind
        // retry that can create the workspace twice.
        closeCreateWorkspace();
        setStatus(`${(caught as Error).message} Check the workspace list before retrying.`, true);
      } else {
        setStatus((caught as Error).message, true);
      }
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

  const LONG_PRESS_MS = 550;
  const PRESS_SLOP_PX = 12;

  function workspaceReorderPress(node: HTMLElement, params: { tree: RelayWorkspaceTree }) {
    let current = params;
    let timer = 0;
    let pointerId = -1;
    let startX = 0;
    let startY = 0;
    let dragging = false;

    function reset() {
      if (timer) window.clearTimeout(timer);
      timer = 0;
      pointerId = -1;
      dragging = false;
    }

    function onPointerDown(event: PointerEvent) {
      if (!event.isPrimary || event.button !== 0 || movingWorkspace) return;
      if (event.target instanceof Element && event.target.closest('button, input, select, textarea, a') && !event.target.closest('.workspace-drag-handle')) return;
      pointerId = event.pointerId;
      startX = event.clientX;
      startY = event.clientY;
      timer = window.setTimeout(() => {
        timer = 0;
        const items = measureWorkspaceSlots(node);
        if (items.length < 2) {
          reset();
          return;
        }
        const sourceIdx = items.findIndex((item) => item.key === current.tree.workspace.workspace_id);
        if (sourceIdx < 0) {
          reset();
          return;
        }
        dragging = true;
        navigator.vibrate?.(12);
        node.setPointerCapture?.(pointerId);
        workspaceDrag = {
          sourceKey: current.tree.workspace.workspace_id,
          pointerId,
          startY: startY + window.scrollY,
          deltaY: 0,
          insertIdx: sourceIdx,
          items,
          gap: items.length > 1 ? Math.max(0, items[1].top - items[0].top - items[0].height) : 12,
        };
      }, LONG_PRESS_MS);
    }

    function onPointerMove(event: PointerEvent) {
      if (event.pointerId !== pointerId) return;
      if (!dragging) {
        if (Math.hypot(event.clientX - startX, event.clientY - startY) > PRESS_SLOP_PX) reset();
        return;
      }
      event.preventDefault();
      trackWorkspaceDrag(event);
    }

    function onPointerUp(event: PointerEvent) {
      if (event.pointerId !== pointerId) return;
      if (dragging) finishWorkspaceDrag(event, current.tree);
      reset();
    }

    function onPointerCancel(event: PointerEvent) {
      if (event.pointerId !== pointerId) return;
      if (workspaceDrag?.pointerId === event.pointerId) workspaceDrag = null;
      reset();
    }

    function onTouchMove(event: TouchEvent) {
      if (dragging) event.preventDefault();
    }

    function onContextMenu(event: Event) {
      if (dragging || timer) event.preventDefault();
    }

    node.addEventListener('pointerdown', onPointerDown);
    node.addEventListener('pointermove', onPointerMove);
    node.addEventListener('pointerup', onPointerUp);
    node.addEventListener('pointercancel', onPointerCancel);
    node.addEventListener('touchmove', onTouchMove, { passive: false });
    node.addEventListener('contextmenu', onContextMenu);
    return {
      update(next: { tree: RelayWorkspaceTree }) {
        current = next;
      },
      destroy() {
        node.removeEventListener('pointerdown', onPointerDown);
        node.removeEventListener('pointermove', onPointerMove);
        node.removeEventListener('pointerup', onPointerUp);
        node.removeEventListener('pointercancel', onPointerCancel);
        node.removeEventListener('touchmove', onTouchMove);
        node.removeEventListener('contextmenu', onContextMenu);
        reset();
      },
    };
  }

  function measureWorkspaceSlots(node: HTMLElement): WorkspaceSlot[] {
    const container = node.closest('.workspace-management-list');
    if (!container) return [];
    return [...container.querySelectorAll<HTMLElement>('.workspace-management-slot')].map((slot) => {
      const bounds = slot.getBoundingClientRect();
      return {
        key: slot.dataset.workspaceKey || '',
        top: bounds.top + window.scrollY,
        height: bounds.height,
      };
    });
  }

  function trackWorkspaceDrag(event: PointerEvent) {
    if (!workspaceDrag || workspaceDrag.pointerId !== event.pointerId) return;
    if (event.clientY < 72) window.scrollBy(0, -12);
    else if (event.clientY > window.innerHeight - 72) window.scrollBy(0, 12);
    const pointerY = event.clientY + window.scrollY;
    let insertIdx = 0;
    for (const item of workspaceDrag.items) {
      if (item.key === workspaceDrag.sourceKey) continue;
      if (pointerY > item.top + item.height / 2) insertIdx += 1;
    }
    workspaceDrag = { ...workspaceDrag, deltaY: pointerY - workspaceDrag.startY, insertIdx };
  }

  function workspaceShift(workspaceID: string): string {
    if (!workspaceDrag) return '';
    if (workspaceID === workspaceDrag.sourceKey) return `translateY(${workspaceDrag.deltaY}px)`;
    const { items, sourceKey, insertIdx, gap } = workspaceDrag;
    const sourceIdx = items.findIndex((item) => item.key === sourceKey);
    const itemIdx = items.findIndex((item) => item.key === workspaceID);
    if (sourceIdx < 0 || itemIdx < 0) return '';
    const span = items[sourceIdx].height + gap;
    const othersIdx = itemIdx > sourceIdx ? itemIdx - 1 : itemIdx;
    const shift = (itemIdx > sourceIdx ? -span : 0) + (othersIdx >= insertIdx ? span : 0);
    return shift ? `translateY(${shift}px)` : '';
  }

  function finishWorkspaceDrag(event: PointerEvent, tree: RelayWorkspaceTree) {
    if (!workspaceDrag || workspaceDrag.pointerId !== event.pointerId) return;
    const completed = workspaceDrag;
    workspaceDrag = null;
    const sourceIdx = completed.items.findIndex((item) => item.key === completed.sourceKey);
    if (sourceIdx < 0 || completed.insertIdx === sourceIdx) return;
    void commitWorkspaceReorder(tree, completed.insertIdx);
  }

  function handleWorkspaceOrderKey(event: KeyboardEvent, tree: RelayWorkspaceTree) {
    if (event.target !== event.currentTarget || !event.altKey || movingWorkspace) return;
    const delta = event.key === 'ArrowUp' || event.key === 'ArrowLeft'
      ? -1
      : event.key === 'ArrowDown' || event.key === 'ArrowRight' ? 1 : 0;
    if (!delta) return;
    const index = displayWorkspaceTrees.findIndex((item) => item.workspace.workspace_id === tree.workspace.workspace_id);
    if (index < 0 || !displayWorkspaceTrees[index + delta]) return;
    event.preventDefault();
    void commitWorkspaceReorder(tree, index + delta);
  }

  async function commitWorkspaceReorder(tree: RelayWorkspaceTree, insertIdx: number) {
    const sourceID = tree.workspace.workspace_id;
    const others = displayWorkspaceTrees.filter((item) => item.workspace.workspace_id !== sourceID);
    const beforeWorkspaceID = others[insertIdx]?.workspace.workspace_id || '';
    const order = [
      ...others.slice(0, insertIdx).map((item) => item.workspace.workspace_id),
      sourceID,
      ...others.slice(insertIdx).map((item) => item.workspace.workspace_id),
    ];
    pendingWorkspaceOrder = { relayId: tree.workspace.relay_id, order };
    movingWorkspace = sourceID;
    try {
      const legacyInsertIndex = beforeWorkspaceID
        ? relayWorkspaces.findIndex((workspace) => workspace.workspace_id === beforeWorkspaceID)
        : relayWorkspaces.length;
      await relayStore.reorderWorkspaceBlock(
        tree.workspace.relay_id,
        tree.workspaceIds,
        beforeWorkspaceID,
        legacyInsertIndex,
      );
      setStatus(`Moved ${tree.workspace.label}.`);
    } catch (caught) {
      pendingWorkspaceOrder = null;
      setStatus((caught as Error).message, true);
    } finally {
      movingWorkspace = '';
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
    const generation = ++worktreeLoadGeneration;
    worktreeWorkspaceId = workspace.workspace_id;
    worktreeOpen = true;
    worktreeListing = null;
    worktreeError = '';
    worktreeLoading = true;
    try {
      const listing = await relayStore.listWorktrees(workspace);
      // A slower listing for a previously shown workspace must not land
      // under the dialog's current title; its Open buttons would then send
      // paths from one repository with another one's workspace id.
      if (generation !== worktreeLoadGeneration) return;
      worktreeListing = listing;
    } catch (caught) {
      if (generation !== worktreeLoadGeneration) return;
      worktreeError = (caught as Error).message;
    } finally {
      if (generation === worktreeLoadGeneration) worktreeLoading = false;
    }
  }

  /**
   * Refreshes the worktree dialog after a mutation, but only when the user
   * still has it open: Escape-dismissing the dialog while the request was
   * busy must not reopen it. When dismissed, the stale listing is dropped so
   * the next explicit open fetches a fresh one.
   */
  async function refreshWorktrees(workspace: RelayWorkspace) {
    if (worktreeOpen && worktreeWorkspaceId === workspace.workspace_id) {
      await showWorktrees(workspace);
      return;
    }
    if (worktreeWorkspaceId === workspace.workspace_id) worktreeListing = null;
  }

  function closeWorktrees() {
    worktreeOpen = false;
    worktreeWorkspaceId = '';
    worktreeListing = null;
  }

  async function createWorktree(event: SubmitEvent) {
    event.preventDefault();
    const workspace = worktreeWorkspace;
    if (!workspace || !branch.trim()) return;
    busy = true;
    try {
      await relayStore.createWorktree(workspace, {
        branch: branch.trim(),
        base: base.trim(),
        label: worktreeLabel.trim(),
      });
      setStatus(`Created worktree ${branch.trim()}.`);
      branch = '';
      base = '';
      worktreeLabel = '';
      await refreshWorktrees(workspace);
    } catch (caught) {
      if (ambiguousOutcome(caught)) {
        // The worktree may exist despite the lost confirmation; refreshing
        // the listing shows the truth instead of inviting a duplicate.
        setStatus(`${(caught as Error).message} Check the worktree list before retrying.`, true);
        await refreshWorktrees(workspace);
      } else {
        setStatus((caught as Error).message, true);
      }
    } finally {
      busy = false;
    }
  }

  async function openWorktree(path: string, labelValue: string) {
    const workspace = worktreeWorkspace;
    if (!workspace) return;
    busy = true;
    try {
      await relayStore.openWorktree(workspace, { path });
      setStatus(`Opened worktree ${labelValue}.`);
      await refreshWorktrees(workspace);
    } catch (caught) {
      if (ambiguousOutcome(caught)) {
        // Opening creates a workspace; a blind retry can open it twice.
        setStatus(`${(caught as Error).message} Check the workspace list before retrying.`, true);
        await refreshWorktrees(workspace);
      } else {
        setStatus((caught as Error).message, true);
      }
    } finally {
      busy = false;
    }
  }

  function agentCount(workspace: RelayWorkspace): number {
    return agentCounts.get(`${workspace.relay_id}\u0000${workspace.workspace_id}`) || 0;
  }
</script>

{#snippet workspacePanel(workspace: RelayWorkspace, nested = false, tree: RelayWorkspaceTree | null = null)}
  <article class:nested-workspace={nested} class="workspace-management-card">
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
      {@const checkoutPath = workspace.cwd || workspace.worktree?.checkout_path || ''}
      {@const shownPath = nested ? informativePath(checkoutPath, workspace.label) : checkoutPath || 'Working directory unavailable'}
      {@const provenance = workspaceProvenance(workspace, nested)}
      <header>
        <span>
          <strong>{workspace.label}</strong>
          {#if shownPath}<small>{shownPath}</small>{/if}
        </span>
        {#if tree}
          <button
            class="workspace-drag-handle"
            type="button"
            aria-label={`Reorder ${workspace.label}`}
            aria-keyshortcuts="Alt+ArrowUp Alt+ArrowDown"
            title="Hold and drag to reorder; Alt+arrow keys also work."
            onkeydown={(event) => handleWorkspaceOrderKey(event, tree)}
          >⠿</button>
        {/if}
      </header>
      <p class="workspace-management-meta">
        {workspace.tab_count} {workspace.tab_count === 1 ? 'tab' : 'tabs'} ·
        {workspace.pane_count} {workspace.pane_count === 1 ? 'pane' : 'panes'} ·
        {agentCount(workspace)} {agentCount(workspace) === 1 ? 'agent' : 'agents'}
      </p>
      {#if provenance}
        <p class="workspace-management-meta">{provenance}</p>
      {/if}
      <div class="workspace-management-actions">
        <Button size="sm" disabled={busy || !(workspace.cwd || workspace.worktree?.checkout_path)} onclick={() => startAgent(workspace)}>Start Agent</Button>
        <Button size="sm" variant="secondary" disabled={busy} onclick={() => beginRename(workspace)}>Rename</Button>
        {#if !workspace.worktree?.is_linked_worktree}
          <Button
            size="sm"
            variant="secondary"
            disabled={busy || !worktreeManagementAvailable}
            title={worktreeManagementAvailable ? undefined : 'This relay does not support worktree management'}
            onclick={() => showWorktrees(workspace)}
          >Worktrees</Button>
        {/if}
        <Button size="sm" variant="danger" disabled={busy} onclick={() => beginConfirm('close', workspace)}>Close</Button>
        {#if workspace.worktree?.is_linked_worktree}
          <Button
            size="sm"
            variant="danger"
            disabled={busy || !worktreeManagementAvailable}
            title={worktreeManagementAvailable ? undefined : 'This relay does not support worktree management'}
            onclick={() => beginConfirm('remove', workspace)}
          >Remove Worktree</Button>
        {/if}
      </div>
    {/if}
  </article>
{/snippet}

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
    <Button
      class="workspace-create-button"
      disabled={!relayId || busy}
      onclick={openCreateWorkspace}
    >Create Workspace</Button>
  </Card>

  <section class="workspace-management-list" aria-label="Herdr workspaces">
    {#if relayId && !relayWorkspaces.length}
      <p class="empty-state">No workspaces are open on this computer.</p>
    {/if}
    {#each displayWorkspaceTrees as tree (tree.workspace.workspace_id)}
      <div
        class:workspace-dragging={workspaceDrag?.sourceKey === tree.workspace.workspace_id}
        class="workspace-management-slot"
        data-workspace-key={tree.workspace.workspace_id}
        title="Hold to reorder this workspace."
        style:transform={workspaceShift(tree.workspace.workspace_id) || undefined}
        use:workspaceReorderPress={{ tree }}
      >
        <Card>
          {@render workspacePanel(tree.workspace, false, tree)}
          {#if tree.children.length}
            <div class="workspace-management-worktrees" aria-label={`${tree.workspace.label} linked worktrees`}>
              {#each tree.children as child (child.workspace_id)}
                {@render workspacePanel(child, true)}
              {/each}
            </div>
          {/if}
        </Card>
      </div>
    {/each}
  </section>

  {#if status}<p class:error class="form-status" role="status">{status}</p>{/if}

</main>

<AppDialog
  id="workspace-create-dialog"
  bind:open={createOpen}
  title="Create Workspace"
  description="Create an empty Herdr workspace with its initial tab. Start an agent later to add a second tab."
>
  <form class="form-stack" onsubmit={createWorkspace}>
    <span id="workspace-cwd-label" class="field-label">Working Directory</span>
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
    <div class="button-row">
      <Button variant="ghost" disabled={busy} onclick={closeCreateWorkspace}>Cancel</Button>
      <Button type="submit" disabled={busy || !relayId || !cwd || !label.trim()}>Confirm</Button>
    </div>
  </form>
</AppDialog>

<AppDialog
  id="worktree-manager-dialog"
  bind:open={worktreeOpen}
  title={worktreeWorkspace ? `${worktreeWorkspace.label} Worktrees` : 'Worktrees'}
  description={worktreeListing?.source.repo_root || worktreeWorkspace?.cwd || 'List, open, or create Git worktrees.'}
>
  {#if worktreeLoading}
    <p role="status">Loading worktrees…</p>
  {:else if worktreeError}
    <p class="error" role="alert">{worktreeError}</p>
  {:else if worktreeListing && worktreeWorkspace}
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
      <h4>Create Worktree</h4>
      <label for="worktree-branch">Branch</label>
      <input id="worktree-branch" bind:value={branch} maxlength="512" required autocomplete="off" placeholder="fix/issue-14" />
      <label for="worktree-base">Base ref <span class="optional">(optional, defaults to HEAD)</span></label>
      <input id="worktree-base" bind:value={base} maxlength="512" autocomplete="off" placeholder="main" />
      <label for="worktree-label">Workspace label <span class="optional">(optional)</span></label>
      <input id="worktree-label" bind:value={worktreeLabel} maxlength="128" autocomplete="off" />
      <div class="button-row">
        <Button variant="ghost" disabled={busy} onclick={closeWorktrees}>Cancel</Button>
        <Button type="submit" disabled={busy || !branch.trim()}>Confirm</Button>
      </div>
    </form>
  {/if}
</AppDialog>

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
