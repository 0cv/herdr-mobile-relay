<script lang="ts">
  import { onMount } from 'svelte';
  import AgentLogo, { hasAgentLogo } from '$components/AgentLogo.svelte';
  import Button from '$components/ui/Button.svelte';
  import {
    agentLastActiveAt,
    agentStatusGroup,
    agentStatusTone,
    approvalButtonTone,
    approvalOptions,
    approvalPromptPreview,
    displayName,
    hostLabel,
    questionInteraction,
    sortedAgents,
    tabName,
  } from '$lib/agents';
  import { homeLayout } from '$lib/preferences';
  import { relayStore } from '$lib/store';
  import type { Agent, RelayConfig, RelayConnectionView, RelayWorkspace } from '$lib/types';
  import { informativePath, workspaceGroupTrees, workspaceGroups, workspaceIdentity, workspaceProvenance, workspaceStateTone, type WorkspaceGroup, type WorkspaceGroupTree, type WorkspaceTab } from '$lib/workspaces';

  let {
    agents,
    relays,
    connections = new Map(),
    workspaces = [],
    workspaceDisclosure = $bindable<Record<string, boolean>>({}),
    responding,
    onopen,
  }: {
    agents: Agent[];
    relays: RelayConfig[];
    workspaces?: RelayWorkspace[];
    connections?: Map<string, RelayConnectionView>;
    workspaceDisclosure?: Record<string, boolean>;
    responding: Set<string>;
    onopen: (agent: Agent) => void;
  } = $props();

  const unavailableRelays = $derived(relays.filter((relay) => {
    const connection = connections.get(relay.id);
    return connection?.status === 'connected' && connection.inventory.state === 'error';
  }));
  const startingRelays = $derived(relays.filter((relay) => {
    const connection = connections.get(relay.id);
    return connection?.status === 'connected' && connection.inventory.state === 'starting';
  }));
  const readyRelays = $derived(relays.filter((relay) => {
    const connection = connections.get(relay.id);
    return connection?.status === 'connected' && connection.inventory.state === 'ready';
  }));

  const statusDefinitions = [
    ['attention', 'Needs inspection', 'warning'],
    ['blocked', 'Needs input', 'danger'],
  ] as const;
  let relativeNow = $state(Date.now());
  let movingTab = $state('');
  interface TabSlot {
    id: string;
    top: number;
    height: number;
  }
  let tabDrag = $state<{
    workspaceKey: string;
    sourceTabId: string;
    pointerId: number;
    startY: number;
    deltaY: number;
    insertIdx: number;
    items: TabSlot[];
    gap: number;
  } | null>(null);
  // Optimistic arrangement applied between releasing a drag and the relay
  // confirming the new order, so tabs never snap back while Herdr catches up.
  let pendingTabOrder = $state<{ key: string; order: string[] } | null>(null);
  const backgroundAgents = $derived(agents.filter((agent) => {
    const group = agentStatusGroup(agent);
    return group !== 'blocked' && group !== 'attention';
  }));
  const workingAgents = $derived(backgroundAgents.filter((agent) => agentStatusGroup(agent) === 'working'));
  const doneAgents = $derived(backgroundAgents.filter((agent) => agentStatusGroup(agent) === 'done'));
  const mixedLayout = $derived($homeLayout === 'mixed');
  const doneWorkspaces = $derived(mixedLayout ? [] : workspaceGroupTrees(workspaceGroups(
    doneAgents,
    workspaceRecordsFor(doneAgents, false),
  )));
  const workingWorkspaces = $derived(mixedLayout ? [] : workspaceGroupTrees(workspaceGroups(
    workingAgents,
    workspaceRecordsFor(workingAgents, false),
  )));
  const idleAgents = $derived(backgroundAgents.filter((agent) => {
    const group = agentStatusGroup(agent);
    return group !== 'working' && group !== 'done';
  }));
  const idleWorkspaces = $derived(mixedLayout ? [] : workspaceGroupTrees(workspaceGroups(
    idleAgents,
    workspaceRecordsFor(idleAgents, true),
  )));
  const mixedWorkspaces = $derived(mixedLayout
    ? workspaceGroupTrees(workspaceGroups(backgroundAgents, workspaceRecordsFor(backgroundAgents, true)))
    : []);

  function workspaceRecordsFor(visible: Agent[], includeEmpty: boolean): RelayWorkspace[] {
    const visibleKeys = new Set(visible.map((agent) => workspaceIdentity(agent)));
    const occupiedKeys = new Set(agents.map((agent) => workspaceIdentity(agent)));
    // A linked worktree whose repository workspace is not open on the same
    // relay has no parent card to nest under (relayWorkspaceTrees renders it
    // top-level), so an empty one must stay visible in its own right.
    const orphanLinkedWorktree = (workspace: RelayWorkspace): boolean => {
      const worktree = workspace.worktree;
      if (worktree?.is_linked_worktree !== true) return false;
      if (!worktree.repo_key) return true;
      return !workspaces.some((candidate) => (
        candidate.relay_id === workspace.relay_id
        && candidate.worktree?.repo_key === worktree.repo_key
        && candidate.worktree.is_linked_worktree === false
      ));
    };
    const selected = new Set(workspaces.filter((workspace) => {
      const key = `${workspace.relay_id}\u0000${workspace.workspace_id}`;
      const unoccupiedTopLevel = includeEmpty
        && !occupiedKeys.has(key)
        && (workspace.worktree?.is_linked_worktree !== true || orphanLinkedWorktree(workspace));
      return visibleKeys.has(key) || unoccupiedTopLevel;
    }).map((workspace) => `${workspace.relay_id}\u0000${workspace.workspace_id}`));
    for (const workspace of workspaces) {
      const key = `${workspace.relay_id}\u0000${workspace.workspace_id}`;
      const worktree = workspace.worktree;
      if (!worktree?.repo_key) continue;
      if (worktree.is_linked_worktree && selected.has(key)) {
        const parent = workspaces.find((candidate) => (
          candidate.relay_id === workspace.relay_id
          && candidate.worktree?.repo_key === worktree.repo_key
          && candidate.worktree.is_linked_worktree === false
        ));
        if (parent) selected.add(`${parent.relay_id}\u0000${parent.workspace_id}`);
      } else if (!worktree.is_linked_worktree && selected.has(key)) {
        for (const child of workspaces) {
          const childKey = `${child.relay_id}\u0000${child.workspace_id}`;
          if (child.relay_id === workspace.relay_id
            && child.worktree?.repo_key === worktree.repo_key
            && child.worktree.is_linked_worktree
            && !occupiedKeys.has(childKey)) {
            selected.add(childKey);
          }
        }
      }
    }
    return workspaces.filter((workspace) => selected.has(`${workspace.relay_id}\u0000${workspace.workspace_id}`));
  }

  $effect(() => {
    if (!pendingTabOrder) return;
    const pending = pendingTabOrder;
    const workspace = [...doneWorkspaces, ...workingWorkspaces, ...idleWorkspaces, ...mixedWorkspaces]
      .flatMap((tree) => [tree.workspace, ...tree.children])
      .find((group) => group.key === pending.key);
    if (!workspace || workspace.tabs.map((tab) => tab.id).join('\u0000') === pending.order.join('\u0000')) {
      pendingTabOrder = null;
    }
  });

  function displayTabs(workspace: WorkspaceGroup): WorkspaceTab[] {
    if (pendingTabOrder?.key !== workspace.key) return workspace.tabs;
    const rank = new Map(pendingTabOrder.order.map((id, index) => [id, index]));
    return [...workspace.tabs].sort((left, right) =>
      (rank.get(left.id) ?? Number.MAX_SAFE_INTEGER) - (rank.get(right.id) ?? Number.MAX_SAFE_INTEGER));
  }

  function rememberWorkspaceDisclosure(key: string, event: Event) {
    const details = event.currentTarget;
    if (!(details instanceof HTMLDetailsElement)) return;
    workspaceDisclosure[key] = details.open;
  }

  function tabOrderingAvailable(workspace: WorkspaceGroup): boolean {
    const connection = connections.get(workspace.relayId);
    return Boolean(
      connection?.status === 'connected'
      && connection.inventory.state === 'ready'
      && connection.capabilities.includes('tab_reorder'),
    );
  }

  async function commitReorder(workspace: WorkspaceGroup, sourceTabId: string, insertIndex: number, order: string[]) {
    const agent = displayTabs(workspace).find((tab) => tab.id === sourceTabId)?.agents[0];
    if (!agent) return;
    pendingTabOrder = { key: workspace.key, order };
    movingTab = sourceTabId;
    try {
      await relayStore.reorderTab(agent, insertIndex);
      relayStore.showToast('Tab order updated on Herdr.');
    } catch (error) {
      pendingTabOrder = null;
      relayStore.showToast((error as Error).message, true);
    } finally {
      movingTab = '';
    }
  }

  const LONG_PRESS_MS = 550;
  const PRESS_SLOP_PX = 12;
  let suppressOpen = false;

  // Long-press a working agent card to lift its tab, drag to reorder, and
  // release to commit; a plain tap still opens the agent.
  function reorderPress(node: HTMLElement, params: { workspace?: WorkspaceGroup; tabId: string }) {
    let current = params;
    let timer = 0;
    let pointerId = -1;
    let startX = 0;
    let startY = 0;
    let dragging = false;

    function reset() {
      if (timer) {
        clearTimeout(timer);
        timer = 0;
      }
      pointerId = -1;
      dragging = false;
    }

    function onPointerDown(event: PointerEvent) {
      suppressOpen = false;
      const workspace = current.workspace;
      if (!workspace || !event.isPrimary || event.button !== 0 || movingTab) return;
      pointerId = event.pointerId;
      startX = event.clientX;
      startY = event.clientY;
      timer = window.setTimeout(() => {
        timer = 0;
        const items = measureTabSlots(node);
        if (items.length < 2) {
          reset();
          return;
        }
        dragging = true;
        suppressOpen = true;
        navigator.vibrate?.(12);
        node.setPointerCapture?.(pointerId);
        const sourceIdx = items.findIndex((item) => item.id === current.tabId);
        tabDrag = {
          workspaceKey: workspace.key,
          sourceTabId: current.tabId,
          pointerId,
          startY: startY + window.scrollY,
          deltaY: 0,
          insertIdx: Math.max(0, sourceIdx),
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
      if (current.workspace) trackTabDrag(event, current.workspace);
    }

    function onPointerUp(event: PointerEvent) {
      if (event.pointerId !== pointerId) return;
      if (dragging && current.workspace) finishTabDrag(event, current.workspace);
      reset();
    }

    function onPointerCancel(event: PointerEvent) {
      if (event.pointerId !== pointerId) return;
      if (tabDrag?.pointerId === event.pointerId) tabDrag = null;
      reset();
    }

    // Non-passive so an active drag can stop the browser from scrolling;
    // Svelte's own touchmove handlers are passive.
    function onTouchMove(event: TouchEvent) {
      if (dragging) event.preventDefault();
    }

    function onContextMenu(event: Event) {
      if (dragging || timer) event.preventDefault();
    }

    function onClick(event: MouseEvent) {
      if (!suppressOpen) return;
      suppressOpen = false;
      event.preventDefault();
      event.stopPropagation();
    }

    node.addEventListener('pointerdown', onPointerDown);
    node.addEventListener('pointermove', onPointerMove);
    node.addEventListener('pointerup', onPointerUp);
    node.addEventListener('pointercancel', onPointerCancel);
    node.addEventListener('touchmove', onTouchMove, { passive: false });
    node.addEventListener('contextmenu', onContextMenu);
    node.addEventListener('click', onClick, true);
    return {
      update(next: { workspace?: WorkspaceGroup; tabId: string }) {
        current = next;
      },
      destroy() {
        node.removeEventListener('pointerdown', onPointerDown);
        node.removeEventListener('pointermove', onPointerMove);
        node.removeEventListener('pointerup', onPointerUp);
        node.removeEventListener('pointercancel', onPointerCancel);
        node.removeEventListener('touchmove', onTouchMove);
        node.removeEventListener('contextmenu', onContextMenu);
        node.removeEventListener('click', onClick, true);
        reset();
      },
    };
  }

  // Slots are captured in document coordinates when the drag starts, so the
  // preview stays correct while the page auto-scrolls under the pointer.
  function measureTabSlots(node: HTMLElement): TabSlot[] {
    const container = node.closest('.workspace-tabs');
    if (!container) return [];
    return [...container.querySelectorAll<HTMLElement>('.workspace-tab')].map((section) => {
      const bounds = section.getBoundingClientRect();
      return { id: section.dataset.tabId || '', top: bounds.top + window.scrollY, height: bounds.height };
    });
  }

  function trackTabDrag(event: PointerEvent, workspace: WorkspaceGroup) {
    if (!tabDrag || tabDrag.pointerId !== event.pointerId || tabDrag.workspaceKey !== workspace.key) return;
    event.preventDefault();
    if (event.clientY < 72) window.scrollBy(0, -12);
    else if (event.clientY > window.innerHeight - 72) window.scrollBy(0, 12);
    const pointerY = event.clientY + window.scrollY;
    let insertIdx = 0;
    for (const item of tabDrag.items) {
      if (item.id === tabDrag.sourceTabId) continue;
      if (pointerY > item.top + item.height / 2) insertIdx += 1;
    }
    tabDrag = { ...tabDrag, deltaY: pointerY - tabDrag.startY, insertIdx };
  }

  // The dragged tab follows the pointer; every other tab translates by the
  // dragged tab's height to preview the drop slot.
  function tabShift(workspace: WorkspaceGroup, tabId: string): string {
    if (!tabDrag || tabDrag.workspaceKey !== workspace.key) return '';
    if (tabId === tabDrag.sourceTabId) return `translateY(${tabDrag.deltaY}px)`;
    const { items, sourceTabId, insertIdx, gap } = tabDrag;
    const sourceIdx = items.findIndex((item) => item.id === sourceTabId);
    const itemIdx = items.findIndex((item) => item.id === tabId);
    if (sourceIdx < 0 || itemIdx < 0) return '';
    const span = items[sourceIdx].height + gap;
    const othersIdx = itemIdx > sourceIdx ? itemIdx - 1 : itemIdx;
    const shift = (itemIdx > sourceIdx ? -span : 0) + (othersIdx >= insertIdx ? span : 0);
    return shift ? `translateY(${shift}px)` : '';
  }

  function finishTabDrag(event: PointerEvent, workspace: WorkspaceGroup) {
    if (!tabDrag || tabDrag.pointerId !== event.pointerId || tabDrag.workspaceKey !== workspace.key) return;
    const completed = tabDrag;
    tabDrag = null;
    const sourceIdx = completed.items.findIndex((item) => item.id === completed.sourceTabId);
    if (sourceIdx < 0 || completed.insertIdx === sourceIdx) return;
    const others = completed.items.filter((item) => item.id !== completed.sourceTabId);
    // Herdr's insert_index addresses the pre-move list and shifts the slot
    // down by one itself when the source sits before it.
    const insertIndex = completed.insertIdx >= others.length
      ? completed.items.length
      : completed.items.findIndex((item) => item.id === others[completed.insertIdx].id);
    const order = [
      ...others.slice(0, completed.insertIdx).map((item) => item.id),
      completed.sourceTabId,
      ...others.slice(completed.insertIdx).map((item) => item.id),
    ];
    void commitReorder(workspace, completed.sourceTabId, insertIndex, order);
  }


  function handleTabOrderKey(event: KeyboardEvent, workspace: WorkspaceGroup, tabId: string) {
    if (!event.altKey) return;
    const delta = event.key === 'ArrowUp' || event.key === 'ArrowLeft'
      ? -1
      : event.key === 'ArrowDown' || event.key === 'ArrowRight' ? 1 : 0;
    if (!delta || movingTab) return;
    const tabs = displayTabs(workspace);
    const index = tabs.findIndex((tab) => tab.id === tabId);
    if (index < 0 || !tabs[index + delta]) return;
    event.preventDefault();
    const insertIndex = delta > 0 ? index + 2 : index - 1;
    const order = tabs.map((tab) => tab.id);
    [order[index], order[index + delta]] = [order[index + delta], order[index]];
    void commitReorder(workspace, tabId, insertIndex, order);
  }

  async function respond(agent: Agent, index: number, total: number, option: string) {
    await relayStore.respond(agent, index, total, option);
  }

  function agentMeta(agent: Agent, compact: boolean): string {
    const tab = compact ? '' : tabName(agent);
    const labels = [
      tab && tab !== displayName(agent) ? tab : '',
      agent.session || '',
    ].filter(Boolean);
    if (agent.agent && !hasAgentLogo(agent.agent)) labels.push(agent.agent);
    return labels.join(' · ');
  }

  function relativeTimestamp(timestamp: number): string {
    if (!timestamp) return '';
    const seconds = Math.max(0, Math.floor((relativeNow - timestamp) / 1_000));
    if (seconds < 60) return 'now';
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return `${minutes}m`;
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return `${hours}h`;
    const days = Math.floor(hours / 24);
    if (days < 30) return `${days}d`;
    const months = Math.floor(days / 30);
    if (months < 12) return `${months}mo`;
    return `${Math.floor(months / 12)}y`;
  }

  function relativeAge(agent: Agent): string {
    return relativeTimestamp(agentLastActiveAt(agent));
  }

  onMount(() => {
    const timer = setInterval(() => { relativeNow = Date.now(); }, 60_000);
    return () => clearInterval(timer);
  });
</script>

{#snippet agentGrid(visible: Agent[], compact: boolean, reorderWorkspace?: WorkspaceGroup, reorderTabId?: string)}
  <div class:compact-agent-grid={compact} class="agent-grid">
    {#each visible as agent (agent.pane_id)}
      {@const interaction = questionInteraction(agent)}
      {@const options = approvalOptions(agent)}
      {@const group = agentStatusGroup(agent)}
      {@const tone = agentStatusTone(agent)}
      {@const blocked = group === 'blocked'}
      {@const needsInspection = group === 'attention'}
      {@const meta = agentMeta(agent, compact)}
      {@const age = relativeAge(agent)}
      {@const inventoryReady = !connections.has(agent.relay_id) || connections.get(agent.relay_id)?.inventory.state === 'ready'}
      <article class:blocked class:compact-agent-card={compact} class:stale={!inventoryReady} class="agent-card">
        <button
          class="agent-open"
          aria-label={`Open ${displayName(agent)} on ${hostLabel(agent)}`}
          disabled={!inventoryReady}
          title={!inventoryReady
            ? 'This cached agent is unavailable until Herdr inventory recovers.'
            : reorderWorkspace ? 'Hold to reorder this tab; Alt+arrow keys also work.' : undefined}
          aria-keyshortcuts={reorderWorkspace ? 'Alt+ArrowUp Alt+ArrowDown' : undefined}
          use:reorderPress={{ workspace: reorderWorkspace, tabId: reorderTabId ?? '' }}
          onkeydown={reorderWorkspace ? (event) => handleTabOrderKey(event, reorderWorkspace, reorderTabId ?? '') : undefined}
          onclick={() => onopen(agent)}
        >
          <span class="agent-identity">
            <AgentLogo agent={agent.agent} />
            <span class={`status-dot status-${tone}`} class:hollow={group === 'ready'} aria-hidden="true"></span>
          </span>
          <span class="agent-copy">
            <span class="agent-title-row">
              <span class="agent-project">{displayName(agent)} <span class="host-badge">@{hostLabel(agent)}</span></span>
              {#if compact && age}
                <time class="agent-age" datetime={new Date(agentLastActiveAt(agent)).toISOString()} title={new Date(agentLastActiveAt(agent)).toLocaleString()}>{age}</time>
              {/if}
            </span>
            {#if meta}<span class="agent-meta">{meta}</span>{/if}
            {#if blocked || needsInspection}
              <span class="prompt-preview">{interaction?.question || approvalPromptPreview(agent)}</span>
            {/if}
          </span>
        </button>
        {#if blocked && !responding.has(agent.pane_id)}
          <div class="agent-actions" aria-label={`Actions for ${displayName(agent)}`}>
            {#if interaction}
              <Button variant="trust" size="sm" onclick={() => onopen(agent)}>
                {interaction.kind === 'multi_select' ? 'Choose options' : 'Choose answer'} ({interaction.options.length})
              </Button>
            {:else}
              {#each options as option, index (`${index}:${option}`)}
                <Button
                  variant={approvalButtonTone(option, index, options.length) === 'deny' ? 'danger' : approvalButtonTone(option, index, options.length) === 'trust' ? 'trust' : 'default'}
                  size="sm"
                  disabled={!inventoryReady}
                  title={!inventoryReady ? 'Agent controls are unavailable until Herdr inventory recovers.' : undefined}
                  onclick={() => respond(agent, index, options.length, option)}
                >{option.length > 48 ? `${option.slice(0, 45)}...` : option}</Button>
              {/each}
            {/if}
          </div>
        {:else if blocked}
          <p class="responding" role="status">Waiting for agent…</p>
        {/if}
      </article>
    {/each}
  </div>
{/snippet}

{#snippet workspaceTabs(workspace: WorkspaceGroup)}
  <div class="workspace-tabs">
    {#each displayTabs(workspace) as tab (tab.id)}
      <section
        class:tab-dragging={tabDrag?.workspaceKey === workspace.key && tabDrag.sourceTabId === tab.id}
        class="workspace-tab"
        data-tab-id={tab.id}
        aria-label={`${tab.label} tab`}
        style:transform={tabShift(workspace, tab.id) || undefined}
      >
        <header class="workspace-tab-header">
          <h3>{tab.label}</h3>
        </header>
        {@render agentGrid(
          tab.agents,
          true,
          workspace.tabs.length > 1 && tabOrderingAvailable(workspace) ? workspace : undefined,
          tab.id,
        )}
      </section>
    {/each}
  </div>
  {#if !workspace.tabs.length}
    <p class="workspace-empty">No agents are running in this workspace.</p>
  {/if}
{/snippet}

{#snippet workspaceGrid(trees: WorkspaceGroupTree[], defaultOpen: boolean, kind: 'working' | 'done' | 'idle' | 'mixed')}
  <div class="workspace-grid">
    {#each trees as tree (tree.workspace.key)}
      {@const workspace = tree.workspace}
      {@const summary = tree.aggregate}
      {@const working = kind === 'working'}
      {@const done = kind === 'done'}
      {@const stateTone = kind === 'mixed' ? workspaceStateTone(summary) : ''}
      {@const provenance = workspaceProvenance(workspace)}
      {@const disclosureKey = `${working ? 'working' : done ? 'done' : 'workspace'}:${workspace.key}`}
      <!-- Mixed mirrors the state sections' opening rules: workspaces with a
           working or done session start expanded, idle-only cards collapsed. -->
      {@const openDefault = kind === 'mixed'
        ? defaultOpen || summary.workingCount > 0 || summary.doneCount > 0
        : defaultOpen}
      <details
        class:working-workspace-card={working}
        class:done-workspace-card={done}
        class="workspace-card"
        open={workspaceDisclosure[disclosureKey] ?? openDefault}
        ontoggle={(event) => rememberWorkspaceDisclosure(disclosureKey, event)}
      >
        <summary>
          {#if stateTone}
            <span
              class={`status-dot workspace-state-dot status-${stateTone}`}
              class:hollow={stateTone === 'muted'}
              role="img"
              aria-label={stateTone === 'success'
                ? 'Has a done session'
                : stateTone === 'warning' ? 'Has a working session' : 'All sessions idle'}
            ></span>
          {/if}
          <span class="workspace-card-copy">
            <strong>{workspace.label}</strong>
            <small>{[workspace.cwd, `@${workspace.host}`].filter(Boolean).join(' · ')}</small>
          </span>
          <span
            class="workspace-counts"
            aria-label={`${summary.tabCount} tabs, ${summary.paneCount} panes, and ${summary.agents.length} agents`}
          >
            {#if working}<em class="workspace-working-count">{summary.workingCount} working</em>{/if}
            {#if done}<em class="workspace-done-count">{summary.doneCount} done</em>{/if}
            <span>{summary.tabCount} {summary.tabCount === 1 ? 'tab' : 'tabs'}</span>
            {#if tree.children.length}<span>{tree.children.length} {tree.children.length === 1 ? 'worktree' : 'worktrees'}</span>{/if}
            {#if !working && !done}<span>{summary.agents.length} {summary.agents.length === 1 ? 'agent' : 'agents'}</span>{/if}
            {#if summary.lastActiveAt}
              <time
                datetime={new Date(summary.lastActiveAt).toISOString()}
                title={`Last agent activity: ${new Date(summary.lastActiveAt).toLocaleString()}`}
                aria-label={`Last agent activity ${new Date(summary.lastActiveAt).toLocaleString()}`}
              >{relativeTimestamp(summary.lastActiveAt)}</time>
            {/if}
          </span>
        </summary>
        {#if provenance}
          <p class="workspace-provenance">{provenance}</p>
        {/if}
        {@render workspaceTabs(workspace)}
        {#if tree.children.length}
          <div class="workspace-worktree-list" role="group" aria-label={`${workspace.label} worktrees`}>
            {#each tree.children as child (child.key)}
              {@const childPath = informativePath(child.cwd, child.label)}
              <section class="workspace-worktree-card" aria-label={`${child.label} worktree`}>
                <header>
                  <span>
                    <strong>{child.label}</strong>
                    {#if childPath}<small>{childPath}</small>{/if}
                  </span>
                  <span>{child.tabCount} {child.tabCount === 1 ? 'tab' : 'tabs'}</span>
                </header>
                {@render workspaceTabs(child)}
              </section>
            {/each}
          </div>
        {/if}
      </details>
    {/each}
  </div>
{/snippet}

<main class="agent-list" aria-label="Agents">
  {#each unavailableRelays as relay (relay.id)}
    {@const inventory = connections.get(relay.id)?.inventory}
    <section class="inventory-warning" role="status" aria-label={`${relay.label} agent inventory unavailable`}>
      <strong>{relay.label} is connected, but its Herdr agent inventory is unavailable.</strong>
      <span>{inventory?.message || 'Refresh after checking Herdr on that computer.'}</span>
      {#if inventory?.stale}<span>Previously reported agents are shown as stale.</span>{/if}
    </section>
  {/each}

  {#if !agents.length && !relays.length}
    <div class="empty-state">
      <span class="empty-icon" aria-hidden="true">🐑</span>
      <h2>Herdr Mobile Relay</h2>
      <p>Monitor and approve agents from your phone.</p>
      <ol>
        <li>Run a relay on each computer.</li>
        <li>Give each computer its own <code>wss://</code> URL.</li>
        <li>Open Settings and add each relay.</li>
      </ol>
    </div>
  {:else if !agents.length && startingRelays.length}
    <div class="empty-state" role="status">Loading agents…</div>
  {:else if !agents.length && readyRelays.length}
    <div class="empty-state" role="status">No chat agents are running.</div>
  {:else if !agents.length && !unavailableRelays.length}
    <div class="empty-state" role="status">Waiting for relays…</div>
  {/if}

  {#each statusDefinitions as [group, title, tone] (group)}
    {@const visible = sortedAgents(agents.filter((agent) => agentStatusGroup(agent) === group))}
    {#if visible.length}
      <section class="agent-section" aria-labelledby={`section-${group}`}>
        <h2 id={`section-${group}`} class="section-heading">
          <span class={`status-dot status-${tone}`}></span>{title}
          <span class="section-count" aria-hidden="true">{visible.length}</span>
        </h2>
        {@render agentGrid(visible, false)}
      </section>
    {/if}
  {/each}

  {#if doneWorkspaces.length}
    <section class="agent-section done-section" aria-labelledby="section-done">
      <h2 id="section-done" class="section-heading">
        <span class="status-dot status-success"></span>Done
        <span class="section-count" aria-hidden="true">{doneAgents.length}</span>
      </h2>
      {@render workspaceGrid(doneWorkspaces, true, 'done')}
    </section>
  {/if}

  {#if workingWorkspaces.length}
    <section class="agent-section working-section" aria-labelledby="section-working">
      <h2 id="section-working" class="section-heading">
        <span class="status-dot status-warning"></span>Working
        <span class="section-count" aria-hidden="true">{workingAgents.length}</span>
      </h2>
      {@render workspaceGrid(workingWorkspaces, true, 'working')}
    </section>
  {/if}

  {#if idleWorkspaces.length}
    <section class="agent-section workspace-section" aria-labelledby="workspace-section-title">
      <h2 id="workspace-section-title" class="section-heading">
        <span class="status-dot hollow"></span>Idle
        <span class="section-count" aria-hidden="true">{idleWorkspaces.length}</span>
      </h2>
      {@render workspaceGrid(idleWorkspaces, idleWorkspaces.length === 1, 'idle')}
    </section>
  {/if}

  {#if mixedWorkspaces.length}
    <!-- No visible heading: the per-card state dots already tell the story. -->
    <section class="agent-section workspace-section" aria-label="Workspaces">
      {@render workspaceGrid(mixedWorkspaces, mixedWorkspaces.length === 1, 'mixed')}
    </section>
  {/if}
</main>
