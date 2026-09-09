import { validateMobileEvidence, type EvidenceMatrixEntry } from './support/evidence';

function option(name: string): string {
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

validateMobileEvidence({
  directory: option('--directory'),
  matrix: matrix(option('--matrix')),
  suite: option('--suite'),
  candidateCommit: option('--candidate-commit'),
  sourceRunHeadSha: option('--source-run-head-sha'),
  candidateWebHash: option('--candidate-web-hash'),
  syntheticWebHash: process.argv.includes('--synthetic-web-hash') ? option('--synthetic-web-hash') : undefined,
}).catch((error: unknown) => {
  process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
  process.exitCode = 1;
});
