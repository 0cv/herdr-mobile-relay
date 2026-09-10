import { strict as assert } from 'node:assert';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
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
import { assertNoKnownSecret, redactText, sanitizeValue, writeSanitizedJson } from '../support/diagnostics';
import { PhaseBudget } from '../support/budget';
import { AppiumClient, ElementLookupError, isFatalDriverError, WebDriverError } from '../support/webdriver';
import { parseAndroidAvdName } from '../support/android';
import { AndroidPlatform, androidChromeCapabilities, androidChromeShortcutArgs, androidLaunchFailureKind, androidOpenUrlArgs, hasAndroidChromeDevToolsSocket, parseAndroidChromeShortcuts } from '../platforms/android';
import { androidPackageContext, resolveStaticLibraryPackage, forcedRestartEvents, type AndroidEnvironmentAcquisitionDiagnostics, type AndroidEnvironmentSnapshot } from '../android-environment';
import { IOSPlatform, iosInstalledContextRejection, iosNativeScrollDirection, iosNativeSwipeDirection, iosOpenURLFailureKind, isIOSSafariBrowserBundle, isIOSSafariViewServiceBundle, isIOSStaleContextError, nativeActionListEvidence } from '../platforms/ios';
import { runtimeScript } from '../platforms/types';
import { prepareOutput, repositoryPath, repositoryRoot } from '../support/paths';
import { validateProvenance, type ProvenanceRun } from '../support/provenance';
import { command } from '../support/process';
import { downloadWithRetry } from '../support/download';
import { compositeIssues } from '../support/composite';
import { retentionIssues } from '../support/retention';
import { validateMobileEvidence } from '../support/evidence';
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
  assertStandaloneOwnership,
  assertUpgradeDidNotComplete,
  isRuntimeIdentityNotReady,
  isQualificationFatal,
  QualificationFailureLatch,
  type RuntimeIdentity,
} from '../support/oracle';

import { androidTransitionTests } from './android-transitions';
import { androidEnvironmentTests } from './android-environment';
import { runIOSRegressions } from './ios';
import { scenarioRunnerTests } from './scenario-runner';
import { webdriverInterruptionTests } from './webdriver-interruption';

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

function iosSafariHierarchy(): string {
  return '<AppiumAUT><XCUIElementTypeApplication name="Safari" bundleId="com.apple.mobilesafari"><XCUIElementTypeButton name="ShareButton" label="Share" enabled="true" visible="true" x="166" y="774" width="61" height="44"/></XCUIElementTypeApplication></AppiumAUT>';
}

function mockIOSNativeObservation(platform: IOSPlatform): void {
  const driver = platform.driver as any;
  (platform as any).springBoardRoot = 'springboard-root';
  let settings: Record<string, unknown> = {};
  driver.updateSettings = async (value: Record<string, unknown>) => { settings = { ...settings, ...value }; return null; };
  driver.settings = async () => settings;
  driver.mobile = async (name: string) => { assert.equal(name, 'queryAppState'); return 4; };
  driver.command = async (path: string) => {
    if (path === '/element/springboard-root/elements') return [{ 'element-6066-11e4-a52e-4f735466cecf': 'springboard-root' }];
    assert.equal(path, '/alert/text');
    throw new WebDriverError({
      code: 'APPIUM_COMMAND', message: 'HTTP 404: {"error":"no such alert"}', status: 404,
      path, method: 'GET', durationMs: 0, selectedContext: 'NATIVE_APP', selectedWindow: '',
    });
  };
}

function iosShareHierarchy(scrolls: number, populated = true): string {
  if (!populated) return '<AppiumAUT><XCUIElementTypeApplication name="Safari" bundleId="com.apple.mobilesafari"><XCUIElementTypeOther name="ActivityListView" visible="true"><XCUIElementTypeOther name="ShareSheet.RemoteContainerView" visible="true"/></XCUIElementTypeOther></XCUIElementTypeApplication></AppiumAUT>';
  const shift = Math.min(scrolls, 2) * 60;
  const targetY = 907 - shift;
  const targetVisible = scrolls >= 2 ? 'true' : 'false';
  const rows = [
    ['Copy', 645 - shift],
    ['Add to Reading List', 706 - shift],
    ['Add Bookmark', 756 - shift],
    ['Add to Favorites', 806 - shift],
    ['Add to Home Screen', targetY],
  ];
  const cells = rows.map(([label, y]) => `<XCUIElementTypeCell name="actionGroupCell" label="${label}" enabled="true" visible="${label === 'Add to Home Screen' ? targetVisible : 'true'}" x="16" y="${y}" width="361" height="51"/>`).join('');
  const horizontalStrip = '<XCUIElementTypeScrollView name="share-apps-strip" enabled="true" visible="true" x="8" y="513" width="377" height="133"><XCUIElementTypeCell name="shareCell" label="Add to Home Screen" enabled="true" visible="true" x="8" y="513" width="78" height="133"/></XCUIElementTypeScrollView>';
  const browserBar = '<XCUIElementTypeOther name="Vertical scroll bar, 2 pages" value="0%" enabled="true" visible="true" x="360" y="0" width="30" height="398"/>';
  return `<AppiumAUT><XCUIElementTypeApplication name="Safari" bundleId="com.apple.mobilesafari">${browserBar}<XCUIElementTypeOther name="ActivityListView" visible="true"><XCUIElementTypeOther name="ShareSheet.RemoteContainerView" visible="true"><XCUIElementTypeCollectionView name="activityCollectionView" enabled="true" visible="true" x="0" y="398" width="393" height="454">${horizontalStrip}${cells}</XCUIElementTypeCollectionView></XCUIElementTypeOther></XCUIElementTypeOther></XCUIElementTypeApplication></AppiumAUT>`;
}

test('transient baseline downloads retry without accepting a failed response', async () => {
  const root = await mkdtemp(join(tmpdir(), 'herdr-mobile-download-'));
  const filename = join(root, 'baseline.tar.gz');
  let attempts = 0;
  await downloadWithRetry('https://example.invalid/baseline.tar.gz', filename, {
    fetchImpl: async () => {
      attempts += 1;
      return attempts === 1 ? new Response('temporary failure', { status: 500 }) : new Response('verified bytes', { status: 200 });
    },
    sleep: async () => undefined,
  });
  assert.equal(attempts, 2);
  assert.equal(await readFile(filename, 'utf8'), 'verified bytes');
});

test('repeated baseline download failure leaves no partial file', async () => {
  const root = await mkdtemp(join(tmpdir(), 'herdr-mobile-download-failure-'));
  const filename = join(root, 'baseline.tar.gz');
  await assert.rejects(
    downloadWithRetry('https://example.invalid/baseline.tar.gz', filename, {
      maxAttempts: 2,
      totalTimeoutMs: 1_000,
      fetchImpl: async () => new Response('server failure', { status: 503 }),
      sleep: async () => undefined,
    }),
    /HTTP 503/,
  );
  assert.equal(existsSync(filename), false);
});

test('checksum validation is terminal and leaves no downloaded artifact', async () => {
  const root = await mkdtemp(join(tmpdir(), 'herdr-mobile-download-checksum-'));
  const filename = join(root, 'baseline.tar.gz');
  let attempts = 0;
  await assert.rejects(
    downloadWithRetry('https://example.invalid/baseline.tar.gz', filename, {
      fetchImpl: async () => {
        attempts += 1;
        return new Response('wrong bytes', { status: 200 });
      },
      validate: () => { throw new Error('checksum mismatch'); },
      sleep: async () => undefined,
    }),
    /checksum mismatch/,
  );
  assert.equal(attempts, 1);
  assert.equal(existsSync(filename), false);
});

test('retention and composite checks cover named and unnamed steps', async () => {
  const named = `jobs:\n  build:\n    steps:\n      - name: named upload\n        uses: actions/upload-artifact@v4\n        with:\n          retention-days: 7\n`;
  const unnamed = `jobs:\n  build:\n    steps:\n      - uses: actions/upload-artifact@v4\n        with:\n          retention-days: \${{ inputs.retention }}\n`;
  const omitted = `jobs:\n  build:\n    steps:\n      - name: omitted upload\n        uses: actions/upload-artifact@v4\n`;
  const unnamedSeven = `jobs:\n  build:\n    steps:\n      - uses: actions/upload-artifact@v4\n        with:\n          retention-days: 7\n`;
  const valid = `jobs:\n  build:\n    steps:\n      - name: named upload\n        uses: actions/upload-artifact@v4\n        with:\n          retention-days: 1\n`;
  assert.equal(retentionIssues(named).length, 1);
  assert.equal(retentionIssues(unnamed).length, 1);
  assert.equal(retentionIssues(omitted).length, 1);
  assert.equal(retentionIssues(unnamedSeven).length, 1);
  assert.equal(retentionIssues(valid).length, 0);
  const missingShell = `runs:\n  using: composite\n  steps:\n    - run: echo test\n`;
  const validComposite = `runs:\n  using: composite\n  steps:\n    - run: echo test\n      shell: bash\n`;
  const reorderedComposite = `runs:\n  using: "composite"\n  steps:\n    - shell: bash\n      run: echo test\n`;
  assert.equal(compositeIssues(missingShell).length, 1);
  assert.equal(compositeIssues(validComposite).length, 0);
  assert.equal(compositeIssues(reorderedComposite).length, 0);
  assert.match(compositeIssues('runs: [').at(0)?.message || '', /invalid YAML/);
});

test('iOS standalone ownership accepts bundle and pid evidence without Android activity', async () => {
  const identity: RuntimeIdentity = {
    url: 'https://fixture.example/', origin: 'https://fixture.example', standalone: true,
    provider: 'ios-home-screen', nativeProvider: 'ios:com.apple.webapp', nativePid: '42',
    version: '0.20.10', assets: 363, build: 'build', entry: '/', script: '/app.js', style: '/app.css',
    requiredAssetsReady: true, applicationInitialized: true,
  };
  assert.doesNotThrow(() => assertStandaloneOwnership(identity, 'https://fixture.example'));
  assert.throws(() => assertStandaloneOwnership({ ...identity, nativeProvider: 'ios:com.apple.mobilesafari' }, 'https://fixture.example'), /STANDALONE_PROVIDER_REQUIRED/);
});

