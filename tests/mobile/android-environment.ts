import { readFile, stat } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { join, posix } from 'node:path';
import { requireOwnedDevice } from './support/device';
import { androidEventDetails, androidLogEvents, measuredAndroidEvents } from './android-events';
import { CommandError, command, type CommandResult } from './support/process';
import { redactText, writeSanitizedJson } from './support/diagnostics';

const ANDROID_PACKAGES = ['com.google.android.gms', 'com.google.android.trichromelibrary', 'com.android.chrome'] as const;
const PLAY_STORE_PACKAGE = 'com.android.vending';
const PACKAGE_DUMP_LIMIT = 2_000_000;
export const ANDROID_LOG_LIMIT = 104_857_600;
const ACQUISITION_COMMAND_LIMIT = 64;
const ACQUISITION_PREVIEW_LIMIT = 4_000;
const DEFAULT_ADB_TIMEOUT_MS = 30_000;

export interface AndroidEnvironmentPolicy {
  systemImage: string;
  systemImagePolicy: string;
  vendingPolicy: 'absent-or-disabled-user-0';
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
  installed: boolean;
  hidden: boolean;
  suspended: boolean;
  dependencyConfig: Record<string, string[]>;
  dependencyConfigSha256: string;
  dumpSha256: string;
  identitySha256: string;
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
  vending: AndroidVendingObservation;
  provenance: AndroidProvenance;
  packages: Record<string, AndroidPackageIdentity>;
  measurement?: { id: string; boundary: 'start' | 'end'; processes: Record<string, string> };
}

export interface AndroidVendingObservation {
  packagePresent: boolean;
  presence: 'absent' | 'installed';
  ordinaryListed: boolean;
  disabledListed: boolean;
  enabledListed: boolean;
  identity?: AndroidPackageIdentity;
}

interface AndroidProvenance {
  foregroundUser: 0;
  avdConfig: string;
  avdConfigSha256: string;
  sdkProperties: string;
  sdkPropertiesSha256: string;
  sdkRevision: string;
}

export interface AndroidPlannedTermination {
  id: string;
  measurementId: string;
  packageName: string;
  pid: string;
  processes: Record<string, string>;
  command: string[];
  succeeded: boolean;
}

export interface AndroidPreparation {
  schema: 1;
  serial: string;
  avdName: string;
  policy: AndroidEnvironmentPolicy;
  system: Record<string, string>;
  provenance: AndroidProvenance;
  before: AndroidVendingObservation;
  after?: AndroidVendingObservation;
  mutation: 'none' | 'disable-user-0';
}

