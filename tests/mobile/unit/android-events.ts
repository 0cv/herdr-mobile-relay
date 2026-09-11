import assert from 'node:assert/strict';
import { measuredAndroidEvents, parseAndroidAuditSubject } from '../android-events';
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
const uid = line(2, 200, 'CompatChangeReporter', 'Compat change id reported: 242716250; UID 90002; state: ENABLED');
const audit = line(3, 200, 'ThreadPoolForeg', 'type=1400 audit(0.0:238): avc:  denied  { setattr } for  name="arbitrary.txt" dev="dm-46" ino=65621 scontext=u:r:isolated_app:s0:c512,c768 tcontext=u:object_r:app_data_file:s0:c145,c256,c512,c768 tclass=file permissive=0').replace(' I ', ' W ');
const auditBody = body.replace(birth, birth + uid);
const snapshot = (boundary: 'start' | 'end', processes: Record<string, string> = { '100': 'com.android.chrome_zygote', '559': 'system_server' }) => ({
  measurement: { id: 'synthetic', boundary, processes },
}) as AndroidEnvironmentSnapshot;
const measure = (log: string, before = snapshot('start'), after = snapshot('end')) => measuredAndroidEvents(log, before, after, []);

export const androidEventTests: [string, () => Promise<void>][] = [
  ['synthetic normal helper has complete traceable proof, distinct from fatal events', async () => {
    const result = measure(start + body + end);
    assert.deepEqual(result.issues, []);
    assert.deepEqual(result.fatalEvents, []);
    assert.equal(result.events.length, 1);
    assert.equal(result.normalRetirements.length, 1);
    assert.equal(result.normalRetirements[0].uid, 90002);
    const consistentIdentity = uid + line(2, 200, 'cr_SplitCompatApp', `version=1 processName=${processName} isIsolatedProcess=true`);
    assert.equal(measure(start + body.replace(child, consistentIdentity + child) + end).normalRetirements.length, 1);
    for (const proof of result.normalRetirements[0].proof) assert.equal((start + body + end).split('\n')[proof.lineNumber - 1], proof.line);
    const unrelated = line(0, 559, 'ActivityManager', 'Force stopping com.android.chrome appid=10145 user=0: from pid 50');
    assert.equal(measure(start + unrelated + body + end).normalRetirements.length, 1);
    for (const producer of [559, 100]) {
      const postProof = line(8, 42, 'Zygote', `Process ${producer} exited due to signal 9 (Killed)`);
      assert.equal(measure(start + body + postProof + end).normalRetirements.length, 1);
    }
    assert.equal(measure(start + body.replace(child, child + unrelated.replace('6370', '6373').replace('user=0', 'user=1')) + end).normalRetirements.length, 1);
  }],
];

androidEventTests.push(['synthetic delayed audit retains typed subject attribution, not helper execution', async () => {
  const log = start + auditBody + audit + end;
  const result = measure(log);
  assert.deepEqual(result.issues, []);
  assert.deepEqual(result.fatalEvents, []);
  const subjects = result.normalRetirements[0].auditSubjects;
  assert.equal(subjects.length, 1);
  assert.equal(subjects[0].attribution, 'logd-audit-subject');
  assert.equal(subjects[0].subjectPid, '200');
  assert.equal(subjects[0].serial, '238');
  assert.equal(subjects[0].time, 1789106373000);
  assert.equal(log.split('\n')[subjects[0].lineNumber - 1], subjects[0].line);
  assert.equal(parseAndroidAuditSubject(audit.trimEnd())?.subjectPid, '200');
}]);