test('evidence validation enforces matrix identity and platform-specific native proof', async () => {
  const root = await mkdtemp(join(tmpdir(), 'herdr-mobile-evidence-'));
  const sourceCommit = 'a'.repeat(40);
  const headSha = 'b'.repeat(40);
  const webHash = 'c'.repeat(64);
  const expectedBaselineIdentity = {
    version: '0.20.10', assets: 363, build: 'old-build', entry: '/old/index.html',
    script: '/assets/old.js', style: '/assets/old.css', webHash: 'd'.repeat(64),
  };
  const expectedCandidateIdentity = {
    version: '0.21.0', assets: 364, build: 'new-build', entry: '/new/index.html',
    script: '/assets/new.js', style: '/assets/new.css', webHash,
  };
  const result = {
    schema: 1, result: 'passed', suite: 'release', platform: 'ios', baseline: '0.20.10', candidate: 'candidate-0.20.10',
    origin: 'https://fixture.test', source_commit: sourceCommit, source_run_head_sha: headSha, candidate_web_hash: webHash,
    initial_identity: {
      standalone: true, provider: 'ios-home-screen', nativeProvider: 'ios:com.apple.webapp', nativePid: '42',
      url: 'https://fixture.test/old', origin: 'https://fixture.test', ...expectedBaselineIdentity,
      requiredAssetsReady: true, applicationInitialized: true,
    },
    final_identity: {
      standalone: true, provider: 'ios-home-screen', nativeProvider: 'ios:com.apple.webapp', nativePid: '42',
      url: 'https://fixture.test/new', origin: 'https://fixture.test', ...expectedCandidateIdentity,
      buildFromApplication: true, requiredAssetsReady: true, applicationInitialized: true,
    },
    credential_preserved: true,
    credential_evidence: { relays: {
      alpha: { invitationAuthCount: 1, credentialAuthCount: 2, credentialPseudonyms: ['one'], connections: 1 },
      beta: { invitationAuthCount: 1, credentialAuthCount: 2, credentialPseudonyms: ['two'], connections: 1 },
    } },
    preference_preserved: true,
    lifecycle_launch_count: 1,
    oracle_controls: [],
    phone_completion: { rawPlanPresent: true, phoneRequired: true, phoneAcknowledged: true, phoneState: 'loaded', visibleCompletion: true },
    faults_exercised: ['corrupt:/assets/new.js'],
    fault_identity: { id: 'id', generation: 'generation', kind: 'corrupt', path: '/assets/new.js' },
    fixture_requests: [{ release: 'candidate', path: '/assets/new.js', fault: 'corrupt', fault_id: 'id', fault_generation: 'generation' }],
  };
  await writeSanitizedJson(join(root, 'mobile-result.json'), result);
  const serialized = JSON.parse(await readFile(join(root, 'mobile-result.json'), 'utf8')) as typeof result;
  assert.equal(serialized.credential_evidence.relays.alpha.invitationAuthCount, 1);
  assert.equal(serialized.credential_evidence.relays.beta.credentialAuthCount, 2);
  const options = {
    directory: root,
    matrix: [{ platform: 'ios', baseline: '0.20.10', scenario: 'historical' }],
    suite: 'release', candidateCommit: sourceCommit, sourceRunHeadSha: headSha, candidateWebHash: webHash,
    candidateIdentity: expectedCandidateIdentity,
    baselineIdentities: [{ name: '0.20.10', identity: expectedBaselineIdentity }],
  };
  await validateMobileEvidence(options);
  const secretSentinel = 'serialization-secret-value';
  await writeSanitizedJson(join(root, 'mobile-result.json'), {
    ...result,
    diagnostics: {
      invitation: secretSentinel,
      nested: { token: secretSentinel, private_key: secretSentinel },
      escapedUrl: `https://fixture.test/#invite=${secretSentinel}`,
    },
  });
  assertNoKnownSecret(await readFile(join(root, 'mobile-result.json'), 'utf8'), [secretSentinel]);
  await validateMobileEvidence(options);
  const invalidCounterResult = {
    ...result,
    credential_evidence: {
      relays: {
        ...result.credential_evidence.relays,
        alpha: { ...result.credential_evidence.relays.alpha, invitationAuthCount: 'counter-secret' },
      },
    },
  };
  await writeSanitizedJson(join(root, 'mobile-result.json'), invalidCounterResult);
  const sanitizedInvalidCounterResult = JSON.parse(await readFile(join(root, 'mobile-result.json'), 'utf8')) as typeof invalidCounterResult;
  assert.equal(sanitizedInvalidCounterResult.credential_evidence.relays.alpha.invitationAuthCount, '[REDACTED]');
  assert.equal(sanitizedInvalidCounterResult.credential_evidence.relays.beta.invitationAuthCount, 1);
  await assert.rejects(validateMobileEvidence(options), /alpha.invitationAuthCount must be an integer/);
  await writeSanitizedJson(join(root, 'mobile-result.json'), result);
  await writeFile(join(root, 'mobile-result.json'), JSON.stringify({
    ...result,
    fixture_requests: [...result.fixture_requests, { ...result.fixture_requests[0] }],
  }));
  await validateMobileEvidence(options);
  await writeFile(join(root, 'mobile-result.json'), JSON.stringify({ ...result, phone_completion: { ...result.phone_completion, rawPlanPresent: false } }));
  await assert.rejects(validateMobileEvidence(options), /raw phone-plan/);
  const historicalBaselineIdentity = {
    ...expectedBaselineIdentity, version: '0.20.8', assets: 361, build: 'historical-build',
    entry: '/historical/index.html', script: '/assets/historical.js', style: '/assets/historical.css', webHash: 'e'.repeat(64),
  };
  const historicalResult = {
    ...result,
    baseline: '0.20.8',
    initial_identity: { ...result.initial_identity, ...historicalBaselineIdentity },
    oracle_controls: ['HISTORICAL_PHONE_ACCOUNTING_UNAVAILABLE:0.20.8'],
    phone_completion: { rawPlanPresent: true, phoneRequired: false, phoneAcknowledged: false, phoneState: 'failed', visibleCompletion: false },
  };
  const historicalOptions = {
    ...options,
    matrix: [{ platform: 'ios', baseline: '0.20.8', scenario: 'historical' }],
    baselineIdentities: [{ name: '0.20.8', identity: historicalBaselineIdentity }],
  };
  await writeFile(join(root, 'mobile-result.json'), JSON.stringify(historicalResult));
  await validateMobileEvidence(historicalOptions);
  await writeFile(join(root, 'mobile-result.json'), JSON.stringify({ ...historicalResult, phone_completion: { ...historicalResult.phone_completion, rawPlanPresent: false } }));
  await validateMobileEvidence(historicalOptions);
  await writeFile(join(root, 'mobile-result.json'), JSON.stringify({ ...result, origin: 'http://fixture.test' }));
  await assert.rejects(validateMobileEvidence(options), /HTTPS origin/);
  await writeFile(join(root, 'mobile-result.json'), JSON.stringify({ ...result, final_identity: { ...result.final_identity, build: 'wrong-build' } }));
  await assert.rejects(validateMobileEvidence(options), /final identity\.build|RUNTIME_BUILD_MISMATCH/);
  await writeFile(join(root, 'mobile-result.json'), JSON.stringify({ ...result, fault_identity: { ...result.fault_identity, id: 'other' } }));
  await assert.rejects(validateMobileEvidence(options), /unrelated id/);
  await writeFile(join(root, 'mobile-result.json'), JSON.stringify(result));
  await assert.rejects(validateMobileEvidence({ ...options, candidateWebHash: 'd'.repeat(64) }), /wrong candidate web hash/);
  await mkdir(join(root, 'duplicate'), { recursive: true });
  await writeFile(join(root, 'duplicate', 'mobile-result.json'), JSON.stringify(result));
  await assert.rejects(validateMobileEvidence(options), /expected 1 result files, found 2/);
  await writeFile(join(root, 'mobile-result.json'), JSON.stringify({ ...result, platform: 'android' }));
  await rm(join(root, 'duplicate'), { recursive: true, force: true });
  await assert.rejects(validateMobileEvidence({ ...options, matrix: [{ platform: 'android', baseline: '0.20.10', scenario: 'historical' }] }), /invalid android provider|nativeActivity/);
});

test('final evidence checkout precedes artifact download', async () => {
  const workflow = await readFile(join(repositoryRoot, '.github/workflows/mobile-ci.yml'), 'utf8');
  const gate = workflow.slice(workflow.indexOf('\n  gate:'));
  const checkout = gate.indexOf('Check out the verified source for final evidence validation');
  const download = gate.indexOf('Download current-attempt device evidence artifacts');
  assert.ok(checkout >= 0 && download >= 0 && checkout < download);
});

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

test('Android environment events require authoritative package or process evidence', async () => {
  assert.equal(forcedRestartEvents('ordinary package com.example changed').length, 0);
  assert.equal(forcedRestartEvents('09-10 08:45:09.464 6538 7277 I DynamiteLoaderV2Impl: Module config changed, forcing restart due to module googlecertificates', { '6538': 'com.android.chrome' }).length, 1);
  assert.equal(forcedRestartEvents('09-10 08:45:09.464 6538 7277 I DynamiteLoaderV2Impl: Module config changed, forcing restart due to module googlecertificates', { '6538': 'com.example.other' }).length, 0);
});

interface AndroidEnvironmentFixture {
  root: string;
  fixtureDirectory: string;
  log: string;
  environment: NodeJS.ProcessEnv;
}

function androidPackageDump(
  packageName: string,
  versionName: string,
  versionCode: string,
  codePath: string,
  extra = '',
): string {
  return [
    'Packages:',
    `Package [${packageName}] (fixture):`,
    `  codePath=${codePath}`,
    `  resourcePath=${codePath}`,
    `  versionCode=${versionCode} minSdk=23 targetSdk=35`,
    `  versionName=${versionName}`,
    '  installerPackageName=com.android.shell',
    '  initiatingPackageName=com.android.shell',
    '  originatingPackageName=com.android.shell',
    '  packageSource=1',
    '  lastUpdateTime=2026-01-01 00:00:00',
    '  flags=[ SYSTEM HAS_CODE ]',
    '  splits=[base]',
    extra,
    '  User 0: installed=true hidden=false suspended=false stopped=false enabled=0',
    '    firstInstallTime=2026-01-01 00:00:00',
  ].filter(Boolean).map((line, index) => index ? `  ${line}` : line).join('\n') + `\nQueries:\n\nCompiler stats:\n  [${packageName}]\n    (No recorded stats)\n`;
}

async function writeAndroidEnvironmentFixtureState(
  directory: string,
  state: string,
  options: { chromeDependencies?: string[]; changedPaths?: boolean; recordedRun?: string } = {},
): Promise<void> {
  const changed = options.changedPaths === true;
  const gmsPath = changed ? '/data/app/~~changed/com.google.android.gms-changed' : '/data/app/~~fixture/com.google.android.gms-fixture';
  const chromePath = changed ? '/data/app/~~changed/com.android.chrome-changed' : '/data/app/~~fixture/com.android.chrome-fixture';
  const fixtures = repositoryPath('tests/mobile/unit/fixtures');
  const run = options.recordedRun || '34478620554';
  const recordedDump = await readFile(join(fixtures, `android-recorded-${run}-trichrome.dump`), 'utf8');
  const recordedContext = await readFile(join(fixtures, `android-recorded-${run}-chrome-context.dump`), 'utf8');
  const recordedPath = recordedDump.match(/codePath=(\S+)/u)![1];
  const libraryPath = changed ? '/data/app/~~changed==/com.google.android.trichromelibrary-changed==' : recordedPath;
  const libraryApkPath = recordedContext.match(/usesLibraryFiles:\n\s+(\S+)/u)![1].replace(recordedPath, libraryPath);
  const libraryDump = recordedDump.replaceAll(recordedPath, libraryPath);
  const listing = (await readFile(join(fixtures, `android-source-derived-${run}-libraries.list`), 'utf8')).replaceAll(recordedPath, libraryPath);
  const dependencies = options.chromeDependencies === undefined
    ? ['com.google.android.trichromelibrary version:677820038']
    : options.chromeDependencies;
  const chromeSource = (await readFile(join(fixtures, 'android15-chrome-package.txt'), 'utf8')).replace('    flags=', '    splits=[base]\n    flags=');
  const chromeDump = chromeSource
    .replaceAll('/data/app/~~fixture/com.android.chrome-fixture', chromePath)
    .replaceAll('/data/app/~~fixture/com.google.android.trichromelibrary-fixture', libraryPath)
    .replace('    usesStaticLibraries:\n      com.google.android.trichromelibrary version:677820038\n',
      dependencies.length ? `    usesStaticLibraries:\n${dependencies.map((dependency) => `      ${dependency}`).join('\n')}\n` : '');
  const gmsDump = androidPackageDump(
    'com.google.android.gms',
    changed ? '24.99.99' : '24.23.35',
    changed ? '249999999' : '242335041',
    gmsPath,
  );
  await Promise.all([
    writeFile(join(directory, `${state}-gms.dump`), gmsDump),
    writeFile(join(directory, `${state}-chrome.dump`), chromeDump),
    writeFile(join(directory, `${state}-trichrome.dump`), libraryDump),
    writeFile(join(directory, `${state}-gms.path`), `package:${gmsPath}/base.apk\n`),
    writeFile(join(directory, `${state}-chrome.path`), `package:${chromeDump.match(/codePath=(\S+)/u)![1]}/base.apk\n`),
    writeFile(join(directory, `${state}-trichrome.list`), listing),
    writeFile(join(directory, `${state}-trichrome.file`), `'${libraryApkPath}'`),
  ]);
}

