import { readFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { CommandError, command, type CommandResult } from './support/process';
import { redactText, writeSanitizedJson } from './support/diagnostics';

const ANDROID_PACKAGES = ['com.google.android.gms', 'com.google.android.trichromelibrary', 'com.android.chrome'] as const;
const PLAY_STORE_PACKAGE = 'com.android.vending';
const PACKAGE_DUMP_LIMIT = 2_000_000;
const MODULE_CONFIG_LIMIT = 500;
const ACQUISITION_COMMAND_LIMIT = 64;
const ACQUISITION_PREVIEW_LIMIT = 4_000;
const DEFAULT_ADB_TIMEOUT_MS = 30_000;

export interface AndroidEnvironmentPolicy {
  systemImage: string;
  systemImagePolicy: string;
  playStore: boolean;
  browserPackage: string;
  browserVersion: string;
  trichromeLibraryPackage: string;
  trichromeLibraryVersion: string;
}

export interface AndroidPackageIdentity {
  packageName: string;
  packageRecordName: string;
  staticLibraryName: string;
  staticLibraryVersion: string;
  versionName: string;
  versionCode: string;
  installerPackageName: string;
  initiatingPackageName: string;
  originatingPackageName: string;
  packageSource: string;
  firstInstallTime: string;
  lastUpdateTime: string;
  enabled: string;
  codePath: string;
  apkPaths: string[];
  moduleConfig: string[];
  moduleConfigSha256: string;
  dumpSha256: string;
}

export interface AndroidEnvironmentSnapshot {
  schema: 1;
  capturedAt: string;
  serial: string;
  avdName: string;
  policy: AndroidEnvironmentPolicy;
  emulatorVersion: string;
  adbVersion: string;
  system: Record<string, string>;
  playStoreInstalled: boolean;
  packages: Record<string, AndroidPackageIdentity>;
}

export interface AndroidEnvironmentCheck {
  schema: 1;
  checkedAt: string;
  before: string;
  after: string;
  log: string;
  issues: string[];
  forcedRestartEvents: string[];
  passed: boolean;
}

interface ToolchainsFile {
  android?: Partial<AndroidEnvironmentPolicy>;
}

export interface AndroidStaticLibraryResolution {
  libraryName: string;
  versionCode: string;
  packageRecordName: string;
}

export interface AndroidAcquisitionCommandDiagnostic {
  args: string[];
  outcome: 'passed' | 'failed';
  durationMs: number;
  exitCode: number;
  timedOut: boolean;
  signal?: string;
  stdoutBytes: number;
  stderrBytes: number;
  stdoutSha256: string;
  stderrSha256: string;
  stdoutPreview?: string;
  stderrPreview?: string;
}

export interface AndroidEnvironmentAcquisitionDiagnostics {
  schema: 1;
  capturedAt: string;
  serial: string;
  policy?: AndroidEnvironmentPolicy;
  stage: string;
  resolvedStaticLibrary?: AndroidStaticLibraryResolution;
  commands: AndroidAcquisitionCommandDiagnostic[];
  failure?: {
    stage: string;
    message: string;
    code?: string;
    exitCode?: number;
    timedOut?: boolean;
    signal?: string;
    detail?: string;
  };
}

class AcquisitionDiagnostics {
  readonly value: AndroidEnvironmentAcquisitionDiagnostics;

  constructor(serial: string) {
    this.value = {
      schema: 1,
      capturedAt: new Date().toISOString(),
      serial,
      stage: 'initialization',
      commands: [],
    };
  }

  setPolicy(policy: AndroidEnvironmentPolicy): void {
    this.value.policy = policy;
  }

  setStage(stage: string): void {
    this.value.stage = stage;
  }

  setResolvedStaticLibrary(resolution: AndroidStaticLibraryResolution): void {
    this.value.resolvedStaticLibrary = resolution;
  }

  recordCommand(binary: string, args: string[], result?: CommandResult, error?: unknown): void {
    if (this.value.commands.length >= ACQUISITION_COMMAND_LIMIT) return;
    const commandError = error instanceof CommandError ? error : undefined;
    const stdout = commandError?.stdout || result?.stdout || '';
    const stderr = commandError?.stderr || result?.stderr || '';
    const diagnostic: AndroidAcquisitionCommandDiagnostic = {
      args: [binary, ...args].map((value) => redactText(value).slice(0, 300)),
      outcome: error ? 'failed' : 'passed',
      durationMs: commandError?.durationMs || result?.durationMs || 0,
      exitCode: commandError?.exitCode ?? result?.code ?? 0,
      timedOut: commandError?.timedOut ?? result?.timedOut ?? false,
      signal: commandError?.signal || result?.signal,
      stdoutBytes: Buffer.byteLength(stdout),
      stderrBytes: Buffer.byteLength(stderr),
      stdoutSha256: sha256(stdout),
      stderrSha256: sha256(stderr),
    };
    if (stdout) diagnostic.stdoutPreview = redactText(stdout).slice(0, ACQUISITION_PREVIEW_LIMIT);
    if (stderr) diagnostic.stderrPreview = redactText(stderr).slice(0, ACQUISITION_PREVIEW_LIMIT);
    this.value.commands.push(diagnostic);
  }

  recordFailure(error: unknown): void {
    const commandError = error instanceof CommandError ? error : undefined;
    const detail = commandError?.stderr || commandError?.stdout || '';
    this.value.failure = {
      stage: this.value.stage,
      message: redactText(error instanceof Error ? error.message : String(error)).slice(0, 1_000),
      code: commandError?.code,
      exitCode: commandError?.exitCode,
      timedOut: commandError?.timedOut,
      signal: commandError?.signal,
      detail: redactText(detail).slice(0, ACQUISITION_PREVIEW_LIMIT) || undefined,
    };
  }
}

function option(name: string): string | undefined {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

function required(name: string): string {
  const value = option(name);
  if (!value) throw new Error(`ANDROID_ENVIRONMENT: missing ${name}`);
  return value;
}

function sha256(value: string): string {
  return createHash('sha256').update(value).digest('hex');
}

function firstMatch(source: string, pattern: RegExp): string {
  return source.match(pattern)?.[1]?.trim() || '';
}

function parseProperties(source: string): Record<string, string> {
  const properties: Record<string, string> = {};
  for (const line of source.split(/\r?\n/u)) {
    const match = line.match(/^\[([^\]]+)\]: \[([^\]]*)\]$/u);
    if (match) properties[match[1]] = match[2];
  }
  return properties;
}

