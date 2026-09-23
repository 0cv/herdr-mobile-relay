import { test, expect } from 'bun:test';
import { createConnection, createServer } from 'node:net';
import { mkdtemp, rm, chmod, lstat } from 'node:fs/promises';
import { join } from 'node:path';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { fileURLToPath } from 'node:url';
import redactionCases from '../contracts/fixtures/pi_provenance_redaction.json';
import bridge, { createCatalog, normalizeCommands, startBridge, instanceIdentity } from '../relay/pi-command-bridge/bridge.mjs';

const sourceInfo = { path: '/fixture/extension.ts', source: 'npm:test', scope: 'user', origin: 'package' };
const entry = (name, source = 'extension', info = sourceInfo) => ({ name, source, sourceInfo: info, description: `${name} description`, handler: () => { throw new Error('must not execute'); }, body: 'secret prompt body' });
const identity = { instance: 'a'.repeat(64), pane: 'pane', session: 'session', pid: process.pid };
function query(path, target = identity) {
  return new Promise((resolve, reject) => {
    const socket = createConnection(path);
    let response = '';
    socket.setTimeout(1500, () => socket.destroy(new Error('timeout')));
    socket.on('error', reject);
    socket.on('connect', () => socket.write(`${JSON.stringify({ ...target, challenge: 'b'.repeat(32) })}\n`));
    socket.on('data', data => { response += data; });
    socket.on('end', () => { try { resolve(JSON.parse(response)); } catch { reject(new Error('empty or malformed response')); } });
  });
}

test('normalizes active invocation names, precedence, provenance, disabled skills, and limits', () => {
  const names = ['orchestrate', 'orchestrate-plan', 'orchestrate-implement', 'orchestrate-status', 'orchestrate-answer'];
  const result = normalizeCommands([
    ...names.map(name => entry(name)), entry('review:1'), entry('review:2'), entry('review:1', 'prompt'),
    entry('template', 'prompt', { ...sourceInfo, scope: 'project', source: 'local', origin: 'top-level' }),
    entry('skill:disabled', 'prompt'), entry('skill:enabled', 'prompt'),
    entry('skill:disabled', 'skill'), entry('skill:enabled', 'skill'),
  ], new Set(['skill:enabled']));
  expect(result.status).toBe('available');
  expect(result.commands.filter(c => c.command.startsWith('/orchestrate'))).toHaveLength(5);
  expect(result.commands.find(c => c.command === '/review:1').kind).toBe('extension');
  expect(result.commands.some(c => c.command === '/skill:disabled')).toBe(false);
  expect(result.commands.find(c => c.command === '/skill:enabled').kind).toBe('skill');
  expect(result.commands.find(c => c.command === '/template').provenance.scope).toBe('project');
  expect(JSON.stringify(result)).not.toContain('secret prompt body');
  expect(JSON.stringify(result)).not.toContain('handler');
  const huge = normalizeCommands(Array.from({ length: 5000 }, (_, i) => entry(`cmd-${i}`)), new Set());
  expect(huge.commands).toHaveLength(4096);
  expect(huge.truncated).toBe(true);
  expect(huge.status).toBe('available');
  expect(normalizeCommands([entry('bad name')], new Set()).status).toBe('partial');
});

test('socket queries current metadata, rejects identity mismatch and concurrent ownership, and cleans up', async () => {
  const directory = await mkdtemp('/tmp/pi-bridge-test-');
  let commands = [entry('first')];
  const close = await startBridge({ ...identity, directory, catalog: () => normalizeCommands(commands, new Set()) });
  const path = join(directory, `${process.pid}.sock`);
  try {
    expect((await lstat(path)).mode & 0o777).toBe(0o600);
    const first = await query(path);
    commands = [entry('second')];
    const second = await query(path);
    expect(second.commands[0].command).toBe('/second');
    expect(second.revision).not.toBe(first.revision);
    expect(second.incarnation).toBe(first.incarnation);
    await expect(query(path, { ...identity, session: 'child' })).rejects.toThrow();
    await expect(query(path, { ...identity, pane: 'other' })).rejects.toThrow();
    await expect(startBridge({ ...identity, directory, catalog: () => ({}) })).rejects.toThrow();
    expect((await query(path)).commands[0].command).toBe('/second');
  } finally { await close(); await close(); await rm(directory, { recursive: true, force: true }); }
});

