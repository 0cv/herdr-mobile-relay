import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { brotliCompressSync, brotliDecompressSync, constants } from 'node:zlib';
import { afterEach, describe, expect, it } from 'vitest';

const compressor = resolve(import.meta.dirname, '../../scripts/compress-assets.mjs');
const temporaryDirectories: string[] = [];
const binary = Buffer.from([0, 255, 128, 195, 40, 254, 97, 10]);
const unicode = Buffer.from('é雪🌿 e\u0301\n', 'utf8');

function identity(bytes: Uint8Array) {
  const hash = createHash('sha256').update(bytes).digest();
  return { sha256: hash.toString('hex'), integrity: `sha256-${hash.toString('base64')}` };
}

// Independent Node byte oracles, not the production identity/discovery helpers.
function brotli(bytes: Uint8Array) {
  return brotliCompressSync(bytes, { params: {
    [constants.BROTLI_PARAM_MODE]: constants.BROTLI_MODE_TEXT,
    [constants.BROTLI_PARAM_QUALITY]: 11,
    [constants.BROTLI_PARAM_SIZE_HINT]: bytes.length,
  } });
}

function encodedIdentity(bytes: Uint8Array) {
  const expected = identity(brotli(bytes));
  return { brotli_sha256: expected.sha256, brotli_integrity: expected.integrity };
}

function inventory(root: string) {
  const files: Record<string, Buffer> = {};
  function walk(relative = '') {
    for (const item of readdirSync(join(root, relative), { withFileTypes: true })) {
      expect(item.isSymbolicLink()).toBe(false);
      const path = join(relative, item.name);
      if (item.isDirectory()) walk(path);
      else {
        expect(item.isFile()).toBe(true);
        files[path] = readFileSync(join(root, path));
      }
    }
  }
  walk();
  return files;
}