function expectedVersion(value: string): { name: string; code: string } {
  const match = value.match(/^([^\s(]+)(?:\s+\((\d+)\))?$/u);
  if (!match) throw new Error(`ANDROID_ENVIRONMENT: invalid declared package version ${value}`);
  return { name: match[1], code: match[2] || '' };
}

interface StaticLibraryDependency {
  name: string;
  versionCode: string;
}

function parseStaticLibraryDependencies(dump: string): StaticLibraryDependency[] {
  const dependencies: StaticLibraryDependency[] = [];
  let inSection = false;
  for (const rawLine of dump.slice(0, PACKAGE_DUMP_LIMIT).split(/\r?\n/u)) {
    const line = rawLine.trim();
    if (line === 'usesStaticLibraries:') {
      inSection = true;
      continue;
    }
    if (!inSection) continue;
    if (!/^[ \t]{4,}\S/u.test(rawLine)) {
      inSection = false;
      continue;
    }
    if (!line) continue;
    const match = line.match(/^(\S+)\s+version:(\d+)$/u);
    if (!match) throw new Error(`ANDROID_ENVIRONMENT: malformed Chrome static library record ${line}`);
    dependencies.push({ name: match[1], versionCode: match[2] });
  }
  return dependencies;
}

export function resolveStaticLibraryPackage(
  chromeDump: string,
  libraryName: string,
  declaredVersion: string,
): AndroidStaticLibraryResolution {
  const expected = expectedVersion(declaredVersion);
  if (!expected.code) throw new Error(`ANDROID_ENVIRONMENT: static library ${libraryName} requires a declared version code`);
  if (!/^[A-Za-z0-9._]+$/u.test(libraryName)) throw new Error(`ANDROID_ENVIRONMENT: invalid static library name ${libraryName}`);
  const matches = parseStaticLibraryDependencies(chromeDump).filter((dependency) => dependency.name === libraryName);
  if (!matches.length) throw new Error(`ANDROID_ENVIRONMENT: Chrome static library dependency ${libraryName} is missing`);
  if (matches.length !== 1) throw new Error(`ANDROID_ENVIRONMENT: Chrome static library dependency ${libraryName} is ambiguous`);
  const dependency = matches[0];
  if (dependency.versionCode !== expected.code) {
    throw new Error(`ANDROID_ENVIRONMENT: Chrome static library ${libraryName} version ${dependency.versionCode} does not match ${expected.code}`);
  }
  return {
    libraryName: dependency.name,
    versionCode: dependency.versionCode,
    packageRecordName: `${dependency.name}_${dependency.versionCode}`,
  };
}

function packageRecordNameFromDump(dump: string): string {
  const boundedDump = dump.slice(0, PACKAGE_DUMP_LIMIT);
  return firstMatch(boundedDump, /^[ \t]+compat name=([^\s]+)[ \t]*$/mu)
    || firstMatch(boundedDump, /^[ \t]*Package \[([^\]]+)\]/mu);
}

