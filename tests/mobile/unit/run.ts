import { strict as assert } from 'node:assert';
import { mkdtemp, mkdir, writeFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  assertDistinctUpgrade,
  prepareBundle,
  safeRelativePath,
  sameIdentity,
  validateWebRoot,
  type BundleIdentity,
  type BundleExpectation,
} from '../support/artifacts';
import { assertNoKnownSecret, redactText, sanitizeValue } from '../support/diagnostics';
import { parseAndroidAvdName } from '../support/android';
import { androidOpenUrlArgs } from '../platforms/android';
import { runtimeScript } from '../platforms/types';
import { repositoryPath, repositoryRoot } from '../support/paths';
import { command } from '../support/process';
import {
  assertBoundedReloads,
  assertCredentialIdentityPreserved,
  assertCredentialPreserved,
  assertNoRelayDeploy,
  assertNoRelayInstall,
  assertPhoneUpdateAcknowledged,
  assertPhoneUpdateNotAcknowledged,
  assertRunningIdentity,
  assertStandalone,
  assertUpgradeDidNotComplete,
  type RuntimeIdentity,
} from '../support/oracle';

const tests: Array<[string, () => Promise<void>]> = [];
function test(name: string, body: () => Promise<void>): void {
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

test('Android emulator-console parser handles names, terminators, and errors', async () => {
  assert.equal(parseAndroidAvdName('herdr-mobile-ci-42-1\r\nOK\r\n'), 'herdr-mobile-ci-42-1');
  assert.equal(parseAndroidAvdName('other-avd\nOK\n'), 'other-avd');
  assert.notEqual(parseAndroidAvdName('other-avd\nOK\n'), 'herdr-mobile-ci-42-1');
  assert.equal(parseAndroidAvdName('OK\n'), undefined);
  assert.equal(parseAndroidAvdName('KO: unknown command\n'), undefined);
  assert.equal(parseAndroidAvdName('\r\n'), undefined);
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

let failures = 0;
for (const [name, body] of tests) {
  try {
    await body();
    process.stdout.write(`ok - ${name}\n`);
  } catch (error) {
    failures += 1;
    process.stderr.write(`not ok - ${name}: ${error instanceof Error ? error.stack || error.message : String(error)}\n`);
  }
}
if (failures) process.exitCode = 1;