test('Android producer-shaped package sections are scoped, heading-relative and diagnostic context is bounded', async () => {
  const dump = await readFile(join(repositoryRoot, 'tests/mobile/unit/fixtures/android15-chrome-package.txt'), 'utf8');
  const resolve = (source: string) => resolveStaticLibraryPackage(source, 'com.google.android.trichromelibrary', '131.0.6778.200 (677820038)');
  assert.equal(resolve(dump).versionCode, '677820038');
  assert.equal(resolve(dump.replace(/^/gmu, '    ')).versionCode, '677820038');
  assert.equal(resolve(dump.replaceAll('\n', '\r\n')).versionCode, '677820038');
  assert.throws(() => resolve(dump.replace('Packages:', 'Inactive packages:')), /active package record.*missing/u);
  assert.throws(() => resolve(dump.replace('      com.google.android.trichromelibrary version:677820038', '      broken dependency')), /malformed/u);
  assert.throws(() => resolve(dump.replace('    usesOptionalLibraries:', '    usesStaticLibraries:')), /malformed|ambiguous/u);
  assert.throws(() => resolve(dump.replace('Package [com.example.other]', 'Package [com.android.chrome]')), /ambiguous/u);
  assert.throws(() => resolve(dump.replace('      com.google.android.trichromelibrary version:677820038\n', '')), /dependency.*missing/u);
  assert.throws(() => resolve('x'.repeat(2_000_001) + dump), /parsing limit/u);
  const context = androidPackageContext('Resolver preamble\n'.repeat(5_000) + dump);
  assert.ok(context.includes('usesStaticLibraries:'));
  assert.ok(context.includes('usesOptionalLibraries:'));
  assert.ok(context.includes('version:677820038'));
  assert.ok(context.length <= 4_000);
});

async function createAndroidEnvironmentFixture(): Promise<AndroidEnvironmentFixture> {
  const root = await mkdtemp(join(process.env.ANDROID_TEST_OUTPUT || tmpdir(), 'herdr-android-environment-'));
  const fixtureDirectory = join(root, 'fixtures');
  const binDirectory = join(root, 'bin');
  await mkdir(fixtureDirectory, { recursive: true });
  await mkdir(binDirectory, { recursive: true });
  await writeAndroidEnvironmentFixtureState(fixtureDirectory, 'valid');
  await writeAndroidEnvironmentFixtureState(fixtureDirectory, 'changed', { changedPaths: true });
  await writeAndroidEnvironmentFixtureState(fixtureDirectory, 'missing', { chromeDependencies: [] });
  await writeAndroidEnvironmentFixtureState(fixtureDirectory, 'ambiguous', {
    chromeDependencies: [
      'com.google.android.trichromelibrary version:677820038',
      'com.google.android.trichromelibrary version:677820039',
    ],
  });
  await writeAndroidEnvironmentFixtureState(fixtureDirectory, 'wrong-version', {
    chromeDependencies: ['com.google.android.trichromelibrary version:677820039'],
  });
  for (const run of ['34478620554', '34478627478']) {
    await writeAndroidEnvironmentFixtureState(fixtureDirectory, `recorded-${run}`, { recordedRun: run });
  }
  for (const state of ['adb-failure', 'adb-timeout', 'listing-failure', 'listing-timeout', 'missing-file', 'file-timeout', 'module-change']) {
    await writeAndroidEnvironmentFixtureState(fixtureDirectory, state);
  }
  const gms = await readFile(join(fixtureDirectory, 'module-change-gms.dump'), 'utf8');
  await writeFile(join(fixtureDirectory, 'module-change-gms.dump'), gms.replace('    flags=', '    usesLibraryFiles:\n      /data/app/module-changed/base.apk\n    flags='));
  const log = join(root, 'adb.log');
  const adb = join(binDirectory, 'adb');
  await writeFile(adb, `#!${process.execPath}\nimport ${JSON.stringify(repositoryPath('tests/mobile/unit/android-fake-adb.ts'))};\n`, { mode: 0o700 });
  await writeFile(join(root, 'ownership'), 'android:emulator-5554\n');
  await mkdir(join(root, 'avd', 'herdr-mobile-ci-fixture.avd'), { recursive: true });
  await mkdir(join(root, 'sdk', 'system-images', 'android-35', 'google_apis', 'x86_64'), { recursive: true });
  await writeFile(join(root, 'avd', 'herdr-mobile-ci-fixture.avd', 'config.ini'), 'image.sysdir.1=system-images/android-35/google_apis/x86_64/\ntag.id=google_apis\nabi.type=x86_64\n');
  await writeFile(join(root, 'sdk', 'system-images', 'android-35', 'google_apis', 'x86_64', 'source.properties'), 'Pkg.Revision=12\nAndroidVersion.ApiLevel=35\nSystemImage.TagId=google_apis\nSystemImage.Abi=x86_64\n');
  await writeFile(join(binDirectory, 'emulator'), '#!/bin/sh\nif [ "$*" != "-no-window -version" ]; then\n  printf "qemu-system-x86_64: error while loading shared libraries: libpulse.so.0\\n" >&2\n  exit 127\nfi\nprintf "Android emulator version 35.0.2.0\\n"\n', { mode: 0o700 });
  const properties = {
    'ro.build.fingerprint': 'fixture/fingerprint',
    'ro.build.id': 'AP4A',
    'ro.build.version.incremental': 'fixture',
    'ro.build.version.release': '15',
    'ro.build.version.sdk': '35',
    'ro.product.name': 'sdk_gphone',
    'ro.product.device': 'emu64x86-64',
    'ro.kernel.qemu': '1',
    'ro.synthetic.padding': 'x'.repeat(4100),
    'ro.synthetic.brackets': '[source-derived]',
    'ro.synthetic.multiline': 'first\nsecond',
    'ro.synthetic.framing': 'first]\n[ro.synthetic.other]: [second',
  };
  await writeFile(join(fixtureDirectory, 'properties.json'), JSON.stringify(properties));
  await writeFile(join(fixtureDirectory, 'getprop'), Object.entries(properties).map(([name, value]) => `[${name}]: [${value}]\n`).join(''));
  return {
    root,
    fixtureDirectory,
    log,
    environment: {
      ...process.env,
      PATH: `${binDirectory}:${process.env.PATH || ''}`,
      FAKE_ANDROID_FIXTURE_DIR: fixtureDirectory,
      FAKE_ANDROID_LOG: log,
      ANDROID_AVD_NAME: 'herdr-mobile-ci-fixture',
      ANDROID_HOME: join(root, 'sdk'),
      ANDROID_AVD_HOME: join(root, 'avd'),
      MOBILE_DEVICE_OWNERSHIP_FILE: join(root, 'ownership'),
    },
  };
}