function staticLibraryMetadata(dump: string): { name: string; versionCode: string } {
  const match = dump.slice(0, PACKAGE_DUMP_LIMIT).match(
    /^[ \t]+static library:[ \t]*\r?\n[ \t]+name:([^\s]+)[ \t]+version:(\d+)[ \t]*$/mu,
  );
  return { name: match?.[1] || '', versionCode: match?.[2] || '' };
}

function policyFromToolchains(value: unknown): AndroidEnvironmentPolicy {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('ANDROID_ENVIRONMENT: toolchains file is not an object');
  const android = (value as ToolchainsFile).android;
  if (!android) throw new Error('ANDROID_ENVIRONMENT: Android toolchain policy is missing');
  const policy = {
    systemImage: String(android.systemImage || ''),
    systemImagePolicy: String(android.systemImagePolicy || ''),
    playStore: android.playStore === true,
    browserPackage: String(android.browserPackage || ''),
    browserVersion: String(android.browserVersion || ''),
    trichromeLibraryPackage: String(android.trichromeLibraryPackage || ''),
    trichromeLibraryVersion: String(android.trichromeLibraryVersion || ''),
  } satisfies AndroidEnvironmentPolicy;
  if (!policy.systemImage || !policy.systemImagePolicy || !policy.browserPackage || !policy.browserVersion
    || !policy.trichromeLibraryPackage || !policy.trichromeLibraryVersion) {
    throw new Error('ANDROID_ENVIRONMENT: Android toolchain policy is incomplete');
  }
  if (android.playStore !== false || policy.systemImagePolicy !== 'google-apis-without-play-store' || policy.playStore) {
    throw new Error('ANDROID_ENVIRONMENT: the selected policy must use Google APIs without the Play Store');
  }
  return policy;
}

async function readPolicy(filename: string): Promise<AndroidEnvironmentPolicy> {
  return policyFromToolchains(JSON.parse(await readFile(filename, 'utf8')));
}

async function adb(
  serial: string,
  args: string[],
  timeoutMs = DEFAULT_ADB_TIMEOUT_MS,
  diagnostics?: AcquisitionDiagnostics,
): Promise<string> {
  try {
    const result = await command('adb', ['-s', serial, ...args], timeoutMs, { label: `adb ${args.join(' ')}` });
    diagnostics?.recordCommand('adb', ['-s', serial, ...args], result);
    return result.stdout;
  } catch (error) {
    diagnostics?.recordCommand('adb', ['-s', serial, ...args], undefined, error);
    throw error;
  }
}

