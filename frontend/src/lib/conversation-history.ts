import { cloneDiagnostics, cloneEntries, clonePlan, estimateEntriesBytes } from './conversation-values';
import { conversationEntries } from './conversation';
import type { ConversationPreview } from './conversation-cache';
import { targetRefForAgent, targetStoreKey } from './resource-id';
import type {
  Agent,
  ConversationBrowseDiagnostics,
  ConversationBrowseError,
  ConversationBrowseProgress,
  ConversationBrowseState,
  ConversationEntry,
  ConversationHistoryRequest,
  ConversationPage,
  OmoTodoState,
} from './types';

export const HISTORY_WIRE_PAGE_SIZE = 24;
export const HISTORY_PREPARATION_INTERVAL_MS = 1_000;
export const HISTORY_MAX_PREPARATION_POLLS = 30;
export const HISTORY_MAX_PAGES_PER_DEMAND = 100;
export const HISTORY_MAX_RAW_ENTRIES = 20_000;
export const HISTORY_MAX_CONTENT_BYTES = 32 * 1024 * 1024;
export const HISTORY_ACTIVE_DEADLINE_MS = 2 * 60_000;
export const HISTORY_MAX_NO_RAW_PROGRESS_PAGES = 32;
export const HISTORY_BREATHER_INTERVAL_PAGES = 5;
export const HISTORY_BREATHER_MS = 100;

export type HistoryRequestPhase = 'idle' | 'initial' | 'refresh' | 'older' | 'preparing';
export type HistoryIntent = 'live' | 'historical';

type Demand = 'initial' | 'refresh' | 'older' | 'full';

type OlderContinuation = {
  nextCursor: string;
  hasMore: boolean;
  snapshotId: string;
  beginningReached: boolean;
  omoPlan: OmoTodoState | null;
  diagnostics?: ConversationBrowseDiagnostics;
};

export interface ConversationBatchAnalysis {
  displayableEntries: ConversationEntry[];
  completedOlderExchanges: number;
  needsLeadingContext: boolean;
  hasUsableLatestExchange: boolean;
  pendingPrefix: ConversationEntry[];
}

export interface ConversationHistoryControllerState {
  epoch: number;
  identity: string | null;
  entries: ConversationEntry[];
  preview: boolean;
  previewHistorical: boolean;
  authoritative: boolean;
  available: boolean;
  reason: string;
  requestPhase: HistoryRequestPhase;
  intent: HistoryIntent;
  state: ConversationBrowseState;
  sourceRevision: string;
  snapshotId: string;
  hasMore: boolean;
  nextCursor: string;
  progress?: ConversationBrowseProgress;
  diagnostics?: ConversationBrowseDiagnostics;
  omoPlan: OmoTodoState | null;
  beginningReached: boolean;
  contextSearching: boolean;
  latestGapOutstanding: boolean;
  preparationPolls: number;
  pausedReason: string;
  error: ConversationBrowseError | null;
  pendingPrefix: ConversationEntry[];
}

export interface ConversationHistoryControllerOptions {
  request: (agent: Agent, request: ConversationHistoryRequest) => Promise<ConversationPage>;
  onState?: (state: ConversationHistoryControllerState) => void;
  /** Called only for an accepted, cursorless unavailable network result. */
  onInitialPage?: (page: ConversationPage) => void;
  getPreview?: (identity: string) => ConversationPreview | null;
  putPreview?: (preview: ConversationPreview) => void;
  yieldToBrowser?: () => Promise<void>;
  isActive?: () => boolean;
  preparationIntervalMs?: number;
  maxPagesPerDemand?: number;
  maxContentBytes?: number;
}

/**
 * Stable cache identity for one conversation. The target tuple is deliberately
 * shared with command routing; provider and cwd distinguish two transcript
 * sources which happen to use the same pane target. Status and presentation
 * metadata are not part of the key.
 */
export function conversationIdentity(agent: Partial<Agent>): string | null {
  const target = targetRefForAgent(agent);
  const targetKey = target ? targetStoreKey(target) : null;
  const provider = normalizedProvider(agent.agent);
  const session = String(agent.agent_session_id || '').trim();
  if (!targetKey || !provider || !session) return null;
  return JSON.stringify([targetKey, provider, String(agent.cwd || '').trim(), session]);
}

/**
 * Analyse an ordered raw range as exchanges. A page boundary can leave a
 * leading assistant/tool fragment without its prompt; it is staged as a
 * prefix while already complete exchanges remain publishable.
 */
export function analyzeConversationBatch(
  entries: ConversationEntry[],
  hasOlder = false,
  sourceExhausted = false,
): ConversationBatchAnalysis {
  const copied = cloneEntries(entries);
  const displayableEntries = conversationEntries(copied);
  const firstUser = copied.findIndex((entry) => entry.role === 'user');
  const userIndexes = copied.flatMap((entry, index) => entry.role === 'user' ? [index] : []);
  const unresolvedOlderContext = hasOlder && !sourceExhausted;
  const pendingPrefix = unresolvedOlderContext && (firstUser > 0 || firstUser < 0)
    ? copied.slice(0, firstUser < 0 ? copied.length : firstUser)
    : [];
  const needsLeadingContext = unresolvedOlderContext && pendingPrefix.length > 0;
  let completedOlderExchanges = 0;
  for (let index = 0; index < userIndexes.length; index++) {
    const isBoundaryKnown = index + 1 < userIndexes.length || sourceExhausted;
    if (isBoundaryKnown) completedOlderExchanges++;
  }
  // A user prompt is an older boundary when the next user prompt or the
  // beginning of the source proves where its exchange ends. The newest prompt
  // remains useful content, but is not counted while the source may continue.
  const hasUsableLatestExchange = userIndexes.length > 0
    || ((!hasOlder || sourceExhausted) && displayableEntries.some((entry) => entry.role === 'assistant' && Boolean(entry.text.trim())));
  return {
    displayableEntries,
    completedOlderExchanges,
    needsLeadingContext,
    hasUsableLatestExchange,
    pendingPrefix,
  };
}

export class ConversationHistoryController {
  private agent: Agent;
  private readonly options: Required<Pick<ConversationHistoryControllerOptions,
    'request' | 'yieldToBrowser' | 'isActive'>>;
  private readonly onState?: ConversationHistoryControllerOptions['onState'];
  private readonly onInitialPage?: ConversationHistoryControllerOptions['onInitialPage'];
  private readonly getPreview?: ConversationHistoryControllerOptions['getPreview'];
  private readonly putPreview?: ConversationHistoryControllerOptions['putPreview'];
  private readonly preparationIntervalMs: number;
  private readonly maxPagesPerDemand: number;
  private readonly maxContentBytes: number;

