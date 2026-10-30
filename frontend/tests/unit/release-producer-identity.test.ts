import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

const finalizer = resolve(import.meta.dirname, '../../scripts/finalize-build.mjs');
const temporaryDirectories: string[] = [];
// Literal binary bytes include invalid UTF-8 and a reference that must NOT be rewritten.
const script = Buffer.concat([Buffer.from([0, 255, 128, 195, 169]), Buffer.from('./app.js\n', 'utf8')]);
const style = 'body{color:red}';
const unrelated = 'export const untouched = "./other.js";\n';

function digest(bytes: string | Uint8Array) {
  return createHash('sha256').update(bytes).digest('hex');
}

function sri(hex: string) {
  return `sha256-${Buffer.from(hex, 'hex').toString('base64')}`;
}

const scriptHash = digest(script);
const nextPath = `assets/app-${scriptHash}.js`;
const entryPath = 'builds/1.2.3-400-0123456789abcdef/index.html';

function fixture(alreadyFinal = false) {
  const root = mkdtempSync(join(tmpdir(), 'herdr-producer-identity-'));
  temporaryDirectories.push(root);
  mkdirSync(join(root, 'assets'));
  mkdirSync(dirname(join(root, entryPath)), { recursive: true });
  const oldPath = alreadyFinal ? nextPath : 'assets/app-before.js';
  const oldHash = '1'.repeat(64);
  const oldIntegrity = sri(oldHash);
  const entry = `<h1>Été 雪 🌿</h1><script src="/${oldPath}" integrity="${oldIntegrity}"></script><a href="/${oldPath}">again</a><i>${oldIntegrity}</i><link href="/assets/keep.css">\n`;
  const lazy = `import './${basename(oldPath)}';import './app.js';import './${basename(oldPath)}';import './app.js';// 雪\n`;
  const descriptor = {
    schema: 1, version: '1.2.3', assets: 400, build: 'b'.repeat(64), entry: `/${entryPath}`,
    files: {
      entry: { path: entryPath, sha256: oldHash, integrity: oldIntegrity, extra: 'entry-kept' },
      javascript: { path: oldPath, sha256: oldHash, integrity: oldIntegrity, extra: 'script-kept' },
      stylesheet: { path: 'assets/keep.css', sha256: digest(style), integrity: sri(digest(style)), extra: 'style-kept' },
    },
    extra: { keep: ['order', 7] },
  };
  const version = {
    extraFirst: 'kept', version: '1.2.3', assets: 400, script: `/${oldPath}`,
    build: descriptor.build, entry: descriptor.entry, style: '/assets/keep.css',
    script_sha256: oldHash, style_sha256: digest(style), extraLast: { keep: true },
  };
  writeFileSync(join(root, oldPath), script);
  writeFileSync(join(root, entryPath), entry);
  writeFileSync(join(root, 'assets/lazy-400.js'), lazy);
  writeFileSync(join(root, 'assets/unrelated.js'), unrelated);
  writeFileSync(join(root, 'assets/keep.css'), style);
  writeFileSync(join(root, 'release.json'), `${JSON.stringify(descriptor, null, 2)}\n`);
  writeFileSync(join(root, 'version.json'), `${JSON.stringify(version)}\n`);
  const nextIntegrity = sri(scriptHash);
  const nextEntry = `<h1>Été 雪 🌿</h1><script src="/${nextPath}" integrity="${nextIntegrity}"></script><a href="/${nextPath}">again</a><i>${nextIntegrity}</i><link href="/assets/keep.css">\n`;
  const nextLazy = `import './${basename(nextPath)}';import './${basename(nextPath)}';import './${basename(nextPath)}';import './${basename(nextPath)}';// 雪\n`;
  const nextDescriptor = {
    ...descriptor,
    files: {
      ...descriptor.files,
      entry: { ...descriptor.files.entry, sha256: digest(Buffer.from(nextEntry, 'utf8')), integrity: sri(digest(Buffer.from(nextEntry, 'utf8'))) },
      javascript: { ...descriptor.files.javascript, path: nextPath, sha256: scriptHash, integrity: nextIntegrity },
    },
  };
  const nextVersion = { ...version, script: `/${nextPath}`, script_sha256: scriptHash };
  return { root, oldPath, entry, lazy, descriptor, version, nextEntry, nextLazy, nextDescriptor, nextVersion };
}

