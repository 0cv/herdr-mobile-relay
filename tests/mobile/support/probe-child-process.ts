import * as real from 'node:child_process';
import { probe } from './case-probe';

export * from 'node:child_process';

export const spawn = ((...args: Parameters<typeof real.spawn>) => { probe.spawns++; return real.spawn(...args); }) as typeof real.spawn;
export const spawnSync = ((...args: Parameters<typeof real.spawnSync>) => { probe.spawns++; return real.spawnSync(...args); }) as typeof real.spawnSync;
export const execFile = ((...args: Parameters<typeof real.execFile>) => { probe.spawns++; return real.execFile(...args); }) as typeof real.execFile;
export const execFileSync = ((...args: Parameters<typeof real.execFileSync>) => { probe.spawns++; return real.execFileSync(...args); }) as typeof real.execFileSync;
