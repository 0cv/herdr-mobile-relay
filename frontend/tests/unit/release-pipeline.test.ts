// @vitest-environment node
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { brotliDecompressSync } from 'node:zlib';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

const frontend = resolve(import.meta.dirname, '../..');
const committedRelease = JSON.parse(readFileSync(join(frontend, '../web/release.json'), 'utf8'));
const stableCompressed = [
  'index.html', 'herdr-bootstrap.js', 'notification-icons.js', 'sw.js', 'manifest-loader.js',
  'manifest.webmanifest', 'setup.webmanifest', 'version.json', 'release.json', 'icons/icon.svg',
];

let root = '';

function identity(bytes: Uint8Array) {
  const hash = createHash('sha256').update(bytes).digest();
  return { sha256: hash.toString('hex'), integrity: `sha256-${hash.toString('base64')}` };
}

function run(command: string, args: string[], env: NodeJS.ProcessEnv) {
  const result = spawnSync(command, args, { cwd: frontend, env, encoding: 'utf8', timeout: 120_000 });
  expect(result.status, `${command} ${args.join(' ')}\n${result.stdout}\n${result.stderr}`).toBe(0);
}

function listFiles(directory: string, prefix = ''): string[] {
  return readdirSync(join(directory, prefix), { withFileTypes: true }).flatMap((item) => {
    const path = join(prefix, item.name);
    return item.isDirectory() ? listFiles(directory, path) : [path];
  });
}

const read = (path: string) => readFileSync(join(root, path));
const readJson = (path: string) => JSON.parse(read(path).toString('utf8'));

beforeAll(() => {
  root = mkdtempSync(join(tmpdir(), 'herdr-release-pipeline-'));
  const env: NodeJS.ProcessEnv = { ...process.env, NODE_ENV: 'production' };
  delete env.HERDR_DEV_RUNTIME;
  run(join(frontend, 'node_modules/.bin/vite'), ['build', '--outDir', root, '--emptyOutDir'], env);
  run('bun', ['scripts/finalize-build.mjs', root], env);
}, 180_000);

afterAll(() => {
  if (root) rmSync(root, { recursive: true, force: true });
});

