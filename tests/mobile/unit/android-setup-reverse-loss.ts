import { strict as assert } from 'node:assert';
import { existsSync, readFileSync, unlinkSync, writeFileSync } from 'node:fs';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { AndroidPlatform } from '../platforms/android';
import { PhaseBudget } from '../support/budget';
import { DiagnosticRecorder } from '../support/diagnostics';
import { AppiumClient, WebDriverError, type SessionOptions } from '../support/webdriver';

const baseline = 'host-17 tcp:44323 tcp:44323\nhost-17 tcp:33251 tcp:33251\nhost-17 tcp:33252 tcp:33252\n';
type Fence = 'setup' | 'lifecycle' | 'launch' | 'retained' | 'ownership' | 'failure' | 'unusable' | 'fatal' | 'active' | 'marker' | 'marker-path' | 'budget' | 'tail';
type Boundary = 'create' | 'inspection' | 'wait' | 'restore';
interface Replay {
  name: string;
  success?: boolean;
  publicEntry?: boolean;
  chrome?: boolean;
  before?: string;
  beforeStderr?: string;
  after?: string;
  afterStderr?: string;
  failCommand?: 'inspection' | 'wait' | 'restore';
  response?: 'crash' | 'different-deadline' | 'status' | 'invalid-json' | 'timeout' | 'interrupted' | 'http' | 'path' | 'method' | 'timed-out' | 'different-error' | 'missing-signature';
  fence?: Fence;
  boundary?: Boundary;
  cap?: boolean;
  repeated?: boolean;
  twice?: boolean;
}

