import { createHash, randomBytes } from 'node:crypto';
import { mkdtempSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { brotliCompressSync } from 'node:zlib';
import { afterEach, describe, expect, it } from 'vitest';

const directories: string[] = [];
const hash = (source: string | Buffer) => createHash('sha256').update(source).digest('hex');
const integrity = (digest: string) => `sha256-${Buffer.from(digest, 'hex').toString('base64')}`;

function fixture() {
  const root = mkdtempSync(join(tmpdir(), 'herdr-initial-payload-'));
  directories.push(root);
  const app = 'export const lazy = () => import("./TerminalView-408.js"); console.log("app");\n';
  const appHash = hash(app);
  const script = `assets/app-${appHash}.js`;
  const style = `assets/app-${'b'.repeat(64)}.css`;
  const entry = 'builds/0.22.4-408-fixture/index.html';
  const files: Record<string, string | Buffer> = {
    'index.html': '<!doctype html><script src="/herdr-bootstrap.js"></script>\n',
    'herdr-bootstrap.js': 'location.replace("/builds/0.22.4-408-fixture/index.html");\n',
    [entry]: `<script src="/manifest-loader.js"></script><script type=module src="/${script}" integrity="${integrity(appHash)}"></script><link rel=stylesheet href="/${style}">`,
    'manifest-loader.js': '/* readable source remains in public/ */\n(() => {\n  console.log("loader");\n})();\n',
    'manifest.webmanifest': '{"start_url":"./","name":"Herdr"}\n',
    'setup.webmanifest': '{"name":"Herdr"}\n',
    'version.json': '{"version":"0.22.4","script":"' + `/${script}` + '"}\n',
    [script]: app,
    [style]: 'body { color: white; }\n',
    'assets/TerminalView-408.js': `import { lazy } from "./app-${appHash}.js"; console.log(lazy);\n`,
    'release.json': JSON.stringify({ files: {
      entry: { path: entry, sha256: hash('entry'), integrity: integrity(hash('entry')) },
      javascript: { path: script, sha256: appHash, integrity: integrity(appHash) },
    } }),
  };
  const write = (path: string, source: string | Buffer) => {
    mkdirSync(resolve(root, path, '..'), { recursive: true });
    writeFileSync(join(root, path), source);
    writeFileSync(join(root, `${path}.br`), brotliCompressSync(source));
    files[path] = source;
  };
  for (const [path, source] of Object.entries(files)) write(path, source);
  const run = (name = 'check-size.mjs') => spawnSync('bun', [resolve(import.meta.dirname, '../../scripts', name), root], {
    encoding: 'utf8', timeout: 30_000,
  });
  return { root, files, entry, script, style, write, run };
}

afterEach(() => {
  for (const directory of directories.splice(0)) rmSync(directory, { recursive: true, force: true });
});

describe('complete eager shell payload', () => {
  it('charges the parser-blocking loader, startup version probe and worst single manifest branch', () => {
    const f = fixture();
    const result = f.run();
    expect(result.status, result.stderr).toBe(0);
    const charged = ['index.html', 'herdr-bootstrap.js', f.entry, 'manifest-loader.js', 'manifest.webmanifest', 'version.json', f.script, f.style];
    const rows = result.stdout.split('\n').filter((line) => / raw /.test(line) && !line.startsWith('TOTAL'));
    expect(rows.map((line) => line.split(/\s+raw /)[0].trim())).toEqual(charged);
    // Recount the measuring runtime's gzip values, not Node-vs-Bun compression.
    const total = rows.reduce((sum, line) => sum + Number(line.match(/gzip\s+(\d+) B/)?.[1]), 0);
    expect(result.stdout).toMatch(new RegExp(`TOTAL.*gzip\\s+${total} B / 169216 B`));
  });

  it('charges setup metadata instead when its gzip representation is larger', () => {
    const f = fixture();
    f.write('setup.webmanifest', JSON.stringify({ name: randomBytes(2048).toString('hex') }));
    const result = f.run();
    expect(result.status, result.stderr).toBe(0);
    expect(result.stdout).toContain('; charging setup.webmanifest');
    expect(result.stdout).toMatch(/setup\.webmanifest\s+raw /);
    expect(result.stdout).not.toMatch(/manifest\.webmanifest\s+raw /);
  });

  it('rejects an oversized loader with the existing 169216-byte ceiling', () => {
    const f = fixture();
    f.write('manifest-loader.js', randomBytes(180_000));
    const result = f.run();
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('Initial payload exceeds the 165 KiB gzip ceiling');
    expect(result.stdout).toContain('/ 169216 B');
  });

  it.each([
    '<script src="/unaccounted.js"></script>',
    '<script src=/unaccounted.js></script>',
    '<script>fetch("/unaccounted.js")</script>',
    '<link rel="stylesheet" href="/unaccounted.css">',
    '<link rel=modulepreload href=/unaccounted.js>',
    '<link rel=preload as=font href=/unaccounted.woff2>',
  ])('fails closed on a newly introduced shell dependency: %s', (addition) => {
    const f = fixture();
    f.write(f.entry, String(f.files[f.entry]) + addition);
    const result = f.run();
    expect(result.status).not.toBe(0);
    expect(result.stderr).toContain('Unaccounted shell');
  });

  it('compacts both scripts without adding chunks or changing external lazy imports, and rebinds the final digest', () => {
    const f = fixture();
    const result = f.run('finalize-build.mjs');
    expect(result.status, result.stderr).toBe(0);
    const descriptor = JSON.parse(readFileSync(join(f.root, 'release.json'), 'utf8'));
    const script = descriptor.files.javascript;
    const source = readFileSync(join(f.root, script.path));
    expect(script.path).toBe(`assets/app-${hash(source)}.js`);
    expect(script.sha256).toBe(hash(source));
    expect(script.integrity).toBe(integrity(hash(source)));
    expect(source.toString()).toContain('./TerminalView-408.js');
    expect(readdirSync(join(f.root, 'assets')).filter((name) => name.endsWith('.js')).sort())
      .toEqual(['TerminalView-408.js', script.path.slice('assets/'.length)].sort());
    const entrySource = readFileSync(join(f.root, f.entry));
    expect(entrySource.toString()).toContain(`/${script.path}`);
    expect(entrySource.toString()).toContain(script.integrity);
    expect(descriptor.files.entry.sha256).toBe(hash(entrySource));
    expect(readFileSync(join(f.root, 'assets/TerminalView-408.js'), 'utf8')).toContain(`./${script.path.slice('assets/'.length)}`);
    const loader = readFileSync(join(f.root, 'manifest-loader.js'), 'utf8');
    expect(loader).toContain('loader');
    expect(loader.length).toBeLessThan(String(f.files['manifest-loader.js']).length);
    expect(loader).not.toMatch(/\b(?:import|export)\b/);
  });
});
