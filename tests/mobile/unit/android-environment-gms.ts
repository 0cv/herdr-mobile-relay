import { execFileSync } from 'node:child_process';
import { closeSync, createWriteStream, existsSync, openSync } from 'node:fs';
import { chmod, mkdir, readdir, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { repositoryPath } from '../support/paths';
import { fileSha256 } from '../support/artifacts';
import { parseAndroidGmsComponentState, type AndroidEnvironmentSnapshot } from '../android-environment';
import { AndroidEnvironmentMeasurement as Measurement, type AndroidMeasurementIO } from '../android-measurement';
import { BoundedCommandError, boundedCommand } from '../support/bounded-process';
import { monotonicNowNs, spawnOwnedProcess } from '../support/owned-process';
import { PhaseBudget } from '../support/budget';
import { measurementIdentity } from './android-product-environment';
import assert from 'node:assert/strict';
import { androidCliTestRegistry, type Harness, type Test, AndroidEnvironmentMeasurement, beginObservedMeasurement, groupPollTestTimers, observerRecords, createBoundedSupervisorObserver, boundedCommandFailureDetails, gmsAttackTimingRule, gmsTimeoutProbeBudgetMs, settledResult, internalGmsFinalization, awaitGatedEntry, gmsStagePersistencePrecondition, gmsWriterDiagnostics, eventLoopStallWindow, createAndroidTransportTestEndpoint, createAndroidTransportFixture, hashText, fakeCommandResult, decodeRetainedHash } from './android-environment';

export function androidEnvironmentGmsObservationTests(harness: Harness): Test[] {
  const { tests, test } = androidCliTestRegistry();
  const withMeasurementOutput = async (body: (output: string, measurement: Measurement) => Promise<void>, io: Partial<AndroidMeasurementIO> = {}) => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const output = join(fixture.root, 'evidence');
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', output, repositoryPath('tests/mobile/toolchains.json'), io);
    try {
      await body(output, measurement);
    } finally {
      await chmod(output, 0o700).catch(() => undefined);
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  };
  const evidenceDirectoryState = async (directory: string) => {
    const state: Record<string, unknown> = {};
    for (const name of (await readdir(directory)).sort()) {
      const path = join(directory, name);
      const info = await stat(path);
      state[name] = info.isDirectory() ? 'directory'
        : { mode: info.mode & 0o777, mtimeMs: info.mtimeMs, sha256: hashText(await readFile(path, 'utf8')) };
    }
    return state;
  };
  const assertEvidenceDirectorySettled = async (directory: string) => {
    const settled = await evidenceDirectoryState(directory);
    await new Promise(resolve => setTimeout(resolve, 300));
    assert.deepEqual(await evidenceDirectoryState(directory), settled);
    assert.deepEqual(Object.keys(settled).filter(name => name.endsWith('.pending')), []);
  };
  const readEvidenceJson = async (directory: string, name: string) => JSON.parse(await readFile(join(directory, `android-environment-${name}.json`), 'utf8'));
  test('measurement evidence keeps its names, bytes and mode, leaves no pending file and stays unchanged after finish', async () => {
    await withMeasurementOutput(async (output, measurement) => {
      await beginObservedMeasurement(measurement);
      const outcome = await measurement.finish();
      assert.deepEqual(outcome.errors, []);
      const names = (await readdir(output)).sort();
      assert.deepEqual(names, ['after', 'after-diagnostics', 'before', 'before-diagnostics', 'check', 'collector', 'completion', 'gms-observations', 'identity', 'operations', 'session']
        .map(name => `android-environment-${name}.json`).concat('android-qualification-logcat.log').sort());
      for (const name of names) assert.equal((await stat(join(output, name))).mode & 0o777, 0o600, name);
      for (const name of names.filter(name => name.endsWith('.json') && !name.endsWith('-session.json'))) {
        const text = await readFile(join(output, name), 'utf8');
        assert.equal(text, `${JSON.stringify(JSON.parse(text), null, 2)}\n`, name);
      }
      assert.equal(await readFile(join(output, 'android-environment-session.json'), 'utf8'), `${JSON.stringify({ id: measurement.id })}\n`);
      assert.deepEqual(await readEvidenceJson(output, 'operations'), []);
      const gmsBytes = await readFile(join(output, 'android-environment-gms-observations.json'), 'utf8');
      assert.equal((await readEvidenceJson(output, 'collector')).gmsObservations.observationSha256, hashText(gmsBytes));
      assert.equal(outcome.assessmentSha256, hashText(await readFile(join(output, 'android-environment-check.json'), 'utf8')));
      assert.deepEqual(await readEvidenceJson(output, 'completion'), { passed: true, errors: [], assessmentSha256: outcome.assessmentSha256 });
      const log = await readFile(join(output, 'android-qualification-logcat.log'), 'utf8');
      assert.match(log, new RegExp(`HerdrMeasure: ${measurement.id} START`, 'u'));
      assert.match(log, new RegExp(`HerdrMeasure: ${measurement.id} END`, 'u'));
      await assertEvidenceDirectorySettled(output);
    });
  });
  test('measurement identity write blocked by a directory is reported, leaves no pending file and publishes no success', async () => {
    await withMeasurementOutput(async (output, measurement) => {
      await mkdir(join(output, 'android-environment-identity.json'), { recursive: true });
      await beginObservedMeasurement(measurement);
      const outcome = await measurement.finish();
      assert.ok(outcome.errors.length > 0);
      assert.equal((await stat(join(output, 'android-environment-identity.json'))).isDirectory(), true);
      const completion = await readEvidenceJson(output, 'completion');
      assert.equal(completion.passed, false);
      assert.ok(completion.errors.length > 0);
      await assertEvidenceDirectorySettled(output);
    });
  });
  test('measurement GMS observation write blocked by a directory is reported and never confirmed', async () => {
    await withMeasurementOutput(async (output, measurement) => {
      await mkdir(join(output, 'android-environment-gms-observations.json'), { recursive: true });
      await beginObservedMeasurement(measurement);
      const outcome = await measurement.finish();
      assert.ok(outcome.errors.some(error => /GMS/u.test(String(error))), JSON.stringify(outcome.errors.map(String)));
      assert.equal((await stat(join(output, 'android-environment-gms-observations.json'))).isDirectory(), true);
      const collector = await readEvidenceJson(output, 'collector');
      assert.equal(collector.gmsObservations.terminalConfirmation, 'unconfirmed');
      assert.equal(collector.gmsObservations.observationSha256, undefined);
      assert.equal((await readEvidenceJson(output, 'completion')).passed, false);
      await assertEvidenceDirectorySettled(output);
    });
  });
  test('measurement writes into a read-only output directory are reported and change nothing', async () => {
    if (process.getuid?.() === 0) return;
    await withMeasurementOutput(async (output, measurement) => {
      await beginObservedMeasurement(measurement);
      const beforeFinish = await evidenceDirectoryState(output);
      await chmod(output, 0o500);
      const outcome = await measurement.finish();
      await chmod(output, 0o700);
      assert.ok(outcome.errors.length > 0);
      assert.equal(outcome.assessment?.passed, false);
      assert.equal(outcome.assessmentSha256, undefined);
      assert.deepEqual(await evidenceDirectoryState(output), beforeFinish);
      await assertEvidenceDirectorySettled(output);
    });
  });
  test('measurement begin refuses an existing session file and leaves it untouched', async () => {
    await withMeasurementOutput(async (output, measurement) => {
      const budget = new PhaseBudget('measurement-session', { timeoutMs: 60_000 });
      await measurement.observeGmsStage('after-device-start', budget);
      await measurement.observeGmsStage('before-measurement-begin', budget);
      const session = join(output, 'android-environment-session.json');
      await writeFile(session, 'previous session\n');
      await assert.rejects(measurement.begin());
      assert.equal(await readFile(session, 'utf8'), 'previous session\n');
      const outcome = await measurement.finish();
      assert.ok(outcome.errors.length > 0);
      assert.equal(await readFile(session, 'utf8'), 'previous session\n');
      assert.equal((await readEvidenceJson(output, 'completion')).passed, false);
      await assertEvidenceDirectorySettled(output);
    });
  });
  test('GMS failed-begin cleanup retires the owned collector tree and freezes its callbacks', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const readyPath = join(fixture.root, 'collector-descendant-ready');
    const latePath = join(fixture.root, 'collector-descendant-late-write');
    const childCode = `process.on('SIGTERM',()=>{});const fs=require('node:fs');setTimeout(()=>{fs.writeFileSync(${JSON.stringify(latePath)},'late');process.stdout.write('late\\n')},1500);setTimeout(()=>process.exit(0),6000);setInterval(()=>process.stdout.write('streaming\\n'),20)`;
    const parentCode = `const fs=require('node:fs');const cp=require('node:child_process');cp.spawn(process.execPath,['-e',${JSON.stringify(childCode)}],{stdio:'inherit'});fs.writeFileSync(${JSON.stringify(readyPath)},'ready');process.on('SIGTERM',()=>process.exit(0));setTimeout(()=>process.exit(0),6000);setInterval(()=>{},1000)`;
    let collectorPid: number | undefined;
    let collectorClosed = false;
    let collectorOwned: ReturnType<typeof spawnOwnedProcess> | undefined;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      spawnCollector: () => {
        collectorOwned = spawnOwnedProcess(process.execPath, ['-e', parentCode], { timeoutMs: 10_000, cleanupReservationMs: 1_000 });
        collectorOwned.supervisor.once('close', () => { collectorClosed = true; });
        collectorOwned.started.then(binding => { collectorPid = binding?.groupID; });
        return collectorOwned;
      },
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('snapshot')) {
          const readyDeadline = Date.now() + 2_000;
          while (!existsSync(readyPath) && Date.now() < readyDeadline) await new Promise(resolve => setTimeout(resolve, 5));
          assert.ok(existsSync(readyPath));
          throw new Error('synthetic failed-begin snapshot after collector launch');
        }
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      await measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-collector-tree', { timeoutMs: 15_000 }));
      await measurement.observeGmsStage('before-measurement-begin', new PhaseBudget('gms-collector-tree', { timeoutMs: 15_000 }));
      await assert.rejects(measurement.begin(), /synthetic failed-begin snapshot after collector launch/u);
      assert.ok(collectorPid);
      assert.equal(collectorClosed, true);
      assert.throws(() => process.kill(-collectorPid!, 0), { code: 'ESRCH' });
      const outcome = await measurement.finish();
      assert.ok(outcome.errors.some(error => String(error).includes('synthetic failed-begin snapshot after collector launch')));
      const collectorPath = join(fixture.root, 'android-environment-collector.json');
      const logPath = join(fixture.root, 'android-qualification-logcat.log');
      const collectorBytes = await readFile(collectorPath);
      const logBytes = await readFile(logPath);
      await new Promise(resolve => setTimeout(resolve, 1_600));
      assert.equal(existsSync(latePath), false);
      assert.deepEqual(await readFile(collectorPath), collectorBytes);
      assert.deepEqual(await readFile(logPath), logBytes);
      assert.throws(() => process.kill(-collectorPid!, 0), { code: 'ESRCH' });
    } finally {
      await measurement.finish().catch(() => undefined);
      await collectorOwned?.retire('test-cleanup').catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('canonical GMS parser rejects malformed headings and contradictory component sections', async () => {
    const fixture = await harness.createFixture();
    const source = await readFile(join(fixture.fixtureDirectory, 'valid-gms.dump'), 'utf8');
    assert.deepEqual(parseAndroidGmsComponentState(source).enabledComponents, []);
    const missingColon = source.replace('Queries:', '      enabledComponents\n        com.google.android.gms.fixture.Component\nQueries:');
    const equalsHeading = source.replace('Queries:', '      enabledComponents=\n        com.google.android.gms.fixture.Component\nQueries:');
    const firstInstallLine = source.split('\n').find(line => line.includes('firstInstallTime='))!;
    const misplacedHeading = source.replace(firstInstallLine,
      `${firstInstallLine}\n    enabledComponents:\n      com.google.android.gms.fixture.Component`);
    const packageBoundaryHeading = source.replace('Queries:',
      '  enabledComponents:\n    com.google.android.gms.fixture.Component\nQueries:');
    assert.throws(() => parseAndroidGmsComponentState(missingColon), /malformed enabledComponents section heading/u);
    assert.throws(() => parseAndroidGmsComponentState(equalsHeading), /malformed enabledComponents section heading/u);
    assert.throws(() => parseAndroidGmsComponentState(misplacedHeading), /outside User 0/u);
    assert.throws(() => parseAndroidGmsComponentState(packageBoundaryHeading), /malformed or outside its package record/u);
    const contradictory = source.replace('Queries:', '      enabledComponents:\n        com.google.android.gms.fixture.Component\n      disabledComponents:\n        com.google.android.gms.fixture.Component\nQueries:');
    assert.throws(() => parseAndroidGmsComponentState(contradictory), /contradict/u);
    const invalidMember = source.replace('Queries:', '      enabledComponents:\n        not-a-component\nQueries:');
    assert.throws(() => parseAndroidGmsComponentState(invalidMember), /malformed enabledComponents component names/u);
  });
  test('GMS ownership failures are excluded from dump output and never dispatch dumpsys', async () => {
    for (const failure of ['wrong-owner', 'missing-marker'] as const) {
      const fixture = await harness.createFixture();
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      const marker = fixture.environment.MOBILE_DEVICE_OWNERSHIP_FILE!;
      if (failure === 'wrong-owner') await writeFile(marker, 'android:another-device');
      else await rm(marker);
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
      try {
        await measurement.observeGmsStage('after-device-start', new PhaseBudget(`gms-owner-${failure}`, { timeoutMs: 10_000 }));
        await measurement.finalizeGmsObservations();
        const observations = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
        const stage = observations.stages[0];
        const requests = existsSync(fixture.log) ? await readFile(fixture.log, 'utf8') : '';
        assert.equal(requests.split('\n').filter(line => line.includes('shell dumpsys package com.google.android.gms')).length, 0, failure);
        assert.equal(stage.outcome, 'failed', failure);
        assert.ok(stage.error, failure);
        assert.ok(observations.errors.length > 0, failure);
        assert.equal(observations.totalCommandOutputBytes, 0, failure);
        assert.equal(observations.totalRetainedCommandOutputBytes, 0, failure);
        assert.equal(stage.stdoutBytes, undefined, failure);
        assert.equal(stage.stderrBytes, undefined, failure);
        assert.equal(stage.stdoutSha256, undefined, failure);
      } finally {
        await measurement.finish().catch(() => undefined);
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    }
  });
  test('GMS stages require premeasurement calls, a successful begin, and an active measurement', async () => {
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const writers = gmsWriterDiagnostics(fixture.root);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (options.label?.startsWith('normalize GMS components at ')) return writers.run(binary, args, timeout, options);
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      for (const stage of stages.slice(0, 2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-no-begin', { timeoutMs: 10_000 }));
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-no-begin', { timeoutMs: 10_000 }));
      await measurement.finalizeGmsObservations();
      const observation = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      assert.deepEqual(observation.stages.map((entry: any) => entry.stage), stages, JSON.stringify({
        stages: observation.stages, errors: observation.errors,
      }));
      assert.equal(observation.stages.filter((entry: any) => entry.outcome === 'collected').length, 2,
        JSON.stringify({ stages: observation.stages, normalizationFailures: writers.failures }));
      assert.ok(observation.stages.slice(2).every((entry: any) => entry.outcome === 'failed'));
      assert.ok(observation.errors.length > 0);
      const requests = await readFile(fixture.log, 'utf8');
      assert.equal(requests.split('\n').filter(line => line.includes('shell dumpsys package com.google.android.gms')).length, 2);
      assert.ok(measurement.failureOutcome().errors.length > 0);
      await assert.rejects(measurement.observeGmsStage(stages[2], new PhaseBudget('gms-after-finalize', { timeoutMs: 1_000 })), /finalized/u);
    } finally {
      await writers.close().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }

    for (const failBegin of [false, true]) {
      const current = await harness.createFixture();
      const prior = { ...process.env };
      Object.assign(process.env, current.environment);
      const io: Partial<AndroidMeasurementIO> = failBegin ? {
        boundedCommand: async (binary, args, timeout, options) => {
          if (args.includes('snapshot')) throw new Error('synthetic begin snapshot failure');
          return boundedCommand(binary, args, timeout, options);
        },
      } : {};
      const guarded = new AndroidEnvironmentMeasurement('emulator-5554', current.root, repositoryPath('tests/mobile/toolchains.json'), io);
      try {

        if (failBegin) {
          for (const stage of stages.slice(0, 2)) await guarded.observeGmsStage(stage, new PhaseBudget('gms-failed-begin', { timeoutMs: 10_000 }));
          await assert.rejects(guarded.begin(), /synthetic begin snapshot failure/u);
        } else await assert.rejects(guarded.begin(), /requires both ordered pre-measurement/u);
        const before = existsSync(current.log) ? await readFile(current.log, 'utf8') : '';
        const gmsReadsBefore = before.split('\n').filter(line => line.includes('shell dumpsys package com.google.android.gms')).length;
        await guarded.observeGmsStage(failBegin ? stages[2] : stages[0], new PhaseBudget('gms-rejected-post', { timeoutMs: 10_000 }));
        await guarded.finalizeGmsObservations();
        const after = existsSync(current.log) ? await readFile(current.log, 'utf8') : '';
        assert.equal(after.split('\n').filter(line => line.includes('shell dumpsys package com.google.android.gms')).length, gmsReadsBefore);
        const observation = JSON.parse(await readFile(join(current.root, 'android-environment-gms-observations.json'), 'utf8'));
        assert.ok(observation.errors.length > 0);
        assert.equal(observation.stages.at(-1).outcome, 'failed');
      } finally {
        for (const key of Object.keys(process.env)) if (!(key in prior)) delete process.env[key];
        Object.assign(process.env, prior);
      }
    }

    const duplicateFixture = await harness.createFixture();
    const duplicateEnv = { ...process.env };
    Object.assign(process.env, duplicateFixture.environment);
    let duplicateDispatches = 0;
    const duplicate = new AndroidEnvironmentMeasurement('emulator-5554', duplicateFixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('dumpsys')) duplicateDispatches++;
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      await duplicate.observeGmsStage(stages[0], new PhaseBudget('gms-duplicate', { timeoutMs: 10_000 }));
      await duplicate.observeGmsStage(stages[0], new PhaseBudget('gms-duplicate', { timeoutMs: 10_000 }));
      await duplicate.finalizeGmsObservations();
      assert.equal(duplicateDispatches, 1);
      const observation = JSON.parse(await readFile(join(duplicateFixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      assert.deepEqual(observation.stages.map((entry: any) => entry.outcome), ['collected', 'failed']);
      assert.ok(observation.errors.length > 0);
    } finally {
      for (const key of Object.keys(process.env)) if (!(key in duplicateEnv)) delete process.env[key];
      Object.assign(process.env, duplicateEnv);
    }

    const concurrentFixture = await harness.createFixture();
    const concurrentEnv = { ...process.env };
    Object.assign(process.env, concurrentFixture.environment);
    let entered!: () => void;
    let release!: () => void;
    const commandEntered = new Promise<void>(resolve => { entered = resolve; });
    const commandGate = new Promise<void>(resolve => { release = resolve; });
    let concurrentDispatches = 0;
    const concurrent = new AndroidEnvironmentMeasurement('emulator-5554', concurrentFixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('dumpsys')) {
          concurrentDispatches++;
          entered();
          await commandGate;
        }
        return boundedCommand(binary, args, timeout, options);
      },
    });
    let first: Promise<void> | undefined;
    try {

      first = concurrent.observeGmsStage(stages[0], new PhaseBudget('gms-concurrent', { timeoutMs: 10_000 }));
      await commandEntered;
      await concurrent.observeGmsStage(stages[1], new PhaseBudget('gms-concurrent', { timeoutMs: 10_000 }));
      release();
      await first;
      await concurrent.finalizeGmsObservations();
      assert.equal(concurrentDispatches, 1);
      const observation = JSON.parse(await readFile(join(concurrentFixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      assert.ok(observation.errors.some((error: string) => /cannot overlap/u.test(error)));
    } finally {
      release();
      await first?.catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in concurrentEnv)) delete process.env[key];
      Object.assign(process.env, concurrentEnv);
    }

    const finishedFixture = await harness.createFixture();
    const finishedEnv = { ...process.env };
    Object.assign(process.env, finishedFixture.environment);
    let finishedSpawns = 0;
    const finished = new AndroidEnvironmentMeasurement('emulator-5554', finishedFixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      spawnCollector: () => { finishedSpawns++; throw new Error('collector must not start after finish'); },
    });
    try {
      for (const stage of stages.slice(0, 2)) await finished.observeGmsStage(stage, new PhaseBudget('gms-finish-before-begin', { timeoutMs: 10_000 }));
      await finished.finish();
      await assert.rejects(finished.begin(), /cannot begin after start or finalization/u);
      assert.equal(finishedSpawns, 0);
    } finally {
      for (const key of Object.keys(process.env)) if (!(key in finishedEnv)) delete process.env[key];
      Object.assign(process.env, finishedEnv);
    }
  });
  test('GMS finalization retires the owned snapshot subprocess tree before begin settles', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const stages = ['after-device-start', 'before-measurement-begin'] as const;
    const readyPath = join(fixture.root, 'snapshot-child-ready');
    const latePath = join(fixture.root, 'snapshot-child-late');
    let entered!: () => void;
    const snapshotEntered = new Promise<void>(resolve => { entered = resolve; });
    const descendant = `process.on('SIGTERM',()=>{});setTimeout(()=>{require('node:fs').writeFileSync(${JSON.stringify(latePath)},'late');process.exit(0)},1200);setInterval(()=>{},1000)`;
    const parent = `process.on('SIGTERM',()=>{});const cp=require('node:child_process');cp.spawn(process.execPath,['-e',${JSON.stringify(descendant)}],{stdio:'inherit'});require('node:fs').writeFileSync(${JSON.stringify(readyPath)},'ready');setInterval(()=>{},1000)`;
    let snapshotFailure: BoundedCommandError | undefined;
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (!args.includes('snapshot')) return boundedCommand(binary, args, timeout, options);
        entered();
        try { return await boundedCommand('node', ['-e', parent], timeout, options); }
        catch (error) { if (error instanceof BoundedCommandError) snapshotFailure = error; throw error; }
      },
    });
    let pendingBegin: Promise<void> | undefined;
    try {
      for (const stage of stages) await measurement.observeGmsStage(stage, new PhaseBudget('gms-snapshot-retirement', { timeoutMs: 20_000 }));
      pendingBegin = measurement.begin();
      await snapshotEntered;
      const readyDeadline = Date.now() + 5_000;
      while (!existsSync(readyPath) && Date.now() < readyDeadline) await new Promise(resolve => setTimeout(resolve, 5));
      assert.ok(existsSync(readyPath));
      const finish = measurement.finish();
      await assert.rejects(pendingBegin, /BOUNDED_COMMAND_FAILED/u);
      await finish;
      assert.ok(snapshotFailure instanceof BoundedCommandError);
      assert.equal(snapshotFailure.result.childExited, true);
      assert.equal(snapshotFailure.result.ownedProcessesExited, true);
      assert.equal(snapshotFailure.result.stdioClosed, true);
      await new Promise(resolve => setTimeout(resolve, 1_300));
      assert.equal(existsSync(latePath), false);
      const observations = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      assert.ok(observations.errors.length > 0);
    } finally {
      await pendingBegin?.catch(() => undefined);
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS failed-begin finalizers and finish settle together and report whether the collector summary was written', async () => {
    for (const summary of ['written', 'blocked'] as const) {
      await withMeasurementOutput(async (output, measurement) => {
        if (summary === 'blocked') await mkdir(join(output, 'android-environment-collector.json'), { recursive: true });
        const budget = new PhaseBudget('failed-begin-publication', { timeoutMs: 15_000 });
        await measurement.observeGmsStage('after-device-start', budget);
        await measurement.observeGmsStage('before-measurement-begin', budget);
        await assert.rejects(measurement.begin(), /synthetic failed-begin snapshot/u);
        const finalizers = [settledResult(measurement.finalizeGmsObservations()), settledResult(measurement.finalizeGmsObservations())];
        const outcome = await measurement.finish();
        const settled = await Promise.all(finalizers);
        const observations = await readEvidenceJson(output, 'gms-observations');
        assert.equal(observations.finalized, true, summary);
        assert.ok(observations.errors.length > 0, summary);
        const errors = outcome.errors.map(error => String(error));
        assert.ok(errors.some(message => message.includes('synthetic failed-begin snapshot')), summary);
        if (summary === 'blocked') {
          assert.ok(settled.every(result => result.status === 'rejected' && /terminal collector summary publication failed/u.test(String(result.reason))), summary);
          assert.equal((await stat(join(output, 'android-environment-collector.json'))).isDirectory(), true);
          assert.ok(errors.length >= 2, JSON.stringify(errors));
        } else {
          assert.ok(settled.every(result => result.status === 'fulfilled'), summary);
          const collector = await readEvidenceJson(output, 'collector');
          assert.equal(collector.gmsObservations.finalized, true);
          assert.equal(collector.gmsObservations.errors, observations.errors.length);
          assert.equal(collector.gmsObservations.persistenceRetirement, 'retired');
          assert.equal(collector.gmsObservations.terminalConfirmation, 'confirmed');
          assert.equal(collector.gmsObservations.observationSha256, hashText(await readFile(join(output, 'android-environment-gms-observations.json'), 'utf8')));
          assert.ok(collector.failure);
        }
        await assertEvidenceDirectorySettled(output);
      }, { boundedCommand: (binary, args, timeout, options) => {
        if (args.includes('snapshot')) throw new Error('synthetic failed-begin snapshot');
        return boundedCommand(binary, args, timeout, options);
      } });
    }
  });
  test('GMS failed-begin finalization retires the transport observation worker without late mutations', async () => {
    const mode = '--android-transport-observation-trigger';
    const fixture = await harness.createFixture();
    const endpoint = await createAndroidTransportTestEndpoint();
    const transportFixture = await createAndroidTransportFixture(fixture.root, endpoint.port, 'present-host');
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const readyPath = join(fixture.root, 'failed-begin-transport-ready');
    const collectorReadyPath = join(fixture.root, 'failed-begin-transport-collector-ready');
    const latePath = join(fixture.root, 'failed-begin-transport-late');
    const observer = createBoundedSupervisorObserver(fixture.root, 'failed-begin-transport-supervisor');
    let failure: BoundedCommandError | undefined;
    let collectorSpawns = 0;
    const startupDetails = () => JSON.stringify({ collectorReady: existsSync(collectorReadyPath),
      writerReady: existsSync(readyPath), transportEndpointRequests: endpoint.requests,
      transportObservationFailure: (measurement as unknown as { transport: { failure?: string } }).transport.failure,
      failure: boundedCommandFailureDetails(failure?.result, observer.records()) });
    const measurement: AndroidEnvironmentMeasurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      transportFixture,
      spawnCollector: () => {
        collectorSpawns++;
        const code = `require('node:fs').writeFileSync(${JSON.stringify(collectorReadyPath)},'ready');const epoch=(Date.now()/1000).toFixed(3);process.stdout.write(epoch+' 77 77 I HerdrMeasure: ${measurement.id} START\\n'+epoch+' 77 77 I adbd: timeout expired while flushing socket, closing\\n');setTimeout(()=>process.exit(0),5000);setInterval(()=>{},1000)`;
        return spawnOwnedProcess(process.execPath, ['-e', code], { timeoutMs: 8_000, cleanupReservationMs: 1_000 });
      },
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('snapshot')) {
          const writerDeadline = Date.now() + 5_000;
          while (!existsSync(readyPath) && Date.now() < writerDeadline) await new Promise(resolve => setTimeout(resolve, 5));
          if (!existsSync(readyPath)) assert.fail(startupDetails());
          throw new Error('synthetic failed-begin snapshot');
        }
        if (!args.includes(mode)) return boundedCommand(binary, args, timeout, options);
        const target = join(fixture.root, 'android-transport-observation.json');
        const child = `process.on('SIGTERM',()=>{});const fs=require('node:fs');fs.writeFileSync(${JSON.stringify(readyPath)},'ready');setTimeout(()=>{fs.writeFileSync(${JSON.stringify(target)},'late');fs.writeFileSync(${JSON.stringify(latePath)},'late');process.exit(0)},1300);setInterval(()=>{},1000)`;
        try { return await boundedCommand(process.execPath, ['-e', child], timeout, { ...options, observerSupervisor: observer.stream }); }
        catch (error) { if (error instanceof BoundedCommandError) failure = error; throw error; }
      },
    });
    let pendingBegin: Promise<void> | undefined;
    try {
      await measurement.observeGmsStage('after-device-start', new PhaseBudget('failed-begin-transport', { timeoutMs: 15_000 }));
      await measurement.observeGmsStage('before-measurement-begin', new PhaseBudget('failed-begin-transport', { timeoutMs: 15_000 }));
      pendingBegin = measurement.begin();
      const startDeadline = Date.now() + 3_000;
      while (!existsSync(readyPath) && Date.now() < startDeadline) await new Promise(resolve => setTimeout(resolve, 5));
      if (!existsSync(readyPath)) assert.fail(startupDetails());
      const finalization = measurement.finalizeGmsObservations();
      const started = performance.now();
      await assert.rejects(pendingBegin, /synthetic failed-begin snapshot/u);
      await finalization;
      assert.ok(performance.now() - started < 2_500);
      await observer.close();
      const details = JSON.stringify({ command: boundedCommandFailureDetails(failure?.result, observer.records()) });
      assert.ok(failure instanceof BoundedCommandError, details);
      assert.equal(failure.result.childExited, true, details);
      assert.equal(failure.result.ownedProcessesExited, true, details);
      assert.equal(failure.result.stdioClosed, true, details);
      assert.equal(collectorSpawns, 1);
      await new Promise(resolve => setTimeout(resolve, 1_400));
      assert.equal(existsSync(latePath), false);
    } finally {
      await observer.close();
      await pendingBegin?.catch(() => undefined);
      await measurement.finish().catch(() => undefined);
      await endpoint.stop();
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('diagnostic GMS samples use exact bounded stages without changing either measurement snapshot', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const parserObserverPath = join(fixture.root, 'gms-parser-supervisor.jsonl');
    const parserObserverFD = openSync(parserObserverPath, 'wx', 0o600);
    const parserObserver = createWriteStream('', { fd: parserObserverFD, autoClose: false });
    const parserRuns: Array<Record<string, unknown>> = [];
    let parserObserverClosed = false;
    const closeParserObserver = async () => {
      if (parserObserverClosed) return;
      parserObserverClosed = true;
      await new Promise<void>(resolve => parserObserver.end(resolve));
      closeSync(parserObserverFD);
    };
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        const evalIndex = args.indexOf('--eval');
        const parserSource = evalIndex < 0 ? '' : String(args[evalIndex + 1] || '');
        if (!parserSource.includes('PARSER_STARTED')) return boundedCommand(binary, args, timeout, options);
        const input = Buffer.from(options.input || '');
        const startedAtNs = monotonicNowNs();
        const record = (result: BoundedCommandError['result']) => parserRuns.push({
          startedAtNs: startedAtNs.toString(), endedAtNs: monotonicNowNs().toString(), timeoutMs: timeout,
          cleanupReservationMs: options.cleanupReservationMs, inputBytes: input.byteLength,
          inputSha256: hashText(input.toString('utf8')), targetPID: result.targetPID,
          targetCloseObserved: result.targetCloseObserved, childExited: result.childExited,
          ownedProcessesExited: result.ownedProcessesExited, stdioClosed: result.stdioClosed,
          retirement: boundedCommandFailureDetails(result, observerRecords(parserObserverPath)),
          timedOut: result.timedOut, durationMs: result.durationMs, exitCode: result.code, stdoutBytes: result.stdoutBytes,
          stdoutSha256: result.stdoutSha256,
        });
        try {
          const result = await boundedCommand(binary, args, timeout, { ...options, observerSupervisor: parserObserver });
          record(result);
          return result;
        } catch (error) {
          if (error instanceof BoundedCommandError) record(error.result);
          throw error;
        }
      },
    });
    const gmsDumpPath = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const gmsDump = await readFile(gmsDumpPath, 'utf8');
    await writeFile(gmsDumpPath, gmsDump.replace('Queries:', '      enabledComponents:\n        com.google.android.gms.fixture.EnabledService\n      disabledComponents:\n        com.google.firebase.auth.api.gms.fixture.DisabledReceiver\nQueries:'));
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    const budget = () => new PhaseBudget('gms-observation-test', { timeoutMs: 30_000 });
    try {
      await measurement.observeGmsStage(stages[0], budget());
      await measurement.observeGmsStage(stages[1], budget());
      await measurement.begin();
      const beforePath = join(fixture.root, 'android-environment-before.json');
      const beforeHash = await fileSha256(beforePath);
      const before = JSON.parse(await readFile(beforePath, 'utf8')) as AndroidEnvironmentSnapshot;
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, budget());
      const observationsPath = join(fixture.root, 'android-environment-gms-observations.json');
      const observations = JSON.parse(await readFile(observationsPath, 'utf8'));
      assert.deepEqual(observations.stages.map((entry: any) => entry.stage), stages);
      assert.ok(observations.stages.every((entry: any) => entry.outcome === 'collected' && Date.parse(entry.startedAt) <= Date.parse(entry.endedAt)),
        JSON.stringify(observations.stages.map((entry: any) => ({ stage: entry.stage, outcome: entry.outcome, startedAt: entry.startedAt,
          endedAt: entry.endedAt, durationMs: entry.durationMs, error: entry.error, timeoutMs: entry.timeoutMs,
          ownershipDurationMs: entry.ownershipDurationMs, ownershipChildExited: entry.ownershipChildExited,
          ownershipProcessesExited: entry.ownershipProcessesExited, ownershipStdioClosed: entry.ownershipStdioClosed,
          parseDurationMs: entry.parseDurationMs, parseTimeoutMs: entry.parseTimeoutMs,
          parseCleanupReservationMs: entry.parseCleanupReservationMs, parserStarted: entry.parserStarted,
          parserReady: entry.parserReady, parserInputBytes: entry.parserInputBytes,
          parserInputSha256: entry.parserInputSha256, parserOutputProduced: entry.parserOutputProduced,
          parserStartupMs: entry.parserStartupMs, parseTargetStarted: entry.parseTargetStarted,
          parseTargetClosed: entry.parseTargetClosed, normalizationTimedOut: entry.normalizationTimedOut,
          parseChildExited: entry.parseChildExited, parseOwnedProcessesExited: entry.parseOwnedProcessesExited,
          parseStdioClosed: entry.parseStdioClosed, childExited: entry.childExited,
          ownedProcessesExited: entry.ownedProcessesExited, stdioClosed: entry.stdioClosed,
          exitCode: entry.exitCode, timedOut: entry.timedOut, signal: entry.signal }))));
      const state = observations.stages[0].state;
      assert.deepEqual(state.enabledComponents, ['com.google.android.gms.fixture.EnabledService']);
      assert.deepEqual(state.disabledComponents, ['com.google.firebase.auth.api.gms.fixture.DisabledReceiver']);
      assert.equal(state.packageName, 'com.google.android.gms');
      assert.equal(state.hashEncoding, 'sha256-hex-dot-separated-v1');
      assert.equal(decodeRetainedHash(state.componentStateSha256), hashText(JSON.stringify({
        enabledComponents: state.enabledComponents,
        disabledComponents: state.disabledComponents,
      })));
      assert.equal(decodeRetainedHash(state.componentStateSha256), decodeRetainedHash(state.sourceHashes.componentStateSha256));
      assert.equal(decodeRetainedHash(state.packageIdentitySha256), decodeRetainedHash(state.sourceHashes.packageIdentitySha256));
      assert.equal(decodeRetainedHash(state.dumpSha256), decodeRetainedHash(state.sourceHashes.dumpSha256));
      assert.equal(await fileSha256(beforePath), beforeHash);
      const outcome = await measurement.finish();
      assert.deepEqual(outcome.errors, []);
      await closeParserObserver();
      const parserEvents = observerRecords(parserObserverPath);
      assert.equal(parserRuns.length, stages.length);
      assert.ok(parserRuns.every(run => run.timeoutMs === 1_000 && run.cleanupReservationMs === 40
        && Number(run.targetPID) > 0 && run.targetCloseObserved === true && run.childExited === true
        && run.ownedProcessesExited === true && run.stdioClosed === true && run.timedOut === false
        && Number(run.inputBytes) > 0 && /^[a-f0-9]{64}$/u.test(String(run.inputSha256))
        && BigInt(String(run.startedAtNs)) < BigInt(String(run.endedAtNs))), JSON.stringify(parserRuns));
      for (const stage of observations.stages) {
        assert.equal(stage.parseTimeoutMs, 1_000);
        assert.equal(stage.parseCleanupReservationMs, 40);
        assert.equal(stage.parserStarted, true);
        assert.equal(stage.parserReady, true);
        assert.equal(stage.parserInputBytes, stage.stdoutBytes);
        assert.equal(stage.parserInputSha256, decodeRetainedHash(stage.stdoutSha256));
        assert.equal(stage.parserOutputProduced, true);
        assert.equal(stage.parseTargetStarted, true);
        assert.equal(stage.parseTargetClosed, true);
        assert.ok(Number.isSafeInteger(stage.parserStartupMs) && stage.parserStartupMs >= 0);
      }
      for (const type of ['supervisor-runtime-started', 'supervisor-deadlines-armed', 'supervisor-target-exit',
        'supervisor-target-close', 'supervisor-retirement-ready']) {
        assert.equal(parserEvents.filter((event: any) => event.type === type).length, stages.length, type);
      }
      const after = JSON.parse(await readFile(join(fixture.root, 'android-environment-after.json'), 'utf8')) as AndroidEnvironmentSnapshot;
      assert.deepEqual(after.packages['com.google.android.gms'], before.packages['com.google.android.gms']);
      assert.equal(await fileSha256(beforePath), beforeHash);
      const persistedObservations = JSON.parse(await readFile(observationsPath, 'utf8'));
      const persistedState = persistedObservations.stages[0].state;
      assert.equal(decodeRetainedHash(persistedState.componentStateSha256), hashText(JSON.stringify({
        enabledComponents: persistedState.enabledComponents,
        disabledComponents: persistedState.disabledComponents,
      })));
      assert.equal(decodeRetainedHash(persistedState.sourceHashes.componentStateSha256), decodeRetainedHash(persistedState.componentStateSha256));
      const commands = (await readFile(fixture.log, 'utf8')).split('\n').filter((line) => line.includes('shell dumpsys package com.google.android.gms'));
      assert.equal(commands.length, stages.length + 2, 'only the existing before/after snapshots add their two GMS package reads');
      const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
      assert.deepEqual(collector.gmsObservations.stages.map((entry: any) => entry.stage), stages);
      assert.equal(collector.failure, undefined);

      const delayedFixture = await harness.createFixture();
      const delayedSaved = { ...process.env };
      Object.assign(process.env, delayedFixture.environment);
      const delayedObserverPath = join(delayedFixture.root, 'gms-parser-supervisor.jsonl');
      const delayedObserverFD = openSync(delayedObserverPath, 'wx', 0o600);
      const delayedObserver = createWriteStream('', { fd: delayedObserverFD, autoClose: false });
      let delayedObserverClosed = false;
      const closeDelayedObserver = async () => {
        if (delayedObserverClosed) return;
        delayedObserverClosed = true;
        await new Promise<void>(resolve => delayedObserver.end(resolve));
        closeSync(delayedObserverFD);
      };
      let delayedParserFailure: BoundedCommandError | undefined;
      let delayedParserReceipt: Record<string, unknown> | undefined;
      const delayedMeasurement = new AndroidEnvironmentMeasurement('emulator-5554', delayedFixture.root,
        repositoryPath('tests/mobile/toolchains.json'), {
          boundedCommand: async (binary, args, timeout, options) => {
            const evalIndex = args.indexOf('--eval');
            const parserSource = evalIndex < 0 ? '' : String(args[evalIndex + 1] || '');
            if (!parserSource.includes('PARSER_STARTED')) return boundedCommand(binary, args, timeout, options);
            const delayedSource = parserSource.replace('const parser = await import(',
              'await new Promise(resolve => setTimeout(resolve, 1_500));\nconst parser = await import(');
            assert.notEqual(delayedSource, parserSource);
            const input = Buffer.from(options.input || '');
            const startedAtNs = monotonicNowNs();
            try {
              return await boundedCommand(binary, args.map((value, index) => index === evalIndex + 1 ? delayedSource : value), timeout,
                { ...options, observerSupervisor: delayedObserver });
            } catch (error) {
              if (error instanceof BoundedCommandError) {
                delayedParserFailure = error;
                delayedParserReceipt = {
                  startedAtNs: startedAtNs.toString(), endedAtNs: monotonicNowNs().toString(), timeoutMs: timeout,
                  cleanupReservationMs: options.cleanupReservationMs, inputBytes: input.byteLength,
                  inputSha256: hashText(input.toString('utf8')), targetPID: error.result.targetPID,
                  targetCloseObserved: error.result.targetCloseObserved, childExited: error.result.childExited,
                  ownedProcessesExited: error.result.ownedProcessesExited, stdioClosed: error.result.stdioClosed,
                  timedOut: error.result.timedOut, durationMs: error.result.durationMs,
                };
              }
              throw error;
            }
          },
        });
      try {
        await delayedMeasurement.observeGmsStage('after-device-start',
          new PhaseBudget('gms-parser-delayed-control', { timeoutMs: 15_000 }));
        await delayedMeasurement.finalizeGmsObservations();
        await closeDelayedObserver();
        const delayedEvents = observerRecords(delayedObserverPath);
        const delayedObservations = JSON.parse(await readFile(join(delayedFixture.root, 'android-environment-gms-observations.json'), 'utf8'));
        const delayedStage = delayedObservations.stages[0];
        const delayedDetails = JSON.stringify({ stage: {
          parserStarted: delayedStage.parserStarted, parserReady: delayedStage.parserReady,
          parseTargetStarted: delayedStage.parseTargetStarted, parseTargetClosed: delayedStage.parseTargetClosed,
          parseChildExited: delayedStage.parseChildExited, parseOwnedProcessesExited: delayedStage.parseOwnedProcessesExited,
          parseStdioClosed: delayedStage.parseStdioClosed,
        }, receipt: delayedParserReceipt, command: boundedCommandFailureDetails(delayedParserFailure?.result, delayedEvents) });
        assert.equal(delayedStage.outcome, 'failed');
        assert.equal(delayedStage.normalizationTimedOut, true);
        assert.equal(delayedStage.parseTimeoutMs, 1_000);
        assert.equal(delayedStage.parseCleanupReservationMs, 40);
        assert.equal(delayedStage.parserStarted, true);
        assert.equal(delayedStage.parserReady, false);
        assert.equal(delayedStage.parserInputBytes, undefined);
        assert.equal(delayedStage.parserOutputProduced, false);
        assert.equal(delayedStage.parseTargetStarted, true);
        assert.equal(delayedStage.parseTargetClosed, true);
        assert.equal(delayedStage.parseChildExited, true);
        assert.equal(delayedStage.parseOwnedProcessesExited, true);
        assert.equal(delayedStage.parseStdioClosed, true);
        assert.ok(delayedStage.parseDurationMs <= 1_000);
        assert.equal(delayedStage.state, undefined);
        assert.ok(delayedParserFailure instanceof BoundedCommandError);
        assert.equal(delayedParserFailure.result.timedOut, true);
        assert.match(delayedParserFailure.result.stdout, /^PARSER_STARTED \d+\n/u);
        assert.doesNotMatch(delayedParserFailure.result.stdout, /PARSER_READY|PARSER_INPUT|PARSER_OUTPUT/u);
        assert.ok(delayedParserReceipt);
        assert.equal(delayedParserReceipt.timeoutMs, 1_000);
        assert.equal(delayedParserReceipt.cleanupReservationMs, 40);
        assert.equal(delayedParserReceipt.inputBytes, delayedStage.stdoutBytes);
        assert.equal(delayedParserReceipt.inputSha256, decodeRetainedHash(delayedStage.stdoutSha256));
        assert.ok(Number(delayedParserReceipt.targetPID) > 0);
        assert.equal(delayedParserReceipt.targetCloseObserved, true);
        assert.equal(delayedParserReceipt.childExited, true);
        assert.equal(delayedParserReceipt.ownedProcessesExited, true, delayedDetails);
        assert.equal(delayedParserReceipt.stdioClosed, true);
        assert.equal(delayedParserReceipt.timedOut, true);
        assert.ok(BigInt(String(delayedParserReceipt.startedAtNs)) < BigInt(String(delayedParserReceipt.endedAtNs)));
        const deadlines = delayedEvents.find((event: any) => event.type === 'supervisor-deadlines-armed');
        const executionDeadline = delayedEvents.find((event: any) => event.type === 'supervisor-execution-deadline');
        assert.ok(deadlines && executionDeadline);
        assert.equal(BigInt(String(deadlines.hardDeadlineNs)) - BigInt(String(deadlines.executionDeadlineNs)), 40_000_000n);
        const deadlineObservedAt = BigInt(String(executionDeadline.clockNs));
        const executionDeadlineNs = BigInt(String(deadlines.executionDeadlineNs));
        assert.ok(deadlineObservedAt >= executionDeadlineNs - 5_000_000n
          && deadlineObservedAt <= BigInt(String(deadlines.hardDeadlineNs)));
        for (const type of ['supervisor-runtime-started', 'supervisor-target-exit', 'supervisor-target-close',
          'supervisor-input-close', 'supervisor-group-absent', 'supervisor-retirement-ready']) {
          const count = delayedEvents.filter((event: any) => event.type === type).length;
          assert.ok(type === 'supervisor-group-absent' ? count >= 1 : count === 1, type);
        }
        await writeFile(join(fixture.root, 'gms-parser-process-controls.json'), JSON.stringify({
          positive: parserRuns,
          positiveStages: observations.stages.map((stage: any) => ({ stage: stage.stage, timeoutMs: stage.parseTimeoutMs,
            cleanupReservationMs: stage.parseCleanupReservationMs, parserStarted: stage.parserStarted, parserReady: stage.parserReady,
            inputBytes: stage.parserInputBytes, inputSha256: stage.parserInputSha256, outputProduced: stage.parserOutputProduced,
            startupMs: stage.parserStartupMs, parseDurationMs: stage.parseDurationMs, targetStarted: stage.parseTargetStarted,
            targetClosed: stage.parseTargetClosed, childExited: stage.parseChildExited,
            ownedProcessesExited: stage.parseOwnedProcessesExited, stdioClosed: stage.parseStdioClosed })),
          delayed: delayedParserReceipt,
          delayedStage: { timeoutMs: delayedStage.parseTimeoutMs, cleanupReservationMs: delayedStage.parseCleanupReservationMs,
            parserStarted: delayedStage.parserStarted, parserReady: delayedStage.parserReady, inputBytes: delayedStage.parserInputBytes,
            outputProduced: delayedStage.parserOutputProduced, parseDurationMs: delayedStage.parseDurationMs,
            targetStarted: delayedStage.parseTargetStarted, targetClosed: delayedStage.parseTargetClosed,
            childExited: delayedStage.parseChildExited, ownedProcessesExited: delayedStage.parseOwnedProcessesExited,
            stdioClosed: delayedStage.parseStdioClosed },
          deadlineEvents: delayedEvents.filter((event: any) => ['supervisor-deadlines-armed',
            'supervisor-execution-deadline', 'supervisor-retirement-ready'].includes(event.type)),
        }, null, 2));
      } finally {
        await delayedMeasurement.finish().catch(() => undefined);
        await closeDelayedObserver();
        for (const key of Object.keys(process.env)) if (!(key in delayedSaved)) delete process.env[key];
        Object.assign(process.env, delayedSaved);
      }
    } catch (error) {
      const readJsonIfPresent = async (name: string) => {
        const path = join(fixture.root, name);
        return existsSync(path) ? JSON.parse(await readFile(path, 'utf8')) : undefined;
      };
      await writeFile(join(fixture.root, 'diagnostic-gms-stage-failure.json'), JSON.stringify({
        error: error instanceof Error ? { name: error.name, message: error.message, stack: error.stack } : String(error),
        measurementErrors: measurement.failureOutcome().errors.map(value => value instanceof Error ? value.message : String(value)),
        observations: await readJsonIfPresent('android-environment-gms-observations.json'),
        collector: await readJsonIfPresent('android-environment-collector.json'),
        completion: await readJsonIfPresent('android-environment-completion.json'),
        check: await readJsonIfPresent('android-environment-check.json'),
      }, null, 2)).catch(() => undefined);
      throw error;
    } finally {
      await measurement.finish().catch(() => undefined);
      await closeParserObserver();
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS terminal observation write blocked by a read-only directory is reported, leaves the staged file untouched and is never confirmed', async () => {
    if (process.getuid?.() === 0) return;
    await withMeasurementOutput(async (output, measurement) => {
      await beginObservedMeasurement(measurement);
      const observationsPath = join(output, 'android-environment-gms-observations.json');
      const staged = await readFile(observationsPath);
      assert.equal(JSON.parse(staged.toString('utf8')).finalized, undefined);
      await chmod(output, 0o500);
      const terminal = settledResult(measurement.finalizeGmsObservations());
      const deadline = Date.now() + 10_000;
      while (!measurement.failureOutcome().errors.some(error => /GMS/u.test(String(error))) && Date.now() < deadline) {
        await new Promise(resolve => setTimeout(resolve, 10));
      }
      await chmod(output, 0o700);
      const outcome = await measurement.finish();
      const settled = await terminal;
      assert.ok(settled.status === 'rejected' && /terminal collector confirmation failed/u.test(String(settled.reason)));
      assert.deepEqual(await readFile(observationsPath), staged);
      const collector = await readEvidenceJson(output, 'collector');
      assert.equal(collector.gmsObservations.terminalConfirmation, 'unconfirmed');
      assert.equal(collector.gmsObservations.observationSha256, undefined);
      assert.equal(collector.gmsObservations.persistenceRetirement, 'retired');
      assert.ok(collector.gmsObservations.errors > 0);
      assert.equal(outcome.assessment?.collection.status, 'UNKNOWN');
      assert.ok(outcome.errors.length > 0);
      await assertEvidenceDirectorySettled(output);
    });
  });
  test('GMS collector summary write blocked by a directory invalidates public success for the assessment consumer', async () => {
    await withMeasurementOutput(async (output, measurement) => {
      await mkdir(join(output, 'android-environment-collector.json'), { recursive: true });
      await beginObservedMeasurement(measurement);
      const identity = measurementIdentity({ measurementId: measurement.id });
      const outcome = await measurement.finish();
      assert.equal((await stat(join(output, 'android-environment-collector.json'))).isDirectory(), true, outcome.errors.join(' | '));
      assert.equal(outcome.assessment?.collection.status, 'UNKNOWN');
      assert.ok(outcome.errors.length > 0);
      const bundlePath = join(output, 'bundle-set.json');
      await writeFile(bundlePath, JSON.stringify({ candidate: {
        provenance: { sourceCommit: identity.sourceCommit },
        identity: { webHash: identity.candidateWebHash, build: identity.candidateBuild },
      } }));
      const args = ['check', '--contract', 'report', '--directory', output,
        '--identity', join(output, 'android-environment-identity.json'), '--bundle-set', bundlePath,
        '--run-id', identity.runId, '--attempt', identity.attempt, '--baseline', identity.baseline,
        '--source-run-head-sha', identity.sourceRunHeadSha];
      assert.throws(() => execFileSync(process.execPath, [repositoryPath('tests/mobile/android-environment.ts'), ...args], {
        encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
      }));
      assert.equal((await readEvidenceJson(output, 'check')).collection.status, 'UNKNOWN');
    });
  });
  test('GMS finalizers and finish settle together on terminal collector confirmation, in either order, whether or not the summary can be written', async () => {
    for (const order of ['finalizers first', 'finish first'] as const) {
      for (const summary of ['written', 'blocked'] as const) {
        const label = `${order}, summary ${summary}`;
        await withMeasurementOutput(async (output, measurement) => {
          if (summary === 'blocked') await mkdir(join(output, 'android-environment-collector.json'), { recursive: true });
          await beginObservedMeasurement(measurement);
          const startFinalizers = () => [settledResult(measurement.finalizeGmsObservations()), settledResult(measurement.finalizeGmsObservations())];
          let finalizers: Array<Promise<PromiseSettledResult<void>>>;
          let finish: Promise<Awaited<ReturnType<Measurement['finish']>>>;
          if (order === 'finalizers first') { finalizers = startFinalizers(); finish = measurement.finish(); }
          else { finish = measurement.finish(); finalizers = startFinalizers(); }
          const outcome = await finish;
          const settled = await Promise.all(finalizers);
          const observationsPath = join(output, 'android-environment-gms-observations.json');
          const bytes = await readFile(observationsPath);
          assert.equal(JSON.parse(bytes.toString('utf8')).finalized, true, label);
          if (summary === 'blocked') {
            assert.ok(outcome.errors.length > 0, label);
            assert.ok(settled.every(result => result.status === 'rejected' && /terminal collector confirmation failed/u.test(String(result.reason))), label);
            assert.equal(outcome.assessment?.collection.status, 'UNKNOWN', label);
            assert.equal((await stat(join(output, 'android-environment-collector.json'))).isDirectory(), true, label);
          } else {
            assert.deepEqual(outcome.errors, [], label);
            assert.ok(settled.every(result => result.status === 'fulfilled'), label);
            assert.equal(outcome.assessment?.collection.status, 'PASS', label);
            const collector = await readEvidenceJson(output, 'collector');
            assert.equal(collector.gmsObservations.terminalConfirmation, 'confirmed', label);
            assert.equal(collector.gmsObservations.observationSha256, hashText(bytes.toString('utf8')), label);
            assert.equal(collector.gmsObservations.errors, 0, label);
          }
          const errors = measurement.failureOutcome().errors.map(error => String(error));
          await new Promise(resolve => setTimeout(resolve, 50));
          assert.deepEqual(await readFile(observationsPath), bytes, label);
          assert.deepEqual(measurement.failureOutcome().errors.map(error => String(error)), errors, label);
          await assertEvidenceDirectorySettled(output);
        });
      }
    }
  });
  test('GMS finalizer-first barriers fail promptly when an earlier stage persistence prevents the terminal writer', async () => {
    const entryDetails = () => ({ case: 'barrier' });
    const entered = groupPollTestTimers();
    let enter!: () => void;
    const entry = new Promise<void>(resolve => { enter = resolve; });
    const enteredBarrier = awaitGatedEntry(entry, new Promise<void>(() => undefined), 'the writer', 'finalization', entryDetails, entered);
    assert.equal(entered.timers.length, 1);
    assert.equal(entered.timers[0]!.delayMs, 20_000);
    enter();
    await enteredBarrier;
    assert.equal(entered.timers[0]!.cancelled, true);
    assert.deepEqual(entered.pending(), []);

    const settledEarly = groupPollTestTimers();
    let lateEnter!: () => void;
    const lateEntry = new Promise<void>(resolve => { lateEnter = resolve; });
    const earlyBarrier = awaitGatedEntry(lateEntry, Promise.resolve(), 'the writer', 'finalization', entryDetails, settledEarly);
    await assert.rejects(earlyBarrier, (error: unknown) => error instanceof assert.AssertionError
      && /^finalization settled before the writer was entered: \{"case":"barrier"\}$/u.test(error.message));
    lateEnter();
    assert.deepEqual(settledEarly.pending(), []);

    const failedEarly = groupPollTestTimers();
    await assert.rejects(awaitGatedEntry(new Promise<void>(() => undefined), Promise.reject(new Error('synthetic finalization failure')),
      'the writer', 'finalization', entryDetails, failedEarly), (error: unknown) => error instanceof assert.AssertionError
      && /^finalization failed before the writer was entered \(synthetic finalization failure\): /u.test(error.message));
    assert.deepEqual(failedEarly.pending(), []);

    const unentered = groupPollTestTimers();
    const watchdog: { outcome: unknown } = { outcome: 'pending' };
    const watchdogBarrier = awaitGatedEntry(new Promise<void>(() => undefined), new Promise<void>(() => undefined),
      'the writer', 'finalization', entryDetails, unentered).then(() => { watchdog.outcome = 'entered'; }, error => { watchdog.outcome = error; });
    await new Promise(resolve => setImmediate(resolve));
    const beforeDelivery = watchdog.outcome;
    assert.equal(beforeDelivery, 'pending');
    unentered.deliver(unentered.timers[0]!);
    await watchdogBarrier;
    const watchdogFailure = watchdog.outcome;
    assert.ok(watchdogFailure instanceof assert.AssertionError, String(watchdogFailure));
    assert.match(watchdogFailure.message, /^the writer was not entered within 20000 ms: /u);

    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    const observationsPath = join(fixture.root, 'android-environment-gms-observations.json');
    await mkdir(observationsPath);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
    let finalizationSettlement: Promise<PromiseSettledResult<void>> | undefined;
    try {
      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-finalizer-barrier', { timeoutMs: 20_000 }));
      const precondition = await gmsStagePersistencePrecondition(fixture.root, stages, measurement);
      assert.match(precondition.violations[0] ?? '', /^GMS observations are missing or unreadable: /u, JSON.stringify(precondition));
      assert.ok(precondition.measurementErrors.some(error => error.includes('EISDIR')), JSON.stringify(precondition));
      assert.ok(precondition.violations.some(violation => violation.startsWith('measurement error: ')
        && violation.includes('EISDIR')), JSON.stringify(precondition));
      const timers = groupPollTestTimers();
      let writerEntered!: () => void;
      const writerEntry = new Promise<void>(resolve => { writerEntered = resolve; });
      finalizationSettlement = settledResult(measurement.finalizeGmsObservations());
      let barrierOutcome: unknown = 'pending';
      const barrier = awaitGatedEntry(writerEntry, internalGmsFinalization(measurement), 'the terminal GMS writer', 'GMS finalization',
        () => precondition, timers).then(() => { barrierOutcome = 'entered'; }, error => { barrierOutcome = error; });
      await internalGmsFinalization(measurement);
      await new Promise(resolve => setImmediate(resolve));
      assert.equal((await stat(observationsPath)).isDirectory(), true);
      assert.ok(barrierOutcome instanceof assert.AssertionError, `a bare writer-entry wait stays ${String(barrierOutcome)} here`);
      assert.match(barrierOutcome.message, /^GMS finalization settled before the terminal GMS writer was entered: .*EISDIR/u);
      assert.equal(timers.timers.length, 1);
      assert.deepEqual(timers.pending(), []);
      writerEntered();
      await barrier;
      assert.equal((await finalizationSettlement).status, 'fulfilled');
    } finally {
      await finalizationSettlement;
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('final GMS publication reports an explicitly scoped prior-persistence total and bound', async () => {
    await withMeasurementOutput(async (output, measurement) => {
      await beginObservedMeasurement(measurement);
      await measurement.finish();
      const observations = await readEvidenceJson(output, 'gms-observations');
      const collector = await readEvidenceJson(output, 'collector');
      const persistedPrior = observations.priorPersistenceAttemptMs;
      const completedAll = collector.gmsObservations.persistenceAttemptMsThroughFinalization;
      assert.ok(persistedPrior > 0);
      assert.ok(completedAll > persistedPrior);
      assert.equal(observations.persistenceAccounting,
        'priorPersistenceAttemptMs includes prior bounded publication attempts; the current attempt is excluded and bounded by persistenceOperationTimeoutMs');
      assert.equal(observations.limits.persistenceOperationTimeoutMs, 5_000);
      assert.equal(observations.temporaryEvidenceCleanup, 'not-needed');
      assert.match(collector.gmsObservations.persistenceAccounting, /includes all bounded observation-publication attempts through finalization/u);
      assert.equal(collector.gmsObservations.temporaryEvidenceCleanup, 'not-needed');
      assert.equal(collector.gmsObservations.persistenceRetirement, 'retired');
      assert.ok(completedAll <= observations.limits.totalPersistenceLimitMs);
    });
  });
  test('failed GMS commands account for both streams against the aggregate byte ceiling', async () => {
    assert.equal(execFileSync('node', ['--version'], { encoding: 'utf8' }).trim(), 'v26.13.0');
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const code = `const fs=require('node:fs');const chunk=Buffer.alloc(1100000,65);const write=fd=>{let offset=0;while(offset<chunk.length)offset+=fs.writeSync(fd,chunk,offset,chunk.length-offset)};write(1);write(2);setTimeout(()=>{process.exitCode=7},250)`;
    let dispatches = 0;
    const runChild: typeof boundedCommand = async (binary, args, timeout, options) => {
      if (!args.includes('dumpsys')) return boundedCommand(binary, args, timeout, options);
      dispatches++;
      return boundedCommand('node', ['-e', code], timeout, options);
    };
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), { boundedCommand: runChild });
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    try {
      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-byte-accounting', { timeoutMs: 60_000 }));
      await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-byte-accounting', { timeoutMs: 60_000 }));
      await measurement.begin();
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-byte-accounting', { timeoutMs: 60_000 }));
      assert.equal(dispatches, stages.length);
      const outcome = await measurement.finish();
      assert.ok(outcome.errors.length > 0);
      const observations = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      const acquired = observations.stages.reduce((total: number, entry: any) => total + (entry.stdoutBytes || 0) + (entry.stderrBytes || 0), 0);
      const retained = observations.stages.reduce((total: number, entry: any) => total + (entry.stdoutRetainedBytes || 0) + (entry.stderrRetainedBytes || 0), 0);
      assert.equal(observations.totalCommandOutputBytes, acquired);
      assert.equal(observations.totalRetainedCommandOutputBytes, retained);
      assert.ok(acquired > observations.limits.totalCommandOutputBytes);
      assert.ok(acquired <= observations.limits.totalCommandOutputBytes + 2_200_000);
      assert.ok(retained <= observations.limits.totalCommandOutputBytes);
      assert.ok(observations.stages.every((entry: any) => entry.outcome === 'failed'));
      assert.equal(observations.stages.at(-1).outputLimitExceeded, true);
      assert.equal(observations.stages.at(-1).childExited, true);
    } finally {
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('event-loop stall window crosses the execution deadline and leaves a cleanup-derived drain allowance', async () => {
    const command = { commandStartedMs: 1_000, timeoutMs: 700, cleanupMs: 200 };
    for (const markerAfterMs of [89, 139, 164, 499]) {
      const admitted = eventLoopStallWindow({ ...command, markerMs: 1_000 + markerAfterMs });
      assert.deepEqual(admitted, { executionDeadlineMs: 1_500, hardDeadlineMs: 1_700, stallUntilMs: 1_550, drainAllowanceMs: 150 }, String(markerAfterMs));
      assert.ok(admitted.stallUntilMs! > admitted.executionDeadlineMs! && admitted.drainAllowanceMs! >= command.cleanupMs / 2);
    }
    const recordedMarkerAfterMs = 164;
    assert.ok(recordedMarkerAfterMs + 550 > command.timeoutMs, 'the former fixed 550 ms stall after the recorded 164 ms marker ends after the hard deadline');
    assert.deepEqual(eventLoopStallWindow({ ...command, markerMs: 1_164, stallEndedMs: 1_550 }),
      { executionDeadlineMs: 1_500, hardDeadlineMs: 1_700, stallUntilMs: 1_550, drainAllowanceMs: 150 });
    assert.equal(eventLoopStallWindow({ ...command, markerMs: 1_164, stallEndedMs: 1_600 }).violation, undefined);
    assert.equal(eventLoopStallWindow({ ...command, markerMs: 1_164, stallEndedMs: 1_600.5 }).violation,
      'stall ended 600.5 ms after the command started, leaving less than half of the 200 ms cleanup reservation to drain before 700 ms');
    assert.equal(eventLoopStallWindow({ ...command, markerMs: 1_164, stallEndedMs: 1_549 }).violation,
      'stall ended 549 ms after the command started, before crossing to 550 ms');
    assert.equal(eventLoopStallWindow({ ...command, markerMs: 1_500 }).violation,
      'child marker arrived 500 ms after the command started, at or after its 500 ms execution deadline; the stall cannot cross it');
    assert.match(eventLoopStallWindow({ ...command, markerMs: 1_650, stallEndedMs: 1_660 }).violation ?? '', /at or after its 500 ms execution deadline/u);
    assert.equal(eventLoopStallWindow({ ...command, markerMs: 999 }).violation, 'child marker precedes the command start');
    for (const cleanupMs of [0, -1, 700, 701]) {
      assert.match(eventLoopStallWindow({ ...command, cleanupMs, markerMs: 1_100 }).violation ?? '', /must be positive and shorter than/u, String(cleanupMs));
    }
    for (const invalid of [{ markerMs: Number.NaN }, { markerMs: undefined }, { markerMs: '1100' }, { markerMs: 1_100, stallEndedMs: Number.POSITIVE_INFINITY },
      { markerMs: 1_100, stallEndedMs: undefined }, { markerMs: 1_100, timeoutMs: '700' }]) {
      const result = eventLoopStallWindow({ ...command, ...invalid });
      assert.equal(result.violation, 'stall window inputs must be finite numbers', JSON.stringify(invalid));
      assert.equal(result.stallUntilMs, undefined);
    }
  });
  test('timed-out GMS child bytes remain in the aggregate allowance after an event-loop stall', async () => {
    assert.equal(execFileSync('node', ['--version'], { encoding: 'utf8' }).trim(), 'v26.13.0');
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const sourcePath = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const source = await readFile(sourcePath, 'utf8');
    const padded = source.replace('Compiler stats:', `${' '.repeat(1_200_000)}\nCompiler stats:`);
    await writeFile(sourcePath, padded);
    const markerPath = join(fixture.root, 'stall-child-ready');
    const latePath = join(fixture.root, 'stall-child-late');
    const childCode = `(async()=>{process.on('SIGTERM',()=>{});const fs=require('node:fs');const chunk=Buffer.alloc(1800000,65);await Promise.all([new Promise(resolve=>process.stdout.write(chunk,resolve)),new Promise(resolve=>process.stderr.write(chunk,resolve))]);fs.writeFileSync(${JSON.stringify(markerPath)},'ready');setTimeout(()=>{fs.writeFileSync(${JSON.stringify(latePath)},'late');process.exit(0)},1000);setInterval(()=>{},1000)})()`;
    const maxBytes: number[] = [];
    const observerPath = join(fixture.root, 'stall-owned-process-observer.jsonl');
    const observerFD = openSync(observerPath, 'w');
    const observerSupervisor = createWriteStream('', { fd: observerFD, autoClose: false });
    let dispatches = 0;
    let stallWindow: Record<string, unknown> | undefined;
    const runChild: typeof boundedCommand = async (binary, args, timeout, options) => {
      if (!args.includes('dumpsys')) return boundedCommand(binary, args, timeout, options);
      dispatches++;
      maxBytes.push(options.maxBytes);
      if (dispatches !== 1) return boundedCommand(binary, args, timeout, options);
      const commandStartedMs = performance.now();
      const command = boundedCommand('node', ['-e', childCode], 700, {
        ...options, cleanupReservationMs: 200, observerSupervisor,
      });
      const readyDeadline = Date.now() + 5_000;
      while (!existsSync(markerPath) && Date.now() < readyDeadline) await new Promise(resolve => setTimeout(resolve, 5));
      const markerMs = performance.now();
      assert.ok(existsSync(markerPath), 'child emitted bytes before watchdog');
      const admitted = eventLoopStallWindow({ commandStartedMs, timeoutMs: 700, cleanupMs: 200, markerMs });
      stallWindow = { ...admitted, markerAfterMs: markerMs - commandStartedMs };
      if (admitted.violation) return command;
      let stallIterations = 0;
      while (performance.now() < admitted.stallUntilMs!) stallIterations++;
      const stallEndedMs = performance.now();
      assert.ok(stallIterations > 0);
      const completed = eventLoopStallWindow({ commandStartedMs, timeoutMs: 700, cleanupMs: 200, markerMs, stallEndedMs });
      stallWindow = { ...completed, markerAfterMs: markerMs - commandStartedMs, stallEndedAfterMs: stallEndedMs - commandStartedMs };
      return command;
    };
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), { boundedCommand: runChild });
    const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
    try {
      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-event-loop-stall', { timeoutMs: 60_000 }));
      await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-event-loop-stall', { timeoutMs: 60_000 }));
      await measurement.begin();
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-event-loop-stall', { timeoutMs: 60_000 }));
      await measurement.finish();
      assert.ok(stallWindow, 'the first GMS command was not stalled');
      assert.equal(stallWindow.violation, undefined, JSON.stringify(stallWindow));
      const observation = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      const first = observation.stages[0];
      assert.equal(first.timedOut, true);
      assert.equal(first.outcome, 'failed');
      assert.ok(first.stdoutBytes + first.stderrBytes >= 3_500_000, JSON.stringify({ stallWindow, first }));
      const ownedRecords = observerRecords(observerPath);
      const admitted = ownedRecords.find(record => record.type === 'supervisor-stopping' && record.reason === 'timeout');
      assert.ok(admitted?.targetSpawned === true && admitted.targetExecConfirmed === true,
        JSON.stringify({ ownedRecords, result: first }));
      assert.equal(first.childExited, true, JSON.stringify({ ownedRecords, result: first }));
      assert.equal(first.ownedProcessesExited, true, JSON.stringify({ ownedRecords, result: first }));
      assert.equal(first.stdioClosed, true, JSON.stringify({ ownedRecords, result: first }));
      assert.ok(ownedRecords.some(record => record.type === 'supervisor-retirement-ready'), JSON.stringify({ ownedRecords, result: first }));
      assert.equal(observation.totalCommandOutputBytes, observation.stages.reduce((sum: number, stage: any) => sum + (stage.stdoutBytes || 0) + (stage.stderrBytes || 0), 0));
      const priorBytes = observation.stages.slice(0, -1).reduce((sum: number, stage: any) => sum + (stage.stdoutBytes || 0) + (stage.stderrBytes || 0), 0);
      assert.equal(maxBytes.length, stages.length);
      assert.equal(maxBytes.at(-1), observation.limits.totalCommandOutputBytes - priorBytes);
      assert.ok(maxBytes.at(-1)! < 4_000_000, 'the fifth stage receives only the remaining aggregate allowance');
      await new Promise(resolve => setTimeout(resolve, 400));
      assert.equal(existsSync(latePath), false);
    } finally {
      await measurement.finish().catch(() => undefined);
      observerSupervisor.end();
      closeSync(observerFD);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS finalization records interrupted command bytes and monotonic stage duration', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const markerPath = join(fixture.root, 'interrupted-gms-command-ready');
    let entered!: () => void;
    const commandEntered = new Promise<void>(resolve => { entered = resolve; });
    let dispatches = 0;
    const childCode = `const fs=require('node:fs');process.stdout.write(Buffer.alloc(65536,65));fs.writeFileSync(${JSON.stringify(markerPath)},'ready');setInterval(()=>{},1000)`;
    const runChild: typeof boundedCommand = async (binary, args, timeout, options) => {
      if (!args.includes('dumpsys')) return boundedCommand(binary, args, timeout, options);
      dispatches++;
      entered();
      return boundedCommand('node', ['-e', childCode], timeout, options);
    };
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), { boundedCommand: runChild });
    let stage: Promise<void> | undefined;
    try {

      stage = measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-finalize-interruption', { timeoutMs: 20_000 }));
      await commandEntered;
      const readyDeadline = Date.now() + 5_000;
      while (!existsSync(markerPath) && Date.now() < readyDeadline) await new Promise(resolve => setTimeout(resolve, 5));
      assert.ok(existsSync(markerPath));
      const finalizeStarted = performance.now();
      await measurement.finalizeGmsObservations();
      assert.ok(performance.now() - finalizeStarted < 1_000);
      await stage;
      const path = join(fixture.root, 'android-environment-gms-observations.json');
      const bytes = await readFile(path);
      const observation = JSON.parse(bytes.toString('utf8'));
      const entry = observation.stages[0];
      assert.equal(dispatches, 1);
      assert.equal(entry.outcome, 'failed');
      assert.match(entry.error, /interrupted by finalization/u);
      assert.ok(entry.stdoutBytes > 0);
      assert.equal(entry.childExited, true);
      assert.equal(entry.ownedProcessesExited, true);
      assert.equal(entry.stdioClosed, true);
      assert.ok(entry.durationMs > 0);
      assert.ok(Date.parse(entry.endedAt) > Date.parse(entry.startedAt));
      assert.ok(entry.parseDurationMs === undefined);
      assert.equal(observation.totalCollectionMs, entry.durationMs);
      await new Promise(resolve => setTimeout(resolve, 100));
      assert.deepEqual(await readFile(path), bytes);
    } finally {
      await stage?.catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS names altered by redaction are withheld without a collected success', async () => {
    const fixture = await harness.createFixture();
    const dumpPath = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const source = await readFile(dumpPath, 'utf8');
    const secretLikeName = `com.google.android.gms.${'A'.repeat(43)}`;
    await writeFile(dumpPath, source.replace('Queries:', `      enabledComponents:\n        ${secretLikeName}\nQueries:`));
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const writers = gmsWriterDiagnostics(fixture.root);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        if (options.label?.startsWith('normalize GMS components at ')) return writers.run(binary, args, timeout, options);
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      await measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-redaction', { timeoutMs: 10_000 }));
      await measurement.finalizeGmsObservations();
      const bytes = await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8');
      const observations = JSON.parse(bytes);
      assert.equal(observations.stages[0].outcome, 'failed');
      assert.equal(observations.stages[0].state, undefined);
      assert.match(observations.stages[0].error, /cannot be retained canonically/u, JSON.stringify({
        stages: observations.stages.map(({ stage, outcome, error }: any) => ({ stage, outcome, error })), normalizationFailures: writers.failures,
      }));
      assert.doesNotMatch(bytes, /A{43}/u);
      assert.ok(measurement.failureOutcome().errors.length > 0);
    } finally {
      await writers.close().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('diagnostic GMS attack timing bounds normalization and collection by declared limits, not a composite literal', async () => {
    const limits = { ownershipTimeoutMs: 500, commandTimeoutMs: 5_000, parseReserveMs: 250, persistenceOperationTimeoutMs: 1_000 };
    const quietWindowStage = { durationMs: 681, parseDurationMs: 106 };
    const quietWindowElapsedMs = 1_054.7;
    assert.ok(quietWindowElapsedMs >= 1_000, 'the recorded quiet-window attack is rejected by the former 1000 ms composite literal');
    assert.deepEqual(gmsAttackTimingRule(limits, quietWindowStage, quietWindowElapsedMs),
      { violations: [], stageAllowanceMs: 5_750, compositeCeilingMs: 7_750 });
    assert.deepEqual(gmsAttackTimingRule(limits, { durationMs: 5_750, parseDurationMs: 250 }, 7_750).violations, []);
    const slowParserElapsedMs = 900;
    assert.ok(slowParserElapsedMs < 1_000, 'the former composite literal accepts this slow normalization');
    assert.deepEqual(gmsAttackTimingRule(limits, { durationMs: 600, parseDurationMs: 251 }, slowParserElapsedMs).violations,
      ['normalization took 251 ms, beyond its 250 ms parse reserve']);
    assert.deepEqual(gmsAttackTimingRule(limits, { durationMs: 5_751, parseDurationMs: 100 }, 6_000).violations,
      ['collection took 5751 ms, beyond its 5750 ms stage allowance']);
    assert.deepEqual(gmsAttackTimingRule(limits, quietWindowStage, 7_750.5).violations,
      ['collection and both persistence operations took 7750.5 ms, beyond 7750 ms']);
    assert.deepEqual(gmsAttackTimingRule(limits, quietWindowStage, 680).violations,
      ['elapsed 680 ms is shorter than the 681 ms collection it contains']);
    const invalidMeasurements: Array<[unknown, unknown]> = [
      [{ durationMs: 681 }, 1_000], [{ durationMs: 681, parseDurationMs: 0 }, 1_000],
      [{ durationMs: 681, parseDurationMs: Number.NaN }, 1_000], [{ parseDurationMs: 106 }, 1_000],
      [{ durationMs: -1, parseDurationMs: 106 }, 1_000], [{ durationMs: '681', parseDurationMs: 106 }, 1_000],
      [quietWindowStage, Number.POSITIVE_INFINITY], [quietWindowStage, undefined], [quietWindowStage, 0], [undefined, 1_000],
    ];
    for (const [stage, elapsedMs] of invalidMeasurements) {
      const result = gmsAttackTimingRule(limits, stage, elapsedMs);
      assert.ok(result.violations.length > 0, JSON.stringify({ stage, elapsedMs }));
      assert.equal(result.compositeCeilingMs, undefined, JSON.stringify({ stage, elapsedMs }));
    }
    const invalidLimits: unknown[] = [undefined, {}, { ...limits, parseReserveMs: 0 }, { ...limits, commandTimeoutMs: 5_000.5 },
      { ...limits, ownershipTimeoutMs: -500 }, { ...limits, persistenceOperationTimeoutMs: '1000' }];
    for (const declared of invalidLimits) {
      const result = gmsAttackTimingRule(declared, quietWindowStage, quietWindowElapsedMs);
      assert.ok(result.violations.length > 0, JSON.stringify(declared));
      assert.equal(result.stageAllowanceMs, undefined, JSON.stringify(declared));
    }
  });
  test('diagnostic GMS timeout probe reserves ownership and parsing before testing the command deadline', async () => {
    const limits = { persistenceOperationTimeoutMs: 5_000, ownershipTimeoutMs: 2_000, parseReserveMs: 1_000 };
    const probeBudgetMs = gmsTimeoutProbeBudgetMs(limits);
    assert.equal(probeBudgetMs, 8_000);
    const shortParent = { timeoutMs: limits.persistenceOperationTimeoutMs + 2_250, ownershipCostMs: 1_300 };
    const fullParent = { timeoutMs: probeBudgetMs, ownershipCostMs: 300 };
    for (const { timeoutMs: parentTimeoutMs, ownershipCostMs } of [shortParent, fullParent]) {
      const fixture = await harness.createFixture();
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      let clockMs = 0;
      let clockReads = 0;
      let dispatches = 0;
      let commandTimeoutMs: number | undefined;
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
        monotonicNow: () => {
          clockReads++;
          if (clockReads === 3) clockMs += ownershipCostMs;
          return clockMs;
        },
        boundedCommand: async (binary, args, timeout, options) => {
          if (!args.includes('dumpsys')) return boundedCommand(binary, args, timeout, options);
          dispatches++;
          commandTimeoutMs = timeout;
          throw new BoundedCommandError(binary, args, fakeCommandResult('', { code: 1, timedOut: true }));
        },
      });
      try {
        await measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-timeout-probe', { timeoutMs: parentTimeoutMs, now: () => clockMs }));
        await measurement.finalizeGmsObservations();
        const snapshot = JSON.parse(await readFile(join(fixture.root, 'android-environment-gms-observations.json'), 'utf8'));
        const stage = snapshot.stages[0];
        assert.deepEqual(snapshot.limits.ownershipTimeoutMs, limits.ownershipTimeoutMs);
        assert.equal(stage.outcome, 'failed');
        assert.equal(stage.ownershipDurationMs, ownershipCostMs);
        if (parentTimeoutMs === shortParent.timeoutMs) {
          assert.equal(dispatches, 0);
          assert.match(stage.error, /owned-device verification exceeded its monotonic time bound/u);
          continue;
        }
        assert.equal(commandTimeoutMs, 1_700);
        assert.equal(stage.timedOut, true);
        assert.match(stage.error, /GMS diagnostic command timed out/u);
      } finally {
        await measurement.finish().catch(() => undefined);
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    }
  });
  test('diagnostic GMS errors are bounded, explicit and fail closed at the collection layer', async () => {
    const fixture = await harness.createFixture();
    const dumpPath = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const originalDump = await readFile(dumpPath, 'utf8');
    assert.throws(() => parseAndroidGmsComponentState(`${originalDump}${' '.repeat(2_000_000)}`), /exceeds parsing limit/u);
    const continuationCount = 65_537 - originalDump.split('\n').length;
    const continuationAttack = originalDump.replace('Queries:', `${' optional:true\n'.repeat(continuationCount)}Queries:`);
    assert.ok(Buffer.byteLength(continuationAttack) < 2_000_000);
    const parseStarted = performance.now();
    assert.throws(() => parseAndroidGmsComponentState(continuationAttack), /exceeds line limit/u);
    assert.ok(performance.now() - parseStarted < 1_000);
    const attackFixture = await harness.createFixture();
    const attackEnvironment = { ...process.env };
    Object.assign(process.env, attackFixture.environment);
    const compactAttack = originalDump.replace('Queries:', `${' optional:true\n'.repeat(512)}${'\n'.repeat(continuationCount - 512)}Queries:`);
    await writeFile(join(attackFixture.fixtureDirectory, 'valid-gms.dump'), compactAttack);
    let attackCommandFailure: BoundedCommandError | undefined;
    let attackParserFailure: BoundedCommandError | undefined;
    const failureMetadata = (failure: BoundedCommandError | undefined) => {
      if (!failure) return undefined;
      return Object.fromEntries(Object.entries(failure.result).filter(([key]) => key !== 'stdout' && key !== 'stderr'));
    };
    const attackMeasurement = new AndroidEnvironmentMeasurement('emulator-5554', attackFixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      boundedCommand: async (binary, args, timeout, options) => {
        try { return await boundedCommand(binary, args, timeout, options); }
        catch (error) {
          if (error instanceof BoundedCommandError) {
            if (args.includes('dumpsys')) attackCommandFailure = error;
            if (args.includes('--eval')) attackParserFailure = error;
          }
          throw error;
        }
      },
    });
    let limits: { ownershipTimeoutMs: number; parseReserveMs: number; persistenceOperationTimeoutMs: number } | undefined;
    try {

      const attackStarted = performance.now();
      await attackMeasurement.observeGmsStage('after-device-start', new PhaseBudget('gms-parser-bound', { timeoutMs: 10_000 }));
      await attackMeasurement.finalizeGmsObservations();
      const attackElapsedMs = performance.now() - attackStarted;
      const attackResult = JSON.parse(await readFile(join(attackFixture.root, 'android-environment-gms-observations.json'), 'utf8'));
      assert.equal(attackResult.stages[0].outcome, 'failed');
      assert.match(attackResult.stages[0].error, /exceeds line limit/u,
        JSON.stringify({ stage: attackResult.stages[0], commandResult: failureMetadata(attackCommandFailure) }));
      assert.ok(attackResult.stages[0].parseDurationMs > 0);
      assert.deepEqual([attackResult.limits.ownershipTimeoutMs, attackResult.limits.commandTimeoutMs, attackResult.limits.parseReserveMs,
        attackResult.limits.persistenceOperationTimeoutMs, attackResult.limits.totalCollectionMs], [2_000, 5_000, 1_000, 5_000, 5 * 8_000]);
      limits = attackResult.limits;
      const attackTiming = gmsAttackTimingRule(attackResult.limits, attackResult.stages[0], attackElapsedMs);
      assert.deepEqual(attackTiming.violations, [], JSON.stringify({ attackElapsedMs, stage: attackResult.stages[0],
        commandResult: failureMetadata(attackCommandFailure), parserResult: failureMetadata(attackParserFailure) }));
      assert.equal(attackTiming.stageAllowanceMs, 8_000);
      assert.equal(attackTiming.compositeCeilingMs, 8_000 + 2 * attackResult.limits.persistenceOperationTimeoutMs);
      assert.equal(attackResult.totalCollectionMs, attackResult.stages[0].durationMs);
    } finally {
      for (const key of Object.keys(process.env)) if (!(key in attackEnvironment)) delete process.env[key];
      Object.assign(process.env, attackEnvironment);
    }
    const ownershipAndParseReservedBudgetMs = gmsTimeoutProbeBudgetMs(limits!);
    const cases = ['malformed', 'timeout', 'wrong-owner', 'stage-order'] as const;
    for (const failure of cases) {
      const current = await harness.createFixture();
      const saved = { ...process.env };
      Object.assign(process.env, current.environment);
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', current.root, repositoryPath('tests/mobile/toolchains.json'));
      const budget = new PhaseBudget(`gms-${failure}`, {
        timeoutMs: failure === 'timeout' ? ownershipAndParseReservedBudgetMs : limits!.persistenceOperationTimeoutMs + 4_000,
      });
      try {

        if (failure === 'malformed') await writeFile(join(current.fixtureDirectory, 'valid-gms.dump'), originalDump.slice(0, -10));
        if (failure === 'timeout') await writeFile(join(current.fixtureDirectory, 'valid-vending.json'), JSON.stringify({ gmsTimeout: true }));
        if (failure === 'wrong-owner') await writeFile(current.environment.MOBILE_DEVICE_OWNERSHIP_FILE!, 'android:other-device');
        if (failure === 'stage-order') await measurement.observeGmsStage('after-setup-url', budget);
        else await measurement.observeGmsStage('after-device-start', budget);
        await measurement.finalizeGmsObservations();
        const observation = JSON.parse(await readFile(join(current.root, 'android-environment-gms-observations.json'), 'utf8'));
        assert.equal(observation.stages[0].outcome, 'failed', failure);
        assert.ok(observation.errors.length > 0, failure);
        assert.ok(measurement.failureOutcome().errors.length > 0, failure);
        if (failure === 'wrong-owner') {
          const adbLog = existsSync(current.log) ? await readFile(current.log, 'utf8') : '';
          assert.doesNotMatch(adbLog, /shell dumpsys package com\.google\.android\.gms/u);
        }
        if (failure === 'timeout') {
          assert.ok(observation.stages[0].timeoutMs <= limits!.ownershipTimeoutMs, JSON.stringify(observation.stages[0]));
          assert.ok(observation.stages[0].timedOut, JSON.stringify(observation.stages[0]));
        }
        if (failure === 'stage-order') assert.match(observation.stages[0].error, /stage order mismatch/u);
      } finally {
        await measurement.finish().catch(() => undefined);
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    }
  });
  return tests;
}
