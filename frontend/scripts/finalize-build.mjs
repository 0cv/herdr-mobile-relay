import { assetIdentity } from './asset-identity.mjs';
import { mkdir, readdir, readFile, rename, rm, writeFile } from 'node:fs/promises';
import { basename, dirname, join, resolve } from 'node:path';
import { brotliCompressSync, constants } from 'node:zlib';
import versions from '../build-versions.json' with { type: 'json' };
import { releaseCompressedAssets } from './compressed-assets.mjs';
import { productVersionFromManifest } from './release-rules.ts';

const BUILD_ID_MARKER = '__HERDR_BUILD_ID__';

function compress(source) {
  return brotliCompressSync(source, {
    params: {
      [constants.BROTLI_PARAM_MODE]: constants.BROTLI_MODE_TEXT,
      [constants.BROTLI_PARAM_QUALITY]: 11,
      [constants.BROTLI_PARAM_SIZE_HINT]: source.length,
    },
  });
}

function fileIdentity(path, source) {
  return { path, ...assetIdentity(source) };
}

function withIntegrity(html, tag, attribute, file) {
  const pattern = new RegExp(`(<${tag}[^>]*${attribute}=["']/?${file.path.replaceAll('/', '\\/')}["'][^>]*)>`);
  return html.replace(
    pattern,
    (match, prefix) => `${prefix} integrity="${file.integrity}"${/\bcrossorigin(?:[=\s]|$)/i.test(prefix) ? '' : ' crossorigin="anonymous"'}>`,
  );
}

function entryHtml(viteHtml, script, stylesheet) {
  const html = viteHtml
    .replaceAll('assets/app.js', script.path)
    .replaceAll('assets/app.css', stylesheet.path)
    .replaceAll('src="manifest-loader.js"', 'src="/manifest-loader.js"')
    .replaceAll('href="icons/', 'href="/icons/')
    .replace(/>\s+</g, '><')
    .replace(/\s{2,}/g, ' ')
    .replace(/\s+\/>/g, '/>')
    .replace(/(\s)((?!(?:src|href|integrity)=")[A-Za-z_:][\w:.-]*)="([A-Za-z0-9_./:#?-]+)"/g, '$1$2=$3')
    .replace(/\/>/g, '>');
  return withIntegrity(withIntegrity(html, 'script', 'src', script), 'link', 'href', stylesheet);
}

function bootstrapScript(entryPath) {
  return `const e = new URL(window.__HERDR_ENTRY__ || ${JSON.stringify(`/${entryPath}`)}, location);\n  e.search = location.search;\n  e.hash = location.hash;\n  location.replace(e);\n`;
}

const root = resolve(process.argv[2] || 'dist');
const productVersion = productVersionFromManifest(
  await readFile(new URL('../../herdr-plugin.toml', import.meta.url), 'utf8'),
);
const viteScript = await readFile(join(root, 'assets/app.js'), 'utf8');
const stylesheetSource = await readFile(join(root, 'assets/app.css'), 'utf8');
const viteHtml = await readFile(join(root, 'index.html'), 'utf8');
if (!viteScript.includes(BUILD_ID_MARKER)) {
  throw new Error('Application bundle is missing its build identity marker');
}

const build = assetIdentity(`${viteScript}\u0000${stylesheetSource}`).sha256;
const scriptSource = viteScript.replaceAll(BUILD_ID_MARKER, build);
const script = fileIdentity(`assets/app-${assetIdentity(scriptSource).sha256}.js`, scriptSource);
const stylesheet = fileIdentity(`assets/app-${assetIdentity(stylesheetSource).sha256}.css`, stylesheetSource);
await writeFile(join(root, script.path), scriptSource);
await rm(join(root, 'assets/app.js'));
await rename(join(root, 'assets/app.css'), join(root, stylesheet.path));

for (const name of await readdir(join(root, 'assets'))) {
  if (!name.endsWith('.js') || name === basename(script.path)) continue;
  const lazyPath = join(root, 'assets', name);
  const lazySource = await readFile(lazyPath, 'utf8');
  const rewritten = lazySource.replaceAll('./app.js', `./${basename(script.path)}`);
  if (rewritten !== lazySource) await writeFile(lazyPath, rewritten);
}

const entryPath = `builds/${productVersion}-${versions.assets}-${build.slice(0, 16)}/index.html`;
const entrySource = entryHtml(viteHtml, script, stylesheet);
const entry = fileIdentity(entryPath, entrySource);
await mkdir(dirname(join(root, entryPath)), { recursive: true });
await writeFile(join(root, entryPath), entrySource);
await writeFile(join(root, 'index.html'), '<!doctype html><script src="/herdr-bootstrap.js"></script>\n');
await writeFile(join(root, 'herdr-bootstrap.js'), bootstrapScript(entryPath));
await writeFile(join(root, '_redirects'), `/ /${entryPath} 302\n/index.html /${entryPath} 302\n`);

const files = { entry, javascript: script, stylesheet };
const descriptor = {
  schema: 1,
  version: productVersion,
  assets: versions.assets,
  build,
  entry: `/${entryPath}`,
  files,
};
await writeFile(join(root, 'version.json'), `${JSON.stringify({
  version: descriptor.version,
  assets: descriptor.assets,
  build,
  entry: descriptor.entry,
  script: `/${script.path}`,
  style: `/${stylesheet.path}`,
  script_sha256: script.sha256,
  style_sha256: stylesheet.sha256,
})}\n`);

const compressedAssets = await releaseCompressedAssets(root, entryPath);
const compressed = new Map();
for (const relative of compressedAssets) {
  if (relative === 'release.json') continue;
  const output = compress(await readFile(join(root, relative)));
  compressed.set(relative, output);
  await writeFile(join(root, `${relative}.br`), output);
}

// Record the encoded representation alongside the decoded asset identity. The
// deployment verifier can therefore check a CDN's Brotli response without
// trusting a Content-Encoding header or silently accepting a corrupt sidecar.
for (const file of Object.values(files)) {
  const output = compressed.get(file.path);
  if (!output) throw new Error(`No Brotli representation was created for ${file.path}`);
  const identity = assetIdentity(output);
  file.brotli_sha256 = identity.sha256;
  file.brotli_integrity = identity.integrity;
}
const serializedDescriptor = `${JSON.stringify(descriptor, null, 2)}\n`;
await writeFile(join(root, 'release.json'), serializedDescriptor);
await writeFile(join(root, 'release.json.br'), compress(Buffer.from(serializedDescriptor)));

console.log(`Created ${compressedAssets.length} Brotli assets in ${root}`);
