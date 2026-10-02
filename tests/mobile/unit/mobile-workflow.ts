import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { repositoryPath } from '../support/paths';

type Status = 'success' | 'failure' | 'cancelled' | 'skipped';
type Context = Record<string, any>;
type Row = { platform: 'android' | 'ios'; product: 'PASS' | 'FAIL' | 'INDETERMINATE'; environment: 'PASS' | 'FAIL' | 'UNKNOWN' | 'NOT_APPLICABLE'; complete: boolean; artifact: boolean; current?: boolean };
type StageRows = { smoke: Row[]; release: Row[]; full: Row[] };
const needed = ['provenance', 'prepare', 'smoke', 'smoke_evidence', 'smoke_environment_report', 'smoke_qualification', 'release_smoke', 'release_evidence', 'device', 'gate'];
const strictOnly = ['smoke_qualification', 'release_smoke', 'release_evidence', 'device'];

function expression(source: string, context: Context): any {
  const text = source.replace(/^\s*\$\{\{\s*|\s*\}\}\s*$/gu, '');
  const tokens: string[] = [];
  let position = 0;
  while (position < text.length) {
    const token = /\s*(\|\||&&|==|!=|[()]|'[^']*'|[A-Za-z_][A-Za-z0-9_.-]*)/uy;
    token.lastIndex = position;
    const match = token.exec(text);
    if (!match) { if (!text.slice(position).trim()) break; throw new Error(`Unsupported expression: ${text.slice(position)}`); }
    tokens.push(match[1]); position = token.lastIndex;
  }
  let index = 0;
  const atom = (): any => {
    const token = tokens[index++];
    if (!token) throw new Error('Missing expression operand');
    if (token === '(') { const value = or(); assert.equal(tokens[index++], ')'); return value; }
    if (token.startsWith("'")) return token.slice(1, -1);
    if (token === 'true' || token === 'false') return token === 'true';
    if (tokens[index] === '(') {
      index++;
      if (token === 'always' || token === 'success') { assert.equal(tokens[index++], ')'); return token === 'always' || context.success; }
      if (token === 'fromJSON') { const value = or(); assert.equal(tokens[index++], ')'); return JSON.parse(value); }
      throw new Error(`Unsupported expression function: ${token}`);
    }
    const fields = token.split('.');
    if (!['needs', 'inputs', 'matrix', 'github', 'steps', 'jobs'].includes(fields[0])) throw new Error(`Unsupported context: ${token}`);
    if (fields[0] === 'needs' && !Object.hasOwn(context.needs, fields[1])) throw new Error(`Undeclared needs edge: ${fields[1]}`);
    return fields.reduce((value, key) => value?.[key], context) ?? '';
  };
  const compare = (): any => { let left = atom(); while (['==', '!='].includes(tokens[index])) { const op = tokens[index++], right = atom(); left = op === '==' ? left === right : left !== right; } return left; };
  const and = (): any => { let left = compare(); while (tokens[index] === '&&') { index++; const right = compare(); left = left && right; } return left; };
  const or = (): any => { let left = and(); while (tokens[index] === '||') { index++; const right = and(); left = left || right; } return left; };
  const value = or(); assert.equal(index, tokens.length, 'unsupported trailing expression'); return value;
}
function condition(source: string | undefined, context: Context): boolean {
  if (source === undefined) return Boolean(context.success);
  if (!/\b(?:always|success)\s*\(/u.test(source) && !context.success) return false;
  return Boolean(expression(source, context));
}
function envValues(step: any, context: Context): Context {
  return Object.fromEntries(Object.entries(step.env || {}).map(([key, value]) => [key, typeof value === 'string' && value.includes('${{') ? expression(value, context) : value]));
}
function shellAssertions(source: string, env: Context): { passed: boolean; outputs: Record<string, string> } {
  let active = true;
  const stack: Array<{ parent: boolean; branch: boolean }> = [];
  const outputs: Record<string, string> = {};
  for (const raw of source.trim().split('\n')) {
    const line = raw.trim();
    if (!line || /^set -[a-z]+(?: pipefail)?$/u.test(line)) continue;
    const branch = line.match(/^if \[ "\$(\w+)" = (\w+) \]; then$/u);
    if (branch) { const yes = env[branch[1]] === branch[2]; stack.push({ parent: active, branch: yes }); active &&= yes; continue; }
    if (line === 'else') { const previous = stack.at(-1); assert.ok(previous); active = previous.parent && !previous.branch; continue; }
    if (line === 'fi') { const previous = stack.pop(); assert.ok(previous); active = previous.parent; continue; }
    const test = line.match(/^test "\$(\w+)" = (\w+)$/u);
    if (test) { if (active && env[test[1]] !== test[2]) return { passed: false, outputs }; continue; }
    const echo = line.match(/^echo '(\w+)=(\w+)' >> "\$GITHUB_OUTPUT"$/u);
    if (echo) { if (active) outputs[echo[1]] = echo[2]; continue; }
    throw new Error(`Unsupported guard shell: ${line}`);
  }
  assert.equal(stack.length, 0); return { passed: true, outputs };
}
function validateNativePath(steps: any[], dimension: 'inputs' | 'matrix'): void {
  const check = steps.filter(step => step.run?.includes('android-environment.ts check'));
  assert.equal(check.length, 1);
  assert.equal(check[0].if, `always() && ${dimension}.platform == 'android'`);
  assert.equal(check[0].env.EXPECTED_BASELINE, '${{ ' + dimension + '.baseline }}');
  assert.equal(check[0].env.EXPECTED_SCENARIO, '${{ ' + dimension + '.scenario }}');
  assert.match(check[0].run, /check --contract report/u);
  for (const flag of ['directory', 'identity', 'bundle-set', 'run-id', 'attempt', 'suite', 'baseline', 'scenario', 'source-run-head-sha']) assert.match(check[0].run, new RegExp('--' + flag + ' '));
  assert.doesNotMatch(check[0].run, /--output|continue-on-error|\|\| true/u);
  const sanitize = steps.find(step => step.name === 'Sanitize bounded diagnostics');
  assert.equal(sanitize.if, 'always()'); assert.doesNotMatch(sanitize.run, /cat .*mobile-result/u);
  const summary = steps.find(step => step.name === 'Publish bounded mobile summary');
  assert.equal(summary.if, 'always()'); assert.match(summary.run, /validate-environment.ts --result/u);
  assert.ok(steps.indexOf(summary) > steps.indexOf(sanitize));
  for (const flag of ['bundle-set', 'run-id', 'attempt', 'suite', 'platform', 'baseline', 'scenario', 'source-run-head-sha']) assert.match(summary.run, new RegExp('--' + flag + ' '));
  const upload = steps.find(step => step.uses?.startsWith('actions/upload-artifact@'));
  assert.equal(upload.if, 'always()'); assert.equal(upload.with['if-no-files-found'], 'error'); assert.equal(upload.with['retention-days'], 1);
  assert.match(upload.with.name, /github.run_attempt/u);
  assert.ok(steps.indexOf(upload) > steps.indexOf(summary));
  for (const step of steps) assert.equal(step['continue-on-error'], undefined);
}
function auditUnitShardWorkflow(check: any, source: string): void {
  const jobs = check.jobs;
  const preparation = jobs['mobile-unit-prepare'];
  const shards = jobs['mobile-unit-shards'];
  const aggregate = jobs['mobile-unit-aggregate'];
  assert.ok(preparation && shards && aggregate);
  assert.deepEqual(shards.needs, 'mobile-unit-prepare');
  assert.equal(shards.if, "needs.mobile-unit-prepare.result == 'success'");
  assert.equal(condition(shards.if, { needs: { 'mobile-unit-prepare': { result: 'failure' } }, success: true }), false);
  assert.equal(shards.strategy['fail-fast'], false);
  assert.equal(shards.strategy['max-parallel'], 12);
  assert.equal(shards.strategy.matrix, '${{ fromJSON(needs.mobile-unit-prepare.outputs.matrix) }}');
  assert.equal(preparation.outputs.matrix, '${{ steps.prepare.outputs.matrix }}');
  assert.equal(aggregate.if, 'always()');
  assert.deepEqual(aggregate.needs, ['mobile-unit-prepare', 'mobile-unit-shards']);
  for (const job of [preparation, shards]) {
    const cache = job.steps.find((step: any) => step.id === 'mobile-unit-cache');
    assert.ok(cache?.uses?.startsWith('actions/cache@'));
    assert.equal(cache.with.path, 'frontend/node_modules\ntests/mobile/node_modules\n');
    assert.equal(cache.with.key, "mobile-unit-${{ runner.os }}-node-24.21.0-bun-1.4.2-${{ hashFiles('frontend/bun.lock', 'tests/mobile/bun.lock') }}");
    const install = job.steps.find((step: any) => step.id === 'install-test-dependencies');
    assert.equal(install.if, "steps.mobile-unit-cache.outputs.cache-hit != 'true'");
    assert.ok(install.run.includes('bun install --frozen-lockfile --cwd frontend'));
    assert.ok(install.run.includes('bun install --frozen-lockfile --cwd tests/mobile'));
  }
  for (const [name, job] of Object.entries({ preparation, shards, aggregate }) as Array<[string, any]>) {
    assert.equal(job['timeout-minutes'], 20);
    assert.equal(job.env.BUN_OPTIONS, '--no-env-file');
    assert.equal(job.env.GOTOOLCHAIN, 'local');
    const checkout = job.steps.find((step: any) => step.uses?.startsWith('actions/checkout@'));
    assert.equal(checkout.with.ref, '${{ github.sha }}', `${name} must use the event merge/source SHA`);
    assert.ok(job.steps.some((step: any) => step.uses?.startsWith('actions/setup-node@') && step.with['node-version'] === '24.21.0'));
    assert.ok(job.steps.some((step: any) => step.uses?.startsWith('oven-sh/setup-bun@') && step.with['bun-version'] === '1.4.2'));
    assert.ok(job.steps.some((step: any) => step.uses?.startsWith('actions/setup-go@') && step.with['go-version'] === '1.27.1'));
    assert.ok(job.steps.some((step: any) => step.run?.includes('go env GOTOOLCHAIN') && step.run.includes('tar --version') && step.run.includes('xmllint --version')));
    assert.ok(job.steps.some((step: any) => step.run?.includes('git rev-parse HEAD') && step.run.includes('$GITHUB_SHA')));
    assert.equal(job['continue-on-error'], undefined);
    for (const step of job.steps) assert.equal(step['continue-on-error'], undefined);
  }
  const prepare = preparation.steps.find((step: any) => step.id === 'prepare');
  assert.match(prepare.run, /bun --no-env-file tests\/mobile\/unit\/test-shard-ci\.ts prepare/u);
  assert.equal(prepare.run.includes('GITHUB_RUN_ID'), true);
  const execute = shards.steps.find((step: any) => step.run?.includes('test-shard-ci.ts execute'));
  assert.ok(execute);
  for (const value of ['--prepared-dir', '--group-id', '--receipt-dir', '--source-sha', '--run-id', '--attempt']) assert.ok(execute.run.includes(value));
  assert.match(source, /--select-tests/u);
  assert.match(source, /unitShardCommandArgs\(shard\.selection\)/u);
  const uploadSteps = [preparation, shards].map((job: any) => job.steps.find((step: any) => step.uses?.startsWith('actions/upload-artifact@')));
  for (const upload of uploadSteps) {
    assert.equal(upload.if, 'always()');
    assert.equal(upload.with['if-no-files-found'], 'error');
    assert.equal(upload.with['retention-days'], 1);
    assert.match(upload.with.name, /github.run_attempt/u);
  }
  const downloads = aggregate.steps.filter((step: any) => step.uses?.startsWith('actions/download-artifact@'));
  assert.equal(downloads.length, 1);
  assert.equal(downloads[0].if, 'always()');
  assert.match(downloads[0].with.pattern, /github.run_attempt/u);
  assert.equal(downloads[0].with['merge-multiple'], false);
  const aggregation = aggregate.steps.find((step: any) => step.run?.includes('test-shard-ci.ts aggregate'));
  assert.ok(aggregation);
  assert.equal(aggregation.if, 'always()');
  assert.match(aggregation.run, /UNIT_PREPARATION_RESULT/u);
  assert.match(aggregation.run, /UNIT_GROUPS_RESULT/u);
  assert.match(source, /export function validateUnitCiAggregate/u);
  assert.match(source, /input\.preparationResult !== 'success'/u);
  assert.match(source, /input\.groupsResult !== 'success'/u);
  assert.match(source, /attemptedShardIds/u);
  assert.match(source, /receiptDigests/u);
}

function audit(workflow: any, action: any, release: any, check: any, shardSource: string): void {
  const jobs = workflow.jobs;
  auditUnitShardWorkflow(check, shardSource);
  for (const name of needed) assert.ok(jobs[name], name);
  validateNativePath(action.runs.steps, 'inputs'); validateNativePath(jobs.device.steps, 'matrix');
  for (const name of ['smoke', 'device']) assert.equal(jobs[name].strategy['fail-fast'], false);
  const validators = Object.entries(jobs).flatMap(([job, value]: [string, any]) => (value.steps || []).filter((step: any) => step.run?.includes('bun tests/mobile/validate-evidence.ts')).map((step: any) => ({ job, step })));
  assert.deepEqual(validators.map(value => value.job), ['smoke_evidence', 'release_evidence', 'gate']);
  for (const { job, step } of validators) {
    assert.match(step.run, new RegExp('--contract ' + (job === 'smoke_evidence' ? 'product' : 'qualified')));
    for (const flag of ['run-id', 'attempt', 'matrix', 'candidate-commit', 'source-run-head-sha', 'candidate-web-hash', 'candidate-identity', 'baseline-identities']) assert.match(step.run, new RegExp('--' + flag + ' '));
    assert.equal(step.env.CANDIDATE_COMMIT, '${{ needs.provenance.outputs.source_sha }}');
    assert.equal(step.env.SOURCE_RUN_HEAD_SHA, '${{ needs.provenance.outputs.head_sha }}');
    assert.equal(step.env.CANDIDATE_IDENTITY, '${{ needs.prepare.outputs.candidate_identity }}');
  }
  assert.ok(!jobs.smoke_environment_report.needs.includes('smoke_evidence'));
  assert.equal(jobs.smoke_environment_report.if, 'always()');
  for (const name of ['release_smoke', 'device', 'gate']) assert.ok(jobs[name].needs.includes('smoke_qualification'));
  for (const name of ['smoke_evidence', 'smoke_environment_report', 'smoke_qualification', 'release_evidence', 'gate']) {
    const steps = jobs[name].steps;
    const checkout = steps.findIndex((step: any) => step.uses?.startsWith('actions/checkout@'));
    const downloads = steps.filter((step: any) => step.uses?.startsWith('actions/download-artifact@'));
    assert.equal(downloads.length, 1);
    assert.ok(checkout >= 0 && checkout < steps.indexOf(downloads[0]));
    assert.equal(steps[checkout].with.ref, '${{ needs.provenance.outputs.source_sha }}');
    assert.match(downloads[0].with.pattern, /-\*?-?\$\{\{ github.run_attempt \}\}$/u);
    assert.equal(downloads[0].with['merge-multiple'], false);
  }
  const report = jobs.smoke_environment_report.steps.find((step: any) => step.id === 'report');
  assert.match(report.run, /validate-environment.ts --contract product/u);
  assert.equal(jobs.smoke_environment_report.outputs.environment_status, '${{ steps.report.outputs.environment_status }}');
  const qualification = jobs.smoke_qualification.steps.find((step: any) => step.run?.includes('validate-environment.ts'));
  assert.match(qualification.run, /--require-pass --contract qualified/u);
  assert.equal(workflow.on.workflow_call.outputs.qualification_status.value, '${{ jobs.gate.outputs.qualification_status }}');
  assert.equal(jobs.gate.outputs.qualification_status, '${{ steps.qualification.outputs.qualification_status }}');
  assert.equal(workflow.on.workflow_call.outputs.product_status.value, '${{ jobs.smoke_environment_report.outputs.product_status }}');
  assert.equal(workflow.on.workflow_call.outputs.environment_status.value, '${{ jobs.smoke_environment_report.outputs.environment_status }}');
  assert.deepEqual(release.jobs.mobile.with.suite, 'release'); assert.equal(release.jobs.mobile.with.platform, 'all'); assert.equal(release.jobs.mobile.with.baseline_set, 'all');
  assert.deepEqual(release.jobs.publish.needs, ['verify', 'build', 'native-smoke', 'mobile']);
  assert.equal(release.jobs.publish.if, undefined);
  assert.equal(release.jobs.publish['continue-on-error'], undefined);
  assert.equal(release.jobs.mobile.needs, 'build');
  assert.equal(release.jobs['native-smoke'].needs, 'build');
  const fullValidator = jobs.gate.steps.find((step: any) => step.run?.includes('validate-evidence.ts'));
  assert.equal(fullValidator.if, "needs.device.result == 'success'");
  assert.ok(jobs.gate.steps.indexOf(fullValidator) < jobs.gate.steps.findIndex((step: any) => step.id === 'qualification'));
  assert.equal(jobs.gate.steps.find((step: any) => step.id === 'qualification').if, undefined);
  assert.deepEqual(check.jobs.mobile.needs, ['release', 'mobile-host-check', 'mobile-inspection-check', 'mobile-unit-aggregate']);
  for (const status of ['failure', 'cancelled', 'skipped'] as const) {
    const dependencies = Object.fromEntries(check.jobs.mobile.needs.map((name: string) => [name, { result: name === 'mobile-unit-aggregate' ? status : 'success', outputs: {} }]));
    assert.equal(condition(check.jobs.mobile.if, { needs: dependencies, success: false, github: { ref: 'refs/heads/main', event_name: 'push' } }), false);
  }
  assert.equal(check.jobs.mobile.uses, './.github/workflows/mobile-ci.yml');
  assert.equal(check.jobs.mobile.with.platform, 'all');
  for (const ref of ['dev', 'main', 'ci-mobile']) {
    const context = { needs: {}, github: { ref: 'refs/heads/' + ref, event_name: 'push' }, success: true };
    assert.equal(condition(check.jobs.mobile.if, context), true);
    assert.equal(expression(check.jobs.mobile.with.suite, context), ref === 'dev' ? 'smoke' : 'release');
    assert.equal(expression(check.jobs.mobile.with.baseline_set, context), ref === 'dev' ? 'latest' : 'all');
  }
  for (const [base, head, sameRepository, admitted] of [['main', 'feature', false, true], ['dev', 'ci-mobile', true, true], ['dev', 'ci-mobile', false, false], ['dev', 'feature', true, false]] as const) {
    const context = { needs: {}, success: true, github: { ref: 'refs/pull/25/merge', event_name: 'pull_request', repository: 'public/repo', event: { pull_request: { base: { ref: base }, head: { ref: head, repo: { full_name: sameRepository ? 'public/repo' : 'foreign/repo' } } } } } };
    assert.equal(condition(check.jobs.mobile.if, context), admitted);
    if (admitted) { assert.equal(expression(check.jobs.mobile.with.suite, context), 'release'); assert.equal(expression(check.jobs.mobile.with.baseline_set, context), 'all'); }
  }
  const guard = release.jobs.publish.steps[0];
  assert.equal(guard.env.QUALIFICATION_STATUS, '${{ needs.mobile.outputs.qualification_status }}');
  assert.equal(guard.if, undefined);
  assert.equal(shellAssertions(guard.run, { QUALIFICATION_STATUS: 'PASS' }).passed, true);
  for (const value of ['', 'NOT_REQUESTED', 'FAIL', 'UNKNOWN']) assert.equal(shellAssertions(guard.run, { QUALIFICATION_STATUS: value }).passed, false);
  for (const job of Object.values(jobs) as any[]) { assert.equal(job['continue-on-error'], undefined); for (const step of job.steps || []) assert.equal(step['continue-on-error'], undefined); }
}
function simulate(workflow: any, suite: string, stages: StageRows, overrides: Record<string, Status> = {}) {
  const completed: Context = {};
  for (const name of needed) {
    const job = workflow.jobs[name];
    const rows = stages[['release_smoke', 'release_evidence'].includes(name) ? 'release' : ['device', 'gate'].includes(name) ? 'full' : 'smoke'];
    const invocations: string[] = [];
    const dependencies = Array.isArray(job.needs) ? job.needs : job.needs ? [job.needs] : [];
    const needs = Object.fromEntries(dependencies.map((key: string) => [key, completed[key] || { result: 'failure', outputs: {} }]));
    const context: Context = { needs, inputs: { suite }, success: dependencies.every((key: string) => needs[key].result === 'success') };
    let result: Status;
    const outputs: Record<string, string> = {};
    if (name === 'provenance' || name === 'prepare') { completed[name] = { result: overrides[name] || 'success', outputs: {} }; continue; }
    if (!condition(job.if, context)) result = 'skipped';
    else if (overrides[name]) result = overrides[name];
    else if (['smoke', 'release_smoke', 'device'].includes(name)) result = rows.every(row => row.product === 'PASS' && row.complete && row.artifact) ? 'success' : 'failure';
    else {
      let succeeded = true;
      for (const step of job.steps || []) {
        const stepContext = { ...context, success: succeeded };
        if (!condition(step.if, stepContext)) continue;
        if (step.run?.includes('test "$PROVENANCE_RESULT"') || step.run?.includes('test "$PRODUCT_RESULT"') || step.id === 'qualification') {
          const guard = shellAssertions(step.run, envValues(step, context)); succeeded &&= guard.passed; Object.assign(outputs, guard.outputs);
        } else if (step.run?.includes('bun tests/mobile/validate-evidence.ts') || step.run?.includes('bun tests/mobile/validate-environment.ts')) {
          invocations.push(step.name);
          const isReport = step.id === 'report', strict = /--contract qualified/u.test(step.run);
          const complete = rows.length > 0 && rows.every(row => row.complete && row.artifact && row.current !== false);
          const product = rows.every(row => row.product === 'PASS');
          const qualified = rows.every(row => row.platform === 'ios' ? row.environment === 'NOT_APPLICABLE' : row.environment === 'PASS');
          succeeded &&= complete && (isReport || product) && (!strict || qualified);
          if (isReport) {
            outputs.product_status = !complete ? 'INDETERMINATE' : product ? 'PASS' : 'FAIL';
            outputs.environment_status = rows.some(row => row.environment === 'FAIL') ? 'FAIL' : !complete || rows.some(row => row.environment === 'UNKNOWN') ? 'UNKNOWN' : rows.every(row => row.platform === 'ios') ? 'NOT_APPLICABLE' : 'PASS';
          }
        }
      }
      result = succeeded ? 'success' : 'failure';
    }
    completed[name] = { result, outputs, invocations };
  }
  return completed;
}
export const workflowMutationNames = ['strict-edge', 'qualified-flag', 'native-continue', 'upload-always', 'no-files', 'attempt', 'source-ref', 'output-map', 'postcheck-contract', 'postcheck-overwrite', 'publish-first', 'publish-assertion', 'report-product-dependency', 'full-validator-skipped', 'publish-native-edge', 'unit-aggregate-edge', 'unit-aggregate-always', 'unit-aggregate-dependency', 'unit-shards-unconditional', 'unit-test-dependencies', 'unit-fail-fast', 'unit-continue', 'unit-selector', 'unit-runtime', 'unit-node-runtime', 'unit-go-runtime', 'unit-source-ref', 'unit-weak-aggregate', 'unit-admission-always'] as const;
export const mobileWorkflowTests: Array<[string, () => Promise<void>]> = [
  ['mobile workflow SM59 10 ordinary and strict aggregation', async () => {
    const runtime = (globalThis as unknown as { Bun: { YAML: { parse(source: string): unknown } } }).Bun;
    assert.ok(runtime?.YAML);
    const parse = async (path: string) => runtime.YAML.parse(await readFile(repositoryPath(path), 'utf8')) as any;
    const workflow = await parse('.github/workflows/mobile-ci.yml'), action = await parse('.github/actions/mobile-device-run/action.yml'), release = await parse('.github/workflows/release.yml'), check = await parse('.github/workflows/check.yml');
    const shardSource = await readFile(repositoryPath('tests/mobile/unit/test-shard-ci.ts'), 'utf8');
    audit(workflow, action, release, check, shardSource);
    const stages = (rows: Row[]): StageRows => ({ smoke: rows, release: rows, full: rows });
    const healthy: Row[] = [{ platform: 'android', product: 'PASS', environment: 'PASS', complete: true, artifact: true }, { platform: 'ios', product: 'PASS', environment: 'NOT_APPLICABLE', complete: true, artifact: true }];
    for (const suite of ['smoke', 'release']) for (const environment of ['PASS', 'FAIL', 'UNKNOWN'] as const) {
      const rows = structuredClone(healthy); rows[0].environment = environment;
      const result = simulate(workflow, suite, stages(rows));
      assert.equal(result.smoke_evidence.result, 'success'); assert.equal(result.smoke_environment_report.result, 'success');
      assert.equal(result.smoke_environment_report.outputs.environment_status, environment);
      assert.equal(result.gate.result, suite === 'smoke' || environment === 'PASS' ? 'success' : 'failure');
      assert.equal(result.gate.outputs.qualification_status, suite === 'smoke' ? 'NOT_REQUESTED' : environment === 'PASS' ? 'PASS' : undefined);
    }
    for (const suite of ['smoke', 'release']) for (const prerequisite of needed.filter(name => name !== 'gate' && (suite === 'release' || !strictOnly.includes(name)))) for (const status of ['failure', 'cancelled', 'skipped'] as const) {
      const result = simulate(workflow, suite, stages(healthy), { [prerequisite]: status });
      assert.equal(result.gate.result, 'failure', `${suite}/${prerequisite}/${status}`); assert.notEqual(result.gate.outputs.qualification_status, 'PASS');
    }
    for (const index of [0, 1]) for (const mode of ['product-failure', 'product-indeterminate', 'missing-artifact', 'incomplete'] as const) {
      const rows = structuredClone(healthy);
      if (mode === 'product-failure') rows[index].product = 'FAIL';
      if (mode === 'product-indeterminate') rows[index].product = 'INDETERMINATE';
      if (mode === 'missing-artifact') rows[index].artifact = false;
      if (mode === 'incomplete') rows[index].complete = false;
      const result = simulate(workflow, 'smoke', stages(rows));
      assert.equal(result.smoke.result, 'failure'); assert.equal(result.smoke_evidence.result, 'failure'); assert.equal(result.gate.result, 'failure');
      assert.equal(result.smoke_environment_report.result, mode.startsWith('product') ? 'success' : 'failure');
    }
    assert.equal(simulate(workflow, 'smoke', stages([healthy[1]])).smoke_environment_report.outputs.environment_status, 'NOT_APPLICABLE');
    assert.equal(simulate(workflow, 'smoke', stages([])).smoke_environment_report.result, 'failure');
    const assertFullStage = (candidate: any) => {
      for (const mode of ['PASS', 'FAIL', 'UNKNOWN', 'MISSING', 'STALE'] as const) {
        const full = structuredClone(healthy);
        if (mode === 'FAIL' || mode === 'UNKNOWN') full[0].environment = mode;
        if (mode === 'MISSING') full.pop();
        if (mode === 'STALE') full[0].current = false;
        if (mode === 'MISSING') full[0].complete = false;
        const outcome = simulate(candidate, 'release', { smoke: healthy, release: healthy, full });
        assert.equal(outcome.smoke_qualification.result, 'success');
        assert.equal(outcome.release_evidence.result, 'success');
        if (mode !== 'MISSING') assert.deepEqual(outcome.gate.invocations, ['Validate full device result contracts']);
        assert.equal(outcome.gate.outputs.qualification_status, mode === 'PASS' ? 'PASS' : undefined);
      }
    };
    assertFullStage(workflow);
    for (const environment of ['FAIL', 'UNKNOWN'] as const) {
      const releaseRows = structuredClone(healthy); releaseRows[0].environment = environment;
      const outcome = simulate(workflow, 'release', { smoke: healthy, release: releaseRows, full: healthy });
      assert.equal(outcome.smoke_qualification.result, 'success');
      assert.equal(outcome.release_evidence.result, 'failure');
      assert.equal(outcome.release_evidence.invocations.length, 1);
      assert.notEqual(outcome.gate.outputs.qualification_status, 'PASS');
    }
    const publication = (candidate: any, statuses: Record<string, Status>, qualification = 'PASS') => {
      const needs = Object.fromEntries(candidate.jobs.publish.needs.map((name: string) => [name, { result: statuses[name] || 'success', outputs: name === 'mobile' ? { qualification_status: qualification } : {} }]));
      const context = { needs, success: Object.values(needs).every((value: any) => value.result === 'success') };
      return condition(candidate.jobs.publish.if, context) && shellAssertions(candidate.jobs.publish.steps[0].run, envValues(candidate.jobs.publish.steps[0], context)).passed;
    };
    for (const prerequisite of ['verify', 'build', 'native-smoke', 'mobile']) for (const status of ['failure', 'cancelled', 'skipped'] as const) assert.equal(publication(release, { [prerequisite]: status }), false);
    assert.equal(publication(release, {}), true);
    for (const status of ['', 'FAIL', 'UNKNOWN', 'NOT_REQUESTED']) assert.equal(publication(release, {}, status), false);
    for (const mutation of workflowMutationNames) {
      const w = structuredClone(workflow), a = structuredClone(action), r = structuredClone(release), k = structuredClone(check);
      let source = shardSource;
      if (mutation === 'strict-edge') w.jobs.device.needs = w.jobs.device.needs.filter((name: string) => name !== 'smoke_qualification');
      if (mutation === 'qualified-flag') { const step = w.jobs.gate.steps.find((step: any) => step.run?.includes('validate-evidence.ts')); step.run = step.run.replace('--contract qualified', '--contract product'); }
      if (mutation === 'native-continue') a.runs.steps.find((step: any) => step.name === 'Run installed-PWA upgrade scenarios')['continue-on-error'] = true;
      if (mutation === 'upload-always') a.runs.steps.find((step: any) => step.uses?.startsWith('actions/upload-artifact@')).if = 'success()';
      if (mutation === 'no-files') w.jobs.device.steps.find((step: any) => step.uses?.startsWith('actions/upload-artifact@')).with['if-no-files-found'] = 'warn';
      if (mutation === 'attempt') w.jobs.smoke_evidence.steps.find((step: any) => step.uses?.startsWith('actions/download-artifact@')).with.pattern = 'mobile-evidence-smoke-*';
      if (mutation === 'source-ref') w.jobs.gate.steps.find((step: any) => step.uses?.startsWith('actions/checkout@')).with.ref = 'main';
      if (mutation === 'output-map') w.on.workflow_call.outputs.qualification_status.value = '${{ jobs.smoke_environment_report.outputs.environment_status }}';
      if (mutation === 'postcheck-contract') { const step = a.runs.steps.find((step: any) => step.run?.includes('android-environment.ts check')); step.run = step.run.replace('--contract report', '--contract qualification'); }
      if (mutation === 'postcheck-overwrite') a.runs.steps.find((step: any) => step.run?.includes('android-environment.ts check')).run += ' --output overwrite.json';
      if (mutation === 'publish-first') r.jobs.publish.steps.unshift({ run: 'echo publish' });
      if (mutation === 'publish-assertion') r.jobs.publish.steps[0].run = 'test "$QUALIFICATION_STATUS" = NOT_REQUESTED';
      if (mutation === 'report-product-dependency') w.jobs.smoke_environment_report.needs.push('smoke_evidence');
      if (mutation === 'full-validator-skipped') {
        w.jobs.gate.steps.find((step: any) => step.run?.includes('validate-evidence.ts')).if = 'false';
        assert.throws(() => assertFullStage(w), mutation);
      }
      if (mutation === 'publish-native-edge') {
        r.jobs.publish.needs = r.jobs.publish.needs.filter((name: string) => name !== 'native-smoke');
        assert.equal(publication(r, { 'native-smoke': 'failure' }), true);
      }
      if (mutation === 'unit-aggregate-edge') k.jobs.mobile.needs = k.jobs.mobile.needs.filter((name: string) => name !== 'mobile-unit-aggregate');
      if (mutation === 'unit-aggregate-always') k.jobs['mobile-unit-aggregate'].if = 'success()';
      if (mutation === 'unit-aggregate-dependency') k.jobs['mobile-unit-aggregate'].needs = ['mobile-unit-prepare'];
      if (mutation === 'unit-shards-unconditional') delete k.jobs['mobile-unit-shards'].if;
      if (mutation === 'unit-test-dependencies') k.jobs['mobile-unit-shards'].steps.find((step: any) => step.id === 'install-test-dependencies').run = 'true';
      if (mutation === 'unit-fail-fast') k.jobs['mobile-unit-shards'].strategy['fail-fast'] = true;
      if (mutation === 'unit-continue') k.jobs['mobile-unit-shards'].steps.find((step: any) => step.run?.includes('test-shard-ci.ts execute'))['continue-on-error'] = true;
      if (mutation === 'unit-selector') source = source.replace('--select-tests', '--selected-tests');
      if (mutation === 'unit-runtime') k.jobs['mobile-unit-shards'].steps.find((step: any) => step.uses?.startsWith('oven-sh/setup-bun@')).with['bun-version'] = '1.4.3';
      if (mutation === 'unit-node-runtime') k.jobs['mobile-unit-shards'].steps.find((step: any) => step.uses?.startsWith('actions/setup-node@')).with['node-version'] = '24.20.0';
      if (mutation === 'unit-go-runtime') k.jobs['mobile-unit-shards'].steps.find((step: any) => step.uses?.startsWith('actions/setup-go@')).with['go-version'] = '1.26.0';
      if (mutation === 'unit-source-ref') k.jobs['mobile-unit-shards'].steps.find((step: any) => step.uses?.startsWith('actions/checkout@')).with.ref = 'main';
      if (mutation === 'unit-weak-aggregate') k.jobs['mobile-unit-aggregate'].steps.find((step: any) => step.run?.includes('test-shard-ci.ts aggregate')).run = 'echo accepted';
      if (mutation === 'unit-admission-always') k.jobs.mobile.if = 'always()';
      assert.throws(() => audit(w, a, r, k, source), mutation);
    }
    for (const unsupported of ['contains(inputs.suite, \'release\')', 'needs.*.result', 'inputs.suite + 1', 'failure()']) assert.throws(() => expression(unsupported, { needs: {}, inputs: { suite: 'release' } }));
  }],
];
