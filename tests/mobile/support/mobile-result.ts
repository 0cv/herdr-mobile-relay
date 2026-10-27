export type ProductStatus = 'PASS' | 'FAIL' | 'INDETERMINATE';
export type EnvironmentStatus = 'PASS' | 'FAIL' | 'UNKNOWN';
export interface MeasurementIdentity {
  runId: string;
  attempt: string;
  suite: 'smoke' | 'release';
  platform: 'android' | 'ios';
  baseline: string;
  scenario: 'historical' | 'synthetic';
  sourceCommit: string;
  sourceRunHeadSha: string;
  candidateWebHash: string;
  candidateBuild: string;
  measurementId: string;
}
export interface ResultFailure {
  stage: string;
  category: 'PRODUCT' | 'OBSERVATION' | 'COLLECTION' | 'FINALIZATION' | 'EVIDENCE';
  message: string;
}
export type EnvironmentQualification = {
  platform: 'android';
  status: EnvironmentStatus;
  collection: 'PASS' | 'UNKNOWN';
  assessmentSha256: string;
  measurementId: string;
} | { platform: 'ios'; applicability: 'NOT_APPLICABLE' };
export interface MobileResultContract {
  schema: 2;
  identity: MeasurementIdentity;
  product: { status: ProductStatus; categories: string[] };
  environment_qualification: EnvironmentQualification;
  finalization: { status: 'PASS' | 'FAIL'; failures: ResultFailure[] };
  primary_failure?: ResultFailure;
  additional_failures: ResultFailure[];
}
export class FailureLedger {
  private readonly entries: Array<{ error: unknown; failure: ResultFailure }> = [];

  retain(error: unknown, stage: string, category: ResultFailure['category']): void {
    if (this.entries.some(entry => entry.error === error)) return;
    this.entries.push({ error, failure: { stage, category, message: error instanceof Error ? error.message : String(error) } });
  }

  get firstError(): unknown { return this.entries[0]?.error; }
  snapshot(): { primary_failure?: ResultFailure; additional_failures: ResultFailure[] } {
    const failures = this.entries.map(entry => ({ ...entry.failure }));
    return { primary_failure: failures[0], additional_failures: failures.slice(1) };
  }
}
export function productStatus(statuses: readonly ProductStatus[]): ProductStatus {
  if (statuses.includes('FAIL')) return 'FAIL';
  return statuses.includes('INDETERMINATE') ? 'INDETERMINATE' : 'PASS';
}
export function environmentStatus(statuses: readonly EnvironmentStatus[]): EnvironmentStatus {
  if (statuses.includes('FAIL')) return 'FAIL';
  if (!statuses.length || statuses.includes('UNKNOWN')) return 'UNKNOWN';
  return 'PASS';
}
export function requiredExecutionPassed(result: Pick<MobileResultContract, 'product' | 'finalization'>): boolean {
  return result.product.status === 'PASS' && result.finalization.status === 'PASS';
}
export function qualified(result: Pick<MobileResultContract, 'product' | 'finalization' | 'environment_qualification'>): boolean {
  if (!requiredExecutionPassed(result)) return false;
  const environment = result.environment_qualification;
  return environment.platform === 'ios' ? environment.applicability === 'NOT_APPLICABLE'
    : environment.status === 'PASS' && environment.collection === 'PASS';
}
export function validateMeasurementIdentity(value: MeasurementIdentity): void {
  if (!value || !/^[1-9]\d*$/u.test(value.runId) || !/^[1-9]\d*$/u.test(value.attempt)
    || !['smoke', 'release'].includes(value.suite) || !['android', 'ios'].includes(value.platform)
    || !['historical', 'synthetic'].includes(value.scenario) || !/^[A-Za-z0-9._-]{1,100}$/u.test(value.baseline)
    || !/^[a-f0-9]{40}$/u.test(value.sourceCommit) || !/^[a-f0-9]{40}$/u.test(value.sourceRunHeadSha)
    || !/^[a-f0-9]{64}$/u.test(value.candidateWebHash) || !/^[a-f0-9]{64}$/u.test(value.candidateBuild)
    || !/^[A-Za-z0-9-]{1,80}$/u.test(value.measurementId)) throw new Error('MOBILE_IDENTITY: incomplete current measurement identity');
}
