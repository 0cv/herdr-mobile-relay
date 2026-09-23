import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { EventEmitter } from 'node:events';
import { execFileSync, spawnSync, type ChildProcess } from 'node:child_process';
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { mkdtemp, readFile, writeFile, rm, truncate, symlink } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, posix } from 'node:path';
import { pathToFileURL } from 'node:url';
import { assessAndroidEnvironment, environmentLeaves, readEnvironmentInputs, type AndroidEnvironmentSnapshot, type AndroidPackageIdentity, type AndroidPlannedTermination, type EnvironmentInputBytes } from '../android-environment';
import { AndroidEnvironmentMeasurement, type AndroidMeasurementIO } from '../android-measurement';
import { AndroidProductRecorder, androidProductImpact, publicMobileSummary, validateProductObservations, type ProductObservations } from '../support/android-product';
import { FailureLedger, qualified, requiredExecutionPassed, type MeasurementIdentity, type MobileResultContract } from '../support/mobile-result';
import { validateAndroidProducer, validateMobileEvidence, type EvidenceValidationOptions } from '../support/evidence';
import { collectEnvironmentReport, serializeEnvironmentReport } from '../validate-environment';
import { redactText, writeSanitizedJson } from '../support/diagnostics';
import { repositoryPath } from '../support/paths';

