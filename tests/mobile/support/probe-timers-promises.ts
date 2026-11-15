import * as real from 'node:timers/promises';
import { noteTimer } from './case-probe';

export * from 'node:timers/promises';

export const setTimeout = (async (ms?: number, ...rest: unknown[]) => {
  const value = await (real.setTimeout as (...a: unknown[]) => Promise<unknown>)(ms, ...rest);
  noteTimer(ms);
  return value;
}) as typeof real.setTimeout;
