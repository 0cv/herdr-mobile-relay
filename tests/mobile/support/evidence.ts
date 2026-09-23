import { readdir, readFile, lstat } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { isDeepStrictEqual } from 'node:util';
import { dirname, join, posix } from 'node:path';
import { assessAndroidEnvironment, readEnvironmentInputs, sameAssessment, type AndroidEnvironmentCheck } from '../android-environment';
import { androidProductImpact, validateProductObservations, type ProductObservations } from './android-product';
import { validateMeasurementIdentity, type MobileResultContract } from './mobile-result';
import { isAndroidPersistentWebAppActivity, runtimeIdentityMismatch } from './oracle';

export interface EvidenceMatrixEntry {
  platform: string;
  baseline: string;
  scenario: string;
}

export interface EvidenceBundleIdentity {
  version: string;
  assets: number;
  build: string;
  entry: string;
  script: string;
  style: string;
  webHash: string;
  descriptor?: boolean;
}

export interface EvidenceExpectedBundle {
  name: string;
  identity: EvidenceBundleIdentity;
}

export interface EvidenceValidationOptions {
  contract: 'product' | 'qualified';
  runId: string;
  attempt: string;
  directory: string;
  matrix: EvidenceMatrixEntry[];
  suite: string;
  candidateCommit: string;
  sourceRunHeadSha: string;
  candidateWebHash: string;
  candidateIdentity: EvidenceBundleIdentity;
  baselineIdentities: EvidenceExpectedBundle[];
  syntheticWebHash?: string;
  syntheticCandidateIdentity?: EvidenceBundleIdentity;
  syntheticBaselineIdentity?: EvidenceBundleIdentity;
}

interface RuntimeIdentity {
  standalone?: unknown;
  provider?: unknown;
  nativeProvider?: unknown;
  nativeActivity?: unknown;
  nativePid?: unknown;
  url?: unknown;
  origin?: unknown;
  version?: unknown;
  assets?: unknown;
  build?: unknown;
  buildFromApplication?: unknown;
  entry?: unknown;
  script?: unknown;
  style?: unknown;
  requiredAssetsReady?: unknown;
  applicationInitialized?: unknown;
}

interface FixtureRequest {
  release?: unknown;
  path?: unknown;
  fault?: unknown;
  fault_id?: unknown;
  fault_generation?: unknown;
}

function fail(message: string): never {
  throw new Error(`MOBILE_EVIDENCE: ${message}`);
}

function record(value: unknown, label: string): Record<string, unknown> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) fail(`${label} must be an object`);
  return value as Record<string, unknown>;
}

function stringValue(value: unknown, label: string): string {
  if (typeof value !== 'string' || value.length === 0) fail(`${label} must be a non-empty string`);
  return value;
}

function booleanValue(value: unknown, label: string): boolean {
  if (typeof value !== 'boolean') fail(`${label} must be a boolean`);
  return value;
}

function stringArray(value: unknown, label: string): string[] {
  if (!Array.isArray(value) || value.length === 0 || value.some((item) => typeof item !== 'string' || item.length === 0)) {
    fail(`${label} must be a non-empty string array`);
  }
  return value as string[];
}

function integerAtLeast(value: unknown, minimum: number, label: string): number {
  if (!Number.isInteger(value) || Number(value) < minimum) fail(`${label} must be an integer >= ${minimum}`);
  return Number(value);
}

async function resultFiles(root: string, depth = 0, count = { files: 0 }): Promise<string[]> {
  const metadata = await lstat(root);
  if (!metadata.isDirectory() || metadata.isSymbolicLink() || depth > 4) fail('evidence directory is invalid');
  const initialCount = count.files;
  const entries = await readdir(root, { withFileTypes: true });
  if (entries.length > 128) fail('evidence directory exceeds file bound');
  const files: string[] = [];
  for (const entry of entries) {
    if (++count.files > 8 * 128 || entry.isSymbolicLink()) fail('evidence inventory exceeds bound or contains a symlink');
    const filename = join(root, entry.name);
    if (entry.isDirectory()) files.push(...await resultFiles(filename, depth + 1, count));
    else if (entry.isFile() && entry.name === 'mobile-result.json') files.push(filename);
    else if (!entry.isFile()) fail('evidence contains a nonregular leaf');
  }
  if (entries.some(entry => entry.name === 'mobile-result.json') && count.files - initialCount > 128) fail('evidence row exceeds file bound');
  return files;
}

