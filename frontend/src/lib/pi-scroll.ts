import type { ScrollState } from './types';

export function normalizeScrollState(value: unknown): ScrollState | undefined {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return;
  const s = value as Record<string, unknown>;
  if (!['auto', 'local'].includes(String(s.selection))
    || !['regular', 'fullscreen', 'unknown'].includes(String(s.renderer))
    || !['sgr_cell', 'unavailable', 'unknown'].includes(String(s.wheel))
    || !['pi_runtime', 'unknown'].includes(String(s.source))
    || !['application', 'local'].includes(String(s.route))
    || !['history', 'live_screen'].includes(String(s.presentation))
    || typeof s.available !== 'boolean' || typeof s.reason !== 'string' || s.reason.length > 160
    || typeof s.revision !== 'string' || !/^[a-f0-9]{64}$/.test(s.revision)
    || ![s.rows, s.columns, s.geometry_epoch].every(n => typeof n === 'number' && Number.isSafeInteger(n) && n >= 0)
    || Number(s.rows) > 65535 || Number(s.columns) > 65535) return;
  if (s.route === 'application' && (s.selection !== 'auto' || !s.available || s.renderer !== 'fullscreen'
    || s.wheel !== 'sgr_cell' || s.source !== 'pi_runtime' || !s.rows || !s.columns)) return;
  return s as unknown as ScrollState;
}

type Intent = { revision: string; target: string; run: () => Promise<void>; expires: number; generation: number };

export class PiScrollQueue {
  private pending?: Intent;
  private active = false;
  private generation = 0;

  constructor(private failed: (error: unknown) => void, private now = Date.now) {}

  cancel(): void { this.generation++; this.pending = undefined; }

  offer(intent: Omit<Intent, 'expires' | 'generation'>): void {
    const next = { ...intent, expires: this.now() + 250, generation: this.generation };
    if (this.active) { this.pending = next; return; }
    void this.dispatch(next);
  }

  private async dispatch(intent: Intent): Promise<void> {
    if (intent.expires <= this.now() || intent.generation !== this.generation) return;
    const generation = intent.generation;
    this.active = true;
    let delivered = false;
    try { await intent.run(); delivered = true; }
    catch (error) { if (generation === this.generation || this.pending?.generation === this.generation) this.failed(error); }
    finally {
      this.active = false;
      const pending = this.pending;
      this.pending = undefined;
      if (delivered && pending && pending.generation === this.generation
        && (pending.generation !== generation || pending.target === intent.target && pending.revision === intent.revision)) void this.dispatch(pending);
    }
  }
}
