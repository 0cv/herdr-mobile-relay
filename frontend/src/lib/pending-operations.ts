export function releasePendingResources(
  pending: {
    timer: ReturnType<typeof setTimeout>;
    cleanup?: () => void;
  },
): void {
  clearTimeout(pending.timer);
  pending.cleanup?.();
}

export function resetPendingDeadline(
  pending: { timer: ReturnType<typeof setTimeout> },
  onTimeout: () => void,
  timeoutMs: number,
): void {
  clearTimeout(pending.timer);
  pending.timer = setTimeout(onTimeout, timeoutMs);
}

export function registerPendingOperation<T>(
  operations: Map<string, T>,
  requestId: string,
  signal: AbortSignal | undefined,
  abort: () => void,
  createPending: () => T,
): void {
  signal?.addEventListener('abort', abort, { once: true });
  const pending = createPending();
  operations.set(requestId, pending);
}