function fixture() {
  const root = mkdtempSync(join(tmpdir(), 'herdr-compressor-identity-'));
  temporaryDirectories.push(root);
  const entryPath = 'builds/1.2.3-400-0123456789abcdef/index.html';
  const javascriptPath = `assets/app-${identity(binary).sha256}.js`;
  const stylesheetPath = `assets/app-${identity(unicode).sha256}.css`;
  const lazyPath = 'assets/lazy_part.foo-007.js';
  const entry = Buffer.from('<h1>Été 雪 🌿</h1>\n', 'utf8');
  const lazy = Buffer.from('export const snow = "雪";\n', 'utf8');
  const empty = Buffer.alloc(0);
  const descriptor = {
    extraFirst: { unknown_extension: ['kept', 7] },
    schema: 1, version: '1.2.3', assets: 400, build: 'b'.repeat(64), entry: `/${entryPath}`,
    files: {
      entry: {
        extraFirst: 'entry-kept', brotli_integrity: 'stale-entry-sri', path: entryPath,
        ...identity(entry), unknown_extension: { keep: true }, brotli_sha256: 'stale-entry-digest',
      },
      javascript: { path: `/${javascriptPath}`, ...identity(binary), extra: ['binary', 0] },
      stylesheet: { ...identity(unicode), path: stylesheetPath, extra: 'unicode-kept' },
      custom: { path: `/${lazyPath}`, ...identity(lazy), arbitrary: { extra: 'lazy-kept' } },
      empty: { path: 'notification-icons.js', ...identity(empty), extra: null },
      unicode: { path: 'manifest-loader.js', ...identity(unicode), extra: 'UTF-8' },
    },
    extraLast: ['preserve', { order: true }],
  };
  const raw: Record<string, Buffer> = {
    'index.html': Buffer.from('<!doctype html>\n'),
    'herdr-bootstrap.js': Buffer.from('location.replace("/builds/example/index.html");\n'),
    'notification-icons.js': empty,
    'sw.js': Buffer.from('self.addEventListener("fetch", () => {});\n'),
    'manifest-loader.js': unicode,
    'manifest.webmanifest': Buffer.from('{"name":"雪"}\n', 'utf8'),
    'setup.webmanifest': Buffer.from('{"name":"setup"}\n'),
    'version.json': Buffer.from(' { "extra": "unchanged", "version": "1.2.3", "assets": 400 }\r\n'),
    'release.json': Buffer.from(` \n${JSON.stringify(descriptor)}\r\n`),
    'icons/icon.svg': Buffer.from('<svg xmlns="http://www.w3.org/2000/svg"><text>🌿</text></svg>\n'),
    [entryPath]: entry,
    [javascriptPath]: binary,
    [stylesheetPath]: unicode,
    [lazyPath]: lazy,
    'assets/ignored.js': Buffer.from('not a discovered release representation\n'),
    'assets/worker-aBC_.js': binary,
    [`assets/app-${'A'.repeat(64)}.js`]: unicode,
    'assets/lazy-42.css': unicode,
    'icons/ignored.png': binary,
    'notes.txt': unicode,
    'notes.txt.br': Buffer.from('unrelated preexisting sidecar'),
    'index.html.br': Buffer.from('stale selected sidecar'),
    'release.json.br': Buffer.from('descriptor sidecar must survive an interrupted publication'),
  };
  for (const [path, bytes] of Object.entries(raw)) {
    mkdirSync(dirname(join(root, path)), { recursive: true });
    writeFileSync(join(root, path), bytes);
  }
  const compressedPaths = [
    'index.html', 'herdr-bootstrap.js', 'notification-icons.js', 'sw.js', 'manifest-loader.js',
    'manifest.webmanifest', 'setup.webmanifest', 'version.json', 'release.json', 'icons/icon.svg',
    entryPath, javascriptPath, stylesheetPath, lazyPath,
  ];
  const expectedDescriptor = { ...descriptor, files: {
    entry: { ...descriptor.files.entry, ...encodedIdentity(entry) },
    javascript: { ...descriptor.files.javascript, ...encodedIdentity(binary) },
    stylesheet: { ...descriptor.files.stylesheet, ...encodedIdentity(unicode) },
    custom: { ...descriptor.files.custom, ...encodedIdentity(lazy) },
    empty: { ...descriptor.files.empty, ...encodedIdentity(empty) },
    unicode: { ...descriptor.files.unicode, ...encodedIdentity(unicode) },
  } };
  const serializedDescriptor = Buffer.from(`${JSON.stringify(expectedDescriptor, null, 2)}\n`);
  const expected: Record<string, Buffer> = { ...raw, 'release.json': serializedDescriptor };
  for (const path of compressedPaths) expected[`${path}.br`] = brotli(expected[path]);
  return { root, raw, descriptor, expectedDescriptor, serializedDescriptor, compressedPaths, expected, entryPath, javascriptPath, stylesheetPath, lazyPath };
}

function compress(root: string) {
  return spawnSync('bun', [compressor, root], { encoding: 'utf8', timeout: 30_000 });
}

function expectSuccess(result: ReturnType<typeof compress>, f: ReturnType<typeof fixture>) {
  expect(result.error).toBeUndefined();
  expect(result.signal).toBeNull();
  expect(result.status, result.stderr).toBe(0);
  expect(result.stderr).toBe('');
  expect(result.stdout).toBe(`Created ${f.compressedPaths.length} Brotli assets in ${f.root}\n`);
}

afterEach(() => {
  for (const root of temporaryDirectories.splice(0)) rmSync(root, { recursive: true, force: true });
});