export async function boundedEvidenceFile(path: string, maximum: number): Promise<Buffer> {
  const metadata = await lstat(path);
  if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size > maximum || !metadata.size) fail('required evidence leaf invalid');
  const bytes = await readFile(path);
  if (bytes.length !== metadata.size || bytes.length > maximum) fail('required evidence leaf changed during read');
  return bytes;
}
const sha256 = (bytes: Buffer | string) => createHash('sha256').update(bytes).digest('hex');

export async function validateAndroidProducer(path: string): Promise<void> {
  const data = JSON.parse((await boundedEvidenceFile(path, 8_000_000)).toString());
  const source = async (leaf: string) => {
    if (!/^[a-zA-Z0-9.-]+$/u.test(leaf)) fail('invalid producer manifest source');
    return readFile(new URL(`../android-appium/${leaf}`, import.meta.url));
  };
  const manifest = JSON.parse((await source('producer-manifest.json')).toString());
  const packages = { appium: JSON.parse((await source('appium-runtime-manifest.json')).toString()), ...JSON.parse((await source('runtime-manifest.json')).toString()) };
  if (data.node !== '24.21.0' || typeof data.home !== 'string' || !data.home.startsWith('/') || data.home.includes('..')) fail('producer runtime identity mismatch');
  if (!isDeepStrictEqual(data.inputs, manifest.inputs)) fail('producer input set mismatch');
  for (const [leaf, hash] of Object.entries(manifest.inputs)) if (sha256(await source(leaf)) !== hash) fail('producer source input hash mismatch');
  if (!isDeepStrictEqual(Object.keys(data.authored || {}).sort(), Object.keys(manifest.authored).sort())) fail('producer authored set mismatch');
  for (const [file, expected] of Object.entries(manifest.authored) as Array<[string, { source: string; sha256: string }]>) {
    if (sha256(await source(expected.source)) !== expected.sha256 || data.authored[file] !== expected.sha256) fail('producer authored hash mismatch');
  }
  const entries: Record<string, string> = {};
  for (const [id, expected] of Object.entries(packages) as Array<[string, any]>) {
    const actual = data[id];
    const root = posix.join(data.home, expected.root);
    const entry = new URL(`file://${posix.join(root, expected.entry)}`).href;
    const parent = expected.parent ? entries[expected.parent] : new URL(`file://${data.home}/package.json`).href;
    if (!actual || actual.entry !== entry || actual.parent !== parent || actual.realpath !== root || actual.condition !== expected.condition
      || actual.version !== expected.version || !isDeepStrictEqual(actual.engines || {}, expected.engines || {})) fail('producer selected resolver identity mismatch');
    const hashes = Object.fromEntries(Object.entries(expected.files).map(([file, original]) => [file, id === 'android' && manifest.files[file] ? manifest.files[file].after : original]));
    if (!isDeepStrictEqual(actual.files, hashes)) fail('producer runtime/source/compiled/declaration hashes mismatch');
    entries[id] = entry;
  }
  const context = new URL('./commands/context/exports.js', entries.android).href;
  const resolvers = { context, helper: new URL('./retained-inspection.cjs', context).href, transport: new URL('./target-inspection.cjs', context).href,
    ws: entries.ws, axiosImporter: new URL('./jsonwp-proxy/proxy-request.js', entries.base).href, axios: entries.axios, axiosVersion: packages.axios.version, absent: ['bufferutil', 'utf-8-validate'] };
  if (!isDeepStrictEqual(data.inspectionResolvers, resolvers)) fail('producer effective inspection resolver mismatch');
  const selected = data.selected?.uiautomator2;
  if (!isDeepStrictEqual(Object.keys(data.selected || {}), ['uiautomator2']) || selected.pkgName !== packages.uiautomator2.name
    || selected.version !== packages.uiautomator2.version || selected.mainClass !== 'AndroidUiautomator2Driver' || selected.automationName !== 'UiAutomator2'
    || selected.installType !== 'npm' || selected.installPath !== posix.join(data.home, packages.uiautomator2.root)) fail('selected native producer mismatch');
}