const hash = (value: string | Buffer) => createHash('sha256').update(value).digest('hex');
export const sm59HistoricalCase = { run: '35556703407', childPid: '5820', product: 'INDETERMINATE', environment: 'FAIL', relation: 'UNPROVEN' } as const;
export function measurementIdentity(overrides: Partial<MeasurementIdentity> = {}): MeasurementIdentity {
  return { runId: '35556703407', attempt: '2', suite: 'smoke', platform: 'android', baseline: '0.20.10', scenario: 'historical',
    sourceCommit: 'a'.repeat(40), sourceRunHeadSha: 'b'.repeat(40), candidateWebHash: 'c'.repeat(64), candidateBuild: 'd'.repeat(64), measurementId: 'android-test', ...overrides };
}
const dependencyFields = ['dynamic libraries', 'static library', 'SDK library', 'usesLibraries', 'usesStaticLibraries', 'usesSdkLibraries', 'usesOptionalLibraries', 'usesNativeLibraries', 'usesOptionalNativeLibraries', 'usesLibraryFiles', 'enabledComponents', 'disabledComponents', 'flags', 'splits', 'privateFlags', 'pkgFlags', 'privatePkgFlags', 'updateOwnerPackageName', 'apkSigningVersion'];
export function rehashPackage(value: AndroidPackageIdentity): void {
  value.dependencyConfigSha256 = hash(JSON.stringify(value.dependencyConfig));
  const fields = Object.fromEntries(Object.entries(value).filter(([name]) => name !== 'dumpSha256' && name !== 'identitySha256'));
  value.identitySha256 = hash(JSON.stringify(fields));
}
function packageFixture(packageName: string): AndroidPackageIdentity {
  const value: AndroidPackageIdentity = { packageName, packageRecordName: packageName, staticLibraryName: '', staticLibraryVersion: '', versionName: '131.0.6778.200', versionCode: '1',
    installerPackageName: 'null', initiatingPackageName: 'null', originatingPackageName: 'null', packageSource: '0', firstInstallTime: '2026-09-10 08:00:00', lastUpdateTime: '2026-09-10 08:00:00',
    enabled: '0', installed: true, hidden: false, suspended: false, codePath: '/system/app/browser', apkPaths: ['package:/system/app/browser/base.apk'],
    dependencyConfig: Object.fromEntries(dependencyFields.map(field => [field, field === 'flags' ? ['SYSTEM'] : field === 'splits' ? ['base'] : []])), dependencyConfigSha256: '', dumpSha256: hash('public synthetic package'), identitySha256: '' };
  rehashPackage(value);
  return value;
}
export const marker = (second: number, text: string, id = 'android-test') => `178910040${second}.000 2000 2000 I HerdrMeasure: ${id} ${text}\n`;
export function environmentFixture(id = 'android-test'): EnvironmentInputBytes {
  const before: AndroidEnvironmentSnapshot = { schema: 1, capturedAt: '2026-09-10T08:00:00.000Z', serial: 'emulator-5554', avdName: 'herdr-mobile-ci-test',
    policy: { systemImage: 'system-images;android-35;google_apis;x86_64', systemImagePolicy: 'owned-google-apis-emulator', vendingPolicy: 'absent-or-disabled-user-0', browserPackage: 'com.android.chrome', browserVersion: '131.0.6778.200', trichromeLibraryPackage: 'com.google.android.trichromelibrary', trichromeLibraryVersion: '131.0.6778.200' },
    emulatorVersion: '36', adbVersion: '35', system: Object.fromEntries(['ro.build.fingerprint', 'ro.build.id', 'ro.build.version.incremental', 'ro.build.version.release', 'ro.build.version.sdk', 'ro.product.name', 'ro.product.device'].map(key => [key, key === 'ro.build.version.sdk' ? '35' : 'public-fixture'])),
    vending: { packagePresent: false, presence: 'absent', ordinaryListed: false, disabledListed: false, enabledListed: false },
    provenance: { foregroundUser: 0, avdConfig: 'public-avd', avdConfigSha256: hash('public-avd'), sdkProperties: 'public-sdk', sdkPropertiesSha256: hash('public-sdk'), sdkRevision: '12' },
    packages: Object.fromEntries(['com.google.android.gms', 'com.android.chrome', 'com.google.android.trichromelibrary'].map(name => [name, packageFixture(name)])),
    measurement: { id, boundary: 'start', processes: { '42': 'com.android.chrome', '546': 'system_server', '77': 'com.android.chrome_zygote' } } };
  const after = structuredClone(before);
  after.capturedAt = '2026-09-10T08:01:00.000Z';
  after.measurement!.boundary = 'end';
  const log = marker(0, 'START', id) + marker(9, 'END', id);
  return { before: JSON.stringify(before), after: JSON.stringify(after), log, operations: '[]', collector: JSON.stringify({ bytes: Buffer.byteLength(log) }) };
}
export function changePackage(input: EnvironmentInputBytes, mutate: (value: AndroidPackageIdentity) => void, name = 'com.google.android.gms', rehash = true): void {
  const after = JSON.parse(input.after) as AndroidEnvironmentSnapshot;
  mutate(after.packages[name]);
  if (rehash) rehashPackage(after.packages[name]);
  input.after = JSON.stringify(after);
}
export function observationsFixture(identity = measurementIdentity(), pid = '42', cold?: AndroidPlannedTermination): ProductObservations {
  const bootId = '11111111-1111-1111-1111-111111111111';
  const native = { pid, startTime: '12080', bootId, namespace: 'kernel-pid-namespaces-disabled' as const,
    kernelCapability: { mode: 'disabled' as const, source: '/proc/config.gz' as const, sha256: 'e'.repeat(64), compressedBytes: 100, configBytes: 200, compressedLimit: 262144 as const, configLimit: 2097152 as const, bootId, acquiredStartedAt: 1, acquiredFinishedAt: 2 } };
  const value: ProductObservations = { schema: 1, identity: structuredClone(identity), coverage: 'bounded-operations', initialOwner: 1, initialNative: structuredClone(native), records: [] };
  let owner = 1, completed = 0, lastInspection = 0;
  const append = (record: Omit<ProductObservations['records'][number], 'ordinal' | 'owner'>) => { const ordinal = value.records.length + 1; value.records.push({ ...record, ordinal, owner }); return ordinal; };
  const inspect = () => {
    const time = 100 * (value.records.length + 1);
    lastInspection = append({ kind: 'inspection', name: 'installed-selected', target: owner, document: owner, native: structuredClone(native), startedAt: time, finishedAt: time + 20, deadline: time + 30, pageCount: 1,
      bounds: [0, 15].map(offset => ({ startedAt: time + offset, finishedAt: time + offset + 5, native: [0, 3].map(n => ({ ...structuredClone(native), startedAt: time + offset + n, finishedAt: time + offset + n + 1 })) })),
      association: { kind: 'bounded-sequential-service-to-process', before: { pid: Number(native.pid), requestId: 1, connection: time, startedAt: time + 6, completedAt: time + 7 }, after: { pid: Number(native.pid), requestId: 11, connection: time, startedAt: time + 10, completedAt: time + 11 } } });
  };
  const checkpoint = (name: string) => append({ kind: 'checkpoint', name, completedOperations: completed, ...(lastInspection ? { beforeInspection: lastInspection } : {}) });
  const transition = (name: string) => append({ kind: 'transition', name, ...(lastInspection ? { beforeInspection: lastInspection } : {}) });
  const perform = (name: 'read' | 'mutation', detail: 'identity' | 'completion' | 'preference' | 'control') => {
    inspect();
    const operation = append({ kind: 'operation-begin', name, detail, beforeInspection: lastInspection, sequence: completed + 1 });
    inspect();
    append({ kind: 'operation-end', name, operation, afterInspection: lastInspection, sequence: ++completed });
  };
  checkpoint('measurement-start'); transition('initial-install');
  perform('read', 'identity'); transition('pairing-navigation'); perform('mutation', 'control'); perform('read', 'preference'); checkpoint('paired');
  transition('update-activation'); perform('mutation', 'control'); perform('read', 'identity'); perform('read', 'completion'); checkpoint('candidate');
  transition('warm-launch'); perform('read', 'identity'); perform('read', 'preference');
  if (identity.suite === 'release') {
    perform('read', 'completion'); perform('mutation', 'control');
    owner = 2;
    append({ kind: 'cold-handoff', name: 'planned-cold', operation: 1, termination: cold });
    native.pid = '43'; native.startTime = '13000'; lastInspection = 0;
    perform('read', 'identity'); perform('read', 'preference'); checkpoint('lifecycle-complete');
  }
  checkpoint('scenario-complete');
  return value;
}
export function driverFixture(teardown = false) {
  return { sessionId: teardown ? '' : '[active]', selectedContext: 'CHROMIUM', selectedWindow: 'installed', unusable: false, firstFatal: null, commands: [], lookups: [] };
}
export function resultContract(identity = measurementIdentity()): MobileResultContract {
  return { schema: 2, identity, product: { status: 'PASS', categories: [] }, finalization: { status: 'PASS', failures: [] }, additional_failures: [],
    environment_qualification: identity.platform === 'ios' ? { platform: 'ios', applicability: 'NOT_APPLICABLE' } : { platform: 'android', status: 'PASS', collection: 'PASS', assessmentSha256: '', measurementId: identity.measurementId } };
}
export function checkCLI(fixture: { root: string; environment?: NodeJS.ProcessEnv }, files: { before: string; after: string; log: string; operations: string; output: string }, contract: 'qualification' | 'report' = 'qualification'): { passed: boolean; stderr: string; issues: string[]; args: string[]; directory: string } {
  const directory = join(fixture.root, 'schema3-check');
  mkdirSync(directory, { recursive: true });
  const input = Object.fromEntries(Object.entries(files).filter(([key]) => key !== 'output').map(([key, path]) => { try { return [key, readFileSync(path, 'utf8')]; } catch { return [key, '']; } })) as unknown as EnvironmentInputBytes;
  input.collector = JSON.stringify({ bytes: Buffer.byteLength(input.log) });
  const measurementId = (() => {
    try { return JSON.parse(input.before).measurement.id; } catch { return 'android-test'; }
  })();
  const identity = measurementIdentity({ measurementId });
  const assessment = assessAndroidEnvironment(input, identity);
  for (const [key, leaf] of Object.entries(environmentLeaves)) writeFileSync(join(directory, leaf), input[key as keyof EnvironmentInputBytes]);
  writeFileSync(join(directory, 'android-environment-check.json'), JSON.stringify(assessment));
  writeFileSync(files.output, JSON.stringify(assessment));
  writeFileSync(join(directory, 'android-environment-session.json'), JSON.stringify({ id: identity.measurementId }));
  writeFileSync(join(directory, 'identity.json'), JSON.stringify(identity));
  writeFileSync(join(directory, 'bundle-set.json'), JSON.stringify({ candidate: { provenance: { sourceCommit: identity.sourceCommit }, identity: { webHash: identity.candidateWebHash, build: identity.candidateBuild } } }));
  const args = ['check', '--contract', contract, '--directory', directory, '--identity', join(directory, 'identity.json'), '--bundle-set', join(directory, 'bundle-set.json'), '--run-id', identity.runId, '--attempt', identity.attempt, '--suite', identity.suite, '--baseline', identity.baseline, '--scenario', identity.scenario, '--source-run-head-sha', identity.sourceRunHeadSha];
  let passed = true, stderr = '';
  try { execFileSync(process.execPath, [process.env.ANDROID_ENVIRONMENT_SOURCE || repositoryPath('tests/mobile/android-environment.ts'), ...args], { env: fixture.environment || process.env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }); }
  catch (error) { passed = false; stderr = String((error as { stderr?: string }).stderr || error); }
  assert.equal(readFileSync(join(directory, 'android-environment-check.json'), 'utf8'), JSON.stringify(assessment));
  return { passed, stderr, issues: assessment.issues, args, directory };
}
export async function producerFixture(): Promise<Record<string, any>> {
  const source = async (name: string) => JSON.parse(await readFile(repositoryPath(`tests/mobile/android-appium/${name}`), 'utf8'));
  const manifest = await source('producer-manifest.json');
  const packages = { appium: await source('appium-runtime-manifest.json'), ...await source('runtime-manifest.json') };
  const data: Record<string, any> = { node: '24.21.0', home: '/public/synthetic-appium', inputs: manifest.inputs, authored: Object.fromEntries(Object.entries(manifest.authored).map(([name, value]: [string, any]) => [name, value.sha256])) };
  const entries: Record<string, string> = {};
  for (const [id, value] of Object.entries(packages) as Array<[string, any]>) {
    const root = posix.join(data.home, value.root), entry = new URL(`file://${posix.join(root, value.entry)}`).href;
    data[id] = { entry, parent: value.parent ? entries[value.parent] : new URL(`file://${data.home}/package.json`).href, realpath: root, condition: value.condition, version: value.version, engines: value.engines || {},
      files: Object.fromEntries(Object.entries(value.files).map(([file, original]) => [file, id === 'android' && manifest.files[file] ? manifest.files[file].after : original])) };
    entries[id] = entry;
  }
  const context = new URL('./commands/context/exports.js', entries.android).href;
  data.inspectionResolvers = { context, helper: new URL('./retained-inspection.cjs', context).href, transport: new URL('./target-inspection.cjs', context).href, ws: entries.ws, axiosImporter: new URL('./jsonwp-proxy/proxy-request.js', entries.base).href, axios: entries.axios, axiosVersion: packages.axios.version, absent: ['bufferutil', 'utf-8-validate'] };
  data.selected = { uiautomator2: { pkgName: packages.uiautomator2.name, version: packages.uiautomator2.version, mainClass: 'AndroidUiautomator2Driver', automationName: 'UiAutomator2', installType: 'npm', installPath: posix.join(data.home, packages.uiautomator2.root) } };
  return data;
}
export async function persistAndroidProof(directory: string, identity: MeasurementIdentity, input = environmentFixture(identity.measurementId)): Promise<{ environment_qualification: MobileResultContract['environment_qualification']; evidence: Record<string, unknown> }> {
  for (const [key, leaf] of Object.entries(environmentLeaves)) await writeFile(join(directory, leaf), input[key as keyof EnvironmentInputBytes]);
  const assessment = assessAndroidEnvironment(input, identity);
  await writeSanitizedJson(join(directory, 'android-environment-check.json'), assessment);
  const assessmentSha256 = hash(await readFile(join(directory, 'android-environment-check.json')));
  await writeSanitizedJson(join(directory, 'android-environment-session.json'), { id: identity.measurementId });
  await writeSanitizedJson(join(directory, 'android-environment-identity.json'), identity);
  await writeSanitizedJson(join(directory, 'android-environment-completion.json'), { passed: true, errors: [], assessmentSha256 });
  await writeFile(join(directory, 'android-appium-integrity.json'), `${JSON.stringify(await producerFixture(), null, 2)}\n`, { mode: 0o600 });
  return { environment_qualification: { platform: 'android', status: assessment.status, collection: assessment.collection.status, assessmentSha256, measurementId: identity.measurementId },
    evidence: { driver: driverFixture(), selectedInstalledWindowValid: true, productObservations: observationsFixture(identity), productEventRelations: androidProductImpact(assessment, observationsFixture(identity)).relations, teardown: { driver: driverFixture(true) } } };
}
export async function evidenceFixture(platform: 'android' | 'ios' = 'android', input = environmentFixture()) {
  const directory = await mkdtemp(join(tmpdir(), 'sm59-evidence-'));
  const identity = measurementIdentity({ platform, measurementId: platform === 'ios' ? 'ios-not-applicable' : 'android-test' });
  const baseline = { version: '0.20.10', assets: 363, build: 'e'.repeat(64), entry: '/old/index.html', script: '/old.js', style: '/old.css', webHash: 'f'.repeat(64) };
  const candidate = { version: '0.21.0', assets: 370, build: identity.candidateBuild, entry: '/new/index.html', script: '/new.js', style: '/new.css', webHash: identity.candidateWebHash };
  const runtime = { standalone: true, provider: platform === 'ios' ? 'ios-home-screen' : 'android-standalone', nativeProvider: platform === 'ios' ? 'ios:com.apple.webapp' : 'android:com.android.chrome', nativeActivity: 'org.chromium.chrome.browser.webapps.WebappActivity', nativePid: '42', origin: 'https://fixture.test', url: 'https://fixture.test/', requiredAssetsReady: true, applicationInitialized: true, buildFromApplication: true };
  const result = { ...resultContract(identity), platform, suite: 'smoke', baseline: identity.baseline, candidate: 'candidate-current', source_commit: identity.sourceCommit, source_run_head_sha: identity.sourceRunHeadSha, candidate_web_hash: identity.candidateWebHash,
    origin: runtime.origin, initial_identity: { ...runtime, ...baseline }, final_identity: { ...runtime, ...candidate }, credential_preserved: true,
    credential_evidence: { relays: Object.fromEntries(['alpha', 'beta'].map(name => [name, { invitationAuthCount: 1, credentialAuthCount: 2, credentialPseudonyms: [name], connections: 1 }])) }, preference_preserved: true,
    phone_completion: { rawPlanPresent: true, phoneRequired: true, phoneAcknowledged: true, phoneState: 'loaded', visibleCompletion: true }, faults_exercised: ['corrupt:/new.js'], fault_identity: { id: 'fault', generation: '1', kind: 'corrupt', path: '/new.js' }, fixture_requests: [{ release: 'candidate', path: '/new.js', fault: 'corrupt', fault_id: 'fault', fault_generation: '1' }],
    ...(platform === 'android' ? await persistAndroidProof(directory, identity, input) : { evidence: { driver: driverFixture(), teardown: { driver: driverFixture(true) } } }) };
  const options: EvidenceValidationOptions = { contract: 'product', runId: identity.runId, attempt: identity.attempt, directory, matrix: [{ platform, baseline: identity.baseline, scenario: 'historical' }], suite: 'smoke', candidateCommit: identity.sourceCommit, sourceRunHeadSha: identity.sourceRunHeadSha, candidateWebHash: identity.candidateWebHash, candidateIdentity: candidate, baselineIdentities: [{ name: identity.baseline, identity: baseline }] };
  const persist = () => writeSanitizedJson(join(directory, 'mobile-result.json'), result);
  await persist();
  return { directory, identity, result, options, persist };
}
async function platformObservationControl(): Promise<ProductObservations> {
  const { retainedFixture } = await import('./android-retained-fixture');
  const { AndroidPlatform } = await import('../platforms/android');
  const { AppiumClient } = await import('../support/webdriver');
  const { PhaseBudget } = await import('../support/budget');
  const handle = '753D4398F5ABC414D3DAABBF0B329743', startedAt = Date.now() - 1;
  const budget = new PhaseBudget('sm59-recorder-seam', { timeoutMs: 300_000, recoveryLimit: 0 });
  let document = startedAt - 1, deferredNavigation = false, inspections = 0;
  const client = new AppiumClient('http://recorder.invalid', 30_000, async (input, init) => {
    const path = new URL(String(input)).pathname, body = init?.body ? JSON.parse(String(init.body)) : {};
    const response = (value: unknown) => Response.json({ value, sessionId: 'original' });
    if (path === '/session') return response({});
    if (path.endsWith('/window/handles')) return response([handle]);
    if (path.endsWith('/window')) return response(handle);
    if (path.endsWith('/url')) return response('https://fixture.test/');
    if (path.endsWith('/elements')) return response([{ 'element-6066-11e4-a52e-4f735466cecf': 'button' }]);
    if (path.endsWith('/text')) return response('Load Update');
    if (path.endsWith('/click')) { deferredNavigation = true; return response(null); }
    if (path.endsWith('/execute/sync')) {
      if (body.script === 'mobile: inspectRetainedChromeTargets') {
        inspections++;
        if (deferredNavigation && inspections % 2 === 0) { document++; deferredNavigation = false; }
        const fixture = retainedFixture([handle], handle, handle, startedAt);
        fixture.before.document.timeOrigin = fixture.after.document.timeOrigin = document;
        fixture.observations[0].document.timeOrigin = document;
        return response(fixture);
      }
      if (body.script.includes('localStorage.getItem')) return response('state');
      return response({ navigationId: String(document), standalone: true, origin: 'https://fixture.test' });
    }
    return response(null);
  });
  await client.create({ capabilities: {}, budget });
  const platform = new AndroidPlatform({ origin: 'https://fixture.test', appiumUrl: 'http://recorder.invalid', outputDir: tmpdir(), certificate: '', setupUrl: '', deviceId: 'emulator-5554', budget });
  const seam = platform as any;
  seam.driver = client;
  seam.retainedOwner = { ...retainedFixture([handle], handle, handle, startedAt).original, driver: client, assertSession: client.retainSessionOwner() };
  seam.installedTarget = { packageName: 'com.android.chrome', activity: 'org.chromium.chrome.browser.webapps.WebappActivity', shortcut: { scope: 'https://fixture.test/' } };
  seam.installedPackage = 'com.android.chrome';
  seam.assertRetainedOwner = async () => seam.assertRetainedSession();
  seam.launchInstalledTarget = async () => seam.inspectInstalledView();
  platform.activateProductRecorder(measurementIdentity());
  await platform.launchInstalledApp();
  await platform.readRunningIdentity();
  await platform.openSetupURLInInstalledApp('https://fixture.test/setup');
  await platform.preferenceValue();
  platform.productCheckpoint('paired');
  await platform.clickDialogText('update-herdr-dialog', 'Load Update');
  await platform.readRunningIdentity();
  await platform.readUpdateCompletion();
  platform.productCheckpoint('candidate');
  await platform.relaunchInstalledApp();
  await platform.readRunningIdentity();
  await platform.preferenceValue();
  platform.productCheckpoint('scenario-complete');
  const observations = platform.evidenceSnapshot().productObservations as ProductObservations;
  validateProductObservations(observations, measurementIdentity());
  assert.equal(observations.records.filter(record => record.kind === 'inspection').length, inspections);
  assert.ok(observations.records.some(record => record.kind === 'document-change'));
  await client.close();
  return observations;
}

