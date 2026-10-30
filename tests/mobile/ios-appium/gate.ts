import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { mkdir, readFile, readdir, realpath, writeFile } from 'node:fs/promises';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import ts from 'typescript';

type Transform = Record<string, Array<[string, string]>>;
type Package = { name: string; version: string; root: string; entry: string; files: Record<string, string> };
type Manifest = {
  schema: 1; contract: string; compiler: string; inputs: Record<string, string>;
  packages: Record<string, Package>; files: Record<string, { before: string; after: string }>;
};
const artifact = (name: string) => new URL(name, import.meta.url);
const load = async <T>(name: string): Promise<T> => JSON.parse(await readFile(artifact(name), 'utf8')) as T;
export const hash = (bytes: string | Buffer): string => createHash('sha256').update(bytes).digest('hex');
const contract = 'herdr-installed-contexts-v1';

export async function configure(home: string): Promise<void> {
  await mkdir(home, { recursive: true });
  assert.deepEqual(await readdir(home), [], 'Fresh iOS Appium home required');
  await writeFile(join(home, 'package.json'), await readFile(artifact('package.json')));
}

/** The only compiler operation: exact reviewed substitutions and JS emission, no dependency rebuild. */
export async function producerOutput(home: string, packages: Record<string, Package>, transform: Transform): Promise<Map<string, string>> {
  const output = new Map<string, string>();
  for (const [key, edits] of Object.entries(transform)) {
    const slash = key.indexOf('/');
    const id = key.slice(0, slash), file = key.slice(slash + 1);
    assert.ok(packages[id] && !file.includes('..') && !file.startsWith('/'));
    let text = await readFile(join(home, packages[id].root, file), 'utf8');
    for (const [before, after] of edits) {
      assert.equal(text.split(before).length, 2, `Nonunique iOS producer substitution: ${key}`);
      text = text.replace(before, after);
    }
    output.set(key, text);
    if (file.startsWith('lib/') && file.endsWith('.ts')) {
      const result = ts.transpileModule(text, { fileName: file, compilerOptions: {
        target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext, sourceMap: true, inlineSources: true,
      } });
      assert.ok(result.sourceMapText);
      const runtime = file.replace(/^lib\//u, 'build/lib/').replace(/\.ts$/u, '.js');
      const map = JSON.parse(result.sourceMapText);
      map.sources = [relative(dirname(runtime), file)];
      output.set(`${id}/${runtime}`, result.outputText);
      output.set(`${id}/${runtime}.map`, JSON.stringify(map));
    }
  }
  return output;
}

async function attestation(home: string, manifest: Manifest) {
  const inputs: Record<string, string> = {};
  for (const file of Object.keys(manifest.inputs)) inputs[file] = hash(await readFile(artifact(file)));
  const artifacts: Record<string, string> = {};
  for (const pkg of Object.values(manifest.packages)) {
    for (const file of Object.keys(pkg.files)) artifacts[join(pkg.root, file)] = hash(await readFile(join(home, pkg.root, file)));
  }
  artifacts['package.json'] = hash(await readFile(join(home, 'package.json')));
  return { manifestSha256: hash(await readFile(artifact('producer-manifest.json'))), inputs, artifacts };
}

async function installation(home: string, patched: boolean): Promise<Record<string, unknown>> {
  assert.ok(process.execArgv.includes('--experimental-import-meta-resolve'), 'Explicit ESM importer resolution required');
  assert.equal(process.versions.node, '26.13.0', 'Pinned Node required');
  assert.ok(!process.env.NODE_PATH && !process.env.NODE_OPTIONS, 'Ambient Node resolver overrides forbidden');
  const root = await realpath(home);
  assert.equal(resolve(home), root, 'Canonical nonsymlinked iOS Appium home required');
  const manifest = await load<Manifest>('producer-manifest.json');
  assert.equal(manifest.schema, 1); assert.equal(manifest.contract, contract);
  assert.equal(ts.version, manifest.compiler);
  for (const [file, digest] of Object.entries(manifest.inputs)) {
    assert.equal(hash(await readFile(artifact(file))), digest, `Changed iOS producer input: ${file}`);
  }
  assert.equal(hash(await readFile(join(root, 'package.json'))), hash(await readFile(artifact('package.json'))));
  const entries: Record<string, string> = {};
  for (const [id, pkg] of Object.entries(manifest.packages)) {
    const pkgRoot = join(root, pkg.root);
    assert.equal(await realpath(pkgRoot), pkgRoot, `Symlinked iOS package: ${id}`);
    const metadata = JSON.parse(await readFile(join(pkgRoot, 'package.json'), 'utf8'));
    assert.equal(metadata.name, pkg.name); assert.equal(metadata.version, pkg.version, `Wrong iOS package version: ${id}`);
    entries[id] = pathToFileURL(join(pkgRoot, pkg.entry)).href;
    for (const [file, original] of Object.entries(pkg.files)) {
      const path = join(pkgRoot, file);
      assert.equal(await realpath(path), path, `Symlinked iOS producer: ${id}/${file}`);
      const change = manifest.files[`${id}/${file}`];
      if (change) assert.equal(change.before, original);
      assert.equal(hash(await readFile(path)), patched && change ? change.after : original, `Changed iOS producer: ${id}/${file}`);
    }
  }
  for (const [parent, specifier, target] of [
    [pathToFileURL(join(root, 'package.json')).href, 'appium', entries.appium],
    [pathToFileURL(join(root, 'package.json')).href, 'appium-xcuitest-driver', entries.xcuitest],
    [new URL('./commands/context.js', entries.xcuitest).href, 'appium-remote-debugger', entries.remote],
    [new URL('./driver.js', entries.xcuitest).href, 'appium/driver.js', pathToFileURL(join(root, manifest.packages.appium.root, 'driver.js')).href],
    [new URL('./driver.js', entries.xcuitest).href, 'appium-webdriveragent', entries.wda],
  ]) assert.equal(import.meta.resolve(specifier, parent), target, 'Unexpected iOS runtime importer route');
  const main = fileURLToPath(entries.appium);
  assert.equal(await realpath(process.execPath), process.execPath, 'Symlinked Node executable');
  const evidence: Record<string, unknown> = { home: root, node: process.execPath, nodeVersion: process.versions.node, nodeSha256: hash(await readFile(process.execPath)), main, entries, contract, attestation: await attestation(root, manifest) };
  if (patched) {
    // Imported implementation checks are in addition to hashes. They do not connect to a device/inspector.
    const { XCUITestDriver } = await import(entries.xcuitest);
    const driver = new XCUITestDriver();
    assert.deepEqual(driver.mobileInstalledContextContract(), { contract, selector: contract });
    const selected = JSON.parse(execFileSync(process.execPath, [main, 'driver', 'list', '--installed', '--json'], {
      env: { ...process.env, APPIUM_HOME: root }, encoding: 'utf8', timeout: 30_000,
    }));
    assert.deepEqual(Object.keys(selected), ['xcuitest'], 'Unexpected selected iOS driver');
    assert.equal(selected.xcuitest.pkgName, manifest.packages.xcuitest.name);
    assert.equal(selected.xcuitest.version, manifest.packages.xcuitest.version);
    assert.equal(resolve(selected.xcuitest.installPath), join(root, manifest.packages.xcuitest.root));
    assert.equal(await realpath(selected.xcuitest.installPath), join(root, manifest.packages.xcuitest.root));
    assert.equal(selected.xcuitest.mainClass, 'XCUITestDriver');
    evidence.selected = { name: selected.xcuitest.pkgName, version: selected.xcuitest.version,
      installPath: selected.xcuitest.installPath, mainClass: selected.xcuitest.mainClass };
  }
  return evidence;
}

export async function patch(home: string): Promise<void> {
  await installation(home, false);
  const manifest = await load<Manifest>('producer-manifest.json');
  const output = await producerOutput(home, manifest.packages, await load<Transform>('producer-transform.json'));
  assert.deepEqual([...output.keys()].sort(), Object.keys(manifest.files).sort(), 'Incomplete iOS patch');
  for (const [key, bytes] of output) assert.equal(hash(bytes), manifest.files[key].after, `Incorrect iOS postimage: ${key}`);
  // Preflight every file before the first write. Partial patches are never accepted on a subsequent run.
  for (const [key, bytes] of output) {
    const slash = key.indexOf('/');
    await writeFile(join(home, manifest.packages[key.slice(0, slash)].root, key.slice(slash + 1)), bytes);
  }
  await writeFile(join(home, 'installed-context-integrity.json'), JSON.stringify(await installation(home, true), null, 2) + '\n');
}

export async function verify(home: string): Promise<void> {
  const expected = JSON.parse(await readFile(join(home, 'installed-context-integrity.json'), 'utf8'));
  assert.deepEqual(await attestation(home, await load<Manifest>('producer-manifest.json')), expected.attestation, 'iOS captured installation hashes changed');
  assert.deepEqual(await installation(home, true), expected, 'iOS installation changed after patch');
}

if (process.argv[1] && await realpath(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [mode, home] = process.argv.slice(2);
  assert.ok(home, 'Explicit iOS APPIUM_HOME required');
  if (mode === 'configure') await configure(home);
  else if (mode === 'pristine') await installation(home, false);
  else if (mode === 'patch') await patch(home);
  else if (mode === 'verify') await verify(home);
  else throw new Error('Expected configure, pristine, patch or verify');
}
