import { get, writable } from 'svelte/store';
import { clientPaneId } from './agents';
import { commandRequestId } from './protocol';
import { terminalHistoryLines, terminalRefreshInterval } from './preferences';
import {
  agentTargetPayload,
  normalizeFrontendTargetRef,
  targetRefForAgent,
  targetRefMatchesAgent,
  targetStoreKey,
} from './resource-id';
import type { Agent, TerminalFrame } from './types';

const PANE_READ_RETRY_MS = 35_000;
// Gateway-relayed traffic is metered project bandwidth. Cap scrollback while
// honoring the user's refresh rate; acknowledged deltas keep idle frames off
// the wire and make the selected cadence affordable during normal output.
const RELAYED_HISTORY_LINES = 1_000;

interface PaneConnection {
  capabilities: string[];
  path: string;
}

interface PaneStreamsOwner {
  connection: (relayId: string) => PaneConnection | undefined;
  agents: () => Agent[];
  sendRaw: (relayId: string, payload: Record<string, unknown>) => boolean;
  applyInteraction: (paneId: string, message: Record<string, any>) => void;
}

export class PaneStreams {
  readonly frames = writable<Map<string, TerminalFrame>>(new Map());

  private framesValue = new Map<string, TerminalFrame>();
  private pendingPaneReads = new Map<string, { requestId: string; requestedAt: number; startedAt: number }>();
  private paneContentFingerprints = new Map<string, string>();
  private observations = new WeakMap<TerminalFrame, PaneConnection>();
  private watchedPanes = new Map<string, Agent>();
  private paneWatchesStarted = new Set<string>();

  constructor(private readonly owner: PaneStreamsOwner) {}

  currentFrame(paneId: string): TerminalFrame | undefined {
    return this.framesValue.get(paneId);
  }

  observedConnection(frame: TerminalFrame): PaneConnection | undefined {
    return this.observations.get(frame);
  }

  forgetObservation(frame: TerminalFrame): void {
    this.observations.delete(frame);
  }

  reset(): void {
    this.pendingPaneReads.clear();
    this.paneContentFingerprints.clear();
    this.watchedPanes.clear();
    this.paneWatchesStarted.clear();
  }

  forgetRelay(relayId: string): void {
    this.pendingPaneReads.clear();
    for (const paneId of this.paneWatchesStarted) {
      if (paneId.startsWith(`${relayId}::`)) this.paneWatchesStarted.delete(paneId);
    }
  }

  clearPendingReads(): void {
    this.pendingPaneReads.clear();
  }

  rereadWatched(relayId: string): void {
    for (const watched of this.watchedPanes.values()) {
      if (watched.relay_id === relayId) this.readPane(watched);
    }
  }

  handleUnchanged(relayId: string, message: Record<string, any>): void {
    const { paneId, matches, staleRead, completeRead } = this.paneMessageContext(relayId, message);
    if (staleRead || !matches) return;
    completeRead();
    if (typeof message.content_fingerprint === 'string' && message.content_fingerprint) {
      this.paneContentFingerprints.set(paneId, message.content_fingerprint);
    }
    this.startPaneWatch(paneId);
  }

  handleResync(relayId: string, message: Record<string, any>): void {
    const { paneId, matches } = this.paneMessageContext(relayId, message);
    if (!matches) return;
    const watched = this.watchedPanes.get(paneId);
    if (watched) this.readPane(watched, true);
  }

  handleDelta(relayId: string, message: Record<string, any>): void {
    const { paneId, matches, inputTarget } = this.paneMessageContext(relayId, message);
    if (!matches) return;
    const frame = this.framesValue.get(paneId);
    const baseFingerprint = this.paneContentFingerprints.get(paneId);
    const metadataOnly = baseFingerprint === message.base_fingerprint
      && message.base_fingerprint === message.content_fingerprint
      && Array.isArray(message.segments) && message.segments.length === 0;
    const nextContent = frame && baseFingerprint === message.base_fingerprint
      ? metadataOnly ? frame.content : applyPaneDelta(frame.content, message.segments)
      : null;
    const watched = this.watchedPanes.get(paneId);
    if (nextContent === null || typeof message.content_fingerprint !== 'string') {
      if (watched) this.readPane(watched, true);
      return;
    }
    this.paneContentFingerprints.set(paneId, message.content_fingerprint);
    const deltaNoEcho = typeof message.no_echo === 'boolean' ? message.no_echo : frame?.noEcho;
    this.framesValue.set(paneId, {
      paneId,
      content: nextContent,
      receivedAt: performance.now(),
      inputTarget,
      target: typeof message.no_echo === 'boolean' ? inputTarget : undefined,
      format: String(message.format || frame?.format || 'plain'),
      truncated: typeof message.truncated === 'boolean' ? message.truncated : frame?.truncated,
      viewportOnly: typeof message.viewport_only === 'boolean' ? message.viewport_only : frame?.viewportOnly,
      viewportRows: typeof message.viewport_rows === 'number' ? message.viewport_rows : frame?.viewportRows,
      noEcho: deltaNoEcho,
      noEchoPrompt: deltaNoEcho
        ? typeof message.no_echo_prompt === 'string' ? message.no_echo_prompt : frame?.noEchoPrompt
        : undefined,
      resizeSettling: message.resize_settling === true,
    });
    this.frames.set(new Map(this.framesValue));
    this.owner.applyInteraction(paneId, message);
    if (watched) this.acknowledgePaneFrame(watched, message.content_fingerprint);
  }