function finalize(root: string) {
  return spawnSync('bun', [finalizer, root], { encoding: 'utf8', timeout: 30_000 });
}

function bytes(root: string) {
  const files: Record<string, Buffer> = {};
  function walk(relative = '') {
    for (const item of readdirSync(join(root, relative), { withFileTypes: true })) {
      const path = join(relative, item.name);
      if (item.isDirectory()) walk(path);
      else files[path] = readFileSync(join(root, path));
    }
  }
  walk();
  return files;
}

function expectPublished(f: ReturnType<typeof fixture>) {
  expect(readFileSync(join(f.root, nextPath))).toEqual(script);
  expect(existsSync(join(f.root, f.oldPath))).toBe(f.oldPath === nextPath);
  expect(readFileSync(join(f.root, entryPath))).toEqual(Buffer.from(f.nextEntry, 'utf8'));
  expect(readFileSync(join(f.root, 'assets/lazy-400.js'), 'utf8')).toBe(f.nextLazy);
  expect(readFileSync(join(f.root, 'assets/unrelated.js'), 'utf8')).toBe(unrelated);
  expect(readFileSync(join(f.root, 'assets/keep.css'), 'utf8')).toBe(style);
  expect(readFileSync(join(f.root, 'release.json'), 'utf8')).toBe(`${JSON.stringify(f.nextDescriptor, null, 2)}\n`);
}

afterEach(() => {
  for (const root of temporaryDirectories.splice(0)) rmSync(root, { recursive: true, force: true });
});

describe('actual finalizer asset identity', () => {
  it.each([false, true])('publishes exact binary/Unicode identities and preserves second-run bytes (already final: %s)', (alreadyFinal) => {
    const f = fixture(alreadyFinal);
    const result = finalize(f.root);
    expect(result.status, result.stderr).toBe(0);
    expectPublished(f);
    expect(readFileSync(join(f.root, 'version.json'), 'utf8')).toBe(`${JSON.stringify(f.nextVersion)}\n`);
    const first = bytes(f.root);
    expect(Object.keys(first).sort()).toEqual([
      nextPath, 'assets/lazy-400.js', 'assets/unrelated.js', 'assets/keep.css', entryPath, 'release.json', 'version.json',
    ].sort());
    const second = finalize(f.root);
    expect(second.status, second.stderr).toBe(0);
    expect(bytes(f.root)).toEqual(first);
  });

  it('fails missing entry metadata only after renaming the script and rewriting lazy references', () => {
    const f = fixture();
    const descriptor = { ...f.descriptor, files: {
      javascript: f.descriptor.files.javascript, stylesheet: f.descriptor.files.stylesheet,
    } };
    const originalDescriptor = `${JSON.stringify(descriptor, null, 2)}\n`;
    writeFileSync(join(f.root, 'release.json'), originalDescriptor);
    const result = finalize(f.root);
    expect(result.status).toBe(1);
    expect(result.stderr).toMatch(/(?:Error|error): release\.json does not describe a build entry/);
    expect(readFileSync(join(f.root, nextPath))).toEqual(script);
    expect(existsSync(join(f.root, f.oldPath))).toBe(false);
    expect(readFileSync(join(f.root, 'assets/lazy-400.js'), 'utf8')).toBe(f.nextLazy);
    expect(readFileSync(join(f.root, entryPath), 'utf8')).toBe(f.entry);
    expect(readFileSync(join(f.root, 'release.json'), 'utf8')).toBe(originalDescriptor);
    expect(readFileSync(join(f.root, 'version.json'), 'utf8')).toBe(`${JSON.stringify(f.version)}\n`);
    expect(readFileSync(join(f.root, 'assets/unrelated.js'), 'utf8')).toBe(unrelated);
    expect(readFileSync(join(f.root, 'assets/keep.css'), 'utf8')).toBe(style);
  });

  it('fails missing version only after entry and descriptor publication', () => {
    const f = fixture();
    rmSync(join(f.root, 'version.json'));
    const result = finalize(f.root);
    expect(result.status).toBe(1);
    expect(result.stderr).toContain('ENOENT: no such file or directory, open');
    expect(result.stderr).toContain(join(f.root, 'version.json'));
    expectPublished(f);
    expect(existsSync(join(f.root, 'version.json'))).toBe(false);
  });
});
