import { appendFileSync } from 'node:fs';

export const probe = { spawns: 0, longTimerMs: 0 };

export function noteTimer(ms: unknown): void {
  if (typeof ms === 'number' && ms >= 1000) probe.longTimerMs += ms;
}

export async function timed<T>(scope: string, name: string, body: () => Promise<T>): Promise<T> {
  probe.spawns = 0;
  probe.longTimerMs = 0;
  const startedAt = performance.now();
  let status = 'ok';
  try {
    return await body();
  } catch (error) {
    status = 'fail';
    throw error;
  } finally {
    const line = [scope, Math.round(performance.now() - startedAt), probe.spawns, Math.round(probe.longTimerMs), status, name.replace(/\s+/gu, ' ')].join('\t');
    appendFileSync(process.env.CASE_TIMING_FILE ?? '/tmp/case-timing.tsv', `${line}\n`);
  }
}

const originalBunSleep = Bun.sleep;
(Bun as { sleep: typeof Bun.sleep }).sleep = ((ms: Parameters<typeof Bun.sleep>[0]) => {
  if (typeof ms === 'number') noteTimer(ms);
  return originalBunSleep(ms);
}) as typeof Bun.sleep;

const originalSetTimeout = globalThis.setTimeout;
globalThis.setTimeout = Object.assign(((handler: TimerHandler, ms?: number, ...args: unknown[]) => {
  if (typeof handler !== 'function' || typeof ms !== 'number' || ms < 1000) return originalSetTimeout(handler, ms, ...args);
  return originalSetTimeout(() => { noteTimer(ms); (handler as (...a: unknown[]) => void)(...args); }, ms);
}) as typeof setTimeout, originalSetTimeout);
