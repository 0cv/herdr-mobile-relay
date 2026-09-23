import { appendFile, readFile, writeFile } from 'node:fs/promises';
import { dirname } from 'node:path';
import { evidenceOptions, option } from './validate-evidence';
import { loadEvidenceRows, validateMobileEvidence, type EvidenceValidationOptions } from './support/evidence';
import { environmentStatus, productStatus, type EnvironmentStatus, type ProductStatus } from './support/mobile-result';
import { publicMobileSummary } from './support/android-product';

export async function collectEnvironmentReport(options: EvidenceValidationOptions): Promise<{ product: ProductStatus; environment: EnvironmentStatus | 'NOT_APPLICABLE'; rows: Record<string, unknown>[]; incomplete: boolean; integrity?: 'COMPLETE' | 'INCOMPLETE'; reasons?: string[] }> {
  let incomplete = false;
  const rows = await loadEvidenceRows(options, () => { incomplete = true; });
  const summaries: Record<string, unknown>[] = [];
  for (const row of rows) {
    const summary = publicMobileSummary(row.result, row.assessment);
    if (summary.product === 'PASS') {
      try { await validateMobileEvidence({ ...options, contract: 'product', directory: dirname(row.filename), matrix: [{ platform: row.result.identity.platform, baseline: row.result.identity.baseline, scenario: row.result.identity.scenario }] }); }
      catch {
        incomplete = true;
        summary.product = 'INDETERMINATE';
        summary.findings = [...(summary.findings as unknown[] || []), { category: 'EVIDENCE_INVALID' }].slice(0, 64);
        summary.integrity = 'INCOMPLETE';
      }
    }
    summaries.push(summary);
  }
  incomplete ||= rows.some(row => row.assessment?.collection.status === 'UNKNOWN');
  for (const expected of options.matrix) {
    if (summaries.some(row => row.platform === expected.platform && row.baseline === expected.baseline && row.scenario === expected.scenario)) continue;
    if (!['android', 'ios'].includes(expected.platform) || !/^[A-Za-z0-9._-]{1,100}$/u.test(expected.baseline) || !['historical', 'synthetic'].includes(expected.scenario)) throw new Error('MOBILE_ENVIRONMENT: invalid expected row');
    summaries.push({ ...expected, product: 'INDETERMINATE', environment: expected.platform === 'ios' ? 'NOT_APPLICABLE' : 'UNKNOWN', collection: 'UNKNOWN', findings: [{ category: 'EVIDENCE_INVALID' }] });
    incomplete = true;
  }
  const product = productStatus([...summaries.map(row => row.product as ProductStatus), ...(incomplete ? ['INDETERMINATE' as const] : [])]);
  const android = summaries.filter(row => row.platform === 'android');
  const environment = android.length ? environmentStatus([...android.map(row => row.environment as EnvironmentStatus), ...(incomplete ? ['UNKNOWN' as const] : [])]) : 'NOT_APPLICABLE';
  return { product, environment, rows: summaries, incomplete, ...(incomplete ? { integrity: 'INCOMPLETE', reasons: ['EVIDENCE_INVALID'] } : { integrity: 'COMPLETE', reasons: [] }) };
}

async function singleRowOptions(): Promise<EvidenceValidationOptions> {
  const bundle = JSON.parse(await readFile(option('--bundle-set'), 'utf8'));
  const scenario = option('--scenario');
  return {
    contract: 'product', runId: option('--run-id'), attempt: option('--attempt'), directory: dirname(option('--result')),
    suite: option('--suite'), matrix: [{ platform: option('--platform'), baseline: option('--baseline'), scenario }],
    candidateCommit: bundle.candidate.provenance.sourceCommit, sourceRunHeadSha: option('--source-run-head-sha'),
    candidateWebHash: bundle.candidate.identity.webHash, candidateIdentity: bundle.candidate.identity,
    baselineIdentities: bundle.baselines.map((row: { name: string; identity: unknown }) => ({ name: row.name, identity: row.identity })),
    ...(scenario === 'synthetic' ? { syntheticWebHash: bundle.candidate.identity.webHash, syntheticCandidateIdentity: bundle.candidate.identity, syntheticBaselineIdentity: bundle.baselines[0].identity } : {}),
  };
}

export function serializeEnvironmentReport(report: Awaited<ReturnType<typeof collectEnvironmentReport>>): string {
  const projected = structuredClone(report);
  const serialize = () => `${JSON.stringify(projected, null, 2)}\n`;
  for (const row of [...projected.rows].reverse()) {
    for (const component of (row.components || []) as Array<{ before: { names: string[]; withheldCount: number }; after: { names: string[]; withheldCount: number } }>) {
      for (const field of [component.after, component.before]) while (field.names.length && Buffer.byteLength(serialize()) > 60_000) { field.names.pop(); field.withheldCount++; }
    }
    for (const [key, omitted] of [['events', 'omittedEvents'], ['findings', 'omittedFindings']]) {
      const items = row[key] as unknown[] | undefined;
      while (items?.length && Buffer.byteLength(serialize()) > 60_000) { items.pop(); row[omitted] = Number(row[omitted] || 0) + 1; }
    }
  }
  const bytes = serialize();
  if (Buffer.byteLength(bytes) > 65_536) throw new Error('MOBILE_SUMMARY: projection exceeds bound');
  return bytes;
}

export async function environmentReport(): Promise<void> {
  const options = process.argv.includes('--result') ? await singleRowOptions() : evidenceOptions();
  const report = await collectEnvironmentReport(options);
  const bytes = serializeEnvironmentReport(report);
  if (process.argv.includes('--output')) await writeFile(option('--output'), bytes, { mode: 0o600 });
  process.stdout.write(bytes);
  const title = `Environment qualification: ${report.environment}${['FAIL', 'UNKNOWN'].includes(report.environment) ? ' — nonblocking for ordinary product smoke' : ''}`;
  if (process.env.GITHUB_STEP_SUMMARY) await appendFile(process.env.GITHUB_STEP_SUMMARY, `${title}\n\n\`\`\`json\n${bytes}\`\`\`\n`);
  if (process.env.GITHUB_OUTPUT) await appendFile(process.env.GITHUB_OUTPUT, `product_status=${report.product}\nenvironment_status=${report.environment}\n`);
  if (report.incomplete) throw new Error('MOBILE_ENVIRONMENT: incomplete collection; report delivery failed');
  if (process.argv.includes('--require-pass')) await validateMobileEvidence({ ...options, contract: 'qualified' });
}

if (import.meta.main) environmentReport().catch(() => {
  process.stderr.write('MOBILE_ENVIRONMENT: required evidence missing, invalid or unqualified; no PASS output may be consumed\n');
  process.exitCode = 1;
});
