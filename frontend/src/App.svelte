<script lang="ts">
  import { onMount } from 'svelte';
  import { get } from 'svelte/store';
  import ActivityDetail from '$components/ActivityDetail.svelte';
  import ActivityView from '$components/ActivityView.svelte';
  import AgentList from '$components/AgentList.svelte';
  import AgentRail from '$components/AgentRail.svelte';
  import GlobalJump from '$components/GlobalJump.svelte';
  import LockScreen from '$components/LockScreen.svelte';
  import ManageDialog from '$components/ManageDialog.svelte';
  import SettingsView from '$components/SettingsView.svelte';
  import TerminalView from '$components/TerminalView.svelte';
  import UpdateProgressDialog from '$components/UpdateProgressDialog.svelte';
  import WorkspaceInspector from '$components/WorkspaceInspector.svelte';
  import WorkspaceManager from '$components/WorkspaceManager.svelte';
  import Button from '$components/ui/Button.svelte';
  import Toast from '$components/ui/Toast.svelte';
  import { agentOpeningRoute } from '$lib/agent-navigation';
  import { hasConversationHistory } from '$lib/agent-view';
  import { activityForNotification } from '$lib/activity';
  import { observeAgentAttention } from '$lib/attention.svelte';
  import {
    agentContextLabel,
    agentStatusGroup,
    agentStatusTone,
    displayName,
    hostLabel,
  } from '$lib/agents';
  import { APP_ASSET_VERSION, APP_BUILD_ID } from '$lib/config';
  import { initializePreferences } from '$lib/preferences';
  import { initializeSpeech, stopSpeech } from '$lib/speech';
  import { initializePush } from '$lib/push';
  import { createPushOpenRouter } from '$lib/push-open';
  import { targetRefForAgent, targetRefMatchesAgent } from '$lib/resource-id';
  import {
    closeCurrentView,
    currentView,
    followInitialAgentSession,
    initializeRouter,
    navigate,
    replaceView,
    routeNotificationUrl,
  } from '$lib/router';
  import { initializeDeviceSecurity, securityState } from '$lib/security';
  import { relayStore } from '$lib/store';
  import {
    AppReloadHeld,
    appUpdateStatus,
    importOrReload,
    initializeAppUpdates,
  } from '$lib/updates';
  import { createRelayUpdateObserver, settingsUpdateLabel } from '$lib/update-reload';
  import { createViewedPaneReporter } from '$lib/viewed-pane';
  import type { Agent, ConversationPage, NotificationTarget } from '$lib/types';
  import type { ViewState } from '$lib/router';
  import type { WorkspaceFileTarget } from '$lib/workspace-inspection';

  const relays = relayStore.relayConfigs;
  const connections = relayStore.connections;
  const agents = relayStore.agents;
  const workspaces = relayStore.workspaces;
  const activities = relayStore.activities;
  const frames = relayStore.terminalFrames;
  const responding = relayStore.responding;

  let manageOpen = $state(false);
  // Bound so the header's Find control can open the terminal's own find bar.
  let terminalView = $state<{ openFind: () => void } | null>(null);
  let jumpOpen = $state(false);
  let workspaceOpen = $state(false);
  let workspaceFileTarget = $state.raw<WorkspaceFileTarget | null>(null);
  let workspaceDisclosure = $state<Record<string, boolean>>({});
  let previousView = '';
  let terminalUnavailable = $state(false);
  const pushOpenRouter = createPushOpenRouter();
  const reportViewedPane = createViewedPaneReporter();
  const observeRelayUpdates = createRelayUpdateObserver();
  let visibilityRevision = $state(0);
  let conversationHistoryComponent = $state<typeof import('$components/ConversationHistory.svelte')['default'] | null>(null);
  let conversationHistoryFailedRoute = $state.raw<ViewState | null>(null);
  let conversationHistoryFailure = $state('');
  let conversationHistoryLoading = false;

  const activeAgent = $derived.by(() => {
    const view = $currentView;
    if (view.view !== 'terminal' && view.view !== 'history') return null;
    const agent = $agents.find((candidate) => candidate.pane_id === view.paneId) || null;
    if (!agent || !view.target) return agent;
    return targetRefMatchesAgent(view.target, agent) ? agent : null;
  });
  const activeReadOnly = $derived(Boolean(
    activeAgent && relayStore.deviceCredential(activeAgent.relay_id)?.role === 'reader',
  ));
  const readOnlyRelayIds = $derived.by(() => {
    void $connections;
    return new Set($relays
      .filter((relay) => relayStore.deviceCredential(relay.id)?.role === 'reader')
      .map((relay) => relay.id));
  });
  const activeConnection = $derived(activeAgent ? $connections.get(activeAgent.relay_id) : null);
  const conversationHistoryAvailable = $derived(hasConversationHistory(activeAgent, activeConnection));
  $effect(() => {
    if ($currentView.view !== 'history' || !activeAgent || conversationHistoryComponent
      || conversationHistoryLoading || conversationHistoryFailedRoute === $currentView) return;
    conversationHistoryLoading = true;
    const stillOpen = () => get(currentView).view === 'history';
    void importOrReload(() => import('$components/ConversationHistory.svelte'), stillOpen)
      .then(({ default: component }) => {
        conversationHistoryComponent = component;
      })
      .catch((error) => {
        if (!stillOpen()) return;
        const route = get(currentView);
        if (route.view === 'history' && route.fallbackToTerminalOnInitialUnavailable) {
          replaceView({ view: 'terminal', paneId: route.paneId, target: route.target });
        } else {
          conversationHistoryFailure = error instanceof AppReloadHeld ? error.message : '';
          conversationHistoryFailedRoute = route;
        }
      })
      .finally(() => {
        conversationHistoryLoading = false;
      });
  });
  const workspaceInspectionAvailable = $derived(Boolean(
    activeAgent?.cwd
    && activeConnection?.capabilities.includes('workspace_inspection'),
  ));
  const connected = $derived([...$connections.values()].filter((connection) => connection.status === 'connected').length);
  const connecting = $derived([...$connections.values()].some((connection) => connection.status === 'connecting'));
  const inventoryUnavailable = $derived([...$connections.values()].filter(
    (connection) => connection.status === 'connected' && connection.inventory.state === 'error',
  ).length);
  const inventoryLoading = $derived([...$connections.values()].filter(
    (connection) => connection.status === 'connected' && connection.inventory.state === 'starting',
  ).length);
  const settingsLabel = $derived(settingsUpdateLabel($connections, $appUpdateStatus.state));
  const headerTitle = $derived.by(() => {
    if ($currentView.view === 'settings') return 'Settings';
    if ($currentView.view === 'push' || $currentView.view === 'push_unavailable') return 'Notification';
    if ($currentView.view === 'workspaces') return 'Workspaces';
    if ($currentView.view === 'launch') return 'Start Agent';
    if ($currentView.view === 'activity') return 'Activity';
    if ($currentView.view === 'activity_detail') return 'Activity';
    if (activeAgent) return activeAgent.project || displayName(activeAgent);
    if ($currentView.view === 'terminal') return 'Terminal';
    return '🐑 herdr';
  });
  const headerMeta = $derived(activeAgent ? terminalSecondaryLabel(activeAgent) : '');
  const headerIndicator = $derived.by(() => {
    if (!activeAgent) return {
      tone: inventoryUnavailable || inventoryLoading ? 'warning' : connected ? 'success' : connecting ? 'warning' : 'danger',
      label: `${connected}/${$relays.length} relays connected${inventoryUnavailable ? `; ${inventoryUnavailable} agent inventory unavailable` : inventoryLoading ? `; ${inventoryLoading} agent inventory loading` : ''}`,
    };
    if (activeConnection?.status !== 'connected') return {
      tone: 'warning' as const,
      label: 'Relay reconnecting',
    };
    if (activeConnection.inventory.state !== 'ready') return {
      tone: 'warning' as const,
      label: activeConnection.inventory.state === 'error' ? 'Agent inventory unavailable' : 'Agent inventory loading',
    };
    const group = agentStatusGroup(activeAgent);
    return {
      tone: agentStatusTone(activeAgent),
      label: `Agent ${group === 'ready'
        ? 'idle'
        : group === 'attention'
          ? 'needs inspection'
          : group === 'other'
            ? activeAgent.status || 'unknown'
            : group}`,
    };
  });

  $effect(() => {
    if (!$securityState.locked) return;
    workspaceOpen = false;
    stopSpeech();
  });

  $effect(() => {
    const view = $currentView.view;
    workspaceOpen = false;
    document.body.dataset.view = view;
    if (view === 'agents' && previousView && previousView !== 'agents') relayStore.requestAgents();
    previousView = view;
  });

  $effect(() => {
    const missingPaneId = $currentView.view === 'terminal' && !activeAgent ? $currentView.paneId : '';
    terminalUnavailable = false;
    if (!missingPaneId) return;
    relayStore.requestAgents();
    const timer = setTimeout(() => { terminalUnavailable = true; }, 5_000);
    return () => clearTimeout(timer);
  });

  observeAgentAttention(() => $agents, () => $connections);

  let notificationFallback: ReturnType<typeof setTimeout> | null = null;
  let notificationFallbackKey = '';
  function clearNotificationFallback() {
    if (notificationFallback) clearTimeout(notificationFallback);
    notificationFallback = null;
    notificationFallbackKey = '';
  }
  $effect(() => {
    if ($currentView.view !== 'notification') { clearNotificationFallback(); return; }
    const target = $currentView.target;
    const agent = resolveNotificationTarget(target, $agents);
    // A plain open shows the stored excerpt card. Activity history streams in on
    // connect, so re-run reactively until it arrives; if it never does (older
    // relay with no stored excerpt), fall back to the live thread.
    const activity = activityForNotification($activities, target.notification_id);
    if (activity) {
      clearNotificationFallback();
      replaceView({ view: 'activity_detail', key: activity.activity_key });
      return;
    }
    if (!agent) return;
    // Re-arm when the tapped notification changes so a rapid second tap can't
    // fall back to the first notification's thread.
    const key = target.notification_id || `${target.host}:${target.pane_id}`;
    if (notificationFallback && notificationFallbackKey !== key) clearNotificationFallback();
    if (!notificationFallback) {
      notificationFallbackKey = key;
      const paneId = agent.pane_id;
      const agentTarget = targetRefForAgent(agent) || undefined;
      notificationFallback = setTimeout(() => {
        notificationFallback = null;
        notificationFallbackKey = '';
        if (get(currentView).view === 'notification') replaceView({ view: 'terminal', paneId, target: agentTarget });
      }, 1500);
    }
  });

  $effect(() => {
    pushOpenRouter.route({
      view: $currentView,
      locked: $securityState.locked,
      relays: $relays,
      connections: $connections,
    });
  });

  $effect(() => {
    void visibilityRevision;
    reportViewedPane({
      view: $currentView,
      agent: activeAgent,
      locked: $securityState.locked,
      connections: $connections,
    });
  });
  $effect(() => { observeRelayUpdates($connections); });

  onMount(() => {
    initializePreferences();
    const releaseSpeech = initializeSpeech();
    initializePush();
    const stopUpdates = initializeAppUpdates();
    const stopSecurity = initializeDeviceSecurity();
    const stopRouter = initializeRouter();
    const stopAgentRoute = followInitialAgentSession(agents);
    const visibilityChanged = () => { visibilityRevision += 1; };
    const listeners = new AbortController();
    const options = { signal: listeners.signal };
    document.addEventListener('visibilitychange', visibilityChanged, options);
    window.addEventListener('focus', visibilityChanged, options);
    window.addEventListener('blur', visibilityChanged, options);
    window.addEventListener('hashchange', () => { relayStore.importSetupLink(location, !$securityState.locked); }, options);
    navigator.serviceWorker?.addEventListener('message', ({ data }) => {
      if (data?.type === 'herdr_notification_click' && data.url) routeNotificationUrl(data.url);
    }, options);
    return () => {
      stopAgentRoute();
      stopRouter();
      releaseSpeech();
      stopSecurity();
      stopUpdates();
      listeners.abort();
      pushOpenRouter.stop();
      relayStore.destroy();
    };
  });

  function openWorkspace(target: WorkspaceFileTarget | null = null) {
    workspaceFileTarget = target;
    workspaceOpen = true;
  }

  function activateFileLink(event: MouseEvent | KeyboardEvent) {
    if (event instanceof KeyboardEvent && event.key !== 'Enter' && event.key !== ' ') return;
    if (!activeAgent || !workspaceInspectionAvailable || workspaceOpen || $securityState.locked) return;
    if (!(event.target instanceof Element)) return;
    const link = event.target.closest<HTMLElement>('[data-file-path]');
    if (!link?.dataset.filePath) return;
    event.preventDefault();
    openWorkspace({
      path: link.dataset.filePath,
      line: Number(link.dataset.fileLine) || undefined,
      pane_id: activeAgent.pane_id,
      cwd: activeAgent.cwd,
    });
  }

  function openAgent(agent: Agent) {
    if (relayStore.deviceCredential(agent.relay_id)?.role !== 'reader') {
      void relayStore.acknowledgePane(agent);
    }
    navigate(agentOpeningRoute(agent));
  }

  function makeInitialPageHandler(openingRoute: Extract<ViewState, { view: 'history' }>): (page: ConversationPage) => void {
    return (page) => {
      if (!openingRoute.fallbackToTerminalOnInitialUnavailable || page.available) return;
      if (get(currentView) !== openingRoute) return;
      const liveAgent = get(agents).find((candidate) => candidate.pane_id === openingRoute.paneId);
      if (!liveAgent || !openingRoute.target) return;
      if (!targetRefMatchesAgent(openingRoute.target, liveAgent)) return;
      replaceView({ view: 'terminal', paneId: openingRoute.paneId, target: openingRoute.target });
    };
  }

  function toggle(view: 'settings' | 'launch' | 'activity' | 'workspaces') {
    if ($currentView.view === view) closeCurrentView();
    else navigate({ view });
  }

  function terminalSecondaryLabel(agent: Agent): string {
    const parts: string[] = [];
    const context = agentContextLabel(agent);
    const primary = agent.project || displayName(agent);
    if (context) parts.push(context);
    if (agent.agent && agent.agent !== primary && agent.agent !== context) parts.push(agent.agent);
    const host = hostLabel(agent);
    if (host) {
      if (parts.length) parts[parts.length - 1] = `${parts[parts.length - 1]} @${host}`;
      else parts.push(`@${host}`);
    }
    return parts.join(' · ');
  }

  function resolveNotificationTarget(target: NotificationTarget, allAgents: Agent[]): Agent | null {
    const matches = allAgents.filter((agent) => agent.raw_pane_id === target.pane_id);
    if (!matches.length) return null;
    const host = target.host.toLowerCase();
    if (host) {
      const exact = matches.find((agent) => [agent.host, hostLabel(agent), agent.relay_label]
        .some((value) => String(value || '').toLowerCase() === host));
      if (exact) return exact;
    }
    return matches.length === 1 ? matches[0] : null;
  }
