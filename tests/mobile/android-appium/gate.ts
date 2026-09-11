import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { mkdir, readFile, readdir, realpath, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import ts from 'typescript';
import { relocateDeclarationMap } from './declaration-map.ts';

type RuntimePackage = {
  name: string; parent: string | null; condition: 'import' | 'require'; version: string;
  engines: Record<string, string>; root: string; entry: string; files: Record<string, string>;
};
type ProducerManifest = {
  compiler: string;
  files: Record<string, { before: string; after: string }>;
  authored: Record<string, { source: string; sha256: string }>;
  inputs: Record<string, string>;
};
const authoredFiles = ['adb-inspection.cjs', 'adb-inspection.d.cts', 'retained-inspection.cjs', 'retained-inspection.d.cts', 'target-inspection.cjs', 'target-inspection.d.cts'];
const hash = (bytes: string | Buffer) => createHash('sha256').update(bytes).digest('hex');
const artifact = (name: string) => new URL(name, import.meta.url);
const load = async <T>(name: string): Promise<T> => JSON.parse(await readFile(artifact(name), 'utf8')) as T;

export async function configure(home: string): Promise<void> {
  await mkdir(home, { recursive: true });
  assert.deepEqual(await readdir(home), [], 'Android Appium home must be fresh');
  for (const file of ['package.json', 'package-lock.json']) await writeFile(join(home, file), await readFile(artifact(file)));
}

async function effectiveInstallation(home: string, patched: boolean): Promise<Record<string, unknown>> {
  assert.ok(process.execArgv.includes('--experimental-import-meta-resolve'), 'Explicit Node ESM parent resolution is required');
  assert.ok(Number(process.versions.node.split('.')[0]) >= 24, 'Use the existing Node 24+ toolchain');
  assert.ok(Number(execFileSync('npm', ['--version'], { encoding: 'utf8' }).trim().split('.')[0]) >= 10);
  assert.ok(!process.env.NODE_PATH, 'NODE_PATH cannot alter the installed producer resolver');
  const root = await realpath(home);
  const packages: Record<string, RuntimePackage> = { appium: await load<RuntimePackage>('appium-runtime-manifest.json'),
    ...await load<Record<string, RuntimePackage>>('runtime-manifest.json') };
  const producer = await load<ProducerManifest>('producer-manifest.json');
  assert.deepEqual(Object.keys(producer.inputs).sort(), ['appium-runtime-manifest.json', 'declaration-map.ts', 'gate.ts', 'producer-transform.json', 'runtime-manifest.json'].sort(), 'Incomplete producer input contract');
  for (const [file, expected] of Object.entries(producer.inputs)) {
    assert.equal(hash(await readFile(artifact(file))), expected, `Changed producer input: ${file}`);
  }
  const authoredPaths = ['lib/commands/context', 'build/lib/commands/context'].flatMap(directory => authoredFiles.map(file => `${directory}/${file}`));
  assert.deepEqual(Object.keys(producer.authored).sort(), authoredPaths.sort(), 'Incomplete authored producer contract');
  const authoredEvidence: Record<string, string> = {};
  for (const file of authoredPaths) {
    const expected = producer.authored[file];
    assert.equal(expected.source, file.split('/').at(-1), 'Incorrect authored source association');
    assert.equal(hash(await readFile(artifact(expected.source))), expected.sha256, `Changed authored input: ${file}`);
    const path = join(root, packages.android.root, file);
    if (patched) {
      assert.equal(await realpath(path), path, `Symlinked authored producer: ${file}`);
      assert.equal(hash(await readFile(path)), expected.sha256, `Changed authored producer: ${file}`);
      authoredEvidence[file] = expected.sha256;
    } else {
      await assert.rejects(readFile(path), { code: 'ENOENT' }, `Unexpected existing authored producer: ${file}`);
    }
  }
  const producerFiles = ['exports', 'helpers'].flatMap(name => [
    `lib/commands/context/${name}.ts`,
    ...['.js', '.js.map', '.d.ts', '.d.ts.map'].map(extension => `build/lib/commands/context/${name}${extension}`),
  ]);
  assert.deepEqual(Object.keys(producer.files).sort(), producerFiles.sort(), 'Incomplete producer artifacts');
  for (const file of producerFiles) {
    assert.equal(packages.android.files[file], producer.files[file].before, `Unreviewed producer original: ${file}`);
  }
  for (const file of ['package.json', 'package-lock.json']) {
    assert.equal(hash(await readFile(join(root, file))), hash(await readFile(artifact(file))), `Changed installation ${file}`);
  }
  const entries: Record<string, string> = {};
  const evidence: Record<string, unknown> = { home: root, node: process.versions.node, authored: authoredEvidence, inputs: producer.inputs };
  for (const [id, expected] of Object.entries(packages)) {
    const parent = expected.parent ? entries[expected.parent] : pathToFileURL(join(root, 'package.json')).href;
    const entry = expected.condition === 'require'
      ? pathToFileURL(createRequire(parent).resolve(expected.name)).href
      : import.meta.resolve(expected.name, parent);
    entries[id] = entry;
    const packageRoot = join(root, expected.root);
    assert.equal(await realpath(packageRoot), packageRoot, `${id}: unexpected package realpath`);
    assert.equal(fileURLToPath(entry), join(packageRoot, expected.entry), `${id}: unexpected effective entrypoint`);
    assert.equal(await realpath(fileURLToPath(entry)), fileURLToPath(entry), `${id}: symlinked entrypoint`);
    const pkg = JSON.parse(await readFile(join(packageRoot, 'package.json'), 'utf8'));
    assert.equal(pkg.name, expected.name);
    assert.equal(pkg.version, expected.version);
    assert.deepEqual(pkg.engines, expected.engines);
    const observed: Record<string, string> = {};
    for (const [file, original] of Object.entries(expected.files)) {
      const path = join(packageRoot, file);
      assert.equal(await realpath(path), path, `${id}/${file}: unexpected realpath`);
      const value = hash(await readFile(path));
      const expectedHash = patched && id === 'android' && producer.files[file] ? producer.files[file].after : original;
      assert.equal(value, expectedHash, `${id}/${file}: unexpected runtime bytes`);
      observed[file] = value;
    }
    evidence[id] = { entry, parent, condition: expected.condition, realpath: packageRoot, version: pkg.version, ...(pkg.engines ? { engines: pkg.engines } : {}), files: observed };
  }
  const context = new URL('./commands/context/exports.js', entries.android).href;
  const helper = new URL('./retained-inspection.cjs', context).href;
  if (patched) assert.equal(pathToFileURL(createRequire(helper).resolve('./adb-inspection.cjs')).href, new URL('./adb-inspection.cjs', context).href);
  const transport = new URL('./target-inspection.cjs', context).href;
  assert.equal(import.meta.resolve('./retained-inspection.cjs', context), helper);
  if (patched) assert.equal(pathToFileURL(createRequire(helper).resolve('./target-inspection.cjs')).href, transport);
  assert.equal(pathToFileURL(createRequire(transport).resolve('ws')).href, entries.ws);
  for (const [parent, specifiers] of [
    [helper, ['appium-android-driver', 'appium-chromedriver']],
    [new URL('./lib/buffer-util.js', entries.ws).href, ['bufferutil']],
    [new URL('./lib/validation.js', entries.ws).href, ['utf-8-validate']],
  ] as const) {
    for (const specifier of specifiers) {
      if (specifier === 'appium-android-driver' || specifier === 'appium-chromedriver') {
        const target = specifier === 'appium-android-driver' ? entries.android : entries.wrapper;
        assert.equal(import.meta.resolve(specifier, parent), target, 'Changed authored declaration type resolver');
      } else {
        assert.throws(() => createRequire(parent).resolve(specifier), { code: 'MODULE_NOT_FOUND' }, `Unexpected optional ws resolver: ${specifier}`);
      }
    }
  }
  evidence.inspectionResolvers = {context, helper, transport, ws: entries.ws, absent: ['bufferutil', 'utf-8-validate']};
  const main = join(root, packages.appium.root, 'build/lib/main.js');
  assert.ok(packages.appium.files['build/lib/main.js'], 'Appium executable must be attested');
  const selected = JSON.parse(execFileSync(process.execPath, [main, 'driver', 'list', '--installed', '--json'], {
    env: { ...process.env, APPIUM_HOME: root }, encoding: 'utf8', timeout: 30_000,
  }));
  assert.deepEqual(Object.keys(selected), ['uiautomator2'], 'Unexpected selected Appium driver');
  assert.equal(selected.uiautomator2.pkgName, packages.uiautomator2.name);
  assert.equal(selected.uiautomator2.version, packages.uiautomator2.version);
  assert.equal(selected.uiautomator2.mainClass, 'AndroidUiautomator2Driver');
  assert.equal(selected.uiautomator2.automationName, 'UiAutomator2');
  assert.equal(selected.uiautomator2.installType, 'npm');
  assert.equal(await realpath(selected.uiautomator2.installPath), join(root, packages.uiautomator2.root));
  evidence.selected = selected;
  for (const [parent, specifier, target, condition] of [
    [new URL('./driver.js', entries.uiautomator2).href, 'appium-android-driver', entries.android, 'import'],
    [new URL('./commands/context/helpers.js', entries.android).href, 'appium-chromedriver', entries.wrapper, 'import'],
    [new URL('./commands/context/exports.js', entries.android).href, 'appium-chromedriver', entries.wrapper, 'import'],
    [new URL('./chromedriver.js', entries.wrapper).href, '@appium/base-driver', entries.base, 'import'],
    [new URL('./build/lib/jsonwp-proxy/proxy.js', entries.base).href, 'axios', entries.axios, 'require'],
  ]) {
    const resolved = condition === 'require' ? pathToFileURL(createRequire(parent).resolve(specifier)).href : import.meta.resolve(specifier, parent);
    assert.equal(resolved, target, `Unexpected effective import route from ${parent}`);
  }
  return evidence;
}

export async function patch(home: string): Promise<void> {
  await effectiveInstallation(home, false);
  const manifest = await load<ProducerManifest>('producer-manifest.json');
  assert.equal(ts.version, manifest.compiler, 'Unexpected source-map compiler');
  const transform = await load<Record<string, Array<[string, string]>>>('producer-transform.json');
  const runtime = await load<Record<string, RuntimePackage>>('runtime-manifest.json');
  const root = join(await realpath(home), runtime.android.root);
  const output = new Map<string, string>();
  for (const [name, replacements] of Object.entries(transform)) {
    const source = `lib/commands/context/${name}.ts`;
    const original = await readFile(join(root, source), 'utf8');
    let text = original;
    for (const [before, after] of replacements) {
      assert.equal(text.split(before).length, 2, `Nonunique producer transformation: ${source}`);
      text = text.replace(before, after);
    }
    const result = ts.transpileModule(text, { fileName: source, compilerOptions: {
      target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext, sourceMap: true, inlineSources: true,
    } });
    assert.ok(result.sourceMapText);
    const map = JSON.parse(result.sourceMapText);
    map.sources = [`../../../../${source}`];
    const declarationPath = `build/lib/commands/context/${name}.d.ts`;
    const declaration = await readFile(join(root, declarationPath), 'utf8');
    output.set(declarationPath, declaration);
    output.set(`${declarationPath}.map`, relocateDeclarationMap(name, original, text, declaration,
      await readFile(join(root, `${declarationPath}.map`), 'utf8'), replacements));
    output.set(source, text);
    output.set(`build/lib/commands/context/${name}.js`, result.outputText);
    output.set(`build/lib/commands/context/${name}.js.map`, JSON.stringify(map));
  }
  assert.deepEqual([...output.keys()].sort(), Object.keys(manifest.files).sort());
  for (const [file, bytes] of output) {
    assert.equal(hash(await readFile(join(root, file))), manifest.files[file].before, `Modified producer input: ${file}`);
    assert.equal(hash(bytes), manifest.files[file].after, `Incorrect producer postimage: ${file}`);
  }
  for (const [file, bytes] of output) await writeFile(join(root, file), bytes);
  for (const [file, expected] of Object.entries(manifest.authored)) {
    await writeFile(join(root, file), await readFile(artifact(expected.source)));
  }
  const evidence = await effectiveInstallation(home, true);
  await writeFile(join(home, 'retained-owner-integrity.json'), JSON.stringify(evidence, null, 2) + '\n');
}

export async function verify(home: string): Promise<void> {
  const expected = JSON.parse(await readFile(join(home, 'retained-owner-integrity.json'), 'utf8'));
  assert.deepEqual(await effectiveInstallation(home, true), expected, 'Installation changed after producer patch');
}

if (process.argv[1] && await realpath(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [mode, home] = process.argv.slice(2);
  assert.ok(home && relative(dirname(fileURLToPath(import.meta.url)), home) !== '', 'Explicit isolated APPIUM_HOME is required');
  if (mode === 'configure') await configure(home);
  else if (mode === 'patch') await patch(home);
  else if (mode === 'verify') await verify(home);
  else throw new Error('Expected configure, patch or verify');
}
