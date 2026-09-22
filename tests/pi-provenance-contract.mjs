import assert from 'node:assert/strict';
import { createConnection } from 'node:net';
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { spawnSync } from 'node:child_process';
import { normalizeCommands, startBridge } from '../relay/pi-command-bridge/bridge.mjs';

if (!process.env.PI_TEST_PACKAGE) throw new Error('Set PI_TEST_PACKAGE to the installed Pi package directory');
const originalEnv = { ...process.env };
const root = resolve(process.env.PI_TEST_PACKAGE);
const dir = mkdtempSync('/tmp/pi-provenance-');
const cwd = join(dir, 'project');
const agentDir = join(dir, 'agent');
process.env.HOME = dir;
process.env.PI_CODING_AGENT_DIR = agentDir;
const load = path => import(pathToFileURL(join(root, path)));
let close;
try {
  const { parseGitUrl } = await load('dist/utils/git.js');
  const { DefaultPackageManager } = await load('dist/core/package-manager.js');
  const { DefaultResourceLoader } = await load('dist/core/resource-loader.js');
  const { SettingsManager } = await load('dist/core/settings-manager.js');
  mkdirSync(cwd, { recursive: true });
  mkdirSync(agentDir, { recursive: true });
  const settingsManager = SettingsManager.create(cwd, agentDir);
  settingsManager.setProjectTrusted(true);
  const packages = new DefaultPackageManager({ cwd, agentDir, settingsManager });
  const loader = new DefaultResourceLoader({ cwd, agentDir, settingsManager, noExtensions: true, noContextFiles: true });
  await loader.reload();
  const cases = JSON.parse(readFileSync(new URL('../contracts/fixtures/pi_provenance_redaction.json', import.meta.url), 'utf8'))
    .filter(fixture => fixture.installed_pi);
  assert.deepEqual(cases.map(fixture => fixture.installed_pi.id), ['password', 'username', 'encoded-password', 'single-label', 'encoded-single-label']);
  const scopes = ['user', 'project', 'temporary'];
  const expected = new Map();
  for (const fixture of cases) {
    const parsed = parseGitUrl(fixture.input);
    assert(parsed?.host.includes('REDACTION_'), 'Pi must accept and retain synthetic authentication in the shorthand authority');
    assert.equal(parsed.path, fixture.installed_pi.repository);
    for (const scope of scopes) {
      const baseDir = packages.getGitInstallPath(parsed, scope);
      assert(baseDir.includes('REDACTION_'), 'Use the credential-bearing install path derived by Pi, not a hand-written safe path');
      const name = `private-${fixture.installed_pi.id}-${scope}`;
      const path = join(baseDir, `${name}.md`);
      mkdirSync(baseDir, { recursive: true });
      writeFileSync(path, '---\ndescription: Offline package prompt\n---\nPrivate prompt body');
      loader.extendResources({ promptPaths: [{ path, metadata: { source: fixture.input, scope, origin: 'package', baseDir } }] });
      let authorityParts = 0;
      const displayBaseDir = baseDir.split('/').map(part => {
        if (part !== parsed.host && part !== `git-${parsed.host}`) return part;
        authorityParts++;
        return fixture.installed_pi.host;
      }).join('/');
      assert.equal(authorityParts, 1, 'Expected exactly one Pi-derived authority component');
      assert(displayBaseDir.endsWith(`/${fixture.installed_pi.repository}`));
      expected.set(`/${name}`, { source: fixture.expected, path: join(displayBaseDir, `${name}.md`), base_dir: displayBaseDir, scope, origin: 'package' });
    }
  }
  const commands = loader.getPrompts().prompts.map(prompt => ({ name: prompt.name, description: prompt.description, source: 'prompt', sourceInfo: prompt.sourceInfo }));
  assert.equal(expected.size, cases.length * scopes.length);
  assert.equal(commands.length, expected.size);
  assert.deepEqual(commands.map(command => `/${command.name}`).sort(), [...expected.keys()].sort());
  for (const command of commands) {
    for (const field of ['source', 'path', 'baseDir']) {
      assert(command.sourceInfo[field].includes('REDACTION_'), `Pi fixture must exercise the unsafe ${field} field`);
    }
  }
  const identity = { instance: 'd'.repeat(64), pane: 'fixture', session: 'fixture', pid: process.pid };
  const directory = join(dir, 'socket');
  close = await startBridge({ ...identity, directory, catalog: () => normalizeCommands(commands, new Set()) });
  const payload = await new Promise((resolve, reject) => {
    const socket = createConnection(join(directory, `${process.pid}.sock`));
    let data = '';
    socket.setTimeout(2000, () => socket.destroy(new Error('Bridge query timed out')));
    socket.on('connect', () => socket.write(`${JSON.stringify({ ...identity, challenge: 'e'.repeat(32) })}\n`));
    socket.on('data', chunk => { data += chunk; });
    socket.on('error', reject);
    socket.on('end', () => resolve(data));
  });
  assert(!payload.includes('REDACTION_'), 'No synthetic authentication marker may appear anywhere in the bridge payload');
  const response = JSON.parse(payload);
  assert.equal(response.commands.length, expected.size);
  assert.deepEqual(response.commands.map(command => command.command).sort(), [...expected.keys()].sort());
  for (const command of response.commands) {
    assert.deepEqual(command.provenance, expected.get(command.command));
  }
  await close();
  close = undefined;
  const raw = commands.map(command => ({
    command: `/${command.name}`, description: command.description,
    source: command.sourceInfo.scope === 'user' ? 'personal' : command.sourceInfo.scope,
    kind: command.source,
    provenance: { path: command.sourceInfo.path, source: command.sourceInfo.source, scope: command.sourceInfo.scope, origin: command.sourceInfo.origin, base_dir: command.sourceInfo.baseDir },
  }));
  const fixturePath = join(dir, 'raw-pi-metadata.json');
  writeFileSync(fixturePath, JSON.stringify({ entries: raw, expected: Object.fromEntries(expected) }), { mode: 0o600 });
  const check = spawnSync('go', ['test', './internal/pibridge', '-run', '^TestInstalledPiProvenanceCredentials$', '-count=1'], {
    cwd: fileURLToPath(new URL('..', import.meta.url)),
    env: { ...originalEnv, GOPROXY: 'off', GOSUMDB: 'off', PI_PROVENANCE_FIXTURE: fixturePath },
    encoding: 'utf8', timeout: 60000,
  });
  assert.equal(check.status, 0, `Go relay-boundary regression failed: ${check.stderr || check.error || ''}`);
  console.log(`PASS: ${cases.length} installed-Pi shorthand cases across ${scopes.length} scopes (${expected.size} resources); exact nonsecret provenance retained and all bridge/relay fields exclude synthetic authentication`);
} finally {
  await close?.();
  rmSync(dir, { recursive: true, force: true });
}
