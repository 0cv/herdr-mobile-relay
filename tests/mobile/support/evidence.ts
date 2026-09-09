import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';

export interface EvidenceMatrixEntry {
  platform: string;
  baseline: string;
  scenario: string;
}

export interface EvidenceValidationOptions {
  directory: string;
  matrix: EvidenceMatrixEntry[];
  suite: string;
  candidateCommit: string;
  sourceRunHeadSha: string;
  candidateWebHash: string;
  syntheticWebHash?: string;
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

function assertRuntime(value: unknown, platform: string, label: string): RuntimeIdentity {
  const identity = record(value, label) as RuntimeIdentity;
  if (identity.standalone !== true) fail(`${label} is not an installed standalone runtime`);
  if (identity.provider !== (platform === 'android' ? 'android-standalone' : 'ios-home-screen')) {
    fail(`${label} has an invalid ${platform} provider`);
  }
  const nativeProvider = stringValue(identity.nativeProvider, `${label}.nativeProvider`);
  const nativePrefix = platform === 'android' ? 'android:' : 'ios:';
  if (!nativeProvider.startsWith(nativePrefix)) fail(`${label} has an invalid native provider for ${platform}`);
  stringValue(identity.nativePid, `${label}.nativePid`);
  if (platform === 'android') stringValue(identity.nativeActivity, `${label}.nativeActivity`);
  stringValue(identity.url, `${label}.url`);
  stringValue(identity.origin, `${label}.origin`);
  stringValue(identity.version, `${label}.version`);
  integerAtLeast(identity.assets, 1, `${label}.assets`);
  stringValue(identity.entry, `${label}.entry`);
  stringValue(identity.script, `${label}.script`);
  stringValue(identity.style, `${label}.style`);
  if (identity.requiredAssetsReady !== true || identity.applicationInitialized !== true) {
    fail(`${label} is missing loaded runtime evidence`);
  }
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

function assertConsumedFault(result: Record<string, unknown>, finalIdentity: RuntimeIdentity): void {
  const labels = stringArray(result.faults_exercised, 'faults_exercised');
  if (labels.length !== 1) fail('exactly one candidate fault must be exercised');
  const candidate = stringValue(result.candidate, 'candidate');
  const expectedLabel = candidate === 'current-code-target'
    ? `missing:${stringValue(finalIdentity.style, 'final_identity.style')}`
    : `corrupt:${stringValue(finalIdentity.script, 'final_identity.script')}`;
  if (labels[0] !== expectedLabel) fail(`fault identity ${labels[0]} does not match the expected candidate asset ${expectedLabel}`);
  const separator = labels[0].indexOf(':');
  const kind = labels[0].slice(0, separator);
  const path = labels[0].slice(separator + 1);
  const requests = result.fixture_requests;
  if (!Array.isArray(requests)) fail('fault evidence is missing');
  const consumed = (requests as unknown[]).filter((entry) => {
    const request = record(entry, 'fixture request') as FixtureRequest;
    return request.release === 'candidate'
      && request.fault === kind
      && request.path === path
      && typeof request.fault_id === 'string'
      && request.fault_id.length > 0
      && typeof request.fault_generation === 'string'
      && request.fault_generation.length > 0;
  });
  if (consumed.length !== 1) fail(`fault ${labels[0]} must have exactly one consumed id and generation`);
}

function assertPhoneCompletion(result: Record<string, unknown>, baseline: string): void {
  const completion = record(result.phone_completion, 'phone_completion');
  booleanValue(completion.rawPlanPresent, 'phone_completion.rawPlanPresent');
  booleanValue(completion.phoneRequired, 'phone_completion.phoneRequired');
  booleanValue(completion.phoneAcknowledged, 'phone_completion.phoneAcknowledged');
  if (typeof completion.phoneState !== 'string') fail('phone_completion.phoneState must be a string');
  booleanValue(completion.visibleCompletion, 'phone_completion.visibleCompletion');
  const historicalException = baseline === '0.20.8' || baseline === '0.20.9';
  if (completion.phoneRequired !== !historicalException) {
    fail(`${baseline} has an invalid phone plan requirement`);
  }
  if (historicalException) {
    const controls = stringArray(result.oracle_controls, 'oracle_controls');
    if (!controls.includes(`HISTORICAL_PHONE_ACCOUNTING_UNAVAILABLE:${baseline}`)) {
      fail(`${baseline} is missing its explicit historical phone-plan control`);
    }
    return;
  }
  if (completion.phoneRequired !== true || completion.phoneAcknowledged !== true
    || completion.phoneState !== 'loaded' || completion.visibleCompletion !== true) {
    fail(`${baseline} is missing completed phone acknowledgement`);
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
    const platform = stringValue(result.platform, `${filename}.platform`);
    const baseline = stringValue(result.baseline, `${filename}.baseline`);
    const key = rowKey({ platform, baseline, scenario });
    const expectedRow = expected.get(key);
    if (!expectedRow || seen.has(key)) fail(`${filename} does not match a unique expected matrix row`);
    if (platform !== 'android' && platform !== 'ios') fail(`${filename} has an unsupported platform`);
    seen.add(key);
    assertRuntime(result.initial_identity, platform, 'initial identity');
    const finalIdentity = assertRuntime(result.final_identity, platform, 'final identity');
    assertCredentialEvidence(result);
    if (options.suite === 'release' && (!Number.isInteger(result.lifecycle_launch_count) || Number(result.lifecycle_launch_count) < 1)) {
      fail(`${filename} is missing lifecycle evidence`);
    }
    if (result.preference_preserved !== true) fail(`${filename} did not preserve preferences`);
    assertPhoneCompletion(result, baseline);
    assertConsumedFault(result, finalIdentity);
  }
  if (seen.size !== expected.size) fail('matrix coverage is incomplete');
}
