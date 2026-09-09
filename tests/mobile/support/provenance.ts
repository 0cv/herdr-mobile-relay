const SHA1 = /^[a-f0-9]{40}$/u;

export type ProvenanceMode = 'internal' | 'external';

export interface ProvenanceRun {
  repository: string;
  headSha: string;
  event: string;
  headBranch: string;
  workflow: string;
  headRepository: string;
  status: string;
  conclusion: string;
}

export interface ProvenanceOptions {
  mode: ProvenanceMode;
  repository: string;
  artifactRunId: string;
  callerRunId: string;
  candidateCommit?: string;
  sourceCommit?: string;
  manifestRevision: string;
}

export interface ProvenanceResult {
  sourceSha: string;
  headSha: string;
}

function requireSha(value: string, label: string): void {
  if (!SHA1.test(value)) throw new Error(`PROVENANCE_${label}: expected a 40-character commit SHA`);
}

export function validateProvenance(run: ProvenanceRun, options: ProvenanceOptions): ProvenanceResult {
  if (run.repository !== options.repository) throw new Error('PROVENANCE_REPOSITORY: artifact run belongs to another repository');
  requireSha(run.headSha, 'RUN_SHA');
  requireSha(options.manifestRevision, 'MANIFEST_SHA');
  if (options.candidateCommit) requireSha(options.candidateCommit, 'CANDIDATE_SHA');
  if (options.sourceCommit) requireSha(options.sourceCommit, 'SOURCE_SHA');

  if (options.mode === 'internal') {
    if (options.artifactRunId !== options.callerRunId) throw new Error('PROVENANCE_CALLER: internal artifacts must come from the current workflow run');
    if (options.candidateCommit && options.candidateCommit !== options.manifestRevision) {
      throw new Error('PROVENANCE_BUILD_SHA: candidate commit does not match the artifact manifest revision');
    }
    if (options.sourceCommit && options.sourceCommit !== options.manifestRevision) {
      throw new Error('PROVENANCE_SOURCE_SHA: source commit does not match the artifact manifest revision');
    }
    return { sourceSha: options.manifestRevision, headSha: run.headSha };
  }

  if (run.status !== 'completed' || run.conclusion !== 'success') throw new Error('PROVENANCE_RUN: external source run did not complete successfully');
  if (run.event !== 'push' || run.headBranch !== 'main') throw new Error('PROVENANCE_REF: external source run is not a push to main');
  if (run.workflow !== '.github/workflows/check.yml') throw new Error('PROVENANCE_WORKFLOW: external source run is not check.yml');
  if (run.headRepository !== options.repository) throw new Error('PROVENANCE_HEAD_REPOSITORY: external source run did not come from this repository');
  if (options.manifestRevision !== run.headSha) throw new Error('PROVENANCE_BUILD_SHA: artifact manifest revision does not match the source run head');
  if (options.candidateCommit && options.candidateCommit !== run.headSha) throw new Error('PROVENANCE_CANDIDATE_SHA: candidate commit does not match the source run head');
  if (options.sourceCommit && options.sourceCommit !== run.headSha) throw new Error('PROVENANCE_SOURCE_SHA: source commit does not match the source run head');
  return { sourceSha: run.headSha, headSha: run.headSha };
}
