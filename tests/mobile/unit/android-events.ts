import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { androidHelperDeaths, measuredAndroidEvents } from '../android-events';
import type { AndroidEnvironmentSnapshot, AndroidPlannedTermination } from '../android-environment';

const processName = 'com.android.chrome:sandboxed_process0:org.chromium.content.app.SandboxedProcessService0:2';
const line = (time: number, pid: number, tag: string, message: string) => `178910637${time}.000 ${pid} ${pid} I ${tag}: ${message}\n`;
const start = line(0, 1, 'HerdrMeasure', 'synthetic START');
const end = line(9, 1, 'HerdrMeasure', 'synthetic END');
const fork = line(1, 100, 'Zygote', 'Forked child process 200');
const birth = line(2, 559, 'ActivityManager', `Start proc 200:${processName}/u0ai2 for  {com.android.chrome/org.chromium.content.app.SandboxedProcessService0:2}`);
const child = line(3, 200, 'chromium', '[INFO:child_process_service.cc(72)] ChildProcessService: Exiting child process.');
const death = line(4, 559, 'ActivityManager', `Killing 200:${processName}/u0a145i-8998 (adj 0): isolated not needed`);
const exit = line(5, 100, 'Zygote', 'Process 200 exited cleanly (0)');
const body = fork + birth + child + death + exit;
const recordedLine = (value: string) => `${value}\n`;
const recordedStart = recordedLine('         1790252388.000 2000 2000 I HerdrMeasure: recorded START');
const recordedEnd = recordedLine('         1790252390.000 2000 2000 I HerdrMeasure: recorded END');
const recordedSnapshot = (boundary: 'start' | 'end', processes: Record<string, string> = { '5253': 'com.android.chrome_zygote', '553': 'system_server', '6538': 'com.android.chrome' }) => ({
  measurement: { id: 'recorded', boundary, processes },
}) as AndroidEnvironmentSnapshot;
const measureRecorded = (log: string, before = recordedSnapshot('start'), after = recordedSnapshot('end')) => measuredAndroidEvents(log, before, after, []);
const snapshot = (boundary: 'start' | 'end', processes: Record<string, string> = { '100': 'com.android.chrome_zygote', '559': 'system_server' }) => ({
  measurement: { id: 'synthetic', boundary, processes },
}) as AndroidEnvironmentSnapshot;
const measure = (log: string, before = snapshot('start'), after = snapshot('end')) => measuredAndroidEvents(log, before, after, []);

export const androidEventTests: [string, () => Promise<void>][] = [
  ['recorded main Chrome process death stays fatal', async () => {
    const mainDeath = recordedLine('         1790252389.460 553 1783 I ActivityManager: Process com.android.chrome (pid 6538) has died: fg TOP');
    const result = measureRecorded(recordedStart + mainDeath + recordedEnd,
      recordedSnapshot('start'), recordedSnapshot('end', { '5253': 'com.android.chrome_zygote', '553': 'system_server' }));
    assert.deepEqual(result.issues, []);
    assert.deepEqual(result.fatalEvents, [mainDeath.trimEnd()]);
  }],
];

androidEventTests.push(['synthetic receive boundaries retain ordinary producer inversions diagnostically', async () => {
  for (const timestamp of ['1789106369.984', '1789106380.016']) {
    const ordinary = line(1, 888, 'OtherProducer', 'ordinary record').replace('1789106371.000', timestamp);
    const mainDeath = line(4, 559, 'ActivityManager', 'Process com.android.chrome (pid 300) has died: fg TOP');
    const result = measure(start + ordinary + mainDeath + end);
    assert.deepEqual(result.issues, []);
    assert.deepEqual(result.boundaryDiscordances, [{ lineNumber: 2, line: ordinary.trimEnd() }]);
  }
}]);

androidEventTests.push(['synthetic adverse spill cannot expire attributed deaths or hide truncated forms', async () => {
  const before = snapshot('start', { '300': 'com.android.chrome' });
  for (const [tag, message] of [
    ['Process', 'Sending signal. PID: 300 SIG:'],
    ['Zygote', 'Process 300 exited due to unknown'],
    ['ActivityManager', 'Process com.android.chrome (pid 300) has died'],
  ]) {
    const initial = line(1, 559, 'ActivityManager', 'Process com.android.chrome (pid 300) has died: fg TOP');
    const spill = line(8, 559, tag, message);
    const result = measure(start + initial + end + spill, before);
    assert.deepEqual(result.fatalEvents, [initial.trimEnd(), spill.trimEnd()]);
  }
}]);

