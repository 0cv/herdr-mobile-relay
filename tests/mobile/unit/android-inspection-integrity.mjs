import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {relocateDeclarationMap} from '../android-appium/declaration-map.ts';

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..');
const source = path.join(repo, 'tests/mobile/android-appium');
const home = process.env.APPIUM_HOME;
const output = process.env.MOBILE_INSPECTION_TEST_OUTPUT;
const pristine = process.env.MOBILE_PRISTINE_ANDROID_PACKAGE;
const pristineUia = process.env.MOBILE_PRISTINE_UIA_PACKAGE;
const temporaryParent = process.env.TMPDIR || '/tmp';
assert.ok(process.env.HERDR_OWNED_INSTALLER_FIXTURE === '1' && [home, output, pristine, pristineUia, temporaryParent].every(value => typeof value === 'string' && value.length > 0 && path.isAbsolute(value)), 'Explicit disposable fixture authority required');
assert.equal(path.relative(repo, fileURLToPath(import.meta.url)), 'tests/mobile/unit/android-inspection-integrity.mjs');
assert.ok(fs.lstatSync(path.dirname(repo)).isDirectory(), 'Disposable fixture must be a directory, not a symlink');
const fixture = fs.realpathSync(path.dirname(repo));
assert.equal(path.dirname(fixture), fs.realpathSync(temporaryParent), 'Disposable fixture must be a direct child of configured TMPDIR');
assert.match(path.basename(fixture), /^herdr-inspection-check\.[A-Za-z0-9]{8}(?![\s\S])/, 'Fresh inspection fixture name required');
for (const [directory, name] of [[repo, 'source'], [home, 'install'], [output, 'output'], [pristine, 'pristine'], [pristineUia, 'pristine-uia']]) {
  assert.ok(fs.lstatSync(directory).isDirectory(), `Owned ${name} directory required`);
  assert.equal(fs.realpathSync(directory), path.join(fixture, name), `Owned ${name} must match fixture layout`);
}
const ownershipMarker = path.join(fixture, '.owned');
assert.ok(fs.lstatSync(ownershipMarker).isFile(), 'Regular non-symlink fixture marker required');
assert.equal(fs.readFileSync(ownershipMarker, 'utf8'), 'herdr-owned-inspection-fixture\n');
const gate = path.join(output, 'gate-fixture');
fs.mkdirSync(output, {recursive: true});
fs.cpSync(source, gate, {recursive: true});
if (!fs.existsSync(path.join(gate, 'node_modules'))) fs.symlinkSync(path.join(repo, 'tests/mobile/node_modules'), path.join(gate, 'node_modules'));
const manifest = JSON.parse(fs.readFileSync(path.join(gate, 'producer-manifest.json')));
const runtime = JSON.parse(fs.readFileSync(path.join(gate, 'runtime-manifest.json')));
const root = path.join(home, runtime.android.root);
const producerPath = file => path.join(home, runtime[manifest.files[file].package].root, file);
const pristinePath = file => path.join(manifest.files[file].package === 'uiautomator2' ? pristineUia : pristine, file);
const results = [];
const marker = path.join(output, 'appium-started');
const bin = path.join(output, 'bin');
fs.mkdirSync(bin, {recursive: true});
fs.writeFileSync(path.join(bin, 'node'), `#!/usr/bin/env bash\nset -euo pipefail\nif [ "\${1:-}" = "$APPIUM_HOME/node_modules/appium/build/lib/main.js" ] && [ "\${2:-}" = --version ]; then touch "$MOBILE_START_MARKER"; fi\nexec ${JSON.stringify(process.execPath)} "$@"\n`, {mode: 0o755});
const env = {...process.env, PATH: `${bin}:${process.env.PATH}`, RUNNER_TEMP: output, MOBILE_START_MARKER: marker};
const paths = ['.github/actions/mobile-device-run/action.yml'].map(file => {
  const text = fs.readFileSync(path.join(repo, file), 'utf8');
  assert.match(text, /bash tests\/mobile\/android-appium\/ci.sh install/);
  const verify = text.split('\n').find(line => line.includes('bash tests/mobile/android-appium/ci.sh verify')).trim();
  const start = text.split('\n').find(line => line.includes('bash tests/mobile/android-appium/ci.sh start')).trim().replace(/ &$/, '');
  assert.ok(text.indexOf(verify) < text.indexOf(start));
  return {file, script: `set -euo pipefail\nappium_args=(--version)\n${verify}\n${start}\n`};
});
function run(mode, pass, label) {
  const result = spawnSync(process.execPath, ['--experimental-import-meta-resolve', path.join(gate, 'gate.ts'), mode, home], {cwd: repo, env, encoding: 'utf8', timeout: 45000});
  assert.equal(result.status === 0, pass, `${label}\n${result.stderr}`);
  results.push({label, mode, status: result.status});
}
function startup(pass, label) {
  for (const entry of paths) {
    fs.rmSync(marker, {force: true});
    const result = spawnSync('bash', ['-c', entry.script], {cwd: repo, env, encoding: 'utf8', timeout: 45000});
    assert.equal(result.status === 0, pass, `${entry.file}: ${label}\n${result.stderr}`);
    assert.equal(fs.existsSync(marker), pass, `Unverified startup in ${entry.file}: ${label}`);
    results.push({label, path: entry.file, status: result.status, started: fs.existsSync(marker)});
  }
}
function mutation(file, change, action) {
  const before = fs.readFileSync(file);
  try { fs.writeFileSync(file, change(before)); action(); }
  finally { fs.writeFileSync(file, before); }
}
function missing(file, action) {
  const saved = `${file}.fixture-original`;
  fs.renameSync(file, saved);
  try { action(); }
  finally { fs.renameSync(saved, file); }
}
const append = bytes => Buffer.concat([bytes, Buffer.from('\n')]);
run('verify', true, 'healthy complete installed contract');
startup(true, 'shared CI verify/start fragments');
run('patch', false, 'already patched producer refuses');
const shadowAxios = path.join(home, runtime.base.root, 'build/lib/jsonwp-proxy/node_modules/axios');
const shadowAxiosMarker = path.join(output, 'shadow-axios-executed');
fs.mkdirSync(shadowAxios, {recursive: true});
fs.writeFileSync(path.join(shadowAxios, 'package.json'), JSON.stringify({name: 'axios', version: '0.0.0', main: 'index.js'}));
fs.writeFileSync(path.join(shadowAxios, 'index.js'), `require('node:fs').writeFileSync(${JSON.stringify(shadowAxiosMarker)}, 'executed'); module.exports = {shadow: true};\n`);
try {
  run('verify', false, 'shadow Axios under effective proxy importer is rejected');
  assert.equal(fs.existsSync(shadowAxiosMarker), false, 'Unverified shadow Axios must not execute during verify');
  startup(false, 'shadow Axios under effective proxy importer is rejected');
  assert.equal(fs.existsSync(shadowAxiosMarker), false, 'Unverified shadow Axios must not execute during startup');
} finally {
  fs.rmSync(path.join(home, runtime.base.root, 'build/lib/jsonwp-proxy/node_modules'), {recursive: true, force: true});
  fs.rmSync(shadowAxiosMarker, {force: true});
}
for (const file of Object.keys(manifest.files)) {
  mutation(producerPath(file), append, () => { run('verify', false, `transformed mutation ${file}`); startup(false, `transformed mutation ${file}`); });
  if (file.endsWith('.map') || manifest.files[file].package === 'uiautomator2') missing(producerPath(file), () => { run('verify', false, `missing required producer ${file}`); startup(false, `missing required producer ${file}`); });
  if (manifest.files[file].package === 'uiautomator2') {
    const wrongPosthash = bytes => {
      const value = JSON.parse(bytes); value.files[file].after = '0'.repeat(64); return JSON.stringify(value);
    };
    mutation(path.join(gate, 'producer-manifest.json'), wrongPosthash, () => run('verify', false, `wrong UiAutomator2 installed posthash ${file}`));
    mutation(path.join(source, 'producer-manifest.json'), wrongPosthash, () => startup(false, `wrong UiAutomator2 installed posthash ${file}`));
  }
}
for (const [id, expected] of Object.entries(runtime)) mutation(path.join(home, expected.root, 'package.json'), bytes => JSON.stringify({...JSON.parse(bytes), version: '0.0.0'}), () => run('verify', false, `wrong runtime version ${id}`));
mutation(path.join(home, runtime.wrapper.root, 'package.json'), bytes => JSON.stringify({...JSON.parse(bytes), exports: './build/lib/chromedriver.js'}), () => run('verify', false, 'changed wrapper import condition'));
mutation(path.join(home, runtime.uiautomator2.root, 'package.json'), bytes => JSON.stringify({...JSON.parse(bytes), exports: './build/lib/driver.js'}), () => {
  run('verify', false, 'wrong effective UiAutomator2 resolver');
  startup(false, 'wrong effective UiAutomator2 resolver');
});
const entry = path.join(home, runtime.wrapper.root, runtime.wrapper.entry);
missing(entry, () => {
  fs.symlinkSync(`${entry}.fixture-original`, entry);
  try { run('verify', false, 'symlinked effective entrypoint'); }
  finally { fs.unlinkSync(entry); }
});
const metadata = path.join(home, 'node_modules/.cache/appium/extensions.yaml');
for (const [name, before, after] of [
  ['class', 'mainClass: AndroidUiautomator2Driver', 'mainClass: WrongDriver'],
  ['version', 'version: 8.7.0', 'version: 0.0.0'],
  ['selected-driver', 'pkgName: appium-uiautomator2-driver', 'pkgName: wrong-driver'],
  ['path', 'installPath: ', 'installPath: /wrong/'],
]) mutation(metadata, bytes => bytes.toString().replace(before, after), () => run('verify', false, `selected driver metadata ${name}`));
mutation(path.join(home, 'package-lock.json'), append, () => run('verify', false, 'modified installation lock'));
for (const file of Object.keys(manifest.authored)) {
  mutation(path.join(root, file), append, () => { run('verify', false, `authored runtime mutation ${file}`); startup(false, `authored runtime mutation ${file}`); });
  missing(path.join(root, file), () => startup(false, `missing authored runtime ${file}`));
}
for (const name of new Set(Object.values(manifest.authored).map(item => item.source))) mutation(path.join(source, name), append, () => startup(false, `authored prospective input mutation ${name}`));
for (const file of Object.keys(manifest.inputs)) mutation(path.join(source, file), append, () => startup(false, `prospective generator input mutation ${file}`));
for (const file of Object.keys(runtime.ws.files)) mutation(path.join(home, runtime.ws.root, file), append, () => { run('verify', false, `ws runtime mutation ${file}`); startup(false, `ws runtime mutation ${file}`); });
for (const resolverDirectory of ['node_modules', 'lib/node_modules']) for (const name of ['bufferutil', 'utf-8-validate']) {
  const directory = path.join(home, runtime.ws.root, resolverDirectory, name);
  assert.equal(fs.existsSync(directory), false);
  fs.mkdirSync(directory, {recursive: true});
  fs.writeFileSync(path.join(directory, 'package.json'), JSON.stringify({name, version: '0.0.0', main: 'index.js'}));
  const executed = path.join(output, `unverified-${name}`);
  fs.rmSync(executed, {force: true});
  fs.writeFileSync(path.join(directory, 'index.js'), `require('node:fs').writeFileSync(${JSON.stringify(executed)}, 'executed'); module.exports = {};\n`);
  try {
    run('verify', false, `new optional resolver ${resolverDirectory}/${name}`);
    startup(false, `new optional resolver ${resolverDirectory}/${name}`);
    assert.equal(fs.existsSync(executed), false, 'Unverified optional dependency must not execute even during driver discovery');
  } finally { fs.rmSync(directory, {recursive: true}); fs.rmSync(executed, {force: true}); }
}
const producerSnapshot = Object.fromEntries(Object.keys(manifest.files).map(file => [file, fs.readFileSync(producerPath(file))]));
const driverSource = 'lib/driver.ts';
const beforeDriver = fs.readFileSync(pristinePath(driverSource), 'utf8');
const afterDriver = producerSnapshot[driverSource].toString();
const declaration = producerSnapshot['build/lib/driver.d.ts'].toString();
const declarationMap = fs.readFileSync(pristinePath('build/lib/driver.d.ts.map'), 'utf8');
const edits = JSON.parse(fs.readFileSync(path.join(gate, 'producer-transform.json'))).driver;
assert.equal(relocateDeclarationMap(driverSource, beforeDriver, afterDriver, declaration, declarationMap, edits), producerSnapshot['build/lib/driver.d.ts.map'].toString());
for (const [from, to] of [
  ['  ): Promise<any> {', '  ): Promise<string> {'],
  ['  systemPort: number | undefined;', '  systemPort: string | undefined;'],
  ["    return {};", "    return {changed: true};"],
  ['      const defaultOpts = {', '      const changedDefaultOpts = {'],
]) assert.throws(() => relocateDeclarationMap(driverSource, beforeDriver, afterDriver.replace(from, to), declaration, declarationMap, [...edits, [from, to]]), /Only the createSession catch may change/);
const wrongMap = JSON.parse(declarationMap);
wrongMap.sources = ['../../wrong.ts'];
assert.throws(() => relocateDeclarationMap(driverSource, beforeDriver, afterDriver, declaration, JSON.stringify(wrongMap), edits));
const runtimeMap = JSON.parse(producerSnapshot['build/lib/driver.js.map']);
assert.deepEqual(runtimeMap.sources, ['../../lib/driver.ts']);
assert.deepEqual(runtimeMap.sourcesContent, [afterDriver]);
assert.deepEqual(producerSnapshot['build/lib/driver.d.ts'], fs.readFileSync(pristinePath('build/lib/driver.d.ts')));
results.push({label: 'UiAutomator2 catch-only, class/member signatures and exact declaration/runtime maps', checks: 9});
const authoredSnapshot = Object.fromEntries(Object.keys(manifest.authored).map(file => [file, fs.readFileSync(path.join(root, file))]));
try {
  for (const file of Object.keys(producerSnapshot)) fs.copyFileSync(pristinePath(file), producerPath(file));
  for (const file of Object.keys(authoredSnapshot)) fs.unlinkSync(path.join(root, file));
  for (const file of Object.keys(manifest.files)) {
    mutation(producerPath(file), append, () => run('patch', false, `modified pristine producer input ${file}`));
    if (manifest.files[file].package === 'uiautomator2') {
      missing(producerPath(file), () => run('patch', false, `missing pristine UiAutomator2 producer ${file}`));
      mutation(path.join(gate, 'producer-manifest.json'), bytes => {
        const value = JSON.parse(bytes); value.files[file].after = '0'.repeat(64); return JSON.stringify(value);
      }, () => run('patch', false, `wrong UiAutomator2 posthash ${file}`));
    }
  }
  mutation(path.join(gate, 'producer-manifest.json'), bytes => {
    const value = JSON.parse(bytes); value.files['lib/driver.ts'].package = 'android'; return JSON.stringify(value);
  }, () => run('patch', false, 'wrong UiAutomator2 producer package ownership'));
  mutation(path.join(gate, 'producer-manifest.json'), bytes => {
    const value = JSON.parse(bytes); value.files['build/lib/commands/context/exports.js'].after = '0'.repeat(64); return JSON.stringify(value);
  }, () => run('patch', false, 'incorrect generated posthash before writes'));
  mutation(path.join(gate, 'producer-manifest.json'), bytes => {
    const value = JSON.parse(bytes); delete value.files['build/lib/commands/context/helpers.d.ts.map']; return JSON.stringify(value);
  }, () => run('patch', false, 'missing prospective declaration map'));
  mutation(path.join(gate, 'producer-manifest.json'), bytes => {
    const value = JSON.parse(bytes); delete value.authored['build/lib/commands/context/retained-inspection.d.cts']; return JSON.stringify(value);
  }, () => run('patch', false, 'missing prospective authored type'));
  run('patch', true, 'pristine deterministic transformed and authored installation');
  for (const [file, bytes] of Object.entries(producerSnapshot)) assert.deepEqual(fs.readFileSync(producerPath(file)), bytes, `Exact regenerated postimage ${file}`);
  for (const [file, bytes] of Object.entries(authoredSnapshot)) assert.deepEqual(fs.readFileSync(path.join(root, file)), bytes, `Exact regenerated authored input ${file}`);
} finally {
  for (const [file, bytes] of Object.entries(producerSnapshot)) fs.writeFileSync(producerPath(file), bytes);
  for (const [file, bytes] of Object.entries(authoredSnapshot)) fs.writeFileSync(path.join(root, file), bytes);
}
run('verify', true, 'restored complete integrity');
startup(true, 'shared CI path restored and reverified');
fs.writeFileSync(path.join(output, 'results.json'), JSON.stringify(results, null, 2) + '\n');
console.log(`PASS ${results.length} effective installer/startup controls for the shared CI path`);
