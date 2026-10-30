import { conversationIdentity } from './conversation-history';
import type { Agent, ConversationBatch, ConversationCapabilities, ConversationEntry, ConversationPage } from './types';

export const LIVE_MAX_BYTES = 1024 * 1024;
export const LIVE_MAX_ENTRIES = 16;
export type LiveReaderState = 'synchronizing' | 'live' | 'recorded' | 'disconnected' | 'incomplete' | 'paused' | 'unavailable';
export interface LiveReaderOptions {
  request: (agent: Agent, revision: string, after: number, signal: AbortSignal, subscriptionId: string) => Promise<ConversationBatch>;
  unsubscribe: (agent: Agent, subscriptionId: string) => void;
  apply: (revision: string, entries: ConversationEntry[], sequence: number) => boolean;
  reset: () => void;
  onState: (state: LiveReaderState, entries: ConversationEntry[], capabilities?: ConversationCapabilities) => void;
}

export class ConversationLiveReader {
  private identity = '';
  private agent: Agent | null = null;
  private revision = '';
  private subscription = '';
  private sequence = 0;
  private epoch = 0;
  private abort: AbortController | null = null;
  private tail = new Map<string, ConversationEntry>();
  private capabilities?: ConversationCapabilities;
  private retry: ReturnType<typeof setTimeout> | undefined;
  private active = true;

  constructor(private readonly options: LiveReaderOptions) {}

  get observing(): boolean { return Boolean(this.identity && this.abort); }

  observe(agent: Agent, page: ConversationPage): void {
    if (!this.active || !page.available || page.state !== 'ready' || !page.capabilities || !page.sourceRevision || !page.subscriptionId) return;
    const identity = `${conversationIdentity(agent)}:${page.sourceRevision}:${page.subscriptionId}`;
    if (identity === this.identity && this.abort && (page.sequence || 0) <= this.sequence) return;
    this.stop(false);
    this.identity = identity;
    this.agent = { ...agent };
    this.revision = page.sourceRevision;
    this.subscription = page.subscriptionId;
    this.sequence = page.sequence || 0;
    this.capabilities = page.capabilities;
    this.tail = new Map((page.provisional || []).map(entry => [entry.id, entry]));
    this.abort = new AbortController();
    this.options.onState('synchronizing', [...this.tail.values()], this.capabilities);
    void this.run(this.epoch, this.abort);
    this.publish(page.liveState || 'ready');
  }

  pause(): void {
    this.active = false;
    this.stop();
    this.options.onState('paused', [], this.capabilities);
  }

  resume(): void { this.active = true; }

  stop(unsubscribe = true): void {
    this.epoch++;
    this.abort?.abort();
    this.abort = null;
    if (unsubscribe && this.agent) this.options.unsubscribe(this.agent, this.subscription);
    this.agent = null;
    this.identity = '';
    this.tail.clear();
    clearTimeout(this.retry);
    this.retry = undefined;
  }

  private async run(epoch: number, abort: AbortController): Promise<void> {
    while (this.active && this.agent && epoch === this.epoch && !abort.signal.aborted) {
      try {
        const batch = await this.options.request(this.agent, this.revision, this.sequence, abort.signal, this.subscription);
        if (epoch !== this.epoch || abort.signal.aborted) return;
        if (batch.subscriptionId !== this.subscription || batch.kind === 'reset' || batch.sourceRevision !== this.revision || batch.sequence < this.sequence
          || (batch.kind !== 'heartbeat' && batch.sequence !== this.sequence + 1)) {
          this.resynchronize('synchronizing');
          return;
        }
        if (batch.kind === 'heartbeat') {
          if (batch.sequence !== this.sequence) { this.resynchronize('synchronizing'); return; }
          this.publish(batch.state);
          continue;
        }
        const committed = batch.entries.filter(entry => !entry.id.startsWith('live:'));
        if (!this.options.apply(this.revision, committed, batch.sequence)) {
          this.options.onState('incomplete', [...this.tail.values()], this.capabilities);
          this.stop();
          return;
        }
        for (const id of batch.remove) this.tail.delete(id);
        for (const entry of batch.entries) if (entry.id.startsWith('live:')) this.tail.set(entry.id, entry);
        if (this.tail.size > LIVE_MAX_ENTRIES || new TextEncoder().encode(JSON.stringify([...this.tail.values()])).length > LIVE_MAX_BYTES) {
          this.resynchronize('incomplete');
          return;
        }
        this.sequence = batch.sequence;
        this.publish(batch.state);
      } catch {
        if (epoch !== this.epoch || abort.signal.aborted) return;
        this.resynchronize('disconnected');
        return;
      }
    }
  }

  private publish(state: string): void {
    this.options.onState(state === 'incomplete' ? 'incomplete' : this.capabilities?.source === 'pi_events' ? 'live' : 'recorded', [...this.tail.values()], this.capabilities);
  }

  private resynchronize(state: LiveReaderState): void {
    this.stop();
    this.options.onState(state, [], this.capabilities);
    if (this.active) this.retry = setTimeout(() => { this.retry = undefined; this.options.reset(); }, 1000);
  }
}
