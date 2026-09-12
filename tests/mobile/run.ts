import { mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import type { ChildProcess } from 'node:child_process';
import { assertDistinctUpgrade, type BundleSet } from './support/artifacts';
import { DiagnosticRecorder, redactText, writeSanitizedJson } from './support/diagnostics';
import { PhaseBudget } from './support/budget';
import { command, startCommand, stopProcess } from './support/process';
import {
  assertBoundedReloads,
  assertCredentialIdentityPreserved,
  assertCredentialPreserved,
  assertPhoneUpdateAcknowledged,
  assertPhoneUpdateNotAcknowledged,
  assertNoRelayDeploy,
  assertNoRelayInstall,
  assertOldIdentity,
  assertInvitationOwnership,
  assertRelayOwnership,
  isQualificationFatal,
  QualificationFailureLatch,
  assertPreferencePreserved,
  assertStandalone,
  assertStandaloneOwnership,
  assertRunningIdentity,
  isRuntimeIdentityNotReady,
  type PreferenceEvidence,
  type QualificationFailureSnapshot,
  type RelayAuthEvidence,
  type RuntimeIdentity,
} from './support/oracle';
import { delay, isFatalDriverError } from './support/webdriver';
import { AndroidPlatform } from './platforms/android';
import { AndroidEnvironmentMeasurement } from './android-measurement';
import { IOSPlatform } from './platforms/ios';
import type { MobilePlatform, PlatformOptions, UpdateCompletionEvidence } from './platforms/types';
import { repositoryPath, repositoryRoot } from './support/paths';

interface FixtureInfo {
  app_url: string;
  relay_urls: string[];
  setup_urls: string[];
  control_url: string;
  control_secret: string;
  ca_certificate: string;
  old_release: string;
  candidate_release: string;
}

interface FixtureRelayState {
  name: string;
  invitation_auth_count: number;
  credential_auth_count: number;
  credential_pseudonyms: string[];
  connections: number;
  install_update_count: number;
  deploy_app_update_count: number;
}

interface FixtureRequest {
  method: string;
  path: string;
  fault?: string;
  fault_id?: string;
  fault_generation?: string;
  release: string;
}

interface FixtureState {
  active_release: string;
  app_url: string;
  requests: FixtureRequest[];
  faults?: Array<{ id: string; generation: string; method: string; path: string; kind: string; remaining: number }>;
  invalidated?: boolean;
  invalidation_reason?: string;
  relays: FixtureRelayState[];
}

interface RunResult {
  schema: 1;
  result: 'passed' | 'product failure' | 'infrastructure failure' | 'blocked';
  platform: string;
  suite: string;
  baseline: string;
  candidate: string;
  origin?: string;
  source_commit: string;
  source_run_head_sha?: string;
  candidate_web_hash: string;
  initial_identity?: RuntimeIdentity;
  final_identity?: RuntimeIdentity;
  credential_preserved?: boolean;
  credential_evidence?: RelayAuthEvidence;
  preference_preserved?: boolean;
  reload_count: number;
  lifecycle_launch_count?: number;
  fixture_requests?: FixtureRequest[];
  faults_exercised?: string[];
  fault_identity?: { id: string; generation: string; kind: string; path: string };
  oracle_controls?: string[];
  phone_completion?: UpdateCompletionEvidence;
  qualification_failure?: QualificationFailureSnapshot;
  failure_stage?: string;
  failure?: string;
  android_environment_failure?: string;
  evidence?: Record<string, unknown>;
  fixture_state?: { requests: FixtureRequest[]; active_release: string };
  budget?: ReturnType<PhaseBudget['snapshot']>;
}

function option(name: string): string | undefined {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

function required(name: string): string {
  const value = option(name);
  if (!value) throw new Error(`missing ${name}`);
  return value;
}

async function readJsonFile<T>(filename: string): Promise<T> {
  return JSON.parse(await readFile(filename, 'utf8')) as T;
}

async function waitForInfo(filename: string, timeoutMs: number, budget?: PhaseBudget): Promise<FixtureInfo> {
  const deadline = Date.now() + Math.min(timeoutMs, budget?.remainingMs ?? timeoutMs);
  while (Date.now() < deadline) {
    try {
      return await readJsonFile<FixtureInfo>(filename);
    } catch {
      await delay(250, budget);
    }
  }
  throw new Error('FIXTURE_STARTUP: fixture did not publish its private info before the deadline');
}

function fixtureEnvironment(root: string): NodeJS.ProcessEnv {
  return {
    PATH: process.env.PATH || '',
    HOME: resolve(root, 'home'),
    XDG_CONFIG_HOME: resolve(root, 'config'),
    XDG_CACHE_HOME: resolve(root, 'cache'),
    XDG_DATA_HOME: resolve(root, 'data'),
    LANG: 'C',
    LC_ALL: 'C',
  };
}

function startFixture(bundleSet: BundleSet, privateDir: string, infoFile: string): { process: ChildProcess; output: string[] } {
  const fixtureRoot = resolve(privateDir, 'fixture-private');
  const binary = process.env.MOBILE_FIXTURE_BINARY || '';
  const args = binary
    ? ['-old-root', bundleSet.baselines[0].root, '-candidate-root', bundleSet.candidate.root, '-run-dir', fixtureRoot, '-info-file', infoFile]
    : ['run', './tests/mobile/fixture', '-old-root', bundleSet.baselines[0].root, '-candidate-root', bundleSet.candidate.root, '-run-dir', fixtureRoot, '-info-file', infoFile];
  const output: string[] = [];
  const child = startCommand(binary || 'go', args, (chunk) => {
    if (output.join('').length < 1_000_000) output.push(chunk);
  }, { cwd: repositoryRoot, env: fixtureEnvironment(fixtureRoot) });
  return { process: child, output };
}

async function control(info: FixtureInfo, path: string, method = 'GET', body?: unknown): Promise<any> {
  const response = await fetch(`${info.control_url}${path}`, {
    method,
    headers: {
      'X-Herdr-Fixture-Secret': info.control_secret,
      ...(body === undefined ? {} : { 'content-type': 'application/json' }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(30_000),
  });
  const text = await response.text();
  if (!response.ok) throw new Error(`FIXTURE_CONTROL: HTTP ${response.status}: ${text.slice(0, 300)}`);
  return text ? JSON.parse(text) : null;
}

async function fixtureState(info: FixtureInfo): Promise<FixtureState> {
  return control(info, '/state') as Promise<FixtureState>;
}

async function waitForCredential(info: FixtureInfo, relayName: string, budget?: PhaseBudget): Promise<FixtureState> {
  const deadline = Date.now() + Math.min(60_000, budget?.remainingMs ?? 60_000);
  while (Date.now() < deadline) {
    const state = await fixtureState(info);
    const relay = state.relays.find((candidate) => candidate.name === relayName);
    if (relay && relay.invitation_auth_count >= 1) return state;
    await delay(250, budget);
  }
  throw new Error(`PAIRING: ${relayName} did not consume its invitation`);
}

async function waitForCredentialReconnect(info: FixtureInfo, relayName: string, previousCount: number, budget?: PhaseBudget): Promise<FixtureState> {
  const deadline = Date.now() + Math.min(60_000, budget?.remainingMs ?? 60_000);
  while (Date.now() < deadline) {
    const state = await fixtureState(info);
    const relay = state.relays.find((candidate) => candidate.name === relayName);
    if (relay && relay.credential_auth_count > previousCount && relay.connections > 0) return state;
    await delay(250, budget);
  }
  throw new Error(`LIFECYCLE: ${relayName} did not reconnect with its issued credential`);
}

function authEvidence(state: FixtureState): RelayAuthEvidence {
  return {
    relays: Object.fromEntries(state.relays.map((relay) => [relay.name, {
      invitationAuthCount: relay.invitation_auth_count,
      credentialAuthCount: relay.credential_auth_count,
      credentialPseudonyms: [...relay.credential_pseudonyms].sort(),
      connections: relay.connections,
    }])),
  };
}

async function waitForFault(
  info: FixtureInfo,
  path: string,
  kind: string,
  faultId: string,
  faultGeneration: string,
  budget?: PhaseBudget,
): Promise<void> {
  const deadline = Date.now() + Math.min(30_000, budget?.remainingMs ?? 30_000);
  while (Date.now() < deadline) {
    const state = await fixtureState(info);
    if (state.invalidated) throw new Error(`FIXTURE_FAULT_EXPIRED: ${state.invalidation_reason || 'fault lifetime expired'}`);
    if (state.requests.some((request) => request.path === path
      && request.fault === kind
      && request.fault_id === faultId
      && request.fault_generation === faultGeneration)) return;
    await delay(250, budget);
  }
  throw new Error(`FIXTURE_FAULT: ${kind} for ${path} was not consumed`);
}

function assertFaultActive(state: FixtureState, path: string, kind: string, faultId: string, faultGeneration: string): void {
  if (state.invalidated) throw new Error(`FIXTURE_FAULT_EXPIRED: ${state.invalidation_reason || 'fault lifetime expired'}`);
  const request = state.requests.find((candidate) => candidate.path === path
    && candidate.fault === kind
    && candidate.fault_id === faultId
    && candidate.fault_generation === faultGeneration);
  if (!request) throw new Error(`FIXTURE_FAULT_GENERATION: consumed ${kind} fault ${faultId} was not recorded with its generation`);
  const fault = state.faults?.find((candidate) => candidate.id === faultId
    && candidate.generation === faultGeneration
    && candidate.path === path
    && candidate.kind === kind);
  if (!fault || fault.remaining === 0) throw new Error(`FIXTURE_FAULT_GENERATION: active ${kind} fault ${faultId} is not present`);
}

async function reconnectAllRelays(info: FixtureInfo, before: RelayAuthEvidence, budget?: PhaseBudget): Promise<RelayAuthEvidence> {
  const relayNames = Object.keys(before.relays).sort();
  for (const relayName of relayNames) {
    await control(info, `/relay/drop?name=${encodeURIComponent(relayName)}`, 'POST');
  }
  let state = await fixtureState(info);
  for (const relayName of relayNames) {
    const previousCount = before.relays[relayName]?.credentialAuthCount;
    if (previousCount === undefined) throw new Error(`CREDENTIAL_RELAY_MISSING: ${relayName}`);
    state = await waitForCredentialReconnect(info, relayName, previousCount, budget);
  }
  return authEvidence(state);
}

function phonePlanContract(
  baseline: BundleSet['baselines'][number],
  evidence: Awaited<ReturnType<MobilePlatform['readUpdateCompletion']>>,
  controls: Set<string>,
): boolean {
  if (evidence.phoneRequired) return true;
  const version = baseline.identity.version;
  if (version === '0.20.8' || version === '0.20.9') {
    controls.add(`HISTORICAL_PHONE_ACCOUNTING_UNAVAILABLE:${version}`);
    return false;
  }
  throw new Error(`PHONE_PLAN_MISSING: ${version} exposed no phone item in its update progress record`);
}

async function waitForPhoneCompletion(
  platform: MobilePlatform,
  baseline: BundleSet['baselines'][number],
  controls: Set<string>,
  qualification: QualificationFailureLatch,
  budget?: PhaseBudget,
): Promise<UpdateCompletionEvidence> {
  const deadline = Date.now() + Math.min(30_000, budget?.remainingMs ?? 30_000);
  let lastError = '';
  while (Date.now() < deadline) {
    try {
      const evidence = await platform.readUpdateCompletion();
      qualification.observe({ completion: evidence });
      if (!phonePlanContract(baseline, evidence, controls)) return evidence;
      assertPhoneUpdateAcknowledged(evidence);
      return evidence;
    } catch (error) {
      if (isFatalDriverError(error)) throw error;
      if (isQualificationFatal(error)) qualification.fail(error, 'upgrade');
      if (error instanceof Error && /PHONE_PLAN_MISSING|PREMATURE_PHONE_COMPLETION/u.test(error.message)) {
        qualification.fail(error, 'upgrade');
      }
      lastError = error instanceof Error ? error.message : String(error);
      await delay(250, budget);
    }
  }
  throw new Error(`PHONE_COMPLETION_MISSING: ${lastError}`);
}

async function assertCandidateFailureObserved(
  platform: MobilePlatform,
  expected: BundleSet['candidate']['identity'],
  origin: string,
  baseline: BundleSet['baselines'][number],
  controls: Set<string>,
  info: FixtureInfo,
  faultPath: string,
  faultKind: string,
  faultId: string,
  faultGeneration: string,
  qualification: QualificationFailureLatch,
  budget?: PhaseBudget,
): Promise<UpdateCompletionEvidence> {
  const deadline = Date.now() + Math.min(60_000, budget?.remainingMs ?? 60_000);
  let lastError = '';
  while (!budget?.exhausted && Date.now() < deadline) {
    const state = await fixtureState(info);
    qualification.observe({ fault: state });
    assertFaultActive(state, faultPath, faultKind, faultId, faultGeneration);
    try {
      const identity = await platform.readRunningIdentity();
      qualification.observe({ identity });
      assertStandaloneOwnership(identity, origin);
      const completion = await platform.readUpdateCompletion();
      qualification.observe({ identity, completion });
      if (phonePlanContract(baseline, completion, controls)) assertPhoneUpdateNotAcknowledged(completion);
      if (identity.entry !== expected.entry || identity.script !== expected.script || identity.style !== expected.style) {
        throw new Error(`UPGRADE_FAILURE_TARGET_MISMATCH: observed ${identity.entry}/${identity.script}/${identity.style}`);
      }
      if (identity.requiredAssetsReady || identity.requiredAssetFailure !== true) {
        throw new Error('REQUIRED_ASSET_FAILURE_MISSING: candidate fault did not produce a required-asset failure');
      }
      if (identity.failureUiVisible !== true) throw new Error('FAILURE_UI_MISSING: candidate fault did not expose the failure recovery UI');
      return completion;
    } catch (error) {
      if (isFatalDriverError(error)) throw error;
      if (isQualificationFatal(error)) qualification.fail(error, 'upgrade-failure-observation');
      if (error instanceof Error && /PHONE_PLAN_MISSING|PREMATURE_PHONE_COMPLETION/u.test(error.message)) {
        qualification.fail(error, 'upgrade-failure-observation');
      }
      lastError = error instanceof Error ? error.message : String(error);
    }
    await delay(500, budget);
  }
  throw new Error(`FAILURE_OBSERVATION: candidate failure UI was not observed before the deadline: ${lastError}`);
}

async function waitForCandidate(
  platform: MobilePlatform,
  expected: BundleSet['candidate']['identity'],
  origin: string,
  navigationIds: Set<string>,
  qualification: QualificationFailureLatch,
  budget?: PhaseBudget,
): Promise<RuntimeIdentity> {
  const deadline = Date.now() + Math.min(180_000, budget?.remainingMs ?? 180_000);
  let lastError = '';
  while (Date.now() < deadline) {
    try {
      const identity = await platform.readRunningIdentity();
      qualification.observe({ identity });
      if (identity.navigationId) navigationIds.add(identity.navigationId);
      assertStandaloneOwnership(identity, origin);
      if (isRuntimeIdentityNotReady(identity, origin)) {
        lastError = 'RUNTIME_NOT_READY: installed document has not initialized its application';
        await delay(500, budget);
        continue;
      }
      assertStandalone(identity, origin);
      assertRunningIdentity(identity, expected);
      return identity;
    } catch (error) {
      if (isFatalDriverError(error)) throw error;
      if (isQualificationFatal(error)) qualification.fail(error, 'upgrade');
      lastError = error instanceof Error ? error.message : String(error);
      await delay(500, budget);
    }
  }
  throw new Error(`UPGRADE: candidate did not initialize before the deadline: ${lastError}`);
}

function platformFor(options: PlatformOptions): MobilePlatform {
  const platform = process.env.MOBILE_PLATFORM || '';
  if (platform === 'android') return new AndroidPlatform(options);
  if (platform === 'ios') return new IOSPlatform(options);
  throw new Error('PLATFORM: MOBILE_PLATFORM must be android or ios');
}

async function reversePorts(info: FixtureInfo, serial: string): Promise<string[]> {
  if (!serial) throw new Error('ANDROID_TARGET: ANDROID_SERIAL is required');
  const ports = [new URL(info.app_url).port, ...info.relay_urls.map((value) => new URL(value.replace(/^wss:/, 'https:')).port)];
  const reversed: string[] = [];
  for (const port of ports) {
    if (!port) throw new Error('ANDROID_TARGET: fixture URL has no port');
    await command(process.env.ADB || 'adb', ['-s', serial, 'reverse', `tcp:${port}`, `tcp:${port}`]);
    reversed.push(port);
  }
  return reversed;
}

async function removeReverses(serial: string, ports: string[]): Promise<void> {
  for (const port of ports) {
    await command(process.env.ADB || 'adb', ['-s', serial, 'reverse', '--remove', `tcp:${port}`]).catch(() => undefined);
  }
}

type ScenarioStage = 'device' | 'pairing' | 'upgrade' | 'lifecycle';

async function runUpgradeScenario(
  platform: MobilePlatform,
  info: FixtureInfo,
  bundleSet: BundleSet,
  suite: string,
  setStage: (stage: ScenarioStage) => void,
  budget: PhaseBudget,
  qualification: QualificationFailureLatch,
  afterDevicePreparation: () => Promise<void>,
): Promise<RunResult> {
  const baseline = bundleSet.baselines[0];
  assertDistinctUpgrade(baseline, bundleSet.candidate);
  const navigationIds = new Set<string>();
  const faultsExercised: string[] = [];
  let faultIdentity: RunResult['fault_identity'];
  const oracleControls = new Set<string>();
  const initialIdentity: RuntimeIdentity = await (async () => {
    setStage('device');
    await platform.startFreshDevice();
    await afterDevicePreparation();
    await platform.openSetupURL(info.setup_urls[0]);
    await platform.installFromBrowser();
    await platform.launchInstalledApp();
    await platform.clickWebText('Settings');
    return platform.assertStandalone(info.app_url);
  })();
  assertOldIdentity(initialIdentity, baseline, info.app_url);
  if (initialIdentity.navigationId) navigationIds.add(initialIdentity.navigationId);
  setStage('pairing');
  await waitForCredential(info, 'alpha', budget);
  await platform.openSetupURLInInstalledApp(info.setup_urls[1]);
  await waitForCredential(info, 'beta', budget);
  const pairedBeforeReconnect = authEvidence(await fixtureState(info));
  qualification.observe({ ownership: pairedBeforeReconnect });
  assertInvitationOwnership(pairedBeforeReconnect, ['alpha', 'beta']);
  const pairedAfterReconnect = await reconnectAllRelays(info, pairedBeforeReconnect, budget);
  qualification.observe({ ownership: pairedAfterReconnect });
  assertRelayOwnership(pairedAfterReconnect);
  await platform.setPreference('state');
  const beforePreference: PreferenceEvidence = { key: 'herdr_home_workspace_layout', value: await platform.preferenceValue() };
  const pairedState = await fixtureState(info);
  const beforeAuth = authEvidence(pairedState);
  qualification.observe({ ownership: beforeAuth });
  assertRelayOwnership(beforeAuth);
  await platform.captureSanitizedEvidence('paired');
  setStage('upgrade');
  await platform.clickWebText('Settings');
  const faultPath = bundleSet.candidate.name === 'current-code-target'
    ? bundleSet.candidate.identity.style
    : bundleSet.candidate.identity.script;
  const faultKind = bundleSet.candidate.name === 'current-code-target' ? 'missing' : 'corrupt';
  const faultId = `candidate-${bundleSet.candidate.name}-${faultKind}`;
  const faultGeneration = `${faultId}-${Date.now()}`;
  if (suite === 'release' || suite === 'smoke') {
    const beforeActivation = await fixtureState(info);
    if (beforeActivation.requests.some((request) => request.release === 'candidate' && request.path === faultPath && !request.fault)) {
      throw new Error(`FIXTURE_FAULT: candidate asset was fetched before fault ${faultPath}`);
    }
    await control(info, '/fault', 'POST', {
      id: faultId, generation: faultGeneration, method: 'GET', path: faultPath, kind: faultKind, remaining: -1,
    });
    faultIdentity = { id: faultId, generation: faultGeneration, kind: faultKind, path: faultPath };
    faultsExercised.push(`${faultKind}:${faultPath}`);
  }
  await control(info, '/activate', 'POST', { release: 'candidate' });
  await platform.clickWebText('Settings');
  await platform.clickWebText('Check for Updates');
  await platform.clickWebText('Load Update');
  await platform.clickDialogText('update-herdr-dialog', 'Load Update');
  if (suite === 'release' || suite === 'smoke') {
    await waitForFault(info, faultPath, faultKind, faultId, faultGeneration, budget);
    const faultState = await fixtureState(info);
    assertFaultActive(faultState, faultPath, faultKind, faultId, faultGeneration);
    await assertCandidateFailureObserved(
      platform,
      bundleSet.candidate.identity,
      info.app_url,
      baseline,
      oracleControls,
      info,
      faultPath,
      faultKind,
      faultId,
      faultGeneration,
      qualification,
      budget,
    );
    await control(info, '/fault/clear', 'POST', { id: faultId, generation: faultGeneration });
    const clearedState = await fixtureState(info);
    qualification.observe({ fault: clearedState });
    if (clearedState.invalidated || clearedState.faults?.some((fault) => fault.id === faultId)) {
      throw new Error(`FIXTURE_FAULT_CLEAR: fault ${faultId} was not explicitly cleared`);
    }
    await platform.clickWebText('Try again');
  }
  await waitForCandidate(platform, bundleSet.candidate.identity, info.app_url, navigationIds, qualification, budget);
  const phoneCompletion = await waitForPhoneCompletion(platform, baseline, oracleControls, qualification, budget);
  await platform.clickDialogText('update-progress-dialog', 'Close');
  const afterUpgradeAuth = await reconnectAllRelays(info, beforeAuth, budget);
  qualification.observe({ ownership: afterUpgradeAuth });
  assertRelayOwnership(afterUpgradeAuth);
  assertCredentialPreserved(beforeAuth, afterUpgradeAuth);
  await platform.captureSanitizedEvidence('candidate');
  const finalBeforeAuth = authEvidence(await fixtureState(info));
  qualification.observe({ ownership: finalBeforeAuth });
  assertRelayOwnership(finalBeforeAuth);
  await platform.relaunchInstalledApp();
  const finalIdentity = await waitForCandidate(platform, bundleSet.candidate.identity, info.app_url, navigationIds, qualification, budget);
  let finalAuth = await reconnectAllRelays(info, finalBeforeAuth, budget);
  qualification.observe({ ownership: finalAuth });
  assertRelayOwnership(finalAuth);
  assertCredentialPreserved(finalBeforeAuth, finalAuth);
  assertCredentialIdentityPreserved(beforeAuth, finalAuth);
  const upgradeNavigationCount = navigationIds.size;
  const afterPreference: PreferenceEvidence = { key: 'herdr_home_workspace_layout', value: await platform.preferenceValue() };
  let finalState = await fixtureState(info);
  assertPreferencePreserved(beforePreference, afterPreference);
  for (const relay of finalState.relays) {
    assertNoRelayInstall(relay.install_update_count);
    assertNoRelayDeploy(relay.deploy_app_update_count);
  }
  if (suite === 'release') {
    setStage('lifecycle');
    const beforeResumeAuth = authEvidence(await fixtureState(info));
    qualification.observe({ ownership: beforeResumeAuth });
    assertRelayOwnership(beforeResumeAuth);
    await platform.backgroundApp();
    await platform.relaunchInstalledApp();
    await waitForCandidate(platform, bundleSet.candidate.identity, info.app_url, navigationIds, qualification, budget);
    const resumeAuth = await reconnectAllRelays(info, beforeResumeAuth, budget);
    qualification.observe({ ownership: resumeAuth });
    assertRelayOwnership(resumeAuth, ['alpha', 'beta']);
    assertCredentialPreserved(beforeResumeAuth, resumeAuth);
    assertCredentialIdentityPreserved(beforeAuth, resumeAuth);
    const agentBeforeAuth = authEvidence(await fixtureState(info));
    qualification.observe({ ownership: agentBeforeAuth });
    assertRelayOwnership(agentBeforeAuth, ['alpha']);
    const agentCompletion = await platform.readUpdateCompletion();
    qualification.observe({ completion: agentCompletion });
    if (phonePlanContract(baseline, agentCompletion, oracleControls)) assertPhoneUpdateAcknowledged(agentCompletion);
    await platform.openFixtureAgent('alpha');
    const agentAfterAuth = authEvidence(await fixtureState(info));
    qualification.observe({ ownership: agentAfterAuth });
    assertRelayOwnership(agentAfterAuth, ['alpha']);
    assertCredentialIdentityPreserved(agentBeforeAuth, agentAfterAuth, ['alpha']);
    await platform.showKeyboardOnComposer();
    await platform.hideKeyboard();
    const beforeColdAuth = authEvidence(await fixtureState(info));
    qualification.observe({ ownership: beforeColdAuth });
    assertRelayOwnership(beforeColdAuth);
    await platform.terminateInstalledApp();
    await platform.relaunchInstalledApp();
    await waitForCandidate(platform, bundleSet.candidate.identity, info.app_url, navigationIds, qualification, budget);
    const coldAuth = await reconnectAllRelays(info, beforeColdAuth, budget);
    qualification.observe({ ownership: coldAuth });
    assertRelayOwnership(coldAuth);
    assertCredentialPreserved(beforeColdAuth, coldAuth);
    assertCredentialIdentityPreserved(beforeAuth, coldAuth);
    finalAuth = coldAuth;
    finalState = await fixtureState(info);
    assertPreferencePreserved(beforePreference, { key: beforePreference.key, value: await platform.preferenceValue() });
    for (const relay of finalState.relays) {
      assertNoRelayInstall(relay.install_update_count);
      assertNoRelayDeploy(relay.deploy_app_update_count);
    }
  }
  const reloadCount = Math.max(0, upgradeNavigationCount - 1);
  const lifecycleLaunchCount = Math.max(0, navigationIds.size - upgradeNavigationCount);
  assertBoundedReloads(reloadCount);
  if (lifecycleLaunchCount > 2) throw new Error(`RELOAD_BOUND_EXCEEDED: ${lifecycleLaunchCount} lifecycle launches exceed 2`);
  return {
    schema: 1, result: 'passed', platform: platform.name, suite,
    baseline: baseline.name, candidate: bundleSet.candidate.name, origin: info.app_url,
    source_commit: bundleSet.candidate.provenance.sourceCommit,
    source_run_head_sha: process.env.MOBILE_SOURCE_RUN_HEAD_SHA || undefined,
    candidate_web_hash: bundleSet.candidate.identity.webHash,
    initial_identity: initialIdentity, final_identity: finalIdentity,
    credential_preserved: true, credential_evidence: finalAuth, preference_preserved: true,
    reload_count: reloadCount, lifecycle_launch_count: lifecycleLaunchCount,
    fixture_requests: (await fixtureState(info)).requests,
    faults_exercised: faultsExercised,
    fault_identity: faultIdentity,
    oracle_controls: [...oracleControls].sort(),
    phone_completion: phoneCompletion,
  };
}

async function runUpgrade(
  platform: MobilePlatform,
  info: FixtureInfo,
  bundleSet: BundleSet,
  suite: string,
  setStage: (stage: ScenarioStage) => void,
  budget: PhaseBudget,
  afterDevicePreparation: () => Promise<void>,
): Promise<RunResult> {
  const qualification = new QualificationFailureLatch();
  let stage: ScenarioStage = 'device';
  try {
    return await runUpgradeScenario(platform, info, bundleSet, suite, (nextStage) => {
      stage = nextStage;
      setStage(nextStage);
    }, budget, qualification, afterDevicePreparation);
  } catch (error) {
    if (isFatalDriverError(error)) throw error;
    return qualification.fail(error, stage);
  }
}

function failureSnapshot(error: unknown, stage: string): QualificationFailureSnapshot | undefined {
  if (isQualificationFatal(error)) return error.snapshot();
  if (!isFatalDriverError(error)) return undefined;
  const message = error instanceof Error ? error.message : String(error);
  const code = error && typeof error === 'object' && 'code' in error
    ? String((error as { code?: unknown }).code)
    : message.match(/^([A-Z][A-Z0-9_]+):/u)?.[1] || 'DRIVER_FAILURE';
  const prefix = `${code}:`;
  return { code, stage, message: message.startsWith(prefix) ? message.slice(prefix.length).trim() : message };
}

async function main(): Promise<void> {
  const bundleSetFile = repositoryPath(required('--bundle-set'));
  const bundleSetDirectory = resolve(bundleSetFile, '..');
  const bundleSetValue = await readJsonFile<BundleSet>(bundleSetFile);
  const resolvedBundleSet: BundleSet = {
    ...bundleSetValue,
    candidate: { ...bundleSetValue.candidate, root: resolve(bundleSetDirectory, bundleSetValue.candidate.root) },
    baselines: bundleSetValue.baselines.map((bundle) => ({ ...bundle, root: resolve(bundleSetDirectory, bundle.root) })),
  };
  const selectedBaseline = option('--baseline');
  const baseline = selectedBaseline
    ? resolvedBundleSet.baselines.find((bundle) => bundle.name === selectedBaseline)
    : resolvedBundleSet.baselines[0];
  if (!baseline) throw new Error(`missing baseline ${selectedBaseline}`);
  const bundleSet: BundleSet = { ...resolvedBundleSet, baselines: [baseline] };
  const outputOption = option('--output') || 'run-artifacts';
  const outputDir = repositoryPath(outputOption);
  const privateDir = repositoryPath(option('--private-output') || process.env.MOBILE_PRIVATE || `${outputOption}.private-${process.pid}`);
  const suite = option('--suite') || 'smoke';
  const budget = new PhaseBudget('mobile-scenario', {
    timeoutMs: suite === 'release' ? 45 * 60_000 : 30 * 60_000,
    recoveryLimit: 8,
  });
  const diagnostics = new DiagnosticRecorder();
  if (resolve(outputDir) === resolve(privateDir)) throw new Error('DIAGNOSTIC_PRIVATE: private output must be separate from evidence output');
  await mkdir(outputDir, { recursive: true, mode: 0o700 });
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  const infoFile = resolve(privateDir, 'fixture-info.json');
  const fixtureProcess = startFixture(bundleSet, privateDir, infoFile);
  let fixtureInfo: FixtureInfo | undefined;
  let platform: MobilePlatform | undefined;
  let reverse: string[] = [];
  let result: RunResult;
  let cleanupFailure: unknown;
  let measurement: AndroidEnvironmentMeasurement | undefined;
  let measurementStarted = false;
  let androidEnvironmentFailure: string | undefined;
  let stage: string = 'fixture';
  try {
    fixtureInfo = await waitForInfo(infoFile, 180_000, budget);
    if (process.env.MOBILE_PLATFORM === 'android') reverse = await reversePorts(fixtureInfo, process.env.ANDROID_SERIAL || '');
    const platformOptions: PlatformOptions = {
      origin: fixtureInfo.app_url,
      appiumUrl: process.env.APPIUM_URL || 'http://127.0.0.1:4723',
      outputDir,
      certificate: fixtureInfo.ca_certificate,
      setupUrl: fixtureInfo.setup_urls[0],
      deviceId: process.env.MOBILE_PLATFORM === 'android' ? process.env.ANDROID_SERIAL : process.env.IOS_SIMULATOR_UDID,
      budget,
      diagnostics,
    };
    platform = platformFor(platformOptions);
    const android = platform instanceof AndroidPlatform ? platform : undefined;
    if (android) measurement = new AndroidEnvironmentMeasurement(platformOptions.deviceId || '', outputDir, repositoryPath('tests/mobile/toolchains.json'));
    result = await runUpgrade(platform, fixtureInfo, bundleSet, suite, (nextStage) => { stage = nextStage; }, budget, async () => {
      if (!android || !measurement) return;
      await measurement.begin();
      measurementStarted = true;
      android.environmentMeasurement = measurement;
    });
    result.evidence = platform.evidenceSnapshot();
    result.budget = budget.snapshot();
  } catch (error) {
    diagnostics.record({ phase: stage, operation: 'scenario-failure', detail: error instanceof Error ? error.message : String(error) });
    await platform?.captureSanitizedEvidence('failure').catch(() => undefined);
    const message = error instanceof Error ? error.message : String(error);
    const partialState = fixtureInfo ? await fixtureState(fixtureInfo).catch(() => undefined) : undefined;
    const failure = failureSnapshot(error, stage);
    const product = /(RUNTIME_|PREMATURE_|PHONE_COMPLETION_|PHONE_PLAN_|CREDENTIAL_|PREFERENCE_|INVITATION_|RELOAD_|UNEXPECTED_|STANDALONE_|ORIGIN_|APP_NOT_INITIALIZED|REQUIRED_ASSET|BASELINE_)/u.test(message);
    result = {
      schema: 1, result: product ? 'product failure' : 'infrastructure failure',
      platform: process.env.MOBILE_PLATFORM || 'unknown', suite,
      baseline: bundleSet.baselines[0]?.name || '', candidate: bundleSet.candidate.name,
      source_commit: bundleSet.candidate.provenance.sourceCommit,
      source_run_head_sha: process.env.MOBILE_SOURCE_RUN_HEAD_SHA || undefined,
      candidate_web_hash: bundleSet.candidate.identity.webHash,
      reload_count: 0,
      failure_stage: failure?.stage || stage,
      failure: message,
      qualification_failure: failure,
      evidence: platform?.evidenceSnapshot(),
      fixture_state: partialState ? { active_release: partialState.active_release, requests: partialState.requests } : undefined,
      budget: budget.snapshot(),
    };
  } finally {
    if (measurementStarted) {
      try {
        await measurement!.finish();
      } catch (error) {
        androidEnvironmentFailure = error instanceof Error ? error.message : String(error);
        diagnostics.record({ phase: 'android-environment', operation: 'postcheck', detail: androidEnvironmentFailure });
      }
    }
    try {
      await platform?.stopOwnedResources();
    } catch (error) {
      cleanupFailure = error;
      diagnostics.record({ phase: 'cleanup', operation: 'session-teardown', detail: error instanceof Error ? error.message : String(error) });
    }
    if (fixtureInfo) await control(fixtureInfo, '/shutdown', 'POST').catch(() => undefined);
    await stopProcess(fixtureProcess.process);
    await removeReverses(process.env.ANDROID_SERIAL || '', reverse);
    const fixtureLog = redactText(fixtureProcess.output.join('')).slice(0, 100 * 1024 * 1024);
    await writeFile(resolve(outputDir, 'fixture.log'), fixtureLog, { mode: 0o600 });
    await rm(infoFile, { force: true });
    await rm(privateDir, { recursive: true, force: true });
  }
  if (androidEnvironmentFailure !== undefined) {
    result.android_environment_failure = androidEnvironmentFailure;
    if (result.result === 'passed') {
      result = { ...result, result: 'infrastructure failure', failure_stage: 'android-environment', failure: androidEnvironmentFailure };
    }
  }
  if (cleanupFailure) {
    result.evidence = platform?.evidenceSnapshot();
    if (result.result === 'passed') {
      const message = cleanupFailure instanceof Error ? cleanupFailure.message : String(cleanupFailure);
      result = {
        ...result,
        result: 'infrastructure failure',
        failure_stage: 'cleanup',
        failure: message,
      };
    }
  }
  await diagnostics.write(resolve(outputDir, 'scenario-events.json'));
  await writeSanitizedJson(resolve(outputDir, 'mobile-result.json'), result);
  if (result.result !== 'passed') process.exitCode = 1;
}

main().catch((error: unknown) => {
  process.stderr.write(`${error instanceof Error ? error.stack || error.message : String(error)}\n`);
  process.exitCode = 1;
});