function withoutOperation(value: ProductObservations, operation: number): ProductObservations {
  const altered = structuredClone(value);
  altered.records = altered.records.filter(record => record.ordinal !== operation && !(record.kind === 'operation-end' && record.operation === operation));
  const mapping = new Map(altered.records.map((record, index) => [record.ordinal, index + 1]));
  for (const record of altered.records) {
    record.ordinal = mapping.get(record.ordinal)!;
    if (record.beforeInspection) record.beforeInspection = mapping.get(record.beforeInspection);
    if (record.afterInspection) record.afterInspection = mapping.get(record.afterInspection);
    if (record.kind === 'operation-end') record.operation = mapping.get(record.operation!);
  }
  return altered;
}

async function evidenceCLI(fixture: Awaited<ReturnType<typeof evidenceFixture>>, report = false, directory = fixture.directory) {
  const { options } = fixture;
  const output = join(fixture.directory, 'public-summary.json'), summary = join(fixture.directory, 'step-summary.txt'), stepOutput = join(fixture.directory, 'step-output.txt');
  for (const path of [output, summary, stepOutput]) await writeFile(path, '');
  const values = { '--contract': options.contract, '--directory': directory, '--run-id': options.runId, '--attempt': options.attempt, '--suite': options.suite,
    '--matrix': JSON.stringify(options.matrix), '--candidate-commit': options.candidateCommit, '--source-run-head-sha': options.sourceRunHeadSha,
    '--candidate-web-hash': options.candidateWebHash, '--candidate-identity': JSON.stringify(options.candidateIdentity), '--baseline-identities': JSON.stringify(options.baselineIdentities) };
  const child = spawnSync(process.execPath, [repositoryPath(`tests/mobile/validate-${report ? 'environment' : 'evidence'}.ts`), ...Object.entries(values).flat(), ...(report ? ['--output', output] : [])],
    { encoding: 'utf8', timeout: 10_000, env: { ...process.env, GITHUB_STEP_SUMMARY: summary, GITHUB_OUTPUT: stepOutput } });
  assert.equal(child.error, undefined);
  return { status: child.status, surfaces: [child.stdout, child.stderr, ...await Promise.all([output, summary, stepOutput].map(path => readFile(path, 'utf8')))], stdout: child.stdout };
}

async function environmentCheckCLI(fixture: Awaited<ReturnType<typeof evidenceFixture>>) {
  const identity = fixture.identity;
  const bundle = join(fixture.directory, 'bundle-set.json');
  await writeFile(bundle, JSON.stringify({ candidate: { provenance: { sourceCommit: identity.sourceCommit }, identity: fixture.options.candidateIdentity } }));
  const child = spawnSync(process.execPath, [repositoryPath('tests/mobile/android-environment.ts'), 'check', '--contract', 'report', '--directory', fixture.directory,
    '--identity', join(fixture.directory, 'android-environment-identity.json'), '--bundle-set', bundle, '--run-id', identity.runId, '--attempt', identity.attempt,
    '--suite', identity.suite, '--baseline', identity.baseline, '--scenario', identity.scenario, '--source-run-head-sha', identity.sourceRunHeadSha], { encoding: 'utf8', timeout: 10_000 });
  assert.equal(child.error, undefined);
  return child;
}

export const measurementFailureCases = ['none', 'begin-snapshot', 'begin-marker', 'close', 'transport', 'log', 'collector', 'identity', 'check', 'completion', 'end-snapshot'] as const;
export async function measurementControl(failure: typeof measurementFailureCases[number]): Promise<void> {
  const directory = await mkdtemp(join(tmpdir(), 'sm59-measurement-'));
  const trace: string[] = [];
  const original = new Error(`injected-${failure}`), cleanup = new Error('injected-cleanup');
  const child = Object.assign(new EventEmitter(), { stdout: new EventEmitter(), stderr: new EventEmitter(), kill: () => true }) as unknown as ChildProcess;
  const failBegin = failure.startsWith('begin-');
  const io: Partial<AndroidMeasurementIO> = {
    requireOwnedDevice: async () => undefined,
    spawnCollector: () => child,
    command: async (_file, args) => {
      const boundary = args[args.indexOf('--boundary') + 1];
      trace.push(`snapshot:${boundary}`);
      if ((failure === 'begin-snapshot' && boundary === 'start') || (failure === 'end-snapshot' && boundary === 'end')) throw original;
      const input = environmentFixture(measurement.id);
      await writeFile(args[args.indexOf('--output') + 1], boundary === 'start' ? input.before : input.after);
      if (failure === 'begin-marker' && boundary === 'start') child.stderr!.emit('data', Buffer.from('failed marker transport'));
      else child.stdout!.emit('data', Buffer.from(marker(boundary === 'start' ? 0 : 9, boundary === 'start' ? 'START' : 'END', measurement.id) + (failure === 'none' && boundary === 'start' ? '1789100401.000 2000 2000 I PublicFixture: https://fixture.test/?token=PRIVATE_SENTINEL\n' : '')));
      return { stdout: '', stderr: '' } as Awaited<ReturnType<AndroidMeasurementIO['command']>>;
    },
    stopProcess: async () => { trace.push('close'); if (failure === 'close') throw original; if (failBegin) throw cleanup; },
    transport: { observe: () => undefined, finish: async () => { trace.push('transport'); if (failure === 'transport') throw original; }, failure: undefined },
    writeFile: (async (path: any, data: any, options: any) => { trace.push(String(path).endsWith('.log') ? 'log' : 'session'); if (failure === 'log' && String(path).endsWith('.log')) throw original; return writeFile(path, data, options); }) as typeof writeFile,
    writeJson: async (path, data) => { const name = path.match(/android-environment-(\w+)\.json$/u)![1]; trace.push(name); if (name === failure) throw original; if (failBegin && name === 'completion') throw new Error('injected-receipt'); await writeSanitizedJson(path, data); },
  };
  const measurement = new AndroidEnvironmentMeasurement('emulator-5554', directory, 'unused-public-toolchains', io);
  measurement.bind(measurementIdentity({ measurementId: measurement.id }));
  if (failBegin) {
    let caught: unknown;
    try { await measurement.begin(); } catch (error) { caught = error; }
    assert.ok(caught);
    if (failure === 'begin-snapshot') assert.equal(caught, original);
    assert.equal(measurement.failureOutcome().errors[0], caught);
    assert.ok(measurement.failureOutcome().errors.includes(cleanup));
    assert.ok(trace.indexOf('close') < trace.indexOf('transport'));
    assert.ok(trace.includes('collector') && trace.includes('completion'));
    return;
  }
  await measurement.begin();
  const outcome = await measurement.finish();
  assert.ok(trace.indexOf('snapshot:end') < trace.indexOf('close'));
  for (const step of ['close', 'transport', 'log', 'collector', 'identity', 'check', 'completion']) assert.ok(trace.includes(step), `${failure}/${step}`);
  if (failure === 'none') { assert.deepEqual(outcome.errors, []); assert.equal(outcome.assessment?.status, 'PASS'); assert.equal(outcome.assessmentSha256, hash(await readFile(join(directory, 'android-environment-check.json')))); }
  else assert.ok(outcome.errors.includes(original), failure);
  if (failure === 'none') {
    const log = await readFile(join(directory, environmentLeaves.log)), collector = JSON.parse(await readFile(join(directory, environmentLeaves.collector), 'utf8'));
    assert.doesNotMatch(log.toString(), /PRIVATE_SENTINEL/u); assert.equal(collector.bytes, log.length); assert.notEqual(collector.acquiredBytes, collector.bytes);
    assert.equal(outcome.assessment!.inputs.log.sha256, hash(log)); assert.equal(outcome.assessment!.inputs.log.bytes, log.length);
  }
  await assert.rejects(measurement.finish(), /already finalized/u);
}

