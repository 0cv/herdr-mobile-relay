import type { FrontendTargetRef } from './types';

export interface VirtualTerminalRange {
  start: number;
  end: number;
  top: number;
  bottom: number;
  total: number;
}

export class VirtualTerminalIndex {
  private sizes: number[] = [];
  private tree: number[] = [0];

  get length(): number {
    return this.sizes.length;
  }

  get total(): number {
    return this.prefix(this.sizes.length);
  }

  reset(sizes: readonly number[]): void {
    this.sizes = [...sizes];
    this.tree = [0, ...this.sizes];
    for (let index = 1; index < this.tree.length; index += 1) {
      const parent = index + (index & -index);
      if (parent < this.tree.length) this.tree[parent] += this.tree[index];
    }
  }

  size(index: number): number {
    return this.sizes[index] || 0;
  }

  offset(index: number): number {
    return this.prefix(Math.max(0, Math.min(index, this.sizes.length)));
  }

  update(index: number, size: number): number {
    if (index < 0 || index >= this.sizes.length || !Number.isFinite(size) || size <= 0) return 0;
    const delta = size - this.sizes[index];
    if (Math.abs(delta) < 0.25) return 0;
    this.sizes[index] = size;
    this.add(index, delta);
    return delta;
  }

  indexAt(offset: number): number {
    if (!this.sizes.length) return 0;
    const target = Math.max(0, Math.min(offset, this.total));
    let index = 0;
    let sum = 0;
    let bit = 1;
    while (bit * 2 < this.tree.length) bit *= 2;
    for (; bit > 0; bit = Math.floor(bit / 2)) {
      const next = index + bit;
      if (next < this.tree.length && sum + this.tree[next] <= target) {
        index = next;
        sum += this.tree[next];
      }
    }
    return Math.min(index, this.sizes.length - 1);
  }

  range(scrollTop: number, viewportHeight: number, overscan: number): VirtualTerminalRange {
    const total = this.total;
    if (!this.sizes.length) return { start: 0, end: 0, top: 0, bottom: 0, total };
    const startOffset = Math.max(0, scrollTop - overscan);
    const endOffset = Math.min(total, scrollTop + Math.max(0, viewportHeight) + overscan);
    const start = this.indexAt(startOffset);
    const end = Math.min(this.sizes.length, this.indexAt(endOffset) + 1);
    const top = this.offset(start);
    return {
      start,
      end,
      top,
      bottom: Math.max(0, total - this.offset(end)),
      total,
    };
  }

  private add(index: number, delta: number): void {
    for (let cursor = index + 1; cursor < this.tree.length; cursor += cursor & -cursor) {
      this.tree[cursor] += delta;
    }
  }

  private prefix(count: number): number {
    let sum = 0;
    for (let cursor = count; cursor > 0; cursor -= cursor & -cursor) sum += this.tree[cursor];
    return sum;
  }
}

export interface VirtualTerminalDelta {
  start_line: number;
  delete_lines: number;
  lines: readonly string[];
}

export interface VirtualTerminalStreamFrame {
  target: FrontendTargetRef;
  stream_id: number;
  seq: number;
  base_seq: number;
  repaint: boolean;
  rows: number;
  columns: number;
  content?: string;
  delta?: VirtualTerminalDelta;
}

export interface VirtualTerminalSnapshot {
  readonly target: FrontendTargetRef;
  readonly stream_id: number;
  readonly seq: number;
  readonly rows: number;
  readonly columns: number;
  readonly content: string;
}

export type VirtualTerminalRepaintReason =
  | 'initial_frame_not_repaint'
  | 'sequence_gap'
  | 'base_mismatch'
  | 'invalid_delta'
  | 'invalid_frame';

export interface VirtualTerminalRepaintRequest {
  target: FrontendTargetRef;
  stream_id: number;
  seq: number;
  reason: VirtualTerminalRepaintReason;
}

export type RequestVirtualTerminalRepaint = (
  request: VirtualTerminalRepaintRequest,
) => void | Promise<void>;

