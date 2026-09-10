import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { repositoryPath, repositoryRoot } from '../support/paths';
import { fileSha256 } from '../support/artifacts';
import type { AndroidEnvironmentSnapshot, AndroidPreparation } from '../android-environment';
import { AndroidEnvironmentMeasurement } from '../android-measurement';
import { AndroidPlatform } from '../platforms/android';
import { AppiumClient } from '../support/webdriver';

interface Fixture {
  root: string;
  fixtureDirectory: string;
  log: string;
  environment: NodeJS.ProcessEnv;
}
interface Harness {
  createFixture(): Promise<Fixture>;
  writeState(directory: string, state: string): Promise<void>;
  snapshot(fixture: Fixture, state: string, output: string, diagnostics: string, timeoutMs?: number): Promise<{ passed: boolean; stderr: string }>;
  check(fixture: Fixture, before: string, after: string, log?: string): Promise<{ passed: boolean; issues: string[] }>;
}
type Test = [string, () => Promise<void>];
const chromeDeath = '09-10 08:45:09.613 546 1761 I ActivityManager: Process com.android.chrome (pid 6538) has died: fg TOP\n';
const benign = '09-10 08:45:09.000 1208 7311 W PlatformConfigurator: \tat com.google.android.gms.platformconfigurator.PhenotypeConfigurationUpdateListener.onHandleIntent(:com.google.android.gms@242335041@24.23.35:207)\n';
const marker = (time: string, message: string) => `09-10 08:45:${time} 2000 2000 I HerdrMeasure: android-test ${message}\n`;

function cli(fixture: Fixture, args: string[]): { passed: boolean; stderr: string } {
  try {
    execFileSync(process.execPath, [process.env.ANDROID_ENVIRONMENT_SOURCE || repositoryPath('tests/mobile/android-environment.ts'), ...args], {
      cwd: repositoryRoot, env: fixture.environment, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
    });
    return { passed: true, stderr: '' };
  } catch (error) {
    return { passed: false, stderr: String((error as { stderr?: string }).stderr || error) };
  }
}

