import { strict as assert } from 'node:assert';
import { mkdtemp, mkdir, writeFile } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  assertDistinctUpgrade,
  fileSha256,
  prepareBundle,
  safeRelativePath,
  sameIdentity,
  validateWebRoot,
  type BundleIdentity,
  type BundleExpectation,
} from '../support/artifacts';
import { assertNoKnownSecret, redactText, sanitizeValue } from '../support/diagnostics';
import { PhaseBudget } from '../support/budget';
import { AppiumClient, isFatalDriverError, WebDriverError } from '../support/webdriver';
import { parseAndroidAvdName } from '../support/android';
import { AndroidPlatform, androidChromeCapabilities, androidChromeShortcutArgs, androidOpenUrlArgs, hasAndroidChromeDevToolsSocket, parseAndroidChromeShortcuts } from '../platforms/android';
import { IOSPlatform, iosInstalledContextRejection, isIOSSafariBrowserBundle, isIOSSafariViewServiceBundle, isIOSStaleContextError } from '../platforms/ios';
import { runtimeScript } from '../platforms/types';
import { prepareOutput, repositoryPath, repositoryRoot } from '../support/paths';
import { validateProvenance, type ProvenanceRun } from '../support/provenance';
import { command } from '../support/process';
import {
  assertBoundedReloads,
  assertCredentialIdentityPreserved,
  assertCredentialPreserved,
  assertInvitationOwnership,
  assertRelayOwnership,
  assertNoRelayDeploy,
  assertNoRelayInstall,
  assertPhoneUpdateAcknowledged,
  assertPhoneUpdateNotAcknowledged,
  assertRunningIdentity,
  assertStandalone,
  assertUpgradeDidNotComplete,
  isQualificationFatal,
  QualificationFailureLatch,
  type RuntimeIdentity,
} from '../support/oracle';

type TestOutcome = void | string;
const tests: Array<[string, () => Promise<TestOutcome>]> = [];
function test(name: string, body: () => Promise<TestOutcome>): void {
  tests.push([name, body]);
}

const legacyExpected: BundleExpectation = {
  name: 'fixture-old',
  version: '0.20.8',
  assets: 361,
  sourceRelease: 'fixture',
  sourceCommit: 'fixture',
};

async function legacyRoot(version = '0.20.8', assets = 361): Promise<string> {
  const root = await mkdtemp(join(tmpdir(), 'herdr-mobile-ci-unit-'));
  await mkdir(join(root, 'assets'), { recursive: true });
  await writeFile(join(root, 'version.json'), JSON.stringify({ version, assets }));
  await writeFile(join(root, 'index.html'), '<html></html>');
  await writeFile(join(root, 'assets', 'app.js'), `window.app = '${version}';`);
  await writeFile(join(root, 'assets', 'app.css'), 'body{}');
  return root;
}

test('safe archive and descriptor paths reject traversal', async () => {
  assert.equal(safeRelativePath('assets/app.js'), true);
  assert.equal(safeRelativePath('../assets/app.js'), false);
  assert.equal(safeRelativePath('/etc/passwd'), false);
  assert.equal(safeRelativePath('assets/../app.js'), false);
  assert.equal(safeRelativePath('assets\\app.js'), false);
});

test('directory preparation verifies legacy identity and hashes', async () => {
  const root = await legacyRoot();
  const identity = await validateWebRoot(root, legacyExpected);
  assert.equal(identity.version, '0.20.8');
  assert.equal(identity.descriptor, false);
  assert.equal(identity.script, '/assets/app.js');
  assert.match(identity.webHash, /^[a-f0-9]{64}$/);
});

test('candidate directories require an explicit local escape hatch', async () => {
  const source = await legacyRoot();
  const output = await mkdtemp(join(tmpdir(), 'herdr-mobile-ci-directory-'));
  await assert.rejects(
    prepareBundle('directory', legacyExpected, source, join(output, 'directory')),
    /ARTIFACT_ARCHIVE_REQUIRED/,
  );
  await assert.rejects(
    prepareBundle('mismatch', { ...legacyExpected, webHash: '0'.repeat(64) }, source, join(output, 'mismatch'), { allowDirectory: true }),
    /ARTIFACT_WEB_HASH/,
  );
});

test('archive checksum mismatch is rejected before extraction', async () => {
  const source = join(await mkdtemp(join(tmpdir(), 'herdr-mobile-ci-archive-')), 'bad.tar.gz');
  await writeFile(source, 'not an archive');
  const output = await mkdtemp(join(tmpdir(), 'herdr-mobile-ci-output-'));
  const expected = { ...legacyExpected, archiveSha256: '0'.repeat(64) };
  await assert.rejects(
    prepareBundle('bad', expected, source, join(output, 'bad')),
    /ARTIFACT_CHECKSUM|ARTIFACT_MANIFEST/,
  );
});

test('valid release archives extract through the verified GNU tar path', async () => {
  const tar = process.env.MOBILE_GNU_TAR || (process.platform === 'darwin' ? 'gtar' : 'tar');
  try {
    execFileSync(tar, ['--version'], { stdio: 'ignore' });
  } catch {
    return 'GNU tar is unavailable';
  }
  const sourceRoot = await mkdtemp(join(tmpdir(), 'herdr-mobile-ci-valid-archive-'));
  const web = join(sourceRoot, 'web');
  await mkdir(join(web, 'assets'), { recursive: true });
  await writeFile(join(web, 'version.json'), JSON.stringify({ version: '0.20.8', assets: 361 }));
  await writeFile(join(web, 'index.html'), '<html></html>');
  await writeFile(join(web, 'assets', 'app.js'), 'window.fixture = true;');
  await writeFile(join(web, 'assets', 'app.css'), 'body{}');
  await mkdir(join(sourceRoot, 'relay'), { recursive: true });
  await writeFile(join(sourceRoot, 'relay', 'binary'), 'not needed by mobile');
  await writeFile(join(sourceRoot, 'release-manifest.json'), JSON.stringify({ version: '0.20.8', revision: 'fixture', web_hash: '' }));
  const archive = join(sourceRoot, 'fixture.tar.gz');
  execFileSync(tar, ['-C', sourceRoot, '-czf', archive, 'web', 'relay', 'release-manifest.json']);
  const output = await mkdtemp(join(tmpdir(), 'herdr-mobile-ci-valid-output-'));
  const expected = { ...legacyExpected, revision: 'fixture', archiveSha256: await fileSha256(archive) };
  const prepared = await prepareBundle('valid', expected, archive, join(output, 'valid'));
  assert.equal(prepared.identity.script, '/assets/app.js');
  assert.equal(prepared.identity.style, '/assets/app.css');
  assert.equal(existsSync(join(output, 'valid', 'release-manifest.json')), true);
  assert.equal(existsSync(join(output, 'valid', 'relay')), false);
});

