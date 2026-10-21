import assert from 'node:assert/strict';
import {
  gmsSettleDecision, gmsSettleWarning, GMS_SETTLE_MAXIMUM_CONSECUTIVE_FAILURES, GMS_SETTLE_MAXIMUM_UPTIME_SECONDS,
  moduleInitCompleted, parseUptimeSeconds, settleGms, type GmsSettleSample,
} from '../android-gms-settle';

const samples = (...entries: [number, string, boolean?][]): GmsSettleSample[] => entries.map(([uptimeSeconds, componentStateSha256, completed]) => ({
  uptimeSeconds, componentStateSha256, moduleInitCompleted: completed ?? false, enabledComponents: 1, disabledComponents: 1,
}));
const every = (from: number, to: number, state: string, completed = false): [number, string, boolean][] => {
  const result: [number, string, boolean][] = [];
  for (let uptime = from; uptime <= to; uptime += 5) result.push([uptime, state, completed]);
  return result;
};

export const androidGmsSettleTests: [string, () => Promise<void>][] = [
  ['GMS settle waits for module init completion even when components are already stable', async () => {
    assert.equal(gmsSettleDecision([]), 'wait');
    assert.equal(gmsSettleDecision(samples(...every(90, 300, 'a'))), 'wait');
    assert.equal(gmsSettleDecision(samples(...every(90, 160, 'a'), ...every(165, 180, 'a', true))), 'wait');
    assert.equal(gmsSettleDecision(samples(...every(90, 160, 'a'), ...every(165, 185, 'a', true))), 'settled');
  }],
  ['GMS settle measures stability only after module init completes', async () => {
    const changing = [...every(90, 150, 'a'), ...every(155, 160, 'b')];
    assert.equal(gmsSettleDecision(samples(...changing, ...every(165, 180, 'b', true))), 'wait');
    assert.equal(gmsSettleDecision(samples(...changing, ...every(165, 185, 'b', true))), 'settled');
    const after = [...every(90, 160, 'a'), ...every(165, 180, 'a', true), ...every(185, 200, 'b', true)];
    assert.equal(gmsSettleDecision(samples(...after)), 'wait');
    assert.equal(gmsSettleDecision(samples(...after, [205, 'b', true])), 'settled');
    assert.equal(gmsSettleDecision(samples(...every(90, 160, 'a', true), [165, 'b', true], ...every(170, 185, 'a', true))), 'wait');
    assert.equal(gmsSettleDecision(samples(...every(90, 160, 'a'), [165, 'a', true], ...every(170, 185, 'a'))), 'settled');
  }],
  ['GMS settle gives up at the uptime limit and reports a device restart', async () => {
    assert.equal(gmsSettleDecision(samples(...every(90, GMS_SETTLE_MAXIMUM_UPTIME_SECONDS - 5, 'a'))), 'wait');
    assert.equal(gmsSettleDecision(samples(...every(90, GMS_SETTLE_MAXIMUM_UPTIME_SECONDS, 'a'))), 'unsettled');
    const churn: [number, string, boolean][] = [];
    for (let uptime = 200; uptime <= GMS_SETTLE_MAXIMUM_UPTIME_SECONDS; uptime += 5) churn.push([uptime, uptime % 15 ? 'a' : 'b', true]);
    assert.equal(gmsSettleDecision(samples(...churn)), 'unsettled');
    assert.equal(gmsSettleDecision(samples([GMS_SETTLE_MAXIMUM_UPTIME_SECONDS + 30, 'a', true])), 'unsettled');
    assert.equal(gmsSettleDecision(samples([300, 'a'], [20, 'a'])), 'restarted');
    assert.equal(gmsSettleDecision(samples([300, 'a', true], [300, 'a', true])), 'restarted');
  }],
  ['GMS settle parses device uptime and the module init completion line', async () => {
    assert.equal(parseUptimeSeconds('167.53 590.12\n'), 167.53);
    assert.equal(parseUptimeSeconds('167.53 590.12\r\n'), 167.53);
    assert.equal(parseUptimeSeconds('167 590\n'), 167);
    for (const malformed of ['', '167.53\n', 'x 590.12\n', '167.53 590.12', '167.53 590.12\n\n']) assert.throws(() => parseUptimeSeconds(malformed), /malformed device uptime/u);
    const header = '--------- beginning of main\n';
    const completed = '10-21 16:43:23.589  1349  2916 I NewModuleHelper: Completed all new module init intents for all processes.';
    assert.equal(moduleInitCompleted(header), false);
    assert.equal(moduleInitCompleted(`${header}${completed}\n`), true);
    assert.equal(moduleInitCompleted(`${header}${completed}\r\n`), true);
    assert.equal(moduleInitCompleted(`${header}${completed.replace(' I NewModuleHelper: ', '\tI  NewModuleHelper:  ')}\n`), true);
    assert.equal(moduleInitCompleted(`${header}10-21 16:43:23.589  1349  2916 I NewModuleHelper: Received com.google.android.chimera.container.NEW_MODULE_FINALIZE(2/0)\n`), false);
    assert.equal(moduleInitCompleted(`${header}${completed.replace(' I ', ' W ')}\n`), false);
    assert.equal(moduleInitCompleted(`${header}${completed.replace('NewModuleHelper', 'OtherHelper')}\n`), false);
  }],
  ['GMS settle loop tolerates brief sampling failures and reports each outcome', async () => {
    const run = async (steps: (GmsSettleSample | Error)[]) => {
      let index = 0, sleeps = 0;
      const outcome = await settleGms(async () => {
        const step = steps[index++];
        if (!step) throw new Error('no more samples');
        if (step instanceof Error) throw step;
        return step;
      }, async () => { sleeps++; });
      return { outcome, sleeps, consumed: index };
    };
    const settledSteps = [...samples(...every(160, 160, 'a')), new Error('ANDROID_GMS_SETTLE: adb timed out'), ...samples(...every(170, 190, 'a', true))];
    const settled = await run(settledSteps);
    assert.equal(settled.outcome.decision, 'settled');
    assert.deepEqual(settled.outcome.failures, ['ANDROID_GMS_SETTLE: adb timed out']);
    assert.equal(settled.outcome.samples.length, 6);
    assert.equal(settled.sleeps, settled.consumed - 1);
    assert.equal(gmsSettleWarning(settled.outcome), undefined);
    const failing = Array.from({ length: GMS_SETTLE_MAXIMUM_CONSECUTIVE_FAILURES }, (_, index) => new Error(`failure ${index}%\n::error::x`));
    const error = await run([...samples([100, 'a']), ...failing, ...samples([200, 'a'])]);
    assert.equal(error.outcome.decision, 'error');
    assert.equal(error.consumed, 1 + GMS_SETTLE_MAXIMUM_CONSECUTIVE_FAILURES);
    assert.equal(error.outcome.samples.length, 1);
    const warning = gmsSettleWarning(error.outcome)!;
    assert.match(warning, /^::warning::Google Play services did not settle before measurement \(error: failure 2%25%0A::error::x\); the environment check decides\.\n$/u);
    const unsettled = await run(samples(...every(GMS_SETTLE_MAXIMUM_UPTIME_SECONDS - 5, GMS_SETTLE_MAXIMUM_UPTIME_SECONDS, 'a')));
    assert.equal(unsettled.outcome.decision, 'unsettled');
    assert.match(gmsSettleWarning(unsettled.outcome)!, /^::warning::Google Play services did not settle before measurement \(unsettled\)/u);
    const restarted = await run(samples([300, 'a'], [20, 'a']));
    assert.equal(restarted.outcome.decision, 'restarted');
    assert.equal(gmsSettleWarning(restarted.outcome), undefined);
  }],
];
