import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { advertisedSkillNames, normalizeCommands } from '../relay/pi-command-bridge/bridge.mjs';

if (!process.env.PI_TEST_PACKAGE) throw new Error('Set PI_TEST_PACKAGE to the installed Pi 0.87.0 package directory');
const root = resolve(process.env.PI_TEST_PACKAGE);
const orchestration = process.env.PI_TEST_ORCHESTRATION;
const dir = mkdtempSync('/tmp/herdr-pi-contract-');
process.env.HOME = dir;
process.env.PI_CODING_AGENT_DIR = join(dir, 'custom-profile', 'agent');
const load = path => import(pathToFileURL(join(root, path)));
try {
  const { AgentSession } = await load('dist/core/agent-session.js');
  const { ExtensionRunner } = await load('dist/core/extensions/runner.js');
  const { InteractiveMode } = await load('dist/modes/interactive/interactive-mode.js');
  const { DefaultResourceLoader } = await load('dist/core/resource-loader.js');
  const { SettingsManager } = await load('dist/core/settings-manager.js');
  const { loadExtensions } = await load('dist/core/extensions/loader.js');
  const loadedBridge = await loadExtensions([fileURLToPath(new URL('../relay/pi-command-bridge/index.ts', import.meta.url))], dir);
  assert.deepEqual(loadedBridge.errors, []);
  assert.equal(loadedBridge.extensions.length, 1);
  assert(loadedBridge.extensions[0].handlers.has('session_start'));
  assert(loadedBridge.extensions[0].handlers.has('session_shutdown'));
  const cwd = join(dir, 'project');
  const agentDir = process.env.PI_CODING_AGENT_DIR;
  const write = (path, content) => { mkdirSync(resolve(path, '..'), { recursive: true }); writeFileSync(path, content); };
  const extension = name => `export default function(pi) { pi.registerCommand(${JSON.stringify(name)}, { description: 'Fixture', handler() { throw new Error('must not execute'); } }); }`;
  write(join(agentDir, 'extensions', 'user.ts'), extension('collision'));
  write(join(cwd, '.pi', 'extensions', 'project.ts'), extension('project-command'));
  write(join(dir, 'cli.ts'), extension('collision'));
  write(join(agentDir, 'prompts', 'collision.md'), '---\ndescription: Template\n---\nPrivate template body');
  const skillPath = join(agentDir, 'skills', 'probe', 'SKILL.md');
  write(skillPath, '---\nname: probe\ndescription: Probe skill\ndisable-model-invocation: true\n---\nProbe body');
  const settingsManager = SettingsManager.create(cwd, agentDir);
  settingsManager.setProjectTrusted(false);
  const loader = new DefaultResourceLoader({ cwd, agentDir, settingsManager, additionalExtensionPaths: [join(dir, 'cli.ts')], noContextFiles: true });
  await loader.reload();
  assert.equal(loader.getExtensions().errors.length, 0);
  const runner = Object.create(ExtensionRunner.prototype);
  runner.extensions = loader.getExtensions().extensions;
  assert(!runner.getRegisteredCommands().some(c => c.name === 'project-command'));
  assert.deepEqual(runner.getRegisteredCommands().map(c => c.invocationName).sort(), ['collision:1', 'collision:2']);
  let core;
  runner.bindCore = value => { core = value; };
  const session = { promptTemplates: loader.getPrompts().prompts, _resourceLoader: loader, resourceLoader: loader, extensionRunner: runner };
  AgentSession.prototype._bindExtensionCore.call(session, runner);
  const commands = core.getCommands();
  assert.deepEqual(commands.map(c => c.source), ['extension', 'extension', 'prompt', 'skill']);
  assert(commands.every(c => c.sourceInfo && c.name && !c.handler && !c.content));
  assert(!commands.some(c => c.name === 'model'));
  let enabled = false;
  const interactive = {
    session, settingsManager: { getEnableSkillCommands: () => enabled }, skillCommands: new Map(),
    sessionManager: { getCwd: () => cwd }, prefixAutocompleteDescription: description => description,
  };
  const signal = new AbortController().signal;
  let provider = InteractiveMode.prototype.createBaseAutocompleteProvider.call(interactive);
  let suggestions = await provider.getSuggestions(['/skill:'], 0, 7, { force: false, signal });
  assert(!suggestions?.items.some(item => item.value.includes('probe')));
  assert(AgentSession.prototype._expandSkillCommand.call(session, '/skill:probe').includes('Probe body'));
  enabled = true;
  provider = InteractiveMode.prototype.createBaseAutocompleteProvider.call(interactive);
  suggestions = await provider.getSuggestions(['/skill:'], 0, 7, { force: false, signal });
  assert(suggestions?.items.some(item => item.value.includes('probe')));
  write(join(agentDir, 'prompts', 'skill:probe.md'), '---\ndescription: Shadowed template\n---\nWrong template body');
  await loader.reload();
  session.promptTemplates = loader.getPrompts().prompts;
  const { expandPromptTemplate } = await load('dist/core/prompt-templates.js');
  for (const visible of [true, false]) {
    enabled = visible;
    provider = InteractiveMode.prototype.createBaseAutocompleteProvider.call(interactive);
    suggestions = await provider.getSuggestions(['/skill:'], 0, 7, { force: false, signal });
    const currentCommands = core.getCommands();
    assert.equal(currentCommands.filter(command => command.name === 'skill:probe').length, 2);
    const catalog = normalizeCommands(currentCommands, advertisedSkillNames(currentCommands, suggestions));
    const invocation = expandPromptTemplate(AgentSession.prototype._expandSkillCommand.call(session, '/skill:probe'), session.promptTemplates);
    assert(invocation.includes('Probe body') && !invocation.includes('Wrong template body'));
    const selected = catalog.commands.find(command => command.command === '/skill:probe');
    if (visible) {
      assert.equal(selected.kind, 'skill');
      assert.equal(selected.description, 'Probe skill');
      assert.equal(selected.provenance.path, skillPath);
    } else {
      assert.equal(selected, undefined);
    }
  }
  runner.extensions[0].commands.set('skill:probe', { name: 'skill:probe', description: 'Extension priority', sourceInfo: commands[0].sourceInfo });
  assert.equal(runner.getCommand('skill:probe').description, 'Extension priority');
  assert.equal(normalizeCommands(core.getCommands(), new Set()).commands.find(command => command.command === '/skill:probe').kind, 'extension');
  runner.extensions[0].commands.delete('skill:probe');
  settingsManager.setProjectTrusted(true);
  await loader.reload();
  runner.extensions = loader.getExtensions().extensions;
  assert(core.getCommands().some(c => c.name === 'project-command' && c.sourceInfo.scope === 'project'));
  for (const [name, source] of [['npm-prompt', 'npm:fixture-package'], ['git-prompt', 'git:github.com/fixture/package']]) {
    const path = join(dir, 'packages', `${name}.md`);
    write(path, `---\ndescription: Package prompt\n---\nPrivate ${name} body`);
    loader.extendResources({ promptPaths: [{ path, metadata: { source, scope: 'user', origin: 'package', baseDir: join(dir, 'packages') } }] });
    session.promptTemplates = loader.getPrompts().prompts;
    assert(core.getCommands().some(c => c.name === name && c.sourceInfo.source === source && c.sourceInfo.origin === 'package'));
  }
  runner.extensions[0].commands.set('dynamic', { name: 'dynamic', description: 'Dynamic', sourceInfo: commands[0].sourceInfo });
  assert(core.getCommands().some(c => c.name === 'dynamic'));
  const disabled = new DefaultResourceLoader({ cwd, agentDir, settingsManager, noExtensions: true, noContextFiles: true });
  await disabled.reload();
  assert.equal(disabled.getExtensions().extensions.length, 0);
  const bridgeEntry = join(dir, 'bridge-reload', 'index.ts');
  const bridgeModule = join(dir, 'bridge-reload', 'bridge.mjs');
  const bridgeEntrypoint = readFileSync(fileURLToPath(new URL('../relay/pi-command-bridge/index.ts', import.meta.url)), 'utf8');
  for (const value of ['before', 'after', 'updated-again']) {
    write(bridgeEntry, value === 'before' ? "export { default } from './bridge.mjs';" : bridgeEntrypoint);
    write(bridgeModule, `export default pi => pi.on('session_start', () => '${value}');`);
    const loaded = await loadExtensions([bridgeEntry], cwd);
    assert.deepEqual(loaded.errors, []);
    assert.equal(await loaded.extensions[0].handlers.get('session_start')[0](), value);
  }
  if (orchestration) {
    const loaded = await loadExtensions([resolve(orchestration)], cwd);
    assert.deepEqual(loaded.errors, []);
    runner.extensions = loaded.extensions;
    for (const name of ['orchestrate', 'orchestrate-plan', 'orchestrate-implement', 'orchestrate-status', 'orchestrate-answer']) {
      assert(core.getCommands().some(c => c.name === name && c.source === 'extension'));
    }
  }
  console.log('PASS: Pi runtime metadata, custom profile, CLI/local/project resources, trust/disable filtering, collisions, prompts, dynamic registration, and skill invocation/autocomplete contract');
} finally {
  rmSync(dir, { recursive: true, force: true });
}