describe('finished release directory', () => {
  it('names the script and stylesheet after the SHA-256 of their bytes', () => {
    const assets = readdirSync(join(root, 'assets'));
    const scripts = assets.filter((name) => /^app.*\.js$/.test(name));
    const styles = assets.filter((name) => /^app.*\.css$/.test(name));
    expect(scripts).toEqual([`app-${identity(read(`assets/${scripts[0]}`)).sha256}.js`]);
    expect(styles).toEqual([`app-${identity(read(`assets/${styles[0]}`)).sha256}.css`]);
  });

  it('describes every file with the digests of the bytes on disk', () => {
    const descriptor = readJson('release.json');
    expect(Object.keys(descriptor)).toEqual(Object.keys(committedRelease));
    expect(Object.keys(descriptor.files)).toEqual(Object.keys(committedRelease.files));
    for (const [name, file] of Object.entries<Record<string, string>>(descriptor.files)) {
      expect(Object.keys(file), name).toEqual(Object.keys(committedRelease.files[name]));
      const plain = identity(read(file.path));
      const encoded = identity(read(`${file.path}.br`));
      expect(file.sha256, name).toBe(plain.sha256);
      expect(file.integrity, name).toBe(plain.integrity);
      expect(file.brotli_sha256, name).toBe(encoded.sha256);
      expect(file.brotli_integrity, name).toBe(encoded.integrity);
    }
    expect(descriptor.files.javascript.path).toBe(`assets/app-${descriptor.files.javascript.sha256}.js`);
    expect(descriptor.files.stylesheet.path).toBe(`assets/app-${descriptor.files.stylesheet.sha256}.css`);
    expect(descriptor.entry).toBe(`/${descriptor.files.entry.path}`);
  });

  it('keeps version.json in agreement with release.json', () => {
    const descriptor = readJson('release.json');
    const version = readJson('version.json');
    expect(version).toEqual({
      version: descriptor.version,
      assets: descriptor.assets,
      build: descriptor.build,
      entry: descriptor.entry,
      script: `/${descriptor.files.javascript.path}`,
      style: `/${descriptor.files.stylesheet.path}`,
      script_sha256: descriptor.files.javascript.sha256,
      style_sha256: descriptor.files.stylesheet.sha256,
    });
    expect(Object.keys(version)).toEqual([
      'version', 'assets', 'build', 'entry', 'script', 'style', 'script_sha256', 'style_sha256',
    ]);
  });

  it('references the final script and stylesheet from the entry with matching integrity', () => {
    const descriptor = readJson('release.json');
    const entry = read(descriptor.files.entry.path).toString('utf8');
    for (const [tag, file] of [['script', descriptor.files.javascript], ['link', descriptor.files.stylesheet]]) {
      const reference = entry.match(new RegExp(`<${tag}[^>]*/${file.path.replaceAll('.', '\\.')}[^>]*>`))?.[0];
      expect(reference, file.path).toBeDefined();
      expect(reference).toContain(`integrity="${file.integrity}"`);
    }
    expect(entry).not.toContain('assets/app.js');
    expect(entry).not.toContain('assets/app.css');
  });

  it('makes every lazy chunk import the final script and nothing import ./app.js', () => {
    const descriptor = readJson('release.json');
    const scriptName = descriptor.files.javascript.path.replace('assets/', '');
    const lazy = readdirSync(join(root, 'assets')).filter((name) => name.endsWith(`-${descriptor.assets}.js`));
    expect(lazy.length).toBeGreaterThan(0);
    for (const name of lazy) expect(read(`assets/${name}`).toString('utf8'), name).toContain(`./${scriptName}`);
    for (const name of readdirSync(join(root, 'assets')).filter((item) => item.endsWith('.js'))) {
      expect(read(`assets/${name}`).toString('utf8'), name).not.toContain('./app.js');
    }
  });

  it('writes a Brotli sidecar that decompresses to each listed file', () => {
    const descriptor = readJson('release.json');
    const assets = readdirSync(join(root, 'assets'));
    const expected = [
      ...stableCompressed,
      descriptor.files.entry.path,
      descriptor.files.javascript.path,
      descriptor.files.stylesheet.path,
      ...assets.filter((name) => name.endsWith(`-${descriptor.assets}.js`)).map((name) => `assets/${name}`),
    ];
    const sidecars = listFiles(root).filter((path) => path.endsWith('.br')).map((path) => path.slice(0, -3));
    expect(sidecars.sort()).toEqual([...new Set(expected)].sort());
    for (const path of sidecars) {
      expect(brotliDecompressSync(read(`${path}.br`)).equals(read(path)), path).toBe(true);
    }
    expect(brotliDecompressSync(read('release.json.br')).equals(read('release.json'))).toBe(true);
  });

  it('is a production build', () => {
    const descriptor = readJson('release.json');
    const script = read(descriptor.files.javascript.path).toString('utf8');
    expect(script).not.toContain('ownership_invalid_binding');
    expect(script).not.toContain('__HERDR_BUILD_ID__');
    expect(script).toContain(descriptor.build);
  });
});

describe('build without a build identity marker', () => {
  it('fails and publishes nothing', () => {
    const unmarked = mkdtempSync(join(tmpdir(), 'herdr-release-unmarked-'));
    try {
      mkdirSync(join(unmarked, 'assets'));
      writeFileSync(join(unmarked, 'assets/app.js'), 'const color="--pf";\n');
      writeFileSync(join(unmarked, 'assets/app.css'), '.leaf{color:var(--pf)}\n');
      writeFileSync(join(unmarked, 'index.html'), '<link href="assets/app.css"><script src="assets/app.js"></script>\n');
      const result = spawnSync('bun', ['scripts/finalize-build.mjs', unmarked], { cwd: frontend, encoding: 'utf8', timeout: 60_000 });
      expect(result.status).not.toBe(0);
      expect(result.stderr).toContain('Application bundle is missing its build identity marker');
      expect(existsSync(join(unmarked, 'release.json'))).toBe(false);
      expect(existsSync(join(unmarked, 'builds'))).toBe(false);
      expect(readdirSync(join(unmarked, 'assets')).sort()).toEqual(['app.css', 'app.js']);
    } finally {
      rmSync(unmarked, { recursive: true, force: true });
    }
  });
});