test('same-version different-build pairs are distinct', async () => {
  const base: BundleIdentity = {
    version: '0.20.10', assets: 363, build: 'a'.repeat(64), entry: '/builds/a/index.html',
    script: '/assets/app-a.js', style: '/assets/app-b.css', scriptSha256: 'a'.repeat(64),
    styleSha256: 'b'.repeat(64), webHash: 'c'.repeat(64), descriptor: true,
  };
  const target = { ...base, build: 'd'.repeat(64), scriptSha256: 'd'.repeat(64), webHash: 'e'.repeat(64) };
  assert.equal(sameIdentity(base, target), false);
  assert.doesNotThrow(() => assertDistinctUpgrade(
    { name: 'base', provenance: legacyExpected, root: '/tmp/base', identity: base, archiveSha256: '' },
    { name: 'target', provenance: legacyExpected, root: '/tmp/target', identity: target, archiveSha256: '' },
  ));
});

test('repository-relative CLI paths are anchored at the repository root', async () => {
  assert.equal(existsSync(join(repositoryRoot, 'go.mod')), true);
  assert.equal(existsSync(join(repositoryRoot, 'tests/mobile/baselines.json')), true);
  assert.equal(repositoryPath('tests/mobile/bundle-set.json'), join(repositoryRoot, 'tests/mobile/bundle-set.json'));
  assert.equal(repositoryPath('/tmp/mobile-bundles/bundle-set.json'), '/tmp/mobile-bundles/bundle-set.json');
});

test('mobile output preparation preserves existing files and rejects unsafe overlaps', async () => {
  const root = await mkdtemp(join(tmpdir(), 'herdr-mobile-ci-output-safety-'));
  const output = join(root, 'output');
  await mkdir(output);
  const sentinel = join(output, 'sentinel.txt');
  await writeFile(sentinel, 'keep');
  await assert.rejects(prepareOutput(output), /MOBILE_OUTPUT/);
  assert.equal(existsSync(sentinel), true);
  await assert.rejects(prepareOutput(repositoryRoot), /repository or its parent/);
  const input = join(root, 'input');
  await mkdir(input);
  await assert.rejects(prepareOutput(join(input, 'nested-output'), [input]), /overlaps input/);
});

test('PR provenance keeps merge build and PR head identities separate', async () => {
  const repository = '0cv/herdr-mobile-relay';
  const prHead = 'f02fcb1d3742487ac4a1e541d60cf3988284caf8';
  const mergeBuild = '59fae417ab8d52e352dc4d6fa79cdbd214a8d6a4';
  const run: ProvenanceRun = {
    repository,
    headSha: prHead,
    event: 'pull_request',
    headBranch: 'review',
    workflow: '.github/workflows/check.yml',
    headRepository: repository,
    status: 'completed',
    conclusion: 'failure',
  };
  assert.deepEqual(validateProvenance(run, {
    mode: 'internal', repository, artifactRunId: '34321851994', callerRunId: '34321851994',
    candidateCommit: mergeBuild, manifestRevision: mergeBuild,
  }), { sourceSha: mergeBuild, headSha: prHead });
  assert.throws(() => validateProvenance(run, {
    mode: 'internal', repository, artifactRunId: '34321851994', callerRunId: '34321851994',
    candidateCommit: prHead, manifestRevision: mergeBuild,
  }), /PROVENANCE_BUILD_SHA/);
});

test('the workflow provenance adapter uses the shared validator', async () => {
  const sha = 'a'.repeat(40);
  const output = execFileSync('bun', ['tests/mobile/provenance-check.ts'], {
    cwd: repositoryRoot,
    encoding: 'utf8',
    env: {
      ...process.env,
      PROVENANCE_MODE: 'external',
      PROVENANCE_REPOSITORY: '0cv/herdr-mobile-relay',
      PROVENANCE_ARTIFACT_RUN_ID: 'source-run',
      PROVENANCE_CALLER_RUN_ID: 'other-run',
      PROVENANCE_MANIFEST_REVISION: sha,
      PROVENANCE_RUN_REPOSITORY: '0cv/herdr-mobile-relay',
      PROVENANCE_HEAD_SHA: sha,
      PROVENANCE_RUN_EVENT: 'push',
      PROVENANCE_RUN_BRANCH: 'main',
      PROVENANCE_RUN_WORKFLOW: '.github/workflows/check.yml',
      PROVENANCE_HEAD_REPOSITORY: '0cv/herdr-mobile-relay',
      PROVENANCE_RUN_STATUS: 'completed',
      PROVENANCE_RUN_CONCLUSION: 'success',
      PROVENANCE_SOURCE_COMMIT: sha,
    },
  });
  assert.deepEqual(JSON.parse(output), { sourceSha: sha, headSha: sha });
});

test('external provenance requires a successful same-repository main push', async () => {
  const repository = '0cv/herdr-mobile-relay';
  const mainSha = '5d169cb2d43cbe80eccf5494978001b63dc2fca9';
  const run: ProvenanceRun = {
    repository,
    headSha: mainSha,
    event: 'push',
    headBranch: 'main',
    workflow: '.github/workflows/check.yml',
    headRepository: repository,
    status: 'completed',
    conclusion: 'success',
  };
  assert.deepEqual(validateProvenance(run, {
    mode: 'external', repository, artifactRunId: '34321848225', callerRunId: 'other-run',
    sourceCommit: mainSha, manifestRevision: mainSha,
  }), { sourceSha: mainSha, headSha: mainSha });
  assert.throws(() => validateProvenance({ ...run, headRepository: 'fork/herdr-mobile-relay' }, {
    mode: 'external', repository, artifactRunId: '34321848225', callerRunId: 'other-run', manifestRevision: mainSha,
  }), /PROVENANCE_HEAD_REPOSITORY/);
  assert.throws(() => validateProvenance(run, {
    mode: 'unsupported' as 'internal', repository, artifactRunId: '34321848225', callerRunId: 'other-run', manifestRevision: mainSha,
  }), /PROVENANCE_MODE/);
});

