import { cp, mkdir, mkdtemp, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { join, relative, sep } from 'node:path';
import { assertDistinctUpgrade, validateWebRoot, writeBundleSet, type BundleExpectation, type BundleSet, type PreparedBundle } from './support/artifacts';
import { command } from './support/process';
import { prepareOutput, repositoryPath, repositoryRoot } from './support/paths';

function option(name: string): string | undefined {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

function required(name: string): string {
  const value = option(name);
  if (!value) throw new Error(`missing ${name}`);
  return value;
}

type BuildMetadata = { version: string; assets: number };

async function buildVariant(sourceRoot: string, destination: string, variant: string): Promise<void> {
  const frontendSource = join(repositoryRoot, 'frontend');
  const frontendTarget = join(sourceRoot, 'frontend');
  await mkdir(sourceRoot, { recursive: true });
  await cp(frontendSource, frontendTarget, {
    recursive: true,
    filter: (source) => !source.split(sep).includes('node_modules') && !source.split(sep).includes('dist'),
  });
  const modules = join(frontendSource, 'node_modules');
  if (existsSync(modules)) await symlink(modules, join(frontendTarget, 'node_modules'), 'dir');
  else await command('bun', ['install', '--frozen-lockfile'], 300_000, { cwd: frontendTarget });
  await cp(join(repositoryRoot, 'herdr-plugin.toml'), join(sourceRoot, 'herdr-plugin.toml'));
  const appFile = join(frontendTarget, 'src', 'App.svelte');
  const appSource = await readFile(appFile, 'utf8');
  const marker = '<div class="app-shell">';
  if (!appSource.includes(marker)) throw new Error('CURRENT_BUILD: application root marker was not found');
  await writeFile(appFile, appSource.replace(marker, `<div class="app-shell" data-mobile-ci-variant="${variant}">`));
  if (variant === 'candidate') {
    const stylesheetFile = join(frontendTarget, 'src', 'app.css');
    const stylesheet = await readFile(stylesheetFile, 'utf8');
    await writeFile(stylesheetFile, `${stylesheet}\n.app-shell[data-mobile-ci-variant="candidate"] { --mobile-ci-current-code: 1; }\n`);
    const versionsFile = join(frontendTarget, 'build-versions.json');
    const versions = JSON.parse(await readFile(versionsFile, 'utf8')) as { assets?: unknown; [key: string]: unknown };
    if (!Number.isSafeInteger(versions.assets) || Number(versions.assets) < 0) {
      throw new Error('CURRENT_BUILD: build asset version must be a non-negative safe integer');
    }
    await writeFile(versionsFile, `${JSON.stringify({ ...versions, assets: Number(versions.assets) + 1 }, null, 2)}\n`);
  }
  await command('bun', ['run', 'build'], 300_000, { cwd: frontendTarget });
  await cp(join(frontendTarget, 'dist'), destination, { recursive: true });
}

const lazyAssetPattern = /import\(\s*[`'"]\.\/([A-Za-z0-9_.-]+-[0-9]+\.js)[`'"]\s*\)/g;

async function lazyAssets(bundle: PreparedBundle): Promise<Set<string>> {
  const source = await readFile(join(bundle.root, bundle.identity.script.slice(1)), 'utf8');
  return new Set([...source.matchAll(lazyAssetPattern)].map(match => `/assets/${match[1]}`));
}

async function main(): Promise<void> {
  const output = await prepareOutput(repositoryPath(required('--output')), [join(repositoryRoot, 'frontend'), join(repositoryRoot, 'herdr-plugin.toml')]);
  await mkdir(join(output, 'bundles'), { recursive: true, mode: 0o700 });
  const temporary = await mkdtemp(join('/tmp', 'herdr-mobile-current-'));
  try {
    const baselineRoot = join(output, 'bundles', 'current-code-baseline');
    const candidateRoot = join(output, 'bundles', 'current-code-target');
    await buildVariant(join(temporary, 'baseline'), baselineRoot, 'baseline');
    await buildVariant(join(temporary, 'candidate'), candidateRoot, 'candidate');
    const baselineMetadata = JSON.parse(await readFile(join(baselineRoot, 'version.json'), 'utf8')) as BuildMetadata;
    const candidateMetadata = JSON.parse(await readFile(join(candidateRoot, 'version.json'), 'utf8')) as BuildMetadata;
    const revision = option('--revision') || 'synthetic-current-code';
    const expectation = (name: string, metadata: BuildMetadata): BundleExpectation => ({
      name,
      version: metadata.version,
      assets: metadata.assets,
      sourceRelease: 'synthetic current-code build',
      sourceCommit: revision,
    });
    const baselineExpectation = expectation('current-code-baseline', baselineMetadata);
    const candidateExpectation = expectation('current-code-target', candidateMetadata);
    const baseline: PreparedBundle = {
      name: 'current-code-baseline',
      provenance: baselineExpectation,
      root: baselineRoot,
      identity: await validateWebRoot(baselineRoot, baselineExpectation),
      archiveSha256: '',
    };
    const candidate: PreparedBundle = {
      name: 'current-code-target',
      provenance: candidateExpectation,
      root: candidateRoot,
      identity: await validateWebRoot(candidateRoot, candidateExpectation),
      archiveSha256: '',
    };
    assertDistinctUpgrade(baseline, candidate);
    const baselineLazyAssets = await lazyAssets(baseline);
    const candidateLazyAssets = await lazyAssets(candidate);
    if (!baselineLazyAssets.size || !candidateLazyAssets.size
      || [...baselineLazyAssets].some(asset => candidateLazyAssets.has(asset))) {
      throw new Error('CURRENT_BUILD: synthetic variants must expose distinct lazy asset URLs');
    }
    if (baseline.identity.style === candidate.identity.style || baseline.identity.styleSha256 === candidate.identity.styleSha256) {
      throw new Error('CURRENT_BUILD: baseline and candidate stylesheets must have distinct immutable identities');
    }
    const portable = (bundle: PreparedBundle): PreparedBundle => ({
      ...bundle,
      root: relative(output, bundle.root).split(sep).join('/'),
    });
    const set: BundleSet = {
      schema: 1,
      candidate: portable(candidate),
      baselines: [portable(baseline)],
      generatedAt: new Date().toISOString(),
    };
    await writeBundleSet(join(output, 'bundle-set.json'), set);
    process.stdout.write(`${join(output, 'bundle-set.json')}\n`);
  } finally {
    await rm(temporary, { recursive: true, force: true });
  }
}

main().catch((error: unknown) => {
  process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
  process.exitCode = 1;
});