async function optionalHostCommand(binary: string, args: string[]): Promise<string> {
  try {
    const result = await command(binary, args, 10_000, { label: `${binary} ${args.join(' ')}` });
    return `${result.stdout}${result.stderr}`;
  } catch {
    return '';
  }
}

function packageIdentity(packageName: string, dump: string, paths: string): AndroidPackageIdentity {
  const boundedDump = dump.slice(0, PACKAGE_DUMP_LIMIT);
  const moduleConfig = boundedDump.split(/\r?\n/u)
    .filter((line) => /(?:chimera|dynamite|module|config|googlecertificates|staticlibraries|useslibrary)/iu.test(line))
    .map((line) => line.trim())
    .filter(Boolean)
    .slice(0, MODULE_CONFIG_LIMIT);
  const staticLibrary = staticLibraryMetadata(boundedDump);
  return {
    packageName,
    packageRecordName: packageRecordNameFromDump(boundedDump),
    staticLibraryName: staticLibrary.name,
    staticLibraryVersion: staticLibrary.versionCode,
    versionName: firstMatch(boundedDump, /\bversionName=([^\s]+)/u),
    versionCode: firstMatch(boundedDump, /\bversionCode=(\d+)/u),
    installerPackageName: firstMatch(boundedDump, /\binstallerPackageName=([^\s]+)/u),
    initiatingPackageName: firstMatch(boundedDump, /\binitiatingPackageName=([^\s]+)/u),
    originatingPackageName: firstMatch(boundedDump, /\boriginatingPackageName=([^\s]+)/u),
    packageSource: firstMatch(boundedDump, /\bpackageSource=([^\s]+)/u),
    firstInstallTime: firstMatch(boundedDump, /\bfirstInstallTime=([^\r\n]+)/u),
    lastUpdateTime: firstMatch(boundedDump, /\blastUpdateTime=([^\r\n]+)/u),
    enabled: firstMatch(boundedDump, /\benabled=([^\s]+)/u),
    codePath: firstMatch(boundedDump, /^[ \t]+codePath=([^\r\n]+)$/mu),
    apkPaths: paths.split(/\r?\n/u)
      .map((line) => line.trim())
      .filter((line) => line.startsWith('package:'))
      .sort(),
    moduleConfig,
    moduleConfigSha256: sha256(moduleConfig.join('\n')),
    dumpSha256: sha256(boundedDump),
  };
}

function packageVersionMatches(identity: AndroidPackageIdentity, packageName: string, declared: string): boolean {
  const expected = expectedVersion(declared);
  return identity.packageName === packageName
    && identity.versionName === expected.name
    && (!expected.code || identity.versionCode === expected.code);
}

function systemProperties(properties: Record<string, string>): Record<string, string> {
  return Object.fromEntries([
    'ro.build.fingerprint',
    'ro.build.id',
    'ro.build.version.incremental',
    'ro.build.version.release',
    'ro.build.version.sdk',
    'ro.product.name',
    'ro.product.device',
  ].map((key) => [key, properties[key] || '']));
}

function packageMapEqual(before: Record<string, AndroidPackageIdentity>, after: Record<string, AndroidPackageIdentity>): string[] {
  const issues: string[] = [];
  for (const packageName of ANDROID_PACKAGES) {
    const previous = before[packageName];
    const current = after[packageName];
    if (!previous || !current) {
      issues.push(`${packageName} package identity is missing`);
      continue;
    }
    for (const key of ['packageRecordName', 'staticLibraryName', 'staticLibraryVersion', 'versionName', 'versionCode', 'installerPackageName', 'initiatingPackageName', 'originatingPackageName', 'packageSource', 'firstInstallTime', 'lastUpdateTime', 'enabled', 'codePath', 'moduleConfigSha256'] as const) {
      if (previous[key] !== current[key]) issues.push(`${packageName} ${key} changed`);
    }
    if (JSON.stringify(previous.apkPaths) !== JSON.stringify(current.apkPaths)) issues.push(`${packageName} APK paths changed`);
    if (JSON.stringify(previous.moduleConfig) !== JSON.stringify(current.moduleConfig)) issues.push(`${packageName} module configuration changed`);
  }
  return issues;
}

