import { setTimeout as delay } from 'node:timers/promises';
import { parseAndroidGmsComponentState } from './android-environment';
import { redactText, writeSanitizedJson } from './support/diagnostics';
import { command } from './support/process';

// Google Play services sends its module init intents once at boot but can take minutes to finish them,
// enabling and disabling its own components until it logs the completion line below.
const MODULE_INIT_COMPLETED = /^.*\s+I\s+NewModuleHelper:\s+Completed all new module init intents for all processes\.\r?$/mu;
export const GMS_SETTLE_STABLE_SECONDS = 20;
export const GMS_SETTLE_MAXIMUM_UPTIME_SECONDS = 600;
export const GMS_SETTLE_MAXIMUM_CONSECUTIVE_FAILURES = 3;
const SAMPLE_INTERVAL_MS = 5_000;
const ADB_TIMEOUT_MS = 30_000;

export interface GmsSettleSample {
  uptimeSeconds: number;
  moduleInitCompleted: boolean;
  componentStateSha256: string;
  enabledComponents: number;
  disabledComponents: number;
}

export type GmsSettleDecision = 'settled' | 'wait' | 'unsettled' | 'restarted';

export function gmsSettleDecision(samples: GmsSettleSample[]): GmsSettleDecision {
  const last = samples.at(-1);
  if (!last) return 'wait';
  if (samples.some((sample, index) => index > 0 && sample.uptimeSeconds <= samples[index - 1].uptimeSeconds)) return 'restarted';
  const completed = samples.findIndex((sample) => sample.moduleInitCompleted);
  if (completed >= 0) {
    let stableFrom = samples.length - 1;
    while (stableFrom > completed && samples[stableFrom - 1].componentStateSha256 === last.componentStateSha256) stableFrom--;
    if (last.uptimeSeconds - samples[stableFrom].uptimeSeconds >= GMS_SETTLE_STABLE_SECONDS) return 'settled';
  }
  return last.uptimeSeconds >= GMS_SETTLE_MAXIMUM_UPTIME_SECONDS ? 'unsettled' : 'wait';
}

export function parseUptimeSeconds(text: string): number {
  const uptime = text.match(/^(\d+(?:\.\d+)?) \d+(?:\.\d+)?\r?\n$/u);
  if (!uptime) throw new Error('ANDROID_GMS_SETTLE: malformed device uptime');
  return Number(uptime[1]);
}

export function moduleInitCompleted(logcat: string): boolean {
  return MODULE_INIT_COMPLETED.test(logcat);
}

export interface GmsSettleOutcome {
  decision: GmsSettleDecision | 'error';
  samples: GmsSettleSample[];
  failures: string[];
}

export async function settleGms(sample: () => Promise<GmsSettleSample>, sleep: () => Promise<void>): Promise<GmsSettleOutcome> {
  const samples: GmsSettleSample[] = [];
  const failures: string[] = [];
  let consecutiveFailures = 0;
  for (let attempt = 0; ; attempt++) {
    if (attempt) await sleep();
    try {
      samples.push(await sample());
      consecutiveFailures = 0;
    } catch (error) {
      failures.push(error instanceof Error ? error.message : 'ANDROID_GMS_SETTLE: sample failed');
      if (++consecutiveFailures >= GMS_SETTLE_MAXIMUM_CONSECUTIVE_FAILURES) return { decision: 'error', samples, failures };
      continue;
    }
    const decision = gmsSettleDecision(samples);
    if (decision !== 'wait') return { decision, samples, failures };
  }
}

export function gmsSettleWarning(outcome: GmsSettleOutcome): string | undefined {
  if (outcome.decision === 'settled' || outcome.decision === 'restarted') return undefined;
  const detail = outcome.decision === 'error' ? `: ${outcome.failures.at(-1)}` : '';
  const text = `Google Play services did not settle before measurement (${outcome.decision}${detail}); the environment check decides.`;
  return `::warning::${text.replaceAll('%', '%25').replaceAll('\r', '%0D').replaceAll('\n', '%0A')}\n`;
}

async function adb(serial: string, args: string[]): Promise<string> {
  const label = `adb ${args.join(' ')}`;
  const result = await command('adb', ['-s', serial, ...args], ADB_TIMEOUT_MS, { label });
  if (result.stderr.trim()) throw new Error(`ANDROID_GMS_SETTLE: ${label} wrote to stderr: ${redactText(result.stderr).slice(0, 500)}`);
  return result.stdout;
}

async function deviceSample(serial: string): Promise<GmsSettleSample> {
  const uptimeSeconds = parseUptimeSeconds(await adb(serial, ['shell', 'cat', '/proc/uptime']));
  const completed = moduleInitCompleted(await adb(serial, ['logcat', '-d', '-b', 'main', '-v', 'threadtime', '-s', 'NewModuleHelper:I']));
  const state = parseAndroidGmsComponentState(await adb(serial, ['shell', 'dumpsys', 'package', 'com.google.android.gms']));
  return {
    uptimeSeconds,
    moduleInitCompleted: completed,
    componentStateSha256: state.componentStateSha256,
    enabledComponents: state.enabledComponents.length,
    disabledComponents: state.disabledComponents.length,
  };
}

function option(name: string): string {
  const index = process.argv.indexOf(`--${name}`);
  const value = index > 0 ? process.argv[index + 1] : undefined;
  if (!value || value.startsWith('--')) throw new Error(`ANDROID_GMS_SETTLE: --${name} is required`);
  return value;
}

async function main(): Promise<void> {
  const serial = option('serial');
  const output = option('output');
  const outcome = await settleGms(() => deviceSample(serial), () => delay(SAMPLE_INTERVAL_MS));
  const limits = {
    stableSeconds: GMS_SETTLE_STABLE_SECONDS,
    maximumUptimeSeconds: GMS_SETTLE_MAXIMUM_UPTIME_SECONDS,
    maximumConsecutiveFailures: GMS_SETTLE_MAXIMUM_CONSECUTIVE_FAILURES,
  };
  try {
    await writeSanitizedJson(output, { schema: 1, serial, limits, ...outcome });
  } catch {
    process.stdout.write('::warning::ANDROID_GMS_SETTLE could not write its samples.\n');
  }
  if (outcome.decision === 'restarted') throw new Error('ANDROID_GMS_SETTLE: the device restarted while Google Play services was settling');
  const warning = gmsSettleWarning(outcome);
  if (warning) process.stdout.write(warning);
}

if (import.meta.main) {
  main().catch((error: unknown) => {
    process.stderr.write(`${error instanceof Error ? error.message : 'ANDROID_GMS_SETTLE: failed'}\n`);
    process.exitCode = 1;
  });
}
