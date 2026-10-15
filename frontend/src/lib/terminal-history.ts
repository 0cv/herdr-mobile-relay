import type { FrontendTargetRef, TerminalFrame, TerminalHistory } from './types';

export interface RetainedPaneHistory {
  identity: string;
  snapshot?: TerminalHistory;
  liveScreenSeen: boolean;
}

export function retainPaneHistory(previous: RetainedPaneHistory | undefined, target: FrontendTargetRef | null, frame: TerminalFrame): RetainedPaneHistory | undefined {
  if (!target) return undefined;
  const identity = JSON.stringify([target.relay_id, target.server_session_id, target.pane_id, target.terminal_id]);
  const retained = previous?.identity === identity ? previous : undefined;
  if (frame.scrolling?.presentation === 'live_screen' || frame.scrolling?.renderer === 'fullscreen') {
    return { identity, snapshot: retained?.snapshot, liveScreenSeen: true };
  }
  const historyPresentation = frame.scrolling?.presentation === 'history';
  const uncertainReplacement = frame.scrolling?.renderer === 'unknown' && (retained?.snapshot || retained?.liveScreenSeen);
  if (frame.resizeSettling || frame.noEcho || uncertainReplacement || frame.scrolling && !historyPresentation || frame.viewportOnly && !historyPresentation) return retained;
  return { identity, snapshot: { content: frame.content, format: frame.format, truncated: frame.truncated }, liveScreenSeen: false };
}
