import { shouldRetainSetupFragment } from './config';
import { stateFromLocation, type ViewState } from './router';

export type HistoryViewState = ViewState & {
  herdrView?: boolean;
  index?: number;
  bootstrapPairingFlow?: string;
};

export function stampView(state: ViewState, index: number): HistoryViewState {
  return { herdrView: true, index, bootstrapPairingFlow: history.state?.bootstrapPairingFlow, ...state };
}

export function listenViewHistory(show: (state: ViewState, index?: number) => void): () => void {
  const onPopState = (event: PopStateEvent) => {
    const state = event.state as HistoryViewState | null;
    const index = state?.index;
    show(state?.herdrView ? state : { view: 'agents' }, Number.isInteger(index) ? index : 0);
  };
  const onHashChange = () => {
    const state = history.state as HistoryViewState | null;
    show(state?.herdrView && shouldRetainSetupFragment(location, navigator.standalone) ? state : stateFromLocation());
  };
  const listeners = new AbortController();
  const options = { signal: listeners.signal };
  window.addEventListener('popstate', onPopState, options);
  window.addEventListener('hashchange', onHashChange, options);
  return () => listeners.abort();
}
