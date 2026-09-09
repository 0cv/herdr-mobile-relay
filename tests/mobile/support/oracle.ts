import type { BundleIdentity, PreparedBundle } from './artifacts';

export interface RuntimeIdentity {
  url: string;
  origin: string;
  standalone: boolean;
  provider: string;
  nativeProvider?: string;
  nativeActivity?: string;
  nativePid?: string;
  navigationId?: string;
  version: string;
  assets: number;
  build: string;
  buildFromApplication?: boolean;
  entry: string;
  script: string;
  style: string;
  requiredAssetsReady: boolean;
  requiredAssetFailure?: boolean;
  failureUiVisible?: boolean;
  applicationInitialized: boolean;
}

export interface RelayAuthRecord {
  invitationAuthCount: number;
  credentialAuthCount: number;
  credentialPseudonyms: string[];
  connections?: number;
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

export interface QualificationFailureSnapshot {
  code: string;
  stage: string;
  message: string;
  detail?: unknown;
}

export class QualificationFatalError extends Error {
  readonly code: string;
  readonly stage: string;
  readonly detail?: unknown;

  constructor(snapshot: QualificationFailureSnapshot, cause?: unknown) {
    super(`${snapshot.code}: ${snapshot.message}`, { cause });
    this.name = 'QualificationFatalError';
    this.code = snapshot.code;
    this.stage = snapshot.stage;
    this.detail = snapshot.detail;
  }

  snapshot(): QualificationFailureSnapshot {
    return { code: this.code, stage: this.stage, message: this.message.slice(this.code.length + 2), ...(this.detail === undefined ? {} : { detail: this.detail }) };
  }
}

export class QualificationFailureLatch {
  private first?: QualificationFatalError;
  private lastDetail?: unknown;

  observe(detail: unknown): void {
    this.lastDetail = detail;
  }

  fail(error: unknown, stage: string, detail?: unknown): never {
    if (!this.first) {
      const message = error instanceof Error ? error.message : String(error);
      const parsed = message.match(/^([A-Z][A-Z0-9_]+):\s*(.*)$/u);
      const code = error instanceof QualificationFatalError
        ? error.code
        : parsed?.[1] || 'QUALIFICATION_FAILURE';
      const detailMessage = error instanceof QualificationFatalError
        ? message
        : parsed?.[1] === code ? parsed[2] : message;
      if (error instanceof QualificationFatalError) {
        this.first = error.detail === undefined && this.lastDetail !== undefined
          ? new QualificationFatalError({ ...error.snapshot(), detail: this.lastDetail }, error)
          : error;
      } else {
        this.first = new QualificationFatalError({ code, stage, message: detailMessage, detail: detail === undefined ? this.lastDetail : detail }, error);
      }
    }
    throw this.first;
  }

  assertClear(): void {
    if (this.first) throw this.first;
  }

