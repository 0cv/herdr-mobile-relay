import { spawnSync } from 'node:child_process';
import { cpSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import versions from '../../build-versions.json';

const repository = resolve(import.meta.dirname, '../../..');

export function validateReleaseFixture(root: string, mutate?: (bundle: string) => void) {
  const bundle = join(root, 'web');
  cpSync(join(repository, 'web'), bundle, { recursive: true });
  const identity = JSON.parse(readFileSync(join(bundle, 'release.json'), 'utf8')) as { assets: number; version: string };
  const frontend = join(root, 'frontend');
  const scripts = join(frontend, 'scripts');
  mkdirSync(scripts, { recursive: true });
  for (const name of ['validate-build.mjs', 'compressed-assets.mjs', 'pages-policy.ts']) {
    cpSync(join(repository, 'frontend/scripts', name), join(scripts, name));
  }
  writeFileSync(join(frontend, 'build-versions.json'), JSON.stringify({ ...versions, assets: identity.assets }));
  const manifest = readFileSync(join(repository, 'herdr-plugin.toml'), 'utf8');
  writeFileSync(join(root, 'herdr-plugin.toml'), manifest.replace(/^version = "[0-9]+\.[0-9]+\.[0-9]+"$/m, `version = "${identity.version}"`));
  mutate?.(bundle);
  return spawnSync('bun', [join(scripts, 'validate-build.mjs'), bundle], { encoding: 'utf8', timeout: 30_000 });
}