export function forcedRestartEvents(log: string): string[] {
  const events: string[] = [];
  let moduleEventWindow = 0;
  for (const sourceLine of log.split(/\r?\n/u)) {
    const line = sourceLine.trim();
    if (!line) continue;
    const moduleEvent = /(?:chimera|dynamite|module).*(?:no existing config|regenerating config|updating module config|module config changed|forcing restart)/iu.test(line);
    const packageEvent = /(?:PackageManager|PackageInstaller|installd).*(?:com\.google\.android\.gms|com\.google\.android\.trichromelibrary|com\.android\.chrome).*(?:install|update|replace|changed|version)/iu.test(line)
      || /(?:com\.google\.android\.gms|com\.google\.android\.trichromelibrary|com\.android\.chrome).*(?:install|update|replace|module config)/iu.test(line);
    if (moduleEvent || packageEvent) {
      events.push(line);
      moduleEventWindow = moduleEvent ? 12 : 0;
      continue;
    }
    if (moduleEventWindow > 0 && /sending signal.*(?:SIG:\s*9|signal\s+9)/iu.test(line)) events.push(line);
    moduleEventWindow = Math.max(0, moduleEventWindow - 1);
  }
  return events;
}

export function compareAndroidEnvironment(
  before: AndroidEnvironmentSnapshot,
  after: AndroidEnvironmentSnapshot,
  log = '',
): string[] {
  const issues: string[] = [];
  if (before.serial !== after.serial) issues.push('device serial changed');
  if (before.avdName !== after.avdName) issues.push('AVD identity changed');
  if (JSON.stringify(before.policy) !== JSON.stringify(after.policy)) issues.push('environment policy changed');
  if (JSON.stringify(before.system) !== JSON.stringify(after.system)) issues.push('system image identity changed');
  if (before.emulatorVersion !== after.emulatorVersion) issues.push('emulator version changed');
  if (before.adbVersion !== after.adbVersion) issues.push('ADB version changed');
  if (before.playStoreInstalled !== after.playStoreInstalled) issues.push('Play Store installation state changed');
  if (after.playStoreInstalled) issues.push('Play Store is installed under the selected policy');
  issues.push(...packageMapEqual(before.packages, after.packages));
  if (forcedRestartEvents(log).length) issues.push('native dependency replacement or forced restart was observed');
  return [...new Set(issues)];
}

function requireInstalledPackage(identity: AndroidPackageIdentity, label: string): void {
  if (!identity.packageRecordName) throw new Error(`ANDROID_ENVIRONMENT: ${label} package record is missing`);
  if (!identity.versionName || !identity.versionCode) throw new Error(`ANDROID_ENVIRONMENT: ${label} version identity is incomplete`);
  if (!identity.codePath || !identity.codePath.startsWith('/')) throw new Error(`ANDROID_ENVIRONMENT: ${label} code path is missing`);
  if (!identity.apkPaths.length || identity.apkPaths.some((path) => !path.startsWith('package:/'))) {
    throw new Error(`ANDROID_ENVIRONMENT: ${label} APK paths are missing or malformed`);
  }
}

function adbTimeoutOption(): number {
  const value = option('--adb-timeout-ms');
  if (!value) return DEFAULT_ADB_TIMEOUT_MS;
  if (!/^\d+$/u.test(value) || Number(value) < 1 || !Number.isSafeInteger(Number(value))) {
    throw new Error(`ANDROID_ENVIRONMENT: invalid --adb-timeout-ms ${value}`);
  }
  return Number(value);
}