async function runAndroidEnvironmentFixtureSnapshot(
  fixture: AndroidEnvironmentFixture,
  state: string,
  output: string,
  diagnostics: string,
  timeoutMs?: number,
): Promise<{ passed: boolean; stderr: string }> {
  await writeFile(fixture.log, '');
  const args = [
    process.env.ANDROID_ENVIRONMENT_SOURCE || 'tests/mobile/android-environment.ts', 'snapshot',
    '--serial', 'emulator-5554',
    '--toolchains', process.env.ANDROID_ENVIRONMENT_TOOLCHAINS || repositoryPath('tests/mobile/toolchains.json'),
    '--boundary', output.endsWith('before.json') ? 'start' : 'end', '--measurement', 'android-test',
    '--output', output,
    '--diagnostics', diagnostics,
  ];
  if (timeoutMs !== undefined) args.push('--adb-timeout-ms', String(timeoutMs));
  try {
    execFileSync('bun', args, {
      cwd: repositoryRoot,
      env: { ...fixture.environment, FAKE_ANDROID_STATE: state },
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    return { passed: true, stderr: '' };
  } catch (error) {
    const childError = error as { stderr?: string | Buffer; stdout?: string | Buffer };
    return { passed: false, stderr: String(childError.stderr || childError.stdout || '') };
  }
}

async function readAndroidAcquisitionDiagnostics(filename: string): Promise<AndroidEnvironmentAcquisitionDiagnostics> {
  return JSON.parse(await readFile(filename, 'utf8')) as AndroidEnvironmentAcquisitionDiagnostics;
}

async function runAndroidEnvironmentFixtureCheck(
  fixture: AndroidEnvironmentFixture,
  before: string,
  after: string,
  log = '',
): Promise<{ passed: boolean; issues: string[] }> {
  const logFile = join(fixture.root, 'qualification.log');
  const output = join(fixture.root, 'check.json');
  await writeFile(logFile, `09-10 08:45:00.000 2000 2000 I HerdrMeasure: android-test START\n${log}09-10 08:46:00.000 2000 2000 I HerdrMeasure: android-test END\n`);
  await writeFile(join(fixture.root, 'operations.json'), '[]');
  let passed = true;
  try {
    execFileSync('bun', [
      process.env.ANDROID_ENVIRONMENT_SOURCE || 'tests/mobile/android-environment.ts', 'check', '--before', before, '--after', after, '--log', logFile, '--output', output,
      '--operations', join(fixture.root, 'operations.json'),
    ], { cwd: repositoryRoot, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch {
    passed = false;
  }
  const result = JSON.parse(await readFile(output, 'utf8')) as { passed: boolean; issues: string[] };
  assert.equal(result.passed, passed);
  return result;
}

test('Android recorded package evidence remains complete and separate from source-derived listings', async () => {
  const fixtures = repositoryPath('tests/mobile/unit/fixtures');
  const contract = JSON.parse(await readFile(join(fixtures, 'android-static-library-contract.json'), 'utf8')) as {
    recorded: Array<{ file: string; bytes: number; sha256: string }>;
  };
  assert.equal(contract.recorded.length, 4);
  for (const entry of contract.recorded) {
    const path = join(fixtures, entry.file);
    assert.equal((await readFile(path)).length, entry.bytes);
    assert.equal(await fileSha256(path), entry.sha256);
  }
  for (const run of ['34478620554', '34478627478']) {
    const context = await readFile(join(fixtures, `android-recorded-${run}-chrome-context.dump`), 'utf8');
    assert.equal(resolveStaticLibraryPackage(context, 'com.google.android.trichromelibrary', '131.0.6778.200 (677820038)').packageRecordName,
      'com.google.android.trichromelibrary_677820038');
    const sourcePath = context.match(/usesLibraryFiles:\n\s+(\S+)/u)![1];
    const listing = await readFile(join(fixtures, `android-source-derived-${run}-libraries.list`), 'utf8');
    assert.ok(listing.includes(`package:${sourcePath}=com.google.android.trichromelibrary versionCode:677820038\n`));
  }
});

for (const state of ['valid', 'recorded-34478620554', 'recorded-34478627478']) {
  test(`Android environment snapshot CLI acquires and checks ${state} metadata with source-derived library listing`, async () => {
    const fixture = await createAndroidEnvironmentFixture();
    for (const packageName of ['com.google.android.trichromelibrary', 'com.google.android.trichromelibrary_677820038']) {
      await assert.rejects(command('adb', ['-s', 'emulator-5554', 'shell', 'pm', 'path', packageName], 5_000, { env: fixture.environment }),
        (error: { exitCode?: number; stdout?: string; stderr?: string; timedOut?: boolean }) => {
          assert.equal(error.exitCode, 1);
          assert.equal(error.stdout, '');
          assert.equal(error.stderr, '');
          assert.equal(error.timedOut, false);
          return true;
        });
    }
    const beforeFile = join(fixture.root, 'outputs', 'before.json');
    const beforeDiagnosticsFile = join(fixture.root, 'outputs', 'before-diagnostics.json');
    const afterFile = join(fixture.root, 'outputs', 'after.json');
    const afterDiagnosticsFile = join(fixture.root, 'outputs', 'after-diagnostics.json');
    const result = await runAndroidEnvironmentFixtureSnapshot(fixture, state, beforeFile, beforeDiagnosticsFile);
    assert.equal(result.passed, true, result.stderr);
    assert.equal((await runAndroidEnvironmentFixtureSnapshot(fixture, state, afterFile, afterDiagnosticsFile)).passed, true);
    const before = JSON.parse(await readFile(beforeFile, 'utf8')) as AndroidEnvironmentSnapshot;
    assert.deepEqual(Object.keys(before.packages).sort(), ['com.android.chrome', 'com.google.android.gms', 'com.google.android.trichromelibrary']);
    const library = before.packages['com.google.android.trichromelibrary'];
    const observedSource = (await readFile(join(fixture.fixtureDirectory, `${state}-trichrome.file`), 'utf8')).slice(1, -1);
    assert.equal(library.packageRecordName, 'com.google.android.trichromelibrary_677820038');
    assert.equal(library.staticLibraryName, 'com.google.android.trichromelibrary');
    assert.equal(library.staticLibraryVersion, '677820038');
    assert.equal(library.versionCode, '677820038');
    assert.equal(library.versionName, '131.0.6778.200');
    assert.deepEqual(library.apkPaths, [`package:${observedSource}`]);
    assert.equal(library.codePath, observedSource.slice(0, -'/base.apk'.length));
    assert.equal(library.dumpSha256, await fileSha256(join(fixture.fixtureDirectory, `${state}-trichrome.dump`)));
    const diagnostics = await readAndroidAcquisitionDiagnostics(beforeDiagnosticsFile);
    assert.equal(diagnostics.resolvedStaticLibrary?.packageRecordName, library.packageRecordName);
    assert.equal(diagnostics.failure, undefined);
    const metadata = diagnostics.commands.find((entry) => entry.args.includes('dumpsys') && entry.args.includes(library.packageRecordName));
    assert.equal(metadata?.stdoutBytes, 2_842);
    assert.equal(metadata?.stdoutPreview, await readFile(join(fixture.fixtureDirectory, `${state}-trichrome.dump`), 'utf8'));
    assert.equal(metadata?.stdoutSha256, library.dumpSha256);
    const chromeContext = diagnostics.commands.find((entry) => entry.args.includes('dumpsys') && entry.args.includes('com.android.chrome'))?.packageContext;
    assert.ok(chromeContext);
    assert.ok(chromeContext.includes('usesOptionalLibraries:'));
    assert.ok(chromeContext.includes('version:677820038'));
    assert.ok(chromeContext.length <= 4_000);
    const requests = await readFile(fixture.log, 'utf8');
    assert.equal(requests.includes('pm path com.google.android.trichromelibrary'), false);
    assert.ok(requests.includes('pm list packages --match-libraries -f --show-versioncode --user 0 com.google.android.trichromelibrary\n'));
    assert.ok(requests.includes(`shell test -f '${observedSource}'\n`));
    assert.deepEqual((await runAndroidEnvironmentFixtureCheck(fixture, beforeFile, afterFile)).issues, []);
    const restart = await runAndroidEnvironmentFixtureCheck(fixture, beforeFile, afterFile, '09-10 08:45:09.464 6538 7277 I DynamiteLoaderV2Impl: Module config changed, forcing restart due to module googlecertificates\n');
    assert.equal(restart.passed, false);
    assert.ok(restart.issues.includes('native dependency replacement or forced restart was observed'));
    if (state !== 'valid') return;
    assert.equal((await runAndroidEnvironmentFixtureSnapshot(fixture, 'changed', afterFile, afterDiagnosticsFile)).passed, true);
    const changed = await runAndroidEnvironmentFixtureCheck(fixture, beforeFile, afterFile);
    assert.equal(changed.passed, false);
    assert.ok(changed.issues.includes('com.google.android.gms versionCode changed'));
    assert.ok(changed.issues.includes('com.google.android.trichromelibrary codePath changed'));
    assert.ok(changed.issues.includes('com.google.android.trichromelibrary APK paths changed'));
    assert.equal((await runAndroidEnvironmentFixtureSnapshot(fixture, 'module-change', afterFile, afterDiagnosticsFile)).passed, true);
    const module = await runAndroidEnvironmentFixtureCheck(fixture, beforeFile, afterFile);
    assert.equal(module.passed, false);
    assert.ok(module.issues.includes('com.google.android.gms dependency configuration changed'));
    assert.ok(module.issues.includes('com.google.android.gms dependencyConfigSha256 changed'));
  });
}

test('Android environment snapshot persists bounded diagnostics for static-library acquisition failures', async () => {
  const fixture = await createAndroidEnvironmentFixture();
  const mutations: Array<[string, string, (source: string) => string, RegExp]> = [
    ['empty-listing', 'list', () => '', /listing is missing/u],
    ['absent-version', 'list', (source) => source.split('\n').filter((line) => !line.endsWith('versionCode:677820038')).join('\n'), /source path is missing/u],
    ['wrong-public-name', 'list', (source) => source.replaceAll('=com.google.android.trichromelibrary ', '=com.google.android.trichromelibrary_677820038 '), /source path is missing/u],
    ['inexact-version', 'list', (source) => source.replaceAll('versionCode:677820038', 'versionCode:0677820038'), /source path is missing/u],
    ['duplicate-listing', 'list', (source) => source + source, /source path is ambiguous/u],
    ['ambiguous-path', 'list', (source) => source + source.replaceAll('/base.apk', '/other.apk'), /source path is ambiguous/u],
    ['malformed-listing', 'list', (source) => source + 'package:/truncated.apk=com.google.android.trichromelibrary versionCode:\n', /malformed/u],
    ['truncated-listing', 'list', (source) => source.trimEnd(), /truncated/u],
    ['truncated-version', 'list', (source) => source.replace('versionCode:677820038', 'versionCode:67782003'), /source path is missing/u],
    ['blank-record', 'list', (source) => source + '\n', /malformed/u],
    ['oversized-listing', 'list', () => 'x'.repeat(2_000_001), /parsing limit/u],
    ['relative-path', 'list', (source) => source.replaceAll('package:/', 'package:'), /malformed/u],
    ['path-traversal', 'list', (source) => source.replaceAll('/base.apk', '/../base.apk'), /malformed/u],
    ['wrong-source', 'list', (source) => source.replaceAll('/base.apk', '/split.apk'), /source path does not match/u],
    ['code-path-change', 'dump', (source) => source.replace('codePath=/data/app/', 'codePath=/wrong/'), /source path does not match/u],
    ['identity-change', 'dump', (source) => source.replace('Package [com.google.android.trichromelibrary_677820038]', 'Package [com.google.android.trichromelibrary_677820039]'), /package record does not match/u],
    ['inactive-record', 'dump', (source) => source.replace('Packages:', 'Hidden system packages:'), /package record does not match/u],
    ['duplicate-record', 'dump', (source) => source + source, /ambiguous/u],
    ['metadata-name', 'dump', (source) => source.replace('name:com.google.android.trichromelibrary', 'name:com.example.library'), /static library metadata does not match/u],
    ['metadata-version', 'dump', (source) => source.replace('version:677820038', 'version:677820039'), /static library metadata does not match/u],
    ['package-version', 'dump', (source) => source.replace('versionCode=677820038', 'versionCode=677820039'), /pinned library identity/u],
    ['package-version-name', 'dump', (source) => source.replace('versionName=131.0.6778.200', 'versionName=131.0.6778.201'), /pinned library identity/u],
    ['split-layout', 'dump', (source) => source.replace('splits=[base]', 'splits=[base, config.x86_64]'), /split layout/u],
    ['missing-splits', 'dump', (source) => source.replace('    splits=[base]\n', ''), /splits|split layout/u],
    ['not-installed', 'dump', (source) => source.replace('installed=true', 'installed=false'), /not installed for user 0/u],
    ['wrong-user', 'dump', (source) => source.replace('User 0:', 'User 10:'), /not installed for user 0/u],
  ];
  const cases: Array<[string, RegExp, (diagnostics: AndroidEnvironmentAcquisitionDiagnostics) => void]> = [
    ['missing', /static library dependency .* is missing/u, () => undefined],
    ['ambiguous', /static library dependency .* is ambiguous/u, () => undefined],
    ['wrong-version', /version .* does not match/u, () => undefined],
    ...['adb-failure', 'adb-timeout', 'listing-failure', 'listing-timeout', 'missing-file', 'file-timeout'].map((state): typeof cases[number] => [
      state, /COMMAND_FAILED/u, (diagnostics) => {
        const failed = diagnostics.commands.at(-1)!;
        assert.equal(failed.outcome, 'failed');
        assert.equal(failed.timedOut, state.endsWith('timeout'));
        assert.equal(diagnostics.failure?.timedOut, failed.timedOut);
        assert.equal(diagnostics.failure?.exitCode, failed.exitCode);
        assert.ok(diagnostics.failure?.message.includes(failed.args.map((arg) => JSON.stringify(arg)).join(' ')));
        if (state.startsWith('listing-')) assert.ok(failed.args.includes('--match-libraries'));
        if (state === 'missing-file' || state === 'file-timeout') assert.ok(failed.args.includes('test'));
        if (state === 'listing-failure') assert.ok(diagnostics.failure?.detail?.includes('[REDACTED]'));
      },
    ]),
  ];
  for (const [state, extension, mutate, failure] of mutations) {
    await writeAndroidEnvironmentFixtureState(fixture.fixtureDirectory, state);
    const filename = join(fixture.fixtureDirectory, `${state}-trichrome.${extension}`);
    await writeFile(filename, mutate(await readFile(filename, 'utf8')));
    cases.push([state, failure, () => undefined]);
  }
  for (const [state, failure, check] of cases) {
    const output = join(fixture.root, `${state}.json`);
    const diagnosticsFile = join(fixture.root, `${state}-diagnostics.json`);
    const result = await runAndroidEnvironmentFixtureSnapshot(fixture, state, output, diagnosticsFile, state.endsWith('timeout') ? 500 : undefined);
    assert.equal(result.passed, false, state);
    const diagnostics = await readAndroidAcquisitionDiagnostics(diagnosticsFile);
    assert.ok(diagnostics.failure, state);
    assert.match(diagnostics.failure?.message || '', failure, state);
    check(diagnostics);
    assert.equal(existsSync(output), false, state);
    const requests = await readFile(fixture.log, 'utf8');
    assert.equal(requests.includes('pm path com.google.android.trichromelibrary'), false, state);
    assert.equal(requests.includes('pm list packages com.android.vending'), false, state);
    assert.ok(diagnostics.commands.length <= 64, state);
    for (const entry of diagnostics.commands) {
      assert.ok((entry.stdoutPreview?.length || 0) <= 4_000, state);
      assert.ok((entry.stderrPreview?.length || 0) <= 4_000, state);
    }
    assert.ok(Buffer.byteLength(JSON.stringify(diagnostics)) < 100_000, state);
    assert.equal(JSON.stringify(diagnostics).includes('android-fixture-secret'), false, state);
  }
});

for (const [name, body] of androidEnvironmentTests({
  createFixture: createAndroidEnvironmentFixture,
  writeState: writeAndroidEnvironmentFixtureState,
  snapshot: runAndroidEnvironmentFixtureSnapshot,
  check: runAndroidEnvironmentFixtureCheck,
})) test(name, body);

test('Android Chrome startup only enables attach mode after explicit launch', async () => {
  const ordinary = androidChromeCapabilities('emulator-5554');
  const attached = androidChromeCapabilities('emulator-5554', true);
  assert.equal('appium:androidUseRunningApp' in ordinary, false);
  assert.equal((ordinary['goog:chromeOptions'] as Record<string, unknown>).androidUseRunningApp, undefined);
  assert.equal((attached['goog:chromeOptions'] as Record<string, unknown>).androidUseRunningApp, true);
});

test('Android native settings are applied and read back before lookup', async () => {
  const platform = new AndroidPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
    certificate: '', setupUrl: '', deviceId: 'emulator-5554', budget: new PhaseBudget('android-settings-test', { timeoutMs: 1_000, recoveryLimit: 1 }),
  });
  const updates: Record<string, unknown>[] = [];
  (platform as any).driver = {
    updateSettings: async (settings: Record<string, unknown>) => { updates.push(settings); return { settings }; },
    settings: async () => ({ settings: { waitForIdleTimeout: 500, waitForSelectorTimeout: 0 } }),
  };
  await (platform as any).configureNativeSettings();
  assert.deepEqual(updates, [{ waitForIdleTimeout: 500, waitForSelectorTimeout: 0 }]);
  assert.deepEqual((platform as any).lastNativeSettings, { waitForIdleTimeout: 500, waitForSelectorTimeout: 0 });
});

test('Android launch failures are classified without hiding command diagnostics', async () => {
  assert.equal(androidLaunchFailureKind(new Error('exit status 1')), 'terminal');
  assert.equal(androidLaunchFailureKind(new Error('command timed out')), 'timeout');
});

test('Android final launch verifies readiness only after bootstrap teardown', async () => {
  const platform = new AndroidPlatform({
    origin: 'https://fixture.test',
    appiumUrl: 'http://fake.test',
    outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
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

test('Android installed attachment selects the owned standalone window instead of a browser window', async () => {
  const platform = new AndroidPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
    certificate: '', setupUrl: '', deviceId: 'emulator-5554',
    budget: new PhaseBudget('android-attachment-test', { timeoutMs: 10_000, recoveryLimit: 1 }),
  });
  (platform as any).installedTarget = {
    packageName: 'com.android.chrome', activity: 'org.chromium.chrome.browser.webapps.WebappActivity',
    shortcut: { id: 'id', shortLabel: 'Herdr Relay', name: 'Herdr Mobile Relay', url: 'https://fixture.test/', scope: 'https://fixture.test/', mac: 'mac' },
  };
  (platform as any).isInstalledTargetForeground = async () => true;
  const driver = platform.driver as any;
  let selectedWindow = '';
  driver.contexts = async () => ['NATIVE_APP', 'CHROMIUM'];
  driver.contextMetadataRaw = async () => [];
  driver.switchContext = async () => undefined;
  driver.windowHandles = async () => ['browser-window', 'installed-window'];
  driver.switchWindow = async (handle: string) => { selectedWindow = handle; };
  driver.currentUrl = async () => 'https://fixture.test/';
  driver.execute = async () => selectedWindow === 'installed-window'
    ? { origin: 'https://fixture.test', standalone: true, provider: 'android-standalone' }
    : { origin: 'https://fixture.test', standalone: false, provider: 'browser' };
  await platform.attachToInstalledView();
  assert.equal((platform as any).selectedInstalledWindow, 'installed-window');
});

test('Android web controls use supported locators and preserve ownership failures', async () => {
  type ControlMode = 'settings' | 'ordinary' | 'disabled' | 'hidden' | 'none';
  const makeHarness = async (initialMode: ControlMode, timeoutMs = 30_000) => {
    const budget = new PhaseBudget(`android-web-control-${initialMode}`, { timeoutMs, recoveryLimit: 1 });
    let mode = initialMode;
    let selected = '';
    let attachmentChecks = 0;
    let requests = 0;
    const locators: Array<{ using: string; value: string }> = [];
    const clicks: string[] = [];
    const element = (id: string) => ({ 'element-6066-11e4-a52e-4f735466cecf': id });
    const response = (value: unknown, status = 200) => new Response(JSON.stringify({ value, sessionId: 'session' }), { status });
    const absent = () => response({ error: 'no such element', message: 'no such element' }, 404);
    const client = new AppiumClient('http://fake.test', 30_000, async (input, init) => {
      requests += 1;
      const path = new URL(String(input)).pathname;
      const body = init?.body ? JSON.parse(String(init.body)) as Record<string, any> : {};
      if (path === '/session') return response({});
      if (path.endsWith('/contexts')) return response(['NATIVE_APP', 'CHROMIUM']);
      if (path.endsWith('/window/handles')) return response(['installed-window']);
      if (path.endsWith('/window') && init?.method === 'POST') {
        selected = String(body.handle || '');
        return response(null);
      }
      if (path.endsWith('/url')) return response('https://fixture.test/');
      if (path.endsWith('/execute/sync')) {
        if (body.script === 'mobile: getContexts') return response([]);
        return response({ origin: 'https://fixture.test', standalone: selected === 'installed-window', provider: selected === 'installed-window' ? 'android-standalone' : 'browser' });
      }
      if (path.endsWith('/element') && init?.method === 'POST') {
        const locator = { using: String(body.using || ''), value: String(body.value || '') };
        locators.push(locator);
        if (locator.using === 'accessibility id') return response({ error: 'invalid argument', message: 'invalid locator' }, 400);
        if (mode === 'settings' && locator.value.includes('starts-with(@aria-label')) return response(element('settings'));
        if (mode === 'ordinary' && locator.value.includes('Mixed')) return response(element('mixed'));
        if (mode === 'disabled' && locator.value.includes('Disabled')) return response(element('disabled'));
        if (mode === 'hidden' && locator.value.includes('Hidden')) return response(element('hidden'));
        return absent();
      }
      if (path.includes('/attribute/')) {
        const id = decodeURIComponent(path.split('/element/')[1]?.split('/')[0] || '');
        const name = decodeURIComponent(path.split('/attribute/')[1] || '');
        if (id === 'disabled' && name === 'disabled') return response('true');
        if (id === 'hidden' && name === 'hidden') return response('true');
        return response(null);
      }
      if (path.endsWith('/rect')) return response({ x: 0, y: 0, width: 100, height: 40 });
      if (path.endsWith('/click')) {
        clicks.push(decodeURIComponent(path.split('/element/')[1]?.split('/')[0] || ''));
        return response(null);
      }
      return response(null);
    });
    await client.create({ capabilities: {}, budget });
    const platform = new AndroidPlatform({
      origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
      certificate: '', setupUrl: '', deviceId: 'emulator-5554', budget,
    });
    (platform as any).driver = client;
    (platform as any).installedTarget = {
      packageName: 'com.android.chrome', activity: 'org.chromium.chrome.browser.webapps.WebappActivity',
      shortcut: { id: 'id', shortLabel: 'Herdr Relay', name: 'Herdr Mobile Relay', url: 'https://fixture.test/', scope: 'https://fixture.test/', mac: 'mac' },
    };
    (platform as any).installedPackage = 'com.android.chrome';
    (platform as any).foregroundEvidence = async () => {
      attachmentChecks += 1;
      return { packageName: 'com.android.chrome', activity: 'org.chromium.chrome.browser.webapps.WebappActivity', pid: '1', raw: '' };
    };
    return {
      platform,
      client,
      locators,
      clicks,
      get requests() { return requests; },
      get attachmentChecks() { return attachmentChecks; },
      setMode(value: ControlMode) { mode = value; },
    };
  };

  const harness = await makeHarness('settings');
  await harness.platform.clickWebText('Settings');
  harness.setMode('ordinary');
  await harness.platform.clickWebText('Mixed');
  assert.deepEqual(harness.clicks, ['settings', 'mixed']);
  assert.ok(harness.attachmentChecks >= 2);
  assert.equal(harness.locators.some((locator) => locator.using === 'accessibility id'), false);
  assert.ok(harness.locators.some((locator) => locator.value.includes('starts-with(@aria-label')));

  const beforeOwnershipFailure = harness.requests;
  (harness.platform as any).foregroundEvidence = async () => ({
    packageName: 'com.google.android.apps.nexuslauncher', activity: 'com.android.launcher3.Launcher', pid: '2', raw: '',
  });
  await assert.rejects(() => harness.platform.clickWebText('Mixed'), /ANDROID_CONTEXT_OWNERSHIP/);
  const afterOwnershipFailure = harness.requests;
  await assert.rejects(() => harness.platform.clickWebText('Mixed'), /ANDROID_CONTEXT_OWNERSHIP/);
  assert.equal(harness.requests, afterOwnershipFailure);
  assert.ok(afterOwnershipFailure >= beforeOwnershipFailure);

  for (const mode of ['disabled', 'hidden', 'none'] as const) {
    const blocked = await makeHarness(mode, 1_200);
    await assert.rejects(() => blocked.platform.clickWebText(mode === 'none' ? 'Missing' : mode[0].toUpperCase() + mode.slice(1)), /APPIUM_BUTTON|PHASE_BUDGET_EXHAUSTED|disabled|hidden|not found/iu);
    assert.equal(blocked.clicks.length, 0);
  }

  const invalid = await makeHarness('none');
  await assert.rejects(() => invalid.client.findAny([{ using: 'accessibility id', value: 'unsupported' }], 5_000), /APPIUM_COMMAND/);
  assert.equal(invalid.client.snapshot().lookups.length, 1);
  assert.equal(invalid.client.snapshot().unusable, false);
});

test('Android fixture verification keeps the HTTP status separate from the Appium response status', async () => {
  const platform = new AndroidPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
    certificate: '', setupUrl: '', deviceId: 'emulator-5554',
    budget: new PhaseBudget('android-fixture-test', { timeoutMs: 15_000, recoveryLimit: 1 }),
  });
  const transport = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const path = new URL(String(input)).pathname;
    if (path === '/session') return new Response(JSON.stringify({ value: {}, sessionId: 'session' }), { status: 200 });
    if (path.endsWith('/contexts')) return new Response(JSON.stringify({ value: ['NATIVE_APP', 'CHROMIUM'] }), { status: 200 });
    if (path.endsWith('/execute/sync')) {
      const script = JSON.parse(String(init?.body || '{}')).script as string;
      if (script.includes('status: response.status')) {
        return new Response(JSON.stringify({ value: { error: 'unknown error', message: 'Matched JSONWP error code 200 to UnknownError' } }), { status: 500 });
      }
      return new Response(JSON.stringify({ value: {
        httpStatus: 200,
        url: 'https://fixture.test/version.json',
        body: JSON.stringify({ version: '0.20.10', assets: 363 }),
      } }), { status: 200 });
    }
    return new Response(JSON.stringify({ value: null }), { status: 200 });
  };
  const driver = new AppiumClient('http://fake.test', 100, transport);
  await driver.create({ capabilities: {} });
  (platform as any).driver = driver;
  driver.setBudget((platform as any).budget);
  await (platform as any).verifyFixtureEndpoint();
});

test('Android fixture verification rejects unsafe HTTP response identities without losing the reason', async () => {
  const cases = [
    {
      observed: { httpStatus: 200, url: 'https://other.test/version.json', body: JSON.stringify({ version: '0.20.10', assets: 363 }) },
      error: /status=200 url=https:\/\/other\.test/,
      advanceMs: 351,
    },
    {
      observed: { httpStatus: 200, url: 'https://fixture.test/version.json', body: '{not-json' },
      error: /Unexpected|JSON|fixture HTTPS response identity was not trusted/,
      advanceMs: 351,
    },
    {
      observed: { httpStatus: 503, url: 'https://fixture.test/version.json', body: JSON.stringify({ version: '0.20.10', assets: 363 }) },
      error: /status=503/,
      advanceMs: 351,
    },
    {
      observed: { httpStatus: 200, url: 'https://fixture.test/version.json', body: 'ERR_CERT_AUTHORITY_INVALID' },
      error: /CERT|fixture HTTPS response identity was not trusted/,
      advanceMs: 351,
    },
  ];
  for (const [index, scenario] of cases.entries()) {
    const clock = { value: 0 };
    const platform = new AndroidPlatform({
      origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
      certificate: '', setupUrl: '', deviceId: 'emulator-5554',
      budget: new PhaseBudget(`android-fixture-rejection-${index}`, { timeoutMs: 350, recoveryLimit: 1, now: () => clock.value }),
    });
    const driver = platform.driver as any;
    driver.contexts = async () => ['NATIVE_APP', 'CHROMIUM'];
    driver.switchContext = async () => undefined;
    driver.navigate = async () => undefined;
    driver.execute = async () => {
      clock.value += scenario.advanceMs;
      return scenario.observed;
    };
    await assert.rejects(() => (platform as any).verifyFixtureEndpoint(), scenario.error);
  }
});

test('Android Chrome shortcut output preserves the signed launch fields', async () => {
  const shortcuts = parseAndroidChromeShortcuts(`ShortcutInfo {id=shortcut-id, flags=0x28a
  shortLabel=Herdr Relay, resId=0[null]
  intents=[Intent { act=com.google.android.apps.chrome.webapps.WebappManager.ACTION_START_WEBAPP pkg=com.android.chrome }/PersistableBundle[{org.chromium.chrome.browser.webapp_scope=https://localhost:38289/, org.chromium.chrome.browser.webapp_name=Herdr Mobile Relay, org.chromium.chrome.browser.webapp_mac=mac+/=, org.chromium.chrome.browser.webapp_id=shortcut-id, org.chromium.chrome.browser.webapp_source=7, org.chromium.chrome.browser.webapp_display_mode=3, org.chromium.content_public.common.orientation=0, org.chromium.chrome.browser.webapp_url=https://localhost:38289/}]]
}`);
  assert.equal(shortcuts.length, 1);
  assert.deepEqual(shortcuts[0], {
    id: 'shortcut-id', flags: '0x28a', shortLabel: 'Herdr Relay', name: 'Herdr Mobile Relay',
    url: 'https://localhost:38289/', scope: 'https://localhost:38289/', mac: 'mac+/=',
    source: '7', displayMode: '3', orientation: '0',
  });
  const args = androidChromeShortcutArgs('emulator-5554', shortcuts[0]);
  const platform = new AndroidPlatform({
    origin: 'https://localhost:38289', appiumUrl: 'http://fake.test', outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
    certificate: '', setupUrl: '', deviceId: 'emulator-5554', budget: new PhaseBudget('shortcut-evidence', { timeoutMs: 10_000, recoveryLimit: 1 }),
  });
  const evidence = (platform as any).shortcutEvidence(shortcuts[0]);
  assert.equal(evidence.url, '[REDACTED]');
  assert.equal(evidence.mac, '[REDACTED]');
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

test('runtime script observations satisfy baseline and candidate validation contracts', async () => {
  const definitions = JSON.parse(await readFile(join(repositoryRoot, 'tests/mobile/baselines.json'), 'utf8')).baselines as Array<Record<string, unknown>>;
  const origin = 'https://fixture.test';
  const candidate: BundleIdentity = {
    version: '0.21.0',
    assets: 999,
    build: 'c'.repeat(64),
    entry: `/builds/0.21.0-999-${'c'.repeat(16)}/index.html`,
    script: '/assets/app-candidate.js',
    style: '/assets/app-candidate.css',
    scriptSha256: '1'.repeat(64),
    styleSha256: '2'.repeat(64),
    webHash: 'd'.repeat(64),
    descriptor: true,
  };
  for (const definition of definitions) {
    const expected: BundleIdentity = {
      version: String(definition.version),
      assets: Number(definition.assets),
      build: String(definition.build || ''),
      entry: String(definition.entry),
      script: String(definition.script),
      style: String(definition.style),
      scriptSha256: '1'.repeat(64),
      styleSha256: '2'.repeat(64),
      webHash: String(definition.webHash),
      descriptor: definition.name === '0.20.10',
    };
    const metadata = definition.name === '0.20.10'
      ? { version: expected.version, assets: expected.assets, build: expected.build, entry: expected.entry }
      : { version: expected.version, assets: expected.assets };
    const location = new URL(expected.entry, origin);
    const hasDescriptor = expected.descriptor;
    const document = {
      querySelector: (selector: string) => selector === '[data-app-assets]'
        ? hasDescriptor ? { getAttribute: (name: string) => name === 'data-app-assets' ? String(expected.assets) : '' } : null
        : selector.startsWith('script')
          ? { getAttribute: () => expected.script }
          : selector.startsWith('link')
            ? { getAttribute: () => expected.style, sheet: {} }
            : null,
      querySelectorAll: () => [],
      documentElement: { dataset: { herdrCssReady: '1' } },
      getElementById: () => ({ childNodes: [{}] }),
      body: { innerText: '' },
    };
    const observed = new Function('document', 'window', 'navigator', 'performance', 'location', 'XMLHttpRequest', runtimeScript())(
      document,
      { matchMedia: () => ({ matches: true }) },
      { userAgent: 'Android', standalone: false },
      { timeOrigin: 123 },
      location,
      class { status = 200; responseText = JSON.stringify(metadata); open() {} send() {} },
    ) as RuntimeIdentity;
    const initial: RuntimeIdentity = {
      ...observed,
      nativeProvider: 'android:com.android.chrome',
      nativeActivity: 'org.chromium.chrome.browser.webapps.WebappActivity',
      nativePid: '42',
    };
    assert.doesNotThrow(() => assertRunningIdentity(initial, expected, false));
    const directory = await mkdtemp(join(tmpdir(), 'herdr-mobile-runtime-contract-'));
    const sourceCommit = 'a'.repeat(40);
    const result = {
      schema: 1, result: 'passed', suite: 'smoke', platform: 'android', baseline: expected.version,
      candidate: 'candidate-proof', origin, source_commit: sourceCommit, source_run_head_sha: sourceCommit,
      candidate_web_hash: candidate.webHash,
      initial_identity: initial,
      final_identity: {
        ...initial,
        ...candidate,
        url: new URL(candidate.entry, origin).href,
        nativeProvider: 'android:com.android.chrome',
        nativeActivity: 'org.chromium.chrome.browser.webapps.WebappActivity',
        nativePid: '42',
        buildFromApplication: true,
      },
      credential_preserved: true,
      credential_evidence: { relays: {
        alpha: { invitationAuthCount: 1, credentialAuthCount: 1, credentialPseudonyms: ['alpha'], connections: 1 },
        beta: { invitationAuthCount: 1, credentialAuthCount: 1, credentialPseudonyms: ['beta'], connections: 1 },
      } },
      preference_preserved: true,
      oracle_controls: expected.version === '0.20.10' ? [] : [`HISTORICAL_PHONE_ACCOUNTING_UNAVAILABLE:${expected.version}`],
      phone_completion: expected.version === '0.20.10'
        ? { rawPlanPresent: true, phoneRequired: true, phoneAcknowledged: true, phoneState: 'loaded', visibleCompletion: true }
        : { rawPlanPresent: true, phoneRequired: false, phoneAcknowledged: false, phoneState: 'failed', visibleCompletion: false },
      faults_exercised: [`corrupt:${candidate.script}`],
      fault_identity: { id: 'contract', generation: '1', kind: 'corrupt', path: candidate.script },
      fixture_requests: [{ release: 'candidate', path: candidate.script, fault: 'corrupt', fault_id: 'contract', fault_generation: '1' }],
    };
    await writeFile(join(directory, 'mobile-result.json'), JSON.stringify(result));
    await validateMobileEvidence({
      directory,
      matrix: [{ platform: 'android', baseline: expected.version, scenario: 'historical' }],
      suite: 'smoke', candidateCommit: sourceCommit, sourceRunHeadSha: sourceCommit,
      candidateWebHash: candidate.webHash, candidateIdentity: candidate,
      baselineIdentities: [{ name: expected.version, identity: expected }],
    });
    if (expected.descriptor) {
      assert.throws(() => assertRunningIdentity({ ...result.final_identity, buildFromApplication: false }, candidate), /RUNTIME_BUILD_SOURCE/);
      await writeFile(join(directory, 'mobile-result.json'), JSON.stringify({
        ...result,
        final_identity: { ...result.final_identity, buildFromApplication: false },
      }));
      await assert.rejects(validateMobileEvidence({
        directory,
        matrix: [{ platform: 'android', baseline: expected.version, scenario: 'historical' }],
        suite: 'smoke', candidateCommit: sourceCommit, sourceRunHeadSha: sourceCommit,
        candidateWebHash: candidate.webHash, candidateIdentity: candidate,
        baselineIdentities: [{ name: expected.version, identity: expected }],
      }), /RUNTIME_BUILD_SOURCE/);
      await writeFile(join(directory, 'mobile-result.json'), JSON.stringify(result));
    }
    if (expected.build) {
      assert.throws(() => assertRunningIdentity({ ...initial, build: 'deadbeefdeadbeef' }, expected, false), /RUNTIME_BUILD_MISMATCH/);
    }
  }
});

test('runtime readiness only permits an owned same-origin loading document', async () => {
  const identity: RuntimeIdentity = {
    url: 'https://localhost/', origin: 'https://localhost', standalone: true, provider: 'android-standalone',
    nativeProvider: 'android:com.android.chrome', nativeActivity: 'WebappActivity', nativePid: '1',
    version: '0.20.10', assets: 363, build: '', entry: '/index.html', script: '/assets/app.js', style: '/assets/app.css',
    requiredAssetsReady: false, applicationInitialized: false,
  };
  assert.equal(isRuntimeIdentityNotReady(identity, 'https://localhost'), true);
  assert.equal(isRuntimeIdentityNotReady({ ...identity, provider: 'browser', nativeProvider: undefined }, 'https://localhost'), false);
  assert.equal(isRuntimeIdentityNotReady({ ...identity, origin: 'https://other.test' }, 'https://localhost'), false);
  assert.equal(isRuntimeIdentityNotReady({ ...identity, applicationInitialized: true }, 'https://localhost'), false);
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
  assert.doesNotThrow(() => assertStandalone({
    ...identity, nativeProvider: 'android:com.android.chrome', nativeActivity: 'org.chromium.chrome.browser.webapps.WebappActivity',
  }, 'https://localhost'));
  assert.throws(() => assertStandalone({
    ...identity, nativeProvider: 'android:com.android.chrome', nativeActivity: 'org.chromium.chrome.browser.webapps.WebappLauncherActivity',
  }, 'https://localhost'), /STANDALONE_PROVIDER_REQUIRED/);
});

test('runtime oracle requires loaded target assets and identity', async () => {
  const identity: RuntimeIdentity = {
    url: 'https://localhost/', origin: 'https://localhost', standalone: true, provider: 'test',
    version: '0.20.10', assets: 363, build: 'cf1b92fa5edff10ab372fcb8479ad789a5443a6a8260a81dc61f30c8198045ab',
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
  assert.deepEqual(sanitizeValue({ invitationAuthCount: 2, credentialAuthCount: 'credential-secret', connections: 1 }), {
    invitationAuthCount: 2, credentialAuthCount: '[REDACTED]', connections: 1,
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

test('Appium native commands are not admitted near the scenario deadline', async () => {
  let now = 0;
  let requests = 0;
  const budget = new PhaseBudget('admission-test', { timeoutMs: 100, now: () => now, recoveryLimit: 0 });
  const client = new AppiumClient('http://fake.test', 100, async () => {
    requests += 1;
    return new Response(JSON.stringify({ value: { sessionId: 'session' }, sessionId: 'session' }), { status: 200 });
  });
  await client.create({ capabilities: {}, budget });
  now = 50;
  await assert.rejects(() => client.mobile('scrollGesture', {}, 100), /APPIUM_COMMAND_NOT_ADMITTED/);
  assert.equal(requests, 1);
  assert.equal(client.snapshot().unusable, false);
  assert.ok((client.snapshot().lastCommand?.durationMs || 0) < 100);
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
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
    certificate: '', setupUrl: '', deviceId: 'emulator-1', budget: new PhaseBudget('android-native-test', { timeoutMs: 10_000, recoveryLimit: 1 }),
  });
  (android as any).driver = {
    windowSize: async () => ({ width: 1_080, height: 2_400 }),
    findAnyOnce: async () => { throw fatal; },
    mobile: async () => { androidScrolls += 1; },
  };
  await assert.rejects(() => (android as any).findNative([{ using: 'accessibility id', value: 'Missing' }], 10_000), (error: unknown) => error === fatal);
  assert.equal(androidScrolls, 0);

  let iosScrolls = 0;
  const ios = new IOSPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
    certificate: '', setupUrl: '', budget: new PhaseBudget('ios-native-test', { timeoutMs: 30_000, recoveryLimit: 1 }),
  });
  (ios as any).driver = {
    pageSource: async () => iosShareHierarchy(0),
    findAll: async () => { throw fatal; },
    mobile: async () => { iosScrolls += 1; },
  };
  await assert.rejects(() => (ios as any).findNativeScrollable([{ using: 'accessibility id', value: 'Missing' }], 'Missing', 20_000), (error: unknown) => error === fatal);
  assert.equal(iosScrolls, 0);
});

test('native lookup scrolls between single-pass locator rounds', async () => {
  const android = new AndroidPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
    certificate: '', setupUrl: '', deviceId: 'emulator-1', budget: new PhaseBudget('android-scroll-test', { timeoutMs: 30_000, recoveryLimit: 1 }),
  });
  let androidLookups = 0;
  let androidScrolls = 0;
  (android as any).driver = {
    windowSize: async () => ({ width: 1_080, height: 2_400 }),
    findAnyOnce: async () => {
      androidLookups += 1;
      if (androidLookups > 1) return 'android-target';
      throw new ElementLookupError('element not found');
    },
    mobile: async () => { androidScrolls += 1; },
  };
  assert.equal(await (android as any).findNative([{ using: 'accessibility id', value: 'Target' }], 20_000), 'android-target');
  assert.equal(androidScrolls, 1);

  const ios = new IOSPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
    certificate: '', setupUrl: '', deviceId: 'simulator-1', budget: new PhaseBudget('ios-scroll-test', { timeoutMs: 30_000, recoveryLimit: 1 }),
  });
  let iosLookups = 0;
  let iosScrolls = 0;
  (ios as any).driver = {
    pageSource: async () => `<AppiumAUT><XCUIElementTypeApplication name="Safari"><XCUIElementTypeOther name="ActivityListView" visible="true"><XCUIElementTypeOther name="ShareSheet.RemoteContainerView" visible="true"><XCUIElementTypeCollectionView name="activityCollectionView" x="0" y="100" width="393" height="600" visible="true"><XCUIElementTypeCell name="actionGroupCell" label="Target" enabled="true" visible="${iosScrolls > 0 ? 'true' : 'false'}" x="16" y="${700 - iosScrolls * 50}" width="361" height="40"/></XCUIElementTypeCollectionView></XCUIElementTypeOther></XCUIElementTypeOther></XCUIElementTypeApplication></AppiumAUT>`,
    findAll: async (locator: { value: string }) => {
      if (locator.value === 'Target') {
        iosLookups += 1;
        return iosLookups > 1 ? ['ios-target'] : [];
      }
      return ['ios-container'];
    },
    elementRect: async (element: string) => element === 'ios-container'
      ? { x: 0, y: 100, width: 393, height: 600 }
      : { x: 16, y: 700 - iosScrolls * 50, width: 361, height: 40 },
    attribute: async () => 'true',
    mobile: async () => { iosScrolls += 1; },
  };
  assert.equal(await (ios as any).findNativeScrollable([{ using: 'accessibility id', value: 'Target' }], 'Target', 20_000), 'ios-target');
  assert.equal(iosScrolls, 1);
  assert.equal(iosNativeScrollDirection({ x: 0, y: 700, width: 10, height: 40 }, { x: 0, y: 100, width: 393, height: 600 }), 'down');
  assert.equal(iosNativeScrollDirection({ x: 0, y: 50, width: 10, height: 40 }, { x: 0, y: 100, width: 393, height: 600 }), 'up');
  assert.equal(iosNativeSwipeDirection('down'), 'up');
  assert.equal(iosNativeSwipeDirection('up'), 'down');
});

test('iOS native scrolling rejects a hidden match when the hierarchy shows no progress', async () => {
  const outputDir = await mkdtemp(join(tmpdir(), 'herdr-mobile-ios-scroll-'));
  const platform = new IOSPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir,
    certificate: '', setupUrl: '', deviceId: 'simulator-1',
    budget: new PhaseBudget('ios-no-progress-test', { timeoutMs: 20_000, recoveryLimit: 1 }),
  });
  const driver = platform.driver as any;
  const source = '<AppiumAUT><XCUIElementTypeApplication name="Safari"><XCUIElementTypeOther name="ActivityListView" visible="true"><XCUIElementTypeOther name="ShareSheet.RemoteContainerView" visible="true"><XCUIElementTypeCollectionView name="activityCollectionView" x="0" y="100" width="393" height="600" visible="true"><XCUIElementTypeCell name="actionGroupCell" label="Add to Home Screen" visible="false" enabled="true" x="16" y="700" width="361" height="40"/></XCUIElementTypeCollectionView></XCUIElementTypeOther></XCUIElementTypeOther></XCUIElementTypeApplication></AppiumAUT>';
  let gestures = 0;
  driver.findAnyOnce = async () => 'target';
  driver.pageSource = async () => source;
  driver.findAll = async (locator: { value: string }) => locator.value.includes('Add to Home Screen') ? ['target'] : ['container'];
  driver.elementRect = async (element: string) => element === 'container'
    ? { x: 0, y: 100, width: 393, height: 600 }
    : { x: 16, y: 700, width: 361, height: 40 };
  driver.attribute = async (element: string, name: string) => element === 'target' && name === 'visible' ? 'false' : 'true';
  driver.mobile = async () => { gestures += 1; };
  await assert.rejects(
    () => (platform as any).findNativeScrollable([{ using: 'accessibility id', value: 'Add to Home Screen' }], 'Add to Home Screen', 20_000),
    /made no verified progress/,
  );
  assert.equal(gestures, 1);
});