async function replay(test: Replay): Promise<void> {
  const recorded = JSON.parse(await readFile(join(import.meta.dirname, 'fixtures/android-startup-reverse-loss.json'), 'utf8'));
  const root = await mkdtemp(join(tmpdir(), 'android-reverse-loss-'));
  const saved = { adb: process.env.ADB, marker: process.env.MOBILE_DEVICE_OWNERSHIP_FILE };
  const diagnostics = new DiagnosticRecorder();
  let now = 0;
  const budget = new PhaseBudget('setup-reverse-loss', { timeoutMs: 300_000, recoveryLimit: test.cap ? 0 : 4, now: () => now });
  const originalDeadline = budget.snapshot().deadline;
  let creates = 0;
  let original: unknown;
  let fenced = false;
  const fence = (): void => {
    fenced = true;
    const fields: Record<string, unknown> = {
      setup: { setupAttempted: false }, lifecycle: { lifecycleActive: false }, launch: { initialLaunchAttempted: true },
      retained: { retainedOwner: {} }, ownership: { ownershipFailure: new Error('ownership fence') }, failure: { lifecycleFailure: new Error('lifecycle fence') },
    };
    if (test.fence && fields[test.fence]) Object.assign(platform, fields[test.fence]);
    if (test.fence === 'marker') unlinkSync(join(root, 'owned'));
    if (test.fence === 'marker-path') process.env.MOBILE_DEVICE_OWNERSHIP_FILE = join(root, 'other-owned');
    if (test.fence === 'budget') now = 300_001;
    if (test.fence === 'tail') now = 300_000 - (test.chrome ? 70_000 : 60_000) + 1;
  };
  const client = new AppiumClient('http://startup.invalid', 30_000, async (_input, init) => {
    if (init?.method === 'DELETE') return Response.json({ value: null });
    creates++;
    if (test.success && (creates % 2 === 0)) return Response.json({ value: { sessionId: `ready-${creates}`, capabilities: {} } });
    if (test.response === 'timeout') throw new DOMException('request timed out', 'TimeoutError');
    if (test.response === 'interrupted') throw new DOMException('request interrupted', 'AbortError');
    if (test.response === 'invalid-json') return new Response('{', { status: 500 });
    const value = { ...recorded.value };
    if (test.response === 'crash') value.message = 'The instrumentation process cannot be initialized. The process crashed.';
    if (test.response === 'different-deadline') value.message = value.message.replace('30000ms', '60000ms');
    if (test.response === 'different-error') value.error = 'session not created';
    if (test.response === 'missing-signature') value.message = 'startup failed';
    return Response.json({ value }, { status: test.response === 'status' ? 404 : recorded.status });
  });
  client.setBudget(budget);
  const snapshot = client.snapshot.bind(client);
  client.snapshot = () => ({ ...snapshot(), ...(fenced && test.fence === 'active' ? { sessionId: '[active]' } : {}),
    ...(fenced && test.fence === 'unusable' ? { unusable: true } : {}),
    ...(fenced && test.fence === 'fatal' ? { firstFatal: { code: 'APPIUM_TIMEOUT', path: '/session', method: 'POST', at: 0, detail: 'prior fatal' } } : {}),
  });
  const create = client.create.bind(client);
  client.create = async options => {
    try {
      if (['http', 'path', 'method', 'timed-out'].includes(test.response || '')) {
        creates++;
        throw new WebDriverError({ code: test.response === 'http' ? 'APPIUM_HTTP' : 'APPIUM_COMMAND', status: 500,
          path: test.response === 'path' ? '/other' : '/session', method: test.response === 'method' ? 'GET' : 'POST',
          timedOut: test.response === 'timed-out', durationMs: 40_413, selectedContext: '', selectedWindow: '',
          message: `HTTP 500: ${JSON.stringify(recorded.value).slice(0, 500)}` });
      }
      return await create(options);
    } catch (error) {
      original = error;
      if (test.boundary === 'create') fence();
      throw error;
    }
  };
  const platform = new AndroidPlatform({ deviceId: 'emulator-5554', origin: 'https://fixture.test', setupUrl: 'https://fixture.test/setup',
    outputDir: root, appiumUrl: 'http://startup.invalid', certificate: '/unused', budget, diagnostics });
  Object.assign(platform, { driver: client, ...(!test.publicEntry ? { setupAttempted: true, lifecycleActive: true } : {}) });
  const setup = platform as unknown as { createSetupSession(options: SessionOptions): Promise<void>; prepareFreshDevice(): Promise<void> };
  const options = { capabilities: test.chrome ? { browserName: 'Chrome' } : {}, requestTimeoutMs: test.chrome ? 70_000 : 60_000, budget };
  if (test.publicEntry) setup.prepareFreshDevice = () => setup.createSetupSession(options);
  const invoke = () => test.publicEntry ? platform.startFreshDevice() : setup.createSetupSession(options);
  const gate = join(root, 'gate');
  const release = join(root, 'release');
  let watcher: ReturnType<typeof setInterval> | undefined;
  try {
    await writeFile(join(root, 'owned'), 'android:emulator-5554');
    await writeFile(join(root, 'other-owned'), 'android:emulator-5554');
    await writeFile(join(root, 'before'), test.before ?? baseline);
    await writeFile(join(root, 'before-stderr'), test.beforeStderr ?? '');
    await writeFile(join(root, 'after'), test.after ?? '');
    await writeFile(join(root, 'after-stderr'), test.afterStderr ?? '');
    await writeFile(join(root, 'adb'), [
      '#!/bin/sh',
      `root='${root}'`,
      'printf "%s\\n" "$*" >> "$root/calls"',
      'stage=',
      'if [ "$3" = reverse ] && [ "$4" = --list ]; then',
      '  if [ -f "$root/listed" ]; then stage=inspection; else stage=baseline; touch "$root/listed"; fi',
      'elif [ "$3" = wait-for-device ]; then stage=wait;',
      'elif [ "$3" = reverse ]; then stage=restore; fi',
      test.boundary && test.boundary !== 'create' ? `if [ "$stage" = '${test.boundary}' ] && [ ! -f "$root/release" ]; then
        printf '%s' "$stage" > "$root/gate.tmp"
        mv "$root/gate.tmp" "$root/gate"
        count=0
        while [ ! -f "$root/release" ] && [ "$count" -lt 200 ]; do sleep 0.01; count=$((count+1)); done
        [ -f "$root/release" ] || exit 2
      fi` : '',
      test.failCommand ? `if [ "$stage" = '${test.failCommand}' ]; then exit 1; fi` : '',
      'if [ "$stage" = baseline ]; then /bin/cat "$root/before"; /bin/cat "$root/before-stderr" >&2; fi',
      'if [ "$stage" = inspection ]; then /bin/cat "$root/after"; /bin/cat "$root/after-stderr" >&2; fi',
      '',
    ].join('\n'), { mode: 0o700 });
    process.env.ADB = join(root, 'adb');
    process.env.MOBILE_DEVICE_OWNERSHIP_FILE = join(root, 'owned');
    if (test.boundary && test.boundary !== 'create') {
      watcher = setInterval(() => {
        if (!existsSync(gate) || existsSync(release)) return;
        assert.equal(readFileSync(gate, 'utf8'), test.boundary);
        fence();
        writeFileSync(release, 'ready');
      }, 1);
    }
    if (test.success) {
      await invoke();
      assert.equal(creates, 2, 'lost reverse proof must admit exactly one fresh create');
      assert.equal(client.snapshot().unusable, false);
      assert.equal(client.snapshot().firstFatal, undefined);
      if (test.twice) {
        await client.close();
        await rm(join(root, 'listed'));
        await setup.createSetupSession(options);
        assert.equal(creates, 4, 'recovery is once per helper invocation, not once globally');
      }
    } else {
      await assert.rejects(invoke(), error => {
        // Recovery failures preserve the actual Appium error, not a replacement inspection/restore error.
        assert.equal(error, original);
        return true;
      });
      assert.equal(creates, test.repeated ? 2 : 1);
      if (test.publicEntry) {
        const terminal = platform as unknown as { lifecycleFailure?: Error; ownershipFailure?: Error };
        assert.ok(terminal.lifecycleFailure, 'public startup must retain its terminal lifecycle fence');
        if (test.fence === 'ownership') assert.equal(terminal.ownershipFailure?.message, 'ownership fence');
        if (test.fence === 'failure') assert.equal(terminal.lifecycleFailure.message, 'lifecycle fence');
        const callsBefore = await readFile(join(root, 'calls'), 'utf8');
        await assert.rejects(platform.startFreshDevice());
        assert.equal(await readFile(join(root, 'calls'), 'utf8'), callsBefore, 'failed public startup must not dispatch again');
        assert.equal(creates, test.repeated ? 2 : 1);
      }
    }
    const calls = (await readFile(join(root, 'calls'), 'utf8')).trim().split('\n');
    assert.ok(calls.every(call => call.startsWith('-s emulator-5554 ')));
    assert.ok(calls.every(call => !call.includes('forward')), 'Appium owns its own forward');
    const inspections = calls.filter(call => call.endsWith('reverse --list')).length - (test.twice ? 2 : 1);
    const restores = calls.filter(call => call.includes('reverse --no-rebind '));
    const waits = calls.filter(call => call.endsWith('wait-for-device'));
    const expectedInspection = test.response || (test.boundary === 'create' && test.fence) || test.before !== undefined || test.beforeStderr ? 0 : test.twice ? 2 : 1;
    assert.equal(inspections, expectedInspection, 'no inspection without exact healthy baseline and signature');
    const recovered = test.success || test.repeated || test.failCommand === 'restore' || test.failCommand === 'wait'
      || (test.boundary && ['wait', 'restore'].includes(test.boundary));
    assert.equal(budget.recoveryCount, recovered ? test.twice ? 2 : 1 : 0);
    assert.equal(waits.length, recovered ? test.twice ? 2 : 1 : 0);
    assert.equal(restores.length, test.success || test.repeated ? test.twice ? 6 : 3 : test.failCommand === 'restore' || test.boundary === 'restore' ? 1 : 0);
    if (test.boundary) assert.ok(fenced, 'await boundary fence exercised');
    if (test.response === 'timeout') assert.equal(client.snapshot().firstFatal?.code, 'APPIUM_TIMEOUT');
    if (!test.response) {
      assert.ok(original instanceof WebDriverError);
      assert.equal(original.message.length, 'APPIUM_COMMAND: HTTP 500: '.length + 500);
      assert.ok(!original.message.endsWith('}'), 'recorded exception detail is truncated, not parseable JSON');
    }
    const refusal = diagnostics.snapshot().filter(event => event.operation === 'adb-recovery-refused');
    const immediate = test.response || (test.boundary === 'create' && ['active', 'unusable', 'fatal'].includes(test.fence || ''));
    assert.equal(refusal.length, test.success || test.repeated || immediate ? 0 : 1, 'secondary recovery failures must be diagnosed');
    assert.equal(budget.snapshot().deadline, originalDeadline);
  } finally {
    if (watcher) clearInterval(watcher);
    if (saved.adb === undefined) delete process.env.ADB; else process.env.ADB = saved.adb;
    if (saved.marker === undefined) delete process.env.MOBILE_DEVICE_OWNERSHIP_FILE; else process.env.MOBILE_DEVICE_OWNERSHIP_FILE = saved.marker;
    await rm(root, { recursive: true, force: true });
  }
}