  private stateValue: ConversationHistoryControllerState;
  /** Entries currently rendered; while preview is warm these are preview entries. */
  private rawEntries: ConversationEntry[] = [];
  /** Accepted entries from the current network cursor chain. Never aliases preview data. */
  private networkEntries: ConversationEntry[] = [];
  private previewEntries: ConversationEntry[] = [];
  /** A disjoint fresh head is bridged through its continuation before merging. */
  private bridgeEntries: ConversationEntry[] = [];
  private bridgeActive = false;
  private demandRunning = false;
  private queuedDemand: Demand | null = null;
  private activeAbort: AbortController | null = null;
  /** Cursor for a cursorful refresh/bridge; never replaces the older lane. */
  private headCursor = '';
  private bridgeRestore: OlderContinuation | null = null;
  private bridgePlan: OmoTodoState | null = null;
  private started = false;
  private paused = false;
  private preparationProgressKey = '';
  private seenReadyCursors = new Set<string>();
  private noRawProgressPages = 0;
  private readyPages = 0;
  private rawProgressThisPage = true;
  private initialPageDelivered = false;
  private retryRequested = false;
  private lastDemand: Demand = 'initial';
  private networkSourceRevision = '';
  private liveSourceRevision = '';
  private liveSequence = 0;
  private networkSnapshotId = '';
  private manualPreparationPause = false;

  constructor(agent: Agent, options: ConversationHistoryControllerOptions) {
    this.agent = { ...agent };
    this.options = {
      request: options.request,
      yieldToBrowser: options.yieldToBrowser || yieldToBrowser,
      isActive: options.isActive || (() => true),
    };
    this.onState = options.onState;
    this.onInitialPage = options.onInitialPage;
    this.getPreview = options.getPreview;
    this.putPreview = options.putPreview;
    this.preparationIntervalMs = Math.max(0, options.preparationIntervalMs ?? HISTORY_PREPARATION_INTERVAL_MS);
    this.maxPagesPerDemand = Math.max(1, options.maxPagesPerDemand ?? HISTORY_MAX_PAGES_PER_DEMAND);
    this.maxContentBytes = Math.max(1, options.maxContentBytes ?? HISTORY_MAX_CONTENT_BYTES);
    this.stateValue = initialState(conversationIdentity(this.agent));
  }

  get state(): ConversationHistoryControllerState {
    return cloneState(this.stateValue);
  }

  matchesRequestTarget(agent: Agent): boolean {
    return requestIdentity(this.agent) === requestIdentity(agent);
  }

  start(): void {
    if (this.started) {
      this.setAgent(this.agent);
      return;
    }
    this.started = true;
    this.hydratePreview();
    this.emit();
    this.enqueue('initial');
  }

  setAgent(agent: Agent): void {
    const next = { ...agent };
    const nextRequestIdentity = requestIdentity(next);
    const currentRequestIdentity = requestIdentity(this.agent);
    this.agent = next;
    if (!this.started || nextRequestIdentity === currentRequestIdentity) return;
    this.liveSourceRevision = '';
    this.liveSequence = 0;
    this.advanceEpoch(true);
    this.rawEntries = [];
    this.networkEntries = [];
    this.previewEntries = [];
    this.clearBridge();
    this.headCursor = '';
    this.networkSourceRevision = '';
    this.networkSnapshotId = '';
    this.queuedDemand = null;
    this.manualPreparationPause = false;
    this.paused = false;
    this.initialPageDelivered = false;
    this.hydratePreview();
    this.emit();
    this.enqueue('initial');
  }

  refresh(): void {
    if (!this.started || this.paused || !this.options.isActive() || this.stateValue.error) return;
    this.enqueue('refresh');
  }

  applyLiveEntries(sourceRevision: string, entries: ConversationEntry[], sequence: number): boolean {
    if (!this.started || this.paused || !this.stateValue.authoritative || this.stateValue.error || sourceRevision !== this.stateValue.sourceRevision) return false;
    this.liveSequence = this.liveSourceRevision === sourceRevision ? Math.max(this.liveSequence, sequence) : sequence;
    this.liveSourceRevision = sourceRevision;
    if (!entries.length) return true;
    const merged = mergeFreshEntries(this.rawEntries, cloneEntries(entries));
    if (merged.length > HISTORY_MAX_RAW_ENTRIES || estimateEntriesBytes(merged) > this.maxContentBytes) {
      this.pauseWithError('History and live output reached this device’s memory limit. Loaded messages remain readable.', false, 'memory_limit');
      return false;
    }
    this.rawEntries = merged;
    this.networkEntries = merged;
    this.stateValue.entries = merged;
    this.savePreview();
    this.emit();
    return true;
  }

  /** Load an older range while retaining the independently refreshed live head. */
  demandOlder(): void {
    if (!this.started || this.paused || !this.options.isActive() || this.stateValue.error) return;
    if (this.demandRunning) {
      this.markHistoricalDemand();
      this.queueDemand('older');
      return;
    }
    if (!this.stateValue.nextCursor || !this.stateValue.hasMore) return;
    this.markHistoricalDemand();
    this.enqueue('older');
  }

  retry(): void {
    if (!this.started || this.paused || !this.options.isActive() || !this.stateValue.error?.retryable) return;
    this.stateValue.pausedReason = '';
    this.retryRequested = true;
    this.emit();
    if (this.stateValue.intent === 'historical' && this.stateValue.nextCursor && this.lastDemand === 'older') this.enqueue('older', true);
    else if (this.stateValue.nextCursor && this.lastDemand !== 'refresh') this.enqueue('full', true);
    else this.enqueue(this.rawEntries.length ? 'refresh' : 'initial', true);
  }

  recoverAfterReconnect(agent: Agent): boolean {
    if (!this.matchesRequestTarget(agent) || !this.started || this.paused || !this.options.isActive()
      || this.demandRunning || this.stateValue.pausedReason
      || this.stateValue.error?.code !== 'transport_disconnected' || !this.stateValue.error.retryable) return false;
    this.retry();
    return true;
  }

