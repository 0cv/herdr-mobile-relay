import { existsSync } from 'node:fs';
import { appendFile, mkdir, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { repositoryPath } from '../support/paths';
import { fileSha256 } from '../support/artifacts';
import { type AndroidEnvironmentSnapshot, type AndroidPlannedTermination } from '../android-environment';
import { BoundedCommandError } from '../support/bounded-process';
import { redactText, writeSanitizedJson } from '../support/diagnostics';
import { CommandError, command, type CommandResult } from '../support/process';
import { FailureLedger, qualified } from '../support/mobile-result';
import { QualificationFailureLatch, QualificationFatalError } from '../support/oracle';
import { checkCLI, measurementIdentity, rehashPackage } from './android-product-environment';
import { bindEmulatorChild, emulatorBindingSha256 } from '../support/android-emulator-lifetime';
import { androidLifecycleCases, runAndroidLifecycleCase, withAndroidRetainedLifecycleFixture } from './android-retained-launch';
import { AppiumClient } from '../support/webdriver';
import { androidEventDetails, androidLogEvents } from '../android-events';
import assert from 'node:assert/strict';
import { androidCliTestRegistry, type Harness, type Test, androidEnvironmentCommands, AndroidEnvironmentMeasurement, beginObservedMeasurement, boundedCommandFailureDetails, chromeDeath, benign, marker, cli } from './android-environment';

export function androidEnvironmentEventTests(harness: Harness): Test[] {
  const { tests, test } = androidCliTestRegistry();
  const { prepare, state, snapshots } = androidEnvironmentCommands(harness);
  test('recorded isolated UID death retains reason and requesting PID without an exemption', async () => {
    const isolated = '1789100452.864 546 1761 I ActivityManager: Killing 5328:com.android.chrome:sandboxed_process0:org.chromium.content.app.SandboxedProcessService0:0/u0a146i-9000 (adj 0): isolated not needed';
    const stopped = '1789100452.274 546 1761 I ActivityManager: Killing 5358:com.android.chrome:privileged_process0/u0a146 (adj 0): stop com.android.chrome due to from pid 5777';
    assert.deepEqual(androidLogEvents(`${isolated}\n${stopped}\n`), [isolated, stopped]);
    assert.equal(androidEventDetails(isolated).uid, 'u0a146i-9000');
    assert.equal(androidEventDetails(isolated).reason, 'isolated not needed');
    assert.equal(androidEventDetails(isolated).kind, 'process-death');
    assert.equal(androidEventDetails(isolated).initiatorPid, undefined);
    assert.equal(androidEventDetails(stopped).initiatorPid, '5777');
    assert.equal(androidEventDetails('1789100452.274 546 1761 I ChimeraCfgMgr: Updating module config: old -> new').kind, 'module-config');
  });
  for (const observationFails of [false, true]) for (const fails of [false, true]) test(`bootstrap close bounded observations preserve settlement and original error ${fails} observation failure ${observationFails}`, async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
    try {
      await beginObservedMeasurement(measurement);
      if (observationFails) await writeFile(join(fixture.fixtureDirectory, 'valid-vending.json'), JSON.stringify({ processListFail: true }));
      let deletes = 0;
      const driver = new AppiumClient('http://fixture.test', 1_000, async (input, init) => {
        if (init?.method === 'DELETE') {
          deletes++;
          if (fails) throw new Error('synthetic close failure');
        }
        return Response.json({ value: new URL(String(input)).pathname === '/session' ? { sessionId: 'bootstrap' } : null });
      });
      await driver.create({ capabilities: {} });
      for (let index = 0; index < 55; index++) await driver.activeAppInfo();
      if (fails) await assert.rejects(() => measurement.observeBootstrapClose(driver), /synthetic close failure/u);
      else await measurement.observeBootstrapClose(driver);
      assert.equal(deletes, 1);
      const trace = JSON.parse(await readFile(join(fixture.root, 'android-environment-bootstrap-close.json'), 'utf8'));
      assert.equal(trace.qualifiesPlannedTermination, false);
      assert.equal(trace.BEGINMarkerSettled, observationFails ? undefined : true);
      assert.equal(trace.ENDMarkerSettled, observationFails ? undefined : true);
      if (observationFails) {
        assert.equal(trace.BEGINObservationFailed, true);
        assert.equal(trace.ENDObservationFailed, true);
      }
      assert.equal(trace.sessionPresentBefore, true);
      assert.equal(trace.sessionPresentAfter, fails);
      assert.equal(trace.deleteCommands.length, 1);
      assert.equal(trace.deleteCommands[0].failed, fails);
      assert.deepEqual(JSON.parse(await readFile(join(fixture.root, 'android-environment-operations.json'), 'utf8')), []);
    } finally {
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  const lifecycleCaseCounts = { happy: 3, warmOwner: 18, coldHandoff: 20, readinessAndBudget: 13 } satisfies Record<keyof typeof androidLifecycleCases, number>;
  for (const group of Object.keys(androidLifecycleCases) as Array<keyof typeof androidLifecycleCases>) {
    test(`retained initial and warm relaunch emit no fabricated close evidence or planned operations: ${group}`, async () => {
      let completed = 0;
      for (const name of androidLifecycleCases[group]) {
        const fixture = await harness.createFixture();
        try {
          await runAndroidLifecycleCase(fixture, group, name);
          assert.equal(existsSync(join(fixture.root, 'android-environment-bootstrap-close.json')), false);
          assert.doesNotMatch(await readFile(fixture.log, 'utf8'), /CLOSE_(?:BEGIN|END)/u);
          completed++;
        } catch (error) {
          try {
            process.stderr.write(`SM56_LIFECYCLE_FAILURE ${redactText(JSON.stringify({
              group, name, command: error instanceof BoundedCommandError ? boundedCommandFailureDetails(error.result) : undefined,
            })).slice(0, 4_000)}\n`);
          } catch (diagnosticError) { void diagnosticError; }
          throw error;
        }
      }
      assert.equal(completed, lifecycleCaseCounts[group]);
    });
  }
  test('SM56 warm lifecycle keeps independent and combined signal, stop and component drift fatal', async () => {
    for (const name of ['healthy-warm', 'early-isolated-signal9-only', 'unknown-stop-only', 'stable-binary-gms-components-only', 'signal9-stop-components-combined', 'package-identity-replacement-only']) {
      const fixture = await harness.createFixture();
      await withAndroidRetainedLifecycleFixture(fixture, async ({ platform, client, measurement, calls, operations }) => {
        const owner = client.retainSessionOwner();
        const window = platform.evidenceSnapshot().selectedInstalledWindow;
        const before = JSON.parse(await readFile(join(fixture.root, 'android-environment-before.json'), 'utf8')) as AndroidEnvironmentSnapshot;
        const combined = name === 'signal9-stop-components-combined';
        const signal = combined || name === 'early-isolated-signal9-only';
        const stop = combined || name === 'unknown-stop-only';
        const components = combined || name === 'stable-binary-gms-components-only';
        const replacement = name === 'package-identity-replacement-only';
        const expectedEvents: string[] = [];
        const native = (tag: string, message: string, fatal = false) => {
          const line = `${(Date.now() / 1000).toFixed(3)} 546 1761 I ${tag}: ${message}\n`;
          if (fatal) expectedEvents.push(line.trimEnd());
          return line;
        };
        let events = '';
        if (signal) {
          const child = 'com.android.chrome:privileged_process0';
          events += native('ActivityManager', `Start proc 5835:${child}/u0a146 for service {com.android.chrome/org.chromium.content.app.PrivilegedProcessService0}`);
          events += native('ActivityManager', `Killing 5835:${child}/u0a146 (adj 0): isolated not needed`, true);
          events += native('Process', 'Sending signal. PID: 5835 SIG: 9', true);
        }
        if (stop) {
          assert.equal(before.measurement?.processes['6538'], 'com.android.chrome');
          events += native('ActivityManager', 'Force stopping com.android.chrome appid=10123 user=0: from pid 9864');
          events += native('ActivityManager', 'Killing 6538:com.android.chrome/u0a123 (adj 0): stop com.android.chrome due to from pid 9864', true);
        }
        if (replacement) events += native('PackageManager', 'Replacing package com.android.chrome', true);
        if (events) await appendFile(join(fixture.fixtureDirectory, 'native.log'), events);
        if (components) {
          const file = join(fixture.fixtureDirectory, 'valid-gms.dump');
          const dump = await readFile(file, 'utf8');
          assert.ok(dump.includes('Queries:'));
          await writeFile(file, dump.replace('Queries:', '      enabledComponents:\n        com.google.android.gms.fixture.ComponentChanged\nQueries:'));
        }
        if (replacement) {
          const file = join(fixture.fixtureDirectory, 'valid-chrome.dump');
          const dump = await readFile(file, 'utf8');
          await writeFile(file, dump.replace(/lastUpdateTime=[^\n]+/u, 'lastUpdateTime=2026-09-20 12:00:00'));
        }
        await platform.relaunchInstalledApp();
        await platform.backgroundApp();
        await platform.relaunchInstalledApp();
        const candidate = await platform.readRunningIdentity();
        assert.equal(candidate.version, '0.21.0');
        assert.equal(candidate.assets, 380);
        assert.equal(candidate.buildFromApplication, true);
        owner();
        assert.equal(platform.evidenceSnapshot().selectedInstalledWindow, window);
        assert.equal(calls.filter(call => call.path === '/session').length, 1);
        assert.equal(calls.filter(call => call.method === 'DELETE').length, 0);
        assert.deepEqual(await operations(), []);
        (measurement as any).identity = measurementIdentity({ measurementId: measurement.id });
        const outcome = await measurement.finish();
        assert.deepEqual(outcome.errors, []);
        assert.equal(outcome.assessment?.status, name === 'healthy-warm' ? 'PASS' : 'FAIL');
        const result = JSON.parse(await readFile(join(fixture.root, 'android-environment-check.json'), 'utf8'));
        assert.equal(result.passed, name === 'healthy-warm');
        assert.deepEqual(result.boundaryDiscordances, []);
        assert.ok((await readFile(join(fixture.root, 'android-qualification-logcat.log'), 'utf8')).includes(events));
        assert.deepEqual(result.forcedRestartEvents, expectedEvents);
        assert.deepEqual(result.nativeEvents.map((event: { line: string }) => event.line), expectedEvents);
        assert.equal(result.eventCounts.rawEvents, expectedEvents.length);
        assert.equal(result.eventCounts.distinctDeathPids, Number(signal) + Number(stop));
        assert.equal(result.eventCounts.fatalEvents, expectedEvents.length);
        assert.deepEqual(result.issues, [
          ...(components ? [
            'com.google.android.gms dependencyConfigSha256 changed',
            'com.google.android.gms identitySha256 changed',
            'com.google.android.gms dependency configuration changed',
          ] : []),
          ...(replacement ? ['com.android.chrome lastUpdateTime changed', 'com.android.chrome identitySha256 changed'] : []),
          ...(expectedEvents.length ? ['native process death, dependency configuration change or package replacement was observed'] : []),
        ]);
        if (signal) assert.ok(result.forcedRestartEvents.some((line: string) => line.includes('5835 SIG: 9')));
        if (stop) assert.ok(result.forcedRestartEvents.some((line: string) => line.includes('from pid 9864')));
        if (stop) {
          const stopped = result.nativeEvents.find((event: { initiatorPid?: string }) => event.initiatorPid === '9864');
          assert.ok(stopped);
          assert.equal(stopped.kind, 'process-death');
          assert.equal(stopped.pid, '6538');
          assert.equal(stopped.processName, 'com.android.chrome');
          assert.equal(stopped.uid, 'u0a123');
          assert.equal(stopped.reason, 'stop com.android.chrome due to from pid 9864');
        }
        if (components) {
          assert.ok(result.issues.includes('com.google.android.gms dependency configuration changed'));
          const after = JSON.parse(await readFile(join(fixture.root, 'android-environment-after.json'), 'utf8')) as AndroidEnvironmentSnapshot;
          for (const key of ['versionCode', 'versionName', 'codePath', 'apkPaths', 'lastUpdateTime'] as const) {
            assert.deepEqual(after.packages['com.google.android.gms'][key], before.packages['com.google.android.gms'][key]);
          }
        }
        if (replacement) assert.ok(result.issues.some((issue: string) => /com.android.chrome.*changed/u.test(issue)));
        assert.deepEqual(await operations(), []);
        assert.equal(existsSync(join(fixture.root, 'android-environment-bootstrap-close.json')), false);
        assert.doesNotMatch(await readFile(fixture.log, 'utf8'), /force-stop|disable-user|CLOSE_(?:BEGIN|END)/u);
      });
    }
  });
  for (const violation of ['earlier SIGKILL', 'unknown close force-stop', 'both'] as const) {
    test(`smoke53 successful scenario and session close cannot qualify ${violation}`, async () => {
      const fixture = await harness.createFixture();
      const child = 'com.android.chrome:privileged_process0';
      await state(fixture, { absent: true, children: { '5759': child } });
      const { before, after } = await snapshots(fixture);
      let deletes = 0;
      const driver = new AppiumClient('http://fixture.test', 1_000, async (input, init) => {
        if (init?.method === 'DELETE') {
          assert.equal(new URL(String(input)).pathname, '/session/scenario');
          deletes++;
          return Response.json({ value: null });
        }
        return Response.json({ value: new URL(String(input)).pathname === '/session' ? { sessionId: 'scenario' } : true });
      });
      await driver.create({ capabilities: {} });
      assert.equal(await driver.execute('return true'), true, 'synthetic scenario assertion completes');
      await driver.close();
      assert.equal(deletes, 1);
      assert.equal(driver.snapshot().sessionId, '');
      assert.equal(driver.snapshot().unusable, false);
      // Source-derived event shapes with fixture times/PIDs. The observed
      // initiator 9970 stays UNKNOWN; DELETE is not an am force-stop receipt.
      const early = `09-10 08:45:09.265 546 1761 I ActivityManager: Killing 5759:${child}/u0a146 (adj 0): isolated not needed\n`
        + '09-10 08:45:09.401 5759 5759 I Process: Sending signal. PID: 5759 SIG: 9\n';
      const close = marker('20.000', 'CLOSE_BEGIN bootstrap')
        + '09-10 08:45:21.180 546 1761 I ActivityManager: Force stopping com.android.chrome appid=10146 user=0: from pid 9970\n'
        + '09-10 08:45:21.181 546 1761 I ActivityManager: Killing 6538:com.android.chrome/u0a146 (adj 0): stop com.android.chrome due to from pid 9970\n'
        + marker('22.000', 'CLOSE_END bootstrap');
      const checked = await harness.check(fixture, before, after, (violation !== 'unknown close force-stop' ? early : '') + (violation !== 'earlier SIGKILL' ? close : ''));
      assert.equal(checked.passed, false);
      assert.ok(checked.issues.includes('native process death, dependency configuration change or package replacement was observed'));
      const report = JSON.parse(await readFile(join(fixture.root, 'check.json'), 'utf8'));
      if (violation !== 'unknown close force-stop') assert.ok(report.forcedRestartEvents.some((line: string) => line.includes('5759 SIG: 9')));
      if (violation !== 'earlier SIGKILL') assert.ok(report.forcedRestartEvents.some((line: string) => line.includes('from pid 9970')));
      assert.deepEqual(JSON.parse(await readFile(join(fixture.root, 'operations.json'), 'utf8')), [], 'never fabricate device initiator ownership from a host session close');
    });
  }
  test('padded epoch measurement framing preserves event attribution and rejects invalid records', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const recordedStart = '         1789081242.368  5536  5536 I HerdrMeasure: f2965e32-5a0d-48c2-beb6-e3a259a020f4 START\n';
    const recordedEnd = '         1789081278.547  6233  6233 I HerdrMeasure: f2965e32-5a0d-48c2-beb6-e3a259a020f4 END\n';
    for (const [path, boundary] of [[before, 'start'], [after, 'end']]) {
      const snapshot = JSON.parse(await readFile(path, 'utf8')) as AndroidEnvironmentSnapshot;
      snapshot.measurement = { ...snapshot.measurement!, id: 'f2965e32-5a0d-48c2-beb6-e3a259a020f4', boundary: boundary as 'start' | 'end' };
      await writeFile(path, JSON.stringify(snapshot));
    }
    const operations = join(fixture.root, 'operations.json');
    const logFile = join(fixture.root, 'epoch.log');
    const output = join(fixture.root, 'check.json');
    await writeFile(operations, '[]');
    const check = async (log: string) => {
      await writeFile(logFile, log);
      const result = checkCLI(fixture, { before, after, log: logFile, operations, output });
      return { ...result, report: JSON.parse(await readFile(output, 'utf8')) };
    };
    const recordedDex = '         1789081242.806  5192  5192 I artd    : Dex parent of /product/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk is not writable: Read-only file system\n';
    for (const padding of ['         ', '', '\t ', ' \t']) {
      const log = (recordedStart + recordedDex + recordedEnd).replace(/^ +/gmu, padding);
      assert.equal((await check(log)).passed, true, JSON.stringify(padding));
    }
    const death = '         1789081250.000 546 1761 I ActivityManager: Process com.android.chrome (pid 6538) has died: fg TOP\n';
    const module = '\t1789081251.000 1427 7277 I ChimeraCfgMgr: Updating module config: old -> new\n';
    const changed = await check(recordedStart + death + module + recordedEnd);
    assert.equal(changed.passed, false);
    assert.deepEqual(changed.report.forcedRestartEvents, [death.trimEnd(), module.trimEnd()]);
    assert.deepEqual(changed.report.issues, ['native process death, dependency configuration change or package replacement was observed']);
    const unrelated = module.replace('1427', '1486');
    assert.equal((await check(death + recordedStart + unrelated + recordedEnd)).passed, false);
    for (const log of [
      recordedStart, recordedEnd, recordedStart + recordedStart + recordedEnd,
      recordedStart + recordedEnd + recordedEnd, recordedEnd + recordedStart,
      recordedStart + recordedEnd.replace('1789081278.547', '1789081241.000'),
      (recordedStart + recordedEnd).trimEnd(),
      ...['broken log record\n', death.replace('1789081250.000', '1789081250.x00'),
        death.replace('546 1761', 'pid 1761'), death.replace('546 1761', '546 tid'),
        death.replace('1789081250.000', '1789081280.000')].map((body) => recordedStart + body + recordedEnd),
      ...['prefix ', '\v', '\f', '\u00a0'].map((prefix) => prefix + recordedStart + recordedEnd),
      recordedStart.replace('I HerdrMeasure:', 'I Other: HerdrMeasure:') + recordedEnd,
    ]) assert.equal((await check(log)).passed, false, JSON.stringify(log));
    const snapshot = JSON.parse(await readFile(after, 'utf8')) as AndroidEnvironmentSnapshot;
    snapshot.packages['com.google.android.gms'].dependencyConfig.enabledComponents = ['com.google.android.gms.fonts.provider.FontsProvider'];
    rehashPackage(snapshot.packages['com.google.android.gms']);
    await writeFile(after, JSON.stringify(snapshot));
    const persistent = await check(recordedStart + recordedEnd);
    assert.equal(persistent.passed, false);
    assert.ok(persistent.report.issues.includes('com.google.android.gms dependency configuration changed'));
  });
  test('recorded push and PR setup notifications are outside the authoritative interval', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const operations = join(fixture.root, 'operations.json');
    await writeFile(operations, '[]');
    for (const run of ['34488014724', '34488020101']) {
      const log = join(fixture.root, `${run}.log`);
      const setup = await readFile(repositoryPath(`tests/mobile/unit/fixtures/android-recorded-${run}-setup.log`), 'utf8');
      await writeFile(log, `${setup}09-10 14:25:00.000 2000 2000 I HerdrMeasure: android-test START\n09-10 14:26:00.000 2000 2000 I HerdrMeasure: android-test END\n`);
      const result = checkCLI(fixture, { before, after, log, operations, output: join(fixture.root, 'check.json') });
      assert.equal(result.passed, true, result.stderr);
    }
  });
  for (const [name, log, passed] of [
    ['direct Chrome death', chromeDeath, false], ['benign stack', benign, true],
    ['unrelated module and SIG9 interleaving', '09-10 08:45:09.464 1486 7277 I DynamiteLoaderV2Impl: Module config changed, forcing restart due to module googlecertificates\n09-10 08:45:09.465 1486 7277 I Process : Sending signal. PID: 1486 SIG: 9\n', true],
    ['hypothetical explicit package replacement', '09-10 08:45:09.464 546 7277 I PackageManager: Replacing package com.google.android.gms\n', false],
    ['source-derived installation force-stop', '09-10 08:45:09.464 546 7277 I ActivityManager: Force stopping com.google.android.gms appid=10143 user=-1: installPackageLI\n', false],
    ['package observer', '09-10 08:45:09.464 1073 1073 D ActivityThread: Package [com.android.chrome] reported as REPLACED, but missing application info. Assuming REMOVED.\n', true],
    ['equal config update', '09-10 08:45:09.464 1427 7277 I ChimeraCfgMgr: Updating module config: container:2423359190800 -> container:2423359190800\n', true],
    ['real config update', '09-10 08:45:09.464 1427 7277 I ChimeraCfgMgr: Updating module config: container:2423359190800 -> container:2633329260800\n', false],
    ['new config inside qualification', '09-10 08:45:09.464 1427 7277 I ChimeraCfgMgr: Updating module config: <no config> -> container:2633329260800\n', false],
    ['PID reuse by unrelated process', '09-10 08:45:09.000 546 7277 I ActivityManager: Start proc 6538:com.example.other/u0a200 for service\n09-10 08:45:09.464 6538 7277 I Process : Sending signal. PID: 6538 SIG: 9\n', true],
    ['new Chrome PID death', '09-10 08:45:09.000 546 7277 I ActivityManager: Start proc 7777:com.android.chrome/u0a146 for activity\n09-10 08:45:09.464 7777 7277 I Process : Sending signal. PID: 7777 SIG: 9\n', false],
    ['malformed interval record', 'broken log record\n', false],
  ] as const) test(`check classifies ${name}`, async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    assert.equal((await harness.check(fixture, before, after, log)).passed, passed, name);
  });
  test('retains attributed recorded googlecertificates kill and ignores unrelated interleaved PIDs', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const log = await readFile(repositoryPath('tests/mobile/unit/fixtures/android-recorded-34455964640-googlecertificates.log'), 'utf8');
    const result = await harness.check(fixture, before, after, log);
    assert.equal(result.passed, false);
    const check = JSON.parse(await readFile(join(fixture.root, 'check.json'), 'utf8')) as { forcedRestartEvents: string[] };
    assert.ok(check.forcedRestartEvents.some((line) => line.includes('PID: 6538 SIG: 9')));
    assert.equal(check.forcedRestartEvents.some((line) => line.includes('PID: 1486 SIG: 9')), false);
  });
  test('missing/unreadable logs, missing/duplicate markers and captured setup deaths fail closed', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const operations = join(fixture.root, 'operations.json');
    await writeFile(operations, '[]');
    const check = (log: string) => checkCLI(fixture, { before, after, log, operations, output: join(fixture.root, 'check.json') });
    for (const path of [join(fixture.root, 'missing.log'), fixture.root]) assert.equal(check(path).passed, false);
    const logFile = join(fixture.root, 'interval.log');
    for (const log of ['', benign, marker('00.000', 'START') + marker('00.100', 'START') + marker('30.000', 'END')]) {
      await writeFile(logFile, log);
      assert.equal(check(logFile).passed, false);
    }
    await writeFile(logFile, chromeDeath + marker('10.000', 'START') + marker('30.000', 'END'));
    assert.equal(check(logFile).passed, false);
  });
  test('F001 snapshot attribution and F002 operation reuse survive discordant capture records', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    for (const [path, boundary] of [[before, 'start'], [after, 'end']] as const) {
      const snapshot = JSON.parse(await readFile(path, 'utf8')) as AndroidEnvironmentSnapshot;
      snapshot.measurement = { id: 'synthetic', boundary, processes: { '300': 'com.android.chrome' } };
      await writeFile(path, JSON.stringify(snapshot));
    }
    const record = (second: number, tag: string, message: string) => `${(1789106370 + second).toFixed(3)} 559 559 I ${tag}: ${message}\n`;
    const start = record(0, 'HerdrMeasure', 'synthetic START');
    const end = record(9, 'HerdrMeasure', 'synthetic END');
    const signal = record(4, 'Process', 'Sending signal. PID: 300 SIG: 9');
    const history = record(-1, 'ActivityManager', 'Start proc 300:com.example.other/u0a200 for service');
    const operation = { id: 'stop', measurementId: 'synthetic', packageName: 'com.android.chrome', pid: '300',
      processes: { '300': 'com.android.chrome' }, succeeded: true,
      command: ['shell', 'am', 'force-stop', '--user', '0', 'com.android.chrome'] };
    const begin = record(2, 'HerdrMeasure', 'synthetic OP_BEGIN stop com.android.chrome 300');
    const finish = record(6, 'HerdrMeasure', 'synthetic OP_END stop com.android.chrome 300');
    const kill = record(3, 'ActivityManager', 'Killing 300:com.android.chrome/u0a145 (adj 0): stop com.android.chrome due to from pid 50');
    const reuse = record(8, 'ActivityManager', 'Start proc 300:com.android.chrome/u0a145 for activity');
    const cases = [
      { name: 'F001 interior signal', log: history + start + signal + end, operations: [], fatal: 1 },
      { name: 'F001 post-END signal', log: history + start + end + signal, operations: [], fatal: 1 },
      { name: 'F001 delayed historical identity', log: start + history + signal + end, operations: [], fatal: 1 },
      { name: 'unrelated in-window reuse', log: start + history.replace('1789106369', '1789106372') + signal + end, operations: [], fatal: 0 },
      { name: 'F003 post-END reuse before delayed signal', log: start + end + history.replace('1789106369', '1789106380') + signal, operations: [], fatal: 1 },
      { name: 'F003 interior reuse before delayed signal', log: start + history.replace('1789106369', '1789106380') + signal + end, operations: [], fatal: 1 },
      { name: 'F004 post-END delayed relevant identity', log: start + history.replace('1789106369', '1789106372') + end + signal + reuse.replace('1789106378', '1789106373'), operations: [], fatal: 1 },
      { name: 'F004 interior delayed relevant identity', log: start + history.replace('1789106369', '1789106372') + signal + reuse.replace('1789106378', '1789106373') + end, operations: [], fatal: 1 },
      { name: 'F003 discordant identity retirement post-END', log: start + reuse.replace('1789106378', '1789106373') + history.replace('1789106369', '1789106372') + end + signal, operations: [], fatal: 1 },
      { name: 'F003 discordant identity retirement interior', log: start + reuse.replace('1789106378', '1789106373') + history.replace('1789106369', '1789106372') + signal + end, operations: [], fatal: 1 },
      { name: 'ordered Chrome to unrelated identity retirement', log: start + reuse.replace('1789106378', '1789106371') + history.replace('1789106369', '1789106372') + signal + end, operations: [], fatal: 0 },
      { name: 'valid planned operation', log: start + begin + kill + signal + finish + end, operations: [operation], fatal: 0 },
      { name: 'F002 discordant reuse', log: start + begin + reuse + kill + signal + finish + end, operations: [operation], fatal: 2 },
    ];
    for (const entry of cases) {
      const logFile = join(fixture.root, 'boundary.log');
      const operations = join(fixture.root, 'operations.json');
      const output = join(fixture.root, 'check.json');
      await writeFile(logFile, entry.log);
      await writeFile(operations, JSON.stringify(entry.operations));
      const result = checkCLI(fixture, { before, after, log: logFile, operations, output });
      const report = JSON.parse(await readFile(output, 'utf8'));
      assert.equal(result.passed, entry.fatal === 0, entry.name);
      assert.equal(report.forcedRestartEvents.length, entry.fatal, entry.name);
      assert.equal(report.eventCounts.fatalEvents, entry.fatal, entry.name);
    }
  });
  test('planned termination exemption requires the actual operation, matching PID/package and time, never a release-suite label', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const operationsFile = join(fixture.root, 'operations.json');
    const logFile = join(fixture.root, 'interval.log');
    const operation = { id: 'cold', measurementId: 'android-test', packageName: 'com.android.chrome', pid: '6538', processes: { '6538': 'com.android.chrome' }, command: ['shell', 'am', 'force-stop', '--user', '0', 'com.android.chrome'], succeeded: true };
    const termination = marker('08.000', 'OP_BEGIN cold com.android.chrome 6538')
      + '09-10 08:45:09.000 546 1761 I ActivityManager: Killing 6538:com.android.chrome/u0a146 (adj 0): stop com.android.chrome due to from pid 2000\n'
      + chromeDeath + marker('10.000', 'OP_END cold com.android.chrome 6538');
    const check = () => checkCLI(fixture, { before, after, log: logFile, operations: operationsFile, output: join(fixture.root, 'check.json') });
    for (const [operations, log, passed] of [
      [[operation], termination, true], [[], termination, false], [[{ ...operation, succeeded: false }], termination, false],
      [[{ ...operation, pid: '7777' }], termination, false], [[{ ...operation, packageName: 'com.example.other' }], termination, false],
      [[operation], termination + chromeDeath.replace('09.613', '12.613'), false],
      [[operation], termination.replace('09-10 08:45:09.000', chromeDeath.replace('09.613', '08.500') + '09-10 08:45:09.000'), false],
      [[operation], termination.replace('stop com.android.chrome due to from pid 2000', 'crash'), false],
      [[operation], termination + '09-10 08:45:09.464 6538 7277 I DynamiteLoaderV2Impl: Module config changed, forcing restart due to module googlecertificates\n', false],
    ] as const) {
      await writeFile(operationsFile, JSON.stringify(operations));
      const interval = marker('00.000', 'START') + log + marker('30.000', 'END');
      for (const framed of [interval, interval.replace(/^09-10 08:45:(\d{2}\.\d{3})/gmu, (_, seconds: string) => `         ${(1789081200 + Number(seconds)).toFixed(3)}`)]) {
        await writeFile(logFile, framed);
        assert.equal(check().passed, passed);
      }
    }
  });
  for (const packageName of ['com.android.chrome', 'org.chromium.webapk.fixture', 'com.google.android.webapk.fixture']) {
    for (const variant of ['planned', 'unplanned', 'unobserved child', 'missing Killing', 'wrong reason', 'wrong user', 'before Killing', 'delayed earlier death', 'before operation', 'after operation', 'reused PID', 'wrong observed name', 'missing process set', 'empty process set', 'malformed process set', 'module change', 'GMS death']) {
      test(`review: ${packageName} process-set check ${variant}`, async () => {
        const fixture = await harness.createFixture();
        const pid = packageName === 'com.android.chrome' ? '6538' : '6600';
        const childName = `${packageName}:renderer`;
        await state(fixture, { absent: true, ...(pid === '6600' ? { foregroundPackage: packageName } : {}), children: { '6700': childName } });
        const { before, after } = await snapshots(fixture);
        const operation = {
          id: 'cold', measurementId: 'android-test', packageName, pid,
          processes: { [pid]: packageName, '6700': childName },
          command: ['shell', 'am', 'force-stop', '--user', '0', packageName], succeeded: true,
        };
        const line = (time: string, tag: string, message: string) => `09-10 08:45:${time} 546 1761 I ${tag}: ${message}\n`;
        const killed = (targetPid: string, name: string) => line(targetPid === pid ? '09.000' : '09.150', 'ActivityManager', `Killing ${targetPid}:${name}/u0a146 (adj 0): stop ${packageName} due to from pid 2000`);
        const death = (time: string, targetPid: string, name: string) => line(time, 'ActivityManager', `Process ${name} (pid ${targetPid}) has died: fg TOP`);
        let body = killed(pid, packageName) + death('09.100', pid, packageName)
          + killed('6700', childName) + death('09.200', '6700', childName)
          + line('09.300', 'Process', 'Sending signal. PID: 6700 SIG: 9')
          + line('09.400', 'Zygote', 'Process 6700 exited due to signal 9 (Killed)');
        if (variant === 'unobserved child') body += killed('6701', `${packageName}:unobserved`);
        if (variant === 'missing Killing') body = body.replace(killed('6700', childName), '');
        if (variant === 'wrong reason') body = body.replaceAll(`stop ${packageName} due to from pid 2000`, 'crash');
        if (variant === 'wrong user') body = body.replaceAll('/u0a146', '/u10a146');
        if (variant === 'before Killing') body = death('08.500', '6700', childName) + body;
        if (variant === 'delayed earlier death') body += death('08.500', '6700', childName);
        if (variant === 'reused PID') body += line('09.500', 'ActivityManager', `Start proc 6700:${childName}/u0a146 for service`) + death('09.600', '6700', childName);
        if (variant === 'module change') body += `09-10 08:45:09.500 6700 6700 I DynamiteLoaderV2Impl: Module config changed, forcing restart due to module googlecertificates\n`;
        if (variant === 'GMS death') body += death('09.500', '1427', 'com.google.android.gms');
        const operations = variant === 'unplanned' ? [] : [{ ...operation,
          ...(variant === 'wrong observed name' ? { processes: { [pid]: packageName, '6700': `${packageName}:other` } } : {}),
          ...(variant === 'missing process set' ? { processes: undefined } : {}),
          ...(variant === 'empty process set' ? { processes: {} } : {}),
          ...(variant === 'malformed process set' ? { processes: { [pid]: packageName, '6700': null } } : {}),
        }];
        const log = marker('00.000', 'START') + (variant === 'before operation' ? death('07.000', pid, packageName) : '') + (variant === 'unplanned' ? body : marker('08.000', `OP_BEGIN cold ${packageName} ${pid}`) + body + marker('10.000', `OP_END cold ${packageName} ${pid}`))
          + (variant === 'after operation' ? death('12.000', '6700', childName) : '') + marker('30.000', 'END');
        const logFile = join(fixture.root, 'interval.log');
        const operationsFile = join(fixture.root, 'operations.json');
        await writeFile(logFile, log);
        await writeFile(operationsFile, JSON.stringify(operations));
        const result = checkCLI(fixture, { before, after, log: logFile, operations: operationsFile, output: join(fixture.root, 'check.json') });
        const check = JSON.parse(await readFile(join(fixture.root, 'check.json'), 'utf8'));
        assert.equal(result.passed, variant === 'planned', JSON.stringify(check));
        if (variant === 'planned') assert.deepEqual(check.forcedRestartEvents, []);
        else if (variant !== 'missing process set') assert.ok(check.forcedRestartEvents.length > 0, JSON.stringify(check));
      });
    }
    for (const event of ['direct death', 'new PID signal']) test(`review: unplanned ${packageName} ${event} cannot pass without a termination journal`, async () => {
      const fixture = await harness.createFixture();
      await state(fixture, { absent: true, ...(packageName === 'com.android.chrome' ? {} : { foregroundPackage: packageName }) });
      const { before, after } = await snapshots(fixture);
      const pid = packageName === 'com.android.chrome' ? '6538' : '6600';
      const log = event === 'new PID signal'
        ? `09-10 08:45:09.000 546 1761 I ActivityManager: Start proc 6800:${packageName}:renderer/u0a146 for service\n09-10 08:45:09.100 6800 6800 I Process: Sending signal. PID: 6800 SIG: 9\n`
        : `09-10 08:45:09.000 546 1761 I ActivityManager: Process ${packageName} (pid ${pid}) has died: fg TOP\n`;
      assert.equal((await harness.check(fixture, before, after, log)).passed, false);
      const check = JSON.parse(await readFile(join(fixture.root, 'check.json'), 'utf8'));
      assert.equal(check.forcedRestartEvents.length, 1);
    });
  }
  test('review: hypothetical initial WebAPK publication inside installation is not a dependency replacement, but its new process death is measured', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const publication = '09-10 08:45:09.000 546 1761 I PackageManager: Successfully installed package org.chromium.webapk.fixture\n'
      + '09-10 08:45:09.100 546 1761 I ActivityManager: Start proc 6600:org.chromium.webapk.fixture/u0a146 for activity\n';
    assert.equal((await harness.check(fixture, before, after, publication)).passed, true);
    assert.equal((await harness.check(fixture, before, after, publication + '09-10 08:45:09.200 6600 6600 I Process: Sending signal. PID: 6600 SIG: 9\n')).passed, false);
  });
  test('current recorded static-library evidence retains complete records and sanitized PR path labels', async () => {
    const entries = JSON.parse(await readFile(repositoryPath('tests/mobile/unit/fixtures/android-iteration-13-evidence.json'), 'utf8')) as Array<{ file: string; sha256: string; classification: string }>;
    for (const entry of entries) assert.equal(await fileSha256(repositoryPath(`tests/mobile/unit/fixtures/${entry.file}`)), entry.sha256);
    for (const run of ['34488014724', '34488020101']) {
      const fixture = await harness.createFixture();
      for (const [suffix, recorded] of [['dump', 'trichrome.dump'], ['list', 'libraries.list']]) {
        await writeFile(join(fixture.fixtureDirectory, `valid-trichrome.${suffix}`), await readFile(repositoryPath(`tests/mobile/unit/fixtures/android-recorded-${run}-${recorded}`)));
      }
      const dump = await readFile(join(fixture.fixtureDirectory, 'valid-trichrome.dump'), 'utf8');
      const path = dump.match(/codePath=(\S+)/u)![1] + '/base.apk';
      await writeFile(join(fixture.fixtureDirectory, 'valid-trichrome.file'), `'${path}'`);
      const result = await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'));
      if (run === '34488014724') assert.equal(result.passed, true, result.stderr);
      else {
        assert.equal(result.passed, false);
        assert.ok(dump.includes('[REDACTED]'));
        assert.equal(result.stderr, 'ANDROID_ENVIRONMENT_INVALID: required environment evidence or operation failed\n');
        assert.ok(entries.filter((entry) => entry.file.includes(run) && /trichrome|libraries/u.test(entry.file)).every((entry) => entry.classification === 'recorded-sanitized'));
        const alias = '/data/app/hypothetical-pr-library';
        for (const suffix of ['dump', 'list']) {
          const filename = join(fixture.fixtureDirectory, `valid-trichrome.${suffix}`);
          const text = await readFile(filename, 'utf8');
          await writeFile(filename, text.replace(/\/data\/app\/~~40L0KnTK29Prxai8pgaj7g==\/com\.google\.android\.\d+\[REDACTED\]-tEf5g==/gu, alias));
        }
        await writeFile(join(fixture.fixtureDirectory, 'valid-trichrome.file'), `'${alias}/base.apk'`);
        const hypothetical = await harness.snapshot(fixture, 'valid', join(fixture.root, 'hypothetical-before.json'), join(fixture.root, 'hypothetical-diagnostics.json'));
        assert.equal(hypothetical.passed, true, `Hypothetical consistent alias, not recovered PR paths: ${hypothetical.stderr}`);
      }
    }
  });
  for (const packageName of ['com.android.chrome', 'org.chromium.webapk.fixture', 'com.google.android.webapk.fixture']) test(`review: measurement collector and real planned ${packageName} process-set force-stop persist a checkable interval without rebaseline`, async () => {
    const fixture = await harness.createFixture();
    const childName = `${packageName}:renderer`;
    const initial = { absent: true, ...(packageName === 'com.android.chrome' ? {} : { foregroundPackage: packageName }) };
    await state(fixture, initial);
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
    try {
      await beginObservedMeasurement(measurement);
      await assert.rejects(measurement.begin(), /rebaseline/u);
      const baseline = JSON.parse(await readFile(join(fixture.root, 'android-environment-before.json'), 'utf8'));
      assert.equal(baseline.measurement.processes['6700'], undefined);
      await state(fixture, { ...initial, children: { '6700': childName } });
      await assert.rejects(measurement.terminate(packageName, '7777'), /PID changed/u);
      await measurement.terminate(packageName, packageName === 'com.android.chrome' ? '6538' : '6600');
      await measurement.finish();
      const check = JSON.parse(await readFile(join(fixture.root, 'android-environment-check.json'), 'utf8'));
      assert.equal(check.passed, true, JSON.stringify(check));
      const operations = JSON.parse(await readFile(join(fixture.root, 'android-environment-operations.json'), 'utf8'));
      assert.equal(operations.length, 1);
      assert.equal(operations[0].succeeded, true);
      assert.equal(operations[0].pid, packageName === 'com.android.chrome' ? '6538' : '6600');
      assert.equal(operations[0].packageName, packageName);
      assert.deepEqual(operations[0].processes, { [operations[0].pid]: packageName, '6700': childName });
      assert.deepEqual(check.forcedRestartEvents, []);
      const requests = await readFile(fixture.log, 'utf8');
      const stop = requests.indexOf(`shell am force-stop --user 0 ${packageName}`);
      assert.ok(requests.lastIndexOf('shell ps -A -o PID,NAME', stop) > requests.lastIndexOf(`shell pidof ${packageName}`, stop));
      assert.ok(requests.indexOf('shell ps -A -o PID,NAME', stop) > stop);
      assert.equal(requests.includes('disable-user'), false);
      await assert.rejects(measurement.terminate('com.android.chrome', '6538'), /outside measurement/u);
    } finally {
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  for (const variant of ['inventory denied', 'malformed inventory', 'main PID changed', 'remaining child', 'new remaining child', 'force-stop denied', 'missing child Killing', 'unobserved child']) {
    test(`review: measured termination fails closed for ${variant}`, async () => {
      const fixture = await harness.createFixture();
      const childName = 'com.android.chrome:renderer';
      const children = { '6700': childName };
      await state(fixture, { absent: true, children });
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), { plannedTerminationExitMs: 300 });
      try {
        await beginObservedMeasurement(measurement);
        const changes = {
          'inventory denied': { processListFail: true },
          'malformed inventory': { processList: 'PID NAME\nmalformed\n' },
          'main PID changed': { processList: 'PID NAME\n6539 com.android.chrome\n6700 com.android.chrome:renderer\n' },
          'remaining child': { remainingChildren: children },
          'new remaining child': { remainingChildren: { '6701': 'com.android.chrome:new' } },
          'force-stop denied': { forceStopFail: true },
          'missing child Killing': { omitKillingPid: '6700' },
          'unobserved child': { unobservedChildren: { '6701': 'com.android.chrome:new' } },
        }[variant];
        await state(fixture, { absent: true, children, ...changes });
        if (variant === 'missing child Killing' || variant === 'unobserved child') {
          await measurement.terminate('com.android.chrome', '6538');
          const outcome = await measurement.finish();
          assert.equal(outcome.assessment?.status, 'FAIL');
          const check = JSON.parse(await readFile(join(fixture.root, 'android-environment-check.json'), 'utf8'));
          assert.equal(check.passed, false);
          assert.ok(check.forcedRestartEvents.some((line: string) => line.includes(variant === 'unobserved child' ? '6701' : '6700')));
          return;
        }
        const terminated = measurement.terminate('com.android.chrome', '6538');
        if (variant.endsWith('remaining child')) await assert.rejects(terminated, /did not remove the observed package process set/u);
        else await assert.rejects(terminated);
        const requests = await readFile(fixture.log, 'utf8');
        const dispatched = !['inventory denied', 'malformed inventory', 'main PID changed'].includes(variant);
        assert.equal(requests.includes('shell am force-stop --user 0'), dispatched);
        const operations = JSON.parse(await readFile(join(fixture.root, 'android-environment-operations.json'), 'utf8'));
        assert.equal(operations.length, dispatched ? 1 : 0);
        if (dispatched) {
          assert.equal(operations[0].succeeded, false);
          assert.deepEqual(operations[0].processes, { '6538': 'com.android.chrome', ...children });
          assert.doesNotMatch(await readFile(join(fixture.fixtureDirectory, 'native.log'), 'utf8'), /OP_END/u);
        }
      } finally {
        await measurement.finish().catch(() => undefined);
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    });
  }
  for (const variant of ['typed timeout', 'typed exit failure', 'unknown error', 'unknown signal', 'invalid fields',
    'receipt write failure', 'unexpected output', 'exit inventory failure', 'OP_END failure', 'complete']) {
    test(`termination command receipt: ${variant}`, async () => {
      const fixture = await harness.createFixture();
      await state(fixture, { absent: true });
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      const lifetime = join(fixture.root, 'android-emulator-lifetime.json');
      const binding = await bindEmulatorChild(process.pid);
      await writeFile(lifetime, JSON.stringify({ schema: 1, binding, bindingSha256: emulatorBindingSha256(binding) }));
      if (variant === 'unknown error') delete process.env.ANDROID_EMULATOR_LIFETIME_FILE;
      else process.env.ANDROID_EMULATOR_LIFETIME_FILE = variant === 'typed exit failure' ? fixture.root : lifetime;
      process.env.ANDROID_EMULATOR_PID = String(process.pid);
      const canary = 'RECEIPT_CANARY_args_output_error_session_device_secret';
      if (variant === 'typed timeout') await writeFile(lifetime, `{${canary}`);
      if (variant === 'invalid fields') await writeFile(lifetime, canary.repeat(100));
      const originalAbortController = globalThis.AbortController;
      const secondary = new Error('receipt evidence write failed');
      const synthetic = !['unexpected output', 'exit inventory failure', 'OP_END failure', 'complete'].includes(variant);
      const helperResult: CommandResult = { code: variant === 'typed exit failure' ? 7 : 1, durationMs: 20_073,
        signal: variant === 'typed exit failure' ? 'SIGKILL' : 'SIGTERM', timedOut: variant !== 'typed exit failure', stdout: canary, stderr: canary };
      if (variant === 'unknown signal') helperResult.signal = canary;
      if (variant === 'invalid fields') Object.assign(helperResult, { code: Number.MAX_SAFE_INTEGER + 1, durationMs: -1, timedOut: canary, signal: canary });
      const primary = variant === 'unknown error'
        ? Object.assign(new Error(`COMMAND_FAILED: ${canary}`), helperResult, { exitCode: 9, args: [canary], sessionId: canary })
        : Object.assign(new CommandError(canary, [canary], helperResult), { sessionId: canary, errorBody: canary });
      const publications: AndroidPlannedTermination[][] = [];
      let commandCalls = 0;
      let successfulResult: CommandResult | undefined;
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
        command: async (binary, args, timeoutMs, options) => {
          commandCalls++;
          assert.deepEqual([binary, args, timeoutMs], ['adb', ['-s', 'emulator-5554', 'shell', 'am', 'force-stop', '--user', '0', 'com.android.chrome'], 20_000]);
          assert.equal(options, undefined);
          if (variant === 'unknown signal' || variant === 'receipt write failure') {
            globalThis.AbortController = class extends originalAbortController {
              constructor() {
                super();
                globalThis.AbortController = originalAbortController;
                if (variant === 'receipt write failure') throw new Error(canary);
                this.abort();
              }
            };
          }
          if (synthetic) throw primary;
          successfulResult = await command(binary, args, timeoutMs, options);
          if (variant === 'exit inventory failure') await state(fixture, { absent: true, processListFail: true });
          return variant === 'unexpected output' ? { ...successfulResult, stdout: canary } : successfulResult;
        },
        writeJson: async (path, value) => {
          if (path === join(fixture.root, 'android-environment-operations.json') && (value as unknown[]).length > 0) {
            const operations = JSON.parse(JSON.stringify(value)) as AndroidPlannedTermination[];
            publications.push(operations);
            if (variant === 'receipt write failure' && operations[0]?.commandReceipt) throw secondary;
          }
          await writeSanitizedJson(path, value);
        },
      });
      try {
        if (variant === 'OP_END failure') {
          const adb = join(fixture.environment.PATH!.split(':')[0], 'adb');
          const source = await readFile(adb, 'utf8');
          await writeFile(adb, source.replace('\n', "\nif (process.argv.slice(2).join(' ').includes(' OP_END ')) process.exit(1);\n"));
        }
        await beginObservedMeasurement(measurement);
        const hostBefore = Date.now();
        let thrown: unknown;
        try { await measurement.terminate('com.android.chrome', '6538'); } catch (error) { thrown = error; }
        const hostAfter = Date.now();
        const operations = JSON.parse(await readFile(join(fixture.root, 'android-environment-operations.json'), 'utf8')) as AndroidPlannedTermination[];
        const retained = measurement.plannedOperations()[0];
        assert.ok(retained.commandReceipt, 'planned termination is missing its command receipt');
        const receipt = retained.commandReceipt;
        if (variant === 'unknown error') assert.equal(retained.hostEmulatorAtSettlement, undefined);
        else {
          assert.ok(retained.hostEmulatorAtSettlement, 'planned termination is missing host settlement evidence');
          const host = retained.hostEmulatorAtSettlement;
          assert.equal(host.hostSettledAtMs, receipt.hostSettledAtMs);
          assert.ok(host.sampleStartedAtMs >= host.hostSettledAtMs && host.sampleEndedAtMs <= hostAfter);
          const unknownReason = { 'typed exit failure': 'io-error', 'typed timeout': 'invalid-receipt',
            'invalid fields': 'byte-limit', 'unknown signal': 'probe-timeout', 'receipt write failure': 'io-error' }[variant];
          assert.equal(host.state, unknownReason || process.platform !== 'linux' ? 'unknown' : 'alive');
          assert.equal(host.reason, unknownReason ?? (process.platform === 'linux' ? 'same-birth' : 'unsupported-os'));
          if (!unknownReason) assert.equal(host.bindingSha256, emulatorBindingSha256(binding));
          assert.equal(host.nativeExitCoverage, 'unknown');
          assert.equal(host.nativeExit, undefined);
          assert.doesNotMatch(JSON.stringify(host), /RECEIPT_CANARY|errorBody|stack/u);
          assert.deepEqual(publications[1][0].hostEmulatorAtSettlement, host);
          if (variant !== 'receipt write failure') assert.deepEqual(operations[0].hostEmulatorAtSettlement, host);
        }
        assert.deepEqual(Object.keys(receipt).sort(), ['operation', 'source', 'timeoutMs', 'hostStartedAtMs', 'hostSettledAtMs',
          'durationMs', 'helperExitCode', 'signal', 'timedOut'].sort());
        assert.equal(receipt.operation, 'am-force-stop-user-0');
        assert.equal(receipt.timeoutMs, 20_000);
        assert.ok(Number.isSafeInteger(receipt.hostStartedAtMs) && receipt.hostStartedAtMs! >= hostBefore);
        assert.ok(Number.isSafeInteger(receipt.hostSettledAtMs) && receipt.hostSettledAtMs! >= receipt.hostStartedAtMs!
          && receipt.hostSettledAtMs! <= hostAfter);
        assert.ok(Buffer.byteLength(JSON.stringify(receipt)) < 512);
        assert.equal(JSON.stringify(receipt).includes(canary), false);
        assert.equal(JSON.stringify(operations).includes(canary), false);
        assert.equal(commandCalls, 1);
        assert.equal(retained.succeeded, variant === 'complete');
        assert.equal(operations[0].succeeded, variant === 'complete');
        assert.equal(publications.length, variant === 'complete' ? 3 : 2);
        assert.equal(publications[0][0].commandReceipt, undefined);
        assert.equal(publications[1][0].succeeded, false);
        assert.deepEqual(publications[1][0].commandReceipt, receipt);
        if (variant === 'receipt write failure') {
          assert.equal(operations[0].commandReceipt, undefined);
          assert.ok(measurement.failureOutcome().errors.includes(secondary));
        } else assert.deepEqual(operations[0].commandReceipt, receipt);
        if (synthetic) {
          assert.equal(thrown, primary);
          assert.equal(receipt.source, variant === 'unknown error' ? 'unknown-error' : 'command-error');
          assert.equal(receipt.helperExitCode, ['unknown error', 'invalid fields'].includes(variant) ? null : helperResult.code);
          assert.equal(receipt.durationMs, ['unknown error', 'invalid fields'].includes(variant) ? null : helperResult.durationMs);
          assert.equal(receipt.timedOut, ['unknown error', 'invalid fields'].includes(variant) ? null : helperResult.timedOut);
          assert.equal(receipt.signal, variant === 'unknown error' ? null : ['unknown signal', 'invalid fields'].includes(variant) ? 'OTHER' : helperResult.signal);
        } else {
          assert.equal(receipt.source, 'command-result');
          assert.equal(receipt.helperExitCode, 0);
          assert.equal(receipt.durationMs, successfulResult!.durationMs);
          assert.equal(receipt.signal, null);
          assert.equal(receipt.timedOut, false);
        }
        const latch = new QualificationFailureLatch();
        if (variant === 'complete') assert.equal(thrown, undefined);
        else {
          assert.ok(thrown);
          let wrapped: unknown;
          try { latch.fail(thrown, 'lifecycle'); } catch (error) { wrapped = error; }
          assert.ok(wrapped instanceof QualificationFatalError);
          assert.equal(wrapped.cause, thrown);
          assert.throws(() => latch.assertClear(), error => error === wrapped);
          const ledger = new FailureLedger();
          const measured = measurement.failureOutcome();
          const category = measured.errors.some(error => error === wrapped || error === wrapped.cause) ? 'COLLECTION' : 'PRODUCT';
          ledger.retain(wrapped, 'lifecycle', category);
          assert.equal(ledger.snapshot().primary_failure?.category, 'PRODUCT');
          if (synthetic) assert.equal(latch.snapshot()?.code, 'COMMAND_FAILED');
          assert.equal((latch.snapshot() as any).durationMs, undefined);
          assert.deepEqual(measurement.plannedOperations()[0].commandReceipt, receipt);
        }
        assert.deepEqual(measurement.failureOutcome().errors, variant === 'receipt write failure' ? [secondary] : []);
        const requests = await readFile(fixture.log, 'utf8');
        assert.doesNotMatch(requests, /shell am start|KEYCODE_HOME|disable-user/u);
        if (variant !== 'complete' && variant !== 'OP_END failure') assert.doesNotMatch(requests, /OP_END/u);
        await state(fixture, { absent: true });
        const outcome = await measurement.finish();
        if (variant === 'receipt write failure') assert.ok(outcome.errors.includes(secondary));
        if (variant === 'complete') assert.equal(outcome.assessment?.status, 'PASS');
        if (['exit inventory failure', 'OP_END failure', 'unexpected output'].includes(variant)) assert.equal(outcome.assessment?.status, 'FAIL');
        assert.equal(qualified({ product: { status: thrown ? 'FAIL' : 'PASS', categories: [] },
          finalization: { status: outcome.errors.length ? 'FAIL' : 'PASS', failures: [] },
          environment_qualification: { platform: 'android', status: outcome.assessment?.status ?? 'UNKNOWN',
            collection: outcome.assessment?.collection.status ?? 'UNKNOWN', assessmentSha256: outcome.assessmentSha256 ?? '', measurementId: measurement.id } }), variant === 'complete');
        assert.equal(measurement.plannedOperations()[0].succeeded, variant === 'complete');
      } finally {
        globalThis.AbortController = originalAbortController;
        await measurement.finish().catch(() => undefined);
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    });
  }
  for (const hostConfigured of [true, false]) test(`termination command receipt: required write failure with host ${hostConfigured ? 'configured' : 'unconfigured'}`, async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { absent: true });
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    if (hostConfigured) {
      const lifetime = join(fixture.root, 'android-emulator-lifetime.json');
      const binding = await bindEmulatorChild(process.pid);
      await writeFile(lifetime, JSON.stringify({ schema: 1, binding, bindingSha256: emulatorBindingSha256(binding) }));
      process.env.ANDROID_EMULATOR_LIFETIME_FILE = lifetime;
      process.env.ANDROID_EMULATOR_PID = String(process.pid);
    } else delete process.env.ANDROID_EMULATOR_LIFETIME_FILE;
    const primary = new Error('required command receipt write failed');
    const publications: AndroidPlannedTermination[][] = [];
    let receiptPublications = 0;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      writeJson: async (path, value) => {
        if (path === join(fixture.root, 'android-environment-operations.json') && (value as unknown[]).length > 0) {
          const operations = JSON.parse(JSON.stringify(value)) as AndroidPlannedTermination[];
          publications.push(operations);
          if (operations[0]?.commandReceipt && receiptPublications++ === 0) throw primary;
        }
        await writeSanitizedJson(path, value);
      },
    });
    try {
      await beginObservedMeasurement(measurement);
      await assert.rejects(measurement.terminate('com.android.chrome', '6538'), error => error === primary);
      const operation = measurement.plannedOperations()[0];
      assert.equal(operation.succeeded, false);
      assert.equal(operation.commandReceipt?.source, 'command-result');
      assert.equal(operation.commandReceipt?.helperExitCode, 0);
      assert.equal(operation.commandReceipt?.timedOut, false);
      assert.equal(Boolean(operation.hostEmulatorAtSettlement), hostConfigured);
      assert.deepEqual(measurement.failureOutcome().errors, []);
      assert.equal(publications.length, 2);
      assert.equal(publications[1][0].succeeded, false);
      assert.deepEqual(publications[1][0].commandReceipt, operation.commandReceipt);
      const persisted = JSON.parse(await readFile(join(fixture.root, 'android-environment-operations.json'), 'utf8'));
      assert.equal(persisted[0].succeeded, false);
      assert.equal(persisted[0].commandReceipt, undefined);
      const requests = (await readFile(fixture.log, 'utf8')).trim().split('\n');
      const stop = requests.findIndex(line => line.includes('shell am force-stop --user 0 com.android.chrome'));
      assert.ok(stop >= 0);
      assert.deepEqual(requests.slice(stop + 1), []);
      assert.doesNotMatch(requests.join('\n'), /OP_END|shell am start|KEYCODE_HOME|disable-user/u);
    } finally {
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  for (const variant of ['read failure', 'invalid receipt', 'byte limit', 'probe timeout', 'probe rejection', 'required collection failure']) test(`termination command receipt: successful command keeps inventory and OP_END after host probe ${variant}`, async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { absent: true });
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment, { ANDROID_EMULATOR_LIFETIME_FILE: fixture.root, ANDROID_EMULATOR_PID: '71' });
    const canary = 'HOST_PROBE_PRIVATE_CANARY';
    if (variant === 'invalid receipt' || variant === 'byte limit') {
      const lifetime = join(fixture.root, 'android-emulator-lifetime.json');
      await writeFile(lifetime, variant === 'invalid receipt' ? `{${canary}` : canary.repeat(100));
      process.env.ANDROID_EMULATOR_LIFETIME_FILE = lifetime;
    }
    const originalAbortController = globalThis.AbortController;
    const publications: AndroidPlannedTermination[][] = [];
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      command: async (...args) => {
        const result = await command(...args);
        if (variant === 'probe timeout' || variant === 'probe rejection') {
          globalThis.AbortController = class extends originalAbortController {
            constructor() {
              super();
              globalThis.AbortController = originalAbortController;
              if (variant === 'probe rejection') throw new Error(canary);
              this.abort();
            }
          };
        }
        return result;
      },
      writeJson: async (path, value) => {
        if (path === join(fixture.root, 'android-environment-operations.json') && (value as unknown[]).length > 0) {
          const operations = JSON.parse(JSON.stringify(value)) as AndroidPlannedTermination[];
          publications.push(operations);
          const requests = await readFile(fixture.log, 'utf8');
          if (operations[0]?.succeeded) assert.match(requests, / OP_END /u);
          else assert.doesNotMatch(requests, / OP_END /u);
        }
        await writeSanitizedJson(path, value);
      },
    });
    if (variant === 'required collection failure') await mkdir(join(fixture.root, 'android-qualification-logcat.log'), { recursive: true });
    try {
      await beginObservedMeasurement(measurement);
      await measurement.terminate('com.android.chrome', '6538');
      const operation = measurement.plannedOperations()[0];
      assert.equal(operation.succeeded, true);
      assert.equal(operation.hostEmulatorAtSettlement?.state, 'unknown');
      const reason = variant === 'invalid receipt' ? 'invalid-receipt' : variant === 'byte limit' ? 'byte-limit'
        : variant === 'probe timeout' ? 'probe-timeout' : 'io-error';
      assert.equal(operation.hostEmulatorAtSettlement?.reason, reason);
      assert.equal(operation.hostEmulatorAtSettlement?.nativeExitCoverage, 'unknown');
      assert.equal(operation.hostEmulatorAtSettlement?.nativeExit, undefined);
      assert.equal(operation.commandReceipt?.helperExitCode, 0);
      assert.equal(operation.commandReceipt?.source, 'command-result');
      assert.equal(operation.commandReceipt?.timeoutMs, 20_000);
      assert.equal(operation.commandReceipt?.timedOut, false);
      assert.deepEqual(Object.keys(operation.commandReceipt!).sort(), ['operation', 'source', 'timeoutMs', 'hostStartedAtMs', 'hostSettledAtMs',
        'durationMs', 'helperExitCode', 'signal', 'timedOut'].sort());
      assert.ok(Buffer.byteLength(JSON.stringify(operation.commandReceipt)) < 512);
      assert.deepEqual(publications.map(operations => operations[0].succeeded), [false, false, true]);
      assert.deepEqual(publications[1][0].commandReceipt, operation.commandReceipt);
      assert.deepEqual(publications[1][0].hostEmulatorAtSettlement, operation.hostEmulatorAtSettlement);
      const requests = await readFile(fixture.log, 'utf8');
      const stop = requests.indexOf('shell am force-stop --user 0 com.android.chrome');
      const end = requests.indexOf(' OP_END ');
      assert.ok(stop >= 0 && end > stop);
      assert.match(requests.slice(stop, end), /shell ps -A -o PID,NAME/u);
      assert.doesNotMatch(requests, /shell am start|KEYCODE_HOME|disable-user/u);
      const persisted = JSON.parse(await readFile(join(fixture.root, 'android-environment-operations.json'), 'utf8'));
      assert.deepEqual(persisted[0], operation);
      const host = operation.hostEmulatorAtSettlement!;
      assert.deepEqual(Object.keys(host).sort(), ['hostSettledAtMs', 'sampleStartedAtMs', 'sampleEndedAtMs', 'state', 'reason', 'nativeExitCoverage'].sort());
      assert.equal(host.hostSettledAtMs, operation.commandReceipt!.hostSettledAtMs);
      assert.ok(host.sampleStartedAtMs >= host.hostSettledAtMs && host.sampleEndedAtMs >= host.sampleStartedAtMs);
      assert.ok(Buffer.byteLength(JSON.stringify(host)) < 512);
      assert.doesNotMatch(JSON.stringify(host), /HOST_PROBE_PRIVATE_CANARY|\/private|\/proc|EISDIR|EACCES|errorBody|stack/u);
      const outcome = await measurement.finish();
      const requiredFailure = variant === 'required collection failure';
      assert.equal(outcome.assessment?.collection.status, requiredFailure ? 'UNKNOWN' : 'PASS');
      assert.equal(outcome.assessment?.status, requiredFailure ? 'UNKNOWN' : 'PASS');
      assert.equal(outcome.assessment?.passed, !requiredFailure);
      if (requiredFailure) assert.ok(outcome.errors.length > 0);
      else {
        assert.deepEqual(outcome.errors, []);
        assert.deepEqual(measurement.failureOutcome().errors, []);
      }
      assert.equal(qualified({ product: { status: 'PASS', categories: [] },
        finalization: { status: outcome.errors.length ? 'FAIL' : 'PASS', failures: [] },
        environment_qualification: { platform: 'android', status: outcome.assessment?.status ?? 'UNKNOWN',
          collection: outcome.assessment?.collection.status ?? 'UNKNOWN', assessmentSha256: outcome.assessmentSha256 ?? '', measurementId: measurement.id } }), !requiredFailure);
    } finally {
      globalThis.AbortController = originalAbortController;
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('review: measured termination waits for force-stopped processes to exit before ending the operation', async () => {
    const fixture = await harness.createFixture();
    const children = { '6700': 'com.android.chrome:renderer' };
    await state(fixture, { absent: true, children });
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
    try {
      await beginObservedMeasurement(measurement);
      await state(fixture, { absent: true, children, lingeringInventories: 2 });
      await measurement.terminate('com.android.chrome', '6538');
      await measurement.finish();
      const operations = JSON.parse(await readFile(join(fixture.root, 'android-environment-operations.json'), 'utf8'));
      assert.equal(operations[0].succeeded, true);
      const check = JSON.parse(await readFile(join(fixture.root, 'android-environment-check.json'), 'utf8'));
      assert.equal(check.passed, true, JSON.stringify(check));
      const requests = await readFile(fixture.log, 'utf8');
      const stop = requests.indexOf('shell am force-stop --user 0 com.android.chrome');
      const end = requests.indexOf('OP_END', stop);
      assert.ok(stop >= 0 && end > stop);
      assert.equal(requests.slice(stop, end).split('shell ps -A -o PID,NAME').length - 1, 3);
      const events = await readFile(join(fixture.fixtureDirectory, 'native.log'), 'utf8');
      const lateExit = events.indexOf('Zygote: Process 6538 exited due to signal 9 (Killed)');
      assert.ok(lateExit >= 0 && lateExit < events.indexOf(' OP_END '), events);
    } finally {
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('measurement collector failure is fatal and cannot leave an active descendant', async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { absent: true, logcatFail: true });
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
    try {
      await assert.rejects(beginObservedMeasurement(measurement), /collector/u);
      const outcome = await measurement.finish();
      assert.ok(outcome.errors.some(error => /no active measurement/u.test(String(error))));
      const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
      assert.ok(collector.failure);
    } finally {
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('post-baseline re-enable fails AFTER acquisition without any repair', async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { enabled: 3 });
    const result = prepare(fixture);
    assert.equal(result.passed, true, result.stderr);
    const before = join(fixture.root, 'before.json');
    assert.equal((await harness.snapshot(fixture, 'valid', before, join(fixture.root, 'before-diagnostics.json'))).passed, true);
    await state(fixture, { enabled: 1 });
    const after = join(fixture.root, 'after.json');
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'after-diagnostics.json'))).passed, false);
    assert.equal(existsSync(after), false);
    assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    const check = await harness.check(fixture, before, after);
    assert.equal(check.passed, false);
    assert.match(check.issues.join(';'), /input unavailable|snapshot invalid/u);
  });
  test('SM59 report and qualification preserve the immutable assessment and current row binding', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const snapshot = JSON.parse(await readFile(after, 'utf8')) as AndroidEnvironmentSnapshot;
    snapshot.packages['com.google.android.gms'].dependencyConfig.enabledComponents = ['com.google.android.gms.fixture.ComponentChanged'];
    rehashPackage(snapshot.packages['com.google.android.gms']);
    await writeFile(after, JSON.stringify(snapshot));
    const log = join(fixture.root, 'report.log'), operations = join(fixture.root, 'report-operations.json'), output = join(fixture.root, 'report-check.json');
    await writeFile(log, marker('00.000', 'START') + marker('30.000', 'END')); await writeFile(operations, '[]');
    const report = checkCLI(fixture, { before, after, log, operations, output }, 'report');
    assert.equal(report.passed, true, report.stderr);
    const strict = checkCLI(fixture, { before, after, log, operations, output }, 'qualification');
    assert.equal(strict.passed, false);
    const checkPath = join(report.directory, 'android-environment-check.json');
    const original = await readFile(checkPath, 'utf8');
    const alteredArgs = [...report.args]; alteredArgs[alteredArgs.indexOf('--attempt') + 1] = '1';
    assert.equal(cli(fixture, alteredArgs).passed, false);
    assert.equal(await readFile(checkPath, 'utf8'), original);
    for (const field of ['status', 'schema', 'counts']) {
      const changed = JSON.parse(original);
      if (field === 'status') changed.status = 'PASS';
      if (field === 'schema') changed.schema = 2;
      if (field === 'counts') changed.eventCounts.rawEvents++;
      const bytes = JSON.stringify(changed); await writeFile(checkPath, bytes);
      assert.equal(cli(fixture, report.args).passed, false, field);
      assert.equal(await readFile(checkPath, 'utf8'), bytes);
    }
  });
  test('Android-only declarations and the shared setup path preserve policy, pinning and read-only postchecks', async () => {
    const policy = JSON.parse(await readFile(repositoryPath('tests/mobile/toolchains.json'), 'utf8')).android;
    assert.equal(policy.vendingPolicy, 'absent-or-disabled-user-0');
    assert.equal(policy.systemImagePolicy, 'owned-google-apis-emulator');
    assert.equal('playStore' in policy, false);
    assert.equal(policy.browserSha256, '261439a1ed20090f9f2f9aeef64024c1cfb9f242d4770f8f7c8f2e777843a35a');
    assert.equal(policy.trichromeLibrarySha256, 'f7d82fa76a99f13980c4205484f4ae78742755d2e9b7276fd60c20e3cd7f9090');
    const workflow = (globalThis as unknown as { Bun: { YAML: { parse(source: string): any } } }).Bun.YAML.parse(await readFile(repositoryPath('.github/workflows/mobile-ci.yml'), 'utf8'));
    const device = workflow.jobs.device;
    assert.deepEqual(device.needs, ['provenance', 'prepare']);
    assert.equal(device.steps.filter((step: any) => step.uses === './.github/actions/mobile-device-run').length, 1);
    assert.equal(device.steps.some((step: any) => step.run), false);
    const source = await readFile(repositoryPath('.github/actions/mobile-device-run/action.yml'), 'utf8');
    assert.match(source, /android-environment\.ts prepare/u);
    assert.doesNotMatch(source, /android-environment\.ts snapshot|logcat -c|google-apis-without-play-store/u);
    const assertPostcheck = (text: string) => {
      const start = text.indexOf('name: Validate immutable Android environment report');
      const end = text.indexOf('name: Sanitize bounded diagnostics');
      assert.ok(start >= 0 && end > start);
      const post = text.slice(start, end);
      assert.match(post, /android-environment\.ts check --contract report/u);
      assert.match(post, /--directory "\$MOBILE_OUTPUT"/u);
      assert.match(post, /--identity "\$MOBILE_OUTPUT\/android-environment-identity\.json"/u);
      assert.doesNotMatch(post, /--output|--operations|disable-user|adb |prepare|>.*android-environment-check/u);
    };
    assertPostcheck(source);
    for (const binding of ['--directory "$MOBILE_OUTPUT"', '--identity "$MOBILE_OUTPUT/android-environment-identity.json"']) assert.throws(() => assertPostcheck(source.replace(binding, '--removed binding')));
    assert.throws(() => assertPostcheck(source.replace('check --contract report', 'check --contract report --output overwrite.json')));
    assert.throws(() => assertPostcheck(source.replace('name: Validate immutable Android environment report', 'name: Removed')));
    assert.match(source, /apksigner verify --verbose --print-certs/u);
    assert.match(source, /sha256sum --check --status/u);

  });
  return tests;
}