androidEventTests.push(['synthetic F001 historical identity cannot erase START snapshot attribution', async () => {
  const before = snapshot('start', { '300': 'com.android.chrome' });
  const historical = line(1, 559, 'ActivityManager', 'Start proc 300:com.example.other/u0a200 for service').replace('1789106371', '1789106369');
  const signal = line(4, 559, 'Process', 'Sending signal. PID: 300 SIG: 9');
  for (const log of [historical + start + signal + end, historical + start + end + signal, start + historical + signal + end]) {
    const result = measure(log, before);
    assert.deepEqual(result.issues, []);
    assert.deepEqual(result.fatalEvents, [signal.trimEnd()]);
  }
  const unrelatedSnapshot = snapshot('start', { '300': 'com.example.other' });
  assert.deepEqual(measure(historical.replace('com.example.other', 'com.android.chrome') + signal + start + end, unrelatedSnapshot).fatalEvents, [signal.trimEnd()]);
  const reuse = historical.replace('1789106369', '1789106372');
  assert.deepEqual(measure(start + reuse + signal + end, before).fatalEvents, []);
}]);

androidEventTests.push(['synthetic F003 later reuse cannot exclude an earlier-timestamp adverse record', async () => {
  const before = snapshot('start', { '300': 'com.android.chrome' });
  const reuse = line(8, 559, 'ActivityManager', 'Start proc 300:com.example.other/u0a200 for service').replace('1789106378', '1789106380');
  const signal = line(4, 559, 'Process', 'Sending signal. PID: 300 SIG: 9');
  for (const log of [start + end + reuse + signal, start + reuse + signal + end,
    start + reuse + reuse.replace('1789106380', '1789106373').replace('com.example.other', 'com.example.third') + signal + end,
    start + reuse.replace('1789106380', '1789106374') + signal + end]) {
    assert.deepEqual(measure(log, before).fatalEvents, [signal.trimEnd()]);
  }
  for (const adverse of [line(4, 559, 'Zygote', 'Process 300 exited cleanly (0)'),
    line(4, 300, 'ChimeraCfgMgr', 'Updating module config: old -> new'), signal.replace('SIG: 9', 'SIG:')]) {
    assert.deepEqual(measure(start + end + reuse + adverse, before).fatalEvents, [adverse.trimEnd()]);
  }
  assert.deepEqual(measure(start + reuse.replace('1789106380', '1789106372') + signal + end, before).fatalEvents, []);
  assert.deepEqual(measure(start + end + reuse + signal + signal.replace('1789106374', '1789106381'), before).fatalEvents, [signal.trimEnd()]);
}]);

androidEventTests.push(['synthetic F004 delayed relevant identity attributes already-received adverse records', async () => {
  const before = snapshot('start', { '300': 'com.android.chrome' });
  const reuse = line(2, 559, 'ActivityManager', 'Start proc 300:com.example.other/u0a200 for service');
  const birth = line(3, 559, 'ActivityManager', 'Start proc 300:com.android.chrome/u0a145 for activity');
  const signal = line(4, 300, 'Process', 'Sending signal. PID: 300 SIG: 9');
  for (const log of [start + reuse + end + signal + birth, start + reuse + signal + birth + end,
    start + reuse + end + signal + birth.replace('6373', '6374')]) {
    assert.deepEqual(measure(log, before).fatalEvents, [signal.trimEnd()]);
  }
  for (const adverse of [line(4, 559, 'Zygote', 'Process 300 exited cleanly (0)'),
    line(4, 300, 'ChimeraCfgMgr', 'Updating module config: old -> new'), signal.replace('SIG: 9', 'SIG:')]) {
    assert.deepEqual(measure(start + reuse + end + adverse + birth, before).fatalEvents, [adverse.trimEnd()]);
  }
  assert.deepEqual(measure(start + reuse + signal + end, before).fatalEvents, []);
  assert.deepEqual(measure(start + reuse + signal + birth.replace('6373', '6375') + end, before).fatalEvents, []);
  const death = line(3, 559, 'ActivityManager', 'Process com.android.chrome (pid 300) has died: fg TOP');
  assert.deepEqual(measure(start + reuse + end + signal + death, before).fatalEvents, [signal.trimEnd(), death.trimEnd()]);
}]);

