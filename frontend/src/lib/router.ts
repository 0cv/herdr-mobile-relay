import { get, writable } from 'svelte/store';
import { shouldRetainSetupFragment } from './config';
import { clientPaneId } from './agents';
import { parseNotificationTarget } from './protocol';
import { decodeTargetRoute, encodeTargetRoute } from './resource-id';
import type { FrontendTargetRef, NotificationTarget } from './types';

export type ViewState =
  | { view: 'agents' }
  | { view: 'settings' }
  | { view: 'workspaces' }
  | { view: 'launch'; relayId?: string; workspaceId?: string; cwd?: string }
  | { view: 'activity' }
  | { view: 'activity_detail'; key: string }
  | { view: 'terminal'; paneId: string; target?: FrontendTargetRef }
  | { view: 'history'; paneId: string; target?: FrontendTargetRef }
  | { view: 'notification'; target: NotificationTarget }
  | { view: 'push'; eventRef: string; deviceId: string }
  | { view: 'push_unavailable' };

type HistoryViewState = ViewState & {
  herdrView?: boolean;
  index?: number;
};

export const currentView = writable<ViewState>({ view: 'agents' });
let viewIndex = 0;

function showView(state: ViewState): void {
  if (state.view !== 'agents') window.scrollTo(0, 0);
  currentView.set(state);
}

export function stateFromLocation(locationValue: Pick<Location, 'hash'> = location): ViewState {
  if (locationValue.hash === '#settings') return { view: 'settings' };
  if (locationValue.hash === '#workspaces') return { view: 'workspaces' };
  if (locationValue.hash === '#launch') return { view: 'launch' };
  const launchTarget = locationValue.hash.match(/^#launch=(.+)$/);
  if (launchTarget) {
    try {
      const target = JSON.parse(decodeURIComponent(launchTarget[1])) as Record<string, unknown>;
      return {
        view: 'launch',
        relayId: String(target.relayId || ''),
        workspaceId: String(target.workspaceId || ''),
        cwd: String(target.cwd || ''),
      };
    } catch {
      return { view: 'launch' };
    }
  }
  if (locationValue.hash === '#activity') return { view: 'activity' };
  const activityDetail = locationValue.hash.match(/^#activity=(.+)$/);
  if (activityDetail) {
    try {
      return { view: 'activity_detail', key: decodeURIComponent(activityDetail[1]) };
    } catch {
      return { view: 'activity' };
    }
  }
  const pane = locationValue.hash.match(/^#pane=(.+)$/);
  if (pane) {
    const target = decodeTargetRoute(pane[1]);
    if (target) return { view: 'terminal', paneId: clientPaneId(target.relay_id, target.pane_id), target };
    if (pane[1].startsWith('r3.')) return { view: 'agents' };
    try {
      return { view: 'terminal', paneId: decodeURIComponent(pane[1]) };
    } catch {
      return { view: 'agents' };
    }
  }
  const historyPane = locationValue.hash.match(/^#history=(.+)$/);
  if (historyPane) {
    const target = decodeTargetRoute(historyPane[1]);
    if (target) return { view: 'history', paneId: clientPaneId(target.relay_id, target.pane_id), target };
    if (historyPane[1].startsWith('r3.')) return { view: 'agents' };
    try {
      return { view: 'history', paneId: decodeURIComponent(historyPane[1]) };
    } catch {
      return { view: 'agents' };
    }
  }
  const push = locationValue.hash.match(/^#push=([A-Za-z0-9_.~-]+)&device=([A-Za-z0-9_-]{1,128})$/);
  if (push) {
    try {
      const eventRef = decodeURIComponent(push[1]);
      const deviceId = decodeURIComponent(push[2]);
      if (/^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/.test(eventRef) && eventRef.length <= 4096) {
        return { view: 'push', eventRef, deviceId };
      }
    } catch {
      return { view: 'push_unavailable' };
    }
  }
  const notification = locationValue.hash.match(/^#notify=(.+)$/);
  if (notification) {
    const target = parseNotificationTarget(notification[1]);
    if (target) return { view: 'notification', target };
  }
  return { view: 'agents' };
}

export function viewUrl(state: ViewState): string {
  if (state.view === 'settings') return '#settings';
  if (state.view === 'workspaces') return '#workspaces';
  if (state.view === 'launch') {
    if (!state.workspaceId) return '#launch';
    return `#launch=${encodeURIComponent(JSON.stringify({
      relayId: state.relayId || '',
      workspaceId: state.workspaceId,
      cwd: state.cwd || '',
    }))}`;
  }
  if (state.view === 'activity') return '#activity';
  if (state.view === 'activity_detail') return `#activity=${encodeURIComponent(state.key)}`;
  if (state.view === 'terminal') {
    const target = state.target && encodeTargetRoute(state.target);
    return `#pane=${target || encodeURIComponent(state.paneId)}`;
  }
  if (state.view === 'history') {
    const target = state.target && encodeTargetRoute(state.target);
    return `#history=${target || encodeURIComponent(state.paneId)}`;
  }
  if (state.view === 'notification') return `#notify=${encodeURIComponent(JSON.stringify(state.target))}`;
  if (state.view === 'push') return `#push=${encodeURIComponent(state.eventRef)}&device=${encodeURIComponent(state.deviceId)}`;
  if (state.view === 'push_unavailable') return '#push-unavailable';
  return location.pathname + location.search;
}

export function navigate(state: ViewState): void {
  viewIndex += 1;
  history.pushState({ herdrView: true, index: viewIndex, ...state }, '', viewUrl(state));
  showView(state);
}

export function replaceView(state: ViewState): void {
  history.replaceState({ herdrView: true, index: viewIndex, ...state }, '', viewUrl(state));
  showView(state);
}

export function closeCurrentView(): void {
  if (get(currentView).view === 'agents') return;
  const state = history.state as HistoryViewState | null;
  if (viewIndex > 0 && state?.herdrView) history.back();
  else replaceView({ view: 'agents' });
}

export function initializeRouter(): () => void {
  const setupUrl = shouldRetainSetupFragment(location, navigator.standalone)
    ? location.pathname + location.search + location.hash
    : '';
  const initial = stateFromLocation();
  replaceView({ view: 'agents' });
  if (setupUrl) history.replaceState(history.state, '', setupUrl);
  if (initial.view !== 'agents') navigate(initial);
  const onPopState = (event: PopStateEvent) => {
    const state = event.state as HistoryViewState | null;
    viewIndex = Number.isInteger(state?.index) ? Number(state?.index) : 0;
    showView(state?.herdrView ? state : { view: 'agents' });
  };
  const onHashChange = () => showView(stateFromLocation());
  window.addEventListener('popstate', onPopState);
  window.addEventListener('hashchange', onHashChange);
  return () => {
    window.removeEventListener('popstate', onPopState);
    window.removeEventListener('hashchange', onHashChange);
  };
}

export function routeNotificationUrl(url: string): void {
  try {
    const target = new URL(url, location.href);
    if (target.origin !== location.origin || !target.hash) return;
    const state = stateFromLocation({ hash: target.hash });
    if (state.view === 'agents') return;
    navigate(state);
  } catch {
    // Ignore cross-origin and malformed notification URLs.
  }
}