test('iOS native action controls stop when readiness is indeterminate', async () => {
  const outputDir = await mkdtemp(join(tmpdir(), 'herdr-mobile-ios-indeterminate-'));
  const platform = new IOSPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir,
    certificate: '', setupUrl: '', deviceId: 'simulator-1',
    budget: new PhaseBudget('ios-indeterminate', { timeoutMs: 20_000, recoveryLimit: 1 }),
  });
  const driver = platform.driver as any;
  const source = '<AppiumAUT><XCUIElementTypeApplication name="Safari"><XCUIElementTypeOther name="ActivityListView" visible="true"><XCUIElementTypeOther name="ShareSheet.RemoteContainerView" visible="true"><XCUIElementTypeCollectionView name="activityCollectionView" x="0" y="100" width="393" height="600" visible="true"><XCUIElementTypeCell name="actionGroupCell" label="Add to Home Screen" visible="true" enabled="true" x="16" y="200" width="361" height="40"/></XCUIElementTypeCollectionView></XCUIElementTypeOther></XCUIElementTypeOther></XCUIElementTypeApplication></AppiumAUT>';
  let gestures = 0;
  driver.pageSource = async () => source;
  driver.findAll = async (locator: { value: string }) => locator.value.includes('Add to Home Screen') ? ['target'] : ['container'];
  driver.elementRect = async (element: string) => element === 'container'
    ? { x: 0, y: 100, width: 393, height: 600 }
    : { x: 16, y: 200, width: 361, height: 40 };
  driver.attribute = async (_element: string, name: string) => name === 'hittable' ? null : 'true';
  driver.mobile = async () => { gestures += 1; };
  await assert.rejects(
    () => (platform as any).findNativeScrollable([{ using: 'xpath', value: 'Add to Home Screen' }], 'Add to Home Screen', 20_000),
    /control readiness is indeterminate/,
  );
  assert.equal(gestures, 0);
});