const cases: Replay[] = [
  { name: 'settings proven reverse loss', success: true },
  { name: 'Chrome proven reverse loss', success: true, chrome: true },
  { name: 'one recovery per invocation', success: true, twice: true },
  { name: 'repeated create failure', repeated: true },
  { name: 'recovery cap', cap: true },
  ...(['crash', 'different-deadline', 'status', 'invalid-json', 'timeout', 'interrupted', 'http', 'path', 'method', 'timed-out', 'different-error', 'missing-signature'] as const).map(response => ({ name: response, response })),
  ...Object.entries({ empty: '', whitespace: '\n ', malformed: `${baseline}bad row\n`, duplicate: `${baseline}host-17 tcp:44323 tcp:44323\n`,
    conflict: `${baseline}host-17 tcp:44323 tcp:12\n`, 'duplicate-local': `${baseline}host-17 tcp:12 tcp:44323\n`,
    'invalid-port': 'host-17 tcp:65536 tcp:1\n', 'zero-port': 'host-17 tcp:0 tcp:1\n',
    'mixed-owner': `${baseline}host-18 tcp:12 tcp:12\n`, 'non-tcp': `${baseline}host-17 localabstract:socket tcp:12\n`,
  }).map(([name, before]) => ({ name: `baseline ${name}`, before })),
  { name: 'baseline stderr', beforeStderr: 'adb warning' },
  ...Object.entries({ unchanged: baseline, partial: 'host-17 tcp:44323 tcp:44323\n', conflict: 'host-17 tcp:44323 tcp:1\n', malformed: 'unrecognized output', whitespace: '\n' })
    .map(([name, after]) => ({ name: `post ${name}`, after })),
  { name: 'post stderr', afterStderr: 'adb warning' },
  ...(['inspection', 'wait', 'restore'] as const).map(failCommand => ({ name: `${failCommand} failure`, failCommand })),
  ...(['create', 'inspection', 'wait', 'restore'] as const).flatMap(boundary => (
    ['setup', 'lifecycle', 'launch', 'retained', 'ownership', 'failure', 'unusable', 'fatal', 'active', 'marker', 'marker-path', 'budget', 'tail'] as const
  ).map(fence => ({ name: `${boundary} ${fence} fence`, boundary, fence }))),
  { name: 'Chrome normal replacement allowance', boundary: 'inspection', fence: 'tail', chrome: true },
  { name: 'public settings proven reverse loss', success: true, publicEntry: true },
  { name: 'public Chrome proven reverse loss', success: true, chrome: true, publicEntry: true },
  ...(['create', 'inspection', 'wait', 'restore'] as const).flatMap(boundary => (
    ['budget', 'tail', 'ownership', 'failure', 'active', 'unusable', 'fatal'] as const
  ).map(fence => ({ name: `public ${boundary} ${fence} fence`, boundary, fence, publicEntry: true }))),
];

export const androidSetupReverseLossTests: Array<[string, () => Promise<void>]> = cases.map(test => [
  `Native startup Android reverse loss ${test.name}`, () => replay(test),
]);
