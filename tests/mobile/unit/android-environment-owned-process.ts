import { createHash } from 'node:crypto';
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { constants as osConstants, tmpdir } from 'node:os';
import { closeSync, createWriteStream, existsSync, openSync, readFileSync } from 'node:fs';
import { appendFile, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import type { Readable } from 'node:stream';
import { repositoryPath, repositoryRoot } from '../support/paths';
import { BoundedCommandError, boundedCommand } from '../support/bounded-process';
import { callerDeadlineToRuntime, callerDurationToRuntime, monotonicNowNs, processGroupAbsent, spawnOwnedProcess, type OwnedProcessHandle, type RetirementEvidence } from '../support/owned-process';
import { admitNonblockingDescriptor, createOwnedGroupPollOwner, createSupervisorObservationWriter, type GroupPollPhase } from '../support/owned-process-runtime';
import { withAndroidRetainedLifecycleFixture } from './android-retained-launch';
import assert from 'node:assert/strict';
import { androidCliTestRegistry, type Harness, type Test, groupPollTestTimers, createBoundedSupervisorObserver, boundedCommandFailureDetails, installFakeAdbLifetimeBackstop, verifyFakeAdbLifetimeBackstop, type FakeAdbDiagnostic, type DarwinProcessExitStatus, type DarwinProcessExitObserver, type LifetimeCaller, sanitizedObserverTail, startLifetimeCaller, processGroupExists, waitForGroupAbsent, waitForPidAbsent, lifetimeProcessTable, lifetimeLiveGroupMembers, lifetimeSupervisor, lifetimeOwnedGroup, waitForValue, waitForFileValue, terminateLifetimeCaller, waitForSupervisorHardDeadlineNs, assertCallerLossRetired, verifyNodeSupervisorBunAnchorStartup, pidExists, observeDarwinProcessExit, verifyDarwinProcessExitStatusControls, verifyObserverDescriptorOwnership, verifyClosedTargetInput, observeRuntimeControl } from './android-environment';

export function androidEnvironmentOwnedProcessTests(harness: Harness): Test[] {
  const { tests, test } = androidCliTestRegistry();
  const darwinProcessExitTest = (name: string, body: () => Promise<void>) => {
    if (process.platform === 'darwin') test(name, body);
  };
  test('registers independent supervisor exit-status witness cases only where Darwin EVFILT_PROC NOTE_EXITSTATUS exists', async () => {
    const registered = new Set(tests.map(([name]) => name));
    for (const name of [
      'owned supervisor retires before its hard deadline with undrained, draining and closed optional observations',
      'HOST lease parent loss retires generic command groups',
      'HOST lease initialization and release interruption stay owned',
      'HOST lease default and injected collectors retire on caller loss',
    ]) assert.equal(registered.has(`Android production CLI ${name}`), process.platform === 'darwin', name);
  });
  test('owned supervisor observations queue short writes and backpressure, bound loss and close failed sinks without waiting', async () => {
    const scriptedSink = (script: Array<number | string>) => {
      const sink = { output: [] as Buffer[], calls: 0, closes: 0, retries: [] as Array<() => void>, cancelled: 0 };
      const writer = createSupervisorObservationWriter({
        write(bytes, offset, length) {
          sink.calls++;
          const next = script.shift();
          if (typeof next === 'string') throw Object.assign(new Error('scripted observer write'), { code: next });
          const written = next === undefined ? length : next;
          if (Number.isSafeInteger(written) && written > 0 && written <= length) sink.output.push(Buffer.from(bytes.subarray(offset, offset + written)));
          return written;
        },
        close: () => { sink.closes++; },
      }, retry => {
        sink.retries.push(retry);
        return () => { sink.cancelled++; };
      });
      const records = () => Buffer.concat(sink.output).toString('utf8').split('\n').slice(0, -1)
        .map(line => JSON.parse(line) as Record<string, unknown>);
      return { sink, writer, records };
    };

    const pressured = scriptedSink([7, 'EAGAIN', 'EAGAIN']);
    assert.equal(pressured.writer.record({ type: 'first' }), true);
    assert.equal(pressured.sink.calls, 2);
    assert.equal(pressured.sink.retries.length, 1);
    assert.equal(pressured.writer.record({ type: 'second' }), true);
    assert.equal(pressured.sink.calls, 3);
    assert.equal(pressured.sink.retries.length, 1);
    assert.equal(Buffer.concat(pressured.sink.output).length, 7);
    assert.ok(pressured.writer.snapshot().queuedBytes > 0);
    pressured.sink.retries.shift()!();
    assert.equal(pressured.sink.calls, 5);
    assert.equal(pressured.sink.retries.length, 0);
    assert.equal(pressured.writer.snapshot().queuedBytes, 0);
    pressured.writer.close();
    pressured.writer.close();
    assert.equal(pressured.writer.record({ type: 'after-close' }), false);
    assert.equal(pressured.sink.closes, 1);
    const pressuredRecords = pressured.records();
    assert.deepEqual(pressuredRecords.map(record => record.type), ['first', 'second', 'supervisor-observation-ended']);
    assert.deepEqual(pressuredRecords.map(record => record.observerDelivery ?? record.delivery), ['best-effort', 'best-effort', 'best-effort']);
    assert.equal(pressuredRecords[2]!.complete, true);
    assert.equal(pressuredRecords[2]!.droppedRecords, 0);

    for (const wouldBlock of ['EAGAIN', 'EWOULDBLOCK', 'EINTR', 0]) {
      const blocked = scriptedSink([wouldBlock, wouldBlock]);
      assert.equal(blocked.writer.record({ type: 'blocked' }), true);
      assert.equal(blocked.sink.retries.length, 1);
      blocked.writer.close();
      assert.equal(blocked.sink.calls, 2);
      assert.equal(blocked.sink.closes, 1);
      assert.equal(blocked.sink.cancelled, 1);
      assert.equal(blocked.writer.snapshot().accepting, false);
      assert.equal(blocked.writer.snapshot().sinkClosed, true);
      assert.equal(blocked.writer.snapshot().queuedBytes, 0);
      assert.equal(blocked.writer.snapshot().droppedRecords, 2);
      assert.equal(blocked.writer.record({ type: 'after-close' }), false);
      blocked.sink.retries.shift()!();
      blocked.writer.close();
      assert.equal(blocked.sink.calls, 2);
      assert.equal(blocked.sink.closes, 1);
      assert.equal(blocked.sink.retries.length, 0);
      assert.deepEqual(blocked.records(), []);
    }

    const partial = scriptedSink([5, 'EAGAIN', 3, 'EAGAIN']);
    assert.equal(partial.writer.record({ type: 'partial-at-close' }), true);
    assert.equal(partial.sink.calls, 2);
    partial.writer.close();
    assert.equal(partial.sink.calls, 4);
    assert.equal(partial.sink.closes, 1);
    const partialBytes = Buffer.concat(partial.sink.output);
    assert.equal(partialBytes.length, 8);
    assert.equal(partialBytes.includes(0x0a), false);
    assert.deepEqual(partial.records(), []);
    assert.equal(partial.writer.snapshot().droppedRecords, 2);
    for (const retry of partial.sink.retries.splice(0)) retry();
    assert.equal(partial.sink.calls, 4);

    const staleRetry = scriptedSink(['EAGAIN']);
    staleRetry.writer.record({ type: 'queued-before-stale-retry' });
    const superseded = staleRetry.sink.retries.shift()!;
    staleRetry.writer.record({ type: 'drained-without-retry' });
    assert.equal(staleRetry.sink.cancelled, 1);
    assert.equal(staleRetry.sink.calls, 3);
    superseded();
    assert.equal(staleRetry.sink.calls, 3);
    staleRetry.writer.close();
    assert.deepEqual(staleRetry.records().map(record => record.type),
      ['queued-before-stale-retry', 'drained-without-retry', 'supervisor-observation-ended']);
    assert.equal(staleRetry.records()[2]!.complete, true);

    const recovered = scriptedSink(['EAGAIN']);
    recovered.writer.record({ type: 'queued' });
    assert.equal(recovered.sink.retries.length, 1);
    recovered.writer.record({ type: 'drains' });
    assert.equal(recovered.sink.cancelled, 1);
    assert.deepEqual(recovered.records().map(record => record.type), ['queued', 'drains']);

    for (const failure of ['EPIPE', 'EBADF', 'EIO', -1, Number.NaN, 1.5, 1_000_000]) {
      const broken = scriptedSink([failure]);
      assert.equal(broken.writer.record({ type: 'supervisor-retirement-ready' }), false);
      assert.equal(broken.sink.closes, 1);
      assert.equal(broken.writer.record({ type: 'supervisor-teardown-started' }), false);
      broken.writer.close();
      assert.equal(broken.sink.calls, 1);
      assert.equal(broken.sink.closes, 1);
      assert.equal(broken.sink.retries.length, 0);
      assert.equal(broken.writer.snapshot().sinkError, typeof failure === 'string' ? failure : 'other');
      assert.equal(broken.writer.snapshot().droppedRecords, 2);
      assert.equal(broken.writer.snapshot().queuedBytes, 0);
      assert.deepEqual(broken.records(), []);
    }

    const closeFailure = createSupervisorObservationWriter({
      write: (_bytes, _offset, length) => length,
      close: () => { throw Object.assign(new Error('scripted observer close'), { code: 'EBADF' }); },
    }, () => () => undefined);
    assert.equal(closeFailure.record({ type: 'last' }), true);
    assert.doesNotThrow(() => closeFailure.close());
    assert.equal(closeFailure.snapshot().sinkError, 'EBADF');
    assert.equal(closeFailure.snapshot().sinkClosed, true);

    const limited = scriptedSink([]);
    assert.equal(limited.writer.record({ type: 'oversized', detail: 'x'.repeat(16 * 1024) }), false);
    let accepted = 0;
    for (let index = 0; index < 20; index++) if (limited.writer.record({ type: 'bounded', detail: 'x'.repeat(8 * 1024) })) accepted++;
    limited.writer.close();
    const limitedRecords = limited.records();
    assert.ok(accepted >= 14 && accepted < 20, String(accepted));
    assert.equal(limitedRecords.length, accepted + 1);
    assert.ok(Buffer.concat(limited.sink.output).length <= 128 * 1024);
    assert.equal(limitedRecords[0]!.observerDroppedRecords, 1);
    assert.equal(limitedRecords.at(-1)!.type, 'supervisor-observation-ended');
    assert.equal(limitedRecords.at(-1)!.complete, false);
    assert.equal(limitedRecords.at(-1)!.droppedRecords, 1 + 20 - accepted);
  });
  test('owned supervisor observation sockets require verified nonblocking status flags before any optional write', async () => {
    const statusDouble = (initial: number, behavior: { readFails?: boolean; writeFails?: boolean; extraFlags?: number; ignoreWrite?: boolean } = {}) => {
      const state = { flags: initial, calls: [] as Array<[number, number, number]> };
      const fcntl = (fd: number, command: number, argument: number) => {
        state.calls.push([fd, command, argument]);
        if (command === 3) return behavior.readFails ? -1 : state.flags;
        assert.equal(command, 4);
        if (behavior.writeFails) return -1;
        if (!behavior.ignoreWrite) state.flags = argument | (state.calls.filter(call => call[1] === 4).length === 1 ? behavior.extraFlags ?? 0 : 0);
        return 0;
      };
      return { state, fcntl };
    };
    const preserved = statusDouble(0x0a);
    assert.deepEqual(admitNonblockingDescriptor(9, preserved.fcntl), { admitted: true, before: 0x0a, after: 0x0e });
    assert.deepEqual(preserved.state.calls, [[9, 3, 0], [9, 4, 0x0e], [9, 3, 0]]);
    assert.equal(preserved.state.flags, 0x0e);

    const already = statusDouble(0x06);
    assert.deepEqual(admitNonblockingDescriptor(9, already.fcntl), { admitted: true, before: 0x06, after: 0x06 });
    assert.deepEqual(already.state.calls, [[9, 3, 0], [9, 3, 0]]);

    const unreadable = statusDouble(0x02, { readFails: true });
    assert.deepEqual(admitNonblockingDescriptor(9, unreadable.fcntl), { admitted: false, reason: 'status-read-failed' });
    assert.deepEqual(unreadable.state.calls, [[9, 3, 0]]);

    const unwritable = statusDouble(0x02, { writeFails: true });
    assert.deepEqual(admitNonblockingDescriptor(9, unwritable.fcntl), { admitted: false, reason: 'status-write-failed', before: 0x02 });
    assert.equal(unwritable.state.flags, 0x02);

    const altered = statusDouble(0x02, { extraFlags: 0x40 });
    assert.deepEqual(admitNonblockingDescriptor(9, altered.fcntl), { admitted: false, reason: 'status-readback-mismatch', before: 0x02, after: 0x46 });
    assert.deepEqual(altered.state.calls, [[9, 3, 0], [9, 4, 0x06], [9, 3, 0], [9, 4, 0x02]]);
    assert.equal(altered.state.flags, 0x02);

    const ignored = statusDouble(0x02, { ignoreWrite: true });
    assert.deepEqual(admitNonblockingDescriptor(9, ignored.fcntl), { admitted: false, reason: 'status-readback-mismatch', before: 0x02, after: 0x02 });
    assert.deepEqual(ignored.state.calls.at(-1), [9, 4, 0x02]);
  });
  darwinProcessExitTest('owned supervisor retires before its hard deadline with undrained, draining and closed optional observations', async () => {
    const fixture = await harness.createFixture();
    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    const requests = 32;
    const targetCode = `process.on('SIGTERM',()=>{});setTimeout(async()=>{let bytes=0;for await(const chunk of process.stdin)bytes+=chunk.length;process.stdout.write('input-ready '+bytes+'\\n')},100);setInterval(()=>{},1000)`;
    for (const [runtime, mode, observation] of [['bun', 'caller-loss', 'paused'], ['bun', 'timeout', 'paused'],
      ['bun', 'timeout', 'draining'], ['bun', 'timeout', 'closed'], ['node', 'timeout', 'paused']] as const) {
      const source = `const {closeSync,createWriteStream}=await import('node:fs');const {spawnOwnedProcess,monotonicNowNs}=await import(${JSON.stringify(repositoryPath('tests/mobile/support/owned-process.ts'))});const publish=value=>process.stdout.write(JSON.stringify(value)+'\\n');let releaseBarrier;const barrierReleased=new Promise(resolve=>{releaseBarrier=resolve});const onRelease=()=>releaseBarrier('signal');process.on('SIGUSR2',onRelease);const supervisorObserver=createWriteStream('',{fd:5,autoClose:false});const owned=spawnOwnedProcess('node',['-e',${JSON.stringify(targetCode)}],{timeoutMs:5000,cleanupReservationMs:1000,input:Buffer.alloc(262144),observerStdout:createWriteStream('',{fd:3,autoClose:false}),observerStderr:createWriteStream('',{fd:4,autoClose:false}),observerSupervisor:supervisorObserver});owned.failure.then(()=>{});publish({event:'dispatched',supervisorPID:owned.supervisor.pid,hardDeadlineNs:owned.supervisor.spawnargs.at(-1)});let resolveInput;const inputReady=new Promise(resolve=>{resolveInput=resolve});let output='';owned.stdout.on('data',chunk=>{output+=String(chunk);if(output.includes('input-ready 262144\\n'))resolveInput()});owned.stderr.resume();await owned.ready;let handoff;try{closeSync(5);handoff={closed:true,atNs:monotonicNowNs().toString(),wrapperBytesWritten:supervisorObserver.bytesWritten??null,wrapperWritableLength:supervisorObserver.writableLength??null}}catch(error){handoff={closed:false,code:typeof error?.code==='string'?error.code:'other'}}publish({event:'observer-handoff',...handoff});owned.start();const binding=await owned.started;await inputReady;publish({event:'armed',groupID:binding.groupID});await new Promise(resolve=>setTimeout(resolve,300));await new Promise(resolve=>owned.supervisor.stdio[3].write((JSON.stringify({type:'stop',reason:'timeout'})+'\\n').repeat(${requests}),resolve));publish({event:'pressure',requests:${requests}});if(${JSON.stringify(mode)}==='caller-loss')process.kill(process.pid,'SIGTERM');const exit=await owned.targetExit;const exitNs=monotonicNowNs().toString();const retirement=await owned.retire('observer-timeout');publish({event:'retired',exitSignal:exit?.signal,exitNs,groupAbsent:retirement.groupAbsent,anchorClosed:retirement.anchorCloseObserved,inputClosed:retirement.inputClosedObserved});let barrierTimer;const outcome=await Promise.race([barrierReleased,new Promise(resolve=>{barrierTimer=setTimeout(()=>resolve('timeout'),2000)})]);clearTimeout(barrierTimer);process.off('SIGUSR2',onRelease);publish({event:'barrier-released',outcome,atNs:monotonicNowNs().toString()});if(outcome!=='signal')process.exitCode=3;`;
      const caller = startLifetimeCaller(source, fixture.environment, runtime === 'bun' ? process.execPath : nodePath,
        runtime === 'bun' ? ['--no-env-file', '-e', source] : ['--experimental-strip-types', '--input-type=module', '-e', source], observation);
      let statusObserver: DarwinProcessExitObserver | undefined;
      let supervisorPID: number | undefined;
      let groupID: number | undefined;
      let originalHardDeadlineNs: bigint | undefined;
      let supervisorExitObservedAtNs: bigint | undefined;
      const failureSnapshot = (error: unknown) => {
        let processes: unknown;
        try {
          processes = lifetimeProcessTable().filter(record => record.pid === caller.child.pid || record.ppid === caller.child.pid
            || record.pid === supervisorPID || record.ppid === supervisorPID || (groupID !== undefined && record.pgid === groupID))
            .slice(0, 12).map(record => ({ pid: record.pid, ppid: record.ppid, pgid: record.pgid, command: sanitizedObserverTail(record.command) }));
        } catch (processError) {
          processes = `process table unavailable: ${sanitizedObserverTail(String(processError))}`;
        }
        const snapshot = { phase: 'failed', runtime, mode, observation,
          error: sanitizedObserverTail(error instanceof Error ? error.message : String(error)),
          hardDeadlineNs: originalHardDeadlineNs?.toString() ?? null, supervisorExitObservedAtNs: supervisorExitObservedAtNs?.toString() ?? null,
          supervisorPID: supervisorPID ?? null, groupID: groupID ?? null, streams: caller.streamDiagnostics(), processes };
        const text = JSON.stringify(snapshot);
        if (Buffer.byteLength(text) <= 8192) return text;
        const compact = JSON.stringify(snapshot, (key, value) => key === 'tail' || key === 'stderrTail' || key === 'processes' ? undefined : value);
        return compact.slice(0, 8192);
      };
      try {
        const dispatched = await caller.waitFor('dispatched', 2_000);
        supervisorPID = Number(dispatched.supervisorPID);
        assert.match(String(dispatched.hardDeadlineNs), /^\d+$/u);
        const hardDeadlineNs = BigInt(String(dispatched.hardDeadlineNs));
        originalHardDeadlineNs = hardDeadlineNs;
        const remainingMs = () => {
          const remaining = Number((hardDeadlineNs - monotonicNowNs()) / 1_000_000n);
          assert.ok(remaining > 0, 'observer pressure exceeded the original hard deadline');
          return remaining;
        };
        statusObserver = await observeDarwinProcessExit(supervisorPID);
        const handoff = await caller.waitFor('observer-handoff', remainingMs());
        assert.equal(handoff.closed, true, JSON.stringify(handoff));
        assert.equal(handoff.wrapperBytesWritten ?? 0, 0);
        assert.equal(handoff.wrapperWritableLength ?? 0, 0);
        const armed = await caller.waitFor('armed', remainingMs());
        groupID = Number(armed.groupID);
        assert.equal(processGroupExists(groupID), true);
        if (observation === 'draining') {
          assert.equal((await caller.waitFor('pressure', remainingMs())).requests, requests);
          await caller.waitFor('retired', remainingMs());
        } else assert.equal(caller.observerSupervisor(), '');
        const status = statusObserver.wait(remainingMs());
        const exitObservedAtNs = monotonicNowNs();
        caller.resumeSupervisorObserver();
        supervisorExitObservedAtNs = exitObservedAtNs;
        const observerAtResume = (caller.streamDiagnostics().supervisorResume as Array<Record<string, unknown>>)[0];
        if (observation !== 'closed') assert.equal(observerAtResume?.errorCode, null, JSON.stringify(observerAtResume));
        if (observation === 'paused') {
          assert.equal(observerAtResume?.destroyed, false, `undrained observer must still be open when draining starts: ${JSON.stringify(observerAtResume)}`);
          assert.equal(observerAtResume?.readableEnded, false);
        }
        assert.ok(exitObservedAtNs < hardDeadlineNs, 'observer pressure exceeded the original hard deadline');
        assert.equal(status.pid, supervisorPID);
        assert.equal(status.filter, -5);
        assert.equal(status.flags & 0x4000, 0);
        assert.notEqual(status.filterFlags & 0x80000000, 0);
        assert.notEqual(status.filterFlags & 0x04000000, 0);
        assert.equal(status.waitStatus, 0, JSON.stringify({ runtime, mode, observation, status }));
        assert.equal(status.exitCode, 0);
        assert.equal(status.signal, 0);
        assert.equal(await waitForPidAbsent(supervisorPID, remainingMs()), true);
        assert.equal(await waitForGroupAbsent(groupID, remainingMs()), true);
        let liveCallerObserverEOF: Record<string, unknown> | undefined;
        if (mode === 'timeout') {
          const retired = await caller.waitFor('retired', remainingMs());
          assert.equal(retired.groupAbsent, true);
          assert.equal(retired.anchorClosed, true);
          assert.equal(retired.inputClosed, true);
          assert.equal(retired.exitSignal, 'SIGKILL');
          assert.ok(BigInt(String(retired.exitNs)) >= hardDeadlineNs - 1_000_000_000n, 'observer pressure must not kill the target before its execution window');
          if (observation !== 'closed') {
            await waitForValue(() => {
              const current = caller.observerStreams();
              return current.supervisorEnded && current.supervisorClosed ? true : undefined;
            }, remainingMs(), 'supervisor observer did not naturally end and close while the caller stayed alive')
              .catch((error: unknown) => {
                let state: string;
                try { state = failureSnapshot(error); } catch (snapshotError) {
                  state = `unavailable: ${sanitizedObserverTail(String(snapshotError))}`;
                }
                throw new Error(`${error instanceof Error ? error.message : String(error)}; state=${state}`);
              });
            assert.equal(caller.child.exitCode, null, 'caller must still be alive when the supervisor observer reaches EOF');
            assert.equal(caller.child.signalCode, null);
            const supervisorTrace = caller.streamDiagnostics().supervisor as Record<string, unknown>;
            assert.equal(supervisorTrace.errorCode, null);
            assert.equal(typeof supervisorTrace.endedAtNs, 'string');
            liveCallerObserverEOF = { handoffAtNs: handoff.atNs, observerEndedAtNs: supervisorTrace.endedAtNs,
              observerClosedAtNs: supervisorTrace.closedAtNs, observerBytes: supervisorTrace.bytes, callerAliveAtNs: monotonicNowNs().toString() };
          }
          assert.equal(caller.child.kill('SIGUSR2'), true);
        }
        const closed = await caller.waitForClose(mode === 'caller-loss' ? remainingMs() : 2_000);
        assert.equal(closed.signal, mode === 'caller-loss' ? 'SIGTERM' : null);
        if (mode === 'timeout') {
          assert.equal(closed.code, 0, caller.stderr());
          assert.equal((await caller.waitFor('barrier-released', 1_000)).outcome, 'signal');
        }
        const streams = await waitForValue(() => {
          const current = caller.observerStreams();
          return current.stdoutEnded && current.stdoutClosed && current.stderrEnded && current.stderrClosed
            && (observation === 'closed' || (current.supervisorEnded && current.supervisorClosed)) ? current : undefined;
        }, mode === 'caller-loss' ? remainingMs() : 1_000, 'observer pipes did not naturally end and close after actual supervisor exit')
          .catch((error: unknown) => {
            let state: string;
            try { state = failureSnapshot(error); } catch (snapshotError) {
              state = `unavailable: ${sanitizedObserverTail(String(snapshotError))}`;
            }
            throw new Error(`${error instanceof Error ? error.message : String(error)}; state=${state}`);
          });
        assert.equal(caller.stdoutEnded(), true);
        assert.equal(caller.stderrEnded(), true);
        const text = caller.observerSupervisor();
        const lines = text.split('\n').slice(0, -1).filter(Boolean);
        const completeLines = lines.flatMap(line => {
          try { return [JSON.parse(line) as Record<string, unknown>]; } catch { return []; }
        });
        const stoppingRecords = completeLines.filter(record => record.type === 'supervisor-stopping').length;
        const ended = completeLines.find(record => record.type === 'supervisor-observation-ended');
        assert.ok(completeLines.every(record => record.observerDelivery === 'best-effort' || record.delivery === 'best-effort'));
        if (ended) assert.equal(ended.complete, ended.droppedRecords === 0);
        if (observation !== 'closed') {
          assert.equal(completeLines[0]?.type, 'supervisor-runtime-started');
          assert.equal(completeLines[0]?.observerSink, runtime === 'bun' ? 'darwin-nonblocking-socket' : 'node-nonblocking-stream');
        }
        if (observation === 'draining') {
          assert.equal(completeLines.length, lines.length);
          assert.ok(Buffer.byteLength(text) > 8192);
          assert.ok(stoppingRecords >= requests, String(stoppingRecords));
          assert.equal(completeLines.at(-1), ended);
          assert.equal(ended?.complete, true);
          assert.ok(completeLines.some(record => record.type === 'supervisor-execution-deadline'));
        } else if (observation === 'paused') {
          assert.ok(completeLines.length > 0);
          assert.ok(stoppingRecords < requests, `undrained observer must withhold offered pressure: ${stoppingRecords}`);
          assert.equal(ended, undefined);
        } else assert.equal(text, '');
        await appendFile(join(fixture.root, 'optional-observer-retirement.jsonl'), `${JSON.stringify({ runtime, mode, observation,
          supervisorPID, groupID, status, hardDeadlineNs: hardDeadlineNs.toString(), exitObservedAtNs: exitObservedAtNs.toString(),
          observerBytes: Buffer.byteLength(text), observerSink: completeLines[0]?.observerSink, stoppingRecords, streams, ended,
          handoff, observerAtResume, liveCallerObserverEOF, callerExitAtNs: (caller.streamDiagnostics().child as { exit?: { atNs?: string } | null }).exit?.atNs ?? null })}\n`);
      } catch (error) {
        await Promise.resolve()
          .then(() => appendFile(join(fixture.root, 'optional-observer-retirement.jsonl'), `${failureSnapshot(error)}\n`))
          .catch(() => undefined);
        throw error;
      } finally {
        caller.resumeSupervisorObserver();
        statusObserver?.close();
        if (caller.child.exitCode === null && caller.child.signalCode === null) await terminateLifetimeCaller(caller, 'SIGTERM');
        if (supervisorPID !== undefined) assert.equal(await waitForPidAbsent(supervisorPID, 8_000), true);
        if (groupID !== undefined) assert.equal(await waitForGroupAbsent(groupID, 8_000), true);
      }
    }
  });
  test('owned process group poll red baseline models duplicate stale callbacks under adversarial scheduling, not as a Bun occurrence', async () => {
    interface LegacyTimer {
      callback: () => void;
      cancelled: boolean;
    }
    const timers: LegacyTimer[] = [];
    let currentTimer: LegacyTimer | undefined;
    let groupPresent = true;
    let absenceRows = 0;
    const schedule = (callback: () => void): LegacyTimer => {
      const timer = { callback, cancelled: false };
      timers.push(timer);
      return timer;
    };
    const cancel = (timer: LegacyTimer) => { timer.cancelled = true; };
    const inspect = () => {
      if (!groupPresent) {
        absenceRows++;
        if (currentTimer) cancel(currentTimer);
        return;
      }
      currentTimer = schedule(inspect);
    };
    inspect();
    const anchorPoll = timers[0]!;
    inspect();
    const managerPoll = timers[1]!;
    assert.notEqual(anchorPoll, managerPoll);
    groupPresent = false;
    anchorPoll.callback();
    assert.equal(managerPoll.cancelled, true);
    managerPoll.callback();
    assert.equal(absenceRows, 2);
  });
  test('owned process group poll owner invalidates canceled callbacks in either callback order', async () => {
    for (const order of ['superseded-first', 'owned-first'] as const) {
      const timers = groupPollTestTimers();
      const probeValues = [true, true, false];
      const samples: Array<{ present: boolean; phase: GroupPollPhase }> = [];
      let probes = 0;
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => {
          probes++;
          const value = probeValues.shift();
          assert.notEqual(value, undefined);
          return value!;
        },
        observe: (present, phase) => samples.push({ present, phase }),
      });
      assert.equal(owner.inspect('before-manager-close'), true);
      const supersededPoll = timers.pending()[0]!;
      assert.equal(owner.inspect('after-manager-close'), true);
      const currentPoll = timers.pending()[0]!;
      assert.equal(supersededPoll.cancelled, true);
      assert.equal(supersededPoll.delayMs, 10);
      assert.equal(currentPoll.delayMs, 10);
      if (order === 'superseded-first') {
        timers.deliver(supersededPoll, true);
        timers.deliver(currentPoll);
      } else {
        timers.deliver(currentPoll);
        timers.deliver(supersededPoll, true);
      }
      assert.equal(probes, 3);
      assert.deepEqual(samples, [
        { present: true, phase: 'before-manager-close' },
        { present: true, phase: 'after-manager-close' },
        { present: false, phase: 'after-manager-close' },
      ]);
      assert.equal(samples.filter(sample => !sample.present && sample.phase === 'after-manager-close').length, 1);
      assert.deepEqual(timers.pending(), []);
      owner.finalize();
    }
  });
  test('owned process group poll owner keeps manager snapshot ahead of fresh post-close probes', async () => {
    for (const scenario of [
      { probeValues: [false, false], snapshot: true, preCloseAbsences: 1 },
      { probeValues: [true, false], snapshot: false, preCloseAbsences: 0 },
    ]) {
      const timers = groupPollTestTimers();
      const probeValues = [...scenario.probeValues];
      const records: Array<{ type: string; phase?: GroupPollPhase; groupAbsent?: boolean }> = [];
      let groupAbsent = false;
      let retirementReady = false;
      let probes = 0;
      const retirementEvidence = {
        anchorClosed: true,
        managerClosed: false,
        inputClosed: false,
        targetSettled: false,
        relayStreamsRetired: false,
      };
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => {
          probes++;
          const value = probeValues.shift();
          assert.notEqual(value, undefined);
          return value!;
        },
        observe: (present, phase, isCurrent) => {
          groupAbsent = !present;
          records.push({ type: present ? 'group-present' : 'supervisor-group-absent', phase });
          if (!present && isCurrent() && groupAbsent && Object.values(retirementEvidence).every(Boolean)) retirementReady = true;
        },
      });
      assert.equal(owner.inspect('before-manager-close'), true);
      const anchorPoll = timers.pending()[0];
      const managerCloseSnapshot = groupAbsent;
      records.push({ type: 'supervisor-manager-close', groupAbsent: managerCloseSnapshot });
      retirementEvidence.managerClosed = true;
      assert.equal(managerCloseSnapshot, scenario.snapshot);
      assert.equal(owner.inspect('after-manager-close'), true);
      assert.equal(groupAbsent, true);
      if (anchorPoll) {
        assert.equal(anchorPoll.cancelled, true);
        timers.deliver(anchorPoll, true);
      }
      assert.equal(probes, 2);
      const managerCloseIndex = records.findIndex(record => record.type === 'supervisor-manager-close');
      const postCloseAbsenceIndex = records.findIndex(record => record.type === 'supervisor-group-absent' && record.phase === 'after-manager-close');
      assert.ok(managerCloseIndex >= 0 && postCloseAbsenceIndex > managerCloseIndex);
      assert.equal(records.filter(record => record.type === 'supervisor-group-absent' && record.phase === 'before-manager-close').length,
        scenario.preCloseAbsences);
      assert.equal(records.filter(record => record.type === 'supervisor-group-absent' && record.phase === 'after-manager-close').length, 1);
      assert.equal(records.filter(record => record.type === 'supervisor-group-absent').length, scenario.preCloseAbsences + 1);
      assert.equal(retirementReady, false);
      assert.deepEqual(timers.pending(), []);
      owner.finalize();
    }
  });
  test('owned process group poll owner rejects reentrant work and remains closed after finalization', async () => {
    {
      const timers = groupPollTestTimers();
      let probes = 0;
      let observations = 0;
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => { probes++; return true; },
        observe: () => { observations++; },
      });
      assert.equal(probes, 0);
      assert.equal(observations, 0);
      assert.deepEqual(timers.pending(), []);
      owner.finalize();
      owner.finalize();
      assert.equal(owner.inspect('before-manager-close'), false);
      assert.equal(probes, 0);
      assert.equal(observations, 0);
    }
    {
      const timers = groupPollTestTimers();
      const samples: Array<{ present: boolean; phase: GroupPollPhase }> = [];
      let probes = 0;
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => {
          probes++;
          if (probes === 1) {
            owner.inspect('after-manager-close');
            return false;
          }
          return true;
        },
        observe: (present, phase) => samples.push({ present, phase }),
      });
      assert.equal(owner.inspect('before-manager-close'), false);
      assert.equal(probes, 2);
      assert.deepEqual(samples, [{ present: true, phase: 'after-manager-close' }]);
      assert.equal(timers.pending().length, 1);
      owner.finalize();
    }
    {
      const timers = groupPollTestTimers();
      const probeValues = [false, false];
      const samples: GroupPollPhase[] = [];
      const notifications: GroupPollPhase[] = [];
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => probeValues.shift()!,
        observe: (present, phase, isCurrent) => {
          samples.push(phase);
          if (!present && phase === 'before-manager-close') owner.inspect('after-manager-close');
          if (!present && isCurrent()) notifications.push(phase);
        },
      });
      assert.equal(owner.inspect('before-manager-close'), false);
      assert.deepEqual(samples, ['before-manager-close', 'after-manager-close']);
      assert.deepEqual(notifications, ['after-manager-close']);
      owner.finalize();
    }
    {
      const timers = groupPollTestTimers();
      const samples: GroupPollPhase[] = [];
      let probes = 0;
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => {
          probes++;
          if (probes === 2) owner.inspect('after-manager-close');
          return true;
        },
        observe: (_present, phase) => samples.push(phase),
      });
      assert.equal(owner.inspect('before-manager-close'), true);
      const callback = timers.pending()[0]!;
      timers.deliver(callback);
      assert.equal(probes, 3);
      assert.deepEqual(samples, ['before-manager-close', 'after-manager-close']);
      assert.equal(timers.pending().length, 1);
      owner.finalize();
    }
    {
      const timers = groupPollTestTimers();
      let probes = 0;
      let notifications = 0;
      const samples: GroupPollPhase[] = [];
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => { probes++; return false; },
        observe: (present, phase, isCurrent) => {
          samples.push(phase);
          owner.finalize();
          if (!present && isCurrent()) notifications++;
        },
      });
      assert.equal(owner.inspect('before-manager-close'), false);
      assert.equal(probes, 1);
      assert.deepEqual(samples, ['before-manager-close']);
      assert.equal(notifications, 0);
      assert.deepEqual(timers.pending(), []);
      assert.equal(owner.inspect('after-manager-close'), false);
      assert.equal(probes, 1);
      assert.deepEqual(samples, ['before-manager-close']);
    }
    for (const finalization of ['normal', 'forced'] as const) {
      const timers = groupPollTestTimers();
      let probes = 0;
      let notifications = 0;
      const samples: GroupPollPhase[] = [];
      const owner = createOwnedGroupPollOwner({
        schedule: timers.schedule,
        cancel: timers.cancel,
        probe: () => { probes++; return true; },
        observe: (present, phase, isCurrent) => {
          samples.push(phase);
          if (!present && isCurrent()) notifications++;
        },
      });
      assert.equal(owner.inspect('before-manager-close'), true);
      const staleCallback = timers.pending()[0]!;
      owner.finalize();
      owner.finalize();
      timers.deliver(staleCallback, true);
      assert.equal(owner.inspect('after-manager-close'), false);
      assert.equal(probes, 1, finalization);
      assert.deepEqual(samples, ['before-manager-close'], finalization);
      assert.equal(notifications, 0, finalization);
      assert.deepEqual(timers.pending(), [], finalization);
    }
  });
  test('owned process-group signals require a live matching group and session member', async () => {
    const source = await readFile(repositoryPath('tests/mobile/support/owned-process-runtime.ts'), 'utf8');
    const groupSignals = [...source.matchAll(/process\.kill\(\s*-\s*([^,\n)]+)(?:,\s*([^)\n]+))?\s*\)/gu)]
      .filter(match => match[2]?.trim() !== '0');
    assert.deepEqual(groupSignals.map(match => [match[1]!.trim(), match[2]?.trim()]), [['groupID', 'signal']]);
    const killApis = [...source.matchAll(/\b([A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*)\s*\(/gu)]
      .map(match => match[1]!)
      .filter(name => /(?:^|\.)(?:kill|killpg|pkill|syscall)$/u.test(name))
      .sort();
    assert.deepEqual(killApis, ['process.kill', 'process.kill', 'state.anchor.kill', 'state.anchor.kill']);
    assert.doesNotMatch(source, /\b[A-Za-z_$][\w$]*\s*\[\s*['"](?:kill|killpg|pkill)['"]\s*\]\s*\(/u);
    assert.doesNotMatch(source, /\b(?:exec|execFile|spawn|spawnSync)\s*\(\s*['"`](?:kill|pkill)['"`]/u);
    assert.match(source, /function signalOwnedProcessGroup[\s\S]*?getpgid\(0\) !== groupID[\s\S]*?getsid\(0\) !== sessionID[\s\S]*?process\.kill\(-groupID, signal\)/u);
    assert.match(source, /Number\(message\.groupID\) !== state\.anchorPID \|\| Number\(message\.sessionID\) !== process\.pid/u);
    assert.match(source, /if \(state\.stopping \|\| state\.startupExpired\) \{\s*guardianSendControl\(state, \{ type: 'guardian-configuration-refused'/u);
    assert.match(source, /state\.guardianConfigured \|\| state\.stopping \|\| state\.startupExpired \|\| !state\.pendingAnchorReady/u);
    assert.match(source, /if \(message\.type === 'anchor-stopping'\) \{\s*state\.stopping = true/u);
  });
  test('owned group guardian retires TERM-resistant descendants after its anchor is killed', async () => {
    assert.notEqual(process.platform, 'win32');
    const fixture = await harness.createFixture();
    const environment = { ...fixture.environment, BUN_OPTIONS: '--no-env-file' };
    const runtimePath = repositoryPath('tests/mobile/support/owned-process-runtime.ts');
    let cleanupHardDeadlineNs: bigint | undefined;
    const targetReadyPath = join(fixture.root, 'anchor-loss-target-ready');
    const descendantReadyPath = join(fixture.root, 'anchor-loss-descendant-ready');
    const descendantCode = `const fs=require('node:fs');process.on('SIGTERM',()=>{});fs.writeFileSync(${JSON.stringify(descendantReadyPath)},JSON.stringify({pid:process.pid,ppid:process.ppid}));setInterval(()=>{},1000);`;
    const targetCode = `(async()=>{const fs=require('node:fs');const {spawn}=require('node:child_process');const ffi=await(new Function('return import("bun:ffi")'))();const types=ffi.FFIType;const library=process.platform==='darwin'?'/usr/lib/libSystem.B.dylib':'libc.so.6';const native=ffi.dlopen(library,{setpgid:{args:[types.i32,types.i32],returns:types.i32},getpgid:{args:[types.i32],returns:types.i32},getsid:{args:[types.i32],returns:types.i32}}).symbols;const groupID=Number(process.argv.at(-2));const sessionID=Number(process.argv.at(-1));if(native.setpgid(0,groupID)!==0||native.getpgid(0)!==groupID||native.getsid(0)!==sessionID)throw new Error('target group identity did not match the admitted anchor');process.on('SIGTERM',()=>{});const descendant=spawn(process.execPath,['--no-env-file','-e',${JSON.stringify(descendantCode)}],{stdio:'ignore',env:process.env});process.on('SIGUSR1',()=>{if(descendant.exitCode===null&&descendant.signalCode===null)descendant.kill('SIGKILL');process.exit(0)});await new Promise((resolve,reject)=>{descendant.once('spawn',resolve);descendant.once('error',reject)});fs.writeFileSync(${JSON.stringify(targetReadyPath)},JSON.stringify({pid:process.pid,ppid:process.ppid,groupID:native.getpgid(0),sessionID:native.getsid(0),descendantPID:descendant.pid}));setTimeout(()=>{if(descendant.exitCode===null&&descendant.signalCode===null)descendant.kill('SIGKILL');process.exit(0)},8000);setInterval(()=>{},1000)})().catch(error=>{require('node:fs').writeFileSync(${JSON.stringify(targetReadyPath)},JSON.stringify({error:String(error)}));process.exitCode=1});`;
    let anchor: ChildProcess | undefined;
    let target: ChildProcess | undefined;
    let anchorCloseValue: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    let targetCloseValue: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    let groupID: number | undefined;
    let descendantPID: number | undefined;
    let descendantExitObserver: DarwinProcessExitObserver | undefined;
    try {
      const ffi = await (new Function('return import("bun:ffi")'))();
      const ffiTypes = ffi.FFIType;
      const library = process.platform === 'darwin' ? '/usr/lib/libSystem.B.dylib' : 'libc.so.6';
      const native = ffi.dlopen(library, { getsid: { args: [ffiTypes.i32], returns: ffiTypes.i32 } }).symbols;
      const sessionID = native.getsid(0);
      assert.ok(Number.isSafeInteger(sessionID) && sessionID > 0);
      const startedAtNs = monotonicNowNs();
      const startupDeadlineNs = startedAtNs + 12_000_000_000n;
      const executionDeadlineNs = startedAtNs + 8_000_000_000n;
      const hardDeadlineNs = startedAtNs + 10_000_000_000n;
      cleanupHardDeadlineNs = hardDeadlineNs;
      anchor = spawn(process.execPath, ['--no-env-file', '--experimental-strip-types', runtimePath, '--anchor',
        startupDeadlineNs.toString(), String(process.pid), String(sessionID)], {
        cwd: repositoryRoot, env: environment, stdio: ['ignore', 'ignore', 'pipe', 'pipe'],
      });
      anchor.once('close', (code, signal) => { anchorCloseValue = { code, signal }; });
      let anchorStderr = '';
      anchor.stderr?.on('data', chunk => { anchorStderr = (anchorStderr + String(chunk)).slice(-4_000); });
      const control = observeRuntimeControl(anchor);
      const nativeReady = await control.waitFor('anchor-native-ready', 3_000);
      groupID = Number(nativeReady.groupID);
      const guardianPID = Number(nativeReady.guardianPID);
      assert.equal(Number(nativeReady.anchorPID), anchor.pid);
      assert.equal(Number(nativeReady.spawnParentPID), process.pid);
      assert.equal(Number(nativeReady.sessionID), sessionID);
      assert.ok(Number.isSafeInteger(groupID) && groupID === anchor.pid);
      assert.ok(Number.isSafeInteger(guardianPID) && guardianPID > 0 && guardianPID !== anchor.pid);
      assert.equal(Number(nativeReady.guardianParentPID), anchor.pid);
      assert.equal(Number(nativeReady.guardianGroupID), groupID);
      assert.equal(Number(nativeReady.guardianSessionID), sessionID);
      const guardian = await waitForValue(() => lifetimeProcessTable().find(record => record.pid === guardianPID), 2_000,
        `anchor guardian was not visible in the process table: ${anchorStderr}`);
      assert.equal(guardian.ppid, anchor.pid);
      assert.equal(guardian.pgid, groupID);
      const configuration = { type: 'configure-anchor', executionDeadlineNs: executionDeadlineNs.toString(),
        hardDeadlineNs: hardDeadlineNs.toString(), killDelayMs: 150 };
      control.send(configuration);
      const ready = await control.waitFor('anchor-ready', 2_000);
      assert.equal(Number(ready.guardianPID), guardianPID);
      assert.equal(Number(ready.guardianGroupID), groupID);
      target = spawn(process.execPath, ['--no-env-file', '-e', targetCode, String(groupID), String(sessionID)], {
        cwd: repositoryRoot, env: environment, stdio: ['ignore', 'ignore', 'pipe'],
      });
      target.once('close', (code, signal) => { targetCloseValue = { code, signal }; });
      const targetIdentity = await waitForFileValue(targetReadyPath, text => JSON.parse(text) as Record<string, unknown>, 4_000);
      assert.equal(targetIdentity.error, undefined);
      assert.equal(Number(targetIdentity.pid), target.pid);
      assert.equal(Number(targetIdentity.ppid), process.pid);
      assert.equal(Number(targetIdentity.groupID), groupID);
      assert.equal(Number(targetIdentity.sessionID), sessionID);
      descendantPID = Number(targetIdentity.descendantPID);
      const descendant = await waitForFileValue(descendantReadyPath, text => JSON.parse(text) as Record<string, unknown>, 2_000);
      assert.equal(Number(descendant.pid), descendantPID);
      assert.equal(Number(descendant.ppid), target.pid);
      if (process.platform === 'darwin') descendantExitObserver = await observeDarwinProcessExit(descendantPID);
      assert.ok(lifetimeLiveGroupMembers(groupID).includes(target.pid!));
      assert.ok(lifetimeLiveGroupMembers(groupID).includes(descendantPID));
      assert.equal(anchor.kill('SIGKILL'), true);
      const anchorClosed = await waitForValue(() => anchorCloseValue, 2_000, 'anchor ChildProcess handle did not observe SIGKILL');
      assert.equal(anchorClosed.signal, 'SIGKILL');
      const targetClosed = await waitForValue(() => targetCloseValue, 2_000, 'live guardian did not retire the target after anchor loss');
      assert.equal(targetClosed.signal, 'SIGKILL');
      if (descendantExitObserver) assert.equal(descendantExitObserver.wait(1_000).signal, osConstants.signals.SIGKILL);
      assert.equal(await waitForPidAbsent(descendantPID, 1_000), true);
      assert.equal(await waitForGroupAbsent(groupID, 1_000), true);
      assert.ok(monotonicNowNs() < hardDeadlineNs, 'guardian retirement did not precede the hard deadline');
    } finally {
      try {
        if (anchor && anchor.exitCode === null && anchor.signalCode === null) anchor.kill('SIGKILL');
        if (target && target.exitCode === null && target.signalCode === null) target.kill('SIGUSR1');
        if (groupID !== undefined && cleanupHardDeadlineNs !== undefined) {
          const remainingMs = Math.max(1, Number((cleanupHardDeadlineNs - monotonicNowNs()) / 1_000_000n) + 1_000);
          assert.equal(await waitForGroupAbsent(groupID, Math.min(13_000, remainingMs)), true,
            'anchor-loss fixture group was not retired by its identity-bound deadline');
        }
        if (descendantPID !== undefined) assert.equal(await waitForPidAbsent(descendantPID, 1_000), true);
      } finally {
        descendantExitObserver?.close();
        await rm(fixture.root, { recursive: true, force: true });
      }
    }
  });
  test('owned group termination prevents guardian readiness before configuration', async () => {
    assert.notEqual(process.platform, 'win32');
    const fixture = await harness.createFixture();
    const environment = { ...fixture.environment, BUN_OPTIONS: '--no-env-file' };
    const runtimePath = repositoryPath('tests/mobile/support/owned-process-runtime.ts');
    let cleanupHardDeadlineNs: bigint | undefined;
    let groupID: number | undefined;
    let anchor: ChildProcess | undefined;
    try {
      const ffi = await (new Function('return import("bun:ffi")'))();
      const ffiTypes = ffi.FFIType;
      const library = process.platform === 'darwin' ? '/usr/lib/libSystem.B.dylib' : 'libc.so.6';
      const native = ffi.dlopen(library, { getsid: { args: [ffiTypes.i32], returns: ffiTypes.i32 } }).symbols;
      const sessionID = native.getsid(0);
      const startedAtNs = monotonicNowNs();
      const startupDeadlineNs = startedAtNs + 5_000_000_000n;
      const executionDeadlineNs = startedAtNs + 3_000_000_000n;
      const hardDeadlineNs = startedAtNs + 4_000_000_000n;
      cleanupHardDeadlineNs = hardDeadlineNs;
      anchor = spawn(process.execPath, ['--no-env-file', '--experimental-strip-types', runtimePath, '--anchor',
        startupDeadlineNs.toString(), String(process.pid), String(sessionID)], {
        cwd: repositoryRoot, env: environment, stdio: ['ignore', 'ignore', 'pipe', 'pipe'],
      });
      let anchorStderr = '';
      anchor.stderr?.on('data', chunk => { anchorStderr = (anchorStderr + String(chunk)).slice(-4_000); });
      const control = observeRuntimeControl(anchor);
      const nativeReady = await control.waitFor('anchor-native-ready', 3_000);
      groupID = Number(nativeReady.groupID);
      const guardianPID = Number(nativeReady.guardianPID);
      assert.equal(Number(nativeReady.anchorPID), anchor.pid);
      assert.ok(Number.isSafeInteger(groupID) && groupID === anchor.pid);
      assert.equal(Number(nativeReady.guardianParentPID), anchor.pid);
      assert.equal(Number(nativeReady.guardianGroupID), groupID);
      assert.equal(Number(nativeReady.guardianSessionID), sessionID);
      const guardian = await waitForValue(() => lifetimeProcessTable().find(record => record.pid === guardianPID), 1_000,
        `anchor guardian was not visible before group termination: ${anchorStderr}`);
      assert.equal(guardian.ppid, anchor.pid);
      assert.equal(guardian.pgid, groupID);
      assert.ok(lifetimeLiveGroupMembers(groupID).includes(anchor.pid!));
      assert.ok(lifetimeLiveGroupMembers(groupID).includes(guardianPID));
      assert.equal(anchor.exitCode, null);
      assert.equal(anchor.signalCode, null);
      assert.equal(process.kill(-groupID, 'SIGTERM'), true);
      await control.waitFor('anchor-stopping', 1_000);
      control.send({ type: 'configure-anchor', executionDeadlineNs: executionDeadlineNs.toString(),
        hardDeadlineNs: hardDeadlineNs.toString(), killDelayMs: 100 });
      await control.waitFor('anchor-configuration-refused', 1_000);
      assert.equal(control.records().some(record => record.type === 'anchor-ready'), false);
      const anchorClose = await waitForValue(() => anchor?.signalCode ? { signal: anchor.signalCode } : undefined, 1_000,
        `anchor did not retire its group after guardian readiness refusal: ${anchorStderr}`);
      assert.equal(anchorClose.signal, 'SIGKILL');
      assert.equal(await waitForGroupAbsent(groupID, 1_000), true);
      assert.ok(monotonicNowNs() < hardDeadlineNs);
    } finally {
      try {
        if (anchor && anchor.exitCode === null && anchor.signalCode === null) anchor.kill('SIGKILL');
        if (groupID !== undefined && cleanupHardDeadlineNs !== undefined) {
          const remainingMs = Math.max(1, Number((cleanupHardDeadlineNs - monotonicNowNs()) / 1_000_000n) + 1_000);
          assert.equal(await waitForGroupAbsent(groupID, Math.min(6_000, remainingMs)), true,
            'pre-configuration guardian group was not retired by its identity-bound deadline');
        }
      } finally { await rm(fixture.root, { recursive: true, force: true }); }
    }
  });
  test('owned guardian timeout reason reaches its stopped anchor before process-group retirement', async () => {
    assert.notEqual(process.platform, 'win32');
    const fixture = await harness.createFixture();
    const environment = { ...fixture.environment, BUN_OPTIONS: '--no-env-file' };
    const runtimePath = repositoryPath('tests/mobile/support/owned-process-runtime.ts');
    let anchor: ChildProcess | undefined;
    let anchorStopped = false;
    let groupID: number | undefined;
    let anchorClose: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    try {
      const ffi = await (new Function('return import("bun:ffi")'))();
      const ffiTypes = ffi.FFIType;
      const library = process.platform === 'darwin' ? '/usr/lib/libSystem.B.dylib' : 'libc.so.6';
      const native = ffi.dlopen(library, { getsid: { args: [ffiTypes.i32], returns: ffiTypes.i32 } }).symbols;
      const sessionID = native.getsid(0);
      const startupDeadlineNs = monotonicNowNs() + 5_000_000_000n;
      anchor = spawn(process.execPath, ['--no-env-file', '--experimental-strip-types', runtimePath, '--anchor',
        startupDeadlineNs.toString(), String(process.pid), String(sessionID)], {
        cwd: repositoryRoot, env: environment, stdio: ['ignore', 'ignore', 'pipe', 'pipe'],
      });
      anchor.once('close', (code, signal) => { anchorClose = { code, signal }; });
      const control = observeRuntimeControl(anchor);
      const nativeReady = await control.waitFor('anchor-native-ready', 2_000);
      groupID = Number(nativeReady.groupID);
      assert.equal(groupID, anchor.pid);
      const executionDeadlineNs = monotonicNowNs() + 250_000_000n;
      const hardDeadlineNs = executionDeadlineNs + 2_000_000_000n;
      control.send({ type: 'configure-anchor', executionDeadlineNs: executionDeadlineNs.toString(),
        hardDeadlineNs: hardDeadlineNs.toString(), killDelayMs: 300 });
      const ready = await control.waitFor('anchor-ready', 1_000);
      assert.equal(Number(ready.groupID), groupID);
      assert.equal(process.kill(anchor.pid!, 'SIGSTOP'), true);
      anchorStopped = true;
      const remainingMs = Math.max(0, Math.ceil(Number(executionDeadlineNs - monotonicNowNs()) / 1_000_000) + 60);
      await new Promise(resolve => setTimeout(resolve, remainingMs));
      assert.equal(process.kill(anchor.pid!, 'SIGCONT'), true);
      anchorStopped = false;
      const stopping = await control.waitFor('anchor-stopping', 1_000);
      assert.equal(stopping.reason, 'timeout');
      const closed = await waitForValue(() => anchorClose, 1_000, 'guardian did not retire the timed-out anchor group');
      assert.equal(closed.signal, 'SIGKILL');
      assert.equal(await waitForGroupAbsent(groupID, 500), true);
      assert.ok(monotonicNowNs() < hardDeadlineNs);
    } finally {
      if (anchorStopped && anchor?.pid) anchor.kill('SIGCONT');
      if (groupID !== undefined && processGroupExists(groupID)) {
        try { process.kill(-groupID, 'SIGKILL'); } catch (error) { assert.equal((error as NodeJS.ErrnoException).code, 'ESRCH'); }
        assert.equal(await waitForGroupAbsent(groupID, 1_000), true);
      }
      if (anchor && anchor.exitCode === null && anchor.signalCode === null) anchor.kill('SIGKILL');
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  test('owned guardian refuses an unrelated numeric process-group identity', async () => {
    assert.notEqual(process.platform, 'win32');
    const fixture = await harness.createFixture();
    const environment = { ...fixture.environment, BUN_OPTIONS: '--no-env-file' };
    const runtimePath = repositoryPath('tests/mobile/support/owned-process-runtime.ts');
    const sentinelReadyPath = join(fixture.root, 'unrelated-group-ready');
    const sentinelSignalPath = join(fixture.root, 'unrelated-group-signaled');
    const sentinelCode = `const fs=require('node:fs');process.on('SIGTERM',()=>fs.writeFileSync(${JSON.stringify(sentinelSignalPath)},'SIGTERM'));process.on('SIGHUP',()=>fs.writeFileSync(${JSON.stringify(sentinelSignalPath)},'SIGHUP'));fs.writeFileSync(${JSON.stringify(sentinelReadyPath)},JSON.stringify({pid:process.pid,ppid:process.ppid}));setInterval(()=>{},1000);`;
    let sentinel: ChildProcess | undefined;
    let probe: ChildProcess | undefined;
    let sentinelCloseValue: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    let probeCloseValue: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    try {
      sentinel = spawn(process.execPath, ['--no-env-file', '-e', sentinelCode], {
        cwd: repositoryRoot, env: environment, detached: true, stdio: ['ignore', 'ignore', 'pipe'],
      });
      sentinel.once('close', (code, signal) => { sentinelCloseValue = { code, signal }; });
      await waitForFileValue(sentinelReadyPath, text => JSON.parse(text) as Record<string, unknown>, 2_000);
      assert.ok(sentinel.pid);
      const runtimeDeadline = (monotonicNowNs() + 3_000_000_000n).toString();
      probe = spawn(process.execPath, ['--no-env-file', '--experimental-strip-types', runtimePath, '--anchor-guardian',
        runtimeDeadline, String(sentinel.pid), String(sentinel.pid)], {
        cwd: repositoryRoot, env: environment, stdio: ['ignore', 'ignore', 'pipe', 'pipe'],
      });
      probe.once('close', (code, signal) => { probeCloseValue = { code, signal }; });
      const control = observeRuntimeControl(probe);
      const refused = await control.waitFor('guardian-bootstrap-error', 2_000);
      assert.match(String(refused.message), /identity did not match/u);
      const probeClosed = await waitForValue(() => probeCloseValue, 1_000, 'misbound guardian process did not fail closed');
      assert.equal(probeClosed.code, 1);
      assert.equal(sentinel.exitCode, null);
      assert.equal(sentinel.signalCode, null);
      assert.equal(existsSync(sentinelSignalPath), false);
    } finally {
      try {
        if (probe && probe.exitCode === null && probe.signalCode === null) probe.kill('SIGKILL');
        let killedSentinel = false;
        if (sentinel && sentinel.exitCode === null && sentinel.signalCode === null) {
          sentinel.kill('SIGKILL');
          killedSentinel = true;
        }
        if (sentinel && sentinel.pid) {
          const closed = await waitForValue(() => sentinelCloseValue, 1_000, 'unrelated-group sentinel did not close');
          if (killedSentinel) assert.equal(closed.signal, 'SIGKILL');
          assert.equal(await waitForGroupAbsent(sentinel.pid, 1_000), true);
        }
      } finally { await rm(fixture.root, { recursive: true, force: true }); }
    }
  });
  test('owned manager output writer failure retires the manager without fabricated stream EOF', async () => {
    assert.notEqual(process.platform, 'win32');
    const fixture = await harness.createFixture();
    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    const bunPath = execFileSync('which', ['bun'], { encoding: 'utf8' }).trim();
    const runtimePath = repositoryPath('tests/mobile/support/owned-process-runtime.ts');
    const ownedProcessPath = repositoryPath('tests/mobile/support/owned-process.ts');
    const targetCode = "process.on('SIGTERM',()=>{});process.stdout.write('MANAGER_OUTPUT_READY');setInterval(()=>process.stdout.write('x'.repeat(8192)),5);";
    const controllerSource = `(async () => {
      const { spawn, execFileSync } = await import('node:child_process');
      const { monotonicNowNs } = await import(${JSON.stringify(ownedProcessPath)});
      const controllerPID = process.pid;
      const ffi = await (new Function('return import("bun:ffi")'))();
      const ffiTypes = ffi.FFIType;
      const nativeLibrary = process.platform === 'darwin' ? '/usr/lib/libSystem.B.dylib' : 'libc.so.6';
      const native = ffi.dlopen(nativeLibrary, {
        setsid: { args: [], returns: ffiTypes.i32 },
        getsid: { args: [ffiTypes.i32], returns: ffiTypes.i32 },
      }).symbols;
      if (native.setsid() !== controllerPID) throw new Error('controller could not establish its owned session');
      const nodePath = ${JSON.stringify(nodePath)};
      const bunPath = ${JSON.stringify(bunPath)};
      const runtimePath = ${JSON.stringify(runtimePath)};
      const targetCode = ${JSON.stringify(targetCode)};
      const startedAtNs = monotonicNowNs();
      const hardDeadlineNs = startedAtNs + 700_000_000n;
      const executionDeadlineNs = hardDeadlineNs - 200_000_000n;
      const records = [];
      let controllerError;
      let manager;
      let control;
      let managerOutput;
      let managerPID;
      let anchorPID;
      let groupID;
      let targetPID;
      let managerProcess;
      let anchorProcess;
      let targetProcess;
      let targetStarted;
      let managerOutputPrefix = '';
      let managerOutputBytes = 0;
      let managerStderrPrefix = '';
      let managerOutputNaturalEnd = false;
      let managerOutputEndAtNs;
      let managerOutputCloseObserved = false;
      let managerOutputClosedByController = false;
      let managerOutputCloseRequestedAtNs;
      let managerOutputBytesAtClose;
      let failureObservedAtNs;
      let targetExit;
      let targetExitAtNs;
      let targetClose;
      let targetCloseAtNs;
      let anchorExit;
      let anchorExitAtNs;
      let anchorClose;
      let anchorCloseAtNs;
      let managerExit;
      let managerExitAtNs;
      let managerClose;
      let managerCloseAtNs;
      let managerStderrNaturalEnd = false;
      let managerStderrCloseObserved = false;
      let pendingControl = '';
      const emit = (event, value = {}) => process.stdout.write(JSON.stringify({ event, ...value }) + '\\n');
      const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
      const alive = pid => {
        if (!Number.isSafeInteger(pid) || pid < 1) return false;
        try { process.kill(pid, 0); return true; }
        catch (error) { return error.code !== 'ESRCH'; }
      };
      const groupAlive = id => {
        if (!Number.isSafeInteger(id) || id < 1) return false;
        try { process.kill(-id, 0); return true; }
        catch (error) { return error.code !== 'ESRCH'; }
      };
      const processInfo = pid => {
        const text = execFileSync('ps', ['-o', 'pid=,ppid=,pgid=', '-p', String(pid)], { encoding: 'utf8', timeout: 250 }).trim();
        const fields = text.split(/\\s+/u).map(Number);
        const sid = native.getsid(pid);
        if (fields.length !== 3 || fields.some(value => !Number.isSafeInteger(value)) || !Number.isSafeInteger(sid) || sid < 1) {
          throw new Error('process identity was not visible in ps or the kernel');
        }
        return { pid: fields[0], ppid: fields[1], pgid: fields[2], sid };
      };
      const send = value => {
        if (!control || control.destroyed || !control.writable) throw new Error('manager control pipe closed before the request');
        control.write(JSON.stringify(value) + '\\n');
      };
      const fail = error => { controllerError ||= error instanceof Error ? error.message : String(error); };
      const maybeCloseOutput = () => {
        if (managerOutputClosedByController || !targetStarted || !managerOutputPrefix.startsWith('MANAGER_OUTPUT_READY')) return;
        try {
          managerProcess = processInfo(managerPID);
          anchorProcess = processInfo(anchorPID);
          targetProcess = processInfo(targetPID);
          if (managerProcess.ppid !== controllerPID || managerProcess.pgid !== controllerPID || managerProcess.sid !== controllerPID
            || anchorProcess.ppid !== managerPID || anchorProcess.pgid !== anchorPID || anchorProcess.sid !== controllerPID
            || targetProcess.ppid !== managerPID || targetProcess.pgid !== anchorPID || targetProcess.sid !== controllerPID) {
            throw new Error('manager, anchor, or ready target process identity did not match the owned group');
          }
          managerOutputCloseRequestedAtNs = monotonicNowNs();
          if (managerOutputCloseRequestedAtNs >= executionDeadlineNs) throw new Error('manager output failure began after the original execution deadline');
        } catch (error) {
          fail(error);
          managerOutputCloseRequestedAtNs = monotonicNowNs();
        }
        managerOutputClosedByController = true;
        managerOutputBytesAtClose = managerOutputBytes;
        emit('manager-output-reader-closed', { managerPID, targetPID, groupID, bytes: managerOutputBytesAtClose,
          atNs: managerOutputCloseRequestedAtNs.toString() });
        managerOutput.destroy();
      };
      const handleRecord = record => {
        records.push(record);
        if (record.type === 'anchor-process-started') {
          anchorPID = Number(record.processID);
          emit('anchor-started', { anchorPID, managerPID });
        }
        if (record.type === 'anchor-native-ready') {
          groupID = Number(record.groupID);
          const anchorClockNs = BigInt(record.clockNs);
          const controllerClockNs = monotonicNowNs();
          const translate = deadline => (anchorClockNs + deadline - controllerClockNs).toString();
          send({ type: 'configure-anchor', executionDeadlineNs: translate(executionDeadlineNs),
            hardDeadlineNs: translate(hardDeadlineNs), killDelayMs: 50 });
        }
        if (record.type === 'anchor-ready') send({ type: 'launch-target', binary: nodePath,
          args: ['-e', targetCode], executionDeadlineNs: executionDeadlineNs.toString() });
        if (record.type === 'target-started') {
          targetStarted = record;
          targetPID = Number(record.targetPID);
          maybeCloseOutput();
        }
        if (record.type === 'failure' && failureObservedAtNs === undefined
          && /EPIPE|closed before all output was written/u.test(String(record.message))) failureObservedAtNs = monotonicNowNs();
        if (record.type === 'target-exit') { targetExit = record; targetExitAtNs = monotonicNowNs(); }
        if (record.type === 'target-close') { targetClose = record; targetCloseAtNs = monotonicNowNs(); }
        if (record.type === 'anchor-exit') { anchorExit = record; anchorExitAtNs = monotonicNowNs(); }
        if (record.type === 'anchor-close') { anchorClose = record; anchorCloseAtNs = monotonicNowNs(); }
      };
      const waitUntil = async (read, label, deadlineNs) => {
        while (monotonicNowNs() < deadlineNs) {
          if (controllerError) throw new Error(controllerError);
          const value = read();
          if (value !== undefined) return value;
          await delay(2);
        }
        throw new Error(label + ' exceeded its original monotonic deadline');
      };
      const cleanFailedRun = async () => {
        if (!manager || managerClose) return;
        try { send({ type: 'stop', reason: 'negative-control-test-failure-cleanup' }); } catch {}
        const cleanupDeadlineNs = monotonicNowNs() + 300_000_000n;
        while (!managerClose && monotonicNowNs() < cleanupDeadlineNs) await delay(5);
        if (groupID && groupAlive(groupID)) {
          try { process.kill(-groupID, 'SIGKILL'); } catch {}
        }
        if (!managerClose && managerPID && alive(managerPID)) manager.kill('SIGKILL');
        const finalDeadlineNs = monotonicNowNs() + 500_000_000n;
        while (!managerClose && monotonicNowNs() < finalDeadlineNs) await delay(5);
      };
      try {
        const controllerProcess = processInfo(controllerPID);
        if (controllerProcess.pgid !== controllerPID || controllerProcess.sid !== controllerPID) throw new Error('controller did not own its process group and session');
        emit('controller-started', { controllerPID, controllerProcess, startedAtNs: startedAtNs.toString(),
          executionDeadlineNs: executionDeadlineNs.toString(), hardDeadlineNs: hardDeadlineNs.toString() });
        manager = spawn(bunPath, ['--no-env-file', '--experimental-strip-types', runtimePath, '--anchor-manager',
          hardDeadlineNs.toString(), String(controllerPID)], {
          detached: false, stdio: ['ignore', 'ignore', 'ignore', 'pipe', 'pipe', 'pipe', 'pipe'],
        });
        managerPID = manager.pid;
        if (!managerPID) throw new Error('production anchor manager did not receive a PID');
        control = manager.stdio[3];
        managerOutput = manager.stdio[4];
        const managerError = manager.stdio[5];
        managerOutput.on('data', chunk => {
          const bytes = Buffer.from(chunk);
          managerOutputBytes += bytes.length;
          managerOutputPrefix = (managerOutputPrefix + bytes.toString('utf8')).slice(0, 256);
          maybeCloseOutput();
        });
        managerOutput.once('end', () => { managerOutputNaturalEnd = true; managerOutputEndAtNs = monotonicNowNs(); });
        managerOutput.once('close', () => { managerOutputCloseObserved = true; });
        managerError.on('data', chunk => { managerStderrPrefix = (managerStderrPrefix + String(chunk)).slice(-2_000); });
        managerError.once('end', () => { managerStderrNaturalEnd = true; });
        managerError.once('close', () => { managerStderrCloseObserved = true; });
        control.setEncoding('utf8');
        control.on('data', chunk => {
          pendingControl += chunk;
          if (pendingControl.length > 1_100_000) { fail('manager control record exceeded its bound'); return; }
          for (;;) {
            const end = pendingControl.indexOf('\\n');
            if (end < 0) break;
            const line = pendingControl.slice(0, end);
            pendingControl = pendingControl.slice(end + 1);
            try { handleRecord(JSON.parse(line)); }
            catch (error) { fail(error); }
          }
        });
        control.on('error', error => fail(error));
        manager.once('error', error => fail(error));
        manager.once('exit', (code, signal) => {
          managerExit = { code, signal };
          managerExitAtNs = monotonicNowNs();
        });
        manager.once('close', (code, signal) => {
          managerClose = { code, signal };
          managerCloseAtNs = monotonicNowNs();
        });
        manager.stdio[6].end();
        emit('manager-started', { managerPID });
        await waitUntil(() => anchorPID && groupID && records.some(record => record.type === 'anchor-ready') ? true : undefined,
          'actual anchor readiness', executionDeadlineNs);
        await waitUntil(() => managerOutputClosedByController ? true : undefined, 'actual manager FD4 reader closure', executionDeadlineNs);
        await waitUntil(() => records.some(record => record.type === 'failure' && /EPIPE|closed before all output was written/u.test(String(record.message))),
          'actual manager FD4 writer failure', executionDeadlineNs);
        await waitUntil(() => managerClose, 'actual manager exit and close', hardDeadlineNs);
        await waitUntil(() => targetExit && targetClose && anchorExit && anchorClose
          && records.some(record => record.type === 'manager-teardown-started'), 'actual target, anchor, and manager teardown witnesses', hardDeadlineNs);
        const groupAbsent = await waitUntil(() => !groupAlive(groupID) ? monotonicNowNs() : undefined, 'actual target process-group absence', hardDeadlineNs);
        await waitUntil(() => !alive(targetPID) && !alive(anchorPID) && !alive(managerPID) ? true : undefined,
          'actual target, anchor, and manager PID absence', hardDeadlineNs);
        const supervisorRetirementReady = records.filter(record => record.type === 'supervisor-retirement-ready');
        if (supervisorRetirementReady.length) throw new Error('direct manager controller received an unproved supervisor-retirement-ready record');
        const finishedAtNs = monotonicNowNs();
        const failureRecords = records.filter(record => record.type === 'failure');
        if (!managerOutputCloseObserved || !managerOutputClosedByController
          || (managerOutputNaturalEnd && managerOutputEndAtNs < managerOutputCloseRequestedAtNs)) {
          throw new Error('manager output end/close did not follow the actual OS reader closure');
        }
        if (!failureRecords.some(record => /EPIPE|closed before all output was written/u.test(String(record.message)))) throw new Error('manager FD4 writer failure fact was missing');
        if (failureObservedAtNs === undefined || failureObservedAtNs >= executionDeadlineNs) throw new Error('manager FD4 failure missed the original execution deadline');
        if (finishedAtNs > hardDeadlineNs || BigInt(groupAbsent) > hardDeadlineNs) throw new Error('manager retirement exceeded the original 700ms hard deadline');
        if (targetExit.signal !== 'SIGKILL' || targetClose.signal !== 'SIGKILL' || anchorExit.signal !== 'SIGKILL' || anchorClose.signal !== 'SIGKILL') {
          throw new Error('actual target and anchor group retirement was not witnessed after the output failure');
        }
        if (managerExit.code !== 0 || managerExit.signal !== null || managerClose.code !== 0 || managerClose.signal !== null) {
          throw new Error('actual manager exit and close did not complete normally');
        }
        const report = {
          event: 'manager-output-result', ok: true, controllerPID, startedAtNs: startedAtNs.toString(),
          executionDeadlineNs: executionDeadlineNs.toString(), hardDeadlineNs: hardDeadlineNs.toString(),
          cleanupReservationMs: 200, finishedAtNs: finishedAtNs.toString(), groupAbsentAtNs: String(groupAbsent),
          managerOutputCloseRequestedAtNs: managerOutputCloseRequestedAtNs.toString(),
          managerPID, managerProcess, anchorPID, anchorProcess, groupID, targetPID, targetProcess,
          targetExit, targetExitAtNs: String(targetExitAtNs), targetClose, targetCloseAtNs: String(targetCloseAtNs),
          anchorExit, anchorExitAtNs: String(anchorExitAtNs), anchorClose, anchorCloseAtNs: String(anchorCloseAtNs),
          managerExit, managerExitAtNs: String(managerExitAtNs), managerClose, managerCloseAtNs: String(managerCloseAtNs),
          managerOutputBytes, managerOutputBytesAtClose, managerOutputPrefix, managerOutputClosedByController,
          managerOutputNaturalEnd, managerOutputEndAtNs: managerOutputEndAtNs?.toString(), managerOutputCloseObserved,
          managerStderrNaturalEnd, managerStderrCloseObserved, failureObservedAtNs: String(failureObservedAtNs), failureRecords, supervisorRetirementReadyCount: supervisorRetirementReady.length, records,
        };
        emit('manager-output-result', report);
      } catch (error) {
        await cleanFailedRun();
        emit('manager-output-result', { ok: false, controllerPID, startedAtNs: startedAtNs.toString(),
          executionDeadlineNs: executionDeadlineNs.toString(), hardDeadlineNs: hardDeadlineNs.toString(),
          failure: error instanceof Error ? error.message : String(error), managerPID, managerExit, managerClose,
          managerOutputNaturalEnd, managerOutputEndAtNs: managerOutputEndAtNs?.toString(), managerOutputCloseObserved,
          managerOutputClosedByController, managerOutputPrefix,
          managerOutputBytes, managerStderrPrefix, targetExit, targetClose, anchorExit, anchorClose, anchorPID, groupID, targetPID, records });
        process.exitCode = 1;
      }
    })().catch(error => {
      process.stderr.write(String(error) + '\\n');
      process.exitCode = 1;
    });`;
    const controller = spawn(bunPath, ['--no-env-file', '-e', controllerSource], {
      cwd: repositoryRoot, env: fixture.environment, detached: false, stdio: ['ignore', 'pipe', 'pipe'],
    });
    assert.ok(controller.pid);
    let pending = '';
    let controllerStderr = '';
    let controllerClose: { code: number | null; signal: NodeJS.Signals | null } | undefined;
    const controllerRecords: Array<Record<string, any>> = [];
    const closed = new Promise<void>(resolve => controller.once('close', (code, signal) => {
      controllerClose = { code, signal };
      resolve();
    }));
    controller.stdout!.on('data', chunk => {
      pending += String(chunk);
      for (;;) {
        const end = pending.indexOf('\n');
        if (end < 0) break;
        const line = pending.slice(0, end);
        pending = pending.slice(end + 1);
        try { controllerRecords.push(JSON.parse(line) as Record<string, any>); }
        catch { controllerStderr += `invalid controller record: ${line.slice(0, 200)}\n`; }
      }
    });
    controller.stderr!.on('data', chunk => { controllerStderr = (controllerStderr + String(chunk)).slice(-8_000); });
    let managerPID: number | undefined;
    let anchorGroupID: number | undefined;
    const waitForControllerRecord = async (read: () => Record<string, any> | undefined, timeoutMs: number, message: string) => {
      try { return await waitForValue(read, timeoutMs, message); }
      catch (error) { throw new Error(`${message}: ${String(error)}; stderr=${controllerStderr}; records=${JSON.stringify(controllerRecords)}`, { cause: error }); }
    };
    try {
      const started = await waitForControllerRecord(() => controllerRecords.find(record => record.event === 'controller-started'),
        1_000, 'detached manager controller did not start');
      assert.equal(started.controllerPID, controller.pid);
      assert.equal(started.controllerProcess.pgid, controller.pid);
      assert.equal(started.controllerProcess.sid, controller.pid);
      const managerStarted = await waitForControllerRecord(() => controllerRecords.find(record => record.event === 'manager-started'),
        1_000, 'production manager did not start in its controller session');
      managerPID = Number(managerStarted.managerPID);
      const result = await waitForControllerRecord(() => controllerRecords.find(record => record.event === 'manager-output-result'),
        2_000, 'manager output failure controller did not return its direct lifecycle witnesses');
      const anchorStarted = await waitForValue(() => controllerRecords.find(record => record.event === 'anchor-started'),
        100, 'production manager did not start its actual anchor');
      anchorGroupID = Number(anchorStarted.anchorPID);
      await waitForValue(() => controllerClose, 1_000, 'manager output controller did not close after reporting');
      assert.equal(controllerClose?.code, 0, JSON.stringify({ controllerClose, result, controllerStderr }));
      assert.equal(controllerClose?.signal, null);
      assert.equal(result.ok, true, JSON.stringify({ result, controllerStderr, controllerRecords }));
      assert.equal(result.controllerPID, controller.pid);
      assert.equal(Number(result.managerPID), managerPID);
      assert.equal(Number(result.anchorPID), anchorGroupID);
      assert.equal(Number(result.groupID), anchorGroupID);
      assert.equal(result.cleanupReservationMs, 200);
      const startedAtNs = BigInt(result.startedAtNs);
      const executionDeadlineNs = BigInt(result.executionDeadlineNs);
      const hardDeadlineNs = BigInt(result.hardDeadlineNs);
      assert.equal(hardDeadlineNs - startedAtNs, 700_000_000n);
      assert.equal(executionDeadlineNs, hardDeadlineNs - 200_000_000n);
      assert.ok(BigInt(result.failureObservedAtNs) < executionDeadlineNs);
      assert.ok(BigInt(result.managerCloseAtNs) <= hardDeadlineNs);
      assert.ok(BigInt(result.targetCloseAtNs) <= hardDeadlineNs);
      assert.ok(BigInt(result.anchorCloseAtNs) <= hardDeadlineNs);
      assert.ok(BigInt(result.groupAbsentAtNs) <= hardDeadlineNs);
      assert.ok(BigInt(result.finishedAtNs) <= hardDeadlineNs);
      assert.ok(result.managerOutputBytesAtClose > 0);
      assert.match(result.managerOutputPrefix, /^MANAGER_OUTPUT_READY/u);
      assert.equal(result.managerOutputClosedByController, true);
      if (result.managerOutputNaturalEnd) assert.ok(BigInt(result.managerOutputEndAtNs) >= BigInt(result.managerOutputCloseRequestedAtNs));
      assert.equal(result.managerOutputCloseObserved, true);
      assert.equal(result.managerStderrNaturalEnd, true);
      assert.equal(result.managerStderrCloseObserved, true);
      assert.ok(result.failureRecords.some((record: Record<string, unknown>) => /EPIPE|closed before all output was written/u.test(String(record.message))));
      assert.equal(result.targetExit.signal, 'SIGKILL');
      assert.equal(result.targetClose.signal, 'SIGKILL');
      assert.equal(result.anchorExit.signal, 'SIGKILL');
      assert.equal(result.anchorClose.signal, 'SIGKILL');
      assert.equal(result.managerExit.code, 0);
      assert.equal(result.managerExit.signal, null);
      assert.equal(result.managerClose.code, 0);
      assert.equal(result.managerClose.signal, null);
      assert.equal(result.managerProcess.ppid, controller.pid);
      assert.equal(result.managerProcess.pgid, controller.pid);
      assert.equal(result.managerProcess.sid, controller.pid);
      assert.equal(result.anchorProcess.ppid, managerPID);
      assert.equal(result.anchorProcess.pgid, anchorGroupID);
      assert.equal(result.anchorProcess.sid, controller.pid);
      assert.equal(result.targetProcess.ppid, managerPID);
      assert.equal(result.targetProcess.pgid, anchorGroupID);
      assert.equal(result.targetProcess.sid, controller.pid);
      assert.equal(result.records.some((record: Record<string, unknown>) => record.type === 'manager-teardown-started'), true);
      assert.equal(result.records.some((record: Record<string, unknown>) => record.type === 'supervisor-retirement-ready'), false);
      assert.equal(result.supervisorRetirementReadyCount, 0);
      assert.equal(pidExists(managerPID), false);
      assert.equal(pidExists(result.targetPID), false);
      assert.equal(pidExists(result.anchorPID), false);
      assert.equal(await waitForGroupAbsent(anchorGroupID, 100), true);
      assert.equal(pidExists(controller.pid), false);
      assert.equal(await waitForGroupAbsent(controller.pid, 100), true);
    } finally {
      if (anchorGroupID !== undefined && processGroupExists(anchorGroupID)) {
        try { process.kill(-anchorGroupID, 'SIGKILL'); } catch (error) { assert.equal((error as NodeJS.ErrnoException).code, 'ESRCH'); }
        assert.equal(await waitForGroupAbsent(anchorGroupID, 1_000), true);
      }
      if (controller.exitCode === null && controller.signalCode === null) {
        try { process.kill(-controller.pid!, 'SIGKILL'); } catch (error) { assert.equal((error as NodeJS.ErrnoException).code, 'ESRCH'); }
        if (controller.exitCode === null && controller.signalCode === null) controller.kill('SIGKILL');
        await Promise.race([closed, new Promise(resolve => setTimeout(resolve, 1_000))]);
      }
      if (managerPID !== undefined && pidExists(managerPID)) {
        try { process.kill(managerPID, 'SIGKILL'); } catch (error) { assert.equal((error as NodeJS.ErrnoException).code, 'ESRCH'); }
        assert.equal(await waitForPidAbsent(managerPID, 1_000), true);
      }
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  test('owned control channel teardown releases its pending descriptor read while the peer stays open', async () => {
    assert.notEqual(process.platform, 'win32');
    const source = `const {controlChannel}=await import(${JSON.stringify(repositoryPath('tests/mobile/support/owned-process-runtime.ts'))});const channel=controlChannel(3);channel.on('data',()=>{});channel.on('error',()=>{});setTimeout(()=>channel.destroy(),100);`;
    const child = spawn(process.execPath, ['--no-env-file', '-e', source], { stdio: ['ignore', 'ignore', 'pipe', 'pipe'] });
    let stderr = '';
    child.stderr!.on('data', chunk => { stderr += String(chunk); });
    const peer = child.stdio[3] as Readable;
    peer.resume();
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      const exit = await new Promise<{ code: number | null; signal: NodeJS.Signals | null } | undefined>(resolve => {
        timer = setTimeout(() => resolve(undefined), 3_000);
        child.once('exit', (code, signal) => resolve({ code, signal }));
      });
      assert.deepEqual(exit, { code: 0, signal: null }, stderr);
    } finally {
      clearTimeout(timer);
      if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
      peer.destroy();
    }
  });
  test('owned process retirement keeps target, relay and caller witnesses independent for full output', async () => {
    assert.notEqual(process.platform, 'win32');
    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    const expectedStdout = Buffer.alloc(128_000, 83);
    const expectedStderr = 'source-bound stderr payload\\n';
    const source = `process.stdout.write(Buffer.alloc(${expectedStdout.length},${expectedStdout[0]}));process.stderr.write(${JSON.stringify(expectedStderr)});`;
    const result = await boundedCommand(nodePath, ['-e', source], 5_000, { maxBytes: 140_000, cleanupReservationMs: 1_000 });
    assert.equal(result.stdout, expectedStdout.toString('utf8'));
    assert.equal(result.stderr, expectedStderr);
    assert.equal(result.targetStdoutNaturalEnd, true);
    assert.equal(result.targetStdoutCloseObserved, true);
    assert.equal(result.targetStderrNaturalEnd, true);
    assert.equal(result.targetStderrCloseObserved, true);
    assert.equal(result.managerStdoutNaturalEnd, true);
    assert.equal(result.managerStdoutCloseObserved, true);
    assert.equal(result.managerStderrNaturalEnd, true);
    assert.equal(result.managerStderrCloseObserved, true);
    assert.equal(result.callerStdoutNaturalEnd, true);
    assert.equal(result.callerStdoutCloseObserved, true);
    assert.equal(result.callerStderrNaturalEnd, true);
    assert.equal(result.callerStderrCloseObserved, true);
    assert.equal(result.targetOutputRelayHealthy, true);
    assert.equal(result.ownedProcessesExited, true);
  });
  test('owned process stop before target start retires without dispatching the target', async () => {
    assert.notEqual(process.platform, 'win32');
    const startedPath = join(tmpdir(), `owned-stop-before-target-${process.pid}`);
    const owned = spawnOwnedProcess(process.execPath, ['-e', `require('node:fs').writeFileSync(${JSON.stringify(startedPath)},'started')`], {
      timeoutMs: 4_000, cleanupReservationMs: 1_000,
    });
    try {
      await owned.ready;
      owned.stop('test-stop-before-target-start');
      assert.equal(await owned.started, undefined);
      const retirement = await owned.retire('test-stop-before-target-start');
      assert.equal(await owned.targetExit, undefined);
      assert.equal(await owned.targetClose, undefined);
      assert.equal(retirement.targetDispatchRequested, false);
      assert.equal(retirement.targetProcessCreated, false);
      assert.equal(retirement.targetNoChildObserved, false);
      assert.equal(retirement.targetExecConfirmed, false);
      assert.equal(retirement.targetExitObserved, false);
      assert.equal(retirement.targetCloseObserved, false);
      assert.equal(retirement.anchorExitObserved, true);
      assert.equal(retirement.anchorCloseObserved, true);
      assert.equal(retirement.groupAbsent, true);
      assert.equal(retirement.supervisorExitObserved, true);
      assert.equal(retirement.supervisorCloseObserved, true);
      assert.equal(existsSync(startedPath), false);
    } finally {
      if (!owned.isSupervisorClosed) await owned.retire('test-stop-before-target-start-cleanup').catch(() => undefined);
      await rm(startedPath, { force: true });
    }
  });
  test('owned process retired immediately after start stops during anchor startup well before its deadline', async () => {
    assert.notEqual(process.platform, 'win32');
    const startedPath = join(tmpdir(), `owned-retire-after-start-${process.pid}`);
    const owned = spawnOwnedProcess(process.execPath, ['-e', `require('node:fs').writeFileSync(${JSON.stringify(startedPath)},'started');setInterval(()=>{},1000)`], {
      timeoutMs: 15_000, cleanupReservationMs: 1_000,
    });
    try {
      await owned.ready;
      owned.start();
      const retiredFrom = performance.now();
      const retirement = await owned.retire('test-retire-after-start');
      const elapsedMs = Math.round(performance.now() - retiredFrom);
      const details = JSON.stringify({ elapsedMs, retirement });
      assert.ok(elapsedMs < 5_000, details);
      assert.equal(await owned.stopCause, 'test-retire-after-start', details);
      assert.equal(await owned.started, undefined, details);
      assert.equal(retirement.targetDispatchRequested, false, details);
      assert.equal(retirement.targetProcessCreated, false, details);
      assert.equal(retirement.anchorExitObserved, true, details);
      assert.equal(retirement.anchorCloseObserved, true, details);
      assert.equal(retirement.groupAbsent, true, details);
      assert.equal(retirement.inputClosedObserved, true, details);
      assert.equal(retirement.supervisorExitObserved, true, details);
      assert.equal(retirement.supervisorCloseObserved, true, details);
      assert.equal(retirement.supervisorExitCode, 0, details);
      assert.equal(retirement.supervisorSignal, null, details);
      assert.equal(existsSync(startedPath), false, details);
    } finally {
      if (!owned.isSupervisorClosed) await owned.retire('test-retire-after-start-cleanup').catch(() => undefined);
      await rm(startedPath, { force: true });
    }
  });
  test('owned process caller output break cannot become healthy target EOF or retirement', async () => {
    assert.notEqual(process.platform, 'win32');
    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    const targetCode = "process.stdout.write('FIRST');setTimeout(()=>process.stdout.write(Buffer.alloc(65536,66)),100);setInterval(()=>{},1000)";
    const owned = spawnOwnedProcess(nodePath, ['-e', targetCode], { timeoutMs: 4_000, cleanupReservationMs: 1_000 });
    const callerClose = new Promise<void>(resolve => owned.stdout.once('close', resolve));
    owned.stdout.once('data', () => owned.stdout.destroy());
    try {
      await owned.ready;
      owned.start();
      const binding = await owned.started;
      assert.ok(binding);
      await owned.targetClose;
      const retirement = await owned.retire('caller-output-break-negative-control');
      await callerClose;
      assert.equal(retirement.stdoutNaturalEnd, true);
      assert.equal(retirement.stdoutCloseObserved, true);
      assert.equal(retirement.managerStdoutNaturalEnd, true);
      assert.equal(retirement.managerStdoutCloseObserved, true);
      assert.equal(retirement.callerStdoutCloseObserved, true);
      assert.equal(retirement.targetOutputRelayHealthy, false);
      assert.equal(retirement.supervisorStdoutCloseObserved, true);
      assert.equal(retirement.groupAbsent, true);
    } finally {
      if (!owned.isSupervisorClosed) {
        await owned.retire('caller-output-break-negative-control-cleanup').catch(() => undefined);
        owned.release();
      }
    }
  });
  darwinProcessExitTest('HOST lease parent loss retires generic command groups', async () => {
    const fixture = await harness.createFixture();
    const fakeAdbEnvironment = await installFakeAdbLifetimeBackstop(fixture);
    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    const cases = [
      { name: 'node-live-term', kind: 'node', signal: 'SIGTERM' as const, leaderExited: false, stoppedSupervisor: false },
      { name: 'node-leader-exited-kill', kind: 'node', signal: 'SIGKILL' as const, leaderExited: true, stoppedSupervisor: false },
      { name: 'node-stopped-supervisor-term', kind: 'node', signal: 'SIGTERM' as const, leaderExited: false, stoppedSupervisor: true },
      { name: 'adb-shaped-term', kind: 'adb', signal: 'SIGTERM' as const, leaderExited: false, stoppedSupervisor: false }
    ];
    try {
      await verifyDarwinProcessExitStatusControls(nodePath, fixture.environment);
      for (const control of cases) {
      const readyPath = join(fixture.root, `${control.name}-target-ready`);
      const descendantReadyPath = join(fixture.root, `${control.name}-descendant-ready`);
      const exitTriggerPath = join(fixture.root, `${control.name}-exit-trigger`);
      const leaderExitedPath = join(fixture.root, `${control.name}-leader-exited`);
      const postLossTriggerPath = join(fixture.root, `${control.name}-post-loss-trigger`);
      const latePath = join(fixture.root, `${control.name}-late`);
      const actorSafetyMs = control.stoppedSupervisor ? 10_000 : 500;
      const targetSafetyMs = control.stoppedSupervisor ? 10_000 : 7_000;
      const descendantCode = `const fs=require('node:fs');process.on('SIGTERM',()=>{});fs.writeFileSync(${JSON.stringify(descendantReadyPath)},JSON.stringify({pid:process.pid,ppid:process.ppid}));setInterval(()=>{if(!fs.existsSync(${JSON.stringify(postLossTriggerPath)}))return;fs.rmSync(${JSON.stringify(postLossTriggerPath)},{force:true});process.stdout.write('POST_LOSS_EPIPE_STDOUT\\n');process.stderr.write('POST_LOSS_EPIPE_STDERR\\n')},5);setTimeout(()=>{fs.writeFileSync(${JSON.stringify(latePath)},'late');process.exit(0)},${actorSafetyMs});setTimeout(()=>process.exit(0),${targetSafetyMs});`;
      const targetCode = `const fs=require('node:fs');const {spawn}=require('node:child_process');process.on('SIGTERM',()=>{});const child=spawn(${JSON.stringify(nodePath)},['-e',${JSON.stringify(descendantCode)}],{stdio:'inherit'});const ready=setInterval(()=>{if(!fs.existsSync(${JSON.stringify(descendantReadyPath)}))return;clearInterval(ready);fs.writeFileSync(${JSON.stringify(readyPath)},JSON.stringify({pid:process.pid,ppid:process.ppid,execPath:process.execPath,execArgv:process.execArgv}));${control.leaderExited ? `const trigger=setInterval(()=>{if(!fs.existsSync(${JSON.stringify(exitTriggerPath)}))return;clearInterval(trigger);fs.writeFileSync(${JSON.stringify(leaderExitedPath)},'exited');process.exit(0)},5);` : 'setInterval(()=>{},1000);'}},5);setTimeout(()=>process.exit(0),${targetSafetyMs});`;
      const binary = control.kind === 'adb' ? 'adb' : nodePath;
      const args = control.kind === 'adb'
        ? ['-s', 'emulator-5554', 'logcat', '-b', 'main', '-b', 'system', '-v', 'epoch', '-T', '1']
        : ['-e', targetCode];
      const commandTimeoutMs = control.stoppedSupervisor ? 5_000 : 8_000;
      const ownerSource = `const {createWriteStream}=await import('node:fs');const {boundedCommand}=await import(${JSON.stringify(repositoryPath('tests/mobile/support/bounded-process.ts'))});const pending=boundedCommand(${JSON.stringify(binary)},${JSON.stringify(args)},${commandTimeoutMs},{maxBytes:4096,cleanupReservationMs:1000,observerStdout:createWriteStream('',{fd:3,autoClose:false}),observerStderr:createWriteStream('',{fd:4,autoClose:false}),observerSupervisor:createWriteStream('',{fd:5,autoClose:false})});pending.then(()=>{},()=>{});process.stdout.write(JSON.stringify({event:'command-dispatched'})+'\\n');setTimeout(()=>process.exit(0),6500);`;
      let caller: LifetimeCaller | undefined;
      let supervisorPID: number | undefined;
      let groupID: number | undefined;
      let targetPID: number | undefined;
      let descendantPID: number | undefined;
      let statusObserver: DarwinProcessExitObserver | undefined;
      let anchorObserver: DarwinProcessExitObserver | undefined;
      try {
        caller = startLifetimeCaller(ownerSource, control.kind === 'adb' ? fakeAdbEnvironment : fixture.environment);
        assert.ok(caller.child.pid);
        await caller.waitFor('command-dispatched', 3_000);
        const observedSupervisor = await waitForValue(() => lifetimeSupervisor(caller!.child.pid!), 3_000,
          `owned supervisor was not spawned for ${control.name}`);
        supervisorPID = observedSupervisor.pid;
        assert.equal(observedSupervisor.ppid, caller.child.pid);
        statusObserver = await observeDarwinProcessExit(supervisorPID);
        assert.equal(pidExists(supervisorPID), true, `supervisor disappeared before its kernel status observer was armed (${control.name})`);
        if (control.kind === 'adb') {
          await waitForValue(() => {
            try { return readFileSync(fixture.log, 'utf8').includes(args.slice(2).join(' ')) ? true : undefined; } catch { return undefined; }
          }, 3_000, 'fake adb did not enter its logcat handler');
        } else {
          const targetIdentity = await waitForFileValue(readyPath, text => JSON.parse(text) as { pid: number; ppid: number; execPath: string; execArgv: string[] }, 4_000);
          const descendantIdentity = await waitForFileValue(descendantReadyPath, text => JSON.parse(text) as { pid: number; ppid: number }, 2_000);
          targetPID = targetIdentity.pid;
          descendantPID = descendantIdentity.pid;
          assert.equal(targetIdentity.execPath, nodePath);
          assert.deepEqual(targetIdentity.execArgv, ['-e', targetCode]);
          assert.equal(descendantIdentity.ppid, targetIdentity.pid);
          assert.equal(pidExists(descendantIdentity.pid), true);
        }
        const group = await waitForValue(() => lifetimeOwnedGroup(caller!.child.pid!), 3_000, 'owned supervisor and live group anchor were not observed');
        assert.equal(group.supervisor.pid, supervisorPID);
        groupID = group.anchor.pid;
        assert.equal(group.anchor.pgid, group.anchor.pid);
        assert.ok(group.target);
        assert.equal(group.target.ppid, group.manager.pid);
        assert.equal(group.target.pgid, group.anchor.pid);
        const hardDeadlineNs = await waitForSupervisorHardDeadlineNs(caller);
        if (targetPID !== undefined) assert.equal(group.target.pid, targetPID);
        if (descendantPID !== undefined) {
          const descendant = lifetimeProcessTable().find(record => record.pid === descendantPID);
          assert.ok(descendant);
          assert.equal(descendant.ppid, targetPID);
          assert.equal(descendant.pgid, group.anchor.pid);
        }
        if (control.leaderExited) {
          await writeFile(exitTriggerPath, 'exit');
          await waitForFileValue(leaderExitedPath, text => text, 1_000);
          assert.equal(await waitForPidAbsent(targetPID!, 1_000), true, 'generic command leader must already be gone');
          assert.equal(processGroupExists(group.anchor.pid), true, 'live descendant and anchor retain the group at caller loss');
        }
        if (control.stoppedSupervisor) {
          assert.equal(process.kill(supervisorPID, 'SIGSTOP'), true);
          await waitForValue(() => {
            try { return execFileSync('ps', ['-p', String(supervisorPID), '-o', 'state='], { encoding: 'utf8' }).trim().startsWith('T') ? true : undefined; }
            catch { return undefined; }
          }, 1_000, 'supervisor did not enter the stopped state');
        }
        assert.equal(caller.child.kill(control.signal), true);
        if (control.stoppedSupervisor) {
          const closed = await caller.waitForClose(2_000);
          assert.equal(closed.signal, control.signal);
          assert.equal(processGroupExists(groupID), true, 'stopped supervisor must retain its live target group until the anchor deadline');
          anchorObserver = await observeDarwinProcessExit(groupID);
          const anchorDeadlineNs = hardDeadlineNs - 500_000_000n;
          const anchorWaitMs = Number((anchorDeadlineNs - monotonicNowNs()) / 1_000_000n);
          assert.ok(anchorWaitMs > 0, `anchor deadline already elapsed while supervisor remained stopped: ${JSON.stringify({
            anchorDeadlineNs: anchorDeadlineNs.toString(), hardDeadlineNs: hardDeadlineNs.toString(),
            nowNs: monotonicNowNs().toString(), caller: caller.observerSupervisor(),
          })}`);
          const anchorStatus = anchorObserver.wait(anchorWaitMs);
          assert.equal(anchorStatus.signal, osConstants.signals.SIGKILL);
          await waitForValue(() => lifetimeLiveGroupMembers(groupID!).length === 0 ? true : undefined,
            Math.max(1, Number((anchorDeadlineNs - monotonicNowNs()) / 1_000_000n)),
            'anchor deadline did not retire all live group members while its supervisor remained stopped');
          assert.equal(pidExists(supervisorPID), true);
          assert.match(execFileSync('ps', ['-o', 'state=', '-p', String(supervisorPID)], { encoding: 'utf8' }).trim(), /^T/u);
          assert.equal(existsSync(latePath), false, 'synthetic actor safety expiry cannot establish anchor retirement');
          assert.ok(monotonicNowNs() < anchorDeadlineNs, 'anchor retirement must precede the original anchor deadline');
          assert.equal(process.kill(supervisorPID, 'SIGCONT'), true);
          await waitForValue(() => {
            try {
              const state = execFileSync('ps', ['-o', 'state=', '-p', String(supervisorPID)], { encoding: 'utf8' }).trim();
              return state && !state.startsWith('T') ? state : undefined;
            } catch { return undefined; }
          }, Math.max(1, Number((hardDeadlineNs - monotonicNowNs()) / 1_000_000n)), 'supervisor remained stopped after SIGCONT');
          assert.equal(await waitForGroupAbsent(groupID, Math.max(1, Number((hardDeadlineNs - monotonicNowNs()) / 1_000_000n))), true,
            'resumed supervisor must reap the already-retired anchor before stream checks');
        }
        if (control.kind === 'node' && !control.stoppedSupervisor) {
          await waitForValue(() => caller!.child.signalCode === control.signal ? true : undefined, 1_000, 'caller process did not exit before the post-loss write');
          await writeFile(postLossTriggerPath, 'write');
          await waitForValue(() => caller!.observerOutput().includes('POST_LOSS_EPIPE_STDOUT')
            && caller!.observerStderr().includes('POST_LOSS_EPIPE_STDERR') ? true : undefined,
          1_000, 'supervisor did not drain both streams after downstream closure');
          const pipeEvents = caller.observerSupervisor().split('\n').map((line) => {
            try { return JSON.parse(line) as { type?: string; stream?: string }; } catch { return undefined; }
          });
          assert.ok(pipeEvents.some(event => event?.type === 'downstream-epipe' && event.stream === 'stdout'));
          assert.ok(pipeEvents.some(event => event?.type === 'downstream-epipe' && event.stream === 'stderr'));
        }
        await assertCallerLossRetired(caller, statusObserver, supervisorPID, groupID, control.signal, hardDeadlineNs, control.name);
        if (control.kind === 'adb') {
          const diagnostic = await waitForValue(() => {
            try {
              const value = JSON.parse(readFileSync(fixture.diagnostic, 'utf8')) as FakeAdbDiagnostic;
              return value.children.flatMap(child => child.events).find(event => event.stage === 'fake-child-exit' && event.childExitCategory === 'signal');
            } catch { return undefined; }
          }, 1_000, 'fake adb did not record its actual signaled exit');
          assert.equal(diagnostic.stage, 'fake-child-exit');
        }
        await new Promise(resolve => setTimeout(resolve, 600));
        assert.equal(existsSync(latePath), false, `caller-loss safety actor wrote late output for ${control.name}`);
        if (targetPID !== undefined) assert.equal(await waitForPidAbsent(targetPID, 1_000), true);
        if (descendantPID !== undefined) assert.equal(await waitForPidAbsent(descendantPID, 1_000), true);
      } finally {
        anchorObserver?.close();
        statusObserver?.close();
        if (control.stoppedSupervisor && supervisorPID !== undefined && pidExists(supervisorPID)) {
          try { process.kill(supervisorPID, 'SIGCONT'); }
          catch (error) { assert.equal((error as NodeJS.ErrnoException).code, 'ESRCH'); }
        }
        if (caller && caller.child.exitCode === null && caller.child.signalCode === null) await terminateLifetimeCaller(caller, 'SIGTERM');
        if (supervisorPID !== undefined) assert.equal(await waitForPidAbsent(supervisorPID, 8_000), true);
        if (groupID !== undefined) assert.equal(await waitForGroupAbsent(groupID, 8_000), true);
        if (targetPID !== undefined) assert.equal(await waitForPidAbsent(targetPID, 8_000), true);
        if (descendantPID !== undefined) assert.equal(await waitForPidAbsent(descendantPID, 8_000), true);
      }
      }
      await verifyObserverDescriptorOwnership(fixture.root, nodePath);
      await verifyClosedTargetInput(fixture.root, nodePath);
      await verifyFakeAdbLifetimeBackstop(fakeAdbEnvironment, fixture);
    } finally {
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  darwinProcessExitTest('HOST lease initialization and release interruption stay owned', async () => {
    const fixture = await harness.createFixture();
    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    assert.equal(callerDurationToRuntime(5_000, 10_000_000_000n, 1_000_000_000n, 2_500_000_000n), 13_500_000_000n);
    assert.equal(callerDeadlineToRuntime(6_000_000_000n, 10_000_000_000n, 2_500_000_000n), 13_500_000_000n);
    const conversionStartedAt = monotonicNowNs();
    const conversionDeadline = conversionStartedAt + 1_000_000_000n;
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 200);
    const callerAfterConversion = monotonicNowNs();
    const translatedDeadline = callerDeadlineToRuntime(conversionDeadline, conversionStartedAt, callerAfterConversion);
    assert.ok(translatedDeadline <= conversionDeadline - 150_000_000n,
      'a delayed native monotonic conversion must consume the original allowance');
    const delayedRuntimeDirectory = join(fixture.root, 'delayed-runtime-bin');
    await mkdir(delayedRuntimeDirectory, { recursive: true });
    const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;
    await writeFile(join(delayedRuntimeDirectory, 'bun'), `#!/bin/sh\n/bin/sleep 0.65\nexec ${quote(process.execPath)} "$@"\n`, { mode: 0o700 });
    const interruptedRuntimeDirectory = join(fixture.root, 'interrupted-runtime-bin');
    await mkdir(interruptedRuntimeDirectory, { recursive: true });
    await writeFile(join(interruptedRuntimeDirectory, 'bun'), `#!/bin/sh\n/bin/sleep 1.25\nexec ${quote(process.execPath)} "$@"\n`, { mode: 0o700 });
    const delayedRuntimeEnvironment = { ...fixture.environment, PATH: `${delayedRuntimeDirectory}:${fixture.environment.PATH}` };
    const interruptedRuntimeEnvironment = { ...fixture.environment, PATH: `${interruptedRuntimeDirectory}:${fixture.environment.PATH}` };
    try {
      for (const mode of ['initialization', 'release', 'unconfigured-delayed-startup', 'interrupted-unconfigured-startup'] as const) {
      const targetReadyPath = join(fixture.root, `${mode}-handoff-target-ready`);
      const releaseTriggerPath = join(fixture.root, `${mode}-handoff-trigger`);
      const targetCode = `const fs=require('node:fs');process.on('SIGTERM',()=>{});fs.writeFileSync(${JSON.stringify(targetReadyPath)},JSON.stringify({pid:process.pid,ppid:process.ppid,execPath:process.execPath,execArgv:process.execArgv}));setInterval(()=>{if(!fs.existsSync(${JSON.stringify(releaseTriggerPath)}))return;fs.rmSync(${JSON.stringify(releaseTriggerPath)},{force:true});process.stdout.write('RELEASED\\n');process.exit(0)},5);setTimeout(()=>process.exit(0),6000);`;
      const timeoutMs = mode === 'unconfigured-delayed-startup' ? 3_000 : 8_000;
      const cleanupReservationMs = mode === 'unconfigured-delayed-startup' ? 500 : 1_000;
      const lifecycleWitness = `const mark=(event,value=null)=>process.stdout.write(JSON.stringify({event,atNs:process.hrtime.bigint().toString(),value})+'\\n');owned.targetExit.then(value=>mark('target-exit',value));owned.targetClose.then(value=>mark('target-close',value));owned.retirement.then(value=>mark('retirement',value));owned.failure.then(value=>mark('failure',String(value)));owned.supervisor.once('exit',(code,signal)=>mark('supervisor-exit',{code,signal}));owned.supervisor.once('close',(code,signal)=>mark('supervisor-close',{code,signal}));owned.stdout.once('end',()=>mark('stdout-end'));owned.stdout.once('close',()=>mark('stdout-close'));owned.stderr.once('end',()=>mark('stderr-end'));owned.stderr.once('close',()=>mark('stderr-close'));`;
      const handoff = mode === 'initialization'
        ? "await owned.ready;process.stdout.write(JSON.stringify({event:'initialized',atNs:process.hrtime.bigint().toString()})+'\\n');"
        : ['unconfigured-delayed-startup', 'interrupted-unconfigured-startup'].includes(mode)
          ? "await owned.inputClosed;process.stdout.write(JSON.stringify({event:'supervisor-spawned',atNs:process.hrtime.bigint().toString(),pid:owned.supervisor.pid})+'\\n');Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,4000);"
          : "await owned.ready;owned.start();const binding=await owned.started;process.stdout.write(JSON.stringify({event:'target-started',atNs:process.hrtime.bigint().toString(),binding})+'\\n');const exit=await owned.targetExit;const close=await owned.targetClose;owned.stop('release-handoff');const evidence=await owned.retirement;process.stdout.write(JSON.stringify({event:'release-handoff',atNs:process.hrtime.bigint().toString(),exit,close,evidence})+'\\n');";
      const ownerSource = `const {createWriteStream}=await import('node:fs');const {spawnOwnedProcess}=await import(${JSON.stringify(repositoryPath('tests/mobile/support/owned-process.ts'))});const options={timeoutMs:${timeoutMs},cleanupReservationMs:${cleanupReservationMs},observerStdout:createWriteStream('',{fd:3,autoClose:false}),observerStderr:createWriteStream('',{fd:4,autoClose:false}),observerSupervisor:createWriteStream('',{fd:5,autoClose:false})};const owned=spawnOwnedProcess(${JSON.stringify(nodePath)},['-e',${JSON.stringify(targetCode)}],options);${lifecycleWitness}${handoff}setTimeout(()=>process.exit(0),6500);`;
      let caller: LifetimeCaller | undefined;
      let supervisorPID: number | undefined;
      let groupID: number | undefined;
      let targetPID: number | undefined;
      let statusObserver: DarwinProcessExitObserver | undefined;
      try {
        const delayedStartupStartedAt = ['unconfigured-delayed-startup', 'interrupted-unconfigured-startup'].includes(mode) ? monotonicNowNs() : undefined;
        const callerEnvironment = mode === 'unconfigured-delayed-startup' ? delayedRuntimeEnvironment
          : mode === 'interrupted-unconfigured-startup' ? interruptedRuntimeEnvironment : fixture.environment;
        caller = startLifetimeCaller(ownerSource, callerEnvironment);
        assert.ok(caller.child.pid);
        if (mode === 'interrupted-unconfigured-startup') {
          const spawned = await caller.waitFor('supervisor-spawned', 1_000);
          supervisorPID = Number(spawned.pid);
          assert.ok(Number.isSafeInteger(supervisorPID) && supervisorPID > 0);
          statusObserver = await observeDarwinProcessExit(supervisorPID);
          const startupProcess = lifetimeProcessTable().find(record => record.pid === supervisorPID);
          assert.ok(startupProcess);
          assert.ok(startupProcess.command.startsWith('/bin/sh '));
          assert.ok(startupProcess.command.includes('interrupted-runtime-bin/bun'));
          assert.equal(caller.observerSupervisor().includes('supervisor-runtime-started'), false);
          assert.equal(caller.child.kill('SIGTERM'), true);
          await assertCallerLossRetired(caller, statusObserver, supervisorPID, undefined, 'SIGTERM', delayedStartupStartedAt! + 3_000_000_000n);
          const elapsedMs = Number(monotonicNowNs() - delayedStartupStartedAt!) / 1_000_000;
          assert.ok(elapsedMs < 3_000, `interrupted unconfigured startup exceeded its finite owner deadline: ${elapsedMs.toFixed(1)}ms`);
          continue;
        }
        if (mode === 'unconfigured-delayed-startup') {
          const spawned = await caller.waitFor('supervisor-spawned', 1_000);
          supervisorPID = Number(spawned.pid);
          assert.ok(Number.isSafeInteger(supervisorPID) && supervisorPID > 0);
          statusObserver = await observeDarwinProcessExit(supervisorPID);
          let runtimeStarted: Record<string, unknown>;
          try {
            runtimeStarted = await waitForValue(() => {
              const record = caller!.observerSupervisor().split('\n').map(line => {
                try { return JSON.parse(line) as Record<string, unknown>; } catch { return undefined; }
              }).find(value => value?.type === 'supervisor-runtime-started');
              return record || undefined;
            }, 2_000, 'delayed supervisor runtime did not start while its caller remained unconfigured');
          } catch (error) {
            const process = lifetimeProcessTable().find(record => record.pid === supervisorPID);
            throw new Error(`${error instanceof Error ? error.message : String(error)}; observer=${caller.observerSupervisor()}; stderr=${caller.stderr()}; process=${JSON.stringify(process)}`, { cause: error });
          }
          assert.equal(runtimeStarted.pid, supervisorPID);
          let armed: Record<string, unknown>;
          try {
            armed = await waitForValue(() => {
              const records = caller!.observerSupervisor().split('\n').map(line => {
                try { return JSON.parse(line) as Record<string, unknown>; } catch { return undefined; }
              });
              return records.find(value => value?.type === 'supervisor-deadlines-armed') || undefined;
            }, 1_000, 'supervisor did not expose its original startup deadline through the live observer');
          } catch (error) {
            const process = lifetimeProcessTable().find(record => record.pid === supervisorPID);
            throw new Error(`${error instanceof Error ? error.message : String(error)}; observer=${caller.observerSupervisor()}; stderr=${caller.stderr()}; process=${JSON.stringify(process)}`, { cause: error });
          }
          const remainingMs = Number((BigInt(String(armed.hardDeadlineNs)) - BigInt(String(armed.clockNs))) / 1_000_000n);
          assert.ok(remainingMs > 0 && remainingMs < timeoutMs - 400, `delayed startup reset the hard allowance: ${remainingMs}ms remain`);
          assert.equal(caller.child.kill('SIGSTOP'), true);
          let status: DarwinProcessExitStatus;
          try { status = statusObserver.wait(3_000); } catch (error) {
            const process = lifetimeProcessTable().find(record => record.pid === supervisorPID);
            throw new Error(`${error instanceof Error ? error.message : String(error)}; elapsedMs=${(Number(monotonicNowNs() - delayedStartupStartedAt!) / 1_000_000).toFixed(1)}; supervisorAlive=${pidExists(supervisorPID)}; process=${JSON.stringify(process)}; observer=${caller.observerSupervisor()}`, { cause: error });
          }
          assert.equal(status.pid, supervisorPID);
          assert.equal(status.waitStatus, 0);
          assert.equal(status.exitCode, 0);
          assert.equal(status.signal, 0);
          const deadlineEvent = await waitForValue(() => caller!.observerSupervisor().split('\n').map(line => {
            try { return JSON.parse(line) as Record<string, unknown>; } catch { return undefined; }
          }).find(record => record?.type === 'supervisor-hard-deadline'), 1_000, 'supervisor hard deadline was not observed');
          assert.equal(deadlineEvent.retirementSent, true);
          assert.equal(deadlineEvent.inputClosed, true);
          assert.equal(deadlineEvent.targetSpawned, false);
          assert.ok(BigInt(String(deadlineEvent.clockNs)) >= BigInt(String(armed.hardDeadlineNs)));
          const elapsedMs = Number(monotonicNowNs() - delayedStartupStartedAt!) / 1_000_000;
          assert.ok(elapsedMs <= timeoutMs + 350, `unconfigured supervisor exceeded the original ${timeoutMs}ms hard deadline: ${elapsedMs.toFixed(1)}ms`);
          assert.equal(caller.child.kill('SIGCONT'), true);
          const closed = await terminateLifetimeCaller(caller, 'SIGKILL');
          assert.equal(closed.signal, 'SIGKILL');
          assert.equal(await waitForPidAbsent(supervisorPID, 1_000), true);
          continue;
        }
        if (mode === 'initialization') {
          await caller.waitFor('initialized', 3_000);
          const supervisor = await waitForValue(() => lifetimeSupervisor(caller!.child.pid!), 2_000, 'unconfigured supervisor was not observed');
          supervisorPID = supervisor.pid;
        } else {
          const started = await caller.waitFor('target-started', 4_000);
          const binding = started.binding as { targetPID: number; spawnParentPID: number; groupID: number; anchorPID: number };
          const identity = await waitForFileValue(targetReadyPath, text => JSON.parse(text) as { pid: number; ppid: number; execPath: string; execArgv: string[] }, 2_000);
          targetPID = identity.pid;
          assert.equal(identity.pid, binding.targetPID);
          assert.equal(identity.ppid, binding.spawnParentPID);
          assert.equal(identity.execPath, nodePath);
          assert.deepEqual(identity.execArgv, ['-e', targetCode]);
          const group = await waitForValue(() => lifetimeOwnedGroup(caller!.child.pid!), 2_000, 'release handoff did not retain a live anchor and target group');
          supervisorPID = group.supervisor.pid;
          groupID = group.anchor.pid;
          assert.equal(group.manager.pid, binding.spawnParentPID);
          assert.equal(group.anchor.pid, binding.groupID);
          assert.equal(group.target?.pid, targetPID);
          assert.equal(processGroupExists(groupID), true);
          await writeFile(releaseTriggerPath, 'release');
          const release = await caller.waitFor('release-handoff', 3_000);
          const evidence = release.evidence as RetirementEvidence;
          assert.equal((release.exit as { code: number }).code, 0);
          assert.equal(evidence.targetExitObserved, true);
          assert.equal(evidence.targetCloseObserved, true);
          assert.equal(evidence.anchorExitObserved, true);
          assert.equal(evidence.anchorCloseObserved, true);
          assert.equal(evidence.groupAbsent, true);
          assert.equal(evidence.stdoutNaturalEnd, true);
          assert.equal(evidence.stderrNaturalEnd, true);
          assert.equal(processGroupExists(groupID), false);
          assert.equal(pidExists(supervisorPID), true, 'supervisor remains live during its release handoff');
          assert.deepEqual(caller.observerStreams(), {
            stdoutEnded: false, stdoutClosed: false, stderrEnded: false, stderrClosed: false,
            supervisorEnded: false, supervisorClosed: false, childCloseObserved: false,
          });
        }
        const expectedSignal = mode === 'initialization' ? 'SIGTERM' : 'SIGKILL';
        statusObserver = await observeDarwinProcessExit(supervisorPID);
        const hardDeadlineNs = await waitForSupervisorHardDeadlineNs(caller);
        assert.equal(caller.child.kill(expectedSignal), true);
        await assertCallerLossRetired(caller, statusObserver, supervisorPID, groupID, expectedSignal, hardDeadlineNs);
      } finally {
        statusObserver?.close();
        if (caller && caller.child.exitCode === null && caller.child.signalCode === null) {
          if (mode === 'unconfigured-delayed-startup') caller.child.kill('SIGCONT');
          await terminateLifetimeCaller(caller, mode === 'unconfigured-delayed-startup' ? 'SIGKILL' : 'SIGTERM');
        }
        if (supervisorPID !== undefined) assert.equal(await waitForPidAbsent(supervisorPID, 8_000), true);
        if (groupID !== undefined) assert.equal(await waitForGroupAbsent(groupID, 8_000), true);
        if (targetPID !== undefined) assert.equal(await waitForPidAbsent(targetPID, 8_000), true);
      }
      }
      await verifyNodeSupervisorBunAnchorStartup(fixture, nodePath);
    } finally {
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  darwinProcessExitTest('HOST lease default and injected collectors retire on caller loss', async () => {
    const fixture = await harness.createFixture();
    const fakeAdbEnvironment = await installFakeAdbLifetimeBackstop(fixture);
    const measurementModule = repositoryPath('tests/mobile/android-measurement.ts');
    const budgetModule = repositoryPath('tests/mobile/support/budget.ts');
    const ownedModule = repositoryPath('tests/mobile/support/owned-process.ts');
    const toolchains = repositoryPath('tests/mobile/toolchains.json');
    try {
      for (const mode of ['default', 'injected-failed-begin'] as const) {
      const outputDir = join(fixture.root, `lifetime-${mode}-output`);
      const observer = "collectorObserverStdout:createWriteStream('',{fd:3,autoClose:false}),collectorObserverStderr:createWriteStream('',{fd:4,autoClose:false}),collectorObserverSupervisor:createWriteStream('',{fd:5,autoClose:false})";
      const injected = mode === 'injected-failed-begin'
        ? `spawnCollector:()=>spawnOwnedProcess('adb',['-s','emulator-5554','logcat','-b','main','-b','system','-v','epoch','-T','1'],{timeoutMs:budget.remainingMs,cleanupReservationMs:1000,observerStdout:createWriteStream('',{fd:3,autoClose:false}),observerStderr:createWriteStream('',{fd:4,autoClose:false}),observerSupervisor:createWriteStream('',{fd:5,autoClose:false})}),stopProcess:async collector=>{process.stdout.write(JSON.stringify({event:'failed-begin-cleanup',atNs:process.hrtime.bigint().toString()})+'\\n');await new Promise(resolve=>setTimeout(resolve,5000));return collector.retire('measurement-finished')},boundedCommand:async(binary,args,timeout,options)=>{if(args.includes('snapshot'))throw new Error('controlled public failed-begin after collector startup');return boundedCommand(binary,args,timeout,options)}`
        : '';
      const io = mode === 'default' ? `{${observer}}` : `{${observer},${injected}}`;
      const ownerSource = `const {createWriteStream}=await import('node:fs');const {AndroidEnvironmentMeasurement}=await import(${JSON.stringify(measurementModule)});const {PhaseBudget}=await import(${JSON.stringify(budgetModule)});const {spawnOwnedProcess}=await import(${JSON.stringify(ownedModule)});const {boundedCommand}=await import(${JSON.stringify(repositoryPath('tests/mobile/support/bounded-process.ts'))});const budget=new PhaseBudget('caller-loss-${mode}',{timeoutMs:10000,recoveryLimit:0});const measurement=new AndroidEnvironmentMeasurement('emulator-5554',${JSON.stringify(outputDir)},${JSON.stringify(toolchains)},${io},budget);measurement.bind({runId:'35556703407',attempt:'2',platform:'android',baseline:'0.20.10',sourceCommit:'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',sourceRunHeadSha:'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',candidateWebHash:'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',candidateBuild:'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd',measurementId:measurement.id});await measurement.observeGmsStage('after-device-start',budget);await measurement.observeGmsStage('before-measurement-begin',budget);const log=(event,value)=>process.stdout.write(JSON.stringify({event,atNs:process.hrtime.bigint().toString(),value})+String.fromCharCode(10));${mode === 'default' ? "try{await measurement.begin();log('measurement-ready',{id:measurement.id})}catch(error){log('begin-error',{message:String(error),result:error?.result})}" : "measurement.begin().then(()=>log('unexpected-begin-success'),error=>log('begin-error',{message:String(error),result:error?.result}));"}setTimeout(()=>process.exit(0),6500);`;
      let caller: LifetimeCaller | undefined;
      let supervisorPID: number | undefined;
      let groupID: number | undefined;
      let targetPID: number | undefined;
      let statusObserver: DarwinProcessExitObserver | undefined;
      try {
        caller = startLifetimeCaller(ownerSource, fakeAdbEnvironment);
        assert.ok(caller.child.pid);
        if (mode === 'default') await caller.waitFor('measurement-ready', 7_000);
        else await caller.waitFor('failed-begin-cleanup', 7_000);
        await waitForValue(() => {
          try { return readFileSync(fixture.log, 'utf8').includes('logcat -b main -b system -v epoch -T 1') ? true : undefined; } catch { return undefined; }
        }, 2_000, 'public measurement did not start the fake adb collector');
        const group = await waitForValue(() => {
          const owned = lifetimeOwnedGroup(caller!.child.pid!);
          return owned?.target ? owned : undefined;
        }, 3_000, 'collector supervisor, group, and target were not observed');
        supervisorPID = group.supervisor.pid;
        groupID = group.anchor.pid;
        targetPID = group.target?.pid;
        assert.ok(targetPID);
        assert.equal(group.target!.ppid, group.manager.pid);
        assert.equal(group.target!.pgid, groupID);
        if (mode === 'default') {
          await waitForValue(() => /HerdrMeasure:.*START/u.test(caller!.observerOutput()) ? true : undefined,
            1_000, 'default collector output observer did not receive its measurement marker');
        }
        statusObserver = await observeDarwinProcessExit(supervisorPID);
        const hardDeadlineNs = await waitForSupervisorHardDeadlineNs(caller);
        assert.equal(caller.child.kill(mode === 'default' ? 'SIGTERM' : 'SIGKILL'), true);
        await assertCallerLossRetired(caller, statusObserver, supervisorPID, groupID, mode === 'default' ? 'SIGTERM' : 'SIGKILL', hardDeadlineNs);
        assert.equal(await waitForPidAbsent(targetPID!, 1_000), true);
        const diagnostic = await waitForValue(() => {
          try {
            const value = JSON.parse(readFileSync(fixture.diagnostic, 'utf8')) as FakeAdbDiagnostic;
            return value.children.flatMap(child => child.events).find(event => event.stage === 'fake-child-exit' && event.childExitCategory === 'signal');
          } catch { return undefined; }
        }, 1_000, 'fake adb collector did not record its actual signal exit');
        assert.equal(diagnostic.stage, 'fake-child-exit');
      } finally {
        statusObserver?.close();
        if (caller && caller.child.exitCode === null && caller.child.signalCode === null) await terminateLifetimeCaller(caller, 'SIGTERM');
        if (supervisorPID !== undefined) assert.equal(await waitForPidAbsent(supervisorPID, 8_000), true);
        if (groupID !== undefined) assert.equal(await waitForGroupAbsent(groupID, 8_000), true);
        if (targetPID !== undefined) assert.equal(await waitForPidAbsent(targetPID, 8_000), true);
      }
      }
    } finally {
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  tests.push(['Android retained production measurement collector honors enclosing fixture lifetime', async () => {
    const fixture = await harness.createFixture();
    let collector: OwnedProcessHandle | undefined;
    let groupID: number | undefined;
    await withAndroidRetainedLifecycleFixture(fixture, async ({ measurement }) => {
      collector = (measurement as unknown as { collector: { process?: OwnedProcessHandle } }).collector.process;
      assert.ok(collector, 'retained fixture must use the production default collector');
      await collector.ready;
      const binding = await collector.started;
      assert.ok(binding, 'production collector target must start');
      groupID = binding.groupID;
      assert.equal(processGroupAbsent(groupID), false, 'production collector group must remain active during the fixture');
    });
    assert.ok(collector);
    assert.ok(groupID);
    const retirement = await collector.retirement;
    assert.equal(retirement.targetExitObserved, true);
    assert.equal(retirement.targetCloseObserved, true);
    assert.equal(retirement.anchorExitObserved, true);
    assert.equal(retirement.anchorCloseObserved, true);
    assert.equal(retirement.groupAbsent, true);
    assert.equal(retirement.inputClosedObserved, true);
    assert.equal(retirement.stdoutNaturalEnd, true);
    assert.equal(retirement.stdoutCloseObserved, true);
    assert.equal(retirement.stderrNaturalEnd, true);
    assert.equal(retirement.stderrCloseObserved, true);
    assert.equal(collector.supervisor.exitCode, 0);
    assert.equal(collector.supervisor.signalCode, null);
    assert.equal(collector.isSupervisorClosed, true);
    assert.equal(processGroupAbsent(groupID), true);
    const summary = JSON.parse(await readFile(join(fixture.root, 'android-environment-collector.json'), 'utf8'));
    assert.ok(summary.bytes > 0, 'production collector must persist bytes acquired during the fixture');
  }]);
  test('bounded stdin relays preserve multi-buffer bytes and natural EOF after delayed target consumption', async () => {
    const fixture = await harness.createFixture();
    const input = Buffer.alloc(256 * 1024);
    for (let index = 0; index < input.length; index++) input[index] = index % 251;
    const sha256 = createHash('sha256').update(input).digest('hex');
    const code = `setTimeout(async()=>{const hash=require('node:crypto').createHash('sha256');let bytes=0;for await(const chunk of process.stdin){bytes+=chunk.length;hash.update(chunk)}process.stdout.write(JSON.stringify({bytes,sha256:hash.digest('hex'),eof:process.stdin.readableEnded})+'\\n')},200)`;
    for (const [name, binary, args] of [['bun', process.execPath, ['--no-env-file', '-e', code]], ['node', 'node', ['-e', code]]] as const) {
      const observer = createBoundedSupervisorObserver(fixture.root, `delayed-input-${name}-supervisor`);
      try {
        const result = await boundedCommand(binary, [...args], 5_000, {
          input, cleanupReservationMs: 1_000, maxBytes: 1_024, observerSupervisor: observer.stream,
        });
        const records = observer.records();
        const details = JSON.stringify({ command: boundedCommandFailureDetails(result, records), records });
        assert.deepEqual(JSON.parse(result.stdout), { bytes: input.length, sha256, eof: true }, details);
        assert.equal(result.childExited, true, details);
        assert.equal(result.ownedProcessesExited, true, details);
        assert.equal(result.stdioClosed, true, details);
        assert.equal(result.timedOut, false, details);
        for (const type of ['supervisor-input-observation', 'supervisor-manager-input-observation']) {
          const observations = records.filter(record => record.type === type);
          assert.ok(observations.length <= 9, details);
          for (const event of ['connected', 'sourceEnd', 'sourceClose', 'destinationFinish', 'destinationClose']) {
            assert.equal(observations.filter(record => record.event === event).length, 1, details);
          }
          assert.equal(observations.some(record => record.event === 'sourceError' || record.event === 'destinationError'), false, details);
          const ended = observations.find(record => record.event === 'sourceEnd')!.summary as Record<string, unknown>;
          assert.equal(ended.sourceBytes, input.length, details);
          assert.equal(ended.countSaturated, false, details);
          assert.ok(BigInt(String(ended.firstByteNs)) <= BigInt(String(ended.lastByteNs)), details);
          assert.ok(BigInt(String(ended.lastByteNs)) <= BigInt(String(ended.sourceEndNs)), details);
          const finished = observations.find(record => record.event === 'destinationFinish')!.summary as Record<string, unknown>;
          assert.equal(finished.destinationWritableLength, 0, details);
          assert.equal(finished.destinationFinished, true, details);
        }
        const teardown = records.find(record => record.type === 'supervisor-teardown-started');
        assert.deepEqual(teardown?.unmetRetirementPrerequisites, [], details);
        assert.ok(Buffer.byteLength(JSON.stringify(records)) < 32_768, details);
      } finally {
        await observer.close();
      }
    }
  });
  test('bounded child commands count mixed output on failure, stop overflow and kill SIGTERM-resistant children', async () => {
    assert.equal(execFileSync('node', ['--version'], { encoding: 'utf8' }).trim(), 'v26.13.0');
    const code = `const fs=require('node:fs');const chunk=Buffer.alloc(1100000,65);const write=fd=>{let offset=0;while(offset<chunk.length)offset+=fs.writeSync(fd,chunk,offset,chunk.length-offset)};write(1);write(2);setTimeout(()=>{process.exitCode=7},250)`;
    let failed: BoundedCommandError | undefined;
    try {
      await boundedCommand('node', ['-e', code], 5_000, { maxBytes: 3_000_000, maxBytesPerStream: 2_000_000 });
    } catch (error) {
      assert.ok(error instanceof BoundedCommandError);
      failed = error;
    }
    assert.ok(failed);
    assert.equal(failed.result.code, 7);
    assert.equal(failed.result.stdoutBytes, 1_100_000, JSON.stringify({ code: failed.result.code,
      durationMs: failed.result.durationMs, timedOut: failed.result.timedOut, aborted: failed.result.aborted,
      stdoutBytes: failed.result.stdoutBytes, stderrBytes: failed.result.stderrBytes,
      childExited: failed.result.childExited, ownedProcessesExited: failed.result.ownedProcessesExited,
      stdioClosed: failed.result.stdioClosed }));
    assert.equal(failed.result.stderrBytes, 1_100_000);
    assert.equal(failed.result.stdoutRetainedBytes, 1_100_000);
    assert.equal(failed.result.stderrRetainedBytes, 1_100_000);
    assert.equal(failed.result.childExited, true);
    assert.equal(failed.result.stdoutSha256, createHash('sha256').update(Buffer.alloc(1_100_000, 65)).digest('hex'));

    let overflow: BoundedCommandError | undefined;
    try {
      await boundedCommand('node', ['-e', code], 3_000, { maxBytes: 1_000_000, maxBytesPerStream: 2_000_000 });
    } catch (error) {
      assert.ok(error instanceof BoundedCommandError);
      overflow = error;
    }
    assert.ok(overflow);
    assert.equal(overflow.result.outputLimitExceeded, true);
    assert.ok(overflow.result.stdoutBytes + overflow.result.stderrBytes > 1_000_000);
    assert.ok(overflow.result.stdoutRetainedBytes + overflow.result.stderrRetainedBytes <= 1_000_000);
    assert.equal(overflow.result.childExited, true);

    const fixture = await harness.createFixture();
    const sentinel = join(fixture.root, 'late-child-sentinel');
    const controller = new AbortController();
    const lateCode = `process.on('SIGTERM',()=>{});setTimeout(()=>{require('node:fs').writeFileSync(${JSON.stringify(sentinel)},'late');process.exit(0)},500)`;
    const timeoutObserver = createBoundedSupervisorObserver(fixture.root, 'bounded-timeout-supervisor');
    const watchdog = setTimeout(() => controller.abort(), 1_500);
    try {
      let timedOut: BoundedCommandError | undefined;
      try {
        await boundedCommand('node', ['-e', lateCode], 250, {
          maxBytes: 1_000, signal: controller.signal, observerSupervisor: timeoutObserver.stream,
        });
      } catch (error) {
        assert.ok(error instanceof BoundedCommandError);
        timedOut = error;
      }
      await timeoutObserver.close();
      const timeoutDetails = JSON.stringify({ command: boundedCommandFailureDetails(timedOut?.result, timeoutObserver.records()) });
      assert.ok(timedOut, timeoutDetails);
      assert.equal(timedOut.result.timedOut, true, timeoutDetails);
      assert.equal(timedOut.result.childExited, true, timeoutDetails);
      assert.ok(timedOut.result.durationMs < 1_000, timeoutDetails);
      const settledEvidence = JSON.stringify(timedOut.result);
      await new Promise(resolve => setTimeout(resolve, 350));
      assert.equal(existsSync(sentinel), false);
      assert.equal(JSON.stringify(timedOut.result), settledEvidence);
    } finally {
      await timeoutObserver.close();
      clearTimeout(watchdog);
    }

    const descendantSentinel = join(fixture.root, 'late-descendant-sentinel');
    const descendantCode = `process.on('SIGTERM',()=>{});setTimeout(()=>{require('node:fs').writeFileSync(${JSON.stringify(descendantSentinel)},'late');process.exit(0)},500);setInterval(()=>{},1000)`;
    const parentCode = `require('node:child_process').spawn(process.execPath,['-e',${JSON.stringify(descendantCode)}],{stdio:'inherit'});process.exit(0)`;
    const descendantObserver = createBoundedSupervisorObserver(fixture.root, 'bounded-descendant-supervisor');
    let descendantResult: BoundedCommandError | undefined;
    try {
      await boundedCommand('node', ['-e', parentCode], 250, {
        maxBytes: 1_000, cleanupReservationMs: 150, observerSupervisor: descendantObserver.stream,
      });
    } catch (error) {
      assert.ok(error instanceof BoundedCommandError);
      descendantResult = error;
    }
    await descendantObserver.close();
    const descendantDetails = JSON.stringify({ command: boundedCommandFailureDetails(descendantResult?.result, descendantObserver.records()) });
    assert.ok(descendantResult, descendantDetails);
    assert.equal(descendantResult.result.timedOut, true, descendantDetails);
    assert.equal(descendantResult.result.childExited, true, descendantDetails);
    assert.equal(descendantResult.result.ownedProcessesExited, true, descendantDetails);
    assert.equal(descendantResult.result.stdioClosed, true, descendantDetails);
    await new Promise(resolve => setTimeout(resolve, 400));
    assert.equal(existsSync(descendantSentinel), false);

    const nodePath = execFileSync('node', ['-e', 'process.stdout.write(process.execPath)'], { encoding: 'utf8' }).trim();
    const fastSignal = spawnOwnedProcess(nodePath, ['-e', "process.kill(process.pid,'SIGTERM')"], { timeoutMs: 5_000, cleanupReservationMs: 1_000 });
    let fastOutput = '';
    fastSignal.stdout.on('data', chunk => { fastOutput += String(chunk); });
    try {
      await fastSignal.ready;
      fastSignal.start();
      const binding = await fastSignal.started;
      assert.ok(binding, 'close-on-exec status EOF must preserve admission before an immediate target signal');
      const exit = await fastSignal.targetExit;
      const close = await fastSignal.targetClose;
      assert.ok(exit);
      assert.ok(close);
      assert.equal(exit.code, null);
      assert.equal(exit.signal, 'SIGTERM');
      assert.equal(exit.launchError, undefined);
      assert.equal(close.code, null);
      assert.equal(close.signal, 'SIGTERM');
      assert.equal(close.launchError, undefined);
      assert.equal(fastOutput, '');
      const retirement = await fastSignal.retire('fast-signaled-target');
      assert.equal(retirement.groupAbsent, true);
      assert.equal(retirement.anchorExitObserved, true);
      assert.equal(retirement.anchorCloseObserved, true);
      assert.equal(retirement.stdoutCloseObserved, true);
      assert.equal(retirement.stderrCloseObserved, true);
    } finally {
      if (!fastSignal.isSupervisorClosed) await fastSignal.retire('fast-signaled-target-cleanup');
    }

    const delayedRuntimeDirectory = join(fixture.root, 'bounded-delayed-runtime-bin');
    const wrapperPIDPath = join(fixture.root, 'bounded-delayed-runtime-pid');
    const startupDelayPath = join(fixture.root, 'bounded-supervisor-startup-delay.mjs');
    const startupObserverPath = join(fixture.root, 'bounded-supervisor-startup-observer.jsonl');
    await mkdir(delayedRuntimeDirectory, { recursive: true });
    await writeFile(startupDelayPath, `import {monotonicNowNs} from ${JSON.stringify(repositoryPath('tests/mobile/support/owned-process.ts'))};const executionDeadlineNs=BigInt(process.argv[process.argv.indexOf('--supervisor')+1]);while(monotonicNowNs()<=executionDeadlineNs)Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,5);`);
    const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;
    const realBun = execFileSync('which', ['bun'], { encoding: 'utf8' }).trim();
    const wrapper = `#!/bin/sh\nfor argument do\n  if [ "$argument" = '--supervisor' ]; then\n    printf '%s\\n' "$$" > ${quote(wrapperPIDPath)}\n    exec ${quote(realBun)} --no-env-file --preload ${quote(startupDelayPath)} "$@"\n  fi\ndone\nexec ${quote(realBun)} --no-env-file "$@"\n`;
    await writeFile(join(delayedRuntimeDirectory, 'bun'), wrapper, { mode: 0o700 });
    const observerFD = openSync(startupObserverPath, 'w');
    const observerSupervisor = createWriteStream('', { fd: observerFD, autoClose: false });
    const previousPath = process.env.PATH;
    let startupTimeout: BoundedCommandError | undefined;
    try {
      process.env.PATH = `${delayedRuntimeDirectory}:${previousPath || ''}`;
      await boundedCommand('node', ['-e', 'process.exit(0)'], 1_800, {
        maxBytes: 1_024, cleanupReservationMs: 600, observerSupervisor,
      });
    } catch (error) {
      assert.ok(error instanceof BoundedCommandError);
      startupTimeout = error;
    } finally {
      if (previousPath === undefined) delete process.env.PATH;
      else process.env.PATH = previousPath;
      await new Promise<void>(resolve => observerSupervisor.end(resolve));
      closeSync(observerFD);
    }
    assert.ok(startupTimeout);
    assert.equal(startupTimeout.result.timedOut, true);
    assert.equal(startupTimeout.result.aborted, false);
    assert.equal(startupTimeout.result.stdioClosed, true);
    assert.ok(startupTimeout.result.durationMs < 1_800, JSON.stringify(startupTimeout.result));
    const delayedSupervisorPID = await waitForFileValue(wrapperPIDPath, text => Number(text), 1_000);
    assert.ok(Number.isSafeInteger(delayedSupervisorPID) && delayedSupervisorPID > 0);
    assert.equal(await waitForPidAbsent(delayedSupervisorPID, 1_000), true);
    assert.equal(await waitForGroupAbsent(delayedSupervisorPID, 1_000), true);
    const startupRecords = (await readFile(startupObserverPath, 'utf8')).trim().split('\n')
      .map(line => JSON.parse(line) as { type?: string; clockNs?: string; executionDeadlineNs?: string; hardDeadlineNs?: string; evidence?: Record<string, unknown> });
    const runtimeStarted = startupRecords.find(record => record.type === 'supervisor-runtime-started');
    const deadlines = startupRecords.find(record => record.type === 'supervisor-deadlines-armed');
    assert.ok(runtimeStarted && deadlines);
    assert.ok(BigInt(runtimeStarted.clockNs!) > BigInt(deadlines.executionDeadlineNs!), 'supervisor must initialize after the original execution deadline');
    assert.ok(BigInt(runtimeStarted.clockNs!) < BigInt(deadlines.hardDeadlineNs!), 'supervisor initialization must remain inside the original hard deadline');
    const retirementRecord = startupRecords.find(record => record.type === 'supervisor-retirement-ready');
    assert.ok(retirementRecord);
    assert.equal(retirementRecord.evidence?.anchorCloseObserved, true);
    assert.equal(retirementRecord.evidence?.groupAbsent, true);
    assert.equal(retirementRecord.evidence?.targetExitObserved, false);
    assert.equal(startupRecords.some(record => record.type === 'anchor-process-started' || record.type === 'target-process-started'
      || record.type === 'target-started'), false);
    assert.equal(startupRecords.some(record => record.type === 'supervisor-teardown-started'), true);
  });
  test('bounded explicit abort remains aborted through owned process retirement', async () => {
    const fixture = await harness.createFixture();
    const readyPath = join(fixture.root, 'bounded-abort-target-ready');
    const controller = new AbortController();
    const observer = createBoundedSupervisorObserver(fixture.root, 'bounded-abort-supervisor');
    const code = `process.on('SIGTERM',()=>{});require('node:fs').writeFileSync(${JSON.stringify(readyPath)},'ready');setInterval(()=>{},1000)`;
    const command = boundedCommand(process.execPath, ['-e', code], 5_000, {
      maxBytes: 1_024, cleanupReservationMs: 1_000, signal: controller.signal, observerSupervisor: observer.stream,
    }).then(result => ({ result }), error => ({ error }));
    try {
      await waitForFileValue(readyPath, text => text, 2_000);
      controller.abort(new Error('unit test cancellation'));
      const outcome = await command;
      await observer.close();
      const details = JSON.stringify({ command: boundedCommandFailureDetails(
        'error' in outcome && outcome.error instanceof BoundedCommandError ? outcome.error.result : undefined, observer.records()) });
      assert.ok('error' in outcome && outcome.error instanceof BoundedCommandError, details);
      assert.equal(outcome.error.result.aborted, true, details);
      assert.equal(outcome.error.result.timedOut, false, details);
      assert.equal(outcome.error.result.outputLimitExceeded, false, details);
      assert.equal(outcome.error.result.childExited, true, details);
      assert.equal(outcome.error.result.ownedProcessesExited, true, details);
      assert.equal(outcome.error.result.stdioClosed, true, details);
      const retirement = observer.records().find(record => record.type === 'supervisor-retirement-ready');
      assert.ok(retirement, details);
      const evidence = retirement.evidence as Record<string, unknown>;
      assert.equal(evidence.stopReason, 'aborted', details);
    } finally {
      if (!controller.signal.aborted) controller.abort(new Error('unit test cleanup'));
      await command;
      await observer.close();
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  test('owned caller stop followed by an immediate release keeps the requested stop reason', async () => {
    const fixture = await harness.createFixture();
    const observer = createBoundedSupervisorObserver(fixture.root, 'stop-release-order-supervisor');
    const owned = spawnOwnedProcess(process.execPath, ['-e', 'setInterval(()=>{},1000)'], {
      timeoutMs: 8_000, cleanupReservationMs: 2_000, observerSupervisor: observer.stream,
    });
    try {
      await owned.ready;
      owned.start();
      assert.ok(await owned.started);
      owned.stop('aborted');
      owned.release();
      assert.equal(owned.supervisor.stdin?.writableEnded, false, 'lease ended before the supervisor acknowledged the stop');
      assert.equal(await owned.stopCause, 'aborted');
      const retirement = await owned.retirement;
      if (!owned.isSupervisorClosed) await new Promise(resolve => owned.supervisor.once('close', resolve));
      await observer.close();
      const records = observer.records();
      const details = JSON.stringify({ records, retirement });
      const ready = records.find(record => record.type === 'supervisor-retirement-ready');
      assert.equal((ready?.evidence as Record<string, unknown> | undefined)?.stopReason, 'aborted', details);
      assert.equal(records.find(record => record.type === 'supervisor-stopping')?.reason, 'aborted', details);
      assert.equal(owned.supervisor.stdin?.writableEnded, true);
      assert.equal(retirement.groupAbsent, true, details);
      assert.equal(owned.supervisor.exitCode, 0, details);
    } finally {
      if (!owned.isSupervisorClosed) {
        await owned.retire('stop-release-order-cleanup').catch(() => undefined);
        owned.release();
      }
      if (owned.binding) assert.equal(await waitForGroupAbsent(owned.binding.groupID, 8_000), true);
      await observer.close();
      await rm(fixture.root, { recursive: true, force: true });
    }
  });
  return tests;
}
