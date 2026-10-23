import assert from 'node:assert/strict';
import { cp, mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { spawnSync } from 'node:child_process';
import { repositoryPath } from '../support/paths';
import { observationsFixture, environmentFixture, measurementIdentity } from './android-product-environment';
import { assessAndroidEnvironment } from '../android-environment';
import { sanitizeValue } from '../support/diagnostics';
import { IOS_CONFIRMATION_CAPTURE_LIMITS as captureLimits, projectIOSConfirmation } from '../support/ios-confirmation-capture';
import type { WebDriverRequestTiming } from '../support/webdriver';
import { confirmationBytePressureFixture } from './ios-confirmation-capture';

type CaptureMode = 'unchanged' | 'changed' | 'teardown-only';

function confirmationCaptureFixture() {
  const receipt = (ordinal: number, scope: WebDriverRequestTiming['scope'], start: number, remaining: number): WebDriverRequestTiming => ({
    attemptId: ordinal, dispatchedOrdinal: ordinal, sessionGeneration: 7, operation: 'other', scope,
    entryMs: start, dispatchMs: start + 0.125, responseAvailableMs: start + 120,
    bodyReadStartMs: start + 120.125, bodyReadCompleteMs: start + 121,
    parseStartMs: start + 121.125, parseCompleteMs: start + 121.25,
    historyStartMs: start + 121.375, redactionCompleteMs: start + 121.5, historyCompleteMs: start + 121.625,
    settlementMs: start + 122, priorSettlementToEntryMs: 0.5, priorSettlementToDispatchMs: 0.625,
    phaseRemainingAtEntryMs: remaining, phaseRemainingAtDispatchMs: remaining - 0.125,
    phaseRemainingAtSettlementMs: remaining - 122, admitted: true, sent: true, completed: true, status: 200, outcome: 'success',
  });
  return {
    status: 'captured', acknowledgement: receipt(41, 'unscoped', 20_000, 80_000),
    source: receipt(42, 'ios-install-confirmation', 20_122.5, 74_999.5),
    projection: projectIOSConfirmation(confirmationBytePressureFixture().source),
  };
}

const fixtureSource = `
import { appendFileSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
export const mode = process.env.RUN_PROTOCOL_CASE;
export const captureMode = process.env.RUN_PROTOCOL_CAPTURE;
export const capture = captureMode ? JSON.parse(readFileSync(process.env.RUN_PROTOCOL_CAPTURE_FILE, 'utf8')) : undefined;
export const recordSnapshot = snapshot => { appendFileSync(process.env.RUN_PROTOCOL_SNAPSHOTS, JSON.stringify(snapshot) + '\\n'); return snapshot; };
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
import { trace, mode, origin, state, identity, observations, releaseObservations, captureMode, capture, recordSnapshot } from '../fixture-protocol';
export class AndroidPlatform {
  name = process.env.MOBILE_PLATFORM;
  selected = true; driver = { unusable:false }; cold = false;
  constructor(options) {
    this.options = options;
    globalThis.protocolPlatform = this;
    this.confirmationCapture = captureMode === 'teardown-only' ? undefined : structuredClone(capture);
    this.extraEvidence = captureMode ? { nativeDiagnosticsBlocked:false, launchPersistence:{receipt:'written'},
      safariNavigation:{lastDiscovery:'expected-origin-context'}, events:[{phase:'fixture',detail:{token:'synthetic-secret'}}] } : {};
    if (captureMode) Object.assign(this.driver, { sessionGeneration:7, selectedContext:'NATIVE_APP', commands:[], lastCommand:{operation:'close'} });
  }
  activateProductRecorder(identity) { this.template = structuredClone(identity.suite === 'release' ? releaseObservations : observations); this.template.identity = identity; this.proof = structuredClone(this.template); this.proof.records = this.proof.records.slice(0,1); trace('recorder:activate'); }
  productCheckpoint(name) { this.proof.records = structuredClone(this.template.records.slice(0,this.template.records.findIndex(record=>record.kind==='checkpoint'&&record.name===name)+1)); }
  async startFreshDevice() { trace('fresh:begin'); if (mode.includes('fresh-failure')) throw new Error('FRESH_FAILED: original'); trace('fresh:end'); }
  async observeGmsStage(stage) {
    if (!this.environmentMeasurement) throw new Error('ANDROID_ENVIRONMENT: GMS measurement is missing');
    if (mode === 'gms-out-of-order' && stage === 'after-device-start') stage = 'after-setup-url';
    if (mode === 'gms-missing-post-begin' && stage === 'after-initial-installed-app-launch') return;
    await this.environmentMeasurement.observeGmsStage(stage);
  }
  async openSetupURL() { trace('invitation'); if (mode === 'sticky-driver-failure') { this.driver.firstFatal={code:'APPIUM_TIMEOUT',operation:'first-inspection',later200:true}; this.driver.unusable=true; } if (mode.includes('scenario-failure')) throw new Error('STANDALONE_ORIGINAL: invitation failure'); if (mode.includes('fatal-failure')) { this.driver.firstFatal={code:'APPIUM_TIMEOUT'}; this.driver.unusable=true; const error = new Error('APPIUM_TIMEOUT: original'); error.code='APPIUM_TIMEOUT'; throw error; } }
  async installFromBrowser() { trace('install'); }
  async launchInstalledApp() { trace('launch'); }
  async clickWebText(text) { trace('click:' + text); if (text === 'Try again' && mode.includes('recovery-failure')) throw new Error('ORIGIN_ORIGINAL: recovery failure'); }
  async clickDialogText(_dialog, text) { trace('dialog:' + text); if (text === 'Close') this.progressClosed = true; }
  async assertStandalone() { return this.readRunningIdentity(); }
  async readRunningIdentity() { const failed = state.faults.length > 0; return { ...identity(state.active_release === 'candidate'),
    url: origin + '/', origin, standalone:true, provider: this.name === 'ios' ? 'ios-home-screen' : 'android-standalone',
    nativeProvider: this.name === 'ios' ? 'ios:com.apple.webapp' : 'android:com.android.chrome', nativePid:this.cold ? '43' : '42',
    nativeActivity:'org.chromium.chrome.browser.webapps.WebappActivity', navigationId:this.cold ? 'two' : 'one', buildFromApplication:true,
    requiredAssetsReady:!failed, requiredAssetFailure:failed, failureUiVisible:failed, applicationInitialized:true }; }
  async readUpdateCompletion() { if (this.progressClosed) return { phoneRequired:false, phoneAcknowledged:false, phoneState:'', visibleCompletion:false, rawPlanPresent:false }; const loaded = state.faults.length === 0; return { phoneRequired:true, phoneAcknowledged:loaded, phoneState: loaded ? 'loaded' : 'failed', visibleCompletion:loaded, rawPlanPresent:true }; }
  async openSetupURLInInstalledApp() { trace('second-invitation'); }
  async setPreference() { trace('preference'); }
  async preferenceValue() { return 'state'; }
  async captureSanitizedEvidence(label) { trace('evidence:' + label); }
  evidenceSnapshot() {
    trace('snapshot:platform');
    if (!this.selected && mode === 'final-snapshot-failure') throw new Error('SNAPSHOT_FAILED');
    return recordSnapshot({ driver:this.driver, selectedInstalledWindowValid:this.selected, productObservations:this.proof,
      ...this.extraEvidence, ...(this.confirmationCapture ? {confirmationCapture:this.confirmationCapture} : {}) });
  }
  async relaunchInstalledApp() { trace('relaunch'); if (!this.pendingCold) return; this.pendingCold=false; this.cold=true; }
  async backgroundApp() { trace('background'); }
  async terminateInstalledApp() { trace('terminate'); this.pendingCold=true; if (this.name === 'android') await this.environmentMeasurement.terminate('com.android.chrome', '42'); }
  async openFixtureAgent() { trace('agent'); }
  async showKeyboardOnComposer() { trace('keyboard'); }
  async hideKeyboard() { trace('hide-keyboard'); }
  async stopOwnedResources() {
    trace('cleanup:platform');
    if (mode === 'late-driver-failure') { this.driver.firstFatal={code:'APPIUM_TIMEOUT',operation:'teardown'}; this.driver.unusable=true; }
    this.selected=false;
    if(this.proof) this.proof.records.length=0;
    if (captureMode === 'teardown-only') this.confirmationCapture = structuredClone(capture);
    if (captureMode === 'changed') { this.confirmationCapture.status='unavailable'; this.confirmationCapture.source.dispatchedOrdinal=99; this.confirmationCapture.projection.nodes.pop(); }
    if (captureMode) this.extraEvidence.nativeDiagnosticsBlocked=this.driver.unusable;
    if (mode.includes('cleanup-failure')) throw new Error('CLEANUP_FAILED');
  }
}
`;
export const runnerProtocolCases = ['success', 'component-drift', 'unknown-relation', 'gms-out-of-order', 'gms-missing-post-begin', 'fresh-failure', 'baseline-failure', 'baseline-failure-cleanup-failure', 'scenario-failure', 'fatal-failure', 'post-failure', 'scenario-failure-post-failure', 'recovery-failure-post-failure', 'scenario-failure-post-failure-cleanup-failure', 'cleanup-failure', 'shutdown-failure', 'fixture-stop-failure', 'reverse-acquire-failure', 'reverse-remove-failure', 'fixture-log-failure', 'private-failure', 'event-write-failure', 'result-write-failure', 'sticky-driver-failure', 'late-driver-failure'] as const;

async function replay(mode: string, platform: string, suite: string, captureMode?: CaptureMode): Promise<void> {
  const root = await mkdtemp(join(tmpdir(), 'herdr-scenario-protocol-'));
  const source = process.env.MOBILE_RUN_TEST_SOURCE || repositoryPath('tests/mobile');
  await mkdir(join(root, 'platforms')); await mkdir(join(root, 'android-appium'));
  await cp(join(source, 'support'), join(root, 'support'), { recursive: true });
  await cp(join(source, 'android-appium/kernel-namespace.cjs'), join(root, 'android-appium/kernel-namespace.cjs'));
  await cp(join(source, 'run.ts'), join(root, 'run.ts'));
  assert.equal(await readFile(join(root, 'run.ts'), 'utf8'), await readFile(join(source, 'run.ts'), 'utf8'));
  const capture = captureMode ? confirmationCaptureFixture() : undefined;
  if (capture) {
    assert.equal(capture.projection.status, 'truncated');
    assert.equal(capture.projection.reason, 'bytes');
    assert.ok(capture.projection.nodes.length > 300);
    await writeFile(join(root, 'capture.json'), JSON.stringify(capture), { flag: 'wx' });
  }
  assert.equal(await readFile(join(root, 'support/diagnostics.ts'), 'utf8'), await readFile(join(source, 'support/diagnostics.ts'), 'utf8'));
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
  id='android-test'; operations=[]; errors=[]; gmsErrors=[]; required=false; stages=[]; begun=false; finalized=false; finished=false; stageFailed=false;
  expectedStages=['after-device-start','before-measurement-begin','after-setup-url','after-browser-install','after-initial-installed-app-launch'];
  bind(identity) { this.identity=identity; }
  failureOutcome() { return { errors:this.errors }; }
  plannedOperations() { return structuredClone(this.operations); }
  requireGmsObservations() {
    if (this.required || this.begun || this.finalized) throw new Error('ANDROID_ENVIRONMENT: GMS requirement must be set once before begin');
    this.required=true; trace('measurement:gms:required');
  }
  async observeGmsStage(stage) {
    const index=this.stages.length;
    if (!this.required || this.finalized || this.stageFailed || index>=this.expectedStages.length || this.expectedStages[index]!==stage || (index<2 && this.begun) || (index>=2 && !this.begun)) {
      this.stageFailed=true; this.gmsErrors.push('GMS stage order or measurement phase is invalid');
      trace('measurement:gms:rejected:' + stage); return;
    }
    this.stages.push(stage); trace('measurement:gms:' + stage);
  }
  startGmsFinalization() {
    if (this.finalized) return;
    this.finalized=true; trace('measurement:gms:finalize');
    if (!this.required || !this.begun || this.stageFailed || this.stages.length!==this.expectedStages.length) {
      this.gmsErrors.push('GMS diagnostic stage sequence is incomplete');
    }
    if (this.gmsErrors.length) this.errors.push(new Error('ANDROID_ENVIRONMENT: ' + this.gmsErrors.length + ' GMS diagnostic observation error(s)'));
  }
  async finalizeGmsObservations() {
    this.startGmsFinalization();
    if (!this.begun) return;
    if (!this.terminalOutcome) this.terminalOutcome=new Promise(resolve=>{this.confirmTerminal=resolve;});
    const outcome=await this.terminalOutcome;
    if (outcome.errors.length || outcome.assessment?.collection.status!=='PASS') throw new Error('ANDROID_ENVIRONMENT: terminal collector confirmation failed');
  }
  async begin() {
    trace('measurement:begin');
    if (!this.required || this.stageFailed || this.stages.length!==2) {
      const error=new Error('ANDROID_ENVIRONMENT: begin requires both ordered pre-measurement GMS stage attempts');
      this.errors.push(error); throw error;
    }
    if (mode.includes('baseline-failure')) { const error=new Error('ANDROID_ENVIRONMENT: baseline failure'); this.errors.push(error,new Error('COLLECTOR_CLOSE_FAILED')); throw error; }
    this.begun=true;
  }
  async finish() {
    if (this.finished) throw new Error('ANDROID_ENVIRONMENT: measurement already finalized');
    this.finished=true;
    this.startGmsFinalization();
    trace('measurement:finish'); const result=structuredClone(assessment); result.identity=this.identity; result.plannedOperations=this.plannedOperations();
    if(mode.includes('component-drift')) { result.status='FAIL'; result.findings=[{category:'GMS_COMPONENT_DRIFT'}]; }
    if(mode.includes('unknown-relation')) { result.status='FAIL'; result.findings=[{category:'ADVERSE_DEATH',eventIndex:0}]; result.nativeEvents=[{kind:'process-death',pid:'5820',line:'SIG: 9'}]; }
    if(mode.includes('post-failure')) { result.collection.status='UNKNOWN'; result.status='UNKNOWN'; result.findings=[{category:'EVIDENCE_INVALID'}]; }
    if(this.gmsErrors.length) {
      result.collection.status='UNKNOWN';
      result.collection.issues=[...result.collection.issues,'GMS terminal observation confirmation is unavailable or invalid'];
      result.status='UNKNOWN'; result.findings=[...result.findings,{category:'EVIDENCE_INVALID'}];
    }
    if(result.collection.status!=='PASS') this.errors.push(new Error('ANDROID_ENVIRONMENT: collection guarantee failed'));
    const outcome={assessment:result,assessmentSha256:'a'.repeat(64), errors:[...this.errors,...(mode.includes('post-failure')?[new Error('ANDROID_ENVIRONMENT: original postcheck failure')]:[])]};
    this.terminalOutcome=Promise.resolve(outcome); this.confirmTerminal?.(outcome);
    return outcome;
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
  writeFile:async(path,...args)=>{const name=String(path).split('/').at(-1);trace('write:'+name); if (name==='mobile-result.json' && process.env.RUN_PROTOCOL_CAPTURE) await original.writeFile(process.env.RUN_PROTOCOL_LIVE_CAPTURE, JSON.stringify(globalThis.protocolPlatform.confirmationCapture), {flag:'wx'}); if ((mode.includes('fixture-log-failure')&&name==='fixture.log')||(mode.includes('event-write-failure')&&name==='scenario-events.json')||(mode.includes('result-write-failure')&&name==='mobile-result.json')) throw Error('WRITE_FAILED:'+name);return original.writeFile(path,...args);},
  rm:async(path,...args)=>{trace('remove:'+String(path).split('/').at(-1));if(mode.includes('private-failure')&&String(path).endsWith('/private'))throw Error('PRIVATE_REMOVE_FAILED');return original.rm(path,...args);}
}));
`);
  const bundle = (candidate: boolean) => ({ name: candidate ? 'candidate-current' : '0.20.10', root: '.', provenance: { sourceCommit: 'a'.repeat(40) }, identity: {
    version: candidate ? '0.21.0' : '0.20.10', assets: candidate ? 370 : 363, build: (candidate ? 'd' : 'e').repeat(64), entry: candidate ? '/new/index.html' : '/old/index.html', script: candidate ? '/new.js' : '/old.js', style: candidate ? '/new.css' : '/old.css', scriptSha256: candidate ? 'new-script' : 'old-script', styleSha256: candidate ? 'new-style' : 'old-style', webHash: (candidate ? 'c' : 'f').repeat(64), descriptor: true } });
  await writeFile(join(root, 'bundles.json'), JSON.stringify({ schema: 1, baselines: [bundle(false)], candidate: bundle(true) }));
  const traceFile = join(root, 'trace.log');
  const child = spawnSync(process.execPath, ['--preload', join(root, 'preload.ts'), join(root, 'run.ts'), '--bundle-set', join(root, 'bundles.json'), '--output', join(root, 'output'), '--private-output', join(root, 'private'), '--suite', suite], {
    env: { ...process.env, MOBILE_PLATFORM: platform, MOBILE_SOURCE_RUN_HEAD_SHA: 'b'.repeat(40), GITHUB_RUN_ID: '35556703407', GITHUB_RUN_ATTEMPT: '2', ANDROID_SERIAL: 'emulator-protocol', RUN_PROTOCOL_CASE: mode, RUN_PROTOCOL_TRACE: traceFile, RUN_PROTOCOL_CAPTURE: captureMode || '', RUN_PROTOCOL_CAPTURE_FILE: join(root, 'capture.json'), RUN_PROTOCOL_SNAPSHOTS: join(root, 'snapshots.jsonl'), RUN_PROTOCOL_LIVE_CAPTURE: join(root, 'capture-live.json') }, encoding: 'utf8', timeout: 20_000,
  });
  await writeFile(join(root, 'command.json'), JSON.stringify({ mode, platform, suite, captureMode, status: child.status, stdout: child.stdout, stderr: child.stderr, error: String(child.error || '') }, null, 2));
  assert.equal(child.error, undefined);
  const events = (await readFile(traceFile, 'utf8')).trim().split('\n');
  const before = (first: string, last: string) => assert.ok(events.includes(first) && events.includes(last) && events.indexOf(first) < events.indexOf(last), `${mode}: ${first} before ${last}: ${events}`);
  const resultMissing = mode === 'result-write-failure';
  const resultText = resultMissing ? '' : await readFile(join(root, 'output/mobile-result.json'), 'utf8');
  const result = resultMissing ? undefined : JSON.parse(resultText);
  const failed = mode.includes('failure') || mode === 'unknown-relation' || mode.startsWith('gms-');
  assert.equal(child.status, failed ? 1 : 0, JSON.stringify({ result, stderr:child.stderr, events }));
  before('cleanup:fixture', 'write:fixture.log'); before('write:fixture.log', 'remove:private'); before('remove:private', 'write:scenario-events.json'); before('write:scenario-events.json', 'write:mobile-result.json');
  if (resultMissing) { await assert.rejects(readFile(join(root, 'output/mobile-result.json'))); return; }
  assert.equal(result.schema, 2);
  assert.equal(resultText, `${JSON.stringify(sanitizeValue(result), null, 2)}\n`);
  const snapshots = (mode === 'reverse-acquire-failure' ? [] : (await readFile(join(root, 'snapshots.jsonl'), 'utf8')).trim().split('\n')).map(line => {
    const snapshot = JSON.parse(line);
    snapshot.driver.firstFatal ??= null;
    return sanitizeValue(snapshot) as any;
  });
  const measuredSnapshot = snapshots.findLast(snapshot => snapshot.selectedInstalledWindowValid) || {};
  const teardownSnapshot = snapshots.find(snapshot => !snapshot.selectedInstalledWindowValid);
  const measuredEvidence = structuredClone(result.evidence);
  delete measuredEvidence.teardown;
  delete measuredEvidence.productEventRelations;
  if (captureMode === 'teardown-only') measuredSnapshot.confirmationCapture = teardownSnapshot.confirmationCapture;
  assert.deepEqual(measuredEvidence, measuredSnapshot);
  if (capture) {
    const withoutCapture = structuredClone(result);
    delete withoutCapture.evidence.confirmationCapture;
    if (withoutCapture.evidence.teardown) delete withoutCapture.evidence.teardown.confirmationCapture;
    const projectionBytes = (teardown = false) => {
      const withoutProjection = structuredClone(result);
      const target = teardown ? withoutProjection.evidence.teardown : withoutProjection.evidence;
      if (!target?.confirmationCapture?.projection) return 0;
      delete target.confirmationCapture.projection;
      return Buffer.byteLength(resultText) - Buffer.byteLength(`${JSON.stringify(sanitizeValue(withoutProjection), null, 2)}\n`);
    };
    const metrics = {
      mode, captureMode, resultBytes: Buffer.byteLength(resultText), nodes: result.evidence.confirmationCapture.projection.nodes.length,
      canonicalProjectionBytes: projectionBytes(), teardownProjectionBytes: projectionBytes(true),
      totalAddedBytesIncludingReceipts: Buffer.byteLength(resultText) - Buffer.byteLength(`${JSON.stringify(sanitizeValue(withoutCapture), null, 2)}\n`),
      captureCopies: (resultText.match(/"confirmationCapture":/gu) || []).length,
    };
    await writeFile(join(root, 'terminal-metrics.json'), `${JSON.stringify(metrics, null, 2)}\n`, { flag: 'wx' });
    assert.equal(metrics.captureCopies, 1, JSON.stringify(metrics));
    assert.ok(metrics.canonicalProjectionBytes > captureLimits.projectionBytes - 256);
    assert.ok(metrics.canonicalProjectionBytes <= captureLimits.projectionBytes);
    assert.equal(metrics.teardownProjectionBytes, 0);
    assert.ok(metrics.totalAddedBytesIncludingReceipts > metrics.canonicalProjectionBytes);
    assert.ok(metrics.totalAddedBytesIncludingReceipts <= captureLimits.evidenceBytes);
    assert.deepEqual(result.evidence.confirmationCapture, capture);
    const liveCapture = JSON.parse(await readFile(join(root, 'capture-live.json'), 'utf8'));
    const expectedLive = structuredClone(capture);
    if (captureMode === 'changed') { expectedLive.status = 'unavailable'; expectedLive.source.dispatchedOrdinal = 99; expectedLive.projection.nodes.pop(); }
    assert.deepEqual(liveCapture, expectedLive);
    assert.equal(result.evidence.events[0].detail.token, '[REDACTED]');
    assert.doesNotMatch(resultText, /synthetic-secret/u);
    assert.deepEqual(result.environment_qualification, { platform: 'ios', applicability: 'NOT_APPLICABLE' });
    if (captureMode === 'teardown-only') {
      assert.equal(result.product.status, 'FAIL');
      assert.equal(result.finalization.status, 'PASS');
    }
    if (teardownSnapshot) delete teardownSnapshot.confirmationCapture;
  } else {
    assert.equal(resultText.includes('confirmationCapture'), false);
  }
  assert.deepEqual(result.evidence.teardown, teardownSnapshot);
  if (mode === 'final-snapshot-failure') {
    assert.equal(result.product.status, 'PASS');
    assert.equal(result.finalization.status, 'FAIL');
    assert.deepEqual(result.finalization.failures, [{ stage: 'final-driver-snapshot', category: 'FINALIZATION', message: 'SNAPSHOT_FAILED' }]);
  }
  const measured = platform === 'android' && !/fresh-failure|baseline-failure|reverse-acquire-failure|gms-out-of-order/u.test(mode);
  assert.equal(events.filter(event => event === 'measurement:finish').length, measured ? 1 : 0);
  if (measured) { before('fresh:end','measurement:begin'); before('measurement:begin','invitation'); before('measurement:finish','cleanup:platform'); }
  if (/fresh-failure|baseline-failure|reverse-acquire-failure|gms-out-of-order/u.test(mode)) assert.ok(!events.includes('invitation'));
  if (mode === 'gms-out-of-order') {
    assert.match(result.primary_failure.message, /begin requires both ordered pre-measurement GMS stage attempts/u);
    assert.ok(result.additional_failures.some((failure:any) => /3 GMS diagnostic observation error\(s\)/u.test(failure.message)));
    assert.equal(events.includes('measurement:gms:before-measurement-begin'), false);
    before('measurement:gms:rejected:after-setup-url', 'measurement:gms:finalize');
  }
  if (mode === 'gms-missing-post-begin') {
    assert.equal(result.finalization.status, 'FAIL');
    assert.ok(result.finalization.failures.some((failure:any) => /1 GMS diagnostic observation error\(s\)/u.test(failure.message)));
    assert.ok(result.finalization.failures.some((failure:any) => /collection guarantee failed/u.test(failure.message)));
    assert.equal(result.environment_qualification.status, 'UNKNOWN');
    assert.equal(result.environment_qualification.collection, 'UNKNOWN');
    assert.equal(result.product.status, 'FAIL');
    assert.ok(result.product.categories.includes('EVIDENCE_INVALID'));
    assert.equal(events.includes('measurement:gms:after-initial-installed-app-launch'), false);
    before('measurement:gms:after-browser-install', 'measurement:gms:finalize');
  }
  if (mode === 'success' && platform === 'android') {
    const stages = ['after-device-start','before-measurement-begin','after-setup-url','after-browser-install','after-initial-installed-app-launch'];
    for (let index = 1; index < stages.length; index++) before('measurement:gms:' + stages[index - 1], 'measurement:gms:' + stages[index]);
    before('measurement:gms:before-measurement-begin', 'measurement:begin');
    before('measurement:gms:after-initial-installed-app-launch', 'measurement:gms:finalize');
    before('measurement:gms:finalize', 'measurement:finish');
  }
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
  ...(['success', 'scenario-failure', 'fatal-failure', 'cleanup-failure', 'sticky-driver-failure', 'late-driver-failure', 'event-write-failure', 'final-snapshot-failure'] as const).map(mode => [
    `production runner iOS terminal byte-pressure ${mode} retains only the frozen capture and all other evidence`,
    () => replay(mode, 'ios', 'release', mode === 'success' ? 'unchanged' : 'changed'),
  ] as [string, () => Promise<void>]),
  ['production runner iOS terminal changed teardown cannot replace the frozen byte-pressure capture', () => replay('success', 'ios', 'release', 'changed')],
  ['production runner iOS terminal teardown-only capture remains diagnostic on scenario failure', () => replay('scenario-failure', 'ios', 'release', 'teardown-only')],
];