androidEventTests.push(['synthetic F003 identity retirement requires ordered replacement of the identity itself', async () => {
  const before = snapshot('start', { '300': 'com.android.chrome' });
  const chrome = line(3, 559, 'ActivityManager', 'Start proc 300:com.android.chrome/u0a145 for activity');
  const unrelated = line(2, 559, 'ActivityManager', 'Start proc 300:com.example.other/u0a200 for service').replace('559 559', '559 560');
  const signal = line(4, 300, 'Process', 'Sending signal. PID: 300 SIG: 9');
  for (const log of [start + chrome + unrelated + end + signal, start + chrome + unrelated + signal + end,
    start + chrome + chrome.replace('6373', '6371') + unrelated + end + signal,
    start + chrome + unrelated.replace('6372', '6373') + end + signal]) {
    assert.deepEqual(measure(log, before).fatalEvents, [signal.trimEnd()]);
  }
  for (const adverse of [line(4, 559, 'Zygote', 'Process 300 exited cleanly (0)'),
    line(4, 300, 'ChimeraCfgMgr', 'Updating module config: old -> new'), signal.replace('SIG: 9', 'SIG:')]) {
    assert.deepEqual(measure(start + chrome + unrelated + end + adverse, before).fatalEvents, [adverse.trimEnd()]);
  }
  assert.deepEqual(measure(start + unrelated + signal + end, before).fatalEvents, []);
  assert.deepEqual(measure(start + chrome.replace('6373', '6371') + unrelated + signal + end, before).fatalEvents, []);
}]);

androidEventTests.push(['synthetic explicit operations require receive and timestamp containment for markers and events', async () => {
  const operation: AndroidPlannedTermination = {
    id: 'stop', measurementId: 'synthetic', packageName: 'com.android.chrome', pid: '300',
    processes: { '300': 'com.android.chrome' }, succeeded: true,
    command: ['shell', 'am', 'force-stop', '--user', '0', 'com.android.chrome'],
  };
  const begin = line(2, 1, 'HerdrMeasure', 'synthetic OP_BEGIN stop com.android.chrome 300');
  const finish = line(6, 1, 'HerdrMeasure', 'synthetic OP_END stop com.android.chrome 300');
  const kill = line(3, 559, 'ActivityManager', 'Killing 300:com.android.chrome/u0a145 (adj 0): stop com.android.chrome due to from pid 50');
  const signal = line(4, 100, 'Zygote', 'Process 300 exited due to signal 9 (Killed)');
  const check = (log: string, operations = [operation]) => measuredAndroidEvents(log, snapshot('start', { '300': 'com.android.chrome' }), snapshot('end'), operations);
  const valid = start + begin + kill + signal + finish + end;
  assert.deepEqual(check(valid).issues, []);
  assert.deepEqual(check(valid).fatalEvents, []);
  for (const log of [
    begin + start + kill + signal + finish + end,
    start + begin + kill + signal + end + finish,
    start + begin + kill + finish + signal + end,
    start + begin + kill + finish + end + signal,
    start + begin + kill + signal.replace('6374', '6369') + finish + end,
    start + begin + finish + end + kill + signal,
    valid.replace(begin, begin + line(8, 559, 'ActivityManager', 'Start proc 300:com.android.chrome/u0a145 for activity')),
    valid.replace(begin, line(4, 559, 'ActivityManager', 'Start proc 300:com.android.chrome/u0a145 for activity') + begin),
    valid + line(4, 559, 'ActivityManager', 'Start proc 300:com.android.chrome/u0a145 for activity'),
    valid.replace(signal, signal.replace('due to signal 9 (Killed)', 'due to unknown')),
    valid.replace(signal, line(4, 559, 'Process', 'Sending signal. PID: 300 SIG:')),
    valid.replace(begin, ''),
    valid.replace(begin, begin + begin),
  ]) {
    const result = check(log);
    assert.ok(result.issues.length || result.fatalEvents.length);
  }
  assert.ok(check(valid, [{ ...operation, succeeded: false }]).issues.length);
  assert.ok(check(valid, []).issues.length);
}]);