async function snapshot(
  serial: string,
  policy: AndroidEnvironmentPolicy,
  diagnostics?: AcquisitionDiagnostics,
  adbTimeoutMs = DEFAULT_ADB_TIMEOUT_MS,
): Promise<AndroidEnvironmentSnapshot> {
  if (!serial) throw new Error('ANDROID_ENVIRONMENT: device serial is required');
  diagnostics?.setStage('read Android system properties');
  const properties = parseProperties(await adb(serial, ['shell', 'getprop'], adbTimeoutMs, diagnostics));
  diagnostics?.setStage('read Android AVD identity');
  const avdName = (await adb(serial, ['emu', 'avd', 'name'], adbTimeoutMs, diagnostics)).replace(/\r/g, '').split('\n').find((line) => line && line !== 'OK') || '';

  const packageDumps: Record<string, string> = {};
  const packagePaths: Record<string, string> = {};
  for (const packageName of ['com.google.android.gms', 'com.android.chrome'] as const) {
    diagnostics?.setStage(`read ${packageName} package metadata`);
    packageDumps[packageName] = await adb(serial, ['shell', 'dumpsys', 'package', packageName], adbTimeoutMs, diagnostics);
    diagnostics?.setStage(`read ${packageName} APK paths`);
    packagePaths[packageName] = await adb(serial, ['shell', 'pm', 'path', packageName], adbTimeoutMs, diagnostics);
  }

  diagnostics?.setStage('resolve Chrome static library dependency');
  const staticLibrary = resolveStaticLibraryPackage(
    packageDumps[policy.browserPackage],
    policy.trichromeLibraryPackage,
    policy.trichromeLibraryVersion,
  );
  diagnostics?.setResolvedStaticLibrary(staticLibrary);
  diagnostics?.setStage(`read ${staticLibrary.packageRecordName} package metadata`);
  packageDumps[policy.trichromeLibraryPackage] = await adb(
    serial,
    ['shell', 'dumpsys', 'package', staticLibrary.packageRecordName],
    adbTimeoutMs,
    diagnostics,
  );
  diagnostics?.setStage(`read ${staticLibrary.packageRecordName} APK paths`);
  packagePaths[policy.trichromeLibraryPackage] = await adb(
    serial,
    ['shell', 'pm', 'path', staticLibrary.packageRecordName],
    adbTimeoutMs,
    diagnostics,
  );

  const packageIdentities = Object.fromEntries(ANDROID_PACKAGES.map((packageName) => [
    packageName,
    packageIdentity(packageName, packageDumps[packageName], packagePaths[packageName]),
  ]));
  diagnostics?.setStage('validate Android package identities');
  for (const packageName of ANDROID_PACKAGES) requireInstalledPackage(packageIdentities[packageName], packageName);
  for (const packageName of ['com.google.android.gms', 'com.android.chrome'] as const) {
    if (packageIdentities[packageName].packageRecordName !== packageName) {
      throw new Error(`ANDROID_ENVIRONMENT: ${packageName} package record is unexpected`);
    }
  }
  if (!packageVersionMatches(packageIdentities[policy.browserPackage], policy.browserPackage, policy.browserVersion)) {
    throw new Error(`ANDROID_ENVIRONMENT: ${policy.browserPackage} does not match the pinned browser identity`);
  }
  const libraryIdentity = packageIdentities[policy.trichromeLibraryPackage];
  if (libraryIdentity.packageRecordName !== staticLibrary.packageRecordName) {
    throw new Error(`ANDROID_ENVIRONMENT: ${policy.trichromeLibraryPackage} package record does not match Chrome's dependency`);
  }
  if (libraryIdentity.staticLibraryName !== staticLibrary.libraryName || libraryIdentity.staticLibraryVersion !== staticLibrary.versionCode) {
    throw new Error(`ANDROID_ENVIRONMENT: ${policy.trichromeLibraryPackage} static library metadata does not match Chrome's dependency`);
  }
  if (!packageVersionMatches(libraryIdentity, policy.trichromeLibraryPackage, policy.trichromeLibraryVersion)) {
    throw new Error(`ANDROID_ENVIRONMENT: ${policy.trichromeLibraryPackage} does not match the pinned library identity`);
  }

  diagnostics?.setStage('check Play Store installation state');
  const installedPackages = await adb(serial, ['shell', 'pm', 'list', 'packages', PLAY_STORE_PACKAGE], adbTimeoutMs, diagnostics);
  const playStoreInstalled = installedPackages.split(/\r?\n/u).some((line) => line.trim() === `package:${PLAY_STORE_PACKAGE}`);
  if (!policy.playStore && playStoreInstalled) throw new Error('ANDROID_ENVIRONMENT: Play Store is installed under the selected policy');
  return {
    schema: 1,
    capturedAt: new Date().toISOString(),
    serial,
    avdName,
    policy,
    emulatorVersion: await optionalHostCommand('emulator', ['-version']),
    adbVersion: await optionalHostCommand('adb', ['version']),
    system: systemProperties(properties),
    playStoreInstalled,
    packages: packageIdentities,
  };
}