test('iOS progress ignores browser bars and rejects a dismissed action list', async () => {
  const run = async (dismissAfterGesture: boolean): Promise<{ gestures: number; failure: string }> => {
    const outputDir = await mkdtemp(join(tmpdir(), 'herdr-mobile-ios-progress-'));
    const platform = new IOSPlatform({
      origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir,
      certificate: '', setupUrl: '', deviceId: 'simulator-1',
      budget: new PhaseBudget(`ios-progress-${dismissAfterGesture}`, { timeoutMs: 20_000, recoveryLimit: 1 }),
    });
    const driver = platform.driver as any;
    let bars = '0%';
    let dismissed = false;
    let gestures = 0;
    const modal = () => dismissed
      ? iosSafariHierarchy()
      : `<AppiumAUT><XCUIElementTypeApplication name="Safari"><XCUIElementTypeOther name="ActivityListView" visible="true"><XCUIElementTypeOther name="ShareSheet.RemoteContainerView" visible="true"><XCUIElementTypeCollectionView name="activityCollectionView" x="0" y="100" width="393" height="600" visible="true"><XCUIElementTypeScrollView name="share-apps-strip" visible="true" x="8" y="110" width="377" height="100"><XCUIElementTypeCell name="shareCell" label="Add to Home Screen" visible="true" x="8" y="110" width="78" height="100"/></XCUIElementTypeScrollView><XCUIElementTypeCell name="actionGroupCell" label="Add to Home Screen" enabled="true" visible="false" x="16" y="700" width="361" height="40"/></XCUIElementTypeCollectionView></XCUIElementTypeOther></XCUIElementTypeOther></XCUIElementTypeApplication><XCUIElementTypeOther name="Vertical scroll bar, 2 pages" value="${bars}" visible="true"/></AppiumAUT>`;
    driver.pageSource = async () => modal();
    driver.findAll = async (locator: { value: string }) => locator.value.includes('Add to Home Screen') ? ['target'] : ['container'];
    driver.elementRect = async (element: string) => element === 'container'
      ? { x: 0, y: 100, width: 393, height: 600 }
      : { x: 16, y: 700, width: 361, height: 40 };
    driver.attribute = async (element: string, name: string) => element === 'target' && name === 'visible' ? 'false' : 'true';
    driver.mobile = async (command: string, args: Record<string, unknown>) => {
      assert.equal(command, 'scroll');
      assert.equal(args.direction, 'down');
      gestures += 1;
      if (dismissAfterGesture) dismissed = true;
      else bars = '50%';
    };
    let failure = '';
    try {
      await (platform as any).findNativeScrollable([{ using: 'xpath', value: 'Add to Home Screen' }], 'Add to Home Screen', 20_000);
    } catch (error) {
      failure = error instanceof Error ? error.message : String(error);
    }
    return { gestures, failure };
  };

  const barChange = await run(false);
  assert.equal(barChange.gestures, 1);
  assert.match(barChange.failure, /made no verified progress/);
  const dismissal = await run(true);
  assert.equal(dismissal.gestures, 1);
  assert.match(dismissal.failure, /dismissed or replaced/);
});

