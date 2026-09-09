import { lstat, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join, relative } from 'node:path';
import { prepareOutput, repositoryPath } from './support/paths';
import {
  assertDistinctUpgrade,
  fileSha256,
  prepareBundle,
  type BundleExpectation,
  type BundleSet,
  type PreparedBundle,
  writeBundleSet,
} from './support/artifacts';

interface BaselineManifest {
  schema: number;
  baselines: BundleExpectation[];
}

function option(name: string): string | undefined {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

function requiredOption(name: string): string {
  const value = option(name);
  if (!value) throw new Error(`missing ${name}`);
  return value;
}

function values(name: string): string[] {
  const result: string[] = [];
  for (let index = 0; index < process.argv.length; index += 1) {
    if (process.argv[index] === name && process.argv[index + 1]) result.push(process.argv[index + 1]);
  }
  return result;
}

async function download(url: string, filename: string): Promise<void> {
  const response = await fetch(url, { redirect: 'follow' });
  if (!response.ok) throw new Error(`baseline download returned HTTP ${response.status}`);
  const data = new Uint8Array(await response.arrayBuffer());
  if (data.byteLength > 512 * 1024 * 1024) throw new Error('baseline archive is larger than the allowed limit');
  await writeFile(filename, data, { mode: 0o600 });
}

const SHA256 = /^[a-f0-9]{64}$/u;

function expectedCandidate(version: string, assets: number, revision: string, archiveHash: string): BundleExpectation {
  return {
    name: `candidate-${version}`,
    version,
    assets,
    sourceRelease: 'release workflow artifact',
    sourceCommit: revision,
    revision,
    archiveSha256: archiveHash,
  };
}

async function main(): Promise<void> {
  const manifestPath = repositoryPath(option('--manifest') || 'tests/mobile/baselines.json');
  const manifest = JSON.parse(await readFile(manifestPath, 'utf8')) as BaselineManifest;
  if (manifest.schema !== 1 || !Array.isArray(manifest.baselines)) throw new Error('baseline manifest schema is invalid');
  const candidateSource = repositoryPath(requiredOption('--candidate'));
  const candidateVersion = requiredOption('--candidate-version');
  const candidateAssets = Number(requiredOption('--candidate-assets'));
  if (!Number.isInteger(candidateAssets) || candidateAssets < 0) throw new Error('candidate assets must be a non-negative integer');
  const candidateRevision = requiredOption('--candidate-revision');
  const candidateHash = requiredOption('--candidate-sha256');
  if (!SHA256.test(candidateHash)) throw new Error('candidate archive checksum must be a SHA-256 hash');
  const output = repositoryPath(option('--output') || 'run-artifacts');
  const sourceOverrides = new Map<string, string>();
  for (const entry of values('--baseline-source')) {
    const [name, value] = entry.split('=', 2);
    if (!name || !value) throw new Error(`invalid baseline source override: ${entry}`);
    sourceOverrides.set(name, repositoryPath(value));
  }
  const names = values('--baseline');
  const selectedNames = names.length ? names : ['0.20.8', '0.20.9'];
  const selected = selectedNames.map((name) => {
    const value = manifest.baselines.find((entry) => entry.name === name);
    if (!value) throw new Error(`baseline ${name} is not declared in ${manifestPath}`);
    if (!sourceOverrides.has(name) && (!value.url || !value.archiveSha256 || !SHA256.test(value.archiveSha256))) {
      throw new Error(`baseline ${name} has no immutable source`);
    }
    if (sourceOverrides.has(name) && (!value.archiveSha256 || !SHA256.test(value.archiveSha256))) {
      throw new Error(`baseline ${name} has no pinned archive hash`);
    }
    return value;
  });
  const allowCandidateDirectory = option('--allow-candidate-directory') === 'true';
  const candidateInfo = await lstat(candidateSource).catch(() => undefined);
  if (!candidateInfo || candidateInfo.isSymbolicLink() || (!candidateInfo.isFile() && !candidateInfo.isDirectory())) {
    throw new Error(`ARTIFACT_ROOT: ${candidateSource}`);
  }
  if (candidateInfo.isDirectory() && !allowCandidateDirectory) {
    throw new Error(`ARTIFACT_ARCHIVE_REQUIRED: candidate must come from a pinned release archive`);
  }
  const baselineSources = selected.map((expected) => sourceOverrides.get(expected.name));
  for (const source of baselineSources) {
    if (!source) continue;
    const sourceInfo = await lstat(source).catch(() => undefined);
    if (!sourceInfo || sourceInfo.isSymbolicLink() || (!sourceInfo.isFile() && !sourceInfo.isDirectory())) {
      throw new Error(`ARTIFACT_ROOT: ${source}`);
    }
  }
  await prepareOutput(output, [manifestPath, candidateSource, ...baselineSources.filter((source): source is string => Boolean(source))]);
  await mkdir(join(output, 'downloads'), { recursive: true });
  await mkdir(join(output, 'bundles'), { recursive: true });

  const baselines: PreparedBundle[] = [];
  for (const [index, expected] of selected.entries()) {
    const sourceOverride = baselineSources[index];
    const source = sourceOverride
      || join(output, 'downloads', expected.archive || `${expected.name}.tar.gz`);
    if (!sourceOverride) {
      if (!expected.url || !expected.archiveSha256) throw new Error(`baseline ${expected.name} has no immutable source`);
      await download(expected.url, source);
      const downloadedHash = await fileSha256(source);
      if (downloadedHash !== expected.archiveSha256) throw new Error(`baseline ${expected.name} checksum mismatch after download`);
    }
    baselines.push(await prepareBundle(
      expected.name,
      expected,
      source,
      join(output, 'bundles', expected.name),
    ));
  }

  const candidateExpected = expectedCandidate(candidateVersion, candidateAssets, candidateRevision, candidateHash);
  const candidate = await prepareBundle(
    candidateExpected.name,
    candidateExpected,
    candidateSource,
    join(output, 'bundles', 'candidate'),
    { allowDirectory: allowCandidateDirectory },
  );
  for (const baseline of baselines) assertDistinctUpgrade(baseline, candidate);
  await rm(join(output, 'downloads'), { recursive: true, force: true });
  const portable = (bundle: PreparedBundle): PreparedBundle => ({
    ...bundle,
    root: relative(output, bundle.root).split('\\').join('/'),
  });
  const set: BundleSet = {
    schema: 1,
    candidate: portable(candidate),
    baselines: baselines.map(portable),
    generatedAt: new Date().toISOString(),
  };
  const outputFile = join(output, 'bundle-set.json');
  await writeBundleSet(outputFile, set);
  await writeFile(join(output, 'candidate-web-root'), `${relative(output, candidate.root).split('\\').join('/')}\n`, { mode: 0o600 });
  process.stdout.write(`${outputFile}\n`);
}

main().catch((error: unknown) => {
  process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
  process.exitCode = 1;
});
