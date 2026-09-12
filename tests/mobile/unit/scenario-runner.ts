import assert from 'node:assert/strict';
import { cp, mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { spawnSync } from 'node:child_process';
import { repositoryPath } from '../support/paths';

const fixtureSource = `
import { appendFileSync, mkdirSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
export const mode = process.env.RUN_PROTOCOL_CASE;
export const trace = (event) => appendFileSync(process.env.RUN_PROTOCOL_TRACE, event + '\\n');
export const origin = 'https://fixture.test:52101';
export const identity = (candidate) => ({ version: candidate ? '0.21.0' : '0.20.10', assets: candidate ? 370 : 363,
  build: candidate ? 'new-build' : 'old-build', entry: candidate ? '/new/index.html' : '/old/index.html',
  script: candidate ? '/new.js' : '/old.js', style: candidate ? '/new.css' : '/old.css',
  scriptSha256: candidate ? 'new-script' : 'old-script', styleSha256: candidate ? 'new-style' : 'old-style',
  webHash: candidate ? 'new-web' : 'old-web', descriptor: true });
export const state = { active_release: 'old', app_url: origin, requests: [], faults: [], relays: ['alpha','beta'].map(name => ({
  name, invitation_auth_count: 1, credential_auth_count: 1, credential_pseudonyms: [name + '-credential'], connections: 1,
  install_update_count: 0, deploy_app_update_count: 0 })) };
export const info = { app_url: origin, relay_urls: ['wss://fixture.test:52102','wss://fixture.test:52103'],
  setup_urls: [origin + '/setup/alpha', origin + '/setup/beta'], control_url: 'http://fixture.invalid', control_secret: 'private',
  ca_certificate: '', old_release: 'old', candidate_release: 'candidate' };
export const startFixture = (args) => {
  const file = args[args.indexOf('-info-file') + 1];
  mkdirSync(dirname(file), {recursive:true}); writeFileSync(file, JSON.stringify(info)); trace('fixture:start');
  return {};
};
globalThis.fetch = async (input, init) => {
  const url = new URL(String(input));
  if (url.origin !== 'http://fixture.invalid') throw new Error('unexpected network request');
  const body = init?.body ? JSON.parse(String(init.body)) : {};
  trace('control:' + url.pathname);
  if (url.pathname === '/relay/drop') state.relays.find(r => r.name === url.searchParams.get('name')).credential_auth_count++;
  if (url.pathname === '/activate') state.active_release = 'candidate';
  if (url.pathname === '/fault') {
    state.faults.push(body);
    state.requests.push({method:'GET',path:body.path,fault:body.kind,fault_id:body.id,fault_generation:body.generation,release:'candidate'});
  }
  if (url.pathname === '/fault/clear') state.faults = [];
  return Response.json(url.pathname === '/state' ? state : {});
};
`;

const platformSource = `
import { trace, mode, origin, state, identity } from '../fixture-protocol';
export class AndroidPlatform {
  name = process.env.MOBILE_PLATFORM;
  constructor(options) { this.options = options; }
  async startFreshDevice() { trace('fresh:begin'); if (mode.includes('fresh-failure')) throw new Error('FRESH_FAILED: original'); trace('fresh:end'); }
  async openSetupURL() { trace('invitation'); if (mode.includes('scenario-failure')) throw new Error('STANDALONE_ORIGINAL: invitation failure'); }
  async installFromBrowser() { trace('install'); }
  async launchInstalledApp() { trace('launch'); }
  async clickWebText(text) { trace('click:' + text); if (text === 'Try again' && mode.includes('recovery-failure')) throw new Error('ORIGIN_ORIGINAL: recovery failure'); }
  async clickDialogText(_dialog, text) { trace('dialog:' + text); }
  async assertStandalone() { return this.readRunningIdentity(); }
  async readRunningIdentity() { const failed = state.faults.length > 0; return { ...identity(state.active_release === 'candidate'),
    url: origin + '/', origin, standalone:true, provider: this.name === 'ios' ? 'ios-home-screen' : 'android-standalone',
    nativeProvider: this.name === 'ios' ? 'ios:com.apple.webapp' : 'android:com.android.chrome', nativePid:'42',
    nativeActivity:'org.chromium.chrome.browser.webapps.WebappActivity', navigationId:'one', buildFromApplication:true,
    requiredAssetsReady:!failed, requiredAssetFailure:failed, failureUiVisible:failed, applicationInitialized:true }; }
  async readUpdateCompletion() { const loaded = state.faults.length === 0; return { phoneRequired:true, phoneAcknowledged:loaded,
    phoneState: loaded ? 'loaded' : 'failed', visibleCompletion:loaded, rawPlanPresent:true }; }
  async openSetupURLInInstalledApp() { trace('second-invitation'); }
  async setPreference() { trace('preference'); }
  async preferenceValue() { return 'state'; }
  async captureSanitizedEvidence(label) { trace('evidence:' + label); }
  evidenceSnapshot() { return { protocol: 'hypothetical platform and fixture; actual production runner' }; }
  async relaunchInstalledApp() { trace('relaunch'); }
  async backgroundApp() { trace('background'); }
  async terminateInstalledApp() { trace('terminate'); if (this.name === 'android') await this.environmentMeasurement?.terminate('com.android.chrome', '42'); }
  async openFixtureAgent() { trace('agent'); }
  async showKeyboardOnComposer() { trace('keyboard'); }
  async hideKeyboard() { trace('hide-keyboard'); }
  async stopOwnedResources() { trace('cleanup:platform'); if (mode.includes('cleanup-failure')) throw new Error('CLEANUP_FAILED'); }
}
`;

async function replay(mode: string, platform: string, suite: string): Promise<void> {
  const root = await mkdtemp(join(tmpdir(), 'herdr-scenario-protocol-'));
  const source = process.env.MOBILE_RUN_TEST_SOURCE || repositoryPath('tests/mobile');
  await mkdir(join(root, 'platforms'));
  await cp(join(source, 'support'), join(root, 'support'), { recursive: true });
  await cp(join(source, 'run.ts'), join(root, 'run.ts'));
  assert.equal(await readFile(join(root, 'run.ts'), 'utf8'), await readFile(join(source, 'run.ts'), 'utf8'));
  await writeFile(join(root, 'fixture-protocol.ts'), fixtureSource);
  await writeFile(join(root, 'platforms/android.ts'), platformSource);
  await writeFile(join(root, 'platforms/ios.ts'), platformSource.replace('class AndroidPlatform', 'class IOSPlatform'));
  await writeFile(join(root, 'support/process.ts'), `
import { trace, startFixture } from '../fixture-protocol';
export const startCommand = (_file, args) => startFixture(args);
export const command = async (_file, args) => { trace('command:' + args.join(' ')); return { stdout:'', stderr:'' }; };
export const stopProcess = async () => { trace('cleanup:fixture'); };
`);
  await writeFile(join(root, 'android-measurement.ts'), `
import { trace, mode } from './fixture-protocol';
export class AndroidEnvironmentMeasurement {
  async begin() { trace('measurement:begin'); if (mode.includes('baseline-failure')) throw new Error('ANDROID_ENVIRONMENT: baseline failure'); }
  async finish() { trace('measurement:finish'); if (mode.includes('post-failure')) throw new Error('ANDROID_ENVIRONMENT: original postcheck failure'); }
  async terminate(packageName, pid) { trace('measurement:terminate:' + packageName + ':' + pid); }
}
`);
  const bundle = (candidate: boolean) => ({ name: candidate ? 'candidate' : 'baseline', root: '.', provenance: { sourceCommit: 'protocol-source' }, identity: {
    version: candidate ? '0.21.0' : '0.20.10', assets: candidate ? 370 : 363, build: candidate ? 'new-build' : 'old-build',
    entry: candidate ? '/new/index.html' : '/old/index.html', script: candidate ? '/new.js' : '/old.js', style: candidate ? '/new.css' : '/old.css',
    scriptSha256: candidate ? 'new-script' : 'old-script', styleSha256: candidate ? 'new-style' : 'old-style', webHash: candidate ? 'new-web' : 'old-web', descriptor: true,
  } });
  await writeFile(join(root, 'bundles.json'), JSON.stringify({ schema: 1, baselines: [bundle(false)], candidate: bundle(true) }));
  const traceFile = join(root, 'trace.log');
  const child = spawnSync(process.execPath, [join(root, 'run.ts'), '--bundle-set', join(root, 'bundles.json'), '--output', join(root, 'output'), '--private-output', join(root, 'private'), '--suite', suite], {
    env: { ...process.env, MOBILE_PLATFORM: platform, ANDROID_SERIAL: 'emulator-protocol', RUN_PROTOCOL_CASE: mode, RUN_PROTOCOL_TRACE: traceFile },
    encoding: 'utf8', timeout: 20_000,
  });
  await writeFile(join(root, 'command.json'), JSON.stringify({ status: child.status, stdout: child.stdout, stderr: child.stderr, error: String(child.error || '') }, null, 2));
  assert.equal(child.error, undefined);
  const result = JSON.parse(await readFile(join(root, 'output/mobile-result.json'), 'utf8'));
  const events = (await readFile(traceFile, 'utf8')).trim().split('\n');
  const count = (event: string) => events.filter(e => e === event).length;
  const before = (first: string, last: string) => {
    assert.ok(events.includes(first) && events.includes(last) && events.indexOf(first) < events.indexOf(last), `${first} must precede ${last}: ${events.join(', ')}`);
  };
  const measured = platform === 'android' && !mode.includes('fresh-failure') && !mode.includes('baseline-failure');
  assert.equal(count('measurement:begin'), platform === 'android' && !mode.includes('fresh-failure') ? 1 : 0);
  assert.equal(count('measurement:finish'), measured ? 1 : 0);
  if (platform === 'android' && !mode.includes('fresh-failure')) before('fresh:end', 'measurement:begin');
  if (measured) {
    before('measurement:begin', 'invitation');
    before('measurement:finish', 'cleanup:platform');
    before('measurement:finish', 'control:/shutdown');
    before('measurement:finish', 'cleanup:fixture');
    before('measurement:finish', 'command:-s emulator-protocol reverse --remove tcp:52101');
  }
  if (/fresh-failure|baseline-failure/u.test(mode)) assert.equal(count('invitation'), 0);
  const failed = mode.includes('failure');
  assert.equal(child.status, failed ? 1 : 0, JSON.stringify(result));
  assert.equal(result.result, /scenario-failure|recovery-failure/u.test(mode) ? 'product failure' : failed ? 'infrastructure failure' : 'passed');
  if (/scenario-failure|recovery-failure/u.test(mode)) {
    const recovery = mode.includes('recovery-failure');
    assert.equal(result.failure, recovery ? 'ORIGIN_ORIGINAL: recovery failure' : 'STANDALONE_ORIGINAL: invitation failure');
    assert.equal(result.failure_stage, recovery ? 'upgrade' : 'device');
    assert.equal(result.qualification_failure.code, recovery ? 'ORIGIN_ORIGINAL' : 'STANDALONE_ORIGINAL');
    if (measured) before('evidence:failure', 'measurement:finish');
  }
  if (mode.includes('post-failure')) {
    assert.equal(result.android_environment_failure, 'ANDROID_ENVIRONMENT: original postcheck failure');
    if (!/scenario-failure|recovery-failure/u.test(mode)) assert.equal(result.failure_stage, 'android-environment');
    const diagnostics = JSON.parse(await readFile(join(root, 'output/scenario-events.json'), 'utf8'));
    assert.ok(diagnostics.some((e: any) => e.phase === 'android-environment' && e.operation === 'postcheck'));
    if (/scenario-failure|recovery-failure/u.test(mode)) assert.ok(diagnostics.some((e: any) => e.operation === 'scenario-failure' && /ORIGINAL/u.test(e.detail)));
  }
  if (!failed) {
    assert.ok(events.includes('click:Try again'));
    assert.ok(events.includes('relaunch'));
    if (suite === 'release') {
      assert.equal(count('terminate'), 1);
      assert.equal(count('measurement:terminate:com.android.chrome:42'), platform === 'android' ? 1 : 0);
    }
  }
}

export const scenarioRunnerTests: Array<[string, () => Promise<void>]> = [
  ...['success', 'fresh-failure', 'baseline-failure', 'scenario-failure', 'post-failure', 'scenario-failure-post-failure', 'recovery-failure-post-failure', 'scenario-failure-post-failure-cleanup-failure'].map(mode => [
    `production runner Android ${mode} preserves the preparation/measurement/cleanup contract`, () => replay(mode, 'android', 'smoke'),
  ] as [string, () => Promise<void>]),
  ['production runner release records the lifecycle operation without rebaseline', () => replay('success', 'android', 'release')],
  ['production runner iOS never starts Android measurement', () => replay('success', 'ios', 'release')],
];
