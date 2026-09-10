import { readFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { command } from './support/process';
import { writeSanitizedJson } from './support/diagnostics';

const ANDROID_PACKAGES = ['com.google.android.gms', 'com.google.android.trichromelibrary', 'com.android.chrome'] as const;
const PLAY_STORE_PACKAGE = 'com.android.vending';
const PACKAGE_DUMP_LIMIT = 2_000_000;
const MODULE_CONFIG_LIMIT = 500;

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
  versionName: string;
  versionCode: string;
  installerPackageName: string;
  initiatingPackageName: string;
  originatingPackageName: string;
  packageSource: string;
  firstInstallTime: string;
  lastUpdateTime: string;
  enabled: string;
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

async function adb(serial: string, args: string[], timeoutMs = 30_000): Promise<string> {
  return (await command('adb', ['-s', serial, ...args], timeoutMs, { label: `adb ${args.join(' ')}` })).stdout;
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
  return {
    packageName,
    versionName: firstMatch(boundedDump, /\bversionName=([^\s]+)/u),
    versionCode: firstMatch(boundedDump, /\bversionCode=(\d+)/u),
    installerPackageName: firstMatch(boundedDump, /\binstallerPackageName=([^\s]+)/u),
    initiatingPackageName: firstMatch(boundedDump, /\binitiatingPackageName=([^\s]+)/u),
    originatingPackageName: firstMatch(boundedDump, /\boriginatingPackageName=([^\s]+)/u),
    packageSource: firstMatch(boundedDump, /\bpackageSource=([^\s]+)/u),
    firstInstallTime: firstMatch(boundedDump, /\bfirstInstallTime=([^\r\n]+)/u),
    lastUpdateTime: firstMatch(boundedDump, /\blastUpdateTime=([^\r\n]+)/u),
    enabled: firstMatch(boundedDump, /\benabled=([^\s]+)/u),
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
    for (const key of ['versionName', 'versionCode', 'installerPackageName', 'initiatingPackageName', 'originatingPackageName', 'packageSource', 'firstInstallTime', 'lastUpdateTime', 'enabled', 'moduleConfigSha256'] as const) {
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

async function snapshot(serial: string, policy: AndroidEnvironmentPolicy): Promise<AndroidEnvironmentSnapshot> {
  if (!serial) throw new Error('ANDROID_ENVIRONMENT: device serial is required');
  const properties = parseProperties(await adb(serial, ['shell', 'getprop']));
  const avdName = (await adb(serial, ['emu', 'avd', 'name'])).replace(/\r/g, '').split('\n').find((line) => line && line !== 'OK') || '';
  const packageDumps: Record<string, string> = {};
  const packagePaths: Record<string, string> = {};
  for (const packageName of ANDROID_PACKAGES) {
    packageDumps[packageName] = await adb(serial, ['shell', 'dumpsys', 'package', packageName]);
    packagePaths[packageName] = await adb(serial, ['shell', 'pm', 'path', packageName]);
  }
  const packageIdentities = Object.fromEntries(ANDROID_PACKAGES.map((packageName) => [
    packageName,
    packageIdentity(packageName, packageDumps[packageName], packagePaths[packageName]),
  ]));
  if (!packageVersionMatches(packageIdentities[policy.browserPackage], policy.browserPackage, policy.browserVersion)) {
    throw new Error(`ANDROID_ENVIRONMENT: ${policy.browserPackage} does not match the pinned browser identity`);
  }
  if (!packageVersionMatches(packageIdentities[policy.trichromeLibraryPackage], policy.trichromeLibraryPackage, policy.trichromeLibraryVersion)) {
    throw new Error(`ANDROID_ENVIRONMENT: ${policy.trichromeLibraryPackage} does not match the pinned library identity`);
  }
  const installedPackages = await adb(serial, ['shell', 'pm', 'list', 'packages', PLAY_STORE_PACKAGE]);
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

async function runSnapshot(): Promise<void> {
  const serial = required('--serial');
  const output = required('--output');
  const toolchains = required('--toolchains');
  const policy = await readPolicy(toolchains);
  await writeSanitizedJson(output, await snapshot(serial, policy));
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