test('Android emulator-console parser handles names, terminators, and errors', async () => {
  assert.equal(parseAndroidAvdName('herdr-mobile-ci-42-1\r\nOK\r\n'), 'herdr-mobile-ci-42-1');
  assert.equal(parseAndroidAvdName('other-avd\nOK\n'), 'other-avd');
  assert.notEqual(parseAndroidAvdName('other-avd\nOK\n'), 'herdr-mobile-ci-42-1');
  assert.equal(parseAndroidAvdName('OK\n'), undefined);
  assert.equal(parseAndroidAvdName('KO: unknown command\n'), undefined);
  assert.equal(parseAndroidAvdName('\r\n'), undefined);
});

test('Android Chrome startup only enables attach mode after explicit launch', async () => {
  const ordinary = androidChromeCapabilities('emulator-5554');
  const attached = androidChromeCapabilities('emulator-5554', true);
  assert.equal('appium:androidUseRunningApp' in ordinary, false);
  assert.equal((ordinary['goog:chromeOptions'] as Record<string, unknown>).androidUseRunningApp, undefined);
  assert.equal((attached['goog:chromeOptions'] as Record<string, unknown>).androidUseRunningApp, true);
});

test('Android final launch verifies readiness only after bootstrap teardown', async () => {
  const platform = new AndroidPlatform({
    origin: 'https://fixture.test',
    appiumUrl: 'http://fake.test',
    outputDir: '/tmp/herdr-mobile-ci-unit',
    certificate: '',
    setupUrl: '',
    deviceId: 'emulator-5554',
    budget: new PhaseBudget('android-launch-test', { timeoutMs: 10_000, recoveryLimit: 1 }),
  });
  const shortcut = {
    id: 'shortcut-id', shortLabel: 'Herdr Relay', name: 'Herdr Mobile Relay',
    url: 'https://fixture.test/', scope: 'https://fixture.test/', mac: 'mac',
  };
  const events: string[] = [];
  const driver = platform.driver as any;
  (platform as any).waitForChromeShortcut = async () => { events.push('shortcut'); return shortcut; };
  driver.close = async () => { events.push('close'); };
  (platform as any).launchChromeShortcut = async () => { events.push('launch'); };
  (platform as any).waitForInstalledTarget = async () => { events.push('target'); };
  (platform as any).waitForChromeDevTools = async () => { events.push('devtools'); };
  (platform as any).createChromeSession = async () => { events.push('create'); };
  (platform as any).attachToInstalledView = async () => { events.push('attach'); };
  const ownershipRoot = await mkdtemp(join(tmpdir(), 'herdr-mobile-ci-ownership-'));
  const ownershipFile = join(ownershipRoot, 'owned');
  await writeFile(ownershipFile, 'android:emulator-5554\n');
  const previousOwnershipFile = process.env.MOBILE_DEVICE_OWNERSHIP_FILE;
  process.env.MOBILE_DEVICE_OWNERSHIP_FILE = ownershipFile;
  try {
    await platform.launchInstalledApp();
  } finally {
    if (previousOwnershipFile === undefined) delete process.env.MOBILE_DEVICE_OWNERSHIP_FILE;
    else process.env.MOBILE_DEVICE_OWNERSHIP_FILE = previousOwnershipFile;
  }
  assert.deepEqual(events, ['shortcut', 'close', 'launch', 'target', 'devtools', 'create', 'attach']);
});

test('Android Chrome DevTools readiness recognizes the published socket', async () => {
  assert.equal(hasAndroidChromeDevToolsSocket('00000000 00000002 00010000 0001 01 12345 @chrome_devtools_remote_42\n'), true);
  assert.equal(hasAndroidChromeDevToolsSocket('00000000 00000002 00010000 0001 01 12345 @webview_devtools_remote_42\n'), false);
});