export interface AndroidEnvironmentCheck {
  schema: 1;
  checkedAt: string;
  before: string;
  after: string;
  log: string;
  issues: string[];
  forcedRestartEvents: string[];
  nativeEvents: ReturnType<typeof androidEventDetails>[];
  passed: boolean;
  observability: string;
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
  packageContext?: string;
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
    if (stdout && !args.includes('getprop')) diagnostic.stdoutPreview = redactText(stdout).slice(0, ACQUISITION_PREVIEW_LIMIT);
    if (args.includes('dumpsys') && args.includes('package')) {
      diagnostic.packageContext = androidPackageContext(stdout);
    }
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

async function acquireSystemProperties(serial: string, timeout: number, diagnostics?: AcquisitionDiagnostics): Promise<Record<string, string>> {
  const properties: Record<string, string> = {};
  for (const key of [...Object.keys(systemProperties({})), 'ro.kernel.qemu']) {
    diagnostics?.setStage(`read required system property ${key}`);
    const response = await adb(serial, ['shell', 'getprop', key], timeout, diagnostics);
    const value = response.replace(/\r?\n$/u, '');
    if (Buffer.byteLength(response) > 4096 || !response.endsWith('\n') || !value
      || [...value].some((character) => character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127)) {
      throw new Error(`ANDROID_ENVIRONMENT: required system property ${key} has missing, malformed, truncated or oversized response`);
    }
    if (!value.trim() || value !== value.trim()) throw new Error(`ANDROID_ENVIRONMENT: required system property ${key} has empty or padded value`);
    properties[key] = value;
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

export function androidPackageContext(dump: string): string {
  const lines = dump.slice(0, PACKAGE_DUMP_LIMIT).split(/\r?\n/u);
  const selected = new Set<number>();
  for (let index = 0; index < lines.length; index++) {
    if (!/^\s*(?:Packages:|Hidden system packages:|Package \[|usesStaticLibraries:|static library:)/u.test(lines[index])) continue;
    for (let nearby = Math.max(0, index - 1); nearby <= Math.min(lines.length - 1, index + 8); nearby++) {
      selected.add(nearby);
    }
    if (selected.size >= 100) break;
  }
  return redactText([...selected].map((index) => `${index + 1}: ${lines[index].slice(0, 300)}`).join('\n')).slice(0, ACQUISITION_PREVIEW_LIMIT);
}

function parseStaticLibraryDependencies(dump: string, packageName: string): StaticLibraryDependency[] {
  if (dump.length > PACKAGE_DUMP_LIMIT) throw new Error('ANDROID_ENVIRONMENT: Chrome package dump exceeds parsing limit');
  const dependencies: StaticLibraryDependency[] = [];
  let packagesIndent = -1;
  let recordIndent = -1;
  let headingIndent = -1;
  let records = 0;
  let sections = 0;
  for (const rawLine of dump.split(/\r?\n/u)) {
    const line = rawLine.trim();
    if (!line) continue;
    const indent = rawLine.length - rawLine.trimStart().length;
    if (packagesIndent >= 0 && indent <= packagesIndent) packagesIndent = -1;
    if (recordIndent >= 0 && indent <= recordIndent) {
      recordIndent = -1;
      headingIndent = -1;
    }
    if (line === 'Packages:') {
      packagesIndent = indent;
      continue;
    }
    const record = line.match(/^Package \[([^\]]+)\]/u);
    if (packagesIndent >= 0 && record?.[1] === packageName) {
      records++;
      recordIndent = indent;
      continue;
    }
    if (recordIndent < 0) continue;
    if (headingIndent >= 0 && indent <= headingIndent) headingIndent = -1;
    if (line === 'usesStaticLibraries:') {
      sections++;
      headingIndent = indent;
      continue;
    }
    if (headingIndent < 0) continue;
    const match = line.match(/^(\S+)\s+version:(\d+)$/u);
    if (!match) throw new Error(`ANDROID_ENVIRONMENT: malformed Chrome static library record ${line.slice(0, 300)}`);
    dependencies.push({ name: match[1], versionCode: match[2] });
  }
  if (!records) throw new Error(`ANDROID_ENVIRONMENT: active package record ${packageName} is missing`);
  if (records !== 1 || sections > 1) throw new Error(`ANDROID_ENVIRONMENT: active package record ${packageName} or static library section is ambiguous`);
  return dependencies;
}

export function resolveStaticLibraryPackage(
  chromeDump: string,
  libraryName: string,
  declaredVersion: string,
  browserPackage = 'com.android.chrome',
): AndroidStaticLibraryResolution {
  const expected = expectedVersion(declaredVersion);
  if (!expected.code) throw new Error(`ANDROID_ENVIRONMENT: static library ${libraryName} requires a declared version code`);
  if (!/^[A-Za-z0-9._]+$/u.test(libraryName)) throw new Error(`ANDROID_ENVIRONMENT: invalid static library name ${libraryName}`);
  const matches = parseStaticLibraryDependencies(chromeDump, browserPackage).filter((dependency) => dependency.name === libraryName);
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

function staticLibraryPackageRecord(dump: string, resolution: AndroidStaticLibraryResolution): string {
  if (dump.length > PACKAGE_DUMP_LIMIT) throw new Error('ANDROID_ENVIRONMENT: static library dump exceeds parsing limit');
  const records: string[][] = [];
  const lines = completePackageLines(dump);
  const rootIndent = lines.find((line) => line.trim() === 'Compiler stats:')!.search(/\S/u);
  let packagesIndent = -1;
  let recordIndent = -1;
  for (const line of lines) {
    if (!line.trim()) continue;
    const indent = line.length - line.trimStart().length;
    if (packagesIndent >= 0 && indent <= packagesIndent) packagesIndent = -1;
    if (recordIndent >= 0 && indent <= recordIndent) recordIndent = -1;
    if (line.trim() === 'Packages:' && indent === rootIndent) {
      packagesIndent = indent;
      continue;
    }
    if (packagesIndent < 0) continue;
    if (/^Package \[/u.test(line.trim())) {
      records.push([line]);
      recordIndent = indent;
      continue;
    }
    if (recordIndent >= 0) records[records.length - 1].push(line);
  }
  if (records.length !== 1 || firstMatch(records[0][0], /Package \[([^\]]+)\]/u) !== resolution.packageRecordName) {
    throw new Error('ANDROID_ENVIRONMENT: static library package record does not match Chrome\'s dependency or is ambiguous');
  }
  return records[0].join('\n');
}

function staticLibrarySourcePath(listing: string, resolution: AndroidStaticLibraryResolution): string {
  if (listing.length > PACKAGE_DUMP_LIMIT) throw new Error('ANDROID_ENVIRONMENT: static library listing exceeds parsing limit');
  if (!listing) throw new Error('ANDROID_ENVIRONMENT: static library listing is missing');
  if (!listing.endsWith('\n')) throw new Error('ANDROID_ENVIRONMENT: static library listing is truncated');
  const matches: string[] = [];
  for (const line of listing.replace(/\r?\n$/u, '').split(/\r?\n/u)) {
    const record = line.match(/^package:(\/[^\s\p{Cc}]+)=([A-Za-z0-9._]+) versionCode:(\d+)$/u);
    if (!record || posix.normalize(record[1]) !== record[1] || !record[1].endsWith('.apk')) {
      throw new Error('ANDROID_ENVIRONMENT: malformed static library listing record');
    }
    if (record[2] === resolution.libraryName && record[3] === resolution.versionCode) matches.push(record[1]);
  }
  if (matches.length !== 1) throw new Error(`ANDROID_ENVIRONMENT: static library source path ${matches.length ? 'is ambiguous' : 'is missing'}`);
  return matches[0];
}

function policyFromToolchains(value: unknown): AndroidEnvironmentPolicy {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('ANDROID_ENVIRONMENT: toolchains file is not an object');
  const android = (value as ToolchainsFile).android;
  if (!android || typeof android !== 'object' || Array.isArray(android)) throw new Error('ANDROID_ENVIRONMENT: Android toolchain policy is missing');
  for (const key of ['systemImage', 'systemImagePolicy', 'vendingPolicy', 'browserPackage', 'browserVersion', 'trichromeLibraryPackage', 'trichromeLibraryVersion'] as const) {
    if (typeof android[key] !== 'string' || !android[key]) throw new Error(`ANDROID_ENVIRONMENT: invalid policy field ${key}`);
  }
  const policy = {
    systemImage: String(android.systemImage || ''),
    systemImagePolicy: String(android.systemImagePolicy || ''),
    vendingPolicy: android.vendingPolicy as AndroidEnvironmentPolicy['vendingPolicy'],
    browserPackage: String(android.browserPackage || ''),
    browserVersion: String(android.browserVersion || ''),
    trichromeLibraryPackage: String(android.trichromeLibraryPackage || ''),
    trichromeLibraryVersion: String(android.trichromeLibraryVersion || ''),
  } satisfies AndroidEnvironmentPolicy;
  if (!policy.systemImage || !policy.systemImagePolicy || !policy.browserPackage || !policy.browserVersion
    || !policy.trichromeLibraryPackage || !policy.trichromeLibraryVersion) {
    throw new Error('ANDROID_ENVIRONMENT: Android toolchain policy is incomplete');
  }
  if (policy.systemImage !== 'system-images;android-35;google_apis;x86_64'
    || policy.systemImagePolicy !== 'owned-google-apis-emulator'
    || policy.vendingPolicy !== 'absent-or-disabled-user-0'
    || policy.browserPackage !== 'com.android.chrome'
    || policy.trichromeLibraryPackage !== 'com.google.android.trichromelibrary') {
    throw new Error('ANDROID_ENVIRONMENT: expected owned Google APIs emulator with Vending absent or disabled-user for user 0');
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
    if (result.stderr.trim()) throw new Error(`ANDROID_ENVIRONMENT: unexpected adb stderr: ${redactText(result.stderr).slice(0, 1000)}`);
    return result.stdout;
  } catch (error) {
    diagnostics?.recordCommand('adb', ['-s', serial, ...args], undefined, error);
    throw error;
  }
}

async function hostVersion(binary: 'adb' | 'emulator'): Promise<string> {
  const args = binary === 'adb' ? ['version'] : ['-no-window', '-version'];
  const result = await command(binary, args, 10_000, { label: `${binary} version` });
  const pattern = binary === 'adb' ? /^Android Debug Bridge version [\d.]+\r?\nVersion [^\r\n]+$/gmu : /^Android emulator version [\d.]+[^\r\n]*$/gmu;
  const matches = [...`${result.stdout}${result.stderr}`.matchAll(pattern)];
  if (matches.length !== 1) throw new Error(`ANDROID_ENVIRONMENT: ${binary} version identity is missing or ambiguous`);
  return matches[0][0].replace(/\r/gu, '');
}

function boundedLines(source: string, label: string): string[] {
  if (Buffer.byteLength(source) > PACKAGE_DUMP_LIMIT) throw new Error(`ANDROID_ENVIRONMENT: ${label} exceeds parsing limit`);
  if (!source.endsWith('\n') || /DUMP TIMEOUT|DUMP SERVICE FAILED|\[REDACTED\]/u.test(source)) {
    throw new Error(`ANDROID_ENVIRONMENT: ${label} is truncated or sanitized`);
  }
  return source.replace(/\r/gu, '').split('\n');
}

function completePackageLines(dump: string): string[] {
  const lines = boundedLines(dump, 'package dump');
  const footer = lines.findIndex((line) => line.trim() === 'Compiler stats:');
  if (footer < 0 || !lines.slice(footer + 1).some((line) => line.trim())) throw new Error('ANDROID_ENVIRONMENT: package dump is truncated before its compiler footer');
  for (let index = 1; index < lines.length; index++) {
    if (!/^ optional:(?:true|false)$/u.test(lines[index])) continue;
    lines[index - 1] += lines[index];
    lines.splice(index--, 1);
  }
  return lines;
}

function activePackageRecord(dump: string, name: string): string {
  const lines = completePackageLines(dump);
  const rootIndent = lines.find((line) => line.trim() === 'Compiler stats:')!.search(/\S/u);
  const records: string[][] = [];
  let section = -1;
  let record = -1;
  for (const line of lines) {
    if (!line.trim()) continue;
    const indent = line.length - line.trimStart().length;
    if (section >= 0 && indent <= section) section = -1;
    if (record >= 0 && indent <= record) record = -1;
    if (line.trim() === 'Packages:' && indent === rootIndent) {
      section = indent;
      continue;
    }
    if (section < 0) continue;
    const match = line.trim().match(/^Package \[([^\]]+)\] \([^()]+\):$/u);
    if (match?.[1] === name) {
      records.push([line]);
      record = indent;
    } else if (record >= 0) {
      records.at(-1)!.push(line);
    }
  }
  if (records.length !== 1) throw new Error(`ANDROID_ENVIRONMENT: active package record ${name} is missing or ambiguous`);
  return records[0].join('\n') + '\n';
}

function field(lines: string[], name: string, pattern = /.+/u, preserveWhitespace = false): string {
  const values = lines.filter((line) => line.trimStart().startsWith(`${name}=`)).map((line) => {
    const value = line.trimStart().slice(name.length + 1);
    return preserveWhitespace ? value : value.trim();
  });
  if (values.length !== 1 || !pattern.test(values[0])) throw new Error(`ANDROID_ENVIRONMENT: ${name} is missing, malformed or ambiguous`);
  return values[0];
}

function namedSections(lines: string[], names: string[]): Record<string, string[]> {
  const sections: Record<string, string[]> = Object.fromEntries(names.map((name) => [name, []]));
  const seen = new Set<string>();
  for (let index = 0; index < lines.length; index++) {
    const line = lines[index];
    const name = line.trim().replace(/:$/u, '');
    if (!names.includes(name) || !line.endsWith(':')) continue;
    if (seen.has(name)) throw new Error(`ANDROID_ENVIRONMENT: ambiguous ${name} section`);
    seen.add(name);
    const indent = line.length - line.trimStart().length;
    while (index + 1 < lines.length && lines[index + 1].length - lines[index + 1].trimStart().length > indent) {
      const value = lines[++index].trim();
      if (!value || value.includes('...')) throw new Error(`ANDROID_ENVIRONMENT: malformed ${name} section`);
      sections[name].push(value);
    }
    if (!sections[name].length) throw new Error(`ANDROID_ENVIRONMENT: empty ${name} section`);
    sections[name].sort();
  }
  return sections;
}

function packageIdentity(packageName: string, record: string, paths: string): AndroidPackageIdentity {
  const lines = record.trimEnd().split('\n');
  const indent = lines[0].length - lines[0].trimStart().length + 2;
  const top = lines.slice(1).filter((line) => line.length - line.trimStart().length === indent);
  const users = top.filter((line) => /^User 0: /u.test(line.trim()));
  if (users.length !== 1) throw new Error(`ANDROID_ENVIRONMENT: ${packageName} is not installed for user 0: missing or ambiguous User 0`);
  const userFields = users[0].trim().slice('User 0: '.length).split(/ +/u);
  const userValue = (name: string, pattern: RegExp) => field(userFields, name, pattern);
  const userIndex = lines.indexOf(users[0]);
  let end = userIndex + 1;
  while (end < lines.length && lines[end].length - lines[end].trimStart().length > indent) end++;
  const user = lines.slice(userIndex + 1, end);
  const dependencyConfig: Record<string, string[]> = {
    ...namedSections(lines.slice(1, lines.findIndex((line) => /^User \d+:/u.test(line.trim()))), [
      'dynamic libraries', 'static library', 'SDK library', 'usesLibraries', 'usesStaticLibraries', 'usesSdkLibraries',
      'usesOptionalLibraries', 'usesNativeLibraries', 'usesOptionalNativeLibraries', 'usesLibraryFiles',
    ]),
    ...namedSections(user, ['enabledComponents', 'disabledComponents']),
    flags: [field(top, 'flags', /^\[[^\]]*\]$/u)],
    splits: [field(top, 'splits', /^\[[^\]]+\]$/u)],
  };
  for (const name of ['privateFlags', 'pkgFlags', 'privatePkgFlags', 'updateOwnerPackageName', 'apkSigningVersion']) {
    const values = top.filter((line) => line.trimStart().startsWith(`${name}=`));
    dependencyConfig[name] = values.length ? [field(values, name)] : [];
  }
  for (const [name, values] of Object.entries(dependencyConfig)) {
    if (new Set(values).size !== values.length) throw new Error(`ANDROID_ENVIRONMENT: duplicate ${name} values`);
    const grammar = name === 'usesStaticLibraries' ? /^\S+ version:\d+$/u
      : name === 'usesSdkLibraries' ? /^\S+ version:\d+ optional:(?:true|false)$/u
        : name === 'static library' ? /^name:\S+ version:\d+$/u
          : name === 'SDK library' ? /^name:\S+ versionMajor:\d+$/u
            : name === 'usesLibraryFiles' ? /^\/[^\s\p{Cc}]+$/u
              : /^(?:uses|dynamic libraries|enabledComponents|disabledComponents)/u.test(name) ? /^[A-Za-z0-9_.$+-]+$/u : undefined;
    if (grammar && values.some((value) => !grammar.test(value))) throw new Error(`ANDROID_ENVIRONMENT: malformed ${name} values`);
  }
  const staticLibrary = staticLibraryMetadata(record);
  const apkPaths = paths === '' ? [] : boundedLines(paths, 'APK paths').slice(0, -1);
  if (new Set(apkPaths).size !== apkPaths.length || apkPaths.some((line) => !/^package:\/[^\s\p{Cc}]+\.apk$/u.test(line)
    || posix.normalize(line.slice(8)) !== line.slice(8))) throw new Error('ANDROID_ENVIRONMENT: APK paths are malformed');
  const codePath = field(top, 'codePath', /^\/[^\s\p{Cc}]+$/u);
  if (posix.normalize(codePath) !== codePath || apkPaths.some((path) => posix.dirname(path.slice(8)) !== codePath)) {
    throw new Error('ANDROID_ENVIRONMENT: source path does not match installed code path');
  }
  const identity = {
    packageName,
    packageRecordName: packageRecordNameFromDump(record),
    staticLibraryName: staticLibrary.name,
    staticLibraryVersion: staticLibrary.versionCode,
    versionName: field(top, 'versionName', /^.+$/u, true),
    versionCode: field(top, 'versionCode', /^\d+ minSdk=\d+ targetSdk=\d+$/u).split(' ')[0],
    installerPackageName: field(top, 'installerPackageName', /^(?:null|[\w.]+)$/u),
    initiatingPackageName: field(top, 'initiatingPackageName', /^(?:null|[\w.]+)$/u),
    originatingPackageName: field(top, 'originatingPackageName', /^(?:null|[\w.]+)$/u),
    packageSource: field(top, 'packageSource', /^\d+$/u),
    firstInstallTime: field(user, 'firstInstallTime', /^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d$/u),
    lastUpdateTime: field(top, 'lastUpdateTime', /^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d$/u),
    enabled: userValue('enabled', /^[0-4]$/u),
    installed: userValue('installed', /^(?:true|false)$/u) === 'true',
    hidden: userValue('hidden', /^(?:true|false)$/u) === 'true',
    suspended: userValue('suspended', /^(?:true|false)$/u) === 'true',
    codePath,
    apkPaths: apkPaths.sort(),
    dependencyConfig,
    dependencyConfigSha256: sha256(JSON.stringify(dependencyConfig)),
  };
  return { ...identity, dumpSha256: sha256(record), identitySha256: sha256(JSON.stringify(identity)) };
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
    for (const key of ['packageRecordName', 'staticLibraryName', 'staticLibraryVersion', 'versionName', 'versionCode', 'installerPackageName', 'initiatingPackageName', 'originatingPackageName', 'packageSource', 'firstInstallTime', 'lastUpdateTime', 'enabled', 'installed', 'hidden', 'suspended', 'codePath', 'dependencyConfigSha256', 'identitySha256'] as const) {
      if (previous[key] !== current[key]) issues.push(`${packageName} ${key} changed`);
    }
    if (JSON.stringify(previous.apkPaths) !== JSON.stringify(current.apkPaths)) issues.push(`${packageName} APK paths changed`);
    if (JSON.stringify(previous.dependencyConfig) !== JSON.stringify(current.dependencyConfig)) issues.push(`${packageName} dependency configuration changed`);
  }
  return issues;
}

