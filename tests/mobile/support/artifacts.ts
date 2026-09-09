import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { lstat, mkdir, readFile, readdir, rm, stat, writeFile, cp } from 'node:fs/promises';
import { join, relative, resolve, sep } from 'node:path';

const MAX_ARCHIVE_BYTES = 512 * 1024 * 1024;
const MAX_ARCHIVE_ENTRIES = 20_000;
const MAX_WEB_FILES = 20_000;
const SHA256 = /^[a-f0-9]{64}$/;
let verifiedTar: string | undefined;

function tarBinary(): string {
  if (verifiedTar) return verifiedTar;
  const candidates = process.env.MOBILE_GNU_TAR ? [process.env.MOBILE_GNU_TAR] : process.platform === 'darwin' ? ['gtar', 'tar'] : ['tar'];
  for (const candidate of candidates) {
    if (!candidate) continue;
    try {
      const version = execFileSync(candidate, ['--version'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
      if (/GNU tar/iu.test(version)) {
        verifiedTar = candidate;
        return candidate;
      }
    } catch {
      continue;
    }
  }
  throw new Error('ARTIFACT_TAR_REQUIRED: bundle extraction requires GNU tar; set MOBILE_GNU_TAR to a GNU tar binary');
}

export interface BundleExpectation {
  name: string;
  version: string;
  assets: number;
  sourceRelease: string;
  sourceCommit: string;
  archive?: string;
  url?: string;
  archiveSha256?: string;
  webHash?: string;
  revision?: string;
  entry?: string;
  script?: string;
  style?: string;
  build?: string;
}

export interface BundleIdentity {
  version: string;
  assets: number;
  build: string;
  entry: string;
  script: string;
  style: string;
  scriptSha256: string;
  styleSha256: string;
  webHash: string;
  descriptor: boolean;
}

export interface PreparedBundle {
  name: string;
  provenance: BundleExpectation;
  root: string;
  identity: BundleIdentity;
  archiveSha256: string;
}

export interface BundleSet {
  schema: 1;
  candidate: PreparedBundle;
  baselines: PreparedBundle[];
  generatedAt: string;
}

interface ArchiveEntry {
  name: string;
  size: number;
  type: string;
}

export function sha256(data: Uint8Array | string): string {
  return createHash('sha256').update(data).digest('hex');
}

export async function fileSha256(filename: string): Promise<string> {
  return sha256(await readFile(filename));
}

export function integrityFor(hash: string): string {
  return `sha256-${Buffer.from(hash, 'hex').toString('base64')}`;
}

export function safeRelativePath(value: string): boolean {
  if (!value || value.startsWith('/') || value.includes('\\') || value.includes('\0')) return false;
  const pieces = value.split('/');
  if (pieces.some((piece) => !piece || piece === '.' || piece === '..')) return false;
  return !value.startsWith('../');
}

async function walkFiles(root: string, current = root): Promise<string[]> {
  const entries = await readdir(current, { withFileTypes: true });
  const files: string[] = [];
  for (const entry of entries) {
    const filename = join(current, entry.name);
    const info = await lstat(filename);
    if (info.isSymbolicLink()) throw new Error(`ARTIFACT_SYMLINK: ${filename}`);
    if (info.isDirectory()) {
      files.push(...await walkFiles(root, filename));
      continue;
    }
    if (!info.isFile()) throw new Error(`ARTIFACT_SPECIAL_FILE: ${filename}`);
    files.push(relative(root, filename).split(sep).join('/'));
  }
  return files;
}

async function webHash(root: string): Promise<string> {
  const files = await walkFiles(root);
  if (files.length === 0 || files.length > MAX_WEB_FILES) throw new Error('ARTIFACT_FILE_COUNT: invalid web file count');
  const pairs: string[] = [];
  for (const name of files.sort()) {
    pairs.push(`web/${name}\0${await fileSha256(join(root, name))}\n`);
  }
  return sha256(pairs.join(''));
}

async function readJson(filename: string): Promise<Record<string, any>> {
  try {
    const value: unknown = JSON.parse(await readFile(filename, 'utf8'));
    if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('not an object');
    return value as Record<string, any>;
  } catch (error) {
    throw new Error(`ARTIFACT_JSON: ${filename}: ${error instanceof Error ? error.message : String(error)}`, { cause: error });
  }
}

async function verifyDescriptor(root: string, expected: BundleExpectation, version: Record<string, any>): Promise<BundleIdentity> {
  const descriptor = await readJson(join(root, 'release.json'));
  if (descriptor.schema !== 1 || descriptor.version !== expected.version || descriptor.assets !== expected.assets) {
    throw new Error('ARTIFACT_DESCRIPTOR_IDENTITY: release descriptor does not match the expected release');
  }
  if (!SHA256.test(String(descriptor.build || ''))) throw new Error('ARTIFACT_DESCRIPTOR_BUILD: invalid build identity');
  const entry = String(descriptor.entry || '');
  if (!entry.startsWith('/') || !safeRelativePath(entry.slice(1))) throw new Error('ARTIFACT_DESCRIPTOR_ENTRY: unsafe entry path');
  const files = descriptor.files;
  if (!files || typeof files !== 'object' || Array.isArray(files)
    || Object.keys(files).sort().join(',') !== 'entry,javascript,stylesheet') {
    throw new Error('ARTIFACT_DESCRIPTOR_FILES: expected entry, javascript, and stylesheet');
  }
  const identities: Record<string, { path: string; sha256: string; integrity: string }> = {};
  for (const kind of ['entry', 'javascript', 'stylesheet']) {
    const file = files[kind];
    const path = String(file?.path || '');
    const digest = String(file?.sha256 || '');
    const integrity = String(file?.integrity || '');
    if (!safeRelativePath(path) || !SHA256.test(digest) || integrity !== integrityFor(digest)) {
      throw new Error(`ARTIFACT_DESCRIPTOR_FILE: invalid ${kind} descriptor`);
    }
    const actual = await fileSha256(join(root, path));
    if (actual !== digest) throw new Error(`ARTIFACT_DESCRIPTOR_HASH: ${kind} does not match its descriptor`);
    identities[kind] = { path, sha256: digest, integrity };
  }
  if (identities.entry.path !== entry.slice(1)) throw new Error('ARTIFACT_DESCRIPTOR_ENTRY: entry path mismatch');
  const entrySource = await readFile(join(root, identities.entry.path), 'utf8');
  if (!entrySource.includes(`src="/${identities.javascript.path}"`)
    || !entrySource.includes(`href="/${identities.stylesheet.path}"`)
    || !entrySource.includes(`integrity="${identities.javascript.integrity}"`)
    || !entrySource.includes(`integrity="${identities.stylesheet.integrity}"`)) {
    throw new Error('ARTIFACT_DESCRIPTOR_REFERENCES: entry does not reference its verified assets');
  }
  if (version.build !== descriptor.build || version.entry !== entry
    || version.script !== `/${identities.javascript.path}` || version.style !== `/${identities.stylesheet.path}`) {
    throw new Error('ARTIFACT_METADATA_MISMATCH: version.json differs from release.json');
  }
  return {
    version: expected.version,
    assets: expected.assets,
    build: descriptor.build,
    entry,
    script: `/${identities.javascript.path}`,
    style: `/${identities.stylesheet.path}`,
    scriptSha256: identities.javascript.sha256,
    styleSha256: identities.stylesheet.sha256,
    webHash: await webHash(root),
    descriptor: true,
  };
}

async function verifyLegacy(root: string, expected: BundleExpectation, version: Record<string, any>): Promise<BundleIdentity> {
  const releaseVersion = String(version.release_version || version.version || '');
  if (releaseVersion !== expected.version || Number(version.assets) !== expected.assets) {
    throw new Error('ARTIFACT_LEGACY_IDENTITY: version.json does not match the expected release');
  }
  const script = expected.script || '/assets/app.js';
  const style = expected.style || '/assets/app.css';
  for (const filename of [script, style]) {
    const path = filename.replace(/^\//, '');
    if (!safeRelativePath(path) || !(await stat(join(root, path)).catch(() => null))) {
      throw new Error(`ARTIFACT_LEGACY_ASSET: missing ${filename}`);
    }
  }
  return {
    version: expected.version,
    assets: expected.assets,
    build: '',
    entry: expected.entry || '/index.html',
    script,
    style,
    scriptSha256: await fileSha256(join(root, script.slice(1))),
    styleSha256: await fileSha256(join(root, style.slice(1))),
    webHash: await webHash(root),
    descriptor: false,
  };
}

export async function validateWebRoot(rootValue: string, expected: BundleExpectation): Promise<BundleIdentity> {
  const root = resolve(rootValue);
  const rootInfo = await lstat(root).catch(() => null);
  if (!rootInfo?.isDirectory() || rootInfo.isSymbolicLink()) throw new Error(`ARTIFACT_ROOT: ${root} is not a directory`);
  const version = await readJson(join(root, 'version.json'));
  const versionValue = String(version.release_version || version.version || '');
  if (versionValue !== expected.version || Number(version.assets) !== expected.assets) {
    throw new Error('ARTIFACT_VERSION: version.json does not match the expected release');
  }
  const identity = await (await stat(join(root, 'release.json')).catch(() => null)
    ? verifyDescriptor(root, expected, version)
    : verifyLegacy(root, expected, version));
  if (expected.webHash && identity.webHash !== expected.webHash) throw new Error('ARTIFACT_WEB_HASH: web tree hash mismatch');
  if (expected.build !== undefined && identity.build !== expected.build) throw new Error('ARTIFACT_BUILD: build identity mismatch');
  if (expected.entry && identity.entry !== expected.entry) throw new Error('ARTIFACT_ENTRY: entry identity mismatch');
  if (expected.script && identity.script !== expected.script) throw new Error('ARTIFACT_SCRIPT: script identity mismatch');
  if (expected.style && identity.style !== expected.style) throw new Error('ARTIFACT_STYLE: stylesheet identity mismatch');
  return identity;
}

function archiveEntries(archive: string): ArchiveEntry[] {
  const output = execFileSync(tarBinary(), ['-tvzf', archive], { encoding: 'utf8', maxBuffer: 8 * 1024 * 1024 });
  const entries: ArchiveEntry[] = [];
  let total = 0;
  for (const line of output.split(/\r?\n/).filter(Boolean)) {
    const type = line[0] || '';
    if (!['-', 'd'].includes(type)) throw new Error(`ARTIFACT_ARCHIVE_TYPE: ${line}`);
    const match = line.match(/^\S+\s+\S+\s+(\d+)\s+\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}\s+(.+)$/);
    if (!match) throw new Error(`ARTIFACT_ARCHIVE_LISTING: ${line}`);
    const name = match[2].replace(/^\.\//, '').replace(/\/$/, '');
    if (name && !safeRelativePath(name)) throw new Error(`ARTIFACT_ARCHIVE_PATH: ${name}`);
    const size = Number(match[1]);
    if (!Number.isSafeInteger(size) || size < 0) throw new Error(`ARTIFACT_ARCHIVE_SIZE: ${name}`);
    total += size;
    if (total > MAX_ARCHIVE_BYTES) throw new Error('ARTIFACT_ARCHIVE_SIZE: archive expands beyond the limit');
    entries.push({ name, size, type });
    if (entries.length > MAX_ARCHIVE_ENTRIES) throw new Error('ARTIFACT_ARCHIVE_ENTRIES: too many members');
  }
  return entries;
}

async function prepareDirectory(source: string, destination: string): Promise<void> {
  await rm(destination, { recursive: true, force: true });
  await mkdir(destination, { recursive: true });
  const sourceInfo = await lstat(source);
  if (sourceInfo.isSymbolicLink() || !sourceInfo.isDirectory()) throw new Error(`ARTIFACT_ROOT: ${source}`);
  await cp(source, destination, { recursive: true, dereference: false, errorOnExist: false, force: true });
  for (const name of await walkFiles(destination)) {
    const info = await lstat(join(destination, name));
    if (info.isSymbolicLink() || !info.isFile()) throw new Error(`ARTIFACT_COPY: invalid file ${name}`);
  }
}

async function prepareArchive(archive: string, destination: string, expectedHash: string): Promise<string> {
  const archiveHash = await fileSha256(archive);
  if (archiveHash !== expectedHash) throw new Error(`ARTIFACT_CHECKSUM: ${archive}`);
  archiveEntries(archive);
  await rm(destination, { recursive: true, force: true });
  await mkdir(destination, { recursive: true });
  execFileSync(tarBinary(), ['-xzf', archive, '--no-same-owner', '--no-same-permissions', '--no-overwrite-dir', '-C', destination, '--']);
  const webRoot = join(destination, 'web');
  const info = await lstat(webRoot).catch(() => null);
  if (!info?.isDirectory() || info.isSymbolicLink()) throw new Error('ARTIFACT_ARCHIVE: archive has no static web tree');
  return webRoot;
}

async function compactArchiveDestination(destination: string): Promise<void> {
  for (const entry of await readdir(destination)) {
    if (entry === 'web' || entry === 'release-manifest.json') continue;
    await rm(join(destination, entry), { recursive: true, force: true });
  }
}

export interface PrepareBundleOptions {
  allowDirectory?: boolean;
}

export async function prepareBundle(
  name: string,
  expected: BundleExpectation,
  source: string,
  destination: string,
  options: PrepareBundleOptions = {},
): Promise<PreparedBundle> {
  const sourceInfo = await lstat(source).catch(() => null);
  let root: string;
  let archiveSha256 = '';
  if (sourceInfo?.isDirectory()) {
    if (!options.allowDirectory) throw new Error(`ARTIFACT_ARCHIVE_REQUIRED: ${name} must come from a pinned release archive`);
    root = join(destination, 'web');
    await prepareDirectory(join(source, 'web'), root).catch(async () => prepareDirectory(source, root));
  } else {
    if (!expected.archiveSha256 || !SHA256.test(expected.archiveSha256)) throw new Error(`ARTIFACT_CHECKSUM: ${name} has no pinned archive hash`);
    root = await prepareArchive(source, destination, expected.archiveSha256);
    archiveSha256 = expected.archiveSha256;
    const manifestFile = join(destination, 'release-manifest.json');
    const manifestInfo = await lstat(manifestFile).catch(() => null);
    if (!manifestInfo?.isFile()) throw new Error(`ARTIFACT_MANIFEST: ${name} has no release manifest`);
    const manifest = await readJson(manifestFile);
    if (manifest.version !== expected.version
      || (expected.revision && manifest.revision !== expected.revision)
      || (expected.webHash && manifest.web_hash !== expected.webHash)) {
      throw new Error(`ARTIFACT_MANIFEST: ${name} provenance does not match the pinned manifest`);
    }
    await compactArchiveDestination(destination);
  }
  const identity = await validateWebRoot(root, expected);
  if (!archiveSha256 && expected.archiveSha256) archiveSha256 = expected.archiveSha256;
  return { name, provenance: expected, root, identity, archiveSha256 };
}

export function sameIdentity(left: BundleIdentity, right: BundleIdentity): boolean {
  return left.version === right.version
    && left.assets === right.assets
    && left.build === right.build
    && left.entry === right.entry
    && left.script === right.script
    && left.style === right.style;
}

export function assertDistinctUpgrade(baseline: PreparedBundle, candidate: PreparedBundle): void {
  if (sameIdentity(baseline.identity, candidate.identity)) {
    throw new Error(`ARTIFACT_NOOP_UPGRADE: ${baseline.name} and ${candidate.name} are byte-identical`);
  }
  if (candidate.identity.version === baseline.identity.version
    && candidate.identity.scriptSha256 === baseline.identity.scriptSha256
    && candidate.identity.styleSha256 === baseline.identity.styleSha256) {
    throw new Error('ARTIFACT_NOOP_UPGRADE: same-version pair has no asset change');
  }
}

export async function writeBundleSet(filename: string, set: BundleSet): Promise<void> {
  await writeFile(filename, `${JSON.stringify(set, null, 2)}\n`, { mode: 0o600 });
}