androidEventTests.push(['synthetic planned Chrome force-stop covers observed helpers the system reaps inside the operation', async () => {
  const helper = 'com.android.chrome:sandboxed_process0:org.chromium.content.app.SandboxedProcessService0:0';
  const operation: AndroidPlannedTermination = {
    id: 'stop', measurementId: 'synthetic', packageName: 'com.android.chrome', pid: '300',
    processes: { '300': 'com.android.chrome', '301': helper }, succeeded: true,
    command: ['shell', 'am', 'force-stop', '--user', '0', 'com.android.chrome'],
  };
  const processes: Record<string, string> = { '100': 'com.android.chrome_zygote', '300': 'com.android.chrome', '301': helper, '559': 'system_server' };
  const begin = line(2, 1, 'HerdrMeasure', 'synthetic OP_BEGIN stop com.android.chrome 300');
  const finish = line(6, 1, 'HerdrMeasure', 'synthetic OP_END stop com.android.chrome 300');
  const stopping = line(3, 559, 'ActivityManager', 'Force stopping com.android.chrome appid=10145 user=0: from pid 50');
  const kill = line(3, 559, 'ActivityManager', 'Killing 300:com.android.chrome/u0a145 (adj 0): stop com.android.chrome due to from pid 50');
  const reap = line(4, 559, 'ActivityManager', `Killing 301:${helper}/u0a145i-9000 (adj 0): isolated not needed`);
  const helperExit = line(4, 100, 'Zygote', 'Process 301 exited due to signal 9 (Killed)');
  const signal = line(5, 100, 'Zygote', 'Process 300 exited due to signal 9 (Killed)');
  const check = (log: string, operations = [operation], before = processes) => measuredAndroidEvents(log, snapshot('start', before), snapshot('end'), operations);
  const operationLog = (...records: string[]) => start + begin + records.join('') + finish + end;
  const valid = operationLog(stopping, kill, reap, helperExit, signal);
  assert.deepEqual(check(valid).issues, []);
  assert.deepEqual(check(valid).fatalEvents, []);
  assert.deepEqual(check(operationLog(stopping, kill, reap, helperExit.replace('due to signal 9 (Killed)', 'cleanly (0)'), signal)).fatalEvents, []);
  const cleanExit = helperExit.replace('due to signal 9 (Killed)', 'cleanly (0)');
  assert.deepEqual(check(operationLog(stopping, kill, cleanExit, reap, signal)).fatalEvents, []);
  const earlyCleanExit = cleanExit.replace('6374.000', '6373.750');
  assert.deepEqual(check(operationLog(stopping, kill, earlyCleanExit, reap, signal)).fatalEvents, []);
  const otherProcesses = { ...operation, processes: { '300': 'com.android.chrome' } };
  const otherName = { ...operation, processes: { '300': 'com.android.chrome', '301': `${helper}1` } };
  for (const [name, log, operations] of [
    ['early clean exit without reaping', operationLog(stopping, kill, earlyCleanExit, signal), [operation]],
    ['early crash exit', operationLog(stopping, kill, earlyCleanExit.replace('cleanly (0)', 'due to signal 11 (Segmentation fault)'), reap, signal), [operation]],
    ['clean exit received before force-stop', operationLog(earlyCleanExit, stopping, kill, reap, signal), [operation]],
    ['clean exit timestamped before force-stop', operationLog(stopping, kill, earlyCleanExit.replace('6373.750', '6372.500'), reap, signal), [operation]],
    ['early clean exit from unknown zygote', operationLog(stopping, kill, earlyCleanExit.replace('100 100', '101 101'), reap, signal), [operation]],
    ['early clean exit with another signal', operationLog(stopping, kill, earlyCleanExit, reap, line(4, 559, 'Process', 'Sending signal. PID: 301 SIG: 9'), signal), [operation]],
    ['early clean exit with duplicate exit', operationLog(stopping, kill, earlyCleanExit, reap, cleanExit, signal), [operation]],
    ['early clean exit with wrong helper identity', operationLog(stopping, kill, earlyCleanExit, reap, signal), [otherName]],
    ['early clean exit with unknown reaper', operationLog(stopping, kill, earlyCleanExit, reap.replace('559 559', '558 558'), signal), [operation]],
    ['helper outside the observed process set', valid, [otherProcesses]],
    ['helper with a different observed name', valid, [otherName]],
    ['reaping received after the operation', start + begin + stopping + kill + signal + finish + reap + helperExit + end, [operation]],
    ['reaping timestamped after the operation', operationLog(stopping, kill, reap.replace('6374.000', '6377.000'), helperExit, signal), [operation]],
    ['reaping before the force-stop record', operationLog(reap, helperExit, stopping, kill, signal), [operation]],
    ['force-stop record for another user', operationLog(stopping.replace('user=0', 'user=10'), kill, reap, helperExit, signal), [operation]],
    ['reaping by an unknown producer', operationLog(stopping, kill, reap.replace('559 559', '558 558'), helperExit, signal), [operation]],
    ['different reaping reason', operationLog(stopping, kill, reap.replace('isolated not needed', 'remove task'), helperExit, signal), [operation]],
    ['crash signal after reaping', operationLog(stopping, kill, reap, helperExit.replace('signal 9 (Killed)', 'signal 11 (Segmentation fault)'), signal), [operation]],
    ['additional signal after reaping', operationLog(stopping, kill, reap, helperExit, line(4, 559, 'Process', 'Sending signal. PID: 301 SIG: 9'), signal), [operation]],
    ['second exit after reaping', operationLog(stopping, kill, reap, helperExit, helperExit.replace('due to signal 9 (Killed)', 'cleanly (0)'), signal), [operation]],
    ['helper exit from an unknown zygote', operationLog(stopping, kill, reap, helperExit.replace('100 100', '101 101'), signal), [operation]],
    ['force-stop record from an unknown producer', operationLog(stopping.replace('559 559', '558 558'), kill, reap, helperExit, signal), [operation]],
    ['repeated reaping record', operationLog(stopping, kill, reap, reap, helperExit, signal), [operation]],
    ['force-stop kill of a sandboxed helper', operationLog(stopping, kill, reap.replace('/u0a145i-9000 (adj 0): isolated not needed', '/u0ai2 (adj 0): stop com.android.chrome due to from pid 50'), reap, helperExit, signal), [operation]],
    ['reaping repeated after the exit', operationLog(stopping, kill, reap, helperExit, reap, helperExit.replace('due to signal 9 (Killed)', 'cleanly (0)'), signal), [operation]],
  ] as [string, string, AndroidPlannedTermination[]][]) {
    const result = check(log, operations);
    assert.deepEqual(result.issues, [], name);
    assert.ok(result.fatalEvents.some((event) => event.includes('301')), name);
  }
}]);