test('Android Chrome shortcut output preserves the signed launch fields', async () => {
  const shortcuts = parseAndroidChromeShortcuts(`ShortcutInfo {id=shortcut-id, flags=0x28a
  shortLabel=Herdr Relay, resId=0[null]
  intents=[Intent { act=com.google.android.apps.chrome.webapps.WebappManager.ACTION_START_WEBAPP pkg=com.android.chrome }/PersistableBundle[{org.chromium.chrome.browser.webapp_scope=https://localhost:38289/, org.chromium.chrome.browser.webapp_name=Herdr Mobile Relay, org.chromium.chrome.browser.webapp_mac=mac+/=, org.chromium.chrome.browser.webapp_id=shortcut-id, org.chromium.chrome.browser.webapp_source=7, org.chromium.chrome.browser.webapp_display_mode=3, org.chromium.content_public.common.orientation=0, org.chromium.chrome.browser.webapp_url=https://localhost:38289/}]]
}`);
  assert.equal(shortcuts.length, 1);
  assert.deepEqual(shortcuts[0], {
    id: 'shortcut-id', shortLabel: 'Herdr Relay', name: 'Herdr Mobile Relay',
    url: 'https://localhost:38289/', scope: 'https://localhost:38289/', mac: 'mac+/=',
    source: '7', displayMode: '3', orientation: '0',
  });
  const args = androidChromeShortcutArgs('emulator-5554', shortcuts[0]);
  assert.equal(args.slice(0, 3).join(' '), '-s emulator-5554 shell');
  assert.match(args[3], /webapp_mac.*mac\+\//s);
  assert.match(args[3], /webapp_url.*https:\/\/localhost:38289\//s);
});

test('Android setup URL survives ADB remote-shell serialization', async () => {
  const url = "https://localhost:1234/#setup=secret&invite=alpha's&relay=wss%3A%2F%2Flocalhost%3A5678";
  const args = androidOpenUrlArgs('emulator-5554', url);
  assert.deepEqual(args.slice(0, 3), ['-s', 'emulator-5554', 'shell']);
  assert.equal(args.length, 4);
  const serialized = await command(process.execPath, [
    '-e', 'process.stdout.write(JSON.stringify(process.argv.slice(1)))', '--', ...args,
  ]);
  assert.deepEqual(JSON.parse(serialized.stdout), args);
  const fakeAdbBin = await mkdtemp(join(tmpdir(), 'herdr-mobile-ci-adb-'));
  await writeFile(join(fakeAdbBin, 'am'), '#!/bin/sh\nprintf "%s\\n" "$@"\n', { mode: 0o700 });
  const remoteCommand = args.slice(3).join(' ');
  const fakeCommand = await command('/bin/sh', ['-c', remoteCommand], 30_000, {
    env: { ...process.env, PATH: `${fakeAdbBin}:${process.env.PATH || ''}` },
  });
  const received = fakeCommand.stdout.trim().split(/\r?\n/u);
  assert.equal(received[received.indexOf('-d') + 1], url);
});

test('WebDriver runtime script returns an identity from function-body execution', async () => {
  const document = {
    querySelector: (selector: string) => selector === '[data-app-assets]'
      ? { getAttribute: (name: string) => name === 'data-app-assets' ? '364' : 'abcdef0123456789' }
      : selector.startsWith('script')
        ? { getAttribute: () => '/assets/app-abc.js' }
        : { sheet: {}, getAttribute: () => '/assets/app-abc.css' },
    querySelectorAll: () => [{ textContent: 'Relay version 0.20.11 Phone app version 0.20.10' }],
    documentElement: { dataset: { appAssets: '364', appBuild: 'abcdef0123456789', herdrCssReady: '1' } },
    getElementById: () => ({ childNodes: [{}] }),
  };
  const window = { matchMedia: () => ({ matches: true }) };
  const navigator = { userAgent: 'test', standalone: false };
  const performance = { timeOrigin: 123 };
  const location = { href: 'https://localhost/builds/0.20.11-364-abcdef0123456789/index.html', origin: 'https://localhost', pathname: '/builds/0.20.11-364-abcdef0123456789/index.html' };
  const XMLHttpRequest = class {
    status = 200;
    responseText = JSON.stringify({ version: '0.20.11', assets: 364, build: 'abcdef0123456789' });
    open() {}
    send() {}
  };
  const identity = new Function('document', 'window', 'navigator', 'performance', 'location', 'XMLHttpRequest', runtimeScript())(
    document, window, navigator, performance, location, XMLHttpRequest,
  ) as RuntimeIdentity;
  assert.equal(identity.version, '0.20.10');
  assert.equal(identity.assets, 364);
  assert.equal(identity.buildFromApplication, true);
  assert.equal(identity.applicationInitialized, true);
});

test('standalone oracle requires native provider evidence', async () => {
  const identity: RuntimeIdentity = {
    url: 'https://localhost/', origin: 'https://localhost', standalone: true, provider: 'android-standalone',
    nativeActivity: 'WebappActivity', nativePid: '1',
    version: '0.20.10', assets: 363, build: '', entry: '/index.html', script: '/assets/app.js', style: '/assets/app.css',
    requiredAssetsReady: true, applicationInitialized: true,
  };
  assert.throws(() => assertStandalone(identity, 'https://localhost'), /STANDALONE_PROVIDER_REQUIRED/);
  assert.doesNotThrow(() => assertStandalone({ ...identity, nativeProvider: 'android:org.chromium.webapk' }, 'https://localhost'));
});

test('runtime oracle requires loaded target assets and identity', async () => {
  const identity: RuntimeIdentity = {
    url: 'https://localhost/', origin: 'https://localhost', standalone: true, provider: 'test',
    version: '0.20.10', assets: 363, build: 'cf1b92fa5edff10a',
    entry: '/builds/0.20.10-363-cf1b92fa5edff10a/index.html',
    buildFromApplication: true,
    script: '/assets/app-script.js', style: '/assets/app-style.css',
    requiredAssetsReady: true, applicationInitialized: true,
  };
  assert.doesNotThrow(() => assertRunningIdentity(identity, {
    version: '0.20.10', assets: 363, build: 'cf1b92fa5edff10ab372fcb8479ad789a5443a6a8260a81dc61f30c8198045ab',
    entry: identity.entry, script: identity.script, style: identity.style,
    scriptSha256: 'a'.repeat(64), styleSha256: 'b'.repeat(64), webHash: 'c'.repeat(64), descriptor: true,
  }));
  assert.throws(() => assertRunningIdentity({ ...identity, script: '/assets/app-old.js' }, {
    version: '0.20.10', assets: 363, build: identity.build, entry: identity.entry,
    script: identity.script, style: identity.style, scriptSha256: 'a'.repeat(64), styleSha256: 'b'.repeat(64),
    webHash: 'c'.repeat(64), descriptor: true,
  }), /RUNTIME_SCRIPT_MISMATCH/);
  assert.throws(() => assertRunningIdentity({ ...identity, buildFromApplication: false }, {
    version: '0.20.10', assets: 363, build: identity.build, entry: identity.entry,
    script: identity.script, style: identity.style, scriptSha256: 'a'.repeat(64), styleSha256: 'b'.repeat(64),
    webHash: 'c'.repeat(64), descriptor: true,
  }), /RUNTIME_BUILD_SOURCE/);
});

test('oracle observes phone completion instead of only runtime readiness', async () => {
  assert.doesNotThrow(() => assertPhoneUpdateNotAcknowledged({
    phoneRequired: true, phoneAcknowledged: false, phoneState: 'failed', visibleCompletion: false, rawPlanPresent: true,
  }));
  assert.throws(() => assertPhoneUpdateNotAcknowledged({
    phoneRequired: true, phoneAcknowledged: true, phoneState: 'loaded', visibleCompletion: true, rawPlanPresent: true,
  }), /PREMATURE_PHONE_COMPLETION/);
  assert.doesNotThrow(() => assertPhoneUpdateAcknowledged({
    phoneRequired: true, phoneAcknowledged: true, phoneState: 'loaded', visibleCompletion: true, rawPlanPresent: true,
  }));
  assert.throws(() => assertPhoneUpdateAcknowledged({
    phoneRequired: true, phoneAcknowledged: false, phoneState: 'loading', visibleCompletion: false, rawPlanPresent: true,
  }), /PHONE_COMPLETION_MISSING/);
  assert.throws(() => assertPhoneUpdateNotAcknowledged({
    phoneRequired: false, phoneAcknowledged: false, phoneState: 'failed', visibleCompletion: false, rawPlanPresent: true,
  }), /PHONE_PLAN_MISSING/);
  assert.throws(() => assertPhoneUpdateNotAcknowledged({
    phoneRequired: false, phoneAcknowledged: false, phoneState: 'failed', visibleCompletion: false, rawPlanPresent: false,
  }), /PHONE_COMPLETION_EVIDENCE_MISSING/);
});

test('oracle rejects premature completion and relay installs', async () => {
  const identity: RuntimeIdentity = {
    url: 'https://localhost/', origin: 'https://localhost', standalone: true, provider: 'test',
    version: '0.20.8', assets: 361, build: '', entry: '/index.html',
    script: '/assets/app.js', style: '/assets/app.css', requiredAssetsReady: true, applicationInitialized: true,
  };
  const target: BundleIdentity = {
    version: '0.20.10', assets: 363, build: 'a'.repeat(64), entry: '/builds/a/index.html',
    script: '/assets/app-a.js', style: '/assets/app-b.css', scriptSha256: 'a'.repeat(64),
    styleSha256: 'b'.repeat(64), webHash: 'c'.repeat(64), descriptor: true,
  };
  assert.throws(() => assertUpgradeDidNotComplete(true, identity, target), /PREMATURE_PHONE_COMPLETION/);
  assert.doesNotThrow(() => assertNoRelayInstall(0));
  assert.throws(() => assertNoRelayInstall(1), /UNEXPECTED_RELAY_INSTALL/);
  assert.doesNotThrow(() => assertNoRelayDeploy(0));
  assert.throws(() => assertNoRelayDeploy(1), /UNEXPECTED_RELAY_DEPLOY/);
});

test('credential evidence rejects fresh enrollment', async () => {
  assert.doesNotThrow(() => assertCredentialPreserved(
    { relays: { alpha: { invitationAuthCount: 1, credentialAuthCount: 1, credentialPseudonyms: ['one'] } } },
    { relays: { alpha: { invitationAuthCount: 1, credentialAuthCount: 2, credentialPseudonyms: ['one'] } } },
  ));
  assert.throws(() => assertCredentialPreserved(
    { relays: { alpha: { invitationAuthCount: 1, credentialAuthCount: 1, credentialPseudonyms: ['one'] } } },
    { relays: { alpha: { invitationAuthCount: 1, credentialAuthCount: 1, credentialPseudonyms: ['one'] } } },
  ), /CREDENTIAL_NOT_USED/);
  assert.throws(() => assertCredentialPreserved(
    { relays: { alpha: { invitationAuthCount: 1, credentialAuthCount: 1, credentialPseudonyms: ['one'] } } },
    { relays: { alpha: { invitationAuthCount: 2, credentialAuthCount: 2, credentialPseudonyms: ['one', 'two'] } } },
  ), /INVITATION_REUSED/);
  assert.throws(() => assertCredentialPreserved(
    { relays: {
      alpha: { invitationAuthCount: 1, credentialAuthCount: 1, credentialPseudonyms: ['one'] },
      beta: { invitationAuthCount: 1, credentialAuthCount: 1, credentialPseudonyms: ['two'] },
    } },
    { relays: { alpha: { invitationAuthCount: 1, credentialAuthCount: 2, credentialPseudonyms: ['one'] } } },
  ), /CREDENTIAL_RELAY_MISSING/);
  assert.throws(() => assertCredentialIdentityPreserved(
    { relays: { alpha: { invitationAuthCount: 1, credentialAuthCount: 1, credentialPseudonyms: ['one'] } } },
    { relays: { alpha: { invitationAuthCount: 2, credentialAuthCount: 3, credentialPseudonyms: ['one', 'replacement'] } } },
  ), /INVITATION_REUSED/);
});

test('qualification ownership and completion failures latch their first cause', async () => {
  const invitation = {
    relays: {
      alpha: { invitationAuthCount: 1, credentialAuthCount: 0, credentialPseudonyms: [] },
      beta: { invitationAuthCount: 1, credentialAuthCount: 0, credentialPseudonyms: [] },
    },
  };
  assert.doesNotThrow(() => assertInvitationOwnership(invitation, ['alpha', 'beta']));
  assert.throws(() => assertRelayOwnership(invitation, ['alpha', 'beta']), /CREDENTIAL_OWNERSHIP_MISSING/);
  assert.throws(() => assertRelayOwnership({ relays: {
    alpha: { invitationAuthCount: 1, credentialAuthCount: 1, credentialPseudonyms: ['replacement'] },
  } }, ['alpha']), /CREDENTIAL_CONNECTION_MISSING/);
  assert.doesNotThrow(() => assertRelayOwnership({ relays: {
    alpha: { invitationAuthCount: 1, credentialAuthCount: 1, credentialPseudonyms: ['replacement'], connections: 1 },
  } }, ['alpha']));

  const latch = new QualificationFailureLatch();
  let first: unknown;
  try {
    latch.fail(new Error('OPEN_FIXTURE_AGENT: native provider precondition failed'), 'lifecycle');
  } catch (error) {
    first = error;
  }
  assert.equal(isQualificationFatal(first), true);
  try {
    latch.fail(new Error('UPGRADE_FAILURE_TARGET_MISMATCH: later polling error'), 'upgrade');
    assert.fail('latch should throw');
  } catch (error) {
    assert.equal(error, first);
  }
  assert.deepEqual((first as { snapshot: () => unknown }).snapshot(), {
    code: 'OPEN_FIXTURE_AGENT',
    stage: 'lifecycle',
    message: 'native provider precondition failed',
  });
});

test('diagnostic redaction covers URL and escaped values', async () => {
  const secret = 'A'.repeat(43);
  const text = redactText(`https://localhost/#setup=${secret}&invite=invite-id`);
  assert.equal(text.includes(secret), false);
  assertNoKnownSecret(text, [secret]);
  assert.deepEqual(sanitizeValue({ url: `#setup=${secret}`, nested: [secret] }), {
    url: '#setup=[REDACTED]', nested: ['[REDACTED]'],
  });
});

test('reload count is bounded', async () => {
  assert.doesNotThrow(() => assertBoundedReloads(2));
  assert.throws(() => assertBoundedReloads(3), /RELOAD_BOUND_EXCEEDED/);
});

test('phase budget prevents a new mutation after expiry', async () => {
  let now = 0;
  let requests = 0;
  const budget = new PhaseBudget('fake-appium', { timeoutMs: 10, now: () => now, recoveryLimit: 1 });
  const client = new AppiumClient('http://fake.test', 100, async () => {
    requests += 1;
    return new Response(JSON.stringify({ value: { sessionId: 'session' }, sessionId: 'session' }), { status: 200 });
  });
  await client.create({ capabilities: {}, budget });
  now = 11;
  await assert.rejects(() => client.contexts(), /PHASE_BUDGET_EXHAUSTED/);
  assert.equal(requests, 1);
});

test('Appium timeout preserves context and blocks follow-up commands', async () => {
  let requests = 0;
  const client = new AppiumClient('http://fake.test', 5, async () => {
    requests += 1;
    if (requests === 1) return new Response(JSON.stringify({ value: { sessionId: 'session' }, sessionId: 'session' }), { status: 200 });
    throw new DOMException('hung command', 'TimeoutError');
  });
  await client.create({ capabilities: {} });
  await assert.rejects(() => client.contexts(), /APPIUM_TIMEOUT/);
  await assert.rejects(() => client.contexts(), /APPIUM_SESSION_UNUSABLE/);
  assert.equal(requests, 2);
  assert.equal(client.snapshot().unusable, true);
  assert.equal(client.snapshot().firstFatal?.code, 'APPIUM_TIMEOUT');
});

test('Appium failed teardown retains the session quarantine until confirmed', async () => {
  let requests = 0;
  const client = new AppiumClient('http://fake.test', 50, async (_input, init) => {
    requests += 1;
    if (requests === 1) return new Response(JSON.stringify({ value: { sessionId: 'session' }, sessionId: 'session' }), { status: 200 });
    if (requests === 2) return new Response(JSON.stringify({ value: { error: 'unknown error' } }), { status: 500 });
    if (requests === 3) return new Response(JSON.stringify({ value: {} }), { status: 200 });
    if (String(init?.method) === 'POST') return new Response(JSON.stringify({ value: { sessionId: 'replacement' }, sessionId: 'replacement' }), { status: 200 });
    return new Response(JSON.stringify({ value: {} }), { status: 200 });
  });
  await client.create({ capabilities: {} });
  await assert.rejects(() => client.close(), /APPIUM_COMMAND/);
  assert.equal(client.snapshot().sessionId, '[active]');
  assert.equal(client.snapshot().unusable, true);
  await assert.rejects(() => client.create({ capabilities: {} }), /APPIUM_SESSION_UNUSABLE/);
  await client.close();
  assert.equal(client.snapshot().sessionId, '');
  assert.equal(client.snapshot().unusable, false);
  await client.create({ capabilities: {} });
  assert.equal(requests, 4);
});

test('Appium teardown ignores an expired scenario budget', async () => {
  let now = 0;
  let requests = 0;
  const budget = new PhaseBudget('expired-appium', { timeoutMs: 10, now: () => now, recoveryLimit: 0 });
  const client = new AppiumClient('http://fake.test', 50, async () => {
    requests += 1;
    if (requests === 1) return new Response(JSON.stringify({ value: { sessionId: 'session' }, sessionId: 'session' }), { status: 200 });
    return new Response(null, { status: 204 });
  });
  await client.create({ capabilities: {}, budget });
  now = 11;
  await client.close();
  assert.equal(requests, 2);
  assert.equal(client.snapshot().sessionId, '');
  assert.equal(client.snapshot().unusable, false);
});

test('Appium teardown treats an already absent session as confirmed', async () => {
  let requests = 0;
  const client = new AppiumClient('http://fake.test', 50, async () => {
    requests += 1;
    if (requests === 1) return new Response(JSON.stringify({ value: { sessionId: 'session' }, sessionId: 'session' }), { status: 200 });
    return new Response(JSON.stringify({ value: { error: 'invalid session id' } }), { status: 404 });
  });
  await client.create({ capabilities: {} });
  await client.close();
  assert.equal(requests, 2);
  assert.equal(client.snapshot().sessionId, '');
  assert.equal(client.snapshot().unusable, false);
});

test('Appium multi-locator lookup stops at its operation deadline', async () => {
  let requests = 0;
  const client = new AppiumClient('http://fake.test', 30_000, async () => {
    requests += 1;
    if (requests === 1) return new Response(JSON.stringify({ value: { sessionId: 'session' }, sessionId: 'session' }), { status: 200 });
    return new Response(JSON.stringify({ value: { error: 'no such element' } }), { status: 404 });
  });
  await client.create({ capabilities: {} });
  const startedAt = Date.now();
  await assert.rejects(() => client.findAny([
    { using: 'css selector', value: '#first' },
    { using: 'css selector', value: '#second' },
    { using: 'css selector', value: '#third' },
  ], 10));
  assert.ok(Date.now() - startedAt < 200);
  assert.ok(requests >= 3 && requests <= 4);
  assert.ok(client.snapshot().lookups.every((lookup) => lookup.sliceMs > 1));
  assert.equal(client.snapshot().unusable, false);
});

test('Appium element lookup applies its child deadline to HTTP', async () => {
  let requests = 0;
  const client = new AppiumClient('http://fake.test', 30_000, async (_input, init) => {
    requests += 1;
    if (requests === 1) return new Response(JSON.stringify({ value: { sessionId: 'session' }, sessionId: 'session' }), { status: 200 });
    return new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => reject(init.signal?.reason || new DOMException('element lookup timed out', 'TimeoutError')), { once: true });
    });
  });
  await client.create({ capabilities: {} });
  await assert.rejects(() => client.find({ using: 'css selector', value: '#missing' }, 5), /APPIUM_TIMEOUT/);
  assert.equal(requests, 2);
  assert.ok((client.snapshot().lastCommand?.timeoutMs || 0) > 1);
  assert.ok((client.snapshot().lastCommand?.timeoutMs || 0) <= 5);
  assert.equal(client.snapshot().unusable, true);
  assert.equal(client.snapshot().lookups.length, 1);
  assert.equal(client.snapshot().lookups[0]?.outcome, 'fatal');
});

