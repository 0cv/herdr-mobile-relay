import assert from 'node:assert/strict';
import { cp, mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { spawnSync } from 'node:child_process';
import { repositoryPath } from '../support/paths';
import { observationsFixture, environmentFixture, measurementIdentity } from './android-product-environment';
import { assessAndroidEnvironment } from '../android-environment';

const fixtureSource = `
import { appendFileSync, mkdirSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
export const mode = process.env.RUN_PROTOCOL_CASE;
export const trace = (event) => appendFileSync(process.env.RUN_PROTOCOL_TRACE, event + '\\n');
export const origin = 'https://fixture.test:52101';
export const identity = (candidate) => ({ version: candidate ? '0.21.0' : '0.20.10', assets: candidate ? 370 : 363,
  build: (candidate ? 'd' : 'e').repeat(64), entry: candidate ? '/new/index.html' : '/old/index.html',
  script: candidate ? '/new.js' : '/old.js', style: candidate ? '/new.css' : '/old.css',
  scriptSha256: candidate ? 'new-script' : 'old-script', styleSha256: candidate ? 'new-style' : 'old-style',
  webHash: (candidate ? 'c' : 'f').repeat(64), descriptor: true });
export const state = { active_release: 'old', app_url: origin, requests: [], faults: [], relays: ['alpha','beta'].map(name => ({
  name, invitation_auth_count: 1, credential_auth_count: 1, credential_pseudonyms: [name + '-credential'], connections: 1,
  install_update_count: 0, deploy_app_update_count: 0 })) };
export const info = { app_url: origin, relay_urls: ['wss://fixture.test:52102','wss://fixture.test:52103'],
  setup_urls: [origin + '/setup/alpha', origin + '/setup/beta'], control_url: 'http://fixture.invalid', control_secret: 'private',
  ca_certificate: '', old_release: 'old', candidate_release: 'candidate' };
export const startFixture = (args) => {
  const file = args[args.indexOf('-info-file') + 1];
  mkdirSync(dirname(file), {recursive:true}); writeFileSync(file, JSON.stringify(info)); trace('fixture:start'); return {};
};
globalThis.fetch = async (input, init) => {
  const url = new URL(String(input));
  if (url.origin !== 'http://fixture.invalid') throw new Error('unexpected network request');
  const body = init?.body ? JSON.parse(String(init.body)) : {};
  trace('control:' + url.pathname);
  if (url.pathname === '/shutdown' && mode.includes('shutdown-failure')) throw Error('SHUTDOWN_FAILED');
  if (url.pathname === '/relay/drop') state.relays.find(r => r.name === url.searchParams.get('name')).credential_auth_count++;
  if (url.pathname === '/activate') state.active_release = 'candidate';
  if (url.pathname === '/fault') { state.faults.push(body); state.requests.push({method:'GET',path:body.path,fault:body.kind,fault_id:body.id,fault_generation:body.generation,release:'candidate'}); }
  if (url.pathname === '/fault/clear') state.faults = [];
  return Response.json(url.pathname === '/state' ? state : {});
};
export const observations = ${JSON.stringify(observationsFixture())};
export const releaseObservations = ${JSON.stringify(observationsFixture(measurementIdentity({suite:'release'}), '42', {id:'cold-1',measurementId:'android-test',packageName:'com.android.chrome',pid:'42',processes:{'42':'com.android.chrome'},command:['shell','am','force-stop','--user','0','com.android.chrome'],succeeded:true}))};
export const assessment = ${JSON.stringify(assessAndroidEnvironment(environmentFixture(), measurementIdentity()))};
`;
const platformSource = `
import { trace, mode, origin, state, identity, observations, releaseObservations } from '../fixture-protocol';
export class AndroidPlatform {
  name = process.env.MOBILE_PLATFORM;
  selected = true; driver = { unusable:false }; cold = false;
  constructor(options) { this.options = options; }
  activateProductRecorder(identity) { this.template = structuredClone(identity.suite === 'release' ? releaseObservations : observations); this.template.identity = identity; this.proof = structuredClone(this.template); this.proof.records = this.proof.records.slice(0,1); trace('recorder:activate'); }
  productCheckpoint(name) { this.proof.records = structuredClone(this.template.records.slice(0,this.template.records.findIndex(record=>record.kind==='checkpoint'&&record.name===name)+1)); }
  async startFreshDevice() { trace('fresh:begin'); if (mode.includes('fresh-failure')) throw new Error('FRESH_FAILED: original'); trace('fresh:end'); }
  async openSetupURL() { trace('invitation'); if (mode === 'sticky-driver-failure') { this.driver.firstFatal={code:'APPIUM_TIMEOUT',operation:'first-inspection',later200:true}; this.driver.unusable=true; } if (mode.includes('scenario-failure')) throw new Error('STANDALONE_ORIGINAL: invitation failure'); if (mode.includes('fatal-failure')) { this.driver.firstFatal={code:'APPIUM_TIMEOUT'}; this.driver.unusable=true; const error = new Error('APPIUM_TIMEOUT: original'); error.code='APPIUM_TIMEOUT'; throw error; } }
  async installFromBrowser() { trace('install'); }
  async launchInstalledApp() { trace('launch'); }
  async clickWebText(text) { trace('click:' + text); if (text === 'Try again' && mode.includes('recovery-failure')) throw new Error('ORIGIN_ORIGINAL: recovery failure'); }
  async clickDialogText(_dialog, text) { trace('dialog:' + text); }
  async assertStandalone() { return this.readRunningIdentity(); }
  async readRunningIdentity() { const failed = state.faults.length > 0; return { ...identity(state.active_release === 'candidate'),
    url: origin + '/', origin, standalone:true, provider: this.name === 'ios' ? 'ios-home-screen' : 'android-standalone',
    nativeProvider: this.name === 'ios' ? 'ios:com.apple.webapp' : 'android:com.android.chrome', nativePid:this.cold ? '43' : '42',
    nativeActivity:'org.chromium.chrome.browser.webapps.WebappActivity', navigationId:this.cold ? 'two' : 'one', buildFromApplication:true,
    requiredAssetsReady:!failed, requiredAssetFailure:failed, failureUiVisible:failed, applicationInitialized:true }; }
  async readUpdateCompletion() { const loaded = state.faults.length === 0; return { phoneRequired:true, phoneAcknowledged:loaded, phoneState: loaded ? 'loaded' : 'failed', visibleCompletion:loaded, rawPlanPresent:true }; }
  async openSetupURLInInstalledApp() { trace('second-invitation'); }
  async setPreference() { trace('preference'); }
  async preferenceValue() { return 'state'; }
  async captureSanitizedEvidence(label) { trace('evidence:' + label); }
  evidenceSnapshot() { trace('snapshot:platform'); return { driver:this.driver, selectedInstalledWindowValid:this.selected, productObservations:this.proof }; }
  async relaunchInstalledApp() { trace('relaunch'); if (!this.pendingCold) return; this.pendingCold=false; this.cold=true; }
  async backgroundApp() { trace('background'); }
  async terminateInstalledApp() { trace('terminate'); this.pendingCold=true; if (this.name === 'android') await this.environmentMeasurement.terminate('com.android.chrome', '42'); }
  async openFixtureAgent() { trace('agent'); }
  async showKeyboardOnComposer() { trace('keyboard'); }
  async hideKeyboard() { trace('hide-keyboard'); }
  async stopOwnedResources() { trace('cleanup:platform'); if (mode === 'late-driver-failure') { this.driver.firstFatal={code:'APPIUM_TIMEOUT',operation:'teardown'}; this.driver.unusable=true; } this.selected=false; if(this.proof) this.proof.records.length=0; if (mode.includes('cleanup-failure')) throw new Error('CLEANUP_FAILED'); }
}
`;
export const runnerProtocolCases = ['success', 'component-drift', 'unknown-relation', 'fresh-failure', 'baseline-failure', 'baseline-failure-cleanup-failure', 'scenario-failure', 'fatal-failure', 'post-failure', 'scenario-failure-post-failure', 'recovery-failure-post-failure', 'scenario-failure-post-failure-cleanup-failure', 'cleanup-failure', 'shutdown-failure', 'fixture-stop-failure', 'reverse-acquire-failure', 'reverse-remove-failure', 'fixture-log-failure', 'private-failure', 'event-write-failure', 'result-write-failure', 'sticky-driver-failure', 'late-driver-failure'] as const;

async function replay(mode: string, platform: string, suite: string): Promise<void> {
  const root = await mkdtemp(join(tmpdir(), 'herdr-scenario-protocol-'));
  const source = process.env.MOBILE_RUN_TEST_SOURCE || repositoryPath('tests/mobile');
  await mkdir(join(root, 'platforms')); await mkdir(join(root, 'android-appium'));
  await cp(join(source, 'support'), join(root, 'support'), { recursive: true });
  await cp(join(source, 'android-appium/kernel-namespace.cjs'), join(root, 'android-appium/kernel-namespace.cjs'));
  await cp(join(source, 'run.ts'), join(root, 'run.ts'));
  assert.equal(await readFile(join(root, 'run.ts'), 'utf8'), await readFile(join(source, 'run.ts'), 'utf8'));
  await writeFile(join(root, 'fixture-protocol.ts'), fixtureSource);
  await writeFile(join(root, 'platforms/android.ts'), platformSource);
  await writeFile(join(root, 'platforms/ios.ts'), platformSource.replace('class AndroidPlatform', 'class IOSPlatform'));
  await writeFile(join(root, 'support/process.ts'), `
import { trace, startFixture, mode } from '../fixture-protocol';
export const startCommand = (_file, args) => startFixture(args);
export const command = async (_file, args) => { trace('command:' + args.join(' ')); if(mode.includes('reverse-acquire-failure') && args.includes('tcp:52102') && !args.includes('--remove')) throw Error('REVERSE_ACQUIRE_FAILED'); if(mode.includes('reverse-remove-failure') && args.includes('--remove') && args.includes('tcp:52101')) throw Error('REVERSE_REMOVE_FAILED'); return { stdout:'', stderr:'' }; };
export const stopProcess = async () => { trace('cleanup:fixture'); if(mode.includes('fixture-stop-failure')) throw Error('FIXTURE_STOP_FAILED'); };
`);
  await writeFile(join(root, 'android-measurement.ts'), `
import { trace, mode, assessment } from './fixture-protocol';
export class AndroidEnvironmentMeasurement {
  id='android-test'; operations=[]; errors=[];
  bind(identity) { this.identity=identity; }
  failureOutcome() { return { errors:this.errors }; }
  plannedOperations() { return structuredClone(this.operations); }
  async begin() { trace('measurement:begin'); if (mode.includes('baseline-failure')) { const error=new Error('ANDROID_ENVIRONMENT: baseline failure'); this.errors.push(error,new Error('COLLECTOR_CLOSE_FAILED')); throw error; } }
  async finish() { trace('measurement:finish'); const result=structuredClone(assessment); result.identity=this.identity; result.plannedOperations=this.plannedOperations();
    if(mode.includes('component-drift')) { result.status='FAIL'; result.findings=[{category:'GMS_COMPONENT_DRIFT'}]; }
    if(mode.includes('unknown-relation')) { result.status='FAIL'; result.findings=[{category:'ADVERSE_DEATH',eventIndex:0}]; result.nativeEvents=[{kind:'process-death',pid:'5820',line:'SIG: 9'}]; }
    if(mode.includes('post-failure')) { result.collection.status='UNKNOWN'; result.status='UNKNOWN'; result.findings=[{category:'EVIDENCE_INVALID'}]; }
    return { assessment:result,assessmentSha256:'a'.repeat(64), errors:mode.includes('post-failure')?[new Error('ANDROID_ENVIRONMENT: original postcheck failure')]:[] };
  }
  async terminate(packageName, pid) { trace('measurement:terminate:' + packageName + ':' + pid); this.operations.push({id:'cold-1',measurementId:this.id,packageName,pid,processes:{[pid]:packageName},command:['shell','am','force-stop','--user','0',packageName],succeeded:true}); }
}
`);
  await writeFile(join(root, 'preload.ts'), `
import { mock } from 'bun:test';
import * as fs from 'node:fs/promises';
import { trace, mode } from './fixture-protocol';
const original={...fs};
mock.module('node:fs/promises',()=>({...original,
  writeFile:async(path,...args)=>{const name=String(path).split('/').at(-1);trace('write:'+name); if ((mode.includes('fixture-log-failure')&&name==='fixture.log')||(mode.includes('event-write-failure')&&name==='scenario-events.json')||(mode.includes('result-write-failure')&&name==='mobile-result.json')) throw Error('WRITE_FAILED:'+name);return original.writeFile(path,...args);},
  rm:async(path,...args)=>{trace('remove:'+String(path).split('/').at(-1));if(mode.includes('private-failure')&&String(path).endsWith('/private'))throw Error('PRIVATE_REMOVE_FAILED');return original.rm(path,...args);}
}));
`);
  const bundle = (candidate: boolean) => ({ name: candidate ? 'candidate-current' : '0.20.10', root: '.', provenance: { sourceCommit: 'a'.repeat(40) }, identity: {
    version: candidate ? '0.21.0' : '0.20.10', assets: candidate ? 370 : 363, build: (candidate ? 'd' : 'e').repeat(64), entry: candidate ? '/new/index.html' : '/old/index.html', script: candidate ? '/new.js' : '/old.js', style: candidate ? '/new.css' : '/old.css', scriptSha256: candidate ? 'new-script' : 'old-script', styleSha256: candidate ? 'new-style' : 'old-style', webHash: (candidate ? 'c' : 'f').repeat(64), descriptor: true } });
  await writeFile(join(root, 'bundles.json'), JSON.stringify({ schema: 1, baselines: [bundle(false)], candidate: bundle(true) }));
  const traceFile = join(root, 'trace.log');
  const child = spawnSync(process.execPath, ['--preload', join(root, 'preload.ts'), join(root, 'run.ts'), '--bundle-set', join(root, 'bundles.json'), '--output', join(root, 'output'), '--private-output', join(root, 'private'), '--suite', suite], {
    env: { ...process.env, MOBILE_PLATFORM: platform, MOBILE_SOURCE_RUN_HEAD_SHA: 'b'.repeat(40), GITHUB_RUN_ID: '35556703407', GITHUB_RUN_ATTEMPT: '2', ANDROID_SERIAL: 'emulator-protocol', RUN_PROTOCOL_CASE: mode, RUN_PROTOCOL_TRACE: traceFile }, encoding: 'utf8', timeout: 20_000,
  });
  await writeFile(join(root, 'command.json'), JSON.stringify({ status: child.status, stdout: child.stdout, stderr: child.stderr, error: String(child.error || '') }, null, 2));
  assert.equal(child.error, undefined);
  const events = (await readFile(traceFile, 'utf8')).trim().split('\n');
  const before = (first: string, last: string) => assert.ok(events.includes(first) && events.includes(last) && events.indexOf(first) < events.indexOf(last), `${mode}: ${first} before ${last}: ${events}`);
  const resultMissing = mode === 'result-write-failure';
  const result = resultMissing ? undefined : JSON.parse(await readFile(join(root, 'output/mobile-result.json'), 'utf8'));
  const failed = mode.includes('failure') || mode === 'unknown-relation';
  assert.equal(child.status, failed ? 1 : 0, JSON.stringify({ result, stderr:child.stderr, events }));
  before('cleanup:fixture', 'write:fixture.log'); before('write:fixture.log', 'remove:private'); before('remove:private', 'write:scenario-events.json'); before('write:scenario-events.json', 'write:mobile-result.json');
  if (resultMissing) { await assert.rejects(readFile(join(root, 'output/mobile-result.json'))); return; }
  assert.equal(result.schema, 2);
  const measured = platform === 'android' && !/fresh-failure|baseline-failure|reverse-acquire-failure/u.test(mode);
  assert.equal(events.filter(event => event === 'measurement:finish').length, measured ? 1 : 0);
  if (measured) { before('fresh:end','measurement:begin'); before('measurement:begin','invitation'); before('measurement:finish','cleanup:platform'); }
  if (/fresh-failure|baseline-failure|reverse-acquire-failure/u.test(mode)) assert.ok(!events.includes('invitation'));
  if (mode === 'component-drift') { assert.equal(result.product.status,'PASS'); assert.equal(result.environment_qualification.status,'FAIL'); }
  if (mode === 'unknown-relation') assert.equal(result.product.status,'INDETERMINATE');
  if (/scenario-failure|recovery-failure/u.test(mode)) { assert.equal(result.primary_failure.message, mode.includes('recovery-failure') ? 'ORIGIN_ORIGINAL: recovery failure' : 'STANDALONE_ORIGINAL: invitation failure'); assert.equal(result.qualification_failure.code, mode.includes('recovery-failure') ? 'ORIGIN_ORIGINAL' : 'STANDALONE_ORIGINAL'); }
  if (mode.includes('baseline-failure')) { assert.equal(result.primary_failure.message,'ANDROID_ENVIRONMENT: baseline failure'); assert.ok(result.additional_failures.some((error:any)=>error.message==='COLLECTOR_CLOSE_FAILED')); }
  if (mode === 'fatal-failure') { assert.equal(result.evidence.driver.firstFatal.code,'APPIUM_TIMEOUT'); assert.equal(events.includes('install'),false); }
  if (mode === 'sticky-driver-failure') { assert.equal(result.product.status,'FAIL'); assert.equal(result.evidence.driver.firstFatal.operation,'first-inspection'); assert.equal(result.evidence.driver.firstFatal.later200,true); }
  if (mode === 'late-driver-failure') { assert.equal(result.product.status,'PASS'); assert.equal(result.finalization.status,'FAIL'); assert.equal(result.evidence.driver.firstFatal,null); assert.equal(result.evidence.teardown.driver.firstFatal.operation,'teardown'); }
  if (mode === 'reverse-acquire-failure') { assert.ok(events.includes('command:-s emulator-protocol reverse --remove tcp:52101')); assert.ok(!events.includes('command:-s emulator-protocol reverse --remove tcp:52102')); }
  if (mode === 'reverse-remove-failure') for (const port of ['52101','52102','52103']) assert.ok(events.includes('command:-s emulator-protocol reverse --remove tcp:'+port));
  if (['cleanup-failure','shutdown-failure','fixture-stop-failure','reverse-remove-failure','fixture-log-failure','private-failure','event-write-failure'].includes(mode)) { assert.equal(result.product.status,'PASS'); assert.equal(result.finalization.status,'FAIL'); }
  if (!failed) { assert.equal(result.product.status,'PASS'); assert.equal(result.finalization.status,'PASS'); assert.equal(result.evidence.selectedInstalledWindowValid,true); assert.equal(result.evidence.teardown.selectedInstalledWindowValid,false); if(platform==='android')assert.ok(result.evidence.productObservations.records.length>0); }
}
export const scenarioRunnerTests: Array<[string, () => Promise<void>]> = [
  ...runnerProtocolCases.map(mode => [`production runner Android ${mode} preserves the preparation/measurement/cleanup contract`, () => replay(mode, 'android', 'smoke')] as [string, () => Promise<void>]),
  ['production runner release records the lifecycle operation without rebaseline', () => replay('success', 'android', 'release')],
  ['production runner iOS never starts Android measurement', () => replay('success', 'ios', 'release')],
];