  snapshot(): QualificationFailureSnapshot | undefined {
    return this.first?.snapshot();
  }
}

export function isQualificationFatal(error: unknown): error is QualificationFatalError {
  return error instanceof QualificationFatalError;
}

export function qualificationFatal(code: string, message: string, stage: string, detail?: unknown): QualificationFatalError {
  return new QualificationFatalError({ code, stage, message, ...(detail === undefined ? {} : { detail }) });
}

export function oracleError(code: string, detail: string): Error {
  return new Error(`${code}: ${detail}`);
}

function ownershipError(code: string, detail: string): QualificationFatalError {
  return qualificationFatal(code, detail, 'ownership');
}

export function assertStandaloneOwnership(identity: RuntimeIdentity, expectedOrigin: string): void {
  if (!identity.standalone) throw oracleError('STANDALONE_REQUIRED', 'the observed document is not in standalone display mode');
  if (identity.origin !== expectedOrigin) throw ownershipError('ORIGIN_MISMATCH', `${identity.origin} is not ${expectedOrigin}`);
  if (identity.provider === 'browser' || identity.provider === 'unknown' || !identity.nativeProvider) {
    throw ownershipError('STANDALONE_PROVIDER_REQUIRED', 'the observed document is not an installed standalone web app with a native provider');
  }
  const ios = identity.provider === 'ios-home-screen';
  const providerPrefix = ios ? 'ios:' : 'android:';
  if (!identity.nativeProvider.startsWith(providerPrefix)) {
    throw ownershipError('STANDALONE_PROVIDER_REQUIRED', `native provider ${identity.nativeProvider} does not match ${ios ? 'iOS' : 'Android'}`);
  }
  if (!identity.nativePid || (!ios && !identity.nativeActivity)) {
    throw ownershipError(
      'STANDALONE_PROVIDER_REQUIRED',
      ios
        ? 'the installed iOS web app has no foreground bundle or pid evidence'
        : 'the installed Android web app has no foreground activity or pid evidence',
    );
  }
}

export function assertStandalone(identity: RuntimeIdentity, expectedOrigin: string): void {
  assertStandaloneOwnership(identity, expectedOrigin);
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

export function assertInvitationOwnership(
  evidence: RelayAuthEvidence,
  relayNames = Object.keys(evidence.relays).sort(),
): void {
  if (!relayNames.length) throw ownershipError('CREDENTIAL_RELAYS', 'no relays were included in invitation evidence');
  for (const relayName of relayNames) {
    const relay = evidence.relays[relayName];
    if (!relay) throw ownershipError('CREDENTIAL_RELAY_MISSING', `${relayName} is missing from invitation evidence`);
    if (relay.invitationAuthCount < 1) throw ownershipError('INVITATION_COUNT', `${relayName} did not complete invitation authentication`);
  }
}

export function assertRelayOwnership(
  evidence: RelayAuthEvidence,
  relayNames = Object.keys(evidence.relays).sort(),
): void {
  if (!relayNames.length) throw ownershipError('CREDENTIAL_RELAYS', 'no relays were included in ownership evidence');
  for (const relayName of relayNames) {
    const relay = evidence.relays[relayName];
    if (!relay) throw ownershipError('CREDENTIAL_RELAY_MISSING', `${relayName} is missing from ownership evidence`);
    if (relay.invitationAuthCount < 1) throw ownershipError('INVITATION_COUNT', `${relayName} did not complete invitation authentication`);
    if (relay.credentialAuthCount < 1 || relay.credentialPseudonyms.length < 1) {
      throw ownershipError('CREDENTIAL_OWNERSHIP_MISSING', `${relayName} has no established credential identity`);
    }
    if (relay.connections === undefined || relay.connections < 1) {
      throw ownershipError('CREDENTIAL_CONNECTION_MISSING', `${relayName} has no active credential-owned connection`);
    }
  }
}

export function assertPhoneUpdateNotAcknowledged(evidence: UpdateCompletionEvidence): void {
  if (!evidence.rawPlanPresent) throw qualificationFatal('PHONE_COMPLETION_EVIDENCE_MISSING', 'the app exposed no update progress record', 'upgrade');
  if (!evidence.phoneRequired) throw qualificationFatal('PHONE_PLAN_MISSING', 'the update plan did not contain a phone item', 'upgrade');
  if (evidence.phoneAcknowledged || evidence.visibleCompletion) {
    throw qualificationFatal('PREMATURE_PHONE_COMPLETION', `phone update reported completion during ${evidence.phoneState || 'asset failure'}`, 'upgrade');
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
  if (!relayNames.length) throw ownershipError('CREDENTIAL_RELAYS', 'no relays were included in credential evidence');
  for (const relayName of relayNames) {
    const previous = before.relays[relayName];
    const current = after.relays[relayName];
    if (!previous || !current) throw ownershipError('CREDENTIAL_RELAY_MISSING', `${relayName} is missing from credential evidence`);
    if (previous.invitationAuthCount < 1) {
      throw ownershipError('INVITATION_COUNT', `${relayName} did not complete bootstrap authentication`);
    }
    if (current.invitationAuthCount !== previous.invitationAuthCount) {
      throw ownershipError('INVITATION_REUSED', `${relayName} invitation authentication changed from ${previous.invitationAuthCount} to ${current.invitationAuthCount}`);
    }
    const beforeIds = [...previous.credentialPseudonyms].sort();
    const afterIds = [...current.credentialPseudonyms].sort();
    if (JSON.stringify(beforeIds) !== JSON.stringify(afterIds)) {
      throw ownershipError('CREDENTIAL_CHANGED', `${relayName} reconnect used a different credential identity`);
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
      throw ownershipError('CREDENTIAL_NOT_USED', `${relayName} had no post-boundary credential-authenticated reconnect`);
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