export type VirtualTerminalConsumeResult =
  | { status: 'applied'; snapshot: VirtualTerminalSnapshot }
  | {
    status: 'ignored';
    reason: 'stale_generation' | 'stale_stream' | 'stale_sequence' | 'awaiting_repaint';
  }
  | { status: 'repaint_requested'; reason: VirtualTerminalRepaintReason }
  | { status: 'rejected'; reason: 'invalid_identity' };

export interface VirtualTerminalConsumerOptions {
  historyLimit?: number;
  streamLimit?: number;
  maxContentLength?: number;
}

interface VirtualTerminalStreamState {
  snapshot: VirtualTerminalSnapshot;
  history: VirtualTerminalSnapshot[];
  awaitingRepaint: boolean;
}

const DEFAULT_HISTORY_LIMIT = 16;
const DEFAULT_STREAM_LIMIT = 64;
const DEFAULT_MAX_CONTENT_LENGTH = 2_000_000;

function safeNonNegativeInteger(value: number): boolean {
  return Number.isSafeInteger(value) && value >= 0;
}

function validTarget(target: FrontendTargetRef): boolean {
  return Boolean(
    target
    && target.relay_id
    && target.server_session_id
    && target.pane_id
    && target.terminal_id
    && safeNonNegativeInteger(target.generation),
  );
}

function targetParts(target: FrontendTargetRef): readonly (string | number)[] {
  return [
    target.relay_id,
    target.server_session_id,
    target.pane_id,
    target.terminal_id,
    target.generation,
    target.agent_session_id || '',
  ];
}

function targetKey(target: FrontendTargetRef): string {
  return JSON.stringify(targetParts(target));
}

function targetLineageKey(target: FrontendTargetRef): string {
  return JSON.stringify([
    target.relay_id,
    target.server_session_id,
    target.pane_id,
    target.terminal_id,
    target.agent_session_id || '',
  ]);
}

export function virtualTerminalStreamKey(target: FrontendTargetRef, streamId: number): string {
  return JSON.stringify([...targetParts(target), streamId]);
}

function cloneTarget(target: FrontendTargetRef): FrontendTargetRef {
  return {
    relay_id: target.relay_id,
    server_session_id: target.server_session_id,
    pane_id: target.pane_id,
    terminal_id: target.terminal_id,
    generation: target.generation,
    ...(target.agent_session_id ? { agent_session_id: target.agent_session_id } : {}),
  };
}

function validStaticFrame(frame: VirtualTerminalStreamFrame): boolean {
  if (
    !safeNonNegativeInteger(frame.seq)
    || !safeNonNegativeInteger(frame.base_seq)
    || !Number.isSafeInteger(frame.rows)
    || frame.rows <= 0
    || !Number.isSafeInteger(frame.columns)
    || frame.columns <= 0
  ) {
    return false;
  }
  if (frame.repaint) {
    return frame.base_seq === 0 && typeof frame.content === 'string' && frame.delta === undefined;
  }
  const hasContent = typeof frame.content === 'string';
  const hasDelta = frame.delta !== undefined;
  return hasContent !== hasDelta;
}

function applyDelta(
  content: string,
  delta: VirtualTerminalDelta,
  maxContentLength: number,
): string | undefined {
  if (
    !delta
    || !safeNonNegativeInteger(delta.start_line)
    || !safeNonNegativeInteger(delta.delete_lines)
    || !Array.isArray(delta.lines)
    || delta.lines.some((line) => typeof line !== 'string')
  ) {
    return undefined;
  }
  const lines = content.split('\n');
  if (
    delta.start_line > lines.length
    || delta.delete_lines > lines.length - delta.start_line
  ) {
    return undefined;
  }
  const nextLength = lines.length - delta.delete_lines + delta.lines.length;
  const next = new Array<string>(nextLength);
  let output = 0;
  for (let index = 0; index < delta.start_line; index += 1) next[output++] = lines[index];
  for (const line of delta.lines) next[output++] = line;
  for (
    let index = delta.start_line + delta.delete_lines;
    index < lines.length;
    index += 1
  ) {
    next[output++] = lines[index];
  }
  const result = next.join('\n');
  return result.length <= maxContentLength ? result : undefined;
}