test('iOS installation scrolls the evidenced action list before clicking ready controls', async () => {
  const outputDir = await mkdtemp(join(tmpdir(), 'herdr-mobile-ios-install-'));
  const platform = new IOSPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir,
    certificate: '', setupUrl: '', deviceId: 'simulator-1',
    budget: new PhaseBudget('ios-install-scroll-test', { timeoutMs: 120_000, recoveryLimit: 1 }),
  });
  let sheetOpen = false;
  let scrolls = 0;
  let shareSourceReads = 0;
  let settings: Record<string, unknown> = {};
  const scrollArguments: Array<Record<string, unknown>> = [];
  const clicks: string[] = [];
  const source = () => {
    if (!sheetOpen) return iosSafariHierarchy();
    if (shareSourceReads++ === 0) return iosShareHierarchy(0, false);
    return iosShareHierarchy(scrolls);
  };
  const element = (id: string) => ({ 'element-6066-11e4-a52e-4f735466cecf': id });
  const absent = () => new Response(JSON.stringify({ value: { error: 'no such element', message: 'No such element' } }), { status: 404 });
  const driver = new AppiumClient('http://fake.test', 100, async (input, init) => {
    const path = new URL(String(input)).pathname;
    const body = init?.body ? JSON.parse(String(init.body)) as Record<string, any> : {};
    if (path === '/session') return new Response(JSON.stringify({ value: {}, sessionId: 'session' }), { status: 200 });
    if (path.endsWith('/context')) return new Response(JSON.stringify({ value: null }), { status: 200 });
    if (path.endsWith('/appium/settings')) {
      if (init?.method === 'GET') return Response.json({ value: settings });
      settings = { ...settings, ...body.settings };
      return Response.json({ value: null });
    }
    if (path.endsWith('/alert/text')) return Response.json({ value: { error: 'no such alert' } }, { status: 404 });
    if (body.script === 'mobile: queryAppState') return Response.json({ value: 4 });
    if (path.endsWith('/source')) return new Response(JSON.stringify({ value: source() }), { status: 200 });
    if (path.endsWith('/screenshot')) return new Response(JSON.stringify({ value: '' }), { status: 200 });
    if (path.endsWith('/execute/sync')) {
      if (body.script === 'mobile: activeAppInfo') return new Response(JSON.stringify({ value: { bundleId: 'com.apple.mobilesafari', pid: '42' } }), { status: 200 });
      if (body.script === 'mobile: tap') {
        sheetOpen = true;
        return new Response(JSON.stringify({ value: null }), { status: 200 });
      }
      if (body.script === 'mobile: scroll') {
        scrolls += 1;
        scrollArguments.push(body.args);
        return new Response(JSON.stringify({ value: null }), { status: 200 });
      }
      return new Response(JSON.stringify({ value: null }), { status: 200 });
    }
    if (path.endsWith('/elements')) {
      const value = String(body.value || '');
      const matches = value.includes('XCUIElementTypeNavigationBar') ? [element('add-button')]
        : value.includes('Add to Home Screen') && scrolls >= 1 ? [element('hidden-add'), element('add-home')]
        : value.includes('activityCollectionView') ? [element('container')] : [];
      return new Response(JSON.stringify({ value: matches }), { status: 200 });
    }
    if (path.endsWith('/element') && init?.method === 'POST') {
      const value = String(body.value || '');
      if (value.includes('ShareButton')) return new Response(JSON.stringify({ value: element('share') }), { status: 200 });
      if (value.includes('Add to Home Screen') && sheetOpen && scrolls >= 1) return new Response(JSON.stringify({ value: element('add-home') }), { status: 200 });
      if (value.includes('Add')) return new Response(JSON.stringify({ value: element('add-button') }), { status: 200 });
      return absent();
    }
    if (path.includes('/attribute/')) {
      const id = decodeURIComponent(path.split('/element/')[1]?.split('/')[0] || '');
      const attribute = path.split('/attribute/')[1];
      if (attribute === 'visible' && (id === 'hidden-add' || id === 'add-home')) {
        return new Response(JSON.stringify({ value: id === 'add-home' && scrolls >= 2 ? 'true' : 'false' }), { status: 200 });
      }
      return new Response(JSON.stringify({ value: 'true' }), { status: 200 });
    }
    if (path.endsWith('/rect')) {
      const id = decodeURIComponent(path.split('/element/')[1]?.split('/')[0] || '');
      const targetY = 907 - Math.min(scrolls, 2) * 60;
      return new Response(JSON.stringify({ value: id === 'container' ? { x: 0, y: 398, width: 393, height: 454 } : { x: 16, y: targetY, width: 361, height: 51 } }), { status: 200 });
    }
    if (path.endsWith('/click')) {
      const id = decodeURIComponent(path.split('/element/')[1]?.split('/')[0] || '');
      clicks.push(id);
      if (id === 'share') sheetOpen = true;
      return new Response(JSON.stringify({ value: null }), { status: 200 });
    }
    return new Response(JSON.stringify({ value: null }), { status: 200 });
  });
  await driver.create({ capabilities: {} });
  const actionList = nativeActionListEvidence(iosShareHierarchy(0), 'Add to Home Screen');
  assert.equal(actionList?.collection.bounds.y, 398);
  assert.deepEqual(actionList?.targetRows.map((row) => row.label), ['Add to Home Screen']);
  (platform as any).driver = driver;
  driver.setBudget((platform as any).budget);
  await platform.installFromBrowser();
  assert.equal(scrolls, 2);
  assert.deepEqual(scrollArguments, [
    { element: 'container', direction: 'down', distance: 0.75 },
    { element: 'container', direction: 'down', distance: 0.75 },
  ]);
  assert.deepEqual(clicks, ['share', 'add-home', 'add-button']);
  assert.equal(driver.snapshot().unusable, false);
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

