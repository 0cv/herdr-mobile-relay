import { existsSync, readFileSync } from 'node:fs';
import { readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { repositoryPath } from '../support/paths';
import { ANDROID_LOG_LIMIT } from '../android-environment';
import { BoundedCommandError, boundedCommand } from '../support/bounded-process';
import { spawnOwnedProcess } from '../support/owned-process';
import { PhaseBudget } from '../support/budget';
import { redactText } from '../support/diagnostics';
import { androidTransportNormalRecords } from './android-transport';
import { fixtureTransportObservation } from './android-measurement-doubles';
import { AndroidTransportTriggerDetector, transportAdbObservation, type AndroidTransportCommandResult } from '../support/android-transport';
import assert from 'node:assert/strict';
import { androidCliTestRegistry, type Harness, type Test, AndroidEnvironmentMeasurement, beginObservedMeasurement, createAndroidTransportTestEndpoint, createAndroidTransportFixture, waitForPidAbsent, waitForValue, waitForFileValue } from './android-environment';

export function androidEnvironmentTransportTests(harness: Harness): Test[] {
  const { tests, test } = androidCliTestRegistry();
  test('GMS transport diagnostics capture host and ADB state at the live flush trigger', async () => {
    const fixture = await harness.createFixture();
    const endpoint = await createAndroidTransportTestEndpoint();
    const transportFixture = await createAndroidTransportFixture(fixture.root, endpoint.port, 'trigger-time-state');
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const triggerPath = join(fixture.root, 'emit-transport-trigger');
    const endPath = join(fixture.root, 'emit-measurement-end');
    const observationPath = join(fixture.root, 'android-transport-observation.json');
    let activeMeasurement = false;
    let finishStarted = false;
    let resolveWorkerStarted!: () => void;
    const workerStarted = new Promise<void>(resolve => { resolveWorkerStarted = resolve; });
    let resolveWorkerFinished!: () => void;
    const workerFinished = new Promise<void>(resolve => { resolveWorkerFinished = resolve; });
    let capturedRequest: any;
    let workerDispatches = 0;
    let collectorCode = '';
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      transportFixture,
      spawnCollector: () => spawnOwnedProcess(process.execPath, ['--no-env-file', '-e', collectorCode], { timeoutMs: 50_000, cleanupReservationMs: 2_000 }),
      boundedCommand: async (binary, args, timeout, options) => {
        if (!args.includes('--android-transport-observation-trigger')) return boundedCommand(binary, args, timeout, options);
        assert.equal(activeMeasurement, true);
        assert.equal(finishStarted, false);
        workerDispatches++;
        capturedRequest = JSON.parse(String(options.input));
        resolveWorkerStarted();
        try { return await boundedCommand(binary, args, timeout, options); }
        finally { resolveWorkerFinished(); }
      },
    });
    collectorCode = `const fs=require('node:fs');setTimeout(()=>{const epoch=(Date.now()/1000).toFixed(3);process.stdout.write(epoch.padStart(23)+'  5606  5606 I HerdrMeasure: ${measurement.id} START\\n')},30);const timer=setInterval(()=>{if(fs.existsSync(${JSON.stringify(triggerPath)})){fs.rmSync(${JSON.stringify(triggerPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3).padStart(23)+'   546   561 I adbd    : timeout expired while flushing socket, closing\\n')}if(fs.existsSync(${JSON.stringify(endPath)})){fs.rmSync(${JSON.stringify(endPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3).padStart(23)+'  6233  6233 I HerdrMeasure: ${measurement.id} END\\n')}},5);process.on('SIGTERM',()=>{clearInterval(timer);process.exit(0)})`;
    try {
      const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;

      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-transport-trigger', { timeoutMs: 20_000 }));
      await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-transport-trigger', { timeoutMs: 20_000 }));
      await measurement.begin();
      activeMeasurement = true;
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-transport-trigger', { timeoutMs: 20_000 }));
      await writeFile(triggerPath, 'trigger');
      await workerStarted;
      assert.equal(capturedRequest.serial, 'emulator-5554');
      assert.ok(Number.isFinite(Number(capturedRequest.guestEpochSeconds)));
      assert.ok(Date.parse(capturedRequest.triggeredAt) <= Date.now());
      await waitForValue(() => {
        const acquired = existsSync(transportFixture.acquisitionLogFile)
          ? readFileSync(transportFixture.acquisitionLogFile, 'utf8').trim().split('\n').filter(Boolean) : [];
        return acquired.length === 3 && endpoint.completed.length === 3 ? true : undefined;
      }, 10_000, 'real transport acquisition did not complete all five host and ADB observations');
      await workerFinished;
      assert.equal(workerDispatches, 1);
      assert.deepEqual(endpoint.completed.sort(), ['adb-state', 'guest-clock', 'guest-uptime']);
      await writeFile(transportFixture.stateFile, 'shutdown-time-state');
      finishStarted = true;
      const finish = measurement.finish();
      await writeFile(endPath, 'end');
      await finish;
      const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
      assert.equal(collector.transportObservationFailure, undefined);
      const observation = JSON.parse(await readFile(observationPath, 'utf8'));
      assert.equal(observation.trigger, 'guest-adbd-flush-timeout');
      assert.equal(observation.acquisitionStatus, 'collected');
      assert.equal(observation.triggerValidation.absoluteEmissionProven, false);
      assert.deepEqual(observation.observations.map((entry: any) => entry.name), [
        'host-processes', 'host-memory', 'host-tcp', 'adb-state', 'guest-uptime',
      ]);
      assert.equal(observation.observations.find((entry: any) => entry.name === 'host-processes').stdout, 'trigger-time-state\n');
      assert.equal(observation.observations.find((entry: any) => entry.name === 'host-memory').stdout, 'MemAvailable: trigger-time-state\n');
      assert.equal(observation.observations.find((entry: any) => entry.name === 'host-tcp').stdout, 'State: trigger-time-state\n');
      assert.equal(observation.observations.find((entry: any) => entry.name === 'adb-state').stdout, 'device');
      assert.match(observation.observations.find((entry: any) => entry.name === 'guest-uptime').stdout, /^1234\.56/u);
      assert.deepEqual(endpoint.requests, ['host:transport:emulator-5554', 'shell:date +%s.%N',
        'host-serial:emulator-5554:get-state', 'host:transport:emulator-5554',
        'shell:echo transport-observation; cat /proc/uptime']);
      assert.deepEqual(existsSync(transportFixture.acquisitionLogFile)
        ? readFileSync(transportFixture.acquisitionLogFile, 'utf8').trim().split('\n').sort() : [],
      ['host-memory', 'host-processes', 'host-tcp']);
      assert.ok(Date.parse(observation.triggeredAt) < Date.parse(observation.endedAt));
      assert.equal(workerDispatches, 1);
    } finally {
      finishStarted = true;
      await writeFile(endPath, 'end');
      await measurement.finish().catch(() => undefined);
      await endpoint.stop();
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS ADB live acquisition rejects empty and incomplete state or uptime replies', async () => {
    const cases = [
      { name: 'empty-state', service: 'get-state' as const, stateFrame: Buffer.from('OKAY0000') },
      { name: 'partial-state', service: 'get-state' as const, stateFrame: Buffer.from('OKAY0006dev') },
      { name: 'empty-uptime', service: 'uptime' as const, uptimeFrame: Buffer.from('OKAY') },
      { name: 'partial-uptime', service: 'uptime' as const, uptimeFrame: Buffer.from('OKAY1234.56 ') },
    ];
    for (const current of cases) {
      const endpoint = await createAndroidTransportTestEndpoint(false, current);
      try {
        const result = await transportAdbObservation('emulator-5554', current.service, endpoint.port, 1_500);
        assert.equal(result.unavailable, true, current.name);
        assert.equal(result.exitCode, null, current.name);
        assert.equal(result.timedOut, false, current.name);
        assert.equal(result.truncated, false, current.name);
        assert.deepEqual(endpoint.requests, current.service === 'get-state'
          ? ['host-serial:emulator-5554:get-state']
          : ['host:transport:emulator-5554', 'shell:echo transport-observation; cat /proc/uptime']);
        const fixture = await harness.createFixture();
        const transportFixture = await createAndroidTransportFixture(fixture.root, endpoint.port, 'present-host');
        const runHost = async (): Promise<AndroidTransportCommandResult> => {
          const timestamp = new Date().toISOString();
          return { startedAt: timestamp, endedAt: timestamp, stdout: 'present-host\n', stderr: '',
            exitCode: 0, signal: null, timedOut: false, truncated: false, unavailable: false };
        };
        const observation = fixtureTransportObservation('emulator-5554', fixture.root, transportFixture, runHost);
        await observation.observeTrigger({ guestEpochSeconds: '123.456', triggeredAt: new Date(Date.now() - 1_000).toISOString() });
        assert.equal(observation.failure, 'transport acquisition was unavailable or failed');
        const record = JSON.parse(await readFile(join(fixture.root, 'android-transport-observation.json'), 'utf8'));
        assert.equal(record.acquisitionStatus, 'failed', current.name);
        assert.equal(record.observations.find((entry: { name: string }) => entry.name === (current.service === 'get-state' ? 'adb-state' : 'guest-uptime')).unavailable, true);
      } finally {
        await endpoint.stop();
      }
      assert.equal(endpoint.closed, endpoint.connected, `${current.name} leaves no ADB transport connection alive`);
    }
  });
  test('GMS transport permits one first post-MAIN SYSTEM divider while fencing replay and cursor restarts', async () => {
    const id = 'ca597b37-30b3-4d90-9127-a0c30047c6a0';
    const marker = (epoch: string, name: string) => `${epoch} 77 77 I HerdrMeasure: ${id} ${name}\n`;
    const flush = (epoch: string) => `${epoch} 77 77 I adbd: timeout expired while flushing socket, closing\n`;
    const replay = new AndroidTransportTriggerDetector(id);
    assert.deepEqual(replay.observe(Buffer.from('--------- beginning of system\n'), 99), []);
    assert.deepEqual(replay.observe(Buffer.from(flush('1790000000.900')), 100), []);
    assert.deepEqual(replay.observe(Buffer.from(marker('1790000000.100', 'START')), 101), []);
    const replayFenced = replay.observe(Buffer.from(flush('1790000000.200')), 102);
    assert.equal(replayFenced.length, 1);
    assert.equal(replayFenced[0].guestEpochSeconds, '1790000000.200');
    assert.equal(replayFenced[0].anchorGuestEpochSeconds, '1790000000.100');
    assert.equal(replayFenced[0].anchorMonotonicMs, 101);

    const firstSystem = new AndroidTransportTriggerDetector(id);
    assert.deepEqual(firstSystem.observe(Buffer.from(marker('1790000000.100', 'START')), 101), []);
    assert.deepEqual(firstSystem.observe(Buffer.from('--------- beginning of system\n'), 102), []);
    const candidates = firstSystem.observe(Buffer.from(flush('1790000000.200')), 103);
    assert.equal(candidates.length, 1);
    assert.equal(candidates[0].anchorGuestEpochSeconds, '1790000000.100');
    assert.equal(candidates[0].anchorMonotonicMs, 101);
    assert.equal(firstSystem.accept(), true);
    assert.deepEqual(firstSystem.observe(Buffer.from(flush('1790000000.300')), 104), []);
    assert.deepEqual(firstSystem.observe(Buffer.from('--------- beginning of system\n'), 105), []);
    assert.equal(firstSystem.failure, 'collector log buffer cursor restarted after admission');

    const repeatedSystem = new AndroidTransportTriggerDetector(id);
    repeatedSystem.observe(Buffer.from(marker('1790000000.100', 'START')), 101);
    repeatedSystem.observe(Buffer.from('--------- beginning of system\n'), 102);
    assert.deepEqual(repeatedSystem.observe(Buffer.from('--------- beginning of system\n'), 103), []);
    assert.equal(repeatedSystem.failure, 'collector log buffer cursor restarted after admission');

    const restartedMain = new AndroidTransportTriggerDetector(id);
    restartedMain.observe(Buffer.from(marker('1790000000.100', 'START')), 101);
    assert.deepEqual(restartedMain.observe(Buffer.from('--------- beginning of main\n'), 102), []);
    assert.equal(restartedMain.failure, 'collector log buffer cursor restarted after admission');

    const before = new AndroidTransportTriggerDetector(id);
    before.observe(Buffer.from('--------- beginning of system\n'), 99);
    assert.deepEqual(before.observe(Buffer.from('--------- beginning of system\n'), 100), []);
    assert.equal(before.failure, 'collector log buffer cursor restarted after admission');
    const duplicated = new AndroidTransportTriggerDetector(id);
    duplicated.observe(Buffer.from(marker('1790000000.100', 'START')), 101);
    assert.deepEqual(duplicated.observe(Buffer.from(marker('1790000000.100', 'START')), 102), []);
    assert.equal(duplicated.failure, 'collector measurement start marker was malformed or duplicated');
    const malformed = new AndroidTransportTriggerDetector(id);
    assert.deepEqual(malformed.observe(Buffer.from(`1790000000.100 77 77 I HerdrMeasure: ${id} INVALID START\n`), 101), []);
    assert.equal(malformed.failure, 'collector measurement start marker was malformed or duplicated');
    for (const suffix of ['START ', 'START extra', 'SYSTEM_START']) {
      const trailing = new AndroidTransportTriggerDetector(id);
      trailing.observe(Buffer.from(marker('1790000000.100', 'START')), 101);
      assert.deepEqual(trailing.observe(Buffer.from(marker('1790000000.200', suffix)), 102), []);
      assert.equal(trailing.failure, 'collector measurement start marker was malformed or duplicated');
    }
    // Device logcat right-aligns the epoch column and pads short tags, as recorded on the hosted emulator.
    const deviceFormat = new AndroidTransportTriggerDetector(id);
    assert.deepEqual(deviceFormat.observe(Buffer.from(`         1790000000.100  5606  5606 I HerdrMeasure: ${id} START\n`), 101), []);
    assert.equal(deviceFormat.failure, undefined);
    const deviceCandidates = deviceFormat.observe(Buffer.from('         1790000000.200   546   561 I adbd    : timeout expired while flushing socket, closing\n'), 102);
    assert.equal(deviceFormat.failure, undefined);
    assert.equal(deviceCandidates.length, 1);
    assert.equal(deviceCandidates[0].guestEpochSeconds, '1790000000.200');
    assert.equal(deviceCandidates[0].anchorGuestEpochSeconds, '1790000000.100');
    const deviceDuplicate = new AndroidTransportTriggerDetector(id);
    deviceDuplicate.observe(Buffer.from(`         1790000000.100  5606  5606 I HerdrMeasure: ${id} START\n`), 101);
    assert.deepEqual(deviceDuplicate.observe(Buffer.from(`         1790000000.150  5606  5606 I HerdrMeasure: ${id} START\n`), 102), []);
    assert.equal(deviceDuplicate.failure, 'collector measurement start marker was malformed or duplicated');
    const deviceMalformedTrigger = new AndroidTransportTriggerDetector(id);
    deviceMalformedTrigger.observe(Buffer.from(`         1790000000.100  5606  5606 I HerdrMeasure: ${id} START\n`), 101);
    assert.deepEqual(deviceMalformedTrigger.observe(Buffer.from('         1790000000.200   546   561 I adbd    : timeout expired while flushing socket, closing now\n'), 102), []);
    assert.equal(deviceMalformedTrigger.failure, 'collector transport trigger record was malformed');
    const unrelated = new AndroidTransportTriggerDetector(id);
    assert.deepEqual(unrelated.observe(Buffer.from(flush('1790000000.900').replace('77 77 I adbd:', 'malformed adbd:')), 100), []);
    assert.equal(unrelated.failure, undefined);
    assert.deepEqual(unrelated.observe(Buffer.from(marker('1790000000.100', 'START')), 101), []);
    assert.deepEqual(unrelated.observe(Buffer.from(marker('1790000000.150', 'OP_BEGIN operation app 77')), 102), []);
  });
  test('Android15 fake ADB rejects unsupported system-buffer marker writing', async () => {
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    try {
      await assert.rejects(
        boundedCommand('adb', ['-s', 'emulator-5554', 'shell', 'log', '-b', 'system', '-p', 'i', '-t', 'HerdrMeasure', "'unsupported SYSTEM_START'"],
          3_000, { maxBytes: 1_024 }),
        (error: unknown) => error instanceof BoundedCommandError && error.result.code !== 0
          && error.result.childExited && error.result.ownedProcessesExited && error.result.stdioClosed,
      );
      assert.match(await readFile(fixture.log, 'utf8'), /shell log -b system -p i -t HerdrMeasure/u);
    } finally {
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS future-dated pre-MAIN SYSTEM replay never dispatches a transport acquisition', async () => {
    const fixture = await harness.createFixture();
    const endpoint = await createAndroidTransportTestEndpoint();
    const transportFixture = await createAndroidTransportFixture(fixture.root, endpoint.port, 'trigger-time-state');
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const endPath = join(fixture.root, 'emit-measurement-end');
    let dispatches = 0;
    let collectorCode = '';
    let replayEpoch = '';
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      transportFixture,
      spawnCollector: () => {
        replayEpoch = ((Date.now() + 500) / 1000).toFixed(3);
        collectorCode = `const fs=require('node:fs');setTimeout(()=>{process.stdout.write('--------- beginning of system\\n'+${JSON.stringify(replayEpoch)}+' 77 77 I adbd: timeout expired while flushing socket, closing\\n'+(Date.now()/1000).toFixed(3)+' 77 77 I HerdrMeasure: ${measurement.id} START\\n')},30);const timer=setInterval(()=>{if(fs.existsSync(${JSON.stringify(endPath)})){fs.rmSync(${JSON.stringify(endPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I HerdrMeasure: ${measurement.id} END\\n')}},5);process.on('SIGTERM',()=>{clearInterval(timer);process.exit(0)})`;
        return spawnOwnedProcess(process.execPath, ['--no-env-file', '-e', collectorCode], { timeoutMs: 50_000, cleanupReservationMs: 2_000 });
      },
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('--android-transport-observation-trigger')) dispatches++;
        return boundedCommand(binary, args, timeout, options);
      },
    });
    try {
      await measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-pre-anchor-replay', { timeoutMs: 20_000 }));
      await measurement.observeGmsStage('before-measurement-begin', new PhaseBudget('gms-pre-anchor-replay', { timeoutMs: 20_000 }));
      await measurement.begin();
      assert.ok(Math.abs(Number(replayEpoch) * 1_000 - Date.now()) < 2_000);
      const sample = await transportAdbObservation('emulator-5554', 'clock', endpoint.port, 1_500);
      assert.equal(sample.unavailable, false);
      assert.ok(Math.abs(Number(sample.stdout.trim()) - Number(replayEpoch)) < 2);
      for (const stage of ['after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const) {
        await measurement.observeGmsStage(stage, new PhaseBudget('gms-pre-anchor-replay', { timeoutMs: 20_000 }));
      }
      const finish = measurement.finish();
      await writeFile(endPath, 'end');
      const outcome = await finish;
      assert.equal(dispatches, 0);
      assert.deepEqual(endpoint.completed, ['guest-clock']);
      assert.equal(existsSync(transportFixture.acquisitionLogFile), false);
      assert.equal(outcome.assessment?.collection.status, 'PASS', outcome.assessment?.collection.status === 'PASS' ? 'collection passed'
        : redactText(JSON.stringify({ errors: outcome.errors, collection: outcome.assessment?.collection }))
          .split(fixture.root).join('<root>').slice(0, 8192));
    } finally {
      await writeFile(endPath, 'end');
      await measurement.finish().catch(() => undefined);
      await endpoint.stop();
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  for (const mode of ['normal', 'oversized', 'eof'] as const) {
    test(`Android transport framing measurement preserves evidence and classification ${mode}`, async () => {
      const fixture = await harness.createFixture();
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      const endPath = join(fixture.root, 'framing-end');
      const observed: Buffer[] = [];
      let dispatches = 0;
      let collectorCode = '';
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
        spawnCollector: () => {
          const collector = spawnOwnedProcess(process.execPath, ['--no-env-file', '-e', collectorCode], { timeoutMs: 50_000, cleanupReservationMs: 2_000 });
          collector.stdout.on('data', chunk => { observed.push(Buffer.from(chunk)); });
          return collector;
        },
        boundedCommand: async (binary, args, timeout, options) => {
          if (args.includes('--android-transport-observation-trigger')) dispatches++;
          const result = await boundedCommand(binary, args, timeout, options);
          if (args.includes('snapshot') && args[args.indexOf('--boundary') + 1] === 'end') await writeFile(endPath, 'end');
          return result;
        },
      });
      const normal = androidTransportNormalRecords.join('\r\n') + '\r\n'
        + '1790000000.150 77 77 I normal: ' + 'é🙂'.repeat(600) + '\n';
      const tail = mode === 'normal' ? '' : mode === 'oversized'
        ? '1790000000.200 77 77 I normal: ' + 'x'.repeat(2050) + ' adbd: timeout expired while flushing socket, closing\n'
        : '1790000000.200 77 77 I adbd: timeout expired while flushing soc';
      collectorCode = `const fs=require('node:fs');setTimeout(()=>{const epoch=(Date.now()/1000).toFixed(3);process.stdout.write(epoch+' 77 77 I HerdrMeasure: ${measurement.id} START\\n');const bytes=Buffer.from(${JSON.stringify(normal)}.replaceAll('1790000000.150',epoch));for(let offset=0;offset<bytes.length;offset+=17)process.stdout.write(bytes.subarray(offset,offset+17))},30);const timer=setInterval(()=>{if(fs.existsSync(${JSON.stringify(endPath)})){fs.rmSync(${JSON.stringify(endPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I HerdrMeasure: ${measurement.id} END\\n')}},5);process.on('SIGTERM',()=>{clearInterval(timer);process.stdout.write(${JSON.stringify(tail)},()=>process.exit(0))})`;
      try {
        const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;
        for (const stage of stages.slice(0, 2)) await measurement.observeGmsStage(stage, new PhaseBudget('framing-measurement', { timeoutMs: 20_000 }));
        await measurement.begin();
        for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('framing-measurement', { timeoutMs: 20_000 }));
        const outcome = await measurement.finish();
        const raw = Buffer.concat(observed);
        const persisted = await readFile(join(fixture.root, 'android-qualification-logcat.log'));
        assert.deepEqual(persisted, raw);
        assert.ok(raw.toString('utf8').endsWith(tail));
        const epoch = raw.subarray(0, raw.indexOf(32)).toString('utf8');
        assert.match(epoch, /^\d{10}\.\d{3}$/u);
        assert.ok(raw.includes(Buffer.from(normal.replaceAll('1790000000.150', epoch))));
        const summary = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
        assert.equal(summary.acquiredBytes, raw.length);
        assert.equal(summary.bytes, raw.length);
        assert.equal(summary.failure, undefined);
        assert.equal(dispatches, 0);
        assert.equal(summary.gmsObservations.terminalConfirmation, 'confirmed');
        if (mode === 'normal') {
          assert.equal(summary.transportObservationFailure, undefined);
          assert.equal(outcome.assessment?.collection.status, 'PASS', JSON.stringify(outcome.assessment?.collection));
          assert.deepEqual(outcome.errors, []);
        } else {
          assert.equal(summary.transportObservationFailure, mode === 'oversized'
            ? 'collector trigger record exceeded its framing bound' : 'collector trigger record was incomplete at EOF');
          assert.equal(outcome.assessment?.collection.status, 'UNKNOWN');
          assert.ok(outcome.assessment?.findings.some(finding => finding.category === 'EVIDENCE_INVALID'));
          assert.ok(outcome.errors.length > 0);
        }
      } finally {
        await writeFile(endPath, 'end');
        await measurement.finish().catch(() => undefined);
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    });
  }
  test('Android transport framing acquisition ceiling precedes trigger detection', async () => {
    assert.equal(ANDROID_LOG_LIMIT, 100 * 1024 * 1024);
    const fixture = await harness.createFixture();
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
    try {
      await beginObservedMeasurement(measurement);
      const internal = measurement as unknown as {
        collector: { bytes: number; stdoutHandler: (chunk: Buffer) => void; process: { stop: (reason: string) => void }; failure?: Error; chunks: Buffer[] };
        transport: { trigger: AndroidTransportTriggerDetector };
      };
      let detectorCalls = 0;
      const stops: string[] = [];
      internal.transport.trigger.observe = () => { detectorCalls++; return []; };
      const stop = internal.collector.process.stop.bind(internal.collector.process);
      internal.collector.process.stop = reason => { stops.push(reason); };
      const chunk = Buffer.from('ordinary message\n');
      const chunksBefore = internal.collector.chunks.length;
      internal.collector.bytes = ANDROID_LOG_LIMIT - chunk.length;
      internal.collector.stdoutHandler(chunk);
      assert.equal(detectorCalls, 1);
      assert.equal(internal.collector.chunks.length, chunksBefore + 1);
      internal.collector.stdoutHandler(chunk);
      internal.collector.process.stop = stop;
      assert.equal(detectorCalls, 1);
      assert.equal(internal.collector.chunks.length, chunksBefore + 1);
      assert.deepEqual(stops, ['collector-output-limit']);
      assert.equal(internal.collector.failure?.message, 'ANDROID_ENVIRONMENT: measured log exceeds bound');
    } finally {
      await measurement.finish().catch(() => undefined);
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS transport trigger framing starts one live acquisition', async () => {
    const fixture = await harness.createFixture();
    const endpoint = await createAndroidTransportTestEndpoint();
    const transportFixture = await createAndroidTransportFixture(fixture.root, endpoint.port, 'trigger-time-state');
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const stalePath = join(fixture.root, 'emit-stale-transport-trigger');
    const releasePath = join(fixture.root, 'release-split-transport-trigger');
    const completePath = join(fixture.root, 'complete-split-transport-trigger');
    const endPath = join(fixture.root, 'emit-measurement-end');
    const staleLine = '123.456 77 77 I adbd: timeout expired while flushing socket, closing';
    const freshPrefix = ' 77 77 I adbd: timeout expired while flushing soc';
    const observedChunks: string[] = [];
    const observedStderr: string[] = [];
    let active = false;
    let finishStarted = false;
    let workerDispatches = 0;
    let capturedRequest: Record<string, any> | undefined;
    let resolveWorkerStarted!: () => void;
    const workerStarted = new Promise<void>(resolve => { resolveWorkerStarted = resolve; });
    let resolveWorkerFinished!: () => void;
    const workerFinished = new Promise<void>(resolve => { resolveWorkerFinished = resolve; });
    let collectorCode = '';
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      transportFixture,
      spawnCollector: () => {
        const collector = spawnOwnedProcess(process.execPath, ['--no-env-file', '-e', collectorCode], { timeoutMs: 50_000, cleanupReservationMs: 2_000 });
        collector.stdout.on('data', chunk => { observedChunks.push(String(chunk)); });
        collector.stderr.on('data', chunk => { observedStderr.push(String(chunk)); });
        return collector;
      },
      boundedCommand: async (binary, args, timeout, options) => {
        if (!args.includes('--android-transport-observation-trigger')) return boundedCommand(binary, args, timeout, options);
        assert.equal(active, true);
        assert.equal(finishStarted, false);
        workerDispatches++;
        capturedRequest = JSON.parse(String(options.input));
        resolveWorkerStarted();
        try { return await boundedCommand(binary, args, timeout, options); }
        finally { resolveWorkerFinished(); }
      },
    });
    collectorCode = `const fs=require('node:fs');let split=0;let fresh='';setTimeout(()=>{const epoch=(Date.now()/1000).toFixed(3);process.stdout.write(epoch+' 77 77 I HerdrMeasure: ${measurement.id} START\\n--------- beginning of system\\n')},30);const timer=setInterval(()=>{if(fs.existsSync(${JSON.stringify(stalePath)})){fs.rmSync(${JSON.stringify(stalePath)},{force:true});process.stdout.write(${JSON.stringify(staleLine)}+'\\n')}if(fs.existsSync(${JSON.stringify(releasePath)})){fs.rmSync(${JSON.stringify(releasePath)},{force:true});split=1;fresh=(Date.now()/1000).toFixed(3);process.stdout.write(fresh+${JSON.stringify(freshPrefix)})}if(split===1&&fs.existsSync(${JSON.stringify(completePath)})){fs.rmSync(${JSON.stringify(completePath)},{force:true});split=2;process.stdout.write('ket, closing\\n'+fresh+${JSON.stringify(freshPrefix)}+'ket, closing\\n')}if(fs.existsSync(${JSON.stringify(endPath)})){fs.rmSync(${JSON.stringify(endPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I HerdrMeasure: ${measurement.id} END\\n')}},5);process.on('SIGTERM',()=>{clearInterval(timer);process.exit(0)})`;
    try {
      const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;

      await measurement.observeGmsStage(stages[0], new PhaseBudget('gms-transport-framing', { timeoutMs: 20_000 }));
      await measurement.observeGmsStage(stages[1], new PhaseBudget('gms-transport-framing', { timeoutMs: 20_000 }));
      await measurement.begin();
      active = true;
      assert.ok(observedChunks.some(chunk => chunk.includes('--------- beginning of system\n')),
        'collector must deliver the first SYSTEM divider after measurement START');
      for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget('gms-transport-framing', { timeoutMs: 20_000 }));
      await writeFile(stalePath, 'stale');
      await waitForValue(() => observedChunks.some(chunk => chunk.includes(staleLine)) ? true : undefined,
        5_000, 'collector did not deliver the stale replay after measurement START');
      assert.equal(workerDispatches, 0);
      await writeFile(releasePath, 'release');
      await waitForValue(() => observedChunks.some(chunk => chunk.includes(freshPrefix)) ? true : undefined,
        5_000, 'collector did not deliver the fresh split trigger prefix');
      assert.equal(workerDispatches, 0);
      await writeFile(completePath, 'complete');
      try {
        await waitForValue(() => workerDispatches === 1 ? true : undefined, 5_000, 'fresh split trigger did not dispatch');
      } catch (error) {
        throw new Error(`fresh split trigger did not dispatch: ${endpoint.requests.join(', ')}; complete=${existsSync(completePath)}; chunks=${JSON.stringify(observedChunks.slice(-4))}; stderr=${JSON.stringify(observedStderr)}`, { cause: error });
      }
      await workerStarted;
      assert.equal(workerDispatches, 1);
      assert.equal(capturedRequest?.guestClockSample?.guestEpochSeconds !== undefined, true);
      await workerFinished;
      assert.ok(observedChunks.some(chunk => chunk.includes('ket, closing')));
      await waitForValue(() => {
        const acquired = existsSync(transportFixture.acquisitionLogFile)
          ? readFileSync(transportFixture.acquisitionLogFile, 'utf8').trim().split('\n').filter(Boolean) : [];
        return acquired.length === 3 && endpoint.completed.length === 3 ? true : undefined;
      }, 10_000, 'fresh transport trigger did not complete one five-source acquisition');
      await writeFile(transportFixture.stateFile, 'shutdown-time-state');
      finishStarted = true;
      const finish = measurement.finish();
      await writeFile(endPath, 'end');
      await finish;
      const observation = JSON.parse(await readFile(join(fixture.root, 'android-transport-observation.json'), 'utf8'));
      assert.equal(observation.acquisitionStatus, 'collected');
      assert.deepEqual(observation.observations.map((entry: any) => entry.name), [
        'host-processes', 'host-memory', 'host-tcp', 'adb-state', 'guest-uptime',
      ]);
      assert.equal(observation.observations.find((entry: any) => entry.name === 'host-processes').stdout, 'trigger-time-state\n');
      assert.deepEqual(existsSync(transportFixture.acquisitionLogFile)
        ? readFileSync(transportFixture.acquisitionLogFile, 'utf8').trim().split('\n').sort() : [],
      ['host-memory', 'host-processes', 'host-tcp']);
      assert.deepEqual(endpoint.completed.sort(), ['adb-state', 'guest-clock', 'guest-uptime']);
      assert.deepEqual(endpoint.requests.slice(0, 2), ['host:transport:emulator-5554', 'shell:date +%s.%N']);
      assert.equal(observation.triggerValidation.absoluteEmissionProven, false);
      assert.equal(observation.triggerValidation.freshnessLimitMs, 2_000);
      assert.equal(observation.observations.length, 5);
      assert.equal(workerDispatches, 1);
      assert.ok(Date.parse(observation.triggeredAt) < Date.parse(observation.endedAt));
    } finally {
      finishStarted = true;
      await writeFile(completePath, 'complete');
      await writeFile(endPath, 'end');
      await measurement.finish().catch(() => undefined);
      await endpoint.stop();
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS transport acquisition cancellation retires workers and sockets', async () => {
    const fixture = await harness.createFixture();
    const endpoint = await createAndroidTransportTestEndpoint(true);
    const transportFixture = await createAndroidTransportFixture(fixture.root, endpoint.port, 'block-processes');
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const endPath = join(fixture.root, 'cancelled-measurement-end');
    const triggerPath = join(fixture.root, 'cancelled-measurement-trigger');
    const observationPath = join(fixture.root, 'android-transport-observation.json');
    let resolveWorkerFinished!: () => void;
    const workerFinished = new Promise<void>(resolve => { resolveWorkerFinished = resolve; });
    let workerFailure: BoundedCommandError | undefined;
    let collectorCode = '';
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
      transportFixture,
      spawnCollector: () => spawnOwnedProcess(process.execPath, ['--no-env-file', '-e', collectorCode], { timeoutMs: 50_000, cleanupReservationMs: 2_000 }),
      boundedCommand: async (binary, args, timeout, options) => {
        if (args.includes('snapshot') && args[args.indexOf('--boundary') + 1] === 'start') {
          await boundedCommand(binary, args, timeout, options);
          await writeFile(triggerPath, 'trigger');
          await waitForValue(() => {
            const acquisitions = existsSync(transportFixture.acquisitionLogFile)
              ? readFileSync(transportFixture.acquisitionLogFile, 'utf8') : '';
            return acquisitions.includes('host-processes') && endpoint.connected >= 3 ? true : undefined;
          }, 5_000, 'transport descendants and isolated ADB sockets did not start during begin');
          await waitForFileValue(transportFixture.commandPidFile, text => Number(text), 1_000);
          await waitForFileValue(transportFixture.descendantPidFile, text => Number(text), 1_000);
          throw new Error('synthetic failed-begin snapshot after transport acquisition started');
        }
        if (!args.includes('--android-transport-observation-trigger')) return boundedCommand(binary, args, timeout, options);
        try { return await boundedCommand(binary, args, timeout, options); }
        catch (error) { if (error instanceof BoundedCommandError) workerFailure = error; throw error; }
        finally { resolveWorkerFinished(); }
      },
    });
    collectorCode = `const fs=require('node:fs');setTimeout(()=>{const epoch=(Date.now()/1000).toFixed(3);process.stdout.write(epoch+' 77 77 I HerdrMeasure: ${measurement.id} START\\n')},30);const timer=setInterval(()=>{if(fs.existsSync(${JSON.stringify(triggerPath)})){fs.rmSync(${JSON.stringify(triggerPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I adbd: timeout expired while flushing socket, closing\\n')}},5);process.on('SIGTERM',()=>{clearInterval(timer);process.exit(0)})`;
    try {
      await measurement.observeGmsStage('after-device-start', new PhaseBudget('gms-transport-cancellation', { timeoutMs: 20_000 }));
      await measurement.observeGmsStage('before-measurement-begin', new PhaseBudget('gms-transport-cancellation', { timeoutMs: 20_000 }));
      await assert.rejects(measurement.begin(), /synthetic failed-begin snapshot after transport acquisition started/u);
      await workerFinished;
      assert.ok(workerFailure instanceof BoundedCommandError);
      assert.equal(workerFailure.result.aborted, true);
      assert.equal(workerFailure.result.childExited, true);
      assert.equal(workerFailure.result.ownedProcessesExited, true);
      assert.equal(workerFailure.result.stdioClosed, true);
      const commandPID = await waitForFileValue(transportFixture.commandPidFile, text => Number(text), 1_000);
      const descendantPID = await waitForFileValue(transportFixture.descendantPidFile, text => Number(text), 1_000);
      assert.equal(await waitForPidAbsent(commandPID, 1_000), true);
      assert.equal(await waitForPidAbsent(descendantPID, 1_000), true);
      await waitForValue(() => endpoint.closed === 3 ? true : undefined, 2_000, 'cancelled ADB observation sockets remained open');
      assert.equal(existsSync(observationPath), false);
      assert.equal(existsSync(transportFixture.lateWriteFile), false);
      await measurement.finalizeGmsObservations();
      const outcome = await measurement.finish();
      assert.ok(outcome.errors.length > 0);
      assert.equal(outcome.assessment?.collection.status, 'UNKNOWN');
      const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
      assert.equal(collector.transportObservationFailure, 'transport observation could not be saved');
      assert.equal(existsSync(observationPath), false);
      assert.equal(existsSync(transportFixture.lateWriteFile), false);
    } finally {
      await writeFile(endPath, 'end');
      await measurement.finish().catch(() => undefined);
      await endpoint.stop();
      for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
      Object.assign(process.env, saved);
    }
  });
  test('GMS transport acquisition failure stays unavailable without late success', async () => {
    for (const mode of ['unavailable', 'nonzero', 'timeout'] as const) {
      const fixture = await harness.createFixture();
      const endpoint = await createAndroidTransportTestEndpoint(false,
        mode === 'unavailable' ? { stateFrame: Buffer.from('FAIL0000') } : {});
      const adbPort = endpoint.port;
      const state = mode === 'nonzero' ? 'nonzero-tcp' : mode === 'timeout' ? 'timeout-memory' : 'trigger-time-state';
      const transportFixture = await createAndroidTransportFixture(fixture.root, adbPort, state);
      const saved = { ...process.env };
      Object.assign(process.env, fixture.environment);
      const triggerPath = join(fixture.root, 'emit-failed-transport-trigger');
      const endPath = join(fixture.root, 'failed-transport-end');
      const observationPath = join(fixture.root, 'android-transport-observation.json');
      let resolveWorkerFinished!: () => void;
      const workerFinished = new Promise<void>(resolve => { resolveWorkerFinished = resolve; });
      let workerDispatches = 0;
      let collectorCode = '';
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'), {
        transportFixture,
        spawnCollector: () => spawnOwnedProcess(process.execPath, ['--no-env-file', '-e', collectorCode], { timeoutMs: 50_000, cleanupReservationMs: 2_000 }),
        boundedCommand: async (binary, args, timeout, options) => {
          if (!args.includes('--android-transport-observation-trigger')) return boundedCommand(binary, args, timeout, options);
          workerDispatches++;
          try { return await boundedCommand(binary, args, timeout, options); }
          finally { resolveWorkerFinished(); }
        },
      });
      collectorCode = `const fs=require('node:fs');setTimeout(()=>{const epoch=(Date.now()/1000).toFixed(3);process.stdout.write(epoch+' 77 77 I HerdrMeasure: ${measurement.id} START\\n')},30);const timer=setInterval(()=>{if(fs.existsSync(${JSON.stringify(triggerPath)})){fs.rmSync(${JSON.stringify(triggerPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I adbd: timeout expired while flushing socket, closing\\n')}if(fs.existsSync(${JSON.stringify(endPath)})){fs.rmSync(${JSON.stringify(endPath)},{force:true});process.stdout.write((Date.now()/1000).toFixed(3)+' 77 77 I HerdrMeasure: ${measurement.id} END\\n')}},5);process.on('SIGTERM',()=>{clearInterval(timer);process.exit(0)})`;
      try {
        const stages = ['after-device-start', 'before-measurement-begin', 'after-setup-url', 'after-browser-install', 'after-initial-installed-app-launch'] as const;

        await measurement.observeGmsStage(stages[0], new PhaseBudget(`gms-transport-${mode}`, { timeoutMs: 20_000 }));
        await measurement.observeGmsStage(stages[1], new PhaseBudget(`gms-transport-${mode}`, { timeoutMs: 20_000 }));
        await measurement.begin();
        for (const stage of stages.slice(2)) await measurement.observeGmsStage(stage, new PhaseBudget(`gms-transport-${mode}`, { timeoutMs: 20_000 }));
        await writeFile(triggerPath, 'trigger');
        await waitForValue(() => workerDispatches === 1 ? true : undefined, 5_000,
          'fresh transport trigger did not dispatch before acquisition failure');
        await workerFinished;
        assert.equal(workerDispatches, 1);
        if (mode === 'timeout') {
          const commandPID = await waitForFileValue(transportFixture.commandPidFile, text => Number(text), 1_000);
          const descendantPID = await waitForFileValue(transportFixture.descendantPidFile, text => Number(text), 1_000);
          assert.equal(await waitForPidAbsent(commandPID, 1_000), true);
          assert.equal(await waitForPidAbsent(descendantPID, 1_000), true);
          assert.equal(existsSync(transportFixture.lateWriteFile), false);
        }
        if (endpoint) await waitForValue(() => endpoint.closed === 3 ? true : undefined, 2_000, 'ADB acquisition sockets did not close');
        await writeFile(transportFixture.stateFile, 'shutdown-time-state');
        const finish = measurement.finish();
        await writeFile(endPath, 'end');
        const outcome = await finish;
        const collector = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
        const bytes = await readFile(observationPath);
        const observation = JSON.parse(bytes.toString('utf8'));
        assert.equal(collector.transportObservationFailure, 'transport acquisition was unavailable or failed');
        assert.equal(observation.acquisitionStatus, 'failed');
        assert.ok(Date.parse(observation.triggeredAt) < Date.parse(observation.endedAt));
        assert.ok(observation.observations.some((entry: any) => entry.unavailable || entry.timedOut || entry.truncated || entry.exitCode !== 0));
        assert.ok(outcome.errors.length > 0);
        assert.equal(outcome.assessment?.collection.status, 'UNKNOWN');
        if (mode === 'unavailable') assert.ok(observation.observations.some((entry: any) => entry.name === 'adb-state' && entry.unavailable));
        if (mode === 'nonzero') assert.equal(observation.observations.find((entry: any) => entry.name === 'host-tcp').exitCode, 7);
        if (mode === 'timeout') assert.equal(observation.observations.find((entry: any) => entry.name === 'host-memory').timedOut, true);
        assert.deepEqual(await readFile(observationPath), bytes);
        assert.equal(workerDispatches, 1);
        assert.equal(existsSync(transportFixture.lateWriteFile), false);
      } finally {
        await writeFile(endPath, 'end');
        await measurement.finish().catch(() => undefined);
        await endpoint?.stop();
        for (const key of Object.keys(process.env)) if (!(key in saved)) delete process.env[key];
        Object.assign(process.env, saved);
      }
    }
  });
  return tests;
}
