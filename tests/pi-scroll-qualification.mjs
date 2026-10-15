import { chmod, copyFile, mkdir, readFile, readdir, realpath, rename, stat, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { allocateRun, loggedCommand, parsePaneList, piLaunch, piSettings, preserveReceipt, sessionSeed } from './pi-scroll-driver.mjs';
import { randomUUID, createHash } from 'node:crypto';

const repo = resolve(import.meta.dir, '..');
const installedIntegration = join(process.env.HOME, '.pi/agent/extensions/herdr-agent-state.ts');
const moduleCache = process.env.GOMODCACHE || join(process.env.HOME, 'go/pkg/mod');
const browserCache = process.env.PLAYWRIGHT_BROWSERS_PATH || join(process.env.HOME, process.platform === 'darwin' ? 'Library/Caches/ms-playwright' : '.cache/ms-playwright');
const output = process.env.HERDR_P1_QUALIFICATION_OUTPUT;
if (!output || !output.startsWith('/')) throw new Error('HERDR_P1_QUALIFICATION_OUTPUT must name the persistent review directory');
const allocation = await allocateRun(output);
const runDirectory = allocation.directory;
await mkdir(join(runDirectory, 'commands'), { mode: 0o700 });
let root;
const receipt = { schema: 1, result: 'failed', root: '', versions: {}, isolation: { socket: '', liveSocketUntouched: 'Never queried or contacted; all Herdr commands use a fresh private socket and clean environment.' }, modes: [], tests: [], viewportMoved: { up: false, down: false }, retries: 0, processCleanup: [], browserEndToEnd: 'remaining-gap: encrypted paired browser → real relay → Pi not exercised; browser specs use an explicit mock.', sourceHashes: {} };
const bins = Object.fromEntries(['herdr', 'pi', 'bun', 'go', 'node', 'lsof'].map(name => [name, Bun.which(name)]));

const path = [...new Set(Object.values(bins).filter(Boolean).map(value => value.slice(0, value.lastIndexOf('/')))), '/usr/bin', '/bin', '/usr/sbin', '/sbin'].join(':');
let env = { PATH: path, LC_ALL: 'C' }, server, isolated = false, activePi;
let sequence = 0;
const sleep = ms => Bun.sleep(ms);
const json = async file => JSON.parse(await readFile(file, 'utf8'));
const assert = (value, message) => { if (!value) throw new Error(message); };
const normalize = value => value.trim().replace(/\s+/g, ' ');

async function identity(pid) {
  assert(Number.isSafeInteger(pid) && pid > 0, 'invalid owned PID');
  const result = await sync(['/bin/ps', '-p', String(pid), '-o', 'lstart=', '-o', 'uid='], 'process-identity');
  const fields = result.stdout.toString().trim().split(/\s+/);
  assert(result.exitCode === 0 && fields.length === 6 && Number(fields[5]) === process.getuid(), `process identity unavailable: ${pid}`);
  return fields.slice(0, 5).join(' ');
}

async function signalOwned(child, birth, signal) {
  if (child.exitCode !== null) return;
  assert(await identity(child.pid) === birth, 'owned child changed; no signal sent');
  child.kill(signal);
}

async function sync(args, label, choices = env) {
  const base = join(runDirectory, 'commands', `${String(++sequence).padStart(4, '0')}-${label}`);
  const record = { args, cwd: repo, env: choices, started: new Date().toISOString() };
  await writeFile(base + '.command.json', JSON.stringify(record, null, 2), { mode: 0o600 });
  const result = Bun.spawnSync(args, { env: choices, cwd: repo, stdin: 'ignore' });
  await writeFile(base + '.stdout', result.stdout, { mode: 0o600 });
  await writeFile(base + '.stderr', result.stderr, { mode: 0o600 });
  await writeFile(base + '.command.json', JSON.stringify({ ...record, ended: new Date().toISOString(), exitCode: result.exitCode }, null, 2), { mode: 0o600 });
  return result;
}

async function run(args, label, extra = {}, timeout = 30000) {
  return loggedCommand(args, { directory: join(runDirectory, 'commands'), label: `${String(++sequence).padStart(4, '0')}-${label}`, env: { ...env, ...extra }, cwd: repo, timeout, identity, signalOwned });
}

async function proveIsolation() {
  assert(server && await identity(server.pid) === server.birth, 'isolation-unavailable: server identity changed');
  const info = await stat(env.HERDR_SOCKET_PATH);
  assert(info.isSocket() && info.uid === process.getuid() && (info.mode & 0o777) === 0o600, 'isolation-unavailable: private socket unavailable');
  const owners = await run([bins.lsof, '-n', '-P', '-a', '-p', String(server.pid), '-U', '-Fn'], 'socket-owner');
  assert(owners.split('\n').includes(`n${env.HERDR_SOCKET_PATH}`), 'isolation-unavailable: spawned server does not own private socket');
  isolated = true;
}

async function herdr(...args) {
  await proveIsolation();
  return run([bins.herdr, ...args], `herdr-${args[0]}-${args[1] || ''}`);
}

async function waitFile(file) {
  const deadline = Date.now() + 10000;
  while (Date.now() < deadline) {
    if (await Bun.file(file).exists()) return json(file);
    await sleep(100);
  }
  throw new Error(`fixture readiness unavailable: ${file}`);
}

async function command(pane, value) {
  await herdr('pane', 'send-text', pane, value);
  await herdr('pane', 'send-keys', pane, 'Enter');
}

async function fixture(mode, pane, terminal, session) {
  const evidenceDirectory = join(root, 'evidence');
  const previous = await readdir(evidenceDirectory);
  assert(previous.length === 0, 'evidence directory not fresh');
  const contract = JSON.stringify({ root, socketPath: env.HERDR_SOCKET_PATH, uid: process.getuid(), serverPid: server.pid, serverStart: server.birth, evidenceDirectory });
  const variables = { HERDR_P1_CONTRACT_JSON: contract };
  for (const prefix of mode === 'fullscreen' ? ['SCROLL', 'LATENCY', 'ROW'] : ['SCROLL']) {
    for (const [key, value] of Object.entries({ SOCKET: env.HERDR_SOCKET_PATH, PANE: pane, TERMINAL: terminal, SESSION: session, BIN: bins.herdr, MODE: mode })) variables[`HERDR_${prefix}_FIXTURE_${key}`] = value;
  }
  if (mode === 'fullscreen') Object.assign(variables, { HERDR_P1_BROWSER_SCRIPT: join(repo, 'tests/pi-scroll-paired-browser.mjs'), HERDR_P1_WEB_ROOT: join(repo, 'frontend/dist') });
  let finished = false;
  const testing = run([bins.go, 'test', '-p', '1', '-run', 'TestDisposablePi', './internal/app', './internal/coordinator', '-count=1', '-v'], `fixtures-${mode}`, variables, 180000).finally(() => { finished = true; });
  const observed = testing.then(value => ({ value }), error => ({ error }));
  if (mode === 'fullscreen') {
    let browserError;
    try {
      while (!finished && !await Bun.file(join(root, 'browser-ready.json')).exists()) await sleep(100);
      assert(!finished, 'native relay fixture ended before browser readiness');
      await run([bins.bun, join(repo, 'tests/pi-scroll-paired-browser.mjs')], 'paired-browser', { PLAYWRIGHT_BROWSERS_PATH: browserCache }, 60000);
      receipt.browserEndToEnd = 'passed: Chromium enrolled a disposable controller over product encrypted WebSocket transport; actual frontend Up/Down moved real Pi; Local/Automatic used the same connection. WebKit focused mock suite remains separate.';
    } catch (error) {
      browserError = error;
      await writeFile(join(root, 'browser-done.json'), JSON.stringify({ result: 'failed', error: String(error) }), { mode: 0o600 });
    }
    const result = await observed;
    if (browserError) throw browserError;
    if (result.error) throw result.error;
  }
  const result = await observed;
  if (result.error) throw result.error;
  receipt.tests.push({ name: `TestDisposablePi (${mode})`, status: 'passed', output: result.value });
  if (mode === 'fullscreen') {
    const row = await json(join(evidenceDirectory, 'row-lease.json'));
    receipt.viewportMoved.up = true;
    receipt.viewportMoved.down = true;
    receipt.geometry = { initial: row.initial, leased: row.leased, restored: row.restored };
    receipt.sgr = { up: row.independent_up, down: row.independent_down };
  }
  await rename(evidenceDirectory, join(root, `evidence-${mode}`));
  await mkdir(evidenceDirectory, { mode: 0o700 });
}

try {
  receipt.allocation = { index: allocation.index, predecessor: allocation.predecessor };
  receipt.started = new Date().toISOString();
  assert(Object.values(bins).every(Boolean), 'installed tools unavailable');
  receipt.head = normalize((await sync(['git', 'rev-parse', 'HEAD'], 'tested-head')).stdout.toString());
  const checkout = await sync(['git', 'status', '--porcelain=v1', '--untracked-files=all'], 'checkout');
  assert(checkout.exitCode === 0 && checkout.stdout.length === 0, 'native qualification requires a clean committed checkout');
  const allocated = await sync(['/usr/bin/mktemp', '-d', '/tmp/herdr-scroll-p1-XXXXXX'], 'allocate-root');
  assert(allocated.exitCode === 0, 'fixture allocation failed');
  root = await realpath(allocated.stdout.toString().trim());
  receipt.root = root;
  await writeFile(join(runDirectory, 'setup.json'), JSON.stringify(receipt, null, 2), { mode: 0o600 });
  console.log(`qualification root: ${root}`);
  await writeFile(join(root, 'fixture-owner'), 'disposable-scroll-fixture\n', { mode: 0o600 });
  for (const dir of ['home', 'config', 'config/herdr', 'cache', 'data', 'state', 'runtime', 'tmp', 'pi', 'work', 'logs', 'evidence']) await mkdir(join(root, dir), { recursive: true, mode: 0o700 });
  const modcache = await realpath(moduleCache);
  env = { ...env, HOME: join(root, 'home'), USER: 'p1-fixture', LOGNAME: 'p1-fixture', SHELL: '/bin/sh', TERM: 'xterm-256color', TMPDIR: join(root, 'tmp'), XDG_CONFIG_HOME: join(root, 'config'), XDG_CACHE_HOME: join(root, 'cache'), XDG_DATA_HOME: join(root, 'data'), XDG_STATE_HOME: join(root, 'state'), XDG_RUNTIME_DIR: join(root, 'runtime'), HERDR_CONFIG_PATH: join(root, 'config/herdr/config.toml'), HERDR_SOCKET_PATH: join(root, 'config/herdr/herdr.sock'), PI_CODING_AGENT_DIR: join(root, 'pi'), PI_OFFLINE: '1', HERDR_P1_FIXTURE_ROOT: root, GOMODCACHE: modcache, GOPROXY: 'off', GOSUMDB: 'off', GOTOOLCHAIN: 'local', GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null', GIT_TERMINAL_PROMPT: '0', BUN_RUNTIME_TRANSPILER_CACHE_PATH: '0' };
  receipt.isolation.socket = env.HERDR_SOCKET_PATH;
  assert(env.HERDR_SOCKET_PATH.startsWith(root + '/') && !await Bun.file(env.HERDR_SOCKET_PATH).exists(), 'isolation-unavailable: socket is not fresh');
  await writeFile(env.HERDR_CONFIG_PATH, '[update]\nversion_check = false\nmanifest_check = false\n', { mode: 0o600 });
  for (const [name, args] of Object.entries({ pi: ['--version'], herdr: ['--version'], bun: ['--version'], go: ['version'] })) receipt.versions[name] = normalize(await run([bins[name], ...args], `${name}-version`));
  for (const [source, target] of [['relay/pi-command-bridge/bridge.mjs', 'bridge.mjs'], ['tests/pi-scroll-evidence-extension.mjs', 'evidence.mjs'], ['tests/pi-scroll-capture.mjs', 'capture.mjs']]) {
    await copyFile(join(repo, source), join(root, 'pi', target));
    await chmod(join(root, 'pi', target), 0o600);
  }
  const integration = await readFile(installedIntegration);
  assert(integration.includes(Buffer.from('HERDR_INTEGRATION_ID=pi')), 'installed Herdr Pi integration unavailable');
  receipt.installedIntegration = { path: installedIntegration, sha256: createHash('sha256').update(integration).digest('hex') };
  await writeFile(join(root, 'pi', 'herdr-agent-state.ts'), integration, { mode: 0o600 });
  await writeFile(join(root, 'editor.sh'), `#!/bin/sh\nprintf active > '${join(root, 'editor-active')}'\n/bin/sleep 2\nprintf done > '${join(root, 'editor-done')}'\n`, { mode: 0o700 });
  await writeFile(join(root, 'pi', 'settings.json'), JSON.stringify(piSettings(root, receipt.versions.pi)), { mode: 0o600 });
  const timestamp = new Date().toISOString(), session = randomUUID();
  const entries = [{ type: 'session', version: 3, id: session, timestamp, cwd: join(root, 'work') }];
  entries.push({ type: 'message', id: '00000001', parentId: null, timestamp, message: { role: 'user', content: Array.from({ length: 96 }, (_, i) => `P1 TEST USER [${String(i + 1).padStart(3, '0')}] generated test content`).join('\n'), timestamp: Date.now() } });
  const seed = sessionSeed(root, timestamp, session);
  await mkdir(dirname(seed), { recursive: true, mode: 0o700 });
  await writeFile(seed, entries.map(entry => JSON.stringify(entry)).join('\n') + '\n', { mode: 0o600 });
  const serverRecord = { args: [bins.herdr, 'server'], env, cwd: repo, started: new Date().toISOString(), stdout: join(root, 'logs', 'herdr-server.log'), stderr: join(root, 'logs', 'herdr-server.stderr') };
  await writeFile(join(runDirectory, 'server-command.json'), JSON.stringify(serverRecord, null, 2), { mode: 0o600 });
  const child = Bun.spawn(serverRecord.args, { env, cwd: repo, stdin: 'ignore', stdout: Bun.file(serverRecord.stdout), stderr: Bun.file(serverRecord.stderr) });
  server = { pid: child.pid, birth: await identity(child.pid), child };
  const deadline = Date.now() + 10000;
  while (!await Bun.file(env.HERDR_SOCKET_PATH).exists() && Date.now() < deadline && child.exitCode === null) await sleep(100);
  await proveIsolation();
  receipt.serverStatus = JSON.parse(await herdr('status', 'server', '--json'));
  const workspace = JSON.parse(await herdr('workspace', 'create', '--cwd', join(root, 'work'), '--label', 'p1-scroll-fixture', '--no-focus', '--env', `PI_CODING_AGENT_DIR=${env.PI_CODING_AGENT_DIR}`, '--env', 'PI_OFFLINE=1', '--env', 'HERDR_ENV=1', '--env', `HERDR_P1_FIXTURE_ROOT=${root}`, '--env', `NODE_OPTIONS=--import=${join(root, 'pi', 'capture.mjs')}`));
  await writeFile(join(root, 'workspace.json'), JSON.stringify(workspace), { mode: 0o600 });
  const panes = JSON.parse(await herdr('pane', 'list'));
  await writeFile(join(root, 'panes.json'), JSON.stringify(panes), { mode: 0o600 });
  const { pane_id: pane, terminal_id: terminal } = parsePaneList(JSON.stringify(panes));
  for (const mode of ['fullscreen', 'regular']) {
    await proveIsolation();
    const launch = await run(piLaunch(bins.herdr, mode, pane, seed, root), `herdr-agent-start-${mode}`);
    await writeFile(join(root, `launch-${mode}.json`), launch, { mode: 0o600 });
    const ready = await waitFile(join(root, `pi-${mode}.json`));
    activePi = { pid: ready.pid, birth: await identity(ready.pid) };
    assert(ready.session === session && ready.file === seed, 'Pi did not load the exact disposable transcript');
    receipt.modes.push({ mode, pid: ready.pid, session: ready.session });
    await sleep(1000);
    await herdr('pane', 'read', pane, '--source', 'visible', '--lines', '80', '--format', 'text');
    try {
      await fixture(mode, pane, terminal, ready.session);
    } catch (error) {
      receipt.tests.push({ name: `TestDisposablePi (${mode})`, status: 'failed', error: String(error) });
      receipt.error = String(error);
      process.exitCode = 1;
      await rename(join(root, 'evidence'), join(root, `evidence-${mode}`));
      await mkdir(join(root, 'evidence'), { mode: 0o700 });
    }
    await command(pane, '/quit');
    const quitDeadline = Date.now() + 5000;
    let running = true;
    while (running && Date.now() < quitDeadline) {
      try { running = await identity(activePi.pid) === activePi.birth; } catch { running = false; }
      if (running) await sleep(50);
    }
    assert(!running, 'owned Pi did not exit after /quit');
    receipt.processCleanup.push({ process: 'pi', mode, pid: activePi.pid, start: activePi.birth, action: 'exited after /quit in private pane' });
    activePi = undefined;
  }
  const received = [];
  for (const mode of ['fullscreen', 'regular']) {
    const events = (await readFile(join(root, 'logs', `pi-${mode}-events.jsonl`), 'utf8')).trim().split('\n').map(line => JSON.parse(line));
    assert(!events.some(event => event.event === 'forbidden_provider_request'), 'model request observed');
    const input = Buffer.concat(events.filter(event => event.event === 'input').map(event => Buffer.from(event.base64, 'base64')));
    received.push({ mode, inputBase64: input.toString('base64'), sgr: [...input.toString().matchAll(/\x1b\[<6[45];\d+;\d+M/g)].map(match => match[0]) });
    const widgets = events.filter(event => event.event === 'widget');
    assert(widgets.length > 0 && widgets.every(event => event.rows === 0 && event.inputHandler === 'undefined' && event.mouseHandler === 'undefined'), 'widget is not invisible/noncapturing');
    assert(mode !== 'regular' || !input.includes(Buffer.from('\x1b[<6')), 'regular mode received wheel input');
  }
  receipt.received = received;
  if (receipt.sgr) {
    assert(received[0].sgr.includes(receipt.sgr.up) && received[0].sgr.includes(receipt.sgr.down) && received[0].sgr.length === 10, 'centered Up/Down bytes not received exactly once by real Pi');
  }
  receipt.bracketedPasteEnabled = (await readFile(join(root, 'logs', 'pi-output.raw'))).includes(Buffer.from('\x1b[?2004h'));
  assert(receipt.bracketedPasteEnabled, 'Pi bracketed paste enable was not captured');
  if (receipt.tests.every(test => test.status === 'passed') && receipt.viewportMoved.up && receipt.viewportMoved.down) receipt.result = 'passed';
} catch (error) {
  receipt.error = String(error.stack || error);
  console.error(receipt.error);
  process.exitCode = 1;
} finally {
  if (activePi) {
    try {
      assert(await identity(activePi.pid) === activePi.birth, 'Pi identity changed; no signal sent');
      process.kill(activePi.pid, 'SIGTERM');
      receipt.processCleanup.push({ process: 'pi', pid: activePi.pid, start: activePi.birth, action: 'verified SIGTERM' });
    } catch (error) { receipt.processCleanup.push({ process: 'pi', error: String(error) }); receipt.result = 'failed'; process.exitCode = 1; }
  }
  if (server && isolated) {
    try {
      await herdr('server', 'stop');
      const deadline = Date.now() + 5000;
      while (server.child.exitCode === null && Date.now() < deadline) await sleep(50);
      assert(server.child.exitCode !== null, 'isolated server did not stop');
      receipt.processCleanup.push({ process: 'herdr', action: 'stopped through private socket', pid: server.pid, start: server.birth });
    } catch (error) { receipt.processCleanup.push({ process: 'herdr', error: String(error) }); receipt.result = 'failed'; process.exitCode = 1; }
  } else if (server) {
    receipt.processCleanup.push({ process: 'herdr', action: 'isolation-unavailable; not signalled' });
  }
  if (receipt.result !== 'passed') process.exitCode = 1;
  if (root) {
    const files = (await sync(['git', 'diff', '--name-only', 'f51e206a289610de849043822296c484338b92a0'], 'source-paths')).stdout.toString().trim().split('\n');
    for (const file of [...files, 'tests/pi-scroll-qualification.mjs']) {
      if (await Bun.file(join(repo, file)).exists()) receipt.sourceHashes[file] = createHash('sha256').update(await readFile(join(repo, file))).digest('hex');
    }
  }
  receipt.ended = new Date().toISOString();
  if (server) {
    receipt.serverExitCode = server.child.exitCode;
    const file = join(runDirectory, 'server-command.json');
    await writeFile(file, JSON.stringify({ ...await json(file), pid: server.pid, start: server.birth, ended: receipt.ended, exitCode: server.child.exitCode, cleanup: receipt.processCleanup.filter(item => item.process === 'herdr') }, null, 2), { mode: 0o600 });
  }
  try {
    await preserveReceipt(runDirectory, root, receipt);
    console.log(`receipt: ${runDirectory}/receipt.json`);
  } finally {
    await allocation.release();
  }
}
