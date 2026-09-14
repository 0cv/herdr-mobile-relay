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
  }
  await command('bun', ['run', 'build'], 300_000, { cwd: frontendTarget });
  await cp(join(frontendTarget, 'dist'), destination, { recursive: true });
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
    const baselineMetadata = JSON.parse(await readFile(join(baselineRoot, 'version.json'), 'utf8')) as { version: string; assets: number };
    const revision = option('--revision') || 'synthetic-current-code';
    const expectation = (name: string): BundleExpectation => ({
      name,
      version: baselineMetadata.version,
      assets: baselineMetadata.assets,
      sourceRelease: 'synthetic current-code build',
      sourceCommit: revision,
    });
    const baseline: PreparedBundle = {
      name: 'current-code-baseline',
      provenance: expectation('current-code-baseline'),
      root: baselineRoot,
      identity: await validateWebRoot(baselineRoot, expectation('current-code-baseline')),
      archiveSha256: '',
    };
    const candidate: PreparedBundle = {
      name: 'current-code-target',
      provenance: expectation('current-code-target'),
      root: candidateRoot,
      identity: await validateWebRoot(candidateRoot, expectation('current-code-target')),
      archiveSha256: '',
    };
    assertDistinctUpgrade(baseline, candidate);
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