  handleContent(relayId: string, message: Record<string, any>): void {
    const { paneId, matches, staleRead, inputTarget, completeRead } = this.paneMessageContext(relayId, message);
    if (staleRead || !matches) return;
    const readStartedAt = completeRead();
    const connection = this.owner.connection(relayId);
    if (typeof message.content_fingerprint === 'string' && message.content_fingerprint) {
      this.paneContentFingerprints.set(paneId, message.content_fingerprint);
    }
    const nextFrame: TerminalFrame = {
      readStartedAt,
      paneId,
      content: typeof message.content === 'string' ? message.content : '(empty)',
      receivedAt: performance.now(),
      target: inputTarget,
      inputTarget,
      noEcho: typeof message.no_echo === 'boolean' ? message.no_echo : undefined,
      format: String(message.format || 'plain'),
    };
    if (message.truncated === true) nextFrame.truncated = true;
    if (message.viewport_only === true) nextFrame.viewportOnly = true;
    if (typeof message.viewport_rows === 'number') nextFrame.viewportRows = message.viewport_rows;
    if (message.no_echo === true) {
      nextFrame.noEcho = true;
      if (typeof message.no_echo_prompt === 'string') nextFrame.noEchoPrompt = message.no_echo_prompt;
    }
    if (message.resize_settling === true) nextFrame.resizeSettling = true;
    if (connection && typeof nextFrame.noEcho === 'boolean' && Number.isFinite(readStartedAt)) this.observations.set(nextFrame, connection);
    this.framesValue.set(paneId, nextFrame);
    this.frames.set(new Map(this.framesValue));
    this.owner.applyInteraction(paneId, message);
    const watched = this.watchedPanes.get(paneId);
    const contentFingerprint = typeof message.content_fingerprint === 'string'
      ? message.content_fingerprint
      : '';
    if (watched && message.ack_required && contentFingerprint) {
      this.acknowledgePaneFrame(watched, contentFingerprint);
    }
    this.startPaneWatch(paneId);
  }

  readPane(agent: Agent, force = false, lines?: number): void {
    const identity = agentTargetPayload(agent);
    if (!identity) return;
    const readKey = targetStoreKey(identity.target);
    if (!readKey) return;
    const pending = this.pendingPaneReads.get(readKey);
    if (!force && pending && Date.now() - pending.requestedAt < PANE_READ_RETRY_MS) return;
    this.paneWatchesStarted.delete(agent.pane_id);
    const requestId = commandRequestId();
    const startedAt = performance.now();
    const sent = this.owner.sendRaw(agent.relay_id, {
      type: 'read_pane',
      request_id: requestId,
      pane_id: agent.raw_pane_id,
      lines: lines ?? this.paneBudget(agent.relay_id).lines,
      format: 'ansi',
      content_fingerprint: force || lines !== undefined ? '' : this.paneContentFingerprints.get(agent.pane_id) || '',
      ...identity,
    });
    if (sent) this.pendingPaneReads.set(readKey, { requestId, startedAt, requestedAt: Date.now() });
  }

  watchPane(agent: Agent): void {
    this.watchedPanes.set(agent.pane_id, agent);
    this.startPaneWatch(agent.pane_id);
  }

  unwatchPane(agent: Agent): void {
    this.watchedPanes.delete(agent.pane_id);
    this.paneWatchesStarted.delete(agent.pane_id);
    const connection = this.owner.connection(agent.relay_id);
    const identity = agentTargetPayload(agent);
    if (!connection?.capabilities.includes('pane_realtime_delta') || !identity) return;
    this.owner.sendRaw(agent.relay_id, {
      type: 'unwatch_pane',
      pane_id: agent.raw_pane_id,
      ...identity,
    });
  }