  returnToLatest(): void {
    if (!this.started || !this.options.isActive()) return;
    this.advanceEpoch(false);
    this.previewEntries = [];
    this.networkEntries = [];
    this.clearBridge();
    this.headCursor = '';
    this.networkSourceRevision = '';
    this.networkSnapshotId = '';
    this.queuedDemand = null;
    this.manualPreparationPause = false;
    const epoch = this.stateValue.epoch;
    const identity = this.stateValue.identity;
    const oldState = this.stateValue;
    this.stateValue = {
      ...initialState(identity),
      epoch,
      entries: cloneEntries(this.rawEntries),
      preview: this.rawEntries.length > 0,
      previewHistorical: oldState.previewHistorical || oldState.intent === 'historical',
      available: true,
      sourceRevision: oldState.sourceRevision,
      omoPlan: oldState.omoPlan ? clonePlan(oldState.omoPlan) : null,
      requestPhase: this.rawEntries.length > 0 ? 'refresh' : 'initial',
    };
    this.emit();
    this.enqueue('initial');
  }

  pause(reason = 'History paused while the app is hidden or locked.'): void {
    if (this.paused) return;
    this.paused = true;
    this.advanceEpoch(true);
    this.queuedDemand = null;
    this.stateValue.requestPhase = 'idle';
    this.stateValue.pausedReason = reason;
    this.emit();
  }

  resume(): void {
    if (!this.paused) return;
    if (this.manualPreparationPause) return;
    this.paused = false;
    this.stateValue.pausedReason = '';
    if (this.stateValue.error) {
      this.emit();
      return;
    }
    this.emit();
    if (this.stateValue.nextCursor && this.stateValue.state === 'preparing') {
      this.enqueue(this.stateValue.intent === 'historical' ? 'older' : 'full');
    } else this.enqueue(this.rawEntries.length ? 'refresh' : 'initial');
  }

  pausePreparation(): void {
    if (this.stateValue.state !== 'preparing' && this.stateValue.requestPhase !== 'preparing') return;
    this.manualPreparationPause = true;
    this.paused = true;
    this.advanceEpoch(true);
    this.queuedDemand = null;
    this.stateValue.requestPhase = 'idle';
    this.stateValue.preparationPolls = HISTORY_MAX_PREPARATION_POLLS;
    this.stateValue.pausedReason = 'Preparation is paused. Continue when you are ready.';
    this.emit();
  }

  continuePreparation(): void {
    const preparationPaused = this.stateValue.error?.code === 'preparation_stalled';
    if ((!this.paused && !preparationPaused) || !this.stateValue.nextCursor) return;
    this.paused = false;
    this.manualPreparationPause = false;
    this.stateValue.pausedReason = '';
    this.stateValue.preparationPolls = 0;
    this.emit();
    this.enqueue(this.stateValue.intent === 'historical' ? 'older' : 'full', true);
  }

  cancel(): void {
    this.liveSourceRevision = '';
    this.liveSequence = 0;
    this.started = false;
    this.manualPreparationPause = false;
    this.paused = true;
    this.advanceEpoch(true);
    this.queuedDemand = null;
    this.stateValue.requestPhase = 'idle';
    this.emit();
  }

  private hydratePreview(): void {
    const identity = conversationIdentity(this.agent);
    const preview = identity ? this.getPreview?.(identity) : null;
    this.previewEntries = preview ? cloneEntries(preview.entries.slice(-HISTORY_WIRE_PAGE_SIZE)) : [];
    this.networkEntries = [];
    this.clearBridge();
    this.headCursor = '';
    this.networkSourceRevision = '';
    this.networkSnapshotId = '';
    this.manualPreparationPause = false;
    this.rawEntries = this.previewEntries;
    this.stateValue = {
      ...initialState(identity),
      epoch: this.stateValue.epoch,
      identity,
      entries: cloneEntries(this.rawEntries),
      preview: Boolean(preview),
      previewHistorical: preview?.historical === true,
      available: true,
      sourceRevision: preview?.sourceRevision || '',
      diagnostics: preview?.diagnostics ? { ...preview.diagnostics } : undefined,
      omoPlan: preview?.plan ? clonePlan(preview.plan) : null,
    };
  }

  private enqueue(demand: Demand, allowError = false): void {
    if (this.paused || (this.stateValue.error && !allowError) || !this.options.isActive()) return;
    if (this.demandRunning) {
      this.queueDemand(demand);
      return;
    }
    void this.runDemand(demand);
  }

  private queueDemand(demand: Demand): void {
    if (demand === 'older' || this.queuedDemand !== 'older') this.queuedDemand = demand;
  }

  private markHistoricalDemand(): void {
    if (this.stateValue.intent === 'historical') return;
    this.stateValue.intent = 'historical';
    this.emit();
  }

