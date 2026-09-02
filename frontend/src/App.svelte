<script lang="ts">
  import { onMount } from 'svelte';
  import { get } from 'svelte/store';
  import ActivityDetail from '$components/ActivityDetail.svelte';
  import ActivityView from '$components/ActivityView.svelte';
  import AgentList from '$components/AgentList.svelte';
  import AgentRail from '$components/AgentRail.svelte';
  import ConversationHistory from '$components/ConversationHistory.svelte';
  import LaunchView from '$components/LaunchView.svelte';
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
  import { activityForNotification } from '$lib/activity';
  import {
    agentContextLabel,
    agentNeedsInspection,
    agentNeedsResponse,
    agentStatusGroup,
    agentStatusTone,
    attentionKind,
    approvalPromptPreview,
    displayName,
    hostLabel,
  } from '$lib/agents';
  import { APP_VERSION } from '$lib/config';
  import { initializePreferences } from '$lib/preferences';
  import { initializeSpeech, stopSpeech } from '$lib/speech';
  import { initializePush, notificationsEnabled, pushOptedIn, showPageNotification } from '$lib/push';
  import { parsePushOpenTarget, RELAY_PROTOCOL_VERSION } from '$lib/protocol';
  import { targetRefForAgent, targetRefMatchesAgent } from '$lib/resource-id';
  import {
    closeCurrentView,
    currentView,
    initializeRouter,
    navigate,
    replaceView,
    routeNotificationUrl,
    viewUrl,
  } from '$lib/router';
  import { initializeDeviceSecurity, securityState } from '$lib/security';
  import { relayStore } from '$lib/store';
  import {
    appUpdateStatus,
    clearPendingRelayUpdate,
    initializeAppUpdates,
    pendingRelayUpdate,
    relayServesCurrentOrigin,
    reloadUpdatedSameOriginApp,
  } from '$lib/updates';
  import type { Agent, NotificationTarget } from '$lib/types';

  const relays = relayStore.relayConfigs;
  const connections = relayStore.connections;
  const agents = relayStore.agents;
  const workspaces = relayStore.workspaces;
  const activities = relayStore.activities;
  const frames = relayStore.terminalFrames;
  const responding = relayStore.responding;
  const appUpdates = appUpdateStatus;

  let manageOpen = $state(false);
  // Bound so the header's Find control can open the terminal's own find bar.
  let terminalView = $state<{ openFind: () => void } | null>(null);
  let jumpOpen = $state(false);
  let workspaceOpen = $state(false);
  let workspaceDisclosure = $state<Record<string, boolean>>({});
  let lastBlocked = new Set<string>();
  let previousView = '';
  let terminalUnavailable = $state(false);
  const typedPushOpens = new Set<string>();
  const typedPushTimeouts = new Map<string, ReturnType<typeof setTimeout>>();
  const automaticUpdateChecks = new Set<string>();
  const awaitedDeployments = new Set<string>();
  let visibilityRevision = $state(0);
  let viewedRelayId = '';
  let viewedTargetSignature = '';

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
  const conversationHistoryAvailable = $derived(Boolean(
    activeAgent?.conversation_history_available
    && activeConnection?.capabilities.includes('conversation_history'),
  ));
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
  const appUpdateAvailable = $derived(['reload-ready', 'deployment-required'].includes($appUpdates.state));
  const relayUpdateAvailable = $derived(
    [...$connections.values()].some((connection) => connection.update.state === 'available'),
  );
  const relayUpdateNeedsAttention = $derived(
    [...$connections.values()].some((connection) => connection.update.state === 'blocked'
      || (connection.status === 'connected' && !connection.capabilities.includes('self_update'))),
  );
  const updateAvailable = $derived(appUpdateAvailable || relayUpdateAvailable || relayUpdateNeedsAttention);
  const settingsLabel = $derived(appUpdateAvailable
    ? relayUpdateAvailable
      ? 'Settings, phone app and relay updates available'
      : relayUpdateNeedsAttention
        ? 'Settings, phone app update available and relay update needs attention'
        : 'Settings, phone app update available'
    : relayUpdateAvailable
      ? 'Settings, relay update available'
      : relayUpdateNeedsAttention
        ? 'Settings, relay update needs attention'
        : 'Settings');
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
      hollow: false,
      label: `${connected}/${$relays.length} relays connected${inventoryUnavailable ? `; ${inventoryUnavailable} agent inventory unavailable` : inventoryLoading ? `; ${inventoryLoading} agent inventory loading` : ''}`,
    };
    if (activeConnection?.status !== 'connected') return {
      tone: 'warning' as const,
      hollow: false,
      label: 'Relay reconnecting',
    };
    if (activeConnection.inventory.state !== 'ready') return {
      tone: 'warning' as const,
      hollow: false,
      label: activeConnection.inventory.state === 'error' ? 'Agent inventory unavailable' : 'Agent inventory loading',
    };
    const group = agentStatusGroup(activeAgent);
    return {
      tone: agentStatusTone(activeAgent),
      hollow: group === 'ready',
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
    if ($securityState.locked) stopSpeech();
  });

  $effect(() => {
    const view = $currentView.view;
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

  $effect(() => {
    const blocked = $agents.filter((agent) => agentNeedsResponse(agent) || agentNeedsInspection(agent));
    document.title = blocked.length ? `(${blocked.length}) 🐑 herdr` : '🐑 herdr';
    if (blocked.length && navigator.setAppBadge) void navigator.setAppBadge(blocked.length).catch(() => {});
    else if (navigator.clearAppBadge) void navigator.clearAppBadge().catch(() => {});
    const attentionKey = (agent: Agent) => `${agent.pane_id}:${agent.event_id || ''}:${attentionKind(agent)}`;
    const added = blocked.filter((agent) => !lastBlocked.has(attentionKey(agent)));
    if (added.length && navigator.vibrate) navigator.vibrate([120, 80, 120]);
    for (const agent of added) void notifyBlockedAgent(agent);
    lastBlocked = new Set(blocked.map(attentionKey));
  });

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
    // Legacy notifications never carry an immutable backend-verifiable action
    // reference. Open their current thread read-only; never execute the action.
    if (target.action) {
      if (!agent) return;
      clearNotificationFallback();
      relayStore.showToast('Open the app to review this request. No notification action was taken.');
      replaceView({ view: 'terminal', paneId: agent.pane_id, target: targetRefForAgent(agent) || undefined });
      return;
    }
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
    if ($currentView.view !== 'push' || $securityState.locked) return;
    const { eventRef, deviceId } = $currentView;
    const matchingRelays = $relays.filter((relay) =>
      relayStore.deviceCredential(relay.id)?.deviceId === deviceId,
    );
    if (matchingRelays.length !== 1) {
      replaceView({ view: 'push_unavailable' });
      return;
    }
    const relayId = matchingRelays[0].id;
    const connection = $connections.get(relayId);
    if (connection?.status !== 'connected' || connection.inventory.state !== 'ready') {
      if (!typedPushTimeouts.has(eventRef)) {
        typedPushTimeouts.set(eventRef, setTimeout(() => {
          typedPushTimeouts.delete(eventRef);
          const current = get(currentView);
          if (current.view === 'push' && current.eventRef === eventRef) {
            replaceView({ view: 'push_unavailable' });
          }
        }, 15_000));
      }
      return;
    }
    const timeout = typedPushTimeouts.get(eventRef);
    if (timeout) clearTimeout(timeout);
    typedPushTimeouts.delete(eventRef);
    if (!connection.capabilities.includes('typed_push')) {
      replaceView({ view: 'push_unavailable' });
      return;
    }
    if (typedPushOpens.has(eventRef)) return;
    typedPushOpens.add(eventRef);
    void resolveTypedPushOpen(eventRef, relayId)
      .finally(() => typedPushOpens.delete(eventRef));
  });


  $effect(() => {
    void visibilityRevision;
    const visible = document.visibilityState === 'visible' && document.hasFocus();
    const target = $currentView.view === 'terminal' && activeAgent && !$securityState.locked
      && activeAgent.server_session_id === 'primary'
      && activeAgent.terminal_id
      && Number.isSafeInteger(activeAgent.generation)
      ? targetRefForAgent(activeAgent)
      : null;
    const relayId = visible && target && activeAgent
      && $connections.get(activeAgent.relay_id)?.status === 'connected'
      ? activeAgent.relay_id
      : '';
    const signature = relayId && target
      ? `${relayId}:${target.pane_id}:${target.terminal_id}:${target.agent_session_id || ''}:${target.generation}`
      : '';
    if (signature === viewedTargetSignature) return;
    if (viewedRelayId) {
      relayStore.sendRaw(viewedRelayId, {
        type: 'push_viewed_pane',
        protocol: RELAY_PROTOCOL_VERSION,
        visible: false,
        unlocked: !$securityState.locked,
      });
    }
    viewedRelayId = relayId;
    viewedTargetSignature = signature;
    if (relayId && target) {
      relayStore.sendRaw(relayId, {
        type: 'push_viewed_pane',
        protocol: RELAY_PROTOCOL_VERSION,
        visible: true,
        unlocked: true,
        target,
      });
    }
  });
  $effect(() => {
    for (const [relayId, connection] of $connections) {
      if (connection.status !== 'connected' || !connection.capabilities.includes('self_update')) continue;
      const identity = `${relayId}:${connection.releaseVersion}:${connection.revision}:${APP_VERSION}`;
      if (automaticUpdateChecks.has(identity)) continue;
      automaticUpdateChecks.add(identity);
      void relayStore.checkRelayUpdate(relayId).catch(() => {
        automaticUpdateChecks.delete(identity);
      });
    }
  });

  $effect(() => {
    for (const [relayId, connection] of $connections) {
      if (connection.status !== 'connected') continue;
      const pending = pendingRelayUpdate(relayId);
      if (!pending || connection.releaseVersion !== pending.version) continue;
      const revision = connection.revision.replace(/-dirty$/, '');
      if (!revision || !pending.revision.startsWith(revision)) continue;
      clearPendingRelayUpdate(relayId);
      relayStore.showToast(`${connection.relay.label} updated to v${pending.version}.`);
      if (relayServesCurrentOrigin(connection.relay.url)) {
        void reloadUpdatedSameOriginApp(pending.version);
      }
    }
  });

  $effect(() => {
    for (const connection of $connections.values()) {
      const deployment = connection.appDeploy;
      if (
        connection.status !== 'connected'
        || deployment.state !== 'succeeded'
        || deployment.origin !== location.origin
        || !deployment.target_version
      ) continue;
      // A relay announces its last successful deployment forever; without this
      // guard every store emission would restart the two-minute wait for a
      // stale target the origin will never serve again.
      const identity = `${deployment.target_version}:${deployment.target_revision}`;
      if (awaitedDeployments.has(identity)) continue;
      awaitedDeployments.add(identity);
      void reloadUpdatedSameOriginApp(deployment.target_version);
    }
  });

  onMount(() => {
    initializePreferences();
    const releaseSpeech = initializeSpeech();
    initializePush();
    const stopUpdates = initializeAppUpdates();
    const stopSecurity = initializeDeviceSecurity();
    const stopRouter = initializeRouter();
    const setupLinkNavigation = () => {
      relayStore.importSetupLink(location, !$securityState.locked);
    };
    const serviceWorkerMessage = (event: MessageEvent) => {
      if (event.data?.type === 'herdr_notification_click' && event.data.url) {
        routeNotificationUrl(event.data.url);
      }
    };
    const visibilityChanged = () => { visibilityRevision += 1; };
    document.addEventListener('visibilitychange', visibilityChanged);
    window.addEventListener('focus', visibilityChanged);
    window.addEventListener('blur', visibilityChanged);
    window.addEventListener('hashchange', setupLinkNavigation);
    navigator.serviceWorker?.addEventListener('message', serviceWorkerMessage);
    return () => {
      stopRouter();
      releaseSpeech();
      stopSecurity();
      stopUpdates();
      window.removeEventListener('hashchange', setupLinkNavigation);
      navigator.serviceWorker?.removeEventListener('message', serviceWorkerMessage);
      document.removeEventListener('visibilitychange', visibilityChanged);
      window.removeEventListener('focus', visibilityChanged);
      window.removeEventListener('blur', visibilityChanged);
      for (const timeout of typedPushTimeouts.values()) clearTimeout(timeout);
      typedPushTimeouts.clear();
      relayStore.destroy();
    };
  });

  function openAgent(agent: Agent) {
    if (relayStore.deviceCredential(agent.relay_id)?.role !== 'reader') {
      void relayStore.acknowledgePane(agent);
    }
    navigate({ view: 'terminal', paneId: agent.pane_id, target: targetRefForAgent(agent) || undefined });
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

  async function resolveTypedPushOpen(eventRef: string, relayId: string): Promise<void> {
    try {
      const result = await relayStore.sendCommand(relayId, { type: 'push_open_ref', event_ref: eventRef });
      const target = parsePushOpenTarget(result.data?.target, relayId);
      if (!target) throw new Error('Invalid notification target');
      const findAgent = () => get(relayStore.agents).find(agent =>
        agent.relay_id === relayId && targetRefMatchesAgent(target, agent),
      );
      let agent = findAgent();
      const deadline = Date.now() + 5_000;
      while (!agent && Date.now() < deadline) {
        relayStore.requestAgents();
        await new Promise(resolve => setTimeout(resolve, 100));
        agent = findAgent();
      }
      const current = get(currentView);
      if (current.view !== 'push' || current.eventRef !== eventRef) return;
      if (!agent) {
        replaceView({ view: 'push_unavailable' });
        return;
      }
      replaceView({ view: 'terminal', paneId: agent.pane_id, target });
    } catch {
      const current = get(currentView);
      if (current.view === 'push' && current.eventRef === eventRef) {
        replaceView({ view: 'push_unavailable' });
      }
    }
  }


  async function notifyBlockedAgent(agent: Agent) {
    if (!notificationsEnabled()) return;
    if (document.visibilityState === 'visible' && document.hasFocus()) return;
    const connection = $connections.get(agent.relay_id);
    if (pushOptedIn() && connection && ['sent', 'subscribed'].includes(connection.pushStatus)) return;
    const kind = attentionKind(agent);
    const target = {
      host: String(agent.host || hostLabel(agent)),
      pane_id: agent.raw_pane_id,
      notification_id: String(agent.event_id || `herdr-${hostLabel(agent)}-${agent.raw_pane_id}`),
    };
    const open = { ...target, action: '', index: null, total: null } as NotificationTarget;
    const title = kind === 'approval'
      ? `${displayName(agent)} blocked`
      : kind === 'question'
        ? `${displayName(agent)} needs answers`
        : `${displayName(agent)} needs inspection`;
    const fallback = kind === 'approval'
      ? `${agent.agent || 'Agent'} needs approval`
      : kind === 'question'
        ? `${agent.agent || 'Agent'} needs an answer`
        : `${agent.agent || 'Agent'} needs inspection`;
    await showPageNotification(title, {
      body: approvalPromptPreview(agent) || fallback,
      tag: `herdr-${target.host}-${target.pane_id}`,
      renotify: true,
      icon: 'icons/icon-192.png',
      badge: 'icons/notification-badge.png',
      actions: [],
      data: {
        url: viewUrl({ view: 'notification', target: open }),
        action_urls: {},
      },
    });
  }
</script>

<div class="app-shell">
  <header class="app-header" class:home-header={$currentView.view === 'agents'}>
    {#if $currentView.view !== 'agents'}
      <Button variant="ghost" size="icon" aria-label="Back" onclick={closeCurrentView}>
        <svg class="back-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
          <path d="m15 18-6-6 6-6"></path>
        </svg>
      </Button>
    {/if}
    <span
      class={`status-dot status-${headerIndicator.tone}`}
      class:hollow={headerIndicator.hollow}
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
          onclick={() => { workspaceOpen = true; }}
        >
          <svg class="header-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
            <path d="M3 5.5h7l2 2h9v11H3z"></path>
          </svg>
        </Button>
        <Button variant="ghost" size="icon" aria-label="Manage agent" disabled={!activeAgent || activeReadOnly} onclick={() => { manageOpen = true; }}>•••</Button>
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
      {:else}
        <Button variant="ghost" size="icon" aria-label="Manage workspaces" title="Manage workspaces" onclick={() => toggle('workspaces')}>
          <svg class="header-symbol" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
            <path d="M3 5.5h7l2 2h9v11H3z"></path>
            <path d="M8 13h8M12 9v8"></path>
          </svg>
        </Button>
        <Button variant="ghost" size="icon" aria-label="Start agent" onclick={() => toggle('launch')}>＋</Button>
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
        {#if updateAvailable}<span class="nav-update-badge" aria-hidden="true"></span>{/if}
      </span>
    </nav>
  </header>

  {#if $currentView.view === 'settings'}
    <SettingsView {readOnlyRelayIds} />
  {:else if $currentView.view === 'workspaces'}
    <WorkspaceManager {readOnlyRelayIds} />
  {:else if $currentView.view === 'launch'}
    <LaunchView
      relayId={$currentView.relayId}
      workspaceId={$currentView.workspaceId}
      cwd={$currentView.cwd}
      {readOnlyRelayIds}
    />
  {:else if $currentView.view === 'activity'}
    <ActivityView />
  {:else if $currentView.view === 'activity_detail'}
    <ActivityDetail key={$currentView.key} />
  {:else if $currentView.view === 'history' && activeAgent}
    <!-- Keyed so a hash navigation straight to another pane's history remounts
         the view: the reply draft, transcript, and scroll pin are all per-pane
         state and must never carry over to a different agent. -->
    {#key activeAgent.pane_id}
      <ConversationHistory agent={activeAgent} readOnly={activeReadOnly} />
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
<WorkspaceInspector bind:open={workspaceOpen} agent={activeAgent} />
<LockScreen />
<Toast />
