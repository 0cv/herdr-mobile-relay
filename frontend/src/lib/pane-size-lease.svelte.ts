import { tick } from 'svelte';
import { paneLeaseRenewalAllowed } from './config';
import type { Agent, TerminalFrame } from './types';

const PANE_SIZE_LEASE_REFRESH_MS = 10_000;
// One relay poll of slack over its own three-second settling window.
const PANE_RESIZE_WAIT_MAX_MS = 4_000;

export interface PaneSizeLeaseHost {
  agent(): Agent;
  supported(target: Agent): boolean;
  locked(): boolean;
  visible(): boolean;
  rowsEnabled(target: Agent): boolean;
  measure(withRows: boolean): { columns: number | null; rows: number; measurable: boolean };
  typing(): boolean;
  frame(): TerminalFrame | undefined;
  lease(target: Agent, columns: number, rows: number): Promise<{ columns: number; rows: number }>;
  release(target: Agent): Promise<void>;
  applied(target: Agent): void;
  canReportRelease(target: Agent): boolean;
}

export interface PaneSizeLeaseInputs {
  enabled: boolean;
  active: boolean;
  questionMode: boolean;
  connected: boolean;
}

export class PaneSizeLease {
  columns = $state(0);
  error = $state('');
  renderedColumns = $state(0);
  // Pre-resize frame kept only to suppress display of transient frames while
  // the agent repaints at the new width.
  // Raw state: the effect compares frame identity, which a deep proxy breaks.
  frameBaseline = $state.raw<TerminalFrame | undefined>(undefined);
  // The relay flags frames while a pane repaints at a new size. Suppressing
  // them keeps a half-drawn screen off the phone, but a shared session whose
  // desktop client keeps fighting the leased size can flag every frame, so the
  // wait is bounded: a stale screen is worse than a transient one.
  waitExpired = $state(false);
  rows = 0;

  private mounted = false;
  private generation = 0;
  private inFlight = false;
  private target: Agent | null = null;
  private queued: { columns: number; rows: number; force: boolean } | null = null;
  // Set when the page goes hidden; renewals continue within the grace so a
  // desktop browser that reports occluded windows as hidden does not lapse
  // the lease on every app switch.
  private hiddenAt = 0;
  private waitTimer: ReturnType<typeof setTimeout> | null = null;
  private renewalTimer: ReturnType<typeof setInterval> | null = null;

  constructor(private readonly host: PaneSizeLeaseHost) {}

  start() {
    this.mounted = true;
    this.renewalTimer = setInterval(() => {
      if (this.allowed()) this.request(true);
    }, PANE_SIZE_LEASE_REFRESH_MS);
  }

  stop() {
    this.mounted = false;
    if (this.renewalTimer !== null) clearInterval(this.renewalTimer);
    this.renewalTimer = null;
    this.release(false);
  }

  // Traffic stops while hidden, but lease renewals keep going for a bounded
  // grace: desktop Safari reports an occluded window as hidden, so treating
  // every app switch as departure lapsed the lease and resized the shared pane
  // twice per glance, stranding stale copies of inline agents' status bars in
  // the scrollback. Once the grace runs out the renewals stop and the relay's
  // TTL hands the size back: a page hidden overnight cannot keep the pane narrow.
  pageHidden() {
    this.hiddenAt = Date.now();
  }

  pageShown() {
    this.hiddenAt = 0;
  }

  sync(inputs: PaneSizeLeaseInputs) {
    if (!inputs.enabled) {
      // Turned off: the store has already released every pane this phone
      // leased, immediately. Only the local lease state is left to drop.
      this.discard();
      this.error = '';
      return;
    }
    if (!inputs.active) {
      this.release(this.mounted);
      this.error = '';
      return;
    }
    if (inputs.questionMode) {
      this.release(this.mounted);
      this.error = '';
      return;
    }
    if (!inputs.connected) {
      this.discard();
      this.error = '';
      return;
    }
    this.error = '';
    void tick().then(() => this.request(false));
  }

  discard() {
    this.generation += 1;
    this.target = null;
    this.columns = 0;
    this.rows = 0;
    this.renderedColumns = 0;
    this.clearSettling();
    this.queued = null;
  }

