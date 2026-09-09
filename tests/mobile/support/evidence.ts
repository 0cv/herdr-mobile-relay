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
  standalone?: boolean;
  provider?: string;
  nativeProvider?: string;
  nativeActivity?: string;
  nativePid?: string;
  requiredAssetsReady?: boolean;
  applicationInitialized?: boolean;
  script?: string;
  style?: string;
}

interface FixtureRequest {
  release?: string;
  path?: string;
  fault?: string;
  fault_id?: string;
  fault_generation?: string;
}

function fail(message: string): never {
  throw new Error(`MOBILE_EVIDENCE: ${message}`);
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

function expectedHash(result: Record<string, any>, options: EvidenceValidationOptions): string {
  if (result.candidate === 'current-code-target') {
    if (!options.syntheticWebHash) fail('synthetic web hash is required');
    return options.syntheticWebHash;
  }
  return options.candidateWebHash;
}

function assertRuntime(identity: RuntimeIdentity, platform: string, label: string): void {
  if (!identity || identity.standalone !== true || identity.provider === 'browser' || identity.provider === 'unknown') {
    fail(`${label} is not an installed standalone runtime`);
  }
  if (!identity.nativeProvider || !identity.nativePid) fail(`${label} is missing native provider or pid evidence`);
  if (platform === 'android' && !identity.nativeActivity) fail(`${label} is missing Android activity evidence`);
  if (identity.requiredAssetsReady !== true || identity.applicationInitialized !== true || !identity.script || !identity.style) {
    fail(`${label} is missing loaded runtime evidence`);
  }
}

function assertCredentialEvidence(result: Record<string, any>): void {
  if (result.credential_preserved !== true) fail('credential preservation was not established');
  const relays = result.credential_evidence?.relays;
  if (!relays || typeof relays !== 'object' || Array.isArray(relays) || Object.keys(relays).length === 0) {
    fail('credential ownership evidence is missing');
  }
  for (const [name, relay] of Object.entries(relays as Record<string, any>)) {
    if (relay.invitationAuthCount < 1 || relay.credentialAuthCount < 1 || !Array.isArray(relay.credentialPseudonyms) || relay.credentialPseudonyms.length < 1 || relay.connections < 1) {
      fail(`${name} has incomplete credential ownership evidence`);
    }
  }
}

function assertConsumedFault(result: Record<string, any>): void {
  const labels = result.faults_exercised;
  const requests = result.fixture_requests as FixtureRequest[] | undefined;
  if (!Array.isArray(labels) || labels.length === 0 || !Array.isArray(requests)) fail('fault evidence is missing');
  for (const label of labels) {
    const separator = label.indexOf(':');
    if (separator <= 0) fail(`fault label is malformed: ${label}`);
    const kind = label.slice(0, separator);
    const path = label.slice(separator + 1);
    const consumed = requests.find((request) => request.release === 'candidate'
      && request.fault === kind
      && request.path === path
      && typeof request.fault_id === 'string'
      && request.fault_id.length > 0
      && typeof request.fault_generation === 'string'
      && request.fault_generation.length > 0);
    if (!consumed) fail(`fault ${label} has no consumed id and generation`);
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
    let result: Record<string, any>;
    try {
      result = JSON.parse(await readFile(filename, 'utf8')) as Record<string, any>;
    } catch (error) {
      fail(`${filename} is not valid JSON: ${error instanceof Error ? error.message : String(error)}`);
    }
    if (result.schema !== 1 || result.result !== 'passed' || result.suite !== options.suite) fail(`${filename} has an invalid result contract`);
    if (result.source_commit !== options.candidateCommit || result.source_run_head_sha !== options.sourceRunHeadSha) fail(`${filename} has stale source or head identity`);
    if (result.candidate_web_hash !== expectedHash(result, options)) fail(`${filename} has the wrong candidate web hash`);
    if (typeof result.candidate_web_hash !== 'string' || !/^[0-9a-f]{64}$/u.test(result.candidate_web_hash)) fail(`${filename} has an invalid candidate web hash`);
    if (result.candidate !== 'current-code-target' && (typeof result.candidate !== 'string' || !result.candidate.startsWith('candidate-'))) {
      fail(`${filename} has an invalid historical candidate identity`);
    }
    const scenario = result.candidate === 'current-code-target' ? 'synthetic' : 'historical';
    const key = rowKey({ platform: String(result.platform), baseline: String(result.baseline), scenario });
    if (!expected.has(key) || seen.has(key)) fail(`${filename} does not match a unique expected matrix row`);
    seen.add(key);
    assertRuntime(result.initial_identity, result.platform, 'initial identity');
    assertRuntime(result.final_identity, result.platform, 'final identity');
    assertCredentialEvidence(result);
    if (options.suite === 'release' && (!Number.isInteger(result.lifecycle_launch_count) || result.lifecycle_launch_count < 1)) {
      fail(`${filename} is missing lifecycle evidence`);
    }
    if (result.preference_preserved !== true) fail(`${filename} did not preserve preferences`);
    const completion = result.phone_completion;
    if (!completion || completion.rawPlanPresent !== true) fail(`${filename} is missing phone completion evidence`);
    if (completion.phoneRequired === true && (completion.phoneAcknowledged !== true || completion.phoneState !== 'loaded' || completion.visibleCompletion !== true)) {
      fail(`${filename} is missing completed phone acknowledgement`);
    }
    assertConsumedFault(result);
  }
  if (seen.size !== expected.size) fail('matrix coverage is incomplete');
}