</script>

<svelte:document onclick={activateFileLink} onkeydown={activateFileLink} />

<div class="app-shell">
  <header
    class="app-header"
    class:home-header={$currentView.view === 'agents'}
    data-app-assets={APP_ASSET_VERSION}
    data-app-build={APP_BUILD_ID}
  >
    {#if $currentView.view !== 'agents'}
      <Button variant="ghost" size="icon" aria-label="Back" onclick={closeCurrentView}>
        <svg class="back-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
          <path d="m15 18-6-6 6-6"></path>
        </svg>
      </Button>
    {/if}
    <span
      class={`status-dot status-${headerIndicator.tone}`}
      role="img"
      aria-label={headerIndicator.label}
    ></span>
    <div class="header-title">
      <h1>{headerTitle}</h1>
      {#if headerMeta}<span>{headerMeta}</span>{/if}
    </div>
    {#if $currentView.view === 'agents'}<span class="agent-count">{connected}/{$relays.length} relays{#if $agents.length} · {$agents.length}{/if}</span>{/if}
    <nav aria-label="Application">
      <Button class="global-jump-button" variant="ghost" size="icon" aria-label="Search all agents" title="Search all agents" onclick={() => { jumpOpen = true; }}>
        <svg class="header-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true" focusable="false">
          <circle cx="11" cy="11" r="6"></circle><path d="m16 16 4 4"></path>
        </svg>
      </Button>
      {#if $currentView.view === 'terminal'}
        <Button
          variant="ghost"
          size="icon"
          aria-label="Find in terminal"
          title="Find in terminal"
          disabled={!activeAgent}
          onclick={() => terminalView?.openFind()}
        >
          <svg class="header-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true" focusable="false">
            <circle cx="11" cy="11" r="6"></circle><path d="m16 16 4 4"></path>
          </svg>
        </Button>
        <Button
          variant="ghost"
          size="icon"
          aria-label="Conversation history"
          disabled={!conversationHistoryAvailable || !activeAgent}
          title={conversationHistoryAvailable ? 'Conversation history' : 'Conversation history is unavailable for this agent'}
          onclick={() => { if (activeAgent) replaceView({ view: 'history', paneId: activeAgent.pane_id, target: targetRefForAgent(activeAgent) || undefined }); }}
        >
          <svg class="header-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
            <path d="M4 5.5A2.5 2.5 0 0 1 6.5 3H20v15H6.5A2.5 2.5 0 0 0 4 20.5z"></path>
            <path d="M4 5.5v15M8 7h8M8 11h6"></path>
          </svg>
        </Button>
        <Button
          variant="ghost"
          size="icon"
          aria-label="Inspect workspace"
          disabled={!workspaceInspectionAvailable}
          title={workspaceInspectionAvailable ? 'Inspect workspace files and Git changes' : 'Workspace inspection is unavailable'}
          onclick={() => openWorkspace()}
        >
          <svg class="header-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
            <path d="M3 5.5h7l2 2h9v11H3z"></path>
          </svg>
        </Button>
        <Button variant="ghost" size="icon" aria-label="Manage agent" disabled={!activeAgent} onclick={() => { manageOpen = true; }}>•••</Button>
      {:else if $currentView.view === 'history'}
        <Button
          variant="ghost"
          size="icon"
          aria-label="Terminal view"
          title="Terminal view"
          disabled={!activeAgent}
          onclick={() => { if (activeAgent) replaceView({ view: 'terminal', paneId: activeAgent.pane_id, target: targetRefForAgent(activeAgent) || undefined }); }}
        >
          <svg class="header-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
            <rect x="3" y="4" width="18" height="16" rx="2"></rect>
            <path d="m7 9 3 3-3 3M12 15h5"></path>
          </svg>
        </Button>
        <Button
          variant="ghost"
          size="icon"
          aria-label="Inspect workspace"
          disabled={!workspaceInspectionAvailable}
          title={workspaceInspectionAvailable ? 'Inspect workspace files and Git changes' : 'Workspace inspection is unavailable'}
          onclick={() => openWorkspace()}
        >
          <svg class="header-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
            <path d="M3 5.5h7l2 2h9v11H3z"></path>
          </svg>
        </Button>
        <Button variant="ghost" size="icon" aria-label="Manage agent" disabled={!activeAgent} onclick={() => { manageOpen = true; }}>•••</Button>
      {:else}
        <Button variant="ghost" size="icon" aria-label="Manage workspaces" title="Manage workspaces" onclick={() => toggle('workspaces')}>
          <svg class="header-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
            <path d="M3 5.5h7l2 2h9v11H3z"></path>
            <path d="M8 13h8M12 9v8"></path>
          </svg>
        </Button>
        {#if [...$connections.values()].some((connection) => connection.capabilities.includes('agent_launch'))}
          <Button variant="ghost" size="icon" aria-label="Start agent" onclick={() => toggle('launch')}>＋</Button>
        {/if}
        <Button variant="ghost" size="icon" aria-label="Activity history" onclick={() => toggle('activity')}>
          <svg class="header-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
            <circle cx="12" cy="12" r="9"></circle>
            <path d="M12 7v5l3 2"></path>
          </svg>
        </Button>
      {/if}
      <span class="nav-button-shell">
        <Button
          variant="ghost"
          size="icon"
          aria-label={settingsLabel}
          onclick={() => toggle('settings')}
        >⚙</Button>
        {#if settingsLabel !== 'Settings'}<span class="nav-update-badge" aria-hidden="true"></span>{/if}
      </span>
    </nav>
  </header>

  {#if $currentView.view === 'settings'}
    <SettingsView {readOnlyRelayIds} />
  {:else if $currentView.view === 'workspaces'}
    <WorkspaceManager {readOnlyRelayIds} />
  {:else if $currentView.view === 'launch'}
    {#await importOrReload(() => import('$components/LaunchView.svelte'), () => get(currentView).view === 'launch')}
      <main class="page" aria-label="Opening launch form"><p role="status">Loading launch form…</p></main>
    {:then { default: LaunchView }}
      <LaunchView
        relayId={$currentView.relayId}
        workspaceId={$currentView.workspaceId}
        cwd={$currentView.cwd}
        {readOnlyRelayIds}
      />
    {:catch error}
      <main class="page"><p role="alert">{error instanceof AppReloadHeld ? error.message : 'The launch form could not be loaded. Reload the app to try again.'}</p></main>
    {/await}
  {:else if $currentView.view === 'activity'}
    <ActivityView />
  {:else if $currentView.view === 'activity_detail'}
    <ActivityDetail key={$currentView.key} />
  {:else if $currentView.view === 'history' && activeAgent}
    {@const openingRoute = $currentView}
    {#key openingRoute}
      {#if conversationHistoryComponent}
        {@const HistoryComponent = conversationHistoryComponent}
        <HistoryComponent
          agent={activeAgent}
          readOnly={activeReadOnly}
          frame={$frames.get(activeAgent.pane_id)}
          responding={$responding}
          onInitialPage={openingRoute.fallbackToTerminalOnInitialUnavailable
            ? makeInitialPageHandler(openingRoute)
            : undefined}
        />
      {:else if conversationHistoryFailedRoute === openingRoute}
        <main class="page terminal-loading" aria-label="Conversation history failed to load">
          <p role="alert">{conversationHistoryFailure || 'Conversation history could not be loaded. Reload the app to try again.'}</p>
        </main>
      {:else}
        <main class="page terminal-loading" aria-label="Opening conversation history">
          <p role="status">Opening conversation history…</p>
        </main>
      {/if}
    {/key}
  {:else if $currentView.view === 'history'}
    <main class="page terminal-loading" aria-label="Conversation history unavailable">
      <p role="alert">This agent is not available.</p>
      <Button onclick={() => replaceView({ view: 'agents' })}>Back to agents</Button>
    </main>
  {:else if $currentView.view === 'terminal' && activeAgent && activeConnection?.status === 'connected' && activeConnection.inventory.state !== 'ready'}
    <main class="page terminal-loading" aria-label="Agent inventory unavailable">
      <p role="alert">{activeConnection.inventory.message || 'This computer’s Herdr agent inventory is not ready.'}</p>
      <Button onclick={() => replaceView({ view: 'agents' })}>Back to agents</Button>
    </main>
  {:else if $currentView.view === 'terminal' && activeAgent}
    {#key activeAgent.pane_id}
      <div class="terminal-layout">
        <AgentRail agents={$agents} active={activeAgent} onopen={openAgent} onjump={() => { jumpOpen = true; }} />
        <TerminalView bind:this={terminalView} agent={activeAgent} allAgents={$agents} frame={$frames.get(activeAgent.pane_id)} responding={$responding} readOnly={activeReadOnly} />
      </div>
    {/key}
  {:else if $currentView.view === 'terminal'}
    <main class="page terminal-loading" aria-label={terminalUnavailable ? 'Agent unavailable' : 'Opening agent'}>
      {#if terminalUnavailable}
        <p role="alert">This agent is not available yet.</p>
        <Button onclick={() => replaceView({ view: 'agents' })}>Back to agents</Button>
      {:else}
        <p role="status">Opening agent…</p>
      {/if}
    </main>
  {:else if $currentView.view === 'push_unavailable'}
    <main class="page terminal-loading" aria-label="Notification unavailable">
      <p role="alert">This notification is stale, ambiguous, or no longer available. No action was taken.</p>
      <Button onclick={() => replaceView({ view: 'agents' })}>Back to agents</Button>
    </main>
  {:else if $currentView.view === 'push'}
    <main class="page terminal-loading" aria-label="Opening notification">
      <p role="status">Unlocking and validating the exact notification target…</p>
    </main>
  {:else}
    <AgentList bind:workspaceDisclosure agents={$agents} workspaces={$workspaces} relays={$relays} connections={$connections} responding={$responding} onopen={openAgent} />
  {/if}
</div>

<UpdateProgressDialog {readOnlyRelayIds} />
<ManageDialog bind:open={manageOpen} agent={activeAgent} readOnly={activeReadOnly} />
<GlobalJump bind:open={jumpOpen} agents={$agents} onselect={openAgent} />
<WorkspaceInspector bind:open={workspaceOpen} agent={activeAgent} fileTarget={workspaceFileTarget} />
<LockScreen />
<Toast />
