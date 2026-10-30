import { mkdtempSync, readFileSync, rmSync, unlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { brotliCompressSync } from 'node:zlib';
import { afterEach, describe, expect, it } from 'vitest';
import { validateReleaseFixture } from './release-validation-fixture';

const temporaryDirectories: string[] = [];

function validate(mutate?: (root: string) => void) {
  const root = mkdtempSync(join(tmpdir(), 'herdr-release-validation-'));
  temporaryDirectories.push(root);
  return validateReleaseFixture(root, mutate);
}

afterEach(() => {
  for (const root of temporaryDirectories.splice(0)) rmSync(root, { recursive: true, force: true });
});

// Recompress mutated JSON independently so validation reaches the intended identity branch.
function writeJson(root: string, name: string, value: unknown) {
  const source = Buffer.from(`${JSON.stringify(value)}\n`);
  writeFileSync(join(root, name), source);
  writeFileSync(join(root, `${name}.br`), brotliCompressSync(source));
}

describe('finalized release validation', () => {
  it('accepts the normally generated and compressed release', () => {
    const result = validate();
    expect(result.status, result.stderr).toBe(0);
    expect(result.stdout).toContain('Validated release structure');
  });

  it('rejects a wrong decoded digest even when its SRI is consistent', () => {
    let path = '';
    const result = validate((root) => {
      const descriptor = JSON.parse(readFileSync(join(root, 'release.json'), 'utf8'));
      path = descriptor.files.entry.path;
      descriptor.files.entry.sha256 = '0'.repeat(64);
      descriptor.files.entry.integrity = `sha256-${Buffer.alloc(32).toString('base64')}`;
      writeJson(root, 'release.json', descriptor);
    });
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain(`entry digest does not match ${path}`);
  });

  it('rejects wrong SRI independently of the decoded digest', () => {
    const result = validate((root) => {
      const descriptor = JSON.parse(readFileSync(join(root, 'release.json'), 'utf8'));
      descriptor.files.entry.integrity = `sha256-${Buffer.alloc(32).toString('base64')}`;
      writeJson(root, 'release.json', descriptor);
    });
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('release.json has an invalid entry descriptor');
  });

  it('rejects a wrong encoded digest even when its Brotli SRI is consistent', () => {
    let path = '';
    const result = validate((root) => {
      const descriptor = JSON.parse(readFileSync(join(root, 'release.json'), 'utf8'));
      path = descriptor.files.entry.path;
      descriptor.files.entry.brotli_sha256 = '0'.repeat(64);
      descriptor.files.entry.brotli_integrity = `sha256-${Buffer.alloc(32).toString('base64')}`;
      writeJson(root, 'release.json', descriptor);
    });
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain(`entry Brotli digest does not match ${path}.br`);
  });

  it('rejects mismatched version projection after consistent JSON recompression', () => {
    const result = validate((root) => {
      const version = JSON.parse(readFileSync(join(root, 'version.json'), 'utf8'));
      version.script_sha256 = '0'.repeat(64);
      writeJson(root, 'version.json', version);
    });
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('version.json differs from release.json, herdr-plugin.toml, or build-versions.json');
  });

  it('requires a top-level 404 to disable implicit Pages SPA fallback', () => {
    const result = validate((root) => unlinkSync(join(root, '404.html')));
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('Required release file is missing: 404.html');
  });

  it.each([
    '<script src="/herdr-bootstrap.js"></script>',
    '<meta http-equiv="refresh" content="0;url=/">',
    '<a href="/" onclick="location.replace(\'/\')">Open</a>',
  ])('rejects executable or automatic error-page navigation', (extra) => {
    const result = validate((root) => writeFileSync(join(root, '404.html'), `<h1>Not found</h1><a href="/">Home</a>${extra}`));
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('404.html must be a static error page');
  });

  it('rejects an overlapping immutable rule even with the correct revalidation rules present', () => {
    const result = validate((root) => {
      const file = join(root, '_headers');
      writeFileSync(file, `${readFileSync(file, 'utf8')}\n/*\n  Cache-Control: public, max-age=31536000, immutable\n`);
    });
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('without overlapping rules');
  });

  it('rejects extra SPA rewrites after finalization', () => {
    const result = validate((root) => {
      const file = join(root, '_redirects');
      writeFileSync(file, `${readFileSync(file, 'utf8')}\n/* /index.html 200\n`);
    });
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('only stable bootstrap paths');
  });
});