export function validateDriverReceipt(value: unknown): void {
  const driver = record(value, 'driver');
  if (typeof driver.unusable !== 'boolean' || typeof driver.sessionId !== 'string' || typeof driver.selectedContext !== 'string'
    || typeof driver.selectedWindow !== 'string' || !Array.isArray(driver.commands) || !Array.isArray(driver.lookups)
    || !Object.hasOwn(driver, 'firstFatal') || (driver.firstFatal !== null && (!driver.firstFatal || typeof driver.firstFatal !== 'object'))) fail('required driver state fields missing');
}

export interface EvidenceRow { filename: string; result: Record<string, unknown> & MobileResultContract; assessment?: AndroidEnvironmentCheck }
export async function loadEvidenceRows(options: EvidenceValidationOptions, invalid?: () => void): Promise<EvidenceRow[]> {
  if (!['product', 'qualified'].includes(options.contract) || !/^[1-9]\d*$/u.test(options.runId) || !/^[1-9]\d*$/u.test(options.attempt)) fail('current contract/run/attempt required');
  if (!Array.isArray(options.matrix) || !options.matrix.length || options.matrix.length > 8) fail('expected matrix bound');
  let files: string[];
  try { files = (await resultFiles(options.directory)).sort(); } catch (error) {
    if (!invalid) throw error;
    invalid();
    files = [];
  }
  if (files.length !== options.matrix.length) {
    if (!invalid) fail('expected matrix results are missing or duplicated');
    invalid();
  }
  const expected = new Map(options.matrix.map(entry => [rowKey(entry), entry]));
  if (expected.size !== options.matrix.length) fail('duplicate expected matrix row');
  const rows: EvidenceRow[] = [];
  for (const filename of files) {
    try {
    const result = record(JSON.parse((await boundedEvidenceFile(filename, 100 * 1024 * 1024)).toString()), 'result') as EvidenceRow['result'];
    if (result.schema !== 2 || !['PASS', 'FAIL', 'INDETERMINATE'].includes(result.product?.status)
      || !['PASS', 'FAIL'].includes(result.finalization?.status) || !Array.isArray(result.finalization?.failures)
      || !Array.isArray(result.additional_failures) || !Array.isArray(result.product.categories)
      || result.product.categories.length > 64 || result.product.categories.some(category => typeof category !== 'string' || !/^[A-Z0-9_]{1,80}$/u.test(category))) fail('invalid schema2 result');
    for (const failure of [...result.finalization.failures, ...result.additional_failures, ...(result.primary_failure ? [result.primary_failure] : [])]) {
      if (!failure || !['PRODUCT', 'OBSERVATION', 'COLLECTION', 'FINALIZATION', 'EVIDENCE'].includes(failure.category)
        || typeof failure.stage !== 'string' || typeof failure.message !== 'string') fail('invalid failure receipt');
    }
    validateMeasurementIdentity(result.identity);
    const scenario = result.candidate === 'current-code-target' ? 'synthetic' : 'historical';
    const key = rowKey({ platform: String(result.platform), baseline: String(result.baseline), scenario });
    if (!expected.has(key)) fail('foreign or duplicate matrix row');
    const candidate = scenario === 'synthetic' ? options.syntheticCandidateIdentity : options.candidateIdentity;
    if (!candidate) fail('missing prepared candidate');
    const identity = { runId: options.runId, attempt: options.attempt, suite: options.suite, platform: result.platform, baseline: result.baseline, scenario,
      sourceCommit: options.candidateCommit, sourceRunHeadSha: options.sourceRunHeadSha, candidateWebHash: expectedHash(result, options), candidateBuild: candidate.build, measurementId: result.identity.measurementId };
    if (!isDeepStrictEqual(result.identity, identity) || result.source_commit !== options.candidateCommit || result.source_run_head_sha !== options.sourceRunHeadSha
      || result.candidate_web_hash !== identity.candidateWebHash || result.suite !== options.suite) fail('stale or inconsistent run identity');
    const row: EvidenceRow = { filename, result };
    const evidence = record(result.evidence, 'evidence');
    validateDriverReceipt(evidence.driver);
    validateDriverReceipt(record(evidence.teardown, 'teardown').driver);
    if (result.product.status !== 'PASS' && !result.product.categories.length) fail('observed product failure category missing');
    const environment = result.environment_qualification;
    if (result.platform === 'ios') {
      if (!isDeepStrictEqual(environment, { platform: 'ios', applicability: 'NOT_APPLICABLE' }) || result.identity.measurementId !== 'ios-not-applicable') fail('invalid iOS applicability');
    } else if (result.platform === 'android') {
      const directory = dirname(filename);
      const session = JSON.parse((await boundedEvidenceFile(join(directory, 'android-environment-session.json'), 64_000)).toString());
      const boundIdentity = JSON.parse((await boundedEvidenceFile(join(directory, 'android-environment-identity.json'), 64_000)).toString());
      if (session.id !== result.identity.measurementId || !isDeepStrictEqual(boundIdentity, result.identity)) fail('measurement session identity mismatch');
      const bytes = await boundedEvidenceFile(join(directory, 'android-environment-check.json'), 8_000_000);
      const check = JSON.parse(bytes.toString()) as AndroidEnvironmentCheck;
      row.assessment = assessAndroidEnvironment(await readEnvironmentInputs(directory), result.identity);
      if (!sameAssessment(check, row.assessment) || environment.platform !== 'android' || environment.assessmentSha256 !== sha256(bytes)
        || environment.status !== check.status || environment.collection !== check.collection.status || environment.measurementId !== result.identity.measurementId) fail('immutable environment assessment binding mismatch');
      try {
        await validateAndroidProducer(join(directory, 'android-appium-integrity.json'));
        validateProductObservations(evidence.productObservations as ProductObservations, result.identity, row.assessment.plannedOperations);
        const completion = JSON.parse((await boundedEvidenceFile(join(directory, 'android-environment-completion.json'), 64_000)).toString());
        if (typeof completion.passed !== 'boolean' || !Array.isArray(completion.errors) || completion.errors.some((error: unknown) => typeof error !== 'string')
          || completion.passed !== (completion.errors.length === 0) || completion.assessmentSha256 !== environment.assessmentSha256) fail('completion receipt invalid');
      } catch (error) {
        if (!invalid) throw error;
        invalid();
      }
    } else fail('unknown platform');
    rows.push(row);
    expected.delete(key);
    } catch (error) {
      if (!invalid) throw error;
      invalid();
    }
  }
  if (expected.size) {
    if (!invalid) fail('incomplete matrix');
    invalid();
  }
  return rows;
}