for (const [name, log] of [
  ['missing start', body + end], ['missing end', start + body], ['reversed markers', end + body + start],
  ['duplicate start', start + start + body + end], ['truncated', (start + body + end).trimEnd()],
  ['malformed', start + body + 'broken record\n' + end],
] as [string, string][]) androidEventTests.push([`synthetic invalid coverage: ${name}`, async () => {
  const result = measure(log);
  assert.ok(result.issues.length);
}]);
androidEventTests.push(['synthetic helper-shaped and Chrome main deaths are all fatal events', async () => {
  const mainDeath = line(6, 559, 'ActivityManager', 'Process com.android.chrome (pid 300) has died: fg TOP');
  assert.deepEqual(measure(start + body + mainDeath + end).fatalEvents, [death.trimEnd(), mainDeath.trimEnd()]);
}]);

const hostedStartProcesses = { '364': 'zygote64', '559': 'system_server', '5242': 'com.android.chrome', '5283': 'com.android.chrome_zygote' };
const hostedHelperDeaths = async (processes: Record<string, string> = hostedStartProcesses) => {
  const log = await readFile(new URL('./fixtures/android-hosted-39634239574-helper-retirements.log', import.meta.url), 'utf8');
  const id = log.match(/HerdrMeasure: (\S+) START/u)![1];
  const before = snapshot('start', processes);
  const after = snapshot('end', { '364': 'zygote64', '559': 'system_server' });
  before.measurement!.id = after.measurement!.id = id;
  const { events } = measuredAndroidEvents(log, before, after, []);
  return { events, helperDeaths: androidHelperDeaths(log, processes, events) };
};

androidEventTests.push(['hosted helper deaths are every death line of the recorded log', async () => {
  const { events, helperDeaths } = await hostedHelperDeaths();
  assert.equal(events.length, 4);
  assert.deepEqual(helperDeaths, events);
}]);

androidEventTests.push(['synthetic helper deaths exclude PIDs named by Chrome, named only by its zygote, or without a PID', async () => {
  const renamed = await hostedHelperDeaths({ ...hostedStartProcesses, '5716': 'com.android.chrome' });
  assert.ok(renamed.events.some((event) => event.includes('Killing 5716:')));
  assert.ok(!renamed.helperDeaths.some((event) => event.includes('5716')));
  assert.ok(renamed.helperDeaths.some((event) => event.includes('9619')));
  const zygoteExit = line(5, 100, 'Zygote', 'Process 700 exited due to signal 9 (Killed)').trimEnd();
  assert.deepEqual(androidHelperDeaths(zygoteExit + '\n', { '700': 'com.android.chrome_zygote' }, [zygoteExit]), []);
  const replacement = line(5, 559, 'ActivityManager', 'Force stopping com.android.chrome appid=10145 user=0: installPackageLI').trimEnd();
  assert.deepEqual(androidHelperDeaths(replacement + '\n', {}, [replacement]), []);
  const helperKill = death.trimEnd();
  const reused = birth.replace(processName, 'com.android.chrome');
  assert.deepEqual(androidHelperDeaths(body, {}, [helperKill]), [helperKill]);
  assert.deepEqual(androidHelperDeaths(body + reused, {}, [helperKill]), []);
}]);
