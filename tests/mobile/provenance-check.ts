import { validateProvenance, type ProvenanceRun } from './support/provenance';

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`PROVENANCE_INPUT: ${name} is required`);
  return value;
}

const run: ProvenanceRun = {
  repository: required('PROVENANCE_RUN_REPOSITORY'),
  headSha: required('PROVENANCE_HEAD_SHA'),
  event: required('PROVENANCE_RUN_EVENT'),
  headBranch: process.env.PROVENANCE_RUN_BRANCH || '',
  workflow: process.env.PROVENANCE_RUN_WORKFLOW || '',
  headRepository: process.env.PROVENANCE_HEAD_REPOSITORY || '',
  status: process.env.PROVENANCE_RUN_STATUS || '',
  conclusion: process.env.PROVENANCE_RUN_CONCLUSION || '',
};

const result = validateProvenance(run, {
  mode: required('PROVENANCE_MODE') as 'internal' | 'external',
  repository: required('PROVENANCE_REPOSITORY'),
  artifactRunId: required('PROVENANCE_ARTIFACT_RUN_ID'),
  callerRunId: required('PROVENANCE_CALLER_RUN_ID'),
  candidateCommit: process.env.PROVENANCE_CANDIDATE_COMMIT || undefined,
  sourceCommit: process.env.PROVENANCE_SOURCE_COMMIT || undefined,
  manifestRevision: required('PROVENANCE_MANIFEST_REVISION'),
});

process.stdout.write(`${JSON.stringify(result)}\n`);
