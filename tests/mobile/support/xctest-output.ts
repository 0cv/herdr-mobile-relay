export type XCTestOutputStream = 'stdout' | 'stderr';
export type XCTestOutputStage = 'tap' | 'find-element' | 'wait-for-idle' | 'wait-for-animation' | 'check-interruptions' | 'synthesize-event' | 'request-snapshot';

interface XCTestOutputEvent {
  ordinal: number;
  stream: XCTestOutputStream;
  stage: XCTestOutputStage;
  observedAt: string;
  monotonicReceiveMs: number;
}

export interface XCTestOutputSummary {
  schema: 1;
  completeRecords: number;
  recognizedRecords: number;
  unknownRecords: number;
  oversizedRecords: number;
  incompleteRecords: number;
  droppedEvents: number;
  inputTruncated: boolean;
  events: XCTestOutputEvent[];
}

interface StreamState {
  pending: string;
  bytes: number;
  oversized: boolean;
  finalized: boolean;
}

const maximumLineBytes = 4_096;
const maximumEvents = 128;
const stages: Array<[XCTestOutputStage, RegExp]> = [
  ['tap', /^Tap .+$/u],
  ['find-element', /^Find .+$/u],
  ['wait-for-idle', /^Wait for .+ to idle$/u],
  ['wait-for-animation', /^Wait for .+ animations to finish$/u],
  ['check-interruptions', /^Check for interrupting elements affecting .+$/u],
  ['synthesize-event', /^Synthesize event$/u],
  ['request-snapshot', /^Requesting snapshot of accessibility hierarchy for app with pid \d+$/u],
];

export class XCTestOutputDiagnostics {
  readonly data: XCTestOutputSummary = {
    schema: 1, completeRecords: 0, recognizedRecords: 0, unknownRecords: 0,
    oversizedRecords: 0, incompleteRecords: 0, droppedEvents: 0, inputTruncated: false, events: [],
  };
  private readonly streams: Record<XCTestOutputStream, StreamState> = {
    stdout: {pending: '', bytes: 0, oversized: false, finalized: false},
    stderr: {pending: '', bytes: 0, oversized: false, finalized: false},
  };

  constructor(private readonly clock: () => { observedAt: string; monotonicReceiveMs: number }) {}

  observe(text: string, stream: XCTestOutputStream): void {
    const state = this.streams[stream];
    if (state.finalized) return;
    let offset = 0;
    while (offset < text.length) {
      const newline = text.indexOf('\n', offset);
      const end = newline === -1 ? text.length : newline;
      if (!state.oversized) {
        const part = text.slice(offset, end);
        state.bytes += Buffer.byteLength(part);
        if (state.bytes > maximumLineBytes) {
          state.pending = '';
          state.oversized = true;
          this.data.oversizedRecords++;
        } else state.pending += part;
      }
      if (newline === -1) return;
      this.data.completeRecords++;
      if (!state.oversized) this.record(state.pending, stream);
      state.pending = '';
      state.bytes = 0;
      state.oversized = false;
      offset = newline + 1;
    }
  }

  finish(stream: XCTestOutputStream): void {
    const state = this.streams[stream];
    if (state.finalized) return;
    if (state.bytes > 0) this.data.incompleteRecords++;
    state.pending = '';
    state.bytes = 0;
    state.oversized = false;
    state.finalized = true;
  }

  private record(line: string, stream: XCTestOutputStream): void {
    const activity = !line.includes('\uFFFD') && /^[ \t]*t = +\d{1,6}\.\d{2}s +([^\r\n]+)\r?$/u.exec(line)?.[1];
    const stage = activity && stages.find(([, pattern]) => pattern.test(activity))?.[0];
    if (!stage) {
      this.data.unknownRecords++;
      return;
    }
    this.data.events.push({ ordinal: ++this.data.recognizedRecords, stream, stage, ...this.clock() });
    if (this.data.events.length > maximumEvents) {
      this.data.events.shift();
      this.data.droppedEvents++;
    }
  }
}