function expectedHash(result: Record<string, unknown>, options: EvidenceValidationOptions): string {
  if (result.candidate === 'current-code-target') {
    if (!options.syntheticWebHash) fail('synthetic web hash is required');
    return options.syntheticWebHash;
  }
  return options.candidateWebHash;
}

function bundleIdentity(value: unknown, label: string): EvidenceBundleIdentity {
  const identity = record(value, label);
  stringValue(identity.version, `${label}.version`);
  integerAtLeast(identity.assets, 1, `${label}.assets`);
  if (typeof identity.build !== 'string') fail(`${label}.build must be a string`);
  const entry = stringValue(identity.entry, `${label}.entry`);
  stringValue(identity.script, `${label}.script`);
  stringValue(identity.style, `${label}.style`);
  const webHash = stringValue(identity.webHash, `${label}.webHash`);
  if (!/^[0-9a-f]{64}$/u.test(webHash)) fail(`${label}.webHash must be a SHA-256 value`);
  if (identity.descriptor !== undefined && typeof identity.descriptor !== 'boolean') fail(`${label}.descriptor must be a boolean`);
  if ((identity.descriptor === true || entry.startsWith('/builds/'))
    && !/^[0-9a-f]{64}$/u.test(identity.build)) fail(`${label}.build must be a SHA-256 value for a descriptor bundle`);
  return identity as unknown as EvidenceBundleIdentity;
}

