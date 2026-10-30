import assert from 'node:assert/strict';
import { cp, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { createServer } from 'node:http';
import { fileSha256 } from '../support/artifacts';
import { join } from 'node:path';
import { repositoryPath } from '../support/paths';
import { command, CommandError } from '../support/process';
import { evidenceFixture } from './android-product-environment';
import { validateMobileEvidence } from '../support/evidence';
import { collectEnvironmentReport } from '../validate-environment';
import { collectIOSWebKitOutput, IOSWebKitOutputDiagnostics } from '../support/ios-webkit-output';

type Status = 'success' | 'failure' | 'cancelled' | 'skipped';
type Context = Record<string, any>;
type Row = { platform: 'android' | 'ios'; product: 'PASS' | 'FAIL' | 'INDETERMINATE'; environment: 'PASS' | 'FAIL' | 'UNKNOWN' | 'NOT_APPLICABLE'; complete: boolean; artifact: boolean; current?: boolean };
const needed = ['provenance', 'prepare', 'device', 'environment_report', 'gate'];

function expression(source: string, context: Context): any {
  const text = source.replace(/^\s*\$\{\{\s*|\s*\}\}\s*$/gu, '');
  const tokens: string[] = [];
  let position = 0;
  while (position < text.length) {
    const token = /\s*(\|\||&&|==|!=|!|[()]|'[^']*'|[A-Za-z_][A-Za-z0-9_.-]*)/uy;
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
    if (token === '!') return !atom();
    if (token.startsWith("'")) return token.slice(1, -1);
    if (token === 'true' || token === 'false') return token === 'true';
    if (tokens[index] === '(') {
      index++;
      if (token === 'always' || token === 'success') { assert.equal(tokens[index++], ')'); return token === 'always' || context.success; }
      if (token === 'cancelled') { assert.equal(tokens[index++], ')'); return Boolean(context.cancelled); }
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
  if (!/\b(?:always|success|cancelled)\s*\(/u.test(source) && !context.success) return false;
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
  const settle = steps.filter(step => step.run?.includes('tests/mobile/android-gms-settle.ts'));
  assert.equal(settle.length, 1);
  assert.equal(settle[0].if, `${dimension}.platform == 'android'`);
  assert.equal(settle[0].run, 'set -euo pipefail\nbun tests/mobile/android-gms-settle.ts --serial "$ANDROID_SERIAL" --output "$MOBILE_OUTPUT/android-gms-settle.json"\n');
  const appium = steps.findIndex(step => step.name === 'Start Appium');
  const scenarios = steps.map((step, index) => step.run?.includes('bun run --cwd tests/mobile run --') ? index : -1).filter(index => index >= 0);
  assert.ok(appium >= 0 && scenarios.length > 0 && appium < steps.indexOf(settle[0]) && scenarios.every(index => steps.indexOf(settle[0]) < index));
  const sanitize = steps.find(step => step.name === 'Sanitize bounded diagnostics');
  assert.equal(sanitize.if, 'always()'); assert.doesNotMatch(sanitize.run, /cat .*mobile-result/u);
  const summary = steps.find(step => step.name === 'Publish bounded mobile summary');
  assert.equal(summary.if, 'always()'); assert.match(summary.run, /validate-environment.ts --result/u);
  assert.ok(steps.indexOf(summary) > steps.indexOf(sanitize));
  for (const flag of ['bundle-set', 'run-id', 'attempt', 'suite', 'platform', 'baseline', 'scenario', 'source-run-head-sha']) assert.match(summary.run, new RegExp('--' + flag + ' '));
  const strict = steps.find(step => step.name === 'Require strict Android environment qualification');
  assert.equal(strict.if, "always() && inputs.platform == 'android'");
  assert.match(strict.run, /validate-environment.ts --require-pass --result/u);
  for (const flag of ['bundle-set', 'run-id', 'attempt', 'suite', 'platform', 'baseline', 'scenario', 'source-run-head-sha']) assert.match(strict.run, new RegExp('--' + flag + ' '));
  const scenariosRun = steps.filter(step => step.run?.includes('bun run --cwd tests/mobile run --'));
  assert.equal(scenariosRun.length, 1);
  assert.equal(scenariosRun[0].if, undefined);
  assert.doesNotMatch(JSON.stringify(steps), /mobile-bundles-current|current-code-baseline/u);
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
  for (const job of [preparation, shards, aggregate]) {
    const cache = job.steps.find((step: any) => step.id === 'mobile-unit-cache');
    assert.ok(cache?.uses?.startsWith('actions/cache@'));
    assert.equal(cache.with.path, 'frontend/node_modules\ntests/mobile/node_modules\n');
    assert.equal(cache.with.key, "mobile-unit-${{ runner.os }}-node-26.13.0-bun-1.4.4-${{ hashFiles('frontend/bun.lock', 'tests/mobile/bun.lock') }}");
    const install = job.steps.find((step: any) => step.id === 'install-test-dependencies');
    assert.equal(install.if, "steps.mobile-unit-cache.outputs.cache-hit != 'true'");
    assert.ok(install.run.includes('bun install --frozen-lockfile --cwd frontend'));
    assert.ok(install.run.includes('bun install --frozen-lockfile --cwd tests/mobile'));
    const unitCommand = job.steps.findIndex((step: any) => step.run?.includes('tests/mobile/unit/test-shard-ci.ts'));
    assert.ok(job.steps.indexOf(cache) < job.steps.indexOf(install) && job.steps.indexOf(install) < unitCommand);
  }
  for (const [name, job] of Object.entries({ preparation, shards, aggregate }) as Array<[string, any]>) {
    assert.equal(job['timeout-minutes'], 20);
    assert.equal(job.env.BUN_OPTIONS, '--no-env-file');
    assert.equal(job.env.GOTOOLCHAIN, 'local');
    const checkout = job.steps.find((step: any) => step.uses?.startsWith('actions/checkout@'));
    assert.equal(checkout.with.ref, '${{ github.sha }}', `${name} must use the event merge/source SHA`);
    assert.ok(job.steps.some((step: any) => step.uses?.startsWith('actions/setup-node@') && step.with['node-version'] === '26.13.0'));
    assert.ok(job.steps.some((step: any) => step.uses?.startsWith('oven-sh/setup-bun@') && step.with['bun-version'] === '1.4.4'));
    assert.ok(job.steps.some((step: any) => step.uses?.startsWith('actions/setup-go@') && step.with['go-version'] === '1.27.3'));
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
  assert.deepEqual(Object.keys(jobs), needed);
  assert.deepEqual(jobs.prepare.needs, 'provenance');
  assert.deepEqual(jobs.device.needs, ['provenance', 'prepare']);
  assert.equal(jobs.device.if, undefined);
  assert.equal(jobs.device.strategy['fail-fast'], false);
  assert.equal(jobs.device.strategy['max-parallel'], undefined);
  assert.equal(jobs.device.strategy.matrix, '${{ fromJSON(needs.prepare.outputs.device_matrix) }}');
  assert.equal(jobs.device['runs-on'], '${{ matrix.runner }}');
  assert.equal(jobs.device['timeout-minutes'], 60);
  const device = jobs.device.steps.find((step: any) => step.uses === './.github/actions/mobile-device-run');
  assert.deepEqual(device.with, { platform: '${{ matrix.platform }}', baseline: '${{ matrix.baseline }}', scenario: '${{ matrix.scenario }}', suite: 'release', source_head_sha: '${{ needs.provenance.outputs.head_sha }}', evidence_prefix: 'mobile-evidence-device' });
  assert.equal(jobs.device.steps.length, 2);
  assert.equal(jobs.device.steps[0].with.ref, '${{ needs.provenance.outputs.source_sha }}');
  validateNativePath(action.runs.steps, 'inputs');
  const timing = action.runs.steps.find((step: any) => step.name === 'Initialize private runner paths').run;
  assert.match(timing, /if \[ "\$WORKFLOW_PLATFORM" = ios \]; then printf 'MOBILE_TIMING_SCALE=3/u);
  assert.equal(timing.match(/MOBILE_TIMING_SCALE/gu)?.length, 1);
  assert.deepEqual(workflow.permissions, { contents: 'read', actions: 'read' });
  for (const document of [workflow, check]) {
    assert.equal(document.concurrency['cancel-in-progress'], "${{ github.event_name == 'pull_request' }}");
    assert.match(document.concurrency.group, /\$\{\{ github.event.pull_request.number \|\| github.run_id \}\}/u);
  }
  assert.notEqual(workflow.concurrency.group, check.concurrency.group);
  for (const inputs of [workflow.on.workflow_call.inputs, workflow.on.workflow_dispatch.inputs, check.jobs.mobile.with, release.jobs.mobile.with]) {
    for (const key of ['suite', 'baseline_set', 'platform']) assert.equal(inputs[key], undefined);
  }
  const matrix = jobs.prepare.steps.find((step: any) => step.id === 'matrix').run;
  assert.match(matrix, /tests\/mobile\/baselines.json/u);
  assert.match(matrix, /web\/version.json/u);
  assert.match(matrix, /runner: "xcode-27"/u);
  const preparations = jobs.prepare.steps.filter((step: any) => step.run?.includes('prepare:bundle'));
  assert.equal(preparations.length, 2);
  for (const step of preparations) assert.ok(step.run.includes('--baseline "${{ steps.matrix.outputs.baseline }}"'));
  assert.doesNotMatch(JSON.stringify(workflow), /0\.20\.|0\.23\.3|synthetic|prepare:current|test:cache-recovery/u);
  for (const [job, name] of [[check.jobs.release, 'release-bundles'], [jobs.prepare, 'mobile-bundles-android'], [jobs.prepare, 'mobile-bundles-ios']] as const) {
    const upload = job.steps.find((step: any) => step.uses?.startsWith('actions/upload-artifact@') && step.with?.name === name);
    assert.equal(upload?.with.overwrite, true);
  }
  assert.deepEqual(jobs.environment_report.needs, ['provenance', 'prepare', 'device']);
  assert.equal(jobs.environment_report.if, 'always()');
  assert.deepEqual(jobs.gate.needs, ['provenance', 'prepare', 'device', 'environment_report']);
  assert.equal(jobs.gate.if, 'always()');
  for (const name of ['environment_report', 'gate']) {
    const steps = jobs[name].steps;
    const checkout = steps.findIndex((step: any) => step.uses?.startsWith('actions/checkout@'));
    const downloads = steps.filter((step: any) => step.uses?.startsWith('actions/download-artifact@'));
    assert.equal(downloads.length, 1);
    assert.ok(checkout >= 0 && checkout < steps.indexOf(downloads[0]));
    assert.equal(steps[checkout].with.ref, '${{ needs.provenance.outputs.source_sha }}');
    assert.equal(downloads[0].with.pattern, 'mobile-evidence-device-*-${{ github.run_attempt }}');
    assert.equal(downloads[0].with['merge-multiple'], false);
  }
  const report = jobs.environment_report.steps.find((step: any) => step.id === 'report');
  assert.match(report.run, /validate-environment.ts --contract product/u);
  assert.equal(report.if, undefined);
  const strict = jobs.environment_report.steps.find((step: any) => step.name === 'Require strict environment qualification');
  assert.match(strict.run, /validate-environment.ts --require-pass --contract qualified/u);
  assert.ok(jobs.environment_report.steps.indexOf(strict) > jobs.environment_report.steps.indexOf(report));
  const validator = jobs.gate.steps.find((step: any) => step.run?.includes('validate-evidence.ts'));
  assert.equal(validator.if, "needs.device.result == 'success'");
  assert.match(validator.run, /--contract qualified/u);
  for (const step of [report, strict, validator]) {
    for (const flag of ['run-id', 'attempt', 'matrix', 'candidate-commit', 'source-run-head-sha', 'candidate-web-hash', 'candidate-identity', 'baseline-identities']) assert.match(step.run, new RegExp('--' + flag + ' '));
    assert.match(step.run, /--suite release/u);
    assert.equal(step.env.DEVICE_MATRIX, '${{ needs.prepare.outputs.device_matrix }}');
    assert.equal(step.env.CANDIDATE_COMMIT, '${{ needs.provenance.outputs.source_sha }}');
    assert.equal(step.env.SOURCE_RUN_HEAD_SHA, '${{ needs.provenance.outputs.head_sha }}');
    assert.equal(step.env.CANDIDATE_IDENTITY, '${{ needs.prepare.outputs.candidate_identity }}');
  }
  for (const key of ['product_status', 'environment_status']) {
    assert.equal(workflow.on.workflow_call.outputs[key].value, '${{ jobs.environment_report.outputs.' + key + ' }}');
    assert.equal(jobs.environment_report.outputs[key], '${{ steps.report.outputs.' + key + ' }}');
  }
  assert.equal(workflow.on.workflow_call.outputs.qualification_status.value, '${{ jobs.gate.outputs.qualification_status }}');
  assert.equal(jobs.gate.outputs.qualification_status, '${{ steps.qualification.outputs.qualification_status }}');
  assert.ok(jobs.gate.steps.indexOf(validator) < jobs.gate.steps.findIndex((step: any) => step.id === 'qualification'));
  assert.equal(jobs.gate.steps.find((step: any) => step.id === 'qualification').if, undefined);
  assert.deepEqual(check.jobs.mobile.needs, ['release', 'mobile-host-check', 'mobile-inspection-check', 'mobile-unit-aggregate']);
  assert.equal(check.jobs.mobile.uses, './.github/workflows/mobile-ci.yml');
  for (const ref of ['dev', 'main', 'ci-mobile', 'feature']) {
    assert.equal(condition(check.jobs.mobile.if, { needs: {}, github: { ref: 'refs/heads/' + ref, event_name: 'push' }, success: true }), ref === 'dev' || ref === 'main');
  }
  for (const base of ['dev', 'main', 'other']) for (const head of ['ci-mobile', 'feature']) for (const repository of ['public/repo', 'foreign/repo']) {
    const github = { ref: 'refs/pull/25/merge', event_name: 'pull_request', repository: 'public/repo', event: { pull_request: { base: { ref: base }, head: { ref: head, repo: { full_name: repository } } } } };
    assert.equal(condition(check.jobs.mobile.if, { needs: {}, success: true, github }), base !== 'other');
    assert.equal(condition(check.jobs.mobile.if, { needs: {}, success: false, github }), false);
  }
  assert.deepEqual(release.jobs.publish.needs, ['verify', 'build', 'native-smoke', 'mobile']);
  assert.equal(release.jobs.mobile.needs, 'build');
  assert.equal(release.jobs['native-smoke'].needs, 'build');
  assert.equal(release.jobs.publish.if, undefined);
  assert.equal(release.jobs.publish['continue-on-error'], undefined);
  const guard = release.jobs.publish.steps[0];
  assert.equal(guard.env.QUALIFICATION_STATUS, '${{ needs.mobile.outputs.qualification_status }}');
  assert.equal(guard.if, undefined);
  assert.equal(shellAssertions(guard.run, { QUALIFICATION_STATUS: 'PASS' }).passed, true);
  for (const value of ['', 'NOT_REQUESTED', 'FAIL', 'UNKNOWN']) assert.equal(shellAssertions(guard.run, { QUALIFICATION_STATUS: value }).passed, false);
  for (const job of Object.values(jobs) as any[]) {
    assert.equal(job['continue-on-error'], undefined);
    for (const step of job.steps || []) {
      assert.equal(step['continue-on-error'], undefined);
      if (step.uses && !step.uses.startsWith('./')) assert.match(step.uses, /@[0-9a-f]{40}$/u);
      if (step.uses?.startsWith('actions/upload-artifact@')) assert.equal(step.with['retention-days'], 1);
    }
  }
}
function simulate(workflow: any, rows: Row[], overrides: Record<string, Status> = {}) {
  const completed: Context = {};
  const complete = rows.length === 2 && new Set(rows.map(row => row.platform)).size === 2 && rows.every(row => row.complete && row.artifact && row.current !== false);
  const product = rows.every(row => row.product === 'PASS');
  const qualified = rows.every(row => row.platform === 'ios' ? row.environment === 'NOT_APPLICABLE' : row.environment === 'PASS');
  for (const name of needed) {
    const job = workflow.jobs[name];
    const invocations: string[] = [];
    const dependencies = Array.isArray(job.needs) ? job.needs : job.needs ? [job.needs] : [];
    const needs = Object.fromEntries(dependencies.map((key: string) => [key, completed[key] || { result: 'failure', outputs: {} }]));
    const context: Context = { needs, success: dependencies.every((key: string) => needs[key].result === 'success') };
    let result: Status;
    const outputs: Record<string, string> = {};
    if (name === 'provenance' || name === 'prepare') { completed[name] = { result: overrides[name] || 'success', outputs }; continue; }
    if (!condition(job.if, context)) result = 'skipped';
    else if (overrides[name]) result = overrides[name];
    else if (name === 'device') result = complete && product && qualified ? 'success' : 'failure';
    else {
      let succeeded = true;
      for (const step of job.steps || []) {
        if (!condition(step.if, { ...context, success: succeeded })) continue;
        if (step.run?.includes('test "$PROVENANCE_RESULT"') || step.id === 'qualification') {
          const guard = shellAssertions(step.run, envValues(step, context)); succeeded &&= guard.passed; Object.assign(outputs, guard.outputs);
        } else if (step.run?.includes('bun tests/mobile/validate-evidence.ts') || step.run?.includes('bun tests/mobile/validate-environment.ts')) {
          invocations.push(step.name);
          const isReport = step.id === 'report', strict = /--contract qualified/u.test(step.run);
          succeeded &&= complete && (isReport || product) && (!strict || qualified);
          if (isReport) {
            outputs.product_status = !complete ? 'INDETERMINATE' : rows.some(row => row.product === 'FAIL') ? 'FAIL' : product ? 'PASS' : 'INDETERMINATE';
            outputs.environment_status = rows.some(row => row.environment === 'FAIL') ? 'FAIL' : !complete || rows.some(row => row.environment === 'UNKNOWN') ? 'UNKNOWN' : 'PASS';
          }
        }
      }
      result = succeeded ? 'success' : 'failure';
    }
    completed[name] = { result, outputs, invocations };
  }
  return completed;
}
export const mobileWorkflowTests: Array<[string, () => Promise<void>]> = [
  ['mobile iOS producer install/start binds the post-install gate and never invokes a shadow global Appium', async () => {
    const runtime = (globalThis as unknown as { Bun: { YAML: { parse(source: string): any } } }).Bun;
    const document = runtime.YAML.parse(await readFile(repositoryPath('.github/actions/mobile-device-run/action.yml'), 'utf8'));
    const steps = document.runs.steps;
    const install = steps.find((step: any) => step.name === 'Install pinned Appium and platform driver');
    assert.match(install.run, /bash tests\/mobile\/ios-appium\/ci\.sh install/u);
    assert.doesNotMatch(install.run, /npm install --global|appium driver install/u);
    const start = steps.find((step: any) => step.name === 'Start Appium');
    assert.match(start.run, /ios-appium\/ci\.sh verify\n\s+bash tests\/mobile\/ios-appium\/ci\.sh start/u);
    assert.doesNotMatch(start.run, /^\s*appium /mu);
    assert.match(start.run, /\n\s+kill -0 "\$appium_pid"/u);
    assert.doesNotMatch(start.run, /(?:sed|cat|tail|head).*appium\.log/u);
    const script = await readFile(repositoryPath('tests/mobile/ios-appium/ci.sh'), 'utf8');
    assert.ok(script.indexOf('npm install ') < script.indexOf('gate.ts" pristine'));
    assert.ok(script.indexOf('gate.ts" pristine') < script.indexOf('"$root/test.ts" original'));
    assert.ok(script.indexOf('"$root/test.ts" original') < script.indexOf('gate.ts" patch'));
    assert.ok(script.indexOf('gate.ts" patch') < script.indexOf('"$root/test.ts" patched'));
    assert.doesNotMatch(script, /MOBILE_UNIT_FILTER|test:unit/u);
    const ordinary = await readFile(repositoryPath('tests/mobile/unit/run.ts'), 'utf8');
    assert.doesNotMatch(ordinary, /iosProducerTests|iosProducerGateTests|IOS_PRODUCER_HOME/u);
    const entry = await readFile(repositoryPath('tests/mobile/ios-appium/test.ts'), 'utf8');
    assert.match(entry, /Unexpected fixture regression inventory/u);
    assert.doesNotMatch(entry, /# SKIP|MOBILE_UNIT_FILTER/u);
    const retain = steps.find((step: any) => step.name === 'Retain captured iOS producer integrity');
    assert.equal(retain.if, "always() && inputs.platform == 'ios'");
    assert.match(retain.run, /bun tests\/mobile\/ios-appium\/evidence\.ts "\$APPIUM_HOME" "\$MOBILE_OUTPUT"/u);
    assert.doesNotMatch(retain.run, /verify|appium .*driver|\|\| true/u);
    const scenarios = steps.findIndex((step: any) => step.name === 'Run installed-PWA upgrade scenarios');
    const upload = steps.findIndex((step: any) => step.name === 'Upload mobile evidence');
    const cleanup = steps.findIndex((step: any) => step.name === 'Stop owned mobile processes and devices');
    assert.ok(scenarios < steps.indexOf(retain) && steps.indexOf(retain) < upload && upload < cleanup);
    assert.equal(retain['continue-on-error'], undefined);
    assert.match(script, /--install-strategy=nested/u);
    assert.match(script, /start\)[\s\S]*gate.ts" verify "\$APPIUM_HOME"\n\s+exec "\$node_binary" "\$APPIUM_HOME\/node_modules\/appium\/build\/lib\/main.js" "\$@"/u);
  }],
  ['mobile evidence accepts exactly one shared baseline across Android and iOS', async () => {
    const android = await evidenceFixture('android'), ios = await evidenceFixture('ios');
    const root = await mkdtemp(join(tmpdir(), 'herdr-mobile-two-rows-'));
    try {
      await cp(android.directory, join(root, 'android'), { recursive: true });
      await cp(ios.directory, join(root, 'ios'), { recursive: true });
      const options = { ...android.options, contract: 'qualified' as const, directory: root, matrix: [...android.options.matrix, ...ios.options.matrix] };
      await validateMobileEvidence(options);
      const report = await collectEnvironmentReport(options);
      assert.equal(report.product, 'PASS');
      assert.equal(report.environment, 'PASS');
      assert.equal(report.rows.length, 2);
      assert.equal(report.incomplete, false);
      for (const matrix of [
        [...options.matrix, ios.options.matrix[0]],
        [android.options.matrix[0], android.options.matrix[0]],
        [android.options.matrix[0], { ...ios.options.matrix[0], baseline: 'another-release' }],
        [android.options.matrix[0], { ...ios.options.matrix[0], scenario: 'synthetic' }],
      ]) {
        await assert.rejects(validateMobileEvidence({ ...options, matrix }), /expected matrix/);
        await assert.rejects(collectEnvironmentReport({ ...options, matrix }), /expected matrix/);
      }
      for (const baselineIdentities of [[], [...options.baselineIdentities, { ...options.baselineIdentities[0], name: 'another-release' }], [{ ...options.baselineIdentities[0], name: 'another-release' }]]) {
        await assert.rejects(validateMobileEvidence({ ...options, baselineIdentities }), /prepared baseline/);
        await assert.rejects(collectEnvironmentReport({ ...options, baselineIdentities }), /prepared baseline/);
      }
      await rm(join(root, 'ios'), { recursive: true });
      await assert.rejects(validateMobileEvidence(options), /missing or duplicated/);
      assert.equal((await collectEnvironmentReport(options)).incomplete, true);
      await cp(ios.directory, join(root, 'ios'), { recursive: true });
      await assert.rejects(validateMobileEvidence({ ...options, attempt: String(Number(options.attempt) + 1) }), /stale or inconsistent/);
    } finally {
      await Promise.all([root, android.directory, ios.directory].map(directory => rm(directory, { recursive: true, force: true })));
    }
  }],
  ['mobile preparation selects one baseline from a multi-release manifest', async () => {
    const root = await mkdtemp(join(tmpdir(), 'herdr-mobile-default-baseline-'));
    try {
      const manifest = join(root, 'baselines.json');
      const args = [repositoryPath('tests/mobile/prepare.ts'), '--manifest', manifest, '--candidate', join(root, 'absent.tar.gz'),
        '--candidate-version', 'candidate', '--candidate-assets', '477', '--candidate-revision', 'a'.repeat(40), '--candidate-sha256', 'b'.repeat(64)];
      const baseline = { name: 'next-release', url: 'https://fixture.invalid/release.tar.gz', archiveSha256: 'c'.repeat(64) };
      const unused = { name: 'unused-release' };
      for (const [baselines, selection] of [
        [[baseline, unused], []],
        [[unused, baseline], ['--baseline', baseline.name]],
      ] as const) {
        await writeFile(manifest, JSON.stringify({ schema: 1, baselines }));
        await assert.rejects(command(process.execPath, [...args, ...selection], 10_000), (error: unknown) => error instanceof CommandError && /ARTIFACT_ROOT/u.test(error.stderr));
      }
      await assert.rejects(command(process.execPath, [...args, '--baseline', 'absent'], 10_000), (error: unknown) => error instanceof CommandError && /not declared/u.test(error.stderr));
      await assert.rejects(command(process.execPath, [...args, '--baseline', baseline.name, '--baseline', unused.name], 10_000), (error: unknown) => error instanceof CommandError && /exactly one selected baseline/u.test(error.stderr));
      await writeFile(manifest, JSON.stringify({ schema: 1, baselines: [] }));
      await assert.rejects(command(process.execPath, args, 10_000), (error: unknown) => error instanceof CommandError && /baseline manifest schema/u.test(error.stderr));
    } finally { await rm(root, { recursive: true, force: true }); }
  }],
  ['mobile preparation downloads only the selected release and writes a single-baseline bundle set', async () => {
    const root = await mkdtemp(join(tmpdir(), 'herdr-mobile-selected-download-'));
    const requests: string[] = [];
    let archiveBytes = Buffer.alloc(0);
    const server = createServer((request, response) => {
      requests.push(request.url || '');
      response.end(archiveBytes);
    });
    try {
      await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve));
      const address = server.address();
      assert.ok(address && typeof address !== 'string');
      const archives: Array<{ archive: string; hash: string }> = [];
      for (const version of ['previous', 'newest']) {
        const source = join(root, version);
        await mkdir(join(source, 'web/assets'), { recursive: true });
        await writeFile(join(source, 'web/version.json'), JSON.stringify({ version, assets: 1 }));
        await writeFile(join(source, 'web/index.html'), '<html></html>');
        await writeFile(join(source, 'web/assets/app.js'), version);
        await writeFile(join(source, 'web/assets/app.css'), 'body{}');
        await writeFile(join(source, 'release-manifest.json'), JSON.stringify({ version, revision: 'a'.repeat(40) }));
        const archive = join(root, version + '.tar.gz');
        await command('tar', ['-C', source, '-czf', archive, 'web', 'release-manifest.json'], 10_000);
        archives.push({ archive, hash: await fileSha256(archive) });
      }
      archiveBytes = await readFile(archives[0].archive);
      const manifest = join(root, 'baselines.json'), output = join(root, 'prepared');
      await writeFile(manifest, JSON.stringify({ schema: 1, baselines: ['newest', 'previous'].map(name => ({
        name, version: name, assets: 1, sourceRelease: name, sourceCommit: 'a'.repeat(40),
        archiveSha256: name === 'previous' ? archives[0].hash : 'c'.repeat(64),
        url: `http://127.0.0.1:${address.port}/${name}.tar.gz`,
      })) }));
      await command(process.execPath, [repositoryPath('tests/mobile/prepare.ts'), '--manifest', manifest,
        '--candidate', archives[1].archive, '--candidate-version', 'newest', '--candidate-assets', '1',
        '--candidate-revision', 'a'.repeat(40), '--candidate-sha256', archives[1].hash,
        '--baseline', 'previous', '--output', output], 10_000);
      const bundle = JSON.parse(await readFile(join(output, 'bundle-set.json'), 'utf8'));
      assert.deepEqual(bundle.baselines.map((entry: any) => entry.name), ['previous']);
      assert.equal(bundle.candidate.identity.version, 'newest');
      assert.deepEqual(requests, ['/previous.tar.gz']);
    } finally {
      await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
      await rm(root, { recursive: true, force: true });
    }
  }],
  ['mobile iOS WebKit trace distinguishes pending listing from completed discovery without retaining payloads', async () => {
    const secret = '秘密 https://private.test/?token=private-invitation arbitrary-application-label';
    const frame = (component: string, message: string) => `2026-10-29 11:04:34:000 - [${component}] ${message}\n`;
    const lines = [
      frame('RemoteDebugger', 'Remote Debugger version 17.4.2'),
      frame('1234abcd][XCUITestDriver@abcd', 'Getting list of available contexts'),
      frame('XCUITestDriver@abcd', 'Retrieving contexts and views'),
      frame('RemoteDebugger', 'Selecting application'),
      frame('RemoteDebugger', "Attempting app 'PID:123'"),
      frame('RemoteDebugger', "Sending '_rpc_forwardGetListing:' message to app 'PID:123' (id: 42): 'connectToApp'"),
    ];
    const stalled = new IOSWebKitOutputDiagnostics();
    for (const part of lines.join('')) stalled.observe(part);
    stalled.finish();
    assert.equal(stalled.data.events.at(-1)?.stage, 'listing-send');
    assert.equal(stalled.data.events.at(-1)?.messageId, 42);
    assert.equal(stalled.data.remoteDebuggerVersion, '17.4.2');
    assert.equal(stalled.data.diagnosticOnly, true);
    const completed = new IOSWebKitOutputDiagnostics();
    const source = lines.join('')
      + frame('RemoteDebugger', `Received response from send (id: 42): '${secret}'`)
        .replace('[RemoteDebugger]', '\u001b[38;5;41m[1234abcd]\u001b[0m\u001b[38;5;141m[RemoteDebugger]\u001b[0m')
      + frame('RemoteDebugger', 'Finally selecting app PID:123')
      + frame('RemoteDebugger', 'Selected app after 55000ms')
      + frame('XCUITestDriver@abcd', 'Found webview context after 55001ms')
      + frame('XCUITestDriver@abcd', `No available web pages after 1 retry: ${secret}`)
      + frame('RemoteDebugger', `The application PID:123 is not connectable yet: ${secret}`)
      + frame('RemoteDebugger', 'Debugger socket disconnected')
      + frame('RemoteDebugger', secret)
      + frame('HTTP', secret);
    for (let offset = 0; offset < source.length; offset += 13) completed.observe(source.slice(offset, offset + 13));
    completed.finish();
    assert.deepEqual(completed.data.events.map(event => event.stage), [
      'debugger-version', 'contexts-start', 'views-start', 'select-app-start', 'app-attempt', 'listing-send',
      'rpc-response', 'app-selected', 'select-app-end', 'contexts-ready', 'page-search-error', 'app-not-connectable', 'socket-disconnected',
    ]);
    assert.equal(completed.data.events.find(event => event.stage === 'rpc-response')?.messageId, 42);
    assert.equal(completed.data.events.find(event => event.stage === 'select-app-end')?.durationMs, 55000);
    assert.equal(completed.data.unknownRecords, 2);
    const serialized = JSON.stringify(completed.data);
    assert.doesNotMatch(serialized, /秘密|private.test|invitation|application-label|Received response/u);
    assert.ok(completed.data.events.every(event => event.timestampMs === Date.UTC(2026, 9, 29, 11, 4, 34)));
  }],
  ['mobile iOS WebKit trace bounds framing input events and missing evidence', async () => {
    const line = '2026-10-29 11:04:34:000 - [RemoteDebugger] Selecting application\n';
    const trace = new IOSWebKitOutputDiagnostics(true);
    trace.observe(line); // This may be a suffix of a payload at the tail boundary.
    trace.observe('秘'.repeat(4096));
    trace.observe('\n');
    for (let count = 0; count < 300; count++) trace.observe(line);
    trace.observe(line.slice(0, -1));
    trace.finish();
    trace.finish();
    trace.observe(line);
    assert.equal(trace.data.recognizedRecords, 300);
    assert.equal(trace.data.events.length, 256);
    assert.equal(trace.data.events[0].ordinal, 45);
    assert.equal(trace.data.droppedEvents, 44);
    assert.equal(trace.data.oversizedRecords, 1);
    assert.equal(trace.data.incompleteRecords, 1);
    assert.equal(trace.data.inputTruncated, true);
    const root = await mkdtemp(join(tmpdir(), 'herdr-webkit-trace-'));
    try {
      const input = join(root, 'appium.log');
      const output = join(root, 'trace.json');
      await collectIOSWebKitOutput(input, output);
      const missing = JSON.parse(await readFile(output, 'utf8'));
      assert.equal(missing.status, 'missing');
      assert.equal(missing.scannedBytes, 0);
      await writeFile(input, '');
      await collectIOSWebKitOutput(input, output);
      assert.equal(JSON.parse(await readFile(output, 'utf8')).status, 'captured');
      await writeFile(input, 'x'.repeat(8 * 1024 * 1024) + '\n' + line);
      await collectIOSWebKitOutput(input, output);
      const bounded = JSON.parse(await readFile(output, 'utf8'));
      assert.equal(bounded.status, 'captured');
      assert.equal(bounded.inputTruncated, true);
      assert.equal(bounded.scannedBytes, 8 * 1024 * 1024);
      assert.equal(bounded.events.length, 1);
      assert.equal(bounded.events[0].stage, 'select-app-start');
      assert.ok((await readFile(output)).length < 1024);
      const driver = join(root, 'node_modules/appium-xcuitest-driver');
      const remote = join(driver, 'node_modules/appium-remote-debugger');
      await mkdir(remote, { recursive: true });
      const manifest = (version: string) => JSON.stringify({ version, exports: { './package.json': './package.json' }, privatePayload: '秘密 https://private.test/' });
      await writeFile(join(driver, 'package.json'), manifest('12.12.4'));
      await writeFile(join(remote, 'package.json'), manifest('17.4.9'));
      await collectIOSWebKitOutput(input, output, root);
      const resolved = JSON.parse(await readFile(output, 'utf8'));
      assert.deepEqual(resolved.runtimeVersions, {
        xcuitest: { status: 'available', version: '12.12.4' }, remoteDebugger: { status: 'available', version: '17.4.9' },
      });
      assert.doesNotMatch(JSON.stringify(resolved), /秘密|private.test/u);
      for (const invalid of [manifest('秘密 https://private.test/'), 'x'.repeat(16_385), '{invalid']) {
        await writeFile(join(remote, 'package.json'), invalid);
        await collectIOSWebKitOutput(input, output, root);
        const unavailable = JSON.parse(await readFile(output, 'utf8'));
        assert.deepEqual(unavailable.runtimeVersions.remoteDebugger, { status: 'unavailable' });
        assert.doesNotMatch(JSON.stringify(unavailable), /秘密|private.test|invalid/u);
      }
    } finally { await rm(root, { recursive: true, force: true }); }
  }],
  ['mobile iOS Appium log withholding executes both scoped copy branches without changing Android', async () => {
    const runtime = (globalThis as unknown as { Bun: { YAML: { parse(source: string): unknown } } }).Bun;
    for (const path of ['.github/actions/mobile-device-run/action.yml']) {
      const document = runtime.YAML.parse(await readFile(repositoryPath(path), 'utf8')) as any;
      const groups = document.runs ? [document.runs.steps] : Object.values(document.jobs).map((job: any) => job.steps || []);
      const copies = groups.flat().filter((step: any) => step.run?.includes('"$MOBILE_OUTPUT/appium.log"'));
      assert.equal(copies.length, 1, path);
      const step = copies[0];
      assert.equal(step.if, 'always()');
      const branch = step.run.match(/if \[ "\$MOBILE_PLATFORM" = ios \]; then\n {2}rm -f "\$MOBILE_OUTPUT\/appium.log"[\s\S]*?\nfi/u)?.[0];
      assert.ok(branch, path);
      assert.match(branch, /elif \[ -f "\$RUNNER_TEMP\/appium.log" \]; then/u);
      assert.equal(step.run.replace(branch, '').includes('appium.log'), false);
      for (const steps of groups) {
        const copyIndex = steps.indexOf(step);
        for (const [index, candidate] of steps.entries()) {
          if (!candidate.run?.includes('appium.log') || candidate === step) continue;
          assert.ok(copyIndex > index, 'only startup logging may precede capture');
          assert.doesNotMatch(candidate.run, /(?:sed|cat|head|tail).*appium\.log/u);
          assert.match(candidate.run, /payload-bearing log withheld/u);
          assert.doesNotMatch(candidate.run, /MOBILE_OUTPUT\/appium.log/u);
        }
        for (const upload of steps.filter((candidate: any) => candidate.uses?.startsWith('actions/upload-artifact'))) {
          assert.doesNotMatch(upload.with.path, /appium.log|\$\{\{ runner.temp \}\}\s*$/u);
        }
      }
      for (const platform of ['ios', 'android']) {
        const root = await mkdtemp(join(tmpdir(), 'herdr-appium-disposition-'));
        const output = join(root, 'output');
        await mkdir(output);
        const raw = 'private source: 秘密 https://private.test/ arbitrary-application-label\n';
        await writeFile(join(root, 'appium.log'), raw, { flag: 'wx' });
        await writeFile(join(output, 'appium.log'), 'stale artifact must be withheld', { flag: 'wx' });
        const result = await command('bash', ['-c', 'set -euo pipefail\n' + branch], 10_000, {
          cwd: repositoryPath('.'),
          env: { ...process.env, MOBILE_PLATFORM: platform, RUNNER_TEMP: root, MOBILE_OUTPUT: output },
        });
        assert.equal(result.stdout, '');
        assert.equal(result.stderr, '');
        assert.equal(await readFile(join(root, 'appium.log'), 'utf8'), raw, 'private ephemeral logging remains unchanged');
        if (platform === 'ios') {
          await assert.rejects(() => readFile(join(output, 'appium.log')), { code: 'ENOENT' });
          const disposition = JSON.parse(await readFile(join(output, 'appium-log-disposition.json'), 'utf8'));
          assert.equal(disposition.appiumLog, 'withheld');
          assert.match(disposition.reason, /client request timing and bounded confirmation diagnostics/u);
          assert.doesNotMatch(JSON.stringify(disposition), /秘密|private.test|application-label/u);
          const trace = JSON.parse(await readFile(join(output, 'ios-webkit-discovery.json'), 'utf8'));
          assert.equal(trace.status, 'captured');
          assert.equal(trace.recognizedRecords, 0);
          assert.equal(trace.unknownRecords, 1);
          assert.doesNotMatch(JSON.stringify(trace), /秘密|private.test|application-label/u);
        } else {
          assert.equal(await readFile(join(output, 'appium.log'), 'utf8'), raw);
          await assert.rejects(() => readFile(join(output, 'appium-log-disposition.json')), { code: 'ENOENT' });
          await assert.rejects(() => readFile(join(output, 'ios-webkit-discovery.json')), { code: 'ENOENT' });
        }
        await rm(root, { recursive: true, force: true });
      }
    }
  }],
  ['mobile iOS XCTest disposition copy preserves only the payload-free decision', async () => {
    const runtime = (globalThis as unknown as { Bun: { YAML: { parse(source: string): unknown } } }).Bun;
    const disposition = '{"schema":1,"disposition":"not-requested-product-first"}\n';
    const sentinel = 'old private simulator system-log payload sentinel';
    const filesUnder = async (directory: string): Promise<string[]> => {
      const entries = await readdir(directory, {withFileTypes: true});
      const nested = await Promise.all(entries.map(entry => entry.isDirectory()
        ? filesUnder(join(directory, entry.name))
        : [join(directory, entry.name)]));
      return nested.flat();
    };
    for (const path of ['.github/actions/mobile-device-run/action.yml']) {
      const document = runtime.YAML.parse(await readFile(repositoryPath(path), 'utf8')) as any;
      const groups = document.runs ? [document.runs.steps] : Object.values(document.jobs).map((job: any) => job.steps || []);
      const copies = groups.flat().filter((step: any) => step.name === 'Sanitize bounded diagnostics');
      assert.equal(copies.length, 1, path);
      const step = copies[0];
      assert.equal(step.if, 'always()');
      assert.equal(step.run.includes('ios-wda-system-log-disposition.json'), true);
      assert.equal(step.run.includes('$RUNNER_TEMP/ios-wda-system.log'), false);
      assert.equal(step.run.includes('xcrun'), false);
      const root = await mkdtemp(join(tmpdir(), 'herdr-xctest-disposition-'));
      const state = join(root, 'state');
      const output = join(root, 'output');
      const runnerTemp = join(root, 'runner-temp');
      try {
        await Promise.all([mkdir(state), mkdir(output), mkdir(runnerTemp)]);
        await writeFile(join(state, 'ios-wda-system-log-disposition.json'), disposition);
        await writeFile(join(state, 'ios-wda-system.log'), sentinel);
        await writeFile(join(runnerTemp, 'ios-wda-system.log'), sentinel);
        await command('bash', ['-c', step.run], 10_000, {
          cwd: repositoryPath('.'),
          env: {...process.env, MOBILE_PLATFORM: 'ios', MOBILE_OUTPUT: output, IOS_XCTEST_STATE_DIR: state, RUNNER_TEMP: runnerTemp},
        });
        assert.equal(await readFile(join(output, 'ios-xctest', 'ios-wda-system-log-disposition.json'), 'utf8'), disposition);
        const uploaded = await filesUnder(output);
        assert.equal(uploaded.some(file => file.endsWith('/ios-wda-system.log')), false);
        for (const file of uploaded) assert.equal((await readFile(file, 'utf8')).includes(sentinel), false);
      } finally {
        await rm(root, {recursive: true, force: true});
      }
    }
  }],
  ['mobile Android SDK install retries three times and fails after the last attempt', async () => {
    const runtime = (globalThis as unknown as { Bun: { YAML: { parse(source: string): any } } }).Bun;
    const action = runtime.YAML.parse(await readFile(repositoryPath('.github/actions/mobile-device-run/action.yml'), 'utf8'));
    const boot = action.runs.steps.find((step: any) => step.name === 'Create and boot Android emulator');
    const retry = boot.run.match(/for attempt in 1 2 3; do[\s\S]*?\ndone/u)?.[0];
    assert.ok(retry);
    const stub = 'set -euo pipefail\nattempts=0\nandroid_system_image="system-images;android-35;google_apis;x86_64"\nsdkmanager() { attempts=$((attempts+1)); printf "install:%s:%s\\n" "$attempts" "$*" >> "$CALLS"; [ "$attempts" -ge "$SUCCEED_AT" ]; }\nsleep() { printf "pause:%s\\n" "$*" >> "$CALLS"; }\n';
    for (const succeedAt of [1, 2, 3, 4]) {
      const root = await mkdtemp(join(tmpdir(), 'herdr-sdk-retry-'));
      try {
        const calls = join(root, 'calls');
        const run = () => command('bash', ['-c', stub + retry], 10_000, { env: { ...process.env, CALLS: calls, SUCCEED_AT: String(succeedAt) } });
        if (succeedAt <= 3) await run();
        else await assert.rejects(run, (error: unknown) => error instanceof CommandError && error.exitCode === 1);
        const expected: string[] = [];
        for (let attempt = 1; attempt <= Math.min(succeedAt, 3); attempt++) {
          expected.push('install:' + attempt + ':platform-tools emulator build-tools;35.0.0 system-images;android-35;google_apis;x86_64');
          if (attempt < Math.min(succeedAt, 3)) expected.push('pause:5');
        }
        assert.deepEqual((await readFile(calls, 'utf8')).trim().split('\n'), expected);
      } finally { await rm(root, { recursive: true, force: true }); }
    }
  }],
  ['mobile XML bootstrap avoids hosted Azure mirror stalls and propagates setup failures', async () => {
    const runtime = (globalThis as unknown as { Bun: { YAML: { parse(source: string): unknown } } }).Bun;
    const check = runtime.YAML.parse(await readFile(repositoryPath('.github/workflows/check.yml'), 'utf8')) as any;
    const ubuntuMirrors = 'https://archive.ubuntu.com/ubuntu/\tpriority:2\nhttps://security.ubuntu.com/ubuntu/\tpriority:3\n';
    for (const job of ['mobile-unit-prepare', 'mobile-unit-shards', 'mobile-unit-aggregate']) {
      const steps = check.jobs[job].steps.filter((step: any) => step.name === 'Install XML test tools');
      assert.equal(steps.length, 1);
      assert.equal(steps[0].if, undefined);
      assert.equal(steps[0]['continue-on-error'], undefined);
      for (const [mode, exitCode, operations] of [
        ['ready', 0, ['mirror', 'update', 'install']],
        ['mirror-failure', 1, ['mirror']],
        ['update-failure', 100, ['mirror', 'update']],
        ['update-timeout', 124, ['mirror', 'update']],
        ['install-failure', 100, ['mirror', 'update', 'install']],
        ['install-timeout', 124, ['mirror', 'update', 'install']],
      ] as const) {
        const root = await mkdtemp(join(tmpdir(), 'herdr-xml-bootstrap-'));
        let passed = false;
        try {
          await writeFile(join(root, 'mirrors'), `http://azure.archive.ubuntu.com/ubuntu/\tpriority:1\n${ubuntuMirrors}`);
          await writeFile(join(root, 'calls'), '');
          await writeFile(join(root, 'sudo'), `#!${process.execPath}
import assert from 'node:assert/strict';
import { appendFileSync, readFileSync, writeFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
const args = process.argv.slice(2);
const root = process.env.FIXTURE;
const mode = process.env.MODE;
const record = (name) => appendFileSync(root + '/calls', name + '\\n');
if (args[0] === 'sed') {
  record('mirror');
  assert.equal(args.length, 4);
  assert.equal(args[1], '-i');
  assert.equal(args[3], '/etc/apt/apt-mirrors.txt');
  if (mode === 'mirror-failure') process.exit(1);
  writeFileSync(root + '/mirrors', execFileSync('/usr/bin/sed', [args[2], root + '/mirrors']));
  process.exit(0);
}
if (readFileSync(root + '/mirrors', 'utf8').includes('azure.archive.ubuntu.com')) {
  console.error('simulated hosted Azure mirror stall');
  process.exit(124);
}
assert.deepEqual(args.slice(0, 4), ['timeout', '--kill-after=5s', '120s', 'apt-get']);
assert.deepEqual(args.slice(4, 10), ['-o', 'Acquire::http::Timeout=15', '-o', 'Acquire::https::Timeout=15', '-o', 'Acquire::Retries=1']);
const update = args.at(-1) === 'update';
assert.deepEqual(args.slice(10), update ? ['-o', 'APT::Update::Error-Mode=any', 'update'] : ['install', '-y', 'libxml2-utils']);
const operation = update ? 'update' : 'install';
record(operation);
if (mode === operation + '-failure') process.exit(100);
if (mode === operation + '-timeout') process.exit(124);
`, { mode: 0o700 });
          const run = () => command('/bin/bash', ['--noprofile', '--norc', '-e', '-o', 'pipefail', '-c', steps[0].run], 5_000, {
            cwd: root,
            env: { PATH: root, FIXTURE: root, MODE: mode, BUN_OPTIONS: '--no-env-file' },
          });
          if (exitCode === 0) await run();
          else await assert.rejects(run, (error: unknown) => error instanceof CommandError && error.exitCode === exitCode && !error.timedOut);
          assert.deepEqual((await readFile(join(root, 'calls'), 'utf8')).trim().split('\n'), operations, `${job}/${mode}`);
          if (mode !== 'mirror-failure') assert.equal(await readFile(join(root, 'mirrors'), 'utf8'), ubuntuMirrors);
          passed = true;
        } finally {
          if (passed) await rm(root, { recursive: true, force: true });
          else console.error(`XML bootstrap fixture retained: ${root}`);
        }
      }
    }
  }],
  ['mobile workflow latest-release qualification and aggregation', async () => {
    const runtime = (globalThis as unknown as { Bun: { YAML: { parse(source: string): unknown } } }).Bun;
    const parse = async (path: string) => runtime.YAML.parse(await readFile(repositoryPath(path), 'utf8')) as any;
    const workflow = await parse('.github/workflows/mobile-ci.yml'), action = await parse('.github/actions/mobile-device-run/action.yml'), release = await parse('.github/workflows/release.yml'), check = await parse('.github/workflows/check.yml');
    const shardSource = await readFile(repositoryPath('tests/mobile/unit/test-shard-ci.ts'), 'utf8');
    audit(workflow, action, release, check, shardSource);
    const healthy: Row[] = [{ platform: 'android', product: 'PASS', environment: 'PASS', complete: true, artifact: true }, { platform: 'ios', product: 'PASS', environment: 'NOT_APPLICABLE', complete: true, artifact: true }];
    assert.equal(simulate(workflow, healthy).gate.outputs.qualification_status, 'PASS');
    for (const prerequisite of needed.filter(name => name !== 'gate')) for (const status of ['failure', 'cancelled', 'skipped'] as const) {
      const result = simulate(workflow, healthy, { [prerequisite]: status });
      assert.equal(result.gate.result, 'failure', prerequisite + '/' + status);
      assert.notEqual(result.gate.outputs.qualification_status, 'PASS');
    }
    for (const environment of ['FAIL', 'UNKNOWN'] as const) {
      const rows = structuredClone(healthy); rows[0].environment = environment;
      const result = simulate(workflow, rows);
      assert.equal(result.device.result, 'failure');
      assert.equal(result.environment_report.result, 'failure');
      assert.equal(result.environment_report.outputs.environment_status, environment);
      assert.notEqual(result.gate.outputs.qualification_status, 'PASS');
    }
    for (const index of [0, 1]) for (const mode of ['product-failure', 'product-indeterminate', 'missing-artifact', 'incomplete', 'stale'] as const) {
      const rows = structuredClone(healthy);
      if (mode === 'product-failure') rows[index].product = 'FAIL';
      if (mode === 'product-indeterminate') rows[index].product = 'INDETERMINATE';
      if (mode === 'missing-artifact') rows[index].artifact = false;
      if (mode === 'incomplete') rows[index].complete = false;
      if (mode === 'stale') rows[index].current = false;
      const result = simulate(workflow, rows);
      assert.equal(result.device.result, 'failure');
      assert.equal(result.environment_report.result, 'failure');
      assert.equal(result.environment_report.invocations[0], 'Publish bounded complete-row environment report');
      assert.equal(result.gate.result, 'failure');
      assert.notEqual(result.gate.outputs.qualification_status, 'PASS');
    }
    for (const rows of [[], [healthy[0]], [healthy[1]], [healthy[0], healthy[0]]]) {
      const outcome = simulate(workflow, rows);
      assert.equal(outcome.gate.result, 'failure');
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
    const render = (source: string, github: Context) => source.replace(/\$\{\{([^}]+)\}\}/gu, (_match, value: string) => String(expression(value, { github })));
    for (const document of [workflow, check]) {
      const pr = { event_name: 'pull_request', event: { pull_request: { number: 25 } }, ref: 'refs/pull/25/merge', run_id: 98 };
      assert.equal(expression(document.concurrency['cancel-in-progress'], { github: pr }), true);
      assert.equal(render(document.concurrency.group, pr), render(document.concurrency.group, { ...pr, sha: 'new-push', run_id: 99 }));
      assert.notEqual(render(document.concurrency.group, pr), render(document.concurrency.group, { ...pr, event: { pull_request: { number: 26 } } }));
      const push = { event_name: 'push', ref: 'refs/heads/dev', run_id: 25 };
      assert.equal(expression(document.concurrency['cancel-in-progress'], { github: push }), false);
      assert.notEqual(render(document.concurrency.group, push), render(document.concurrency.group, { ...push, sha: 'new-push', run_id: 26 }));
      assert.notEqual(render(document.concurrency.group, push), render(document.concurrency.group, pr));
    }
    const mutations: Array<(w: any, a: any, r: any, k: any) => void> = [
      w => w.jobs.device.needs.push('environment_report'),
      w => w.jobs.device.strategy['fail-fast'] = true,
      w => w.jobs.device['timeout-minutes'] = 45,
      w => w.jobs.gate.needs.pop(),
      w => w.jobs.environment_report.if = 'success()',
      w => w.jobs.gate.if = 'success()',
      w => w.jobs.gate.steps.find((s: any) => s.run?.includes('validate-evidence.ts')).run = 'bun tests/mobile/validate-evidence.ts --contract product',
      w => w.jobs.gate.steps.find((s: any) => s.run?.includes('validate-evidence.ts')).if = 'false',
      w => w.jobs.environment_report.steps.find((s: any) => s.name === 'Require strict environment qualification').run = 'true',
      w => w.jobs.gate.steps.find((s: any) => s.uses?.startsWith('actions/download-artifact@')).with.pattern = 'mobile-evidence-device-*',
      w => w.jobs.gate.steps.find((s: any) => s.uses?.startsWith('actions/checkout@')).with.ref = 'main',
      w => w.on.workflow_call.outputs.qualification_status.value = '${{ jobs.environment_report.outputs.environment_status }}',
      w => w.jobs.prepare.steps.find((s: any) => s.id === 'matrix').run = 'echo hardcoded',
      w => w.concurrency['cancel-in-progress'] = false,
      (_w, a) => a.runs.steps.find((s: any) => s.name === 'Run installed-PWA upgrade scenarios')['continue-on-error'] = true,
      (_w, a) => a.runs.steps.find((s: any) => s.name === 'Require strict Android environment qualification').run = 'true',
      (_w, a) => a.runs.steps.find((s: any) => s.uses?.startsWith('actions/upload-artifact@')).if = 'success()',
      (_w, a) => a.runs.steps.find((s: any) => s.uses?.startsWith('actions/upload-artifact@')).with['if-no-files-found'] = 'warn',
      (_w, a) => a.runs.steps.find((s: any) => s.run?.includes('android-environment.ts check')).run += ' --output overwrite.json',
      (_w, a) => a.runs.steps.find((s: any) => s.run?.includes('android-gms-settle.ts')).run += ' || true',
      (_w, a) => a.runs.steps.find((s: any) => s.run?.includes('android-gms-settle.ts')).if = 'always()',
      (_w, a) => { const i = a.runs.steps.findIndex((s: any) => s.run?.includes('android-gms-settle.ts')); a.runs.steps.push(...a.runs.steps.splice(i, 1)); },
      (_w, _a, r) => r.jobs.publish.steps.unshift({ run: 'echo publish' }),
      (_w, _a, r) => r.jobs.publish.steps[0].run = 'true',
      (_w, _a, r) => r.jobs.publish.needs.pop(),
      (_w, _a, _r, k) => k.jobs.mobile.needs.pop(),
      (_w, _a, _r, k) => k.jobs.mobile.if = 'always()',
      (_w, _a, _r, k) => k.concurrency.group = 'check-${{ github.event_name }}-${{ github.event.pull_request.number || github.ref }}',
      (_w, _a, _r, k) => k.jobs['mobile-unit-aggregate'].if = 'success()',
      (_w, _a, _r, k) => k.jobs['mobile-unit-aggregate'].needs.pop(),
      (_w, _a, _r, k) => delete k.jobs['mobile-unit-shards'].if,
      (_w, _a, _r, k) => k.jobs['mobile-unit-shards'].strategy['fail-fast'] = true,
      (_w, _a, _r, k) => k.jobs['mobile-unit-shards'].steps.find((s: any) => s.id === 'install-test-dependencies').run = 'true',
      (_w, _a, _r, k) => k.jobs['mobile-unit-aggregate'].steps.find((s: any) => s.run?.includes('test-shard-ci.ts aggregate')).run = 'echo accepted',
      (_w, _a, _r, k) => k.jobs['mobile-unit-shards'].steps.find((s: any) => s.uses?.startsWith('oven-sh/setup-bun@')).with['bun-version'] = '1.4.3',
      (_w, _a, _r, k) => delete k.jobs.release.steps.find((s: any) => s.with?.name === 'release-bundles').with.overwrite,
    ];
    for (const [index, mutate] of mutations.entries()) {
      const w = structuredClone(workflow), a = structuredClone(action), r = structuredClone(release), k = structuredClone(check);
      mutate(w, a, r, k);
      assert.throws(() => audit(w, a, r, k, shardSource), 'mutation ' + index);
    }
    const matrixCommand = workflow.jobs.prepare.steps.find((s: any) => s.id === 'matrix').run;
    const manifest = JSON.parse(await readFile(repositoryPath('tests/mobile/baselines.json'), 'utf8'));
    assert.deepEqual(manifest.baselines.map((entry: any) => entry.name), ['0.23.5', '0.23.4']);
    const newest = manifest.baselines[0], previous = manifest.baselines[1];
    const cases = [
      { baselines: manifest.baselines, candidate: newest, expected: previous.name },
      { baselines: manifest.baselines, candidate: { ...newest, version: 'next-version' }, expected: newest.name },
      { baselines: manifest.baselines, candidate: { ...newest, script: '/changed.js' }, expected: newest.name },
      { baselines: manifest.baselines, candidate: { ...newest, style: '/changed.css' }, expected: newest.name },
      { baselines: manifest.baselines, candidate: { ...newest, assets: newest.assets + 1, build: 'changed', entry: '/changed.html' }, expected: previous.name },
      { baselines: [previous, newest], candidate: newest, expected: previous.name },
      { baselines: [newest, { ...newest, name: previous.name }], candidate: newest, error: /no distinct release baseline/u },
      { baselines: [], candidate: newest, error: /manifest is empty or invalid/u },
      { baselines: [{ ...newest, name: '../invalid' }, previous], candidate: newest, error: /invalid release baseline name/u },
      { baselines: [newest, { ...previous, name: 'invalid name' }], candidate: { ...newest, script: '/changed.js' }, error: /invalid release baseline name/u },
    ];
    for (const { baselines, candidate, expected, error } of cases) {
      const root = await mkdtemp(join(tmpdir(), 'herdr-mobile-matrix-'));
      try {
        await mkdir(join(root, 'tests/mobile'), { recursive: true });
        await writeFile(join(root, 'tests/mobile/baselines.json'), JSON.stringify({ schema: 1, baselines }));
        await mkdir(join(root, 'web'));
        await writeFile(join(root, 'web/version.json'), JSON.stringify(candidate));
        const output = join(root, 'output');
        const run = () => command('bash', ['-c', matrixCommand], 10_000, { cwd: root, env: { ...process.env, GITHUB_OUTPUT: output } });
        if (error) {
          await assert.rejects(run, (failure: unknown) => failure instanceof CommandError && error.test(failure.stderr));
          assert.equal(await readFile(output, 'utf8'), '');
          continue;
        }
        await run();
        const outputs = Object.fromEntries((await readFile(output, 'utf8')).trim().split('\n').map(line => {
          const separator = line.indexOf('=');
          return [line.slice(0, separator), line.slice(separator + 1)];
        }));
        assert.equal(outputs.baseline, expected);
        assert.deepEqual(JSON.parse(outputs.device_matrix).include, [
          { platform: 'android', runner: 'ubuntu-24.04', baseline: expected, scenario: 'historical' },
          { platform: 'ios', runner: 'xcode-27', baseline: expected, scenario: 'historical' },
        ]);
      } finally { await rm(root, { recursive: true, force: true }); }
    }
    for (const unsupported of ['contains(inputs.suite, \'release\')', 'needs.*.result', 'inputs.suite + 1', 'failure()']) assert.throws(() => expression(unsupported, { needs: {}, inputs: { suite: 'release' } }));
  }],
];