  private async runDemand(demand: Demand): Promise<void> {
    if (this.demandRunning || this.paused || !this.options.isActive()) return;
    this.demandRunning = true;
    this.lastDemand = demand;
    if (demand === 'refresh') this.headCursor = '';
    this.seenReadyCursors.clear();
    this.noRawProgressPages = 0;
    this.preparationProgressKey = '';
    this.readyPages = 0;
    const demandEpoch = this.stateValue.epoch;
    const demandStartedAt = Date.now();
    this.stateValue.requestPhase = demand === 'initial' ? 'initial'
      : demand === 'refresh' ? 'refresh'
        : demand === 'older' ? 'older' : 'initial';
    this.stateValue.pausedReason = '';
    this.emit();
    const abort = new AbortController();
    this.activeAbort = abort;
    try {
      for (let pageNumber = 0; pageNumber < this.maxPagesPerDemand; pageNumber++) {
        if (!this.canContinue(demandEpoch, abort)) return;
        if (Date.now() - demandStartedAt >= HISTORY_ACTIVE_DEADLINE_MS) {
          const error = workDeadlineError();
          this.pauseWithError(error.message, error.retryable, error.code);
          return;
        }
        const requestedCursor = demand === 'initial'
          ? (pageNumber === 0 ? '' : this.stateValue.nextCursor)
          : demand === 'refresh'
            ? (pageNumber === 0 ? '' : this.headCursor)
            : this.stateValue.nextCursor;
        if (demand !== 'initial' && demand !== 'refresh' && !requestedCursor) {
          this.finishDemand();
          return;
        }
        const retry = pageNumber === 0 && this.retryRequested;
        if (pageNumber === 0) this.retryRequested = false;
        const page = await this.requestPage(demandEpoch, demandStartedAt, abort, {
          ...(requestedCursor ? { cursor: requestedCursor } : {}),
          limit: HISTORY_WIRE_PAGE_SIZE,
          ...(retry ? { retry: true } : {}),
          signal: abort.signal,
        });
        if (!this.canContinue(demandEpoch, abort)) return;
        const result = this.acceptPage(page, demand, requestedCursor);
        if (!result.accepted) return;
        if (page.state === 'preparing') {
          if (!this.advancePreparation(page, demandEpoch, abort)) return;
          await this.wait(this.preparationIntervalMs, demandEpoch, abort, demandStartedAt);
          continue;
        }
        if (page.state === 'failed' || page.error) return;
        this.stateValue.requestPhase = demand === 'older' ? 'older' : 'idle';
        this.readyPages++;
        this.emit();
        if (requestedCursor && page.nextCursor === requestedCursor && !this.rawProgressThisPage) {
          this.pauseWithError('History loading stalled on a repeated cursor.', false, 'stalled');
          return;
        }
        if (!this.bridgeActive) {
          this.finishDemand();
          return;
        }
        const continuationCursor = demand === 'refresh' ? this.headCursor : this.stateValue.nextCursor;
        if (!continuationCursor || (demand !== 'refresh' && !this.stateValue.hasMore)) {
          this.finishDemand();
          return;
        }
        if (this.seenReadyCursors.has(continuationCursor)) {
          this.pauseWithError('History loading stalled on a repeated cursor.', false, 'stalled');
          return;
        }
        this.seenReadyCursors.add(continuationCursor);
        if (!this.rawProgressThisPage) {
          this.noRawProgressPages++;
          if (this.noRawProgressPages >= HISTORY_MAX_NO_RAW_PROGRESS_PAGES) {
            this.pauseWithError('History loading stalled without finding more records. Continue to retry.', true, 'stalled');
            return;
          }
        } else {
          this.noRawProgressPages = 0;
        }
        await this.options.yieldToBrowser();
        if (this.readyPages % HISTORY_BREATHER_INTERVAL_PAGES === 0) {
          await this.wait(HISTORY_BREATHER_MS, demandEpoch, abort, demandStartedAt);
        }
      }
      this.pauseWithError('History loading was paused after too many pages. Continue to load more.', true, 'work_limit');
    } catch (failure) {
      if (demandEpoch !== this.stateValue.epoch || isAbortFailure(failure)) return;
      const error = conversationError(failure, 'Conversation history could not be loaded.');
      this.stateValue.error = error;
      this.stateValue.requestPhase = 'idle';
      this.stateValue.pausedReason = error.retryable ? '' : error.message;
      this.queuedDemand = null;
      this.emit();
    } finally {
      if (this.activeAbort === abort) this.activeAbort = null;
      if (demandEpoch === this.stateValue.epoch) {
        this.demandRunning = false;
        if (this.stateValue.requestPhase === 'initial' || this.stateValue.requestPhase === 'refresh') {
          this.stateValue.requestPhase = 'idle';
          this.emit();
        }
      }
      const queued = this.queuedDemand;
      this.queuedDemand = null;
      if (queued && this.started && !this.paused && this.options.isActive()) this.enqueue(queued);
    }
  }

  private captureOlderContinuation(): OlderContinuation {
    return {
      nextCursor: this.stateValue.nextCursor,
      hasMore: this.stateValue.hasMore,
      snapshotId: this.networkSnapshotId,
      beginningReached: this.stateValue.beginningReached,
      omoPlan: this.stateValue.omoPlan ? clonePlan(this.stateValue.omoPlan) : null,
      ...(this.stateValue.diagnostics ? { diagnostics: cloneDiagnostics(this.stateValue.diagnostics) } : {}),
    };
  }

  private restoreOlderContinuation(value: OlderContinuation, restoreNetworkSnapshot = true, restorePlan = true, restoreDiagnostics = true): void {
    this.stateValue.nextCursor = value.nextCursor;
    this.stateValue.hasMore = value.hasMore;
    this.stateValue.snapshotId = value.snapshotId;
    this.stateValue.beginningReached = value.beginningReached;
    if (restorePlan) this.stateValue.omoPlan = value.omoPlan ? clonePlan(value.omoPlan) : null;
    if (restoreDiagnostics) this.stateValue.diagnostics = value.diagnostics ? cloneDiagnostics(value.diagnostics) : undefined;
    if (restoreNetworkSnapshot) this.networkSnapshotId = value.snapshotId;
  }