function verifiedOrigin(value: unknown, label: string): string {
  const origin = stringValue(value, label);
  let parsed: URL;
  try {
    parsed = new URL(origin);
  } catch {
    fail(`${label} is not a valid URL origin`);
  }
  if (parsed.protocol !== 'https:' || parsed.origin !== origin) fail(`${label} is not a verified HTTPS origin`);
  return origin;
}

function assertExpectedBundle(
  identity: RuntimeIdentity,
  expected: EvidenceBundleIdentity,
  label: string,
  requireExecutingBuild: boolean,
): void {
  const mismatch = runtimeIdentityMismatch(identity as any, expected, requireExecutingBuild);
  if (mismatch) fail(`${label}.${mismatch.code}: ${mismatch.detail}`);
}

function assertNativeProvider(identity: RuntimeIdentity, platform: string, label: string): void {
  const nativeProvider = stringValue(identity.nativeProvider, `${label}.nativeProvider`);
  if (platform === 'ios') {
    if (nativeProvider !== 'ios:com.apple.webapp') fail(`${label} has an invalid iOS installed provider`);
    return;
  }
  if (!/^android:(?:com\.android\.chrome|org\.chromium\.webapk(?:\.[A-Za-z0-9_.-]+)?|com\.google\.android\.webapk(?:\.[A-Za-z0-9_.-]+)?)$/u.test(nativeProvider)) {
    fail(`${label} has an invalid Android installed provider`);
  }
  const activity = stringValue(identity.nativeActivity, `${label}.nativeActivity`);
  if (!isAndroidPersistentWebAppActivity(activity)) {
    fail(`${label} has an invalid Android installed activity`);
  }
}

function assertRuntime(
  value: unknown,
  platform: string,
  label: string,
  expectedOrigin: string,
  expectedBundle: EvidenceBundleIdentity,
  requireExecutingBuild: boolean,
): RuntimeIdentity {
  const identity = record(value, label) as RuntimeIdentity;
  if (identity.standalone !== true) fail(`${label} is not an installed standalone runtime`);
  if (identity.provider !== (platform === 'android' ? 'android-standalone' : 'ios-home-screen')) {
    fail(`${label} has an invalid ${platform} provider`);
  }
  assertNativeProvider(identity, platform, label);
  stringValue(identity.nativePid, `${label}.nativePid`);
  const origin = verifiedOrigin(identity.origin, `${label}.origin`);
  if (origin !== expectedOrigin) fail(`${label}.origin does not match the verified fixture origin`);
  const url = stringValue(identity.url, `${label}.url`);
  let parsedUrl: URL;
  try {
    parsedUrl = new URL(url);
  } catch {
    fail(`${label}.url is not a valid URL`);
  }
  if (parsedUrl.origin !== origin) fail(`${label}.url does not belong to its verified origin`);
  stringValue(identity.version, `${label}.version`);
  integerAtLeast(identity.assets, 1, `${label}.assets`);
  if (typeof identity.build !== 'string') fail(`${label}.build must be a string`);
  stringValue(identity.entry, `${label}.entry`);
  stringValue(identity.script, `${label}.script`);
  stringValue(identity.style, `${label}.style`);
  if (identity.buildFromApplication !== undefined && typeof identity.buildFromApplication !== 'boolean') {
    fail(`${label}.buildFromApplication must be a boolean`);
  }
  if (identity.requiredAssetsReady !== true || identity.applicationInitialized !== true) {
    fail(`${label} is missing loaded runtime evidence`);
  }
  assertExpectedBundle(identity, expectedBundle, label, requireExecutingBuild);
  return identity;
}

