import assert from 'node:assert/strict';
import { closeSync, createWriteStream, openSync } from 'node:fs';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer } from 'node:net';
import type { BoundedCommandResult } from '../support/bounded-process';
import { AndroidTransportObservation, transportAdbObservation, transportCommand } from '../support/android-transport';

export const androidTransportTests: Array<[string, () => Promise<void>]> = [
  ['Android transport missing owned loopback server stays absent without a daemon', async () => {
    const server = createServer();
    await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
    const address = server.address();
    assert.ok(address && typeof address !== 'string');
    await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    for (const service of ['get-state', 'uptime'] as const) {
      const result = await transportAdbObservation('owned-test-device', service, address.port);
      assert.equal(result.unavailable, true);
      assert.equal(result.timedOut, false);
    }
    await new Promise(resolve => setTimeout(resolve, 300));
    await new Promise<void>((resolve, reject) => {
      server.once('error', reject);
      server.listen(address.port, '127.0.0.1', resolve);
    });
    await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
  }],
  ['Android transport smart socket uses only read services and drains bounded output', async () => {
    const requests: string[] = [];
    let uptimeReplies = 0;
    const server = createServer(socket => {
      let pending = Buffer.alloc(0);
      socket.on('error', () => undefined);
      socket.on('data', (chunk: Buffer) => {
        pending = Buffer.concat([pending, chunk]);
        if (pending.length < 4) return;
        const length = parseInt(pending.subarray(0, 4).toString(), 16);
        if (pending.length < length + 4) return;
        const request = pending.subarray(4, length + 4).toString();
        pending = pending.subarray(length + 4);
        requests.push(request);
        if (request === 'host-serial:owned-test-device:get-state') socket.end('OKAY0006device');
        else if (request === 'host:transport:owned-test-device') socket.write('OKAY');
        else if (request === 'shell:echo transport-observation; cat /proc/uptime') {
          const response = uptimeReplies++ === 0 ? 'transport-observation\n123.45 678.90\n'
            : uptimeReplies === 2 ? 'malformed\n' : 'x'.repeat(1000000);
          socket.end(`OKAY${response}`);
        }
        else socket.end('FAIL0000');
      });
    });
    await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
    const address = server.address();
    assert.ok(address && typeof address !== 'string');
    try {
      const state = await transportAdbObservation('owned-test-device', 'get-state', address.port);
      assert.equal(state.stdout, 'device');
      assert.equal(state.exitCode, 0);
      const valid = await transportAdbObservation('owned-test-device', 'uptime', address.port);
      assert.equal(valid.exitCode, null);
      assert.equal(valid.unavailable, false);
      assert.equal(valid.truncated, false);
      assert.equal(valid.stdout, 'transport-observation\n123.45 678.90\n');
      const malformed = await transportAdbObservation('owned-test-device', 'uptime', address.port);
      assert.equal(malformed.unavailable, true);
      assert.equal(malformed.truncated, false);
      const oversized = await transportAdbObservation('owned-test-device', 'uptime', address.port);
      assert.equal(oversized.unavailable, true);
      assert.equal(oversized.stdout.length, 16384);
      assert.equal(oversized.truncated, true);
      assert.deepEqual(requests, ['host-serial:owned-test-device:get-state',
        ...Array.from({length: 3}, () => ['host:transport:owned-test-device', 'shell:echo transport-observation; cat /proc/uptime']).flat()]);
    } finally {
      await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    }
  }],
  ['Android transport smart socket timeout and protocol failure are bounded', async () => {
    for (const response of ['', 'FAIL0000', 'OKAYzzzz']) {
      const server = createServer(socket => {
        socket.on('error', () => undefined);
        socket.on('data', () => { if (response) socket.write(response); });
      });
      await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
      const address = server.address();
      assert.ok(address && typeof address !== 'string');
      try {
        const start = Date.now();
        const result = await transportAdbObservation('owned-test-device', 'get-state', address.port, 100);
        assert.equal(result.unavailable, true);
        assert.equal(result.timedOut, response === '');
        assert.ok(Date.now() - start < 2000);
      } finally {
        await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
      }
    }
  }],
  ['Android transport subprocess success and separate streams', async () => {
    const result = await transportCommand(process.execPath, ['-e', 'console.log("ready"); console.error("observed")']);
    assert.equal(result.exitCode, 0);
    assert.equal(result.stdout.trim(), 'ready');
    assert.equal(result.stderr.trim(), 'observed');
    assert.ok(Date.parse(result.endedAt) >= Date.parse(result.startedAt));
    assert.equal(result.unavailable, false);
  }],
  ['Android transport subprocess hung and unavailable are bounded', async () => {
    const root = await mkdtemp(join(tmpdir(), 'mobile-transport-supervisor-'));
    const observerPath = join(root, 'supervisor.jsonl');
    const observerFD = openSync(observerPath, 'wx', 0o600);
    const observerSupervisor = createWriteStream('', { fd: observerFD, autoClose: false });
    let observerClosed = false;
    let boundedResult: BoundedCommandResult | undefined;
    const closeObserver = async () => {
      if (observerClosed) return;
      observerClosed = true;
      await new Promise<void>(resolve => observerSupervisor.end(resolve));
      closeSync(observerFD);
    };
    const start = Date.now();
    try {
      const hung = await transportCommand(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], 100, 16_384, undefined, {
        observerSupervisor,
        onBoundedResult: result => { boundedResult = result; },
      });
      await closeObserver();
      const records = (await readFile(observerPath, 'utf8')).trim().split('\n').filter(Boolean)
        .map(line => JSON.parse(line) as Record<string, unknown>);
      const retirement = [...records].reverse().find(record =>
        record.type === 'supervisor-retirement-ready' || record.type === 'supervisor-retirement-unproved');
      const evidence = retirement?.evidence as Record<string, unknown> | undefined;
      const evidenceFields = ['targetExitObserved', 'targetCloseObserved', 'targetPIDObserved', 'anchorExitObserved',
        'anchorCloseObserved', 'groupAbsent', 'inputClosedObserved', 'stdoutNaturalEnd', 'stdoutCloseObserved',
        'stderrNaturalEnd', 'stderrCloseObserved', 'targetStdoutNaturalEnd', 'targetStdoutCloseObserved',
        'targetStderrNaturalEnd', 'targetStderrCloseObserved', 'managerStdoutNaturalEnd', 'managerStdoutCloseObserved',
        'managerStderrNaturalEnd', 'managerStderrCloseObserved', 'supervisorStdoutFinished', 'supervisorStdoutCloseObserved',
        'supervisorStderrFinished', 'supervisorStderrCloseObserved', 'managerProcessCreated', 'targetDispatchRequested',
        'targetProcessCreated', 'targetNoChildObserved', 'targetExecConfirmed', 'targetUnconfirmedCloseObserved',
        'targetOutputRelayHealthy', 'stopReason'];
      const safeEvidence = evidence && Object.fromEntries(evidenceFields.filter(key => key in evidence).map(key => [key, evidence[key]]));
      const missingRetirementWitnesses: string[] = [];
      const requireTrue = (key: string) => { if (evidence?.[key] !== true) missingRetirementWitnesses.push(key); };
      if (evidence) {
        for (const key of ['anchorExitObserved', 'anchorCloseObserved', 'groupAbsent', 'inputClosedObserved',
          'supervisorStdoutFinished', 'supervisorStdoutCloseObserved', 'supervisorStderrFinished', 'supervisorStderrCloseObserved']) requireTrue(key);
        if (evidence.managerProcessCreated === true || evidence.targetDispatchRequested === true
          || evidence.targetProcessCreated === true || evidence.targetNoChildObserved === true) {
          for (const key of ['managerStdoutNaturalEnd', 'managerStdoutCloseObserved', 'managerStderrNaturalEnd', 'managerStderrCloseObserved']) requireTrue(key);
        }
        if (evidence.targetDispatchRequested === true) {
          if (evidence.targetNoChildObserved === true) {
            if (evidence.targetProcessCreated === true) missingRetirementWitnesses.push('targetProcessCreated=false after targetNoChildObserved');
          } else if (evidence.targetProcessCreated === true && evidence.targetPIDObserved === true) {
            for (const key of ['targetExecConfirmed', 'targetExitObserved', 'targetCloseObserved', 'targetStdoutNaturalEnd',
              'targetStdoutCloseObserved', 'targetStderrNaturalEnd', 'targetStderrCloseObserved', 'targetOutputRelayHealthy']) requireTrue(key);
          } else if (evidence.targetProcessCreated === true) requireTrue('targetUnconfirmedCloseObserved');
          else missingRetirementWitnesses.push('targetNoChildObserved or targetProcessCreated');
        }
      } else missingRetirementWitnesses.push('supervisor-retirement-ready or supervisor-retirement-unproved evidence');
      if (boundedResult && !boundedResult.ownedProcessesExited) missingRetirementWitnesses.push('ownedProcessesExited (supervisor and process-group retirement projection)');
      if (boundedResult && !boundedResult.stdioClosed) missingRetirementWitnesses.push('stdioClosed');
      if (boundedResult) {
        if (evidence?.targetPIDObserved === true && !boundedResult.childExited) missingRetirementWitnesses.push('childExited');
        if (evidence?.targetPIDObserved === true && !boundedResult.targetCloseObserved) missingRetirementWitnesses.push('targetCloseObserved');
        for (const key of ['callerStdoutNaturalEnd', 'callerStdoutCloseObserved', 'callerStderrNaturalEnd', 'callerStderrCloseObserved'] as const) {
          if (boundedResult[key] !== true) missingRetirementWitnesses.push(key);
        }
      }
      const eventTypes = new Set(['supervisor-runtime-started', 'supervisor-deadlines-armed', 'supervisor-progress',
        'supervisor-stopping', 'supervisor-execution-deadline', 'supervisor-hard-deadline', 'supervisor-target-process-started',
        'supervisor-target-process-unconfirmed', 'supervisor-target-no-child', 'supervisor-target-started',
        'supervisor-target-exit', 'supervisor-target-close', 'supervisor-retirement-ready', 'supervisor-retirement-unproved']);
      const eventFields = ['type', 'clockNs', 'executionDeadlineNs', 'hardDeadlineNs', 'stage', 'reason', 'targetPID',
        'targetProcessPID', 'groupID', 'anchorPID', 'targetSpawned', 'targetExecConfirmed', 'targetExit', 'targetClose',
        'anchorClosed', 'groupAbsent', 'inputClosed', 'evidence'];
      const supervisorEvents = records.filter(record => eventTypes.has(String(record.type))).map(record => Object.fromEntries(
        eventFields.filter(key => key in record).map(key => [key, key === 'evidence' && record[key] && typeof record[key] === 'object'
          ? Object.fromEntries(evidenceFields.filter(evidenceKey => evidenceKey in (record[key] as Record<string, unknown>))
            .map(evidenceKey => [evidenceKey, (record[key] as Record<string, unknown>)[evidenceKey]]))
          : record[key]])));
      const details = JSON.stringify({ transport: { timedOut: hung.timedOut, unavailable: hung.unavailable,
        durationMs: Date.now() - start }, bounded: boundedResult ? {
        timedOut: boundedResult.timedOut, aborted: boundedResult.aborted, outputLimitExceeded: boundedResult.outputLimitExceeded,
        durationMs: boundedResult.durationMs, targetPID: boundedResult.targetPID ?? null, targetCloseObserved: boundedResult.targetCloseObserved,
        childExited: boundedResult.childExited, ownedProcessesExited: boundedResult.ownedProcessesExited, stdioClosed: boundedResult.stdioClosed,
        targetStdoutNaturalEnd: boundedResult.targetStdoutNaturalEnd, targetStdoutCloseObserved: boundedResult.targetStdoutCloseObserved,
        targetStderrNaturalEnd: boundedResult.targetStderrNaturalEnd, targetStderrCloseObserved: boundedResult.targetStderrCloseObserved,
      } : undefined, retirementEvidence: safeEvidence, missingRetirementWitnesses, observerSupervisor: supervisorEvents });
      assert.equal(hung.timedOut, true, details);
      assert.ok(Date.now() - start < 2000, details);
      const absent = await transportCommand('/nonexistent/mobile-transport-command', []);
      assert.equal(absent.unavailable, true);
      assert.equal(absent.exitCode, 127);
    } finally {
      await closeObserver();
      await rm(root, { recursive: true, force: true });
    }
  }],
  ['Android transport drains over-bound output without exposing credentials or ADB tracing', async () => {
    const result = await transportCommand(process.execPath, ['-e', 'console.log("Authorization: Bearer secret-value"); console.error(process.env.ADB_TRACE || "trace-off"); console.log("x".repeat(1000000))'], 1500, 512);
    assert.equal(Number.isInteger(result.exitCode), true);
    assert.equal(result.unavailable, true);
    assert.equal(result.truncated, true);
    assert.ok(result.stdout.length <= 512);
    assert.ok(!result.stdout.includes('secret-value'));
    const previousTrace = process.env.ADB_TRACE;
    process.env.ADB_TRACE = 'trace-leak-sentinel';
    try {
      const underLimit = await transportCommand(process.execPath, ['-e', 'console.error(process.env.ADB_TRACE || "trace-off")'], 1500, 512);
      assert.equal(underLimit.exitCode, 0);
      assert.equal(underLimit.unavailable, false);
      assert.equal(underLimit.stderr.trim(), 'trace-off');
    } finally {
      if (previousTrace === undefined) delete process.env.ADB_TRACE;
      else process.env.ADB_TRACE = previousTrace;
    }
  }],
  ['Android transport split flush signal triggers once with safe commands and no driver work', async () => {
    const root = await mkdtemp(join(tmpdir(), 'mobile-transport-'));
    const calls: Array<{ file: string; args: string[] }> = [];
    try {
      const run = async (file: string, args: string[]) => {
        calls.push({ file, args });
        return { startedAt: 'start', endedAt: 'end', stdout: '', stderr: '', exitCode: 0, signal: null, timedOut: false, truncated: false, unavailable: false };
      };
      const observation = new AndroidTransportObservation('emulator-5554', root, run,
        async (serial, service) => run('adb-socket', [serial, service]));
      observation.observe(Buffer.from('123.456 123 456 I unrelated: timeout expired while flushing socket, closing\n'));
      await observation.finish();
      assert.equal(calls.length, 0);
      observation.observe(Buffer.from('123.456 123 456 I adbd    : timeout expired while flushing soc'));
      observation.observe(Buffer.from('ket, closing\n'));
      observation.observe(Buffer.from('124.456 123 456 I adbd: timeout expired while flushing socket, closing\n'));
      await observation.finish();
      assert.equal(calls.length, 5);
      assert.deepEqual(calls.filter(call => call.file === 'adb-socket').map(call => call.args), [
        ['emulator-5554', 'get-state'],
        ['emulator-5554', 'uptime'],
      ]);
      assert.deepEqual(calls[0], { file: 'ps', args: ['-e', '-o', 'pid=,comm=,stat=,wchan=,rss=,pcpu='] });
      assert.ok(!JSON.stringify(calls).includes('argv'));
      const evidence = JSON.parse(await readFile(join(root, 'android-transport-observation.json'), 'utf8'));
      assert.equal(evidence.trigger, 'guest-adbd-flush-timeout');
      assert.equal(evidence.guestEpochSeconds, '123.456');
      assert.equal(evidence.observations.length, 5);
      assert.ok(BigInt(evidence.hostMonotonicReceipt.completedAtNs) > BigInt(evidence.hostMonotonicReceipt.receivedAtNs));
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  }],
  ['Android transport future trigger is rejected before any acquisition', async () => {
    const root = await mkdtemp(join(tmpdir(), 'mobile-transport-future-'));
    let calls = 0;
    try {
      const run = async () => { calls++; throw new Error('future trigger dispatched'); };
      const observation = new AndroidTransportObservation('emulator-5554', root, run, run);
      await observation.observeTrigger({guestEpochSeconds: '123.456', triggeredAt: new Date(Date.now() + 60_000).toISOString()});
      assert.equal(observation.failure, 'transport trigger timestamp is invalid or in the future');
      assert.equal(calls, 0);
      await assert.rejects(readFile(join(root, 'android-transport-observation.json')), /ENOENT/u);
    } finally {
      await rm(root, {recursive: true, force: true});
    }
  }],
  ['Android transport in-flight wall rollback retains observed monotonic completion', async () => {
    const root = await mkdtemp(join(tmpdir(), 'mobile-transport-rollback-'));
    const originalDate = Date;
    let rewound = false;
    try {
      const run = async () => {
        if (!rewound) {
          rewound = true;
          globalThis.Date = class extends originalDate {
            constructor(...args: any[]) { super(args.length ? args[0] : originalDate.now() - 60_000); }
            static now() { return originalDate.now() - 60_000; }
          } as typeof Date;
        }
        return {startedAt: 'start', endedAt: 'end', stdout: '', stderr: '', exitCode: 0, signal: null, timedOut: false, truncated: false, unavailable: false};
      };
      const observation = new AndroidTransportObservation('emulator-5554', root, run, run);
      const triggeredAt = new originalDate(originalDate.now() - 1).toISOString();
      await observation.observeTrigger({guestEpochSeconds: '123.456', triggeredAt});
      assert.equal(observation.failure, undefined);
      const evidence = JSON.parse(await readFile(join(root, 'android-transport-observation.json'), 'utf8'));
      assert.equal(evidence.acquisitionStatus, 'collected');
      assert.ok(originalDate.parse(evidence.endedAt) < originalDate.parse(triggeredAt));
      assert.ok(BigInt(evidence.hostMonotonicReceipt.completedAtNs) > BigInt(evidence.hostMonotonicReceipt.receivedAtNs));
    } finally {
      globalThis.Date = originalDate;
      await rm(root, {recursive: true, force: true});
    }
  }],
  ['Android transport diagnostic failure cannot replace original error', async () => {
    const original = new Error('original fatal');
    const root = await mkdtemp(join(tmpdir(), 'mobile-transport-failure-'));
    const output = join(root, 'not-a-directory');
    await writeFile(output, '');
    const observation = new AndroidTransportObservation('emulator-5554', output,
      async () => { throw new Error('unavailable'); }, async () => { throw new Error('unavailable'); });
    let caught: unknown;
    try {
      try {
        observation.observe(Buffer.from('123.456 123 456 I adbd: timeout expired while flushing socket, closing\n'));
        throw original;
      } finally {
        await observation.finish();
      }
    } catch (error) {
      caught = error;
    }
    try {
      assert.equal(caught, original);
      assert.equal(observation.failure, 'transport observation could not be saved');
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  }],
];