export function androidEnvironmentTests(harness: Harness): Test[] {
  const tests: Test[] = [];
  const test = (name: string, body: () => Promise<void>) => tests.push([`Android production CLI ${name}`, body]);
  const prepare = (fixture: Fixture) => cli(fixture, ['prepare', '--serial', 'emulator-5554', '--toolchains', process.env.ANDROID_ENVIRONMENT_TOOLCHAINS || repositoryPath('tests/mobile/toolchains.json'), '--output', join(fixture.root, 'preparation.json'), '--adb-timeout-ms', '1000']);
  const state = (fixture: Fixture, value: unknown) => writeFile(join(fixture.fixtureDirectory, 'valid-vending.json'), JSON.stringify(value));
  const snapshots = async (fixture: Fixture) => {
    const before = join(fixture.root, 'before.json');
    const after = join(fixture.root, 'after.json');
    for (const output of [before, after]) {
      const result = await harness.snapshot(fixture, 'valid', output, output.replace('.json', '-diagnostics.json'));
      assert.equal(result.passed, true, result.stderr);
    }
    return { before, after };
  };
  for (const [name, vending, mutations] of [
    ['absent', { absent: true }, 0], ['disabled ordinary-listed', { enabled: 3 }, 0],
    ['default preparation', { enabled: 0 }, 1], ['enabled preparation', { enabled: 1 }, 1],
    ['component-only preparation', { enabled: 0, components: true }, 1],
    ['absent for user 0 but package remains', { installed: false, enabled: 0 }, 0],
  ] as const) test(`prepare, independent readback, snapshots and persistence: ${name}`, async () => {
    const fixture = await harness.createFixture();
    await state(fixture, vending);
    const result = prepare(fixture);
    assert.equal(result.passed, true, result.stderr);
    const observation = JSON.parse(await readFile(join(fixture.root, 'preparation.json'), 'utf8')) as AndroidPreparation;
    assert.ok(observation.provenance.avdConfig && observation.provenance.sdkRevision && observation.system['ro.build.fingerprint']);
    assert.equal(observation.mutation, mutations ? 'disable-user-0' : 'none');
    const requests = await readFile(fixture.log, 'utf8');
    assert.equal(requests.split('\n').filter((line) => line.includes('disable-user')).length, mutations);
    if (mutations) {
      assert.ok(requests.indexOf('dumpsys package com.android.vending') < requests.indexOf('disable-user'));
      assert.ok(requests.lastIndexOf('dumpsys package com.android.vending') > requests.indexOf('disable-user'));
    }
    const { before, after } = await snapshots(fixture);
    const persisted = JSON.parse(await readFile(before, 'utf8')) as AndroidEnvironmentSnapshot;
    assert.equal(persisted.vending.presence, observation.after?.presence);
    assert.equal(persisted.vending.ordinaryListed, observation.after?.presence === 'installed');
    assert.equal((await harness.check(fixture, before, after)).passed, true);
    assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    assert.equal(requests.includes('disable-user --user 0 com.google.android.gms'), false);
  });
  test('headless version acquisition avoids the GUI runtime and preserves measured identity', async () => {
    const fixture = await harness.createFixture();
    assert.throws(() => execFileSync('emulator', ['-version'], { env: fixture.environment, stdio: 'pipe' }), /libpulse.so.0/u);
    assert.equal(execFileSync('emulator', ['-no-window', '-version'], { env: fixture.environment, encoding: 'utf8' }).trim(), 'Android emulator version 35.0.2.0');
    const { before, after } = await snapshots(fixture);
    assert.equal((JSON.parse(await readFile(before, 'utf8')) as AndroidEnvironmentSnapshot).emulatorVersion, 'Android emulator version 35.0.2.0');
    assert.equal((await harness.check(fixture, before, after)).passed, true);
    await writeFile(join(fixture.root, 'bin', 'emulator'), '#!/bin/sh\nexit 127\n');
    const output = join(fixture.root, 'failed-version.json');
    assert.equal((await harness.snapshot(fixture, 'valid', output, join(fixture.root, 'failed-version-diagnostics.json'))).passed, false);
    assert.equal(existsSync(output), false);
  });
  test('acquires only eight named identity properties despite legal ambiguous dump records beyond preview', async () => {
    const fixture = await harness.createFixture();
    const dump = await readFile(join(fixture.fixtureDirectory, 'getprop'), 'utf8');
    assert.ok(dump.indexOf('[source-derived]') > 4000);
    assert.ok(dump.includes('first]\n[ro.synthetic.other]: [second'));
    assert.equal(prepare(fixture).passed, true);
    const preparation = JSON.parse(await readFile(join(fixture.root, 'preparation.json'), 'utf8')) as AndroidPreparation;
    const { before, after } = await snapshots(fixture);
    const snapshot = JSON.parse(await readFile(before, 'utf8')) as AndroidEnvironmentSnapshot;
    assert.deepEqual(snapshot.system, preparation.system);
    assert.equal(Object.keys(snapshot.system).length, 7);
    const requests = (await readFile(fixture.log, 'utf8')).trim().split('\n');
    const properties = requests.filter((line) => line.includes('getprop'));
    assert.equal(properties.length, 8);
    assert.ok(properties.includes('-s emulator-5554 shell getprop ro.kernel.qemu'));
    assert.ok(properties.every((line) => /^-s emulator-5554 shell getprop ro\.[a-z.]+$/u.test(line)));
    const diagnostics = JSON.parse(await readFile(after.replace('.json', '-diagnostics.json'), 'utf8'));
    assert.ok(diagnostics.commands.filter((entry: { args: string[] }) => entry.args.includes('getprop')).every((entry: { stdoutPreview?: string }) => entry.stdoutPreview === undefined));
    assert.equal((await harness.check(fixture, before, after)).passed, true);
  });
  for (const [name, value] of [
    ['empty', ''], ['missing', '\n'], ['truncated', 'fixture'], ['multiline', 'first\nsecond\n'],
    ['framed dump', '[ro.build.id]: [AP4A]\n[ro.build.id]: [AP4A]\n'],
    ['oversized', 'x'.repeat(4096) + '\n'], ['control', 'fixture\u0000\n'], ['padded', ' fixture\n'],
  ]) test(`rejects ${name} required property in preparation and snapshot without mutation`, async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { enabled: 0, propertyResponses: { 'ro.build.id': value } });
    const preparation = prepare(fixture);
    assert.equal(preparation.passed, false, name);
    assert.match(preparation.stderr, /required system property ro.build.id/u);
    assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    const snapshot = await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'));
    assert.equal(snapshot.passed, false, name);
    assert.match(snapshot.stderr, /required system property ro.build.id/u);
    assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
  });
  for (const [name, value] of [['ro.build.version.sdk', '34\n'], ['ro.kernel.qemu', '0\n']]) {
    test(`rejects wrong ${name} before preparation mutation and during snapshot`, async () => {
      const fixture = await harness.createFixture();
      await state(fixture, { enabled: 0, propertyResponses: { [name]: value } });
      assert.equal(prepare(fixture).passed, false);
      assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
      assert.equal((await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'))).passed, false);
      assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    });
  }
  for (const failure of ['propertyTimeout', 'propertyFailure', 'propertyStderr']) {
    test(`rejects named acquisition ${failure} before any preparation mutation and snapshot`, async () => {
      const fixture = await harness.createFixture();
      await state(fixture, { enabled: 0, [failure]: 'ro.build.id' });
      assert.equal(prepare(fixture).passed, false);
      assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
      assert.equal((await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'), 1000)).passed, false);
      assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
      const diagnostics = JSON.parse(await readFile(join(fixture.root, 'diagnostics.json'), 'utf8'));
      assert.equal(diagnostics.failure.stage, 'read required system property ro.build.id');
      if (failure === 'propertyTimeout') assert.equal(diagnostics.failure.timedOut, true);
    });
  }
  test('requires every persisted identity field and qemu before preparation mutation', async () => {
    const keys = ['ro.build.fingerprint', 'ro.build.id', 'ro.build.version.incremental', 'ro.build.version.release', 'ro.build.version.sdk', 'ro.product.name', 'ro.product.device', 'ro.kernel.qemu'];
    for (const key of keys) {
      const fixture = await harness.createFixture();
      await state(fixture, { enabled: 0, propertyResponses: { [key]: '\n' } });
      assert.equal(prepare(fixture).passed, false, key);
      assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false, key);
    }
  });
  test('independent preparation readback rejects changed system identity without repair', async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { enabled: 0, propertyChangeOnDisable: { 'ro.build.id': 'CHANGED\n' } });
    const result = prepare(fixture);
    assert.equal(result.passed, false);
    assert.match(result.stderr, /preparation provenance changed/u);
    assert.equal((await readFile(fixture.log, 'utf8')).split('\n').filter((line) => line.includes('disable-user')).length, 1);
  });
  test('rejects changed named system identity after measurement', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    await state(fixture, { absent: true, propertyResponses: { 'ro.build.id': 'CHANGED\n' } });
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'diagnostics.json'))).passed, true);
    assert.equal((await harness.check(fixture, before, after)).passed, false);
  });
  for (const enabled of [2, 4]) test(`review: shell denies preparation from enabled=${enabled} without additional mutation`, async () => {
    const fixture = await harness.createFixture();
    const value = { enabled };
    await state(fixture, value);
    const result = prepare(fixture);
    assert.equal(result.passed, false, result.stderr);
    assert.match(result.stderr, /Shell cannot change component state/u);
    assert.deepEqual(JSON.parse(await readFile(join(fixture.fixtureDirectory, 'valid-vending.json'), 'utf8')), value);
    const preparation = JSON.parse(await readFile(join(fixture.root, 'preparation.json'), 'utf8')) as AndroidPreparation;
    assert.equal(preparation.before.identity?.enabled, String(enabled));
    assert.equal(preparation.mutation, 'disable-user-0');
    assert.equal(preparation.after, undefined);
    const requests = (await readFile(fixture.log, 'utf8')).trim().split('\n');
    assert.equal(requests.filter((line) => /disable-user/u.test(line)).length, 1);
    assert.equal(requests.at(-1), '-s emulator-5554 shell pm disable-user --user 0 com.android.vending');
    assert.equal(requests.some((line) => /uninstall|enable |disable-user.*gms|root|remount/u.test(line)), false);
    const diagnostics = JSON.parse(await readFile(join(fixture.root, 'preparation-diagnostics.json'), 'utf8'));
    assert.equal(diagnostics.failure.exitCode, 1);
    assert.equal(diagnostics.failure.stage, 'disable Vending for owned emulator user 0');
  });
  for (const [name, vending] of [
    ['wrong foreground user', { foregroundUser: 10 }], ['wrong AVD', { avd: 'unowned-avd' }],
    ['wrong package user', { user: 10 }], ['malformed enabled state', { enabled: 9 }],
    ['malformed listing', { list: 'Failure [denied]\n' }], ['truncated listing', { list: 'package:com.android.vending' }],
    ['substring is not absence', { list: 'package:com.android.vending.other\n' }],
    ['empty dump is not absence', { dump: '' }], ['unknown dump is not absence', { dump: 'Unknown package\n' }],
    ['failed disable command', { disableFail: true }], ['failed disable readback', { readbackFail: true }],
    ['false disable response', { disableOutput: 'Success\n' }],
  ] as const) test(`rejects preparation ${name}`, async () => {
    const fixture = await harness.createFixture();
    await state(fixture, vending);
    const result = prepare(fixture);
    assert.equal(result.passed, false, name);
    const allowed = name.includes('disable');
    assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), allowed, result.stderr);
  });
  test('refuses missing ownership, wrong SDK and ambiguous AVD config before mutation', async () => {
    for (const change of ['ownership', 'sdk', 'avd']) {
      const fixture = await harness.createFixture();
      await state(fixture, { enabled: 0 });
      if (change === 'ownership') await writeFile(join(fixture.root, 'ownership'), 'android:emulator-5556\n');
      if (change === 'sdk') await writeFile(join(fixture.root, 'sdk/system-images/android-35/google_apis/x86_64/source.properties'), 'Pkg.Revision=\n');
      if (change === 'avd') await writeFile(join(fixture.root, 'avd/herdr-mobile-ci-fixture.avd/config.ini'), 'image.sysdir.1=wrong\nimage.sysdir.1=also-wrong\n');
      assert.equal(prepare(fixture).passed, false);
      assert.equal(existsSync(fixture.log) && (await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    }
  });
  for (const enabled of [0, 1, 2, 4]) test(`snapshot refuses Vending state ${enabled} without repair`, async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { enabled });
    const result = await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'));
    assert.equal(result.passed, false);
    assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
  });
  for (const [name, changed] of [['version', { version: '456' }], ['path', { path: '/product/priv-app/Changed' }], ['presence', { absent: true }]] as const) {
    test(`post-run Vending ${name} changes fail without repair`, async () => {
      const fixture = await harness.createFixture();
      await state(fixture, { enabled: 3 });
      const { before, after } = await snapshots(fixture);
      await state(fixture, { enabled: 3, ...changed });
      const result = await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'after-diagnostics.json'));
      assert.equal(result.passed, true, result.stderr);
      const check = await harness.check(fixture, before, after);
      assert.equal(check.passed, false);
      assert.match(check.issues.join(';'), /Vending/u);
      assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    });
  }
  for (const versionName of ['24.23.35 (190800-646585959)', ' 24.23.35 (190800-646585959)  ', 'release candidate\tβ']) {
    test(`review: preserves and compares complete source-derived versionName ${JSON.stringify(versionName)}`, async () => {
      const fixture = await harness.createFixture();
      const path = join(fixture.fixtureDirectory, 'valid-gms.dump');
      const original = await readFile(path, 'utf8');
      await writeFile(path, original.replace('versionName=24.23.35\n', `versionName=${versionName}\n`));
      const { before, after } = await snapshots(fixture);
      const identity = (JSON.parse(await readFile(before, 'utf8')) as AndroidEnvironmentSnapshot).packages['com.google.android.gms'];
      assert.equal(identity.versionName, versionName);
      assert.equal((await harness.check(fixture, before, after)).passed, true);
      await writeFile(path, original.replace('versionName=24.23.35\n', `versionName=${versionName} changed\n`));
      const result = await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'after-diagnostics.json'));
      assert.equal(result.passed, true, result.stderr);
      const checked = await harness.check(fixture, before, after);
      assert.equal(checked.passed, false);
      assert.ok(checked.issues.includes('com.google.android.gms versionName changed'));
      assert.ok(checked.issues.includes('com.google.android.gms identitySha256 changed'));
    });
  }
  for (const [name, mutate] of [
    ['empty version', (text: string) => text.replace('versionName=24.23.35', 'versionName=')],
    ['missing version', (text: string) => text.replace('    versionName=24.23.35\n', '')],
    ['duplicate version', (text: string) => text.replace('    versionName=24.23.35', '    versionName=24.23.35\n    versionName=24.23.35')],
    ['missing user field', (text: string) => text.replace('installed=true ', '')],
    ['duplicate user field', (text: string) => text.replace('installed=true ', 'installed=true installed=true ')],
    ['missing firstInstallTime', (text: string) => text.replace('      firstInstallTime=2026-01-01 00:00:00\n', '')],
    ['missing flags', (text: string) => text.replace('    flags=[ SYSTEM HAS_CODE ]\n', '')],
    ['ambiguous user record', (text: string) => text.replace('Queries:', '    User 0: installed=true hidden=false suspended=false enabled=0\n      firstInstallTime=2026-01-01 00:00:00\nQueries:')],
    ['malformed dependency values', (text: string) => text.replace('    flags=', '    usesLibraryFiles:\n      missing-absolute-path\n    flags=')],
    ['truncated dump', (text: string) => text.trimEnd()],
    ['missing dump footer', (text: string) => text.split('Queries:')[0]],
    ['oversized dump', (text: string) => text + 'x'.repeat(2_000_001)],
    ['duplicate active record', (text: string) => text + text],
    ['hidden only', (text: string) => text.replace('Packages:', 'Hidden system packages:')],
  ] as const) test(`rejects ${name} through snapshot`, async () => {
    const fixture = await harness.createFixture();
    const path = join(fixture.fixtureDirectory, 'valid-gms.dump');
    await writeFile(path, mutate(await readFile(path, 'utf8')));
    const result = await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'));
    assert.equal(result.passed, false, name);
  });
  test('ignores resolver/hidden/object-ID/order noise but compares all dependency section values beyond 500 lines', async () => {
    const fixture = await harness.createFixture();
    const path = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const original = await readFile(path, 'utf8');
    const values = Array.from({ length: 650 }, (_, index) => `      /system/framework/library-${index}.jar\n`).join('');
    const expanded = original.replace('    flags=', `    usesLibraryFiles:\n${values}    flags=`);
    await writeFile(path, expanded);
    const { before, after } = await snapshots(fixture);
    const irrelevant = `Activity Resolver Table:\n  module config object-id=changed\n${expanded.replace('(fixture)', '(different)').replace(values, values.trimEnd().split('\n').reverse().join('\n') + '\n')}Hidden system packages:\n  Package [com.google.android.gms] (old):\n    versionName=wrong\n`;
    await writeFile(path, irrelevant);
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'after-diagnostics.json'))).passed, true);
    assert.deepEqual((await harness.check(fixture, before, after)).issues, []);
    await writeFile(path, irrelevant.replace('library-649.jar', 'library-649-changed.jar'));
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'after-diagnostics.json'))).passed, true);
    assert.equal((await harness.check(fixture, before, after)).passed, false);
  });
  test('detects a dependency change beyond the old 500 matching-line cap independently of resolver noise', async () => {
    const fixture = await harness.createFixture();
    const path = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const original = await readFile(path, 'utf8');
    const values = Array.from({ length: 650 }, (_, index) => `      /data/app/module-${index}/base.apk\n`).join('');
    const expanded = original.replace('    flags=', `    usesLibraryFiles:\n${values}    flags=`);
    await writeFile(path, expanded);
    const { before, after } = await snapshots(fixture);
    await writeFile(path, expanded.replace('module-649/', 'module-649-changed/'));
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'after-diagnostics.json'))).passed, true);
    assert.equal((await harness.check(fixture, before, after)).passed, false);
  });
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
      const result = cli(fixture, ['check', '--before', before, '--after', after, '--log', logFile, '--operations', operations, '--output', output]);
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
    assert.deepEqual(changed.report.issues, ['native dependency replacement or forced restart was observed']);
    const unrelated = module.replace('1427', '1486');
    assert.equal((await check(death + recordedStart + unrelated + recordedEnd)).passed, true);
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
      const result = cli(fixture, ['check', '--before', before, '--after', after, '--log', log, '--operations', operations, '--output', join(fixture.root, 'check.json')]);
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
  test('missing/unreadable logs, missing/duplicate markers and setup-only events fail closed or remain outside measurement', async () => {
    const fixture = await harness.createFixture();
    const { before, after } = await snapshots(fixture);
    const operations = join(fixture.root, 'operations.json');
    await writeFile(operations, '[]');
    const check = (log: string) => cli(fixture, ['check', '--before', before, '--after', after, '--log', log, '--operations', operations, '--output', join(fixture.root, 'check.json')]);
    for (const path of [join(fixture.root, 'missing.log'), fixture.root]) assert.equal(check(path).passed, false);
    const logFile = join(fixture.root, 'interval.log');
    for (const log of ['', benign, marker('00.000', 'START') + marker('00.100', 'START') + marker('30.000', 'END')]) {
      await writeFile(logFile, log);
      assert.equal(check(logFile).passed, false);
    }
    await writeFile(logFile, chromeDeath + marker('10.000', 'START') + marker('30.000', 'END'));
    assert.equal(check(logFile).passed, true);
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
    const check = () => cli(fixture, ['check', '--before', before, '--after', after, '--log', logFile, '--operations', operationsFile, '--output', join(fixture.root, 'check.json')]);
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
        const result = cli(fixture, ['check', '--before', before, '--after', after, '--log', logFile, '--operations', operationsFile, '--output', join(fixture.root, 'check.json')]);
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
        assert.match(result.stderr, /sanitized|source path does not match/u);
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
      await measurement.begin();
      await assert.rejects(measurement.begin(), /rebaseline/u);
      const baseline = JSON.parse(await readFile(join(fixture.root, 'android-environment-before.json'), 'utf8'));
      assert.equal(baseline.measurement.processes['6700'], undefined);
      await state(fixture, { ...initial, children: { '6700': childName } });
      await assert.rejects(measurement.terminate(packageName, '7777'), /PID changed/u);
      const driver = new AppiumClient('http://android-protocol.test', 30_000, async (input) => new Response(JSON.stringify({
        value: new URL(String(input)).pathname === '/session' ? { sessionId: 'android-measurement' } : null,
      }), { status: 200 }));
      await driver.create({ capabilities: {} });
      const platform = new AndroidPlatform({ origin: 'https://fixture.test', appiumUrl: 'http://android-protocol.test', outputDir: fixture.root, certificate: '', setupUrl: '', deviceId: 'emulator-5554' });
      (platform as any).driver = driver;
      (platform as any).installedPackage = packageName;
      platform.environmentMeasurement = measurement;
      await platform.terminateInstalledApp();
      await driver.close();
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
      const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
      try {
        await measurement.begin();
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
          await assert.rejects(measurement.finish(), /measurement failed/u);
          const check = JSON.parse(await readFile(join(fixture.root, 'android-environment-check.json'), 'utf8'));
          assert.equal(check.passed, false);
          assert.ok(check.forcedRestartEvents.some((line: string) => line.includes(variant === 'unobserved child' ? '6701' : '6700')));
          return;
        }
        await assert.rejects(measurement.terminate('com.android.chrome', '6538'));
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
  test('measurement collector failure is fatal and cannot leave an active descendant', async () => {
    const fixture = await harness.createFixture();
    await state(fixture, { absent: true, logcatFail: true });
    const saved = { ...process.env };
    Object.assign(process.env, fixture.environment);
    const measurement = new AndroidEnvironmentMeasurement('emulator-5554', fixture.root, repositoryPath('tests/mobile/toolchains.json'));
    try {
      await assert.rejects(measurement.begin(), /collector/u);
      await assert.rejects(measurement.finish(), /no active measurement/u);
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
    assert.match(check.issues.join(';'), /evidence unavailable/u);
  });
  test('Android-only declarations and both setup paths preserve policy, pinning and read-only postchecks', async () => {
    const policy = JSON.parse(await readFile(repositoryPath('tests/mobile/toolchains.json'), 'utf8')).android;
    assert.equal(policy.vendingPolicy, 'absent-or-disabled-user-0');
    assert.equal(policy.systemImagePolicy, 'owned-google-apis-emulator');
    assert.equal('playStore' in policy, false);
    assert.equal(policy.browserSha256, '261439a1ed20090f9f2f9aeef64024c1cfb9f242d4770f8f7c8f2e777843a35a');
    assert.equal(policy.trichromeLibrarySha256, 'f7d82fa76a99f13980c4205484f4ae78742755d2e9b7276fd60c20e3cd7f9090');
    for (const path of ['.github/workflows/mobile-ci.yml', '.github/actions/mobile-device-run/action.yml']) {
      const source = await readFile(repositoryPath(path), 'utf8');
      assert.match(source, /android-environment\.ts prepare/u);
      assert.doesNotMatch(source, /android-environment\.ts snapshot|logcat -c|google-apis-without-play-store/u);
      assert.match(source, /--operations "\$MOBILE_OUTPUT\/android-environment-operations\.json"/u);
      assert.match(source, /apksigner verify --verbose --print-certs/u);
      assert.match(source, /sha256sum --check --status/u);
      const post = source.slice(source.indexOf('name: Verify Android environment stability'), source.indexOf('name: Sanitize bounded diagnostics'));
      assert.doesNotMatch(post, /disable-user|adb |prepare/u);
    }
  });
  return tests;
}
