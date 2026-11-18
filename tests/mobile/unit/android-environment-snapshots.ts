import { execFileSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { type AndroidEnvironmentSnapshot, type AndroidPreparation } from '../android-environment';
import assert from 'node:assert/strict';
import { androidCliTestRegistry, type Harness, type Test, androidEnvironmentCommands, type FakeAdbDiagnostic, validateFakeAdbDiagnostic } from './android-environment';

export function androidEnvironmentSnapshotTests(harness: Harness): Test[] {
  const { tests, test } = androidCliTestRegistry();
  const { prepare, state, snapshots } = androidEnvironmentCommands(harness);
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
  test('smoke53 stable package versions and paths do not excuse normalized GMS component drift', async () => {
    // Normalized delta from run 35296215928, environment-delta.json (not a
    // device dump). Unchanged members are synthetic padding to the observed
    // 186 -> 245 enabled / 80 -> 77 disabled counts; added/removed names are observed.
    const enabledAdded = [
      'com.google.android.gms.family.v2.create.FamilyCreationActivity',
      'com.google.android.gms.family.v2.invites.SendInvitationsActivity',
      'com.google.android.gms.family.v2.manage.DeleteMemberActivity',
      'com.google.android.gms.family.v2.manage.FamilyManagementActivity',
      'com.google.android.gms.family.v2.tos.TosActivity',
      'com.google.android.gms.family.webview.FamilyWebViewActivity',
      'com.google.android.gms.findmydevice.spot.locationreporting.taptoid.TapToIdHalfSheetActivity',
      'com.google.android.gms.fonts.provider.FontsProvider',
      'com.google.android.gms.fonts.update.UpdateSchedulerService',
      'com.google.android.gms.googlehelp.GcmBroadcastReceiver',
      'com.google.android.gms.googlehelp.gcm.InvalidateGcmTokenGcmTaskService',
      'com.google.android.gms.googlehelp.helpactivities.OpenHelpRtcActivity',
      'com.google.android.gms.googlehelp.webview.GoogleHelpRenderingApiWebViewActivity',
      'com.google.android.gms.googlehelp.webview.GoogleHelpSupportWebViewActivity',
      'com.google.android.gms.growth.featuredrops.activity.FeatureDropsActivity',
      'com.google.android.gms.growth.featuredrops.activity.FeatureDropsProofingActivity',
      'com.google.android.gms.growth.notifications.GcmBroadcastReceiver',
      'com.google.android.gms.growth.notifications.NotificationActionActivity',
      'com.google.android.gms.growth.surveys.activity.GmsSurveyActivity',
      'com.google.android.gms.growth.ui.GrowthDebugActivity',
      'com.google.android.gms.growth.ui.webview.GrowthWebViewActivity',
      'com.google.android.gms.ipa.base.IpaGcmTaskService',
      'com.google.android.gms.languageprofile.GcmReceiverService',
      'com.google.android.gms.languageprofile.GcmTaskService',
      'com.google.android.gms.lockbox.service.LockboxBrokerService',
      'com.google.android.gms.octarine.ui.OctarineWebviewActivity',
      'com.google.android.gms.pay.deeplink.AliasSavePkPassActivity',
      'com.google.android.gms.pay.deeplink.AliasSaveSmartHealthCardActivity',
      'com.google.android.gms.people.sync.coreui.ContactsSyncCoreActivity',
      'com.google.android.gms.recaptcha.RecaptchaActivity',
      'com.google.android.gms.romanesco.settings.ContactsRestoreContactsActivity',
      'com.google.android.gms.romanesco.settings.ContactsRestoreDialogActivity',
      'com.google.android.gms.romanesco.settings.ContactsRestoreSettingsActivity',
      'com.google.android.gms.security.provider.SecurityProvider',
      'com.google.android.gms.security.recaptcha.RecaptchaActivity',
      'com.google.android.gms.security.settings.VerifyAppsSettingsActivity',
      'com.google.android.gms.semanticlocationhistory.service.OnDeviceSettingsInjectorService',
      'com.google.android.gms.semanticlocationhistory.settings.OnDeviceSettingsActivity',
      'com.google.android.gms.setupservices.GoogleServicesActivity',
      'com.google.android.gms.smartdevice.d2d.ui.ForwardingActivity',
      'com.google.android.gms.smartdevice.d2d.ui.TargetDirectTransferActivity',
      'com.google.android.gms.smartdevice.magicwand.MagicWandActivity',
      'com.google.android.gms.smartdevice.setup.ui.AccountChallengeActivity',
      'com.google.android.gms.trustlet.place.ui.TrustedPlacesSettingsActivity',
      'com.google.android.gms.udc.gcm.GcmBroadcastReceiver',
      'com.google.android.gms.udc.service.UdcContextInitService',
      'com.google.android.gms.udc.ui.AuthenticatingWebViewActivity',
      'com.google.android.gms.udc.ui.UdcSettingsListActivity',
      'com.google.android.gms.update.OtaSuggestionSummaryProvider',
      'com.google.android.gms.update.SystemUpdateActivity',
      'com.google.android.gms.update.SystemUpdateV2Activity',
      'com.google.android.gms.update.UpdateFromSdCardActivity',
      'com.google.android.gms.vision.DependencyBroadcastReceiverProxy',
      'com.google.android.gms.wallet.ocr.CardRecognitionShimProxyActivity',
      'com.google.android.personalsafety.settings.BleTagPlatformSettingsActivity',
      'com.google.android.personalsafety.settings.BleTagSettingsActivity',
      'com.google.firebase.auth.api.gms.service.FirebaseAuthService',
      'com.google.firebase.auth.api.gms.ui.BrowserSignInResponseHandlerActivity',
      'com.google.firebase.auth.api.gms.ui.BrowserSignInStarterActivity',
    ];
    const disabledRemoved = [
      'com.google.android.gms.pay.deeplink.AliasSaveSmartHealthCardActivity',
      'com.google.android.gms.semanticlocationhistory.service.OnDeviceSettingsInjectorService',
      'com.google.android.gms.semanticlocationhistory.settings.OnDeviceSettingsActivity',
      'com.google.android.personalsafety.settings.BleTagPlatformSettingsActivity',
    ];
    const disabledAdded = ['com.google.android.gms.findmydevice.spot.suw.SetupWizardActivity'];
    const enabled = Array.from({ length: 186 }, (_, index) => `fixture.gms.Enabled${index}`);
    const disabled = Array.from({ length: 76 }, (_, index) => `fixture.gms.Disabled${index}`);
    const fixture = await harness.createFixture();
    const path = join(fixture.fixtureDirectory, 'valid-gms.dump');
    const original = await readFile(path, 'utf8');
    const dump = (enabled: string[], disabled: string[]) => original.replace('Queries:',
      `      enabledComponents:\n${enabled.map(name => `        ${name}\n`).join('')}      disabledComponents:\n${disabled.map(name => `        ${name}\n`).join('')}Queries:`);
    await writeFile(path, dump(enabled, [...disabled, ...disabledRemoved]));
    const { before, after } = await snapshots(fixture);
    const identity = async (path: string) => (JSON.parse(await readFile(path, 'utf8')) as AndroidEnvironmentSnapshot).packages['com.google.android.gms'];
    const first = await identity(before);
    assert.equal(first.dependencyConfig.enabledComponents.length, 186);
    assert.equal(first.dependencyConfig.disabledComponents.length, 80);
    // Reorder sections' members and change an object ID: dump hash changes,
    // normalized component identity must not. No new baseline is taken.
    await writeFile(path, dump([...enabled].reverse(), [...disabled, ...disabledRemoved].reverse()).replace('(fixture)', '(noise)'));
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'noise-diagnostics.json'))).passed, true);
    const noise = await identity(after);
    assert.notEqual(noise.dumpSha256, first.dumpSha256);
    assert.equal(noise.identitySha256, first.identitySha256);
    assert.equal(noise.dependencyConfigSha256, first.dependencyConfigSha256);
    assert.equal((await harness.check(fixture, before, after)).passed, true);
    await writeFile(path, dump([...enabled, ...enabledAdded].reverse(), [...disabled, ...disabledAdded].reverse()));
    assert.equal((await harness.snapshot(fixture, 'valid', after, join(fixture.root, 'drift-diagnostics.json'))).passed, true);
    const last = await identity(after);
    assert.equal(last.dependencyConfig.enabledComponents.length, 245);
    assert.equal(last.dependencyConfig.disabledComponents.length, 77);
    assert.deepEqual(last.dependencyConfig.enabledComponents.filter(name => !first.dependencyConfig.enabledComponents.includes(name)), [...enabledAdded].sort());
    assert.deepEqual(first.dependencyConfig.disabledComponents.filter(name => !last.dependencyConfig.disabledComponents.includes(name)), [...disabledRemoved].sort());
    assert.deepEqual(last.dependencyConfig.disabledComponents.filter(name => !first.dependencyConfig.disabledComponents.includes(name)), disabledAdded);
    for (const key of Object.keys(first) as Array<keyof typeof first>) {
      if (['dependencyConfig', 'dependencyConfigSha256', 'identitySha256', 'dumpSha256'].includes(key)) continue;
      assert.deepEqual(last[key], first[key], `stable package field ${key}`);
    }
    assert.notEqual(last.dependencyConfigSha256, first.dependencyConfigSha256);
    assert.notEqual(last.identitySha256, first.identitySha256);
    const checked = await harness.check(fixture, before, after);
    assert.equal(checked.passed, false);
    assert.deepEqual(checked.issues, [
      'com.google.android.gms dependencyConfigSha256 changed',
      'com.google.android.gms identitySha256 changed',
      'com.google.android.gms dependency configuration changed',
    ]);
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
    const preparation = prepare(fixture, 5_000);
    assert.equal(preparation.passed, false, name);
    assert.equal(preparation.stderr, 'ANDROID_ENVIRONMENT_INVALID: required environment evidence or operation failed\n');
    const preparationDiagnostics = JSON.parse(await readFile(join(fixture.root, 'preparation-diagnostics.json'), 'utf8'));
    assert.equal(preparationDiagnostics.failure.stage, 'read required system property ro.build.id');
    assert.equal((await readFile(fixture.log, 'utf8')).includes('disable-user'), false);
    const snapshot = await harness.snapshot(fixture, 'valid', join(fixture.root, 'before.json'), join(fixture.root, 'diagnostics.json'));
    assert.equal(snapshot.passed, false, name);
    assert.equal(snapshot.stderr, 'ANDROID_ENVIRONMENT_INVALID: required environment evidence or operation failed\n');
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
      assert.equal(prepare(fixture, failure === 'propertyTimeout' ? 1000 : 5_000).passed, false);
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
    assert.equal(result.stderr, 'ANDROID_ENVIRONMENT_INVALID: required environment evidence or operation failed\n');
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
    assert.equal(result.stderr, 'ANDROID_ENVIRONMENT_INVALID: required environment evidence or operation failed\n');
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
    const result = prepare(fixture, name === 'substring is not absence' ? 5_000 : 1_000);
    assert.equal(result.passed, false, name);
    if (name === 'substring is not absence') {
      const diagnostics = JSON.parse(await readFile(join(fixture.root, 'preparation-diagnostics.json'), 'utf8'));
      assert.equal(diagnostics.failure.stage, 'observe Vending user 0 package state');
    }
    const allowed = name.includes('disable');
    const requests = await readFile(fixture.log, 'utf8');
    assert.equal(requests.includes('disable-user'), allowed, result.stderr);
    if (name === 'wrong AVD') {
      assert.equal(requests, '-s emulator-5554 emu avd name\n');
      const diagnostic = JSON.parse(await readFile(fixture.diagnostic, 'utf8')) as FakeAdbDiagnostic;
      assert.doesNotThrow(() => validateFakeAdbDiagnostic(diagnostic));
    }
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
  return tests;
}