  release(reportFailure: boolean) {
    const target = this.target;
    this.discard();
    if (!target) return;
    void this.host.release(target).catch((error) => {
      if (reportFailure && this.mounted && this.host.canReportRelease(target)) {
        this.error = `Resize Session release failed: ${(error as Error).message}`;
      }
    });
  }

  request(force: boolean) {
    const target = this.host.agent();
    // A hidden page renews only within the grace window: after it, the
    // relay's lease TTL returns the desktop size, and the refocus handler
    // re-leases the moment the page is visible again.
    if (!this.allowed()) return;
    if (!this.supported(target)) return;
    const measured = this.host.measure(this.host.rowsEnabled(target));
    if (measured.columns === null) {
      if (measured.measurable) {
        this.error = 'Resize Session could not measure the terminal cell width.';
      }
      return;
    }
    const columns = measured.columns;
    let rows = measured.rows;
    // The on-screen keyboard shrinks the terminal while the user types, and
    // leasing that transient height would SIGWINCH the agent twice per
    // keyboard toggle. Every full-height redraw can strand a stale copy of a
    // bottom-anchored status bar in the scrollback, so while a text input
    // owns focus the lease keeps its resting height and may only grow; the
    // resize listeners re-measure the moment the keyboard closes.
    if (this.host.typing() && rows && this.rows && rows < this.rows) rows = this.rows;
    if (!force && columns === this.columns && rows === this.rows) return;
    if (this.queued && this.queued.columns === columns && this.queued.rows === rows) {
      this.queued = { columns, rows, force: this.queued.force || force };
    } else this.queued = { columns, rows, force };
    if (!this.inFlight) void this.flush();
  }

  // The lease needs its own gate: being locked is not the same as being
  // briefly hidden. The grace window exists so an app switch does not
  // resize the computer's pane, but an unverified holder must not keep it
  // narrowed at all, so a lock skips the grace instead of entering it. The
  // relay's TTL hands the size back if the lock outlasts it.
  private allowed(): boolean {
    return !this.host.locked()
      && paneLeaseRenewalAllowed(this.host.visible(), this.hiddenAt, Date.now());
  }

  private supported(target: Agent): boolean {
    return this.mounted && this.host.supported(target);
  }

  private beginSettling() {
    this.frameBaseline = this.host.frame();
    this.waitExpired = false;
    if (this.waitTimer !== null) clearTimeout(this.waitTimer);
    this.waitTimer = setTimeout(() => {
      this.waitTimer = null;
      this.waitExpired = true;
    }, PANE_RESIZE_WAIT_MAX_MS);
  }

  private clearSettling() {
    this.frameBaseline = undefined;
    this.waitExpired = false;
    if (this.waitTimer === null) return;
    clearTimeout(this.waitTimer);
    this.waitTimer = null;
  }

  private async flush() {
    if (this.inFlight) return;
    this.inFlight = true;
    try {
      while (this.queued) {
        const request = this.queued;
        this.queued = null;
        const target = this.host.agent();
        if (!this.supported(target)) continue;
        if (!request.force
          && request.columns === this.columns
          && request.rows === this.rows) continue;
        const generation = this.generation;
        this.target = target;
        try {
          if (request.columns !== this.columns
            || request.rows !== this.rows) this.beginSettling();
          const applied = await this.host.lease(target, request.columns, request.rows);
          if (generation !== this.generation || !this.supported(target)) continue;
          const changed = applied.columns !== this.columns
            || applied.rows !== this.rows;
          this.columns = applied.columns;
          this.rows = applied.rows;
          this.error = '';
          if (changed) {
            // The pane repaints at the new size: read again so the live
            // screen is the resized one. History stays with the relay journal.
            this.host.applied(target);
          }
        } catch (error) {
          if (generation === this.generation && this.supported(target)) {
            this.queued = null;
            this.columns = 0;
            this.rows = 0;
            this.clearSettling();
            this.error = `Resize Session failed: ${(error as Error).message}`;
          }
        }
      }
    } finally {
      this.inFlight = false;
      if (this.queued) void this.flush();
    }
  }
}