function assertCredentialEvidence(result: Record<string, unknown>): void {
  if (result.credential_preserved !== true) fail('credential preservation was not established');
  const relays = record(result.credential_evidence, 'credential_evidence').relays;
  const relayMap = record(relays, 'credential_evidence.relays');
  const expectedRelays = ['alpha', 'beta'];
  const actualRelays = Object.keys(relayMap).sort();
  if (JSON.stringify(actualRelays) !== JSON.stringify(expectedRelays)) {
    fail(`credential evidence must contain exactly ${expectedRelays.join(', ')}`);
  }
  for (const name of expectedRelays) {
    const relay = record(relayMap[name], `credential evidence for ${name}`);
    integerAtLeast(relay.invitationAuthCount, 1, `${name}.invitationAuthCount`);
    integerAtLeast(relay.credentialAuthCount, 1, `${name}.credentialAuthCount`);
    const pseudonyms = stringArray(relay.credentialPseudonyms, `${name}.credentialPseudonyms`);
    if (new Set(pseudonyms).size !== pseudonyms.length) fail(`${name}.credentialPseudonyms contains duplicate identities`);
    integerAtLeast(relay.connections, 1, `${name}.connections`);
  }
}

function assertConsumedFault(result: Record<string, unknown>, expectedCandidate: EvidenceBundleIdentity): void {
  const labels = stringArray(result.faults_exercised, 'faults_exercised');
  if (labels.length !== 1) fail('exactly one candidate fault must be exercised');
  const candidate = stringValue(result.candidate, 'candidate');
  const expectedKind = candidate === 'current-code-target' ? 'missing' : 'corrupt';
  const expectedPath = candidate === 'current-code-target' ? expectedCandidate.style : expectedCandidate.script;
  const expectedLabel = `${expectedKind}:${expectedPath}`;
  if (labels[0] !== expectedLabel) fail(`fault identity ${labels[0]} does not match the expected candidate asset ${expectedLabel}`);
  const fault = record(result.fault_identity, 'fault_identity');
  const kind = stringValue(fault.kind, 'fault_identity.kind');
  const path = stringValue(fault.path, 'fault_identity.path');
  const faultId = stringValue(fault.id, 'fault_identity.id');
  const faultGeneration = stringValue(fault.generation, 'fault_identity.generation');
  if (kind !== expectedKind || path !== expectedPath) fail('fault_identity does not match the expected candidate asset');
  const requests = result.fixture_requests;
  if (!Array.isArray(requests)) fail('fault evidence is missing');
  const consumed: string[] = [];
  for (const entry of requests) {
    const request = record(entry, 'fixture request') as FixtureRequest;
    if (request.release !== 'candidate' || request.fault === undefined) continue;
    if (request.fault !== kind || request.path !== path) fail('fixture evidence contains an unrelated candidate fault');
    const requestId = stringValue(request.fault_id, 'fixture request.fault_id');
    const requestGeneration = stringValue(request.fault_generation, 'fixture request.fault_generation');
    consumed.push(`${requestId}\u0000${requestGeneration}`);
  }
  if (!consumed.length) fail(`fault ${labels[0]} has no consumed id and generation`);
  if (new Set(consumed).size !== 1 || consumed[0] !== `${faultId}\u0000${faultGeneration}`) {
    fail(`fault ${labels[0]} was consumed with an unrelated id or generation`);
  }
}

