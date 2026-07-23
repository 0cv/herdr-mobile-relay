import { get, writable } from 'svelte/store';
import { parseNotificationTarget } from './protocol';
import type { NotificationTarget } from './types';

export type ViewState =
  | { view: 'agents' }
  | { view: 'settings' }
  | { view: 'launch' }
  | { view: 'activity' }
  | { view: 'activity_detail'; key: string }
  | { view: 'terminal'; paneId: string }
  | { view: 'notification'; target: NotificationTarget };

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
  if (locationValue.hash === '#launch') return { view: 'launch' };
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
    try {
      return { view: 'terminal', paneId: decodeURIComponent(pane[1]) };
    } catch {
      return { view: 'agents' };
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
  if (state.view === 'launch') return '#launch';
  if (state.view === 'activity') return '#activity';
  if (state.view === 'activity_detail') return `#activity=${encodeURIComponent(state.key)}`;
  if (state.view === 'terminal') return `#pane=${encodeURIComponent(state.paneId)}`;
  if (state.view === 'notification') return `#notify=${encodeURIComponent(JSON.stringify(state.target))}`;
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
  const initial = stateFromLocation();
  replaceView({ view: 'agents' });
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
    if (location.hash !== target.hash) location.hash = target.hash;
    else showView(stateFromLocation());
  } catch {
    // Ignore cross-origin and malformed notification URLs.
  }
}