test('Appium lookup rotates locators fairly without overlapping commands', async () => {
  let requests = 0;
  let active = 0;
  let maximumActive = 0;
  const attempts: string[] = [];
  const perLocator = new Map<string, number>();
  const client = new AppiumClient('http://fake.test', 1_000, async (input, init) => {
    requests += 1;
    if (requests === 1) return new Response(JSON.stringify({ value: { sessionId: 'session' }, sessionId: 'session' }), { status: 200 });
    active += 1;
    maximumActive = Math.max(maximumActive, active);
    const locator = JSON.parse(String(init?.body)).value as string;
    attempts.push(locator);
    perLocator.set(locator, (perLocator.get(locator) || 0) + 1);
    await new Promise<void>((resolve) => setTimeout(resolve, 3));
    active -= 1;
    if (locator === '#second' && (perLocator.get(locator) || 0) >= 2) {
      return new Response(JSON.stringify({ value: { 'element-6066-11e4-a52e-4f735466cecf': 'target' } }), { status: 200 });
    }
    return new Response(JSON.stringify({ value: { error: 'no such element' } }), { status: 404 });
  });
  const startedAt = Date.now();
  await client.create({ capabilities: {} });
  assert.equal(await client.findAny([
    { using: 'css selector', value: '#first' },
    { using: 'css selector', value: '#second' },
  ], 500), 'target');
  assert.ok(Date.now() - startedAt < 500);
  assert.equal(maximumActive, 1);
  assert.deepEqual(attempts.slice(0, 2), ['#first', '#second']);
  assert.ok(client.snapshot().lookups.every((lookup) => lookup.sliceMs > 1));
  assert.equal(client.snapshot().lookups.at(-1)?.outcome, 'matched');
});

