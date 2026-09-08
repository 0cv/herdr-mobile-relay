import type { BundleIdentity, PreparedBundle } from './artifacts';

export interface RuntimeIdentity {
  url: string;
  origin: string;
  standalone: boolean;
  provider: string;
  nativeProvider?: string;
  navigationId?: string;
  version: string;
  assets: number;
  build: string;
  buildFromApplication?: boolean;
  entry: string;
  script: string;
  style: string;
  requiredAssetsReady: boolean;
  applicationInitialized: boolean;
}

export interface RelayAuthRecord {
  invitationAuthCount: number;
  credentialAuthCount: number;
  credentialPseudonyms: string[];
}

export interface RelayAuthEvidence {
  relays: Record<string, RelayAuthRecord>;
}

export interface PreferenceEvidence {
  key: string;
  value: string;
}

export interface UpdateCompletionEvidence {
  phoneRequired: boolean;
  phoneAcknowledged: boolean;
  phoneState: string;
  visibleCompletion: boolean;
  rawPlanPresent: boolean;
}

export function oracleError(code: string, detail: string): Error {
  return new Error(`${code}: ${detail}`);
}

export function assertStandalone(identity: RuntimeIdentity, expectedOrigin: string): void {
  if (!identity.standalone) throw oracleError('STANDALONE_REQUIRED', 'the observed document is not in standalone display mode');
  if (identity.origin !== expectedOrigin) throw oracleError('ORIGIN_MISMATCH', `${identity.origin} is not ${expectedOrigin}`);
  if (identity.provider === 'browser' || identity.provider === 'unknown' || !identity.nativeProvider) {
    throw oracleError('STANDALONE_PROVIDER_REQUIRED', 'the observed document is not an installed standalone web app with native provider evidence');
  }
  if (!identity.applicationInitialized) throw oracleError('APP_NOT_INITIALIZED', 'the installed document did not initialize the application');
}

export function assertRequiredAssets(identity: RuntimeIdentity): void {
  if (!identity.requiredAssetsReady) throw oracleError('REQUIRED_ASSET_FAILURE', 'the application stylesheet or entry did not finish successfully');
  if (!identity.script || !identity.style) throw oracleError('RUNTIME_ASSET_IDENTITY_MISSING', 'the running document has no application script and stylesheet');
}

export function assertRunningIdentity(
  identity: RuntimeIdentity,
  expected: BundleIdentity,
  requireExecutingBuild = true,
): void {
  assertRequiredAssets(identity);
  if (identity.version !== expected.version) throw oracleError('RUNTIME_VERSION_MISMATCH', `${identity.version} is not ${expected.version}`);
  if (identity.assets !== expected.assets) throw oracleError('RUNTIME_ASSET_VERSION_MISMATCH', `${identity.assets} is not ${expected.assets}`);
  if (identity.entry !== expected.entry && expected.descriptor) {
    throw oracleError('RUNTIME_ENTRY_MISMATCH', `${identity.entry} is not ${expected.entry}`);
  }
  if (identity.script !== expected.script) throw oracleError('RUNTIME_SCRIPT_MISMATCH', `${identity.script} is not ${expected.script}`);
  if (identity.style !== expected.style) throw oracleError('RUNTIME_STYLE_MISMATCH', `${identity.style} is not ${expected.style}`);
  if (expected.build && (!identity.build || !expected.build.startsWith(identity.build))) {
    throw oracleError('RUNTIME_BUILD_MISMATCH', `${identity.build} is not the expected build prefix`);
  }
  if (expected.descriptor && requireExecutingBuild && !identity.buildFromApplication) {
    throw oracleError('RUNTIME_BUILD_SOURCE', 'the executing document did not expose its compile-time build identity');
  }
}

export function assertOldIdentity(identity: RuntimeIdentity, baseline: PreparedBundle, expectedOrigin: string): void {
  assertStandalone(identity, expectedOrigin);
  assertRunningIdentity(identity, baseline.identity, false);
}

