import { spawnSync } from 'node:child_process';
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import versions from '../../build-versions.json';
import { validateReleaseFixture } from './release-validation-fixture';

const bumpScript = resolve(import.meta.dirname, '../../scripts/bump-assets.mjs');
const temporaryDirectories: string[] = [];

function temporaryDirectory() {
  const root = mkdtempSync(join(tmpdir(), 'herdr-asset-withdrawal-'));
  temporaryDirectories.push(root);
  return root;
}

function bump(assets: number, withdrawnAssets = versions.withdrawnAssets) {
  const root = temporaryDirectory();
  mkdirSync(join(root, 'scripts'));
  cpSync(bumpScript, join(root, 'scripts/bump-assets.mjs'));
  const versionsFile = join(root, 'build-versions.json');
  const input = { ...versions, assets, withdrawnAssets };
  writeFileSync(versionsFile, JSON.stringify(input));
  const result = spawnSync('bun', [join(root, 'scripts/bump-assets.mjs')], { encoding: 'utf8', timeout: 30_000 });
  expect(result.status, result.stderr).toBe(0);
  return { input, output: JSON.parse(readFileSync(versionsFile, 'utf8')) as typeof input };
}

function validate(mutate?: (root: string) => void) {
  return validateReleaseFixture(temporaryDirectory(), mutate);
}

afterEach(() => {
  for (const root of temporaryDirectories.splice(0)) rmSync(root, { recursive: true, force: true });
});

describe('withdrawn release assets', () => {
  it.each([[395, 397], [397, 398]])('bumps generation %i to %i', (current, next) => {
    const { input, output } = bump(current);
    expect(output).toEqual({ ...input, assets: next });
  });

  it('skips every consecutive withdrawn generation', () => {
    const { input, output } = bump(395, [399, 397, 396]);
    expect(output).toEqual({ ...input, assets: 398 });
  });

  it('rejects withdrawn generation 396 explicitly', () => {
    const result = validate((root) => {
      const file = join(root, 'release.json');
      const descriptor = JSON.parse(readFileSync(file, 'utf8'));
      descriptor.assets = 396;
      writeFileSync(file, JSON.stringify(descriptor));
    });
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('Release uses withdrawn asset generation: 396');
  });

  it('rejects the withdrawn 0.22.5 application script explicitly', () => {
    expect(versions.withdrawnScripts).toContain('assets/app-9617cb64a45fb9bbb8aff3c0577e2ecdb3bdc2865bcd5de5e07e8f051155ead1.js');
    const result = validate((root) => {
      const file = join(root, 'release.json');
      const descriptor = JSON.parse(readFileSync(file, 'utf8'));
      descriptor.files.javascript.path = versions.withdrawnScripts[0];
      writeFileSync(file, JSON.stringify(descriptor));
    });
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain(`Release uses withdrawn application script: ${versions.withdrawnScripts[0]}`);
  });

  it('accepts the current web bundle without changing it', () => {
    const result = validate();
    expect(result.status, result.stderr).toBe(0);
    expect(result.stdout).toContain('Validated release structure');
  });
});
