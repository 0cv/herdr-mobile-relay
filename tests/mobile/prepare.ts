import { mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { join, relative } from 'node:path';
import { repositoryPath } from './support/paths';
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
  const output = repositoryPath(option('--output') || 'run-artifacts');
  await rm(output, { recursive: true, force: true });
  const names = values('--baseline');
  const selectedNames = names.length ? names : ['0.20.8', '0.20.9'];
  const selected = selectedNames.map((name) => {
    const value = manifest.baselines.find((entry) => entry.name === name);
    if (!value) throw new Error(`baseline ${name} is not declared in ${manifestPath}`);
    return value;
  });
  await mkdir(join(output, 'downloads'), { recursive: true });
  await mkdir(join(output, 'bundles'), { recursive: true });

  const baselines: PreparedBundle[] = [];
  for (const expected of selected) {
    const sourceOverride = values('--baseline-source')
      .map((entry) => entry.split('=', 2))
      .find(([name]) => name === expected.name)?.[1];
    const source = sourceOverride
      ? repositoryPath(sourceOverride)
      : join(output, 'downloads', expected.archive || `${expected.name}.tar.gz`);
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
    { allowDirectory: option('--allow-candidate-directory') === 'true' },
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
