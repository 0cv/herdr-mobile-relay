import { createHash } from 'node:crypto';
import { readdir, readFile, rename, writeFile } from 'node:fs/promises';
import { basename, join, resolve } from 'node:path';

const root = resolve(process.argv[2] || 'dist');
const descriptorPath = join(root, 'release.json');
const descriptor = JSON.parse(await readFile(descriptorPath, 'utf8'));
const javascript = descriptor?.files?.javascript;
if (!javascript || typeof javascript.path !== 'string' || typeof javascript.sha256 !== 'string') {
  throw new Error('release.json does not describe an application script');
}

// Use the existing release runtime to compact both eagerly shipped scripts.
// All imports stay external: no rebundling, new chunks, or changes to the eager
// graph. The application digest is computed only after this transformation.
async function compactScript(path, format) {
  const result = await globalThis.Bun.build({
    entrypoints: [path],
    target: 'browser',
    format,
    minify: true,
    external: ['*'],
  });
  if (!result.success || result.outputs.length !== 1) {
    throw new Error(`Could not compact release script ${path}: ${result.logs.join('\n')}`);
  }
  return Buffer.from(await result.outputs[0].text());
}
const loaderPath = join(root, 'manifest-loader.js');
await writeFile(loaderPath, await compactScript(loaderPath, 'iife'));
const sourcePath = join(root, javascript.path);
const source = await compactScript(sourcePath, 'esm');
await writeFile(sourcePath, source);
const sha256 = createHash('sha256').update(source).digest('hex');
const integrity = `sha256-${Buffer.from(sha256, 'hex').toString('base64')}`;
const nextPath = `assets/app-${sha256}.js`;
const oldPath = javascript.path;
if (oldPath !== nextPath) await rename(sourcePath, join(root, nextPath));
const oldScriptName = basename(oldPath);
const nextScriptName = basename(nextPath);
for (const name of await readdir(join(root, 'assets'))) {
  if (!name.endsWith('.js') || name === nextScriptName || name === oldScriptName) continue;
  const lazyPath = join(root, 'assets', name);
  const lazySource = await readFile(lazyPath, 'utf8');
  const rewritten = lazySource.replaceAll(`./${oldScriptName}`, `./${nextScriptName}`).replaceAll('./app.js', `./${nextScriptName}`);
  if (rewritten !== lazySource) await writeFile(lazyPath, rewritten);
}

const entryPath = descriptor?.files?.entry?.path;
if (typeof entryPath !== 'string') throw new Error('release.json does not describe a build entry');
const entryFile = join(root, entryPath);
const entry = await readFile(entryFile, 'utf8');
const nextEntry = entry
  .replaceAll(`/${oldPath}`, `/${nextPath}`)
  .replaceAll(javascript.integrity, integrity);
await writeFile(entryFile, nextEntry);
const entrySha256 = createHash('sha256').update(nextEntry).digest('hex');
const entryIntegrity = `sha256-${Buffer.from(entrySha256, 'hex').toString('base64')}`;

javascript.path = nextPath;
javascript.sha256 = sha256;
javascript.integrity = integrity;
descriptor.files.entry.sha256 = entrySha256;
descriptor.files.entry.integrity = entryIntegrity;
await writeFile(descriptorPath, `${JSON.stringify(descriptor, null, 2)}\n`);

const versionPath = join(root, 'version.json');
const version = JSON.parse(await readFile(versionPath, 'utf8'));
version.script = `/${nextPath}`;
version.script_sha256 = sha256;
await writeFile(versionPath, `${JSON.stringify(version)}\n`);