  private acceptPage(page: ConversationPage, demand: Demand, requestedCursor: string): { accepted: boolean } {
    const previewWasVisible = this.stateValue.preview && !this.stateValue.authoritative;
    const previousSource = this.networkSourceRevision || (previewWasVisible ? '' : this.stateValue.sourceRevision);
    const stateBefore = cloneState(this.stateValue);
    const rawBefore = this.rawEntries;
    const networkBefore = this.networkEntries;
    const bridgeBefore = this.bridgeEntries;
    const bridgeRestoreBefore = this.bridgeRestore;
    const bridgePlanBefore = this.bridgePlan ? clonePlan(this.bridgePlan) : null;
    const bridgeWasActive = this.bridgeActive;
    const sourceBefore = this.networkSourceRevision;
    const snapshotBefore = this.networkSnapshotId;
    const headCursorBefore = this.headCursor;
    const planBefore = this.stateValue.omoPlan ? clonePlan(this.stateValue.omoPlan) : null;
    const isFreshHead = (demand === 'initial' || demand === 'refresh') && !requestedCursor;
    const preserveOlderContinuation = isFreshHead
      && demand === 'refresh'
      && !previewWasVisible
      && this.stateValue.authoritative
      && (this.rawEntries.length > 0 || Boolean(this.stateValue.nextCursor) || this.stateValue.beginningReached);
    const olderContinuationBefore = preserveOlderContinuation ? this.captureOlderContinuation() : null;
    const pageState = page.state || 'ready';
    if (requestedCursor && previousSource && page.sourceRevision && previousSource !== page.sourceRevision) {
      return this.rejectPage('source_changed', 'The conversation source changed while history was being browsed.', false, requestedCursor);
    }
    if (requestedCursor && this.networkSnapshotId) {
      if (page.mode !== 'snapshot' || !page.snapshotId || this.networkSnapshotId !== page.snapshotId) {
        return this.rejectPage('source_changed', 'The conversation snapshot changed while history was being browsed.', false, requestedCursor);
      }
    }
    if (isFreshHead && demand === 'refresh' && !previewWasVisible
      && previousSource && page.sourceRevision && previousSource !== page.sourceRevision) {
      return this.rejectPage('source_changed', 'The conversation source changed while history was being refreshed.', false, requestedCursor);
    }
    if (page.mode === 'snapshot' && !page.snapshotId) {
      return this.rejectPage('invalid_page', 'Conversation history returned an incomplete snapshot identity.', false, requestedCursor);
    }
    if (!isFreshHead && demand === 'refresh' && previousSource && page.sourceRevision && previousSource !== page.sourceRevision) {
      return this.rejectPage('source_changed', 'The conversation source changed while history was being refreshed.', false, requestedCursor);
    }
    if (page.sourceRevision === this.liveSourceRevision && page.sequence !== undefined && page.sequence < this.liveSequence) {
      const currentEntries = new Map(this.rawEntries.map(entry => [entry.id, entry]));
      page = { ...page, entries: page.entries.map(entry => currentEntries.get(entry.id) || entry) };
    }
    this.stateValue.progress = page.progress;
    this.stateValue.reason = page.reason || (isFreshHead ? '' : this.stateValue.reason);
    this.stateValue.sourceRevision = page.sourceRevision || this.stateValue.sourceRevision;
    if (page.snapshotId && !preserveOlderContinuation) this.stateValue.snapshotId = page.snapshotId;
    if (isFreshHead && page.mode && page.mode !== 'snapshot' && !preserveOlderContinuation) this.stateValue.snapshotId = '';
    this.stateValue.state = pageState;
    // A cursorless latest response is authoritative for the live head. Keep
    // the older cursor lane separately, but do not carry its continuation
    // warning into a successful refresh that has rediscovered the child.
    this.stateValue.diagnostics = isFreshHead
      ? cloneDiagnostics(page.diagnostics)
      : mergeDiagnostics(this.stateValue.diagnostics, page.diagnostics);
    if (page.omoPlan && !bridgeWasActive) this.stateValue.omoPlan = clonePlan(page.omoPlan);

    if (page.error || pageState === 'failed') {
      this.stateValue.error = page.error || {
        code: 'history_failed',
        message: page.reason || 'Conversation history could not be loaded.',
        retryable: false,
      };
      this.stateValue.requestPhase = 'idle';
      if (olderContinuationBefore) this.restoreOlderContinuation(olderContinuationBefore);
      else if (!this.restoreBridgeAfterFailure()) {
        this.stateValue.nextCursor = page.nextCursor || requestedCursor || this.stateValue.nextCursor;
        this.stateValue.hasMore = Boolean(this.stateValue.nextCursor) || page.hasMore;
      }
      this.stateValue.contextSearching = false;
      this.emit();
      return { accepted: false };
    }

    if (pageState !== 'preparing' && page.hasMore && !page.nextCursor) {
      return this.rejectPage('invalid_page', 'Conversation history returned an incomplete continuation.', false, requestedCursor);
    }

    if (demand === 'initial' && !requestedCursor && pageState !== 'preparing' && !this.initialPageDelivered) {
      this.initialPageDelivered = true;
      this.onInitialPage?.(clonePage(page));
    }

    if (!page.available) {
      // A cursorless result is the only result allowed to replace a warm
      // preview with an authoritative unavailable state. A continuation that
      // disappears keeps the accepted window and exposes a recoverable error.
      if (requestedCursor || demand === 'older' || demand === 'full') {
        this.stateValue.error = page.error || {
          code: page.reasonCode || 'history_unavailable',
          message: page.reason || 'Older conversation history is unavailable.',
          retryable: true,
        };
        this.stateValue.requestPhase = 'idle';
        this.stateValue.contextSearching = false;
        if (!this.restoreBridgeAfterFailure()) {
          this.stateValue.nextCursor = page.nextCursor || requestedCursor || this.stateValue.nextCursor;
          this.stateValue.hasMore = Boolean(this.stateValue.nextCursor) || page.hasMore;
        }
        this.emit();
        return { accepted: false };
      }
      this.rawEntries = [];
      this.networkEntries = [];
      this.previewEntries = [];
      this.clearBridge();
      this.headCursor = '';
      this.networkSourceRevision = '';
      this.networkSnapshotId = '';
      this.stateValue.error = null;
      this.stateValue.entries = [];
      this.stateValue.available = false;
      this.stateValue.authoritative = true;
      this.stateValue.preview = false;
      this.stateValue.previewHistorical = false;
      this.stateValue.hasMore = false;
      this.stateValue.nextCursor = '';
      this.stateValue.beginningReached = false;
      this.stateValue.contextSearching = false;
      this.stateValue.pendingPrefix = [];
      this.emit();
      return { accepted: true };
    }

    const incoming = cloneEntries(page.entries);
    const laneBefore = cloneEntries(this.networkEntries);
    if (isFreshHead) {
      if (page.sourceRevision) this.networkSourceRevision = page.sourceRevision;
      else if (!preserveOlderContinuation) this.networkSourceRevision = '';
      if (!preserveOlderContinuation) {
        this.networkSnapshotId = page.mode === 'snapshot' ? page.snapshotId || '' : '';
      }
    } else {
      if (page.sourceRevision && !this.networkSourceRevision) this.networkSourceRevision = page.sourceRevision;
      if (!page.mode || page.mode !== 'snapshot') this.networkSnapshotId = '';
      else if (page.snapshotId) this.networkSnapshotId = page.snapshotId;
    }
    if (demand === 'refresh') this.headCursor = page.nextCursor || requestedCursor || this.headCursor;
    if (pageState === 'preparing') {
      // Preparation pages are status, not content. An empty preparation page
      // must not erase a warm preview or replace the fresh lane with nothing.
      if (olderContinuationBefore) this.restoreOlderContinuation(olderContinuationBefore, false, true, false);
      else {
        this.stateValue.nextCursor = page.nextCursor || requestedCursor || this.stateValue.nextCursor;
        this.stateValue.hasMore = Boolean(page.hasMore || this.stateValue.nextCursor);
      }
      this.stateValue.error = null;
      this.stateValue.available = true;
      this.stateValue.contextSearching = true;
      this.stateValue.pendingPrefix = [];
      this.stateValue.entries = cloneEntries(this.rawEntries);
      this.stateValue.authoritative = !previewWasVisible && this.rawEntries.length > 0;
      this.stateValue.preview = previewWasVisible;
      this.rawProgressThisPage = false;
      this.emit();
      return { accepted: true };
    }
    let replacedWindow = false;
    if (isFreshHead) {
      if (previewWasVisible) {
        // Never merge a fresh page into cache-owned content merely because a
        // source revision or entry ID happens to match. The fresh lane is
        // promoted only after it provides usable/authoritatively empty data.
        this.networkEntries = incoming;
        this.bridgeEntries = [];
        this.bridgeActive = false;
        this.stateValue.latestGapOutstanding = false;
      } else if (demand === 'refresh' && this.rawEntries.length > 0 && !sharedEntryId(this.rawEntries, incoming)) {
        this.bridgeRestore = olderContinuationBefore;
        this.bridgePlan = page.omoPlan ? clonePlan(page.omoPlan) : planBefore;
        this.bridgeEntries = incoming;
        this.networkEntries = this.bridgeEntries;
        this.bridgeActive = Boolean(page.hasMore || page.nextCursor);
        this.stateValue.latestGapOutstanding = this.bridgeActive;
        // A bridge uses the fresh head's cursor and snapshot cohort. The
        // established older lane is restored after overlap is proven.
        this.networkSnapshotId = page.mode === 'snapshot' ? page.snapshotId || '' : '';
        if (!this.bridgeActive) {
          this.rawEntries = cloneEntries(this.bridgeEntries);
          this.networkEntries = this.rawEntries;
          this.bridgeEntries = [];
          this.bridgeRestore = null;
          this.stateValue.latestGapOutstanding = false;
          replacedWindow = true;
        }
      } else if (demand === 'refresh' && this.rawEntries.length > 0) {
        this.rawEntries = mergeFreshEntries(this.rawEntries, incoming);
        this.networkEntries = this.rawEntries;
        this.bridgeEntries = [];
        this.bridgeActive = false;
        this.stateValue.latestGapOutstanding = false;
      } else {
        this.rawEntries = incoming;
        this.networkEntries = this.rawEntries;
        this.bridgeEntries = [];
        this.bridgeActive = false;
        this.stateValue.latestGapOutstanding = false;
      }
    } else if (this.bridgeActive) {
      this.bridgeEntries = mergeOlderEntries(this.bridgeEntries, incoming);
      this.networkEntries = this.bridgeEntries;
      if (sharedEntryId(this.rawEntries, this.bridgeEntries)) {
        this.rawEntries = mergeBridgedEntries(this.rawEntries, this.bridgeEntries);
        this.networkEntries = this.rawEntries;
        this.bridgeEntries = [];
        this.bridgeActive = false;
        this.stateValue.latestGapOutstanding = false;
      } else if (!page.hasMore && !page.nextCursor) {
        // The new chain reached source start without overlap. It is safe to
        // replace the old window, but never mix the two disconnected chains.
        this.rawEntries = this.bridgeEntries;
        this.networkEntries = this.rawEntries;
        this.bridgeEntries = [];
        this.bridgeRestore = null;
        this.bridgeActive = false;
        this.stateValue.latestGapOutstanding = false;
        replacedWindow = true;
      }
    } else {
      this.networkEntries = previewWasVisible
        ? mergeOlderEntries(this.networkEntries, incoming)
        : mergeOlderEntries(this.rawEntries, incoming);
      if (!previewWasVisible) this.rawEntries = this.networkEntries;
    }
    const preservedOlderLane = olderContinuationBefore || this.bridgeRestore;
    if (preservedOlderLane && !replacedWindow) {
      // A fresh head may use a different cursor chain. Keep the accepted older
      // boundary visible and usable while that head is merged or bridged.
      this.restoreOlderContinuation(preservedOlderLane, !this.bridgeActive, false, false);
      if (!this.bridgeActive) this.bridgeRestore = null;
    } else {
      this.stateValue.nextCursor = page.nextCursor || '';
      this.stateValue.hasMore = Boolean(page.hasMore || this.stateValue.nextCursor);
      const acceptedDiagnostics = this.stateValue.diagnostics;
      this.stateValue.beginningReached = pageState === 'ready'
        && !page.error
        && !page.hasMore
        && !page.nextCursor
        && !acceptedDiagnostics?.continuation_incomplete
        && (!acceptedDiagnostics?.source_truncated || page.mode === 'snapshot');
    }
    if (page.mode === 'snapshot' && !page.diagnostics?.source_truncated && this.stateValue.diagnostics) {
      this.stateValue.diagnostics = { ...this.stateValue.diagnostics, source_truncated: false };
    }
    const lane = this.networkEntries;
    const analysis = analyzeConversationBatch(lane, this.stateValue.hasMore, this.stateValue.beginningReached);
    const freshUsable = !this.bridgeActive && (!previewWasVisible
      || lane.length > 0
      || (this.stateValue.beginningReached && !page.diagnostics?.source_truncated));
    if (previewWasVisible && !freshUsable && !page.hasMore && !page.nextCursor) {
      this.pauseWithError('History loading ended before the current conversation could be verified. Reload to try again.', true, 'incomplete_history');
      return { accepted: false };
    }
    if (previewWasVisible && freshUsable) {
      this.rawEntries = cloneEntries(this.networkEntries);
      this.previewEntries = [];
      this.networkEntries = this.rawEntries;
      this.stateValue.preview = false;
      this.stateValue.previewHistorical = false;
      this.stateValue.authoritative = true;
    } else if (previewWasVisible) {
      this.stateValue.preview = true;
      this.stateValue.authoritative = false;
    } else {
      this.stateValue.preview = false;
      this.stateValue.previewHistorical = false;
      this.stateValue.authoritative = true;
    }
    if (bridgeWasActive || this.bridgeActive) {
      this.stateValue.omoPlan = this.bridgePlan ? clonePlan(this.bridgePlan) : null;
    }
    if (!this.bridgeActive) this.bridgePlan = null;
    this.stateValue.entries = cloneEntries(this.rawEntries);
    this.stateValue.available = true;
    this.stateValue.pendingPrefix = cloneEntries(analysis.pendingPrefix);
    this.stateValue.contextSearching = this.bridgeActive;
    this.stateValue.error = null;
    this.stateValue.preparationPolls = 0;
    this.rawProgressThisPage = !sameEntryIDs(laneBefore, this.networkEntries);

    if (retainedEntryCount(this.rawEntries, this.networkEntries, this.bridgeEntries) > HISTORY_MAX_RAW_ENTRIES
      || retainedEntryBytes(this.rawEntries, this.networkEntries, this.bridgeEntries) > this.maxContentBytes) {
      this.rawEntries = rawBefore;
      this.networkEntries = networkBefore;
      this.bridgeEntries = bridgeBefore;
      this.bridgeRestore = bridgeRestoreBefore;
      this.bridgePlan = bridgePlanBefore;
      this.networkSourceRevision = sourceBefore;
      this.networkSnapshotId = snapshotBefore;
      this.headCursor = headCursorBefore;
      this.stateValue = stateBefore;
      this.pauseWithError('History is too large to load further on this device. Loaded messages are still available.', false, 'memory_limit');
      return { accepted: false };
    }
    if (!this.bridgeActive) this.savePreview();
    this.emit();
    return { accepted: true };
  }