export function forcedRestartEvents(log: string, processes: Record<string, string> = {}): string[] {
  return androidLogEvents(log, processes);
}

export function compareAndroidEnvironment(
  before: AndroidEnvironmentSnapshot,
  after: AndroidEnvironmentSnapshot,
): string[] {
  const issues: string[] = [];
  if (before.serial !== after.serial) issues.push('device serial changed');
  if (before.avdName !== after.avdName) issues.push('AVD identity changed');
  if (JSON.stringify(before.policy) !== JSON.stringify(after.policy)) issues.push('environment policy changed');
  if (JSON.stringify(before.system) !== JSON.stringify(after.system)) issues.push('system image identity changed');
  if (before.emulatorVersion !== after.emulatorVersion) issues.push('emulator version changed');
  if (before.adbVersion !== after.adbVersion) issues.push('ADB version changed');
  if (JSON.stringify(stableVending(before.vending)) !== JSON.stringify(stableVending(after.vending))) issues.push('Vending presence, user state or package identity changed');
  for (const value of [before.vending, after.vending]) {
    try { requireVendingPolicy(value); } catch (error) { issues.push(String(error)); }
  }
  if (JSON.stringify(before.provenance) !== JSON.stringify(after.provenance)) issues.push('owned emulator provenance changed');
  issues.push(...packageMapEqual(before.packages, after.packages));
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
  const provenance = await ownedProvenance(serial, policy, diagnostics, adbTimeoutMs);

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
    policy.browserPackage,
  );
  diagnostics?.setResolvedStaticLibrary(staticLibrary);
  diagnostics?.setStage(`read ${staticLibrary.packageRecordName} package metadata`);
  const libraryDump = await adb(
    serial,
    ['shell', 'dumpsys', 'package', staticLibrary.packageRecordName],
    adbTimeoutMs,
    diagnostics,
  );
  diagnostics?.setStage('validate static library package record');
  packageDumps[policy.trichromeLibraryPackage] = staticLibraryPackageRecord(libraryDump, staticLibrary) + '\n';
  diagnostics?.setStage(`read ${staticLibrary.packageRecordName} APK paths`);
  const librarySourcePath = staticLibrarySourcePath(await adb(
    serial,
    ['shell', 'pm', 'list', 'packages', '--match-libraries', '-f', '--show-versioncode', '--user', '0', staticLibrary.libraryName],
    adbTimeoutMs,
    diagnostics,
  ), staticLibrary);
  packagePaths[policy.trichromeLibraryPackage] = `package:${librarySourcePath}\n`;

  const packageIdentities = Object.fromEntries(ANDROID_PACKAGES.map((packageName) => [
    packageName,
    packageIdentity(packageName, packageName === policy.trichromeLibraryPackage ? packageDumps[packageName] : activePackageRecord(packageDumps[packageName], packageName), packagePaths[packageName]),
  ]));
  diagnostics?.setStage('validate Android package identities');
  for (const packageName of ANDROID_PACKAGES) {
    requireInstalledPackage(packageIdentities[packageName], packageName);
    if (!packageIdentities[packageName].installed || !['0', '1'].includes(packageIdentities[packageName].enabled)
      || packageIdentities[packageName].hidden || packageIdentities[packageName].suspended) {
      throw new Error(`ANDROID_ENVIRONMENT: ${packageName} is not installed for user 0 or is not enabled`);
    }
    if (packageName !== policy.trichromeLibraryPackage) packageIdentities[packageName].dumpSha256 = sha256(packageDumps[packageName]);
  }
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
  const libraryRecord = packageDumps[policy.trichromeLibraryPackage];
  const splits = [...libraryRecord.matchAll(/^[ \t]+splits=([^\r\n]+)$/gmu)];
  if (splits.length !== 1 || splits[0][1] !== '[base]') {
    throw new Error('ANDROID_ENVIRONMENT: static library split layout must be base-only');
  }
  const users = [...libraryRecord.matchAll(/^[ \t]+User 0: ([^\r\n]+)$/gmu)];
  if (users.length !== 1 || !/(?:^| )installed=true(?: |$)/u.test(users[0][1])) {
    throw new Error('ANDROID_ENVIRONMENT: static library is not installed for user 0');
  }
  if (librarySourcePath !== `${libraryIdentity.codePath}/base.apk`) {
    throw new Error('ANDROID_ENVIRONMENT: static library source path does not match its installed code path');
  }
  libraryIdentity.dumpSha256 = sha256(libraryDump);
  diagnostics?.setStage('verify static library APK exists');
  await adb(serial, ['shell', 'test', '-f', `'${librarySourcePath.replaceAll("'", "'\\''")}'`], adbTimeoutMs, diagnostics);

  const vending = await observeVending(serial, diagnostics, adbTimeoutMs);
  requireVendingPolicy(vending);
  return {
    schema: 1,
    capturedAt: new Date().toISOString(),
    serial,
    ...provenance,
    policy,
    emulatorVersion: await hostVersion('emulator'),
    adbVersion: await hostVersion('adb'),
    vending,
    packages: packageIdentities,
  };
}