/**
 * Reconstructs independent terminal streams without ever applying a delta to
 * another relay, session, pane, terminal, generation, or stream. A recovery
 * request is emitted once per broken chain; only a newer repaint can resume it.
 */
export class VirtualTerminalStreamConsumer {
  private readonly states = new Map<string, VirtualTerminalStreamState>();
  private readonly activeGeneration = new Map<string, number>();
  private readonly activeStream = new Map<string, number>();
  private readonly historyLimit: number;
  private readonly streamLimit: number;
  private readonly maxContentLength: number;

  constructor(
    private readonly requestRepaint: RequestVirtualTerminalRepaint,
    options: VirtualTerminalConsumerOptions = {},
  ) {
    this.historyLimit = positiveOption(options.historyLimit, DEFAULT_HISTORY_LIMIT, 'historyLimit');
    this.streamLimit = positiveOption(options.streamLimit, DEFAULT_STREAM_LIMIT, 'streamLimit');
    this.maxContentLength = positiveOption(
      options.maxContentLength,
      DEFAULT_MAX_CONTENT_LENGTH,
      'maxContentLength',
    );
  }

  consume(frame: VirtualTerminalStreamFrame): VirtualTerminalConsumeResult {
    if (!validTarget(frame.target) || !safeNonNegativeInteger(frame.stream_id)) {
      return { status: 'rejected', reason: 'invalid_identity' };
    }

    const lineage = targetLineageKey(frame.target);
    const generation = this.activeGeneration.get(lineage);
    if (generation !== undefined && frame.target.generation < generation) {
      return { status: 'ignored', reason: 'stale_generation' };
    }

    const target = targetKey(frame.target);
    const stream = this.activeStream.get(target);
    if (
      generation === frame.target.generation
      && stream !== undefined
      && frame.stream_id < stream
    ) {
      return { status: 'ignored', reason: 'stale_stream' };
    }

    if (!validStaticFrame(frame)) {
      return this.recover(frame, 'invalid_frame');
    }

    if (generation === undefined || frame.target.generation > generation) {
      this.dropLineage(lineage);
      this.activeGeneration.set(lineage, frame.target.generation);
    }

    const currentStream = this.activeStream.get(target);
    if (currentStream === undefined || frame.stream_id > currentStream) {
      this.dropTargetStreams(target);
      this.activeStream.set(target, frame.stream_id);
    }

    const key = virtualTerminalStreamKey(frame.target, frame.stream_id);
    const state = this.states.get(key);

    if (frame.repaint) {
      if (state && frame.seq <= state.snapshot.seq) {
        return { status: 'ignored', reason: 'stale_sequence' };
      }
      if (frame.content!.length > this.maxContentLength) {
        return this.recover(frame, 'invalid_frame', state);
      }
      const snapshot = makeSnapshot(frame, frame.content!);
      this.states.delete(key);
      this.states.set(key, { snapshot, history: [snapshot], awaitingRepaint: false });
      this.enforceStreamLimit();
      return { status: 'applied', snapshot };
    }

    if (!state) return this.recover(frame, 'initial_frame_not_repaint');
    if (state.awaitingRepaint) return { status: 'ignored', reason: 'awaiting_repaint' };
    if (frame.seq <= state.snapshot.seq) {
      return { status: 'ignored', reason: 'stale_sequence' };
    }
    if (frame.seq !== state.snapshot.seq + 1) {
      return this.recover(frame, 'sequence_gap', state);
    }
    if (frame.base_seq !== state.snapshot.seq) {
      return this.recover(frame, 'base_mismatch', state);
    }

    const content = frame.delta
      ? applyDelta(state.snapshot.content, frame.delta, this.maxContentLength)
      : frame.content!.length <= this.maxContentLength
        ? frame.content
        : undefined;
    if (content === undefined) return this.recover(frame, 'invalid_delta', state);

    const snapshot = makeSnapshot(frame, content);
    state.snapshot = snapshot;
    state.history.push(snapshot);
    if (state.history.length > this.historyLimit) {
      state.history.splice(0, state.history.length - this.historyLimit);
    }
    this.states.delete(key);
    this.states.set(key, state);
    return { status: 'applied', snapshot };
  }

