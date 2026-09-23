import {
  validateMobileEvidence,
  type EvidenceBundleIdentity,
  type EvidenceExpectedBundle,
  type EvidenceMatrixEntry,
  type EvidenceValidationOptions,
} from './support/evidence';

export function option(name: string): string {
  const index = process.argv.indexOf(name);
  const value = index >= 0 ? process.argv[index + 1] : undefined;
  if (!value) throw new Error(`missing ${name}`);
  return value;
}

function matrix(value: string): EvidenceMatrixEntry[] {
  const parsed = JSON.parse(value) as { include?: EvidenceMatrixEntry[] } | EvidenceMatrixEntry[];
  const entries = Array.isArray(parsed) ? parsed : parsed.include;
  if (!entries?.length) throw new Error('matrix is empty');
  return entries.map((entry) => ({
    platform: String(entry.platform),
    baseline: String(entry.baseline),
    scenario: String(entry.scenario),
  }));
}

function json<T>(name: string): T {
  try {
    return JSON.parse(option(name)) as T;
  } catch (error) {
    throw new Error(`${name} is not valid JSON: ${error instanceof Error ? error.message : String(error)}`, { cause: error });
  }
}

export function evidenceOptions(): EvidenceValidationOptions {
  const contract = option('--contract');
  if (contract !== 'product' && contract !== 'qualified') throw new Error('explicit product or qualified contract required');
  return {
  contract,
  runId: option('--run-id'),
  attempt: option('--attempt'),
  directory: option('--directory'),
  matrix: matrix(option('--matrix')),
  suite: option('--suite'),
  candidateCommit: option('--candidate-commit'),
  sourceRunHeadSha: option('--source-run-head-sha'),
  candidateWebHash: option('--candidate-web-hash'),
  candidateIdentity: json<EvidenceBundleIdentity>('--candidate-identity'),
  baselineIdentities: json<EvidenceExpectedBundle[]>('--baseline-identities'),
  syntheticWebHash: process.argv.includes('--synthetic-web-hash') ? option('--synthetic-web-hash') : undefined,
  syntheticCandidateIdentity: process.argv.includes('--synthetic-candidate-identity')
    ? json<EvidenceBundleIdentity>('--synthetic-candidate-identity')
    : undefined,
  syntheticBaselineIdentity: process.argv.includes('--synthetic-baseline-identity')
    ? json<EvidenceBundleIdentity>('--synthetic-baseline-identity')
    : undefined,
  };
}

if (import.meta.main) Promise.resolve().then(() => validateMobileEvidence(evidenceOptions())).catch(() => {
  process.stderr.write('MOBILE_EVIDENCE_INVALID: required product or qualification evidence rejected\n');
  process.exitCode = 1;
});
