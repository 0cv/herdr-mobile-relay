// @vitest-environment node
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { URL } from 'node:url';
import type { Plugin } from 'vite';
import { describe, expect, it } from 'vitest';
import versions from '../../build-versions.json' with { type: 'json' };
import config, { assetContentVersion } from '../../vite.config.ts';

type Chunk = { type: 'chunk'; fileName: string; code: string };
type Asset = { type: 'asset'; fileName: string; source: string | Uint8Array };
type Bundle = Record<string, Chunk | Asset>;

function fixture(code: string, css: string, html: string) {
  const javascript: Chunk = { type: 'chunk', fileName: 'assets/app.js', code };
  const stylesheet: Asset = { type: 'asset', fileName: 'assets/app.css', source: css };
  const entry: Asset = { type: 'asset', fileName: 'index.html', source: html };
  const bundle: Bundle = { 'assets/app.js': javascript, 'assets/app.css': stylesheet, 'index.html': entry };
  const emitted: Asset[] = [];
  const errors: string[] = [];
  return { javascript, stylesheet, entry, bundle, emitted, errors };
}

function publish(f: ReturnType<typeof fixture>) {
  const plugin = config.plugins?.find((item): item is Plugin => item !== null
    && typeof item === 'object' && !Array.isArray(item)
    && 'name' in item && item.name === 'stable-release-assets');
  if (!plugin) throw new Error('Missing actual stable-release-assets plugin');
  const hook = plugin.generateBundle;
  if (!hook) throw new Error('Missing actual generateBundle hook');
  const handler = typeof hook === 'function' ? hook : hook.handler;
  // Only the two used Rollup context callbacks are adapted; this is not a bundler fixture.
  return Reflect.apply(handler, {
    error(message: string): never {
      f.errors.push(message);
      throw new Error(message);
    },
    emitFile(asset: Asset) {
      f.emitted.push(asset);
      return String(f.emitted.length);
    },
  }, [{}, f.bundle, false]);
}

// Independent Node crypto oracle, never the production digest or assetIdentity helper.
function identity(source: string | Uint8Array) {
  const hash = createHash('sha256').update(source).digest();
  return { sha256: hash.toString('hex'), integrity: `sha256-${hash.toString('base64')}` };
}

