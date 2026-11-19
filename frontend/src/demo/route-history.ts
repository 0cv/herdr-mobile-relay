import type { HistoryViewState } from '$lib/route-history';
import { stateFromLocation, viewUrl, type ViewState } from '$lib/router';

const visit = crypto.getRandomValues(new Uint32Array(4)).join('-');
type VisitState = HistoryViewState & { visit?: string };

export function stampView(state: ViewState, index: number): VisitState {
  return { herdrView: true, index, ...state, visit };
}

export function listenViewHistory(show: (state: ViewState, index?: number) => void): () => void {
  const onPopState = (event: PopStateEvent) => {
    const state = event.state as VisitState | null;
    if (state?.herdrView && state.visit !== visit) {
      const initial = { view: 'agents' } as const;
      history.replaceState(stampView(initial, 0), '', viewUrl(initial));
      show(initial, 0);
      return;
    }
    show(state?.herdrView ? state : { view: 'agents' }, Number.isInteger(state?.index) ? state!.index : 0);
  };
  const onHashChange = () => {
    if (history.state?.herdrView) return;
    const view = stateFromLocation();
    history.replaceState(stampView(view, 0), '');
    show(view, 0);
  };
  const listeners = new AbortController();
  const options = { signal: listeners.signal };
  window.addEventListener('popstate', onPopState, options);
  window.addEventListener('hashchange', onHashChange, options);
  return () => listeners.abort();
}
