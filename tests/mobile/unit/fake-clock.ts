type Timer = { at: number; ms: number; callback: () => void };

export class FakeClock {
  elapsed = 0;
  readonly epoch = Date.UTC(2026, 8, 20, 23, 59, 59, 250);
  readonly timers = new Map<number, Timer>();
  private lastId = 0;
  private pumping = false;

  constructor(private readonly automatic = false) {}

  wall = () => this.epoch + this.elapsed;
  mono = () => this.elapsed;
  now = () => this.elapsed;

  timer = (callback: () => void, ms: number) => {
    const id = ++this.lastId;
    this.timers.set(id, { at: this.elapsed + ms, ms, callback });
    this.pump();
    return id as unknown as ReturnType<typeof setTimeout>;
  };

  clear = (id: ReturnType<typeof setTimeout>) => { this.timers.delete(id as unknown as number); };

  advance(ms: number): void {
    const end = this.elapsed + ms;
    for (;;) {
      const next = this.earliest(end);
      if (!next) break;
      this.fire(next);
    }
    this.elapsed = end;
  }

  sleep = (ms: number): Promise<void> => {
    if (!this.automatic) {
      this.advance(ms);
      return Promise.resolve();
    }
    return new Promise(resolve => { this.timer(resolve, ms); });
  };

  private earliest(limit = Infinity): number | undefined {
    let best: number | undefined;
    for (const [id, timer] of this.timers) {
      if (timer.at > limit) continue;
      const current = best === undefined ? undefined : this.timers.get(best)!;
      if (!current || timer.at < current.at || (timer.at === current.at && id < best!)) best = id;
    }
    return best;
  }

  private fire(id: number): void {
    const timer = this.timers.get(id)!;
    this.timers.delete(id);
    this.elapsed = Math.max(this.elapsed, timer.at);
    timer.callback();
  }

  private pump(): void {
    if (!this.automatic || this.pumping) return;
    this.pumping = true;
    setImmediate(() => {
      this.pumping = false;
      const next = this.earliest();
      if (next === undefined) return;
      this.fire(next);
      this.pump();
    });
  }
}