describe('actual Vite release identity', () => {
  it('publishes canonical and final identities with exact descriptor and version projections', () => {
    const manifest = readFileSync(new URL('../../../herdr-plugin.toml', import.meta.url), 'utf8');
    const productVersion = manifest.match(/^version = "([0-9]+\.[0-9]+\.[0-9]+)"$/m)?.[1];
    if (!productVersion) throw new Error('Missing current product version fixture input');
    const originalJavascript = 'const builds=["__HERDR_BUILD_ID__","__HERDR_BUILD_ID__"];const snow="雪🌿";const color="--primary-foreground";\n';
    const canonicalJavascript = 'const builds=["__HERDR_BUILD_ID__","__HERDR_BUILD_ID__"];const snow="雪🌿";const color="--pf";\n';
    const originalStylesheet = ':root{--primary-foreground:red}.leaf{color:var(--primary-foreground)}\n';
    const finalStylesheet = ':root{--pf:red}.leaf{color:var(--pf)}\n';
    // Canonical bytes are hashed before either marker is replaced, with one NUL separator.
    const canonical = `${canonicalJavascript}\u0000${finalStylesheet}`;
    const build = createHash('sha256').update(canonical, 'utf8').digest('hex');
    const finalJavascript = `const builds=["${build}","${build}"];const snow="雪🌿";const color="--pf";\n`;
    const javascriptIdentity = identity(finalJavascript);
    const stylesheetIdentity = identity(finalStylesheet);
    const javascriptPath = `assets/app-${javascriptIdentity.sha256}.js`;
    const stylesheetPath = `assets/app-${stylesheetIdentity.sha256}.css`;
    const entryPath = `builds/${productVersion}-${versions.assets}-${build.slice(0, 16)}/index.html`;
    const originalHtml = '<!doctype html>\n<html lang="en">\n  <head>\n    <link rel="icon" href="icons/icon.svg" />\n    <link rel="stylesheet" href="/assets/app.css" />\n  </head>\n  <body>\n    <script src="manifest-loader.js"></script>\n    <script type="module" src="/assets/app.js"></script>\n  </body>\n</html>\n';
    const entrySource = `<!doctype html><html lang=en><head><link rel=icon href="/icons/icon.svg"><link rel=stylesheet href="/${stylesheetPath}" integrity="${stylesheetIdentity.integrity}" crossorigin="anonymous"></head><body><script src="/manifest-loader.js"></script><script type=module src="/${javascriptPath}" integrity="${javascriptIdentity.integrity}" crossorigin="anonymous"></script></body></html>\n`;
    const entryIdentity = identity(entrySource);
    const descriptor = {
      schema: 1,
      version: productVersion,
      assets: versions.assets,
      build,
      entry: `/${entryPath}`,
      files: {
        entry: { path: entryPath, sha256: entryIdentity.sha256, integrity: entryIdentity.integrity },
        javascript: { path: javascriptPath, sha256: javascriptIdentity.sha256, integrity: javascriptIdentity.integrity },
        stylesheet: { path: stylesheetPath, sha256: stylesheetIdentity.sha256, integrity: stylesheetIdentity.integrity },
      },
    };
    const version = {
      version: productVersion,
      assets: versions.assets,
      build,
      entry: `/${entryPath}`,
      script: `/${javascriptPath}`,
      style: `/${stylesheetPath}`,
      script_sha256: javascriptIdentity.sha256,
      style_sha256: stylesheetIdentity.sha256,
    };
    const bootstrap = '<!doctype html><script src="/herdr-bootstrap.js"></script>\n';
    const expected: Record<string, string> = {
      [entryPath]: entrySource,
      'herdr-bootstrap.js': `const e = new URL(window.__HERDR_ENTRY__ || "/${entryPath}", location);\n  e.search = location.search;\n  e.hash = location.hash;\n  location.replace(e);\n`,
      'release.json': `${JSON.stringify(descriptor, null, 2)}\n`,
      'version.json': `${JSON.stringify(version)}\n`,
      '_redirects': `/ /${entryPath} 302\n/index.html /${entryPath} 302\n`,
      '_headers': '/herdr-bootstrap.js\n  Cache-Control: no-cache, no-store\n\n/manifest-loader.js\n  Cache-Control: no-cache, no-store\n\n/manifest.webmanifest\n  Cache-Control: no-cache, no-store\n\n/setup.webmanifest\n  Cache-Control: no-cache, no-store\n\n/sw.js\n  Cache-Control: no-cache, no-store\n\n/version.json\n  Cache-Control: no-cache, no-store\n\n/release.json\n  Cache-Control: no-cache, no-store\n\n/\n  Cache-Control: no-cache, no-store\n\n/index.html\n  Cache-Control: no-cache, no-store\n\n/404.html\n  Cache-Control: no-store\n\n/404\n  Cache-Control: no-store\n\n/builds/*\n  Cache-Control: no-cache\n\n/assets/*\n  Cache-Control: no-cache\n',
    };
    const f = fixture(originalJavascript, originalStylesheet, originalHtml);
    publish(f);
    expect(f.errors).toEqual([]);
    expect(f.bundle).toEqual({
      'assets/app.js': { type: 'chunk', fileName: javascriptPath, code: finalJavascript },
      'assets/app.css': { type: 'asset', fileName: stylesheetPath, source: finalStylesheet },
      'index.html': { type: 'asset', fileName: 'index.html', source: bootstrap },
    });
    expect(f.emitted.map(asset => asset.fileName)).toEqual([
      entryPath, 'herdr-bootstrap.js', 'release.json', 'version.json', '_redirects', '_headers',
    ]);
    const actual: Record<string, string | Uint8Array> = {};
    for (const asset of f.emitted) {
      expect(asset.type).toBe('asset');
      expect(typeof asset.source).toBe('string');
      actual[asset.fileName] = asset.source;
    }
    expect(actual).toEqual(expected);
    expect(JSON.parse(String(actual['release.json']))).toEqual(descriptor);
    expect(JSON.parse(String(actual['version.json']))).toEqual(version);
    expect(Object.keys(descriptor)).toEqual(['schema', 'version', 'assets', 'build', 'entry', 'files']);
    expect(Object.keys(descriptor.files)).toEqual(['entry', 'javascript', 'stylesheet']);
    for (const file of Object.values(descriptor.files)) {
      expect(Object.keys(file)).toEqual(['path', 'sha256', 'integrity']);
      expect(file.sha256).toMatch(/^[a-f0-9]{64}$/);
      expect(file.integrity).toMatch(/^sha256-[A-Za-z0-9+/]{43}=$/);
    }
    expect(Object.keys(version)).toEqual([
      'version', 'assets', 'build', 'entry', 'script', 'style', 'script_sha256', 'style_sha256',
    ]);
    expect(canonicalJavascript.match(/__HERDR_BUILD_ID__/g)).toHaveLength(2);
    expect(f.javascript.code).not.toContain('__HERDR_BUILD_ID__');
    expect(f.javascript.code.match(new RegExp(build, 'g'))).toHaveLength(2);
    expect(build).not.toBe(javascriptIdentity.sha256);
    expect(entryIdentity.sha256).not.toBe(javascriptIdentity.sha256);
    expect(stylesheetIdentity.sha256).not.toBe(javascriptIdentity.sha256);
    expect(assetContentVersion(canonical)).toBe(build.slice(0, 16));
    expect(assetContentVersion(finalJavascript)).toBe(javascriptIdentity.sha256.slice(0, 16));
    expect(assetContentVersion(finalStylesheet)).toBe(stylesheetIdentity.sha256.slice(0, 16));
    expect(assetContentVersion(entrySource)).toBe(entryIdentity.sha256.slice(0, 16));
  });

  it('derives content versions from exact string and binary view bytes', () => {
    const backing = Uint8Array.from([0x7a, 0, 0xff, 0x80, 0xc3, 0x28, 0xfe, 0x61, 0x0a, 0x7b]);
    const view = new Uint8Array(backing.buffer, 2, 5);
    const vectors: { source: string | Uint8Array; bytes: Uint8Array }[] = [
      { source: '', bytes: Buffer.alloc(0) },
      { source: 'é雪🌿\u0000e\u0301\n', bytes: Buffer.from('é雪🌿\u0000e\u0301\n', 'utf8') },
      { source: Uint8Array.from([0, 255, 128, 195, 40, 254, 97, 10]), bytes: Buffer.from([0, 255, 128, 195, 40, 254, 97, 10]) },
      { source: new Uint8Array(0), bytes: Buffer.alloc(0) },
      { source: view, bytes: Buffer.from([0xff, 0x80, 0xc3, 0x28, 0xfe]) },
      { source: new Uint8Array(backing.buffer, 3, 0), bytes: Buffer.alloc(0) },
      { source: new Uint8Array(backing.buffer, 7, 2), bytes: Buffer.from([0x61, 0x0a]) },
      { source: Buffer.from(backing.buffer, 1, 3), bytes: Buffer.from([0, 0xff, 0x80]) },
    ];
    for (const vector of vectors) {
      const expected = createHash('sha256').update(vector.bytes).digest('hex').slice(0, 16);
      const actual = assetContentVersion(vector.source);
      expect(actual).toBe(expected);
      expect(actual).toMatch(/^[a-f0-9]{16}$/);
    }
    const before = assetContentVersion(view);
    expect(before).not.toBe(createHash('sha256').update(backing).digest('hex').slice(0, 16));
    expect(before).not.toBe(createHash('sha256').update(Buffer.from(view).toString('utf8')).digest('hex').slice(0, 16));
    backing[0] = 0x62;
    expect(assetContentVersion(view)).toBe(before);
    backing[3] = 0x62;
    const changed = createHash('sha256').update(Buffer.from([0xff, 0x62, 0xc3, 0x28, 0xfe])).digest('hex').slice(0, 16);
    expect(assetContentVersion(view)).toBe(changed);
    expect(changed).not.toBe(before);
  });

  it('rejects a missing build marker after compaction before publication', () => {
    const originalJavascript = 'const color="--primary-foreground";const snow="雪";\n';
    const compactedJavascript = 'const color="--pf";const snow="雪";\n';
    const originalStylesheet = '.leaf{color:var(--primary-foreground)}\n';
    const compactedStylesheet = '.leaf{color:var(--pf)}\n';
    const originalHtml = '<link href="assets/app.css"><script src="assets/app.js"></script>\n';
    const f = fixture(originalJavascript, originalStylesheet, originalHtml);
    expect(() => publish(f)).toThrowError(new Error('Application bundle is missing its build identity marker'));
    expect(f.errors).toEqual(['Application bundle is missing its build identity marker']);
    expect(f.emitted).toEqual([]);
    expect(f.bundle).toEqual({
      'assets/app.js': { type: 'chunk', fileName: 'assets/app.js', code: compactedJavascript },
      'assets/app.css': { type: 'asset', fileName: 'assets/app.css', source: compactedStylesheet },
      'index.html': { type: 'asset', fileName: 'index.html', source: originalHtml },
    });
  });
});