test('redacts authenticated package provenance before bridge serialization', async () => {
  const directory = await mkdtemp('/tmp/pi-redaction-');
  const commands = redactionCases.map((fixture, i) => entry(`package-${i}`, 'extension', {
    ...sourceInfo, source: fixture.input, path: fixture.path || sourceInfo.path,
    ...(fixture.base_dir ? { baseDir: fixture.base_dir } : {}),
  }));
  const close = await startBridge({ ...identity, directory, catalog: () => normalizeCommands(commands, new Set()) });
  try {
    const result = await query(join(directory, `${process.pid}.sock`));
    expect(JSON.stringify(result)).not.toContain('REDACTION_');
    for (const [i, fixture] of redactionCases.entries()) {
      const provenance = result.commands.find(command => command.command === `/package-${i}`).provenance;
      expect(provenance.source).toBe(fixture.expected);
      if (fixture.expected_path) expect(provenance.path).toBe(fixture.expected_path);
      if (fixture.expected_base_dir) expect(provenance.base_dir).toBe(fixture.expected_base_dir);
    }
  } finally { await close(); await rm(directory, { recursive: true, force: true }); }
});

test('Node subprocess survives null and malformed request frames and serves a later valid request', async () => {
  const directory = await mkdtemp('/tmp/pi-node-');
  const child = spawn('node', ['--unhandled-rejections=strict', fileURLToPath(new URL('./fixtures/pi-bridge-server.mjs', import.meta.url)), identity.instance, identity.pane, identity.session, directory], { stdio: ['ignore', 'pipe', 'pipe'] });
  const exited = once(child, 'exit');
  let errors = '';
  child.stderr.on('data', data => { errors += data; });
  try {
    await Promise.race([once(child.stdout, 'data'), exited.then(() => { throw new Error('Bridge exited before ready'); })]);
    const path = join(directory, `${child.pid}.sock`);
    for (const frame of ['null', '[]', '"text"', '12', '{']) {
      await new Promise((resolve, reject) => {
        const socket = createConnection(path);
        socket.on('error', reject);
        socket.on('connect', () => socket.write(`${frame}\n`));
        socket.on('close', resolve);
      });
      expect(child.exitCode).toBeNull();
      expect((await query(path, { ...identity, pid: child.pid })).status).toBe('available');
    }
    expect(errors).toBe('');
  } finally {
    child.kill('SIGTERM');
    await exited;
    await rm(directory, { recursive: true, force: true });
  }
});

test('passes request signals and recovers metadata discovery without accumulating stalled autocomplete work', async () => {
  const directory = await mkdtemp('/tmp/pi-cancellation-');
  let calls = 0;
  let stalledSignal;
  let settle;
  let provider = { getSuggestions: (_lines, _line, _column, options) => {
    expect(options.signal.aborted).toBe(false);
    calls++;
    if (calls === 1) {
      stalledSignal = options.signal;
      return new Promise(resolve => { settle = resolve; });
    }
    return { items: [{ value: '/skill:active' }] };
  } };
  const commands = [entry('active'), entry('skill:active', 'skill'), entry('skill:active', 'prompt')];
  const catalog = createCatalog(() => commands, () => provider);
  const close = await startBridge({ ...identity, directory, catalog });
  const path = join(directory, `${process.pid}.sock`);
  try {
    const started = Date.now();
    await expect(query(path)).rejects.toThrow();
    expect(Date.now() - started).toBeLessThan(1400);
    expect(stalledSignal.aborted).toBe(true);
    provider = { getSuggestions: (_lines, _line, _column, { signal }) => {
      expect(signal.aborted).toBe(false);
      calls++;
      return { items: [{ value: '/skill:active' }, { value: '/skill:active' }] };
    } };
    for (let i = 0; i < 4; i++) {
      const result = await query(path);
      expect(result.status).toBe('partial');
      expect(result.commands.map(command => command.command)).toEqual(['/active']);
    }
    expect(calls).toBe(1);
    const replacementCatalog = createCatalog(() => [entry('replacement-session')], () => provider);
    const replacement = await replacementCatalog(new AbortController().signal);
    expect(replacement.status).toBe('partial');
    expect(replacement.commands[0].command).toBe('/replacement-session');
    expect(calls).toBe(1);
    settle({ items: [] });
    await new Promise(resolve => setTimeout(resolve, 0));
    const recovered = await query(path);
    expect(recovered.status).toBe('available');
    expect(recovered.commands.find(command => command.command === '/skill:active').kind).toBe('skill');
    expect(calls).toBe(2);
  } finally { await close(); await rm(directory, { recursive: true, force: true }); }
});