function assertPhoneCompletion(result: Record<string, unknown>, baseline: string): void {
  const completion = record(result.phone_completion, 'phone_completion');
  booleanValue(completion.rawPlanPresent, 'phone_completion.rawPlanPresent');
  booleanValue(completion.phoneRequired, 'phone_completion.phoneRequired');
  booleanValue(completion.phoneAcknowledged, 'phone_completion.phoneAcknowledged');
  if (typeof completion.phoneState !== 'string') fail('phone_completion.phoneState must be a string');
  booleanValue(completion.visibleCompletion, 'phone_completion.visibleCompletion');
  if (completion.phoneRequired) {
    if (completion.rawPlanPresent !== true) fail(`${baseline} is missing raw phone-plan evidence`);
    if (completion.phoneAcknowledged !== true || completion.phoneState !== 'loaded' || completion.visibleCompletion !== true) {
      fail(`${baseline} is missing completed phone acknowledgement`);
    }
    return;
  }
  if (baseline !== '0.20.8' && baseline !== '0.20.9') fail(`${baseline} has no phone plan`);
  if (completion.phoneAcknowledged || completion.visibleCompletion) fail(`${baseline} reported phone completion without a phone plan`);
  const controls = stringArray(result.oracle_controls, 'oracle_controls');
  if (!controls.includes(`HISTORICAL_PHONE_ACCOUNTING_UNAVAILABLE:${baseline}`)) {
    fail(`${baseline} is missing its explicit historical phone-plan control`);
  }
}

function rowKey(entry: EvidenceMatrixEntry): string {
  return `${entry.platform}/${entry.baseline}/${entry.scenario}`;
}