test('Appium lookup preserves a fatal element timeout and does not try later locators', async () => {
  let requests = 0;
  const client = new AppiumClient('http://fake.test', 50, async (_input, init) => {
    requests += 1;
    if (requests === 1) return new Response(JSON.stringify({ value: { sessionId: 'session' }, sessionId: 'session' }), { status: 200 });
    return new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => reject(init.signal?.reason || new DOMException('hung element lookup', 'TimeoutError')), { once: true });
    });
  });
  await client.create({ capabilities: {} });
  let error: unknown;
  try {
    await client.findAny([
      { using: 'accessibility id', value: 'first' },
      { using: 'accessibility id', value: 'second' },
    ], 20);
    assert.fail('lookup should fail');
  } catch (caught) {
    error = caught;
  }
  assert.equal(isFatalDriverError(error), true);
  assert.equal(requests, 2);
  assert.equal(client.snapshot().lookups.length, 1);
  assert.equal(client.snapshot().lookups[0]?.locator.value, 'first');
  assert.equal(client.snapshot().lookups[0]?.outcome, 'fatal');
  assert.equal(client.snapshot().unusable, true);
});

test('Appium response-body timeout quarantines the session', async () => {
  let requests = 0;
  const client = new AppiumClient('http://fake.test', 5, async () => {
    requests += 1;
    if (requests === 1) return new Response(JSON.stringify({ value: { sessionId: 'session' }, sessionId: 'session' }), { status: 200 });
    return {
      ok: true,
      status: 200,
      text: () => new Promise<string>(() => undefined),
    } as Response;
  });
  await client.create({ capabilities: {} });
  await assert.rejects(() => client.contexts(), /APPIUM_TIMEOUT/);
  assert.equal(client.snapshot().unusable, true);
  await assert.rejects(() => client.contexts(), /APPIUM_SESSION_UNUSABLE/);
  assert.equal(requests, 2);
});

