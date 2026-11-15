import {
  validateMobileEvidence,
  type EvidenceBundleIdentity,
  type EvidenceExpectedBundle,
  type EvidenceMatrixEntry,
  type EvidenceValidationOptions,
} from './support/evidence';
import { required } from './support/cli';

function matrix(value: string): EvidenceMatrixEntry[] {
  const parsed = JSON.parse(value) as { include?: EvidenceMatrixEntry[] } | EvidenceMatrixEntry[];
  const entries = Array.isArray(parsed) ? parsed : parsed.include;
  if (!entries?.length) throw new Error('matrix is empty');
  return entries.map((entry) => ({
    platform: String(entry.platform),
    baseline: String(entry.baseline),
  }));
}

function json<T>(name: string): T {
  try {
    return JSON.parse(required(name)) as T;
  } catch (error) {
    throw new Error(`${name} is not valid JSON: ${error instanceof Error ? error.message : String(error)}`, { cause: error });
  }
}

export function evidenceOptions(): EvidenceValidationOptions {
  const contract = required('--contract');
  if (contract !== 'product' && contract !== 'qualified') throw new Error('explicit product or qualified contract required');
  return {
  contract,
  runId: required('--run-id'),
  attempt: required('--attempt'),
  directory: required('--directory'),
  matrix: matrix(required('--matrix')),
  candidateCommit: required('--candidate-commit'),
  sourceRunHeadSha: required('--source-run-head-sha'),
  candidateWebHash: required('--candidate-web-hash'),
  candidateIdentity: json<EvidenceBundleIdentity>('--candidate-identity'),
  baselineIdentities: json<EvidenceExpectedBundle[]>('--baseline-identities'),
  };
}

if (import.meta.main) Promise.resolve().then(() => validateMobileEvidence(evidenceOptions())).catch(() => {
  process.stderr.write('MOBILE_EVIDENCE_INVALID: required product or qualification evidence rejected\n');
  process.exitCode = 1;
});