  restartWatches(): void {
    for (const paneId of [...this.paneWatchesStarted]) {
      this.paneWatchesStarted.delete(paneId);
      this.startPaneWatch(paneId);
    }
  }

  private paneMessageContext(relayId: string, message: Record<string, any>) {
    const paneId = clientPaneId(relayId, String(message.pane_id || ''));
    const agent = this.owner.agents().find((candidate) => candidate.pane_id === paneId)
      || this.watchedPanes.get(paneId);
    const hasTarget = Object.prototype.hasOwnProperty.call(message, 'target');
    const target = !agent
      ? null
      : hasTarget
        ? normalizeFrontendTargetRef({
          ...(message.target as Record<string, unknown>),
          relay_id: relayId,
        })
        : targetRefForAgent(agent);
    const readKey = target && targetStoreKey(target);
    const pendingRead = readKey ? this.pendingPaneReads.get(readKey) : undefined;
    return {
      paneId,
      matches: !hasTarget || Boolean(target && agent && targetRefMatchesAgent(target, agent)),
      staleRead: Boolean(message.request_id && message.request_id !== pendingRead?.requestId),
      inputTarget: !message.error && hasTarget ? target : undefined,
      completeRead: (): number | undefined => {
        if (!readKey || !pendingRead || message.request_id !== pendingRead.requestId) return;
        this.pendingPaneReads.delete(readKey);
        return !message.error ? pendingRead.startedAt : undefined;
      },
    };
  }

  private acknowledgePaneFrame(agent: Agent, contentFingerprint: string): void {
    const identity = agentTargetPayload(agent);
    if (!identity) return;
    this.owner.sendRaw(agent.relay_id, {
      type: 'pane_applied',
      pane_id: agent.raw_pane_id,
      content_fingerprint: contentFingerprint,
      ...identity,
    });
  }

  private startPaneWatch(paneId: string): void {
    const agent = this.watchedPanes.get(paneId);
    if (!agent || this.paneWatchesStarted.has(paneId)) return;
    const connection = this.owner.connection(agent.relay_id);
    if (!connection?.capabilities.includes('pane_realtime_delta')) return;
    const contentFingerprint = this.paneContentFingerprints.get(paneId);
    const identity = agentTargetPayload(agent);
    const readKey = identity && targetStoreKey(identity.target);
    if (!contentFingerprint || !identity || !readKey || this.pendingPaneReads.has(readKey)) return;
    const budget = this.paneBudget(agent.relay_id);
    const sent = this.owner.sendRaw(agent.relay_id, {
      type: 'watch_pane',
      pane_id: agent.raw_pane_id,
      lines: budget.lines,
      interval_ms: budget.intervalMs,
      format: 'ansi',
      content_fingerprint: contentFingerprint,
      ...identity,
    });
    if (sent) this.paneWatchesStarted.add(paneId);
  }

  /**
   * Terminal history the active path can afford. Gateway-relayed traffic is
   * metered project bandwidth, so it caps scrollback; every path honors the
   * user's selected refresh interval.
   */
  private paneBudget(relayId: string): { lines: number; intervalMs: number } {
    const lines = get(terminalHistoryLines);
    const intervalMs = get(terminalRefreshInterval);
    if (this.owner.connection(relayId)?.path !== 'gateway') return { lines, intervalMs };
    return { lines: Math.min(lines, RELAYED_HISTORY_LINES), intervalMs };
  }
}

function applyPaneDelta(previous: string, value: unknown): string | null {
  if (!Array.isArray(value)) return null;
  const boundaries = [0];
  for (let index = 0; index < previous.length; index += 1) {
    if (previous.charCodeAt(index) === 10) boundaries.push(index + 1);
  }
  boundaries.push(previous.length);

  const chunks: string[] = [];
  for (const candidate of value) {
    if (!candidate || typeof candidate !== 'object') return null;
    const segment = candidate as Record<string, unknown>;
    if (segment.copy_lines !== undefined) {
      if (!Number.isInteger(segment.copy_lines) || Number(segment.copy_lines) <= 0) return null;
      const copyLines = Number(segment.copy_lines);
      const copyStart = segment.copy_start === undefined ? 0 : segment.copy_start;
      if (!Number.isInteger(copyStart) || Number(copyStart) < 0) return null;
      const copyEnd = Number(copyStart) + copyLines;
      if (copyEnd > boundaries.length - 1) return null;
      chunks.push(previous.slice(boundaries[Number(copyStart)], boundaries[copyEnd]));
      continue;
    }
    if (typeof segment.text !== 'string') return null;
    chunks.push(segment.text);
  }
  return chunks.join('');
}