export const androidProductEnvironmentTests: Array<[string, () => Promise<void>]> = [
  ['Android product/environment SM59 01 component-only decision', async () => {
    for (const mode of ['stable', 'dump-noise', 'enabled', 'disabled', 'equal-count-members']) {
      const input = environmentFixture();
      if (mode !== 'stable') changePackage(input, value => { if (mode === 'dump-noise') value.dumpSha256 = hash('noise'); else value.dependencyConfig[mode === 'disabled' ? 'disabledComponents' : 'enabledComponents'] = ['com.google.android.gms.PublicComponent']; });
      if (mode === 'equal-count-members') { const before = JSON.parse(input.before); before.packages['com.google.android.gms'].dependencyConfig.enabledComponents = ['com.google.android.gms.OtherComponent']; rehashPackage(before.packages['com.google.android.gms']); input.before = JSON.stringify(before); }
      const check = assessAndroidEnvironment(input, measurementIdentity());
      assert.equal(check.collection.status, 'PASS', JSON.stringify(check.collection));
      assert.equal(check.status, ['stable', 'dump-noise'].includes(mode) ? 'PASS' : 'FAIL');
      assert.equal(androidProductImpact(check, observationsFixture()).status, 'PASS');
      const fixture = await evidenceFixture('android', input);
      await validateMobileEvidence(fixture.options);
      if (check.status === 'FAIL') await assert.rejects(validateMobileEvidence({ ...fixture.options, contract: 'qualified' }), /strict environment/u);
      else await validateMobileEvidence({ ...fixture.options, contract: 'qualified' });
    }
  }],
  ['Android product/environment SM59 02 noncomponent identity and hash rejection', async () => {
    for (const field of ['chrome-components', 'trichrome-components', 'source', 'version', 'path', 'hidden', 'library', 'hash', 'missing-hash', 'duplicate', 'unknown-section', 'withheld']) {
      const input = environmentFixture();
      changePackage(input, value => {
        if (field.endsWith('components')) value.dependencyConfig.enabledComponents = ['Public.Component'];
        else if (field === 'source') value.packageSource = '1';
        else if (field === 'version') value.versionCode = '2';
        else if (field === 'path') value.codePath = '/new/path';
        else if (field === 'hidden') value.hidden = true;
        else if (field === 'library') value.dependencyConfig.usesLibraries = ['new.library'];
        else if (field === 'hash') value.identitySha256 = 'f'.repeat(64);
        else if (field === 'missing-hash') delete (value as Partial<AndroidPackageIdentity>).dependencyConfigSha256;
        else if (field === 'duplicate') value.dependencyConfig.enabledComponents = ['Public.Component', 'Public.Component'];
        else if (field === 'unknown-section') value.dependencyConfig.unknown = [];
        else value.dependencyConfig.enabledComponents = ['[REDACTED]'];
      }, field === 'chrome-components' ? 'com.android.chrome' : field === 'trichrome-components' ? 'com.google.android.trichromelibrary' : 'com.google.android.gms', !['hash', 'missing-hash'].includes(field));
      const check = assessAndroidEnvironment(input, measurementIdentity());
      assert.equal(androidProductImpact(check, observationsFixture()).status, 'FAIL', field);
      assert.notEqual(check.status, 'PASS', field);
    }
  }],
  ['Android product/environment SM59 03 adverse event relation', async () => {
    for (const owned of [false, true]) for (const initiator of [false, true]) {
      const input = environmentFixture(), pid = '42';
      const event = `1789100404.000 546 546 I ActivityManager: Killing ${pid}:com.android.chrome/u0a146 (adj 0): isolated not needed${initiator ? ' from pid 100' : ''}\n`;
      input.log = marker(0, 'START') + event + `1789100405.000 77 77 I Zygote: Process ${pid} exited due to signal 9 (Killed)\n` + marker(9, 'END');
      input.collector = JSON.stringify({ bytes: Buffer.byteLength(input.log) });
      const check = assessAndroidEnvironment(input, measurementIdentity());
      assert.equal(check.status, 'FAIL');
      assert.equal(check.eventCounts.fatalEvents, 2);
      assert.equal(androidProductImpact(check, observationsFixture(measurementIdentity(), owned ? '42' : '43')).status, owned ? 'FAIL' : 'INDETERMINATE');
      assert.equal(check.nativeEvents[0].initiatorPid, initiator ? '100' : undefined);
    }
  }],
  ['Android product/environment SM59 04 retirement and cold handoff', async () => {
    const input = environmentFixture();
    const normal = [
      '1789100401.000 77 77 I Zygote: Forked child process 5820',
      '1789100402.000 546 546 I ActivityManager: Start proc 5820:com.android.chrome:sandboxed_process0:org.chromium.content.app.SandboxedProcessService0:0/u0ai0 for  {com.android.chrome/org.chromium.content.app.SandboxedProcessService0:0}',
      '1789100403.000 5820 5820 I chromium: [INFO:child_process_service.cc(1)] ChildProcessService: Exiting child process.',
      '1789100404.000 77 77 I Zygote: Process 5820 exited cleanly (0)',
      '1789100405.000 546 546 I ActivityManager: Process com.android.chrome:sandboxed_process0:org.chromium.content.app.SandboxedProcessService0:0 (pid 5820) has died: vis BTOP',
    ];
    input.log = marker(0, 'START') + normal.join('\n') + '\n' + marker(9, 'END');
    input.collector = JSON.stringify({ bytes: Buffer.byteLength(input.log) });
    assert.equal(assessAndroidEnvironment(input, measurementIdentity()).normalRetirements.length, 1);
    for (const mutation of ['fork', 'birth', 'producer', 'uid', 'service', 'exit', 'boundary', 'signal', 'reuse']) {
      const lines = [...normal];
      if (mutation === 'fork') lines.splice(0, 1);
      if (mutation === 'birth') lines.splice(1, 1);
      if (mutation === 'producer') lines[0] = lines[0].replace('77 77', '78 78');
      if (mutation === 'uid') lines[1] = lines[1].replace('u0ai0', 'u0ai9999');
      if (mutation === 'service') lines[1] = lines[1].replace('Service0:0}', 'Service0:1}');
      if (mutation === 'exit') lines[3] = lines[3].replace('(0)', '(1)');
      if (mutation === 'boundary') lines[0] = lines[0].replace('1789100401', '1789100399');
      if (mutation === 'signal') lines.splice(3, 0, '1789100403.500 5820 5820 I Process: Sending signal. PID: 5820 SIG: 9');
      if (mutation === 'reuse') lines.splice(2, 0, lines[1]);
      const log = marker(0, 'START') + lines.join('\n') + '\n' + marker(9, 'END');
      const check = assessAndroidEnvironment({ ...input, log, collector: JSON.stringify({ bytes: Buffer.byteLength(log) }) }, measurementIdentity());
      assert.equal(check.normalRetirements.length, 0, mutation);
      for (const pid of ['42', '5820']) assert.notEqual(androidProductImpact(check, observationsFixture(measurementIdentity(), pid)).status, 'PASS', `${mutation}/${pid}`);
    }
    const identity = measurementIdentity({ suite: 'release' });
    const operation: AndroidPlannedTermination = { id: 'cold-1', measurementId: identity.measurementId, packageName: 'com.android.chrome', pid: '42', processes: { '42': 'com.android.chrome' }, command: ['shell', 'am', 'force-stop', '--user', '0', 'com.android.chrome'], succeeded: true };
    const observations = observationsFixture(identity, '42', operation);
    validateProductObservations(observations, identity, [operation]);
    for (const initiator of [false, true]) {
      const releaseInput = environmentFixture();
      const after = JSON.parse(releaseInput.after); delete after.measurement.processes['42']; releaseInput.after = JSON.stringify(after);
      releaseInput.operations = JSON.stringify([operation]);
      const birth = '1789100403.000 546 546 I ActivityManager: Start proc 43:com.android.chrome/u0a146 for activity {com.android.chrome/Main}\n';
      releaseInput.log = marker(0, 'START') + marker(1, 'OP_BEGIN cold-1 com.android.chrome 42') + marker(2, 'OP_END cold-1 com.android.chrome 42') + birth
        + `1789100404.000 546 546 I ActivityManager: Killing 43:com.android.chrome/u0a146 (adj 0): measured loss${initiator ? ' from pid 100' : ''}\n`
        + '1789100405.000 77 77 I Zygote: Process 43 exited due to signal 9 (Killed)\n' + marker(9, 'END');
      releaseInput.collector = JSON.stringify({ bytes: Buffer.byteLength(releaseInput.log) });
      const check = assessAndroidEnvironment(releaseInput, identity), impact = androidProductImpact(check, observations);
      assert.equal(check.collection.status, 'PASS'); assert.equal(check.eventCounts.fatalEvents, 2); assert.equal(check.status, 'FAIL');
      assert.equal(impact.status, 'FAIL'); assert.ok(impact.relations.every(relation => relation.relation === 'OWNED_BROWSER' && relation.proof?.owner === 2));
      assert.equal(impact.relations[0].proof!.startLine, 3); assert.equal(impact.relations[0].proof!.clock, 'logcat-epoch-ms');
      for (const mode of ['unknown-child', 'missing-birth', 'reused-pid', 'missing-boundary', 'old-segment-time', 'missing-inspection-bound', 'numeric-owner-reuse']) {
        const alteredInput = structuredClone(releaseInput), alteredObservations = structuredClone(observations);
        if (mode === 'numeric-owner-reuse') {
          alteredInput.log = alteredInput.log.replaceAll('43:', '42:').replaceAll('Process 43 ', 'Process 42 ');
          for (const record of alteredObservations.records.filter(record => record.kind === 'inspection' && record.owner === 2)) {
            record.native!.pid = '42';
            for (const bound of record.bounds!) for (const native of bound.native) native.pid = '42';
            record.association!.before.pid = record.association!.after.pid = 42;
          }
        }
        if (mode === 'unknown-child') alteredInput.log = alteredInput.log.replaceAll('43:', '5820:').replaceAll('Process 43 ', 'Process 5820 ');
        if (mode === 'missing-birth') alteredInput.log = alteredInput.log.replace(birth, '');
        if (mode === 'reused-pid') alteredInput.log = alteredInput.log.replace(birth, birth + birth.replace('0403.000', '0403.500'));
        if (mode === 'old-segment-time') alteredInput.log = alteredInput.log.replace('0404.000', '0401.500').replace('0405.000', '0401.600');
        let altered = assessAndroidEnvironment({ ...alteredInput, collector: JSON.stringify({ bytes: Buffer.byteLength(alteredInput.log) }) }, identity);
        if (mode === 'missing-boundary') altered = { ...altered, processBoundaryProof: undefined };
        if (mode === 'missing-inspection-bound') { alteredObservations.records.find(record => record.kind === 'inspection' && record.owner === 2)!.bounds = undefined; }
        const uncertain = androidProductImpact(altered, alteredObservations);
        assert.notEqual(uncertain.status, 'PASS', mode);
        assert.ok(uncertain.relations.every(relation => relation.relation === 'UNRESOLVED'), mode);
        assert.equal(altered.eventCounts.fatalEvents, 2, mode);
      }
    }

    for (const field of ['id', 'measurementId', 'pid', 'packageName', 'ordinal', 'owner']) {
      const altered = structuredClone(observations), handoff = altered.records.find(record => record.kind === 'cold-handoff')!;
      if (field === 'ordinal') handoff.operation = 2;
      else if (field === 'owner') handoff.owner = 3;
      else (handoff.termination as any)[field] = 'changed';
      assert.throws(() => validateProductObservations(altered, identity, [operation]), /coverage/u, field);
    }
  }],
  ['Android product/environment SM59 05 observation and producer binding', async () => {
    validateProductObservations(observationsFixture(), measurementIdentity());
    const recorded = await platformObservationControl();
    for (const value of [observationsFixture(), recorded]) {
      for (const operation of value.records.filter(record => record.kind === 'operation-begin')) {
        assert.throws(() => validateProductObservations(withoutOperation(value, operation.ordinal), value.identity), /coverage/u);
      }
      const collapsed = structuredClone(value), inspection = value.records.find(record => record.kind === 'inspection')!;
      collapsed.records = [value.records[0], inspection, ...value.records.filter(record => record.kind === 'checkpoint' && record.name !== 'measurement-start')].map((record, index) => ({ ...record, ordinal: index + 1, ...(record.kind === 'checkpoint' ? { completedOperations: 0, beforeInspection: index ? 2 : undefined } : {}) }));
      assert.throws(() => validateProductObservations(collapsed, value.identity), /coverage/u);
    }
    const unrecorded = structuredClone(recorded);
    unrecorded.records = unrecorded.records.filter(record => record.kind !== 'document-change');
    const remap = new Map(unrecorded.records.map((record, index) => [record.ordinal, index + 1]));
    for (const record of unrecorded.records) {
      record.ordinal = remap.get(record.ordinal)!;
      if (record.beforeInspection) record.beforeInspection = remap.get(record.beforeInspection);
      if (record.afterInspection) record.afterInspection = remap.get(record.afterInspection);
      if (record.kind === 'operation-end') record.operation = remap.get(record.operation!);
    }
    assert.throws(() => validateProductObservations(unrecorded, unrecorded.identity), /coverage/u);
    const betweenReads = observationsFixture();
    const nextRead = betweenReads.records.find(record => record.kind === 'operation-begin' && record.detail === 'completion')!;
    const before = betweenReads.records[nextRead.beforeInspection! - 1];
    before.document!++;
    assert.throws(() => validateProductObservations(betweenReads, betweenReads.identity), /coverage/u);

    for (const field of ['target', 'document', 'owner', 'birth', 'boot', 'namespace', 'capability', 'pid', 'connection', 'fractional-connection', 'request', 'time', 'page-count', 'measurement', 'phase', 'last-checkpoint']) {
      const value = observationsFixture(), receipt = value.records[value.records.find(record => record.kind === 'operation-end')!.afterInspection! - 1];
      if (field === 'target') receipt.target = 2;
      if (field === 'document') receipt.document = 2;
      if (field === 'owner') receipt.owner = 2;
      if (field === 'birth') receipt.native!.startTime = '12081';
      if (field === 'boot') receipt.native!.bootId = '22222222-2222-2222-2222-222222222222';
      if (field === 'namespace') receipt.native!.namespace = 'reader-and-browser-active-in-procfs-mount-pid-namespace';
      if (field === 'capability') receipt.native!.kernelCapability.sha256 = 'private-hash';
      if (field === 'pid') receipt.association!.before.pid = 5820;
      if (field === 'connection') receipt.association!.after.connection++;
      if (field === 'fractional-connection') receipt.association!.before.connection = receipt.association!.after.connection = 1.5;
      if (field === 'phase') receipt.name = 'initial-browser-selected';
      if (field === 'last-checkpoint') value.records.push({ ...structuredClone(receipt), ordinal: 9 });
      if (field === 'request') receipt.association!.after.requestId++;
      if (field === 'time') receipt.bounds![0].native[0].finishedAt = receipt.deadline! + 1;
      if (field === 'page-count') receipt.pageCount = 0;
      if (field === 'measurement') value.identity.attempt = '1';
      assert.throws(() => validateProductObservations(value, measurementIdentity()), /coverage/u, field);
    }
    const directory = await mkdtemp(join(tmpdir(), 'sm59-producer-')), path = join(directory, 'producer.json'), original = await producerFixture();
    await writeFile(path, JSON.stringify(original)); await validateAndroidProducer(path);
    for (const field of ['node', 'inputs', 'authored', 'compiled', 'declaration', 'resolver', 'selected', 'allMatch-only']) {
      const value = structuredClone(original);
      if (field === 'node') value.node = '24.16.0';
      if (field === 'inputs') value.inputs[Object.keys(value.inputs)[0]] = '0'.repeat(64);
      if (field === 'authored') value.authored[Object.keys(value.authored)[0]] = '0'.repeat(64);
      if (field === 'compiled' || field === 'declaration') { const key = Object.keys(value.android.files).find(key => key.endsWith(field === 'compiled' ? '.js' : '.d.ts')); assert.ok(key); value.android.files[key] = '0'.repeat(64); }
      if (field === 'resolver') value.inspectionResolvers.ws = 'file:///wrong/resolver.js';
      if (field === 'selected') value.selected.uiautomator2.installPath += '/wrong';
      await writeFile(path, JSON.stringify(field === 'allMatch-only' ? { allMatch: true } : value));
      await assert.rejects(validateAndroidProducer(path), field);
    }
    const controlNames = ['envStatic', 'envDynamic', 'fsRead', 'spawn'] as const;
    type GuardControl = typeof controlNames[number];
    const fileModuleSource = (control: GuardControl, fixture: string): string => {
      const operation = {
        envStatic: 'void process.env.PATH;',
        envDynamic: "const key = 'PUBLIC_DYNAMIC_SENTINEL'; void process.env[key];",
        fsRead: `import * as fs from 'node:fs'; fs.default.readFileSync(${JSON.stringify(fixture)});`,
        spawn: `import * as cp from 'node:child_process'; const child = cp.default.spawn('/usr/bin/printf', ['SM59_GUARD_CHILD'], { stdio: 'ignore' }); await new Promise((resolve, reject) => { child.once('error', reject); child.once('exit', () => resolve(undefined)); });`,
      }[control];
      return `globalThis.__sm59GuardState.moduleBodyWitness += 1;\nglobalThis.__sm59GuardState.operationAttempt += 1;\n${operation}\nexport const sm59Fixture = 'SM59_FILE_MODULE';\n`;
    };
    const childEnvironment = (directory: string): NodeJS.ProcessEnv => ({
      PATH: join(directory, 'public-path-sentinel'), HOME: join(directory, 'home'), TMPDIR: join(directory, 'tmp'),
      BUN_INSTALL_CACHE_DIR: join(directory, 'bun-cache'), LANG: 'C', LC_ALL: 'C',
      PUBLIC_NON_PATH_SENTINEL: 'SM59_PUBLIC_NON_PATH', PUBLIC_DYNAMIC_SENTINEL: 'SM59_PUBLIC_DYNAMIC',
    });
    const negativeChildSource = (control: GuardControl, caseId: string, moduleURL: string): string => {
      const install = control.startsWith('env')
        ? "Object.defineProperty(process, 'env', { configurable: true, get: refuse });"
        : control === 'fsRead'
          ? 'fs.default.readFileSync = refuse; syncBuiltinESMExports();'
          : 'cp.default.spawn = refuse; syncBuiltinESMExports();';
      return `
import * as fs from 'node:fs';
import * as cp from 'node:child_process';
import { syncBuiltinESMExports } from 'node:module';
await import('node:util');
const writeOutput = process.stdout.write.bind(process.stdout);
const stringify = JSON.stringify;
const state = { moduleBodyWitness: 0, operationAttempt: 0, guardHit: 0 };
Object.defineProperty(globalThis, '__sm59GuardState', { configurable: false, enumerable: false, value: state });
const refuse = () => { state.guardHit += 1; throw Error('IMPORT_SIDE_EFFECT'); };
let setup = 'setup_error';
let outcome = 'setup_error';
let errorCode = 'SETUP_ERROR';
try {
  ${install}
  setup = 'installed';
} catch {}
if (setup === 'installed') {
  try {
    const namespace = await import(${JSON.stringify(moduleURL)});
    if (namespace.sm59Fixture === 'SM59_FILE_MODULE' && state.moduleBodyWitness === 1 && state.operationAttempt === 1 && state.guardHit === 0) {
      outcome = 'unexpected_return';
      errorCode = 'NONE';
    }
  } catch (error) {
    if (error instanceof Error && error.message === 'IMPORT_SIDE_EFFECT'
      && state.moduleBodyWitness === 1 && state.operationAttempt === 1 && state.guardHit === 1) {
      outcome = 'guard_error';
      errorCode = 'IMPORT_SIDE_EFFECT';
    }
  }
}
const projection = {
  candidate: 'process-env-accessor',
  runtimeKind: process.versions?.bun ? 'bun' : 'other',
  setup,
  cases: { ${JSON.stringify(caseId)}: { moduleBodyWitness: state.moduleBodyWitness, operationAttempt: state.operationAttempt, guardHit: state.guardHit, outcome, errorCode } },
};
writeOutput(stringify(projection));
if (projection.runtimeKind !== 'bun' || setup !== 'installed' || outcome !== 'guard_error' || errorCode !== 'IMPORT_SIDE_EFFECT'
  || state.moduleBodyWitness !== 1 || state.operationAttempt !== 1 || state.guardHit !== 1) process.exitCode = 1;
`;
    };
    const positiveChildSource = (androidURL: string, resultURL: string): string => `
import * as fs from 'node:fs';
import * as cp from 'node:child_process';
import { syncBuiltinESMExports } from 'node:module';
await import('node:util');
const writeOutput = process.stdout.write.bind(process.stdout);
const stringify = JSON.stringify;
const state = {
  runtimeKind: process.versions?.bun ? 'bun' : 'other', setup: 'setup_error', wrapperStartWitness: 0,
  beforeImportCheckpoint: 0, afterImportCheckpoint: 0, helperImportCompleteWitness: 0, guardHit: 0,
  exportWitness: {
    androidRecorder: false, androidValidate: false, androidSummary: false, androidImpact: false, androidRecordLimit: false, androidByteLimit: false,
    resultLedger: false, resultProductStatus: false, resultEnvironmentStatus: false, resultRequiredExecutionPassed: false, resultQualified: false, resultValidateIdentity: false,
  },
};
state.wrapperStartWitness = 1;
const refuse = () => { state.guardHit += 1; throw Error('IMPORT_SIDE_EFFECT'); };
try {
  Object.defineProperty(process, 'env', { configurable: true, get: refuse });
  fs.default.readFileSync = refuse;
  fs.default.writeFileSync = refuse;
  cp.default.spawn = refuse;
  cp.default.execFileSync = refuse;
  syncBuiltinESMExports();
  state.setup = 'installed';
  state.beforeImportCheckpoint = 1;
  const androidProduct = await import(${JSON.stringify(androidURL)});
  const mobileResult = await import(${JSON.stringify(resultURL)});
  state.exportWitness = {
    androidRecorder: typeof androidProduct.AndroidProductRecorder === 'function',
    androidValidate: typeof androidProduct.validateProductObservations === 'function',
    androidSummary: typeof androidProduct.publicMobileSummary === 'function',
    androidImpact: typeof androidProduct.androidProductImpact === 'function',
    androidRecordLimit: androidProduct.PRODUCT_RECORD_LIMIT === 1000,
    androidByteLimit: androidProduct.PRODUCT_BYTE_LIMIT === 8 * 1024 * 1024,
    resultLedger: typeof mobileResult.FailureLedger === 'function',
    resultProductStatus: typeof mobileResult.productStatus === 'function',
    resultEnvironmentStatus: typeof mobileResult.environmentStatus === 'function',
    resultRequiredExecutionPassed: typeof mobileResult.requiredExecutionPassed === 'function',
    resultQualified: typeof mobileResult.qualified === 'function',
    resultValidateIdentity: typeof mobileResult.validateMeasurementIdentity === 'function',
  };
  state.helperImportCompleteWitness = 1;
  state.afterImportCheckpoint = 1;
} catch {}
writeOutput(stringify(state));
if (state.runtimeKind !== 'bun' || state.setup !== 'installed' || state.wrapperStartWitness !== 1 || state.beforeImportCheckpoint !== 1
  || state.afterImportCheckpoint !== 1 || state.helperImportCompleteWitness !== 1 || state.guardHit !== 0
  || Object.values(state.exportWitness).some(value => !value)) process.exitCode = 1;
`;
    const runFileModule = async (control: GuardControl): Promise<void> => {
      const directory = await mkdtemp(join(tmpdir(), 'sm59-file-module-'));
      try {
        for (const name of ['public-path-sentinel', 'home', 'tmp', 'bun-cache']) mkdirSync(join(directory, name), { recursive: true, mode: 0o700 });
        const fixture = join(directory, 'owned-fixture.txt'), modulePath = join(directory, 'public.mjs');
        await writeFile(fixture, 'public synthetic guard fixture\n', { flag: 'wx', mode: 0o600 });
        await writeFile(modulePath, fileModuleSource(control, fixture), { flag: 'wx', mode: 0o600 });
        const caseId = `process-env-accessor/${control}/file-module`;
        const child = spawnSync(process.execPath, ['-e', negativeChildSource(control, caseId, pathToFileURL(modulePath).href)], {
          cwd: repositoryPath('.'), env: childEnvironment(directory), encoding: 'utf8', timeout: 10_000, maxBuffer: 64 * 1024,
        });
        assert.equal(child.error, undefined, `${caseId} child spawn`);
        assert.equal(child.status, 0, `${caseId} child status`);
        assert.equal(child.signal, null, `${caseId} child signal`);
        assert.equal(child.stderr, '', `${caseId} child stderr`);
        const stdout = typeof child.stdout === 'string' ? child.stdout : '';
        const projection = JSON.parse(stdout) as { candidate: string; runtimeKind: string; setup: string; cases: Record<string, Record<string, unknown>> };
        assert.deepEqual(Object.keys(projection).sort(), ['candidate', 'cases', 'runtimeKind', 'setup']);
        assert.equal(projection.candidate, 'process-env-accessor');
        assert.equal(projection.runtimeKind, 'bun');
        assert.equal(projection.setup, 'installed');
        assert.deepEqual(Object.keys(projection.cases), [caseId]);
        assert.deepEqual(projection.cases[caseId], { moduleBodyWitness: 1, operationAttempt: 1, guardHit: 1, outcome: 'guard_error', errorCode: 'IMPORT_SIDE_EFFECT' });
      } finally {
        await rm(directory, { recursive: true, force: true });
      }
    };
    for (const control of controlNames) await runFileModule(control);
    const positiveDirectory = await mkdtemp(join(tmpdir(), 'sm59-positive-module-'));
    try {
      for (const name of ['public-path-sentinel', 'home', 'tmp', 'bun-cache']) mkdirSync(join(positiveDirectory, name), { recursive: true, mode: 0o700 });
      const child = spawnSync(process.execPath, ['-e', positiveChildSource(
        pathToFileURL(repositoryPath('tests/mobile/support/android-product.ts')).href,
        pathToFileURL(repositoryPath('tests/mobile/support/mobile-result.ts')).href,
      )], { cwd: repositoryPath('.'), env: childEnvironment(positiveDirectory), encoding: 'utf8', timeout: 10_000, maxBuffer: 64 * 1024 });
      assert.equal(child.error, undefined, 'positive helper child spawn');
      assert.equal(child.status, 0, 'positive helper child status');
      assert.equal(child.signal, null, 'positive helper child signal');
      assert.equal(child.stderr, '', 'positive helper child stderr');
      const stdout = typeof child.stdout === 'string' ? child.stdout : '';
      const projection = JSON.parse(stdout) as Record<string, unknown>;
      assert.deepEqual(Object.keys(projection).sort(), ['afterImportCheckpoint', 'beforeImportCheckpoint', 'exportWitness', 'guardHit', 'helperImportCompleteWitness', 'runtimeKind', 'setup', 'wrapperStartWitness']);
      assert.equal(projection.runtimeKind, 'bun');
      assert.equal(projection.setup, 'installed');
      assert.equal(projection.wrapperStartWitness, 1);
      assert.equal(projection.beforeImportCheckpoint, 1);
      assert.equal(projection.afterImportCheckpoint, 1);
      assert.equal(projection.helperImportCompleteWitness, 1);
      assert.equal(projection.guardHit, 0);
      assert.deepEqual(projection.exportWitness, {
        androidRecorder: true, androidValidate: true, androidSummary: true, androidImpact: true, androidRecordLimit: true, androidByteLimit: true,
        resultLedger: true, resultProductStatus: true, resultEnvironmentStatus: true, resultRequiredExecutionPassed: true, resultQualified: true, resultValidateIdentity: true,
      });
    } finally {
      await rm(positiveDirectory, { recursive: true, force: true });
    }
    for (const file of ['support/mobile-result.ts', 'support/android-product.ts', 'support/evidence.ts']) assert.doesNotMatch(await readFile(repositoryPath(`tests/mobile/${file}`), 'utf8'), /(?:from|import\()\s*['"][^'"]*(?:unit\/|gate\.ts)/u);
  }],
  ['Android product/environment SM59 06 sticky inspection failure', async () => {
    const retained = new AndroidProductRecorder(), actualOwner = {};
    retained.owner({}); retained.activate(measurementIdentity(), actualOwner, observationsFixture().initialNative);
    retained.checkpoint('paired', actualOwner);
    assert.equal(retained.snapshot()!.initialOwner, 2); assert.ok(retained.snapshot()!.records.every(record => record.owner === 2));
    for (const failure of ['OWNERSHIP_LOSS', 'INSPECTION_FAILURE', 'RECEIPT_LIMIT'] as const) {
      const recorder = new AndroidProductRecorder(), owner = {}, identity = measurementIdentity();
      recorder.owner(owner); recorder.activate(identity, owner, observationsFixture().initialNative); recorder.fail(failure); recorder.fail('OWNERSHIP_LOSS');
      assert.throws(() => recorder.checkpoint('paired', owner), /already failed/u);
      assert.equal(recorder.snapshot()!.failure!.category, failure);
      assert.equal(androidProductImpact(assessAndroidEnvironment(environmentFixture(), identity), recorder.snapshot()).status, 'FAIL');
    }
    for (const mode of ['missing-checkpoint', 'open-operation', 'overlap', 'limit', 'empty']) {
      const value = observationsFixture();
      if (mode === 'missing-checkpoint') value.records.find(record => record.name === 'candidate')!.name = 'other';
      if (mode === 'open-operation') value.records.splice(value.records.findIndex(record => record.kind === 'operation-end'), 1);
      if (mode === 'overlap') { const index = value.records.findIndex(record => record.kind === 'operation-begin'); value.records[index + 1] = { ...value.records[index], ordinal: index + 2 }; }
      if (mode === 'limit') value.records = Array.from({ length: 1001 }, (_, index) => ({ ...value.records[0], ordinal: index + 1 }));
      if (mode === 'empty') value.records = [];
      assert.throws(() => validateProductObservations(value, measurementIdentity()), /coverage/u, mode);
    }
    const recorder = new AndroidProductRecorder(), owner = {};
    recorder.activate(measurementIdentity(), owner, observationsFixture().initialNative);
    for (let index = 1; index < 1000; index++) recorder.checkpoint('paired', owner);
    assert.throws(() => recorder.checkpoint('paired', owner), /bound exceeded/u);
    assert.equal(recorder.snapshot()!.records.length, 1000);
    assert.equal(recorder.snapshot()!.failure!.category, 'RECEIPT_LIMIT');
    const oversized = new AndroidProductRecorder(); oversized.activate(measurementIdentity(), owner, observationsFixture().initialNative);
    assert.throws(() => oversized.checkpoint('x'.repeat(8 * 1024 * 1024), owner), /bound exceeded/u);
    assert.equal(oversized.snapshot()!.failure!.category, 'RECEIPT_LIMIT');
    const ledger = new FailureLedger(), original = new Error('firstFatal'); ledger.retain(original, 'inspection', 'OBSERVATION'); ledger.retain(new Error('cleanup'), 'cleanup', 'FINALIZATION'); ledger.retain(original, 'late200', 'PRODUCT');
    assert.equal(ledger.firstError, original); assert.equal(ledger.snapshot().additional_failures.length, 1);
  }],
  ['Android product/environment SM59 07 collection integrity', async () => {
    const rawLog = marker(0, 'START') + '1789100401.000 2000 2000 I PublicFixture: \uFFFD https://fixture.test/?token=PRIVATE_SENTINEL\n' + marker(9, 'END');
    const input = environmentFixture(); input.log = redactText(rawLog); input.collector = JSON.stringify({ bytes: Buffer.byteLength(input.log) });
    assert.notEqual(rawLog, input.log);
    const fixture = await evidenceFixture('android', input);
    const logPath = join(fixture.directory, environmentLeaves.log), checkPath = join(fixture.directory, 'android-environment-check.json');
    const persisted = await readFile(logPath), original = await readFile(checkPath);
    const recomputed = assessAndroidEnvironment(await readEnvironmentInputs(fixture.directory), fixture.identity);
    assert.equal(recomputed.inputs.log.sha256, hash(persisted)); assert.equal(recomputed.inputs.log.bytes, persisted.length);
    await validateMobileEvidence(fixture.options); assert.equal((await environmentCheckCLI(fixture)).status, 0);
    const preSanitization = JSON.parse(original.toString()); preSanitization.inputs.log.sha256 = hash(rawLog); preSanitization.inputs.log.bytes = Buffer.byteLength(rawLog);
    await writeFile(checkPath, JSON.stringify(preSanitization));
    const environment = fixture.result.environment_qualification; assert.equal(environment.platform, 'android');
    if (environment.platform !== 'android') throw new Error('fixture platform');
    environment.assessmentSha256 = hash(await readFile(checkPath)); await fixture.persist();
    await assert.rejects(validateMobileEvidence(fixture.options)); assert.equal((await environmentCheckCLI(fixture)).status, 1);
    assert.equal(await readFile(checkPath, 'utf8'), JSON.stringify(preSanitization));
    await writeFile(checkPath, original);
    environment.assessmentSha256 = hash(original); await fixture.persist();
    const replacement = persisted.indexOf(Buffer.from('\uFFFD'));
    assert.ok(replacement >= 0);
    await writeFile(logPath, Buffer.concat([persisted.subarray(0, replacement), Buffer.from([0xff]), persisted.subarray(replacement + 3)]));
    assert.equal((await readEnvironmentInputs(fixture.directory)).log, '');
    await assert.rejects(validateMobileEvidence(fixture.options)); assert.equal((await environmentCheckCLI(fixture)).status, 1);
    assert.deepEqual(await readFile(checkPath), original);
    await writeFile(logPath, Buffer.concat([persisted, Buffer.from('1789100408.000 2000 2000 I PublicFixture: extra\n')]));
    await assert.rejects(validateMobileEvidence(fixture.options)); assert.equal((await environmentCheckCLI(fixture)).status, 1);
    assert.deepEqual(await readFile(checkPath), original);

    for (const mode of ['collector-exit', 'collector-stderr', 'empty', 'truncated', 'malformed', 'dropped', 'missing-start', 'duplicate-end', 'out-of-order', 'identity', 'missing-snapshot', 'bad-operations']) {
      const input = environmentFixture();
      if (mode.startsWith('collector')) input.collector = JSON.stringify({ bytes: 10, failure: mode });
      if (mode === 'empty') input.log = '';
      if (mode === 'truncated') input.log = input.log.trimEnd();
      if (mode === 'malformed') input.log += 'bad record\n';
      if (mode === 'dropped') input.log += 'logcat: dropped 1\n';
      if (mode === 'missing-start') input.log = marker(9, 'END');
      if (mode === 'duplicate-end') input.log += marker(9, 'END');
      if (mode === 'out-of-order') input.log = marker(9, 'END') + marker(0, 'START');
      if (mode === 'identity') input.before = input.before.replace('android-test', 'another-measurement');
      if (mode === 'missing-snapshot') input.before = '';
      if (mode === 'bad-operations') input.operations = '{}';
      const check = assessAndroidEnvironment(input, measurementIdentity());
      assert.equal(check.status, 'UNKNOWN', mode); assert.equal(check.collection.status, 'UNKNOWN', mode); assert.equal(check.eventCounts.fatalEvents, 0, mode);
      assert.equal(androidProductImpact(check, observationsFixture()).status, 'FAIL', mode);
      const withoutAdverse = resultContract(); withoutAdverse.environment_qualification = { platform: 'android', status: check.status, collection: check.collection.status, assessmentSha256: '', measurementId: withoutAdverse.identity.measurementId };
      assert.equal(qualified(withoutAdverse), false, mode);
      const adverseLine = '1789100405.000 546 546 I ActivityManager: Killing 5820:com.android.chrome:sandboxed_process0/u0a146 (adj 0): signal death\n';
      const endMarker = marker(9, 'END').trimEnd();
      const endIndex = input.log.lastIndexOf(endMarker);
      if (endIndex < 0) input.log += `${input.log && !input.log.endsWith('\n') ? '\n' : ''}${adverseLine}`;
      else {
        const prefix = input.log.slice(0, endIndex), suffix = input.log.slice(endIndex + endMarker.length);
        input.log = `${prefix}${prefix && !prefix.endsWith('\n') ? '\n' : ''}${adverseLine}${endMarker}${suffix}`;
      }
      const adverse = assessAndroidEnvironment(input, measurementIdentity());
      assert.equal(adverse.status, 'FAIL', mode); assert.equal(adverse.collection.status, 'UNKNOWN', mode); assert.ok(adverse.eventCounts.fatalEvents > 0, mode);
      assert.equal(androidProductImpact(adverse, observationsFixture()).status, 'FAIL', mode);
      const withAdverse = resultContract(); withAdverse.environment_qualification = { platform: 'android', status: adverse.status, collection: adverse.collection.status, assessmentSha256: '', measurementId: withAdverse.identity.measurementId };
      assert.equal(qualified(withAdverse), false, mode);
    }
    const directory = await mkdtemp(join(tmpdir(), 'sm59-input-bounds-')), valid = environmentFixture();
    for (const [key, leaf] of Object.entries(environmentLeaves)) await writeFile(join(directory, leaf), valid[key as keyof EnvironmentInputBytes]);
    for (const key of ['before', 'after', 'log', 'operations', 'collector'] as const) {
      const path = join(directory, environmentLeaves[key]);
      await truncate(path, (key === 'log' ? 104_857_600 : key === 'operations' ? 64_000 : 8_000_000) + 1);
      const input = await readEnvironmentInputs(directory);
      assert.equal(input[key], ''); assert.equal(assessAndroidEnvironment(input, measurementIdentity()).collection.status, 'UNKNOWN');
      await writeFile(path, valid[key]);
    }
  }],
  ['Android product/environment SM59 08 finalization ordering', async () => {
    for (const failure of measurementFailureCases) await measurementControl(failure);
    const result = resultContract(); result.environment_qualification = { platform: 'android', status: 'FAIL', collection: 'PASS', measurementId: result.identity.measurementId, assessmentSha256: 'a'.repeat(64) };
    assert.equal(requiredExecutionPassed(result), true); assert.equal(qualified(result), false);
    result.finalization.status = 'FAIL'; assert.equal(requiredExecutionPassed(result), false);
  }],
  ['Android product/environment SM59 09 evidence contracts and iOS applicability', async () => {
    for (const platform of ['android', 'ios'] as const) {
      const fixture = await evidenceFixture(platform); await validateMobileEvidence(fixture.options);
      const report = await collectEnvironmentReport(fixture.options); assert.equal(report.environment, platform === 'ios' ? 'NOT_APPLICABLE' : 'PASS'); assert.equal(report.incomplete, false);
      const original = structuredClone(fixture.result);
      for (const mode of ['schema', 'status', 'fatal', 'unusable', 'finalization', 'attempt', 'head', 'candidate', 'missing-row']) {
        Object.assign(fixture.result, structuredClone(original));
        if (mode === 'schema') (fixture.result as any).schema = 1;
        if (mode === 'status') (fixture.result.product as any).status = 'UNKNOWN';
        if (mode === 'fatal') (fixture.result.evidence as any).driver.firstFatal = { code: 'TIMEOUT' };
        if (mode === 'unusable') (fixture.result.evidence as any).driver.unusable = true;
        if (mode === 'finalization') fixture.result.finalization.status = 'FAIL';
        if (mode === 'attempt') fixture.result.identity.attempt = '1';
        if (mode === 'head') fixture.result.identity.sourceRunHeadSha = '0'.repeat(40);
        if (mode === 'candidate') fixture.result.identity.candidateBuild = '0'.repeat(64);
        await fixture.persist();
        if (mode === 'missing-row') await rm(join(fixture.directory, 'mobile-result.json'));
        await assert.rejects(validateMobileEvidence(fixture.options), mode);
      }
    }
    const input = environmentFixture(); changePackage(input, value => { value.dependencyConfig.enabledComponents = ['com.google.android.gms.Visible']; });
    const fixture = await evidenceFixture('android', input);
    const report = await collectEnvironmentReport({ ...fixture.options, matrix: [...fixture.options.matrix, { platform: 'ios', baseline: '0.20.10', scenario: 'historical' }] });
    assert.equal(report.incomplete, true); assert.equal(report.environment, 'FAIL'); assert.equal(report.rows.length, 2); assert.notEqual(report.product, 'PASS');
    await writeFile(join(fixture.directory, 'android-environment-log.json'), 'irrelevant public leaf');
    for (const leaf of ['android-environment-check.json', 'android-appium-integrity.json', 'android-environment-identity.json', 'android-environment-completion.json']) {
      const path = join(fixture.directory, leaf), bytes = await readFile(path);
      await rm(path); await assert.rejects(validateMobileEvidence(fixture.options), leaf);
      assert.equal((await evidenceCLI(fixture, true)).status, 1, leaf);
      await writeFile(path, '{'); await assert.rejects(validateMobileEvidence(fixture.options), leaf);
      assert.equal((await evidenceCLI(fixture, true)).status, 1, leaf);
      await rm(path); await symlink(join(fixture.directory, 'mobile-result.json'), path); await assert.rejects(validateMobileEvidence(fixture.options), leaf);
      assert.equal((await evidenceCLI(fixture, true)).status, 1, leaf);
      await rm(path); await writeFile(path, bytes);
    }
    const observed = await evidenceFixture();
    observed.result.product = { status: 'FAIL', categories: ['REQUIRED_SCENARIO_FAILURE'] };
    Object.assign(observed.result, { primary_failure: { category: 'PRODUCT', stage: 'upgrade', message: 'recorded preference failure' } });
    await observed.persist();
    const negativeReport = await evidenceCLI(observed, true);
    assert.equal(negativeReport.status, 0);
    assert.equal(JSON.parse(negativeReport.stdout).product, 'FAIL');
    assert.equal(JSON.parse(negativeReport.stdout).environment, 'PASS');
    assert.equal(JSON.parse(negativeReport.stdout).integrity, 'COMPLETE');
    assert.equal((await evidenceCLI(observed)).status, 1);
    for (const key of ['productObservations', 'driver', 'teardown']) {
      const originalEvidence = structuredClone(fixture.result.evidence);
      delete (fixture.result.evidence as any)[key]; await fixture.persist();
      await assert.rejects(validateMobileEvidence(fixture.options), key);
      const rejected = await evidenceCLI(fixture, true); assert.equal(rejected.status, 1, key);
      assert.equal(JSON.parse(rejected.stdout).integrity, 'INCOMPLETE');
      fixture.result.evidence = originalEvidence; await fixture.persist();
    }
    for (const snapshot of ['driver', 'teardown']) for (const field of ['all', 'unusable', 'sessionId', 'selectedContext', 'selectedWindow', 'commands', 'lookups', 'firstFatal']) {
      const originalEvidence = structuredClone(fixture.result.evidence);
      const target = snapshot === 'driver' ? fixture.result.evidence as any : (fixture.result.evidence as any).teardown;
      if (field === 'all') target.driver = {};
      else delete target.driver[field];
      await fixture.persist(); await assert.rejects(validateMobileEvidence(fixture.options)); assert.equal((await evidenceCLI(fixture, true)).status, 1);
      fixture.result.evidence = originalEvidence; await fixture.persist();
    }
    const original = await readFile(join(fixture.directory, 'android-environment-check.json'));
    const altered = JSON.parse(original.toString()); altered.eventCounts.fatalEvents++;
    await writeFile(join(fixture.directory, 'android-environment-check.json'), JSON.stringify(altered));
    await assert.rejects(validateMobileEvidence(fixture.options), /immutable/u);
  }],
  ['Android product/environment SM59 11 public projection', async () => {
    const sentinel = 'PRIVATE_SENTINEL', input = environmentFixture();
    changePackage(input, value => { value.dependencyConfig.enabledComponents = Array.from({ length: 200 }, (_, index) => `com.google.android.gms.Public${String(index).padStart(3, '0')}`); });
    const check = assessAndroidEnvironment(input, measurementIdentity());
    check.nativeEvents.push({ kind: 'process-death', line: `1789100405.000 77 77 I Zygote: Process 5820 exited due to signal 9 (${sentinel})`, pid: '5820', processName: sentinel, reason: `https://private/#${sentinel}` });
    check.findings.push(...Array.from({ length: 80 }, () => ({ category: 'ADVERSE_DEATH' as const, packageName: sentinel })));
    const result = resultContract(); const projection = publicMobileSummary(result, check), bytes = JSON.stringify(projection);
    assert.doesNotMatch(bytes, /PRIVATE_SENTINEL|https:\/\/private/u); assert.ok(Buffer.byteLength(bytes) <= 65_536);
    assert.equal((projection.components as any[])[0].after.total, 200); assert.equal((projection.components as any[])[0].after.withheldCount, 72);
    assert.equal((projection.events as any[])[0].signal, 9); assert.equal((projection.events as any[])[0].pid, '5820'); assert.ok(Number(projection.omittedFindings) > 0);
    assert.equal(androidProductImpact(check, observationsFixture()).status, 'INDETERMINATE');
    const matrix = { product: 'PASS' as const, environment: 'FAIL' as const, incomplete: false, rows: Array.from({ length: 8 }, () => structuredClone(projection)) };
    const bounded = serializeEnvironmentReport(matrix);
    assert.ok(Buffer.byteLength(bounded) <= 65_536); assert.doesNotMatch(bounded, /PRIVATE_SENTINEL/u);
    assert.equal(JSON.parse(bounded).rows[0].components[0].after.total, 200);
    assert.throws(() => publicMobileSummary({ ...result, identity: { ...result.identity, baseline: sentinel + '/private' } }, check), /identity/u);
    const trichromeInput = environmentFixture();
    changePackage(trichromeInput, value => { value.dependencyConfig.enabledComponents = ['com.google.android.trichromelibrary.Visible', 'private.hidden.Component']; }, 'com.google.android.trichromelibrary');
    const trichrome = publicMobileSummary(resultContract(), assessAndroidEnvironment(trichromeInput, measurementIdentity()));
    assert.equal((trichrome.components as any[])[0].packageName, 'com.google.android.trichromelibrary');
    assert.deepEqual((trichrome.components as any[])[0].after, { total: 2, names: ['com.google.android.trichromelibrary.Visible'], withheldCount: 1 });
    assert.equal(trichrome.componentDeltaCount, 1); assert.equal(trichrome.omittedComponentDeltas, 0);
    const fixture = await evidenceFixture();
    const privateText = 'PRIVATE_SENTINEL/owner/session/https://private/#token\u001b[31m\n' + 'X'.repeat(4000);
    fixture.result.faults_exercised[0] = privateText; await fixture.persist();
    for (const report of [false, true]) {
      const rejected = await evidenceCLI(fixture, report);
      assert.equal(rejected.status, 1);
      for (const surface of rejected.surfaces) { assert.doesNotMatch(surface, /PRIVATE_SENTINEL|https:\/\/private/u); assert.equal(surface.includes(String.fromCharCode(0x1b)), false); assert.ok(Buffer.byteLength(surface) <= 66_000); }
      const missing = await evidenceCLI(fixture, report, join(fixture.directory, 'PRIVATE_SENTINEL-artifact'));
      assert.equal(missing.status, 1);
      for (const surface of missing.surfaces) { assert.doesNotMatch(surface, /PRIVATE_SENTINEL|https:\/\/private/u); assert.equal(surface.includes(String.fromCharCode(0x1b)), false); }
    }

  }],
];
