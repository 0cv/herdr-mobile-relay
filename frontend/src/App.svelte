<script lang="ts">
  import { onMount } from 'svelte';
  import ActivityView from '$components/ActivityView.svelte';
  import AgentList from '$components/AgentList.svelte';
  import LaunchView from '$components/LaunchView.svelte';
  import LockScreen from '$components/LockScreen.svelte';
  import ManageDialog from '$components/ManageDialog.svelte';
  import SettingsView from '$components/SettingsView.svelte';
  import TerminalView from '$components/TerminalView.svelte';
  import Button from '$components/ui/Button.svelte';
  import Toast from '$components/ui/Toast.svelte';
  import {
    agentContextLabel,
    agentStatusGroup,
    agentStatusTone,
    approvalOptions,
    approvalPromptPreview,
    displayName,
    hostLabel,
  } from '$lib/agents';
  import { HANDLED_NOTIFICATION_ACTIONS_KEY } from '$lib/config';
  import { initializePreferences } from '$lib/preferences';
  import { initializePush, notificationsEnabled, pushOptedIn, showPageNotification } from '$lib/push';
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
    clearPendingAppDeploy,
    clearPendingRelayUpdate,
    initializeAppUpdates,
    pendingAppDeploy,
    pendingRelayUpdate,
    relayServesCurrentOrigin,
    reloadUpdatedSameOriginApp,
  } from '$lib/updates';
  import type { Agent, NotificationTarget } from '$lib/types';

  const relays = relayStore.relayConfigs;
  const connections = relayStore.connections;
  const agents = relayStore.agents;
  const frames = relayStore.terminalFrames;
  const responding = relayStore.responding;
  const appUpdates = appUpdateStatus;

  let manageOpen = $state(false);
  let lastBlocked = new Set<string>();
  let previousView = '';
  let terminalUnavailable = $state(false);
  const handlingNotifications = new Set<string>();

  const activeAgent = $derived($currentView.view === 'terminal'
    ? $agents.find((agent) => agent.pane_id === $currentView.paneId) || null
    : null);
  const activeConnection = $derived(activeAgent ? $connections.get(activeAgent.relay_id) : null);
  const connected = $derived([...$connections.values()].filter((connection) => connection.status === 'connected').length);
  const connecting = $derived([...$connections.values()].some((connection) => connection.status === 'connecting'));
  const updateAvailable = $derived(
    ['reload-ready', 'deployment-required'].includes($appUpdates.state)
      || [...$connections.values()].some((connection) => ['available', 'blocked'].includes(connection.update.state)),
  );
  const headerTitle = $derived.by(() => {
    if ($currentView.view === 'settings') return 'Settings';
    if ($currentView.view === 'launch') return 'Start Agent';
    if ($currentView.view === 'activity') return 'Activity';
    if (activeAgent) return activeAgent.project || displayName(activeAgent);
    if ($currentView.view === 'terminal') return 'Terminal';
    return '🐑 herdr';
  });
  const headerMeta = $derived(activeAgent ? terminalSecondaryLabel(activeAgent) : '');
  const headerIndicator = $derived.by(() => {
    if (!activeAgent) return {
      tone: connected ? 'success' : connecting ? 'warning' : 'danger',
      hollow: false,
      label: `${connected}/${$relays.length} relays connected`,
    };
    if (activeConnection?.status !== 'connected') return {
      tone: 'warning' as const,
      hollow: false,
      label: 'Relay reconnecting',
    };
    const group = agentStatusGroup(activeAgent);
    return {
      tone: agentStatusTone(activeAgent),
      hollow: group === 'ready',
      label: `Agent ${group === 'ready' ? 'idle' : group === 'other' ? activeAgent.status || 'unknown' : group}`,
    };
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
    const blocked = $agents.filter((agent) => agentStatusGroup(agent) === 'blocked');
    document.title = blocked.length ? `(${blocked.length}) 🐑 herdr` : '🐑 herdr';
    if (blocked.length && navigator.setAppBadge) void navigator.setAppBadge(blocked.length).catch(() => {});
    else if (navigator.clearAppBadge) void navigator.clearAppBadge().catch(() => {});
    const added = blocked.filter((agent) => !lastBlocked.has(agent.pane_id));
    if (added.length && navigator.vibrate) navigator.vibrate([120, 80, 120]);
    for (const agent of added) void notifyBlockedAgent(agent);
    lastBlocked = new Set(blocked.map((agent) => agent.pane_id));
  });

  $effect(() => {
    if ($currentView.view !== 'notification') return;
    const target = $currentView.target;
    const agent = resolveNotificationTarget(target, $agents);
    if (!agent) return;
    replaceView({ view: 'terminal', paneId: agent.pane_id });
    if (target.action) void executeNotificationAction(agent, target);
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
      const deployAfter = pendingAppDeploy(relayId);
      if (deployAfter) {
        clearPendingAppDeploy(relayId);
        if (connection.releaseVersion === deployAfter) {
          relayStore.showToast(`Deploying app v${deployAfter} from ${connection.relay.label}…`);
          void relayStore.deployAppUpdate(relayId, deployAfter).catch((error) => {
            relayStore.showToast((error as Error).message, true);
          });
        }
      } else if (relayServesCurrentOrigin(connection.relay.url)) {
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
      void reloadUpdatedSameOriginApp(deployment.target_version);
    }
  });

  onMount(() => {
    initializePreferences();
    initializePush();
    const stopUpdates = initializeAppUpdates();
    const stopSecurity = initializeDeviceSecurity();
    const stopRouter = initializeRouter();
    const setupLinkNavigation = () => {
      relayStore.importSetupLink(location, !$securityState.locked);
    };
    const serviceWorkerMessage = (event: MessageEvent) => {
      if (event.data?.type === 'herdr_notification_click' && event.data.url) routeNotificationUrl(event.data.url);
    };
    window.addEventListener('hashchange', setupLinkNavigation);
    navigator.serviceWorker?.addEventListener('message', serviceWorkerMessage);
    return () => {
      stopRouter();
      stopSecurity();
      stopUpdates();
      window.removeEventListener('hashchange', setupLinkNavigation);
      navigator.serviceWorker?.removeEventListener('message', serviceWorkerMessage);
      relayStore.destroy();
    };
  });

  function openAgent(agent: Agent) {
    void relayStore.acknowledgePane(agent);
    navigate({ view: 'terminal', paneId: agent.pane_id });
  }

  function toggle(view: 'settings' | 'launch' | 'activity') {
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

  function handledNotificationActions(): string[] {
    try {
      const parsed = JSON.parse(localStorage.getItem(HANDLED_NOTIFICATION_ACTIONS_KEY) || '[]');
      return Array.isArray(parsed) ? parsed.filter(Boolean).slice(-50) : [];
    } catch {
      return [];
    }
  }

  function notificationActionKey(target: NotificationTarget): string {
    return `${target.notification_id || `${target.host}:${target.pane_id}`}:${target.action}`;
  }

  function rememberNotificationAction(target: NotificationTarget) {
    const key = notificationActionKey(target);
    const handled = handledNotificationActions().filter((value) => value !== key);
    handled.push(key);
    localStorage.setItem(HANDLED_NOTIFICATION_ACTIONS_KEY, JSON.stringify(handled.slice(-50)));
  }

  async function executeNotificationAction(agent: Agent, target: NotificationTarget) {
    const key = notificationActionKey(target);
    if (handlingNotifications.has(key)) return;
    handlingNotifications.add(key);
    try {
      if (handledNotificationActions().includes(key)) {
        relayStore.showToast('This notification action was already handled.');
        return;
      }
      if (agentStatusGroup(agent) !== 'blocked') {
        rememberNotificationAction(target);
        relayStore.showToast('The agent is no longer blocked.');
        return;
      }
      rememberNotificationAction(target);
      const options = approvalOptions(agent);
      const index = target.index ?? 0;
      const total = target.total ?? Math.max(2, options.length);
      await relayStore.respond(agent, index, total, options[index] || 'approve once', `Notification: ${target.action}`);
    } finally {
      handlingNotifications.delete(key);
    }
  }

  async function notifyBlockedAgent(agent: Agent) {
    if (!notificationsEnabled()) return;
    if (document.visibilityState === 'visible' && document.hasFocus()) return;
    const connection = $connections.get(agent.relay_id);
    if (pushOptedIn() && connection && ['sent', 'subscribed'].includes(connection.pushStatus)) return;
    const options = approvalOptions(agent);
    const total = Math.max(2, options.length);
    const target = {
      host: String(agent.host || hostLabel(agent)),
      pane_id: agent.raw_pane_id,
      notification_id: String(agent.event_id || `herdr-${hostLabel(agent)}-${agent.raw_pane_id}`),
    };
    const approve = { ...target, action: 'approve', index: 0, total } as NotificationTarget;
    await showPageNotification(`${displayName(agent)} blocked`, {
      body: approvalPromptPreview(agent) || `${agent.agent || 'Agent'} needs approval`,
      tag: `herdr-${target.host}-${target.pane_id}`,
      renotify: true,
      icon: typeof HERDR_NOTIFICATION_ICON === 'string' ? HERDR_NOTIFICATION_ICON : undefined,
      badge: typeof HERDR_NOTIFICATION_BADGE === 'string' ? HERDR_NOTIFICATION_BADGE : undefined,
      actions: [{ action: 'approve', title: 'Approve once' }],
      data: {
        url: viewUrl({ view: 'terminal', paneId: agent.pane_id }),
        action_urls: { approve: viewUrl({ view: 'notification', target: approve }) },
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
      {#if $currentView.view === 'terminal'}
        <Button variant="ghost" size="icon" aria-label="Manage agent" disabled={!activeAgent} onclick={() => { manageOpen = true; }}>•••</Button>
      {:else}
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
          aria-label={updateAvailable ? 'Settings, update available' : 'Settings'}
          onclick={() => toggle('settings')}
        >⚙</Button>
        {#if updateAvailable}<span class="nav-update-badge" aria-hidden="true"></span>{/if}
      </span>
    </nav>
  </header>

  {#if $currentView.view === 'settings'}
    <SettingsView />
  {:else if $currentView.view === 'launch'}
    <LaunchView />
  {:else if $currentView.view === 'activity'}
    <ActivityView />
  {:else if $currentView.view === 'terminal' && activeAgent}
    {#key activeAgent.pane_id}
      <TerminalView agent={activeAgent} allAgents={$agents} frame={$frames.get(activeAgent.pane_id)} responding={$responding} />
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
  {:else}
    <AgentList agents={$agents} relays={$relays} responding={$responding} onopen={openAgent} />
  {/if}
</div>

<ManageDialog bind:open={manageOpen} agent={activeAgent} />
<LockScreen />
<Toast />