androidEventTests.push(['synthetic receive boundaries retain ordinary producer inversions diagnostically', async () => {
  for (const timestamp of ['1789106369.984', '1789106380.016']) {
    const ordinary = line(1, 888, 'OtherProducer', 'ordinary record').replace('1789106371.000', timestamp);
    const log = start + ordinary + body + end;
    const result = measure(log);
    assert.deepEqual(result.issues, []);
    assert.equal(result.normalRetirements.length, 1);
    assert.deepEqual(result.boundaryDiscordances, [{ lineNumber: 2, line: ordinary.trimEnd() }]);
    for (const proof of result.normalRetirements[0].proof) assert.equal(log.split('\n')[proof.lineNumber - 1], proof.line);
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

const boundaryNegatives: [string, string][] = [
  ['fork before marker time', start + body.replace('1789106371.000', '1789106369.984') + end],
  ['exit after marker time', start + body.replace('1789106375.000', '1789106380.016') + end],
  ['fork before START receipt', fork + start + body.replace(fork, '') + end],
  ['exit after END receipt', start + body.replace(exit, '') + end + exit],
  ['audit after END receipt', start + auditBody + end + audit],
  ['audit before marker time', start + auditBody + audit.replace('1789106373.000', '1789106369.984') + end],
  ['audit after marker time', start + auditBody + audit.replace('1789106373.000', '1789106380.016') + end],
  ['in-time post-END signal', start + body + end + line(4, 559, 'Process', 'Sending signal. PID: 200 SIG: 9')],
  ['out-of-time received signal', start + body + line(4, 559, 'Process', 'Sending signal. PID: 200 SIG: 9').replace('1789106374.000', '1789106369.984') + end],
  ['unknown helper spill outside both', start + body + end + line(8, 888, 'Other', 'unknown subject 200').replace('1789106378.000', '1789106380.016')],
  ['truncated adverse shape spill', start + body + end + line(4, 559, 'Process', 'Sending signal. PID: 200 SIG:')],
  ['malformed spill', start + body + end + 'unparsed adverse record\n'],
  ['pre-START malformed', 'unparsed adverse record\n' + start + body + end],
];
for (const [name, log] of boundaryNegatives) androidEventTests.push([`synthetic capture boundary rejects ${name}`, async () => {
  const result = measure(log);
  assert.equal(result.normalRetirements.length, 0);
  assert.ok(result.fatalEvents.length > 0 || result.issues.length > 0);
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

const malformedAudits = [
  audit.replace('200 200', '200 201'), audit.replace(' W ', ' I '),
  audit.replace('type=1400', 'type=1300'), audit.replace('audit(0.0:', 'audit(1789106373.0:'),
  audit.replace('isolated_app', 'untrusted_app'), audit.replace('c512,c768', 'c513,c768'),
  audit.replace('permissive=0', 'permissive=1'), audit.replace('ino=65621 ', ''),
  audit.replace(' for  ', ' for  pid=200 '), audit.replace('\n', ' extra=field\n'),
  audit.replace('name="arbitrary.txt"', 'name="unclosed'),
];
for (const [index, malformed] of malformedAudits.entries()) androidEventTests.push([`synthetic audit parser rejects unsupported shape ${index}`, async () => {
  assert.equal(parseAndroidAuditSubject(malformed.trimEnd()), undefined);
}]);

const conflictingAudit = audit.replace('0.0:238', '0.0:239').replace('scontext=u:r:isolated_app:', 'scontext=u:r:untrusted_app:');
const negative: [string, string][] = [
  ['recovery F002 direct UID contradiction without audit', auditBody.replace('UID 90002', 'UID 90003')],
  ['recovery F002 direct process contradiction without audit', body.replace(child, line(2, 200, 'cr_SplitCompatApp', 'version=1 processName=com.example isIsolatedProcess=true') + child)],
  ['recovery F002 direct isolation contradiction without audit', body.replace(child, line(2, 200, 'cr_SplitCompatApp', `version=1 processName=${processName} isIsolatedProcess=false`) + child)],
  ['recovery F002 malformed direct UID without audit', auditBody.replace('UID 90002', 'UID unknown')],
  ['recovery F001 in-lifetime conflicting audit subject with delayed valid audit', auditBody.replace(child, conflictingAudit + child) + audit],
  ['recovery F001 conflicting audit without supported audit', auditBody.replace(child, conflictingAudit + child)],
  ['recovery F001 conflicting audit user categories', auditBody.replace(child, audit.replace('0.0:238', '0.0:239').replace('s0:c512,c768', 's0:c513,c768') + child) + audit],
  ['recovery F001 duplicated contradictory source context', auditBody.replace(child, audit.replace('0.0:238', '0.0:239').replace(' tcontext=', ' scontext=u:r:untrusted_app:s0:c512,c768 tcontext=') + child) + audit],
  ...malformedAudits.map((value, index): [string, string] => [`delayed malformed audit ${index}`, auditBody + value]),
  ['audit missing UID corroboration', body + audit],
  ['audit conflicting UID', auditBody.replace('UID 90002', 'UID 90003') + audit],
  ['audit malformed UID', auditBody.replace('UID 90002', 'UID unknown') + audit],
  ['audit contradictory process identity', auditBody.replace(uid, uid + line(2, 200, 'cr_SplitCompatApp', 'version=1 processName=com.example isIsolatedProcess=true')) + audit],
  ['audit contradictory isolation', auditBody.replace(uid, uid + line(2, 200, 'cr_SplitCompatApp', `version=1 processName=${processName} isIsolatedProcess=false`)) + audit],
  ['audit duplicate serial', auditBody + audit + audit],
  ['audit contradictory serial subject', auditBody + audit + audit.replaceAll('200 200', '201 201')],
  ['audit before fork source position', audit + auditBody],
  ['audit timestamp before fork', auditBody + audit.replace('6373', '6370')],
  ['audit timestamp after exit', auditBody + audit.replace('6373', '6378')],
  ['audit adverse content retained', auditBody + audit.replace('arbitrary.txt', 'fatal-signal.txt')],
  ['audit does not cover unknown post-exit execution', auditBody + audit + line(3, 200, 'Other', 'unknown')],
  ['audit does not replace clean-exit proof', auditBody.replace(exit, '') + audit],
  ['signal9 with isolated-not-needed', body.replace(exit, exit.replace('cleanly (0)', 'due to signal 9 (Killed)'))],
  ['nonzero', body.replace('cleanly (0)', 'cleanly (1)')],
  ['contradictory exit', body + exit.replace('6375', '6376').replace('cleanly (0)', 'due to signal 9 (Killed)')],
  ['late contradictory signal', body + line(8, 559, 'Process', 'Sending signal. PID: 200 SIG: 9')],
  ['misordered adverse clock', body.replace(child, child + line(0, 559, 'Process', 'Sending signal. PID: 200 SIG: 9'))],
  ['duplicate death observation', body + death],
  ['missing fork', body.replace(fork, '')],
  ['missing birth', body.replace(birth, '')],
  ['missing child', body.replace(child, '')],
  ['missing system exit', body.replace(exit, '')],
  ['reused PID', body + birth.replace('6372', '6376')],
  ['wrong user', body.replace('/u0ai2', '/u1ai2')],
  ['wrong isolated UID', body.replace('i-8998', 'i-8997')],
  ['wrong component', body.replace('for  {com.android.chrome/', 'for  {com.example/')],
  ['wrong process', body.replace(`Killing 200:${processName}`, 'Killing 200:com.android.chrome:privileged_process0')],
  ['unexpected kill reason', body.replace('isolated not needed', 'remove task')],
  ['mismatched death user', body + death.replace('/u0a145', '/u1a145')],
  ['force stop', body.replace(child, child + line(3, 559, 'ActivityManager', 'Force stopping com.android.chrome appid=10145 user=0: from pid 50'))],
  ['F001 forward-misordered force-stop clock', body.replace(child, child + line(8, 559, 'ActivityManager', 'Force stopping com.android.chrome appid=10145 user=0: from pid 50'))],
  ['F001 truncated force-stop attribution', body.replace(child, child + line(3, 559, 'ActivityManager', 'Force stopping com.android.chrome'))],
  ['F001 incomplete other-user attribution', body.replace(child, child + line(3, 559, 'ActivityManager', 'Force stopping com.android.chrome appid=10145 user=1:'))],
  ['F002 reused ActivityManager producer', line(0, 42, 'ActivityManager', 'Start proc 559:com.example/u0a123 for service') + body],
  ...[559, 100].flatMap((producer): [string, string][] => [
    [`F002 terminated producer ${producer}`, line(0, 42, 'Zygote', `Process ${producer} exited due to signal 9 (Killed)`) + body],
    [`F002 cleanly exited producer ${producer}`, line(0, 42, 'Zygote', `Process ${producer} exited cleanly (0)`) + body],
    [`F002 AM producer death ${producer}`, line(0, 42, 'ActivityManager', `Process com.example (pid ${producer}) has died: fg TOP`) + body],
    [`F002 producer kill ${producer}`, line(0, 42, 'ActivityManager', `Killing ${producer}:com.example/u0a123 (adj 0): stop`) + body],
    [`F002 producer signal ${producer}`, line(0, 42, 'Process', `Sending signal. PID: ${producer} SIG: 9`) + body],
    [`F002 producer death forward clock ${producer}`, line(8, 42, 'Zygote', `Process ${producer} exited due to signal 9 (Killed)`) + body],
  ]),
  ['replacement', body.replace(child, child + line(3, 559, 'PackageManager', 'Replacing package com.android.chrome'))],
  ['crash', body.replace(child, child + line(3, 200, 'AndroidRuntime', 'FATAL EXCEPTION: main'))],
  ['signal despite clean', body.replace(child, child + line(3, 200, 'Process', 'Sending signal. PID: 200 SIG: 15'))],
  ['wrong zygote', body.replace('6375.000 100 100', '6375.000 101 101')],
  ['wrong AM producer', body.replaceAll('559 559', '558 558')],
  ['unknown earlier PID use', line(0, 200, 'Other', 'unattributed earlier lifetime') + body],
  ['F003 helper observation before fork with in-lifetime timestamp', line(3, 200, 'Other', 'unattributed earlier lifetime') + body],
  ['F003 helper observation after exit with in-lifetime timestamp', body + line(3, 200, 'Other', 'unattributed later lifetime')],
  ['reused zygote', body + line(6, 559, 'ActivityManager', 'Start proc 100:com.example/u0a123 for service')],
  ['out of order child', fork + child + birth + death + exit],
  ['backwards child clock', body.replace('6373.000', '6371.000')],
  ...['com.android.chrome', 'com.android.chrome:privileged_process0', 'com.google.android.gms'].map((name): [string, string] => [name + ' clean death', body.replaceAll(processName, name)]),
];
for (const [name, value] of negative) androidEventTests.push([`synthetic helper rejection: ${name}`, async () => {
  const result = measure(start + value + end);
  assert.equal(result.normalRetirements.length, 0);
  assert.ok(result.fatalEvents.length > 0);
}]);
for (const [name, log] of [
  ['missing start', body + end], ['missing end', start + body], ['reversed markers', end + body + start],
  ['duplicate start', start + start + body + end], ['truncated', (start + body + end).trimEnd()],
  ['malformed', start + body + 'broken record\n' + end],
] as [string, string][]) androidEventTests.push([`synthetic invalid coverage: ${name}`, async () => {
  const result = measure(log);
  assert.ok(result.issues.length);
  assert.equal(result.normalRetirements.length, 0);
}]);
androidEventTests.push(['synthetic mixed fatal and normal observations and inventory contradictions stay fatal', async () => {
  const result = measure(start + body + line(6, 559, 'ActivityManager', 'Process com.android.chrome (pid 300) has died: fg TOP') + end);
  assert.equal(result.normalRetirements.length, 1);
  assert.equal(result.fatalEvents.length, 1);
  for (const [before, after] of [[snapshot('start', { '200': processName }), snapshot('end')], [snapshot('start'), snapshot('end', { '200': 'com.example' })]]) {
    assert.equal(measure(start + body + end, before, after).normalRetirements.length, 0);
  }
}]);