function packageMembership(source: string): boolean {
  if (!source) return false;
  const lines = boundedLines(source, 'package listing').slice(0, -1);
  if (new Set(lines).size !== lines.length || lines.some((line) => !/^package:[A-Za-z0-9_.]+$/u.test(line))) {
    throw new Error('ANDROID_ENVIRONMENT: malformed or ambiguous package listing');
  }
  return lines.includes(`package:${PLAY_STORE_PACKAGE}`);
}

async function observeVending(serial: string, diagnostics: AcquisitionDiagnostics | undefined, timeout: number): Promise<AndroidVendingObservation> {
  diagnostics?.setStage('observe Vending user 0 package state');
  const query = (args: string[]) => adb(serial, ['shell', ...args], timeout, diagnostics);
  const ordinaryListed = packageMembership(await query(['pm', 'list', 'packages', '--user', '0', PLAY_STORE_PACKAGE]));
  const disabledListed = packageMembership(await query(['pm', 'list', 'packages', '-d', '--user', '0', PLAY_STORE_PACKAGE]));
  const enabledListed = packageMembership(await query(['pm', 'list', 'packages', '-e', '--user', '0', PLAY_STORE_PACKAGE]));
  const dump = await query(['dumpsys', 'package', PLAY_STORE_PACKAGE]);
  const packagePresent = dump.replace(/\r/gu, '') !== `Unable to find package: ${PLAY_STORE_PACKAGE}\n`;
  let identity: AndroidPackageIdentity | undefined;
  if (packagePresent) {
    const record = activePackageRecord(dump, PLAY_STORE_PACKAGE);
    identity = packageIdentity(PLAY_STORE_PACKAGE, record, '');
    if (identity.installed) identity = packageIdentity(PLAY_STORE_PACKAGE, record, await query(['pm', 'path', '--user', '0', PLAY_STORE_PACKAGE]));
    identity.dumpSha256 = sha256(dump);
    if (identity.installed) requireInstalledPackage(identity, PLAY_STORE_PACKAGE);
  }
  if (ordinaryListed !== Boolean(identity?.installed) || (disabledListed && enabledListed)
    || ordinaryListed !== (disabledListed || enabledListed)
    || (identity?.installed && ['2', '3', '4'].includes(identity.enabled) && !disabledListed)
    || (identity?.installed && identity.enabled === '1' && !enabledListed)) {
    throw new Error('ANDROID_ENVIRONMENT: Vending exact package/user readback disagrees');
  }
  return { packagePresent, presence: ordinaryListed ? 'installed' : 'absent', ordinaryListed, disabledListed, enabledListed, identity };
}