export async function validateMobileEvidence(options: EvidenceValidationOptions): Promise<void> {
  if (!/^[0-9a-f]{40}$/u.test(options.candidateCommit) || !/^[0-9a-f]{40}$/u.test(options.sourceRunHeadSha)) {
    fail('source commit and run head must be 40-character SHA-1 values');
  }
  if (!/^[0-9a-f]{64}$/u.test(options.candidateWebHash) || (options.syntheticWebHash !== undefined && !/^[0-9a-f]{64}$/u.test(options.syntheticWebHash))) {
    fail('candidate web hashes must be 64-character SHA-256 values');
  }
  const candidateIdentity = bundleIdentity(options.candidateIdentity, 'candidate_identity');
  if (candidateIdentity.webHash !== options.candidateWebHash) fail('candidate identity has the wrong candidate web hash');
  const baselineIdentities = new Map(options.baselineIdentities.map((entry) => {
    const name = stringValue(entry.name, 'baseline identity name');
    return [name, bundleIdentity(entry.identity, `baseline identity ${name}`)] as const;
  }));
  if (baselineIdentities.size !== options.baselineIdentities.length) fail('baseline identities contain duplicate names');
  const syntheticCandidateIdentity = options.syntheticCandidateIdentity === undefined
    ? undefined
    : bundleIdentity(options.syntheticCandidateIdentity, 'synthetic_candidate_identity');
  if (syntheticCandidateIdentity && options.syntheticWebHash !== undefined && syntheticCandidateIdentity.webHash !== options.syntheticWebHash) {
    fail('synthetic candidate identity does not match the verified synthetic web hash');
  }
  const syntheticBaselineIdentity = options.syntheticBaselineIdentity === undefined
    ? undefined
    : bundleIdentity(options.syntheticBaselineIdentity, 'synthetic_baseline_identity');
  const rows = await loadEvidenceRows(options);
  const expected = new Map(options.matrix.map((entry) => [rowKey(entry), entry]));
  if (expected.size !== options.matrix.length) fail('expected matrix contains duplicate rows');
  const seen = new Set<string>();
  for (const { filename, result, assessment } of rows) {
    if (result.product.status !== 'PASS' || result.finalization.status !== 'PASS' || result.finalization.failures.length
      || result.primary_failure || result.additional_failures.length || result.qualification_failure) fail('required product or finalization failure');
    const evidence = record(result.evidence, 'evidence');
    for (const snapshot of [evidence, evidence.teardown]) {
      const observed = record(snapshot, 'driver evidence');
      const driver = record(observed.driver, 'driver');
      validateDriverReceipt(driver);
      if (driver.firstFatal || driver.unusable || observed.ownershipFailure) fail('sticky driver or ownership failure');
    }
    if (result.platform === 'android') {
      if (!assessment || assessment.collection.status !== 'PASS') fail('Android collection guarantee unavailable');
      const observations = evidence.productObservations as ProductObservations;
      validateProductObservations(observations, result.identity, assessment.plannedOperations);
      const completion = JSON.parse((await boundedEvidenceFile(join(dirname(filename), 'android-environment-completion.json'), 64_000)).toString());
      if (completion.passed !== true || !Array.isArray(completion.errors) || completion.errors.length || completion.assessmentSha256 !== (result.environment_qualification as { assessmentSha256: string }).assessmentSha256) fail('measurement completion failed');
      const inspections = observations.records.filter(receipt => receipt.kind === 'inspection');
      if ((result.initial_identity as RuntimeIdentity)?.nativePid !== observations.initialNative.pid || (result.final_identity as RuntimeIdentity)?.nativePid !== inspections.at(-1)?.native?.pid) fail('runtime and measured owner mismatch');
      const impact = androidProductImpact(assessment, observations);
      if (!isDeepStrictEqual(evidence.productEventRelations, impact.relations)) fail('Android event relation receipt mismatch');
      if (evidence.selectedInstalledWindowValid !== true || impact.status !== 'PASS') fail('Android product proof failed');
      if (options.contract === 'qualified' && assessment.status !== 'PASS') fail('strict environment qualification failed');
    }
    if (result.source_commit !== options.candidateCommit || result.source_run_head_sha !== options.sourceRunHeadSha) fail(`${filename} has stale source or head identity`);
    if (result.candidate_web_hash !== expectedHash(result, options)) fail(`${filename} has the wrong candidate web hash`);
    if (typeof result.candidate_web_hash !== 'string' || !/^[0-9a-f]{64}$/u.test(result.candidate_web_hash)) fail(`${filename} has an invalid candidate web hash`);
    const candidate = stringValue(result.candidate, `${filename}.candidate`);
    const scenario = candidate === 'current-code-target' ? 'synthetic' : 'historical';
    if (scenario === 'historical' && !candidate.startsWith('candidate-')) fail(`${filename} has an invalid historical candidate identity`);
    const expectedCandidate = scenario === 'synthetic'
      ? syntheticCandidateIdentity
      : candidateIdentity;
    if (!expectedCandidate) fail(`${filename} has no expected ${scenario} candidate identity`);
    const platform = stringValue(result.platform, `${filename}.platform`);
    const baseline = stringValue(result.baseline, `${filename}.baseline`);
    const expectedBaseline = baseline === 'current-code-baseline' ? syntheticBaselineIdentity : baselineIdentities.get(baseline);
    if (!expectedBaseline) fail(`${filename} has no expected identity for baseline ${baseline}`);
    const key = rowKey({ platform, baseline, scenario });
    const expectedRow = expected.get(key);
    if (!expectedRow || seen.has(key)) fail(`${filename} does not match a unique expected matrix row`);
    if (platform !== 'android' && platform !== 'ios') fail(`${filename} has an unsupported platform`);
    seen.add(key);
    const origin = verifiedOrigin(result.origin, `${filename}.origin`);
    assertRuntime(result.initial_identity, platform, 'initial identity', origin, expectedBaseline, false);
    const finalIdentity = assertRuntime(result.final_identity, platform, 'final identity', origin, expectedCandidate, true);
    if (finalIdentity.origin !== (result.initial_identity as Record<string, unknown>).origin) fail(`${filename} changed fixture origin between identities`);
    assertCredentialEvidence(result);
    if (options.suite === 'release' && (!Number.isInteger(result.lifecycle_launch_count) || Number(result.lifecycle_launch_count) < 1)) {
      fail(`${filename} is missing lifecycle evidence`);
    }
    if (result.preference_preserved !== true) fail(`${filename} did not preserve preferences`);
    assertPhoneCompletion(result, baseline);
    assertConsumedFault(result, expectedCandidate);
  }
  if (seen.size !== expected.size) fail('matrix coverage is incomplete');
}