  getSnapshot(
    target: FrontendTargetRef,
    streamId: number,
  ): VirtualTerminalSnapshot | undefined {
    return this.states.get(virtualTerminalStreamKey(target, streamId))?.snapshot;
  }

  getActiveSnapshot(target: FrontendTargetRef): VirtualTerminalSnapshot | undefined {
    const stream = this.activeStream.get(targetKey(target));
    return stream === undefined ? undefined : this.getSnapshot(target, stream);
  }

  getHistory(
    target: FrontendTargetRef,
    streamId: number,
  ): readonly VirtualTerminalSnapshot[] {
    return this.states.get(virtualTerminalStreamKey(target, streamId))?.history || [];
  }

  forgetTarget(target: FrontendTargetRef): void {
    const key = targetKey(target);
    this.dropTargetStreams(key);
    this.activeStream.delete(key);
    const lineage = targetLineageKey(target);
    if (this.activeGeneration.get(lineage) === target.generation) {
      this.activeGeneration.delete(lineage);
    }
  }

  clear(): void {
    this.states.clear();
    this.activeGeneration.clear();
    this.activeStream.clear();
  }

  private recover(
    frame: VirtualTerminalStreamFrame,
    reason: VirtualTerminalRepaintReason,
    existing?: VirtualTerminalStreamState,
  ): VirtualTerminalConsumeResult {
    const key = virtualTerminalStreamKey(frame.target, frame.stream_id);
    const state = existing || this.states.get(key);
    if (state?.awaitingRepaint) return { status: 'ignored', reason: 'awaiting_repaint' };
    if (state) state.awaitingRepaint = true;
    const seq = state?.snapshot.seq || 0;
    void this.requestRepaint({
      target: cloneTarget(frame.target),
      stream_id: frame.stream_id,
      seq,
      reason,
    });
    return { status: 'repaint_requested', reason };
  }

  private dropLineage(lineage: string): void {
    for (const [key, state] of this.states) {
      if (targetLineageKey(state.snapshot.target) !== lineage) continue;
      this.states.delete(key);
      this.activeStream.delete(targetKey(state.snapshot.target));
    }
  }

  private dropTargetStreams(target: string): void {
    for (const [key, state] of this.states) {
      if (targetKey(state.snapshot.target) === target) this.states.delete(key);
    }
  }

  private enforceStreamLimit(): void {
    while (this.states.size > this.streamLimit) {
      const oldest = this.states.entries().next().value as
        | [string, VirtualTerminalStreamState]
        | undefined;
      if (!oldest) return;
      this.states.delete(oldest[0]);
      const target = targetKey(oldest[1].snapshot.target);
      if (this.activeStream.get(target) === oldest[1].snapshot.stream_id) {
        this.activeStream.delete(target);
      }
    }
  }
}

function positiveOption(value: number | undefined, fallback: number, name: string): number {
  const option = value ?? fallback;
  if (!Number.isSafeInteger(option) || option <= 0) {
    throw new RangeError(`${name}_must_be_positive`);
  }
  return option;
}

function makeSnapshot(
  frame: VirtualTerminalStreamFrame,
  content: string,
): VirtualTerminalSnapshot {
  return Object.freeze({
    target: Object.freeze(cloneTarget(frame.target)),
    stream_id: frame.stream_id,
    seq: frame.seq,
    rows: frame.rows,
    columns: frame.columns,
    content,
  });
}