function diagnosticsFilename(output: string): string {
  const explicit = option('--diagnostics');
  if (explicit) return explicit;
  return output.endsWith('.json') ? `${output.slice(0, -5)}-diagnostics.json` : `${output}.diagnostics.json`;
}

async function runSnapshot(): Promise<void> {
  const serial = required('--serial');
  const output = required('--output');
  const toolchains = required('--toolchains');
  const diagnosticsOutput = diagnosticsFilename(output);
  const diagnostics = new AcquisitionDiagnostics(serial);
  try {
    diagnostics.setStage('read Android toolchain policy');
    const policy = await readPolicy(toolchains);
    diagnostics.setPolicy(policy);
    await writeSanitizedJson(output, await snapshot(serial, policy, diagnostics, adbTimeoutOption()));
    await writeSanitizedJson(diagnosticsOutput, diagnostics.value);
  } catch (error) {
    diagnostics.recordFailure(error);
    await writeSanitizedJson(diagnosticsOutput, diagnostics.value).catch((diagnosticError: unknown) => {
      process.stderr.write(`ANDROID_ENVIRONMENT: acquisition diagnostics unavailable: ${diagnosticError instanceof Error ? diagnosticError.message : String(diagnosticError)}\n`);
    });
    throw error;
  }
}

async function runCheck(): Promise<void> {
  const beforeFile = required('--before');
  const afterFile = required('--after');
  const logFile = required('--log');
  const before = JSON.parse(await readFile(beforeFile, 'utf8')) as AndroidEnvironmentSnapshot;
  const after = JSON.parse(await readFile(afterFile, 'utf8')) as AndroidEnvironmentSnapshot;
  const log = await readFile(logFile, 'utf8').catch(() => '');
  const events = forcedRestartEvents(log);
  const issues = compareAndroidEnvironment(before, after, log);
  const result: AndroidEnvironmentCheck = {
    schema: 1,
    checkedAt: new Date().toISOString(),
    before: beforeFile,
    after: afterFile,
    log: logFile,
    issues,
    forcedRestartEvents: events,
    passed: issues.length === 0,
  };
  const output = option('--output');
  if (output) await writeSanitizedJson(output, result);
  if (issues.length) throw new Error(`ANDROID_ENVIRONMENT: ${issues.join('; ')}`);
}

async function main(): Promise<void> {
  const mode = process.argv[2];
  if (mode === 'snapshot') {
    await runSnapshot();
    return;
  }
  if (mode === 'check') {
    await runCheck();
    return;
  }
  throw new Error('ANDROID_ENVIRONMENT: expected snapshot or check');
}

if (import.meta.main) {
  main().catch((error: unknown) => {
    process.stderr.write(`${error instanceof Error ? error.stack || error.message : String(error)}\n`);
    process.exitCode = 1;
  });
}