test('Appium context metadata keeps provider identity separate from context names', async () => {
  const client = new AppiumClient('http://fake.test', 100, async (_input, init) => {
    const body = String(init?.body || '');
    const value = body.includes('mobile: getContexts')
      ? [{ id: 'WEBVIEW_1', url: 'https://fixture.test/', title: 'Installed', bundleId: 'com.apple.webapp' }]
      : { sessionId: 'session' };
    return new Response(JSON.stringify({ value, sessionId: 'session' }), { status: 200 });
  });
  await client.create({ capabilities: {} });
  const contexts = await client.contextMetadata();
  assert.deepEqual(contexts, [{
    id: 'WEBVIEW_1', url: 'https://fixture.test/', title: 'Installed', bundleId: 'com.apple.webapp', isKey: false,
    raw: { id: 'WEBVIEW_1', url: 'https://fixture.test/', title: 'Installed', bundleId: 'com.apple.webapp' },
  }]);
});

test('native lookup wrappers preserve a fatal Appium operation and skip fallbacks', async () => {
  const fatal = new WebDriverError({
    code: 'APPIUM_TIMEOUT', message: 'element request timed out', path: '/session/session/element', method: 'POST',
    durationMs: 20, timedOut: true, selectedContext: 'NATIVE_APP', selectedWindow: '',
  });
  let androidScrolls = 0;
  const android = new AndroidPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: '/tmp/herdr-mobile-ci-unit',
    certificate: '', setupUrl: '', deviceId: 'emulator-1', budget: new PhaseBudget('android-native-test', { timeoutMs: 1_000, recoveryLimit: 1 }),
  });
  (android as any).driver = {
    windowSize: async () => ({ width: 1_080, height: 2_400 }),
    findAny: async () => { throw fatal; },
    mobile: async () => { androidScrolls += 1; },
  };
  await assert.rejects(() => (android as any).findNative([{ using: 'accessibility id', value: 'Missing' }], 100), (error: unknown) => error === fatal);
  assert.equal(androidScrolls, 0);

  let iosScrolls = 0;
  const ios = new IOSPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: '/tmp/herdr-mobile-ci-unit',
    certificate: '', setupUrl: '', budget: new PhaseBudget('ios-native-test', { timeoutMs: 1_000, recoveryLimit: 1 }),
  });
  (ios as any).driver = {
    findAny: async () => { throw fatal; },
    mobile: async () => { iosScrolls += 1; },
  };
  await assert.rejects(() => (ios as any).findNativeScrollable([{ using: 'accessibility id', value: 'Missing' }], 'Missing', 100), (error: unknown) => error === fatal);
  assert.equal(iosScrolls, 0);
});