export function assertPhoneUpdateNotAcknowledged(evidence: UpdateCompletionEvidence): void {
  if (!evidence.rawPlanPresent) throw oracleError('PHONE_COMPLETION_EVIDENCE_MISSING', 'the app exposed no update progress record');
  if (!evidence.phoneRequired) throw oracleError('PHONE_PLAN_MISSING', 'the update plan did not contain a phone item');
  if (evidence.phoneAcknowledged || evidence.visibleCompletion) {
    throw oracleError('PREMATURE_PHONE_COMPLETION', `phone update reported completion during ${evidence.phoneState || 'asset failure'}`);
  }
}

export function assertPhoneUpdateAcknowledged(evidence: UpdateCompletionEvidence): void {
  if (!evidence.rawPlanPresent || !evidence.phoneRequired || !evidence.phoneAcknowledged || evidence.phoneState !== 'loaded' || !evidence.visibleCompletion) {
    throw oracleError('PHONE_COMPLETION_MISSING', 'the running candidate did not expose a completed phone acknowledgement');
  }
}

export function assertUpgradeDidNotComplete(
  acknowledged: boolean,
  runtime: RuntimeIdentity,
  target: BundleIdentity,
): void {
  if (!acknowledged) return;
  try {
    assertRunningIdentity(runtime, target);
  } catch (error) {
    throw oracleError('PREMATURE_PHONE_COMPLETION', error instanceof Error ? error.message : String(error));
  }
}

export function assertCredentialIdentityPreserved(
  before: RelayAuthEvidence,
  after: RelayAuthEvidence,
  relayNames = Object.keys(before.relays).sort(),
): void {
  if (!relayNames.length) throw oracleError('CREDENTIAL_RELAYS', 'no relays were included in credential evidence');
  for (const relayName of relayNames) {
    const previous = before.relays[relayName];
    const current = after.relays[relayName];
    if (!previous || !current) throw oracleError('CREDENTIAL_RELAY_MISSING', `${relayName} is missing from credential evidence`);
    if (previous.invitationAuthCount < 1) {
      throw oracleError('INVITATION_COUNT', `${relayName} did not complete bootstrap authentication`);
    }
    if (current.invitationAuthCount !== previous.invitationAuthCount) {
      throw oracleError('INVITATION_REUSED', `${relayName} invitation authentication changed from ${previous.invitationAuthCount} to ${current.invitationAuthCount}`);
    }
    const beforeIds = [...previous.credentialPseudonyms].sort();
    const afterIds = [...current.credentialPseudonyms].sort();
    if (JSON.stringify(beforeIds) !== JSON.stringify(afterIds)) {
      throw oracleError('CREDENTIAL_CHANGED', `${relayName} reconnect used a different credential identity`);
    }
  }
}

export function assertCredentialPreserved(
  before: RelayAuthEvidence,
  after: RelayAuthEvidence,
  relayNames = Object.keys(before.relays).sort(),
): void {
  assertCredentialIdentityPreserved(before, after, relayNames);
  for (const relayName of relayNames) {
    const previous = before.relays[relayName];
    const current = after.relays[relayName];
    if (!previous || !current || current.credentialAuthCount <= previous.credentialAuthCount) {
      throw oracleError('CREDENTIAL_NOT_USED', `${relayName} had no post-boundary credential-authenticated reconnect`);
    }
  }
}

export function assertPreferencePreserved(before: PreferenceEvidence, after: PreferenceEvidence): void {
  if (before.key !== after.key || before.value !== after.value) {
    throw oracleError('PREFERENCE_LOST', `${before.key} changed during the upgrade`);
  }
}

export function assertNoRelayInstall(count: number): void {
  if (count !== 0) throw oracleError('UNEXPECTED_RELAY_INSTALL', `${count} relay install commands were recorded`);
}

export function assertNoRelayDeploy(count: number): void {
  if (count !== 0) throw oracleError('UNEXPECTED_RELAY_DEPLOY', `${count} relay deploy commands were recorded`);
}

export function assertBoundedReloads(count: number, maximum = 2): void {
  if (!Number.isInteger(count) || count < 0 || count > maximum) {
    throw oracleError('RELOAD_BOUND_EXCEEDED', `${count} logical reloads exceed ${maximum}`);
  }
}
