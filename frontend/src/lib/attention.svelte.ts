import { agentNeedsInspection, agentNeedsResponse, approvalPromptPreview, attentionKind, displayName, hostLabel } from './agents';
import { notificationsEnabled, pushOptedIn, showPageNotification } from './push';
import { viewUrl } from './router';
import type { Agent, RelayConnectionView } from './types';

export function observeAgentAttention(getAgents: () => Agent[], getConnections: () => Map<string, RelayConnectionView>) {
  let lastBlocked = new Set<string>();

  async function notifyBlockedAgent(agent: Agent) {
    if (!notificationsEnabled()) return;
    if (document.visibilityState === 'visible' && document.hasFocus()) return;
    const connection = getConnections().get(agent.relay_id);
    if (pushOptedIn() && connection && ['sent', 'subscribed'].includes(connection.pushStatus)) return;
    const kind = attentionKind(agent);
    const host = hostLabel(agent);
    const paneId = agent.raw_pane_id;
    const target = {
      host: String(agent.host || host),
      pane_id: paneId,
      notification_id: String(agent.event_id || `herdr-${host}-${paneId}`),
    };
    const title = kind === 'approval' ? 'blocked' : kind === 'question' ? 'needs answers' : 'needs inspection';
    const fallback = kind === 'approval' ? 'needs approval' : kind === 'question' ? 'needs an answer' : title;
    await showPageNotification(`${displayName(agent)} ${title}`, {
      body: approvalPromptPreview(agent) || `${agent.agent || 'Agent'} ${fallback}`,
      tag: `herdr-${target.host}-${target.pane_id}`,
      renotify: true,
      icon: 'icons/icon-192.png',
      badge: 'icons/notification-badge.png',
      actions: [],
      data: {
        url: viewUrl({ view: 'notification', target }),
      },
    });
  }

  $effect(() => {
    const blocked = getAgents().filter((agent) => agentNeedsResponse(agent) || agentNeedsInspection(agent));
    const count = blocked.length;
    document.title = `${count ? `(${count}) ` : ''}🐑 herdr`;
    const badge = count && navigator.setAppBadge ? navigator.setAppBadge(count) : navigator.clearAppBadge?.();
    void badge?.catch(() => {});
    const keys = new Set<string>();
    let vibrated = false;
    for (const agent of blocked) {
      const key = `${agent.pane_id}:${agent.event_id || ''}:${attentionKind(agent)}`;
      keys.add(key);
      if (lastBlocked.has(key)) continue;
      if (!vibrated) navigator.vibrate?.([120, 80, 120]);
      vibrated = true;
      void notifyBlockedAgent(agent);
    }
    lastBlocked = keys;
  });
}