test('Android context metadata keeps the recorded response beside canonical context IDs', async () => {
  const androidResponse = [{
    webviewName: 'WEBVIEW_com.android.chrome',
    webview: 'WEBVIEW_com.android.chrome_devtools_remote',
    proc: 'com.android.chrome:sandboxed_process0',
    info: { 'Android-Package': 'com.android.chrome' },
    pages: [{ id: 'page-1', url: 'https://fixture.test/', title: 'Installed', type: 'page' }],
  }];
  const client = new AppiumClient('http://fake.test', 100, async (input, init) => {
    const path = new URL(String(input)).pathname;
    const body = String(init?.body || '');
    const value = path === '/session/session/contexts'
      ? ['NATIVE_APP', 'CHROMIUM']
      : body.includes('mobile: getContexts') ? androidResponse : { sessionId: 'session' };
    return new Response(JSON.stringify({ value, sessionId: 'session' }), { status: 200 });
  });
  await client.create({ capabilities: {} });
  assert.deepEqual(await client.contexts(), ['NATIVE_APP', 'CHROMIUM']);
  assert.deepEqual(await client.contextMetadataRaw(), androidResponse);
  assert.deepEqual(await client.contextMetadata(), []);
});

test('iOS installed-page candidates distinguish Safari from SafariViewService', async () => {
  const origin = 'https://fixture.test';
  assert.equal(isIOSSafariBrowserBundle('com.apple.mobilesafari'), true);
  assert.equal(isIOSSafariViewServiceBundle('com.apple.SafariViewService'), true);
  assert.match(iosInstalledContextRejection({ id: 'WEBVIEW_1', bundleId: 'com.apple.mobilesafari', url: origin, raw: {} }, origin), /Safari browser/);
  assert.equal(iosInstalledContextRejection({ id: 'WEBVIEW_2', bundleId: 'com.apple.SafariViewService', url: origin, raw: {} }, origin), '');
  assert.match(iosInstalledContextRejection({ id: 'WEBVIEW_3', bundleId: 'com.apple.SafariViewService', url: 'https://other.test/', raw: {} }, origin), /origin/);
});

test('iOS attachment selects a page without enumerating windows', async () => {
  const platform = new IOSPlatform({
    origin: 'https://fixture.test',
    appiumUrl: 'http://fake.test',
    outputDir: '/tmp/herdr-mobile-ci-unit',
    certificate: '',
    setupUrl: '',
    budget: new PhaseBudget('ios-test', { timeoutMs: 1_000, recoveryLimit: 1 }),
  });
  const driver = platform.driver as any;
  (platform as any).installedBundleId = 'com.apple.webapp';
  const calls: string[] = [];
  driver.contextMetadata = async () => [{
    id: 'WEBVIEW_1', bundleId: 'com.apple.SafariViewService', url: 'https://fixture.test/', raw: {},
  }];
  driver.switchContext = async (name: string) => { calls.push(`context:${name}`); };
  driver.currentUrl = async () => 'https://fixture.test/';
  driver.activeAppInfo = async () => ({ bundleId: 'com.apple.webapp', pid: '19193' });
  driver.execute = async () => ({ origin: 'https://fixture.test', standalone: true });
  driver.windowHandles = async () => { calls.push('windows'); return ['unexpected']; };
  await platform.attachToInstalledView();
  assert.equal(calls.includes('windows'), false);
});

test('iOS attachment rejects incorrect foreground, origin, and standalone state', async () => {
  const cases = [
    { foreground: 'com.apple.mobilesafari', url: 'https://fixture.test/', standalone: true, error: /installed provider/ },
    { foreground: 'com.apple.webapp', url: 'https://other.test/', standalone: true, error: /document origin/ },
    { foreground: 'com.apple.webapp', url: 'https://fixture.test/', standalone: false, error: /not standalone/ },
  ];
  for (const scenario of cases) {
    const platform = new IOSPlatform({
      origin: 'https://fixture.test',
      appiumUrl: 'http://fake.test',
      outputDir: '/tmp/herdr-mobile-ci-unit',
      certificate: '',
      setupUrl: '',
      budget: new PhaseBudget('ios-negative-test', { timeoutMs: 1_000, recoveryLimit: 1 }),
    });
    const driver = platform.driver as any;
    (platform as any).installedBundleId = 'com.apple.webapp';
    driver.contextMetadata = async () => [{
      id: 'WEBVIEW_1', bundleId: 'com.apple.SafariViewService', url: 'https://fixture.test/', raw: {},
    }];
    driver.switchContext = async () => undefined;
    driver.currentUrl = async () => scenario.url;
    driver.activeAppInfo = async () => ({ bundleId: scenario.foreground, pid: '19193' });
    driver.execute = async () => ({ origin: scenario.url.replace(/\/$/u, ''), standalone: scenario.standalone });
    await assert.rejects(() => platform.attachToInstalledView(), scenario.error);
  }
});

test('iOS attachment rediscoveries only a stale cached context', async () => {
  const platform = new IOSPlatform({
    origin: 'https://fixture.test',
    appiumUrl: 'http://fake.test',
    outputDir: '/tmp/herdr-mobile-ci-unit',
    certificate: '',
    setupUrl: '',
    budget: new PhaseBudget('ios-stale-test', { timeoutMs: 1_000, recoveryLimit: 1 }),
  });
  const driver = platform.driver as any;
  (platform as any).installedBundleId = 'com.apple.webapp';
  (platform as any).selectedInstalledContext = 'WEBVIEW_OLD';
  const contexts: string[] = [];
  driver.switchContext = async (name: string) => {
    contexts.push(name);
    if (name === 'WEBVIEW_OLD') throw new Error('no such context');
  };
  driver.contextMetadata = async () => [{
    id: 'WEBVIEW_NEW', bundleId: 'com.apple.SafariViewService', url: 'https://fixture.test/', raw: {},
  }];
  driver.currentUrl = async () => 'https://fixture.test/';
  driver.activeAppInfo = async () => ({ bundleId: 'com.apple.webapp', pid: '19193' });
  driver.execute = async () => ({ origin: 'https://fixture.test', standalone: true });
  await platform.attachToInstalledView();
  assert.equal(isIOSStaleContextError(new Error('no such context')), true);
  assert.equal(isIOSStaleContextError(new Error('document origin mismatch')), false);
  assert.deepEqual(contexts, ['NATIVE_APP', 'WEBVIEW_OLD', 'NATIVE_APP', 'WEBVIEW_NEW']);
});

let failures = 0;
for (const [name, body] of tests) {
  try {
    const outcome = await body();
    process.stdout.write(outcome ? `ok - ${name} # SKIP ${outcome}\n` : `ok - ${name}\n`);
  } catch (error) {
    failures += 1;
    process.stderr.write(`not ok - ${name}: ${error instanceof Error ? error.stack || error.message : String(error)}\n`);
  }
}
if (failures) process.exitCode = 1;