test('iOS native installation rejects a disabled Share control', async () => {
  const outputDir = await mkdtemp(join(tmpdir(), 'herdr-mobile-ios-install-'));
  const platform = new IOSPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir,
    certificate: '', setupUrl: '', deviceId: 'simulator-1',
    budget: new PhaseBudget('ios-install-test', { timeoutMs: 120_000, recoveryLimit: 1 }),
  });
  const driver = platform.driver as any;
  const gestures: string[] = [];
  mockIOSNativeObservation(platform);
  driver.switchContext = async () => undefined;
  driver.activeAppInfo = async () => ({ bundleId: 'com.apple.mobilesafari', pid: 42 });
  driver.pageSource = async () => iosSafariHierarchy().replace('enabled="true"', 'enabled="false"');
  driver.screenshot = async () => '';
  driver.findAnyOnce = async () => 'share';
  driver.attribute = async (_element: string, name: string) => name === 'enabled' ? 'false' : 'true';
  driver.mobile = async (command: string) => {
    if (command === 'queryAppState') return 4;
    gestures.push(command);
  };
  await assert.rejects(() => platform.installFromBrowser(), /Share: control is disabled/);
  assert.deepEqual(gestures, []);
});

test('iOS openurl failures distinguish transient, terminal, and timeout outcomes', async () => {
  assert.equal(iosOpenURLFailureKind(new Error('LaunchServices temporarily unavailable')), 'transient');
  assert.equal(iosOpenURLFailureKind(new Error('invalid simulator device')), 'terminal');
  assert.equal(iosOpenURLFailureKind(new Error('ETIMEDOUT')), 'timeout');
});

test('iOS WebKit discovery retries until Safari publishes a delayed page', async () => {
  const platform = new IOSPlatform({
    origin: 'https://fixture.test', appiumUrl: 'http://fake.test', outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
    certificate: '', setupUrl: '', deviceId: 'simulator-1',
    budget: new PhaseBudget('ios-discovery-test', { timeoutMs: 30_000, recoveryLimit: 1 }),
  });
  let discoveries = 0;
  const contexts: string[] = [];
  const driver = new AppiumClient('http://fake.test', 100, async (input, init) => {
    const path = new URL(String(input)).pathname;
    const body = String(init?.body || '');
    if (path === '/session') return new Response(JSON.stringify({ value: {}, sessionId: 'session' }), { status: 200 });
    if (path.endsWith('/execute/sync')) {
      discoveries += 1;
      const value = discoveries < 9 ? [] : [{ id: 'WEBVIEW_1', bundleId: 'com.apple.mobilesafari', url: 'https://fixture.test/', raw: {} }];
      return new Response(JSON.stringify({ value }), { status: 200 });
    }
    if (path.endsWith('/context')) {
      contexts.push(JSON.parse(body).name);
      return new Response(JSON.stringify({ value: null }), { status: 200 });
    }
    if (path.endsWith('/url') && init?.method === 'GET') {
      return new Response(JSON.stringify({ value: 'https://fixture.test/' }), { status: 200 });
    }
    return new Response(JSON.stringify({ value: null }), { status: 200 });
  });
  await driver.create({ capabilities: {} });
  (platform as any).driver = driver;
  driver.setBudget((platform as any).budget);
  await (platform as any).waitForSafariFixturePage('https://fixture.test/setup', 46_000, (platform as any).budget);
  assert.ok(discoveries >= 9);
  assert.deepEqual(contexts, ['WEBVIEW_1', 'NATIVE_APP']);
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
    outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
    certificate: '',
    setupUrl: '',
    budget: new PhaseBudget('ios-test', { timeoutMs: 30_000, recoveryLimit: 1 }),
  });
  const driver = platform.driver as any;
  (platform as any).installedBundleId = 'com.apple.webapp';
  const calls: string[] = [];
  mockIOSNativeObservation(platform);
  driver.contextMetadata = async () => [{
    id: 'WEBVIEW_1', bundleId: 'com.apple.SafariViewService', url: 'https://fixture.test/', raw: {},
  }];
  driver.switchContext = async (name: string) => { calls.push(`context:${name}`); driver.selectedContext = name; };
  driver.currentUrl = async () => 'https://fixture.test/';
  driver.activeAppInfo = async () => ({ bundleId: 'com.apple.webapp', pid: '19193' });
  driver.execute = async () => ({ origin: 'https://fixture.test', standalone: true });
  driver.windowHandles = async () => { calls.push('windows'); return ['unexpected']; };
  await platform.attachToInstalledView();
  assert.equal(calls.includes('windows'), false);
});

test('iOS attachment rejects incorrect foreground, origin, and standalone state', async () => {
  const cases = [
    { foreground: 'com.apple.mobilesafari', url: 'https://fixture.test/', standalone: true, error: /native provider/ },
    { foreground: 'com.apple.webapp', url: 'https://other.test/', standalone: true, error: /document origin/ },
    { foreground: 'com.apple.webapp', url: 'https://fixture.test/', standalone: false, error: /not standalone/ },
  ];
  for (const scenario of cases) {
    const platform = new IOSPlatform({
      origin: 'https://fixture.test',
      appiumUrl: 'http://fake.test',
      outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
      certificate: '',
      setupUrl: '',
      budget: new PhaseBudget('ios-negative-test', { timeoutMs: 30_000, recoveryLimit: 1 }),
    });
    const driver = platform.driver as any;
    (platform as any).installedBundleId = 'com.apple.webapp';
    mockIOSNativeObservation(platform);
    driver.contextMetadata = async () => [{
      id: 'WEBVIEW_1', bundleId: 'com.apple.SafariViewService', url: 'https://fixture.test/', raw: {},
    }];
    driver.switchContext = async (name: string) => { driver.selectedContext = name; };
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
    outputDir: join(tmpdir(), 'herdr-mobile-ci-unit'),
    certificate: '',
    setupUrl: '',
    budget: new PhaseBudget('ios-stale-test', { timeoutMs: 30_000, recoveryLimit: 1 }),
  });
  const driver = platform.driver as any;
  (platform as any).installedBundleId = 'com.apple.webapp';
  (platform as any).selectedInstalledContext = 'WEBVIEW_OLD';
  mockIOSNativeObservation(platform);
  const contexts: string[] = [];
  driver.switchContext = async (name: string) => {
    contexts.push(name);
    if (name === 'WEBVIEW_OLD') throw new Error('no such context');
    driver.selectedContext = name;
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

for (const [name, body] of androidTransitionTests) test(name, body);
for (const [name, body] of scenarioRunnerTests) test(name, body);
for (const [name, body] of webdriverInterruptionTests) test(name, body);
test('iOS recorded publication, installation and navigation protocol regressions', runIOSRegressions);

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
