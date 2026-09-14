export interface BudgetSnapshot {
  phase: string;
  startedAt: string;
  deadline: string;
  remainingMs: number;
  recoveryCount: number;
  recoveryLimit: number;
  exhausted: boolean;
}

export class PhaseBudgetError extends Error {
  readonly code: 'PHASE_BUDGET_EXHAUSTED' | 'PHASE_RECOVERY_EXHAUSTED';
  readonly phase: string;
  readonly operation: string;
  readonly elapsedMs: number;
  readonly recoveryCount: number;

  constructor(
    code: 'PHASE_BUDGET_EXHAUSTED' | 'PHASE_RECOVERY_EXHAUSTED',
    phase: string,
    operation: string,
    elapsedMs: number,
    recoveryCount: number,
  ) {
    super(`${code}: ${phase}/${operation}`);
    this.name = 'PhaseBudgetError';
    this.code = code;
    this.phase = phase;
    this.operation = operation;
    this.elapsedMs = elapsedMs;
    this.recoveryCount = recoveryCount;
  }
}

export interface PhaseBudgetOptions {
  timeoutMs: number;
  recoveryLimit?: number;
  parent?: PhaseBudget;
  reserveParentMs?: number;
  now?: () => number;
}

export class PhaseBudget {
  private readonly root: PhaseBudget;
  private readonly clock: () => number;
  private readonly startedAtMs: number;
  private readonly deadlineMs: number;
  private recoveryCountValue = 0;
  private recoveryLimitValue: number;

  readonly phase: string;

  constructor(phase: string, options: PhaseBudgetOptions) {
    if (!phase) throw new Error('PHASE_BUDGET: phase is required');
    if (!Number.isFinite(options.timeoutMs) || options.timeoutMs <= 0) {
      throw new Error('PHASE_BUDGET: timeout must be positive');
    }
    this.phase = phase;
    this.clock = options.now || options.parent?.clock || (() => Date.now());
    this.startedAtMs = this.clock();
    const requestedDeadline = this.startedAtMs + options.timeoutMs;
    const parent = options.parent;
    const reserved = options.reserveParentMs ?? 0;
    if (!Number.isFinite(reserved) || reserved < 0) throw new Error('PHASE_BUDGET: parent reserve must be nonnegative');
    this.deadlineMs = parent ? Math.min(requestedDeadline, parent.deadlineMs - reserved) : requestedDeadline;
    this.root = parent?.root || this;
    this.recoveryLimitValue = Math.max(0, Math.floor(options.recoveryLimit ?? 2));
    if (this.root === this) return;
    this.recoveryLimitValue = this.root.recoveryLimitValue;
  }

  get remainingMs(): number {
    return Math.max(0, this.deadlineMs - this.clock());
  }

  get exhausted(): boolean {
    return this.remainingMs <= 0;
  }

  get recoveryCount(): number {
    return this.root.recoveryCountValue;
  }

  get recoveryLimit(): number {
    return this.root.recoveryLimitValue;
  }

  phaseView(name: string, timeoutMs: number, reserveParentMs = 0): PhaseBudget {
    return new PhaseBudget(name, { timeoutMs, parent: this, reserveParentMs });
  }

  assertAvailable(operation: string): void {
    if (this.exhausted) {
      throw new PhaseBudgetError('PHASE_BUDGET_EXHAUSTED', this.phase, operation, this.clock() - this.startedAtMs, this.recoveryCount);
    }
  }

  recovery(reason: string): number {
    this.assertAvailable(`recovery:${reason}`);
    if (this.root.recoveryCountValue >= this.root.recoveryLimitValue) {
      throw new PhaseBudgetError('PHASE_RECOVERY_EXHAUSTED', this.phase, reason, this.clock() - this.startedAtMs, this.recoveryCount);
    }
    this.root.recoveryCountValue += 1;
    return this.root.recoveryCountValue;
  }

  async wait(milliseconds: number, operation = 'wait'): Promise<void> {
    this.assertAvailable(operation);
    const duration = Math.min(Math.max(0, milliseconds), this.remainingMs);
    await new Promise<void>((resolve) => setTimeout(resolve, duration));
    this.assertAvailable(operation);
  }

  snapshot(): BudgetSnapshot {
    return {
      phase: this.phase,
      startedAt: new Date(this.startedAtMs).toISOString(),
      deadline: new Date(this.deadlineMs).toISOString(),
      remainingMs: this.remainingMs,
      recoveryCount: this.recoveryCount,
      recoveryLimit: this.recoveryLimit,
      exhausted: this.exhausted,
    };
  }
}

export function phaseBudget(phase: string, timeoutMs: number, recoveryLimit = 2): PhaseBudget {
  return new PhaseBudget(phase, { timeoutMs, recoveryLimit });
}
