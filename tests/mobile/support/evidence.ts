import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';

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
}

export interface EvidenceExpectedBundle {
  name: string;
  identity: EvidenceBundleIdentity;
}

export interface EvidenceValidationOptions {
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

async function resultFiles(root: string): Promise<string[]> {
  const entries = await readdir(root, { withFileTypes: true }).catch(() => []);
  const files: string[] = [];
  for (const entry of entries) {
    const filename = join(root, entry.name);
    if (entry.isDirectory()) files.push(...await resultFiles(filename));
    else if (entry.isFile() && entry.name === 'mobile-result.json') files.push(filename);
  }
  return files;
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
  stringValue(identity.entry, `${label}.entry`);
  stringValue(identity.script, `${label}.script`);
  stringValue(identity.style, `${label}.style`);
  const webHash = stringValue(identity.webHash, `${label}.webHash`);
  if (!/^[0-9a-f]{64}$/u.test(webHash)) fail(`${label}.webHash must be a SHA-256 value`);
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

function assertExpectedBundle(identity: RuntimeIdentity, expected: EvidenceBundleIdentity, label: string): void {
  if (identity.version !== expected.version) fail(`${label}.version does not match the expected bundle`);
  if (identity.assets !== expected.assets) fail(`${label}.assets does not match the expected bundle`);
  if (identity.build !== expected.build) fail(`${label}.build does not match the expected bundle`);
  if (identity.entry !== expected.entry) fail(`${label}.entry does not match the expected bundle`);
  if (identity.script !== expected.script) fail(`${label}.script does not match the expected bundle`);
  if (identity.style !== expected.style) fail(`${label}.style does not match the expected bundle`);
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
  if (!/(?:^|[.$])(?:Webapp|WebApk)[A-Za-z0-9_.-]*Activity$/u.test(activity)) {
    fail(`${label} has an invalid Android installed activity`);
  }
}

function assertRuntime(
  value: unknown,
  platform: string,
  label: string,
  expectedOrigin: string,
  expectedBundle: EvidenceBundleIdentity,
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
  stringValue(identity.entry, `${label}.entry`);
  stringValue(identity.script, `${label}.script`);
  stringValue(identity.style, `${label}.style`);
  if (identity.requiredAssetsReady !== true || identity.applicationInitialized !== true) {
    fail(`${label} is missing loaded runtime evidence`);
  }
  assertExpectedBundle(identity, expectedBundle, label);
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
  const files = (await resultFiles(options.directory)).sort();
  if (files.length !== options.matrix.length) fail(`expected ${options.matrix.length} result files, found ${files.length}`);
  const expected = new Map(options.matrix.map((entry) => [rowKey(entry), entry]));
  if (expected.size !== options.matrix.length) fail('expected matrix contains duplicate rows');
  const seen = new Set<string>();
  for (const filename of files) {
    let result: Record<string, unknown>;
    try {
      result = record(JSON.parse(await readFile(filename, 'utf8')), filename);
    } catch (error) {
      fail(`${filename} is not valid JSON: ${error instanceof Error ? error.message : String(error)}`);
    }
    if (result.schema !== 1 || result.result !== 'passed' || result.suite !== options.suite) fail(`${filename} has an invalid result contract`);
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
    assertRuntime(result.initial_identity, platform, 'initial identity', origin, expectedBaseline);
    const finalIdentity = assertRuntime(result.final_identity, platform, 'final identity', origin, expectedCandidate);
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