describe('actual compressor asset identity', () => {
  it('publishes encoded binary Unicode and empty identities with the complete exact byte inventory', () => {
    const f = fixture();
    const result = compress(f.root);
    expectSuccess(result, f);
    const actual = inventory(f.root);
    expect(Object.keys(actual).sort()).toEqual(Object.keys(f.expected).sort());
    expect(actual).toEqual(f.expected);
    for (const path of f.compressedPaths) {
      expect(actual[`${path}.br`]).toEqual(brotli(actual[path]));
      expect(brotliDecompressSync(actual[`${path}.br`])).toEqual(actual[path]);
    }
    const published = JSON.parse(actual['release.json'].toString('utf8')) as typeof f.expectedDescriptor;
    expect(published).toEqual(f.expectedDescriptor);
    expect(Object.keys(published)).toEqual(Object.keys(f.descriptor));
    expect(Object.keys(published.files)).toEqual(Object.keys(f.descriptor.files));
    expect(Object.keys(published.files.entry)).toEqual([
      'extraFirst', 'brotli_integrity', 'path', 'sha256', 'integrity', 'unknown_extension', 'brotli_sha256',
    ]);
    expect(Object.keys(published.files.javascript)).toEqual([
      'path', 'sha256', 'integrity', 'extra', 'brotli_sha256', 'brotli_integrity',
    ]);
    for (const file of Object.values(published.files)) {
      const path = file.path.replace(/^\//, '');
      const encoded = identity(actual[`${path}.br`]);
      expect(file.brotli_sha256).toBe(encoded.sha256);
      expect(file.brotli_integrity).toBe(encoded.integrity);
      expect(file.brotli_integrity).toMatch(/^sha256-[A-Za-z0-9+/]{43}=$/);
      expect(file.brotli_sha256).not.toBe(file.sha256);
      expect(file.brotli_integrity).not.toBe(file.integrity);
      expect({ sha256: file.sha256, integrity: file.integrity }).toEqual(identity(f.raw[path]));
    }
    expect(identity(binary)).not.toEqual(identity(Buffer.from(binary.toString('utf8'), 'utf8')));
    expect(actual['release.json']).toEqual(f.serializedDescriptor);
    expect(brotliDecompressSync(actual['release.json.br'])).toEqual(f.serializedDescriptor);
    for (const [path, bytes] of Object.entries(f.raw)) {
      if (path !== 'release.json' && !path.endsWith('.br')) expect(actual[path]).toEqual(bytes);
    }
  });

  it('keeps every output byte and encoded field order identical on a second execution', () => {
    const f = fixture();
    expectSuccess(compress(f.root), f);
    const first = inventory(f.root);
    expect(first).toEqual(f.expected);
    expectSuccess(compress(f.root), f);
    expect(inventory(f.root)).toEqual(first);
    expect(readFileSync(join(f.root, 'release.json'))).toEqual(f.serializedDescriptor);
  });

  it('fails a missing representation after all discovered sidecars but before descriptor publication', () => {
    const f = fixture();
    // The entry is processed first in memory, then this existing but undiscovered file fails.
    const descriptor = { ...f.descriptor, files: {
      ...f.descriptor.files,
      missing: { path: '/assets/ignored.js', ...identity(f.raw['assets/ignored.js']), extra: 'kept' },
    } };
    const original = Buffer.from(`${JSON.stringify(descriptor)}\r\n`);
    writeFileSync(join(f.root, 'release.json'), original);
    const before = inventory(f.root);
    const result = compress(f.root);
    expect(result.error).toBeUndefined();
    expect(result.signal).toBeNull();
    expect(result.status).toBe(1);
    expect(result.stdout).toBe('');
    expect(result.stderr).toMatch(/(?:Error|error): No Brotli representation was created for assets\/ignored\.js(?:\r?\n|$)/);
    const errors = result.stderr.match(/^(?:Error|error): .+$/gm);
    expect(errors).toEqual(['error: No Brotli representation was created for assets/ignored.js']);
    const partial = { ...before };
    for (const path of f.compressedPaths) {
      if (path !== 'release.json') partial[`${path}.br`] = brotli(before[path]);
    }
    const actual = inventory(f.root);
    expect(Object.keys(actual).sort()).toEqual(Object.keys(partial).sort());
    expect(actual).toEqual(partial);
    expect(actual['release.json']).toEqual(original);
    expect(actual['release.json.br']).toEqual(before['release.json.br']);
    expect(actual['index.html.br']).not.toEqual(before['index.html.br']);
    expect(actual['version.json']).toEqual(f.raw['version.json']);
    expect(actual['notes.txt.br']).toEqual(before['notes.txt.br']);
  });
});