  private restoreBridgeAfterFailure(): boolean {
    const bridgedOlderLane = this.bridgeRestore;
    if (!bridgedOlderLane) return false;
    this.bridgeEntries = [];
    this.networkEntries = this.rawEntries;
    this.bridgeActive = false;
    this.bridgeRestore = null;
    this.bridgePlan = null;
    this.restoreOlderContinuation(bridgedOlderLane);
    this.stateValue.latestGapOutstanding = false;
    return true;
  }

  private rejectPage(code: string, message: string, retryable: boolean, requestedCursor: string): { accepted: boolean } {
    if (!this.restoreBridgeAfterFailure()) {
      this.stateValue.nextCursor = requestedCursor || this.stateValue.nextCursor;
      this.stateValue.hasMore = Boolean(this.stateValue.nextCursor);
    }
    this.stateValue.error = { code, message, retryable };
    this.stateValue.requestPhase = 'idle';
    this.stateValue.contextSearching = false;
    this.stateValue.beginningReached = false;
    this.queuedDemand = null;
    this.emit();
    return { accepted: false };
  }

  private advancePreparation(page: ConversationPage, demandEpoch: number, abort: AbortController): boolean {
    const progressKey = JSON.stringify(page.progress || {});
    this.stateValue.requestPhase = 'preparing';
    this.stateValue.contextSearching = true;
    this.stateValue.preparationPolls++;
    if (progressKey !== this.preparationProgressKey) {
      this.preparationProgressKey = progressKey;
      this.stateValue.preparationPolls = 1;
    }
    this.emit();
    if (this.stateValue.preparationPolls >= HISTORY_MAX_PREPARATION_POLLS) {
      this.pauseWithError('Preparation is paused because progress has not changed. Continue to retry.', true, 'preparation_stalled');
      return false;
    }
    return this.canContinue(demandEpoch, abort);
  }