test('aborts autocomplete when a client disconnects or the bridge shuts down', async () => {
  const directory = await mkdtemp('/tmp/pi-abort-');
  let observed;
  let started;
  let begun = new Promise(resolve => { started = resolve; });
  const catalog = createCatalog(() => [entry('active')], () => ({ getSuggestions: (_lines, _line, _column, { signal }) => {
    observed = signal;
    started();
    return new Promise((_resolve, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true }));
  } }));
  const close = await startBridge({ ...identity, directory, catalog });
  const path = join(directory, `${process.pid}.sock`);
  const socket = createConnection(path);
  socket.on('error', () => {});
  try {
    await once(socket, 'connect');
    socket.write(`${JSON.stringify({ ...identity, challenge: 'b'.repeat(32) })}\n`);
    await begun;
    socket.destroy();
    await new Promise(resolve => setTimeout(resolve, 20));
    expect(observed.aborted).toBe(true);
    begun = new Promise(resolve => { started = resolve; });
    const pending = query(path).catch(() => undefined);
    await begun;
    await close();
    await pending;
    expect(observed.aborted).toBe(true);
  } finally { socket.destroy(); await close(); await rm(directory, { recursive: true, force: true }); }
});

test('rejects public runtime directories', async () => {
  const directory = await mkdtemp('/tmp/pi-bridge-test-');
  try {
    await chmod(directory, 0o755);
    await expect(startBridge({ ...identity, directory, catalog: () => ({}) })).rejects.toThrow('Unsafe');
  } finally { await rm(directory, { recursive: true, force: true }); }
});

test('session lifecycle waits for post-discovery autocomplete, isolates nested runtimes, reloads, and shuts down', async () => {
  const fixture = await mkdtemp('/tmp/pi-lifecycle-');
  const herdrPath = join(fixture, 'herdr.sock');
  const herdr = createServer();
  await new Promise(resolve => herdr.listen(herdrPath, resolve));
  const instance = await instanceIdentity(herdrPath);
  const directory = `/tmp/herdr-pi-${process.getuid()}-${instance.slice(0, 16)}`;
  const savedPane = process.env.HERDR_PANE_ID;
  const savedSocket = process.env.HERDR_SOCKET_PATH;
  const tty = process.stdin.isTTY;
  process.stdin.isTTY = true;
  process.env.HERDR_PANE_ID = 'pane';
  process.env.HERDR_SOCKET_PATH = herdrPath;
  let registered = [entry('loaded')];
  let factory;
  const provider = { getSuggestions: async () => ({ items: [{ value: '/skill:active' }] }) };
  const sessionFile = join(fixture, '2026-09-23T12-00-00-000Z_01a0c7af-665a-76ea-94ee-5e4c09cce1a7.jsonl');
  const context = { mode: 'tui', sessionManager: { getSessionId: () => 'session-uuid', getSessionFile: () => sessionFile }, ui: { addAutocompleteProvider: fn => { factory = fn; fn(provider); } } };
  const makeAPI = () => {
    const handlers = {};
    bridge({ on: (event, handler) => { handlers[event] = handler; }, getCommands: () => registered });
    return handlers;
  };
  let handlers = makeAPI();
  const target = { ...identity, instance, session: sessionFile };
  const path = join(directory, `${process.pid}.sock`);
  try {
    await handlers.session_start({}, context);
    expect((await query(path, target)).status).toBe('loading');
    await expect(query(path, { ...target, session: 'session-uuid' })).rejects.toThrow();
    handlers.resources_discover();
    registered.push(entry('skill:active', 'skill'), entry('skill:hidden', 'skill'));
    factory(provider);
    expect((await query(path, target)).commands.map(c => c.command)).toEqual(['/loaded', '/skill:active']);
    const child = makeAPI();
    await child.session_start({}, { ...context, sessionManager: { getSessionId: () => 'child', getSessionFile: () => join(fixture, 'child.jsonl') } });
    await child.session_shutdown();
    expect((await query(path, target)).status).toBe('available');
    await handlers.session_shutdown();
    await expect(query(path, target)).rejects.toThrow();
    const missingFile = makeAPI();
    await missingFile.session_start({}, { ...context, sessionManager: { getSessionId: () => 'session-uuid', getSessionFile: () => undefined } });
    await expect(query(path, target)).rejects.toThrow();
    handlers = makeAPI();
    await handlers.session_start({}, context);
    handlers.resources_discover();
    factory(provider);
    registered = [entry('reloaded')];
    expect((await query(path, target)).commands[0].command).toBe('/reloaded');
  } finally {
    await handlers.session_shutdown();
    process.stdin.isTTY = tty;
    if (savedPane === undefined) delete process.env.HERDR_PANE_ID; else process.env.HERDR_PANE_ID = savedPane;
    if (savedSocket === undefined) delete process.env.HERDR_SOCKET_PATH; else process.env.HERDR_SOCKET_PATH = savedSocket;
    await new Promise(resolve => herdr.close(resolve));
    await rm(directory, { recursive: true, force: true });
    await rm(fixture, { recursive: true, force: true });
  }
});