function requireVendingPolicy(value: AndroidVendingObservation): void {
  if (![value.packagePresent, value.ordinaryListed, value.disabledListed, value.enabledListed].every((entry) => typeof entry === 'boolean')
    || value.packagePresent !== Boolean(value.identity)
    || (value.identity && (value.identity.packageName !== PLAY_STORE_PACKAGE || !/^[a-f0-9]{64}$/u.test(value.identity.identitySha256)))) {
    throw new Error('ANDROID_ENVIRONMENT: Vending observation is incomplete');
  }
  if (value.presence === 'absent' && !value.ordinaryListed && !value.disabledListed && !value.enabledListed
    && (!value.identity || value.identity.installed === false)) return;
  if (value.presence === 'installed' && value.packagePresent && value.ordinaryListed && value.disabledListed && !value.enabledListed
    && value.identity?.installed && value.identity.enabled === '3') return;
  throw new Error('ANDROID_ENVIRONMENT: Vending must be absent or explicitly disabled-user for user 0');
}

async function ownedProvenance(serial: string, policy: AndroidEnvironmentPolicy, diagnostics?: AcquisitionDiagnostics, timeout = DEFAULT_ADB_TIMEOUT_MS): Promise<{
  avdName: string; system: Record<string, string>; provenance: AndroidProvenance;
}> {
  if (!/^emulator-\d+$/u.test(serial)) throw new Error('ANDROID_ENVIRONMENT: refusing a non-emulator');
  await requireOwnedDevice('android', serial);
  diagnostics?.setStage('verify owned AVD, foreground user and system image provenance');
  const expectedAvd = process.env.ANDROID_AVD_NAME || '';
  if (!/^herdr-mobile-ci-[A-Za-z0-9-]+$/u.test(expectedAvd)) throw new Error('ANDROID_ENVIRONMENT: owned AVD name is required');
  const response = (await adb(serial, ['emu', 'avd', 'name'], timeout, diagnostics)).replace(/\r/gu, '');
  if (response !== `${expectedAvd}\nOK\n`) throw new Error('ANDROID_ENVIRONMENT: owned AVD identity mismatch');
  const user = await adb(serial, ['shell', 'am', 'get-current-user'], timeout, diagnostics);
  if (user.replace(/\r/gu, '') !== '0\n') throw new Error('ANDROID_ENVIRONMENT: foreground user must be exactly 0');
  const properties = await acquireSystemProperties(serial, timeout, diagnostics);
  const system = systemProperties(properties);
  if (Object.values(system).some((value) => !value) || system['ro.build.version.sdk'] !== '35' || properties['ro.kernel.qemu'] !== '1') {
    throw new Error('ANDROID_ENVIRONMENT: missing or unexpected emulator system identity');
  }
  if (!process.env.ANDROID_AVD_HOME || !process.env.ANDROID_HOME) throw new Error('ANDROID_ENVIRONMENT: SDK and AVD directories are required');
  const avdConfig = await readFile(join(process.env.ANDROID_AVD_HOME, `${expectedAvd}.avd`, 'config.ini'), 'utf8');
  const sdkProperties = await readFile(join(process.env.ANDROID_HOME, ...policy.systemImage.split(';'), 'source.properties'), 'utf8');
  const ini = (source: string) => {
    const result: Record<string, string> = {};
    for (const line of boundedLines(source, 'image provenance')) {
      if (!line.trim() || /^\s*[#;]/u.test(line)) continue;
      const match = line.match(/^([^=\s]+)\s*=\s*(.*?)\s*$/u);
      if (!match || Object.hasOwn(result, match[1])) throw new Error('ANDROID_ENVIRONMENT: malformed or ambiguous image provenance');
      result[match[1]] = match[2];
    }
    return result;
  };
  const config = ini(avdConfig);
  const sdk = ini(sdkProperties);
  const imageDirectory = policy.systemImage.split(';').join('/') + '/';
  if (config['image.sysdir.1'] !== imageDirectory || config['tag.id'] !== 'google_apis' || config['abi.type'] !== 'x86_64'
    || sdk['AndroidVersion.ApiLevel'] !== '35' || sdk['SystemImage.TagId'] !== 'google_apis' || sdk['SystemImage.Abi'] !== 'x86_64'
    || !/^\d+(?:\.\d+)*$/u.test(sdk['Pkg.Revision'] || '')) throw new Error('ANDROID_ENVIRONMENT: SDK/AVD image provenance mismatch');
  return {
    avdName: expectedAvd, system,
    provenance: { foregroundUser: 0, avdConfig, avdConfigSha256: sha256(avdConfig), sdkProperties, sdkPropertiesSha256: sha256(sdkProperties), sdkRevision: sdk['Pkg.Revision'] },
  };
}

async function runPrepare(): Promise<void> {
  const serial = required('--serial');
  const output = required('--output');
  const diagnostics = new AcquisitionDiagnostics(serial);
  try {
    const policy = await readPolicy(required('--toolchains'));
    diagnostics.setPolicy(policy);
    const timeout = adbTimeoutOption();
    const provenance = await ownedProvenance(serial, policy, diagnostics, timeout);
    const before = await observeVending(serial, diagnostics, timeout);
    const preparation: AndroidPreparation = { schema: 1, serial, policy, ...provenance, before, mutation: 'none' };
    await writeSanitizedJson(output, preparation);
    if (before.presence === 'installed' && before.identity?.enabled !== '3') {
      preparation.mutation = 'disable-user-0';
      await writeSanitizedJson(output, preparation);
      diagnostics.setStage('disable Vending for owned emulator user 0');
      const result = await adb(serial, ['shell', 'pm', 'disable-user', '--user', '0', PLAY_STORE_PACKAGE], timeout, diagnostics);
      if (result.replace(/\r/gu, '') !== `Package ${PLAY_STORE_PACKAGE} new state: disabled-user\n`) {
        throw new Error('ANDROID_ENVIRONMENT: disable-user returned unexpected state');
      }
    }
    const afterProvenance = await ownedProvenance(serial, policy, diagnostics, timeout);
    if (JSON.stringify(provenance) !== JSON.stringify(afterProvenance)) throw new Error('ANDROID_ENVIRONMENT: preparation provenance changed');
    preparation.after = await observeVending(serial, diagnostics, timeout);
    await writeSanitizedJson(output, preparation);
    requireVendingPolicy(preparation.after);
    const previous = structuredClone(before);
    if (preparation.mutation === 'disable-user-0' && previous.identity) {
      previous.identity.enabled = '3';
      const { dumpSha256, identitySha256, ...stableIdentity } = previous.identity;
      previous.identity = { ...stableIdentity, dumpSha256, identitySha256: sha256(JSON.stringify(stableIdentity)) };
      if (!identitySha256) throw new Error('ANDROID_ENVIRONMENT: preparation identity hash is missing');
      previous.enabledListed = false;
      previous.disabledListed = true;
    }
    if (JSON.stringify(stableVending(previous)) !== JSON.stringify(stableVending(preparation.after))) {
      throw new Error('ANDROID_ENVIRONMENT: Vending identity changed during preparation');
    }
    await writeSanitizedJson(diagnosticsFilename(output), diagnostics.value);
  } catch (error) {
    diagnostics.recordFailure(error);
    await writeSanitizedJson(diagnosticsFilename(output), diagnostics.value);
    throw error;
  }
}

function stableVending(value: AndroidVendingObservation): unknown {
  if (!value.identity) return value;
  const identity = { ...value.identity, dumpSha256: '' };
  return { ...value, identity };
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
    const boundary = option('--boundary');
    const id = option('--measurement');
    if (boundary && (!['start', 'end'].includes(boundary) || !id || !/^[A-Za-z0-9-]{1,80}$/u.test(id))) {
      throw new Error('ANDROID_ENVIRONMENT: invalid measurement boundary');
    }
    const processes = boundary ? parseAndroidProcesses(await adb(serial, ['shell', 'ps', '-A', '-o', 'PID,NAME'], adbTimeoutOption(), diagnostics)) : undefined;
    if (boundary === 'start') await androidMeasurementMarker(serial, `${id} START`);
    const value = await snapshot(serial, policy, diagnostics, adbTimeoutOption());
    if (boundary) value.measurement = { id: id!, boundary: boundary as 'start' | 'end', processes: processes! };
    if (boundary === 'end') await androidMeasurementMarker(serial, `${id} END`);
    await writeSanitizedJson(output, value);
    await writeSanitizedJson(diagnosticsOutput, diagnostics.value);
  } catch (error) {
    diagnostics.recordFailure(error);
    await writeSanitizedJson(diagnosticsOutput, diagnostics.value).catch((diagnosticError: unknown) => {
      process.stderr.write(`ANDROID_ENVIRONMENT: acquisition diagnostics unavailable: ${diagnosticError instanceof Error ? diagnosticError.message : String(diagnosticError)}\n`);
    });
    throw error;
  }
}

export async function androidMeasurementMarker(serial: string, message: string): Promise<void> {
  if (!/^[A-Za-z0-9 ._-]{1,300}$/u.test(message)) throw new Error('ANDROID_ENVIRONMENT: invalid measurement marker');
  await adb(serial, ['shell', 'log', '-p', 'i', '-t', 'HerdrMeasure', `'${message}'`]);
}

export function parseAndroidProcesses(source: string): Record<string, string> {
  const lines = boundedLines(source, 'process list').filter((line) => line.trim());
  if (lines.shift()?.trim().replace(/\s+/gu, ' ') !== 'PID NAME') throw new Error('ANDROID_ENVIRONMENT: malformed process header');
  const processes: Record<string, string> = {};
  for (const line of lines) {
    const match = line.trim().match(/^([1-9]\d*)\s+(\S+)$/u);
    if (!match || Object.hasOwn(processes, match[1])) throw new Error('ANDROID_ENVIRONMENT: malformed process identity');
    processes[match[1]] = match[2];
  }
  if (!Object.keys(processes).length) throw new Error('ANDROID_ENVIRONMENT: empty process inventory');
  return processes;
}

async function readSnapshot(filename: string): Promise<AndroidEnvironmentSnapshot> {
  if ((await stat(filename)).size > 8_000_000) throw new Error('snapshot exceeds bound');
  const value = JSON.parse(await readFile(filename, 'utf8')) as AndroidEnvironmentSnapshot;
  if (value.schema !== 1 || !value.provenance || !value.vending || !value.measurement || !value.measurement.processes
    || !value.packages || !value.system || Object.values(value.system).some((entry) => typeof entry !== 'string' || !entry)) {
    throw new Error('incomplete snapshot');
  }
  policyFromToolchains({ android: value.policy });
  if (!/^emulator-\d+$/u.test(value.serial) || !/^herdr-mobile-ci-[A-Za-z0-9-]+$/u.test(value.avdName)
    || !Number.isFinite(Date.parse(value.capturedAt)) || value.provenance.foregroundUser !== 0
    || !value.provenance.avdConfig || !value.provenance.sdkProperties || !value.provenance.sdkRevision
    || !/^[a-f0-9]{64}$/u.test(value.provenance.avdConfigSha256) || !/^[a-f0-9]{64}$/u.test(value.provenance.sdkPropertiesSha256)
    || Object.values(systemProperties(value.system)).some((entry) => !entry) || value.system['ro.build.version.sdk'] !== '35'
    || !Object.keys(value.measurement.processes).length || Object.entries(value.measurement.processes).some(([pid, name]) => !/^[1-9]\d*$/u.test(pid) || typeof name !== 'string' || !name)) {
    throw new Error('incomplete measured emulator identity');
  }
  for (const packageName of ANDROID_PACKAGES) {
    const identity = value.packages[packageName];
    requireInstalledPackage(identity, packageName);
    if (!identity.installed || identity.hidden || identity.suspended || !['0', '1'].includes(identity.enabled) || !identity.dependencyConfig
      || !/^[a-f0-9]{64}$/u.test(identity.dependencyConfigSha256) || !/^[a-f0-9]{64}$/u.test(identity.dumpSha256)
      || !/^[a-f0-9]{64}$/u.test(identity.identitySha256) || !/^\d+$/u.test(identity.versionCode)
      || identity.packageName !== packageName || typeof identity.hidden !== 'boolean' || typeof identity.suspended !== 'boolean'
      || !identity.firstInstallTime || !identity.lastUpdateTime || !identity.installerPackageName || !identity.packageSource
      || !identity.initiatingPackageName || !identity.originatingPackageName
      || Object.values(identity.dependencyConfig).some((section) => !Array.isArray(section) || section.some((entry) => typeof entry !== 'string'))
      || !identity.dependencyConfig.flags?.length || !identity.dependencyConfig.splits?.length) throw new Error('incomplete package identity');
  }
  if (!packageVersionMatches(value.packages[value.policy.browserPackage], value.policy.browserPackage, value.policy.browserVersion)
    || !packageVersionMatches(value.packages[value.policy.trichromeLibraryPackage], value.policy.trichromeLibraryPackage, value.policy.trichromeLibraryVersion)) throw new Error('snapshot pinned package identity mismatch');
  return value;
}

async function runCheck(): Promise<void> {
  const beforeFile = required('--before');
  const afterFile = required('--after');
  const logFile = required('--log');
  let events: string[] = [];
  const issues: string[] = [];
  try {
    const before = await readSnapshot(beforeFile);
    const after = await readSnapshot(afterFile);
    issues.push(...compareAndroidEnvironment(before, after));
    if (Date.parse(after.capturedAt) < Date.parse(before.capturedAt)) issues.push('snapshot time order is invalid');
    const size = (await stat(logFile)).size;
    if (!size || size > ANDROID_LOG_LIMIT) throw new Error('measurement log is empty or exceeds its bound');
    const log = await readFile(logFile, 'utf8');
    const operationsFile = required('--operations');
    if ((await stat(operationsFile)).size > 64_000) throw new Error('measurement operations exceed bound');
    const operations = JSON.parse(await readFile(operationsFile, 'utf8')) as AndroidPlannedTermination[];
    if (!Array.isArray(operations) || operations.length > 8) throw new Error('measurement operations are malformed');
    const measured = measuredAndroidEvents(log, before, after, operations);
    events = measured.events;
    issues.push(...measured.issues);
    if (events.length) issues.push('native process death, dependency configuration change or package replacement was observed');
  } catch (error) {
    issues.push(`measurement evidence unavailable or invalid: ${error instanceof Error ? error.message : String(error)}`);
  }
  const result: AndroidEnvironmentCheck = {
    schema: 1,
    checkedAt: new Date().toISOString(),
    before: beforeFile,
    after: afterFile,
    log: logFile,
    issues,
    forcedRestartEvents: events,
    nativeEvents: events.map(androidEventDetails),
    passed: issues.length === 0,
    observability: 'PackageManager persistent dependency sections and supported PID/package-attributed native log events only; dumpsys package does not expose a complete runtime Dynamite/Chimera module inventory.',
  };
  const output = option('--output');
  if (output) await writeSanitizedJson(output, result);
  if (issues.length) throw new Error(`ANDROID_ENVIRONMENT: ${issues.join('; ')}`);
}

async function main(): Promise<void> {
  const mode = process.argv[2];
  if (mode === 'prepare') {
    await runPrepare();
    return;
  }
  if (mode === 'snapshot') {
    await runSnapshot();
    return;
  }
  if (mode === 'check') {
    await runCheck();
    return;
  }
  throw new Error('ANDROID_ENVIRONMENT: expected prepare, snapshot or check');
}

if (import.meta.main) {
  main().catch((error: unknown) => {
    process.stderr.write(`${error instanceof Error ? error.stack || error.message : String(error)}\n`);
    process.exitCode = 1;
  });
}
