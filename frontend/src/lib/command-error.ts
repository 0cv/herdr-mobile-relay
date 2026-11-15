export class CommandError extends Error {
  data?: Record<string, unknown>;
  code?: string;
  args?: Record<string, string | number | boolean>;
}

/**
 * A rejection for a frame that was already written to the transport. The
 * relay may have received and applied the command even though no result came
 * back, so callers must never treat a retry as safe (the retry-safety
 * doctrine's `dispatched_unknown` phase). Definitive pre-send failures —
 * capability checks, validation, a relay that never connected, a write the
 * transport refused — stay plain CommandErrors.
 */
export function dispatchedUnknownError(message: string): CommandError {
  const error = new CommandError(message);
  error.data = { dispatched_unknown: true };
  return error;
}