  private finishDemand(): void {
    this.stateValue.requestPhase = 'idle';
    this.stateValue.contextSearching = false;
    this.stateValue.pausedReason = '';
    this.emit();
  }

  private pauseWithError(message: string, retryable: boolean, code: string): void {
    this.stateValue.error = { code, message, retryable };
    this.stateValue.pausedReason = message;
    this.stateValue.requestPhase = 'idle';
    this.stateValue.contextSearching = false;
    this.queuedDemand = null;
    this.emit();
  }

  private canContinue(epoch: number, abort: AbortController): boolean {
    return this.started && !this.paused && epoch === this.stateValue.epoch
      && !abort.signal.aborted && this.options.isActive();
  }

  private async requestPage(
    epoch: number,
    startedAt: number,
    abort: AbortController,
    request: ConversationHistoryRequest,
  ): Promise<ConversationPage> {
    const remaining = HISTORY_ACTIVE_DEADLINE_MS - (Date.now() - startedAt);
    if (remaining <= 0) {
      throw { error: workDeadlineError() };
    }
    let timer: ReturnType<typeof setTimeout> | undefined;
    let timedOut = false;
    const timeout = new Promise<ConversationPage>((_, reject) => {
      timer = setTimeout(() => {
        timedOut = true;
        abort.abort();
        reject({ error: workDeadlineError() });
      }, remaining);
    });
    try {
      return await Promise.race([this.options.request({ ...this.agent }, request), timeout]);
    } catch (failure) {
      if (timedOut) {
        throw { error: workDeadlineError() };
      }
      throw failure;
    } finally {
      if (timer !== undefined) clearTimeout(timer);
      if (epoch !== this.stateValue.epoch || this.paused) abort.abort();
    }
  }

  private async wait(milliseconds: number, epoch: number, abort: AbortController, startedAt?: number): Promise<void> {
    if (milliseconds <= 0) return;
    if (startedAt !== undefined) {
      const remaining = HISTORY_ACTIVE_DEADLINE_MS - (Date.now() - startedAt);
      if (remaining <= 0) {
        throw { error: workDeadlineError() };
      }
      milliseconds = Math.min(milliseconds, remaining);
    }
    await new Promise<void>((resolve, reject) => {
      const timer = setTimeout(resolve, milliseconds);
      const cancel = () => {
        clearTimeout(timer);
        reject(new DOMException('History request was cancelled.', 'AbortError'));
      };
      abort.signal.addEventListener('abort', cancel, { once: true });
      if (epoch !== this.stateValue.epoch || this.paused) cancel();
    });
  }

  private clearBridge(): void {
    this.bridgeEntries = [];
    this.bridgeActive = false;
    this.bridgeRestore = null;
    this.bridgePlan = null;
  }

  private advanceEpoch(retainPaused = false): void {
    this.stateValue.epoch++;
    this.activeAbort?.abort();
    this.activeAbort = null;
    this.headCursor = '';
    this.demandRunning = false;
    if (!retainPaused) this.paused = false;
  }

  private savePreview(): void {
    const identity = this.stateValue.identity;
    if (!identity || !this.putPreview || !this.rawEntries.length || !this.stateValue.authoritative) return;
    this.putPreview({
      identity,
      relayId: this.agent.relay_id,
      savedAt: Date.now(),
      sourceRevision: this.stateValue.sourceRevision,
      historical: this.stateValue.intent === 'historical',
      ...(this.stateValue.diagnostics ? { diagnostics: { ...this.stateValue.diagnostics } } : {}),
      plan: this.stateValue.omoPlan ? clonePlan(this.stateValue.omoPlan) : null,
      entries: cloneEntries(this.rawEntries),
    });
  }

  private emit(): void {
    this.stateValue.entries = cloneEntries(this.rawEntries);
    this.onState?.(cloneState(this.stateValue));
  }
}

function initialState(identity: string | null): ConversationHistoryControllerState {
  return {
    epoch: 0,
    identity,
    entries: [],
    preview: false,
    previewHistorical: false,
    authoritative: false,
    available: true,
    reason: '',
    requestPhase: 'idle',
    intent: 'live',
    state: 'ready',
    sourceRevision: '',
    snapshotId: '',
    hasMore: false,
    nextCursor: '',
    omoPlan: null,
    beginningReached: false,
    contextSearching: false,
    latestGapOutstanding: false,
    preparationPolls: 0,
    pausedReason: '',
    error: null,
    pendingPrefix: [],
  };
}

function requestIdentity(agent: Agent): string {
  const target = targetRefForAgent(agent);
  const targetKey = (target ? targetStoreKey(target) : null) || JSON.stringify([
    agent.relay_id, agent.server_session_id, agent.raw_pane_id, agent.terminal_id, agent.generation, agent.agent_session_id || '',
  ]);
  return JSON.stringify([targetKey, normalizedProvider(agent.agent), String(agent.cwd || '').trim(), String(agent.agent_session_id || '').trim()]);
}

function normalizedProvider(value: unknown): string {
  return String(value || '').trim().toLocaleLowerCase().replace(/[\s_-]+/gu, '');
}

function sameEntryIDs(first: ConversationEntry[], second: ConversationEntry[]): boolean {
  if (first.length !== second.length) return false;
  return first.every((entry, index) => entry.id === second[index]?.id);
}

function retainedEntries(...groups: ConversationEntry[][]): ConversationEntry[] {
  const seen = new Set<ConversationEntry>();
  const retained: ConversationEntry[] = [];
  for (const group of groups) {
    for (const entry of group) {
      if (seen.has(entry)) continue;
      seen.add(entry);
      retained.push(entry);
    }
  }
  return retained;
}

function retainedEntryCount(...groups: ConversationEntry[][]): number {
  return retainedEntries(...groups).length;
}

function retainedEntryBytes(...groups: ConversationEntry[][]): number {
  return estimateEntriesBytes(retainedEntries(...groups));
}

function mergeOlderEntries(existing: ConversationEntry[], older: ConversationEntry[]): ConversationEntry[] {
  const existingIds = new Set(existing.map((entry) => entry.id));
  return [...older.filter((entry) => !existingIds.has(entry.id)), ...existing];
}

function mergeFreshEntries(existing: ConversationEntry[], fresh: ConversationEntry[]): ConversationEntry[] {
  const freshById = new Map(fresh.map((entry) => [entry.id, entry]));
  return existing.map((entry) => freshById.get(entry.id) || entry)
    .concat(fresh.filter((entry) => !existing.some((old) => old.id === entry.id)));
}

function mergeBridgedEntries(existing: ConversationEntry[], bridge: ConversationEntry[]): ConversationEntry[] {
  const existingIndex = new Map(existing.map((entry, index) => [entry.id, index]));
  const bridgeAnchor = bridge.findIndex((entry) => existingIndex.has(entry.id));
  if (bridgeAnchor < 0) return cloneEntries(bridge);
  const existingAnchor = existingIndex.get(bridge[bridgeAnchor].id)!;
  const ordered = [
    ...existing.slice(0, existingAnchor),
    ...bridge.slice(0, bridgeAnchor + 1),
    ...existing.slice(existingAnchor + 1),
    ...bridge.slice(bridgeAnchor + 1),
  ];
  const seen = new Set<string>();
  return ordered.filter((entry) => {
    if (seen.has(entry.id)) return false;
    seen.add(entry.id);
    return true;
  });
}

function sharedEntryId(first: ConversationEntry[], second: ConversationEntry[]): boolean {
  const ids = new Set(first.map((entry) => entry.id));
  return second.some((entry) => ids.has(entry.id));
}

function mergeDiagnostics(
  previous: ConversationBrowseDiagnostics | undefined,
  next: ConversationBrowseDiagnostics | undefined,
): ConversationBrowseDiagnostics | undefined {
  if (!previous && !next) return undefined;
  return {
    oversized_records: Math.max(previous?.oversized_records || 0, next?.oversized_records || 0),
    corrupt_records: Math.max(previous?.corrupt_records || 0, next?.corrupt_records || 0),
    omitted_tools: Math.max(previous?.omitted_tools || 0, next?.omitted_tools || 0),
    omitted_payloads: Math.max(previous?.omitted_payloads || 0, next?.omitted_payloads || 0),
    plan_corrupt: Boolean(previous?.plan_corrupt || next?.plan_corrupt),
    source_truncated: Boolean(previous?.source_truncated || next?.source_truncated),
    continuation_incomplete: Boolean(previous?.continuation_incomplete || next?.continuation_incomplete),
    ...(next?.continuation_reason || previous?.continuation_reason
      ? { continuation_reason: next?.continuation_reason || previous?.continuation_reason } : {}),
  };
}

function clonePage(page: ConversationPage): ConversationPage {
  return {
    ...page,
    provisional: undefined,
    entries: cloneEntries(page.entries),
    ...(page.progress ? { progress: { ...page.progress } } : {}),
    ...(page.diagnostics ? { diagnostics: { ...page.diagnostics } } : {}),
    ...(page.error ? { error: { ...page.error } } : {}),
    ...(page.omoPlan ? { omoPlan: clonePlan(page.omoPlan) } : {}),
  };
}

function cloneState(state: ConversationHistoryControllerState): ConversationHistoryControllerState {
  return {
    ...state,
    entries: cloneEntries(state.entries),
    ...(state.progress ? { progress: { ...state.progress } } : {}),
    ...(state.diagnostics ? { diagnostics: { ...state.diagnostics } } : {}),
    ...(state.error ? { error: { ...state.error } } : {}),
    ...(state.omoPlan ? { omoPlan: clonePlan(state.omoPlan) } : {}),
  };
}

function workDeadlineError(): ConversationBrowseError {
  return { code: 'work_deadline', message: 'History loading was paused after two minutes of active work. Continue to load more.', retryable: true };
}

function conversationError(value: unknown, fallback: string): ConversationBrowseError {
  if (value && typeof value === 'object' && 'error' in value) {
    const error = (value as { error?: unknown }).error;
    if (error && typeof error === 'object') {
      const candidate = error as Partial<ConversationBrowseError>;
      if (typeof candidate.message === 'string') {
        return {
          code: typeof candidate.code === 'string' ? candidate.code : 'history_failed',
          message: candidate.message,
          retryable: candidate.retryable !== false,
        };
      }
    }
  }
  return {
    code: 'history_failed',
    message: value instanceof Error && value.message ? value.message : fallback,
    retryable: true,
  };
}

function isAbortFailure(value: unknown): boolean {
  return value instanceof DOMException && value.name === 'AbortError'
    || Boolean(value && typeof value === 'object' && 'code' in value && (value as { code?: string }).code === 'request_cancelled');
}

function yieldToBrowser(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}
